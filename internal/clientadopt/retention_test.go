package clientadopt

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

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

type terminalAdoptionFixture struct {
	materialized materializedFixture
	prepared     *PreparedPlan
	before       downloader.LedgerSnapshot
	after        downloader.LedgerSnapshot
}

func makeTerminalAdoptionFixture(t *testing.T) terminalAdoptionFixture {
	t.Helper()
	ctx := context.Background()
	materialized := makeMaterializedFixture(t, ctx)
	prepared := prepareFixturePlan(t, materialized)
	savePath, _ := prepared.savePath()
	contentPath, _ := prepared.contentPath()
	before := ledgerSnapshot(materialized.meta, nil, time.Now().UTC())
	after := ledgerSnapshot(materialized.meta, &downloader.Torrent{
		Hash: "opaque-retention-job", InfoHashV1: materialized.meta.InfoHashV1, IdentityStatus: downloader.IdentityStatusValid,
		IdentityEvidence: []string{"magnet_xt_btih_hex"}, IdentityIssues: []string{}, SizeBytes: materialized.meta.TotalLength,
		State: "stoppedDL", SavePath: savePath, ContentPath: contentPath,
	}, before.ObservedAtEnd.Add(time.Millisecond))
	session := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{before, after}}
	report, err := Run(ctx, RunOptions{Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Metafile: materialized.payload(t),
		Session: session, AcknowledgeAdd: true})
	if err != nil || report.Outcome != OutcomeAdoptedPendingRecheck {
		t.Fatalf("terminal adoption report=%#v err=%v", report, err)
	}
	return terminalAdoptionFixture{materialized: materialized, prepared: prepared, before: before, after: after}
}

func (fixture terminalAdoptionFixture) pruneOptions() PruneOptions {
	return PruneOptions{TargetRoot: fixture.materialized.targetRoot, OperationID: fixture.prepared.OperationID(),
		ExpectedPlanID: fixture.prepared.PlanID(), Acknowledge: true, Limits: DefaultRetentionLimits()}
}

func TestPruneRetainsExactCompletionAuthorityAndIsIdempotent(t *testing.T) {
	fixture := makeTerminalAdoptionFixture(t)
	ctx := context.Background()
	before, observationBefore, err := VerifyCompletion(ctx, CompletionProofOptions{TargetRoot: fixture.materialized.targetRoot,
		OperationID: fixture.prepared.OperationID(), ExpectedPlanID: fixture.prepared.PlanID()})
	if err != nil || before == nil || !before.Verified() {
		t.Fatalf("pre-prune completion=%#v observation=%#v err=%v", before, observationBefore, err)
	}
	report, err := Prune(ctx, fixture.pruneOptions())
	if err != nil || report.Outcome != RetentionOutcomePruned || !report.Markers.ExactTombstone ||
		!report.Markers.IntentDurable || !report.Markers.CompletionDurable || report.WritesPerformed == 0 || report.Operation.Resumable {
		t.Fatalf("prune report=%#v err=%v", report, err)
	}
	status, err := Status(ctx, StatusOptions{TargetRoot: fixture.materialized.targetRoot, OperationID: fixture.prepared.OperationID()})
	if err != nil || status.Outcome != OutcomeHistoricalAdopted || status.Operation.Status != "retained" ||
		status.Operation.Resumable || status.Journal.RetentionState != "complete" || !status.Journal.RetentionCompletionPresent ||
		status.Journal.RetentionIntentDurable || status.Journal.RetentionCompletionDurable || status.WritesPerformed != 0 {
		t.Fatalf("retained status=%#v err=%v", status, err)
	}
	after, observationAfter, err := VerifyCompletion(ctx, CompletionProofOptions{TargetRoot: fixture.materialized.targetRoot,
		OperationID: fixture.prepared.OperationID(), ExpectedPlanID: fixture.prepared.PlanID()})
	if err != nil || after == nil || !after.Verified() || observationAfter.CompletionID != observationBefore.CompletionID ||
		!strings.Contains(observationAfter.Assurance, "retention_tombstone") {
		t.Fatalf("retained completion=%#v observation=%#v err=%v", after, observationAfter, err)
	}
	repeated, err := Prune(ctx, fixture.pruneOptions())
	if err != nil || repeated.Outcome != RetentionOutcomeAlreadyPruned || repeated.WritesPerformed != 0 || !repeated.Markers.ExactTombstone {
		t.Fatalf("repeated prune=%#v err=%v", repeated, err)
	}
	directory, _ := operationDirectoryName(fixture.prepared.OperationID())
	entries, err := os.ReadDir(filepath.Join(fixture.materialized.targetRoot, directory))
	if err != nil || len(entries) != 2 || entries[0].Name() != ".fsbind-operation.lock" || entries[1].Name() != retentionDirectoryName {
		t.Fatalf("retained namespace=%v err=%v", entries, err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{fixture.materialized.targetRoot, "opaque-retention-job", "renamed-source"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("retention report leaked %q: %s", secret, encoded)
		}
	}
}

func TestPruneCrashBoundariesRequireExplicitRecovery(t *testing.T) {
	for _, phase := range []string{"intent_published", "legacy_state_removed"} {
		t.Run(phase, func(t *testing.T) {
			fixture := makeTerminalAdoptionFixture(t)
			stop := errors.New("retention transition stopped")
			retentionTransitionHook = func(observed string) error {
				if observed == phase {
					return stop
				}
				return nil
			}
			t.Cleanup(func() { retentionTransitionHook = nil })
			report, err := Prune(context.Background(), fixture.pruneOptions())
			if !errors.Is(err, stop) || report.Outcome != RetentionOutcomeInterrupted || !report.Markers.IntentDurable || !report.Markers.PruneResumable {
				t.Fatalf("stopped prune=%#v err=%v", report, err)
			}
			status, statusErr := Status(context.Background(), StatusOptions{TargetRoot: fixture.materialized.targetRoot,
				OperationID: fixture.prepared.OperationID()})
			if statusErr != nil || status.Operation.Status != "pruning" || status.Operation.Resumable || status.WritesPerformed != 0 {
				t.Fatalf("pruning status=%#v err=%v", status, statusErr)
			}
			verified, observation, verifyErr := VerifyCompletion(context.Background(), CompletionProofOptions{TargetRoot: fixture.materialized.targetRoot,
				OperationID: fixture.prepared.OperationID(), ExpectedPlanID: fixture.prepared.PlanID()})
			if !errors.Is(verifyErr, ErrPolicy) || verified != nil || observation.CompletionID != "" {
				t.Fatalf("incomplete tombstone granted authority=%#v observation=%#v err=%v", verified, observation, verifyErr)
			}
			retentionTransitionHook = nil
			recovered, recoverErr := Prune(context.Background(), fixture.pruneOptions())
			if recoverErr != nil || recovered.Outcome != RetentionOutcomePruned || !recovered.Markers.ExactTombstone {
				t.Fatalf("recovered prune=%#v err=%v", recovered, recoverErr)
			}
		})
	}
}

func TestPruneRebindsRetentionIntentBeforeDeletingLegacyJournal(t *testing.T) {
	fixture := makeTerminalAdoptionFixture(t)
	directory, _ := operationDirectoryName(fixture.prepared.OperationID())
	operationRoot := filepath.Join(fixture.materialized.targetRoot, directory)
	intentPath := filepath.Join(operationRoot, retentionDirectoryName, retentionIntentName)
	retentionTransitionHook = func(observed string) error {
		if observed == "intent_published" {
			return os.WriteFile(intentPath, []byte("{}\n"), 0o600)
		}
		return nil
	}
	t.Cleanup(func() { retentionTransitionHook = nil })
	report, err := Prune(context.Background(), fixture.pruneOptions())
	if !errors.Is(err, ErrIntegrity) || report.Outcome != RetentionOutcomeIntegrityFailed || report.Operation.Resumable ||
		report.Markers.IntentDurable || report.Markers.PruneResumable {
		t.Fatalf("changed retention intent report=%#v err=%v", report, err)
	}
	for _, name := range []string{intentFileName, attemptFileName(1), completionFileName} {
		if _, statErr := os.Stat(filepath.Join(operationRoot, name)); statErr != nil {
			t.Fatalf("legacy marker %q was deleted before intent rebind: %v", name, statErr)
		}
	}
}

func TestRetentionIntentDecoderRejectsNonCanonicalInput(t *testing.T) {
	fixture := makeTerminalAdoptionFixture(t)
	if _, err := Prune(context.Background(), fixture.pruneOptions()); err != nil {
		t.Fatal(err)
	}
	directory, _ := operationDirectoryName(fixture.prepared.OperationID())
	raw, err := os.ReadFile(filepath.Join(fixture.materialized.targetRoot, directory, retentionDirectoryName, retentionIntentName))
	if err != nil {
		t.Fatal(err)
	}
	unknown := append([]byte(nil), raw[:len(raw)-2]...)
	unknown = append(unknown, []byte(",\"unknown\":true}\n")...)
	mutations := map[string][]byte{
		"duplicate_key": bytes.Replace(raw, []byte(`{"schema":`), []byte(`{"schema":"`+RetentionIntentSchemaV1+`","schema":`), 1),
		"unknown_field": unknown,
		"trailing_json": append(append([]byte(nil), raw...), []byte("{}\n")...),
		"over_budget":   bytes.Repeat([]byte(" "), int(maximumMarkerBytes)+1),
	}
	for name, input := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, _, decodeErr := DecodeRetentionIntent(bytes.NewReader(input)); decodeErr == nil {
				t.Fatalf("DecodeRetentionIntent accepted %s", name)
			}
		})
	}
}

func TestRetainedCompletionAuthorityRequiresExactOperationNamespace(t *testing.T) {
	fixture := makeTerminalAdoptionFixture(t)
	if _, err := Prune(context.Background(), fixture.pruneOptions()); err != nil {
		t.Fatal(err)
	}
	directory, _ := operationDirectoryName(fixture.prepared.OperationID())
	unexpected := filepath.Join(fixture.materialized.targetRoot, directory, "unexpected.bin")
	if err := os.WriteFile(unexpected, []byte("unexpected"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, statusErr := Status(context.Background(), StatusOptions{TargetRoot: fixture.materialized.targetRoot,
		OperationID: fixture.prepared.OperationID()})
	if !errors.Is(statusErr, ErrIntegrity) || status.Outcome != OutcomeIntegrityFailed || status.Operation.Resumable {
		t.Fatalf("unexpected retained namespace status=%#v err=%v", status, statusErr)
	}
	verified, observation, verifyErr := VerifyCompletion(context.Background(), CompletionProofOptions{
		TargetRoot: fixture.materialized.targetRoot, OperationID: fixture.prepared.OperationID(), ExpectedPlanID: fixture.prepared.PlanID()})
	if !errors.Is(verifyErr, ErrIntegrity) || verified != nil || observation.CompletionID != "" {
		t.Fatalf("unexpected namespace granted authority=%#v observation=%#v err=%v", verified, observation, verifyErr)
	}
}

func TestPrunePolicyAndCorruptionFailClosed(t *testing.T) {
	t.Run("policy-before-write", func(t *testing.T) {
		fixture := makeTerminalAdoptionFixture(t)
		for _, mutate := range []func(*PruneOptions){
			func(options *PruneOptions) { options.Acknowledge = false },
			func(options *PruneOptions) { options.ExpectedPlanID = strings.Repeat("f", 24) },
			func(options *PruneOptions) { options.Limits.MaxMarkerBytes = 1 },
		} {
			options := fixture.pruneOptions()
			mutate(&options)
			report, err := Prune(context.Background(), options)
			if !errors.Is(err, ErrPolicy) || report.Outcome != RetentionOutcomeBlocked || report.WritesPerformed != 0 {
				t.Fatalf("blocked prune=%#v err=%v", report, err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		report, err := Prune(ctx, fixture.pruneOptions())
		if !errors.Is(err, context.Canceled) || report.Outcome != RetentionOutcomeInterrupted || report.WritesPerformed != 0 {
			t.Fatalf("cancelled prune=%#v err=%v", report, err)
		}
	})

	t.Run("not-terminal", func(t *testing.T) {
		ctx := context.Background()
		fixture := makeMaterializedFixture(t, ctx)
		prepared := prepareFixturePlan(t, fixture)
		before := ledgerSnapshot(fixture.meta, nil, time.Now().UTC())
		session := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{before}, addErr: errors.New("transport lost")}
		_, _ = Run(ctx, RunOptions{Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Metafile: fixture.payload(t), Session: session, AcknowledgeAdd: true})
		report, err := Prune(ctx, PruneOptions{TargetRoot: fixture.targetRoot, OperationID: prepared.OperationID(),
			ExpectedPlanID: prepared.PlanID(), Acknowledge: true, Limits: DefaultRetentionLimits()})
		if !errors.Is(err, ErrPolicy) || report.Outcome != RetentionOutcomeBlocked || report.WritesPerformed != 0 {
			t.Fatalf("nonterminal prune=%#v err=%v", report, err)
		}
	})

	t.Run("corrupt-retained-marker", func(t *testing.T) {
		fixture := makeTerminalAdoptionFixture(t)
		if _, err := Prune(context.Background(), fixture.pruneOptions()); err != nil {
			t.Fatal(err)
		}
		directory, _ := operationDirectoryName(fixture.prepared.OperationID())
		path := filepath.Join(fixture.materialized.targetRoot, directory, retentionDirectoryName, retentionIntentName)
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		report, err := Prune(context.Background(), fixture.pruneOptions())
		if !errors.Is(err, ErrIntegrity) || report.Outcome != RetentionOutcomeIntegrityFailed || report.Operation.Resumable {
			t.Fatalf("corrupt prune=%#v err=%v", report, err)
		}
		status, statusErr := Status(context.Background(), StatusOptions{TargetRoot: fixture.materialized.targetRoot,
			OperationID: fixture.prepared.OperationID()})
		if !errors.Is(statusErr, ErrIntegrity) || status.Outcome != OutcomeIntegrityFailed || status.Operation.Resumable {
			t.Fatalf("corrupt status=%#v err=%v", status, statusErr)
		}
	})
}

func TestResumeDoesNotCrossRetainedAdoptionBoundary(t *testing.T) {
	fixture := makeTerminalAdoptionFixture(t)
	if _, err := Prune(context.Background(), fixture.pruneOptions()); err != nil {
		t.Fatal(err)
	}
	session := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{fixture.after}}
	report, err := Resume(context.Background(), fixture.prepared.OperationID(), RunOptions{Prepared: fixture.prepared,
		ExpectedPlanID: fixture.prepared.PlanID(), Session: session})
	if !errors.Is(err, ErrPolicy) || report.Outcome != OutcomeBlocked || report.Operation.Status != "retained" ||
		report.Operation.Resumable || session.requests != 1 || session.adds != 0 {
		t.Fatalf("retained resume=%#v requests=%d adds=%d err=%v", report, session.requests, session.adds, err)
	}
}
