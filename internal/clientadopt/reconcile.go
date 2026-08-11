package clientadopt

import (
	"math"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
)

const adoptionCurrentJobAssurance = "same_invocation_existing_reconciliation_bracket_bound_to_canonical_stopped_adoption_and_exact_final_with_current_typed_job_claim_non_atomic_without_job_incarnation_proof"

// ReconciliationAdoptionCompletion exposes a bounded public view only while
// this process still holds authority from VerifyCompletion. The DTO is
// historical evidence and cannot recreate the authority after serialization.
func (verified *VerifiedCompletion) ReconciliationAdoptionCompletion() (reconcile.ClientAdoptionCompletion, bool) {
	if !verified.Verified() {
		return reconcile.ClientAdoptionCompletion{}, false
	}
	observation := verified.Observation()
	started, startErr := time.Parse(time.RFC3339Nano, observation.ObservedAtStart)
	ended, endErr := time.Parse(time.RFC3339Nano, observation.ObservedAtEnd)
	if startErr != nil || endErr != nil || started.IsZero() || ended.Before(started) {
		return reconcile.ClientAdoptionCompletion{}, false
	}
	return reconcile.ClientAdoptionCompletion{
		Driver: observation.Driver, Action: observation.Action, OperationID: observation.OperationID, PlanID: observation.PlanID,
		CompletionID: observation.CompletionID, MetafileVariantID: observation.MetafileVariantID,
		MetafileBytes: observation.MetafileBytes, InfoHashV1: observation.InfoHashV1, InfoHashV2: observation.InfoHashV2,
		MaterializeOperationID: observation.MaterializeOperationID, MaterializePlanID: observation.MaterializePlanID,
		ClientConfigID: observation.ClientConfigID, PathMappingID: observation.PathMappingID,
		ClientPathSemantics: observation.ClientPathSemantics, ExpectedSavePathRef: observation.ExpectedSavePathRef,
		ExpectedContentPathRef: observation.ExpectedContentPathRef, JobID: observation.JobID,
		TerminalJobState: observation.JobState, TargetRootIdentity: observation.TargetRootIdentity,
		FinalObjectIdentity: observation.FinalObjectIdentity, MultiFile: observation.MultiFile,
		ManifestFiles: observation.ManifestFiles, ContentBytes: observation.ContentBytes,
		ObservedAtStart: started, ObservedAtEnd: ended, RetainedTombstone: observation.RetainedTombstone,
		Assurance: observation.Assurance,
	}, true
}

// ReconcileAdoptedCurrentJob binds a historical stopped-job adoption completion to the
// exact typed job claim in the caller's already-observed reconciliation
// bracket. It performs no network or filesystem operation. Downloader APIs do
// not expose a stable incarnation/generation, so success never claims that a
// remove-and-readd cycle was excluded.
func (verified *VerifiedCompletion) ReconcileAdoptedCurrentJob(bracket reconcile.ClientBracket) (reconcile.ClientAdoptionCurrentJob, bool) {
	if !verified.Verified() || !bracket.Requested || bracket.Before == nil || bracket.After == nil ||
		bracket.StopReason != "" || bracket.FileStopReason != "" || bracket.RequestsMade < 0 || bracket.FileRequestsMade < 0 {
		return reconcile.ClientAdoptionCurrentJob{}, false
	}
	plan := verified.authority.plan
	completion := verified.authority.completion
	before, after := *bracket.Before, *bracket.After
	descriptor, ok := downloader.DescribeLedgerDriver(plan.Driver)
	if !ok || bracket.RequestsMade != descriptor.OpenRequests+2+bracket.FileRequestsMade ||
		before.Driver != plan.Driver || after.Driver != plan.Driver || !before.Complete || !after.Complete ||
		before.Capabilities != after.Capabilities || !before.Capabilities.TypedInfoHashes || !before.Capabilities.ContentPath ||
		before.ObservedAtStart.IsZero() || before.ObservedAtEnd.Before(before.ObservedAtStart) ||
		after.ObservedAtStart.Before(before.ObservedAtEnd) || after.ObservedAtEnd.Before(after.ObservedAtStart) ||
		before.ObservedAtStart.Before(completion.ObservedAtEnd) ||
		downloader.ValidateLedgerDriverClaims(before) != nil || downloader.ValidateLedgerDriverClaims(after) != nil {
		return reconcile.ClientAdoptionCurrentJob{}, false
	}
	if bracket.FileAttempted {
		if bracket.FileRequestsMade != 2 || bracket.FilesBefore == nil || bracket.FilesAfter == nil {
			return reconcile.ClientAdoptionCurrentJob{}, false
		}
	} else if bracket.FileRequestsMade != 0 || bracket.FilesBefore != nil || bracket.FilesAfter != nil {
		return reconcile.ClientAdoptionCurrentJob{}, false
	}
	identity := downloader.TypedIdentity{InfoHashV1: plan.InfoHashV1, InfoHashV2: plan.InfoHashV2}
	beforeMatch, beforeErr := downloader.AssessLedgerIdentity(before, identity)
	afterMatch, afterErr := downloader.AssessLedgerIdentity(after, identity)
	if beforeErr != nil || afterErr != nil || beforeMatch.Status != downloader.LedgerIdentityExactUnique ||
		afterMatch.Status != downloader.LedgerIdentityExactUnique || beforeMatch.ExactJob == nil || afterMatch.ExactJob == nil {
		return reconcile.ClientAdoptionCurrentJob{}, false
	}
	beforeJob, afterJob := *beforeMatch.ExactJob, *afterMatch.ExactJob
	if !stableAdoptionReconciliationJob(beforeJob, afterJob) || jobID(beforeJob.Hash) != completion.JobID ||
		beforeJob.SizeBytes != plan.ContentBytes || math.IsNaN(beforeJob.Progress) || math.IsInf(beforeJob.Progress, 0) ||
		beforeJob.Progress < 0 || beforeJob.Progress > 1 {
		return reconcile.ClientAdoptionCurrentJob{}, false
	}
	windows := plan.ClientPathSemantics == "windows_exact"
	if !windows && plan.ClientPathSemantics != "posix_exact" {
		return reconcile.ClientAdoptionCurrentJob{}, false
	}
	saveRef, saveErr := reconcile.ClientPathReference(beforeJob.SavePath, windows)
	contentRef, contentErr := reconcile.ClientPathReference(beforeJob.ContentPath, windows)
	state := safeAdoptionReconciliationState(beforeJob.State)
	if saveErr != nil || contentErr != nil || saveRef != plan.ExpectedSavePathRef || contentRef != plan.ExpectedContentPathRef ||
		state != beforeJob.State {
		return reconcile.ClientAdoptionCurrentJob{}, false
	}
	return reconcile.ClientAdoptionCurrentJob{
		Driver: plan.Driver, JobID: completion.JobID, JobState: state, JobProgress: beforeJob.Progress,
		SavePathRef: saveRef, ContentPathRef: contentRef, ObservedAtStart: before.ObservedAtStart,
		ObservedAtEnd: after.ObservedAtEnd, RequestsMade: bracket.RequestsMade,
		JobsExaminedBefore: beforeMatch.JobsExamined, JobsExaminedAfter: afterMatch.JobsExamined,
		FinalObjectIdentity: plan.FinalObjectIdentity, Assurance: adoptionCurrentJobAssurance,
	}, true
}

func stableAdoptionReconciliationJob(before, after downloader.Torrent) bool {
	return before.Hash == after.Hash && before.InfoHashV1 == after.InfoHashV1 && before.InfoHashV2 == after.InfoHashV2 &&
		before.IdentityStatus == after.IdentityStatus && before.SavePath == after.SavePath && before.ContentPath == after.ContentPath &&
		before.SizeBytes == after.SizeBytes && before.State == after.State && before.Progress == after.Progress && before.Downloaded == after.Downloaded
}

func safeAdoptionReconciliationState(value string) string {
	switch value {
	case "error", "missingFiles", "uploading", "pausedUP", "queuedUP", "stalledUP", "checkingUP", "forcedUP", "stoppedUP",
		"allocating", "downloading", "metaDL", "forcedMetaDL", "pausedDL", "queuedDL", "stalledDL", "checkingDL", "forcedDL", "stoppedDL",
		"checkingResumeData", "moving", "unknown":
		return value
	default:
		return "unknown"
	}
}
