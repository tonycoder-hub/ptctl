package site

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
)

func TestBonusReviewLimitsConfigAndIdentity(t *testing.T) {
	limits := DefaultBonusReviewLimits()
	if err := limits.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := limits
	bad.MaxRequests = 2
	if bad.Validate() == nil {
		t.Fatal("multi-request bonus review limits accepted")
	}
	if _, err := NewBonusReviewConfig("https://www.tjupt.org", "tjupt.bonus.review.v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBonusReviewConfig("http://www.tjupt.org", "tjupt.bonus.review.v1"); err == nil {
		t.Fatal("insecure origin accepted")
	}

	review := validBonusReviewFixture(t)
	originalID := review.ReviewID
	review.Balance = "99999.5"
	changedBalanceID, err := ComputeBonusReviewID(review)
	if err != nil {
		t.Fatal(err)
	}
	if changedBalanceID != originalID {
		t.Fatal("balance unexpectedly changed semantic review identity")
	}
	review.Columns[1] = "different visible price"
	changedOfferID, err := ComputeBonusReviewID(review)
	if err != nil {
		t.Fatal(err)
	}
	if changedOfferID == originalID {
		t.Fatal("visible offer change did not change review identity")
	}
}

func TestObservedBonusReviewAuthorityIsProcessLocal(t *testing.T) {
	review := validBonusReviewFixture(t)
	start := time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)
	limits := DefaultBonusReviewLimits()
	receipt := BonusReviewReceipt{
		Effect: BonusReviewReadEffect, SiteID: "tjupt", Selector: "1",
		Origin: "https://www.tjupt.org", RouteID: "tjupt.bonus.review.v1",
		ObservedAtStart: start, ObservedAtEnd: start.Add(time.Second), Complete: true, Limits: limits,
		Used: BonusReviewUsage{
			RequestsAttempted: 1, ResponseBytesRead: 512, ResponseBytesKnown: true,
			FormsExamined: 2, FieldsExamined: 4, TokensExamined: 80, VisibleTextBytes: 120,
		},
	}
	observed, err := NewObservedBonusReview(review, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if !observed.Verified() || !observed.MatchesReceipt(receipt) || !observed.Matches("tjupt", "1", receipt.Origin, receipt.RouteID) {
		t.Fatal("live observation authority was not retained")
	}
	raw, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ObservedBonusReview
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Verified() || decoded.MatchesReceipt(receipt) {
		t.Fatal("serialized public review recovered process-local authority")
	}
	public := observed.PublicCopy()
	public.Columns[0] = "mutated"
	if observed.PublicCopy().Columns[0] == "mutated" {
		t.Fatal("public copy mutated opaque observation")
	}
}

func TestObservedBonusReviewRejectsNonCanonicalFormShapeID(t *testing.T) {
	review := validBonusReviewFixture(t)
	review.FormShapeID = "sha256:" + strings.ToUpper(strings.TrimPrefix(review.FormShapeID, "sha256:"))
	start := time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)
	limits := DefaultBonusReviewLimits()
	receipt := BonusReviewReceipt{
		Effect: BonusReviewReadEffect, SiteID: "tjupt", Selector: "1",
		Origin: "https://www.tjupt.org", RouteID: "tjupt.bonus.review.v1",
		ObservedAtStart: start, ObservedAtEnd: start.Add(time.Second), Complete: true, Limits: limits,
		Used: BonusReviewUsage{
			RequestsAttempted: 1, ResponseBytesRead: 512, ResponseBytesKnown: true,
			FormsExamined: 2, FieldsExamined: 4, TokensExamined: 80, VisibleTextBytes: 120,
		},
	}
	if _, err := NewObservedBonusReview(review, receipt); err == nil {
		t.Fatal("non-canonical form shape ID was accepted")
	}
}

func validBonusReviewFixture(t *testing.T) domain.BonusOfferReview {
	t.Helper()
	review := domain.BonusOfferReview{
		SiteID: "tjupt", Selector: "1", Balance: "12345.67",
		Columns:      []string{"Upload credit", "1000", "Exchange"},
		Availability: BonusReviewAvailabilityAvailable, InputMode: BonusReviewInputNone,
		ActionMethod: "post", ActionRouteID: "tjupt.bonus.exchange.v1",
		FormShapeID: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		EvidenceBasis: []string{
			BonusReviewBasisAuthenticated, BonusReviewBasisExactSelector, BonusReviewBasisExactForm,
			BonusReviewBasisOneRequest, BonusReviewBasisSiteClaimOnly, BonusReviewBasisNoSubmission,
		},
	}
	id, err := ComputeBonusReviewID(review)
	if err != nil {
		t.Fatal(err)
	}
	review.ReviewID = id
	return review
}
