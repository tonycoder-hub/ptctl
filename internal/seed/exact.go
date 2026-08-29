package seed

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

// ExactSourceOptions controls the public projection of an explicitly selected
// exact-layout source. It does not turn the source into a search root and never
// claims filesystem-wide uniqueness.
type ExactSourceOptions struct {
	ShowAbsolutePaths bool
	TimeBudget        time.Duration
}

// ObserveExactSource reopens one caller-selected exact content root, verifies
// every physical manifest name and byte commitment, and projects that proof
// through the ordinary discovery report shape. The retained authority is
// process-local and is lost by PublicReportCopy or JSON serialization.
func ObserveExactSource(ctx context.Context, meta *metafile.MetaInfo, contentPath string, options ExactSourceOptions) (DiscoveryResult, error) {
	result := newIncompleteExactSourceObservation(meta, options)
	if meta == nil {
		return result, fmt.Errorf("metafile is nil")
	}
	if contentPath == "" {
		return result, fmt.Errorf("exact source path is empty")
	}
	if options.TimeBudget < 0 {
		return result, fmt.Errorf("exact source time budget is invalid")
	}
	if err := ctx.Err(); err != nil {
		result.Scan.StopReasons = append(result.Scan.StopReasons, "context_cancelled")
		return result, err
	}

	verified, err := metafile.VerifyContentSource(ctx, meta, contentPath)
	if err != nil {
		if ctx.Err() != nil {
			result.Scan.StopReasons = append(result.Scan.StopReasons, "context_cancelled")
		} else {
			result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "source.exact_root_verification_failed", Message: "the explicitly selected exact source did not pass current layout and content verification"})
		}
		return result, err
	}
	verification := verified.Result()
	if !verification.Verified || verification.SourceSnapshotID == "" {
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "source.exact_root_verification_failed", Message: "the explicitly selected exact source did not pass current layout and content verification"})
		return result, ErrSourceIntegrity
	}

	inputPath, err := filepath.Abs(contentPath)
	if err != nil {
		return result, fmt.Errorf("resolve exact source path: %w", err)
	}
	inputPath = filepath.Clean(inputPath)
	resolvedInputPath, err := filepath.EvalSymlinks(inputPath)
	if err != nil {
		return result, fmt.Errorf("resolve physical exact source path: %w", err)
	}
	rootPath := filepath.Clean(resolvedInputPath)
	if !meta.MultiFile {
		if bound, ok := verified.Path(0); ok && sameCleanPath(bound, rootPath) {
			rootPath = filepath.Dir(rootPath)
		}
	}
	selectionID := exactSourceSelectionID(meta.MetafileVariantID, verification.SourceSnapshotID)
	rootID := exactSourceRootID(selectionID, rootPath)
	bindingsByIndex := make(map[int]metafile.SourceBinding, len(verified.Bindings()))
	for _, binding := range verified.Bindings() {
		bindingsByIndex[binding.FileIndex] = binding
	}

	files := make([]DiscoveryFile, 0, len(meta.Files))
	bindings := make([]DiscoveryBinding, 0, len(bindingsByIndex))
	for fileIndex, file := range meta.Files {
		publicFile := DiscoveryFile{
			FileIndex: fileIndex, TorrentPath: strings.Join(file.Path, "/"), Length: file.Length,
			Candidates: []DiscoveryCandidate{},
		}
		if strings.Contains(file.Attribute, "p") {
			publicFile.Requirement = "virtual_padding"
			files = append(files, publicFile)
			continue
		}
		binding, ok := bindingsByIndex[fileIndex]
		if !ok {
			return result, fmt.Errorf("verified exact source has no physical binding for manifest file %d", fileIndex)
		}
		relative, components, err := exactSourceRelativePath(rootPath, binding.Path)
		if err != nil {
			return result, err
		}
		candidateID := exactSourceCandidateID(selectionID, fileIndex, relative)
		candidate := DiscoveryCandidate{
			ID: candidateID, RootID: rootID, RelativePath: relative,
			RelativeComponentsRawBase64: components, EvidenceLevel: "verified",
			EvidenceBasis: "explicit_exact_layout_identity", MatchRank: "explicit_exact_path",
		}
		if options.ShowAbsolutePaths {
			candidate.AbsolutePath = binding.Path
		}
		publicFile.CandidateCount = 1
		publicFile.Candidates = append(publicFile.Candidates, candidate)
		if file.Length == 0 {
			publicFile.Requirement = "physical_empty_source"
		} else {
			publicFile.Requirement = "physical_source"
		}
		files = append(files, publicFile)
		discoveryBinding := DiscoveryBinding{
			FileIndex: fileIndex, TorrentPath: publicFile.TorrentPath, CandidateID: candidateID,
			RootID: rootID, RelativePath: relative, RelativeComponentsRawBase64: append([]string(nil), components...),
		}
		if options.ShowAbsolutePaths {
			discoveryBinding.AbsolutePath = binding.Path
		}
		bindings = append(bindings, discoveryBinding)
	}

	evidenceBasis := []string{"explicit_exact_layout_identity"}
	for _, check := range verification.Checks {
		evidenceBasis = append(evidenceBasis, check.Algorithm)
	}
	match := DiscoveryMatch{
		ID: selectionID, EvidenceLevel: "verified", EvidenceBasis: evidenceBasis, Layout: "exact_root",
		Coverage: DiscoveryCoverage{
			FilesFound: len(bindings), FilesExpected: nonPaddingFileCount(meta),
			BytesFound: physicalManifestBytes(meta), BytesExpected: physicalManifestBytes(meta),
		},
		Bindings: bindings, Verification: verification,
		Mapping: DiscoveryMapping{Status: "not_requested"}, Blockers: []DiscoveryBlocker{},
	}
	root := DiscoveryRoot{ID: rootID, Status: "verified_exact_root"}
	if options.ShowAbsolutePaths {
		root.InputPath = inputPath
		root.ResolvedPath = rootPath
	}
	result.SourceOutcome = "verified_exact_root"
	result.Selection = DiscoverySelection{
		Status: "ready_exact", SelectedID: selectionID,
		Basis: "explicit_exact_root_full_layout_verified", ScopeID: selectionID,
	}
	result.BestEvidence = "verified"
	result.Scan.Complete = true
	result.Scan.VerificationComplete = true
	result.Scan.SearchRoots = []DiscoveryRoot{root}
	result.Scan.MatchUsed.FullVerifications = 1
	result.Scan.MatchUsed.VerifiedLayouts = 1
	result.Files = files
	result.Matches = []DiscoveryMatch{match}
	result.verifiedSource = verified
	result.verifiedSelectionID = selectionID
	result.verifiedSourceMode = "exact_root"
	result.verifiedScopeID = selectionID
	result.Warnings = append(result.Warnings,
		"the exact source was explicitly selected and fully verified; no filesystem-wide uniqueness claim was made",
		"exact-source observation performs metadata and content reads; zero writes were intentionally performed",
	)
	return result, nil
}

// NewIncompleteExactSourceObservation creates an authority-free public report
// shape for an exact source mode that could not reach its content proof. It can
// never synthesize VerifiedSource authority and is intended for structured,
// report-first failures in higher-level read workflows.
func NewIncompleteExactSourceObservation(meta *metafile.MetaInfo, options ExactSourceOptions, blocker DiscoveryBlocker) DiscoveryResult {
	result := newIncompleteExactSourceObservation(meta, options)
	if blocker.Code != "" && blocker.Message != "" {
		result.Blockers = append(result.Blockers, blocker)
	}
	return result
}

func newIncompleteExactSourceObservation(meta *metafile.MetaInfo, options ExactSourceOptions) DiscoveryResult {
	result := newDiscoveryResult(meta, DiscoverOptions{ShowAbsolutePaths: options.ShowAbsolutePaths})
	result.Scan = DiscoveryScan{
		Complete: false, VerificationComplete: false, TimeBudgetMillis: options.TimeBudget.Milliseconds(),
		PathConfinement: "explicit_exact_root", SearchRoots: []DiscoveryRoot{},
		InventoryLimits: storage.DefaultInventoryLimits(), MatchLimits: metafile.DefaultSourceMatchLimits(),
		StopReasons: []string{}, InventoryIssues: []storage.ScanIssue{}, MatchIssues: []metafile.SourceMatchIssue{},
	}
	return result
}

func exactSourceSelectionID(variantID, snapshotID string) string {
	digest := sha256.Sum256([]byte("ptctl-exact-source-selection-v1\x00" + variantID + "\x00" + snapshotID))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func exactSourceRootID(selectionID, rootPath string) string {
	digest := sha256.Sum256([]byte("ptctl-exact-source-root-v1\x00" + selectionID + "\x00" + filepath.Clean(rootPath)))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func exactSourceCandidateID(selectionID string, fileIndex int, relative string) string {
	digest := sha256.Sum256([]byte("ptctl-exact-source-candidate-v1\x00" + selectionID + "\x00" + strconv.Itoa(fileIndex) + "\x00" + relative))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func exactSourceRelativePath(rootPath, filePath string) (string, []string, error) {
	relative, err := filepath.Rel(rootPath, filePath)
	if err != nil {
		return "", nil, fmt.Errorf("relativize exact source binding: %w", err)
	}
	relative = filepath.Clean(relative)
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", nil, fmt.Errorf("verified exact source binding escaped its selected root")
	}
	parts := strings.Split(relative, string(filepath.Separator))
	encoded := make([]string, len(parts))
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", nil, fmt.Errorf("verified exact source binding has an invalid relative component")
		}
		encoded[index] = base64.StdEncoding.EncodeToString([]byte(part))
	}
	return filepath.ToSlash(relative), encoded, nil
}

func sameCleanPath(left, right string) bool {
	return filepath.Clean(left) == filepath.Clean(right)
}
