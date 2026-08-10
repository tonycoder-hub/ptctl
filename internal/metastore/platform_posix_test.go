//go:build linux || darwin

package metastore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPOSIXStoreRejectsWidePermissions(t *testing.T) {
	t.Run("root", func(t *testing.T) {
		store := newTestStore(t)
		if err := os.Chmod(store.root, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(store.root, 0o700) })
		if opened, err := Open(store.root); err == nil || opened != nil {
			t.Fatalf("Open accepted a wide root mode: store=%v err=%v", opened, err)
		}
	})

	t.Run("marker", func(t *testing.T) {
		store := newTestStore(t)
		marker := filepath.Join(store.root, markerName)
		if err := os.Chmod(marker, 0o644); err != nil {
			t.Fatal(err)
		}
		if opened, err := Open(store.root); err == nil || opened != nil {
			t.Fatalf("Open accepted a wide marker mode: store=%v err=%v", opened, err)
		}
	})

	t.Run("artifact", func(t *testing.T) {
		store := newTestStore(t)
		raw := testMetafile("https://tracker.invalid/announce", "mode.bin", []byte("content"))
		_, ref, _, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		object := filepath.Join(store.root, objectRelativePath(ref.ID))
		if err := os.Chmod(object, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Load(context.Background(), ref.ID, DefaultLimits()); !errors.Is(err, ErrCorruptArtifact) {
			t.Fatalf("Load accepted a wide artifact mode: %v", err)
		}
	})
}

func TestPOSIXRenamedRootPublishesToBoundHandleButCannotReportSuccess(t *testing.T) {
	store := newTestStore(t)
	raw := testMetafile("https://tracker.invalid/announce", "renamed-root.bin", []byte("content"))
	moved := store.root + "-moved"
	originalHook := operationIdentityHook
	t.Cleanup(func() { operationIdentityHook = originalHook })
	operationIdentityHook = func(stage string, session *rootSession) error {
		if stage == "after_publish" {
			if err := os.Rename(session.path, moved); err != nil {
				return err
			}
		}
		return nil
	}
	meta, ref, receipt, err := store.Import(context.Background(), bytes.NewReader(raw), DefaultLimits())
	operationIdentityHook = originalHook
	if err == nil || meta == nil || ref.ID == "" || receipt.WritesPerformed != 1 || strings.Contains(err.Error(), store.root) {
		t.Fatalf("Import returned meta=%v ref=%+v receipt=%+v err=%v", meta, ref, receipt, err)
	}
	boundStore, openErr := Open(moved)
	if openErr != nil {
		t.Fatalf("open moved bound store: %v", openErr)
	}
	loaded, loadedRef, loadErr := boundStore.Load(context.Background(), ref.ID, DefaultLimits())
	if loadErr != nil || loaded == nil || loadedRef.ID != ref.ID {
		t.Fatalf("bound publication missing: meta=%v ref=%+v err=%v", loaded, loadedRef, loadErr)
	}
}
