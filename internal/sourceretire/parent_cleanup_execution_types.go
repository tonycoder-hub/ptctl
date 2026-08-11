package sourceretire

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

const (
	ParentCleanupIntentSchemaV1   = "ptctl.source-retirement-parent-cleanup-intent/v1"
	ParentCleanupAttemptSchemaV1  = "ptctl.source-retirement-parent-cleanup-attempt/v1"
	ParentCleanupRemovedSchemaV1  = "ptctl.source-retirement-parent-cleanup-removed/v1"
	ParentCleanupCompleteSchemaV1 = "ptctl.source-retirement-parent-cleanup-complete/v1"

	ParentCleanupExecutionOutcomeRemoved        = "parents_removed"
	ParentCleanupExecutionOutcomeAlreadyRemoved = "already_removed"
	ParentCleanupExecutionOutcomePartial        = "partial"
	ParentCleanupExecutionOutcomeBlocked        = "blocked"
	ParentCleanupExecutionOutcomeIncomplete     = "incomplete"
	ParentCleanupExecutionOutcomeIntegrity      = "integrity_failed"

	ParentCleanupRemovalBasisConfirmed = "confirmed_empty_identity_bound_removal"
	ParentCleanupRemovalBasisRecovered = "recovered_absence_after_durable_attempt"

	parentCleanupOperationDomain = "ptctl-source-retirement-parent-cleanup-operation-v1\x00"
	parentCleanupIntentDomain    = "ptctl-source-retirement-parent-cleanup-intent-v1\x00"
	parentCleanupAttemptDomain   = "ptctl-source-retirement-parent-cleanup-attempt-v1\x00"
	parentCleanupRemovedDomain   = "ptctl-source-retirement-parent-cleanup-removed-v1\x00"
	parentCleanupCompleteDomain  = "ptctl-source-retirement-parent-cleanup-complete-v1\x00"

	defaultParentCleanupExecutionMaxParents   = 10_000
	hardParentCleanupExecutionMaxParents      = 49_000
	defaultParentCleanupExecutionMaxPathBytes = int64(8 << 20)
	hardParentCleanupExecutionMaxPathBytes    = int64(64 << 20)
	defaultParentCleanupExecutionMaxIntent    = int64(16 << 20)
	hardParentCleanupExecutionMaxIntent       = int64(64 << 20)
	defaultParentCleanupExecutionMaxMarker    = int64(64 << 10)
	hardParentCleanupExecutionMaxMarker       = int64(1 << 20)
	defaultParentCleanupExecutionMaxScratch   = int64(16 << 20)
	hardParentCleanupExecutionMaxScratch      = int64(64 << 20)
	defaultParentCleanupExecutionMaxEntryName = int64(64 << 10)
	hardParentCleanupExecutionMaxEntryName    = int64(1 << 20)
)

type ParentCleanupOperationID string

func ParseParentCleanupOperationID(value string) (ParentCleanupOperationID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("parent-cleanup operation ID is invalid")
	}
	return ParentCleanupOperationID(value), nil
}

func (id ParentCleanupOperationID) String() string { return string(id) }

func ParentCleanupOperationIDForPlanID(planID string) (ParentCleanupOperationID, error) {
	if !canonicalSHA256ID(planID) {
		return "", fmt.Errorf("parent-cleanup plan ID is invalid")
	}
	digest := sha256.Sum256([]byte(parentCleanupOperationDomain + planID))
	return ParentCleanupOperationID(markerIDPrefix + hex.EncodeToString(digest[:])), nil
}

func ParentCleanupOperationDirectoryName(id ParentCleanupOperationID) (string, error) {
	parsed, err := ParseParentCleanupOperationID(id.String())
	if err != nil || parsed != id {
		return "", fmt.Errorf("parent-cleanup operation ID is invalid")
	}
	return materialize.ParentCleanupOperationDirectoryPrefix + strings.TrimPrefix(id.String(), markerIDPrefix), nil
}

func ParseParentCleanupOperationDirectoryName(name string) (ParentCleanupOperationID, error) {
	if !strings.HasPrefix(name, materialize.ParentCleanupOperationDirectoryPrefix) {
		return "", fmt.Errorf("parent-cleanup operation directory is invalid")
	}
	return ParseParentCleanupOperationID(markerIDPrefix + strings.TrimPrefix(name, materialize.ParentCleanupOperationDirectoryPrefix))
}

type ParentCleanupExecutionLimits struct {
	MaxParents        int   `json:"max_parents"`
	MaxPathBytes      int64 `json:"max_path_bytes"`
	MaxIntentBytes    int64 `json:"max_intent_bytes"`
	MaxMarkerBytes    int64 `json:"max_marker_bytes"`
	MaxScratchBytes   int64 `json:"max_scratch_bytes"`
	MaxEntryNameBytes int64 `json:"max_entry_name_bytes"`
}

func DefaultParentCleanupExecutionLimits() ParentCleanupExecutionLimits {
	return ParentCleanupExecutionLimits{
		MaxParents: defaultParentCleanupExecutionMaxParents, MaxPathBytes: defaultParentCleanupExecutionMaxPathBytes,
		MaxIntentBytes: defaultParentCleanupExecutionMaxIntent, MaxMarkerBytes: defaultParentCleanupExecutionMaxMarker,
		MaxScratchBytes: defaultParentCleanupExecutionMaxScratch, MaxEntryNameBytes: defaultParentCleanupExecutionMaxEntryName,
	}
}

func (limits ParentCleanupExecutionLimits) Validate() error {
	if limits.MaxParents <= 0 || limits.MaxParents > hardParentCleanupExecutionMaxParents ||
		limits.MaxPathBytes <= 0 || limits.MaxPathBytes > hardParentCleanupExecutionMaxPathBytes ||
		limits.MaxIntentBytes <= 0 || limits.MaxIntentBytes > hardParentCleanupExecutionMaxIntent ||
		limits.MaxMarkerBytes <= 0 || limits.MaxMarkerBytes > hardParentCleanupExecutionMaxMarker ||
		limits.MaxScratchBytes <= 0 || limits.MaxScratchBytes > hardParentCleanupExecutionMaxScratch ||
		limits.MaxEntryNameBytes <= 0 || limits.MaxEntryNameBytes > hardParentCleanupExecutionMaxEntryName ||
		limits.MaxScratchBytes < limits.MaxIntentBytes || limits.MaxScratchBytes < limits.MaxMarkerBytes ||
		(fsbind.ListLimits{MaxEntries: 1, MaxNameBytes: limits.MaxEntryNameBytes}).Validate() != nil ||
		(fsbind.ListLimits{MaxEntries: 2*limits.MaxParents + 2, MaxNameBytes: int64(2*limits.MaxParents+2) * 112}).Validate() != nil {
		return fmt.Errorf("%w: parent-cleanup execution limits are invalid", ErrExecutionPolicy)
	}
	return nil
}

type ParentCleanupIntentDirectory struct {
	Sequence       int    `json:"sequence"`
	ParentPath     string `json:"parent_path"`
	ParentPathRef  string `json:"parent_path_ref"`
	ParentIdentity string `json:"parent_identity"`
	RetiredFiles   int    `json:"retired_files"`
}

type ParentCleanupIntent struct {
	Schema                 string                         `json:"schema"`
	OperationID            ParentCleanupOperationID       `json:"operation_id"`
	OperationRootIdentity  string                         `json:"operation_root_identity"`
	TargetRootIdentity     string                         `json:"target_root_identity"`
	CleanupPlanID          string                         `json:"cleanup_plan_id"`
	RetirementOperationID  OperationID                    `json:"retirement_operation_id"`
	RetirementPlanID       string                         `json:"retirement_plan_id"`
	RetirementCompletionID string                         `json:"retirement_completion_id"`
	SearchScopeID          string                         `json:"search_scope_id"`
	Limits                 ParentCleanupExecutionLimits   `json:"limits"`
	Directories            []ParentCleanupIntentDirectory `json:"directories"`
}

func (intent ParentCleanupIntent) Validate() error {
	operation, operationErr := ParseParentCleanupOperationID(intent.OperationID.String())
	expectedOperation, deriveErr := ParentCleanupOperationIDForPlanID(intent.CleanupPlanID)
	retirement, retirementErr := ParseOperationID(intent.RetirementOperationID.String())
	expectedRetirement, retirementDeriveErr := executionOperationID(intent.RetirementPlanID)
	rootIdentity, rootErr := fsbind.ParseIdentity(intent.OperationRootIdentity)
	targetIdentity, targetErr := fsbind.ParseIdentity(intent.TargetRootIdentity)
	if intent.Schema != ParentCleanupIntentSchemaV1 || operationErr != nil || deriveErr != nil || operation != intent.OperationID ||
		expectedOperation != intent.OperationID || retirementErr != nil || retirementDeriveErr != nil || retirement != expectedRetirement ||
		rootErr != nil || targetErr != nil || rootIdentity.IsZero() || targetIdentity.IsZero() ||
		!canonicalSHA256ID(intent.RetirementCompletionID) || !canonicalSHA256ID(intent.SearchScopeID) ||
		intent.Limits.Validate() != nil || len(intent.Directories) <= 0 || len(intent.Directories) > intent.Limits.MaxParents {
		return fmt.Errorf("%w: parent-cleanup intent is invalid", ErrExecutionIntegrity)
	}
	seenPaths := make(map[string]struct{}, len(intent.Directories))
	seenRefs := make(map[string]struct{}, len(intent.Directories))
	seenIdentities := make(map[string]struct{}, len(intent.Directories))
	var pathBytes int64
	for index, directory := range intent.Directories {
		identity, identityErr := fsbind.ParseIdentity(directory.ParentIdentity)
		base := filepath.Base(directory.ParentPath)
		if directory.Sequence != index || directory.ParentPath == "" || !filepath.IsAbs(directory.ParentPath) ||
			filepath.Clean(directory.ParentPath) != directory.ParentPath || parentCleanupPathRef(directory.ParentPath) != directory.ParentPathRef ||
			identityErr != nil || identity.IsZero() || directory.RetiredFiles <= 0 || fsbind.ValidatePathComponents([]string{base}) != nil ||
			materialize.IsReservedControlName(base) {
			return fmt.Errorf("%w: parent-cleanup intent directory is invalid", ErrExecutionIntegrity)
		}
		if _, duplicate := seenPaths[directory.ParentPath]; duplicate {
			return fmt.Errorf("%w: parent-cleanup intent path is duplicated", ErrExecutionIntegrity)
		}
		if _, duplicate := seenRefs[directory.ParentPathRef]; duplicate {
			return fmt.Errorf("%w: parent-cleanup intent path reference is duplicated", ErrExecutionIntegrity)
		}
		if _, duplicate := seenIdentities[directory.ParentIdentity]; duplicate {
			return fmt.Errorf("%w: parent-cleanup intent identity is duplicated", ErrExecutionIntegrity)
		}
		seenPaths[directory.ParentPath], seenRefs[directory.ParentPathRef], seenIdentities[directory.ParentIdentity] = struct{}{}, struct{}{}, struct{}{}
		pathBytes += int64(len(directory.ParentPath))
		if pathBytes < 0 || pathBytes > intent.Limits.MaxPathBytes {
			return fmt.Errorf("%w: parent-cleanup intent path budget is invalid", ErrExecutionIntegrity)
		}
	}
	return nil
}

type ParentCleanupAttempt struct {
	Schema         string                   `json:"schema"`
	OperationID    ParentCleanupOperationID `json:"operation_id"`
	IntentID       string                   `json:"intent_id"`
	Sequence       int                      `json:"sequence"`
	ParentPathRef  string                   `json:"parent_path_ref"`
	ParentIdentity string                   `json:"parent_identity"`
}

type ParentCleanupRemoved struct {
	Schema         string                   `json:"schema"`
	OperationID    ParentCleanupOperationID `json:"operation_id"`
	IntentID       string                   `json:"intent_id"`
	AttemptID      string                   `json:"attempt_id"`
	Sequence       int                      `json:"sequence"`
	ParentPathRef  string                   `json:"parent_path_ref"`
	ParentIdentity string                   `json:"parent_identity"`
	Basis          string                   `json:"basis"`
}

type ParentCleanupComplete struct {
	Schema         string                   `json:"schema"`
	OperationID    ParentCleanupOperationID `json:"operation_id"`
	IntentID       string                   `json:"intent_id"`
	ParentsRemoved int                      `json:"parents_removed"`
}

type ParentCleanupRunOptions struct {
	Review                ParentCleanupOptions
	ExpectedCleanupPlanID string
	Acknowledge           bool
	Limits                ParentCleanupExecutionLimits
	ShowAbsolutePaths     bool
}

type ParentCleanupResumeOptions struct {
	TargetRoot            string
	OperationID           ParentCleanupOperationID
	ExpectedCleanupPlanID string
	SearchRoots           []string
	AllowNetwork          bool
	Acknowledge           bool
	Limits                ParentCleanupExecutionLimits
	ShowAbsolutePaths     bool
}

type ParentCleanupStatusOptions struct {
	TargetRoot        string
	OperationID       ParentCleanupOperationID
	Limits            ParentCleanupExecutionLimits
	ShowAbsolutePaths bool
}

type ParentCleanupExecutionOperationReport struct {
	ID           string `json:"id,omitempty"`
	PlanID       string `json:"plan_id,omitempty"`
	IntentID     string `json:"intent_id,omitempty"`
	CompletionID string `json:"completion_id,omitempty"`
	Status       string `json:"status"`
	Phase        string `json:"phase"`
	Resumable    bool   `json:"resumable"`
}

type ParentCleanupExecutionTargetReport struct {
	ExpectedRootIdentity string `json:"expected_root_identity,omitempty"`
	ObservedRootIdentity string `json:"observed_root_identity,omitempty"`
	RootIdentityBound    bool   `json:"root_identity_bound"`
	SameFilesystem       bool   `json:"same_filesystem"`
}

type ParentCleanupExecutionDirectoryReport struct {
	Sequence       int    `json:"sequence"`
	ParentPathRef  string `json:"parent_path_ref"`
	ParentPath     string `json:"parent_path,omitempty"`
	ParentIdentity string `json:"parent_identity"`
	RetiredFiles   int    `json:"retired_files"`
	Status         string `json:"status"`
	AttemptID      string `json:"attempt_id,omitempty"`
	RemovalID      string `json:"removal_id,omitempty"`
	RemovalBasis   string `json:"removal_basis,omitempty"`
}

type ParentCleanupExecutionWrites struct {
	ScratchFilesCreated     int   `json:"scratch_files_created"`
	ScratchBytesWritten     int64 `json:"scratch_bytes_written"`
	JournalObjectsPublished int   `json:"journal_objects_published"`
	JournalObjectsExisting  int   `json:"journal_objects_existing"`
	RemovalAttempts         int   `json:"removal_attempts"`
	DirectoriesRemoved      int   `json:"directories_removed"`
	DurabilityUnconfirmed   int   `json:"durability_unconfirmed"`
	AmbiguousPublications   int   `json:"ambiguous_publications"`
	AmbiguousRemovals       int   `json:"ambiguous_removals"`
}

type ParentCleanupExecutionUsage struct {
	ParentsConsidered int   `json:"parents_considered"`
	ParentPathBytes   int64 `json:"parent_path_bytes"`
	DirectoryReads    int   `json:"directory_reads"`
	EntriesObserved   int   `json:"entries_observed"`
	EntryNameBytes    int64 `json:"entry_name_bytes"`
	JournalBytes      int64 `json:"journal_bytes"`
}

type ParentCleanupExecutionReport struct {
	Outcome                string                                  `json:"outcome"`
	Effect                 []string                                `json:"effect"`
	WritesPerformed        int                                     `json:"writes_performed"`
	WritesUncertain        bool                                    `json:"writes_uncertain"`
	DeletionPerformed      bool                                    `json:"deletion_performed"`
	Eligibility            *ParentCleanupReport                    `json:"eligibility,omitempty"`
	Operation              ParentCleanupExecutionOperationReport   `json:"operation"`
	Target                 ParentCleanupExecutionTargetReport      `json:"target"`
	RetirementOperationID  string                                  `json:"retirement_operation_id,omitempty"`
	RetirementPlanID       string                                  `json:"retirement_plan_id,omitempty"`
	RetirementCompletionID string                                  `json:"retirement_completion_id,omitempty"`
	SearchScopeID          string                                  `json:"search_scope_id,omitempty"`
	Directories            []ParentCleanupExecutionDirectoryReport `json:"directories"`
	Limits                 ParentCleanupExecutionLimits            `json:"limits"`
	Used                   ParentCleanupExecutionUsage             `json:"used"`
	Writes                 ParentCleanupExecutionWrites            `json:"writes"`
	Blockers               []Finding                               `json:"blockers"`
	Issues                 []Finding                               `json:"issues"`
	Warnings               []string                                `json:"warnings"`
}

func newParentCleanupExecutionReport(limits ParentCleanupExecutionLimits) ParentCleanupExecutionReport {
	return ParentCleanupExecutionReport{
		Outcome: ParentCleanupExecutionOutcomeIncomplete, Effect: []string{},
		Operation:   ParentCleanupExecutionOperationReport{Status: "not_created", Phase: "planned"},
		Directories: []ParentCleanupExecutionDirectoryReport{}, Limits: limits,
		Blockers: []Finding{}, Issues: []Finding{},
		Warnings: []string{
			"the operation removes only exact immediate parents captured by a same-call eligible cleanup review; it never removes search roots, ancestors, files, or non-empty directories",
			"directory removal is identity-bound and durable but observations remain bracketed non-atomic with respect to out-of-band writers",
			"status is historical journal evidence only and does not assert that a removed directory remains absent now",
		},
	}
}

func (report *ParentCleanupExecutionReport) addEffect(values ...string) {
	for _, value := range values {
		present := false
		for _, existing := range report.Effect {
			if existing == value {
				present = true
				break
			}
		}
		if !present {
			report.Effect = append(report.Effect, value)
		}
	}
}

func (report *ParentCleanupExecutionReport) addBlocker(code, message string) {
	report.Blockers = append(report.Blockers, Finding{Code: code, Message: message})
}

func (report *ParentCleanupExecutionReport) addIssue(code, message string) {
	report.Issues = append(report.Issues, Finding{Code: code, Message: message})
}

func (report *ParentCleanupExecutionReport) finalize() {
	if report.Effect == nil {
		report.Effect = []string{}
	}
	if report.Directories == nil {
		report.Directories = []ParentCleanupExecutionDirectoryReport{}
	}
	if report.Blockers == nil {
		report.Blockers = []Finding{}
	}
	if report.Issues == nil {
		report.Issues = []Finding{}
	}
	if report.Warnings == nil {
		report.Warnings = []string{}
	}
	sort.Strings(report.Effect)
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
