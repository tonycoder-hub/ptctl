package mteam

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

const (
	BonusReviewOrigin = Origin
	// BonusReviewRouteID names the one POST to api/mall/list. The mall list is
	// the site's own catalog of goods; reviewing one item never submits it.
	BonusReviewRouteID = "mteam.mall_list_review.v1"
	// BonusExchangeRouteID names the one POST to api/mall/exchange.
	BonusExchangeRouteID = "mteam.mall_exchange_post.v1"

	bonusConfirmedCode = "mteam.mall_exchange.confirm" + site.BonusExchangeMessageConfirmationSuffix

	mallListPath     = "api/mall/list"
	mallExchangePath = "api/mall/exchange"

	// exchangeQuantity is the only amount ptctl ever asks for: one unit of
	// the reviewed goods, such as 1 GB of upload or one invite.
	exchangeQuantity = "1"
	maxGoodsIDDigits = 10
	maxGoodsName     = 256
	maxGoodsType     = 64
)

// mallInputModes maps the goods types ptctl has seen to the input the site's
// exchange asks for. UPLOAD and INVITE take only the goods ID and an amount,
// which ptctl fixes at one. Titles and the global-free pools need a value the
// user chooses, so they stay review-only. Any other type is unsupported.
var mallInputModes = map[string]string{
	"UPLOAD":             site.BonusReviewInputNone,
	"INVITE":             site.BonusReviewInputNone,
	"TITLE":              site.BonusReviewInputRequired,
	"GLOBAL_FREE_SINGLE": site.BonusReviewInputRequired,
	"GLOBAL_FREE_POOL":   site.BonusReviewInputRequired,
}

var bonusEvidenceBasis = []string{
	site.BonusReviewBasisAuthenticated,
	site.BonusReviewBasisExactSelector,
	site.BonusReviewBasisExactForm,
	site.BonusReviewBasisOneRequest,
	site.BonusReviewBasisSiteClaimOnly,
	site.BonusReviewBasisNoSubmission,
}

var (
	_ site.BonusCatalogReader   = (*Adapter)(nil)
	_ site.BonusReviewReader    = (*Adapter)(nil)
	_ site.BonusReviewSession   = (*bonusReviewSession)(nil)
	_ site.BonusExchangeWriter  = (*Adapter)(nil)
	_ site.BonusExchangeSession = (*bonusExchangeSession)(nil)
)

// guardedEffectClient is a guardedClient that can also send the one
// state-changing exchange POST. Only the exchange session asks for it.
type guardedEffectClient interface {
	guardedClient
	PostEffectOnce(context.Context, string, string, []byte, string, int64, int64, int64) (httpguard.StrictResponse, error)
}

// BonusCatalog reads the balance from the profile and then the mall list, on
// one client so the two reads keep the site's request spacing.
func (a *Adapter) BonusCatalog(ctx context.Context, credential site.Credential) (domain.BonusCatalog, error) {
	if err := ctx.Err(); err != nil {
		return domain.BonusCatalog{}, err
	}
	client, err := a.openClient(credential)
	if err != nil {
		return domain.BonusCatalog{}, err
	}
	defer client.Close()
	data, err := post(ctx, client, profilePath, emptyFormMediaType, []byte(emptyFormBody))
	if err != nil {
		return domain.BonusCatalog{}, err
	}
	profile, err := parseProfile(data)
	if err != nil {
		return domain.BonusCatalog{}, err
	}
	data, err = post(ctx, client, mallListPath, emptyFormMediaType, []byte(emptyFormBody))
	if err != nil {
		return domain.BonusCatalog{}, err
	}
	offers, _, err := parseMall(data, site.DefaultBonusReviewLimits())
	if err != nil || len(offers) == 0 {
		return domain.BonusCatalog{}, fmt.Errorf("M-Team mall response was not recognized; refusing an empty catalog")
	}
	rows := make([]domain.BonusCatalogRow, 0, len(offers))
	for _, offer := range offers {
		rows = append(rows, domain.BonusCatalogRow{Selector: offer.id, Columns: append(offer.columns(), offer.catalogStatus())})
	}
	return domain.BonusCatalog{SiteID: SiteID, Balance: profile.Bonus, Rows: rows, ObservedAt: time.Now().UTC()}, nil
}

func (a *Adapter) ValidateBonusReviewSelector(selector string) error {
	return validateGoodsSelector(selector)
}

func (a *Adapter) BonusReviewConfig() (site.BonusReviewConfig, error) {
	config, err := site.NewBonusReviewConfig(BonusReviewOrigin, BonusReviewRouteID)
	if err != nil {
		return site.BonusReviewConfig{}, fmt.Errorf("M-Team bonus review configuration is invalid")
	}
	return config, nil
}

func (a *Adapter) OpenBonusReviewSession(ctx context.Context, credential site.Credential) (site.BonusReviewSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, err := a.BonusReviewConfig()
	if err != nil {
		return nil, err
	}
	if credential.Method() != domain.AuthMethodAPIKeyHeader {
		return nil, fmt.Errorf("M-Team bonus review requires api_key_header authentication")
	}
	if !validAPIKey(credential.SecretValue()) {
		return nil, fmt.Errorf("M-Team API key is malformed: %w", site.ErrCredentialMalformed)
	}
	client, err := a.clientFactory()(DefaultBaseURL, credential.SecretValue(), minRequestInterval)
	if err != nil || client == nil {
		return nil, fmt.Errorf("open M-Team bonus review session failed")
	}
	return &bonusReviewSession{client: client, config: config}, nil
}

type bonusReviewSession struct {
	client guardedClient
	config site.BonusReviewConfig
	mu     sync.Mutex
	used   bool
	closed bool
}

func (session *bonusReviewSession) ReadBonusReview(ctx context.Context, selector string, limits site.BonusReviewLimits) (*site.ObservedBonusReview, site.BonusReviewReceipt, error) {
	now := time.Now().UTC()
	receipt := site.BonusReviewReceipt{
		Effect:          site.BonusReviewReadEffect,
		SiteID:          SiteID,
		Selector:        selector,
		Origin:          session.config.Origin,
		RouteID:         session.config.RouteID,
		ObservedAtStart: now,
		ObservedAtEnd:   now,
		Limits:          limits,
	}
	if err := limits.Validate(); err != nil {
		receipt.StopReason = "invalid_limits"
		return nil, receipt, fmt.Errorf("M-Team bonus review limits are invalid")
	}
	if err := validateGoodsSelector(selector); err != nil {
		receipt.StopReason = "invalid_selector"
		return nil, receipt, err
	}
	if err := ctx.Err(); err != nil {
		receipt.StopReason = "context_done"
		return nil, receipt, err
	}

	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		receipt.StopReason = "session_closed"
		return nil, receipt, fmt.Errorf("M-Team bonus review session is closed")
	}
	if session.used {
		session.mu.Unlock()
		receipt.StopReason = "request_budget_exhausted"
		receipt.Used.RequestsAttempted = session.RequestsMade()
		return nil, receipt, fmt.Errorf("M-Team bonus review request budget is exhausted")
	}
	session.used = true
	client := session.client
	session.mu.Unlock()

	response, requestErr := client.PostReadOnce(ctx, mallListPath, emptyFormMediaType, []byte(emptyFormBody), apiAccept, maxRequestBytes, limits.MaxResponseBytes, limits.MaxResponseHeaderBytes)
	receipt.Used.RequestsAttempted = client.RequestsMade()
	receipt.Used.ResponseBytesRead = response.ResponseBytesRead
	receipt.Used.ResponseBytesKnown = response.ResponseBytesKnown
	if !response.ObservedAtStart.IsZero() {
		receipt.ObservedAtStart = response.ObservedAtStart
	}
	if !response.ObservedAtEnd.IsZero() {
		receipt.ObservedAtEnd = response.ObservedAtEnd
	} else {
		receipt.ObservedAtEnd = time.Now().UTC()
	}
	if requestErr != nil {
		if errors.Is(requestErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			receipt.StopReason = "context_done"
			return nil, receipt, fmt.Errorf("M-Team bonus review request stopped: %w", context.Canceled)
		}
		if errors.Is(requestErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			receipt.StopReason = "context_done"
			return nil, receipt, fmt.Errorf("M-Team bonus review request stopped: %w", context.DeadlineExceeded)
		}
		if receipt.Used.RequestsAttempted != 1 {
			receipt.StopReason = "request_accounting_invalid"
			return nil, receipt, fmt.Errorf("M-Team bonus review request accounting is invalid")
		}
		receipt.StopReason = "site_request_failed"
		return nil, receipt, fmt.Errorf("M-Team bonus review request failed")
	}
	if receipt.Used.RequestsAttempted != 1 {
		receipt.StopReason = "request_accounting_invalid"
		return nil, receipt, fmt.Errorf("M-Team bonus review request accounting is invalid")
	}
	if !response.ResponseBytesKnown || response.ResponseBytesRead != int64(len(response.Body)) ||
		response.ObservedAtStart.IsZero() || response.ObservedAtEnd.IsZero() || response.ObservedAtEnd.Before(response.ObservedAtStart) ||
		int64(len(response.Body)) > limits.MaxResponseBytes {
		receipt.StopReason = "response_accounting_invalid"
		return nil, receipt, fmt.Errorf("M-Team bonus review response accounting is invalid")
	}
	if stopReason, err := classifyMallResponse(response); err != nil {
		receipt.StopReason = stopReason
		return nil, receipt, err
	}
	tokens, err := countJSONTokens(response.Body, limits.MaxTokens)
	receipt.Used.TokensExamined = tokens
	if err != nil {
		receipt.StopReason = mallStopReason(err)
		return nil, receipt, fmt.Errorf("M-Team mall response was not recognized")
	}
	data, stopReason, err := decodeMallEnvelope(response.Body)
	if err != nil {
		receipt.StopReason = stopReason
		return nil, receipt, err
	}
	offers, usage, err := parseMall(data, limits)
	receipt.Used.FormsExamined = usage.forms
	receipt.Used.FieldsExamined = usage.fields
	receipt.Used.VisibleTextBytes = usage.visibleTextBytes
	if err != nil {
		receipt.StopReason = mallStopReason(err)
		return nil, receipt, fmt.Errorf("M-Team mall response was not recognized")
	}
	offer, ok := findOffer(offers, selector)
	if !ok {
		receipt.StopReason = "selector_not_found"
		return nil, receipt, fmt.Errorf("M-Team mall has no goods %s", selector)
	}
	review, err := offer.review(selector)
	if err != nil {
		receipt.StopReason = "unrecognized_offer"
		return nil, receipt, fmt.Errorf("M-Team mall goods was not recognized")
	}
	receipt.Complete = true
	observed, err := site.NewObservedBonusReview(review, receipt)
	if err != nil {
		receipt.Complete = false
		receipt.StopReason = "response_accounting_invalid"
		return nil, receipt, fmt.Errorf("M-Team bonus review observation is invalid")
	}
	return observed, receipt, nil
}

func (session *bonusReviewSession) RequestsMade() int {
	if session == nil || session.client == nil {
		return 0
	}
	return session.client.RequestsMade()
}

func (session *bonusReviewSession) Close() error {
	if session == nil {
		return nil
	}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return nil
	}
	session.closed = true
	client := session.client
	session.mu.Unlock()
	if client == nil {
		return nil
	}
	if err := client.Close(); err != nil {
		return fmt.Errorf("close M-Team bonus review session failed")
	}
	return nil
}

func (a *Adapter) ValidateBonusExchangeSelector(selector string) error {
	return validateGoodsSelector(selector)
}

func (a *Adapter) BonusExchangeConfig() (site.BonusExchangeConfig, error) {
	config, err := site.NewBonusExchangeConfig(BonusReviewOrigin, BonusReviewRouteID, BonusExchangeRouteID)
	if err != nil {
		return site.BonusExchangeConfig{}, fmt.Errorf("M-Team bonus exchange configuration is invalid")
	}
	return config, nil
}

// OpenBonusExchangeSession binds one client for the fresh mall review and the
// one exchange POST. M-Team asks for no password: the API key alone
// authorizes the exchange.
func (a *Adapter) OpenBonusExchangeSession(ctx context.Context, credential site.Credential) (site.BonusExchangeSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, err := a.BonusExchangeConfig()
	if err != nil {
		return nil, err
	}
	if credential.Method() != domain.AuthMethodAPIKeyHeader {
		return nil, fmt.Errorf("M-Team bonus exchange requires api_key_header authentication")
	}
	if !validAPIKey(credential.SecretValue()) {
		return nil, fmt.Errorf("M-Team API key is malformed: %w", site.ErrCredentialMalformed)
	}
	readClient, err := a.clientFactory()(DefaultBaseURL, credential.SecretValue(), minRequestInterval)
	if err != nil || readClient == nil {
		return nil, fmt.Errorf("open M-Team bonus exchange session failed")
	}
	client, ok := readClient.(guardedEffectClient)
	if !ok {
		_ = readClient.Close()
		return nil, fmt.Errorf("open M-Team bonus exchange session failed")
	}
	reviewConfig, err := site.NewBonusReviewConfig(config.Origin, config.ReviewRouteID)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("open M-Team bonus exchange session failed")
	}
	reviewer := &bonusReviewSession{client: client, config: reviewConfig}
	return &bonusExchangeSession{client: client, config: config, reviewer: reviewer}, nil
}

type bonusExchangeSession struct {
	client         guardedEffectClient
	config         site.BonusExchangeConfig
	reviewer       *bonusReviewSession
	mu             sync.Mutex
	closed         bool
	reviewed       bool
	submitted      bool
	observed       *site.ObservedBonusReview
	receipt        site.BonusReviewReceipt
	body           []byte
	exchangeLimits site.BonusExchangeLimits
}

func (session *bonusExchangeSession) ReadFreshBonusReview(ctx context.Context, selector string, limits site.BonusReviewLimits, exchangeLimits site.BonusExchangeLimits) (*site.ObservedBonusReview, site.BonusReviewReceipt, error) {
	if session == nil {
		return nil, site.BonusReviewReceipt{}, fmt.Errorf("M-Team bonus exchange session is unavailable")
	}
	if exchangeLimits.Validate() != nil {
		return nil, site.BonusReviewReceipt{}, fmt.Errorf("M-Team bonus exchange limits are invalid")
	}
	session.mu.Lock()
	if session.closed || session.reviewed {
		session.mu.Unlock()
		return nil, site.BonusReviewReceipt{}, fmt.Errorf("M-Team bonus exchange review is unavailable")
	}
	session.reviewed = true
	reviewer := session.reviewer
	session.mu.Unlock()
	if reviewer == nil {
		return nil, site.BonusReviewReceipt{}, fmt.Errorf("M-Team bonus exchange review is unavailable")
	}
	observed, receipt, err := reviewer.ReadBonusReview(ctx, selector, limits)
	if err != nil {
		return nil, receipt, err
	}
	// Only goods that need nothing beyond the ID and the fixed amount have a
	// form ptctl can send.
	review := observed.PublicCopy()
	if review.Availability != site.BonusReviewAvailabilityAvailable || review.InputMode != site.BonusReviewInputNone {
		receipt.Complete = false
		receipt.StopReason = "submission_form_unavailable"
		return nil, receipt, fmt.Errorf("M-Team bonus exchange form is unavailable")
	}
	body := exchangeBody(selector)
	if exchangeLimits.MaxFormFields < 2 || int64(len(body)) > exchangeLimits.MaxFormBytes {
		receipt.Complete = false
		receipt.StopReason = "submission_form_budget_exceeded"
		return nil, receipt, fmt.Errorf("M-Team bonus exchange form exceeds the configured budget")
	}
	session.mu.Lock()
	session.observed = observed
	session.receipt = receipt
	session.body = body
	session.exchangeLimits = exchangeLimits
	session.mu.Unlock()
	return observed, receipt, nil
}

func (session *bonusExchangeSession) SubmitFreshBonusExchange(ctx context.Context, observed *site.ObservedBonusReview, expectedReviewID string, limits site.BonusExchangeLimits) (*site.ObservedBonusExchange, site.BonusExchangeReceipt, error) {
	now := time.Now().UTC()
	receipt := site.BonusExchangeReceipt{
		Effect: site.BonusExchangeSubmitEffect, ExpectedReviewID: expectedReviewID,
		FreshReviewID: expectedReviewID, ObservedAtStart: now, ObservedAtEnd: now,
		Limits: limits, Outcome: site.BonusExchangeOutcomeNotSubmitted,
	}
	if session == nil {
		receipt.StopReason = "session_unavailable"
		return nil, completeBonusExchangeReceipt(receipt), fmt.Errorf("M-Team bonus exchange session is unavailable")
	}
	receipt.SiteID = SiteID
	receipt.Origin = session.config.Origin
	receipt.ReviewRouteID = session.config.ReviewRouteID
	receipt.ActionRouteID = session.config.ActionRouteID
	if limits.Validate() != nil || site.ValidateBonusReviewID(expectedReviewID) != nil {
		receipt.StopReason = "invalid_precondition"
		return nil, completeBonusExchangeReceipt(receipt), fmt.Errorf("M-Team bonus exchange precondition is invalid")
	}

	session.mu.Lock()
	if session.closed || session.submitted || !session.reviewed || session.observed == nil || session.body == nil ||
		observed != session.observed || limits != session.exchangeLimits {
		session.mu.Unlock()
		receipt.StopReason = "fresh_review_authority_unavailable"
		return nil, completeBonusExchangeReceipt(receipt), fmt.Errorf("M-Team bonus exchange fresh review authority is unavailable")
	}
	freshReceipt := session.receipt
	body := session.body
	session.submitted = true
	client := session.client
	session.mu.Unlock()

	review := observed.PublicCopy()
	receipt.Selector = review.Selector
	receipt.FreshReviewID = review.ReviewID
	receipt.Used.ReviewRequestsAttempted = freshReceipt.Used.RequestsAttempted
	receipt.Used.TotalRequestsAttempted = boundedBonusRequestCount(client.RequestsMade())
	if !observed.MatchesReceipt(freshReceipt) || !observed.Matches(SiteID, review.Selector, session.config.Origin, session.config.ReviewRouteID) ||
		review.ReviewID != expectedReviewID || review.Availability != site.BonusReviewAvailabilityAvailable || review.InputMode != site.BonusReviewInputNone ||
		review.ActionMethod != "post" || review.ActionRouteID != session.config.ActionRouteID {
		receipt.StopReason = "fresh_review_mismatch"
		return nil, completeBonusExchangeReceipt(receipt), fmt.Errorf("M-Team bonus exchange fresh review did not match the approved intent")
	}
	if err := ctx.Err(); err != nil {
		receipt.StopReason = "context_before_submission"
		completed := completeBonusExchangeReceipt(receipt)
		authority, authorityErr := site.NewObservedBonusExchange(completed)
		if authorityErr != nil {
			return nil, completed, fmt.Errorf("M-Team bonus exchange receipt is invalid")
		}
		return authority, completed, err
	}

	response, submitErr := client.PostEffectOnce(ctx, mallExchangePath, emptyFormMediaType, body, apiAccept, limits.MaxFormBytes, limits.MaxResponseBytes, limits.MaxResponseHeaderBytes)
	receipt.ObservedAtStart = nonzeroBonusTime(response.ObservedAtStart, receipt.ObservedAtStart)
	receipt.ObservedAtEnd = nonzeroBonusTime(response.ObservedAtEnd, time.Now().UTC())
	receipt.Used.TotalRequestsAttempted = boundedBonusRequestCount(client.RequestsMade())
	receipt.Used.SubmissionRequestsAttempted = receipt.Used.TotalRequestsAttempted - receipt.Used.ReviewRequestsAttempted
	if receipt.Used.SubmissionRequestsAttempted < 0 {
		receipt.Used.SubmissionRequestsAttempted = 0
	}
	receipt.RequestAttempted = receipt.Used.SubmissionRequestsAttempted == 1
	if receipt.RequestAttempted {
		receipt.Used.FormFieldsSubmitted = 2
		receipt.Used.FormBytesSubmitted = int64(len(body))
		receipt.Used.ResponseBytesRead = response.ResponseBytesRead
		receipt.Used.ResponseBytesKnown = response.ResponseBytesKnown
		receipt.ResponseComplete = response.ResponseBytesKnown
	}
	if submitErr != nil {
		if !receipt.RequestAttempted {
			receipt.Outcome = site.BonusExchangeOutcomeNotSubmitted
			receipt.StopReason = "submission_not_sent"
		} else {
			receipt.Outcome = site.BonusExchangeOutcomeUnknown
			receipt.StopReason = "submission_transport_uncertain"
		}
		receipt = completeBonusExchangeReceipt(receipt)
		authority, authorityErr := site.NewObservedBonusExchange(receipt)
		if authorityErr != nil {
			return nil, receipt, fmt.Errorf("M-Team bonus exchange receipt is invalid")
		}
		if errors.Is(submitErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return authority, receipt, fmt.Errorf("M-Team bonus exchange stopped: %w", context.Canceled)
		}
		if errors.Is(submitErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return authority, receipt, fmt.Errorf("M-Team bonus exchange stopped: %w", context.DeadlineExceeded)
		}
		return authority, receipt, fmt.Errorf("M-Team bonus exchange response is uncertain")
	}

	receipt.Outcome, receipt.StopReason = classifyExchangeResponse(response)
	if receipt.Outcome == site.BonusExchangeOutcomeConfirmed {
		receipt.ConfirmationCode = bonusConfirmedCode
	}
	receipt = completeBonusExchangeReceipt(receipt)
	authority, authorityErr := site.NewObservedBonusExchange(receipt)
	if authorityErr != nil {
		return nil, receipt, fmt.Errorf("M-Team bonus exchange receipt is invalid")
	}
	if receipt.Outcome != site.BonusExchangeOutcomeConfirmed {
		return authority, receipt, fmt.Errorf("M-Team bonus exchange was not confirmed")
	}
	return authority, receipt, nil
}

func (session *bonusExchangeSession) RequestsMade() int {
	if session == nil || session.client == nil {
		return 0
	}
	return session.client.RequestsMade()
}

func (session *bonusExchangeSession) Close() error {
	if session == nil {
		return nil
	}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return nil
	}
	session.closed = true
	client := session.client
	session.mu.Unlock()
	if client != nil {
		if err := client.Close(); err != nil {
			return fmt.Errorf("close M-Team bonus exchange session failed")
		}
	}
	return nil
}

// classifyExchangeResponse reads only the envelope code of a complete JSON
// answer. Code "0" is the API's success; any other code means the site
// refused the exchange before spending bonus. Anything that is not a complete
// envelope stays unknown, because the exchange may have happened.
func classifyExchangeResponse(response httpguard.StrictResponse) (string, string) {
	complete := response.ResponseBytesKnown && response.ResponseBytesRead == int64(len(response.Body))
	switch {
	case !complete:
		return site.BonusExchangeOutcomeUnknown, "submission_response_unrecognized"
	case response.StatusCode == http.StatusUnauthorized:
		return site.BonusExchangeOutcomeRejected, "server_authentication_rejected"
	case response.StatusCode == http.StatusTooManyRequests:
		return site.BonusExchangeOutcomeRejected, "server_rate_limited"
	case response.StatusCode != http.StatusOK || !utf8.Valid(response.Body):
		return site.BonusExchangeOutcomeUnknown, "submission_response_unrecognized"
	}
	switch response.MediaType {
	case "application/json", "text/plain", "":
	default:
		return site.BonusExchangeOutcomeUnknown, "submission_response_unrecognized"
	}
	var envelope struct {
		Code flexText `json:"code"`
	}
	if !isJSONObject(response.Body) || json.Unmarshal(response.Body, &envelope) != nil || !envelope.Code.set {
		return site.BonusExchangeOutcomeUnknown, "submission_response_unrecognized"
	}
	switch envelope.Code.value {
	case "0":
		return site.BonusExchangeOutcomeConfirmed, ""
	case "401":
		return site.BonusExchangeOutcomeRejected, "server_authentication_rejected"
	case "4":
		return site.BonusExchangeOutcomeRejected, "server_rate_limited"
	default:
		return site.BonusExchangeOutcomeRejected, "server_refused"
	}
}

// classifyMallResponse maps the transport side of one mall answer to a public
// stop reason before any JSON is read.
func classifyMallResponse(response httpguard.StrictResponse) (string, error) {
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return "authentication_required", fmt.Errorf("M-Team API rejected the request: %w", site.ErrCredentialRejected)
	case response.StatusCode == http.StatusNotFound:
		return "not_found", fmt.Errorf("M-Team mall was not found")
	case response.StatusCode == http.StatusTooManyRequests:
		return "rate_limited", fmt.Errorf("M-Team API refused the request: %w", site.ErrRateLimited)
	case response.StatusCode >= 300 && response.StatusCode <= 399:
		return "redirect_rejected", fmt.Errorf("M-Team API redirect was rejected")
	case response.StatusCode != http.StatusOK:
		return "http_status_rejected", fmt.Errorf("M-Team bonus review response status was rejected")
	case len(response.Body) == 0:
		return "empty_response", fmt.Errorf("M-Team bonus review response was empty")
	case !utf8.Valid(response.Body):
		return "invalid_text_encoding", fmt.Errorf("M-Team bonus review response text was invalid")
	}
	switch response.MediaType {
	case "application/json", "text/plain", "":
		return "", nil
	default:
		return "content_type_rejected", fmt.Errorf("M-Team API response was not JSON")
	}
}

func decodeMallEnvelope(body []byte) (json.RawMessage, string, error) {
	var envelope struct {
		Code flexText        `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if !isJSONObject(body) || json.Unmarshal(body, &envelope) != nil || !envelope.Code.set {
		return nil, "unrecognized_response", fmt.Errorf("M-Team API response was not recognized")
	}
	switch envelope.Code.value {
	case "0":
		return envelope.Data, "", nil
	case "401":
		return nil, "authentication_required", fmt.Errorf("M-Team API rejected the request: %w", site.ErrCredentialRejected)
	case "4":
		return nil, "rate_limited", fmt.Errorf("M-Team API refused the request: %w", site.ErrRateLimited)
	default:
		return nil, "unrecognized_response", fmt.Errorf("M-Team API refused the bonus review request")
	}
}

type mallParseError struct{ reason string }

func (err *mallParseError) Error() string { return err.reason }

func mallStopReason(err error) string {
	var parseErr *mallParseError
	if errors.As(err, &parseErr) {
		return parseErr.reason
	}
	return "unrecognized_response"
}

// countJSONTokens walks the whole answer once so the review's token budget
// covers every value the site sent, not only the members ptctl keeps.
func countJSONTokens(body []byte, maxTokens int) (int, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	tokens := 0
	for {
		_, err := decoder.Token()
		if err == io.EOF {
			return tokens, nil
		}
		if err != nil {
			return tokens, &mallParseError{reason: "unrecognized_response"}
		}
		if tokens == maxTokens {
			return tokens, &mallParseError{reason: "token_budget_exceeded"}
		}
		tokens++
	}
}

type mallUsage struct {
	forms            int
	fields           int
	visibleTextBytes int64
}

type mallGoods struct {
	ID      flexText        `json:"id"`
	NameChs *string         `json:"nameChs"`
	NameCht *string         `json:"nameCht"`
	NameEng *string         `json:"nameEng"`
	Type    *string         `json:"type"`
	Points  flexText        `json:"points"`
	Roles   json.RawMessage `json:"roles"`
	Shelf   *string         `json:"shelf"`
}

type mallOffer struct {
	id, name, kind, points  string
	availability, inputMode string
}

// parseMall maps api/mall/list data, a list of goods objects. Each goods
// counts as one form and each of its members as one field. Any goods without
// a canonical ID, a name, a type, or a price fails the whole list, and so does
// a repeated ID, rather than reviewing a partial or ambiguous mall.
func parseMall(data json.RawMessage, limits site.BonusReviewLimits) ([]mallOffer, mallUsage, error) {
	var usage mallUsage
	var items []json.RawMessage
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '[' || json.Unmarshal(data, &items) != nil {
		return nil, usage, &mallParseError{reason: "unrecognized_response"}
	}
	if len(items) == 0 {
		return nil, usage, &mallParseError{reason: "selector_not_found"}
	}
	offers := make([]mallOffer, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		usage.forms++
		if usage.forms > limits.MaxForms {
			return nil, usage, &mallParseError{reason: "form_budget_exceeded"}
		}
		var members map[string]json.RawMessage
		var goods mallGoods
		if !isJSONObject(item) || json.Unmarshal(item, &members) != nil || json.Unmarshal(item, &goods) != nil {
			return nil, usage, &mallParseError{reason: "unrecognized_offer"}
		}
		usage.fields += len(members)
		if usage.fields > limits.MaxFields {
			return nil, usage, &mallParseError{reason: "field_budget_exceeded"}
		}
		offer, ok := goods.offer()
		if !ok {
			return nil, usage, &mallParseError{reason: "unrecognized_offer"}
		}
		if _, duplicate := seen[offer.id]; duplicate {
			return nil, usage, &mallParseError{reason: "selector_ambiguous"}
		}
		seen[offer.id] = struct{}{}
		for _, column := range offer.columns() {
			usage.visibleTextBytes += int64(len(column))
		}
		if usage.visibleTextBytes > limits.MaxVisibleTextBytes {
			return nil, usage, &mallParseError{reason: "visible_text_budget_exceeded"}
		}
		offers = append(offers, offer)
	}
	return offers, usage, nil
}

func findOffer(offers []mallOffer, id string) (mallOffer, bool) {
	for _, offer := range offers {
		if offer.id == id {
			return offer, true
		}
	}
	return mallOffer{}, false
}

func (goods mallGoods) offer() (mallOffer, bool) {
	if !goods.ID.set || validateGoodsSelector(goods.ID.value) != nil || goods.Type == nil || !mallType(*goods.Type) {
		return mallOffer{}, false
	}
	points := decimalText(goods.Points)
	if points == "" {
		return mallOffer{}, false
	}
	name := ""
	for _, candidate := range []*string{goods.NameChs, goods.NameCht, goods.NameEng} {
		if candidate == nil {
			continue
		}
		text, ok := cleanText(*candidate, maxGoodsName)
		if !ok {
			return mallOffer{}, false
		}
		if text != "" {
			name = text
			break
		}
	}
	if name == "" {
		return mallOffer{}, false
	}
	offer := mallOffer{id: goods.ID.value, name: name, kind: *goods.Type, points: points}
	offer.availability, offer.inputMode = goods.state()
	return offer, true
}

// state reports what the list claims about one goods item. Goods off the
// shelf are disabled. A role restriction is not checked against the account,
// so restricted goods stay unknown rather than claimed available.
func (goods mallGoods) state() (string, string) {
	inputMode, known := mallInputModes[*goods.Type]
	if !known {
		return site.BonusReviewAvailabilityUnknown, site.BonusReviewInputUnsupported
	}
	shelf := ""
	if goods.Shelf != nil {
		shelf = *goods.Shelf
	}
	switch {
	case shelf == "NO":
		return site.BonusReviewAvailabilityDisabled, inputMode
	case shelf != "YES" || !unrestrictedRoles(goods.Roles):
		return site.BonusReviewAvailabilityUnknown, inputMode
	default:
		return site.BonusReviewAvailabilityAvailable, inputMode
	}
}

func unrestrictedRoles(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return true
	}
	var roles []json.RawMessage
	return raw[0] == '[' && json.Unmarshal(raw, &roles) == nil && len(roles) == 0
}

func (offer mallOffer) columns() []string {
	price := "points " + offer.points
	if offer.kind == "UPLOAD" {
		price += " per GB"
	}
	return []string{offer.name, offer.kind, price}
}

// catalogStatus uses bonus review's vocabulary, as the TJUPT catalog does.
func (offer mallOffer) catalogStatus() string {
	switch {
	case offer.inputMode == site.BonusReviewInputUnsupported:
		return site.BonusReviewAvailabilityUnknown
	case offer.availability == site.BonusReviewAvailabilityAvailable && offer.inputMode == site.BonusReviewInputRequired:
		return offer.availability + ", requires input"
	default:
		return offer.availability
	}
}

func (offer mallOffer) review(selector string) (domain.BonusOfferReview, error) {
	review := domain.BonusOfferReview{
		SiteID:        SiteID,
		Selector:      selector,
		Columns:       offer.columns(),
		Availability:  offer.availability,
		InputMode:     offer.inputMode,
		ActionMethod:  "post",
		ActionRouteID: BonusExchangeRouteID,
		FormShapeID:   mallFormShapeID(offer),
		EvidenceBasis: append([]string(nil), bonusEvidenceBasis...),
	}
	reviewID, err := site.ComputeBonusReviewID(review)
	if err != nil {
		return domain.BonusOfferReview{}, err
	}
	review.ReviewID = reviewID
	return review, nil
}

// mallFormShapeID binds the review to the exact exchange ptctl would send and
// to the type and price the list showed, so a changed price changes the
// review ID and stops an approved intent.
func mallFormShapeID(offer mallOffer) string {
	hash := sha256.New()
	hash.Write([]byte("ptctl-mteam-mall-exchange-v1\x00"))
	var length [8]byte
	for _, part := range []string{"goodsId", offer.id, "num", exchangeQuantity, offer.kind, offer.points} {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		hash.Write(length[:])
		hash.Write([]byte(part))
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func exchangeBody(goodsID string) []byte {
	return formBody("goodsId", goodsID, "num", exchangeQuantity)
}

func validateGoodsSelector(selector string) error {
	if len(selector) > maxGoodsIDDigits || !canonicalID(selector) {
		return fmt.Errorf("M-Team bonus selector must be a canonical goods ID")
	}
	return nil
}

func mallType(value string) bool {
	if value == "" || len(value) > maxGoodsType {
		return false
	}
	for i := 0; i < len(value); i++ {
		if c := value[i]; !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func completeBonusExchangeReceipt(receipt site.BonusExchangeReceipt) site.BonusExchangeReceipt {
	if receipt.ObservedAtStart.IsZero() {
		receipt.ObservedAtStart = time.Now().UTC()
	}
	if receipt.ObservedAtEnd.IsZero() || receipt.ObservedAtEnd.Before(receipt.ObservedAtStart) {
		receipt.ObservedAtEnd = receipt.ObservedAtStart
	}
	receipt.Complete = true
	return receipt
}

func nonzeroBonusTime(value, fallback time.Time) time.Time {
	if value.IsZero() {
		return fallback.UTC()
	}
	return value.UTC()
}

func boundedBonusRequestCount(value int) int {
	if value < 0 {
		return 0
	}
	if value > 3 {
		return 3
	}
	return value
}
