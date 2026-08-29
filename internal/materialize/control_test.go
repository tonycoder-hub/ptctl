package materialize

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/seed"
)

func TestStatusAndAbandonRetainStagedBytes(t *testing.T) {
	ctx := context.Background()
	meta, discovery, searchRoot, targetRoot, operationID := interruptedMaterialize(t, PhaseFileStaged)

	status, err := Status(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if status.Outcome != OutcomeActive || !status.Operation.Resumable || status.Operation.PhaseAfter != string(PhaseFileStaged) {
		t.Fatalf("unexpected active status: %#v", status)
	}

	directory, _ := OperationDirectoryName(operationID)
	staged := filepath.Join(targetRoot, directory, stageDirectoryName, "final.bin")
	if _, err := os.Stat(staged); err != nil {
		t.Fatal(err)
	}
	abandoned, err := Abandon(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if abandoned.Outcome != OutcomeAbandoned || abandoned.Operation.Resumable || abandoned.Writes.JournalEvents != 1 ||
		!abandoned.Target.RootIdentityBound || abandoned.Target.ExpectedRootIdentity == "" ||
		abandoned.Target.ExpectedRootIdentity != abandoned.Target.ObservedRootIdentity || !abandoned.Target.SameFilesystemStage {
		t.Fatalf("unexpected abandon report: %#v", abandoned)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatal("abandon removed staged bytes", err)
	}
	repeated, err := Abandon(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
	if err != nil || repeated.Outcome != OutcomeAbandoned || repeated.WritesPerformed != 0 ||
		!strings.Contains(strings.Join(repeated.Warnings, " "), "performs no deletion") {
		t.Fatalf("idempotent abandon hid retained staged bytes: %#v %v", repeated, err)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatal("idempotent abandon removed staged bytes", err)
	}

	status, err = Status(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
	if err != nil || status.Outcome != OutcomeAbandoned || status.Operation.Resumable ||
		!strings.Contains(strings.Join(status.Warnings, " "), "performs no deletion") {
		t.Fatalf("unexpected abandoned status: %#v %v", status, err)
	}
	fresh := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	resumed, err := Resume(ctx, ResumeOptions{
		Meta: meta, Discovery: &fresh, TargetRoot: targetRoot, OperationID: operationID,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err == nil || resumed.Outcome != OutcomeAbandoned {
		t.Fatalf("abandoned operation resumed: %#v %v", resumed, err)
	}
}

func TestEmptyTargetRootIsNeverInterpretedAsTheWorkingDirectory(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	working := t.TempDir()
	if err := os.Chdir(working); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	operationID, err := ParseOperationID("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	planID := strings.Repeat("b", 24)
	checks := []func() error{
		func() error {
			_, callErr := Run(context.Background(), RunOptions{TargetRoot: "", ExpectedPlanID: planID, Limits: DefaultLimits()})
			return callErr
		},
		func() error {
			_, callErr := Resume(context.Background(), ResumeOptions{TargetRoot: "", OperationID: operationID, ExpectedPlanID: planID, Limits: DefaultLimits()})
			return callErr
		},
		func() error {
			_, callErr := Status(context.Background(), ControlOptions{TargetRoot: "", OperationID: operationID, Limits: DefaultLimits()})
			return callErr
		},
		func() error {
			_, callErr := Abandon(context.Background(), ControlOptions{TargetRoot: "", OperationID: operationID, Limits: DefaultLimits()})
			return callErr
		},
		func() error {
			_, callErr := ListOperations(context.Background(), "", DefaultOperationListLimits())
			return callErr
		},
	}
	for index, check := range checks {
		if callErr := check(); !errors.Is(callErr, ErrPolicy) {
			t.Fatalf("empty target case %d did not fail as policy: %v", index, callErr)
		}
	}
	entries, err := os.ReadDir(working)
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty target APIs touched the working directory: %#v %v", entries, err)
	}
}

func TestAbandonRefusesPublicationUncertainOperation(t *testing.T) {
	ctx := context.Background()
	_, _, _, targetRoot, operationID := interruptedMaterialize(t, PhasePublishIntent)
	report, err := Abandon(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
	if err == nil || !errors.Is(err, ErrPolicy) || report.Outcome != OutcomeBlocked || report.WritesPerformed != 0 ||
		report.Target.Publication != "historical_publication_uncertain" {
		t.Fatalf("publish-intent operation was abandoned: %#v %v", report, err)
	}
	directory, _ := OperationDirectoryName(operationID)
	if _, err := os.Stat(filepath.Join(targetRoot, directory, stageDirectoryName, "final.bin")); err != nil {
		t.Fatal("blocked abandon removed the publication source", err)
	}
}

func TestListOperationsIsBoundedAndNeverSelectsLatest(t *testing.T) {
	ctx := context.Background()
	_, _, _, targetRoot, first := interruptedMaterialize(t, PhaseFileStaged)
	_, _, _, _, second := interruptedMaterializeInRoots(t, PhaseFileStaged, "other.bin", targetRoot)

	result, err := ListOperations(ctx, targetRoot, DefaultOperationListLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.Operations) != 2 {
		t.Fatalf("unexpected operation listing: %#v", result)
	}
	seen := map[OperationID]bool{}
	for _, operation := range result.Operations {
		seen[operation.ID] = true
		if operation.Status != "not_inspected" {
			t.Fatalf("listing inferred operation status: %#v", operation)
		}
	}
	if !seen[first] || !seen[second] {
		t.Fatalf("listing omitted explicit IDs: %#v", result)
	}

	limits := DefaultOperationListLimits()
	limits.MaxOperations = 1
	bounded, err := ListOperations(ctx, targetRoot, limits)
	if err != nil {
		t.Fatal(err)
	}
	if bounded.Complete || bounded.StopReason != "max_operations" || len(bounded.Operations) != 1 {
		t.Fatalf("operation N+1 was not fail-closed: %#v", bounded)
	}
}

func TestStatusRejectsCorruptJournal(t *testing.T) {
	ctx := context.Background()
	_, _, _, targetRoot, operationID := interruptedMaterialize(t, PhaseFileStaged)
	directory, _ := OperationDirectoryName(operationID)
	events, err := os.ReadDir(filepath.Join(targetRoot, directory, journalDirectoryName))
	if err != nil || len(events) == 0 {
		t.Fatal("journal events unavailable", err)
	}
	if err := os.WriteFile(filepath.Join(targetRoot, directory, journalDirectoryName, events[0].Name()), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Status(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
	if err == nil || report.Outcome != OutcomeIntegrityFailed || report.Operation.Resumable ||
		report.Operation.ID != operationID.String() || report.Operation.Status != "inspection_incomplete" ||
		report.Operation.PhaseAfter != "unknown" {
		t.Fatalf("corrupt journal was accepted: %#v %v", report, err)
	}
}

func TestStatusDistinguishesMissingOperationFromCorruptJournal(t *testing.T) {
	targetRoot := t.TempDir()
	preflightMaterializeFilesystem(t, targetRoot)
	operationID, _ := ParseOperationID("sha256:" + strings.Repeat("0", 64))
	report, err := Status(context.Background(), ControlOptions{
		TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits(),
	})
	if !errors.Is(err, ErrOperationNotFound) || report.Outcome != OutcomeInterrupted ||
		report.Operation.ID != operationID.String() || report.Operation.Status != "not_found" {
		t.Fatalf("missing operation was reported as corruption: %#v %v", report, err)
	}
}

func TestRetainedScratchBudgetBlocksFurtherWritesWithoutDeletion(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxScratchEntries = 1
	_, _, _, targetRoot, operationID := interruptedMaterializeWithLimits(t, PhaseFileStaged, "final.bin", t.TempDir(), limits)
	directory, _ := OperationDirectoryName(operationID)
	scratchName := "copy-000000000-" + strings.Repeat("0", 32) + ".pending"
	scratch := filepath.Join(targetRoot, directory, scratchDirectoryName, scratchName)
	session, _, err := fsbind.BindExisting(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	subtree, err := session.OpenPrivateSubtreeObserved(directory)
	if err != nil {
		t.Fatal(err)
	}
	scratchPath, _ := fsbind.PathFromComponents([]string{scratchDirectoryName, scratchName})
	file, err := subtree.CreateRegular(context.Background(), scratchPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := subtree.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	status, err := Status(context.Background(), ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: limits})
	if err != nil || status.Used.ScratchEntries != 1 || status.Outcome != OutcomeBlocked ||
		status.Operation.Status != "scratch_capacity_blocked" || status.Operation.Resumable {
		t.Fatalf("retained scratch was not observed: %#v %v", status, err)
	}
	abandoned, err := Abandon(context.Background(), ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: limits})
	if err == nil || !errors.Is(err, ErrPolicy) || abandoned.Outcome != OutcomeBlocked || abandoned.WritesPerformed != 0 ||
		abandoned.Operation.Status != "scratch_capacity_blocked" || abandoned.Operation.Resumable {
		t.Fatalf("scratch budget did not fail closed: %#v %v", abandoned, err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatal("scratch budget enforcement deleted retained state", err)
	}
}

func TestResumeScratchByteCapacityFailureIsNotReportedResumable(t *testing.T) {
	ctx := context.Background()
	limits := DefaultLimits()
	limits.MaxScratchBytes = limits.MaxEventBytes
	content := make([]byte, limits.MaxScratchBytes)
	meta, discovery, searchRoot, targetRoot := singleMaterializeFixture(t, ctx, "final.bin", content)
	stop := errors.New("stop before staging")
	transitionHook = func(phase Phase) error {
		if phase == PhaseStageCreated {
			return stop
		}
		return nil
	}
	report, err := Run(ctx, RunOptions{
		Meta: meta, Discovery: &discovery, TargetRoot: targetRoot,
		ExpectedPlanID: discovery.Plan.ID, Limits: limits,
	})
	transitionHook = nil
	t.Cleanup(func() { transitionHook = nil })
	if !errors.Is(err, stop) {
		t.Fatalf("operation did not stop before staging: %#v %v", report, err)
	}
	operationID, err := ParseOperationID(report.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	directory, _ := OperationDirectoryName(operationID)
	session, _, err := fsbind.BindExisting(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	subtree, err := session.OpenPrivateSubtreeObserved(directory)
	if err != nil {
		t.Fatal(err)
	}
	scratchName := "copy-000000000-" + strings.Repeat("1", 32) + ".pending"
	scratchPath, _ := fsbind.PathFromComponents([]string{scratchDirectoryName, scratchName})
	file, err := subtree.CreateRegular(ctx, scratchPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := subtree.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	fresh := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	resumed, err := Resume(ctx, ResumeOptions{
		Meta: meta, Discovery: &fresh, TargetRoot: targetRoot, OperationID: operationID,
		ExpectedPlanID: discovery.Plan.ID, Limits: limits,
	})
	if err == nil || !errors.Is(err, errScratchCapacity) || resumed.Outcome != OutcomeBlocked ||
		resumed.Operation.Status != "scratch_capacity_blocked" || resumed.Operation.Resumable {
		t.Fatalf("scratch byte exhaustion was reported as resumable: %#v %v", resumed, err)
	}
	if _, statErr := os.Lstat(filepath.Join(targetRoot, "final.bin")); !os.IsNotExist(statErr) {
		t.Fatal("scratch-exhausted resume published a final object")
	}
}

func interruptedMaterialize(t *testing.T, phase Phase) (*metafile.MetaInfo, seed.DiscoveryResult, string, string, OperationID) {
	t.Helper()
	targetRoot := t.TempDir()
	return interruptedMaterializeInRoots(t, phase, "final.bin", targetRoot)
}

func interruptedMaterializeInRoots(t *testing.T, phase Phase, name, targetRoot string) (*metafile.MetaInfo, seed.DiscoveryResult, string, string, OperationID) {
	return interruptedMaterializeWithLimits(t, phase, name, targetRoot, DefaultLimits())
}

func interruptedMaterializeWithLimits(t *testing.T, phase Phase, name, targetRoot string, limits Limits) (*metafile.MetaInfo, seed.DiscoveryResult, string, string, OperationID) {
	t.Helper()
	ctx := context.Background()
	content := []byte("control " + name)
	meta := materializeSingleV1Meta(t, name, content)
	searchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(searchRoot, "source"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	preflightMaterializeFilesystem(t, targetRoot)
	discovery := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	stop := errors.New("stop at control boundary")
	transitionHook = func(current Phase) error {
		if current == phase {
			return stop
		}
		return nil
	}
	report, err := Run(ctx, RunOptions{
		Meta: meta, Discovery: &discovery, TargetRoot: targetRoot,
		ExpectedPlanID: discovery.Plan.ID, Limits: limits,
	})
	transitionHook = nil
	t.Cleanup(func() { transitionHook = nil })
	if !errors.Is(err, stop) {
		t.Fatalf("operation did not stop at %s: %#v %v", phase, report, err)
	}
	operationID, err := ParseOperationID(report.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return meta, discovery, searchRoot, targetRoot, operationID
}
