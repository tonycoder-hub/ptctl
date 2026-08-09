package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

func TestReconcileReportBracketsOneClientSessionAndProducesConsistentJSON(t *testing.T) {
	torrentPath, searchRoot, meta := writeReconciliationFixture(t)
	const (
		clientRoot    = "/downloads"
		clientPath    = "/downloads/PTCTL-CLIENT-PATH-CANARY.bin"
		password      = "DOWNLOADER-PASSWORD-CANARY"
		magnetCanary  = "TRACKER-MAGNET-CANARY"
		clientUser    = "CLIENT-USERNAME-CANARY"
		userSiteRef   = "tjupt/4242"
		opaqueJobHash = "opaque-job-key"
	)
	magnet := "magnet:?xt=urn:btih:" + meta.InfoHashV1 + "&tr=https%3A%2F%2Ftracker.invalid%2Fannounce%3Fpasskey%3D" + magnetCanary
	var loginRequests atomic.Int32
	var listRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			loginRequests.Add(1)
			if r.Method != http.MethodPost {
				t.Errorf("login method=%s", r.Method)
			}
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse login form: %v", err)
			}
			if r.Form.Get("username") != clientUser || r.Form.Get("password") != password {
				t.Errorf("unexpected login credential")
			}
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "ok", Path: "/"})
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			listRequests.Add(1)
			if cookie, err := r.Cookie("SID"); err != nil || cookie.Value != "ok" {
				t.Errorf("ledger read did not reuse authenticated session")
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"hash": opaqueJobHash, "magnet_uri": magnet,
				"name": "PTCTL-CLIENT-PATH-CANARY.bin", "size": int64(len("content")),
				"progress": 1.0, "state": "uploading", "save_path": clientRoot,
				"content_path": clientPath, "downloaded": int64(len("content")), "uploaded": int64(10),
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var out, errOut bytes.Buffer
	code := Run([]string{
		"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot,
		"--driver", "qbittorrent", "--url", server.URL, "--username", clientUser, "--password-stdin",
		"--host-root", searchRoot, "--client-root", clientRoot, "--client-style", "posix",
		"--site-ref", userSiteRef, "--timeout", "1m", "--output", "json",
	}, strings.NewReader(password+"\n"), &out, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if loginRequests.Load() != 1 || listRequests.Load() != 2 {
		t.Fatalf("login=%d list=%d; wanted one login and two bracket reads", loginRequests.Load(), listRequests.Load())
	}
	var response struct {
		Schema string           `json:"schema"`
		Kind   string           `json:"kind"`
		Data   reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Schema != "ptctl.dev/v1" || response.Kind != "ledger.reconciliation" || response.Data.Outcome != "consistent" || response.Data.WritesPerformed != 0 || response.Data.Assurance != "local_content_proof_and_bracketed_typed_client_identity_with_lexical_path_agreement" {
		t.Fatalf("unexpected reconciliation contract: %s", out.String())
	}
	if !response.Data.Scope.PathMappingRequested || response.Data.Scope.PathMappingID == "" || response.Data.Scope.ClientPathSemantics != "posix_exact" {
		t.Fatalf("path mapping scope is not auditable: %#v", response.Data.Scope)
	}
	if len(response.Data.Relations) != 5 || response.Data.Relations[0].Kind != "site_metafile" || response.Data.Relations[0].Status != "declared_unbound" || response.Data.Relations[1].Status != "unobservable" || response.Data.Relations[2].Status != "exact_unique" || response.Data.Relations[3].Status != "verified_unique" || response.Data.Relations[4].Status != "same_location" || response.Data.Relations[4].EvidenceLevel != "lexical" {
		t.Fatalf("unclear relation contract: %#v", response.Data.Relations)
	}
	if response.Data.Ledgers.Downloader.RequestsMade != 3 || response.Data.Ledgers.Downloader.JobsExaminedBefore != 1 || response.Data.Ledgers.Downloader.JobsExaminedAfter != 1 || len(response.Data.Ledgers.Downloader.Matches) != 1 || !response.Data.Ledgers.Storage.ProcessLocalProof || len(response.Data.Blockers) != 0 {
		t.Fatalf("unexpected ledger summaries: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), torrentPath, searchRoot, filepath.Join(searchRoot, "PTCTL-CLIENT-PATH-CANARY.bin"), clientPath, server.URL, clientUser, password, magnet, magnetCanary)
}

func TestReconcileReportStorageOnlyIsPartialAndReportOriented(t *testing.T) {
	torrentPath, searchRoot, _ := writeReconciliationFixture(t)
	var out, errOut bytes.Buffer
	code := Run([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--output", "json"}, strings.NewReader(""), &out, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var response struct {
		Kind string           `json:"kind"`
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Kind != "ledger.reconciliation" || response.Data.Outcome != "partial" || response.Data.Ledgers.Storage.Status != "verified_unique" || !response.Data.Ledgers.Storage.ProcessLocalProof || response.Data.Ledgers.Downloader.Status != "not_requested" || response.Data.Scope.ClientRequested || len(response.Data.Relations) != 5 {
		t.Fatalf("unexpected storage-only report: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), torrentPath, searchRoot)
}

func TestReconcileReportValidatesEverythingBeforePasswordRead(t *testing.T) {
	torrentPath, searchRoot, _ := writeReconciliationFixture(t)
	missingTorrent := filepath.Join(t.TempDir(), "missing.torrent")
	clientGroup := []string{"--driver", "qbittorrent", "--url", "https://seedbox.invalid", "--username", "alice", "--password-stdin"}
	tooManyRoots := []string{"reconcile", "report", "--torrent", torrentPath}
	for index := 0; index <= storage.DefaultInventoryLimits().MaxRoots; index++ {
		tooManyRoots = append(tooManyRoots, "--search-root", searchRoot)
	}
	tooManyRoots = append(tooManyRoots, clientGroup...)
	tests := [][]string{
		{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--url", "https://seedbox.invalid"},
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--output", "yaml"}, clientGroup...),
		{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--driver", "qbittorrent", "--url", "http://example.invalid", "--username", "alice", "--password-stdin"},
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--max-entries", "0"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--host-root", searchRoot, "--client-root", "relative"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--site-ref", "tjupt/one/two"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--site-ref="}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--site-ref", "tjupt/<script>"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--site-ref", "tjupt/" + strings.Repeat("x", 257)}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", missingTorrent, "--search-root", searchRoot}, clientGroup...),
		tooManyRoots,
	}
	for _, args := range tests {
		reader := &trackingReader{}
		var out, errOut bytes.Buffer
		if code := Run(args, reader, &out, &errOut); code == 0 {
			t.Fatalf("expected rejected command: %v", args)
		}
		if reader.read {
			t.Fatalf("password input was read before validation for %v", args)
		}
	}
}

func TestReconcileReportClientFailureIsSafeStructuredEvidence(t *testing.T) {
	torrentPath, searchRoot, _ := writeReconciliationFixture(t)
	const password = "CLIENT-FAILURE-PASSWORD-CANARY"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("REMOTE-AUTH-BODY-CANARY"))
	}))
	defer server.Close()
	var out, errOut bytes.Buffer
	code := Run([]string{
		"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot,
		"--driver", "qbittorrent", "--url", server.URL, "--username", "alice", "--password-stdin", "--output", "json",
	}, strings.NewReader(password), &out, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "incomplete" || response.Data.Ledgers.Downloader.StopReason != "client_session_failed" || response.Data.Ledgers.Downloader.RequestsMade != 1 || response.Data.Ledgers.Storage.Status != "verified_unique" {
		t.Fatalf("client failure erased or polluted storage evidence: %s", out.String())
	}
	if strings.Contains(out.String()+errOut.String(), password) || strings.Contains(out.String()+errOut.String(), server.URL) || strings.Contains(out.String()+errOut.String(), "REMOTE-AUTH-BODY-CANARY") {
		t.Fatalf("unsafe client diagnostic: stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestReconcileReportRequireReconciledExitsFourAfterJSON(t *testing.T) {
	torrentPath, searchRoot, _ := writeReconciliationFixture(t)
	var out, errOut bytes.Buffer
	code := Run([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--require-reconciled", "--output", "json"}, strings.NewReader(""), &out, &errOut)
	if code != 4 || !strings.Contains(out.String(), `"kind": "ledger.reconciliation"`) || !strings.Contains(out.String(), `"outcome": "partial"`) || !strings.Contains(errOut.String(), "outcome is not consistent") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestReconcileReportHelpAndHumanOrderAreExplicit(t *testing.T) {
	var helpOut, helpErr bytes.Buffer
	if code := Run([]string{"reconcile", "report", "--help"}, strings.NewReader(""), &helpOut, &helpErr); code != 0 || helpErr.Len() != 0 || !strings.Contains(helpOut.String(), "Client flags are one optional group") || !strings.Contains(helpOut.String(), "max-candidate-edges") || !strings.Contains(helpOut.String(), "require-reconciled") || !strings.Contains(helpOut.String(), "lexical only") {
		t.Fatalf("code/help stdout=%q stderr=%q", helpOut.String(), helpErr.String())
	}

	torrentPath, searchRoot, _ := writeReconciliationFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	text := out.String()
	blockers := strings.Index(text, "BLOCKERS")
	relations := strings.Index(text, "RELATIONS")
	ledgers := strings.Index(text, "LEDGERS")
	scan := strings.Index(text, "STORAGE SCAN")
	matches := strings.Index(text, "VERIFIED STORAGE MATCHES")
	if blockers < 0 || relations <= blockers || ledgers <= relations || scan <= ledgers || matches <= scan || !strings.Contains(text, "METAFILE VARIANT NOTE") || !strings.Contains(text, "PATH NOTE") || !strings.Contains(text, "lexical only") {
		t.Fatalf("unclear reconciliation human order: %q", text)
	}
}

func writeReconciliationFixture(t *testing.T) (string, string, *metafile.MetaInfo) {
	t.Helper()
	const name = "PTCTL-CLIENT-PATH-CANARY.bin"
	content := []byte("content")
	torrentPath := filepath.Join(t.TempDir(), "reconcile.torrent")
	if err := os.WriteFile(torrentPath, testV1Metafile(name, content), 0o600); err != nil {
		t.Fatal(err)
	}
	searchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(searchRoot, name), content, 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := metafile.Read(torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	return torrentPath, searchRoot, meta
}
