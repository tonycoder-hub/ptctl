package clientadopt

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

var forgetTransitionHook func(string) error

type forgetInProgressError struct {
	marker      ForgetIntent
	markerID    ForgetMarkerID
	rootDurable bool
}

func (*forgetInProgressError) Error() string {
	return "client adoption historical evidence deletion is in progress"
}
func (*forgetInProgressError) Unwrap() error { return ErrPolicy }

// Forget irreversibly removes one explicitly selected, already-pruned client
// adoption tombstone. A private root-level intent is durably published first
// and remains the recovery authority until the exact operation subtree is
// durably absent. That last intent is then removed.
func Forget(ctx context.Context, options ForgetOptions) (ForgetReport, error) {
	report := newForgetReport(options)
	if err := options.Limits.Validate(); err != nil {
		return forgetBlocked(&report, "limits.invalid", "client adoption forget limits are invalid")
	}
	if !options.Acknowledge {
		return forgetBlocked(&report, "acknowledgement.required", "historical adoption tombstone deletion requires its dedicated acknowledgement")
	}
	if options.TargetRoot == "" || !canonicalPlanID(options.ExpectedPlanID) || OperationIDForPlan(options.ExpectedPlanID) != options.OperationID {
		return forgetBlocked(&report, "forget.selector_invalid", "target root, operation ID, or reviewed adoption plan ID is invalid")
	}
	if parsed, err := ParseOperationID(options.OperationID.String()); err != nil || parsed != options.OperationID {
		return forgetBlocked(&report, "forget.selector_invalid", "target root, operation ID, or reviewed adoption plan ID is invalid")
	}
	if err := ctx.Err(); err != nil {
		return forgetInterrupted(&report, err, "client adoption forget was interrupted before target observation")
	}
	absolute, err := filepath.Abs(options.TargetRoot)
	if err != nil {
		return forgetBlocked(&report, "target.invalid_root", "the client adoption target root is invalid")
	}
	target, rootInfo, err := fsbind.BindExisting(filepath.Clean(absolute))
	if err != nil {
		return mapForgetError(&report, err, "the target root cannot provide bound adoption tombstone-forget semantics")
	}
	defer target.Close()
	report.Target.ObservedRootIdentity = rootInfo.Identity.String()
	report.Target.RootIdentityBound = true
	report.Target.StabilityAssurance = "non_atomic_bound_filesystem"

	rootName, _ := ForgetRootName(options.OperationID)
	marker, markerID, rootObject, _, markerErr := readForgetRootIntent(ctx, target, rootName, options.Limits.MaxMarkerBytes)
	var handle *journalHandle
	if errors.Is(markerErr, fsbind.ErrNotFound) {
		operationName, _ := operationDirectoryName(options.OperationID)
		subtree, openErr := target.OpenPrivateSubtreeObserved(operationName)
		if errors.Is(openErr, fsbind.ErrNotFound) {
			report.Outcome = ForgetOutcomeAbsentUnattributed
			report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "absent_unattributed", "absent", false
			report.Authority.State = "absent"
			report.Issues = append(report.Issues, Finding{Code: "forget.absent_unattributed", Message: "neither the retained adoption operation nor a durable forget intent exists; prior erasure cannot be distinguished from an unknown selector"})
			return report, ErrOperationNotFound
		}
		if openErr != nil {
			return mapForgetError(&report, openErr, "the retained client adoption operation could not be opened")
		}
		handle = &journalHandle{session: target, subtree: subtree}
		defer func() {
			if handle != nil && handle.subtree != nil {
				_ = handle.subtree.Close()
			}
		}()
		state, loadErr := loadRetentionState(ctx, handle, options.OperationID, rootInfo.Identity)
		if loadErr != nil {
			return mapForgetError(&report, loadErr, "the retained client adoption tombstone could not be loaded")
		}
		if !state.IntentPresent || !state.CompletePresent || state.IntentPending || state.CompletePending {
			return forgetBlocked(&report, "operation.prune_required", "only a complete retained client adoption tombstone may be forgotten")
		}
		if state.Intent.PlanID != options.ExpectedPlanID {
			return forgetBlocked(&report, "plan.id_mismatch", "the reviewed adoption plan ID does not select this tombstone")
		}
		marker = ForgetIntent{Schema: ForgetIntentSchemaV1, OperationID: options.OperationID,
			OperationRootIdentity: subtree.Identity().String(), TargetRootIdentity: rootInfo.Identity.String(), PlanID: options.ExpectedPlanID,
			RetentionIntentMarkerID: state.IntentID, RetentionCompleteMarkerID: state.CompleteID,
			RetentionIntent: state.Intent, RetentionComplete: state.Complete, Basis: ForgetBasisExact}
		if err := marker.Validate(); err != nil {
			return mapForgetError(&report, err, "the retained client adoption tombstone cannot authorize forgetting")
		}
		if err := auditInitialForgetTombstone(ctx, handle, state, marker); err != nil {
			return mapForgetError(&report, err, "the client adoption tombstone is not exact before forget intent publication")
		}
		preparedRaw, preparedID, prepareErr := EncodeForgetIntent(marker)
		if prepareErr != nil {
			return mapForgetError(&report, prepareErr, "the client adoption forget intent cannot be encoded")
		}
		if int64(len(preparedRaw)) > options.Limits.MaxMarkerBytes {
			return mapForgetError(&report, ErrPolicy, "the client adoption forget marker byte budget is insufficient")
		}
		markerID = preparedID
		populateForgetAuthority(&report, marker, markerID)
		report.Authority.State = "exact_tombstone_observed"
		report.Operation.Status, report.Operation.PhaseBefore, report.Operation.PhaseAfter = "retained", "retained_adoption_completion", "retained_adoption_completion"
		ensuredID, receipt, object, publishErr := ensureForgetRootIntent(ctx, target, handle, marker, options.Limits.MaxMarkerBytes)
		report.recordForgetMarker(receipt)
		rootObject, markerID = object, ensuredID
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
			return mapForgetError(&report, publishErr, "the client adoption forget intent could not be durably published")
		}
	} else if markerErr != nil {
		return mapForgetError(&report, markerErr, "the client adoption forget intent could not be read")
	} else if err := selectForgetMarker(marker, options, rootInfo.Identity); err != nil {
		return mapForgetError(&report, err, "the client adoption forget intent does not match the explicit selector")
	} else {
		populateForgetAuthority(&report, marker, markerID)
		report.Authority.State = "root_intent_visible_durability_unconfirmed"
		report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "forgetting", "root_intent_visible", true
		if err := target.SyncRoot(ctx); err != nil {
			return mapForgetError(&report, err, "the visible client adoption forget intent durability could not be confirmed")
		}
		fresh, freshID, freshObject, _, freshErr := readForgetRootIntent(ctx, target, rootName, options.Limits.MaxMarkerBytes)
		if freshErr != nil || !sameForgetIntent(fresh, marker) || freshID != markerID || !freshObject.Identity.Equal(rootObject.Identity) {
			if freshErr != nil {
				return mapForgetError(&report, classifyJournalError(freshErr), "the client adoption forget intent could not be re-read after durability confirmation")
			}
			return mapForgetError(&report, ErrIntegrity, "the client adoption forget intent changed during durability confirmation")
		}
		rootObject = freshObject
	}

	if markerID == "" {
		_, derivedID, encodeErr := EncodeForgetIntent(marker)
		if encodeErr != nil {
			return mapForgetError(&report, encodeErr, "the client adoption forget intent cannot be reproduced")
		}
		markerID = derivedID
	}
	populateForgetAuthority(&report, marker, markerID)
	report.Authority.MarkerDurable = true
	report.Authority.State = "root_intent_published"
	report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "forgetting", "root_intent_published", true
	if forgetTransitionHook != nil {
		if hookErr := forgetTransitionHook("root_intent_published"); hookErr != nil {
			return mapForgetError(&report, hookErr, "client adoption forgetting stopped after its durable root intent")
		}
	}

	if handle == nil {
		operationName, _ := operationDirectoryName(options.OperationID)
		expectedOperation, parseErr := fsbind.ParseIdentity(marker.OperationRootIdentity)
		if parseErr != nil {
			return mapForgetError(&report, parseErr, "the client adoption forget operation identity is invalid")
		}
		subtree, openErr := target.OpenPrivateSubtree(operationName, expectedOperation)
		if errors.Is(openErr, fsbind.ErrNotFound) {
			observed, inspectErr := target.InspectRoot(ctx, operationName)
			switch {
			case errors.Is(inspectErr, fsbind.ErrNotFound):
				if syncErr := target.SyncRoot(ctx); syncErr != nil {
					return mapForgetError(&report, syncErr, "the recovered client adoption operation absence is not durable")
				}
				if _, confirmErr := target.InspectRoot(ctx, operationName); !errors.Is(confirmErr, fsbind.ErrNotFound) {
					if confirmErr == nil {
						confirmErr = fmt.Errorf("%w: the client adoption operation reappeared", ErrIntegrity)
					}
					return mapForgetError(&report, confirmErr, "the recovered client adoption operation absence changed")
				}
			case inspectErr != nil:
				return mapForgetError(&report, inspectErr, "the client adoption operation removal state is unavailable")
			case observed.Kind != fsbind.ObjectKindDirectory || !observed.Identity.Equal(expectedOperation):
				return mapForgetError(&report, fsbind.ErrUnsafeObject, "the client adoption operation residue changed identity")
			default:
				residue, residueErr := target.RemovePrivateSubtreeResidueExact(ctx, operationName, expectedOperation)
				report.Removals.OperationSubtree = residue
				report.recordForgetSubtreeRemoval(residue)
				if residueErr != nil {
					return mapForgetError(&report, residueErr, "the client adoption empty operation residue could not be removed")
				}
			}
		} else if openErr != nil {
			return mapForgetError(&report, openErr, "the client adoption operation could not be rebound for forgetting")
		} else {
			handle = &journalHandle{session: target, subtree: subtree}
			defer func() {
				if handle != nil && handle.subtree != nil {
					_ = handle.subtree.Close()
				}
			}()
		}
	}
	if handle != nil {
		ensuredID, receipt, ensuredObject, ensureErr := ensureForgetRootIntent(ctx, target, handle, marker, options.Limits.MaxMarkerBytes)
		report.recordForgetMarker(receipt)
		if ensureErr != nil {
			return mapForgetError(&report, ensureErr, "the existing client adoption forget intent could not be rebound to its retained tombstone")
		}
		if ensuredID != markerID || !ensuredObject.Identity.Equal(rootObject.Identity) {
			return mapForgetError(&report, ErrIntegrity, "the existing client adoption forget intent changed while its tombstone was rebound")
		}
		rootObject = ensuredObject
		if err := removeTombstoneFromForgetIntent(ctx, target, handle, marker, &report); err != nil {
			return mapForgetError(&report, err, "the retained client adoption tombstone could not be completely removed")
		}
		handle = nil
	}
	report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "forgetting", "operation_removed", true
	if forgetTransitionHook != nil {
		if hookErr := forgetTransitionHook("operation_removed"); hookErr != nil {
			return mapForgetError(&report, hookErr, "client adoption forgetting stopped after operation removal")
		}
	}

	freshMarker, freshID, freshObject, _, freshErr := readForgetRootIntent(ctx, target, rootName, options.Limits.MaxMarkerBytes)
	if freshErr != nil || !sameForgetIntent(freshMarker, marker) || freshID != markerID || !freshObject.Identity.Equal(rootObject.Identity) {
		if errors.Is(freshErr, fsbind.ErrNotFound) {
			return forgetAbsentAfterAuthority(&report, "the exact last marker disappeared before this invocation could re-read it")
		}
		if freshErr != nil {
			return mapForgetError(&report, freshErr, "the final client adoption forget marker read failed")
		}
		return mapForgetError(&report, ErrIntegrity, "the final client adoption forget marker changed")
	}
	if forgetTransitionHook != nil {
		if hookErr := forgetTransitionHook("before_root_intent_remove"); hookErr != nil {
			return mapForgetError(&report, hookErr, "client adoption forgetting stopped before last-marker removal")
		}
	}
	operationName, _ := operationDirectoryName(options.OperationID)
	if _, absenceErr := target.InspectRoot(ctx, operationName); !errors.Is(absenceErr, fsbind.ErrNotFound) {
		if absenceErr == nil {
			absenceErr = fmt.Errorf("%w: the client adoption operation name reappeared", ErrIntegrity)
		}
		return mapForgetError(&report, absenceErr, "the client adoption operation is no longer absent before last-marker removal")
	}
	if syncErr := target.SyncRoot(ctx); syncErr != nil {
		return mapForgetError(&report, syncErr, "the client adoption operation absence is not durable before last-marker removal")
	}
	if _, absenceErr := target.InspectRoot(ctx, operationName); !errors.Is(absenceErr, fsbind.ErrNotFound) {
		if absenceErr == nil {
			absenceErr = fmt.Errorf("%w: the client adoption operation name reappeared", ErrIntegrity)
		}
		return mapForgetError(&report, absenceErr, "the client adoption operation absence changed before last-marker removal")
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
		return mapForgetError(&report, removeErr, "the last client adoption forget marker removal was not confirmed")
	}
	if !rootRemoval.Removed || rootRemoval.Durability != fsbind.DurabilityConfirmed {
		report.Authority.State = "last_marker_removal_durability_unconfirmed"
		report.Authority.MarkerDurable = false
		return mapForgetError(&report, fsbind.ErrDurabilityUnconfirmed, "the last client adoption forget marker durability was not confirmed")
	}
	report.Outcome = ForgetOutcomeForgotten
	report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "forgotten", "forgotten", false
	report.Authority.State = "erased"
	report.Authority.MarkerDurable = false
	report.Authority.ExactTombstoneEvidenceAvailable = false
	report.Authority.TargetHistoricalEvidenceErased = true
	report.Warnings = append(report.Warnings, "forget completed: no ptctl client adoption history for this operation remains in the target root")
	return report, nil
}

func sameForgetIntent(left, right ForgetIntent) bool {
	leftRaw, leftID, leftErr := EncodeForgetIntent(left)
	rightRaw, rightID, rightErr := EncodeForgetIntent(right)
	return leftErr == nil && rightErr == nil && leftID == rightID && string(leftRaw) == string(rightRaw)
}

func selectForgetMarker(marker ForgetIntent, options ForgetOptions, targetIdentity fsbind.Identity) error {
	if err := marker.Validate(); err != nil {
		return err
	}
	if marker.OperationID != options.OperationID || marker.TargetRootIdentity != targetIdentity.String() {
		return fmt.Errorf("%w: client adoption forget marker is bound to another filesystem object", ErrIntegrity)
	}
	if marker.PlanID != options.ExpectedPlanID {
		return fmt.Errorf("%w: client adoption forget selector disagrees", ErrPolicy)
	}
	return nil
}

func inspectForgetControl(ctx context.Context, target *fsbind.Session, operation OperationID, expectedPlanID string) (*forgetInProgressError, error) {
	if target == nil {
		return nil, fmt.Errorf("%w: client adoption forget inspection authority is unavailable", ErrIntegrity)
	}
	rootName, err := ForgetRootName(operation)
	if err != nil {
		return nil, fmt.Errorf("%w: client adoption forget selector is invalid", ErrPolicy)
	}
	marker, markerID, _, _, err := readForgetRootIntent(ctx, target, rootName, maximumForgetMarkerBytes)
	if errors.Is(err, fsbind.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if marker.OperationID != operation || marker.TargetRootIdentity != target.Info().Identity.String() {
		return nil, fmt.Errorf("%w: client adoption forget marker is bound to another operation", ErrIntegrity)
	}
	if expectedPlanID != "" && marker.PlanID != expectedPlanID {
		return nil, fmt.Errorf("%w: client adoption forget marker differs from the reviewed plan", ErrPolicy)
	}
	return &forgetInProgressError{marker: marker, markerID: markerID, rootDurable: true}, nil
}

func applyForgetControlReport(report *Report, pending *forgetInProgressError) {
	if report == nil || pending == nil {
		return
	}
	marker := pending.marker
	report.Effect = []string{"read_private_client_adoption_forget_intent"}
	phase := "forget_intent_staging"
	if pending.rootDurable {
		phase = "forget_intent_recorded"
	}
	report.Operation = OperationReport{ID: marker.OperationID.String(), Status: "forgetting", PhaseBefore: "retained_adoption_completion", PhaseAfter: phase, Resumable: false}
	plan := marker.RetentionIntent.Intent.Plan
	report.Plan = PlanReport{ID: marker.PlanID, ExpectedID: report.Plan.ExpectedID, Matches: report.Plan.ExpectedID == "" || report.Plan.ExpectedID == marker.PlanID,
		Action: plan.Action, ClientConfigID: plan.ClientConfigID, PathMappingID: plan.PathMappingID,
		ClientPathSemantics: plan.ClientPathSemantics, ExpectedSavePathRef: plan.ExpectedSavePathRef,
		ExpectedContentPathRef: plan.ExpectedContentPathRef}
	report.Final = FinalReport{Status: "historical_forget_basis_current_not_observed", Observation: historicalFinalObservation(plan)}
	report.Client.Status = "historical_forget_basis_current_client_not_observed"
	report.Journal = JournalReport{Status: phase, RetentionState: "forgetting", RetentionIntentPresent: true, RetentionCompletionPresent: true}
	report.Outcome = OutcomeForgetting
	report.addBlocker("operation.forget_required", "a private forget intent is visible; only explicit client adopt forget may confirm durability and advance or recover historical evidence deletion")
	markerWarning := "client adoption historical evidence deletion has a private staged intent; only explicit forget may publish or recover it"
	if pending.rootDurable {
		markerWarning = "client adoption historical evidence is being forgotten under an explicit root-level recovery marker; read-only status does not reassert its durability"
	}
	report.Warnings = append(report.Warnings, markerWarning,
		"forget marker "+pending.markerID.String()+" is a stable pseudonym and not anonymization")
}

func populateForgetAuthority(report *ForgetReport, marker ForgetIntent, markerID ForgetMarkerID) {
	if report.Operation.PhaseBefore == "" || report.Operation.PhaseBefore == "unknown" {
		report.Operation.PhaseBefore = "retained_adoption_completion"
	}
	plan := marker.RetentionIntent.Intent.Plan
	report.Plan = PlanReport{ID: marker.PlanID, ExpectedID: report.Plan.ExpectedID, Matches: marker.PlanID == report.Plan.ExpectedID,
		Action: plan.Action, ClientConfigID: plan.ClientConfigID, PathMappingID: plan.PathMappingID,
		ClientPathSemantics: plan.ClientPathSemantics, ExpectedSavePathRef: plan.ExpectedSavePathRef,
		ExpectedContentPathRef: plan.ExpectedContentPathRef}
	report.Target.ExpectedRootIdentity = marker.TargetRootIdentity
	report.Authority.MarkerID = markerID.String()
	report.Authority.RetentionIntentMarkerID = marker.RetentionIntentMarkerID.String()
	report.Authority.RetentionCompleteMarkerID = marker.RetentionCompleteMarkerID.String()
	report.Authority.ExactTombstoneEvidenceAvailable = true
	report.Proof = RetentionProofReport{Basis: marker.RetentionIntent.Basis, IntentID: marker.RetentionIntent.IntentID.String(),
		CompletionID: marker.RetentionIntent.CompletionID.String(), AttemptsRecorded: len(marker.RetentionIntent.Attempts), HistoricalAuthority: true,
		Assurance: "exact_adoption_retention_tombstone_copied_into_durable_forget_intent"}
}

func auditInitialForgetTombstone(ctx context.Context, handle *journalHandle, state retentionState, marker ForgetIntent) error {
	if handle == nil || handle.subtree == nil || !state.IntentPresent || !state.CompletePresent || state.IntentPending || state.CompletePending ||
		state.IntentID != marker.RetentionIntentMarkerID || state.CompleteID != marker.RetentionCompleteMarkerID ||
		!sameRetentionIntent(state.Intent, marker.RetentionIntent) || state.Complete != marker.RetentionComplete {
		return fmt.Errorf("%w: retained client adoption tombstone disagrees with forget intent", ErrIntegrity)
	}
	if _, err := auditRetentionNamespace(ctx, handle, state.Intent, DefaultRetentionLimits(), true, true, true); err != nil {
		return err
	}
	raw, markerID, err := EncodeForgetIntent(marker)
	if err != nil {
		return err
	}
	directory, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	listing, err := handle.subtree.List(ctx, directory, fsbind.ListLimits{MaxEntries: 4, MaxNameBytes: 1 << 20})
	if err != nil || !listing.Complete {
		if err != nil {
			return classifyJournalError(err)
		}
		return fmt.Errorf("%w: client adoption forget preflight inventory is incomplete", ErrIntegrity)
	}
	seenIntent, seenComplete := false, false
	pendingName := forgetPendingName(markerID)
	for _, entry := range listing.Entries {
		switch entry.Name {
		case retentionIntentName:
			seenIntent = entry.Kind == string(fsbind.ObjectKindRegular)
		case retentionCompleteName:
			seenComplete = entry.Kind == string(fsbind.ObjectKindRegular)
		case pendingName:
			if entry.Kind != string(fsbind.ObjectKindRegular) {
				return fmt.Errorf("%w: client adoption forget pending marker is unsafe", ErrIntegrity)
			}
			if _, verifyErr := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, pendingName}, raw); verifyErr != nil {
				return verifyErr
			}
		default:
			return fmt.Errorf("%w: client adoption forget preflight contains an unexpected object", ErrIntegrity)
		}
	}
	if !seenIntent || !seenComplete {
		return fmt.Errorf("%w: retained client adoption tombstone is incomplete", ErrIntegrity)
	}
	return classifyJournalError(handle.subtree.CheckPaths(fsbind.Path{}, directory))
}

func removeTombstoneFromForgetIntent(ctx context.Context, target *fsbind.Session, handle *journalHandle, marker ForgetIntent, report *ForgetReport) error {
	expectedOperation, err := fsbind.ParseIdentity(marker.OperationRootIdentity)
	if err != nil || handle == nil || handle.subtree == nil || !handle.subtree.Identity().Equal(expectedOperation) {
		return fmt.Errorf("%w: client adoption operation identity changed", ErrIntegrity)
	}
	directoryPath, _ := fsbind.PathFromComponents([]string{retentionDirectoryName})
	directory, directoryErr := handle.subtree.Inspect(ctx, directoryPath)
	if errors.Is(directoryErr, fsbind.ErrNotFound) {
		return removeForgetOperationRoot(ctx, target, handle, report)
	}
	if directoryErr != nil || directory.Kind != fsbind.ObjectKindDirectory {
		if directoryErr != nil {
			return classifyJournalError(directoryErr)
		}
		return fmt.Errorf("%w: client adoption retention directory is unsafe", ErrIntegrity)
	}
	listing, err := handle.subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 3, MaxNameBytes: 1 << 20})
	if err != nil || !listing.Complete {
		if err != nil {
			return classifyJournalError(err)
		}
		return fmt.Errorf("%w: client adoption forget recovery inventory is incomplete", ErrIntegrity)
	}
	for _, entry := range listing.Entries {
		if entry.Kind != string(fsbind.ObjectKindRegular) || entry.Name != retentionIntentName && entry.Name != retentionCompleteName {
			return fmt.Errorf("%w: client adoption forget recovery contains an unexpected object", ErrIntegrity)
		}
	}
	completeRaw, completeID, completeErr := EncodeRetentionComplete(marker.RetentionComplete)
	intentRaw, intentID, intentErr := EncodeRetentionIntent(marker.RetentionIntent)
	if completeErr != nil || intentErr != nil || completeID != marker.RetentionCompleteMarkerID || intentID != marker.RetentionIntentMarkerID {
		return fmt.Errorf("%w: retained client adoption tombstone cannot be reproduced", ErrIntegrity)
	}
	for _, selected := range []struct {
		name string
		raw  []byte
		dst  *fsbind.Removal
	}{{retentionCompleteName, completeRaw, &report.Removals.RetentionComplete}, {retentionIntentName, intentRaw, &report.Removals.RetentionIntent}} {
		path, _ := fsbind.PathFromComponents([]string{retentionDirectoryName, selected.name})
		object, inspectErr := handle.subtree.Inspect(ctx, path)
		if errors.Is(inspectErr, fsbind.ErrNotFound) {
			continue
		}
		if inspectErr != nil || object.Kind != fsbind.ObjectKindRegular || object.SizeBytes != int64(len(selected.raw)) {
			if inspectErr != nil {
				return classifyJournalError(inspectErr)
			}
			return fmt.Errorf("%w: retained client adoption marker is unsafe", ErrIntegrity)
		}
		identity, verifyErr := handle.verifyNamedBytes(ctx, []string{retentionDirectoryName, selected.name}, selected.raw)
		if verifyErr != nil || !identity.Equal(object.Identity) {
			if verifyErr != nil {
				return verifyErr
			}
			return fmt.Errorf("%w: retained client adoption marker identity changed", ErrIntegrity)
		}
		removal, removeErr := handle.subtree.RemoveRegularExact(ctx, path, object.Identity, object.SizeBytes)
		*selected.dst = removal
		report.recordForgetRemoval(removal)
		if removeErr != nil {
			return removeErr
		}
	}
	after, err := handle.subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 1, MaxNameBytes: 1 << 10})
	if err != nil || !after.Complete || len(after.Entries) != 0 {
		if err != nil {
			return classifyJournalError(err)
		}
		return fmt.Errorf("%w: client adoption retention directory is not empty after marker removal", ErrIntegrity)
	}
	directoryRemoval, removeErr := handle.subtree.RemoveEmptyDirectory(ctx, directoryPath, directory.Identity)
	report.Removals.RetentionDirectory = directoryRemoval
	report.recordForgetRemoval(directoryRemoval)
	if removeErr != nil {
		return removeErr
	}
	return removeForgetOperationRoot(ctx, target, handle, report)
}

func removeForgetOperationRoot(ctx context.Context, target *fsbind.Session, handle *journalHandle, report *ForgetReport) error {
	root, err := handle.subtree.List(ctx, fsbind.Path{}, fsbind.ListLimits{MaxEntries: 2, MaxNameBytes: 1 << 10})
	if err != nil || !root.Complete || len(root.Entries) != 1 || root.Entries[0].Name != operationLockEntryName || root.Entries[0].Kind != string(fsbind.ObjectKindRegular) {
		if err != nil {
			return classifyJournalError(err)
		}
		return fmt.Errorf("%w: client adoption operation root is not an exact lock-only namespace", ErrIntegrity)
	}
	removal, removeErr := target.RemovePrivateSubtreeExact(ctx, handle.subtree)
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

func forgetBlocked(report *ForgetReport, code, message string) (ForgetReport, error) {
	report.Outcome = ForgetOutcomeBlocked
	report.Blockers = append(report.Blockers, Finding{Code: code, Message: message})
	return *report, fmt.Errorf("%w: %s", ErrPolicy, message)
}

func forgetInterrupted(report *ForgetReport, err error, message string) (ForgetReport, error) {
	report.Outcome = ForgetOutcomeInterrupted
	report.Issues = append(report.Issues, Finding{Code: "forget.interrupted", Message: message})
	return *report, err
}

func forgetAbsentAfterAuthority(report *ForgetReport, message string) (ForgetReport, error) {
	report.Outcome = ForgetOutcomeAbsentUnattributed
	report.Operation.Status, report.Operation.PhaseAfter, report.Operation.Resumable = "absent_unattributed", "last_marker_disappeared", false
	report.Authority.State = "absent_after_exact_authority"
	report.Authority.MarkerDurable = false
	report.Authority.ExactTombstoneEvidenceAvailable = false
	report.Issues = append(report.Issues, Finding{Code: "forget.absent_unattributed", Message: message + "; completion cannot be attributed"})
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
		report.Blockers = append(report.Blockers, Finding{Code: "forget.policy_blocked", Message: message})
		return *report, err
	case errors.Is(err, ErrIntegrity), errors.Is(err, fsbind.ErrUnsafeObject), errors.Is(err, fsbind.ErrBindingChanged), errors.Is(err, fsbind.ErrCrossFilesystem):
		report.Outcome = ForgetOutcomeIntegrityFailed
		report.Operation.Resumable = false
		report.Issues = append(report.Issues, Finding{Code: "forget.integrity_failed", Message: message})
		return *report, fmt.Errorf("%w: %s", ErrIntegrity, message)
	case errors.Is(err, fsbind.ErrDurabilityUnconfirmed):
		report.Outcome = ForgetOutcomeDurabilityUnconfirmed
		report.WritesUncertain = true
		report.Issues = append(report.Issues, Finding{Code: "forget.durability_unconfirmed", Message: message})
		return *report, err
	default:
		report.Outcome = ForgetOutcomeInterrupted
		report.Issues = append(report.Issues, Finding{Code: "forget.operation_failed", Message: message})
		return *report, err
	}
}
