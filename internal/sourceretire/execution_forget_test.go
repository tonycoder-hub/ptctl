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

func retainedRetirementFixture(t *testing.T) (retireFixture, Plan, OperationID) {
	t.Helper()
	fixture, plan, operation := completedRetirementFixture(t)
	report, err := PruneExecution(context.Background(), ExecutionPruneOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: plan.ID, Acknowledge: true,
		JournalLimits: DefaultExecutionLimits(), RetentionLimits: DefaultExecutionRetentionLimits(),
	})
	if err != nil || report.Outcome != ExecutionRetentionOutcomePruned || !report.Markers.ExactTombstone {
		t.Fatalf("prune=%#v err=%v", report, err)
	}
	return fixture, plan, operation
}

func forgetOptions(fixture retireFixture, plan Plan, operation OperationID) ExecutionForgetOptions {
	return ExecutionForgetOptions{TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: plan.ID,
		Acknowledge: true, Limits: DefaultExecutionForgetLimits()}
}

func TestForgetExecutionTombstoneRemovesLastHistoricalEvidence(t *testing.T) {
	fixture, plan, operation := retainedRetirementFixture(t)
	report, err := ForgetExecutionTombstone(context.Background(), forgetOptions(fixture, plan, operation))
	if err != nil || report.Outcome != ExecutionForgetOutcomeForgotten || !report.Authority.TargetHistoricalEvidenceErased ||
		report.Authority.MarkerDurable || report.WritesPerformed == 0 || report.Operation.Resumable ||
		!report.RootIntentPublication.Published || !report.Removals.RetentionIntent.Removed ||
		!report.Removals.RetentionComplete.Removed || !report.Removals.RetentionDirectory.Removed ||
		!report.Removals.OperationSubtree.Removed || !report.Removals.RootIntent.Removed {
		t.Fatalf("forget=%#v err=%v", report, err)
	}
	operationName, _ := OperationDirectoryName(operation)
	rootName, _ := ExecutionForgetRootName(operation)
	for _, path := range []string{filepath.Join(fixture.targetRoot, operationName), filepath.Join(fixture.targetRoot, rootName)} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("forgotten control object remains at %q: %v", path, statErr)
		}
	}
	finalPath, _, ok := fixture.final.ProcessFilePath(0)
	if !ok {
		t.Fatal("final path unavailable")
	}
	if raw, readErr := os.ReadFile(finalPath); readErr != nil || string(raw) != "retirement source fixture" {
		t.Fatalf("forget changed final: %q %v", raw, readErr)
	}
	if _, statErr := os.Lstat(fixture.sourcePath); !os.IsNotExist(statErr) {
		t.Fatalf("forget recreated retired source: %v", statErr)
	}
	rawReport, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, secret := range []string{fixture.sourceRoot, fixture.sourcePath, fixture.targetRoot, filepath.Base(fixture.sourcePath)} {
		if bytes.Contains(rawReport, []byte(secret)) {
			t.Fatalf("forget report leaked %q: %s", secret, rawReport)
		}
	}

	repeated, repeatErr := ForgetExecutionTombstone(context.Background(), forgetOptions(fixture, plan, operation))
	if !errors.Is(repeatErr, ErrOperationNotFound) || repeated.Outcome != ExecutionForgetOutcomeAbsentUnattributed ||
		repeated.WritesPerformed != 0 || repeated.Authority.TargetHistoricalEvidenceErased {
		t.Fatalf("repeated forget=%#v err=%v", repeated, repeatErr)
	}
}

func TestForgetExecutionTombstoneRecoversAcrossBothDurableBoundaries(t *testing.T) {
	for _, stopPhase := range []string{"root_intent_published", "operation_removed"} {
		t.Run(stopPhase, func(t *testing.T) {
			fixture, plan, operation := retainedRetirementFixture(t)
			stop := errors.New("stop after " + stopPhase)
			executionForgetTransitionHook = func(phase string) error {
				if phase == stopPhase {
					return stop
				}
				return nil
			}
			t.Cleanup(func() { executionForgetTransitionHook = nil })
			report, err := ForgetExecutionTombstone(context.Background(), forgetOptions(fixture, plan, operation))
			if !errors.Is(err, stop) || report.Outcome != ExecutionForgetOutcomeInterrupted || !report.Authority.MarkerDurable || !report.Operation.Resumable {
				t.Fatalf("interrupted forget=%#v err=%v", report, err)
			}
			rootName, _ := ExecutionForgetRootName(operation)
			rootPath := filepath.Join(fixture.targetRoot, rootName)
			if _, statErr := os.Lstat(rootPath); statErr != nil {
				t.Fatalf("durable root intent missing: %v", statErr)
			}
			if parsed, parseErr := ParseExecutionForgetRootName(rootName); parseErr != nil || parsed != operation {
				t.Fatalf("root marker name parsed as %s: %v", parsed, parseErr)
			}
			if stopPhase == "root_intent_published" {
				raw, readErr := os.ReadFile(rootPath)
				marker, markerID, decodeErr := DecodeExecutionForgetIntent(bytes.NewReader(raw))
				canonical, canonicalID, encodeErr := EncodeExecutionForgetIntent(marker)
				if readErr != nil || decodeErr != nil || encodeErr != nil || markerID != canonicalID || !bytes.Equal(raw, canonical) {
					t.Fatalf("forget marker round trip id=%s/%s read=%v decode=%v encode=%v", markerID, canonicalID, readErr, decodeErr, encodeErr)
				}
				mutations := [][]byte{
					bytes.Replace(raw, []byte(`{"schema":`), []byte(`{"schema":"duplicate","schema":`), 1),
					bytes.Replace(raw, []byte("}\n"), []byte(",\"unknown\":true}\n"), 1),
					append(append([]byte(nil), raw...), []byte("{}\n")...),
					bytes.Repeat([]byte{'x'}, int(maximumExecutionForgetBytes)+1),
				}
				for index, mutation := range mutations {
					if _, _, err := DecodeExecutionForgetIntent(bytes.NewReader(mutation)); err == nil || !errors.Is(err, ErrExecutionIntegrity) {
						t.Fatalf("unsafe forget marker mutation %d accepted: %v", index, err)
					}
				}
			}
			listed, listErr := ListExecutionOperations(context.Background(), fixture.targetRoot, DefaultExecutionOperationListLimits())
			if listErr != nil || !listed.Complete || len(listed.Operations) != 1 || listed.Operations[0].ID != operation ||
				listed.Operations[0].Status != "forget_in_progress_not_inspected" {
				t.Fatalf("forget listing=%#v err=%v", listed, listErr)
			}
			status, statusErr := Status(context.Background(), StatusOptions{TargetRoot: fixture.targetRoot,
				OperationID: operation, Limits: DefaultExecutionLimits()})
			if statusErr != nil || status.Outcome != ExecutionOutcomeIncomplete || status.Operation.Status != "forgetting" ||
				status.Operation.Resumable || status.WritesPerformed != 0 {
				t.Fatalf("forget status=%#v err=%v", status, statusErr)
			}
			prune, pruneErr := PruneExecution(context.Background(), ExecutionPruneOptions{TargetRoot: fixture.targetRoot,
				OperationID: operation, ExpectedPlanID: plan.ID, Acknowledge: true,
				JournalLimits: DefaultExecutionLimits(), RetentionLimits: DefaultExecutionRetentionLimits()})
			if pruneErr != nil || prune.Outcome != ExecutionRetentionOutcomeBlocked || prune.WritesPerformed != 0 || prune.Operation.Status != "forgetting" {
				t.Fatalf("forget-boundary prune=%#v err=%v", prune, pruneErr)
			}
			executionForgetTransitionHook = nil
			recovered, recoverErr := ForgetExecutionTombstone(context.Background(), forgetOptions(fixture, plan, operation))
			if recoverErr != nil || recovered.Outcome != ExecutionForgetOutcomeForgotten || !recovered.Authority.TargetHistoricalEvidenceErased {
				t.Fatalf("recovered forget=%#v err=%v", recovered, recoverErr)
			}
		})
	}
}

func TestForgetExecutionTombstoneRecoversPartiallyRemovedMarkers(t *testing.T) {
	fixture, plan, operation := retainedRetirementFixture(t)
	stop := errors.New("stop after root intent")
	executionForgetTransitionHook = func(phase string) error {
		if phase == "root_intent_published" {
			return stop
		}
		return nil
	}
	t.Cleanup(func() { executionForgetTransitionHook = nil })
	if _, err := ForgetExecutionTombstone(context.Background(), forgetOptions(fixture, plan, operation)); !errors.Is(err, stop) {
		t.Fatalf("initial forget error=%v", err)
	}
	operationName, _ := OperationDirectoryName(operation)
	completePath := filepath.Join(fixture.targetRoot, operationName, executionRetentionDirectory, executionRetentionCompleteFile)
	if err := os.Remove(completePath); err != nil {
		t.Fatal(err)
	}
	executionForgetTransitionHook = nil
	recovered, err := ForgetExecutionTombstone(context.Background(), forgetOptions(fixture, plan, operation))
	if err != nil || recovered.Outcome != ExecutionForgetOutcomeForgotten {
		t.Fatalf("partial recovery=%#v err=%v", recovered, err)
	}
}

func TestForgetExecutionTombstonePolicyAndIntegrityAreFailClosed(t *testing.T) {
	t.Run("acknowledgement", func(t *testing.T) {
		fixture, plan, operation := retainedRetirementFixture(t)
		options := forgetOptions(fixture, plan, operation)
		options.Acknowledge = false
		before, err := os.ReadDir(fixture.targetRoot)
		if err != nil {
			t.Fatal(err)
		}
		report, forgetErr := ForgetExecutionTombstone(context.Background(), options)
		after, readErr := os.ReadDir(fixture.targetRoot)
		if forgetErr != nil || readErr != nil || report.Outcome != ExecutionForgetOutcomeBlocked || report.WritesPerformed != 0 || len(after) != len(before) {
			t.Fatalf("blocked forget=%#v err=%v before=%d after=%d read=%v", report, forgetErr, len(before), len(after), readErr)
		}
	})

	t.Run("marker budget", func(t *testing.T) {
		fixture, plan, operation := retainedRetirementFixture(t)
		options := forgetOptions(fixture, plan, operation)
		options.Limits.MaxMarkerBytes = 1
		before, err := os.ReadDir(fixture.targetRoot)
		if err != nil {
			t.Fatal(err)
		}
		report, forgetErr := ForgetExecutionTombstone(context.Background(), options)
		after, readErr := os.ReadDir(fixture.targetRoot)
		if forgetErr == nil || report.Outcome != ExecutionForgetOutcomeBlocked || report.WritesPerformed != 0 ||
			readErr != nil || len(after) != len(before) {
			t.Fatalf("budget forget=%#v err=%v before=%d after=%d read=%v", report, forgetErr, len(before), len(after), readErr)
		}
	})

	t.Run("not pruned", func(t *testing.T) {
		fixture, plan, operation := completedRetirementFixture(t)
		report, err := ForgetExecutionTombstone(context.Background(), forgetOptions(fixture, plan, operation))
		if err != nil || report.Outcome != ExecutionForgetOutcomeBlocked || report.WritesPerformed != 0 {
			t.Fatalf("unpruned forget=%#v err=%v", report, err)
		}
	})

	t.Run("changed root intent", func(t *testing.T) {
		fixture, plan, operation := retainedRetirementFixture(t)
		stop := errors.New("stop after root intent")
		executionForgetTransitionHook = func(phase string) error {
			if phase == "root_intent_published" {
				return stop
			}
			return nil
		}
		t.Cleanup(func() { executionForgetTransitionHook = nil })
		if _, err := ForgetExecutionTombstone(context.Background(), forgetOptions(fixture, plan, operation)); !errors.Is(err, stop) {
			t.Fatalf("initial forget error=%v", err)
		}
		executionForgetTransitionHook = nil
		rootName, _ := ExecutionForgetRootName(operation)
		if err := os.WriteFile(filepath.Join(fixture.targetRoot, rootName), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		report, err := ForgetExecutionTombstone(context.Background(), forgetOptions(fixture, plan, operation))
		if err == nil || !errors.Is(err, ErrExecutionIntegrity) || report.Outcome != ExecutionForgetOutcomeIntegrityFailed || report.Operation.Resumable {
			t.Fatalf("changed intent forget=%#v err=%v", report, err)
		}
	})

	t.Run("operation name reappears", func(t *testing.T) {
		fixture, plan, operation := retainedRetirementFixture(t)
		operationName, _ := OperationDirectoryName(operation)
		executionForgetTransitionHook = func(phase string) error {
			if phase == "before_root_intent_remove" {
				return os.Mkdir(filepath.Join(fixture.targetRoot, operationName), 0o700)
			}
			return nil
		}
		t.Cleanup(func() { executionForgetTransitionHook = nil })
		report, err := ForgetExecutionTombstone(context.Background(), forgetOptions(fixture, plan, operation))
		if err == nil || !errors.Is(err, ErrExecutionIntegrity) || report.Outcome != ExecutionForgetOutcomeIntegrityFailed ||
			report.Authority.TargetHistoricalEvidenceErased || report.Operation.Resumable {
			t.Fatalf("reappeared operation forget=%#v err=%v", report, err)
		}
		rootName, _ := ExecutionForgetRootName(operation)
		if _, statErr := os.Lstat(filepath.Join(fixture.targetRoot, rootName)); statErr != nil {
			t.Fatalf("root intent was erased despite operation replacement: %v", statErr)
		}
	})

	t.Run("last marker disappears before removal", func(t *testing.T) {
		fixture, plan, operation := retainedRetirementFixture(t)
		rootName, _ := ExecutionForgetRootName(operation)
		executionForgetTransitionHook = func(phase string) error {
			if phase == "before_root_intent_remove" {
				return os.Remove(filepath.Join(fixture.targetRoot, rootName))
			}
			return nil
		}
		t.Cleanup(func() { executionForgetTransitionHook = nil })
		report, err := ForgetExecutionTombstone(context.Background(), forgetOptions(fixture, plan, operation))
		if !errors.Is(err, ErrOperationNotFound) || report.Outcome != ExecutionForgetOutcomeAbsentUnattributed ||
			report.Operation.Resumable || report.Authority.TargetHistoricalEvidenceErased ||
			report.Authority.MarkerDurable || report.Removals.RootIntent.Attempted {
			t.Fatalf("disappeared marker forget=%#v err=%v", report, err)
		}
	})
}
