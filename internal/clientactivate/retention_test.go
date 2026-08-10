package clientactivate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type terminalActivationFixture struct {
	activation activationFixture
	operation  OperationID
	planID     string
}

func makeTerminalActivation(t *testing.T, start bool) terminalActivationFixture {
	t.Helper()
	fixture := makeActivationFixtureWithRetainedAdoption(t)
	now := time.Now().UTC()
	stopped := fixture.job("stoppedDL", 0.25)
	complete := fixture.job("stoppedUP", 1)
	preview, err := Preview(context.Background(), fixture.authority,
		newActivationSession(fixture, activationLedger(fixture.meta, &stopped, now)), start)
	if err != nil {
		t.Fatal(err)
	}
	if !start {
		run, runErr := Run(context.Background(), RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
			Session: newActivationSession(fixture, activationLedger(fixture.meta, &stopped, now.Add(time.Second)),
				activationLedger(fixture.meta, &complete, now.Add(2*time.Second))), AcknowledgeRecheck: true})
		if runErr != nil || run.Outcome != OutcomeCheckedStopped {
			t.Fatalf("terminal recheck=%#v err=%v", run, runErr)
		}
		operation, _ := ParseOperationID(run.Operation.ID)
		return terminalActivationFixture{activation: fixture, operation: operation, planID: preview.Plan.ID}
	}
	checking := fixture.job("checkingDL", 0.25)
	started := fixture.job("uploading", 1)
	run, runErr := Run(context.Background(), RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
		Session: newActivationSession(fixture, activationLedger(fixture.meta, &stopped, now.Add(time.Second)),
			activationLedger(fixture.meta, &checking, now.Add(2*time.Second))), StartAfterRecheck: true, AcknowledgeRecheck: true})
	if runErr != nil {
		t.Fatal(runErr)
	}
	operation, _ := ParseOperationID(run.Operation.ID)
	checked, checkErr := Resume(context.Background(), operation, RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
		Session: newActivationSession(fixture, activationLedger(fixture.meta, &complete, now.Add(3*time.Second))), StartAfterRecheck: true})
	if checkErr != nil || checked.Outcome != OutcomeCheckedStopped {
		t.Fatalf("checked=%#v err=%v", checked, checkErr)
	}
	terminal, terminalErr := Resume(context.Background(), operation, RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
		Session: newActivationSession(fixture, activationLedger(fixture.meta, &complete, now.Add(4*time.Second)),
			activationLedger(fixture.meta, &started, now.Add(5*time.Second))), StartAfterRecheck: true, AcknowledgeStart: true})
	if terminalErr != nil || terminal.Outcome != OutcomeStartedClientClaim {
		t.Fatalf("terminal start=%#v err=%v", terminal, terminalErr)
	}
	return terminalActivationFixture{activation: fixture, operation: operation, planID: preview.Plan.ID}
}

func (fixture terminalActivationFixture) options() PruneOptions {
	return PruneOptions{TargetRoot: fixture.activation.targetRoot, OperationID: fixture.operation, ExpectedPlanID: fixture.planID,
		Acknowledge: true, Limits: DefaultRetentionLimits()}
}

func TestPruneRetainsExactTerminalActivationAuthority(t *testing.T) {
	for _, start := range []bool{false, true} {
		t.Run(map[bool]string{false: "recheck_only", true: "recheck_then_start"}[start], func(t *testing.T) {
			fixture := makeTerminalActivation(t, start)
			before, beforeObservation, err := VerifyCompletion(context.Background(), CompletionProofOptions{TargetRoot: fixture.activation.targetRoot,
				OperationID: fixture.operation, ExpectedPlanID: fixture.planID})
			if err != nil || before == nil || !before.Verified() {
				t.Fatalf("before=%#v observation=%#v err=%v", before, beforeObservation, err)
			}
			report, err := Prune(context.Background(), fixture.options())
			if err != nil || report.Outcome != RetentionOutcomePruned || !report.Markers.ExactTombstone || report.WritesPerformed == 0 {
				t.Fatalf("prune=%#v err=%v", report, err)
			}
			encoded, marshalErr := json.Marshal(report)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			for _, secret := range []string{fixture.activation.targetRoot, fixture.activation.savePath, fixture.activation.contentPath, fixture.activation.opaqueKey} {
				escaped, _ := json.Marshal(secret)
				if strings.Contains(string(encoded), strings.Trim(string(escaped), `"`)) {
					t.Fatalf("retention report leaked %q: %s", secret, encoded)
				}
			}
			status, statusErr := Status(context.Background(), StatusOptions{TargetRoot: fixture.activation.targetRoot, OperationID: fixture.operation})
			if statusErr != nil || status.Operation.Status != "retained" || status.Operation.Resumable || status.Journal.RetentionState != "complete" ||
				status.Journal.IntentDurable || status.Journal.RecheckCompletionDurable || status.Journal.ActivationCompletionDurable {
				t.Fatalf("retained status=%#v err=%v", status, statusErr)
			}
			after, observation, verifyErr := VerifyCompletion(context.Background(), CompletionProofOptions{TargetRoot: fixture.activation.targetRoot,
				OperationID: fixture.operation, ExpectedPlanID: fixture.planID})
			if verifyErr != nil || after == nil || !after.Verified() || observation.TerminalMarkerID != beforeObservation.TerminalMarkerID ||
				!strings.Contains(observation.Assurance, "retention_tombstone") {
				t.Fatalf("after=%#v observation=%#v err=%v", after, observation, verifyErr)
			}
			repeated, repeatErr := Prune(context.Background(), fixture.options())
			if repeatErr != nil || repeated.Outcome != RetentionOutcomeAlreadyPruned || repeated.WritesPerformed != 0 {
				t.Fatalf("repeated=%#v err=%v", repeated, repeatErr)
			}
		})
	}
}

func TestPruneActivationRebindsIntentBeforeDeletingLegacyJournal(t *testing.T) {
	fixture := makeTerminalActivation(t, false)
	directory, _ := operationDirectoryName(fixture.operation)
	operationRoot := filepath.Join(fixture.activation.targetRoot, directory)
	entries, err := os.ReadDir(operationRoot)
	if err != nil {
		t.Fatal(err)
	}
	legacy := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && entry.Name() != operationLockEntryName {
			legacy = append(legacy, entry.Name())
		}
	}
	intentPath := filepath.Join(operationRoot, retentionDirectoryName, retentionIntentName)
	retentionTransitionHook = func(phase string) error {
		if phase == "intent_published" {
			return os.WriteFile(intentPath, []byte("{}\n"), 0o600)
		}
		return nil
	}
	t.Cleanup(func() { retentionTransitionHook = nil })
	report, pruneErr := Prune(context.Background(), fixture.options())
	if !errors.Is(pruneErr, ErrIntegrity) || report.Outcome != RetentionOutcomeIntegrityFailed || report.Operation.Resumable ||
		report.Markers.IntentDurable || report.Markers.PruneResumable || report.Markers.State != "intent_revalidation_failed" {
		t.Fatalf("changed retention intent report=%#v err=%v", report, pruneErr)
	}
	for _, name := range legacy {
		if _, statErr := os.Stat(filepath.Join(operationRoot, name)); statErr != nil {
			t.Fatalf("legacy marker %q was deleted before intent rebind: %v", name, statErr)
		}
	}
}

func TestPruneActivationRejectsInputBeforeRetentionWrites(t *testing.T) {
	fixture := makeTerminalActivation(t, false)
	directory, _ := operationDirectoryName(fixture.operation)
	retentionPath := filepath.Join(fixture.activation.targetRoot, directory, retentionDirectoryName)
	wrongPlan := fixture.planID[:len(fixture.planID)-1] + map[bool]string{true: "1", false: "0"}[fixture.planID[len(fixture.planID)-1] == '0']
	for _, test := range []struct {
		name    string
		ctx     context.Context
		options PruneOptions
	}{
		{name: "acknowledgement", ctx: context.Background(), options: func() PruneOptions { value := fixture.options(); value.Acknowledge = false; return value }()},
		{name: "limits", ctx: context.Background(), options: func() PruneOptions { value := fixture.options(); value.Limits.MaxObjects = 0; return value }()},
		{name: "marker_budget", ctx: context.Background(), options: func() PruneOptions {
			value := fixture.options()
			value.Limits.MaxMarkerBytes = 1
			return value
		}()},
		{name: "plan_operation_pair", ctx: context.Background(), options: func() PruneOptions {
			value := fixture.options()
			value.ExpectedPlanID = wrongPlan
			return value
		}()},
		{name: "cancelled", ctx: func() context.Context {
			value, cancel := context.WithCancel(context.Background())
			cancel()
			return value
		}(), options: fixture.options()},
	} {
		t.Run(test.name, func(t *testing.T) {
			report, err := Prune(test.ctx, test.options)
			if err == nil || report.WritesPerformed != 0 || report.WritesUncertain {
				t.Fatalf("report=%#v err=%v", report, err)
			}
			if _, statErr := os.Stat(retentionPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid request created retention state: %v", statErr)
			}
		})
	}
}

func TestRetainedActivationResumePreflightStillValidatesLiveSelectors(t *testing.T) {
	fixture := makeTerminalActivation(t, false)
	if _, err := Prune(context.Background(), fixture.options()); err != nil {
		t.Fatal(err)
	}
	changed := *fixture.activation.authority
	changed.clientConfigID = "sha256:" + strings.Repeat("f", 64)
	report, err := PreflightResume(context.Background(), &changed, fixture.activation.targetRoot, fixture.operation, fixture.planID)
	if !errors.Is(err, ErrPolicy) || report.Outcome != OutcomeBlocked || report.Operation.Status == "retained" {
		t.Fatalf("mismatched retained preflight=%#v err=%v", report, err)
	}
}

func TestActivationRetentionDecoderRejectsNonCanonicalInput(t *testing.T) {
	fixture := makeTerminalActivation(t, false)
	if _, err := Prune(context.Background(), fixture.options()); err != nil {
		t.Fatal(err)
	}
	directory, _ := operationDirectoryName(fixture.operation)
	raw, err := os.ReadFile(filepath.Join(fixture.activation.targetRoot, directory, retentionDirectoryName, retentionIntentName))
	if err != nil {
		t.Fatal(err)
	}
	unknown := append([]byte(nil), raw[:len(raw)-2]...)
	unknown = append(unknown, []byte(",\"unknown\":true}\n")...)
	if _, _, decodeErr := DecodeRetentionIntent(strings.NewReader(string(unknown))); decodeErr == nil {
		t.Fatal("unknown retention field was accepted")
	}
	duplicate := strings.Replace(string(raw), `"schema":`, `"schema":"`+RetentionIntentSchemaV1+`","schema":`, 1)
	if _, _, decodeErr := DecodeRetentionIntent(strings.NewReader(duplicate)); decodeErr == nil {
		t.Fatal("duplicate retention field was accepted")
	}
}

func TestPruneActivationCrashBoundaryRequiresExplicitRecovery(t *testing.T) {
	for _, phase := range []string{"intent_published", "legacy_state_removed"} {
		t.Run(phase, func(t *testing.T) {
			fixture := makeTerminalActivation(t, false)
			stop := errors.New("stop during activation retention")
			retentionTransitionHook = func(observed string) error {
				if observed == phase {
					return stop
				}
				return nil
			}
			t.Cleanup(func() { retentionTransitionHook = nil })
			report, err := Prune(context.Background(), fixture.options())
			if !errors.Is(err, stop) || report.Outcome != RetentionOutcomeInterrupted || !report.Markers.IntentDurable || !report.Markers.PruneResumable {
				t.Fatalf("interrupted=%#v err=%v", report, err)
			}
			if phase == "legacy_state_removed" && report.Operation.PhaseAfter != "legacy_activation_state_removed" {
				t.Fatalf("removed legacy state was not reported: %#v", report.Operation)
			}
			verified, _, verifyErr := VerifyCompletion(context.Background(), CompletionProofOptions{TargetRoot: fixture.activation.targetRoot,
				OperationID: fixture.operation, ExpectedPlanID: fixture.planID})
			if !errors.Is(verifyErr, ErrPolicy) || verified != nil {
				t.Fatalf("incomplete tombstone granted authority: %#v %v", verified, verifyErr)
			}
			retentionTransitionHook = nil
			recovered, recoverErr := Prune(context.Background(), fixture.options())
			if recoverErr != nil || recovered.Outcome != RetentionOutcomePruned || !recovered.Markers.ExactTombstone {
				t.Fatalf("recovered=%#v err=%v", recovered, recoverErr)
			}
		})
	}
}

func TestRetainedActivationNamespaceMustRemainExact(t *testing.T) {
	fixture := makeTerminalActivation(t, false)
	if _, err := Prune(context.Background(), fixture.options()); err != nil {
		t.Fatal(err)
	}
	directory, _ := operationDirectoryName(fixture.operation)
	if err := os.WriteFile(filepath.Join(fixture.activation.targetRoot, directory, "unexpected.bin"), []byte("unexpected"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := Status(context.Background(), StatusOptions{TargetRoot: fixture.activation.targetRoot, OperationID: fixture.operation})
	if !errors.Is(err, ErrIntegrity) || status.Outcome != OutcomeIntegrityFailed || status.Operation.Resumable {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}
