package metafile

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseAndVerifyV1AcrossFileBoundary(t *testing.T) {
	data := []byte("abcdef")
	piece0 := sha1.Sum(data[:4])
	piece1 := sha1.Sum(data[4:])
	pieces := append(piece0[:], piece1[:]...)
	secret := "CANARY-PASSKEY-DO-NOT-LEAK"
	doc := bencode(map[string]any{
		"announce": "https://tracker.invalid/announce?passkey=" + secret,
		"info": map[string]any{
			"files": []any{
				map[string]any{"length": int64(3), "path": []any{"a.bin"}},
				map[string]any{"length": int64(3), "path": []any{"b.bin"}},
			},
			"name":         "bundle",
			"piece length": int64(4),
			"pieces":       pieces,
			"private":      int64(1),
		},
	})
	meta, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Version != "v1" || !meta.Private || meta.V1PieceCount != 2 || meta.TotalLength != 6 || meta.MetafileVariantID == "" {
		t.Fatalf("unexpected metadata: %#v", meta)
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || len(meta.Trackers) != 1 || meta.Trackers[0] != "https://tracker.invalid" {
		t.Fatalf("tracker secret leaked: %s", encoded)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.bin"), data[:3], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.bin"), data[3:], 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := VerifyV1(context.Background(), meta, root)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified || result.PiecesMatched != 2 || result.BytesVerified != 6 {
		t.Fatalf("unexpected verification: %#v", result)
	}
}

func TestRejectTraversal(t *testing.T) {
	piece := sha1.Sum([]byte("x"))
	doc := bencode(map[string]any{"info": map[string]any{
		"files":        []any{map[string]any{"length": int64(1), "path": []any{"..", "x"}}},
		"name":         "bad",
		"piece length": int64(1),
		"pieces":       piece[:],
	}})
	_, err := Parse(doc)
	if err == nil || !strings.Contains(err.Error(), "unsafe torrent manifest") {
		t.Fatalf("expected traversal rejection, got %v", err)
	}
}

func TestMetafileVariantIdentityDistinguishesPrivateWrappers(t *testing.T) {
	piece := sha1.Sum([]byte("x"))
	info := map[string]any{"length": int64(1), "name": "x", "piece length": int64(1), "pieces": piece[:]}
	first, err := Parse(bencode(map[string]any{"announce": "https://tracker.invalid/announce?passkey=one", "info": info}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse(bencode(map[string]any{"announce": "https://tracker.invalid/announce?passkey=two", "info": info}))
	if err != nil {
		t.Fatal(err)
	}
	if first.InfoHashV1 != second.InfoHashV1 || first.MetafileVariantID == second.MetafileVariantID {
		t.Fatalf("info identity and artifact identity were conflated: %#v %#v", first, second)
	}
}

func TestVerifyEmptyV1Torrent(t *testing.T) {
	doc := bencode(map[string]any{"info": map[string]any{
		"length": int64(0), "name": "empty", "piece length": int64(1), "pieces": []byte{},
	}})
	meta, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := VerifyV1(context.Background(), meta, path)
	if err != nil || !result.Verified || result.PiecesExpected != 0 {
		t.Fatalf("empty verification result=%#v err=%v", result, err)
	}
}

func TestSourceSnapshotRejectsSwappedPaths(t *testing.T) {
	pieceA := sha1.Sum([]byte("a"))
	pieceB := sha1.Sum([]byte("b"))
	pieces := append(pieceA[:], pieceB[:]...)
	doc := bencode(map[string]any{"info": map[string]any{
		"files": []any{
			map[string]any{"length": int64(1), "path": []any{"a"}},
			map[string]any{"length": int64(1), "path": []any{"b"}},
		},
		"name": "bundle", "piece length": int64(1), "pieces": pieces,
	}})
	meta, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if err := os.WriteFile(a, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixed := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(a, fixed, fixed); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(b, fixed, fixed); err != nil {
		t.Fatal(err)
	}
	verification, err := VerifyV1(context.Background(), meta, root)
	if err != nil || !verification.Verified {
		t.Fatalf("verification=%#v err=%v", verification, err)
	}
	temporary := filepath.Join(root, "swap")
	if err := os.Rename(a, temporary); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, b); err != nil {
		t.Fatal(err)
	}
	if _, err := verification.MatchSourceSnapshot(a); err == nil {
		t.Fatal("swapped source path matched the wrong verified snapshot")
	}
}

func TestRejectNonCanonicalDictionaryOrder(t *testing.T) {
	_, err := Parse([]byte("d1:bi1e1:ai2ee"))
	if err == nil {
		t.Fatal("expected unsorted dictionary rejection")
	}
}

func TestRejectConflictingLegacyAndUTF8Paths(t *testing.T) {
	piece := sha1.Sum([]byte("x"))
	doc := bencode(map[string]any{"info": map[string]any{
		"files": []any{map[string]any{
			"length": int64(1), "path": []any{"legacy"}, "path.utf-8": []any{"preferred"},
		}},
		"name": "bundle", "name.utf-8": "other", "piece length": int64(1), "pieces": piece[:],
	}})
	if _, err := Parse(doc); err == nil || !strings.Contains(err.Error(), "name.utf-8 and name disagree") {
		t.Fatalf("expected alternate-name conflict, got %v", err)
	}

	doc = bencode(map[string]any{"info": map[string]any{
		"files": []any{map[string]any{
			"length": int64(1), "path": []any{"legacy"}, "path.utf-8": []any{"preferred"},
		}},
		"name": "bundle", "piece length": int64(1), "pieces": piece[:],
	}})
	if _, err := Parse(doc); err == nil || !strings.Contains(err.Error(), "path.utf-8 and path disagree") {
		t.Fatalf("expected alternate-path conflict, got %v", err)
	}
}

func TestParseV2SingleFile(t *testing.T) {
	rootHash := bytes.Repeat([]byte{0x42}, 32)
	doc := bencode(map[string]any{"info": map[string]any{
		"file tree": map[string]any{"one.bin": map[string]any{"": map[string]any{
			"length": int64(1), "pieces root": rootHash,
		}}},
		"meta version": int64(2),
		"name":         "one.bin",
		"piece length": int64(16384),
	}})
	meta, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Version != "v2" || meta.MultiFile || len(meta.Files) != 1 || meta.Files[0].Path[0] != "one.bin" {
		t.Fatalf("unexpected v2 metadata: %#v", meta)
	}
}

func TestRejectV2FileAtFileTreeRoot(t *testing.T) {
	rootHash := bytes.Repeat([]byte{0x42}, 32)
	doc := bencode(map[string]any{"info": map[string]any{
		"file tree":    map[string]any{"": map[string]any{"length": int64(1), "pieces root": rootHash}},
		"meta version": int64(2), "name": "one.bin", "piece length": int64(16384),
	}})
	if _, err := Parse(doc); err == nil || !strings.Contains(err.Error(), "root must not be a file") {
		t.Fatalf("expected root-file rejection, got %v", err)
	}
}

func TestRejectHybridWithoutV2Layout(t *testing.T) {
	piece := sha1.Sum([]byte("x"))
	doc := bencode(map[string]any{"info": map[string]any{
		"length":       int64(1),
		"meta version": int64(2),
		"name":         "x",
		"piece length": int64(16384),
		"pieces":       piece[:],
	}})
	if _, err := Parse(doc); err == nil || !strings.Contains(err.Error(), "hybrid v2 layout") {
		t.Fatalf("expected invalid hybrid rejection, got %v", err)
	}
}

func TestParseHybridRequiresConsistentLayouts(t *testing.T) {
	piece := sha1.Sum([]byte("x"))
	rootHash := bytes.Repeat([]byte{0x42}, 32)
	info := map[string]any{
		"file tree": map[string]any{"x": map[string]any{"": map[string]any{"length": int64(1), "pieces root": rootHash}}},
		"length":    int64(1), "meta version": int64(2), "name": "x", "piece length": int64(16384), "pieces": piece[:],
	}
	meta, err := Parse(bencode(map[string]any{"info": info}))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Version != "hybrid" || meta.Validation != "hybrid_layout_consistent" || meta.Files[0].PiecesRoot == "" {
		t.Fatalf("unexpected hybrid metadata: %#v", meta)
	}
}

func TestHybridRequiresExactPieceAlignmentPadding(t *testing.T) {
	rootA := bytes.Repeat([]byte{0x41}, 32)
	rootB := bytes.Repeat([]byte{0x42}, 32)
	fileTree := map[string]any{
		"a": map[string]any{"": map[string]any{"length": int64(1), "pieces root": rootA}},
		"b": map[string]any{"": map[string]any{"length": int64(1), "pieces root": rootB}},
	}
	base := map[string]any{
		"file tree": fileTree, "meta version": int64(2), "name": "bundle", "piece length": int64(16384),
	}

	missingPad := cloneMap(base)
	missingPad["files"] = []any{
		map[string]any{"length": int64(1), "path": []any{"a"}},
		map[string]any{"length": int64(1), "path": []any{"b"}},
	}
	missingPad["pieces"] = make([]byte, 20)
	if _, err := Parse(bencode(map[string]any{"info": missingPad})); err == nil || !strings.Contains(err.Error(), "piece boundary") {
		t.Fatalf("expected missing padding rejection, got %v", err)
	}

	valid := cloneMap(base)
	valid["files"] = []any{
		map[string]any{"length": int64(1), "path": []any{"a"}},
		map[string]any{"attr": "p", "length": int64(16383), "path": []any{".pad", "16383"}},
		map[string]any{"length": int64(1), "path": []any{"b"}},
	}
	valid["pieces"] = make([]byte, 40)
	meta, err := Parse(bencode(map[string]any{"info": valid}))
	if err != nil || meta.Validation != "hybrid_layout_consistent" {
		t.Fatalf("valid hybrid rejected: meta=%#v err=%v", meta, err)
	}
}

func cloneMap(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func TestRejectV2MissingPieceLayers(t *testing.T) {
	rootHash := bytes.Repeat([]byte{0x42}, 32)
	doc := bencode(map[string]any{"info": map[string]any{
		"file tree":    map[string]any{"large.bin": map[string]any{"": map[string]any{"length": int64(32768), "pieces root": rootHash}}},
		"meta version": int64(2), "name": "large.bin", "piece length": int64(16384),
	}})
	if _, err := Parse(doc); err == nil || !strings.Contains(err.Error(), "missing required piece layers") {
		t.Fatalf("expected missing piece layers rejection, got %v", err)
	}
}

func TestRejectInvalidV2PieceLength(t *testing.T) {
	rootHash := bytes.Repeat([]byte{0x42}, 32)
	doc := bencode(map[string]any{"info": map[string]any{
		"file tree":    map[string]any{"one.bin": map[string]any{"": map[string]any{"length": int64(1), "pieces root": rootHash}}},
		"meta version": int64(2), "name": "one.bin", "piece length": int64(20000),
	}})
	if _, err := Parse(doc); err == nil || !strings.Contains(err.Error(), "power of two") {
		t.Fatalf("expected invalid v2 piece length rejection, got %v", err)
	}
}

func bencode(value any) []byte {
	var out bytes.Buffer
	encodeValue(&out, value)
	return out.Bytes()
}

func encodeValue(out *bytes.Buffer, value any) {
	switch v := value.(type) {
	case string:
		out.WriteString(strconv.Itoa(len(v)))
		out.WriteByte(':')
		out.WriteString(v)
	case []byte:
		out.WriteString(strconv.Itoa(len(v)))
		out.WriteByte(':')
		out.Write(v)
	case int64:
		out.WriteByte('i')
		out.WriteString(strconv.FormatInt(v, 10))
		out.WriteByte('e')
	case []any:
		out.WriteByte('l')
		for _, item := range v {
			encodeValue(out, item)
		}
		out.WriteByte('e')
	case map[string]any:
		out.WriteByte('d')
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			encodeValue(out, key)
			encodeValue(out, v[key])
		}
		out.WriteByte('e')
	default:
		panic("unsupported test bencode type")
	}
}
