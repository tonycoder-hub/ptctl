package clientadopt

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

type fakeMutationSession struct {
	requests           int
	ledgers            []downloader.LedgerSnapshot
	readErrs           []error
	addErr             error
	addStopReason      string
	adds               int
	suppressAddRequest bool
}

func (session *fakeMutationSession) ReadLedger(context.Context) (downloader.LedgerSnapshot, error) {
	session.requests++
	if len(session.readErrs) > 0 {
		err := session.readErrs[0]
		session.readErrs = session.readErrs[1:]
		if err != nil {
			return downloader.LedgerSnapshot{}, err
		}
	}
	if len(session.ledgers) == 0 {
		return downloader.LedgerSnapshot{}, fmt.Errorf("no ledger")
	}
	result := session.ledgers[0]
	if len(session.ledgers) > 1 {
		session.ledgers = session.ledgers[1:]
	}
	return result, nil
}

func (session *fakeMutationSession) ReadJobFiles(context.Context, string, downloader.JobFileLedgerLimits) (downloader.JobFileLedgerSnapshot, error) {
	return downloader.JobFileLedgerSnapshot{}, fmt.Errorf("not supported")
}

func (session *fakeMutationSession) AddStopped(_ context.Context, request downloader.AddStoppedRequest) (downloader.MutationReceipt, error) {
	if !session.suppressAddRequest {
		session.requests++
	}
	session.adds++
	reader, err := request.Metafile.Open()
	if err != nil {
		return downloader.MutationReceipt{}, err
	}
	raw, err := io.ReadAll(reader)
	if err != nil || int64(len(raw)) != request.Metafile.SizeBytes() {
		return downloader.MutationReceipt{}, fmt.Errorf("payload read failed")
	}
	now := time.Now().UTC()
	receipt := downloader.MutationReceipt{
		Effect: "submit_exact_metafile_stopped", ObservedAtStart: now, ObservedAtEnd: now,
		RequestsAttempted: 1, BytesSubmitted: int64(len(raw)), BytesSubmittedKnown: session.addErr == nil,
		Complete: session.addErr == nil,
	}
	if session.suppressAddRequest {
		receipt.RequestsAttempted = 0
		receipt.BytesSubmitted = 0
		receipt.BytesSubmittedKnown = false
		receipt.Complete = false
	}
	if session.addErr != nil {
		receipt.StopReason = session.addStopReason
		if receipt.StopReason == "" {
			receipt.StopReason = "transport_failed"
		}
	}
	return receipt, session.addErr
}

func TestRunClassifiesPreRequestCancellationAsUnknownNotIntegrity(t *testing.T) {
	ctx := context.Background()
	fixture := makeMaterializedFixture(t, ctx)
	prepared := prepareFixturePlan(t, fixture)
	before := ledgerSnapshot(fixture.meta, nil, time.Now().UTC())
	session := &fakeMutationSession{
		requests: 1, ledgers: []downloader.LedgerSnapshot{before}, addErr: context.Canceled, suppressAddRequest: true,
	}
	report, err := Run(ctx, RunOptions{
		Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Metafile: fixture.payload(t),
		Session: session, AcknowledgeAdd: true,
	})
	if !errors.Is(err, ErrRequestUnknown) || !errors.Is(err, context.Canceled) || errors.Is(err, ErrIntegrity) ||
		report.Outcome != OutcomeRequestUnknown || report.Client.AddAttempted || report.Client.RequestsMade != 2 ||
		report.Operation.PhaseAfter != "request_result_unknown" || !report.Operation.Resumable {
		t.Fatalf("report=%#v requests=%d err=%v", report, session.requests, err)
	}
}

func (session *fakeMutationSession) RequestsMade() int { return session.requests }
func (session *fakeMutationSession) Close() error      { return nil }

func TestRunAdoptsAbsentJobStoppedAndRecordsTerminalJournal(t *testing.T) {
	ctx := context.Background()
	fixture := makeMaterializedFixture(t, ctx)
	prepared := prepareFixturePlan(t, fixture)
	savePath, _ := prepared.savePath()
	contentPath, _ := prepared.contentPath()
	before := ledgerSnapshot(fixture.meta, nil, time.Now().UTC())
	after := ledgerSnapshot(fixture.meta, &downloader.Torrent{
		Hash: "opaque-job", InfoHashV1: fixture.meta.InfoHashV1, IdentityStatus: downloader.IdentityStatusValid,
		IdentityEvidence: []string{"magnet_xt_btih_hex"}, IdentityIssues: []string{},
		SizeBytes: fixture.meta.TotalLength, State: "stoppedDL", SavePath: savePath, ContentPath: contentPath,
	}, before.ObservedAtEnd.Add(time.Millisecond))
	session := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{before, after}}
	payload := fixture.payload(t)
	if err := PreflightRun(ctx, prepared, prepared.PlanID(), payload); err != nil {
		t.Fatal(err)
	}
	report, err := Run(ctx, RunOptions{
		Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Metafile: payload, Session: session, AcknowledgeAdd: true,
	})
	if err != nil || report.Outcome != OutcomeAdoptedPendingRecheck || report.Operation.PhaseAfter != "adopted_pending_client_recheck" ||
		report.Client.JobID == "" || report.Client.ContentPathRef != prepared.plan.ExpectedContentPathRef || !report.Journal.CompletionDurable || session.requests != 4 {
		t.Fatalf("report=%#v requests=%d err=%v", report, session.requests, err)
	}
	status, err := Status(ctx, StatusOptions{TargetRoot: fixture.targetRoot, OperationID: prepared.OperationID()})
	if err != nil || status.Outcome != OutcomeHistoricalAdopted || status.Client.Status != "historical_adoption_recorded_current_client_not_observed" || status.WritesPerformed != 0 {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	if status.Journal.IntentDurable || status.Journal.CompletionDurable || !containsWarning(status.Warnings, "does not refresh directory durability") {
		t.Fatalf("read-only status overclaimed journal durability: %#v", status)
	}
	current := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{after}}
	repeated, err := Resume(ctx, prepared.OperationID(), RunOptions{
		Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Session: current,
	})
	if err != nil || repeated.Outcome != OutcomeAlreadyAdopted || current.adds != 0 || repeated.WritesPerformed != 0 ||
		!repeated.Journal.IntentDurable || !repeated.Journal.CompletionDurable {
		t.Fatalf("resume=%#v adds=%d err=%v", repeated, current.adds, err)
	}
	absentNow := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{before}}
	missing, err := Resume(ctx, prepared.OperationID(), RunOptions{
		Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Session: absentNow,
	})
	if !errors.Is(err, ErrPolicy) || missing.Outcome != OutcomeBlocked || missing.Operation.Status != "terminal" ||
		missing.Operation.Resumable || missing.Client.Status != "exact_job_absent" || absentNow.adds != 0 {
		t.Fatalf("missing current job report=%#v adds=%d err=%v", missing, absentNow.adds, err)
	}
	replacement := after
	replacement.Jobs = append([]downloader.Torrent(nil), after.Jobs...)
	replacement.Jobs[0].Hash = "different-opaque-job"
	replacedNow := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{replacement}}
	replaced, err := Resume(ctx, prepared.OperationID(), RunOptions{
		Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Session: replacedNow,
	})
	if !errors.Is(err, ErrPolicy) || replaced.Outcome != OutcomeBlocked || replaced.Operation.Status != "terminal" ||
		replaced.Operation.Resumable || len(replaced.Blockers) == 0 || replacedNow.adds != 0 {
		t.Fatalf("replaced current job report=%#v adds=%d err=%v", replaced, replacedNow.adds, err)
	}
	encoded := fmt.Sprintf("%#v", report)
	if strings.Contains(encoded, fixture.targetRoot) || strings.Contains(encoded, savePath) || strings.Contains(encoded, contentPath) || strings.Contains(encoded, "opaque-job") {
		t.Fatalf("report leaked raw path or job key: %s", encoded)
	}
}

func TestTransmissionRunAdoptsV1StoppedWithoutGrantingUnknownResponseAttribution(t *testing.T) {
	ctx := context.Background()
	fixture := makeMaterializedFixture(t, ctx)
	prepared := prepareTransmissionFixturePlan(t, fixture)
	savePath, _ := prepared.savePath()
	contentPath, _ := prepared.contentPath()
	before := ledgerSnapshotForDriver(fixture.meta, downloader.DriverTransmission, nil, time.Now().UTC())
	after := ledgerSnapshotForDriver(fixture.meta, downloader.DriverTransmission, &downloader.Torrent{
		Hash: fixture.meta.InfoHashV1, InfoHashV1: fixture.meta.InfoHashV1, IdentityStatus: downloader.IdentityStatusValid,
		IdentityEvidence: []string{"transmission_hash_string_sha1"}, IdentityIssues: []string{},
		SizeBytes: fixture.meta.TotalLength, State: "stoppedDL", SavePath: savePath, ContentPath: contentPath,
	}, before.ObservedAtEnd.Add(time.Millisecond))
	session := &fakeMutationSession{requests: 2, ledgers: []downloader.LedgerSnapshot{before, after}}
	report, err := Run(ctx, RunOptions{
		Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Metafile: fixture.payload(t), Session: session, AcknowledgeAdd: true,
	})
	if err != nil || report.Outcome != OutcomeAdoptedPendingRecheck || report.Plan.Driver != DriverTransmission ||
		session.requests != 5 || !report.Journal.CompletionDurable {
		t.Fatalf("report=%#v requests=%d err=%v", report, session.requests, err)
	}

	secondFixture := makeMaterializedFixture(t, ctx)
	uncertainPlan := prepareTransmissionFixturePlan(t, secondFixture)
	uncertainSave, _ := uncertainPlan.savePath()
	uncertainContent, _ := uncertainPlan.contentPath()
	uncertainBefore := ledgerSnapshotForDriver(secondFixture.meta, downloader.DriverTransmission, nil, time.Now().UTC())
	uncertainAfter := ledgerSnapshotForDriver(secondFixture.meta, downloader.DriverTransmission, &downloader.Torrent{
		Hash: secondFixture.meta.InfoHashV1, InfoHashV1: secondFixture.meta.InfoHashV1, IdentityStatus: downloader.IdentityStatusValid,
		IdentityEvidence: []string{"transmission_hash_string_sha1"}, IdentityIssues: []string{}, SizeBytes: secondFixture.meta.TotalLength,
		State: "stoppedDL", SavePath: uncertainSave, ContentPath: uncertainContent,
	}, uncertainBefore.ObservedAtEnd.Add(time.Millisecond))
	failed := &fakeMutationSession{requests: 2, ledgers: []downloader.LedgerSnapshot{uncertainBefore, uncertainAfter}, addErr: fmt.Errorf("response lost")}
	unknown, err := Run(ctx, RunOptions{
		Prepared: uncertainPlan, ExpectedPlanID: uncertainPlan.PlanID(), Metafile: secondFixture.payload(t), Session: failed, AcknowledgeAdd: true,
	})
	if !errors.Is(err, ErrRequestUnknown) || unknown.Outcome != OutcomeRequestUnknown || unknown.Journal.CompletionDurable {
		t.Fatalf("uncertain report=%#v err=%v", unknown, err)
	}
	resume := &fakeMutationSession{requests: 2, ledgers: []downloader.LedgerSnapshot{uncertainAfter}}
	resumed, err := Resume(ctx, uncertainPlan.OperationID(), RunOptions{
		Prepared: uncertainPlan, ExpectedPlanID: uncertainPlan.PlanID(), Session: resume,
	})
	if !errors.Is(err, ErrRequestUnknown) || resumed.Outcome != OutcomeRequestUnknown || resume.adds != 0 ||
		resumed.Client.Status != "exact_job_observed_without_durable_transmission_acceptance" {
		t.Fatalf("resume=%#v adds=%d err=%v", resumed, resume.adds, err)
	}
}

func TestTransmissionDuplicateResponseNeverCreatesAdoptionCompletion(t *testing.T) {
	ctx := context.Background()
	fixture := makeMaterializedFixture(t, ctx)
	prepared := prepareTransmissionFixturePlan(t, fixture)
	savePath, _ := prepared.savePath()
	contentPath, _ := prepared.contentPath()
	before := ledgerSnapshotForDriver(fixture.meta, downloader.DriverTransmission, nil, time.Now().UTC())
	after := ledgerSnapshotForDriver(fixture.meta, downloader.DriverTransmission, &downloader.Torrent{
		Hash: fixture.meta.InfoHashV1, InfoHashV1: fixture.meta.InfoHashV1, IdentityStatus: downloader.IdentityStatusValid,
		IdentityEvidence: []string{"transmission_hash_string_sha1"}, IdentityIssues: []string{}, SizeBytes: fixture.meta.TotalLength,
		State: "stoppedDL", SavePath: savePath, ContentPath: contentPath,
	}, before.ObservedAtEnd.Add(time.Millisecond))
	session := &fakeMutationSession{requests: 2, ledgers: []downloader.LedgerSnapshot{before, after},
		addErr: fmt.Errorf("duplicate"), addStopReason: "already_exists"}
	report, err := Run(ctx, RunOptions{
		Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Metafile: fixture.payload(t), Session: session, AcknowledgeAdd: true,
	})
	if !errors.Is(err, ErrPolicy) || report.Outcome != OutcomeBlocked || report.Journal.CompletionDurable ||
		report.Client.Status != "add_rejected_existing_exact_job" {
		t.Fatalf("report=%#v err=%v", report, err)
	}
}

func containsWarning(warnings []string, fragment string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, fragment) {
			return true
		}
	}
	return false
}

func TestResumeNeverRepeatsUnknownAddWithoutExplicitAcknowledgement(t *testing.T) {
	ctx := context.Background()
	fixture := makeMaterializedFixture(t, ctx)
	prepared := prepareFixturePlan(t, fixture)
	before := ledgerSnapshot(fixture.meta, nil, time.Now().UTC())
	failed := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{before, before}, addErr: fmt.Errorf("transport failed")}
	report, err := Run(ctx, RunOptions{
		Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Metafile: fixture.payload(t),
		Session: failed, AcknowledgeAdd: true,
	})
	if !errors.Is(err, ErrRequestUnknown) || report.Outcome != OutcomeRequestUnknown || failed.adds != 1 {
		t.Fatalf("initial report=%#v adds=%d err=%v", report, failed.adds, err)
	}
	observeOnly := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{before}}
	resumed, err := Resume(ctx, prepared.OperationID(), RunOptions{
		Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Session: observeOnly,
	})
	if !errors.Is(err, ErrRequestUnknown) || resumed.Outcome != OutcomeRequestUnknown || observeOnly.adds != 0 || observeOnly.requests != 2 {
		t.Fatalf("observe-only resume=%#v requests=%d adds=%d err=%v", resumed, observeOnly.requests, observeOnly.adds, err)
	}
	savePath, _ := prepared.savePath()
	contentPath, _ := prepared.contentPath()
	after := ledgerSnapshot(fixture.meta, &downloader.Torrent{
		Hash: "opaque-repeat", InfoHashV1: fixture.meta.InfoHashV1, IdentityStatus: downloader.IdentityStatusValid,
		IdentityEvidence: []string{"magnet_xt_btih_hex"}, IdentityIssues: []string{}, SizeBytes: fixture.meta.TotalLength,
		State: "pausedDL", SavePath: savePath, ContentPath: contentPath,
	}, before.ObservedAtEnd.Add(time.Second))
	repeat := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{before, after}}
	completed, err := Resume(ctx, prepared.OperationID(), RunOptions{
		Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Session: repeat, AcknowledgeAdd: true, RepeatAdd: true,
		Metafile: fixture.payload(t),
	})
	if err != nil || completed.Outcome != OutcomeAdoptedPendingRecheck || repeat.adds != 1 || completed.Journal.AttemptsRecorded != 2 {
		t.Fatalf("repeat resume=%#v adds=%d err=%v", completed, repeat.adds, err)
	}
}

func TestRunRejectsMissingPayloadBeforeLedgerOrJournal(t *testing.T) {
	ctx := context.Background()
	fixture := makeMaterializedFixture(t, ctx)
	prepared := prepareFixturePlan(t, fixture)
	session := &fakeMutationSession{requests: 1}
	report, err := Run(ctx, RunOptions{
		Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Session: session, AcknowledgeAdd: true,
	})
	if !errors.Is(err, ErrPolicy) || report.Outcome != OutcomeBlocked || report.WritesPerformed != 0 ||
		session.requests != 1 || session.adds != 0 {
		t.Fatalf("report=%#v requests=%d adds=%d err=%v", report, session.requests, session.adds, err)
	}
	if _, statusErr := Status(ctx, StatusOptions{TargetRoot: fixture.targetRoot, OperationID: prepared.OperationID()}); !errors.Is(statusErr, ErrOperationNotFound) {
		t.Fatalf("missing payload created an operation: %v", statusErr)
	}
}

func TestSafeMutationReceiptNeverRewritesContradictionsAsSuccessOrZeroRetries(t *testing.T) {
	now := time.Now().UTC()
	got := safeMutationReceipt(downloader.MutationReceipt{
		Effect: "UNTRUSTED-CANARY", ObservedAtStart: now, ObservedAtEnd: now,
		Complete: true, RequestsAttempted: 2, AutomaticRetries: 3, RedirectsFollowed: -1,
		BytesSubmitted: 23, BytesSubmittedKnown: true, StopReason: "UNTRUSTED-STOP-CANARY",
	}, 23)
	if got.Complete || got.RequestsAttempted != -1 || got.AutomaticRetries != 3 || got.RedirectsFollowed != -1 ||
		got.StopReason != "client_mutation_failed" || got.Effect != "submit_exact_metafile_stopped" {
		t.Fatalf("unsafe mutation receipt was overclaimed: %#v", got)
	}
	encoded := fmt.Sprintf("%#v", got)
	if strings.Contains(encoded, "UNTRUSTED") {
		t.Fatalf("untrusted mutation strings escaped normalization: %s", encoded)
	}
}

func TestResumeRecoversOnlyAnExactEmptyInitializationNamespace(t *testing.T) {
	ctx := context.Background()
	fixture := makeMaterializedFixture(t, ctx)
	prepared := prepareFixturePlan(t, fixture)
	directoryName, err := operationDirectoryName(prepared.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := fsbind.BindExisting(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	subtree, _, err := session.CreatePrivateSubtreeWithReceipt(directoryName)
	if err != nil {
		t.Fatal(err)
	}
	if err := subtree.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	status, err := Status(ctx, StatusOptions{TargetRoot: fixture.targetRoot, OperationID: prepared.OperationID()})
	if !errors.Is(err, ErrInitializationIncomplete) || status.Outcome != OutcomeIncomplete ||
		status.Operation.Status != "initialization_incomplete" || !status.Operation.Resumable || status.WritesPerformed != 0 {
		t.Fatalf("status=%#v err=%v", status, err)
	}

	savePath, _ := prepared.savePath()
	contentPath, _ := prepared.contentPath()
	before := ledgerSnapshot(fixture.meta, nil, time.Now().UTC())
	after := ledgerSnapshot(fixture.meta, &downloader.Torrent{
		Hash: "recovered-job", InfoHashV1: fixture.meta.InfoHashV1, IdentityStatus: downloader.IdentityStatusValid,
		IdentityEvidence: []string{"magnet_xt_btih_hex"}, IdentityIssues: []string{}, SizeBytes: fixture.meta.TotalLength,
		State: "stoppedDL", SavePath: savePath, ContentPath: contentPath,
	}, before.ObservedAtEnd.Add(time.Millisecond))
	mutation := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{before, after}}
	report, err := Resume(ctx, prepared.OperationID(), RunOptions{
		Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Session: mutation, AcknowledgeAdd: true,
		Metafile: fixture.payload(t),
	})
	if err != nil || report.Outcome != OutcomeAdoptedPendingRecheck || report.Writes.ControlDirectories != 1 ||
		!report.Journal.IntentDurable || !report.Journal.CompletionDurable || mutation.adds != 1 {
		t.Fatalf("report=%#v adds=%d err=%v", report, mutation.adds, err)
	}
}

func TestResumeNeverRepairsAnInitializationNamespaceWithUnexpectedObjects(t *testing.T) {
	ctx := context.Background()
	fixture := makeMaterializedFixture(t, ctx)
	prepared := prepareFixturePlan(t, fixture)
	directoryName, _ := operationDirectoryName(prepared.OperationID())
	session, _, err := fsbind.BindExisting(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	subtree, _, err := session.CreatePrivateSubtreeWithReceipt(directoryName)
	if err != nil {
		t.Fatal(err)
	}
	unexpected, _ := fsbind.PathFromComponents([]string{"unexpected"})
	if _, err := subtree.MkdirAll(ctx, unexpected); err != nil {
		t.Fatal(err)
	}
	_ = subtree.Close()
	_ = session.Close()

	mutation := &fakeMutationSession{requests: 1}
	report, err := Resume(ctx, prepared.OperationID(), RunOptions{
		Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Session: mutation, AcknowledgeAdd: true,
		Metafile: fixture.payload(t),
	})
	if !errors.Is(err, ErrIntegrity) || report.Outcome != OutcomeIntegrityFailed || report.WritesPerformed != 0 ||
		mutation.requests != 1 || mutation.adds != 0 {
		t.Fatalf("report=%#v requests=%d adds=%d err=%v", report, mutation.requests, mutation.adds, err)
	}
}

func TestStatusNeverCallsAnUnobservedClientStateReady(t *testing.T) {
	ctx := context.Background()
	fixture := makeMaterializedFixture(t, ctx)
	prepared := prepareFixturePlan(t, fixture)
	handle, _, err := createJournal(ctx, fixture.targetRoot, prepared.plan, prepared.planID)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := Status(ctx, StatusOptions{TargetRoot: fixture.targetRoot, OperationID: prepared.OperationID()})
	if err != nil || report.Outcome != OutcomeIncomplete || !report.Operation.Resumable ||
		report.Client.Status != "historical_intent_no_add_request_current_client_not_observed" || len(report.Blockers) == 0 {
		t.Fatalf("status=%#v err=%v", report, err)
	}
}

func TestStatusReportsExplicitMissingOperationWithoutInventingJournalState(t *testing.T) {
	operationID, err := ParseOperationID("sha256:" + strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	report, err := Status(context.Background(), StatusOptions{TargetRoot: t.TempDir(), OperationID: operationID})
	if !errors.Is(err, ErrOperationNotFound) || report.Outcome != OutcomeBlocked || report.Operation.Status != "not_found" ||
		report.Operation.ID != operationID.String() || report.Operation.Resumable || report.WritesPerformed != 0 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
}

func TestPreviewRejectsAContradictoryLedgerCapabilityBeforeAnyWrite(t *testing.T) {
	ctx := context.Background()
	fixture := makeMaterializedFixture(t, ctx)
	prepared := prepareFixturePlan(t, fixture)
	snapshot := ledgerSnapshot(fixture.meta, nil, time.Now().UTC())
	snapshot.Capabilities.ContentPath = false
	session := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{snapshot}}
	report, err := Preview(ctx, prepared, session)
	if !errors.Is(err, ErrPolicy) || report.Outcome != OutcomeBlocked || report.WritesPerformed != 0 ||
		session.requests != 2 || session.adds != 0 {
		t.Fatalf("report=%#v requests=%d adds=%d err=%v", report, session.requests, session.adds, err)
	}
}

func TestPreviewReverifiesCurrentFinalBeforeReadingClientLedger(t *testing.T) {
	ctx := context.Background()
	fixture := makeMaterializedFixture(t, ctx)
	prepared := prepareFixturePlan(t, fixture)
	if err := os.WriteFile(filepath.Join(fixture.targetRoot, "adopt.bin"), []byte("changed after plan preparation"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := &fakeMutationSession{requests: 1, ledgers: []downloader.LedgerSnapshot{ledgerSnapshot(fixture.meta, nil, time.Now().UTC())}}
	report, err := Preview(ctx, prepared, session)
	if !errors.Is(err, materialize.ErrIntegrity) || report.Outcome != OutcomeIntegrityFailed ||
		report.Final.Status == "verified_current" || session.requests != 1 || report.Client.RequestsMade != 1 || report.Client.BeforeIdentity != "not_observed" {
		t.Fatalf("report=%#v requests=%d err=%v", report, session.requests, err)
	}
}

func TestRunRejectsAReusedOrUnauthenticatedSessionBeforeJournal(t *testing.T) {
	ctx := context.Background()
	fixture := makeMaterializedFixture(t, ctx)
	prepared := prepareFixturePlan(t, fixture)
	for _, initialRequests := range []int{0, 2} {
		session := &fakeMutationSession{requests: initialRequests}
		report, err := Run(ctx, RunOptions{
			Prepared: prepared, ExpectedPlanID: prepared.PlanID(), Session: session, AcknowledgeAdd: true,
			Metafile: fixture.payload(t),
		})
		if !errors.Is(err, ErrIntegrity) || report.Outcome != OutcomeIntegrityFailed || report.WritesPerformed != 0 ||
			session.requests != initialRequests || session.adds != 0 {
			t.Fatalf("initial=%d report=%#v requests=%d adds=%d err=%v", initialRequests, report, session.requests, session.adds, err)
		}
	}
}

type materializedFixture struct {
	meta       *metafile.MetaInfo
	store      *metastore.Store
	artifact   metastore.ArtifactID
	targetRoot string
	operation  materialize.OperationID
	planID     string
	verified   *materialize.VerifiedFinal
}

func makeMaterializedFixture(t *testing.T, ctx context.Context) materializedFixture {
	t.Helper()
	content := []byte("client adoption fixture")
	raw := singleV1Metafile(t, "adopt.bin", content)
	meta, err := metafile.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	store, _, err := metastore.Init(filepath.Join(t.TempDir(), "metastore"))
	if err != nil {
		t.Fatal(err)
	}
	_, artifact, _, err := store.Import(ctx, bytes.NewReader(raw), metastore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	searchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(searchRoot, "renamed-source"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	discovery, err := seed.Discover(ctx, meta, seed.DiscoverOptions{
		SearchRoots: []string{searchRoot}, InventoryLimits: storage.DefaultInventoryLimits(),
		MatchLimits: metafile.DefaultSourceMatchLimits(), TimeBudget: 10 * time.Second,
		TargetRoot: targetRoot, Strategy: materialize.StrategyCopy,
	})
	if err != nil || discovery.Plan == nil || discovery.SourceOutcome != "verified_unique" {
		t.Fatalf("discovery=%#v err=%v", discovery, err)
	}
	report, err := materialize.Run(ctx, materialize.RunOptions{
		Meta: meta, Discovery: &discovery, TargetRoot: targetRoot, ExpectedPlanID: discovery.Plan.ID, Limits: materialize.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := materialize.ParseOperationID(report.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	verified, _, err := materialize.VerifyCurrentFinal(ctx, materialize.FinalProofOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operation, ExpectedPlanID: discovery.Plan.ID, Limits: materialize.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return materializedFixture{
		meta: meta, store: store, artifact: artifact.ID, targetRoot: targetRoot,
		operation: operation, planID: discovery.Plan.ID, verified: verified,
	}
}

func (fixture materializedFixture) payload(t *testing.T) *metastore.ArtifactPayload {
	t.Helper()
	payload, err := fixture.store.LoadPayload(context.Background(), fixture.artifact, metastore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func prepareFixturePlan(t *testing.T, fixture materializedFixture) *PreparedPlan {
	t.Helper()
	root, err := filepath.EvalSymlinks(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := BuildPlan(fixture.verified, PlanOptions{
		ClientConfigID: "sha256:" + strings.Repeat("c", 64), HostRoot: root, ClientRoot: "/downloads", ClientWindows: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func prepareTransmissionFixturePlan(t *testing.T, fixture materializedFixture) *PreparedPlan {
	t.Helper()
	root, err := filepath.EvalSymlinks(fixture.targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := BuildPlan(fixture.verified, PlanOptions{
		Driver: DriverTransmission, ClientConfigID: "sha256:" + strings.Repeat("d", 64),
		HostRoot: root, ClientRoot: "/downloads", ClientWindows: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func ledgerSnapshot(meta *metafile.MetaInfo, job *downloader.Torrent, started time.Time) downloader.LedgerSnapshot {
	return ledgerSnapshotForDriver(meta, downloader.DriverQBittorrent, job, started)
}

func ledgerSnapshotForDriver(meta *metafile.MetaInfo, driver string, job *downloader.Torrent, started time.Time) downloader.LedgerSnapshot {
	jobs := []downloader.Torrent{}
	if job != nil {
		jobs = append(jobs, *job)
	}
	return downloader.LedgerSnapshot{
		Driver: driver, ObservedAtStart: started, ObservedAtEnd: started.Add(time.Millisecond), Complete: true,
		Capabilities: downloader.LedgerCapabilities{TypedInfoHashes: true, ContentPath: true, JobFiles: true}, Jobs: jobs,
	}
}

func singleV1Metafile(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	piece := sha1.Sum(content)
	info := fmt.Sprintf("d6:lengthi%de4:name%d:%s12:piece lengthi16384e6:pieces20:", len(content), len(name), name)
	return append(append([]byte("d4:info"+info), piece[:]...), []byte("ee")...)
}
