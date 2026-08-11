package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
)

func TestClientStatusTransmission(t *testing.T) {
	const password = "TRANSMISSION-STATUS-PASSWORD-CANARY"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("X-Transmission-Session-Id") == "" {
			w.Header().Set("X-Transmission-Session-Id", "fixture-token")
			w.Header().Set("X-Transmission-Rpc-Version", "6.0.0")
			w.WriteHeader(http.StatusConflict)
			return
		}
		var request struct {
			ID int64 `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": request.ID,
			"result": map[string]any{"version": "4.1.0", "rpc_version_semver": "6.0.0", "rpc_version": 18},
		})
	}))
	defer server.Close()

	var out, errOut bytes.Buffer
	code := Run([]string{
		"client", "status", "--driver", downloader.DriverTransmission, "--url", server.URL,
		"--username", "alice", "--password-stdin", "--output", "json",
	}, strings.NewReader(password+"\n"), &out, &errOut)
	if code != 0 || errOut.Len() != 0 || requests.Load() != 2 {
		t.Fatalf("code=%d requests=%d stdout=%q stderr=%q", code, requests.Load(), out.String(), errOut.String())
	}
	var response struct {
		Data downloader.Status `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	status := response.Data
	if status.Driver != downloader.DriverTransmission || status.Version != "4.1.0" || status.WebAPIVersion != "6.0.0" {
		t.Fatalf("unexpected status: %#v", status)
	}
	assertJSONStringsExclude(t, out.Bytes(), password, server.URL+"/transmission/rpc")
}

func TestReconcileTransmissionReadOnlyLedger(t *testing.T) {
	torrentPath, searchRoot, meta := writeReconciliationFixture(t)
	const (
		username = "TRANSMISSION-USER-CANARY"
		password = "TRANSMISSION-PASSWORD-CANARY"
	)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/transmission/rpc" {
			http.NotFound(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != username || pass != password {
			t.Errorf("unexpected Transmission credentials")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Transmission-Session-Id") == "" {
			w.Header().Set("X-Transmission-Session-Id", "fixture-token")
			w.Header().Set("X-Transmission-Rpc-Version", "6.0.0")
			w.WriteHeader(http.StatusConflict)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var request struct {
			JSONRPC string `json:"jsonrpc"`
			Method  string `json:"method"`
			ID      int64  `json:"id"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var result map[string]any
		switch request.Method {
		case "session_get":
			result = map[string]any{"version": "4.1.0", "rpc_version_semver": "6.0.0", "rpc_version": 18}
		case "torrent_get":
			result = map[string]any{"torrents": []any{map[string]any{
				"hash_string": meta.InfoHashV1, "name": "PTCTL-CLIENT-PATH-CANARY.bin", "total_size": int64(len("content")),
				"percent_complete": 1.0, "status": 6, "download_dir": "/downloads", "downloaded_ever": int64(len("content")), "uploaded_ever": int64(10),
			}}}
		default:
			t.Errorf("unexpected method %q", request.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "result": result, "id": request.ID})
	}))
	defer server.Close()

	var out, errOut bytes.Buffer
	code := Run([]string{
		"reconcile", "report", "--torrent", torrentPath, "--search-root", searchRoot,
		"--driver", downloader.DriverTransmission, "--url", server.URL, "--username", username, "--password-stdin",
		"--host-root", searchRoot, "--client-root", "/downloads", "--client-style", "posix",
		"--timeout", "1m", "--output", "json",
	}, strings.NewReader(password+"\n"), &out, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if requests.Load() != 4 {
		t.Fatalf("wire requests=%d, want one 409 handshake, one version read, and two bracket reads", requests.Load())
	}
	var response struct {
		Data reconcile.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Outcome != "consistent" || response.Data.Ledgers.Downloader.Driver != downloader.DriverTransmission || response.Data.Ledgers.Downloader.RequestsMade != 4 ||
		relationStatusForCLI(response.Data, "client_infohash_relation") != "exact_unique" || relationStatusForCLI(response.Data, "verified_source_vs_job_path") != "same_location" {
		t.Fatalf("unexpected Transmission reconciliation: %s", out.String())
	}
	assertJSONStringsExclude(t, out.Bytes(), torrentPath, searchRoot, server.URL, username, password)
}

func relationStatusForCLI(report reconcile.Report, kind string) string {
	for _, relation := range report.Relations {
		if relation.Kind == kind {
			return relation.Status
		}
	}
	return ""
}
