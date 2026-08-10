package site

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

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
	ReadTorrentDetail(context.Context, domain.TorrentRef, TorrentDetailLimits) (*ObservedTorrentDetail, TorrentDetailReceipt, error)
	RequestsMade() int
	Close() error
}

// ObservedTorrentDetail is same-invocation authority for one bounded detail
// response. PublicCopy is safe to serialize, but serialization never preserves
// the authority required by MatchesReceipt.
type ObservedTorrentDetail struct {
	detail    domain.TorrentDetail
	receipt   TorrentDetailReceipt
	authority *observedTorrentDetailAuthority
}

type observedTorrentDetailAuthority struct{}

func NewObservedTorrentDetail(detail domain.TorrentDetail, receipt TorrentDetailReceipt) (*ObservedTorrentDetail, error) {
	if err := validateObservedTorrentDetail(detail, receipt); err != nil {
		return nil, err
	}
	return &ObservedTorrentDetail{
		detail:    copyTorrentDetail(detail),
		receipt:   receipt,
		authority: &observedTorrentDetailAuthority{},
	}, nil
}

func (observed *ObservedTorrentDetail) PublicCopy() domain.TorrentDetail {
	if observed == nil {
		return domain.TorrentDetail{EvidenceBasis: []string{}}
	}
	return copyTorrentDetail(observed.detail)
}

func (observed *ObservedTorrentDetail) Verified() bool {
	return observed != nil && observed.authority != nil && validateObservedTorrentDetail(observed.detail, observed.receipt) == nil
}

func (observed *ObservedTorrentDetail) MatchesReceipt(receipt TorrentDetailReceipt) bool {
	return observed != nil && observed.Verified() && observed.receipt == receipt
}

func (observed *ObservedTorrentDetail) Matches(ref domain.TorrentRef, origin, routeID string) bool {
	return observed != nil && observed.Verified() && observed.detail.Ref == ref && observed.receipt.Ref == ref && observed.receipt.Origin == origin && observed.receipt.RouteID == routeID
}

func (observed *ObservedTorrentDetail) String() string { return "site.ObservedTorrentDetail{[OPAQUE]}" }
func (observed *ObservedTorrentDetail) GoString() string {
	return "site.ObservedTorrentDetail{[OPAQUE]}"
}

func (observed *ObservedTorrentDetail) MarshalJSON() ([]byte, error) {
	return json.Marshal(observed.PublicCopy())
}

func (observed *ObservedTorrentDetail) UnmarshalJSON(raw []byte) error {
	if observed == nil {
		return fmt.Errorf("observed torrent detail is nil")
	}
	var public domain.TorrentDetail
	if err := json.Unmarshal(raw, &public); err != nil {
		return err
	}
	observed.detail = copyTorrentDetail(public)
	observed.receipt = TorrentDetailReceipt{}
	observed.authority = nil
	return nil
}

func validateObservedTorrentDetail(detail domain.TorrentDetail, receipt TorrentDetailReceipt) error {
	_, configErr := NewTorrentDetailConfig(receipt.Origin, receipt.RouteID)
	if receipt.Effect != TorrentDetailReadEffect || receipt.Ref != detail.Ref || !receipt.Complete || receipt.StopReason != "" ||
		receipt.Limits.Validate() != nil || receipt.ObservedAtStart.IsZero() || receipt.ObservedAtEnd.IsZero() ||
		receipt.ObservedAtStart.Location() != time.UTC || receipt.ObservedAtEnd.Location() != time.UTC || receipt.ObservedAtEnd.Before(receipt.ObservedAtStart) ||
		receipt.Used.RequestsAttempted != 1 || receipt.Used.AutomaticRetries != 0 || receipt.Used.RedirectsFollowed != 0 ||
		!receipt.Used.ResponseBytesKnown || receipt.Used.ResponseBytesRead <= 0 || receipt.Used.ResponseBytesRead > receipt.Limits.MaxResponseBytes ||
		configErr != nil || !validObservedDetailTitle(detail.DisplayTitle) ||
		detail.Seeders != nil && *detail.Seeders < 0 || detail.Leechers != nil && *detail.Leechers < 0 {
		return fmt.Errorf("torrent detail observation is invalid")
	}
	wantBasis := []string{DetailBasisAuthenticated, DetailBasisExactRef, DetailBasisOneRequest, DetailBasisSiteClaimOnly}
	if detail.DownloadReferenceObserved {
		wantBasis = append(wantBasis, DetailBasisDownloadRef)
	}
	if len(detail.EvidenceBasis) != len(wantBasis) {
		return fmt.Errorf("torrent detail evidence is invalid")
	}
	for index := range wantBasis {
		if detail.EvidenceBasis[index] != wantBasis[index] {
			return fmt.Errorf("torrent detail evidence is invalid")
		}
	}
	return nil
}

func validObservedDetailTitle(value string) bool {
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

func copyTorrentDetail(value domain.TorrentDetail) domain.TorrentDetail {
	result := value
	result.EvidenceBasis = append([]string(nil), value.EvidenceBasis...)
	if result.EvidenceBasis == nil {
		result.EvidenceBasis = []string{}
	}
	if value.Seeders != nil {
		seeders := *value.Seeders
		result.Seeders = &seeders
	}
	if value.Leechers != nil {
		leechers := *value.Leechers
		result.Leechers = &leechers
	}
	return result
}
