package materialize

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

func TestRetentionTreeInventoryPrecedesDeterministicRemoval(t *testing.T) {
	ctx := context.Background()
	session, _, _ := bindMaterializeTestRoot(t)
	subtree, err := session.CreatePrivateSubtree("retention-tree-operation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subtree.Close() })
	for _, components := range [][]string{{"stage", "a"}, {"stage", "nested", "b"}, {"scratch", "pending"}} {
		if len(components) > 1 {
			if _, err := subtree.MkdirAll(ctx, mustFSPath(t, components[:len(components)-1]...)); err != nil {
				t.Fatal(err)
			}
		}
		file, err := subtree.CreateRegular(ctx, mustFSPath(t, components...))
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
	}
	limits := RetentionLimits{MaxObjects: 16, MaxPathBytes: 1024, MaxBytes: 16, MaxMemoryBytes: 1 << 20, MaxDepth: 8, MaxFindings: 4}
	nodes, usage, err := inventoryRetentionTrees(ctx, subtree, []string{"stage", "scratch"}, limits)
	if err != nil || len(nodes) != 6 || usage.ObjectsConsidered != 6 || usage.BytesConsidered != 3 {
		t.Fatalf("inventory: nodes=%d usage=%#v err=%v", len(nodes), usage, err)
	}
	usage, err = removeRetentionNodes(ctx, subtree, nodes, usage)
	if err != nil || usage.FilesRemoved != 3 || usage.DirectoriesRemoved != 3 || usage.BytesRemoved != 3 || usage.RemovalAttempts != 6 {
		t.Fatalf("removal: usage=%#v err=%v", usage, err)
	}
	for _, name := range []string{"stage", "scratch"} {
		if _, err := subtree.Inspect(ctx, mustFSPath(t, name)); !errors.Is(err, fsbind.ErrNotFound) {
			t.Fatalf("removed tree %s remained: %v", name, err)
		}
	}
}

func TestRetentionTreeBudgetFailurePerformsNoRemoval(t *testing.T) {
	ctx := context.Background()
	session, _, _ := bindMaterializeTestRoot(t)
	subtree, err := session.CreatePrivateSubtree("retention-budget-operation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subtree.Close() })
	if _, err := subtree.MkdirAll(ctx, mustFSPath(t, "stage")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		file, err := subtree.CreateRegular(ctx, mustFSPath(t, "stage", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	limits := RetentionLimits{MaxObjects: 2, MaxPathBytes: 1024, MaxBytes: 16, MaxMemoryBytes: 1 << 20, MaxDepth: 8, MaxFindings: 4}
	nodes, usage, err := inventoryRetentionTrees(ctx, subtree, []string{"stage"}, limits)
	if !errors.Is(err, ErrPolicy) || nodes != nil || usage.RemovalAttempts != 0 {
		t.Fatalf("budget failure crossed deletion boundary: nodes=%#v usage=%#v err=%v", nodes, usage, err)
	}
	listing, err := subtree.List(ctx, mustFSPath(t, "stage"), fsbind.DefaultListLimits())
	if err != nil || len(listing.Entries) != 2 {
		t.Fatalf("budget failure changed the tree: %#v %v", listing, err)
	}
}

func TestRetentionTreeRejectsSizeGrowthBeforeRemoval(t *testing.T) {
	ctx := context.Background()
	session, _, _ := bindMaterializeTestRoot(t)
	subtree, err := session.CreatePrivateSubtree("retention-size-operation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subtree.Close() })
	path := mustFSPath(t, "payload")
	file, err := subtree.CreateRegular(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	limits := RetentionLimits{MaxObjects: 4, MaxPathBytes: 1024, MaxBytes: 16, MaxMemoryBytes: 1 << 20, MaxDepth: 8, MaxFindings: 4}
	nodes, usage, err := inventoryRetentionTrees(ctx, subtree, []string{"payload"}, limits)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := subtree.OpenRegular(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.Seek(0, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.Write([]byte("b")); err != nil {
		t.Fatal(err)
	}
	if err := opened.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	usage, err = removeRetentionNodes(ctx, subtree, nodes, usage)
	if !errors.Is(err, fsbind.ErrUnsafeObject) || usage.RemovalAttempts != 1 || usage.FilesRemoved != 0 {
		t.Fatalf("grown file crossed removal boundary: %#v %v", usage, err)
	}
	if object, err := subtree.Inspect(ctx, path); err != nil || object.SizeBytes != 2 {
		t.Fatalf("grown file was removed or changed: %#v %v", object, err)
	}
}

func TestRetentionTreeRejectsDirectoryReplacementDuringInventory(t *testing.T) {
	ctx := context.Background()
	session, _, root := bindMaterializeTestRoot(t)
	const operationName = "retention-directory-swap-operation"
	subtree, err := session.CreatePrivateSubtree(operationName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subtree.Close() })
	if _, err := subtree.MkdirAll(ctx, mustFSPath(t, "stage")); err != nil {
		t.Fatal(err)
	}
	file, err := subtree.CreateRegular(ctx, mustFSPath(t, "stage", "payload"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, operationName, "stage")
	backup := filepath.Join(root, operationName, "stage-backup")
	retentionInventoryDirectoryHook = func(components []string) error {
		if len(components) != 1 || components[0] != "stage" {
			return nil
		}
		if err := os.Rename(stage, backup); err != nil {
			return err
		}
		return os.Mkdir(stage, 0o700)
	}
	_, usage, err := inventoryRetentionTrees(ctx, subtree, []string{"stage"}, DefaultRetentionLimits())
	retentionInventoryDirectoryHook = nil
	t.Cleanup(func() { retentionInventoryDirectoryHook = nil })
	if !errors.Is(err, ErrIntegrity) || usage.RemovalAttempts != 0 {
		t.Fatalf("directory replacement crossed inventory boundary: %#v %v", usage, err)
	}
	if _, err := os.Lstat(filepath.Join(backup, "payload")); err != nil {
		t.Fatalf("inventory replacement check deleted an object: %v", err)
	}
}

func TestRetentionTreeMemoryBudgetBlocksDeepComponentRetention(t *testing.T) {
	ctx := context.Background()
	session, _, _ := bindMaterializeTestRoot(t)
	subtree, err := session.CreatePrivateSubtree("retention-memory-operation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subtree.Close() })
	components := []string{"stage", "a", "b", "c", "d", "e"}
	if _, err := subtree.MkdirAll(ctx, mustFSPath(t, components...)); err != nil {
		t.Fatal(err)
	}
	limits := RetentionLimits{
		MaxObjects: 100, MaxPathBytes: 1 << 20, MaxBytes: 16,
		MaxMemoryBytes: 512, MaxDepth: 16, MaxFindings: 4,
	}
	nodes, usage, err := inventoryRetentionTrees(ctx, subtree, []string{"stage"}, limits)
	if !errors.Is(err, ErrPolicy) || nodes != nil || usage.RemovalAttempts != 0 || usage.MemoryBytesConsidered > limits.MaxMemoryBytes {
		t.Fatalf("deep component memory budget was bypassed: nodes=%#v usage=%#v err=%v", nodes, usage, err)
	}
	if _, err := subtree.Inspect(ctx, mustFSPath(t, components...)); err != nil {
		t.Fatalf("memory preflight changed the tree: %v", err)
	}
}

func TestRetentionTreeMemoryBudgetBoundsDirectoryListingNPlusOne(t *testing.T) {
	ctx := context.Background()
	session, _, _ := bindMaterializeTestRoot(t)
	subtree, err := session.CreatePrivateSubtree("retention-list-memory-operation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subtree.Close() })
	if _, err := subtree.MkdirAll(ctx, mustFSPath(t, "stage")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two", "three"} {
		file, createErr := subtree.CreateRegular(ctx, mustFSPath(t, "stage", name))
		if createErr != nil {
			t.Fatal(createErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	limits := RetentionLimits{
		MaxObjects: 100, MaxPathBytes: 1 << 20, MaxBytes: 16,
		MaxMemoryBytes: 3 * retentionListEntryMemoryBytes, MaxDepth: 16, MaxFindings: 4,
	}
	nodes, usage, err := inventoryRetentionTrees(ctx, subtree, []string{"stage"}, limits)
	if !errors.Is(err, ErrPolicy) || nodes != nil || usage.RemovalAttempts != 0 || usage.MemoryBytesConsidered > limits.MaxMemoryBytes {
		t.Fatalf("directory listing memory budget was bypassed: nodes=%#v usage=%#v err=%v", nodes, usage, err)
	}
	for _, name := range []string{"one", "two", "three"} {
		if _, inspectErr := subtree.Inspect(ctx, mustFSPath(t, "stage", name)); inspectErr != nil {
			t.Fatalf("memory preflight changed %s: %v", name, inspectErr)
		}
	}
}

func mustFSPath(t *testing.T, components ...string) fsbind.Path {
	t.Helper()
	path, err := fsbind.PathFromComponents(components)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
