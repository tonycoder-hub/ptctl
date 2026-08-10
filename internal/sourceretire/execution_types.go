package sourceretire

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
)

const (
	ExecutionIntentSchemaV1   = "ptctl.source-retirement-intent/v1"
	DeleteAttemptSchemaV1     = "ptctl.source-retirement-delete-attempt/v1"
	DeleteCompleteSchemaV1    = "ptctl.source-retirement-delete-complete/v1"
	ExecutionCompleteSchemaV1 = "ptctl.source-retirement-complete/v1"

	ExecutionOutcomeRetired        = "retired"
	ExecutionOutcomeAlreadyRetired = "already_retired"
	ExecutionOutcomePartial        = "partial"
	ExecutionOutcomeBlocked        = "blocked"
	ExecutionOutcomeIncomplete     = "incomplete"
	ExecutionOutcomeIntegrity      = "integrity_failed"

	DeleteBasisConfirmed = "identity_bound_unlink_confirmed"
	DeleteBasisRecovered = "absence_after_durable_attempt_and_parent_durability_recovered"

	executionOperationDomain = "ptctl-source-retirement-operation-v1\x00"
	executionScopeDomain     = "ptctl-source-retirement-search-scope-v1\x00"
	markerIDPrefix           = "sha256:"
)

var (
	ErrExecutionPolicy    = errors.New("source retirement execution is blocked by policy")
	ErrExecutionIntegrity = errors.New("source retirement execution failed integrity validation")
	ErrOperationNotFound  = errors.New("source retirement operation was not found")
)

const (
	defaultExecutionMaxFiles       = 10_000
	hardExecutionMaxFiles          = 49_999
	defaultExecutionMaxPathBytes   = int64(8 << 20)
	hardExecutionMaxPathBytes      = int64(64 << 20)
	defaultExecutionMaxIntentBytes = int64(16 << 20)
	hardExecutionMaxIntentBytes    = int64(64 << 20)
	defaultExecutionMaxMarkerBytes = int64(64 << 10)
	hardExecutionMaxMarkerBytes    = int64(1 << 20)
	defaultExecutionMaxContent     = int64(16 << 40)
	hardExecutionMaxContent        = int64(1 << 50)
	hardExecutionMaxSearchRoots    = 64
)

type ExecutionLimits struct {
	MaxFiles        int   `json:"max_files"`
	MaxPathBytes    int64 `json:"max_path_bytes"`
	MaxIntentBytes  int64 `json:"max_intent_bytes"`
	MaxMarkerBytes  int64 `json:"max_marker_bytes"`
	MaxContentBytes int64 `json:"max_content_bytes"`
}

func DefaultExecutionLimits() ExecutionLimits {
	return ExecutionLimits{MaxFiles: defaultExecutionMaxFiles, MaxPathBytes: defaultExecutionMaxPathBytes,
		MaxIntentBytes: defaultExecutionMaxIntentBytes, MaxMarkerBytes: defaultExecutionMaxMarkerBytes,
		MaxContentBytes: defaultExecutionMaxContent}
}

func (limits ExecutionLimits) Validate() error {
	if limits.MaxFiles <= 0 || limits.MaxFiles > hardExecutionMaxFiles || limits.MaxPathBytes <= 0 || limits.MaxPathBytes > hardExecutionMaxPathBytes ||
		limits.MaxIntentBytes <= 0 || limits.MaxIntentBytes > hardExecutionMaxIntentBytes || limits.MaxMarkerBytes <= 0 || limits.MaxMarkerBytes > hardExecutionMaxMarkerBytes ||
		limits.MaxContentBytes <= 0 || limits.MaxContentBytes > hardExecutionMaxContent {
		return fmt.Errorf("%w: source retirement execution limits are invalid", ErrExecutionPolicy)
	}
	return nil
}

type OperationID string

func ParseOperationID(value string) (OperationID, error) {
	if !canonicalSHA256ID(value) {
		return "", fmt.Errorf("invalid source retirement operation ID")
	}
	return OperationID(value), nil
}

func (id OperationID) String() string { return string(id) }

func executionOperationID(planID string) (OperationID, error) {
	if !canonicalSHA256ID(planID) {
		return "", fmt.Errorf("%w: reviewed source retirement plan ID is invalid", ErrExecutionPolicy)
	}
	digest := sha256.Sum256([]byte(executionOperationDomain + planID))
	return OperationID(markerIDPrefix + hex.EncodeToString(digest[:])), nil
}

// OperationIDForPlanID deterministically derives the only operation selector
// a reviewed source-retirement plan may use. It grants no journal, source, or
// deletion authority.
func OperationIDForPlanID(planID string) (OperationID, error) {
	return executionOperationID(planID)
}

func OperationDirectoryName(id OperationID) (string, error) {
	parsed, err := ParseOperationID(id.String())
	if err != nil || parsed != id {
		return "", fmt.Errorf("invalid source retirement operation ID")
	}
	return materialize.SourceRetireOperationDirectoryPrefix + strings.TrimPrefix(id.String(), markerIDPrefix), nil
}

func ParseOperationDirectoryName(name string) (OperationID, error) {
	if !strings.HasPrefix(name, materialize.SourceRetireOperationDirectoryPrefix) {
		return "", fmt.Errorf("invalid source retirement operation directory")
	}
	return ParseOperationID(markerIDPrefix + strings.TrimPrefix(name, materialize.SourceRetireOperationDirectoryPrefix))
}

type IntentFile struct {
	Sequence             int       `json:"sequence"`
	ManifestIndex        int       `json:"manifest_index"`
	SizeBytes            int64     `json:"size_bytes"`
	ModifiedAt           time.Time `json:"modified_at"`
	SourcePathRef        string    `json:"source_path_ref"`
	ParentPath           string    `json:"parent_path"`
	ParentIdentity       string    `json:"parent_identity"`
	Name                 string    `json:"name"`
	SourceObjectIdentity string    `json:"source_object_identity"`
}

type ExecutionIntent struct {
	Schema                 string          `json:"schema"`
	OperationID            OperationID     `json:"operation_id"`
	OperationRootIdentity  string          `json:"operation_root_identity"`
	PlanID                 string          `json:"plan_id"`
	SearchScopeID          string          `json:"search_scope_id"`
	TargetRootIdentity     string          `json:"target_root_identity"`
	FinalObjectIdentity    string          `json:"final_object_identity"`
	MetafileVariantID      string          `json:"metafile_variant_id"`
	MaterializeOperationID string          `json:"materialize_operation_id"`
	MaterializePlanID      string          `json:"materialize_plan_id"`
	ActivationOperationID  string          `json:"activation_operation_id"`
	ActivationPlanID       string          `json:"activation_plan_id"`
	ClientCompletionID     string          `json:"client_completion_id"`
	CurrentClientUseID     string          `json:"current_client_use_id"`
	SourceSelectionID      string          `json:"source_selection_id"`
	Limits                 ExecutionLimits `json:"limits"`
	Files                  []IntentFile    `json:"files"`
	ContentBytes           int64           `json:"content_bytes"`
}

func (intent ExecutionIntent) Validate() error {
	if intent.Schema != ExecutionIntentSchemaV1 || intent.Limits.Validate() != nil || !canonicalSHA256ID(intent.PlanID) ||
		!canonicalSHA256ID(intent.SearchScopeID) ||
		!canonicalSHA256ID(intent.MetafileVariantID) || !canonicalSHA256ID(intent.MaterializeOperationID) || !canonicalPlanID(intent.MaterializePlanID) ||
		!canonicalSHA256ID(intent.ActivationOperationID) || !canonicalPlanID(intent.ActivationPlanID) || !canonicalSHA256ID(intent.ClientCompletionID) ||
		!canonicalSHA256ID(intent.CurrentClientUseID) || !canonicalSHA256ID(intent.SourceSelectionID) || len(intent.Files) == 0 ||
		len(intent.Files) > intent.Limits.MaxFiles || intent.ContentBytes <= 0 || intent.ContentBytes > intent.Limits.MaxContentBytes {
		return fmt.Errorf("%w: source retirement intent is invalid", ErrExecutionIntegrity)
	}
	derived, err := executionOperationID(intent.PlanID)
	if err != nil || derived != intent.OperationID {
		return fmt.Errorf("%w: source retirement operation ID disagrees", ErrExecutionIntegrity)
	}
	operationRoot, operationErr := fsbind.ParseIdentity(intent.OperationRootIdentity)
	targetRoot, targetErr := fsbind.ParseIdentity(intent.TargetRootIdentity)
	final, finalErr := fsbind.ParseIdentity(intent.FinalObjectIdentity)
	if operationErr != nil || targetErr != nil || finalErr != nil || operationRoot.IsZero() || targetRoot.IsZero() || final.IsZero() {
		return fmt.Errorf("%w: source retirement filesystem identity is invalid", ErrExecutionIntegrity)
	}
	seenPaths := make(map[string]struct{}, len(intent.Files))
	seenManifest := make(map[int]struct{}, len(intent.Files))
	var pathBytes, contentBytes int64
	for index, file := range intent.Files {
		parent, parentErr := fsbind.ParseIdentity(file.ParentIdentity)
		object, objectErr := fsbind.ParseIdentity(file.SourceObjectIdentity)
		fullPath := filepath.Join(file.ParentPath, file.Name)
		if file.Sequence != index || file.ManifestIndex < 0 || file.SizeBytes <= 0 || file.ModifiedAt.IsZero() || !canonicalSHA256ID(file.SourcePathRef) ||
			file.ParentPath == "" || !filepath.IsAbs(file.ParentPath) || filepath.Clean(file.ParentPath) != file.ParentPath ||
			strings.ContainsRune(file.ParentPath, '\x00') || file.ModifiedAt.Location() != time.UTC ||
			fsbind.ValidatePathComponents([]string{file.Name}) != nil || sourcePathRef(fullPath) != file.SourcePathRef ||
			parentErr != nil || objectErr != nil || parent.IsZero() || object.IsZero() {
			return fmt.Errorf("%w: source retirement intent file is invalid", ErrExecutionIntegrity)
		}
		if _, exists := seenPaths[file.SourcePathRef]; exists {
			return fmt.Errorf("%w: source retirement intent path is duplicated", ErrExecutionIntegrity)
		}
		if _, exists := seenManifest[file.ManifestIndex]; exists {
			return fmt.Errorf("%w: source retirement intent manifest index is duplicated", ErrExecutionIntegrity)
		}
		seenPaths[file.SourcePathRef], seenManifest[file.ManifestIndex] = struct{}{}, struct{}{}
		pathBytes += int64(len(file.ParentPath) + 1 + len(file.Name))
		contentBytes += file.SizeBytes
		if pathBytes > intent.Limits.MaxPathBytes || contentBytes < 0 || contentBytes > intent.ContentBytes {
			return fmt.Errorf("%w: source retirement intent budget is invalid", ErrExecutionIntegrity)
		}
	}
	if contentBytes != intent.ContentBytes {
		return fmt.Errorf("%w: source retirement intent byte total disagrees", ErrExecutionIntegrity)
	}
	return nil
}

type DeleteAttempt struct {
	Schema        string      `json:"schema"`
	OperationID   OperationID `json:"operation_id"`
	IntentID      string      `json:"intent_id"`
	Sequence      int         `json:"sequence"`
	ManifestIndex int         `json:"manifest_index"`
	SourcePathRef string      `json:"source_path_ref"`
	FileIdentity  string      `json:"file_identity"`
	SizeBytes     int64       `json:"size_bytes"`
}

type DeleteComplete struct {
	Schema        string      `json:"schema"`
	OperationID   OperationID `json:"operation_id"`
	IntentID      string      `json:"intent_id"`
	AttemptID     string      `json:"attempt_id"`
	Sequence      int         `json:"sequence"`
	ManifestIndex int         `json:"manifest_index"`
	SourcePathRef string      `json:"source_path_ref"`
	FileIdentity  string      `json:"file_identity"`
	SizeBytes     int64       `json:"size_bytes"`
	Basis         string      `json:"basis"`
}

type ExecutionComplete struct {
	Schema              string      `json:"schema"`
	OperationID         OperationID `json:"operation_id"`
	IntentID            string      `json:"intent_id"`
	FilesRetired        int         `json:"files_retired"`
	BytesRetired        int64       `json:"bytes_retired"`
	FinalObjectIdentity string      `json:"final_object_identity"`
	CurrentClientUseID  string      `json:"current_client_use_id"`
	ClientSnapshotID    string      `json:"client_snapshot_id"`
}

type ExecutionUsage struct {
	FilesConsidered int   `json:"files_considered"`
	PathBytes       int64 `json:"path_bytes"`
	ContentBytes    int64 `json:"content_bytes"`
	JournalBytes    int64 `json:"journal_bytes"`
}

type ExecutionWrites struct {
	JournalObjectsPublished int   `json:"journal_objects_published"`
	JournalBytesWritten     int64 `json:"journal_bytes_written"`
	DeletionAttempts        int   `json:"deletion_attempts"`
	NamesRemoved            int   `json:"names_removed"`
	BytesRemoved            int64 `json:"bytes_removed"`
	DurabilityUnconfirmed   int   `json:"durability_unconfirmed"`
	AmbiguousRemovals       int   `json:"ambiguous_removals"`
}

type ExecutionFileReport struct {
	Sequence      int    `json:"sequence"`
	ManifestIndex int    `json:"manifest_index"`
	SourcePathRef string `json:"source_path_ref"`
	SizeBytes     int64  `json:"size_bytes"`
	Status        string `json:"status"`
	AttemptID     string `json:"attempt_id,omitempty"`
	CompletionID  string `json:"completion_id,omitempty"`
	Basis         string `json:"basis,omitempty"`
}

type ExecutionOperationReport struct {
	ID           string `json:"id,omitempty"`
	PlanID       string `json:"plan_id,omitempty"`
	Status       string `json:"status"`
	Phase        string `json:"phase"`
	Resumable    bool   `json:"resumable"`
	IntentID     string `json:"intent_id,omitempty"`
	CompletionID string `json:"completion_id,omitempty"`
}

type ExecutionReport struct {
	Outcome           string                       `json:"outcome"`
	Effect            []string                     `json:"effect"`
	WritesPerformed   int                          `json:"writes_performed"`
	WritesUncertain   bool                         `json:"writes_uncertain"`
	DeletionPerformed bool                         `json:"deletion_performed"`
	Operation         ExecutionOperationReport     `json:"operation"`
	Eligibility       *Report                      `json:"eligibility,omitempty"`
	Limits            ExecutionLimits              `json:"limits"`
	Used              ExecutionUsage               `json:"used"`
	Writes            ExecutionWrites              `json:"writes"`
	Files             []ExecutionFileReport        `json:"files"`
	Final             materialize.FinalObservation `json:"materialized_final"`
	ClientUse         ClientUseReport              `json:"current_client_use"`
	Blockers          []Finding                    `json:"blockers"`
	Issues            []Finding                    `json:"issues"`
	Warnings          []string                     `json:"warnings"`
}

func newExecutionReport(limits ExecutionLimits) ExecutionReport {
	return ExecutionReport{Outcome: ExecutionOutcomeIncomplete,
		Effect: []string{}, Limits: limits,
		Operation: ExecutionOperationReport{Status: "not_created", Phase: "planned"},
		Files:     []ExecutionFileReport{}, Blockers: []Finding{}, Issues: []Finding{},
		Warnings: []string{
			"source deletion is explicit, per-name, identity-bound, and cannot be rolled back",
			"only content-bearing regular-file names are removed; parent directories, aliases, empty files, padding, and the published final remain untouched",
			"removed bytes may remain reachable through unselected hardlinks and no reclaimed-space claim is made",
			"live downloader state and paths remain bracketed non-atomic lexical claims rather than proof of a remote open inode",
			"operation, plan, filesystem, client, and path references are stable pseudonyms and are not anonymization",
		},
	}
}

func (report *ExecutionReport) addBlocker(code, message string) {
	report.Blockers = append(report.Blockers, Finding{Code: code, Message: message})
}

func (report *ExecutionReport) addIssue(code, message string) {
	report.Issues = append(report.Issues, Finding{Code: code, Message: message})
}

func (report *ExecutionReport) addEffect(values ...string) {
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

func (report *ExecutionReport) finalize() {
	if report.Effect == nil {
		report.Effect = []string{}
	}
	sort.Strings(report.Effect)
	if report.Files == nil {
		report.Files = []ExecutionFileReport{}
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
	sort.Slice(report.Blockers, func(i, j int) bool {
		return report.Blockers[i].Code < report.Blockers[j].Code || report.Blockers[i].Code == report.Blockers[j].Code && report.Blockers[i].Message < report.Blockers[j].Message
	})
	sort.Slice(report.Issues, func(i, j int) bool {
		return report.Issues[i].Code < report.Issues[j].Code || report.Issues[i].Code == report.Issues[j].Code && report.Issues[i].Message < report.Issues[j].Message
	})
	sort.Strings(report.Warnings)
}

type RunOptions struct {
	Review         BuildOptions
	ExpectedPlanID string
	SearchRoots    []string
	Acknowledge    bool
	Limits         ExecutionLimits
}

type ResumeOptions struct {
	Meta           *metafile.MetaInfo
	Final          *materialize.VerifiedFinal
	Activation     *clientactivate.VerifiedCompletion
	ClientUse      *clientactivate.CurrentUseAuthority
	ClientSession  downloader.LedgerSession
	TargetRoot     string
	OperationID    OperationID
	ExpectedPlanID string
	SearchRoots    []string
	Acknowledge    bool
	Limits         ExecutionLimits
}

type StatusOptions struct {
	TargetRoot  string
	OperationID OperationID
	Limits      ExecutionLimits
}

func searchScopeID(roots []string) (string, error) {
	if len(roots) == 0 || len(roots) > hardExecutionMaxSearchRoots {
		return "", fmt.Errorf("%w: source search-root count is invalid", ErrExecutionPolicy)
	}
	values := append([]string(nil), roots...)
	sort.Strings(values)
	for i, root := range values {
		if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || i > 0 && root == values[i-1] {
			return "", fmt.Errorf("%w: source search-root scope is invalid", ErrExecutionPolicy)
		}
	}
	digest := sha256.Sum256([]byte(executionScopeDomain + strings.Join(values, "\x00")))
	return markerIDPrefix + hex.EncodeToString(digest[:]), nil
}

func canonicalFSIdentity(value string) bool {
	identity, err := fsbind.ParseIdentity(value)
	return err == nil && !identity.IsZero() && identity.String() == value
}
