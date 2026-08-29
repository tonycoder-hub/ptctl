package clientadopt

import (
	"fmt"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

type RetentionOutcome string

const (
	RetentionOutcomePruned          RetentionOutcome = "pruned"
	RetentionOutcomeAlreadyPruned   RetentionOutcome = "already_pruned"
	RetentionOutcomeBlocked         RetentionOutcome = "blocked"
	RetentionOutcomeInterrupted     RetentionOutcome = "interrupted"
	RetentionOutcomeIntegrityFailed RetentionOutcome = "integrity_failed"
)

type RetentionLimits struct {
	MaxMarkerBytes int64 `json:"max_marker_bytes"`
	MaxObjects     int   `json:"max_objects"`
	MaxPathBytes   int64 `json:"max_path_bytes"`
}

func DefaultRetentionLimits() RetentionLimits {
	return RetentionLimits{MaxMarkerBytes: maximumMarkerBytes, MaxObjects: maximumAttempts + 8, MaxPathBytes: 1 << 20}
}

func (limits RetentionLimits) Validate() error {
	if limits.MaxMarkerBytes <= 0 || limits.MaxMarkerBytes > maximumMarkerBytes ||
		limits.MaxObjects < maximumAttempts+8 || limits.MaxObjects > 64 ||
		limits.MaxPathBytes <= 0 || limits.MaxPathBytes > 1<<20 {
		return fmt.Errorf("%w: client adoption retention limits are invalid", ErrPolicy)
	}
	return nil
}

type PruneOptions struct {
	TargetRoot     string          `json:"-"`
	OperationID    OperationID     `json:"operation_id"`
	ExpectedPlanID string          `json:"expected_plan_id"`
	Acknowledge    bool            `json:"acknowledge"`
	Limits         RetentionLimits `json:"limits"`
}

type RetentionTargetReport struct {
	ExpectedRootIdentity string `json:"expected_root_identity,omitempty"`
	ObservedRootIdentity string `json:"observed_root_identity,omitempty"`
	RootIdentityBound    bool   `json:"root_identity_bound"`
	StabilityAssurance   string `json:"stability_assurance"`
}

type RetentionProofReport struct {
	Basis               string `json:"basis"`
	IntentID            string `json:"intent_id,omitempty"`
	CompletionID        string `json:"completion_id,omitempty"`
	AttemptsRecorded    int    `json:"attempts_recorded"`
	HistoricalAuthority bool   `json:"historical_terminal_authority"`
	Assurance           string `json:"assurance"`
}

type RetentionMarkerReport struct {
	State             string `json:"state"`
	IntentMarkerID    string `json:"intent_marker_id,omitempty"`
	CompleteMarkerID  string `json:"complete_marker_id,omitempty"`
	IntentDurable     bool   `json:"intent_durable"`
	CompletionDurable bool   `json:"completion_durable"`
	ExactTombstone    bool   `json:"exact_tombstone"`
	PruneResumable    bool   `json:"prune_resumable"`
}

type RetentionWriteReport struct {
	ControlDirectoriesCreated int   `json:"control_directories_created"`
	MarkerTemporaryFiles      int   `json:"marker_temporary_files_created"`
	MarkerTemporaryBytes      int64 `json:"marker_temporary_bytes_written"`
	MarkerPublicationAttempts int   `json:"marker_publication_attempts"`
	MarkerPublications        int   `json:"marker_publications"`
	MarkerTemporaryRemovals   int   `json:"marker_temporary_removals"`
	RemovalAttempts           int   `json:"removal_attempts"`
	FilesRemoved              int   `json:"files_removed"`
	DirectoriesRemoved        int   `json:"directories_removed"`
	BytesRemoved              int64 `json:"bytes_removed"`
	AmbiguousRemovals         int   `json:"ambiguous_removals"`
}

type RetentionRemovalReport struct {
	Completion fsbind.Removal   `json:"completion"`
	Attempts   []fsbind.Removal `json:"attempts"`
	Intent     fsbind.Removal   `json:"intent"`
	Scratch    fsbind.Removal   `json:"scratch"`
}

type RetentionUsage struct {
	ObjectsConsidered   int   `json:"objects_considered"`
	PathBytesConsidered int64 `json:"path_bytes_considered"`
	BytesConsidered     int64 `json:"bytes_considered"`
}

type RetentionReport struct {
	Outcome         RetentionOutcome       `json:"outcome"`
	Effect          []string               `json:"effect"`
	WritesPerformed int                    `json:"writes_performed"`
	WritesUncertain bool                   `json:"writes_uncertain"`
	Operation       OperationReport        `json:"operation"`
	Plan            PlanReport             `json:"plan"`
	Target          RetentionTargetReport  `json:"target"`
	Proof           RetentionProofReport   `json:"proof"`
	Markers         RetentionMarkerReport  `json:"markers"`
	Writes          RetentionWriteReport   `json:"writes"`
	Removals        RetentionRemovalReport `json:"removals"`
	Limits          RetentionLimits        `json:"limits"`
	Used            RetentionUsage         `json:"used"`
	Blockers        []Finding              `json:"blockers"`
	Issues          []Finding              `json:"issues"`
	Warnings        []string               `json:"warnings"`
}

type retentionMarkerReceipt struct {
	DirectoryCreated           bool
	DirectoryDurability        string
	TemporaryCreated           bool
	TemporaryCreationUncertain bool
	TemporaryStateUncertain    bool
	TemporaryBytesWritten      int64
	AlreadyPresent             bool
	Publication                fsbind.Publication
	PublicationUncertain       bool
	TemporaryRemoval           fsbind.Removal
	TemporaryRemovalUncertain  bool
}

func newRetentionReport(options PruneOptions) RetentionReport {
	return RetentionReport{
		Outcome: RetentionOutcomeInterrupted,
		Effect: []string{
			"read_private_client_adoption_operation_state",
			"write_private_client_adoption_retention_markers",
			"delete_private_client_adoption_operation_state",
		},
		Operation: OperationReport{ID: options.OperationID.String(), Status: "inspection_incomplete", PhaseBefore: "unknown", PhaseAfter: "unknown"},
		Plan:      PlanReport{ExpectedID: options.ExpectedPlanID},
		Target:    RetentionTargetReport{StabilityAssurance: "not_observed"},
		Proof:     RetentionProofReport{Assurance: "not_observed"},
		Markers:   RetentionMarkerReport{State: "not_started"},
		Limits:    options.Limits,
		Blockers:  []Finding{}, Issues: []Finding{}, Warnings: []string{
			"prune deletes only one explicit adoption operation's owner-private journal and retains a small tombstone",
			"the downloader job, materialized final, metafile store, and every other operation remain outside prune authority",
			"the tombstone records historical completion evidence and does not prove current downloader state",
			"operation, plan, filesystem, client, path, and content references are stable pseudonyms and are not anonymization",
		},
		Removals: RetentionRemovalReport{Attempts: []fsbind.Removal{}},
	}
}

func (report *RetentionReport) addRetentionBlocker(code, message string) {
	report.Blockers = append(report.Blockers, Finding{Code: code, Message: message})
}

func (report *RetentionReport) addRetentionIssue(code, message string) {
	report.Issues = append(report.Issues, Finding{Code: code, Message: message})
}

func (report *RetentionReport) recordRetentionMarker(receipt retentionMarkerReceipt) {
	if receipt.DirectoryCreated {
		report.WritesPerformed++
		report.Writes.ControlDirectoriesCreated++
		if receipt.DirectoryDurability != "confirmed" {
			report.WritesUncertain = true
		}
	}
	if receipt.TemporaryCreated {
		report.WritesPerformed++
		report.Writes.MarkerTemporaryFiles++
		report.Writes.MarkerTemporaryBytes += receipt.TemporaryBytesWritten
	}
	if receipt.Publication.Attempted {
		report.Writes.MarkerPublicationAttempts++
	}
	if receipt.Publication.Published {
		report.WritesPerformed++
		report.Writes.MarkerPublications++
	}
	if receipt.TemporaryRemoval.Removed {
		report.WritesPerformed++
		report.Writes.MarkerTemporaryRemovals++
	}
	if receipt.TemporaryCreationUncertain || receipt.TemporaryStateUncertain || receipt.PublicationUncertain || receipt.TemporaryRemovalUncertain ||
		receipt.Publication.Attempted && (!receipt.Publication.Published || receipt.Publication.Durability != "confirmed") ||
		receipt.TemporaryRemoval.Attempted && (!receipt.TemporaryRemoval.Removed || receipt.TemporaryRemoval.Durability != "confirmed") {
		report.WritesUncertain = true
	}
}

func (report *RetentionReport) recordRetentionRemoval(removal fsbind.Removal) {
	if removal.Attempted {
		report.Writes.RemovalAttempts++
	}
	if removal.Removed {
		report.WritesPerformed++
		if removal.Kind == fsbind.ObjectKindDirectory {
			report.Writes.DirectoriesRemoved++
		} else {
			report.Writes.FilesRemoved++
			report.Writes.BytesRemoved += removal.SizeBytes
		}
	}
	if removal.Attempted && (!removal.Removed || removal.Durability != "confirmed") {
		report.WritesUncertain = true
		report.Writes.AmbiguousRemovals++
	}
}
