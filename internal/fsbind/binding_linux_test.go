//go:build linux

package fsbind

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestPublishedViewToleratesReadableModesAndHardLinksButNotForeignWrite(t *testing.T) {
	root, session := newSupportedSession(t)
	ctx := context.Background()
	subtree, err := session.CreatePrivateSubtree("published-modes")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	if _, err := subtree.MkdirAll(ctx, mustPath(t, "payload", "child")); err != nil {
		t.Fatal(err)
	}
	file, err := subtree.CreateRegular(ctx, mustPath(t, "payload", "child", "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("published bytes")); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	receipt, err := session.PublishNoReplace(ctx, subtree, mustPath(t, "payload"), "published-tree")
	if err != nil || !receipt.Published {
		t.Fatalf("PublishNoReplace: %+v %v", receipt, err)
	}
	final := filepath.Join(root, "published-tree")
	child := filepath.Join(final, "child")
	data := filepath.Join(child, "data.bin")
	chmod := func(path string, mode os.FileMode) {
		t.Helper()
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	chmod(final, 0o755)
	chmod(child, 0o750)
	chmod(data, 0o444)
	if err := os.Link(data, filepath.Join(root, "cross-seed.bin")); err != nil {
		t.Skipf("hard links are unavailable on the test filesystem: %v", err)
	}

	published, err := session.OpenPublishedRoot("published-tree", receipt.FinalIdentity)
	if err != nil {
		t.Fatalf("readable hard-linked publication was rejected: %v", err)
	}
	defer published.Close()
	opened, err := published.OpenRegular(ctx, mustPath(t, "child", "data.bin"))
	if err != nil {
		t.Fatalf("published OpenRegular: %v", err)
	}
	content := make([]byte, len("published bytes"))
	_, readErr := opened.Read(content)
	_ = opened.Close()
	if readErr != nil || string(content) != "published bytes" {
		t.Fatalf("published read: %q %v", content, readErr)
	}
	if info, err := published.Inspect(ctx, mustPath(t, "child", "data.bin")); err != nil || info.Kind != ObjectKindRegular {
		t.Fatalf("published Inspect: %+v %v", info, err)
	}
	if err := published.Check(); err != nil {
		t.Fatalf("published Check: %v", err)
	}

	chmod(data, 0o664)
	if _, err := published.OpenRegular(ctx, mustPath(t, "child", "data.bin")); !errors.Is(err, ErrUnsafeObject) {
		t.Fatalf("group-writable published file = %v, want ErrUnsafeObject", err)
	}
	chmod(data, 0o444)
	chmod(child, 0o757)
	if err := published.Check(); !errors.Is(err, ErrBindingChanged) {
		t.Fatalf("world-writable published directory = %v, want ErrBindingChanged", err)
	}
	chmod(child, 0o750)
	chmod(final, 0o777)
	if _, err := session.OpenPublishedRoot("published-tree", receipt.FinalIdentity); !errors.Is(err, ErrUnsafeObject) {
		t.Fatalf("world-writable published root = %v, want ErrUnsafeObject", err)
	}
	chmod(final, 0o755)

	private, err := subtree.CreateRegular(ctx, mustPath(t, "private.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := private.Close(); err != nil {
		t.Fatal(err)
	}
	chmod(filepath.Join(root, "published-modes", "private.bin"), 0o644)
	if _, err := subtree.Inspect(ctx, mustPath(t, "private.bin")); !errors.Is(err, ErrUnsafeObject) {
		t.Fatalf("readable in-flight private object = %v, want ErrUnsafeObject", err)
	}
}

func TestLinuxFIFOAtReviewedNamesIsRejectedWithoutBlocking(t *testing.T) {
	root, session := newSupportedSession(t)
	ctx := context.Background()
	subtree, err := session.CreatePrivateSubtree("fifo-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	if _, err := subtree.MkdirAll(ctx, mustPath(t, "payload")); err != nil {
		t.Fatal(err)
	}
	receipt, err := session.PublishNoReplace(ctx, subtree, mustPath(t, "payload"), "published-tree")
	if err != nil || !receipt.Published {
		t.Fatalf("PublishNoReplace: %+v %v", receipt, err)
	}
	for _, path := range []string{
		filepath.Join(root, "planted-fifo"),
		filepath.Join(root, "published-tree", "pipe"),
		filepath.Join(root, "fifo-operation", "pipe"),
	} {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skipf("FIFOs are unavailable on the test filesystem: %v", err)
		}
	}
	published, err := session.OpenPublishedRoot("published-tree", receipt.FinalIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer published.Close()

	rejects := func(label string, call func() error) {
		t.Helper()
		result := make(chan error, 1)
		go func() { result <- call() }()
		select {
		case err := <-result:
			if err == nil {
				t.Fatalf("%s accepted a FIFO", label)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s blocked opening a FIFO", label)
		}
	}
	rejects("InspectRoot", func() error { _, err := session.InspectRoot(ctx, "planted-fifo"); return err })
	rejects("InspectRootRegular", func() error { _, err := session.InspectRootRegular(ctx, "planted-fifo"); return err })
	rejects("OpenRootRegular", func() error { _, err := session.OpenRootRegular(ctx, "planted-fifo"); return err })
	rejects("RemoveRootRegularExact", func() error {
		_, err := session.RemoveRootRegularExact(ctx, "planted-fifo", receipt.FinalIdentity, 0)
		return err
	})
	rejects("OpenPublishedRoot", func() error { _, err := session.OpenPublishedRoot("planted-fifo", receipt.FinalIdentity); return err })
	rejects("Published.OpenRegular", func() error { _, err := published.OpenRegular(ctx, mustPath(t, "pipe")); return err })
	rejects("Published.Inspect", func() error { _, err := published.Inspect(ctx, mustPath(t, "pipe")); return err })
	rejects("Subtree.Inspect", func() error { _, err := subtree.Inspect(ctx, mustPath(t, "pipe")); return err })
	rejects("Subtree.OpenRegular", func() error { _, err := subtree.OpenRegular(ctx, mustPath(t, "pipe")); return err })
	if info, err := os.Lstat(filepath.Join(root, "planted-fifo")); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("rejected FIFO was changed: %v", err)
	}
}

func TestLinuxRootDiscoveryCountsUnrepresentableNamesWhileStrictListingsFailClosed(t *testing.T) {
	root, session := newSupportedSession(t)
	ctx := context.Background()
	for _, name := range []string{"ok.bin", "legacy-\xff.bin", "control-\x01.bin"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if listing, err := session.ListRoot(ctx, DefaultListLimits()); !errors.Is(err, ErrUnsafeObject) || listing.Complete || listing.StopReason != "unsafe_entry" {
		t.Fatalf("strict root listing accepted an unrepresentable name: %+v, %v", listing, err)
	}
	listing, err := session.ListRootForDiscovery(ctx, DefaultListLimits())
	if err != nil || !listing.Complete || len(listing.Entries) != 1 || listing.Entries[0].Name != "ok.bin" ||
		listing.Used.EntriesExamined != 3 || listing.Used.Unrepresentable != 2 {
		t.Fatalf("discovery listing = %+v, %v", listing, err)
	}
	bounded, err := session.ListRootForDiscovery(ctx, ListLimits{MaxEntries: 2, MaxNameBytes: 1 << 10})
	if err != nil || bounded.Complete || bounded.StopReason != "max_entries" {
		t.Fatalf("skipped names escaped the entry budget: %+v, %v", bounded, err)
	}

	subtree, err := session.CreatePrivateSubtree("strict-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	if err := os.WriteFile(filepath.Join(root, "strict-operation", "legacy-\xff.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := subtree.List(ctx, Path{}, DefaultListLimits()); !errors.Is(err, ErrUnsafeObject) {
		t.Fatalf("private subtree listing accepted an unrepresentable name: %v", err)
	}
}

func TestLinuxExactRemovalRechecksHeldIdentityAroundUnlink(t *testing.T) {
	root, session := newSupportedSession(t)
	ctx := context.Background()
	previousHook := linuxRemoveHook
	t.Cleanup(func() { linuxRemoveHook = previousHook })
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	readBack := func(name string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(raw)
	}
	var hookErr error
	swapAt := func(stage, name, moved string) {
		hookErr = nil
		ran := false
		linuxRemoveHook = func(current string) {
			if ran || current != stage {
				return
			}
			ran = true
			if err := os.Rename(filepath.Join(root, name), filepath.Join(root, moved)); err != nil {
				hookErr = err
				return
			}
			hookErr = os.WriteFile(filepath.Join(root, name), []byte("intruder bytes"), 0o600)
		}
	}

	write("before.bin", "verified bytes")
	before, err := session.InspectRootRegular(ctx, "before.bin")
	if err != nil {
		t.Fatal(err)
	}
	swapAt("verified", "before.bin", "before-moved.bin")
	receipt, err := session.RemoveRootRegularExact(ctx, "before.bin", before.Identity, before.SizeBytes)
	linuxRemoveHook = previousHook
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !errors.Is(err, ErrUnsafeObject) || receipt.Removed {
		t.Fatalf("name replaced after verification reached unlink: %+v, %v", receipt, err)
	}
	if readBack("before.bin") != "intruder bytes" || readBack("before-moved.bin") != "verified bytes" {
		t.Fatal("rejected removal changed either object")
	}

	write("during.bin", "verified bytes")
	during, err := session.InspectRootRegular(ctx, "during.bin")
	if err != nil {
		t.Fatal(err)
	}
	swapAt("unlink", "during.bin", "during-moved.bin")
	receipt, err = session.RemoveRootRegularExact(ctx, "during.bin", during.Identity, during.SizeBytes)
	linuxRemoveHook = previousHook
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !errors.Is(err, ErrRemovalAmbiguous) || !receipt.Removed || receipt.Durability == DurabilityConfirmed {
		t.Fatalf("unproven root unlink reported as exact: %+v, %v", receipt, err)
	}
	if readBack("during-moved.bin") != "verified bytes" {
		t.Fatal("verified object was changed")
	}

	write("marker.bin", "verified bytes")
	marker, err := session.InspectRootRegular(ctx, "marker.bin")
	if err != nil {
		t.Fatal(err)
	}
	swapAt("unlink", "marker.bin", "marker-moved.bin")
	receipt, err = session.RemoveRootPrivateRegularExact(ctx, "marker.bin", marker.Identity, marker.SizeBytes)
	linuxRemoveHook = previousHook
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !errors.Is(err, ErrRemovalAmbiguous) || !receipt.Removed || receipt.Durability == DurabilityConfirmed {
		t.Fatalf("unproven private unlink reported as exact: %+v, %v", receipt, err)
	}
	if readBack("marker-moved.bin") != "verified bytes" {
		t.Fatal("verified marker was changed")
	}

	write("exact.bin", "verified bytes")
	exact, err := session.InspectRootRegular(ctx, "exact.bin")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err = session.RemoveRootRegularExact(ctx, "exact.bin", exact.Identity, exact.SizeBytes)
	if err != nil || !receipt.Removed || receipt.Durability != DurabilityConfirmed {
		t.Fatalf("exact removal = %+v, %v", receipt, err)
	}
}
