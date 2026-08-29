package materialize

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
)

type RetentionOutcome string

const (
	RetentionOutcomePruned          RetentionOutcome = "pruned"
	RetentionOutcomeAlreadyPruned   RetentionOutcome = "already_pruned"
	RetentionOutcomeBlocked         RetentionOutcome = "blocked"
	RetentionOutcomeInterrupted     RetentionOutcome = "interrupted"
	RetentionOutcomeIntegrityFailed RetentionOutcome = "integrity_failed"
)

type PruneOptions struct {
	Meta            *metafile.MetaInfo
	TargetRoot      string
	OperationID     OperationID
	ExpectedPlanID  string
	JournalLimits   Limits
	RetentionLimits RetentionLimits
}

type RetentionProofReport struct {
	Basis                     string `json:"basis"`
	TerminalEventID           string `json:"terminal_event_id,omitempty"`
	CurrentFinalVerified      bool   `json:"current_final_verified"`
	CurrentPublicationAbsent  bool   `json:"current_publication_absent"`
	FinalVerifyBytes          int64  `json:"final_verify_bytes"`
	HistoricalMarkerAuthority bool   `json:"historical_marker_authority"`
	Assurance                 string `json:"assurance"`
}

type RetentionTargetReport struct {
	ExpectedRootIdentity string `json:"expected_root_identity,omitempty"`
	ObservedRootIdentity string `json:"observed_root_identity,omitempty"`
	RootIdentityBound    bool   `json:"root_identity_bound"`
	FinalState           string `json:"final_state"`
	StabilityAssurance   string `json:"stability_assurance"`
}

type RetentionMarkerReport struct {
	State              string `json:"state"`
	IntentMarkerID     string `json:"intent_marker_id,omitempty"`
	CompleteMarkerID   string `json:"complete_marker_id,omitempty"`
	IntentDurable      bool   `json:"intent_durable"`
	CompletionDurable  bool   `json:"completion_durable"`
	ExactTombstone     bool   `json:"exact_tombstone"`
	OperationStateKept bool   `json:"operation_tombstone_retained"`
	PruneResumable     bool   `json:"prune_resumable"`
}

type RetentionWriteReport struct {
	ControlDirectoriesCreated int   `json:"control_directories_created"`
	MarkerTemporaryFiles      int   `json:"marker_temporary_files_created"`
	MarkerTemporaryBytes      int64 `json:"marker_temporary_bytes_written"`
	MarkerPublicationAttempts int   `json:"marker_publication_attempts"`
	MarkerPublications        int   `json:"marker_publications"`
	MarkerDurabilityConfirms  int   `json:"marker_durability_confirmations"`
	MarkerTemporaryRemovals   int   `json:"marker_temporary_removals"`
	RemovalAttempts           int   `json:"removal_attempts"`
	FilesRemoved              int   `json:"files_removed"`
	DirectoriesRemoved        int   `json:"directories_removed"`
	BytesRemoved              int64 `json:"bytes_removed"`
	AmbiguousRemovals         int   `json:"ambiguous_removals"`
}

type RetentionReport struct {
	Outcome         RetentionOutcome      `json:"outcome"`
	Effect          []string              `json:"effect"`
	WritesPerformed int                   `json:"writes_performed"`
	WritesUncertain bool                  `json:"writes_uncertain"`
	Operation       OperationReport       `json:"operation"`
	Plan            PlanReport            `json:"plan"`
	Target          RetentionTargetReport `json:"target"`
	Proof           RetentionProofReport  `json:"proof"`
	Markers         RetentionMarkerReport `json:"markers"`
	Writes          RetentionWriteReport  `json:"writes"`
	Limits          RetentionLimits       `json:"limits"`
	Used            RetentionUsage        `json:"used"`
	Blockers        []Finding             `json:"blockers"`
	Issues          []Finding             `json:"issues"`
	Warnings        []string              `json:"warnings"`
}

var retentionTransitionHook func(string) error

func newRetentionReport(options PruneOptions) RetentionReport {
	variantID := ""
	if options.Meta != nil {
		variantID = options.Meta.MetafileVariantID
	}
	return RetentionReport{
		Outcome: RetentionOutcomeInterrupted,
		Effect: []string{
			"read_private_materialize_operation_state",
			"write_private_materialize_retention_markers",
			"delete_private_materialize_operation_state",
		},
		Operation: OperationReport{
			ID: options.OperationID.String(), Status: "inspection_incomplete",
			PhaseBefore: "unknown", PhaseAfter: "unknown",
		},
		Plan: PlanReport{
			ExpectedID: options.ExpectedPlanID, MetafileVariantID: variantID, Strategy: StrategyCopy,
		},
		Target:  RetentionTargetReport{FinalState: "not_observed", StabilityAssurance: "not_observed"},
		Proof:   RetentionProofReport{Assurance: "not_observed"},
		Markers: RetentionMarkerReport{State: "not_started"},
		Limits:  options.RetentionLimits, Blockers: []Finding{}, Issues: []Finding{},
		Warnings: []string{
			"prune retains a small private tombstone and never deletes a source or published final layout",
			"filesystem proof is a same-invocation bracketed observation, not an atomic content snapshot",
		},
	}
}

// Prune removes the heavy private state of one explicitly selected terminal
// materialize operation. It never selects by age or latest state and never
// removes the public final layout, source bytes, or the retained tombstone.
func Prune(ctx context.Context, options PruneOptions) (RetentionReport, error) {
	report := newRetentionReport(options)
	if err := options.JournalLimits.Validate(); err != nil {
		report.Outcome = RetentionOutcomeBlocked
		report.addRetentionBlocker("limits.journal_invalid", "the installed materialize journal limits are invalid")
		return report, fmt.Errorf("%w: invalid journal limits", ErrPolicy)
	}
	if err := options.RetentionLimits.Validate(); err != nil {
		report.Outcome = RetentionOutcomeBlocked
		report.addRetentionBlocker("limits.retention_invalid", "materialize retention limits are invalid")
		return report, fmt.Errorf("%w: invalid retention limits", ErrPolicy)
	}
	if _, err := ParseOperationID(options.OperationID.String()); err != nil || !canonicalPlanID(options.ExpectedPlanID) || options.TargetRoot == "" {
		report.Outcome = RetentionOutcomeBlocked
		report.addRetentionBlocker("prune.selector_invalid", "target root, operation ID, or reviewed plan ID is invalid")
		return report, fmt.Errorf("%w: prune selector is invalid", ErrPolicy)
	}
	if err := ctx.Err(); err != nil {
		report.addRetentionIssue("prune.interrupted", "prune was canceled before the target root was observed")
		return report, err
	}
	absolute, err := filepath.Abs(options.TargetRoot)
	if err != nil {
		report.Outcome = RetentionOutcomeBlocked
		report.addRetentionBlocker("target.invalid_root", "the target root is invalid")
		return report, fmt.Errorf("%w: invalid target root", ErrPolicy)
	}
	session, rootInfo, err := fsbind.BindExisting(filepath.Clean(absolute))
	if err != nil {
		report.Outcome = RetentionOutcomeBlocked
		report.addRetentionBlocker("target.unsupported_filesystem", "the target root cannot provide bound deletion semantics")
		return report, fmt.Errorf("%w: bind target root: %v", ErrPolicy, err)
	}
	defer session.Close()
	report.Target.ObservedRootIdentity = rootInfo.Identity.String()
	report.Target.RootIdentityBound = true
	report.Target.StabilityAssurance = "non_atomic_bound_filesystem"
	forgetName, _ := ForgetRootName(options.OperationID)
	forgetMarker, _, _, _, forgetErr := readForgetRootIntent(ctx, session, forgetName, maxForgetMarkerBytes)
	if forgetErr == nil {
		if forgetMarker.OperationID != options.OperationID || forgetMarker.TargetRootIdentity != rootInfo.Identity.String() {
			classifyRetentionFailure(&report, ErrIntegrity)
			return report, fmt.Errorf("%w: durable forget intent is bound to another filesystem object", ErrIntegrity)
		}
		report.Plan.ObservedID = forgetMarker.PlanID
		report.Plan.Matches = forgetMarker.PlanID == options.ExpectedPlanID
		report.Plan.MetafileVariantID = forgetMarker.RetentionIntent.MetafileVariantID
		if forgetMarker.PlanID != options.ExpectedPlanID {
			report.Outcome = RetentionOutcomeBlocked
			report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "forgetting", "forget_intent_recorded", false
			report.Markers.State = "forget_intent_published"
			report.addRetentionBlocker("plan.id_mismatch", "the reviewed plan ID does not select this materialize forget intent")
			return report, fmt.Errorf("%w: durable forget intent differs from the explicit prune selector", ErrPolicy)
		}
		report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "forgetting", "forget_intent_recorded", false
		report.Markers.State = "forget_intent_published"
		report.Outcome = RetentionOutcomeBlocked
		report.addRetentionBlocker("operation.forget_required", "a private forget intent is visible; prune may no longer advance this operation")
		return report, fmt.Errorf("%w: materialize historical evidence deletion is in progress", ErrPolicy)
	}
	if !errors.Is(forgetErr, fsbind.ErrNotFound) {
		classifyRetentionFailure(&report, forgetErr)
		return report, forgetErr
	}

	handle, journalErr := openJournalForRetention(ctx, session, options.OperationID, options.JournalLimits)
	var subtree *fsbind.Subtree
	if handle != nil {
		subtree = handle.subtree
	} else {
		subtree, err = openRetentionSubtree(ctx, session, options.OperationID)
		if err != nil {
			classifyRetentionFailure(&report, err)
			return report, err
		}
	}
	defer subtree.Close()

	markerState, markerErr := loadRetentionMarkerState(ctx, subtree, options.OperationID, rootInfo.Identity)
	if markerErr != nil {
		classifyRetentionFailure(&report, markerErr)
		return report, markerErr
	}
	if handle == nil && !markerState.IntentPresent {
		if journalErr == nil {
			journalErr = fmt.Errorf("%w: retention intent is unavailable", ErrIntegrity)
		}
		classifyRetentionFailure(&report, journalErr)
		return report, journalErr
	}

	if markerState.IntentPresent {
		if err := applyRetentionMarkerSelection(&report, markerState, options); err != nil {
			classifyRetentionFailure(&report, err)
			return report, err
		}
		if handle != nil {
			if err := validateRetentionIntentAgainstJournal(markerState.Intent, handle); err != nil {
				classifyRetentionFailure(&report, err)
				return report, err
			}
		}
		if err := confirmRetentionMarkerDurability(ctx, subtree); err != nil {
			classifyRetentionFailure(&report, err)
			return report, err
		}
		report.Writes.MarkerDurabilityConfirms++
		report.Markers.IntentDurable = true
		intentID, receipt, err := ensureRetentionIntentMarker(ctx, subtree, markerState.Intent)
		report.recordRetentionMarker(receipt)
		if err != nil {
			classifyRetentionFailure(&report, err)
			return report, err
		}
		markerState.IntentID = intentID
		if markerState.CompletePresent {
			report.Markers.State = "completion_published_unverified_tombstone"
			report.Markers.CompleteMarkerID = markerState.CompleteID.String()
			report.Markers.CompletionDurable = true
			report.Markers.PruneResumable = false
			completeID, completeReceipt, completeErr := ensureRetentionCompleteMarker(ctx, subtree, markerState.Complete)
			report.recordRetentionMarker(completeReceipt)
			if completeErr != nil {
				classifyRetentionFailure(&report, completeErr)
				return report, completeErr
			}
			markerState.CompleteID = completeID
			fresh, loadErr := loadRetentionMarkerState(ctx, subtree, options.OperationID, rootInfo.Identity)
			if loadErr != nil {
				classifyRetentionFailure(&report, loadErr)
				return report, loadErr
			}
			if _, rootErr := retentionHeavyNames(ctx, subtree, true); rootErr != nil {
				classifyRetentionFailure(&report, rootErr)
				return report, rootErr
			}
			if auditErr := auditRetentionControlDirectory(ctx, subtree, fresh, false); auditErr != nil {
				classifyRetentionFailure(&report, auditErr)
				return report, auditErr
			}
			report.Markers.State = "complete"
			report.Markers.IntentMarkerID = fresh.IntentID.String()
			report.Markers.CompleteMarkerID = fresh.CompleteID.String()
			report.Markers.IntentDurable = true
			report.Markers.CompletionDurable = true
			report.Markers.ExactTombstone = true
			report.Markers.OperationStateKept = true
			report.Operation.Status = "retained"
			report.Operation.Resumable = false
			if report.WritesPerformed == 0 {
				report.Outcome = RetentionOutcomeAlreadyPruned
			} else {
				report.Outcome = RetentionOutcomePruned
			}
			return report, nil
		}
	}

	var nodes []retentionNode
	if !markerState.IntentPresent {
		if journalErr != nil || handle == nil {
			err := fmt.Errorf("%w: terminal journal authority is unavailable", ErrIntegrity)
			classifyRetentionFailure(&report, err)
			return report, err
		}
		marker, proofErr := prepareRetentionIntent(handle, options, &report)
		if proofErr != nil {
			classifyRetentionFailure(&report, proofErr)
			return report, proofErr
		}
		names, rootErr := retentionHeavyNames(ctx, subtree, false)
		if rootErr != nil {
			classifyRetentionFailure(&report, rootErr)
			return report, rootErr
		}
		if len(names) != len(retentionHeavyTopNames()) {
			err := fmt.Errorf("%w: terminal operation state is incomplete before pruning", ErrIntegrity)
			classifyRetentionFailure(&report, err)
			return report, err
		}
		var inventoryErr error
		nodes, report.Used, inventoryErr = inventoryRetentionTrees(ctx, subtree, names, options.RetentionLimits)
		if inventoryErr != nil {
			inventoryErr = classifyRetentionAuthorityError(inventoryErr)
			classifyRetentionFailure(&report, inventoryErr)
			return report, inventoryErr
		}
		if err := auditRetentionDirectoryBeforeIntent(ctx, subtree, marker); err != nil {
			classifyRetentionFailure(&report, err)
			return report, err
		}
		if err := proveRetentionIntentBasis(ctx, handle, filepath.Clean(absolute), options, marker, &report); err != nil {
			classifyRetentionFailure(&report, err)
			return report, err
		}
		intentID, receipt, publishErr := ensureRetentionIntentMarker(ctx, subtree, marker)
		report.recordRetentionMarker(receipt)
		if publishErr != nil {
			switch {
			case intentID != "" && (receipt.AlreadyPresent || receipt.Publication.Published):
				report.Markers.State = "intent_publication_observed_unverified"
				report.Markers.IntentMarkerID = intentID.String()
			case receipt.Publication.Attempted:
				report.Markers.State = "intent_publication_ambiguous"
			case receipt.DirectoryCreated || receipt.TemporaryCreated:
				report.Markers.State = "intent_preparation_incomplete"
			}
			classifyRetentionFailure(&report, publishErr)
			return report, publishErr
		}
		markerState = retentionMarkerState{
			DirectoryPresent: true, IntentPresent: true, Intent: marker, IntentID: intentID,
		}
		if retentionTransitionHook != nil {
			if hookErr := retentionTransitionHook("intent_published"); hookErr != nil {
				report.Markers.State = "intent_published"
				report.Markers.IntentMarkerID = intentID.String()
				report.Markers.IntentDurable = true
				report.Markers.PruneResumable = true
				classifyRetentionFailure(&report, hookErr)
				return report, hookErr
			}
		}
	} else {
		names, rootErr := retentionHeavyNames(ctx, subtree, false)
		if rootErr != nil {
			classifyRetentionFailure(&report, rootErr)
			return report, rootErr
		}
		if len(names) != 0 {
			var inventoryErr error
			nodes, report.Used, inventoryErr = inventoryRetentionTrees(ctx, subtree, names, options.RetentionLimits)
			if inventoryErr != nil {
				inventoryErr = classifyRetentionAuthorityError(inventoryErr)
				classifyRetentionFailure(&report, inventoryErr)
				return report, inventoryErr
			}
		}
	}

	report.Markers.State = "intent_published"
	report.Markers.IntentMarkerID = markerState.IntentID.String()
	report.Markers.IntentDurable = true
	report.Markers.OperationStateKept = true
	report.Markers.PruneResumable = true
	removedUsage, removeErr := removeRetentionNodes(ctx, subtree, nodes, report.Used)
	report.recordRetentionRemovals(removedUsage)
	if removeErr != nil {
		removeErr = classifyRetentionAuthorityError(removeErr)
		classifyRetentionFailure(&report, removeErr)
		return report, removeErr
	}
	if retentionTransitionHook != nil {
		if hookErr := retentionTransitionHook("heavy_state_removed"); hookErr != nil {
			classifyRetentionFailure(&report, hookErr)
			return report, hookErr
		}
	}
	if _, err := retentionHeavyNames(ctx, subtree, true); err != nil {
		classifyRetentionFailure(&report, err)
		return report, err
	}
	complete := RetentionComplete{
		Schema: RetentionCompleteSchemaV1, OperationID: options.OperationID,
		OperationRootIdentity: subtree.Identity().String(), TargetRootIdentity: rootInfo.Identity.String(),
		IntentMarkerID: markerState.IntentID,
	}
	completeID, completeReceipt, completeErr := ensureRetentionCompleteMarker(ctx, subtree, complete)
	report.recordRetentionMarker(completeReceipt)
	if completeErr != nil {
		if completeID != "" && (completeReceipt.AlreadyPresent || completeReceipt.Publication.Published) {
			report.Markers.State = "completion_publication_observed_unverified"
			report.Markers.CompleteMarkerID = completeID.String()
		} else if completeReceipt.Publication.Attempted {
			report.Markers.State = "completion_publication_ambiguous"
		}
		classifyRetentionFailure(&report, completeErr)
		return report, completeErr
	}
	fresh, loadErr := loadRetentionMarkerState(ctx, subtree, options.OperationID, rootInfo.Identity)
	if loadErr != nil {
		classifyRetentionFailure(&report, loadErr)
		return report, loadErr
	}
	if fresh.CompleteID != completeID {
		err := fmt.Errorf("%w: retention completion identity changed", ErrIntegrity)
		classifyRetentionFailure(&report, err)
		return report, err
	}
	if _, rootErr := retentionHeavyNames(ctx, subtree, true); rootErr != nil {
		classifyRetentionFailure(&report, rootErr)
		return report, rootErr
	}
	if auditErr := auditRetentionControlDirectory(ctx, subtree, fresh, false); auditErr != nil {
		classifyRetentionFailure(&report, auditErr)
		return report, auditErr
	}
	report.Markers.State = "complete"
	report.Markers.CompleteMarkerID = completeID.String()
	report.Markers.CompletionDurable = true
	report.Markers.ExactTombstone = true
	report.Markers.PruneResumable = false
	report.Operation.Status = "retained"
	report.Operation.Resumable = false
	report.Outcome = RetentionOutcomePruned
	return report, nil
}

func prepareRetentionIntent(handle *journal, options PruneOptions, report *RetentionReport) (RetentionIntent, error) {
	if handle == nil || handle.subtree == nil || !handle.state.Terminal ||
		(handle.state.Phase != PhaseCommitted && handle.state.Phase != PhaseAbandoned) {
		if report != nil {
			report.addRetentionBlocker("operation.not_terminal", "only committed or abandoned operations may be pruned")
		}
		return RetentionIntent{}, fmt.Errorf("%w: operation is not a supported terminal phase", ErrPolicy)
	}
	report.Operation = OperationReport{
		ID: handle.state.OperationID.String(), Status: "selected", PhaseBefore: string(handle.state.Phase),
		PhaseAfter: string(handle.state.Phase), Resumable: false,
	}
	report.Plan.ObservedID = handle.intent.PlanID
	report.Plan.Matches = handle.intent.PlanID == options.ExpectedPlanID
	report.Plan.MetafileVariantID = handle.intent.MetafileVariantID
	report.Target.ExpectedRootIdentity = handle.intent.TargetRootIdentity
	if !report.Plan.Matches {
		report.addRetentionBlocker("plan.id_mismatch", "the reviewed plan ID does not select this terminal operation")
		return RetentionIntent{}, fmt.Errorf("%w: reviewed plan ID differs from operation intent", ErrPolicy)
	}
	digest, err := IntentSHA256(handle.intent)
	if err != nil {
		return RetentionIntent{}, fmt.Errorf("%w: operation intent cannot be canonically hashed", ErrIntegrity)
	}
	marker := RetentionIntent{
		Schema: RetentionIntentSchemaV1, OperationID: handle.state.OperationID,
		OperationRootIdentity: handle.subtree.Identity().String(), TargetRootIdentity: handle.session.Info().Identity.String(),
		IntentSHA256: digest, TerminalEventID: handle.state.LastEventID, TerminalPhase: handle.state.Phase,
		PlanID: handle.intent.PlanID, MetafileVariantID: handle.intent.MetafileVariantID,
	}
	if handle.state.Phase == PhaseCommitted {
		if options.Meta == nil {
			report.addRetentionBlocker("metafile.required", "committed prune requires the exact metafile for current final verification")
			return RetentionIntent{}, fmt.Errorf("%w: committed prune requires an exact metafile", ErrPolicy)
		}
		layout, layoutErr := BuildLayout(options.Meta, handle.intent.Limits)
		if layoutErr != nil {
			report.addRetentionBlocker("metafile.unsupported_layout", "the selected metafile cannot reproduce the terminal layout")
			return RetentionIntent{}, layoutErr
		}
		if err := validateResumeIntent(options.Meta, layout, handle.intent, options.ExpectedPlanID); err != nil {
			report.addRetentionBlocker("metafile.selector_mismatch", "the selected metafile does not match this terminal operation")
			return RetentionIntent{}, err
		}
		identity, parseErr := fsbind.ParseIdentity(handle.state.FinalIdentity)
		if parseErr != nil || identity.IsZero() {
			return RetentionIntent{}, fmt.Errorf("%w: terminal final identity is invalid", ErrIntegrity)
		}
		marker.Basis = RetentionBasisCommitted
		marker.FinalObjectIdentity = identity.String()
	} else {
		if options.Meta != nil && (options.Meta.MetafileVariantID != handle.intent.MetafileVariantID ||
			options.Meta.InfoHashV1 != handle.intent.InfoHashV1 || options.Meta.InfoHashV2 != handle.intent.InfoHashV2) {
			report.addRetentionBlocker("metafile.selector_mismatch", "the optional metafile does not match this abandoned operation")
			return RetentionIntent{}, fmt.Errorf("%w: metafile differs from abandoned operation", ErrPolicy)
		}
		marker.Basis = RetentionBasisAbandoned
	}
	report.Proof.Basis = marker.Basis
	report.Proof.TerminalEventID = marker.TerminalEventID.String()
	return marker, nil
}

func proveRetentionIntentBasis(ctx context.Context, handle *journal, targetRoot string, options PruneOptions, marker RetentionIntent, report *RetentionReport) error {
	if handle == nil || handle.subtree == nil || report == nil {
		return fmt.Errorf("%w: terminal operation proof authority is unavailable", ErrIntegrity)
	}
	switch marker.Basis {
	case RetentionBasisCommitted:
		layout, err := BuildLayout(options.Meta, handle.intent.Limits)
		if err != nil {
			return err
		}
		identity, err := fsbind.ParseIdentity(marker.FinalObjectIdentity)
		if err != nil || identity.IsZero() {
			return fmt.Errorf("%w: terminal final identity is invalid", ErrIntegrity)
		}
		verifiedBytes, verifyErr := verifyFinalLayout(ctx, options.Meta, layout, targetRoot, handle, identity)
		report.Proof.FinalVerifyBytes = verifiedBytes
		if verifyErr != nil {
			report.addRetentionIssue("final.verification_failed", "the current published final layout failed exact verification")
			return verifyErr
		}
		report.Target.FinalState = "verified_current"
		report.Proof.CurrentFinalVerified = true
		report.Proof.Assurance = "same_invocation_bracketed_non_atomic"
	case RetentionBasisAbandoned:
		finalName, err := finalNameFromIntent(handle.intent)
		if err != nil {
			return err
		}
		if _, inspectErr := handle.session.InspectRoot(ctx, finalName); inspectErr == nil {
			report.addRetentionBlocker("target.final_exists", "the abandoned operation's final name is currently occupied")
			return fmt.Errorf("%w: abandoned final name is occupied", ErrPolicy)
		} else if !errors.Is(inspectErr, fsbind.ErrNotFound) {
			return classifyRetentionAuthorityError(inspectErr)
		}
		report.Target.FinalState = "absent_current"
		report.Proof.CurrentPublicationAbsent = true
		report.Proof.Assurance = "same_invocation_bound_name_observation"
	default:
		return fmt.Errorf("%w: terminal operation proof basis is invalid", ErrIntegrity)
	}
	return nil
}

func applyRetentionMarkerSelection(report *RetentionReport, state retentionMarkerState, options PruneOptions) error {
	if report == nil || !state.IntentPresent {
		return fmt.Errorf("%w: retention intent is unavailable", ErrIntegrity)
	}
	marker := state.Intent
	report.Operation = OperationReport{
		ID: marker.OperationID.String(), Status: "pruning", PhaseBefore: string(marker.TerminalPhase),
		PhaseAfter: string(marker.TerminalPhase), Resumable: true,
	}
	report.Plan.ObservedID = marker.PlanID
	report.Plan.Matches = marker.PlanID == options.ExpectedPlanID
	report.Plan.MetafileVariantID = marker.MetafileVariantID
	report.Target.ExpectedRootIdentity = marker.TargetRootIdentity
	report.Proof = RetentionProofReport{
		Basis: marker.Basis, TerminalEventID: marker.TerminalEventID.String(), HistoricalMarkerAuthority: true,
		Assurance: "historical_exact_observation_bound_to_private_marker",
	}
	report.Markers.State = "intent_published"
	report.Markers.IntentMarkerID = state.IntentID.String()
	report.Markers.OperationStateKept = true
	report.Markers.PruneResumable = !state.CompletePresent
	if !report.Plan.Matches {
		report.addRetentionBlocker("plan.id_mismatch", "the reviewed plan ID does not select this retention marker")
		return fmt.Errorf("%w: reviewed plan ID differs from retention marker", ErrPolicy)
	}
	if options.Meta != nil && options.Meta.MetafileVariantID != marker.MetafileVariantID {
		report.addRetentionBlocker("metafile.selector_mismatch", "the optional metafile differs from the retention marker")
		return fmt.Errorf("%w: metafile differs from retention marker", ErrPolicy)
	}
	return nil
}

func validateRetentionIntentAgainstJournal(marker RetentionIntent, handle *journal) error {
	if handle == nil || handle.subtree == nil {
		return fmt.Errorf("%w: journal authority is unavailable", ErrIntegrity)
	}
	digest, err := IntentSHA256(handle.intent)
	if err != nil || marker.OperationID != handle.state.OperationID || marker.OperationRootIdentity != handle.subtree.Identity().String() ||
		marker.TargetRootIdentity != handle.session.Info().Identity.String() || marker.IntentSHA256 != digest ||
		marker.TerminalEventID != handle.state.LastEventID || marker.TerminalPhase != handle.state.Phase ||
		marker.PlanID != handle.intent.PlanID || marker.MetafileVariantID != handle.intent.MetafileVariantID || !handle.state.Terminal {
		return fmt.Errorf("%w: retention intent disagrees with the terminal journal", ErrIntegrity)
	}
	if marker.TerminalPhase == PhaseCommitted && marker.FinalObjectIdentity != handle.state.FinalIdentity {
		return fmt.Errorf("%w: retention final identity disagrees with the terminal journal", ErrIntegrity)
	}
	return nil
}

func openRetentionSubtree(ctx context.Context, session *fsbind.Session, operationID OperationID) (*fsbind.Subtree, error) {
	directoryName, err := OperationDirectoryName(operationID)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid operation ID", ErrPolicy)
	}
	object, err := session.InspectRoot(ctx, directoryName)
	if errors.Is(err, fsbind.ErrNotFound) {
		return nil, fmt.Errorf("%w: explicit operation does not exist", ErrOperationNotFound)
	}
	if err != nil {
		return nil, classifyRetentionAuthorityError(err)
	}
	if object.Kind != fsbind.ObjectKindDirectory {
		return nil, fmt.Errorf("%w: explicit operation is not a directory", ErrIntegrity)
	}
	subtree, err := session.OpenPrivateSubtreeObserved(directoryName)
	if err != nil {
		return nil, classifyRetentionAuthorityError(err)
	}
	return subtree, nil
}

func retentionHeavyTopNames() []string {
	return []string{intentFileName, journalDirectoryName, scratchDirectoryName, stageDirectoryName}
}

func retentionHeavyNames(ctx context.Context, subtree *fsbind.Subtree, requireAbsent bool) ([]string, error) {
	rootPath, _ := fsbind.PathFromComponents(nil)
	listing, err := subtree.List(ctx, rootPath, fsbind.ListLimits{MaxEntries: 8, MaxNameBytes: 1 << 20})
	if err != nil {
		return nil, classifyRetentionAuthorityError(err)
	}
	if !listing.Complete {
		return nil, fmt.Errorf("%w: operation root inventory is incomplete", ErrIntegrity)
	}
	wanted := map[string]string{
		intentFileName: "regular", journalDirectoryName: "directory",
		scratchDirectoryName: "directory", stageDirectoryName: "directory",
	}
	present := make(map[string]bool, len(wanted))
	retentionPresent := false
	for _, entry := range listing.Entries {
		switch entry.Name {
		case operationLockEntryName:
			if entry.Kind != "regular" {
				return nil, fmt.Errorf("%w: operation lock is unsafe", ErrIntegrity)
			}
		case retentionDirectoryName:
			if entry.Kind != "directory" {
				return nil, fmt.Errorf("%w: retention directory is unsafe", ErrIntegrity)
			}
			retentionPresent = true
		default:
			kind, ok := wanted[entry.Name]
			if !ok || entry.Kind != kind {
				return nil, fmt.Errorf("%w: operation root contains an unexpected object", ErrIntegrity)
			}
			present[entry.Name] = true
		}
	}
	names := make([]string, 0, len(wanted))
	for _, name := range retentionHeavyTopNames() {
		if present[name] {
			if requireAbsent {
				return nil, fmt.Errorf("%w: retained operation still contains heavy state", ErrIntegrity)
			}
			names = append(names, name)
		}
	}
	retentionPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	paths := []fsbind.Path{rootPath}
	if retentionPresent {
		paths = append(paths, retentionPath)
	}
	if err := subtree.CheckPaths(paths...); err != nil {
		return nil, classifyRetentionAuthorityError(err)
	}
	return names, nil
}

func auditRetentionDirectoryBeforeIntent(ctx context.Context, subtree *fsbind.Subtree, marker RetentionIntent) error {
	retentionPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	object, err := subtree.Inspect(ctx, retentionPath)
	if errors.Is(err, fsbind.ErrNotFound) {
		return nil
	}
	if err != nil {
		return classifyRetentionAuthorityError(err)
	}
	if object.Kind != fsbind.ObjectKindDirectory {
		return fmt.Errorf("%w: retention control directory is unsafe", ErrIntegrity)
	}
	raw, id, err := EncodeRetentionIntent(marker)
	if err != nil {
		return err
	}
	pendingName := retentionTemporaryName("intent", id)
	listing, err := subtree.List(ctx, retentionPath, fsbind.ListLimits{MaxEntries: 2, MaxNameBytes: 1 << 20})
	if err != nil {
		return classifyRetentionAuthorityError(err)
	}
	if !listing.Complete || len(listing.Entries) > 1 {
		return fmt.Errorf("%w: unsealed retention directory is not empty", ErrIntegrity)
	}
	if len(listing.Entries) == 1 {
		entry := listing.Entries[0]
		if entry.Name != pendingName || entry.Kind != "regular" {
			return fmt.Errorf("%w: unsealed retention directory contains an unexpected object", ErrIntegrity)
		}
		pendingPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, pendingName})
		if _, err := verifyRetentionNamedBytes(ctx, subtree, pendingPath, raw); err != nil {
			return err
		}
	}
	return subtree.CheckPaths(retentionPath)
}

func finalNameFromIntent(intent Intent) (string, error) {
	if len(intent.FinalRawComponentsBase64) != 1 {
		return "", fmt.Errorf("%w: terminal final name is invalid", ErrIntegrity)
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(intent.FinalRawComponentsBase64[0])
	if err != nil || !utf8.Valid(raw) || base64.StdEncoding.EncodeToString(raw) != intent.FinalRawComponentsBase64[0] {
		return "", fmt.Errorf("%w: terminal final name is invalid", ErrIntegrity)
	}
	name := string(raw)
	if err := fsbind.ValidatePathComponents([]string{name}); err != nil {
		return "", fmt.Errorf("%w: terminal final name is unsafe", ErrIntegrity)
	}
	return name, nil
}

func confirmRetentionMarkerDurability(ctx context.Context, subtree *fsbind.Subtree) error {
	retentionPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	rootPath, _ := fsbind.PathFromComponents(nil)
	if err := subtree.SyncDirectory(ctx, retentionPath); err != nil {
		return err
	}
	if err := subtree.SyncDirectory(ctx, rootPath); err != nil {
		return err
	}
	return subtree.CheckPaths(rootPath, retentionPath)
}

func (report *RetentionReport) recordRetentionMarker(receipt retentionMarkerReceipt) {
	if report == nil {
		return
	}
	if receipt.DirectoryCreated {
		report.WritesPerformed++
		report.Writes.ControlDirectoriesCreated++
		if receipt.DirectoryDurability != fsbind.DurabilityConfirmed {
			report.WritesUncertain = true
		}
	}
	if receipt.TemporaryCreated {
		report.WritesPerformed++
		report.Writes.MarkerTemporaryFiles++
		report.Writes.MarkerTemporaryBytes += receipt.TemporaryBytesWritten
	}
	if receipt.Publication.Attempted {
		report.Writes.MarkerPublicationAttempts++
	}
	if receipt.Publication.Published {
		report.WritesPerformed++
		report.Writes.MarkerPublications++
		if receipt.Publication.Durability == fsbind.DurabilityConfirmed {
			report.Writes.MarkerDurabilityConfirms++
		} else {
			report.WritesUncertain = true
		}
	}
	if receipt.TemporaryRemoval.Removed {
		report.WritesPerformed++
		report.Writes.MarkerTemporaryRemovals++
		if receipt.TemporaryRemoval.Durability != fsbind.DurabilityConfirmed {
			report.WritesUncertain = true
		}
	}
}

func (report *RetentionReport) recordRetentionRemovals(usage RetentionUsage) {
	if report == nil {
		return
	}
	report.Used = usage
	report.Writes.RemovalAttempts = usage.RemovalAttempts
	report.Writes.FilesRemoved = usage.FilesRemoved
	report.Writes.DirectoriesRemoved = usage.DirectoriesRemoved
	report.Writes.BytesRemoved = usage.BytesRemoved
	report.Writes.AmbiguousRemovals = usage.AmbiguousRemovals
	report.WritesPerformed += usage.FilesRemoved + usage.DirectoriesRemoved
	if usage.AmbiguousRemovals != 0 {
		report.WritesUncertain = true
	}
}

func (report *RetentionReport) addRetentionBlocker(code, message string) {
	report.addRetentionFinding(&report.Blockers, code, message)
}

func (report *RetentionReport) addRetentionIssue(code, message string) {
	report.addRetentionFinding(&report.Issues, code, message)
}

func (report *RetentionReport) addRetentionFinding(destination *[]Finding, code, message string) {
	if report == nil || destination == nil {
		return
	}
	limit := report.Limits.MaxFindings
	// Invalid caller limits must block the operation, but they must not also
	// suppress the fixed diagnostic explaining that zero-write decision.
	if limit <= 0 || limit > hardRetentionMaxFindings {
		limit = defaultRetentionMaxFindings
	}
	if len(*destination) >= limit {
		return
	}
	*destination = append(*destination, Finding{Code: code, Message: message})
}

func classifyRetentionFailure(report *RetentionReport, err error) {
	if report == nil || err == nil {
		return
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		report.Outcome = RetentionOutcomeInterrupted
		report.addRetentionIssue("prune.interrupted", "materialize operation pruning was canceled")
	case errors.Is(err, ErrPolicy), errors.Is(err, ErrOperationNotFound), errors.Is(err, fsbind.ErrUnsupported), errors.Is(err, fsbind.ErrBusy):
		report.Outcome = RetentionOutcomeBlocked
		report.addRetentionBlocker("prune.policy_blocked", "the explicit operation cannot be pruned under the current selector or safety policy")
	case errors.Is(err, ErrIntegrity), errors.Is(err, ErrCorruptJournal), errors.Is(err, fsbind.ErrNotFound),
		errors.Is(err, fsbind.ErrUnsafeObject), errors.Is(err, fsbind.ErrBindingChanged), errors.Is(err, fsbind.ErrCrossFilesystem):
		report.Outcome = RetentionOutcomeIntegrityFailed
		report.Operation.Resumable = false
		report.Markers.PruneResumable = false
		report.addRetentionIssue("prune.integrity_failed", "operation, marker, final proof, or private namespace integrity failed")
	default:
		report.Outcome = RetentionOutcomeInterrupted
		report.addRetentionIssue("prune.operation_failed", "materialize operation pruning could not be completed")
	}
	if errors.Is(err, fsbind.ErrPublicationAmbiguous) || errors.Is(err, fsbind.ErrRemovalAmbiguous) || errors.Is(err, fsbind.ErrDurabilityUnconfirmed) {
		report.WritesUncertain = true
	}
}

func controlReportFromRetention(ctx context.Context, session *fsbind.Session, rootInfo fsbind.RootInfo, operationID OperationID, limits Limits) (Report, bool, error) {
	report := newReport("", "", limits)
	report.Effect = []string{"read_private_materialize_retention_state"}
	setOperationInspectionSelector(&report, operationID)
	report.Source = SourceReport{Mode: "not_requested", Outcome: "not_requested"}
	report.Target.RootIdentityBound = true
	report.Target.ObservedRootIdentity = rootInfo.Identity.String()
	report.Target.StabilityAssurance = "historical_retention_marker_only"
	subtree, err := openRetentionSubtree(ctx, session, operationID)
	if err != nil {
		return report, false, err
	}
	defer subtree.Close()
	state, err := loadRetentionMarkerState(ctx, subtree, operationID, rootInfo.Identity)
	if err != nil {
		if state.DirectoryPresent {
			report.Outcome = OutcomeIntegrityFailed
			report.Operation.Status = "retention_inspection_failed"
			report.Operation.Resumable = false
			report.addIssue("retention.integrity_failed", "the private retention marker could not be safely rebound", nil)
			return report, true, err
		}
		return report, false, err
	}
	if !state.IntentPresent {
		if state.DirectoryPresent {
			report.Outcome = OutcomePruning
			report.Operation.Status = "retention_initializing"
			report.Operation.Resumable = false
			report.addBlocker("operation.prune_required", "the reserved retention boundary exists but its durable intent is not yet observable; only explicit materialize prune may recover it")
			return report, true, nil
		}
		return report, false, nil
	}
	marker := state.Intent
	report.Operation = OperationReport{
		ID: operationID.String(), Status: "pruning", PhaseBefore: string(marker.TerminalPhase),
		PhaseAfter: string(marker.TerminalPhase), Resumable: false,
	}
	report.Plan.ObservedID = marker.PlanID
	report.Plan.MetafileVariantID = marker.MetafileVariantID
	report.Target.ExpectedRootIdentity = marker.TargetRootIdentity
	if marker.TerminalPhase == PhaseCommitted {
		report.Target.Publication = "historical_committed_prune_basis"
	} else {
		report.Target.Publication = "historical_unpublished_prune_basis"
	}
	if state.CompletePresent {
		if _, err := retentionHeavyNames(ctx, subtree, true); err != nil {
			report.Outcome = OutcomeIntegrityFailed
			report.Operation.Status = "retention_inspection_failed"
			report.addIssue("retention.tombstone_invalid", "the completed retention tombstone contains unexpected heavy state", nil)
			return report, true, err
		}
		if err := auditRetentionControlDirectory(ctx, subtree, state, false); err != nil {
			report.Outcome = OutcomeIntegrityFailed
			report.Operation.Status = "retention_inspection_failed"
			report.addIssue("retention.tombstone_invalid", "the completed retention marker namespace is not exact", nil)
			return report, true, err
		}
		report.Outcome = OutcomeRetained
		report.Operation.Status = "retained"
		report.Warnings = append(report.Warnings, "heavy private operation state was pruned; a small tombstone is intentionally retained")
		return report, true, nil
	}
	if _, err := retentionHeavyNames(ctx, subtree, false); err != nil {
		report.Outcome = OutcomeIntegrityFailed
		report.Operation.Status = "retention_inspection_failed"
		report.addIssue("retention.namespace_invalid", "the in-progress retention namespace could not be safely inventoried", nil)
		return report, true, err
	}
	report.Outcome = OutcomePruning
	report.addBlocker("operation.prune_required", "retention intent is durable; only explicit materialize prune may continue this operation")
	return report, true, nil
}
