package sourceretire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

func completedParentCleanupFixture(t *testing.T) (retireFixture, Plan, OperationID, ParentCleanupReport, ParentCleanupOperationID, string) {
	t.Helper()
	fixture, retirementPlan, retirementOperation, parent := completedNestedRetirementFixture(t)
	preview, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, retirementOperation))
	if err != nil || preview.Outcome != ParentCleanupOutcomeEligible {
		t.Fatalf("parent cleanup preview=%#v err=%v", preview, err)
	}
	report, err := RunParentCleanup(context.Background(), parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, preview.Plan.ID))
	if err != nil || report.Outcome != ParentCleanupExecutionOutcomeRemoved {
		t.Fatalf("parent cleanup run=%#v err=%v", report, err)
	}
	operation, err := ParseParentCleanupOperationID(report.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, retirementPlan, retirementOperation, preview, operation, parent
}

func parentCleanupPruneOptions(fixture retireFixture, preview ParentCleanupReport, operation ParentCleanupOperationID) ParentCleanupPruneOptions {
	return ParentCleanupPruneOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedCleanupPlanID: preview.Plan.ID,
		Acknowledge: true, JournalLimits: DefaultParentCleanupExecutionLimits(),
		RetentionLimits: DefaultExecutionRetentionLimits(),
	}
}

func retainedParentCleanupFixture(t *testing.T) (retireFixture, Plan, OperationID, ParentCleanupReport, ParentCleanupOperationID, string) {
	t.Helper()
	fixture, retirementPlan, retirementOperation, preview, operation, parent := completedParentCleanupFixture(t)
	report, err := PruneParentCleanup(context.Background(), parentCleanupPruneOptions(fixture, preview, operation))
	if err != nil || report.Outcome != ParentCleanupRetentionOutcomePruned || !report.Markers.ExactTombstone {
		t.Fatalf("parent cleanup prune=%#v err=%v", report, err)
	}
	return fixture, retirementPlan, retirementOperation, preview, operation, parent
}

func parentCleanupForgetOptions(fixture retireFixture, preview ParentCleanupReport, operation ParentCleanupOperationID) ParentCleanupForgetOptions {
	return ParentCleanupForgetOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedCleanupPlanID: preview.Plan.ID,
		Acknowledge: true, Limits: DefaultParentCleanupForgetLimits(),
	}
}

func TestPruneParentCleanupRetainsExactNoPathTombstone(t *testing.T) {
	fixture, retirementPlan, retirementOperation, preview, operation, parent := completedParentCleanupFixture(t)
	options := parentCleanupPruneOptions(fixture, preview, operation)
	report, err := PruneParentCleanup(context.Background(), options)
	if err != nil || report.Outcome != ParentCleanupRetentionOutcomePruned || !report.Markers.ExactTombstone ||
		report.Markers.PruneResumable || report.Writes.FilesRemoved < 3 || report.Writes.DirectoriesRemoved != 2 ||
		report.WritesPerformed == 0 || report.Operation.Status != "retained" || report.Proof.ParentsRemoved != 1 ||
		report.Proof.RetiredFiles != 1 {
		t.Fatalf("prune=%#v err=%v", report, err)
	}
	operationName, _ := ParentCleanupOperationDirectoryName(operation)
	rootEntries, err := os.ReadDir(filepath.Join(fixture.targetRoot, operationName))
	if err != nil || len(rootEntries) != 2 || rootEntries[0].Name() != ".fsbind-operation.lock" || rootEntries[1].Name() != parentCleanupRetentionDirectory {
		t.Fatalf("retained root=%#v err=%v", rootEntries, err)
	}
	markerEntries, err := os.ReadDir(filepath.Join(fixture.targetRoot, operationName, parentCleanupRetentionDirectory))
	if err != nil || len(markerEntries) != 2 || markerEntries[0].Name() != parentCleanupRetentionCompleteFile || markerEntries[1].Name() != parentCleanupRetentionIntentFile {
		t.Fatalf("retained markers=%#v err=%v", markerEntries, err)
	}

	status, err := ParentCleanupStatus(context.Background(), ParentCleanupStatusOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, Limits: DefaultParentCleanupExecutionLimits(),
	})
	if err != nil || status.Outcome != ParentCleanupExecutionOutcomeAlreadyRemoved || status.Operation.Status != "retained" ||
		status.Operation.Resumable || status.WritesPerformed != 0 || len(status.Directories) != 0 ||
		containsString(status.Effect, "read_private_parent_cleanup_journal") {
		t.Fatalf("retained status=%#v err=%v", status, err)
	}
	resumed, err := ResumeParentCleanup(context.Background(), ParentCleanupResumeOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedCleanupPlanID: preview.Plan.ID,
		SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultParentCleanupExecutionLimits(),
	})
	if err != nil || resumed.Outcome != ParentCleanupExecutionOutcomeAlreadyRemoved || resumed.Operation.Status != "retained" || resumed.WritesPerformed != 0 {
		t.Fatalf("retained resume=%#v err=%v", resumed, err)
	}
	rerun, err := RunParentCleanup(context.Background(), parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, preview.Plan.ID))
	if err != nil || rerun.Outcome != ParentCleanupExecutionOutcomeAlreadyRemoved || rerun.Operation.Status != "retained" || rerun.WritesPerformed != 0 {
		t.Fatalf("retained run=%#v err=%v", rerun, err)
	}
	repeated, err := PruneParentCleanup(context.Background(), options)
	if err != nil || repeated.Outcome != ParentCleanupRetentionOutcomeAlreadyPruned || repeated.WritesPerformed != 0 || !repeated.Markers.ExactTombstone {
		t.Fatalf("repeated prune=%#v err=%v", repeated, err)
	}
	raw, err := json.Marshal(repeated)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{fixture.sourceRoot, fixture.targetRoot, fixture.sourcePath, parent, filepath.Base(parent)} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("retention report leaked %q: %s", secret, raw)
		}
	}
}

func TestPruneParentCleanupRecoversAcrossDurableBoundaries(t *testing.T) {
	for _, stopPhase := range []string{"intent_published", "heavy_state_removed"} {
		t.Run(stopPhase, func(t *testing.T) {
			fixture, _, _, preview, operation, _ := completedParentCleanupFixture(t)
			stop := errors.New("stop after " + stopPhase)
			parentCleanupRetentionTransitionHook = func(phase string) error {
				if phase == stopPhase {
					return stop
				}
				return nil
			}
			t.Cleanup(func() { parentCleanupRetentionTransitionHook = nil })
			options := parentCleanupPruneOptions(fixture, preview, operation)
			report, err := PruneParentCleanup(context.Background(), options)
			if !errors.Is(err, stop) || report.Outcome != ParentCleanupRetentionOutcomeInterrupted ||
				!report.Markers.IntentDurable || !report.Markers.PruneResumable {
				t.Fatalf("interrupted prune=%#v err=%v", report, err)
			}
			status, statusErr := ParentCleanupStatus(context.Background(), ParentCleanupStatusOptions{
				TargetRoot: fixture.targetRoot, OperationID: operation, Limits: DefaultParentCleanupExecutionLimits(),
			})
			if statusErr != nil || status.Outcome != ParentCleanupExecutionOutcomePartial || status.Operation.Status != "pruning" || status.Operation.Resumable {
				t.Fatalf("pruning status=%#v err=%v", status, statusErr)
			}
			parentCleanupRetentionTransitionHook = nil
			recovered, err := PruneParentCleanup(context.Background(), options)
			if err != nil || recovered.Outcome != ParentCleanupRetentionOutcomePruned || !recovered.Markers.ExactTombstone {
				t.Fatalf("recovered prune=%#v err=%v", recovered, err)
			}
		})
	}
}

func TestPruneParentCleanupReestablishesIntentBeforeDeletingHeavyState(t *testing.T) {
	fixture, _, _, preview, operation, _ := completedParentCleanupFixture(t)
	stop := errors.New("stop after retention intent")
	parentCleanupRetentionTransitionHook = func(phase string) error {
		if phase == "intent_published" {
			return stop
		}
		return nil
	}
	t.Cleanup(func() { parentCleanupRetentionTransitionHook = nil })
	options := parentCleanupPruneOptions(fixture, preview, operation)
	interrupted, err := PruneParentCleanup(context.Background(), options)
	if !errors.Is(err, stop) || interrupted.Outcome != ParentCleanupRetentionOutcomeInterrupted || !interrupted.Markers.IntentDurable {
		t.Fatalf("interrupted prune=%#v err=%v", interrupted, err)
	}

	operationName, _ := ParentCleanupOperationDirectoryName(operation)
	intentRaw, err := os.ReadFile(filepath.Join(fixture.targetRoot, operationName, parentCleanupRetentionDirectory, parentCleanupRetentionIntentFile))
	if err != nil {
		t.Fatal(err)
	}
	_, intentID, err := DecodeParentCleanupRetentionIntent(bytes.NewReader(intentRaw))
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := fsbind.BindExisting(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	subtree, err := target.OpenPrivateSubtreeObserved(operationName)
	if err != nil {
		_ = target.Close()
		t.Fatal(err)
	}
	pendingName := parentCleanupRetentionTemporaryName("intent", intentID)
	pendingPath, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory, pendingName})
	file, err := subtree.CreateRegular(context.Background(), pendingPath)
	if err != nil {
		_ = subtree.Close()
		_ = target.Close()
		t.Fatal(err)
	}
	if _, err := writeExecutionBytes(context.Background(), file, intentRaw); err != nil {
		_ = file.Close()
		_ = subtree.Close()
		_ = target.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = subtree.Close()
		_ = target.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		_ = subtree.Close()
		_ = target.Close()
		t.Fatal(err)
	}
	retentionPath, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory})
	if err := subtree.SyncDirectory(context.Background(), retentionPath); err != nil {
		_ = subtree.Close()
		_ = target.Close()
		t.Fatal(err)
	}
	if err := subtree.Close(); err != nil {
		_ = target.Close()
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}

	parentCleanupRetentionTransitionHook = nil
	recovered, err := PruneParentCleanup(context.Background(), options)
	if err != nil || recovered.Outcome != ParentCleanupRetentionOutcomePruned || !recovered.Markers.ExactTombstone ||
		recovered.Writes.MarkerTemporaryRemovals != 1 {
		t.Fatalf("recovered prune=%#v err=%v", recovered, err)
	}
}

func TestPruneParentCleanupPolicyIsZeroWrite(t *testing.T) {
	fixture, _, _, preview, operation, _ := completedParentCleanupFixture(t)
	operationName, _ := ParentCleanupOperationDirectoryName(operation)
	operationRoot := filepath.Join(fixture.targetRoot, operationName)
	before, err := os.ReadDir(operationRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ParentCleanupPruneOptions){
		func(options *ParentCleanupPruneOptions) { options.Acknowledge = false },
		func(options *ParentCleanupPruneOptions) { options.RetentionLimits.MaxObjects = 1 },
	} {
		options := parentCleanupPruneOptions(fixture, preview, operation)
		mutate(&options)
		report, err := PruneParentCleanup(context.Background(), options)
		if report.Outcome != ParentCleanupRetentionOutcomeBlocked || report.WritesPerformed != 0 {
			t.Fatalf("blocked prune=%#v err=%v", report, err)
		}
		after, readErr := os.ReadDir(operationRoot)
		if readErr != nil || len(after) != len(before) {
			t.Fatalf("blocked prune changed operation: before=%d after=%d err=%v", len(before), len(after), readErr)
		}
		if _, statErr := os.Lstat(filepath.Join(operationRoot, parentCleanupRetentionDirectory)); !os.IsNotExist(statErr) {
			t.Fatalf("blocked prune created retention state: %v", statErr)
		}
	}
}

func TestPruneParentCleanupRejectsUnexpectedRetainedObject(t *testing.T) {
	fixture, _, _, preview, operation, _ := retainedParentCleanupFixture(t)
	createParentCleanupPrivateRegular(t, fixture.targetRoot, operation,
		[]string{parentCleanupRetentionDirectory, "unexpected.json"}, []byte("{}\n"))

	pruned, pruneErr := PruneParentCleanup(context.Background(), parentCleanupPruneOptions(fixture, preview, operation))
	if !errors.Is(pruneErr, ErrExecutionIntegrity) || pruned.Outcome != ParentCleanupRetentionOutcomeIntegrityFailed ||
		pruned.WritesPerformed != 0 || pruned.Markers.PruneResumable {
		t.Fatalf("prune with unexpected tombstone object=%#v err=%v", pruned, pruneErr)
	}
	forgotten, forgetErr := ForgetParentCleanupTombstone(context.Background(), parentCleanupForgetOptions(fixture, preview, operation))
	if !errors.Is(forgetErr, ErrExecutionIntegrity) || forgotten.Outcome != ParentCleanupForgetOutcomeIntegrityFailed ||
		forgotten.WritesPerformed != 0 || forgotten.Authority.MarkerDurable {
		t.Fatalf("forget with unexpected tombstone object=%#v err=%v", forgotten, forgetErr)
	}
}

func TestForgetParentCleanupRequiresCompleteRetentionTombstone(t *testing.T) {
	fixture, _, _, preview, operation, _ := completedParentCleanupFixture(t)
	report, err := ForgetParentCleanupTombstone(context.Background(), parentCleanupForgetOptions(fixture, preview, operation))
	if err != nil || report.Outcome != ParentCleanupForgetOutcomeBlocked || report.WritesPerformed != 0 ||
		!hasFinding(report.Blockers, "operation.prune_required") {
		t.Fatalf("unpruned forget=%#v err=%v", report, err)
	}
	rootName, _ := ParentCleanupForgetRootName(operation)
	if _, statErr := os.Lstat(filepath.Join(fixture.targetRoot, rootName)); !os.IsNotExist(statErr) {
		t.Fatalf("blocked forget published a root marker: %v", statErr)
	}
}

func TestParentCleanupRetentionAndForgetPreCancelledAreZeroWrite(t *testing.T) {
	t.Run("prune", func(t *testing.T) {
		fixture, _, _, preview, operation, _ := completedParentCleanupFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		report, err := PruneParentCleanup(ctx, parentCleanupPruneOptions(fixture, preview, operation))
		if !errors.Is(err, context.Canceled) || report.Outcome != ParentCleanupRetentionOutcomeInterrupted || report.WritesPerformed != 0 {
			t.Fatalf("pre-cancelled prune=%#v err=%v", report, err)
		}
		operationName, _ := ParentCleanupOperationDirectoryName(operation)
		if _, statErr := os.Lstat(filepath.Join(fixture.targetRoot, operationName, parentCleanupRetentionDirectory)); !os.IsNotExist(statErr) {
			t.Fatalf("pre-cancelled prune created retention state: %v", statErr)
		}
	})
	t.Run("forget", func(t *testing.T) {
		fixture, _, _, preview, operation, _ := retainedParentCleanupFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		report, err := ForgetParentCleanupTombstone(ctx, parentCleanupForgetOptions(fixture, preview, operation))
		if !errors.Is(err, context.Canceled) || report.Outcome != ParentCleanupForgetOutcomeInterrupted || report.WritesPerformed != 0 {
			t.Fatalf("pre-cancelled forget=%#v err=%v", report, err)
		}
		rootName, _ := ParentCleanupForgetRootName(operation)
		if _, statErr := os.Lstat(filepath.Join(fixture.targetRoot, rootName)); !os.IsNotExist(statErr) {
			t.Fatalf("pre-cancelled forget created root intent: %v", statErr)
		}
	})
}

func createParentCleanupPrivateRegular(t *testing.T, targetRoot string, operation ParentCleanupOperationID, components []string, raw []byte) {
	t.Helper()
	target, _, err := fsbind.BindExisting(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	operationName, _ := ParentCleanupOperationDirectoryName(operation)
	subtree, err := target.OpenPrivateSubtreeObserved(operationName)
	if err != nil {
		_ = target.Close()
		t.Fatal(err)
	}
	path, err := fsbind.PathFromComponents(components)
	if err != nil {
		_ = subtree.Close()
		_ = target.Close()
		t.Fatal(err)
	}
	file, err := subtree.CreateRegular(context.Background(), path)
	if err != nil {
		_ = subtree.Close()
		_ = target.Close()
		t.Fatal(err)
	}
	if _, err := writeExecutionBytes(context.Background(), file, raw); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil && len(components) > 1 {
		parent, pathErr := fsbind.PathFromComponents(components[:len(components)-1])
		if pathErr != nil {
			err = pathErr
		} else {
			err = subtree.SyncDirectory(context.Background(), parent)
		}
	}
	closeSubtreeErr := subtree.Close()
	closeTargetErr := target.Close()
	if err == nil {
		err = closeSubtreeErr
	}
	if err == nil {
		err = closeTargetErr
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestForgetParentCleanupTombstoneRemovesLastHistoricalEvidence(t *testing.T) {
	fixture, _, _, preview, operation, parent := retainedParentCleanupFixture(t)
	report, err := ForgetParentCleanupTombstone(context.Background(), parentCleanupForgetOptions(fixture, preview, operation))
	if err != nil || report.Outcome != ParentCleanupForgetOutcomeForgotten || !report.Authority.TargetHistoricalEvidenceErased ||
		report.Authority.MarkerDurable || report.WritesPerformed == 0 || report.Operation.Resumable ||
		!report.RootIntentPublication.Published || !report.Removals.RetentionIntent.Removed ||
		!report.Removals.RetentionComplete.Removed || !report.Removals.RetentionDirectory.Removed ||
		!report.Removals.OperationSubtree.Removed || !report.Removals.RootIntent.Removed {
		t.Fatalf("forget=%#v err=%v", report, err)
	}
	operationName, _ := ParentCleanupOperationDirectoryName(operation)
	rootName, _ := ParentCleanupForgetRootName(operation)
	for _, path := range []string{filepath.Join(fixture.targetRoot, operationName), filepath.Join(fixture.targetRoot, rootName)} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("forgotten control object remains at %q: %v", path, statErr)
		}
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{fixture.sourceRoot, fixture.targetRoot, fixture.sourcePath, parent, filepath.Base(parent)} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("forget report leaked %q: %s", secret, raw)
		}
	}
	repeated, repeatErr := ForgetParentCleanupTombstone(context.Background(), parentCleanupForgetOptions(fixture, preview, operation))
	if !errors.Is(repeatErr, ErrOperationNotFound) || repeated.Outcome != ParentCleanupForgetOutcomeAbsentUnattributed ||
		repeated.WritesPerformed != 0 || repeated.Authority.TargetHistoricalEvidenceErased {
		t.Fatalf("repeated forget=%#v err=%v", repeated, repeatErr)
	}
}

func TestForgetParentCleanupTombstoneRecoversAcrossDurableBoundaries(t *testing.T) {
	for _, stopPhase := range []string{"root_intent_published", "operation_removed"} {
		t.Run(stopPhase, func(t *testing.T) {
			fixture, _, _, preview, operation, _ := retainedParentCleanupFixture(t)
			stop := errors.New("stop after " + stopPhase)
			parentCleanupForgetTransitionHook = func(phase string) error {
				if phase == stopPhase {
					return stop
				}
				return nil
			}
			t.Cleanup(func() { parentCleanupForgetTransitionHook = nil })
			options := parentCleanupForgetOptions(fixture, preview, operation)
			report, err := ForgetParentCleanupTombstone(context.Background(), options)
			if !errors.Is(err, stop) || report.Outcome != ParentCleanupForgetOutcomeInterrupted ||
				!report.Authority.MarkerDurable || !report.Operation.Resumable {
				t.Fatalf("interrupted forget=%#v err=%v", report, err)
			}
			status, statusErr := ParentCleanupStatus(context.Background(), ParentCleanupStatusOptions{
				TargetRoot: fixture.targetRoot, OperationID: operation, Limits: DefaultParentCleanupExecutionLimits(),
			})
			if statusErr != nil || status.Outcome != ParentCleanupExecutionOutcomeIncomplete || status.Operation.Status != "forgetting" ||
				status.Operation.Resumable || status.WritesPerformed != 0 || !hasFinding(status.Blockers, "operation.forget_required") ||
				containsString(status.Effect, "read_private_parent_cleanup_journal") {
				t.Fatalf("forget status=%#v err=%v", status, statusErr)
			}
			prune, pruneErr := PruneParentCleanup(context.Background(), parentCleanupPruneOptions(fixture, preview, operation))
			if pruneErr != nil || prune.Outcome != ParentCleanupRetentionOutcomeBlocked || prune.WritesPerformed != 0 {
				t.Fatalf("forget-boundary prune=%#v err=%v", prune, pruneErr)
			}
			parentCleanupForgetTransitionHook = nil
			recovered, err := ForgetParentCleanupTombstone(context.Background(), options)
			if err != nil || recovered.Outcome != ParentCleanupForgetOutcomeForgotten || !recovered.Authority.TargetHistoricalEvidenceErased {
				t.Fatalf("recovered forget=%#v err=%v", recovered, err)
			}
		})
	}
}
