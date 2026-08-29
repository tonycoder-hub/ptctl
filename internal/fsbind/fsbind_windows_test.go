//go:build windows

package fsbind

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsHandleRelativeSessionDetectsNamedRootSwap(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "guarded")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	session, _, err := BindExisting(root)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(parent, "replacement")
	if err := os.Rename(root, replacement); err != nil {
		t.Fatalf("test could not swap named root: %v", err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := session.Check(); !errors.Is(err, ErrBindingChanged) {
		t.Fatalf("named root swap check = %v", err)
	}
	if _, err := session.CreatePrivateSubtree("must-not-write"); !errors.Is(err, ErrBindingChanged) {
		t.Fatalf("operation after named root swap = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "must-not-write")); !os.IsNotExist(err) {
		t.Fatalf("replacement root was mutated: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsRejectsReparseAndExistingDestination(t *testing.T) {
	root, session := newSupportedSession(t)
	subtree, err := session.CreatePrivateSubtree("operation")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	outside := filepath.Join(filepath.Dir(root), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "operation", "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Logf("reparse subcase skipped (developer mode unavailable): %v", err)
	} else {
		if _, err := subtree.OpenRegular(context.Background(), mustPath(t, "link")); !errors.Is(err, ErrUnsafeObject) {
			t.Fatalf("reparse open = %v, want ErrUnsafeObject", err)
		}
	}

	file, err := subtree.CreateRegular(context.Background(), mustPath(t, "source"))
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if err := os.WriteFile(filepath.Join(root, "occupied"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt, err := session.PublishNoReplace(context.Background(), subtree, mustPath(t, "source"), "occupied")
	if !errors.Is(err, ErrAlreadyExists) || receipt.Published {
		t.Fatalf("existing destination: %+v %v", receipt, err)
	}
}

func TestWindowsDirectoryPublicationAndBoundReopen(t *testing.T) {
	_, session := newSupportedSession(t)
	subtree, err := session.CreatePrivateSubtree("directory-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	if _, err := subtree.MkdirAll(context.Background(), mustPath(t, "payload", "nested")); err != nil {
		t.Fatal(err)
	}
	file, err := subtree.CreateRegular(context.Background(), mustPath(t, "payload", "nested", "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("multi-file"))
	_ = file.Sync()
	_ = file.Close()
	receipt, err := session.PublishNoReplace(context.Background(), subtree, mustPath(t, "payload"), "published-directory")
	if err != nil || !receipt.Published {
		t.Fatalf("directory publish: %+v %v", receipt, err)
	}
	published, err := session.OpenPublishedRoot("published-directory", receipt.FinalIdentity)
	if err != nil || published.Kind() != ObjectKindDirectory {
		t.Fatalf("open published directory: %v", err)
	}
	defer published.Close()
	opened, err := published.OpenRegular(context.Background(), mustPath(t, "nested", "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	buffer := make([]byte, len("multi-file"))
	if _, err := opened.Read(buffer); err != nil || string(buffer) != "multi-file" {
		t.Fatalf("read = %q %v", buffer, err)
	}
}

func TestWindowsRegularPublicationAcrossBoundDirectories(t *testing.T) {
	_, session := newSupportedSession(t)
	subtree, err := session.CreatePrivateSubtree("regular-cross-directory-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	if _, err := subtree.MkdirAll(context.Background(), mustPath(t, "scratch")); err != nil {
		t.Fatal(err)
	}
	if _, err := subtree.MkdirAll(context.Background(), mustPath(t, "stage", "bundle")); err != nil {
		t.Fatal(err)
	}
	if _, err := subtree.Inspect(context.Background(), mustPath(t, "stage", "bundle", "a")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unexpected preexisting destination: %v", err)
	}
	file, err := subtree.CreateRegular(context.Background(), mustPath(t, "scratch", "copy-000000000-00000000000000000000000000000000.pending"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Info(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	receipt, err := subtree.CommitRegularNoReplace(context.Background(),
		mustPath(t, "scratch", "copy-000000000-00000000000000000000000000000000.pending"), mustPath(t, "stage", "bundle", "a"))
	if err != nil || !receipt.Published || receipt.Durability != durabilityConfirmed {
		t.Fatalf("cross-directory regular publication: %+v %v", receipt, err)
	}
}

func TestWindowsAmbiguousMovePreservesPublicationAndBindingErrors(t *testing.T) {
	_, session := newSupportedSession(t)
	subtree, err := session.CreatePrivateSubtree("ambiguous-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	file, err := subtree.CreateRegular(context.Background(), mustPath(t, "source"))
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	previousMove := windowsRenameRelative
	previousHook := checkHook
	windowsRenameRelative = func(source, destination windows.Handle, name string) error {
		if err := previousMove(source, destination, name); err != nil {
			return err
		}
		return windows.ERROR_GEN_FAILURE
	}
	checkHook = func(stage string, _ *Session) error {
		if stage == "after_publish" {
			return errors.New("injected binding change")
		}
		return nil
	}
	t.Cleanup(func() {
		windowsRenameRelative = previousMove
		checkHook = previousHook
	})
	receipt, err := session.PublishNoReplace(context.Background(), subtree, mustPath(t, "source"), "final")
	if !receipt.Published || receipt.Durability != durabilityUnconfirmed || !errors.Is(err, ErrDurabilityUnconfirmed) || !errors.Is(err, ErrBindingChanged) {
		t.Fatalf("ambiguous move receipt/error: %+v %v", receipt, err)
	}
}

func TestWindowsPreCancelledOperationDoesNotCreate(t *testing.T) {
	root, session := newSupportedSession(t)
	subtree, err := session.CreatePrivateSubtree("cancel-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := subtree.CreateRegular(ctx, mustPath(t, "never")); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "cancel-operation", "never")); !os.IsNotExist(err) {
		t.Fatalf("pre-cancel created an object: %v", err)
	}
}

func TestWindowsRootBindingSeamDuringHandleRelativePublishCannotWriteReplacement(t *testing.T) {
	root, session := newSupportedSession(t)
	subtree, err := session.CreatePrivateSubtree("swap-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	file, err := subtree.CreateRegular(context.Background(), mustPath(t, "source"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("bound"))
	_ = file.Sync()
	_ = file.Close()

	oldRename := windowsRenameRelative
	replacement := filepath.Join(filepath.Dir(root), "replacement-root")
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	originalGuardPath := session.platform.data.guards[len(session.platform.data.guards)-1].path
	windowsRenameRelative = func(source, destination windows.Handle, name string) error {
		session.platform.data.guards[len(session.platform.data.guards)-1].path = replacement
		return oldRename(source, destination, name)
	}
	t.Cleanup(func() {
		windowsRenameRelative = oldRename
		session.platform.data.guards[len(session.platform.data.guards)-1].path = originalGuardPath
	})
	receipt, err := session.PublishNoReplace(context.Background(), subtree, mustPath(t, "source"), "final")
	if !receipt.Published || !errors.Is(err, ErrBindingChanged) {
		t.Fatalf("root swap publication: %+v %v", receipt, err)
	}
	if _, err := os.Lstat(filepath.Join(replacement, "final")); !os.IsNotExist(err) {
		t.Fatalf("replacement binding received publication: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "final")); err != nil {
		t.Fatalf("bound original did not retain publication evidence: %v", err)
	}
}

func TestWindowsPublishedCheckRejectsFinalNameSwap(t *testing.T) {
	root, session := newSupportedSession(t)
	subtree, err := session.CreatePrivateSubtree("published-swap-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	file, err := subtree.CreateRegular(context.Background(), mustPath(t, "source"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("one"))
	_ = file.Sync()
	_ = file.Close()
	receipt, err := session.PublishNoReplace(context.Background(), subtree, mustPath(t, "source"), "final")
	if err != nil {
		t.Fatal(err)
	}
	published, err := session.OpenPublishedRoot("final", receipt.FinalIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer published.Close()
	if err := os.Rename(filepath.Join(root, "final"), filepath.Join(root, "moved-final")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "final"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := published.Check(); !errors.Is(err, ErrBindingChanged) {
		t.Fatalf("published final-name swap = %v", err)
	}
}

func TestWindowsMkdirReceiptPreservesCreatedCountOnDurabilityFailure(t *testing.T) {
	root, session := newSupportedSession(t)
	subtree, err := session.CreatePrivateSubtree("mkdir-receipt-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	oldFlush := windowsFlushFileBuffers
	windowsFlushFileBuffers = func(windows.Handle) error { return windows.ERROR_GEN_FAILURE }
	receipt, err := subtree.MkdirAll(context.Background(), mustPath(t, "visible-child"))
	windowsFlushFileBuffers = oldFlush
	if !errors.Is(err, ErrDurabilityUnconfirmed) || receipt.DirectoriesCreated != 1 || receipt.Durability != durabilityUnconfirmed {
		t.Fatalf("mkdir post-create receipt: %+v %v", receipt, err)
	}
	info, inspectErr := subtree.Inspect(context.Background(), mustPath(t, "visible-child"))
	if inspectErr != nil || info.Kind != ObjectKindDirectory {
		t.Fatalf("created directory evidence unavailable: %+v %v", info, inspectErr)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "mkdir-receipt-operation", "visible-child")); statErr != nil {
		t.Fatalf("visible created directory missing: %v", statErr)
	}
}

func TestWindowsFailedPrivateFileOwnerAssignmentCleansCreatedObject(t *testing.T) {
	root, session := newSupportedSession(t)
	subtree, err := session.CreatePrivateSubtree("file-cleanup-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	oldOwner := windowsSetPrivateOwner
	windowsSetPrivateOwner = func(windows.Handle) error { return windows.ERROR_PRIVILEGE_NOT_HELD }
	_, createErr := subtree.CreateRegular(context.Background(), mustPath(t, "must-not-remain"))
	windowsSetPrivateOwner = oldOwner
	if createErr == nil {
		t.Fatal("injected owner failure was ignored")
	}
	if _, err := os.Lstat(filepath.Join(root, "file-cleanup-operation", "must-not-remain")); !os.IsNotExist(err) {
		t.Fatalf("failed private creation left an object: %v", err)
	}
}
