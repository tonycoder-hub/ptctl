package mteam

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

// mallFixture is synthetic. It mirrors the shape of api/mall/list and covers
// every state a goods item can be in.
const mallFixture = `{"code":"0","message":"SUCCESS","data":[
	{"id":"6","type":"GLOBAL_FREE_SINGLE","points":"0","shelf":"YES","roles":[],"nameChs":"全站免费 CANARY","nameEng":"Global free CANARY",
		"description":"{freeDay} CANARY","extra":{"freeDay":"3"},"createdDate":"2026-01-01 00:00:00"},
	{"id":"1","type":"UPLOAD","points":"800","shelf":"YES","roles":[],"nameChs":"上传量 CANARY","nameEng":"Upload CANARY","extra":{"ratioLimitBonus":"4"}},
	{"id":"2","type":"INVITE","points":"80000","shelf":"YES","roles":[],"nameChs":"","nameEng":"Invite CANARY","extra":{}},
	{"id":"3","type":"TITLE","points":"5000","shelf":"YES","roles":[],"nameChs":"头衔 CANARY"},
	{"id":"5","type":"INVITE_BUY_BONUS","points":"80000","shelf":"YES","roles":[],"nameChs":"邀请换魔力 CANARY"},
	{"id":"8","type":"UPLOAD","points":"700","shelf":"NO","roles":[],"nameChs":"下架 CANARY"},
	{"id":"9","type":"INVITE","points":"1","shelf":"YES","roles":["VIP"],"nameChs":"限定 CANARY"}
]}`

const exchangeConfirmed = `{"code":"0","message":"SUCCESS","data":null}`

type scriptedStep struct {
	response httpguard.StrictResponse
	err      error
	// unsent leaves the request uncounted, as httpguard does when it refuses
	// before the request leaves the process.
	unsent bool
}

type scriptedCall struct {
	fakeCall
	effect bool
}

// scriptedClient answers each request with the next step and can also send
// the exchange POST.
type scriptedClient struct {
	steps  []scriptedStep
	calls  []scriptedCall
	closed bool
}

func (f *scriptedClient) next(call scriptedCall) (httpguard.StrictResponse, error) {
	if len(f.steps) == 0 {
		return httpguard.StrictResponse{}, errors.New("scripted client has no step left")
	}
	step := f.steps[0]
	f.steps = f.steps[1:]
	if !step.unsent {
		f.calls = append(f.calls, call)
	}
	return step.response, step.err
}

func (f *scriptedClient) PostReadOnce(_ context.Context, path, contentType string, body []byte, accept string, maxRequest, maxBody, maxHeader int64) (httpguard.StrictResponse, error) {
	return f.next(scriptedCall{fakeCall: fakeCall{path, contentType, accept, string(body), maxRequest, maxBody, maxHeader}})
}

func (f *scriptedClient) PostEffectOnce(_ context.Context, path, contentType string, body []byte, accept string, maxRequest, maxBody, maxHeader int64) (httpguard.StrictResponse, error) {
	return f.next(scriptedCall{fakeCall: fakeCall{path, contentType, accept, string(body), maxRequest, maxBody, maxHeader}, effect: true})
}

func (f *scriptedClient) RequestsMade() int { return len(f.calls) }

func (f *scriptedClient) Close() error {
	f.closed = true
	return nil
}

func scriptedAdapter(client guardedClient) (*Adapter, *factoryRecord) {
	record := &factoryRecord{}
	return &Adapter{newClient: func(baseURL, apiKey string, interval time.Duration) (guardedClient, error) {
		record.baseURL, record.apiKey, record.interval = baseURL, apiKey, interval
		record.opened++
		return client, nil
	}}, record
}

func observedStatus(status int, mediaType, body string) httpguard.StrictResponse {
	response := observedJSON(body)
	response.StatusCode, response.MediaType = status, mediaType
	return response
}

func steps(responses ...httpguard.StrictResponse) []scriptedStep {
	result := make([]scriptedStep, len(responses))
	for i, response := range responses {
		result[i] = scriptedStep{response: response}
	}
	return result
}

func TestBonusConfigsArePinned(t *testing.T) {
	adapter := New()
	review, err := adapter.BonusReviewConfig()
	if err != nil || review.Origin != "https://api2.m-team.cc" || review.RouteID != "mteam.mall_list_review.v1" {
		t.Fatalf("review config=%#v err=%v", review, err)
	}
	exchange, err := adapter.BonusExchangeConfig()
	if err != nil || exchange.Origin != "https://api2.m-team.cc" || exchange.ReviewRouteID != "mteam.mall_list_review.v1" ||
		exchange.ActionRouteID != "mteam.mall_exchange_post.v1" {
		t.Fatalf("exchange config=%#v err=%v", exchange, err)
	}
	if !site.IsBonusExchangeMessageConfirmation(bonusConfirmedCode) {
		t.Fatalf("confirmation code %q is not a message confirmation", bonusConfirmedCode)
	}
	for _, selector := range []string{"1", "7", "1234567890"} {
		if adapter.ValidateBonusReviewSelector(selector) != nil || adapter.ValidateBonusExchangeSelector(selector) != nil {
			t.Fatalf("selector %q rejected", selector)
		}
	}
	for _, selector := range []string{"", "0", "01", " 1", "1 ", "1\n", "-1", "1a", "12345678901"} {
		if adapter.ValidateBonusReviewSelector(selector) == nil || adapter.ValidateBonusExchangeSelector(selector) == nil {
			t.Fatalf("selector %q accepted", selector)
		}
	}
}

func TestBonusCatalogReadsBalanceThenMallOnOneClient(t *testing.T) {
	client := &scriptedClient{steps: steps(jsonResponse(profileFixture), jsonResponse(mallFixture))}
	adapter, record := scriptedAdapter(client)
	catalog, err := adapter.BonusCatalog(context.Background(), apiKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if record.opened != 1 || record.interval != minRequestInterval || !client.closed || len(client.calls) != 2 ||
		client.calls[0].path != profilePath || client.calls[1].path != mallListPath || client.calls[1].body != emptyFormBody ||
		client.calls[1].contentType != emptyFormMediaType || client.calls[1].effect {
		t.Fatalf("record=%#v calls=%#v closed=%t", record, client.calls, client.closed)
	}
	want := []domain.BonusCatalogRow{
		{Selector: "6", Columns: []string{"全站免费 CANARY", "GLOBAL_FREE_SINGLE", "points 0", "available, requires input"}},
		{Selector: "1", Columns: []string{"上传量 CANARY", "UPLOAD", "points 800 per GB", "available"}},
		{Selector: "2", Columns: []string{"Invite CANARY", "INVITE", "points 80000", "available"}},
		{Selector: "3", Columns: []string{"头衔 CANARY", "TITLE", "points 5000", "available, requires input"}},
		{Selector: "5", Columns: []string{"邀请换魔力 CANARY", "INVITE_BUY_BONUS", "points 80000", "unknown"}},
		{Selector: "8", Columns: []string{"下架 CANARY", "UPLOAD", "points 700 per GB", "disabled"}},
		{Selector: "9", Columns: []string{"限定 CANARY", "INVITE", "points 1", "unknown"}},
	}
	if catalog.SiteID != SiteID || catalog.Balance != "12345.6" || !reflect.DeepEqual(catalog.Rows, want) || catalog.ObservedAt.IsZero() {
		t.Fatalf("catalog=%#v", catalog)
	}
}

func TestBonusCatalogFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name  string
		mall  string
		calls int
	}{
		{name: "empty mall", mall: `{"code":"0","data":[]}`, calls: 2},
		{name: "mall object", mall: `{"code":"0","data":{"id":"1"}}`, calls: 2},
		{name: "goods without a name", mall: `{"code":"0","data":[{"id":"1","type":"UPLOAD","points":"800","shelf":"YES"}]}`, calls: 2},
		{name: "goods without a price", mall: `{"code":"0","data":[{"id":"1","type":"UPLOAD","shelf":"YES","nameChs":"CANARY"}]}`, calls: 2},
		{name: "goods with a bad ID", mall: `{"code":"0","data":[{"id":"01","type":"UPLOAD","points":"800","nameChs":"CANARY"}]}`, calls: 2},
		{name: "goods with a bad type", mall: `{"code":"0","data":[{"id":"1","type":"upload","points":"800","nameChs":"CANARY"}]}`, calls: 2},
		{name: "repeated goods", mall: `{"code":"0","data":[{"id":"1","type":"UPLOAD","points":"800","nameChs":"A"},{"id":"1","type":"UPLOAD","points":"800","nameChs":"B"}]}`, calls: 2},
		{name: "refused", mall: `{"code":"1","message":"CANARY refusal","data":null}`, calls: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &scriptedClient{steps: steps(jsonResponse(profileFixture), jsonResponse(test.mall))}
			adapter, _ := scriptedAdapter(client)
			catalog, err := adapter.BonusCatalog(context.Background(), apiKey(t))
			if err == nil || len(catalog.Rows) != 0 || len(client.calls) != test.calls || strings.Contains(err.Error(), "CANARY") {
				t.Fatalf("catalog=%#v err=%v calls=%d", catalog, err, len(client.calls))
			}
		})
	}
	// A rejected key stops before the mall is read.
	client := &scriptedClient{steps: steps(jsonResponse(`{"code":"401","message":"CANARY","data":null}`), jsonResponse(mallFixture))}
	adapter, _ := scriptedAdapter(client)
	if _, err := adapter.BonusCatalog(context.Background(), apiKey(t)); !errors.Is(err, site.ErrCredentialRejected) || len(client.calls) != 1 {
		t.Fatalf("err=%v calls=%d", err, len(client.calls))
	}
}

func openReview(t *testing.T, client guardedClient) site.BonusReviewSession {
	t.Helper()
	adapter, record := scriptedAdapter(client)
	session, err := adapter.OpenBonusReviewSession(context.Background(), apiKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if record.opened != 1 || record.baseURL != DefaultBaseURL || record.apiKey != testAPIKey || record.interval != minRequestInterval {
		t.Fatalf("factory=%#v", record)
	}
	return session
}

func TestBonusReviewReadsOneGoodsItemInOneRequest(t *testing.T) {
	client := &scriptedClient{steps: steps(observedJSON(mallFixture))}
	session := openReview(t, client)
	limits := site.DefaultBonusReviewLimits()
	observed, receipt, err := session.ReadBonusReview(context.Background(), "1", limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 1 || client.calls[0].path != mallListPath || client.calls[0].body != emptyFormBody || client.calls[0].effect ||
		client.calls[0].maxBody != limits.MaxResponseBytes || client.calls[0].maxHeader != limits.MaxResponseHeaderBytes {
		t.Fatalf("calls=%#v", client.calls)
	}
	review := observed.PublicCopy()
	if review.SiteID != SiteID || review.Selector != "1" || review.Balance != "" ||
		!reflect.DeepEqual(review.Columns, []string{"上传量 CANARY", "UPLOAD", "points 800 per GB"}) ||
		review.Availability != site.BonusReviewAvailabilityAvailable || review.InputMode != site.BonusReviewInputNone ||
		review.ActionMethod != "post" || review.ActionRouteID != BonusExchangeRouteID || site.ValidateBonusReviewID(review.ReviewID) != nil {
		t.Fatalf("review=%#v", review)
	}
	if !receipt.Complete || receipt.StopReason != "" || receipt.RouteID != BonusReviewRouteID || receipt.Origin != BonusReviewOrigin ||
		receipt.Used.RequestsAttempted != 1 || receipt.Used.FormsExamined != 7 || receipt.Used.FieldsExamined == 0 || receipt.Used.TokensExamined == 0 ||
		receipt.Used.VisibleTextBytes == 0 || !observed.MatchesReceipt(receipt) {
		t.Fatalf("receipt=%#v", receipt)
	}

	// A second read on the session is refused without a request.
	_, receipt, err = session.ReadBonusReview(context.Background(), "1", limits)
	if err == nil || receipt.StopReason != "request_budget_exhausted" || len(client.calls) != 1 {
		t.Fatalf("second read err=%v receipt=%#v", err, receipt)
	}
	if err := session.Close(); err != nil || !client.closed {
		t.Fatalf("close err=%v closed=%t", err, client.closed)
	}
}

func TestBonusReviewReportsWhatTheMallClaims(t *testing.T) {
	for _, test := range []struct {
		selector, availability, inputMode string
	}{
		{"2", site.BonusReviewAvailabilityAvailable, site.BonusReviewInputNone},
		{"3", site.BonusReviewAvailabilityAvailable, site.BonusReviewInputRequired},
		{"6", site.BonusReviewAvailabilityAvailable, site.BonusReviewInputRequired},
		{"5", site.BonusReviewAvailabilityUnknown, site.BonusReviewInputUnsupported},
		{"8", site.BonusReviewAvailabilityDisabled, site.BonusReviewInputNone},
		{"9", site.BonusReviewAvailabilityUnknown, site.BonusReviewInputNone},
	} {
		session := openReview(t, &scriptedClient{steps: steps(observedJSON(mallFixture))})
		observed, _, err := session.ReadBonusReview(context.Background(), test.selector, site.DefaultBonusReviewLimits())
		if err != nil {
			t.Fatalf("%s: %v", test.selector, err)
		}
		if review := observed.PublicCopy(); review.Availability != test.availability || review.InputMode != test.inputMode {
			t.Fatalf("%s: review=%#v", test.selector, review)
		}
	}

	// The review ID moves with the price, so an approved intent cannot be
	// spent at a different one.
	first := openReview(t, &scriptedClient{steps: steps(observedJSON(mallFixture))})
	second := openReview(t, &scriptedClient{steps: steps(observedJSON(strings.Replace(mallFixture, `"points":"800"`, `"points":"900"`, 1)))})
	a, _, errA := first.ReadBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits())
	b, _, errB := second.ReadBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits())
	if errA != nil || errB != nil || a.PublicCopy().ReviewID == b.PublicCopy().ReviewID || a.PublicCopy().FormShapeID == b.PublicCopy().FormShapeID {
		t.Fatalf("errA=%v errB=%v", errA, errB)
	}
}

func TestBonusReviewStopsWithoutEchoingServerText(t *testing.T) {
	small := func(change func(*site.BonusReviewLimits)) site.BonusReviewLimits {
		limits := site.DefaultBonusReviewLimits()
		change(&limits)
		return limits
	}
	for _, test := range []struct {
		name     string
		response httpguard.StrictResponse
		selector string
		limits   site.BonusReviewLimits
		stop     string
	}{
		{name: "http 401", response: observedStatus(http.StatusUnauthorized, "application/json", `{"code":"401"}`), stop: "authentication_required"},
		{name: "http 404", response: observedStatus(http.StatusNotFound, "text/html", "CANARY"), stop: "not_found"},
		{name: "http 429", response: observedStatus(http.StatusTooManyRequests, "text/html", "CANARY"), stop: "rate_limited"},
		{name: "redirect", response: observedStatus(http.StatusFound, "text/html", "CANARY"), stop: "redirect_rejected"},
		{name: "http 500", response: observedStatus(http.StatusInternalServerError, "text/html", "CANARY"), stop: "http_status_rejected"},
		{name: "html", response: observedStatus(http.StatusOK, "text/html", "<html>CANARY</html>"), stop: "content_type_rejected"},
		{name: "empty", response: observedJSON(""), stop: "empty_response"},
		{name: "not json", response: observedJSON("CANARY"), stop: "unrecognized_response"},
		{name: "code 401", response: observedJSON(`{"code":"401","message":"CANARY","data":null}`), stop: "authentication_required"},
		{name: "code 4", response: observedJSON(`{"code":"4","message":"CANARY","data":null}`), stop: "rate_limited"},
		{name: "code 1", response: observedJSON(`{"code":"1","message":"CANARY","data":null}`), stop: "unrecognized_response"},
		{name: "missing goods", response: observedJSON(mallFixture), selector: "99", stop: "selector_not_found"},
		{name: "repeated goods", response: observedJSON(`{"code":"0","data":[{"id":"1","type":"UPLOAD","points":"8","nameChs":"A"},{"id":"1","type":"UPLOAD","points":"8","nameChs":"B"}]}`), stop: "selector_ambiguous"},
		{name: "malformed goods", response: observedJSON(`{"code":"0","data":[{"id":"1","type":"UPLOAD","points":"8","nameChs":"A"},{"id":"2","type":"UPLOAD","nameChs":"B"}]}`), stop: "unrecognized_offer"},
		{name: "token budget", response: observedJSON(mallFixture), limits: small(func(l *site.BonusReviewLimits) { l.MaxTokens = 20 }), stop: "token_budget_exceeded"},
		{name: "form budget", response: observedJSON(mallFixture), limits: small(func(l *site.BonusReviewLimits) { l.MaxForms = 3 }), stop: "form_budget_exceeded"},
		{name: "field budget", response: observedJSON(mallFixture), limits: small(func(l *site.BonusReviewLimits) { l.MaxFields = 10 }), stop: "field_budget_exceeded"},
		{name: "text budget", response: observedJSON(mallFixture), limits: small(func(l *site.BonusReviewLimits) { l.MaxVisibleTextBytes = 40 }), stop: "visible_text_budget_exceeded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			selector, limits := test.selector, test.limits
			if selector == "" {
				selector = "1"
			}
			if limits == (site.BonusReviewLimits{}) {
				limits = site.DefaultBonusReviewLimits()
			}
			client := &scriptedClient{steps: steps(test.response)}
			session := openReview(t, client)
			observed, receipt, err := session.ReadBonusReview(context.Background(), selector, limits)
			if err == nil || observed != nil || receipt.Complete || receipt.StopReason != test.stop || len(client.calls) != 1 || strings.Contains(err.Error(), "CANARY") {
				t.Fatalf("observed=%v receipt=%#v err=%v", observed, receipt, err)
			}
		})
	}
}

func TestBonusReviewFailsBeforeAnyRequest(t *testing.T) {
	client := &scriptedClient{steps: steps(observedJSON(mallFixture))}
	session := openReview(t, client)
	if _, receipt, err := session.ReadBonusReview(context.Background(), "01", site.DefaultBonusReviewLimits()); err == nil || receipt.StopReason != "invalid_selector" {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	invalid := site.DefaultBonusReviewLimits()
	invalid.MaxRequests = 2
	if _, receipt, err := session.ReadBonusReview(context.Background(), "1", invalid); err == nil || receipt.StopReason != "invalid_limits" {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, receipt, err := session.ReadBonusReview(ctx, "1", site.DefaultBonusReviewLimits()); !errors.Is(err, context.Canceled) || receipt.StopReason != "context_done" {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	if len(client.calls) != 0 {
		t.Fatalf("calls=%#v", client.calls)
	}
}

func openExchange(t *testing.T, client guardedClient) site.BonusExchangeSession {
	t.Helper()
	adapter, record := scriptedAdapter(client)
	session, err := adapter.OpenBonusExchangeSession(context.Background(), apiKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if record.opened != 1 || record.apiKey != testAPIKey || record.interval != minRequestInterval {
		t.Fatalf("factory=%#v", record)
	}
	return session
}

func reviewForExchange(t *testing.T, session site.BonusExchangeSession, selector string) *site.ObservedBonusReview {
	t.Helper()
	observed, _, err := session.ReadFreshBonusReview(context.Background(), selector, site.DefaultBonusReviewLimits(), site.DefaultBonusExchangeLimits())
	if err != nil {
		t.Fatal(err)
	}
	return observed
}

func TestBonusExchangeSendsOneEffectPostAfterTheFreshReview(t *testing.T) {
	client := &scriptedClient{steps: steps(observedJSON(mallFixture), observedJSON(exchangeConfirmed))}
	session := openExchange(t, client)
	observed := reviewForExchange(t, session, "1")
	limits := site.DefaultBonusExchangeLimits()
	authority, receipt, err := session.SubmitFreshBonusExchange(context.Background(), observed, observed.PublicCopy().ReviewID, limits)
	if err != nil {
		t.Fatal(err)
	}
	wantBody := "--" + formBoundary + "\r\nContent-Disposition: form-data; name=\"goodsId\"\r\n\r\n1\r\n" +
		"--" + formBoundary + "\r\nContent-Disposition: form-data; name=\"num\"\r\n\r\n1\r\n" +
		"--" + formBoundary + "--\r\n"
	if len(client.calls) != 2 || client.calls[0].effect || !client.calls[1].effect || client.calls[1].path != mallExchangePath ||
		client.calls[1].contentType != emptyFormMediaType || client.calls[1].body != wantBody || client.calls[1].accept != apiAccept ||
		client.calls[1].maxRequest != limits.MaxFormBytes || client.calls[1].maxBody != limits.MaxResponseBytes {
		t.Fatalf("calls=%#v", client.calls)
	}
	if receipt.Outcome != site.BonusExchangeOutcomeConfirmed || receipt.ConfirmationCode != bonusConfirmedCode || receipt.StopReason != "" ||
		!receipt.RequestAttempted || !receipt.ResponseComplete || receipt.Used.TotalRequestsAttempted != 2 || receipt.Used.ReviewRequestsAttempted != 1 ||
		receipt.Used.SubmissionRequestsAttempted != 1 || receipt.Used.FormFieldsSubmitted != 2 || receipt.Used.FormBytesSubmitted != int64(len(wantBody)) ||
		!authority.MatchesReceipt(receipt) {
		t.Fatalf("receipt=%#v", receipt)
	}
	config, _ := New().BonusExchangeConfig()
	if !authority.Matches(SiteID, "1", observed.PublicCopy().ReviewID, config) {
		t.Fatal("authority does not match the exchange config")
	}

	// The session submits once.
	if _, receipt, err := session.SubmitFreshBonusExchange(context.Background(), observed, observed.PublicCopy().ReviewID, limits); err == nil ||
		receipt.StopReason != "fresh_review_authority_unavailable" || len(client.calls) != 2 {
		t.Fatalf("second submit receipt=%#v err=%v", receipt, err)
	}
}

func TestBonusExchangeOutcomes(t *testing.T) {
	for _, test := range []struct {
		name    string
		step    scriptedStep
		outcome string
		stop    string
	}{
		{name: "refused", step: scriptedStep{response: observedJSON(`{"code":"1","message":"CANARY","data":null}`)}, outcome: site.BonusExchangeOutcomeRejected, stop: "server_refused"},
		{name: "code 401", step: scriptedStep{response: observedJSON(`{"code":"401","message":"CANARY"}`)}, outcome: site.BonusExchangeOutcomeRejected, stop: "server_authentication_rejected"},
		{name: "http 401", step: scriptedStep{response: observedStatus(http.StatusUnauthorized, "text/html", "CANARY")}, outcome: site.BonusExchangeOutcomeRejected, stop: "server_authentication_rejected"},
		{name: "code 4", step: scriptedStep{response: observedJSON(`{"code":"4","message":"CANARY"}`)}, outcome: site.BonusExchangeOutcomeRejected, stop: "server_rate_limited"},
		{name: "http 429", step: scriptedStep{response: observedStatus(http.StatusTooManyRequests, "text/html", "CANARY")}, outcome: site.BonusExchangeOutcomeRejected, stop: "server_rate_limited"},
		{name: "http 500", step: scriptedStep{response: observedStatus(http.StatusInternalServerError, "text/html", "CANARY")}, outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "redirect", step: scriptedStep{response: observedStatus(http.StatusFound, "text/html", "")}, outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "html", step: scriptedStep{response: observedStatus(http.StatusOK, "text/html", "<p>CANARY</p>")}, outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "no code", step: scriptedStep{response: observedJSON(`{"message":"CANARY"}`)}, outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "transport after send", step: scriptedStep{err: errors.New("CANARY reset")}, outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_transport_uncertain"},
		{name: "not sent", step: scriptedStep{err: errors.New("CANARY refused"), unsent: true}, outcome: site.BonusExchangeOutcomeNotSubmitted, stop: "submission_not_sent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &scriptedClient{steps: []scriptedStep{{response: observedJSON(mallFixture)}, test.step}}
			session := openExchange(t, client)
			observed := reviewForExchange(t, session, "2")
			authority, receipt, err := session.SubmitFreshBonusExchange(context.Background(), observed, observed.PublicCopy().ReviewID, site.DefaultBonusExchangeLimits())
			if err == nil || authority == nil || !authority.MatchesReceipt(receipt) || receipt.Outcome != test.outcome || receipt.StopReason != test.stop ||
				receipt.ConfirmationCode != "" || strings.Contains(err.Error(), "CANARY") {
				t.Fatalf("receipt=%#v err=%v", receipt, err)
			}
		})
	}
}

func TestBonusExchangeRefusesBeforeTheEffect(t *testing.T) {
	// Goods that need input or are not available never get a form.
	for _, selector := range []string{"3", "5", "8", "9"} {
		client := &scriptedClient{steps: steps(observedJSON(mallFixture), observedJSON(exchangeConfirmed))}
		session := openExchange(t, client)
		observed, receipt, err := session.ReadFreshBonusReview(context.Background(), selector, site.DefaultBonusReviewLimits(), site.DefaultBonusExchangeLimits())
		if err == nil || observed != nil || receipt.StopReason != "submission_form_unavailable" || len(client.calls) != 1 {
			t.Fatalf("%s: receipt=%#v err=%v", selector, receipt, err)
		}
		if _, receipt, err := session.SubmitFreshBonusExchange(context.Background(), nil, strings.Repeat("0", 0)+"sha256:"+strings.Repeat("a", 64), site.DefaultBonusExchangeLimits()); err == nil ||
			receipt.StopReason != "fresh_review_authority_unavailable" || len(client.calls) != 1 {
			t.Fatalf("%s: submit receipt=%#v err=%v", selector, receipt, err)
		}
	}

	t.Run("mismatched review ID", func(t *testing.T) {
		client := &scriptedClient{steps: steps(observedJSON(mallFixture), observedJSON(exchangeConfirmed))}
		session := openExchange(t, client)
		observed := reviewForExchange(t, session, "1")
		_, receipt, err := session.SubmitFreshBonusExchange(context.Background(), observed, "sha256:"+strings.Repeat("b", 64), site.DefaultBonusExchangeLimits())
		if err == nil || receipt.StopReason != "fresh_review_mismatch" || len(client.calls) != 1 {
			t.Fatalf("receipt=%#v err=%v", receipt, err)
		}
	})
	t.Run("canceled before submission", func(t *testing.T) {
		client := &scriptedClient{steps: steps(observedJSON(mallFixture), observedJSON(exchangeConfirmed))}
		session := openExchange(t, client)
		observed := reviewForExchange(t, session, "1")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		authority, receipt, err := session.SubmitFreshBonusExchange(ctx, observed, observed.PublicCopy().ReviewID, site.DefaultBonusExchangeLimits())
		if !errors.Is(err, context.Canceled) || authority == nil || receipt.Outcome != site.BonusExchangeOutcomeNotSubmitted ||
			receipt.StopReason != "context_before_submission" || len(client.calls) != 1 {
			t.Fatalf("receipt=%#v err=%v", receipt, err)
		}
	})
	t.Run("different limits", func(t *testing.T) {
		client := &scriptedClient{steps: steps(observedJSON(mallFixture), observedJSON(exchangeConfirmed))}
		session := openExchange(t, client)
		observed := reviewForExchange(t, session, "1")
		limits := site.DefaultBonusExchangeLimits()
		limits.MaxFormBytes--
		_, receipt, err := session.SubmitFreshBonusExchange(context.Background(), observed, observed.PublicCopy().ReviewID, limits)
		if err == nil || receipt.StopReason != "fresh_review_authority_unavailable" || len(client.calls) != 1 {
			t.Fatalf("receipt=%#v err=%v", receipt, err)
		}
	})
	t.Run("one fresh review per session", func(t *testing.T) {
		client := &scriptedClient{steps: steps(observedJSON(mallFixture), observedJSON(mallFixture))}
		session := openExchange(t, client)
		reviewForExchange(t, session, "1")
		if _, _, err := session.ReadFreshBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits(), site.DefaultBonusExchangeLimits()); err == nil || len(client.calls) != 1 {
			t.Fatalf("err=%v calls=%d", err, len(client.calls))
		}
	})
	t.Run("closed session", func(t *testing.T) {
		client := &scriptedClient{steps: steps(observedJSON(mallFixture))}
		session := openExchange(t, client)
		if err := session.Close(); err != nil || !client.closed {
			t.Fatalf("close err=%v", err)
		}
		if _, _, err := session.ReadFreshBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits(), site.DefaultBonusExchangeLimits()); err == nil || len(client.calls) != 0 {
			t.Fatalf("err=%v calls=%d", err, len(client.calls))
		}
	})
}

func TestBonusSessionsRejectForeignCredentialsAndReadOnlyClients(t *testing.T) {
	cookie, err := site.NewCookieCredential("sid=CANARY")
	if err != nil {
		t.Fatal(err)
	}
	adapter, record := scriptedAdapter(&scriptedClient{})
	if _, err := adapter.OpenBonusReviewSession(context.Background(), cookie); err == nil {
		t.Fatal("review opened with a cookie")
	}
	if _, err := adapter.OpenBonusExchangeSession(context.Background(), cookie); err == nil {
		t.Fatal("exchange opened with a cookie")
	}
	if _, err := adapter.BonusCatalog(context.Background(), cookie); err == nil {
		t.Fatal("catalog read with a cookie")
	}
	if record.opened != 0 {
		t.Fatalf("factory opened %d clients", record.opened)
	}

	// A client that cannot send an effect POST never backs an exchange.
	readOnly := &fakeClient{}
	adapter, _ = scriptedAdapter(readOnly)
	if session, err := adapter.OpenBonusExchangeSession(context.Background(), apiKey(t)); err == nil || session != nil || !readOnly.closed {
		t.Fatalf("session=%v err=%v closed=%t", session, err, readOnly.closed)
	}
}
