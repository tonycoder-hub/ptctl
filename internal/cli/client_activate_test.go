package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/clientstop"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
)

type clientActivateJSONEnvelope struct {
	Schema string                `json:"schema"`
	Kind   string                `json:"kind"`
	Data   clientactivate.Report `json:"data"`
}

type clientActivateRetentionJSONEnvelope struct {
	Schema string                         `json:"schema"`
	Kind   string                         `json:"kind"`
	Data   clientactivate.RetentionReport `json:"data"`
}

type clientActivateForgetJSONEnvelope struct {
	Schema string                      `json:"schema"`
	Kind   string                      `json:"kind"`
	Data   clientactivate.ForgetReport `json:"data"`
}

type clientActivateServer struct {
	server *httptest.Server
	meta   *metafile.MetaInfo
	raw    []byte
	t      *testing.T

	mu       sync.Mutex
	state    string
	progress float64
	added    bool

	login      atomic.Int32
	version    atomic.Int32
	ledger     atomic.Int32
	recheck    atomic.Int32
	start      atomic.Int32
	add        atomic.Int32
	stop       atomic.Int32
	remove     atomic.Int32
	stopMode   string
	stopHook   func()
	removeMode string
	removeHook func()
}

func TestClientActivatePlanRunResumeStatusAndPrivacy(t *testing.T) {
	fixture := newClientAdoptCLIFixture(t)
	activationServer := newClientActivateServer(t, fixture.meta, fixture.raw)
	defer activationServer.server.Close()
	adoptionBase := clientAdoptBaseArgs(fixture, activationServer.server.URL)
	var out, errOut bytes.Buffer
	if code := Run(append([]string{"client", "adopt", "plan"}, adoptionBase...), strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("adoption plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	adoptionPlan := decodeClientAdoptReport(t, out.Bytes())
	out.Reset()
	errOut.Reset()
	adoptionRun := append([]string{"client", "adopt", "run"}, adoptionBase...)
	adoptionRun = append(adoptionRun, "--expect-adoption-plan-id", adoptionPlan.Data.Plan.ID, "--acknowledge-client-add")
	if code := Run(adoptionRun, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("adoption run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	adopted := decodeClientAdoptReport(t, out.Bytes())
	if adopted.Data.Outcome != clientadopt.OutcomeAdoptedPendingRecheck {
		t.Fatalf("adoption did not complete: %s", out.String())
	}

	base := clientActivateBaseArgs(fixture, activationServer.server.URL, adopted.Data.Operation.ID, adopted.Data.Plan.ID)
	out.Reset()
	errOut.Reset()
	planArgs := append([]string{"client", "activate", "plan"}, base...)
	planArgs = append(planArgs, "--start-after-recheck")
	if code := Run(planArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("activation plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	planned := decodeClientActivateReport(t, out.Bytes())
	if planned.Data.Outcome != clientactivate.OutcomeReady || planned.Data.Plan.Action != clientactivate.ActionRecheckThenStart ||
		planned.Data.Plan.Control.Protocol != "qbittorrent_webapi_v5" || planned.Data.WritesPerformed != 0 || planned.Data.Client.RequestsMade != 3 {
		t.Fatalf("unexpected activation plan: %s", out.String())
	}
	assertClientActivatePrivate(t, out.Bytes(), fixture, activationServer.server.URL)

	out.Reset()
	errOut.Reset()
	runArgs := append([]string{"client", "activate", "run"}, base...)
	runArgs = append(runArgs, "--start-after-recheck", "--expect-activation-plan-id", planned.Data.Plan.ID, "--acknowledge-client-recheck")
	if code := Run(runArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("activation run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	run := decodeClientActivateReport(t, out.Bytes())
	if run.Data.Outcome != clientactivate.OutcomeRecheckInProgress || run.Data.Operation.ID != planned.Data.Operation.ID ||
		run.Data.Journal.RecheckAttempts != 1 || !run.Data.Journal.RecheckStartedDurable || run.Data.Client.ActionAttempted != "recheck" ||
		!run.Data.Client.ActionReceipt.Complete || run.Data.Client.ActionReceipt.AutomaticRetries != 0 || run.Data.Client.ActionReceipt.RedirectsFollowed != 0 {
		t.Fatalf("unexpected activation run: %s", out.String())
	}
	assertClientActivatePrivate(t, out.Bytes(), fixture, activationServer.server.URL)

	activationServer.setState("stoppedUP", 1)
	out.Reset()
	errOut.Reset()
	resumeArgs := append([]string{"client", "activate", "resume"}, base...)
	resumeArgs = append(resumeArgs, "--start-after-recheck", "--expect-activation-plan-id", planned.Data.Plan.ID,
		"--acknowledge-client-start", planned.Data.Operation.ID)
	if code := Run(resumeArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("activation resume code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	resumed := decodeClientActivateReport(t, out.Bytes())
	if resumed.Data.Outcome != clientactivate.OutcomeStartedClientClaim || resumed.Data.Operation.Resumable ||
		!resumed.Data.Journal.RecheckCompletionDurable || resumed.Data.Journal.StartAttempts != 1 ||
		!resumed.Data.Journal.ActivationCompletionDurable || resumed.Data.Client.ActionAttempted != "start" {
		t.Fatalf("unexpected activation resume: %s", out.String())
	}
	assertClientActivatePrivate(t, out.Bytes(), fixture, activationServer.server.URL)

	requestsBeforeStatus := activationServer.totalRequests()
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"client", "activate", "status", "--target", fixture.materialize.targetRoot, "--output", "json", planned.Data.Operation.ID},
		strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("activation status code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	status := decodeClientActivateReport(t, out.Bytes())
	if status.Data.Outcome != clientactivate.OutcomeHistoricalStarted || status.Data.WritesPerformed != 0 ||
		status.Data.Client.RequestsMade != 0 || status.Data.Operation.Resumable || activationServer.totalRequests() != requestsBeforeStatus {
		t.Fatalf("unexpected activation status: %s", out.String())
	}
	assertClientActivatePrivate(t, out.Bytes(), fixture, activationServer.server.URL)

	requestsBeforePrune := activationServer.totalRequests()
	reader := &trackingReader{}
	out.Reset()
	errOut.Reset()
	pruneArgs := []string{"client", "activate", "prune", "--target", fixture.materialize.targetRoot,
		"--expect-activation-plan-id", planned.Data.Plan.ID, "--acknowledge-operation-state-deletion", "--output", "json", planned.Data.Operation.ID}
	if code := Run(pruneArgs, reader, &out, &errOut); code != 0 || reader.read || activationServer.totalRequests() != requestsBeforePrune {
		t.Fatalf("activation prune code=%d read=%t requests=%d stdout=%q stderr=%q", code, reader.read,
			activationServer.totalRequests()-requestsBeforePrune, out.String(), errOut.String())
	}
	retained := decodeClientActivateRetentionReport(t, out.Bytes())
	if retained.Data.Outcome != clientactivate.RetentionOutcomePruned || !retained.Data.Markers.ExactTombstone || retained.Data.WritesPerformed == 0 {
		t.Fatalf("activation retention=%#v", retained.Data)
	}
	assertClientActivatePrivate(t, out.Bytes(), fixture, activationServer.server.URL)

	reader = &trackingReader{}
	out.Reset()
	errOut.Reset()
	retainedResume := append([]string{"client", "activate", "resume"}, base...)
	retainedResume = append(retainedResume, "--start-after-recheck", "--expect-activation-plan-id", planned.Data.Plan.ID, planned.Data.Operation.ID)
	if code := Run(retainedResume, reader, &out, &errOut); code != 0 || reader.read || activationServer.totalRequests() != requestsBeforePrune {
		t.Fatalf("retained activation resume code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
	retainedStatus := decodeClientActivateReport(t, out.Bytes())
	if retainedStatus.Data.Outcome != clientactivate.OutcomeHistoricalStarted || retainedStatus.Data.Operation.Status != "retained" ||
		retainedStatus.Data.Journal.RetentionState != "complete" {
		t.Fatalf("retained activation resume=%#v", retainedStatus.Data)
	}
	reader = &trackingReader{}
	out.Reset()
	errOut.Reset()
	mismatchedRetainedResume := append([]string{"client", "activate", "resume"}, base...)
	mismatchedRetainedResume = append(mismatchedRetainedResume, "--expect-activation-plan-id", planned.Data.Plan.ID, planned.Data.Operation.ID)
	if code := Run(mismatchedRetainedResume, reader, &out, &errOut); code != 4 || reader.read || activationServer.totalRequests() != requestsBeforePrune {
		t.Fatalf("mismatched retained resume code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
	forgetArgs := []string{"client", "activate", "forget", "--target", fixture.materialize.targetRoot,
		"--expect-activation-plan-id", planned.Data.Plan.ID, "--acknowledge-historical-evidence-deletion", "--output", "json", planned.Data.Operation.ID}
	reader = &trackingReader{}
	out.Reset()
	errOut.Reset()
	if code := Run(forgetArgs, reader, &out, &errOut); code != 0 || reader.read || activationServer.totalRequests() != requestsBeforePrune {
		t.Fatalf("activation forget code=%d read=%t requests=%d stdout=%q stderr=%q", code, reader.read,
			activationServer.totalRequests()-requestsBeforePrune, out.String(), errOut.String())
	}
	forgotten := decodeClientActivateForgetReport(t, out.Bytes())
	if forgotten.Data.Outcome != clientactivate.ForgetOutcomeForgotten || !forgotten.Data.Authority.TargetHistoricalEvidenceErased ||
		forgotten.Data.Authority.MarkerDurable || forgotten.Data.Operation.Resumable || forgotten.Data.WritesPerformed == 0 ||
		forgotten.Data.Blockers == nil || forgotten.Data.Issues == nil || forgotten.Data.Warnings == nil {
		t.Fatalf("unexpected activation forget report: %s", out.String())
	}
	assertClientActivatePrivate(t, out.Bytes(), fixture, activationServer.server.URL)
	reader = &trackingReader{}
	out.Reset()
	errOut.Reset()
	if code := Run(forgetArgs, reader, &out, &errOut); code != 1 || reader.read || activationServer.totalRequests() != requestsBeforePrune {
		t.Fatalf("repeated activation forget code=%d read=%t requests=%d stdout=%q stderr=%q", code, reader.read,
			activationServer.totalRequests()-requestsBeforePrune, out.String(), errOut.String())
	}
	absent := decodeClientActivateForgetReport(t, out.Bytes())
	if absent.Data.Outcome != clientactivate.ForgetOutcomeAbsentUnattributed || absent.Data.WritesPerformed != 0 || absent.Data.Authority.TargetHistoricalEvidenceErased {
		t.Fatalf("repeated activation forget claimed historical idempotence: %s", out.String())
	}
	assertClientActivatePrivate(t, out.Bytes(), fixture, activationServer.server.URL)
	if activationServer.login.Load() != 5 || activationServer.version.Load() != 3 || activationServer.ledger.Load() != 8 ||
		activationServer.add.Load() != 1 || activationServer.recheck.Load() != 1 || activationServer.start.Load() != 1 {
		t.Fatalf("requests login=%d version=%d ledger=%d add=%d recheck=%d start=%d", activationServer.login.Load(), activationServer.version.Load(),
			activationServer.ledger.Load(), activationServer.add.Load(), activationServer.recheck.Load(), activationServer.start.Load())
	}
}

func TestClientActivateStartsOnlyFromAttributedTerminalStop(t *testing.T) {
	fixture := prepareClientStopCLIFixture(t)
	stopBase := clientStopBaseArgs(fixture)
	stopPlan := runClientStopJSON(t, append([]string{"client", "stop", "plan"}, stopBase...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	stopRun := append([]string{"client", "stop", "run"}, stopBase...)
	stopRun = append(stopRun, "--expect-stop-plan-id", stopPlan.Data.Plan.ID, "--acknowledge-client-stop")
	stopped := runClientStopJSON(t, stopRun, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if stopped.Data.Outcome != clientstop.OutcomeStopped || fixture.server.stop.Load() != 1 {
		t.Fatalf("terminal stop=%#v", stopped.Data)
	}

	base := clientActivateAfterStopBaseArgs(fixture, stopped.Data.Operation.ID, stopPlan.Data.Plan.ID)
	requestsBefore := fixture.server.totalRequests()
	reader := &trackingReader{}
	var out, errOut bytes.Buffer
	badSelectorArgs := append([]string{"client", "activate", "plan"}, base...)
	badSelectorArgs = append(badSelectorArgs, "--adoption-operation=")
	if code := Run(badSelectorArgs, reader, &out, &errOut); code != 2 || reader.read || fixture.server.totalRequests() != requestsBefore {
		t.Fatalf("explicit empty lineage selector code=%d read=%t requests=%d stdout=%q stderr=%q", code, reader.read,
			fixture.server.totalRequests()-requestsBefore, out.String(), errOut.String())
	}

	requestsBefore = fixture.server.totalRequests()
	planned := runClientActivateJSON(t, append([]string{"client", "activate", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	if planned.Data.Outcome != clientactivate.OutcomeReady || planned.Data.Plan.Action != clientactivate.ActionStartAfterStop ||
		planned.Data.TerminalStop.Status != "canonical_attributed_completion_authority_available" ||
		planned.Data.TerminalStop.OperationID != stopped.Data.Operation.ID || planned.Data.TerminalStop.PlanID != stopPlan.Data.Plan.ID ||
		planned.Data.TerminalStop.Basis != "accepted_response_then_exact_stopped" || planned.Data.TerminalStop.AuthorityForm != "live_journal" ||
		planned.Data.Adoption.Status != "historical_prior_adoption_lineage_from_activation" ||
		planned.Data.WritesPerformed != 0 || fixture.server.totalRequests()-requestsBefore != 3 {
		t.Fatalf("start-after-stop plan=%#v", planned.Data)
	}
	assertClientActivatePrivate(t, mustJSON(t, planned), fixture.materialized, fixture.server.server.URL)

	requestsBefore = fixture.server.totalRequests()
	runArgs := append([]string{"client", "activate", "run"}, base...)
	runArgs = append(runArgs, "--expect-activation-plan-id", planned.Data.Plan.ID, "--acknowledge-client-start")
	started := runClientActivateJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if started.Data.Outcome != clientactivate.OutcomeStartedClientClaim || started.Data.Operation.Resumable ||
		started.Data.Plan.Action != clientactivate.ActionStartAfterStop || started.Data.Journal.RecheckAttempts != 0 ||
		started.Data.Journal.RecheckCompletionDurable || started.Data.Journal.StartAttempts != 1 ||
		!started.Data.Journal.ActivationCompletionDurable || started.Data.Client.ActionAttempted != "start" ||
		started.Data.Client.ActionReceipt.Effect != downloader.ControlEffectStart || fixture.server.recheck.Load() != 1 ||
		fixture.server.start.Load() != 1 || fixture.server.totalRequests()-requestsBefore != 5 {
		t.Fatalf("start-after-stop run=%#v", started.Data)
	}
	assertClientActivatePrivate(t, mustJSON(t, started), fixture.materialized, fixture.server.server.URL)

	requestsBefore = fixture.server.totalRequests()
	status := runClientActivateJSON(t, []string{"client", "activate", "status", "--target", fixture.materialized.materialize.targetRoot,
		"--output", "json", started.Data.Operation.ID}, &trackingReader{}, 0)
	if status.Data.Outcome != clientactivate.OutcomeHistoricalStarted || status.Data.Plan.Action != clientactivate.ActionStartAfterStop ||
		status.Data.TerminalStop.Status != "historical_terminal_stop_reference_current_not_observed" ||
		status.Data.TerminalStop.AuthorityForm != "historical_plan_reference" || fixture.server.totalRequests() != requestsBefore {
		t.Fatalf("start-after-stop status=%#v", status.Data)
	}

	reconcileArgs := clientReconcileBaseArgsForActivation(fixture, started.Data.Operation.ID, planned.Data.Plan.ID)
	requestsBefore = fixture.server.totalRequests()
	out.Reset()
	errOut.Reset()
	if code := Run(reconcileArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("start-after-stop reconcile code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if delta := fixture.server.totalRequests() - requestsBefore; delta != 3 {
		t.Fatalf("start-after-stop reconcile requests=%d", delta)
	}
	var reconciled struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &reconciled); err != nil {
		t.Fatal(err)
	}
	activation := reconciled.Data.Ledgers.Activation
	if reconciled.Data.Outcome != "consistent" || !reconciled.Data.Scope.ClientActivationRequested ||
		activation.Status != "historical_completion_current_job_bound" || !activation.ProcessLocalCompletionProof ||
		!activation.ProcessLocalCurrentUseProof || activation.Completion == nil || activation.CurrentUse == nil ||
		activation.Completion.Action != clientactivate.ActionStartAfterStop ||
		activation.Completion.StopOperationID != stopped.Data.Operation.ID || activation.Completion.StopPlanID != stopPlan.Data.Plan.ID ||
		activation.Completion.StopCompletionID == "" || activation.Completion.StopCompletionBasis != "accepted_response_then_exact_stopped" ||
		activation.CurrentUse.JobState != "uploading" {
		t.Fatalf("start-after-stop reconcile=%s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(),
		fixture.materialized.materialize.targetRoot, fixture.materialized.materialize.sourceRoot,
		fixture.materialized.materialize.sourcePath, fixture.materialized.materialize.finalPath,
		fixture.materialized.materialize.torrentPath, fixture.materialized.storeRoot,
		clientAdoptRoot, clientAdoptRoot+"/"+materializeFinalName,
		fixture.server.server.URL, clientAdoptUser, clientAdoptPassword, clientAdoptJobKey,
		"magnet:?xt=urn:btih:"+fixture.materialized.meta.InfoHashV1)

	secondStopBase := clientStopBaseArgsForActivation(fixture, started.Data.Operation.ID, planned.Data.Plan.ID)
	requestsBefore = fixture.server.totalRequests()
	secondStopPlan := runClientStopJSON(t, append([]string{"client", "stop", "plan"}, secondStopBase...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	if fixture.server.totalRequests()-requestsBefore != 3 || secondStopPlan.Data.Plan.Data.ActivationOperationID != started.Data.Operation.ID ||
		secondStopPlan.Data.Plan.Data.ActivationPlanID != planned.Data.Plan.ID {
		t.Fatalf("second stop plan=%#v requests=%d", secondStopPlan.Data, fixture.server.totalRequests()-requestsBefore)
	}
	requestsBefore = fixture.server.totalRequests()
	secondStopArgs := append([]string{"client", "stop", "run"}, secondStopBase...)
	secondStopArgs = append(secondStopArgs, "--expect-stop-plan-id", secondStopPlan.Data.Plan.ID, "--acknowledge-client-stop")
	secondStopped := runClientStopJSON(t, secondStopArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if secondStopped.Data.Outcome != clientstop.OutcomeStopped || fixture.server.stop.Load() != 2 ||
		fixture.server.start.Load() != 1 || fixture.server.totalRequests()-requestsBefore != 6 {
		t.Fatalf("second stop=%#v requests=%d", secondStopped.Data, fixture.server.totalRequests()-requestsBefore)
	}

	pruneArgs := []string{"client", "activate", "prune", "--target", fixture.materialized.materialize.targetRoot,
		"--expect-activation-plan-id", planned.Data.Plan.ID, "--acknowledge-operation-state-deletion", "--output", "json", started.Data.Operation.ID}
	out.Reset()
	errOut.Reset()
	if code := Run(pruneArgs, &trackingReader{}, &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("start-after-stop prune code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	retained := decodeClientActivateRetentionReport(t, out.Bytes())
	if retained.Data.Outcome != clientactivate.RetentionOutcomePruned || !retained.Data.Markers.ExactTombstone ||
		retained.Data.Plan.Action != clientactivate.ActionStartAfterStop {
		t.Fatalf("start-after-stop retention=%#v", retained.Data)
	}
	stopPruneArgs := []string{"client", "stop", "prune", "--target", fixture.materialized.materialize.targetRoot,
		"--expect-stop-plan-id", secondStopPlan.Data.Plan.ID, "--acknowledge-operation-state-deletion", "--output", "json",
		secondStopped.Data.Operation.ID}
	var stopPruneOut, stopPruneErr bytes.Buffer
	if code := Run(stopPruneArgs, &trackingReader{}, &stopPruneOut, &stopPruneErr); code != 0 || stopPruneErr.Len() != 0 {
		t.Fatalf("second stop prune code=%d stdout=%q stderr=%q", code, stopPruneOut.String(), stopPruneErr.String())
	}
	var retainedStop clientStopRetentionJSONEnvelope
	if err := json.Unmarshal(stopPruneOut.Bytes(), &retainedStop); err != nil {
		t.Fatal(err)
	}
	if retainedStop.Data.Outcome != clientstop.RetentionOutcomePruned || !retainedStop.Data.Markers.ExactTombstone {
		t.Fatalf("second stop retention=%#v", retainedStop.Data)
	}

	requestsBefore = fixture.server.totalRequests()
	nextBase := clientActivateAfterStopBaseArgs(fixture, secondStopped.Data.Operation.ID, secondStopPlan.Data.Plan.ID)
	next := runClientActivateJSON(t, append([]string{"client", "activate", "plan"}, nextBase...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	if next.Data.Outcome != clientactivate.OutcomeReady || next.Data.Plan.Action != clientactivate.ActionStartAfterStop ||
		next.Data.TerminalStop.OperationID != secondStopped.Data.Operation.ID || next.Data.TerminalStop.PlanID != secondStopPlan.Data.Plan.ID ||
		next.Data.TerminalStop.AuthorityForm != "retained_tombstone" || !next.Data.TerminalStop.Retained ||
		fixture.server.totalRequests()-requestsBefore != 3 ||
		fixture.server.start.Load() != 1 || fixture.server.stop.Load() != 2 {
		t.Fatalf("next start-after-stop plan=%#v requests=%d", next.Data, fixture.server.totalRequests()-requestsBefore)
	}
}

func TestTransmissionClientActivatePlanRunAndResume(t *testing.T) {
	fixture := newClientAdoptCLIFixture(t)
	server := newTransmissionAdoptServer(t, fixture.meta, fixture.raw)
	defer server.server.Close()

	adoptionBase := transmissionClientAdoptBaseArgs(fixture, server.server.URL)
	var out, errOut bytes.Buffer
	if code := Run(append([]string{"client", "adopt", "plan"}, adoptionBase...), strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("adoption plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	adoptionPlan := decodeClientAdoptReport(t, out.Bytes())
	out.Reset()
	errOut.Reset()
	adoptionRun := append([]string{"client", "adopt", "run"}, adoptionBase...)
	adoptionRun = append(adoptionRun, "--expect-adoption-plan-id", adoptionPlan.Data.Plan.ID, "--acknowledge-client-add")
	if code := Run(adoptionRun, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("adoption run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	adopted := decodeClientAdoptReport(t, out.Bytes())
	if adopted.Data.Outcome != clientadopt.OutcomeAdoptedPendingRecheck || adopted.Data.Plan.Driver != "transmission" {
		t.Fatalf("Transmission adoption did not complete: %s", out.String())
	}

	base := transmissionClientActivateBaseArgs(fixture, server.server.URL, adopted.Data.Operation.ID, adopted.Data.Plan.ID)
	out.Reset()
	errOut.Reset()
	planArgs := append([]string{"client", "activate", "plan"}, base...)
	planArgs = append(planArgs, "--start-after-recheck")
	if code := Run(planArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("activation plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	planned := decodeClientActivateReport(t, out.Bytes())
	if planned.Data.Outcome != clientactivate.OutcomeReady || planned.Data.Plan.Driver != "transmission" ||
		planned.Data.Plan.Control.Protocol != "transmission_rpc_v6" || planned.Data.Plan.Control.RecheckRouteID != "transmission.torrent.verify.v1" ||
		planned.Data.Plan.Control.StartRouteID != "transmission.torrent.start.v1" || planned.Data.Client.RequestsMade != 3 || planned.Data.WritesPerformed != 0 {
		t.Fatalf("unexpected Transmission activation plan: %s", out.String())
	}
	assertClientActivatePrivate(t, out.Bytes(), fixture, server.server.URL)

	out.Reset()
	errOut.Reset()
	runArgs := append([]string{"client", "activate", "run"}, base...)
	runArgs = append(runArgs, "--start-after-recheck", "--expect-activation-plan-id", planned.Data.Plan.ID, "--acknowledge-client-recheck")
	if code := Run(runArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("activation run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	run := decodeClientActivateReport(t, out.Bytes())
	if run.Data.Outcome != clientactivate.OutcomeRecheckInProgress || run.Data.Client.RequestsMade != 5 ||
		run.Data.Client.JobState != "checkingResumeData" || run.Data.Client.ActionAttempted != "recheck" ||
		!run.Data.Client.ActionReceipt.Complete || run.Data.Client.ActionReceipt.RequestID <= 0 ||
		run.Data.Client.ActionReceipt.RequestsAttempted != 1 || !run.Data.Journal.RecheckStartedDurable {
		t.Fatalf("unexpected Transmission activation run: %s", out.String())
	}
	assertClientActivatePrivate(t, out.Bytes(), fixture, server.server.URL)

	server.setState(0, 1)
	out.Reset()
	errOut.Reset()
	resumeArgs := append([]string{"client", "activate", "resume"}, base...)
	resumeArgs = append(resumeArgs, "--start-after-recheck", "--expect-activation-plan-id", planned.Data.Plan.ID,
		"--acknowledge-client-start", planned.Data.Operation.ID)
	if code := Run(resumeArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("activation resume code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	resumed := decodeClientActivateReport(t, out.Bytes())
	if resumed.Data.Outcome != clientactivate.OutcomeStartedClientClaim || resumed.Data.Client.RequestsMade != 5 ||
		resumed.Data.Client.JobState != "queuedUP" || resumed.Data.Client.ActionAttempted != "start" ||
		!resumed.Data.Client.ActionReceipt.Complete || resumed.Data.Client.ActionReceipt.RequestID <= 0 ||
		!resumed.Data.Journal.RecheckCompletionDurable || !resumed.Data.Journal.ActivationCompletionDurable || resumed.Data.Operation.Resumable {
		t.Fatalf("unexpected Transmission activation resume: %s", out.String())
	}
	assertClientActivatePrivate(t, out.Bytes(), fixture, server.server.URL)

	requestsBeforeStatus := server.handshake.Load() + server.session.Load() + server.ledger.Load() + server.add.Load() + server.verify.Load() + server.start.Load()
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"client", "activate", "status", "--target", fixture.materialize.targetRoot, "--output", "json", planned.Data.Operation.ID},
		strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("activation status code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	status := decodeClientActivateReport(t, out.Bytes())
	if status.Data.Outcome != clientactivate.OutcomeHistoricalStarted || status.Data.Client.RequestsMade != 0 ||
		server.handshake.Load()+server.session.Load()+server.ledger.Load()+server.add.Load()+server.verify.Load()+server.start.Load() != requestsBeforeStatus {
		t.Fatalf("unexpected Transmission activation status: %s", out.String())
	}
	if server.handshake.Load() != 5 || server.session.Load() != 5 || server.ledger.Load() != 8 ||
		server.add.Load() != 1 || server.verify.Load() != 1 || server.start.Load() != 1 {
		t.Fatalf("requests handshake=%d session=%d ledger=%d add=%d verify=%d start=%d", server.handshake.Load(), server.session.Load(),
			server.ledger.Load(), server.add.Load(), server.verify.Load(), server.start.Load())
	}
}

func decodeClientActivateRetentionReport(t *testing.T, raw []byte) clientActivateRetentionJSONEnvelope {
	t.Helper()
	var result clientActivateRetentionJSONEnvelope
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode client activation retention report: %v\n%s", err, raw)
	}
	if result.Schema != "ptctl.dev/v1" || result.Kind != "client.activation.retention" {
		t.Fatalf("unexpected client activation retention envelope: %s", raw)
	}
	return result
}

func decodeClientActivateForgetReport(t *testing.T, raw []byte) clientActivateForgetJSONEnvelope {
	t.Helper()
	var result clientActivateForgetJSONEnvelope
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode client activation forget report: %v\n%s", err, raw)
	}
	if result.Schema != "ptctl.dev/v1" || result.Kind != "client.activation.forget" {
		t.Fatalf("unexpected client activation forget envelope: %s", raw)
	}
	return result
}

func TestClientActivateBadUsageDoesNotReadPassword(t *testing.T) {
	planID := strings.Repeat("f", 24)
	operationID := clientactivate.OperationIDForPlan(planID).String()
	for _, args := range [][]string{
		{"client", "activate", "run", "--password-stdin", "--expect-activation-plan-id", planID, "--acknowledge-client-recheck"},
		{"client", "activate", "prune", "--target", `C:\not-opened`, "--expect-activation-plan-id", planID, operationID},
		{"client", "activate", "forget", "--target", `C:\not-opened`, "--expect-activation-plan-id", planID, operationID},
		{"client", "activate", "plan", "--stop-operation", "sha256:" + strings.Repeat("a", 64), "--stop-plan-id", planID,
			"--start-after-recheck", "--password-stdin"},
	} {
		reader := &trackingReader{}
		var out, errOut bytes.Buffer
		code := Run(args, reader, &out, &errOut)
		if code != 2 || reader.read {
			t.Fatalf("args=%v code=%d read=%t stdout=%q stderr=%q", args, code, reader.read, out.String(), errOut.String())
		}
	}
}

func clientActivateAfterStopBaseArgs(fixture clientRemoveCLIFixture, stopOperation, stopPlanID string) []string {
	return []string{
		"--metafile-store", fixture.materialized.storeRoot, "--metafile-variant", fixture.materialized.variantID,
		"--target", fixture.materialized.materialize.targetRoot, "--materialize-operation", fixture.materialized.operation,
		"--materialize-plan-id", fixture.materialized.materialize.planID, "--stop-operation", stopOperation,
		"--stop-plan-id", stopPlanID, "--host-root", fixture.materialized.materialize.targetRoot,
		"--client-root", clientAdoptRoot, "--client-style", "posix", "--driver", "qbittorrent",
		"--url", fixture.server.server.URL, "--username", clientAdoptUser, "--password-stdin", "--timeout", "1m", "--output", "json",
	}
}

func clientStopBaseArgsForActivation(fixture clientRemoveCLIFixture, activationOperation, activationPlanID string) []string {
	args := append([]string(nil), clientStopBaseArgs(fixture)...)
	for index := 0; index+1 < len(args); index++ {
		switch args[index] {
		case "--activation-operation":
			args[index+1] = activationOperation
		case "--activation-plan-id":
			args[index+1] = activationPlanID
		}
	}
	return args
}

func clientReconcileBaseArgsForActivation(fixture clientRemoveCLIFixture, activationOperation, activationPlanID string) []string {
	return []string{
		"reconcile", "report", "--metafile-store", fixture.materialized.storeRoot,
		"--metafile-variant", fixture.materialized.variantID, "--target", fixture.materialized.materialize.targetRoot,
		"--materialize-operation", fixture.materialized.operation, "--materialize-plan-id", fixture.materialized.materialize.planID,
		"--activation-operation", activationOperation, "--activation-plan-id", activationPlanID,
		"--host-root", fixture.materialized.materialize.targetRoot, "--client-root", clientAdoptRoot, "--client-style", "posix",
		"--driver", "qbittorrent", "--url", fixture.server.server.URL, "--username", clientAdoptUser,
		"--password-stdin", "--timeout", "1m", "--output", "json",
	}
}

func runClientActivateJSON(t *testing.T, args []string, stdin ioReader, expectedCode int) clientActivateJSONEnvelope {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := Run(args, stdin, &out, &errOut); code != expectedCode {
		t.Fatalf("command=%v code=%d want=%d stdout=%q stderr=%q", args[:3], code, expectedCode, out.String(), errOut.String())
	}
	return decodeClientActivateReport(t, out.Bytes())
}

func newClientActivateServer(t *testing.T, meta *metafile.MetaInfo, raw []byte) *clientActivateServer {
	t.Helper()
	result := &clientActivateServer{meta: meta, raw: append([]byte(nil), raw...), t: t, state: "stoppedDL", progress: 0}
	result.server = httptest.NewServer(http.HandlerFunc(result.serveHTTP))
	return result
}

func (server *clientActivateServer) setState(state string, progress float64) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.state, server.progress = state, progress
}

func (server *clientActivateServer) currentState() (string, float64) {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.state, server.progress
}

func (server *clientActivateServer) totalRequests() int32 {
	return server.login.Load() + server.version.Load() + server.ledger.Load() + server.add.Load() + server.recheck.Load() + server.start.Load() + server.stop.Load() + server.remove.Load()
}

func (server *clientActivateServer) setStopMode(mode string) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.stopMode = mode
}

func (server *clientActivateServer) setStopHook(hook func()) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.stopHook = hook
}

func (server *clientActivateServer) setRemoveMode(mode string) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.removeMode = mode
}

func (server *clientActivateServer) setRemoveHook(hook func()) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.removeHook = hook
}

func (server *clientActivateServer) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/api/v2/auth/login":
		server.login.Add(1)
		if request.Method != http.MethodPost || request.ParseForm() != nil || request.Form.Get("username") != clientAdoptUser || request.Form.Get("password") != clientAdoptPassword {
			server.t.Errorf("invalid activation login")
			http.Error(writer, "rejected", http.StatusForbidden)
			return
		}
		http.SetCookie(writer, &http.Cookie{Name: "SID", Value: "client-activate-session", Path: "/"})
		_, _ = writer.Write([]byte("Ok."))
	case "/api/v2/app/version":
		server.version.Add(1)
		_, _ = writer.Write([]byte("v5.0.4"))
	case "/api/v2/torrents/info":
		server.ledger.Add(1)
		state, progress := server.currentState()
		writer.Header().Set("Content-Type", "application/json")
		server.mu.Lock()
		added := server.added
		server.mu.Unlock()
		jobs := []map[string]any{}
		if added {
			jobs = append(jobs, map[string]any{
				"hash": clientAdoptJobKey, "magnet_uri": "magnet:?xt=urn:btih:" + server.meta.InfoHashV1,
				"name": materializeFinalName, "size": server.meta.TotalLength, "progress": progress, "state": state,
				"save_path": clientAdoptRoot, "content_path": clientAdoptRoot + "/" + materializeFinalName,
				"downloaded": int64(float64(server.meta.TotalLength) * progress), "uploaded": int64(0),
			})
		}
		_ = json.NewEncoder(writer).Encode(jobs)
	case "/api/v2/torrents/add":
		server.add.Add(1)
		if err := request.ParseMultipartForm(1 << 20); err != nil || request.FormValue("savepath") != clientAdoptRoot ||
			request.FormValue("paused") != "true" || request.FormValue("stopped") != "true" {
			server.t.Errorf("invalid adoption add request: %v", err)
			http.Error(writer, "invalid", http.StatusBadRequest)
			return
		}
		files := request.MultipartForm.File["torrents"]
		if len(files) != 1 {
			server.t.Errorf("missing adoption metafile")
			http.Error(writer, "invalid", http.StatusBadRequest)
			return
		}
		file, err := files[0].Open()
		if err != nil {
			server.t.Error(err)
			http.Error(writer, "invalid", http.StatusBadRequest)
			return
		}
		uploaded, readErr := io.ReadAll(file)
		_ = file.Close()
		if readErr != nil || !bytes.Equal(uploaded, server.raw) {
			server.t.Errorf("adoption metafile differed")
			http.Error(writer, "invalid", http.StatusBadRequest)
			return
		}
		server.mu.Lock()
		server.added = true
		server.mu.Unlock()
		_, _ = writer.Write([]byte("Ok."))
	case "/api/v2/torrents/recheck":
		server.recheck.Add(1)
		server.validateAction(writer, request)
		server.setState("checkingDL", 0)
	case "/api/v2/torrents/start":
		server.start.Add(1)
		server.validateAction(writer, request)
		server.setState("uploading", 1)
	case "/api/v2/torrents/pause", "/api/v2/torrents/stop":
		server.stop.Add(1)
		if !server.validateStopAction(writer, request) {
			return
		}
		server.mu.Lock()
		mode := server.stopMode
		hook := server.stopHook
		server.mu.Unlock()
		if mode != "unknown_keep_started" && mode != "accepted_keep_started" {
			server.setState("stoppedUP", 1)
		}
		if hook != nil {
			hook()
		}
		if mode == "unknown_keep_started" || mode == "unknown_job_stopped" {
			connection, _, err := writer.(http.Hijacker).Hijack()
			if err != nil {
				server.t.Errorf("hijack stop response: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		_, _ = writer.Write([]byte("Ok."))
	case "/api/v2/torrents/delete":
		server.remove.Add(1)
		if request.Method != http.MethodPost || request.ParseForm() != nil || len(request.PostForm) != 2 ||
			request.PostForm.Get("hashes") != clientAdoptJobKey || request.PostForm.Get("deleteFiles") != "false" {
			server.t.Errorf("invalid keep-data removal request: %#v", request.PostForm)
			http.Error(writer, "invalid", http.StatusBadRequest)
			return
		}
		server.mu.Lock()
		mode := server.removeMode
		hook := server.removeHook
		if mode != "unknown_keep_job" && mode != "accepted_keep_job" {
			server.added = false
		}
		server.mu.Unlock()
		if hook != nil {
			hook()
		}
		if mode == "unknown_keep_job" || mode == "unknown_job_removed" {
			connection, _, err := writer.(http.Hijacker).Hijack()
			if err != nil {
				server.t.Errorf("hijack removal response: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		_, _ = writer.Write([]byte("Ok."))
	default:
		http.NotFound(writer, request)
	}
}

func (server *clientActivateServer) validateAction(writer http.ResponseWriter, request *http.Request) {
	if !server.validateStopAction(writer, request) {
		return
	}
	_, _ = writer.Write([]byte("Ok."))
}

func (server *clientActivateServer) validateStopAction(writer http.ResponseWriter, request *http.Request) bool {
	if request.Method != http.MethodPost || request.ParseForm() != nil || len(request.PostForm) != 1 || request.PostForm.Get("hashes") != clientAdoptJobKey {
		server.t.Errorf("invalid activation request: %#v", request.PostForm)
		http.Error(writer, "invalid", http.StatusBadRequest)
		return false
	}
	return true
}

func clientActivateBaseArgs(fixture clientAdoptCLIFixture, endpoint, adoptionOperation, adoptionPlanID string) []string {
	return []string{
		"--metafile-store", fixture.storeRoot, "--metafile-variant", fixture.variantID,
		"--target", fixture.materialize.targetRoot, "--materialize-operation", fixture.operation,
		"--materialize-plan-id", fixture.materialize.planID, "--adoption-operation", adoptionOperation,
		"--adoption-plan-id", adoptionPlanID, "--host-root", fixture.materialize.targetRoot,
		"--client-root", clientAdoptRoot, "--client-style", "posix", "--driver", "qbittorrent",
		"--url", endpoint, "--username", clientAdoptUser, "--password-stdin", "--timeout", "1m", "--output", "json",
	}
}

func transmissionClientActivateBaseArgs(fixture clientAdoptCLIFixture, endpoint, adoptionOperation, adoptionPlanID string) []string {
	result := clientActivateBaseArgs(fixture, endpoint, adoptionOperation, adoptionPlanID)
	for index := range result {
		if index > 0 && result[index-1] == "--driver" {
			result[index] = "transmission"
			break
		}
	}
	return result
}

func decodeClientActivateReport(t *testing.T, raw []byte) clientActivateJSONEnvelope {
	t.Helper()
	var result clientActivateJSONEnvelope
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode client activation report: %v\n%s", err, raw)
	}
	if result.Schema != "ptctl.dev/v1" || result.Kind != "client.activation" {
		t.Fatalf("unexpected activation envelope: %s", raw)
	}
	return result
}

func assertClientActivatePrivate(t *testing.T, raw []byte, fixture clientAdoptCLIFixture, endpoint string) {
	t.Helper()
	assertJSONStringsExclude(t, raw,
		fixture.materialize.targetRoot, fixture.materialize.sourceRoot, fixture.materialize.sourcePath,
		fixture.materialize.finalPath, fixture.materialize.torrentPath, fixture.storeRoot,
		clientAdoptRoot, clientAdoptRoot+"/"+materializeFinalName, materializeFinalName,
		endpoint, clientAdoptUser, clientAdoptPassword, clientAdoptJobKey,
		"magnet:?xt=urn:btih:"+fixture.meta.InfoHashV1,
	)
}
