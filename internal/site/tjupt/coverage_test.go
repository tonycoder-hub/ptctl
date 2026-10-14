package tjupt

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/site"
)

func TestClassifyBonusReviewResponseStopReasons(t *testing.T) {
	page := tjuptBonusPage("CANARY-USER", "12,345.6", tjuptBonusRows)
	tests := []struct {
		name   string
		modify func(*int, *[]byte, *string)
		want   string
	}{
		{"unauthorized", func(status *int, _ *[]byte, _ *string) { *status = http.StatusUnauthorized }, "authentication_required"},
		{"forbidden", func(status *int, _ *[]byte, _ *string) { *status = http.StatusForbidden }, "authentication_required"},
		{"not found", func(status *int, _ *[]byte, _ *string) { *status = http.StatusNotFound }, "not_found"},
		{"rate limited", func(status *int, _ *[]byte, _ *string) { *status = http.StatusTooManyRequests }, "rate_limited"},
		{"redirect", func(status *int, _ *[]byte, _ *string) { *status = http.StatusFound }, "redirect_rejected"},
		{"server error", func(status *int, _ *[]byte, _ *string) { *status = http.StatusInternalServerError }, "http_status_rejected"},
		{"no content", func(status *int, _ *[]byte, _ *string) { *status = http.StatusNoContent }, "http_status_rejected"},
		{"empty", func(_ *int, body *[]byte, _ *string) { *body = nil }, "empty_response"},
		{"invalid utf8", func(_ *int, body *[]byte, _ *string) { *body = append(append([]byte(nil), page...), 0xff) }, "invalid_text_encoding"},
		{"json", func(_ *int, _ *[]byte, mediaType *string) { *mediaType = "application/json" }, "content_type_rejected"},
		{"login", func(_ *int, body *[]byte, _ *string) {
			*body = []byte(`<form action="takelogin.php" method="post"><input name="username"><input type="password" name="password"></form>`)
		}, "authentication_required"},
		{"challenge", func(_ *int, body *[]byte, _ *string) {
			*body = []byte(`<html><title>Just a moment...</title><div id="cf-challenge-running">CANARY</div></html>`)
		}, "challenge_response"},
		{"untyped html", func(_ *int, _ *[]byte, mediaType *string) { *mediaType = "" }, ""},
		{"html", func(*int, *[]byte, *string) {}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := strictBonusResponse(page, time.Now().UTC())
			test.modify(&response.StatusCode, &response.Body, &response.MediaType)
			stop, err := classifyBonusReviewResponse(response)
			if stop != test.want || (err == nil) != (test.want == "") {
				t.Fatalf("stop=%q err=%v, want %q", stop, err, test.want)
			}
			if err != nil && strings.Contains(err.Error(), "CANARY") {
				t.Fatalf("error leaked page text: %v", err)
			}
		})
	}
}

func TestBonusParseStopReasonAllowlist(t *testing.T) {
	for _, reason := range []string{"selector_not_found", "selector_ambiguous", "token_budget_exceeded", "form_budget_exceeded", "field_budget_exceeded", "visible_text_budget_exceeded", "invalid_html", "ambiguous_form_structure", "unrecognized_response", "unrecognized_offer"} {
		err := fmt.Errorf("wrapped: %w", &bonusParseError{reason: reason})
		if got := bonusParseStopReason(err); got != reason {
			t.Fatalf("bonusParseStopReason(%q)=%q", reason, got)
		}
		if got := (&bonusParseError{reason: reason}).Error(); got != reason {
			t.Fatalf("Error()=%q, want %q", got, reason)
		}
	}
	for _, err := range []error{nil, errors.New("selector_not_found"), &bonusParseError{reason: "CANARY-unlisted"}} {
		if got := bonusParseStopReason(err); got != "unrecognized_response" {
			t.Fatalf("bonusParseStopReason(%v)=%q", err, got)
		}
	}
}

func TestBonusReviewRejectsAFormOutsideATableRow(t *testing.T) {
	// A form that is not inside a row has only its loose text as one cell,
	// which is never enough to describe an offer.
	body := tjuptBonusPage("CANARY-USER", "12,345.6", "</table><div>"+
		`<form action="?action=exchange" method="post"><input type="hidden" name="option" value="1">CANARY 说明 500<input type="submit" value="交换"></form>`+
		"</div><table>")
	_, _, err := parseBonusOfferReview(body, "1", site.DefaultBonusReviewLimits())
	if got := bonusParseStopReason(err); got != "unrecognized_offer" {
		t.Fatalf("stop=%q err=%v", got, err)
	}
}

func TestAppendBonusCellNormalizesAndBounds(t *testing.T) {
	form := &parsedBonusForm{}
	appendBonusCell(form, "  \n ")
	if len(form.cells) != 0 || form.cellsOverflow {
		t.Fatalf("blank text produced %#v", form)
	}
	appendBonusCell(form, " CANARY  说明\n500 ")
	if len(form.cells) != 1 || form.cells[0] != "CANARY 说明 500" {
		t.Fatalf("cells=%q", form.cells)
	}
	for len(form.cells) < 16 {
		appendBonusCell(form, "x")
	}
	appendBonusCell(form, "overflow")
	if len(form.cells) != 16 || !form.cellsOverflow {
		t.Fatalf("cells=%d overflow=%v", len(form.cells), form.cellsOverflow)
	}
}

func TestMatchesAccountURL(t *testing.T) {
	const base = DefaultBaseURL
	tests := []struct {
		observed string
		want     bool
	}{
		{"https://www.tjupt.org/mybonusapps.php", true},
		{"HTTPS://WWW.TJUPT.ORG/mybonusapps.php", true},
		{"http://www.tjupt.org/mybonusapps.php", false},
		{"https://tjupt.org/mybonusapps.php", false},
		{"https://www.tjupt.org/MyBonusApps.php", false},
		{"https://www.tjupt.org/mybonusapps.php?action=exchange", false},
		{"https://www.tjupt.org/mybonusapps.php?", false},
		{"https://www.tjupt.org/mybonusapps.php#top", false},
		{"https://user@www.tjupt.org/mybonusapps.php", false},
		{"https://www.tjupt.org/mybonus%61pps.php", false},
		{"https://www.tjupt.org/login.php", false},
	}
	for _, test := range tests {
		observed, err := url.Parse(test.observed)
		if err != nil {
			t.Fatal(err)
		}
		if got := matchesAccountURL(base, observed); got != test.want {
			t.Errorf("matchesAccountURL(%q)=%v, want %v", test.observed, got, test.want)
		}
	}
	if matchesAccountURL(base, nil) {
		t.Fatal("nil URL matched")
	}
	if matchesAccountURL("://bad", &url.URL{Scheme: "https", Host: "www.tjupt.org", Path: "/mybonusapps.php"}) {
		t.Fatal("unparseable base URL matched")
	}
}

func TestCanonicalBonusBalance(t *testing.T) {
	tests := map[string]string{
		"12,345.6":               "12345.6",
		"1,000,000":              "1000000",
		"0.50":                   "0.50",
		"42":                     "42",
		"":                       "",
		"1,00.0":                 "",
		"1234,567":               "",
		",123":                   "",
		"1,234,":                 "",
		"1.":                     "",
		".5":                     "",
		"1.2.3":                  "",
		"1.2e3":                  "",
		"-1":                     "",
		"１２":                     "",
		strings.Repeat("9", 129): "",
	}
	for input, want := range tests {
		if got := canonicalBonusBalance(input); got != want {
			t.Errorf("canonicalBonusBalance(%q)=%q, want %q", input, got, want)
		}
	}
}
