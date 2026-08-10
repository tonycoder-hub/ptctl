package sitebinding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

type Repository struct {
	store  *metastore.Store
	limits Limits
}

func NewRepository(store *metastore.Store, limits Limits) (*Repository, error) {
	if store == nil || store.Info().StoreID == "" {
		return nil, fmt.Errorf("site metafile binding store is unavailable")
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &Repository{store: store, limits: limits}, nil
}

// Seal accepts only the process-local exact-response authority returned by
// site.FetchedMetafile.BindImported. The complete fetch receipt is checked
// before any store write, then the binding and linked private artifact are
// verified in one operation-bound metastore session.
func (repository *Repository) Seal(ctx context.Context, observed *site.ObservedMetafileBinding, fetch site.MetafileFetchReceipt, artifact metastore.ArtifactRef) (metastore.RecordRef, SealReceipt, error) {
	receipt := SealReceipt{Effect: sealEffect}
	if repository == nil || repository.store == nil {
		return metastore.RecordRef{}, receipt, fmt.Errorf("seal site metafile binding: repository is unavailable")
	}
	receipt.Store = repository.store.Info()
	if err := ctx.Err(); err != nil {
		return metastore.RecordRef{}, receipt, err
	}
	if observed == nil || !observed.MatchesReceipt(fetch) {
		return metastore.RecordRef{}, receipt, fmt.Errorf("%w: fetch authority is unavailable", ErrInvalidBinding)
	}
	observation := observed.PublicCopy()
	if err := site.ValidateMetafileObservation(observation); err != nil ||
		!observed.Matches(observation.Ref, observation.Origin, observation.RouteID, observation.MetafileVariantID) {
		return metastore.RecordRef{}, receipt, fmt.Errorf("%w: observation authority is invalid", ErrInvalidBinding)
	}
	parsedArtifactID, artifactErr := metastore.ParseArtifactID(artifact.ID.String())
	if artifactErr != nil || parsedArtifactID != artifact.ID || artifact.ID.String() != observation.MetafileVariantID ||
		artifact.MetafileVariantID != observation.MetafileVariantID || artifact.SizeBytes != observation.ResponseBytes {
		return metastore.RecordRef{}, receipt, fmt.Errorf("%w: imported artifact identity is invalid", ErrInvalidBinding)
	}

	record := recordFromObservation(observation, fetch, artifact)
	raw, err := EncodeRecord(record, repository.limits)
	if err != nil {
		return metastore.RecordRef{}, receipt, err
	}
	link := record.artifactLink()
	recordRef, imported, err := repository.store.ImportRecordLinkedArtifact(
		ctx, metastore.RecordKindSiteMetafileBindingV1, bytes.NewReader(raw),
		repository.recordLimits(), link, metastore.DefaultLimits(),
	)
	receipt.WritesPerformed = imported.WritesPerformed
	receipt.AlreadyPresent = imported.AlreadyPresent
	receipt.BytesConsumed = imported.BytesConsumed
	receipt.Record = recordRef
	if err != nil {
		if imported.WritesPerformed > 0 {
			receipt.Artifact = link
		}
		return recordRef, receipt, err
	}
	receipt.Artifact = link
	if recordRef.Kind != metastore.RecordKindSiteMetafileBindingV1 || recordRef.SizeBytes != int64(len(raw)) {
		return recordRef, receipt, fmt.Errorf("seal site metafile binding: committed record identity disagrees")
	}
	return recordRef, receipt, nil
}

// Load verifies one explicit RecordID and its exact private artifact under one
// metastore session. It never enumerates records or infers a current value.
func (repository *Repository) Load(ctx context.Context, id metastore.RecordID) (*VerifiedSiteBinding, LoadReceipt, error) {
	receipt := LoadReceipt{Effect: loadEffect}
	if repository == nil || repository.store == nil {
		return nil, receipt, fmt.Errorf("load site metafile binding: repository is unavailable")
	}
	receipt.Store = repository.store.Info()
	parsedID, err := metastore.ParseRecordID(id.String())
	if err != nil || parsedID != id {
		return nil, receipt, fmt.Errorf("load site metafile binding: record identity is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, receipt, err
	}

	var record Record
	var recordDecodeErr error
	recordRef, loaded, meta, artifact, err := repository.store.LoadRecordLinkedArtifact(
		ctx, metastore.RecordKindSiteMetafileBindingV1, id, repository.recordLimits(), metastore.DefaultLimits(),
		func(reader io.Reader) (metastore.ArtifactLink, error) {
			decoded, decodeErr := DecodeRecord(reader, repository.limits)
			if decodeErr != nil {
				recordDecodeErr = decodeErr
				return metastore.ArtifactLink{}, decodeErr
			}
			record = decoded
			return decoded.artifactLink(), nil
		},
	)
	receipt.RecordBytesRead = loaded.RecordBytesRead
	receipt.ConsumerBytesRead = loaded.ConsumerBytesRead
	receipt.Record = recordRef
	receipt.Artifact = artifact
	if err != nil {
		if recordDecodeErr != nil {
			return nil, receipt, fmt.Errorf("%w: canonical record validation failed", ErrCorruptBinding)
		}
		if errors.Is(err, metastore.ErrCorruptRecord) || errors.Is(err, metastore.ErrRecordConsumerIncomplete) {
			return nil, receipt, fmt.Errorf("%w: sealed record verification failed", ErrCorruptBinding)
		}
		return nil, receipt, err
	}
	if meta == nil || !meta.Private || record.Validate() != nil || recordRef.ID != id ||
		artifact.ID != record.ArtifactID || artifact.SizeBytes != record.ArtifactSizeBytes ||
		artifact.MetafileVariantID != record.MetafileVariantID {
		return nil, receipt, fmt.Errorf("%w: linked identities disagree", ErrCorruptBinding)
	}
	public := PublicBinding{Record: record, RecordRef: recordRef, Artifact: artifact, Store: repository.store.Info()}
	verified := &VerifiedSiteBinding{public: public, authority: &verifiedAuthority{
		storeID: public.Store.StoreID, recordID: recordRef.ID, artifactID: artifact.ID,
		ref: record.observation().Ref, variantID: record.MetafileVariantID,
	}}
	if !verified.Verified() {
		return nil, receipt, fmt.Errorf("%w: verification authority is invalid", ErrCorruptBinding)
	}
	receipt.Complete = true
	return verified, receipt, nil
}

// Select adds an exact caller expectation to explicit-ID Load. It never falls
// back to another binding record.
func (repository *Repository) Select(ctx context.Context, id metastore.RecordID, expectedRef domain.TorrentRef, expectedVariantID string) (*VerifiedSiteBinding, LoadReceipt, error) {
	verified, receipt, err := repository.Load(ctx, id)
	if err != nil {
		return nil, receipt, err
	}
	if !verified.Matches(id, expectedRef, expectedVariantID) {
		return nil, receipt, ErrBindingMismatch
	}
	receipt.Selected = true
	return verified, receipt, nil
}

func (repository *Repository) recordLimits() metastore.RecordLimits {
	limits := metastore.DefaultRecordLimits()
	limits.MaxRecordBytes = repository.limits.MaxRecordBytes
	return limits
}

func recordFromObservation(observation domain.SiteMetafileObservation, fetch site.MetafileFetchReceipt, artifact metastore.ArtifactRef) Record {
	return Record{
		Schema: SchemaV1, SiteID: observation.Ref.SiteID, RemoteID: observation.Ref.RemoteID,
		Origin: observation.Origin, RouteID: observation.RouteID,
		MetafileVariantID: observation.MetafileVariantID,
		ArtifactID:        artifact.ID, ArtifactSizeBytes: artifact.SizeBytes,
		Basis: observation.Basis, ObservedAtStart: observation.ObservedAtStart,
		ObservedAtEnd: observation.ObservedAtEnd, ResponseBytes: observation.ResponseBytes,
		FetchEffect: fetch.Effect, FetchComplete: fetch.Complete, FetchStopReason: fetch.StopReason,
		FetchLimits: fetch.Limits, FetchUsage: fetch.Used,
	}
}

func (binding VerifiedSiteBinding) MarshalJSON() ([]byte, error) {
	return json.Marshal(binding.public)
}

// UnmarshalJSON retains the public report but deliberately cannot recreate
// process-local verification authority.
func (binding *VerifiedSiteBinding) UnmarshalJSON(raw []byte) error {
	if binding == nil {
		return fmt.Errorf("site metafile binding destination is unavailable")
	}
	if len(raw) == 0 || int64(len(raw)) > hardMaxRecordBytes*2 {
		return fmt.Errorf("site metafile binding public report is invalid")
	}
	var public PublicBinding
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&public); err != nil {
		return fmt.Errorf("site metafile binding public report is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("site metafile binding public report is invalid")
	}
	binding.public = public
	binding.authority = nil
	return nil
}
