package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/clientstop"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
)

type clientStopTrackingReader struct {
	read   bool
	reader *strings.Reader
}

func (reader *clientStopTrackingReader) Read(buffer []byte) (int, error) {
	reader.read = true
	return reader.reader.Read(buffer)
}

type clientStopJSONEnvelope struct {
	Schema string            `json:"schema"`
	Kind   string            `json:"kind"`
	Data   clientstop.Report `json:"data"`
}

type clientStopRetentionJSONEnvelope struct {
	Schema string                     `json:"schema"`
	Kind   string                     `json:"kind"`
	Data   clientstop.RetentionReport `json:"data"`
}

type clientStopForgetJSONEnvelope struct {
	Schema string                  `json:"schema"`
	Kind   string                  `json:"kind"`
	Data   clientstop.ForgetReport `json:"data"`
}

type transmissionClientStopCLIFixture struct {
	materialized clientAdoptCLIFixture
	server       *transmissionAdoptServer
	activation   clientactivate.Report
}

func TestClientStopPlanRunStatusAndFreshCompletionProof(t *testing.T) {
	fixture := prepareClientStopCLIFixture(t)
	base := clientStopBaseArgs(fixture)
	requestsBeforePlan := fixture.server.totalRequests()

	planned := runClientStopJSON(t, append([]string{"client", "stop", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	if planned.Data.Outcome != clientstop.OutcomeReady || planned.Data.Effect != "none" || planned.Data.Plan.ID == "" ||
		planned.Data.Plan.Data.ReviewedJobState != "uploading" || planned.Data.Plan.Data.Stop.Protocol != "qbittorrent_webapi_v5" ||
		planned.Data.Writes.WritesPerformed != 0 || planned.Data.Client.RequestsMade != 3 ||
		fixture.server.totalRequests()-requestsBeforePlan != 3 {
		t.Fatalf("unexpected stop plan: %#v", planned.Data)
	}
	assertClientStopPrivate(t, mustJSON(t, planned), fixture)

	requestsBeforeRun := fixture.server.totalRequests()
	runArgs := append([]string{"client", "stop", "run"}, base...)
	runArgs = append(runArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-client-stop")
	stopped := runClientStopJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if stopped.Data.Outcome != clientstop.OutcomeStopped || stopped.Data.Operation.Resumable ||
		stopped.Data.Operation.ID != clientstop.OperationIDForPlan(planned.Data.Plan.ID).String() ||
		stopped.Data.Plan.ExpectedID != planned.Data.Plan.ID || !stopped.Data.Plan.Matches ||
		stopped.Data.Writes.DownloaderRequests != 1 || stopped.Data.Client.RequestsMade != 6 ||
		stopped.Data.Mutation.Status != "accepted" || !stopped.Data.Mutation.Receipt.Complete ||
		stopped.Data.Mutation.Receipt.RequestsAttempted != 1 || stopped.Data.Mutation.Receipt.AutomaticRetries != 0 ||
		stopped.Data.Mutation.Receipt.RedirectsFollowed != 0 || stopped.Data.Stopped.Status != "exact_typed_job_stopped" ||
		stopped.Data.Stopped.Observation.JobState != "stoppedUP" || stopped.Data.Final.BytesVerified != fixture.materialized.meta.TotalLength ||
		stopped.Data.Writes.WritesPerformed == 0 || fixture.server.stop.Load() != 1 ||
		fixture.server.totalRequests()-requestsBeforeRun != 6 {
		t.Fatalf("unexpected stop run: %#v", stopped.Data)
	}
	assertClientStopPrivate(t, mustJSON(t, stopped), fixture)

	requestsBeforeStatus := fixture.server.totalRequests()
	statusArgs := []string{"client", "stop", "status", "--target", fixture.materialized.materialize.targetRoot,
		"--expect-stop-plan-id", planned.Data.Plan.ID, "--output", "json", stopped.Data.Operation.ID}
	status := runClientStopJSON(t, statusArgs, &trackingReader{}, 0)
	if status.Data.Outcome != clientstop.OutcomeHistoricalComplete || status.Data.Operation.Resumable ||
		status.Data.Final.MetafileVariantID != "" || status.Data.Assurance.QueueEvidence != "historical_journal_only_not_currently_observed" ||
		status.Data.Assurance.FilesystemEvidence != "historical_plan_only_not_currently_reverified" ||
		status.Data.Assurance.CompletionBasis != "accepted_response_then_exact_stopped" ||
		fixture.server.totalRequests() != requestsBeforeStatus {
		t.Fatalf("unexpected historical status: %#v", status.Data)
	}

	resumeArgs := append([]string{"client", "stop", "resume"}, base...)
	resumeArgs = append(resumeArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, stopped.Data.Operation.ID)
	fresh := runClientStopJSON(t, resumeArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if fresh.Data.Outcome != clientstop.OutcomeAlreadyComplete || fresh.Data.Operation.Resumable ||
		fresh.Data.Stopped.Status != "exact_typed_job_stopped" || fresh.Data.Final.BytesVerified != fixture.materialized.meta.TotalLength ||
		fresh.Data.Client.RequestsMade != 2 || fixture.server.stop.Load() != 1 {
		t.Fatalf("unexpected fresh completion proof: %#v", fresh.Data)
	}
	assertClientStopPrivate(t, mustJSON(t, fresh), fixture)
}

func TestReconcileReportBindsTerminalClientStopToCurrentStoppedJobWithoutExtraRequests(t *testing.T) {
	fixture := prepareClientStopCLIFixture(t)
	base := clientStopBaseArgs(fixture)
	planned := runClientStopJSON(t, append([]string{"client", "stop", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	runArgs := append([]string{"client", "stop", "run"}, base...)
	runArgs = append(runArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-client-stop")
	stopped := runClientStopJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if stopped.Data.Outcome != clientstop.OutcomeStopped {
		t.Fatalf("client stop did not complete: %#v", stopped.Data)
	}

	reconcileArgs := append([]string{"reconcile", "report"}, base...)
	reconcileArgs = append(reconcileArgs, "--stop-operation", stopped.Data.Operation.ID, "--stop-plan-id", planned.Data.Plan.ID)
	requestsBefore := fixture.server.totalRequests()
	stopsBefore := fixture.server.stop.Load()
	var out, errOut bytes.Buffer
	if code := Run(reconcileArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("reconcile code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if delta := fixture.server.totalRequests() - requestsBefore; delta != 3 {
		t.Fatalf("stop reconciliation made %d requests; wanted one login plus the existing two-read bracket", delta)
	}
	if fixture.server.stop.Load() != stopsBefore {
		t.Fatal("read-only stop reconciliation repeated the stop mutation")
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	stop := response.Data.Ledgers.Stop
	activation := response.Data.Ledgers.Activation
	if response.Data.Outcome != "consistent" || !response.Data.Scope.ClientStopRequested ||
		stop.Status != "historical_stop_current_job_stopped" || !stop.Historical ||
		!stop.ProcessLocalCompletionProof || !stop.ProcessLocalCurrentProof ||
		stop.Completion == nil || stop.CurrentStopped == nil || stop.Completion.RetainedTombstone ||
		stop.Completion.OperationID != stopped.Data.Operation.ID || stop.Completion.PlanID != planned.Data.Plan.ID ||
		stop.Completion.CompletionBasis != "accepted_response_then_exact_stopped" ||
		stop.CurrentStopped.JobID != stop.Completion.JobID || stop.CurrentStopped.JobState != "stoppedUP" ||
		activation.Status != "historical_completion_current_job_bound" || !activation.ProcessLocalCurrentUseProof ||
		!slices.Contains(response.Data.Effect, "read_client_stop_operation_state") ||
		!slices.Contains(response.Data.Effect, "read_downloader_state") || len(response.Data.Relations) != 5 ||
		!strings.Contains(response.Data.Assurance, "canonical_historical_client_stop_bound_to_current_exact_stopped_job") {
		t.Fatalf("terminal client stop was not reconciled against the current stopped job: %s", out.String())
	}
	var human bytes.Buffer
	if err := writeReconciliationHuman(&human, response.Data); err != nil ||
		!strings.Contains(human.String(), "CLIENT STOP") ||
		!strings.Contains(human.String(), "PROCESS-LOCAL CURRENT-STOPPED BRIDGE  true") ||
		!strings.Contains(human.String(), "client stop        historical_stop_current_job_stopped") ||
		strings.Index(human.String(), "CLIENT STOP") > strings.Index(human.String(), "LEDGERS") {
		t.Fatalf("client-stop table contract is unclear: err=%v\n%s", err, human.String())
	}
	assertReconciledClientStopPrivate(t, out.Bytes(), fixture)

	fixture.server.setState("uploading", 1)
	requestsBefore = fixture.server.totalRequests()
	out.Reset()
	errOut.Reset()
	if code := Run(reconcileArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("restarted-job reconcile code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if delta := fixture.server.totalRequests() - requestsBefore; delta != 3 {
		t.Fatalf("restarted-job stop reconciliation made %d requests", delta)
	}
	response = struct {
		Data reconcile.Report `json:"data"`
	}{}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "incomplete" || response.Data.Ledgers.Stop.Status != "incomplete" ||
		response.Data.Ledgers.Stop.StopReason != "stop_current_stopped_bridge_failed" ||
		response.Data.Ledgers.Stop.ProcessLocalCurrentProof || response.Data.Ledgers.Stop.CurrentStopped != nil ||
		!hasReportFindingCLI(response.Data.Blockers, "stop.current_stopped_unavailable") {
		t.Fatalf("historical stop was mistaken for current stopped state after restart: %s", out.String())
	}
	fixture.server.setState("stoppedUP", 1)

	pruneArgs := []string{"client", "stop", "prune", "--target", fixture.materialized.materialize.targetRoot,
		"--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-operation-state-deletion", "--output", "json", stopped.Data.Operation.ID}
	out.Reset()
	errOut.Reset()
	if code := Run(pruneArgs, &trackingReader{}, &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("stop prune code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	requestsBefore = fixture.server.totalRequests()
	out.Reset()
	errOut.Reset()
	if code := Run(reconcileArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("retained stop reconcile code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if delta := fixture.server.totalRequests() - requestsBefore; delta != 3 {
		t.Fatalf("retained stop reconciliation made %d requests", delta)
	}
	response = struct {
		Data reconcile.Report `json:"data"`
	}{}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "consistent" || response.Data.Ledgers.Stop.Completion == nil ||
		!response.Data.Ledgers.Stop.Completion.RetainedTombstone ||
		response.Data.Ledgers.Stop.Status != "historical_stop_current_job_stopped" ||
		!response.Data.Ledgers.Stop.ProcessLocalCurrentProof {
		t.Fatalf("retained stop tombstone lost terminal authority: %s", out.String())
	}
}

func TestReconcileReportDoesNotConsumeCredentialsForHistoricallyUnattributedStop(t *testing.T) {
	fixture := prepareClientStopCLIFixture(t)
	base := clientStopBaseArgs(fixture)
	planned := runClientStopJSON(t, append([]string{"client", "stop", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	fixture.server.setStopMode("unknown_job_stopped")
	runArgs := append([]string{"client", "stop", "run"}, base...)
	runArgs = append(runArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-client-stop")
	stopped := runClientStopJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if stopped.Data.Outcome != clientstop.OutcomeStoppedUnattributed {
		t.Fatalf("stop was not recorded as causality-unproven: %#v", stopped.Data)
	}

	reconcileArgs := append([]string{"reconcile", "report"}, base...)
	reconcileArgs = append(reconcileArgs, "--stop-operation", stopped.Data.Operation.ID, "--stop-plan-id", planned.Data.Plan.ID)
	reader := &trackingReader{}
	requestsBefore := fixture.server.totalRequests()
	var out, errOut bytes.Buffer
	if code := Run(reconcileArgs, reader, &out, &errOut); code != 0 || errOut.Len() != 0 || reader.read {
		t.Fatalf("reconcile code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
	if fixture.server.totalRequests() != requestsBefore {
		t.Fatal("causality-unproven stop reconciliation contacted the downloader")
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	stop := response.Data.Ledgers.Stop
	if response.Data.Outcome != "incomplete" || stop.Status != "historical_stop_causality_unproven" ||
		stop.StopReason != "stop_causality_unproven" || !stop.ProcessLocalCompletionProof ||
		stop.ProcessLocalCurrentProof || stop.Completion == nil ||
		stop.Completion.CompletionBasis != "exact_stopped_after_unknown_attempt_causality_unproven" ||
		response.Data.Ledgers.Downloader.RequestsMade != 0 ||
		!hasReportFindingCLI(response.Data.Blockers, "stop.causality_unproven") ||
		slices.Contains(response.Data.Effect, "read_downloader_state") {
		t.Fatalf("causality-unproven stop was overstated or erased: %s", out.String())
	}
	assertReconciledClientStopPrivate(t, out.Bytes(), fixture)

	requireArgs := append(append([]string(nil), reconcileArgs...), "--require-reconciled")
	reader = &trackingReader{}
	requestsBefore = fixture.server.totalRequests()
	out.Reset()
	errOut.Reset()
	if code := Run(requireArgs, reader, &out, &errOut); code != 4 || reader.read ||
		!strings.Contains(out.String(), `"outcome": "incomplete"`) ||
		!strings.Contains(errOut.String(), "outcome is not consistent") {
		t.Fatalf("require reconcile code/report mismatch: read=%t stdout=%q stderr=%q", reader.read, out.String(), errOut.String())
	}
	if fixture.server.totalRequests() != requestsBefore {
		t.Fatal("required causality-unproven stop reconciliation contacted the downloader")
	}
}

func TestReconcileReportRejectsCorruptStopJournalBeforeCredentialsOrNetwork(t *testing.T) {
	fixture := prepareClientStopCLIFixture(t)
	base := clientStopBaseArgs(fixture)
	planned := runClientStopJSON(t, append([]string{"client", "stop", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	runArgs := append([]string{"client", "stop", "run"}, base...)
	runArgs = append(runArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-client-stop")
	stopped := runClientStopJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)

	operationDirectory := filepath.Join(fixture.materialized.materialize.targetRoot,
		".ptctl-client-stop-"+strings.TrimPrefix(stopped.Data.Operation.ID, "sha256:"))
	completionPath := filepath.Join(operationDirectory, "completion.json")
	raw, err := os.ReadFile(completionPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || raw[0] != '{' {
		t.Fatalf("unexpected completion marker: %q", raw)
	}
	raw[0] = '['
	if err := os.WriteFile(completionPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	reconcileArgs := append([]string{"reconcile", "report"}, base...)
	reconcileArgs = append(reconcileArgs, "--stop-operation", stopped.Data.Operation.ID, "--stop-plan-id", planned.Data.Plan.ID)
	reader := &trackingReader{}
	requestsBefore := fixture.server.totalRequests()
	var out, errOut bytes.Buffer
	if code := Run(reconcileArgs, reader, &out, &errOut); code != 3 || reader.read {
		t.Fatalf("reconcile code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
	if fixture.server.totalRequests() != requestsBefore {
		t.Fatal("corrupt stop reconciliation contacted the downloader")
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatalf("integrity failure did not emit a report: %v\n%s", err, out.String())
	}
	if response.Data.Outcome != "integrity_failed" || response.Data.Ledgers.Stop.Status != "integrity_failed" ||
		response.Data.Ledgers.Stop.StopReason != "stop_completion_integrity_failed" ||
		response.Data.Ledgers.Stop.ProcessLocalCompletionProof ||
		!hasReportFindingCLI(response.Data.Blockers, "stop.completion_proof_unavailable") ||
		!strings.Contains(errOut.String(), "client-stop journal failed integrity verification") {
		t.Fatalf("corrupt stop journal was not reported precisely: stdout=%s stderr=%s", out.String(), errOut.String())
	}
	assertReconciledClientStopPrivate(t, out.Bytes(), fixture)
}

func TestReconcileStopSelectorUsageFailsBeforeCredentialRead(t *testing.T) {
	planID := strings.Repeat("a", 24)
	operation := clientstop.OperationIDForPlan(planID).String()
	for _, args := range [][]string{
		{"reconcile", "report", "--stop-operation", operation, "--password-stdin"},
		{"reconcile", "report", "--stop-plan-id", planID, "--password-stdin"},
		{"reconcile", "report", "--stop-operation", operation, "--stop-plan-id", planID,
			"--removal-operation", strings.Repeat("b", 64), "--removal-plan-id", strings.Repeat("b", 24), "--password-stdin"},
	} {
		reader := &trackingReader{}
		var out, errOut bytes.Buffer
		if code := Run(args, reader, &out, &errOut); code != 2 || reader.read {
			t.Fatalf("args=%v code=%d read=%t stdout=%q stderr=%q", args, code, reader.read, out.String(), errOut.String())
		}
	}
}

func TestClientStopUnknownResultObservesBeforeRequiringRepeat(t *testing.T) {
	fixture := prepareClientStopCLIFixture(t)
	base := clientStopBaseArgs(fixture)
	planned := runClientStopJSON(t, append([]string{"client", "stop", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	fixture.server.setStopMode("unknown_keep_started")

	runArgs := append([]string{"client", "stop", "run"}, base...)
	runArgs = append(runArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-client-stop")
	unknown := runClientStopJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 4)
	if unknown.Data.Outcome != clientstop.OutcomeRequestUnknown || !unknown.Data.Operation.Resumable ||
		unknown.Data.Mutation.Status != "request_result_unknown" || unknown.Data.Mutation.Receipt.StopReason != "transport_failed" ||
		unknown.Data.Writes.DownloaderRequests != 1 || fixture.server.stop.Load() != 1 {
		t.Fatalf("unexpected unknown result: %#v", unknown.Data)
	}

	reader := &clientStopTrackingReader{reader: strings.NewReader(clientAdoptPassword + "\n")}
	resumeArgs := append([]string{"client", "stop", "resume"}, base...)
	resumeArgs = append(resumeArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, unknown.Data.Operation.ID)
	blocked := runClientStopJSON(t, resumeArgs, reader, 4)
	if !reader.read || fixture.server.stop.Load() != 1 || blocked.Data.Outcome != clientstop.OutcomeRequestUnknown || blocked.Data.Client.RequestsMade != 3 {
		t.Fatalf("unacknowledged resume read=%t stops=%d report=%#v", reader.read, fixture.server.stop.Load(), blocked.Data)
	}

	fixture.server.setStopMode("")
	repeatArgs := append([]string(nil), resumeArgs[:len(resumeArgs)-1]...)
	repeatArgs = append(repeatArgs, "--acknowledge-client-stop", "--acknowledge-repeat-stop", unknown.Data.Operation.ID)
	repeated := runClientStopJSON(t, repeatArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if repeated.Data.Outcome != clientstop.OutcomeStopped || repeated.Data.Operation.Resumable ||
		repeated.Data.Mutation.Status != "accepted" || repeated.Data.Stopped.Status != "exact_typed_job_stopped" ||
		repeated.Data.Writes.DownloaderRequests != 1 || repeated.Data.Client.RequestsMade != 6 || fixture.server.stop.Load() != 2 {
		t.Fatalf("unexpected acknowledged repeat: %#v", repeated.Data)
	}
}

func TestClientStopUnknownResponseCanProveUnattributedStoppedState(t *testing.T) {
	fixture := prepareClientStopCLIFixture(t)
	base := clientStopBaseArgs(fixture)
	planned := runClientStopJSON(t, append([]string{"client", "stop", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	fixture.server.setStopMode("unknown_job_stopped")
	runArgs := append([]string{"client", "stop", "run"}, base...)
	runArgs = append(runArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-client-stop")
	report := runClientStopJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if report.Data.Outcome != clientstop.OutcomeStoppedUnattributed || report.Data.Operation.Resumable ||
		report.Data.Mutation.Status != "request_result_unknown" || report.Data.Mutation.Receipt.Complete ||
		report.Data.Stopped.Status != "exact_typed_job_stopped" || report.Data.Final.BytesVerified != fixture.materialized.meta.TotalLength ||
		report.Data.Assurance.CompletionBasis != "exact_stopped_after_unknown_attempt_causality_unproven" ||
		len(report.Data.Warnings) != 1 || !strings.Contains(report.Data.Warnings[0], "causality is not attributed") || fixture.server.stop.Load() != 1 {
		t.Fatalf("unexpected unattributed completion: %#v", report.Data)
	}
}

func TestClientStopResumeRecoversPendingResponseBeforeObservingStoppedState(t *testing.T) {
	fixture := prepareClientStopCLIFixture(t)
	base := clientStopBaseArgs(fixture)
	planned := runClientStopJSON(t, append([]string{"client", "stop", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	fixture.server.setStopMode("unknown_keep_started")
	runArgs := append([]string{"client", "stop", "run"}, base...)
	runArgs = append(runArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-client-stop")
	unknown := runClientStopJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 4)

	operationDirectory := filepath.Join(fixture.materialized.materialize.targetRoot,
		".ptctl-client-stop-"+strings.TrimPrefix(unknown.Data.Operation.ID, "sha256:"))
	response := filepath.Join(operationDirectory, "response-01.json")
	pending := filepath.Join(operationDirectory, "scratch", "response-01.json.pending")
	if err := os.Rename(response, pending); err != nil {
		t.Fatal(err)
	}
	statusArgs := []string{"client", "stop", "status", "--target", fixture.materialized.materialize.targetRoot,
		"--expect-stop-plan-id", planned.Data.Plan.ID, "--output", "json", unknown.Data.Operation.ID}
	requestsBeforeStatus := fixture.server.totalRequests()
	status := runClientStopJSON(t, statusArgs, &trackingReader{}, 4)
	if status.Data.Operation.Status != "recovery_required" || status.Data.Operation.Phase != "marker_recovery_required" ||
		!status.Data.Operation.Resumable || fixture.server.totalRequests() != requestsBeforeStatus {
		t.Fatalf("pending response status=%#v", status.Data)
	}

	fixture.server.setState("stoppedUP", 1)
	resumeArgs := append([]string{"client", "stop", "resume"}, base...)
	resumeArgs = append(resumeArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, unknown.Data.Operation.ID)
	recovered := runClientStopJSON(t, resumeArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if recovered.Data.Outcome != clientstop.OutcomeStoppedUnattributed || recovered.Data.Operation.Resumable ||
		recovered.Data.Assurance.CompletionBasis != "exact_stopped_after_unknown_attempt_causality_unproven" ||
		recovered.Data.Writes.DownloaderRequests != 0 || fixture.server.stop.Load() != 1 {
		t.Fatalf("pending response recovery=%#v", recovered.Data)
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("pending response remains after recovery: %v", err)
	}
}

func TestClientStopAcceptedResponseWithoutStoppedStateRemainsBlocked(t *testing.T) {
	fixture := prepareClientStopCLIFixture(t)
	base := clientStopBaseArgs(fixture)
	planned := runClientStopJSON(t, append([]string{"client", "stop", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	fixture.server.setStopMode("accepted_keep_started")
	runArgs := append([]string{"client", "stop", "run"}, base...)
	runArgs = append(runArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-client-stop")
	report := runClientStopJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 4)
	if report.Data.Outcome != clientstop.OutcomeBlocked || !report.Data.Operation.Resumable ||
		report.Data.Mutation.Status != "accepted" || !report.Data.Mutation.Receipt.Complete ||
		report.Data.Stopped.Status != "not_proven" || report.Data.Final.BytesVerified != 0 || fixture.server.stop.Load() != 1 {
		t.Fatalf("accepted response hid a still-started job: %#v", report.Data)
	}
	fixture.server.setState("stoppedUP", 1)
	resumeArgs := append([]string{"client", "stop", "resume"}, base...)
	resumeArgs = append(resumeArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, report.Data.Operation.ID)
	resumed := runClientStopJSON(t, resumeArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if resumed.Data.Outcome != clientstop.OutcomeStoppedUnattributed || resumed.Data.Assurance.CompletionBasis != "exact_stopped_after_unknown_attempt_causality_unproven" ||
		resumed.Data.Mutation.Receipt.Complete != true || fixture.server.stop.Load() != 1 {
		t.Fatalf("cross-session stopped proof claimed response causality: %#v", resumed.Data)
	}
}

func TestClientStopAcceptedResponseCannotHideFinalContentDamage(t *testing.T) {
	fixture := prepareClientStopCLIFixture(t)
	base := clientStopBaseArgs(fixture)
	planned := runClientStopJSON(t, append([]string{"client", "stop", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	fixture.server.setStopHook(func() {
		if err := os.WriteFile(fixture.materialized.materialize.finalPath, []byte("tampered-after-stop"), 0o600); err != nil {
			t.Errorf("tamper final: %v", err)
		}
	})
	runArgs := append([]string{"client", "stop", "run"}, base...)
	runArgs = append(runArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-client-stop")
	report := runClientStopJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 3)
	if report.Data.Outcome != clientstop.OutcomeIntegrityFailed || report.Data.Operation.Resumable ||
		report.Data.Mutation.Status != "accepted" || !report.Data.Mutation.Receipt.Complete ||
		report.Data.Stopped.Status != "not_proven" || report.Data.Final.BytesVerified != 0 || fixture.server.stop.Load() != 1 {
		t.Fatalf("damaged final was not kept separate from stop response: %#v", report.Data)
	}
}

func TestClientStopBadUsageDoesNotReadPassword(t *testing.T) {
	planID := strings.Repeat("a", 24)
	operation := clientstop.OperationIDForPlan(planID).String()
	for _, args := range [][]string{
		{"client", "stop", "run", "--password-stdin", "--expect-stop-plan-id", planID, "--acknowledge-client-stop"},
		{"client", "stop", "resume", "--password-stdin", "--expect-stop-plan-id", planID, "--acknowledge-repeat-stop", operation},
		{"client", "stop", "status", "--target", `C:\not-opened`, "--expect-stop-plan-id", strings.Repeat("b", 24), operation},
		{"client", "stop", "prune", "--target", `C:\not-opened`, "--expect-stop-plan-id", planID, operation},
		{"client", "stop", "forget", "--target", `C:\not-opened`, "--expect-stop-plan-id", planID, operation},
	} {
		reader := &trackingReader{}
		var out, errOut bytes.Buffer
		if code := Run(args, reader, &out, &errOut); code != 2 || reader.read {
			t.Fatalf("args=%v code=%d read=%t stdout=%q stderr=%q", args, code, reader.read, out.String(), errOut.String())
		}
	}
}

func TestClientStopPruneAndForgetAreCredentialFreeLocalTransitions(t *testing.T) {
	fixture := prepareClientStopCLIFixture(t)
	base := clientStopBaseArgs(fixture)
	planned := runClientStopJSON(t, append([]string{"client", "stop", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	runArgs := append([]string{"client", "stop", "run"}, base...)
	runArgs = append(runArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-client-stop")
	stopped := runClientStopJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	requests := fixture.server.totalRequests()

	pruneArgs := []string{"client", "stop", "prune", "--target", fixture.materialized.materialize.targetRoot,
		"--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-operation-state-deletion", "--output", "json", stopped.Data.Operation.ID}
	reader := &trackingReader{}
	prunedRaw := runClientStopRaw(t, pruneArgs, reader, 0)
	var pruned clientStopRetentionJSONEnvelope
	if err := json.Unmarshal(prunedRaw, &pruned); err != nil {
		t.Fatal(err)
	}
	if reader.read || fixture.server.totalRequests() != requests || pruned.Kind != "client.stop.retention" ||
		pruned.Data.Outcome != clientstop.RetentionOutcomePruned || !pruned.Data.Markers.ExactTombstone ||
		pruned.Data.Proof.AttemptsRecorded != 1 || pruned.Data.Proof.ResponsesRecorded != 1 {
		t.Fatalf("pruned=%#v read=%t requests=%d/%d", pruned.Data, reader.read, fixture.server.totalRequests(), requests)
	}
	assertClientStopPrivate(t, prunedRaw, fixture)

	statusArgs := []string{"client", "stop", "status", "--target", fixture.materialized.materialize.targetRoot,
		"--expect-stop-plan-id", planned.Data.Plan.ID, "--output", "json", stopped.Data.Operation.ID}
	status := runClientStopJSON(t, statusArgs, &trackingReader{}, 0)
	if status.Data.Outcome != clientstop.OutcomeHistoricalRetained || !status.Data.Retention.CompletionDurable || status.Data.Operation.Resumable {
		t.Fatalf("retained status=%#v", status.Data)
	}
	resumeArgs := append([]string{"client", "stop", "resume"}, base...)
	resumeArgs = append(resumeArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, stopped.Data.Operation.ID)
	reader = &trackingReader{}
	blocked := runClientStopJSON(t, resumeArgs, reader, 4)
	if reader.read || fixture.server.totalRequests() != requests || blocked.Data.Operation.Status != "retained" {
		t.Fatalf("retained resume read=%t requests=%d/%d report=%#v", reader.read, fixture.server.totalRequests(), requests, blocked.Data)
	}
	runAgainArgs := append([]string{"client", "stop", "run"}, base...)
	runAgainArgs = append(runAgainArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-client-stop")
	reader = &trackingReader{}
	blockedRun := runClientStopJSON(t, runAgainArgs, reader, 4)
	if reader.read || fixture.server.totalRequests() != requests || blockedRun.Data.Operation.Status != "retained" {
		t.Fatalf("retained run read=%t requests=%d/%d report=%#v", reader.read, fixture.server.totalRequests(), requests, blockedRun.Data)
	}

	forgetArgs := []string{"client", "stop", "forget", "--target", fixture.materialized.materialize.targetRoot,
		"--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-historical-evidence-deletion", "--output", "json", stopped.Data.Operation.ID}
	reader = &trackingReader{}
	forgottenRaw := runClientStopRaw(t, forgetArgs, reader, 0)
	var forgotten clientStopForgetJSONEnvelope
	if err := json.Unmarshal(forgottenRaw, &forgotten); err != nil {
		t.Fatal(err)
	}
	if reader.read || fixture.server.totalRequests() != requests || forgotten.Kind != "client.stop.forget" ||
		forgotten.Data.Outcome != clientstop.ForgetOutcomeForgotten || !forgotten.Data.Authority.TargetHistoricalEvidenceErased ||
		forgotten.Data.Authority.ExactTombstoneEvidenceAvailable {
		t.Fatalf("forgotten=%#v read=%t requests=%d/%d", forgotten.Data, reader.read, fixture.server.totalRequests(), requests)
	}
	assertClientStopPrivate(t, forgottenRaw, fixture)

	reader = &trackingReader{}
	repeatedRaw := runClientStopRaw(t, forgetArgs, reader, 1)
	var repeated clientStopForgetJSONEnvelope
	if err := json.Unmarshal(repeatedRaw, &repeated); err != nil {
		t.Fatal(err)
	}
	if reader.read || fixture.server.totalRequests() != requests || repeated.Data.Outcome != clientstop.ForgetOutcomeAbsentUnattributed ||
		repeated.Data.WritesPerformed != 0 {
		t.Fatalf("repeated forget=%#v read=%t requests=%d/%d", repeated.Data, reader.read, fixture.server.totalRequests(), requests)
	}
	assertClientStopPrivate(t, repeatedRaw, fixture)
}

func TestTransmissionClientStopUsesExactV1RPCAndStoppedProof(t *testing.T) {
	fixture := prepareTransmissionClientStopCLIFixture(t)
	base := transmissionClientStopBaseArgs(fixture)
	planned := runClientStopJSON(t, append([]string{"client", "stop", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	if planned.Data.Outcome != clientstop.OutcomeReady || planned.Data.Plan.Data.Driver != "transmission" ||
		planned.Data.Plan.Data.Stop.Protocol != "transmission_rpc_v6" || planned.Data.Plan.Data.Stop.StopRouteID != "transmission.torrent.stop.v1" ||
		planned.Data.Plan.Data.InfoHashV1 == "" || planned.Data.Plan.Data.InfoHashV2 != "" || planned.Data.Client.RequestsMade != 3 {
		t.Fatalf("unexpected Transmission stop plan: %#v", planned.Data)
	}
	runArgs := append([]string{"client", "stop", "run"}, base...)
	runArgs = append(runArgs, "--expect-stop-plan-id", planned.Data.Plan.ID, "--acknowledge-client-stop")
	stopped := runClientStopJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if stopped.Data.Outcome != clientstop.OutcomeStopped || stopped.Data.Operation.Resumable ||
		stopped.Data.Mutation.Status != "accepted" || stopped.Data.Mutation.Receipt.RequestID <= 0 ||
		stopped.Data.Stopped.Observation.JobState != "stoppedUP" || stopped.Data.Client.RequestsMade != 6 || fixture.server.stop.Load() != 1 {
		t.Fatalf("unexpected Transmission stop result: %#v", stopped.Data)
	}
	assertJSONStringsExclude(t, mustJSON(t, stopped),
		fixture.materialized.materialize.targetRoot, fixture.materialized.materialize.sourceRoot,
		fixture.materialized.materialize.sourcePath, fixture.materialized.materialize.finalPath,
		fixture.materialized.materialize.torrentPath, fixture.materialized.storeRoot,
		clientAdoptRoot, clientAdoptRoot+"/"+materializeFinalName, materializeFinalName,
		fixture.server.server.URL, clientAdoptUser, clientAdoptPassword,
		"magnet:?xt=urn:btih:"+fixture.materialized.meta.InfoHashV1)

	reconcileArgs := append([]string{"reconcile", "report"}, base...)
	reconcileArgs = append(reconcileArgs, "--stop-operation", stopped.Data.Operation.ID, "--stop-plan-id", planned.Data.Plan.ID)
	requestsBefore := transmissionClientStopRequests(fixture.server)
	stopsBefore := fixture.server.stop.Load()
	var out, errOut bytes.Buffer
	if code := Run(reconcileArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("Transmission stop reconcile code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if delta := transmissionClientStopRequests(fixture.server) - requestsBefore; delta != 4 {
		t.Fatalf("Transmission stop reconciliation made %d requests; wanted the existing 409 handshake, version read, and two-read bracket", delta)
	}
	if fixture.server.stop.Load() != stopsBefore {
		t.Fatal("read-only Transmission stop reconciliation repeated the stop mutation")
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	stop := response.Data.Ledgers.Stop
	if response.Data.Outcome != "consistent" || response.Data.Ledgers.Downloader.Driver != "transmission" ||
		response.Data.Ledgers.Downloader.RequestsMade != 4 || stop.Status != "historical_stop_current_job_stopped" ||
		!stop.ProcessLocalCompletionProof || !stop.ProcessLocalCurrentProof || stop.CurrentStopped == nil ||
		stop.CurrentStopped.JobState != "stoppedUP" || len(response.Data.Relations) != 5 {
		t.Fatalf("Transmission stop history did not reconcile against the current stopped job: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(),
		fixture.materialized.materialize.targetRoot, fixture.materialized.materialize.sourceRoot,
		fixture.materialized.materialize.sourcePath, fixture.materialized.materialize.finalPath,
		fixture.materialized.materialize.torrentPath, fixture.materialized.storeRoot,
		clientAdoptRoot, fixture.server.server.URL, clientAdoptUser, clientAdoptPassword,
		"magnet:?xt=urn:btih:"+fixture.materialized.meta.InfoHashV1)
}

func transmissionClientStopRequests(server *transmissionAdoptServer) int32 {
	return server.handshake.Load() + server.session.Load() + server.ledger.Load() + server.add.Load() +
		server.verify.Load() + server.start.Load() + server.stop.Load()
}

func prepareClientStopCLIFixture(t *testing.T) clientRemoveCLIFixture {
	t.Helper()
	fixture := prepareClientRemoveCLIFixture(t)
	fixture.server.setState("uploading", 1)
	return fixture
}

func prepareTransmissionClientStopCLIFixture(t *testing.T) transmissionClientStopCLIFixture {
	t.Helper()
	fixture := newClientAdoptCLIFixture(t)
	server := newTransmissionAdoptServer(t, fixture.meta, fixture.raw)
	t.Cleanup(server.server.Close)
	baseAdopt := transmissionClientAdoptBaseArgs(fixture, server.server.URL)
	adoptionPlan := runClientAdoptForRemoval(t, append([]string{"client", "adopt", "plan"}, baseAdopt...))
	adoptionRun := append([]string{"client", "adopt", "run"}, baseAdopt...)
	adoptionRun = append(adoptionRun, "--expect-adoption-plan-id", adoptionPlan.Plan.ID, "--acknowledge-client-add")
	adopted := runClientAdoptForRemoval(t, adoptionRun)
	if adopted.Outcome != clientadopt.OutcomeAdoptedPendingRecheck {
		t.Fatalf("Transmission adoption did not complete: %#v", adopted)
	}
	baseActivation := transmissionClientActivateBaseArgs(fixture, server.server.URL, adopted.Operation.ID, adopted.Plan.ID)
	activationPlan := runClientActivateForRemoval(t, append([]string{"client", "activate", "plan"}, baseActivation...))
	activationRun := append([]string{"client", "activate", "run"}, baseActivation...)
	activationRun = append(activationRun, "--expect-activation-plan-id", activationPlan.Plan.ID, "--acknowledge-client-recheck")
	checking := runClientActivateForRemoval(t, activationRun)
	if checking.Outcome != clientactivate.OutcomeRecheckInProgress {
		t.Fatalf("Transmission activation did not enter checking: %#v", checking)
	}
	server.setState(0, 1)
	activationResume := append([]string{"client", "activate", "resume"}, baseActivation...)
	activationResume = append(activationResume, "--expect-activation-plan-id", activationPlan.Plan.ID, activationPlan.Operation.ID)
	activated := runClientActivateForRemoval(t, activationResume)
	if activated.Outcome != clientactivate.OutcomeCheckedStopped {
		t.Fatalf("Transmission activation did not complete: %#v", activated)
	}
	server.setState(6, 1)
	return transmissionClientStopCLIFixture{materialized: fixture, server: server, activation: activated}
}

func clientStopBaseArgs(fixture clientRemoveCLIFixture) []string {
	return []string{
		"--metafile-store", fixture.materialized.storeRoot, "--metafile-variant", fixture.materialized.variantID,
		"--target", fixture.materialized.materialize.targetRoot, "--materialize-operation", fixture.materialized.operation,
		"--materialize-plan-id", fixture.materialized.materialize.planID,
		"--activation-operation", fixture.activation.Operation.ID, "--activation-plan-id", fixture.activation.Plan.ID,
		"--host-root", fixture.materialized.materialize.targetRoot, "--client-root", clientAdoptRoot, "--client-style", "posix",
		"--driver", "qbittorrent", "--url", fixture.server.server.URL, "--username", clientAdoptUser,
		"--password-stdin", "--timeout", "1m", "--output", "json",
	}
}

func transmissionClientStopBaseArgs(fixture transmissionClientStopCLIFixture) []string {
	return []string{
		"--metafile-store", fixture.materialized.storeRoot, "--metafile-variant", fixture.materialized.variantID,
		"--target", fixture.materialized.materialize.targetRoot, "--materialize-operation", fixture.materialized.operation,
		"--materialize-plan-id", fixture.materialized.materialize.planID,
		"--activation-operation", fixture.activation.Operation.ID, "--activation-plan-id", fixture.activation.Plan.ID,
		"--host-root", fixture.materialized.materialize.targetRoot, "--client-root", clientAdoptRoot, "--client-style", "posix",
		"--driver", "transmission", "--url", fixture.server.server.URL, "--username", clientAdoptUser,
		"--password-stdin", "--timeout", "1m", "--output", "json",
	}
}

func runClientStopJSON(t *testing.T, args []string, stdin ioReader, expectedCode int) clientStopJSONEnvelope {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, stdin, &out, &errOut)
	if code != expectedCode {
		t.Fatalf("command=%v code=%d want=%d stdout=%q stderr=%q", args[:3], code, expectedCode, out.String(), errOut.String())
	}
	var result clientStopJSONEnvelope
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode client stop report: %v\n%s\nstderr=%s", err, out.Bytes(), errOut.Bytes())
	}
	if result.Schema != "ptctl.dev/v1" || result.Kind != "client.stop" || result.Data.Blockers == nil || result.Data.Warnings == nil {
		t.Fatalf("unexpected client stop envelope: %s", out.Bytes())
	}
	return result
}

func runClientStopRaw(t *testing.T, args []string, stdin ioReader, expectedCode int) []byte {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := Run(args, stdin, &out, &errOut); code != expectedCode {
		t.Fatalf("command=%v code=%d want=%d stdout=%q stderr=%q", args[:3], code, expectedCode, out.String(), errOut.String())
	}
	return out.Bytes()
}

func assertClientStopPrivate(t *testing.T, raw []byte, fixture clientRemoveCLIFixture) {
	t.Helper()
	assertJSONStringsExclude(t, raw,
		fixture.materialized.materialize.targetRoot, fixture.materialized.materialize.sourceRoot,
		fixture.materialized.materialize.sourcePath, fixture.materialized.materialize.finalPath,
		fixture.materialized.materialize.torrentPath, fixture.materialized.storeRoot,
		clientAdoptRoot, clientAdoptRoot+"/"+materializeFinalName, materializeFinalName,
		fixture.server.server.URL, clientAdoptUser, clientAdoptPassword, clientAdoptJobKey,
		"magnet:?xt=urn:btih:"+fixture.materialized.meta.InfoHashV1,
	)
}

func assertReconciledClientStopPrivate(t *testing.T, raw []byte, fixture clientRemoveCLIFixture) {
	t.Helper()
	assertJSONStringsExclude(t, raw,
		fixture.materialized.materialize.targetRoot, fixture.materialized.materialize.sourceRoot,
		fixture.materialized.materialize.sourcePath, fixture.materialized.materialize.finalPath,
		fixture.materialized.materialize.torrentPath, fixture.materialized.storeRoot,
		clientAdoptRoot, fixture.server.server.URL, clientAdoptUser, clientAdoptPassword, clientAdoptJobKey,
		"magnet:?xt=urn:btih:"+fixture.materialized.meta.InfoHashV1,
	)
}
