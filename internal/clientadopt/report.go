package clientadopt

import (
	"sort"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

type Outcome string

const (
	OutcomeReady                 Outcome = "ready"
	OutcomeAdoptedPendingRecheck Outcome = "adopted_pending_client_recheck"
	OutcomeAlreadyAdopted        Outcome = "already_adopted_pending_client_recheck"
	OutcomeHistoricalAdopted     Outcome = "historical_adoption_recorded"
	OutcomeRequestUnknown        Outcome = "request_result_unknown"
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
	ID                     string `json:"id"`
	ExpectedID             string `json:"expected_id,omitempty"`
	Matches                bool   `json:"matches"`
	Action                 string `json:"action"`
	ClientConfigID         string `json:"client_config_id"`
	PathMappingID          string `json:"path_mapping_id"`
	ClientPathSemantics    string `json:"client_path_semantics"`
	ExpectedSavePathRef    string `json:"expected_save_path_ref"`
	ExpectedContentPathRef string `json:"expected_content_path_ref"`
}

type FinalReport struct {
	Status      string                        `json:"status"`
	Observation materialize.FinalObservation  `json:"observation"`
	PostAction  *materialize.FinalObservation `json:"post_action_observation,omitempty"`
}

type ClientReport struct {
	Status             string                     `json:"status"`
	RequestsMade       int                        `json:"requests_made"`
	BeforeIdentity     string                     `json:"before_identity"`
	AfterIdentity      string                     `json:"after_identity"`
	JobsExaminedBefore int                        `json:"jobs_examined_before"`
	JobsExaminedAfter  int                        `json:"jobs_examined_after"`
	AddAttempted       bool                       `json:"add_attempted"`
	AddReceipt         downloader.MutationReceipt `json:"add_receipt"`
	JobID              string                     `json:"job_id,omitempty"`
	JobState           string                     `json:"job_state,omitempty"`
	ContentPathRef     string                     `json:"content_path_ref,omitempty"`
	VariantRelation    string                     `json:"metafile_variant_relation"`
	Assurance          string                     `json:"assurance"`
}

type JournalReport struct {
	Status                string `json:"status"`
	IntentDurable         bool   `json:"intent_durable"`
	AttemptsRecorded      int    `json:"attempts_recorded"`
	CompletionDurable     bool   `json:"completion_durable"`
	PendingRecoveryMarker bool   `json:"pending_recovery_marker"`
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
	Client          ClientReport    `json:"client"`
	Journal         JournalReport   `json:"journal"`
	Writes          WriteReport     `json:"writes"`
	Blockers        []Finding       `json:"blockers"`
	Issues          []Finding       `json:"issues"`
	Warnings        []string        `json:"warnings"`
}

func newReport(prepared *PreparedPlan, expectedID string) Report {
	plan := Plan{}
	planID, operationID := "", ""
	observation := materialize.FinalObservation{}
	finalStatus := "not_observed"
	if prepared != nil {
		plan, planID, operationID = prepared.plan, prepared.planID, prepared.operation.String()
		if prepared.verified != nil {
			observation = prepared.verified.Observation()
			if prepared.verified.Verified() {
				finalStatus = "process_authority_available_current_not_reverified"
			}
		}
	}
	return Report{
		Outcome:   OutcomeIncomplete,
		Effect:    []string{"read_exact_materialized_final", "read_downloader_ledger"},
		Operation: OperationReport{ID: operationID, Status: "not_created", PhaseBefore: "planned", PhaseAfter: "planned"},
		Plan: PlanReport{
			ID: planID, ExpectedID: expectedID, Matches: expectedID == "" || expectedID == planID,
			Action: plan.Action, ClientConfigID: plan.ClientConfigID, PathMappingID: plan.PathMappingID,
			ClientPathSemantics: plan.ClientPathSemantics, ExpectedSavePathRef: plan.ExpectedSavePathRef,
			ExpectedContentPathRef: plan.ExpectedContentPathRef,
		},
		Final: FinalReport{Status: finalStatus, Observation: observation},
		Client: ClientReport{Status: "not_observed", BeforeIdentity: "not_observed", AfterIdentity: "not_observed",
			VariantRelation: "unobservable", Assurance: "not_observed"},
		Journal:  JournalReport{Status: "not_created"},
		Blockers: []Finding{}, Issues: []Finding{},
		Warnings: []string{
			"qBittorrent cannot prove that its stored private metafile bytes equal the submitted exact variant",
			"adoption stops before client recheck; source retirement remains a separate explicit workflow",
			"client and filesystem observations are bracketed and non-atomic",
		},
	}
}

func (report *Report) addBlocker(code, message string) {
	report.Blockers = append(report.Blockers, Finding{Code: code, Message: message})
}

func (report *Report) addIssue(code, message string) {
	report.Issues = append(report.Issues, Finding{Code: code, Message: message})
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
	if receipt.PublicationUncertain || receipt.Publication.Attempted && (!receipt.Publication.Published || receipt.Publication.Durability != "confirmed") {
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
