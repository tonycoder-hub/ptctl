package tjupt

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

type accountFakeClient struct {
	body       []byte
	finalURL   *url.URL
	err        error
	requests   int
	closeCalls int
	path       string
	query      url.Values
}

func (client *accountFakeClient) Get(_ context.Context, path string, query url.Values) ([]byte, *url.URL, error) {
	client.requests++
	client.path = path
	client.query = query
	return append([]byte(nil), client.body...), client.finalURL, client.err
}

func (*accountFakeClient) GetOnce(context.Context, string, url.Values, string, int64, int64) (httpguard.StrictResponse, error) {
	panic("unexpected strict GET")
}

func (client *accountFakeClient) RequestsMade() int { return client.requests }

func (client *accountFakeClient) Close() error {
	client.closeCalls++
	return nil
}

func TestAccountCapabilityAndPartialSnapshot(t *testing.T) {
	const cookie = "sid=COOKIE-CANARY"
	pageURL, err := url.Parse("https://www.tjupt.org/mybonusapps.php")
	if err != nil {
		t.Fatal(err)
	}
	client := &accountFakeClient{body: accountPage("Alice", "12,345.67"), finalURL: pageURL}
	adapter := New("")
	if !adapter.Descriptor().Supports(domain.CapabilityAccountRead) {
		t.Fatal("TJUPT adapter did not declare account.read")
	}
	if _, ok := any(adapter).(site.AccountReader); !ok {
		t.Fatal("TJUPT adapter did not implement AccountReader")
	}
	adapter.newClient = func(baseURL, gotCookie string, interval time.Duration) (guardedClient, error) {
		if baseURL != DefaultBaseURL || gotCookie != cookie || interval != 2*time.Second {
			t.Fatalf("factory base=%q cookie=%q interval=%s", baseURL, gotCookie, interval)
		}
		return client, nil
	}
	credential, err := site.NewCookieCredential(cookie)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	snapshot, err := adapter.Account(context.Background(), credential)
	after := time.Now().UTC()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SiteID != "tjupt" || snapshot.Username != "Alice" || snapshot.Bonus != "12345.67" ||
		snapshot.UploadedBytes != nil || snapshot.DownloadedBytes != nil || snapshot.Ratio != "" || snapshot.Seeding != nil || snapshot.Leeching != nil ||
		snapshot.ObservedAt.Before(before) || snapshot.ObservedAt.After(after) || snapshot.ObservedAt.Location() != time.UTC {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	if client.requests != 1 || client.closeCalls != 1 || client.path != accountPath || len(client.query) != 0 {
		t.Fatalf("client=%#v", client)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), cookie) || strings.Contains(string(raw), "mybonusapps.php") {
		t.Fatalf("snapshot disclosed request authority: %s", raw)
	}
}

func TestAccountFailsClosedOnUnrecognizedOrUnsafePages(t *testing.T) {
	accountURL, _ := url.Parse("https://www.tjupt.org/mybonusapps.php")
	wrongURL, _ := url.Parse("https://www.tjupt.org/index.php")
	wrongOrigin, _ := url.Parse("https://example.invalid/mybonusapps.php")
	queryURL, _ := url.Parse("https://www.tjupt.org/mybonusapps.php?unexpected=1")
	loginURL, _ := url.Parse("https://www.tjupt.org/login.php")
	tests := []struct {
		name string
		body []byte
		url  *url.URL
	}{
		{name: "invalid UTF-8", body: append(accountPage("Alice", "1"), 0xff), url: accountURL},
		{name: "wrong route", body: accountPage("Alice", "1"), url: wrongURL},
		{name: "wrong origin", body: accountPage("Alice", "1"), url: wrongOrigin},
		{name: "unexpected query", body: accountPage("Alice", "1"), url: queryURL},
		{name: "missing username", body: []byte(`<div>&#24403;&#21069;&#39764;&#21147;&#20540;: 1</div>`), url: accountURL},
		{name: "missing balance", body: []byte(`<title>PT :: Alice&#30340;&#39764;&#21147;&#20540;</title>`), url: accountURL},
		{name: "malformed thousands", body: accountPage("Alice", "12,34"), url: accountURL},
		{name: "trailing decimal", body: accountPage("Alice", "12.34.56"), url: accountURL},
		{name: "oversized username", body: accountPage(strings.Repeat("A", 257), "1"), url: accountURL},
		{name: "login", body: []byte(`<input name="username"><input name="password">`), url: loginURL},
		{name: "challenge", body: []byte(`Just a moment <div id="cf-chl-widget"></div>`), url: accountURL},
	}
	credential, err := site.NewCookieCredential("sid=secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &accountFakeClient{body: test.body, finalURL: test.url}
			adapter := New("")
			adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
			snapshot, err := adapter.Account(context.Background(), credential)
			if err == nil || snapshot != (domain.AccountSnapshot{}) || client.requests != 1 || client.closeCalls != 1 {
				t.Fatalf("snapshot=%#v err=%v client=%#v", snapshot, err, client)
			}
		})
	}
}

func TestAccountPreflightAndErrorsDoNotLeakCredential(t *testing.T) {
	adapter := New("")
	factoryCalls := 0
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) {
		factoryCalls++
		return &accountFakeClient{err: errors.New("COOKIE-CANARY RESPONSE-CANARY")}, nil
	}
	credential, err := site.NewCookieCredential("sid=COOKIE-CANARY")
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.Account(canceled, credential); !errors.Is(err, context.Canceled) || factoryCalls != 0 {
		t.Fatalf("pre-canceled err=%v factoryCalls=%d", err, factoryCalls)
	}
	if _, err := adapter.Account(context.Background(), credential); err == nil || strings.Contains(err.Error(), "CANARY") || factoryCalls != 1 {
		t.Fatalf("request err=%v factoryCalls=%d", err, factoryCalls)
	}
	other, err := site.NewCredential(domain.AuthMethod("other"), "COOKIE-CANARY")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Account(context.Background(), other); err == nil || strings.Contains(err.Error(), "CANARY") || factoryCalls != 1 {
		t.Fatalf("auth method err=%v factoryCalls=%d", err, factoryCalls)
	}
}

func TestLoginRedirectIsAnAuthenticationFailure(t *testing.T) {
	base, err := url.Parse(DefaultBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	// TJUPT's loggedinorreturn answers an expired cookie with a relative
	// "Location: login.php?returnto=<page>"; NexusPHP forks may send it absolute.
	for _, raw := range []string{"login.php?returnto=mybonusapps.php", "https://www.tjupt.org/login.php?returnto=torrents.php%3Fsearch%3Dx", "/LOGIN.PHP"} {
		reference, _ := url.Parse(raw)
		if id, ok := classifyLoginRedirect(base.ResolveReference(reference)); !ok || id != loginRedirectID {
			t.Fatalf("%s: id=%q ok=%v", raw, id, ok)
		}
	}
	for _, raw := range []string{"mybonusapps.php?do=upload", "/login.php.bak", "/takelogin.php", "/login.php/extra"} {
		reference, _ := url.Parse(raw)
		if id, ok := classifyLoginRedirect(base.ResolveReference(reference)); ok || id != "" {
			t.Fatalf("%s: id=%q ok=%v", raw, id, ok)
		}
	}
	if id, ok := classifyLoginRedirect(nil); ok || id != "" {
		t.Fatalf("nil: id=%q ok=%v", id, ok)
	}

	production, err := newGuardedClient(DefaultBaseURL, "sid=secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := production.(loginRedirectClient); !ok {
		t.Fatalf("production client %T does not classify login redirects", production)
	}
	if _, ok := production.(guardedExchangeClient); !ok {
		t.Fatalf("production client %T lost the exchange port", production)
	}
	_ = production.Close()

	credential, err := site.NewCookieCredential("sid=secret")
	if err != nil {
		t.Fatal(err)
	}
	withErr := func(err error) (*Adapter, *accountFakeClient) {
		client := &accountFakeClient{err: err}
		adapter := New("")
		adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
		return adapter, client
	}
	adapter, client := withErr(&httpguard.RedirectError{ID: loginRedirectID})
	status, err := adapter.CheckSession(context.Background(), credential)
	if err != nil || status.State != domain.AuthenticationUnauthenticated || status.Username != "" || status.ObservedAt.IsZero() || client.requests != 1 || client.closeCalls != 1 {
		t.Fatalf("check status=%#v err=%v client=%#v", status, err, client)
	}
	adapter, client = withErr(&httpguard.RedirectError{ID: loginRedirectID})
	if _, err := adapter.Search(context.Background(), credential, "query"); err == nil || err.Error() != "TJUPT session is not authenticated" || client.requests != 1 {
		t.Fatalf("search err=%v client=%#v", err, client)
	}
	adapter, client = withErr(&httpguard.RedirectError{ID: loginRedirectID})
	if _, err := adapter.BonusCatalog(context.Background(), credential); err == nil || err.Error() != "TJUPT session is not authenticated" || client.requests != 1 {
		t.Fatalf("catalog err=%v client=%#v", err, client)
	}
	adapter, client = withErr(&httpguard.RedirectError{ID: loginRedirectID})
	if _, err := adapter.Account(context.Background(), credential); err == nil || err.Error() != "TJUPT session is not authenticated" || client.requests != 1 {
		t.Fatalf("account err=%v client=%#v", err, client)
	}
	for _, other := range []error{&httpguard.RedirectError{ID: "tjupt.other"}, errors.New("site redirect was rejected")} {
		adapter, client = withErr(other)
		if status, err := adapter.CheckSession(context.Background(), credential); err == nil || status.State != "" || client.requests != 1 {
			t.Fatalf("other redirect status=%#v err=%v", status, err)
		}
	}
}

func accountPage(username, balance string) []byte {
	return []byte(`<html><head><title>PT :: ` + username + `&#30340;&#39764;&#21147;&#20540;</title></head><body><div>&#24403;&#21069;&#39764;&#21147;&#20540;: ` + balance + `</div></body></html>`)
}
