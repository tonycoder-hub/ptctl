package tjupt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

func TestTorrentDetailCapabilityAndCredentialFreePreflight(t *testing.T) {
	trusted := New("")
	if !trusted.Descriptor().Supports(domain.CapabilityDetail) {
		t.Fatal("trusted production adapter did not declare torrent detail")
	}
	config, err := trusted.TorrentDetailConfig()
	if err != nil || config.Origin != DetailOrigin || config.RouteID != DetailRouteID || config.Validate() != nil {
		t.Fatalf("config=%#v err=%v", config, err)
	}
	for _, valid := range []string{"1", "42", "18446744073709551615"} {
		if err := trusted.ValidateTorrentDetailRef(domain.TorrentRef{SiteID: "tjupt", RemoteID: valid}); err != nil {
			t.Fatalf("valid ref %q: %v", valid, err)
		}
	}
	for _, invalid := range []domain.TorrentRef{
		{},
		{SiteID: "other", RemoteID: "1"},
		{SiteID: "tjupt", RemoteID: "0"},
		{SiteID: "tjupt", RemoteID: "01"},
		{SiteID: "tjupt", RemoteID: "+1"},
		{SiteID: "tjupt", RemoteID: "18446744073709551616"},
	} {
		if err := trusted.ValidateTorrentDetailRef(invalid); err == nil {
			t.Fatalf("invalid ref accepted: %#v", invalid)
		}
	}
	custom := New("https://www.tjupt.org")
	if custom.Descriptor().Supports(domain.CapabilityDetail) {
		t.Fatal("non-exact origin declared torrent detail")
	}
	if _, err := custom.TorrentDetailConfig(); err == nil {
		t.Fatal("non-exact origin produced detail config")
	}
}

func TestTorrentDetailHappyPathUsesOneNoHitRequest(t *testing.T) {
	const cookie = "sid=COOKIE-CANARY"
	body := []byte(`<html><a href="logout.php">logout</a><h1 align="center" id="top">A Release <span>FREE</span></h1><a href="download.php?id=42">download</a><div id="peercount"><b>12 seeders</b> | <b>3 leechers</b></div></html>`)
	start := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	client := &fakeGuardedClient{response: httpguard.StrictResponse{
		StatusCode:         http.StatusOK,
		MediaType:          "text/html",
		Body:               body,
		ObservedAtStart:    start,
		ObservedAtEnd:      start.Add(time.Second),
		ResponseBytesRead:  int64(len(body)),
		ResponseBytesKnown: true,
	}}
	adapter := New("")
	adapter.newClient = func(baseURL, gotCookie string, interval time.Duration) (guardedClient, error) {
		if baseURL != DefaultBaseURL || gotCookie != cookie || interval != 2*time.Second {
			t.Fatalf("factory args base=%q cookie=%q interval=%s", baseURL, gotCookie, interval)
		}
		return client, nil
	}
	session, err := adapter.OpenTorrentDetailSession(context.Background(), mustCredential(t, cookie))
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.TorrentRef{SiteID: "tjupt", RemoteID: "42"}
	limits := site.DefaultTorrentDetailLimits()
	detail, receipt, err := session.ReadTorrentDetail(context.Background(), ref, limits)
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 || session.RequestsMade() != 1 || client.path != detailPath || len(client.query) != 1 || client.query.Get("id") != "42" || client.query.Has("hit") ||
		client.accept != detailAccept || client.maxBody != limits.MaxResponseBytes || client.maxHeader != limits.MaxResponseHeaderBytes {
		t.Fatalf("path=%q query=%v accept=%q budgets=%d/%d calls=%d", client.path, client.query, client.accept, client.maxBody, client.maxHeader, client.calls)
	}
	public := detail.PublicCopy()
	if !detail.MatchesReceipt(receipt) || !detail.Matches(ref, DetailOrigin, DetailRouteID) || public.Ref != ref || public.DisplayTitle != "A Release FREE" || public.Seeders == nil || *public.Seeders != 12 || public.Leechers == nil || *public.Leechers != 3 ||
		!public.DownloadReferenceObserved || len(public.EvidenceBasis) != 5 {
		t.Fatalf("detail=%#v", detail)
	}
	if !receipt.Complete || receipt.StopReason != "" || receipt.Ref != ref || receipt.Origin != DetailOrigin || receipt.RouteID != DetailRouteID ||
		receipt.ObservedAtStart != start || receipt.ObservedAtEnd != start.Add(time.Second) || receipt.Used.RequestsAttempted != 1 ||
		receipt.Used.AutomaticRetries != 0 || receipt.Used.RedirectsFollowed != 0 || !receipt.Used.ResponseBytesKnown || receipt.Used.ResponseBytesRead != int64(len(body)) {
		t.Fatalf("receipt=%#v", receipt)
	}
	if _, second, secondErr := session.ReadTorrentDetail(context.Background(), ref, limits); secondErr == nil || second.StopReason != "request_budget_exhausted" || client.calls != 1 {
		t.Fatalf("second=%#v err=%v calls=%d", second, secondErr, client.calls)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil || client.closeCalls != 1 {
		t.Fatalf("close err=%v calls=%d", err, client.closeCalls)
	}
	encoded, _ := json.Marshal(struct {
		Detail  domain.TorrentDetail      `json:"detail"`
		Receipt site.TorrentDetailReceipt `json:"receipt"`
	}{public, receipt})
	if strings.Contains(string(encoded), cookie) || strings.Contains(string(encoded), string(body)) {
		t.Fatal("serialized detail disclosed cookie or raw response")
	}
}

func TestTorrentDetailResponseClassificationFailsClosed(t *testing.T) {
	valid := `<a href="logout.php">logout</a><h1 id="top">Release</h1><a href="download.php?id=42">download</a>`
	tests := []struct {
		name       string
		status     int
		mediaType  string
		body       []byte
		stopReason string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, mediaType: "text/html", body: []byte(valid), stopReason: "authentication_required"},
		{name: "not found", status: http.StatusNotFound, mediaType: "text/html", body: []byte(valid), stopReason: "not_found"},
		{name: "rate limited", status: http.StatusTooManyRequests, mediaType: "text/html", body: []byte(valid), stopReason: "rate_limited"},
		{name: "redirect", status: http.StatusFound, mediaType: "text/html", body: []byte(valid), stopReason: "redirect_rejected"},
		{name: "wrong content type", status: http.StatusOK, mediaType: "application/json", body: []byte(valid), stopReason: "content_type_rejected"},
		{name: "login", status: http.StatusOK, mediaType: "text/html", body: []byte(`<form><input name="username"><input name="password"></form>`), stopReason: "authentication_required"},
		{name: "challenge", status: http.StatusOK, mediaType: "text/html", body: []byte(`Just a moment CF-CHL-123`), stopReason: "challenge_response"},
		{name: "invalid utf8", status: http.StatusOK, mediaType: "text/html", body: []byte{0xff, 0xfe}, stopReason: "invalid_text_encoding"},
		{name: "missing auth marker", status: http.StatusOK, mediaType: "text/html", body: []byte(`<h1 id="top">Release</h1><a href="download.php?id=42">download</a>`), stopReason: "unrecognized_response"},
		{name: "data attributes are not authority", status: http.StatusOK, mediaType: "text/html", body: []byte(`<a data-href="logout.php">logout</a><h1 data-id="top">Release</h1><a data-href="download.php?id=42">download</a>`), stopReason: "unrecognized_response"},
		{name: "wrong ref", status: http.StatusOK, mediaType: "text/html", body: []byte(`<a href="logout.php">logout</a><h1 id="top">Release</h1><a href="download.php?id=43">download</a>`), stopReason: "unrecognized_response"},
		{name: "duplicate ref", status: http.StatusOK, mediaType: "text/html", body: []byte(`<a href="logout.php">logout</a><h1 id="top">Release</h1><a href="download.php?id=42&amp;id=42">download</a>`), stopReason: "unrecognized_response"},
		{name: "absolute ref", status: http.StatusOK, mediaType: "text/html", body: []byte(`<a href="logout.php">logout</a><h1 id="top">Release</h1><a href="https://other.invalid/download.php?id=42">download</a>`), stopReason: "unrecognized_response"},
		{name: "missing title", status: http.StatusOK, mediaType: "text/html", body: []byte(`<a href="logout.php">logout</a><a href="download.php?id=42">download</a>`), stopReason: "unrecognized_response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			start := time.Now().UTC()
			client := &fakeGuardedClient{response: httpguard.StrictResponse{
				StatusCode: test.status, MediaType: test.mediaType, Body: test.body,
				ObservedAtStart: start, ObservedAtEnd: start, ResponseBytesRead: int64(len(test.body)), ResponseBytesKnown: true,
			}}
			adapter := New("")
			adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
			session, err := adapter.OpenTorrentDetailSession(context.Background(), mustCredential(t, "sid=secret"))
			if err != nil {
				t.Fatal(err)
			}
			detail, receipt, readErr := session.ReadTorrentDetail(context.Background(), domain.TorrentRef{SiteID: "tjupt", RemoteID: "42"}, site.DefaultTorrentDetailLimits())
			if readErr == nil || detail != nil || receipt.Complete || receipt.StopReason != test.stopReason || client.calls != 1 || strings.Contains(readErr.Error(), string(test.body)) {
				t.Fatalf("detail=%#v receipt=%#v err=%v calls=%d", detail, receipt, readErr, client.calls)
			}
		})
	}
}

func TestTorrentDetailCancellationLimitsAndErrorsAreSafe(t *testing.T) {
	adapter := New("")
	client := &fakeGuardedClient{}
	factoryCalls := 0
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) {
		factoryCalls++
		return client, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.OpenTorrentDetailSession(ctx, mustCredential(t, "sid=secret")); !errors.Is(err, context.Canceled) || factoryCalls != 0 {
		t.Fatalf("err=%v factory=%d", err, factoryCalls)
	}

	session, err := adapter.OpenTorrentDetailSession(context.Background(), mustCredential(t, "sid=secret"))
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.TorrentRef{SiteID: "tjupt", RemoteID: "42"}
	if _, receipt, err := session.ReadTorrentDetail(context.Background(), ref, site.TorrentDetailLimits{}); err == nil || receipt.StopReason != "invalid_limits" || client.calls != 0 {
		t.Fatalf("receipt=%#v err=%v calls=%d", receipt, err, client.calls)
	}
	preCanceled, stop := context.WithCancel(context.Background())
	stop()
	if _, receipt, err := session.ReadTorrentDetail(preCanceled, ref, site.DefaultTorrentDetailLimits()); !errors.Is(err, context.Canceled) || receipt.StopReason != "context_done" || client.calls != 0 {
		t.Fatalf("receipt=%#v err=%v calls=%d", receipt, err, client.calls)
	}

	adapter = New("")
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) {
		return nil, fmt.Errorf("COOKIE-FACTORY-CANARY")
	}
	if _, err := adapter.OpenTorrentDetailSession(context.Background(), mustCredential(t, "sid=secret")); err == nil || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("unsafe factory error: %v", err)
	}

	adapter = New("")
	client = &fakeGuardedClient{err: fmt.Errorf("https://www.tjupt.org/details.php?id=42 Cookie=COOKIE-CANARY")}
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
	session, err = adapter.OpenTorrentDetailSession(context.Background(), mustCredential(t, "sid=secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, receipt, err := session.ReadTorrentDetail(context.Background(), ref, site.DefaultTorrentDetailLimits()); err == nil || receipt.StopReason != "site_request_failed" || strings.Contains(err.Error(), "CANARY") || client.calls != 1 {
		t.Fatalf("receipt=%#v err=%v calls=%d", receipt, err, client.calls)
	}
}

func TestParseTorrentDetailAllowsNoDownloadLinkButRequiresExactBoundLink(t *testing.T) {
	body := []byte(`<a href="logout.php">logout</a><h1 id='top'>Release</h1><a href="report.php?torrent=42">report</a><div id="peercount-extra"><b>8 seeders</b><b>2 leechers</b></div>`)
	detail, err := parseTorrentDetail(body, domain.TorrentRef{SiteID: "tjupt", RemoteID: "42"})
	if err != nil || detail.DownloadReferenceObserved || detail.Seeders != nil || detail.Leechers != nil || len(detail.EvidenceBasis) != 4 {
		t.Fatalf("detail=%#v err=%v", detail, err)
	}
}
