package tjupt

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/site"
)

func TestBonusCatalogReadsTheExchangePage(t *testing.T) {
	read := func(body []byte, final string) (*accountFakeClient, error) {
		finalURL, err := url.Parse(final)
		if err != nil {
			t.Fatal(err)
		}
		client := &accountFakeClient{body: body, finalURL: finalURL}
		adapter := New("")
		adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
		catalog, err := adapter.BonusCatalog(context.Background(), mustCredential(t, "sid=secret"))
		if err == nil {
			if catalog.SiteID != SiteID || catalog.Balance != "12345.67" || catalog.ObservedAt.IsZero() || len(catalog.Rows) != 2 ||
				catalog.Rows[0].Selector != "1" || catalog.Rows[1].Selector != "2" || strings.Join(catalog.Rows[1].Columns, "|") != "邀请名额|CANARY 5,000|交换|available" {
				t.Fatalf("catalog=%#v", catalog)
			}
		}
		if client.requests != 1 || client.closeCalls != 1 || client.path != bonusPagePath || len(client.query) != 0 {
			t.Fatalf("client=%#v", client)
		}
		return client, err
	}
	const page = "https://www.tjupt.org/mybonus.php"
	offers := tjuptBonusRow("1", "1", "上传量兑换", "用魔力值兑换 10.0 GiB 上传量。", "1,000", `<input type="submit" name="submit" value="交换" />`) +
		tjuptBonusRow("2", "2", "邀请名额", "CANARY 说明", "CANARY 5,000", `<input type="submit" name="submit" value="交换" />`)
	if _, err := read(bonusReviewPage(offers), page); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		body  []byte
		final string
		want  string
	}{
		"login form":         {[]byte(`<form action="takelogin.php"><input name="username"><input name="password" type="password"></form>`), page, "TJUPT session is not authenticated"},
		"games page":         {bonusReviewPage(offers), "https://www.tjupt.org/mybonusapps.php", "TJUPT returned an unrecognized bonus page; parser is fail-closed"},
		"challenge":          {append(bonusReviewPage(offers), "cf-chl-CANARY"...), page, "TJUPT returned an unrecognized bonus page; parser is fail-closed"},
		"no balance":         {[]byte(strings.Replace(string(bonusReviewPage(offers)), "当前魔力值：12,345.67", "", 1)), page, "TJUPT returned an unrecognized bonus page; parser is fail-closed"},
		"no offers":          {bonusReviewPage(`<tr><td>CANARY 系统维护</td></tr>`), page, "TJUPT bonus page structure was not recognized; refusing an empty catalog"},
		"ambiguous selector": {bonusReviewPage(offers + tjuptBonusRow("1", "3", "C", "c", "300", `<input type="submit">`)), page, "TJUPT bonus page structure was not recognized; refusing an empty catalog"},
	} {
		if _, err := read(test.body, test.final); err == nil || err.Error() != test.want {
			t.Fatalf("%s: err=%v, want %q", name, err, test.want)
		}
	}
}

func TestClassifyBonusReviewResponseStopReasons(t *testing.T) {
	page := bonusReviewPage(tjuptBonusRow("1", "1", "A", "a", "100", `<input type="submit">`))
	tests := []struct {
		name   string
		modify func(*int, *[]byte, *string, *string)
		want   string
	}{
		{"unauthorized", func(status *int, _ *[]byte, _, _ *string) { *status = http.StatusUnauthorized }, "authentication_required"},
		{"forbidden", func(status *int, _ *[]byte, _, _ *string) { *status = http.StatusForbidden }, "authentication_required"},
		{"not found", func(status *int, _ *[]byte, _, _ *string) { *status = http.StatusNotFound }, "not_found"},
		{"rate limited", func(status *int, _ *[]byte, _, _ *string) { *status = http.StatusTooManyRequests }, "rate_limited"},
		{"login redirect", func(status *int, _ *[]byte, _, redirect *string) {
			*status, *redirect = http.StatusFound, loginRedirectID
		}, "authentication_required"},
		{"other redirect", func(status *int, _ *[]byte, _, redirect *string) {
			*status, *redirect = http.StatusFound, "tjupt.other"
		}, "redirect_rejected"},
		{"server error", func(status *int, _ *[]byte, _, _ *string) { *status = http.StatusInternalServerError }, "http_status_rejected"},
		{"no content", func(status *int, _ *[]byte, _, _ *string) { *status = http.StatusNoContent }, "http_status_rejected"},
		{"empty", func(_ *int, body *[]byte, _, _ *string) { *body = nil }, "empty_response"},
		{"invalid utf8", func(_ *int, body *[]byte, _, _ *string) { *body = append(append([]byte(nil), page...), 0xff) }, "invalid_text_encoding"},
		{"json", func(_ *int, _ *[]byte, mediaType, _ *string) { *mediaType = "application/json" }, "content_type_rejected"},
		{"login page", func(_ *int, body *[]byte, _, _ *string) {
			*body = []byte(`<form action="takelogin.php" method="post"><input name="username"><input type="password" name="password"></form>`)
		}, "authentication_required"},
		{"challenge", func(_ *int, body *[]byte, _, _ *string) {
			*body = []byte(`<html><title>Just a moment...</title><div id="cf-challenge-running">CANARY</div></html>`)
		}, "challenge_response"},
		{"untyped html", func(_ *int, _ *[]byte, mediaType, _ *string) { *mediaType = "" }, ""},
		{"html", func(*int, *[]byte, *string, *string) {}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := strictBonusResponse(page, time.Now().UTC())
			test.modify(&response.StatusCode, &response.Body, &response.MediaType, &response.RedirectID)
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
	// A form outside any row has only its loose text as one cell, which is
	// never enough to describe an offer.
	body := bonusReviewPage(`</table><div><form action="?action=exchange" method="post"><input type="hidden" name="option" value="1">CANARY 说明 500<input type="submit" value="交换"></form></div><table>`)
	_, _, err := parseBonusOfferReview(body, "1", site.DefaultBonusReviewLimits())
	if got := bonusParseStopReason(err); got != "unrecognized_offer" {
		t.Fatalf("stop=%q err=%v", got, err)
	}
}

func TestAppendBonusCellNormalizesAndBounds(t *testing.T) {
	form := &parsedBonusForm{}
	appendBonusCell(form, " \n ")
	if len(form.cells) != 0 || form.cellsOverflow {
		t.Fatalf("blank text produced %#v", form)
	}
	appendBonusCell(form, " CANARY  说明\n500 ")
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

func TestCanonicalBonusBalance(t *testing.T) {
	tests := map[string]string{
		"12,345.6":  "12345.6",
		"1,000,000": "1000000",
		"0.50":      "0.50",
		"42":        "42",
		"":          "",
		"1,00.0":    "",
		"1234,567":  "",
		",123":      "",
		"1,234,":    "",
		"1.":        "",
		".5":        "",
		"1.2.3":     "",
		"1.2e3":     "",
		"-1":        "",
		"１２":        "",
	}
	tests[strings.Repeat("9", 129)] = ""
	for input, want := range tests {
		if got := canonicalBonusBalance(input); got != want {
			t.Errorf("canonicalBonusBalance(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestFirstSizeRejectsValuesInt64CannotHold(t *testing.T) {
	// 8192 PiB is exactly 2^63. The bound must be checked before converting,
	// because converting 2^63 to int64 is negative on amd64.
	for _, text := range []string{"8192 PiB", "8192.00 PB", "8191.9999999999999999 PiB", "99999 PB"} {
		if size, ok := firstSize([]string{text}); ok || size != 0 {
			t.Fatalf("firstSize(%q)=%d,%v", text, size, ok)
		}
	}
	if size, ok := firstSize([]string{"8191 PiB"}); !ok || size != 8191<<50 {
		t.Fatalf("firstSize(8191 PiB)=%d,%v", size, ok)
	}
}
