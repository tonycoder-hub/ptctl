package site

import (
	"context"
	"fmt"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
)

const (
	TorrentDetailReadEffect  = "read_site_torrent_detail"
	DetailBasisAuthenticated = "authenticated_torrent_detail_page"
	DetailBasisExactRef      = "exact_remote_id_page_link"
	DetailBasisOneRequest    = "single_bounded_no_redirect_read"
	DetailBasisSiteClaimOnly = "site_claim_only"
	DetailBasisDownloadRef   = "matching_download_reference_observed"

	defaultTorrentDetailResponseBytes = int64(4 << 20)
	hardTorrentDetailResponseBytes    = int64(8 << 20)
	hardTorrentDetailHeaderBytes      = int64(64 << 10)
)

// TorrentDetailLimits bounds the sole request and all response material held
// by one detail session. MaxRequests is deliberately fixed at one.
type TorrentDetailLimits struct {
	MaxRequests            int   `json:"max_requests"`
	MaxResponseBytes       int64 `json:"max_response_bytes"`
	MaxResponseHeaderBytes int64 `json:"max_response_header_bytes"`
}

func DefaultTorrentDetailLimits() TorrentDetailLimits {
	return TorrentDetailLimits{
		MaxRequests:            1,
		MaxResponseBytes:       defaultTorrentDetailResponseBytes,
		MaxResponseHeaderBytes: hardTorrentDetailHeaderBytes,
	}
}

func (limits TorrentDetailLimits) Validate() error {
	if limits.MaxRequests != 1 {
		return fmt.Errorf("torrent detail request budget must be exactly one")
	}
	if limits.MaxResponseBytes <= 0 || limits.MaxResponseBytes > hardTorrentDetailResponseBytes {
		return fmt.Errorf("torrent detail response byte budget is outside the supported range")
	}
	if limits.MaxResponseHeaderBytes <= 0 || limits.MaxResponseHeaderBytes > hardTorrentDetailHeaderBytes {
		return fmt.Errorf("torrent detail response header budget is outside the supported range")
	}
	return nil
}

type TorrentDetailUsage struct {
	RequestsAttempted  int   `json:"requests_attempted"`
	AutomaticRetries   int   `json:"automatic_retries"`
	RedirectsFollowed  int   `json:"redirects_followed"`
	ResponseBytesRead  int64 `json:"response_bytes_read"`
	ResponseBytesKnown bool  `json:"response_bytes_known"`
}

// TorrentDetailReceipt is safe to serialize. It contains no cookie, request
// URL, redirect target, response body, or server-provided diagnostic text.
type TorrentDetailReceipt struct {
	Effect          string              `json:"effect"`
	Ref             domain.TorrentRef   `json:"ref"`
	Origin          string              `json:"origin"`
	RouteID         string              `json:"route_id"`
	ObservedAtStart time.Time           `json:"observed_at_start"`
	ObservedAtEnd   time.Time           `json:"observed_at_end"`
	Complete        bool                `json:"complete"`
	Limits          TorrentDetailLimits `json:"limits"`
	Used            TorrentDetailUsage  `json:"used"`
	StopReason      string              `json:"stop_reason,omitempty"`
}

// TorrentDetailConfig is credential-free adapter provenance. A caller pins it
// before reading a credential and compares the completed receipt with the
// same canonical origin and route identifier.
type TorrentDetailConfig struct {
	Origin  string `json:"origin"`
	RouteID string `json:"route_id"`
}

func NewTorrentDetailConfig(origin, routeID string) (TorrentDetailConfig, error) {
	config := TorrentDetailConfig{Origin: origin, RouteID: routeID}
	if err := config.Validate(); err != nil {
		return TorrentDetailConfig{}, err
	}
	return config, nil
}

func (config TorrentDetailConfig) Validate() error {
	if err := validateCanonicalOrigin(config.Origin); err != nil {
		return err
	}
	return validateRouteID(config.RouteID)
}

type TorrentDetailReader interface {
	ValidateTorrentDetailRef(domain.TorrentRef) error
	TorrentDetailConfig() (TorrentDetailConfig, error)
	OpenTorrentDetailSession(context.Context, Credential) (TorrentDetailSession, error)
}

type TorrentDetailSession interface {
	ReadTorrentDetail(context.Context, domain.TorrentRef, TorrentDetailLimits) (domain.TorrentDetail, TorrentDetailReceipt, error)
	RequestsMade() int
	Close() error
}
