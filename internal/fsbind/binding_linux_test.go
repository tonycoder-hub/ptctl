//go:build linux

package fsbind

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxParentRenameAndLocatorReplacementCannotPublishToDetachedRoot(t *testing.T) {
	container, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	locatorParent := filepath.Join(container, "locator-parent")
	targetRoot := filepath.Join(locatorParent, "target")
	if err := os.MkdirAll(targetRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	session, _, err := BindExisting(targetRoot)
	if errors.Is(err, ErrUnsupported) {
		t.Skip("test filesystem is not an allowlisted Linux target filesystem")
	}
	if err != nil {
		t.Fatalf("BindExisting: %v", err)
	}
	defer session.Close()

	subtree, err := session.CreatePrivateSubtree("parent-swap-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	source := mustPath(t, "payload.tmp")
	file, err := subtree.CreateRegular(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	parentComponentIndex := len(strings.Split(strings.TrimPrefix(filepath.Clean(locatorParent), string(filepath.Separator)), string(filepath.Separator))) - 1
	movedParent := locatorParent + "-moved"
	previousHook := linuxAbsoluteOpenHook
	var hookErr error
	hookRan := false
	linuxAbsoluteOpenHook = func(componentIndex int) {
		if hookRan || componentIndex != parentComponentIndex {
			return
		}
		hookRan = true
		if err := os.Rename(locatorParent, movedParent); err != nil {
			hookErr = err
			return
		}
		if err := os.Mkdir(locatorParent, 0o700); err != nil {
			hookErr = err
			return
		}
		if err := os.Mkdir(targetRoot, 0o700); err != nil {
			hookErr = err
		}
	}
	t.Cleanup(func() { linuxAbsoluteOpenHook = previousHook })

	receipt, publishErr := session.PublishNoReplace(context.Background(), subtree, source, "published.bin")
	linuxAbsoluteOpenHook = previousHook
	if hookErr != nil {
		t.Fatalf("replace locator parent: %v", hookErr)
	}
	if !hookRan {
		t.Fatal("absolute traversal replacement seam did not run")
	}
	if !errors.Is(publishErr, ErrBindingChanged) || receipt.Attempted || receipt.Published {
		t.Fatalf("publication after locator replacement: receipt=%+v err=%v", receipt, publishErr)
	}
	for _, path := range []string{
		filepath.Join(movedParent, "target", "published.bin"),
		filepath.Join(targetRoot, "published.bin"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("publication escaped binding check: %v", err)
		}
	}
}

func TestLinuxOperationLockRenameReplacementInvalidatesAuthorityAndStaysBusy(t *testing.T) {
	root, firstSession := newSupportedSession(t)
	first, err := firstSession.CreatePrivateSubtree("lock-name-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	lockPath := filepath.Join(root, "lock-name-operation", operationLockName)
	movedLockPath := lockPath + ".moved"
	if err := os.Rename(lockPath, movedLockPath); err != nil {
		t.Fatalf("rename held operation lock: %v", err)
	}
	replacement, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("create replacement operation lock: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}

	if err := first.Check(); !errors.Is(err, ErrBindingChanged) {
		t.Fatalf("renamed lock retained authority: %v", err)
	}
	secondSession, _, err := BindExisting(root)
	if err != nil {
		t.Fatal(err)
	}
	defer secondSession.Close()
	if _, err := secondSession.OpenPrivateSubtree("lock-name-operation", first.Identity()); !errors.Is(err, ErrBusy) {
		t.Fatalf("second session acquired replacement lock inode: %v", err)
	}
}
