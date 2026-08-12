package clientstop

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/reconcile"
)

func TestVerifyStopCompletionReadsLiveAndRetainedAuthority(t *testing.T) {
	fixture := makeTerminalStopFixture(t, true)
	verified, observation, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.root, OperationID: fixture.operation, ExpectedPlanID: fixture.planID,
	})
	if err != nil || verified == nil || !verified.Verified() || observation.RetainedTombstone ||
		observation.OperationID != fixture.operation.String() || observation.PlanID != fixture.planID ||
		observation.IntentID != fixture.intentID.String() || observation.CompletionID != fixture.completionID.String() ||
		observation.CompletionBasis != "accepted_response_then_exact_stopped" ||
		observation.Assurance != "same_invocation_bound_canonical_terminal_client_stop_journal_read_without_current_client_inference" {
		t.Fatalf("verified=%#v observation=%#v err=%v", verified, observation, err)
	}
	public, ok := verified.ReconciliationStopCompletion()
	if !ok || public.CompletionID != fixture.completionID.String() || public.RetainedTombstone {
		t.Fatalf("public=%#v ok=%t", public, ok)
	}

	if _, pruneErr := Prune(context.Background(), fixture.pruneOptions()); pruneErr != nil {
		t.Fatal(pruneErr)
	}
	retained, retainedObservation, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.root, OperationID: fixture.operation, ExpectedPlanID: fixture.planID,
	})
	if err != nil || retained == nil || !retained.Verified() || !retainedObservation.RetainedTombstone ||
		retainedObservation.CompletionID != observation.CompletionID || retainedObservation.IntentID != observation.IntentID ||
		retainedObservation.Assurance != "same_invocation_bound_canonical_client_stop_retention_tombstone_read_without_current_client_inference" {
		t.Fatalf("retained=%#v observation=%#v err=%v", retained, retainedObservation, err)
	}

	raw, err := json.Marshal(struct {
		Completion CompletionObservation `json:"completion"`
		Authority  *VerifiedCompletion   `json:"authority"`
	}{Completion: retainedObservation, Authority: retained})
	if err != nil {
		t.Fatal(err)
	}
	var copied struct {
		Completion CompletionObservation `json:"completion"`
		Authority  VerifiedCompletion    `json:"authority"`
	}
	if err := json.Unmarshal(raw, &copied); err != nil {
		t.Fatal(err)
	}
	if copied.Authority.Verified() {
		t.Fatal("JSON round-trip recreated stop authority")
	}
	if _, ok := copied.Authority.ReconciliationStopCompletion(); ok {
		t.Fatal("JSON round-trip exposed stop reconciliation proof")
	}
}

func TestStopCurrentBridgeRequiresMatchingStoppedCurrentUse(t *testing.T) {
	fixture := makeTerminalStopFixture(t, true)
	verified, observation, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.root, OperationID: fixture.operation, ExpectedPlanID: fixture.planID,
	})
	if err != nil {
		t.Fatal(err)
	}
	completion, ok := verified.ReconciliationStopCompletion()
	if !ok {
		t.Fatal("completion bridge unavailable")
	}
	current := reconcile.ClientActivationCurrentUse{
		Driver: completion.Driver, UseID: completion.UseID, JobID: completion.JobID,
		FileLayoutID: completion.FileLayoutID, CompleteSnapshotID: completion.CompleteSnapshotID,
		JobState: "stoppedUP", JobProgress: 1, ObservedAtStart: completion.ObservedAtEnd.Add(1),
		ObservedAtEnd: completion.ObservedAtEnd.Add(2), FinalObjectIdentity: completion.FinalObjectIdentity,
	}
	bound, ok := verified.ReconcileCurrentStopped(current)
	if !ok || bound.JobState != "stoppedUP" || bound.JobID != observation.JobID {
		t.Fatalf("bound=%#v ok=%t", bound, ok)
	}
	current.JobState = "uploading"
	if _, ok := verified.ReconcileCurrentStopped(current); ok {
		t.Fatal("started current job was accepted as currently stopped")
	}
	current.JobState = "stoppedUP"
	current.CompleteSnapshotID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, ok := verified.ReconcileCurrentStopped(current); ok {
		t.Fatal("changed current layout snapshot was accepted")
	}
}
