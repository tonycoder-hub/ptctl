package tjupt

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

// The one-shot sessions deliberately classify failures in different orders.
// Metafile checks request accounting before the request error and reports
// cancellation as site_request_failed; detail and bonus review check the
// context first and report context_done. These tables pin each port's
// ordering so that sharing session plumbing cannot silently unify them.

type miscountingClient struct {
	*fakeGuardedClient
	reported int
}

func (client *miscountingClient) RequestsMade() int {
	if client.reported != 0 {
		return client.reported
	}
	return client.fakeGuardedClient.RequestsMade()
}

type sessionOrderingCase struct {
	name     string
	response httpguard.StrictResponse
	err      error
	reported int
	wantStop string
	wantIs   error
}

func timedResponse(status int, mediaType string, body []byte) httpguard.StrictResponse {
	start := time.Date(2026, 10, 13, 1, 2, 3, 0, time.UTC)
	return httpguard.StrictResponse{
		StatusCode: status, MediaType: mediaType, Body: body,
		ObservedAtStart: start, ObservedAtEnd: start.Add(time.Second),
		ResponseBytesRead: int64(len(body)), ResponseBytesKnown: true,
	}
}

func timingOnlyResponse() httpguard.StrictResponse {
	start := time.Date(2026, 10, 13, 1, 2, 3, 0, time.UTC)
	return httpguard.StrictResponse{ObservedAtStart: start, ObservedAtEnd: start.Add(time.Second)}
}

func sharedOrderingCases(htmlType string) []sessionOrderingCase {
	return []sessionOrderingCase{
		{name: "request error with miscounted attempts", response: timingOnlyResponse(), err: fmt.Errorf("CANARY-REMOTE"), reported: 2, wantStop: "request_accounting_invalid"},
		{name: "plain request error", response: timingOnlyResponse(), err: fmt.Errorf("CANARY-REMOTE"), wantStop: "site_request_failed"},
		{name: "success with miscounted attempts", response: timedResponse(http.StatusOK, htmlType, []byte("<html></html>")), reported: 2, wantStop: "request_accounting_invalid"},
		{name: "server error", response: timedResponse(http.StatusInternalServerError, htmlType, []byte("CANARY")), wantStop: "http_status_rejected"},
	}
}

func runOrderingCases(t *testing.T, cases []sessionOrderingCase, read func(*testing.T, guardedClient) (string, error)) {
	t.Helper()
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			client := &miscountingClient{fakeGuardedClient: &fakeGuardedClient{response: test.response, err: test.err}, reported: test.reported}
			stop, err := read(t, client)
			if err == nil || stop != test.wantStop || client.calls != 1 {
				t.Fatalf("stop=%q want=%q err=%v calls=%d", stop, test.wantStop, err, client.calls)
			}
			if test.wantIs != nil && !errors.Is(err, test.wantIs) {
				t.Fatalf("err=%v does not wrap %v", err, test.wantIs)
			}
		})
	}
}

func TestMetafileSessionStopReasonOrdering(t *testing.T) {
	cases := append(sharedOrderingCases("application/x-bittorrent"),
		sessionOrderingCase{name: "mid-request cancel", response: timingOnlyResponse(), err: fmt.Errorf("w: %w", context.Canceled), wantStop: "site_request_failed", wantIs: context.Canceled},
		sessionOrderingCase{name: "mid-request deadline", response: timingOnlyResponse(), err: fmt.Errorf("w: %w", context.DeadlineExceeded), wantStop: "site_request_failed", wantIs: context.DeadlineExceeded},
		sessionOrderingCase{name: "cancel with miscounted attempts", response: timingOnlyResponse(), err: fmt.Errorf("w: %w", context.Canceled), reported: 2, wantStop: "request_accounting_invalid"},
		sessionOrderingCase{name: "not found has no dedicated reason", response: timedResponse(http.StatusNotFound, "text/html", []byte("CANARY")), wantStop: "http_status_rejected"},
	)
	runOrderingCases(t, cases, func(t *testing.T, client guardedClient) (string, error) {
		adapter := New("")
		adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
		session, err := adapter.OpenMetafileFetchSession(context.Background(), mustCredential(t, "sid=secret"))
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		_, receipt, err := session.FetchMetafile(context.Background(), domain.TorrentRef{SiteID: "tjupt", RemoteID: "1"}, site.DefaultMetafileFetchLimits())
		return receipt.StopReason, err
	})
}

func TestTorrentDetailSessionStopReasonOrdering(t *testing.T) {
	cases := append(sharedOrderingCases("text/html"),
		sessionOrderingCase{name: "mid-request cancel", response: timingOnlyResponse(), err: fmt.Errorf("w: %w", context.Canceled), wantStop: "context_done", wantIs: context.Canceled},
		sessionOrderingCase{name: "mid-request deadline", response: timingOnlyResponse(), err: fmt.Errorf("w: %w", context.DeadlineExceeded), wantStop: "context_done", wantIs: context.DeadlineExceeded},
		sessionOrderingCase{name: "cancel with miscounted attempts", response: timingOnlyResponse(), err: fmt.Errorf("w: %w", context.Canceled), reported: 2, wantStop: "context_done", wantIs: context.Canceled},
		sessionOrderingCase{name: "not found", response: timedResponse(http.StatusNotFound, "text/html", []byte("CANARY")), wantStop: "not_found"},
	)
	runOrderingCases(t, cases, func(t *testing.T, client guardedClient) (string, error) {
		adapter := New("")
		adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
		session, err := adapter.OpenTorrentDetailSession(context.Background(), mustCredential(t, "sid=secret"))
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		_, receipt, err := session.ReadTorrentDetail(context.Background(), domain.TorrentRef{SiteID: "tjupt", RemoteID: "1"}, site.DefaultTorrentDetailLimits())
		return receipt.StopReason, err
	})
}

func TestBonusReviewSessionStopReasonOrdering(t *testing.T) {
	cases := append(sharedOrderingCases("text/html"),
		sessionOrderingCase{name: "mid-request cancel", response: timingOnlyResponse(), err: fmt.Errorf("w: %w", context.Canceled), wantStop: "context_done", wantIs: context.Canceled},
		sessionOrderingCase{name: "mid-request deadline", response: timingOnlyResponse(), err: fmt.Errorf("w: %w", context.DeadlineExceeded), wantStop: "context_done", wantIs: context.DeadlineExceeded},
		sessionOrderingCase{name: "cancel with miscounted attempts", response: timingOnlyResponse(), err: fmt.Errorf("w: %w", context.Canceled), reported: 2, wantStop: "context_done", wantIs: context.Canceled},
		sessionOrderingCase{name: "not found", response: timedResponse(http.StatusNotFound, "text/html", []byte("CANARY")), wantStop: "not_found"},
	)
	runOrderingCases(t, cases, func(t *testing.T, client guardedClient) (string, error) {
		adapter := New("")
		adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
		session, err := adapter.OpenBonusReviewSession(context.Background(), mustCredential(t, "sid=secret"))
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		_, receipt, err := session.ReadBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits())
		return receipt.StopReason, err
	})
}

func TestClosedOneShotSessionsReportSessionClosedWithoutRequest(t *testing.T) {
	ref := domain.TorrentRef{SiteID: "tjupt", RemoteID: "1"}
	reads := map[string]func(*testing.T, *Adapter) (string, error){
		"metafile": func(t *testing.T, adapter *Adapter) (string, error) {
			session, err := adapter.OpenMetafileFetchSession(context.Background(), mustCredential(t, "sid=secret"))
			if err != nil {
				t.Fatal(err)
			}
			session.Close()
			_, receipt, err := session.FetchMetafile(context.Background(), ref, site.DefaultMetafileFetchLimits())
			return receipt.StopReason, err
		},
		"detail": func(t *testing.T, adapter *Adapter) (string, error) {
			session, err := adapter.OpenTorrentDetailSession(context.Background(), mustCredential(t, "sid=secret"))
			if err != nil {
				t.Fatal(err)
			}
			session.Close()
			_, receipt, err := session.ReadTorrentDetail(context.Background(), ref, site.DefaultTorrentDetailLimits())
			return receipt.StopReason, err
		},
		"bonus review": func(t *testing.T, adapter *Adapter) (string, error) {
			session, err := adapter.OpenBonusReviewSession(context.Background(), mustCredential(t, "sid=secret"))
			if err != nil {
				t.Fatal(err)
			}
			session.Close()
			_, receipt, err := session.ReadBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits())
			return receipt.StopReason, err
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			client := &fakeGuardedClient{}
			adapter := New("")
			adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
			stop, err := read(t, adapter)
			if err == nil || stop != "session_closed" || client.calls != 0 || client.closeCalls != 1 {
				t.Fatalf("stop=%q err=%v calls=%d closeCalls=%d", stop, err, client.calls, client.closeCalls)
			}
		})
	}
}
