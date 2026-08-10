package clientactivate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (fixture terminalActivationFixture) forgetOptions() ForgetOptions {
	return ForgetOptions{TargetRoot: fixture.activation.targetRoot, OperationID: fixture.operation,
		ExpectedPlanID: fixture.planID, Acknowledge: true, Limits: DefaultForgetLimits()}
}

func TestForgetActivationTombstoneRemovesOnlyExactHistoricalAuthority(t *testing.T) {
	for _, start := range []bool{false, true} {
		t.Run(map[bool]string{false: "recheck_only", true: "recheck_then_start"}[start], func(t *testing.T) {
			fixture := makeTerminalActivation(t, start)
			if _, err := Prune(context.Background(), fixture.options()); err != nil {
				t.Fatal(err)
			}
			verified, before, err := VerifyCompletion(context.Background(), CompletionProofOptions{TargetRoot: fixture.activation.targetRoot,
				OperationID: fixture.operation, ExpectedPlanID: fixture.planID})
			if err != nil || verified == nil || !verified.Verified() {
				t.Fatalf("verified=%#v observation=%#v err=%v", verified, before, err)
			}
			report, forgetErr := Forget(context.Background(), fixture.forgetOptions())
			if forgetErr != nil || report.Outcome != ForgetOutcomeForgotten || report.WritesPerformed == 0 || report.WritesUncertain ||
				!report.Authority.TargetHistoricalEvidenceErased || report.Authority.ExactTombstoneEvidenceAvailable || report.Operation.Resumable {
				t.Fatalf("forget=%#v err=%v", report, forgetErr)
			}
			operationName, _ := operationDirectoryName(fixture.operation)
			rootName, _ := ForgetRootName(fixture.operation)
			for _, name := range []string{operationName, rootName} {
				if _, statErr := os.Stat(filepath.Join(fixture.activation.targetRoot, name)); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("forgotten control object %q remains: %v", name, statErr)
				}
			}
			if _, _, verifyErr := VerifyCompletion(context.Background(), CompletionProofOptions{TargetRoot: fixture.activation.targetRoot,
				OperationID: fixture.operation, ExpectedPlanID: fixture.planID}); !errors.Is(verifyErr, ErrOperationNotFound) {
				t.Fatalf("forgotten activation still granted historical authority: %v", verifyErr)
			}
			repeated, repeatErr := Forget(context.Background(), fixture.forgetOptions())
			if !errors.Is(repeatErr, ErrOperationNotFound) || repeated.Outcome != ForgetOutcomeAbsentUnattributed || repeated.WritesPerformed != 0 {
				t.Fatalf("repeated=%#v err=%v", repeated, repeatErr)
			}
			encoded, marshalErr := json.Marshal(report)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			for _, secret := range []string{fixture.activation.targetRoot, fixture.activation.savePath, fixture.activation.contentPath, fixture.activation.opaqueKey} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("forget report leaked %q: %s", secret, encoded)
				}
			}
		})
	}
}

func TestForgetActivationCrashBoundariesRecoverOnlyThroughExplicitForget(t *testing.T) {
	for _, phase := range []string{"pending_staged", "root_intent_published", "operation_removed"} {
		t.Run(phase, func(t *testing.T) {
			fixture := makeTerminalActivation(t, false)
			if _, err := Prune(context.Background(), fixture.options()); err != nil {
				t.Fatal(err)
			}
			stop := errors.New("stop activation forget")
			forgetTransitionHook = func(observed string) error {
				if observed == phase {
					return stop
				}
				return nil
			}
			t.Cleanup(func() { forgetTransitionHook = nil })
			report, err := Forget(context.Background(), fixture.forgetOptions())
			if !errors.Is(err, stop) || report.Outcome != ForgetOutcomeInterrupted || report.WritesPerformed == 0 {
				t.Fatalf("interrupted=%#v err=%v", report, err)
			}
			status, statusErr := Status(context.Background(), StatusOptions{TargetRoot: fixture.activation.targetRoot, OperationID: fixture.operation})
			if statusErr != nil || status.Outcome != OutcomeForgetting || status.Operation.Status != "forgetting" || status.Operation.Resumable ||
				status.Journal.RetentionState != "forgetting" || !status.Journal.RetentionIntentPresent || !status.Journal.RetentionCompletionPresent ||
				(status.Operation.PhaseAfter != "forget_intent_staging" && status.Operation.PhaseAfter != "forget_intent_recorded") {
				t.Fatalf("status=%#v err=%v", status, statusErr)
			}
			if verified, _, verifyErr := VerifyCompletion(context.Background(), CompletionProofOptions{TargetRoot: fixture.activation.targetRoot,
				OperationID: fixture.operation, ExpectedPlanID: fixture.planID}); verified != nil || !errors.Is(verifyErr, ErrPolicy) {
				t.Fatalf("forget boundary granted authority: %#v %v", verified, verifyErr)
			}
			if pruned, pruneErr := Prune(context.Background(), fixture.options()); !errors.Is(pruneErr, ErrPolicy) || pruned.Outcome != RetentionOutcomeBlocked {
				t.Fatalf("prune crossed forget boundary: %#v %v", pruned, pruneErr)
			}
			forgetTransitionHook = nil
			recovered, recoverErr := Forget(context.Background(), fixture.forgetOptions())
			if recoverErr != nil || recovered.Outcome != ForgetOutcomeForgotten {
				t.Fatalf("recovered=%#v err=%v", recovered, recoverErr)
			}
		})
	}
}

func TestForgetActivationRejectsPolicyBeforeWrites(t *testing.T) {
	fixture := makeTerminalActivation(t, false)
	if _, err := Prune(context.Background(), fixture.options()); err != nil {
		t.Fatal(err)
	}
	operationName, _ := operationDirectoryName(fixture.operation)
	rootName, _ := ForgetRootName(fixture.operation)
	wrongPlan := fixture.planID[:len(fixture.planID)-1] + map[bool]string{true: "1", false: "0"}[fixture.planID[len(fixture.planID)-1] == '0']
	for _, test := range []struct {
		name    string
		ctx     context.Context
		options ForgetOptions
	}{
		{name: "acknowledgement", ctx: context.Background(), options: func() ForgetOptions { value := fixture.forgetOptions(); value.Acknowledge = false; return value }()},
		{name: "limits", ctx: context.Background(), options: func() ForgetOptions { value := fixture.forgetOptions(); value.Limits.MaxMarkerBytes = 0; return value }()},
		{name: "plan", ctx: context.Background(), options: func() ForgetOptions { value := fixture.forgetOptions(); value.ExpectedPlanID = wrongPlan; return value }()},
		{name: "cancelled", ctx: func() context.Context {
			value, cancel := context.WithCancel(context.Background())
			cancel()
			return value
		}(), options: fixture.forgetOptions()},
	} {
		t.Run(test.name, func(t *testing.T) {
			report, err := Forget(test.ctx, test.options)
			if err == nil || report.WritesPerformed != 0 || report.WritesUncertain {
				t.Fatalf("report=%#v err=%v", report, err)
			}
			if _, statErr := os.Stat(filepath.Join(fixture.activation.targetRoot, operationName)); statErr != nil {
				t.Fatalf("invalid request changed tombstone: %v", statErr)
			}
			if _, statErr := os.Stat(filepath.Join(fixture.activation.targetRoot, rootName)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid request created root marker: %v", statErr)
			}
		})
	}
}

func TestForgetActivationRequiresExactRetainedNamespace(t *testing.T) {
	fixture := makeTerminalActivation(t, false)
	if _, err := Prune(context.Background(), fixture.options()); err != nil {
		t.Fatal(err)
	}
	operationName, _ := operationDirectoryName(fixture.operation)
	if err := os.WriteFile(filepath.Join(fixture.activation.targetRoot, operationName, "unexpected.bin"), []byte("unexpected"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Forget(context.Background(), fixture.forgetOptions())
	if !errors.Is(err, ErrIntegrity) || report.Outcome != ForgetOutcomeIntegrityFailed || report.WritesPerformed != 0 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
}

func TestForgetActivationRebindsRootIntentBeforeDeletingTombstone(t *testing.T) {
	fixture := makeTerminalActivation(t, false)
	if _, err := Prune(context.Background(), fixture.options()); err != nil {
		t.Fatal(err)
	}
	rootName, _ := ForgetRootName(fixture.operation)
	forgetTransitionHook = func(phase string) error {
		if phase == "root_intent_published" {
			return os.WriteFile(filepath.Join(fixture.activation.targetRoot, rootName), []byte("{}\n"), 0o600)
		}
		return nil
	}
	t.Cleanup(func() { forgetTransitionHook = nil })
	report, err := Forget(context.Background(), fixture.forgetOptions())
	if !errors.Is(err, ErrIntegrity) || report.Outcome != ForgetOutcomeIntegrityFailed || report.WritesPerformed == 0 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	operationName, _ := operationDirectoryName(fixture.operation)
	for _, path := range []string{
		filepath.Join(fixture.activation.targetRoot, operationName),
		filepath.Join(fixture.activation.targetRoot, operationName, retentionDirectoryName, retentionIntentName),
		filepath.Join(fixture.activation.targetRoot, operationName, retentionDirectoryName, retentionCompleteName),
	} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("root-intent corruption deleted retained authority %q: %v", path, statErr)
		}
	}
}

func TestActivationForgetMarkerCanonicalAndReserved(t *testing.T) {
	fixture := makeTerminalActivation(t, false)
	if _, err := Prune(context.Background(), fixture.options()); err != nil {
		t.Fatal(err)
	}
	operationName, _ := operationDirectoryName(fixture.operation)
	handle, _, err := openJournal(context.Background(), fixture.activation.targetRoot, fixture.operation, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := handle.retention
	marker := ForgetIntent{Schema: ForgetIntentSchemaV1, OperationID: fixture.operation, OperationRootIdentity: handle.subtree.Identity().String(),
		TargetRootIdentity: handle.session.Info().Identity.String(), PlanID: fixture.planID, RetentionIntentMarkerID: state.IntentID,
		RetentionCompleteMarkerID: state.CompleteID, RetentionIntent: state.Intent, RetentionComplete: state.Complete, Basis: ForgetBasisExact}
	_ = handle.Close()
	raw, id, err := EncodeForgetIntent(marker)
	if err != nil {
		t.Fatal(err)
	}
	decoded, decodedID, err := DecodeForgetIntent(strings.NewReader(string(raw)))
	if err != nil || decodedID != id || !sameForgetIntent(decoded, marker) {
		t.Fatalf("decoded=%#v id=%s/%s err=%v", decoded, id, decodedID, err)
	}
	unknown := append([]byte(nil), raw[:len(raw)-2]...)
	unknown = append(unknown, []byte(",\"unknown\":true}\n")...)
	if _, _, err := DecodeForgetIntent(strings.NewReader(string(unknown))); err == nil {
		t.Fatal("unknown forget field was accepted")
	}
	changed := marker
	changed.RetentionComplete.TerminalMarkerID = MarkerID("sha256:" + strings.Repeat("f", 64))
	if _, _, err := EncodeForgetIntent(changed); err == nil {
		t.Fatal("forget marker with a contradictory terminal link was accepted")
	}
	rootName, err := ForgetRootName(fixture.operation)
	if err != nil || !strings.HasPrefix(rootName, ".ptctl-client-activate-forget-") || operationName == rootName {
		t.Fatalf("operation=%q root=%q err=%v", operationName, rootName, err)
	}
}
