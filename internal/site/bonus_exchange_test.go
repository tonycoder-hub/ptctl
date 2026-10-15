package site

import (
	"encoding/json"
	"fmt"
	"strings"
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

func TestAccountPasswordIsValidatedAndNeverFormatted(t *testing.T) {
	const canary = "PASSWORD-CANARY 密码"
	password, err := NewAccountPassword(canary)
	if err != nil || password.SecretValue() != canary {
		t.Fatalf("password err=%v", err)
	}
	holder := struct {
		label    string
		password AccountPassword
	}{label: "holder", password: password}
	raw, _ := json.Marshal(password)
	formatted := fmt.Sprintf("%v %+v %#v %s %v %+v %#v", password, password, password, password, holder, holder, holder) + string(raw)
	if strings.Contains(formatted, "PASSWORD-CANARY") {
		t.Fatalf("password was formatted: %s", formatted)
	}
	if (AccountPassword{}).SecretValue() != "" {
		t.Fatal("zero password has a value")
	}
	for _, invalid := range []string{"", "line\nbreak", "tab\tseparated", "del\x7f", string([]byte{0xff}), strings.Repeat("a", 1025)} {
		if _, err := NewAccountPassword(invalid); err == nil || strings.Contains(err.Error(), invalid) && invalid != "" {
			t.Fatalf("invalid password %q err=%v", invalid, err)
		}
	}
	if _, err := NewAccountPassword(strings.Repeat("a", 1024)); err != nil {
		t.Fatal(err)
	}
}
