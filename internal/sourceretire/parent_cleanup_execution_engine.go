package sourceretire

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

// beforeParentCleanupRemovalHook is a deterministic package-test seam. The
// real target-root and exact parent-name checks always run after it.
var beforeParentCleanupRemovalHook func(sequence int, parent string) error

// afterParentCleanupReviewHook is a deterministic package-test seam between
// the zero-write review and the execution preflight. The real preflight always
// runs afterward and before any operation state is created.
var afterParentCleanupReviewHook func() error

// afterParentCleanupRemovalHook simulates a process stop after a durable
// namespace removal but before its completion marker is published.
var afterParentCleanupRemovalHook func(sequence int, parent string) error

// RunParentCleanup rebuilds the read-only eligibility plan in this invocation
// and crosses the removal boundary only after the exact reviewed plan ID is
// acknowledged. Serialized plan JSON is never accepted as authority.
func RunParentCleanup(ctx context.Context, options ParentCleanupRunOptions) (ParentCleanupExecutionReport, error) {
	report := newParentCleanupExecutionReport(options.Limits)
	if options.Limits.Validate() != nil {
		return parentCleanupExecutionBlocked(&report, "limits.invalid", "parent-cleanup execution limits are invalid")
	}
	if !options.Acknowledge {
		return parentCleanupExecutionBlocked(&report, "acknowledgement.required", "empty parent-directory removal requires its dedicated acknowledgement")
	}
	if !canonicalSHA256ID(options.ExpectedCleanupPlanID) {
		return parentCleanupExecutionBlocked(&report, "plan.id_invalid", "the expected parent-cleanup plan ID is invalid")
	}
	if err := ctx.Err(); err != nil {
		return parentCleanupExecutionInterrupted(&report, err, "parent cleanup was interrupted before live review")
	}
	operation, err := ParentCleanupOperationIDForPlanID(options.ExpectedCleanupPlanID)
	if err != nil || options.Review.TargetRoot == "" {
		return parentCleanupExecutionBlocked(&report, "selector.invalid", "the parent-cleanup target or plan selector is invalid")
	}
	report.Operation = ParentCleanupExecutionOperationReport{
		ID: operation.String(), PlanID: options.ExpectedCleanupPlanID, Status: "inspection_incomplete", Phase: "unknown",
	}
	// An existing durable operation is already the authority for resume. Check
	// it before rebuilding the now-historical empty-parent review, so rerunning
	// the same acknowledged command is idempotent after directories disappear.
	if existingTarget, targetInfo, bindErr := fsbind.BindExisting(options.Review.TargetRoot); bindErr == nil {
		existing, openErr := openParentCleanupJournal(ctx, existingTarget, operation, options.Limits)
		if openErr == nil {
			defer existingTarget.Close()
			defer existing.close()
			populateParentCleanupExecutionFromJournal(&report, existing, options.ShowAbsolutePaths)
			report.Target = ParentCleanupExecutionTargetReport{
				ExpectedRootIdentity: existing.state.Intent.TargetRootIdentity, ObservedRootIdentity: targetInfo.Identity.String(),
				RootIdentityBound: true, SameFilesystem: true,
			}
			if existing.state.Intent.CleanupPlanID != options.ExpectedCleanupPlanID {
				return parentCleanupExecutionIntegrity(&report, "the existing parent-cleanup operation disagrees with the expected plan")
			}
			if existing.state.Intent.RetirementOperationID != options.Review.OperationID ||
				existing.state.Intent.RetirementPlanID != options.Review.ExpectedPlanID {
				return parentCleanupExecutionBlocked(&report, "selector.retirement_mismatch", "the retirement selectors do not match the durable parent-cleanup intent")
			}
			roots, scopeID, scopeErr := normalizeExecutionRoots(ctx, options.Review.SearchRoots, options.Review.AllowNetwork)
			if scopeErr != nil {
				return mapParentCleanupExecutionError(&report, scopeErr, "the explicit source search-root scope could not be rebound")
			}
			if scopeID != existing.state.Intent.SearchScopeID {
				return parentCleanupExecutionBlocked(&report, "source.scope_mismatch", "the explicit source search-root scope does not match the durable cleanup intent")
			}
			report.addEffect("read_private_parent_cleanup_journal")
			return continueParentCleanup(ctx, &report, existing, roots, options.ShowAbsolutePaths)
		}
		_ = existingTarget.Close()
		if !errors.Is(openErr, ErrOperationNotFound) {
			return mapParentCleanupExecutionError(&report, openErr, "the existing parent-cleanup operation could not be inspected")
		}
		report.Operation.Status, report.Operation.Phase = "not_created", "planned"
	} else {
		return mapParentCleanupExecutionError(&report, bindErr, "the parent-cleanup journal root could not be bound")
	}

	reviewOptions := options.Review
	reviewOptions.ShowAbsolutePaths = false
	review, reviewErr := BuildParentCleanupPlan(ctx, reviewOptions)
	publicReview := publicParentCleanupReportCopy(review)
	report.Eligibility = &publicReview
	report.addEffect(review.Effect...)
	if review.Outcome != ParentCleanupOutcomeEligible || reviewErr != nil {
		return mapParentCleanupReviewFailure(&report, reviewErr)
	}
	if review.Plan.ID != options.ExpectedCleanupPlanID {
		return parentCleanupExecutionBlocked(&report, "plan.id_mismatch", "the same-invocation cleanup review does not reproduce the expected plan ID")
	}
	if review.authority == nil || review.authority.planID != review.Plan.ID ||
		review.authority.retirementOperationID.String() != review.Plan.RetirementOperationID ||
		review.authority.retirementPlanID != review.Plan.RetirementPlanID ||
		review.authority.retirementCompletionID != review.Plan.RetirementCompletionID ||
		review.authority.searchScopeID != review.Plan.SearchScopeID || !canonicalFSIdentity(review.authority.targetRootIdentity) {
		return parentCleanupExecutionIntegrity(&report, "the live cleanup review did not retain matching process-local authority")
	}
	if afterParentCleanupReviewHook != nil {
		if err := afterParentCleanupReviewHook(); err != nil {
			return parentCleanupExecutionInterrupted(&report, err, "parent cleanup stopped after live review")
		}
	}

	roots, scopeID, err := normalizeExecutionRoots(ctx, reviewOptions.SearchRoots, reviewOptions.AllowNetwork)
	if err != nil {
		return mapParentCleanupExecutionError(&report, err, "the source search-root scope could not be rebound after cleanup review")
	}
	if scopeID != review.authority.searchScopeID {
		return parentCleanupExecutionIntegrity(&report, "the source search-root scope changed after cleanup review")
	}
	target, targetInfo, err := fsbind.BindExisting(reviewOptions.TargetRoot)
	if err != nil {
		return mapParentCleanupExecutionError(&report, err, "the parent-cleanup journal root could not be bound")
	}
	defer target.Close()
	report.Target = ParentCleanupExecutionTargetReport{
		ExpectedRootIdentity: review.authority.targetRootIdentity, ObservedRootIdentity: targetInfo.Identity.String(),
		RootIdentityBound: true, SameFilesystem: true,
	}
	if targetInfo.Identity.String() != review.authority.targetRootIdentity {
		return parentCleanupExecutionIntegrity(&report, "the parent-cleanup journal root changed after live review")
	}
	if operation, err = ParentCleanupOperationIDForPlanID(review.Plan.ID); err != nil {
		return parentCleanupExecutionIntegrity(&report, "the parent-cleanup operation ID could not be derived")
	}
	report.Operation = ParentCleanupExecutionOperationReport{ID: operation.String(), PlanID: review.Plan.ID, Status: "initializing", Phase: "planned"}
	intent, err := parentCleanupIntentFromAuthority(operation, review.authority, options.Limits, roots)
	if err != nil {
		return mapParentCleanupExecutionError(&report, err, "the live cleanup authority cannot be represented by the execution protocol")
	}
	if err := preflightParentCleanupJournalEncoding(intent, targetInfo.Identity.String()); err != nil {
		return mapParentCleanupExecutionError(&report, err, "the parent-cleanup journal protocol exceeds its reviewed limits")
	}
	// Prove every reviewed parent can be rebound through fsbind and remains
	// empty before the first private journal write. Unsupported/network
	// filesystems and predictable namespace changes therefore remain zero-write
	// policy failures rather than abandoned operation state.
	for _, directory := range intent.Directories {
		observation, observeErr := observeCleanupParent(ctx, directory, roots, intent.Limits.MaxEntryNameBytes)
		recordParentObservation(&report, observation)
		if observeErr != nil {
			return mapParentCleanupExecutionError(&report, observeErr, "a reviewed parent could not be rebound before journal initialization")
		}
		if observation.absent {
			return parentCleanupExecutionIntegrity(&report, "a reviewed parent disappeared before journal initialization")
		}
		if !observation.empty {
			return parentCleanupExecutionBlocked(&report, "parent.not_empty", "a reviewed parent became non-empty before journal initialization")
		}
	}
	report.addEffect("read_private_parent_cleanup_journal", "write_private_parent_cleanup_journal")
	journal, creation, directories, marker, err := initializeParentCleanupJournal(ctx, target, intent)
	recordParentCleanupJournalInitialization(&report, creation, directories, marker)
	if err != nil {
		return mapParentCleanupExecutionError(&report, err, "the parent-cleanup journal could not be initialized")
	}
	defer journal.close()
	populateParentCleanupExecutionFromJournal(&report, journal, options.ShowAbsolutePaths)
	return continueParentCleanup(ctx, &report, journal, roots, options.ShowAbsolutePaths)
}

// ResumeParentCleanup continues one explicit operation from its private
// journal. The journal supplies exact path and identity authority; the caller
// must still re-supply the same normalized search-root scope.
func ResumeParentCleanup(ctx context.Context, options ParentCleanupResumeOptions) (ParentCleanupExecutionReport, error) {
	report := newParentCleanupExecutionReport(options.Limits)
	report.Operation = ParentCleanupExecutionOperationReport{ID: options.OperationID.String(), PlanID: options.ExpectedCleanupPlanID, Status: "inspection_incomplete", Phase: "unknown"}
	if options.Limits.Validate() != nil || options.TargetRoot == "" || len(options.SearchRoots) == 0 {
		return parentCleanupExecutionBlocked(&report, "selector.invalid", "parent-cleanup resume selectors or limits are invalid")
	}
	if !options.Acknowledge {
		return parentCleanupExecutionBlocked(&report, "acknowledgement.required", "parent-cleanup resume requires its dedicated acknowledgement")
	}
	parsed, parseErr := ParseParentCleanupOperationID(options.OperationID.String())
	derived, deriveErr := ParentCleanupOperationIDForPlanID(options.ExpectedCleanupPlanID)
	if parseErr != nil || deriveErr != nil || parsed != options.OperationID || derived != options.OperationID {
		return parentCleanupExecutionBlocked(&report, "selector.invalid", "the parent-cleanup operation and plan selectors disagree")
	}
	if err := ctx.Err(); err != nil {
		return parentCleanupExecutionInterrupted(&report, err, "parent-cleanup resume was interrupted")
	}
	target, targetInfo, err := fsbind.BindExisting(options.TargetRoot)
	if err != nil {
		return mapParentCleanupExecutionError(&report, err, "the parent-cleanup journal root could not be bound")
	}
	defer target.Close()
	report.addEffect("read_private_parent_cleanup_journal")
	journal, err := openParentCleanupJournal(ctx, target, options.OperationID, options.Limits)
	if err != nil {
		return mapParentCleanupExecutionError(&report, err, "the parent-cleanup journal could not be opened")
	}
	defer journal.close()
	populateParentCleanupExecutionFromJournal(&report, journal, options.ShowAbsolutePaths)
	report.Target = ParentCleanupExecutionTargetReport{
		ExpectedRootIdentity: journal.state.Intent.TargetRootIdentity, ObservedRootIdentity: targetInfo.Identity.String(),
		RootIdentityBound: true, SameFilesystem: true,
	}
	if journal.state.Intent.CleanupPlanID != options.ExpectedCleanupPlanID || targetInfo.Identity.String() != journal.state.Intent.TargetRootIdentity {
		return parentCleanupExecutionIntegrity(&report, "the parent-cleanup selectors or target-root identity disagree with the durable intent")
	}
	roots, scopeID, err := normalizeExecutionRoots(ctx, options.SearchRoots, options.AllowNetwork)
	if err != nil {
		return mapParentCleanupExecutionError(&report, err, "the explicit source search-root scope could not be rebound")
	}
	if scopeID != journal.state.Intent.SearchScopeID {
		return parentCleanupExecutionBlocked(&report, "source.scope_mismatch", "the explicit source search-root scope does not match the durable cleanup intent")
	}
	if err := validateParentCleanupIntentScope(journal.state.Intent, roots); err != nil {
		return mapParentCleanupExecutionError(&report, err, "the durable parent-cleanup paths do not fit the explicit source scope")
	}
	return continueParentCleanup(ctx, &report, journal, roots, options.ShowAbsolutePaths)
}

// ParentCleanupStatus reads historical private-journal evidence only. It does
// not access source roots or assert that a historically removed name remains
// absent now.
func ParentCleanupStatus(ctx context.Context, options ParentCleanupStatusOptions) (ParentCleanupExecutionReport, error) {
	report := newParentCleanupExecutionReport(options.Limits)
	report.Operation = ParentCleanupExecutionOperationReport{ID: options.OperationID.String(), Status: "inspection_incomplete", Phase: "unknown"}
	parsed, parseErr := ParseParentCleanupOperationID(options.OperationID.String())
	if options.Limits.Validate() != nil || options.TargetRoot == "" || parseErr != nil || parsed != options.OperationID {
		return parentCleanupExecutionBlocked(&report, "selector.invalid", "parent-cleanup status selectors are invalid")
	}
	if err := ctx.Err(); err != nil {
		return parentCleanupExecutionInterrupted(&report, err, "parent-cleanup status was interrupted")
	}
	target, targetInfo, err := fsbind.BindExisting(options.TargetRoot)
	if err != nil {
		return mapParentCleanupExecutionError(&report, err, "the parent-cleanup journal root could not be bound")
	}
	defer target.Close()
	report.addEffect("read_private_parent_cleanup_journal")
	journal, err := openParentCleanupJournal(ctx, target, options.OperationID, options.Limits)
	if err != nil {
		return mapParentCleanupExecutionError(&report, err, "the parent-cleanup journal could not be opened")
	}
	defer journal.close()
	populateParentCleanupExecutionFromJournal(&report, journal, options.ShowAbsolutePaths)
	report.Target = ParentCleanupExecutionTargetReport{
		ExpectedRootIdentity: journal.state.Intent.TargetRootIdentity, ObservedRootIdentity: targetInfo.Identity.String(),
		RootIdentityBound: true, SameFilesystem: true,
	}
	report.Warnings = append(report.Warnings, "status does not open source parents; all removal and absence claims are historical journal evidence")
	if journal.state.CompletePresent {
		report.Outcome, report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = ParentCleanupExecutionOutcomeAlreadyRemoved, "historical_complete", "complete", false
	} else {
		report.Outcome, report.Operation.Status, report.Operation.Resumable = ParentCleanupExecutionOutcomePartial, "active", true
	}
	report.finalize()
	return report, nil
}

func continueParentCleanup(ctx context.Context, report *ParentCleanupExecutionReport, journal *parentCleanupJournal, roots []string, showPaths bool) (ParentCleanupExecutionReport, error) {
	report.addEffect("read_retired_source_parent_namespaces")
	if journal.state.CompletePresent {
		if err := requireCleanupParentsAbsent(ctx, report, journal, roots); err != nil {
			return mapParentCleanupExecutionError(report, err, "a historically removed parent directory is currently present or unobservable")
		}
		report.Outcome, report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = ParentCleanupExecutionOutcomeAlreadyRemoved, "complete", "complete", false
		report.finalize()
		return *report, nil
	}
	for sequence, directory := range journal.state.Intent.Directories {
		if err := ctx.Err(); err != nil {
			return parentCleanupExecutionInterrupted(report, err, "parent cleanup was interrupted")
		}
		observation, err := observeCleanupParent(ctx, directory, roots, journal.limits.MaxEntryNameBytes)
		recordParentObservation(report, observation)
		if err != nil {
			return mapParentCleanupExecutionError(report, err, "a reviewed source parent could not be safely observed")
		}
		if journal.state.RemovedPresent[sequence] {
			if !observation.absent {
				return parentCleanupExecutionIntegrity(report, "a historically removed parent directory reappeared")
			}
			continue
		}
		if observation.absent {
			if !journal.state.AttemptPresent[sequence] {
				return parentCleanupExecutionIntegrity(report, "a reviewed parent disappeared before its durable removal attempt")
			}
			if err := confirmCleanupParentAbsent(ctx, directory, roots); err != nil {
				return mapParentCleanupExecutionError(report, err, "a recovered parent removal could not confirm namespace durability")
			}
			report.addEffect("write_private_parent_cleanup_journal")
			_, receipt, markerErr := journal.publishRemoved(ctx, sequence, ParentCleanupRemovalBasisRecovered)
			recordParentCleanupJournalWrite(report, receipt)
			if markerErr != nil {
				return mapParentCleanupExecutionError(report, markerErr, "a recovered parent removal could not be journaled")
			}
			populateParentCleanupExecutionFromJournal(report, journal, showPaths)
			continue
		}
		if !observation.empty {
			return parentCleanupExecutionPartial(report, "parent.not_empty", "a reviewed parent is currently non-empty; no directory was removed")
		}
		if !journal.state.AttemptPresent[sequence] {
			report.addEffect("write_private_parent_cleanup_journal")
			_, receipt, markerErr := journal.publishAttempt(ctx, sequence)
			recordParentCleanupJournalWrite(report, receipt)
			if markerErr != nil {
				return mapParentCleanupExecutionError(report, markerErr, "a parent-removal attempt could not be journaled")
			}
			populateParentCleanupExecutionFromJournal(report, journal, showPaths)
			// The journal publication is an intentional delay. Reobserve the
			// exact parent after it, so removal never relies on the planning read.
			observation, err = observeCleanupParent(ctx, directory, roots, journal.limits.MaxEntryNameBytes)
			recordParentObservation(report, observation)
			if err != nil {
				return mapParentCleanupExecutionError(report, err, "a reviewed parent changed after its durable removal attempt")
			}
			if observation.absent {
				if err := confirmCleanupParentAbsent(ctx, directory, roots); err != nil {
					return mapParentCleanupExecutionError(report, err, "a concurrent parent removal could not confirm namespace durability")
				}
				report.addEffect("write_private_parent_cleanup_journal")
				_, receipt, markerErr = journal.publishRemoved(ctx, sequence, ParentCleanupRemovalBasisRecovered)
				recordParentCleanupJournalWrite(report, receipt)
				if markerErr != nil {
					return mapParentCleanupExecutionError(report, markerErr, "a recovered parent removal could not be journaled")
				}
				populateParentCleanupExecutionFromJournal(report, journal, showPaths)
				continue
			}
			if !observation.empty {
				return parentCleanupExecutionPartial(report, "parent.not_empty", "a reviewed parent became non-empty after its durable removal attempt")
			}
		}
		if beforeParentCleanupRemovalHook != nil {
			if err := beforeParentCleanupRemovalHook(sequence, directory.ParentPath); err != nil {
				return parentCleanupExecutionInterrupted(report, err, "parent cleanup was interrupted before directory removal")
			}
		}
		if err := journal.target.Check(); err != nil {
			return mapParentCleanupExecutionError(report, err, "the parent-cleanup journal root changed before directory removal")
		}
		report.addEffect("remove_exact_empty_source_parent_directories")
		removal, removeErr := removeCleanupParent(ctx, directory, roots)
		recordParentDirectoryRemoval(report, removal, removeErr)
		if errors.Is(removeErr, fsbind.ErrNotEmpty) {
			return parentCleanupExecutionPartial(report, "parent.not_empty", "the parent became non-empty at the exact removal boundary")
		}
		if removeErr != nil || !removal.Removed || removal.Durability != fsbind.DurabilityConfirmed {
			if removeErr == nil {
				removeErr = fsbind.ErrRemovalAmbiguous
			}
			return mapParentCleanupExecutionError(report, removeErr, "an empty parent-directory removal did not reach a confirmed durable result")
		}
		if afterParentCleanupRemovalHook != nil {
			if err := afterParentCleanupRemovalHook(sequence, directory.ParentPath); err != nil {
				return parentCleanupExecutionInterrupted(report, err, "parent cleanup stopped after a durable directory removal")
			}
		}
		report.addEffect("write_private_parent_cleanup_journal")
		_, receipt, markerErr := journal.publishRemoved(ctx, sequence, ParentCleanupRemovalBasisConfirmed)
		recordParentCleanupJournalWrite(report, receipt)
		if markerErr != nil {
			return mapParentCleanupExecutionError(report, markerErr, "a confirmed parent-directory removal could not be journaled")
		}
		populateParentCleanupExecutionFromJournal(report, journal, showPaths)
	}
	if err := requireCleanupParentsAbsent(ctx, report, journal, roots); err != nil {
		return mapParentCleanupExecutionError(report, err, "the removed parent-directory set could not be reverified before completion")
	}
	report.addEffect("write_private_parent_cleanup_journal")
	_, receipt, err := journal.publishComplete(ctx)
	recordParentCleanupJournalWrite(report, receipt)
	if err != nil {
		return mapParentCleanupExecutionError(report, err, "parent-cleanup completion could not be journaled")
	}
	populateParentCleanupExecutionFromJournal(report, journal, showPaths)
	report.Outcome, report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = ParentCleanupExecutionOutcomeRemoved, "complete", "complete", false
	report.finalize()
	return *report, nil
}

type cleanupParentObservation struct {
	absent    bool
	empty     bool
	reads     int
	entries   int
	nameBytes int64
}

func observeCleanupParent(ctx context.Context, directory ParentCleanupIntentDirectory, roots []string, maxNameBytes int64) (cleanupParentObservation, error) {
	result := cleanupParentObservation{}
	if err := validateCleanupParentPath(directory.ParentPath, directory.ParentPathRef, roots); err != nil {
		return result, err
	}
	expected, err := fsbind.ParseIdentity(directory.ParentIdentity)
	if err != nil || expected.IsZero() {
		return result, fmt.Errorf("%w: parent identity is invalid", ErrExecutionIntegrity)
	}
	grandparent, name := filepath.Dir(directory.ParentPath), filepath.Base(directory.ParentPath)
	parentOfParent, _, err := fsbind.BindExisting(grandparent)
	if err != nil {
		return result, classifyExecutionBindingError(err)
	}
	observed, inspectErr := parentOfParent.InspectRoot(ctx, name)
	checkErr := parentOfParent.Check()
	closeErr := parentOfParent.Close()
	if errors.Is(inspectErr, fsbind.ErrNotFound) {
		if checkErr != nil {
			return result, classifyExecutionBindingError(checkErr)
		}
		if closeErr != nil {
			return result, closeErr
		}
		result.absent = true
		return result, nil
	}
	if inspectErr != nil {
		return result, classifyExecutionBindingError(inspectErr)
	}
	if checkErr != nil {
		return result, classifyExecutionBindingError(checkErr)
	}
	if closeErr != nil {
		return result, closeErr
	}
	if observed.Kind != fsbind.ObjectKindDirectory || !observed.Identity.Equal(expected) {
		return result, fmt.Errorf("%w: reviewed parent identity changed", ErrExecutionIntegrity)
	}
	parent, _, err := fsbind.BindExisting(directory.ParentPath)
	if err != nil {
		return result, classifyExecutionBindingError(err)
	}
	if !parent.Info().Identity.Equal(expected) {
		_ = parent.Close()
		return result, fmt.Errorf("%w: reviewed parent identity changed", ErrExecutionIntegrity)
	}
	listing, listErr := parent.ListRoot(ctx, fsbind.ListLimits{MaxEntries: 1, MaxNameBytes: maxNameBytes})
	result.reads = 1
	result.entries = listing.Used.EntriesExamined
	result.nameBytes = listing.Used.NameBytes
	checkErr = parent.Check()
	closeErr = parent.Close()
	if listErr != nil {
		return result, classifyExecutionBindingError(listErr)
	}
	if checkErr != nil {
		return result, classifyExecutionBindingError(checkErr)
	}
	if closeErr != nil {
		return result, closeErr
	}
	if listing.Complete && listing.Used.EntriesExamined == 0 && len(listing.Entries) == 0 {
		result.empty = true
		return result, nil
	}
	if listing.Used.EntriesExamined > 0 {
		return result, nil
	}
	return result, fmt.Errorf("parent namespace observation was incomplete")
}

func removeCleanupParent(ctx context.Context, directory ParentCleanupIntentDirectory, roots []string) (fsbind.Removal, error) {
	result := fsbind.Removal{Durability: fsbind.DurabilityNotPublished, Kind: fsbind.ObjectKindDirectory}
	if err := validateCleanupParentPath(directory.ParentPath, directory.ParentPathRef, roots); err != nil {
		return result, err
	}
	expected, err := fsbind.ParseIdentity(directory.ParentIdentity)
	if err != nil || expected.IsZero() {
		return result, fmt.Errorf("%w: parent identity is invalid", ErrExecutionIntegrity)
	}
	grandparent, name := filepath.Dir(directory.ParentPath), filepath.Base(directory.ParentPath)
	session, _, err := fsbind.BindExisting(grandparent)
	if err != nil {
		return result, classifyExecutionBindingError(err)
	}
	result, removeErr := session.RemoveRootEmptyDirectoryExact(ctx, name, expected)
	closeErr := session.Close()
	if removeErr != nil {
		return result, removeErr
	}
	if closeErr != nil {
		return result, closeErr
	}
	return result, nil
}

func confirmCleanupParentAbsent(ctx context.Context, directory ParentCleanupIntentDirectory, roots []string) error {
	if err := validateCleanupParentPath(directory.ParentPath, directory.ParentPathRef, roots); err != nil {
		return err
	}
	grandparent, name := filepath.Dir(directory.ParentPath), filepath.Base(directory.ParentPath)
	session, _, err := fsbind.BindExisting(grandparent)
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = session.Close()
		}
	}()
	if _, err := session.InspectRoot(ctx, name); !errors.Is(err, fsbind.ErrNotFound) {
		if err == nil {
			err = fmt.Errorf("%w: parent name exists during removal recovery", ErrExecutionIntegrity)
		}
		return classifyExecutionBindingError(err)
	}
	if err := session.SyncRoot(ctx); err != nil {
		return classifyExecutionBindingError(err)
	}
	if _, err := session.InspectRoot(ctx, name); !errors.Is(err, fsbind.ErrNotFound) {
		if err == nil {
			err = fmt.Errorf("%w: parent name reappeared during removal recovery", ErrExecutionIntegrity)
		}
		return classifyExecutionBindingError(err)
	}
	if err := session.Check(); err != nil {
		return classifyExecutionBindingError(err)
	}
	closeErr := session.Close()
	closed = true
	return closeErr
}

func requireCleanupParentsAbsent(ctx context.Context, report *ParentCleanupExecutionReport, journal *parentCleanupJournal, roots []string) error {
	for sequence, directory := range journal.state.Intent.Directories {
		if !journal.state.RemovedPresent[sequence] {
			return fmt.Errorf("%w: parent cleanup is incomplete", ErrExecutionIntegrity)
		}
		observation, err := observeCleanupParent(ctx, directory, roots, journal.limits.MaxEntryNameBytes)
		recordParentObservation(report, observation)
		if err != nil {
			return err
		}
		if !observation.absent {
			return fmt.Errorf("%w: a removed parent directory is currently present", ErrExecutionIntegrity)
		}
	}
	return nil
}

func recordParentObservation(report *ParentCleanupExecutionReport, observation cleanupParentObservation) {
	report.Used.DirectoryReads += observation.reads
	report.Used.EntriesObserved += observation.entries
	report.Used.EntryNameBytes += observation.nameBytes
}

func parentCleanupIntentFromAuthority(operation ParentCleanupOperationID, authority *verifiedParentCleanupAuthority,
	limits ParentCleanupExecutionLimits, roots []string) (ParentCleanupIntent, error) {
	if authority == nil || len(authority.candidates) == 0 || len(authority.candidates) > limits.MaxParents {
		return ParentCleanupIntent{}, fmt.Errorf("%w: parent-cleanup authority is unavailable or over budget", ErrExecutionPolicy)
	}
	directories := make([]ParentCleanupIntentDirectory, len(authority.candidates))
	var pathBytes int64
	for sequence, candidate := range authority.candidates {
		if err := validateCleanupParentPath(candidate.path, parentCleanupPathRef(candidate.path), roots); err != nil {
			return ParentCleanupIntent{}, err
		}
		pathBytes += int64(len(candidate.path))
		if pathBytes < 0 || pathBytes > limits.MaxPathBytes || candidate.files <= 0 || !canonicalFSIdentity(candidate.identity) {
			return ParentCleanupIntent{}, fmt.Errorf("%w: parent-cleanup authority exceeds its reviewed budget", ErrExecutionPolicy)
		}
		directories[sequence] = ParentCleanupIntentDirectory{
			Sequence: sequence, ParentPath: candidate.path, ParentPathRef: parentCleanupPathRef(candidate.path),
			ParentIdentity: candidate.identity, RetiredFiles: candidate.files,
		}
	}
	intent := ParentCleanupIntent{
		Schema: ParentCleanupIntentSchemaV1, OperationID: operation, TargetRootIdentity: authority.targetRootIdentity,
		CleanupPlanID: authority.planID, RetirementOperationID: authority.retirementOperationID,
		RetirementPlanID: authority.retirementPlanID, RetirementCompletionID: authority.retirementCompletionID,
		SearchScopeID: authority.searchScopeID, Limits: limits, Directories: directories,
	}
	return intent, nil
}

func validateParentCleanupIntentScope(intent ParentCleanupIntent, roots []string) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	for _, directory := range intent.Directories {
		if err := validateCleanupParentPath(directory.ParentPath, directory.ParentPathRef, roots); err != nil {
			return err
		}
	}
	return nil
}

func validateCleanupParentPath(path, pathRef string, roots []string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || parentCleanupPathRef(path) != pathRef ||
		!withinExecutionRoots(roots, path) || exactCleanupRoot(roots, path) {
		return fmt.Errorf("%w: parent-cleanup path lies outside the explicit source scope", ErrExecutionPolicy)
	}
	name := filepath.Base(path)
	if filepath.Dir(path) == path || fsbind.ValidatePathComponents([]string{name}) != nil || materialize.IsReservedControlName(name) {
		return fmt.Errorf("%w: parent-cleanup path is unsafe", ErrExecutionPolicy)
	}
	return nil
}

func mapParentCleanupReviewFailure(report *ParentCleanupExecutionReport, err error) (ParentCleanupExecutionReport, error) {
	if report.Eligibility == nil {
		return parentCleanupExecutionIntegrity(report, "the live parent-cleanup review result is unavailable")
	}
	switch report.Eligibility.Outcome {
	case ParentCleanupOutcomeBlocked, ParentCleanupOutcomeNothingToClean:
		report.Outcome = ParentCleanupExecutionOutcomeBlocked
	case ParentCleanupOutcomeIntegrity:
		report.Outcome = ParentCleanupExecutionOutcomeIntegrity
	default:
		report.Outcome = ParentCleanupExecutionOutcomeIncomplete
	}
	report.Blockers = append(report.Blockers, report.Eligibility.Blockers...)
	report.Issues = append(report.Issues, report.Eligibility.Issues...)
	if report.Eligibility.Outcome == ParentCleanupOutcomeNothingToClean {
		report.addBlocker("cleanup.nothing_to_clean", "the same-invocation review found no eligible empty immediate parent")
	}
	report.finalize()
	if report.Outcome == ParentCleanupExecutionOutcomeIntegrity {
		return *report, fmt.Errorf("%w: live parent-cleanup review changed", ErrExecutionIntegrity)
	}
	return *report, err
}

func mapParentCleanupExecutionError(report *ParentCleanupExecutionReport, err error, message string) (ParentCleanupExecutionReport, error) {
	switch {
	case errors.Is(err, ErrExecutionPolicy), errors.Is(err, fsbind.ErrUnsupported), errors.Is(err, fsbind.ErrAlreadyExists):
		return parentCleanupExecutionBlocked(report, "execution.blocked", message)
	case errors.Is(err, ErrExecutionIntegrity), errors.Is(err, fsbind.ErrUnsafeObject), errors.Is(err, fsbind.ErrBindingChanged), errors.Is(err, fsbind.ErrCrossFilesystem):
		return parentCleanupExecutionIntegrity(report, message)
	case errors.Is(err, ErrOperationNotFound):
		report.Outcome = ParentCleanupExecutionOutcomeBlocked
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "not_found", "unknown", false
		report.addBlocker("operation.not_found", message)
		report.finalize()
		return *report, nil
	default:
		return parentCleanupExecutionInterrupted(report, err, message)
	}
}

func parentCleanupExecutionBlocked(report *ParentCleanupExecutionReport, code, message string) (ParentCleanupExecutionReport, error) {
	report.Outcome = ParentCleanupExecutionOutcomeBlocked
	report.addBlocker(code, message)
	report.finalize()
	return *report, nil
}

func parentCleanupExecutionPartial(report *ParentCleanupExecutionReport, code, message string) (ParentCleanupExecutionReport, error) {
	report.Outcome, report.Operation.Resumable = ParentCleanupExecutionOutcomePartial, report.Operation.IntentID != "" && report.Operation.Phase != "complete"
	report.addBlocker(code, message)
	report.finalize()
	return *report, nil
}

func parentCleanupExecutionIntegrity(report *ParentCleanupExecutionReport, message string) (ParentCleanupExecutionReport, error) {
	report.Outcome, report.Operation.Resumable = ParentCleanupExecutionOutcomeIntegrity, false
	report.addBlocker("integrity.failed", message)
	report.finalize()
	return *report, fmt.Errorf("%w: %s", ErrExecutionIntegrity, message)
}

func parentCleanupExecutionInterrupted(report *ParentCleanupExecutionReport, err error, message string) (ParentCleanupExecutionReport, error) {
	if report.DeletionPerformed || report.Operation.IntentID != "" {
		report.Outcome = ParentCleanupExecutionOutcomePartial
	} else {
		report.Outcome = ParentCleanupExecutionOutcomeIncomplete
	}
	report.Operation.Resumable = report.Operation.IntentID != "" && report.Operation.Phase != "complete"
	report.addIssue("operation.interrupted", message)
	report.finalize()
	return *report, err
}
