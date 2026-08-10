//go:build windows

package metastore

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsErrorPathsReleaseStoreHandles(t *testing.T) {
	valid := windowsTestMetafile([]byte("valid"))
	tests := []struct {
		name string
		run  func(*testing.T, *Store, string)
	}{
		{name: "invalid import", run: func(t *testing.T, store *Store, _ string) {
			if _, _, _, err := store.Import(context.Background(), bytes.NewReader([]byte("invalid")), DefaultLimits()); !errors.Is(err, ErrInvalidMetafile) {
				t.Fatalf("unexpected import error: %v", err)
			}
		}},
		{name: "valid import", run: func(t *testing.T, store *Store, _ string) {
			if _, _, _, err := store.Import(context.Background(), bytes.NewReader(valid), DefaultLimits()); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "corrupt load", run: func(t *testing.T, store *Store, root string) {
			_, ref, _, err := store.Import(context.Background(), bytes.NewReader(valid), DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, objectRelativePath(ref.ID)), []byte("corrupt"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Load(context.Background(), ref.ID, DefaultLimits()); !errors.Is(err, ErrCorruptArtifact) {
				t.Fatalf("unexpected load error: %v", err)
			}
		}},
		{name: "file import and corrupt load", run: func(t *testing.T, store *Store, root string) {
			source := filepath.Join(t.TempDir(), "source.torrent")
			if err := os.WriteFile(source, valid, 0o600); err != nil {
				t.Fatal(err)
			}
			_, ref, _, err := store.ImportFile(context.Background(), source, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, objectRelativePath(ref.ID)), []byte("corrupt"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Load(context.Background(), ref.ID, DefaultLimits()); !errors.Is(err, ErrCorruptArtifact) {
				t.Fatalf("unexpected load error: %v", err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "store")
			store, _, err := Init(root)
			if err != nil {
				t.Fatal(err)
			}
			test.run(t, store, root)
			if err := os.RemoveAll(root); err != nil {
				t.Fatalf("store retained an open handle: %v", err)
			}
		})
	}
}

func windowsTestMetafile(content []byte) []byte {
	piece := sha1.Sum(content)
	value := []byte("d4:infod6:lengthi5e4:name1:x12:piece lengthi5e6:pieces20:")
	value = append(value, piece[:]...)
	return append(value, 'e', 'e')
}
