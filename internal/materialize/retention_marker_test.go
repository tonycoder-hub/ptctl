package materialize

import (
	"context"
	"errors"
	"testing"
)

func TestRetentionMarkerPublicationIsIdempotentAndIndependentOfScratchCapacity(t *testing.T) {
	ctx := context.Background()
	session, info, _ := bindMaterializeTestRoot(t)
	intent := testIntent()
	intent.TargetRootIdentity = info.Identity.String()
	handle, _, err := createJournal(ctx, session, intent)
	if err != nil {
		t.Fatal(err)
	}
	marker := retentionIntentFixture(t, handle)
	markerID, first, err := ensureRetentionIntentMarker(ctx, handle.subtree, marker)
	if err != nil || markerID == "" || !first.DirectoryCreated || !first.TemporaryCreated || !first.Publication.Published || first.AlreadyPresent {
		t.Fatalf("first marker publication: id=%q receipt=%#v err=%v", markerID, first, err)
	}
	secondID, second, err := ensureRetentionIntentMarker(ctx, handle.subtree, marker)
	if err != nil || secondID != markerID || !second.AlreadyPresent || second.DirectoryCreated || second.TemporaryCreated || second.Publication.Attempted {
		t.Fatalf("idempotent marker publication: id=%q receipt=%#v err=%v", secondID, second, err)
	}
	if err := handle.subtree.Close(); err != nil {
		t.Fatal(err)
	}
	ordinary, err := openJournal(ctx, session, handle.state.OperationID, intent.Limits)
	if !errors.Is(err, ErrCorruptJournal) || ordinary != nil {
		t.Fatalf("ordinary resume accepted retention intent: journal=%#v err=%v", ordinary, err)
	}
	retentionJournal, err := openJournalForRetention(ctx, session, handle.state.OperationID, intent.Limits)
	if err != nil {
		t.Fatalf("retention replay rejected its reserved directory: %v", err)
	}
	_ = retentionJournal.subtree.Close()
}

func TestRetentionMarkerRejectsChangedDeterministicTemporary(t *testing.T) {
	ctx := context.Background()
	session, info, _ := bindMaterializeTestRoot(t)
	intent := testIntent()
	intent.TargetRootIdentity = info.Identity.String()
	handle, _, err := createJournal(ctx, session, intent)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.subtree.Close()
	marker := retentionIntentFixture(t, handle)
	_, id, err := EncodeRetentionIntent(marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.subtree.MkdirAll(ctx, mustFSPath(t, retentionDirectoryName)); err != nil {
		t.Fatal(err)
	}
	temporary := mustFSPath(t, retentionDirectoryName, "intent-"+id.String()[len("sha256:"):]+".pending")
	file, err := handle.subtree.CreateRegular(ctx, temporary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("different")); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, receipt, err := ensureRetentionIntentMarker(ctx, handle.subtree, marker); !errors.Is(err, ErrIntegrity) || receipt.Publication.Attempted {
		t.Fatalf("changed retention temporary crossed publication boundary: %#v %v", receipt, err)
	}
}

func retentionIntentFixture(t *testing.T, handle *journal) RetentionIntent {
	t.Helper()
	digest, err := IntentSHA256(handle.intent)
	if err != nil {
		t.Fatal(err)
	}
	return RetentionIntent{
		Schema: RetentionIntentSchemaV1, OperationID: handle.state.OperationID,
		OperationRootIdentity: handle.subtree.Identity().String(), TargetRootIdentity: handle.session.Info().Identity.String(),
		IntentSHA256: digest, TerminalEventID: handle.state.LastEventID, TerminalPhase: PhaseAbandoned,
		PlanID: handle.intent.PlanID, MetafileVariantID: handle.intent.MetafileVariantID, Basis: RetentionBasisAbandoned,
	}
}
