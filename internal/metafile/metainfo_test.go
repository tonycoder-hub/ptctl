package metafile

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

func TestRejectV1SymbolicLinkAttribute(t *testing.T) {
	doc := bencode(map[string]any{"info": map[string]any{
		"files": []any{map[string]any{"attr": "l", "length": int64(0), "path": []any{"link"}}},
		"name":  "bundle", "piece length": int64(1), "pieces": []byte{},
	}})
	if _, err := Parse(doc); err == nil || !strings.Contains(err.Error(), "symbolic-link") {
		t.Fatalf("expected v1 symbolic-link rejection, got %v", err)
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

func TestVerifyV2BoundarySizes(t *testing.T) {
	for _, size := range []int{0, 1, 16383, 16384, 16385} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			content := bytes.Repeat([]byte{0x5a}, size)
			root, layer := testV2BlockRoot(content)
			doc := testV2SingleFileTorrent("boundary.bin", int64(size), 16384, root, layer)
			meta, err := Parse(doc)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "boundary.bin")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := Verify(context.Background(), meta, path)
			if err != nil {
				t.Fatal(err)
			}
			expectedPieces := 0
			if size > 0 {
				expectedPieces = (size + 16383) / 16384
			}
			if !result.Verified || result.PiecesExpected != expectedPieces || result.PiecesMatched != expectedPieces || len(result.Checks) != 1 || result.Checks[0].Algorithm != "bt-v2" {
				t.Fatalf("unexpected v2 boundary verification: %#v", result)
			}
		})
	}
}

func TestVerifyV2PadsOnlyLeavesBeyondEOF(t *testing.T) {
	first := bytes.Repeat([]byte{0x11}, 16384)
	second := bytes.Repeat([]byte{0x22}, 16384)
	content := append(append(append([]byte(nil), first...), second...), 0x33)
	leaf0 := sha256.Sum256(first)
	leaf1 := sha256.Sum256(second)
	leaf2 := sha256.Sum256([]byte{0x33})
	piece0 := testV2HashPair(leaf0, leaf1)
	piece1 := testV2HashPair(leaf2, [32]byte{})
	root := testV2HashPair(piece0, piece1)
	layer := append(append([]byte(nil), piece0[:]...), piece1[:]...)

	meta, err := Parse(testV2SingleFileTorrent("three-blocks.bin", int64(len(content)), 32768, root, layer))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "three-blocks.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := VerifyV2(context.Background(), meta, path)
	if err != nil || !result.Verified || result.PiecesMatched != 2 || result.RootsMatched != 1 {
		t.Fatalf("verification=%#v err=%v", result, err)
	}

	content[16384] ^= 1
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = VerifyV2(context.Background(), meta, path)
	if err != nil || result.Verified || len(result.MismatchPieces) == 0 {
		t.Fatalf("tampered verification=%#v err=%v", result, err)
	}
}

func TestVerifyV2NonPowerOfTwoPieceLayer(t *testing.T) {
	block0 := bytes.Repeat([]byte{0x10}, 16384)
	block1 := bytes.Repeat([]byte{0x20}, 16384)
	content := append(append(append([]byte(nil), block0...), block1...), 0x30)
	piece0 := sha256.Sum256(block0)
	piece1 := sha256.Sum256(block1)
	piece2 := sha256.Sum256([]byte{0x30})
	left := testV2HashPair(piece0, piece1)
	right := testV2HashPair(piece2, [32]byte{})
	root := testV2HashPair(left, right)
	layer := append(append(append([]byte(nil), piece0[:]...), piece1[:]...), piece2[:]...)

	meta, err := Parse(testV2SingleFileTorrent("three-pieces.bin", int64(len(content)), 16384, root, layer))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "three-pieces.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := VerifyV2(context.Background(), meta, path)
	if err != nil || !result.Verified || result.PiecesMatched != 3 || result.RootsMatched != 1 {
		t.Fatalf("verification=%#v err=%v", result, err)
	}
}

func TestVerifyV2FivePieceLayerUsesHigherLevelZeroSubtrees(t *testing.T) {
	content := make([]byte, 0, 4*32768+1)
	pieces := make([][32]byte, 0, 5)
	for value := byte(1); value <= 4; value++ {
		pieceBytes := bytes.Repeat([]byte{value}, 32768)
		content = append(content, pieceBytes...)
		left := sha256.Sum256(pieceBytes[:16384])
		right := sha256.Sum256(pieceBytes[16384:])
		pieces = append(pieces, testV2HashPair(left, right))
	}
	content = append(content, 0x05)
	lastLeaf := sha256.Sum256([]byte{0x05})
	pieces = append(pieces, testV2HashPair(lastLeaf, [32]byte{}))
	zeroPiece := testV2HashPair([32]byte{}, [32]byte{})
	leftHalf := testV2HashPair(testV2HashPair(pieces[0], pieces[1]), testV2HashPair(pieces[2], pieces[3]))
	rightHalf := testV2HashPair(testV2HashPair(pieces[4], zeroPiece), testV2HashPair(zeroPiece, zeroPiece))
	root := testV2HashPair(leftHalf, rightHalf)
	layer := make([]byte, 0, len(pieces)*32)
	for _, piece := range pieces {
		layer = append(layer, piece[:]...)
	}

	meta, err := Parse(testV2SingleFileTorrent("five-pieces.bin", int64(len(content)), 32768, root, layer))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "five-pieces.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := VerifyV2(context.Background(), meta, path)
	if err != nil || !result.Verified || result.PiecesMatched != 5 || result.RootsMatched != 1 {
		t.Fatalf("verification=%#v err=%v", result, err)
	}
}

func TestVerifyV2CancellationClosesFile(t *testing.T) {
	content := bytes.Repeat([]byte{0x5a}, 16385)
	root, layer := testV2BlockRoot(content)
	meta, err := Parse(testV2SingleFileTorrent("cancel.bin", int64(len(content)), 16384, root, layer))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "cancel.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := VerifyV2(ctx, meta, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("verification leaked an open file handle: %v", err)
	}
}

func TestRejectForgedV2PieceLayerWithCorrectLength(t *testing.T) {
	content := bytes.Repeat([]byte{0x41}, 16385)
	root, layer := testV2BlockRoot(content)
	valid := testV2SingleFileTorrent("forged.bin", int64(len(content)), 16384, root, layer)
	if _, err := Parse(valid); err != nil {
		t.Fatalf("valid layer rejected: %v", err)
	}
	forged := append([]byte(nil), layer...)
	forged[0] ^= 1
	if _, err := Parse(testV2SingleFileTorrent("forged.bin", int64(len(content)), 16384, root, forged)); err == nil || !strings.Contains(err.Error(), "does not hash") {
		t.Fatalf("expected authenticated-layer rejection, got %v", err)
	}
}

func TestVerifyV2FilesCanShareAuthenticatedPieceLayer(t *testing.T) {
	content := bytes.Repeat([]byte{0x44}, 16385)
	root, layer := testV2BlockRoot(content)
	doc := bencode(map[string]any{
		"piece layers": map[string]any{string(root[:]): layer},
		"info": map[string]any{
			"file tree": map[string]any{
				"a.bin": map[string]any{"": map[string]any{"length": int64(len(content)), "pieces root": root[:]}},
				"b.bin": map[string]any{"": map[string]any{"length": int64(len(content)), "pieces root": root[:]}},
			},
			"meta version": int64(2), "name": "shared", "piece length": int64(16384),
		},
	})
	meta, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	contentRoot := t.TempDir()
	for _, name := range []string{"a.bin", "b.bin"} {
		if err := os.WriteFile(filepath.Join(contentRoot, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := VerifyV2(context.Background(), meta, contentRoot)
	if err != nil || !result.Verified || result.PiecesMatched != 4 || result.RootsMatched != 2 {
		t.Fatalf("verification=%#v err=%v", result, err)
	}
}

func TestHybridVerificationRequiresBothHashFamilies(t *testing.T) {
	content := []byte("x")
	goodV1 := sha1.Sum(content)
	badV1 := sha1.Sum([]byte("y"))
	goodV2 := sha256.Sum256(content)
	badV2 := sha256.Sum256([]byte("y"))
	tests := []struct {
		name     string
		v1       [20]byte
		v2       [32]byte
		wantV1   bool
		wantV2   bool
		verified bool
	}{
		{name: "both", v1: goodV1, v2: goodV2, wantV1: true, wantV2: true, verified: true},
		{name: "v1-only", v1: goodV1, v2: badV2, wantV1: true, wantV2: false},
		{name: "v2-only", v1: badV1, v2: goodV2, wantV1: false, wantV2: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := bencode(map[string]any{"info": map[string]any{
				"file tree": map[string]any{"x": map[string]any{"": map[string]any{"length": int64(1), "pieces root": test.v2[:]}}},
				"length":    int64(1), "meta version": int64(2), "name": "x", "piece length": int64(16384), "pieces": test.v1[:],
			}})
			meta, err := Parse(doc)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "x")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := Verify(context.Background(), meta, path)
			if err != nil {
				t.Fatal(err)
			}
			if result.Verified != test.verified || len(result.Checks) != 2 || result.Checks[0].Verified != test.wantV1 || result.Checks[1].Verified != test.wantV2 {
				t.Fatalf("unexpected hybrid result: %#v", result)
			}
		})
	}
}

func TestVerifyHybridUsesVirtualV1PaddingAndPerFileV2Roots(t *testing.T) {
	piece0Bytes := append([]byte{'a'}, make([]byte, 16383)...)
	piece0 := sha1.Sum(piece0Bytes)
	piece1 := sha1.Sum([]byte{'b'})
	v1Pieces := append(append([]byte(nil), piece0[:]...), piece1[:]...)
	rootA := sha256.Sum256([]byte{'a'})
	rootB := sha256.Sum256([]byte{'b'})
	doc := bencode(map[string]any{"info": map[string]any{
		"file tree": map[string]any{
			"a": map[string]any{"": map[string]any{"length": int64(1), "pieces root": rootA[:]}},
			"b": map[string]any{"": map[string]any{"length": int64(1), "pieces root": rootB[:]}},
		},
		"files": []any{
			map[string]any{"length": int64(1), "path": []any{"a"}},
			map[string]any{"attr": "p", "length": int64(16383), "path": []any{".pad", "16383"}},
			map[string]any{"length": int64(1), "path": []any{"b"}},
		},
		"meta version": int64(2), "name": "bundle", "piece length": int64(16384), "pieces": v1Pieces,
	}})
	meta, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	contentRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(contentRoot, "a"), []byte{'a'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contentRoot, "b"), []byte{'b'}, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Verify(context.Background(), meta, contentRoot)
	if err != nil || !result.Verified || result.BytesVerified != 2 || result.PaddingBytes != 16383 || len(result.Checks) != 2 || !result.Checks[0].Verified || !result.Checks[1].Verified || result.Checks[0].BytesVerified != 2 || result.Checks[0].ProofStreamBytes != 16385 || result.Checks[1].BytesVerified != 2 || result.Checks[1].ProofStreamBytes != 2 {
		t.Fatalf("verification=%#v err=%v", result, err)
	}
}

func TestVerifyHybridSinglePassUsesAuthenticatedPieceLayer(t *testing.T) {
	first := bytes.Repeat([]byte{0x61}, 16384)
	second := bytes.Repeat([]byte{0x62}, 16384)
	content := append(append(append([]byte(nil), first...), second...), 0x63)
	v1Piece0 := sha1.Sum(content[:32768])
	v1Piece1 := sha1.Sum(content[32768:])
	v1Pieces := append(append([]byte(nil), v1Piece0[:]...), v1Piece1[:]...)
	leaf0 := sha256.Sum256(first)
	leaf1 := sha256.Sum256(second)
	leaf2 := sha256.Sum256([]byte{0x63})
	v2Piece0 := testV2HashPair(leaf0, leaf1)
	v2Piece1 := testV2HashPair(leaf2, [32]byte{})
	v2Root := testV2HashPair(v2Piece0, v2Piece1)
	v2Layer := append(append([]byte(nil), v2Piece0[:]...), v2Piece1[:]...)
	doc := bencode(map[string]any{
		"piece layers": map[string]any{string(v2Root[:]): v2Layer},
		"info": map[string]any{
			"file tree": map[string]any{"large.bin": map[string]any{"": map[string]any{
				"length": int64(len(content)), "pieces root": v2Root[:],
			}}},
			"length": int64(len(content)), "meta version": int64(2), "name": "large.bin", "piece length": int64(32768), "pieces": v1Pieces,
		},
	})
	meta, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "large.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Verify(context.Background(), meta, path)
	if err != nil || !result.Verified || len(result.Checks) != 2 || result.Checks[0].PiecesMatched != 2 || result.Checks[1].PiecesMatched != 2 || result.Checks[0].ProofStreamBytes != int64(len(content)) || result.Checks[1].ProofStreamBytes != int64(len(content)) {
		t.Fatalf("verification=%#v err=%v", result, err)
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

func TestRejectV2PaddingAttribute(t *testing.T) {
	rootHash := sha256.Sum256([]byte("x"))
	doc := bencode(map[string]any{"info": map[string]any{
		"file tree": map[string]any{"padding.bin": map[string]any{"": map[string]any{
			"attr": "p", "length": int64(1), "pieces root": rootHash[:],
		}}},
		"meta version": int64(2), "name": "padding.bin", "piece length": int64(16384),
	}})
	if _, err := Parse(doc); err == nil || !strings.Contains(err.Error(), "must not contain padding") {
		t.Fatalf("expected v2 padding rejection, got %v", err)
	}
}

func TestRejectV2SymbolicLinkAttribute(t *testing.T) {
	doc := bencode(map[string]any{"info": map[string]any{
		"file tree": map[string]any{"link": map[string]any{"": map[string]any{
			"attr": "l", "length": int64(0), "symlink path": []any{"target"},
		}}},
		"meta version": int64(2), "name": "link", "piece length": int64(16384),
	}})
	if _, err := Parse(doc); err == nil || !strings.Contains(err.Error(), "symbolic-link") {
		t.Fatalf("expected v2 symbolic-link rejection, got %v", err)
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

func testV2SingleFileTorrent(name string, length, pieceLength int64, root [32]byte, layer []byte) []byte {
	leaf := map[string]any{"length": length}
	if length > 0 {
		leaf["pieces root"] = root[:]
	}
	top := map[string]any{"info": map[string]any{
		"file tree":    map[string]any{name: map[string]any{"": leaf}},
		"meta version": int64(2),
		"name":         name,
		"piece length": pieceLength,
	}}
	if layer != nil {
		top["piece layers"] = map[string]any{string(root[:]): layer}
	}
	return bencode(top)
}

// These test helpers directly transcribe the small fixed trees used by the
// cases above; they intentionally do not call the production Merkle reducer.
func testV2BlockRoot(content []byte) ([32]byte, []byte) {
	if len(content) == 0 {
		return [32]byte{}, nil
	}
	firstBytes := len(content)
	if firstBytes > 16384 {
		firstBytes = 16384
	}
	first := sha256.Sum256(content[:firstBytes])
	if len(content) <= 16384 {
		return first, nil
	}
	second := sha256.Sum256(content[16384:])
	root := testV2HashPair(first, second)
	layer := append(append([]byte(nil), first[:]...), second[:]...)
	return root, layer
}

func testV2HashPair(left, right [32]byte) [32]byte {
	var pair [64]byte
	copy(pair[:32], left[:])
	copy(pair[32:], right[:])
	return sha256.Sum256(pair[:])
}
