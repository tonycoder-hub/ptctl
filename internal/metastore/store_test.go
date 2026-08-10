package metastore

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestInitCreatesAndReopensPrivateStore(t *testing.T) {
	root := filepath.Join(physicalTempDir(t), "store")
	store, receipt, err := Init(root)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if store == nil || receipt.WritesPerformed != 1 || receipt.AlreadyInitialized || receipt.Store != store.Info() {
		t.Fatalf("unexpected initial receipt: store=%v receipt=%+v", store, receipt)
	}
	if info := store.Info(); info.StoreID == "" || info.Format != storeFormat || info.Privacy != privacyAssurance || info.CommitAssurance == "" {
		t.Fatalf("unexpected store info: %+v", info)
	}
	reopened, second, err := Init(root)
	if err != nil {
		t.Fatalf("idempotent Init failed: %v", err)
	}
	if reopened.Info() != store.Info() || second.WritesPerformed != 0 || !second.AlreadyInitialized || second.Store != store.Info() {
		t.Fatalf("unexpected idempotent receipt: store=%v receipt=%+v", reopened, second)
	}
	opened, err := Open(root)
	if err != nil || opened.Info() != store.Info() {
		t.Fatalf("Open returned store=%v err=%v", opened, err)
	}

	encoded, err := json.Marshal(struct {
		Store   *Store      `json:"store"`
		Receipt InitReceipt `json:"receipt"`
	}{store, receipt})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(root)) || strings.Contains(fmt.Sprintf("%#v", store), root) {
		t.Fatalf("store path leaked through public values")
	}
}

func TestConcurrentInitPublishesExactlyOneMarker(t *testing.T) {
	root := filepath.Join(physicalTempDir(t), "store")
	const workers = 6
	type result struct {
		store   *Store
		receipt InitReceipt
		err     error
	}
	results := make([]result, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for index := range results {
		go func() {
			defer wait.Done()
			results[index].store, results[index].receipt, results[index].err = Init(root)
		}()
	}
	wait.Wait()
	writes := 0
	already := 0
	var wanted StoreInfo
	for index, result := range results {
		if result.err != nil || result.store == nil {
			t.Fatalf("worker %d: store=%v err=%v", index, result.store, result.err)
		}
		if wanted.StoreID == "" {
			wanted = result.store.Info()
		} else if result.store.Info() != wanted {
			t.Fatalf("worker stores differ: %+v != %+v", result.store.Info(), wanted)
		}
		writes += result.receipt.WritesPerformed
		if result.receipt.AlreadyInitialized {
			already++
		}
	}
	if writes != 1 || already != workers-1 {
		t.Fatalf("writes=%d already=%d; wanted 1/%d", writes, already, workers-1)
	}
	opened, receipt, err := Init(root)
	if err != nil || opened == nil || opened.Info() != wanted || !receipt.AlreadyInitialized || receipt.WritesPerformed != 0 {
		t.Fatalf("final idempotent Init returned store=%v receipt=%+v err=%v", opened, receipt, err)
	}
}

func TestInitPreflightsExistingDirectoryWithoutAdditionalWrites(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{"unknown_entry", func(t *testing.T, root string) {
			createPrivateTestFile(t, filepath.Join(root, "UNKNOWN-ENTRY-CANARY"), []byte("unknown"))
		}},
		{"invalid_marker", func(t *testing.T, root string) {
			createPrivateTestFile(t, filepath.Join(root, markerName), []byte("invalid marker"))
		}},
		{"nonempty_objects", func(t *testing.T, root string) {
			if err := platformCreatePrivateDirectory(filepath.Join(root, objectsDir)); err != nil {
				t.Fatal(err)
			}
			createPrivateTestFile(t, filepath.Join(root, objectsDir, "unexpected.torrent"), []byte("unknown"))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(physicalTempDir(t), "existing")
			if err := platformCreatePrivateDirectory(root); err != nil {
				t.Fatal(err)
			}
			test.setup(t, root)
			before := testDirectoryTree(t, root)
			store, receipt, err := Init(root)
			if err == nil || store != nil || receipt.WritesPerformed != 0 {
				t.Fatalf("Init returned store=%v receipt=%+v err=%v", store, receipt, err)
			}
			after := testDirectoryTree(t, root)
			if strings.Join(after, "\n") != strings.Join(before, "\n") {
				t.Fatalf("Init changed rejected directory: before=%v after=%v", before, after)
			}
			if err := os.RemoveAll(root); err != nil {
				t.Fatalf("rejected Init retained a store handle: %v", err)
			}
		})
	}
}

func TestInitPreservesOpaquePreexistingStagingContent(t *testing.T) {
	root := filepath.Join(physicalTempDir(t), "existing")
	if err := platformCreatePrivateDirectory(root); err != nil {
		t.Fatal(err)
	}
	if err := platformCreatePrivateDirectory(filepath.Join(root, temporaryDir)); err != nil {
		t.Fatal(err)
	}
	stagingPath := filepath.Join(root, temporaryDir, "OPAQUE-STAGING-CANARY")
	wanted := []byte("opaque staging content")
	createPrivateTestFile(t, stagingPath, wanted)
	store, receipt, err := Init(root)
	if err != nil || store == nil || receipt.WritesPerformed != 1 {
		t.Fatalf("Init returned store=%v receipt=%+v err=%v", store, receipt, err)
	}
	got, err := os.ReadFile(stagingPath)
	if err != nil || !bytes.Equal(got, wanted) {
		t.Fatalf("preexisting staging content changed: got=%q err=%v", got, err)
	}
}

func TestDirectoryPrefixReadIsBounded(t *testing.T) {
	directory := physicalTempDir(t)
	for index := 0; index < 24; index++ {
		if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("entry-%02d", index)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	file, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	entries, overflow, err := readDirectoryPrefix(file, 3)
	closeErr := file.Close()
	if err != nil || closeErr != nil || !overflow || len(entries) != 3 {
		t.Fatalf("entries=%d overflow=%t err=%v close=%v", len(entries), overflow, err, closeErr)
	}
}

func TestInitLayoutDurabilityFailureDoesNotPublishMarker(t *testing.T) {
	root := filepath.Join(physicalTempDir(t), "store")
	original := confirmPrivateStoreLayout
	confirmPrivateStoreLayout = func(*rootSession) error { return errors.New("injected durability failure") }
	t.Cleanup(func() { confirmPrivateStoreLayout = original })
	store, receipt, err := Init(root)
	if err == nil || store != nil || receipt.WritesPerformed != 0 || receipt.Store.StoreID != "" {
		t.Fatalf("Init returned store=%v receipt=%+v err=%v", store, receipt, err)
	}
	if _, statErr := os.Lstat(filepath.Join(root, markerName)); !os.IsNotExist(statErr) {
		t.Fatalf("marker was published despite durability failure: %v", statErr)
	}
}

func TestImportPreservesExactVariantsAndLoadsStrictly(t *testing.T) {
	store := newTestStore(t)
	content := []byte("same payload")
	firstRaw := testMetafile("https://tracker.invalid/announce?passkey=one", "payload.bin", content)
	secondRaw := testMetafile("https://tracker.invalid/announce?passkey=two", "payload.bin", content)

	firstMeta, firstRef, firstReceipt, err := store.Import(context.Background(), bytes.NewReader(firstRaw), DefaultLimits())
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	secondMeta, secondRef, secondReceipt, err := store.Import(context.Background(), bytes.NewReader(secondRaw), DefaultLimits())
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if firstReceipt.WritesPerformed != 1 || firstReceipt.AlreadyPresent || secondReceipt.WritesPerformed != 1 || secondReceipt.AlreadyPresent {
		t.Fatalf("unexpected import receipts: %+v %+v", firstReceipt, secondReceipt)
	}
	if firstRef.ID == secondRef.ID || firstMeta.MetafileVariantID == secondMeta.MetafileVariantID || firstMeta.InfoHashV1 != secondMeta.InfoHashV1 {
		t.Fatalf("whole-metafile variants were not separated: first=%+v second=%+v", firstRef, secondRef)
	}
	for _, item := range []struct {
		ref ArtifactRef
		raw []byte
	}{
		{firstRef, firstRaw},
		{secondRef, secondRaw},
	} {
		stored, err := os.ReadFile(filepath.Join(store.root, objectRelativePath(item.ref.ID)))
		if err != nil || !bytes.Equal(stored, item.raw) {
			t.Fatalf("stored bytes do not exactly match import: err=%v", err)
		}
		loaded, loadedRef, err := store.Load(context.Background(), item.ref.ID, DefaultLimits())
		if err != nil || loaded.MetafileVariantID != item.ref.ID.String() || loadedRef != item.ref {
			t.Fatalf("Load returned meta=%v ref=%+v err=%v", loaded, loadedRef, err)
		}
	}

	repeatedMeta, repeatedRef, repeated, err := store.Import(context.Background(), bytes.NewReader(firstRaw), DefaultLimits())
	if err != nil || repeated.WritesPerformed != 0 || !repeated.AlreadyPresent || repeatedRef != firstRef || repeatedMeta.MetafileVariantID != firstMeta.MetafileVariantID {
		t.Fatalf("idempotent import returned meta=%v ref=%+v receipt=%+v err=%v", repeatedMeta, repeatedRef, repeated, err)
	}
}

func TestImportFileUsesAStableSourceSnapshot(t *testing.T) {
	store := newTestStore(t)
	raw := testMetafile("https://tracker.invalid/announce", "source.bin", []byte("source content"))
	source := filepath.Join(physicalTempDir(t), "source.torrent")
	if err := os.WriteFile(source, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	meta, ref, receipt, err := store.ImportFile(context.Background(), source, DefaultLimits())
	if err != nil || meta == nil || ref.ID == "" || receipt.WritesPerformed != 1 || receipt.BytesConsumed != int64(len(raw)) {
		t.Fatalf("ImportFile returned meta=%v ref=%+v receipt=%+v err=%v", meta, ref, receipt, err)
	}
	stored, err := os.ReadFile(filepath.Join(store.root, objectRelativePath(ref.ID)))
	if err != nil || !bytes.Equal(stored, raw) {
		t.Fatalf("ImportFile did not preserve exact bytes: %v", err)
	}
}

func TestConcurrentImportPublishesExactlyOneObject(t *testing.T) {
	store := newTestStore(t)
	raw := testMetafile("https://tracker.invalid/announce", "concurrent.bin", []byte("content"))
	const workers = 8
	type result struct {
		ref     ArtifactRef
		receipt ImportReceipt
		err     error
	}
	results := make([]result, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for index := range results {
		go func() {
			defer wait.Done()
			_, results[index].ref, results[index].receipt, results[index].err = store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
		}()
	}
	wait.Wait()
	writes := 0
	already := 0
	var wanted ArtifactID
	for index, result := range results {
		if result.err != nil {
			t.Fatalf("worker %d: %v", index, result.err)
		}
		if wanted == "" {
			wanted = result.ref.ID
		} else if result.ref.ID != wanted {
			t.Fatalf("worker IDs differ: %s != %s", result.ref.ID, wanted)
		}
		writes += result.receipt.WritesPerformed
		if result.receipt.AlreadyPresent {
			already++
		}
	}
	if writes != 1 || already != workers-1 {
		t.Fatalf("writes=%d already=%d; wanted 1/%d", writes, already, workers-1)
	}
	entries, err := os.ReadDir(filepath.Join(store.root, objectsDir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("objects=%d err=%v", len(entries), err)
	}
}

func TestInvalidAndCorruptArtifactsAreClassifiedAndNeverOverwritten(t *testing.T) {
	store := newTestStore(t)
	invalid := []byte("PRIVATE-RAW-CANARY")
	meta, ref, receipt, err := store.Import(context.Background(), bytes.NewReader(invalid), DefaultLimits())
	if !errors.Is(err, ErrInvalidMetafile) || meta != nil || ref.ID != "" || receipt.WritesPerformed != 0 || strings.Contains(err.Error(), string(invalid)) {
		t.Fatalf("invalid import returned meta=%v ref=%+v receipt=%+v err=%v", meta, ref, receipt, err)
	}
	entries, readErr := os.ReadDir(filepath.Join(store.root, objectsDir))
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("invalid import published objects=%d err=%v", len(entries), readErr)
	}

	raw := testMetafile("https://tracker.invalid/announce?passkey=PRIVATE-PASSKEY-CANARY", "private-name.bin", []byte("valid"))
	_, validRef, _, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(store.root, objectRelativePath(validRef.ID))
	corrupt := []byte("CORRUPT-OBJECT-CANARY")
	if err := os.WriteFile(objectPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(context.Background(), validRef.ID, DefaultLimits()); !errors.Is(err, ErrCorruptArtifact) || strings.Contains(err.Error(), string(corrupt)) || strings.Contains(err.Error(), validRef.ID.String()) {
		t.Fatalf("corrupt Load error=%v", err)
	}
	before, err := os.ReadFile(objectPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _, corruptReceipt, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
	if !errors.Is(err, ErrCorruptArtifact) || corruptReceipt.WritesPerformed != 0 {
		t.Fatalf("import over corrupt object returned receipt=%+v err=%v", corruptReceipt, err)
	}
	after, err := os.ReadFile(objectPath)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("corrupt object was changed: err=%v", err)
	}
}

func TestOpenRejectsUnknownOrTamperedMarkerWithoutEcho(t *testing.T) {
	store := newTestStore(t)
	const canary = "PRIVATE-MARKER-CANARY"
	if err := os.WriteFile(filepath.Join(store.root, markerName), []byte(canary), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(store.root)
	if err == nil || opened != nil || strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), store.root) {
		t.Fatalf("Open returned store=%v err=%v", opened, err)
	}
}

func TestImportLimitsAndCancellation(t *testing.T) {
	store := newTestStore(t)
	raw := testMetafile("https://tracker.invalid/announce", "limited.bin", []byte("content"))
	limits := Limits{MaxArtifactBytes: int64(len(raw) - 1)}
	_, _, receipt, err := store.Import(context.Background(), bytes.NewReader(raw), limits)
	if !errors.Is(err, ErrInvalidMetafile) || receipt.WritesPerformed != 0 || receipt.BytesConsumed <= limits.MaxArtifactBytes {
		t.Fatalf("bounded import receipt=%+v err=%v", receipt, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, canceled, err := store.Import(ctx, bytes.NewReader(raw), DefaultLimits())
	if !errors.Is(err, context.Canceled) || canceled.BytesConsumed != 0 || canceled.WritesPerformed != 0 {
		t.Fatalf("pre-canceled import receipt=%+v err=%v", canceled, err)
	}
	staging, err := os.ReadDir(filepath.Join(store.root, temporaryDir))
	if err != nil || len(staging) != 0 {
		t.Fatalf("staging after rejected imports=%d err=%v", len(staging), err)
	}
}

func TestLimitsValidation(t *testing.T) {
	if limits := DefaultLimits(); limits.MaxArtifactBytes != hardMaxArtifactBytes || limits.Validate() != nil {
		t.Fatalf("unexpected defaults: %+v", limits)
	}
	for _, value := range []int64{-1, 0, hardMaxArtifactBytes + 1} {
		if err := (Limits{MaxArtifactBytes: value}).Validate(); err == nil {
			t.Fatalf("MaxArtifactBytes=%d was accepted", value)
		}
	}
}

func TestImportFileRejectsSourceSymlink(t *testing.T) {
	store := newTestStore(t)
	raw := testMetafile("https://tracker.invalid/announce", "linked.bin", []byte("content"))
	sourceDir := physicalTempDir(t)
	target := filepath.Join(sourceDir, "target.torrent")
	link := filepath.Join(sourceDir, "SOURCE-PATH-CANARY.torrent")
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	_, _, linked, err := store.ImportFile(context.Background(), link, DefaultLimits())
	if err == nil || linked.WritesPerformed != 0 || strings.Contains(err.Error(), link) || strings.Contains(err.Error(), "SOURCE-PATH-CANARY") {
		t.Fatalf("source symlink receipt=%+v err=%v", linked, err)
	}
}

func TestParseArtifactIDAndPublicErrorsDoNotEchoPrivateInputs(t *testing.T) {
	valid := "sha256:" + strings.Repeat("a", 64)
	if parsed, err := ParseArtifactID(valid); err != nil || parsed.String() != valid {
		t.Fatalf("valid ID parsed=%q err=%v", parsed, err)
	}
	for _, invalid := range []string{
		"", "PRIVATE-ID-CANARY", "sha256:" + strings.Repeat("A", 64),
		"sha256:" + strings.Repeat("a", 63), "sha256:" + strings.Repeat("g", 64),
	} {
		if _, err := ParseArtifactID(invalid); err == nil || strings.Contains(err.Error(), invalid) && invalid != "" {
			t.Fatalf("invalid ID %q returned %v", invalid, err)
		}
	}
	root := filepath.Join(physicalTempDir(t), "PRIVATE-ROOT-PATH-CANARY")
	if _, err := Open(root); err == nil || strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "PRIVATE-ROOT-PATH-CANARY") {
		t.Fatalf("Open error leaked root: %v", err)
	}
}

func TestPublishedPostCommitFailuresRetainEvidenceAndReceipt(t *testing.T) {
	tests := []struct {
		name     string
		sentinel error
	}{
		{"durability", ErrDurabilityUnconfirmed},
		{"cleanup", ErrPublishedCleanupIncomplete},
	}
	for _, test := range tests {
		t.Run("init_"+test.name, func(t *testing.T) {
			root := filepath.Join(physicalTempDir(t), "store")
			withPublishError(t, test.sentinel)
			store, receipt, err := Init(root)
			if !errors.Is(err, test.sentinel) || store == nil || receipt.WritesPerformed != 1 || receipt.Store != store.Info() {
				t.Fatalf("Init returned store=%v receipt=%+v err=%v", store, receipt, err)
			}
			if reopened, openErr := Open(root); openErr != nil || reopened.Info() != store.Info() {
				t.Fatalf("published store did not reopen: store=%v err=%v", reopened, openErr)
			}
		})
		t.Run("import_"+test.name, func(t *testing.T) {
			store := newTestStore(t)
			raw := testMetafile("https://tracker.invalid/announce", "published.bin", []byte(test.name))
			withPublishError(t, test.sentinel)
			meta, ref, receipt, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
			if !errors.Is(err, test.sentinel) || meta == nil || ref.ID == "" || receipt.WritesPerformed != 1 || receipt.Store != store.Info() {
				t.Fatalf("Import returned meta=%v ref=%+v receipt=%+v err=%v", meta, ref, receipt, err)
			}
			if loaded, loadedRef, loadErr := store.Load(context.Background(), ref.ID, DefaultLimits()); loadErr != nil || loaded.MetafileVariantID != ref.ID.String() || loadedRef != ref {
				t.Fatalf("published object did not load: meta=%v ref=%+v err=%v", loaded, loadedRef, loadErr)
			}
		})
	}
}

func TestPublishedPlatformErrorSurvivesAfterPublishIdentityFailure(t *testing.T) {
	tests := []struct {
		name     string
		sentinel error
	}{
		{"durability", ErrDurabilityUnconfirmed},
		{"cleanup", ErrPublishedCleanupIncomplete},
	}
	for _, test := range tests {
		t.Run("init_"+test.name, func(t *testing.T) {
			root := filepath.Join(physicalTempDir(t), "store")
			withPublishError(t, test.sentinel)
			withIdentityFailure(t, "after_publish")
			store, receipt, err := Init(root)
			if !errors.Is(err, test.sentinel) || store == nil || receipt.WritesPerformed != 1 || receipt.Store != store.Info() {
				t.Fatalf("Init returned store=%v receipt=%+v err=%v", store, receipt, err)
			}
		})

		t.Run("import_"+test.name, func(t *testing.T) {
			store := newTestStore(t)
			raw := testMetafile("https://tracker.invalid/announce", "combined.bin", []byte(test.name))
			withPublishError(t, test.sentinel)
			withIdentityFailure(t, "after_publish")
			meta, ref, receipt, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
			if !errors.Is(err, test.sentinel) || meta == nil || ref.ID == "" || receipt.WritesPerformed != 1 || receipt.Store != store.Info() {
				t.Fatalf("Import returned meta=%v ref=%+v receipt=%+v err=%v", meta, ref, receipt, err)
			}
		})
	}
}

func TestPublishedRevalidationFailureRetainsAttemptedIdentity(t *testing.T) {
	store := newTestStore(t)
	raw := testMetafile("https://tracker.invalid/announce", "post-commit.bin", []byte("content"))
	original := publishNoReplace
	publishNoReplace = func(session *rootSession, temporaryRelative, finalRelative string) (bool, error) {
		published, err := original(session, temporaryRelative, finalRelative)
		if published && err == nil {
			if writeErr := os.WriteFile(filepath.Join(session.path, finalRelative), []byte("post-commit tamper"), 0o600); writeErr != nil {
				return true, writeErr
			}
		}
		return published, err
	}
	t.Cleanup(func() { publishNoReplace = original })
	meta, ref, receipt, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
	if !errors.Is(err, ErrCorruptArtifact) || meta == nil || ref.ID == "" || receipt.WritesPerformed != 1 || receipt.Store != store.Info() {
		t.Fatalf("Import returned meta=%v ref=%+v receipt=%+v err=%v", meta, ref, receipt, err)
	}
}

func TestBoundIdentitySeamFailsClosedAtOperationBoundaries(t *testing.T) {
	t.Run("init_before_directory_create", func(t *testing.T) {
		root := filepath.Join(physicalTempDir(t), "store")
		if err := platformCreatePrivateDirectory(root); err != nil {
			t.Fatal(err)
		}
		withIdentityFailure(t, "before_directory_create")
		store, receipt, err := Init(root)
		if err == nil || store != nil || receipt.WritesPerformed != 0 {
			t.Fatalf("Init returned store=%v receipt=%+v err=%v", store, receipt, err)
		}
		if entries, readErr := os.ReadDir(root); readErr != nil || len(entries) != 0 {
			t.Fatalf("failed identity check mutated root: entries=%v err=%v", entries, readErr)
		}
		if err := os.RemoveAll(root); err != nil {
			t.Fatalf("failed Init retained a store handle: %v", err)
		}
	})

	t.Run("init_after_publish", func(t *testing.T) {
		root := filepath.Join(physicalTempDir(t), "store")
		withIdentityFailure(t, "after_publish")
		store, receipt, err := Init(root)
		if err == nil || store == nil || receipt.WritesPerformed != 1 || receipt.Store.StoreID == "" || errors.Is(err, ErrDurabilityUnconfirmed) {
			t.Fatalf("Init returned store=%v receipt=%+v err=%v", store, receipt, err)
		}
	})

	t.Run("import_after_publish", func(t *testing.T) {
		store := newTestStore(t)
		raw := testMetafile("https://tracker.invalid/announce", "identity.bin", []byte("identity"))
		withIdentityFailure(t, "after_publish")
		meta, ref, receipt, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
		if err == nil || meta == nil || ref.ID == "" || receipt.WritesPerformed != 1 || errors.Is(err, ErrDurabilityUnconfirmed) {
			t.Fatalf("Import returned meta=%v ref=%+v receipt=%+v err=%v", meta, ref, receipt, err)
		}
	})

	t.Run("load_before_success", func(t *testing.T) {
		store := newTestStore(t)
		raw := testMetafile("https://tracker.invalid/announce", "load.bin", []byte("load"))
		_, ref, _, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		withIdentityFailure(t, "before_success")
		meta, loadedRef, err := store.Load(context.Background(), ref.ID, DefaultLimits())
		if err == nil || meta != nil || loadedRef.ID != "" {
			t.Fatalf("Load returned meta=%v ref=%+v err=%v", meta, loadedRef, err)
		}
	})
}

func TestFilesystemIdentityMismatchFailsClosedAndReleasesSession(t *testing.T) {
	store := newTestStore(t)
	raw := testMetafile("https://tracker.invalid/announce", "volume.bin", []byte("volume"))
	_, ref, _, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	original := sessionFilesystemIdentity
	calls := 0
	sessionFilesystemIdentity = func(file *os.File) (platformFilesystemID, error) {
		identity, identityErr := original(file)
		calls++
		if identityErr == nil && calls > 1 {
			identity.volume++
		}
		return identity, identityErr
	}
	_, _, loadErr := store.Load(context.Background(), ref.ID, DefaultLimits())
	sessionFilesystemIdentity = original
	if loadErr == nil {
		t.Fatal("Load accepted a split filesystem identity")
	}
	if err := os.RemoveAll(store.root); err != nil {
		t.Fatalf("failed identity session retained a handle: %v", err)
	}
}

func TestStoreOperationsReleaseWindowsDeletionHandles(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, *Store, string)
	}{
		{"init_only", func(*testing.T, *Store, string) {}},
		{"invalid_import", func(t *testing.T, store *Store, parent string) {
			invalidPath := filepath.Join(parent, "invalid.torrent")
			if err := os.WriteFile(invalidPath, []byte("invalid"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, _, _ = store.ImportFile(context.Background(), invalidPath, DefaultLimits())
		}},
		{"valid_import", func(t *testing.T, store *Store, _ string) {
			raw := testMetafile("https://tracker.invalid/announce", "valid.bin", []byte("valid"))
			if _, _, _, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits()); err != nil {
				t.Fatal(err)
			}
		}},
		{"corrupt_load", func(t *testing.T, store *Store, _ string) {
			raw := testMetafile("https://tracker.invalid/announce", "valid.bin", []byte("valid"))
			_, ref, _, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(store.root, objectRelativePath(ref.ID)), []byte("corrupt"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, _ = store.Load(context.Background(), ref.ID, DefaultLimits())
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := physicalTempDir(t)
			root := filepath.Join(parent, "store")
			store, _, err := Init(root)
			if err != nil {
				t.Fatal(err)
			}
			test.run(t, store, parent)
			if err := os.RemoveAll(root); err != nil {
				staging, stagingErr := os.ReadDir(filepath.Join(root, temporaryDir))
				t.Fatalf("store retained a deletion-blocking handle: %v; staging=%v stagingErr=%v", err, staging, stagingErr)
			}
		})
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, receipt, err := Init(filepath.Join(physicalTempDir(t), "store"))
	if err != nil || store == nil || receipt.WritesPerformed != 1 {
		t.Fatalf("Init returned store=%v receipt=%+v err=%v", store, receipt, err)
	}
	return store
}

func physicalTempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatalf("resolve temporary directory: %v", err)
	}
	return resolved
}

func createPrivateTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	file, err := platformCreatePrivateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAndFlush(file, content); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func testDirectoryTree(t *testing.T, root string) []string {
	t.Helper()
	var result []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root {
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			result = append(result, relative)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(result)
	return result
}

func withPublishError(t *testing.T, sentinel error) {
	t.Helper()
	original := publishNoReplace
	publishNoReplace = func(session *rootSession, temporaryRelative, finalRelative string) (bool, error) {
		published, err := original(session, temporaryRelative, finalRelative)
		if published && err == nil {
			return true, sentinel
		}
		return published, err
	}
	t.Cleanup(func() { publishNoReplace = original })
}

func withIdentityFailure(t *testing.T, wantedStage string) {
	t.Helper()
	original := operationIdentityHook
	operationIdentityHook = func(stage string, _ *rootSession) error {
		if stage == wantedStage {
			return errors.New("injected bound identity failure")
		}
		return nil
	}
	t.Cleanup(func() { operationIdentityHook = original })
}

func testMetafile(announce, name string, content []byte) []byte {
	piece := sha1.Sum(content)
	return testBencode(map[string]any{
		"announce": announce,
		"info": map[string]any{
			"length":       int64(len(content)),
			"name":         name,
			"piece length": int64(max(1, len(content))),
			"pieces":       piece[:],
			"private":      int64(1),
		},
	})
}

func testBencode(value any) []byte {
	var output bytes.Buffer
	var encode func(any)
	encode = func(current any) {
		switch typed := current.(type) {
		case string:
			output.WriteString(strconv.Itoa(len(typed)))
			output.WriteByte(':')
			output.WriteString(typed)
		case []byte:
			output.WriteString(strconv.Itoa(len(typed)))
			output.WriteByte(':')
			output.Write(typed)
		case int64:
			output.WriteByte('i')
			output.WriteString(strconv.FormatInt(typed, 10))
			output.WriteByte('e')
		case map[string]any:
			output.WriteByte('d')
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				encode(key)
				encode(typed[key])
			}
			output.WriteByte('e')
		default:
			panic(fmt.Sprintf("unsupported test bencode value %T", current))
		}
	}
	encode(value)
	return output.Bytes()
}
