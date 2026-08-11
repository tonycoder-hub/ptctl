package clientremove

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type terminalRemovalFixture struct {
	root         string
	plan         Plan
	planID       string
	operation    OperationID
	intentID     MarkerID
	attemptID    MarkerID
	responseID   MarkerID
	completionID MarkerID
}

func makeTerminalRemovalFixture(t *testing.T, retainResponse bool) terminalRemovalFixture {
	t.Helper()
	root, plan := testRemovalPlan(t)
	planID, err := PlanID(plan)
	if err != nil {
		t.Fatal(err)
	}
	operation := OperationIDForPlan(planID)
	handle, _, err := createJournal(context.Background(), root, plan, planID)
	if err != nil {
		t.Fatal(err)
	}
	intentID := handle.state.IntentID
	now := time.Now().UTC()
	attempt := testAttempt(operation, planID, plan, now, 1, "")
	_, attemptID, err := handle.appendAttempt(context.Background(), attempt)
	if err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	responseID := MarkerID("")
	if retainResponse {
		response := testResponse(operation, planID, attemptID, now.Add(time.Second), true)
		_, responseID, err = handle.appendResponse(context.Background(), 1, response)
		if err != nil {
			_ = handle.Close()
			t.Fatal(err)
		}
	}
	completion := testCompletion(operation, planID, plan, attemptID, responseID, now.Add(2*time.Second), retainResponse)
	_, completionID, err := handle.appendCompletion(context.Background(), completion)
	if err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	return terminalRemovalFixture{root: root, plan: plan, planID: planID, operation: operation,
		intentID: intentID, attemptID: attemptID, responseID: responseID, completionID: completionID}
}

func (fixture terminalRemovalFixture) pruneOptions() PruneOptions {
	return PruneOptions{TargetRoot: fixture.root, OperationID: fixture.operation, ExpectedPlanID: fixture.planID,
		Acknowledge: true, Limits: DefaultRetentionLimits()}
}

func (fixture terminalRemovalFixture) forgetOptions() ForgetOptions {
	return ForgetOptions{TargetRoot: fixture.root, OperationID: fixture.operation, ExpectedPlanID: fixture.planID,
		Acknowledge: true, Limits: DefaultForgetLimits()}
}

func TestRemovalPruneRetainsSparseRequestEvidenceAndIsIdempotent(t *testing.T) {
	for _, retainResponse := range []bool{true, false} {
		t.Run(map[bool]string{true: "accepted_response", false: "response_missing_after_attempt"}[retainResponse], func(t *testing.T) {
			fixture := makeTerminalRemovalFixture(t, retainResponse)
			report, err := Prune(context.Background(), fixture.pruneOptions())
			if err != nil || report.Outcome != RetentionOutcomePruned || !report.Markers.ExactTombstone ||
				!report.Markers.IntentDurable || !report.Markers.CompletionDurable || report.Operation.Resumable {
				t.Fatalf("prune report=%#v err=%v", report, err)
			}
			if report.Proof.AttemptsRecorded != 1 || report.Proof.ResponsesRecorded != boolInt(retainResponse) {
				t.Fatalf("proof=%#v", report.Proof)
			}
			status, statusErr := Status(context.Background(), StatusOptions{TargetRoot: fixture.root,
				OperationID: fixture.operation, ExpectedPlanID: fixture.planID})
			if statusErr != nil || status.Outcome != OutcomeHistoricalRetained || status.Operation.Status != "retained" ||
				status.Operation.Resumable || !status.Retention.IntentDurable || !status.Retention.CompletionDurable {
				t.Fatalf("status=%#v err=%v", status, statusErr)
			}
			repeated, repeatErr := Prune(context.Background(), fixture.pruneOptions())
			if repeatErr != nil || repeated.Outcome != RetentionOutcomeAlreadyPruned || repeated.WritesPerformed != 0 {
				t.Fatalf("repeated=%#v err=%v", repeated, repeatErr)
			}
			directory, _ := operationDirectoryName(fixture.operation)
			entries, readErr := os.ReadDir(filepath.Join(fixture.root, directory))
			if readErr != nil || len(entries) != 2 || entries[0].Name() != operationLockEntryName || entries[1].Name() != retentionDirectoryName {
				t.Fatalf("entries=%v err=%v", entries, readErr)
			}
			raw, marshalErr := json.Marshal(report)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if strings.Contains(string(raw), fixture.root) {
				t.Fatalf("report leaked root: %s", raw)
			}
		})
	}
}

func TestRemovalRetentionIntentRejectsChangedResponseEvidence(t *testing.T) {
	fixture := makeTerminalRemovalFixture(t, true)
	report, err := Prune(context.Background(), fixture.pruneOptions())
	if err != nil {
		t.Fatal(err)
	}
	directory, _ := operationDirectoryName(fixture.operation)
	path := filepath.Join(fixture.root, directory, retentionDirectoryName, retentionIntentName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	marker, _, err := DecodeRetentionIntent(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	marker.Responses[0].Response.RequestBytes++
	if _, _, err := EncodeRetentionIntent(marker); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("changed response accepted: %v", err)
	}
	if report.Proof.ResponsesRecorded != 1 {
		t.Fatalf("proof=%#v", report.Proof)
	}
}

func TestRemovalPruneCrashBoundaryResumesWithoutDownloader(t *testing.T) {
	fixture := makeTerminalRemovalFixture(t, true)
	stop := errors.New("stop after retention intent")
	retentionTransitionHook = func(phase string) error {
		if phase == "intent_published" {
			return stop
		}
		return nil
	}
	t.Cleanup(func() { retentionTransitionHook = nil })
	report, err := Prune(context.Background(), fixture.pruneOptions())
	if !errors.Is(err, stop) || !report.Markers.IntentDurable || !report.Markers.PruneResumable {
		t.Fatalf("stopped=%#v err=%v", report, err)
	}
	status, statusErr := Status(context.Background(), StatusOptions{TargetRoot: fixture.root,
		OperationID: fixture.operation, ExpectedPlanID: fixture.planID})
	if statusErr != nil || status.Operation.Status != "pruning" || status.Operation.Resumable {
		t.Fatalf("status=%#v err=%v", status, statusErr)
	}
	retentionTransitionHook = nil
	recovered, recoverErr := Prune(context.Background(), fixture.pruneOptions())
	if recoverErr != nil || recovered.Outcome != RetentionOutcomePruned || !recovered.Markers.ExactTombstone {
		t.Fatalf("recovered=%#v err=%v", recovered, recoverErr)
	}
}

func TestRemovalForgetErasesOnlyExplicitRetainedHistory(t *testing.T) {
	fixture := makeTerminalRemovalFixture(t, true)
	if _, err := Prune(context.Background(), fixture.pruneOptions()); err != nil {
		t.Fatal(err)
	}
	report, err := Forget(context.Background(), fixture.forgetOptions())
	if err != nil || report.Outcome != ForgetOutcomeForgotten || !report.Authority.TargetHistoricalEvidenceErased ||
		report.Authority.ExactTombstoneEvidenceAvailable || report.WritesPerformed == 0 {
		t.Fatalf("forget=%#v err=%v", report, err)
	}
	directory, _ := operationDirectoryName(fixture.operation)
	rootMarker, _ := ForgetRootName(fixture.operation)
	for _, path := range []string{filepath.Join(fixture.root, directory), filepath.Join(fixture.root, rootMarker)} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("historical object remains at %q: %v", path, statErr)
		}
	}
	repeated, repeatErr := Forget(context.Background(), fixture.forgetOptions())
	if !errors.Is(repeatErr, ErrOperationNotFound) || repeated.Outcome != ForgetOutcomeAbsentUnattributed || repeated.WritesPerformed != 0 {
		t.Fatalf("repeated=%#v err=%v", repeated, repeatErr)
	}
}

func TestRemovalForgetCrashBoundariesRecoverFromExactRootAuthority(t *testing.T) {
	for _, phase := range []string{"root_intent_published", "operation_removed"} {
		t.Run(phase, func(t *testing.T) {
			fixture := makeTerminalRemovalFixture(t, true)
			if _, err := Prune(context.Background(), fixture.pruneOptions()); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(fixture.root, "retained-content.bin")
			if err := os.WriteFile(sentinel, []byte("retained"), 0o600); err != nil {
				t.Fatal(err)
			}
			stop := errors.New("forget transition stopped")
			forgetTransitionHook = func(observed string) error {
				if observed == phase {
					return stop
				}
				return nil
			}
			t.Cleanup(func() { forgetTransitionHook = nil })
			report, err := Forget(context.Background(), fixture.forgetOptions())
			if !errors.Is(err, stop) || report.Outcome != ForgetOutcomeInterrupted || !report.Authority.MarkerDurable ||
				!report.Operation.Resumable {
				t.Fatalf("stopped=%#v err=%v", report, err)
			}
			status, statusErr := Status(context.Background(), StatusOptions{TargetRoot: fixture.root,
				OperationID: fixture.operation, ExpectedPlanID: fixture.planID})
			if statusErr != nil || status.Operation.Status != "forgetting" || status.Operation.Resumable ||
				status.Outcome != OutcomeBlocked {
				t.Fatalf("status=%#v err=%v", status, statusErr)
			}
			forgetTransitionHook = nil
			recovered, recoverErr := Forget(context.Background(), fixture.forgetOptions())
			if recoverErr != nil || recovered.Outcome != ForgetOutcomeForgotten || !recovered.Authority.TargetHistoricalEvidenceErased {
				t.Fatalf("recovered=%#v err=%v", recovered, recoverErr)
			}
			if raw, readErr := os.ReadFile(sentinel); readErr != nil || string(raw) != "retained" {
				t.Fatalf("retained content changed: %q err=%v", raw, readErr)
			}
		})
	}
}

func TestRemovalForgetRejectsUnexpectedTombstoneObjectsBeforeWrite(t *testing.T) {
	fixture := makeTerminalRemovalFixture(t, true)
	if _, err := Prune(context.Background(), fixture.pruneOptions()); err != nil {
		t.Fatal(err)
	}
	directory, _ := operationDirectoryName(fixture.operation)
	unexpected := filepath.Join(fixture.root, directory, retentionDirectoryName, "unexpected.bin")
	if err := os.WriteFile(unexpected, []byte("unexpected"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Forget(context.Background(), fixture.forgetOptions())
	if !errors.Is(err, ErrIntegrity) || report.Outcome != ForgetOutcomeIntegrityFailed || report.WritesPerformed != 0 || report.Operation.Resumable {
		t.Fatalf("report=%#v err=%v", report, err)
	}
}

func TestRemovalRetentionPolicyFailsBeforeWrite(t *testing.T) {
	fixture := makeTerminalRemovalFixture(t, true)
	for _, mutate := range []func(*PruneOptions){
		func(options *PruneOptions) { options.Acknowledge = false },
		func(options *PruneOptions) { options.ExpectedPlanID = strings.Repeat("f", 24) },
		func(options *PruneOptions) { options.Limits.MaxMarkerBytes = 1 },
	} {
		options := fixture.pruneOptions()
		mutate(&options)
		report, err := Prune(context.Background(), options)
		if !errors.Is(err, ErrPolicy) || report.Outcome != RetentionOutcomeBlocked || report.WritesPerformed != 0 {
			t.Fatalf("report=%#v err=%v", report, err)
		}
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
