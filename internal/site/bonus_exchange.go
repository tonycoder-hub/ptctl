package site

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const (
	BonusExchangeSubmitEffect = "submit_site_bonus_exchange"

	BonusExchangeOutcomeConfirmed    = "confirmed"
	BonusExchangeOutcomeRejected     = "rejected"
	BonusExchangeOutcomeUnknown      = "unknown"
	BonusExchangeOutcomeNotSubmitted = "not_submitted"

	defaultBonusExchangeResponseBytes = int64(4 << 20)
	hardBonusExchangeResponseBytes    = int64(8 << 20)
	hardBonusExchangeFormBytes        = int64(64 << 10)
	hardBonusExchangeFormFields       = 256
)

// BonusExchangeLimits bounds the complete fresh-review-plus-submit session.
// The transport budget is fixed at one GET and at most one POST.
type BonusExchangeLimits struct {
	MaxSessionRequests     int   `json:"max_session_requests"`
	MaxSubmissionRequests  int   `json:"max_submission_requests"`
	MaxFormFields          int   `json:"max_form_fields"`
	MaxFormBytes           int64 `json:"max_form_bytes"`
	MaxResponseBytes       int64 `json:"max_response_bytes"`
	MaxResponseHeaderBytes int64 `json:"max_response_header_bytes"`
}

func DefaultBonusExchangeLimits() BonusExchangeLimits {
	return BonusExchangeLimits{
		MaxSessionRequests:     2,
		MaxSubmissionRequests:  1,
		MaxFormFields:          128,
		MaxFormBytes:           32 << 10,
		MaxResponseBytes:       defaultBonusExchangeResponseBytes,
		MaxResponseHeaderBytes: hardBonusReviewHeaderBytes,
	}
}

func (limits BonusExchangeLimits) Validate() error {
	if limits.MaxSessionRequests != 2 || limits.MaxSubmissionRequests != 1 {
		return fmt.Errorf("bonus exchange request budgets must be exactly one review and one submission")
	}
	if limits.MaxFormFields <= 0 || limits.MaxFormFields > hardBonusExchangeFormFields ||
		limits.MaxFormBytes <= 0 || limits.MaxFormBytes > hardBonusExchangeFormBytes {
		return fmt.Errorf("bonus exchange form budget is outside the supported range")
	}
	if limits.MaxResponseBytes <= 0 || limits.MaxResponseBytes > hardBonusExchangeResponseBytes ||
		limits.MaxResponseHeaderBytes <= 0 || limits.MaxResponseHeaderBytes > hardBonusReviewHeaderBytes {
		return fmt.Errorf("bonus exchange response budget is outside the supported range")
	}
	return nil
}

type BonusExchangeUsage struct {
	TotalRequestsAttempted      int   `json:"total_requests_attempted"`
	ReviewRequestsAttempted     int   `json:"review_requests_attempted"`
	SubmissionRequestsAttempted int   `json:"submission_requests_attempted"`
	AutomaticRetries            int   `json:"automatic_retries"`
	RedirectsFollowed           int   `json:"redirects_followed"`
	FormFieldsSubmitted         int   `json:"form_fields_submitted"`
	FormBytesSubmitted          int64 `json:"form_bytes_submitted"`
	ResponseBytesRead           int64 `json:"response_bytes_read"`
	ResponseBytesKnown          bool  `json:"response_bytes_known"`
}

// BonusExchangeReceipt is safe for persistence and reports. ConfirmationCode
// is an adapter-defined allowlisted route code, never raw response text or a
// Location header.
type BonusExchangeReceipt struct {
	Effect           string              `json:"effect"`
	SiteID           string              `json:"site_id"`
	Selector         string              `json:"selector"`
	ExpectedReviewID string              `json:"expected_review_id"`
	FreshReviewID    string              `json:"fresh_review_id"`
	Origin           string              `json:"origin"`
	ReviewRouteID    string              `json:"review_route_id"`
	ActionRouteID    string              `json:"action_route_id"`
	ObservedAtStart  time.Time           `json:"observed_at_start"`
	ObservedAtEnd    time.Time           `json:"observed_at_end"`
	Complete         bool                `json:"complete"`
	RequestAttempted bool                `json:"request_attempted"`
	ResponseComplete bool                `json:"response_complete"`
	Outcome          string              `json:"outcome"`
	ConfirmationCode string              `json:"confirmation_code,omitempty"`
	Limits           BonusExchangeLimits `json:"limits"`
	Used             BonusExchangeUsage  `json:"used"`
	StopReason       string              `json:"stop_reason,omitempty"`
}

type BonusExchangeConfig struct {
	Origin        string `json:"origin"`
	ReviewRouteID string `json:"review_route_id"`
	ActionRouteID string `json:"action_route_id"`
}

func NewBonusExchangeConfig(origin, reviewRouteID, actionRouteID string) (BonusExchangeConfig, error) {
	config := BonusExchangeConfig{Origin: origin, ReviewRouteID: reviewRouteID, ActionRouteID: actionRouteID}
	if err := config.Validate(); err != nil {
		return BonusExchangeConfig{}, err
	}
	return config, nil
}

func (config BonusExchangeConfig) Validate() error {
	if err := validateCanonicalOrigin(config.Origin); err != nil {
		return err
	}
	if err := validateRouteID(config.ReviewRouteID); err != nil {
		return err
	}
	return validateRouteID(config.ActionRouteID)
}

// BonusExchangeWriter is deliberately separate from BonusReviewReader. A site
// may support zero-write review without exposing a form-submission surface.
type BonusExchangeWriter interface {
	// Selector/config preflight must be side-effect free and must not read a
	// credential or contact the site.
	ValidateBonusExchangeSelector(string) error
	BonusExchangeConfig() (BonusExchangeConfig, error)
	// OpenBonusExchangeSession constructs process-local transport state only.
	// The first remote request must be the bounded ReadFreshBonusReview call so
	// callers can finish all policy and credential ordering before any effect.
	OpenBonusExchangeSession(context.Context, Credential) (BonusExchangeSession, error)
}

// BonusExchangeSession first returns a fresh review authority, then may consume
// that exact in-memory authority once. Implementations must reject a different
// object, a serialized copy, or a second submission.
type BonusExchangeSession interface {
	// ReadFreshBonusReview must validate the captured form against the exact
	// exchange limits before returning authority. This keeps predictable form
	// budget failures on the read-only side of the durable attempt marker.
	ReadFreshBonusReview(context.Context, string, BonusReviewLimits, BonusExchangeLimits) (*ObservedBonusReview, BonusReviewReceipt, error)
	SubmitFreshBonusExchange(context.Context, *ObservedBonusReview, string, BonusExchangeLimits) (*ObservedBonusExchange, BonusExchangeReceipt, error)
	RequestsMade() int
	Close() error
}

// ObservedBonusExchange is the process-local authority proving that a site
// adapter produced a complete, structurally valid submission receipt. The
// public receipt remains useful for reporting, but a JSON round trip cannot
// recreate the authority required by a durable outcome writer.
type ObservedBonusExchange struct {
	receipt   BonusExchangeReceipt
	authority *observedBonusExchangeAuthority
}

type observedBonusExchangeAuthority struct{}

// NewObservedBonusExchange is the construction boundary for site adapters.
// It validates and copies the complete safe receipt; no response body,
// credential, redirect URL, or submitted field value enters the authority.
func NewObservedBonusExchange(receipt BonusExchangeReceipt) (*ObservedBonusExchange, error) {
	if err := ValidateBonusExchangeReceipt(receipt); err != nil {
		return nil, err
	}
	return &ObservedBonusExchange{receipt: receipt, authority: &observedBonusExchangeAuthority{}}, nil
}

func (observed *ObservedBonusExchange) PublicCopy() BonusExchangeReceipt {
	if observed == nil {
		return BonusExchangeReceipt{}
	}
	return observed.receipt
}

func (observed *ObservedBonusExchange) Verified() bool {
	return observed != nil && observed.authority != nil && ValidateBonusExchangeReceipt(observed.receipt) == nil
}

func (observed *ObservedBonusExchange) MatchesReceipt(receipt BonusExchangeReceipt) bool {
	return observed != nil && observed.Verified() && observed.receipt == receipt
}

func (observed *ObservedBonusExchange) Matches(siteID, selector, expectedReviewID string, config BonusExchangeConfig) bool {
	return observed != nil && observed.Verified() && config.Validate() == nil &&
		observed.receipt.SiteID == siteID && observed.receipt.Selector == selector &&
		observed.receipt.ExpectedReviewID == expectedReviewID && observed.receipt.FreshReviewID == expectedReviewID &&
		observed.receipt.Origin == config.Origin && observed.receipt.ReviewRouteID == config.ReviewRouteID &&
		observed.receipt.ActionRouteID == config.ActionRouteID
}

func (observed ObservedBonusExchange) String() string { return "site.ObservedBonusExchange{[OPAQUE]}" }
func (observed ObservedBonusExchange) GoString() string {
	return "site.ObservedBonusExchange{[OPAQUE]}"
}

func (observed ObservedBonusExchange) MarshalJSON() ([]byte, error) {
	return json.Marshal(observed.receipt)
}

func (observed *ObservedBonusExchange) UnmarshalJSON(raw []byte) error {
	if observed == nil {
		return fmt.Errorf("observed bonus exchange is nil")
	}
	var receipt BonusExchangeReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return err
	}
	observed.receipt = receipt
	observed.authority = nil
	return nil
}

func ValidateBonusExchangeReceipt(receipt BonusExchangeReceipt) error {
	if receipt.Effect != BonusExchangeSubmitEffect || !receipt.Complete || receipt.Limits.Validate() != nil ||
		!safeBonusText(receipt.SiteID, 128) || !safeBonusText(receipt.Selector, 128) ||
		ValidateBonusReviewID(receipt.ExpectedReviewID) != nil || receipt.FreshReviewID != receipt.ExpectedReviewID ||
		ValidateBonusReviewID(receipt.FreshReviewID) != nil || receipt.ObservedAtStart.IsZero() || receipt.ObservedAtEnd.IsZero() ||
		receipt.ObservedAtStart.Location() != time.UTC || receipt.ObservedAtEnd.Location() != time.UTC || receipt.ObservedAtEnd.Before(receipt.ObservedAtStart) ||
		receipt.Used.ReviewRequestsAttempted != 1 || receipt.Used.TotalRequestsAttempted < 1 || receipt.Used.TotalRequestsAttempted > 2 ||
		receipt.Used.AutomaticRetries != 0 || receipt.Used.RedirectsFollowed != 0 ||
		receipt.Used.FormFieldsSubmitted < 0 || receipt.Used.FormFieldsSubmitted > receipt.Limits.MaxFormFields ||
		receipt.Used.FormBytesSubmitted < 0 || receipt.Used.FormBytesSubmitted > receipt.Limits.MaxFormBytes ||
		receipt.Used.ResponseBytesRead < 0 || receipt.Used.ResponseBytesRead > receipt.Limits.MaxResponseBytes {
		return fmt.Errorf("bonus exchange receipt is invalid")
	}
	config, err := NewBonusExchangeConfig(receipt.Origin, receipt.ReviewRouteID, receipt.ActionRouteID)
	if err != nil || config.Validate() != nil {
		return fmt.Errorf("bonus exchange receipt is invalid")
	}
	if receipt.RequestAttempted {
		if receipt.Used.SubmissionRequestsAttempted != 1 || receipt.Used.TotalRequestsAttempted != 2 || receipt.Used.FormFieldsSubmitted <= 0 || receipt.Used.FormBytesSubmitted <= 0 {
			return fmt.Errorf("bonus exchange receipt is invalid")
		}
	} else if receipt.Used.SubmissionRequestsAttempted != 0 || receipt.Used.TotalRequestsAttempted != 1 ||
		receipt.Used.FormFieldsSubmitted != 0 || receipt.Used.FormBytesSubmitted != 0 || receipt.ResponseComplete || receipt.Used.ResponseBytesKnown || receipt.Used.ResponseBytesRead != 0 {
		return fmt.Errorf("bonus exchange receipt is invalid")
	}
	if receipt.ResponseComplete != receipt.Used.ResponseBytesKnown {
		return fmt.Errorf("bonus exchange receipt is invalid")
	}
	switch receipt.Outcome {
	case BonusExchangeOutcomeConfirmed:
		if !receipt.RequestAttempted || !receipt.ResponseComplete || validateRouteID(receipt.ConfirmationCode) != nil || receipt.StopReason != "" {
			return fmt.Errorf("bonus exchange receipt is invalid")
		}
	case BonusExchangeOutcomeRejected:
		if !receipt.RequestAttempted || !receipt.ResponseComplete || receipt.StopReason == "" || receipt.ConfirmationCode != "" || validateRouteID(receipt.StopReason) != nil {
			return fmt.Errorf("bonus exchange receipt is invalid")
		}
	case BonusExchangeOutcomeUnknown:
		if !receipt.RequestAttempted || receipt.StopReason == "" || receipt.ConfirmationCode != "" || validateRouteID(receipt.StopReason) != nil {
			return fmt.Errorf("bonus exchange receipt is invalid")
		}
	case BonusExchangeOutcomeNotSubmitted:
		if receipt.RequestAttempted || receipt.StopReason == "" || receipt.ConfirmationCode != "" || validateRouteID(receipt.StopReason) != nil {
			return fmt.Errorf("bonus exchange receipt is invalid")
		}
	default:
		return fmt.Errorf("bonus exchange receipt is invalid")
	}
	return nil
}
