package sourceretire

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestParentCleanupCompletionAndCurrentAbsenceAuthorities(t *testing.T) {
	fixture, _, _, preview, operation, _ := completedParentCleanupFixture(t)
	verified, observation, err := VerifyParentCleanupCompletion(context.Background(), ParentCleanupCompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedCleanupPlanID: preview.Plan.ID,
		Limits: DefaultParentCleanupExecutionLimits(),
	})
	if err != nil || !verified.Verified() || observation.RetainedTombstone || observation.ParentsRemoved != 1 ||
		observation.RetiredFiles != 1 || observation.CleanupPlanID != preview.Plan.ID {
		t.Fatalf("completion=%#v verified=%#v err=%v", observation, verified, err)
	}
	absence, current, err := VerifyCurrentParentCleanupAbsence(context.Background(), verified, []string{fixture.sourceRoot}, false)
	if err != nil || !absence.Verified() || !absence.Matches(verified) || current.ParentsChecked != 1 ||
		current.RetiredFiles != 1 || current.AbsenceID == "" || current.ObservedAtEnd.Before(current.ObservedAtStart) {
		t.Fatalf("absence=%#v verified=%#v err=%v", current, absence, err)
	}

	raw, err := json.Marshal(verified)
	if err != nil {
		t.Fatal(err)
	}
	var copied VerifiedParentCleanupCompletion
	if err := json.Unmarshal(raw, &copied); err != nil {
		t.Fatal(err)
	}
	if copied.Verified() {
		t.Fatal("JSON round trip recreated parent-cleanup completion authority")
	}
	raw, err = json.Marshal(absence)
	if err != nil {
		t.Fatal(err)
	}
	var copiedAbsence VerifiedParentCleanupCurrentAbsence
	if err := json.Unmarshal(raw, &copiedAbsence); err != nil {
		t.Fatal(err)
	}
	if copiedAbsence.Verified() {
		t.Fatal("JSON round trip recreated removed-parent absence authority")
	}
}

func TestParentCleanupAbsenceCanBindCurrentRetiredNameAbsence(t *testing.T) {
	fixture, retirementPlan, retirementOperation, cleanupPreview, cleanupOperation, _ := completedParentCleanupFixture(t)
	pruned, err := PruneExecution(context.Background(), ExecutionPruneOptions{
		TargetRoot: fixture.targetRoot, OperationID: retirementOperation, ExpectedPlanID: retirementPlan.ID,
		Acknowledge: true, JournalLimits: DefaultExecutionLimits(), RetentionLimits: DefaultExecutionRetentionLimits(),
	})
	if err != nil || pruned.Outcome != ExecutionRetentionOutcomePruned || !pruned.Markers.ExactTombstone {
		t.Fatalf("retirement prune=%#v err=%v", pruned, err)
	}
	retirement, retired, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: retirementOperation, ExpectedPlanID: retirementPlan.ID,
		Limits: DefaultExecutionLimits(),
	})
	if err != nil || retirement == nil || !retirement.Verified() || !retired.RetainedTombstone {
		t.Fatalf("retirement=%#v observation=%#v err=%v", retirement, retired, err)
	}
	cleanup, cleaned, err := VerifyParentCleanupCompletion(context.Background(), ParentCleanupCompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: cleanupOperation, ExpectedCleanupPlanID: cleanupPreview.Plan.ID,
		Limits: DefaultParentCleanupExecutionLimits(),
	})
	if err != nil || cleanup == nil || !cleanup.Verified() {
		t.Fatalf("cleanup=%#v observation=%#v err=%v", cleanup, cleaned, err)
	}
	parentAbsence, parentObservation, err := VerifyCurrentParentCleanupAbsence(context.Background(), cleanup, []string{fixture.sourceRoot}, false)
	if err != nil || parentAbsence == nil || !parentAbsence.Verified() {
		t.Fatalf("parent absence=%#v observation=%#v err=%v", parentAbsence, parentObservation, err)
	}

	derived, observation, err := BindCurrentRetiredNameAbsenceFromParentCleanup(retirement, cleanup, parentAbsence)
	if err != nil || derived == nil || !derived.Verified() || !derived.Matches(retirement) ||
		observation.OperationID != retired.OperationID || observation.CompletionID != retired.CompletionID ||
		observation.FilesChecked != retired.FilesRetired || observation.BytesRetired != retired.BytesRetired ||
		observation.ParentDirectoriesChecked != parentObservation.ParentsChecked ||
		observation.Assurance != "same_invocation_two_pass_identity_bound_removed_parent_absence_implies_retired_name_absence_bracketed_non_atomic" {
		t.Fatalf("derived=%#v observation=%#v err=%v", derived, observation, err)
	}
	directBasis := observation
	directBasis.Assurance = "same_invocation_two_pass_identity_bound_retired_name_absence_bracketed_non_atomic"
	if currentAbsenceID(directBasis) == observation.AbsenceID {
		t.Fatal("retired-name absence ID did not bind its direct versus parent-cleanup-derived evidence basis")
	}

	raw, err := json.Marshal(derived)
	if err != nil {
		t.Fatal(err)
	}
	var copied VerifiedCurrentAbsence
	if err := json.Unmarshal(raw, &copied); err != nil {
		t.Fatal(err)
	}
	if copied.Verified() {
		t.Fatal("JSON round trip recreated parent-cleanup-derived retired-name absence authority")
	}
}

func TestParentCleanupCurrentAbsenceDetectsReappearedParent(t *testing.T) {
	fixture, _, _, preview, operation, parent := completedParentCleanupFixture(t)
	verified, _, err := VerifyParentCleanupCompletion(context.Background(), ParentCleanupCompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedCleanupPlanID: preview.Plan.ID,
		Limits: DefaultParentCleanupExecutionLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyCurrentParentCleanupAbsence(context.Background(), verified, []string{fixture.sourceRoot}, false); !errors.Is(err, ErrRemovedParentPresent) {
		t.Fatalf("reappeared parent error=%v", err)
	}
}

func TestRetainedParentCleanupCompletionCannotRecreatePathAuthority(t *testing.T) {
	fixture, _, _, preview, operation, _ := retainedParentCleanupFixture(t)
	verified, observation, err := VerifyParentCleanupCompletion(context.Background(), ParentCleanupCompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedCleanupPlanID: preview.Plan.ID,
		Limits: DefaultParentCleanupExecutionLimits(),
	})
	if err != nil || !verified.Verified() || !observation.RetainedTombstone || observation.ParentsRemoved != 1 || observation.RetiredFiles != 1 {
		t.Fatalf("retained completion=%#v verified=%#v err=%v", observation, verified, err)
	}
	if _, _, err := VerifyCurrentParentCleanupAbsence(context.Background(), verified, []string{fixture.sourceRoot}, false); !errors.Is(err, ErrCurrentParentAbsenceUnavailable) {
		t.Fatalf("retained tombstone recreated path authority: %v", err)
	}
}

func TestParentCleanupCompletionSelectorAndCancellationFailClosed(t *testing.T) {
	fixture, _, _, preview, operation, _ := completedParentCleanupFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := VerifyParentCleanupCompletion(ctx, ParentCleanupCompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedCleanupPlanID: preview.Plan.ID,
		Limits: DefaultParentCleanupExecutionLimits(),
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel error=%v", err)
	}
	badPlan := markerIDPrefix + "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if _, _, err := VerifyParentCleanupCompletion(context.Background(), ParentCleanupCompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedCleanupPlanID: badPlan,
		Limits: DefaultParentCleanupExecutionLimits(),
	}); !errors.Is(err, ErrExecutionPolicy) {
		t.Fatalf("selector mismatch error=%v", err)
	}
}
