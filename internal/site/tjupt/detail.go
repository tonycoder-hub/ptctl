package tjupt

import (
	"context"
	"errors"
	"fmt"
	"html"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

const (
	DetailOrigin  = "https://www.tjupt.org"
	DetailRouteID = "tjupt.details_by_id_no_hit.v1"

	detailPath   = "details.php"
	detailAccept = "text/html,application/xhtml+xml;q=0.9"
)

var (
	detailHeadingPattern = regexp.MustCompile(`(?is)<h1\b([^>]*)>(.*?)</h1\s*>`)
	topIDAttribute       = regexp.MustCompile(`(?i)(?:^|\s)id\s*=\s*(?:"top"|'top'|top)(?:\s|$)`)
	anchorTagPattern     = regexp.MustCompile(`(?is)<a\b([^>]*)>`)
	hrefAttribute        = regexp.MustCompile(`(?is)(?:^|\s)href\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	divPattern           = regexp.MustCompile(`(?is)<div\b([^>]*)>(.*?)</div\s*>`)
	peerIDAttribute      = regexp.MustCompile(`(?i)(?:^|\s)id\s*=\s*(?:"peercount"|'peercount'|peercount)(?:\s|$)`)
	decimalPattern       = regexp.MustCompile(`[0-9]+`)
)

var (
	_ site.TorrentDetailReader  = (*Adapter)(nil)
	_ site.TorrentDetailSession = (*torrentDetailSession)(nil)
)

func (a *Adapter) ValidateTorrentDetailRef(ref domain.TorrentRef) error {
	if _, err := a.TorrentDetailConfig(); err != nil {
		return err
	}
	return validateCanonicalTorrentRef(ref)
}

func (a *Adapter) TorrentDetailConfig() (site.TorrentDetailConfig, error) {
	if a == nil || a.baseURL != DefaultBaseURL {
		return site.TorrentDetailConfig{}, fmt.Errorf("TJUPT torrent detail read is unavailable for this origin")
	}
	config, err := site.NewTorrentDetailConfig(DetailOrigin, DetailRouteID)
	if err != nil {
		return site.TorrentDetailConfig{}, fmt.Errorf("TJUPT torrent detail configuration is invalid")
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
	if credential.Method() != domain.AuthMethodCookieHeader {
		return nil, fmt.Errorf("TJUPT torrent detail requires cookie_header authentication")
	}
	if !validCookieHeader(credential.SecretValue()) {
		return nil, fmt.Errorf("TJUPT cookie credential is invalid")
	}
	client, err := a.clientFactory()(DefaultBaseURL, credential.SecretValue(), 2*time.Second)
	if err != nil || client == nil {
		return nil, fmt.Errorf("open TJUPT torrent detail session failed")
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
		return nil, receipt, fmt.Errorf("TJUPT torrent detail limits are invalid")
	}
	if err := validateCanonicalTorrentRef(ref); err != nil {
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
		return nil, receipt, fmt.Errorf("TJUPT torrent detail session is closed")
	}
	if session.used {
		session.mu.Unlock()
		receipt.StopReason = "request_budget_exhausted"
		receipt.Used.RequestsAttempted = session.RequestsMade()
		return nil, receipt, fmt.Errorf("TJUPT torrent detail request budget is exhausted")
	}
	session.used = true
	client := session.client
	session.mu.Unlock()

	response, requestErr := client.GetOnce(ctx, detailPath, url.Values{"id": {ref.RemoteID}}, detailAccept, limits.MaxResponseBytes, limits.MaxResponseHeaderBytes)
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
			return nil, receipt, fmt.Errorf("TJUPT torrent detail request stopped: %w", context.Canceled)
		}
		if errors.Is(requestErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			receipt.StopReason = "context_done"
			return nil, receipt, fmt.Errorf("TJUPT torrent detail request stopped: %w", context.DeadlineExceeded)
		}
		if receipt.Used.RequestsAttempted != 1 {
			receipt.StopReason = "request_accounting_invalid"
			return nil, receipt, fmt.Errorf("TJUPT torrent detail request accounting is invalid")
		}
		receipt.StopReason = "site_request_failed"
		return nil, receipt, fmt.Errorf("TJUPT torrent detail request failed")
	}
	if receipt.Used.RequestsAttempted != 1 {
		receipt.StopReason = "request_accounting_invalid"
		return nil, receipt, fmt.Errorf("TJUPT torrent detail request accounting is invalid")
	}
	if !response.ResponseBytesKnown || response.ResponseBytesRead != int64(len(response.Body)) ||
		response.ObservedAtStart.IsZero() || response.ObservedAtEnd.IsZero() || response.ObservedAtEnd.Before(response.ObservedAtStart) ||
		int64(len(response.Body)) > limits.MaxResponseBytes {
		receipt.StopReason = "response_accounting_invalid"
		return nil, receipt, fmt.Errorf("TJUPT torrent detail response accounting is invalid")
	}
	if stopReason, err := classifyDetailResponse(response, ref); err != nil {
		receipt.StopReason = stopReason
		return nil, receipt, err
	}
	detail, err := parseTorrentDetail(response.Body, ref)
	if err != nil {
		receipt.StopReason = "unrecognized_response"
		return nil, receipt, fmt.Errorf("TJUPT torrent detail page was not recognized")
	}
	receipt.Complete = true
	observed, err := site.NewObservedTorrentDetail(detail, receipt)
	if err != nil {
		receipt.Complete = false
		receipt.StopReason = "response_accounting_invalid"
		return nil, receipt, fmt.Errorf("TJUPT torrent detail observation is invalid")
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
		return fmt.Errorf("close TJUPT torrent detail session failed")
	}
	return nil
}

func classifyDetailResponse(response httpguard.StrictResponse, ref domain.TorrentRef) (string, error) {
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return "authentication_required", fmt.Errorf("TJUPT authentication was rejected")
	case response.StatusCode == http.StatusNotFound:
		return "not_found", fmt.Errorf("TJUPT torrent detail was not found")
	case response.StatusCode == http.StatusTooManyRequests:
		return "rate_limited", fmt.Errorf("TJUPT rate limited the torrent detail request")
	case response.StatusCode >= 300 && response.StatusCode <= 399:
		return "redirect_rejected", fmt.Errorf("TJUPT torrent detail redirect was rejected")
	case response.StatusCode != http.StatusOK:
		return "http_status_rejected", fmt.Errorf("TJUPT torrent detail response status was rejected")
	case len(response.Body) == 0:
		return "empty_response", fmt.Errorf("TJUPT torrent detail response was empty")
	case !utf8.Valid(response.Body):
		return "invalid_text_encoding", fmt.Errorf("TJUPT torrent detail response text was invalid")
	}
	switch response.MediaType {
	case "", "text/html", "application/xhtml+xml":
	default:
		return "content_type_rejected", fmt.Errorf("TJUPT torrent detail content type was rejected")
	}
	if isLoginPage(nil, response.Body) {
		return "authentication_required", fmt.Errorf("TJUPT authentication was required")
	}
	if challengeMarker.Match(response.Body) {
		return "challenge_response", fmt.Errorf("TJUPT returned an interactive challenge")
	}
	return "", nil
}

func parseTorrentDetail(body []byte, ref domain.TorrentRef) (domain.TorrentDetail, error) {
	if !utf8.Valid(body) || !hasExactLogoutLink(body) {
		return domain.TorrentDetail{}, fmt.Errorf("authenticated detail markers are missing")
	}
	title := ""
	for _, match := range detailHeadingPattern.FindAllSubmatch(body, 32) {
		if len(match) != 3 || !topIDAttribute.Match(match[1]) {
			continue
		}
		title = plainText(string(match[2]))
		break
	}
	if !validDetailTitle(title) {
		return domain.TorrentDetail{}, fmt.Errorf("detail title is invalid")
	}
	idBound, downloadBound := detailLinksForRef(body, ref.RemoteID)
	if !idBound {
		return domain.TorrentDetail{}, fmt.Errorf("detail reference marker is missing")
	}
	seeders, leechers := parseDetailPeerCounts(body)
	basis := []string{
		site.DetailBasisAuthenticated,
		site.DetailBasisExactRef,
		site.DetailBasisOneRequest,
		site.DetailBasisSiteClaimOnly,
	}
	if downloadBound {
		basis = append(basis, site.DetailBasisDownloadRef)
	}
	return domain.TorrentDetail{
		Ref:                       ref,
		DisplayTitle:              title,
		Seeders:                   seeders,
		Leechers:                  leechers,
		DownloadReferenceObserved: downloadBound,
		EvidenceBasis:             basis,
	}, nil
}

func validDetailTitle(value string) bool {
	if value == "" || len(value) > 4<<10 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func detailLinksForRef(body []byte, remoteID string) (bool, bool) {
	idBound := false
	downloadBound := false
	for _, anchor := range anchorTagPattern.FindAllSubmatch(body, 4096) {
		if len(anchor) != 2 {
			continue
		}
		match := hrefAttribute.FindSubmatch(anchor[1])
		if len(match) == 0 {
			continue
		}
		raw := ""
		for _, candidate := range match[1:] {
			if len(candidate) != 0 {
				raw = string(candidate)
				break
			}
		}
		parsed, err := url.Parse(html.UnescapeString(raw))
		if err != nil {
			continue
		}
		values := parsed.Query()
		base := exactInternalRoute(parsed)
		key := "id"
		if base == "report.php" {
			key = "torrent"
		}
		if base != "download.php" && base != "viewsnatches.php" && base != "report.php" {
			continue
		}
		items := values[key]
		if len(items) != 1 || items[0] != remoteID {
			continue
		}
		idBound = true
		downloadBound = downloadBound || base == "download.php"
	}
	return idBound, downloadBound
}

func parseDetailPeerCounts(body []byte) (*int, *int) {
	var content []byte
	for _, match := range divPattern.FindAllSubmatch(body, 128) {
		if len(match) == 3 && peerIDAttribute.Match(match[1]) {
			content = match[2]
			break
		}
	}
	if len(content) == 0 {
		return nil, nil
	}
	values := decimalPattern.FindAllString(plainText(string(content)), 3)
	if len(values) != 2 {
		return nil, nil
	}
	seeders, seedErr := strconv.ParseInt(values[0], 10, 32)
	leechers, leechErr := strconv.ParseInt(values[1], 10, 32)
	if seedErr != nil || leechErr != nil || seeders < 0 || leechers < 0 || seeders > math.MaxInt32 || leechers > math.MaxInt32 {
		return nil, nil
	}
	seedersValue, leechersValue := int(seeders), int(leechers)
	return &seedersValue, &leechersValue
}

func hasExactLogoutLink(body []byte) bool {
	for _, anchor := range anchorTagPattern.FindAllSubmatch(body, 4096) {
		if len(anchor) != 2 {
			continue
		}
		match := hrefAttribute.FindSubmatch(anchor[1])
		if len(match) == 0 {
			continue
		}
		raw := ""
		for _, candidate := range match[1:] {
			if len(candidate) != 0 {
				raw = string(candidate)
				break
			}
		}
		parsed, err := url.Parse(html.UnescapeString(raw))
		if err == nil && exactInternalRoute(parsed) == "logout.php" {
			return true
		}
	}
	return false
}

func exactInternalRoute(parsed *url.URL) string {
	if parsed == nil || parsed.IsAbs() || parsed.Host != "" || parsed.RawPath != "" || strings.Contains(parsed.Path, "\\") {
		return ""
	}
	route := strings.TrimPrefix(parsed.Path, "/")
	if route == "" || strings.Contains(route, "/") {
		return ""
	}
	return strings.ToLower(route)
}
