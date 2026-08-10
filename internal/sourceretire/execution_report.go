package sourceretire

import (
	"errors"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

func populateExecutionFromJournal(report *ExecutionReport, journal *executionJournal) {
	state := journal.state
	report.Operation.ID, report.Operation.PlanID, report.Operation.IntentID = state.Intent.OperationID.String(), state.Intent.PlanID, state.IntentID
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "active", "intent_recorded", true
	report.Files = sortedExecutionFileReports(state)
	report.Used.FilesConsidered, report.Used.ContentBytes = len(state.Intent.Files), state.Intent.ContentBytes
	report.Used.PathBytes = 0
	for _, file := range state.Intent.Files {
		report.Used.PathBytes += int64(len(file.ParentPath) + 1 + len(file.Name))
	}
	completed := 0
	for _, done := range state.DeletedPresent {
		if done {
			completed++
		}
	}
	if completed > 0 {
		report.Operation.Phase = "partially_retired"
	}
	if completed == len(state.Intent.Files) {
		report.Operation.Phase = "all_names_retired"
	}
	if state.CompletePresent {
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "complete", "complete", false
		report.Operation.CompletionID = state.CompleteID
	}
}

func recordJournalInitialization(report *ExecutionReport, creation fsbind.Creation, directories fsbind.MkdirReceipt, marker executionJournalWrite) {
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
	recordExecutionJournalWrite(report, marker)
}

func recordExecutionJournalWrite(report *ExecutionReport, receipt executionJournalWrite) {
	if receipt.ScratchCreated {
		report.WritesPerformed++
	}
	if receipt.ScratchRemoved {
		report.WritesPerformed++
	}
	if receipt.Published {
		report.WritesPerformed++
		report.Writes.JournalObjectsPublished++
	}
	report.Writes.JournalBytesWritten += receipt.ScratchBytesWritten
	report.Used.JournalBytes += receipt.ScratchBytesWritten
	if receipt.Durability == fsbind.DurabilityUnconfirmed {
		report.Writes.DurabilityUnconfirmed++
	}
	if receipt.Ambiguous {
		report.WritesUncertain = true
	}
}

func recordSourceRemoval(report *ExecutionReport, receipt fsbind.Removal, err error) {
	if receipt.Attempted {
		report.Writes.DeletionAttempts++
	}
	if receipt.Removed {
		report.WritesPerformed++
		report.DeletionPerformed = true
		report.Writes.NamesRemoved++
		report.Writes.BytesRemoved += receipt.SizeBytes
	}
	if receipt.Durability == fsbind.DurabilityUnconfirmed {
		report.Writes.DurabilityUnconfirmed++
	}
	if errors.Is(err, fsbind.ErrRemovalAmbiguous) {
		report.Writes.AmbiguousRemovals++
		report.WritesUncertain = true
	}
}
