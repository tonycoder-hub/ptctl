package transmission

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

func TestExistingJobControlUsesExactHashSelectorForBothProtocolsAndActions(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, effect := range []string{downloader.ControlEffectRecheck, downloader.ControlEffectStart} {
			name := "legacy"
			if modern {
				name = "json-rpc"
			}
			t.Run(name+"/"+effect, func(t *testing.T) {
				server, state := newControlFixture(t, modern, "success")
				defer server.Close()
				adapter, err := New(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				credential, _ := downloader.NewCredential("alice", "secret")
				session, err := adapter.OpenExistingJobMutationSession(context.Background(), credential)
				if err != nil {
					t.Fatal(err)
				}
				defer session.Close()
				if session.RequestsMade() != 2 {
					t.Fatalf("open requests = %d, want 2", session.RequestsMade())
				}
				descriptor, err := session.ReadExistingJobControlDescriptor(context.Background())
				if err != nil || descriptor.Driver != downloader.DriverTransmission || session.RequestsMade() != 2 {
					t.Fatalf("descriptor=%#v requests=%d err=%v", descriptor, session.RequestsMade(), err)
				}
				wantProtocol := downloader.ControlProtocolTransmissionV5
				if modern {
					wantProtocol = downloader.ControlProtocolTransmissionV6
				}
				if descriptor.Protocol != wantProtocol {
					t.Fatalf("protocol = %q, want %q", descriptor.Protocol, wantProtocol)
				}
				request := downloader.ExistingJobMutationRequest{JobKey: testHash}
				var receipt downloader.ExistingJobMutationReceipt
				if effect == downloader.ControlEffectStart {
					receipt, err = session.Start(context.Background(), request)
				} else {
					receipt, err = session.Recheck(context.Background(), request)
				}
				captured, wireRequests, actionRequests := state.snapshot()
				if err != nil || !receipt.Complete || receipt.Effect != effect || receipt.RequestsAttempted != 1 ||
					receipt.AutomaticRetries != 0 || receipt.RedirectsFollowed != 0 || !receipt.RequestBytesKnown ||
					receipt.RequestBytes != int64(len(captured)) || receipt.RequestID <= 0 ||
					session.RequestsMade() != 3 || wireRequests != 3 || actionRequests != 1 {
					t.Fatalf("receipt=%#v session=%d wire=%d actions=%d err=%v", receipt, session.RequestsMade(), wireRequests, actionRequests, err)
				}
				expected, err := downloader.MarshalExistingJobMutationRequest(descriptor, effect, testHash, receipt.RequestID)
				if err != nil || !bytes.Equal(captured, expected) {
					t.Fatalf("wire request differs:\n got %s\nwant %s\nerr=%v", captured, expected, err)
				}
				if _, err := session.ReadExistingJobControlDescriptor(context.Background()); err == nil || session.RequestsMade() != 3 {
					t.Fatalf("second descriptor read crossed a boundary: requests=%d err=%v", session.RequestsMade(), err)
				}
			})
		}
	}
}

func TestExistingJobControlNeverReplaysExpiredCSRFOrLeaksSelector(t *testing.T) {
	server, state := newControlFixture(t, true, "csrf")
	defer server.Close()
	adapter, _ := New(server.URL)
	credential, _ := downloader.NewCredential("alice", "secret")
	session, err := adapter.OpenExistingJobMutationSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.ReadExistingJobControlDescriptor(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt, err := session.Recheck(context.Background(), downloader.ExistingJobMutationRequest{JobKey: testHash})
	_, requests, actions := state.snapshot()
	if err == nil || receipt.Complete || receipt.StopReason != "csrf_expired" || receipt.RequestsAttempted != 1 ||
		session.RequestsMade() != 3 || requests != 3 || actions != 1 || strings.Contains(err.Error(), testHash) ||
		strings.Contains(err.Error(), server.URL) {
		t.Fatalf("receipt=%#v requests=%d actions=%d err=%v", receipt, requests, actions, err)
	}
}

func TestExistingJobControlRejectsInvalidOrCancelledRequestBeforeNetwork(t *testing.T) {
	server, state := newControlFixture(t, true, "success")
	defer server.Close()
	adapter, _ := New(server.URL)
	credential, _ := downloader.NewCredential("alice", "secret")
	session, err := adapter.OpenExistingJobMutationSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.ReadExistingJobControlDescriptor(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt, err := session.Recheck(context.Background(), downloader.ExistingJobMutationRequest{JobKey: "opaque-not-a-v1-hash"})
	if _, requests, actions := state.snapshot(); err == nil || receipt.StopReason != "job_locator_invalid" || requests != 2 || actions != 0 {
		t.Fatalf("invalid receipt=%#v requests=%d actions=%d err=%v", receipt, requests, actions, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	receipt, err = session.Start(ctx, downloader.ExistingJobMutationRequest{JobKey: testHash})
	if _, requests, actions := state.snapshot(); err == nil || receipt.StopReason != "context_cancelled" || requests != 2 || actions != 0 {
		t.Fatalf("cancelled receipt=%#v requests=%d actions=%d err=%v", receipt, requests, actions, err)
	}
}

func TestExistingJobControlRejectsNonEmptySuccessResult(t *testing.T) {
	server, state := newControlFixture(t, false, "nonempty")
	defer server.Close()
	adapter, _ := New(server.URL)
	credential, _ := downloader.NewCredential("alice", "secret")
	session, err := adapter.OpenExistingJobMutationSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.ReadExistingJobControlDescriptor(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt, err := session.Start(context.Background(), downloader.ExistingJobMutationRequest{JobKey: testHash})
	_, requests, actions := state.snapshot()
	if err == nil || receipt.Complete || receipt.StopReason != "response_invalid" || requests != 3 || actions != 1 {
		t.Fatalf("receipt=%#v requests=%d actions=%d err=%v", receipt, requests, actions, err)
	}
}

type controlFixtureState struct {
	mu             sync.Mutex
	requests       int
	actionRequests int
	captured       []byte
}

func (state *controlFixtureState) snapshot() ([]byte, int, int) {
	state.mu.Lock()
	defer state.mu.Unlock()
	return append([]byte(nil), state.captured...), state.requests, state.actionRequests
}

func newControlFixture(t *testing.T, modern bool, outcome string) (*httptest.Server, *controlFixtureState) {
	t.Helper()
	state := &controlFixtureState{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		state.mu.Lock()
		state.requests++
		state.mu.Unlock()
		if request.URL.Path != defaultRPCPath {
			http.NotFound(writer, request)
			return
		}
		username, password, ok := request.BasicAuth()
		if !ok || username != "alice" || password != "secret" {
			t.Errorf("missing control authentication")
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Header.Get("X-Transmission-Session-Id") == "" {
			writer.Header().Set("X-Transmission-Session-Id", "control-token")
			if modern {
				writer.Header().Set("X-Transmission-Rpc-Version", "6.0.0")
			}
			writer.WriteHeader(http.StatusConflict)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read control request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var rpc map[string]json.RawMessage
		if err := json.Unmarshal(body, &rpc); err != nil {
			t.Errorf("decode control request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var method string
		_ = json.Unmarshal(rpc["method"], &method)
		sessionMethod := "session-get"
		if modern {
			sessionMethod = "session_get"
		}
		if method == sessionMethod {
			request.Body = io.NopCloser(bytes.NewReader(body))
			if modern {
				writeRPCResponse(t, writer, request, true, map[string]any{"version": "4.1.0", "rpc_version_semver": "6.0.0", "rpc_version": 18})
			} else {
				writeRPCResponse(t, writer, request, false, map[string]any{"version": "4.0.6", "rpc-version-semver": "5.3.0", "rpc-version": 17})
			}
			return
		}
		validMethod := method == "torrent-verify" || method == "torrent-start" || method == "torrent_verify" || method == "torrent_start" ||
			method == "torrent-remove" || method == "torrent_remove" || method == "torrent-stop" || method == "torrent_stop"
		if !validMethod {
			t.Errorf("unexpected control method %q", method)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		state.mu.Lock()
		state.actionRequests++
		state.captured = append([]byte(nil), body...)
		state.mu.Unlock()
		if outcome == "csrf" {
			writer.Header().Set("X-Transmission-Session-Id", "new-token-no-replay")
			writer.WriteHeader(http.StatusConflict)
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		result := map[string]any{}
		if outcome == "nonempty" {
			result["CANARY-UNEXPECTED"] = true
		}
		writeRPCResponse(t, writer, request, modern, result)
	}))
	return server, state
}
