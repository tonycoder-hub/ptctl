package site

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/domain"
)

const (
	BonusReviewReadEffect = "read_site_bonus_offer_review"

	BonusReviewAvailabilityAvailable = "available"
	BonusReviewAvailabilityDisabled  = "disabled"
	BonusReviewAvailabilityUnknown   = "unknown"

	BonusReviewInputNone        = "none"
	BonusReviewInputRequired    = "requires_user_input"
	BonusReviewInputUnsupported = "unsupported"

	BonusReviewBasisAuthenticated = "authenticated_bonus_page"
	BonusReviewBasisExactSelector = "exact_site_defined_option"
	BonusReviewBasisExactForm     = "bounded_form_observation"
	BonusReviewBasisOneRequest    = "single_bounded_no_redirect_read"
	BonusReviewBasisSiteClaimOnly = "site_claim_only"
	BonusReviewBasisNoSubmission  = "form_not_submitted"

	defaultBonusReviewResponseBytes = int64(4 << 20)
	hardBonusReviewResponseBytes    = int64(8 << 20)
	hardBonusReviewHeaderBytes      = int64(64 << 10)
)

type BonusReviewLimits struct {
	MaxRequests            int   `json:"max_requests"`
	MaxResponseBytes       int64 `json:"max_response_bytes"`
	MaxResponseHeaderBytes int64 `json:"max_response_header_bytes"`
	MaxForms               int   `json:"max_forms"`
	MaxFields              int   `json:"max_fields"`
	MaxTokens              int   `json:"max_tokens"`
	MaxVisibleTextBytes    int64 `json:"max_visible_text_bytes"`
}

func DefaultBonusReviewLimits() BonusReviewLimits {
	return BonusReviewLimits{
		MaxRequests:            1,
		MaxResponseBytes:       defaultBonusReviewResponseBytes,
		MaxResponseHeaderBytes: hardBonusReviewHeaderBytes,
		MaxForms:               128,
		MaxFields:              4096,
		MaxTokens:              100000,
		MaxVisibleTextBytes:    256 << 10,
	}
}

func (limits BonusReviewLimits) Validate() error {
	if limits.MaxRequests != 1 {
		return fmt.Errorf("bonus review request budget must be exactly one")
	}
	if limits.MaxResponseBytes <= 0 || limits.MaxResponseBytes > hardBonusReviewResponseBytes {
		return fmt.Errorf("bonus review response byte budget is outside the supported range")
	}
	if limits.MaxResponseHeaderBytes <= 0 || limits.MaxResponseHeaderBytes > hardBonusReviewHeaderBytes {
		return fmt.Errorf("bonus review response header budget is outside the supported range")
	}
	if limits.MaxForms <= 0 || limits.MaxForms > 256 || limits.MaxFields <= 0 || limits.MaxFields > 8192 ||
		limits.MaxTokens <= 0 || limits.MaxTokens > 200000 || limits.MaxVisibleTextBytes <= 0 || limits.MaxVisibleTextBytes > 1<<20 {
		return fmt.Errorf("bonus review parser budget is outside the supported range")
	}
	return nil
}

type BonusReviewUsage struct {
	RequestsAttempted  int   `json:"requests_attempted"`
	AutomaticRetries   int   `json:"automatic_retries"`
	RedirectsFollowed  int   `json:"redirects_followed"`
	ResponseBytesRead  int64 `json:"response_bytes_read"`
	ResponseBytesKnown bool  `json:"response_bytes_known"`
	FormsExamined      int   `json:"forms_examined"`
	FieldsExamined     int   `json:"fields_examined"`
	TokensExamined     int   `json:"tokens_examined"`
	VisibleTextBytes   int64 `json:"visible_text_bytes"`
}

type BonusReviewReceipt struct {
	Effect          string            `json:"effect"`
	SiteID          string            `json:"site_id"`
	Selector        string            `json:"selector"`
	Origin          string            `json:"origin"`
	RouteID         string            `json:"route_id"`
	ObservedAtStart time.Time         `json:"observed_at_start"`
	ObservedAtEnd   time.Time         `json:"observed_at_end"`
	Complete        bool              `json:"complete"`
	Limits          BonusReviewLimits `json:"limits"`
	Used            BonusReviewUsage  `json:"used"`
	StopReason      string            `json:"stop_reason,omitempty"`
}

type BonusReviewConfig struct {
	Origin  string `json:"origin"`
	RouteID string `json:"route_id"`
}

func NewBonusReviewConfig(origin, routeID string) (BonusReviewConfig, error) {
	config := BonusReviewConfig{Origin: origin, RouteID: routeID}
	if err := config.Validate(); err != nil {
		return BonusReviewConfig{}, err
	}
	return config, nil
}

func (config BonusReviewConfig) Validate() error {
	if err := validateCanonicalOrigin(config.Origin); err != nil {
		return err
	}
	return validateRouteID(config.RouteID)
}

type BonusReviewReader interface {
	ValidateBonusReviewSelector(string) error
	BonusReviewConfig() (BonusReviewConfig, error)
	OpenBonusReviewSession(context.Context, Credential) (BonusReviewSession, error)
}

type BonusReviewSession interface {
	ReadBonusReview(context.Context, string, BonusReviewLimits) (*ObservedBonusReview, BonusReviewReceipt, error)
	RequestsMade() int
	Close() error
}

type ObservedBonusReview struct {
	review    domain.BonusOfferReview
	receipt   BonusReviewReceipt
	authority *observedBonusReviewAuthority
}

type observedBonusReviewAuthority struct{}

func NewObservedBonusReview(review domain.BonusOfferReview, receipt BonusReviewReceipt) (*ObservedBonusReview, error) {
	if err := validateObservedBonusReview(review, receipt); err != nil {
		return nil, err
	}
	return &ObservedBonusReview{
		review:    copyBonusReview(review),
		receipt:   receipt,
		authority: &observedBonusReviewAuthority{},
	}, nil
}

func (observed *ObservedBonusReview) PublicCopy() domain.BonusOfferReview {
	if observed == nil {
		return domain.BonusOfferReview{Columns: []string{}, EvidenceBasis: []string{}}
	}
	return copyBonusReview(observed.review)
}

func (observed *ObservedBonusReview) Verified() bool {
	return observed != nil && observed.authority != nil && validateObservedBonusReview(observed.review, observed.receipt) == nil
}

func (observed *ObservedBonusReview) MatchesReceipt(receipt BonusReviewReceipt) bool {
	return observed != nil && observed.Verified() && observed.receipt == receipt
}

func (observed *ObservedBonusReview) Matches(siteID, selector, origin, routeID string) bool {
	return observed != nil && observed.Verified() && observed.review.SiteID == siteID && observed.review.Selector == selector &&
		observed.receipt.SiteID == siteID && observed.receipt.Selector == selector && observed.receipt.Origin == origin && observed.receipt.RouteID == routeID
}

func (observed *ObservedBonusReview) String() string { return "site.ObservedBonusReview{[OPAQUE]}" }
func (observed *ObservedBonusReview) GoString() string {
	return "site.ObservedBonusReview{[OPAQUE]}"
}

func (observed *ObservedBonusReview) MarshalJSON() ([]byte, error) {
	return json.Marshal(observed.PublicCopy())
}

func (observed *ObservedBonusReview) UnmarshalJSON(raw []byte) error {
	if observed == nil {
		return fmt.Errorf("observed bonus review is nil")
	}
	var public domain.BonusOfferReview
	if err := json.Unmarshal(raw, &public); err != nil {
		return err
	}
	observed.review = copyBonusReview(public)
	observed.receipt = BonusReviewReceipt{}
	observed.authority = nil
	return nil
}

func ComputeBonusReviewID(review domain.BonusOfferReview) (string, error) {
	copy := copyBonusReview(review)
	copy.ReviewID = ""
	copy.Balance = ""
	copy.EvidenceBasis = nil
	if err := validateBonusReviewProjection(copy, false); err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write([]byte("ptctl-bonus-offer-review-v1\x00"))
	writeReviewString(hash, copy.SiteID)
	writeReviewString(hash, copy.Selector)
	writeReviewString(hash, copy.Availability)
	writeReviewString(hash, copy.InputMode)
	writeReviewString(hash, copy.ActionMethod)
	writeReviewString(hash, copy.ActionRouteID)
	writeReviewString(hash, copy.FormShapeID)
	for _, column := range copy.Columns {
		writeReviewString(hash, column)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

type reviewHashWriter interface{ Write([]byte) (int, error) }

func writeReviewString(writer reviewHashWriter, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write([]byte(value))
}

func validateObservedBonusReview(review domain.BonusOfferReview, receipt BonusReviewReceipt) error {
	if receipt.Effect != BonusReviewReadEffect || !receipt.Complete || receipt.StopReason != "" || receipt.SiteID != review.SiteID || receipt.Selector != review.Selector ||
		receipt.Limits.Validate() != nil || receipt.ObservedAtStart.IsZero() || receipt.ObservedAtEnd.IsZero() ||
		receipt.ObservedAtStart.Location() != time.UTC || receipt.ObservedAtEnd.Location() != time.UTC || receipt.ObservedAtEnd.Before(receipt.ObservedAtStart) ||
		receipt.Used.RequestsAttempted != 1 || receipt.Used.AutomaticRetries != 0 || receipt.Used.RedirectsFollowed != 0 ||
		!receipt.Used.ResponseBytesKnown || receipt.Used.ResponseBytesRead <= 0 || receipt.Used.ResponseBytesRead > receipt.Limits.MaxResponseBytes ||
		receipt.Used.FormsExamined <= 0 || receipt.Used.FormsExamined > receipt.Limits.MaxForms || receipt.Used.FieldsExamined < 0 || receipt.Used.FieldsExamined > receipt.Limits.MaxFields ||
		receipt.Used.TokensExamined <= 0 || receipt.Used.TokensExamined > receipt.Limits.MaxTokens || receipt.Used.VisibleTextBytes < 0 || receipt.Used.VisibleTextBytes > receipt.Limits.MaxVisibleTextBytes {
		return fmt.Errorf("bonus review receipt is invalid")
	}
	if _, err := NewBonusReviewConfig(receipt.Origin, receipt.RouteID); err != nil {
		return fmt.Errorf("bonus review receipt is invalid")
	}
	if err := validateBonusReviewProjection(review, true); err != nil {
		return err
	}
	wantID, err := ComputeBonusReviewID(review)
	if err != nil || review.ReviewID != wantID {
		return fmt.Errorf("bonus review identity is invalid")
	}
	wantBasis := []string{BonusReviewBasisAuthenticated, BonusReviewBasisExactSelector, BonusReviewBasisExactForm, BonusReviewBasisOneRequest, BonusReviewBasisSiteClaimOnly, BonusReviewBasisNoSubmission}
	if len(review.EvidenceBasis) != len(wantBasis) {
		return fmt.Errorf("bonus review evidence is invalid")
	}
	for index := range wantBasis {
		if review.EvidenceBasis[index] != wantBasis[index] {
			return fmt.Errorf("bonus review evidence is invalid")
		}
	}
	return nil
}

func validateBonusReviewProjection(review domain.BonusOfferReview, requireEvidence bool) error {
	if !safeBonusText(review.SiteID, 128) || !safeBonusText(review.Selector, 128) ||
		!safeBonusText(review.ActionMethod, 16) || validateRouteID(review.ActionRouteID) != nil || !validBonusSHA256ID(review.FormShapeID) ||
		len(review.Columns) < 2 || len(review.Columns) > 16 {
		return fmt.Errorf("bonus review projection is invalid")
	}
	switch review.Availability {
	case BonusReviewAvailabilityAvailable, BonusReviewAvailabilityDisabled, BonusReviewAvailabilityUnknown:
	default:
		return fmt.Errorf("bonus review availability is invalid")
	}
	switch review.InputMode {
	case BonusReviewInputNone, BonusReviewInputRequired, BonusReviewInputUnsupported:
	default:
		return fmt.Errorf("bonus review input mode is invalid")
	}
	if review.Balance != "" && !validBonusDecimal(review.Balance) {
		return fmt.Errorf("bonus review balance is invalid")
	}
	total := 0
	for _, column := range review.Columns {
		if !safeBonusText(column, 4096) {
			return fmt.Errorf("bonus review column is invalid")
		}
		total += len(column)
	}
	if total > 16<<10 {
		return fmt.Errorf("bonus review columns exceed the supported budget")
	}
	if requireEvidence && len(review.EvidenceBasis) == 0 {
		return fmt.Errorf("bonus review evidence is missing")
	}
	return nil
}

func validBonusSHA256ID(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size
}

// ValidateBonusReviewID validates the public, domain-separated review
// correlator without treating it as replay or submission authority.
func ValidateBonusReviewID(value string) error {
	if !validBonusSHA256ID(value) {
		return fmt.Errorf("bonus review ID is invalid")
	}
	return nil
}

func safeBonusText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validBonusDecimal(value string) bool {
	if value == "" || len(value) > 128 || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	dot := false
	for _, r := range value {
		if r == '.' && !dot {
			dot = true
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func copyBonusReview(value domain.BonusOfferReview) domain.BonusOfferReview {
	result := value
	result.Columns = append([]string(nil), value.Columns...)
	result.EvidenceBasis = append([]string(nil), value.EvidenceBasis...)
	if result.Columns == nil {
		result.Columns = []string{}
	}
	if result.EvidenceBasis == nil {
		result.EvidenceBasis = []string{}
	}
	return result
}
