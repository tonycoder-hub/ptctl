package storage

import (
	"path/filepath"
	"testing"
)

func TestRejectDangerousWindowsPaths(t *testing.T) {
	semantics := PathSemantics{Windows: true, CaseSensitive: false, UnicodeNormalization: true}
	bad := [][][]byte{
		{[]byte("..")},
		{[]byte("CON")},
		{[]byte("movie:stream")},
		{[]byte("trailing.")},
		{[]byte("a/b")},
		{[]byte("e\u0301")},
		{[]byte("bad?.mkv")},
		{[]byte("bad\x1b.mkv")},
		{[]byte("COM¹.txt")},
	}
	for _, path := range bad {
		if err := ValidateComponents(path, semantics); err == nil {
			t.Fatalf("expected rejection for %q", path)
		}
	}
}

func TestRejectFileDirectoryPrefixCollision(t *testing.T) {
	paths := [][][]byte{{[]byte("a")}, {[]byte("a"), []byte("b.mkv")}}
	if err := ValidateManifestPaths(paths, PathSemantics{CaseSensitive: true}); err == nil {
		t.Fatal("expected file/directory prefix collision")
	}
}

func TestDetectCaseCollision(t *testing.T) {
	paths := [][][]byte{{[]byte("A.mkv")}, {[]byte("a.mkv")}}
	if err := ValidateManifestPaths(paths, PathSemantics{Windows: true, CaseSensitive: false}); err == nil {
		t.Fatal("expected collision")
	}
}

func TestMapHostToClient(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "movies", "a.mkv")
	got, err := MapHostToClient(root, path, "/downloads", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientPath != "/downloads/movies/a.mkv" {
		t.Fatalf("client path = %q", got.ClientPath)
	}
}

func TestMapHostToClientRejectsAmbiguousClientRoots(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.mkv")
	for _, test := range []struct {
		root    string
		windows bool
	}{
		{root: `\\downloads`, windows: false},
		{root: `\\server\`, windows: true},
		{root: `downloads`, windows: false},
		{root: `C:downloads`, windows: true},
	} {
		if _, err := MapHostToClient(root, path, test.root, test.windows); err == nil {
			t.Fatalf("expected client root %q to be rejected", test.root)
		}
	}
}

func TestMapHostToClientPreservesFilesystemRoots(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.mkv")
	posix, err := MapHostToClient(root, path, "/", false)
	if err != nil {
		t.Fatal(err)
	}
	if posix.ClientPath != "/a.mkv" {
		t.Fatalf("POSIX root mapping = %q", posix.ClientPath)
	}
	windows, err := MapHostToClient(root, path, `C:\`, true)
	if err != nil {
		t.Fatal(err)
	}
	if windows.ClientPath != `C:\a.mkv` {
		t.Fatalf("Windows root mapping = %q", windows.ClientPath)
	}
}
