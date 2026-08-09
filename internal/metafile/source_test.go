package metafile

import (
	"context"
	"crypto/sha1"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifiedSourceRejectsDifferentMetafileVariant(t *testing.T) {
	content := []byte("content")
	first := testSingleV1Meta(t, "first.bin", content)
	second := testSingleV1Meta(t, "second.bin", content)
	path := filepath.Join(t.TempDir(), "renamed")
	writeTestFile(t, path, content)
	verified, err := VerifySourceMap(context.Background(), first, SourceMap{Bindings: []SourceBinding{{FileIndex: 0, Path: path}}})
	if err != nil {
		t.Fatal(err)
	}
	if !verified.Matches(first) || verified.Matches(second) {
		t.Fatal("opaque verification observation was not bound to one metafile variant")
	}
}

func TestVerifiedSourceRejectsChangedFileBeforePlanPrecondition(t *testing.T) {
	content := []byte("content")
	meta := testSingleV1Meta(t, "source.bin", content)
	path := filepath.Join(t.TempDir(), "renamed")
	writeTestFile(t, path, content)
	verified, err := VerifySourceMap(context.Background(), meta, SourceMap{Bindings: []SourceBinding{{FileIndex: 0, Path: path}}})
	if err != nil {
		t.Fatal(err)
	}
	observed, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	backup := path + ".old"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, []byte("changed"))
	if err := os.Chtimes(path, observed.ModTime(), observed.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := verified.SourcePrecondition(0); err == nil {
		t.Fatal("same-size, same-mtime replacement retained a verified precondition")
	}
}

func TestVerifySourceMapRejectsOpenerForDifferentNamedPath(t *testing.T) {
	content := []byte("content")
	meta := testSingleV1Meta(t, "source.bin", content)
	root := t.TempDir()
	namedPath := filepath.Join(root, "named")
	openedPath := filepath.Join(root, "opened")
	writeTestFile(t, namedPath, content)
	writeTestFile(t, openedPath, content)

	_, err := VerifySourceMap(context.Background(), meta, SourceMap{Bindings: []SourceBinding{{
		FileIndex: 0,
		Path:      namedPath,
		Open:      func() (*os.File, error) { return os.Open(openedPath) },
	}}})
	if err == nil {
		t.Fatal("an opener for a different file was allowed to authenticate the named path")
	}
}

func TestVerifiedSourceBindingsHideIdentityOpener(t *testing.T) {
	content := []byte("content")
	meta := testSingleV1Meta(t, "source.bin", content)
	path := filepath.Join(t.TempDir(), "named")
	writeTestFile(t, path, content)
	verified, err := VerifySourceMap(context.Background(), meta, SourceMap{Bindings: []SourceBinding{{
		FileIndex: 0,
		Path:      path,
		Open:      func() (*os.File, error) { return os.Open(path) },
	}}})
	if err != nil {
		t.Fatal(err)
	}
	bindings := verified.Bindings()
	if len(bindings) != 1 || bindings[0].Path != path || bindings[0].Open != nil {
		t.Fatalf("public bindings leaked the process-local opener: %#v", bindings)
	}
}

func testSingleV1Meta(t *testing.T, name string, content []byte) *MetaInfo {
	t.Helper()
	piece := sha1.Sum(content)
	meta, err := Parse(bencode(map[string]any{"info": map[string]any{
		"length": int64(len(content)), "name": name, "piece length": int64(len(content)), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}
	return meta
}
