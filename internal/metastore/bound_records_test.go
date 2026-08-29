package metastore

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
)

func TestBoundRecordSessionKeepsOneAuthorityAcrossMarkerProtocol(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	bound, err := store.OpenBoundRecordSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rootAuthority := bound.session

	intentPayload := []byte("canonical-intent\n")
	wantIntent, err := ComputeRecordRef(RecordKindSiteBonusExchangeIntentV1, intentPayload)
	if err != nil {
		t.Fatal(err)
	}
	intent, imported, err := bound.ImportRecord(ctx, wantIntent.Kind, bytes.NewReader(intentPayload), DefaultRecordLimits())
	if err != nil || intent != wantIntent || imported.WritesPerformed != 1 || imported.AlreadyPresent {
		t.Fatalf("intent=%#v receipt=%#v err=%v", intent, imported, err)
	}
	if bound.session != rootAuthority {
		t.Fatal("bound session replaced its physical root authority")
	}

	attemptPayload := []byte("canonical-attempt\n")
	wantAttempt, err := ComputeRecordRef(RecordKindSiteBonusExchangeAttemptV1, attemptPayload)
	if err != nil {
		t.Fatal(err)
	}
	attempt, attempted, err := bound.ImportRecord(ctx, wantAttempt.Kind, bytes.NewReader(attemptPayload), DefaultRecordLimits())
	if err != nil || attempt != wantAttempt || attempted.WritesPerformed != 1 || attempted.AlreadyPresent {
		t.Fatalf("attempt=%#v receipt=%#v err=%v", attempt, attempted, err)
	}

	var loaded []byte
	loadedRef, loadReceipt, err := bound.LoadRecord(ctx, intent.Kind, intent.ID, DefaultRecordLimits(), func(reader io.Reader) error {
		var readErr error
		loaded, readErr = io.ReadAll(reader)
		return readErr
	})
	if err != nil || loadedRef != intent || !loadReceipt.Complete || !bytes.Equal(loaded, intentPayload) {
		t.Fatalf("loaded=%q ref=%#v receipt=%#v err=%v", loaded, loadedRef, loadReceipt, err)
	}
	verified, err := bound.VerifyRecordSet(ctx, []RecordRef{intent, attempt}, DefaultRecordLimits())
	if err != nil || !verified.Complete || verified.RecordsVerified != 2 {
		t.Fatalf("verification=%#v err=%v", verified, err)
	}
	listed, err := bound.ListRecords(ctx, attempt.Kind, DefaultRecordLimits())
	if err != nil || !listed.Complete || len(listed.Records) != 1 || listed.Records[0] != attempt {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	if err := bound.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bound.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bound.Close(); err != nil {
		t.Fatal(err)
	}
	if _, receipt, err := bound.ImportRecord(ctx, attempt.Kind, bytes.NewReader(attemptPayload), DefaultRecordLimits()); err == nil || receipt.WritesPerformed != 0 {
		t.Fatalf("closed import receipt=%#v err=%v", receipt, err)
	}
	if err := os.RemoveAll(store.root); err != nil {
		t.Fatalf("bound session leaked a deletion-blocking handle: %v", err)
	}
}

func TestComputeRecordRefIsKindSeparated(t *testing.T) {
	payload := []byte("same payload\n")
	intent, err := ComputeRecordRef(RecordKindSiteBonusExchangeIntentV1, payload)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := ComputeRecordRef(RecordKindSiteBonusExchangeAttemptV1, payload)
	if err != nil {
		t.Fatal(err)
	}
	if intent.ID == attempt.ID || intent.SizeBytes != int64(len(payload)) || attempt.SizeBytes != int64(len(payload)) {
		t.Fatalf("intent=%#v attempt=%#v", intent, attempt)
	}
	if _, err := ComputeRecordRef("untrusted.kind", payload); err == nil {
		t.Fatal("unknown record kind produced a deterministic locator")
	}
}
