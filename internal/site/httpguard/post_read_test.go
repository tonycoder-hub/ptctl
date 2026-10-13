package httpguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func headerCredentialTestClient(t *testing.T, transport http.RoundTripper, header, value string) *Client {
	t.Helper()
	client := strictTestClient(t, transport, "")
	client.credentialHeader, client.credentialValue = header, value
	return client
}

func TestNewWithHeaderCredentialValidatesWithoutEcho(t *testing.T) {
	client, err := NewWithHeaderCredential("https://api.example.test/", "x-api-key", "KEY-CANARY", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.credentialHeader != "X-Api-Key" || client.credentialValue != "KEY-CANARY" {
		t.Fatalf("credential header=%q", client.credentialHeader)
	}
	for _, test := range []struct{ base, header, value string }{
		{"https://api.example.test/", "", "KEY-CANARY"},
		{"https://api.example.test/", "Cookie", "KEY-CANARY"},
		{"https://api.example.test/", "user-agent", "KEY-CANARY"},
		{"https://api.example.test/", "Content-Type", "KEY-CANARY"},
		{"https://api.example.test/", "Host", "KEY-CANARY"},
		{"https://api.example.test/", "x api key", "KEY-CANARY"},
		{"https://api.example.test/", "x-api-key:", "KEY-CANARY"},
		{"https://api.example.test/", strings.Repeat("x", 65), "KEY-CANARY"},
		{"https://api.example.test/", "x-api-key", ""},
		{"https://api.example.test/", "x-api-key", " KEY-CANARY"},
		{"https://api.example.test/", "x-api-key", "KEY-CANARY\r\nX-Injected: 1"},
		{"https://api.example.test/", "x-api-key", "KEY-CANARY-é"},
		{"https://api.example.test/", "x-api-key", "KEY-CANARY" + strings.Repeat("k", 8<<10)},
		{"http://api.example.test/", "x-api-key", "KEY-CANARY"},
		{"https://api.example.test/?token=CANARY", "x-api-key", "KEY-CANARY"},
	} {
		if client, err := NewWithHeaderCredential(test.base, test.header, test.value, 0); err == nil || client != nil || strings.Contains(err.Error(), "CANARY") {
			t.Fatalf("base=%q header=%q client=%v err=%v", test.base, test.header, client, err)
		}
	}
}

func TestPostReadOnceSendsOneNonReplayableRead(t *testing.T) {
	const key = "KEY-CANARY"
	const payload = `{"keyword":"CANARY query"}`
	calls := 0
	client := headerCredentialTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if request.Method != http.MethodPost || request.URL.Path != "/api/torrent/search" || request.URL.RawQuery != "" ||
			request.Header.Get("X-Api-Key") != key || request.Header.Get("Cookie") != "" ||
			request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "application/json" ||
			request.Header.Get("Accept-Encoding") != "identity" || !strings.Contains(request.Header.Get("User-Agent"), "conservative-read-only-client") ||
			!request.Close || request.GetBody != nil || request.ContentLength != int64(len(payload)) || string(body) != payload {
			t.Fatalf("unexpected strict POST: request=%#v body=%q", request, body)
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": {"application/json;charset=UTF-8"}},
			Body:          io.NopCloser(strings.NewReader(`{"code":"0"}`)),
			ContentLength: 12,
		}, nil
	}), "X-Api-Key", key)
	response, err := client.PostReadOnce(context.Background(), "api/torrent/search", "application/json", []byte(payload), "application/json", 1024, 1024, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || client.RequestsMade() != 1 || response.StatusCode != http.StatusOK || response.MediaType != "application/json" ||
		string(response.Body) != `{"code":"0"}` || response.RequestBytes != int64(len(payload)) || !response.ResponseBytesKnown ||
		response.ObservedAtStart.IsZero() || response.ObservedAtEnd.Before(response.ObservedAtStart) {
		t.Fatalf("calls=%d requests=%d response=%#v", calls, client.RequestsMade(), response)
	}
}

func TestPostReadOnceRejectsInvalidInputBeforeAnyRequest(t *testing.T) {
	tests := []struct {
		name        string
		ctx         func() context.Context
		path        string
		contentType string
		body        string
		accept      string
		maxRequest  int64
		maxBody     int64
	}{
		{name: "pre canceled", ctx: canceledHTTPContext, path: "api/x", contentType: "application/json", accept: "application/json", maxRequest: 64, maxBody: 64},
		{name: "absolute path", ctx: context.Background, path: "/api/x", contentType: "application/json", accept: "application/json", maxRequest: 64, maxBody: 64},
		{name: "cross origin", ctx: context.Background, path: "https://other.invalid/CANARY", contentType: "application/json", accept: "application/json", maxRequest: 64, maxBody: 64},
		{name: "body over budget", ctx: context.Background, path: "api/x", contentType: "application/json", body: "CANARY-BODY", accept: "application/json", maxRequest: 4, maxBody: 64},
		{name: "request budget zero", ctx: context.Background, path: "api/x", contentType: "application/json", accept: "application/json", maxBody: 64},
		{name: "request budget too large", ctx: context.Background, path: "api/x", contentType: "application/json", accept: "application/json", maxRequest: 1<<20 + 1, maxBody: 64},
		{name: "response budget zero", ctx: context.Background, path: "api/x", contentType: "application/json", accept: "application/json", maxRequest: 64},
		{name: "empty content type", ctx: context.Background, path: "api/x", accept: "application/json", maxRequest: 64, maxBody: 64},
		{name: "malformed content type", ctx: context.Background, path: "api/x", contentType: "not a type ;;;", accept: "application/json", maxRequest: 64, maxBody: 64},
		{name: "content type injection", ctx: context.Background, path: "api/x", contentType: "application/json\r\nX-CANARY: 1", accept: "application/json", maxRequest: 64, maxBody: 64},
		{name: "empty accept", ctx: context.Background, path: "api/x", contentType: "application/json", maxRequest: 64, maxBody: 64},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := headerCredentialTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected request")
			}), "X-Api-Key", "KEY-CANARY")
			_, err := client.PostReadOnce(test.ctx(), test.path, test.contentType, []byte(test.body), test.accept, test.maxRequest, test.maxBody, 1024)
			if err == nil || calls != 0 || client.RequestsMade() != 0 || strings.Contains(err.Error(), "CANARY") {
				t.Fatalf("err=%v calls=%d requests=%d", err, calls, client.RequestsMade())
			}
		})
	}
}

func TestPostReadOnceDoesNotFollowRedirectOrFailOpen(t *testing.T) {
	tests := []struct {
		name     string
		response *http.Response
		wantErr  bool
		status   int
	}{
		{name: "redirect is returned, not followed", status: http.StatusFound, response: &http.Response{
			StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://other.invalid/CANARY-LOCATION"}}, Body: io.NopCloser(strings.NewReader("")),
		}},
		{name: "encoded body", wantErr: true, response: &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Encoding": {"gzip"}}, Body: io.NopCloser(strings.NewReader("CANARY")), ContentLength: -1,
		}},
		{name: "body over budget", wantErr: true, response: &http.Response{
			StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 65))), ContentLength: -1,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := headerCredentialTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return test.response, nil
			}), "X-Api-Key", "KEY-CANARY")
			response, err := client.PostReadOnce(context.Background(), "api/x", "application/json", []byte("{}"), "application/json", 64, 64, 1024)
			if calls != 1 || client.RequestsMade() != 1 || response.RedirectID != "" || (err != nil) != test.wantErr ||
				(err != nil && strings.Contains(err.Error(), "CANARY")) || (!test.wantErr && response.StatusCode != test.status) {
				t.Fatalf("response=%#v err=%v calls=%d", response, err, calls)
			}
		})
	}
}

func TestBlockedAddressKeepsGenericTextButIsDetectable(t *testing.T) {
	_, err := newStrictTransport().DialContext(context.Background(), "tcp", "198.18.0.1:443")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("fake-IP range dial err=%v, want ErrBlockedAddress", err)
	}
	// Through the real net/http stack too; the dial policy refuses the address
	// before any packet is sent.
	live, err := NewWithHeaderCredential("https://198.18.0.1/", "x-api-key", "KEY-CANARY", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := live.PostReadOnce(context.Background(), "api/x", "application/json", []byte("{}"), "application/json", 64, 64, 1024); err == nil ||
		err.Error() != "site request failed" || !errors.Is(err, ErrBlockedAddress) || live.RequestsMade() != 1 {
		t.Fatalf("live blocked err=%v requests=%d", err, live.RequestsMade())
	}
	for name, call := range map[string]func(*Client) error{
		"GetOnce": func(client *Client) error {
			_, err := client.GetOnce(context.Background(), "x", nil, "text/html", 64, 1024)
			return err
		},
		"PostReadOnce": func(client *Client) error {
			_, err := client.PostReadOnce(context.Background(), "x", "application/json", []byte("{}"), "application/json", 64, 64, 1024)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			blocked := strictTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, &url.Error{Op: "Post", URL: "https://example.test/?CANARY", Err: fmt.Errorf("dial: %w", ErrBlockedAddress)}
			}), "")
			if err := call(blocked); err == nil || err.Error() != "site request failed" || !errors.Is(err, ErrBlockedAddress) {
				t.Fatalf("blocked err=%v", err)
			}
			other := strictTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("connection reset CANARY")
			}), "")
			if err := call(other); err == nil || err.Error() != "site request failed" || errors.Is(err, ErrBlockedAddress) {
				t.Fatalf("other err=%v", err)
			}
		})
	}
}
