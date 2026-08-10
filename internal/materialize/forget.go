package materialize

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

var forgetTransitionHook func(string) error

// Forget irreversibly removes one explicitly selected, already-pruned
// materialize tombstone. A private root-level intent is durably published
// first and remains the recovery authority until the exact operation subtree
// is durably absent. That last intent is then removed.
func Forget(ctx context.Context, options ForgetOptions) (ForgetReport, error) {
	report := newForgetReport(options)
	if err := options.Limits.Validate(); err != nil {
		return forgetBlocked(&report, "limits.invalid", "materialize forget limits are invalid")
	}
	if !options.Acknowledge {
		return forgetBlocked(&report, "acknowledgement.required", "historical tombstone deletion requires its dedicated acknowledgement")
	}
	if options.TargetRoot == "" || !canonicalPlanID(options.ExpectedPlanID) {
		return forgetBlocked(&report, "forget.selector_invalid", "target root, operation ID, or reviewed plan ID is invalid")
	}
	if parsed, err := ParseOperationID(options.OperationID.String()); err != nil || parsed != options.OperationID {
		return forgetBlocked(&report, "forget.selector_invalid", "target root, operation ID, or reviewed plan ID is invalid")
	}
	if err := ctx.Err(); err != nil {
		return forgetInterrupted(&report, err, "materialize forget was interrupted before target observation")
	}
	absolute, err := filepath.Abs(options.TargetRoot)
	if err != nil {
		return forgetBlocked(&report, "target.invalid_root", "the materialize target root is invalid")
	}
	target, rootInfo, err := fsbind.BindExisting(filepath.Clean(absolute))
	if err != nil {
		return mapForgetError(&report, err, "the target root cannot provide bound tombstone-forget semantics")
	}
	defer target.Close()
	report.Target.ObservedRootIdentity = rootInfo.Identity.String()
	report.Target.RootIdentityBound = true
	report.Target.StabilityAssurance = "non_atomic_bound_filesystem"

	rootName, _ := ForgetRootName(options.OperationID)
	marker, markerID, rootObject, _, markerErr := readForgetRootIntent(ctx, target, rootName, options.Limits.MaxMarkerBytes)
	var subtree *fsbind.Subtree
	rootIntentEnsured := false
	if errors.Is(markerErr, fsbind.ErrNotFound) {
		operationName, _ := OperationDirectoryName(options.OperationID)
		subtree, err = target.OpenPrivateSubtreeObserved(operationName)
		if errors.Is(err, fsbind.ErrNotFound) {
			report.Outcome = ForgetOutcomeAbsentUnattributed
			report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "absent_unattributed", "absent", false
			report.Authority.State = "absent"
			report.addForgetIssue("forget.absent_unattributed", "neither the retained operation nor a durable forget intent exists; prior erasure cannot be distinguished from an unknown selector")
			return report, ErrOperationNotFound
		}
		if err != nil {
			return mapForgetError(&report, err, "the retained materialize operation could not be opened")
		}
		defer subtree.Close()
		state, loadErr := loadRetentionMarkerState(ctx, subtree, options.OperationID, rootInfo.Identity)
		if loadErr != nil {
			return mapForgetError(&report, loadErr, "the retained materialize tombstone could not be loaded")
		}
		if !state.IntentPresent || !state.CompletePresent {
			return forgetBlocked(&report, "operation.prune_required", "only a complete retained materialize tombstone may be forgotten")
		}
		if state.Intent.PlanID != options.ExpectedPlanID {
			return forgetBlocked(&report, "plan.id_mismatch", "the reviewed plan ID does not select this materialize tombstone")
		}
		marker = ForgetIntent{
			Schema: ForgetIntentSchemaV1, OperationID: options.OperationID,
			OperationRootIdentity: subtree.Identity().String(), TargetRootIdentity: rootInfo.Identity.String(),
			PlanID: options.ExpectedPlanID, RetentionIntentMarkerID: state.IntentID,
			RetentionCompleteMarkerID: state.CompleteID, RetentionIntent: state.Intent,
			RetentionComplete: state.Complete, Basis: ForgetBasisExact,
		}
		if err := marker.Validate(); err != nil {
			return mapForgetError(&report, err, "the retained materialize tombstone cannot authorize forgetting")
		}
		if err := auditInitialForgetTombstone(ctx, subtree, state, marker); err != nil {
			return mapForgetError(&report, err, "the materialize tombstone is not exact before forget intent publication")
		}
		preparedRaw, preparedID, prepareErr := EncodeForgetIntent(marker)
		if prepareErr != nil {
			return mapForgetError(&report, prepareErr, "the materialize forget intent cannot be encoded")
		}
		if int64(len(preparedRaw)) > options.Limits.MaxMarkerBytes {
			return mapForgetError(&report, ErrPolicy, "the materialize forget marker byte budget is insufficient")
		}
		markerID = preparedID
		populateForgetAuthority(&report, marker, markerID)
		report.Authority.State = "exact_tombstone_observed"
		report.Operation.Status, report.Operation.PhaseBefore, report.Operation.PhaseAfter = "retained", "retained", "retained"
		markerID, receipt, object, publishErr := ensureForgetRootIntent(ctx, target, subtree, marker, options.Limits.MaxMarkerBytes)
		report.recordForgetMarker(receipt)
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
				report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "forgetting", "root_intent_publication_incomplete", true
			}
			return mapForgetError(&report, publishErr, "the materialize forget intent could not be durably published")
		}
		rootIntentEnsured = true
	} else if markerErr != nil {
		return mapForgetError(&report, markerErr, "the materialize forget intent could not be read")
	} else if err := selectForgetMarker(marker, options, rootInfo.Identity); err != nil {
		return mapForgetError(&report, err, "the materialize forget intent does not match the explicit selector")
	} else {
		populateForgetAuthority(&report, marker, markerID)
		report.Authority.State = "root_intent_visible_durability_unconfirmed"
		report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "forgetting", "root_intent_visible", true
		if err := target.SyncRoot(ctx); err != nil {
			return mapForgetError(&report, err, "the visible materialize forget intent durability could not be confirmed")
		}
		fresh, freshID, freshObject, _, freshErr := readForgetRootIntent(ctx, target, rootName, options.Limits.MaxMarkerBytes)
		if freshErr != nil || fresh != marker || freshID != markerID || !freshObject.Identity.Equal(rootObject.Identity) {
			if freshErr != nil {
				return mapForgetError(&report, classifyRetentionAuthorityError(freshErr), "the materialize forget intent could not be re-read after durability confirmation")
			}
			return mapForgetError(&report, ErrIntegrity, "the materialize forget intent changed during durability confirmation")
		}
		rootObject = freshObject
	}

	if markerID == "" {
		_, derivedMarkerID, encodeErr := EncodeForgetIntent(marker)
		if encodeErr != nil {
			return mapForgetError(&report, encodeErr, "the materialize forget intent cannot be reproduced")
		}
		markerID = derivedMarkerID
	}
	populateForgetAuthority(&report, marker, markerID)
	report.Authority.MarkerDurable = true
	report.Authority.State = "root_intent_published"
	report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "forgetting", "root_intent_published", true
	if forgetTransitionHook != nil {
		if hookErr := forgetTransitionHook("root_intent_published"); hookErr != nil {
			return mapForgetError(&report, hookErr, "materialize forgetting stopped after its durable root intent")
		}
	}

	if subtree == nil {
		operationName, _ := OperationDirectoryName(options.OperationID)
		expectedOperation, parseErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
		if parseErr != nil {
			return mapForgetError(&report, parseErr, "the materialize forget operation identity is invalid")
		}
		subtree, err = target.OpenPrivateSubtree(operationName, expectedOperation)
		if errors.Is(err, fsbind.ErrNotFound) {
			observed, inspectErr := target.InspectRoot(ctx, operationName)
			switch {
			case errors.Is(inspectErr, fsbind.ErrNotFound):
				if syncErr := target.SyncRoot(ctx); syncErr != nil {
					return mapForgetError(&report, syncErr, "the recovered materialize operation absence is not durable")
				}
				if _, confirmErr := target.InspectRoot(ctx, operationName); !errors.Is(confirmErr, fsbind.ErrNotFound) {
					if confirmErr == nil {
						confirmErr = fmt.Errorf("%w: the materialize operation reappeared", ErrIntegrity)
					}
					return mapForgetError(&report, confirmErr, "the recovered materialize operation absence changed")
				}
			case inspectErr != nil:
				return mapForgetError(&report, inspectErr, "the materialize operation removal state is unavailable")
			case observed.Kind != fsbind.ObjectKindDirectory || !observed.Identity.Equal(expectedOperation):
				return mapForgetError(&report, fsbind.ErrUnsafeObject, "the materialize operation residue changed identity")
			default:
				residue, residueErr := target.RemovePrivateSubtreeResidueExact(ctx, operationName, expectedOperation)
				report.Removals.OperationSubtree = residue
				report.recordForgetSubtreeRemoval(residue)
				if residueErr != nil {
					return mapForgetError(&report, residueErr, "the materialize empty operation residue could not be removed")
				}
			}
		} else if err != nil {
			return mapForgetError(&report, err, "the materialize operation could not be rebound for forgetting")
		} else {
			defer subtree.Close()
		}
	}
	if subtree != nil {
		if !rootIntentEnsured {
			ensuredID, receipt, ensuredObject, ensureErr := ensureForgetRootIntent(ctx, target, subtree, marker, options.Limits.MaxMarkerBytes)
			report.recordForgetMarker(receipt)
			if ensureErr != nil {
				return mapForgetError(&report, ensureErr, "the existing materialize forget intent could not be rebound to its retained tombstone")
			}
			if ensuredID != markerID || !ensuredObject.Identity.Equal(rootObject.Identity) {
				return mapForgetError(&report, ErrIntegrity, "the existing materialize forget intent changed while its tombstone was rebound")
			}
			rootObject = ensuredObject
			rootIntentEnsured = true
		}
		if err := removeTombstoneFromForgetIntent(ctx, target, subtree, marker, &report); err != nil {
			return mapForgetError(&report, err, "the retained materialize tombstone could not be completely removed")
		}
		subtree = nil
	}
	report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "forgetting", "operation_removed", true
	if forgetTransitionHook != nil {
		if hookErr := forgetTransitionHook("operation_removed"); hookErr != nil {
			return mapForgetError(&report, hookErr, "materialize forgetting stopped after operation removal")
		}
	}

	freshMarker, freshID, freshObject, _, freshErr := readForgetRootIntent(ctx, target, rootName, options.Limits.MaxMarkerBytes)
	if freshErr != nil || freshMarker != marker || freshID != markerID || !freshObject.Identity.Equal(rootObject.Identity) {
		if errors.Is(freshErr, fsbind.ErrNotFound) {
			return forgetAbsentAfterAuthority(&report, "the exact last marker disappeared before this invocation could re-read it")
		}
		if freshErr != nil {
			return mapForgetError(&report, freshErr, "the final materialize forget marker read failed")
		}
		return mapForgetError(&report, ErrIntegrity, "the final materialize forget marker changed")
	}
	if forgetTransitionHook != nil {
		if hookErr := forgetTransitionHook("before_root_intent_remove"); hookErr != nil {
			return mapForgetError(&report, hookErr, "materialize forgetting stopped before last-marker removal")
		}
	}
	operationName, _ := OperationDirectoryName(options.OperationID)
	if _, absenceErr := target.InspectRoot(ctx, operationName); !errors.Is(absenceErr, fsbind.ErrNotFound) {
		if absenceErr == nil {
			absenceErr = fmt.Errorf("%w: the materialize operation name reappeared", ErrIntegrity)
		}
		return mapForgetError(&report, absenceErr, "the materialize operation is no longer absent before last-marker removal")
	}
	if syncErr := target.SyncRoot(ctx); syncErr != nil {
		return mapForgetError(&report, syncErr, "the materialize operation absence is not durable before last-marker removal")
	}
	if _, absenceErr := target.InspectRoot(ctx, operationName); !errors.Is(absenceErr, fsbind.ErrNotFound) {
		if absenceErr == nil {
			absenceErr = fmt.Errorf("%w: the materialize operation name reappeared", ErrIntegrity)
		}
		return mapForgetError(&report, absenceErr, "the materialize operation absence changed before last-marker removal")
	}
	rootRemoval, removeErr := target.RemoveRootPrivateRegularExact(ctx, rootName, freshObject.Identity, freshObject.SizeBytes)
	report.Removals.RootIntent = rootRemoval
	report.recordForgetRemoval(rootRemoval)
	if rootRemoval.Attempted {
		report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "forget_completion_uncertain", "last_marker_removal_attempted", false
	}
	if removeErr != nil {
		if errors.Is(removeErr, fsbind.ErrNotFound) && !rootRemoval.Attempted {
			return forgetAbsentAfterAuthority(&report, "the exact last marker disappeared before this invocation attempted its removal")
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
		return mapForgetError(&report, removeErr, "the last materialize forget marker removal was not confirmed")
	}
	if !rootRemoval.Removed || rootRemoval.Durability != fsbind.DurabilityConfirmed {
		report.Authority.State = "last_marker_removal_durability_unconfirmed"
		report.Authority.MarkerDurable = false
		return mapForgetError(&report, fsbind.ErrDurabilityUnconfirmed, "the last materialize forget marker durability was not confirmed")
	}
	report.Outcome = ForgetOutcomeForgotten
	report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "forgotten", "forgotten", false
	report.Authority.State = "erased"
	report.Authority.MarkerDurable = false
	report.Authority.ExactTombstoneEvidenceAvailable = false
	report.Authority.TargetHistoricalEvidenceErased = true
	report.Warnings = append(report.Warnings, "forget completed: no ptctl materialize history for this operation remains in the target root")
	return report, nil
}

func selectForgetMarker(marker ForgetIntent, options ForgetOptions, targetIdentity fsbind.Identity) error {
	if err := marker.Validate(); err != nil {
		return err
	}
	if marker.OperationID != options.OperationID || marker.TargetRootIdentity != targetIdentity.String() {
		return fmt.Errorf("%w: materialize forget marker is bound to another filesystem object", ErrIntegrity)
	}
	if marker.PlanID != options.ExpectedPlanID {
		return fmt.Errorf("%w: materialize forget selector disagrees", ErrPolicy)
	}
	return nil
}

func applyForgetControl(ctx context.Context, target *fsbind.Session, operation OperationID, expectedPlanID string, report *Report) (bool, error) {
	if target == nil || report == nil {
		return false, fmt.Errorf("%w: materialize forget inspection authority is unavailable", ErrIntegrity)
	}
	rootName, err := ForgetRootName(operation)
	if err != nil {
		return false, fmt.Errorf("%w: materialize forget selector is invalid", ErrPolicy)
	}
	marker, markerID, _, _, err := readForgetRootIntent(ctx, target, rootName, maxForgetMarkerBytes)
	if errors.Is(err, fsbind.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		classifyForgetControlError(report, operation, target, err)
		return true, err
	}
	if marker.OperationID != operation || marker.TargetRootIdentity != target.Info().Identity.String() {
		err = fmt.Errorf("%w: materialize forget marker is bound to another operation", ErrIntegrity)
		classifyForgetControlError(report, operation, target, err)
		return true, err
	}
	if expectedPlanID != "" && marker.PlanID != expectedPlanID {
		err = fmt.Errorf("%w: materialize forget marker differs from the reviewed plan", ErrPolicy)
		classifyForgetControlError(report, operation, target, err)
		return true, err
	}
	report.Effect = []string{"read_private_materialize_forget_intent"}
	report.Operation = OperationReport{
		ID: operation.String(), Status: "forgetting", PhaseBefore: string(marker.RetentionIntent.TerminalPhase),
		PhaseAfter: "forget_intent_recorded", Resumable: false,
	}
	report.Plan.ObservedID = marker.PlanID
	report.Plan.MetafileVariantID = marker.RetentionIntent.MetafileVariantID
	report.Plan.Strategy = StrategyCopy
	report.Plan.Matches = expectedPlanID != "" && expectedPlanID == marker.PlanID
	report.Source = SourceReport{Mode: "not_requested", Outcome: "not_requested"}
	report.Target.RootIdentityBound = true
	report.Target.ExpectedRootIdentity = marker.TargetRootIdentity
	report.Target.ObservedRootIdentity = target.Info().Identity.String()
	report.Target.Publication = "historical_forget_basis"
	report.Target.StabilityAssurance = "historical_forget_marker_only"
	report.Outcome = OutcomeForgetting
	report.addBlocker("operation.forget_required", "a private forget intent is visible; only explicit materialize forget may confirm durability and advance or recover historical evidence deletion")
	report.Warnings = append(report.Warnings,
		"materialize historical evidence is being forgotten under an explicit root-level recovery marker; read-only status does not reassert its durability",
		"forget marker "+markerID.String()+" is a stable pseudonym and not anonymization",
	)
	return true, nil
}

func classifyForgetControlError(report *Report, operation OperationID, target *fsbind.Session, err error) {
	if report == nil {
		return
	}
	report.Effect = []string{"read_private_materialize_forget_intent"}
	report.Operation = OperationReport{
		ID: operation.String(), Status: "inspection_incomplete", PhaseBefore: "unknown", PhaseAfter: "unknown", Resumable: false,
	}
	report.Source = SourceReport{Mode: "not_requested", Outcome: "not_requested"}
	report.Target.Publication = "forget_marker_inspection_incomplete"
	if target != nil {
		report.Target.RootIdentityBound = true
		report.Target.ObservedRootIdentity = target.Info().Identity.String()
		report.Target.StabilityAssurance = "non_atomic_bound_filesystem"
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		report.Outcome = OutcomeInterrupted
		report.addIssue("context_cancelled", "materialize forget marker inspection was cancelled", nil)
	case errors.Is(err, ErrPolicy), errors.Is(err, fsbind.ErrUnsupported), errors.Is(err, fsbind.ErrBusy):
		report.Outcome = OutcomeBlocked
		report.addBlocker("operation.forget_selector_mismatch", "the visible materialize forget marker does not match this control selector")
	case errors.Is(err, ErrIntegrity), errors.Is(err, ErrCorruptJournal), errors.Is(err, fsbind.ErrUnsafeObject),
		errors.Is(err, fsbind.ErrBindingChanged), errors.Is(err, fsbind.ErrCrossFilesystem):
		report.Outcome = OutcomeIntegrityFailed
		report.addIssue("operation.forget_marker_invalid", "the visible materialize forget marker failed exact integrity validation", nil)
	default:
		report.Outcome = OutcomeInterrupted
		report.addIssue("operation.forget_inspection_failed", "the visible materialize forget marker could not be completely inspected", nil)
	}
}

func populateForgetAuthority(report *ForgetReport, marker ForgetIntent, markerID ForgetMarkerID) {
	if report.Operation.PhaseBefore == "" || report.Operation.PhaseBefore == "unknown" {
		report.Operation.PhaseBefore = "retained"
	}
	report.Plan.ObservedID = marker.PlanID
	report.Plan.Matches = marker.PlanID == report.Plan.ExpectedID
	report.Plan.MetafileVariantID = marker.RetentionIntent.MetafileVariantID
	report.Target.ExpectedRootIdentity = marker.TargetRootIdentity
	if marker.RetentionIntent.TerminalPhase == PhaseCommitted {
		report.Target.FinalState = "historical_committed_tombstone"
	} else {
		report.Target.FinalState = "historical_unpublished_tombstone"
	}
	report.Authority.MarkerID = markerID.String()
	report.Authority.RetentionIntentMarkerID = marker.RetentionIntentMarkerID.String()
	report.Authority.RetentionCompleteMarkerID = marker.RetentionCompleteMarkerID.String()
	report.Authority.ExactTombstoneEvidenceAvailable = true
	report.Proof = ForgetProofReport{
		Basis: marker.RetentionIntent.Basis, TerminalEventID: marker.RetentionIntent.TerminalEventID.String(),
		TerminalPhase: string(marker.RetentionIntent.TerminalPhase), MetafileVariantID: marker.RetentionIntent.MetafileVariantID,
		HistoricalAuthority: true, Assurance: "exact_retention_tombstone_copied_into_durable_forget_intent",
	}
}

func auditInitialForgetTombstone(ctx context.Context, subtree *fsbind.Subtree, state retentionMarkerState, marker ForgetIntent) error {
	if !state.IntentPresent || !state.CompletePresent || state.IntentID != marker.RetentionIntentMarkerID || state.CompleteID != marker.RetentionCompleteMarkerID ||
		state.Intent != marker.RetentionIntent || state.Complete != marker.RetentionComplete {
		return fmt.Errorf("%w: retained materialize tombstone disagrees with forget intent", ErrIntegrity)
	}
	if _, err := retentionHeavyNames(ctx, subtree, true); err != nil {
		return err
	}
	raw, markerID, err := EncodeForgetIntent(marker)
	if err != nil {
		return err
	}
	expectedPending := forgetPendingName(markerID)
	directory, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	listing, err := subtree.List(ctx, directory, fsbind.ListLimits{MaxEntries: 4, MaxNameBytes: 1 << 20})
	if err != nil || !listing.Complete {
		if err != nil {
			return classifyRetentionAuthorityError(err)
		}
		return fmt.Errorf("%w: materialize forget preflight inventory is incomplete", ErrIntegrity)
	}
	seenIntent, seenComplete := false, false
	for _, entry := range listing.Entries {
		switch entry.Name {
		case retentionIntentFileName:
			seenIntent = entry.Kind == string(fsbind.ObjectKindRegular)
		case retentionCompleteName:
			seenComplete = entry.Kind == string(fsbind.ObjectKindRegular)
		case expectedPending:
			if entry.Kind != string(fsbind.ObjectKindRegular) {
				return fmt.Errorf("%w: materialize forget pending marker is unsafe", ErrIntegrity)
			}
			pending, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, expectedPending})
			if _, verifyErr := verifyRetentionNamedBytes(ctx, subtree, pending, raw); verifyErr != nil {
				return verifyErr
			}
		default:
			return fmt.Errorf("%w: materialize forget preflight contains an unexpected object", ErrIntegrity)
		}
	}
	if !seenIntent || !seenComplete {
		return fmt.Errorf("%w: retained materialize tombstone is incomplete", ErrIntegrity)
	}
	intentRaw, intentID, intentErr := EncodeRetentionIntent(state.Intent)
	completeRaw, completeID, completeErr := EncodeRetentionComplete(state.Complete)
	if intentErr != nil || completeErr != nil || intentID != state.IntentID || completeID != state.CompleteID {
		return fmt.Errorf("%w: retained materialize tombstone identity changed", ErrIntegrity)
	}
	intentPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, retentionIntentFileName})
	if _, err := verifyRetentionNamedBytes(ctx, subtree, intentPath, intentRaw); err != nil {
		return err
	}
	completePath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, retentionCompleteName})
	if _, err := verifyRetentionNamedBytes(ctx, subtree, completePath, completeRaw); err != nil {
		return err
	}
	after, err := subtree.List(ctx, directory, fsbind.ListLimits{MaxEntries: 4, MaxNameBytes: 1 << 20})
	if err != nil || !after.Complete || !sameRetentionEntries(listing.Entries, after.Entries) {
		if err != nil {
			return classifyRetentionAuthorityError(err)
		}
		return fmt.Errorf("%w: materialize forget preflight changed during observation", ErrIntegrity)
	}
	return subtree.CheckPaths(fsbind.Path{}, directory)
}

func sameRetentionEntries(left, right []fsbind.Entry) bool {
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

func removeTombstoneFromForgetIntent(ctx context.Context, target *fsbind.Session, subtree *fsbind.Subtree, marker ForgetIntent, report *ForgetReport) error {
	expectedOperation, err := fsbind.ParseIdentity(marker.OperationRootIdentity)
	if err != nil || subtree == nil || !subtree.Identity().Equal(expectedOperation) {
		return fmt.Errorf("%w: materialize operation identity changed", ErrIntegrity)
	}
	directoryPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	directory, directoryErr := subtree.Inspect(ctx, directoryPath)
	if errors.Is(directoryErr, fsbind.ErrNotFound) {
		return removeForgetOperationRoot(ctx, target, subtree, report)
	}
	if directoryErr != nil || directory.Kind != fsbind.ObjectKindDirectory {
		if directoryErr != nil {
			return classifyRetentionAuthorityError(directoryErr)
		}
		return fmt.Errorf("%w: materialize retention directory is unsafe", ErrIntegrity)
	}
	listing, err := subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 3, MaxNameBytes: 1 << 20})
	if err != nil || !listing.Complete {
		if err != nil {
			return classifyRetentionAuthorityError(err)
		}
		return fmt.Errorf("%w: materialize forget recovery inventory is incomplete", ErrIntegrity)
	}
	for _, entry := range listing.Entries {
		if entry.Kind != string(fsbind.ObjectKindRegular) || entry.Name != retentionIntentFileName && entry.Name != retentionCompleteName {
			return fmt.Errorf("%w: materialize forget recovery contains an unexpected object", ErrIntegrity)
		}
	}
	completeRaw, completeID, completeErr := EncodeRetentionComplete(marker.RetentionComplete)
	intentRaw, intentID, intentErr := EncodeRetentionIntent(marker.RetentionIntent)
	if completeErr != nil || intentErr != nil || completeID != marker.RetentionCompleteMarkerID || intentID != marker.RetentionIntentMarkerID {
		return fmt.Errorf("%w: retained materialize tombstone cannot be reproduced", ErrIntegrity)
	}
	for _, selected := range []struct {
		name string
		raw  []byte
		dst  *fsbind.Removal
	}{
		{name: retentionCompleteName, raw: completeRaw, dst: &report.Removals.RetentionComplete},
		{name: retentionIntentFileName, raw: intentRaw, dst: &report.Removals.RetentionIntent},
	} {
		path, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, selected.name})
		object, inspectErr := subtree.Inspect(ctx, path)
		if errors.Is(inspectErr, fsbind.ErrNotFound) {
			continue
		}
		if inspectErr != nil || object.Kind != fsbind.ObjectKindRegular || object.SizeBytes != int64(len(selected.raw)) {
			if inspectErr != nil {
				return classifyRetentionAuthorityError(inspectErr)
			}
			return fmt.Errorf("%w: retained materialize marker is unsafe", ErrIntegrity)
		}
		if identity, verifyErr := verifyRetentionNamedBytes(ctx, subtree, path, selected.raw); verifyErr != nil || !identity.Equal(object.Identity) {
			if verifyErr != nil {
				return verifyErr
			}
			return fmt.Errorf("%w: retained materialize marker identity changed", ErrIntegrity)
		}
		removal, removeErr := subtree.RemoveRegularExact(ctx, path, object.Identity, object.SizeBytes)
		*selected.dst = removal
		report.recordForgetRemoval(removal)
		if removeErr != nil {
			return removeErr
		}
	}
	after, err := subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 1, MaxNameBytes: 1 << 10})
	if err != nil || !after.Complete || len(after.Entries) != 0 {
		if err != nil {
			return classifyRetentionAuthorityError(err)
		}
		return fmt.Errorf("%w: materialize retention directory is not empty after marker removal", ErrIntegrity)
	}
	directoryRemoval, removeErr := subtree.RemoveEmptyDirectory(ctx, directoryPath, directory.Identity)
	report.Removals.RetentionDirectory = directoryRemoval
	report.recordForgetRemoval(directoryRemoval)
	if removeErr != nil {
		return removeErr
	}
	return removeForgetOperationRoot(ctx, target, subtree, report)
}

func removeForgetOperationRoot(ctx context.Context, target *fsbind.Session, subtree *fsbind.Subtree, report *ForgetReport) error {
	root, err := subtree.List(ctx, fsbind.Path{}, fsbind.ListLimits{MaxEntries: 2, MaxNameBytes: 1 << 10})
	if err != nil || !root.Complete || len(root.Entries) != 1 || root.Entries[0].Name != operationLockEntryName || root.Entries[0].Kind != string(fsbind.ObjectKindRegular) {
		if err != nil {
			return classifyRetentionAuthorityError(err)
		}
		return fmt.Errorf("%w: materialize operation root is not an exact lock-only namespace", ErrIntegrity)
	}
	removal, removeErr := target.RemovePrivateSubtreeExact(ctx, subtree)
	report.Removals.OperationSubtree = removal
	report.recordForgetSubtreeRemoval(removal)
	return removeErr
}

func (report *ForgetReport) recordForgetMarker(receipt forgetMarkerReceipt) {
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
		report.recordForgetRemoval(receipt.TemporaryRemoval)
	}
}

func (report *ForgetReport) recordForgetRemoval(removal fsbind.Removal) {
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

func (report *ForgetReport) recordForgetSubtreeRemoval(removal fsbind.PrivateSubtreeRemoval) {
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

func (report *ForgetReport) addForgetBlocker(code, message string) {
	report.Blockers = append(report.Blockers, Finding{Code: code, Message: message})
}

func (report *ForgetReport) addForgetIssue(code, message string) {
	report.Issues = append(report.Issues, Finding{Code: code, Message: message})
}

func forgetBlocked(report *ForgetReport, code, message string) (ForgetReport, error) {
	report.Outcome = ForgetOutcomeBlocked
	report.addForgetBlocker(code, message)
	return *report, fmt.Errorf("%w: %s", ErrPolicy, message)
}

func forgetInterrupted(report *ForgetReport, err error, message string) (ForgetReport, error) {
	report.Outcome = ForgetOutcomeInterrupted
	report.addForgetIssue("forget.interrupted", message)
	return *report, err
}

func forgetAbsentAfterAuthority(report *ForgetReport, message string) (ForgetReport, error) {
	report.Outcome = ForgetOutcomeAbsentUnattributed
	report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "absent_unattributed", "last_marker_disappeared", false
	report.Authority.State = "absent_after_exact_authority"
	report.Authority.MarkerDurable = false
	report.Authority.ExactTombstoneEvidenceAvailable = false
	report.addForgetIssue("forget.absent_unattributed", message+"; completion cannot be attributed")
	return *report, ErrOperationNotFound
}

func mapForgetError(report *ForgetReport, err error, message string) (ForgetReport, error) {
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
		return forgetInterrupted(report, err, message)
	case errors.Is(err, ErrPolicy), errors.Is(err, fsbind.ErrUnsupported), errors.Is(err, fsbind.ErrBusy), errors.Is(err, fsbind.ErrAlreadyExists):
		report.Outcome = ForgetOutcomeBlocked
		report.addForgetBlocker("forget.policy_blocked", message)
		return *report, err
	case errors.Is(err, ErrIntegrity), errors.Is(err, ErrCorruptJournal), errors.Is(err, fsbind.ErrUnsafeObject),
		errors.Is(err, fsbind.ErrBindingChanged), errors.Is(err, fsbind.ErrCrossFilesystem):
		report.Outcome = ForgetOutcomeIntegrityFailed
		report.Operation.Resumable = false
		report.addForgetIssue("forget.integrity_failed", message)
		return *report, fmt.Errorf("%w: %s", ErrIntegrity, message)
	case errors.Is(err, fsbind.ErrDurabilityUnconfirmed):
		report.Outcome = ForgetOutcomeDurabilityUnconfirmed
		report.WritesUncertain = true
		report.addForgetIssue("forget.durability_unconfirmed", message)
		return *report, err
	default:
		report.Outcome = ForgetOutcomeInterrupted
		report.addForgetIssue("forget.operation_failed", message)
		return *report, err
	}
}
