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
	"time"

	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

func completedNestedRetirementFixture(t *testing.T) (retireFixture, Plan, OperationID, string) {
	t.Helper()
	fixture := makeRetireFixture(t)
	parent := filepath.Join(fixture.sourceRoot, "PRIVATE-EMPTY-PARENT-CANARY")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	nestedSource := filepath.Join(parent, filepath.Base(fixture.sourcePath))
	if err := os.Rename(fixture.sourcePath, nestedSource); err != nil {
		t.Fatal(err)
	}
	fixture.sourcePath = nestedSource
	discovery, err := seed.Discover(context.Background(), fixture.meta, seed.DiscoverOptions{
		SearchRoots: []string{fixture.sourceRoot}, InventoryLimits: storage.DefaultInventoryLimits(),
		MatchLimits: metafile.DefaultSourceMatchLimits(), TimeBudget: time.Minute, Strategy: materialize.StrategyCopy,
	})
	if err != nil || discovery.SourceOutcome != "verified_unique" {
		t.Fatalf("nested discovery=%#v err=%v", discovery, err)
	}
	fixture.discovery = discovery
	preview, err := Build(context.Background(), retireBuildOptions(fixture, fixture.meta, &fixture.discovery, false))
	if err != nil || preview.Outcome != OutcomeEligible {
		t.Fatalf("nested retirement preview=%#v err=%v", preview, err)
	}
	session := executionSession(fixture, 3)
	report, err := Run(context.Background(), RunOptions{Review: BuildOptions{
		Meta: fixture.meta, Discovery: &fixture.discovery, Final: fixture.final, Activation: fixture.activation,
		ClientUse: fixture.currentUse, ClientSession: session,
	}, ExpectedPlanID: preview.Plan.ID, SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultExecutionLimits()})
	if err != nil || report.Outcome != ExecutionOutcomeRetired {
		t.Fatalf("nested retirement=%#v err=%v", report, err)
	}
	operation, err := ParseOperationID(report.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(fixture.sourcePath); !os.IsNotExist(err) {
		t.Fatalf("nested source remains after retirement: %v", err)
	}
	return fixture, preview.Plan, operation, parent
}

func parentCleanupOptions(fixture retireFixture, plan Plan, operation OperationID) ParentCleanupOptions {
	return ParentCleanupOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: plan.ID,
		SearchRoots: []string{fixture.sourceRoot}, Limits: DefaultParentCleanupLimits(),
		RetirementLimits: DefaultExecutionLimits(),
	}
}

func TestParentCleanupPlanFindsOnlyStableEmptyImmediateParentAndRemainsZeroWrite(t *testing.T) {
	fixture, retirementPlan, operation, parent := completedNestedRetirementFixture(t)
	options := parentCleanupOptions(fixture, retirementPlan, operation)
	report, err := BuildParentCleanupPlan(context.Background(), options)
	if err != nil || report.Outcome != ParentCleanupOutcomeEligible || report.WritesPerformed != 0 || report.DeletionPerformed ||
		report.Plan.CleanupAuthority != "none" || report.Plan.CandidateParents != 1 || report.Plan.ParentsConsidered != 1 ||
		len(report.Plan.Directories) != 1 || report.Plan.Directories[0].Status != "empty_stable_candidate" ||
		report.Plan.Directories[0].ObservationPasses != 2 || report.Used.DirectoryReads != 2 || report.Plan.ID == "" ||
		report.Plan.Validate() != nil || report.ParentObservedAtStart.IsZero() || report.ParentObservedAtEnd.Before(report.ParentObservedAtStart) {
		t.Fatalf("cleanup plan=%#v err=%v", report, err)
	}
	if report.Plan.Directories[0].ParentPath != "" {
		t.Fatalf("default report exposed parent path: %#v", report.Plan.Directories[0])
	}
	if info, err := os.Stat(parent); err != nil || !info.IsDir() {
		t.Fatalf("zero-write cleanup plan changed parent: %v", err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{parent, filepath.Base(parent), fixture.sourcePath} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("cleanup plan leaked %q: %s", secret, raw)
		}
	}

	options.ShowAbsolutePaths = true
	shown, err := BuildParentCleanupPlan(context.Background(), options)
	if err != nil || shown.Outcome != ParentCleanupOutcomeEligible || shown.Plan.ID != report.Plan.ID ||
		shown.Plan.Directories[0].ParentPath != parent || shown.Plan.Validate() != nil {
		t.Fatalf("shown cleanup plan=%#v err=%v", shown, err)
	}
}

func TestParentCleanupPlanRetainsNonemptyParentWithoutDisclosingEntry(t *testing.T) {
	fixture, retirementPlan, operation, parent := completedNestedRetirementFixture(t)
	entry := filepath.Join(parent, "PRIVATE-UNRELATED-ENTRY-CANARY")
	if err := os.WriteFile(entry, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := parentCleanupOptions(fixture, retirementPlan, operation)
	options.Limits.MaxEntryNameBytes = 1
	report, err := BuildParentCleanupPlan(context.Background(), options)
	if err != nil || report.Outcome != ParentCleanupOutcomeNothingToClean || report.Plan.CandidateParents != 0 ||
		report.Plan.RetainedParents != 1 || len(report.Plan.Directories) != 1 ||
		report.Plan.Directories[0].Status != "retained_nonempty" || report.Plan.Directories[0].EntriesObserved <= 0 ||
		report.Used.DirectoryReads != 1 || report.Used.EntryNameBytes <= report.Limits.MaxEntryNameBytes || report.Plan.Validate() != nil {
		t.Fatalf("nonempty cleanup plan=%#v err=%v", report, err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(filepath.Base(entry))) || bytes.Contains(raw, []byte(entry)) {
		t.Fatalf("cleanup plan leaked an unrelated entry: %s", raw)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("cleanup plan changed unrelated entry: %v", err)
	}
}

func TestParentCleanupPlanProtectsSearchRootAndRequiresLiveJournal(t *testing.T) {
	fixture, retirementPlan, operation := completedRetirementFixture(t)
	options := parentCleanupOptions(fixture, retirementPlan, operation)
	report, err := BuildParentCleanupPlan(context.Background(), options)
	if err != nil || report.Outcome != ParentCleanupOutcomeNothingToClean || report.Plan.ProtectedRoots != 1 ||
		len(report.Plan.Directories) != 1 || report.Plan.Directories[0].Status != "protected_search_root" ||
		report.Used.DirectoryReads != 0 || report.Plan.Validate() != nil {
		t.Fatalf("protected-root cleanup plan=%#v err=%v", report, err)
	}
	if _, err := os.Stat(fixture.sourceRoot); err != nil {
		t.Fatalf("cleanup plan changed protected search root: %v", err)
	}

	pruned, err := PruneExecution(context.Background(), ExecutionPruneOptions{
		TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: retirementPlan.ID, Acknowledge: true,
		JournalLimits: DefaultExecutionLimits(), RetentionLimits: DefaultExecutionRetentionLimits(),
	})
	if err != nil || pruned.Outcome != ExecutionRetentionOutcomePruned {
		t.Fatalf("prune=%#v err=%v", pruned, err)
	}
	report, err = BuildParentCleanupPlan(context.Background(), options)
	if err != nil || report.Outcome != ParentCleanupOutcomeBlocked || report.Plan.ID != "" ||
		!hasFinding(report.Blockers, "cleanup.live_path_authority_unavailable") || report.Used.DirectoryReads != 0 {
		t.Fatalf("retained cleanup plan=%#v err=%v", report, err)
	}
}

func TestParentCleanupPlanFailsClosedOnBudgetNameReappearanceAndParentSwap(t *testing.T) {
	t.Run("budget before absence scan", func(t *testing.T) {
		fixture, retirementPlan, operation, parent := completedNestedRetirementFixture(t)
		options := parentCleanupOptions(fixture, retirementPlan, operation)
		options.Limits.MaxPathBytes = 1
		report, err := BuildParentCleanupPlan(context.Background(), options)
		if err != nil || report.Outcome != ParentCleanupOutcomeIncomplete || report.Plan.ID != "" ||
			!hasFinding(report.Blockers, "cleanup.parent_budget_exhausted") || report.Used.DirectoryReads != 0 ||
			containsString(report.Effect, "read_retired_source_name_absence") {
			t.Fatalf("budget cleanup plan=%#v err=%v", report, err)
		}
		if _, err := os.Stat(parent); err != nil {
			t.Fatalf("budget failure changed parent: %v", err)
		}
	})

	t.Run("retired name reappeared", func(t *testing.T) {
		fixture, retirementPlan, operation, _ := completedNestedRetirementFixture(t)
		if err := os.WriteFile(fixture.sourcePath, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		report, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, operation))
		if err != nil || report.Outcome != ParentCleanupOutcomeBlocked || report.Plan.ID != "" ||
			!hasFinding(report.Blockers, "cleanup.retired_name_present") || report.Used.DirectoryReads != 0 {
			t.Fatalf("reappeared cleanup plan=%#v err=%v", report, err)
		}
	})

	t.Run("parent identity changed between empty observations", func(t *testing.T) {
		fixture, retirementPlan, operation, parent := completedNestedRetirementFixture(t)
		oldParent := parent + "-old"
		mutated := false
		parentCleanupObservationHook = func(stage, observed string) {
			if stage != "before_second_observation" || observed != parent || mutated {
				return
			}
			mutated = true
			if err := os.Rename(parent, oldParent); err != nil {
				t.Fatalf("rename cleanup parent: %v", err)
			}
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatalf("replace cleanup parent: %v", err)
			}
		}
		defer func() { parentCleanupObservationHook = nil }()
		report, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, operation))
		if err == nil || report.Outcome != ParentCleanupOutcomeIncomplete || report.Plan.ID != "" ||
			len(report.Plan.Directories) != 1 || report.Plan.Directories[0].Status != "unstable" ||
			!hasFinding(report.Issues, "cleanup.parent_unstable") {
			t.Fatalf("swapped cleanup plan=%#v err=%v", report, err)
		}
	})
}

func TestParentCleanupPlanPreCancelledAndPublicPlanHasNoAuthority(t *testing.T) {
	fixture, retirementPlan, operation, _ := completedNestedRetirementFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := BuildParentCleanupPlan(ctx, parentCleanupOptions(fixture, retirementPlan, operation))
	if !errors.Is(err, context.Canceled) || report.Outcome != ParentCleanupOutcomeIncomplete || report.WritesPerformed != 0 ||
		len(report.Effect) != 0 || !hasFinding(report.Issues, "cleanup.context_cancelled") {
		t.Fatalf("cancelled cleanup plan=%#v err=%v", report, err)
	}

	valid, err := BuildParentCleanupPlan(context.Background(), parentCleanupOptions(fixture, retirementPlan, operation))
	if err != nil || valid.Plan.Validate() != nil {
		t.Fatalf("valid cleanup plan=%#v err=%v", valid, err)
	}
	raw, err := json.Marshal(valid.Plan)
	if err != nil {
		t.Fatal(err)
	}
	var replay ParentCleanupPlan
	if err := json.Unmarshal(raw, &replay); err != nil || replay.Validate() != nil {
		t.Fatalf("public plan should remain valid review data: %#v err=%v", replay, err)
	}
	if replay.CleanupAuthority != "none" || strings.Contains(string(raw), fixture.sourceRoot) {
		t.Fatalf("serialized cleanup plan gained authority or leaked a path: %s", raw)
	}
	replay.CandidateParents, replay.RetainedParents = 0, 1
	replay.ID, err = parentCleanupPlanID(replay)
	if err != nil {
		t.Fatal(err)
	}
	if err := replay.Validate(); err == nil {
		t.Fatal("parent-cleanup plan accepted counters that disagree with directory statuses")
	}
}

func TestParentCleanupPlanCancellationDuringParentObservationIsAttributed(t *testing.T) {
	fixture, retirementPlan, operation, parent := completedNestedRetirementFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	parentCleanupObservationHook = func(stage, observed string) {
		if stage == "before_second_observation" && observed == parent {
			cancel()
		}
	}
	defer func() { parentCleanupObservationHook = nil }()
	report, err := BuildParentCleanupPlan(ctx, parentCleanupOptions(fixture, retirementPlan, operation))
	if !errors.Is(err, context.Canceled) || report.Outcome != ParentCleanupOutcomeIncomplete || report.Plan.ID != "" ||
		len(report.Plan.Directories) != 1 || report.Plan.Directories[0].Status != "observation_incomplete" ||
		!hasFinding(report.Issues, "cleanup.context_cancelled") || hasFinding(report.Issues, "cleanup.parent_unstable") {
		t.Fatalf("mid-observation cancellation plan=%#v err=%v", report, err)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
