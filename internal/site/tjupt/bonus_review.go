package tjupt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

const (
	BonusReviewOrigin        = "https://www.tjupt.org"
	BonusReviewRouteID       = "tjupt.mybonusapps_offer_review.v1"
	BonusExchangeRouteID     = "tjupt.mybonusapps_exchange_post.v1"
	bonusUnknownActionRoute  = "tjupt.bonus_action_unrecognized.v1"
	bonusReviewPath          = "mybonusapps.php"
	bonusReviewAccept        = "text/html"
	maxBonusSelectorDigits   = 10
	maxBonusAttributesPerTag = 64
)

var decimalBonusSelector = regexp.MustCompile(`^(0|[1-9][0-9]{0,9})$`)

var (
	_ site.BonusReviewReader  = (*Adapter)(nil)
	_ site.BonusReviewSession = (*bonusReviewSession)(nil)
)

func (a *Adapter) ValidateBonusReviewSelector(selector string) error {
	if _, err := a.BonusReviewConfig(); err != nil {
		return err
	}
	return validateBonusSelector(selector)
}

func (a *Adapter) BonusReviewConfig() (site.BonusReviewConfig, error) {
	if a == nil || a.baseURL != DefaultBaseURL {
		return site.BonusReviewConfig{}, fmt.Errorf("TJUPT bonus review is unavailable for this origin")
	}
	config, err := site.NewBonusReviewConfig(BonusReviewOrigin, BonusReviewRouteID)
	if err != nil {
		return site.BonusReviewConfig{}, fmt.Errorf("TJUPT bonus review configuration is invalid")
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
	if credential.Method() != domain.AuthMethodCookieHeader {
		return nil, fmt.Errorf("TJUPT bonus review requires cookie_header authentication")
	}
	if !validCookieHeader(credential.SecretValue()) {
		return nil, fmt.Errorf("TJUPT cookie credential is invalid")
	}
	client, err := a.clientFactory()(DefaultBaseURL, credential.SecretValue(), 2*time.Second)
	if err != nil || client == nil {
		return nil, fmt.Errorf("open TJUPT bonus review session failed")
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
		SiteID:          "tjupt",
		Selector:        selector,
		Origin:          session.config.Origin,
		RouteID:         session.config.RouteID,
		ObservedAtStart: now,
		ObservedAtEnd:   now,
		Limits:          limits,
	}
	if err := limits.Validate(); err != nil {
		receipt.StopReason = "invalid_limits"
		return nil, receipt, fmt.Errorf("TJUPT bonus review limits are invalid")
	}
	if err := validateBonusSelector(selector); err != nil {
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
		return nil, receipt, fmt.Errorf("TJUPT bonus review session is closed")
	}
	if session.used {
		session.mu.Unlock()
		receipt.StopReason = "request_budget_exhausted"
		receipt.Used.RequestsAttempted = session.RequestsMade()
		return nil, receipt, fmt.Errorf("TJUPT bonus review request budget is exhausted")
	}
	session.used = true
	client := session.client
	session.mu.Unlock()

	response, requestErr := client.GetOnce(ctx, bonusReviewPath, nil, bonusReviewAccept, limits.MaxResponseBytes, limits.MaxResponseHeaderBytes)
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
			return nil, receipt, fmt.Errorf("TJUPT bonus review request stopped: %w", context.Canceled)
		}
		if errors.Is(requestErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			receipt.StopReason = "context_done"
			return nil, receipt, fmt.Errorf("TJUPT bonus review request stopped: %w", context.DeadlineExceeded)
		}
		if receipt.Used.RequestsAttempted != 1 {
			receipt.StopReason = "request_accounting_invalid"
			return nil, receipt, fmt.Errorf("TJUPT bonus review request accounting is invalid")
		}
		receipt.StopReason = "site_request_failed"
		return nil, receipt, fmt.Errorf("TJUPT bonus review request failed")
	}
	if receipt.Used.RequestsAttempted != 1 {
		receipt.StopReason = "request_accounting_invalid"
		return nil, receipt, fmt.Errorf("TJUPT bonus review request accounting is invalid")
	}
	if !response.ResponseBytesKnown || response.ResponseBytesRead != int64(len(response.Body)) ||
		response.ObservedAtStart.IsZero() || response.ObservedAtEnd.IsZero() || response.ObservedAtEnd.Before(response.ObservedAtStart) ||
		int64(len(response.Body)) > limits.MaxResponseBytes {
		receipt.StopReason = "response_accounting_invalid"
		return nil, receipt, fmt.Errorf("TJUPT bonus review response accounting is invalid")
	}
	if stopReason, err := classifyBonusReviewResponse(response); err != nil {
		receipt.StopReason = stopReason
		return nil, receipt, err
	}
	review, parseUsage, err := parseBonusOfferReview(response.Body, selector, limits)
	receipt.Used.FormsExamined = parseUsage.forms
	receipt.Used.FieldsExamined = parseUsage.fields
	receipt.Used.TokensExamined = parseUsage.tokens
	receipt.Used.VisibleTextBytes = parseUsage.visibleTextBytes
	if err != nil {
		receipt.StopReason = bonusParseStopReason(err)
		return nil, receipt, fmt.Errorf("TJUPT bonus offer page was not recognized")
	}
	receipt.Complete = true
	observed, err := site.NewObservedBonusReview(review, receipt)
	if err != nil {
		receipt.Complete = false
		receipt.StopReason = "response_accounting_invalid"
		return nil, receipt, fmt.Errorf("TJUPT bonus review observation is invalid")
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
		return fmt.Errorf("close TJUPT bonus review session failed")
	}
	return nil
}

func validateBonusSelector(selector string) error {
	if len(selector) == 0 || len(selector) > maxBonusSelectorDigits || !decimalBonusSelector.MatchString(selector) {
		return fmt.Errorf("TJUPT bonus selector must be a canonical non-negative decimal option")
	}
	return nil
}

func classifyBonusReviewResponse(response httpguard.StrictResponse) (string, error) {
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return "authentication_required", fmt.Errorf("TJUPT authentication was rejected")
	case response.StatusCode == http.StatusNotFound:
		return "not_found", fmt.Errorf("TJUPT bonus page was not found")
	case response.StatusCode == http.StatusTooManyRequests:
		return "rate_limited", fmt.Errorf("TJUPT rate limited the bonus review request")
	case response.StatusCode >= 300 && response.StatusCode <= 399:
		return "redirect_rejected", fmt.Errorf("TJUPT bonus review redirect was rejected")
	case response.StatusCode != http.StatusOK:
		return "http_status_rejected", fmt.Errorf("TJUPT bonus review response status was rejected")
	case len(response.Body) == 0:
		return "empty_response", fmt.Errorf("TJUPT bonus review response was empty")
	case !utf8.Valid(response.Body):
		return "invalid_text_encoding", fmt.Errorf("TJUPT bonus review response text was invalid")
	}
	switch response.MediaType {
	case "", "text/html":
	default:
		return "content_type_rejected", fmt.Errorf("TJUPT bonus review content type was rejected")
	}
	if isLoginPage(nil, response.Body) {
		return "authentication_required", fmt.Errorf("TJUPT authentication was required")
	}
	if challengeMarker.Match(response.Body) {
		return "challenge_response", fmt.Errorf("TJUPT returned an interactive challenge")
	}
	return "", nil
}

type bonusParseUsage struct {
	forms            int
	fields           int
	tokens           int
	visibleTextBytes int64
}

type parsedBonusForm struct {
	selector         string
	selectorCount    int
	optionValues     []string
	method           string
	actionRouteID    string
	actionSupported  bool
	submitSeen       bool
	enabledSubmit    bool
	submitCount      int
	requiresInput    bool
	unsupportedInput bool
	formShape        []string
	cells            []string
	cellsOverflow    bool
	row              *bonusRowState
	looseText        strings.Builder
}

type bonusRowState struct {
	cells         []string
	cellsOverflow bool
	cellStack     []strings.Builder
}

type bonusParseError struct{ reason string }

func (err *bonusParseError) Error() string { return err.reason }

func parseBonusOfferReview(body []byte, selector string, limits site.BonusReviewLimits) (domain.BonusOfferReview, bonusParseUsage, error) {
	var usage bonusParseUsage
	if err := limits.Validate(); err != nil {
		return domain.BonusOfferReview{}, usage, err
	}
	pageURL := &url.URL{Scheme: "https", Host: "www.tjupt.org", Path: "/" + bonusReviewPath}
	state, _ := classifyBonusPage(pageURL, body)
	if !utf8.Valid(body) || state != domain.AuthenticationAuthenticated {
		return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "unrecognized_response"}
	}
	tokenizer := html.NewTokenizer(bytes.NewReader(body))
	var current *parsedBonusForm
	forms := make([]*parsedBonusForm, 0, 16)
	rows := make([]*bonusRowState, 0, 4)
	skipTextDepth := 0
	logoutSeen := false
	for {
		tokenType := tokenizer.Next()
		if tokenType == html.ErrorToken {
			if errors.Is(tokenizer.Err(), io.EOF) {
				break
			}
			return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "invalid_html"}
		}
		usage.tokens++
		if usage.tokens > limits.MaxTokens {
			return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "token_budget_exceeded"}
		}
		token := tokenizer.Token()
		switch tokenType {
		case html.StartTagToken, html.SelfClosingTagToken:
			name := strings.ToLower(token.Data)
			attrs, attrsErr := strictBonusAttributes(token.Attr)
			if attrsErr != nil {
				return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "invalid_html"}
			}
			if name == "base" || name == "template" {
				return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "ambiguous_form_structure"}
			}
			if name == "script" || name == "style" {
				if tokenType == html.StartTagToken {
					skipTextDepth++
				}
				continue
			}
			if skipTextDepth > 0 {
				continue
			}
			if name == "a" && isExactBonusLogout(attrs["href"]) {
				logoutSeen = true
			}
			if name == "tr" {
				if len(rows) >= 16 {
					return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "invalid_html"}
				}
				rows = append(rows, &bonusRowState{cells: make([]string, 0, 4), cellStack: make([]strings.Builder, 0, 2)})
				continue
			}
			if name == "form" {
				if current != nil {
					return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "ambiguous_form_structure"}
				}
				usage.forms++
				if usage.forms > limits.MaxForms {
					return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "form_budget_exceeded"}
				}
				method, route, supported := classifyBonusFormAction(attrs)
				current = &parsedBonusForm{method: method, actionRouteID: route, actionSupported: supported, cells: make([]string, 0, 4)}
				if len(rows) > 0 {
					current.row = rows[len(rows)-1]
				}
				forms = append(forms, current)
				continue
			}
			if current != nil && name == "fieldset" {
				// Fieldset-disabledness, including the first-legend exception, is
				// browser tree semantics rather than a local control attribute. Keep
				// the observation usable as a review, but never claim its inputs or
				// submit availability are understood.
				current.unsupportedInput = true
			}
			if (name == "td" || name == "th") && len(rows) > 0 {
				row := rows[len(rows)-1]
				if len(row.cellStack) != 0 {
					return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "ambiguous_form_structure"}
				}
				row.cellStack = append(row.cellStack, strings.Builder{})
				continue
			}
			if current == nil {
				continue
			}
			switch name {
			case "input", "button", "select", "textarea":
				usage.fields++
				if usage.fields > limits.MaxFields {
					return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "field_budget_exceeded"}
				}
				observeBonusControl(current, name, attrs)
			}
		case html.EndTagToken:
			name := strings.ToLower(token.Data)
			if name == "script" || name == "style" {
				if skipTextDepth == 0 {
					return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "invalid_html"}
				}
				skipTextDepth--
				continue
			}
			if skipTextDepth > 0 {
				continue
			}
			if (name == "td" || name == "th") && len(rows) > 0 {
				row := rows[len(rows)-1]
				if len(row.cellStack) > 0 {
					last := len(row.cellStack) - 1
					appendBonusRowCell(row, row.cellStack[last].String())
					row.cellStack = row.cellStack[:last]
				}
				continue
			}
			if name == "tr" && len(rows) > 0 {
				row := rows[len(rows)-1]
				if current != nil && current.row == row {
					return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "ambiguous_form_structure"}
				}
				for len(row.cellStack) > 0 {
					last := len(row.cellStack) - 1
					appendBonusRowCell(row, row.cellStack[last].String())
					row.cellStack = row.cellStack[:last]
				}
				rows = rows[:len(rows)-1]
				continue
			}
			if name == "form" {
				if current == nil {
					return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "invalid_html"}
				}
				if current.row == nil {
					appendBonusCell(current, current.looseText.String())
				}
				current = nil
			}
		case html.TextToken:
			if skipTextDepth > 0 {
				continue
			}
			if current == nil && len(rows) == 0 {
				continue
			}
			text := token.Data
			usage.visibleTextBytes += int64(len(text))
			if usage.visibleTextBytes > limits.MaxVisibleTextBytes {
				return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "visible_text_budget_exceeded"}
			}
			if len(rows) > 0 && len(rows[len(rows)-1].cellStack) > 0 {
				row := rows[len(rows)-1]
				_, _ = row.cellStack[len(row.cellStack)-1].WriteString(text)
			} else if current != nil {
				_, _ = current.looseText.WriteString(text)
			}
		}
	}
	if current != nil || len(rows) != 0 || skipTextDepth != 0 {
		return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "invalid_html"}
	}
	if !logoutSeen {
		return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "unrecognized_response"}
	}
	matches := make([]*parsedBonusForm, 0, 1)
	for _, form := range forms {
		if form.row != nil {
			form.cells = append([]string(nil), form.row.cells...)
			form.cellsOverflow = form.row.cellsOverflow
		}
		if form.selectorCount > 1 {
			return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "selector_ambiguous"}
		}
		for _, value := range form.optionValues {
			if value == selector && (form.selectorCount != 1 || form.selector != selector) {
				return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "selector_ambiguous"}
			}
		}
		if form.selectorCount == 1 && form.selector == selector {
			matches = append(matches, form)
		}
	}
	if len(matches) == 0 {
		return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "selector_not_found"}
	}
	if len(matches) != 1 {
		return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "selector_ambiguous"}
	}
	match := matches[0]
	if match.cellsOverflow || len(match.cells) < 2 || len(match.cells) > 16 {
		return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "unrecognized_offer"}
	}
	if !match.actionSupported || match.method != "post" {
		return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "unrecognized_offer"}
	}
	availability := site.BonusReviewAvailabilityUnknown
	if match.submitSeen {
		availability = site.BonusReviewAvailabilityDisabled
		if match.enabledSubmit {
			availability = site.BonusReviewAvailabilityAvailable
		}
	}
	inputMode := site.BonusReviewInputNone
	if match.requiresInput {
		inputMode = site.BonusReviewInputRequired
	}
	if match.unsupportedInput || match.submitCount > 1 {
		inputMode = site.BonusReviewInputUnsupported
		availability = site.BonusReviewAvailabilityUnknown
	}
	review := domain.BonusOfferReview{
		SiteID:        "tjupt",
		Selector:      selector,
		Balance:       parseBonusBalance(body),
		Columns:       append([]string(nil), match.cells...),
		Availability:  availability,
		InputMode:     inputMode,
		ActionMethod:  match.method,
		ActionRouteID: match.actionRouteID,
		FormShapeID:   bonusFormShapeID(match.formShape),
		EvidenceBasis: []string{
			site.BonusReviewBasisAuthenticated,
			site.BonusReviewBasisExactSelector,
			site.BonusReviewBasisExactForm,
			site.BonusReviewBasisOneRequest,
			site.BonusReviewBasisSiteClaimOnly,
			site.BonusReviewBasisNoSubmission,
		},
	}
	reviewID, err := site.ComputeBonusReviewID(review)
	if err != nil {
		return domain.BonusOfferReview{}, usage, &bonusParseError{reason: "unrecognized_offer"}
	}
	review.ReviewID = reviewID
	return review, usage, nil
}

func strictBonusAttributes(attrs []html.Attribute) (map[string]string, error) {
	if len(attrs) > maxBonusAttributesPerTag {
		return nil, fmt.Errorf("too many attributes")
	}
	result := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		key := strings.ToLower(attr.Key)
		if key == "" || len(key) > 128 || len(attr.Val) > 16<<10 || !utf8.ValidString(key) || !utf8.ValidString(attr.Val) || hasBonusControlRune(key) || hasBonusControlRune(attr.Val) {
			return nil, fmt.Errorf("invalid attribute")
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate attribute")
		}
		result[key] = attr.Val
	}
	return result, nil
}

func hasBonusControlRune(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func classifyBonusFormAction(attrs map[string]string) (string, string, bool) {
	method := strings.ToLower(attrs["method"])
	if method == "" {
		method = "get"
	}
	if method != "get" && method != "post" {
		method = "unsupported"
	}
	rawAction := strings.Trim(attrs["action"], " ")
	ref, err := url.Parse(rawAction)
	if err != nil || ref.IsAbs() || ref.Host != "" || ref.User != nil || ref.Fragment != "" || ref.RawPath != "" {
		return method, bonusUnknownActionRoute, false
	}
	path := strings.TrimPrefix(ref.Path, "/")
	if path == "" {
		path = bonusReviewPath
	}
	query, queryErr := url.ParseQuery(ref.RawQuery)
	if queryErr != nil {
		return method, bonusUnknownActionRoute, false
	}
	querySupported := len(query) == 0 || len(query) == 1 && len(query["action"]) == 1 && query.Get("action") == "exchange"
	supported := method == "post" && path == bonusReviewPath && querySupported && bonusFormSubmissionAttributesSupported(attrs)
	if !supported {
		return method, bonusUnknownActionRoute, false
	}
	return method, BonusExchangeRouteID, true
}

func isExactBonusLogout(raw string) bool {
	ref, err := url.Parse(raw)
	if err != nil || ref.IsAbs() || ref.Host != "" || ref.User != nil || ref.RawPath != "" || ref.RawQuery != "" || ref.Fragment != "" {
		return false
	}
	return strings.TrimPrefix(ref.Path, "/") == "logout.php"
}

func bonusFormSubmissionAttributesSupported(attrs map[string]string) bool {
	for key := range attrs {
		if strings.HasPrefix(key, "on") {
			return false
		}
	}
	if value := attrs["enctype"]; value != "" && !strings.EqualFold(value, "application/x-www-form-urlencoded") {
		return false
	}
	if value := attrs["target"]; value != "" && !strings.EqualFold(value, "_self") {
		return false
	}
	if value := attrs["accept-charset"]; value != "" && !strings.EqualFold(value, "utf-8") {
		return false
	}
	return true
}

func observeBonusControl(form *parsedBonusForm, tag string, attrs map[string]string) {
	name := attrs["name"]
	typeValue := strings.ToLower(attrs["type"])
	if tag == "input" && typeValue == "" {
		typeValue = "text"
	}
	if tag == "button" && typeValue == "" {
		typeValue = "submit"
	}
	form.formShape = append(form.formShape, tag+"\x00"+name+"\x00"+typeValue)
	_, hasExternalForm := attrs["form"]
	if hasExternalForm {
		form.unsupportedInput = true
	}
	for key := range attrs {
		if strings.HasPrefix(key, "on") {
			form.actionSupported = false
		}
	}
	if name == "option" {
		form.selectorCount++
		form.optionValues = append(form.optionValues, attrs["value"])
		_, disabled := attrs["disabled"]
		if tag != "input" || typeValue != "hidden" || disabled || hasExternalForm || validateBonusSelector(attrs["value"]) != nil {
			form.unsupportedInput = true
			return
		}
		form.selector = attrs["value"]
		return
	}
	_, disabled := attrs["disabled"]
	if disabled && !(tag == "input" && typeValue == "submit") && !(tag == "button" && typeValue == "submit") {
		return
	}
	switch tag {
	case "select", "textarea":
		form.requiresInput = true
		return
	case "button":
		if typeValue == "submit" {
			if bonusSubmitOverridesAction(attrs) {
				form.actionSupported = false
			}
			form.submitSeen = true
			form.submitCount++
			if _, disabled := attrs["disabled"]; !disabled {
				form.enabledSubmit = true
			}
			return
		}
		if typeValue != "button" && typeValue != "reset" {
			form.unsupportedInput = true
		}
		return
	case "input":
		switch typeValue {
		case "hidden", "button", "reset":
			return
		case "submit":
			if bonusSubmitOverridesAction(attrs) {
				form.actionSupported = false
			}
			form.submitSeen = true
			form.submitCount++
			if _, disabled := attrs["disabled"]; !disabled {
				form.enabledSubmit = true
			}
			return
		case "text", "number", "email", "search", "url", "tel", "date", "datetime-local", "month", "week", "time", "range", "color", "checkbox", "radio":
			form.requiresInput = true
			return
		default:
			form.unsupportedInput = true
			return
		}
	}
}

func bonusSubmitOverridesAction(attrs map[string]string) bool {
	for _, key := range []string{"form", "formaction", "formmethod", "formenctype", "formtarget", "formnovalidate"} {
		if _, exists := attrs[key]; exists {
			return true
		}
	}
	return false
}

func bonusFormShapeID(parts []string) string {
	hash := sha256.New()
	hash.Write([]byte("ptctl-tjupt-bonus-form-shape-v1\x00"))
	var length [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		hash.Write(length[:])
		hash.Write([]byte(part))
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func appendBonusCell(form *parsedBonusForm, raw string) {
	text := normalizeBonusVisibleText(raw)
	if text == "" {
		return
	}
	if len(form.cells) >= 16 {
		form.cellsOverflow = true
		return
	}
	form.cells = append(form.cells, text)
}

func appendBonusRowCell(row *bonusRowState, raw string) {
	text := normalizeBonusVisibleText(raw)
	if text == "" {
		return
	}
	if len(row.cells) >= 16 {
		row.cellsOverflow = true
		return
	}
	row.cells = append(row.cells, text)
}

func normalizeBonusVisibleText(raw string) string {
	// x/net/html has already decoded character references and separated markup
	// from text tokens. Unescaping or stripping tags a second time would change
	// what the authenticated page actually displayed.
	return strings.Join(strings.Fields(strings.ReplaceAll(raw, "\u00a0", " ")), " ")
}

func bonusParseStopReason(err error) string {
	var parseErr *bonusParseError
	if errors.As(err, &parseErr) {
		switch parseErr.reason {
		case "selector_not_found", "selector_ambiguous", "token_budget_exceeded", "form_budget_exceeded", "field_budget_exceeded", "visible_text_budget_exceeded", "invalid_html", "ambiguous_form_structure", "unrecognized_response", "unrecognized_offer":
			return parseErr.reason
		}
	}
	return "unrecognized_response"
}
