package metastore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestRecordKindsIDsAndLimitsAreStrict(t *testing.T) {
	kinds := []RecordKind{
		RecordKindStorageProfileV1,
		RecordKindStorageIndexDataV1,
		RecordKindStorageIndexDescriptorV1,
		RecordKindSiteMetafileBindingV1,
	}
	for _, kind := range kinds {
		parsed, err := ParseRecordKind(string(kind))
		if err != nil || parsed != kind {
			t.Fatalf("ParseRecordKind(%q) = %q, %v", kind, parsed, err)
		}
	}
	for _, invalid := range []string{"", "storage.index.v1", "Storage.Profile.V1", "storage.profile.v1\n", "é"} {
		if parsed, err := ParseRecordKind(invalid); err == nil || parsed != "" || strings.Contains(err.Error(), invalid) && invalid != "" {
			t.Fatalf("ParseRecordKind(%q) = %q, %v", invalid, parsed, err)
		}
	}

	payload := []byte("same raw record")
	first := expectedRecordID(RecordKindStorageIndexDataV1, payload)
	second := expectedRecordID(RecordKindStorageIndexDescriptorV1, payload)
	plain := sha256.Sum256(payload)
	if first == second || first.String() == "sha256:"+hex.EncodeToString(plain[:]) {
		t.Fatalf("record IDs were not kind/domain separated: first=%s second=%s", first, second)
	}
	if ArtifactID(first) == ArtifactID(second) {
		t.Fatal("record kinds unexpectedly shared an identity")
	}
	if parsed, err := ParseRecordID(first.String()); err != nil || parsed != first {
		t.Fatalf("ParseRecordID = %q, %v", parsed, err)
	}
	for _, invalid := range []string{"", "sha256:" + strings.Repeat("A", 64), "sha256:" + strings.Repeat("g", 64), "PRIVATE-ID-CANARY"} {
		if parsed, err := ParseRecordID(invalid); err == nil || parsed != "" || strings.Contains(err.Error(), invalid) && invalid != "" {
			t.Fatalf("ParseRecordID(%q) = %q, %v", invalid, parsed, err)
		}
	}

	limits := DefaultRecordLimits()
	if err := limits.Validate(); err != nil || limits.MaxRecordBytes != 64<<20 {
		t.Fatalf("default limits = %+v, %v", limits, err)
	}
	invalidLimits := []RecordLimits{
		{MaxRecordBytes: 0, MaxEntries: 1, MaxRecords: 1, MaxPathBytes: 1},
		{MaxRecordBytes: hardMaxRecordBytes + 1, MaxEntries: 1, MaxRecords: 1, MaxPathBytes: 1},
		{MaxRecordBytes: 1, MaxEntries: hardMaxEntries + 1, MaxRecords: 1, MaxPathBytes: 1},
		{MaxRecordBytes: 1, MaxEntries: 1, MaxRecords: hardMaxRecords + 1, MaxPathBytes: 1},
		{MaxRecordBytes: 1, MaxEntries: 1, MaxRecords: 1, MaxPathBytes: hardMaxPathBytes + 1},
	}
	for _, invalid := range invalidLimits {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid limits accepted: %+v", invalid)
		}
	}
}

func TestRecordImportLoadListAndIdempotency(t *testing.T) {
	store := newTestStore(t)
	payload := []byte("opaque storage index record\x00with raw bytes")
	ref, receipt, err := store.ImportRecord(context.Background(), RecordKindStorageIndexDataV1, bytes.NewReader(payload), DefaultRecordLimits())
	if err != nil {
		t.Fatalf("ImportRecord: %v", err)
	}
	if ref.Kind != RecordKindStorageIndexDataV1 || ref.ID != expectedRecordID(ref.Kind, payload) || ref.SizeBytes != int64(len(payload)) || receipt.WritesPerformed != 1 || receipt.AlreadyPresent || receipt.BytesConsumed != int64(len(payload)) || receipt.Store != store.Info() {
		t.Fatalf("unexpected import: ref=%+v receipt=%+v", ref, receipt)
	}

	repeatedRef, repeated, err := store.ImportRecord(context.Background(), ref.Kind, bytes.NewReader(payload), DefaultRecordLimits())
	if err != nil || repeatedRef != ref || repeated.WritesPerformed != 0 || !repeated.AlreadyPresent || repeated.BytesConsumed != int64(len(payload)) {
		t.Fatalf("idempotent import: ref=%+v receipt=%+v err=%v", repeatedRef, repeated, err)
	}

	var got bytes.Buffer
	loadedRef, loaded, err := store.LoadRecord(context.Background(), ref.Kind, ref.ID, DefaultRecordLimits(), func(reader io.Reader) error {
		_, copyErr := io.Copy(&got, reader)
		return copyErr
	})
	if err != nil || loadedRef != ref || !loaded.Complete || !loaded.RecordBytesKnown || loaded.RecordBytesRead != int64(len(payload)) || loaded.ConsumerBytesRead != int64(len(payload)) || !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("LoadRecord: ref=%+v receipt=%+v got=%q err=%v", loadedRef, loaded, got.Bytes(), err)
	}

	// Existing torrent artifacts are valid siblings and must not make record
	// enumeration fail closed.
	torrent := testMetafile("https://tracker.invalid/announce", "sibling.bin", []byte("sibling"))
	if _, _, _, err := store.Import(context.Background(), bytes.NewReader(torrent), DefaultLimits()); err != nil {
		t.Fatalf("import sibling torrent: %v", err)
	}
	other, _, err := store.ImportRecord(context.Background(), RecordKindStorageProfileV1, bytes.NewReader(payload), DefaultRecordLimits())
	if err != nil || other.ID == ref.ID {
		t.Fatalf("domain-separated import: ref=%+v err=%v", other, err)
	}
	listed, err := store.ListRecords(context.Background(), ref.Kind, DefaultRecordLimits())
	if err != nil || !listed.Complete || listed.StopReason != "" || listed.Used.RecordsMatched != 1 || len(listed.Records) != 1 || listed.Records[0] != ref {
		t.Fatalf("ListRecords: result=%+v err=%v", listed, err)
	}
}

func TestConcurrentRecordImportPublishesExactlyOneObject(t *testing.T) {
	store := newTestStore(t)
	payload := []byte("concurrent sealed record")
	const workers = 8
	type outcome struct {
		ref     RecordRef
		receipt RecordImportReceipt
		err     error
	}
	results := make([]outcome, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for index := range results {
		go func() {
			defer wait.Done()
			results[index].ref, results[index].receipt, results[index].err = store.ImportRecord(context.Background(), RecordKindStorageIndexDescriptorV1, bytes.NewReader(payload), DefaultRecordLimits())
		}()
	}
	wait.Wait()
	writes := 0
	already := 0
	wanted := expectedRecordID(RecordKindStorageIndexDescriptorV1, payload)
	for index, result := range results {
		if result.err != nil || result.ref.ID != wanted {
			t.Fatalf("worker %d: ref=%+v receipt=%+v err=%v", index, result.ref, result.receipt, result.err)
		}
		writes += result.receipt.WritesPerformed
		if result.receipt.AlreadyPresent {
			already++
		}
	}
	if writes != 1 || already != workers-1 {
		t.Fatalf("writes=%d already=%d, want 1/%d", writes, already, workers-1)
	}
}

func TestRecordConsumerMustReachEOFAndCannotRetainReader(t *testing.T) {
	store := newTestStore(t)
	payload := []byte("consumer completeness proof")
	ref, _, err := store.ImportRecord(context.Background(), RecordKindStorageProfileV1, bytes.NewReader(payload), DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}

	var retained io.Reader
	loadedRef, receipt, err := store.LoadRecord(context.Background(), ref.Kind, ref.ID, DefaultRecordLimits(), func(reader io.Reader) error {
		retained = reader
		one := make([]byte, 1)
		_, readErr := reader.Read(one)
		return readErr
	})
	if !errors.Is(err, ErrRecordConsumerIncomplete) || loadedRef != ref || receipt.Complete || receipt.ConsumerBytesRead != 1 || receipt.RecordBytesRead != int64(len(payload)) {
		t.Fatalf("incomplete load: ref=%+v receipt=%+v err=%v", loadedRef, receipt, err)
	}
	if n, err := retained.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("retained reader remained usable: n=%d err=%v", n, err)
	}
	_, exact, err := store.LoadRecord(context.Background(), ref.Kind, ref.ID, DefaultRecordLimits(), func(reader io.Reader) error {
		buffer := make([]byte, len(payload))
		_, readErr := io.ReadFull(reader, buffer)
		return readErr
	})
	if !errors.Is(err, ErrRecordConsumerIncomplete) || exact.ConsumerBytesRead != int64(len(payload)) || exact.RecordBytesRead != int64(len(payload)) || exact.Complete {
		t.Fatalf("exact-length read incorrectly proved EOF: receipt=%+v err=%v", exact, err)
	}

	const canary = "PRIVATE-CONSUMER-ERROR-CANARY"
	_, failed, err := store.LoadRecord(context.Background(), ref.Kind, ref.ID, DefaultRecordLimits(), func(reader io.Reader) error {
		_, _ = io.Copy(io.Discard, reader)
		return errors.New(canary)
	})
	if err == nil || failed.Complete || strings.Contains(err.Error(), canary) || failed.RecordBytesRead != int64(len(payload)) {
		t.Fatalf("consumer error was not redacted/verified: receipt=%+v err=%v", failed, err)
	}
}

func TestRecordLoadDetectsCorruptionAndListRejectsUnknownObjects(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		store := newTestStore(t)
		payload := []byte("CORRUPT-RECORD-PAYLOAD-CANARY")
		ref, _, err := store.ImportRecord(context.Background(), RecordKindSiteMetafileBindingV1, bytes.NewReader(payload), DefaultRecordLimits())
		if err != nil {
			t.Fatal(err)
		}
		object := filepath.Join(store.root, recordRelativePath(ref.Kind, ref.ID))
		corrupt := []byte("PRIVATE-CORRUPTION-CANARY")
		if err := os.WriteFile(object, corrupt, 0o600); err != nil {
			t.Fatal(err)
		}
		called := false
		_, receipt, err := store.LoadRecord(context.Background(), ref.Kind, ref.ID, DefaultRecordLimits(), func(reader io.Reader) error {
			called = true
			_, copyErr := io.Copy(io.Discard, reader)
			return copyErr
		})
		if !called || !errors.Is(err, ErrCorruptRecord) || receipt.Complete || strings.Contains(err.Error(), string(corrupt)) || strings.Contains(err.Error(), ref.ID.String()) || strings.Contains(err.Error(), store.root) {
			t.Fatalf("corrupt load: called=%t receipt=%+v err=%v", called, receipt, err)
		}
		if err := os.RemoveAll(store.root); err != nil {
			t.Fatalf("corrupt load retained a deletion handle: %v", err)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		store := newTestStore(t)
		unknown := filepath.Join(store.root, objectsDir, "UNKNOWN-OBJECT-CANARY")
		createPrivateTestFile(t, unknown, []byte("PRIVATE-UNKNOWN-CONTENT-CANARY"))
		result, err := store.ListRecords(context.Background(), RecordKindStorageProfileV1, DefaultRecordLimits())
		if !errors.Is(err, ErrCorruptRecord) || result.Complete || strings.Contains(err.Error(), "UNKNOWN-OBJECT-CANARY") || strings.Contains(err.Error(), store.root) {
			t.Fatalf("unknown object list: result=%+v err=%v", result, err)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		store := newTestStore(t)
		missing := expectedRecordID(RecordKindStorageProfileV1, []byte("missing"))
		called := false
		_, _, err := store.LoadRecord(context.Background(), RecordKindStorageProfileV1, missing, DefaultRecordLimits(), func(io.Reader) error {
			called = true
			return nil
		})
		if !errors.Is(err, ErrRecordNotFound) || called {
			t.Fatalf("missing record: called=%t err=%v", called, err)
		}
	})
}

func TestRecordListBudgetsAreNPlusOneAndDeterministic(t *testing.T) {
	makeStore := func(t *testing.T) (*Store, []RecordRef) {
		t.Helper()
		store := newTestStore(t)
		refs := make([]RecordRef, 0, 2)
		for _, payload := range [][]byte{[]byte("record-b"), []byte("record-a")} {
			ref, _, err := store.ImportRecord(context.Background(), RecordKindStorageIndexDataV1, bytes.NewReader(payload), DefaultRecordLimits())
			if err != nil {
				t.Fatal(err)
			}
			refs = append(refs, ref)
		}
		return store, refs
	}

	t.Run("entry", func(t *testing.T) {
		store, _ := makeStore(t)
		limits := DefaultRecordLimits()
		limits.MaxEntries = 1
		first, err := store.ListRecords(context.Background(), RecordKindStorageIndexDataV1, limits)
		if err != nil || first.Complete || first.StopReason != "entry_limit" || first.Used.EntriesConsidered != 2 || len(first.Records) != 0 {
			t.Fatalf("entry limit: result=%+v err=%v", first, err)
		}
		second, err := store.ListRecords(context.Background(), RecordKindStorageIndexDataV1, limits)
		if err != nil || !reflect.DeepEqual(first, second) {
			t.Fatalf("entry result was not deterministic: first=%+v second=%+v err=%v", first, second, err)
		}
	})

	t.Run("record", func(t *testing.T) {
		store, _ := makeStore(t)
		limits := DefaultRecordLimits()
		limits.MaxRecords = 1
		first, err := store.ListRecords(context.Background(), RecordKindStorageIndexDataV1, limits)
		if err != nil || first.Complete || first.StopReason != "record_limit" || first.Used.RecordsMatched != 2 || len(first.Records) != 1 {
			t.Fatalf("record limit: result=%+v err=%v", first, err)
		}
		second, err := store.ListRecords(context.Background(), RecordKindStorageIndexDataV1, limits)
		if err != nil || !reflect.DeepEqual(first, second) {
			t.Fatalf("record result was not deterministic: first=%+v second=%+v err=%v", first, second, err)
		}
	})

	t.Run("path", func(t *testing.T) {
		store, _ := makeStore(t)
		limits := DefaultRecordLimits()
		limits.MaxPathBytes = 1
		first, err := store.ListRecords(context.Background(), RecordKindStorageIndexDataV1, limits)
		if err != nil || first.Complete || first.StopReason != "path_limit" || first.Used.EntriesConsidered != 1 || first.Used.PathBytes != 0 || len(first.Records) != 0 {
			t.Fatalf("path limit: result=%+v err=%v", first, err)
		}
		second, err := store.ListRecords(context.Background(), RecordKindStorageIndexDataV1, limits)
		if err != nil || !reflect.DeepEqual(first, second) {
			t.Fatalf("path result was not deterministic: first=%+v second=%+v err=%v", first, second, err)
		}
	})
}

func TestRecordOperationsHonorCancellationWithoutPublishing(t *testing.T) {
	store := newTestStore(t)
	payload := []byte("cancelled record")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before, err := os.ReadDir(filepath.Join(store.root, temporaryDir))
	if err != nil {
		t.Fatal(err)
	}
	ref, receipt, err := store.ImportRecord(ctx, RecordKindStorageProfileV1, bytes.NewReader(payload), DefaultRecordLimits())
	if !errors.Is(err, context.Canceled) || ref.ID != "" || receipt.WritesPerformed != 0 || receipt.BytesConsumed != 0 {
		t.Fatalf("cancelled import: ref=%+v receipt=%+v err=%v", ref, receipt, err)
	}
	after, err := os.ReadDir(filepath.Join(store.root, temporaryDir))
	if err != nil || len(after) != len(before) {
		t.Fatalf("cancelled import changed staging: before=%d after=%d err=%v", len(before), len(after), err)
	}

	validRef, _, err := store.ImportRecord(context.Background(), RecordKindStorageProfileV1, bytes.NewReader(payload), DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}
	called := false
	_, loaded, err := store.LoadRecord(ctx, validRef.Kind, validRef.ID, DefaultRecordLimits(), func(io.Reader) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) || called || loaded.RecordBytesRead != 0 || loaded.Complete {
		t.Fatalf("cancelled load: called=%t receipt=%+v err=%v", called, loaded, err)
	}
	listed, err := store.ListRecords(ctx, validRef.Kind, DefaultRecordLimits())
	if !errors.Is(err, context.Canceled) || listed.Complete || listed.StopReason != "context_cancelled" || listed.Used.EntriesConsidered != 0 {
		t.Fatalf("cancelled list: result=%+v err=%v", listed, err)
	}
}

func TestRecordPostPublicationErrorsPreserveRefAndReceipt(t *testing.T) {
	for _, test := range []struct {
		name     string
		sentinel error
	}{
		{"durability", ErrDurabilityUnconfirmed},
		{"cleanup", ErrPublishedCleanupIncomplete},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			withPublishError(t, test.sentinel)
			withIdentityFailure(t, "after_publish")
			payload := []byte("post-publication " + test.name)
			ref, receipt, err := store.ImportRecord(context.Background(), RecordKindStorageIndexDescriptorV1, bytes.NewReader(payload), DefaultRecordLimits())
			if !errors.Is(err, test.sentinel) || ref.ID != expectedRecordID(ref.Kind, payload) || receipt.WritesPerformed != 1 || receipt.AlreadyPresent || receipt.BytesConsumed != int64(len(payload)) {
				t.Fatalf("post-publication result: ref=%+v receipt=%+v err=%v", ref, receipt, err)
			}
			// Restore only the identity seam so the already-published object can be
			// verified while the subtest cleanup later restores both globals.
			operationIdentityHook = nil
			var got bytes.Buffer
			loadedRef, loaded, loadErr := store.LoadRecord(context.Background(), ref.Kind, ref.ID, DefaultRecordLimits(), func(reader io.Reader) error {
				_, copyErr := io.Copy(&got, reader)
				return copyErr
			})
			if loadErr != nil || loadedRef != ref || !loaded.Complete || !bytes.Equal(got.Bytes(), payload) {
				t.Fatalf("published record did not load: ref=%+v receipt=%+v err=%v", loadedRef, loaded, loadErr)
			}
		})
	}
}

func TestRecordImportProvesEOFAndEnforcesNPlusOneByteLimit(t *testing.T) {
	store := newTestStore(t)
	limits := DefaultRecordLimits()
	limits.MaxRecordBytes = 4
	reader := &countingReader{raw: []byte("12345PRIVATE-CANARY")}
	ref, receipt, err := store.ImportRecord(context.Background(), RecordKindStorageProfileV1, reader, limits)
	if err == nil || ref.ID != "" || receipt.WritesPerformed != 0 || receipt.BytesConsumed != 5 || reader.read != 5 || strings.Contains(err.Error(), "PRIVATE-CANARY") {
		t.Fatalf("byte limit was not exact N+1: ref=%+v receipt=%+v read=%d err=%v", ref, receipt, reader.read, err)
	}
}

func TestVerifyRecordSetBindsAllRecordsToOneStoreSession(t *testing.T) {
	store := newTestStore(t)
	first, _, err := store.ImportRecord(context.Background(), RecordKindStorageIndexDescriptorV1, bytes.NewBufferString("descriptor"), DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.ImportRecord(context.Background(), RecordKindStorageIndexDataV1, bytes.NewBufferString("data"), DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := store.VerifyRecordSet(context.Background(), []RecordRef{first, second}, DefaultRecordLimits())
	if err != nil || !receipt.Complete || receipt.RecordsVerified != 2 || receipt.BytesRead != first.SizeBytes+second.SizeBytes {
		t.Fatalf("record set verification failed: receipt=%#v err=%v", receipt, err)
	}
	if _, err := store.VerifyRecordSet(context.Background(), []RecordRef{first, first}, DefaultRecordLimits()); err == nil {
		t.Fatal("duplicate record set was accepted")
	}
	if err := os.Remove(filepath.Join(store.root, filepath.FromSlash(recordRelativePath(second.Kind, second.ID)))); err != nil {
		t.Fatal(err)
	}
	receipt, err = store.VerifyRecordSet(context.Background(), []RecordRef{first, second}, DefaultRecordLimits())
	if err == nil || receipt.Complete || receipt.RecordsVerified >= 2 {
		t.Fatalf("split record set was accepted: receipt=%#v err=%v", receipt, err)
	}
}

func TestRecordOperationsReleaseDeletionHandles(t *testing.T) {
	parent := physicalTempDir(t)
	root := filepath.Join(parent, "store")
	store, _, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("handle lifecycle")
	ref, _, err := store.ImportRecord(context.Background(), RecordKindStorageProfileV1, bytes.NewReader(payload), DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = store.LoadRecord(context.Background(), ref.Kind, ref.ID, DefaultRecordLimits(), func(io.Reader) error { return nil })
	_, _ = store.ListRecords(context.Background(), ref.Kind, DefaultRecordLimits())
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("sealed record operation retained a deletion-blocking handle: %v", err)
	}
}

type countingReader struct {
	raw  []byte
	read int
}

func (reader *countingReader) Read(buffer []byte) (int, error) {
	if len(reader.raw) == 0 {
		return 0, io.EOF
	}
	n := copy(buffer, reader.raw)
	reader.raw = reader.raw[n:]
	reader.read += n
	return n, nil
}

func expectedRecordID(kind RecordKind, payload []byte) RecordID {
	hasher := sha256.New()
	_, _ = io.WriteString(hasher, recordDigestDomain)
	_, _ = io.WriteString(hasher, string(kind))
	_, _ = io.WriteString(hasher, "\x00")
	_, _ = hasher.Write(payload)
	return RecordID(fmt.Sprintf("sha256:%x", hasher.Sum(nil)))
}
