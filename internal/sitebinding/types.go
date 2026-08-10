// Package sitebinding persists and verifies exact site-fetch-to-metafile
// observations. A binding is historical provenance for one explicit remote
// reference and one exact whole-metafile variant; it is never inferred from an
// info hash and this package never selects a latest record.
package sitebinding

import (
	"errors"
	"fmt"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const (
	SchemaV1 = "ptctl.site-metafile-binding/v1"

	defaultMaxRecordBytes = int64(64 << 10)
	hardMaxRecordBytes    = int64(256 << 10)

	sealEffect = "write_private_site_metafile_binding"
	loadEffect = "read_verified_site_metafile_binding"
)

var (
	ErrInvalidBinding  = errors.New("site metafile binding is invalid")
	ErrCorruptBinding  = errors.New("site metafile binding is corrupt")
	ErrBindingMismatch = errors.New("site metafile binding does not match the requested identity")
)

// Limits bounds the complete canonical binding record. Records are deliberately
// small even though the underlying generic sealed-record store supports larger
// payloads.
type Limits struct {
	MaxRecordBytes int64 `json:"max_record_bytes"`
}

func DefaultLimits() Limits { return Limits{MaxRecordBytes: defaultMaxRecordBytes} }

func (limits Limits) Validate() error {
	if limits.MaxRecordBytes <= 0 || limits.MaxRecordBytes > hardMaxRecordBytes {
		return fmt.Errorf("site metafile binding byte limit is outside the supported range")
	}
	return nil
}

// Record is the complete canonical v1 value stored as a sealed record. It is a
// public provenance DTO: response bytes, credentials, paths, announce URLs and
// tracker material are never present.
type Record struct {
	Schema            string                   `json:"schema"`
	SiteID            string                   `json:"site_id"`
	RemoteID          string                   `json:"remote_id"`
	Origin            string                   `json:"origin"`
	RouteID           string                   `json:"route_id"`
	MetafileVariantID string                   `json:"metafile_variant_id"`
	ArtifactID        metastore.ArtifactID     `json:"artifact_id"`
	ArtifactSizeBytes int64                    `json:"artifact_size_bytes"`
	Basis             string                   `json:"basis"`
	ObservedAtStart   time.Time                `json:"observed_at_start"`
	ObservedAtEnd     time.Time                `json:"observed_at_end"`
	ResponseBytes     int64                    `json:"response_bytes"`
	FetchEffect       string                   `json:"fetch_effect"`
	FetchComplete     bool                     `json:"fetch_complete"`
	FetchStopReason   string                   `json:"fetch_stop_reason"`
	FetchLimits       site.MetafileFetchLimits `json:"fetch_limits"`
	FetchUsage        site.MetafileFetchUsage  `json:"fetch_usage"`
}

func (record Record) Validate() error {
	if record.Schema != SchemaV1 {
		return fmt.Errorf("%w: schema is unsupported", ErrInvalidBinding)
	}
	observation := record.observation()
	if err := site.ValidateMetafileObservation(observation); err != nil {
		return fmt.Errorf("%w: observation is invalid", ErrInvalidBinding)
	}
	if record.ObservedAtStart.Location() != time.UTC || record.ObservedAtEnd.Location() != time.UTC {
		return fmt.Errorf("%w: observation time is not canonical UTC", ErrInvalidBinding)
	}
	artifactID, err := metastore.ParseArtifactID(record.ArtifactID.String())
	if err != nil || artifactID != record.ArtifactID || artifactID.String() != record.MetafileVariantID ||
		record.ArtifactSizeBytes != record.ResponseBytes {
		return fmt.Errorf("%w: artifact link is invalid", ErrInvalidBinding)
	}
	if err := record.FetchLimits.Validate(); err != nil {
		return fmt.Errorf("%w: fetch limits are invalid", ErrInvalidBinding)
	}
	if record.FetchEffect != site.MetafileFetchEffect || !record.FetchComplete || record.FetchStopReason != "" ||
		record.FetchUsage.RequestsAttempted != 1 || record.FetchUsage.AutomaticRetries != 0 ||
		record.FetchUsage.RedirectsFollowed != 0 || !record.FetchUsage.ResponseBytesKnown ||
		record.FetchUsage.ResponseBytesRead != record.ResponseBytes ||
		record.ResponseBytes > record.FetchLimits.MaxResponseBytes {
		return fmt.Errorf("%w: fetch account is invalid", ErrInvalidBinding)
	}
	return nil
}

func (record Record) observation() domain.SiteMetafileObservation {
	return domain.SiteMetafileObservation{
		Ref:    domain.TorrentRef{SiteID: record.SiteID, RemoteID: record.RemoteID},
		Origin: record.Origin, RouteID: record.RouteID,
		MetafileVariantID: record.MetafileVariantID, Basis: record.Basis,
		ObservedAtStart: record.ObservedAtStart, ObservedAtEnd: record.ObservedAtEnd,
		ResponseBytes: record.ResponseBytes,
	}
}

func (record Record) artifactLink() metastore.ArtifactLink {
	return metastore.ArtifactLink{
		ID: record.ArtifactID, SizeBytes: record.ArtifactSizeBytes, RequirePrivate: true,
	}
}

type SealReceipt struct {
	Effect          string                 `json:"effect"`
	WritesPerformed int                    `json:"writes_performed"`
	AlreadyPresent  bool                   `json:"already_present"`
	BytesConsumed   int64                  `json:"bytes_consumed"`
	Record          metastore.RecordRef    `json:"record"`
	Artifact        metastore.ArtifactLink `json:"artifact"`
	Store           metastore.StoreInfo    `json:"store"`
}

type LoadReceipt struct {
	Effect            string                `json:"effect"`
	Complete          bool                  `json:"complete"`
	Selected          bool                  `json:"selected"`
	RecordBytesRead   int64                 `json:"record_bytes_read"`
	ConsumerBytesRead int64                 `json:"consumer_bytes_read"`
	Record            metastore.RecordRef   `json:"record"`
	Artifact          metastore.ArtifactRef `json:"artifact"`
	Store             metastore.StoreInfo   `json:"store"`
}

// PublicBinding is the serializable, authority-free representation of a
// verified binding. It is useful for reports but cannot be used as proof.
type PublicBinding struct {
	Record    Record                `json:"binding"`
	RecordRef metastore.RecordRef   `json:"record"`
	Artifact  metastore.ArtifactRef `json:"artifact"`
	Store     metastore.StoreInfo   `json:"store"`
}

type verifiedAuthority struct {
	storeID    string
	recordID   metastore.RecordID
	artifactID metastore.ArtifactID
	ref        domain.TorrentRef
	variantID  string
}

// VerifiedSiteBinding is process-local authority created only after a single
// bound metastore session verifies both the explicit record and its exact
// private artifact. JSON contains only PublicBinding and cannot retain this
// authority.
type VerifiedSiteBinding struct {
	public    PublicBinding
	authority *verifiedAuthority
}

func (binding *VerifiedSiteBinding) PublicCopy() PublicBinding {
	if binding == nil {
		return PublicBinding{}
	}
	return binding.public
}

func (binding *VerifiedSiteBinding) Verified() bool {
	if binding == nil || binding.authority == nil {
		return false
	}
	authority := binding.authority
	public := binding.public
	return public.Store.StoreID == authority.storeID && public.RecordRef.ID == authority.recordID &&
		public.Artifact.ID == authority.artifactID && public.Record.observation().Ref == authority.ref &&
		public.Record.MetafileVariantID == authority.variantID && public.Record.Validate() == nil
}

// Matches requires the explicit persistent RecordID in addition to the
// expected remote ref and exact whole-metafile variant.
func (binding *VerifiedSiteBinding) Matches(recordID metastore.RecordID, ref domain.TorrentRef, variantID string) bool {
	return binding.Verified() && binding.authority.recordID == recordID &&
		binding.authority.ref == ref && binding.authority.variantID == variantID
}

func (binding VerifiedSiteBinding) String() string { return "[VERIFIED_SITE_METAFILE_BINDING]" }
func (binding VerifiedSiteBinding) GoString() string {
	return "sitebinding.VerifiedSiteBinding{[REDACTED_AUTHORITY]}"
}
