package fsbind

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateRootMarkerAndExactSubtreeRetirement(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	session, _, err := BindExisting(root)
	if errors.Is(err, ErrUnsupported) {
		t.Skipf("bound subtree retirement is unsupported: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	subtree, err := session.CreatePrivateSubtree("retire-operation")
	if err != nil {
		t.Fatal(err)
	}
	markerScratch := mustRetirementPath(t, "purge-intent.pending")
	marker, err := subtree.CreateRegular(ctx, markerScratch)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("private root transition marker")
	if _, err := marker.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := marker.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := marker.Close(); err != nil {
		t.Fatal(err)
	}
	publication, err := session.PublishNoReplace(ctx, subtree, markerScratch, ".ptctl-retirement-intent-test")
	if err != nil || !publication.Published || publication.Durability != DurabilityConfirmed {
		t.Fatalf("publication=%#v err=%v", publication, err)
	}
	markerName := ".ptctl-retirement-intent-test"
	markerPath := filepath.Join(root, markerName)
	markerAlias := filepath.Join(root, ".ptctl-retirement-intent-alias")
	if linkErr := os.Link(markerPath, markerAlias); linkErr == nil {
		if private, openErr := session.OpenRootPrivateRegular(ctx, markerName); !errors.Is(openErr, ErrUnsafeObject) {
			if private != nil {
				_ = private.Close()
			}
			t.Fatalf("hardlinked root marker retained private authority: %v", openErr)
		}
		if err := os.Remove(markerAlias); err != nil {
			t.Fatal(err)
		}
	}

	rootMarker, err := session.OpenRootPrivateRegular(ctx, markerName)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := rootMarker.Info()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(rootMarker)
	if err != nil || string(raw) != string(payload) {
		t.Fatalf("raw=%q err=%v", raw, err)
	}
	if err := rootMarker.Close(); err != nil {
		t.Fatal(err)
	}
	if receipt, err := session.RemoveRootPrivateRegularExact(ctx, markerName, observed.Identity, observed.SizeBytes+1); !errors.Is(err, ErrUnsafeObject) || receipt.Attempted {
		t.Fatalf("wrong-size receipt=%#v err=%v", receipt, err)
	}
	cancelledMarker, cancelMarker := context.WithCancel(ctx)
	cancelMarker()
	if receipt, err := session.RemoveRootPrivateRegularExact(cancelledMarker, markerName, observed.Identity, observed.SizeBytes); !errors.Is(err, context.Canceled) || receipt.Attempted {
		t.Fatalf("cancelled marker removal=%#v err=%v", receipt, err)
	}
	markerRemoval, err := session.RemoveRootPrivateRegularExact(ctx, markerName, observed.Identity, observed.SizeBytes)
	if err != nil || !markerRemoval.Attempted || !markerRemoval.Removed || markerRemoval.Durability != DurabilityConfirmed {
		t.Fatalf("marker removal=%#v err=%v", markerRemoval, err)
	}

	extraPath := mustRetirementPath(t, "unexpected.bin")
	extra, err := subtree.CreateRegular(ctx, extraPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extra.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	extraInfo, err := extra.Info()
	if err != nil {
		t.Fatal(err)
	}
	if err := extra.Close(); err != nil {
		t.Fatal(err)
	}
	if receipt, err := session.RemovePrivateSubtreeExact(ctx, subtree); !errors.Is(err, ErrUnsafeObject) || receipt.Attempted {
		t.Fatalf("nonempty receipt=%#v err=%v", receipt, err)
	}
	if receipt, err := subtree.RemoveRegularExact(ctx, extraPath, extraInfo.Identity, extraInfo.SizeBytes); err != nil || !receipt.Removed {
		t.Fatalf("extra removal=%#v err=%v", receipt, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if receipt, err := session.RemovePrivateSubtreeExact(cancelled, subtree); !errors.Is(err, context.Canceled) || receipt.Attempted {
		t.Fatalf("cancelled retirement=%#v err=%v", receipt, err)
	}
	if err := subtree.Check(); err != nil {
		t.Fatalf("pre-attempt cancellation consumed subtree: %v", err)
	}

	retired, err := session.RemovePrivateSubtreeExact(ctx, subtree)
	if err != nil || !retired.Attempted || !retired.LockRemovalAttempted || !retired.LockRemoved ||
		!retired.DirectoryAttempted || !retired.Removed || retired.Durability != DurabilityConfirmed || retired.Identity.IsZero() || retired.LockIdentity.IsZero() {
		t.Fatalf("retired=%#v err=%v", retired, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "retire-operation")); !os.IsNotExist(err) {
		t.Fatalf("retired operation remains: %v", err)
	}
	if err := subtree.Close(); err != nil {
		t.Fatalf("consumed subtree close: %v", err)
	}
}

func TestRemovePrivateSubtreeResidueRequiresExactEmptyIdentity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	session, _, err := BindExisting(root)
	if errors.Is(err, ErrUnsupported) {
		t.Skipf("bound subtree retirement is unsupported: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	subtree, err := session.CreatePrivateSubtree("retire-residue")
	if err != nil {
		t.Fatal(err)
	}
	expected := subtree.Identity()
	if err := subtree.Close(); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "retire-residue", operationLockName)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	unexpectedPath := filepath.Join(root, "retire-residue", "unexpected")
	if err := os.WriteFile(unexpectedPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if receipt, err := session.RemovePrivateSubtreeResidueExact(ctx, "retire-residue", expected); !errors.Is(err, ErrUnsafeObject) || receipt.Attempted {
		t.Fatalf("nonempty residue receipt=%#v err=%v", receipt, err)
	}
	if err := os.Remove(unexpectedPath); err != nil {
		t.Fatal(err)
	}
	removed, err := session.RemovePrivateSubtreeResidueExact(ctx, "retire-residue", expected)
	if err != nil || !removed.Attempted || !removed.LockAbsentBefore || removed.LockRemovalAttempted ||
		!removed.DirectoryAttempted || !removed.Removed || removed.Durability != DurabilityConfirmed || !removed.Identity.Equal(expected) {
		t.Fatalf("residue removal=%#v err=%v", removed, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "retire-residue")); !os.IsNotExist(err) {
		t.Fatalf("residue remains: %v", err)
	}
}

func mustRetirementPath(t *testing.T, components ...string) Path {
	t.Helper()
	path, err := PathFromComponents(components)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
