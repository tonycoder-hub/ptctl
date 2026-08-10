package storageindex

import (
	"bytes"
	"context"
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
	existing, complete, err := repository.loadProfiles(ctx)
	if err != nil {
		return receipt, err
	}
	if !complete {
		return receipt, ErrProfileSelectionIncomplete
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
	profiles, complete, err := repository.loadProfiles(ctx)
	result.RecordsConsidered = len(profiles)
	if err != nil {
		return result, err
	}
	if !complete {
		result.Status = "incomplete"
		result.StopReason = "record_listing_incomplete"
		return result, ErrProfileSelectionIncomplete
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

	descriptors, complete, err := repository.loadDescriptors(ctx)
	result.RecordsConsidered = len(descriptors)
	if err != nil {
		result.Status = "incomplete"
		result.StopReason = "descriptor_invalid"
		return result, err
	}
	if !complete {
		result.Status = "incomplete"
		result.StopReason = "record_listing_incomplete"
		return result, ErrSnapshotSelectionIncomplete
	}
	matches := make([]loadedDescriptor, 0)
	for _, descriptor := range descriptors {
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

func (repository *Repository) loadProfiles(ctx context.Context) ([]loadedProfile, bool, error) {
	listed, err := repository.store.ListRecords(ctx, metastore.RecordKindStorageProfileV1, repository.recordLimits(repository.limits.MaxProfiles))
	if err != nil {
		return nil, false, err
	}
	if !listed.Complete {
		return nil, false, nil
	}
	profiles := make([]loadedProfile, 0, len(listed.Records))
	for _, ref := range listed.Records {
		loaded, err := repository.loadProfile(ctx, ref)
		if err != nil {
			return nil, false, err
		}
		profiles = append(profiles, loaded)
	}
	return profiles, true, nil
}

func (repository *Repository) loadProfile(ctx context.Context, observed metastore.RecordRef) (loadedProfile, error) {
	var profile Profile
	ref, receipt, err := repository.store.LoadRecord(ctx, metastore.RecordKindStorageProfileV1, observed.ID, repository.recordLimits(repository.limits.MaxProfiles), func(reader io.Reader) error {
		var decodeErr error
		profile, decodeErr = DecodeProfile(reader)
		return decodeErr
	})
	if err != nil || !receipt.Complete {
		if err != nil {
			return loadedProfile{}, err
		}
		return loadedProfile{}, fmt.Errorf("storage profile record load is incomplete")
	}
	if observed.SizeBytes != 0 && ref.SizeBytes != observed.SizeBytes {
		return loadedProfile{}, fmt.Errorf("storage profile record size changed")
	}
	return loadedProfile{ref: ref, profile: profile}, nil
}

func (repository *Repository) loadDescriptors(ctx context.Context) ([]loadedDescriptor, bool, error) {
	listed, err := repository.store.ListRecords(ctx, metastore.RecordKindStorageIndexDescriptorV1, repository.recordLimits(repository.limits.MaxSnapshots))
	if err != nil {
		return nil, false, err
	}
	if !listed.Complete {
		return nil, false, nil
	}
	descriptors := make([]loadedDescriptor, 0, len(listed.Records))
	for _, ref := range listed.Records {
		loaded, err := repository.loadDescriptor(ctx, ref)
		if err != nil {
			return nil, false, err
		}
		descriptors = append(descriptors, loaded)
	}
	return descriptors, true, nil
}

func (repository *Repository) loadDescriptor(ctx context.Context, observed metastore.RecordRef) (loadedDescriptor, error) {
	var descriptor SnapshotDescriptor
	ref, receipt, err := repository.store.LoadRecord(ctx, metastore.RecordKindStorageIndexDescriptorV1, observed.ID, repository.recordLimits(repository.limits.MaxSnapshots), func(reader io.Reader) error {
		var decodeErr error
		descriptor, decodeErr = DecodeDescriptor(reader, repository.limits)
		return decodeErr
	})
	if err != nil || !receipt.Complete {
		if err != nil {
			return loadedDescriptor{}, err
		}
		return loadedDescriptor{}, fmt.Errorf("storage index descriptor load is incomplete")
	}
	if observed.SizeBytes != 0 && ref.SizeBytes != observed.SizeBytes {
		return loadedDescriptor{}, fmt.Errorf("storage index descriptor size changed")
	}
	return loadedDescriptor{ref: ref, descriptor: descriptor}, nil
}

func descriptorMatchesProfile(descriptor SnapshotDescriptor, profile Profile) bool {
	return descriptor.ProfileID == profile.ID && descriptor.ProfileRevision == profile.Revision &&
		descriptor.Platform == profile.Platform && descriptor.PathEncoding == profile.PathEncoding
}

func (repository *Repository) recordLimits(maxRecords int) metastore.RecordLimits {
	limits := metastore.DefaultRecordLimits()
	limits.MaxRecordBytes = repository.limits.MaxSnapshotBytes
	limits.MaxRecords = maxRecords
	return limits
}
