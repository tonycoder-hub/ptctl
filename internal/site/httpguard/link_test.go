package httpguard

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const testLink = "https://example.test/api/rss/dlv2?tid=42&sign=SIGN-CANARY%2B&uid=7"

func mustSecretLink(t *testing.T, raw string) SecretLink {
	t.Helper()
	link, err := NewSecretLink(raw)
	if err != nil {
		t.Fatal(err)
	}
	return link
}

func TestNewSecretLinkValidatesAndRedacts(t *testing.T) {
	link := mustSecretLink(t, testLink)
	if link.IsZero() || link.URL().String() != testLink {
		t.Fatalf("link was not kept exactly")
	}
	link.URL().RawQuery = "changed"
	if link.URL().RawQuery != "tid=42&sign=SIGN-CANARY%2B&uid=7" {
		t.Fatalf("URL returned shared state")
	}
	holder := struct {
		Exported SecretLink
		private  SecretLink
	}{link, link}
	for _, formatted := range []string{
		link.String(), link.GoString(), fmt.Sprint(link), fmt.Sprintf("%v %+v %#v", holder, holder, holder),
	} {
		if strings.Contains(formatted, "CANARY") {
			t.Fatalf("formatted link leaked: %q", formatted)
		}
	}
	for _, raw := range []string{
		"",
		"http://example.test/x?sign=CANARY",
		"https://user:CANARY@example.test/x",
		"https://example.test/x?sign=CANARY#fragment",
		"https://example.test/x?sign=CANARY ",
		"https://example.test/x?sign=CANARY\r\n",
		"https://example.test/x?sign=CANARY-é",
		"https://example.test\\x?sign=CANARY",
		"https://example.test/x?",
		"https:example.test/x?sign=CANARY",
		"/api/rss/dlv2?sign=CANARY",
		"magnet:?xt=urn:btih:CANARY",
		"https://example.test/x?sign=CANARY" + strings.Repeat("a", maxSecretLinkBytes),
	} {
		if link, err := NewSecretLink(raw); err == nil || !link.IsZero() || strings.Contains(err.Error(), "CANARY") {
			t.Fatalf("raw=%q accepted or leaked: %v", raw, err)
		}
	}
}

func TestNewWithoutCredentialSendsNoCredential(t *testing.T) {
	client, err := NewWithoutCredential("https://example.test/", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.credentialHeader != "" || client.credentialValue != "" {
		t.Fatalf("credential bound")
	}
	if client, err := NewWithoutCredential("http://example.test/", 0); err == nil || client != nil {
		t.Fatalf("plain HTTP accepted")
	}
}

func TestGetLinkOnceSendsExactLinkOnce(t *testing.T) {
	calls := 0
	client := strictTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodGet || request.URL.String() != testLink || request.URL.RawQuery != "tid=42&sign=SIGN-CANARY%2B&uid=7" {
			t.Fatalf("request was not the exact link")
		}
		if request.Header.Get("Cookie") != "" || request.Header.Get("X-Api-Key") != "" || request.Header.Get("User-Agent") != readOnlyUserAgent ||
			request.Header.Get("Accept") != "application/x-bittorrent" || request.Header.Get("Accept-Encoding") != "identity" || !request.Close {
			t.Fatalf("headers=%v close=%v", request.Header, request.Close)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/x-bittorrent"}}, Body: io.NopCloser(strings.NewReader("d4:infod4:name1:xee")), Request: request}, nil
	}), "")
	response, next, err := client.GetLinkOnce(context.Background(), mustSecretLink(t, testLink), "application/x-bittorrent", 1<<20, 32<<10, func(*url.URL) bool {
		t.Fatal("approve called for a non-redirect")
		return false
	})
	if err != nil || calls != 1 || client.RequestsMade() != 1 {
		t.Fatalf("calls=%d requests=%d err=%v", calls, client.RequestsMade(), err)
	}
	if !next.IsZero() || response.StatusCode != http.StatusOK || response.MediaType != "application/x-bittorrent" ||
		string(response.Body) != "d4:infod4:name1:xee" || !response.ResponseBytesKnown || response.RedirectID != "" {
		t.Fatalf("response=%+v next=%v", response, next)
	}
}

func TestGetLinkOnceReturnsOnlyAnApprovedRedirect(t *testing.T) {
	const cdn = "https://cdn.example.test/file.torrent?token=CDN-CANARY"
	for _, test := range []struct {
		name      string
		locations []string
		approve   func(*url.URL) bool
		want      string
	}{
		{name: "approved absolute", locations: []string{cdn}, approve: hostIs("cdn.example.test"), want: cdn},
		{name: "approved relative", locations: []string{"/files/1.torrent?k=CANARY"}, approve: hostIs("example.test"), want: "https://example.test/files/1.torrent?k=CANARY"},
		{name: "not approved", locations: []string{cdn}, approve: hostIs("other.test")},
		{name: "no approve function", locations: []string{cdn}},
		{name: "magnet", locations: []string{"magnet:?xt=urn:btih:CANARY"}, approve: approveAll},
		{name: "plain http", locations: []string{"http://cdn.example.test/file.torrent?token=CANARY"}, approve: approveAll},
		{name: "user info", locations: []string{"https://u:CANARY@cdn.example.test/f"}, approve: approveAll},
		{name: "fragment", locations: []string{cdn + "#CANARY"}, approve: approveAll},
		{name: "two locations", locations: []string{cdn, cdn}, approve: approveAll},
		{name: "no location", approve: approveAll},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := strictTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				header := http.Header{"Content-Type": {"text/html"}}
				for _, location := range test.locations {
					header.Add("Location", location)
				}
				return &http.Response{StatusCode: http.StatusFound, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
			}), "")
			response, next, err := client.GetLinkOnce(context.Background(), mustSecretLink(t, testLink), "application/x-bittorrent", 1<<20, 32<<10, test.approve)
			if err != nil || response.StatusCode != http.StatusFound || response.RedirectID != "" || client.RequestsMade() != 1 {
				t.Fatalf("response=%+v err=%v", response, err)
			}
			if test.want == "" {
				if !next.IsZero() {
					t.Fatalf("redirect target returned")
				}
				return
			}
			if next.IsZero() || next.URL().String() != test.want {
				t.Fatalf("redirect target was not returned exactly")
			}
		})
	}
}

func TestGetLinkOnceRefusesBeforeSending(t *testing.T) {
	calls := 0
	client := strictTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("unexpected request")
	}), "")
	for _, test := range []struct {
		name   string
		link   SecretLink
		accept string
		body   int64
	}{
		{name: "other host", link: mustSecretLink(t, "https://other.test/x?sign=CANARY"), accept: "*/*", body: 1 << 20},
		{name: "other port", link: mustSecretLink(t, "https://example.test:8443/x?sign=CANARY"), accept: "*/*", body: 1 << 20},
		{name: "zero link", accept: "*/*", body: 1 << 20},
		{name: "bad accept", link: mustSecretLink(t, testLink), accept: "a\r\nb", body: 1 << 20},
		{name: "bad budget", link: mustSecretLink(t, testLink), accept: "*/*", body: 0},
	} {
		if _, next, err := client.GetLinkOnce(context.Background(), test.link, test.accept, test.body, 32<<10, approveAll); err == nil || !next.IsZero() || strings.Contains(err.Error(), "CANARY") {
			t.Fatalf("%s: err=%v", test.name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := client.GetLinkOnce(ctx, mustSecretLink(t, testLink), "*/*", 1<<20, 32<<10, nil); err == nil {
		t.Fatal("canceled context sent a request")
	}
	if calls != 0 || client.RequestsMade() != 0 {
		t.Fatalf("calls=%d requests=%d", calls, client.RequestsMade())
	}
}

func TestGetLinkOnceErrorsNeverCarryTheLink(t *testing.T) {
	client := strictTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: request.URL.String(), Err: fmt.Errorf("dial failed")}
	}), "")
	_, _, err := client.GetLinkOnce(context.Background(), mustSecretLink(t, testLink), "*/*", 1<<20, 32<<10, nil)
	if err == nil || strings.Contains(err.Error(), "CANARY") || strings.Contains(err.Error(), "dlv2") {
		t.Fatalf("err=%v", err)
	}
}

func hostIs(host string) func(*url.URL) bool {
	return func(target *url.URL) bool { return target.Hostname() == host }
}

func approveAll(*url.URL) bool { return true }
