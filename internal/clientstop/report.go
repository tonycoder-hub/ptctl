package clientstop

import (
	"context"
	"errors"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

const (
	OutcomeReady               = "ready"
	OutcomeStopped             = "stopped"
	OutcomeStoppedUnattributed = "stopped_causality_unproven"
	OutcomeRequestUnknown      = "request_unknown"
	OutcomeBlocked             = "blocked"
	OutcomeIncomplete          = "incomplete"
	OutcomeIntegrityFailed     = "integrity_failed"
	OutcomeAlreadyComplete     = "already_complete"
	OutcomeHistoricalComplete  = "historical_stop_complete"
)

type PlanReport struct {
	ID         string `json:"id"`
	ExpectedID string `json:"expected_id,omitempty"`
	Matches    bool   `json:"matches"`
	Data       Plan   `json:"data"`
}

type OperationReport struct {
	ID          string `json:"id,omitempty"`
	Status      string `json:"status"`
	Phase       string `json:"phase,omitempty"`
	PhaseBefore string `json:"phase_before,omitempty"`
	PhaseAfter  string `json:"phase_after,omitempty"`
	Resumable   bool   `json:"resumable"`
}

type WriteReport struct {
	WritesPerformed           int   `json:"writes_performed"`
	WritesUncertain           bool  `json:"writes_uncertain"`
	PrivateDirectoriesCreated int   `json:"private_directories_created"`
	PrivateFilesCreated       int   `json:"private_files_created"`
	PrivateFilesRemoved       int   `json:"private_files_removed"`
	MarkerPublications        int   `json:"marker_publications"`
	MarkerBytesWritten        int64 `json:"marker_bytes_written"`
	DownloaderRequests        int   `json:"downloader_mutation_requests"`
}

type ClientSessionReport struct {
	RequestsMade int `json:"requests_made"`
}

type MutationReport struct {
	Status  string                                `json:"status"`
	Receipt downloader.ExistingJobMutationReceipt `json:"receipt"`
}

type StoppedReport struct {
	Status      string                               `json:"status"`
	Observation clientactivate.CurrentUseObservation `json:"observation"`
}

type AssuranceReport struct {
	RequestRetryPolicy string `json:"request_retry_policy"`
	QueueEvidence      string `json:"queue_evidence"`
	FilesystemEvidence string `json:"filesystem_evidence"`
	CompletionBasis    string `json:"completion_basis"`
	Atomicity          string `json:"atomicity"`
}

type Report struct {
	Outcome   string                               `json:"outcome"`
	Effect    string                               `json:"effect"`
	Plan      PlanReport                           `json:"plan"`
	Operation OperationReport                      `json:"operation"`
	Writes    WriteReport                          `json:"writes"`
	Client    ClientSessionReport                  `json:"client_session"`
	Before    clientactivate.CurrentUseObservation `json:"before"`
	Mutation  MutationReport                       `json:"mutation"`
	Stopped   StoppedReport                        `json:"stopped"`
	Final     materialize.FinalObservation         `json:"materialized_final"`
	Assurance AssuranceReport                      `json:"assurance"`
	Blockers  []string                             `json:"blockers"`
	Warnings  []string                             `json:"warnings"`
}

func newReport(prepared *PreparedPlan) Report {
	report := Report{
		Outcome: OutcomeBlocked, Effect: "none", Operation: OperationReport{Status: "not_created", Phase: "planned"},
		Mutation: MutationReport{Status: "not_attempted"}, Stopped: StoppedReport{Status: "not_observed"},
		Assurance: AssuranceReport{RequestRetryPolicy: "single_effectful_request_no_automatic_retry",
			QueueEvidence: "not_observed", FilesystemEvidence: "not_reverified_after_stop", CompletionBasis: "not_completed",
			Atomicity: "downloader_and_filesystem_observations_are_bracketed_non_atomic"},
		Blockers: []string{}, Warnings: []string{},
	}
	if prepared != nil {
		report.Plan = PlanReport{ID: prepared.planID, Data: prepared.plan}
		if prepared.current != nil && prepared.current.Verified() {
			report.Before = prepared.current.Observation()
		}
	}
	return report
}

func FailureReport(expectedPlanID string, operationID OperationID, phase string, err error) Report {
	report := newReport(nil)
	if canonicalPlanID(expectedPlanID) {
		report.Plan.ExpectedID = expectedPlanID
	}
	if operationID != "" {
		report.Operation.ID = operationID.String()
	}
	if phase != "" {
		report.Operation.Phase = phase
	}
	report.classify(err)
	return report
}

func (report *Report) RecordSessionRequests(requests int) {
	if report == nil {
		return
	}
	if requests < 0 {
		report.Writes.WritesUncertain = true
		return
	}
	report.Client.RequestsMade = requests
}

func WithFailure(report Report, err error) Report { report.classify(err); return report }

func (report *Report) applyCreation(receipt journalCreationReceipt) {
	if receipt.Subtree.Created {
		report.Writes.PrivateDirectoriesCreated++
		report.Writes.WritesPerformed++
		if receipt.Subtree.Durability != fsbind.DurabilityConfirmed {
			report.Writes.WritesUncertain = true
		}
	}
	if receipt.Mkdir.DirectoriesCreated > 0 {
		report.Writes.PrivateDirectoriesCreated += receipt.Mkdir.DirectoriesCreated
		report.Writes.WritesPerformed += receipt.Mkdir.DirectoriesCreated
		if receipt.Mkdir.Durability != fsbind.DurabilityConfirmed {
			report.Writes.WritesUncertain = true
		}
	}
	report.applyMarker(receipt.Intent)
}

func (report *Report) applyRecovery(receipt journalRecoveryReceipt) {
	if receipt.Mkdir.DirectoriesCreated > 0 {
		report.Writes.PrivateDirectoriesCreated += receipt.Mkdir.DirectoriesCreated
		report.Writes.WritesPerformed += receipt.Mkdir.DirectoriesCreated
		if receipt.Mkdir.Durability != fsbind.DurabilityConfirmed {
			report.Writes.WritesUncertain = true
		}
	}
	report.applyMarker(receipt.Marker)
}

func (report *Report) applyMarker(receipt markerWriteReceipt) {
	if receipt.TemporaryCreated {
		report.Writes.PrivateFilesCreated++
		report.Writes.WritesPerformed++
	}
	report.Writes.MarkerBytesWritten += receipt.BytesWritten
	if receipt.Publication.Published {
		report.Writes.MarkerPublications++
		report.Writes.WritesPerformed++
	}
	if receipt.TemporaryRemoved {
		report.Writes.PrivateFilesRemoved++
		report.Writes.WritesPerformed++
	}
	if receipt.TemporaryCreationUncertain || receipt.PublicationUncertain || receipt.TemporaryRemovalUncertain ||
		receipt.Publication.Attempted && (!receipt.Publication.Published || receipt.Publication.Durability != fsbind.DurabilityConfirmed) {
		report.Writes.WritesUncertain = true
	}
}

func (report *Report) classify(err error) {
	if err == nil {
		return
	}
	switch {
	case errors.Is(err, ErrIntegrity), errors.Is(err, clientactivate.ErrIntegrity), errors.Is(err, materialize.ErrIntegrity):
		report.Outcome = OutcomeIntegrityFailed
		report.Blockers = append(report.Blockers, "client_stop.integrity_failed")
		report.Operation.Resumable = false
	case errors.Is(err, ErrRequestUnknown):
		report.Outcome = OutcomeRequestUnknown
		report.Blockers = append(report.Blockers, "client_stop.request_unknown")
	case errors.Is(err, ErrOperationNotFound):
		report.Outcome = OutcomeBlocked
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "not_found", "absent", false
		report.Blockers = append(report.Blockers, "client_stop.operation_not_found")
	case errors.Is(err, ErrInitializationIncomplete):
		report.Outcome = OutcomeIncomplete
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "initialization_incomplete", "intent_not_durable", true
		report.Blockers = append(report.Blockers, "client_stop.initialization_incomplete")
	case errors.Is(err, ErrMarkerRecoveryRequired):
		report.Outcome = OutcomeIncomplete
		report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "recovery_required", "marker_recovery_required", true
		report.Blockers = append(report.Blockers, "client_stop.marker_recovery_required")
	case errors.Is(err, ErrPolicy), errors.Is(err, clientactivate.ErrPolicy), errors.Is(err, fsbind.ErrAlreadyExists), errors.Is(err, fsbind.ErrBusy):
		report.Outcome = OutcomeBlocked
		report.Blockers = append(report.Blockers, "client_stop.policy_blocked")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		report.Outcome = OutcomeIncomplete
		report.Blockers = append(report.Blockers, "context_cancelled")
	default:
		report.Outcome = OutcomeIncomplete
		report.Blockers = append(report.Blockers, "client_stop.operation_incomplete")
	}
}

func publicReceipt(value downloader.ExistingJobMutationReceipt) downloader.ExistingJobMutationReceipt {
	return downloader.ExistingJobMutationReceipt{Effect: value.Effect, ObservedAtStart: value.ObservedAtStart, ObservedAtEnd: value.ObservedAtEnd,
		Complete: value.Complete, RequestsAttempted: value.RequestsAttempted, AutomaticRetries: value.AutomaticRetries,
		RedirectsFollowed: value.RedirectsFollowed, RequestBytes: value.RequestBytes, RequestBytesKnown: value.RequestBytesKnown,
		RequestID: value.RequestID, StopReason: value.StopReason}
}

func parseObservedTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}
func nonnegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}
