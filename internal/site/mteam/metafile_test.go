package mteam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

// The signed links below are synthetic; a real one is a credential.
const (
	signedLinkFixture = "https://api2.m-team.cc/api/rss/dlv2?sign=SIGN-CANARY&t=1760000000&tid=900001&uid=100001"
	tokenFixture      = `{"code":"0","message":"SUCCESS","data":"` + signedLinkFixture + `"}`
	metafileFixture   = "d8:announce30:https://tracker.invalid/CANARY4:infod4:name6:canary7:privatei1eee"
)

type fakeLinkClient struct {
	baseURL  string
	response httpguard.StrictResponse
	next     string
	err      error
	links    []string
	approve  []bool
	closed   bool
}

func (f *fakeLinkClient) GetLinkOnce(_ context.Context, link httpguard.SecretLink, accept string, _, _ int64, approve func(*url.URL) bool) (httpguard.StrictResponse, httpguard.SecretLink, error) {
	if accept != metafileAccept {
		return httpguard.StrictResponse{}, httpguard.SecretLink{}, fmt.Errorf("unexpected accept")
	}
	f.links = append(f.links, link.URL().String())
	f.approve = append(f.approve, approve != nil)
	var next httpguard.SecretLink
	if parsed, err := httpguard.NewSecretLink(f.next); err == nil && approve != nil && approve(parsed.URL()) {
		next = parsed
	}
	return f.response, next, f.err
}

func (f *fakeLinkClient) RequestsMade() int { return len(f.links) }

func (f *fakeLinkClient) Close() error {
	f.closed = true
	return nil
}

type linkRecord struct {
	opened []string
	pauses []time.Duration
}

func observedBody(status int, mediaType, body string) httpguard.StrictResponse {
	response := observedJSON(body)
	response.StatusCode, response.MediaType = status, mediaType
	return response
}

func torrentBody() httpguard.StrictResponse {
	return observedBody(http.StatusOK, "application/x-bittorrent", metafileFixture)
}

func redirectTo() httpguard.StrictResponse {
	return observedBody(http.StatusFound, "text/html", "")
}

// openMetafile opens a session whose link clients come from links, keyed by
// base URL. A base URL with no entry fails the test when it is opened.
func openMetafile(t *testing.T, api *fakeClient, links map[string]*fakeLinkClient) (site.MetafileFetchSession, *linkRecord) {
	t.Helper()
	adapter, apiRecord := fakeAdapter(api)
	record := &linkRecord{}
	adapter.newLinkClient = func(baseURL string) (linkClient, error) {
		record.opened = append(record.opened, baseURL)
		client, ok := links[baseURL]
		if !ok {
			t.Fatalf("unexpected link host opened")
		}
		client.baseURL = baseURL
		return client, nil
	}
	adapter.pause = func(ctx context.Context, wait time.Duration) error {
		record.pauses = append(record.pauses, wait)
		return ctx.Err()
	}
	session, err := adapter.OpenMetafileFetchSession(context.Background(), apiKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if apiRecord.opened != 1 || apiRecord.baseURL != DefaultBaseURL || apiRecord.apiKey != testAPIKey || apiRecord.interval != minRequestInterval {
		t.Fatalf("api factory=%#v", apiRecord)
	}
	return session, record
}

func metafileLimits(t *testing.T) site.MetafileFetchLimits {
	t.Helper()
	config, err := New().MetafileFetchConfig()
	if err != nil {
		t.Fatal(err)
	}
	return config.DefaultLimits()
}

func assertNoSecret(t *testing.T, values ...any) {
	t.Helper()
	for _, value := range values {
		encoded, _ := json.Marshal(value)
		text := fmt.Sprintf("%v %+v %s", value, value, encoded)
		for _, secret := range []string{"CANARY", "sign=", "dlv2", "種子", "种子", "最多", "请求太多"} {
			if strings.Contains(text, secret) {
				t.Fatalf("secret or server text %q leaked: %s", secret, text)
			}
		}
	}
}

func TestMetafileConfigIsPinned(t *testing.T) {
	config, err := New().MetafileFetchConfig()
	if err != nil || config.Origin != "https://api2.m-team.cc" || config.RouteID != "mteam.download_by_token.v1" || config.LinkRequests != 2 {
		t.Fatalf("config=%#v err=%v", config, err)
	}
	if limits := config.DefaultLimits(); limits.MaxRequests != 1 || limits.MaxLinkRequests != 2 || limits.Validate() != nil {
		t.Fatalf("limits=%#v", limits)
	}
	if err := New().ValidateMetafileRef(detailRef("900001")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "0", "01", "-1", "1a", "900001 "} {
		if err := New().ValidateMetafileRef(detailRef(id)); err == nil {
			t.Fatalf("ref %q accepted", id)
		}
	}
}

func TestMetafileFetchesASignedLinkDirectly(t *testing.T) {
	api := &fakeClient{response: observedJSON(tokenFixture)}
	link := &fakeLinkClient{response: torrentBody()}
	session, record := openMetafile(t, api, map[string]*fakeLinkClient{"https://api2.m-team.cc/": link})
	fetched, receipt, err := session.FetchMetafile(context.Background(), detailRef("900001"), metafileLimits(t))
	if err != nil || fetched == nil || !receipt.Complete || receipt.StopReason != "" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if len(api.calls) != 1 || api.calls[0].path != "api/torrent/genDlToken" || api.calls[0].contentType != emptyFormMediaType ||
		api.calls[0].body != string(formBody("id", "900001")) || api.calls[0].maxBody != maxTokenBytes {
		t.Fatalf("token calls=%#v", api.calls)
	}
	if len(link.links) != 1 || link.links[0] != signedLinkFixture || !link.approve[0] {
		t.Fatalf("link was not requested exactly once as issued")
	}
	if len(record.pauses) != 1 || record.pauses[0] != minRequestInterval {
		t.Fatalf("pauses=%v", record.pauses)
	}
	want := site.MetafileFetchUsage{RequestsAttempted: 1, LinkRequestsAttempted: 1, ResponseBytesRead: int64(len(metafileFixture)), ResponseBytesKnown: true}
	if receipt.Used != want || session.RequestsMade() != 2 || !fetched.MatchesReceipt(receipt) {
		t.Fatalf("used=%+v requests=%d", receipt.Used, session.RequestsMade())
	}
	if receipt.Origin != MetafileOrigin || receipt.RouteID != MetafileRouteID || receipt.Ref != detailRef("900001") {
		t.Fatalf("receipt identity=%+v", receipt)
	}
	assertNoSecret(t, receipt, err)
	if err := session.Close(); err != nil || !api.closed || !link.closed {
		t.Fatalf("close err=%v api=%v link=%v", err, api.closed, link.closed)
	}
}

func TestMetafileFollowsOneRedirectToAnMTeamHost(t *testing.T) {
	const target = "https://cdn.m-team.cc/torrents/900001.torrent?k=CANARY"
	api := &fakeClient{response: observedJSON(tokenFixture)}
	link := &fakeLinkClient{response: redirectTo(), next: target}
	final := &fakeLinkClient{response: torrentBody(), next: "https://cdn.m-team.cc/again?k=CANARY"}
	session, record := openMetafile(t, api, map[string]*fakeLinkClient{"https://api2.m-team.cc/": link, "https://cdn.m-team.cc/": final})
	fetched, receipt, err := session.FetchMetafile(context.Background(), detailRef("900001"), metafileLimits(t))
	if err != nil || fetched == nil || !receipt.Complete {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if strings.Join(record.opened, " ") != "https://api2.m-team.cc/ https://cdn.m-team.cc/" {
		t.Fatalf("opened %d link clients", len(record.opened))
	}
	if len(final.links) != 1 || final.links[0] != target || final.approve[0] {
		t.Fatalf("final hop was not one exact GET without further redirects")
	}
	want := site.MetafileFetchUsage{RequestsAttempted: 1, LinkRequestsAttempted: 2, ResponseBytesRead: int64(len(metafileFixture)), ResponseBytesKnown: true}
	if receipt.Used != want || session.RequestsMade() != 3 || !fetched.MatchesReceipt(receipt) {
		t.Fatalf("used=%+v requests=%d", receipt.Used, session.RequestsMade())
	}
	assertNoSecret(t, receipt)
	if err := session.Close(); err != nil || !final.closed || !link.closed {
		t.Fatalf("close err=%v", err)
	}
}

func TestMetafileNeverContactsAnotherHost(t *testing.T) {
	for _, test := range []struct {
		name, next string
		final      *fakeLinkClient
		wantLinks  int
	}{
		{name: "other host", next: "https://www.google.com/?q=CANARY", wantLinks: 1},
		{name: "lookalike host", next: "https://m-team.cc.example/x?k=CANARY", wantLinks: 1},
		{name: "other port", next: "https://cdn.m-team.cc:8443/x?k=CANARY", wantLinks: 1},
		{name: "plain http", next: "http://cdn.m-team.cc/x?k=CANARY", wantLinks: 1},
		{name: "magnet", next: "magnet:?xt=urn:btih:CANARY", wantLinks: 1},
		{name: "no location", wantLinks: 1},
		{name: "second redirect", next: "https://cdn.m-team.io/x?k=CANARY", final: &fakeLinkClient{response: redirectTo(), next: "https://cdn.m-team.io/y"}, wantLinks: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			links := map[string]*fakeLinkClient{"https://api2.m-team.cc/": {response: redirectTo(), next: test.next}}
			if test.final != nil {
				links["https://cdn.m-team.io/"] = test.final
			}
			session, record := openMetafile(t, &fakeClient{response: observedJSON(tokenFixture)}, links)
			fetched, receipt, err := session.FetchMetafile(context.Background(), detailRef("900001"), metafileLimits(t))
			if err == nil || fetched != nil || receipt.Complete || receipt.StopReason != "redirect_rejected" {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			if len(record.opened) != test.wantLinks || receipt.Used.LinkRequestsAttempted != test.wantLinks || receipt.Used.RequestsAttempted != 1 || session.RequestsMade() != test.wantLinks+1 {
				t.Fatalf("opened=%d used=%+v", len(record.opened), receipt.Used)
			}
			assertNoSecret(t, receipt, err)
		})
	}
}

func TestMetafileTokenRefusalsStopBeforeTheLink(t *testing.T) {
	link := func(raw string) httpguard.StrictResponse {
		return observedJSON(`{"code":"0","message":"SUCCESS","data":"` + raw + `"}`)
	}
	for _, test := range []struct {
		name     string
		response httpguard.StrictResponse
		stop     string
		cause    error
	}{
		{name: "not found", response: observedJSON(`{"code":"1","message":"種子未找到","data":null}`), stop: "not_found"},
		{name: "not found simplified", response: observedJSON(`{"code":1,"message":"种子未找到"}`), stop: "not_found"},
		{name: "daily limit", response: observedJSON(`{"code":"1","message":"相同種子當天最多下載10次","data":null}`), stop: "rate_limited", cause: site.ErrRateLimited},
		{name: "burst", response: observedJSON(`{"code":"1","message":"当前请求太多，请10秒后重试","data":null}`), stop: "rate_limited", cause: site.ErrRateLimited},
		{name: "other refusal", response: observedJSON(`{"code":"1","message":"CANARY refusal","data":null}`), stop: "unrecognized_response"},
		{name: "api code 401", response: observedJSON(`{"code":"401","message":"CANARY","data":null}`), stop: "authentication_required", cause: site.ErrCredentialRejected},
		{name: "http 401", response: observedBody(http.StatusUnauthorized, "application/json", `{}`), stop: "authentication_required", cause: site.ErrCredentialRejected},
		{name: "api code 4", response: observedJSON(`{"code":"4","message":"CANARY","data":null}`), stop: "rate_limited", cause: site.ErrRateLimited},
		{name: "http 429", response: observedBody(http.StatusTooManyRequests, "application/json", `{}`), stop: "rate_limited", cause: site.ErrRateLimited},
		{name: "redirect", response: observedBody(http.StatusFound, "text/html", ""), stop: "redirect_rejected"},
		{name: "server error", response: observedBody(http.StatusInternalServerError, "text/html", "CANARY"), stop: "http_status_rejected"},
		{name: "html", response: observedBody(http.StatusOK, "text/html", "<html>CANARY</html>"), stop: "content_type_rejected"},
		{name: "not json", response: observedJSON("CANARY"), stop: "unrecognized_response"},
		{name: "object data", response: observedJSON(`{"code":"0","data":{"url":"` + signedLinkFixture + `"}}`), stop: "unrecognized_response"},
		{name: "other host", response: link("https://api.example.invalid/api/rss/dlv2?sign=SIGN-CANARY&tid=900001"), stop: "unrecognized_response"},
		{name: "plain http", response: link("http://api.m-team.cc/api/rss/dlv2?sign=SIGN-CANARY&tid=900001"), stop: "unrecognized_response"},
		{name: "port", response: link("https://api.m-team.cc:8443/api/rss/dlv2?sign=SIGN-CANARY&tid=900001"), stop: "unrecognized_response"},
		{name: "other path", response: link("https://api.m-team.cc/api/rss/other?sign=SIGN-CANARY&tid=900001"), stop: "unrecognized_response"},
		{name: "other torrent", response: link("https://api.m-team.cc/api/rss/dlv2?sign=SIGN-CANARY&tid=900002"), stop: "unrecognized_response"},
		{name: "two torrents", response: link("https://api.m-team.cc/api/rss/dlv2?sign=SIGN-CANARY&tid=900001&tid=900002"), stop: "unrecognized_response"},
		{name: "unsigned", response: link("https://api.m-team.cc/api/rss/dlv2?tid=900001"), stop: "unrecognized_response"},
		{name: "user info", response: link("https://u:CANARY@api.m-team.cc/api/rss/dlv2?sign=SIGN-CANARY&tid=900001"), stop: "unrecognized_response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, record := openMetafile(t, &fakeClient{response: test.response}, nil)
			fetched, receipt, err := session.FetchMetafile(context.Background(), detailRef("900001"), metafileLimits(t))
			if err == nil || fetched != nil || receipt.StopReason != test.stop || (test.cause != nil && !errors.Is(err, test.cause)) {
				t.Fatalf("stop=%q err=%v", receipt.StopReason, err)
			}
			if len(record.opened) != 0 || len(record.pauses) != 0 || receipt.Used.LinkRequestsAttempted != 1 || receipt.Used.RequestsAttempted != 0 || session.RequestsMade() != 1 {
				t.Fatalf("opened=%d pauses=%d used=%+v", len(record.opened), len(record.pauses), receipt.Used)
			}
			assertNoSecret(t, receipt, err)
		})
	}
}

func TestMetafileLinkRefusals(t *testing.T) {
	for _, test := range []struct {
		name     string
		response httpguard.StrictResponse
		err      error
		stop     string
	}{
		// The token request signs a link for any ID; the link answers for an
		// unknown torrent with this exact shape, observed live on 2026-10-16.
		{name: "not found", response: observedJSON(`{"code":"1","message":"種子未找到","data":null}`), stop: "not_found"},
		{name: "not found simplified", response: observedJSON(`{"code":"1","message":"种子未找到"}`), stop: "not_found"},
		{name: "not found with data", response: observedJSON(`{"code":"1","message":"種子未找到","data":{"x":"CANARY"}}`), stop: "unrecognized_response"},
		{name: "daily limit", response: observedJSON(`{"code":"1","message":"相同種子當天最多下載10次","data":null}`), stop: "rate_limited"},
		{name: "burst", response: observedJSON(`{"code":1,"message":"当前请求太多，请10秒后重试"}`), stop: "rate_limited"},
		{name: "other json", response: observedJSON(`{"code":"1","message":"CANARY","data":null}`), stop: "unrecognized_response"},
		{name: "html", response: observedBody(http.StatusOK, "text/html", "<html>CANARY</html>"), stop: "unrecognized_response"},
		{name: "bencoded as html", response: observedBody(http.StatusOK, "text/html", metafileFixture), stop: "content_type_rejected"},
		{name: "empty", response: observedBody(http.StatusOK, "application/x-bittorrent", ""), stop: "empty_response"},
		{name: "forbidden", response: observedBody(http.StatusForbidden, "text/html", ""), stop: "authentication_required"},
		{name: "too many", response: observedBody(http.StatusTooManyRequests, "text/html", ""), stop: "rate_limited"},
		{name: "server error", response: observedBody(http.StatusBadGateway, "text/html", ""), stop: "http_status_rejected"},
		{name: "request failed", err: fmt.Errorf("site request failed"), stop: "site_request_failed"},
		{name: "blocked address", err: fmt.Errorf("wrapped: %w", httpguard.ErrBlockedAddress), stop: "site_request_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			link := &fakeLinkClient{response: test.response, err: test.err}
			session, _ := openMetafile(t, &fakeClient{response: observedJSON(tokenFixture)}, map[string]*fakeLinkClient{"https://api2.m-team.cc/": link})
			fetched, receipt, err := session.FetchMetafile(context.Background(), detailRef("900001"), metafileLimits(t))
			if err == nil || fetched != nil || receipt.Complete || receipt.StopReason != test.stop {
				t.Fatalf("stop=%q err=%v", receipt.StopReason, err)
			}
			if receipt.Used.LinkRequestsAttempted != 1 || receipt.Used.RequestsAttempted != 1 || session.RequestsMade() != 2 {
				t.Fatalf("used=%+v requests=%d", receipt.Used, session.RequestsMade())
			}
			assertNoSecret(t, receipt, err)
		})
	}
}

func TestMetafileSessionBudgetsAndCredential(t *testing.T) {
	api := &fakeClient{response: observedJSON(tokenFixture)}
	session, _ := openMetafile(t, api, map[string]*fakeLinkClient{"https://api2.m-team.cc/": {response: torrentBody()}})
	direct := site.DefaultMetafileFetchLimits()
	if _, receipt, err := session.FetchMetafile(context.Background(), detailRef("900001"), direct); err == nil || receipt.StopReason != "invalid_limits" || session.RequestsMade() != 0 {
		t.Fatalf("direct-route limits accepted: stop=%q", receipt.StopReason)
	}
	if _, _, err := session.FetchMetafile(context.Background(), detailRef("900001"), metafileLimits(t)); err != nil {
		t.Fatal(err)
	}
	_, receipt, err := session.FetchMetafile(context.Background(), detailRef("900001"), metafileLimits(t))
	if err == nil || receipt.StopReason != "request_budget_exhausted" || receipt.Used.RequestsAttempted != 1 || receipt.Used.LinkRequestsAttempted != 1 || len(api.calls) != 1 {
		t.Fatalf("second fetch: stop=%q used=%+v", receipt.StopReason, receipt.Used)
	}

	canceled, record := openMetafile(t, &fakeClient{response: observedJSON(tokenFixture)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	adapterPause := func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	canceled.(*metafileFetchSession).pause = adapterPause
	if _, receipt, err := canceled.FetchMetafile(ctx, detailRef("900001"), metafileLimits(t)); !errors.Is(err, context.Canceled) || receipt.StopReason != "context_done" || len(record.opened) != 0 {
		t.Fatalf("canceled pause: stop=%q err=%v", receipt.StopReason, err)
	}

	cookie, err := site.NewCookieCredential("c_secure_pass=CANARY")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New().OpenMetafileFetchSession(context.Background(), cookie); err == nil {
		t.Fatal("cookie credential accepted")
	}
	malformed, err := site.NewAPIKeyCredential("MTEAM KEY CANARY")
	if err == nil {
		if _, err := New().OpenMetafileFetchSession(context.Background(), malformed); !errors.Is(err, site.ErrCredentialMalformed) {
			t.Fatalf("malformed key: %v", err)
		}
	}
}

func TestSiteHostAllowsOnlyMTeamDomains(t *testing.T) {
	for host, want := range map[string]bool{
		"m-team.cc": true, "api.m-team.cc": true, "api2.m-team.cc": true, "cdn.m-team.io": true,
		"API.m-team.cc": false, "m-team.cc.": false, "api.m-team.cc:443": false, "evil-m-team.cc": false,
		"m-team.cc.example": false, "m-team.com": false, "": false,
	} {
		if got := siteHost(host); got != want {
			t.Fatalf("siteHost(%q)=%v", host, got)
		}
	}
	if validSignedLink(nil, domain.TorrentRef{}) || approvedRedirect(nil) {
		t.Fatal("nil URL accepted")
	}
}
