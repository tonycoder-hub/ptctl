package mteam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

// All fixtures are synthetic. They mirror the documented response shape and
// never come from a real account.
const testAPIKey = "MTEAM-KEY-CANARY-0123456789"

type fakeCall struct {
	path, contentType, accept, body string
	maxRequest, maxBody, maxHeader  int64
}

type fakeClient struct {
	response httpguard.StrictResponse
	err      error
	calls    []fakeCall
	closed   bool
}

func (f *fakeClient) PostReadOnce(_ context.Context, path, contentType string, body []byte, accept string, maxRequest, maxBody, maxHeader int64) (httpguard.StrictResponse, error) {
	f.calls = append(f.calls, fakeCall{path, contentType, accept, string(body), maxRequest, maxBody, maxHeader})
	return f.response, f.err
}

func (f *fakeClient) RequestsMade() int { return len(f.calls) }

func (f *fakeClient) Close() error {
	f.closed = true
	return nil
}

type factoryRecord struct {
	baseURL, apiKey string
	interval        time.Duration
	opened          int
}

func fakeAdapter(client *fakeClient) (*Adapter, *factoryRecord) {
	record := &factoryRecord{}
	return &Adapter{newClient: func(baseURL, apiKey string, interval time.Duration) (guardedClient, error) {
		record.baseURL, record.apiKey, record.interval = baseURL, apiKey, interval
		record.opened++
		return client, nil
	}}, record
}

func jsonResponse(body string) httpguard.StrictResponse {
	return httpguard.StrictResponse{StatusCode: http.StatusOK, MediaType: "application/json", Body: []byte(body), ResponseBytesKnown: true}
}

func apiKey(t *testing.T) site.Credential {
	t.Helper()
	credential, err := site.NewAPIKeyCredential(testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

const profileFixture = `{"code":"0","message":"SUCCESS","data":{
	"id":"100001","username":"canary_user","role":"1",
	"memberCount":{"uploaded":"1099511627776","downloaded":"536870912000","shareRate":"2.048","bonus":"12345.6"}}}`

func TestIdentityIsPinned(t *testing.T) {
	if SiteID != "mteam" || Origin != "https://api2.m-team.cc" || DefaultBaseURL != "https://api2.m-team.cc/" {
		t.Fatalf("site identity drifted: %q %q %q", SiteID, Origin, DefaultBaseURL)
	}
	want := domain.SiteDescriptor{
		ID: "mteam", Name: "M-Team / 馒头PT", BaseURL: "https://api2.m-team.cc/", Stability: "experimental",
		AuthMethods: []domain.AuthMethod{domain.AuthMethodAPIKeyHeader},
		Capabilities: []domain.Capability{
			domain.CapabilityAuthCheck, domain.CapabilityAccountRead, domain.CapabilitySearch, domain.CapabilityBonusRead,
			domain.CapabilityDetail, domain.CapabilityMetafile, domain.CapabilityBonusReview, domain.CapabilityBonusExchange,
		},
	}
	if got := New().Descriptor(); !reflect.DeepEqual(got, want) {
		t.Fatalf("descriptor=%#v", got)
	}
}

func TestAccountSendsOneProfileReadAndMapsCounters(t *testing.T) {
	client := &fakeClient{response: jsonResponse(profileFixture)}
	adapter, record := fakeAdapter(client)
	account, err := adapter.Account(context.Background(), apiKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if record.opened != 1 || record.baseURL != DefaultBaseURL || record.apiKey != testAPIKey || record.interval != 3*time.Second || !client.closed {
		t.Fatalf("factory=%#v closed=%v", record, client.closed)
	}
	want := fakeCall{
		path: "api/member/profile", contentType: "multipart/form-data; boundary=ptctlMTeamFormBoundary",
		accept: "application/json, text/plain, */*", body: "--ptctlMTeamFormBoundary--\r\n",
		maxRequest: 16 << 10, maxBody: 4 << 20, maxHeader: 32 << 10,
	}
	if len(client.calls) != 1 || client.calls[0] != want {
		t.Fatalf("calls=%#v", client.calls)
	}
	if account.SiteID != "mteam" || account.Username != "canary_user" || account.UploadedBytes == nil || *account.UploadedBytes != 1099511627776 ||
		account.DownloadedBytes == nil || *account.DownloadedBytes != 536870912000 || account.Ratio != "2.048" || account.Bonus != "12345.6" ||
		account.Seeding != nil || account.Leeching != nil || account.ObservedAt.IsZero() {
		t.Fatalf("account=%#v", account)
	}
}

func TestAccountDropsMalformedCountersButRequiresUsername(t *testing.T) {
	client := &fakeClient{response: jsonResponse(`{"code":0,"data":{"username":"canary_user","memberCount":{"uploaded":"-1","downloaded":12,"shareRate":"Infinity","bonus":"1e9"}}}`)}
	adapter, _ := fakeAdapter(client)
	account, err := adapter.Account(context.Background(), apiKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if account.UploadedBytes != nil || account.DownloadedBytes == nil || *account.DownloadedBytes != 12 || account.Ratio != "" || account.Bonus != "" {
		t.Fatalf("account=%#v", account)
	}
	for _, body := range []string{
		`{"code":"0","data":{"memberCount":{}}}`,
		`{"code":"0","data":null}`,
		`{"code":"0","data":{"username":"bad\u0007name"}}`,
		`{"code":"0","data":{"username":"canary_user","memberCount":{"uploaded":{"nested":true}}}}`,
	} {
		adapter, _ := fakeAdapter(&fakeClient{response: jsonResponse(body)})
		if _, err := adapter.Account(context.Background(), apiKey(t)); err == nil {
			t.Fatalf("body=%s unexpectedly produced an account", body)
		}
	}
}

func TestCheckSessionStates(t *testing.T) {
	tests := []struct {
		name     string
		response httpguard.StrictResponse
		state    domain.AuthenticationState
		username string
	}{
		{name: "authenticated", response: jsonResponse(profileFixture), state: domain.AuthenticationAuthenticated, username: "canary_user"},
		{name: "code 401", response: jsonResponse(`{"code":"401","message":"Full authentication is required to access this resource","data":null}`), state: domain.AuthenticationUnauthenticated},
		{name: "HTTP 401", response: httpguard.StrictResponse{StatusCode: http.StatusUnauthorized, MediaType: "application/json", Body: []byte(`{}`)}, state: domain.AuthenticationUnauthenticated},
		{name: "no username", response: jsonResponse(`{"code":"0","data":{"id":"1"}}`), state: domain.AuthenticationIndeterminate},
		{name: "data not an object", response: jsonResponse(`{"code":"0","data":"CANARY"}`), state: domain.AuthenticationIndeterminate},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, _ := fakeAdapter(&fakeClient{response: test.response})
			status, err := adapter.CheckSession(context.Background(), apiKey(t))
			if err != nil || status.State != test.state || status.Username != test.username || status.ObservedAt.IsZero() {
				t.Fatalf("status=%#v err=%v", status, err)
			}
		})
	}
}

func TestFailuresAreClassifiedWithoutEchoingServerText(t *testing.T) {
	tests := []struct {
		name     string
		response httpguard.StrictResponse
		err      error
		want     error
		text     string
	}{
		{name: "rate limit code", response: jsonResponse(`{"code":"4","message":"請求過於頻繁 CANARY"}`), want: site.ErrRateLimited},
		{name: "HTTP 429", response: httpguard.StrictResponse{StatusCode: http.StatusTooManyRequests, Body: []byte("CANARY")}, want: site.ErrRateLimited},
		{name: "code 401", response: jsonResponse(`{"code":"401","message":"CANARY"}`), want: site.ErrCredentialRejected},
		{name: "other code", response: jsonResponse(`{"code":"1","message":"無權限 CANARY"}`), text: "M-Team API refused the request (code 1)"},
		{name: "unsafe code", response: jsonResponse(`{"code":"CANARY code","message":"x"}`), text: "M-Team API refused the request"},
		{name: "HTML", response: httpguard.StrictResponse{StatusCode: http.StatusOK, MediaType: "text/html", Body: []byte("<html>CANARY")}, text: "M-Team API response was not JSON"},
		{name: "not JSON", response: jsonResponse(`CANARY`), text: "M-Team API response was not recognized"},
		{name: "no code", response: jsonResponse(`{"data":{"username":"CANARY"}}`), text: "M-Team API response was not recognized"},
		{name: "redirect", response: httpguard.StrictResponse{StatusCode: http.StatusFound}, text: "M-Team API redirect was rejected"},
		{name: "HTTP 403", response: httpguard.StrictResponse{StatusCode: http.StatusForbidden, Body: []byte("CANARY")}, text: "M-Team API returned HTTP 403"},
		{name: "blocked address", err: fmt.Errorf("site request failed: %w", httpguard.ErrBlockedAddress), want: site.ErrAddressBlocked},
		{name: "transport", err: errors.New("dial CANARY"), text: "M-Team API request failed"},
		{name: "canceled mid request", err: fmt.Errorf("site request canceled: %w", context.Canceled), want: context.Canceled},
		{name: "deadline", err: fmt.Errorf("site request canceled: %w", context.DeadlineExceeded), want: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeClient{response: test.response, err: test.err}
			adapter, _ := fakeAdapter(client)
			_, err := adapter.Account(context.Background(), apiKey(t))
			if err == nil || len(client.calls) != 1 || strings.Contains(err.Error(), "CANARY") || strings.Contains(err.Error(), "頻繁") {
				t.Fatalf("err=%v calls=%d", err, len(client.calls))
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("err=%v, want %v", err, test.want)
			}
			if test.text != "" && err.Error() != test.text {
				t.Fatalf("err=%q, want %q", err, test.text)
			}
		})
	}
}

func TestRejectsBeforeAnyRequest(t *testing.T) {
	cookie, err := site.NewCookieCredential("sid=COOKIE-CANARY")
	if err != nil {
		t.Fatal(err)
	}
	spaced, err := site.NewAPIKeyCredential("KEY CANARY")
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name       string
		ctx        context.Context
		credential site.Credential
		want       error
	}{
		{name: "canceled", ctx: canceled, credential: apiKey(t), want: context.Canceled},
		{name: "cookie credential", ctx: context.Background(), credential: cookie},
		{name: "malformed key", ctx: context.Background(), credential: spaced, want: site.ErrCredentialMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeClient{response: jsonResponse(profileFixture)}
			adapter, record := fakeAdapter(client)
			for name, call := range map[string]func() error{
				"status":  func() error { _, err := adapter.CheckSession(test.ctx, test.credential); return err },
				"account": func() error { _, err := adapter.Account(test.ctx, test.credential); return err },
				"search":  func() error { _, err := adapter.Search(test.ctx, test.credential, "canary"); return err },
			} {
				err := call()
				if err == nil || strings.Contains(err.Error(), "CANARY") || (test.want != nil && !errors.Is(err, test.want)) {
					t.Fatalf("%s err=%v", name, err)
				}
			}
			if record.opened != 0 || len(client.calls) != 0 {
				t.Fatalf("opened=%d calls=%d", record.opened, len(client.calls))
			}
		})
	}
}

const searchFixture = `{"code":"0","message":"SUCCESS","data":{"pageNumber":"1","pageSize":"50","total":"3","totalPages":"1","data":[
	{"id":"900001","name":"Canary.Movie.2026.1080p","smallDescr":"synthetic","size":"8589934592","category":"401",
	 "createdDate":"2026-10-01 20:30:00",
	 "status":{"discount":"FREE","discountEndTime":"2026-10-15 08:00:00","seeders":"12","leechers":"3","timesCompleted":"45"}},
	{"id":900002,"name":"Canary.Show.S01","size":1024,"category":"","createdDate":"not a date",
	 "status":{"discount":null,"seeders":"0","leechers":"-1","timesCompleted":"x"}},
	{"id":"900003","name":"Canary.Album","size":"1.5GB","status":{"discount":"SOMETHING_NEW"}},
	{"id":"900001","name":"Canary.Duplicate","size":"1"}
]}}`

func TestSearchSendsOneJSONQueryAndMapsRows(t *testing.T) {
	client := &fakeClient{response: jsonResponse(searchFixture)}
	adapter, _ := fakeAdapter(client)
	results, err := adapter.Search(context.Background(), apiKey(t), "  canary <&> 电影  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 1 || client.calls[0].path != "api/torrent/search" || client.calls[0].contentType != "application/json" {
		t.Fatalf("calls=%#v", client.calls)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(client.calls[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sent, map[string]any{"mode": "normal", "keyword": "canary <&> 电影", "pageNumber": float64(1), "pageSize": float64(50)}) {
		t.Fatalf("search body=%s", client.calls[0].body)
	}
	i64 := func(v int64) *int64 { return &v }
	i := func(v int) *int { return &v }
	at := func(v time.Time) *time.Time { return &v }
	want := []domain.TorrentSummary{
		{
			Ref: domain.TorrentRef{SiteID: "mteam", RemoteID: "900001"}, Name: "Canary.Movie.2026.1080p", SizeBytes: i64(8589934592),
			Seeders: i(12), Leechers: i(3), Snatches: i(45), Category: "401", CategoryName: "电影/SD",
			PublishedAt: at(time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)),
			Promotion:   &domain.Promotion{UploadFactor: "1", DownloadFactor: "0", EndsAt: at(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC))},
		},
		{
			Ref: domain.TorrentRef{SiteID: "mteam", RemoteID: "900002"}, Name: "Canary.Show.S01", SizeBytes: i64(1),
			Seeders: i(0), Promotion: &domain.Promotion{UploadFactor: "1", DownloadFactor: "1"},
		},
		{Ref: domain.TorrentRef{SiteID: "mteam", RemoteID: "900003"}, Name: "Canary.Album"},
	}
	want[1].SizeBytes = i64(1024)
	if !reflect.DeepEqual(results, want) {
		got, _ := json.MarshalIndent(results, "", "  ")
		t.Fatalf("results=%s", got)
	}
}

func TestSearchFailsClosedOnUnrecognizedPages(t *testing.T) {
	for _, body := range []string{
		`{"code":"0","data":{"total":"0"}}`,
		`{"code":"0","data":null}`,
		`{"code":"0","data":[]}`,
		`{"code":"0","data":{"data":[{"id":"0","name":"x"}]}}`,
		`{"code":"0","data":{"data":[{"id":"12a","name":"x"}]}}`,
		`{"code":"0","data":{"data":[{"id":"1"}]}}`,
		`{"code":"0","data":{"data":[{"id":"1","name":"  "}]}}`,
	} {
		adapter, _ := fakeAdapter(&fakeClient{response: jsonResponse(body)})
		if results, err := adapter.Search(context.Background(), apiKey(t), "canary"); err == nil {
			t.Fatalf("body=%s results=%#v", body, results)
		}
	}
	adapter, _ := fakeAdapter(&fakeClient{response: jsonResponse(`{"code":"0","data":{"total":"0","data":[]}}`)})
	if results, err := adapter.Search(context.Background(), apiKey(t), "canary"); err != nil || results == nil || len(results) != 0 {
		t.Fatalf("empty page results=%#v err=%v", results, err)
	}
}

func TestSearchRejectsQueriesBeforeAnyRequest(t *testing.T) {
	for _, query := range []string{"", "   ", strings.Repeat("q", maxQueryBytes+1), "canary\x00", "bad\xff"} {
		client := &fakeClient{response: jsonResponse(searchFixture)}
		adapter, record := fakeAdapter(client)
		if _, err := adapter.Search(context.Background(), apiKey(t), query); err == nil || record.opened != 0 || len(client.calls) != 0 {
			t.Fatalf("query=%q err=%v opened=%d", query, err, record.opened)
		}
	}
}

func TestPromotionFactors(t *testing.T) {
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := map[string]*domain.Promotion{
		"NORMAL":         {UploadFactor: "1", DownloadFactor: "1"},
		"FREE":           {UploadFactor: "1", DownloadFactor: "0", EndsAt: &end},
		"_2X_FREE":       {UploadFactor: "2", DownloadFactor: "0", EndsAt: &end},
		"PERCENT_50":     {UploadFactor: "1", DownloadFactor: "0.5", EndsAt: &end},
		"PERCENT_70":     {UploadFactor: "1", DownloadFactor: "0.7", EndsAt: &end},
		"_2X":            {UploadFactor: "2", DownloadFactor: "1", EndsAt: &end},
		"_2X_PERCENT_50": {UploadFactor: "2", DownloadFactor: "0.5", EndsAt: &end},
		"UNKNOWN":        nil,
	}
	for discount, want := range tests {
		if got := promotion(discount, &end); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %#v want %#v", discount, got, want)
		}
	}
}

func TestGuardedClientFactoryBindsOnlyTheAPIKeyHeader(t *testing.T) {
	client, err := newGuardedClient(DefaultBaseURL, testAPIKey, minRequestInterval)
	if err != nil || client == nil {
		t.Fatalf("client=%v err=%v", client, err)
	}
	_ = client.Close()
	if client, err := newGuardedClient(DefaultBaseURL, "bad\r\nkey CANARY", minRequestInterval); err == nil || client != nil || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("client=%v err=%v", client, err)
	}
}
