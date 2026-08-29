package transmission

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

const testHash = "0123456789abcdef0123456789abcdef01234567"

func TestReadSessionSupportsLegacyAndJSONRPC(t *testing.T) {
	for _, test := range []struct {
		name       string
		newRPC     bool
		rpcVersion string
	}{
		{name: "legacy-4.0", rpcVersion: "5.3.0"},
		{name: "json-rpc-4.1", newRPC: true, rpcVersion: "6.0.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newRPCFixture(t, test.newRPC, test.rpcVersion)
			defer server.Close()
			adapter, err := New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			credential, err := downloader.NewCredential("alice", "secret")
			if err != nil {
				t.Fatal(err)
			}
			session, err := adapter.OpenReadSession(context.Background(), credential)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if session.RequestsMade() != 2 {
				t.Fatalf("open request count = %d, want 2", session.RequestsMade())
			}
			ledger, err := session.ReadLedger(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if session.RequestsMade() != 3 || ledger.Driver != downloader.DriverTransmission || !ledger.Complete || len(ledger.Jobs) != 1 {
				t.Fatalf("unexpected ledger: requests=%d value=%#v", session.RequestsMade(), ledger)
			}
			job := ledger.Jobs[0]
			if job.Hash != testHash || job.InfoHashV1 != testHash || job.InfoHashV2 != "" || job.IdentityStatus != downloader.IdentityStatusValid ||
				len(job.IdentityEvidence) != 1 || job.IdentityEvidence[0] != "transmission_hash_string_sha1" || job.ContentPath != "/downloads/bundle" || job.State != "uploading" {
				t.Fatalf("unexpected normalized job: %#v", job)
			}
			limits := downloader.DefaultJobFileLedgerLimits()
			files, err := session.ReadJobFiles(context.Background(), testHash, limits)
			if err != nil {
				t.Fatal(err)
			}
			if session.RequestsMade() != 4 || !files.Complete || len(files.Files) != 2 || files.Used.FilesConsidered != 2 || files.Used.ResponseBytes <= 0 {
				t.Fatalf("unexpected file ledger: requests=%d value=%#v", session.RequestsMade(), files)
			}
			if files.SavePath != "/downloads" || files.ContentPath != "/downloads/bundle" {
				t.Fatalf("file snapshot did not bind its top-level path claims: %#v", files)
			}
			if got := strings.Join(files.Files[0].RelativeComponents, "/"); got != "bundle/a.bin" || files.Files[0].Selection != downloader.JobFileSelectionSelected || !files.Files[0].Complete {
				t.Fatalf("unexpected first file: %#v", files.Files[0])
			}
		})
	}
}

func TestExpiredCSRFTokenIsNotRetried(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		current := requests
		mu.Unlock()
		if current == 1 {
			w.Header().Set("X-Transmission-Session-Id", "token-one")
			w.Header().Set("X-Transmission-Rpc-Version", "6.0.0")
			w.WriteHeader(http.StatusConflict)
			return
		}
		if current == 2 {
			writeRPCResponse(t, w, r, true, map[string]any{"version": "4.1.0", "rpc_version_semver": "6.0.0", "rpc_version": 18})
			return
		}
		w.Header().Set("X-Transmission-Session-Id", "token-two")
		w.WriteHeader(http.StatusConflict)
	}))
	defer server.Close()
	session := openTestSession(t, server.URL)
	defer session.Close()
	if _, err := session.ReadLedger(context.Background()); err == nil || session.RequestsMade() != 3 {
		t.Fatalf("expired token was not a single fail-closed request: requests=%d err=%v", session.RequestsMade(), err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 3 {
		t.Fatalf("wire requests = %d, want 3", requests)
	}
}

func TestUnknownRPCVersionStopsAfterHandshakeResponse(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("X-Transmission-Session-Id", "token")
		w.Header().Set("X-Transmission-Rpc-Version", "7.0.0")
		w.WriteHeader(http.StatusConflict)
	}))
	defer server.Close()
	adapter, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := downloader.NewCredential("alice", "secret")
	session, err := adapter.OpenReadSession(context.Background(), credential)
	if err == nil || session != nil || requests != 1 {
		t.Fatalf("unknown protocol did not stop after one request: session=%v requests=%d err=%v", session, requests, err)
	}
	if count, ok := downloader.RequestsMadeFromError(err); !ok || count != 1 {
		t.Fatalf("open error count = %d,%t", count, ok)
	}
}

func TestTransportErrorsDoNotLeakCredentialsOrJobKey(t *testing.T) {
	server := newRPCFixture(t, true, "6.0.0")
	session := openTestSession(t, server.URL)
	server.Close()
	_, err := session.ReadJobFiles(context.Background(), testHash, downloader.DefaultJobFileLedgerLimits())
	_ = session.Close()
	if err == nil {
		t.Fatal("closed server unexpectedly succeeded")
	}
	message := err.Error()
	for _, secret := range []string{"alice", "secret", testHash, server.URL, "/transmission/rpc"} {
		if strings.Contains(message, secret) {
			t.Fatalf("transport error leaked %q: %q", secret, message)
		}
	}
}

func TestOpenTransportFailureIsCountedAndRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL
	server.Close()
	adapter, err := New(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := downloader.NewCredential("OPEN-USER-CANARY", "OPEN-PASSWORD-CANARY")
	session, err := adapter.OpenReadSession(context.Background(), credential)
	if err == nil || session != nil {
		t.Fatalf("closed endpoint unexpectedly opened a session: session=%v err=%v", session, err)
	}
	if count, ok := downloader.RequestsMadeFromError(err); !ok || count != 1 {
		t.Fatalf("open failure count = %d,%t, want 1,true", count, ok)
	}
	for _, secret := range []string{endpoint, "OPEN-USER-CANARY", "OPEN-PASSWORD-CANARY", defaultRPCPath} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("open error leaked %q: %q", secret, err)
		}
	}
}

func TestInvalidUsernameStopsBeforeRequest(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	adapter, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := downloader.NewCredential("bad:user", "secret")
	session, err := adapter.OpenReadSession(context.Background(), credential)
	if err == nil || session != nil || requests != 0 {
		t.Fatalf("invalid username crossed the network boundary: session=%v requests=%d err=%v", session, requests, err)
	}
	if count, ok := downloader.RequestsMadeFromError(err); !ok || count != 0 {
		t.Fatalf("invalid username count = %d,%t, want 0,true", count, ok)
	}
}

func TestStrictResponseAndWantedTypes(t *testing.T) {
	legacy := json.RawMessage(`{"torrents":[{"hashString":"` + testHash + `","name":"bundle","downloadDir":"/downloads","files":[{"bytesCompleted":1,"length":1,"name":"bundle/a"}],"fileStats":[{"bytesCompleted":1,"wanted":true,"priority":0}]}]}`)
	if _, _, _, _, err := decodeFileResult(context.Background(), legacy, protocolLegacy, testHash, downloader.DefaultJobFileLedgerLimits()); err == nil {
		t.Fatal("legacy boolean wanted was accepted")
	}
	modern := json.RawMessage(`{"torrents":[{"hash_string":"` + testHash + `","name":"bundle","download_dir":"/downloads","files":[{"bytes_completed":1,"length":1,"name":"bundle/a"}],"file_stats":[{"bytes_completed":1,"wanted":1,"priority":0}]}]}`)
	if _, _, _, _, err := decodeFileResult(context.Background(), modern, protocolJSONRPC, testHash, downloader.DefaultJobFileLedgerLimits()); err == nil {
		t.Fatal("JSON-RPC numeric wanted was accepted")
	}
	duplicate := []byte(`{"jsonrpc":"2.0","id":1,"id":1,"result":{}}`)
	if _, err := decodeRPCResult(duplicate, protocolJSONRPC, 1); err == nil {
		t.Fatal("duplicate response field was accepted")
	}
	surrogate := []byte(`{"jsonrpc":"2.0","id":1,"result":{"x":"\ud800"}}`)
	if _, err := decodeRPCResult(surrogate, protocolJSONRPC, 1); err == nil {
		t.Fatal("unpaired JSON surrogate was accepted")
	}
	consumed := 0
	_, err := decodeRawArray([]byte(`[{},"CANARY-N-PLUS-ONE-SECRET\ud800"]`), 1, func(_ int, _ json.RawMessage) error {
		consumed++
		return nil
	})
	if err == nil || consumed != 1 || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("N+1 was decoded or leaked: consumed=%d err=%v", consumed, err)
	}
}

func TestEndpointValidation(t *testing.T) {
	for _, endpoint := range []string{
		"http://example.com/transmission/rpc",
		"https://user:pass@example.com/transmission/rpc",
		"https://example.com/transmission/rpc?token=secret",
		"https://example.com/transmission/rpc/",
	} {
		if _, err := New(endpoint); err == nil {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
}

func openTestSession(t *testing.T, endpoint string) downloader.LedgerSession {
	t.Helper()
	adapter, err := New(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := downloader.NewCredential("alice", "secret")
	session, err := adapter.OpenReadSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func newRPCFixture(t *testing.T, modern bool, rpcVersion string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != defaultRPCPath {
			t.Errorf("request path = %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != "alice" || password != "secret" {
			t.Errorf("missing basic authentication")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Transmission-Session-Id") == "" {
			w.Header().Set("X-Transmission-Session-Id", "fixture-token")
			if modern {
				w.Header().Set("X-Transmission-Rpc-Version", rpcVersion)
			}
			w.WriteHeader(http.StatusConflict)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var request map[string]json.RawMessage
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var method string
		if err := json.Unmarshal(request["method"], &method); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sessionMethod, torrentMethod := "session-get", "torrent-get"
		if modern {
			sessionMethod, torrentMethod = "session_get", "torrent_get"
		}
		switch method {
		case sessionMethod:
			if modern {
				writeRPCResponse(t, w, r, true, map[string]any{"version": "4.1.0", "rpc_version_semver": rpcVersion, "rpc_version": 18})
			} else {
				writeRPCResponse(t, w, r, false, map[string]any{"version": "4.0.6", "rpc-version-semver": rpcVersion, "rpc-version": 17})
			}
		case torrentMethod:
			argumentsKey := "arguments"
			if modern {
				argumentsKey = "params"
			}
			var arguments struct {
				Fields []string `json:"fields"`
			}
			if err := json.Unmarshal(request[argumentsKey], &arguments); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fileRead := false
			for _, field := range arguments.Fields {
				if field == "files" {
					fileRead = true
				}
			}
			if fileRead {
				writeRPCResponse(t, w, r, modern, map[string]any{"torrents": []any{fileFixture(modern)}})
			} else {
				writeRPCResponse(t, w, r, modern, map[string]any{"torrents": []any{ledgerFixture(modern)}})
			}
		default:
			t.Errorf("unexpected method %q", method)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
}

func ledgerFixture(modern bool) map[string]any {
	if modern {
		return map[string]any{"hash_string": testHash, "name": "bundle", "total_size": 6, "percent_complete": 1, "status": 6, "download_dir": "/downloads", "downloaded_ever": 6, "uploaded_ever": 7}
	}
	return map[string]any{"hashString": testHash, "name": "bundle", "totalSize": 6, "percentComplete": 1, "status": 6, "downloadDir": "/downloads", "downloadedEver": 6, "uploadedEver": 7}
}

func fileFixture(modern bool) map[string]any {
	if modern {
		return map[string]any{
			"hash_string": testHash, "name": "bundle", "download_dir": "/downloads",
			"files": []any{
				map[string]any{"bytes_completed": 3, "length": 3, "name": "bundle/a.bin"},
				map[string]any{"bytes_completed": 3, "length": 3, "name": "bundle/b.bin"},
			},
			"file_stats": []any{
				map[string]any{"bytes_completed": 3, "wanted": true, "priority": 0},
				map[string]any{"bytes_completed": 3, "wanted": true, "priority": 0},
			},
		}
	}
	return map[string]any{
		"hashString": testHash, "name": "bundle", "downloadDir": "/downloads",
		"files": []any{
			map[string]any{"bytesCompleted": 3, "length": 3, "name": "bundle/a.bin"},
			map[string]any{"bytesCompleted": 3, "length": 3, "name": "bundle/b.bin"},
		},
		"fileStats": []any{
			map[string]any{"bytesCompleted": 3, "wanted": 1, "priority": 0},
			map[string]any{"bytesCompleted": 3, "wanted": 1, "priority": 0},
		},
	}
}

func writeRPCResponse(t *testing.T, w http.ResponseWriter, r *http.Request, modern bool, result map[string]any) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		t.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var id int64
	idKey := "tag"
	if modern {
		idKey = "id"
	}
	if err := json.Unmarshal(request[idKey], &id); err != nil {
		t.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	var response any
	if modern {
		response = map[string]any{"jsonrpc": "2.0", "result": result, "id": id}
	} else {
		response = map[string]any{"arguments": result, "result": "success", "tag": id}
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		t.Error(fmt.Errorf("encode fixture response: %w", err))
	}
}
