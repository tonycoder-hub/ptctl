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
	}
	for _, path := range bad {
		if err := ValidateComponents(path, semantics); err == nil {
			t.Fatalf("expected rejection for %q", path)
		}
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
