package transmission

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

type mutationPayload struct {
	variant string
	size    int64
	bytes   []byte
}

func (payload mutationPayload) VariantID() string        { return payload.variant }
func (payload mutationPayload) SizeBytes() int64         { return payload.size }
func (payload mutationPayload) Open() (io.Reader, error) { return bytes.NewReader(payload.bytes), nil }

func TestAddStoppedStreamsExactMetafileForBothProtocols(t *testing.T) {
	for _, modern := range []bool{false, true} {
		name := "legacy"
		if modern {
			name = "json-rpc"
		}
		t.Run(name, func(t *testing.T) {
			var captured map[string]json.RawMessage
			server, requestCount := newMutationFixture(t, modern, "added", func(request map[string]json.RawMessage) {
				captured = request
			})
			defer server.Close()
			adapter, err := New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			credential, _ := downloader.NewCredential("alice", "secret")
			session, err := adapter.OpenMutationSession(context.Background(), credential)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			raw := []byte("d4:infod4:name6:bundleee")
			receipt, err := session.AddStopped(context.Background(), downloader.AddStoppedRequest{Metafile: mutationPayload{
				variant: "sha256:" + strings.Repeat("a", 64), size: int64(len(raw)), bytes: raw,
			}, SavePath: "/downloads", Identity: downloader.TypedIdentity{InfoHashV1: testHash}})
			if err != nil || !receipt.Complete || !receipt.BytesSubmittedKnown || receipt.BytesSubmitted != int64(len(raw)) ||
				receipt.RequestsAttempted != 1 || receipt.AutomaticRetries != 0 || receipt.RedirectsFollowed != 0 {
				t.Fatalf("receipt=%#v err=%v", receipt, err)
			}
			if session.RequestsMade() != 3 || requestCount() != 3 {
				t.Fatalf("request counts = session %d wire %d, want 3", session.RequestsMade(), requestCount())
			}
			argumentsKey := "arguments"
			if modern {
				argumentsKey = "params"
			}
			var arguments map[string]json.RawMessage
			if err := json.Unmarshal(captured[argumentsKey], &arguments); err != nil {
				t.Fatal(err)
			}
			pathKey := "download-dir"
			if modern {
				pathKey = "download_dir"
			}
			var path, encoded string
			var paused bool
			if json.Unmarshal(arguments[pathKey], &path) != nil || json.Unmarshal(arguments["metainfo"], &encoded) != nil ||
				json.Unmarshal(arguments["paused"], &paused) != nil || path != "/downloads" || !paused {
				t.Fatalf("unexpected add arguments: %#v", arguments)
			}
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil || !bytes.Equal(decoded, raw) {
				t.Fatalf("submitted metafile differs: %q err=%v", decoded, err)
			}
			if _, err := session.AddStopped(context.Background(), downloader.AddStoppedRequest{Metafile: mutationPayload{
				variant: "sha256:" + strings.Repeat("a", 64), size: int64(len(raw)), bytes: raw,
			}, SavePath: "/downloads", Identity: downloader.TypedIdentity{InfoHashV1: testHash}}); err == nil || requestCount() != 3 {
				t.Fatalf("second add was not stopped before the network: requests=%d err=%v", requestCount(), err)
			}
		})
	}
}

func TestAddStoppedDuplicateAndExpiredCSRFAreNotAcceptedOrRetried(t *testing.T) {
	for _, outcome := range []string{"duplicate", "csrf"} {
		t.Run(outcome, func(t *testing.T) {
			server, requestCount := newMutationFixture(t, true, outcome, nil)
			defer server.Close()
			adapter, _ := New(server.URL)
			credential, _ := downloader.NewCredential("alice", "secret")
			session, err := adapter.OpenMutationSession(context.Background(), credential)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			raw := []byte("d4:infod4:name6:bundleee")
			receipt, err := session.AddStopped(context.Background(), downloader.AddStoppedRequest{Metafile: mutationPayload{
				variant: "sha256:" + strings.Repeat("b", 64), size: int64(len(raw)), bytes: raw,
			}, SavePath: "/downloads", Identity: downloader.TypedIdentity{InfoHashV1: testHash}})
			if err == nil || receipt.Complete || receipt.RequestsAttempted != 1 || requestCount() != 3 || session.RequestsMade() != 3 {
				t.Fatalf("receipt=%#v requests=%d err=%v", receipt, requestCount(), err)
			}
			wanted := "already_exists"
			if outcome == "csrf" {
				wanted = "csrf_expired"
			}
			if receipt.StopReason != wanted {
				t.Fatalf("stop reason = %q, want %q", receipt.StopReason, wanted)
			}
		})
	}
}

func TestAddStoppedRejectsAcceptedResponseForAnotherIdentity(t *testing.T) {
	server, requestCount := newMutationFixture(t, true, "wrong_hash", nil)
	defer server.Close()
	adapter, _ := New(server.URL)
	credential, _ := downloader.NewCredential("alice", "secret")
	session, err := adapter.OpenMutationSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	raw := []byte("d4:infod4:name6:bundleee")
	receipt, err := session.AddStopped(context.Background(), downloader.AddStoppedRequest{Metafile: mutationPayload{
		variant: "sha256:" + strings.Repeat("d", 64), size: int64(len(raw)), bytes: raw,
	}, SavePath: "/downloads", Identity: downloader.TypedIdentity{InfoHashV1: testHash}})
	if err == nil || receipt.Complete || receipt.StopReason != "response_invalid" || requestCount() != 3 {
		t.Fatalf("mismatched accepted identity was trusted: receipt=%#v requests=%d err=%v", receipt, requestCount(), err)
	}
}

func TestAddStoppedRejectsNonExactPayloadAndNormalizesClientConfigID(t *testing.T) {
	server, requestCount := newMutationFixture(t, false, "short", nil)
	defer server.Close()
	adapter, _ := New(server.URL)
	credential, _ := downloader.NewCredential("alice", "secret")
	session, err := adapter.OpenMutationSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	receipt, err := session.AddStopped(context.Background(), downloader.AddStoppedRequest{Metafile: mutationPayload{
		variant: "sha256:" + strings.Repeat("c", 64), size: 4, bytes: []byte("abc"),
	}, SavePath: "/downloads", Identity: downloader.TypedIdentity{InfoHashV1: testHash}})
	serverRequests := requestCount()
	if err == nil || receipt.Complete || receipt.RequestsAttempted != 1 || session.RequestsMade() != 3 || serverRequests < 2 || serverRequests > 3 {
		t.Fatalf("short payload was accepted: receipt=%#v requests=%d/%d err=%v", receipt, serverRequests, session.RequestsMade(), err)
	}

	first, _ := New("https://EXAMPLE.com:443/transmission/rpc")
	second, _ := New("https://example.com/transmission/rpc")
	firstID, err := first.ClientConfigID("alice")
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := second.ClientConfigID("alice")
	if err != nil || firstID != secondID {
		t.Fatalf("normalized config IDs differ: %q %q err=%v", firstID, secondID, err)
	}
	thirdID, _ := second.ClientConfigID("bob")
	if thirdID == firstID {
		t.Fatal("username was not bound into the client configuration ID")
	}
}

func newMutationFixture(t *testing.T, modern bool, outcome string, capture func(map[string]json.RawMessage)) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		if r.Header.Get("X-Transmission-Session-Id") == "" {
			w.Header().Set("X-Transmission-Session-Id", "mutation-token")
			if modern {
				w.Header().Set("X-Transmission-Rpc-Version", "6.0.0")
			}
			w.WriteHeader(http.StatusConflict)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			if outcome == "short" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var request map[string]json.RawMessage
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var method string
		_ = json.Unmarshal(request["method"], &method)
		sessionMethod, addMethod := "session-get", "torrent-add"
		if modern {
			sessionMethod, addMethod = "session_get", "torrent_add"
		}
		switch method {
		case sessionMethod:
			r.Body = io.NopCloser(bytes.NewReader(body))
			if modern {
				writeRPCResponse(t, w, r, true, map[string]any{"version": "4.1.0", "rpc_version_semver": "6.0.0", "rpc_version": 18})
			} else {
				writeRPCResponse(t, w, r, false, map[string]any{"version": "4.0.6", "rpc-version-semver": "5.3.0", "rpc-version": 17})
			}
		case addMethod:
			if capture != nil {
				capture(request)
			}
			if outcome == "csrf" {
				w.Header().Set("X-Transmission-Session-Id", "new-token-no-replay")
				w.WriteHeader(http.StatusConflict)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			key := "torrent-added"
			hashKey := "hashString"
			if outcome == "duplicate" {
				key = "torrent-duplicate"
			}
			if modern {
				key = strings.ReplaceAll(key, "-", "_")
				hashKey = "hash_string"
			}
			responseHash := testHash
			if outcome == "wrong_hash" {
				responseHash = strings.Repeat("f", 40)
			}
			writeRPCResponse(t, w, r, modern, map[string]any{key: map[string]any{"id": 7, "name": "bundle", hashKey: responseHash}})
		default:
			t.Errorf("unexpected method %q", method)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	return server, func() int {
		mu.Lock()
		defer mu.Unlock()
		return requests
	}
}
