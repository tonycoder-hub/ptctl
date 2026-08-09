package metafile

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/storage"
)

type VerificationResult struct {
	Version            string              `json:"version"`
	Evidence           string              `json:"evidence"`
	Verified           bool                `json:"verified"`
	BytesVerified      int64               `json:"bytes_verified"`
	ProofStreamBytes   int64               `json:"proof_stream_bytes,omitempty"`
	FilesChecked       int                 `json:"files_checked"`
	PaddingBytes       int64               `json:"virtual_padding_bytes,omitempty"`
	SourceSnapshotID   string              `json:"source_snapshot_id,omitempty"`
	StabilityAssurance string              `json:"stability_assurance"`
	PiecesExpected     int                 `json:"pieces_expected"`
	PiecesMatched      int                 `json:"pieces_matched"`
	RootsExpected      int                 `json:"roots_expected,omitempty"`
	RootsMatched       int                 `json:"roots_matched,omitempty"`
	MismatchPieces     []int               `json:"mismatch_pieces,omitempty"`
	MismatchOverflow   int                 `json:"mismatch_overflow,omitempty"`
	Checks             []VerificationCheck `json:"checks"`
	snapshots          []snapshotRecord
	snapshotAuthority  bool
}

type VerificationCheck struct {
	Algorithm        string `json:"algorithm"`
	Evidence         string `json:"evidence"`
	Verified         bool   `json:"verified"`
	BytesVerified    int64  `json:"bytes_verified"`
	ProofStreamBytes int64  `json:"proof_stream_bytes,omitempty"`
	FilesChecked     int    `json:"files_checked"`
	PaddingBytes     int64  `json:"virtual_padding_bytes,omitempty"`
	PiecesExpected   int    `json:"pieces_expected"`
	PiecesMatched    int    `json:"pieces_matched"`
	RootsExpected    int    `json:"roots_expected,omitempty"`
	RootsMatched     int    `json:"roots_matched,omitempty"`
	MismatchPieces   []int  `json:"mismatch_pieces,omitempty"`
	MismatchOverflow int    `json:"mismatch_overflow,omitempty"`
}

// PublicCopy removes process-local filesystem snapshot authority while
// preserving the serializable verification evidence. The returned slices do
// not alias the original result.
func (r VerificationResult) PublicCopy() VerificationResult {
	if r.MismatchPieces != nil {
		r.MismatchPieces = append([]int(nil), r.MismatchPieces...)
	}
	if r.Checks != nil {
		r.Checks = append([]VerificationCheck(nil), r.Checks...)
		for i := range r.Checks {
			if r.Checks[i].MismatchPieces != nil {
				r.Checks[i].MismatchPieces = append([]int(nil), r.Checks[i].MismatchPieces...)
			}
		}
	}
	r.snapshots = nil
	r.snapshotAuthority = false
	return r
}

type SourcePrecondition struct {
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
}

type fileSpec struct {
	path       string
	length     int64
	padding    bool
	empty      bool
	open       func() (*os.File, error)
	sizeBefore int64
	modBefore  time.Time
	infoBefore os.FileInfo
}

type snapshotRecord struct {
	path string
	info os.FileInfo
}

// Verify selects the exact verifier required by the metafile. A hybrid is
// accepted only when the same physical reads validate both its v1 piece stream
// and every v2 file Merkle root.
func Verify(ctx context.Context, meta *MetaInfo, contentPath string) (VerificationResult, error) {
	switch meta.Version {
	case "v1":
		return VerifyV1(ctx, meta, contentPath)
	case "v2":
		return VerifyV2(ctx, meta, contentPath)
	case "hybrid":
		return verifyHybrid(ctx, meta, contentPath)
	default:
		return VerificationResult{}, fmt.Errorf("unsupported metafile version %q", meta.Version)
	}
}

// VerifyV1 verifies the exact v1 piece stream. Pieces are hashed across file
// boundaries; a per-file checksum is not a valid BitTorrent verification.
func VerifyV1(ctx context.Context, meta *MetaInfo, contentPath string) (VerificationResult, error) {
	result := VerificationResult{Version: meta.Version, Evidence: "v1-sha1-pieces", StabilityAssurance: "file_identity_size_mtime_checked_non_atomic", PiecesExpected: len(meta.pieceHashes)}
	if meta.InfoHashV1 == "" {
		return result, fmt.Errorf("v1 verification is unavailable for a pure v2 torrent")
	}
	if meta.PieceLength <= 0 || meta.PieceLength > 64<<20 {
		return result, fmt.Errorf("invalid piece length")
	}
	specs, err := resolveFiles(ctx, meta, contentPath)
	if err != nil {
		return result, err
	}
	return verifyV1Resolved(ctx, meta, specs)
}

func verifyV1Resolved(ctx context.Context, meta *MetaInfo, specs []fileSpec) (VerificationResult, error) {
	result := VerificationResult{Version: meta.Version, Evidence: "v1-sha1-pieces", StabilityAssurance: "file_identity_size_mtime_checked_non_atomic", PiecesExpected: len(meta.pieceHashes)}
	if meta.InfoHashV1 == "" {
		return result, fmt.Errorf("v1 verification is unavailable for a pure v2 torrent")
	}
	if meta.PieceLength <= 0 || meta.PieceLength > 64<<20 {
		return result, fmt.Errorf("invalid piece length")
	}
	if len(specs) != len(meta.Files) {
		return result, fmt.Errorf("resolved file layout does not match v1 manifest")
	}
	for _, spec := range specs {
		if spec.padding {
			result.PaddingBytes += spec.length
		} else if !spec.empty {
			result.FilesChecked++
		}
	}
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
		result.ProofStreamBytes += pieceBytes
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
	result.SourceSnapshotID = sourceSnapshotID(specs)
	for _, spec := range specs {
		if !spec.padding && !spec.empty {
			result.snapshots = append(result.snapshots, snapshotRecord{path: spec.path, info: spec.infoBefore})
		}
	}
	result.snapshotAuthority = true
	result.BytesVerified = result.ProofStreamBytes - result.PaddingBytes
	result.Verified = result.PiecesMatched == result.PiecesExpected
	result.Checks = []VerificationCheck{verificationCheck("bt-v1", result)}
	return result, nil
}

// VerifyV2 verifies each non-padding file independently using BEP 52's
// 16 KiB SHA-256 Merkle tree. Unlike v1, v2 pieces never span files.
func VerifyV2(ctx context.Context, meta *MetaInfo, contentPath string) (VerificationResult, error) {
	result := VerificationResult{Version: meta.Version, Evidence: "v2-sha256-merkle", StabilityAssurance: "file_identity_size_mtime_checked_non_atomic"}
	if meta.InfoHashV2 == "" {
		return result, fmt.Errorf("v2 verification is unavailable for a pure v1 torrent")
	}
	specs, err := resolveFiles(ctx, meta, contentPath)
	if err != nil {
		return result, err
	}
	return verifyV2Resolved(ctx, meta, specs, nil)
}

// verifyV2Resolved optionally mirrors every actual content byte, and every v1
// virtual padding byte, to proofStream. Hybrid verification uses that mirror to
// calculate its v1 proof from the exact same read that feeds the v2 trees.
func verifyV2Resolved(ctx context.Context, meta *MetaInfo, specs []fileSpec, proofStream io.Writer) (VerificationResult, error) {
	result := VerificationResult{Version: meta.Version, Evidence: "v2-sha256-merkle", StabilityAssurance: "file_identity_size_mtime_checked_non_atomic"}
	if meta.InfoHashV2 == "" {
		return result, fmt.Errorf("v2 verification is unavailable for a pure v1 torrent")
	}
	depth, err := v2PieceLayerDepth(meta.PieceLength)
	if err != nil {
		return result, err
	}
	blocksPerPiece := meta.PieceLength / v2BlockSize
	if len(specs) != len(meta.Files) {
		return result, fmt.Errorf("resolved file layout does not match v2 manifest")
	}
	buffer := make([]byte, int(v2BlockSize))
	zeroBuffer := make([]byte, int(v2BlockSize))
	rootsMatch := true
	for fileIndex, spec := range specs {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}
		fileMeta := meta.Files[fileIndex]
		if spec.padding {
			result.PaddingBytes += spec.length
			if proofStream != nil {
				if err := writeZeroBytes(ctx, proofStream, spec.length, zeroBuffer); err != nil {
					return result, fmt.Errorf("hash v1 virtual padding file %d: %w", fileIndex, err)
				}
			}
			continue
		}
		if spec.empty {
			continue
		}
		result.FilesChecked++
		file, err := openFileSpec(spec)
		if err != nil {
			return result, fmt.Errorf("open content file %q: %w", spec.path, err)
		}
		before, err := statOpenedContentPath(spec.path, file)
		if err != nil {
			_ = file.Close()
			return result, fmt.Errorf("stat open content file %q: %w", spec.path, err)
		}
		if !os.SameFile(spec.infoBefore, before) || before.Size() != spec.sizeBefore || !before.ModTime().Equal(spec.modBefore) {
			_ = file.Close()
			return result, fmt.Errorf("content file changed before hashing: %q", spec.path)
		}

		contentReader := io.Reader(file)
		if proofStream != nil {
			contentReader = io.TeeReader(file, proofStream)
		}
		var actualRoot v2Hash
		if spec.length > 0 {
			if len(fileMeta.piecesRootRaw) != sha256.Size {
				_ = file.Close()
				return result, fmt.Errorf("non-empty v2 file has no valid pieces root")
			}
			result.RootsExpected++
			if spec.length <= meta.PieceLength {
				blocks := ((spec.length - 1) / v2BlockSize) + 1
				targetBlocks, err := nextPowerOfTwo(blocks)
				if err != nil {
					_ = file.Close()
					return result, err
				}
				actualRoot, err = hashV2Segment(ctx, contentReader, spec.length, targetBlocks, buffer)
				if err != nil {
					_ = file.Close()
					return result, fmt.Errorf("hash v2 file %d: %w", fileIndex, err)
				}
				pieceIndex := result.PiecesExpected
				result.PiecesExpected++
				if bytes.Equal(actualRoot[:], fileMeta.piecesRootRaw) {
					result.PiecesMatched++
				} else {
					recordMismatch(&result, pieceIndex)
				}
				result.BytesVerified += spec.length
			} else {
				pieceCount := ((spec.length - 1) / meta.PieceLength) + 1
				if pieceCount > int64(^uint(0)>>1) || int64(len(fileMeta.pieceLayerRaw)) != pieceCount*sha256.Size {
					_ = file.Close()
					return result, fmt.Errorf("v2 file %d has no valid authenticated piece layer", fileIndex)
				}
				var rootAccumulator merkleAccumulator
				remaining := spec.length
				for localPiece := int64(0); localPiece < pieceCount; localPiece++ {
					pieceBytes := meta.PieceLength
					if remaining < pieceBytes {
						pieceBytes = remaining
					}
					pieceRoot, err := hashV2Segment(ctx, contentReader, pieceBytes, blocksPerPiece, buffer)
					if err != nil {
						_ = file.Close()
						return result, fmt.Errorf("hash v2 file %d piece %d: %w", fileIndex, localPiece, err)
					}
					pieceIndex := result.PiecesExpected
					result.PiecesExpected++
					start := int(localPiece * sha256.Size)
					if bytes.Equal(pieceRoot[:], fileMeta.pieceLayerRaw[start:start+sha256.Size]) {
						result.PiecesMatched++
					} else {
						recordMismatch(&result, pieceIndex)
					}
					if err := rootAccumulator.add(pieceRoot, depth); err != nil {
						_ = file.Close()
						return result, err
					}
					result.BytesVerified += pieceBytes
					remaining -= pieceBytes
				}
				targetPieces, err := nextPowerOfTwo(pieceCount)
				if err != nil {
					_ = file.Close()
					return result, err
				}
				zeroPiece := v2ZeroHash(depth)
				for piece := pieceCount; piece < targetPieces; piece++ {
					if err := rootAccumulator.add(zeroPiece, depth); err != nil {
						_ = file.Close()
						return result, err
					}
				}
				actualRoot, err = rootAccumulator.root()
				if err != nil {
					_ = file.Close()
					return result, err
				}
			}
			if bytes.Equal(actualRoot[:], fileMeta.piecesRootRaw) {
				result.RootsMatched++
			} else {
				rootsMatch = false
			}
		}

		var extra [1]byte
		if n, readErr := file.Read(extra[:]); n != 0 || (readErr != nil && readErr != io.EOF) {
			_ = file.Close()
			return result, fmt.Errorf("content file %q contains bytes beyond the torrent manifest", spec.path)
		}
		after, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return result, fmt.Errorf("re-stat open content file %q: %w", spec.path, err)
		}
		if !os.SameFile(before, after) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
			_ = file.Close()
			return result, fmt.Errorf("content file changed while hashing: %q", spec.path)
		}
		if err := file.Close(); err != nil {
			return result, fmt.Errorf("close content file %q: %w", spec.path, err)
		}
	}
	if err := ensureStable(specs); err != nil {
		return result, err
	}
	result.SourceSnapshotID = sourceSnapshotID(specs)
	for _, spec := range specs {
		if !spec.padding && !spec.empty {
			result.snapshots = append(result.snapshots, snapshotRecord{path: spec.path, info: spec.infoBefore})
		}
	}
	result.snapshotAuthority = true
	result.Verified = rootsMatch && result.PiecesMatched == result.PiecesExpected && result.RootsMatched == result.RootsExpected
	result.ProofStreamBytes = result.BytesVerified
	result.Checks = []VerificationCheck{verificationCheck("bt-v2", result)}
	return result, nil
}

func verifyHybrid(ctx context.Context, meta *MetaInfo, contentPath string) (VerificationResult, error) {
	if meta.InfoHashV1 == "" || meta.InfoHashV2 == "" {
		return VerificationResult{}, fmt.Errorf("hybrid verification requires both v1 and v2 metadata")
	}
	specs, err := resolveFiles(ctx, meta, contentPath)
	if err != nil {
		return VerificationResult{}, err
	}
	return verifyHybridResolved(ctx, meta, specs)
}

func verifyHybridResolved(ctx context.Context, meta *MetaInfo, specs []fileSpec) (VerificationResult, error) {
	if meta.InfoHashV1 == "" || meta.InfoHashV2 == "" {
		return VerificationResult{}, fmt.Errorf("hybrid verification requires both v1 and v2 metadata")
	}
	v1Stream := newV1ProofStream(ctx, meta)
	v2, err := verifyV2Resolved(ctx, meta, specs, v1Stream)
	if err != nil {
		return VerificationResult{}, err
	}
	if err := v1Stream.complete(); err != nil {
		return VerificationResult{}, err
	}
	if v1Stream.result.ProofStreamBytes != meta.TotalLength {
		return VerificationResult{}, fmt.Errorf("hybrid proof stream covered %d bytes, expected %d", v1Stream.result.ProofStreamBytes, meta.TotalLength)
	}
	v1 := v1Stream.result
	v1.FilesChecked = v2.FilesChecked
	v1.PaddingBytes = v2.PaddingBytes
	v1.BytesVerified = v2.BytesVerified
	v1.SourceSnapshotID = v2.SourceSnapshotID
	v1.snapshots = v2.snapshots
	v1.snapshotAuthority = v2.snapshotAuthority
	v1.Verified = v1.PiecesMatched == v1.PiecesExpected
	v1.Checks = []VerificationCheck{verificationCheck("bt-v1", v1)}

	if v1.PiecesExpected > int(^uint(0)>>1)-v2.PiecesExpected || v1.PiecesMatched > int(^uint(0)>>1)-v2.PiecesMatched {
		return VerificationResult{}, fmt.Errorf("hybrid verification piece count overflows int")
	}
	result := VerificationResult{
		Version:            "hybrid",
		Evidence:           "v1-sha1-pieces+v2-sha256-merkle",
		Verified:           v1.Verified && v2.Verified,
		BytesVerified:      v2.BytesVerified,
		ProofStreamBytes:   v1.ProofStreamBytes,
		FilesChecked:       v2.FilesChecked,
		PaddingBytes:       v1.PaddingBytes,
		SourceSnapshotID:   v2.SourceSnapshotID,
		StabilityAssurance: "single_read_file_identity_size_mtime_checked_non_atomic",
		PiecesExpected:     v1.PiecesExpected + v2.PiecesExpected,
		PiecesMatched:      v1.PiecesMatched + v2.PiecesMatched,
		RootsExpected:      v2.RootsExpected,
		RootsMatched:       v2.RootsMatched,
		Checks:             append(append([]VerificationCheck(nil), v1.Checks...), v2.Checks...),
		snapshots:          v2.snapshots,
		snapshotAuthority:  v2.snapshotAuthority,
	}
	return result, nil
}

type v1ProofStream struct {
	ctx         context.Context
	pieceLength int64
	expected    [][sha1.Size]byte
	hasher      hash.Hash
	pieceBytes  int64
	pieceIndex  int
	result      VerificationResult
}

func newV1ProofStream(ctx context.Context, meta *MetaInfo) *v1ProofStream {
	return &v1ProofStream{
		ctx:         ctx,
		pieceLength: meta.PieceLength,
		expected:    meta.pieceHashes,
		hasher:      sha1.New(),
		result: VerificationResult{
			Version:        meta.Version,
			Evidence:       "v1-sha1-pieces",
			PiecesExpected: len(meta.pieceHashes),
		},
	}
}

func (stream *v1ProofStream) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		select {
		case <-stream.ctx.Done():
			return written, stream.ctx.Err()
		default:
		}
		pieceRemaining := stream.pieceLength - stream.pieceBytes
		if pieceRemaining <= 0 {
			return written, fmt.Errorf("invalid v1 proof-stream piece boundary")
		}
		chunk := len(data)
		if int64(chunk) > pieceRemaining {
			chunk = int(pieceRemaining)
		}
		n, err := stream.hasher.Write(data[:chunk])
		written += n
		stream.pieceBytes += int64(n)
		stream.result.ProofStreamBytes += int64(n)
		if err != nil {
			return written, err
		}
		if n != chunk {
			return written, io.ErrShortWrite
		}
		data = data[chunk:]
		if stream.pieceBytes == stream.pieceLength {
			if err := stream.finishPiece(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (stream *v1ProofStream) finishPiece() error {
	if stream.pieceIndex >= len(stream.expected) {
		return fmt.Errorf("content contains bytes beyond the v1 piece manifest")
	}
	actualBytes := stream.hasher.Sum(nil)
	var actual [sha1.Size]byte
	copy(actual[:], actualBytes)
	if actual == stream.expected[stream.pieceIndex] {
		stream.result.PiecesMatched++
	} else {
		recordMismatch(&stream.result, stream.pieceIndex)
	}
	stream.pieceIndex++
	stream.pieceBytes = 0
	stream.hasher.Reset()
	return nil
}

func (stream *v1ProofStream) complete() error {
	if stream.pieceBytes > 0 {
		if err := stream.finishPiece(); err != nil {
			return err
		}
	}
	if stream.pieceIndex != len(stream.expected) {
		return fmt.Errorf("v1 piece list ended with %d proofs unobserved", len(stream.expected)-stream.pieceIndex)
	}
	return nil
}

func writeZeroBytes(ctx context.Context, writer io.Writer, count int64, buffer []byte) error {
	for count > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		chunk := int64(len(buffer))
		if count < chunk {
			chunk = count
		}
		n, err := writer.Write(buffer[:int(chunk)])
		count -= int64(n)
		if err != nil {
			return err
		}
		if int64(n) != chunk {
			return io.ErrShortWrite
		}
	}
	return nil
}

func recordMismatch(result *VerificationResult, piece int) {
	if len(result.MismatchPieces) < 100 {
		result.MismatchPieces = append(result.MismatchPieces, piece)
	} else {
		result.MismatchOverflow++
	}
}

func verificationCheck(algorithm string, result VerificationResult) VerificationCheck {
	return VerificationCheck{
		Algorithm:        algorithm,
		Evidence:         result.Evidence,
		Verified:         result.Verified,
		BytesVerified:    result.BytesVerified,
		ProofStreamBytes: result.ProofStreamBytes,
		FilesChecked:     result.FilesChecked,
		PaddingBytes:     result.PaddingBytes,
		PiecesExpected:   result.PiecesExpected,
		PiecesMatched:    result.PiecesMatched,
		RootsExpected:    result.RootsExpected,
		RootsMatched:     result.RootsMatched,
		MismatchPieces:   append([]int(nil), result.MismatchPieces...),
		MismatchOverflow: result.MismatchOverflow,
	}
}

// MatchSourceSnapshot ensures a planned source is the same file object and
// metadata snapshot that was observed during piece verification.
func (r VerificationResult) MatchSourceSnapshot(path string) (SourcePrecondition, error) {
	if !r.snapshotAuthority {
		return SourcePrecondition{}, fmt.Errorf("verification result has no process-local source snapshot authority")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourcePrecondition{}, fmt.Errorf("re-stat planned source %q: %w", path, err)
	}
	for _, snapshot := range r.snapshots {
		if !sameSnapshotPath(snapshot.path, path) {
			continue
		}
		if os.SameFile(snapshot.info, info) && snapshot.info.Size() == info.Size() && snapshot.info.ModTime().Equal(info.ModTime()) {
			return SourcePrecondition{SizeBytes: info.Size(), ModifiedAt: info.ModTime().UTC()}, nil
		}
	}
	return SourcePrecondition{}, fmt.Errorf("planned source %q no longer matches the verified file snapshot", path)
}

func sameSnapshotPath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if storage.CurrentSemantics().CaseSensitive {
		return a == b
	}
	return strings.EqualFold(a, b)
}

func resolveFiles(ctx context.Context, meta *MetaInfo, contentPath string) ([]fileSpec, error) {
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
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve content path: %w", err)
		}
		return preflight(ctx, []fileSpec{{path: path, length: meta.Files[0].Length, padding: strings.Contains(meta.Files[0].Attribute, "p")}})
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
	return preflight(ctx, specs)
}

func preflight(ctx context.Context, specs []fileSpec) ([]fileSpec, error) {
	for i := range specs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if specs[i].padding || specs[i].empty {
			continue
		}
		var info os.FileInfo
		if specs[i].open != nil {
			file, err := specs[i].open()
			if err != nil {
				return nil, fmt.Errorf("open identity-bound content file %q: %w", specs[i].path, err)
			}
			info, err = statOpenedContentPath(specs[i].path, file)
			closeErr := file.Close()
			if err != nil {
				return nil, fmt.Errorf("inspect identity-bound content file %q: %w", specs[i].path, err)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("close identity-bound content file %q: %w", specs[i].path, closeErr)
			}
		} else {
			var err error
			info, err = os.Lstat(specs[i].path)
			if err != nil {
				return nil, fmt.Errorf("inspect content file %q: %w", specs[i].path, err)
			}
			if storage.IsLinkLike(info) {
				return nil, fmt.Errorf("content path %q is a symbolic link or reparse point", specs[i].path)
			}
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("content path %q is not a regular file", specs[i].path)
		}
		if info.Size() != specs[i].length {
			return nil, fmt.Errorf("content file %q has size %d, expected %d", specs[i].path, info.Size(), specs[i].length)
		}
		specs[i].sizeBefore = info.Size()
		specs[i].modBefore = info.ModTime()
		specs[i].infoBefore = info
	}
	return specs, nil
}

func ensureStable(specs []fileSpec) error {
	for _, spec := range specs {
		if spec.padding || spec.empty {
			continue
		}
		var info os.FileInfo
		if spec.open != nil {
			file, err := spec.open()
			if err != nil {
				return fmt.Errorf("re-open identity-bound content file %q: %w", spec.path, err)
			}
			info, err = statOpenedContentPath(spec.path, file)
			closeErr := file.Close()
			if err != nil {
				return fmt.Errorf("re-stat identity-bound content file %q: %w", spec.path, err)
			}
			if closeErr != nil {
				return fmt.Errorf("close identity-bound content file %q: %w", spec.path, closeErr)
			}
		} else {
			var err error
			info, err = os.Stat(spec.path)
			if err != nil {
				return fmt.Errorf("re-stat content file %q: %w", spec.path, err)
			}
		}
		if !os.SameFile(spec.infoBefore, info) || info.Size() != spec.sizeBefore || !info.ModTime().Equal(spec.modBefore) {
			return fmt.Errorf("content file changed while hashing: %q", spec.path)
		}
	}
	return nil
}

func openFileSpec(spec fileSpec) (*os.File, error) {
	if spec.open != nil {
		return spec.open()
	}
	return os.Open(spec.path)
}

func statOpenedContentPath(path string, file *os.File) (os.FileInfo, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect named content path: %w", err)
	}
	if storage.IsLinkLike(pathInfo) || !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("named content path is not a regular non-link file")
	}
	if !os.SameFile(info, pathInfo) {
		return nil, fmt.Errorf("opened content file does not match its named path")
	}
	return info, nil
}

func sourceSnapshotID(specs []fileSpec) string {
	digest := sha256.New()
	for _, spec := range specs {
		if spec.padding || spec.empty {
			continue
		}
		fmt.Fprintf(digest, "%s\x00%d\x00%d\n", filepath.Clean(spec.path), spec.sizeBefore, spec.modBefore.UnixNano())
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

type sequenceReader struct {
	specs         []fileSpec
	index         int
	current       *os.File
	currentBefore os.FileInfo
	currentRemain int64
	pendingErr    error
}

func (r *sequenceReader) Read(p []byte) (int, error) {
	if r.pendingErr != nil {
		return 0, r.pendingErr
	}
	for len(p) > 0 {
		if r.index >= len(r.specs) {
			return 0, io.EOF
		}
		spec := r.specs[r.index]
		if r.currentRemain == 0 {
			r.currentRemain = spec.length
			if !spec.padding && !spec.empty && r.current == nil {
				file, err := openFileSpec(spec)
				if err != nil {
					return 0, err
				}
				before, err := statOpenedContentPath(spec.path, file)
				if err != nil {
					_ = file.Close()
					return 0, err
				}
				if !os.SameFile(spec.infoBefore, before) || before.Size() != spec.sizeBefore || !before.ModTime().Equal(spec.modBefore) {
					_ = file.Close()
					return 0, fmt.Errorf("content file changed before hashing: %q", spec.path)
				}
				r.current = file
				r.currentBefore = before
			}
			if spec.length == 0 {
				if err := r.advance(); err != nil {
					return 0, err
				}
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
				if err := r.advance(); err != nil {
					return int(limit), err
				}
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
			if advanceErr := r.advance(); advanceErr != nil {
				return n, advanceErr
			}
		}
		if n > 0 {
			return n, nil
		}
	}
	return 0, nil
}

func (r *sequenceReader) advance() error {
	var result error
	if r.current != nil {
		after, err := r.current.Stat()
		if err != nil {
			result = err
		} else if !os.SameFile(r.currentBefore, after) || after.Size() != r.currentBefore.Size() || !after.ModTime().Equal(r.currentBefore.ModTime()) {
			result = fmt.Errorf("content file changed while hashing: %q", r.specs[r.index].path)
		}
		if closeErr := r.current.Close(); result == nil && closeErr != nil {
			result = closeErr
		}
		r.current = nil
		r.currentBefore = nil
	}
	r.currentRemain = 0
	r.index++
	if result != nil {
		r.pendingErr = result
	}
	return result
}

func (r *sequenceReader) Close() error {
	if r.current != nil {
		return r.current.Close()
	}
	return nil
}
