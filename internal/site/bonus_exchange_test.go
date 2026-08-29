package site

import (
	"encoding/json"
	"testing"
	"time"
)

func TestObservedBonusExchangeAuthorityIsProcessLocal(t *testing.T) {
	receipt := validBonusExchangeReceiptForTest()
	observed, err := NewObservedBonusExchange(receipt)
	if err != nil {
		t.Fatal(err)
	}
	config := BonusExchangeConfig{Origin: receipt.Origin, ReviewRouteID: receipt.ReviewRouteID, ActionRouteID: receipt.ActionRouteID}
	if !observed.Verified() || !observed.MatchesReceipt(receipt) || !observed.Matches(receipt.SiteID, receipt.Selector, receipt.ExpectedReviewID, config) {
		t.Fatal("fresh observation did not retain authority")
	}

	raw, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	var replay ObservedBonusExchange
	if err := json.Unmarshal(raw, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Verified() || replay.MatchesReceipt(receipt) || replay.PublicCopy() != receipt {
		t.Fatalf("serialized observation regained authority: %#v", replay.PublicCopy())
	}
}

func TestObservedBonusExchangeRejectsContradictoryReceipt(t *testing.T) {
	receipt := validBonusExchangeReceiptForTest()
	receipt.Used.SubmissionRequestsAttempted = 0
	if _, err := NewObservedBonusExchange(receipt); err == nil {
		t.Fatal("contradictory receipt created process-local authority")
	}
}

func TestObservedBonusExchangeRejectsUnboundedStopText(t *testing.T) {
	receipt := validBonusExchangeReceiptForTest()
	receipt.Outcome = BonusExchangeOutcomeUnknown
	receipt.ConfirmationCode = ""
	receipt.StopReason = "COOKIE=CANARY"
	if _, err := NewObservedBonusExchange(receipt); err == nil {
		t.Fatal("untrusted stop text entered persistent exchange authority")
	}
	receipt.StopReason = "submission_transport_uncertain"
	if _, err := NewObservedBonusExchange(receipt); err != nil {
		t.Fatalf("stable stop code was rejected: %v", err)
	}
}

func validBonusExchangeReceiptForTest() BonusExchangeReceipt {
	start := time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)
	limits := DefaultBonusExchangeLimits()
	return BonusExchangeReceipt{
		Effect: BonusExchangeSubmitEffect, SiteID: "tjupt", Selector: "1",
		ExpectedReviewID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		FreshReviewID:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Origin:           "https://www.tjupt.org",
		ReviewRouteID:    "tjupt.bonus.review.v1",
		ActionRouteID:    "tjupt.bonus.exchange.v1",
		ObservedAtStart:  start, ObservedAtEnd: start.Add(time.Second), Complete: true,
		RequestAttempted: true, ResponseComplete: true, Outcome: BonusExchangeOutcomeConfirmed,
		ConfirmationCode: "tjupt.bonus_exchange.confirm.upload.v1", Limits: limits,
		Used: BonusExchangeUsage{
			TotalRequestsAttempted: 2, ReviewRequestsAttempted: 1, SubmissionRequestsAttempted: 1,
			FormFieldsSubmitted: 2, FormBytesSubmitted: 24, ResponseBytesKnown: true,
		},
	}
}
