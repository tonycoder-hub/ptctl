package cli

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const (
	reconciliationClientRoot     = "/downloads"
	reconciliationClientUser     = "CLIENT-USERNAME-CANARY"
	reconciliationClientPassword = "DOWNLOADER-PASSWORD-CANARY"
)

type reconciliationClientRequests struct {
	login atomic.Int32
	jobs  atomic.Int32
	files atomic.Int32
}

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
	if response.Data.Scope.ClientFileLayoutMode != "auto" || response.Data.Ledgers.Downloader.FileLayout.Status != "not_applicable" || response.Data.Ledgers.Downloader.FileLayout.RequestsMade != 0 {
		t.Fatalf("single-file reconciliation unexpectedly read a file ledger: %#v", response.Data.Ledgers.Downloader.FileLayout)
	}
	assertJSONStringsExclude(t, out.Bytes(), torrentPath, searchRoot, filepath.Join(searchRoot, "PTCTL-CLIENT-PATH-CANARY.bin"), clientPath, server.URL, clientUser, password, magnet, magnetCanary)
}

func TestReconcileConsumesOnlyAnExplicitSealedSiteBindingRecord(t *testing.T) {
	storeRoot, variantID, recordID, searchRoot, adapter := prepareStoredSiteBinding(t)
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.reconcileReport([]string{
		"--metafile-store", storeRoot, "--metafile-variant", variantID,
		"--site-binding-record", recordID, "--site-ref", "fakept/42",
		"--search-root", searchRoot, "--output", "json",
	}); err != nil {
		t.Fatalf("reconcile stored binding: %v stdout=%s", err, out.String())
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "partial" || !response.Data.Scope.SiteBindingRequested ||
		response.Data.Scope.SiteBindingSelector != "explicit_record" ||
		relationStatusCLI(response.Data, "site_metafile") != "historical_observed_exact_variant" ||
		!response.Data.Ledgers.Site.ProcessLocalProof || response.Data.Ledgers.Site.BindingRecordID != recordID {
		t.Fatalf("explicit binding was not represented exactly: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), storeRoot, filepath.Join(storeRoot, "objects"), "SITE-BINDING-PASSKEY-CANARY")

	out.Reset()
	if err := a.reconcileReport([]string{
		"--metafile-store", storeRoot, "--metafile-variant", variantID,
		"--site-binding-record", recordID, "--site-ref", "fakept/43",
		"--search-root", searchRoot, "--output", "json",
	}); err != nil {
		t.Fatalf("mismatch should still produce a report: %v stdout=%s", err, out.String())
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "conflict" || relationStatusCLI(response.Data, "site_metafile") != "selected_binding_mismatch" {
		t.Fatalf("explicit expected ref mismatch did not fail closed: %s", out.String())
	}
}

func TestInvalidSiteBindingStopsBeforeDownloaderCredentialOrRequest(t *testing.T) {
	storeRoot, variantID, _, searchRoot, adapter := prepareStoredSiteBinding(t)
	missingID := "sha256:" + strings.Repeat("1", 64)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	reader := &trackingReader{}
	var out bytes.Buffer
	a := &app{stdin: reader, stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.reconcileReport([]string{
		"--metafile-store", storeRoot, "--metafile-variant", variantID,
		"--site-binding-record", missingID, "--search-root", searchRoot,
		"--driver", "qbittorrent", "--url", server.URL, "--username", "user", "--password-stdin",
		"--output", "json",
	}); err != nil {
		t.Fatalf("missing explicit binding should produce an incomplete report: %v stdout=%s", err, out.String())
	}
	if reader.read || requests.Load() != 0 || !strings.Contains(out.String(), `"outcome": "incomplete"`) || !strings.Contains(out.String(), "site.binding_proof_unavailable") {
		t.Fatalf("binding preflight crossed credential/network boundary: read=%t requests=%d stdout=%s", reader.read, requests.Load(), out.String())
	}
}

func TestCorruptExplicitSiteBindingReportsIntegrityBeforeCredential(t *testing.T) {
	storeRoot, variantID, recordID, searchRoot, adapter := prepareStoredSiteBinding(t)
	digest := strings.TrimPrefix(recordID, "sha256:")
	recordPath := filepath.Join(storeRoot, "objects", "record-site.metafile.binding.v1-"+digest+".sealed")
	if err := os.WriteFile(recordPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := &trackingReader{}
	var out bytes.Buffer
	a := &app{stdin: reader, stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	err := a.reconcileReport([]string{
		"--metafile-store", storeRoot, "--metafile-variant", variantID,
		"--site-binding-record", recordID, "--search-root", searchRoot,
		"--driver", "qbittorrent", "--url", "http://127.0.0.1:1", "--username", "user", "--password-stdin",
		"--output", "json",
	})
	var integrity *integrityErr
	if !errors.As(err, &integrity) || reader.read || !strings.Contains(out.String(), `"outcome": "integrity_failed"`) || strings.Contains(out.String(), recordPath) {
		t.Fatalf("corrupt binding boundary failed: err=%v read=%t stdout=%s", err, reader.read, out.String())
	}
}

func TestReconcileReportMultiFileLayoutUsesFiveRequestsAndReconciles(t *testing.T) {
	torrentPath, searchRoot, hostRoot, fileNames, meta := writeMultiFileReconciliationFixture(t)
	const jobKey = "OPAQUE-JOB-KEY-CANARY"
	server, requests := newMultiFileReconciliationServer(t, meta, fileNames, []string{jobKey}, 0)
	defer server.Close()

	var out, errOut bytes.Buffer
	code := Run([]string{
		"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot,
		"--driver", "qbittorrent", "--url", server.URL, "--username", reconciliationClientUser, "--password-stdin",
		"--host-root", hostRoot, "--client-root", reconciliationClientRoot, "--client-style", "posix",
		"--max-client-files", "8", "--max-client-file-path-bytes", "4096", "--max-client-file-response-bytes", "65536",
		"--timeout", "1m", "--output", "json",
	}, strings.NewReader(reconciliationClientPassword+"\n"), &out, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if requests.login.Load() != 1 || requests.jobs.Load() != 2 || requests.files.Load() != 2 {
		t.Fatalf("login=%d jobs=%d files=%d; wanted five-request bracket", requests.login.Load(), requests.jobs.Load(), requests.files.Load())
	}
	var response struct {
		Kind string           `json:"kind"`
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	layout := response.Data.Ledgers.Downloader.FileLayout
	if response.Kind != "ledger.reconciliation" || response.Data.Outcome != "consistent" || response.Data.Ledgers.Downloader.RequestsMade != 5 || response.Data.Scope.ClientFileLayoutMode != "auto" {
		t.Fatalf("unexpected multi-file reconciliation: %s", out.String())
	}
	if layout.Status != "observed_stable" || layout.RequestsMade != 2 || layout.FilesExpected != 2 || layout.FilesObserved != 2 || layout.FilesSelected != 2 || layout.FilesComplete != 2 || layout.Limits.MaxFiles != 8 || layout.Limits.MaxPathBytes != 4096 || layout.Limits.MaxResponseBytes != 65536 || len(layout.StopReasons) != 0 || len(layout.Findings) != 0 {
		t.Fatalf("unexpected bounded file ledger: %#v", layout)
	}
	assertJSONStringsExclude(t, out.Bytes(), torrentPath, searchRoot, hostRoot, server.URL, reconciliationClientUser, reconciliationClientPassword, jobKey,
		reconciliationClientRoot+"/bundle/"+fileNames[0], reconciliationClientRoot+"/bundle/"+fileNames[1])
}

func TestReconcileReportFileLayoutOffAndAmbiguousDoNotReadJobFiles(t *testing.T) {
	for _, test := range []struct {
		name        string
		jobKeys     []string
		extra       []string
		wantStatus  string
		wantOutcome string
	}{
		{name: "off", jobKeys: []string{"opaque-job-one"}, extra: []string{"--client-file-layout", "off"}, wantStatus: "not_requested", wantOutcome: "partial"},
		{name: "ambiguous", jobKeys: []string{"opaque-job-one", "opaque-job-two"}, wantStatus: "not_attempted", wantOutcome: "ambiguous"},
	} {
		t.Run(test.name, func(t *testing.T) {
			torrentPath, searchRoot, hostRoot, fileNames, meta := writeMultiFileReconciliationFixture(t)
			server, requests := newMultiFileReconciliationServer(t, meta, fileNames, test.jobKeys, 0)
			defer server.Close()
			args := []string{
				"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot,
				"--driver", "qbittorrent", "--url", server.URL, "--username", reconciliationClientUser, "--password-stdin",
				"--host-root", hostRoot, "--client-root", reconciliationClientRoot, "--output", "json",
			}
			args = append(args, test.extra...)
			var out, errOut bytes.Buffer
			if code := Run(args, strings.NewReader(reconciliationClientPassword), &out, &errOut); code != 0 || errOut.Len() != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
			}
			if requests.login.Load() != 1 || requests.jobs.Load() != 2 || requests.files.Load() != 0 {
				t.Fatalf("login=%d jobs=%d files=%d; file endpoint must not fan out", requests.login.Load(), requests.jobs.Load(), requests.files.Load())
			}
			var response struct {
				Data reconcile.Report `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Data.Ledgers.Downloader.RequestsMade != 3 || response.Data.Ledgers.Downloader.FileLayout.Status != test.wantStatus || response.Data.Outcome != test.wantOutcome {
				t.Fatalf("unexpected no-file-read report: %s", out.String())
			}
		})
	}
}

func TestReconcileReportFileReadFailureIsReportFirst(t *testing.T) {
	torrentPath, searchRoot, hostRoot, fileNames, meta := writeMultiFileReconciliationFixture(t)
	server, requests := newMultiFileReconciliationServer(t, meta, fileNames, []string{"opaque-job-key"}, http.StatusInternalServerError)
	defer server.Close()
	var out, errOut bytes.Buffer
	code := Run([]string{
		"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot,
		"--driver", "qbittorrent", "--url", server.URL, "--username", reconciliationClientUser, "--password-stdin",
		"--host-root", hostRoot, "--client-root", reconciliationClientRoot, "--output", "json",
	}, strings.NewReader(reconciliationClientPassword), &out, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if requests.login.Load() != 1 || requests.jobs.Load() != 2 || requests.files.Load() != 1 {
		t.Fatalf("login=%d jobs=%d files=%d; failed before read must not trigger an after file read", requests.login.Load(), requests.jobs.Load(), requests.files.Load())
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	layout := response.Data.Ledgers.Downloader.FileLayout
	if response.Data.Outcome != "incomplete" || response.Data.Ledgers.Storage.Status != "verified_unique" || response.Data.Ledgers.Downloader.RequestsMade != 4 || layout.Status != "incomplete" || layout.RequestsMade != 1 || len(layout.StopReasons) != 1 || layout.StopReasons[0] != "client_file_snapshot_before_failed" {
		t.Fatalf("file failure did not become safe structured evidence: %s", out.String())
	}
	if strings.Contains(out.String(), "REMOTE-FILE-ERROR-CANARY") || strings.Contains(out.String(), server.URL) {
		t.Fatalf("unsafe remote diagnostic: %q", out.String())
	}
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
		{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--client-file-layout", "off"},
		{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--max-client-files", "2"},
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--output", "yaml"}, clientGroup...),
		{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--driver", "qbittorrent", "--url", "http://example.invalid", "--username", "alice", "--password-stdin"},
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--max-entries", "0"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--client-file-layout", "always"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--max-client-files", "0"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--max-client-file-path-bytes", "0"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--max-client-file-response-bytes", "0"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--host-root", searchRoot, "--client-root", "relative"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--site-ref", "tjupt/one/two"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--site-ref="}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--site-ref", "tjupt/<script>"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--site-ref", "tjupt/" + strings.Repeat("x", 257)}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--site-binding-record", "sha256:" + strings.Repeat("0", 64)}, clientGroup...),
		append([]string{"reconcile", "report", "--metafile-store", "unused", "--metafile-variant", "sha256:" + strings.Repeat("0", 64), "--site-binding-record", "not-an-id", "--search-root", searchRoot}, clientGroup...),
		append([]string{"reconcile", "report", "--metafile-store", "unused", "--metafile-variant", "sha256:" + strings.Repeat("0", 64), "--site-binding-record=", "--search-root", searchRoot}, clientGroup...),
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
	if code := Run([]string{"reconcile", "report", "--help"}, strings.NewReader(""), &helpOut, &helpErr); code != 0 || helpErr.Len() != 0 || !strings.Contains(helpOut.String(), "Client flags are one optional group") || !strings.Contains(helpOut.String(), "max-candidate-edges") || !strings.Contains(helpOut.String(), "client-file-layout") || !strings.Contains(helpOut.String(), "max-client-file-response-bytes") || !strings.Contains(helpOut.String(), "site-binding-record") || !strings.Contains(helpOut.String(), "at most two bounded file-list reads") || !strings.Contains(helpOut.String(), "never retried") || !strings.Contains(helpOut.String(), "require-reconciled") || !strings.Contains(helpOut.String(), "lexical only") {
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
	siteBinding := strings.Index(text, "SITE BINDING")
	ledgers := strings.Index(text, "LEDGERS")
	fileLayout := strings.Index(text, "CLIENT FILE LAYOUT (BOUNDED)")
	fileFindings := strings.Index(text, "CLIENT FILE FINDINGS (BOUNDED)")
	downloaderMatches := strings.Index(text, "DOWNLOADER MATCHES")
	scan := strings.Index(text, "STORAGE SCAN")
	matches := strings.Index(text, "VERIFIED STORAGE MATCHES")
	bindings := strings.Index(text, "VERIFIED STORAGE BINDINGS (BOUNDED)")
	if blockers < 0 || relations <= blockers || siteBinding <= relations || ledgers <= siteBinding || fileLayout <= ledgers || fileFindings <= fileLayout || downloaderMatches <= fileFindings || scan <= downloaderMatches || matches <= scan || bindings <= matches || !strings.Contains(text, "METAFILE VARIANT NOTE") || !strings.Contains(text, "PATH NOTE") || !strings.Contains(text, "lexical only") || !strings.Contains(text, "CONTENT PATH") || !strings.Contains(text, "BEFORE FILES CONSIDERED") {
		t.Fatalf("unclear reconciliation human order: %q", text)
	}
}

func TestReconcileHumanShowAbsolutePathsRendersHostAndClientBindings(t *testing.T) {
	torrentPath, searchRoot, hostRoot, fileNames, meta := writeMultiFileReconciliationFixture(t)
	server, requests := newMultiFileReconciliationServer(t, meta, fileNames, []string{"opaque-job-key"}, 0)
	defer server.Close()
	var out, errOut bytes.Buffer
	code := Run([]string{
		"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot,
		"--driver", "qbittorrent", "--url", server.URL, "--username", reconciliationClientUser, "--password-stdin",
		"--host-root", hostRoot, "--client-root", reconciliationClientRoot, "--show-absolute-paths", "--output", "table",
	}, strings.NewReader(reconciliationClientPassword), &out, &errOut)
	if code != 0 || errOut.Len() != 0 || requests.login.Load() != 1 || requests.jobs.Load() != 2 || requests.files.Load() != 2 {
		t.Fatalf("code=%d requests=%d/%d/%d stdout=%q stderr=%q", code, requests.login.Load(), requests.jobs.Load(), requests.files.Load(), out.String(), errOut.String())
	}
	expectedHostPath, err := filepath.EvalSymlinks(filepath.Join(searchRoot, fileNames[0]))
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, expected := range []string{
		expectedHostPath,
		reconciliationClientRoot + "/bundle/" + fileNames[0],
		reconciliationClientRoot + "/bundle",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("--show-absolute-paths did not render %q: %q", expected, text)
		}
	}
}

func prepareStoredSiteBinding(t *testing.T) (storeRoot, variantID, recordID, searchRoot string, adapter *fakeMetafileFetchAdapter) {
	t.Helper()
	storeRoot = filepath.Join(physicalCLITempDir(t), "SITE-BINDING-STORE-PATH-CANARY")
	if _, _, err := metastore.Init(storeRoot); err != nil {
		t.Fatal(err)
	}
	content := []byte("payload")
	adapter = &fakeMetafileFetchAdapter{raw: sitePrivateTrackerMetafile("source.bin", content, "SITE-BINDING-PASSKEY-CANARY")}
	var fetchOut bytes.Buffer
	fetchApp := &app{stdin: strings.NewReader("SID=site-binding-test\n"), stdout: &fetchOut, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := fetchApp.siteMetafileFetch([]string{
		"--cookie-stdin", "--acknowledge-site-effect", "--metafile-store", storeRoot,
		"--output", "json", "fakept", "42",
	}); err != nil {
		t.Fatalf("prepare site binding: %v stdout=%s", err, fetchOut.String())
	}
	var fetchResponse struct {
		Data struct {
			StoreOperation struct {
				Artifact struct {
					VariantID string `json:"metafile_variant_id"`
				} `json:"artifact"`
			} `json:"store_operation"`
			Persistent struct {
				Record struct {
					ID string `json:"record_id"`
				} `json:"record"`
			} `json:"persistent_binding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(fetchOut.Bytes(), &fetchResponse); err != nil {
		t.Fatal(err)
	}
	variantID = fetchResponse.Data.StoreOperation.Artifact.VariantID
	recordID = fetchResponse.Data.Persistent.Record.ID
	if variantID == "" || recordID == "" {
		t.Fatalf("fetch did not return durable handoff IDs: %s", fetchOut.String())
	}
	searchRoot = t.TempDir()
	if err := os.WriteFile(filepath.Join(searchRoot, "renamed.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	return storeRoot, variantID, recordID, searchRoot, adapter
}

func relationStatusCLI(report reconcile.Report, kind string) string {
	for _, relation := range report.Relations {
		if relation.Kind == kind {
			return relation.Status
		}
	}
	return ""
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

func writeMultiFileReconciliationFixture(t *testing.T) (torrentPath, searchRoot, hostRoot string, fileNames []string, meta *metafile.MetaInfo) {
	t.Helper()
	data := []byte("abcdef")
	torrentPath = filepath.Join(t.TempDir(), "reconcile-multi.torrent")
	if err := os.WriteFile(torrentPath, testV1MultiFileMetafile(data), 0o600); err != nil {
		t.Fatal(err)
	}
	hostRoot = t.TempDir()
	searchRoot = filepath.Join(hostRoot, "bundle")
	if err := os.Mkdir(searchRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	fileNames = []string{"PTCTL-RENAMED-ONE-CANARY", "PTCTL-RENAMED-TWO-CANARY"}
	if err := os.WriteFile(filepath.Join(searchRoot, fileNames[0]), data[:3], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(searchRoot, fileNames[1]), data[3:], 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	meta, err = metafile.Read(torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	return torrentPath, searchRoot, hostRoot, fileNames, meta
}

func testV1MultiFileMetafile(content []byte) []byte {
	piece0 := sha1.Sum(content[:4])
	piece1 := sha1.Sum(content[4:])
	result := []byte("d4:infod5:filesld6:lengthi3e4:pathl5:a.bineed6:lengthi3e4:pathl5:b.bineee4:name6:bundle12:piece lengthi4e6:pieces40:")
	result = append(result, piece0[:]...)
	result = append(result, piece1[:]...)
	return append(result, 'e', 'e')
}

func newMultiFileReconciliationServer(t *testing.T, meta *metafile.MetaInfo, fileNames, jobKeys []string, fileStatus int) (*httptest.Server, *reconciliationClientRequests) {
	t.Helper()
	requests := &reconciliationClientRequests{}
	magnet := "magnet:?xt=urn:btih:" + meta.InfoHashV1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			requests.login.Add(1)
			if r.Method != http.MethodPost {
				t.Errorf("login method=%s", r.Method)
			}
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse login form: %v", err)
			}
			if r.Form.Get("username") != reconciliationClientUser || r.Form.Get("password") != reconciliationClientPassword {
				t.Errorf("unexpected login credential")
			}
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "ok", Path: "/"})
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			requests.jobs.Add(1)
			assertReconciliationSessionCookie(t, r)
			rows := make([]map[string]any, 0, len(jobKeys))
			for _, jobKey := range jobKeys {
				rows = append(rows, map[string]any{
					"hash": jobKey, "magnet_uri": magnet, "name": "bundle", "size": int64(6),
					"progress": 1.0, "state": "uploading", "save_path": reconciliationClientRoot,
					"content_path": reconciliationClientRoot + "/bundle", "downloaded": int64(6), "uploaded": int64(10),
				})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(rows)
		case "/api/v2/torrents/files":
			requests.files.Add(1)
			assertReconciliationSessionCookie(t, r)
			if len(jobKeys) != 1 || r.URL.Query().Get("hash") != jobKeys[0] {
				t.Errorf("unexpected file-ledger job locator")
			}
			if fileStatus != 0 {
				w.WriteHeader(fileStatus)
				_, _ = w.Write([]byte("REMOTE-FILE-ERROR-CANARY"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"index": 0, "name": "bundle/" + fileNames[0], "size": int64(3), "progress": 1.0, "priority": 1, "is_seed": true},
				{"index": 1, "name": "bundle/" + fileNames[1], "size": int64(3), "progress": 1.0, "priority": 1, "is_seed": true},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	return server, requests
}

func assertReconciliationSessionCookie(t *testing.T, request *http.Request) {
	t.Helper()
	cookie, err := request.Cookie("SID")
	if err != nil || cookie.Value != "ok" {
		t.Errorf("downloader read did not reuse authenticated session")
	}
}
