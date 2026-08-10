package fsbind

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func mustPath(t *testing.T, components ...string) Path {
	t.Helper()
	path, err := PathFromComponents(components)
	if err != nil {
		t.Fatalf("PathFromComponents: %v", err)
	}
	return path
}

func TestIdentityPathAndListLimits(t *testing.T) {
	raw := rawIdentity{volume: 1, mount: 2, fileHigh: 3, fileLow: 4, directory: true}
	identity := identityFromRaw(raw)
	parsed, err := ParseIdentity(identity.String())
	if err != nil || !parsed.Equal(identity) {
		t.Fatalf("identity round trip failed: %v", err)
	}
	if _, err := ParseIdentity(strings.ToUpper(identity.String())); err == nil {
		t.Fatal("uppercase identity accepted")
	}
	encoded, err := json.Marshal(identity)
	if err != nil || !strings.Contains(string(encoded), identity.String()) {
		t.Fatalf("identity JSON failed: %s %v", encoded, err)
	}
	emptyReceipt, err := json.Marshal(Publication{Durability: DurabilityNotPublished})
	if err != nil || strings.Contains(string(emptyReceipt), "identity") {
		t.Fatalf("zero receipt identity was not safely omitted: %s %v", emptyReceipt, err)
	}
	for _, components := range [][]string{{""}, {"."}, {".."}, {"a/b"}, {"bad\x00name"}, {operationLockName}} {
		if _, err := PathFromComponents(components); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("invalid path accepted: %#v: %v", components, err)
		}
	}
	if err := DefaultListLimits().Validate(); err != nil {
		t.Fatalf("default list limits: %v", err)
	}
	if err := (ListLimits{MaxEntries: hardMaxListEntries + 1, MaxNameBytes: 1}).Validate(); err == nil {
		t.Fatal("hard entry limit accepted")
	}
}

func newSupportedSession(t *testing.T) (string, *Session) {
	t.Helper()
	parent := t.TempDir()
	root := parent + string(os.PathSeparator) + "target"
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	session, _, err := BindExisting(root)
	if errors.Is(err, ErrUnsupported) {
		t.Skipf("filesystem unsupported on %s", runtime.GOOS)
	}
	if err != nil {
		t.Fatalf("BindExisting: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return root, session
}

func TestWorkflowCommitInspectListLockAndPublishedOpen(t *testing.T) {
	_, session := newSupportedSession(t)
	ctx := context.Background()
	subtree, err := session.CreatePrivateSubtree(".ptctl-materialize-one")
	if err != nil {
		t.Fatalf("CreatePrivateSubtree: %v", err)
	}
	t.Cleanup(func() { _ = subtree.Close() })

	second, _, err := BindExisting(session.path)
	if err != nil {
		t.Fatalf("second BindExisting: %v", err)
	}
	defer second.Close()
	if _, err := second.OpenPrivateSubtree(".ptctl-materialize-one", subtree.Identity()); !errors.Is(err, ErrBusy) {
		t.Fatalf("second lock = %v, want ErrBusy", err)
	}

	if receipt, err := subtree.MkdirAll(ctx, mustPath(t, "scratch")); err != nil || receipt.DirectoriesCreated != 1 {
		t.Fatalf("MkdirAll: %+v %v", receipt, err)
	}
	source := mustPath(t, "scratch", "intent.tmp")
	file, err := subtree.CreateRegular(ctx, source)
	if err != nil {
		t.Fatalf("CreateRegular: %v", err)
	}
	if _, err := file.Write([]byte("verified payload")); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatalf("file sync: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	destination := mustPath(t, "scratch", "intent.json")
	publication, err := subtree.CommitRegularNoReplace(ctx, source, destination)
	if err != nil || !publication.Published || publication.Durability != durabilityConfirmed {
		t.Fatalf("CommitRegularNoReplace: %+v %v", publication, err)
	}
	if _, err := subtree.CommitRegularNoReplace(ctx, destination, destination); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("self/existing destination = %v", err)
	}
	object, err := subtree.Inspect(ctx, destination)
	if err != nil || object.Kind != ObjectKindRegular || object.SizeBytes != int64(len("verified payload")) || !object.Identity.Equal(publication.FinalIdentity) {
		t.Fatalf("Inspect: %+v %v", object, err)
	}
	listed, err := subtree.List(ctx, mustPath(t, "scratch"), DefaultListLimits())
	if err != nil || !listed.Complete || len(listed.Entries) != 1 || listed.Entries[0].Name != "intent.json" {
		t.Fatalf("List: %+v %v", listed, err)
	}

	payload, err := subtree.CreateRegular(ctx, mustPath(t, "payload.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := payload.Write([]byte("torrent bytes")); err != nil {
		t.Fatal(err)
	}
	if err := payload.Sync(); err != nil {
		t.Fatal(err)
	}
	payloadIdentity := payload.Identity()
	if err := payload.Close(); err != nil {
		t.Fatal(err)
	}
	rootPublication, err := session.PublishNoReplace(ctx, subtree, mustPath(t, "payload.tmp"), "published.bin")
	if err != nil || !rootPublication.Published || !rootPublication.FinalIdentity.Equal(payloadIdentity) {
		t.Fatalf("PublishNoReplace: %+v %v", rootPublication, err)
	}
	published, err := session.OpenPublishedRoot("published.bin", rootPublication.FinalIdentity)
	if err != nil {
		t.Fatalf("OpenPublishedRoot: %v", err)
	}
	defer published.Close()
	opened, err := published.OpenRegular(ctx, mustPath(t))
	if err != nil {
		t.Fatalf("Published.OpenRegular: %v", err)
	}
	data := make([]byte, len("torrent bytes"))
	if _, err := opened.Read(data); err != nil || string(data) != "torrent bytes" {
		t.Fatalf("published read: %q %v", data, err)
	}
	if _, err := opened.Write([]byte("must fail")); err == nil {
		t.Fatal("published verifier handle was writable")
	}
	_ = opened.Close()
	rootList, err := session.ListRoot(ctx, DefaultListLimits())
	if err != nil || !rootList.Complete || len(rootList.Entries) < 2 {
		t.Fatalf("ListRoot: %+v %v", rootList, err)
	}
	if err := session.SyncRoot(ctx); err != nil {
		t.Fatalf("SyncRoot: %v", err)
	}
}

func TestIdentityBoundRemovalReceipts(t *testing.T) {
	_, session := newSupportedSession(t)
	ctx := context.Background()
	subtree, err := session.CreatePrivateSubtree("removal-operation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subtree.Close() })
	if _, err := subtree.MkdirAll(ctx, mustPath(t, "retention")); err != nil {
		t.Fatal(err)
	}
	filePath := mustPath(t, "retention", "marker.tmp")
	file, err := subtree.CreateRegular(ctx, filePath)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("bounded private marker")
	if _, err := file.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	fileIdentity := file.Identity()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	wrongIdentity := identityFromRaw(rawIdentity{volume: 101, mount: 202, fileLow: 303})
	wrongReceipt, err := subtree.RemoveRegular(ctx, filePath, wrongIdentity)
	if !errors.Is(err, ErrUnsafeObject) || wrongReceipt.Attempted || wrongReceipt.Removed {
		t.Fatalf("wrong-identity removal crossed the attempt boundary: %+v %v", wrongReceipt, err)
	}
	if _, err := subtree.Inspect(ctx, filePath); err != nil {
		t.Fatalf("wrong-identity removal changed the object: %v", err)
	}
	wrongSize, err := subtree.RemoveRegularExact(ctx, filePath, fileIdentity, int64(len(payload)+1))
	if !errors.Is(err, ErrUnsafeObject) || wrongSize.Attempted || wrongSize.Removed {
		t.Fatalf("wrong-size removal crossed the attempt boundary: %+v %v", wrongSize, err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	cancelledReceipt, err := subtree.RemoveRegular(cancelled, filePath, fileIdentity)
	if !errors.Is(err, context.Canceled) || cancelledReceipt.Attempted || cancelledReceipt.Removed {
		t.Fatalf("pre-cancelled removal crossed the attempt boundary: %+v %v", cancelledReceipt, err)
	}

	removed, err := subtree.RemoveRegularExact(ctx, filePath, fileIdentity, int64(len(payload)))
	if err != nil || !removed.Attempted || !removed.Removed || removed.Durability != DurabilityConfirmed ||
		!removed.Identity.Equal(fileIdentity) || removed.Kind != ObjectKindRegular || removed.SizeBytes != int64(len(payload)) {
		t.Fatalf("regular removal receipt: %+v %v", removed, err)
	}
	if _, err := subtree.Inspect(ctx, filePath); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed regular remained visible: %v", err)
	}

	childPath := mustPath(t, "retention", "child")
	child, err := subtree.CreateRegular(ctx, childPath)
	if err != nil {
		t.Fatal(err)
	}
	childIdentity := child.Identity()
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	directoryPath := mustPath(t, "retention")
	directory, err := subtree.Inspect(ctx, directoryPath)
	if err != nil {
		t.Fatal(err)
	}
	nonempty, err := subtree.RemoveEmptyDirectory(ctx, directoryPath, directory.Identity)
	if err == nil || nonempty.Removed {
		t.Fatalf("nonempty directory was removed: %+v %v", nonempty, err)
	}
	if childRemoval, err := subtree.RemoveRegular(ctx, childPath, childIdentity); err != nil || !childRemoval.Removed {
		t.Fatalf("remove child after nonempty refusal: %+v %v", childRemoval, err)
	}
	directory, err = subtree.Inspect(ctx, directoryPath)
	if err != nil {
		t.Fatal(err)
	}
	directoryRemoval, err := subtree.RemoveEmptyDirectory(ctx, directoryPath, directory.Identity)
	if err != nil || !directoryRemoval.Attempted || !directoryRemoval.Removed || directoryRemoval.Durability != DurabilityConfirmed ||
		directoryRemoval.Kind != ObjectKindDirectory || directoryRemoval.SizeBytes != 0 {
		t.Fatalf("empty-directory removal receipt: %+v %v", directoryRemoval, err)
	}
	if _, err := subtree.Inspect(ctx, directoryPath); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed directory remained visible: %v", err)
	}
}

func TestCreationReceiptSurvivesPostCreateBindingFailure(t *testing.T) {
	_, session := newSupportedSession(t)
	previous := checkHook
	checkHook = func(stage string, _ *Session) error {
		if stage == "after_subtree_create" {
			return errors.New("injected")
		}
		return nil
	}
	_, receipt, err := session.CreatePrivateSubtreeWithReceipt("receipt-operation")
	checkHook = previous
	if !errors.Is(err, ErrBindingChanged) || !receipt.Created || receipt.Durability != durabilityConfirmed || receipt.Identity.IsZero() {
		t.Fatalf("creation receipt: %+v %v", receipt, err)
	}
	resumed, err := session.OpenPrivateSubtreeObserved("receipt-operation")
	if err != nil {
		t.Fatalf("observe created subtree: %v", err)
	}
	if !resumed.Identity().Equal(receipt.Identity) {
		t.Fatal("observed identity differs from creation receipt")
	}
	_ = resumed.Close()
}

func TestInspectRootAbsentAndPublicExisting(t *testing.T) {
	root, session := newSupportedSession(t)
	if _, err := session.InspectRoot(context.Background(), "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent root object: %v", err)
	}
	if err := os.WriteFile(root+string(os.PathSeparator)+"public-existing", []byte("public"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := session.InspectRoot(context.Background(), "public-existing")
	if err != nil || info.Kind != ObjectKindRegular || info.SizeBytes != 6 || info.Identity.IsZero() {
		t.Fatalf("public root inspection: %+v %v", info, err)
	}
}

func TestOperationLockReleasesWithSubtreeHandle(t *testing.T) {
	_, firstSession := newSupportedSession(t)
	first, err := firstSession.CreatePrivateSubtree("lock-operation")
	if err != nil {
		t.Fatal(err)
	}
	secondSession, _, err := BindExisting(firstSession.path)
	if err != nil {
		t.Fatal(err)
	}
	defer secondSession.Close()
	if _, err := secondSession.OpenPrivateSubtree("lock-operation", first.Identity()); !errors.Is(err, ErrBusy) {
		t.Fatalf("live operation lock = %v", err)
	}
	expected := first.Identity()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := secondSession.OpenPrivateSubtree("lock-operation", expected)
	if err != nil {
		t.Fatalf("released operation lock remained busy: %v", err)
	}
	_ = resumed.Close()
}

func TestOpenPrivateSubtreeNeverRecreatesMissingOperationLock(t *testing.T) {
	root, session := newSupportedSession(t)
	subtree, err := session.CreatePrivateSubtree("missing-lock-operation")
	if err != nil {
		t.Fatal(err)
	}
	identity := subtree.Identity()
	if err := subtree.Close(); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "missing-lock-operation", operationLockName)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if _, err := session.OpenPrivateSubtree("missing-lock-operation", identity); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing lock open = %v, want ErrNotFound", err)
	}
	if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("open repaired a missing lock: %v", err)
	}
}

func TestListBudgetAndBindingSeam(t *testing.T) {
	_, session := newSupportedSession(t)
	subtree, err := session.CreatePrivateSubtree(".ptctl-materialize-budget")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	for _, name := range []string{"b", "a"} {
		file, err := subtree.CreateRegular(context.Background(), mustPath(t, name))
		if err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
	}
	result, err := subtree.List(context.Background(), mustPath(t), ListLimits{MaxEntries: 1, MaxNameBytes: 1024})
	if err != nil || result.Complete || result.StopReason != "max_entries" || result.Used.EntriesExamined != 2 || result.Entries != nil {
		t.Fatalf("bounded list: %+v %v", result, err)
	}
	complete, err := subtree.List(context.Background(), mustPath(t), DefaultListLimits())
	if err != nil || !complete.Complete || len(complete.Entries) != 3 || complete.Entries[0].Name != operationLockName || complete.Entries[1].Name != "a" || complete.Entries[2].Name != "b" {
		t.Fatalf("deterministic list: %+v %v", complete, err)
	}
	previous := checkHook
	checkHook = func(stage string, _ *Session) error {
		if stage == "explicit_check" {
			return errors.New("injected")
		}
		return nil
	}
	t.Cleanup(func() { checkHook = previous })
	if err := session.Check(); !errors.Is(err, ErrBindingChanged) {
		t.Fatalf("binding seam: %v", err)
	}
}

func TestSubtreeExplicitCheckOwnsCachedDirectoryGraphScanAndDetectsSwap(t *testing.T) {
	root, session := newSupportedSession(t)
	subtree, err := session.CreatePrivateSubtree("directory-check-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	childPath := mustPath(t, "child")
	if _, err := subtree.MkdirAll(context.Background(), childPath); err != nil {
		t.Fatal(err)
	}

	previousHook := boundDirectoryCheckHook
	deepChecks := 0
	boundDirectoryCheckHook = func(string) { deepChecks++ }
	t.Cleanup(func() { boundDirectoryCheckHook = previousHook })
	if _, err := subtree.List(context.Background(), mustPath(t), DefaultListLimits()); err != nil {
		t.Fatalf("ordinary root list failed: %v", err)
	}
	if deepChecks != 0 {
		t.Fatalf("ordinary operation scanned %d cached descendants", deepChecks)
	}
	if err := subtree.Check(); err != nil || deepChecks != 1 {
		t.Fatalf("explicit initial check: scans=%d err=%v", deepChecks, err)
	}

	childAbsolute := filepath.Join(root, "directory-check-operation", "child")
	movedAbsolute := filepath.Join(root, "directory-check-operation", "moved-child")
	if err := os.Rename(childAbsolute, movedAbsolute); err != nil {
		t.Fatalf("rename cached child: %v", err)
	}
	replacement, created, durable, err := platformCreatePrivateDirectory(session, subtree.base, "child")
	if err != nil || !created || !durable {
		t.Fatalf("create replacement child: created=%t durable=%t err=%v", created, durable, err)
	}
	if err := replacement.file.Close(); err != nil {
		t.Fatal(err)
	}

	deepChecks = 0
	if _, err := subtree.List(context.Background(), mustPath(t), DefaultListLimits()); err != nil {
		t.Fatalf("ordinary operation unnecessarily rejected a cached child swap: %v", err)
	}
	if deepChecks != 0 {
		t.Fatalf("ordinary operation scanned %d cached descendants after swap", deepChecks)
	}
	if err := subtree.Check(); !errors.Is(err, ErrBindingChanged) || deepChecks != 1 {
		t.Fatalf("explicit check did not catch child swap: scans=%d err=%v", deepChecks, err)
	}
}

func TestSubtreeCheckPathsOnlyScansSelectedCachedAncestors(t *testing.T) {
	root, session := newSupportedSession(t)
	subtree, err := session.CreatePrivateSubtree("narrow-directory-check")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	selected := mustPath(t, "stage", "selected")
	unrelated := mustPath(t, "other", "cached")
	if _, err := subtree.MkdirAll(context.Background(), selected); err != nil {
		t.Fatal(err)
	}
	if _, err := subtree.MkdirAll(context.Background(), unrelated); err != nil {
		t.Fatal(err)
	}

	previousHook := boundDirectoryCheckHook
	checked := []string{}
	boundDirectoryCheckHook = func(key string) { checked = append(checked, key) }
	t.Cleanup(func() { boundDirectoryCheckHook = previousHook })
	if err := subtree.CheckPaths(selected); err != nil {
		t.Fatalf("selected path check: %v", err)
	}
	want := []string{"stage", "stage\x00selected"}
	if len(checked) != len(want) || checked[0] != want[0] || checked[1] != want[1] {
		t.Fatalf("selected path checks = %#v, want %#v", checked, want)
	}

	unrelatedAbsolute := filepath.Join(root, "narrow-directory-check", "other", "cached")
	movedAbsolute := filepath.Join(root, "narrow-directory-check", "other", "moved-cached")
	if err := os.Rename(unrelatedAbsolute, movedAbsolute); err != nil {
		t.Fatalf("rename unrelated cached directory: %v", err)
	}
	replacement, created, durable, err := platformCreatePrivateDirectory(session, subtree.dirs["other"], "cached")
	if err != nil || !created || !durable {
		t.Fatalf("create unrelated replacement: created=%t durable=%t err=%v", created, durable, err)
	}
	if err := replacement.file.Close(); err != nil {
		t.Fatal(err)
	}

	checked = nil
	if err := subtree.CheckPaths(selected); err != nil {
		t.Fatalf("unrelated swap affected narrow check: %v", err)
	}
	if len(checked) != len(want) || checked[0] != want[0] || checked[1] != want[1] {
		t.Fatalf("narrow check scanned unrelated cache: %#v", checked)
	}
	if err := subtree.CheckPaths(unrelated); !errors.Is(err, ErrBindingChanged) {
		t.Fatalf("selected replaced directory was accepted: %v", err)
	}
	if err := subtree.CheckPaths(mustPath(t, "not-cached")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncached path check = %v, want ErrNotFound", err)
	}
	if err := subtree.CheckPaths(); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("empty path selection = %v, want ErrInvalidPath", err)
	}
	if err := subtree.CheckPaths(mustPath(t)); err != nil {
		t.Fatalf("explicit subtree-root check: %v", err)
	}
}

func TestPublishedExplicitCheckDetectsCachedChildSwap(t *testing.T) {
	root, session := newSupportedSession(t)
	subtree, err := session.CreatePrivateSubtree("published-directory-check")
	if err != nil {
		t.Fatal(err)
	}
	defer subtree.Close()
	if _, err := subtree.MkdirAll(context.Background(), mustPath(t, "payload", "child")); err != nil {
		t.Fatal(err)
	}
	receipt, err := session.PublishNoReplace(context.Background(), subtree, mustPath(t, "payload"), "published-tree")
	if err != nil {
		t.Fatal(err)
	}
	published, err := session.OpenPublishedRoot("published-tree", receipt.FinalIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer published.Close()
	if _, err := published.List(context.Background(), mustPath(t, "child"), DefaultListLimits()); err != nil {
		t.Fatalf("cache published child: %v", err)
	}

	childAbsolute := filepath.Join(root, "published-tree", "child")
	movedAbsolute := filepath.Join(root, "published-tree", "moved-child")
	if err := os.Rename(childAbsolute, movedAbsolute); err != nil {
		t.Fatalf("rename published child: %v", err)
	}
	replacement, created, durable, err := platformCreatePrivateDirectory(session, published.dirs[""], "child")
	if err != nil || !created || !durable {
		t.Fatalf("create published replacement: created=%t durable=%t err=%v", created, durable, err)
	}
	if err := replacement.file.Close(); err != nil {
		t.Fatal(err)
	}

	previousHook := boundDirectoryCheckHook
	deepChecks := 0
	boundDirectoryCheckHook = func(string) { deepChecks++ }
	t.Cleanup(func() { boundDirectoryCheckHook = previousHook })
	if _, err := published.List(context.Background(), mustPath(t), DefaultListLimits()); err != nil {
		t.Fatalf("ordinary published operation rejected cached child swap: %v", err)
	}
	if deepChecks != 0 {
		t.Fatalf("ordinary published operation scanned %d cached descendants", deepChecks)
	}
	if err := published.Check(); !errors.Is(err, ErrBindingChanged) || deepChecks != 1 {
		t.Fatalf("published explicit check did not catch child swap: scans=%d err=%v", deepChecks, err)
	}
}

func TestHandlesCloseBeforeRootRemoval(t *testing.T) {
	parent := t.TempDir()
	root := parent + string(os.PathSeparator) + "removable"
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	session, _, err := BindExisting(root)
	if errors.Is(err, ErrUnsupported) {
		t.Skip("unsupported filesystem")
	}
	if err != nil {
		t.Fatal(err)
	}
	subtree, err := session.CreatePrivateSubtree("operation")
	if err != nil {
		t.Fatal(err)
	}
	file, err := subtree.CreateRegular(context.Background(), mustPath(t, "file"))
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	_ = subtree.Close()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("root remained held open: %v", err)
	}
}

func TestSessionCloseClosesSubtreeAndPublishedFileAuthorities(t *testing.T) {
	parent := t.TempDir()
	root := parent + string(os.PathSeparator) + "authority-close"
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	session, _, err := BindExisting(root)
	if errors.Is(err, ErrUnsupported) {
		t.Skip("unsupported filesystem")
	}
	if err != nil {
		t.Fatal(err)
	}
	subtree, err := session.CreatePrivateSubtree("operation")
	if err != nil {
		t.Fatal(err)
	}
	file, err := subtree.CreateRegular(context.Background(), mustPath(t, "source"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("payload"))
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
	reader, err := published.OpenRegular(context.Background(), mustPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("published reader survived Session.Close: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("session-owned authority leaked a handle: %v", err)
	}
}
