package sourceretire

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

var executionRetentionTransitionHook func(string) error

// PruneExecution removes the bounded, owner-private journal of one explicitly
// selected terminal source-retirement operation. It never selects a latest
// operation and never removes source names, the final layout, or client state.
func PruneExecution(ctx context.Context, options ExecutionPruneOptions) (ExecutionRetentionReport, error) {
	report := newExecutionRetentionReport(options)
	if err := options.JournalLimits.Validate(); err != nil {
		return executionRetentionBlocked(&report, "limits.journal_invalid", "source retirement journal limits are invalid")
	}
	if err := options.RetentionLimits.Validate(); err != nil {
		return executionRetentionBlocked(&report, "limits.retention_invalid", "source retirement retention limits are invalid")
	}
	if !options.Acknowledge {
		return executionRetentionBlocked(&report, "acknowledgement.required", "private operation-state deletion requires its dedicated acknowledgement")
	}
	if _, err := ParseOperationID(options.OperationID.String()); err != nil || !canonicalSHA256ID(options.ExpectedPlanID) || options.TargetRoot == "" {
		return executionRetentionBlocked(&report, "prune.selector_invalid", "target root, operation ID, or reviewed plan ID is invalid")
	}
	if err := ctx.Err(); err != nil {
		return executionRetentionInterrupted(&report, err, "source retirement pruning was interrupted before target observation")
	}
	absolute, err := filepath.Abs(options.TargetRoot)
	if err != nil {
		return executionRetentionBlocked(&report, "target.invalid_root", "the source retirement target root is invalid")
	}
	target, rootInfo, err := fsbind.BindExisting(filepath.Clean(absolute))
	if err != nil {
		return mapExecutionRetentionError(&report, err, "the target root cannot provide bound retention semantics")
	}
	defer target.Close()
	report.Target.ObservedRootIdentity = rootInfo.Identity.String()
	report.Target.RootIdentityBound = true
	report.Target.StabilityAssurance = "non_atomic_bound_filesystem"

	handle, journalErr := openExecutionJournalForRetention(ctx, target, options.OperationID, options.JournalLimits)
	var subtree *fsbind.Subtree
	if handle != nil {
		subtree = handle.subtree
	} else {
		subtree, err = openExecutionRetentionSubtree(target, options.OperationID)
		if err != nil {
			return mapExecutionRetentionError(&report, err, "the explicit source retirement operation could not be opened")
		}
	}
	defer subtree.Close()

	state, err := loadExecutionRetentionState(ctx, subtree, options.OperationID, rootInfo.Identity)
	if err != nil {
		return mapExecutionRetentionError(&report, err, "source retirement retention state could not be loaded")
	}
	if handle == nil && !state.IntentPresent {
		if journalErr == nil {
			journalErr = fmt.Errorf("%w: terminal journal authority is unavailable", ErrExecutionIntegrity)
		}
		return mapExecutionRetentionError(&report, journalErr, "terminal source retirement journal authority is unavailable")
	}

	var marker ExecutionRetentionIntent
	if state.IntentPresent {
		marker = state.Intent
		if err := selectExecutionRetentionMarker(&report, marker, options, rootInfo.Identity, subtree.Identity()); err != nil {
			return mapExecutionRetentionError(&report, err, "the source retirement retention marker does not match the explicit selector")
		}
		if handle != nil {
			if err := validateExecutionRetentionAgainstJournal(marker, handle); err != nil {
				return mapExecutionRetentionError(&report, err, "the source retirement retention marker disagrees with its terminal journal")
			}
		}
		if err := confirmExecutionRetentionDurability(ctx, subtree); err != nil {
			return mapExecutionRetentionError(&report, err, "the source retirement retention intent durability could not be confirmed")
		}
		report.Markers.IntentDurable = true
		intentID, receipt, ensureErr := ensureExecutionRetentionIntent(ctx, subtree, marker)
		report.recordExecutionRetentionMarker(receipt)
		if ensureErr != nil {
			return mapExecutionRetentionError(&report, ensureErr, "the source retirement retention intent could not be re-established")
		}
		state.IntentID = intentID
		if state.CompletePresent {
			completeID, completeReceipt, completeErr := ensureExecutionRetentionComplete(ctx, subtree, state.Complete)
			report.recordExecutionRetentionMarker(completeReceipt)
			if completeErr != nil {
				return mapExecutionRetentionError(&report, completeErr, "the source retirement retention completion could not be re-established")
			}
			state.CompleteID = completeID
			if err := verifyExecutionRetentionTombstone(ctx, subtree, options.OperationID, rootInfo.Identity); err != nil {
				return mapExecutionRetentionError(&report, err, "the source retirement retention tombstone is not exact")
			}
			report.Markers.State = "complete"
			report.Markers.IntentMarkerID = state.IntentID.String()
			report.Markers.CompleteMarkerID = state.CompleteID.String()
			report.Markers.IntentDurable = true
			report.Markers.CompletionDurable = true
			report.Markers.ExactTombstone = true
			report.Markers.PruneResumable = false
			report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "retained", "retained", false
			if report.WritesPerformed == 0 {
				report.Outcome = ExecutionRetentionOutcomeAlreadyPruned
			} else {
				report.Outcome = ExecutionRetentionOutcomePruned
			}
			return report, nil
		}
		fresh, loadErr := loadExecutionRetentionState(ctx, subtree, options.OperationID, rootInfo.Identity)
		if loadErr != nil {
			return mapExecutionRetentionError(&report, loadErr, "source retirement retention intent could not be reloaded")
		}
		if err := auditExecutionRetentionControl(ctx, subtree, fresh, true); err != nil {
			return mapExecutionRetentionError(&report, err, "source retirement retention control namespace is invalid")
		}
		state = fresh
	} else {
		if handle == nil || !handle.state.CompletePresent {
			report.addExecutionRetentionBlocker("operation.not_terminal", "only a source retirement operation with a durable completion may be pruned")
			return mapExecutionRetentionError(&report, fmt.Errorf("%w: source retirement operation is not terminal", ErrExecutionPolicy), "the source retirement operation is not terminal")
		}
		marker, err = prepareExecutionRetentionIntent(handle, options, rootInfo.Identity, &report)
		if err != nil {
			return mapExecutionRetentionError(&report, err, "the terminal source retirement journal could not authorize pruning")
		}
		if err := auditExecutionRetentionBeforeIntent(ctx, subtree, state, marker); err != nil {
			return mapExecutionRetentionError(&report, err, "the source retirement retention boundary is not clean")
		}
	}

	nodes, usage, inventoryErr := inventoryExecutionRetentionState(ctx, subtree, marker, options.RetentionLimits, !state.IntentPresent)
	report.Used = usage
	if inventoryErr != nil {
		return mapExecutionRetentionError(&report, inventoryErr, "source retirement private state could not be completely inventoried")
	}
	if !state.IntentPresent {
		intentID, receipt, publishErr := ensureExecutionRetentionIntent(ctx, subtree, marker)
		report.recordExecutionRetentionMarker(receipt)
		if publishErr != nil {
			if intentID != "" && (receipt.AlreadyPresent || receipt.Publication.Published) {
				report.Markers.State = "intent_publication_observed_unverified"
				report.Markers.IntentMarkerID = intentID.String()
			} else if receipt.Publication.Attempted {
				report.Markers.State = "intent_publication_ambiguous"
			}
			return mapExecutionRetentionError(&report, publishErr, "source retirement retention intent publication failed")
		}
		state = executionRetentionState{DirectoryPresent: true, IntentPresent: true, Intent: marker, IntentID: intentID, Entries: []fsbind.Entry{}}
		fresh, loadErr := loadExecutionRetentionState(ctx, subtree, options.OperationID, rootInfo.Identity)
		if loadErr != nil {
			return mapExecutionRetentionError(&report, loadErr, "source retirement retention intent could not be reloaded")
		}
		state = fresh
		if err := auditExecutionRetentionControl(ctx, subtree, state, true); err != nil {
			return mapExecutionRetentionError(&report, err, "source retirement retention control namespace is invalid")
		}
		report.Markers.State = "intent_published"
		report.Markers.IntentMarkerID = state.IntentID.String()
		report.Markers.IntentDurable = true
		report.Markers.PruneResumable = true
		if executionRetentionTransitionHook != nil {
			if hookErr := executionRetentionTransitionHook("intent_published"); hookErr != nil {
				return mapExecutionRetentionError(&report, hookErr, "source retirement pruning stopped after its durable intent")
			}
		}
	}
	report.Markers.State = "intent_published"
	report.Markers.IntentMarkerID = state.IntentID.String()
	report.Markers.IntentDurable = true
	report.Markers.PruneResumable = true

	removed, removeErr := removeExecutionRetentionState(ctx, subtree, nodes, report.Used)
	report.recordExecutionRetentionRemovals(removed)
	if removeErr != nil {
		return mapExecutionRetentionError(&report, removeErr, "source retirement private state deletion was interrupted")
	}
	if err := auditExecutionRetentionHeavyAbsent(ctx, subtree); err != nil {
		return mapExecutionRetentionError(&report, err, "source retirement private state remains after pruning")
	}
	if executionRetentionTransitionHook != nil {
		if hookErr := executionRetentionTransitionHook("heavy_state_removed"); hookErr != nil {
			return mapExecutionRetentionError(&report, hookErr, "source retirement pruning stopped after private state removal")
		}
	}
	complete := ExecutionRetentionComplete{
		Schema: ExecutionRetentionCompleteSchemaV1, OperationID: options.OperationID,
		OperationRootIdentity: subtree.Identity().String(), TargetRootIdentity: rootInfo.Identity.String(),
		IntentMarkerID: state.IntentID,
	}
	completeID, receipt, completeErr := ensureExecutionRetentionComplete(ctx, subtree, complete)
	report.recordExecutionRetentionMarker(receipt)
	if completeErr != nil {
		if completeID != "" && (receipt.AlreadyPresent || receipt.Publication.Published) {
			report.Markers.State = "completion_publication_observed_unverified"
			report.Markers.CompleteMarkerID = completeID.String()
		} else if receipt.Publication.Attempted {
			report.Markers.State = "completion_publication_ambiguous"
		}
		return mapExecutionRetentionError(&report, completeErr, "source retirement retention completion publication failed")
	}
	if err := verifyExecutionRetentionTombstone(ctx, subtree, options.OperationID, rootInfo.Identity); err != nil {
		return mapExecutionRetentionError(&report, err, "source retirement retention tombstone could not be reverified")
	}
	report.Markers.State = "complete"
	report.Markers.CompleteMarkerID = completeID.String()
	report.Markers.CompletionDurable = true
	report.Markers.ExactTombstone = true
	report.Markers.PruneResumable = false
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "retained", "retained", false
	report.Outcome = ExecutionRetentionOutcomePruned
	return report, nil
}

func prepareExecutionRetentionIntent(handle *executionJournal, options ExecutionPruneOptions, targetIdentity fsbind.Identity, report *ExecutionRetentionReport) (ExecutionRetentionIntent, error) {
	if handle == nil || handle.subtree == nil || !handle.state.CompletePresent {
		return ExecutionRetentionIntent{}, fmt.Errorf("%w: terminal source retirement authority is unavailable", ErrExecutionIntegrity)
	}
	intent, complete := handle.state.Intent, handle.state.Complete
	if intent.PlanID != options.ExpectedPlanID {
		report.addExecutionRetentionBlocker("plan.id_mismatch", "the reviewed plan ID does not select this terminal source retirement operation")
		return ExecutionRetentionIntent{}, fmt.Errorf("%w: reviewed plan ID differs from source retirement journal", ErrExecutionPolicy)
	}
	marker := ExecutionRetentionIntent{
		Schema: ExecutionRetentionIntentSchemaV1, OperationID: intent.OperationID,
		OperationRootIdentity: handle.subtree.Identity().String(), TargetRootIdentity: targetIdentity.String(),
		PlanID: intent.PlanID, IntentID: handle.state.IntentID, CompletionID: handle.state.CompleteID,
		Basis: ExecutionRetentionBasisComplete, SearchScopeID: intent.SearchScopeID,
		MetafileVariantID: intent.MetafileVariantID, MaterializeOperationID: intent.MaterializeOperationID,
		MaterializePlanID: intent.MaterializePlanID, ActivationOperationID: intent.ActivationOperationID,
		ActivationPlanID: intent.ActivationPlanID, ClientCompletionID: intent.ClientCompletionID,
		CurrentClientUseID: intent.CurrentClientUseID, SourceSelectionID: intent.SourceSelectionID,
		FilesRetired: complete.FilesRetired, BytesRetired: complete.BytesRetired,
		FinalObjectIdentity: complete.FinalObjectIdentity, ClientSnapshotID: complete.ClientSnapshotID,
	}
	if err := marker.Validate(); err != nil {
		return ExecutionRetentionIntent{}, err
	}
	_, intentMarkerID, err := EncodeExecutionRetentionIntent(marker)
	if err != nil {
		return ExecutionRetentionIntent{}, err
	}
	if _, _, err := EncodeExecutionRetentionComplete(ExecutionRetentionComplete{
		Schema: ExecutionRetentionCompleteSchemaV1, OperationID: marker.OperationID,
		OperationRootIdentity: marker.OperationRootIdentity, TargetRootIdentity: marker.TargetRootIdentity,
		IntentMarkerID: intentMarkerID,
	}); err != nil {
		return ExecutionRetentionIntent{}, err
	}
	selectExecutionRetentionMarker(report, marker, options, targetIdentity, handle.subtree.Identity())
	return marker, nil
}

func selectExecutionRetentionMarker(report *ExecutionRetentionReport, marker ExecutionRetentionIntent, options ExecutionPruneOptions, targetIdentity, operationIdentity fsbind.Identity) error {
	if err := marker.Validate(); err != nil || marker.OperationID != options.OperationID || marker.PlanID != options.ExpectedPlanID ||
		marker.TargetRootIdentity != targetIdentity.String() || marker.OperationRootIdentity != operationIdentity.String() {
		return fmt.Errorf("%w: source retirement retention selector disagrees", ErrExecutionPolicy)
	}
	report.Operation = ExecutionOperationReport{ID: marker.OperationID.String(), PlanID: marker.PlanID, Status: "pruning", Phase: "complete", Resumable: false,
		IntentID: marker.IntentID, CompletionID: marker.CompletionID}
	report.Target.ExpectedRootIdentity = marker.TargetRootIdentity
	report.Proof = ExecutionRetentionProofReport{
		Basis: marker.Basis, IntentID: marker.IntentID, TerminalCompletionID: marker.CompletionID,
		FilesRetired: marker.FilesRetired, BytesRetired: marker.BytesRetired, HistoricalAuthority: true,
		Assurance: "historical_exact_terminal_journal_bound_to_private_retention_marker",
	}
	return nil
}

func validateExecutionRetentionAgainstJournal(marker ExecutionRetentionIntent, handle *executionJournal) error {
	if handle == nil || !handle.state.CompletePresent {
		return fmt.Errorf("%w: terminal source retirement journal is unavailable", ErrExecutionIntegrity)
	}
	expected, err := prepareExecutionRetentionIntent(handle, ExecutionPruneOptions{OperationID: marker.OperationID, ExpectedPlanID: marker.PlanID}, handle.target.Info().Identity, &ExecutionRetentionReport{})
	if err != nil || expected != marker {
		return fmt.Errorf("%w: source retirement retention intent disagrees with its terminal journal", ErrExecutionIntegrity)
	}
	return nil
}

func openExecutionRetentionSubtree(target *fsbind.Session, operation OperationID) (*fsbind.Subtree, error) {
	name, err := OperationDirectoryName(operation)
	if err != nil {
		return nil, err
	}
	subtree, err := target.OpenPrivateSubtreeObserved(name)
	if errors.Is(err, fsbind.ErrNotFound) {
		return nil, ErrOperationNotFound
	}
	return subtree, err
}

func auditExecutionRetentionBeforeIntent(ctx context.Context, subtree *fsbind.Subtree, state executionRetentionState, marker ExecutionRetentionIntent) error {
	if !state.DirectoryPresent {
		return nil
	}
	raw, id, err := EncodeExecutionRetentionIntent(marker)
	if err != nil {
		return err
	}
	expectedPending := executionRetentionTemporaryName("intent", id)
	if len(state.Entries) > 1 {
		return fmt.Errorf("%w: unsealed source retirement retention directory is not empty", ErrExecutionIntegrity)
	}
	if len(state.Entries) == 1 {
		entry := state.Entries[0]
		if entry.Name != expectedPending || entry.Kind != string(fsbind.ObjectKindRegular) {
			return fmt.Errorf("%w: unsealed source retirement retention directory contains an unexpected object", ErrExecutionIntegrity)
		}
		pendingPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory, expectedPending})
		if _, err := verifyExecutionRetentionNamedBytes(ctx, subtree, pendingPath, raw); err != nil {
			return err
		}
	}
	retentionPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory})
	return subtree.CheckPaths(retentionPath)
}

func auditExecutionRetentionHeavyAbsent(ctx context.Context, subtree *fsbind.Subtree) error {
	root, err := subtree.List(ctx, fsbind.Path{}, fsbind.ListLimits{MaxEntries: 3, MaxNameBytes: 1 << 20})
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	if !root.Complete || len(root.Entries) != 2 {
		return fmt.Errorf("%w: pruned source retirement operation namespace is not exact", ErrExecutionIntegrity)
	}
	seenLock, seenRetention := false, false
	for _, entry := range root.Entries {
		switch entry.Name {
		case ".fsbind-operation.lock":
			seenLock = entry.Kind == string(fsbind.ObjectKindRegular)
		case executionRetentionDirectory:
			seenRetention = entry.Kind == string(fsbind.ObjectKindDirectory)
		}
	}
	if !seenLock || !seenRetention {
		return fmt.Errorf("%w: pruned source retirement operation namespace is invalid", ErrExecutionIntegrity)
	}
	retentionPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory})
	return subtree.CheckPaths(fsbind.Path{}, retentionPath)
}

func verifyExecutionRetentionTombstone(ctx context.Context, subtree *fsbind.Subtree, operation OperationID, targetIdentity fsbind.Identity) error {
	state, err := loadExecutionRetentionState(ctx, subtree, operation, targetIdentity)
	if err != nil {
		return err
	}
	if !state.IntentPresent || !state.CompletePresent {
		return fmt.Errorf("%w: source retirement retention tombstone is incomplete", ErrExecutionIntegrity)
	}
	if err := auditExecutionRetentionHeavyAbsent(ctx, subtree); err != nil {
		return err
	}
	if err := auditExecutionRetentionControl(ctx, subtree, state, false); err != nil {
		return err
	}
	return auditExecutionRetentionHeavyAbsent(ctx, subtree)
}

// applyExecutionRetentionControl lets ordinary run/resume/status recognize the
// durable prune boundary without crossing it. Only PruneExecution may advance
// an intent-only state; ordinary commands remain read-only here.
func applyExecutionRetentionControl(ctx context.Context, target *fsbind.Session, operation OperationID, report *ExecutionReport) (bool, error) {
	if target == nil || report == nil {
		return false, fmt.Errorf("%w: source retirement retention inspection authority is unavailable", ErrExecutionIntegrity)
	}
	subtree, err := openExecutionRetentionSubtree(target, operation)
	if errors.Is(err, ErrOperationNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer subtree.Close()
	state, err := loadExecutionRetentionState(ctx, subtree, operation, target.Info().Identity)
	if err != nil {
		return state.DirectoryPresent, err
	}
	if !state.DirectoryPresent {
		return false, nil
	}
	report.addEffect("read_private_source_retirement_retention_state")
	report.Files = []ExecutionFileReport{}
	report.Operation.ID = operation.String()
	report.Operation.Resumable = false
	if !state.IntentPresent {
		if err := auditExecutionRetentionControl(ctx, subtree, state, true); err != nil {
			return true, err
		}
		report.Outcome = ExecutionOutcomeIncomplete
		report.Operation.Status, report.Operation.Phase = "retention_initializing", "retention_initializing"
		report.addBlocker("operation.prune_required", "a reserved retention boundary exists; only explicit source retirement prune may recover it")
		report.finalize()
		return true, nil
	}
	marker := state.Intent
	report.Operation.PlanID = marker.PlanID
	report.Operation.IntentID = marker.IntentID
	report.Operation.CompletionID = marker.CompletionID
	report.Used.FilesConsidered = marker.FilesRetired
	report.Used.ContentBytes = marker.BytesRetired
	report.Warnings = append(report.Warnings, "heavy source-retirement journal state is being pruned or has been replaced by a historical tombstone")
	if state.CompletePresent {
		if err := verifyExecutionRetentionTombstone(ctx, subtree, operation, target.Info().Identity); err != nil {
			return true, err
		}
		report.Outcome = ExecutionOutcomeAlreadyRetired
		report.Operation.Status, report.Operation.Phase = "retained", "retained"
		report.Warnings = append(report.Warnings, "retained status is historical only and does not prove that a retired source name remains absent now")
		report.finalize()
		return true, nil
	}
	if err := auditExecutionRetentionControl(ctx, subtree, state, true); err != nil {
		return true, err
	}
	report.Outcome = ExecutionOutcomePartial
	report.Operation.Status, report.Operation.Phase = "pruning", "retention_intent_recorded"
	report.addBlocker("operation.prune_required", "retention intent is durable; only explicit source retirement prune may continue deletion of private operation state")
	report.finalize()
	return true, nil
}

func (report *ExecutionRetentionReport) recordExecutionRetentionMarker(receipt executionRetentionMarkerReceipt) {
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
		if receipt.Publication.Durability != fsbind.DurabilityConfirmed {
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

func (report *ExecutionRetentionReport) recordExecutionRetentionRemovals(usage ExecutionRetentionUsage) {
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

func (report *ExecutionRetentionReport) addExecutionRetentionBlocker(code, message string) {
	report.addExecutionRetentionFinding(&report.Blockers, code, message)
}

func (report *ExecutionRetentionReport) addExecutionRetentionIssue(code, message string) {
	report.addExecutionRetentionFinding(&report.Issues, code, message)
}

func (report *ExecutionRetentionReport) addExecutionRetentionFinding(destination *[]Finding, code, message string) {
	limit := report.Limits.MaxFindings
	if limit <= 0 || limit > hardExecutionRetentionMaxFindings {
		limit = defaultExecutionRetentionMaxFindings
	}
	if len(*destination) < limit {
		*destination = append(*destination, Finding{Code: code, Message: message})
	}
}

func executionRetentionBlocked(report *ExecutionRetentionReport, code, message string) (ExecutionRetentionReport, error) {
	report.Outcome = ExecutionRetentionOutcomeBlocked
	report.addExecutionRetentionBlocker(code, message)
	return *report, nil
}

func executionRetentionInterrupted(report *ExecutionRetentionReport, err error, message string) (ExecutionRetentionReport, error) {
	report.Outcome = ExecutionRetentionOutcomeInterrupted
	report.addExecutionRetentionIssue("prune.interrupted", message)
	return *report, err
}

func mapExecutionRetentionError(report *ExecutionRetentionReport, err error, message string) (ExecutionRetentionReport, error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return executionRetentionInterrupted(report, err, message)
	case errors.Is(err, ErrExecutionPolicy), errors.Is(err, ErrOperationNotFound), errors.Is(err, fsbind.ErrUnsupported), errors.Is(err, fsbind.ErrBusy):
		report.Outcome = ExecutionRetentionOutcomeBlocked
		report.addExecutionRetentionBlocker("prune.policy_blocked", message)
		return *report, err
	case errors.Is(err, ErrExecutionIntegrity), errors.Is(err, fsbind.ErrNotFound), errors.Is(err, fsbind.ErrUnsafeObject),
		errors.Is(err, fsbind.ErrBindingChanged), errors.Is(err, fsbind.ErrCrossFilesystem):
		report.Outcome = ExecutionRetentionOutcomeIntegrityFailed
		report.Operation.Resumable = false
		report.Markers.PruneResumable = false
		report.addExecutionRetentionIssue("prune.integrity_failed", message)
		return *report, fmt.Errorf("%w: %s", ErrExecutionIntegrity, message)
	default:
		report.Outcome = ExecutionRetentionOutcomeInterrupted
		report.addExecutionRetentionIssue("prune.operation_failed", message)
		if errors.Is(err, fsbind.ErrPublicationAmbiguous) || errors.Is(err, fsbind.ErrRemovalAmbiguous) || errors.Is(err, fsbind.ErrDurabilityUnconfirmed) {
			report.WritesUncertain = true
		}
		return *report, err
	}
}
