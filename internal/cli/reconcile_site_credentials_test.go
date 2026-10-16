package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const reconcileDownloaderPasswordCanary = "RECONCILE-DOWNLOADER-PASSWORD-CANARY"

// fakeReconcileQBittorrent serves one finished torrent for the fixture of
// writeReconciliationFixture and counts logins.
func fakeReconcileQBittorrent(t *testing.T, infoHash string, logins *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			logins.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("username") != "user" || r.Form.Get("password") != reconcileDownloaderPasswordCanary {
				t.Errorf("unexpected downloader credential")
			}
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "ok", Path: "/"})
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"hash": "opaque", "magnet_uri": "magnet:?xt=urn:btih:" + infoHash,
				"name": "PTCTL-CLIENT-PATH-CANARY.bin", "size": int64(len("content")), "progress": 1.0,
				"state": "uploading", "save_path": "/downloads", "content_path": "/downloads/PTCTL-CLIENT-PATH-CANARY.bin",
				"downloaded": int64(len("content")), "uploaded": int64(10),
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func decodeReconcileReport(t *testing.T, encoded []byte) reconcile.Report {
	t.Helper()
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatalf("decode report: %v stdout=%s", err, encoded)
	}
	return response.Data
}

func TestReconcileObservesAPIKeySiteFromStdin(t *testing.T) {
	torrentPath, searchRoot, _ := writeReconciliationFixture(t)
	adapter := successfulFakeTorrentDetail(t)
	adapter.authMethod = domain.AuthMethodAPIKeyHeader
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader(apiKeyCanary + "\n"), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.reconcileReport([]string{
		"--torrent", torrentPath, "--search-root", searchRoot, "--site-ref", "fakept/42", "--site-api-key-stdin", "--output", "json",
	}); err != nil {
		t.Fatalf("API-key site-only reconciliation: %v stdout=%s", err, out.String())
	}
	report := decodeReconcileReport(t, out.Bytes())
	if report.Ledgers.Site.Detail.Status != "observed_current_ref" || report.Ledgers.Downloader.Status != "not_requested" ||
		adapter.opened != 1 || adapter.credential != apiKeyCanary {
		t.Fatalf("API key did not reach the site detail read: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), apiKeyCanary, torrentPath, searchRoot)
}

func TestReconcileSiteLiveUsesSavedCredentialWithoutReadingStdin(t *testing.T) {
	useTempConfigDir(t)
	torrentPath, searchRoot, _ := writeReconciliationFixture(t)
	for _, test := range []struct {
		name    string
		method  domain.AuthMethod
		content string
		want    string
	}{
		{name: "cookie", method: domain.AuthMethodCookieHeader, content: savedCookieCanary + "\n", want: savedCookieCanary},
		{name: "api key", method: domain.AuthMethodAPIKeyHeader, content: savedAPIKeyCanary + "\n", want: savedAPIKeyCanary},
	} {
		t.Run(test.name, func(t *testing.T) {
			writeSavedCredential(t, "fakept", test.method, test.content)
			adapter := successfulFakeTorrentDetail(t)
			adapter.authMethod = test.method
			reader := &trackingReader{}
			var out bytes.Buffer
			a := &app{stdin: reader, stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
			if err := a.reconcileReport([]string{
				"--torrent", torrentPath, "--search-root", searchRoot, "--site-ref", "fakept/42", "--site-live", "--output", "json",
			}); err != nil {
				t.Fatalf("saved-credential reconciliation: %v stdout=%s", err, out.String())
			}
			report := decodeReconcileReport(t, out.Bytes())
			if reader.read || report.Ledgers.Site.Detail.Status != "observed_current_ref" || adapter.opened != 1 || adapter.credential != test.want {
				t.Fatalf("saved credential was not used alone: read=%t stdout=%s", reader.read, out.String())
			}
			assertJSONStringsExclude(t, out.Bytes(), test.want, torrentPath, searchRoot)
		})
	}
}

func TestReconcileSiteLiveLeavesStdinToTheDownloaderPassword(t *testing.T) {
	useTempConfigDir(t)
	writeSavedCredential(t, "fakept", domain.AuthMethodAPIKeyHeader, savedAPIKeyCanary)
	torrentPath, searchRoot, meta := writeReconciliationFixture(t)
	var logins atomic.Int32
	server := fakeReconcileQBittorrent(t, meta.InfoHashV1, &logins)
	adapter := successfulFakeTorrentDetail(t)
	adapter.authMethod = domain.AuthMethodAPIKeyHeader
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader(reconcileDownloaderPasswordCanary + "\n"), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.reconcileReport([]string{
		"--torrent", torrentPath, "--search-root", searchRoot, "--site-ref", "fakept/42", "--site-live",
		"--driver", "qbittorrent", "--url", server.URL, "--username", "user", "--password-stdin",
		"--host-root", searchRoot, "--client-root", "/downloads", "--client-style", "posix", "--output", "json",
	}); err != nil {
		t.Fatalf("saved site credential with downloader: %v stdout=%s", err, out.String())
	}
	report := decodeReconcileReport(t, out.Bytes())
	if report.Outcome != "consistent" || report.Ledgers.Site.Detail.Status != "observed_current_ref" ||
		adapter.credential != savedAPIKeyCanary || logins.Load() != 1 {
		t.Fatalf("saved site credential and downloader password were not kept apart: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), savedAPIKeyCanary, reconcileDownloaderPasswordCanary, server.URL, torrentPath, searchRoot)
}

func TestReconcileCredentialBundleCarriesSiteAPIKey(t *testing.T) {
	torrentPath, searchRoot, meta := writeReconciliationFixture(t)
	var logins atomic.Int32
	server := fakeReconcileQBittorrent(t, meta.InfoHashV1, &logins)
	args := []string{
		"--torrent", torrentPath, "--search-root", searchRoot, "--site-ref", "fakept/42", "--credential-bundle-stdin",
		"--driver", "qbittorrent", "--url", server.URL, "--username", "user",
		"--host-root", searchRoot, "--client-root", "/downloads", "--client-style", "posix", "--output", "json",
	}
	bundle := func(t *testing.T, fields map[string]string) []byte {
		t.Helper()
		values := map[string]string{"schema": reconciliationCredentialSchema, "downloader_password": reconcileDownloaderPasswordCanary}
		for key, value := range fields {
			values[key] = value
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}

	adapter := successfulFakeTorrentDetail(t)
	adapter.authMethod = domain.AuthMethodAPIKeyHeader
	var out bytes.Buffer
	a := &app{stdin: bytes.NewReader(bundle(t, map[string]string{"site_api_key": apiKeyCanary})), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.reconcileReport(args); err != nil {
		t.Fatalf("API-key bundle: %v stdout=%s", err, out.String())
	}
	if report := decodeReconcileReport(t, out.Bytes()); report.Outcome != "consistent" || adapter.credential != apiKeyCanary || logins.Load() != 1 {
		t.Fatalf("bundled API key was not used: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), apiKeyCanary, reconcileDownloaderPasswordCanary)

	for _, test := range []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{name: "both site credentials", fields: map[string]string{"site_cookie": "sid=BUNDLE-COOKIE-CANARY", "site_api_key": apiKeyCanary}, want: "reconciliation credential bundle is invalid"},
		{name: "no site credential", fields: nil, want: "reconciliation credential bundle is invalid"},
		{name: "invalid API key", fields: map[string]string{"site_api_key": "bad key"}, want: "reconciliation site credential is invalid"},
		{name: "cookie for an API-key site", fields: map[string]string{"site_cookie": "sid=BUNDLE-COOKIE-CANARY"}, want: `site "fakept" does not support cookie_header authentication`},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter := successfulFakeTorrentDetail(t)
			adapter.authMethod = domain.AuthMethodAPIKeyHeader
			before := logins.Load()
			var out bytes.Buffer
			a := &app{stdin: bytes.NewReader(bundle(t, test.fields)), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
			err := a.reconcileReport(args)
			if err == nil || err.Error() != test.want || out.Len() != 0 || adapter.opened != 0 || logins.Load() != before {
				t.Fatalf("bundle was not refused before any request: err=%v opened=%d stdout=%q", err, adapter.opened, out.String())
			}
			for _, secret := range []string{apiKeyCanary, "BUNDLE-COOKIE-CANARY", reconcileDownloaderPasswordCanary} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error disclosed a secret: %v", err)
				}
			}
		})
	}
}

func TestReconcileSiteCredentialModesRejectBeforeStdin(t *testing.T) {
	torrentPath, searchRoot, _ := writeReconciliationFixture(t)
	base := []string{"--torrent", torrentPath, "--search-root", searchRoot, "--site-ref", "fakept/42", "--output", "json"}
	client := []string{"--driver", "qbittorrent", "--url", "http://127.0.0.1:1", "--username", "user"}
	with := func(parts ...[]string) []string {
		args := append([]string{}, base...)
		for _, part := range parts {
			args = append(args, part...)
		}
		return args
	}
	for _, test := range []struct {
		name   string
		method domain.AuthMethod
		saved  bool
		args   []string
		want   string
	}{
		{name: "two site sources", args: with([]string{"--site-cookie-stdin", "--site-api-key-stdin"}),
			want: "--site-cookie-stdin, --site-api-key-stdin, --site-live, and --credential-bundle-stdin are mutually exclusive"},
		{name: "site live and bundle", saved: true, args: with([]string{"--site-live", "--credential-bundle-stdin"}, client),
			want: "--site-cookie-stdin, --site-api-key-stdin, --site-live, and --credential-bundle-stdin are mutually exclusive"},
		{name: "site live disabled", args: with([]string{"--site-live=false"}), want: "--site-live cannot be explicitly disabled"},
		{name: "API key on a cookie site", args: with([]string{"--site-api-key-stdin"}), want: `site "fakept" does not support api_key_header authentication`},
		{name: "cookie on an API-key site", method: domain.AuthMethodAPIKeyHeader, args: with([]string{"--site-cookie-stdin"}),
			want: `site "fakept" does not support authenticated torrent detail reads`},
		{name: "site live without a saved credential", args: with([]string{"--site-live"}),
			want: `no saved session cookie for site "fakept"; run 'ptctl site login fakept' first, or pipe one with --cookie-stdin`},
		{name: "site live with a client but no password flag", saved: true, args: with([]string{"--site-live"}, client),
			want: "combined --site-live and downloader reads require --password-stdin"},
		{name: "stdin site credential with a client", args: with([]string{"--site-api-key-stdin", "--password-stdin"}, client),
			want: "combined live site and downloader reads require --credential-bundle-stdin, or --site-live with --password-stdin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			useTempConfigDir(t)
			adapter := successfulFakeTorrentDetail(t)
			adapter.authMethod = test.method
			if test.saved {
				writeSavedCredential(t, "fakept", adapter.method(), savedCookieCanary)
			}
			reader := &trackingReader{}
			var out bytes.Buffer
			a := &app{stdin: reader, stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
			err := a.reconcileReport(test.args)
			var usage *usageErr
			if !errors.As(err, &usage) || err.Error() != test.want || reader.read || out.Len() != 0 || adapter.opened != 0 {
				t.Fatalf("err=%v read=%t opened=%d stdout=%q", err, reader.read, adapter.opened, out.String())
			}
		})
	}
}

func TestReconcileSiteLiveRefusesAnUnsafeSavedCredential(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	useTempConfigDir(t)
	path := writeSavedCredential(t, "fakept", domain.AuthMethodCookieHeader, savedCookieCanary)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	torrentPath, searchRoot, _ := writeReconciliationFixture(t)
	adapter := successfulFakeTorrentDetail(t)
	var out bytes.Buffer
	a := &app{stdin: &trackingReader{}, stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	err := a.reconcileReport([]string{
		"--torrent", torrentPath, "--search-root", searchRoot, "--site-ref", "fakept/42", "--site-live", "--output", "json",
	})
	if err == nil || !strings.Contains(err.Error(), "because other users can access it") || strings.Contains(err.Error(), savedCookieCanary) ||
		out.Len() != 0 || adapter.opened != 0 {
		t.Fatalf("unsafe saved credential was not refused: err=%v opened=%d stdout=%q", err, adapter.opened, out.String())
	}
}
