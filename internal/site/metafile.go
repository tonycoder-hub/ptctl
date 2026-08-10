package site

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/domain"
)

const (
	MetafileFetchEffect                = "site_metafile_fetch"
	MetafileObservationExactFetchBasis = "effectful_fetch_exact_response"

	defaultMetafileResponseBytes = int64(8 << 20)
	hardMetafileResponseBytes    = int64(32 << 20)
	hardMetafileHeaderBytes      = int64(64 << 10)
)

// MetafileFetchLimits bounds the sole remote effect and all response material
// retained by a fetch session. MaxRequests is intentionally fixed at one.
type MetafileFetchLimits struct {
	MaxRequests            int   `json:"max_requests"`
	MaxResponseBytes       int64 `json:"max_response_bytes"`
	MaxResponseHeaderBytes int64 `json:"max_response_header_bytes"`
}

func DefaultMetafileFetchLimits() MetafileFetchLimits {
	return MetafileFetchLimits{
		MaxRequests:            1,
		MaxResponseBytes:       defaultMetafileResponseBytes,
		MaxResponseHeaderBytes: hardMetafileHeaderBytes,
	}
}

func (limits MetafileFetchLimits) Validate() error {
	if limits.MaxRequests != 1 {
		return fmt.Errorf("metafile fetch request budget must be exactly one")
	}
	if limits.MaxResponseBytes <= 0 || limits.MaxResponseBytes > hardMetafileResponseBytes {
		return fmt.Errorf("metafile response byte budget is outside the supported range")
	}
	if limits.MaxResponseHeaderBytes <= 0 || limits.MaxResponseHeaderBytes > hardMetafileHeaderBytes {
		return fmt.Errorf("metafile response header budget is outside the supported range")
	}
	return nil
}

type MetafileFetchUsage struct {
	RequestsAttempted  int   `json:"requests_attempted"`
	AutomaticRetries   int   `json:"automatic_retries"`
	RedirectsFollowed  int   `json:"redirects_followed"`
	ResponseBytesRead  int64 `json:"response_bytes_read"`
	ResponseBytesKnown bool  `json:"response_bytes_known"`
}

// MetafileFetchReceipt is safe to serialize. It contains no response bytes,
// cookie, redirect target, or server-provided diagnostic text.
type MetafileFetchReceipt struct {
	Effect          string              `json:"effect"`
	Ref             domain.TorrentRef   `json:"ref"`
	Origin          string              `json:"origin"`
	RouteID         string              `json:"route_id"`
	ObservedAtStart time.Time           `json:"observed_at_start"`
	ObservedAtEnd   time.Time           `json:"observed_at_end"`
	Complete        bool                `json:"complete"`
	Limits          MetafileFetchLimits `json:"limits"`
	Used            MetafileFetchUsage  `json:"used"`
	StopReason      string              `json:"stop_reason,omitempty"`
}

// MetafileFetchConfig is credential-free adapter provenance. Callers obtain
// and pin it before reading a credential, then compare every effect receipt to
// the same canonical origin and stable route identifier.
type MetafileFetchConfig struct {
	Origin  string `json:"origin"`
	RouteID string `json:"route_id"`
}

func NewMetafileFetchConfig(origin, routeID string) (MetafileFetchConfig, error) {
	config := MetafileFetchConfig{Origin: origin, RouteID: routeID}
	if err := config.Validate(); err != nil {
		return MetafileFetchConfig{}, err
	}
	return config, nil
}

func (config MetafileFetchConfig) Validate() error {
	if err := validateCanonicalOrigin(config.Origin); err != nil {
		return err
	}
	return validateRouteID(config.RouteID)
}

// MetafileRefValidator lets callers reject an adapter-specific remote
// reference before obtaining or reading a credential.
type MetafileRefValidator interface {
	ValidateMetafileRef(domain.TorrentRef) error
}

type MetafileFetchConfigurer interface {
	MetafileFetchConfig() (MetafileFetchConfig, error)
}

type MetafileFetcher interface {
	MetafileRefValidator
	MetafileFetchConfigurer
	OpenMetafileFetchSession(context.Context, Credential) (MetafileFetchSession, error)
}

type MetafileFetchSession interface {
	FetchMetafile(context.Context, domain.TorrentRef, MetafileFetchLimits) (*FetchedMetafile, MetafileFetchReceipt, error)
	RequestsMade() int
	Close() error
}

// FetchedMetafile deliberately has no exported content field. Adapters create
// it from the exact response and consumers can only obtain a fresh reader or
// bind a completed import to that response digest.
type FetchedMetafile struct {
	ref       domain.TorrentRef
	origin    string
	routeID   string
	start     time.Time
	end       time.Time
	raw       []byte
	digest    [sha256.Size]byte
	authority *fetchAuthority
}

type fetchAuthority struct{}

// NewFetchedMetafile is the safe construction boundary for site adapters,
// including adapters outside this package. Origin and routeID must be stable,
// adapter-controlled identifiers rather than values copied from a user ref.
// The input is cloned to prevent later mutation by an adapter. Logical body
// copies during construction are therefore bounded at 16 MiB by default and
// 64 MiB at the hard ceiling, excluding allocator and transport overhead.
func NewFetchedMetafile(ref domain.TorrentRef, origin, routeID string, start, end time.Time, raw []byte) (*FetchedMetafile, error) {
	if err := validatePublicRef(ref); err != nil {
		return nil, err
	}
	if err := (MetafileFetchConfig{Origin: origin, RouteID: routeID}).Validate(); err != nil {
		return nil, err
	}
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return nil, fmt.Errorf("metafile observation interval is invalid")
	}
	if len(raw) == 0 || int64(len(raw)) > hardMetafileResponseBytes {
		return nil, fmt.Errorf("metafile response size is outside the supported range")
	}
	copyOfRaw := bytes.Clone(raw)
	return &FetchedMetafile{
		ref:       ref,
		origin:    origin,
		routeID:   routeID,
		start:     start.UTC(),
		end:       end.UTC(),
		raw:       copyOfRaw,
		digest:    sha256.Sum256(copyOfRaw),
		authority: &fetchAuthority{},
	}, nil
}

func (f *FetchedMetafile) Reader() (io.Reader, error) {
	if f == nil || f.authority == nil || len(f.raw) == 0 {
		return nil, fmt.Errorf("fetched metafile is unavailable")
	}
	// Do not return bytes.Reader: its WriterTo method can expose the backing
	// slice to an untrusted io.Writer. This narrow reader only copies into p.
	return &fetchedMetafileReader{raw: f.raw}, nil
}

type fetchedMetafileReader struct {
	raw    []byte
	offset int
}

func (reader *fetchedMetafileReader) Read(p []byte) (int, error) {
	if reader.offset >= len(reader.raw) {
		return 0, io.EOF
	}
	read := copy(p, reader.raw[reader.offset:])
	reader.offset += read
	return read, nil
}

func (*fetchedMetafileReader) String() string   { return "[REDACTED_PRIVATE_METAFILE_READER]" }
func (*fetchedMetafileReader) GoString() string { return "site.fetchedMetafileReader{[REDACTED]}" }
func (*fetchedMetafileReader) MarshalJSON() ([]byte, error) {
	return json.Marshal("[REDACTED_PRIVATE_METAFILE_READER]")
}

func (f *FetchedMetafile) SizeBytes() int64 {
	if f == nil || f.authority == nil {
		return 0
	}
	return int64(len(f.raw))
}

// MatchesReceipt validates the adapter's public effect account against the
// private response authority before a caller performs any store write.
func (f *FetchedMetafile) MatchesReceipt(receipt MetafileFetchReceipt) bool {
	if f == nil || f.authority == nil || receipt.Limits.Validate() != nil {
		return false
	}
	return receipt.Effect == MetafileFetchEffect && receipt.Ref == f.ref &&
		receipt.Origin == f.origin && receipt.RouteID == f.routeID &&
		receipt.ObservedAtStart.Equal(f.start) && receipt.ObservedAtEnd.Equal(f.end) &&
		receipt.Complete && receipt.StopReason == "" &&
		receipt.Used.RequestsAttempted == 1 && receipt.Used.AutomaticRetries == 0 &&
		receipt.Used.RedirectsFollowed == 0 && receipt.Used.ResponseBytesKnown &&
		receipt.Used.ResponseBytesRead == int64(len(f.raw)) &&
		int64(len(f.raw)) <= receipt.Limits.MaxResponseBytes
}

// BindImported proves only an exact whole-response import. An info hash, a
// declared site reference, or a partial read can never create this binding.
func (f *FetchedMetafile) BindImported(variantID string, bytesConsumed int64) (*ObservedMetafileBinding, error) {
	if f == nil || f.authority == nil || len(f.raw) == 0 {
		return nil, fmt.Errorf("fetched metafile is unavailable")
	}
	if bytesConsumed != int64(len(f.raw)) {
		return nil, fmt.Errorf("metafile import did not consume the exact response")
	}
	digest, err := parseVariantID(variantID)
	if err != nil || subtle.ConstantTimeCompare(digest, f.digest[:]) != 1 {
		return nil, fmt.Errorf("imported metafile variant does not match the fetched response")
	}
	return &ObservedMetafileBinding{
		observation: domain.SiteMetafileObservation{
			Ref:               f.ref,
			Origin:            f.origin,
			RouteID:           f.routeID,
			MetafileVariantID: variantID,
			Basis:             MetafileObservationExactFetchBasis,
			ObservedAtStart:   f.start,
			ObservedAtEnd:     f.end,
			ResponseBytes:     int64(len(f.raw)),
		},
		authority: f.authority,
	}, nil
}

func (f FetchedMetafile) String() string   { return "[REDACTED_PRIVATE_METAFILE]" }
func (f FetchedMetafile) GoString() string { return "site.FetchedMetafile{[REDACTED]}" }
func (f FetchedMetafile) MarshalJSON() ([]byte, error) {
	return json.Marshal("[REDACTED_PRIVATE_METAFILE]")
}

// ObservedMetafileBinding retains process-local authority separately from its
// serializable DTO. A deserialized public observation cannot regain authority.
type ObservedMetafileBinding struct {
	observation domain.SiteMetafileObservation
	authority   *fetchAuthority
}

func (binding *ObservedMetafileBinding) PublicCopy() domain.SiteMetafileObservation {
	if binding == nil || binding.authority == nil {
		return domain.SiteMetafileObservation{}
	}
	return binding.observation
}

func (binding *ObservedMetafileBinding) Matches(ref domain.TorrentRef, origin, routeID, variantID string) bool {
	return binding != nil && binding.authority != nil &&
		binding.observation.Ref == ref && binding.observation.Origin == origin &&
		binding.observation.RouteID == routeID && binding.observation.MetafileVariantID == variantID
}

func (binding ObservedMetafileBinding) String() string { return "[REDACTED_METAFILE_BINDING]" }
func (binding ObservedMetafileBinding) GoString() string {
	return "site.ObservedMetafileBinding{[REDACTED]}"
}
func (binding ObservedMetafileBinding) MarshalJSON() ([]byte, error) {
	return json.Marshal("[REDACTED_METAFILE_BINDING]")
}

func validatePublicRef(ref domain.TorrentRef) error {
	if !validPublicText(ref.SiteID, 64) || !validPublicText(ref.RemoteID, 256) {
		return fmt.Errorf("metafile reference is invalid")
	}
	return nil
}

func validPublicText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, ch := range value {
		if ch < 0x20 || ch == 0x7f {
			return false
		}
	}
	return true
}

func validateCanonicalOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Host != strings.ToLower(parsed.Host) || strings.HasSuffix(parsed.Hostname(), ".") || parsed.Port() == "443" ||
		origin != "https://"+parsed.Host {
		return fmt.Errorf("metafile fetch origin is not a canonical HTTPS origin")
	}
	return nil
}

func validateRouteID(routeID string) error {
	if routeID == "" || len(routeID) > 128 {
		return fmt.Errorf("metafile route identifier is invalid")
	}
	for i := 0; i < len(routeID); i++ {
		ch := routeID[i]
		if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '.' || ch == '_' || ch == '-') {
			return fmt.Errorf("metafile route identifier is invalid")
		}
	}
	return nil
}

func parseVariantID(value string) ([]byte, error) {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return nil, fmt.Errorf("invalid metafile variant identifier")
	}
	digest, err := hex.DecodeString(value[len("sha256:"):])
	if err != nil || "sha256:"+hex.EncodeToString(digest) != value {
		return nil, fmt.Errorf("invalid metafile variant identifier")
	}
	return digest, nil
}
