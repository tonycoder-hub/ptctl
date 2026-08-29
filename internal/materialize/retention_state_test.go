package materialize

import (
	"context"
	"errors"
	"testing"
)

func TestLoadRetentionMarkerStateRebindsCanonicalMarkers(t *testing.T) {
	ctx := context.Background()
	session, rootInfo, _ := bindMaterializeTestRoot(t)
	intent := testIntent()
	intent.TargetRootIdentity = rootInfo.Identity.String()
	handle, _, err := createJournal(ctx, session, intent)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.subtree.Close()
	if _, err := handle.append(ctx, Event{Phase: PhaseAbandoned, Bytes: 0}); err != nil {
		t.Fatal(err)
	}
	marker := retentionIntentFixture(t, handle)
	intentID, _, err := ensureRetentionIntentMarker(ctx, handle.subtree, marker)
	if err != nil {
		t.Fatal(err)
	}
	complete := RetentionComplete{
		Schema: RetentionCompleteSchemaV1, OperationID: handle.state.OperationID,
		OperationRootIdentity: handle.subtree.Identity().String(), TargetRootIdentity: rootInfo.Identity.String(),
		IntentMarkerID: intentID,
	}
	completeID, _, err := ensureRetentionCompleteMarker(ctx, handle.subtree, complete)
	if err != nil {
		t.Fatal(err)
	}
	state, err := loadRetentionMarkerState(ctx, handle.subtree, handle.state.OperationID, rootInfo.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if !state.IntentPresent || !state.CompletePresent || state.IntentID != intentID || state.CompleteID != completeID || state.Intent != marker || state.Complete != complete {
		t.Fatalf("unexpected rebound state: %#v", state)
	}
	if err := auditRetentionControlDirectory(ctx, handle.subtree, state, false); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRetentionMarkerStateRejectsUnexpectedControlObject(t *testing.T) {
	ctx := context.Background()
	session, rootInfo, _ := bindMaterializeTestRoot(t)
	intent := testIntent()
	intent.TargetRootIdentity = rootInfo.Identity.String()
	handle, _, err := createJournal(ctx, session, intent)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.subtree.Close()
	if _, err := handle.append(ctx, Event{Phase: PhaseAbandoned, Bytes: 0}); err != nil {
		t.Fatal(err)
	}
	marker := retentionIntentFixture(t, handle)
	if _, _, err := ensureRetentionIntentMarker(ctx, handle.subtree, marker); err != nil {
		t.Fatal(err)
	}
	extra := mustFSPath(t, retentionDirectoryName, "unexpected")
	file, err := handle.subtree.CreateRegular(ctx, extra)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRetentionMarkerState(ctx, handle.subtree, handle.state.OperationID, rootInfo.Identity); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("unexpected retention control object accepted: %v", err)
	}
}
