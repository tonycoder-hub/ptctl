package httpguard

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func loginResponse(cookies ...string) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusFound,
		Header:        http.Header{"Location": {"/index.php"}, "Content-Type": {"text/html; charset=utf-8"}, "Set-Cookie": cookies},
		Body:          io.NopCloser(strings.NewReader("")),
		ContentLength: 0,
	}
}

func TestPostLoginFormOnceCapturesOnlyRequestedLiveCookies(t *testing.T) {
	future := time.Now().Add(7 * 24 * time.Hour).UTC().Format(http.TimeFormat)
	calls := 0
	client := strictTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if request.Method != http.MethodPost || request.URL.Path != "/takelogin.php" || request.URL.RawQuery != "" ||
			request.Header.Get("Cookie") != "" || string(body) != "username=CANARY-USER&password=PASSWORD-CANARY" {
			t.Fatalf("unexpected login POST: %#v body=%q", request, body)
		}
		return loginResponse(
			"tracker=OTHER-COOKIE-CANARY; Path=/",
			"access_token=SESSION-CANARY.part-2_x; Domain=.example.test; Path=/; Expires="+future,
		), nil
	}), "")
	response, cookies, err := client.PostLoginFormOnce(context.Background(), "takelogin.php",
		[]FormField{{Name: "username", Value: "CANARY-USER"}, {Name: "password", Value: "PASSWORD-CANARY"}},
		"text/html", 4, 1024, 1024, 1024, nil, []string{"access_token"})
	if err != nil || calls != 1 || response.StatusCode != http.StatusFound || len(cookies) != 1 {
		t.Fatalf("response=%#v cookies=%d err=%v calls=%d", response, len(cookies), err, calls)
	}
	cookie := cookies[0]
	if cookie.Name != "access_token" || cookie.Value() != "SESSION-CANARY.part-2_x" || cookie.Expires.IsZero() || cookie.Expires.Location() != time.UTC {
		t.Fatalf("cookie name=%q expires=%v", cookie.Name, cookie.Expires)
	}
	for _, rendered := range []string{fmt.Sprint(cookie), fmt.Sprintf("%v %+v %#v %s", cookie, cookie, cookie, cookie), fmt.Sprintf("%v %+v", cookies, struct{ C SessionCookie }{cookie})} {
		if strings.Contains(rendered, "SESSION-CANARY") {
			t.Fatalf("session cookie leaked through formatting: %q", rendered)
		}
	}
}

func TestPostLoginFormOnceIgnoresDeletedAndExpiredCookies(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	for _, header := range []string{
		"access_token=deleted; Path=/",
		"access_token=; Path=/",
		"access_token=STALE-CANARY; Max-Age=0",
		"access_token=STALE-CANARY; Expires=" + past,
	} {
		client := strictTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			return loginResponse(header), nil
		}), "")
		_, cookies, err := client.PostLoginFormOnce(context.Background(), "takelogin.php", []FormField{{Name: "username", Value: "u"}},
			"text/html", 4, 1024, 1024, 1024, nil, []string{"access_token"})
		if err != nil || len(cookies) != 0 {
			t.Fatalf("%q: cookies=%d err=%v", header, len(cookies), err)
		}
	}
}

func TestPostLoginFormOnceFailsClosedOnAmbiguousOrInvalidCookies(t *testing.T) {
	for _, headers := range [][]string{
		{"access_token=FIRST-CANARY", "access_token=SECOND-CANARY"},
		{"access_token=" + strings.Repeat("A", maxCapturedCookieBytes+1)},
		{`access_token="QUOTED CANARY"`},
	} {
		client := strictTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			return loginResponse(headers...), nil
		}), "")
		_, cookies, err := client.PostLoginFormOnce(context.Background(), "takelogin.php", []FormField{{Name: "username", Value: "u"}},
			"text/html", 4, 1024, 1024, 1024, nil, []string{"access_token"})
		if err == nil || cookies != nil || client.RequestsMade() != 1 || strings.Contains(err.Error(), "CANARY") {
			t.Fatalf("%q: cookies=%d err=%v", headers, len(cookies), err)
		}
	}
	client := strictTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return loginResponse("access_token=SAME-CANARY; Path=/", "access_token=SAME-CANARY; Path=/"), nil
	}), "")
	if _, cookies, err := client.PostLoginFormOnce(context.Background(), "takelogin.php", []FormField{{Name: "username", Value: "u"}},
		"text/html", 4, 1024, 1024, 1024, nil, []string{"access_token"}); err != nil || len(cookies) != 1 {
		t.Fatalf("repeated identical cookie: cookies=%d err=%v", len(cookies), err)
	}
}

func TestPostLoginFormOnceRejectsInvalidCookieNamesBeforeSending(t *testing.T) {
	client := strictTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("request sent with invalid cookie names")
		return nil, nil
	}), "")
	for _, names := range [][]string{nil, {""}, {"a b"}, {"a;b"}, {"dup", "dup"}, {"1", "2", "3", "4", "5", "6", "7", "8", "9"}} {
		if _, _, err := client.PostLoginFormOnce(context.Background(), "takelogin.php", []FormField{{Name: "username", Value: "u"}},
			"text/html", 4, 1024, 1024, 1024, nil, names); err == nil {
			t.Fatalf("names %q accepted", names)
		}
	}
	if client.RequestsMade() != 0 {
		t.Fatalf("requests=%d", client.RequestsMade())
	}
}
