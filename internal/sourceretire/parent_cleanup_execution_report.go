package sourceretire

import (
	"errors"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

func publicParentCleanupReportCopy(source ParentCleanupReport) ParentCleanupReport {
	result := source
	result.Effect = append([]string{}, source.Effect...)
	result.Plan.Directories = append([]ParentCleanupDirectory{}, source.Plan.Directories...)
	result.Plan.EvidenceBasis = append([]string{}, source.Plan.EvidenceBasis...)
	result.Blockers = append([]Finding{}, source.Blockers...)
	result.Issues = append([]Finding{}, source.Issues...)
	result.Warnings = append([]string{}, source.Warnings...)
	result.authority = nil
	return result
}

func populateParentCleanupExecutionFromJournal(report *ParentCleanupExecutionReport, journal *parentCleanupJournal, showPaths bool) {
	state := journal.state
	report.Operation.ID = state.Intent.OperationID.String()
	report.Operation.PlanID = state.Intent.CleanupPlanID
	report.Operation.IntentID = state.IntentID
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "active", "intent_recorded", true
	report.RetirementOperationID = state.Intent.RetirementOperationID.String()
	report.RetirementPlanID = state.Intent.RetirementPlanID
	report.RetirementCompletionID = state.Intent.RetirementCompletionID
	report.SearchScopeID = state.Intent.SearchScopeID
	report.Directories = make([]ParentCleanupExecutionDirectoryReport, len(state.Intent.Directories))
	report.Used.ParentsConsidered = len(state.Intent.Directories)
	report.Used.ParentPathBytes = 0
	removed := 0
	for sequence, directory := range state.Intent.Directories {
		status := "pending"
		if state.AttemptPresent[sequence] {
			status = "attempt_recorded"
		}
		if state.RemovedPresent[sequence] {
			status = "removed"
			removed++
		}
		item := ParentCleanupExecutionDirectoryReport{
			Sequence: sequence, ParentPathRef: directory.ParentPathRef, ParentIdentity: directory.ParentIdentity,
			RetiredFiles: directory.RetiredFiles, Status: status, AttemptID: state.AttemptIDs[sequence], RemovalID: state.RemovedIDs[sequence],
		}
		if showPaths {
			item.ParentPath = directory.ParentPath
		}
		if state.RemovedPresent[sequence] {
			item.RemovalBasis = state.Removed[sequence].Basis
		}
		report.Directories[sequence] = item
		report.Used.ParentPathBytes += int64(len(directory.ParentPath))
	}
	if removed > 0 {
		report.Operation.Phase = "partially_removed"
	}
	if removed == len(state.Intent.Directories) {
		report.Operation.Phase = "all_parents_removed"
	}
	if state.CompletePresent {
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "complete", "complete", false
		report.Operation.CompletionID = state.CompleteID
	}
}

func recordParentCleanupJournalInitialization(report *ParentCleanupExecutionReport, creation fsbind.Creation, directories fsbind.MkdirReceipt, marker parentCleanupJournalWrite) {
	if creation.Created {
		report.WritesPerformed++
		if creation.Durability != fsbind.DurabilityConfirmed {
			report.Writes.DurabilityUnconfirmed++
		}
	}
	report.WritesPerformed += directories.DirectoriesCreated
	if directories.DirectoriesCreated > 0 && directories.Durability != fsbind.DurabilityConfirmed {
		report.Writes.DurabilityUnconfirmed += directories.DirectoriesCreated
	}
	recordParentCleanupJournalWrite(report, marker)
}

func recordParentCleanupJournalWrite(report *ParentCleanupExecutionReport, receipt parentCleanupJournalWrite) {
	if receipt.ScratchCreated {
		report.WritesPerformed++
		report.Writes.ScratchFilesCreated++
	}
	if receipt.ScratchRemoved {
		report.WritesPerformed++
	}
	if receipt.Published {
		report.WritesPerformed++
		report.Writes.JournalObjectsPublished++
	}
	if receipt.Existing {
		report.Writes.JournalObjectsExisting++
	}
	report.Writes.ScratchBytesWritten += receipt.ScratchBytesWritten
	report.Used.JournalBytes += receipt.ScratchBytesWritten
	if receipt.Durability == fsbind.DurabilityUnconfirmed {
		report.Writes.DurabilityUnconfirmed++
	}
	if receipt.Ambiguous {
		report.Writes.AmbiguousPublications++
		report.WritesUncertain = true
	}
}

func recordParentDirectoryRemoval(report *ParentCleanupExecutionReport, receipt fsbind.Removal, err error) {
	if receipt.NamespaceRead {
		report.Used.DirectoryReads++
	}
	report.Used.EntriesObserved += receipt.EntriesExamined
	report.Used.EntryNameBytes += receipt.NameBytesExamined
	if receipt.Attempted {
		report.Writes.RemovalAttempts++
	}
	if receipt.Removed {
		report.WritesPerformed++
		report.DeletionPerformed = true
		report.Writes.DirectoriesRemoved++
	}
	if receipt.Durability == fsbind.DurabilityUnconfirmed {
		report.Writes.DurabilityUnconfirmed++
	}
	if errors.Is(err, fsbind.ErrRemovalAmbiguous) {
		report.Writes.AmbiguousRemovals++
		report.WritesUncertain = true
	}
}
