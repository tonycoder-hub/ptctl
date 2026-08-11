package sourceretire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

func executionSession(fixture retireFixture, count int) *retireActivationSession {
	now := time.Now().UTC()
	ledgers := make([]downloader.LedgerSnapshot, count)
	for index := range ledgers {
		job := fixture.currentJob
		ledgers[index] = retireLedger(&job, now.Add(time.Duration(index)*time.Second))
	}
	return &retireActivationSession{requests: 1, ledgers: ledgers}
}

func TestRunRetiresOneExactSourceAndStatusIsHistorical(t *testing.T) {
	fixture := makeRetireFixture(t)
	previewOptions := retireBuildOptions(fixture, fixture.meta, &fixture.discovery, false)
	preview, err := Build(context.Background(), previewOptions)
	if err != nil || preview.Outcome != OutcomeEligible {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	roots, scopeID, err := normalizeExecutionRoots(context.Background(), []string{fixture.sourceRoot}, false)
	if err != nil {
		t.Fatal(err)
	}
	bound, intentFiles, err := bindRunSources(context.Background(), fixture.meta, &fixture.discovery, fixture.final, preview.Plan, roots)
	if err != nil {
		t.Fatal(err)
	}
	bound.close()
	target, targetInfo, err := fsbind.BindExisting(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	_ = target.Close()
	derivedOperation, _ := executionOperationID(preview.Plan.ID)
	intent := executionIntentFromPlan(derivedOperation, scopeID, preview.Plan, DefaultExecutionLimits(), intentFiles)
	intent.OperationRootIdentity = targetInfo.Identity.String()
	if err := intent.Validate(); err != nil {
		t.Fatalf("pre-write intent validation: %#v: %v", intent, err)
	}
	session := executionSession(fixture, 3)
	runOptions := RunOptions{Review: BuildOptions{Meta: fixture.meta, Discovery: &fixture.discovery, Final: fixture.final,
		Activation: fixture.activation, ClientUse: fixture.currentUse, ClientSession: session, ShowAbsolutePaths: true},
		ExpectedPlanID: preview.Plan.ID, SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultExecutionLimits()}
	report, err := Run(context.Background(), runOptions)
	if err != nil || report.Outcome != ExecutionOutcomeRetired || !report.DeletionPerformed || report.Writes.NamesRemoved != 1 ||
		report.Writes.BytesRemoved != fixture.meta.TotalLength || report.Operation.Phase != "complete" || report.Operation.Resumable ||
		len(report.Files) != 1 || report.Files[0].Status != "retired" || session.RequestsMade() != 4 {
		t.Fatalf("run=%#v requests=%d err=%v", report, session.RequestsMade(), err)
	}
	if _, err := os.Lstat(fixture.sourcePath); !os.IsNotExist(err) {
		t.Fatalf("source name remains after retirement: %v", err)
	}
	finalPath, _, ok := fixture.final.ProcessFilePath(0)
	if !ok {
		t.Fatal("final path unavailable")
	}
	if raw, err := os.ReadFile(finalPath); err != nil || string(raw) != "retirement source fixture" {
		t.Fatalf("published final changed: %q %v", raw, err)
	}
	rawReport, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawReport, []byte(fixture.sourcePath)) || bytes.Contains(rawReport, []byte(filepath.Base(fixture.sourcePath))) {
		t.Fatalf("execution report leaked a source path: %s", rawReport)
	}
	operation, err := ParseOperationID(report.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	status, err := Status(context.Background(), StatusOptions{TargetRoot: fixture.targetRoot, OperationID: operation, Limits: DefaultExecutionLimits()})
	if err != nil || status.Outcome != ExecutionOutcomeAlreadyRetired || status.Operation.Status != "historical_complete" || status.WritesPerformed != 0 {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	resumed, err := Resume(context.Background(), ResumeOptions{Meta: fixture.meta, Final: fixture.final, Activation: fixture.activation,
		ClientUse: fixture.currentUse, TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: preview.Plan.ID,
		SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultExecutionLimits()})
	if err != nil || resumed.Outcome != ExecutionOutcomeAlreadyRetired || resumed.WritesPerformed != 0 {
		t.Fatalf("idempotent resume=%#v err=%v", resumed, err)
	}
	if err := os.WriteFile(finalPath, bytes.Repeat([]byte{'x'}, int(fixture.meta.TotalLength)), 0o600); err != nil {
		t.Fatal(err)
	}
	resumed, err = Resume(context.Background(), ResumeOptions{Meta: fixture.meta, Final: fixture.final, Activation: fixture.activation,
		ClientUse: fixture.currentUse, TargetRoot: fixture.targetRoot, OperationID: operation, ExpectedPlanID: preview.Plan.ID,
		SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultExecutionLimits()})
	if err == nil || !errors.Is(err, ErrExecutionIntegrity) || resumed.Outcome != ExecutionOutcomeIntegrity || resumed.WritesPerformed != 0 {
		t.Fatalf("terminal resume after final mutation=%#v err=%v", resumed, err)
	}
}

func TestNormalizeExecutionRootsRejectsUnacknowledgedWindowsNetworkPathBeforeAccess(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows UNC policy")
	}
	if _, _, err := normalizeExecutionRoots(context.Background(), []string{`\\PTCTL-NETWORK-CANARY\share`}, false); !errors.Is(err, ErrExecutionPolicy) {
		t.Fatalf("unacknowledged UNC root was not rejected locally: %v", err)
	}
}

func TestRunPlanMismatchAndMissingAcknowledgementAreZeroWrite(t *testing.T) {
	for _, test := range []struct {
		name string
		ack  bool
		plan string
	}{
		{name: "missing acknowledgement", ack: false},
		{name: "plan mismatch", ack: true, plan: "sha256:" + string(bytes.Repeat([]byte{'a'}, 64))},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := makeRetireFixture(t)
			preview, err := Build(context.Background(), retireBuildOptions(fixture, fixture.meta, &fixture.discovery, false))
			if err != nil {
				t.Fatal(err)
			}
			expected := preview.Plan.ID
			if test.plan != "" {
				expected = test.plan
			}
			session := executionSession(fixture, 3)
			report, err := Run(context.Background(), RunOptions{Review: BuildOptions{Meta: fixture.meta, Discovery: &fixture.discovery,
				Final: fixture.final, Activation: fixture.activation, ClientUse: fixture.currentUse, ClientSession: session},
				ExpectedPlanID: expected, SearchRoots: []string{fixture.sourceRoot}, Acknowledge: test.ack, Limits: DefaultExecutionLimits()})
			if err != nil || report.Outcome != ExecutionOutcomeBlocked || report.WritesPerformed != 0 || report.DeletionPerformed {
				t.Fatalf("report=%#v err=%v", report, err)
			}
			if _, err := os.Stat(fixture.sourcePath); err != nil {
				t.Fatalf("zero-write blocker changed source: %v", err)
			}
			entries, err := os.ReadDir(fixture.targetRoot)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if len(entry.Name()) >= len(".ptctl-source-retire-") && entry.Name()[:len(".ptctl-source-retire-")] == ".ptctl-source-retire-" {
					t.Fatalf("zero-write blocker created %q", entry.Name())
				}
			}
		})
	}
}

func TestRunProtocolBudgetsFailBeforeJournalWrite(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ExecutionLimits)
	}{
		{name: "intent", mutate: func(limits *ExecutionLimits) { limits.MaxIntentBytes = 1 }},
		{name: "markers", mutate: func(limits *ExecutionLimits) { limits.MaxMarkerBytes = 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := makeRetireFixture(t)
			preview, err := Build(context.Background(), retireBuildOptions(fixture, fixture.meta, &fixture.discovery, false))
			if err != nil {
				t.Fatal(err)
			}
			limits := DefaultExecutionLimits()
			test.mutate(&limits)
			session := executionSession(fixture, 3)
			report, err := Run(context.Background(), RunOptions{Review: BuildOptions{Meta: fixture.meta, Discovery: &fixture.discovery,
				Final: fixture.final, Activation: fixture.activation, ClientUse: fixture.currentUse, ClientSession: session},
				ExpectedPlanID: preview.Plan.ID, SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: limits})
			if err != nil || report.Outcome != ExecutionOutcomeBlocked || report.WritesPerformed != 0 || report.DeletionPerformed {
				t.Fatalf("report=%#v err=%v", report, err)
			}
			assertNoRetireOperation(t, fixture.targetRoot)
		})
	}
}

func TestExecutionPreflightIncludesRecoveredDeletionMarker(t *testing.T) {
	fixture := makeRetireFixture(t)
	preview, err := Build(context.Background(), retireBuildOptions(fixture, fixture.meta, &fixture.discovery, false))
	if err != nil || preview.Outcome != OutcomeEligible {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	roots, scopeID, err := normalizeExecutionRoots(context.Background(), []string{fixture.sourceRoot}, false)
	if err != nil {
		t.Fatal(err)
	}
	sources, intentFiles, err := bindRunSources(context.Background(), fixture.meta, &fixture.discovery, fixture.final, preview.Plan, roots)
	if err != nil {
		t.Fatal(err)
	}
	sources.close()
	target, targetInfo, err := fsbind.BindExisting(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	_ = target.Close()
	operation, err := executionOperationID(preview.Plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	intent := executionIntentFromPlan(operation, scopeID, preview.Plan, DefaultExecutionLimits(), intentFiles)
	intent.OperationRootIdentity = targetInfo.Identity.String()
	_, intentID, err := encodeExecutionIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	file := intent.Files[0]
	attempt := DeleteAttempt{Schema: DeleteAttemptSchemaV1, OperationID: intent.OperationID, IntentID: intentID,
		Sequence: 0, ManifestIndex: file.ManifestIndex, SourcePathRef: file.SourcePathRef,
		FileIdentity: file.SourceObjectIdentity, SizeBytes: file.SizeBytes}
	_, attemptID, err := encodeDeleteAttempt(attempt, intent.Limits)
	if err != nil {
		t.Fatal(err)
	}
	marker := func(basis string) []byte {
		raw, _, encodeErr := encodeDeleteComplete(DeleteComplete{Schema: DeleteCompleteSchemaV1, OperationID: intent.OperationID,
			IntentID: intentID, AttemptID: attemptID, Sequence: 0, ManifestIndex: file.ManifestIndex,
			SourcePathRef: file.SourcePathRef, FileIdentity: file.SourceObjectIdentity, SizeBytes: file.SizeBytes, Basis: basis}, intent.Limits)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		return raw
	}
	confirmed, recovered := marker(DeleteBasisConfirmed), marker(DeleteBasisRecovered)
	if len(recovered) <= len(confirmed) {
		t.Fatalf("recovered marker is not the larger protocol shape: %d <= %d", len(recovered), len(confirmed))
	}
	intent.Limits.MaxMarkerBytes = int64(len(recovered) - 1)
	if err := preflightExecutionJournalEncoding(intent, targetInfo.Identity.String()); !errors.Is(err, ErrExecutionPolicy) {
		t.Fatalf("preflight accepted a budget that cannot encode recovery: %v", err)
	}
}

func TestResumeRecoversNameAbsentAfterDurableAttempt(t *testing.T) {
	fixture := makeRetireFixture(t)
	preview, err := Build(context.Background(), retireBuildOptions(fixture, fixture.meta, &fixture.discovery, false))
	if err != nil || preview.Outcome != OutcomeEligible {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	operation := createAttemptedRetirement(t, fixture, preview.Plan, true)
	if _, err := os.Lstat(fixture.sourcePath); !os.IsNotExist(err) {
		t.Fatalf("source was not removed in crash window: %v", err)
	}

	session := executionSession(fixture, 2)
	report, err := Resume(context.Background(), ResumeOptions{Meta: fixture.meta, Final: fixture.final, Activation: fixture.activation,
		ClientUse: fixture.currentUse, ClientSession: session, TargetRoot: fixture.targetRoot, OperationID: operation,
		ExpectedPlanID: preview.Plan.ID, SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultExecutionLimits()})
	if err != nil || report.Outcome != ExecutionOutcomeRetired || report.Writes.NamesRemoved != 0 || report.Operation.Phase != "complete" ||
		len(report.Files) != 1 || report.Files[0].Basis != DeleteBasisRecovered || session.RequestsMade() != 3 {
		t.Fatalf("resume=%#v requests=%d err=%v", report, session.RequestsMade(), err)
	}
}

func TestResumeRejectsContentChangeAfterDurableAttempt(t *testing.T) {
	fixture := makeRetireFixture(t)
	preview, err := Build(context.Background(), retireBuildOptions(fixture, fixture.meta, &fixture.discovery, false))
	if err != nil {
		t.Fatal(err)
	}
	operation := createAttemptedRetirement(t, fixture, preview.Plan, false)
	if err := os.WriteFile(fixture.sourcePath, bytes.Repeat([]byte{'x'}, int(fixture.meta.TotalLength)), 0o600); err != nil {
		t.Fatal(err)
	}
	session := executionSession(fixture, 2)
	report, err := Resume(context.Background(), ResumeOptions{Meta: fixture.meta, Final: fixture.final, Activation: fixture.activation,
		ClientUse: fixture.currentUse, ClientSession: session, TargetRoot: fixture.targetRoot, OperationID: operation,
		ExpectedPlanID: preview.Plan.ID, SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultExecutionLimits()})
	if err == nil || !errors.Is(err, ErrExecutionIntegrity) || report.Outcome != ExecutionOutcomeIntegrity || report.Writes.NamesRemoved != 0 {
		t.Fatalf("resume=%#v err=%v", report, err)
	}
	if _, err := os.Stat(fixture.sourcePath); err != nil {
		t.Fatalf("changed source was removed: %v", err)
	}
}

func TestRunRetiresOnlySelectedHardlinkName(t *testing.T) {
	fixture := makeRetireFixture(t)
	preview, err := Build(context.Background(), retireBuildOptions(fixture, fixture.meta, &fixture.discovery, false))
	if err != nil || preview.Outcome != OutcomeEligible {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	alias := filepath.Join(filepath.Dir(fixture.sourceRoot), "retained-hardlink.bin")
	if err := os.Link(fixture.sourcePath, alias); err != nil {
		t.Skipf("hardlinks are unavailable: %v", err)
	}
	session := executionSession(fixture, 3)
	report, err := Run(context.Background(), RunOptions{Review: BuildOptions{Meta: fixture.meta, Discovery: &fixture.discovery,
		Final: fixture.final, Activation: fixture.activation, ClientUse: fixture.currentUse, ClientSession: session},
		ExpectedPlanID: preview.Plan.ID, SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultExecutionLimits()})
	if err != nil || report.Outcome != ExecutionOutcomeRetired || report.Writes.NamesRemoved != 1 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	if _, err := os.Lstat(fixture.sourcePath); !os.IsNotExist(err) {
		t.Fatalf("selected source name remains: %v", err)
	}
	if raw, err := os.ReadFile(alias); err != nil || string(raw) != "retirement source fixture" {
		t.Fatalf("unselected hardlink changed: %q %v", raw, err)
	}
}

func TestRunRejectsReplacedSourceBeforeJournalWrite(t *testing.T) {
	fixture := makeRetireFixture(t)
	preview, err := Build(context.Background(), retireBuildOptions(fixture, fixture.meta, &fixture.discovery, false))
	if err != nil || preview.Outcome != OutcomeEligible {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	replacement := fixture.sourcePath + ".replacement"
	if err := os.WriteFile(replacement, []byte("retirement source fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, fixture.sourcePath); err != nil {
		t.Fatal(err)
	}
	session := executionSession(fixture, 3)
	report, err := Run(context.Background(), RunOptions{Review: BuildOptions{Meta: fixture.meta, Discovery: &fixture.discovery,
		Final: fixture.final, Activation: fixture.activation, ClientUse: fixture.currentUse, ClientSession: session},
		ExpectedPlanID: preview.Plan.ID, SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultExecutionLimits()})
	if err == nil || report.WritesPerformed != 0 || report.DeletionPerformed || report.Outcome != ExecutionOutcomeIntegrity {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	assertNoRetireOperation(t, fixture.targetRoot)
}

func TestRunRejectsTargetRootReplacementBeforeSourceRemoval(t *testing.T) {
	fixture := makeRetireFixture(t)
	preview, err := Build(context.Background(), retireBuildOptions(fixture, fixture.meta, &fixture.discovery, false))
	if err != nil || preview.Outcome != OutcomeEligible {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	moved := fixture.targetRoot + ".moved"
	var hookErr error
	beforeSourceRemovalHook = func(int) error {
		hookErr = os.Rename(fixture.targetRoot, moved)
		if hookErr != nil {
			return hookErr
		}
		hookErr = os.Mkdir(fixture.targetRoot, 0o700)
		return hookErr
	}
	t.Cleanup(func() { beforeSourceRemovalHook = nil })
	session := executionSession(fixture, 3)
	report, runErr := Run(context.Background(), RunOptions{Review: BuildOptions{Meta: fixture.meta, Discovery: &fixture.discovery,
		Final: fixture.final, Activation: fixture.activation, ClientUse: fixture.currentUse, ClientSession: session},
		ExpectedPlanID: preview.Plan.ID, SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultExecutionLimits()})
	if hookErr != nil {
		t.Skipf("target-root replacement is unavailable on this platform: %v", hookErr)
	}
	if runErr == nil || !errors.Is(runErr, ErrExecutionIntegrity) || report.Outcome != ExecutionOutcomeIntegrity || report.Writes.NamesRemoved != 0 {
		t.Fatalf("run after target replacement=%#v err=%v", report, runErr)
	}
	if _, err := os.Stat(fixture.sourcePath); err != nil {
		t.Fatalf("source was removed after target-root replacement: %v", err)
	}
}

func TestStatusRejectsCorruptIntentWithoutPathDisclosure(t *testing.T) {
	fixture := makeRetireFixture(t)
	preview, err := Build(context.Background(), retireBuildOptions(fixture, fixture.meta, &fixture.discovery, false))
	if err != nil {
		t.Fatal(err)
	}
	session := executionSession(fixture, 3)
	report, err := Run(context.Background(), RunOptions{Review: BuildOptions{Meta: fixture.meta, Discovery: &fixture.discovery,
		Final: fixture.final, Activation: fixture.activation, ClientUse: fixture.currentUse, ClientSession: session},
		ExpectedPlanID: preview.Plan.ID, SearchRoots: []string{fixture.sourceRoot}, Acknowledge: true, Limits: DefaultExecutionLimits()})
	if err != nil {
		t.Fatal(err)
	}
	operation, _ := ParseOperationID(report.Operation.ID)
	directory, _ := OperationDirectoryName(operation)
	intentPath := filepath.Join(fixture.targetRoot, directory, executionJournalDirectory, executionIntentFile)
	if err := os.WriteFile(intentPath, []byte("{\"corrupt\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, statusErr := Status(context.Background(), StatusOptions{TargetRoot: fixture.targetRoot, OperationID: operation, Limits: DefaultExecutionLimits()})
	if statusErr == nil || !errors.Is(statusErr, ErrExecutionIntegrity) || status.Outcome != ExecutionOutcomeIntegrity || status.Operation.Status != "inspection_incomplete" {
		t.Fatalf("status=%#v err=%v", status, statusErr)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(fixture.sourceRoot)) || bytes.Contains(raw, []byte(filepath.Base(fixture.sourcePath))) {
		t.Fatalf("corrupt status leaked private path data: %s", raw)
	}
}

func mustExecutionIdentity(t *testing.T, value string) fsbind.Identity {
	t.Helper()
	identity, err := fsbind.ParseIdentity(value)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func createAttemptedRetirement(t *testing.T, fixture retireFixture, plan Plan, remove bool) OperationID {
	t.Helper()
	roots, scopeID, err := normalizeExecutionRoots(context.Background(), []string{fixture.sourceRoot}, false)
	if err != nil {
		t.Fatal(err)
	}
	sources, intentFiles, err := bindRunSources(context.Background(), fixture.meta, &fixture.discovery, fixture.final, plan, roots)
	if err != nil {
		t.Fatal(err)
	}
	defer sources.close()
	target, targetInfo, err := fsbind.BindExisting(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	operation, err := executionOperationID(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	intent := executionIntentFromPlan(operation, scopeID, plan, DefaultExecutionLimits(), intentFiles)
	intent.OperationRootIdentity = targetInfo.Identity.String()
	journal, _, _, _, err := initializeExecutionJournal(context.Background(), target, intent)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.close()
	if _, _, err := journal.publishAttempt(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if remove {
		file := journal.state.Intent.Files[0]
		removal, err := sources.files[0].session.RemoveRootRegularExact(context.Background(), file.Name, mustExecutionIdentity(t, file.SourceObjectIdentity), file.SizeBytes)
		if err != nil || !removal.Removed || removal.Durability != fsbind.DurabilityConfirmed {
			t.Fatalf("removal=%#v err=%v", removal, err)
		}
	}
	return operation
}

func assertNoRetireOperation(t *testing.T, targetRoot string) {
	t.Helper()
	entries, err := os.ReadDir(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), materialize.SourceRetireOperationDirectoryPrefix) {
			t.Fatalf("zero-write blocker created %q", entry.Name())
		}
	}
}

func TestExecutionFormatRejectsNonCanonicalAndWrongIdentity(t *testing.T) {
	limits := DefaultExecutionLimits()
	marker := DeleteAttempt{Schema: DeleteAttemptSchemaV1, OperationID: OperationID("sha256:" + string(bytes.Repeat([]byte{'1'}, 64))),
		IntentID: "sha256:" + string(bytes.Repeat([]byte{'2'}, 64)), Sequence: 0, ManifestIndex: 0,
		SourcePathRef: "sha256:" + string(bytes.Repeat([]byte{'3'}, 64)), FileIdentity: "sha256:" + string(bytes.Repeat([]byte{'4'}, 64)), SizeBytes: 1}
	if _, _, err := encodeDeleteAttempt(marker, limits); !errors.Is(err, ErrExecutionIntegrity) {
		t.Fatalf("non-fsbind identity accepted: %v", err)
	}
	deep := strings.Repeat("[", 9) + "0" + strings.Repeat("]", 9)
	if _, err := readExecutionMarker(strings.NewReader(deep), limits.MaxMarkerBytes); !errors.Is(err, ErrExecutionIntegrity) {
		t.Fatalf("deep marker JSON accepted: %v", err)
	}
	if _, err := readExecutionMarker(bytes.NewReader([]byte{'{', '"', 0xff, '"', ':', '0', '}'}), limits.MaxMarkerBytes); !errors.Is(err, ErrExecutionIntegrity) {
		t.Fatalf("invalid UTF-8 marker accepted: %v", err)
	}
}

func TestExecutionRootScopeRejectsDuplicateAndNestedRoots(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, roots := range [][]string{{root, filepath.Join(root, ".")}, {root, nested}} {
		if _, _, err := normalizeExecutionRoots(context.Background(), roots, false); !errors.Is(err, ErrExecutionPolicy) {
			t.Fatalf("overlapping roots accepted: %#v err=%v", roots, err)
		}
	}
}

func TestExecutionPreCancelledRootWorkDoesNotTouchFilesystem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	missing := filepath.Join(t.TempDir(), "missing")
	invalid, invalidErr := Status(context.Background(), StatusOptions{TargetRoot: missing, OperationID: "invalid", Limits: DefaultExecutionLimits()})
	if invalidErr != nil || invalid.Outcome != ExecutionOutcomeBlocked || !hasFinding(invalid.Blockers, "selector.invalid") {
		t.Fatalf("invalid status selector=%#v err=%v", invalid, invalidErr)
	}
	if _, _, err := normalizeExecutionRoots(ctx, []string{missing}, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled root normalization = %v", err)
	}
	operation, err := ParseOperationID("sha256:" + string(bytes.Repeat([]byte{'a'}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	report, err := Status(ctx, StatusOptions{TargetRoot: missing, OperationID: operation, Limits: DefaultExecutionLimits()})
	if !errors.Is(err, context.Canceled) || report.Outcome != ExecutionOutcomeIncomplete || report.Operation.Status != "inspection_incomplete" {
		t.Fatalf("pre-cancelled status=%#v err=%v", report, err)
	}
}
