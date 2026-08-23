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

	generation := uint64(1)
	latest, latestErr := repository.SelectSnapshot(ctx, profile, "")
	switch {
	case latestErr == nil:
		if latest.Descriptor.Generation == math.MaxUint64 {
			return result, fmt.Errorf("storage index generation is exhausted")
		}
		generation = latest.Descriptor.Generation + 1
	case errors.Is(latestErr, ErrSnapshotNotFound):
		// The first descriptor is generation one.
	default:
		return result, latestErr
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

	dataRef, dataReceipt, importErr := repository.store.ImportRecord(ctx, metastore.RecordKindStorageIndexDataV1, reader, repository.recordLimits(repository.limits.MaxSnapshots))
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
	if streamResult.err != nil {
		result.StopReasons = appendUniqueString(result.StopReasons, streamStopReason(streamResult))
		if errors.Is(streamResult.err, errHistoricalInventoryIncomplete) {
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
	result.Status = "stored"
	result.authorityDigest = refreshAuthorityDigest(result)
	result.authoritySet = true
	return result, nil
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

func streamStopReason(stream streamedInventory) string {
	if errors.Is(stream.err, errHistoricalInventoryIncomplete) {
		return "inventory_incomplete"
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
