package tjupt

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

const (
	bonusExchangeAccept = "text/html"

	bonusRedirectDuplicate    = "tjupt.bonus_exchange.redirect.duplicated.v1"
	bonusRedirectNoPermission = "tjupt.bonus_exchange.redirect.no_permission.v1"
)

var bonusSuccessRedirects = map[string]string{
	"upload":               "tjupt.bonus_exchange.confirm.upload.v1",
	"download":             "tjupt.bonus_exchange.confirm.download.v1",
	"invite":               "tjupt.bonus_exchange.confirm.invite.v1",
	"tmp_invite":           "tjupt.bonus_exchange.confirm.tmp_invite.v1",
	"vip":                  "tjupt.bonus_exchange.confirm.vip.v1",
	"title":                "tjupt.bonus_exchange.confirm.title.v1",
	"transfer":             "tjupt.bonus_exchange.confirm.transfer.v1",
	"noad":                 "tjupt.bonus_exchange.confirm.noad.v1",
	"charity":              "tjupt.bonus_exchange.confirm.charity.v1",
	"cancel_hr":            "tjupt.bonus_exchange.confirm.cancel_hr.v1",
	"buy_medal":            "tjupt.bonus_exchange.confirm.buy_medal.v1",
	"attendance_card":      "tjupt.bonus_exchange.confirm.attendance_card.v1",
	"rainbow_id":           "tjupt.bonus_exchange.confirm.rainbow_id.v1",
	"change_username_card": "tjupt.bonus_exchange.confirm.change_username_card.v1",
}

var (
	_ site.BonusExchangeWriter  = (*Adapter)(nil)
	_ site.BonusExchangeSession = (*bonusExchangeSession)(nil)
)

func (a *Adapter) ValidateBonusExchangeSelector(selector string) error {
	if _, err := a.BonusExchangeConfig(); err != nil {
		return err
	}
	return validateBonusSelector(selector)
}

func (a *Adapter) BonusExchangeConfig() (site.BonusExchangeConfig, error) {
	if a == nil || a.baseURL != DefaultBaseURL {
		return site.BonusExchangeConfig{}, fmt.Errorf("TJUPT bonus exchange is unavailable for this origin")
	}
	config, err := site.NewBonusExchangeConfig(BonusReviewOrigin, BonusReviewRouteID, BonusExchangeRouteID)
	if err != nil {
		return site.BonusExchangeConfig{}, fmt.Errorf("TJUPT bonus exchange configuration is invalid")
	}
	return config, nil
}

func (a *Adapter) OpenBonusExchangeSession(ctx context.Context, credential site.Credential) (site.BonusExchangeSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, err := a.BonusExchangeConfig()
	if err != nil {
		return nil, err
	}
	if credential.Method() != domain.AuthMethodCookieHeader {
		return nil, fmt.Errorf("TJUPT bonus exchange requires cookie_header authentication")
	}
	if !validCookieHeader(credential.SecretValue()) {
		return nil, fmt.Errorf("TJUPT cookie credential is invalid")
	}
	readClient, err := a.clientFactory()(DefaultBaseURL, credential.SecretValue(), 2*time.Second)
	if err != nil || readClient == nil {
		return nil, fmt.Errorf("open TJUPT bonus exchange session failed")
	}
	client, ok := readClient.(guardedExchangeClient)
	if !ok {
		_ = readClient.Close()
		return nil, fmt.Errorf("open TJUPT bonus exchange session failed")
	}
	reviewConfig, err := site.NewBonusReviewConfig(config.Origin, config.ReviewRouteID)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("open TJUPT bonus exchange session failed")
	}
	reviewer := &bonusReviewSession{client: client, config: reviewConfig, captureSubmission: true}
	return &bonusExchangeSession{client: client, config: config, reviewer: reviewer}, nil
}

type bonusExchangeSession struct {
	client         guardedExchangeClient
	config         site.BonusExchangeConfig
	reviewer       *bonusReviewSession
	mu             sync.Mutex
	closed         bool
	reviewed       bool
	submitted      bool
	observed       *site.ObservedBonusReview
	receipt        site.BonusReviewReceipt
	submission     *bonusFormSubmission
	exchangeLimits site.BonusExchangeLimits
}

func (session *bonusExchangeSession) ReadFreshBonusReview(ctx context.Context, selector string, limits site.BonusReviewLimits, exchangeLimits site.BonusExchangeLimits) (*site.ObservedBonusReview, site.BonusReviewReceipt, error) {
	if session == nil {
		return nil, site.BonusReviewReceipt{}, fmt.Errorf("TJUPT bonus exchange session is unavailable")
	}
	if exchangeLimits.Validate() != nil {
		return nil, site.BonusReviewReceipt{}, fmt.Errorf("TJUPT bonus exchange limits are invalid")
	}
	session.mu.Lock()
	if session.closed || session.reviewed {
		session.mu.Unlock()
		return nil, site.BonusReviewReceipt{}, fmt.Errorf("TJUPT bonus exchange review is unavailable")
	}
	session.reviewed = true
	reviewer := session.reviewer
	session.mu.Unlock()
	if reviewer == nil {
		return nil, site.BonusReviewReceipt{}, fmt.Errorf("TJUPT bonus exchange review is unavailable")
	}
	observed, receipt, err := reviewer.ReadBonusReview(ctx, selector, limits)
	if err != nil {
		return nil, receipt, err
	}
	submission := reviewer.capturedSubmission()
	if submission == nil {
		receipt.Complete = false
		receipt.StopReason = "submission_form_unavailable"
		return nil, receipt, fmt.Errorf("TJUPT bonus exchange form is unavailable")
	}
	if _, _, validationErr := httpguard.ValidateFormFields(submission.fields, exchangeLimits.MaxFormFields, exchangeLimits.MaxFormBytes); validationErr != nil {
		receipt.Complete = false
		receipt.StopReason = "submission_form_budget_exceeded"
		return nil, receipt, fmt.Errorf("TJUPT bonus exchange form exceeds the configured budget")
	}
	session.mu.Lock()
	session.observed = observed
	session.receipt = receipt
	session.submission = submission
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
		return nil, completeBonusExchangeReceipt(receipt), fmt.Errorf("TJUPT bonus exchange session is unavailable")
	}
	receipt.SiteID = "tjupt"
	receipt.Origin = session.config.Origin
	receipt.ReviewRouteID = session.config.ReviewRouteID
	receipt.ActionRouteID = session.config.ActionRouteID
	if limits.Validate() != nil || site.ValidateBonusReviewID(expectedReviewID) != nil {
		receipt.StopReason = "invalid_precondition"
		return nil, completeBonusExchangeReceipt(receipt), fmt.Errorf("TJUPT bonus exchange precondition is invalid")
	}

	session.mu.Lock()
	if session.closed || session.submitted || !session.reviewed || session.observed == nil || session.submission == nil ||
		observed != session.observed || limits != session.exchangeLimits {
		session.mu.Unlock()
		receipt.StopReason = "fresh_review_authority_unavailable"
		return nil, completeBonusExchangeReceipt(receipt), fmt.Errorf("TJUPT bonus exchange fresh review authority is unavailable")
	}
	freshReceipt := session.receipt
	submission := session.submission
	session.submitted = true
	client := session.client
	session.mu.Unlock()

	review := observed.PublicCopy()
	receipt.Selector = review.Selector
	receipt.FreshReviewID = review.ReviewID
	receipt.Used.ReviewRequestsAttempted = freshReceipt.Used.RequestsAttempted
	receipt.Used.TotalRequestsAttempted = boundedBonusRequestCount(client.RequestsMade())
	if !observed.MatchesReceipt(freshReceipt) || !observed.Matches("tjupt", review.Selector, session.config.Origin, session.config.ReviewRouteID) ||
		review.ReviewID != expectedReviewID || review.Availability != site.BonusReviewAvailabilityAvailable || review.InputMode != site.BonusReviewInputNone ||
		review.ActionMethod != "post" || review.ActionRouteID != session.config.ActionRouteID {
		receipt.StopReason = "fresh_review_mismatch"
		return nil, completeBonusExchangeReceipt(receipt), fmt.Errorf("TJUPT bonus exchange fresh review did not match the approved intent")
	}
	if err := ctx.Err(); err != nil {
		receipt.StopReason = "context_before_submission"
		completed := completeBonusExchangeReceipt(receipt)
		authority, authorityErr := site.NewObservedBonusExchange(completed)
		if authorityErr != nil {
			return nil, completed, fmt.Errorf("TJUPT bonus exchange receipt is invalid")
		}
		return authority, completed, err
	}

	response, submitErr := client.PostFormOnce(
		ctx, submission.target.path, submission.target.query, submission.fields,
		bonusExchangeAccept, limits.MaxFormFields, limits.MaxFormBytes,
		limits.MaxResponseBytes, limits.MaxResponseHeaderBytes, classifyBonusExchangeRedirect,
	)
	receipt.ObservedAtStart = nonzeroBonusTime(response.ObservedAtStart, receipt.ObservedAtStart)
	receipt.ObservedAtEnd = nonzeroBonusTime(response.ObservedAtEnd, time.Now().UTC())
	receipt.Used.TotalRequestsAttempted = boundedBonusRequestCount(client.RequestsMade())
	receipt.Used.SubmissionRequestsAttempted = receipt.Used.TotalRequestsAttempted - receipt.Used.ReviewRequestsAttempted
	if receipt.Used.SubmissionRequestsAttempted < 0 {
		receipt.Used.SubmissionRequestsAttempted = 0
	}
	receipt.Used.FormFieldsSubmitted = response.RequestFields
	receipt.Used.FormBytesSubmitted = response.RequestBytes
	receipt.Used.ResponseBytesRead = response.ResponseBytesRead
	receipt.Used.ResponseBytesKnown = response.ResponseBytesKnown
	receipt.RequestAttempted = receipt.Used.SubmissionRequestsAttempted == 1
	receipt.ResponseComplete = response.ResponseBytesKnown
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
			return nil, receipt, fmt.Errorf("TJUPT bonus exchange receipt is invalid")
		}
		if errors.Is(submitErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return authority, receipt, fmt.Errorf("TJUPT bonus exchange stopped: %w", context.Canceled)
		}
		if errors.Is(submitErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return authority, receipt, fmt.Errorf("TJUPT bonus exchange stopped: %w", context.DeadlineExceeded)
		}
		return authority, receipt, fmt.Errorf("TJUPT bonus exchange response is uncertain")
	}

	switch {
	case (response.StatusCode == http.StatusFound || response.StatusCode == http.StatusSeeOther) && isBonusSuccessRedirect(response.RedirectID):
		receipt.Outcome = site.BonusExchangeOutcomeConfirmed
		receipt.ConfirmationCode = response.RedirectID
		receipt.StopReason = ""
	case (response.StatusCode == http.StatusFound || response.StatusCode == http.StatusSeeOther) && response.RedirectID == bonusRedirectDuplicate:
		receipt.Outcome = site.BonusExchangeOutcomeRejected
		receipt.StopReason = "server_duplicate_lock"
	case (response.StatusCode == http.StatusFound || response.StatusCode == http.StatusSeeOther) && response.RedirectID == bonusRedirectNoPermission:
		receipt.Outcome = site.BonusExchangeOutcomeRejected
		receipt.StopReason = "server_permission_rejected"
	default:
		receipt.Outcome = site.BonusExchangeOutcomeUnknown
		receipt.StopReason = "submission_response_unrecognized"
	}
	receipt = completeBonusExchangeReceipt(receipt)
	authority, authorityErr := site.NewObservedBonusExchange(receipt)
	if authorityErr != nil {
		return nil, receipt, fmt.Errorf("TJUPT bonus exchange receipt is invalid")
	}
	if receipt.Outcome != site.BonusExchangeOutcomeConfirmed {
		return authority, receipt, fmt.Errorf("TJUPT bonus exchange was not confirmed")
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
			return fmt.Errorf("close TJUPT bonus exchange session failed")
		}
	}
	return nil
}

func classifyBonusExchangeRedirect(target *url.URL) (string, bool) {
	if target == nil || target.Path != "/"+bonusReviewPath || target.Fragment != "" {
		return "", false
	}
	// URL.Query silently discards malformed query pairs. A redirect such as
	// `?do=upload&bad=a;b` must not be reduced to the otherwise-confirming
	// `do=upload` subset, so parse the complete raw query fail-closed.
	query, err := url.ParseQuery(target.RawQuery)
	if err != nil {
		return "", false
	}
	values, ok := query["do"]
	if !ok || len(query) != 1 || len(values) != 1 {
		return "", false
	}
	if code, exists := bonusSuccessRedirects[values[0]]; exists {
		return code, true
	}
	switch values[0] {
	case "duplicated":
		return bonusRedirectDuplicate, true
	case "vipfalse":
		return bonusRedirectNoPermission, true
	default:
		return "", false
	}
}

func isBonusSuccessRedirect(value string) bool {
	for _, candidate := range bonusSuccessRedirects {
		if value == candidate {
			return true
		}
	}
	return false
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
