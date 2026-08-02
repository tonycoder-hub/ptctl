package metafile

import (
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
)

type File struct {
	Path          []string `json:"path"`
	PathRawBase64 []string `json:"path_raw_base64"`
	Length        int64    `json:"length"`
	Attribute     string   `json:"attribute,omitempty"`
	PiecesRoot    string   `json:"pieces_root,omitempty"`
	RawPath       [][]byte `json:"-"`
}

type MetaInfo struct {
	Name          string   `json:"name"`
	NameRawBase64 string   `json:"name_raw_base64"`
	Version       string   `json:"version"`
	InfoHashV1    string   `json:"info_hash_v1,omitempty"`
	InfoHashV2    string   `json:"info_hash_v2,omitempty"`
	Private       bool     `json:"private"`
	MultiFile     bool     `json:"multi_file"`
	PieceLength   int64    `json:"piece_length"`
	PieceCount    int      `json:"piece_count"`
	TotalLength   int64    `json:"total_length"`
	Trackers      []string `json:"tracker_origins"`
	Files         []File   `json:"files"`
	NameRaw       []byte   `json:"-"`
	pieceHashes   [][20]byte
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
	meta := &MetaInfo{}
	nameNode, ok := preferredBytes(info, "name.utf-8", "name")
	if !ok || len(nameNode.Bytes) == 0 {
		return nil, fmt.Errorf("info dictionary has no name")
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
	if private, ok := integer(info, "private"); ok {
		meta.Private = private == 1
	}

	pieces, hasV1 := bytesValue(info, "pieces")
	metaVersion, hasMetaVersion := integer(info, "meta version")
	hasV2 := hasMetaVersion && metaVersion == 2
	if hasMetaVersion && metaVersion != 2 {
		return nil, fmt.Errorf("unsupported meta version %d", metaVersion)
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

	if hasV1 {
		if err := parseV1Files(meta, info); err != nil {
			return nil, err
		}
	} else {
		if err := parseV2Files(meta, info); err != nil {
			return nil, err
		}
	}
	meta.PieceCount = len(meta.pieceHashes)
	if hasV1 {
		expectedPieces := 0
		if meta.TotalLength > 0 {
			expectedPieces = int((meta.TotalLength + meta.PieceLength - 1) / meta.PieceLength)
		}
		if meta.PieceLength == 0 || meta.PieceCount != expectedPieces {
			return nil, fmt.Errorf("piece count %d does not match total length and piece length (expected %d)", meta.PieceCount, expectedPieces)
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
			file := File{Length: length}
			if attr, ok := bytesValue(item, "attr"); ok {
				file.Attribute = string(attr)
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
	keys := make([]string, 0, len(node.Dict))
	for key := range node.Dict {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		child := node.Dict[key]
		if key == "" {
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
			if root, ok := bytesValue(child, "pieces root"); ok {
				if len(root) != sha256.Size {
					return fmt.Errorf("v2 file pieces root is not 32 bytes")
				}
				file.PiecesRoot = hex.EncodeToString(root)
			}
			if err := addLength(meta, length); err != nil {
				return err
			}
			meta.Files = append(meta.Files, file)
			continue
		}
		if key == "" || child.Kind != KindDictionary || len(key) == 0 {
			return fmt.Errorf("invalid v2 file-tree path component")
		}
		next := append(append([][]byte(nil), prefix...), []byte(key))
		if err := walkV2Tree(meta, child, next, depth+1); err != nil {
			return err
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
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
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
