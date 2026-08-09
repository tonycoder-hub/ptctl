package metafile

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/storage"
)

type File struct {
	Path          []string `json:"path"`
	PathRawBase64 []string `json:"path_raw_base64"`
	Length        int64    `json:"length"`
	Attribute     string   `json:"attribute,omitempty"`
	PiecesRoot    string   `json:"pieces_root,omitempty"`
	RawPath       [][]byte `json:"-"`
	piecesRootRaw []byte
	pieceLayerRaw []byte
}

type MetaInfo struct {
	Name              string   `json:"name"`
	NameRawBase64     string   `json:"name_raw_base64"`
	Version           string   `json:"version"`
	Validation        string   `json:"validation"`
	MetafileVariantID string   `json:"metafile_variant_id"`
	MetafileBytes     int64    `json:"metafile_bytes"`
	InfoHashV1        string   `json:"info_hash_v1,omitempty"`
	InfoHashV2        string   `json:"info_hash_v2,omitempty"`
	Private           bool     `json:"private"`
	MultiFile         bool     `json:"multi_file"`
	PieceLength       int64    `json:"piece_length"`
	V1PieceCount      int      `json:"v1_piece_count,omitempty"`
	TotalLength       int64    `json:"total_length"`
	Trackers          []string `json:"tracker_origins"`
	Files             []File   `json:"files"`
	NameRaw           []byte   `json:"-"`
	pieceHashes       [][20]byte
}

func Read(path string) (*MetaInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open metafile: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (32<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("read metafile: %w", err)
	}
	if len(data) > 32<<20 {
		return nil, fmt.Errorf("metafile exceeds 32 MiB")
	}
	return Parse(data)
}

func Parse(data []byte) (*MetaInfo, error) {
	root, err := parseBencode(data)
	if err != nil {
		return nil, fmt.Errorf("decode metafile: %w", err)
	}
	if root.Kind != KindDictionary {
		return nil, fmt.Errorf("metafile root is not a dictionary")
	}
	info, ok := root.Get("info")
	if !ok || info.Kind != KindDictionary {
		return nil, fmt.Errorf("metafile has no info dictionary")
	}
	variantDigest := sha256.Sum256(data)
	meta := &MetaInfo{
		MetafileVariantID: "sha256:" + hex.EncodeToString(variantDigest[:]),
		MetafileBytes:     int64(len(data)),
	}
	nameNode, ok := preferredBytes(info, "name.utf-8", "name")
	if !ok || len(nameNode.Bytes) == 0 {
		return nil, fmt.Errorf("info dictionary has no name")
	}
	if err := validateAlternateBytes(info, "name.utf-8", "name"); err != nil {
		return nil, err
	}
	meta.NameRaw = append([]byte(nil), nameNode.Bytes...)
	meta.Name = displayBytes(meta.NameRaw)
	meta.NameRawBase64 = base64.StdEncoding.EncodeToString(meta.NameRaw)

	if pieceLength, ok := integer(info, "piece length"); ok {
		if pieceLength <= 0 || pieceLength > 64<<20 {
			return nil, fmt.Errorf("piece length is outside 1..64 MiB")
		}
		meta.PieceLength = pieceLength
	}
	if meta.PieceLength == 0 {
		return nil, fmt.Errorf("info dictionary has no piece length")
	}
	if private, ok := integer(info, "private"); ok {
		if private != 0 && private != 1 {
			return nil, fmt.Errorf("private flag must be 0 or 1")
		}
		meta.Private = private == 1
	}

	pieces, hasV1 := bytesValue(info, "pieces")
	metaVersion, hasMetaVersion := integer(info, "meta version")
	hasV2 := hasMetaVersion && metaVersion == 2
	if hasMetaVersion && metaVersion != 2 {
		return nil, fmt.Errorf("unsupported meta version %d", metaVersion)
	}
	if hasV2 && (meta.PieceLength < 16<<10 || meta.PieceLength&(meta.PieceLength-1) != 0) {
		return nil, fmt.Errorf("v2 piece length must be a power of two and at least 16 KiB")
	}
	if hasV1 {
		if len(pieces)%sha1.Size != 0 {
			return nil, fmt.Errorf("v1 pieces string length is not divisible by 20")
		}
		meta.pieceHashes = make([][20]byte, len(pieces)/sha1.Size)
		for i := range meta.pieceHashes {
			copy(meta.pieceHashes[i][:], pieces[i*sha1.Size:(i+1)*sha1.Size])
		}
		v1 := sha1.Sum(data[info.Start:info.End])
		meta.InfoHashV1 = hex.EncodeToString(v1[:])
	}
	if hasV2 {
		v2 := sha256.Sum256(data[info.Start:info.End])
		meta.InfoHashV2 = hex.EncodeToString(v2[:])
	}
	switch {
	case hasV1 && hasV2:
		meta.Version = "hybrid"
	case hasV2:
		meta.Version = "v2"
	case hasV1:
		meta.Version = "v1"
	default:
		return nil, fmt.Errorf("info dictionary has neither v1 pieces nor meta version 2")
	}

	if hasV1 && hasV2 {
		if err := parseV1Files(meta, info); err != nil {
			return nil, err
		}
		v2Layout := &MetaInfo{Name: meta.Name, NameRaw: meta.NameRaw, NameRawBase64: meta.NameRawBase64, PieceLength: meta.PieceLength}
		if err := parseV2Files(v2Layout, info); err != nil {
			return nil, fmt.Errorf("invalid hybrid v2 layout: %w", err)
		}
		if err := validateV2PieceLayers(root, v2Layout.Files, meta.PieceLength); err != nil {
			return nil, err
		}
		if err := reconcileHybridLayouts(meta, v2Layout); err != nil {
			return nil, err
		}
		meta.Validation = "hybrid_layout_consistent"
	} else if hasV1 {
		if err := parseV1Files(meta, info); err != nil {
			return nil, err
		}
		meta.Validation = "v1_manifest_structural"
	} else {
		if err := parseV2Files(meta, info); err != nil {
			return nil, err
		}
		if err := validateV2PieceLayers(root, meta.Files, meta.PieceLength); err != nil {
			return nil, err
		}
		meta.Validation = "v2_manifest_structural"
	}
	manifestPaths := make([][][]byte, len(meta.Files))
	for i := range meta.Files {
		manifestPaths[i] = meta.Files[i].RawPath
	}
	if err := storage.ValidateManifestPaths(manifestPaths, storage.PathSemantics{CaseSensitive: true}); err != nil {
		return nil, fmt.Errorf("unsafe torrent manifest: %w", err)
	}
	meta.V1PieceCount = len(meta.pieceHashes)
	if hasV1 {
		expectedPieces64 := int64(0)
		if meta.TotalLength > 0 {
			expectedPieces64 = ((meta.TotalLength - 1) / meta.PieceLength) + 1
		}
		if expectedPieces64 > int64(^uint(0)>>1) {
			return nil, fmt.Errorf("v1 piece count overflows int")
		}
		expectedPieces := int(expectedPieces64)
		if meta.PieceLength == 0 || meta.V1PieceCount != expectedPieces {
			return nil, fmt.Errorf("piece count %d does not match total length and piece length (expected %d)", meta.V1PieceCount, expectedPieces)
		}
	}
	meta.Trackers = trackerOrigins(root)
	return meta, nil
}

func parseV1Files(meta *MetaInfo, info *Node) error {
	if files, ok := info.Get("files"); ok {
		if files.Kind != KindList || len(files.List) == 0 {
			return fmt.Errorf("multi-file torrent has an invalid files list")
		}
		meta.MultiFile = true
		for index, item := range files.List {
			if item.Kind != KindDictionary {
				return fmt.Errorf("file %d is not a dictionary", index)
			}
			length, ok := integer(item, "length")
			if !ok || length < 0 {
				return fmt.Errorf("file %d has an invalid length", index)
			}
			pathNode, ok := preferred(item, "path.utf-8", "path")
			if !ok || pathNode.Kind != KindList || len(pathNode.List) == 0 {
				return fmt.Errorf("file %d has an invalid path", index)
			}
			if err := validateAlternatePath(item, index); err != nil {
				return err
			}
			file := File{Length: length}
			if attr, ok := bytesValue(item, "attr"); ok {
				file.Attribute = string(attr)
			}
			if strings.Contains(file.Attribute, "l") {
				return fmt.Errorf("v1 symbolic-link files are not supported")
			}
			for _, component := range pathNode.List {
				if component.Kind != KindBytes || len(component.Bytes) == 0 {
					return fmt.Errorf("file %d has an invalid path component", index)
				}
				file.RawPath = append(file.RawPath, append([]byte(nil), component.Bytes...))
				file.Path = append(file.Path, displayBytes(component.Bytes))
				file.PathRawBase64 = append(file.PathRawBase64, base64.StdEncoding.EncodeToString(component.Bytes))
			}
			if err := addLength(meta, length); err != nil {
				return err
			}
			meta.Files = append(meta.Files, file)
		}
		return nil
	}

	length, ok := integer(info, "length")
	if !ok || length < 0 {
		return fmt.Errorf("single-file torrent has an invalid length")
	}
	meta.Files = []File{{
		Path:          []string{meta.Name},
		PathRawBase64: []string{base64.StdEncoding.EncodeToString(meta.NameRaw)},
		RawPath:       [][]byte{append([]byte(nil), meta.NameRaw...)},
		Length:        length,
	}}
	return addLength(meta, length)
}

func parseV2Files(meta *MetaInfo, info *Node) error {
	tree, ok := info.Get("file tree")
	if !ok || tree.Kind != KindDictionary {
		return fmt.Errorf("v2 torrent has no file tree")
	}
	meta.MultiFile = true
	if err := walkV2Tree(meta, tree, nil, 0); err != nil {
		return err
	}
	if len(meta.Files) == 0 {
		return fmt.Errorf("v2 file tree is empty")
	}
	if len(meta.Files) == 1 && len(meta.Files[0].RawPath) == 1 && string(meta.Files[0].RawPath[0]) == string(meta.NameRaw) {
		meta.MultiFile = false
	}
	return nil
}

func walkV2Tree(meta *MetaInfo, node *Node, prefix [][]byte, depth int) error {
	if depth > 64 {
		return fmt.Errorf("v2 file tree exceeds 64 levels")
	}
	if _, leaf := node.Dict[""]; leaf && len(node.Dict) != 1 {
		return fmt.Errorf("v2 file-tree node is both a file and a directory")
	}
	keys := make([]string, 0, len(node.Dict))
	for key := range node.Dict {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		child := node.Dict[key]
		if key == "" {
			if depth == 0 {
				return fmt.Errorf("v2 file-tree root must not be a file")
			}
			if child.Kind != KindDictionary {
				return fmt.Errorf("invalid v2 file leaf")
			}
			leafPath := prefix
			if len(leafPath) == 0 {
				leafPath = [][]byte{meta.NameRaw}
			}
			length, ok := integer(child, "length")
			if !ok || length < 0 {
				return fmt.Errorf("v2 file has an invalid length")
			}
			file := File{Length: length}
			for _, component := range leafPath {
				file.RawPath = append(file.RawPath, append([]byte(nil), component...))
				file.Path = append(file.Path, displayBytes(component))
				file.PathRawBase64 = append(file.PathRawBase64, base64.StdEncoding.EncodeToString(component))
			}
			if attr, ok := bytesValue(child, "attr"); ok {
				file.Attribute = string(attr)
			}
			if strings.Contains(file.Attribute, "p") {
				return fmt.Errorf("v2 file tree must not contain padding files")
			}
			if strings.Contains(file.Attribute, "l") {
				return fmt.Errorf("v2 symbolic-link files are not supported")
			}
			if root, ok := bytesValue(child, "pieces root"); ok {
				if len(root) != sha256.Size {
					return fmt.Errorf("v2 file pieces root is not 32 bytes")
				}
				file.PiecesRoot = hex.EncodeToString(root)
				file.piecesRootRaw = append([]byte(nil), root...)
			} else if length > 0 {
				return fmt.Errorf("non-empty v2 file has no pieces root")
			}
			if err := addLength(meta, length); err != nil {
				return err
			}
			meta.Files = append(meta.Files, file)
			continue
		}
		if key == "" || child.Kind != KindDictionary || len(key) == 0 || !utf8.ValidString(key) {
			return fmt.Errorf("invalid v2 file-tree path component")
		}
		next := append(append([][]byte(nil), prefix...), []byte(key))
		if err := walkV2Tree(meta, child, next, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func reconcileHybridLayouts(v1, v2 *MetaInfo) error {
	for _, file := range v2.Files {
		if strings.Contains(file.Attribute, "p") {
			return fmt.Errorf("hybrid v2 layout must not use padding files")
		}
	}
	v2Files := nonPaddingFiles(v2.Files)
	if len(nonPaddingFiles(v1.Files)) != len(v2Files) {
		return fmt.Errorf("hybrid v1/v2 layouts have different non-padding file counts")
	}
	offset := int64(0)
	nonPaddingIndex := 0
	for fileIndex := range v1.Files {
		file := &v1.Files[fileIndex]
		if strings.Contains(file.Attribute, "p") {
			if nonPaddingIndex == 0 || nonPaddingIndex >= len(v2Files) || offset%v1.PieceLength == 0 {
				return fmt.Errorf("hybrid v1 padding file %d is leading, trailing, or starts at an aligned offset", fileIndex)
			}
			expected := v1.PieceLength - (offset % v1.PieceLength)
			if file.Length != expected {
				return fmt.Errorf("hybrid v1 padding file %d has length %d, expected %d", fileIndex, file.Length, expected)
			}
			offset += file.Length
			continue
		}
		if nonPaddingIndex >= len(v2Files) {
			return fmt.Errorf("hybrid v1 layout has an extra non-padding file")
		}
		expected := v2Files[nonPaddingIndex]
		if file.Length != expected.Length || !sameRawPath(file.RawPath, expected.RawPath) {
			return fmt.Errorf("hybrid v1/v2 layouts disagree at non-padding file %d", nonPaddingIndex)
		}
		if file.Length > 0 && offset%v1.PieceLength != 0 {
			return fmt.Errorf("hybrid non-padding file %d does not start on a piece boundary", nonPaddingIndex)
		}
		file.PiecesRoot = expected.PiecesRoot
		file.piecesRootRaw = append([]byte(nil), expected.piecesRootRaw...)
		file.pieceLayerRaw = expected.pieceLayerRaw
		offset += file.Length
		nonPaddingIndex++
	}
	if nonPaddingIndex != len(v2Files) {
		return fmt.Errorf("hybrid v1 layout is missing non-padding files")
	}
	return nil
}

func nonPaddingFiles(files []File) []*File {
	items := make([]*File, 0, len(files))
	for i := range files {
		if !strings.Contains(files[i].Attribute, "p") {
			items = append(items, &files[i])
		}
	}
	return items
}

func sameRawPath(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func validateV2PieceLayers(root *Node, files []File, pieceLength int64) error {
	type expectedLayer struct {
		count       int64
		fileIndexes []int
	}
	expected := make(map[string]*expectedLayer)
	for fileIndex, file := range files {
		if strings.Contains(file.Attribute, "p") || file.Length <= pieceLength {
			continue
		}
		if len(file.piecesRootRaw) != sha256.Size {
			return fmt.Errorf("v2 file larger than one piece has no pieces root")
		}
		count := ((file.Length - 1) / pieceLength) + 1
		key := string(file.piecesRootRaw)
		entry, ok := expected[key]
		if ok {
			if entry.count != count {
				return fmt.Errorf("v2 files sharing a pieces root disagree on piece-layer length")
			}
			entry.fileIndexes = append(entry.fileIndexes, fileIndex)
			continue
		}
		expected[key] = &expectedLayer{count: count, fileIndexes: []int{fileIndex}}
	}
	layers, present := root.Get("piece layers")
	if !present {
		if len(expected) != 0 {
			return fmt.Errorf("v2 metafile is missing required piece layers")
		}
		return nil
	}
	if layers.Kind != KindDictionary {
		return fmt.Errorf("v2 piece layers is not a dictionary")
	}
	if len(layers.Dict) != len(expected) {
		return fmt.Errorf("v2 piece layers contains missing or unexpected roots")
	}
	for key, node := range layers.Dict {
		entry, ok := expected[key]
		if !ok || len(key) != sha256.Size || node.Kind != KindBytes || entry.count > int64(^uint64(0)>>1)/sha256.Size || int64(len(node.Bytes)) != entry.count*sha256.Size {
			return fmt.Errorf("v2 piece layer has an unexpected root or length")
		}
		computed, err := reduceV2PieceLayer(node.Bytes, entry.count, pieceLength)
		if err != nil {
			return fmt.Errorf("validate v2 piece layer: %w", err)
		}
		if !bytes.Equal(computed[:], []byte(key)) {
			return fmt.Errorf("v2 piece layer does not hash to its file pieces root")
		}
		layer := append([]byte(nil), node.Bytes...)
		for _, fileIndex := range entry.fileIndexes {
			files[fileIndex].pieceLayerRaw = layer
		}
	}
	return nil
}

func addLength(meta *MetaInfo, length int64) error {
	if length < 0 || meta.TotalLength > int64(^uint64(0)>>1)-length {
		return fmt.Errorf("torrent total length overflows int64")
	}
	meta.TotalLength += length
	return nil
}

func preferred(dict *Node, keys ...string) (*Node, bool) {
	for _, key := range keys {
		if node, ok := dict.Get(key); ok {
			return node, true
		}
	}
	return nil, false
}

func preferredBytes(dict *Node, keys ...string) (*Node, bool) {
	node, ok := preferred(dict, keys...)
	return node, ok && node.Kind == KindBytes
}

func validateAlternateBytes(dict *Node, preferredKey, legacyKey string) error {
	preferredNode, hasPreferred := dict.Get(preferredKey)
	legacyNode, hasLegacy := dict.Get(legacyKey)
	if !hasPreferred || !hasLegacy {
		return nil
	}
	if preferredNode.Kind != KindBytes || legacyNode.Kind != KindBytes || !bytes.Equal(preferredNode.Bytes, legacyNode.Bytes) {
		return fmt.Errorf("%s and %s disagree", preferredKey, legacyKey)
	}
	return nil
}

func validateAlternatePath(dict *Node, fileIndex int) error {
	preferredNode, hasPreferred := dict.Get("path.utf-8")
	legacyNode, hasLegacy := dict.Get("path")
	if !hasPreferred || !hasLegacy {
		return nil
	}
	if preferredNode.Kind != KindList || legacyNode.Kind != KindList || len(preferredNode.List) != len(legacyNode.List) {
		return fmt.Errorf("file %d path.utf-8 and path disagree", fileIndex)
	}
	for i := range preferredNode.List {
		if preferredNode.List[i].Kind != KindBytes || legacyNode.List[i].Kind != KindBytes || !bytes.Equal(preferredNode.List[i].Bytes, legacyNode.List[i].Bytes) {
			return fmt.Errorf("file %d path.utf-8 and path disagree", fileIndex)
		}
	}
	return nil
}

func integer(dict *Node, key string) (int64, bool) {
	node, ok := dict.Get(key)
	return func() (int64, bool) {
		if !ok || node.Kind != KindInteger {
			return 0, false
		}
		return node.Int, true
	}()
}

func bytesValue(dict *Node, key string) ([]byte, bool) {
	node, ok := dict.Get(key)
	if !ok || node.Kind != KindBytes {
		return nil, false
	}
	return node.Bytes, true
}

func displayBytes(raw []byte) string {
	if utf8.Valid(raw) {
		return string(raw)
	}
	return strings.ToValidUTF8(string(raw), "�")
}

func trackerOrigins(root *Node) []string {
	seen := map[string]struct{}{}
	var add = func(raw []byte) {
		u, err := url.Parse(string(raw))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "udp") || u.Host == "" {
			return
		}
		origin := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
		seen[origin] = struct{}{}
	}
	if announce, ok := bytesValue(root, "announce"); ok {
		add(announce)
	}
	if lists, ok := root.Get("announce-list"); ok && lists.Kind == KindList {
		for _, tier := range lists.List {
			if tier.Kind == KindBytes {
				add(tier.Bytes)
				continue
			}
			if tier.Kind == KindList {
				for _, item := range tier.List {
					if item.Kind == KindBytes {
						add(item.Bytes)
					}
				}
			}
		}
	}
	origins := make([]string, 0, len(seen))
	for origin := range seen {
		origins = append(origins, origin)
	}
	sort.Strings(origins)
	return origins
}
