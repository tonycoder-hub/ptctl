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
	if meta.Version != "v1" || !meta.Private || meta.PieceCount != 2 || meta.TotalLength != 6 {
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
	meta, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	_, err = VerifyV1(context.Background(), meta, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsafe torrent manifest") {
		t.Fatalf("expected traversal rejection, got %v", err)
	}
}

func TestRejectNonCanonicalDictionaryOrder(t *testing.T) {
	_, err := Parse([]byte("d1:bi1e1:ai2ee"))
	if err == nil {
		t.Fatal("expected unsorted dictionary rejection")
	}
}

func TestParseV2SingleFile(t *testing.T) {
	rootHash := bytes.Repeat([]byte{0x42}, 32)
	doc := bencode(map[string]any{"info": map[string]any{
		"file tree": map[string]any{"": map[string]any{
			"length":      int64(1),
			"pieces root": rootHash,
		}},
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
