package sourceretire

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyCompletionAndCurrentAbsenceKeepHistoricalAndCurrentAuthoritySeparate(t *testing.T) {
	fixture, plan, operation := completedRetirementFixture(t)
	verified, completion, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: plan.ID, Limits: DefaultExecutionLimits(),
	})
	if err != nil || verified == nil || !verified.Verified() || completion.RetainedTombstone ||
		completion.OperationID != operation.String() || completion.PlanID != plan.ID || completion.FilesRetired != 1 ||
		completion.BytesRetired != int64(len("retirement source fixture")) ||
		!verified.Matches(operation, plan.ID, plan.MetafileVariantID, plan.MaterializeOperationID, plan.MaterializePlanID,
			plan.ActivationOperationID, plan.ActivationPlanID, plan.ClientCompletionID, plan.FinalObjectIdentity) {
		t.Fatalf("completion=%#v verified=%#v err=%v", completion, verified, err)
	}
	absence, observed, err := VerifyCurrentAbsence(context.Background(), verified, []string{fixture.sourceRoot}, false)
	if err != nil || absence == nil || !absence.Verified() || !absence.Matches(verified) || observed.FilesChecked != 1 ||
		observed.ParentDirectoriesChecked != 1 || observed.SearchScopeID != completion.SearchScopeID || observed.AbsenceID == "" ||
		observed.ObservedAtStart.IsZero() || observed.ObservedAtEnd.Before(observed.ObservedAtStart) {
		t.Fatalf("absence=%#v verified=%#v err=%v", observed, absence, err)
	}

	public, err := json.Marshal(struct {
		Completion CompletionObservation     `json:"completion"`
		Absence    CurrentAbsenceObservation `json:"absence"`
	}{Completion: completion, Absence: observed})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{fixture.sourceRoot, fixture.sourcePath, "PRIVATE-SOURCE-PATH-CANARY"} {
		if strings.Contains(string(public), secret) {
			t.Fatalf("public retirement observation leaked %q: %s", secret, public)
		}
	}
	var copiedCompletion VerifiedCompletion
	if raw, marshalErr := json.Marshal(verified); marshalErr != nil {
		t.Fatal(marshalErr)
	} else if unmarshalErr := json.Unmarshal(raw, &copiedCompletion); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	var copiedAbsence VerifiedCurrentAbsence
	if raw, marshalErr := json.Marshal(absence); marshalErr != nil {
		t.Fatal(marshalErr)
	} else if unmarshalErr := json.Unmarshal(raw, &copiedAbsence); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if copiedCompletion.Verified() || copiedAbsence.Verified() {
		t.Fatal("serialized retirement observations recreated process-local authority")
	}

	if err := os.WriteFile(fixture.sourcePath, []byte("replacement source name"), 0o600); err != nil {
		t.Fatal(err)
	}
	if current, observation, currentErr := VerifyCurrentAbsence(context.Background(), verified, []string{fixture.sourceRoot}, false); current != nil || observation.AbsenceID != "" || !errors.Is(currentErr, ErrRetiredNamePresent) {
		t.Fatalf("reappeared source current=%#v observation=%#v err=%v", current, observation, currentErr)
	}
}

func TestRetainedCompletionCannotRecreatePrunedPathAuthority(t *testing.T) {
	fixture, plan, operation := retainedRetirementFixture(t)
	verified, observation, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: plan.ID, Limits: DefaultExecutionLimits(),
	})
	if err != nil || verified == nil || !verified.Verified() || !observation.RetainedTombstone ||
		!strings.Contains(observation.Assurance, "retention_tombstone") {
		t.Fatalf("retained completion=%#v verified=%#v err=%v", observation, verified, err)
	}
	if current, currentObservation, currentErr := VerifyCurrentAbsence(context.Background(), verified, []string{fixture.sourceRoot}, false); current != nil || currentObservation.AbsenceID != "" || !errors.Is(currentErr, ErrCurrentAbsenceUnavailable) {
		t.Fatalf("retained tombstone recreated path authority: current=%#v observation=%#v err=%v", current, currentObservation, currentErr)
	}
}

func TestRetirementCompletionSelectorAndCancellationFailBeforeAuthority(t *testing.T) {
	fixture, plan, operation := completedRetirementFixture(t)
	wrongPlan := "sha256:" + strings.Repeat("f", 64)
	if verified, observation, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: wrongPlan, Limits: DefaultExecutionLimits(),
	}); verified != nil || observation.OperationID != "" || !errors.Is(err, ErrExecutionPolicy) {
		t.Fatalf("wrong selector verified=%#v observation=%#v err=%v", verified, observation, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if verified, observation, err := VerifyCompletion(cancelled, CompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: plan.ID, Limits: DefaultExecutionLimits(),
	}); verified != nil || observation.OperationID != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled selector verified=%#v observation=%#v err=%v", verified, observation, err)
	}
}

func TestExecutionIntentRejectsConflictingIdentityForOneParentPath(t *testing.T) {
	fixture, plan, operation := completedRetirementFixture(t)
	verified, _, err := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: plan.ID, Limits: DefaultExecutionLimits(),
	})
	if err != nil || verified == nil || verified.authority == nil || verified.authority.intent == nil {
		t.Fatalf("verified=%#v err=%v", verified, err)
	}
	intent := *verified.authority.intent
	intent.Files = append([]IntentFile(nil), intent.Files...)
	duplicate := intent.Files[0]
	duplicate.Sequence = 1
	duplicate.ManifestIndex++
	duplicate.Name = "SECOND-PARENT-IDENTITY-CANARY.bin"
	duplicate.SourcePathRef = sourcePathRef(filepath.Join(duplicate.ParentPath, duplicate.Name))
	duplicate.ParentIdentity = duplicate.SourceObjectIdentity
	intent.Files = append(intent.Files, duplicate)
	intent.ContentBytes += duplicate.SizeBytes
	if err := intent.Validate(); !errors.Is(err, ErrExecutionIntegrity) {
		t.Fatalf("conflicting parent identity was accepted: %v", err)
	}
}
