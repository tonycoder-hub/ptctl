package metastore

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"sync"
	"testing"
)

func TestLinkedRecordUsesOneBoundSessionAndVerifiesBothObjects(t *testing.T) {
	store := newTestStore(t)
	raw := testMetafile("https://tracker.invalid/announce", "linked.bin", []byte("linked content"))
	_, artifact, _, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	link := ArtifactLink{ID: artifact.ID, SizeBytes: artifact.SizeBytes, RequirePrivate: true}
	payload := []byte("linked-record-payload")

	original := operationIdentityHook
	var lock sync.Mutex
	var operationSession *rootSession
	operationIdentityHook = func(_ string, current *rootSession) error {
		lock.Lock()
		defer lock.Unlock()
		if operationSession == nil {
			operationSession = current
		} else if operationSession != current {
			return errors.New("operation used a second root session")
		}
		return nil
	}
	t.Cleanup(func() { operationIdentityHook = original })

	recordRef, imported, err := store.ImportRecordLinkedArtifact(
		context.Background(), RecordKindSiteMetafileBindingV1, bytes.NewReader(payload),
		DefaultRecordLimits(), link, DefaultLimits(),
	)
	if err != nil || recordRef.ID != expectedRecordID(recordRef.Kind, payload) || imported.WritesPerformed != 1 {
		t.Fatalf("linked import: ref=%+v receipt=%+v err=%v", recordRef, imported, err)
	}

	operationSession = nil
	loadedRecord, loaded, meta, loadedArtifact, err := store.LoadRecordLinkedArtifact(
		context.Background(), recordRef.Kind, recordRef.ID, DefaultRecordLimits(), DefaultLimits(),
		func(reader io.Reader) (ArtifactLink, error) {
			got, readErr := io.ReadAll(reader)
			if readErr != nil || !bytes.Equal(got, payload) {
				return ArtifactLink{}, errors.New("record payload disagrees")
			}
			return link, nil
		},
	)
	if err != nil || loadedRecord != recordRef || !loaded.Complete || meta == nil || !meta.Private || loadedArtifact != artifact {
		t.Fatalf("linked load: record=%+v receipt=%+v meta=%v artifact=%+v err=%v", loadedRecord, loaded, meta, loadedArtifact, err)
	}
}

func TestLinkedRecordRejectsArtifactMismatchBeforePublication(t *testing.T) {
	store := newTestStore(t)
	raw := testMetafile("https://tracker.invalid/announce", "linked.bin", []byte("linked content"))
	_, artifact, _, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("must-not-be-published")
	tests := []ArtifactLink{
		{ID: artifact.ID, SizeBytes: artifact.SizeBytes + 1, RequirePrivate: true},
		{ID: artifactIDFor([]byte("missing")), SizeBytes: artifact.SizeBytes, RequirePrivate: true},
	}
	for _, link := range tests {
		ref, receipt, err := store.ImportRecordLinkedArtifact(context.Background(), RecordKindSiteMetafileBindingV1, bytes.NewReader(payload), DefaultRecordLimits(), link, DefaultLimits())
		if err == nil || ref.ID != "" || receipt.WritesPerformed != 0 {
			t.Fatalf("mismatched link published: link=%+v ref=%+v receipt=%+v err=%v", link, ref, receipt, err)
		}
	}
	if _, err := store.ListRecords(context.Background(), RecordKindSiteMetafileBindingV1, DefaultRecordLimits()); err != nil {
		t.Fatalf("mismatch left an invalid object: %v", err)
	}
}

func TestLinkedRecordRequiresPrivateArtifact(t *testing.T) {
	store := newTestStore(t)
	content := []byte("public content")
	raw := testBencode(map[string]any{
		"announce": "https://tracker.invalid/announce",
		"info": map[string]any{
			"length": int64(len(content)), "name": "public.bin", "piece length": int64(len(content)), "pieces": testPiece(content),
		},
	})
	meta, artifact, _, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
	if err != nil || meta.Private {
		t.Fatalf("public artifact fixture: meta=%v err=%v", meta, err)
	}
	_, receipt, err := store.ImportRecordLinkedArtifact(context.Background(), RecordKindSiteMetafileBindingV1, bytes.NewReader([]byte("binding")), DefaultRecordLimits(), ArtifactLink{
		ID: artifact.ID, SizeBytes: artifact.SizeBytes, RequirePrivate: true,
	}, DefaultLimits())
	if !errors.Is(err, ErrCorruptArtifact) || receipt.WritesPerformed != 0 {
		t.Fatalf("public artifact accepted: receipt=%+v err=%v", receipt, err)
	}
}

func TestLinkedRecordPreservesPublishedReceiptOnBindingFailure(t *testing.T) {
	store := newTestStore(t)
	raw := testMetafile("https://tracker.invalid/announce", "published.bin", []byte("content"))
	_, artifact, _, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	withIdentityFailure(t, "linked_record_imported")
	ref, receipt, err := store.ImportRecordLinkedArtifact(context.Background(), RecordKindSiteMetafileBindingV1, bytes.NewReader([]byte("published record")), DefaultRecordLimits(), ArtifactLink{
		ID: artifact.ID, SizeBytes: artifact.SizeBytes, RequirePrivate: true,
	}, DefaultLimits())
	if err == nil || ref.ID == "" || receipt.WritesPerformed != 1 {
		t.Fatalf("publication evidence lost: ref=%+v receipt=%+v err=%v", ref, receipt, err)
	}
}

func testPiece(content []byte) []byte {
	digest := sha1.Sum(content)
	return digest[:]
}
