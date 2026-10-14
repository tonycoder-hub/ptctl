package storageindex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
)

func TestPruneSnapshotsRetainsNewestGenerationsAndRemovesSupersededData(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	profile := testIndexProfile(t, repository, "media")
	other := testIndexProfile(t, repository, "other")
	descriptors := map[uint64]metastore.RecordID{}
	for generation := uint64(1); generation <= 5; generation++ {
		descriptors[generation] = importTestDescriptor(t, repository, profile, generation, fmt.Sprintf("media-%d", generation))
	}
	tie := importTestDescriptor(t, repository, profile, 5, "media-5-tie")
	otherOld := importTestDescriptor(t, repository, other, 1, "other-1")
	otherNew := importTestDescriptor(t, repository, other, 2, "other-2")
	data := map[metastore.RecordID]metastore.RecordID{}
	for _, id := range []metastore.RecordID{descriptors[1], descriptors[2], descriptors[3], descriptors[4], descriptors[5], tie, otherOld, otherNew} {
		data[id] = testDescriptorDataID(t, repository, id)
	}

	result, err := repository.PruneSnapshots(ctx, profile, PruneOptions{RetainGenerations: 2, Protect: []metastore.RecordID{descriptors[1]}})
	if err != nil || result.Status != "complete" || !result.Complete || !result.ListingComplete || result.DescriptorsMatched != 6 ||
		len(result.RetainedGenerations) != 2 || result.RetainedGenerations[0] != 5 || result.RetainedGenerations[1] != 4 || len(result.Pruned) != 2 || result.WritesPerformed == 0 {
		t.Fatalf("retention did not prune exactly the superseded generations: result=%#v err=%v", result, err)
	}
	for index, generation := range []uint64{2, 3} {
		entry := result.Pruned[index]
		if entry.Generation != generation || entry.DescriptorRecordID != descriptors[generation] || entry.DataRemoval != "removed" || entry.DescriptorRemoval != "removed" {
			t.Fatalf("pruned entry %d is unexpected: %#v", index, entry)
		}
		if testRecordPresent(t, repository, metastore.RecordKindStorageIndexDescriptorV1, descriptors[generation]) ||
			testRecordPresent(t, repository, metastore.RecordKindStorageIndexDataV1, data[descriptors[generation]]) {
			t.Fatalf("generation %d was not removed", generation)
		}
	}
	for _, kept := range []metastore.RecordID{descriptors[1], descriptors[4], descriptors[5], tie, otherOld, otherNew} {
		if !testRecordPresent(t, repository, metastore.RecordKindStorageIndexDescriptorV1, kept) || !testRecordPresent(t, repository, metastore.RecordKindStorageIndexDataV1, data[kept]) {
			t.Fatalf("retention removed a retained, protected, tied, or foreign snapshot %s", kept)
		}
	}
	// A tie at the latest generation is never resolved by deleting one side.
	if _, err := repository.SelectSnapshot(ctx, profile, ""); !errors.Is(err, ErrSnapshotAmbiguous) {
		t.Fatalf("retention changed latest tie handling: %v", err)
	}

	again, err := repository.PruneSnapshots(ctx, profile, PruneOptions{RetainGenerations: 2, Protect: []metastore.RecordID{descriptors[1]}})
	if err != nil || again.Status != "complete" || again.WritesPerformed != 0 || len(again.Pruned) != 0 {
		t.Fatalf("retention rerun was not idempotent: result=%#v err=%v", again, err)
	}
	if _, err := repository.PruneSnapshots(ctx, profile, PruneOptions{RetainGenerations: 0}); err == nil {
		t.Fatal("retention accepted a zero generation window")
	}
}

func TestPruneSnapshotsRecoversProfileOverSelectionBudget(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxSnapshots = 4
	repository := testRepositoryWithLimits(t, limits)
	ctx := context.Background()
	root := physicalIndexTempDir(t)
	writeTestFile(t, filepath.Join(root, "payload.bin"), []byte("payload"))
	profileReceipt, err := repository.CreateProfile(ctx, "media", []string{root}, false, DefaultScanLimits(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	profile := profileReceipt.Profile
	for generation := uint64(1); generation <= 6; generation++ {
		importTestDescriptor(t, repository, profile, generation, fmt.Sprintf("over-%d", generation))
	}
	if _, err := repository.SelectSnapshot(ctx, profile, ""); !errors.Is(err, ErrSnapshotSelectionIncomplete) ||
		!strings.Contains(err.Error(), "--retain-generations at most 4 divided by the number of profiles") {
		t.Fatalf("over-budget profile did not report incomplete selection with a recovery: %v", err)
	}
	// Refresh uses the maintenance budget, so it still publishes a strictly
	// newer generation rather than failing permanently.
	refreshed, err := repository.Refresh(ctx, profile, RefreshOptions{Clock: deterministicClock(time.Now().UTC())})
	if err != nil || refreshed.Status != "stored" || refreshed.Generation != 7 || refreshed.WritesPerformed != 2 {
		t.Fatalf("over-budget profile could not refresh: result=%#v err=%v", refreshed, err)
	}
	pruned, err := repository.PruneSnapshots(ctx, profile, PruneOptions{RetainGenerations: 3})
	if err != nil || pruned.Status != "complete" || len(pruned.Pruned) != 4 {
		t.Fatalf("over-budget profile was not pruned: result=%#v err=%v", pruned, err)
	}
	selected, err := repository.SelectSnapshot(ctx, profile, "")
	if err != nil || selected.Status != "selected" || selected.DescriptorRecordID != refreshed.DescriptorRecord.ID || selected.Descriptor.Generation != 7 {
		t.Fatalf("latest selection did not recover after prune: selection=%#v err=%v", selected, err)
	}
}

func TestPruneSnapshotsMakesProgressFromPartialListing(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxSnapshots = 2
	repository := testRepositoryWithLimits(t, limits)
	repository.maintenanceRecords = 3
	ctx := context.Background()
	profile := testIndexProfile(t, repository, "media")
	var latest metastore.RecordID
	for generation := uint64(1); generation <= 8; generation++ {
		latest = importTestDescriptor(t, repository, profile, generation, fmt.Sprintf("partial-%d", generation))
	}
	latestData := testDescriptorDataID(t, repository, latest)
	sawPartial := false
	for attempt := 0; attempt < 16; attempt++ {
		result, err := repository.PruneSnapshots(ctx, profile, PruneOptions{RetainGenerations: 1})
		if err != nil {
			t.Fatalf("partial prune failed: result=%#v err=%v", result, err)
		}
		if result.Status == "complete" {
			break
		}
		if result.Status != "partial" || result.ListingComplete || !containsString(result.StopReasons, "record_listing_incomplete") || len(result.Pruned) == 0 {
			t.Fatalf("partial prune made no verified progress: result=%#v", result)
		}
		sawPartial = true
	}
	if !sawPartial {
		t.Fatal("prune never exercised a partial descriptor listing")
	}
	listed, err := repository.store.ListRecords(ctx, metastore.RecordKindStorageIndexDescriptorV1, repository.recordLimits(10))
	if err != nil || !listed.Complete || len(listed.Records) != 1 || listed.Records[0].ID != latest {
		t.Fatalf("partial prune did not converge to the latest generation: listing=%#v err=%v", listed, err)
	}
	if !testRecordPresent(t, repository, metastore.RecordKindStorageIndexDataV1, latestData) {
		t.Fatal("partial prune removed the latest snapshot data")
	}
	if selected, err := repository.SelectSnapshot(ctx, profile, ""); err != nil || selected.DescriptorRecordID != latest {
		t.Fatalf("latest selection did not recover after partial prunes: selection=%#v err=%v", selected, err)
	}
}

func TestPruneSnapshotsConvergesAfterCrashBetweenDataAndDescriptorRemoval(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	profile := testIndexProfile(t, repository, "media")
	first := importTestDescriptor(t, repository, profile, 1, "crash-1")
	second := importTestDescriptor(t, repository, profile, 2, "crash-2")
	latest := importTestDescriptor(t, repository, profile, 3, "crash-3")
	firstData := testDescriptorDataID(t, repository, first)
	// Simulate a prune that crashed after removing generation one's data.
	listed, err := repository.store.ListRecords(ctx, metastore.RecordKindStorageIndexDataV1, repository.recordLimits(10))
	if err != nil {
		t.Fatal(err)
	}
	removedData := false
	for _, ref := range listed.Records {
		if ref.ID == firstData {
			if _, err := repository.store.RemoveRecordExact(ctx, ref, repository.recordLimits(10)); err != nil {
				t.Fatal(err)
			}
			removedData = true
		}
	}
	if !removedData {
		t.Fatal("test data record was not listed")
	}
	if selected, err := repository.SelectSnapshot(ctx, profile, ""); err != nil || selected.DescriptorRecordID != latest {
		t.Fatalf("crash residue changed latest selection: selection=%#v err=%v", selected, err)
	}
	if _, err := repository.LoadCandidates(ctx, profile, first, []int64{1}, DefaultCandidateLimits()); err == nil {
		t.Fatal("explicit read of a descriptor without data did not fail closed")
	}
	result, err := repository.PruneSnapshots(ctx, profile, PruneOptions{RetainGenerations: 1})
	if err != nil || result.Status != "complete" || len(result.Pruned) != 2 ||
		result.Pruned[0].DescriptorRecordID != first || result.Pruned[0].DataRemoval != "already_absent" || result.Pruned[0].DescriptorRemoval != "removed" ||
		result.Pruned[1].DescriptorRecordID != second || result.Pruned[1].DataRemoval != "removed" {
		t.Fatalf("prune did not converge after a partial removal: result=%#v err=%v", result, err)
	}
	if testRecordPresent(t, repository, metastore.RecordKindStorageIndexDescriptorV1, first) || !testRecordPresent(t, repository, metastore.RecordKindStorageIndexDescriptorV1, latest) {
		t.Fatal("prune left the crash residue or removed the latest descriptor")
	}
}

func TestPruneSnapshotsCollectsOnlyAttributableSupersededOrphanData(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxSnapshots = 2
	repository := testRepositoryWithLimits(t, limits)
	ctx := context.Background()
	clock := deterministicClock(time.Now().UTC())
	profile := testIndexProfile(t, repository, "media")
	other := testIndexProfile(t, repository, "other")
	// lostRefresh publishes data at generation and then fails its pre-commit
	// check because a competitor committed that generation first.
	lostRefresh := func(target Profile, generation uint64, salt string) metastore.RecordID {
		t.Helper()
		lost, err := repository.Refresh(ctx, target, RefreshOptions{Clock: clock, afterDataPublication: func() {
			importTestDescriptor(t, repository, target, generation, salt)
		}})
		if !errors.Is(err, ErrSnapshotGenerationConflict) || lost.Generation != generation || lost.DataRecord.ID == "" {
			t.Fatalf("refresh did not lose generation %d: result=%#v err=%v", generation, lost, err)
		}
		return lost.DataRecord.ID
	}
	orphan := lostRefresh(profile, 1, "media-1")
	foreign := lostRefresh(other, 1, "other-1")
	if stored, err := repository.Refresh(ctx, profile, RefreshOptions{Clock: clock}); err != nil || stored.Generation != 2 {
		t.Fatalf("refresh was not stored: result=%#v err=%v", stored, err)
	}
	// Data at the latest generation is what an in-flight refresh looks like.
	inFlight := lostRefresh(profile, 3, "media-3")
	garbage, _, err := repository.store.ImportRecord(ctx, metastore.RecordKindStorageIndexDataV1, strings.NewReader("not a snapshot\n"), repository.recordLimits(repository.limits.MaxSnapshots))
	if err != nil {
		t.Fatal(err)
	}

	// Without a complete descriptor listing nothing is provably unreferenced.
	// Three of the four descriptors always include a media generation above 1.
	repository.maintenanceRecords = 3
	if partial, err := repository.PruneSnapshots(ctx, profile, PruneOptions{RetainGenerations: 1}); err != nil || partial.Status != "partial" ||
		!testRecordPresent(t, repository, metastore.RecordKindStorageIndexDataV1, orphan) {
		t.Fatalf("partial prune collected orphan data: result=%#v err=%v", partial, err)
	}
	repository.maintenanceRecords = 0

	result, err := repository.PruneSnapshots(ctx, profile, PruneOptions{RetainGenerations: 1})
	orphans := []PrunedSnapshot{}
	for _, entry := range result.Pruned {
		if entry.DescriptorRecordID == "" {
			orphans = append(orphans, entry)
		}
	}
	if err != nil || result.Status != "complete" || len(result.RetainedGenerations) != 1 || result.RetainedGenerations[0] != 3 || len(orphans) != 1 ||
		orphans[0].Generation != 1 || orphans[0].DataRecordID != orphan || orphans[0].DataRemoval != "removed" || orphans[0].DescriptorRemoval != "already_absent" {
		t.Fatalf("prune did not collect exactly the superseded orphan: result=%#v err=%v", result, err)
	}
	if testRecordPresent(t, repository, metastore.RecordKindStorageIndexDataV1, orphan) {
		t.Fatal("superseded orphan data survived prune")
	}
	for _, kept := range []metastore.RecordID{inFlight, foreign, garbage.ID} {
		if !testRecordPresent(t, repository, metastore.RecordKindStorageIndexDataV1, kept) {
			t.Fatalf("prune removed unattributable or current orphan data %s", kept)
		}
	}
	if again, err := repository.PruneSnapshots(ctx, profile, PruneOptions{RetainGenerations: 1}); err != nil || again.WritesPerformed != 0 || len(again.Pruned) != 0 {
		t.Fatalf("orphan collection rerun was not idempotent: result=%#v err=%v", again, err)
	}
}

func TestRefreshRetentionOptionsReuseCommittedListing(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	clock := deterministicClock(time.Now().UTC())
	profile := testIndexProfile(t, repository, "media")
	other := testIndexProfile(t, repository, "other")
	first, err := repository.Refresh(ctx, profile, RefreshOptions{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	second, err := repository.Refresh(ctx, profile, RefreshOptions{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	// Published after the commit check, so absent from the reused listing.
	importTestDescriptor(t, repository, other, 1, "other-1")
	importTestDescriptor(t, repository, other, 2, "other-2")
	result, err := repository.PruneSnapshots(ctx, profile, second.RetentionOptions(1))
	if err != nil || result.Status != "complete" || result.RecordsConsidered != 2 || len(result.Pruned) != 1 ||
		result.Pruned[0].DescriptorRecordID != first.DescriptorRecord.ID || !testRecordPresent(t, repository, metastore.RecordKindStorageIndexDescriptorV1, second.DescriptorRecord.ID) {
		t.Fatalf("retention did not reuse the committed listing: result=%#v err=%v", result, err)
	}
	if foreign, err := repository.PruneSnapshots(ctx, other, second.RetentionOptions(1)); err != nil || foreign.RecordsConsidered != 3 || len(foreign.Pruned) != 1 {
		t.Fatalf("another profile reused a refresh listing: result=%#v err=%v", foreign, err)
	}
}

func testRepositoryWithLimits(t *testing.T, limits Limits) *Repository {
	t.Helper()
	store, _, err := metastore.Init(filepath.Join(physicalIndexTempDir(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store, limits)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func testIndexProfile(t *testing.T, repository *Repository, name string) Profile {
	t.Helper()
	receipt, err := repository.CreateProfile(context.Background(), name, []string{physicalIndexTempDir(t)}, false, DefaultScanLimits(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return receipt.Profile
}

func testDescriptorDataID(t *testing.T, repository *Repository, descriptor metastore.RecordID) metastore.RecordID {
	t.Helper()
	loaded, err := repository.loadDescriptor(context.Background(), metastore.RecordRef{Kind: metastore.RecordKindStorageIndexDescriptorV1, ID: descriptor})
	if err != nil {
		t.Fatal(err)
	}
	id, err := metastore.ParseRecordID(loaded.descriptor.DataRecordID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func testRecordPresent(t *testing.T, repository *Repository, kind metastore.RecordKind, id metastore.RecordID) bool {
	t.Helper()
	_, _, err := repository.store.LoadRecord(context.Background(), kind, id, repository.recordLimits(10), func(reader io.Reader) error {
		_, copyErr := io.Copy(io.Discard, reader)
		return copyErr
	})
	if errors.Is(err, metastore.ErrRecordNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("load %s record %s: %v", kind, id, err)
	}
	return true
}
