package clientadopt

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
)

func TestVerifiedCompletionBindsExistingReconciliationBracketWithoutRequests(t *testing.T) {
	fixture := makeTerminalAdoptionFixture(t)
	verified, observation, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.materialized.targetRoot, OperationID: fixture.prepared.OperationID(), ExpectedPlanID: fixture.prepared.PlanID(),
	})
	if err != nil || !verified.Verified() {
		t.Fatalf("verified=%#v observation=%#v err=%v", verified, observation, err)
	}
	completion, ok := verified.ReconciliationAdoptionCompletion()
	if !ok || completion.OperationID != observation.OperationID || completion.CompletionID != observation.CompletionID ||
		completion.MetafileVariantID != fixture.materialized.meta.MetafileVariantID || completion.RetainedTombstone {
		t.Fatalf("completion=%#v ok=%t", completion, ok)
	}

	start := completion.ObservedAtEnd.Add(time.Millisecond)
	before := fixture.after
	before.ObservedAtStart, before.ObservedAtEnd = start, start.Add(time.Millisecond)
	after := before
	after.ObservedAtStart, after.ObservedAtEnd = before.ObservedAtEnd.Add(time.Millisecond), before.ObservedAtEnd.Add(2*time.Millisecond)
	bracket := reconcile.ClientBracket{Requested: true, Before: &before, After: &after, RequestsMade: 3,
		FileLayoutMode: "auto", FileLimits: downloader.DefaultJobFileLedgerLimits()}
	current, ok := verified.ReconcileAdoptedCurrentJob(bracket)
	if !ok || current.JobID != completion.JobID || current.ContentPathRef != completion.ExpectedContentPathRef ||
		current.SavePathRef != completion.ExpectedSavePathRef || current.RequestsMade != 3 ||
		current.ObservedAtStart != before.ObservedAtStart || current.ObservedAtEnd != after.ObservedAtEnd {
		t.Fatalf("current=%#v ok=%t", current, ok)
	}

	moved := after
	moved.Jobs = append([]downloader.Torrent(nil), after.Jobs...)
	moved.Jobs[0].ContentPath += "-moved"
	if _, ok := verified.ReconcileAdoptedCurrentJob(reconcile.ClientBracket{Requested: true, Before: &before, After: &moved, RequestsMade: 3}); ok {
		t.Fatal("moved current job unexpectedly retained adoption authority")
	}
	replaced := after
	replaced.Jobs = append([]downloader.Torrent(nil), after.Jobs...)
	replaced.Jobs[0].Hash = "replacement-job-key"
	if _, ok := verified.ReconcileAdoptedCurrentJob(reconcile.ClientBracket{Requested: true, Before: &replaced, After: &replaced, RequestsMade: 3}); ok {
		t.Fatal("replacement opaque job key unexpectedly retained adoption authority")
	}

	raw, err := json.Marshal(verified)
	if err != nil {
		t.Fatal(err)
	}
	var replay VerifiedCompletion
	if err := json.Unmarshal(raw, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Verified() {
		t.Fatal("serialized completion recreated process-local authority")
	}
}

func TestRetainedAdoptionCompletionStillBindsHistoricalLineage(t *testing.T) {
	fixture := makeTerminalAdoptionFixture(t)
	if _, err := Prune(context.Background(), fixture.pruneOptions()); err != nil {
		t.Fatal(err)
	}
	verified, _, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.materialized.targetRoot, OperationID: fixture.prepared.OperationID(), ExpectedPlanID: fixture.prepared.PlanID(),
	})
	completion, ok := verified.ReconciliationAdoptionCompletion()
	if err != nil || !ok || !completion.RetainedTombstone ||
		completion.Assurance != "same_invocation_bound_canonical_adoption_retention_tombstone_read_without_durability_refresh_or_current_client_observation" {
		t.Fatalf("completion=%#v ok=%t err=%v", completion, ok, err)
	}
}

func TestTransmissionAdoptionBridgeUsesItsExistingFourRequestBracket(t *testing.T) {
	fixture := makeTerminalAdoptionFixture(t)
	plan := fixture.prepared.plan
	plan.Driver = downloader.DriverTransmission
	planID, err := PlanID(plan)
	if err != nil {
		t.Fatal(err)
	}
	operationID := OperationIDForPlan(planID)
	completion := fixtureCompletion(t, fixture)
	completion.OperationID, completion.PlanID = operationID, planID
	attempt := fixtureAttempt(t, fixture)
	attempt.OperationID, attempt.PlanID = operationID, planID
	job := fixture.after.Jobs[0]
	job.Hash = fixture.materialized.meta.InfoHashV1
	job.IdentityEvidence = []string{"transmission_hash_string_sha1"}
	completion.JobID = jobID(job.Hash)
	_, completionID, err := encodeCompletion(completion)
	if err != nil {
		t.Fatal(err)
	}
	verified := &VerifiedCompletion{authority: &verifiedCompletionAuthority{
		plan: plan, planID: planID, operationID: operationID, attempt: attempt, completion: completion, completionID: completionID,
	}}
	if !verified.Verified() {
		t.Fatal("synthetic canonical Transmission completion was not verified")
	}
	start := completion.ObservedAtEnd.Add(time.Millisecond)
	before := ledgerSnapshotForDriver(fixture.materialized.meta, downloader.DriverTransmission, &job, start)
	after := ledgerSnapshotForDriver(fixture.materialized.meta, downloader.DriverTransmission, &job, before.ObservedAtEnd.Add(time.Millisecond))
	current, ok := verified.ReconcileAdoptedCurrentJob(reconcile.ClientBracket{
		Requested: true, Before: &before, After: &after, RequestsMade: 4,
		FileLayoutMode: "auto", FileLimits: downloader.DefaultJobFileLedgerLimits(),
	})
	if !ok || current.Driver != downloader.DriverTransmission || current.RequestsMade != 4 || current.JobID != completion.JobID {
		t.Fatalf("current=%#v ok=%t", current, ok)
	}
	if _, ok := verified.ReconcileAdoptedCurrentJob(reconcile.ClientBracket{
		Requested: true, Before: &before, After: &after, RequestsMade: 3,
	}); ok {
		t.Fatal("Transmission bridge accepted an impossible request count")
	}
}

func fixtureCompletion(t *testing.T, fixture terminalAdoptionFixture) Completion {
	t.Helper()
	verified, _, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.materialized.targetRoot, OperationID: fixture.prepared.OperationID(), ExpectedPlanID: fixture.prepared.PlanID(),
	})
	if err != nil || !verified.Verified() {
		t.Fatalf("completion fixture unavailable: %v", err)
	}
	return verified.authority.completion
}

func fixtureAttempt(t *testing.T, fixture terminalAdoptionFixture) Attempt {
	t.Helper()
	verified, _, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.materialized.targetRoot, OperationID: fixture.prepared.OperationID(), ExpectedPlanID: fixture.prepared.PlanID(),
	})
	if err != nil || !verified.Verified() {
		t.Fatalf("attempt fixture unavailable: %v", err)
	}
	return verified.authority.attempt
}
