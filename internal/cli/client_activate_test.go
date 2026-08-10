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
	"github.com/tonycoder-hub/ptctl/internal/metafile"
)

type clientActivateJSONEnvelope struct {
	Schema string                `json:"schema"`
	Kind   string                `json:"kind"`
	Data   clientactivate.Report `json:"data"`
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

	login   atomic.Int32
	version atomic.Int32
	ledger  atomic.Int32
	recheck atomic.Int32
	start   atomic.Int32
	add     atomic.Int32
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
	if activationServer.login.Load() != 5 || activationServer.version.Load() != 3 || activationServer.ledger.Load() != 8 ||
		activationServer.add.Load() != 1 || activationServer.recheck.Load() != 1 || activationServer.start.Load() != 1 {
		t.Fatalf("requests login=%d version=%d ledger=%d add=%d recheck=%d start=%d", activationServer.login.Load(), activationServer.version.Load(),
			activationServer.ledger.Load(), activationServer.add.Load(), activationServer.recheck.Load(), activationServer.start.Load())
	}
}

func TestClientActivateBadUsageDoesNotReadPassword(t *testing.T) {
	reader := &trackingReader{}
	var out, errOut bytes.Buffer
	code := Run([]string{"client", "activate", "run", "--password-stdin", "--expect-activation-plan-id", strings.Repeat("f", 24),
		"--acknowledge-client-recheck"}, reader, &out, &errOut)
	if code != 2 || reader.read {
		t.Fatalf("code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
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
	return server.login.Load() + server.version.Load() + server.ledger.Load() + server.add.Load() + server.recheck.Load() + server.start.Load()
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
	default:
		http.NotFound(writer, request)
	}
}

func (server *clientActivateServer) validateAction(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.ParseForm() != nil || len(request.PostForm) != 1 ||
		request.PostForm.Get("hashes") != clientAdoptJobKey {
		server.t.Errorf("invalid activation request: %#v", request.PostForm)
		http.Error(writer, "invalid", http.StatusBadRequest)
		return
	}
	_, _ = writer.Write([]byte("Ok."))
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
