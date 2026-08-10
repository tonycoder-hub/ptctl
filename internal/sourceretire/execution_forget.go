package sourceretire

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

var executionForgetTransitionHook func(string) error

// ForgetExecutionTombstone irreversibly removes one explicitly selected,
// already-pruned source-retirement tombstone. A private root-level intent is
// durably published first and remains the only recovery authority until the
// operation subtree is durably absent. The intent is then removed last.
func ForgetExecutionTombstone(ctx context.Context, options ExecutionForgetOptions) (ExecutionForgetReport, error) {
	report := newExecutionForgetReport(options)
	if err := options.Limits.Validate(); err != nil {
		return executionForgetBlocked(&report, "limits.invalid", "source retirement forget limits are invalid")
	}
	if !options.Acknowledge {
		return executionForgetBlocked(&report, "acknowledgement.required", "historical tombstone deletion requires its dedicated acknowledgement")
	}
	derived, derivedErr := executionOperationID(options.ExpectedPlanID)
	if options.TargetRoot == "" || derivedErr != nil || derived != options.OperationID {
		return executionForgetBlocked(&report, "forget.selector_invalid", "target root, operation ID, or reviewed plan ID is invalid")
	}
	if err := ctx.Err(); err != nil {
		return executionForgetInterrupted(&report, err, "source retirement forget was interrupted before target observation")
	}
	absolute, err := filepath.Abs(options.TargetRoot)
	if err != nil {
		return executionForgetBlocked(&report, "target.invalid_root", "the source retirement target root is invalid")
	}
	target, rootInfo, err := fsbind.BindExisting(filepath.Clean(absolute))
	if err != nil {
		return mapExecutionForgetError(&report, err, "the target root cannot provide bound tombstone-forget semantics")
	}
	defer target.Close()
	report.Target.ObservedRootIdentity = rootInfo.Identity.String()
	report.Target.RootIdentityBound = true
	report.Target.StabilityAssurance = "non_atomic_bound_filesystem"

	rootName, _ := ExecutionForgetRootName(options.OperationID)
	marker, markerID, rootObject, _, markerErr := readExecutionForgetRootIntent(ctx, target, rootName, options.Limits.MaxMarkerBytes)
	var subtree *fsbind.Subtree
	rootIntentEnsured := false
	if errors.Is(markerErr, fsbind.ErrNotFound) {
		operationName, _ := OperationDirectoryName(options.OperationID)
		subtree, err = target.OpenPrivateSubtreeObserved(operationName)
		if errors.Is(err, fsbind.ErrNotFound) {
			report.Outcome = ExecutionForgetOutcomeAbsentUnattributed
			report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "absent_unattributed", "absent", false
			report.Authority.State = "absent"
			report.addExecutionForgetIssue("forget.absent_unattributed", "neither the retained operation nor a durable forget intent exists; prior erasure cannot be distinguished from an unknown selector")
			return report, ErrOperationNotFound
		}
		if err != nil {
			return mapExecutionForgetError(&report, err, "the retained source retirement operation could not be opened")
		}
		defer subtree.Close()
		state, loadErr := loadExecutionRetentionState(ctx, subtree, options.OperationID, rootInfo.Identity)
		if loadErr != nil {
			return mapExecutionForgetError(&report, loadErr, "the retained source retirement tombstone could not be loaded")
		}
		if !state.IntentPresent || !state.CompletePresent {
			return executionForgetBlocked(&report, "operation.prune_required", "only a complete retained tombstone may be forgotten")
		}
		marker = ExecutionForgetIntent{
			Schema: ExecutionForgetIntentSchemaV1, OperationID: options.OperationID,
			OperationRootIdentity: subtree.Identity().String(), TargetRootIdentity: rootInfo.Identity.String(),
			PlanID: options.ExpectedPlanID, RetentionIntentMarkerID: state.IntentID,
			RetentionCompleteMarkerID: state.CompleteID, RetentionIntent: state.Intent,
			RetentionComplete: state.Complete, Basis: ExecutionForgetBasisExact,
		}
		if err := marker.Validate(); err != nil {
			return mapExecutionForgetError(&report, err, "the retained source retirement tombstone cannot authorize forgetting")
		}
		if err := auditInitialExecutionForgetTombstone(ctx, subtree, state, marker); err != nil {
			return mapExecutionForgetError(&report, err, "the source retirement tombstone is not exact before forget intent publication")
		}
		preparedRaw, preparedID, prepareErr := EncodeExecutionForgetIntent(marker)
		if prepareErr != nil {
			return mapExecutionForgetError(&report, prepareErr, "the source retirement forget intent cannot be encoded")
		}
		if int64(len(preparedRaw)) > options.Limits.MaxMarkerBytes {
			return mapExecutionForgetError(&report, ErrExecutionPolicy, "the source retirement forget marker byte budget is insufficient")
		}
		markerID = preparedID
		populateExecutionForgetAuthority(&report, marker, markerID)
		report.Authority.State = "exact_tombstone_observed"
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "retained", "retained", false
		markerID, receipt, object, publishErr := ensureExecutionForgetRootIntent(ctx, target, subtree, marker, options.Limits.MaxMarkerBytes)
		report.recordExecutionForgetMarker(receipt)
		rootObject = object
		if markerID != "" {
			report.Authority.MarkerID = markerID.String()
		}
		if publishErr != nil {
			if receipt.Publication.Published || receipt.AlreadyPresent {
				report.Authority.State = "root_intent_publication_observed_unverified"
			} else if receipt.Publication.Attempted {
				report.Authority.State = "root_intent_publication_ambiguous"
			}
			if receipt.Publication.Attempted || receipt.AlreadyPresent {
				report.Authority.MarkerDurable = receipt.AlreadyPresent || receipt.Publication.Durability == fsbind.DurabilityConfirmed
				report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "forgetting", "root_intent_publication_incomplete", true
			}
			return mapExecutionForgetError(&report, publishErr, "the source retirement forget intent could not be durably published")
		}
		rootIntentEnsured = true
	} else if markerErr != nil {
		return mapExecutionForgetError(&report, markerErr, "the source retirement forget intent could not be read")
	} else if err := selectExecutionForgetMarker(marker, options, rootInfo.Identity); err != nil {
		return mapExecutionForgetError(&report, err, "the source retirement forget intent does not match the explicit selector")
	} else {
		populateExecutionForgetAuthority(&report, marker, markerID)
		report.Authority.State = "root_intent_visible_durability_unconfirmed"
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "forgetting", "root_intent_visible", true
		if err := target.SyncRoot(ctx); err != nil {
			return mapExecutionForgetError(&report, err, "the visible source retirement forget intent durability could not be confirmed")
		}
		fresh, freshID, freshObject, _, freshErr := readExecutionForgetRootIntent(ctx, target, rootName, options.Limits.MaxMarkerBytes)
		if freshErr != nil || fresh != marker || freshID != markerID || !freshObject.Identity.Equal(rootObject.Identity) {
			if freshErr != nil {
				return mapExecutionForgetError(&report, freshErr, "the source retirement forget intent could not be re-read after durability confirmation")
			}
			return mapExecutionForgetError(&report, ErrExecutionIntegrity, "the source retirement forget intent changed during durability confirmation")
		}
		rootObject = freshObject
	}

	if markerID == "" {
		_, derivedMarkerID, encodeErr := EncodeExecutionForgetIntent(marker)
		if encodeErr != nil {
			return mapExecutionForgetError(&report, encodeErr, "the source retirement forget intent cannot be reproduced")
		}
		markerID = derivedMarkerID
	}
	populateExecutionForgetAuthority(&report, marker, markerID)
	report.Authority.MarkerDurable = true
	report.Authority.State = "root_intent_published"
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "forgetting", "root_intent_published", true
	if executionForgetTransitionHook != nil {
		if hookErr := executionForgetTransitionHook("root_intent_published"); hookErr != nil {
			return mapExecutionForgetError(&report, hookErr, "source retirement forgetting stopped after its durable root intent")
		}
	}

	if subtree == nil {
		operationName, _ := OperationDirectoryName(options.OperationID)
		expectedOperation, parseErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
		if parseErr != nil {
			return mapExecutionForgetError(&report, parseErr, "the source retirement forget operation identity is invalid")
		}
		subtree, err = target.OpenPrivateSubtree(operationName, expectedOperation)
		if errors.Is(err, fsbind.ErrNotFound) {
			observed, inspectErr := target.InspectRoot(ctx, operationName)
			switch {
			case errors.Is(inspectErr, fsbind.ErrNotFound):
				// The prior invocation already removed the operation subtree.
				// Confirm the current parent-directory durability before the
				// root intent that authorizes this recovery can be erased.
				if syncErr := target.SyncRoot(ctx); syncErr != nil {
					return mapExecutionForgetError(&report, syncErr, "the recovered source retirement operation absence is not durable")
				}
				if _, confirmErr := target.InspectRoot(ctx, operationName); !errors.Is(confirmErr, fsbind.ErrNotFound) {
					if confirmErr == nil {
						confirmErr = fmt.Errorf("%w: the source retirement operation reappeared", ErrExecutionIntegrity)
					}
					return mapExecutionForgetError(&report, confirmErr, "the recovered source retirement operation absence changed")
				}
			case inspectErr != nil:
				return mapExecutionForgetError(&report, inspectErr, "the source retirement operation removal state is unavailable")
			case observed.Kind != fsbind.ObjectKindDirectory || !observed.Identity.Equal(expectedOperation):
				return mapExecutionForgetError(&report, fsbind.ErrUnsafeObject, "the source retirement operation residue changed identity")
			default:
				residue, residueErr := target.RemovePrivateSubtreeResidueExact(ctx, operationName, expectedOperation)
				report.Removals.OperationSubtree = residue
				report.recordExecutionForgetSubtreeRemoval(residue)
				if residueErr != nil {
					return mapExecutionForgetError(&report, residueErr, "the source retirement empty operation residue could not be removed")
				}
			}
		} else if err != nil {
			return mapExecutionForgetError(&report, err, "the source retirement operation could not be rebound for forgetting")
		} else {
			defer subtree.Close()
		}
	}
	if subtree != nil {
		if !rootIntentEnsured {
			ensuredID, receipt, ensuredObject, ensureErr := ensureExecutionForgetRootIntent(ctx, target, subtree, marker, options.Limits.MaxMarkerBytes)
			report.recordExecutionForgetMarker(receipt)
			if ensureErr != nil {
				return mapExecutionForgetError(&report, ensureErr, "the existing source retirement forget intent could not be rebound to its retained tombstone")
			}
			if ensuredID != markerID || !ensuredObject.Identity.Equal(rootObject.Identity) {
				return mapExecutionForgetError(&report, ErrExecutionIntegrity, "the existing source retirement forget intent changed while its tombstone was rebound")
			}
			rootObject = ensuredObject
			rootIntentEnsured = true
		}
		if err := removeExecutionTombstoneFromForgetIntent(ctx, target, subtree, marker, &report); err != nil {
			return mapExecutionForgetError(&report, err, "the source retirement retained tombstone could not be completely removed")
		}
		subtree = nil
	}
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "forgetting", "operation_removed", true
	if executionForgetTransitionHook != nil {
		if hookErr := executionForgetTransitionHook("operation_removed"); hookErr != nil {
			return mapExecutionForgetError(&report, hookErr, "source retirement forgetting stopped after operation removal")
		}
	}

	freshMarker, freshID, freshObject, _, freshErr := readExecutionForgetRootIntent(ctx, target, rootName, options.Limits.MaxMarkerBytes)
	if freshErr != nil || freshMarker != marker || freshID != markerID || !freshObject.Identity.Equal(rootObject.Identity) {
		if freshErr != nil {
			if errors.Is(freshErr, fsbind.ErrNotFound) {
				freshErr = fmt.Errorf("%w: the final source retirement forget marker disappeared", ErrExecutionIntegrity)
			}
			return mapExecutionForgetError(&report, freshErr, "the final source retirement forget marker read failed")
		}
		return mapExecutionForgetError(&report, ErrExecutionIntegrity, "the final source retirement forget marker changed")
	}
	if executionForgetTransitionHook != nil {
		if hookErr := executionForgetTransitionHook("before_root_intent_remove"); hookErr != nil {
			return mapExecutionForgetError(&report, hookErr, "source retirement forgetting stopped before last-marker removal")
		}
	}
	operationName, _ := OperationDirectoryName(options.OperationID)
	if _, absenceErr := target.InspectRoot(ctx, operationName); !errors.Is(absenceErr, fsbind.ErrNotFound) {
		if absenceErr == nil {
			absenceErr = fmt.Errorf("%w: the source retirement operation name reappeared", ErrExecutionIntegrity)
		}
		return mapExecutionForgetError(&report, absenceErr, "the source retirement operation is no longer absent before last-marker removal")
	}
	if syncErr := target.SyncRoot(ctx); syncErr != nil {
		return mapExecutionForgetError(&report, syncErr, "the source retirement operation absence is not durable before last-marker removal")
	}
	if _, absenceErr := target.InspectRoot(ctx, operationName); !errors.Is(absenceErr, fsbind.ErrNotFound) {
		if absenceErr == nil {
			absenceErr = fmt.Errorf("%w: the source retirement operation name reappeared", ErrExecutionIntegrity)
		}
		return mapExecutionForgetError(&report, absenceErr, "the source retirement operation absence changed before last-marker removal")
	}
	rootRemoval, removeErr := target.RemoveRootPrivateRegularExact(ctx, rootName, freshObject.Identity, freshObject.SizeBytes)
	report.Removals.RootIntent = rootRemoval
	report.recordExecutionForgetRemoval(rootRemoval)
	if rootRemoval.Attempted {
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "forget_completion_uncertain", "last_marker_removal_attempted", false
	}
	if removeErr != nil {
		if errors.Is(removeErr, fsbind.ErrNotFound) && !rootRemoval.Attempted {
			report.Outcome = ExecutionForgetOutcomeAbsentUnattributed
			report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "absent_unattributed", "last_marker_disappeared", false
			report.Authority.State = "absent_after_exact_read"
			report.Authority.MarkerDurable = false
			report.addExecutionForgetIssue("forget.absent_unattributed", "the exact last marker disappeared before this invocation attempted its removal; completion cannot be attributed")
			return report, ErrOperationNotFound
		}
		if rootRemoval.Removed {
			switch {
			case errors.Is(removeErr, fsbind.ErrRemovalAmbiguous):
				report.Authority.State = "last_marker_removal_ambiguous"
			case rootRemoval.Durability != fsbind.DurabilityConfirmed:
				report.Authority.State = "last_marker_removal_durability_unconfirmed"
			default:
				report.Authority.State = "last_marker_removed_binding_changed"
			}
			report.Authority.MarkerDurable = false
			report.WritesUncertain = true
		} else if rootRemoval.Attempted {
			report.Authority.State = "last_marker_removal_ambiguous"
			report.Authority.MarkerDurable = false
		}
		return mapExecutionForgetError(&report, removeErr, "the last source retirement forget marker removal was not confirmed")
	}
	if !rootRemoval.Removed || rootRemoval.Durability != fsbind.DurabilityConfirmed {
		report.Authority.State = "last_marker_removal_durability_unconfirmed"
		report.Authority.MarkerDurable = false
		return mapExecutionForgetError(&report, fsbind.ErrDurabilityUnconfirmed, "the last source retirement forget marker durability was not confirmed")
	}
	report.Outcome = ExecutionForgetOutcomeForgotten
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "forgotten", "forgotten", false
	report.Authority.State = "erased"
	report.Authority.MarkerDurable = false
	report.Authority.TargetHistoricalEvidenceErased = true
	report.Warnings = append(report.Warnings, "forget completed: no ptctl source-retirement history for this operation remains in the target root")
	return report, nil
}

func selectExecutionForgetMarker(marker ExecutionForgetIntent, options ExecutionForgetOptions, targetIdentity fsbind.Identity) error {
	if err := marker.Validate(); err != nil {
		return err
	}
	if marker.OperationID != options.OperationID || marker.PlanID != options.ExpectedPlanID || marker.TargetRootIdentity != targetIdentity.String() {
		return fmt.Errorf("%w: source retirement forget selector disagrees", ErrExecutionPolicy)
	}
	return nil
}

// applyExecutionForgetControl lets ordinary run, resume, and status observe
// the durable forget boundary without advancing it. Only the separately
// acknowledged ForgetExecutionTombstone operation may remove either the
// retained tombstone or the last root-level recovery marker.
func applyExecutionForgetControl(ctx context.Context, target *fsbind.Session, operation OperationID, report *ExecutionReport) (bool, error) {
	if target == nil || report == nil {
		return false, fmt.Errorf("%w: source retirement forget inspection authority is unavailable", ErrExecutionIntegrity)
	}
	rootName, err := ExecutionForgetRootName(operation)
	if err != nil {
		return false, fmt.Errorf("%w: source retirement forget selector is invalid", ErrExecutionPolicy)
	}
	marker, markerID, _, _, err := readExecutionForgetRootIntent(ctx, target, rootName, maximumExecutionForgetBytes)
	if errors.Is(err, fsbind.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if marker.OperationID != operation || marker.TargetRootIdentity != target.Info().Identity.String() {
		return true, fmt.Errorf("%w: source retirement forget marker is bound to another operation", ErrExecutionIntegrity)
	}
	report.addEffect("read_private_source_retirement_forget_intent")
	report.Files = []ExecutionFileReport{}
	report.Operation.ID = operation.String()
	report.Operation.PlanID = marker.PlanID
	report.Operation.IntentID = marker.RetentionIntent.IntentID
	report.Operation.CompletionID = marker.RetentionIntent.CompletionID
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "forgetting", "forget_intent_recorded", false
	report.Used.FilesConsidered = marker.RetentionIntent.FilesRetired
	report.Used.ContentBytes = marker.RetentionIntent.BytesRetired
	report.Outcome = ExecutionOutcomeIncomplete
	report.addBlocker("operation.forget_required", "a private forget intent is visible; only explicit source retirement forget may confirm durability and advance or recover historical evidence deletion")
	report.Warnings = append(report.Warnings,
		"source-retirement historical evidence is being forgotten under an explicit root-level recovery marker; read-only status does not reassert its durability",
		"forget marker "+markerID.String()+" is a stable pseudonym and not anonymization",
	)
	report.finalize()
	return true, nil
}

func populateExecutionForgetAuthority(report *ExecutionForgetReport, marker ExecutionForgetIntent, markerID ExecutionForgetMarkerID) {
	report.Target.ExpectedRootIdentity = marker.TargetRootIdentity
	report.Authority.MarkerID = markerID.String()
	report.Authority.RetentionIntentMarkerID = marker.RetentionIntentMarkerID.String()
	report.Authority.RetentionCompleteMarkerID = marker.RetentionCompleteMarkerID.String()
	report.Authority.ExactTombstoneEvidenceAvailable = true
	report.Proof = ExecutionRetentionProofReport{
		Basis: ExecutionRetentionBasisComplete, IntentID: marker.RetentionIntent.IntentID,
		TerminalCompletionID: marker.RetentionIntent.CompletionID,
		FilesRetired:         marker.RetentionIntent.FilesRetired, BytesRetired: marker.RetentionIntent.BytesRetired,
		HistoricalAuthority: true, Assurance: "exact_retention_tombstone_copied_into_durable_forget_intent",
	}
}

func auditInitialExecutionForgetTombstone(ctx context.Context, subtree *fsbind.Subtree, state executionRetentionState, marker ExecutionForgetIntent) error {
	if !state.IntentPresent || !state.CompletePresent || state.IntentID != marker.RetentionIntentMarkerID || state.CompleteID != marker.RetentionCompleteMarkerID ||
		state.Intent != marker.RetentionIntent || state.Complete != marker.RetentionComplete {
		return fmt.Errorf("%w: source retirement retained tombstone disagrees with forget intent", ErrExecutionIntegrity)
	}
	if err := auditExecutionRetentionHeavyAbsent(ctx, subtree); err != nil {
		return err
	}
	raw, markerID, err := EncodeExecutionForgetIntent(marker)
	if err != nil {
		return err
	}
	expectedPending := executionForgetPendingName(markerID)
	directory, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory})
	listing, err := subtree.List(ctx, directory, fsbind.ListLimits{MaxEntries: 4, MaxNameBytes: 1 << 20})
	if err != nil || !listing.Complete {
		if err != nil {
			return classifyExecutionBindingError(err)
		}
		return fmt.Errorf("%w: source retirement forget preflight inventory is incomplete", ErrExecutionIntegrity)
	}
	seenIntent, seenComplete := false, false
	for _, entry := range listing.Entries {
		switch entry.Name {
		case executionRetentionIntentFile:
			seenIntent = entry.Kind == string(fsbind.ObjectKindRegular)
		case executionRetentionCompleteFile:
			seenComplete = entry.Kind == string(fsbind.ObjectKindRegular)
		case expectedPending:
			if entry.Kind != string(fsbind.ObjectKindRegular) {
				return fmt.Errorf("%w: source retirement forget pending marker is unsafe", ErrExecutionIntegrity)
			}
			pending, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory, expectedPending})
			if _, verifyErr := verifyExecutionRetentionNamedBytes(ctx, subtree, pending, raw); verifyErr != nil {
				return verifyErr
			}
		default:
			return fmt.Errorf("%w: source retirement forget preflight contains an unexpected object", ErrExecutionIntegrity)
		}
	}
	if !seenIntent || !seenComplete {
		return fmt.Errorf("%w: source retirement retained tombstone is incomplete", ErrExecutionIntegrity)
	}
	intentRaw, intentID, intentErr := EncodeExecutionRetentionIntent(state.Intent)
	completeRaw, completeID, completeErr := EncodeExecutionRetentionComplete(state.Complete)
	if intentErr != nil || completeErr != nil || intentID != state.IntentID || completeID != state.CompleteID {
		return fmt.Errorf("%w: source retirement retained tombstone identity changed", ErrExecutionIntegrity)
	}
	intentPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory, executionRetentionIntentFile})
	if _, err := verifyExecutionRetentionNamedBytes(ctx, subtree, intentPath, intentRaw); err != nil {
		return err
	}
	completePath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory, executionRetentionCompleteFile})
	if _, err := verifyExecutionRetentionNamedBytes(ctx, subtree, completePath, completeRaw); err != nil {
		return err
	}
	after, err := subtree.List(ctx, directory, fsbind.ListLimits{MaxEntries: 4, MaxNameBytes: 1 << 20})
	if err != nil || !after.Complete || !sameExecutionRetentionEntries(listing.Entries, after.Entries) {
		if err != nil {
			return classifyExecutionBindingError(err)
		}
		return fmt.Errorf("%w: source retirement forget preflight changed during observation", ErrExecutionIntegrity)
	}
	return subtree.CheckPaths(fsbind.Path{}, directory)
}

func removeExecutionTombstoneFromForgetIntent(ctx context.Context, target *fsbind.Session, subtree *fsbind.Subtree, marker ExecutionForgetIntent, report *ExecutionForgetReport) error {
	if subtree == nil || !subtree.Identity().Equal(mustExecutionForgetIdentity(marker.OperationRootIdentity)) {
		return fmt.Errorf("%w: source retirement operation identity changed", ErrExecutionIntegrity)
	}
	directoryPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory})
	directory, directoryErr := subtree.Inspect(ctx, directoryPath)
	if errors.Is(directoryErr, fsbind.ErrNotFound) {
		return removeExecutionForgetOperationRoot(ctx, target, subtree, report)
	}
	if directoryErr != nil || directory.Kind != fsbind.ObjectKindDirectory {
		if directoryErr != nil {
			return classifyExecutionBindingError(directoryErr)
		}
		return fmt.Errorf("%w: source retirement retention directory is unsafe", ErrExecutionIntegrity)
	}
	listing, err := subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 3, MaxNameBytes: 1 << 20})
	if err != nil || !listing.Complete {
		if err != nil {
			return classifyExecutionBindingError(err)
		}
		return fmt.Errorf("%w: source retirement forget recovery inventory is incomplete", ErrExecutionIntegrity)
	}
	for _, entry := range listing.Entries {
		if entry.Kind != string(fsbind.ObjectKindRegular) || entry.Name != executionRetentionIntentFile && entry.Name != executionRetentionCompleteFile {
			return fmt.Errorf("%w: source retirement forget recovery contains an unexpected object", ErrExecutionIntegrity)
		}
	}
	completeRaw, completeID, completeErr := EncodeExecutionRetentionComplete(marker.RetentionComplete)
	intentRaw, intentID, intentErr := EncodeExecutionRetentionIntent(marker.RetentionIntent)
	if completeErr != nil || intentErr != nil || completeID != marker.RetentionCompleteMarkerID || intentID != marker.RetentionIntentMarkerID {
		return fmt.Errorf("%w: source retirement retained tombstone cannot be reproduced", ErrExecutionIntegrity)
	}
	for _, selected := range []struct {
		name string
		raw  []byte
		dst  *fsbind.Removal
	}{
		{name: executionRetentionCompleteFile, raw: completeRaw, dst: &report.Removals.RetentionComplete},
		{name: executionRetentionIntentFile, raw: intentRaw, dst: &report.Removals.RetentionIntent},
	} {
		path, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory, selected.name})
		object, inspectErr := subtree.Inspect(ctx, path)
		if errors.Is(inspectErr, fsbind.ErrNotFound) {
			continue
		}
		if inspectErr != nil || object.Kind != fsbind.ObjectKindRegular || object.SizeBytes != int64(len(selected.raw)) {
			if inspectErr != nil {
				return classifyExecutionBindingError(inspectErr)
			}
			return fmt.Errorf("%w: source retirement retained marker is unsafe", ErrExecutionIntegrity)
		}
		if identity, verifyErr := verifyExecutionRetentionNamedBytes(ctx, subtree, path, selected.raw); verifyErr != nil || !identity.Equal(object.Identity) {
			if verifyErr != nil {
				return verifyErr
			}
			return fmt.Errorf("%w: source retirement retained marker identity changed", ErrExecutionIntegrity)
		}
		removal, removeErr := subtree.RemoveRegularExact(ctx, path, object.Identity, object.SizeBytes)
		*selected.dst = removal
		report.recordExecutionForgetRemoval(removal)
		if removeErr != nil {
			return removeErr
		}
	}
	after, err := subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 1, MaxNameBytes: 1 << 10})
	if err != nil || !after.Complete || len(after.Entries) != 0 {
		if err != nil {
			return classifyExecutionBindingError(err)
		}
		return fmt.Errorf("%w: source retirement retention directory is not empty after marker removal", ErrExecutionIntegrity)
	}
	directoryRemoval, removeErr := subtree.RemoveEmptyDirectory(ctx, directoryPath, directory.Identity)
	report.Removals.RetentionDirectory = directoryRemoval
	report.recordExecutionForgetRemoval(directoryRemoval)
	if removeErr != nil {
		return removeErr
	}
	return removeExecutionForgetOperationRoot(ctx, target, subtree, report)
}

func removeExecutionForgetOperationRoot(ctx context.Context, target *fsbind.Session, subtree *fsbind.Subtree, report *ExecutionForgetReport) error {
	root, err := subtree.List(ctx, fsbind.Path{}, fsbind.ListLimits{MaxEntries: 2, MaxNameBytes: 1 << 10})
	if err != nil || !root.Complete || len(root.Entries) != 1 || root.Entries[0].Name != ".fsbind-operation.lock" || root.Entries[0].Kind != string(fsbind.ObjectKindRegular) {
		if err != nil {
			return classifyExecutionBindingError(err)
		}
		return fmt.Errorf("%w: source retirement operation root is not an exact lock-only namespace", ErrExecutionIntegrity)
	}
	removal, removeErr := target.RemovePrivateSubtreeExact(ctx, subtree)
	report.Removals.OperationSubtree = removal
	report.recordExecutionForgetSubtreeRemoval(removal)
	return removeErr
}

func mustExecutionForgetIdentity(value string) fsbind.Identity {
	identity, _ := fsbind.ParseIdentity(value)
	return identity
}

func (report *ExecutionForgetReport) recordExecutionForgetMarker(receipt executionForgetMarkerReceipt) {
	if receipt.Publication.Attempted || receipt.Publication.Published {
		report.RootIntentPublication = receipt.Publication
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
	if receipt.TemporaryRemoval.Attempted {
		report.Removals.MarkerTemporary = receipt.TemporaryRemoval
		report.recordExecutionForgetRemoval(receipt.TemporaryRemoval)
	}
}

func (report *ExecutionForgetReport) recordExecutionForgetRemoval(removal fsbind.Removal) {
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
		if removal.Durability != fsbind.DurabilityConfirmed {
			report.WritesUncertain = true
		}
	}
}

func (report *ExecutionForgetReport) recordExecutionForgetSubtreeRemoval(removal fsbind.PrivateSubtreeRemoval) {
	if removal.LockRemovalAttempted {
		report.Writes.RemovalAttempts++
	}
	if removal.DirectoryAttempted {
		report.Writes.RemovalAttempts++
	}
	if removal.LockRemoved {
		report.WritesPerformed++
		report.Writes.FilesRemoved++
	}
	if removal.Removed {
		report.WritesPerformed++
		report.Writes.DirectoriesRemoved++
		if removal.Durability != fsbind.DurabilityConfirmed {
			report.WritesUncertain = true
		}
	}
}

func (report *ExecutionForgetReport) addExecutionForgetBlocker(code, message string) {
	report.Blockers = append(report.Blockers, Finding{Code: code, Message: message})
}

func (report *ExecutionForgetReport) addExecutionForgetIssue(code, message string) {
	report.Issues = append(report.Issues, Finding{Code: code, Message: message})
}

func executionForgetBlocked(report *ExecutionForgetReport, code, message string) (ExecutionForgetReport, error) {
	report.Outcome = ExecutionForgetOutcomeBlocked
	report.addExecutionForgetBlocker(code, message)
	return *report, nil
}

func executionForgetInterrupted(report *ExecutionForgetReport, err error, message string) (ExecutionForgetReport, error) {
	report.Outcome = ExecutionForgetOutcomeInterrupted
	report.addExecutionForgetIssue("forget.interrupted", message)
	return *report, err
}

func mapExecutionForgetError(report *ExecutionForgetReport, err error, message string) (ExecutionForgetReport, error) {
	if errors.Is(err, fsbind.ErrPublicationAmbiguous) {
		report.WritesUncertain = true
		report.Writes.AmbiguousMarkerPublications++
	}
	if errors.Is(err, fsbind.ErrRemovalAmbiguous) {
		report.WritesUncertain = true
		report.Writes.AmbiguousRemovals++
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return executionForgetInterrupted(report, err, message)
	case errors.Is(err, ErrExecutionPolicy), errors.Is(err, fsbind.ErrUnsupported), errors.Is(err, fsbind.ErrBusy), errors.Is(err, fsbind.ErrAlreadyExists):
		report.Outcome = ExecutionForgetOutcomeBlocked
		report.addExecutionForgetBlocker("forget.policy_blocked", message)
		return *report, err
	case errors.Is(err, ErrExecutionIntegrity), errors.Is(err, fsbind.ErrUnsafeObject), errors.Is(err, fsbind.ErrBindingChanged), errors.Is(err, fsbind.ErrCrossFilesystem):
		report.Outcome = ExecutionForgetOutcomeIntegrityFailed
		report.Operation.Resumable = false
		report.addExecutionForgetIssue("forget.integrity_failed", message)
		return *report, fmt.Errorf("%w: %s", ErrExecutionIntegrity, message)
	case errors.Is(err, fsbind.ErrDurabilityUnconfirmed):
		report.Outcome = ExecutionForgetOutcomeDurabilityUnconfirmed
		report.WritesUncertain = true
		report.addExecutionForgetIssue("forget.durability_unconfirmed", message)
		return *report, err
	default:
		report.Outcome = ExecutionForgetOutcomeInterrupted
		report.addExecutionForgetIssue("forget.operation_failed", message)
		return *report, err
	}
}
