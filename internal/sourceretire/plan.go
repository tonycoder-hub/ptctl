package sourceretire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/seed"
)

type BuildOptions struct {
	Meta              *metafile.MetaInfo
	Discovery         *seed.DiscoveryResult
	Final             *materialize.VerifiedFinal
	Activation        *clientactivate.VerifiedCompletion
	ShowAbsolutePaths bool
}

// Build creates a review-only plan. It repeats current final proof before and
// after source identity checks, but it never opens a source for writing and has
// no deletion API. A complete live discovery is required because a stored index
// cannot prove current uniqueness.
func Build(ctx context.Context, options BuildOptions) (Report, error) {
	report := newReport()
	if options.Meta == nil || options.Discovery == nil || options.Final == nil || options.Activation == nil {
		report.Outcome = OutcomeBlocked
		report.addBlocker("authority.unavailable", "metafile, live source, final, and client-completion authorities are all required")
		report.finalize()
		return report, nil
	}
	if err := ctx.Err(); err != nil {
		report.addIssue("operation.interrupted", "source retirement planning was interrupted")
		report.finalize()
		return report, err
	}
	if !options.Final.Verified() || !options.Activation.Verified() {
		report.Outcome = OutcomeBlocked
		report.addBlocker("authority.process_capability_unavailable", "serialized final or client-completion output cannot authorize a retirement plan")
		report.finalize()
		return report, nil
	}
	report.Final = options.Final.Observation()
	report.Activation = options.Activation.Observation()
	meta, ok := options.Final.ProcessMetafile()
	if !ok || meta == nil || meta.MetafileVariantID != options.Meta.MetafileVariantID ||
		meta.InfoHashV1 != options.Meta.InfoHashV1 || meta.InfoHashV2 != options.Meta.InfoHashV2 ||
		meta.Version != options.Meta.Version || meta.MetafileBytes != options.Meta.MetafileBytes ||
		meta.TotalLength != options.Meta.TotalLength || len(meta.Files) != len(options.Meta.Files) {
		report.Outcome = OutcomeBlocked
		report.addBlocker("metafile.authority_mismatch", "the selected metafile does not match the exact materialized-final authority")
		report.finalize()
		return report, nil
	}

	report.Source.Status = options.Discovery.SourceOutcome
	report.Source.SelectionID = options.Discovery.Selection.SelectedID
	report.Scan = SourceScanReport{
		Complete: options.Discovery.Scan.Complete, VerificationComplete: options.Discovery.Scan.VerificationComplete,
		TimeBudgetMillis: options.Discovery.Scan.TimeBudgetMillis, PathConfinement: options.Discovery.Scan.PathConfinement,
		InventoryLimits: options.Discovery.Scan.InventoryLimits, MatchLimits: options.Discovery.Scan.MatchLimits,
		InventoryUsed: options.Discovery.Scan.InventoryUsed, MatchUsed: options.Discovery.Scan.MatchUsed,
		StopReasons:         append([]string(nil), options.Discovery.Scan.StopReasons...),
		InventoryIssueCount: len(options.Discovery.Scan.InventoryIssues), MatchIssueCount: len(options.Discovery.Scan.MatchIssues),
	}
	if !options.Discovery.Scan.Complete || !options.Discovery.Scan.VerificationComplete || len(options.Discovery.Scan.StopReasons) != 0 {
		report.Outcome = OutcomeIncomplete
		report.addBlocker("source.discovery_incomplete", "live source discovery did not prove a complete current result")
		report.finalize()
		return report, nil
	}
	switch options.Discovery.SourceOutcome {
	case "incomplete":
		report.Outcome = OutcomeIncomplete
		report.addBlocker("source.discovery_incomplete", "live source discovery did not prove a complete current result")
		report.finalize()
		return report, nil
	case "verified_ambiguous":
		report.Outcome = OutcomeBlocked
		report.addBlocker("source.ambiguous", "more than one exact source assignment is currently verified")
		report.finalize()
		return report, nil
	case "not_found":
		report.Outcome = OutcomeBlocked
		report.addBlocker("source.not_found", "no exact source assignment is currently verified")
		report.finalize()
		return report, nil
	case "verified_unique":
	default:
		report.Outcome = OutcomeBlocked
		report.addBlocker("source.status_invalid", "live source discovery has no usable exact outcome")
		report.finalize()
		return report, nil
	}
	if options.Discovery.Selection.Status != "ready" || options.Discovery.Selection.SelectedID == "" {
		report.Outcome = OutcomeBlocked
		report.addBlocker("source.selection_invalid", "live source discovery has no canonical ready selection")
		report.finalize()
		return report, nil
	}
	source, ok := options.Discovery.VerifiedSource(meta)
	if !ok || source == nil || !source.Result().Verified {
		report.Outcome = OutcomeBlocked
		report.addBlocker("source.process_authority_unavailable", "serialized or detached discovery output cannot authorize a retirement plan")
		report.finalize()
		return report, nil
	}
	freshFinal, firstFinal, err := options.Final.Reverify(ctx)
	if err != nil {
		return failProof(&report, err, "materialized final could not be reverified before source planning")
	}
	report.Final = firstFinal
	activationOperation, parseErr := clientactivate.ParseOperationID(report.Activation.OperationID)
	if parseErr != nil || !options.Activation.Matches(activationOperation, report.Activation.PlanID, firstFinal.MetafileVariantID,
		firstFinal.OperationID, firstFinal.MaterializePlanID, firstFinal.FinalObjectIdentity) {
		report.Outcome = OutcomeBlocked
		report.addBlocker("client.completion_mismatch", "terminal client completion does not belong to the current exact materialized final")
		report.finalize()
		return report, nil
	}
	finalRoot, ok := freshFinal.ProcessFinalPath()
	if !ok || finalRoot == "" {
		report.Outcome = OutcomeBlocked
		report.addBlocker("final.path_authority_unavailable", "the current final has no process-local path authority")
		report.finalize()
		return report, nil
	}

	bindings := source.Bindings()
	expectedPhysical := 0
	var expectedBytes int64
	for _, file := range meta.Files {
		if file.Length > 0 && !strings.Contains(file.Attribute, "p") {
			expectedPhysical++
			expectedBytes += file.Length
		}
	}
	if expectedPhysical == 0 || len(bindings) != expectedPhysical || expectedBytes <= 0 {
		report.Outcome = OutcomeBlocked
		report.addBlocker("source.no_retirable_regular_content", "the exact source has no complete set of content-bearing regular-file names")
		report.finalize()
		return report, nil
	}

	files := make([]SourceFile, 0, len(bindings))
	seenRefs := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if err := ctx.Err(); err != nil {
			report.addIssue("operation.interrupted", "source retirement planning was interrupted")
			report.finalize()
			return report, err
		}
		if binding.FileIndex < 0 || binding.FileIndex >= len(meta.Files) {
			return failIntegrity(&report, "verified source contains an invalid manifest index")
		}
		manifestFile := meta.Files[binding.FileIndex]
		if manifestFile.Length <= 0 || strings.Contains(manifestFile.Attribute, "p") || binding.Path == "" || !filepath.IsAbs(binding.Path) {
			return failIntegrity(&report, "verified source contains an invalid physical binding")
		}
		finalPath, finalLength, found := freshFinal.ProcessFilePath(binding.FileIndex)
		if !found || finalLength != manifestFile.Length {
			return failIntegrity(&report, "materialized final file mapping disagrees with the metafile")
		}
		if pathWithin(finalRoot, binding.Path) {
			report.Outcome = OutcomeBlocked
			report.addBlocker("source.overlaps_final", "a selected source name is inside the exact materialized final")
			report.finalize()
			return report, nil
		}
		before, preconditionErr := source.SourcePrecondition(binding.FileIndex)
		if preconditionErr != nil {
			return failIntegrity(&report, "a selected source changed after exact discovery")
		}
		if before.SizeBytes != manifestFile.Length {
			return failIntegrity(&report, "a selected source size disagrees with the metafile")
		}
		sourceInfo, sourceErr := os.Lstat(binding.Path)
		finalInfo, finalErr := os.Lstat(finalPath)
		if sourceErr != nil || finalErr != nil {
			report.Outcome = OutcomeIncomplete
			report.addIssue("filesystem.reobserve_failed", "a named source or final file could not be reobserved")
			report.finalize()
			return report, fmt.Errorf("%w: named file reobservation failed", ErrIncomplete)
		}
		if !sourceInfo.Mode().IsRegular() || !finalInfo.Mode().IsRegular() {
			return failIntegrity(&report, "a selected source or final path is no longer a regular file")
		}
		if os.SameFile(sourceInfo, finalInfo) {
			report.Outcome = OutcomeBlocked
			report.addBlocker("source.aliases_final", "a selected source name aliases the materialized final object")
			report.finalize()
			return report, nil
		}
		after, preconditionErr := source.SourcePrecondition(binding.FileIndex)
		if preconditionErr != nil || before.SizeBytes != after.SizeBytes || !before.ModifiedAt.Equal(after.ModifiedAt) {
			return failIntegrity(&report, "a selected source changed during retirement planning")
		}
		pathRef := sourcePathRef(binding.Path)
		if _, duplicate := seenRefs[pathRef]; duplicate {
			return failIntegrity(&report, "the verified source reuses one named path for multiple manifest files")
		}
		seenRefs[pathRef] = struct{}{}
		file := SourceFile{ManifestIndex: binding.FileIndex, SizeBytes: before.SizeBytes, ModifiedAt: before.ModifiedAt.UTC(),
			SourcePathRef: pathRef, DistinctObject: true}
		if options.ShowAbsolutePaths {
			file.SourcePath = filepath.Clean(binding.Path)
		}
		files = append(files, file)
	}
	// Discovery proved uniqueness earlier in this invocation. Re-read and
	// cryptographically verify that selected named mapping after its identity
	// and alias checks so metadata-only stability is not mistaken for current
	// byte equality.
	reverifiedSource, sourceErr := source.Reverify(ctx, meta)
	if sourceErr != nil {
		report.Outcome = OutcomeIncomplete
		report.addIssue("source.reverification_incomplete", "the selected named source mapping could not be reverified")
		report.finalize()
		return report, sourceErr
	}
	if reverifiedSource == nil || !reverifiedSource.Result().Verified || reverifiedSource.Result().BytesVerified != expectedBytes {
		return failIntegrity(&report, "the selected source bytes changed after unique discovery")
	}
	for index, binding := range bindings {
		reverifiedPrecondition, preconditionErr := reverifiedSource.SourcePrecondition(binding.FileIndex)
		originalPrecondition, originalErr := source.SourcePrecondition(binding.FileIndex)
		if preconditionErr != nil || originalErr != nil || reverifiedPrecondition.SizeBytes != files[index].SizeBytes ||
			originalPrecondition.SizeBytes != files[index].SizeBytes ||
			!reverifiedPrecondition.ModifiedAt.Equal(files[index].ModifiedAt) ||
			!originalPrecondition.ModifiedAt.Equal(files[index].ModifiedAt) {
			return failIntegrity(&report, "a selected source identity changed across exact reverification")
		}
	}

	secondFinal, finalAfter, err := freshFinal.Reverify(ctx)
	if err != nil {
		return failProof(&report, err, "materialized final could not be reverified after source planning")
	}
	if secondFinal == nil || !secondFinal.Verified() || firstFinal != finalAfter {
		return failIntegrity(&report, "materialized final identity changed during source retirement planning")
	}
	report.Final = finalAfter
	report.Source = SourceReport{Status: "verified_unique_distinct_from_final", SelectionID: options.Discovery.Selection.SelectedID,
		PhysicalFiles: len(files), ContentBytes: expectedBytes,
		Assurance: "same_invocation_unique_source_with_post_selection_exact_reverification_bracketed_by_exact_final_reverification"}
	report.Plan = Plan{
		Schema: PlanSchemaV1, Mode: PlanModeV1, DeletionAuthority: "none",
		MetafileVariantID: meta.MetafileVariantID, InfoHashV1: meta.InfoHashV1, InfoHashV2: meta.InfoHashV2,
		MaterializeOperationID: firstFinal.OperationID, MaterializePlanID: firstFinal.MaterializePlanID,
		ActivationOperationID: report.Activation.OperationID, ActivationPlanID: report.Activation.PlanID,
		ClientCompletionID: report.Activation.TerminalMarkerID, SourceSelectionID: options.Discovery.Selection.SelectedID,
		TargetRootIdentity: firstFinal.TargetRootIdentity, FinalObjectIdentity: firstFinal.FinalObjectIdentity,
		ManifestFiles: len(meta.Files), PhysicalSourceFiles: len(files), ContentBytes: expectedBytes,
		AbsolutePathsShown: options.ShowAbsolutePaths, SourceFiles: files,
		EvidenceBasis: []string{
			"same_invocation_complete_unique_source_verification",
			"same_invocation_post_selection_exact_source_reverification",
			"same_invocation_pre_and_post_exact_final_verification",
			"canonical_terminal_client_activation_journal_read",
			"named_source_identity_reobservation",
			"source_final_object_non_alias_observation",
			"bracketed_non_atomic",
		},
	}
	report.Plan.ID, err = planID(report.Plan)
	if err != nil || report.Plan.Validate() != nil {
		return failIntegrity(&report, "source retirement plan could not be canonicalized")
	}
	report.Outcome = OutcomeEligible
	report.finalize()
	return report, nil
}

func pathWithin(base, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(base), filepath.Clean(path))
	if err != nil || relative == "" || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func failProof(report *Report, err error, message string) (Report, error) {
	if errors.Is(err, materialize.ErrIntegrity) {
		return failIntegrity(report, message)
	}
	report.Outcome = OutcomeIncomplete
	report.addIssue("proof.incomplete", message)
	report.finalize()
	return *report, err
}

func failIntegrity(report *Report, message string) (Report, error) {
	report.Outcome = OutcomeIntegrityFailed
	report.addBlocker("integrity.proof_changed", message)
	report.finalize()
	return *report, fmt.Errorf("%w: %s", ErrIntegrity, message)
}
