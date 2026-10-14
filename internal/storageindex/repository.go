package storageindex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
)

var (
	ErrProfileNotFound             = errors.New("storage profile was not found")
	ErrProfileConflict             = errors.New("storage profile name has a different immutable declaration")
	ErrProfileSelectionIncomplete  = errors.New("storage profile selection is incomplete")
	ErrProfilePlatformMismatch     = errors.New("storage profile belongs to a different operating-system namespace")
	ErrProfileLivePathInvalid      = errors.New("storage profile path is invalid for the current operating-system namespace")
	ErrProfileExceedsIndexLimits   = errors.New("storage profile scan policy exceeds repository index limits")
	ErrSnapshotNotFound            = errors.New("storage index snapshot was not found")
	ErrSnapshotAmbiguous           = errors.New("storage index snapshot selection is ambiguous")
	ErrSnapshotSelectionIncomplete = errors.New("storage index snapshot selection is incomplete")
	// ErrSnapshotGenerationConflict reports that another refresh of the same
	// profile published the same or a newer generation while this refresh was
	// running. Nothing is removed; retrying publishes a newer unique
	// generation, which also supersedes any resulting tie.
	ErrSnapshotGenerationConflict = errors.New("storage index generation conflicts with a concurrent refresh")
	// ErrIntegrity identifies structurally invalid or non-canonical storage
	// index records and descriptor/data binding failures. Callers may classify
	// it separately from ordinary store and filesystem I/O failures.
	ErrIntegrity = errors.New("storage index failed integrity validation")
)

const (
	profileImportEffect  = "write_private_storage_profile"
	profileSelectEffect  = "read_private_storage_profile"
	snapshotSelectEffect = "read_private_storage_index_descriptor"
)

// Repository stores immutable profiles and index snapshots in an already
// initialized private metastore. It never treats a record filename or a
// historical snapshot as current filesystem authority.
type Repository struct {
	store  *metastore.Store
	limits Limits
	// maintenanceRecords overrides the descriptor/data listing budget used by
	// refresh generation checks and retention; zero selects the default.
	maintenanceRecords int
}

func NewRepository(store *metastore.Store, limits Limits) (*Repository, error) {
	if store == nil {
		return nil, fmt.Errorf("private state store is unavailable")
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &Repository{store: store, limits: limits}, nil
}

type ProfileReceipt struct {
	Effect          string              `json:"effect"`
	WritesPerformed int                 `json:"writes_performed"`
	AlreadyPresent  bool                `json:"already_present"`
	Profile         Profile             `json:"profile"`
	RecordID        metastore.RecordID  `json:"record_id,omitempty"`
	Store           metastore.StoreInfo `json:"store"`
}

type ProfileSelection struct {
	Effect            string              `json:"effect"`
	Status            string              `json:"status"`
	Complete          bool                `json:"complete"`
	RecordsConsidered int                 `json:"records_considered"`
	ProfilesMatched   int                 `json:"profiles_matched"`
	Profile           Profile             `json:"profile"`
	RecordID          metastore.RecordID  `json:"record_id,omitempty"`
	Store             metastore.StoreInfo `json:"store"`
	StopReason        string              `json:"stop_reason,omitempty"`
}

type SnapshotSelection struct {
	Effect             string              `json:"effect"`
	Status             string              `json:"status"`
	Complete           bool                `json:"complete"`
	RecordsConsidered  int                 `json:"records_considered"`
	DescriptorsMatched int                 `json:"descriptors_matched"`
	Descriptor         SnapshotDescriptor  `json:"descriptor"`
	DescriptorRecordID metastore.RecordID  `json:"descriptor_record_id,omitempty"`
	Store              metastore.StoreInfo `json:"store"`
	StopReason         string              `json:"stop_reason,omitempty"`
	// ForeignInvalidDescriptors counts sealed descriptors that do not decode
	// but name a different profile; they cannot affect this selection.
	ForeignInvalidDescriptors int `json:"foreign_invalid_descriptors,omitempty"`
}

type loadedProfile struct {
	ref     metastore.RecordRef
	profile Profile
}

type loadedDescriptor struct {
	ref        metastore.RecordRef
	descriptor SnapshotDescriptor
}

func (repository *Repository) CreateProfile(ctx context.Context, name string, roots []string, allowNetwork bool, scanLimits ScanLimits, now time.Time) (ProfileReceipt, error) {
	receipt := ProfileReceipt{Effect: profileImportEffect, Store: repository.store.Info()}
	if err := ValidateScanLimitsForIndex(scanLimits, repository.limits); err != nil {
		return receipt, err
	}
	profile, err := NewProfile(name, roots, allowNetwork, scanLimits, now)
	if err != nil {
		return receipt, err
	}
	receipt.Profile = profile
	existing, err := repository.loadProfiles(ctx)
	if err != nil {
		return receipt, err
	}
	var equivalent []loadedProfile
	for _, candidate := range existing {
		if candidate.profile.Name != profile.Name {
			continue
		}
		if candidate.profile.ID != profile.ID || candidate.profile.Revision != profile.Revision {
			return receipt, ErrProfileConflict
		}
		equivalent = append(equivalent, candidate)
	}
	if len(equivalent) > 0 {
		sort.Slice(equivalent, func(i, j int) bool { return equivalent[i].ref.ID < equivalent[j].ref.ID })
		receipt.AlreadyPresent = true
		receipt.Profile = equivalent[0].profile
		receipt.RecordID = equivalent[0].ref.ID
		return receipt, nil
	}

	raw, err := EncodeProfile(profile)
	if err != nil {
		return receipt, err
	}
	ref, imported, err := repository.store.ImportRecord(ctx, metastore.RecordKindStorageProfileV1, bytes.NewReader(raw), repository.recordLimits(repository.limits.MaxProfiles))
	receipt.WritesPerformed = imported.WritesPerformed
	receipt.RecordID = ref.ID
	if err != nil {
		return receipt, err
	}
	receipt.AlreadyPresent = imported.AlreadyPresent
	return receipt, nil
}

// SelectProfile accepts either an immutable profile ID or a display name.
// Names which resolve to multiple immutable declarations are ambiguous.
func (repository *Repository) SelectProfile(ctx context.Context, selector string) (ProfileSelection, error) {
	result := ProfileSelection{Effect: profileSelectEffect, Status: "not_found", Store: repository.store.Info()}
	if selector == "" {
		return result, fmt.Errorf("storage profile selector is empty")
	}
	byID := validateOpaqueID(selector, "profile") == nil
	if !byID {
		if err := validateProfileName(selector); err != nil {
			return result, err
		}
	}
	profiles, err := repository.loadProfiles(ctx)
	result.RecordsConsidered = len(profiles)
	if errors.Is(err, ErrProfileSelectionIncomplete) {
		result.Status = "incomplete"
		result.StopReason = "record_listing_incomplete"
	}
	if err != nil {
		return result, err
	}
	matches := make([]loadedProfile, 0)
	for _, candidate := range profiles {
		if (byID && candidate.profile.ID == selector) || (!byID && candidate.profile.Name == selector) {
			matches = append(matches, candidate)
		}
	}
	result.ProfilesMatched = len(matches)
	if len(matches) == 0 {
		result.Complete = true
		return result, ErrProfileNotFound
	}
	authorities := make(map[string]struct{})
	for _, match := range matches {
		authorities[match.profile.ID+"\x00"+match.profile.Revision] = struct{}{}
	}
	if len(authorities) != 1 {
		result.Status = "ambiguous"
		result.Complete = true
		return result, ErrProfileConflict
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].ref.ID < matches[j].ref.ID })
	result.Status = "selected"
	result.Complete = true
	result.Profile = matches[0].profile
	result.RecordID = matches[0].ref.ID
	return result, nil
}

// SelectSnapshot selects an explicit descriptor record without listing, or
// the unique highest generation for one exact immutable profile revision.
func (repository *Repository) SelectSnapshot(ctx context.Context, profile Profile, explicitDescriptor metastore.RecordID) (SnapshotSelection, error) {
	result := SnapshotSelection{Effect: snapshotSelectEffect, Status: "not_found", Store: repository.store.Info()}
	if err := profile.Validate(); err != nil {
		return result, err
	}
	if explicitDescriptor != "" {
		loaded, err := repository.loadDescriptor(ctx, metastore.RecordRef{Kind: metastore.RecordKindStorageIndexDescriptorV1, ID: explicitDescriptor})
		result.RecordsConsidered = 1
		if err != nil {
			if errors.Is(err, metastore.ErrRecordNotFound) {
				result.Complete = true
				return result, ErrSnapshotNotFound
			}
			result.Status = "incomplete"
			result.StopReason = "descriptor_invalid"
			return result, err
		}
		if !descriptorMatchesProfile(loaded.descriptor, profile) {
			result.Status = "incomplete"
			result.StopReason = "profile_revision_mismatch"
			return result, ErrSnapshotSelectionIncomplete
		}
		result.Status = "selected"
		result.Complete = true
		result.DescriptorsMatched = 1
		result.Descriptor = loaded.descriptor
		result.DescriptorRecordID = loaded.ref.ID
		return result, nil
	}

	listing, err := repository.listDescriptors(ctx, profile.ID, repository.limits.MaxSnapshots, false, nil)
	result.RecordsConsidered = len(listing.descriptors)
	result.ForeignInvalidDescriptors = listing.foreignInvalid
	if err != nil {
		result.Status = "incomplete"
		result.StopReason = "descriptor_invalid"
		return result, err
	}
	if !listing.complete {
		result.Status = "incomplete"
		result.StopReason = "record_listing_incomplete"
		return result, repository.incompleteDescriptorListingError(listing, repository.limits.MaxSnapshots)
	}
	matches := make([]loadedDescriptor, 0)
	for _, descriptor := range listing.descriptors {
		if descriptorMatchesProfile(descriptor.descriptor, profile) {
			matches = append(matches, descriptor)
		}
	}
	result.DescriptorsMatched = len(matches)
	if len(matches) == 0 {
		result.Complete = true
		return result, ErrSnapshotNotFound
	}
	maximum := matches[0].descriptor.Generation
	for _, match := range matches[1:] {
		if match.descriptor.Generation > maximum {
			maximum = match.descriptor.Generation
		}
	}
	latest := matches[:0]
	for _, match := range matches {
		if match.descriptor.Generation == maximum {
			latest = append(latest, match)
		}
	}
	if len(latest) != 1 {
		result.Status = "ambiguous"
		result.Complete = true
		return result, ErrSnapshotAmbiguous
	}
	result.Status = "selected"
	result.Complete = true
	result.Descriptor = latest[0].descriptor
	result.DescriptorRecordID = latest[0].ref.ID
	return result, nil
}

// loadProfiles loads every profile record. An incomplete listing returns an
// error wrapping both ErrProfileSelectionIncomplete and the metastore
// incomplete-listing cause, which names the exhausted budget.
func (repository *Repository) loadProfiles(ctx context.Context) ([]loadedProfile, error) {
	listed, err := repository.store.ListRecords(ctx, metastore.RecordKindStorageProfileV1, repository.recordLimits(repository.limits.MaxProfiles))
	if err != nil {
		return nil, err
	}
	if !listed.Complete {
		return nil, fmt.Errorf("%w: %w", ErrProfileSelectionIncomplete, listed.IncompleteError())
	}
	profiles := make([]loadedProfile, 0, len(listed.Records))
	for _, ref := range listed.Records {
		loaded, err := repository.loadProfile(ctx, ref)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, loaded)
	}
	return profiles, nil
}

func (repository *Repository) loadProfile(ctx context.Context, observed metastore.RecordRef) (loadedProfile, error) {
	var profile Profile
	var decodeErr error
	ref, receipt, err := repository.store.LoadRecord(ctx, metastore.RecordKindStorageProfileV1, observed.ID, repository.recordLimits(repository.limits.MaxProfiles), func(reader io.Reader) error {
		profile, decodeErr = DecodeProfile(reader)
		return decodeErr
	})
	if err != nil || !receipt.Complete {
		if err != nil {
			if decodeErr != nil && !errors.Is(decodeErr, context.Canceled) && !errors.Is(decodeErr, context.DeadlineExceeded) {
				return loadedProfile{}, fmt.Errorf("%w: storage profile record is invalid", ErrIntegrity)
			}
			return loadedProfile{}, err
		}
		return loadedProfile{}, fmt.Errorf("%w: storage profile record load is incomplete", ErrIntegrity)
	}
	if observed.SizeBytes != 0 && ref.SizeBytes != observed.SizeBytes {
		return loadedProfile{}, fmt.Errorf("%w: storage profile record size changed", ErrIntegrity)
	}
	return loadedProfile{ref: ref, profile: profile}, nil
}

// descriptorListing is one bounded listing of descriptor records, decoded on
// behalf of one profile. Descriptors of every profile are returned; callers
// filter with descriptorMatchesProfile.
type descriptorListing struct {
	descriptors []loadedDescriptor
	complete    bool
	stopReason  string
	// incompleteErr is the metastore explanation of an incomplete listing.
	incompleteErr error
	// foreignInvalid counts sealed records which failed to decode but whose
	// verified bytes name a different valid profile ID.
	foreignInvalid int
}

// listDescriptors lists descriptor records with an N+1 budget of maxRecords.
// An incomplete listing is decoded only when allowPartial is set (retention
// may act on a verified subset; selection never does). A sealed but invalid
// descriptor fails the listing unless its verified bytes attribute it to a
// different profile, so one profile's bad record no longer blocks every
// other profile. cache may carry descriptors already verified by record ID;
// sealed record IDs are content digests, so a cached entry is that content.
func (repository *Repository) listDescriptors(ctx context.Context, profileID string, maxRecords int, allowPartial bool, cache map[metastore.RecordID]loadedDescriptor) (descriptorListing, error) {
	result := descriptorListing{descriptors: []loadedDescriptor{}}
	listed, err := repository.store.ListRecords(ctx, metastore.RecordKindStorageIndexDescriptorV1, repository.recordLimits(maxRecords))
	if err != nil {
		return result, err
	}
	result.complete = listed.Complete
	result.stopReason = listed.StopReason
	result.incompleteErr = listed.IncompleteError()
	if !listed.Complete && !allowPartial {
		return result, nil
	}
	for _, ref := range listed.Records {
		if cached, ok := cache[ref.ID]; ok && cached.ref.SizeBytes == ref.SizeBytes {
			result.descriptors = append(result.descriptors, cached)
			continue
		}
		loaded, owner, err := repository.loadDescriptorAttributed(ctx, ref)
		if owner != "" && owner != profileID {
			result.foreignInvalid++
			continue
		}
		if err != nil {
			return result, err
		}
		if cache != nil {
			cache[ref.ID] = loaded
		}
		result.descriptors = append(result.descriptors, loaded)
	}
	return result, nil
}

func (repository *Repository) loadDescriptor(ctx context.Context, observed metastore.RecordRef) (loadedDescriptor, error) {
	loaded, _, err := repository.loadDescriptorAttributed(ctx, observed)
	return loaded, err
}

// loadDescriptorAttributed returns, alongside a decode failure (ErrIntegrity),
// the profile ID named by the record's verified sealed bytes when that is
// unambiguous; the owner is empty for every other outcome.
func (repository *Repository) loadDescriptorAttributed(ctx context.Context, observed metastore.RecordRef) (loadedDescriptor, string, error) {
	var descriptor SnapshotDescriptor
	var decodeErr error
	captured := &boundedCapture{limit: maxSmallRecordBytes + 1}
	ref, receipt, err := repository.store.LoadRecord(ctx, metastore.RecordKindStorageIndexDescriptorV1, observed.ID, repository.recordLimits(repository.limits.MaxSnapshots), func(reader io.Reader) error {
		descriptor, decodeErr = DecodeDescriptor(io.TeeReader(reader, captured), repository.limits)
		return decodeErr
	})
	if err != nil || !receipt.Complete {
		if err != nil {
			if decodeErr != nil && !errors.Is(decodeErr, context.Canceled) && !errors.Is(decodeErr, context.DeadlineExceeded) {
				owner := ""
				// LoadRecord returns the reference only after the complete
				// object digest verified, so captured holds sealed bytes.
				if ref.ID == observed.ID && ref.ID != "" && !captured.overflow {
					owner = invalidDescriptorProfileID(captured.data)
				}
				return loadedDescriptor{}, owner, fmt.Errorf("%w: storage index descriptor is invalid", ErrIntegrity)
			}
			return loadedDescriptor{}, "", err
		}
		return loadedDescriptor{}, "", fmt.Errorf("%w: storage index descriptor load is incomplete", ErrIntegrity)
	}
	if observed.SizeBytes != 0 && ref.SizeBytes != observed.SizeBytes {
		return loadedDescriptor{}, "", fmt.Errorf("%w: storage index descriptor size changed", ErrIntegrity)
	}
	return loadedDescriptor{ref: ref, descriptor: descriptor}, "", nil
}

// invalidDescriptorProfileID extracts the exact top-level profile_id string
// from one undecodable descriptor line. It requires valid JSON without
// duplicate keys and a well-formed profile ID; anything else is
// unattributable and returns "".
func invalidDescriptorProfileID(raw []byte) string {
	if len(raw) == 0 || int64(len(raw)) > maxSmallRecordBytes || !json.Valid(raw) {
		return ""
	}
	if err := rejectDuplicateObjectKeys(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return ""
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ""
	}
	value, ok := envelope["profile_id"]
	if !ok {
		return ""
	}
	var profileID string
	if err := json.Unmarshal(value, &profileID); err != nil || validateOpaqueID(profileID, "profile") != nil {
		return ""
	}
	return profileID
}

// boundedCapture keeps at most limit bytes written to it and records overflow.
type boundedCapture struct {
	limit    int64
	data     []byte
	overflow bool
}

func (capture *boundedCapture) Write(value []byte) (int, error) {
	remaining := capture.limit - int64(len(capture.data))
	if int64(len(value)) > remaining {
		capture.overflow = true
		if remaining > 0 {
			capture.data = append(capture.data, value[:remaining]...)
		}
		return len(value), nil
	}
	capture.data = append(capture.data, value...)
	return len(value), nil
}

// incompleteDescriptorListingError explains an incomplete descriptor listing
// and wraps ErrRecordListIncomplete as well. Only a record_limit stop means
// that too many descriptors exist, which prune can fix; entry and path limits
// are store-wide budgets explained by the metastore.
func (repository *Repository) incompleteDescriptorListingError(listing descriptorListing, maxRecords int) error {
	if listing.stopReason == "record_limit" {
		return fmt.Errorf("%w: more than %d descriptor records exist across all profiles; run ptctl storage index prune --acknowledge-index-prune for each profile, with --retain-generations at most %d divided by the number of profiles (%w)",
			ErrSnapshotSelectionIncomplete, maxRecords, repository.limits.MaxSnapshots, metastore.ErrRecordListIncomplete)
	}
	return fmt.Errorf("%w: descriptor listing stopped at the private store budget: %w", ErrSnapshotSelectionIncomplete, listing.incompleteErr)
}

func descriptorMatchesProfile(descriptor SnapshotDescriptor, profile Profile) bool {
	return descriptor.ProfileID == profile.ID && descriptor.ProfileRevision == profile.Revision &&
		descriptor.Platform == profile.Platform && descriptor.PathEncoding == profile.PathEncoding
}

// maintenanceMaxRecords is the listing budget for refresh generation checks
// and retention. It is larger than MaxSnapshots so an installation that has
// already accumulated more descriptors than latest selection lists can still
// refresh exactly and prune itself back below the selection budget.
func (repository *Repository) maintenanceMaxRecords() int {
	budget := metastore.DefaultRecordLimits().MaxRecords
	if repository.maintenanceRecords > 0 {
		budget = repository.maintenanceRecords
	}
	return max(budget, repository.limits.MaxSnapshots)
}

func (repository *Repository) recordLimits(maxRecords int) metastore.RecordLimits {
	limits := metastore.DefaultRecordLimits()
	limits.MaxRecordBytes = repository.limits.MaxSnapshotBytes
	limits.MaxRecords = maxRecords
	return limits
}
