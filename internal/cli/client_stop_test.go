package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/clientstop"
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
	} {
		reader := &trackingReader{}
		var out, errOut bytes.Buffer
		if code := Run(args, reader, &out, &errOut); code != 2 || reader.read {
			t.Fatalf("args=%v code=%d read=%t stdout=%q stderr=%q", args, code, reader.read, out.String(), errOut.String())
		}
	}
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
