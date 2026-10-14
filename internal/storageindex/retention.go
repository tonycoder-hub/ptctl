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
}

// PrunedSnapshot is one superseded generation handled by PruneSnapshots.
// DataRemoval is removed, already_absent, or retained_referenced;
// DescriptorRemoval is removed or already_absent.
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
// read of it fails closed, and rerunning prune removes it. Descriptor-first
// ordering would instead leak an unreferenced data record that nothing lists.
// Removal is exact (digest and size bound); Refresh never prunes by itself.
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
	listing, err := repository.listDescriptors(ctx, profile.ID, maxRecords, true, nil)
	result.RecordsConsidered = len(listing.descriptors)
	result.ListingComplete = listing.complete
	if err != nil {
		result.StopReasons = appendUniqueString(result.StopReasons, pruneStopReason("descriptor_listing", err))
		return result, err
	}
	if !listing.complete {
		result.StopReasons = appendUniqueString(result.StopReasons, "record_listing_incomplete")
		if len(listing.descriptors) == 0 {
			return result, fmt.Errorf("%w: no descriptor records could be listed within the private store entry budget", ErrSnapshotSelectionIncomplete)
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
	retained := generations
	if len(retained) > options.RetainGenerations {
		retained = retained[:options.RetainGenerations]
	}
	result.RetainedGenerations = append(result.RetainedGenerations, retained...)

	superseded := make([]loadedDescriptor, 0)
	if len(generations) > options.RetainGenerations {
		oldestRetained := generations[options.RetainGenerations-1]
		for _, candidate := range matches {
			if _, keep := protected[candidate.ref.ID]; keep || candidate.descriptor.Generation >= oldestRetained {
				continue
			}
			superseded = append(superseded, candidate)
		}
	}
	sort.Slice(superseded, func(i, j int) bool {
		if superseded[i].descriptor.Generation != superseded[j].descriptor.Generation {
			return superseded[i].descriptor.Generation < superseded[j].descriptor.Generation
		}
		return superseded[i].ref.ID < superseded[j].ref.ID
	})
	removing := make(map[metastore.RecordID]struct{}, len(superseded))
	for _, candidate := range superseded {
		removing[candidate.ref.ID] = struct{}{}
	}
	referenced := make(map[string]struct{})
	for _, candidate := range listing.descriptors {
		if _, ok := removing[candidate.ref.ID]; !ok {
			referenced[candidate.descriptor.DataRecordID] = struct{}{}
		}
	}

	dataSizes := make(map[metastore.RecordID]int64)
	dataListingComplete := false
	if len(superseded) > 0 {
		dataListing, err := repository.store.ListRecords(ctx, metastore.RecordKindStorageIndexDataV1, limits)
		if err != nil {
			result.StopReasons = appendUniqueString(result.StopReasons, pruneStopReason("data_listing", err))
			return result, err
		}
		dataListingComplete = dataListing.Complete
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
		if _, shared := referenced[candidate.descriptor.DataRecordID]; shared {
			entry.DataRemoval = "retained_referenced"
		} else {
			outcome, writes, err := repository.removeSnapshotData(ctx, dataID, dataSizes, dataListingComplete, limits)
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
