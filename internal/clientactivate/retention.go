package clientactivate

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

var retentionTransitionHook func(string) error

// Prune seals one explicit terminal activation journal into a small private
// tombstone before deleting only that operation's original markers and empty
// scratch. It never contacts or mutates the downloader.
func Prune(ctx context.Context, options PruneOptions) (RetentionReport, error) {
	report := newRetentionReport(options)
	finish := func(err error, message string) (RetentionReport, error) {
		mapRetentionError(&report, err, message)
		finalizeRetentionReport(&report)
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return finish(err, "client activation pruning was cancelled before observation")
	}
	if options.TargetRoot == "" || options.Limits.Validate() != nil || !options.Acknowledge {
		return finish(fmt.Errorf("%w: client activation prune input is invalid", ErrPolicy), "client activation prune requires valid selectors, limits, and acknowledgement")
	}
	if parsed, err := ParseOperationID(options.OperationID.String()); err != nil || parsed != options.OperationID ||
		!canonicalPlanID(options.ExpectedPlanID) || OperationIDForPlan(options.ExpectedPlanID) != options.OperationID {
		return finish(fmt.Errorf("%w: client activation prune selector is invalid", ErrPolicy), "client activation prune selector is invalid")
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
		return finish(inspectErr, "the client activation forget boundary could not be inspected")
	} else if pending != nil {
		return finish(pending, "client activation historical evidence deletion is already in progress")
	}
	directoryName, _ := operationDirectoryName(options.OperationID)
	object, err := target.InspectRoot(ctx, directoryName)
	if errors.Is(err, fsbind.ErrNotFound) {
		return finish(ErrOperationNotFound, "the explicit client activation operation does not exist")
	}
	if err != nil || object.Kind != fsbind.ObjectKindDirectory {
		if err == nil {
			err = ErrIntegrity
		}
		return finish(classifyJournalError(err), "the client activation operation directory is unsafe")
	}
	subtree, err := target.OpenPrivateSubtreeObserved(directoryName)
	if err != nil {
		return finish(classifyJournalError(err), "the client activation operation could not be bound")
	}
	defer subtree.Close()
	handle := &journalHandle{session: target, subtree: subtree}
	state, err := loadRetentionState(ctx, handle, options.OperationID, rootInfo.Identity)
	if err != nil {
		return finish(err, "client activation retention state could not be loaded")
	}
	if state.ForgetPending {
		return finish(&forgetInProgressError{marker: state.ForgetIntent, markerID: state.ForgetID}, "client activation historical evidence deletion is already staged")
	}
	var marker RetentionIntent
	needLive := !state.IntentPresent
	if needLive {
		live, readErr := handle.readStateForRetention(ctx)
		if readErr != nil {
			return finish(readErr, "terminal client activation journal authority is unavailable")
		}
		if live.Pending != "" || !terminalActivationState(live) {
			return finish(fmt.Errorf("%w: client activation operation is not terminal", ErrPolicy), "only a canonical terminal activation operation may be pruned")
		}
		marker, err = prepareRetentionIntent(handle, live, rootInfo.Identity)
		if err != nil {
			return finish(err, "the terminal activation journal could not authorize pruning")
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
		report.Operation.PhaseBefore, report.Operation.PhaseAfter = "retained_activation_completion", "retained_activation_completion"
	} else if state.IntentPresent {
		report.Operation.PhaseBefore, report.Operation.PhaseAfter = "retention_intent_recorded", "retention_intent_recorded"
	} else if state.DirectoryPresent {
		report.Operation.Status = "retention_initializing"
		report.Operation.PhaseBefore, report.Operation.PhaseAfter = "retention_initializing", "retention_initializing"
		report.Markers.State, report.Markers.PruneResumable = "initializing", true
	}
	intentRaw, intentMarkerID, err := EncodeRetentionIntent(marker)
	if err != nil || int64(len(intentRaw)) > options.Limits.MaxMarkerBytes {
		if err == nil {
			err = fmt.Errorf("%w: retention intent exceeds the requested marker budget", ErrPolicy)
		}
		return finish(err, "the client activation retention intent cannot be encoded within limits")
	}
	usage, err := auditRetentionNamespace(ctx, handle, marker, options.Limits, state.DirectoryPresent, state.IntentPresent, false)
	report.Used = usage
	if err != nil {
		return finish(err, "client activation private state could not be completely inventoried")
	}
	if needLive {
		if err := handle.confirmDurability(ctx); err != nil {
			return finish(err, "the terminal activation journal durability could not be refreshed")
		}
		fresh, auditErr := auditRetentionNamespace(ctx, handle, marker, options.Limits, state.DirectoryPresent, false, false)
		report.Used = fresh
		if auditErr != nil {
			return finish(auditErr, "the terminal activation journal changed before retention publication")
		}
	}
	receipt, markerErr := ensureRetentionMarker(ctx, handle, retentionIntentName, retentionIntentPending, intentRaw)
	report.recordRetentionMarker(receipt)
	if markerErr != nil {
		if report.WritesPerformed > 0 || report.WritesUncertain {
			report.Operation.Status, report.Operation.PhaseAfter, report.Markers.State = "retention_initializing", "retention_initializing", "initializing"
		}
		return finish(markerErr, "client activation retention intent publication failed")
	}
	report.Markers = RetentionMarkerReport{State: "intent_published", IntentMarkerID: intentMarkerID.String(), IntentDurable: true, PruneResumable: true}
	state, err = reloadSelectedRetentionIntent(ctx, handle, options.OperationID, rootInfo.Identity, marker, intentMarkerID)
	if err != nil {
		if retentionAuthorityLost(err) {
			markRetentionIntentUnbound(&report)
		}
		return finish(err, "the durable retention intent could not be rebound")
	}
	if retentionTransitionHook != nil && !state.CompletePresent {
		if hookErr := retentionTransitionHook("intent_published"); hookErr != nil {
			return finish(hookErr, "client activation pruning stopped after its durable intent")
		}
	}
	if state.CompletePresent {
		completeRaw, completeID, encodeErr := EncodeRetentionComplete(state.Complete)
		if encodeErr != nil || completeID != state.CompleteID {
			return finish(fmt.Errorf("%w: retained completion cannot be reproduced", ErrIntegrity), "the retained completion cannot be reproduced")
		}
		if int64(len(completeRaw)) > options.Limits.MaxMarkerBytes {
			return finish(fmt.Errorf("%w: retained completion exceeds the requested marker budget", ErrPolicy), "the retained completion cannot be encoded within limits")
		}
		completeReceipt, ensureErr := ensureRetentionMarker(ctx, handle, retentionCompleteName, retentionCompletePending, completeRaw)
		report.recordRetentionMarker(completeReceipt)
		if ensureErr != nil {
			return finish(ensureErr, "the retained completion durability could not be refreshed")
		}
		if err := verifyRetentionTombstone(ctx, handle, options.OperationID, rootInfo.Identity, options.Limits); err != nil {
			return finish(err, "the retained activation tombstone is not exact")
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
	if retentionTransitionHook != nil {
		if hookErr := retentionTransitionHook("before_legacy_removal"); hookErr != nil {
			return finish(hookErr, "client activation pruning stopped before private journal deletion")
		}
	}
	state, err = reloadSelectedRetentionIntent(ctx, handle, options.OperationID, rootInfo.Identity, marker, intentMarkerID)
	if err != nil {
		if retentionAuthorityLost(err) {
			markRetentionIntentUnbound(&report)
		}
		return finish(err, "the durable retention intent changed before private journal deletion")
	}
	if err := removeLegacyActivationState(ctx, handle, marker, &report); err != nil {
		return finish(err, "client activation private journal deletion was interrupted")
	}
	if _, err := auditRetentionNamespace(ctx, handle, marker, options.Limits, true, true, true); err != nil {
		return finish(err, "client activation private journal remains after pruning")
	}
	report.Operation.PhaseAfter = "legacy_activation_state_removed"
	if retentionTransitionHook != nil {
		if hookErr := retentionTransitionHook("legacy_state_removed"); hookErr != nil {
			return finish(hookErr, "client activation pruning stopped after private state removal")
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
	terminalID := marker.RecheckCompletionID
	if marker.ActivationCompletion != nil {
		terminalID = marker.ActivationCompletionID
	}
	complete := RetentionComplete{Schema: RetentionCompleteSchemaV1, OperationID: marker.OperationID,
		OperationRootIdentity: marker.OperationRootIdentity, TargetRootIdentity: marker.TargetRootIdentity,
		PlanID: marker.PlanID, IntentMarkerID: intentMarkerID, TerminalMarkerID: terminalID}
	completeRaw, completeID, err := EncodeRetentionComplete(complete)
	if err != nil || int64(len(completeRaw)) > options.Limits.MaxMarkerBytes {
		if err == nil {
			err = fmt.Errorf("%w: retention completion exceeds the requested marker budget", ErrPolicy)
		}
		return finish(err, "the client activation retention completion cannot be encoded")
	}
	completeReceipt, completeErr := ensureRetentionMarker(ctx, handle, retentionCompleteName, retentionCompletePending, completeRaw)
	report.recordRetentionMarker(completeReceipt)
	if completeErr != nil {
		return finish(completeErr, "client activation retention completion publication failed")
	}
	if err := verifyRetentionTombstone(ctx, handle, options.OperationID, rootInfo.Identity, options.Limits); err != nil {
		return finish(err, "the client activation retention tombstone could not be reverified")
	}
	state, err = loadRetentionState(ctx, handle, options.OperationID, rootInfo.Identity)
	if err != nil || state.CompleteID != completeID {
		if err == nil {
			err = fmt.Errorf("%w: retention completion identity changed", ErrIntegrity)
		}
		return finish(err, "the client activation retention completion changed")
	}
	populateRetentionCompleteReport(&report, state)
	report.Outcome = RetentionOutcomePruned
	finalizeRetentionReport(&report)
	return report, nil
}

func terminalActivationState(state journalState) bool {
	if state.Intent.Plan.Action == ActionRecheckOnly {
		return state.RecheckCompletion != nil && state.RecheckCompletionID != "" && state.ActivationCompletion == nil && len(state.StartAttempts) == 0
	}
	if state.Intent.Plan.Action == ActionRecheckThenStart {
		return state.RecheckCompletion != nil && state.RecheckCompletionID != "" && state.ActivationCompletion != nil && state.ActivationCompletionID != ""
	}
	return state.Intent.Plan.Action == ActionStartAfterStop && state.RecheckCompletion == nil && state.RecheckCompletionID == "" &&
		state.ActivationCompletion != nil && state.ActivationCompletionID != ""
}

func prepareRetentionIntent(handle *journalHandle, state journalState, targetIdentity fsbind.Identity) (RetentionIntent, error) {
	if handle == nil || handle.subtree == nil || !terminalActivationState(state) || state.Pending != "" {
		return RetentionIntent{}, fmt.Errorf("%w: terminal client activation authority is unavailable", ErrIntegrity)
	}
	links := make([]RetainedMarkerLink, 0, 10)
	add := func(name string, raw []byte, id MarkerID, err error) error {
		if err != nil || len(raw) == 0 || id == "" {
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: terminal activation marker cannot be reproduced", ErrIntegrity)
		}
		links = append(links, RetainedMarkerLink{Name: name, MarkerID: id, SizeBytes: int64(len(raw))})
		return nil
	}
	raw, id, err := encodeIntent(state.Intent)
	if err = add(intentFileName, raw, id, err); err != nil {
		return RetentionIntent{}, err
	}
	for index, attempt := range state.RecheckAttempts {
		raw, id, err = encodeAttempt(attempt)
		if err = add(attemptFileName(AttemptActionRecheck, index+1), raw, id, err); err != nil {
			return RetentionIntent{}, err
		}
	}
	if state.RecheckStarted != nil {
		raw, id, err = encodeRecheckStarted(*state.RecheckStarted)
		if err = add(recheckStartedFileName, raw, id, err); err != nil {
			return RetentionIntent{}, err
		}
	}
	if state.RecheckCompletion != nil {
		raw, id, err = encodeRecheckCompletion(*state.RecheckCompletion)
		if err = add(recheckCompletionFileName, raw, id, err); err != nil {
			return RetentionIntent{}, err
		}
	}
	for index, attempt := range state.StartAttempts {
		raw, id, err = encodeAttempt(attempt)
		if err = add(attemptFileName(AttemptActionStart, index+1), raw, id, err); err != nil {
			return RetentionIntent{}, err
		}
	}
	if state.ActivationCompletion != nil {
		raw, id, err = encodeActivationCompletion(*state.ActivationCompletion)
		if err = add(activationCompletionName, raw, id, err); err != nil {
			return RetentionIntent{}, err
		}
	}
	marker := RetentionIntent{Schema: RetentionIntentSchemaV1, OperationID: state.Intent.OperationID,
		OperationRootIdentity: handle.subtree.Identity().String(), TargetRootIdentity: targetIdentity.String(), PlanID: state.Intent.PlanID,
		IntentID: state.IntentID, Intent: state.Intent, RecheckCompletionID: state.RecheckCompletionID,
		RecheckCompletion: state.RecheckCompletion, ActivationCompletionID: state.ActivationCompletionID,
		ActivationCompletion: state.ActivationCompletion, Markers: links, Basis: RetentionBasisTerminal}
	if err := marker.Validate(); err != nil {
		return RetentionIntent{}, err
	}
	return marker, nil
}

func reloadSelectedRetentionIntent(ctx context.Context, handle *journalHandle, operation OperationID, targetIdentity fsbind.Identity, marker RetentionIntent, markerID RetentionMarkerID) (retentionState, error) {
	state, err := loadRetentionState(ctx, handle, operation, targetIdentity)
	if err != nil {
		return state, err
	}
	if !state.IntentPresent || state.IntentPending || state.IntentID != markerID || !sameRetentionIntent(state.Intent, marker) {
		return state, fmt.Errorf("%w: selected client activation retention intent changed", ErrIntegrity)
	}
	return state, nil
}

func selectRetentionMarker(report *RetentionReport, marker RetentionIntent, options PruneOptions, targetIdentity, operationIdentity fsbind.Identity) error {
	if marker.Validate() != nil || marker.OperationID != options.OperationID || marker.PlanID != options.ExpectedPlanID ||
		marker.TargetRootIdentity != targetIdentity.String() || marker.OperationRootIdentity != operationIdentity.String() {
		return fmt.Errorf("%w: client activation retention selector disagrees", ErrPolicy)
	}
	plan := marker.Intent.Plan
	report.Operation = OperationReport{ID: marker.OperationID.String(), Status: "pruning", PhaseBefore: phaseForRetainedMarker(marker), PhaseAfter: "retention_intent_recorded", Resumable: false}
	report.Plan = PlanReport{ID: marker.PlanID, ExpectedID: options.ExpectedPlanID, Matches: true, Action: plan.Action, Driver: plan.Driver,
		ClientConfigID: plan.ClientConfigID, Control: plan.Control, PathMappingID: plan.PathMappingID,
		ClientPathSemantics: plan.ClientPathSemantics, ExpectedSavePathRef: plan.ExpectedSavePathRef,
		ExpectedContentPathRef: plan.ExpectedContentPathRef, ExpectedFileLayoutID: plan.ExpectedFileLayoutID, JobID: plan.JobID}
	terminalID := marker.RecheckCompletionID
	if marker.ActivationCompletion != nil {
		terminalID = marker.ActivationCompletionID
	}
	report.Target.ExpectedRootIdentity = marker.TargetRootIdentity
	report.Proof = RetentionProofReport{Basis: marker.Basis, IntentID: marker.IntentID.String(), TerminalMarkerID: terminalID.String(),
		MarkersRecorded: len(marker.Markers), HistoricalAuthority: true,
		Assurance: "same_invocation_exact_terminal_activation_selected_for_private_retention"}
	return nil
}

func phaseForRetainedMarker(marker RetentionIntent) string {
	if marker.ActivationCompletion != nil {
		return "started_client_claim_observed"
	}
	return "recheck_complete_stopped"
}

func auditRetentionNamespace(ctx context.Context, handle *journalHandle, marker RetentionIntent, limits RetentionLimits, retentionExpected, allowMissingLegacy, requireHeavyAbsent bool) (RetentionUsage, error) {
	usage := RetentionUsage{}
	listing, err := handle.subtree.List(ctx, fsbind.Path{}, fsbind.ListLimits{MaxEntries: limits.MaxObjects, MaxNameBytes: limits.MaxPathBytes})
	if err != nil {
		return usage, classifyJournalError(err)
	}
	if !listing.Complete {
		return usage, fmt.Errorf("%w: client activation operation inventory is incomplete", ErrPolicy)
	}
	usage.ObjectsConsidered, usage.PathBytesConsidered = len(listing.Entries), listing.Used.NameBytes
	expected := make(map[string]RetainedMarkerLink, len(marker.Markers))
	for _, link := range marker.Markers {
		if link.SizeBytes > limits.MaxMarkerBytes {
			return usage, fmt.Errorf("%w: retained activation marker exceeds the requested byte budget", ErrPolicy)
		}
		expected[link.Name] = link
	}
	seenLock, seenScratch, seenRetention := false, false, false
	seenLegacy := make(map[string]bool)
	for _, entry := range listing.Entries {
		switch {
		case entry.Name == operationLockEntryName && entry.Kind == string(fsbind.ObjectKindRegular):
			seenLock = true
		case entry.Name == scratchDirectory && entry.Kind == string(fsbind.ObjectKindDirectory):
			seenScratch = true
		case entry.Name == retentionDirectoryName && entry.Kind == string(fsbind.ObjectKindDirectory):
			seenRetention = true
		case entry.Kind == string(fsbind.ObjectKindRegular) && expected[entry.Name].Name != "":
			link := expected[entry.Name]
			raw, readErr := handle.readNamedBytes(ctx, []string{entry.Name})
			if readErr != nil {
				return usage, readErr
			}
			id, idErr := markerIDForName(entry.Name, raw)
			if idErr != nil || id != link.MarkerID || int64(len(raw)) != link.SizeBytes {
				return usage, fmt.Errorf("%w: client activation legacy marker differs from retained authority", ErrIntegrity)
			}
			seenLegacy[entry.Name] = true
			usage.BytesConsidered += int64(len(raw))
		default:
			return usage, fmt.Errorf("%w: client activation operation contains an unexpected object", ErrIntegrity)
		}
	}
	if !seenLock || retentionExpected && !seenRetention || !retentionExpected && seenRetention {
		return usage, fmt.Errorf("%w: client activation retention namespace is incomplete", ErrIntegrity)
	}
	if requireHeavyAbsent {
		if seenScratch || len(seenLegacy) != 0 {
			return usage, fmt.Errorf("%w: client activation legacy journal remains after pruning", ErrIntegrity)
		}
	} else if !allowMissingLegacy && (!seenScratch || len(seenLegacy) != len(expected)) {
		return usage, fmt.Errorf("%w: client activation terminal journal is incomplete before pruning", ErrIntegrity)
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
			return usage, fmt.Errorf("%w: client activation scratch is not empty", ErrIntegrity)
		}
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

func removeLegacyActivationState(ctx context.Context, handle *journalHandle, marker RetentionIntent, report *RetentionReport) error {
	report.Removals.Markers = make([]fsbind.Removal, len(marker.Markers))
	for index := len(marker.Markers) - 1; index >= 0; index-- {
		link := marker.Markers[index]
		path, _ := fsbind.PathFromComponents([]string{link.Name})
		object, err := handle.subtree.Inspect(ctx, path)
		if errors.Is(err, fsbind.ErrNotFound) {
			continue
		}
		if err != nil || object.Kind != fsbind.ObjectKindRegular || object.SizeBytes != link.SizeBytes {
			if err != nil {
				return classifyJournalError(err)
			}
			return fmt.Errorf("%w: retained activation marker object is unsafe", ErrIntegrity)
		}
		raw, readErr := handle.readNamedBytes(ctx, []string{link.Name})
		if readErr != nil {
			return readErr
		}
		id, idErr := markerIDForName(link.Name, raw)
		if idErr != nil || id != link.MarkerID || int64(len(raw)) != link.SizeBytes {
			return fmt.Errorf("%w: retained activation marker bytes disagree", ErrIntegrity)
		}
		removal, removeErr := handle.subtree.RemoveRegularExact(ctx, path, object.Identity, object.SizeBytes)
		report.Removals.Markers[index] = removal
		report.recordRetentionRemoval(removal)
		if removeErr != nil {
			return removeErr
		}
	}
	scratchPath, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	scratch, err := handle.subtree.Inspect(ctx, scratchPath)
	if errors.Is(err, fsbind.ErrNotFound) {
		return nil
	}
	if err != nil || scratch.Kind != fsbind.ObjectKindDirectory {
		if err != nil {
			return classifyJournalError(err)
		}
		return fmt.Errorf("%w: client activation scratch object is unsafe", ErrIntegrity)
	}
	listing, err := handle.subtree.List(ctx, scratchPath, fsbind.ListLimits{MaxEntries: 1, MaxNameBytes: 1 << 20})
	if err != nil || !listing.Complete || len(listing.Entries) != 0 {
		if err != nil {
			return classifyJournalError(err)
		}
		return fmt.Errorf("%w: client activation scratch is not empty", ErrIntegrity)
	}
	removal, err := handle.subtree.RemoveEmptyDirectory(ctx, scratchPath, scratch.Identity)
	report.Removals.Scratch = removal
	report.recordRetentionRemoval(removal)
	return err
}

func verifyRetentionTombstone(ctx context.Context, handle *journalHandle, operation OperationID, targetIdentity fsbind.Identity, limits RetentionLimits) error {
	state, err := loadRetentionState(ctx, handle, operation, targetIdentity)
	if err != nil {
		return err
	}
	if !state.IntentPresent || !state.CompletePresent || state.IntentPending || state.CompletePending || !retentionCompleteMatchesIntent(state.Complete, state.Intent, state.IntentID) {
		return fmt.Errorf("%w: client activation retention tombstone is incomplete", ErrIntegrity)
	}
	_, err = auditRetentionNamespace(ctx, handle, state.Intent, limits, true, true, true)
	return err
}

func populateRetentionCompleteReport(report *RetentionReport, state retentionState) {
	report.Markers = RetentionMarkerReport{State: "complete", IntentMarkerID: state.IntentID.String(), CompleteMarkerID: state.CompleteID.String(), IntentDurable: true, CompletionDurable: true, ExactTombstone: true}
	report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "retained", "retained_activation_completion", false
	report.Proof.Assurance = "historical_exact_terminal_activation_bound_to_complete_private_retention_tombstone"
}

func markRetentionIntentUnbound(report *RetentionReport) {
	if report == nil {
		return
	}
	report.Markers.State = "intent_revalidation_failed"
	report.Markers.IntentDurable = false
	report.Markers.CompletionDurable = false
	report.Markers.ExactTombstone = false
	report.Markers.PruneResumable = false
}

func retentionAuthorityLost(err error) bool {
	return errors.Is(err, ErrIntegrity) || errors.Is(err, fsbind.ErrUnsafeObject) ||
		errors.Is(err, fsbind.ErrBindingChanged) || errors.Is(err, fsbind.ErrCrossFilesystem)
}

func ensureRetentionDirectory(ctx context.Context, handle *journalHandle) (retentionMarkerReceipt, error) {
	receipt := retentionMarkerReceipt{}
	path, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	object, err := handle.subtree.Inspect(ctx, path)
	if err == nil {
		if object.Kind != fsbind.ObjectKindDirectory {
			return receipt, fmt.Errorf("%w: client activation retention path is unsafe", ErrIntegrity)
		}
		return receipt, handle.subtree.CheckPaths(path)
	}
	if !errors.Is(err, fsbind.ErrNotFound) {
		return receipt, classifyJournalError(err)
	}
	mkdir, err := handle.subtree.MkdirAll(ctx, path)
	if mkdir.DirectoriesCreated > 0 {
		receipt.DirectoryCreated, receipt.DirectoryDurability = true, mkdir.Durability
	}
	if err != nil || mkdir.DirectoriesCreated != 1 {
		if err == nil {
			err = fmt.Errorf("%w: client activation retention directory creation was not exact", ErrIntegrity)
		}
		return receipt, err
	}
	if err := handle.subtree.SyncDirectory(ctx, fsbind.Path{}); err != nil {
		return receipt, err
	}
	return receipt, handle.subtree.CheckPaths(path)
}

func ensureRetentionMarker(ctx context.Context, handle *journalHandle, destination, pending string, raw []byte) (retentionMarkerReceipt, error) {
	receipt, err := ensureRetentionDirectory(ctx, handle)
	if err != nil {
		return receipt, err
	}
	if len(raw) == 0 || int64(len(raw)) > maximumRetentionBytes {
		return receipt, fmt.Errorf("%w: client activation retention marker exceeds its budget", ErrPolicy)
	}
	destinationPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, destination})
	pendingPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, pending})
	if object, inspectErr := handle.subtree.Inspect(ctx, destinationPath); inspectErr == nil {
		if object.Kind != fsbind.ObjectKindRegular {
			return receipt, fmt.Errorf("%w: client activation retention destination is unsafe", ErrIntegrity)
		}
		if _, verifyErr := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, destination}, raw); verifyErr != nil {
			return receipt, verifyErr
		}
		receipt.AlreadyPresent = true
		if pendingObject, pendingErr := handle.subtree.Inspect(ctx, pendingPath); pendingErr == nil {
			if _, verifyErr := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, pending}, raw); verifyErr != nil {
				return receipt, verifyErr
			}
			removal, removeErr := handle.subtree.RemoveRegularExact(ctx, pendingPath, pendingObject.Identity, pendingObject.SizeBytes)
			receipt.TemporaryRemoval = removal
			receipt.TemporaryRemovalUncertain = removeErr != nil || (removal.Attempted && (!removal.Removed || removal.Durability != "confirmed"))
			if removeErr != nil {
				return receipt, removeErr
			}
		} else if !errors.Is(pendingErr, fsbind.ErrNotFound) {
			return receipt, classifyJournalError(pendingErr)
		}
		return receipt, confirmRetentionDurability(ctx, handle)
	} else if !errors.Is(inspectErr, fsbind.ErrNotFound) {
		return receipt, classifyJournalError(inspectErr)
	}
	pendingObject, pendingErr := handle.subtree.Inspect(ctx, pendingPath)
	if errors.Is(pendingErr, fsbind.ErrNotFound) {
		file, createErr := handle.subtree.CreateRegular(ctx, pendingPath)
		if createErr != nil {
			if object, inspectErr := handle.subtree.Inspect(ctx, pendingPath); inspectErr == nil && object.Kind == fsbind.ObjectKindRegular {
				receipt.TemporaryCreationUncertain = true
			}
			return receipt, createErr
		}
		receipt.TemporaryCreated = true
		written, writeErr := writeAll(ctx, file, raw)
		receipt.TemporaryBytesWritten = written
		syncErr := file.Sync()
		info, infoErr := file.Info()
		closeErr := file.Close()
		receipt.TemporaryStateUncertain = syncErr != nil || infoErr != nil || closeErr != nil
		if writeErr != nil {
			return receipt, writeErr
		}
		if syncErr != nil || infoErr != nil || closeErr != nil || written != int64(len(raw)) || info.SizeBytes != int64(len(raw)) {
			return receipt, fmt.Errorf("client activation retention staging could not be confirmed")
		}
		pendingObject = fsbind.ObjectInfo{Identity: info.Identity, Kind: fsbind.ObjectKindRegular, SizeBytes: info.SizeBytes}
	} else if pendingErr != nil {
		return receipt, classifyJournalError(pendingErr)
	} else if pendingObject.Kind != fsbind.ObjectKindRegular {
		return receipt, fmt.Errorf("%w: client activation retention staging object is unsafe", ErrIntegrity)
	}
	if _, err := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, pending}, raw); err != nil {
		return receipt, err
	}
	publication, publishErr := handle.subtree.CommitRegularNoReplace(ctx, pendingPath, destinationPath)
	receipt.Publication = publication
	if publishErr != nil {
		receipt.PublicationUncertain = publication.Attempted
		if errors.Is(publishErr, fsbind.ErrAlreadyExists) {
			if _, verifyErr := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, destination}, raw); verifyErr == nil {
				receipt.AlreadyPresent = true
				receipt.PublicationUncertain = false
				removal, removeErr := handle.subtree.RemoveRegularExact(ctx, pendingPath, pendingObject.Identity, pendingObject.SizeBytes)
				receipt.TemporaryRemoval = removal
				receipt.TemporaryRemovalUncertain = removeErr != nil || (removal.Attempted && (!removal.Removed || removal.Durability != "confirmed"))
				if removeErr != nil {
					return receipt, removeErr
				}
				return receipt, confirmRetentionDurability(ctx, handle)
			}
		}
		return receipt, publishErr
	}
	if !publication.Published || !publication.SourceIdentity.Equal(pendingObject.Identity) || !publication.FinalIdentity.Equal(pendingObject.Identity) {
		receipt.PublicationUncertain = true
		return receipt, fsbind.ErrPublicationAmbiguous
	}
	if _, err := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, destination}, raw); err != nil {
		receipt.PublicationUncertain = true
		return receipt, err
	}
	if err := confirmRetentionDurability(ctx, handle); err != nil {
		receipt.PublicationUncertain = true
		return receipt, err
	}
	return receipt, nil
}

func confirmRetentionDurability(ctx context.Context, handle *journalHandle) error {
	directoryPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	if err := handle.subtree.SyncDirectory(ctx, directoryPath); err != nil {
		return err
	}
	if err := handle.subtree.SyncDirectory(ctx, fsbind.Path{}); err != nil {
		return err
	}
	return classifyJournalError(handle.subtree.CheckPaths(fsbind.Path{}, directoryPath))
}

func newRetentionReport(options PruneOptions) RetentionReport {
	return RetentionReport{Outcome: RetentionOutcomeInterrupted,
		Effect:    []string{"read_private_client_activation_operation_state", "write_private_client_activation_retention_markers", "delete_private_client_activation_operation_state"},
		Operation: OperationReport{ID: options.OperationID.String(), Status: "inspection_incomplete", PhaseBefore: "unknown", PhaseAfter: "unknown"},
		Plan:      PlanReport{ExpectedID: options.ExpectedPlanID}, Target: RetentionTargetReport{StabilityAssurance: "not_observed"},
		Proof: RetentionProofReport{Assurance: "not_observed"}, Markers: RetentionMarkerReport{State: "not_started"},
		Limits: options.Limits, Removals: RetentionRemovalReport{Markers: []fsbind.Removal{}}, Blockers: []Finding{}, Issues: []Finding{},
		Warnings: []string{"prune deletes only one explicit activation operation's private journal and retains a small tombstone", "the downloader job, materialized final, metafile store, adoption record, and every other operation remain outside prune authority", "the tombstone records historical activation evidence and does not prove current downloader state"}}
}

func (report *RetentionReport) recordRetentionMarker(receipt retentionMarkerReceipt) {
	if receipt.DirectoryCreated {
		report.WritesPerformed++
		report.Writes.ControlDirectoriesCreated++
		if receipt.DirectoryDurability != "confirmed" {
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
	}
	if receipt.TemporaryRemoval.Removed {
		report.WritesPerformed++
		report.Writes.MarkerTemporaryRemovals++
	}
	if receipt.TemporaryCreationUncertain || receipt.TemporaryStateUncertain || receipt.PublicationUncertain || receipt.TemporaryRemovalUncertain ||
		receipt.Publication.Attempted && !receipt.AlreadyPresent && (!receipt.Publication.Published || receipt.Publication.Durability != "confirmed") {
		report.WritesUncertain = true
	}
}

func (report *RetentionReport) recordRetentionRemoval(removal fsbind.Removal) {
	if removal.Attempted {
		report.Writes.RemovalAttempts++
	}
	if removal.Removed {
		report.WritesPerformed++
		if removal.Kind == fsbind.ObjectKindDirectory {
			report.Writes.DirectoriesRemoved++
		} else {
			report.Writes.FilesRemoved++
			report.Writes.BytesRemoved += removal.SizeBytes
		}
	}
	if removal.Attempted && (!removal.Removed || removal.Durability != "confirmed") {
		report.WritesUncertain = true
		report.Writes.AmbiguousRemovals++
	}
}

func mapRetentionError(report *RetentionReport, err error, message string) {
	if err == nil {
		return
	}
	switch {
	case errors.Is(err, ErrIntegrity), errors.Is(err, fsbind.ErrUnsafeObject), errors.Is(err, fsbind.ErrBindingChanged), errors.Is(err, fsbind.ErrCrossFilesystem):
		report.Outcome, report.Operation.Resumable = RetentionOutcomeIntegrityFailed, false
		report.Issues = append(report.Issues, Finding{Code: "integrity.verification_failed", Message: message})
	case errors.Is(err, ErrOperationNotFound):
		report.Outcome = RetentionOutcomeBlocked
		report.Operation.Status, report.Operation.Resumable = "not_found", false
		report.Blockers = append(report.Blockers, Finding{Code: "operation.not_found", Message: message})
	case errors.Is(err, ErrPolicy), errors.Is(err, fsbind.ErrAlreadyExists), errors.Is(err, fsbind.ErrUnsupported), errors.Is(err, fsbind.ErrInvalidPath):
		report.Outcome = RetentionOutcomeBlocked
		report.Blockers = append(report.Blockers, Finding{Code: "operation.policy_blocked", Message: message})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		report.Outcome = RetentionOutcomeInterrupted
		report.Issues = append(report.Issues, Finding{Code: "operation.interrupted", Message: message})
	default:
		report.Outcome = RetentionOutcomeInterrupted
		report.Issues = append(report.Issues, Finding{Code: "operation.incomplete", Message: message})
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
	if report.Removals.Markers == nil {
		report.Removals.Markers = []fsbind.Removal{}
	}
	sort.Slice(report.Blockers, func(i, j int) bool { return report.Blockers[i].Code < report.Blockers[j].Code })
	sort.Slice(report.Issues, func(i, j int) bool { return report.Issues[i].Code < report.Issues[j].Code })
	sort.Strings(report.Warnings)
}
