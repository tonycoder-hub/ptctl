package metastore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestRemoveRecordExactIsRecoverableAndIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	payload := []byte("terminal bonus exchange record\n")
	ref, _, err := store.ImportRecord(ctx, RecordKindSiteBonusExchangeOutcomeV1, bytes.NewReader(payload), DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.OpenBoundRecordSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()

	receipt, err := bound.RemoveRecordExact(ctx, ref, DefaultRecordLimits())
	if err != nil || !receipt.Attempted || !receipt.Removed || receipt.AlreadyAbsent || receipt.WritesPerformed == 0 ||
		!receipt.DurabilityConfirmed || receipt.Record != ref || receipt.Store.StoreID != store.Info().StoreID {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	if _, _, err := bound.LoadRecord(ctx, ref.Kind, ref.ID, DefaultRecordLimits(), func(reader io.Reader) error {
		_, readErr := io.Copy(io.Discard, reader)
		return readErr
	}); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("removed record remained readable: %v", err)
	}

	repeated, err := bound.RemoveRecordExact(ctx, ref, DefaultRecordLimits())
	if err != nil || repeated.Attempted || repeated.Removed || !repeated.AlreadyAbsent || repeated.WritesPerformed != 0 || !repeated.DurabilityConfirmed {
		t.Fatalf("repeated=%#v err=%v", repeated, err)
	}
}

func TestRemoveRecordExactRecoversDeterministicStagingResidue(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	payload := []byte("recoverable terminal record\n")
	ref, _, err := store.ImportRecord(ctx, RecordKindSiteBonusExchangeOutcomeV1, bytes.NewReader(payload), DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.OpenBoundRecordSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	object := recordRelativePath(ref.Kind, ref.ID)
	staging := recordRemovalStagingPath(ref)
	attempted, staged, err := platformSessionStageRecordRemoval(bound.session, object, staging)
	if err != nil || !attempted || !staged {
		t.Fatalf("stage attempted=%t staged=%t err=%v", attempted, staged, err)
	}
	if err := bound.Close(); err != nil {
		t.Fatal(err)
	}
	bound, err = store.OpenBoundRecordSession(ctx)
	if err != nil {
		t.Fatalf("reopen with removal residue: %v", err)
	}
	defer bound.Close()

	receipt, err := bound.RemoveRecordExact(ctx, ref, DefaultRecordLimits())
	if err != nil || !receipt.ResidueRecovered || !receipt.Removed || !receipt.DurabilityConfirmed {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	objectPresent, err := verifyRemovalRecord(ctx, bound.session, object, ref, DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}
	stagingPresent, err := verifyRemovalRecord(ctx, bound.session, staging, ref, DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}
	if objectPresent || stagingPresent {
		t.Fatalf("retirement residue remained object=%t staging=%t", objectPresent, stagingPresent)
	}
}

func TestRemoveRecordExactRejectsCorruptResidueBeforeDeletion(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	payload := []byte("selected exact record\n")
	ref, _, err := store.ImportRecord(ctx, RecordKindSiteBonusExchangeOutcomeV1, bytes.NewReader(payload), DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.OpenBoundRecordSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	file, err := bound.session.createPrivateFile(recordRemovalStagingPath(ref))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("different bytes\n")); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	receipt, err := bound.RemoveRecordExact(ctx, ref, DefaultRecordLimits())
	if !errors.Is(err, ErrCorruptRecord) || receipt.Attempted || receipt.Removed {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	if present, verifyErr := verifyRemovalRecord(ctx, bound.session, recordRelativePath(ref.Kind, ref.ID), ref, DefaultRecordLimits()); verifyErr != nil || !present {
		t.Fatalf("selected record changed present=%t err=%v", present, verifyErr)
	}
}

func TestRemoveRecordExactPreservesPostRemovalDurabilityEvidence(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	payload := []byte("durability receipt record\n")
	ref, _, err := store.ImportRecord(ctx, RecordKindSiteBonusExchangeOutcomeV1, bytes.NewReader(payload), DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.OpenBoundRecordSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	oldSync := syncRecordRemoval
	syncRecordRemoval = func(*rootSession) error { return errors.New("CANARY sync failure") }
	t.Cleanup(func() { syncRecordRemoval = oldSync })

	receipt, err := bound.RemoveRecordExact(ctx, ref, DefaultRecordLimits())
	if !errors.Is(err, ErrRemovalDurabilityUnconfirmed) || !receipt.Removed || receipt.DurabilityConfirmed || receipt.WritesPerformed == 0 ||
		bytes.Contains([]byte(err.Error()), []byte("CANARY")) {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
}

func TestRemoveRecordExactHonorsPreCancellationWithoutMutation(t *testing.T) {
	store := newTestStore(t)
	payload := []byte("cancelled removal record\n")
	ref, _, err := store.ImportRecord(context.Background(), RecordKindSiteBonusExchangeOutcomeV1, bytes.NewReader(payload), DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.OpenBoundRecordSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	receipt, err := bound.RemoveRecordExact(ctx, ref, DefaultRecordLimits())
	if !errors.Is(err, context.Canceled) || receipt.Attempted || receipt.WritesPerformed != 0 {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	if present, verifyErr := verifyRemovalRecord(context.Background(), bound.session, recordRelativePath(ref.Kind, ref.ID), ref, DefaultRecordLimits()); verifyErr != nil || !present {
		t.Fatalf("cancelled removal changed record present=%t err=%v", present, verifyErr)
	}
}
