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
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/clientremove"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/sourceretire"
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

func TestReconcileObservesLiveSiteAndDownloaderFromOneStrictCredentialBundle(t *testing.T) {
	torrentPath, searchRoot, meta := writeReconciliationFixture(t)
	const (
		clientRoot   = "/downloads"
		clientPath   = "/downloads/PTCTL-CLIENT-PATH-CANARY.bin"
		siteCookie   = "sid=LIVE-SITE-COOKIE-CANARY"
		clientUser   = "bundle-user"
		clientSecret = "BUNDLE-DOWNLOADER-PASSWORD-CANARY"
	)
	var loginRequests, listRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			loginRequests.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("username") != clientUser || r.Form.Get("password") != clientSecret {
				t.Errorf("unexpected bundled downloader credential")
			}
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "ok", Path: "/"})
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			listRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"hash": "opaque", "magnet_uri": "magnet:?xt=urn:btih:" + meta.InfoHashV1,
				"name": "PTCTL-CLIENT-PATH-CANARY.bin", "size": int64(len("content")), "progress": 1.0,
				"state": "uploading", "save_path": clientRoot, "content_path": clientPath,
				"downloaded": int64(len("content")), "uploaded": int64(10),
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter := successfulFakeTorrentDetail(t)
	bundle, err := json.Marshal(map[string]string{
		"schema": reconciliationCredentialSchema, "site_cookie": siteCookie, "downloader_password": clientSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{stdin: bytes.NewReader(bundle), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.reconcileReport([]string{
		"--torrent", torrentPath, "--search-root", searchRoot,
		"--site-ref", "fakept/42", "--credential-bundle-stdin",
		"--driver", "qbittorrent", "--url", server.URL, "--username", clientUser,
		"--host-root", searchRoot, "--client-root", clientRoot, "--client-style", "posix",
		"--timeout", "1m", "--output", "json",
	}); err != nil {
		t.Fatalf("reconcile live site/client: %v stdout=%s", err, out.String())
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	detail := response.Data.Ledgers.Site.Detail
	if response.Data.Outcome != "consistent" || !response.Data.Scope.SiteDetailRequested || detail.Status != "observed_current_ref" ||
		!detail.ProcessLocalProof || detail.Observation == nil || detail.Observation.DisplayTitle != "Safe Release" || detail.RequestsMade != 1 ||
		relationStatusCLI(response.Data, "site_metafile") != "declared_unbound" || !strings.Contains(response.Data.Assurance, "same_invocation_current_site_ref_claim") ||
		adapter.opened != 1 || adapter.credential != siteCookie || loginRequests.Load() != 1 || listRequests.Load() != 2 {
		t.Fatalf("live site axis was not kept separate: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), siteCookie, clientSecret, server.URL, clientUser, torrentPath, searchRoot, clientPath)

	const failureCanary = "SITE-DETAIL-FAILURE-BODY-URL-CANARY"
	adapter.receipt.Complete = false
	adapter.receipt.StopReason = "site_request_failed"
	adapter.readErr = errors.New(failureCanary)
	a.stdin = bytes.NewReader(bundle)
	out.Reset()
	if err := a.reconcileReport([]string{
		"--torrent", torrentPath, "--search-root", searchRoot,
		"--site-ref", "fakept/42", "--credential-bundle-stdin",
		"--driver", "qbittorrent", "--url", server.URL, "--username", clientUser,
		"--host-root", searchRoot, "--client-root", clientRoot, "--client-style", "posix",
		"--timeout", "1m", "--output", "json",
	}); err != nil {
		t.Fatalf("failed site axis should still produce a report: %v stdout=%s", err, out.String())
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "incomplete" || response.Data.Ledgers.Site.Detail.Status != "incomplete" ||
		relationStatusCLI(response.Data, "client_infohash_relation") != "exact_unique" || !response.Data.Ledgers.Storage.ProcessLocalProof ||
		loginRequests.Load() != 2 || listRequests.Load() != 4 || strings.Contains(out.String(), failureCanary) {
		t.Fatalf("site failure erased another axis or leaked diagnostics: %s", out.String())
	}
}

func TestReconcileObservesLiveSiteWithoutDownloader(t *testing.T) {
	torrentPath, searchRoot, _ := writeReconciliationFixture(t)
	const cookie = "sid=SITE-ONLY-COOKIE-CANARY"
	adapter := successfulFakeTorrentDetail(t)
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader(cookie), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.reconcileReport([]string{
		"--torrent", torrentPath, "--search-root", searchRoot, "--site-ref", "fakept/42", "--site-cookie-stdin", "--output", "json",
	}); err != nil {
		t.Fatalf("site-only reconciliation: %v stdout=%s", err, out.String())
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "partial" || !response.Data.Scope.SiteDetailRequested || response.Data.Scope.ClientRequested ||
		response.Data.Ledgers.Site.Detail.Status != "observed_current_ref" || response.Data.Ledgers.Downloader.Status != "not_requested" ||
		adapter.credential != cookie {
		t.Fatalf("site-only observation was not isolated: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), cookie, torrentPath, searchRoot)
}

func TestReconcileLiveSiteCredentialModesRejectBeforeStdin(t *testing.T) {
	torrentPath, searchRoot, _ := writeReconciliationFixture(t)
	base := []string{"--torrent", torrentPath, "--search-root", searchRoot, "--output", "json"}
	tests := [][]string{
		append(append([]string{}, base...), "--site-cookie-stdin"),
		append(append([]string{}, base...), "--site-ref", "fakept/42", "--site-cookie-stdin", "--driver", "qbittorrent", "--url", "http://127.0.0.1:1", "--username", "user", "--password-stdin"),
		append(append([]string{}, base...), "--site-ref", "fakept/42", "--credential-bundle-stdin", "--password-stdin", "--driver", "qbittorrent", "--url", "http://127.0.0.1:1", "--username", "user"),
		append(append([]string{}, base...), "--site-ref", "fakept/42", "--credential-bundle-stdin"),
	}
	for index, args := range tests {
		reader := &trackingReader{}
		var out bytes.Buffer
		a := &app{stdin: reader, stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(successfulFakeTorrentDetail(t))}
		if err := a.reconcileReport(args); err == nil || reader.read || out.Len() != 0 {
			t.Fatalf("case %d crossed credential boundary: err=%v read=%t out=%q", index, err, reader.read, out.String())
		}
	}
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

func TestReconcileReportExactSourceReconcilesPhysicalEmptyFileEndToEnd(t *testing.T) {
	torrentPath, sourceRoot, hostRoot, fileNames, meta := writeExactEmptyReconciliationFixture(t)
	const jobKey = "OPAQUE-EMPTY-JOB-KEY-CANARY"
	server, requests := newMultiFileReconciliationServer(t, meta, fileNames, []string{jobKey}, 0)
	defer server.Close()

	var out, errOut bytes.Buffer
	code := Run([]string{
		"reconcile", "report", "--torrent", torrentPath, "--source", sourceRoot,
		"--driver", "qbittorrent", "--url", server.URL, "--username", reconciliationClientUser, "--password-stdin",
		"--host-root", hostRoot, "--client-root", reconciliationClientRoot, "--client-style", "posix",
		"--timeout", "1m", "--output", "json",
	}, strings.NewReader(reconciliationClientPassword), &out, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if requests.login.Load() != 1 || requests.jobs.Load() != 2 || requests.files.Load() != 2 {
		t.Fatalf("login=%d jobs=%d files=%d; wanted exact-source five-request bracket", requests.login.Load(), requests.jobs.Load(), requests.files.Load())
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	layout := response.Data.Ledgers.Downloader.FileLayout
	if response.Data.Outcome != "consistent" || response.Data.Ledgers.Storage.Status != "verified_exact_root" || !response.Data.Ledgers.Storage.ProcessLocalProof ||
		relationStatusCLI(response.Data, "storage_content_proof") != "verified_exact_root" || relationStatusCLI(response.Data, "verified_source_vs_job_path") != "same_location" ||
		layout.Status != "observed_stable" || layout.FilesExpected != 2 || layout.FilesObserved != 2 || layout.FilesSelected != 2 || layout.FilesComplete != 2 {
		t.Fatalf("physical empty file did not close exact reconciliation: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), torrentPath, sourceRoot, hostRoot, server.URL, reconciliationClientUser, reconciliationClientPassword, jobKey)
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

func TestReconcileReportExactSourceIsSelectedProofWithoutUniquenessClaim(t *testing.T) {
	torrentPath, sourceRoot, _ := writeReconciliationFixture(t)
	sourcePath := filepath.Join(sourceRoot, "PTCTL-CLIENT-PATH-CANARY.bin")
	var out, errOut bytes.Buffer
	code := Run([]string{"reconcile", "report", "--torrent", torrentPath, "--source", sourcePath, "--output", "json"}, strings.NewReader(""), &out, &errOut)
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
	storageRelation := relationStatusCLI(response.Data, "storage_content_proof")
	if response.Kind != "ledger.reconciliation" || response.Data.Outcome != "partial" ||
		response.Data.Ledgers.Storage.Status != "verified_exact_root" || storageRelation != "verified_exact_root" ||
		!response.Data.Ledgers.Storage.ProcessLocalProof || response.Data.Ledgers.Storage.SelectedSourceID == "" ||
		response.Data.Ledgers.Storage.Discovery.Selection.Basis != "explicit_exact_root_full_layout_verified" ||
		response.Data.Ledgers.Storage.Discovery.Scan.PathConfinement != "explicit_exact_root" {
		t.Fatalf("exact source proof was mislabeled: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), torrentPath, sourceRoot, sourcePath)

	out.Reset()
	errOut.Reset()
	code = Run([]string{"reconcile", "report", "--torrent", torrentPath, "--source", sourcePath}, strings.NewReader(""), &out, &errOut)
	if code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), "SOURCE SCOPE") || !strings.Contains(out.String(), "explicit_exact_root") {
		t.Fatalf("exact source scope was not explicit in the table: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestReconcileReportExplicitMaterializedFinalBridgesCurrentProofAndRejectsTampering(t *testing.T) {
	fixture := newMaterializeCLIFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{
		"seed", "materialize", "run", "--torrent", fixture.torrentPath,
		"--search-root", fixture.sourceRoot, "--target", fixture.targetRoot,
		"--expect-plan-id", fixture.planID, "--acknowledge-filesystem-write", "--output", "json",
	}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("materialize code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	operationID := decodeMaterializeReport(t, out.Bytes()).Data.Operation.ID
	meta, err := metafile.Read(fixture.torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	const clientRoot = "/downloads"
	clientPath := clientRoot + "/" + materializeFinalName
	var loginRequests, listRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			loginRequests.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "materialized", Path: "/"})
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			listRequests.Add(1)
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"hash": "opaque-materialized-job", "magnet_uri": "magnet:?xt=urn:btih:" + meta.InfoHashV1,
				"name": materializeFinalName, "size": int64(len(fixture.content)), "progress": 1.0,
				"state": "uploading", "save_path": clientRoot, "content_path": clientPath,
				"downloaded": int64(len(fixture.content)), "uploaded": int64(1),
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	out.Reset()
	errOut.Reset()
	args := []string{
		"reconcile", "report", "--torrent", fixture.torrentPath,
		"--target", fixture.targetRoot, "--materialize-operation", operationID, "--materialize-plan-id", fixture.planID,
		"--driver", "qbittorrent", "--url", server.URL, "--username", reconciliationClientUser, "--password-stdin",
		"--host-root", fixture.targetRoot, "--client-root", clientRoot, "--client-style", "posix", "--output", "json",
	}
	if code := Run(args, strings.NewReader(reconciliationClientPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("reconcile code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var response struct {
		Kind string           `json:"kind"`
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	materialized := response.Data.Ledgers.Storage.MaterializedFinal
	if response.Kind != "ledger.reconciliation" || response.Data.Outcome != "consistent" || response.Data.WritesPerformed != 0 ||
		!response.Data.Scope.MaterializedFinalRequested || response.Data.Ledgers.Storage.Status != "verified_materialized_final" ||
		relationStatusCLI(response.Data, "storage_content_proof") != "verified_materialized_final" ||
		materialized.Status != "verified_current_final_source" || !materialized.ProcessLocalFinalProof || !materialized.ProcessLocalSourceBridge ||
		materialized.Observation == nil || materialized.Observation.OperationID != operationID ||
		response.Data.Ledgers.Storage.Discovery.SourceOutcome != "verified_exact_root" ||
		!strings.Contains(response.Data.Assurance, "explicit_materialized_final") {
		t.Fatalf("materialized final was not represented as paired current proof: %s", out.String())
	}
	if loginRequests.Load() != 1 || listRequests.Load() != 2 {
		t.Fatalf("materialized proof was not inside one client bracket: login=%d list=%d", loginRequests.Load(), listRequests.Load())
	}
	assertJSONStringsExclude(t, out.Bytes(), fixture.targetRoot, fixture.sourceRoot, fixture.sourcePath, fixture.finalPath, fixture.torrentPath, clientPath, server.URL, reconciliationClientPassword)

	out.Reset()
	errOut.Reset()
	if code := Run([]string{
		"reconcile", "report", "--torrent", fixture.torrentPath,
		"--target", fixture.targetRoot, "--materialize-operation", operationID,
		"--materialize-plan-id", strings.Repeat("c", 24), "--output", "json",
	}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("wrong-plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	materialized = response.Data.Ledgers.Storage.MaterializedFinal
	if response.Data.Outcome != "incomplete" || response.Data.Ledgers.Storage.Status != "incomplete" ||
		materialized.Status != "incomplete" || materialized.ProcessLocalFinalProof || materialized.ProcessLocalSourceBridge ||
		materialized.StopReason != "materialized_final_policy_blocked" {
		t.Fatalf("wrong reviewed plan fell back to path proof: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), fixture.targetRoot, fixture.sourceRoot, fixture.sourcePath, fixture.finalPath, fixture.torrentPath)

	if err := os.WriteFile(fixture.finalPath, bytes.Repeat([]byte{'x'}, len(fixture.content)), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{
		"reconcile", "report", "--torrent", fixture.torrentPath,
		"--target", fixture.targetRoot, "--materialize-operation", operationID, "--materialize-plan-id", fixture.planID, "--output", "json",
	}, strings.NewReader(""), &out, &errOut); code != 3 {
		t.Fatalf("tampered code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	materialized = response.Data.Ledgers.Storage.MaterializedFinal
	if response.Data.Outcome != "integrity_failed" || response.Data.Ledgers.Storage.Status != "integrity_failed" ||
		relationStatusCLI(response.Data, "storage_content_proof") != "integrity_failed" ||
		materialized.Status != "integrity_failed" || materialized.ProcessLocalSourceBridge ||
		materialized.StopReason != "materialized_final_integrity_failed" ||
		!hasReportFindingCLI(response.Data.Blockers, "storage.materialized_final_proof_unavailable") {
		t.Fatalf("tampered materialized final was not report-first integrity evidence: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), fixture.targetRoot, fixture.sourceRoot, fixture.sourcePath, fixture.finalPath, fixture.torrentPath)
}

func TestReconcileReportBindsTerminalAdoptionAndActivationToExistingClientBracketWithoutExtraRequests(t *testing.T) {
	fixture := newClientAdoptCLIFixture(t)
	server := newClientActivateServer(t, fixture.meta, fixture.raw)
	defer server.server.Close()
	var out, errOut bytes.Buffer

	adoptionBase := clientAdoptBaseArgs(fixture, server.server.URL)
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

	activationBase := clientActivateBaseArgs(fixture, server.server.URL, adopted.Data.Operation.ID, adopted.Data.Plan.ID)
	out.Reset()
	errOut.Reset()
	if code := Run(append([]string{"client", "activate", "plan"}, activationBase...), strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("activation plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	planned := decodeClientActivateReport(t, out.Bytes())
	out.Reset()
	errOut.Reset()
	activationRun := append([]string{"client", "activate", "run"}, activationBase...)
	activationRun = append(activationRun, "--expect-activation-plan-id", planned.Data.Plan.ID, "--acknowledge-client-recheck")
	if code := Run(activationRun, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("activation run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	running := decodeClientActivateReport(t, out.Bytes())
	server.setState("stoppedUP", 1)
	out.Reset()
	errOut.Reset()
	activationResume := append([]string{"client", "activate", "resume"}, activationBase...)
	activationResume = append(activationResume, "--expect-activation-plan-id", planned.Data.Plan.ID, running.Data.Operation.ID)
	if code := Run(activationResume, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("activation resume code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	completed := decodeClientActivateReport(t, out.Bytes())
	if completed.Data.Outcome != clientactivate.OutcomeCheckedStopped || !completed.Data.Journal.RecheckCompletionDurable {
		t.Fatalf("activation did not reach terminal completion: %s", out.String())
	}

	reconcileArgs := []string{
		"reconcile", "report", "--metafile-store", fixture.storeRoot, "--metafile-variant", fixture.variantID,
		"--target", fixture.materialize.targetRoot, "--materialize-operation", fixture.operation,
		"--materialize-plan-id", fixture.materialize.planID,
		"--adoption-operation", adopted.Data.Operation.ID, "--adoption-plan-id", adopted.Data.Plan.ID,
		"--activation-operation", completed.Data.Operation.ID, "--activation-plan-id", completed.Data.Plan.ID,
		"--driver", "qbittorrent", "--url", server.server.URL, "--username", clientAdoptUser, "--password-stdin",
		"--host-root", fixture.materialize.targetRoot, "--client-root", clientAdoptRoot, "--client-style", "posix",
		"--timeout", "1m", "--output", "json",
	}
	requestsBefore := server.totalRequests()
	out.Reset()
	errOut.Reset()
	if code := Run(reconcileArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("reconcile code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if delta := server.totalRequests() - requestsBefore; delta != 3 {
		t.Fatalf("adoption/activation reconciliation made %d requests; wanted one login plus the existing two-read bracket", delta)
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	activation := response.Data.Ledgers.Activation
	adoption := response.Data.Ledgers.Adoption
	if response.Data.Outcome != "consistent" || !response.Data.Scope.ClientAdoptionRequested || !response.Data.Scope.ClientActivationRequested ||
		adoption.Status != "historical_completion_current_job_bound" || !adoption.Historical ||
		!adoption.ProcessLocalCompletionProof || !adoption.ProcessLocalCurrentJobProof ||
		adoption.Completion == nil || adoption.CurrentJob == nil ||
		adoption.Completion.OperationID != adopted.Data.Operation.ID || adoption.CurrentJob.JobID != adoption.Completion.JobID ||
		activation.Status != "historical_completion_current_job_bound" || !activation.Historical ||
		!activation.ProcessLocalCompletionProof || !activation.ProcessLocalCurrentUseProof ||
		activation.Completion == nil || activation.CurrentUse == nil ||
		activation.Completion.OperationID != completed.Data.Operation.ID || activation.CurrentUse.JobID != activation.Completion.JobID ||
		activation.Completion.AdoptionOperationID != adoption.Completion.OperationID ||
		activation.Completion.AdoptionPlanID != adoption.Completion.PlanID ||
		activation.Completion.AdoptionCompletionID != adoption.Completion.CompletionID ||
		!strings.Contains(response.Data.Assurance, "canonical_historical_stopped_adoption_bound_to_current_exact_job_claim") ||
		!strings.Contains(response.Data.Assurance, "canonical_historical_client_activation_bound_to_current_exact_job") || len(response.Data.Relations) != 5 {
		t.Fatalf("adoption/activation axes were not kept separate and bound: %s", out.String())
	}
	var human bytes.Buffer
	if err := writeReconciliationHuman(&human, response.Data); err != nil ||
		!strings.Contains(human.String(), "CLIENT ADOPTION") ||
		!strings.Contains(human.String(), "ACTION") || !strings.Contains(human.String(), "add_stopped") ||
		!strings.Contains(human.String(), "PROCESS-LOCAL CURRENT-JOB BRIDGE  true") ||
		!strings.Contains(human.String(), "CLIENT ACTIVATION") ||
		!strings.Contains(human.String(), "PROCESS-LOCAL CURRENT-USE BRIDGE  true") ||
		strings.Index(human.String(), "CLIENT ADOPTION") > strings.Index(human.String(), "LEDGERS") ||
		strings.Index(human.String(), "CLIENT ACTIVATION") > strings.Index(human.String(), "LEDGERS") {
		t.Fatalf("adoption/activation table contract is unclear: err=%v\n%s", err, human.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), fixture.materialize.targetRoot, fixture.materialize.sourceRoot,
		fixture.materialize.sourcePath, fixture.materialize.finalPath, fixture.storeRoot, server.server.URL,
		clientAdoptUser, clientAdoptPassword, clientAdoptJobKey, clientAdoptRoot)

	out.Reset()
	errOut.Reset()
	pruneArgs := []string{"client", "adopt", "prune", "--target", fixture.materialize.targetRoot,
		"--expect-adoption-plan-id", adopted.Data.Plan.ID, "--acknowledge-operation-state-deletion", "--output", "json", adopted.Data.Operation.ID}
	if code := Run(pruneArgs, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("adoption prune code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	requestsBefore = server.totalRequests()
	out.Reset()
	errOut.Reset()
	if code := Run(reconcileArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("retained adoption reconcile code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if delta := server.totalRequests() - requestsBefore; delta != 3 {
		t.Fatalf("retained adoption reconciliation made %d requests; wanted the unchanged existing bracket", delta)
	}
	response = struct {
		Data reconcile.Report `json:"data"`
	}{}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "consistent" || response.Data.Ledgers.Adoption.Completion == nil ||
		!response.Data.Ledgers.Adoption.Completion.RetainedTombstone ||
		response.Data.Ledgers.Adoption.Status != "historical_completion_current_job_bound" ||
		!response.Data.Ledgers.Adoption.ProcessLocalCurrentJobProof {
		t.Fatalf("retained adoption did not preserve bounded historical authority: %s", out.String())
	}

	missingAdoptionPlan := strings.Repeat("e", 24)
	missingAdoptionArgs := append([]string(nil), reconcileArgs...)
	for index := range missingAdoptionArgs {
		if index > 0 && missingAdoptionArgs[index-1] == "--adoption-plan-id" {
			missingAdoptionArgs[index] = missingAdoptionPlan
		}
		if index > 0 && missingAdoptionArgs[index-1] == "--adoption-operation" {
			missingAdoptionArgs[index] = clientadopt.OperationIDForPlan(missingAdoptionPlan).String()
		}
	}
	requestsBefore = server.totalRequests()
	reader := &trackingReader{}
	out.Reset()
	errOut.Reset()
	if code := Run(missingAdoptionArgs, reader, &out, &errOut); code != 0 || errOut.Len() != 0 || reader.read {
		t.Fatalf("missing adoption selector code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
	if server.totalRequests() != requestsBefore {
		t.Fatal("missing adoption selector contacted the downloader")
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "incomplete" || response.Data.Ledgers.Adoption.Status != "incomplete" ||
		response.Data.Ledgers.Adoption.StopReason != "adoption_completion_load_failed" ||
		response.Data.Ledgers.Adoption.ProcessLocalCompletionProof || response.Data.Ledgers.Adoption.ProcessLocalCurrentJobProof {
		t.Fatalf("missing adoption selector was not a report-first incomplete gate: %s", out.String())
	}

	wrongPlan := strings.Repeat("f", 24)
	badArgs := append([]string(nil), reconcileArgs...)
	for index := range badArgs {
		if index > 0 && badArgs[index-1] == "--activation-plan-id" {
			badArgs[index] = wrongPlan
		}
	}
	requestsBefore = server.totalRequests()
	reader = &trackingReader{}
	out.Reset()
	errOut.Reset()
	if code := Run(badArgs, reader, &out, &errOut); code != 0 || errOut.Len() != 0 || reader.read {
		t.Fatalf("bad selector code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
	if server.totalRequests() != requestsBefore {
		t.Fatal("mismatched activation selector contacted the downloader")
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "incomplete" || response.Data.Ledgers.Activation.Status != "incomplete" ||
		response.Data.Ledgers.Activation.StopReason != "activation_completion_load_failed" ||
		response.Data.Ledgers.Activation.ProcessLocalCompletionProof || response.Data.Ledgers.Activation.ProcessLocalCurrentUseProof {
		t.Fatalf("mismatched selector was not a report-first incomplete gate: %s", out.String())
	}

	configMismatch := append([]string(nil), reconcileArgs...)
	for index := range configMismatch {
		if index > 0 && configMismatch[index-1] == "--username" {
			configMismatch[index] = "different-client-config"
		}
	}
	requestsBefore = server.totalRequests()
	reader = &trackingReader{}
	out.Reset()
	errOut.Reset()
	if code := Run(configMismatch, reader, &out, &errOut); code != 0 || errOut.Len() != 0 || reader.read {
		t.Fatalf("config mismatch code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
	if server.totalRequests() != requestsBefore {
		t.Fatal("mismatched client configuration contacted the downloader")
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "conflict" || response.Data.Ledgers.Activation.Status != "selected_activation_mismatch" ||
		response.Data.Ledgers.Activation.StopReason != "activation_completion_selector_mismatch" {
		t.Fatalf("positive activation/config mismatch was not a conflict: %s", out.String())
	}

	retirementPlanArgs := sourceRetireBaseArgs(fixture, server.server.URL, completed.Data.Operation.ID, completed.Data.Plan.ID)
	out.Reset()
	errOut.Reset()
	if code := Run(retirementPlanArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("retirement plan code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	retirementPlan := decodeSourceRetireReport(t, out.Bytes())
	retirementRunArgs := append([]string(nil), retirementPlanArgs...)
	retirementRunArgs[2] = "run"
	retirementRunArgs = append(retirementRunArgs, "--expect-plan-id", retirementPlan.Data.Plan.ID, "--acknowledge-source-deletion")
	out.Reset()
	errOut.Reset()
	if code := Run(retirementRunArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("retirement run code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	retired := decodeSourceRetireExecutionReport(t, out.Bytes())
	if retired.Data.Outcome != sourceretire.ExecutionOutcomeRetired || retired.Data.Operation.Resumable || !retired.Data.DeletionPerformed {
		t.Fatalf("source retirement did not reach a terminal deletion: %s", out.String())
	}
	if _, statErr := os.Lstat(fixture.materialize.sourcePath); !os.IsNotExist(statErr) {
		t.Fatalf("retired source name remains: %v", statErr)
	}
	// The complete file snapshot is stable across ordinary stopped-to-seeding
	// state transitions; retirement attribution must not bind transient state.
	server.setState("uploading", 1)

	retirementReconcileArgs := append([]string(nil), reconcileArgs...)
	retirementReconcileArgs = append(retirementReconcileArgs,
		"--retirement-operation", retired.Data.Operation.ID,
		"--retirement-plan-id", retirementPlan.Data.Plan.ID,
		"--retirement-search-root", fixture.materialize.sourceRoot,
	)
	requestsBefore = server.totalRequests()
	out.Reset()
	errOut.Reset()
	if code := Run(retirementReconcileArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("retirement reconciliation code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if delta := server.totalRequests() - requestsBefore; delta != 3 {
		t.Fatalf("retirement reconciliation made %d requests; wanted the unchanged login plus two-read client bracket", delta)
	}
	response = struct {
		Data reconcile.Report `json:"data"`
	}{}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	retirement := response.Data.Ledgers.Retirement
	if response.Data.Outcome != "consistent" || !response.Data.Scope.SourceRetirementRequested ||
		retirement.Status != "historical_completion_current_absence_observed" || !retirement.Historical ||
		!retirement.ProcessLocalCompletionProof || !retirement.ProcessLocalAbsenceProof ||
		response.Data.Ledgers.Activation.CurrentUse == nil || response.Data.Ledgers.Activation.CurrentUse.JobState != "uploading" ||
		retirement.Completion == nil || retirement.CurrentAbsence == nil ||
		retirement.Completion.OperationID != retired.Data.Operation.ID || retirement.Completion.RetainedTombstone ||
		retirement.CurrentAbsence.OperationID != retirement.Completion.OperationID ||
		retirement.CurrentAbsence.FilesChecked != retirement.Completion.FilesRetired ||
		!strings.Contains(response.Data.Assurance, "canonical_historical_source_retirement_with_current_bound_name_absence") ||
		len(response.Data.Relations) != 5 || !slices.Contains(response.Data.Effect, "read_source_retirement_operation_state") ||
		!slices.Contains(response.Data.Effect, "read_retired_source_name_absence") {
		t.Fatalf("retirement axis was not kept separate and currently rebound: %s", out.String())
	}
	human.Reset()
	if err := writeReconciliationHuman(&human, response.Data); err != nil ||
		!strings.Contains(human.String(), "SOURCE RETIREMENT") ||
		!strings.Contains(human.String(), "PROCESS-LOCAL CURRENT-ABSENCE PROOF  true") ||
		strings.Index(human.String(), "SOURCE RETIREMENT") > strings.Index(human.String(), "LEDGERS") {
		t.Fatalf("retirement table contract is unclear: err=%v\n%s", err, human.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), fixture.materialize.targetRoot, fixture.materialize.sourceRoot,
		fixture.materialize.sourcePath, fixture.materialize.finalPath, fixture.storeRoot, server.server.URL,
		clientAdoptUser, clientAdoptPassword, clientAdoptJobKey, clientAdoptRoot)

	if writeErr := os.WriteFile(fixture.materialize.sourcePath, fixture.materialize.content, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	requestsBefore = server.totalRequests()
	reader = &trackingReader{}
	out.Reset()
	errOut.Reset()
	if code := Run(retirementReconcileArgs, reader, &out, &errOut); code != 0 || errOut.Len() != 0 || reader.read {
		t.Fatalf("reappeared source code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
	if server.totalRequests() != requestsBefore {
		t.Fatal("reappeared retired source contacted the downloader")
	}
	response = struct {
		Data reconcile.Report `json:"data"`
	}{}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	retirement = response.Data.Ledgers.Retirement
	if response.Data.Outcome != "conflict" || retirement.Status != "source_name_reappeared" ||
		retirement.StopReason != "retirement_source_name_reappeared" || !retirement.ProcessLocalCompletionProof ||
		retirement.ProcessLocalAbsenceProof || retirement.CurrentAbsence != nil ||
		!hasReportFindingCLI(response.Data.Blockers, "retirement.source_name_reappeared") ||
		!slices.Contains(response.Data.Effect, "read_source_retirement_operation_state") ||
		!slices.Contains(response.Data.Effect, "read_retired_source_name_absence") ||
		slices.Contains(response.Data.Effect, "read_downloader_state") {
		t.Fatalf("reappeared retired name was not a current conflict: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), fixture.materialize.targetRoot, fixture.materialize.sourceRoot,
		fixture.materialize.sourcePath, fixture.materialize.finalPath, fixture.storeRoot, server.server.URL,
		clientAdoptUser, clientAdoptPassword, clientAdoptJobKey, clientAdoptRoot)

	if removeErr := os.Remove(fixture.materialize.sourcePath); removeErr != nil {
		t.Fatal(removeErr)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{
		"seed", "retire", "prune", "--target", fixture.materialize.targetRoot,
		"--expect-plan-id", retirementPlan.Data.Plan.ID, "--acknowledge-operation-state-deletion",
		"--output", "json", retired.Data.Operation.ID,
	}, strings.NewReader(""), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("retirement prune code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	requestsBefore = server.totalRequests()
	reader = &trackingReader{}
	out.Reset()
	errOut.Reset()
	if code := Run(retirementReconcileArgs, reader, &out, &errOut); code != 0 || errOut.Len() != 0 || reader.read {
		t.Fatalf("retained retirement code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
	if server.totalRequests() != requestsBefore {
		t.Fatal("retained retirement tombstone contacted the downloader")
	}
	response = struct {
		Data reconcile.Report `json:"data"`
	}{}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	retirement = response.Data.Ledgers.Retirement
	if response.Data.Outcome != "incomplete" || retirement.Status != "historical_completion_current_absence_unobserved" ||
		retirement.StopReason != "retirement_current_absence_unavailable" || !retirement.ProcessLocalCompletionProof ||
		retirement.ProcessLocalAbsenceProof || retirement.Completion == nil || !retirement.Completion.RetainedTombstone ||
		retirement.CurrentAbsence != nil || !hasReportFindingCLI(response.Data.Blockers, "retirement.current_absence_unavailable") ||
		!slices.Contains(response.Data.Effect, "read_source_retirement_operation_state") ||
		slices.Contains(response.Data.Effect, "read_retired_source_name_absence") || slices.Contains(response.Data.Effect, "read_downloader_state") {
		t.Fatalf("retained tombstone recreated current path authority: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), fixture.materialize.targetRoot, fixture.materialize.sourceRoot,
		fixture.materialize.sourcePath, fixture.materialize.finalPath, fixture.storeRoot, server.server.URL,
		clientAdoptUser, clientAdoptPassword, clientAdoptJobKey, clientAdoptRoot)
}

func TestReconcileReportBindsTerminalKeepDataRemovalToCurrentQueueAbsenceWithoutExtraRequests(t *testing.T) {
	fixture := prepareClientRemoveCLIFixture(t)
	base := clientRemoveBaseArgs(fixture)
	planned := runClientRemoveJSON(t, append([]string{"client", "remove", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	runArgs := append([]string{"client", "remove", "run"}, base...)
	runArgs = append(runArgs, "--expect-removal-plan-id", planned.Data.Plan.ID, "--acknowledge-client-removal")
	removed := runClientRemoveJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if removed.Data.Outcome != clientremove.OutcomeRemovedKeepData {
		t.Fatalf("removal did not complete: %#v", removed.Data)
	}

	reconcileArgs := append([]string{"reconcile", "report"}, base...)
	reconcileArgs = append(reconcileArgs, "--removal-operation", removed.Data.Operation.ID, "--removal-plan-id", planned.Data.Plan.ID)
	var out, errOut bytes.Buffer
	requestsBefore := fixture.server.totalRequests()
	if code := Run(reconcileArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("removal reconcile code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if delta := fixture.server.totalRequests() - requestsBefore; delta != 3 {
		t.Fatalf("removal reconciliation made %d requests; wanted one login plus the existing two-read bracket", delta)
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	removal := response.Data.Ledgers.Removal
	activation := response.Data.Ledgers.Activation
	if response.Data.Outcome != "consistent" || !response.Data.Scope.ClientRemovalRequested ||
		removal.Status != "historical_keep_data_removal_current_job_absent" || !removal.Historical ||
		!removal.ProcessLocalCompletionProof || removal.Completion == nil || removal.Completion.RetainedTombstone ||
		removal.Completion.OperationID != removed.Data.Operation.ID || removal.Completion.CompletionBasis != "accepted_response_then_exact_absence" ||
		activation.Status != "historical_completion_current_job_absent" || activation.CurrentAbsence == nil ||
		!activation.ProcessLocalCurrentAbsenceProof || activation.ProcessLocalCurrentUseProof || activation.CurrentUse != nil ||
		relationStatusCLI(response.Data, "client_infohash_relation") != "absent" ||
		relationStatusCLI(response.Data, "verified_source_vs_job_path") != "not_comparable" ||
		hasReportFindingCLI(response.Data.Blockers, "client.exact_job_absent") ||
		hasReportFindingCLI(response.Data.Blockers, "path.client_identity_unavailable") || len(response.Data.Relations) != 5 ||
		!strings.Contains(response.Data.Assurance, "canonical_historical_keep_data_removal") ||
		!slices.Contains(response.Data.Effect, "read_client_removal_operation_state") ||
		!slices.Contains(response.Data.Effect, "read_downloader_state") || slices.Contains(response.Data.Effect, "read_downloader_file_layout") {
		t.Fatalf("terminal removal was not reconciled as expected current absence: %s", out.String())
	}
	var human bytes.Buffer
	if err := writeReconciliationHuman(&human, response.Data); err != nil ||
		!strings.Contains(human.String(), "CLIENT REMOVAL (KEEP DATA)") ||
		!strings.Contains(human.String(), "PROCESS-LOCAL ABSENCE BRIDGE") ||
		strings.Index(human.String(), "CLIENT REMOVAL (KEEP DATA)") > strings.Index(human.String(), "LEDGERS") {
		t.Fatalf("removal table contract is unclear: err=%v\n%s", err, human.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), fixture.materialized.materialize.targetRoot, fixture.materialized.materialize.sourceRoot,
		fixture.materialized.materialize.sourcePath, fixture.materialized.materialize.finalPath, fixture.materialized.storeRoot,
		fixture.server.server.URL, clientAdoptUser, clientAdoptPassword, clientAdoptJobKey, clientAdoptRoot)

	pruneArgs := []string{"client", "remove", "prune", "--target", fixture.materialized.materialize.targetRoot,
		"--expect-removal-plan-id", planned.Data.Plan.ID, "--acknowledge-operation-state-deletion", "--output", "json", removed.Data.Operation.ID}
	out.Reset()
	errOut.Reset()
	if code := Run(pruneArgs, &trackingReader{}, &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("removal prune code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	requestsBefore = fixture.server.totalRequests()
	out.Reset()
	errOut.Reset()
	if code := Run(reconcileArgs, strings.NewReader(clientAdoptPassword+"\n"), &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("retained removal reconcile code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if delta := fixture.server.totalRequests() - requestsBefore; delta != 3 {
		t.Fatalf("retained removal reconciliation made %d requests", delta)
	}
	response = struct {
		Data reconcile.Report `json:"data"`
	}{}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "consistent" || response.Data.Ledgers.Removal.Completion == nil ||
		!response.Data.Ledgers.Removal.Completion.RetainedTombstone ||
		response.Data.Ledgers.Removal.Status != "historical_keep_data_removal_current_job_absent" {
		t.Fatalf("retained tombstone lost exact terminal removal authority: %s", out.String())
	}
}

func TestReconcileReportDoesNotConsumeCredentialsForHistoricallyUnattributedRemoval(t *testing.T) {
	fixture := prepareClientRemoveCLIFixture(t)
	base := clientRemoveBaseArgs(fixture)
	planned := runClientRemoveJSON(t, append([]string{"client", "remove", "plan"}, base...), strings.NewReader(clientAdoptPassword+"\n"), 0)
	fixture.server.setRemoveMode("unknown_job_removed")
	runArgs := append([]string{"client", "remove", "run"}, base...)
	runArgs = append(runArgs, "--expect-removal-plan-id", planned.Data.Plan.ID, "--acknowledge-client-removal")
	removed := runClientRemoveJSON(t, runArgs, strings.NewReader(clientAdoptPassword+"\n"), 0)
	if removed.Data.Outcome != clientremove.OutcomeRemovedUnattributed {
		t.Fatalf("removal was not historically unattributed: %#v", removed.Data)
	}

	reconcileArgs := append([]string{"reconcile", "report"}, base...)
	reconcileArgs = append(reconcileArgs, "--removal-operation", removed.Data.Operation.ID, "--removal-plan-id", planned.Data.Plan.ID)
	reader := &trackingReader{}
	requestsBefore := fixture.server.totalRequests()
	var out, errOut bytes.Buffer
	if code := Run(reconcileArgs, reader, &out, &errOut); code != 0 || errOut.Len() != 0 || reader.read {
		t.Fatalf("unattributed reconcile code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
	if fixture.server.totalRequests() != requestsBefore {
		t.Fatal("known-unattributed removal reconciliation contacted the downloader")
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "incomplete" || response.Data.Ledgers.Removal.Status != "historical_absence_causality_unproven" ||
		response.Data.Ledgers.Removal.StopReason != "removal_absence_causality_unproven" ||
		!response.Data.Ledgers.Removal.ProcessLocalCompletionProof || response.Data.Ledgers.Removal.Completion == nil ||
		response.Data.Ledgers.Removal.Completion.CompletionBasis != "exact_absence_after_unknown_attempt_causality_unproven" ||
		response.Data.Ledgers.Activation.ProcessLocalCurrentAbsenceProof ||
		!hasReportFindingCLI(response.Data.Blockers, "removal.absence_causality_unproven") ||
		slices.Contains(response.Data.Effect, "read_downloader_state") {
		t.Fatalf("unattributed removal was overstated or erased: %s", out.String())
	}
}

func TestReconcileReportExactSourceFailureIsStructuredAndPathPrivate(t *testing.T) {
	torrentPath, sourceRoot, _ := writeReconciliationFixture(t)
	missing := filepath.Join(sourceRoot, "PTCTL-EXACT-SOURCE-FAILURE-CANARY.bin")
	var out, errOut bytes.Buffer
	code := Run([]string{"reconcile", "report", "--torrent", torrentPath, "--source", missing, "--output", "json"}, strings.NewReader(""), &out, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "incomplete" || response.Data.Ledgers.Storage.Status != "incomplete" || relationStatusCLI(response.Data, "storage_content_proof") != "incomplete" || !hasReportFindingCLI(response.Data.Blockers, "source.exact_root_verification_failed") {
		t.Fatalf("exact source failure was not structured: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), torrentPath, sourceRoot, missing, "PTCTL-EXACT-SOURCE-FAILURE-CANARY")
}

func TestReconcileReportValidatesEverythingBeforePasswordRead(t *testing.T) {
	torrentPath, searchRoot, _ := writeReconciliationFixture(t)
	missingTorrent := filepath.Join(t.TempDir(), "missing.torrent")
	clientGroup := []string{"--driver", "qbittorrent", "--url", "https://seedbox.invalid", "--username", "alice", "--password-stdin"}
	validMaterializeOperation := "sha256:" + strings.Repeat("a", 64)
	validMaterializePlan := strings.Repeat("b", 24)
	validAdoptionPlan := strings.Repeat("a", 24)
	validAdoptionOperation := clientadopt.OperationIDForPlan(validAdoptionPlan).String()
	validActivationPlan := strings.Repeat("c", 24)
	validActivationOperation := clientactivate.OperationIDForPlan(validActivationPlan).String()
	validRemovalPlan := strings.Repeat("e", 24)
	validRemovalOperation := clientremove.OperationIDForPlan(validRemovalPlan).String()
	validRetirementPlan := "sha256:" + strings.Repeat("d", 64)
	validRetirementOperation, err := sourceretire.OperationIDForPlanID(validRetirementPlan)
	if err != nil {
		t.Fatal(err)
	}
	validParentCleanupPlan := "sha256:" + strings.Repeat("1", 64)
	validParentCleanupOperation, err := sourceretire.ParentCleanupOperationIDForPlanID(validParentCleanupPlan)
	if err != nil {
		t.Fatal(err)
	}
	otherParentCleanupPlan := "sha256:" + strings.Repeat("2", 64)
	otherParentCleanupOperation, err := sourceretire.ParentCleanupOperationIDForPlanID(otherParentCleanupPlan)
	if err != nil {
		t.Fatal(err)
	}
	tooManyRoots := []string{"reconcile", "report", "--torrent", torrentPath}
	for index := 0; index <= storage.DefaultInventoryLimits().MaxRoots; index++ {
		tooManyRoots = append(tooManyRoots, "--search-root", searchRoot)
	}
	tooManyRoots = append(tooManyRoots, clientGroup...)
	tooManyParentCleanupRoots := []string{
		"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot,
		"--parent-cleanup-operation", validParentCleanupOperation.String(), "--parent-cleanup-plan-id", validParentCleanupPlan,
	}
	for index := 0; index <= 64; index++ {
		tooManyParentCleanupRoots = append(tooManyParentCleanupRoots, "--parent-cleanup-search-root", searchRoot)
	}
	tooManyParentCleanupRoots = append(tooManyParentCleanupRoots, clientGroup...)
	tests := [][]string{
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--source", filepath.Join(searchRoot, "PTCTL-CLIENT-PATH-CANARY.bin"), "--search-root", searchRoot}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--source", filepath.Join(searchRoot, "PTCTL-CLIENT-PATH-CANARY.bin"), "--state-store", "unused", "--storage-profile", "unused"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--source", filepath.Join(searchRoot, "PTCTL-CLIENT-PATH-CANARY.bin"), "--allow-network"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--source="}, clientGroup...),
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
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", "not-an-operation", "--materialize-plan-id", validMaterializePlan}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", "NOT-A-PLAN"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--source", filepath.Join(searchRoot, "PTCTL-CLIENT-PATH-CANARY.bin"), "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--max-states", "1"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--adoption-operation", validAdoptionOperation}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--adoption-operation", "bad", "--adoption-plan-id", validAdoptionPlan, "--host-root", searchRoot, "--client-root", "/downloads"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--adoption-operation", validAdoptionOperation, "--adoption-plan-id", "BAD", "--host-root", searchRoot, "--client-root", "/downloads"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--adoption-operation", validAdoptionOperation, "--adoption-plan-id", validAdoptionPlan, "--host-root", searchRoot, "--client-root", "/downloads"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--adoption-operation", validAdoptionOperation, "--adoption-plan-id", validAdoptionPlan}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--adoption-operation", validAdoptionOperation, "--adoption-plan-id", validAdoptionPlan, "--host-root", searchRoot, "--client-root", "/downloads", "--client-file-layout", "off"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", "bad", "--activation-plan-id", validActivationPlan}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation, "--activation-plan-id", "BAD"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan, "--host-root", searchRoot, "--client-root", "/downloads", "--client-file-layout", "off"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan, "--host-root", searchRoot, "--client-root", "/downloads", "--max-client-files", "2"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan, "--removal-operation", validRemovalOperation}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan, "--removal-operation", "bad", "--removal-plan-id", validRemovalPlan, "--host-root", searchRoot, "--client-root", "/downloads"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan, "--removal-operation", validRemovalOperation, "--removal-plan-id", "BAD", "--host-root", searchRoot, "--client-root", "/downloads"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan, "--removal-operation", validRemovalOperation, "--removal-plan-id", validRemovalPlan, "--host-root", searchRoot, "--client-root", "/downloads", "--client-file-layout", "off"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--adoption-operation", validAdoptionOperation, "--adoption-plan-id", validAdoptionPlan, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan, "--removal-operation", validRemovalOperation, "--removal-plan-id", validRemovalPlan, "--host-root", searchRoot, "--client-root", "/downloads"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan, "--retirement-operation", validRetirementOperation.String()}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan, "--retirement-operation", "bad", "--retirement-plan-id", validRetirementPlan, "--retirement-search-root", searchRoot}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan, "--retirement-operation", validRetirementOperation.String(), "--retirement-plan-id", "BAD", "--retirement-search-root", searchRoot}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan, "--retirement-operation", validRetirementOperation.String(), "--retirement-plan-id", validRetirementPlan}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--retirement-allow-network"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--retirement-operation", validRetirementOperation.String(), "--retirement-plan-id", validRetirementPlan, "--retirement-search-root", searchRoot}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan, "--retirement-operation", validRetirementOperation.String(), "--retirement-plan-id", validRetirementPlan, "--retirement-search-root", searchRoot, "--host-root", searchRoot, "--client-root", "/downloads", "--client-file-layout", "off"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--target", searchRoot, "--materialize-operation", validMaterializeOperation, "--materialize-plan-id", validMaterializePlan, "--activation-operation", validActivationOperation, "--activation-plan-id", validActivationPlan, "--removal-operation", validRemovalOperation, "--removal-plan-id", validRemovalPlan, "--retirement-operation", validRetirementOperation.String(), "--retirement-plan-id", validRetirementPlan, "--retirement-search-root", searchRoot, "--host-root", searchRoot, "--client-root", "/downloads"}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--parent-cleanup-operation", validParentCleanupOperation.String()}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--parent-cleanup-operation", "bad", "--parent-cleanup-plan-id", validParentCleanupPlan, "--parent-cleanup-search-root", searchRoot}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--parent-cleanup-operation", validParentCleanupOperation.String(), "--parent-cleanup-plan-id", "BAD", "--parent-cleanup-search-root", searchRoot}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--parent-cleanup-operation", otherParentCleanupOperation.String(), "--parent-cleanup-plan-id", validParentCleanupPlan, "--parent-cleanup-search-root", searchRoot}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot, "--parent-cleanup-operation", validParentCleanupOperation.String(), "--parent-cleanup-plan-id", validParentCleanupPlan, "--parent-cleanup-search-root", searchRoot}, clientGroup...),
		append([]string{"reconcile", "report", "--torrent", missingTorrent, "--search-root", searchRoot}, clientGroup...),
		tooManyRoots,
		tooManyParentCleanupRoots,
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
	if code := Run([]string{"reconcile", "report", "--help"}, strings.NewReader(""), &helpOut, &helpErr); code != 0 || helpErr.Len() != 0 || !strings.Contains(helpOut.String(), "Client-only reads") || !strings.Contains(helpOut.String(), "--source PATH") || !strings.Contains(helpOut.String(), "not filesystem-wide uniqueness") || !strings.Contains(helpOut.String(), "--materialize-operation") || !strings.Contains(helpOut.String(), "--adoption-operation") || !strings.Contains(helpOut.String(), "terminal client-adoption journal or retained tombstone") || !strings.Contains(helpOut.String(), "observation-only existing-job adoption") || !strings.Contains(helpOut.String(), "job incarnation remains unobservable") || !strings.Contains(helpOut.String(), "--stop-operation") || !strings.Contains(helpOut.String(), "terminal stop journal or complete retention tombstone") || !strings.Contains(helpOut.String(), "unknown stop response remains causality-unproven") || !strings.Contains(helpOut.String(), "--removal-operation") || !strings.Contains(helpOut.String(), "terminal keep-data removal") || !strings.Contains(helpOut.String(), "--retirement-operation") || !strings.Contains(helpOut.String(), "--retirement-search-root") || !strings.Contains(helpOut.String(), "retirement-allow-network") || !strings.Contains(helpOut.String(), "twice reobserve its exact retired names absent") || !strings.Contains(helpOut.String(), "--parent-cleanup-operation") || !strings.Contains(helpOut.String(), "--parent-cleanup-search-root") || !strings.Contains(helpOut.String(), "parent-cleanup selectors") || !strings.Contains(helpOut.String(), "twice reobserve the exact removed parent names absent") || !strings.Contains(helpOut.String(), "sequential non-atomic observations") || !strings.Contains(helpOut.String(), "site-cookie-stdin") || !strings.Contains(helpOut.String(), "credential-bundle-stdin") || !strings.Contains(helpOut.String(), "current site claim") || !strings.Contains(helpOut.String(), "max-candidate-edges") || !strings.Contains(helpOut.String(), "client-file-layout") || !strings.Contains(helpOut.String(), "max-client-file-response-bytes") || !strings.Contains(helpOut.String(), "site-binding-record") || !strings.Contains(helpOut.String(), "at most two bounded file-list reads") || !strings.Contains(helpOut.String(), "never retried") || !strings.Contains(helpOut.String(), "require-reconciled") {
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
	liveSite := strings.Index(text, "LIVE SITE DETAIL")
	materialized := strings.Index(text, "MATERIALIZED FINAL")
	adoption := strings.Index(text, "CLIENT ADOPTION")
	activation := strings.Index(text, "CLIENT ACTIVATION")
	retirement := strings.Index(text, "SOURCE RETIREMENT")
	parentCleanup := strings.Index(text, "PARENT CLEANUP")
	ledgers := strings.Index(text, "LEDGERS")
	fileLayout := strings.Index(text, "CLIENT FILE LAYOUT (BOUNDED)")
	fileFindings := strings.Index(text, "CLIENT FILE FINDINGS (BOUNDED)")
	downloaderMatches := strings.Index(text, "DOWNLOADER MATCHES")
	scan := strings.Index(text, "STORAGE SCAN")
	matches := strings.Index(text, "VERIFIED STORAGE MATCHES")
	bindings := strings.Index(text, "VERIFIED STORAGE BINDINGS (BOUNDED)")
	if blockers < 0 || relations <= blockers || siteBinding <= relations || liveSite <= siteBinding || materialized <= liveSite || adoption <= materialized || activation <= adoption || retirement <= activation || parentCleanup <= retirement || ledgers <= parentCleanup || fileLayout <= ledgers || fileFindings <= fileLayout || downloaderMatches <= fileFindings || scan <= downloaderMatches || matches <= scan || bindings <= matches || !strings.Contains(text, "METAFILE VARIANT NOTE") || !strings.Contains(text, "PATH NOTE") || !strings.Contains(text, "lexical only") || !strings.Contains(text, "CONTENT PATH") || !strings.Contains(text, "BEFORE FILES CONSIDERED") {
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

func hasReportFindingCLI(findings []reconcile.ReportFinding, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
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

func writeExactEmptyReconciliationFixture(t *testing.T) (torrentPath, sourceRoot, hostRoot string, fileNames []string, meta *metafile.MetaInfo) {
	t.Helper()
	content := []byte("abc")
	torrentPath = filepath.Join(t.TempDir(), "reconcile-empty.torrent")
	if err := os.WriteFile(torrentPath, testV1MultiFileMetafileWithEmpty(content), 0o600); err != nil {
		t.Fatal(err)
	}
	hostRoot = t.TempDir()
	sourceRoot = filepath.Join(hostRoot, "bundle")
	if err := os.Mkdir(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	fileNames = []string{"data.bin", "empty.bin"}
	if err := os.WriteFile(filepath.Join(sourceRoot, fileNames[0]), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, fileNames[1]), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	meta, err = metafile.Read(torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	return torrentPath, sourceRoot, hostRoot, fileNames, meta
}

func testV1MultiFileMetafile(content []byte) []byte {
	piece0 := sha1.Sum(content[:4])
	piece1 := sha1.Sum(content[4:])
	result := []byte("d4:infod5:filesld6:lengthi3e4:pathl5:a.bineed6:lengthi3e4:pathl5:b.bineee4:name6:bundle12:piece lengthi4e6:pieces40:")
	result = append(result, piece0[:]...)
	result = append(result, piece1[:]...)
	return append(result, 'e', 'e')
}

func testV1MultiFileMetafileWithEmpty(content []byte) []byte {
	piece := sha1.Sum(content)
	result := []byte("d4:infod5:filesld6:lengthi3e4:pathl8:data.bineed6:lengthi0e4:pathl9:empty.bineee4:name6:bundle12:piece lengthi3e6:pieces20:")
	result = append(result, piece[:]...)
	return append(result, 'e', 'e')
}

func newMultiFileReconciliationServer(t *testing.T, meta *metafile.MetaInfo, fileNames, jobKeys []string, fileStatus int) (*httptest.Server, *reconciliationClientRequests) {
	t.Helper()
	requests := &reconciliationClientRequests{}
	magnet := "magnet:?xt=urn:btih:" + meta.InfoHashV1
	physicalBytes := int64(0)
	for _, file := range meta.Files {
		if !strings.Contains(file.Attribute, "p") {
			physicalBytes += file.Length
		}
	}
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
					"hash": jobKey, "magnet_uri": magnet, "name": meta.Name, "size": physicalBytes,
					"progress": 1.0, "state": "uploading", "save_path": reconciliationClientRoot,
					"content_path": reconciliationClientRoot + "/" + meta.Name, "downloaded": physicalBytes, "uploaded": int64(10),
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
			rows := make([]map[string]any, 0, len(fileNames))
			for index, fileName := range fileNames {
				rows = append(rows, map[string]any{
					"index": index, "name": meta.Name + "/" + fileName, "size": meta.Files[index].Length,
					"progress": 1.0, "priority": 1, "is_seed": true,
				})
			}
			_ = json.NewEncoder(w).Encode(rows)
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
