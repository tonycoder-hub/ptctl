package materialize

import (
	"context"
	"errors"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

type Outcome string

const (
	OutcomeMaterializedVerified           Outcome = "materialized_verified"
	OutcomeActive                         Outcome = "active"
	OutcomeAlreadyCommitted               Outcome = "already_committed"
	OutcomeInterrupted                    Outcome = "interrupted"
	OutcomeBlocked                        Outcome = "blocked"
	OutcomeIntegrityFailed                Outcome = "integrity_failed"
	OutcomeAbandoned                      Outcome = "abandoned"
	OutcomePublishedIntegrityFailed       Outcome = "published_integrity_failed"
	OutcomePublishedDurabilityUnconfirmed Outcome = "published_durability_unconfirmed"
	OutcomePublicationAmbiguous           Outcome = "publication_ambiguous"
	OutcomePruning                        Outcome = "pruning"
	OutcomeRetained                       Outcome = "retained"
	OutcomeForgetting                     Outcome = "forgetting"
)

type Finding struct {
	Code          string `json:"code"`
	ManifestIndex *int   `json:"manifest_index,omitempty"`
	Message       string `json:"message"`
}

type OperationReport struct {
	ID          string `json:"id,omitempty"`
	Status      string `json:"status"`
	PhaseBefore string `json:"phase_before"`
	PhaseAfter  string `json:"phase_after"`
	Resumable   bool   `json:"resumable"`
}

type PlanReport struct {
	ExpectedID        string `json:"expected_id"`
	ObservedID        string `json:"observed_id,omitempty"`
	Matches           bool   `json:"matches"`
	MetafileVariantID string `json:"metafile_variant_id"`
	Strategy          string `json:"strategy"`
}

type SourceReport struct {
	Mode                   string `json:"mode"`
	Outcome                string `json:"outcome"`
	PreconditionsRechecked bool   `json:"preconditions_rechecked"`
	ContentVerified        bool   `json:"content_verified"`
}

type TargetReport struct {
	ExpectedRootIdentity string `json:"expected_root_identity,omitempty"`
	ObservedRootIdentity string `json:"observed_root_identity,omitempty"`
	Publication          string `json:"publication"`
	RootIdentityBound    bool   `json:"root_identity_bound"`
	SameFilesystemStage  bool   `json:"same_filesystem_staging"`
	NoClobberCapable     bool   `json:"no_clobber_capable"`
	NoClobber            bool   `json:"no_clobber"`
	StabilityAssurance   string `json:"stability_assurance"`
	StageContentVerified bool   `json:"stage_content_verified"`
	FinalContentVerified bool   `json:"final_content_verified"`
	DurabilityConfirmed  bool   `json:"durability_confirmed"`
}

type WriteReport struct {
	OperationSubtrees          int   `json:"operation_subtrees"`
	JournalDirectories         int   `json:"journal_directories"`
	IntentObjects              int   `json:"intent_objects"`
	JournalEvents              int   `json:"journal_events"`
	JournalPublicationAttempts int   `json:"journal_publication_attempts"`
	JournalDurabilityConfirms  int   `json:"journal_durability_confirmations"`
	ScratchFilesCreated        int   `json:"scratch_files_created"`
	ScratchBytesWritten        int64 `json:"scratch_bytes_written"`
	StagedFiles                int   `json:"staged_files"`
	StagePublicationAttempts   int   `json:"stage_publication_attempts"`
	StagedDirectories          int   `json:"staged_directories"`
	FinalLayoutPublications    int   `json:"final_layout_publications"`
	FinalPublicationAttempts   int   `json:"final_publication_attempts"`
	AmbiguousPublications      int   `json:"ambiguous_publications"`
	BytesWritten               int64 `json:"bytes_written"`
}

type Usage struct {
	SourceCopyBytes  int64 `json:"source_copy_bytes"`
	TargetWriteBytes int64 `json:"target_write_bytes"`
	StageVerifyBytes int64 `json:"stage_verify_bytes"`
	FinalVerifyBytes int64 `json:"final_verify_bytes"`
	ScratchEntries   int   `json:"scratch_entries_observed"`
	ScratchBytes     int64 `json:"scratch_bytes_observed"`
	FindingsRetained int   `json:"findings_retained"`
	FindingOverflow  int   `json:"finding_overflow"`
}

type Report struct {
	Outcome         Outcome         `json:"outcome"`
	Effect          []string        `json:"effect"`
	WritesPerformed int             `json:"writes_performed"`
	WritesUncertain bool            `json:"writes_uncertain"`
	Operation       OperationReport `json:"operation"`
	Plan            PlanReport      `json:"plan"`
	Source          SourceReport    `json:"source"`
	Target          TargetReport    `json:"target"`
	Writes          WriteReport     `json:"writes"`
	Limits          Limits          `json:"limits"`
	Used            Usage           `json:"used"`
	Blockers        []Finding       `json:"blockers"`
	Issues          []Finding       `json:"issues"`
	Warnings        []string        `json:"warnings"`
}

func newReport(metaVariantID, expectedPlanID string, limits Limits) Report {
	return Report{
		Outcome: OutcomeInterrupted,
		Effect:  []string{"write_private_materialize_journal", "write_target_layout"},
		Operation: OperationReport{
			Status: "not_created", PhaseBefore: "planned", PhaseAfter: "planned",
		},
		Plan: PlanReport{
			ExpectedID: expectedPlanID, MetafileVariantID: metaVariantID, Strategy: StrategyCopy,
		},
		Source: SourceReport{Mode: "live_discovery", Outcome: "unavailable"},
		Target: TargetReport{Publication: "not_attempted", StabilityAssurance: "not_observed"},
		Limits: limits, Blockers: []Finding{}, Issues: []Finding{},
		Warnings: []string{
			"materialized bytes are not a downloader job; client reachability and seeding state remain unknown",
			"filesystem verification is a same-invocation bracketed observation, not an atomic content snapshot",
		},
	}
}

func (report *Report) addBlocker(code, message string) {
	if report == nil {
		return
	}
	report.addFinding(&report.Blockers, code, message, nil)
}

func (report *Report) addIssue(code, message string, manifestIndex *int) {
	if report == nil {
		return
	}
	report.addFinding(&report.Issues, code, message, manifestIndex)
}

func markScratchCapacityBlocked(report *Report) {
	if report == nil || report.Operation.Status == "terminal" {
		return
	}
	report.Operation.Status = "scratch_capacity_blocked"
	report.Operation.Resumable = false
	for _, blocker := range report.Blockers {
		if blocker.Code == "scratch.capacity_exhausted" {
			return
		}
	}
	report.addBlocker("scratch.capacity_exhausted", "retained private scratch leaves no capacity for another journal transition; no automatic deletion is performed")
}

func (report *Report) addFinding(destination *[]Finding, code, message string, manifestIndex *int) {
	if len(*destination) < report.Limits.MaxFindings {
		finding := Finding{Code: code, Message: message}
		if manifestIndex != nil {
			value := *manifestIndex
			finding.ManifestIndex = &value
		}
		*destination = append(*destination, finding)
		report.Used.FindingsRetained++
	} else {
		report.Used.FindingOverflow++
	}
}

func (report *Report) recordJournalWrite(receipt journalWriteReceipt, event bool) {
	if report == nil {
		return
	}
	if receipt.ScratchCreated {
		report.WritesPerformed++
		report.Writes.ScratchFilesCreated++
		report.Writes.ScratchBytesWritten += receipt.ScratchBytesWritten
	}
	if receipt.Attempted {
		report.Writes.JournalPublicationAttempts++
	}
	if receipt.Ambiguous {
		report.WritesUncertain = true
		report.Writes.AmbiguousPublications++
	}
	if !receipt.Published {
		return
	}
	report.WritesPerformed++
	if event {
		report.Writes.JournalEvents++
	} else {
		report.Writes.IntentObjects++
	}
}

func classifyJournalOpenReport(report *Report, err error) {
	if report == nil || err == nil {
		return
	}
	switch {
	case errors.Is(err, ErrOperationNotFound):
		report.Outcome = OutcomeInterrupted
		report.Operation.Status = "not_found"
		report.addIssue("operation.not_found", "the explicit operation does not exist in the bound target root", nil)
	case errors.Is(err, ErrPolicy), errors.Is(err, fsbind.ErrUnsupported):
		report.Outcome = OutcomeBlocked
		report.addBlocker("journal.policy_blocked", "the explicit operation cannot be inspected within the configured safety budget")
	case errors.Is(err, ErrCorruptJournal), errors.Is(err, ErrIntegrity):
		report.Outcome = OutcomeIntegrityFailed
		report.Operation.Resumable = false
		report.addIssue("journal.open_failed", "the explicit operation journal is corrupt or bound to another filesystem object", nil)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		report.Outcome = OutcomeInterrupted
		report.addIssue("journal.read_interrupted", "reading the explicit operation journal was canceled", nil)
	default:
		report.Outcome = OutcomeInterrupted
		report.addIssue("journal.read_interrupted", "the explicit operation journal could not be fully read", nil)
	}
}

func setOperationInspectionSelector(report *Report, id OperationID) {
	if report == nil {
		return
	}
	report.Operation = OperationReport{
		ID: id.String(), Status: "inspection_incomplete",
		PhaseBefore: "unknown", PhaseAfter: "unknown",
	}
}
