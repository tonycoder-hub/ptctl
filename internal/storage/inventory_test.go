package storage

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestInventoryCandidatesSkipsLinksAndCollapsesHardlinks(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "movie.bin")
	if err := os.WriteFile(original, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias.bin")
	hardlinkCreated := os.Link(original, alias) == nil
	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(outside, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked.bin")
	symlinkCreated := os.Symlink(outside, link) == nil

	result, err := InventoryCandidates(context.Background(), []string{root}, InventoryOptions{
		Limits:      DefaultInventoryLimits(),
		WantedSizes: []int64{7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.Candidates) != 1 {
		t.Fatalf("unexpected inventory: %#v", result)
	}
	if hardlinkCreated && result.Stats.AliasesCollapsed != 1 {
		t.Fatalf("hardlink alias was not collapsed: %#v", result.Stats)
	}
	if symlinkCreated && result.Stats.SymlinksSkipped == 0 && result.Stats.ReparseSkipped == 0 {
		t.Fatalf("link was not skipped: %#v", result.Stats)
	}
	resolved, err := result.Candidates[0].ResolveObservedRegular()
	if err != nil {
		t.Fatal(err)
	}
	canonicalOriginal, err := filepath.EvalSymlinks(original)
	if err != nil {
		t.Fatal(err)
	}
	canonicalAlias := ""
	if hardlinkCreated {
		canonicalAlias, err = filepath.EvalSymlinks(alias)
		if err != nil {
			t.Fatal(err)
		}
	}
	if resolved != canonicalOriginal && (!hardlinkCreated || resolved != canonicalAlias) {
		t.Fatalf("unexpected resolved candidate %q", resolved)
	}
	if result.Candidates[0].RelativePath == "" || len(result.Candidates[0].RelativeComponentsRawBase64) != 1 {
		t.Fatalf("candidate lacks auditable relative identity: %#v", result.Candidates[0])
	}
}

func TestInventoryEntryBudgetIsIncompleteAndDeterministic(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"b.bin", "a.bin"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	limits := DefaultInventoryLimits()
	limits.MaxEntries = 1
	first, err := InventoryCandidates(context.Background(), []string{root}, InventoryOptions{Limits: limits, WantedSizes: []int64{1}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := InventoryCandidates(context.Background(), []string{root}, InventoryOptions{Limits: limits, WantedSizes: []int64{1}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Complete || second.Complete || !slices.Contains(first.LimitHits, "max_entries") {
		t.Fatalf("entry limit was not surfaced: %#v", first)
	}
	if len(first.Candidates) != 0 || len(second.Candidates) != 0 {
		t.Fatalf("a partially enumerated directory must not retain arbitrary entries")
	}
}

func TestObservedFileReplacementIsRejected(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "x.bin")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "replacement.bin")
	if err := os.WriteFile(replacement, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := InventoryCandidates(context.Background(), []string{root}, InventoryOptions{Limits: DefaultInventoryLimits(), WantedSizes: []int64{3}})
	if err != nil || len(result.Candidates) != 1 {
		t.Fatalf("inventory=%#v err=%v", result, err)
	}
	modified := result.Candidates[0].ModifiedAt
	backup := filepath.Join(root, "old.bin")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	if _, err := result.Candidates[0].ResolveObservedRegular(); err == nil {
		t.Fatal("same-size, same-mtime replacement matched the inventory identity")
	}
}

func TestOpenObservedRegularBindsScanIdentityAcrossParentReplacement(t *testing.T) {
	root := t.TempDir()
	originalDir := filepath.Join(root, "library")
	if err := os.Mkdir(originalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(originalDir, "x.bin")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := InventoryCandidates(context.Background(), []string{root}, InventoryOptions{Limits: DefaultInventoryLimits(), WantedSizes: []int64{3}})
	if err != nil || len(result.Candidates) != 1 {
		t.Fatalf("inventory=%#v err=%v", result, err)
	}
	file, err := result.Candidates[0].OpenObservedRegular()
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || string(data) != "old" {
		t.Fatalf("identity-bound read data=%q read=%v close=%v", data, readErr, closeErr)
	}

	moved := filepath.Join(root, "moved")
	if err := os.Rename(originalDir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(originalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(originalDir, "x.bin")
	if err := os.WriteFile(replacement, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, result.Candidates[0].ModifiedAt, result.Candidates[0].ModifiedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := result.Candidates[0].OpenObservedRegular(); err == nil {
		t.Fatal("parent replacement redirected an identity-bound proof open")
	}
}

func TestReadObservedDirectoryRejectsReplacementBeforeEnumeration(t *testing.T) {
	rootPath := t.TempDir()
	childPath := filepath.Join(rootPath, "child")
	if err := os.Mkdir(childPath, 0o700); err != nil {
		t.Fatal(err)
	}
	roots, issues, err := prepareSearchRoots(context.Background(), []string{rootPath}, false)
	if err != nil || len(issues) != 0 || len(roots) != 1 {
		t.Fatalf("roots=%#v issues=%#v err=%v", roots, issues, err)
	}
	childInfo, err := os.Lstat(childPath)
	if err != nil {
		t.Fatal(err)
	}
	childIdentity, ok := fileIdentity(childPath, childInfo)
	if !ok {
		t.Fatal("child directory identity is unavailable")
	}
	moved := filepath.Join(rootPath, "moved")
	if err := os.Rename(childPath, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(childPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readObservedDirectoryBounded(context.Background(), roots[0], directoryTask{rootIndex: 0, path: childPath, components: []string{"child"}, depth: 1, info: childInfo, identity: childIdentity}, 10); err == nil {
		t.Fatal("replacement directory was enumerated under a stale task identity")
	}
}

func TestInventoryKeepsUsableRootsButMarksUnavailableRootIncomplete(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	result, err := InventoryCandidates(context.Background(), []string{missing, root}, InventoryOptions{Limits: DefaultInventoryLimits(), WantedSizes: []int64{1}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || len(result.Candidates) != 1 || len(result.Issues) == 0 {
		t.Fatalf("partial multi-root inventory not represented: %#v", result)
	}
	statuses := map[string]int{}
	for _, observedRoot := range result.Roots {
		statuses[observedRoot.Status]++
	}
	if statuses["complete"] != 1 || statuses["unavailable"] != 1 {
		t.Fatalf("unexpected root statuses: %#v", statuses)
	}
}

func TestUnavailableRootIssueOrderDoesNotDependOnArgumentOrder(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingA := filepath.Join(t.TempDir(), "missing-a")
	missingB := filepath.Join(t.TempDir(), "missing-b")
	options := InventoryOptions{Limits: DefaultInventoryLimits(), WantedSizes: []int64{1}}
	first, err := InventoryCandidates(context.Background(), []string{missingB, root, missingA}, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := InventoryCandidates(context.Background(), []string{missingA, root, missingB}, options)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Issues, second.Issues) {
		t.Fatalf("unavailable-root issue order changed with argument order:\n%#v\n%#v", first.Issues, second.Issues)
	}
}

func TestInventoryRejectsOverlappingRoots(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := InventoryCandidates(context.Background(), []string{root, child}, InventoryOptions{Limits: DefaultInventoryLimits(), WantedSizes: []int64{1}})
	if err == nil {
		t.Fatal("overlapping roots were accepted")
	}
}

func TestInventoryCancellationProducesIncompleteObservation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "must-not-be-inspected")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := InventoryCandidates(ctx, []string{root}, InventoryOptions{Limits: DefaultInventoryLimits(), WantedSizes: []int64{1}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || !slices.Contains(result.LimitHits, "context_cancelled") {
		t.Fatalf("cancel was not represented as an incomplete scan: %#v", result)
	}
}

func TestInventoryDoesNotMutateTree(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "x")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = InventoryCandidates(context.Background(), []string{root}, InventoryOptions{Limits: DefaultInventoryLimits(), WantedSizes: []int64{1}})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) || before[0].Name() != after[0].Name() || beforeInfo.Size() != afterInfo.Size() || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("inventory mutated the search tree")
	}
}
