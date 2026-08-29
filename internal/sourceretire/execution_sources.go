package sourceretire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/seed"
)

type boundSourceFile struct {
	intent  IntentFile
	session *fsbind.Session
}

type boundSourceSet struct {
	files    []boundSourceFile
	sessions map[string]*fsbind.Session
}

func (set *boundSourceSet) close() {
	if set == nil {
		return
	}
	for _, session := range set.sessions {
		_ = session.Close()
	}
	set.sessions = nil
}

func normalizeExecutionRoots(ctx context.Context, input []string, allowNetwork bool) ([]string, string, error) {
	if len(input) == 0 || len(input) > hardExecutionMaxSearchRoots {
		return nil, "", fmt.Errorf("%w: source search-root count is invalid", ErrExecutionPolicy)
	}
	type observedRoot struct {
		path string
		info os.FileInfo
	}
	observed := make([]observedRoot, 0, len(input))
	for _, raw := range input {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if raw == "" {
			return nil, "", fmt.Errorf("%w: source search-root scope is invalid", ErrExecutionPolicy)
		}
		absolute, err := filepath.Abs(raw)
		if err != nil {
			return nil, "", fmt.Errorf("%w: source search-root scope is invalid", ErrExecutionPolicy)
		}
		if executionNetworkPath(absolute) && !allowNetwork {
			return nil, "", fmt.Errorf("%w: network source search root requires explicit permission", ErrExecutionPolicy)
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, "", err
		}
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if executionNetworkPath(resolved) && !allowNetwork {
			return nil, "", fmt.Errorf("%w: resolved network source search root requires explicit permission", ErrExecutionPolicy)
		}
		info, err := os.Lstat(resolved)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, "", fmt.Errorf("%w: source search root is not a safe directory", ErrExecutionPolicy)
		}
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		observed = append(observed, observedRoot{path: filepath.Clean(resolved), info: info})
	}
	sort.Slice(observed, func(i, j int) bool { return observed[i].path < observed[j].path })
	for i := range observed {
		for j := 0; j < i; j++ {
			if observed[i].path == observed[j].path || os.SameFile(observed[i].info, observed[j].info) ||
				executionPathWithin(observed[i].path, observed[j].path) || executionPathWithin(observed[j].path, observed[i].path) {
				return nil, "", fmt.Errorf("%w: source search roots overlap", ErrExecutionPolicy)
			}
		}
	}
	roots := make([]string, len(observed))
	for index := range observed {
		roots[index] = observed[index].path
	}
	id, err := searchScopeID(roots)
	return roots, id, err
}

func executionNetworkPath(path string) bool {
	return runtime.GOOS == "windows" && strings.HasPrefix(filepath.VolumeName(path), `\\`)
}

func bindRunSources(ctx context.Context, meta *metafile.MetaInfo, discovery *seed.DiscoveryResult, final *materialize.VerifiedFinal, plan Plan, roots []string) (*boundSourceSet, []IntentFile, error) {
	if meta == nil || discovery == nil || final == nil {
		return nil, nil, fmt.Errorf("%w: live source authority is unavailable", ErrExecutionPolicy)
	}
	source, ok := discovery.VerifiedSource(meta)
	if !ok || source == nil {
		return nil, nil, fmt.Errorf("%w: live source authority is unavailable", ErrExecutionPolicy)
	}
	bindings := source.Bindings()
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].FileIndex < bindings[j].FileIndex })
	if len(bindings) != len(plan.SourceFiles) {
		return nil, nil, fmt.Errorf("%w: live source binding count changed", ErrExecutionIntegrity)
	}
	set := &boundSourceSet{sessions: make(map[string]*fsbind.Session), files: make([]boundSourceFile, len(bindings))}
	fail := func(err error) (*boundSourceSet, []IntentFile, error) {
		set.close()
		return nil, nil, err
	}
	intentFiles := make([]IntentFile, len(bindings))
	for sequence, binding := range bindings {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		planned := plan.SourceFiles[sequence]
		path := filepath.Clean(binding.Path)
		if binding.FileIndex != planned.ManifestIndex || sourcePathRef(path) != planned.SourcePathRef || !withinExecutionRoots(roots, path) ||
			sourcePathUsesReservedControlName(path) {
			return fail(fmt.Errorf("%w: live source path disagrees with the reviewed plan", ErrExecutionIntegrity))
		}
		parent, name := filepath.Dir(path), filepath.Base(path)
		session := set.sessions[parent]
		if session == nil {
			var err error
			session, _, err = fsbind.BindExisting(parent)
			if err != nil {
				return fail(err)
			}
			set.sessions[parent] = session
		}
		observed, err := session.InspectRootRegular(ctx, name)
		if err != nil {
			return fail(classifyExecutionBindingError(err))
		}
		reader, err := session.OpenRootRegular(ctx, name)
		if err != nil {
			return fail(classifyExecutionBindingError(err))
		}
		info, infoErr := reader.Info()
		closeErr := reader.Close()
		if infoErr != nil || closeErr != nil || observed.SizeBytes != planned.SizeBytes || !observed.Identity.Equal(info.Identity) || !info.Modified.Equal(planned.ModifiedAt) {
			return fail(fmt.Errorf("%w: live source identity changed after review", ErrExecutionIntegrity))
		}
		intent := IntentFile{Sequence: sequence, ManifestIndex: binding.FileIndex, SizeBytes: planned.SizeBytes, ModifiedAt: planned.ModifiedAt,
			SourcePathRef: planned.SourcePathRef, ParentPath: parent, ParentIdentity: session.Info().Identity.String(), Name: name,
			SourceObjectIdentity: observed.Identity.String()}
		intentFiles[sequence], set.files[sequence] = intent, boundSourceFile{intent: intent, session: session}
	}
	state := executionJournalState{Intent: ExecutionIntent{Files: intentFiles}}
	if err := verifyRemainingBoundSources(ctx, state, set, meta, final); err != nil {
		return fail(err)
	}
	return set, intentFiles, nil
}

func bindIntentSources(ctx context.Context, state executionJournalState, roots []string) (*boundSourceSet, error) {
	set := &boundSourceSet{sessions: make(map[string]*fsbind.Session), files: make([]boundSourceFile, len(state.Intent.Files))}
	fail := func(err error) (*boundSourceSet, error) {
		set.close()
		return nil, err
	}
	for sequence, file := range state.Intent.Files {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		full := filepath.Join(file.ParentPath, file.Name)
		if !withinExecutionRoots(roots, full) {
			return fail(fmt.Errorf("%w: durable source path lies outside the explicit roots", ErrExecutionPolicy))
		}
		session := set.sessions[file.ParentPath]
		if session == nil {
			var err error
			session, _, err = fsbind.BindExisting(file.ParentPath)
			if err != nil {
				return fail(err)
			}
			if session.Info().Identity.String() != file.ParentIdentity {
				return fail(fmt.Errorf("%w: source parent identity changed", ErrExecutionIntegrity))
			}
			set.sessions[file.ParentPath] = session
		}
		set.files[sequence] = boundSourceFile{intent: file, session: session}
		observed, err := session.InspectRootRegular(ctx, file.Name)
		if state.DeletedPresent[sequence] {
			if !errors.Is(err, fsbind.ErrNotFound) {
				return fail(fmt.Errorf("%w: retired source name reappeared", ErrExecutionIntegrity))
			}
			continue
		}
		if state.AttemptPresent[sequence] && errors.Is(err, fsbind.ErrNotFound) {
			continue
		}
		if err != nil {
			return fail(classifyExecutionBindingError(err))
		}
		if observed.SizeBytes != file.SizeBytes || observed.Identity.String() != file.SourceObjectIdentity {
			return fail(fmt.Errorf("%w: remaining source identity changed", ErrExecutionIntegrity))
		}
	}
	return set, nil
}

func verifyRemainingBoundSources(ctx context.Context, state executionJournalState, sources *boundSourceSet, meta *metafile.MetaInfo, final *materialize.VerifiedFinal) error {
	if meta == nil || final == nil || !final.Verified() || len(sources.files) != len(state.Intent.Files) {
		return fmt.Errorf("%w: source verification authority is unavailable", ErrExecutionPolicy)
	}
	fresh, _, err := final.Reverify(ctx)
	if err != nil {
		return classifyFinalProofError(err)
	}
	for sequence, file := range state.Intent.Files {
		if len(state.DeletedPresent) == len(state.Intent.Files) && state.DeletedPresent[sequence] {
			continue
		}
		if len(state.AttemptPresent) == len(state.Intent.Files) && state.AttemptPresent[sequence] {
			if _, err := sources.files[sequence].session.InspectRootRegular(ctx, file.Name); errors.Is(err, fsbind.ErrNotFound) {
				continue
			}
		}
		finalPath, size, ok := fresh.ProcessFilePath(file.ManifestIndex)
		if !ok || size != file.SizeBytes {
			return fmt.Errorf("%w: final file mapping disagrees", ErrExecutionIntegrity)
		}
		if err := compareBoundSourceAndFinal(ctx, sources.files[sequence], finalPath); err != nil {
			return err
		}
	}
	_, _, err = fresh.Reverify(ctx)
	if err != nil {
		return classifyFinalProofError(err)
	}
	return nil
}

func compareBoundSourceAndFinal(ctx context.Context, source boundSourceFile, finalPath string) error {
	reader, err := source.session.OpenRootRegular(ctx, source.intent.Name)
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	defer reader.Close()
	sourceBefore, err := reader.Info()
	sourceNative, sourceStatErr := reader.Stat()
	if err != nil || sourceStatErr != nil || sourceBefore.Identity.String() != source.intent.SourceObjectIdentity || sourceBefore.SizeBytes != source.intent.SizeBytes || !sourceBefore.Modified.Equal(source.intent.ModifiedAt) {
		return fmt.Errorf("%w: source identity changed before byte comparison", ErrExecutionIntegrity)
	}
	finalNamedBefore, err := os.Lstat(finalPath)
	if err != nil || !finalNamedBefore.Mode().IsRegular() {
		return fmt.Errorf("%w: final name is unsafe", ErrExecutionIntegrity)
	}
	finalFile, err := os.Open(finalPath)
	if err != nil {
		return err
	}
	finalHandle, statErr := finalFile.Stat()
	if statErr != nil || !finalHandle.Mode().IsRegular() || !os.SameFile(finalNamedBefore, finalHandle) || os.SameFile(finalHandle, sourceNative) {
		_ = finalFile.Close()
		return fmt.Errorf("%w: source and final identity comparison failed", ErrExecutionIntegrity)
	}
	compareErr := compareReadersContext(ctx, reader, finalFile, source.intent.SizeBytes)
	finalAfter, finalAfterErr := finalFile.Stat()
	closeErr := finalFile.Close()
	sourceAfter, sourceAfterErr := reader.Info()
	finalNamedAfter, namedAfterErr := os.Lstat(finalPath)
	if compareErr != nil {
		return compareErr
	}
	if finalAfterErr != nil || closeErr != nil || sourceAfterErr != nil || namedAfterErr != nil {
		return fmt.Errorf("source/final reobservation failed")
	}
	if !sourceBefore.Identity.Equal(sourceAfter.Identity) || sourceBefore.SizeBytes != sourceAfter.SizeBytes || !sourceBefore.Modified.Equal(sourceAfter.Modified) ||
		!os.SameFile(finalHandle, finalAfter) || !os.SameFile(finalAfter, finalNamedAfter) {
		return fmt.Errorf("%w: source or final changed during byte comparison", ErrExecutionIntegrity)
	}
	return source.session.Check()
}

func compareReadersContext(ctx context.Context, left, right io.Reader, expected int64) error {
	leftBuffer, rightBuffer := make([]byte, 128<<10), make([]byte, 128<<10)
	var compared int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		leftN, leftErr := io.ReadFull(left, leftBuffer)
		rightN, rightErr := io.ReadFull(right, rightBuffer)
		if leftN != rightN || !bytes.Equal(leftBuffer[:leftN], rightBuffer[:rightN]) {
			return fmt.Errorf("%w: source bytes differ from the exact final", ErrExecutionIntegrity)
		}
		compared += int64(leftN)
		leftDone := errors.Is(leftErr, io.EOF) || errors.Is(leftErr, io.ErrUnexpectedEOF)
		rightDone := errors.Is(rightErr, io.EOF) || errors.Is(rightErr, io.ErrUnexpectedEOF)
		if leftDone || rightDone {
			if !leftDone || !rightDone || compared != expected {
				return fmt.Errorf("%w: source/final length differs", ErrExecutionIntegrity)
			}
			return nil
		}
		if leftErr != nil {
			return leftErr
		}
		if rightErr != nil {
			return rightErr
		}
	}
}

func executionIntentFromPlan(operation OperationID, scopeID string, plan Plan, limits ExecutionLimits, files []IntentFile) ExecutionIntent {
	return ExecutionIntent{Schema: ExecutionIntentSchemaV1, OperationID: operation, PlanID: plan.ID, SearchScopeID: scopeID,
		TargetRootIdentity: plan.TargetRootIdentity, FinalObjectIdentity: plan.FinalObjectIdentity, MetafileVariantID: plan.MetafileVariantID,
		MaterializeOperationID: plan.MaterializeOperationID, MaterializePlanID: plan.MaterializePlanID,
		ActivationOperationID: plan.ActivationOperationID, ActivationPlanID: plan.ActivationPlanID, ClientCompletionID: plan.ClientCompletionID,
		CurrentClientUseID: plan.CurrentClientUseID, SourceSelectionID: plan.SourceSelectionID, Limits: limits, Files: files, ContentBytes: plan.ContentBytes}
}

func validateResumeAuthorities(intent ExecutionIntent, options ResumeOptions) error {
	if options.Meta == nil || options.Final == nil || options.Activation == nil || options.ClientUse == nil || !options.Final.Verified() || !options.Activation.Verified() {
		return fmt.Errorf("%w: resume authorities are unavailable", ErrExecutionPolicy)
	}
	boundMeta, ok := options.Final.ProcessMetafile()
	if !ok || boundMeta == nil || boundMeta.MetafileVariantID != options.Meta.MetafileVariantID || boundMeta.InfoHashV1 != options.Meta.InfoHashV1 ||
		boundMeta.InfoHashV2 != options.Meta.InfoHashV2 || boundMeta.Version != options.Meta.Version || boundMeta.MetafileBytes != options.Meta.MetafileBytes {
		return fmt.Errorf("%w: resume metafile authority disagrees", ErrExecutionPolicy)
	}
	final := options.Final.Observation()
	activation := options.Activation.Observation()
	if options.Meta.MetafileVariantID != intent.MetafileVariantID || final.MetafileVariantID != intent.MetafileVariantID ||
		final.TargetRootIdentity != intent.TargetRootIdentity || final.FinalObjectIdentity != intent.FinalObjectIdentity ||
		final.OperationID != intent.MaterializeOperationID || final.MaterializePlanID != intent.MaterializePlanID ||
		activation.OperationID != intent.ActivationOperationID || activation.PlanID != intent.ActivationPlanID || activation.TerminalMarkerID != intent.ClientCompletionID {
		return fmt.Errorf("%w: resume selectors disagree with the durable intent", ErrExecutionPolicy)
	}
	return nil
}

func requireRetiredNamesAbsent(ctx context.Context, state executionJournalState, sources *boundSourceSet) error {
	for sequence, file := range state.Intent.Files {
		if !state.DeletedPresent[sequence] {
			return fmt.Errorf("%w: source retirement is not complete", ErrExecutionIntegrity)
		}
		if _, err := sources.files[sequence].session.InspectRootRegular(ctx, file.Name); !errors.Is(err, fsbind.ErrNotFound) {
			if err == nil {
				err = fmt.Errorf("%w: a retired source name exists", ErrExecutionIntegrity)
			}
			return err
		}
	}
	return nil
}

func withinExecutionRoots(roots []string, path string) bool {
	for _, root := range roots {
		if executionPathWithin(root, path) {
			return true
		}
	}
	return false
}

func executionPathWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != "" && !filepath.IsAbs(relative) && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func sourcePathUsesReservedControlName(path string) bool {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	clean = strings.TrimPrefix(clean, volume)
	for _, component := range strings.FieldsFunc(clean, func(value rune) bool {
		return value == '/' || value == '\\'
	}) {
		if materialize.IsReservedControlName(component) {
			return true
		}
	}
	return false
}

func classifyFinalProofError(err error) error {
	if errors.Is(err, materialize.ErrIntegrity) {
		return fmt.Errorf("%w: materialized final proof changed", ErrExecutionIntegrity)
	}
	return err
}
