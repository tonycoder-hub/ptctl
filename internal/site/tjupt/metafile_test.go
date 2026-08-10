package tjupt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

type fakeGuardedClient struct {
	response   httpguard.StrictResponse
	err        error
	calls      int
	closeCalls int
	path       string
	query      url.Values
	accept     string
	maxBody    int64
	maxHeader  int64
}

func (*fakeGuardedClient) Get(context.Context, string, url.Values) ([]byte, *url.URL, error) {
	panic("unexpected generic GET")
}

func (client *fakeGuardedClient) GetOnce(_ context.Context, path string, query url.Values, accept string, maxBody, maxHeader int64) (httpguard.StrictResponse, error) {
	client.calls++
	client.path = path
	client.query = query
	client.accept = accept
	client.maxBody = maxBody
	client.maxHeader = maxHeader
	return client.response, client.err
}

func (client *fakeGuardedClient) RequestsMade() int { return client.calls }
func (client *fakeGuardedClient) Close() error {
	client.closeCalls++
	return nil
}

func TestMetafileCapabilityOnlyForTrustedDefaultOrigin(t *testing.T) {
	trusted := New("")
	if DefaultBaseURL != MetafileOrigin+"/" {
		t.Fatalf("trusted base/origin drift: %q vs %q", DefaultBaseURL, MetafileOrigin)
	}
	if !trusted.Descriptor().Supports(domain.CapabilityMetafile) {
		t.Fatal("trusted default origin did not declare metafile capability")
	}
	config, err := trusted.MetafileFetchConfig()
	if err != nil || config.Origin != MetafileOrigin || config.RouteID != MetafileRouteID || config.Validate() != nil {
		t.Fatalf("trusted config=%#v err=%v", config, err)
	}
	custom := New("https://www.tjupt.org")
	if custom.Descriptor().Supports(domain.CapabilityMetafile) {
		t.Fatal("non-exact origin declared metafile capability")
	}
	if err := custom.ValidateMetafileRef(domain.TorrentRef{SiteID: "tjupt", RemoteID: "1"}); err == nil {
		t.Fatal("credential-free config preflight accepted a custom origin")
	}
	if config, err := custom.MetafileFetchConfig(); err == nil || config != (site.MetafileFetchConfig{}) {
		t.Fatalf("custom config=%#v err=%v", config, err)
	}
}

func TestValidateMetafileRefCanonicalPositiveDecimal(t *testing.T) {
	adapter := New("")
	for _, valid := range []string{"1", "42", "18446744073709551615"} {
		if err := adapter.ValidateMetafileRef(domain.TorrentRef{SiteID: "tjupt", RemoteID: valid}); err != nil {
			t.Fatalf("valid id %q: %v", valid, err)
		}
	}
	for _, invalid := range []domain.TorrentRef{
		{},
		{SiteID: "other", RemoteID: "1"},
		{SiteID: "tjupt", RemoteID: ""},
		{SiteID: "tjupt", RemoteID: "0"},
		{SiteID: "tjupt", RemoteID: "01"},
		{SiteID: "tjupt", RemoteID: "+1"},
		{SiteID: "tjupt", RemoteID: "-1"},
		{SiteID: "tjupt", RemoteID: "1 "},
		{SiteID: "tjupt", RemoteID: "18446744073709551616"},
	} {
		if err := adapter.ValidateMetafileRef(invalid); err == nil {
			t.Fatalf("invalid ref accepted: %#v", invalid)
		}
	}
}

func TestOpenMetafileFetchSessionValidatesBeforeCreatingClient(t *testing.T) {
	validCredential, err := site.NewCookieCredential("sid=a; uid=b")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		adapter    *Adapter
		credential site.Credential
		ctx        func() context.Context
	}{
		{name: "custom origin", adapter: New("https://mirror.example/"), credential: validCredential, ctx: context.Background},
		{name: "leading OWS", adapter: New(""), credential: mustCredential(t, " sid=a"), ctx: context.Background},
		{name: "trailing OWS", adapter: New(""), credential: mustCredential(t, "sid=a "), ctx: context.Background},
		{name: "non ASCII", adapter: New(""), credential: mustCredential(t, "sid=秘密"), ctx: context.Background},
		{name: "wrong method", adapter: New(""), credential: mustMethodCredential(t, domain.AuthMethod("token"), "secret"), ctx: context.Background},
		{name: "pre canceled", adapter: New(""), credential: validCredential, ctx: canceledContext},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factoryCalls := 0
			test.adapter.newClient = func(string, string, time.Duration) (guardedClient, error) {
				factoryCalls++
				return &fakeGuardedClient{}, nil
			}
			if _, err := test.adapter.OpenMetafileFetchSession(test.ctx(), test.credential); err == nil || factoryCalls != 0 || strings.Contains(err.Error(), test.credential.SecretValue()) {
				t.Fatalf("err=%v factoryCalls=%d", err, factoryCalls)
			}
		})
	}

	adapter := New("")
	adapter.newClient = func(baseURL, cookie string, interval time.Duration) (guardedClient, error) {
		if baseURL != DefaultBaseURL || cookie != "sid=a; uid=b" || interval != 2*time.Second {
			t.Fatalf("factory args base=%q cookie=%q interval=%s", baseURL, cookie, interval)
		}
		return &fakeGuardedClient{}, nil
	}
	if session, err := adapter.OpenMetafileFetchSession(context.Background(), validCredential); err != nil {
		t.Fatal(err)
	} else {
		session.Close()
	}

	adapter = New("")
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) {
		return nil, fmt.Errorf("CANARY-FACTORY-COOKIE-LEAK")
	}
	if _, err := adapter.OpenMetafileFetchSession(context.Background(), validCredential); err == nil || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("factory error was not fixed/redacted: %v", err)
	}
}

func TestMetafileFetchHappyPathOneRequestAndOpaqueAuthority(t *testing.T) {
	const cookie = "sid=COOKIE-CANARY"
	raw := []byte("d4:infod4:name1:xee")
	start := time.Date(2026, 8, 10, 1, 2, 3, 0, time.UTC)
	client := &fakeGuardedClient{response: httpguard.StrictResponse{
		StatusCode:         http.StatusOK,
		MediaType:          "application/x-bittorrent",
		Body:               raw,
		ObservedAtStart:    start,
		ObservedAtEnd:      start.Add(time.Second),
		ResponseBytesRead:  int64(len(raw)),
		ResponseBytesKnown: true,
	}}
	adapter := New("")
	adapter.newClient = func(baseURL, gotCookie string, _ time.Duration) (guardedClient, error) {
		if baseURL != DefaultBaseURL || gotCookie != cookie {
			t.Fatal("session factory did not receive fixed origin and credential")
		}
		return client, nil
	}
	credential := mustCredential(t, cookie)
	session, err := adapter.OpenMetafileFetchSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.TorrentRef{SiteID: "tjupt", RemoteID: "42"}
	limits := site.DefaultMetafileFetchLimits()
	fetched, receipt, err := session.FetchMetafile(context.Background(), ref, limits)
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 || session.RequestsMade() != 1 || client.path != "download.php" || len(client.query) != 1 || client.query.Get("id") != "42" ||
		client.accept != metafileAccept || client.maxBody != limits.MaxResponseBytes || client.maxHeader != limits.MaxResponseHeaderBytes {
		t.Fatalf("request path=%q query=%v accept=%q budgets=%d/%d calls=%d", client.path, client.query, client.accept, client.maxBody, client.maxHeader, client.calls)
	}
	if !receipt.Complete || receipt.StopReason != "" || receipt.Ref != ref || receipt.Origin != MetafileOrigin || receipt.RouteID != MetafileRouteID ||
		receipt.Used.RequestsAttempted != 1 || receipt.Used.AutomaticRetries != 0 || receipt.Used.RedirectsFollowed != 0 ||
		!receipt.Used.ResponseBytesKnown || receipt.Used.ResponseBytesRead != int64(len(raw)) || !fetched.MatchesReceipt(receipt) {
		t.Fatalf("fetched=%v receipt=%#v", fetched, receipt)
	}
	digest := sha256.Sum256(raw)
	variantID := "sha256:" + hex.EncodeToString(digest[:])
	binding, err := fetched.BindImported(variantID, int64(len(raw)))
	if err != nil || !binding.Matches(ref, MetafileOrigin, MetafileRouteID, variantID) {
		t.Fatalf("binding=%v err=%v", binding, err)
	}
	if second, secondReceipt, secondErr := session.FetchMetafile(context.Background(), ref, limits); secondErr == nil || second != nil || client.calls != 1 || secondReceipt.StopReason != "request_budget_exhausted" {
		t.Fatalf("second=%v receipt=%#v err=%v calls=%d", second, secondReceipt, secondErr, client.calls)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil || client.closeCalls != 1 || client.calls != 1 {
		t.Fatalf("close err=%v closeCalls=%d calls=%d", err, client.closeCalls, client.calls)
	}
	encodedReceipt, _ := json.Marshal(receipt)
	encodedFetched, _ := json.Marshal(fetched)
	if strings.Contains(string(encodedReceipt), "CANARY") || strings.Contains(string(encodedReceipt), string(raw)) || strings.Contains(string(encodedFetched), string(raw)) {
		t.Fatal("serialized result disclosed credential or response")
	}
}

func TestMetafileFetchCancellationAndFailureAccounting(t *testing.T) {
	credential := mustCredential(t, "sid=secret")
	ref := domain.TorrentRef{SiteID: "tjupt", RemoteID: "1"}
	limits := site.DefaultMetafileFetchLimits()

	client := &fakeGuardedClient{response: httpguard.StrictResponse{StatusCode: http.StatusOK, Body: []byte("d1:ae"), ObservedAtStart: time.Now().UTC(), ObservedAtEnd: time.Now().UTC(), ResponseBytesRead: 5, ResponseBytesKnown: true}}
	session := openFakeSession(t, client, credential)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, receipt, err := session.FetchMetafile(ctx, ref, limits); !errors.Is(err, context.Canceled) || receipt.Used.RequestsAttempted != 0 || client.calls != 0 {
		t.Fatalf("pre-cancel receipt=%#v err=%v calls=%d", receipt, err, client.calls)
	}
	if _, _, err := session.FetchMetafile(context.Background(), ref, limits); err != nil || client.calls != 1 {
		t.Fatalf("pre-cancel consumed request budget: err=%v calls=%d", err, client.calls)
	}

	client = &fakeGuardedClient{response: httpguard.StrictResponse{ObservedAtStart: time.Now().UTC(), ObservedAtEnd: time.Now().UTC()}, err: fmt.Errorf("CANARY-CANCEL-WRAPPER: %w", context.Canceled)}
	session = openFakeSession(t, client, credential)
	if _, receipt, err := session.FetchMetafile(context.Background(), ref, limits); !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "CANARY") || receipt.Used.RequestsAttempted != 1 || client.calls != 1 {
		t.Fatalf("mid-cancel receipt=%#v err=%v calls=%d", receipt, err, client.calls)
	}

	client = &fakeGuardedClient{response: httpguard.StrictResponse{ObservedAtStart: time.Now().UTC(), ObservedAtEnd: time.Now().UTC()}, err: fmt.Errorf("CANARY-REMOTE-ERROR")}
	session = openFakeSession(t, client, credential)
	_, receipt, err := session.FetchMetafile(context.Background(), ref, limits)
	encoded, _ := json.Marshal(receipt)
	if err == nil || strings.Contains(err.Error(), "CANARY") || strings.Contains(string(encoded), "CANARY") || receipt.Used.RequestsAttempted != 1 || client.calls != 1 {
		t.Fatalf("receipt=%s err=%v calls=%d", encoded, err, client.calls)
	}
}

func TestInvalidMetafileRefDoesNotEnterReceiptOrConsumeRequest(t *testing.T) {
	client := &fakeGuardedClient{}
	session := openFakeSession(t, client, mustCredential(t, "sid=secret"))
	invalid := domain.TorrentRef{SiteID: "tjupt", RemoteID: "CANARY-SECRET-AS-ID"}
	_, receipt, err := session.FetchMetafile(context.Background(), invalid, site.DefaultMetafileFetchLimits())
	encoded, _ := json.Marshal(receipt)
	if err == nil || client.calls != 0 || receipt.Ref != (domain.TorrentRef{}) || strings.Contains(string(encoded), "CANARY") || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("receipt=%s err=%v calls=%d", encoded, err, client.calls)
	}
}

func TestMetafileResponseAccountingFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	valid := httpguard.StrictResponse{
		StatusCode: http.StatusOK, Body: []byte("d1:ae"), ObservedAtStart: now, ObservedAtEnd: now.Add(time.Second),
		ResponseBytesRead: 5, ResponseBytesKnown: true,
	}
	tests := []struct {
		name     string
		response httpguard.StrictResponse
		limits   site.MetafileFetchLimits
	}{
		{name: "unknown bytes", response: func() httpguard.StrictResponse { value := valid; value.ResponseBytesKnown = false; return value }(), limits: site.DefaultMetafileFetchLimits()},
		{name: "byte mismatch", response: func() httpguard.StrictResponse { value := valid; value.ResponseBytesRead = 4; return value }(), limits: site.DefaultMetafileFetchLimits()},
		{name: "missing time", response: func() httpguard.StrictResponse { value := valid; value.ObservedAtStart = time.Time{}; return value }(), limits: site.DefaultMetafileFetchLimits()},
		{name: "reversed time", response: func() httpguard.StrictResponse {
			value := valid
			value.ObservedAtEnd = now.Add(-time.Second)
			return value
		}(), limits: site.DefaultMetafileFetchLimits()},
		{name: "adapter exceeded limit", response: valid, limits: site.MetafileFetchLimits{MaxRequests: 1, MaxResponseBytes: 4, MaxResponseHeaderBytes: 1024}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeGuardedClient{response: test.response}
			session := openFakeSession(t, client, mustCredential(t, "sid=secret"))
			fetched, receipt, err := session.FetchMetafile(context.Background(), domain.TorrentRef{SiteID: "tjupt", RemoteID: "1"}, test.limits)
			if err == nil || fetched != nil || receipt.Complete || receipt.StopReason != "response_accounting_invalid" || client.calls != 1 {
				t.Fatalf("fetched=%v receipt=%#v err=%v calls=%d", fetched, receipt, err, client.calls)
			}
		})
	}
}

func TestClassifyMetafileResponseFailsClosed(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		mediaType string
		body      string
		wantStop  string
		wantOK    bool
	}{
		{name: "valid", status: 200, mediaType: "application/x-bittorrent", body: "d1:ae", wantOK: true},
		{name: "valid no type", status: 200, body: "d7:comment20:captcha is just datae", wantOK: true},
		{name: "login", status: 200, mediaType: "text/html", body: `<input name="username"><input name="password">`, wantStop: "authentication_required"},
		{name: "challenge", status: 200, mediaType: "text/html", body: `<div id="cf-chl-widget">CANARY</div>`, wantStop: "challenge_response"},
		{name: "redirect", status: 302, body: "CANARY", wantStop: "redirect_rejected"},
		{name: "unauthorized", status: 403, body: "CANARY", wantStop: "authentication_required"},
		{name: "rate", status: 429, body: "CANARY", wantStop: "rate_limited"},
		{name: "server", status: 500, body: "CANARY", wantStop: "http_status_rejected"},
		{name: "empty", status: 200, wantStop: "empty_response"},
		{name: "unknown", status: 200, body: "CANARY", wantStop: "unrecognized_response"},
		{name: "content type", status: 200, mediaType: "text/html", body: "d1:ae", wantStop: "content_type_rejected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stop, err := classifyMetafileResponse(test.status, test.mediaType, []byte(test.body))
			if test.wantOK {
				if err != nil || stop != "" {
					t.Fatalf("stop=%q err=%v", stop, err)
				}
				return
			}
			if err == nil || stop != test.wantStop || strings.Contains(err.Error(), "CANARY") {
				t.Fatalf("stop=%q err=%v", stop, err)
			}
		})
	}
}

func mustCredential(t *testing.T, cookie string) site.Credential {
	t.Helper()
	credential, err := site.NewCookieCredential(cookie)
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

func mustMethodCredential(t *testing.T, method domain.AuthMethod, secret string) site.Credential {
	t.Helper()
	credential, err := site.NewCredential(method, secret)
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func openFakeSession(t *testing.T, client *fakeGuardedClient, credential site.Credential) site.MetafileFetchSession {
	t.Helper()
	adapter := New("")
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
	session, err := adapter.OpenMetafileFetchSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	return session
}
