package httpguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func strictTestClient(t *testing.T, transport http.RoundTripper, cookie string) *Client {
	t.Helper()
	base, err := url.Parse("https://example.test/")
	if err != nil {
		t.Fatal(err)
	}
	return &Client{
		base:       base,
		cookie:     cookie,
		maxBody:    defaultMaxBody,
		strictHTTP: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

func TestPublicIPPolicy(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.169.254", "192.0.2.1", "::1", "fc00::1", "2001:db8::1"}
	for _, raw := range blocked {
		if publicIP(net.ParseIP(raw)) {
			t.Fatalf("%s should be blocked", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(net.ParseIP(raw)) {
			t.Fatalf("%s should be public", raw)
		}
	}
}

func TestPublicDialCandidatesAreBoundedAndOrdered(t *testing.T) {
	input := []net.IP{
		net.ParseIP("127.0.0.1"),
		net.ParseIP("1.1.1.1"),
		net.ParseIP("10.0.0.1"),
		net.ParseIP("8.8.8.8"),
		net.ParseIP("9.9.9.9"),
		net.ParseIP("208.67.222.222"),
		net.ParseIP("4.2.2.2"),
	}
	got := publicDialCandidates(input)
	want := []string{"1.1.1.1", "8.8.8.8", "9.9.9.9", "208.67.222.222"}
	if len(got) != len(want) {
		t.Fatalf("candidates=%v", got)
	}
	for index := range want {
		if got[index].String() != want[index] {
			t.Fatalf("candidate[%d]=%s want=%s", index, got[index], want[index])
		}
	}
}

func TestStrictTransportIsFreshH1Only(t *testing.T) {
	client, err := New("https://example.test/", "sid=secret", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	strict, ok := client.strictHTTP.Transport.(*http.Transport)
	if !ok || strict.ForceAttemptHTTP2 || !strict.DisableKeepAlives || strict.TLSNextProto == nil ||
		len(strict.TLSClientConfig.NextProtos) != 1 || strict.TLSClientConfig.NextProtos[0] != "http/1.1" || !strict.DisableCompression {
		t.Fatalf("strict transport is not H1-only/no-reuse: %#v", strict)
	}
}

func TestNewRejectsSecretBearingOrMalformedBaseWithoutEcho(t *testing.T) {
	for _, raw := range []string{
		"https://example.test/?token=CANARY-BASE-SECRET",
		"https://example.test/#CANARY-BASE-SECRET",
		"https://example.test/%zzCANARY-BASE-SECRET",
	} {
		if _, err := New(raw, "", 0); err == nil || strings.Contains(err.Error(), "CANARY") {
			t.Fatalf("raw=%q err=%v", raw, err)
		}
	}
}

func TestGetOnceUsesExactURLAndSafeHeaders(t *testing.T) {
	const cookie = "sid=COOKIE-CANARY"
	calls := 0
	client := strictTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodGet || request.URL.Path != "/download.php" || request.URL.Query().Get("id") != "1&next=CANARY" ||
			request.Header.Get("Cookie") != cookie || request.Header.Get("Accept-Encoding") != "identity" || !request.Close {
			t.Fatalf("unexpected strict request: %#v", request)
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": {"application/x-bittorrent; charset=binary"}},
			Body:          io.NopCloser(bytes.NewBufferString("d1:ae")),
			ContentLength: 5,
		}, nil
	}), cookie)
	response, err := client.GetOnce(context.Background(), "download.php", url.Values{"id": {"1&next=CANARY"}}, "application/x-bittorrent", 1024, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || client.RequestsMade() != 1 || response.StatusCode != http.StatusOK || response.MediaType != "application/x-bittorrent" ||
		string(response.Body) != "d1:ae" || response.ResponseBytesRead != 5 || !response.ResponseBytesKnown || response.ObservedAtStart.IsZero() || response.ObservedAtEnd.Before(response.ObservedAtStart) {
		t.Fatalf("calls=%d requests=%d response=%#v", calls, client.RequestsMade(), response)
	}
}

func TestGetOncePreCanceledMakesNoRequest(t *testing.T) {
	calls := 0
	client := strictTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not run")
	}), "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.GetOnce(ctx, "download.php", nil, "application/x-bittorrent", 1024, 1024)
	if !errors.Is(err, context.Canceled) || calls != 0 || client.RequestsMade() != 0 {
		t.Fatalf("err=%v calls=%d requests=%d", err, calls, client.RequestsMade())
	}
}

func TestGetOnceDoesNotFollowRedirectOrRetry(t *testing.T) {
	calls := 0
	client := strictTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": {"https://redirect.invalid/CANARY"}},
			Body:       io.NopCloser(strings.NewReader("CANARY-REDIRECT-BODY")),
		}, nil
	}), "")
	response, err := client.GetOnce(context.Background(), "download.php", nil, "application/x-bittorrent", 1024, 1024)
	if err != nil || response.StatusCode != http.StatusFound || calls != 1 || client.RequestsMade() != 1 {
		t.Fatalf("response=%#v err=%v calls=%d requests=%d", response, err, calls, client.RequestsMade())
	}

	calls = 0
	client = strictTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("CANARY-TRANSPORT-SECRET")
	}), "")
	_, err = client.GetOnce(context.Background(), "download.php", url.Values{"id": {"CANARY-ID"}}, "application/x-bittorrent", 1024, 1024)
	if err == nil || strings.Contains(err.Error(), "CANARY") || calls != 1 || client.RequestsMade() != 1 {
		t.Fatalf("err=%v calls=%d requests=%d", err, calls, client.RequestsMade())
	}
}

func TestOrdinaryGetAlsoUsesOneStrictAttempt(t *testing.T) {
	calls := 0
	client := strictTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if !request.Close || request.Header.Get("Accept-Encoding") != "identity" {
			t.Fatalf("ordinary request did not use the strict transport contract: %#v", request)
		}
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": {"https://example.test/second?secret=CANARY"}},
			Body:       io.NopCloser(strings.NewReader("CANARY-REDIRECT-BODY")),
		}, nil
	}), "sid=secret")
	if _, _, err := client.Get(context.Background(), "first", nil); err == nil || calls != 1 || client.RequestsMade() != 1 || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("err=%v calls=%d requests=%d", err, calls, client.RequestsMade())
	}

	calls = 0
	client = strictTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("https://example.test/path?COOKIE-CANARY")
	}), "sid=COOKIE-CANARY")
	if _, _, err := client.Get(context.Background(), "first", nil); err == nil || calls != 1 || client.RequestsMade() != 1 || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("err=%v calls=%d requests=%d", err, calls, client.RequestsMade())
	}
}

func TestGetOnceResponseBudgetsAndEncodingFailClosed(t *testing.T) {
	tests := []struct {
		name      string
		header    http.Header
		body      string
		bodyLimit int64
		headLimit int64
		wantRead  int64
		transfer  []string
		trailer   http.Header
		unpacked  bool
	}{
		{name: "body n plus one", body: "1234", bodyLimit: 3, headLimit: 1024, wantRead: 4},
		{name: "header", header: http.Header{"X-Large": {strings.Repeat("x", 32)}}, body: "x", bodyLimit: 1024, headLimit: 8},
		{name: "content range", header: http.Header{"Content-Range": {"bytes 0-1/10"}}, body: "12", bodyLimit: 1024, headLimit: 1024},
		{name: "encoding", header: http.Header{"Content-Encoding": {"gzip"}}, body: "CANARY-COMPRESSED", bodyLimit: 1024, headLimit: 1024},
		{name: "duplicate encoding", header: http.Header{"Content-Encoding": {"identity", "gzip"}}, body: "CANARY-COMPRESSED", bodyLimit: 1024, headLimit: 1024},
		{name: "auto unpacked", body: "CANARY-COMPRESSED", bodyLimit: 1024, headLimit: 1024, unpacked: true},
		{name: "transfer encoding", body: "CANARY-TRANSFER", bodyLimit: 1024, headLimit: 1024, transfer: []string{"gzip", "chunked"}},
		{name: "trailer", body: "CANARY-TRAILER", bodyLimit: 1024, headLimit: 1024, trailer: http.Header{"X-Trailer": {"value"}}},
		{name: "malformed content type", header: http.Header{"Content-Type": {"not a media type ;;;"}}, body: "x", bodyLimit: 1024, headLimit: 1024},
		{name: "unsupported HTML charset", header: http.Header{"Content-Type": {"text/html; charset=gbk"}}, body: "CANARY-CHARSET", bodyLimit: 1024, headLimit: 1024},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := strictTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode:       http.StatusOK,
					Header:           test.header,
					Body:             io.NopCloser(strings.NewReader(test.body)),
					ContentLength:    -1,
					TransferEncoding: test.transfer,
					Trailer:          test.trailer,
					Uncompressed:     test.unpacked,
				}, nil
			}), "")
			response, err := client.GetOnce(context.Background(), "download.php", nil, "application/x-bittorrent", test.bodyLimit, test.headLimit)
			if err == nil || client.RequestsMade() != 1 || response.ResponseBytesRead != test.wantRead || response.ResponseBytesKnown || len(response.Body) != 0 || strings.Contains(err.Error(), "CANARY") {
				t.Fatalf("response=%#v err=%v requests=%d", response, err, client.RequestsMade())
			}
		})
	}
}

func TestPostFormOnceIsOrderedNonReplayableAndRetainsOnlyAllowedRedirectID(t *testing.T) {
	const cookie = "sid=COOKIE-CANARY"
	calls := 0
	client := strictTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if request.Method != http.MethodPost || request.URL.Path != "/mybonusapps.php" || request.URL.RawQuery != "action=exchange" ||
			request.Header.Get("Cookie") != cookie || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" ||
			request.Header.Get("Accept-Encoding") != "identity" || !request.Close || request.GetBody != nil ||
			string(body) != "option=1&csrf=opaque+value&csrf=second&submit=Exchange%21" {
			t.Fatalf("unexpected strict POST: request=%#v body=%q", request, body)
		}
		return &http.Response{
			StatusCode: http.StatusFound,
			Header: http.Header{
				"Location":     {"/mybonusapps.php?do=upload"},
				"Content-Type": {"text/html; charset=utf-8"},
			},
			Body:          io.NopCloser(strings.NewReader("")),
			ContentLength: 0,
		}, nil
	}), cookie)
	response, err := client.PostFormOnce(
		context.Background(), "mybonusapps.php", url.Values{"action": {"exchange"}},
		[]FormField{{Name: "option", Value: "1"}, {Name: "csrf", Value: "opaque value"}, {Name: "csrf", Value: "second"}, {Name: "submit", Value: "Exchange!"}},
		"text/html", 8, 1024, 1024, 1024,
		func(target *url.URL) (string, bool) {
			if target.Path == "/mybonusapps.php" && target.Query().Get("do") == "upload" {
				return "test.bonus.confirm.upload.v1", true
			}
			return "", false
		},
	)
	if err != nil || calls != 1 || client.RequestsMade() != 1 || response.StatusCode != http.StatusFound ||
		response.RedirectID != "test.bonus.confirm.upload.v1" || !response.ResponseBytesKnown || response.ResponseBytesRead != 0 {
		t.Fatalf("response=%#v err=%v calls=%d requests=%d", response, err, calls, client.RequestsMade())
	}
}

func TestPostFormOnceResponseBudgetsAndEncodingFailClosed(t *testing.T) {
	tests := []struct {
		name      string
		header    http.Header
		body      string
		bodyLimit int64
		headLimit int64
		wantRead  int64
		transfer  []string
		trailer   http.Header
		unpacked  bool
	}{
		{name: "body n plus one", body: "1234", bodyLimit: 3, headLimit: 1024, wantRead: 4},
		{name: "header", header: http.Header{"X-Large": {strings.Repeat("x", 32)}}, body: "x", bodyLimit: 1024, headLimit: 8},
		{name: "content range", header: http.Header{"Content-Range": {"bytes 0-1/10"}}, body: "12", bodyLimit: 1024, headLimit: 1024},
		{name: "encoding", header: http.Header{"Content-Encoding": {"gzip"}}, body: "CANARY-COMPRESSED", bodyLimit: 1024, headLimit: 1024},
		{name: "auto unpacked", body: "CANARY-COMPRESSED", bodyLimit: 1024, headLimit: 1024, unpacked: true},
		{name: "transfer encoding", body: "CANARY-TRANSFER", bodyLimit: 1024, headLimit: 1024, transfer: []string{"gzip", "chunked"}},
		{name: "trailer", body: "CANARY-TRAILER", bodyLimit: 1024, headLimit: 1024, trailer: http.Header{"X-Trailer": {"value"}}},
		{name: "malformed content type", header: http.Header{"Content-Type": {"not a media type ;;;"}}, body: "x", bodyLimit: 1024, headLimit: 1024},
		{name: "unsupported HTML charset", header: http.Header{"Content-Type": {"text/html; charset=gbk"}}, body: "CANARY-CHARSET", bodyLimit: 1024, headLimit: 1024},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := strictTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode:       http.StatusOK,
					Header:           test.header,
					Body:             io.NopCloser(strings.NewReader(test.body)),
					ContentLength:    -1,
					TransferEncoding: test.transfer,
					Trailer:          test.trailer,
					Uncompressed:     test.unpacked,
				}, nil
			}), "sid=COOKIE-CANARY")
			response, err := client.PostFormOnce(
				context.Background(), "mybonusapps.php", nil, []FormField{{Name: "option", Value: "1"}},
				"text/html", 4, 1024, test.bodyLimit, test.headLimit, nil,
			)
			if err == nil || client.RequestsMade() != 1 || response.ResponseBytesRead != test.wantRead ||
				response.ResponseBytesKnown || len(response.Body) != 0 || strings.Contains(err.Error(), "CANARY") {
				t.Fatalf("response=%#v err=%v requests=%d", response, err, client.RequestsMade())
			}
		})
	}
}

func TestValidateFormFieldsMatchesExactPostEncodingBudget(t *testing.T) {
	fields := []FormField{{Name: "option", Value: "1"}, {Name: "csrf", Value: "opaque value"}, {Name: "csrf", Value: "second"}}
	count, encodedBytes, err := ValidateFormFields(fields, len(fields), int64(len("option=1&csrf=opaque+value&csrf=second")))
	if err != nil || count != len(fields) || encodedBytes != int64(len("option=1&csrf=opaque+value&csrf=second")) {
		t.Fatalf("count=%d bytes=%d err=%v", count, encodedBytes, err)
	}
	if _, _, err := ValidateFormFields(fields, len(fields)-1, 1024); err == nil {
		t.Fatal("field N+1 unexpectedly passed preflight")
	}
	if _, _, err := ValidateFormFields(fields, len(fields), encodedBytes-1); err == nil {
		t.Fatal("encoded byte N+1 unexpectedly passed preflight")
	}
}

func TestPostFormOnceFailsClosedWithoutLeakingOrRetrying(t *testing.T) {
	tests := []struct {
		name     string
		ctx      func() context.Context
		fields   []FormField
		location string
		failure  error
		wantCall int
	}{
		{name: "pre canceled", ctx: canceledHTTPContext, fields: []FormField{{Name: "option", Value: "1"}}, wantCall: 0},
		{name: "empty field name", ctx: context.Background, fields: []FormField{{Name: "", Value: "CANARY-FIELD"}}, wantCall: 0},
		{name: "transport", ctx: context.Background, fields: []FormField{{Name: "option", Value: "1"}}, failure: errors.New("https://example.test/?COOKIE-CANARY"), wantCall: 1},
		{name: "cross origin redirect", ctx: context.Background, fields: []FormField{{Name: "option", Value: "1"}}, location: "https://other.invalid/CANARY-LOCATION", wantCall: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := strictTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if test.failure != nil {
					return nil, test.failure
				}
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {test.location}}, Body: io.NopCloser(strings.NewReader(""))}, nil
			}), "sid=COOKIE-CANARY")
			response, err := client.PostFormOnce(test.ctx(), "mybonusapps.php", nil, test.fields, "text/html", 4, 1024, 1024, 1024, func(*url.URL) (string, bool) {
				return "test.allowed.v1", true
			})
			if calls != test.wantCall || client.RequestsMade() != test.wantCall || strings.Contains(fmt.Sprint(err), "CANARY") || response.RedirectID != "" {
				t.Fatalf("response=%#v err=%v calls=%d requests=%d", response, err, calls, client.RequestsMade())
			}
			if test.name != "cross origin redirect" && err == nil {
				t.Fatal("invalid or failed POST unexpectedly succeeded")
			}
			if test.name == "cross origin redirect" && err != nil {
				t.Fatalf("unrecognized redirect should remain a safe response: %v", err)
			}
		})
	}
}

func canceledHTTPContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
