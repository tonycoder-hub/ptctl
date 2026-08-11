package sourceretire

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

var parentCleanupForgetTransitionHook func(string) error

// ForgetParentCleanupTombstone irreversibly removes one explicitly selected,
// already-pruned parent-cleanup tombstone. A private root-level intent is
// durably published first and remains the only recovery authority until the
// operation subtree is durably absent. The intent is then removed last.
func ForgetParentCleanupTombstone(ctx context.Context, options ParentCleanupForgetOptions) (ParentCleanupForgetReport, error) {
	report := newParentCleanupForgetReport(options)
	if err := options.Limits.Validate(); err != nil {
		return parentCleanupForgetBlocked(&report, "limits.invalid", "parent cleanup forget limits are invalid")
	}
	if !options.Acknowledge {
		return parentCleanupForgetBlocked(&report, "acknowledgement.required", "historical tombstone deletion requires its dedicated acknowledgement")
	}
	derived, derivedErr := ParentCleanupOperationIDForPlanID(options.ExpectedCleanupPlanID)
	if options.TargetRoot == "" || derivedErr != nil || derived != options.OperationID {
		return parentCleanupForgetBlocked(&report, "forget.selector_invalid", "target root, operation ID, or reviewed plan ID is invalid")
	}
	if err := ctx.Err(); err != nil {
		return parentCleanupForgetInterrupted(&report, err, "parent cleanup forget was interrupted before target observation")
	}
	absolute, err := filepath.Abs(options.TargetRoot)
	if err != nil {
		return parentCleanupForgetBlocked(&report, "target.invalid_root", "the parent cleanup target root is invalid")
	}
	target, rootInfo, err := fsbind.BindExisting(filepath.Clean(absolute))
	if err != nil {
		return mapParentCleanupForgetError(&report, err, "the target root cannot provide bound tombstone-forget semantics")
	}
	defer target.Close()
	report.Target.ObservedRootIdentity = rootInfo.Identity.String()
	report.Target.RootIdentityBound = true
	report.Target.StabilityAssurance = "non_atomic_bound_filesystem"

	rootName, _ := ParentCleanupForgetRootName(options.OperationID)
	marker, markerID, rootObject, _, markerErr := readParentCleanupForgetRootIntent(ctx, target, rootName, options.Limits.MaxMarkerBytes)
	var subtree *fsbind.Subtree
	rootIntentEnsured := false
	if errors.Is(markerErr, fsbind.ErrNotFound) {
		operationName, _ := ParentCleanupOperationDirectoryName(options.OperationID)
		subtree, err = target.OpenPrivateSubtreeObserved(operationName)
		if errors.Is(err, fsbind.ErrNotFound) {
			report.Outcome = ParentCleanupForgetOutcomeAbsentUnattributed
			report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "absent_unattributed", "absent", false
			report.Authority.State = "absent"
			report.addParentCleanupForgetIssue("forget.absent_unattributed", "neither the retained operation nor a durable forget intent exists; prior erasure cannot be distinguished from an unknown selector")
			return report, ErrOperationNotFound
		}
		if err != nil {
			return mapParentCleanupForgetError(&report, err, "the retained parent cleanup operation could not be opened")
		}
		defer subtree.Close()
		state, loadErr := loadParentCleanupRetentionState(ctx, subtree, options.OperationID, rootInfo.Identity)
		if loadErr != nil {
			return mapParentCleanupForgetError(&report, loadErr, "the retained parent cleanup tombstone could not be loaded")
		}
		if !state.IntentPresent || !state.CompletePresent {
			return parentCleanupForgetBlocked(&report, "operation.prune_required", "only a complete retained tombstone may be forgotten")
		}
		marker = ParentCleanupForgetIntent{
			Schema: ParentCleanupForgetIntentSchemaV1, OperationID: options.OperationID,
			OperationRootIdentity: subtree.Identity().String(), TargetRootIdentity: rootInfo.Identity.String(),
			CleanupPlanID: options.ExpectedCleanupPlanID, RetentionIntentMarkerID: state.IntentID,
			RetentionCompleteMarkerID: state.CompleteID, RetentionIntent: state.Intent,
			RetentionComplete: state.Complete, Basis: ParentCleanupForgetBasisExact,
		}
		if err := marker.Validate(); err != nil {
			return mapParentCleanupForgetError(&report, err, "the retained parent cleanup tombstone cannot authorize forgetting")
		}
		if err := auditInitialParentCleanupForgetTombstone(ctx, subtree, state, marker); err != nil {
			return mapParentCleanupForgetError(&report, err, "the parent cleanup tombstone is not exact before forget intent publication")
		}
		preparedRaw, preparedID, prepareErr := EncodeParentCleanupForgetIntent(marker)
		if prepareErr != nil {
			return mapParentCleanupForgetError(&report, prepareErr, "the parent cleanup forget intent cannot be encoded")
		}
		if int64(len(preparedRaw)) > options.Limits.MaxMarkerBytes {
			return mapParentCleanupForgetError(&report, ErrExecutionPolicy, "the parent cleanup forget marker byte budget is insufficient")
		}
		markerID = preparedID
		populateParentCleanupForgetAuthority(&report, marker, markerID)
		report.Authority.State = "exact_tombstone_observed"
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "retained", "retained", false
		markerID, receipt, object, publishErr := ensureParentCleanupForgetRootIntent(ctx, target, subtree, marker, options.Limits.MaxMarkerBytes)
		report.recordParentCleanupForgetMarker(receipt)
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
			return mapParentCleanupForgetError(&report, publishErr, "the parent cleanup forget intent could not be durably published")
		}
		rootIntentEnsured = true
	} else if markerErr != nil {
		return mapParentCleanupForgetError(&report, markerErr, "the parent cleanup forget intent could not be read")
	} else if err := selectParentCleanupForgetMarker(marker, options, rootInfo.Identity); err != nil {
		return mapParentCleanupForgetError(&report, err, "the parent cleanup forget intent does not match the explicit selector")
	} else {
		populateParentCleanupForgetAuthority(&report, marker, markerID)
		report.Authority.State = "root_intent_visible_durability_unconfirmed"
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "forgetting", "root_intent_visible", true
		if err := target.SyncRoot(ctx); err != nil {
			return mapParentCleanupForgetError(&report, err, "the visible parent cleanup forget intent durability could not be confirmed")
		}
		fresh, freshID, freshObject, _, freshErr := readParentCleanupForgetRootIntent(ctx, target, rootName, options.Limits.MaxMarkerBytes)
		if freshErr != nil || fresh != marker || freshID != markerID || !freshObject.Identity.Equal(rootObject.Identity) {
			if freshErr != nil {
				return mapParentCleanupForgetError(&report, freshErr, "the parent cleanup forget intent could not be re-read after durability confirmation")
			}
			return mapParentCleanupForgetError(&report, ErrExecutionIntegrity, "the parent cleanup forget intent changed during durability confirmation")
		}
		rootObject = freshObject
	}

	if markerID == "" {
		_, derivedMarkerID, encodeErr := EncodeParentCleanupForgetIntent(marker)
		if encodeErr != nil {
			return mapParentCleanupForgetError(&report, encodeErr, "the parent cleanup forget intent cannot be reproduced")
		}
		markerID = derivedMarkerID
	}
	populateParentCleanupForgetAuthority(&report, marker, markerID)
	report.Authority.MarkerDurable = true
	report.Authority.State = "root_intent_published"
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "forgetting", "root_intent_published", true
	if parentCleanupForgetTransitionHook != nil {
		if hookErr := parentCleanupForgetTransitionHook("root_intent_published"); hookErr != nil {
			return mapParentCleanupForgetError(&report, hookErr, "parent cleanup forgetting stopped after its durable root intent")
		}
	}

	if subtree == nil {
		operationName, _ := ParentCleanupOperationDirectoryName(options.OperationID)
		expectedOperation, parseErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
		if parseErr != nil {
			return mapParentCleanupForgetError(&report, parseErr, "the parent cleanup forget operation identity is invalid")
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
					return mapParentCleanupForgetError(&report, syncErr, "the recovered parent cleanup operation absence is not durable")
				}
				if _, confirmErr := target.InspectRoot(ctx, operationName); !errors.Is(confirmErr, fsbind.ErrNotFound) {
					if confirmErr == nil {
						confirmErr = fmt.Errorf("%w: the parent cleanup operation reappeared", ErrExecutionIntegrity)
					}
					return mapParentCleanupForgetError(&report, confirmErr, "the recovered parent cleanup operation absence changed")
				}
			case inspectErr != nil:
				return mapParentCleanupForgetError(&report, inspectErr, "the parent cleanup operation removal state is unavailable")
			case observed.Kind != fsbind.ObjectKindDirectory || !observed.Identity.Equal(expectedOperation):
				return mapParentCleanupForgetError(&report, fsbind.ErrUnsafeObject, "the parent cleanup operation residue changed identity")
			default:
				residue, residueErr := target.RemovePrivateSubtreeResidueExact(ctx, operationName, expectedOperation)
				report.Removals.OperationSubtree = residue
				report.recordParentCleanupForgetSubtreeRemoval(residue)
				if residueErr != nil {
					return mapParentCleanupForgetError(&report, residueErr, "the parent cleanup empty operation residue could not be removed")
				}
			}
		} else if err != nil {
			return mapParentCleanupForgetError(&report, err, "the parent cleanup operation could not be rebound for forgetting")
		} else {
			defer subtree.Close()
		}
	}
	if subtree != nil {
		if !rootIntentEnsured {
			ensuredID, receipt, ensuredObject, ensureErr := ensureParentCleanupForgetRootIntent(ctx, target, subtree, marker, options.Limits.MaxMarkerBytes)
			report.recordParentCleanupForgetMarker(receipt)
			if ensureErr != nil {
				return mapParentCleanupForgetError(&report, ensureErr, "the existing parent cleanup forget intent could not be rebound to its retained tombstone")
			}
			if ensuredID != markerID || !ensuredObject.Identity.Equal(rootObject.Identity) {
				return mapParentCleanupForgetError(&report, ErrExecutionIntegrity, "the existing parent cleanup forget intent changed while its tombstone was rebound")
			}
			rootObject = ensuredObject
			rootIntentEnsured = true
		}
		if err := removeParentCleanupTombstoneFromForgetIntent(ctx, target, subtree, marker, &report); err != nil {
			return mapParentCleanupForgetError(&report, err, "the parent cleanup retained tombstone could not be completely removed")
		}
		subtree = nil
	}
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "forgetting", "operation_removed", true
	if parentCleanupForgetTransitionHook != nil {
		if hookErr := parentCleanupForgetTransitionHook("operation_removed"); hookErr != nil {
			return mapParentCleanupForgetError(&report, hookErr, "parent cleanup forgetting stopped after operation removal")
		}
	}

	freshMarker, freshID, freshObject, _, freshErr := readParentCleanupForgetRootIntent(ctx, target, rootName, options.Limits.MaxMarkerBytes)
	if freshErr != nil || freshMarker != marker || freshID != markerID || !freshObject.Identity.Equal(rootObject.Identity) {
		if freshErr != nil {
			if errors.Is(freshErr, fsbind.ErrNotFound) {
				freshErr = fmt.Errorf("%w: the final parent cleanup forget marker disappeared", ErrExecutionIntegrity)
			}
			return mapParentCleanupForgetError(&report, freshErr, "the final parent cleanup forget marker read failed")
		}
		return mapParentCleanupForgetError(&report, ErrExecutionIntegrity, "the final parent cleanup forget marker changed")
	}
	if parentCleanupForgetTransitionHook != nil {
		if hookErr := parentCleanupForgetTransitionHook("before_root_intent_remove"); hookErr != nil {
			return mapParentCleanupForgetError(&report, hookErr, "parent cleanup forgetting stopped before last-marker removal")
		}
	}
	operationName, _ := ParentCleanupOperationDirectoryName(options.OperationID)
	if _, absenceErr := target.InspectRoot(ctx, operationName); !errors.Is(absenceErr, fsbind.ErrNotFound) {
		if absenceErr == nil {
			absenceErr = fmt.Errorf("%w: the parent cleanup operation name reappeared", ErrExecutionIntegrity)
		}
		return mapParentCleanupForgetError(&report, absenceErr, "the parent cleanup operation is no longer absent before last-marker removal")
	}
	if syncErr := target.SyncRoot(ctx); syncErr != nil {
		return mapParentCleanupForgetError(&report, syncErr, "the parent cleanup operation absence is not durable before last-marker removal")
	}
	if _, absenceErr := target.InspectRoot(ctx, operationName); !errors.Is(absenceErr, fsbind.ErrNotFound) {
		if absenceErr == nil {
			absenceErr = fmt.Errorf("%w: the parent cleanup operation name reappeared", ErrExecutionIntegrity)
		}
		return mapParentCleanupForgetError(&report, absenceErr, "the parent cleanup operation absence changed before last-marker removal")
	}
	rootRemoval, removeErr := target.RemoveRootPrivateRegularExact(ctx, rootName, freshObject.Identity, freshObject.SizeBytes)
	report.Removals.RootIntent = rootRemoval
	report.recordParentCleanupForgetRemoval(rootRemoval)
	if rootRemoval.Attempted {
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "forget_completion_uncertain", "last_marker_removal_attempted", false
	}
	if removeErr != nil {
		if errors.Is(removeErr, fsbind.ErrNotFound) && !rootRemoval.Attempted {
			report.Outcome = ParentCleanupForgetOutcomeAbsentUnattributed
			report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "absent_unattributed", "last_marker_disappeared", false
			report.Authority.State = "absent_after_exact_read"
			report.Authority.MarkerDurable = false
			report.addParentCleanupForgetIssue("forget.absent_unattributed", "the exact last marker disappeared before this invocation attempted its removal; completion cannot be attributed")
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
		return mapParentCleanupForgetError(&report, removeErr, "the last parent cleanup forget marker removal was not confirmed")
	}
	if !rootRemoval.Removed || rootRemoval.Durability != fsbind.DurabilityConfirmed {
		report.Authority.State = "last_marker_removal_durability_unconfirmed"
		report.Authority.MarkerDurable = false
		return mapParentCleanupForgetError(&report, fsbind.ErrDurabilityUnconfirmed, "the last parent cleanup forget marker durability was not confirmed")
	}
	report.Outcome = ParentCleanupForgetOutcomeForgotten
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "forgotten", "forgotten", false
	report.Authority.State = "erased"
	report.Authority.MarkerDurable = false
	report.Authority.TargetHistoricalEvidenceErased = true
	report.Warnings = append(report.Warnings, "forget completed: no ptctl parent-cleanup history for this operation remains in the target root")
	return report, nil
}

func selectParentCleanupForgetMarker(marker ParentCleanupForgetIntent, options ParentCleanupForgetOptions, targetIdentity fsbind.Identity) error {
	if err := marker.Validate(); err != nil {
		return err
	}
	if marker.OperationID != options.OperationID || marker.CleanupPlanID != options.ExpectedCleanupPlanID || marker.TargetRootIdentity != targetIdentity.String() {
		return fmt.Errorf("%w: parent cleanup forget selector disagrees", ErrExecutionPolicy)
	}
	return nil
}

// applyParentCleanupForgetControl lets ordinary run, resume, and status observe
// the durable forget boundary without advancing it. Only the separately
// acknowledged ForgetParentCleanupTombstone operation may remove either the
// retained tombstone or the last root-level recovery marker.
func applyParentCleanupForgetControl(ctx context.Context, target *fsbind.Session, operation ParentCleanupOperationID, report *ParentCleanupExecutionReport) (bool, error) {
	if target == nil || report == nil {
		return false, fmt.Errorf("%w: parent cleanup forget inspection authority is unavailable", ErrExecutionIntegrity)
	}
	return inspectParentCleanupForgetControl(ctx, target, operation, "", report)
}

func inspectParentCleanupForgetControl(ctx context.Context, target *fsbind.Session, operation ParentCleanupOperationID, expectedCleanupPlanID string, report *ParentCleanupExecutionReport) (bool, error) {
	if target == nil {
		return false, fmt.Errorf("%w: parent cleanup forget inspection authority is unavailable", ErrExecutionIntegrity)
	}
	rootName, err := ParentCleanupForgetRootName(operation)
	if err != nil {
		return false, fmt.Errorf("%w: parent cleanup forget selector is invalid", ErrExecutionPolicy)
	}
	marker, markerID, _, _, err := readParentCleanupForgetRootIntent(ctx, target, rootName, maximumParentCleanupForgetBytes)
	if errors.Is(err, fsbind.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if marker.OperationID != operation || marker.TargetRootIdentity != target.Info().Identity.String() {
		return true, fmt.Errorf("%w: parent cleanup forget marker is bound to another operation", ErrExecutionIntegrity)
	}
	if expectedCleanupPlanID != "" && marker.CleanupPlanID != expectedCleanupPlanID {
		return true, fmt.Errorf("%w: parent cleanup forget marker disagrees with the reviewed cleanup plan", ErrExecutionPolicy)
	}
	if report == nil {
		return true, nil
	}
	report.addEffect("read_private_parent_cleanup_forget_intent")
	report.Directories = []ParentCleanupExecutionDirectoryReport{}
	report.Target.ExpectedRootIdentity = marker.TargetRootIdentity
	report.Operation.ID = operation.String()
	report.Operation.PlanID = marker.CleanupPlanID
	report.Operation.IntentID = marker.RetentionIntent.IntentID
	report.Operation.CompletionID = marker.RetentionIntent.CompletionID
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "forgetting", "forget_intent_recorded", false
	report.RetirementOperationID = marker.RetentionIntent.RetirementOperationID.String()
	report.RetirementPlanID = marker.RetentionIntent.RetirementPlanID
	report.RetirementCompletionID = marker.RetentionIntent.RetirementCompletionID
	report.SearchScopeID = marker.RetentionIntent.SearchScopeID
	report.Used.ParentsConsidered = marker.RetentionIntent.ParentsRemoved
	report.Outcome = ParentCleanupExecutionOutcomeIncomplete
	report.addBlocker("operation.forget_required", "a private forget intent is visible; only explicit parent cleanup forget may confirm durability and advance or recover historical evidence deletion")
	report.Warnings = append(report.Warnings,
		"parent-cleanup historical evidence is being forgotten under an explicit root-level recovery marker; read-only status does not reassert its durability",
		"forget marker "+markerID.String()+" is a stable pseudonym and not anonymization",
	)
	report.finalize()
	return true, nil
}

func populateParentCleanupForgetAuthority(report *ParentCleanupForgetReport, marker ParentCleanupForgetIntent, markerID ParentCleanupForgetMarkerID) {
	report.Target.ExpectedRootIdentity = marker.TargetRootIdentity
	report.Authority.MarkerID = markerID.String()
	report.Authority.RetentionIntentMarkerID = marker.RetentionIntentMarkerID.String()
	report.Authority.RetentionCompleteMarkerID = marker.RetentionCompleteMarkerID.String()
	report.Authority.ExactTombstoneEvidenceAvailable = true
	report.Proof = ParentCleanupRetentionProofReport{
		Basis: ParentCleanupRetentionBasisComplete, IntentID: marker.RetentionIntent.IntentID,
		TerminalCompletionID:   marker.RetentionIntent.CompletionID,
		RetirementOperationID:  marker.RetentionIntent.RetirementOperationID.String(),
		RetirementPlanID:       marker.RetentionIntent.RetirementPlanID,
		RetirementCompletionID: marker.RetentionIntent.RetirementCompletionID,
		SearchScopeID:          marker.RetentionIntent.SearchScopeID,
		ParentsRemoved:         marker.RetentionIntent.ParentsRemoved,
		RetiredFiles:           marker.RetentionIntent.RetiredFiles,
		HistoricalAuthority:    true,
		Assurance:              "exact_retention_tombstone_copied_into_durable_forget_intent",
	}
}

func auditInitialParentCleanupForgetTombstone(ctx context.Context, subtree *fsbind.Subtree, state parentCleanupRetentionState, marker ParentCleanupForgetIntent) error {
	if !state.IntentPresent || !state.CompletePresent || state.IntentID != marker.RetentionIntentMarkerID || state.CompleteID != marker.RetentionCompleteMarkerID ||
		state.Intent != marker.RetentionIntent || state.Complete != marker.RetentionComplete {
		return fmt.Errorf("%w: parent cleanup retained tombstone disagrees with forget intent", ErrExecutionIntegrity)
	}
	if err := auditParentCleanupRetentionHeavyAbsent(ctx, subtree); err != nil {
		return err
	}
	raw, markerID, err := EncodeParentCleanupForgetIntent(marker)
	if err != nil {
		return err
	}
	expectedPending := parentCleanupForgetPendingName(markerID)
	directory, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory})
	listing, err := subtree.List(ctx, directory, fsbind.ListLimits{MaxEntries: 4, MaxNameBytes: 1 << 20})
	if err != nil || !listing.Complete {
		if err != nil {
			return classifyExecutionBindingError(err)
		}
		return fmt.Errorf("%w: parent cleanup forget preflight inventory is incomplete", ErrExecutionIntegrity)
	}
	seenIntent, seenComplete := false, false
	for _, entry := range listing.Entries {
		switch entry.Name {
		case parentCleanupRetentionIntentFile:
			seenIntent = entry.Kind == string(fsbind.ObjectKindRegular)
		case parentCleanupRetentionCompleteFile:
			seenComplete = entry.Kind == string(fsbind.ObjectKindRegular)
		case expectedPending:
			if entry.Kind != string(fsbind.ObjectKindRegular) {
				return fmt.Errorf("%w: parent cleanup forget pending marker is unsafe", ErrExecutionIntegrity)
			}
			pending, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory, expectedPending})
			if _, verifyErr := verifyParentCleanupRetentionNamedBytes(ctx, subtree, pending, raw); verifyErr != nil {
				return verifyErr
			}
		default:
			return fmt.Errorf("%w: parent cleanup forget preflight contains an unexpected object", ErrExecutionIntegrity)
		}
	}
	if !seenIntent || !seenComplete {
		return fmt.Errorf("%w: parent cleanup retained tombstone is incomplete", ErrExecutionIntegrity)
	}
	intentRaw, intentID, intentErr := EncodeParentCleanupRetentionIntent(state.Intent)
	completeRaw, completeID, completeErr := EncodeParentCleanupRetentionComplete(state.Complete)
	if intentErr != nil || completeErr != nil || intentID != state.IntentID || completeID != state.CompleteID {
		return fmt.Errorf("%w: parent cleanup retained tombstone identity changed", ErrExecutionIntegrity)
	}
	intentPath, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory, parentCleanupRetentionIntentFile})
	if _, err := verifyParentCleanupRetentionNamedBytes(ctx, subtree, intentPath, intentRaw); err != nil {
		return err
	}
	completePath, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory, parentCleanupRetentionCompleteFile})
	if _, err := verifyParentCleanupRetentionNamedBytes(ctx, subtree, completePath, completeRaw); err != nil {
		return err
	}
	after, err := subtree.List(ctx, directory, fsbind.ListLimits{MaxEntries: 4, MaxNameBytes: 1 << 20})
	if err != nil || !after.Complete || !sameExecutionRetentionEntries(listing.Entries, after.Entries) {
		if err != nil {
			return classifyExecutionBindingError(err)
		}
		return fmt.Errorf("%w: parent cleanup forget preflight changed during observation", ErrExecutionIntegrity)
	}
	return subtree.CheckPaths(fsbind.Path{}, directory)
}

func removeParentCleanupTombstoneFromForgetIntent(ctx context.Context, target *fsbind.Session, subtree *fsbind.Subtree, marker ParentCleanupForgetIntent, report *ParentCleanupForgetReport) error {
	if subtree == nil || !subtree.Identity().Equal(mustParentCleanupForgetIdentity(marker.OperationRootIdentity)) {
		return fmt.Errorf("%w: parent cleanup operation identity changed", ErrExecutionIntegrity)
	}
	directoryPath, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory})
	directory, directoryErr := subtree.Inspect(ctx, directoryPath)
	if errors.Is(directoryErr, fsbind.ErrNotFound) {
		return removeParentCleanupForgetOperationRoot(ctx, target, subtree, report)
	}
	if directoryErr != nil || directory.Kind != fsbind.ObjectKindDirectory {
		if directoryErr != nil {
			return classifyExecutionBindingError(directoryErr)
		}
		return fmt.Errorf("%w: parent cleanup retention directory is unsafe", ErrExecutionIntegrity)
	}
	listing, err := subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 3, MaxNameBytes: 1 << 20})
	if err != nil || !listing.Complete {
		if err != nil {
			return classifyExecutionBindingError(err)
		}
		return fmt.Errorf("%w: parent cleanup forget recovery inventory is incomplete", ErrExecutionIntegrity)
	}
	for _, entry := range listing.Entries {
		if entry.Kind != string(fsbind.ObjectKindRegular) || entry.Name != parentCleanupRetentionIntentFile && entry.Name != parentCleanupRetentionCompleteFile {
			return fmt.Errorf("%w: parent cleanup forget recovery contains an unexpected object", ErrExecutionIntegrity)
		}
	}
	completeRaw, completeID, completeErr := EncodeParentCleanupRetentionComplete(marker.RetentionComplete)
	intentRaw, intentID, intentErr := EncodeParentCleanupRetentionIntent(marker.RetentionIntent)
	if completeErr != nil || intentErr != nil || completeID != marker.RetentionCompleteMarkerID || intentID != marker.RetentionIntentMarkerID {
		return fmt.Errorf("%w: parent cleanup retained tombstone cannot be reproduced", ErrExecutionIntegrity)
	}
	for _, selected := range []struct {
		name string
		raw  []byte
		dst  *fsbind.Removal
	}{
		{name: parentCleanupRetentionCompleteFile, raw: completeRaw, dst: &report.Removals.RetentionComplete},
		{name: parentCleanupRetentionIntentFile, raw: intentRaw, dst: &report.Removals.RetentionIntent},
	} {
		path, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory, selected.name})
		object, inspectErr := subtree.Inspect(ctx, path)
		if errors.Is(inspectErr, fsbind.ErrNotFound) {
			continue
		}
		if inspectErr != nil || object.Kind != fsbind.ObjectKindRegular || object.SizeBytes != int64(len(selected.raw)) {
			if inspectErr != nil {
				return classifyExecutionBindingError(inspectErr)
			}
			return fmt.Errorf("%w: parent cleanup retained marker is unsafe", ErrExecutionIntegrity)
		}
		if identity, verifyErr := verifyParentCleanupRetentionNamedBytes(ctx, subtree, path, selected.raw); verifyErr != nil || !identity.Equal(object.Identity) {
			if verifyErr != nil {
				return verifyErr
			}
			return fmt.Errorf("%w: parent cleanup retained marker identity changed", ErrExecutionIntegrity)
		}
		removal, removeErr := subtree.RemoveRegularExact(ctx, path, object.Identity, object.SizeBytes)
		*selected.dst = removal
		report.recordParentCleanupForgetRemoval(removal)
		if removeErr != nil {
			return removeErr
		}
	}
	after, err := subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 1, MaxNameBytes: 1 << 10})
	if err != nil || !after.Complete || len(after.Entries) != 0 {
		if err != nil {
			return classifyExecutionBindingError(err)
		}
		return fmt.Errorf("%w: parent cleanup retention directory is not empty after marker removal", ErrExecutionIntegrity)
	}
	directoryRemoval, removeErr := subtree.RemoveEmptyDirectory(ctx, directoryPath, directory.Identity)
	report.Removals.RetentionDirectory = directoryRemoval
	report.recordParentCleanupForgetRemoval(directoryRemoval)
	if removeErr != nil {
		return removeErr
	}
	return removeParentCleanupForgetOperationRoot(ctx, target, subtree, report)
}

func removeParentCleanupForgetOperationRoot(ctx context.Context, target *fsbind.Session, subtree *fsbind.Subtree, report *ParentCleanupForgetReport) error {
	root, err := subtree.List(ctx, fsbind.Path{}, fsbind.ListLimits{MaxEntries: 2, MaxNameBytes: 1 << 10})
	if err != nil || !root.Complete || len(root.Entries) != 1 || root.Entries[0].Name != ".fsbind-operation.lock" || root.Entries[0].Kind != string(fsbind.ObjectKindRegular) {
		if err != nil {
			return classifyExecutionBindingError(err)
		}
		return fmt.Errorf("%w: parent cleanup operation root is not an exact lock-only namespace", ErrExecutionIntegrity)
	}
	removal, removeErr := target.RemovePrivateSubtreeExact(ctx, subtree)
	report.Removals.OperationSubtree = removal
	report.recordParentCleanupForgetSubtreeRemoval(removal)
	return removeErr
}

func mustParentCleanupForgetIdentity(value string) fsbind.Identity {
	identity, _ := fsbind.ParseIdentity(value)
	return identity
}

func (report *ParentCleanupForgetReport) recordParentCleanupForgetMarker(receipt parentCleanupForgetMarkerReceipt) {
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
		report.recordParentCleanupForgetRemoval(receipt.TemporaryRemoval)
	}
}

func (report *ParentCleanupForgetReport) recordParentCleanupForgetRemoval(removal fsbind.Removal) {
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

func (report *ParentCleanupForgetReport) recordParentCleanupForgetSubtreeRemoval(removal fsbind.PrivateSubtreeRemoval) {
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

func (report *ParentCleanupForgetReport) addParentCleanupForgetBlocker(code, message string) {
	report.Blockers = append(report.Blockers, Finding{Code: code, Message: message})
}

func (report *ParentCleanupForgetReport) addParentCleanupForgetIssue(code, message string) {
	report.Issues = append(report.Issues, Finding{Code: code, Message: message})
}

func parentCleanupForgetBlocked(report *ParentCleanupForgetReport, code, message string) (ParentCleanupForgetReport, error) {
	report.Outcome = ParentCleanupForgetOutcomeBlocked
	report.addParentCleanupForgetBlocker(code, message)
	return *report, nil
}

func parentCleanupForgetInterrupted(report *ParentCleanupForgetReport, err error, message string) (ParentCleanupForgetReport, error) {
	report.Outcome = ParentCleanupForgetOutcomeInterrupted
	report.addParentCleanupForgetIssue("forget.interrupted", message)
	return *report, err
}

func mapParentCleanupForgetError(report *ParentCleanupForgetReport, err error, message string) (ParentCleanupForgetReport, error) {
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
		return parentCleanupForgetInterrupted(report, err, message)
	case errors.Is(err, ErrExecutionPolicy), errors.Is(err, fsbind.ErrUnsupported), errors.Is(err, fsbind.ErrBusy), errors.Is(err, fsbind.ErrAlreadyExists):
		report.Outcome = ParentCleanupForgetOutcomeBlocked
		report.addParentCleanupForgetBlocker("forget.policy_blocked", message)
		return *report, err
	case errors.Is(err, ErrExecutionIntegrity), errors.Is(err, fsbind.ErrUnsafeObject), errors.Is(err, fsbind.ErrBindingChanged), errors.Is(err, fsbind.ErrCrossFilesystem):
		report.Outcome = ParentCleanupForgetOutcomeIntegrityFailed
		report.Operation.Resumable = false
		report.addParentCleanupForgetIssue("forget.integrity_failed", message)
		return *report, fmt.Errorf("%w: %s", ErrExecutionIntegrity, message)
	case errors.Is(err, fsbind.ErrDurabilityUnconfirmed):
		report.Outcome = ParentCleanupForgetOutcomeDurabilityUnconfirmed
		report.WritesUncertain = true
		report.addParentCleanupForgetIssue("forget.durability_unconfirmed", message)
		return *report, err
	default:
		report.Outcome = ParentCleanupForgetOutcomeInterrupted
		report.addParentCleanupForgetIssue("forget.operation_failed", message)
		return *report, err
	}
}
