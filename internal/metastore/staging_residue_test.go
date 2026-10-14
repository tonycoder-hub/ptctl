package metastore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func placeStagingTestFile(t *testing.T, path string, stale bool) string {
	t.Helper()
	createPrivateTestFile(t, path, []byte("residue"))
	if stale {
		old := time.Now().Add(-2 * stagingResidueMinimumAge)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestStandaloneWritesCollectOnlyStaleOwnStagingResidue(t *testing.T) {
	store := newTestStore(t)
	staging := filepath.Join(store.root, temporaryDir)
	random := strings.Repeat("0123456789abcdef", 2)
	pending := filepath.Base(recordRemovalStagingPath(RecordRef{Kind: RecordKindStorageProfileV1, ID: expectedRecordID(RecordKindStorageProfileV1, []byte("pending"))}))
	collected := []string{
		placeStagingTestFile(t, filepath.Join(staging, ".import-"+random+".torrent"), true),
		placeStagingTestFile(t, filepath.Join(staging, ".init-"+random+".marker"), true),
		placeStagingTestFile(t, filepath.Join(staging, ".record-import-"+random+recordSuffix), true),
	}
	kept := []string{
		placeStagingTestFile(t, filepath.Join(staging, ".import-"+strings.Repeat("f", 32)+".torrent"), false),
		placeStagingTestFile(t, filepath.Join(staging, pending), true),
		placeStagingTestFile(t, filepath.Join(staging, "OPAQUE-STAGING-CANARY"), true),
		placeStagingTestFile(t, filepath.Join(staging, ".import-"+strings.ToUpper(random)+".torrent"), true),
		placeStagingTestFile(t, filepath.Join(staging, ".import-"+random[:31]+".torrent"), true),
		placeStagingTestFile(t, filepath.Join(staging, ".import-"+random+".torrent.bak"), true),
	}
	directory := filepath.Join(staging, ".record-import-"+strings.Repeat("e", 32)+recordSuffix)
	if err := platformCreatePrivateDirectory(directory); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * stagingResidueMinimumAge)
	if err := os.Chtimes(directory, old, old); err != nil {
		t.Fatal(err)
	}
	kept = append(kept, directory)
	if runtime.GOOS != "windows" {
		shared := filepath.Join(staging, ".record-import-"+strings.Repeat("d", 32)+recordSuffix)
		if err := os.WriteFile(shared, []byte("shared"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(shared, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(shared, old, old); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, shared)
	}

	raw := testMetafile("https://tracker.invalid/announce", "volume.bin", []byte("volume"))
	if _, _, receipt, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits()); err != nil || receipt.WritesPerformed != 1 {
		t.Fatalf("Import receipt=%+v err=%v", receipt, err)
	}
	for _, path := range collected {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("stale staging residue %s was not collected: %v", filepath.Base(path), err)
		}
	}
	for _, path := range kept {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("staging entry %s must be kept: %v", filepath.Base(path), err)
		}
	}

	// Collection runs once per Store handle, not on every write.
	again := placeStagingTestFile(t, filepath.Join(staging, ".record-import-"+strings.Repeat("c", 32)+recordSuffix), true)
	if _, receipt, err := store.ImportRecord(context.Background(), RecordKindStorageProfileV1, bytes.NewReader([]byte("profile record")), DefaultRecordLimits()); err != nil || receipt.WritesPerformed != 1 {
		t.Fatalf("ImportRecord receipt=%+v err=%v", receipt, err)
	}
	if _, err := os.Lstat(again); err != nil {
		t.Fatalf("the same Store handle collected staging residue twice: %v", err)
	}
	reopened, err := Open(store.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, receipt, err := reopened.ImportRecord(context.Background(), RecordKindStorageProfileV1, bytes.NewReader([]byte("another record")), DefaultRecordLimits()); err != nil || receipt.WritesPerformed != 1 {
		t.Fatalf("ImportRecord receipt=%+v err=%v", receipt, err)
	}
	if _, err := os.Lstat(again); !os.IsNotExist(err) {
		t.Fatalf("record import did not collect stale staging residue: %v", err)
	}
	for _, path := range kept {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("staging entry %s must be kept: %v", filepath.Base(path), err)
		}
	}
}

func TestStagingResidueCollectionIsBoundedAndCancellable(t *testing.T) {
	store := newTestStore(t)
	staging := filepath.Join(store.root, temporaryDir)
	const extra = 4
	for index := 0; index < stagingResidueRemoveLimit+extra; index++ {
		random := strings.Repeat("0", 30) + string("0123456789abcdef"[index/16]) + string("0123456789abcdef"[index%16])
		placeStagingTestFile(t, filepath.Join(staging, ".import-"+random+".torrent"), true)
	}
	remaining := func() int {
		t.Helper()
		entries, err := os.ReadDir(staging)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	session, err := store.validatedSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	collectStagingResidue(context.Background(), session)
	if got := remaining(); got != extra {
		t.Fatalf("one collection pass left %d entries, want %d", got, extra)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	collectStagingResidue(cancelled, session)
	if got := remaining(); got != extra {
		t.Fatalf("a cancelled collection removed entries: %d left", got)
	}
	collectStagingResidue(context.Background(), session)
	if got := remaining(); got != 0 {
		t.Fatalf("second collection pass left %d entries", got)
	}
}
