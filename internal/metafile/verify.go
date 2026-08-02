package metafile

import (
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/storage"
)

type VerificationResult struct {
	Version          string `json:"version"`
	Evidence         string `json:"evidence"`
	Verified         bool   `json:"verified"`
	BytesVerified    int64  `json:"bytes_verified"`
	FilesChecked     int    `json:"files_checked"`
	PiecesExpected   int    `json:"pieces_expected"`
	PiecesMatched    int    `json:"pieces_matched"`
	MismatchPieces   []int  `json:"mismatch_pieces,omitempty"`
	MismatchOverflow int    `json:"mismatch_overflow,omitempty"`
}

type fileSpec struct {
	path       string
	length     int64
	padding    bool
	sizeBefore int64
	modBefore  time.Time
}

// VerifyV1 verifies the exact v1 piece stream. Pieces are hashed across file
// boundaries; a per-file checksum is not a valid BitTorrent verification.
func VerifyV1(ctx context.Context, meta *MetaInfo, contentPath string) (VerificationResult, error) {
	result := VerificationResult{Version: meta.Version, Evidence: "v1-sha1-pieces", PiecesExpected: len(meta.pieceHashes)}
	if len(meta.pieceHashes) == 0 {
		return result, fmt.Errorf("v1 verification is unavailable for a pure v2 torrent")
	}
	if meta.PieceLength <= 0 || meta.PieceLength > 64<<20 {
		return result, fmt.Errorf("invalid piece length")
	}
	specs, err := resolveFiles(meta, contentPath)
	if err != nil {
		return result, err
	}
	result.FilesChecked = len(specs)
	reader := &sequenceReader{specs: specs}
	defer reader.Close()
	buffer := make([]byte, int(meta.PieceLength))
	remainingTotal := meta.TotalLength
	for index, expected := range meta.pieceHashes {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}
		pieceBytes := meta.PieceLength
		if remainingTotal < pieceBytes {
			pieceBytes = remainingTotal
		}
		if pieceBytes < 0 {
			return result, fmt.Errorf("negative remaining byte count")
		}
		if _, err := io.ReadFull(reader, buffer[:int(pieceBytes)]); err != nil {
			return result, fmt.Errorf("read piece %d: %w", index, err)
		}
		actual := sha1.Sum(buffer[:int(pieceBytes)])
		if actual == expected {
			result.PiecesMatched++
		} else if len(result.MismatchPieces) < 100 {
			result.MismatchPieces = append(result.MismatchPieces, index)
		} else {
			result.MismatchOverflow++
		}
		result.BytesVerified += pieceBytes
		remainingTotal -= pieceBytes
	}
	if remainingTotal != 0 {
		return result, fmt.Errorf("piece list ended with %d bytes unverified", remainingTotal)
	}
	var extra [1]byte
	if n, err := reader.Read(extra[:]); n != 0 || (err != nil && err != io.EOF) {
		return result, fmt.Errorf("content contains bytes beyond the torrent manifest")
	}
	if err := ensureStable(specs); err != nil {
		return result, err
	}
	result.Verified = result.PiecesMatched == result.PiecesExpected
	return result, nil
}

func resolveFiles(meta *MetaInfo, contentPath string) ([]fileSpec, error) {
	semantics := storage.CurrentSemantics()
	manifestPaths := make([][][]byte, len(meta.Files))
	for i := range meta.Files {
		manifestPaths[i] = meta.Files[i].RawPath
	}
	if err := storage.ValidateManifestPaths(manifestPaths, semantics); err != nil {
		return nil, fmt.Errorf("unsafe torrent manifest: %w", err)
	}

	if !meta.MultiFile {
		info, err := os.Lstat(contentPath)
		if err != nil {
			return nil, fmt.Errorf("inspect content path: %w", err)
		}
		path := contentPath
		if info.IsDir() {
			path, err = storage.SecureJoinExisting(contentPath, meta.Files[0].RawPath, semantics)
			if err != nil {
				return nil, err
			}
		} else if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("single-file content path is a symlink")
		}
		return preflight([]fileSpec{{path: path, length: meta.Files[0].Length, padding: strings.Contains(meta.Files[0].Attribute, "p")}})
	}

	rootInfo, err := os.Stat(contentPath)
	if err != nil {
		return nil, fmt.Errorf("inspect content root: %w", err)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("multi-file content path must be a directory representing the torrent root")
	}
	specs := make([]fileSpec, 0, len(meta.Files))
	for _, file := range meta.Files {
		padding := strings.Contains(file.Attribute, "p")
		if padding {
			specs = append(specs, fileSpec{length: file.Length, padding: true})
			continue
		}
		path, err := storage.SecureJoinExisting(contentPath, file.RawPath, semantics)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", filepath.Join(file.Path...), err)
		}
		specs = append(specs, fileSpec{path: path, length: file.Length})
	}
	return preflight(specs)
}

func preflight(specs []fileSpec) ([]fileSpec, error) {
	for i := range specs {
		if specs[i].padding {
			continue
		}
		info, err := os.Stat(specs[i].path)
		if err != nil {
			return nil, fmt.Errorf("inspect content file %q: %w", specs[i].path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("content path %q is not a regular file", specs[i].path)
		}
		if info.Size() != specs[i].length {
			return nil, fmt.Errorf("content file %q has size %d, expected %d", specs[i].path, info.Size(), specs[i].length)
		}
		specs[i].sizeBefore = info.Size()
		specs[i].modBefore = info.ModTime()
	}
	return specs, nil
}

func ensureStable(specs []fileSpec) error {
	for _, spec := range specs {
		if spec.padding {
			continue
		}
		info, err := os.Stat(spec.path)
		if err != nil {
			return fmt.Errorf("re-stat content file %q: %w", spec.path, err)
		}
		if info.Size() != spec.sizeBefore || !info.ModTime().Equal(spec.modBefore) {
			return fmt.Errorf("content file changed while hashing: %q", spec.path)
		}
	}
	return nil
}

type sequenceReader struct {
	specs         []fileSpec
	index         int
	current       *os.File
	currentRemain int64
}

func (r *sequenceReader) Read(p []byte) (int, error) {
	for len(p) > 0 {
		if r.index >= len(r.specs) {
			return 0, io.EOF
		}
		spec := r.specs[r.index]
		if r.currentRemain == 0 {
			r.currentRemain = spec.length
			if !spec.padding && r.current == nil {
				file, err := os.Open(spec.path)
				if err != nil {
					return 0, err
				}
				r.current = file
			}
			if spec.length == 0 {
				r.advance()
				continue
			}
		}
		limit := int64(len(p))
		if r.currentRemain < limit {
			limit = r.currentRemain
		}
		if spec.padding {
			clear(p[:int(limit)])
			r.currentRemain -= limit
			if r.currentRemain == 0 {
				r.advance()
			}
			return int(limit), nil
		}
		n, err := r.current.Read(p[:int(limit)])
		r.currentRemain -= int64(n)
		if err != nil && err != io.EOF {
			return n, err
		}
		if n == 0 && err == io.EOF && r.currentRemain != 0 {
			return 0, io.ErrUnexpectedEOF
		}
		if r.currentRemain == 0 {
			r.advance()
		}
		if n > 0 {
			return n, nil
		}
	}
	return 0, nil
}

func (r *sequenceReader) advance() {
	if r.current != nil {
		_ = r.current.Close()
		r.current = nil
	}
	r.currentRemain = 0
	r.index++
}

func (r *sequenceReader) Close() error {
	if r.current != nil {
		return r.current.Close()
	}
	return nil
}
