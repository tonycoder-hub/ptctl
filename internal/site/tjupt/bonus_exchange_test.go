package tjupt

import (
	"context"
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

type exchangeFakeClient struct {
	getResponse      httpguard.StrictResponse
	getErr           error
	postResponse     httpguard.StrictResponse
	postErr          error
	redirectLocation string
	requests         int
	postCalls        int
	postPath         string
	postQuery        url.Values
	postFields       []httpguard.FormField
	closed           int
}

func (*exchangeFakeClient) Get(context.Context, string, url.Values) ([]byte, *url.URL, error) {
	panic("unexpected generic GET")
}

func (client *exchangeFakeClient) GetOnce(context.Context, string, url.Values, string, int64, int64) (httpguard.StrictResponse, error) {
	client.requests++
	return client.getResponse, client.getErr
}

func (client *exchangeFakeClient) PostFormOnce(_ context.Context, path string, query url.Values, fields []httpguard.FormField, _ string, _ int, _ int64, _ int64, _ int64, classify httpguard.RedirectClassifier) (httpguard.StrictResponse, error) {
	client.requests++
	client.postCalls++
	client.postPath = path
	client.postQuery = cloneTestValues(query)
	client.postFields = append([]httpguard.FormField(nil), fields...)
	response := client.postResponse
	response.RequestFields = len(fields)
	response.RequestBytes = int64(len(encodeTestForm(fields)))
	if response.ObservedAtStart.IsZero() {
		response.ObservedAtStart = time.Now().UTC()
	}
	if response.ObservedAtEnd.IsZero() {
		response.ObservedAtEnd = response.ObservedAtStart.Add(time.Millisecond)
	}
	if client.redirectLocation != "" && classify != nil {
		target, _ := url.Parse(client.redirectLocation)
		response.RedirectID, _ = classify(target)
	}
	return response, client.postErr
}

func (client *exchangeFakeClient) RequestsMade() int { return client.requests }
func (client *exchangeFakeClient) Close() error {
	client.closed++
	return nil
}

const exchangeTestPassword = "PASSWORD-CANARY"

func mustAccountPassword(t *testing.T, value string) site.AccountPassword {
	t.Helper()
	password, err := site.NewAccountPassword(value)
	if err != nil {
		t.Fatal(err)
	}
	return password
}

func openExchangeTestSession(t *testing.T, adapter *Adapter, cookie string) site.BonusExchangeSession {
	t.Helper()
	session, err := adapter.OpenPasswordConfirmedBonusExchangeSession(context.Background(), mustCredential(t, cookie), mustAccountPassword(t, exchangeTestPassword))
	if err != nil {
		t.Fatal(err)
	}
	return session
}

// bonusMessagePage mirrors the authenticated page TJUPT returns for an
// exchange POST: the site header, one stdmsg() box, and the footer.
func bonusMessagePage(heading, text string) []byte {
	return []byte(`<html><head><title>北洋园PT :: 魔力值 - Powered by NexusPHP</title></head><body><a href="logout.php">退出</a>` +
		`<table align="center" class="main" width="500" border="0" cellpadding="0" cellspacing="0"><tr><td class="embedded">` + "\n" +
		`<h2>` + heading + `</h2>` + "\n" +
		`<table width="100%" border="1" cellspacing="0" cellpadding="10"><tr><td class="text">` + text + `</td></tr></table></td></tr></table>` + "\n" +
		`</body></html>`)
}

func TestBonusExchangeRequiresTheAccountPassword(t *testing.T) {
	adapter := New("")
	factoryCalls := 0
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) {
		factoryCalls++
		return &exchangeFakeClient{}, nil
	}
	if !adapter.Descriptor().Supports(domain.CapabilityBonusExchange) || !adapter.Descriptor().Supports(domain.CapabilityBonusReview) {
		t.Fatal("production adapter did not declare bonus review and exchange")
	}
	if err := adapter.ValidateBonusExchangeSelector("1"); err != nil {
		t.Fatal(err)
	}
	if session, err := adapter.OpenBonusExchangeSession(context.Background(), mustCredential(t, "sid=secret")); err == nil || session != nil ||
		!strings.Contains(err.Error(), "password") || factoryCalls != 0 {
		t.Fatalf("session=%v err=%v factoryCalls=%d", session, err, factoryCalls)
	}
	if session, err := adapter.OpenPasswordConfirmedBonusExchangeSession(context.Background(), mustCredential(t, "sid=secret"), site.AccountPassword{}); err == nil || session != nil || factoryCalls != 0 {
		t.Fatalf("empty password session=%v err=%v factoryCalls=%d", session, err, factoryCalls)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if session, err := adapter.OpenPasswordConfirmedBonusExchangeSession(canceled, mustCredential(t, "sid=secret"), mustAccountPassword(t, exchangeTestPassword)); err == nil || session != nil || factoryCalls != 0 {
		t.Fatalf("canceled session=%v err=%v factoryCalls=%d", session, err, factoryCalls)
	}
}

func TestBonusExchangeCapabilityAndConfigAreCredentialFree(t *testing.T) {
	trusted := New("")
	if !trusted.Descriptor().Supports(domain.CapabilityBonusExchange) {
		t.Fatal("trusted production adapter did not declare bonus exchange")
	}
	config, err := trusted.BonusExchangeConfig()
	if err != nil || config.Origin != BonusReviewOrigin || config.ReviewRouteID != BonusReviewRouteID || config.ActionRouteID != BonusExchangeRouteID || config.Validate() != nil {
		t.Fatalf("config=%#v err=%v", config, err)
	}
	if err := trusted.ValidateBonusExchangeSelector("1"); err != nil {
		t.Fatal(err)
	}
	if err := trusted.ValidateBonusExchangeSelector("01"); err == nil {
		t.Fatal("non-canonical selector accepted")
	}
	custom := New("https://www.tjupt.org")
	if custom.Descriptor().Supports(domain.CapabilityBonusExchange) {
		t.Fatal("custom origin declared bonus exchange")
	}
	if _, err := custom.BonusExchangeConfig(); err == nil {
		t.Fatal("custom origin returned bonus exchange config")
	}
	if session, err := custom.OpenPasswordConfirmedBonusExchangeSession(context.Background(), mustCredential(t, "sid=secret"), mustAccountPassword(t, exchangeTestPassword)); err == nil || session != nil {
		t.Fatalf("custom origin session=%v err=%v", session, err)
	}

	readOnlyClient := &fakeGuardedClient{}
	missingWritePort := New("")
	missingWritePort.newClient = func(string, string, time.Duration) (guardedClient, error) {
		return readOnlyClient, nil
	}
	if session, err := missingWritePort.OpenPasswordConfirmedBonusExchangeSession(context.Background(), mustCredential(t, "sid=secret"), mustAccountPassword(t, exchangeTestPassword)); err == nil || session != nil || readOnlyClient.closeCalls != 1 {
		t.Fatalf("session=%v err=%v closeCalls=%d", session, err, readOnlyClient.closeCalls)
	}
}

func TestBonusExchangeUsesFreshOpaqueFormExactlyOnce(t *testing.T) {
	const cookie = "sid=COOKIE-CANARY"
	const hidden = "HIDDEN-CSRF-CANARY"
	start := time.Date(2026, 8, 12, 4, 5, 6, 0, time.UTC)
	page := bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="hidden" name="csrf" value="` + hidden + `"><input type="submit" name="submit" value="Exchange!"></form></tr>`)
	client := &exchangeFakeClient{
		getResponse:  strictBonusResponse(page, start),
		postResponse: strictBonusResponse(bonusMessagePage("成功", "享受无广告的时间！"), start.Add(2*time.Second)),
	}
	adapter := New("")
	adapter.newClient = func(baseURL, gotCookie string, interval time.Duration) (guardedClient, error) {
		if baseURL != DefaultBaseURL || gotCookie != cookie || interval != 2*time.Second {
			t.Fatalf("factory base=%q cookie=%q interval=%s", baseURL, gotCookie, interval)
		}
		return client, nil
	}
	session := openExchangeTestSession(t, adapter, cookie)
	observed, reviewReceipt, err := session.ReadFreshBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits(), site.DefaultBonusExchangeLimits())
	if err != nil {
		t.Fatal(err)
	}
	review := observed.PublicCopy()
	authority, receipt, err := session.SubmitFreshBonusExchange(context.Background(), observed, review.ReviewID, site.DefaultBonusExchangeLimits())
	if err != nil || authority == nil || !authority.Verified() || !authority.MatchesReceipt(receipt) || site.ValidateBonusExchangeReceipt(receipt) != nil || receipt.Outcome != site.BonusExchangeOutcomeConfirmed ||
		receipt.ConfirmationCode != bonusMessageConfirmed || session.RequestsMade() != 2 || client.postCalls != 1 ||
		client.postPath != bonusReviewPath || client.postQuery.Get("action") != "exchange" || len(client.postQuery) != 1 ||
		len(client.postFields) != 4 || client.postFields[0] != (httpguard.FormField{Name: "option", Value: "1"}) ||
		client.postFields[1] != (httpguard.FormField{Name: "csrf", Value: hidden}) || client.postFields[2] != (httpguard.FormField{Name: "submit", Value: "Exchange!"}) ||
		client.postFields[3] != (httpguard.FormField{Name: "passwd", Value: exchangeTestPassword}) {
		t.Fatalf("reviewReceipt=%#v receipt=%#v err=%v fields=%d", reviewReceipt, receipt, err, len(client.postFields))
	}
	// The receipt counts the password field but leaves its bytes out.
	if receipt.Used.FormFieldsSubmitted != 4 || receipt.Used.FormBytesSubmitted != int64(len(encodeTestForm(client.postFields[:3]))) {
		t.Fatalf("usage=%#v", receipt.Used)
	}
	raw, _ := json.Marshal(receipt)
	for _, secret := range []string{cookie, hidden, exchangeTestPassword, "mybonus.php"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("receipt disclosed secret %q: %s", secret, raw)
		}
	}
	secondAuthority, second, secondErr := session.SubmitFreshBonusExchange(context.Background(), observed, review.ReviewID, site.DefaultBonusExchangeLimits())
	if secondErr == nil || secondAuthority != nil || second.RequestAttempted || client.postCalls != 1 {
		t.Fatalf("second=%#v err=%v postCalls=%d", second, secondErr, client.postCalls)
	}
	if err := session.Close(); err != nil || client.closed != 1 {
		t.Fatalf("close err=%v calls=%d", err, client.closed)
	}
}

func TestBonusExchangeRejectsMismatchedOrSerializedReviewBeforePOST(t *testing.T) {
	start := time.Now().UTC()
	page := bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`)
	for _, test := range []struct {
		name         string
		mutate       func(*site.ObservedBonusReview, domain.BonusOfferReview) (*site.ObservedBonusReview, string)
		submitLimits func(site.BonusExchangeLimits) site.BonusExchangeLimits
	}{
		{name: "wrong expected ID", mutate: func(observed *site.ObservedBonusReview, review domain.BonusOfferReview) (*site.ObservedBonusReview, string) {
			return observed, "sha256:" + strings.Repeat("0", 64)
		}},
		{name: "serialized authority", mutate: func(observed *site.ObservedBonusReview, review domain.BonusOfferReview) (*site.ObservedBonusReview, string) {
			raw, _ := json.Marshal(observed)
			var decoded site.ObservedBonusReview
			_ = json.Unmarshal(raw, &decoded)
			return &decoded, review.ReviewID
		}},
		{name: "different exchange limits", mutate: func(observed *site.ObservedBonusReview, review domain.BonusOfferReview) (*site.ObservedBonusReview, string) {
			return observed, review.ReviewID
		}, submitLimits: func(limits site.BonusExchangeLimits) site.BonusExchangeLimits {
			limits.MaxFormBytes--
			return limits
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &exchangeFakeClient{getResponse: strictBonusResponse(page, start)}
			adapter := New("")
			adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
			session := openExchangeTestSession(t, adapter, "sid=secret")
			observed, _, err := session.ReadFreshBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits(), site.DefaultBonusExchangeLimits())
			if err != nil {
				t.Fatal(err)
			}
			supplied, expected := test.mutate(observed, observed.PublicCopy())
			submitLimits := site.DefaultBonusExchangeLimits()
			if test.submitLimits != nil {
				submitLimits = test.submitLimits(submitLimits)
			}
			authority, receipt, err := session.SubmitFreshBonusExchange(context.Background(), supplied, expected, submitLimits)
			if err == nil || authority != nil || receipt.RequestAttempted || client.postCalls != 0 || session.RequestsMade() != 1 {
				t.Fatalf("receipt=%#v err=%v requests=%d", receipt, err, session.RequestsMade())
			}
		})
	}
}

func TestBonusExchangePersistsConservativeResponseLattice(t *testing.T) {
	start := time.Now().UTC()
	page := bonusReviewPage(`<tr><form action="mybonus.php" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`)
	message := func(heading, text string) httpguard.StrictResponse {
		return strictBonusResponse(bonusMessagePage(heading, text), start.Add(time.Second))
	}
	body := func(raw string) httpguard.StrictResponse {
		return strictBonusResponse([]byte(raw), start.Add(time.Second))
	}
	const confirmForm = `<form action="?action=exchange" method="post"><input type="hidden" name="option" value=1 />你正在使用 <b>100</b> 个魔力值兑换。<br />请输入你的密码确认上述操作：<input type=password name=passwd /><input type=submit value="确定"> &nbsp;<input type=button value="返回" onclick="location.href='javascript:history.go(-1)'" /></form>`
	const backButton = `<input type=button value="返回上一页" onclick="location.href='javascript:history.go(-1)'" />`
	withoutLogout := strictBonusResponse([]byte(strings.Replace(string(bonusMessagePage("成功", "享受无广告的时间！")), `<a href="logout.php">退出</a>`, "", 1)), start.Add(time.Second))
	twoHeadings := strictBonusResponse([]byte(strings.Replace(string(bonusMessagePage("成功", "享受无广告的时间！")), "<body>", "<body><h2>公告</h2>", 1)), start.Add(time.Second))
	notHTML := message("成功", "享受无广告的时间！")
	notHTML.MediaType = "application/json"
	partial := message("成功", "享受无广告的时间！")
	partial.ResponseBytesKnown = false
	tests := []struct {
		name     string
		response httpguard.StrictResponse
		postErr  error
		redirect string
		outcome  string
		stop     string
		code     string
	}{
		{name: "success message confirmed", response: message("成功", "享受无广告的时间！"), outcome: site.BonusExchangeOutcomeConfirmed, code: bonusMessageConfirmed},
		{name: "success redirect confirmed", response: completeExchangeResponse(start, http.StatusFound), redirect: "https://www.tjupt.org/mybonus.php?do=upload", outcome: site.BonusExchangeOutcomeConfirmed, code: bonusSuccessRedirects["upload"]},
		{name: "wrong password rejected", response: message("出错了！", "密码不正确"+backButton), outcome: site.BonusExchangeOutcomeRejected, stop: "server_password_rejected"},
		{name: "password requested again rejected", response: message("确认操作", confirmForm), outcome: site.BonusExchangeOutcomeRejected, stop: "server_password_required"},
		{name: "price change rejected", response: message("兑换价格有变化", "由于物价变动，当前价格产生了变化。"), outcome: site.BonusExchangeOutcomeRejected, stop: "server_price_changed"},
		{name: "confirmation without password input", response: message("确认操作", "请返回"), outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "other error message", response: message("出错了！", "未知错误"+backButton), outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "generic error heading", response: message("错误", "想作弊？没门！"), outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "success without logout link", response: withoutLogout, outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "success beside another heading", response: twoHeadings, outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "success as another media type", response: notHTML, outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "partial success body", response: partial, outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "bare die text", response: body(`<html><body><a href="logout.php">退出</a>想作弊？没门！</body></html>`), outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "login page", response: body(`<html><body><h2>成功</h2><table><tr><td class="text"><a href="logout.php">x</a><input name="username"><input name="password"></td></tr></table></body></html>`), outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "duplicate rejected", response: completeExchangeResponse(start, http.StatusFound), redirect: "https://www.tjupt.org/mybonus.php?do=duplicated", outcome: site.BonusExchangeOutcomeRejected, stop: "server_duplicate_lock"},
		{name: "malformed redirect query", response: completeExchangeResponse(start, http.StatusFound), redirect: "https://www.tjupt.org/mybonus.php?do=upload&bad=a;b", outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "unknown HTML", response: completeExchangeResponse(start, http.StatusOK), outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "transport uncertain", response: httpguard.StrictResponse{ObservedAtStart: start.Add(time.Second), ObservedAtEnd: start.Add(2 * time.Second)}, postErr: errors.New("COOKIE-CANARY HIDDEN-CANARY"), outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_transport_uncertain"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &exchangeFakeClient{getResponse: strictBonusResponse(page, start), postResponse: test.response, postErr: test.postErr, redirectLocation: test.redirect}
			adapter := New("")
			adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
			session := openExchangeTestSession(t, adapter, "sid=secret")
			observed, _, err := session.ReadFreshBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits(), site.DefaultBonusExchangeLimits())
			if err != nil {
				t.Fatal(err)
			}
			authority, receipt, submitErr := session.SubmitFreshBonusExchange(context.Background(), observed, observed.PublicCopy().ReviewID, site.DefaultBonusExchangeLimits())
			confirmed := test.outcome == site.BonusExchangeOutcomeConfirmed
			if (submitErr == nil) != confirmed || receipt.Outcome != test.outcome || receipt.StopReason != test.stop || receipt.ConfirmationCode != test.code ||
				!receipt.RequestAttempted || client.postCalls != 1 || authority == nil || !authority.MatchesReceipt(receipt) || site.ValidateBonusExchangeReceipt(receipt) != nil ||
				(submitErr != nil && strings.Contains(submitErr.Error(), "CANARY")) {
				t.Fatalf("receipt=%#v err=%v", receipt, submitErr)
			}
		})
	}
}

func TestBonusExchangeRequiredInputNeverProducesSubmissionAuthority(t *testing.T) {
	page := bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>Gift</td><td>100</td><input type="hidden" name="option" value="1"><input name="username"><input type="submit"></form></tr>`)
	client := &exchangeFakeClient{getResponse: strictBonusResponse(page, time.Now().UTC())}
	adapter := New("")
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
	session := openExchangeTestSession(t, adapter, "sid=secret")
	observed, receipt, err := session.ReadFreshBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits(), site.DefaultBonusExchangeLimits())
	if err == nil || observed != nil || receipt.Complete || receipt.StopReason != "submission_form_unavailable" || client.requests != 1 || client.postCalls != 0 {
		t.Fatalf("observed=%#v receipt=%#v err=%v client=%#v", observed, receipt, err, client)
	}
}

func TestBonusExchangeFormBudgetFailsBeforeAttemptAuthority(t *testing.T) {
	page := bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`)
	client := &exchangeFakeClient{getResponse: strictBonusResponse(page, time.Now().UTC())}
	adapter := New(DefaultBaseURL)
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
	session := openExchangeTestSession(t, adapter, "sid=secret")
	exchangeLimits := site.DefaultBonusExchangeLimits()
	exchangeLimits.MaxFormBytes = 1
	observed, receipt, err := session.ReadFreshBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits(), exchangeLimits)
	if err == nil || observed != nil || receipt.Complete || receipt.StopReason != "submission_form_budget_exceeded" ||
		client.requests != 1 || client.postCalls != 0 {
		t.Fatalf("observed=%#v receipt=%#v err=%v client=%#v", observed, receipt, err, client)
	}
}

func TestBonusExchangeBudgetCountsThePassword(t *testing.T) {
	page := bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`)
	client := &exchangeFakeClient{getResponse: strictBonusResponse(page, time.Now().UTC())}
	adapter := New(DefaultBaseURL)
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
	session := openExchangeTestSession(t, adapter, "sid=secret")
	exchangeLimits := site.DefaultBonusExchangeLimits()
	// option=1 fits; option=1&passwd=PASSWORD-CANARY does not.
	exchangeLimits.MaxFormBytes = int64(len("option=1")) + 4
	observed, receipt, err := session.ReadFreshBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits(), exchangeLimits)
	if err == nil || observed != nil || receipt.Complete || receipt.StopReason != "submission_form_budget_exceeded" ||
		client.requests != 1 || client.postCalls != 0 || strings.Contains(err.Error(), exchangeTestPassword) {
		t.Fatalf("observed=%#v receipt=%#v err=%v requests=%d postCalls=%d", observed, receipt, err, client.requests, client.postCalls)
	}
}

func TestBonusExchangeRefusesAReviewedFormCarryingAPassword(t *testing.T) {
	page := bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="hidden" name="passwd" value="FORM-CANARY"><input type="submit"></form></tr>`)
	client := &exchangeFakeClient{getResponse: strictBonusResponse(page, time.Now().UTC())}
	adapter := New("")
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
	session := openExchangeTestSession(t, adapter, "sid=secret")
	observed, receipt, err := session.ReadFreshBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits(), site.DefaultBonusExchangeLimits())
	if err == nil || observed != nil || receipt.Complete || client.postCalls != 0 || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("observed=%#v receipt=%#v err=%v postCalls=%d", observed, receipt, err, client.postCalls)
	}
}

func TestBonusExchangeTransportErrorsDoNotEchoThePassword(t *testing.T) {
	start := time.Now().UTC()
	page := bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`)
	client := &exchangeFakeClient{
		getResponse:  strictBonusResponse(page, start),
		postResponse: httpguard.StrictResponse{ObservedAtStart: start.Add(time.Second), ObservedAtEnd: start.Add(2 * time.Second)},
		postErr:      errors.New("passwd=" + exchangeTestPassword),
	}
	adapter := New("")
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
	session := openExchangeTestSession(t, adapter, "sid=secret")
	observed, _, err := session.ReadFreshBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits(), site.DefaultBonusExchangeLimits())
	if err != nil {
		t.Fatal(err)
	}
	_, receipt, err := session.SubmitFreshBonusExchange(context.Background(), observed, observed.PublicCopy().ReviewID, site.DefaultBonusExchangeLimits())
	raw, _ := json.Marshal(receipt)
	if err == nil || strings.Contains(err.Error(), exchangeTestPassword) || strings.Contains(string(raw), exchangeTestPassword) ||
		strings.Contains(fmt.Sprintf("%v %+v %#v", session, session, session), exchangeTestPassword) {
		t.Fatalf("receipt=%s err=%v", raw, err)
	}
}

func completeExchangeResponse(start time.Time, status int) httpguard.StrictResponse {
	return httpguard.StrictResponse{
		StatusCode: status, ObservedAtStart: start.Add(time.Second), ObservedAtEnd: start.Add(2 * time.Second),
		ResponseBytesKnown: true,
	}
}

func cloneTestValues(input url.Values) url.Values {
	result := make(url.Values, len(input))
	for key, values := range input {
		result[key] = append([]string(nil), values...)
	}
	return result
}

func encodeTestForm(fields []httpguard.FormField) string {
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		parts = append(parts, url.QueryEscape(field.Name)+"="+url.QueryEscape(field.Value))
	}
	return strings.Join(parts, "&")
}

func TestBonusExchangeOpenErrorsDoNotEchoCredential(t *testing.T) {
	adapter := New("")
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) {
		return nil, fmt.Errorf("COOKIE-CANARY")
	}
	if _, err := adapter.OpenPasswordConfirmedBonusExchangeSession(context.Background(), mustCredential(t, "sid=COOKIE-CANARY"), mustAccountPassword(t, exchangeTestPassword)); err == nil || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("err=%v", err)
	}
}
