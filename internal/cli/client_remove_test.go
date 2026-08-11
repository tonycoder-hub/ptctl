package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/clientremove"
)

type clientRemoveJSONEnvelope struct {
	Schema string              `json:"schema"`
	Kind   string              `json:"kind"`
	Data   clientremove.Report `json:"data"`
}

type clientRemoveCLIFixture struct {
	materialized clientAdoptCLIFixture
	server       *clientActivateServer
	activation   clientactivate.Report
}

func TestClientRemovePlanRunStatusAndFreshCompletionProof(t *testing.T) {
	fixture := prepareClientRemoveCLIFixture(t)
	base := clientRemoveBaseArgs(fixture)
	requestsBeforePlan := fixture.server.totalRequests()

	planned := runClientRemoveJSON(t, append([]string{"client", "remove", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	if planned.Data.Outcome != clientremove.OutcomeReady || planned.Data.Effect != "none" || planned.Data.Plan.ID == "" ||
		planned.Data.Plan.Data.DeleteLocalData || planned.Data.Plan.Data.Removal.Protocol != "qbittorrent_webapi_v5" ||
		planned.Data.Writes.WritesPerformed != 0 || planned.Data.Client.RequestsMade != 3 ||
		fixture.server.totalRequests()-requestsBeforePlan != 3 {
		t.Fatalf("unexpected removal plan: %#v", planned.Data)
	}
	assertClientRemovePrivate(t, mustJSON(t, planned), fixture)

	requestsBeforeRun := fixture.server.totalRequests()
	runArgs := append([]string{"client", "remove", "run"}, base...)
	runArgs = append(runArgs, "--expect-removal-plan-id", planned.Data.Plan.ID, "--acknowledge-client-removal")
	removed := runClientRemoveJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if removed.Data.Outcome != clientremove.OutcomeRemovedKeepData || removed.Data.Operation.Resumable ||
		removed.Data.Operation.ID != clientremove.OperationIDForPlan(planned.Data.Plan.ID).String() ||
		removed.Data.Plan.ExpectedID != planned.Data.Plan.ID || !removed.Data.Plan.Matches ||
		removed.Data.Writes.DownloaderRequests != 1 || removed.Data.Client.RequestsMade != 6 ||
		removed.Data.Mutation.Status != "accepted" || !removed.Data.Mutation.Receipt.Complete ||
		removed.Data.Mutation.Receipt.RequestsAttempted != 1 || removed.Data.Mutation.Receipt.AutomaticRetries != 0 ||
		removed.Data.Mutation.Receipt.RedirectsFollowed != 0 || removed.Data.Absence.Status != "exact_typed_job_absent" ||
		removed.Data.Final.BytesVerified != fixture.materialized.meta.TotalLength || removed.Data.Assurance.DeleteLocalDataRequested ||
		removed.Data.Writes.WritesPerformed == 0 || fixture.server.remove.Load() != 1 ||
		fixture.server.totalRequests()-requestsBeforeRun != 6 {
		t.Fatalf("unexpected removal run: %#v", removed.Data)
	}
	assertClientRemovePrivate(t, mustJSON(t, removed), fixture)

	requestsBeforeStatus := fixture.server.totalRequests()
	statusArgs := []string{"client", "remove", "status", "--target", fixture.materialized.materialize.targetRoot,
		"--expect-removal-plan-id", planned.Data.Plan.ID, "--output", "json", removed.Data.Operation.ID}
	status := runClientRemoveJSON(t, statusArgs, &trackingReader{}, 0)
	if status.Data.Outcome != clientremove.OutcomeHistoricalComplete || status.Data.Operation.Resumable ||
		status.Data.Final.MetafileVariantID != "" || status.Data.Assurance.QueueEvidence != "historical_journal_only_not_currently_observed" ||
		status.Data.Assurance.FilesystemEvidence != "historical_plan_only_not_currently_reverified" ||
		status.Data.Assurance.CompletionBasis != "accepted_response_then_exact_absence" ||
		fixture.server.totalRequests() != requestsBeforeStatus {
		t.Fatalf("unexpected historical status: %#v", status.Data)
	}

	resumeArgs := append([]string{"client", "remove", "resume"}, base...)
	resumeArgs = append(resumeArgs, "--expect-removal-plan-id", planned.Data.Plan.ID, removed.Data.Operation.ID)
	fresh := runClientRemoveJSON(t, resumeArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if fresh.Data.Outcome != clientremove.OutcomeAlreadyComplete || fresh.Data.Operation.Resumable ||
		fresh.Data.Absence.Status != "exact_typed_job_absent" || fresh.Data.Final.BytesVerified != fixture.materialized.meta.TotalLength ||
		fresh.Data.Client.RequestsMade != 2 || fixture.server.remove.Load() != 1 {
		t.Fatalf("unexpected fresh completion proof: %#v", fresh.Data)
	}
	assertClientRemovePrivate(t, mustJSON(t, fresh), fixture)
}

func TestClientRemoveUnknownResultNeverRepeatsWithoutExplicitAcknowledgement(t *testing.T) {
	fixture := prepareClientRemoveCLIFixture(t)
	base := clientRemoveBaseArgs(fixture)
	planned := runClientRemoveJSON(t, append([]string{"client", "remove", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	fixture.server.setRemoveMode("unknown_keep_job")

	runArgs := append([]string{"client", "remove", "run"}, base...)
	runArgs = append(runArgs, "--expect-removal-plan-id", planned.Data.Plan.ID, "--acknowledge-client-removal")
	unknown := runClientRemoveJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 4)
	if unknown.Data.Outcome != clientremove.OutcomeRequestUnknown || !unknown.Data.Operation.Resumable ||
		unknown.Data.Mutation.Status != "request_result_unknown" || unknown.Data.Mutation.Receipt.StopReason != "transport_failed" ||
		unknown.Data.Writes.DownloaderRequests != 1 || fixture.server.remove.Load() != 1 {
		t.Fatalf("unexpected unknown result: %#v", unknown.Data)
	}
	assertClientRemovePrivate(t, mustJSON(t, unknown), fixture)

	reader := &trackingReader{}
	resumeArgs := append([]string{"client", "remove", "resume"}, base...)
	resumeArgs = append(resumeArgs, "--expect-removal-plan-id", planned.Data.Plan.ID, unknown.Data.Operation.ID)
	blocked := runClientRemoveJSON(t, resumeArgs, reader, 4)
	if reader.read || fixture.server.remove.Load() != 1 || blocked.Data.Outcome != clientremove.OutcomeRequestUnknown {
		t.Fatalf("unacknowledged resume read=%t removals=%d report=%#v", reader.read, fixture.server.remove.Load(), blocked.Data)
	}

	fixture.server.setRemoveMode("")
	repeatArgs := append([]string(nil), resumeArgs[:len(resumeArgs)-1]...)
	repeatArgs = append(repeatArgs, "--acknowledge-client-removal", "--acknowledge-repeat-removal", unknown.Data.Operation.ID)
	repeated := runClientRemoveJSON(t, repeatArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if repeated.Data.Outcome != clientremove.OutcomeRemovedKeepData || repeated.Data.Operation.Resumable ||
		repeated.Data.Mutation.Status != "accepted" || repeated.Data.Absence.Status != "exact_typed_job_absent" ||
		repeated.Data.Writes.DownloaderRequests != 1 || repeated.Data.Client.RequestsMade != 6 || fixture.server.remove.Load() != 2 {
		t.Fatalf("unexpected acknowledged repeat: %#v", repeated.Data)
	}
	assertClientRemovePrivate(t, mustJSON(t, repeated), fixture)
}

func TestClientRemoveUnknownResponseCanProveUnattributedAbsence(t *testing.T) {
	fixture := prepareClientRemoveCLIFixture(t)
	base := clientRemoveBaseArgs(fixture)
	planned := runClientRemoveJSON(t, append([]string{"client", "remove", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	fixture.server.setRemoveMode("unknown_job_removed")
	runArgs := append([]string{"client", "remove", "run"}, base...)
	runArgs = append(runArgs, "--expect-removal-plan-id", planned.Data.Plan.ID, "--acknowledge-client-removal")
	report := runClientRemoveJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if report.Data.Outcome != clientremove.OutcomeRemovedUnattributed || report.Data.Operation.Resumable ||
		report.Data.Mutation.Status != "request_result_unknown" || report.Data.Mutation.Receipt.Complete ||
		report.Data.Absence.Status != "exact_typed_job_absent" || report.Data.Final.BytesVerified != fixture.materialized.meta.TotalLength ||
		report.Data.Assurance.CompletionBasis != "exact_absence_after_unknown_attempt_causality_unproven" ||
		len(report.Data.Warnings) != 1 || !strings.Contains(report.Data.Warnings[0], "causality is not attributed") || fixture.server.remove.Load() != 1 {
		t.Fatalf("unexpected unattributed completion: %#v", report.Data)
	}
	assertClientRemovePrivate(t, mustJSON(t, report), fixture)
	statusArgs := []string{"client", "remove", "status", "--target", fixture.materialized.materialize.targetRoot,
		"--expect-removal-plan-id", planned.Data.Plan.ID, "--output", "json", report.Data.Operation.ID}
	status := runClientRemoveJSON(t, statusArgs, &trackingReader{}, 0)
	if status.Data.Assurance.CompletionBasis != "exact_absence_after_unknown_attempt_causality_unproven" ||
		len(status.Data.Warnings) != 1 || !strings.Contains(status.Data.Warnings[0], "did not attribute") {
		t.Fatalf("historical status lost unattributed basis: %#v", status.Data)
	}
}

func TestClientRemoveAcceptedResponseWithoutAbsenceRemainsBlocked(t *testing.T) {
	fixture := prepareClientRemoveCLIFixture(t)
	base := clientRemoveBaseArgs(fixture)
	planned := runClientRemoveJSON(t, append([]string{"client", "remove", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	fixture.server.setRemoveMode("accepted_keep_job")
	runArgs := append([]string{"client", "remove", "run"}, base...)
	runArgs = append(runArgs, "--expect-removal-plan-id", planned.Data.Plan.ID, "--acknowledge-client-removal")
	report := runClientRemoveJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 4)
	if report.Data.Outcome != clientremove.OutcomeBlocked || !report.Data.Operation.Resumable ||
		report.Data.Mutation.Status != "accepted" || !report.Data.Mutation.Receipt.Complete ||
		report.Data.Absence.Status != "not_proven" || report.Data.Final.BytesVerified != 0 || fixture.server.remove.Load() != 1 {
		t.Fatalf("accepted response hid a still-present job: %#v", report.Data)
	}
	statusArgs := []string{"client", "remove", "status", "--target", fixture.materialized.materialize.targetRoot,
		"--expect-removal-plan-id", planned.Data.Plan.ID, "--output", "json", report.Data.Operation.ID}
	status := runClientRemoveJSON(t, statusArgs, &trackingReader{}, 0)
	if status.Data.Outcome != clientremove.OutcomeIncomplete || status.Data.Mutation.Status != "accepted" {
		t.Fatalf("historical accepted response was mislabeled: %#v", status.Data)
	}
	resumeArgs := append([]string{"client", "remove", "resume"}, base...)
	resumeArgs = append(resumeArgs, "--expect-removal-plan-id", planned.Data.Plan.ID, report.Data.Operation.ID)
	reader := &trackingReader{}
	blocked := runClientRemoveJSON(t, resumeArgs, reader, 4)
	if reader.read || fixture.server.remove.Load() != 1 || blocked.Data.Outcome != clientremove.OutcomeRequestUnknown {
		t.Fatalf("accepted unresolved resume read=%t removals=%d report=%#v", reader.read, fixture.server.remove.Load(), blocked.Data)
	}
}

func TestClientRemoveAcceptedResponseCannotHideFinalContentDamage(t *testing.T) {
	fixture := prepareClientRemoveCLIFixture(t)
	base := clientRemoveBaseArgs(fixture)
	planned := runClientRemoveJSON(t, append([]string{"client", "remove", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	fixture.server.setRemoveHook(func() {
		if err := os.WriteFile(fixture.materialized.materialize.finalPath, []byte("tampered-after-removal"), 0o600); err != nil {
			t.Errorf("tamper final: %v", err)
		}
	})
	runArgs := append([]string{"client", "remove", "run"}, base...)
	runArgs = append(runArgs, "--expect-removal-plan-id", planned.Data.Plan.ID, "--acknowledge-client-removal")
	report := runClientRemoveJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 3)
	if report.Data.Outcome != clientremove.OutcomeIntegrityFailed || report.Data.Operation.Resumable ||
		report.Data.Mutation.Status != "accepted" || !report.Data.Mutation.Receipt.Complete ||
		report.Data.Absence.Status != "not_proven" || report.Data.Final.BytesVerified != 0 || fixture.server.remove.Load() != 1 {
		t.Fatalf("damaged final was not kept separate from removal response: %#v", report.Data)
	}
	assertClientRemovePrivate(t, mustJSON(t, report), fixture)
}

func TestClientRemoveBadUsageDoesNotReadPassword(t *testing.T) {
	planID := strings.Repeat("a", 24)
	operation := clientremove.OperationIDForPlan(planID).String()
	for _, args := range [][]string{
		{"client", "remove", "run", "--password-stdin", "--expect-removal-plan-id", planID, "--acknowledge-client-removal"},
		{"client", "remove", "resume", "--password-stdin", "--expect-removal-plan-id", planID, "--acknowledge-repeat-removal", operation},
		{"client", "remove", "status", "--target", `C:\not-opened`, "--expect-removal-plan-id", strings.Repeat("b", 24), operation},
	} {
		reader := &trackingReader{}
		var out, errOut bytes.Buffer
		if code := Run(args, reader, &out, &errOut); code != 2 || reader.read {
			t.Fatalf("args=%v code=%d read=%t stdout=%q stderr=%q", args, code, reader.read, out.String(), errOut.String())
		}
	}
}

func prepareClientRemoveCLIFixture(t *testing.T) clientRemoveCLIFixture {
	t.Helper()
	fixture := newClientAdoptCLIFixture(t)
	server := newClientActivateServer(t, fixture.meta, fixture.raw)
	t.Cleanup(server.server.Close)
	baseAdopt := clientAdoptBaseArgs(fixture, server.server.URL)
	adoptionPlan := runClientAdoptForRemoval(t, append([]string{"client", "adopt", "plan"}, baseAdopt...))
	adoptionRun := append([]string{"client", "adopt", "run"}, baseAdopt...)
	adoptionRun = append(adoptionRun, "--expect-adoption-plan-id", adoptionPlan.Plan.ID, "--acknowledge-client-add")
	adopted := runClientAdoptForRemoval(t, adoptionRun)
	if adopted.Outcome != clientadopt.OutcomeAdoptedPendingRecheck {
		t.Fatalf("adoption did not complete: %#v", adopted)
	}

	baseActivation := clientActivateBaseArgs(fixture, server.server.URL, adopted.Operation.ID, adopted.Plan.ID)
	activationPlan := runClientActivateForRemoval(t, append([]string{"client", "activate", "plan"}, baseActivation...))
	activationRun := append([]string{"client", "activate", "run"}, baseActivation...)
	activationRun = append(activationRun, "--expect-activation-plan-id", activationPlan.Plan.ID, "--acknowledge-client-recheck")
	checking := runClientActivateForRemoval(t, activationRun)
	if checking.Outcome != clientactivate.OutcomeRecheckInProgress {
		t.Fatalf("activation did not enter checking: %#v", checking)
	}
	server.setState("stoppedUP", 1)
	activationResume := append([]string{"client", "activate", "resume"}, baseActivation...)
	activationResume = append(activationResume, "--expect-activation-plan-id", activationPlan.Plan.ID, activationPlan.Operation.ID)
	activated := runClientActivateForRemoval(t, activationResume)
	if activated.Outcome != clientactivate.OutcomeCheckedStopped || !activated.Journal.RecheckCompletionDurable {
		t.Fatalf("activation did not complete: %#v", activated)
	}
	return clientRemoveCLIFixture{materialized: fixture, server: server, activation: activated}
}

func clientRemoveBaseArgs(fixture clientRemoveCLIFixture) []string {
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

func runClientAdoptForRemoval(t *testing.T, args []string) clientadopt.Report {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := Run(args, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("command=%v code=%d stdout=%q stderr=%q", args[:3], code, out.String(), errOut.String())
	}
	return decodeClientAdoptReport(t, out.Bytes()).Data
}

func runClientActivateForRemoval(t *testing.T, args []string) clientactivate.Report {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := Run(args, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("command=%v code=%d stdout=%q stderr=%q", args[:3], code, out.String(), errOut.String())
	}
	return decodeClientActivateReport(t, out.Bytes()).Data
}

func runClientRemoveJSON(t *testing.T, args []string, stdin ioReader, expectedCode int) clientRemoveJSONEnvelope {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, stdin, &out, &errOut)
	if code != expectedCode {
		t.Fatalf("command=%v code=%d want=%d stdout=%q stderr=%q", args[:3], code, expectedCode, out.String(), errOut.String())
	}
	var result clientRemoveJSONEnvelope
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode client removal report: %v\n%s\nstderr=%s", err, out.Bytes(), errOut.Bytes())
	}
	if result.Schema != "ptctl.dev/v1" || result.Kind != "client.removal" || result.Data.Blockers == nil || result.Data.Warnings == nil {
		t.Fatalf("unexpected client removal envelope: %s", out.Bytes())
	}
	return result
}

type ioReader interface {
	Read([]byte) (int, error)
}

func assertClientRemovePrivate(t *testing.T, raw []byte, fixture clientRemoveCLIFixture) {
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

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
