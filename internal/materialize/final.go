package materialize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const (
	FinalAuthorityJournal   = "committed_journal"
	FinalAuthorityRetention = "sealed_retention_tombstone"
)

type FinalProofOptions struct {
	Meta           *metafile.MetaInfo
	TargetRoot     string
	OperationID    OperationID
	ExpectedPlanID string
	Limits         Limits
}

type FinalObservation struct {
	OperationID         string `json:"operation_id"`
	MaterializePlanID   string `json:"materialize_plan_id"`
	MetafileVariantID   string `json:"metafile_variant_id"`
	MetafileBytes       int64  `json:"metafile_bytes"`
	InfoHashV1          string `json:"info_hash_v1,omitempty"`
	InfoHashV2          string `json:"info_hash_v2,omitempty"`
	TargetRootIdentity  string `json:"target_root_identity"`
	FinalObjectIdentity string `json:"final_object_identity"`
	MultiFile           bool   `json:"multi_file"`
	ManifestFiles       int    `json:"manifest_files"`
	ContentBytes        int64  `json:"content_bytes"`
	BytesVerified       int64  `json:"bytes_verified"`
	AuthorityBasis      string `json:"authority_basis"`
	Assurance           string `json:"assurance"`
}

// VerifiedFinal is process-local authority for one same-invocation exact final
// verification. Its private paths and parsed proof material have no JSON form.
type VerifiedFinal struct {
	authority *verifiedFinalAuthority
}

type verifiedFinalAuthority struct {
	meta       *metafile.MetaInfo
	targetRoot string
	operation  OperationID
	planID     string
	limits     Limits
	layout     Layout
	rootID     fsbind.Identity
	finalID    fsbind.Identity
	basis      string
}

type FinalClientProjection struct {
	PathMappingID  string `json:"path_mapping_id"`
	PathSemantics  string `json:"path_semantics"`
	SavePathRef    string `json:"save_path_ref"`
	ContentPathRef string `json:"content_path_ref"`
	ManifestFiles  int    `json:"manifest_files"`
	savePath       string
	contentPath    string
	filePaths      map[int]string
	filePathRefs   map[int]string
	fileSizes      map[int]int64
}

func (projection FinalClientProjection) SavePath() (string, bool) {
	return projection.savePath, projection.savePath != ""
}

func (projection FinalClientProjection) ContentPath() (string, bool) {
	return projection.contentPath, projection.contentPath != ""
}

// FilePath returns one process-local projected final file path by manifest
// index. Raw paths have no JSON representation and are copied on access.
func (projection FinalClientProjection) FilePath(index int) (string, bool) {
	value, ok := projection.filePaths[index]
	return value, ok && value != ""
}

func (projection FinalClientProjection) FilePathRef(index int) (string, bool) {
	value, ok := projection.filePathRefs[index]
	return value, ok && value != ""
}

func (projection FinalClientProjection) FileSize(index int) (int64, bool) {
	value, ok := projection.fileSizes[index]
	return value, ok && value >= 0
}

func (verified *VerifiedFinal) Verified() bool {
	return verified != nil && verified.authority != nil && verified.authority.meta != nil &&
		!verified.authority.rootID.IsZero() && !verified.authority.finalID.IsZero()
}

func (verified *VerifiedFinal) Observation() FinalObservation {
	if !verified.Verified() {
		return FinalObservation{}
	}
	authority := verified.authority
	return FinalObservation{
		OperationID: authority.operation.String(), MaterializePlanID: authority.planID,
		MetafileVariantID: authority.meta.MetafileVariantID, MetafileBytes: authority.meta.MetafileBytes, InfoHashV1: authority.meta.InfoHashV1,
		InfoHashV2: authority.meta.InfoHashV2, TargetRootIdentity: authority.rootID.String(),
		FinalObjectIdentity: authority.finalID.String(), MultiFile: authority.layout.MultiFile,
		ManifestFiles: len(authority.layout.Files), ContentBytes: authority.layout.ContentBytes,
		BytesVerified: authority.layout.ContentBytes, AuthorityBasis: authority.basis,
		Assurance: "same_invocation_bracketed_non_atomic_exact_content_and_namespace",
	}
}

// ProcessTargetRoot returns the private locator needed by another internal
// same-process workflow. The value is never part of FinalObservation or JSON.
func (verified *VerifiedFinal) ProcessTargetRoot() (string, bool) {
	if !verified.Verified() {
		return "", false
	}
	return verified.authority.targetRoot, true
}

func (verified *VerifiedFinal) Matches(operation OperationID, planID, variantID string) bool {
	if !verified.Verified() {
		return false
	}
	authority := verified.authority
	return authority.operation == operation && authority.planID == planID && authority.meta.MetafileVariantID == variantID
}

// Reverify repeats the full content and exact-namespace proof from a private
// clone of the parsed metafile. It never trusts the previous observation as
// current proof.
func (verified *VerifiedFinal) Reverify(ctx context.Context) (*VerifiedFinal, FinalObservation, error) {
	if !verified.Verified() {
		return nil, FinalObservation{}, fmt.Errorf("%w: current final authority is unavailable", ErrPolicy)
	}
	authority := verified.authority
	return VerifyCurrentFinal(ctx, FinalProofOptions{
		Meta: authority.meta.Clone(), TargetRoot: authority.targetRoot, OperationID: authority.operation,
		ExpectedPlanID: authority.planID, Limits: authority.limits,
	})
}

// ProjectClientPaths maps the bound target root and final object into one
// invocation-scoped downloader namespace. Raw paths remain process-local.
func (verified *VerifiedFinal) ProjectClientPaths(hostRoot, clientRoot string, clientWindows bool) (FinalClientProjection, error) {
	if !verified.Verified() {
		return FinalClientProjection{}, fmt.Errorf("current final authority is unavailable")
	}
	authority := verified.authority
	save, err := storage.MapHostToClient(hostRoot, authority.targetRoot, clientRoot, clientWindows)
	if err != nil {
		return FinalClientProjection{}, err
	}
	content, err := storage.MapHostToClient(hostRoot, filepath.Join(authority.targetRoot, authority.layout.FinalName), clientRoot, clientWindows)
	if err != nil {
		return FinalClientProjection{}, err
	}
	semantics := "posix_exact"
	if clientWindows {
		semantics = "windows_exact"
	}
	mappingDigest := sha256.Sum256([]byte(strings.Join([]string{
		"ptctl-path-mapping-v1", semantics, save.HostRoot, save.ClientRoot,
	}, "\x00")))
	pathRef := func(value string) string {
		digest := sha256.Sum256([]byte("ptctl-client-path-v1\x00" + value))
		return "sha256:" + hex.EncodeToString(digest[:])
	}
	filePaths := make(map[int]string, len(authority.layout.Files))
	filePathRefs := make(map[int]string, len(authority.layout.Files))
	fileSizes := make(map[int]int64, len(authority.layout.Files))
	for _, file := range authority.layout.Files {
		hostPath := filepath.Join(append([]string{authority.targetRoot}, file.Components...)...)
		mapped, mapErr := storage.MapHostToClient(hostRoot, hostPath, clientRoot, clientWindows)
		if mapErr != nil {
			return FinalClientProjection{}, fmt.Errorf("project materialized file %d: %w", file.ManifestIndex, mapErr)
		}
		if mapped.ClientPath == "" {
			return FinalClientProjection{}, fmt.Errorf("project materialized file %d: client path is empty", file.ManifestIndex)
		}
		if _, duplicate := filePaths[file.ManifestIndex]; duplicate {
			return FinalClientProjection{}, fmt.Errorf("materialized client projection has a duplicate manifest index")
		}
		filePaths[file.ManifestIndex] = mapped.ClientPath
		filePathRefs[file.ManifestIndex] = pathRef(mapped.ClientPath)
		fileSizes[file.ManifestIndex] = file.Length
	}
	return FinalClientProjection{
		PathMappingID: "sha256:" + hex.EncodeToString(mappingDigest[:]), PathSemantics: semantics,
		SavePathRef: pathRef(save.ClientPath), ContentPathRef: pathRef(content.ClientPath),
		ManifestFiles: len(filePaths), savePath: save.ClientPath, contentPath: content.ClientPath,
		filePaths: filePaths, filePathRefs: filePathRefs, fileSizes: fileSizes,
	}, nil
}

func VerifyCurrentFinal(ctx context.Context, options FinalProofOptions) (*VerifiedFinal, FinalObservation, error) {
	if options.Meta == nil || options.TargetRoot == "" || options.ExpectedPlanID == "" {
		return nil, FinalObservation{}, fmt.Errorf("%w: current final selector is incomplete", ErrPolicy)
	}
	if _, err := ParseOperationID(options.OperationID.String()); err != nil || !canonicalPlanID(options.ExpectedPlanID) {
		return nil, FinalObservation{}, fmt.Errorf("%w: current final selector is invalid", ErrPolicy)
	}
	if err := options.Limits.Validate(); err != nil {
		return nil, FinalObservation{}, fmt.Errorf("%w: current final limits are invalid", ErrPolicy)
	}
	if err := ctx.Err(); err != nil {
		return nil, FinalObservation{}, err
	}
	absolute, err := filepath.Abs(options.TargetRoot)
	if err != nil {
		return nil, FinalObservation{}, fmt.Errorf("%w: target root is invalid", ErrPolicy)
	}
	absolute = filepath.Clean(absolute)
	session, rootInfo, err := fsbind.BindExisting(absolute)
	if err != nil {
		return nil, FinalObservation{}, err
	}
	defer session.Close()

	meta := options.Meta.Clone()
	var expectedFinal fsbind.Identity
	var layout Layout
	basis := ""

	handle, journalErr := openJournalForRetention(ctx, session, options.OperationID, options.Limits)
	if journalErr == nil {
		defer handle.subtree.Close()
		if handle.state.Phase != PhaseCommitted || !handle.state.Terminal {
			return nil, FinalObservation{}, fmt.Errorf("%w: materialize operation is not committed", ErrPolicy)
		}
		layout, err = BuildLayout(meta, handle.intent.Limits)
		if err != nil {
			return nil, FinalObservation{}, err
		}
		if err := validateResumeIntent(meta, layout, handle.intent, options.ExpectedPlanID); err != nil {
			return nil, FinalObservation{}, err
		}
		expectedFinal, err = fsbind.ParseIdentity(handle.state.FinalIdentity)
		if err != nil || expectedFinal.IsZero() {
			return nil, FinalObservation{}, fmt.Errorf("%w: committed final identity is invalid", ErrIntegrity)
		}
		basis = FinalAuthorityJournal
	} else {
		if errors.Is(journalErr, context.Canceled) || errors.Is(journalErr, context.DeadlineExceeded) {
			return nil, FinalObservation{}, journalErr
		}
		subtree, openErr := openRetentionSubtree(ctx, session, options.OperationID)
		if openErr != nil {
			return nil, FinalObservation{}, openErr
		}
		defer subtree.Close()
		state, stateErr := loadRetentionMarkerState(ctx, subtree, options.OperationID, rootInfo.Identity)
		if stateErr != nil {
			return nil, FinalObservation{}, stateErr
		}
		if !state.IntentPresent || !state.CompletePresent || state.Intent.TerminalPhase != PhaseCommitted ||
			state.Intent.Basis != RetentionBasisCommitted || state.Intent.PlanID != options.ExpectedPlanID ||
			state.Intent.MetafileVariantID != meta.MetafileVariantID || state.Intent.TargetRootIdentity != rootInfo.Identity.String() {
			return nil, FinalObservation{}, fmt.Errorf("%w: retained final authority disagrees with the selector", ErrIntegrity)
		}
		if _, err := retentionHeavyNames(ctx, subtree, true); err != nil {
			return nil, FinalObservation{}, fmt.Errorf("%w: retained final authority still contains mutable operation state", ErrIntegrity)
		}
		if err := auditRetentionControlDirectory(ctx, subtree, state, false); err != nil {
			return nil, FinalObservation{}, fmt.Errorf("%w: retained final marker namespace is not exact", ErrIntegrity)
		}
		layout, err = BuildLayout(meta, options.Limits)
		if err != nil {
			return nil, FinalObservation{}, err
		}
		expectedFinal, err = fsbind.ParseIdentity(state.Intent.FinalObjectIdentity)
		if err != nil || expectedFinal.IsZero() {
			return nil, FinalObservation{}, fmt.Errorf("%w: retained final identity is invalid", ErrIntegrity)
		}
		basis = FinalAuthorityRetention
	}

	bytesVerified, err := verifyCurrentFinalLayout(ctx, meta, layout, absolute, session, expectedFinal, options.Limits)
	if err != nil {
		return nil, FinalObservation{}, err
	}
	if bytesVerified != layout.ContentBytes {
		return nil, FinalObservation{}, fmt.Errorf("%w: exact final verification byte count disagrees", ErrIntegrity)
	}
	result := &VerifiedFinal{authority: &verifiedFinalAuthority{
		meta: meta, targetRoot: absolute, operation: options.OperationID, planID: options.ExpectedPlanID,
		limits: options.Limits, layout: layout, rootID: rootInfo.Identity, finalID: expectedFinal, basis: basis,
	}}
	return result, result.Observation(), nil
}

func verifyCurrentFinalLayout(ctx context.Context, meta *metafile.MetaInfo, layout Layout, targetRoot string, session *fsbind.Session, expected fsbind.Identity, limits Limits) (int64, error) {
	published, err := session.OpenPublishedRoot(layout.FinalName, expected)
	if err != nil {
		return 0, classifyBoundContentError(err)
	}
	defer published.Close()
	wantKind := fsbind.ObjectKindRegular
	if layout.MultiFile {
		wantKind = fsbind.ObjectKindDirectory
	}
	if published.Kind() != wantKind {
		return 0, classifyBoundContentError(fsbind.ErrUnsafeObject)
	}
	before, err := auditCurrentPublishedNamespace(ctx, layout, published, expected, limits)
	if err != nil {
		return 0, err
	}
	bindings := make([]metafile.SourceBinding, 0, len(layout.Files))
	for _, file := range layout.Files {
		relative := []string(nil)
		if layout.MultiFile {
			relative = append(relative, file.Components[1:]...)
		}
		boundPath, pathErr := fsbind.PathFromComponents(relative)
		if pathErr != nil {
			return 0, pathErr
		}
		absolutePath := filepath.Join(append([]string{targetRoot}, file.Components...)...)
		if file.Length == 0 {
			opened, openErr := published.OpenRegular(ctx, boundPath)
			if openErr != nil {
				return 0, classifyBoundContentError(openErr)
			}
			info, infoErr := opened.Info()
			closeErr := opened.Close()
			if infoErr != nil {
				return 0, classifyBoundContentError(infoErr)
			}
			if closeErr != nil {
				return 0, closeErr
			}
			if info.SizeBytes != 0 {
				return 0, ErrIntegrity
			}
			continue
		}
		pathForOpen := boundPath
		bindings = append(bindings, metafile.SourceBinding{
			FileIndex: file.ManifestIndex, Path: absolutePath,
			Open: func() (metafile.SourceFile, error) { return published.OpenRegular(ctx, pathForOpen) },
		})
	}
	verified, err := metafile.VerifySourceMap(ctx, meta, metafile.SourceMap{Bindings: bindings})
	if err != nil {
		return 0, classifyBoundContentError(err)
	}
	if !verified.Result().Verified {
		return verified.Result().BytesVerified, ErrIntegrity
	}
	after, err := auditCurrentPublishedNamespace(ctx, layout, published, expected, limits)
	if err != nil {
		return verified.Result().BytesVerified, err
	}
	if !sameNamespaceSnapshot(before, after) {
		return verified.Result().BytesVerified, ErrIntegrity
	}
	if err := published.Check(); err != nil {
		return verified.Result().BytesVerified, classifyBoundContentError(err)
	}
	if err := published.Close(); err != nil {
		return 0, err
	}
	reopened, err := session.OpenPublishedRoot(layout.FinalName, expected)
	if err != nil {
		return 0, classifyBoundContentError(err)
	}
	if !reopened.Identity().Equal(expected) {
		_ = reopened.Close()
		return 0, ErrIntegrity
	}
	if err := reopened.Close(); err != nil {
		return 0, err
	}
	return verified.Result().BytesVerified, nil
}
