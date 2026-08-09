package metafile

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type SourceBinding struct {
	FileIndex int                      `json:"file_index"`
	Path      string                   `json:"source_path"`
	Open      func() (*os.File, error) `json:"-"`
}

type SourceMap struct {
	Bindings []SourceBinding `json:"bindings"`
}

// VerifiedSource is an opaque, process-local verification observation. It
// binds a normalized source map to one exact metafile variant and retains the
// live file identities needed by the layout planner. It is not durable proof.
type VerifiedSource struct {
	metafileVariantID string
	bindings          []SourceBinding
	result            VerificationResult
}

func VerifySourceMap(ctx context.Context, meta *MetaInfo, source SourceMap) (*VerifiedSource, error) {
	if meta == nil {
		return nil, fmt.Errorf("metafile is nil")
	}
	specs, bindings, err := specsFromSourceMap(ctx, meta, source)
	if err != nil {
		return nil, err
	}
	result, err := verifyResolved(ctx, meta, specs)
	if err != nil {
		return nil, err
	}
	return &VerifiedSource{
		metafileVariantID: meta.MetafileVariantID,
		bindings:          bindings,
		result:            result,
	}, nil
}

// VerifyContentSource is the exact-layout compatibility entry point used by
// seed plan. It returns the same opaque observation as scattered source maps.
func VerifyContentSource(ctx context.Context, meta *MetaInfo, contentPath string) (*VerifiedSource, error) {
	if meta == nil {
		return nil, fmt.Errorf("metafile is nil")
	}
	specs, err := resolveFiles(ctx, meta, contentPath)
	if err != nil {
		return nil, err
	}
	result, err := verifyResolved(ctx, meta, specs)
	if err != nil {
		return nil, err
	}
	bindings := make([]SourceBinding, 0, len(specs))
	for fileIndex, spec := range specs {
		if spec.padding || spec.empty || spec.path == "" || spec.length == 0 {
			continue
		}
		bindings = append(bindings, SourceBinding{FileIndex: fileIndex, Path: filepath.Clean(spec.path)})
	}
	return &VerifiedSource{metafileVariantID: meta.MetafileVariantID, bindings: bindings, result: result}, nil
}

func (source *VerifiedSource) Matches(meta *MetaInfo) bool {
	return source != nil && meta != nil && source.metafileVariantID != "" && source.metafileVariantID == meta.MetafileVariantID
}

func (source *VerifiedSource) Result() VerificationResult {
	if source == nil {
		return VerificationResult{}
	}
	result := source.result
	result.MismatchPieces = append([]int(nil), result.MismatchPieces...)
	result.Checks = append([]VerificationCheck(nil), result.Checks...)
	for i := range result.Checks {
		result.Checks[i].MismatchPieces = append([]int(nil), result.Checks[i].MismatchPieces...)
	}
	result.snapshots = append([]snapshotRecord(nil), result.snapshots...)
	return result
}

func (source *VerifiedSource) Bindings() []SourceBinding {
	if source == nil {
		return nil
	}
	bindings := make([]SourceBinding, len(source.bindings))
	for index, binding := range source.bindings {
		bindings[index] = SourceBinding{FileIndex: binding.FileIndex, Path: binding.Path}
	}
	return bindings
}

func (source *VerifiedSource) Path(fileIndex int) (string, bool) {
	if source == nil {
		return "", false
	}
	position := sort.Search(len(source.bindings), func(i int) bool { return source.bindings[i].FileIndex >= fileIndex })
	if position >= len(source.bindings) || source.bindings[position].FileIndex != fileIndex {
		return "", false
	}
	return source.bindings[position].Path, true
}

func (source *VerifiedSource) SourcePrecondition(fileIndex int) (SourcePrecondition, error) {
	if source == nil {
		return SourcePrecondition{}, fmt.Errorf("verified source has no physical binding for manifest file %d", fileIndex)
	}
	position := sort.Search(len(source.bindings), func(i int) bool { return source.bindings[i].FileIndex >= fileIndex })
	if position >= len(source.bindings) || source.bindings[position].FileIndex != fileIndex {
		return SourcePrecondition{}, fmt.Errorf("verified source has no physical binding for manifest file %d", fileIndex)
	}
	binding := source.bindings[position]
	if binding.Open == nil {
		return source.result.MatchSourceSnapshot(binding.Path)
	}
	file, err := binding.Open()
	if err != nil {
		return SourcePrecondition{}, fmt.Errorf("re-open planned source %q: %w", binding.Path, err)
	}
	info, statErr := statOpenedContentPath(binding.Path, file)
	closeErr := file.Close()
	if statErr != nil {
		return SourcePrecondition{}, fmt.Errorf("re-stat planned source %q: %w", binding.Path, statErr)
	}
	if closeErr != nil {
		return SourcePrecondition{}, fmt.Errorf("close planned source %q: %w", binding.Path, closeErr)
	}
	for _, snapshot := range source.result.snapshots {
		if sameSnapshotPath(snapshot.path, binding.Path) && os.SameFile(snapshot.info, info) && snapshot.info.Size() == info.Size() && snapshot.info.ModTime().Equal(info.ModTime()) {
			return SourcePrecondition{SizeBytes: info.Size(), ModifiedAt: info.ModTime().UTC()}, nil
		}
	}
	return SourcePrecondition{}, fmt.Errorf("planned source %q no longer matches the verified file snapshot", binding.Path)
}

func specsFromSourceMap(ctx context.Context, meta *MetaInfo, source SourceMap) ([]fileSpec, []SourceBinding, error) {
	byIndex := make(map[int]SourceBinding, len(source.Bindings))
	for _, binding := range source.Bindings {
		if binding.FileIndex < 0 || binding.FileIndex >= len(meta.Files) {
			return nil, nil, fmt.Errorf("source binding file index %d is outside the manifest", binding.FileIndex)
		}
		if _, exists := byIndex[binding.FileIndex]; exists {
			return nil, nil, fmt.Errorf("source binding file index %d is duplicated", binding.FileIndex)
		}
		file := meta.Files[binding.FileIndex]
		if strings.Contains(file.Attribute, "p") {
			return nil, nil, fmt.Errorf("padding manifest file %d must not have a physical source binding", binding.FileIndex)
		}
		if binding.Path == "" || !filepath.IsAbs(binding.Path) {
			return nil, nil, fmt.Errorf("source binding file %d must use an absolute path", binding.FileIndex)
		}
		clean := filepath.Clean(binding.Path)
		binding.Path = clean
		byIndex[binding.FileIndex] = binding
	}

	specs := make([]fileSpec, len(meta.Files))
	normalized := make([]SourceBinding, 0, len(source.Bindings))
	for fileIndex, file := range meta.Files {
		if strings.Contains(file.Attribute, "p") {
			specs[fileIndex] = fileSpec{length: file.Length, padding: true}
			continue
		}
		binding, bound := byIndex[fileIndex]
		if !bound {
			if file.Length == 0 {
				specs[fileIndex] = fileSpec{length: 0, empty: true}
				continue
			}
			return nil, nil, fmt.Errorf("manifest file %d has no source binding", fileIndex)
		}
		specs[fileIndex] = fileSpec{path: binding.Path, length: file.Length, open: binding.Open}
		normalized = append(normalized, SourceBinding{FileIndex: fileIndex, Path: binding.Path, Open: binding.Open})
	}
	prepared, err := preflight(ctx, specs)
	if err != nil {
		return nil, nil, err
	}
	return prepared, normalized, nil
}

func verifyResolved(ctx context.Context, meta *MetaInfo, specs []fileSpec) (VerificationResult, error) {
	switch meta.Version {
	case "v1":
		return verifyV1Resolved(ctx, meta, specs)
	case "v2":
		return verifyV2Resolved(ctx, meta, specs, nil)
	case "hybrid":
		return verifyHybridResolved(ctx, meta, specs)
	default:
		return VerificationResult{}, fmt.Errorf("unsupported metafile version %q", meta.Version)
	}
}

type V2FileRootObservation struct {
	PiecesRoot   string             `json:"pieces_root"`
	BytesRead    int64              `json:"bytes_read"`
	Precondition SourcePrecondition `json:"source_precondition"`
}

// ObserveV2FileRoot computes the BEP 52 file tree root independently of the
// torrent piece length. Discovery can use it as an exact per-file candidate
// filter; hybrid layouts must still pass VerifySourceMap afterward.
func ObserveV2FileRoot(ctx context.Context, path string, expectedLength int64) (V2FileRootObservation, error) {
	return observeV2FileRoot(ctx, path, expectedLength, nil)
}

func observeV2FileRoot(ctx context.Context, path string, expectedLength int64, opener func() (*os.File, error)) (V2FileRootObservation, error) {
	if expectedLength <= 0 {
		return V2FileRootObservation{}, fmt.Errorf("v2 file-root observation requires a positive length")
	}
	if path == "" || !filepath.IsAbs(path) {
		return V2FileRootObservation{}, fmt.Errorf("v2 file-root path must be absolute")
	}
	specs, err := preflight(ctx, []fileSpec{{path: filepath.Clean(path), length: expectedLength, open: opener}})
	if err != nil {
		return V2FileRootObservation{}, err
	}
	spec := specs[0]
	file, err := openFileSpec(spec)
	if err != nil {
		return V2FileRootObservation{}, fmt.Errorf("open v2 candidate %q: %w", spec.path, err)
	}
	before, err := statOpenedContentPath(spec.path, file)
	if err != nil {
		_ = file.Close()
		return V2FileRootObservation{}, fmt.Errorf("stat open v2 candidate %q: %w", spec.path, err)
	}
	if !os.SameFile(spec.infoBefore, before) || before.Size() != spec.sizeBefore || !before.ModTime().Equal(spec.modBefore) {
		_ = file.Close()
		return V2FileRootObservation{}, fmt.Errorf("v2 candidate changed before hashing: %q", spec.path)
	}
	blocks := ((expectedLength - 1) / v2BlockSize) + 1
	targetBlocks, err := nextPowerOfTwo(blocks)
	if err != nil {
		_ = file.Close()
		return V2FileRootObservation{}, err
	}
	root, err := hashV2Segment(ctx, io.Reader(file), expectedLength, targetBlocks, make([]byte, int(v2BlockSize)))
	if err != nil {
		_ = file.Close()
		return V2FileRootObservation{}, fmt.Errorf("hash v2 candidate: %w", err)
	}
	var extra [1]byte
	if n, readErr := file.Read(extra[:]); n != 0 || (readErr != nil && readErr != io.EOF) {
		_ = file.Close()
		return V2FileRootObservation{}, fmt.Errorf("v2 candidate contains bytes beyond the expected length")
	}
	after, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return V2FileRootObservation{}, fmt.Errorf("re-stat v2 candidate: %w", err)
	}
	if !os.SameFile(before, after) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		_ = file.Close()
		return V2FileRootObservation{}, fmt.Errorf("v2 candidate changed while hashing")
	}
	if err := file.Close(); err != nil {
		return V2FileRootObservation{}, fmt.Errorf("close v2 candidate: %w", err)
	}
	if err := ensureStable(specs); err != nil {
		return V2FileRootObservation{}, err
	}
	return V2FileRootObservation{
		PiecesRoot: hex.EncodeToString(root[:]),
		BytesRead:  expectedLength,
		Precondition: SourcePrecondition{
			SizeBytes:  expectedLength,
			ModifiedAt: before.ModTime().UTC(),
		},
	}, nil
}
