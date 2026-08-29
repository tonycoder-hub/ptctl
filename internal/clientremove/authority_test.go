package clientremove

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestVerifyCompletionKeepsTerminalRemovalAuthorityProcessLocal(t *testing.T) {
	fixture := makeTerminalRemovalFixture(t, true)
	verified, observation, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.root, OperationID: fixture.operation, ExpectedPlanID: fixture.planID,
	})
	if err != nil || verified == nil || !verified.Verified() || observation.RetainedTombstone ||
		observation.CompletionBasis != "accepted_response_then_exact_absence" ||
		observation.IntentID != fixture.intentID.String() || observation.CompletionID != fixture.completionID.String() ||
		!verified.Matches(fixture.operation, fixture.planID, fixture.plan.MetafileVariantID,
			fixture.plan.MaterializeOperationID, fixture.plan.MaterializePlanID, fixture.plan.ActivationOperationID,
			fixture.plan.ActivationPlanID, fixture.plan.ActivationTerminalID, fixture.plan.FinalObjectIdentity) {
		t.Fatalf("verified=%#v observation=%#v err=%v", verified, observation, err)
	}
	raw, err := json.Marshal(verified)
	if err != nil || strings.Contains(string(raw), fixture.root) {
		t.Fatalf("serialized authority leaked a private locator: %s err=%v", raw, err)
	}
	var replay VerifiedCompletion
	if err := json.Unmarshal(raw, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Verified() || replay.Observation().OperationID != "" {
		t.Fatal("serialized removal observation recreated process-local authority")
	}
	prerequisite, ok := verified.AdoptionReAddPrerequisite()
	if !ok || prerequisite.OperationID != observation.OperationID || prerequisite.PlanID != observation.PlanID ||
		prerequisite.CompletionID != observation.CompletionID || prerequisite.CompletionBasis != "accepted_response_then_exact_absence" ||
		prerequisite.InfoHashV1 != fixture.plan.InfoHashV1 || prerequisite.InfoHashV2 != fixture.plan.InfoHashV2 ||
		prerequisite.RetainedTombstone {
		t.Fatalf("re-add prerequisite=%#v ok=%v", prerequisite, ok)
	}
	if _, ok := replay.AdoptionReAddPrerequisite(); ok {
		t.Fatal("serialized removal authority authorized a stopped re-add")
	}
}

func TestVerifyCompletionReadsExactRetainedRemovalTombstone(t *testing.T) {
	fixture := makeTerminalRemovalFixture(t, true)
	if _, err := Prune(context.Background(), fixture.pruneOptions()); err != nil {
		t.Fatal(err)
	}
	verified, observation, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.root, OperationID: fixture.operation, ExpectedPlanID: fixture.planID,
	})
	if err != nil || verified == nil || !verified.Verified() || !observation.RetainedTombstone ||
		observation.CompletionID != fixture.completionID.String() || !strings.Contains(observation.Assurance, "retention_tombstone") {
		t.Fatalf("verified=%#v observation=%#v err=%v", verified, observation, err)
	}
	if prerequisite, ok := verified.AdoptionReAddPrerequisite(); !ok || !prerequisite.RetainedTombstone {
		t.Fatalf("retained re-add prerequisite=%#v ok=%v", prerequisite, ok)
	}
}

func TestVerifyCompletionPreservesUnattributedHistoricalBasisAndRejectsSelectors(t *testing.T) {
	fixture := makeTerminalRemovalFixture(t, false)
	verified, observation, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.root, OperationID: fixture.operation, ExpectedPlanID: fixture.planID,
	})
	if err != nil || verified == nil || observation.CompletionBasis != "exact_absence_after_unknown_attempt_causality_unproven" {
		t.Fatalf("verified=%#v observation=%#v err=%v", verified, observation, err)
	}
	if _, ok := verified.AdoptionReAddPrerequisite(); ok {
		t.Fatal("unattributed absence authorized a stopped re-add")
	}
	wrongPlan := strings.Repeat("f", 24)
	if got, public, selectorErr := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.root, OperationID: fixture.operation, ExpectedPlanID: wrongPlan,
	}); got != nil || public.OperationID != "" || !errors.Is(selectorErr, ErrPolicy) {
		t.Fatalf("wrong selector verified=%#v observation=%#v err=%v", got, public, selectorErr)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got, public, cancelErr := VerifyCompletion(cancelled, CompletionProofOptions{
		TargetRoot: fixture.root, OperationID: fixture.operation, ExpectedPlanID: fixture.planID,
	}); got != nil || public.OperationID != "" || !errors.Is(cancelErr, context.Canceled) {
		t.Fatalf("cancelled verified=%#v observation=%#v err=%v", got, public, cancelErr)
	}
}
