package storageindex

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
)

func TestRefreshPublishesDataThenDescriptorAndAdvancesGeneration(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	root := physicalIndexTempDir(t)
	writeTestFile(t, filepath.Join(root, "b.bin"), []byte("b"))
	writeTestFile(t, filepath.Join(root, "a", "x.bin"), []byte("alpha"))
	profileReceipt, err := repository.CreateProfile(ctx, "media", []string{root}, false, DefaultScanLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	next := deterministicClock(time.Date(2026, 8, 10, 1, 0, 0, 0, time.UTC))
	first, err := repository.Refresh(ctx, profileReceipt.Profile, RefreshOptions{Clock: next})
	if err != nil || first.Status != "stored" || first.WritesPerformed != 2 || first.Generation != 1 || !first.Scan.Complete || first.DataRecord.ID == "" || first.DescriptorRecord.ID == "" {
		t.Fatalf("first refresh failed: result=%#v err=%v", first, err)
	}
	selected, err := repository.SelectSnapshot(ctx, profileReceipt.Profile, "")
	if err != nil || selected.DescriptorRecordID != first.DescriptorRecord.ID || selected.Descriptor.DataRecordID != first.DataRecord.ID.String() {
		t.Fatalf("published descriptor was not selected: selection=%#v err=%v", selected, err)
	}
	dataID, err := metastore.ParseRecordID(selected.Descriptor.DataRecordID)
	if err != nil {
		t.Fatal(err)
	}
	var files int
	dataRef, receipt, err := repository.store.LoadRecord(ctx, metastore.RecordKindStorageIndexDataV1, dataID, repository.recordLimits(repository.limits.MaxSnapshots), func(reader io.Reader) error {
		header, footer, decodeErr := DecodeSnapshot(ctx, reader, repository.limits, func(Entry) error {
			files++
			return nil
		})
		if decodeErr != nil {
			return decodeErr
		}
		if header.SnapshotID != selected.Descriptor.ID || footer.Files != selected.Descriptor.Files || footer.PathBytes != selected.Descriptor.PathBytes {
			return errors.New("descriptor does not bind the data stream")
		}
		return nil
	})
	if err != nil || !receipt.Complete || dataRef.ID != first.DataRecord.ID || files != 2 {
		t.Fatalf("sealed snapshot data did not round trip: ref=%#v receipt=%#v files=%d err=%v", dataRef, receipt, files, err)
	}

	second, err := repository.Refresh(ctx, profileReceipt.Profile, RefreshOptions{Clock: next})
	if err != nil || second.Status != "stored" || second.Generation != 2 {
		t.Fatalf("second refresh did not advance generation: result=%#v err=%v", second, err)
	}
}

func TestRefreshIncompleteInventoryPublishesNoDiscoverableSnapshot(t *testing.T) {
	repository := testRepository(t)
	ctx := context.Background()
	root := physicalIndexTempDir(t)
	writeTestFile(t, filepath.Join(root, "a"), []byte("a"))
	writeTestFile(t, filepath.Join(root, "b"), []byte("b"))
	scanLimits := DefaultScanLimits()
	scanLimits.MaxFiles = 1
	profileReceipt, err := repository.CreateProfile(ctx, "limited", []string{root}, false, scanLimits, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	result, err := repository.Refresh(ctx, profileReceipt.Profile, RefreshOptions{Clock: deterministicClock(time.Now().UTC())})
	if err != nil || result.Status != "incomplete" || result.WritesPerformed != 0 || result.Scan.Complete || !containsString(result.StopReasons, "inventory_incomplete") {
		t.Fatalf("incomplete scan publication contract failed: result=%#v err=%v", result, err)
	}
	selection, err := repository.SelectSnapshot(ctx, profileReceipt.Profile, "")
	if !errors.Is(err, ErrSnapshotNotFound) || !selection.Complete {
		t.Fatalf("incomplete inventory became discoverable: selection=%#v err=%v", selection, err)
	}
	data, err := repository.store.ListRecords(ctx, metastore.RecordKindStorageIndexDataV1, repository.recordLimits(repository.limits.MaxSnapshots))
	if err != nil || !data.Complete || len(data.Records) != 0 {
		t.Fatalf("incomplete data stream left a sealed record: list=%#v err=%v", data, err)
	}
}

func TestLiveOperationsRejectForeignPlatformProfileBeforeStateOrFilesystemRead(t *testing.T) {
	repository := testRepository(t)
	profile, err := NewProfile("foreign", []string{filepath.Join(physicalIndexTempDir(t), "must-not-be-read")}, false, DefaultScanLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	profile = foreignPlatformProfile(t, profile)

	refresh, err := repository.Refresh(context.Background(), profile, RefreshOptions{})
	if !errors.Is(err, ErrProfilePlatformMismatch) || refresh.WritesPerformed != 0 || refresh.Scan.Stats.EntriesExamined != 0 {
		t.Fatalf("foreign refresh crossed the live-platform gate: result=%#v err=%v", refresh, err)
	}
	query, err := repository.LoadCandidates(context.Background(), profile, "", []int64{1}, DefaultCandidateLimits())
	if !errors.Is(err, ErrProfilePlatformMismatch) || query.Stats.SnapshotFilesConsidered != 0 || len(query.Candidates) != 0 {
		t.Fatalf("foreign query crossed the live-platform gate: result=%#v err=%v", query, err)
	}

	local, err := NewProfile("relative", []string{physicalIndexTempDir(t)}, false, DefaultScanLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	local.Roots[0].PathRawBase64 = base64.StdEncoding.EncodeToString([]byte("relative-root"))
	local = rebindProfileDeclaration(t, local, runtime.GOOS)
	if _, err := repository.Refresh(context.Background(), local, RefreshOptions{}); !errors.Is(err, ErrProfileLivePathInvalid) {
		t.Fatalf("relative stored path crossed the live-path gate: %v", err)
	}

	tooLarge := DefaultScanLimits()
	tooLarge.MaxFiles = DefaultLimits().MaxFiles + 1
	if err := ValidateScanLimitsForIndex(tooLarge, DefaultLimits()); !errors.Is(err, ErrProfileExceedsIndexLimits) {
		t.Fatalf("incompatible scan/index limits were accepted: %v", err)
	}
}

func TestRefreshRevalidatesDataAndDescriptorAfterPublication(t *testing.T) {
	stateParent := physicalIndexTempDir(t)
	stateRoot := filepath.Join(stateParent, "state")
	store, _, err := metastore.Init(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	contentRoot := physicalIndexTempDir(t)
	writeTestFile(t, filepath.Join(contentRoot, "file.bin"), []byte("payload"))
	profileReceipt, err := repository.CreateProfile(context.Background(), "media", []string{contentRoot}, false, DefaultScanLimits(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(filepath.Join(stateRoot, ".ptctl-metastore"))
	if err != nil {
		t.Fatal(err)
	}
	displaced := filepath.Join(stateParent, "displaced-state")
	result, err := repository.Refresh(context.Background(), profileReceipt.Profile, RefreshOptions{
		Clock: deterministicClock(time.Now().UTC()),
		afterDataPublication: func() {
			if renameErr := os.Rename(stateRoot, displaced); renameErr != nil {
				t.Fatalf("replace store root: %v", renameErr)
			}
			if _, _, initErr := metastore.Init(stateRoot); initErr != nil {
				t.Fatalf("initialize replacement store: %v", initErr)
			}
			if writeErr := os.WriteFile(filepath.Join(stateRoot, ".ptctl-metastore"), marker, 0o600); writeErr != nil {
				t.Fatalf("copy store identity marker: %v", writeErr)
			}
		},
	})
	if err == nil || result.Status == "stored" || result.WritesPerformed != 2 || !containsString(result.StopReasons, "post_publish_revalidation_failed") {
		t.Fatalf("split-root publication was reported stored: result=%#v err=%v", result, err)
	}
}

func deterministicClock(start time.Time) func() time.Time {
	current := start.UTC()
	return func() time.Time {
		result := current
		current = current.Add(time.Second)
		return result
	}
}

func writeTestFile(t *testing.T, path string, value []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, value, 0o600); err != nil {
		t.Fatal(err)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func foreignPlatformProfile(t *testing.T, profile Profile) Profile {
	t.Helper()
	platform := "linux"
	if runtime.GOOS == platform {
		platform = "windows"
	}
	return rebindProfileDeclaration(t, profile, platform)
}

func rebindProfileDeclaration(t *testing.T, profile Profile, platform string) Profile {
	t.Helper()
	paths := make([]string, len(profile.Roots))
	for index := range profile.Roots {
		paths[index] = profile.Roots[index].PathRawBase64
	}
	sort.Strings(paths)
	profile.Platform = platform
	profile.ID, profile.Revision = profileDeclarationIDs(platform, profile.AllowNetwork, profile.ScanLimits, paths)
	for index := range profile.Roots {
		digest := sha256.Sum256([]byte("ptctl-storage-profile-root-v1\x00" + profile.ID + "\x00" + profile.Roots[index].PathRawBase64))
		profile.Roots[index].ID = "root:" + hex.EncodeToString(digest[:])
	}
	sort.Slice(profile.Roots, func(i, j int) bool { return profile.Roots[i].ID < profile.Roots[j].ID })
	if err := profile.Validate(); err != nil {
		t.Fatalf("foreign profile fixture is invalid: %v", err)
	}
	return profile
}
