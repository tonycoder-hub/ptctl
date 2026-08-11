package clientactivate

import (
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
)

// ReconciliationCompletion exposes a bounded public view only while the
// process-local terminal completion authority remains valid. The returned DTO
// is historical evidence; it cannot recreate this method's authority.
func (verified *VerifiedCompletion) ReconciliationCompletion() (reconcile.ClientActivationCompletion, bool) {
	if !verified.Verified() {
		return reconcile.ClientActivationCompletion{}, false
	}
	observation := verified.Observation()
	started, startErr := time.Parse(time.RFC3339Nano, observation.ObservedAtStart)
	ended, endErr := time.Parse(time.RFC3339Nano, observation.ObservedAtEnd)
	if startErr != nil || endErr != nil || started.IsZero() || ended.Before(started) {
		return reconcile.ClientActivationCompletion{}, false
	}
	return reconcile.ClientActivationCompletion{
		Driver: observation.Driver, OperationID: observation.OperationID, PlanID: observation.PlanID,
		TerminalMarkerID: observation.TerminalMarkerID, Action: observation.Action, TerminalPhase: observation.TerminalPhase,
		TerminalJobState: observation.JobState, MetafileVariantID: observation.MetafileVariantID,
		MaterializeOperationID: observation.MaterializeOperationID, MaterializePlanID: observation.MaterializePlanID,
		ClientConfigID: observation.ClientConfigID, PathMappingID: observation.PathMappingID, JobID: observation.JobID,
		FinalObjectIdentity: observation.FinalObjectIdentity, ObservedAtStart: started, ObservedAtEnd: ended,
		Assurance: observation.Assurance,
	}, true
}

// ReconcileCurrentUse validates the downloader bracket already read by
// reconcile. It performs no request and cannot upgrade the independent client,
// path, or storage axes. Success only binds those current claims to this
// process-local exact-final and terminal-activation authority.
func (authority *CurrentUseAuthority) ReconcileCurrentUse(bracket reconcile.ClientBracket) (reconcile.ClientActivationCurrentUse, bool) {
	if authority == nil || authority.prepared == nil || authority.completion == nil || !authority.completion.Verified() ||
		authority.plan.Validate() != nil || bracket.Before == nil || bracket.After == nil || !bracket.Requested ||
		bracket.StopReason != "" || bracket.FileStopReason != "" || bracket.FileLimits != authority.plan.FileLimits ||
		bracket.FileLayoutMode != "auto" || bracket.RequestsMade < 0 || bracket.FileRequestsMade < 0 {
		return reconcile.ClientActivationCurrentUse{}, false
	}
	beforeLedger, afterLedger := *bracket.Before, *bracket.After
	descriptor, ok := downloader.DescribeLedgerDriver(authority.plan.Driver)
	if !ok || bracket.RequestsMade != descriptor.OpenRequests+2+bracket.FileRequestsMade ||
		beforeLedger.Driver != authority.plan.Driver || afterLedger.Driver != authority.plan.Driver ||
		!beforeLedger.Complete || !afterLedger.Complete || beforeLedger.Capabilities != afterLedger.Capabilities ||
		!beforeLedger.Capabilities.TypedInfoHashes || !beforeLedger.Capabilities.ContentPath ||
		beforeLedger.ObservedAtStart.IsZero() || beforeLedger.ObservedAtEnd.Before(beforeLedger.ObservedAtStart) ||
		afterLedger.ObservedAtStart.Before(beforeLedger.ObservedAtEnd) || afterLedger.ObservedAtEnd.Before(afterLedger.ObservedAtStart) {
		return reconcile.ClientActivationCurrentUse{}, false
	}
	before, ok := reconciliationLedgerObservation(authority, beforeLedger)
	if !ok {
		return reconcile.ClientActivationCurrentUse{}, false
	}
	after, ok := reconciliationLedgerObservation(authority, afterLedger)
	if !ok || !stableReconciliationJob(before.job, after.job) || before.jobID != after.jobID {
		return reconcile.ClientActivationCurrentUse{}, false
	}

	if authority.plan.MultiFile {
		if !beforeLedger.Capabilities.JobFiles || !bracket.FileAttempted || bracket.FileRequestsMade != 2 ||
			bracket.FilesBefore == nil || bracket.FilesAfter == nil {
			return reconcile.ClientActivationCurrentUse{}, false
		}
		beforeFiles, afterFiles := *bracket.FilesBefore, *bracket.FilesAfter
		if beforeFiles.Driver != authority.plan.Driver || afterFiles.Driver != authority.plan.Driver ||
			beforeFiles.JobKey != before.job.Hash || afterFiles.JobKey != after.job.Hash ||
			beforeFiles.ObservedAtStart.Before(beforeLedger.ObservedAtEnd) ||
			afterFiles.ObservedAtStart.Before(beforeFiles.ObservedAtEnd) ||
			afterLedger.ObservedAtStart.Before(afterFiles.ObservedAtEnd) ||
			!reconciliationFileEnvelope(beforeFiles, before.job) || !reconciliationFileEnvelope(afterFiles, after.job) {
			return reconcile.ClientActivationCurrentUse{}, false
		}
		beforeLayout, beforeSnapshot, beforeSelected, beforeComplete, err := validateFileLayout(authority.prepared, before.job, beforeFiles)
		if err != nil {
			return reconcile.ClientActivationCurrentUse{}, false
		}
		afterLayout, afterSnapshot, afterSelected, afterComplete, err := validateFileLayout(authority.prepared, after.job, afterFiles)
		if err != nil || beforeLayout != afterLayout || beforeSnapshot != afterSnapshot ||
			beforeLayout != authority.plan.ExpectedFileLayoutID || !beforeSelected || !afterSelected || !beforeComplete || !afterComplete {
			return reconcile.ClientActivationCurrentUse{}, false
		}
		before.fileLayoutID, before.completeSnapshotID = beforeLayout, beforeSnapshot
		after.fileLayoutID, after.completeSnapshotID = afterLayout, afterSnapshot
	} else {
		if bracket.FileAttempted || bracket.FileRequestsMade != 0 || bracket.FilesBefore != nil || bracket.FilesAfter != nil {
			return reconcile.ClientActivationCurrentUse{}, false
		}
		beforeLayout, beforeSnapshot, err := singleFileLayoutIdentity(authority.prepared, before.job)
		if err != nil {
			return reconcile.ClientActivationCurrentUse{}, false
		}
		afterLayout, afterSnapshot, err := singleFileLayoutIdentity(authority.prepared, after.job)
		if err != nil || beforeLayout != afterLayout || beforeSnapshot != afterSnapshot || beforeLayout != authority.plan.ExpectedFileLayoutID {
			return reconcile.ClientActivationCurrentUse{}, false
		}
		before.fileLayoutID, before.completeSnapshotID = beforeLayout, beforeSnapshot
		after.fileLayoutID, after.completeSnapshotID = afterLayout, afterSnapshot
	}
	if before.fileLayoutID != after.fileLayoutID || before.completeSnapshotID != after.completeSnapshotID ||
		before.jobID != authority.plan.JobID || before.job.Progress != 1 ||
		(!completeStoppedState(before.job.State) && !startedState(before.job.State)) {
		return reconcile.ClientActivationCurrentUse{}, false
	}
	useID, err := currentUseID(authority.plan)
	if err != nil || useID != authority.useID {
		return reconcile.ClientActivationCurrentUse{}, false
	}
	return reconcile.ClientActivationCurrentUse{
		Driver: authority.plan.Driver, UseID: useID, JobID: before.jobID, FileLayoutID: before.fileLayoutID,
		CompleteSnapshotID: before.completeSnapshotID,
		JobState:           before.job.State, JobProgress: before.job.Progress,
		ObservedAtStart: beforeLedger.ObservedAtStart, ObservedAtEnd: afterLedger.ObservedAtEnd,
		FinalObjectIdentity: authority.plan.FinalObjectIdentity,
		Assurance:           "same_invocation_existing_reconciliation_bracket_bound_to_canonical_terminal_activation_and_exact_final_non_atomic",
	}, true
}

func reconciliationLedgerObservation(authority *CurrentUseAuthority, ledger downloader.LedgerSnapshot) (clientObservation, bool) {
	var result clientObservation
	if authority == nil || authority.prepared == nil || ledger.Driver != authority.plan.Driver ||
		downloader.ValidateLedgerDriverClaims(ledger) != nil {
		return result, false
	}
	assessment, err := downloader.AssessLedgerIdentity(ledger, downloader.TypedIdentity{
		InfoHashV1: authority.plan.InfoHashV1, InfoHashV2: authority.plan.InfoHashV2,
	})
	if err != nil || assessment.Status != downloader.LedgerIdentityExactUnique || assessment.ExactJob == nil {
		return result, false
	}
	job := *assessment.ExactJob
	job.State = normalizedClientState(job.State)
	result.job, result.jobID = job, opaqueJobID(job.Hash)
	if validateJobEnvelope(authority.prepared, job, result.jobID) != nil || job.Progress != 1 ||
		(!completeStoppedState(job.State) && !startedState(job.State)) {
		return clientObservation{}, false
	}
	return result, true
}

func reconciliationFileEnvelope(snapshot downloader.JobFileLedgerSnapshot, job downloader.Torrent) bool {
	if !snapshot.Complete || snapshot.Limits.Validate() != nil || snapshot.ObservedAtStart.IsZero() ||
		snapshot.ObservedAtEnd.Before(snapshot.ObservedAtStart) || snapshot.Used.ResponseBytes <= 0 ||
		snapshot.Used.ResponseBytes > snapshot.Limits.MaxResponseBytes {
		return false
	}
	switch snapshot.Driver {
	case downloader.DriverQBittorrent:
		return snapshot.SavePath == "" && snapshot.ContentPath == ""
	case downloader.DriverTransmission:
		return snapshot.SavePath == job.SavePath && snapshot.ContentPath == job.ContentPath
	default:
		return false
	}
}

func stableReconciliationJob(before, after downloader.Torrent) bool {
	return before.Hash == after.Hash && before.InfoHashV1 == after.InfoHashV1 && before.InfoHashV2 == after.InfoHashV2 &&
		before.IdentityStatus == after.IdentityStatus && before.SavePath == after.SavePath && before.ContentPath == after.ContentPath &&
		before.SizeBytes == after.SizeBytes && before.State == after.State && before.Progress == after.Progress && before.Downloaded == after.Downloaded
}
