package materialize

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

func bindMaterializeTestRoot(t *testing.T) (*fsbind.Session, fsbind.RootInfo, string) {
	t.Helper()
	root := t.TempDir()
	session, info, err := fsbind.BindExisting(root)
	if errors.Is(err, fsbind.ErrUnsupported) {
		t.Skip("test filesystem is outside the materialize v1 allowlist")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, info, root
}

func TestCreateAndOpenJournalBindsOperationDirectoryIdentity(t *testing.T) {
	ctx := context.Background()
	session, info, _ := bindMaterializeTestRoot(t)
	intent := testIntent()
	intent.TargetRootIdentity = info.Identity.String()
	handle, creation, err := createJournal(ctx, session, intent)
	if err != nil {
		t.Fatal(err)
	}
	operationID := handle.state.OperationID
	if handle.state.Phase != PhaseStageCreated || handle.state.OperationRootIdentity != handle.subtree.Identity().String() {
		t.Fatalf("unexpected initial journal state: %#v", handle.state)
	}
	if !creation.SubtreeCreated || creation.DirectoriesCreated != 3 || len(creation.Objects) != 3 {
		t.Fatalf("unexpected durable journal creation receipt: %#v", creation)
	}
	for _, receipt := range creation.Objects {
		if !receipt.Published || receipt.Durability != "confirmed" || receipt.Identity == "" {
			t.Fatalf("journal write lacked publication assurance: %#v", receipt)
		}
	}
	if err := handle.subtree.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openJournal(ctx, session, operationID, intent.Limits)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.subtree.Close()
	if reopened.state.Phase != PhaseStageCreated || reopened.state.LastSequence != 1 ||
		reopened.state.OperationRootIdentity != reopened.subtree.Identity().String() {
		t.Fatalf("replayed state differs: %#v", reopened.state)
	}
}

func TestCreateJournalHonorsPreCanceledContextBeforeAnyWrite(t *testing.T) {
	session, info, root := bindMaterializeTestRoot(t)
	intent := testIntent()
	intent.TargetRootIdentity = info.Identity.String()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handle, creation, err := createJournal(ctx, session, intent)
	if !errors.Is(err, context.Canceled) || handle != nil || creation.SubtreeCreated ||
		creation.DirectoriesCreated != 0 || len(creation.Objects) != 0 {
		t.Fatalf("pre-canceled journal creation performed a write: %#v %#v %v", handle, creation, err)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("pre-canceled journal creation changed the target root: %#v %v", entries, readErr)
	}
}

func TestOpenJournalRejectsTamperedEvent(t *testing.T) {
	ctx := context.Background()
	session, info, root := bindMaterializeTestRoot(t)
	intent := testIntent()
	intent.TargetRootIdentity = info.Identity.String()
	handle, _, err := createJournal(ctx, session, intent)
	if err != nil {
		t.Fatal(err)
	}
	operationID := handle.state.OperationID
	lastName, err := EventFileName(handle.state.LastSequence, handle.state.LastEventID)
	if err != nil {
		t.Fatal(err)
	}
	directoryName, _ := OperationDirectoryName(operationID)
	if err := handle.subtree.Close(); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(root, directoryName, journalDirectoryName, lastName)
	if err := os.WriteFile(eventPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openJournal(ctx, session, operationID, intent.Limits); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("tampered event was not rejected as corrupt: %v", err)
	}
}

func TestOpenJournalRejectsWholeSubtreeReplacement(t *testing.T) {
	ctx := context.Background()
	session, info, root := bindMaterializeTestRoot(t)
	intent := testIntent()
	intent.TargetRootIdentity = info.Identity.String()
	handle, _, err := createJournal(ctx, session, intent)
	if err != nil {
		t.Fatal(err)
	}
	operationID := handle.state.OperationID
	directoryName, _ := OperationDirectoryName(operationID)
	if err := handle.subtree.Close(); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(root, directoryName)
	replacement := filepath.Join(root, "replacement")
	if err := copyDirectoryForIdentityTest(original, replacement); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "original-backup")
	if err := os.Rename(original, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, original); err != nil {
		t.Fatal(err)
	}
	if _, err := openJournal(ctx, session, operationID, intent.Limits); err == nil {
		t.Fatalf("replacement subtree retained journal authority: %v", err)
	}
}

func TestOpenJournalRejectsNamedEventReplacementAfterFirstRead(t *testing.T) {
	ctx := context.Background()
	session, info, root := bindMaterializeTestRoot(t)
	intent := testIntent()
	intent.TargetRootIdentity = info.Identity.String()
	handle, _, err := createJournal(ctx, session, intent)
	if err != nil {
		t.Fatal(err)
	}
	operationID := handle.state.OperationID
	directoryName, _ := OperationDirectoryName(operationID)
	journalRoot := filepath.Join(root, directoryName, journalDirectoryName)
	journalPath, _ := fsbind.PathFromComponents([]string{journalDirectoryName})
	listing, err := handle.subtree.List(ctx, journalPath, fsbind.ListLimits{MaxEntries: 8, MaxNameBytes: 1024})
	if err != nil || !listing.Complete || len(listing.Entries) == 0 {
		t.Fatalf("read journal entries: %v", err)
	}
	eventPath := filepath.Join(journalRoot, listing.Entries[0].Name)
	if err := handle.subtree.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	replaced := false
	journalReplayReadHook = func(kind string, position int) error {
		if kind != "event" || position != 0 || replaced {
			return nil
		}
		replaced = true
		if err := os.Rename(eventPath, filepath.Join(root, "detached-event")); err != nil {
			return err
		}
		return os.WriteFile(eventPath, raw, 0o600)
	}
	t.Cleanup(func() { journalReplayReadHook = nil })
	if _, err := openJournal(ctx, session, operationID, intent.Limits); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("same-byte named event replacement was accepted: %v", err)
	}
	journalReplayReadHook = nil
}

func copyDirectoryForIdentityTest(source, destination string) error {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.Mkdir(target, 0o700)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	})
}
