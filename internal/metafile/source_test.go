package metafile

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"io"
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
		Open:      func() (SourceFile, error) { return os.Open(openedPath) },
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
		Open:      func() (SourceFile, error) { return os.Open(path) },
	}}})
	if err != nil {
		t.Fatal(err)
	}
	bindings := verified.Bindings()
	if len(bindings) != 1 || bindings[0].Path != path || bindings[0].Open != nil {
		t.Fatalf("public bindings leaked the process-local opener: %#v", bindings)
	}
	reverified, err := verified.Reverify(context.Background(), meta)
	if err != nil || reverified == nil || !reverified.Result().Verified {
		t.Fatalf("process-local reverification failed: verified=%v err=%v", reverified != nil, err)
	}
}

func TestVerifyContentSourceRetainsPhysicalEmptyFileAuthority(t *testing.T) {
	content := []byte("x")
	piece := sha1.Sum(content)
	meta, err := Parse(bencode(map[string]any{"info": map[string]any{
		"files": []any{
			map[string]any{"length": int64(len(content)), "path": []any{"data.bin"}},
			map[string]any{"length": int64(0), "path": []any{"empty.bin"}},
		},
		"name": "bundle", "piece length": int64(1), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "data.bin"), content)
	emptyPath := filepath.Join(root, "empty.bin")
	writeTestFile(t, emptyPath, nil)

	verified, err := VerifyContentSource(context.Background(), meta, root)
	if err != nil {
		t.Fatal(err)
	}
	if !verified.Result().Verified {
		t.Fatalf("exact layout did not verify: %#v", verified.Result())
	}
	path, ok := verified.Path(1)
	if !ok || path != emptyPath {
		t.Fatalf("physical empty-file binding was discarded: path=%q ok=%t bindings=%#v", path, ok, verified.Bindings())
	}
	precondition, err := verified.SourcePrecondition(1)
	if err != nil || precondition.SizeBytes != 0 {
		t.Fatalf("empty-file authority is unusable: precondition=%#v err=%v", precondition, err)
	}
	if reverified, err := verified.Reverify(context.Background(), meta); err != nil || !reverified.Result().Verified {
		t.Fatalf("empty-file authority could not be reverified: result=%#v err=%v", reverified, err)
	}

	observed, err := os.Stat(emptyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(emptyPath, emptyPath+".old"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, emptyPath, nil)
	if err := os.Chtimes(emptyPath, observed.ModTime(), observed.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := verified.Reverify(context.Background(), meta); err == nil {
		t.Fatal("same-size, same-mtime empty-file replacement retained source authority")
	}
}

func TestVerifiedSourceReverifyRejectsExactNamedReplacement(t *testing.T) {
	content := []byte("content")
	meta := testSingleV1Meta(t, "source.bin", content)
	path := filepath.Join(t.TempDir(), "named")
	writeTestFile(t, path, content)
	verified, err := VerifySourceMap(context.Background(), meta, SourceMap{Bindings: []SourceBinding{{FileIndex: 0, Path: path}}})
	if err != nil {
		t.Fatal(err)
	}
	observed, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, content)
	if err := os.Chtimes(path, observed.ModTime(), observed.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := verified.Reverify(context.Background(), meta); err == nil {
		t.Fatal("an exact same-size, same-mtime named replacement retained reverification authority")
	}
}

func TestConsumeVerifiedFileUsesProcessLocalIdentityBoundAuthority(t *testing.T) {
	content := []byte("content to copy")
	meta := testSingleV1Meta(t, "source.bin", content)
	path := filepath.Join(t.TempDir(), "renamed")
	writeTestFile(t, path, content)
	verified, err := VerifySourceMap(context.Background(), meta, SourceMap{Bindings: []SourceBinding{{
		FileIndex: 0,
		Path:      path,
		Open:      func() (SourceFile, error) { return os.Open(path) },
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var copied bytes.Buffer
	precondition, err := verified.ConsumeVerifiedFile(context.Background(), 0, func(reader io.Reader) error {
		_, err := io.Copy(&copied, reader)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(copied.Bytes(), content) || precondition.SizeBytes != int64(len(content)) {
		t.Fatalf("unexpected consumed bytes or precondition: %q %#v", copied.Bytes(), precondition)
	}
}

func TestConsumeVerifiedFileRejectsShortConsumer(t *testing.T) {
	content := []byte("content")
	meta := testSingleV1Meta(t, "source.bin", content)
	path := filepath.Join(t.TempDir(), "renamed")
	writeTestFile(t, path, content)
	verified, err := VerifySourceMap(context.Background(), meta, SourceMap{Bindings: []SourceBinding{{FileIndex: 0, Path: path}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verified.ConsumeVerifiedFile(context.Background(), 0, func(reader io.Reader) error {
		var one [1]byte
		_, err := reader.Read(one[:])
		return err
	}); err == nil {
		t.Fatal("a consumer that stopped before the exact length was accepted")
	}
}

func TestConsumeVerifiedFileRejectsChangedNamedIdentity(t *testing.T) {
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
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, []byte("changed"))
	if err := os.Chtimes(path, observed.ModTime(), observed.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := verified.ConsumeVerifiedFile(context.Background(), 0, func(reader io.Reader) error {
		_, err := io.Copy(io.Discard, reader)
		return err
	}); err == nil {
		t.Fatal("a same-size, same-mtime named replacement retained copy authority")
	}
}

func TestConsumeVerifiedFileHonorsPreCancelledContext(t *testing.T) {
	content := []byte("content")
	meta := testSingleV1Meta(t, "source.bin", content)
	path := filepath.Join(t.TempDir(), "renamed")
	writeTestFile(t, path, content)
	verified, err := VerifySourceMap(context.Background(), meta, SourceMap{Bindings: []SourceBinding{{FileIndex: 0, Path: path}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if _, err := verified.ConsumeVerifiedFile(ctx, 0, func(io.Reader) error {
		called = true
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if called {
		t.Fatal("consumer ran after cancellation")
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
