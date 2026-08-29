package materialize

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
)

func TestPruneCommittedOperationRetainsExactTombstoneAndFinalBytes(t *testing.T) {
	ctx := context.Background()
	meta, content, targetRoot, planID, operationID := committedOperationForPrune(t, ctx)
	report, err := Prune(ctx, PruneOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID, ExpectedPlanID: planID,
		JournalLimits: DefaultLimits(), RetentionLimits: DefaultRetentionLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != RetentionOutcomePruned || !report.Proof.CurrentFinalVerified || !report.Markers.ExactTombstone ||
		report.Writes.MarkerDurabilityConfirms != 2 ||
		report.Writes.FilesRemoved == 0 || report.Writes.DirectoriesRemoved == 0 || report.WritesPerformed == 0 {
		t.Fatalf("unexpected committed prune report: %#v", report)
	}
	raw, err := os.ReadFile(filepath.Join(targetRoot, "final.bin"))
	if err != nil || !bytes.Equal(raw, content) {
		t.Fatalf("prune changed the published final: %q %v", raw, err)
	}
	assertExactRetentionTombstone(t, targetRoot, operationID)
	status, err := Status(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
	if err != nil || status.Outcome != OutcomeRetained || status.Operation.Status != "retained" || status.Operation.Resumable {
		t.Fatalf("status did not recognize the retained tombstone: %#v %v", status, err)
	}
	resumed, err := Resume(ctx, ResumeOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID, ExpectedPlanID: planID, Limits: DefaultLimits(),
	})
	if err == nil || !errors.Is(err, ErrPolicy) || resumed.Outcome != OutcomeBlocked || resumed.Operation.Status != "retained" {
		t.Fatalf("resume crossed the retention boundary: %#v %v", resumed, err)
	}
	abandoned, err := Abandon(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
	if err == nil || !errors.Is(err, ErrPolicy) || abandoned.Outcome != OutcomeBlocked || abandoned.Operation.Status != "retained" {
		t.Fatalf("abandon crossed the retention boundary: %#v %v", abandoned, err)
	}

	repeated, err := Prune(ctx, PruneOptions{
		TargetRoot: targetRoot, OperationID: operationID, ExpectedPlanID: planID,
		JournalLimits: DefaultLimits(), RetentionLimits: DefaultRetentionLimits(),
	})
	if err != nil || repeated.Outcome != RetentionOutcomeAlreadyPruned || repeated.WritesPerformed != 0 || !repeated.Markers.ExactTombstone {
		t.Fatalf("idempotent prune changed tombstone: %#v %v", repeated, err)
	}
}

func TestPruneDoesNotPromotePostPublicationFailureToDurableIntent(t *testing.T) {
	ctx := context.Background()
	meta, _, targetRoot, planID, operationID := committedOperationForPrune(t, ctx)
	previous := publicationTestHook
	publicationTestHook = func(kind string) (fsbind.Publication, error, bool) {
		if kind != "retention" {
			return fsbind.Publication{}, nil, false
		}
		return fsbind.Publication{
			Attempted: true, Published: true, Durability: fsbind.DurabilityConfirmed,
		}, fsbind.ErrBindingChanged, true
	}
	t.Cleanup(func() { publicationTestHook = previous })
	report, err := Prune(ctx, PruneOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID, ExpectedPlanID: planID,
		JournalLimits: DefaultLimits(), RetentionLimits: DefaultRetentionLimits(),
	})
	if !errors.Is(err, fsbind.ErrBindingChanged) || report.Outcome != RetentionOutcomeIntegrityFailed ||
		report.Markers.State != "intent_publication_observed_unverified" || report.Markers.IntentMarkerID == "" ||
		report.Markers.IntentDurable || report.Markers.PruneResumable || report.Writes.MarkerPublications != 1 ||
		report.Writes.MarkerDurabilityConfirms != 1 {
		t.Fatalf("post-publication failure was promoted to durable intent: %#v %v", report, err)
	}
}

func TestPruneInvalidLimitsStillReturnStructuredZeroWriteBlocker(t *testing.T) {
	report, err := Prune(context.Background(), PruneOptions{})
	if !errors.Is(err, ErrPolicy) || report.Outcome != RetentionOutcomeBlocked || report.WritesPerformed != 0 || len(report.Blockers) == 0 {
		t.Fatalf("invalid limits did not retain their zero-write blocker: %#v %v", report, err)
	}
}

func TestPruneAbandonedOperationDoesNotRequireMetafile(t *testing.T) {
	ctx := context.Background()
	session, rootInfo, targetRoot := bindMaterializeTestRoot(t)
	intent := testIntent()
	intent.TargetRootIdentity = rootInfo.Identity.String()
	handle, _, err := createJournal(ctx, session, intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.append(ctx, Event{Phase: PhaseAbandoned, Bytes: 0}); err != nil {
		t.Fatal(err)
	}
	operationID := handle.state.OperationID
	if err := handle.subtree.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := Prune(ctx, PruneOptions{
		TargetRoot: targetRoot, OperationID: operationID, ExpectedPlanID: intent.PlanID,
		JournalLimits: DefaultLimits(), RetentionLimits: DefaultRetentionLimits(),
	})
	if err != nil || report.Outcome != RetentionOutcomePruned || !report.Proof.CurrentPublicationAbsent ||
		report.Proof.Basis != RetentionBasisAbandoned || !report.Markers.ExactTombstone {
		t.Fatalf("unexpected abandoned prune: %#v %v", report, err)
	}
	if _, err := os.Lstat(filepath.Join(targetRoot, "torrent")); !os.IsNotExist(err) {
		t.Fatal("abandoned prune created or removed an unrelated final object")
	}
	assertExactRetentionTombstone(t, targetRoot, operationID)
}

func TestPrunePlanMismatchPerformsNoWrite(t *testing.T) {
	ctx := context.Background()
	session, rootInfo, targetRoot := bindMaterializeTestRoot(t)
	intent := testIntent()
	intent.TargetRootIdentity = rootInfo.Identity.String()
	handle, _, err := createJournal(ctx, session, intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.append(ctx, Event{Phase: PhaseAbandoned, Bytes: 0}); err != nil {
		t.Fatal(err)
	}
	operationID := handle.state.OperationID
	if err := handle.subtree.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := Prune(ctx, PruneOptions{
		TargetRoot: targetRoot, OperationID: operationID, ExpectedPlanID: strings.Repeat("0", 24),
		JournalLimits: DefaultLimits(), RetentionLimits: DefaultRetentionLimits(),
	})
	if err == nil || !errors.Is(err, ErrPolicy) || report.Outcome != RetentionOutcomeBlocked || report.WritesPerformed != 0 {
		t.Fatalf("plan mismatch was not a zero-write block: %#v %v", report, err)
	}
	directoryName, _ := OperationDirectoryName(operationID)
	if _, err := os.Lstat(filepath.Join(targetRoot, directoryName, retentionDirectoryName)); !os.IsNotExist(err) {
		t.Fatal("plan mismatch created retention state")
	}
}

func TestStatusDoesNotMisreportUnsealedRetentionBoundaryAsJournalCorruption(t *testing.T) {
	ctx := context.Background()
	session, rootInfo, targetRoot := bindMaterializeTestRoot(t)
	intent := testIntent()
	intent.TargetRootIdentity = rootInfo.Identity.String()
	handle, _, err := createJournal(ctx, session, intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.append(ctx, Event{Phase: PhaseAbandoned, Bytes: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.subtree.MkdirAll(ctx, mustFSPath(t, retentionDirectoryName)); err != nil {
		t.Fatal(err)
	}
	operationID := handle.state.OperationID
	if err := handle.subtree.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := Status(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
	if err != nil || report.Outcome != OutcomePruning || report.Operation.Status != "retention_initializing" || report.Operation.Resumable {
		t.Fatalf("unsealed retention boundary was misclassified: %#v %v", report, err)
	}
}

func TestPruneResumesFromDurableIntentWithoutMetafile(t *testing.T) {
	ctx := context.Background()
	meta, _, targetRoot, planID, operationID := committedOperationForPrune(t, ctx)
	crash := errors.New("stop after retention intent")
	retentionTransitionHook = func(stage string) error {
		if stage == "intent_published" {
			return crash
		}
		return nil
	}
	report, err := Prune(ctx, PruneOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID, ExpectedPlanID: planID,
		JournalLimits: DefaultLimits(), RetentionLimits: DefaultRetentionLimits(),
	})
	retentionTransitionHook = nil
	t.Cleanup(func() { retentionTransitionHook = nil })
	if !errors.Is(err, crash) || report.Markers.State != "intent_published" || !report.Markers.IntentDurable || !report.Markers.PruneResumable || report.Writes.FilesRemoved != 0 {
		t.Fatalf("durable intent boundary was not preserved: %#v %v", report, err)
	}
	resumed, err := Prune(ctx, PruneOptions{
		TargetRoot: targetRoot, OperationID: operationID, ExpectedPlanID: planID,
		JournalLimits: DefaultLimits(), RetentionLimits: DefaultRetentionLimits(),
	})
	if err != nil || resumed.Outcome != RetentionOutcomePruned || !resumed.Proof.HistoricalMarkerAuthority || !resumed.Markers.ExactTombstone {
		t.Fatalf("marker-authorized prune recovery failed: %#v %v", resumed, err)
	}
}

func TestPruneResumesAfterPartialIdentityBoundRemoval(t *testing.T) {
	ctx := context.Background()
	session, rootInfo, targetRoot := bindMaterializeTestRoot(t)
	intent := testIntent()
	intent.TargetRootIdentity = rootInfo.Identity.String()
	handle, _, err := createJournal(ctx, session, intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.append(ctx, Event{Phase: PhaseAbandoned, Bytes: 0}); err != nil {
		t.Fatal(err)
	}
	operationID := handle.state.OperationID
	if err := handle.subtree.Close(); err != nil {
		t.Fatal(err)
	}
	crash := errors.New("stop after one identity-bound removal")
	retentionRemovalHook = func(retentionNode, fsbind.Removal) error { return crash }
	report, err := Prune(ctx, PruneOptions{
		TargetRoot: targetRoot, OperationID: operationID, ExpectedPlanID: intent.PlanID,
		JournalLimits: DefaultLimits(), RetentionLimits: DefaultRetentionLimits(),
	})
	retentionRemovalHook = nil
	t.Cleanup(func() { retentionRemovalHook = nil })
	if !errors.Is(err, crash) || report.Writes.DirectoriesRemoved+report.Writes.FilesRemoved != 1 ||
		!report.Markers.IntentDurable || report.Markers.CompletionDurable || !report.Markers.PruneResumable {
		t.Fatalf("partial removal receipt was not preserved: %#v %v", report, err)
	}
	resumed, err := Prune(ctx, PruneOptions{
		TargetRoot: targetRoot, OperationID: operationID, ExpectedPlanID: intent.PlanID,
		JournalLimits: DefaultLimits(), RetentionLimits: DefaultRetentionLimits(),
	})
	if err != nil || resumed.Outcome != RetentionOutcomePruned || !resumed.Markers.ExactTombstone {
		t.Fatalf("partial deletion did not recover: %#v %v", resumed, err)
	}
}

func committedOperationForPrune(t *testing.T, ctx context.Context) (*metafile.MetaInfo, []byte, string, string, OperationID) {
	t.Helper()
	content := []byte("retention committed content")
	meta := materializeSingleV1Meta(t, "final.bin", content)
	searchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(searchRoot, "source"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	preflightMaterializeFilesystem(t, targetRoot)
	discovery := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	report, err := Run(ctx, RunOptions{
		Meta: meta, Discovery: &discovery, TargetRoot: targetRoot,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := ParseOperationID(report.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return meta, content, targetRoot, discovery.Plan.ID, operationID
}

func assertExactRetentionTombstone(t *testing.T, targetRoot string, operationID OperationID) {
	t.Helper()
	directoryName, _ := OperationDirectoryName(operationID)
	rootEntries, err := os.ReadDir(filepath.Join(targetRoot, directoryName))
	if err != nil {
		t.Fatal(err)
	}
	if len(rootEntries) != 2 || rootEntries[0].Name() != operationLockEntryName || rootEntries[1].Name() != retentionDirectoryName {
		t.Fatalf("unexpected retained operation root: %#v", rootEntries)
	}
	retentionEntries, err := os.ReadDir(filepath.Join(targetRoot, directoryName, retentionDirectoryName))
	if err != nil {
		t.Fatal(err)
	}
	if len(retentionEntries) != 2 || retentionEntries[0].Name() != retentionCompleteName || retentionEntries[1].Name() != retentionIntentFileName {
		t.Fatalf("unexpected retention marker namespace: %#v", retentionEntries)
	}
}
