package clientactivate

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestVerifyCompletionRequiresReviewedTerminalAction(t *testing.T) {
	fixture := makeActivationFixture(t)
	now := time.Now().UTC()
	stopped := fixture.job("stoppedDL", 0.25)
	checking := fixture.job("checkingDL", 0.25)
	complete := fixture.job("stoppedUP", 1)
	started := fixture.job("uploading", 1)
	preview, err := Preview(context.Background(), fixture.authority,
		newActivationSession(fixture, activationLedger(fixture.meta, &stopped, now)), true)
	if err != nil {
		t.Fatal(err)
	}
	run, err := Run(context.Background(), RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
		Session: newActivationSession(fixture, activationLedger(fixture.meta, &stopped, now.Add(time.Second)),
			activationLedger(fixture.meta, &checking, now.Add(2*time.Second))),
		StartAfterRecheck: true, AcknowledgeRecheck: true})
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := ParseOperationID(run.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The recheck completion exists after this resume, but start was reviewed and
	// has not completed, so it is not terminal authority for retirement.
	checked, err := Resume(context.Background(), operationID, RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
		Session:           newActivationSession(fixture, activationLedger(fixture.meta, &complete, now.Add(3*time.Second))),
		StartAfterRecheck: true})
	if err != nil || checked.Outcome != OutcomeCheckedStopped {
		t.Fatalf("checked=%#v err=%v", checked, err)
	}
	if _, _, err := VerifyCompletion(context.Background(), CompletionProofOptions{TargetRoot: fixture.targetRoot,
		OperationID: operationID, ExpectedPlanID: preview.Plan.ID}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("intermediate checked state became terminal authority: %v", err)
	}

	resumed, err := Resume(context.Background(), operationID, RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
		Session: newActivationSession(fixture, activationLedger(fixture.meta, &complete, now.Add(4*time.Second)),
			activationLedger(fixture.meta, &started, now.Add(5*time.Second))),
		StartAfterRecheck: true, AcknowledgeStart: true})
	if err != nil || resumed.Outcome != OutcomeStartedClientClaim {
		t.Fatalf("resumed=%#v err=%v", resumed, err)
	}
	verified, observation, err := VerifyCompletion(context.Background(), CompletionProofOptions{TargetRoot: fixture.targetRoot,
		OperationID: operationID, ExpectedPlanID: preview.Plan.ID})
	if err != nil || !verified.Verified() || observation.TerminalPhase != "started_client_claim_observed" ||
		observation.ActivationCompletionID == "" || observation.TerminalMarkerID != observation.ActivationCompletionID ||
		observation.ObservedAtStart == "" || observation.ObservedAtEnd == "" ||
		!verified.Matches(operationID, preview.Plan.ID, fixture.meta.MetafileVariantID,
			fixture.verifiedFinal.Observation().OperationID, fixture.verifiedFinal.Observation().MaterializePlanID,
			fixture.verifiedFinal.Observation().FinalObjectIdentity) {
		t.Fatalf("verified=%v observation=%#v err=%v", verified != nil && verified.Verified(), observation, err)
	}
}

func TestVerifyCompletionAcceptsTerminalRecheckOnlyAndPreservesCancellation(t *testing.T) {
	fixture := makeActivationFixture(t)
	now := time.Now().UTC()
	incomplete := fixture.job("stoppedDL", 0.25)
	complete := fixture.job("stoppedUP", 1)
	preview, err := Preview(context.Background(), fixture.authority,
		newActivationSession(fixture, activationLedger(fixture.meta, &incomplete, now)), false)
	if err != nil {
		t.Fatal(err)
	}
	run, err := Run(context.Background(), RunOptions{Authority: fixture.authority, ExpectedPlanID: preview.Plan.ID,
		Session: newActivationSession(fixture, activationLedger(fixture.meta, &incomplete, now.Add(time.Second)),
			activationLedger(fixture.meta, &complete, now.Add(2*time.Second))),
		AcknowledgeRecheck: true})
	if err != nil || run.Outcome != OutcomeCheckedStopped {
		t.Fatalf("run=%#v err=%v", run, err)
	}
	operationID, err := ParseOperationID(run.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	verified, observation, err := VerifyCompletion(context.Background(), CompletionProofOptions{TargetRoot: fixture.targetRoot,
		OperationID: operationID, ExpectedPlanID: preview.Plan.ID})
	if err != nil || !verified.Verified() || observation.TerminalPhase != "recheck_complete_stopped" ||
		observation.ActivationCompletionID != "" || observation.TerminalMarkerID != observation.RecheckCompletionID ||
		observation.ObservedAtStart == "" || observation.ObservedAtEnd == "" {
		t.Fatalf("verified=%v observation=%#v err=%v", verified != nil && verified.Verified(), observation, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := VerifyCompletion(cancelled, CompletionProofOptions{TargetRoot: fixture.targetRoot,
		OperationID: operationID, ExpectedPlanID: preview.Plan.ID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was not preserved: %v", err)
	}
}
