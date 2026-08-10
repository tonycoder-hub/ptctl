package clientactivate

import (
	"sort"

	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

type Outcome string

const (
	OutcomeReady                 Outcome = "ready"
	OutcomeRecheckInProgress     Outcome = "recheck_in_progress"
	OutcomeRecheckRequestUnknown Outcome = "recheck_request_result_unknown"
	OutcomeCheckedStopped        Outcome = "client_recheck_observed_complete_stopped"
	OutcomeAlreadyCheckedStopped Outcome = "historical_recheck_complete_current_stopped"
	OutcomeStartRequestUnknown   Outcome = "start_request_result_unknown"
	OutcomeStartedClientClaim    Outcome = "client_started_claim_observed"
	OutcomeHistoricalChecked     Outcome = "historical_recheck_recorded_current_client_not_observed"
	OutcomeHistoricalStarted     Outcome = "historical_start_recorded_current_client_not_observed"
	OutcomeForgetting            Outcome = "forgetting"
	OutcomeBlocked               Outcome = "blocked"
	OutcomeIncomplete            Outcome = "incomplete"
	OutcomeIntegrityFailed       Outcome = "integrity_failed"
)

type Finding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type OperationReport struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	PhaseBefore string `json:"phase_before"`
	PhaseAfter  string `json:"phase_after"`
	Resumable   bool   `json:"resumable"`
}

type PlanReport struct {
	ID                     string                                  `json:"id"`
	ExpectedID             string                                  `json:"expected_id,omitempty"`
	Matches                bool                                    `json:"matches"`
	Action                 string                                  `json:"action"`
	ClientConfigID         string                                  `json:"client_config_id"`
	Control                downloader.ExistingJobControlDescriptor `json:"control"`
	PathMappingID          string                                  `json:"path_mapping_id"`
	ClientPathSemantics    string                                  `json:"client_path_semantics"`
	ExpectedSavePathRef    string                                  `json:"expected_save_path_ref"`
	ExpectedContentPathRef string                                  `json:"expected_content_path_ref"`
	ExpectedFileLayoutID   string                                  `json:"expected_file_layout_id"`
	JobID                  string                                  `json:"job_id"`
}

type FinalReport struct {
	Status      string                        `json:"status"`
	Observation materialize.FinalObservation  `json:"observation"`
	PostAction  *materialize.FinalObservation `json:"post_action_observation,omitempty"`
}

type AdoptionReport struct {
	Status      string                            `json:"status"`
	Observation clientadopt.CompletionObservation `json:"observation"`
}

type ClientReport struct {
	Status                 string                                `json:"status"`
	RequestsMade           int                                   `json:"requests_made"`
	LedgerReads            int                                   `json:"ledger_reads"`
	FileLedgerReads        int                                   `json:"file_ledger_reads"`
	IdentityStatus         string                                `json:"identity_status"`
	JobID                  string                                `json:"job_id,omitempty"`
	JobState               string                                `json:"job_state,omitempty"`
	JobProgress            float64                               `json:"job_progress"`
	FileLayoutID           string                                `json:"file_layout_id,omitempty"`
	CompleteFileSnapshotID string                                `json:"complete_file_snapshot_id,omitempty"`
	AllFilesSelected       bool                                  `json:"all_files_selected"`
	AllFilesComplete       bool                                  `json:"all_files_complete"`
	ActionAttempted        string                                `json:"action_attempted,omitempty"`
	ActionReceipt          downloader.ExistingJobMutationReceipt `json:"action_receipt"`
	Assurance              string                                `json:"assurance"`
}

type JournalReport struct {
	Status                      string `json:"status"`
	IntentDurable               bool   `json:"intent_durable"`
	RecheckAttempts             int    `json:"recheck_attempts"`
	RecheckStartedDurable       bool   `json:"recheck_started_durable"`
	RecheckCompletionDurable    bool   `json:"recheck_completion_durable"`
	StartAttempts               int    `json:"start_attempts"`
	ActivationCompletionDurable bool   `json:"activation_completion_durable"`
	PendingRecoveryMarker       bool   `json:"pending_recovery_marker"`
	RetentionState              string `json:"retention_state"`
	RetentionIntentPresent      bool   `json:"retention_intent_present"`
	RetentionCompletionPresent  bool   `json:"retention_completion_present"`
	RetentionIntentDurable      bool   `json:"retention_intent_durable"`
	RetentionCompletionDurable  bool   `json:"retention_completion_durable"`
}

type WriteReport struct {
	OperationDirectories  int   `json:"operation_directories_created"`
	ControlDirectories    int   `json:"control_directories_created"`
	TemporaryFiles        int   `json:"temporary_files_created"`
	TemporaryBytes        int64 `json:"temporary_bytes_written"`
	MarkerPublications    int   `json:"marker_publications"`
	TemporaryRemovals     int   `json:"temporary_removals"`
	AmbiguousPublications int   `json:"ambiguous_publications"`
}

type Report struct {
	Outcome         Outcome         `json:"outcome"`
	Effect          []string        `json:"effect"`
	WritesPerformed int             `json:"writes_performed"`
	WritesUncertain bool            `json:"writes_uncertain"`
	Operation       OperationReport `json:"operation"`
	Plan            PlanReport      `json:"plan"`
	Final           FinalReport     `json:"materialized_final"`
	Adoption        AdoptionReport  `json:"stopped_adoption"`
	Client          ClientReport    `json:"client"`
	Journal         JournalReport   `json:"journal"`
	Writes          WriteReport     `json:"writes"`
	Blockers        []Finding       `json:"blockers"`
	Issues          []Finding       `json:"issues"`
	Warnings        []string        `json:"warnings"`
}

func newReport(authority *PreparedAuthority, expectedID string) Report {
	report := Report{
		Outcome: OutcomeIncomplete, Effect: []string{"read_exact_materialized_final", "read_stopped_adoption", "read_downloader_control_descriptor", "read_downloader_ledger"},
		Operation: OperationReport{Status: "not_created", PhaseBefore: "planned", PhaseAfter: "planned"},
		Client: ClientReport{Status: "not_observed", IdentityStatus: "not_observed", Assurance: "not_observed",
			ActionReceipt: downloader.ExistingJobMutationReceipt{RequestsAttempted: -1, AutomaticRetries: -1, RedirectsFollowed: -1}},
		Journal: JournalReport{Status: "not_created", RetentionState: "not_requested"}, Blockers: []Finding{}, Issues: []Finding{},
		Warnings: []string{
			"qBittorrent state, progress, and per-file completion remain untrusted bracketed client claims",
			"a successful recheck observation does not reveal the raw private metafile variant",
			"start can announce to trackers and transfer data; source retirement remains a separate explicit workflow",
			"client and filesystem observations are same-invocation bracketed and non-atomic",
			"read-only status reports historical markers and does not refresh directory durability",
		},
	}
	if authority != nil {
		report.Final = FinalReport{Status: "process_authority_available_current_not_reverified", Observation: authority.final}
		report.Adoption = AdoptionReport{Status: "canonical_completion_authority_available", Observation: authority.adoption}
	}
	report.Plan.ExpectedID = expectedID
	return report
}

func (report *Report) applyPrepared(prepared *PreparedPlan) {
	if report == nil || prepared == nil {
		return
	}
	plan := prepared.plan
	report.Operation.ID = prepared.operationID.String()
	report.Plan = PlanReport{
		ID: prepared.planID, ExpectedID: report.Plan.ExpectedID, Matches: report.Plan.ExpectedID == "" || report.Plan.ExpectedID == prepared.planID,
		Action: plan.Action, ClientConfigID: plan.ClientConfigID, Control: plan.Control, PathMappingID: plan.PathMappingID,
		ClientPathSemantics: plan.ClientPathSemantics, ExpectedSavePathRef: plan.ExpectedSavePathRef,
		ExpectedContentPathRef: plan.ExpectedContentPathRef, ExpectedFileLayoutID: plan.ExpectedFileLayoutID, JobID: plan.JobID,
	}
}

func (report *Report) observeClient(value clientObservation) {
	if report == nil {
		return
	}
	report.Client.IdentityStatus = "exact_unique"
	report.Client.JobID, report.Client.JobState, report.Client.JobProgress = value.jobID, value.job.State, value.job.Progress
	report.Client.FileLayoutID, report.Client.CompleteFileSnapshotID = value.fileLayoutID, value.completeSnapshotID
	report.Client.AllFilesSelected, report.Client.AllFilesComplete = value.allSelected, value.allComplete
	report.Client.Status = "unique_exact_job_observed"
	report.Client.Assurance = "typed_identity_and_lexical_layout_client_claim"
}

func (report *Report) recordClientObservationUsage(value clientObservation) {
	if report == nil {
		return
	}
	if value.ledgerRequestMade {
		report.Client.LedgerReads++
	}
	if value.fileRequestMade {
		report.Client.FileLedgerReads++
	}
}

func (report *Report) addBlocker(code, message string) {
	report.Blockers = append(report.Blockers, Finding{Code: code, Message: message})
}
func (report *Report) addIssue(code, message string) {
	report.Issues = append(report.Issues, Finding{Code: code, Message: message})
}

func (report *Report) addEffect(value string) {
	for _, existing := range report.Effect {
		if existing == value {
			return
		}
	}
	report.Effect = append(report.Effect, value)
}

func (report *Report) finalize() {
	if report.Blockers == nil {
		report.Blockers = []Finding{}
	}
	if report.Issues == nil {
		report.Issues = []Finding{}
	}
	if report.Effect == nil {
		report.Effect = []string{}
	}
	if report.Warnings == nil {
		report.Warnings = []string{}
	}
	sort.Slice(report.Blockers, func(i, j int) bool {
		if report.Blockers[i].Code != report.Blockers[j].Code {
			return report.Blockers[i].Code < report.Blockers[j].Code
		}
		return report.Blockers[i].Message < report.Blockers[j].Message
	})
	sort.Slice(report.Issues, func(i, j int) bool {
		if report.Issues[i].Code != report.Issues[j].Code {
			return report.Issues[i].Code < report.Issues[j].Code
		}
		return report.Issues[i].Message < report.Issues[j].Message
	})
	sort.Strings(report.Warnings)
}

func (report *Report) recordMarker(receipt markerWriteReceipt) {
	if receipt.TemporaryCreated {
		report.WritesPerformed++
		report.Writes.TemporaryFiles++
		report.Writes.TemporaryBytes += receipt.BytesWritten
	}
	if receipt.Publication.Attempted && receipt.Publication.Published {
		report.WritesPerformed++
		report.Writes.MarkerPublications++
	}
	if receipt.TemporaryRemoved {
		report.WritesPerformed++
		report.Writes.TemporaryRemovals++
	}
	if receipt.TemporaryCreationUncertain || receipt.TemporaryRemovalUncertain {
		report.WritesUncertain = true
	}
	if receipt.PublicationUncertain || receipt.Publication.Published && receipt.Publication.Durability != "confirmed" {
		report.WritesUncertain = true
		report.Writes.AmbiguousPublications++
	}
}

func (report *Report) recordCreation(receipt journalCreationReceipt) {
	if receipt.Subtree.Created {
		report.WritesPerformed++
		report.Writes.OperationDirectories++
		if receipt.Subtree.Durability != "confirmed" {
			report.WritesUncertain = true
		}
	}
	if receipt.Mkdir.DirectoriesCreated > 0 {
		report.WritesPerformed += receipt.Mkdir.DirectoriesCreated
		report.Writes.ControlDirectories += receipt.Mkdir.DirectoriesCreated
		if receipt.Mkdir.Durability != "confirmed" {
			report.WritesUncertain = true
		}
	}
	report.recordMarker(receipt.Intent)
}

func (report *Report) recordRecovery(receipt journalRecoveryReceipt) {
	if receipt.Mkdir.DirectoriesCreated > 0 {
		report.WritesPerformed += receipt.Mkdir.DirectoriesCreated
		report.Writes.ControlDirectories += receipt.Mkdir.DirectoriesCreated
		if receipt.Mkdir.Durability != "confirmed" {
			report.WritesUncertain = true
		}
	}
	report.recordMarker(receipt.Marker)
}
