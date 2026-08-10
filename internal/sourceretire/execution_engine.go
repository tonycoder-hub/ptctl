package sourceretire

import (
	"context"
	"errors"
	"fmt"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
)

// beforeSourceRemovalHook is a deterministic package-test seam. The real
// bound-target check always runs after it and immediately before unlink.
var beforeSourceRemovalHook func(sequence int) error

// Run rebuilds the zero-write review in this invocation and only then crosses
// the separately acknowledged deletion boundary. ExpectedPlanID is a review
// guard; serialized plan JSON is never parsed or accepted as authority.
func Run(ctx context.Context, options RunOptions) (ExecutionReport, error) {
	report := newExecutionReport(options.Limits)
	if err := options.Limits.Validate(); err != nil {
		return executionBlocked(&report, "limits.invalid", "source retirement execution limits are invalid")
	}
	if !options.Acknowledge {
		return executionBlocked(&report, "acknowledgement.required", "source deletion requires its dedicated acknowledgement")
	}
	if !canonicalSHA256ID(options.ExpectedPlanID) {
		return executionBlocked(&report, "plan.id_invalid", "the expected source retirement plan ID is invalid")
	}
	if err := ctx.Err(); err != nil {
		return executionInterrupted(&report, err, "source retirement was interrupted before live review")
	}
	reviewOptions := options.Review
	// Effectful execution reports never disclose source paths, even when a
	// caller reused presentation options from the read-only planning command.
	reviewOptions.ShowAbsolutePaths = false
	review, reviewErr := Build(ctx, reviewOptions)
	report.Eligibility = &review
	report.addEffect(review.Effect...)
	report.Final, report.ClientUse = review.Final, review.ClientUse
	if review.Outcome != OutcomeEligible || reviewErr != nil {
		return mapReviewFailure(&report, reviewErr)
	}
	if review.Plan.ID != options.ExpectedPlanID {
		return executionBlocked(&report, "plan.id_mismatch", "the same-invocation live review does not reproduce the expected plan ID")
	}
	if review.execution == nil || review.execution.clientBefore == nil || !review.execution.clientBefore.Verified() || review.execution.currentUse == nil {
		return executionIntegrity(&report, "the live source-retirement review did not retain process-local execution authority")
	}
	roots, scopeID, err := normalizeExecutionRoots(ctx, options.SearchRoots)
	if err != nil {
		return mapExecutionError(&report, err, "source search-root scope could not be bound")
	}
	sources, intentFiles, err := bindRunSources(ctx, options.Review.Meta, options.Review.Discovery, options.Review.Final, review.Plan, roots)
	if err != nil {
		return mapExecutionError(&report, err, "source names could not be rebound for deletion")
	}
	defer sources.close()
	targetPath, ok := options.Review.Final.ProcessTargetRoot()
	if !ok || targetPath == "" {
		return executionIntegrity(&report, "the materialized final lost its target-root authority")
	}
	target, targetInfo, err := fsbind.BindExisting(targetPath)
	if err != nil {
		return mapExecutionError(&report, err, "source retirement target root could not be bound")
	}
	defer target.Close()
	if targetInfo.Identity.String() != review.Plan.TargetRootIdentity {
		return executionIntegrity(&report, "the source retirement target-root identity changed after review")
	}
	operation, err := executionOperationID(review.Plan.ID)
	if err != nil {
		return executionIntegrity(&report, "the source retirement operation ID could not be derived")
	}
	report.Operation = ExecutionOperationReport{ID: operation.String(), PlanID: review.Plan.ID, Status: "initializing", Phase: "planned"}
	if retained, retentionErr := applyExecutionRetentionControl(ctx, target, operation, &report); retained {
		if retentionErr != nil {
			return mapExecutionError(&report, retentionErr, "source retirement retention state could not be inspected")
		}
		return report, nil
	}
	intent := executionIntentFromPlan(operation, scopeID, review.Plan, options.Limits, intentFiles)
	if err := preflightExecutionJournalEncoding(intent, targetInfo.Identity.String()); err != nil {
		return mapExecutionError(&report, err, "source retirement journal protocol exceeds its reviewed limits")
	}
	report.addEffect("read_private_source_retirement_journal", "write_private_source_retirement_journal")
	journal, creation, directories, marker, err := initializeExecutionJournal(ctx, target, intent)
	recordJournalInitialization(&report, creation, directories, marker)
	if err != nil {
		return mapExecutionError(&report, err, "source retirement journal could not be initialized")
	}
	defer journal.close()
	populateExecutionFromJournal(&report, journal)
	return continueSourceRetirement(ctx, &report, journal, sources, options.Review.Meta, options.Review.Final,
		review.execution.currentUse, options.Review.ClientSession, review.execution.clientBefore)
}

// Resume continues only an explicit operation. It does not rediscover source
// candidates: the private durable intent supplies exact names and identities,
// while the caller must re-supply the same root scope and live authorities.
func Resume(ctx context.Context, options ResumeOptions) (ExecutionReport, error) {
	report := newExecutionReport(options.Limits)
	report.Operation = ExecutionOperationReport{ID: options.OperationID.String(), PlanID: options.ExpectedPlanID, Status: "inspection_incomplete", Phase: "unknown"}
	if err := options.Limits.Validate(); err != nil {
		return executionBlocked(&report, "limits.invalid", "source retirement execution limits are invalid")
	}
	if !options.Acknowledge {
		return executionBlocked(&report, "acknowledgement.required", "source deletion resume requires its dedicated acknowledgement")
	}
	if _, err := ParseOperationID(options.OperationID.String()); err != nil || !canonicalSHA256ID(options.ExpectedPlanID) || options.TargetRoot == "" {
		return executionBlocked(&report, "selector.invalid", "source retirement resume selectors are invalid")
	}
	if err := ctx.Err(); err != nil {
		return executionInterrupted(&report, err, "source retirement resume was interrupted")
	}
	target, _, err := fsbind.BindExisting(options.TargetRoot)
	if err != nil {
		return mapExecutionError(&report, err, "source retirement target root could not be bound")
	}
	defer target.Close()
	report.addEffect("read_private_source_retirement_journal")
	if retained, retentionErr := applyExecutionRetentionControl(ctx, target, options.OperationID, &report); retained {
		if retentionErr != nil {
			return mapExecutionError(&report, retentionErr, "source retirement retention state could not be inspected")
		}
		return report, nil
	}
	journal, err := openExecutionJournal(ctx, target, options.OperationID, options.Limits)
	if err != nil {
		return mapExecutionError(&report, err, "source retirement journal could not be opened")
	}
	defer journal.close()
	populateExecutionFromJournal(&report, journal)
	if journal.state.Intent.PlanID != options.ExpectedPlanID {
		return executionBlocked(&report, "plan.id_mismatch", "the expected plan ID does not select this source retirement operation")
	}
	roots, scopeID, err := normalizeExecutionRoots(ctx, options.SearchRoots)
	if err != nil || scopeID != journal.state.Intent.SearchScopeID {
		return executionBlocked(&report, "source.scope_mismatch", "the explicit source search-root scope does not match the durable intent")
	}
	if err := validateResumeAuthorities(journal.state.Intent, options); err != nil {
		return mapExecutionError(&report, err, "source retirement resume authorities disagree with the durable intent")
	}
	report.addEffect("read_source_metadata", "read_exact_materialized_final")
	sources, err := bindIntentSources(ctx, journal.state, roots)
	if err != nil {
		return mapExecutionError(&report, err, "durable source names could not be rebound")
	}
	defer sources.close()
	freshFinal, finalObservation, err := options.Final.Reverify(ctx)
	if err != nil {
		return mapFinalExecutionError(&report, err, "the materialized final could not be reverified before resume")
	}
	if freshFinal == nil || finalObservation.FinalObjectIdentity != journal.state.Intent.FinalObjectIdentity {
		return executionIntegrity(&report, "the materialized final identity changed before resume")
	}
	report.Final = finalObservation
	if journal.state.CompletePresent {
		if err := requireRetiredNamesAbsent(ctx, journal.state, sources); err != nil {
			return mapExecutionError(&report, err, "a retired source name reappeared")
		}
		report.Outcome, report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = ExecutionOutcomeAlreadyRetired, "complete", "complete", false
		report.finalize()
		return report, nil
	}
	if options.ClientSession == nil || options.ClientUse == nil {
		return executionBlocked(&report, "client.authority_unavailable", "resume requires one live client-use authority and read session")
	}
	report.addEffect("read_source_content", "read_live_downloader_ledger")
	before, beforeObservation, err := clientactivate.VerifyCurrentUse(ctx, options.ClientUse, options.ClientSession)
	report.ClientUse.Before = beforeObservation
	report.ClientUse.RequestsMade = options.ClientSession.RequestsMade()
	if err != nil {
		return mapClientExecutionError(&report, err, "the live downloader could not prove current use before resume")
	}
	if !before.Matches(journal.state.Intent.CurrentClientUseID, journal.state.Intent.MetafileVariantID,
		journal.state.Intent.MaterializeOperationID, journal.state.Intent.MaterializePlanID, journal.state.Intent.FinalObjectIdentity,
		journal.state.Intent.ActivationOperationID, journal.state.Intent.ActivationPlanID, journal.state.Intent.ClientCompletionID) {
		return executionIntegrity(&report, "the live downloader authority does not match the durable retirement intent")
	}
	if err := verifyRemainingBoundSources(ctx, journal.state, sources, options.Meta, freshFinal); err != nil {
		return mapExecutionError(&report, err, "remaining source bytes could not be reverified")
	}
	return continueSourceRetirement(ctx, &report, journal, sources, options.Meta, freshFinal, options.ClientUse, options.ClientSession, before)
}

// Status reads only the explicit private journal or retained tombstone. It
// does not access source roots, reverify the final, read credentials, contact
// a downloader, or infer that a historically retired name remains absent.
func Status(ctx context.Context, options StatusOptions) (ExecutionReport, error) {
	report := newExecutionReport(options.Limits)
	report.Operation = ExecutionOperationReport{ID: options.OperationID.String(), Status: "inspection_incomplete", Phase: "unknown"}
	if _, err := ParseOperationID(options.OperationID.String()); err != nil || options.Limits.Validate() != nil || options.TargetRoot == "" {
		return executionBlocked(&report, "selector.invalid", "source retirement status selectors are invalid")
	}
	if err := ctx.Err(); err != nil {
		return executionInterrupted(&report, err, "source retirement status was interrupted")
	}
	target, _, err := fsbind.BindExisting(options.TargetRoot)
	if err != nil {
		return mapExecutionError(&report, err, "source retirement target root could not be bound")
	}
	defer target.Close()
	report.addEffect("read_private_source_retirement_journal")
	if retained, retentionErr := applyExecutionRetentionControl(ctx, target, options.OperationID, &report); retained {
		if retentionErr != nil {
			return mapExecutionError(&report, retentionErr, "source retirement retention state could not be inspected")
		}
		return report, nil
	}
	journal, err := openExecutionJournal(ctx, target, options.OperationID, options.Limits)
	if err != nil {
		return mapExecutionError(&report, err, "source retirement journal could not be opened")
	}
	defer journal.close()
	populateExecutionFromJournal(&report, journal)
	report.Warnings = append(report.Warnings, "status reports historical private-journal evidence only; it does not prove that a retired source name remains absent now")
	if journal.state.CompletePresent {
		report.Outcome, report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = ExecutionOutcomeAlreadyRetired, "historical_complete", "complete", false
	} else {
		report.Outcome, report.Operation.Status, report.Operation.Resumable = ExecutionOutcomePartial, "active", true
	}
	report.finalize()
	return report, nil
}

func continueSourceRetirement(ctx context.Context, report *ExecutionReport, journal *executionJournal, sources *boundSourceSet, meta *metafile.MetaInfo,
	final *materialize.VerifiedFinal, currentUse *clientactivate.CurrentUseAuthority, session downloader.LedgerSession,
	before *clientactivate.VerifiedCurrentUse) (ExecutionReport, error) {
	if journal.state.CompletePresent {
		if err := requireRetiredNamesAbsent(ctx, journal.state, sources); err != nil {
			return mapExecutionError(report, err, "a retired source name reappeared")
		}
		report.Outcome, report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = ExecutionOutcomeAlreadyRetired, "complete", "complete", false
		report.finalize()
		return *report, nil
	}
	report.addEffect("write_private_source_retirement_journal", "unlink_explicit_source_names")
	for sequence, file := range journal.state.Intent.Files {
		if err := ctx.Err(); err != nil {
			return executionInterrupted(report, err, "source retirement was interrupted")
		}
		bound := sources.files[sequence]
		if journal.state.DeletedPresent[sequence] {
			if _, err := bound.session.InspectRootRegular(ctx, file.Name); !errors.Is(err, fsbind.ErrNotFound) {
				if err == nil {
					err = fmt.Errorf("%w: a retired source name reappeared", ErrExecutionIntegrity)
				}
				return mapExecutionError(report, err, "a retired source name reappeared")
			}
			continue
		}
		observed, observeErr := bound.session.InspectRootRegular(ctx, file.Name)
		if errors.Is(observeErr, fsbind.ErrNotFound) {
			if !journal.state.AttemptPresent[sequence] {
				return executionIntegrity(report, "a source name disappeared before its durable deletion attempt")
			}
			report.addEffect("confirm_source_parent_durability")
			if err := bound.session.SyncRoot(ctx); err != nil {
				if errors.Is(err, fsbind.ErrDurabilityUnconfirmed) {
					report.Writes.DurabilityUnconfirmed++
				}
				return mapExecutionError(report, err, "a recovered source deletion could not confirm parent-directory durability")
			}
			if _, err := bound.session.InspectRootRegular(ctx, file.Name); !errors.Is(err, fsbind.ErrNotFound) {
				if err == nil {
					err = fmt.Errorf("%w: a source name reappeared during recovery", ErrExecutionIntegrity)
				}
				return mapExecutionError(report, err, "a recovered source deletion changed during durability confirmation")
			}
			_, markerReceipt, err := journal.publishDeleted(ctx, sequence, DeleteBasisRecovered)
			recordExecutionJournalWrite(report, markerReceipt)
			if err != nil {
				return mapExecutionError(report, err, "a recovered source deletion could not be journaled")
			}
			populateExecutionFromJournal(report, journal)
			continue
		}
		if observeErr != nil {
			return mapExecutionError(report, observeErr, "a source name could not be observed before deletion")
		}
		if observed.Kind != fsbind.ObjectKindRegular || observed.SizeBytes != file.SizeBytes || observed.Identity.String() != file.SourceObjectIdentity {
			return executionIntegrity(report, "a source name changed before deletion")
		}
		finalPath, finalSize, finalOK := final.ProcessFilePath(file.ManifestIndex)
		if !finalOK || finalSize != file.SizeBytes {
			return executionIntegrity(report, "the exact final mapping changed before source deletion")
		}
		if err := compareBoundSourceAndFinal(ctx, bound, finalPath); err != nil {
			return mapExecutionError(report, err, "source bytes changed before their durable deletion attempt")
		}
		if !journal.state.AttemptPresent[sequence] {
			_, markerReceipt, err := journal.publishAttempt(ctx, sequence)
			recordExecutionJournalWrite(report, markerReceipt)
			if err != nil {
				return mapExecutionError(report, err, "a source deletion attempt could not be journaled")
			}
			populateExecutionFromJournal(report, journal)
			// Journal publication may involve durable filesystem work. Re-read
			// the source and final afterward so the unlink is bracketed by a
			// fresh exact comparison on the right side of that delay.
			if err := compareBoundSourceAndFinal(ctx, bound, finalPath); err != nil {
				return mapExecutionError(report, err, "source bytes changed after their durable deletion attempt")
			}
		}
		if beforeSourceRemovalHook != nil {
			if err := beforeSourceRemovalHook(sequence); err != nil {
				return executionInterrupted(report, err, "source retirement was interrupted before source-name removal")
			}
		}
		if err := journal.target.Check(); err != nil {
			return mapExecutionError(report, err, "the materialized target-root identity changed before source-name removal")
		}
		removal, removeErr := bound.session.RemoveRootRegularExact(ctx, file.Name, observed.Identity, file.SizeBytes)
		recordSourceRemoval(report, removal, removeErr)
		if removeErr != nil || !removal.Removed || removal.Durability != fsbind.DurabilityConfirmed {
			if removeErr == nil {
				removeErr = fsbind.ErrRemovalAmbiguous
			}
			return mapExecutionError(report, removeErr, "a source-name removal did not reach a confirmed durable result")
		}
		_, markerReceipt, err := journal.publishDeleted(ctx, sequence, DeleteBasisConfirmed)
		recordExecutionJournalWrite(report, markerReceipt)
		if err != nil {
			return mapExecutionError(report, err, "a confirmed source deletion could not be journaled")
		}
		populateExecutionFromJournal(report, journal)
	}
	freshFinal, finalObservation, err := final.Reverify(ctx)
	if err != nil {
		return mapFinalExecutionError(report, err, "the materialized final could not be reverified after source deletion")
	}
	if freshFinal == nil || finalObservation.FinalObjectIdentity != journal.state.Intent.FinalObjectIdentity {
		return executionIntegrity(report, "the materialized final identity changed during source retirement")
	}
	report.Final = finalObservation
	after, afterObservation, err := clientactivate.VerifyCurrentUse(ctx, currentUse, session)
	report.ClientUse.After = afterObservation
	report.ClientUse.RequestsMade = session.RequestsMade()
	if err != nil {
		return mapClientExecutionError(report, err, "the live downloader could not be reobserved after source deletion")
	}
	if before == nil || !before.StableWith(after) || !after.Matches(journal.state.Intent.CurrentClientUseID, journal.state.Intent.MetafileVariantID,
		journal.state.Intent.MaterializeOperationID, journal.state.Intent.MaterializePlanID, journal.state.Intent.FinalObjectIdentity,
		journal.state.Intent.ActivationOperationID, journal.state.Intent.ActivationPlanID, journal.state.Intent.ClientCompletionID) {
		report.ClientUse.Status = "unstable"
		return executionPartial(report, nil, "client.current_use_unstable", "the live downloader identity, state, layout, or paths changed across deletion")
	}
	report.ClientUse.Status, report.ClientUse.Stable = "observed_stable", true
	report.ClientUse.Assurance = "same_session_before_after_typed_job_and_effective_path_claims_bracketed_with_deletion_and_exact_final_reverification_non_atomic"
	_, markerReceipt, err := journal.publishComplete(ctx, afterObservation.CompleteFileSnapshotID)
	recordExecutionJournalWrite(report, markerReceipt)
	if err != nil {
		return mapExecutionError(report, err, "source retirement completion could not be journaled")
	}
	populateExecutionFromJournal(report, journal)
	report.Outcome, report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = ExecutionOutcomeRetired, "complete", "complete", false
	report.finalize()
	return *report, nil
}

func mapReviewFailure(report *ExecutionReport, err error) (ExecutionReport, error) {
	if report.Eligibility == nil {
		return executionIntegrity(report, "the live source-retirement review result is unavailable")
	}
	switch report.Eligibility.Outcome {
	case OutcomeBlocked:
		report.Outcome = ExecutionOutcomeBlocked
	case OutcomeIntegrityFailed:
		report.Outcome = ExecutionOutcomeIntegrity
	default:
		report.Outcome = ExecutionOutcomeIncomplete
	}
	report.Blockers = append(report.Blockers, report.Eligibility.Blockers...)
	report.Issues = append(report.Issues, report.Eligibility.Issues...)
	report.finalize()
	if report.Outcome == ExecutionOutcomeIntegrity {
		return *report, fmt.Errorf("%w: live source-retirement review changed", ErrExecutionIntegrity)
	}
	return *report, err
}

func mapExecutionError(report *ExecutionReport, err error, message string) (ExecutionReport, error) {
	switch {
	case errors.Is(err, ErrExecutionPolicy), errors.Is(err, fsbind.ErrUnsupported), errors.Is(err, fsbind.ErrAlreadyExists):
		return executionBlocked(report, "execution.blocked", message)
	case errors.Is(err, ErrExecutionIntegrity):
		return executionIntegrity(report, message)
	case errors.Is(err, fsbind.ErrUnsafeObject), errors.Is(err, fsbind.ErrBindingChanged), errors.Is(err, fsbind.ErrCrossFilesystem):
		return executionIntegrity(report, message)
	case errors.Is(err, ErrOperationNotFound):
		report.Outcome = ExecutionOutcomeBlocked
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "not_found", "unknown", false
		report.addBlocker("operation.not_found", message)
		report.finalize()
		return *report, nil
	default:
		return executionInterrupted(report, err, message)
	}
}

func mapFinalExecutionError(report *ExecutionReport, err error, message string) (ExecutionReport, error) {
	if errors.Is(err, materialize.ErrIntegrity) {
		return executionIntegrity(report, message)
	}
	return executionInterrupted(report, err, message)
}

func mapClientExecutionError(report *ExecutionReport, err error, message string) (ExecutionReport, error) {
	if errors.Is(err, clientactivate.ErrIntegrity) || errors.Is(err, materialize.ErrIntegrity) {
		return executionIntegrity(report, message)
	}
	if errors.Is(err, clientactivate.ErrPolicy) {
		if report.DeletionPerformed {
			return executionPartial(report, nil, "client.current_use_blocked", message)
		}
		return executionBlocked(report, "client.current_use_blocked", message)
	}
	return executionInterrupted(report, err, message)
}

func executionBlocked(report *ExecutionReport, code, message string) (ExecutionReport, error) {
	report.Outcome = ExecutionOutcomeBlocked
	report.addBlocker(code, message)
	report.finalize()
	return *report, nil
}

func executionIntegrity(report *ExecutionReport, message string) (ExecutionReport, error) {
	report.Outcome = ExecutionOutcomeIntegrity
	report.Operation.Resumable = false
	report.addBlocker("integrity.failed", message)
	report.finalize()
	return *report, fmt.Errorf("%w: %s", ErrExecutionIntegrity, message)
}

func executionInterrupted(report *ExecutionReport, err error, message string) (ExecutionReport, error) {
	if report.DeletionPerformed || report.Operation.IntentID != "" {
		report.Outcome = ExecutionOutcomePartial
	} else {
		report.Outcome = ExecutionOutcomeIncomplete
	}
	report.Operation.Resumable = report.Operation.IntentID != "" && report.Operation.Phase != "complete"
	report.addIssue("operation.interrupted", message)
	report.finalize()
	return *report, err
}

func executionPartial(report *ExecutionReport, err error, code, message string) (ExecutionReport, error) {
	report.Outcome, report.Operation.Resumable = ExecutionOutcomePartial, true
	report.addBlocker(code, message)
	report.finalize()
	return *report, err
}
