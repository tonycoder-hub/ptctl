package storageindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const refreshEffect = "read_storage_metadata+write_private_storage_index"

type RefreshOptions struct {
	Clock func() time.Time
	// afterDataPublication is a package-local test seam for exercising a
	// store-root replacement between the two immutable publications.
	afterDataPublication func()
	// afterDescriptorPublication is a package-local test seam for exercising
	// a concurrent publication between the descriptor commit and its
	// generation uniqueness recheck.
	afterDescriptorPublication func()
}

type RefreshResult struct {
	Effect                string                        `json:"effect"`
	Status                string                        `json:"status"`
	WritesPerformed       int                           `json:"writes_performed"`
	ProfileID             string                        `json:"profile_id"`
	ProfileRevision       string                        `json:"profile_revision"`
	Generation            uint64                        `json:"generation"`
	SnapshotID            string                        `json:"snapshot_id,omitempty"`
	ObservedAtStart       time.Time                     `json:"observed_at_start,omitempty"`
	ObservedAtEnd         time.Time                     `json:"observed_at_end,omitempty"`
	DataRecord            metastore.RecordRef           `json:"data_record"`
	DescriptorRecord      metastore.RecordRef           `json:"descriptor_record"`
	DataPublication       metastore.RecordImportReceipt `json:"data_publication"`
	DescriptorPublication metastore.RecordImportReceipt `json:"descriptor_publication"`
	Scan                  storage.FullInventoryResult   `json:"scan"`
	StopReasons           []string                      `json:"stop_reasons"`
	authorityDigest       [sha256.Size]byte
	authoritySet          bool
}

// HasLiveAuthority reports whether this is the unchanged process-local result
// of one complete Refresh for the exact immutable profile. Serialized reports
// lose the private authority bit, and changing any receipt, scan accounting,
// root observation, or record reference invalidates the digest.
func (result RefreshResult) HasLiveAuthority(profile Profile) bool {
	if !result.authoritySet || result.Status != "stored" || !result.Scan.Complete ||
		!fullInventoryRootsComplete(result.Scan.Roots) || len(result.StopReasons) != 0 ||
		result.ProfileID != profile.ID || result.ProfileRevision != profile.Revision ||
		result.SnapshotID == "" || result.DataRecord.ID == "" || result.DescriptorRecord.ID == "" ||
		result.ObservedAtStart.IsZero() || result.ObservedAtEnd.Before(result.ObservedAtStart) || profile.Validate() != nil {
		return false
	}
	digest := refreshAuthorityDigest(result)
	return subtle.ConstantTimeCompare(result.authorityDigest[:], digest[:]) == 1
}

func refreshAuthorityDigest(result RefreshResult) [sha256.Size]byte {
	// RefreshResult contains no maps. encoding/json therefore gives a stable
	// field-ordered projection while deliberately omitting the private fields.
	type publicRefresh RefreshResult
	raw, err := json.Marshal(publicRefresh(result))
	if err != nil {
		return [sha256.Size]byte{}
	}
	payload := append([]byte("ptctl-storage-index-refresh-authority-v1\x00"), raw...)
	return sha256.Sum256(payload)
}

type streamedInventory struct {
	result storage.FullInventoryResult
	footer SnapshotFooter
	err    error
}

var errHistoricalInventoryIncomplete = errors.New("historical inventory capture is incomplete")

// Refresh performs one bounded live metadata enumeration and publishes an
// immutable data record followed by its descriptor commit. A data object can
// become an orphan if descriptor publication fails; latest selection ignores
// such objects because it lists descriptors only.
//
// The store has no exclusive lock, so publication is optimistic: the
// generation is one more than the highest published generation of the exact
// profile (a tie at that maximum is superseded rather than blocking), and the
// same-profile descriptors are re-listed immediately before and after the
// descriptor commit. A refresh that observes a competing descriptor at its
// generation (or a newer one before committing) fails with
// ErrSnapshotGenerationConflict and never reports a tie as stored; retrying
// publishes a newer unique generation. Refresh never removes records.
func (repository *Repository) Refresh(ctx context.Context, profile Profile, options RefreshOptions) (RefreshResult, error) {
	result := RefreshResult{
		Effect: refreshEffect, Status: "incomplete", ProfileID: profile.ID, ProfileRevision: profile.Revision,
		StopReasons: []string{}, Scan: emptyFullInventoryResult(profile),
	}
	if repository == nil || repository.store == nil {
		return result, fmt.Errorf("storage index repository is unavailable")
	}
	if err := ValidateProfileForLiveUse(profile, repository.limits); err != nil {
		return result, err
	}
	selectedProfile, err := repository.SelectProfile(ctx, profile.ID)
	if err != nil {
		return result, err
	}
	if selectedProfile.Profile.Revision != profile.Revision {
		return result, ErrProfileConflict
	}
	profile = selectedProfile.Profile
	if err := ValidateProfileForLiveUse(profile, repository.limits); err != nil {
		return result, err
	}
	result.ProfileID = profile.ID
	result.ProfileRevision = profile.Revision

	descriptorCache := map[metastore.RecordID]loadedDescriptor{}
	generation, err := repository.nextGeneration(ctx, profile, descriptorCache)
	if err != nil {
		return result, err
	}
	result.Generation = generation

	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	header, err := NewSnapshotHeader(profile, generation, clock())
	if err != nil {
		return result, err
	}
	result.SnapshotID = header.SnapshotID
	result.ObservedAtStart = header.ObservedAtStart
	roots, err := fullInventoryRoots(profile)
	if err != nil {
		return result, err
	}
	scanLimits := fullInventoryLimits(profile.ScanLimits, len(roots))
	if err := scanLimits.Validate(); err != nil {
		return result, err
	}

	streamContext, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	reader, writer := io.Pipe()
	streamed := make(chan streamedInventory, 1)
	go func() {
		encoder, encoderErr := NewSnapshotEncoder(writer, header, repository.limits)
		if encoderErr != nil {
			_ = writer.CloseWithError(encoderErr)
			streamed <- streamedInventory{result: emptyFullInventoryResult(profile), err: encoderErr}
			return
		}
		scan, scanErr := storage.StreamRegularFileInventory(streamContext, roots, storage.FullInventoryOptions{
			Limits: scanLimits, AllowNetwork: profile.AllowNetwork,
		}, func(record storage.RegularFileIndexEntry) error {
			return encoder.WriteEntry(Entry{
				Type: "file", RootID: record.RootID,
				RelativeComponentsRawBase64: append([]string(nil), record.RelativeComponentsRawBase64...),
				SizeBytes:                   record.SizeBytes, ModifiedUnixNanos: record.ModifiedUnixNanos, IdentityHint: record.IdentityHint,
			})
		})
		if scanErr != nil {
			_ = writer.CloseWithError(scanErr)
			streamed <- streamedInventory{result: scan, err: scanErr}
			return
		}
		if !scan.Complete || !fullInventoryRootsComplete(scan.Roots) {
			_ = writer.CloseWithError(errHistoricalInventoryIncomplete)
			streamed <- streamedInventory{result: scan, err: errHistoricalInventoryIncomplete}
			return
		}
		footer, closeErr := encoder.Close(clock())
		if closeErr != nil {
			_ = writer.CloseWithError(closeErr)
			streamed <- streamedInventory{result: scan, err: closeErr}
			return
		}
		_ = writer.Close()
		streamed <- streamedInventory{result: scan, footer: footer}
	}()

	dataSource := &producerErrorReader{reader: reader}
	dataRef, dataReceipt, importErr := repository.store.ImportRecord(ctx, metastore.RecordKindStorageIndexDataV1, dataSource, repository.recordLimits(repository.limits.MaxSnapshots))
	_ = reader.Close()
	if importErr != nil {
		cancelStream()
	}
	streamResult := <-streamed
	result.Scan = streamResult.result
	result.ObservedAtEnd = streamResult.footer.ObservedAtEnd
	result.DataRecord = dataRef
	result.DataPublication = dataReceipt
	result.WritesPerformed += dataReceipt.WritesPerformed
	if importErr != nil && dataSource.err == nil && !errors.Is(importErr, context.Canceled) && !errors.Is(importErr, context.DeadlineExceeded) {
		// The import failed without observing a producer error. Closing the
		// pipe and cancelling the scan then made the producer fail too, so its
		// error is a consequence; report the publication failure instead.
		result.StopReasons = appendUniqueString(result.StopReasons, publicationStopReason("data", importErr))
		return result, importErr
	}
	if streamResult.err != nil {
		result.StopReasons = appendUniqueString(result.StopReasons, streamStopReason(streamResult))
		if errors.Is(streamResult.err, errHistoricalInventoryIncomplete) || errors.Is(streamResult.err, errSnapshotByteBudget) {
			// Both are budget/enumeration incompleteness: no data record is
			// published and the report carries a distinct stop reason.
			return result, nil
		}
		if errors.Is(streamResult.err, context.Canceled) || errors.Is(streamResult.err, context.DeadlineExceeded) {
			return result, streamResult.err
		}
		return result, fmt.Errorf("capture storage index stream failed")
	}
	if importErr != nil {
		result.StopReasons = appendUniqueString(result.StopReasons, publicationStopReason("data", importErr))
		return result, importErr
	}
	if !result.Scan.Complete || !fullInventoryRootsComplete(result.Scan.Roots) || streamResult.footer.Files != result.Scan.Stats.FilesEmitted ||
		streamResult.footer.PathBytes != result.Scan.Stats.EmittedPathBytes {
		result.StopReasons = appendUniqueString(result.StopReasons, "inventory_receipt_mismatch")
		return result, fmt.Errorf("%w: inventory receipt does not match its sealed stream", ErrIntegrity)
	}
	if options.afterDataPublication != nil {
		options.afterDataPublication()
	}
	// A refresh that lost the race fails here, before it can publish a tie.
	if err := repository.checkGenerationUnclaimed(ctx, profile, generation, "", descriptorCache); err != nil {
		result.StopReasons = appendUniqueString(result.StopReasons, generationCheckStopReason(err))
		return result, err
	}

	descriptor := SnapshotDescriptor{
		Format: SnapshotDescriptorFormat, ID: header.SnapshotID, Generation: generation,
		ProfileID: profile.ID, ProfileRevision: profile.Revision, Platform: profile.Platform, PathEncoding: profile.PathEncoding,
		DataRecordID: dataRef.ID.String(), ObservedAtStart: header.ObservedAtStart, ObservedAtEnd: streamResult.footer.ObservedAtEnd,
		Files: streamResult.footer.Files, PathBytes: streamResult.footer.PathBytes,
		EnumerationScope: "complete_snapshot", LiveFreshness: "unproven_since_snapshot",
		Roots: snapshotRootObservations(result.Scan.Roots),
	}
	descriptorRaw, err := EncodeDescriptor(descriptor, repository.limits)
	if err != nil {
		result.StopReasons = appendUniqueString(result.StopReasons, "descriptor_invalid")
		return result, err
	}
	descriptorRef, descriptorReceipt, descriptorErr := repository.store.ImportRecord(ctx, metastore.RecordKindStorageIndexDescriptorV1, bytes.NewReader(descriptorRaw), repository.recordLimits(repository.limits.MaxSnapshots))
	result.DescriptorRecord = descriptorRef
	result.DescriptorPublication = descriptorReceipt
	result.WritesPerformed += descriptorReceipt.WritesPerformed
	if descriptorErr != nil {
		result.StopReasons = appendUniqueString(result.StopReasons, publicationStopReason("descriptor", descriptorErr))
		return result, descriptorErr
	}
	verification, err := repository.store.VerifyRecordSet(ctx, []metastore.RecordRef{descriptorRef, dataRef}, repository.recordLimits(repository.limits.MaxSnapshots))
	if err != nil {
		result.StopReasons = appendUniqueString(result.StopReasons, "post_publish_revalidation_failed")
		return result, err
	}
	if !verification.Complete || verification.RecordsVerified != 2 {
		result.StopReasons = appendUniqueString(result.StopReasons, "post_publish_revalidation_failed")
		return result, fmt.Errorf("%w: storage index publication could not be revalidated", ErrIntegrity)
	}
	if options.afterDescriptorPublication != nil {
		options.afterDescriptorPublication()
	}
	// Two refreshes may pass the pre-commit check together. Neither may then
	// claim a stored generation that latest selection reports as ambiguous;
	// the tie remains until the next refresh supersedes it.
	if err := repository.checkGenerationUnclaimed(ctx, profile, generation, descriptorRef.ID, descriptorCache); err != nil {
		result.StopReasons = appendUniqueString(result.StopReasons, generationCheckStopReason(err))
		return result, err
	}
	result.Status = "stored"
	result.authorityDigest = refreshAuthorityDigest(result)
	result.authoritySet = true
	return result, nil
}

// nextGeneration returns one more than the highest generation published for
// the exact profile revision. Unlike latest selection it tolerates a tie at
// that maximum, because publishing a strictly newer complete generation is
// the recovery for a profile left ambiguous by concurrent refreshes. It uses
// the larger maintenance listing budget so an installation that already
// exceeds MaxSnapshots descriptors can still refresh and then be pruned. Any
// invalid descriptor that could belong to this profile still fails closed.
func (repository *Repository) nextGeneration(ctx context.Context, profile Profile, cache map[metastore.RecordID]loadedDescriptor) (uint64, error) {
	listing, err := repository.listDescriptors(ctx, profile.ID, repository.maintenanceMaxRecords(), false, cache)
	if err != nil {
		return 0, err
	}
	if !listing.complete {
		return 0, incompleteDescriptorListingError(listing, repository.maintenanceMaxRecords())
	}
	maximum := uint64(0)
	for _, candidate := range listing.descriptors {
		if descriptorMatchesProfile(candidate.descriptor, profile) && candidate.descriptor.Generation > maximum {
			maximum = candidate.descriptor.Generation
		}
	}
	if maximum == math.MaxUint64 {
		return 0, fmt.Errorf("storage index generation is exhausted")
	}
	return maximum + 1, nil
}

// checkGenerationUnclaimed re-lists the exact profile's descriptors. Before
// the descriptor commit (own == "") any descriptor at or above generation is
// a conflict. After it, only another descriptor at the same generation is: a
// newer generation merely supersedes this one without making it ambiguous.
func (repository *Repository) checkGenerationUnclaimed(ctx context.Context, profile Profile, generation uint64, own metastore.RecordID, cache map[metastore.RecordID]loadedDescriptor) error {
	listing, err := repository.listDescriptors(ctx, profile.ID, repository.maintenanceMaxRecords(), false, cache)
	if err != nil {
		return err
	}
	if !listing.complete {
		return fmt.Errorf("generation uniqueness could not be rechecked: %w", incompleteDescriptorListingError(listing, repository.maintenanceMaxRecords()))
	}
	for _, candidate := range listing.descriptors {
		if !descriptorMatchesProfile(candidate.descriptor, profile) || (own != "" && candidate.ref.ID == own) {
			continue
		}
		if candidate.descriptor.Generation == generation || (own == "" && candidate.descriptor.Generation > generation) {
			return fmt.Errorf("%w: generation %d was published concurrently; retry the refresh", ErrSnapshotGenerationConflict, generation)
		}
	}
	return nil
}

func generationCheckStopReason(err error) string {
	if errors.Is(err, ErrSnapshotGenerationConflict) {
		return "generation_conflict"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "context_cancelled"
	}
	return "generation_check_failed"
}

func fullInventoryRoots(profile Profile) ([]storage.FullInventoryRoot, error) {
	roots := make([]storage.FullInventoryRoot, len(profile.Roots))
	for index, root := range profile.Roots {
		path, err := root.Path()
		if err != nil {
			return nil, err
		}
		roots[index] = storage.FullInventoryRoot{ID: root.ID, Path: path}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })
	return roots, nil
}

func fullInventoryLimits(profileLimits ScanLimits, roots int) storage.FullInventoryLimits {
	return storage.FullInventoryLimits{
		MaxRoots: roots, MaxDepth: profileLimits.MaxDepth, MaxDirectories: profileLimits.MaxDirectories,
		MaxEntries: profileLimits.MaxEntries, MaxEntriesPerDirectory: profileLimits.MaxEntriesPerDirectory,
		MaxFiles: profileLimits.MaxFiles, MaxPathBytes: profileLimits.MaxPathBytes, MaxIssues: profileLimits.MaxIssues,
	}
}

func snapshotRootObservations(roots []storage.FullInventoryRootObservation) []SnapshotRootObservation {
	result := make([]SnapshotRootObservation, len(roots))
	for index, root := range roots {
		result[index] = SnapshotRootObservation{
			RootID: root.ID, Status: root.Status,
			FilesystemIdentityHint: root.FilesystemIdentityHint, RootIdentityHint: root.RootIdentityHint,
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RootID < result[j].RootID })
	return result
}

func fullInventoryRootsComplete(roots []storage.FullInventoryRootObservation) bool {
	if len(roots) == 0 {
		return false
	}
	for _, root := range roots {
		if root.Status != "complete" || root.FilesystemIdentityHint == "" || root.RootIdentityHint == "" {
			return false
		}
	}
	return true
}

func emptyFullInventoryResult(profile Profile) storage.FullInventoryResult {
	limits := fullInventoryLimits(profile.ScanLimits, len(profile.Roots))
	return storage.FullInventoryResult{
		Complete: false, PathConfinement: "not_started", Limits: limits,
		Roots: []storage.FullInventoryRootObservation{}, LimitHits: []string{}, StopReasons: []string{}, Issues: []storage.ScanIssue{}, Warnings: []string{},
	}
}

// producerErrorReader records a non-EOF read error from the snapshot pipe.
// ImportRecord reads synchronously in the Refresh goroutine, so the field is
// only accessed there.
type producerErrorReader struct {
	reader io.Reader
	err    error
}

func (source *producerErrorReader) Read(buffer []byte) (int, error) {
	count, err := source.reader.Read(buffer)
	if err != nil && err != io.EOF && source.err == nil {
		source.err = err
	}
	return count, err
}

func streamStopReason(stream streamedInventory) string {
	if errors.Is(stream.err, errHistoricalInventoryIncomplete) {
		return "inventory_incomplete"
	}
	if errors.Is(stream.err, errSnapshotByteBudget) {
		return "snapshot_byte_limit"
	}
	if errors.Is(stream.err, context.Canceled) || errors.Is(stream.err, context.DeadlineExceeded) {
		return "context_cancelled"
	}
	return "inventory_stream_failed"
}

func publicationStopReason(stage string, err error) string {
	suffix := "failed"
	if errors.Is(err, metastore.ErrDurabilityUnconfirmed) {
		suffix = "durability_unconfirmed"
	} else if errors.Is(err, metastore.ErrPublishedCleanupIncomplete) {
		suffix = "cleanup_incomplete"
	} else if errors.Is(err, metastore.ErrCorruptRecord) {
		suffix = "integrity_failed"
	}
	return stage + "_publication_" + suffix
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
