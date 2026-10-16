package mteam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

const (
	DetailOrigin = Origin
	// DetailRouteID names the one POST to api/torrent/detail. The route never
	// calls the separate api/torrent/viewHits endpoint the site's own page
	// uses to count views.
	DetailRouteID = "mteam.torrent_detail_api.v1"

	detailPath = "api/torrent/detail"
)

// detailNotFoundMessages are the exact messages that, with the generic error
// code "1" and no data, mean the ID has no visible torrent. The API uses the
// same code for unrelated refusals, so the code alone is not enough. The text
// is only compared, never surfaced.
var detailNotFoundMessages = map[string]struct{}{
	"種子未找到": {},
	"种子未找到": {},
}

var (
	_ site.TorrentDetailReader  = (*Adapter)(nil)
	_ site.TorrentDetailSession = (*torrentDetailSession)(nil)
)

func (a *Adapter) ValidateTorrentDetailRef(ref domain.TorrentRef) error {
	return validateTorrentRef(ref)
}

func (a *Adapter) TorrentDetailConfig() (site.TorrentDetailConfig, error) {
	config, err := site.NewTorrentDetailConfig(DetailOrigin, DetailRouteID)
	if err != nil {
		return site.TorrentDetailConfig{}, fmt.Errorf("M-Team torrent detail configuration is invalid")
	}
	return config, nil
}

func (a *Adapter) OpenTorrentDetailSession(ctx context.Context, credential site.Credential) (site.TorrentDetailSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, err := a.TorrentDetailConfig()
	if err != nil {
		return nil, err
	}
	if credential.Method() != domain.AuthMethodAPIKeyHeader {
		return nil, fmt.Errorf("M-Team torrent detail requires api_key_header authentication")
	}
	if !validAPIKey(credential.SecretValue()) {
		return nil, fmt.Errorf("M-Team API key is malformed: %w", site.ErrCredentialMalformed)
	}
	client, err := a.clientFactory()(DefaultBaseURL, credential.SecretValue(), minRequestInterval)
	if err != nil || client == nil {
		return nil, fmt.Errorf("open M-Team torrent detail session failed")
	}
	return &torrentDetailSession{client: client, config: config}, nil
}

type torrentDetailSession struct {
	client guardedClient
	config site.TorrentDetailConfig
	mu     sync.Mutex
	used   bool
	closed bool
}

func (session *torrentDetailSession) ReadTorrentDetail(ctx context.Context, ref domain.TorrentRef, limits site.TorrentDetailLimits) (*site.ObservedTorrentDetail, site.TorrentDetailReceipt, error) {
	now := time.Now().UTC()
	receipt := site.TorrentDetailReceipt{
		Effect:          site.TorrentDetailReadEffect,
		Origin:          session.config.Origin,
		RouteID:         session.config.RouteID,
		ObservedAtStart: now,
		ObservedAtEnd:   now,
		Limits:          limits,
	}
	if err := limits.Validate(); err != nil {
		receipt.StopReason = "invalid_limits"
		return nil, receipt, fmt.Errorf("M-Team torrent detail limits are invalid")
	}
	if err := validateTorrentRef(ref); err != nil {
		receipt.StopReason = "invalid_reference"
		return nil, receipt, err
	}
	receipt.Ref = ref
	if err := ctx.Err(); err != nil {
		receipt.StopReason = "context_done"
		return nil, receipt, err
	}

	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		receipt.StopReason = "session_closed"
		return nil, receipt, fmt.Errorf("M-Team torrent detail session is closed")
	}
	if session.used {
		session.mu.Unlock()
		receipt.StopReason = "request_budget_exhausted"
		receipt.Used.RequestsAttempted = session.RequestsMade()
		return nil, receipt, fmt.Errorf("M-Team torrent detail request budget is exhausted")
	}
	session.used = true
	client := session.client
	session.mu.Unlock()

	body := formBody("id", ref.RemoteID)
	response, requestErr := client.PostReadOnce(ctx, detailPath, emptyFormMediaType, body, apiAccept, maxRequestBytes, limits.MaxResponseBytes, limits.MaxResponseHeaderBytes)
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
			return nil, receipt, fmt.Errorf("M-Team torrent detail request stopped: %w", context.Canceled)
		}
		if errors.Is(requestErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			receipt.StopReason = "context_done"
			return nil, receipt, fmt.Errorf("M-Team torrent detail request stopped: %w", context.DeadlineExceeded)
		}
		if receipt.Used.RequestsAttempted != 1 {
			receipt.StopReason = "request_accounting_invalid"
			return nil, receipt, fmt.Errorf("M-Team torrent detail request accounting is invalid")
		}
		receipt.StopReason = "site_request_failed"
		return nil, receipt, fmt.Errorf("M-Team torrent detail request failed")
	}
	if receipt.Used.RequestsAttempted != 1 {
		receipt.StopReason = "request_accounting_invalid"
		return nil, receipt, fmt.Errorf("M-Team torrent detail request accounting is invalid")
	}
	if !response.ResponseBytesKnown || response.ResponseBytesRead != int64(len(response.Body)) ||
		response.ObservedAtStart.IsZero() || response.ObservedAtEnd.IsZero() || response.ObservedAtEnd.Before(response.ObservedAtStart) ||
		int64(len(response.Body)) > limits.MaxResponseBytes {
		receipt.StopReason = "response_accounting_invalid"
		return nil, receipt, fmt.Errorf("M-Team torrent detail response accounting is invalid")
	}
	data, stopReason, err := classifyDetailResponse(response)
	if err != nil {
		receipt.StopReason = stopReason
		return nil, receipt, err
	}
	detail, err := parseTorrentDetail(data, ref)
	if err != nil {
		receipt.StopReason = "unrecognized_response"
		return nil, receipt, fmt.Errorf("M-Team torrent detail response was not recognized")
	}
	receipt.Complete = true
	observed, err := site.NewObservedTorrentDetail(detail, receipt)
	if err != nil {
		receipt.Complete = false
		receipt.StopReason = "response_accounting_invalid"
		return nil, receipt, fmt.Errorf("M-Team torrent detail observation is invalid")
	}
	return observed, receipt, nil
}

func (session *torrentDetailSession) RequestsMade() int {
	if session == nil || session.client == nil {
		return 0
	}
	return session.client.RequestsMade()
}

func (session *torrentDetailSession) Close() error {
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
		return fmt.Errorf("close M-Team torrent detail session failed")
	}
	return nil
}

// classifyDetailResponse maps one detail answer to its data member or to a
// public stop reason. The server's message is compared for the not-found case
// only and is never returned.
func classifyDetailResponse(response httpguard.StrictResponse) (json.RawMessage, string, error) {
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return nil, "authentication_required", fmt.Errorf("M-Team API rejected the request: %w", site.ErrCredentialRejected)
	case response.StatusCode == http.StatusNotFound:
		return nil, "not_found", fmt.Errorf("M-Team torrent detail was not found")
	case response.StatusCode == http.StatusTooManyRequests:
		return nil, "rate_limited", fmt.Errorf("M-Team API refused the request: %w", site.ErrRateLimited)
	case response.StatusCode >= 300 && response.StatusCode <= 399:
		return nil, "redirect_rejected", fmt.Errorf("M-Team API redirect was rejected")
	case response.StatusCode != http.StatusOK:
		return nil, "http_status_rejected", fmt.Errorf("M-Team torrent detail response status was rejected")
	case len(response.Body) == 0:
		return nil, "empty_response", fmt.Errorf("M-Team torrent detail response was empty")
	case !utf8.Valid(response.Body):
		return nil, "invalid_text_encoding", fmt.Errorf("M-Team torrent detail response text was invalid")
	}
	switch response.MediaType {
	case "application/json", "text/plain", "":
	default:
		return nil, "content_type_rejected", fmt.Errorf("M-Team API response was not JSON")
	}
	var envelope struct {
		Code    flexText        `json:"code"`
		Message *string         `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response.Body, &envelope); err != nil || !envelope.Code.set {
		return nil, "unrecognized_response", fmt.Errorf("M-Team API response was not recognized")
	}
	switch envelope.Code.value {
	case "0":
		return envelope.Data, "", nil
	case "401":
		return nil, "authentication_required", fmt.Errorf("M-Team API rejected the request: %w", site.ErrCredentialRejected)
	case "4":
		return nil, "rate_limited", fmt.Errorf("M-Team API refused the request: %w", site.ErrRateLimited)
	case "1":
		data := strings.TrimSpace(string(envelope.Data))
		if envelope.Message != nil && (data == "" || data == "null") {
			if _, ok := detailNotFoundMessages[strings.TrimSpace(*envelope.Message)]; ok {
				return nil, "not_found", fmt.Errorf("M-Team torrent detail was not found")
			}
		}
	}
	return nil, "unrecognized_response", fmt.Errorf("M-Team API refused the torrent detail request")
}

type detailResponse struct {
	ID     flexText `json:"id"`
	Name   *string  `json:"name"`
	Size   flexText `json:"size"`
	Status *struct {
		Seeders        flexText `json:"seeders"`
		Leechers       flexText `json:"leechers"`
		TimesCompleted flexText `json:"timesCompleted"`
	} `json:"status"`
}

// parseTorrentDetail maps api/torrent/detail data. The answer must echo the
// requested ID and carry a name; the counters are optional and dropped when
// malformed. The API's ID echo is what binds the answer to ref, in place of
// the page links an HTML detail page offers.
func parseTorrentDetail(data json.RawMessage, ref domain.TorrentRef) (domain.TorrentDetail, error) {
	var response detailResponse
	if !isJSONObject(data) || json.Unmarshal(data, &response) != nil {
		return domain.TorrentDetail{}, fmt.Errorf("detail data is not an object")
	}
	if !response.ID.set || response.ID.value != ref.RemoteID || response.Name == nil {
		return domain.TorrentDetail{}, fmt.Errorf("detail reference marker is missing")
	}
	title, ok := cleanText(*response.Name, 4<<10)
	if !ok || title == "" {
		return domain.TorrentDetail{}, fmt.Errorf("detail title is invalid")
	}
	detail := domain.TorrentDetail{
		Ref:          ref,
		DisplayTitle: title,
		SizeBytes:    byteCount(response.Size),
		EvidenceBasis: []string{
			site.DetailBasisAuthenticated,
			site.DetailBasisExactRef,
			site.DetailBasisOneRequest,
			site.DetailBasisSiteClaimOnly,
		},
	}
	if status := response.Status; status != nil {
		detail.Seeders = count(status.Seeders)
		detail.Leechers = count(status.Leechers)
		detail.Snatches = count(status.TimesCompleted)
	}
	return detail, nil
}

func validateTorrentRef(ref domain.TorrentRef) error {
	if ref.SiteID != SiteID || !canonicalID(ref.RemoteID) {
		return fmt.Errorf("M-Team torrent reference must use site %q and a canonical numeric ID", SiteID)
	}
	return nil
}

// formBody encodes one multipart field. Callers pass only canonical IDs, so
// the value never needs quoting.
func formBody(name, value string) []byte {
	return []byte("--" + formBoundary + "\r\n" +
		"Content-Disposition: form-data; name=\"" + name + "\"\r\n\r\n" +
		value + "\r\n" +
		"--" + formBoundary + "--\r\n")
}
