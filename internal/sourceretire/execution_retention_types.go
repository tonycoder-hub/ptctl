package sourceretire

import (
	"fmt"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	ExecutionRetentionOutcomePruned          = "pruned"
	ExecutionRetentionOutcomeAlreadyPruned   = "already_pruned"
	ExecutionRetentionOutcomeBlocked         = "blocked"
	ExecutionRetentionOutcomeInterrupted     = "interrupted"
	ExecutionRetentionOutcomeIntegrityFailed = "integrity_failed"
)

const (
	defaultExecutionRetentionMaxObjects   = 25_000
	defaultExecutionRetentionMaxPathBytes = int64(16 << 20)
	defaultExecutionRetentionMaxBytes     = int64(2 << 30)
	defaultExecutionRetentionMaxMemory    = int64(64 << 20)
	defaultExecutionRetentionMaxFindings  = 128

	hardExecutionRetentionMaxObjects   = 100_002
	hardExecutionRetentionMaxPathBytes = int64(64 << 20)
	hardExecutionRetentionMaxBytes     = int64(130 << 30)
	hardExecutionRetentionMaxMemory    = int64(512 << 20)
	hardExecutionRetentionMaxFindings  = 1_024
)

type ExecutionRetentionLimits struct {
	MaxObjects     int   `json:"max_objects"`
	MaxPathBytes   int64 `json:"max_path_bytes"`
	MaxBytes       int64 `json:"max_bytes"`
	MaxMemoryBytes int64 `json:"max_memory_bytes"`
	MaxFindings    int   `json:"max_findings"`
}

func DefaultExecutionRetentionLimits() ExecutionRetentionLimits {
	return ExecutionRetentionLimits{
		MaxObjects: defaultExecutionRetentionMaxObjects, MaxPathBytes: defaultExecutionRetentionMaxPathBytes,
		MaxBytes: defaultExecutionRetentionMaxBytes, MaxMemoryBytes: defaultExecutionRetentionMaxMemory,
		MaxFindings: defaultExecutionRetentionMaxFindings,
	}
}

func (limits ExecutionRetentionLimits) Validate() error {
	if limits.MaxObjects <= 0 || limits.MaxObjects > hardExecutionRetentionMaxObjects ||
		limits.MaxPathBytes <= 0 || limits.MaxPathBytes > hardExecutionRetentionMaxPathBytes ||
		limits.MaxBytes <= 0 || limits.MaxBytes > hardExecutionRetentionMaxBytes ||
		limits.MaxMemoryBytes <= 0 || limits.MaxMemoryBytes > hardExecutionRetentionMaxMemory ||
		limits.MaxFindings <= 0 || limits.MaxFindings > hardExecutionRetentionMaxFindings {
		return fmt.Errorf("%w: source retirement retention limits are invalid", ErrExecutionPolicy)
	}
	return nil
}

type ExecutionPruneOptions struct {
	TargetRoot      string                   `json:"-"`
	OperationID     OperationID              `json:"operation_id"`
	ExpectedPlanID  string                   `json:"expected_plan_id"`
	Acknowledge     bool                     `json:"acknowledge"`
	JournalLimits   ExecutionLimits          `json:"journal_limits"`
	RetentionLimits ExecutionRetentionLimits `json:"retention_limits"`
}

type ExecutionRetentionUsage struct {
	ObjectsConsidered        int   `json:"objects_considered"`
	PathBytesConsidered      int64 `json:"path_bytes_considered"`
	BytesConsidered          int64 `json:"bytes_considered"`
	MemoryBytesConsidered    int64 `json:"memory_bytes_considered"`
	DirectoryEntriesExamined int   `json:"directory_entries_examined"`
	RemovalAttempts          int   `json:"removal_attempts"`
	FilesRemoved             int   `json:"files_removed"`
	DirectoriesRemoved       int   `json:"directories_removed"`
	BytesRemoved             int64 `json:"bytes_removed"`
	AmbiguousRemovals        int   `json:"ambiguous_removals"`
}

type ExecutionRetentionProofReport struct {
	Basis                string `json:"basis"`
	IntentID             string `json:"intent_id,omitempty"`
	TerminalCompletionID string `json:"terminal_completion_id,omitempty"`
	FilesRetired         int    `json:"files_retired"`
	BytesRetired         int64  `json:"bytes_retired"`
	HistoricalAuthority  bool   `json:"historical_terminal_authority"`
	Assurance            string `json:"assurance"`
}

type ExecutionRetentionMarkerReport struct {
	State             string `json:"state"`
	IntentMarkerID    string `json:"intent_marker_id,omitempty"`
	CompleteMarkerID  string `json:"complete_marker_id,omitempty"`
	IntentDurable     bool   `json:"intent_durable"`
	CompletionDurable bool   `json:"completion_durable"`
	ExactTombstone    bool   `json:"exact_tombstone"`
	PruneResumable    bool   `json:"prune_resumable"`
}

type ExecutionRetentionWriteReport struct {
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

type ExecutionRetentionTargetReport struct {
	ExpectedRootIdentity string `json:"expected_root_identity,omitempty"`
	ObservedRootIdentity string `json:"observed_root_identity,omitempty"`
	RootIdentityBound    bool   `json:"root_identity_bound"`
	StabilityAssurance   string `json:"stability_assurance"`
}

type ExecutionRetentionReport struct {
	Outcome         string                         `json:"outcome"`
	Effect          []string                       `json:"effect"`
	WritesPerformed int                            `json:"writes_performed"`
	WritesUncertain bool                           `json:"writes_uncertain"`
	Operation       ExecutionOperationReport       `json:"operation"`
	Target          ExecutionRetentionTargetReport `json:"target"`
	Proof           ExecutionRetentionProofReport  `json:"proof"`
	Markers         ExecutionRetentionMarkerReport `json:"markers"`
	Writes          ExecutionRetentionWriteReport  `json:"writes"`
	JournalLimits   ExecutionLimits                `json:"journal_limits"`
	Limits          ExecutionRetentionLimits       `json:"limits"`
	Used            ExecutionRetentionUsage        `json:"used"`
	Blockers        []Finding                      `json:"blockers"`
	Issues          []Finding                      `json:"issues"`
	Warnings        []string                       `json:"warnings"`
}

type executionRetentionMarkerReceipt struct {
	DirectoryCreated      bool
	DirectoryDurability   string
	TemporaryCreated      bool
	TemporaryBytesWritten int64
	AlreadyPresent        bool
	Publication           fsbind.Publication
	TemporaryRemoval      fsbind.Removal
}

func newExecutionRetentionReport(options ExecutionPruneOptions) ExecutionRetentionReport {
	return ExecutionRetentionReport{
		Outcome: ExecutionRetentionOutcomeInterrupted,
		Effect: []string{
			"read_private_source_retirement_operation_state",
			"write_private_source_retirement_retention_markers",
			"delete_private_source_retirement_operation_state",
		},
		Operation: ExecutionOperationReport{ID: options.OperationID.String(), PlanID: options.ExpectedPlanID,
			Status: "inspection_incomplete", Phase: "unknown"},
		Target:        ExecutionRetentionTargetReport{StabilityAssurance: "not_observed"},
		Proof:         ExecutionRetentionProofReport{Assurance: "not_observed"},
		Markers:       ExecutionRetentionMarkerReport{State: "not_started"},
		JournalLimits: options.JournalLimits,
		Limits:        options.RetentionLimits,
		Blockers:      []Finding{}, Issues: []Finding{},
		Warnings: []string{
			"prune deletes only one explicit operation's owner-private journal and retains a small tombstone",
			"source names, the materialized final, and downloader state are never modified by prune",
			"the tombstone records historical terminal evidence and does not prove that retired names remain absent now",
			"operation, plan, filesystem, client, and lineage references are stable pseudonyms and are not anonymization",
		},
	}
}
