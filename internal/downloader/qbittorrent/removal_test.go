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

func TestExistingJobRemovalKeepsDataForBothSupportedVersions(t *testing.T) {
	for _, test := range []struct {
		version  string
		protocol string
	}{
		{version: "v4.6.7", protocol: downloader.ControlProtocolQBittorrentV4},
		{version: "v5.0.4", protocol: downloader.ControlProtocolQBittorrentV5},
	} {
		t.Run(test.version, func(t *testing.T) {
			const opaque = "opaque&a=b?#%"
			var requests, removals atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				switch request.URL.Path {
				case "/api/v2/auth/login":
					http.SetCookie(writer, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/"})
					_, _ = writer.Write([]byte("Ok."))
				case "/api/v2/app/version":
					_, _ = writer.Write([]byte(test.version))
				case "/api/v2/torrents/delete":
					removals.Add(1)
					if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
						t.Errorf("unexpected request: %s %s", request.Method, request.Header.Get("Content-Type"))
					}
					if err := request.ParseForm(); err != nil || len(request.PostForm) != 2 || len(request.PostForm["hashes"]) != 1 ||
						request.PostForm.Get("hashes") != opaque || request.PostForm.Get("deleteFiles") != "false" {
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
			session, err := adapter.OpenExistingJobRemovalSession(context.Background(), credential)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			descriptor, err := session.ReadExistingJobRemovalDescriptor(context.Background())
			if err != nil || descriptor.Protocol != test.protocol {
				t.Fatalf("descriptor=%#v err=%v", descriptor, err)
			}
			receipt, err := session.RemoveKeepData(context.Background(), downloader.ExistingJobMutationRequest{JobKey: opaque})
			if err != nil || !receipt.Complete || receipt.Effect != downloader.RemoveEffectKeepData || receipt.RequestsAttempted != 1 ||
				receipt.AutomaticRetries != 0 || receipt.RedirectsFollowed != 0 || !receipt.RequestBytesKnown ||
				requests.Load() != 3 || removals.Load() != 1 || session.RequestsMade() != 3 {
				t.Fatalf("receipt=%#v requests=%d removals=%d session=%d err=%v", receipt, requests.Load(), removals.Load(), session.RequestsMade(), err)
			}
			second, secondErr := session.RemoveKeepData(context.Background(), downloader.ExistingJobMutationRequest{JobKey: opaque})
			if secondErr == nil || second.RequestsAttempted != 0 || removals.Load() != 1 || session.RequestsMade() != 3 {
				t.Fatalf("second=%#v requests=%d err=%v", second, session.RequestsMade(), secondErr)
			}
		})
	}
}

func TestExistingJobRemovalDoesNotRedirectRetryOrLeakSelector(t *testing.T) {
	const opaque = "CANARY-OPAQUE-JOB&a=b"
	const responseCanary = "CANARY-LOCATION"
	var removal, redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(writer, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/"})
			_, _ = writer.Write([]byte("Ok."))
		case "/api/v2/app/version":
			_, _ = writer.Write([]byte("v5.0.0"))
		case "/api/v2/torrents/delete":
			removal.Add(1)
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
	session, err := adapter.OpenExistingJobRemovalSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	descriptor, err := session.ReadExistingJobRemovalDescriptor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := session.RemoveKeepData(context.Background(), downloader.ExistingJobMutationRequest{JobKey: opaque})
	expected, marshalErr := downloader.MarshalExistingJobRemovalRequest(descriptor, opaque, 0)
	if err == nil || receipt.Complete || receipt.RequestsAttempted != 1 || removal.Load() != 1 || redirected.Load() != 0 ||
		!receipt.RequestBytesKnown || receipt.RequestBytes != int64(len(expected)) || marshalErr != nil ||
		session.RequestsMade() != 3 || strings.Contains(err.Error(), opaque) || strings.Contains(err.Error(), responseCanary) {
		t.Fatalf("receipt=%#v removal=%d redirected=%d requests=%d err=%v", receipt, removal.Load(), redirected.Load(), session.RequestsMade(), err)
	}
}

func TestExistingJobRemovalRejectsCancelledOrBroadSelectorBeforeMutation(t *testing.T) {
	var removals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(writer, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/"})
			_, _ = writer.Write([]byte("Ok."))
		case "/api/v2/app/version":
			_, _ = writer.Write([]byte("v5.0.0"))
		default:
			removals.Add(1)
		}
	}))
	defer server.Close()
	adapter, _ := New(server.URL)
	credential, _ := downloader.NewCredential("alice", "secret")
	session, _ := adapter.OpenExistingJobRemovalSession(context.Background(), credential)
	defer session.Close()
	_, _ = session.ReadExistingJobRemovalDescriptor(context.Background())
	receipt, err := session.RemoveKeepData(context.Background(), downloader.ExistingJobMutationRequest{JobKey: "all"})
	if err == nil || receipt.RequestsAttempted != 0 || removals.Load() != 0 {
		t.Fatalf("broad receipt=%#v removals=%d err=%v", receipt, removals.Load(), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	receipt, err = session.RemoveKeepData(ctx, downloader.ExistingJobMutationRequest{JobKey: "opaque"})
	if !errors.Is(err, context.Canceled) || receipt.RequestsAttempted != 0 || removals.Load() != 0 {
		t.Fatalf("cancelled receipt=%#v removals=%d err=%v", receipt, removals.Load(), err)
	}
}
