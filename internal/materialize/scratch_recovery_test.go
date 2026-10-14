package materialize

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

// cancelAfterCopyScratch reports cancellation from the moment a copy scratch
// object exists, i.e. after the copy target is created and before any byte of
// it is consumed.
type cancelAfterCopyScratch struct {
	context.Context
	pattern  string
	canceled atomic.Bool
}

func (ctx *cancelAfterCopyScratch) Err() error {
	if !ctx.canceled.Load() {
		if matches, _ := filepath.Glob(ctx.pattern); len(matches) == 0 {
			return nil
		}
		ctx.canceled.Store(true)
	}
	return context.Canceled
}

func TestCancelledCopyRemovesItsOwnScratch(t *testing.T) {
	content := []byte("scratch discard on cancellation")
	meta, discovery, _, targetRoot := singleMaterializeFixture(t, context.Background(), "final.bin", content)
	ctx := &cancelAfterCopyScratch{
		Context: context.Background(),
		pattern: filepath.Join(targetRoot, operationDirectoryPrefix+"*", scratchDirectoryName, "copy-*.pending"),
	}
	report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
	if !errors.Is(err, context.Canceled) || report.Outcome != OutcomeInterrupted || report.Writes.ScratchFilesRemoved != 1 {
		t.Fatalf("cancelled copy did not remove its own scratch: %#v %v", report, err)
	}
	entries, readErr := os.ReadDir(filepath.Join(onlyOperationDirectory(t, targetRoot), scratchDirectoryName))
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("cancelled copy left scratch residue: %d %v", len(entries), readErr)
	}
}

func TestFailedCopyRemovesOnlyItsOwnScratchByIdentity(t *testing.T) {
	ctx := context.Background()
	content := []byte("scratch discard on copy failure")
	meta, discovery, searchRoot, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", content)
	transitionHook = func(phase Phase) error {
		if phase == PhaseStageCreated {
			return os.WriteFile(filepath.Join(searchRoot, "source"), bytes.Repeat([]byte{'x'}, len(content)), 0o600)
		}
		return nil
	}
	t.Cleanup(func() { transitionHook = nil })
	report, err := Run(ctx, runOptionsFor(t, meta, &discovery, targetRoot))
	transitionHook = nil
	if err == nil || report.Writes.StagedFiles != 0 || report.Writes.ScratchFilesRemoved != 1 || report.Writes.ScratchFilesCreated < 2 {
		t.Fatalf("failed copy did not remove its own scratch: %#v %v", report, err)
	}
	entries, readErr := os.ReadDir(filepath.Join(onlyOperationDirectory(t, targetRoot), scratchDirectoryName))
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("failed copy left scratch residue: %d %v", len(entries), readErr)
	}
}

func TestResumeRemovesRetainedScratchBeforeCommit(t *testing.T) {
	ctx := context.Background()
	meta, discovery, _, targetRoot, operationID := interruptedMaterialize(t, PhaseFinalVerified)
	names := []string{
		"copy-000000000-" + strings.Repeat("a", 32) + ".pending",
		"event-000000009-" + strings.Repeat("b", 32) + ".pending",
	}
	plantMaterializeScratch(t, targetRoot, operationID, names[0], []byte("partial"))
	plantMaterializeScratch(t, targetRoot, operationID, names[1], nil)
	status, err := Status(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
	if err != nil || status.Used.ScratchEntries != 2 || status.WritesPerformed != 0 {
		t.Fatalf("status did not observe retained scratch read-only: %#v %v", status, err)
	}
	resumed, err := Resume(ctx, ResumeOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err != nil || resumed.Outcome != OutcomeMaterializedVerified || resumed.Operation.PhaseAfter != string(PhaseCommitted) ||
		resumed.Writes.ScratchFilesRemoved != 2 || resumed.WritesUncertain || resumed.Used.ScratchEntries != 0 {
		t.Fatalf("retained scratch still blocked the final commit: %#v %v", resumed, err)
	}
	directory, _ := OperationDirectoryName(operationID)
	entries, readErr := os.ReadDir(filepath.Join(targetRoot, directory, scratchDirectoryName))
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("commit recovery left scratch residue: %d %v", len(entries), readErr)
	}
}

func TestResumeKeepsUnexpectedScratchAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	meta, discovery, _, targetRoot, operationID := interruptedMaterialize(t, PhaseFinalVerified)
	name := "copy-1-" + strings.Repeat("c", 32) + ".pending"
	plantMaterializeScratch(t, targetRoot, operationID, name, []byte("foreign"))
	resumed, err := Resume(ctx, ResumeOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err == nil || !errors.Is(err, ErrCorruptJournal) || resumed.Writes.ScratchFilesRemoved != 0 || resumed.Operation.PhaseAfter == string(PhaseCommitted) {
		t.Fatalf("unexpected scratch name was not fail-closed: %#v %v", resumed, err)
	}
	directory, _ := OperationDirectoryName(operationID)
	if _, statErr := os.Lstat(filepath.Join(targetRoot, directory, scratchDirectoryName, name)); statErr != nil {
		t.Fatal("unexpected scratch object was removed", statErr)
	}
}

func TestDiscardScratchRequiresCanonicalNameAndIdentity(t *testing.T) {
	ctx := context.Background()
	_, _, _, targetRoot, operationID := interruptedMaterialize(t, PhaseFinalVerified)
	name := "copy-000000000-" + strings.Repeat("d", 32) + ".pending"
	plantMaterializeScratch(t, targetRoot, operationID, name, []byte("retained"))
	session, _, err := fsbind.BindExisting(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	handle, err := openJournal(ctx, session, operationID, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer handle.subtree.Close()
	if len(handle.scratchObjects) != 1 || handle.scratchObjects[0].name != name {
		t.Fatalf("scratch inventory is not retained for identity binding: %#v", handle.scratchObjects)
	}
	retained := handle.scratchObjects[0]
	report := newReport("", "", DefaultLimits())
	if err := handle.discardScratch(ctx, name, handle.subtree.Identity(), -1, &report); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("scratch removal accepted another identity: %v", err)
	}
	if err := handle.discardScratch(ctx, name, retained.identity, retained.size+1, &report); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("scratch removal accepted a stale size: %v", err)
	}
	if err := handle.discardScratch(ctx, "stage", retained.identity, -1, &report); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("scratch removal accepted a non-scratch name: %v", err)
	}
	if report.WritesPerformed != 0 || report.Writes.ScratchFilesRemoved != 0 {
		t.Fatalf("refused scratch removal was reported as a write: %#v", report.Writes)
	}
	scratch := filepath.Join(targetRoot, onlyOperationDirectoryName(t, targetRoot), scratchDirectoryName, name)
	if _, statErr := os.Lstat(scratch); statErr != nil {
		t.Fatal("refused scratch removal deleted the object", statErr)
	}
	if err := handle.discardScratch(ctx, name, retained.identity, retained.size, &report); err != nil {
		t.Fatal(err)
	}
	if report.WritesPerformed != 1 || report.Writes.ScratchFilesRemoved != 1 || report.WritesUncertain {
		t.Fatalf("confirmed scratch removal was not reported: %#v", report)
	}
	if _, statErr := os.Lstat(scratch); !os.IsNotExist(statErr) {
		t.Fatal("identity-bound scratch removal left the object", statErr)
	}
}

func plantMaterializeScratch(t *testing.T, targetRoot string, operationID OperationID, name string, content []byte) {
	t.Helper()
	ctx := context.Background()
	directory, _ := OperationDirectoryName(operationID)
	session, _, err := fsbind.BindExisting(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	subtree, err := session.OpenPrivateSubtreeObserved(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	path, err := fsbind.PathFromComponents([]string{scratchDirectoryName, name})
	if err != nil {
		t.Fatal(err)
	}
	file, err := subtree.CreateRegular(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func onlyOperationDirectoryName(t *testing.T, targetRoot string) string {
	t.Helper()
	return filepath.Base(onlyOperationDirectory(t, targetRoot))
}
