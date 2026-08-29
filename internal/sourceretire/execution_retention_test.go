package sourceretire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func completedRetirementFixture(t *testing.T) (retireFixture, Plan, OperationID) {
	t.Helper()
	fixture := makeRetireFixture(t)
	preview, err := Build(context.Background(), retireBuildOptions(fixture, fixture.meta, &fixture.discovery, false))
	if err != nil || preview.Outcome != OutcomeEligible {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	session := executionSession(fixture, 3)
	report, err := Run(context.Background(), RunOptions{Review: BuildOptions{Meta: fixture.meta, Discovery: &fixture.discovery,
		Final: fixture.final, Activation: fixture.activation, ClientUse: fixture.currentUse, ClientSession: session},
		ExpectedPlanID: preview.Plan.ID, SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultExecutionLimits()})
	if err != nil || report.Outcome != ExecutionOutcomeRetired {
		t.Fatalf("run=%#v err=%v", report, err)
	}
	operation, err := ParseOperationID(report.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, preview.Plan, operation
}

func TestPruneExecutionRetainsExactTombstoneAndStatusRecognizesIt(t *testing.T) {
	fixture, plan, operation := completedRetirementFixture(t)
	options := ExecutionPruneOptions{TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: plan.ID,
		Acknowledge: true, JournalLimits: DefaultExecutionLimits(), RetentionLimits: DefaultExecutionRetentionLimits()}
	report, err := PruneExecution(context.Background(), options)
	if err != nil || report.Outcome != ExecutionRetentionOutcomePruned || !report.Markers.ExactTombstone ||
		report.Markers.PruneResumable || report.Writes.FilesRemoved < 3 || report.Writes.DirectoriesRemoved != 2 ||
		report.WritesPerformed == 0 || report.Operation.Status != "retained" {
		t.Fatalf("prune=%#v err=%v", report, err)
	}
	directory, _ := OperationDirectoryName(operation)
	rootEntries, err := os.ReadDir(filepath.Join(fixture.targetRoot, directory))
	if err != nil {
		t.Fatal(err)
	}
	if len(rootEntries) != 2 || rootEntries[0].Name() != ".fsbind-operation.lock" || rootEntries[1].Name() != executionRetentionDirectory {
		t.Fatalf("unexpected retained namespace: %#v", rootEntries)
	}
	markerEntries, err := os.ReadDir(filepath.Join(fixture.targetRoot, directory, executionRetentionDirectory))
	if err != nil {
		t.Fatal(err)
	}
	if len(markerEntries) != 2 || markerEntries[0].Name() != executionRetentionCompleteFile || markerEntries[1].Name() != executionRetentionIntentFile {
		t.Fatalf("unexpected retained marker namespace: %#v", markerEntries)
	}
	finalPath, _, ok := fixture.final.ProcessFilePath(0)
	if !ok {
		t.Fatal("final path unavailable")
	}
	if raw, err := os.ReadFile(finalPath); err != nil || string(raw) != "retirement source fixture" {
		t.Fatalf("prune changed final: %q %v", raw, err)
	}
	if _, err := os.Lstat(fixture.sourcePath); !os.IsNotExist(err) {
		t.Fatalf("prune recreated retired source: %v", err)
	}

	status, err := Status(context.Background(), StatusOptions{TargetRoot: fixture.targetRoot, OperationID: operation, Limits: DefaultExecutionLimits()})
	if err != nil || status.Outcome != ExecutionOutcomeAlreadyRetired || status.Operation.Status != "retained" || status.WritesPerformed != 0 || status.Operation.Resumable {
		t.Fatalf("retained status=%#v err=%v", status, err)
	}
	resumed, err := Resume(context.Background(), ResumeOptions{TargetRoot: fixture.targetRoot, OperationID: operation,
		ExpectedPlanID: plan.ID, Acknowledge: true, Limits: DefaultExecutionLimits()})
	if err != nil || resumed.Outcome != ExecutionOutcomeAlreadyRetired || resumed.Operation.Status != "retained" || resumed.WritesPerformed != 0 {
		t.Fatalf("retained resume=%#v err=%v", resumed, err)
	}

	repeated, err := PruneExecution(context.Background(), options)
	if err != nil || repeated.Outcome != ExecutionRetentionOutcomeAlreadyPruned || repeated.WritesPerformed != 0 || !repeated.Markers.ExactTombstone {
		t.Fatalf("repeated prune=%#v err=%v", repeated, err)
	}
	rawReport, err := json.Marshal(repeated)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{fixture.sourceRoot, fixture.sourcePath, fixture.targetRoot, filepath.Base(fixture.sourcePath)} {
		if bytes.Contains(rawReport, []byte(secret)) {
			t.Fatalf("retention report leaked %q: %s", secret, rawReport)
		}
	}
}

func TestPruneExecutionIntentMakesInterruptedDeletionResumable(t *testing.T) {
	fixture, plan, operation := completedRetirementFixture(t)
	stop := errors.New("stop after durable retention intent")
	executionRetentionTransitionHook = func(stage string) error {
		if stage == "intent_published" {
			return stop
		}
		return nil
	}
	t.Cleanup(func() { executionRetentionTransitionHook = nil })
	options := ExecutionPruneOptions{TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: plan.ID,
		Acknowledge: true, JournalLimits: DefaultExecutionLimits(), RetentionLimits: DefaultExecutionRetentionLimits()}
	report, err := PruneExecution(context.Background(), options)
	if !errors.Is(err, stop) || report.Outcome != ExecutionRetentionOutcomeInterrupted || !report.Markers.IntentDurable ||
		!report.Markers.PruneResumable || report.Writes.FilesRemoved != 0 {
		t.Fatalf("interrupted prune=%#v err=%v", report, err)
	}
	status, statusErr := Status(context.Background(), StatusOptions{TargetRoot: fixture.targetRoot, OperationID: operation, Limits: DefaultExecutionLimits()})
	if statusErr != nil || status.Outcome != ExecutionOutcomePartial || status.Operation.Status != "pruning" || status.Operation.Resumable {
		t.Fatalf("in-progress retention status=%#v err=%v", status, statusErr)
	}
	executionRetentionTransitionHook = nil
	resumed, err := PruneExecution(context.Background(), options)
	if err != nil || resumed.Outcome != ExecutionRetentionOutcomePruned || !resumed.Markers.ExactTombstone {
		t.Fatalf("resumed prune=%#v err=%v", resumed, err)
	}
}

func TestPruneExecutionPolicyBlockersAreZeroWrite(t *testing.T) {
	fixture, plan, operation := completedRetirementFixture(t)
	directory, _ := OperationDirectoryName(operation)
	before, err := os.ReadDir(filepath.Join(fixture.targetRoot, directory))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		ack  bool
		plan string
	}{
		{name: "missing acknowledgement", ack: false, plan: plan.ID},
		{name: "plan mismatch", ack: true, plan: "sha256:" + string(bytes.Repeat([]byte{'a'}, 64))},
	} {
		t.Run(test.name, func(t *testing.T) {
			report, err := PruneExecution(context.Background(), ExecutionPruneOptions{TargetRoot: fixture.targetRoot,
				OperationID: operation, ExpectedPlanID: test.plan, Acknowledge: test.ack,
				JournalLimits: DefaultExecutionLimits(), RetentionLimits: DefaultExecutionRetentionLimits()})
			if report.Outcome != ExecutionRetentionOutcomeBlocked || report.WritesPerformed != 0 {
				t.Fatalf("blocked prune=%#v err=%v", report, err)
			}
			after, readErr := os.ReadDir(filepath.Join(fixture.targetRoot, directory))
			if readErr != nil || len(after) != len(before) {
				t.Fatalf("blocked prune changed namespace: before=%d after=%d err=%v", len(before), len(after), readErr)
			}
		})
	}
}

func TestPruneExecutionRecoversAfterHeavyStateRemoval(t *testing.T) {
	fixture, plan, operation := completedRetirementFixture(t)
	stop := errors.New("stop after heavy operation state removal")
	executionRetentionTransitionHook = func(stage string) error {
		if stage == "heavy_state_removed" {
			return stop
		}
		return nil
	}
	t.Cleanup(func() { executionRetentionTransitionHook = nil })
	options := ExecutionPruneOptions{TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: plan.ID,
		Acknowledge: true, JournalLimits: DefaultExecutionLimits(), RetentionLimits: DefaultExecutionRetentionLimits()}
	report, err := PruneExecution(context.Background(), options)
	if !errors.Is(err, stop) || report.Outcome != ExecutionRetentionOutcomeInterrupted || report.Writes.FilesRemoved == 0 ||
		!report.Markers.IntentDurable || !report.Markers.PruneResumable || report.Markers.CompletionDurable {
		t.Fatalf("post-removal interruption=%#v err=%v", report, err)
	}
	status, err := Status(context.Background(), StatusOptions{TargetRoot: fixture.targetRoot, OperationID: operation, Limits: DefaultExecutionLimits()})
	if err != nil || status.Outcome != ExecutionOutcomePartial || status.Operation.Status != "pruning" {
		t.Fatalf("post-removal status=%#v err=%v", status, err)
	}
	executionRetentionTransitionHook = nil
	recovered, err := PruneExecution(context.Background(), options)
	if err != nil || recovered.Outcome != ExecutionRetentionOutcomePruned || !recovered.Markers.ExactTombstone ||
		recovered.Writes.FilesRemoved != 0 || recovered.Writes.DirectoriesRemoved != 0 {
		t.Fatalf("post-removal recovery=%#v err=%v", recovered, err)
	}
}

func TestPruneExecutionRejectsNonterminalAndPredictableBudgetBeforeRetentionWrite(t *testing.T) {
	for _, budgetLimited := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonterminal", true: "inventory budget"}[budgetLimited], func(t *testing.T) {
			fixture := makeRetireFixture(t)
			preview, err := Build(context.Background(), retireBuildOptions(fixture, fixture.meta, &fixture.discovery, false))
			if err != nil {
				t.Fatal(err)
			}
			var operation OperationID
			if budgetLimited {
				_, _, operation = completedRetirementFixtureFrom(t, fixture, preview.Plan)
			} else {
				operation = createAttemptedRetirement(t, fixture, preview.Plan, false)
			}
			limits := DefaultExecutionRetentionLimits()
			if budgetLimited {
				limits.MaxObjects = 1
			}
			report, pruneErr := PruneExecution(context.Background(), ExecutionPruneOptions{TargetRoot: fixture.targetRoot,
				OperationID: operation, ExpectedPlanID: preview.Plan.ID, Acknowledge: true,
				JournalLimits: DefaultExecutionLimits(), RetentionLimits: limits})
			if report.Outcome != ExecutionRetentionOutcomeBlocked || report.WritesPerformed != 0 {
				t.Fatalf("zero-write prune blocker=%#v err=%v", report, pruneErr)
			}
			directory, _ := OperationDirectoryName(operation)
			if _, err := os.Lstat(filepath.Join(fixture.targetRoot, directory, executionRetentionDirectory)); !os.IsNotExist(err) {
				t.Fatalf("blocked prune created retention state: %v", err)
			}
		})
	}
}

func completedRetirementFixtureFrom(t *testing.T, fixture retireFixture, plan Plan) (retireFixture, Plan, OperationID) {
	t.Helper()
	session := executionSession(fixture, 3)
	report, err := Run(context.Background(), RunOptions{Review: BuildOptions{Meta: fixture.meta, Discovery: &fixture.discovery,
		Final: fixture.final, Activation: fixture.activation, ClientUse: fixture.currentUse, ClientSession: session},
		ExpectedPlanID: plan.ID, SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultExecutionLimits()})
	if err != nil || report.Outcome != ExecutionOutcomeRetired {
		t.Fatalf("run=%#v err=%v", report, err)
	}
	operation, err := ParseOperationID(report.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, plan, operation
}

func TestRetainedExecutionStatusRejectsCorruptOrExtraControlState(t *testing.T) {
	for _, extra := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed marker", true: "extra object"}[extra], func(t *testing.T) {
			fixture, plan, operation := completedRetirementFixture(t)
			_, err := PruneExecution(context.Background(), ExecutionPruneOptions{TargetRoot: fixture.targetRoot,
				OperationID: operation, ExpectedPlanID: plan.ID, Acknowledge: true,
				JournalLimits: DefaultExecutionLimits(), RetentionLimits: DefaultExecutionRetentionLimits()})
			if err != nil {
				t.Fatal(err)
			}
			directory, _ := OperationDirectoryName(operation)
			retentionRoot := filepath.Join(fixture.targetRoot, directory, executionRetentionDirectory)
			if extra {
				if err := os.WriteFile(filepath.Join(retentionRoot, "unexpected"), []byte("private canary"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(retentionRoot, executionRetentionIntentFile), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			status, statusErr := Status(context.Background(), StatusOptions{TargetRoot: fixture.targetRoot, OperationID: operation, Limits: DefaultExecutionLimits()})
			if statusErr == nil || !errors.Is(statusErr, ErrExecutionIntegrity) || status.Outcome != ExecutionOutcomeIntegrity || status.Operation.Resumable {
				t.Fatalf("corrupt retained status=%#v err=%v", status, statusErr)
			}
		})
	}
}
