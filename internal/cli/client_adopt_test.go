package cli

import (
	"bytes"
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

func newClientAdoptServer(t *testing.T, meta *metafile.MetaInfo, raw []byte) *clientAdoptServer {
	t.Helper()
	result := &clientAdoptServer{meta: meta, raw: append([]byte(nil), raw...), testing: t}
	result.server = httptest.NewServer(http.HandlerFunc(result.serveHTTP))
	return result
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
