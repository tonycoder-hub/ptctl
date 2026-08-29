package qbittorrent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

func TestExistingJobStopUsesVersionBoundRouteAndExactSelector(t *testing.T) {
	for _, test := range []struct {
		name         string
		version      string
		wantProtocol string
		wantPath     string
	}{
		{name: "v4 pause", version: "v4.6.7", wantProtocol: downloader.ControlProtocolQBittorrentV4, wantPath: "/api/v2/torrents/pause"},
		{name: "v5 stop", version: "v5.0.4", wantProtocol: downloader.ControlProtocolQBittorrentV5, wantPath: "/api/v2/torrents/stop"},
	} {
		t.Run(test.name, func(t *testing.T) {
			const opaque = "opaque&a=b?#%"
			var requests, stops atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				switch request.URL.Path {
				case "/api/v2/auth/login":
					http.SetCookie(writer, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/"})
					_, _ = writer.Write([]byte("Ok."))
				case "/api/v2/app/version":
					_, _ = writer.Write([]byte(test.version))
				case test.wantPath:
					stops.Add(1)
					if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
						t.Errorf("unexpected request: %s %s", request.Method, request.Header.Get("Content-Type"))
					}
					if err := request.ParseForm(); err != nil || len(request.PostForm) != 1 || len(request.PostForm["hashes"]) != 1 || request.PostForm.Get("hashes") != opaque {
						t.Errorf("unexpected form: %#v err=%v", request.PostForm, err)
					}
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
			session, err := adapter.OpenExistingJobStopSession(context.Background(), credential)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			descriptor, err := session.ReadExistingJobStopDescriptor(context.Background())
			if err != nil || descriptor.Protocol != test.wantProtocol {
				t.Fatalf("descriptor=%#v err=%v", descriptor, err)
			}
			receipt, err := session.Stop(context.Background(), downloader.ExistingJobMutationRequest{JobKey: opaque})
			if err != nil || !receipt.Complete || receipt.Effect != downloader.StopEffect || receipt.RequestsAttempted != 1 ||
				receipt.AutomaticRetries != 0 || receipt.RedirectsFollowed != 0 || !receipt.RequestBytesKnown {
				t.Fatalf("receipt=%#v err=%v", receipt, err)
			}
			second, secondErr := session.Stop(context.Background(), downloader.ExistingJobMutationRequest{JobKey: opaque})
			if secondErr == nil || second.RequestsAttempted != 0 || requests.Load() != 3 || stops.Load() != 1 || session.RequestsMade() != 3 {
				t.Fatalf("second=%#v requests=%d stops=%d session=%d err=%v", second, requests.Load(), stops.Load(), session.RequestsMade(), secondErr)
			}
		})
	}
}

func TestExistingJobStopRejectsInvalidOrCancelledRequestBeforeNetwork(t *testing.T) {
	var stops atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(writer, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/"})
			_, _ = writer.Write([]byte("Ok."))
		case "/api/v2/app/version":
			_, _ = writer.Write([]byte("v5.0.0"))
		default:
			stops.Add(1)
		}
	}))
	defer server.Close()
	adapter, _ := New(server.URL)
	credential, _ := downloader.NewCredential("alice", "secret")
	session, err := adapter.OpenExistingJobStopSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.ReadExistingJobStopDescriptor(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt, err := session.Stop(context.Background(), downloader.ExistingJobMutationRequest{JobKey: "all"})
	if err == nil || receipt.RequestsAttempted != 0 || receipt.StopReason != "job_locator_invalid" || stops.Load() != 0 || session.RequestsMade() != 2 {
		t.Fatalf("invalid receipt=%#v stops=%d requests=%d err=%v", receipt, stops.Load(), session.RequestsMade(), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	receipt, err = session.Stop(ctx, downloader.ExistingJobMutationRequest{JobKey: "opaque"})
	if !errors.Is(err, context.Canceled) || receipt.RequestsAttempted != 0 || receipt.StopReason != "context_cancelled" || stops.Load() != 0 || session.RequestsMade() != 2 {
		t.Fatalf("cancelled receipt=%#v stops=%d requests=%d err=%v", receipt, stops.Load(), session.RequestsMade(), err)
	}
}

func TestExistingJobStopNeverRedirectsRetriesOrLeaksSelector(t *testing.T) {
	const opaque = "CANARY-OPAQUE-JOB&a=b"
	const responseCanary = "CANARY-STOP-LOCATION"
	var stop, redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(writer, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/"})
			_, _ = writer.Write([]byte("Ok."))
		case "/api/v2/app/version":
			_, _ = writer.Write([]byte("v5.0.0"))
		case "/api/v2/torrents/stop":
			stop.Add(1)
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
	session, err := adapter.OpenExistingJobStopSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.ReadExistingJobStopDescriptor(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt, err := session.Stop(context.Background(), downloader.ExistingJobMutationRequest{JobKey: opaque})
	if err == nil || receipt.Complete || receipt.RequestsAttempted != 1 || receipt.AutomaticRetries != 0 || receipt.RedirectsFollowed != 0 ||
		stop.Load() != 1 || redirected.Load() != 0 || session.RequestsMade() != 3 || strings.Contains(err.Error(), opaque) || strings.Contains(err.Error(), responseCanary) {
		t.Fatalf("receipt=%#v stop=%d redirected=%d requests=%d err=%v", receipt, stop.Load(), redirected.Load(), session.RequestsMade(), err)
	}
}
