package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
)

const (
	clientAdoptRoot     = "/downloads"
	clientAdoptUser     = "CLIENT-ADOPT-USERNAME-CANARY"
	clientAdoptPassword = "CLIENT-ADOPT-PASSWORD-CANARY"
	clientAdoptJobKey   = "CLIENT-ADOPT-OPAQUE-JOB-CANARY"
)

type clientAdoptJSONEnvelope struct {
	Schema string             `json:"schema"`
	Kind   string             `json:"kind"`
	Data   clientadopt.Report `json:"data"`
}

type clientAdoptRetentionJSONEnvelope struct {
	Schema string                      `json:"schema"`
	Kind   string                      `json:"kind"`
	Data   clientadopt.RetentionReport `json:"data"`
}

type clientAdoptForgetJSONEnvelope struct {
	Schema string                   `json:"schema"`
	Kind   string                   `json:"kind"`
	Data   clientadopt.ForgetReport `json:"data"`
}

type clientAdoptCLIFixture struct {
	materialize materializeCLIFixture
	storeRoot   string
	variantID   string
	operation   string
	meta        *metafile.MetaInfo
	raw         []byte
}

type clientAdoptServer struct {
	server *httptest.Server
	meta   *metafile.MetaInfo
	raw    []byte

	added      atomic.Bool
	login      atomic.Int32
	ledger     atomic.Int32
	add        atomic.Int32
	serverFail atomic.Bool
	testing    *testing.T
}

type transmissionAdoptServer struct {
	server *httptest.Server
	meta   *metafile.MetaInfo
	raw    []byte

	added     atomic.Bool
	handshake atomic.Int32
	session   atomic.Int32
	ledger    atomic.Int32
	add       atomic.Int32
	testing   *testing.T
}

func TestClientAdoptPlanRunStatusAndPrivacy(t *testing.T) {
	fixture := newClientAdoptCLIFixture(t)
	server := newClientAdoptServer(t, fixture.meta, fixture.raw)
	defer server.server.Close()

	base := clientAdoptBaseArgs(fixture, server.server.URL)
	var out, errOut bytes.Buffer
	planArgs := append([]string{"client", "adopt", "plan"}, base...)
	if code := Run(planArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	planned := decodeClientAdoptReport(t, out.Bytes())
	if planned.Schema != "ptctl.dev/v1" || planned.Kind != "client.adoption" ||
		planned.Data.Outcome != clientadopt.OutcomeReady || planned.Data.Plan.ID == "" ||
		planned.Data.Operation.ID == "" || planned.Data.Client.RequestsMade != 2 ||
		planned.Data.Client.BeforeIdentity != "absent" || len(planned.Data.Blockers) != 0 {
		t.Fatalf("unexpected plan report: %s", out.String())
	}
	assertClientAdoptPrivate(t, out.Bytes(), fixture, server.server.URL)

	out.Reset()
	errOut.Reset()
	runArgs := append([]string{"client", "adopt", "run"}, base...)
	runArgs = append(runArgs, "--expect-adoption-plan-id", planned.Data.Plan.ID, "--acknowledge-client-add")
	if code := Run(runArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	adopted := decodeClientAdoptReport(t, out.Bytes())
	if adopted.Data.Outcome != clientadopt.OutcomeAdoptedPendingRecheck ||
		adopted.Data.Operation.Status != "terminal" || adopted.Data.Operation.Resumable ||
		adopted.Data.Operation.ID != planned.Data.Operation.ID ||
		adopted.Data.Client.RequestsMade != 4 || !adopted.Data.Client.AddAttempted ||
		!adopted.Data.Client.AddReceipt.Complete || adopted.Data.Client.AddReceipt.RequestsAttempted != 1 ||
		adopted.Data.Client.AddReceipt.AutomaticRetries != 0 || adopted.Data.Client.AddReceipt.RedirectsFollowed != 0 ||
		adopted.Data.Client.BeforeIdentity != "absent" || adopted.Data.Client.AfterIdentity != "exact_unique" ||
		adopted.Data.Client.JobID == "" || !adopted.Data.Journal.IntentDurable ||
		!adopted.Data.Journal.CompletionDurable || adopted.Data.WritesPerformed == 0 ||
		adopted.Data.Blockers == nil || adopted.Data.Issues == nil || adopted.Data.Warnings == nil {
		t.Fatalf("unexpected adoption report: %s", out.String())
	}
	if server.login.Load() != 2 || server.ledger.Load() != 3 || server.add.Load() != 1 {
		t.Fatalf("requests login=%d ledger=%d add=%d", server.login.Load(), server.ledger.Load(), server.add.Load())
	}
	assertClientAdoptPrivate(t, out.Bytes(), fixture, server.server.URL)

	requestsBeforeStatus := server.login.Load() + server.ledger.Load() + server.add.Load()
	out.Reset()
	errOut.Reset()
	if code := Run([]string{
		"client", "adopt", "status", "--target", fixture.materialize.targetRoot, "--output", "json", planned.Data.Operation.ID,
	}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("status code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	status := decodeClientAdoptReport(t, out.Bytes())
	if status.Data.Outcome != clientadopt.OutcomeHistoricalAdopted || status.Data.WritesPerformed != 0 ||
		status.Data.Client.RequestsMade != 0 || status.Data.Client.Status != "historical_adoption_recorded_current_client_not_observed" ||
		status.Data.Journal.IntentDurable || status.Data.Journal.CompletionDurable {
		t.Fatalf("unexpected status: %s", out.String())
	}
	if requestsAfterStatus := server.login.Load() + server.ledger.Load() + server.add.Load(); requestsAfterStatus != requestsBeforeStatus {
		t.Fatalf("read-only status made a downloader request: before=%d after=%d", requestsBeforeStatus, requestsAfterStatus)
	}
	assertClientAdoptPrivate(t, out.Bytes(), fixture, server.server.URL)

	// A canonical but different reviewed plan ID is rejected after local proof
	// and before password stdin, login, or any new journal mutation.
	reader := &trackingReader{}
	out.Reset()
	errOut.Reset()
	wrongID := strings.Repeat("f", 24)
	if wrongID == planned.Data.Plan.ID {
		wrongID = strings.Repeat("e", 24)
	}
	badRun := append([]string{"client", "adopt", "run"}, base...)
	badRun = append(badRun, "--expect-adoption-plan-id", wrongID, "--acknowledge-client-add")
	if code := Run(badRun, reader, &out, &errOut); code != 4 || !strings.Contains(out.String(), "plan") {
		t.Fatalf("mismatched plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if reader.read || server.login.Load()+server.ledger.Load()+server.add.Load() != requestsBeforeStatus {
		t.Fatalf("mismatched plan crossed credential/network boundary: read=%t requests=%d", reader.read, server.login.Load()+server.ledger.Load()+server.add.Load())
	}
}

func TestTransmissionClientAdoptPlanAndRunRemainStoppedAndV1Only(t *testing.T) {
	fixture := newClientAdoptCLIFixture(t)
	server := newTransmissionAdoptServer(t, fixture.meta, fixture.raw)
	defer server.server.Close()
	base := transmissionClientAdoptBaseArgs(fixture, server.server.URL)

	var out, errOut bytes.Buffer
	if code := Run(append([]string{"client", "adopt", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	planned := decodeClientAdoptReport(t, out.Bytes())
	if planned.Data.Outcome != clientadopt.OutcomeReady || planned.Data.Plan.Driver != "transmission" ||
		planned.Data.Client.RequestsMade != 3 || planned.Data.Client.BeforeIdentity != "absent" {
		t.Fatalf("unexpected Transmission plan: %s", out.String())
	}
	assertClientAdoptPrivate(t, out.Bytes(), fixture, server.server.URL)

	out.Reset()
	errOut.Reset()
	runArgs := append([]string{"client", "adopt", "run"}, base...)
	runArgs = append(runArgs, "--expect-adoption-plan-id", planned.Data.Plan.ID, "--acknowledge-client-add")
	if code := Run(runArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	adopted := decodeClientAdoptReport(t, out.Bytes())
	if adopted.Data.Outcome != clientadopt.OutcomeAdoptedPendingRecheck || adopted.Data.Plan.Driver != "transmission" ||
		adopted.Data.Client.RequestsMade != 5 || !adopted.Data.Client.AddReceipt.Complete ||
		adopted.Data.Client.BeforeIdentity != "absent" || adopted.Data.Client.AfterIdentity != "exact_unique" ||
		!adopted.Data.Journal.CompletionDurable {
		t.Fatalf("unexpected Transmission adoption: %s", out.String())
	}
	if server.handshake.Load() != 2 || server.session.Load() != 2 || server.ledger.Load() != 3 || server.add.Load() != 1 {
		t.Fatalf("wire counts handshake=%d session=%d ledger=%d add=%d", server.handshake.Load(), server.session.Load(), server.ledger.Load(), server.add.Load())
	}
	assertClientAdoptPrivate(t, out.Bytes(), fixture, server.server.URL)
}

func TestClientAdoptUnknownRequestIsNotRepeatedWithoutAcknowledgement(t *testing.T) {
	fixture := newClientAdoptCLIFixture(t)
	server := newClientAdoptServer(t, fixture.meta, fixture.raw)
	server.serverFail.Store(true)
	defer server.server.Close()
	base := clientAdoptBaseArgs(fixture, server.server.URL)

	var out, errOut bytes.Buffer
	if code := Run(append([]string{"client", "adopt", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	planned := decodeClientAdoptReport(t, out.Bytes())
	out.Reset()
	errOut.Reset()
	runArgs := append([]string{"client", "adopt", "run"}, base...)
	runArgs = append(runArgs, "--expect-adoption-plan-id", planned.Data.Plan.ID, "--acknowledge-client-add")
	if code := Run(runArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 4 {
		t.Fatalf("unknown run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	unknown := decodeClientAdoptReport(t, out.Bytes())
	if unknown.Data.Outcome != clientadopt.OutcomeRequestUnknown || unknown.Data.Operation.PhaseAfter != "request_result_unknown" ||
		unknown.Data.Journal.AttemptsRecorded != 1 || server.add.Load() != 1 {
		t.Fatalf("request was not represented as unknown: %s", out.String())
	}
	assertClientAdoptPrivate(t, out.Bytes(), fixture, server.server.URL)
	assertClientAdoptErrorPrivate(t, errOut.String(), fixture, server.server.URL)

	out.Reset()
	errOut.Reset()
	resumeArgs := append([]string{"client", "adopt", "resume"}, base...)
	resumeArgs = append(resumeArgs, "--expect-adoption-plan-id", planned.Data.Plan.ID, planned.Data.Operation.ID)
	if code := Run(resumeArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 4 {
		t.Fatalf("observe-only resume code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	resumed := decodeClientAdoptReport(t, out.Bytes())
	if resumed.Data.Outcome != clientadopt.OutcomeRequestUnknown || resumed.Data.Client.AddAttempted ||
		server.add.Load() != 1 || resumed.Data.Journal.AttemptsRecorded != 1 {
		t.Fatalf("resume repeated an unknown request: %s", out.String())
	}
	assertClientAdoptPrivate(t, out.Bytes(), fixture, server.server.URL)
	assertClientAdoptErrorPrivate(t, errOut.String(), fixture, server.server.URL)
}

func TestClientAdoptPruneIsLocalAndRetainedResumeStopsBeforeCredentials(t *testing.T) {
	fixture := newClientAdoptCLIFixture(t)
	server := newClientAdoptServer(t, fixture.meta, fixture.raw)
	defer server.server.Close()
	base := clientAdoptBaseArgs(fixture, server.server.URL)
	var out, errOut bytes.Buffer
	if code := Run(append([]string{"client", "adopt", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	planned := decodeClientAdoptReport(t, out.Bytes())
	out.Reset()
	errOut.Reset()
	runArgs := append([]string{"client", "adopt", "run"}, base...)
	runArgs = append(runArgs, "--expect-adoption-plan-id", planned.Data.Plan.ID, "--acknowledge-client-add")
	if code := Run(runArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	requestsBefore := server.login.Load() + server.ledger.Load() + server.add.Load()
	reader := &trackingReader{}
	out.Reset()
	errOut.Reset()
	pruneArgs := []string{"client", "adopt", "prune", "--target", fixture.materialize.targetRoot,
		"--expect-adoption-plan-id", planned.Data.Plan.ID, "--acknowledge-operation-state-deletion", "--output", "json", planned.Data.Operation.ID}
	if code := Run(pruneArgs, reader, &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("prune code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	retained := decodeClientAdoptRetentionReport(t, out.Bytes())
	if retained.Data.Outcome != clientadopt.RetentionOutcomePruned || !retained.Data.Markers.ExactTombstone ||
		retained.Data.Operation.Status != "retained" || retained.Data.WritesPerformed == 0 || reader.read {
		t.Fatalf("retention report=%#v stdin_read=%t", retained.Data, reader.read)
	}
	if requestsAfter := server.login.Load() + server.ledger.Load() + server.add.Load(); requestsAfter != requestsBefore {
		t.Fatalf("prune contacted downloader: before=%d after=%d", requestsBefore, requestsAfter)
	}
	assertClientAdoptPrivate(t, out.Bytes(), fixture, server.server.URL)

	reader = &trackingReader{}
	out.Reset()
	errOut.Reset()
	resumeArgs := append([]string{"client", "adopt", "resume"}, base...)
	resumeArgs = append(resumeArgs, "--expect-adoption-plan-id", planned.Data.Plan.ID, planned.Data.Operation.ID)
	if code := Run(resumeArgs, reader, &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("retained resume code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	resume := decodeClientAdoptReport(t, out.Bytes())
	if resume.Data.Outcome != clientadopt.OutcomeHistoricalAdopted || resume.Data.Operation.Status != "retained" ||
		resume.Data.Journal.RetentionState != "complete" || resume.Data.Plan.ExpectedID != planned.Data.Plan.ID ||
		!resume.Data.Plan.Matches || reader.read {
		t.Fatalf("retained resume=%#v stdin_read=%t", resume.Data, reader.read)
	}
	if requestsAfter := server.login.Load() + server.ledger.Load() + server.add.Load(); requestsAfter != requestsBefore {
		t.Fatalf("retained resume contacted downloader: before=%d after=%d", requestsBefore, requestsAfter)
	}

	reader = &trackingReader{}
	out.Reset()
	errOut.Reset()
	wrongID := strings.Repeat("f", 24)
	if wrongID == planned.Data.Plan.ID {
		wrongID = strings.Repeat("e", 24)
	}
	wrongResume := append([]string{"client", "adopt", "resume"}, base...)
	wrongResume = append(wrongResume, "--expect-adoption-plan-id", wrongID, planned.Data.Operation.ID)
	if code := Run(wrongResume, reader, &out, &errOut); code != 4 {
		t.Fatalf("wrong retained resume code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if reader.read || server.login.Load()+server.ledger.Load()+server.add.Load() != requestsBefore {
		t.Fatalf("wrong retained resume crossed credential/network boundary: read=%t", reader.read)
	}

	out.Reset()
	errOut.Reset()
	if code := Run(pruneArgs, strings.NewReader("CANARY-MUST-NOT-BE-READ"), &out, &errOut); code != 0 {
		t.Fatalf("repeated prune code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	repeated := decodeClientAdoptRetentionReport(t, out.Bytes())
	if repeated.Data.Outcome != clientadopt.RetentionOutcomeAlreadyPruned || repeated.Data.WritesPerformed != 0 {
		t.Fatalf("repeated retention=%#v", repeated.Data)
	}

	forgetArgs := []string{"client", "adopt", "forget", "--target", fixture.materialize.targetRoot,
		"--expect-adoption-plan-id", planned.Data.Plan.ID, "--acknowledge-historical-evidence-deletion", "--output", "json", planned.Data.Operation.ID}
	reader = &trackingReader{}
	out.Reset()
	errOut.Reset()
	if code := Run(forgetArgs, reader, &out, &errOut); code != 0 || reader.read || server.login.Load()+server.ledger.Load()+server.add.Load() != requestsBefore {
		t.Fatalf("adoption forget code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
	forgotten := decodeClientAdoptForgetReport(t, out.Bytes())
	if forgotten.Data.Outcome != clientadopt.ForgetOutcomeForgotten || !forgotten.Data.Authority.TargetHistoricalEvidenceErased ||
		forgotten.Data.Authority.MarkerDurable || forgotten.Data.Operation.Resumable || forgotten.Data.WritesPerformed == 0 ||
		forgotten.Data.Blockers == nil || forgotten.Data.Issues == nil || forgotten.Data.Warnings == nil {
		t.Fatalf("unexpected adoption forget report: %s", out.String())
	}
	assertClientAdoptPrivate(t, out.Bytes(), fixture, server.server.URL)
	reader = &trackingReader{}
	out.Reset()
	errOut.Reset()
	if code := Run(forgetArgs, reader, &out, &errOut); code != 1 || reader.read || server.login.Load()+server.ledger.Load()+server.add.Load() != requestsBefore {
		t.Fatalf("repeated adoption forget code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
	absent := decodeClientAdoptForgetReport(t, out.Bytes())
	if absent.Data.Outcome != clientadopt.ForgetOutcomeAbsentUnattributed || absent.Data.WritesPerformed != 0 || absent.Data.Authority.TargetHistoricalEvidenceErased {
		t.Fatalf("repeated adoption forget claimed historical idempotence: %s", out.String())
	}
	assertClientAdoptPrivate(t, out.Bytes(), fixture, server.server.URL)
}

func TestClientAdoptForgetBadUsageDoesNotReadInput(t *testing.T) {
	planID := strings.Repeat("f", 24)
	operationID := clientadopt.OperationIDForPlan(planID).String()
	for _, args := range [][]string{
		{"client", "adopt", "forget", "--target", `C:\not-opened`, "--expect-adoption-plan-id", planID, operationID},
		{"client", "adopt", "forget", "--target", `C:\not-opened`, "--expect-adoption-plan-id", "bad", "--acknowledge-historical-evidence-deletion", operationID},
	} {
		reader := &trackingReader{}
		var out, errOut bytes.Buffer
		if code := Run(args, reader, &out, &errOut); code != 2 || reader.read {
			t.Fatalf("args=%v code=%d read=%t stdout=%q stderr=%q", args, code, reader.read, out.String(), errOut.String())
		}
	}
}

func TestClientAdoptUnsupportedDriverDoesNotReadPasswordOrFilesystem(t *testing.T) {
	reader := &trackingReader{}
	var out, errOut bytes.Buffer
	code := Run([]string{
		"client", "adopt", "plan", "--metafile-store", "not-opened", "--metafile-variant", "not-parsed",
		"--target", "not-opened", "--materialize-operation", "not-parsed", "--materialize-plan-id", "not-parsed",
		"--host-root", "not-opened", "--client-root", "/not-opened", "--driver", "deluge",
		"--url", "https://not-opened.invalid", "--username", "nobody", "--password-stdin",
	}, reader, &out, &errOut)
	if code != 2 || reader.read || !strings.Contains(errOut.String(), "qbittorrent or transmission") {
		t.Fatalf("code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
}

func newClientAdoptCLIFixture(t *testing.T) clientAdoptCLIFixture {
	t.Helper()
	fixture := newMaterializeCLIFixture(t)
	storeRoot, variantID := storeMaterializeMetafile(t, fixture.torrentPath)
	var out, errOut bytes.Buffer
	if code := Run([]string{
		"seed", "materialize", "run", "--metafile-store", storeRoot, "--metafile-variant", variantID,
		"--search-root", fixture.sourceRoot, "--target", fixture.targetRoot,
		"--expect-plan-id", fixture.planID, "--acknowledge-filesystem-write", "--output", "json",
	}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("materialize fixture code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	materialized := decodeMaterializeReport(t, out.Bytes())
	meta, err := metafile.Read(fixture.torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(fixture.torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	return clientAdoptCLIFixture{
		materialize: fixture, storeRoot: storeRoot, variantID: variantID,
		operation: materialized.Data.Operation.ID, meta: meta, raw: raw,
	}
}

func clientAdoptBaseArgs(fixture clientAdoptCLIFixture, endpoint string) []string {
	return []string{
		"--metafile-store", fixture.storeRoot, "--metafile-variant", fixture.variantID,
		"--target", fixture.materialize.targetRoot, "--materialize-operation", fixture.operation,
		"--materialize-plan-id", fixture.materialize.planID,
		"--host-root", fixture.materialize.targetRoot, "--client-root", clientAdoptRoot, "--client-style", "posix",
		"--driver", "qbittorrent", "--url", endpoint, "--username", clientAdoptUser, "--password-stdin",
		"--timeout", "1m", "--output", "json",
	}
}

func transmissionClientAdoptBaseArgs(fixture clientAdoptCLIFixture, endpoint string) []string {
	return []string{
		"--metafile-store", fixture.storeRoot, "--metafile-variant", fixture.variantID,
		"--target", fixture.materialize.targetRoot, "--materialize-operation", fixture.operation,
		"--materialize-plan-id", fixture.materialize.planID,
		"--host-root", fixture.materialize.targetRoot, "--client-root", clientAdoptRoot, "--client-style", "posix",
		"--driver", "transmission", "--url", endpoint, "--username", clientAdoptUser, "--password-stdin",
		"--timeout", "1m", "--output", "json",
	}
}

func newClientAdoptServer(t *testing.T, meta *metafile.MetaInfo, raw []byte) *clientAdoptServer {
	t.Helper()
	result := &clientAdoptServer{meta: meta, raw: append([]byte(nil), raw...), testing: t}
	result.server = httptest.NewServer(http.HandlerFunc(result.serveHTTP))
	return result
}

func newTransmissionAdoptServer(t *testing.T, meta *metafile.MetaInfo, raw []byte) *transmissionAdoptServer {
	t.Helper()
	result := &transmissionAdoptServer{meta: meta, raw: append([]byte(nil), raw...), testing: t}
	result.server = httptest.NewServer(http.HandlerFunc(result.serveHTTP))
	return result
}

func (server *transmissionAdoptServer) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/transmission/rpc" {
		http.NotFound(writer, request)
		return
	}
	username, password, ok := request.BasicAuth()
	if !ok || username != clientAdoptUser || password != clientAdoptPassword {
		server.testing.Errorf("invalid Transmission adoption authentication")
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	if request.Header.Get("X-Transmission-Session-Id") == "" {
		server.handshake.Add(1)
		writer.Header().Set("X-Transmission-Session-Id", "transmission-adopt-token")
		writer.Header().Set("X-Transmission-Rpc-Version", "6.0.0")
		writer.WriteHeader(http.StatusConflict)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		server.testing.Errorf("read Transmission adoption request: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	var rpc map[string]json.RawMessage
	if err := json.Unmarshal(body, &rpc); err != nil {
		server.testing.Errorf("decode Transmission adoption request: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	var method string
	_ = json.Unmarshal(rpc["method"], &method)
	switch method {
	case "session_get":
		server.session.Add(1)
		server.writeResponse(writer, rpc, map[string]any{"version": "4.1.0", "rpc_version_semver": "6.0.0", "rpc_version": 18})
	case "torrent_get":
		server.ledger.Add(1)
		jobs := []any{}
		if server.added.Load() {
			jobs = append(jobs, map[string]any{
				"hash_string": server.meta.InfoHashV1, "name": materializeFinalName,
				"total_size": server.meta.TotalLength, "percent_complete": 0.0, "status": 0,
				"download_dir": clientAdoptRoot, "downloaded_ever": int64(0), "uploaded_ever": int64(0),
			})
		}
		server.writeResponse(writer, rpc, map[string]any{"torrents": jobs})
	case "torrent_add":
		server.add.Add(1)
		var params struct {
			DownloadDir string `json:"download_dir"`
			Metainfo    string `json:"metainfo"`
			Paused      bool   `json:"paused"`
		}
		if err := json.Unmarshal(rpc["params"], &params); err != nil {
			server.testing.Errorf("decode Transmission add params: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(params.Metainfo)
		if err != nil || !bytes.Equal(decoded, server.raw) || params.DownloadDir != clientAdoptRoot || !params.Paused {
			server.testing.Errorf("Transmission add did not submit exact stopped payload")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		server.added.Store(true)
		server.writeResponse(writer, rpc, map[string]any{"torrent_added": map[string]any{
			"id": 7, "name": materializeFinalName, "hash_string": server.meta.InfoHashV1,
		}})
	default:
		server.testing.Errorf("unexpected Transmission adoption method %q", method)
		writer.WriteHeader(http.StatusBadRequest)
	}
}

func (server *transmissionAdoptServer) writeResponse(writer http.ResponseWriter, request map[string]json.RawMessage, result map[string]any) {
	var id int64
	if err := json.Unmarshal(request["id"], &id); err != nil {
		server.testing.Errorf("decode Transmission adoption request id: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "result": result, "id": id}); err != nil {
		server.testing.Errorf("encode Transmission adoption response: %v", err)
	}
}

func (server *clientAdoptServer) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/api/v2/auth/login":
		server.login.Add(1)
		if request.Method != http.MethodPost || request.ParseForm() != nil ||
			request.Form.Get("username") != clientAdoptUser || request.Form.Get("password") != clientAdoptPassword {
			server.testing.Errorf("invalid client adoption login")
			http.Error(writer, "rejected", http.StatusForbidden)
			return
		}
		http.SetCookie(writer, &http.Cookie{Name: "SID", Value: "client-adopt-session", Path: "/"})
		_, _ = writer.Write([]byte("Ok."))
	case "/api/v2/torrents/info":
		server.ledger.Add(1)
		if _, err := request.Cookie("SID"); err != nil {
			server.testing.Errorf("ledger request is unauthenticated")
		}
		writer.Header().Set("Content-Type", "application/json")
		jobs := []map[string]any{}
		if server.added.Load() {
			jobs = append(jobs, map[string]any{
				"hash": clientAdoptJobKey, "magnet_uri": "magnet:?xt=urn:btih:" + server.meta.InfoHashV1,
				"name": materializeFinalName, "size": server.meta.TotalLength, "progress": 0.0,
				"state": "stoppedDL", "save_path": clientAdoptRoot,
				"content_path": clientAdoptRoot + "/" + materializeFinalName,
				"downloaded":   int64(0), "uploaded": int64(0),
			})
		}
		_ = json.NewEncoder(writer).Encode(jobs)
	case "/api/v2/torrents/add":
		server.add.Add(1)
		if _, err := request.Cookie("SID"); err != nil {
			server.testing.Errorf("add request is unauthenticated")
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			server.testing.Errorf("parse add request: %v", err)
			http.Error(writer, "invalid", http.StatusBadRequest)
			return
		}
		if request.FormValue("savepath") != clientAdoptRoot || request.FormValue("paused") != "true" ||
			request.FormValue("stopped") != "true" || request.FormValue("skip_checking") != "false" {
			server.testing.Errorf("unsafe add form values: %#v", request.MultipartForm.Value)
		}
		files := request.MultipartForm.File["torrents"]
		if len(files) != 1 || files[0].Filename != "metafile.torrent" {
			server.testing.Errorf("unexpected metafile upload: %#v", files)
			http.Error(writer, "invalid", http.StatusBadRequest)
			return
		}
		file, err := files[0].Open()
		if err != nil {
			server.testing.Errorf("open upload: %v", err)
			http.Error(writer, "invalid", http.StatusBadRequest)
			return
		}
		uploaded, readErr := io.ReadAll(file)
		_ = file.Close()
		if readErr != nil || !bytes.Equal(uploaded, server.raw) {
			server.testing.Errorf("uploaded metafile was not exact")
		}
		if server.serverFail.Load() {
			http.Error(writer, "CLIENT-ADOPT-RESPONSE-CANARY", http.StatusInternalServerError)
			return
		}
		server.added.Store(true)
		_, _ = writer.Write([]byte("Ok."))
	default:
		http.NotFound(writer, request)
	}
}

func decodeClientAdoptReport(t *testing.T, raw []byte) clientAdoptJSONEnvelope {
	t.Helper()
	var result clientAdoptJSONEnvelope
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode client adoption report: %v\n%s", err, raw)
	}
	if result.Schema != "ptctl.dev/v1" || result.Kind != "client.adoption" {
		t.Fatalf("unexpected client adoption envelope: %s", raw)
	}
	return result
}

func decodeClientAdoptRetentionReport(t *testing.T, raw []byte) clientAdoptRetentionJSONEnvelope {
	t.Helper()
	var result clientAdoptRetentionJSONEnvelope
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode client adoption retention report: %v\n%s", err, raw)
	}
	if result.Schema != "ptctl.dev/v1" || result.Kind != "client.adoption.retention" {
		t.Fatalf("unexpected client adoption retention envelope: %s", raw)
	}
	return result
}

func decodeClientAdoptForgetReport(t *testing.T, raw []byte) clientAdoptForgetJSONEnvelope {
	t.Helper()
	var result clientAdoptForgetJSONEnvelope
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode client adoption forget report: %v\n%s", err, raw)
	}
	if result.Schema != "ptctl.dev/v1" || result.Kind != "client.adoption.forget" {
		t.Fatalf("unexpected client adoption forget envelope: %s", raw)
	}
	return result
}

func assertClientAdoptPrivate(t *testing.T, raw []byte, fixture clientAdoptCLIFixture, endpoint string) {
	t.Helper()
	assertJSONStringsExclude(t, raw,
		fixture.materialize.targetRoot, fixture.materialize.sourceRoot, fixture.materialize.sourcePath,
		fixture.materialize.finalPath, fixture.materialize.torrentPath, fixture.storeRoot,
		clientAdoptRoot, clientAdoptRoot+"/"+materializeFinalName, materializeFinalName,
		endpoint, clientAdoptUser, clientAdoptPassword, clientAdoptJobKey,
		"magnet:?xt=urn:btih:"+fixture.meta.InfoHashV1, "CLIENT-ADOPT-RESPONSE-CANARY",
	)
}

func assertClientAdoptErrorPrivate(t *testing.T, output string, fixture clientAdoptCLIFixture, endpoint string) {
	t.Helper()
	for _, secret := range []string{
		fixture.materialize.targetRoot, fixture.materialize.sourceRoot, fixture.materialize.sourcePath,
		fixture.materialize.finalPath, fixture.materialize.torrentPath, fixture.storeRoot,
		clientAdoptRoot, clientAdoptRoot + "/" + materializeFinalName, materializeFinalName,
		endpoint, clientAdoptUser, clientAdoptPassword, clientAdoptJobKey,
		"magnet:?xt=urn:btih:" + fixture.meta.InfoHashV1, "CLIENT-ADOPT-RESPONSE-CANARY",
	} {
		if secret != "" && strings.Contains(output, secret) {
			t.Fatalf("private value %q leaked in stderr: %q", secret, output)
		}
	}
}
