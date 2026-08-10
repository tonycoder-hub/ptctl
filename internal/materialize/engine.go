package materialize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/seed"
)

type RunOptions struct {
	Meta           *metafile.MetaInfo
	Discovery      *seed.DiscoveryResult
	TargetRoot     string
	ExpectedPlanID string
	Limits         Limits
}

type ResumeOptions struct {
	Meta           *metafile.MetaInfo
	Discovery      *seed.DiscoveryResult
	TargetRoot     string
	OperationID    OperationID
	ExpectedPlanID string
	Limits         Limits
}

type copyReceipt struct {
	Bytes       int64
	Digest      string
	Identity    fsbind.Identity
	Publication fsbind.Publication
}

// transitionHook is a deterministic package-test crash seam. It runs only
// after the named event is durably appended and never substitutes for a real
// filesystem check.
var transitionHook func(Phase) error
var namespacePublishHook func(fsbind.Publication) error
var targetBindHook func() error
var publicationTestHook func(string) (fsbind.Publication, error, bool)
var rootSyncTestHook func() error

func runPublication(kind string, perform func() (fsbind.Publication, error)) (fsbind.Publication, error) {
	if publicationTestHook != nil {
		if receipt, err, handled := publicationTestHook(kind); handled {
			return receipt, err
		}
	}
	return perform()
}

func publicationResultAmbiguous(publication fsbind.Publication, err error) bool {
	return publication.Attempted && (errors.Is(err, fsbind.ErrPublicationAmbiguous) || (!publication.Published && err == nil))
}

func syncJournaledRoot(ctx context.Context, journal *journal) error {
	if rootSyncTestHook != nil {
		return rootSyncTestHook()
	}
	return journal.session.SyncRoot(ctx)
}

func checkTransitionHook(phase Phase) error {
	if transitionHook != nil {
		return transitionHook(phase)
	}
	return nil
}

func Run(ctx context.Context, options RunOptions) (Report, error) {
	variantID := ""
	if options.Meta != nil {
		variantID = options.Meta.MetafileVariantID
	}
	report := newReport(variantID, options.ExpectedPlanID, options.Limits)
	if err := options.Limits.Validate(); err != nil {
		report.Outcome = OutcomeBlocked
		report.addBlocker("limits.invalid", "materialize limits are invalid")
		return report, fmt.Errorf("%w: invalid limits", ErrPolicy)
	}
	if !canonicalPlanID(options.ExpectedPlanID) {
		report.Outcome = OutcomeBlocked
		report.addBlocker("plan.expected_id_invalid", "the reviewed plan ID is invalid")
		return report, fmt.Errorf("%w: expected plan ID is invalid", ErrPolicy)
	}
	if options.TargetRoot == "" {
		report.Outcome = OutcomeBlocked
		report.addBlocker("target.invalid_root", "the target root is invalid")
		return report, fmt.Errorf("%w: target root is empty", ErrPolicy)
	}
	layout, err := BuildLayout(options.Meta, options.Limits)
	if err != nil {
		report.Outcome = OutcomeBlocked
		report.addBlocker("manifest.unsupported_layout", "the metafile layout is outside materialize v1")
		return report, err
	}
	var source *metafile.VerifiedSource
	if options.Discovery != nil {
		source, _ = options.Discovery.VerifiedSource(options.Meta)
	}
	if source == nil || !source.Matches(options.Meta) || !source.Result().Verified {
		report.Outcome = OutcomeBlocked
		report.addBlocker("source.process_authority_missing", "same-invocation verified source authority is required")
		return report, fmt.Errorf("%w: verified source authority is unavailable", ErrPolicy)
	}
	report.Source.Outcome = "verified_unique"
	report.Source.ContentVerified = true
	plan, err := seed.BuildMaterializePlanFromVerified(ctx, options.Meta, source, options.TargetRoot, StrategyCopy)
	if err != nil {
		report.Outcome = OutcomeBlocked
		report.addBlocker("plan.rebuild_failed", "the read-only layout plan could not be rebuilt")
		return report, err
	}
	report.Plan.ObservedID = plan.ID
	report.Plan.Matches = plan.ID == options.ExpectedPlanID
	if !report.Plan.Matches {
		report.Outcome = OutcomeBlocked
		report.addBlocker("plan.id_mismatch", "the live plan differs from the reviewed plan")
		return report, fmt.Errorf("%w: live plan ID differs from the reviewed ID", ErrPolicy)
	}
	expectedTargetIdentity, err := fsbind.ParseIdentity(plan.TargetRootIdentity)
	if err != nil || expectedTargetIdentity.IsZero() {
		report.Outcome = OutcomeBlocked
		report.addBlocker("target.bound_identity_unavailable", "the reviewed plan lacks the bound target-root identity required for journaled materialize")
		return report, fmt.Errorf("%w: reviewed plan has no usable target-root identity", ErrPolicy)
	}
	report.Target.ExpectedRootIdentity = plan.TargetRootIdentity
	report.Source.PreconditionsRechecked = true
	if targetBindHook != nil {
		if err := targetBindHook(); err != nil {
			return report, err
		}
	}
	targetRoot, err := filepath.Abs(options.TargetRoot)
	if err != nil {
		report.Outcome = OutcomeBlocked
		report.addBlocker("target.invalid_root", "the target root is invalid")
		return report, fmt.Errorf("%w: target root is invalid", ErrPolicy)
	}
	targetRoot = filepath.Clean(targetRoot)
	session, rootInfo, err := fsbind.BindExisting(targetRoot)
	if err != nil {
		report.Outcome = OutcomeBlocked
		report.addBlocker("target.unsupported_filesystem", "the target root cannot provide the required bound no-clobber semantics")
		return report, fmt.Errorf("%w: bind target root: %v", ErrPolicy, err)
	}
	defer session.Close()
	report.Target.ObservedRootIdentity = rootInfo.Identity.String()
	if !rootInfo.Identity.Equal(expectedTargetIdentity) {
		report.Outcome = OutcomeBlocked
		report.addBlocker("target.root_identity_mismatch", "the bound target root differs from the reviewed filesystem object")
		return report, fmt.Errorf("%w: target-root identity differs from review", ErrPolicy)
	}
	report.Target.RootIdentityBound = true
	report.Target.SameFilesystemStage = true
	report.Target.NoClobberCapable = true
	report.Target.StabilityAssurance = "non_atomic_bound_filesystem"
	if _, err := session.InspectRoot(ctx, layout.FinalName); err == nil {
		report.Outcome = OutcomeBlocked
		report.addBlocker("target.final_exists", "the final top-level target already exists")
		return report, fmt.Errorf("%w: final target already exists", ErrPolicy)
	} else if !errors.Is(err, fsbind.ErrNotFound) {
		report.Outcome = OutcomeBlocked
		report.addBlocker("target.final_uninspectable", "the final target namespace could not be safely inspected")
		return report, fmt.Errorf("%w: inspect final target: %v", ErrPolicy, err)
	}
	intent, err := NewIntent(options.Meta, layout, plan.ID, rootInfo.Identity.String(), options.Limits)
	if err != nil {
		report.Outcome = OutcomeBlocked
		report.addBlocker("intent.invalid", "the durable operation intent could not be constructed")
		return report, err
	}
	intendedOperationID, err := OperationIDFor(intent)
	if err != nil {
		report.Outcome = OutcomeBlocked
		report.addBlocker("intent.operation_id_failed", "the operation identity could not be derived")
		return report, err
	}
	handle, journalCreation, err := createJournal(ctx, session, intent)
	if journalCreation.SubtreeCreated {
		report.Operation.ID = intendedOperationID.String()
		report.Operation.Status = "initializing"
		report.WritesPerformed++
		report.Writes.OperationSubtrees++
	}
	report.WritesPerformed += journalCreation.DirectoriesCreated
	report.Writes.JournalDirectories += journalCreation.DirectoriesCreated
	for index, receipt := range journalCreation.Objects {
		report.recordJournalWrite(receipt, index != 0)
	}
	if journalCreation.DurableReplayPhase == PhaseJournaled {
		report.Operation.Status = "active"
		report.Operation.PhaseAfter = string(PhaseJournaled)
		report.Operation.Resumable = true
	}
	if journalCreation.DurableReplayPhase == PhaseStageCreated {
		report.Operation.Status = "active"
		report.Operation.PhaseAfter = string(PhaseStageCreated)
		report.Operation.Resumable = true
	}
	if handle != nil {
		defer handle.subtree.Close()
	}
	if err != nil {
		report.addIssue("journal.initialize_failed", "the private operation journal was not fully initialized", nil)
		classifyExecutionError(&report, err)
		return report, err
	}
	report.Operation = OperationReport{
		ID: handle.state.OperationID.String(), Status: "active",
		PhaseBefore: "planned", PhaseAfter: string(handle.state.Phase), Resumable: true,
	}
	operationDirectory, _ := OperationDirectoryName(handle.state.OperationID)
	operationRoot := filepath.Join(targetRoot, operationDirectory)
	err = continueRun(ctx, options.Meta, source, layout, targetRoot, operationRoot, handle, &report)
	report.Operation.PhaseAfter = string(handle.state.Phase)
	if err != nil {
		classifyExecutionError(&report, err)
		return report, err
	}
	report.Outcome = OutcomeMaterializedVerified
	report.Operation.Status = "terminal"
	report.Operation.Resumable = false
	return report, nil
}

func Resume(ctx context.Context, options ResumeOptions) (Report, error) {
	variantID := ""
	if options.Meta != nil {
		variantID = options.Meta.MetafileVariantID
	}
	report := newReport(variantID, options.ExpectedPlanID, options.Limits)
	if err := options.Limits.Validate(); err != nil {
		report.Outcome = OutcomeBlocked
		report.addBlocker("limits.invalid", "materialize limits are invalid")
		return report, fmt.Errorf("%w: invalid limits", ErrPolicy)
	}
	if _, err := ParseOperationID(options.OperationID.String()); err != nil || !canonicalPlanID(options.ExpectedPlanID) {
		report.Outcome = OutcomeBlocked
		report.addBlocker("resume.selector_invalid", "the explicit operation or reviewed plan ID is invalid")
		return report, fmt.Errorf("%w: resume selector is invalid", ErrPolicy)
	}
	if options.TargetRoot == "" {
		report.Outcome = OutcomeBlocked
		report.addBlocker("target.invalid_root", "the target root is invalid")
		return report, fmt.Errorf("%w: target root is empty", ErrPolicy)
	}
	setOperationInspectionSelector(&report, options.OperationID)
	report.Source = SourceReport{Mode: "phase_unknown", Outcome: "not_observed"}
	layout, err := BuildLayout(options.Meta, options.Limits)
	if err != nil {
		report.Outcome = OutcomeBlocked
		report.addBlocker("manifest.unsupported_layout", "the metafile layout is outside materialize v1")
		return report, err
	}
	targetRoot, err := filepath.Abs(options.TargetRoot)
	if err != nil {
		report.Outcome = OutcomeBlocked
		report.addBlocker("target.invalid_root", "the target root is invalid")
		return report, fmt.Errorf("%w: target root is invalid", ErrPolicy)
	}
	targetRoot = filepath.Clean(targetRoot)
	session, rootInfo, err := fsbind.BindExisting(targetRoot)
	if err != nil {
		report.Outcome = OutcomeBlocked
		report.addBlocker("target.unsupported_filesystem", "the target root cannot provide the required bound recovery semantics")
		return report, fmt.Errorf("%w: bind target root: %v", ErrPolicy, err)
	}
	defer session.Close()
	report.Target.RootIdentityBound = true
	report.Target.ObservedRootIdentity = rootInfo.Identity.String()
	report.Target.NoClobberCapable = true
	report.Target.StabilityAssurance = "non_atomic_bound_filesystem"
	handle, err := openJournal(ctx, session, options.OperationID, options.Limits)
	if err != nil {
		classifyJournalOpenReport(&report, err)
		return report, err
	}
	defer handle.subtree.Close()
	report.Target.ExpectedRootIdentity = handle.intent.TargetRootIdentity
	report.Target.SameFilesystemStage = handle.state.StageContainerIdentity != ""
	setHistoricalPublicationFromPhase(&report, handle.state.Phase)
	report.Operation = OperationReport{
		ID: options.OperationID.String(), Status: "active",
		PhaseBefore: string(handle.state.Phase), PhaseAfter: string(handle.state.Phase), Resumable: true,
	}
	report.Used.ScratchEntries = handle.scratchEntries
	report.Used.ScratchBytes = handle.scratchBytes
	if err := validateResumeIntent(options.Meta, layout, handle.intent, options.ExpectedPlanID); err != nil {
		report.Outcome = OutcomeBlocked
		report.addBlocker("resume.selector_mismatch", "the explicit metafile or reviewed plan does not select this healthy operation intent")
		return report, err
	}
	report.Plan.ObservedID = handle.intent.PlanID
	report.Plan.Matches = true
	if !handle.state.Terminal && scratchCapacityDefinitelyExhausted(handle) {
		err := scratchCapacityError("retained scratch prevents the next resume transition")
		markScratchCapacityBlocked(&report)
		report.Outcome = OutcomeBlocked
		return report, err
	}
	if handle.state.Phase != PhaseAbandoned {
		if err := handle.confirmReplayDurability(ctx); err != nil {
			report.addIssue("journal.durability_unconfirmed", "the visible journal could not be durably reconfirmed before resume", nil)
			classifyExecutionError(&report, err)
			return report, err
		}
		report.Writes.JournalDurabilityConfirms++
	}
	if handle.state.Phase == PhaseCommitted {
		report.Source = SourceReport{Mode: "not_required_after_staging", Outcome: "not_requested"}
		report.Target.Publication = "historical_committed"
		if err := confirmJournaledFinal(ctx, layout, handle, &report); err != nil {
			report.Operation.Resumable = false
			classifyExecutionError(&report, err)
			return report, err
		}
		finalIdentity, parseErr := fsbind.ParseIdentity(handle.state.FinalIdentity)
		if parseErr != nil {
			err = fmt.Errorf("%w: final identity is invalid", ErrCorruptJournal)
			report.Operation.Resumable = false
			classifyExecutionError(&report, err)
			return report, err
		}
		verifyBytes, verifyErr := verifyFinalLayout(ctx, options.Meta, layout, targetRoot, handle, finalIdentity)
		report.Used.FinalVerifyBytes += verifyBytes
		if verifyErr != nil {
			report.Operation.Resumable = false
			classifyExecutionError(&report, verifyErr)
			return report, verifyErr
		}
		report.Outcome = OutcomeAlreadyCommitted
		report.Operation.Status = "terminal"
		report.Operation.Resumable = false
		report.Target.Publication = "historical_committed_reverified"
		report.Target.FinalContentVerified = true
		report.Target.StabilityAssurance = "same_invocation_bracketed_non_atomic"
		return report, nil
	}
	if handle.state.Phase == PhaseAbandoned {
		report.Source = SourceReport{Mode: "not_required_after_staging", Outcome: "not_requested"}
		report.Outcome = OutcomeAbandoned
		report.Operation.Status = "terminal"
		report.Operation.Resumable = false
		report.addBlocker("operation.abandoned", "the operation was explicitly abandoned and its retained staging cannot resume")
		return report, fmt.Errorf("%w: operation is abandoned", ErrPolicy)
	}
	var source *metafile.VerifiedSource
	if handle.state.Phase == PhaseJournaled || handle.state.Phase == PhaseStageCreated || handle.state.Phase == PhaseFileStaged {
		if options.Discovery != nil {
			source, _ = options.Discovery.VerifiedSource(options.Meta)
		}
		if source == nil {
			report.Outcome = OutcomeBlocked
			report.addBlocker("source.process_authority_missing", "resume before stage verification requires fresh unique live discovery")
			return report, fmt.Errorf("%w: fresh verified source authority is unavailable", ErrPolicy)
		}
		plan, planErr := seed.BuildMaterializePlanFromVerified(ctx, options.Meta, source, targetRoot, StrategyCopy)
		if planErr != nil {
			report.Outcome = OutcomeBlocked
			report.addBlocker("plan.rebuild_failed", "the live resume plan could not be rebuilt")
			return report, planErr
		}
		report.Plan.ObservedID = plan.ID
		report.Plan.Matches = plan.ID == handle.intent.PlanID
		if !report.Plan.Matches {
			report.Outcome = OutcomeBlocked
			report.addBlocker("plan.id_mismatch", "the fresh source plan differs from the operation intent")
			return report, fmt.Errorf("%w: fresh plan differs from operation intent", ErrPolicy)
		}
		report.Source = SourceReport{
			Mode: "live_discovery", Outcome: "verified_unique",
			PreconditionsRechecked: true, ContentVerified: true,
		}
	} else {
		report.Source = SourceReport{Mode: "not_required_after_staging", Outcome: "not_requested"}
	}
	directoryName, _ := OperationDirectoryName(options.OperationID)
	operationRoot := filepath.Join(targetRoot, directoryName)
	err = continueRun(ctx, options.Meta, source, layout, targetRoot, operationRoot, handle, &report)
	report.Operation.PhaseAfter = string(handle.state.Phase)
	if err != nil {
		classifyExecutionError(&report, err)
		return report, err
	}
	report.Outcome = OutcomeMaterializedVerified
	report.Operation.Status = "terminal"
	report.Operation.Resumable = false
	return report, nil
}

func validateResumeIntent(meta *metafile.MetaInfo, layout Layout, intent Intent, expectedPlanID string) error {
	if meta == nil || intent.MetafileVariantID != meta.MetafileVariantID || intent.InfoHashV1 != meta.InfoHashV1 ||
		intent.InfoHashV2 != meta.InfoHashV2 || intent.PlanID != expectedPlanID || intent.Strategy != StrategyCopy ||
		intent.MultiFile != layout.MultiFile || intent.ManifestFiles != len(layout.Files) ||
		intent.ContentBytes != layout.ContentBytes || intent.ManifestPathBytes != layout.ManifestPathBytes ||
		intent.NamespaceObjects != layout.NamespaceObjects || intent.NamespaceBytes != layout.NamespaceBytes ||
		len(intent.FinalRawComponentsBase64) != 1 || intent.FinalRawComponentsBase64[0] != layout.FinalRawBase64 {
		return fmt.Errorf("%w: resume selector disagrees with operation intent", ErrPolicy)
	}
	return nil
}

func classifyExecutionError(report *Report, err error) {
	if report == nil {
		return
	}
	if report.Target.Publication == "committed" {
		report.Outcome = OutcomeAlreadyCommitted
		report.Operation.Status = "terminal"
		report.Operation.Resumable = false
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		report.Outcome = OutcomeInterrupted
		return
	}
	if errors.Is(err, ErrPolicy) || errors.Is(err, fsbind.ErrAlreadyExists) || errors.Is(err, fsbind.ErrUnsupported) {
		report.Outcome = OutcomeBlocked
		if errors.Is(err, errScratchCapacity) {
			markScratchCapacityBlocked(report)
		}
		return
	}
	if errors.Is(err, ErrIntegrity) || errors.Is(err, ErrCorruptJournal) {
		if publicationVisible(report.Target.Publication) {
			report.Outcome = OutcomePublishedIntegrityFailed
		} else {
			report.Outcome = OutcomeIntegrityFailed
		}
		report.Operation.Resumable = false
		return
	}
	if errors.Is(err, fsbind.ErrDurabilityUnconfirmed) {
		if publicationVisible(report.Target.Publication) {
			report.Outcome = OutcomePublishedDurabilityUnconfirmed
		} else {
			report.Outcome = OutcomeInterrupted
			report.addIssue("private_state.durability_unconfirmed", "a private journal or staging write was observed but its durability was not confirmed", nil)
		}
		return
	}
	if errors.Is(err, fsbind.ErrPublicationAmbiguous) &&
		(report.Target.Publication == "publication_ambiguous" || report.Target.Publication == "published_ambiguous" ||
			report.Target.Publication == "historical_publication_ambiguous") {
		report.Outcome = OutcomePublicationAmbiguous
		return
	}
	report.Outcome = OutcomeInterrupted
}

func publicationVisible(value string) bool {
	switch value {
	case "published", "published_recovered", "published_recovered_unverified", "published_durability_unconfirmed", "publication_ambiguous", "published_ambiguous", "historical_publication_ambiguous",
		"publication_conflict_observed", "historical_publication_unverified", "committed", "historical_committed":
		return true
	default:
		return false
	}
}

func setHistoricalPublicationFromPhase(report *Report, phase Phase) {
	if report == nil {
		return
	}
	switch phase {
	case PhasePublishIntent:
		report.Target.Publication = "historical_publication_uncertain"
	case PhasePublished, PhaseFinalVerified:
		report.Target.Publication = "historical_publication_unverified"
	case PhaseCommitted:
		report.Target.Publication = "historical_committed"
	}
}

func continueRun(ctx context.Context, meta *metafile.MetaInfo, source *metafile.VerifiedSource, layout Layout, targetRoot, operationRoot string, journal *journal, report *Report) (runErr error) {
	if journal == nil {
		return fmt.Errorf("%w: journal is unavailable", ErrCorruptJournal)
	}
	defer func() {
		report.Used.ScratchEntries = journal.scratchEntries
		report.Used.ScratchBytes = journal.scratchBytes
	}()
	buffer := make([]byte, journal.intent.Limits.CopyBufferBytes)
	if journal.state.Phase == PhaseJournaled {
		stagePath, _ := fsbind.PathFromComponents([]string{stageDirectoryName})
		listing, listErr := journal.subtree.List(ctx, stagePath, fsbind.ListLimits{MaxEntries: 1, MaxNameBytes: 1 << 20})
		if listErr != nil {
			return classifyJournalReadError(listErr, "pre-staging directory is unsafe")
		}
		if !listing.Complete || len(listing.Entries) != 0 {
			return fmt.Errorf("%w: pre-staging directory is not empty and stable", ErrCorruptJournal)
		}
		stage, inspectErr := journal.subtree.Inspect(ctx, stagePath)
		if inspectErr != nil {
			return classifyJournalReadError(inspectErr, "pre-staging directory is unsafe")
		}
		if stage.Kind != fsbind.ObjectKindDirectory {
			return fmt.Errorf("%w: staging directory is unavailable", ErrCorruptJournal)
		}
		receipt, appendErr := journal.append(ctx, Event{Phase: PhaseStageCreated, ObjectIdentity: stage.Identity.String()})
		report.recordJournalWrite(receipt, true)
		if appendErr != nil {
			return appendErr
		}
		if err := checkTransitionHook(PhaseStageCreated); err != nil {
			return err
		}
	}
	if journal.state.Phase == PhaseStageCreated || journal.state.Phase == PhaseFileStaged {
		if err := verifyStageContainer(ctx, journal); err != nil {
			return err
		}
		if source == nil {
			return fmt.Errorf("%w: live verified source is required to resume staging", ErrPolicy)
		}
		for _, file := range layout.Files[:len(journal.state.StagedFiles)] {
			if err := verifyRecordedStagedFile(ctx, file, journal, buffer); err != nil {
				if errors.Is(err, ErrIntegrity) {
					index := file.ManifestIndex
					report.addIssue("stage.recorded_file_changed", "a journaled staged file changed", &index)
				}
				return err
			}
		}
		if err := ensureStageDirectories(ctx, layout, journal, report); err != nil {
			report.addIssue("stage.directories_failed", "the private staging directory tree could not be completed", nil)
			return err
		}
		for _, file := range layout.Files[len(journal.state.StagedFiles):] {
			if err := ctx.Err(); err != nil {
				return err
			}
			staged, err := stageManifestFile(ctx, file, source, journal, buffer, report)
			if err != nil {
				index := file.ManifestIndex
				report.addIssue("stage.file_failed", "a manifest file could not be durably staged", &index)
				return err
			}
			receipt, err := journal.append(ctx, Event{
				Phase: PhaseFileStaged, ManifestIndex: &file.ManifestIndex, Bytes: file.Length,
				ObjectIdentity: staged.Identity.String(), ContentSHA256: staged.Digest,
			})
			report.recordJournalWrite(receipt, true)
			if err != nil {
				return err
			}
			if err := checkTransitionHook(PhaseFileStaged); err != nil {
				return err
			}
		}
		stageIdentity, verifyBytes, err := verifyStagedLayout(ctx, meta, layout, operationRoot, journal)
		report.Used.StageVerifyBytes += verifyBytes
		if err != nil {
			if errors.Is(err, ErrIntegrity) {
				report.addIssue("stage.exact_verification_failed", "the complete staged layout failed exact torrent verification", nil)
			}
			return err
		}
		report.Target.StageContentVerified = true
		report.Target.StabilityAssurance = "same_invocation_bracketed_non_atomic"
		receipt, err := journal.append(ctx, Event{
			Phase: PhaseStageVerified, Bytes: layout.ContentBytes,
			ObjectIdentity: stageIdentity.String(), Proof: layout.Proof,
		})
		report.recordJournalWrite(receipt, true)
		if err != nil {
			return err
		}
		if err := checkTransitionHook(PhaseStageVerified); err != nil {
			return err
		}
	}
	if journal.state.Phase == PhaseStageVerified {
		stageIdentity, err := fsbind.ParseIdentity(journal.state.StageIdentity)
		if err != nil {
			return fmt.Errorf("%w: stage identity is invalid", ErrCorruptJournal)
		}
		observedIdentity, verifyBytes, verifyErr := verifyStagedLayout(ctx, meta, layout, operationRoot, journal)
		report.Used.StageVerifyBytes += verifyBytes
		if verifyErr != nil {
			if errors.Is(verifyErr, ErrIntegrity) {
				report.addIssue("stage.reverification_failed", "the staged layout changed before publish intent", nil)
			}
			return verifyErr
		}
		if !observedIdentity.Equal(stageIdentity) {
			report.addIssue("stage.reverification_failed", "the staged layout changed before publish intent", nil)
			return fmt.Errorf("%w: staged layout identity changed", ErrIntegrity)
		}
		report.Target.StageContentVerified = true
		if _, finalErr := journal.session.InspectRoot(ctx, layout.FinalName); finalErr == nil {
			report.addBlocker("target.final_exists_before_publish_intent", "the final target appeared before durable publish intent and was not overwritten")
			return fmt.Errorf("%w: final target appeared before durable publish intent", fsbind.ErrAlreadyExists)
		} else if !errors.Is(finalErr, fsbind.ErrNotFound) {
			return finalErr
		}
		receipt, err := journal.append(ctx, Event{
			Phase: PhasePublishIntent, Bytes: layout.ContentBytes, ObjectIdentity: stageIdentity.String(),
		})
		report.recordJournalWrite(receipt, true)
		if err != nil {
			return err
		}
		if err := checkTransitionHook(PhasePublishIntent); err != nil {
			return err
		}
	}
	if journal.state.Phase == PhasePublishIntent {
		finalIdentity, err := publishOrRecover(ctx, meta, layout, operationRoot, journal, report)
		if err != nil {
			return err
		}
		receipt, err := journal.append(ctx, Event{
			Phase: PhasePublished, Bytes: layout.ContentBytes, ObjectIdentity: finalIdentity.String(),
		})
		report.recordJournalWrite(receipt, true)
		if err != nil {
			return err
		}
		if err := checkTransitionHook(PhasePublished); err != nil {
			return err
		}
	}
	if (journal.state.Phase == PhasePublished || journal.state.Phase == PhaseFinalVerified) && !report.Target.DurabilityConfirmed {
		if err := confirmJournaledFinal(ctx, layout, journal, report); err != nil {
			return err
		}
	}
	if journal.state.Phase == PhasePublished {
		finalIdentity, err := fsbind.ParseIdentity(journal.state.FinalIdentity)
		if err != nil {
			return fmt.Errorf("%w: final identity is invalid", ErrCorruptJournal)
		}
		verifyBytes, err := verifyFinalLayout(ctx, meta, layout, targetRoot, journal, finalIdentity)
		report.Used.FinalVerifyBytes += verifyBytes
		if err != nil {
			if errors.Is(err, ErrIntegrity) {
				report.addIssue("final.exact_verification_failed", "the published layout failed exact torrent verification", nil)
			}
			return err
		}
		report.Target.FinalContentVerified = true
		report.Target.StabilityAssurance = "same_invocation_bracketed_non_atomic"
		receipt, err := journal.append(ctx, Event{
			Phase: PhaseFinalVerified, Bytes: layout.ContentBytes,
			ObjectIdentity: finalIdentity.String(), Proof: layout.Proof,
		})
		report.recordJournalWrite(receipt, true)
		if err != nil {
			return err
		}
		if err := checkTransitionHook(PhaseFinalVerified); err != nil {
			return err
		}
	}
	if journal.state.Phase == PhaseFinalVerified {
		finalIdentity, err := fsbind.ParseIdentity(journal.state.FinalIdentity)
		if err != nil {
			return fmt.Errorf("%w: final identity is invalid", ErrCorruptJournal)
		}
		verifyBytes, verifyErr := verifyFinalLayout(ctx, meta, layout, targetRoot, journal, finalIdentity)
		report.Used.FinalVerifyBytes += verifyBytes
		if verifyErr != nil {
			if errors.Is(verifyErr, ErrIntegrity) {
				report.addIssue("final.reverification_failed", "the published layout changed before commit", nil)
			}
			return verifyErr
		}
		report.Target.FinalContentVerified = true
		journal.scratchObserved = false
		if err := journal.observeScratch(ctx); err != nil {
			return err
		}
		if journal.scratchEntries != 0 || journal.scratchBytes != 0 {
			return fmt.Errorf("%w: retained scratch prevents final commit", ErrPolicy)
		}
		receipt, err := journal.append(ctx, Event{
			Phase: PhaseCommitted, Bytes: layout.ContentBytes, ObjectIdentity: finalIdentity.String(),
		})
		report.recordJournalWrite(receipt, true)
		if err != nil {
			return err
		}
		report.Target.Publication = "committed"
		report.Target.StabilityAssurance = "same_invocation_bracketed_non_atomic"
		if err := checkTransitionHook(PhaseCommitted); err != nil {
			return err
		}
		return nil
	}
	if journal.state.Phase == PhaseCommitted {
		report.Target.Publication = "committed"
		return nil
	}
	return fmt.Errorf("%w: materialize journal phase cannot continue", ErrCorruptJournal)
}

func confirmJournaledFinal(ctx context.Context, layout Layout, journal *journal, report *Report) error {
	identity, err := fsbind.ParseIdentity(journal.state.FinalIdentity)
	if err != nil {
		return fmt.Errorf("%w: final identity is invalid", ErrCorruptJournal)
	}
	wantKind := fsbind.ObjectKindRegular
	if layout.MultiFile {
		wantKind = fsbind.ObjectKindDirectory
	}
	object, err := journal.session.InspectRoot(ctx, layout.FinalName)
	if err != nil {
		return classifyBoundContentError(err)
	}
	if object.Kind != wantKind || !object.Identity.Equal(identity) {
		return fmt.Errorf("%w: journaled final object is absent or changed", ErrIntegrity)
	}
	if err := syncJournaledRoot(ctx, journal); err != nil {
		classified := classifyPublishedSyncError(err)
		if !errors.Is(classified, ErrIntegrity) {
			report.Target.Publication = "published_durability_unconfirmed"
		}
		return classified
	}
	report.Target.Publication = "published"
	report.Target.DurabilityConfirmed = true
	return nil
}

func publishOrRecover(ctx context.Context, meta *metafile.MetaInfo, layout Layout, operationRoot string, journal *journal, report *Report) (fsbind.Identity, error) {
	stageIdentity, err := fsbind.ParseIdentity(journal.state.StageIdentity)
	if err != nil {
		return fsbind.Identity{}, fmt.Errorf("%w: stage identity is invalid", ErrCorruptJournal)
	}
	wantKind := fsbind.ObjectKindRegular
	if layout.MultiFile {
		wantKind = fsbind.ObjectKindDirectory
	}
	stageTop, _ := fsbind.PathFromComponents([]string{stageDirectoryName, layout.FinalName})
	final, finalErr := journal.session.InspectRoot(ctx, layout.FinalName)
	if finalErr == nil {
		if final.Kind != wantKind || !final.Identity.Equal(stageIdentity) {
			report.Target.Publication = "publication_conflict_observed"
			return fsbind.Identity{}, fmt.Errorf("%w: final target conflicts with the publish intent", ErrIntegrity)
		}
		if _, stageErr := journal.subtree.Inspect(ctx, stageTop); stageErr == nil {
			report.Target.Publication = "historical_publication_ambiguous"
			return fsbind.Identity{}, fsbind.ErrPublicationAmbiguous
		} else if !errors.Is(stageErr, fsbind.ErrNotFound) {
			return fsbind.Identity{}, classifyPublishIntentObservationError(report, stageErr)
		}
		report.Target.Publication = "published_recovered_unverified"
		if err := syncJournaledRoot(ctx, journal); err != nil {
			classified := classifyPublishedSyncError(err)
			if !errors.Is(classified, ErrIntegrity) {
				report.Target.Publication = "published_durability_unconfirmed"
			}
			return fsbind.Identity{}, classified
		}
		report.Target.Publication = "published_recovered"
		report.Target.DurabilityConfirmed = true
		report.Target.NoClobber = false
		report.Warnings = append(report.Warnings, "a recovered publication proves current identity and durability, not the historical no-clobber action")
		return final.Identity, nil
	}
	if !errors.Is(finalErr, fsbind.ErrNotFound) {
		return fsbind.Identity{}, classifyPublishIntentObservationError(report, finalErr)
	}
	stage, err := journal.subtree.Inspect(ctx, stageTop)
	if err != nil {
		return fsbind.Identity{}, classifyBoundContentError(err)
	}
	if stage.Kind != wantKind || !stage.Identity.Equal(stageIdentity) {
		return fsbind.Identity{}, fmt.Errorf("%w: stage object disagrees with the publish intent", ErrIntegrity)
	}
	observedIdentity, verifyBytes, verifyErr := verifyStagedLayout(ctx, meta, layout, operationRoot, journal)
	report.Used.StageVerifyBytes += verifyBytes
	if verifyErr != nil {
		if errors.Is(verifyErr, ErrIntegrity) {
			report.addIssue("stage.reverification_failed", "the staged layout changed after publish intent", nil)
		}
		return fsbind.Identity{}, verifyErr
	}
	if !observedIdentity.Equal(stageIdentity) {
		report.addIssue("stage.reverification_failed", "the staged layout changed after publish intent", nil)
		return fsbind.Identity{}, fmt.Errorf("%w: staged layout reverification failed", ErrIntegrity)
	}
	report.Target.StageContentVerified = true
	publication, publishErr := runPublication("final", func() (fsbind.Publication, error) {
		return journal.session.PublishNoReplace(ctx, journal.subtree, stageTop, layout.FinalName)
	})
	if publication.Attempted {
		report.Writes.FinalPublicationAttempts++
	}
	if publication.Published {
		report.WritesPerformed++
		report.Writes.FinalLayoutPublications++
		report.Target.Publication = "published"
	}
	if publishErr != nil {
		if publicationResultAmbiguous(publication, publishErr) {
			if publication.Published {
				report.Target.Publication = "published_ambiguous"
			} else {
				report.Target.Publication = "publication_ambiguous"
			}
			report.WritesUncertain = true
			report.Writes.AmbiguousPublications++
		}
		if publication.Published && publication.Durability != fsbind.DurabilityConfirmed && !publicationResultAmbiguous(publication, publishErr) {
			report.Target.Publication = "published_durability_unconfirmed"
		}
		return fsbind.Identity{}, classifyBoundContentError(publishErr)
	}
	if !publication.Published {
		if publicationResultAmbiguous(publication, nil) {
			report.Target.Publication = "publication_ambiguous"
			report.WritesUncertain = true
			report.Writes.AmbiguousPublications++
		}
		return fsbind.Identity{}, fsbind.ErrPublicationAmbiguous
	}
	if publication.Durability != fsbind.DurabilityConfirmed {
		return fsbind.Identity{}, fsbind.ErrDurabilityUnconfirmed
	}
	if !publication.FinalIdentity.Equal(stageIdentity) || !publication.SourceIdentity.Equal(stageIdentity) {
		return fsbind.Identity{}, fmt.Errorf("%w: published final identity disagrees with the verified stage", ErrIntegrity)
	}
	report.Target.DurabilityConfirmed = true
	report.Target.NoClobber = true
	if namespacePublishHook != nil {
		if err := namespacePublishHook(publication); err != nil {
			return fsbind.Identity{}, err
		}
	}
	return publication.FinalIdentity, nil
}

func ensureStageDirectories(ctx context.Context, layout Layout, journal *journal, report *Report) error {
	for _, components := range layout.Directories {
		pathComponents := append([]string{stageDirectoryName}, components...)
		path, err := fsbind.PathFromComponents(pathComponents)
		if err != nil {
			return err
		}
		receipt, err := journal.subtree.MkdirAll(ctx, path)
		report.Writes.StagedDirectories += receipt.DirectoriesCreated
		report.WritesPerformed += receipt.DirectoriesCreated
		if err != nil {
			return classifyBoundContentError(err)
		}
	}
	paths := make([][]string, 0, len(layout.Directories)+1)
	paths = append(paths, []string{stageDirectoryName})
	for _, components := range layout.Directories {
		paths = append(paths, append([]string{stageDirectoryName}, components...))
	}
	for _, components := range paths {
		path, _ := fsbind.PathFromComponents(components)
		if err := journal.subtree.SyncDirectory(ctx, path); err != nil {
			return classifyBoundContentError(err)
		}
	}
	return nil
}

func stageManifestFile(ctx context.Context, layoutFile LayoutFile, source *metafile.VerifiedSource, journal *journal, buffer []byte, report *Report) (copyReceipt, error) {
	destinationComponents := append([]string{stageDirectoryName}, layoutFile.Components...)
	destination, err := fsbind.PathFromComponents(destinationComponents)
	if err != nil {
		return copyReceipt{}, err
	}
	if existing, inspectErr := journal.subtree.Inspect(ctx, destination); inspectErr == nil {
		if existing.Kind != fsbind.ObjectKindRegular || existing.SizeBytes != layoutFile.Length {
			return copyReceipt{}, fmt.Errorf("%w: unjournaled stage object is unsafe", ErrIntegrity)
		}
		digest, identity, hashErr := hashStagedFile(ctx, journal.subtree, destination, layoutFile.Length, buffer)
		if hashErr != nil {
			return copyReceipt{}, classifyBoundContentError(hashErr)
		}
		expected, readBytes, sourceErr := hashVerifiedSource(ctx, source, layoutFile, buffer)
		report.Used.SourceCopyBytes += readBytes
		if sourceErr != nil {
			return copyReceipt{}, sourceErr
		}
		if digest != expected {
			return copyReceipt{}, fmt.Errorf("%w: unjournaled stage file does not match the live verified source", ErrIntegrity)
		}
		return copyReceipt{Digest: digest, Identity: identity}, nil
	} else if !errors.Is(inspectErr, fsbind.ErrNotFound) {
		return copyReceipt{}, classifyBoundContentError(inspectErr)
	}
	nonce, err := randomHex(16)
	if err != nil {
		return copyReceipt{}, err
	}
	scratch, err := fsbind.PathFromComponents([]string{
		scratchDirectoryName, fmt.Sprintf("copy-%09d-%s.pending", layoutFile.ManifestIndex, nonce),
	})
	if err != nil {
		return copyReceipt{}, err
	}
	if err := journal.reserveScratch(ctx, layoutFile.Length); err != nil {
		return copyReceipt{}, err
	}
	target, err := journal.subtree.CreateRegular(ctx, scratch)
	if err != nil {
		return copyReceipt{}, err
	}
	report.WritesPerformed++
	report.Writes.ScratchFilesCreated++
	hasher := sha256.New()
	var copied int64
	if layoutFile.Length > 0 {
		_, consumeErr := source.ConsumeVerifiedFile(ctx, layoutFile.ManifestIndex, func(reader io.Reader) error {
			written, copyErr := io.CopyBuffer(io.MultiWriter(target, hasher), reader, buffer)
			copied = written
			return copyErr
		})
		report.Used.SourceCopyBytes += copied
		report.Used.TargetWriteBytes += copied
		report.Writes.BytesWritten += copied
		report.Writes.ScratchBytesWritten += copied
		if consumeErr != nil {
			_ = target.Close()
			return copyReceipt{Bytes: copied}, consumeErr
		}
	}
	if copied != layoutFile.Length {
		_ = target.Close()
		return copyReceipt{Bytes: copied}, fmt.Errorf("%w: copied byte count disagrees with manifest", ErrIntegrity)
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		return copyReceipt{Bytes: copied}, err
	}
	info, err := target.Info()
	closeErr := target.Close()
	if err != nil {
		return copyReceipt{Bytes: copied}, err
	}
	if closeErr != nil {
		return copyReceipt{Bytes: copied}, closeErr
	}
	if info.SizeBytes != layoutFile.Length || info.Identity.IsZero() {
		return copyReceipt{Bytes: copied}, fmt.Errorf("%w: staged scratch file changed before publication", ErrIntegrity)
	}
	publication, err := runPublication("stage", func() (fsbind.Publication, error) {
		return journal.subtree.CommitRegularNoReplace(ctx, scratch, destination)
	})
	if publication.Attempted {
		report.Writes.StagePublicationAttempts++
	}
	if publicationResultAmbiguous(publication, err) {
		report.WritesUncertain = true
		report.Writes.AmbiguousPublications++
	}
	receipt := copyReceipt{
		Bytes: copied, Digest: hex.EncodeToString(hasher.Sum(nil)),
		Identity: publication.FinalIdentity, Publication: publication,
	}
	if publication.Published {
		journal.releaseScratch(layoutFile.Length)
		report.WritesPerformed++
		report.Writes.StagedFiles++
	}
	if err != nil {
		return receipt, fmt.Errorf("commit staged scratch failed (published=%t, durability=%s): %w", publication.Published, publication.Durability, classifyBoundContentError(err))
	}
	if !publication.Published {
		return receipt, fsbind.ErrPublicationAmbiguous
	}
	if publication.Durability != fsbind.DurabilityConfirmed {
		return receipt, fsbind.ErrDurabilityUnconfirmed
	}
	if !publication.SourceIdentity.Equal(info.Identity) || !publication.FinalIdentity.Equal(info.Identity) {
		return receipt, fmt.Errorf("%w: staged publication identity disagrees with the copied bytes", ErrIntegrity)
	}
	return receipt, nil
}

func verifyRecordedStagedFile(ctx context.Context, layoutFile LayoutFile, journal *journal, buffer []byte) error {
	if layoutFile.ManifestIndex >= len(journal.state.StagedFiles) {
		return fmt.Errorf("%w: staged file event is missing", ErrCorruptJournal)
	}
	recorded := journal.state.StagedFiles[layoutFile.ManifestIndex]
	if recorded.Bytes != layoutFile.Length {
		return fmt.Errorf("%w: staged file byte count disagrees", ErrCorruptJournal)
	}
	path, _ := fsbind.PathFromComponents(append([]string{stageDirectoryName}, layoutFile.Components...))
	digest, identity, err := hashStagedFile(ctx, journal.subtree, path, layoutFile.Length, buffer)
	if err != nil {
		if errors.Is(err, fsbind.ErrNotFound) || errors.Is(err, fsbind.ErrUnsafeObject) ||
			errors.Is(err, fsbind.ErrBindingChanged) || errors.Is(err, fsbind.ErrCrossFilesystem) {
			return fmt.Errorf("%w: staged file object changed", ErrIntegrity)
		}
		return err
	}
	if digest != recorded.ContentSHA256 || identity.String() != recorded.ObjectIdentity {
		return fmt.Errorf("%w: staged file identity or digest disagrees", ErrIntegrity)
	}
	return nil
}

func hashVerifiedSource(ctx context.Context, source *metafile.VerifiedSource, layoutFile LayoutFile, buffer []byte) (string, int64, error) {
	hasher := sha256.New()
	if layoutFile.Length == 0 {
		return hex.EncodeToString(hasher.Sum(nil)), 0, nil
	}
	var read int64
	_, err := source.ConsumeVerifiedFile(ctx, layoutFile.ManifestIndex, func(reader io.Reader) error {
		count, copyErr := io.CopyBuffer(hasher, reader, buffer)
		read = count
		return copyErr
	})
	if err != nil || read != layoutFile.Length {
		return "", read, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), read, nil
}

func hashStagedFile(ctx context.Context, subtree *fsbind.Subtree, path fsbind.Path, expected int64, buffer []byte) (string, fsbind.Identity, error) {
	file, err := subtree.OpenRegular(ctx, path)
	if err != nil {
		return "", fsbind.Identity{}, err
	}
	digest, identity, err := hashBoundFile(ctx, file, expected, buffer)
	return digest, identity, err
}

func hashBoundFile(ctx context.Context, file *fsbind.File, expected int64, buffer []byte) (string, fsbind.Identity, error) {
	if file == nil {
		return "", fsbind.Identity{}, fsbind.ErrUnsafeObject
	}
	before, err := file.Info()
	if err != nil {
		_ = file.Close()
		return "", fsbind.Identity{}, err
	}
	if before.SizeBytes != expected {
		_ = file.Close()
		return "", fsbind.Identity{}, fsbind.ErrUnsafeObject
	}
	hasher := sha256.New()
	reader := &contextExactReader{ctx: ctx, reader: file, remaining: expected}
	read, readErr := io.CopyBuffer(hasher, reader, buffer)
	var extra [1]byte
	if readErr == nil {
		n, extraErr := file.Read(extra[:])
		switch {
		case n != 0:
			readErr = fsbind.ErrUnsafeObject
		case errors.Is(extraErr, io.EOF):
		case extraErr != nil:
			readErr = extraErr
		default:
			readErr = io.ErrNoProgress
		}
	}
	after, statErr := file.Info()
	closeErr := file.Close()
	if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
		return "", fsbind.Identity{}, readErr
	}
	if readErr != nil {
		return "", fsbind.Identity{}, readErr
	}
	if statErr != nil {
		return "", fsbind.Identity{}, statErr
	}
	if closeErr != nil {
		return "", fsbind.Identity{}, closeErr
	}
	if read != expected ||
		!before.Identity.Equal(after.Identity) || before.SizeBytes != after.SizeBytes || !before.Modified.Equal(after.Modified) {
		return "", fsbind.Identity{}, fsbind.ErrUnsafeObject
	}
	return hex.EncodeToString(hasher.Sum(nil)), before.Identity, nil
}

type contextExactReader struct {
	ctx       context.Context
	reader    io.Reader
	remaining int64
}

func (reader *contextExactReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(buffer)) > reader.remaining {
		buffer = buffer[:reader.remaining]
	}
	n, err := reader.reader.Read(buffer)
	reader.remaining -= int64(n)
	if n == 0 && err == nil {
		return 0, io.ErrNoProgress
	}
	return n, err
}

func verifyStagedLayout(ctx context.Context, meta *metafile.MetaInfo, layout Layout, operationRoot string, journal *journal) (fsbind.Identity, int64, error) {
	beforeNamespace, err := auditStageNamespace(ctx, layout, journal)
	if err != nil {
		return fsbind.Identity{}, 0, err
	}
	bindings := make([]metafile.SourceBinding, 0, len(layout.Files))
	for _, file := range layout.Files {
		pathComponents := append([]string{stageDirectoryName}, file.Components...)
		boundPath, err := fsbind.PathFromComponents(pathComponents)
		if err != nil {
			return fsbind.Identity{}, 0, err
		}
		absolutePath := filepath.Join(append([]string{operationRoot}, pathComponents...)...)
		if file.Length == 0 {
			opened, openErr := journal.subtree.OpenRegular(ctx, boundPath)
			if openErr != nil {
				return fsbind.Identity{}, 0, classifyBoundContentError(openErr)
			}
			info, infoErr := opened.Info()
			closeErr := opened.Close()
			if infoErr != nil {
				return fsbind.Identity{}, 0, classifyBoundContentError(infoErr)
			}
			if closeErr != nil {
				return fsbind.Identity{}, 0, closeErr
			}
			if info.SizeBytes != 0 {
				return fsbind.Identity{}, 0, classifyBoundContentError(fsbind.ErrUnsafeObject)
			}
			continue
		}
		pathForOpen := boundPath
		bindings = append(bindings, metafile.SourceBinding{
			FileIndex: file.ManifestIndex, Path: absolutePath,
			Open: func() (metafile.SourceFile, error) {
				return journal.subtree.OpenRegular(ctx, pathForOpen)
			},
		})
	}
	verified, err := metafile.VerifySourceMap(ctx, meta, metafile.SourceMap{Bindings: bindings})
	if err != nil {
		return fsbind.Identity{}, 0, classifyBoundContentError(err)
	}
	if !verified.Result().Verified {
		return fsbind.Identity{}, verified.Result().BytesVerified, ErrIntegrity
	}
	afterNamespace, err := auditStageNamespace(ctx, layout, journal)
	if err != nil {
		return fsbind.Identity{}, verified.Result().BytesVerified, err
	}
	if !sameNamespaceSnapshot(beforeNamespace, afterNamespace) {
		return fsbind.Identity{}, verified.Result().BytesVerified, ErrIntegrity
	}
	if afterNamespace.topIdentity.IsZero() {
		return fsbind.Identity{}, verified.Result().BytesVerified, ErrIntegrity
	}
	return afterNamespace.topIdentity, verified.Result().BytesVerified, nil
}

func verifyFinalLayout(ctx context.Context, meta *metafile.MetaInfo, layout Layout, targetRoot string, journal *journal, expected fsbind.Identity) (int64, error) {
	published, err := journal.session.OpenPublishedRoot(layout.FinalName, expected)
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
	beforeNamespace, err := auditPublishedNamespace(ctx, layout, journal, published)
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
				return 0, classifyBoundContentError(fsbind.ErrUnsafeObject)
			}
			continue
		}
		pathForOpen := boundPath
		bindings = append(bindings, metafile.SourceBinding{
			FileIndex: file.ManifestIndex,
			Path:      absolutePath,
			Open: func() (metafile.SourceFile, error) {
				return published.OpenRegular(ctx, pathForOpen)
			},
		})
	}
	verified, err := metafile.VerifySourceMap(ctx, meta, metafile.SourceMap{Bindings: bindings})
	if err != nil {
		return 0, classifyBoundContentError(err)
	}
	if !verified.Result().Verified {
		return verified.Result().BytesVerified, ErrIntegrity
	}
	afterNamespace, err := auditPublishedNamespace(ctx, layout, journal, published)
	if err != nil {
		return verified.Result().BytesVerified, err
	}
	if !sameNamespaceSnapshot(beforeNamespace, afterNamespace) {
		return verified.Result().BytesVerified, ErrIntegrity
	}
	if err := published.Check(); err != nil {
		return verified.Result().BytesVerified, classifyBoundContentError(err)
	}
	if err := published.Close(); err != nil {
		return 0, err
	}
	reopened, err := journal.session.OpenPublishedRoot(layout.FinalName, expected)
	if err != nil {
		return 0, classifyBoundContentError(err)
	}
	if !reopened.Identity().Equal(expected) {
		_ = reopened.Close()
		return 0, fmt.Errorf("%w: final publication identity changed", ErrIntegrity)
	}
	if err := reopened.Close(); err != nil {
		return 0, err
	}
	return verified.Result().BytesVerified, nil
}

func classifyBoundContentError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, fsbind.ErrNotFound) || errors.Is(err, fsbind.ErrUnsafeObject) ||
		errors.Is(err, fsbind.ErrBindingChanged) || errors.Is(err, fsbind.ErrCrossFilesystem) {
		return fmt.Errorf("%w: bound content object changed", ErrIntegrity)
	}
	return err
}

func classifyPublishIntentObservationError(report *Report, err error) error {
	classified := classifyBoundContentError(err)
	if errors.Is(classified, ErrIntegrity) && report != nil {
		report.Target.Publication = "publication_conflict_observed"
	}
	return classified
}

func classifyPublishedSyncError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	classified := classifyBoundContentError(err)
	if errors.Is(classified, ErrIntegrity) || errors.Is(classified, fsbind.ErrDurabilityUnconfirmed) {
		return classified
	}
	return errors.Join(fsbind.ErrDurabilityUnconfirmed, classified)
}
