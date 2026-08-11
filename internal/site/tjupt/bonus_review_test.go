package tjupt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

func TestBonusReviewCapabilityAndCredentialFreePreflight(t *testing.T) {
	trusted := New("")
	if !trusted.Descriptor().Supports(domain.CapabilityBonusReview) {
		t.Fatal("trusted production adapter did not declare bonus review")
	}
	config, err := trusted.BonusReviewConfig()
	if err != nil || config.Origin != BonusReviewOrigin || config.RouteID != BonusReviewRouteID || config.Validate() != nil {
		t.Fatalf("config=%#v err=%v", config, err)
	}
	for _, valid := range []string{"0", "1", "4294967295"} {
		if err := trusted.ValidateBonusReviewSelector(valid); err != nil {
			t.Fatalf("valid selector %q: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "01", "+1", "-1", "1 ", "10000000000", "name"} {
		if err := trusted.ValidateBonusReviewSelector(invalid); err == nil {
			t.Fatalf("invalid selector accepted: %q", invalid)
		}
	}
	custom := New("https://www.tjupt.org")
	if custom.Descriptor().Supports(domain.CapabilityBonusReview) {
		t.Fatal("non-exact origin declared bonus review")
	}
	if _, err := custom.BonusReviewConfig(); err == nil {
		t.Fatal("non-exact origin produced bonus review config")
	}
}

func TestBonusReviewHappyPathUsesOneReadAndHidesOpaqueFields(t *testing.T) {
	const cookie = "sid=COOKIE-CANARY"
	body := bonusReviewPage(`<tr><form action="?action=exchange" method="post">
<td><input type="hidden" name="option" value="1"><b>2</b></td>
<td>上传量兑换 10 GiB</td><td>1,000</td>
<td><input type="hidden" name="csrf" value="HIDDEN-FIELD-CANARY"><input type="submit" value="兑换"></td>
</form></tr>`)
	start := time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)
	client := &fakeGuardedClient{response: strictBonusResponse(body, start)}
	adapter := New("")
	adapter.newClient = func(baseURL, gotCookie string, interval time.Duration) (guardedClient, error) {
		if baseURL != DefaultBaseURL || gotCookie != cookie || interval != 2*time.Second {
			t.Fatalf("factory args base=%q cookie=%q interval=%s", baseURL, gotCookie, interval)
		}
		return client, nil
	}
	session, err := adapter.OpenBonusReviewSession(context.Background(), mustCredential(t, cookie))
	if err != nil {
		t.Fatal(err)
	}
	limits := site.DefaultBonusReviewLimits()
	observed, receipt, err := session.ReadBonusReview(context.Background(), "1", limits)
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 || session.RequestsMade() != 1 || client.path != bonusReviewPath || len(client.query) != 0 || client.accept != bonusReviewAccept || client.maxBody != limits.MaxResponseBytes || client.maxHeader != limits.MaxResponseHeaderBytes {
		t.Fatalf("path=%q query=%v accept=%q budgets=%d/%d calls=%d", client.path, client.query, client.accept, client.maxBody, client.maxHeader, client.calls)
	}
	review := observed.PublicCopy()
	if !observed.MatchesReceipt(receipt) || !observed.Matches("tjupt", "1", BonusReviewOrigin, BonusReviewRouteID) || review.Balance != "12345.67" || review.Availability != site.BonusReviewAvailabilityAvailable || review.InputMode != site.BonusReviewInputNone || review.ActionMethod != "post" || review.ActionRouteID != BonusExchangeRouteID || len(review.Columns) != 3 || !strings.HasPrefix(review.ReviewID, "sha256:") {
		t.Fatalf("review=%#v receipt=%#v", review, receipt)
	}
	if !receipt.Complete || receipt.StopReason != "" || receipt.Used.RequestsAttempted != 1 || receipt.Used.FormsExamined != 1 || receipt.Used.FieldsExamined != 3 || receipt.Used.TokensExamined <= 0 || receipt.Used.VisibleTextBytes <= 0 {
		t.Fatalf("receipt=%#v", receipt)
	}
	raw, _ := json.Marshal(struct {
		Review  domain.BonusOfferReview `json:"review"`
		Receipt site.BonusReviewReceipt `json:"receipt"`
	}{review, receipt})
	for _, secret := range []string{cookie, "HIDDEN-FIELD-CANARY", string(body)} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("serialized review disclosed secret %q", secret)
		}
	}
	if _, second, secondErr := session.ReadBonusReview(context.Background(), "1", limits); secondErr == nil || second.StopReason != "request_budget_exhausted" || client.calls != 1 {
		t.Fatalf("second=%#v err=%v calls=%d", second, secondErr, client.calls)
	}
}

func TestParseBonusReviewClassifiesInputAndAvailability(t *testing.T) {
	tests := []struct {
		name         string
		form         string
		availability string
		inputMode    string
		route        string
	}{
		{name: "direct", form: `<form action="mybonusapps.php" method="post"><input type="hidden" name="option" value="1"><td>A</td><td>100</td><input type="submit"></form>`, availability: site.BonusReviewAvailabilityAvailable, inputMode: site.BonusReviewInputNone, route: BonusExchangeRouteID},
		{name: "user input", form: `<form action="?action=exchange" method="post"><input type="hidden" name="option" value="1"><td>Gift</td><td>100</td><input name="username"><input type="submit"></form>`, availability: site.BonusReviewAvailabilityAvailable, inputMode: site.BonusReviewInputRequired, route: BonusExchangeRouteID},
		{name: "disabled", form: `<form action="?action=exchange" method="post"><input type="hidden" name="option" value="1"><td>VIP</td><td>5000</td><input type="submit" disabled></form>`, availability: site.BonusReviewAvailabilityDisabled, inputMode: site.BonusReviewInputNone, route: BonusExchangeRouteID},
		{name: "fieldset semantics", form: `<form action="?action=exchange" method="post"><fieldset><input type="hidden" name="option" value="1"><td>VIP</td><td>5000</td><input type="submit"></fieldset></form>`, availability: site.BonusReviewAvailabilityUnknown, inputMode: site.BonusReviewInputUnsupported, route: BonusExchangeRouteID},
		{name: "unmodeled object control", form: `<form action="?action=exchange" method="post"><input type="hidden" name="option" value="1"><td>VIP</td><td>5000</td><object name="opaque"></object><input type="submit"></form>`, availability: site.BonusReviewAvailabilityUnknown, inputMode: site.BonusReviewInputUnsupported, route: BonusExchangeRouteID},
		{name: "magic charset control", form: `<form action="?action=exchange" method="post"><input type="hidden" name="option" value="1"><td>VIP</td><td>5000</td><input type="hidden" name="_charset_" value="utf-8"><input type="submit"></form>`, availability: site.BonusReviewAvailabilityUnknown, inputMode: site.BonusReviewInputUnsupported, route: BonusExchangeRouteID},
		{name: "form associated custom element", form: `<form action="?action=exchange" method="post"><input type="hidden" name="option" value="1"><td>VIP</td><td>5000</td><bonus-token name="opaque"></bonus-token><input type="submit"></form>`, availability: site.BonusReviewAvailabilityUnknown, inputMode: site.BonusReviewInputUnsupported, route: BonusExchangeRouteID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			review, usage, err := parseBonusOfferReview(bonusReviewPage("<tr>"+test.form+"</tr>"), "1", site.DefaultBonusReviewLimits())
			if err != nil || review.Availability != test.availability || review.InputMode != test.inputMode || review.ActionRouteID != test.route || usage.forms != 1 {
				t.Fatalf("review=%#v usage=%#v err=%v", review, usage, err)
			}
		})
	}
}

func TestBonusReviewUsesInnermostRowAndOmitsScriptText(t *testing.T) {
	body := bonusReviewPage(`<tr><td>outer-a</td><td>outer-b</td><td><table><tr>
	<td>inner-&amp;amp;<script>VISIBLE-TEXT-CANARY</script></td><td>inner-b</td>
<td><form action="?action=exchange" method="post"><input type="hidden" name="option" value="1"><input type="submit"></form></td>
</tr></table></td></tr>`)
	review, _, err := parseBonusOfferReview(body, "1", site.DefaultBonusReviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(review.Columns) != 2 || review.Columns[0] != "inner-&amp;" || review.Columns[1] != "inner-b" {
		t.Fatalf("columns=%#v", review.Columns)
	}
	raw, _ := json.Marshal(review)
	if strings.Contains(string(raw), "VISIBLE-TEXT-CANARY") || strings.Contains(string(raw), "outer-a") {
		t.Fatalf("review retained non-offer text: %s", raw)
	}
}

func TestBonusReviewIDBindsSubmitterValueAndSuccessfulControlShape(t *testing.T) {
	base := `<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1">%s<input type="submit" name="action" value="%s"></form></tr>`
	parse := func(extra, submitValue string) domain.BonusOfferReview {
		t.Helper()
		review, _, err := parseBonusOfferReview(bonusReviewPage(fmt.Sprintf(base, extra, submitValue)), "1", site.DefaultBonusReviewLimits())
		if err != nil {
			t.Fatalf("parse bonus review: %v", err)
		}
		return review
	}
	original := parse(`<input type="hidden" name="token" value="opaque">`, "exchange")
	changedSubmit := parse(`<input type="hidden" name="token" value="opaque">`, "other")
	changedDisabledness := parse(`<input type="hidden" name="token" value="opaque" disabled>`, "exchange")
	if original.ReviewID == changedSubmit.ReviewID || original.ReviewID == changedDisabledness.ReviewID {
		t.Fatalf("review ID did not bind stable submit semantics: original=%s submit=%s disabled=%s", original.ReviewID, changedSubmit.ReviewID, changedDisabledness.ReviewID)
	}
}

func TestBonusReviewRequiresRealExactLogoutAnchor(t *testing.T) {
	form := `<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`
	for _, body := range [][]byte{
		[]byte(`<html><head><title>鍖楁磱鍥璓T :: Alice鐨勯瓟鍔涘€?- Powered by NexusPHP</title></head><body><script>"<a href='logout.php'>"</script><div>褰撳墠榄斿姏鍊硷細12,345.67</div><table>` + form + `</table></body></html>`),
		[]byte(`<html><head><title>鍖楁磱鍥璓T :: Alice鐨勯瓟鍔涘€?- Powered by NexusPHP</title></head><body><a href="logout.php?next=CANARY">logout</a><div>褰撳墠榄斿姏鍊硷細12,345.67</div><table>` + form + `</table></body></html>`),
	} {
		_, _, err := parseBonusOfferReview(body, "1", site.DefaultBonusReviewLimits())
		if err == nil || bonusParseStopReason(err) != "unrecognized_response" {
			t.Fatalf("reason=%q err=%v", bonusParseStopReason(err), err)
		}
	}
}

func TestBonusReviewFormShapeHidesValuesButBindsFieldNames(t *testing.T) {
	form := func(name, value string) []byte {
		return bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="hidden" name="` + name + `" value="` + value + `"><input type="submit"></form></tr>`)
	}
	first, _, err := parseBonusOfferReview(form("csrf", "FIRST-HIDDEN-CANARY"), "1", site.DefaultBonusReviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	changedValue, _, err := parseBonusOfferReview(form("csrf", "SECOND-HIDDEN-CANARY"), "1", site.DefaultBonusReviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	changedName, _, err := parseBonusOfferReview(form("nonce", "FIRST-HIDDEN-CANARY"), "1", site.DefaultBonusReviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	if first.FormShapeID != changedValue.FormShapeID || first.ReviewID != changedValue.ReviewID {
		t.Fatal("opaque hidden value changed public review identity")
	}
	if first.FormShapeID == changedName.FormShapeID || first.ReviewID == changedName.ReviewID {
		t.Fatal("hidden field name change did not change form/review identity")
	}
	raw, _ := json.Marshal([]domain.BonusOfferReview{first, changedValue, changedName})
	if strings.Contains(string(raw), "HIDDEN-CANARY") || strings.Contains(string(raw), `"csrf"`) || strings.Contains(string(raw), `"nonce"`) {
		t.Fatalf("public review disclosed hidden fields: %s", raw)
	}
}

func TestBonusReviewParserFailsClosedOnAmbiguityAndBudgets(t *testing.T) {
	valid := `<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`
	tests := []struct {
		name   string
		body   []byte
		limits site.BonusReviewLimits
		reason string
	}{
		{name: "missing", body: bonusReviewPage(valid), limits: site.DefaultBonusReviewLimits(), reason: "selector_not_found"},
		{name: "ambiguous", body: bonusReviewPage(valid + valid), limits: site.DefaultBonusReviewLimits(), reason: "selector_ambiguous"},
		{name: "duplicate option", body: bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "selector_ambiguous"},
		{name: "duplicate option in another form", body: bonusReviewPage(valid + `<tr><form action="?action=exchange" method="post"><td>B</td><td>200</td><input type="hidden" name="option" value="1"><input type="hidden" name="option" value="2"><input type="submit"></form></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "selector_ambiguous"},
		{name: "invalid target control in another form", body: bonusReviewPage(valid + `<tr><form action="?action=exchange" method="post"><td>B</td><td>200</td><input name="option" value="1"><input type="submit"></form></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "selector_ambiguous"},
		{name: "duplicate attribute", body: bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" name="option" value="1"><input type="submit"></form></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "invalid_html"},
		{name: "unknown action", body: bonusReviewPage(`<tr><form action="other.php" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "unrecognized_offer"},
		{name: "malformed action query", body: bonusReviewPage(`<tr><form action="?action=exchange&amp;bad=%zz" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "unrecognized_offer"},
		{name: "base URL override", body: bonusReviewPage(`<base href="https://example.invalid/"><tr>` + valid + `</tr>`), limits: site.DefaultBonusReviewLimits(), reason: "ambiguous_form_structure"},
		{name: "disabled selector", body: bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1" disabled><input type="submit"></form></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "selector_ambiguous"},
		{name: "external selector owner", body: bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1" form="other"><input type="submit"></form></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "ambiguous_form_structure"},
		{name: "external control outside form", body: bonusReviewPage(`<tr><form id="exchange" action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit"></form><input type="hidden" name="csrf" value="opaque" form="exchange"></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "ambiguous_form_structure"},
		{name: "non-exact selector name", body: bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name=" option " value="1"><input type="submit"></form></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "selector_not_found"},
		{name: "non-exact selector type", body: bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type=" hidden " name="option" value="1"><input type="submit"></form></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "selector_ambiguous"},
		{name: "submit action override", body: bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit" formaction="other.php"></form></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "unrecognized_offer"},
		{name: "command submitter", body: bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><button command="--exchange">Go</button></form></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "unrecognized_offer"},
		{name: "non-exact method", body: bonusReviewPage(`<tr><form action="?action=exchange" method=" post "><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "unrecognized_offer"},
		{name: "column retention overflow", body: bonusReviewPage(`<tr>` + strings.Repeat(`<td>x</td>`, 17) + `<form action="?action=exchange" method="post"><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`), limits: site.DefaultBonusReviewLimits(), reason: "unrecognized_offer"},
	}
	formLimited := site.DefaultBonusReviewLimits()
	formLimited.MaxForms = 1
	tests = append(tests, struct {
		name   string
		body   []byte
		limits site.BonusReviewLimits
		reason string
	}{name: "form budget", body: bonusReviewPage(valid + strings.ReplaceAll(valid, `value="1"`, `value="2"`)), limits: formLimited, reason: "form_budget_exceeded"})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selector := "1"
			if test.name == "missing" {
				selector = "2"
			}
			_, _, err := parseBonusOfferReview(test.body, selector, test.limits)
			if err == nil || bonusParseStopReason(err) != test.reason {
				t.Fatalf("reason=%q err=%v", bonusParseStopReason(err), err)
			}
		})
	}
}

func TestBonusReviewSessionErrorsAreSafe(t *testing.T) {
	adapter := New("")
	client := &fakeGuardedClient{err: fmt.Errorf("https://www.tjupt.org/mybonusapps.php Cookie=COOKIE-CANARY HIDDEN-FIELD-CANARY")}
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
	session, err := adapter.OpenBonusReviewSession(context.Background(), mustCredential(t, "sid=secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, receipt, err := session.ReadBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits()); err == nil || receipt.StopReason != "site_request_failed" || strings.Contains(err.Error(), "CANARY") || client.calls != 1 {
		t.Fatalf("receipt=%#v err=%v calls=%d", receipt, err, client.calls)
	}
}

func TestBonusReviewRejectsXHTMLSemantics(t *testing.T) {
	response := strictBonusResponse(bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`), time.Now().UTC())
	response.MediaType = "application/xhtml+xml"
	stop, err := classifyBonusReviewResponse(response)
	if err == nil || stop != "content_type_rejected" {
		t.Fatalf("stop=%q err=%v", stop, err)
	}
}

func bonusReviewPage(forms string) []byte {
	return []byte(`<html><head><title>北洋园PT :: Alice的魔力值 - Powered by NexusPHP</title></head><body><a href="logout.php">退出</a><div>当前魔力值：12,345.67</div><table>` + forms + `</table></body></html>`)
}

func strictBonusResponse(body []byte, start time.Time) httpguard.StrictResponse {
	return httpguard.StrictResponse{
		StatusCode: http.StatusOK, MediaType: "text/html", Body: body,
		ObservedAtStart: start, ObservedAtEnd: start.Add(time.Second),
		ResponseBytesRead: int64(len(body)), ResponseBytesKnown: true,
	}
}
