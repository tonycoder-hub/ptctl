package storageindex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
)

const (
	// DefaultRetainedSnapshotGenerations is the number of distinct newest
	// generations retention keeps per profile. DefaultMaxProfiles profiles at
	// this depth stay below DefaultMaxSnapshots, so latest selection keeps a
	// complete descriptor listing.
	DefaultRetainedSnapshotGenerations = 3

	snapshotPruneEffect = "read_private_storage_index+remove_private_storage_index"
)

// PruneOptions selects which superseded generations PruneSnapshots removes.
type PruneOptions struct {
	// RetainGenerations is the number of distinct newest observed
	// generations kept; it must be in 1..MaxSnapshots.
	RetainGenerations int
	// Protect names descriptor records which must survive this run, such as
	// one a caller has just published and is still reading.
	Protect []metastore.RecordID
	// refresh is set only by RefreshResult.RetentionOptions.
	refresh *refreshListing
}

// refreshListing is the complete descriptor listing a stored Refresh verified
// after its descriptor commit.
type refreshListing struct {
	repository *Repository
	profileID  string
	listing    descriptorListing
}

// RetentionOptions returns the PruneOptions for retention right after this
// stored refresh. The new descriptor is protected, and PruneSnapshots on the
// same repository and profile reuses the complete descriptor listing Refresh
// verified after its commit instead of listing the store again. Descriptors
// published since are newer generations, which only make retention keep
// more, or stalled ones that PruneSnapshots would treat as superseded anyway.
func (result RefreshResult) RetentionOptions(retainGenerations int) PruneOptions {
	return PruneOptions{RetainGenerations: retainGenerations, Protect: []metastore.RecordID{result.DescriptorRecord.ID}, refresh: result.retention}
}

// PrunedSnapshot is one superseded generation handled by PruneSnapshots.
// DataRemoval is removed, already_absent, or retained_referenced;
// DescriptorRemoval is removed or already_absent. An orphan data record,
// which no descriptor references, has an empty DescriptorRecordID and
// DescriptorRemoval already_absent.
type PrunedSnapshot struct {
	Generation         uint64             `json:"generation"`
	SnapshotID         string             `json:"snapshot_id"`
	DescriptorRecordID metastore.RecordID `json:"descriptor_record_id"`
	DataRecordID       metastore.RecordID `json:"data_record_id"`
	DataRemoval        string             `json:"data_removal"`
	DescriptorRemoval  string             `json:"descriptor_removal"`
}

// PruneResult reports one retention pass for one exact profile revision.
// Status is complete when the descriptor listing was complete and every
// superseded generation was handled, partial when only a verified subset of
// descriptors could be listed, and incomplete otherwise.
type PruneResult struct {
	Effect              string              `json:"effect"`
	Status              string              `json:"status"`
	Complete            bool                `json:"complete"`
	WritesPerformed     int                 `json:"writes_performed"`
	ProfileID           string              `json:"profile_id"`
	ProfileRevision     string              `json:"profile_revision"`
	RetainGenerations   int                 `json:"retain_generations"`
	ListingComplete     bool                `json:"listing_complete"`
	RecordsConsidered   int                 `json:"records_considered"`
	DescriptorsMatched  int                 `json:"descriptors_matched"`
	RetainedGenerations []uint64            `json:"retained_generations"`
	Pruned              []PrunedSnapshot    `json:"pruned"`
	Store               metastore.StoreInfo `json:"store"`
	StopReasons         []string            `json:"stop_reasons"`
}

// PruneSnapshots removes superseded index generations of one exact profile
// revision. A descriptor is removed only when at least RetainGenerations
// distinct newer generations of the same profile were observed, so the
// latest generation (and any tie at it) is never removed, and neither are
// protected records or descriptors of other profiles. That rule is safe on a
// partial listing: it never needs to see every descriptor, so installations
// that already exceed the latest-selection budget can recover. Concurrent
// refreshes only add newer generations, which cannot make an observed
// superseded generation current again.
//
// For each superseded generation the data record is removed before its
// descriptor. A crash between the two leaves an older, never-selected
// descriptor whose data is absent: latest selection ignores it, an explicit
// read of it fails closed, and rerunning prune removes it. Removal is exact
// (digest and size bound); Refresh never prunes by itself.
//
// After a complete descriptor listing, prune also removes orphan data that a
// failed, conflicting, or interrupted refresh published without a
// descriptor: a data record no listed descriptor references whose verified
// header names this profile revision and a superseded generation. A refresh
// still holding such a record fails its pre-commit check, because a newer
// generation is already published and the latest one is never pruned; one
// stalled past that check can only commit a superseded descriptor without
// data, the fail-closed state described above.
func (repository *Repository) PruneSnapshots(ctx context.Context, profile Profile, options PruneOptions) (PruneResult, error) {
	result := PruneResult{
		Effect: snapshotPruneEffect, Status: "incomplete", ProfileID: profile.ID, ProfileRevision: profile.Revision,
		RetainGenerations: options.RetainGenerations, RetainedGenerations: []uint64{}, Pruned: []PrunedSnapshot{}, StopReasons: []string{},
	}
	if repository == nil || repository.store == nil {
		return result, fmt.Errorf("storage index repository is unavailable")
	}
	result.Store = repository.store.Info()
	if err := profile.Validate(); err != nil {
		return result, err
	}
	if options.RetainGenerations < 1 || options.RetainGenerations > repository.limits.MaxSnapshots {
		return result, fmt.Errorf("retained storage index generations must be in 1..%d", repository.limits.MaxSnapshots)
	}
	protected := make(map[metastore.RecordID]struct{}, len(options.Protect))
	for _, id := range options.Protect {
		if parsed, err := metastore.ParseRecordID(id.String()); err != nil || parsed != id {
			return result, fmt.Errorf("protected storage index descriptor record is invalid")
		}
		protected[id] = struct{}{}
	}

	maxRecords := repository.maintenanceMaxRecords()
	limits := repository.recordLimits(maxRecords)
	var listing descriptorListing
	var err error
	if reuse := options.refresh; reuse != nil && reuse.repository == repository && reuse.profileID == profile.ID {
		listing = reuse.listing
	} else {
		listing, err = repository.listDescriptors(ctx, profile.ID, maxRecords, true, nil)
	}
	result.RecordsConsidered = len(listing.descriptors)
	result.ListingComplete = listing.complete
	if err != nil {
		result.StopReasons = appendUniqueString(result.StopReasons, pruneStopReason("descriptor_listing", err))
		return result, err
	}
	if !listing.complete {
		result.StopReasons = appendUniqueString(result.StopReasons, "record_listing_incomplete")
		if len(listing.descriptors) == 0 {
			return result, fmt.Errorf("no descriptor records could be listed: %w", repository.incompleteDescriptorListingError(listing, maxRecords))
		}
	}

	matches := make([]loadedDescriptor, 0)
	seen := make(map[uint64]struct{})
	generations := make([]uint64, 0)
	for _, candidate := range listing.descriptors {
		if !descriptorMatchesProfile(candidate.descriptor, profile) {
			continue
		}
		matches = append(matches, candidate)
		if _, ok := seen[candidate.descriptor.Generation]; !ok {
			seen[candidate.descriptor.Generation] = struct{}{}
			generations = append(generations, candidate.descriptor.Generation)
		}
	}
	result.DescriptorsMatched = len(matches)
	sort.Slice(generations, func(i, j int) bool { return generations[i] > generations[j] })
	result.RetainedGenerations = append(result.RetainedGenerations, generations[:min(len(generations), options.RetainGenerations)]...)
	// Generations below oldestRetained are superseded; zero supersedes none.
	oldestRetained := uint64(0)
	if len(generations) >= options.RetainGenerations {
		oldestRetained = generations[options.RetainGenerations-1]
	}

	superseded := make([]loadedDescriptor, 0)
	removing := make(map[metastore.RecordID]struct{})
	for _, candidate := range matches {
		if _, keep := protected[candidate.ref.ID]; !keep && candidate.descriptor.Generation < oldestRetained {
			superseded = append(superseded, candidate)
			removing[candidate.ref.ID] = struct{}{}
		}
	}
	sort.Slice(superseded, func(i, j int) bool {
		if superseded[i].descriptor.Generation != superseded[j].descriptor.Generation {
			return superseded[i].descriptor.Generation < superseded[j].descriptor.Generation
		}
		return superseded[i].ref.ID < superseded[j].ref.ID
	})
	// referenced holds the data record of every listed descriptor, mapped to
	// whether a descriptor this pass keeps still references it.
	referenced := make(map[string]bool, len(listing.descriptors))
	for _, candidate := range listing.descriptors {
		_, removed := removing[candidate.ref.ID]
		referenced[candidate.descriptor.DataRecordID] = referenced[candidate.descriptor.DataRecordID] || !removed
	}

	collectOrphans := listing.complete && oldestRetained > 0
	dataSizes := make(map[metastore.RecordID]int64)
	dataListing := metastore.RecordListResult{}
	if len(superseded) > 0 || collectOrphans {
		dataListing, err = repository.store.ListRecords(ctx, metastore.RecordKindStorageIndexDataV1, limits)
		if err != nil {
			result.StopReasons = appendUniqueString(result.StopReasons, pruneStopReason("data_listing", err))
			return result, err
		}
		for _, ref := range dataListing.Records {
			dataSizes[ref.ID] = ref.SizeBytes
		}
	}

	for _, candidate := range superseded {
		if err := ctx.Err(); err != nil {
			result.StopReasons = appendUniqueString(result.StopReasons, "context_cancelled")
			return result, err
		}
		dataID, err := metastore.ParseRecordID(candidate.descriptor.DataRecordID)
		if err != nil {
			result.StopReasons = appendUniqueString(result.StopReasons, "descriptor_invalid")
			return result, fmt.Errorf("%w: storage index descriptor data reference is invalid", ErrIntegrity)
		}
		entry := PrunedSnapshot{
			Generation: candidate.descriptor.Generation, SnapshotID: candidate.descriptor.ID,
			DescriptorRecordID: candidate.ref.ID, DataRecordID: dataID,
		}
		if referenced[candidate.descriptor.DataRecordID] {
			entry.DataRemoval = "retained_referenced"
		} else {
			outcome, writes, err := repository.removeSnapshotData(ctx, dataID, dataSizes, dataListing.Complete, limits)
			result.WritesPerformed += writes
			if err != nil {
				result.StopReasons = appendUniqueString(result.StopReasons, pruneStopReason("data_removal", err))
				return result, err
			}
			entry.DataRemoval = outcome
		}
		receipt, err := repository.store.RemoveRecordExact(ctx, candidate.ref, limits)
		result.WritesPerformed += receipt.WritesPerformed
		if err != nil {
			result.StopReasons = appendUniqueString(result.StopReasons, pruneStopReason("descriptor_removal", err))
			return result, err
		}
		entry.DescriptorRemoval = removalOutcome(receipt)
		result.Pruned = append(result.Pruned, entry)
	}

	if collectOrphans {
		for _, ref := range dataListing.Records {
			if _, listed := referenced[ref.ID.String()]; listed {
				continue
			}
			entry, writes, err := repository.removeOrphanData(ctx, profile, ref, oldestRetained, limits)
			result.WritesPerformed += writes
			if err != nil {
				result.StopReasons = appendUniqueString(result.StopReasons, pruneStopReason("orphan_data_removal", err))
				return result, err
			}
			if entry != nil {
				result.Pruned = append(result.Pruned, *entry)
			}
		}
	}

	if listing.complete {
		result.Status = "complete"
		result.Complete = true
	} else {
		result.Status = "partial"
	}
	return result, nil
}

// removeSnapshotData removes one unreferenced data record by exact identity.
// Its size comes from the stat-only data listing; a record missing from a
// partial listing is located by a verifying read instead.
func (repository *Repository) removeSnapshotData(ctx context.Context, id metastore.RecordID, sizes map[metastore.RecordID]int64, listingComplete bool, limits metastore.RecordLimits) (string, int, error) {
	size, listed := sizes[id]
	if !listed {
		if listingComplete {
			return "already_absent", 0, nil
		}
		ref, _, err := repository.store.LoadRecord(ctx, metastore.RecordKindStorageIndexDataV1, id, limits, func(reader io.Reader) error {
			_, err := io.Copy(io.Discard, reader)
			return err
		})
		if errors.Is(err, metastore.ErrRecordNotFound) {
			return "already_absent", 0, nil
		}
		if err != nil {
			return "", 0, err
		}
		size = ref.SizeBytes
	}
	receipt, err := repository.store.RemoveRecordExact(ctx, metastore.RecordRef{Kind: metastore.RecordKindStorageIndexDataV1, ID: id, SizeBytes: size}, limits)
	if err != nil {
		return "", receipt.WritesPerformed, err
	}
	return removalOutcome(receipt), receipt.WritesPerformed, nil
}

// removeOrphanData removes one unreferenced data record when it decodes as a
// valid snapshot of this exact profile revision at a generation below
// oldestRetained. Any record it cannot attribute that way is left alone.
func (repository *Repository) removeOrphanData(ctx context.Context, profile Profile, ref metastore.RecordRef, oldestRetained uint64, limits metastore.RecordLimits) (*PrunedSnapshot, int, error) {
	var header SnapshotHeader
	_, _, err := repository.store.LoadRecord(ctx, metastore.RecordKindStorageIndexDataV1, ref.ID, limits, func(reader io.Reader) error {
		var err error
		header, _, err = DecodeSnapshot(ctx, reader, repository.limits, func(Entry) error { return nil })
		return err
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, 0, err
	}
	if err != nil || header.ProfileID != profile.ID || header.ProfileRevision != profile.Revision || header.Generation >= oldestRetained {
		return nil, 0, nil
	}
	receipt, err := repository.store.RemoveRecordExact(ctx, ref, limits)
	if err != nil {
		return nil, receipt.WritesPerformed, err
	}
	return &PrunedSnapshot{
		Generation: header.Generation, SnapshotID: header.SnapshotID, DataRecordID: ref.ID,
		DataRemoval: removalOutcome(receipt), DescriptorRemoval: "already_absent",
	}, receipt.WritesPerformed, nil
}

func removalOutcome(receipt metastore.RecordRemovalReceipt) string {
	if receipt.AlreadyAbsent {
		return "already_absent"
	}
	return "removed"
}

func pruneStopReason(stage string, err error) string {
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return "context_cancelled"
	case errors.Is(err, metastore.ErrRemovalDurabilityUnconfirmed):
		return stage + "_durability_unconfirmed"
	case errors.Is(err, ErrIntegrity) || errors.Is(err, metastore.ErrCorruptRecord):
		return stage + "_integrity_failed"
	default:
		return stage + "_failed"
	}
}
