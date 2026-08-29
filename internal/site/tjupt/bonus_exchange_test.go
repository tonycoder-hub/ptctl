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

	readOnlyClient := &fakeGuardedClient{}
	missingWritePort := New("")
	missingWritePort.newClient = func(string, string, time.Duration) (guardedClient, error) {
		return readOnlyClient, nil
	}
	if session, err := missingWritePort.OpenBonusExchangeSession(context.Background(), mustCredential(t, "sid=secret")); err == nil || session != nil || readOnlyClient.closeCalls != 1 {
		t.Fatalf("session=%v err=%v closeCalls=%d", session, err, readOnlyClient.closeCalls)
	}
}

func TestBonusExchangeUsesFreshOpaqueFormExactlyOnce(t *testing.T) {
	const cookie = "sid=COOKIE-CANARY"
	const hidden = "HIDDEN-CSRF-CANARY"
	start := time.Date(2026, 8, 12, 4, 5, 6, 0, time.UTC)
	page := bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="hidden" name="csrf" value="` + hidden + `"><input type="submit" name="submit" value="Exchange!"></form></tr>`)
	client := &exchangeFakeClient{
		getResponse: strictBonusResponse(page, start),
		postResponse: httpguard.StrictResponse{
			StatusCode: http.StatusFound, ObservedAtStart: start.Add(2 * time.Second), ObservedAtEnd: start.Add(3 * time.Second),
			ResponseBytesKnown: true,
		},
		redirectLocation: "https://www.tjupt.org/mybonusapps.php?do=upload",
	}
	adapter := New("")
	adapter.newClient = func(baseURL, gotCookie string, interval time.Duration) (guardedClient, error) {
		if baseURL != DefaultBaseURL || gotCookie != cookie || interval != 2*time.Second {
			t.Fatalf("factory base=%q cookie=%q interval=%s", baseURL, gotCookie, interval)
		}
		return client, nil
	}
	session, err := adapter.OpenBonusExchangeSession(context.Background(), mustCredential(t, cookie))
	if err != nil {
		t.Fatal(err)
	}
	observed, reviewReceipt, err := session.ReadFreshBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits(), site.DefaultBonusExchangeLimits())
	if err != nil {
		t.Fatal(err)
	}
	review := observed.PublicCopy()
	authority, receipt, err := session.SubmitFreshBonusExchange(context.Background(), observed, review.ReviewID, site.DefaultBonusExchangeLimits())
	if err != nil || authority == nil || !authority.Verified() || !authority.MatchesReceipt(receipt) || site.ValidateBonusExchangeReceipt(receipt) != nil || receipt.Outcome != site.BonusExchangeOutcomeConfirmed ||
		receipt.ConfirmationCode != bonusSuccessRedirects["upload"] || session.RequestsMade() != 2 || client.postCalls != 1 ||
		client.postPath != bonusReviewPath || client.postQuery.Get("action") != "exchange" || len(client.postQuery) != 1 ||
		len(client.postFields) != 3 || client.postFields[0] != (httpguard.FormField{Name: "option", Value: "1"}) ||
		client.postFields[1] != (httpguard.FormField{Name: "csrf", Value: hidden}) || client.postFields[2] != (httpguard.FormField{Name: "submit", Value: "Exchange!"}) {
		t.Fatalf("reviewReceipt=%#v receipt=%#v err=%v fields=%#v", reviewReceipt, receipt, err, client.postFields)
	}
	raw, _ := json.Marshal(receipt)
	for _, secret := range []string{cookie, hidden, "mybonusapps.php"} {
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
			session, err := adapter.OpenBonusExchangeSession(context.Background(), mustCredential(t, "sid=secret"))
			if err != nil {
				t.Fatal(err)
			}
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
	page := bonusReviewPage(`<tr><form action="mybonusapps.php" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`)
	tests := []struct {
		name     string
		response httpguard.StrictResponse
		postErr  error
		redirect string
		outcome  string
		stop     string
	}{
		{name: "duplicate rejected", response: completeExchangeResponse(start, http.StatusFound), redirect: "https://www.tjupt.org/mybonusapps.php?do=duplicated", outcome: site.BonusExchangeOutcomeRejected, stop: "server_duplicate_lock"},
		{name: "malformed redirect query", response: completeExchangeResponse(start, http.StatusFound), redirect: "https://www.tjupt.org/mybonusapps.php?do=upload&bad=a;b", outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "unknown HTML", response: completeExchangeResponse(start, http.StatusOK), outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_response_unrecognized"},
		{name: "transport uncertain", response: httpguard.StrictResponse{ObservedAtStart: start.Add(time.Second), ObservedAtEnd: start.Add(2 * time.Second)}, postErr: errors.New("COOKIE-CANARY HIDDEN-CANARY"), outcome: site.BonusExchangeOutcomeUnknown, stop: "submission_transport_uncertain"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &exchangeFakeClient{getResponse: strictBonusResponse(page, start), postResponse: test.response, postErr: test.postErr, redirectLocation: test.redirect}
			adapter := New("")
			adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
			session, err := adapter.OpenBonusExchangeSession(context.Background(), mustCredential(t, "sid=secret"))
			if err != nil {
				t.Fatal(err)
			}
			observed, _, err := session.ReadFreshBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits(), site.DefaultBonusExchangeLimits())
			if err != nil {
				t.Fatal(err)
			}
			authority, receipt, submitErr := session.SubmitFreshBonusExchange(context.Background(), observed, observed.PublicCopy().ReviewID, site.DefaultBonusExchangeLimits())
			if submitErr == nil || receipt.Outcome != test.outcome || receipt.StopReason != test.stop || !receipt.RequestAttempted || client.postCalls != 1 ||
				authority == nil || !authority.MatchesReceipt(receipt) || site.ValidateBonusExchangeReceipt(receipt) != nil || strings.Contains(submitErr.Error(), "CANARY") {
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
	session, err := adapter.OpenBonusExchangeSession(context.Background(), mustCredential(t, "sid=secret"))
	if err != nil {
		t.Fatal(err)
	}
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
	session, err := adapter.OpenBonusExchangeSession(context.Background(), mustCredential(t, "sid=secret"))
	if err != nil {
		t.Fatal(err)
	}
	exchangeLimits := site.DefaultBonusExchangeLimits()
	exchangeLimits.MaxFormBytes = 1
	observed, receipt, err := session.ReadFreshBonusReview(context.Background(), "1", site.DefaultBonusReviewLimits(), exchangeLimits)
	if err == nil || observed != nil || receipt.Complete || receipt.StopReason != "submission_form_budget_exceeded" ||
		client.requests != 1 || client.postCalls != 0 {
		t.Fatalf("observed=%#v receipt=%#v err=%v client=%#v", observed, receipt, err, client)
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
	if _, err := adapter.OpenBonusExchangeSession(context.Background(), mustCredential(t, "sid=COOKIE-CANARY")); err == nil || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("err=%v", err)
	}
}
