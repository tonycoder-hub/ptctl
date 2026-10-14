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

var testAddIdentity = downloader.TypedIdentity{InfoHashV1: strings.Repeat("ab", 20)}

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
				request.FormValue("stopped") != "true" || request.FormValue("skip_checking") != "false" ||
				request.FormValue("autoTMM") != "false" || request.FormValue("contentLayout") != "Original" ||
				request.FormValue("root_folder") != "true" {
				t.Errorf("unexpected form values: %#v", request.MultipartForm.Value)
			}
			for name, values := range request.MultipartForm.Value {
				if len(values) != 1 {
					t.Errorf("form field %q repeated: %#v", name, values)
				}
			}
			if len(request.MultipartForm.Value) != 7 {
				t.Errorf("unexpected form fields: %#v", request.MultipartForm.Value)
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
	receipt, err := session.AddStopped(context.Background(), downloader.AddStoppedRequest{Metafile: payload, SavePath: "/downloads/new", Identity: testAddIdentity})
	if err != nil || !receipt.Complete || receipt.RequestsAttempted != 1 || receipt.AutomaticRetries != 0 ||
		receipt.RedirectsFollowed != 0 || !receipt.BytesSubmittedKnown || receipt.BytesSubmitted != int64(len(raw)) {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	if requests.Load() != 2 || session.RequestsMade() != 2 || !payload.opened {
		t.Fatalf("requests=%d/%d opened=%t", requests.Load(), session.RequestsMade(), payload.opened)
	}
}

func TestAddStoppedRejectsMissingIdentityBeforeRequest(t *testing.T) {
	var adds atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v2/auth/login" {
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
	payload := &testMetafilePayload{raw: []byte("d4:infod4:name1:xee")}
	receipt, err := session.AddStopped(context.Background(), downloader.AddStoppedRequest{Metafile: payload, SavePath: "/downloads"})
	if err == nil || receipt.StopReason != "payload_invalid" || receipt.RequestsAttempted != 0 || payload.opened || adds.Load() != 0 || session.RequestsMade() != 1 {
		t.Fatalf("receipt=%#v opened=%t adds=%d requests=%d err=%v", receipt, payload.opened, adds.Load(), session.RequestsMade(), err)
	}
}

func TestAddStoppedResponseShapesAcrossVersions(t *testing.T) {
	v1 := strings.Repeat("ab", 20)
	v2 := strings.Repeat("cd", 32)
	summary := func(id string) string {
		// qBittorrent 5.2.4 addAction; QJsonDocument::Compact sorts keys.
		return `{"added_torrent_ids":["` + id + `"],"failure_count":0,"pending_count":0,"success_count":1}`
	}
	for _, test := range []struct {
		name       string
		identity   downloader.TypedIdentity
		status     int
		body       string
		wantReason string
	}{
		{name: "4.x-5.1 accepted", identity: downloader.TypedIdentity{InfoHashV1: v1}, status: http.StatusOK, body: "Ok."},
		{name: "5.2 v1 accepted", identity: downloader.TypedIdentity{InfoHashV1: v1}, status: http.StatusOK, body: summary(v1)},
		{name: "5.2 hybrid truncated v2", identity: downloader.TypedIdentity{InfoHashV1: v1, InfoHashV2: v2}, status: http.StatusOK, body: summary(v2[:40])},
		{name: "5.2 hybrid libtorrent 1.2 v1", identity: downloader.TypedIdentity{InfoHashV1: v1, InfoHashV2: v2}, status: http.StatusOK, body: summary(v1)},
		{name: "5.2 v2-only truncated v2", identity: downloader.TypedIdentity{InfoHashV2: v2}, status: http.StatusOK, body: summary(v2[:40])},
		{name: "4.x-5.1 failed", identity: downloader.TypedIdentity{InfoHashV1: v1}, status: http.StatusOK, body: "Fails.", wantReason: "response_invalid"},
		{name: "5.2 all failed", identity: downloader.TypedIdentity{InfoHashV1: v1}, status: http.StatusConflict, body: "Conflict", wantReason: "http_rejected"},
		{name: "5.2 pending", identity: downloader.TypedIdentity{InfoHashV1: v1}, status: http.StatusAccepted, body: `{"added_torrent_ids":[],"failure_count":0,"pending_count":1,"success_count":0}`, wantReason: "http_rejected"},
		{name: "5.2 other identity", identity: downloader.TypedIdentity{InfoHashV1: v1}, status: http.StatusOK, body: summary(strings.Repeat("ef", 20)), wantReason: "response_invalid"},
		{name: "5.2 v2 identity as v1", identity: downloader.TypedIdentity{InfoHashV2: v2}, status: http.StatusOK, body: summary(v1), wantReason: "response_invalid"},
		{name: "5.2 uppercase id", identity: downloader.TypedIdentity{InfoHashV1: v1}, status: http.StatusOK, body: summary(strings.ToUpper(v1)), wantReason: "response_invalid"},
		{name: "5.2 partial failure", identity: downloader.TypedIdentity{InfoHashV1: v1}, status: http.StatusOK, body: `{"added_torrent_ids":["` + v1 + `"],"failure_count":1,"pending_count":0,"success_count":1}`, wantReason: "response_invalid"},
		{name: "5.2 two ids", identity: downloader.TypedIdentity{InfoHashV1: v1}, status: http.StatusOK, body: `{"added_torrent_ids":["` + v1 + `","` + v1 + `"],"failure_count":0,"pending_count":0,"success_count":1}`, wantReason: "response_invalid"},
		{name: "5.2 missing count", identity: downloader.TypedIdentity{InfoHashV1: v1}, status: http.StatusOK, body: `{"added_torrent_ids":["` + v1 + `"],"failure_count":0,"success_count":1}`, wantReason: "response_invalid"},
		{name: "5.2 duplicate key", identity: downloader.TypedIdentity{InfoHashV1: v1}, status: http.StatusOK, body: `{"added_torrent_ids":["` + v1 + `"],"failure_count":0,"pending_count":0,"success_count":1,"success_count":1}`, wantReason: "response_invalid"},
		{name: "5.2 trailing data", identity: downloader.TypedIdentity{InfoHashV1: v1}, status: http.StatusOK, body: summary(v1) + "{}", wantReason: "response_invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var adds atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/v2/auth/login":
					http.SetCookie(w, &http.Cookie{Name: "QBT_SID_8080", Value: "synthetic", Path: "/"})
					w.WriteHeader(http.StatusNoContent)
				case "/api/v2/torrents/add":
					adds.Add(1)
					if strings.HasPrefix(test.body, "{") {
						w.Header().Set("Content-Type", "application/json")
					}
					w.WriteHeader(test.status)
					_, _ = w.Write([]byte(test.body))
				default:
					http.NotFound(w, request)
				}
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
				Metafile: &testMetafilePayload{raw: []byte("d4:infod4:name1:xee")}, SavePath: "/downloads", Identity: test.identity,
			})
			if adds.Load() != 1 || receipt.RequestsAttempted != 1 || receipt.AutomaticRetries != 0 || session.RequestsMade() != 2 {
				t.Fatalf("adds=%d receipt=%#v requests=%d", adds.Load(), receipt, session.RequestsMade())
			}
			if test.wantReason == "" {
				if err != nil || !receipt.Complete || receipt.StopReason != "" {
					t.Fatalf("accepted shape was rejected: receipt=%#v err=%v", receipt, err)
				}
				return
			}
			if err == nil || receipt.Complete || receipt.StopReason != test.wantReason {
				t.Fatalf("receipt=%#v err=%v, want stop reason %q", receipt, err, test.wantReason)
			}
		})
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
		Metafile: &testMetafilePayload{raw: []byte(canary)}, SavePath: "/" + canary, Identity: testAddIdentity,
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
		Metafile: &testMetafilePayload{raw: []byte("d4:infod4:name1:xee")}, SavePath: "/downloads", Identity: testAddIdentity,
	})
	if err == nil || receipt.Complete || receipt.RequestsAttempted != 1 || receipt.AutomaticRetries != 0 ||
		receipt.RedirectsFollowed != 0 || login.Load() != 1 || add.Load() != 1 || redirected.Load() != 0 ||
		session.RequestsMade() != 2 || strings.Contains(err.Error(), canary) {
		t.Fatalf("receipt=%#v login=%d add=%d redirected=%d requests=%d err=%v", receipt, login.Load(), add.Load(), redirected.Load(), session.RequestsMade(), err)
	}
}

func TestExistingJobControlUsesVersionBoundRoutesAndExactForm(t *testing.T) {
	for _, test := range []struct {
		name          string
		version       string
		wantProtocol  string
		wantStartPath string
	}{
		{name: "v4 resume", version: "v4.6.7", wantProtocol: downloader.ControlProtocolQBittorrentV4, wantStartPath: "/api/v2/torrents/resume"},
		{name: "v5 start", version: "v5.0.4", wantProtocol: downloader.ControlProtocolQBittorrentV5, wantStartPath: "/api/v2/torrents/start"},
	} {
		t.Run(test.name, func(t *testing.T) {
			const opaque = "opaque&a=b?#%"
			var requests atomic.Int32
			var gotPaths []string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				switch request.URL.Path {
				case "/api/v2/auth/login":
					http.SetCookie(writer, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/"})
					_, _ = writer.Write([]byte("Ok."))
				case "/api/v2/app/version":
					_, _ = writer.Write([]byte(test.version))
				case "/api/v2/torrents/recheck", test.wantStartPath:
					if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
						t.Errorf("unexpected request: %s %s", request.Method, request.Header.Get("Content-Type"))
					}
					if err := request.ParseForm(); err != nil || len(request.PostForm) != 1 || len(request.PostForm["hashes"]) != 1 || request.PostForm.Get("hashes") != opaque {
						t.Errorf("unexpected form: %#v err=%v", request.PostForm, err)
					}
					gotPaths = append(gotPaths, request.URL.Path)
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
			session, err := adapter.OpenExistingJobMutationSession(context.Background(), credential)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			descriptor, err := session.ReadExistingJobControlDescriptor(context.Background())
			if err != nil || descriptor.Protocol != test.wantProtocol {
				t.Fatalf("descriptor=%#v err=%v", descriptor, err)
			}
			recheck, err := session.Recheck(context.Background(), downloader.ExistingJobMutationRequest{JobKey: opaque})
			if err != nil || !recheck.Complete || recheck.RequestsAttempted != 1 || !recheck.RequestBytesKnown {
				t.Fatalf("recheck=%#v err=%v", recheck, err)
			}
			start, err := session.Start(context.Background(), downloader.ExistingJobMutationRequest{JobKey: opaque})
			if err != nil || !start.Complete || start.RequestsAttempted != 1 || !start.RequestBytesKnown {
				t.Fatalf("start=%#v err=%v", start, err)
			}
			if requests.Load() != 4 || session.RequestsMade() != 4 || len(gotPaths) != 2 || gotPaths[0] != "/api/v2/torrents/recheck" || gotPaths[1] != test.wantStartPath {
				t.Fatalf("requests=%d/%d paths=%v", requests.Load(), session.RequestsMade(), gotPaths)
			}
		})
	}
}

func TestExistingJobControlRejectsUnsupportedVersionAndPreCanceledMutation(t *testing.T) {
	var mutations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(writer, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/"})
			_, _ = writer.Write([]byte("Ok."))
		case "/api/v2/app/version":
			_, _ = writer.Write([]byte("v6.0.0"))
		default:
			mutations.Add(1)
		}
	}))
	defer server.Close()
	adapter, _ := New(server.URL)
	credential, _ := downloader.NewCredential("alice", "secret")
	session, err := adapter.OpenExistingJobMutationSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.ReadExistingJobControlDescriptor(context.Background()); err == nil {
		t.Fatal("unsupported qBittorrent major was accepted")
	}
	for _, job := range []string{"all", "a|b"} {
		for _, mutate := range []func(context.Context, downloader.ExistingJobMutationRequest) (downloader.ExistingJobMutationReceipt, error){session.Recheck, session.Start} {
			receipt, err := mutate(context.Background(), downloader.ExistingJobMutationRequest{JobKey: job})
			if err == nil || receipt.RequestsAttempted != 0 || receipt.StopReason != "job_locator_invalid" || mutations.Load() != 0 || session.RequestsMade() != 2 {
				t.Fatalf("broad selector %q receipt=%#v mutations=%d requests=%d err=%v", job, receipt, mutations.Load(), session.RequestsMade(), err)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	receipt, err := session.Recheck(ctx, downloader.ExistingJobMutationRequest{JobKey: "opaque"})
	if !errors.Is(err, context.Canceled) || receipt.RequestsAttempted != 0 || receipt.StopReason != "context_cancelled" || mutations.Load() != 0 || session.RequestsMade() != 2 {
		t.Fatalf("receipt=%#v mutations=%d requests=%d err=%v", receipt, mutations.Load(), session.RequestsMade(), err)
	}
}

func TestExistingJobControlDoesNotRedirectRetryOrLeakOpaqueKey(t *testing.T) {
	const opaque = "CANARY-OPAQUE-JOB&a=b"
	const responseCanary = "CANARY-LOCATION"
	var mutation, redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(writer, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/"})
			_, _ = writer.Write([]byte("Ok."))
		case "/api/v2/app/version":
			_, _ = writer.Write([]byte("v5.0.0"))
		case "/api/v2/torrents/recheck":
			mutation.Add(1)
			writer.Header().Set("Location", "/redirected?secret="+responseCanary)
			writer.WriteHeader(http.StatusTemporaryRedirect)
		case "/redirected":
			redirected.Add(1)
		default:
			http.NotFound(writer, request)
		}
	}))
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
	receipt, err := session.Recheck(context.Background(), downloader.ExistingJobMutationRequest{JobKey: opaque})
	if err == nil || receipt.Complete || receipt.RequestsAttempted != 1 || receipt.AutomaticRetries != 0 || receipt.RedirectsFollowed != 0 ||
		mutation.Load() != 1 || redirected.Load() != 0 || session.RequestsMade() != 3 || strings.Contains(err.Error(), opaque) || strings.Contains(err.Error(), responseCanary) {
		t.Fatalf("receipt=%#v mutation=%d redirected=%d requests=%d err=%v", receipt, mutation.Load(), redirected.Load(), session.RequestsMade(), err)
	}
}
