package sourceretire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

func parentCleanupRunOptions(fixture retireFixture, plan Plan, operation OperationID, cleanupPlanID string) ParentCleanupRunOptions {
	return ParentCleanupRunOptions{
		Review: parentCleanupOptions(fixture, plan, operation), ExpectedCleanupPlanID: cleanupPlanID,
		Acknowledge: true, Limits: DefaultParentCleanupExecutionLimits(),
	}
}

func TestParentCleanupExecutionRemovesOnlyReviewedEmptyParentAndReportsHistoricalStatus(t *testing.T) {
	fixture, retirementPlan, retirementOperation, parent := completedNestedRetirementFixture(t)
	preview, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, retirementOperation))
	if err != nil || preview.Outcome != ParentCleanupOutcomeEligible {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	report, err := RunParentCleanup(context.Background(), parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, preview.Plan.ID))
	if err != nil || report.Outcome != ParentCleanupExecutionOutcomeRemoved || !report.DeletionPerformed || report.WritesPerformed == 0 ||
		report.Writes.DirectoriesRemoved != 1 || report.Writes.RemovalAttempts != 1 || report.Operation.Status != "complete" ||
		report.Operation.Resumable || len(report.Directories) != 1 || report.Directories[0].Status != "removed" ||
		report.Directories[0].RemovalBasis != ParentCleanupRemovalBasisConfirmed || report.Eligibility == nil ||
		report.Eligibility.Blockers == nil || report.Eligibility.Issues == nil || report.Eligibility.Warnings == nil {
		t.Fatalf("run=%#v err=%v", report, err)
	}
	if _, err := os.Lstat(parent); !os.IsNotExist(err) {
		t.Fatalf("reviewed empty parent remains: %v", err)
	}
	if _, err := os.Stat(fixture.sourceRoot); err != nil {
		t.Fatalf("search root was removed: %v", err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{parent, filepath.Base(parent), fixture.sourcePath} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("execution report leaked %q: %s", secret, raw)
		}
	}
	operation, err := ParseParentCleanupOperationID(report.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	status, err := ParentCleanupStatus(context.Background(), ParentCleanupStatusOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, Limits: DefaultParentCleanupExecutionLimits(),
	})
	if err != nil || status.Outcome != ParentCleanupExecutionOutcomeAlreadyRemoved || status.Operation.Status != "historical_complete" ||
		status.Operation.Resumable || status.DeletionPerformed || status.WritesPerformed != 0 || len(status.Directories) != 1 ||
		status.Directories[0].ParentPath != "" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	rerun, err := RunParentCleanup(context.Background(), parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, preview.Plan.ID))
	if err != nil || rerun.Outcome != ParentCleanupExecutionOutcomeAlreadyRemoved || rerun.WritesPerformed != 0 || rerun.DeletionPerformed {
		t.Fatalf("idempotent run=%#v err=%v", rerun, err)
	}
	if !containsString(rerun.Effect, "read_retired_source_parent_namespaces") {
		t.Fatalf("idempotent run omitted its live parent-namespace read: %#v", rerun.Effect)
	}
	resumed, err := ResumeParentCleanup(context.Background(), ParentCleanupResumeOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedCleanupPlanID: preview.Plan.ID,
		SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultParentCleanupExecutionLimits(),
	})
	if err != nil || resumed.Outcome != ParentCleanupExecutionOutcomeAlreadyRemoved || resumed.DeletionPerformed || resumed.WritesPerformed != 0 {
		t.Fatalf("completed resume=%#v err=%v", resumed, err)
	}
	if !containsString(resumed.Effect, "read_retired_source_parent_namespaces") {
		t.Fatalf("completed resume omitted its live parent-namespace read: %#v", resumed.Effect)
	}
	wrongRetirementPlan := markerIDPrefix + strings.Repeat("f", 64)
	wrongRetirementOperation, err := executionOperationID(wrongRetirementPlan)
	if err != nil {
		t.Fatal(err)
	}
	wrongSelectors := parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, preview.Plan.ID)
	wrongSelectors.Review.OperationID = wrongRetirementOperation
	wrongSelectors.Review.ExpectedPlanID = wrongRetirementPlan
	blocked, err := RunParentCleanup(context.Background(), wrongSelectors)
	if err != nil || blocked.Outcome != ParentCleanupExecutionOutcomeBlocked || blocked.WritesPerformed != 0 || blocked.DeletionPerformed ||
		!hasFinding(blocked.Blockers, "selector.retirement_mismatch") {
		t.Fatalf("mismatched retirement selectors=%#v err=%v", blocked, err)
	}
}

func TestParentCleanupExecutionRechecksAfterJournalAndResumesWhenParentBecomesEmpty(t *testing.T) {
	fixture, retirementPlan, retirementOperation, parent := completedNestedRetirementFixture(t)
	preview, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, retirementOperation))
	if err != nil {
		t.Fatal(err)
	}
	inserted := filepath.Join(parent, "PRIVATE-LATE-ENTRY-CANARY")
	beforeParentCleanupRemovalHook = func(sequence int, observed string) error {
		if sequence == 0 && observed == parent {
			return os.WriteFile(inserted, []byte("late"), 0o600)
		}
		return nil
	}
	report, runErr := RunParentCleanup(context.Background(), parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, preview.Plan.ID))
	beforeParentCleanupRemovalHook = nil
	if runErr != nil || report.Outcome != ParentCleanupExecutionOutcomePartial || !report.Operation.Resumable || report.DeletionPerformed ||
		report.Writes.DirectoriesRemoved != 0 || report.Writes.RemovalAttempts != 0 || report.Used.DirectoryReads != 4 ||
		!hasFinding(report.Blockers, "parent.not_empty") {
		t.Fatalf("blocked run=%#v err=%v", report, runErr)
	}
	if _, err := os.Stat(inserted); err != nil {
		t.Fatalf("late entry was removed: %v", err)
	}
	if err := os.Remove(inserted); err != nil {
		t.Fatal(err)
	}
	operation, _ := ParseParentCleanupOperationID(report.Operation.ID)
	resumed, err := ResumeParentCleanup(context.Background(), ParentCleanupResumeOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedCleanupPlanID: preview.Plan.ID,
		SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultParentCleanupExecutionLimits(),
	})
	if err != nil || resumed.Outcome != ParentCleanupExecutionOutcomeRemoved || !resumed.DeletionPerformed ||
		resumed.Writes.DirectoriesRemoved != 1 || resumed.Directories[0].RemovalBasis != ParentCleanupRemovalBasisConfirmed {
		t.Fatalf("resume=%#v err=%v", resumed, err)
	}
}

func TestParentCleanupExecutionRecoversRemovalAfterDurableAttempt(t *testing.T) {
	fixture, retirementPlan, retirementOperation, parent := completedNestedRetirementFixture(t)
	preview, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, retirementOperation))
	if err != nil {
		t.Fatal(err)
	}
	stopErr := errors.New("simulated process stop")
	afterParentCleanupRemovalHook = func(sequence int, observed string) error {
		if sequence == 0 && observed == parent {
			return stopErr
		}
		return nil
	}
	report, runErr := RunParentCleanup(context.Background(), parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, preview.Plan.ID))
	afterParentCleanupRemovalHook = nil
	if !errors.Is(runErr, stopErr) || report.Outcome != ParentCleanupExecutionOutcomePartial || !report.DeletionPerformed ||
		!report.Operation.Resumable || report.Directories[0].Status != "attempt_recorded" {
		t.Fatalf("stopped run=%#v err=%v", report, runErr)
	}
	if _, err := os.Lstat(parent); !os.IsNotExist(err) {
		t.Fatalf("durably removed parent unexpectedly exists: %v", err)
	}
	operation, _ := ParseParentCleanupOperationID(report.Operation.ID)
	resumed, err := ResumeParentCleanup(context.Background(), ParentCleanupResumeOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedCleanupPlanID: preview.Plan.ID,
		SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultParentCleanupExecutionLimits(),
	})
	if err != nil || resumed.Outcome != ParentCleanupExecutionOutcomeRemoved || resumed.DeletionPerformed ||
		resumed.Directories[0].RemovalBasis != ParentCleanupRemovalBasisRecovered {
		t.Fatalf("recovered resume=%#v err=%v", resumed, err)
	}
}

func TestParentCleanupExecutionRecoversConcurrentAbsenceAfterDurableAttempt(t *testing.T) {
	fixture, retirementPlan, retirementOperation, parent := completedNestedRetirementFixture(t)
	preview, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, retirementOperation))
	if err != nil {
		t.Fatal(err)
	}
	beforeParentCleanupRemovalHook = func(sequence int, observed string) error {
		if sequence == 0 && observed == parent {
			return os.Remove(parent)
		}
		return nil
	}
	report, runErr := RunParentCleanup(context.Background(), parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, preview.Plan.ID))
	beforeParentCleanupRemovalHook = nil
	if !errors.Is(runErr, fsbind.ErrNotFound) || report.Outcome != ParentCleanupExecutionOutcomePartial || report.DeletionPerformed ||
		!report.Operation.Resumable || report.Directories[0].Status != "attempt_recorded" {
		t.Fatalf("concurrent absence run=%#v err=%v", report, runErr)
	}
	operation, _ := ParseParentCleanupOperationID(report.Operation.ID)
	resumed, err := ResumeParentCleanup(context.Background(), ParentCleanupResumeOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedCleanupPlanID: preview.Plan.ID,
		SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultParentCleanupExecutionLimits(),
	})
	if err != nil || resumed.Outcome != ParentCleanupExecutionOutcomeRemoved || resumed.DeletionPerformed ||
		resumed.Directories[0].RemovalBasis != ParentCleanupRemovalBasisRecovered {
		t.Fatalf("concurrent absence recovery=%#v err=%v", resumed, err)
	}
}

func TestParentCleanupHistoricalStatusDoesNotHideCurrentParentReappearance(t *testing.T) {
	fixture, retirementPlan, retirementOperation, parent := completedNestedRetirementFixture(t)
	preview, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, retirementOperation))
	if err != nil {
		t.Fatal(err)
	}
	report, err := RunParentCleanup(context.Background(), parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, preview.Plan.ID))
	if err != nil || report.Outcome != ParentCleanupExecutionOutcomeRemoved {
		t.Fatalf("run=%#v err=%v", report, err)
	}
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	operation, _ := ParseParentCleanupOperationID(report.Operation.ID)
	status, err := ParentCleanupStatus(context.Background(), ParentCleanupStatusOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, Limits: DefaultParentCleanupExecutionLimits(),
	})
	if err != nil || status.Outcome != ParentCleanupExecutionOutcomeAlreadyRemoved || status.Operation.Status != "historical_complete" {
		t.Fatalf("historical status=%#v err=%v", status, err)
	}
	rerun, err := RunParentCleanup(context.Background(), parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, preview.Plan.ID))
	if !errors.Is(err, ErrExecutionIntegrity) || rerun.Outcome != ParentCleanupExecutionOutcomeIntegrity || rerun.Operation.Resumable || rerun.DeletionPerformed {
		t.Fatalf("reappeared parent rerun=%#v err=%v", rerun, err)
	}
	if info, statErr := os.Stat(parent); statErr != nil || !info.IsDir() {
		t.Fatalf("reappeared parent was changed: %v", statErr)
	}
}

func TestParentCleanupJournalCannotBeMovedToAnotherTargetRoot(t *testing.T) {
	fixture, retirementPlan, retirementOperation, _ := completedNestedRetirementFixture(t)
	preview, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, retirementOperation))
	if err != nil {
		t.Fatal(err)
	}
	report, err := RunParentCleanup(context.Background(), parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, preview.Plan.ID))
	if err != nil || report.Outcome != ParentCleanupExecutionOutcomeRemoved {
		t.Fatalf("run=%#v err=%v", report, err)
	}
	operation, _ := ParseParentCleanupOperationID(report.Operation.ID)
	operationName, _ := ParentCleanupOperationDirectoryName(operation)
	originalTarget := fixture.targetRoot + "-original"
	if err := os.Rename(fixture.targetRoot, originalTarget); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fixture.targetRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(originalTarget, operationName), filepath.Join(fixture.targetRoot, operationName)); err != nil {
		t.Fatal(err)
	}
	status, err := ParentCleanupStatus(context.Background(), ParentCleanupStatusOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, Limits: DefaultParentCleanupExecutionLimits(),
	})
	if !errors.Is(err, ErrExecutionIntegrity) || status.Outcome != ParentCleanupExecutionOutcomeIntegrity || status.Operation.Resumable {
		t.Fatalf("moved journal status=%#v err=%v", status, err)
	}
}

func TestParentCleanupCompletedJournalRejectsRetainedScratchObject(t *testing.T) {
	fixture, retirementPlan, retirementOperation, _ := completedNestedRetirementFixture(t)
	preview, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, retirementOperation))
	if err != nil {
		t.Fatal(err)
	}
	report, err := RunParentCleanup(context.Background(), parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, preview.Plan.ID))
	if err != nil || report.Outcome != ParentCleanupExecutionOutcomeRemoved {
		t.Fatalf("run=%#v err=%v", report, err)
	}
	operation, _ := ParseParentCleanupOperationID(report.Operation.ID)
	operationName, _ := ParentCleanupOperationDirectoryName(operation)
	retainedScratch := filepath.Join(fixture.targetRoot, operationName, parentCleanupScratchDirectory, "unrelated.pending")
	if err := os.WriteFile(retainedScratch, []byte("PRIVATE-SCRATCH-CANARY"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := ParentCleanupStatus(context.Background(), ParentCleanupStatusOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, Limits: DefaultParentCleanupExecutionLimits(),
	})
	if !errors.Is(err, ErrExecutionIntegrity) || status.Outcome != ParentCleanupExecutionOutcomeIntegrity || status.Operation.Resumable {
		t.Fatalf("retained scratch status=%#v err=%v", status, err)
	}
}

func TestParentCleanupExecutionPlanMismatchAndCancellationAreZeroWrite(t *testing.T) {
	fixture, retirementPlan, retirementOperation, parent := completedNestedRetirementFixture(t)
	preview, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, retirementOperation))
	if err != nil {
		t.Fatal(err)
	}
	wrong := markerIDPrefix + strings.Repeat("0", 64)
	report, err := RunParentCleanup(context.Background(), parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, wrong))
	if err != nil || report.Outcome != ParentCleanupExecutionOutcomeBlocked || report.WritesPerformed != 0 || report.DeletionPerformed ||
		!hasFinding(report.Blockers, "plan.id_mismatch") {
		t.Fatalf("mismatch=%#v err=%v", report, err)
	}
	if _, err := os.Stat(parent); err != nil {
		t.Fatalf("mismatch changed parent: %v", err)
	}
	operation, _ := ParentCleanupOperationIDForPlanID(preview.Plan.ID)
	name, _ := ParentCleanupOperationDirectoryName(operation)
	if _, err := os.Lstat(filepath.Join(fixture.targetRoot, name)); !os.IsNotExist(err) {
		t.Fatalf("mismatch created an operation: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err = RunParentCleanup(ctx, parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, preview.Plan.ID))
	if !errors.Is(err, context.Canceled) || report.WritesPerformed != 0 || report.DeletionPerformed ||
		report.Outcome != ParentCleanupExecutionOutcomeIncomplete {
		t.Fatalf("cancelled=%#v err=%v", report, err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.targetRoot, name)); !os.IsNotExist(err) {
		t.Fatalf("cancelled run created an operation: %v", err)
	}
}

func TestParentCleanupExecutionPreflightBlocksChangedParentBeforeJournalWrite(t *testing.T) {
	fixture, retirementPlan, retirementOperation, parent := completedNestedRetirementFixture(t)
	preview, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, retirementOperation))
	if err != nil {
		t.Fatal(err)
	}
	late := filepath.Join(parent, "PRIVATE-PREFLIGHT-ENTRY-CANARY")
	afterParentCleanupReviewHook = func() error { return os.WriteFile(late, []byte("late"), 0o600) }
	defer func() { afterParentCleanupReviewHook = nil }()
	report, err := RunParentCleanup(context.Background(), parentCleanupRunOptions(fixture, retirementPlan, retirementOperation, preview.Plan.ID))
	if err != nil || report.Outcome != ParentCleanupExecutionOutcomeBlocked || report.WritesPerformed != 0 || report.DeletionPerformed ||
		!hasFinding(report.Blockers, "parent.not_empty") {
		t.Fatalf("preflight=%#v err=%v", report, err)
	}
	operation, _ := ParentCleanupOperationIDForPlanID(preview.Plan.ID)
	name, _ := ParentCleanupOperationDirectoryName(operation)
	if _, err := os.Lstat(filepath.Join(fixture.targetRoot, name)); !os.IsNotExist(err) {
		t.Fatalf("preflight failure created operation state: %v", err)
	}
	if raw, err := os.ReadFile(late); err != nil || string(raw) != "late" {
		t.Fatalf("preflight changed late entry: %q, %v", raw, err)
	}
}

func TestParentCleanupJournalRoundTripDoesNotCarryPlanAuthority(t *testing.T) {
	fixture, retirementPlan, retirementOperation, _ := completedNestedRetirementFixture(t)
	review, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, retirementOperation))
	if err != nil || review.authority == nil {
		t.Fatalf("review=%#v err=%v", review, err)
	}
	raw, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	var replay ParentCleanupReport
	if err := json.Unmarshal(raw, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.authority != nil || replay.Plan.Validate() != nil {
		t.Fatalf("serialized review recreated authority: %#v", replay)
	}
}

func TestParentCleanupExecutionProtocolIsCanonicalAndStrict(t *testing.T) {
	fixture, retirementPlan, retirementOperation, _ := completedNestedRetirementFixture(t)
	review, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, retirementOperation))
	if err != nil || review.authority == nil {
		t.Fatalf("review=%#v err=%v", review, err)
	}
	roots, _, err := normalizeExecutionRoots(context.Background(), []string{fixture.sourceRoot}, false)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := ParentCleanupOperationIDForPlanID(review.Plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := parentCleanupIntentFromAuthority(operation, review.authority, DefaultParentCleanupExecutionLimits(), roots)
	if err != nil {
		t.Fatal(err)
	}
	target, info, err := fsbind.BindExisting(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	intent.OperationRootIdentity = info.Identity.String()
	raw, id, err := encodeParentCleanupIntent(intent)
	if err != nil || id == "" {
		t.Fatalf("encode intent id=%q err=%v", id, err)
	}
	decoded, decodedID, err := decodeParentCleanupIntent(bytes.NewReader(raw), intent.Limits)
	if err != nil || decodedID != id || decoded.OperationID != intent.OperationID || decoded.Validate() != nil {
		t.Fatalf("decode intent=%#v id=%q err=%v", decoded, decodedID, err)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	object["unknown_canary"] = "PRIVATE-PROTOCOL-CANARY"
	unknown, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	unknown = append(unknown, '\n')
	if _, _, err := decodeParentCleanupIntent(bytes.NewReader(unknown), intent.Limits); !errors.Is(err, ErrExecutionIntegrity) {
		t.Fatalf("unknown intent field was accepted: %v", err)
	}
	if _, _, err := decodeParentCleanupIntent(bytes.NewReader(append(append([]byte(nil), raw...), []byte("{}")...)), intent.Limits); !errors.Is(err, ErrExecutionIntegrity) {
		t.Fatalf("trailing intent JSON was accepted: %v", err)
	}
	directoryName, err := ParentCleanupOperationDirectoryName(operation)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseParentCleanupOperationDirectoryName(directoryName)
	if err != nil || parsed != operation {
		t.Fatalf("operation directory round trip=%s %v", parsed, err)
	}
}
