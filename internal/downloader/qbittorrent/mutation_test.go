package qbittorrent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

type testMetafilePayload struct {
	raw     []byte
	size    int64
	variant string
	opened  bool
}

func (payload *testMetafilePayload) VariantID() string {
	if payload.variant != "" {
		return payload.variant
	}
	return "sha256:" + strings.Repeat("a", 64)
}
func (payload *testMetafilePayload) SizeBytes() int64 {
	if payload.size != 0 {
		return payload.size
	}
	return int64(len(payload.raw))
}
func (payload *testMetafilePayload) Open() (io.Reader, error) {
	if payload.opened {
		return nil, fmt.Errorf("opened twice")
	}
	payload.opened = true
	return bytes.NewReader(payload.raw), nil
}

func TestExactMetafileReaderRejectsDeclaredLengthMismatch(t *testing.T) {
	for name, raw := range map[string][]byte{"short": []byte("ab"), "long": []byte("abcd")} {
		t.Run(name, func(t *testing.T) {
			reader := &exactMetafileReader{reader: bytes.NewReader(raw), remaining: 3}
			_, err := io.ReadAll(reader)
			if name == "short" && !errors.Is(err, io.ErrUnexpectedEOF) || name == "long" && !errors.Is(err, errAddPayloadLength) {
				t.Fatalf("unexpected mismatch error: %v", err)
			}
		})
	}
}

func TestAddStoppedRejectsMalformedVariantBeforeRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v2/auth/login" {
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/"})
			_, _ = w.Write([]byte("Ok."))
			return
		}
		t.Fatalf("unexpected add request: %s", request.URL.Path)
	}))
	defer server.Close()
	adapter, _ := New(server.URL)
	credential, _ := downloader.NewCredential("alice", "secret")
	session, err := adapter.OpenMutationSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	receipt, err := session.AddStopped(context.Background(), downloader.AddStoppedRequest{
		Metafile: &testMetafilePayload{raw: []byte("x"), variant: "sha256:" + strings.Repeat("Z", 64)}, SavePath: "/downloads",
	})
	if err == nil || receipt.RequestsAttempted != 0 || session.RequestsMade() != 1 {
		t.Fatalf("receipt=%#v requests=%d err=%v", receipt, session.RequestsMade(), err)
	}
}

func TestAddStoppedPreCanceledDoesNotOpenPayloadOrSendRequest(t *testing.T) {
	var adds atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v2/auth/login" {
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/"})
			_, _ = w.Write([]byte("Ok."))
			return
		}
		adds.Add(1)
	}))
	defer server.Close()
	adapter, _ := New(server.URL)
	credential, _ := downloader.NewCredential("alice", "secret")
	session, err := adapter.OpenMutationSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	payload := &testMetafilePayload{raw: []byte("d4:infod4:name1:xee")}
	receipt, err := session.AddStopped(ctx, downloader.AddStoppedRequest{Metafile: payload, SavePath: "/downloads"})
	if !errors.Is(err, context.Canceled) || receipt.RequestsAttempted != 0 || receipt.StopReason != "context_cancelled" ||
		payload.opened || adds.Load() != 0 || session.RequestsMade() != 1 {
		t.Fatalf("receipt=%#v opened=%t adds=%d requests=%d err=%v", receipt, payload.opened, adds.Load(), session.RequestsMade(), err)
	}
}

func TestAddStoppedSubmitsExactPayloadOnce(t *testing.T) {
	var requests atomic.Int32
	raw := []byte("d4:infod4:name1:xee")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		switch request.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/"})
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/add":
			if err := request.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse multipart: %v", err)
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			if request.FormValue("savepath") != "/downloads/new" || request.FormValue("paused") != "true" ||
				request.FormValue("stopped") != "true" || request.FormValue("skip_checking") != "false" {
				t.Errorf("unexpected form values: %#v", request.MultipartForm.Value)
			}
			files := request.MultipartForm.File["torrents"]
			if len(files) != 1 || files[0].Filename != "metafile.torrent" {
				t.Fatalf("unexpected upload: %#v", files)
			}
			file, err := files[0].Open()
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(file)
			_ = file.Close()
			if !bytes.Equal(got, raw) {
				t.Fatalf("payload changed: %q", got)
			}
			_, _ = w.Write([]byte("Ok."))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	adapter, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	configID, err := adapter.ClientConfigID("alice")
	if err != nil || !strings.HasPrefix(configID, "sha256:") {
		t.Fatalf("config ID=%q err=%v", configID, err)
	}
	credential, _ := downloader.NewCredential("alice", "secret")
	session, err := adapter.OpenMutationSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	payload := &testMetafilePayload{raw: raw}
	receipt, err := session.AddStopped(context.Background(), downloader.AddStoppedRequest{Metafile: payload, SavePath: "/downloads/new"})
	if err != nil || !receipt.Complete || receipt.RequestsAttempted != 1 || receipt.AutomaticRetries != 0 ||
		receipt.RedirectsFollowed != 0 || !receipt.BytesSubmittedKnown || receipt.BytesSubmitted != int64(len(raw)) {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	if requests.Load() != 2 || session.RequestsMade() != 2 || !payload.opened {
		t.Fatalf("requests=%d/%d opened=%t", requests.Load(), session.RequestsMade(), payload.opened)
	}
}

func TestAddStoppedTransportFailureDoesNotLeakPathOrPayload(t *testing.T) {
	const canary = "CANARY-PRIVATE-PATH"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v2/auth/login" {
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/"})
			_, _ = w.Write([]byte("Ok."))
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("server cannot hijack")
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = connection.Close()
	}))
	defer server.Close()
	adapter, _ := New(server.URL)
	credential, _ := downloader.NewCredential("alice", "secret")
	session, err := adapter.OpenMutationSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	receipt, err := session.AddStopped(context.Background(), downloader.AddStoppedRequest{
		Metafile: &testMetafilePayload{raw: []byte(canary)}, SavePath: "/" + canary,
	})
	if err == nil || receipt.RequestsAttempted != 1 || receipt.Complete || strings.Contains(err.Error(), canary) {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
}

func TestAddStoppedNeverFollowsRedirectOrRetries(t *testing.T) {
	const canary = "CLIENT-ADD-REDIRECT-CANARY"
	var login, add, redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/auth/login":
			login.Add(1)
			http.SetCookie(writer, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/"})
			_, _ = writer.Write([]byte("Ok."))
		case "/api/v2/torrents/add":
			add.Add(1)
			writer.Header().Set("Location", "/redirected?secret="+canary)
			writer.WriteHeader(http.StatusTemporaryRedirect)
		case "/redirected":
			redirected.Add(1)
			_, _ = writer.Write([]byte("Ok."))
		default:
			http.NotFound(writer, request)
		}
	}))
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
	receipt, err := session.AddStopped(context.Background(), downloader.AddStoppedRequest{
		Metafile: &testMetafilePayload{raw: []byte("d4:infod4:name1:xee")}, SavePath: "/downloads",
	})
	if err == nil || receipt.Complete || receipt.RequestsAttempted != 1 || receipt.AutomaticRetries != 0 ||
		receipt.RedirectsFollowed != 0 || login.Load() != 1 || add.Load() != 1 || redirected.Load() != 0 ||
		session.RequestsMade() != 2 || strings.Contains(err.Error(), canary) {
		t.Fatalf("receipt=%#v login=%d add=%d redirected=%d requests=%d err=%v", receipt, login.Load(), add.Load(), redirected.Load(), session.RequestsMade(), err)
	}
}
