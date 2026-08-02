package qbittorrent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

func TestEndpointPolicy(t *testing.T) {
	allowed := []string{"https://seedbox.example", "http://127.0.0.1:8080", "http://[::1]:8080"}
	for _, endpoint := range allowed {
		if _, err := New(endpoint); err != nil {
			t.Fatalf("%s should be allowed: %v", endpoint, err)
		}
	}
	blocked := []string{"http://localhost:8080", "http://192.168.1.2:8080", "http://seedbox.example", "https://user:pass@example.com", "ftp://example.com"}
	for _, endpoint := range blocked {
		if _, err := New(endpoint); err == nil {
			t.Fatalf("%s should be blocked", endpoint)
		}
	}
}

func TestStatusAuthenticatesAndReadsVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			if r.Method != http.MethodPost || r.FormValue("username") != "alice" || r.FormValue("password") != "secret" {
				http.Error(w, "bad login", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/", HttpOnly: true})
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/app/version":
			if _, err := r.Cookie("SID"); err != nil {
				http.Error(w, "missing session", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("v5.0.0"))
		case "/api/v2/app/webapiVersion":
			_, _ = w.Write([]byte("2.11.0"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := downloader.NewCredential("alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	status, err := adapter.Status(context.Background(), credential)
	if err != nil || status.Version != "v5.0.0" || status.WebAPIVersion != "2.11.0" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestLoginBodyIsNotReplayedAcrossOriginRedirect(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests.Add(1)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	adapter, err := New(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := downloader.NewCredential("alice", "CANARY-QBIT-PASSWORD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Status(context.Background(), credential); err == nil {
		t.Fatal("expected cross-origin redirect rejection")
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("redirect target received %d requests", targetRequests.Load())
	}
}
