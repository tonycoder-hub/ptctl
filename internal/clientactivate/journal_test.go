package clientactivate

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

func testActivationPlan(t *testing.T, root string) Plan {
	t.Helper()
	session, info, err := fsbind.BindExisting(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	id := func(character string) string { return "sha256:" + strings.Repeat(character, 64) }
	return Plan{
		Schema: PlanSchemaV1, Action: ActionRecheckOnly, Driver: DriverQBittorrent,
		ClientConfigID: id("1"), Control: downloader.ExistingJobControlDescriptor{
			Driver: DriverQBittorrent, Protocol: downloader.ControlProtocolQBittorrentV5,
			RecheckRouteID: "qbittorrent.torrents.recheck.v1", StartRouteID: "qbittorrent.torrents.start.v1",
		},
		PathMappingID: id("2"), ClientPathSemantics: "posix_exact", ExpectedSavePathRef: id("3"),
		ExpectedContentPathRef: id("4"), ExpectedFileLayoutID: id("5"), JobID: id("6"),
		MetafileVariantID: id("7"), InfoHashV1: strings.Repeat("8", 40),
		MaterializeOperationID: id("9"), MaterializePlanID: strings.Repeat("a", 24),
		AdoptionOperationID: id("b"), AdoptionPlanID: strings.Repeat("c", 24), AdoptionCompletionID: id("d"),
		TargetRootIdentity: info.Identity.String(), FinalObjectIdentity: info.Identity.String(),
		ManifestFiles: 1, ContentBytes: 1, FileLimits: downloader.DefaultJobFileLedgerLimits(),
	}
}

func TestActivationJournalRoundTripIsReadOnlyAndCanonical(t *testing.T) {
	root := t.TempDir()
	plan := testActivationPlan(t, root)
	planID, err := PlanID(plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, creation, err := createJournal(context.Background(), root, plan, planID)
	if err != nil {
		t.Fatal(err)
	}
	if !creation.Subtree.Created || !creation.Intent.Publication.Published || !handle.state.Durable {
		t.Fatalf("creation=%#v state=%#v", creation, handle.state)
	}
	now := time.Now().UTC()
	observation := clientObservation{
		ledger: downloader.LedgerSnapshot{ObservedAtStart: now, ObservedAtEnd: now.Add(time.Millisecond)},
		job:    downloader.Torrent{State: "stoppedDL", Progress: 0.25}, jobID: plan.JobID, fileLayoutID: plan.ExpectedFileLayoutID,
	}
	if receipt, _, appendErr := handle.appendAttempt(context.Background(), AttemptActionRecheck, observation); appendErr != nil || !receipt.Publication.Published {
		t.Fatalf("receipt=%#v err=%v", receipt, appendErr)
	}
	operationID := handle.state.Intent.OperationID
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	reopened, recovery, err := openJournal(context.Background(), root, operationID, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Mkdir.DirectoriesCreated != 0 || recovery.Marker.Publication.Attempted || len(before) != len(after) || len(reopened.state.RecheckAttempts) != 1 || reopened.state.Durable {
		t.Fatalf("recovery=%#v before=%d after=%d state=%#v", recovery, len(before), len(after), reopened.state)
	}
}

func TestActivationJournalAlreadyPresentRemovesMatchingPendingMarker(t *testing.T) {
	root := t.TempDir()
	plan := testActivationPlan(t, root)
	planID, _ := PlanID(plan)
	handle, _, err := createJournal(context.Background(), root, plan, planID)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	raw, _, err := encodeIntent(handle.state.Intent)
	if err != nil {
		t.Fatal(err)
	}
	pendingPath, _ := fsbind.PathFromComponents([]string{scratchDirectory, pendingName(intentFileName)})
	file, err := handle.subtree.CreateRegular(context.Background(), pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	receipt, err := handle.writeMarker(context.Background(), intentFileName, raw)
	if err != nil || !receipt.AlreadyPresent || !receipt.TemporaryRemovalAttempted || !receipt.TemporaryRemoved {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	if _, err := handle.subtree.Inspect(context.Background(), pendingPath); !errors.Is(err, fsbind.ErrNotFound) {
		t.Fatalf("pending marker remains: %v", err)
	}
}

func TestActivationJournalPublicationRejectsUnexpectedScratchNamespace(t *testing.T) {
	root := t.TempDir()
	plan := testActivationPlan(t, root)
	planID, _ := PlanID(plan)
	handle, _, err := createJournal(context.Background(), root, plan, planID)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	unexpected, _ := fsbind.PathFromComponents([]string{scratchDirectory, "unexpected.pending"})
	file, err := handle.subtree.CreateRegular(context.Background(), unexpected)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _, err := encodeIntent(handle.state.Intent)
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := handle.writeMarker(context.Background(), intentFileName, raw); !errors.Is(err, ErrIntegrity) || !receipt.AlreadyPresent {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
}

func TestActivationJournalPreCanceledOpenDoesNotClaimCorruption(t *testing.T) {
	root := t.TempDir()
	plan := testActivationPlan(t, root)
	planID, _ := PlanID(plan)
	handle, _, err := createJournal(context.Background(), root, plan, planID)
	if err != nil {
		t.Fatal(err)
	}
	operationID := handle.state.Intent.OperationID
	_ = handle.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = openJournal(ctx, root, operationID, false, nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrIntegrity) {
		t.Fatalf("pre-cancel error=%v", err)
	}
}
