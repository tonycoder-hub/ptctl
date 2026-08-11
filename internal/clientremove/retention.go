package clientremove

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

var retentionTransitionHook func(string) error

// Prune replaces one explicitly selected terminal removal journal with a
// small exact tombstone. It never contacts the downloader and never modifies
// the materialized final, metafile store, or another operation.
func Prune(ctx context.Context, options PruneOptions) (RetentionReport, error) {
	report := newRetentionReport(options)
	finish := func(err error, message string) (RetentionReport, error) {
		mapRetentionError(&report, err, message)
		finalizeRetentionReport(&report)
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return finish(err, "client removal pruning was cancelled before observation")
	}
	if options.TargetRoot == "" || options.Limits.Validate() != nil {
		report.addRetentionBlocker("limits.or_selector_invalid", "client removal prune requires a target and valid hard limits")
		return finish(fmt.Errorf("%w: client removal prune input is invalid", ErrPolicy), "client removal prune input is invalid")
	}
	if parsed, err := ParseOperationID(options.OperationID.String()); err != nil || parsed != options.OperationID || !canonicalPlanID(options.ExpectedPlanID) {
		report.addRetentionBlocker("operation.selector_invalid", "client removal prune requires canonical operation and plan IDs")
		return finish(fmt.Errorf("%w: client removal prune selector is invalid", ErrPolicy), "client removal prune selector is invalid")
	}
	if !options.Acknowledge {
		report.addRetentionBlocker("acknowledgement.operation_state_deletion_required", "client removal prune requires explicit acknowledgement of private operation-state deletion")
		return finish(fmt.Errorf("%w: client removal prune acknowledgement is required", ErrPolicy), "client removal prune acknowledgement is required")
	}
	absolute, err := filepath.Abs(options.TargetRoot)
	if err != nil {
		return finish(fmt.Errorf("%w: target root is invalid", ErrPolicy), "the target root is invalid")
	}
	target, rootInfo, err := fsbind.BindExisting(filepath.Clean(absolute))
	if err != nil {
		return finish(err, "the target root cannot provide bound retention semantics")
	}
	defer target.Close()
	report.Target.ObservedRootIdentity = rootInfo.Identity.String()
	report.Target.RootIdentityBound = true
	report.Target.StabilityAssurance = "non_atomic_bound_filesystem"
	if pending, inspectErr := inspectForgetControl(ctx, target, options.OperationID, options.ExpectedPlanID); inspectErr != nil {
		return finish(inspectErr, "the client removal forget boundary could not be inspected")
	} else if pending != nil {
		return finish(pending, "client removal historical evidence deletion is already in progress")
	}
	directoryName, _ := operationDirectoryName(options.OperationID)
	object, err := target.InspectRoot(ctx, directoryName)
	if errors.Is(err, fsbind.ErrNotFound) {
		return finish(ErrOperationNotFound, "the explicit client removal operation does not exist")
	}
	if err != nil || object.Kind != fsbind.ObjectKindDirectory {
		if err == nil {
			err = ErrIntegrity
		}
		return finish(classifyJournalError(err), "the client removal operation directory is unsafe")
	}
	subtree, err := target.OpenPrivateSubtreeObserved(directoryName)
	if err != nil {
		return finish(classifyJournalError(err), "the client removal operation could not be bound")
	}
	handle := &journalHandle{session: target, subtree: subtree}
	defer subtree.Close()

	state, err := loadRetentionState(ctx, handle, options.OperationID, rootInfo.Identity)
	if err != nil {
		return finish(err, "client removal retention state could not be loaded")
	}
	if state.ForgetPending {
		return finish(&forgetInProgressError{marker: state.ForgetIntent, markerID: state.ForgetID}, "client removal historical evidence deletion is already staged")
	}
	var marker RetentionIntent
	var live journalState
	needLive := !state.IntentPresent
	if needLive {
		live, err = handle.readStateForRetention(ctx, options.OperationID)
		if err != nil {
			return finish(err, "terminal client removal journal authority is unavailable")
		}
		if live.Pending != "" || live.Completion == nil || live.CompletionID == "" {
			report.addRetentionBlocker("operation.not_terminal", "only an removal operation with a canonical terminal completion may be pruned")
			return finish(fmt.Errorf("%w: client removal operation is not terminal", ErrPolicy), "the client removal operation is not terminal")
		}
		marker, err = prepareRetentionIntent(handle, live, options, rootInfo.Identity)
		if err != nil {
			return finish(err, "the terminal removal journal could not authorize pruning")
		}
		if state.IntentID != "" && !sameRetentionIntent(state.Intent, marker) {
			return finish(fmt.Errorf("%w: pending retention intent disagrees with the terminal journal", ErrIntegrity), "the pending retention intent disagrees with the terminal journal")
		}
	} else {
		marker = state.Intent
	}
	if err := selectRetentionMarker(&report, marker, options, rootInfo.Identity, subtree.Identity()); err != nil {
		return finish(err, "the retention marker does not match the explicit selector")
	}
	if state.CompletePresent {
		report.Operation.PhaseBefore = "retained_removal_completion"
		report.Operation.PhaseAfter = report.Operation.PhaseBefore
	} else if state.IntentPresent {
		report.Operation.PhaseBefore = "retention_intent_recorded"
		report.Operation.PhaseAfter = report.Operation.PhaseBefore
	} else if state.DirectoryPresent {
		report.Operation.Status = "retention_initializing"
		report.Operation.PhaseBefore = "retention_initializing"
		report.Operation.PhaseAfter = report.Operation.PhaseBefore
		report.Markers.State = "initializing"
		report.Markers.PruneResumable = true
	}
	if needLive {
		if err := validateRetentionAgainstLive(marker, live); err != nil {
			return finish(err, "the retention marker disagrees with the terminal removal journal")
		}
	}
	usage, err := auditRetentionNamespace(ctx, handle, marker, options.Limits, state.DirectoryPresent, state.IntentPresent, false)
	report.Used = usage
	if err != nil {
		return finish(err, "client removal private state could not be completely inventoried")
	}
	intentRaw, intentMarkerID, err := EncodeRetentionIntent(marker)
	if err != nil || int64(len(intentRaw)) > options.Limits.MaxMarkerBytes {
		if err == nil {
			err = fmt.Errorf("%w: retention intent exceeds the requested marker budget", ErrPolicy)
		}
		return finish(err, "the client removal retention intent cannot be encoded within limits")
	}
	if needLive {
		if err := handle.confirmDurability(ctx); err != nil {
			return finish(err, "the terminal removal journal durability could not be refreshed")
		}
		freshUsage, auditErr := auditRetentionNamespace(ctx, handle, marker, options.Limits, state.DirectoryPresent, false, false)
		if auditErr != nil {
			return finish(auditErr, "the terminal removal journal changed before retention intent publication")
		}
		report.Used = freshUsage
	}
	intentReceipt, markerReceiptErr := ensureRetentionMarker(ctx, handle, retentionIntentName, retentionIntentPending, intentRaw)
	report.recordRetentionMarker(intentReceipt)
	if markerReceiptErr != nil {
		if report.WritesPerformed > 0 || report.WritesUncertain {
			report.Operation.Status = "retention_initializing"
			report.Operation.PhaseAfter = "retention_initializing"
			report.Markers.State = "initializing"
			report.Markers.PruneResumable = false
		}
		return finish(markerReceiptErr, "client removal retention intent publication failed")
	}
	report.Markers.State = "intent_published"
	report.Markers.IntentMarkerID = intentMarkerID.String()
	report.Markers.IntentDurable = true
	report.Markers.PruneResumable = true
	state, err = loadRetentionState(ctx, handle, options.OperationID, rootInfo.Identity)
	if err != nil || !state.IntentPresent || state.IntentID != intentMarkerID || !sameRetentionIntent(state.Intent, marker) {
		if err == nil || retentionAuthorityLost(err) {
			markRetentionIntentUnbound(&report)
		}
		if err == nil {
			err = fmt.Errorf("%w: durable retention intent changed", ErrIntegrity)
		}
		return finish(err, "the durable retention intent could not be rebound")
	}
	if retentionTransitionHook != nil && !state.CompletePresent {
		if hookErr := retentionTransitionHook("intent_published"); hookErr != nil {
			return finish(hookErr, "client removal pruning stopped after its durable intent")
		}
	}
	if state.CompletePresent {
		completeRaw, completeID, encodeErr := EncodeRetentionComplete(state.Complete)
		if encodeErr != nil || completeID != state.CompleteID {
			return finish(fmt.Errorf("%w: retained completion cannot be reproduced", ErrIntegrity), "the retained completion cannot be reproduced")
		}
		completeReceipt, ensureErr := ensureRetentionMarker(ctx, handle, retentionCompleteName, retentionCompletePending, completeRaw)
		report.recordRetentionMarker(completeReceipt)
		if ensureErr != nil {
			return finish(ensureErr, "the retained completion durability could not be refreshed")
		}
		if err := verifyRetentionTombstone(ctx, handle, options.OperationID, rootInfo.Identity, options.Limits); err != nil {
			return finish(err, "the retained client removal tombstone is not exact")
		}
		state, err = loadRetentionState(ctx, handle, options.OperationID, rootInfo.Identity)
		if err != nil {
			return finish(err, "the retained client removal tombstone changed")
		}
		populateRetentionCompleteReport(&report, state)
		if report.WritesPerformed == 0 {
			report.Outcome = RetentionOutcomeAlreadyPruned
		} else {
			report.Outcome = RetentionOutcomePruned
		}
		finalizeRetentionReport(&report)
		return report, nil
	}
	state, err = reloadSelectedRetentionIntent(ctx, handle, options.OperationID, rootInfo.Identity, marker, intentMarkerID)
	if err != nil {
		if retentionAuthorityLost(err) {
			markRetentionIntentUnbound(&report)
		}
		return finish(err, "the durable retention intent changed before private journal deletion")
	}

	if err := removeLegacyRemovalState(ctx, handle, marker, &report); err != nil {
		return finish(err, "client removal private journal deletion was interrupted")
	}
	if _, err := auditRetentionNamespace(ctx, handle, marker, options.Limits, true, true, true); err != nil {
		return finish(err, "client removal private journal remains after pruning")
	}
	if retentionTransitionHook != nil {
		if hookErr := retentionTransitionHook("legacy_state_removed"); hookErr != nil {
			return finish(hookErr, "client removal pruning stopped after private state removal")
		}
	}
	state, err = reloadSelectedRetentionIntent(ctx, handle, options.OperationID, rootInfo.Identity, marker, intentMarkerID)
	if err != nil || state.CompletePresent {
		if err != nil && retentionAuthorityLost(err) {
			markRetentionIntentUnbound(&report)
		}
		if err == nil {
			err = fmt.Errorf("%w: retention completion appeared outside this prune transition", ErrIntegrity)
		}
		return finish(err, "the durable retention intent changed before completion publication")
	}
	complete := RetentionComplete{Schema: RetentionCompleteSchemaV1, OperationID: marker.OperationID,
		OperationRootIdentity: marker.OperationRootIdentity, TargetRootIdentity: marker.TargetRootIdentity,
		PlanID: marker.PlanID, IntentMarkerID: intentMarkerID, CompletionID: marker.CompletionID}
	completeRaw, completeID, err := EncodeRetentionComplete(complete)
	if err != nil || int64(len(completeRaw)) > options.Limits.MaxMarkerBytes {
		if err == nil {
			err = fmt.Errorf("%w: retention completion exceeds the requested marker budget", ErrPolicy)
		}
		return finish(err, "the client removal retention completion cannot be encoded")
	}
	completeReceipt, completeErr := ensureRetentionMarker(ctx, handle, retentionCompleteName, retentionCompletePending, completeRaw)
	report.recordRetentionMarker(completeReceipt)
	if completeErr != nil {
		return finish(completeErr, "client removal retention completion publication failed")
	}
	if err := verifyRetentionTombstone(ctx, handle, options.OperationID, rootInfo.Identity, options.Limits); err != nil {
		return finish(err, "the client removal retention tombstone could not be reverified")
	}
	state, err = loadRetentionState(ctx, handle, options.OperationID, rootInfo.Identity)
	if err != nil || state.CompleteID != completeID {
		if err == nil {
			err = fmt.Errorf("%w: retention completion identity changed", ErrIntegrity)
		}
		return finish(err, "the client removal retention completion changed")
	}
	populateRetentionCompleteReport(&report, state)
	report.Outcome = RetentionOutcomePruned
	finalizeRetentionReport(&report)
	return report, nil
}

func markRetentionIntentUnbound(report *RetentionReport) {
	if report == nil {
		return
	}
	report.Markers.State = "intent_revalidation_failed"
	report.Markers.IntentDurable = false
	report.Markers.ExactTombstone = false
	report.Markers.PruneResumable = false
}

func retentionAuthorityLost(err error) bool {
	return errors.Is(err, ErrIntegrity) || errors.Is(err, fsbind.ErrUnsafeObject) ||
		errors.Is(err, fsbind.ErrBindingChanged) || errors.Is(err, fsbind.ErrCrossFilesystem)
}

func reloadSelectedRetentionIntent(ctx context.Context, handle *journalHandle, operation OperationID, targetIdentity fsbind.Identity, marker RetentionIntent, markerID RetentionMarkerID) (retentionState, error) {
	state, err := loadRetentionState(ctx, handle, operation, targetIdentity)
	if err != nil {
		return state, err
	}
	if !state.IntentPresent || state.IntentPending || state.IntentID != markerID || !sameRetentionIntent(state.Intent, marker) {
		return state, fmt.Errorf("%w: selected client removal retention intent changed", ErrIntegrity)
	}
	return state, nil
}

func prepareRetentionIntent(handle *journalHandle, live journalState, options PruneOptions, targetIdentity fsbind.Identity) (RetentionIntent, error) {
	if handle == nil || handle.subtree == nil || live.Completion == nil || live.IntentID == "" || live.CompletionID == "" ||
		live.Intent.PlanID != options.ExpectedPlanID || live.Intent.OperationID != options.OperationID {
		return RetentionIntent{}, fmt.Errorf("%w: terminal client removal authority disagrees with the selector", ErrPolicy)
	}
	marker := RetentionIntent{Schema: RetentionIntentSchemaV1, OperationID: options.OperationID,
		OperationRootIdentity: handle.subtree.Identity().String(), TargetRootIdentity: targetIdentity.String(),
		PlanID: live.Intent.PlanID, IntentID: live.IntentID, Intent: live.Intent,
		AttemptIDs: append([]MarkerID{}, live.AttemptIDs...), Attempts: append([]Attempt{}, live.Attempts...),
		Responses:    retainedResponses(live),
		CompletionID: live.CompletionID, Completion: *live.Completion, Basis: RetentionBasisTerminal}
	if err := marker.Validate(); err != nil {
		return RetentionIntent{}, err
	}
	return marker, nil
}

func validateRetentionAgainstLive(marker RetentionIntent, live journalState) error {
	if live.Intent != marker.Intent || live.IntentID != marker.IntentID || live.Completion == nil || *live.Completion != marker.Completion ||
		live.CompletionID != marker.CompletionID || len(live.Attempts) != len(marker.Attempts) || len(live.AttemptIDs) != len(marker.AttemptIDs) ||
		len(live.Responses) != len(marker.Responses) {
		return fmt.Errorf("%w: retention intent differs from terminal client removal journal", ErrIntegrity)
	}
	for index := range live.Attempts {
		if live.Attempts[index] != marker.Attempts[index] || live.AttemptIDs[index] != marker.AttemptIDs[index] {
			return fmt.Errorf("%w: retention attempt chain differs from terminal client removal journal", ErrIntegrity)
		}
	}
	for _, retained := range marker.Responses {
		if response, ok := live.Responses[retained.Sequence]; !ok || response != retained.Response || live.ResponseIDs[retained.Sequence] != retained.MarkerID {
			return fmt.Errorf("%w: retention response chain differs from terminal client removal journal", ErrIntegrity)
		}
	}
	return nil
}

func retainedResponses(state journalState) []RetainedResponse {
	result := make([]RetainedResponse, 0, len(state.Responses))
	for sequence := 1; sequence <= len(state.Attempts); sequence++ {
		if response, ok := state.Responses[sequence]; ok {
			result = append(result, RetainedResponse{Sequence: sequence, MarkerID: state.ResponseIDs[sequence], Response: response})
		}
	}
	return result
}

func selectRetentionMarker(report *RetentionReport, marker RetentionIntent, options PruneOptions, targetIdentity, operationIdentity fsbind.Identity) error {
	if marker.Validate() != nil || marker.OperationID != options.OperationID || marker.PlanID != options.ExpectedPlanID ||
		marker.TargetRootIdentity != targetIdentity.String() || marker.OperationRootIdentity != operationIdentity.String() {
		report.addRetentionBlocker("operation.selector_mismatch", "the explicit operation or reviewed plan does not select this retention authority")
		return fmt.Errorf("%w: client removal retention selector disagrees", ErrPolicy)
	}
	plan := marker.Intent.Plan
	report.Operation = OperationReport{ID: marker.OperationID.String(), Status: "pruning", PhaseBefore: "removal_complete", PhaseAfter: "retention_intent_recorded", Resumable: false}
	report.Plan = PlanReport{ID: marker.PlanID, ExpectedID: options.ExpectedPlanID, Matches: marker.PlanID == options.ExpectedPlanID, Data: plan}
	report.Target.ExpectedRootIdentity = marker.TargetRootIdentity
	report.Proof = RetentionProofReport{Basis: marker.Basis, IntentID: marker.IntentID.String(), CompletionID: marker.CompletionID.String(),
		AttemptsRecorded: len(marker.Attempts), ResponsesRecorded: len(marker.Responses), HistoricalAuthority: true,
		Assurance: "historical_exact_terminal_journal_bound_to_private_retention_marker"}
	return nil
}

func auditRetentionNamespace(ctx context.Context, handle *journalHandle, marker RetentionIntent, limits RetentionLimits, retentionExpected, allowMissingLegacy, requireHeavyAbsent bool) (RetentionUsage, error) {
	usage := RetentionUsage{}
	listing, err := handle.subtree.List(ctx, fsbind.Path{}, fsbind.ListLimits{MaxEntries: limits.MaxObjects, MaxNameBytes: limits.MaxPathBytes})
	if err != nil {
		return usage, classifyJournalError(err)
	}
	if !listing.Complete {
		return usage, fmt.Errorf("%w: client removal operation inventory is incomplete", ErrPolicy)
	}
	usage.ObjectsConsidered += len(listing.Entries)
	usage.PathBytesConsidered += listing.Used.NameBytes
	if usage.ObjectsConsidered > limits.MaxObjects || usage.PathBytesConsidered > limits.MaxPathBytes {
		return usage, fmt.Errorf("%w: client removal retention inventory budget is exhausted", ErrPolicy)
	}
	expected := map[string][]byte{}
	intentRaw, _, _ := encodeIntent(marker.Intent)
	expected[intentFileName] = intentRaw
	for index, attempt := range marker.Attempts {
		raw, _, _ := encodeAttempt(attempt)
		expected[attemptFileName(index+1)] = raw
	}
	for _, retained := range marker.Responses {
		raw, _, _ := encodeResponse(retained.Response)
		expected[responseFileName(retained.Sequence)] = raw
	}
	completionRaw, _, _ := encodeCompletion(marker.Completion)
	expected[completionFileName] = completionRaw
	seenLock, seenScratch, seenRetention := false, false, false
	seenLegacy := make(map[string]bool)
	for _, entry := range listing.Entries {
		switch {
		case entry.Name == ".fsbind-operation.lock" && entry.Kind == string(fsbind.ObjectKindRegular):
			seenLock = true
		case entry.Name == scratchDirectory && entry.Kind == string(fsbind.ObjectKindDirectory):
			seenScratch = true
		case entry.Name == retentionDirectoryName && entry.Kind == string(fsbind.ObjectKindDirectory):
			seenRetention = true
		case entry.Kind == string(fsbind.ObjectKindRegular) && expected[entry.Name] != nil:
			seenLegacy[entry.Name] = true
			raw, readErr := handle.readNamedBytes(ctx, []string{entry.Name})
			if readErr != nil || !bytesEqual(raw, expected[entry.Name]) {
				return usage, fmt.Errorf("%w: client removal legacy marker differs from retained authority", ErrIntegrity)
			}
			usage.BytesConsidered += int64(len(raw))
		default:
			return usage, fmt.Errorf("%w: client removal operation contains an unexpected object", ErrIntegrity)
		}
	}
	if !seenLock || retentionExpected && !seenRetention || !retentionExpected && seenRetention {
		return usage, fmt.Errorf("%w: client removal retention namespace is incomplete", ErrIntegrity)
	}
	if requireHeavyAbsent {
		if seenScratch || len(seenLegacy) != 0 {
			return usage, fmt.Errorf("%w: client removal legacy journal remains after pruning", ErrIntegrity)
		}
	} else if !allowMissingLegacy {
		if !seenScratch || len(seenLegacy) != len(expected) {
			return usage, fmt.Errorf("%w: client removal terminal journal is incomplete before pruning", ErrIntegrity)
		}
	}
	if seenScratch {
		scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
		scratchListing, listErr := handle.subtree.List(ctx, scratch, fsbind.ListLimits{MaxEntries: 2, MaxNameBytes: limits.MaxPathBytes})
		if listErr != nil {
			return usage, classifyJournalError(listErr)
		}
		usage.ObjectsConsidered += len(scratchListing.Entries)
		usage.PathBytesConsidered += scratchListing.Used.NameBytes
		if !scratchListing.Complete || len(scratchListing.Entries) != 0 {
			return usage, fmt.Errorf("%w: client removal scratch is not empty", ErrIntegrity)
		}
	}
	if usage.BytesConsidered > limits.MaxMarkerBytes*int64(2*maximumAttempts+2) {
		return usage, fmt.Errorf("%w: client removal retention byte budget is exhausted", ErrPolicy)
	}
	retentionPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	paths := []fsbind.Path{fsbind.Path{}}
	if seenRetention {
		paths = append(paths, retentionPath)
	}
	if err := handle.subtree.CheckPaths(paths...); err != nil {
		return usage, classifyJournalError(err)
	}
	return usage, nil
}

func removeLegacyRemovalState(ctx context.Context, handle *journalHandle, marker RetentionIntent, report *RetentionReport) error {
	completionRaw, _, _ := encodeCompletion(marker.Completion)
	removal, _, err := removeExpectedRetentionFile(ctx, handle, []string{completionFileName}, completionRaw)
	report.Removals.Completion = removal
	report.recordRetentionRemoval(removal)
	if err != nil {
		markRetentionRemovalError(report, removal)
		return err
	}
	report.Removals.Attempts = make([]fsbind.Removal, len(marker.Attempts))
	report.Removals.Responses = make([]fsbind.Removal, len(marker.Responses))
	for index := len(marker.Responses) - 1; index >= 0; index-- {
		retained := marker.Responses[index]
		raw, _, _ := encodeResponse(retained.Response)
		removal, _, err = removeExpectedRetentionFile(ctx, handle, []string{responseFileName(retained.Sequence)}, raw)
		report.Removals.Responses[index] = removal
		report.recordRetentionRemoval(removal)
		if err != nil {
			markRetentionRemovalError(report, removal)
			return err
		}
	}
	for index := len(marker.Attempts) - 1; index >= 0; index-- {
		raw, _, _ := encodeAttempt(marker.Attempts[index])
		removal, _, err = removeExpectedRetentionFile(ctx, handle, []string{attemptFileName(index + 1)}, raw)
		report.Removals.Attempts[index] = removal
		report.recordRetentionRemoval(removal)
		if err != nil {
			markRetentionRemovalError(report, removal)
			return err
		}
	}
	intentRaw, _, _ := encodeIntent(marker.Intent)
	removal, _, err = removeExpectedRetentionFile(ctx, handle, []string{intentFileName}, intentRaw)
	report.Removals.Intent = removal
	report.recordRetentionRemoval(removal)
	if err != nil {
		markRetentionRemovalError(report, removal)
		return err
	}
	scratchPath, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	scratch, inspectErr := handle.subtree.Inspect(ctx, scratchPath)
	if errors.Is(inspectErr, fsbind.ErrNotFound) {
		return nil
	}
	if inspectErr != nil {
		return classifyJournalError(inspectErr)
	}
	if scratch.Kind != fsbind.ObjectKindDirectory {
		return fmt.Errorf("%w: client removal scratch object is unsafe", ErrIntegrity)
	}
	listing, listErr := handle.subtree.List(ctx, scratchPath, fsbind.ListLimits{MaxEntries: 1, MaxNameBytes: 1 << 20})
	if listErr != nil || !listing.Complete || len(listing.Entries) != 0 {
		if listErr != nil {
			return classifyJournalError(listErr)
		}
		return fmt.Errorf("%w: client removal scratch is not empty", ErrIntegrity)
	}
	removal, err = handle.subtree.RemoveEmptyDirectory(ctx, scratchPath, scratch.Identity)
	report.Removals.Scratch = removal
	report.recordRetentionRemoval(removal)
	if err != nil {
		markRetentionRemovalError(report, removal)
	}
	return err
}

func markRetentionRemovalError(report *RetentionReport, removal fsbind.Removal) {
	if report == nil || !removal.Attempted {
		return
	}
	if removal.Removed && removal.Durability == "confirmed" {
		report.WritesUncertain = true
		report.Writes.AmbiguousRemovals++
	}
}

func verifyRetentionTombstone(ctx context.Context, handle *journalHandle, operation OperationID, targetIdentity fsbind.Identity, limits RetentionLimits) error {
	state, err := loadRetentionState(ctx, handle, operation, targetIdentity)
	if err != nil {
		return err
	}
	if !state.IntentPresent || !state.CompletePresent || state.IntentPending || state.CompletePending ||
		!retentionCompleteMatchesIntent(state.Complete, state.Intent, state.IntentID) {
		return fmt.Errorf("%w: client removal retention tombstone is incomplete", ErrIntegrity)
	}
	if _, err := auditRetentionNamespace(ctx, handle, state.Intent, limits, true, true, true); err != nil {
		return err
	}
	return nil
}

func populateRetentionCompleteReport(report *RetentionReport, state retentionState) {
	report.Markers = RetentionMarkerReport{State: "complete", IntentMarkerID: state.IntentID.String(), CompleteMarkerID: state.CompleteID.String(),
		IntentDurable: true, CompletionDurable: true, ExactTombstone: true, PruneResumable: false}
	report.Operation.Status = "retained"
	report.Operation.PhaseAfter = "retained_removal_completion"
	report.Operation.Resumable = false
}

func mapRetentionError(report *RetentionReport, err error, message string) {
	if report == nil || err == nil {
		return
	}
	switch {
	case errors.Is(err, ErrIntegrity), errors.Is(err, fsbind.ErrUnsafeObject), errors.Is(err, fsbind.ErrBindingChanged), errors.Is(err, fsbind.ErrCrossFilesystem):
		report.Outcome = RetentionOutcomeIntegrityFailed
		report.Operation.Resumable = false
		report.addRetentionIssue("integrity.verification_failed", message)
	case errors.Is(err, ErrOperationNotFound):
		report.Outcome = RetentionOutcomeBlocked
		report.Operation.Status = "not_found"
		report.Operation.Resumable = false
		report.addRetentionBlocker("operation.not_found", message)
	case errors.Is(err, ErrPolicy), errors.Is(err, fsbind.ErrAlreadyExists):
		report.Outcome = RetentionOutcomeBlocked
		report.addRetentionBlocker("operation.policy_blocked", message)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		report.Outcome = RetentionOutcomeInterrupted
		report.addRetentionIssue("operation.interrupted", message)
	default:
		report.Outcome = RetentionOutcomeInterrupted
		report.addRetentionIssue("operation.incomplete", message)
	}
}

func finalizeRetentionReport(report *RetentionReport) {
	if report.Blockers == nil {
		report.Blockers = []Finding{}
	}
	if report.Issues == nil {
		report.Issues = []Finding{}
	}
	if report.Warnings == nil {
		report.Warnings = []string{}
	}
	if report.Effect == nil {
		report.Effect = []string{}
	}
	if report.Removals.Attempts == nil {
		report.Removals.Attempts = []fsbind.Removal{}
	}
	if report.Removals.Responses == nil {
		report.Removals.Responses = []fsbind.Removal{}
	}
	sort.Slice(report.Blockers, func(i, j int) bool { return report.Blockers[i].Code < report.Blockers[j].Code })
	sort.Slice(report.Issues, func(i, j int) bool { return report.Issues[i].Code < report.Issues[j].Code })
	sort.Strings(report.Warnings)
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
