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

func TestBoundRootRegularRemovalUsesExactNameIdentityAndSize(t *testing.T) {
	root, session := newSupportedSession(t)
	ctx := context.Background()
	original := filepath.Join(root, "original.bin")
	selected := filepath.Join(root, "selected.bin")
	if err := os.WriteFile(original, []byte("verified bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(original, selected); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}

	observed, err := session.InspectRootRegular(ctx, "selected.bin")
	if err != nil || observed.Kind != ObjectKindRegular || observed.SizeBytes != int64(len("verified bytes")) || observed.Identity.IsZero() {
		t.Fatalf("InspectRootRegular = %+v, %v", observed, err)
	}
	reader, err := session.OpenRootRegular(ctx, "selected.bin")
	if err != nil {
		t.Fatalf("OpenRootRegular: %v", err)
	}
	if info, err := reader.Info(); err != nil || !info.Identity.Equal(observed.Identity) || info.SizeBytes != observed.SizeBytes {
		t.Fatalf("bound reader = %+v, %v", info, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if receipt, err := session.RemoveRootRegularExact(cancelled, "selected.bin", observed.Identity, observed.SizeBytes); !errors.Is(err, context.Canceled) || receipt.Attempted {
		t.Fatalf("pre-cancelled root removal crossed attempt boundary: %+v, %v", receipt, err)
	}
	if receipt, err := session.RemoveRootRegularExact(ctx, "selected.bin", observed.Identity, observed.SizeBytes); err == nil || receipt.Attempted {
		t.Fatalf("open bound handle did not block removal: %+v, %v", receipt, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other.bin")
	if err := os.WriteFile(other, []byte("verified bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	otherInfo, err := session.InspectRootRegular(ctx, "other.bin")
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := session.RemoveRootRegularExact(ctx, "selected.bin", otherInfo.Identity, observed.SizeBytes); !errors.Is(err, ErrUnsafeObject) || receipt.Attempted {
		t.Fatalf("wrong identity reached unlink: %+v, %v", receipt, err)
	}
	if receipt, err := session.RemoveRootRegularExact(ctx, "selected.bin", observed.Identity, observed.SizeBytes+1); !errors.Is(err, ErrUnsafeObject) || receipt.Attempted {
		t.Fatalf("wrong size reached unlink: %+v, %v", receipt, err)
	}
	receipt, err := session.RemoveRootRegularExact(ctx, "selected.bin", observed.Identity, observed.SizeBytes)
	if err != nil || !receipt.Attempted || !receipt.Removed || receipt.Durability != DurabilityConfirmed {
		t.Fatalf("exact removal = %+v, %v", receipt, err)
	}
	if _, err := os.Lstat(selected); !os.IsNotExist(err) {
		t.Fatalf("selected name remains after removal: %v", err)
	}
	if raw, err := os.ReadFile(original); err != nil || string(raw) != "verified bytes" {
		t.Fatalf("unselected hardlink changed: %q, %v", raw, err)
	}
	if _, err := session.InspectRootRegular(ctx, "selected.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed name observation = %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := session.InspectRootRegular(ctx, "directory"); !errors.Is(err, ErrUnsafeObject) {
		t.Fatalf("directory accepted as root regular: %v", err)
	}
	symlink := filepath.Join(root, "symlink")
	if err := os.Symlink(other, symlink); err == nil {
		if _, err := session.InspectRootRegular(ctx, "symlink"); !errors.Is(err, ErrUnsafeObject) {
			t.Fatalf("link accepted as root regular: %v", err)
		}
	}
}

func TestBoundRootEmptyDirectoryRemovalRequiresExactIdentityAndEmptyNamespace(t *testing.T) {
	root, session := newSupportedSession(t)
	ctx := context.Background()
	selected := filepath.Join(root, "selected-directory")
	other := filepath.Join(root, "other-directory")
	if err := os.Mkdir(selected, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	observed, err := session.InspectRoot(ctx, "selected-directory")
	if err != nil || observed.Kind != ObjectKindDirectory || observed.Identity.IsZero() {
		t.Fatalf("InspectRoot selected directory = %+v, %v", observed, err)
	}
	otherObserved, err := session.InspectRoot(ctx, "other-directory")
	if err != nil || otherObserved.Kind != ObjectKindDirectory || otherObserved.Identity.IsZero() {
		t.Fatalf("InspectRoot other directory = %+v, %v", otherObserved, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if receipt, err := session.RemoveRootEmptyDirectoryExact(cancelled, "selected-directory", observed.Identity); !errors.Is(err, context.Canceled) || receipt.Attempted {
		t.Fatalf("pre-cancelled directory removal crossed attempt boundary: %+v, %v", receipt, err)
	}
	if receipt, err := session.RemoveRootEmptyDirectoryExact(ctx, "selected-directory", otherObserved.Identity); !errors.Is(err, ErrUnsafeObject) || receipt.Attempted {
		t.Fatalf("wrong directory identity reached removal: %+v, %v", receipt, err)
	}
	child := filepath.Join(selected, "unrelated.bin")
	if err := os.WriteFile(child, []byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if receipt, err := session.RemoveRootEmptyDirectoryExact(ctx, "selected-directory", observed.Identity); !errors.Is(err, ErrNotEmpty) || receipt.Attempted ||
		!receipt.NamespaceRead || receipt.EntriesExamined != 1 || receipt.NameBytesExamined != int64(len("unrelated.bin")) {
		t.Fatalf("nonempty directory reached removal: %+v, %v", receipt, err)
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	receipt, err := session.RemoveRootEmptyDirectoryExact(ctx, "selected-directory", observed.Identity)
	if err != nil || !receipt.Attempted || !receipt.Removed || receipt.Durability != DurabilityConfirmed ||
		receipt.Kind != ObjectKindDirectory || !receipt.Identity.Equal(observed.Identity) {
		t.Fatalf("exact empty-directory removal = %+v, %v", receipt, err)
	}
	if _, err := os.Lstat(selected); !os.IsNotExist(err) {
		t.Fatalf("selected directory remains after removal: %v", err)
	}
	if info, err := os.Stat(other); err != nil || !info.IsDir() {
		t.Fatalf("unselected directory changed: %v", err)
	}

	raceName := "race-directory"
	racePath := filepath.Join(root, raceName)
	oldRacePath := filepath.Join(root, "race-directory-old")
	if err := os.Mkdir(racePath, 0o755); err != nil {
		t.Fatal(err)
	}
	raceObserved, err := session.InspectRoot(ctx, raceName)
	if err != nil {
		t.Fatal(err)
	}
	mutated := false
	defer func() { checkHook = nil }()
	checkHook = func(stage string, _ *Session) error {
		if stage == "after_root_empty_directory_list" && !mutated {
			mutated = true
			if err := os.Rename(racePath, oldRacePath); err != nil {
				t.Fatalf("rename reviewed directory: %v", err)
			}
			if err := os.Mkdir(racePath, 0o755); err != nil {
				t.Fatalf("replace reviewed directory: %v", err)
			}
		}
		return nil
	}
	receipt, err = session.RemoveRootEmptyDirectoryExact(ctx, raceName, raceObserved.Identity)
	checkHook = nil
	if !errors.Is(err, ErrUnsafeObject) || !receipt.Attempted || receipt.Removed {
		t.Fatalf("directory identity swap reached removal: %+v, %v", receipt, err)
	}
	for _, retained := range []string{racePath, oldRacePath} {
		if info, err := os.Stat(retained); err != nil || !info.IsDir() {
			t.Fatalf("identity-swap guard removed %q: %v", retained, err)
		}
	}

	concurrentName := "concurrently-removed-directory"
	concurrentPath := filepath.Join(root, concurrentName)
	if err := os.Mkdir(concurrentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	concurrentObserved, err := session.InspectRoot(ctx, concurrentName)
	if err != nil {
		t.Fatal(err)
	}
	checkHook = func(stage string, _ *Session) error {
		if stage == "after_root_empty_directory_list" {
			return os.Remove(concurrentPath)
		}
		return nil
	}
	receipt, err = session.RemoveRootEmptyDirectoryExact(ctx, concurrentName, concurrentObserved.Identity)
	checkHook = nil
	if !errors.Is(err, ErrNotFound) || !receipt.Attempted || receipt.Removed {
		t.Fatalf("concurrent absence was misclassified: %+v, %v", receipt, err)
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
