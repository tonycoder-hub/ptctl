package tjupt

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const (
	MetafileOrigin  = "https://www.tjupt.org"
	MetafileRouteID = "tjupt.download_by_id.v1"

	metafileDownloadPath = "download.php"
	metafileAccept       = "application/x-bittorrent,application/octet-stream;q=0.9"
)

var (
	_ site.MetafileFetcher      = (*Adapter)(nil)
	_ site.MetafileFetchSession = (*metafileFetchSession)(nil)
)

func (a *Adapter) ValidateMetafileRef(ref domain.TorrentRef) error {
	if _, err := a.MetafileFetchConfig(); err != nil {
		return err
	}
	return validateCanonicalMetafileRef(ref)
}

func (a *Adapter) MetafileFetchConfig() (site.MetafileFetchConfig, error) {
	if a == nil || a.baseURL != DefaultBaseURL {
		return site.MetafileFetchConfig{}, fmt.Errorf("TJUPT metafile fetch is unavailable for this origin")
	}
	config, err := site.NewMetafileFetchConfig(MetafileOrigin, MetafileRouteID)
	if err != nil {
		return site.MetafileFetchConfig{}, fmt.Errorf("TJUPT metafile fetch configuration is invalid")
	}
	return config, nil
}

func validateCanonicalMetafileRef(ref domain.TorrentRef) error {
	if ref.SiteID != "tjupt" || ref.RemoteID == "" || len(ref.RemoteID) > 20 || ref.RemoteID[0] == '0' {
		return fmt.Errorf("TJUPT metafile reference is invalid")
	}
	for i := 0; i < len(ref.RemoteID); i++ {
		if ref.RemoteID[i] < '0' || ref.RemoteID[i] > '9' {
			return fmt.Errorf("TJUPT metafile reference is invalid")
		}
	}
	parsed, err := strconv.ParseUint(ref.RemoteID, 10, 64)
	if err != nil || parsed == 0 || strconv.FormatUint(parsed, 10) != ref.RemoteID {
		return fmt.Errorf("TJUPT metafile reference is invalid")
	}
	return nil
}

func (a *Adapter) OpenMetafileFetchSession(ctx context.Context, credential site.Credential) (site.MetafileFetchSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, err := a.MetafileFetchConfig()
	if err != nil {
		return nil, err
	}
	if credential.Method() != domain.AuthMethodCookieHeader {
		return nil, fmt.Errorf("TJUPT metafile fetch requires cookie_header authentication")
	}
	if !validCookieHeader(credential.SecretValue()) {
		return nil, fmt.Errorf("TJUPT cookie credential is invalid")
	}
	client, err := a.clientFactory()(DefaultBaseURL, credential.SecretValue(), 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("open TJUPT metafile fetch session failed")
	}
	if client == nil {
		return nil, fmt.Errorf("open TJUPT metafile fetch session failed")
	}
	return &metafileFetchSession{client: client, config: config}, nil
}

type metafileFetchSession struct {
	client guardedClient
	config site.MetafileFetchConfig
	mu     sync.Mutex
	used   bool
	closed bool
}

func (session *metafileFetchSession) FetchMetafile(ctx context.Context, ref domain.TorrentRef, limits site.MetafileFetchLimits) (*site.FetchedMetafile, site.MetafileFetchReceipt, error) {
	now := time.Now().UTC()
	receipt := site.MetafileFetchReceipt{
		Effect:          site.MetafileFetchEffect,
		Origin:          session.config.Origin,
		RouteID:         session.config.RouteID,
		ObservedAtStart: now,
		ObservedAtEnd:   now,
		Limits:          limits,
	}
	if err := limits.Validate(); err != nil {
		receipt.StopReason = "invalid_limits"
		return nil, receipt, fmt.Errorf("TJUPT metafile fetch limits are invalid")
	}
	if err := validateCanonicalMetafileRef(ref); err != nil {
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
		return nil, receipt, fmt.Errorf("TJUPT metafile fetch session is closed")
	}
	if session.used {
		session.mu.Unlock()
		receipt.StopReason = "request_budget_exhausted"
		receipt.Used.RequestsAttempted = session.RequestsMade()
		return nil, receipt, fmt.Errorf("TJUPT metafile fetch request budget is exhausted")
	}
	session.used = true
	client := session.client
	session.mu.Unlock()

	response, requestErr := client.GetOnce(
		ctx,
		metafileDownloadPath,
		url.Values{"id": {ref.RemoteID}},
		metafileAccept,
		limits.MaxResponseBytes,
		limits.MaxResponseHeaderBytes,
	)
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
	if receipt.Used.RequestsAttempted != 1 {
		receipt.StopReason = "request_accounting_invalid"
		return nil, receipt, fmt.Errorf("TJUPT metafile request accounting is invalid")
	}
	if requestErr != nil {
		receipt.StopReason = "site_request_failed"
		if errors.Is(requestErr, context.Canceled) {
			return nil, receipt, fmt.Errorf("TJUPT metafile request stopped: %w", context.Canceled)
		}
		if errors.Is(requestErr, context.DeadlineExceeded) {
			return nil, receipt, fmt.Errorf("TJUPT metafile request stopped: %w", context.DeadlineExceeded)
		}
		return nil, receipt, fmt.Errorf("TJUPT metafile request failed")
	}
	if !response.ResponseBytesKnown || response.ResponseBytesRead != int64(len(response.Body)) ||
		response.ObservedAtStart.IsZero() || response.ObservedAtEnd.IsZero() || response.ObservedAtEnd.Before(response.ObservedAtStart) ||
		int64(len(response.Body)) > limits.MaxResponseBytes {
		receipt.StopReason = "response_accounting_invalid"
		return nil, receipt, fmt.Errorf("TJUPT metafile response accounting is invalid")
	}
	if stopReason, classificationErr := classifyMetafileResponse(response.StatusCode, response.MediaType, response.Body); classificationErr != nil {
		receipt.StopReason = stopReason
		return nil, receipt, classificationErr
	}
	fetched, err := site.NewFetchedMetafile(ref, session.config.Origin, session.config.RouteID, receipt.ObservedAtStart, receipt.ObservedAtEnd, response.Body)
	if err != nil {
		receipt.StopReason = "response_authority_invalid"
		return nil, receipt, fmt.Errorf("TJUPT metafile response authority could not be created")
	}
	receipt.Complete = true
	return fetched, receipt, nil
}

func (session *metafileFetchSession) RequestsMade() int {
	if session == nil || session.client == nil {
		return 0
	}
	return session.client.RequestsMade()
}

func (session *metafileFetchSession) Close() error {
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
		return fmt.Errorf("close TJUPT metafile fetch session failed")
	}
	return nil
}

func classifyMetafileResponse(status int, mediaType string, body []byte) (string, error) {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication_required", fmt.Errorf("TJUPT authentication was rejected")
	case status == http.StatusTooManyRequests:
		return "rate_limited", fmt.Errorf("TJUPT rate limited the metafile request")
	case status >= 300 && status <= 399:
		return "redirect_rejected", fmt.Errorf("TJUPT metafile redirect was rejected")
	case status != http.StatusOK:
		return "http_status_rejected", fmt.Errorf("TJUPT metafile response status was rejected")
	case len(body) == 0:
		return "empty_response", fmt.Errorf("TJUPT metafile response was empty")
	}
	if body[0] != 'd' {
		sample := body
		if len(sample) > 64<<10 {
			sample = sample[:64<<10]
		}
		if isLoginPage(nil, sample) {
			return "authentication_required", fmt.Errorf("TJUPT authentication was required")
		}
		if challengeMarker.Match(sample) {
			return "challenge_response", fmt.Errorf("TJUPT returned an interactive challenge")
		}
		return "unrecognized_response", fmt.Errorf("TJUPT metafile response was not recognized")
	}
	switch mediaType {
	case "", "application/x-bittorrent", "application/octet-stream", "application/download", "application/force-download", "binary/octet-stream":
		return "", nil
	default:
		return "content_type_rejected", fmt.Errorf("TJUPT metafile content type was rejected")
	}
}

func validCookieHeader(cookie string) bool {
	if cookie == "" || cookie[0] == ' ' || cookie[len(cookie)-1] == ' ' {
		return false
	}
	for i := 0; i < len(cookie); i++ {
		if cookie[i] < 0x20 || cookie[i] > 0x7e {
			return false
		}
	}
	return true
}
