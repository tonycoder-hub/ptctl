package mteam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

const (
	MetafileOrigin = Origin
	// MetafileRouteID names the linked download: one POST to
	// api/torrent/genDlToken for a signed link, one GET of that link, and, only
	// when the link redirects to another M-Team host, one GET of the target.
	MetafileRouteID = "mteam.download_by_token.v1"

	tokenPath      = "api/torrent/genDlToken"
	signedLinkPath = "/api/rss/dlv2"
	metafileAccept = "application/x-bittorrent,application/octet-stream;q=0.9"

	// metafileLinkRequests counts the token request and one redirect.
	metafileLinkRequests = 2
	maxTokenBytes        = 64 << 10
)

// metafileLimitMarkers identify a refusal for the per-torrent daily download
// limit or a request burst, which the API reports with the generic code "1".
// The text is only compared, never surfaced.
var metafileLimitMarkers = []string{"最多下載", "最多下载", "請求太多", "请求太多"}

// siteDomains are the registrable domains a signed link or its redirect target
// may use. Any other host, or a non-HTTPS target such as magnet:, is never
// contacted.
var siteDomains = []string{"m-team.cc", "m-team.io"}

var (
	_ site.MetafileFetcher      = (*Adapter)(nil)
	_ site.MetafileFetchSession = (*metafileFetchSession)(nil)
)

type linkClient interface {
	GetLinkOnce(context.Context, httpguard.SecretLink, string, int64, int64, func(*url.URL) bool) (httpguard.StrictResponse, httpguard.SecretLink, error)
	RequestsMade() int
	Close() error
}

// linkClientFactory binds one credential-free client to one link host.
type linkClientFactory func(baseURL string) (linkClient, error)

func newLinkClient(baseURL string) (linkClient, error) {
	client, err := httpguard.NewWithoutCredential(baseURL, 0)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (a *Adapter) ValidateMetafileRef(ref domain.TorrentRef) error {
	return validateTorrentRef(ref)
}

func (a *Adapter) MetafileFetchConfig() (site.MetafileFetchConfig, error) {
	config, err := site.NewLinkedMetafileFetchConfig(MetafileOrigin, MetafileRouteID, metafileLinkRequests)
	if err != nil {
		return site.MetafileFetchConfig{}, fmt.Errorf("M-Team metafile fetch configuration is invalid")
	}
	return config, nil
}

func (a *Adapter) OpenMetafileFetchSession(ctx context.Context, credential site.Credential) (site.MetafileFetchSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, err := a.MetafileFetchConfig()
	if err != nil {
		return nil, err
	}
	if credential.Method() != domain.AuthMethodAPIKeyHeader {
		return nil, fmt.Errorf("M-Team metafile fetch requires api_key_header authentication")
	}
	if !validAPIKey(credential.SecretValue()) {
		return nil, fmt.Errorf("M-Team API key is malformed: %w", site.ErrCredentialMalformed)
	}
	client, err := a.clientFactory()(DefaultBaseURL, credential.SecretValue(), minRequestInterval)
	if err != nil || client == nil {
		return nil, fmt.Errorf("open M-Team metafile fetch session failed")
	}
	return &metafileFetchSession{api: client, newLink: a.linkClientFactory(), pause: a.pauseFunc(), config: config}, nil
}

func (a *Adapter) linkClientFactory() linkClientFactory {
	if a != nil && a.newLinkClient != nil {
		return a.newLinkClient
	}
	return newLinkClient
}

func (a *Adapter) pauseFunc() func(context.Context, time.Duration) error {
	if a != nil && a.pause != nil {
		return a.pause
	}
	return pause
}

func pause(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type metafileFetchSession struct {
	api     guardedClient
	newLink linkClientFactory
	pause   func(context.Context, time.Duration) error
	config  site.MetafileFetchConfig
	mu      sync.Mutex
	links   []linkClient
	usage   site.MetafileFetchUsage
	used    bool
	closed  bool
}

// fetchStop is a failed step: the public stop reason and the adapter error.
type fetchStop struct {
	reason string
	err    error
}

func stopWith(reason, format string, args ...any) *fetchStop {
	return &fetchStop{reason: reason, err: fmt.Errorf(format, args...)}
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
	if err := limits.Validate(); err != nil || limits.MaxLinkRequests != session.config.LinkRequests {
		receipt.StopReason = "invalid_limits"
		return nil, receipt, fmt.Errorf("M-Team metafile fetch limits are invalid")
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
		return nil, receipt, fmt.Errorf("M-Team metafile fetch session is closed")
	}
	if session.used {
		receipt.Used.RequestsAttempted = session.usage.RequestsAttempted
		receipt.Used.LinkRequestsAttempted = session.usage.LinkRequestsAttempted
		session.mu.Unlock()
		receipt.StopReason = "request_budget_exhausted"
		return nil, receipt, fmt.Errorf("M-Team metafile fetch request budget is exhausted")
	}
	session.used = true
	session.mu.Unlock()

	raw, stop := session.fetch(ctx, ref, limits, &receipt)
	session.mu.Lock()
	session.usage = receipt.Used
	session.mu.Unlock()
	if stop != nil {
		receipt.StopReason = stop.reason
		return nil, receipt, stop.err
	}
	fetched, err := site.NewLinkedFetchedMetafile(ref, session.config.Origin, session.config.RouteID, receipt.ObservedAtStart, receipt.ObservedAtEnd, receipt.Used.LinkRequestsAttempted, raw)
	if err != nil {
		receipt.StopReason = "response_authority_invalid"
		return nil, receipt, fmt.Errorf("M-Team metafile response authority could not be created")
	}
	receipt.Complete = true
	return fetched, receipt, nil
}

// fetch runs the linked route and returns the metafile response body. It
// records every request in receipt as it is made.
func (session *metafileFetchSession) fetch(ctx context.Context, ref domain.TorrentRef, limits site.MetafileFetchLimits, receipt *site.MetafileFetchReceipt) ([]byte, *fetchStop) {
	started := false
	observe := func(response httpguard.StrictResponse) {
		if !started && !response.ObservedAtStart.IsZero() {
			receipt.ObservedAtStart = response.ObservedAtStart
			started = true
		}
		if !response.ObservedAtEnd.IsZero() {
			receipt.ObservedAtEnd = response.ObservedAtEnd
		} else {
			receipt.ObservedAtEnd = time.Now().UTC()
		}
	}

	response, err := session.api.PostReadOnce(ctx, tokenPath, emptyFormMediaType, formBody("id", ref.RemoteID), apiAccept, maxRequestBytes, maxTokenBytes, limits.MaxResponseHeaderBytes)
	receipt.Used.LinkRequestsAttempted = session.api.RequestsMade()
	observe(response)
	if stop := requestStop(ctx, err, receipt.Used.LinkRequestsAttempted, response); stop != nil {
		return nil, stop
	}
	link, stop := classifyTokenResponse(response, ref)
	if stop != nil {
		return nil, stop
	}

	// The token request and the link GET reach the same API, so keep the
	// interval the site asks for between them.
	if err := session.pause(ctx, minRequestInterval); err != nil {
		return nil, stopWith("context_done", "M-Team metafile request stopped: %w", err)
	}
	response, next, stop := session.get(ctx, link, limits, approvedRedirect)
	if stop != nil {
		receipt.Used.RequestsAttempted = 1
		observe(response)
		return nil, stop
	}
	observe(response)
	if response.StatusCode >= 300 && response.StatusCode <= 399 && !next.IsZero() {
		receipt.Used.LinkRequestsAttempted++
		response, _, stop = session.get(ctx, next, limits, nil)
		observe(response)
		if stop != nil {
			receipt.Used.RequestsAttempted = 1
			return nil, stop
		}
	}
	receipt.Used.RequestsAttempted = 1
	receipt.Used.ResponseBytesRead = response.ResponseBytesRead
	receipt.Used.ResponseBytesKnown = response.ResponseBytesKnown
	if stop := classifyMetafileResponse(response); stop != nil {
		return nil, stop
	}
	return response.Body, nil
}

// get performs one GET of link on a fresh credential-free client bound to the
// link's own host.
func (session *metafileFetchSession) get(ctx context.Context, link httpguard.SecretLink, limits site.MetafileFetchLimits, approve func(*url.URL) bool) (httpguard.StrictResponse, httpguard.SecretLink, *fetchStop) {
	target := link.URL()
	if target == nil || !siteHost(target.Host) {
		return httpguard.StrictResponse{}, httpguard.SecretLink{}, stopWith("redirect_rejected", "M-Team download link host was rejected")
	}
	client, err := session.newLink("https://" + target.Host + "/")
	if err != nil || client == nil {
		return httpguard.StrictResponse{}, httpguard.SecretLink{}, stopWith("site_request_failed", "open M-Team download link client failed")
	}
	session.mu.Lock()
	session.links = append(session.links, client)
	session.mu.Unlock()
	response, next, err := client.GetLinkOnce(ctx, link, metafileAccept, limits.MaxResponseBytes, limits.MaxResponseHeaderBytes, approve)
	if stop := requestStop(ctx, err, client.RequestsMade(), response); stop != nil {
		return response, httpguard.SecretLink{}, stop
	}
	return response, next, nil
}

// requestStop checks one request's error and accounting.
func requestStop(ctx context.Context, err error, requests int, response httpguard.StrictResponse) *fetchStop {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return stopWith("context_done", "M-Team metafile request stopped: %w", context.Canceled)
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return stopWith("context_done", "M-Team metafile request stopped: %w", context.DeadlineExceeded)
		}
		if requests != 1 {
			return stopWith("request_accounting_invalid", "M-Team metafile request accounting is invalid")
		}
		if errors.Is(err, httpguard.ErrBlockedAddress) {
			return stopWith("site_request_failed", "M-Team metafile request failed: %w", site.ErrAddressBlocked)
		}
		return stopWith("site_request_failed", "M-Team metafile request failed")
	}
	if requests != 1 {
		return stopWith("request_accounting_invalid", "M-Team metafile request accounting is invalid")
	}
	if !response.ResponseBytesKnown || response.ResponseBytesRead != int64(len(response.Body)) ||
		response.ObservedAtStart.IsZero() || response.ObservedAtEnd.IsZero() || response.ObservedAtEnd.Before(response.ObservedAtStart) {
		return stopWith("response_accounting_invalid", "M-Team metafile response accounting is invalid")
	}
	return nil
}

// classifyTokenResponse maps the genDlToken answer to the signed link it
// carries. The link is a secret: it is never returned in an error.
func classifyTokenResponse(response httpguard.StrictResponse, ref domain.TorrentRef) (httpguard.SecretLink, *fetchStop) {
	code, message, data, stop := decodeAPIAnswer(response)
	if stop != nil {
		return httpguard.SecretLink{}, stop
	}
	switch code {
	case "0":
		var raw string
		if json.Unmarshal(data, &raw) != nil {
			break
		}
		link, err := httpguard.NewSecretLink(raw)
		if err != nil || !validSignedLink(link.URL(), ref) {
			break
		}
		return link, nil
	case "401":
		return httpguard.SecretLink{}, stopWith("authentication_required", "M-Team API rejected the request: %w", site.ErrCredentialRejected)
	case "4":
		return httpguard.SecretLink{}, stopWith("rate_limited", "M-Team API refused the request: %w", site.ErrRateLimited)
	case "1":
		if notFoundRefusal(message, data) {
			return httpguard.SecretLink{}, stopWith("not_found", "M-Team torrent was not found")
		}
		if limitRefusal(message) {
			return httpguard.SecretLink{}, stopWith("rate_limited", "M-Team refused the download: %w", site.ErrRateLimited)
		}
	}
	return httpguard.SecretLink{}, stopWith("unrecognized_response", "M-Team download token response was not recognized")
}

// classifyMetafileResponse accepts a bencoded body and maps the API's JSON
// refusals, which answer with HTTP 200, to stop reasons. The token request
// signs a link for any ID, so an unknown torrent is only reported here.
func classifyMetafileResponse(response httpguard.StrictResponse) *fetchStop {
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return stopWith("authentication_required", "M-Team rejected the download link")
	case response.StatusCode == http.StatusTooManyRequests:
		return stopWith("rate_limited", "M-Team refused the download: %w", site.ErrRateLimited)
	case response.StatusCode >= 300 && response.StatusCode <= 399:
		return stopWith("redirect_rejected", "M-Team download redirect was rejected")
	case response.StatusCode != http.StatusOK:
		return stopWith("http_status_rejected", "M-Team metafile response status was rejected")
	case len(response.Body) == 0:
		return stopWith("empty_response", "M-Team metafile response was empty")
	}
	if response.Body[0] == 'd' {
		switch response.MediaType {
		case "", "application/x-bittorrent", "application/octet-stream", "application/download", "application/force-download", "binary/octet-stream":
			return nil
		}
		return stopWith("content_type_rejected", "M-Team metafile content type was rejected")
	}
	if code, message, data, stop := decodeAPIAnswer(response); stop == nil {
		switch {
		case code == "401":
			return stopWith("authentication_required", "M-Team rejected the download link")
		case code == "1" && notFoundRefusal(message, data):
			return stopWith("not_found", "M-Team torrent was not found")
		case code == "4", code == "1" && limitRefusal(message):
			return stopWith("rate_limited", "M-Team refused the download: %w", site.ErrRateLimited)
		}
	}
	return stopWith("unrecognized_response", "M-Team metafile response was not recognized")
}

// decodeAPIAnswer reads one API envelope. The message is returned only for
// comparison against known refusals.
func decodeAPIAnswer(response httpguard.StrictResponse) (string, *string, json.RawMessage, *fetchStop) {
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return "", nil, nil, stopWith("authentication_required", "M-Team API rejected the request: %w", site.ErrCredentialRejected)
	case response.StatusCode == http.StatusTooManyRequests:
		return "", nil, nil, stopWith("rate_limited", "M-Team API refused the request: %w", site.ErrRateLimited)
	case response.StatusCode >= 300 && response.StatusCode <= 399:
		return "", nil, nil, stopWith("redirect_rejected", "M-Team API redirect was rejected")
	case response.StatusCode != http.StatusOK:
		return "", nil, nil, stopWith("http_status_rejected", "M-Team API response status was rejected")
	case len(response.Body) == 0:
		return "", nil, nil, stopWith("empty_response", "M-Team API response was empty")
	case !utf8.Valid(response.Body):
		return "", nil, nil, stopWith("unrecognized_response", "M-Team API response text was invalid")
	}
	switch response.MediaType {
	case "application/json", "text/plain", "":
	default:
		return "", nil, nil, stopWith("content_type_rejected", "M-Team API response was not JSON")
	}
	var envelope struct {
		Code    flexText        `json:"code"`
		Message *string         `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response.Body, &envelope); err != nil || !envelope.Code.set {
		return "", nil, nil, stopWith("unrecognized_response", "M-Team API response was not recognized")
	}
	return envelope.Code.value, envelope.Message, envelope.Data, nil
}

func emptyData(data json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(data))
	return trimmed == "" || trimmed == "null"
}

// notFoundRefusal reports the exact code "1" answer for a torrent that does not
// exist: a known message and no data, the same pair torrent detail reads.
func notFoundRefusal(message *string, data json.RawMessage) bool {
	if message == nil || !emptyData(data) {
		return false
	}
	_, ok := detailNotFoundMessages[strings.TrimSpace(*message)]
	return ok
}

func limitRefusal(message *string) bool {
	if message == nil {
		return false
	}
	for _, marker := range metafileLimitMarkers {
		if strings.Contains(*message, marker) {
			return true
		}
	}
	return false
}

// validSignedLink accepts only the RSS download endpoint on an M-Team host,
// signed for exactly the requested torrent.
func validSignedLink(link *url.URL, ref domain.TorrentRef) bool {
	if link == nil || !siteHost(link.Host) || link.Path != signedLinkPath {
		return false
	}
	query, err := url.ParseQuery(link.RawQuery)
	if err != nil {
		return false
	}
	tid, sign := query["tid"], query["sign"]
	return len(tid) == 1 && tid[0] == ref.RemoteID && len(sign) == 1 && sign[0] != ""
}

// approvedRedirect lets the link GET report a redirect target only on an
// M-Team host over HTTPS on the default port.
func approvedRedirect(target *url.URL) bool {
	return target != nil && target.Scheme == "https" && siteHost(target.Host)
}

// siteHost reports whether host, which must carry no port, is one of
// siteDomains or a subdomain of one.
func siteHost(host string) bool {
	if host == "" || host != strings.ToLower(host) || strings.HasSuffix(host, ".") {
		return false
	}
	for _, domain := range siteDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func (session *metafileFetchSession) RequestsMade() int {
	if session == nil {
		return 0
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	total := 0
	if session.api != nil {
		total += session.api.RequestsMade()
	}
	for _, client := range session.links {
		total += client.RequestsMade()
	}
	return total
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
	clients := append([]linkClient(nil), session.links...)
	api := session.api
	session.mu.Unlock()
	failed := false
	if api != nil && api.Close() != nil {
		failed = true
	}
	for _, client := range clients {
		if client.Close() != nil {
			failed = true
		}
	}
	if failed {
		return fmt.Errorf("close M-Team metafile fetch session failed")
	}
	return nil
}
