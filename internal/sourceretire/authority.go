package sourceretire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

var (
	// ErrCurrentAbsenceUnavailable means the historical retirement could not
	// be rebound to its original source namespace for a current observation.
	// It does not imply that the canonical retirement journal is corrupt.
	ErrCurrentAbsenceUnavailable = errors.New("current retired-source absence is unavailable")
	// ErrRetiredNamePresent means a complete current observation found at least
	// one explicitly retired source name again. It is a current-state conflict,
	// not evidence that the historical retirement journal is corrupt.
	ErrRetiredNamePresent = errors.New("a retired source name is currently present")
)

// CompletionProofOptions selects one explicit terminal source-retirement
// operation. Reading the journal or retained tombstone is local and read-only.
type CompletionProofOptions struct {
	TargetRoot     string
	OperationID    OperationID
	ExpectedPlanID string
	Limits         ExecutionLimits
}

// CompletionObservation is a public, non-authoritative view of one canonical
// terminal source-retirement journal or retained tombstone. It is historical
// evidence and does not assert that a retired name remains absent now.
type CompletionObservation struct {
	OperationID            string `json:"operation_id"`
	PlanID                 string `json:"plan_id"`
	IntentID               string `json:"intent_id"`
	CompletionID           string `json:"completion_id"`
	SearchScopeID          string `json:"search_scope_id"`
	MetafileVariantID      string `json:"metafile_variant_id"`
	MaterializeOperationID string `json:"materialize_operation_id"`
	MaterializePlanID      string `json:"materialize_plan_id"`
	ActivationOperationID  string `json:"activation_operation_id"`
	ActivationPlanID       string `json:"activation_plan_id"`
	ClientCompletionID     string `json:"client_completion_id"`
	CurrentClientUseID     string `json:"current_client_use_id"`
	SourceSelectionID      string `json:"source_selection_id"`
	TargetRootIdentity     string `json:"target_root_identity"`
	FinalObjectIdentity    string `json:"final_object_identity"`
	ClientSnapshotID       string `json:"client_snapshot_id"`
	FilesRetired           int    `json:"files_retired"`
	BytesRetired           int64  `json:"bytes_retired"`
	RetainedTombstone      bool   `json:"retained_tombstone"`
	Assurance              string `json:"assurance"`
}

// CurrentAbsenceObservation describes a same-invocation, two-pass observation
// of the exact retired names under their identity-bound original parents.
// Paths are deliberately absent from the public DTO.
type CurrentAbsenceObservation struct {
	OperationID              string    `json:"operation_id"`
	PlanID                   string    `json:"plan_id"`
	CompletionID             string    `json:"completion_id"`
	AbsenceID                string    `json:"absence_id"`
	SearchScopeID            string    `json:"search_scope_id"`
	FilesChecked             int       `json:"files_checked"`
	BytesRetired             int64     `json:"bytes_retired"`
	ParentDirectoriesChecked int       `json:"parent_directories_checked"`
	ObservedAtStart          time.Time `json:"observed_at_start"`
	ObservedAtEnd            time.Time `json:"observed_at_end"`
	Assurance                string    `json:"assurance"`
}

type completionAuthority struct {
	observation CompletionObservation
	intent      *ExecutionIntent
}

// VerifiedCompletion is process-local authority that one canonical terminal
// source-retirement record was read from the bound target root. JSON cannot
// recreate this authority.
type VerifiedCompletion struct {
	authority *completionAuthority
}

type currentAbsenceAuthority struct {
	completion  *VerifiedCompletion
	observation CurrentAbsenceObservation
}

// VerifiedCurrentAbsence is process-local authority for the bounded current
// absence observation. It remains sequential and non-atomic.
type VerifiedCurrentAbsence struct {
	authority *currentAbsenceAuthority
}

// VerifyCompletion reads either the live terminal journal or the exact
// retained tombstone. A retained tombstone deliberately carries no path
// authority, so it cannot later produce VerifiedCurrentAbsence.
func VerifyCompletion(ctx context.Context, options CompletionProofOptions) (*VerifiedCompletion, CompletionObservation, error) {
	if options.TargetRoot == "" || options.Limits.Validate() != nil || !canonicalSHA256ID(options.ExpectedPlanID) {
		return nil, CompletionObservation{}, fmt.Errorf("%w: source retirement completion selector is invalid", ErrExecutionPolicy)
	}
	derived, deriveErr := executionOperationID(options.ExpectedPlanID)
	if parsed, err := ParseOperationID(options.OperationID.String()); deriveErr != nil || err != nil || parsed != options.OperationID || derived != options.OperationID {
		return nil, CompletionObservation{}, fmt.Errorf("%w: source retirement completion selector is invalid", ErrExecutionPolicy)
	}
	if err := ctx.Err(); err != nil {
		return nil, CompletionObservation{}, err
	}
	target, targetInfo, err := fsbind.BindExisting(options.TargetRoot)
	if err != nil {
		return nil, CompletionObservation{}, err
	}
	defer target.Close()
	subtree, err := openExecutionRetentionSubtree(target, options.OperationID)
	if err != nil {
		return nil, CompletionObservation{}, err
	}
	defer subtree.Close()

	retention, err := loadExecutionRetentionState(ctx, subtree, options.OperationID, targetInfo.Identity)
	if err != nil {
		return nil, CompletionObservation{}, err
	}
	var authority completionAuthority
	if retention.DirectoryPresent {
		if !retention.IntentPresent || !retention.CompletePresent {
			if auditErr := auditExecutionRetentionControl(ctx, subtree, retention, true); auditErr != nil {
				return nil, CompletionObservation{}, auditErr
			}
			return nil, CompletionObservation{}, fmt.Errorf("%w: source retirement retention is not terminal", ErrExecutionPolicy)
		}
		if err := verifyExecutionRetentionTombstone(ctx, subtree, options.OperationID, targetInfo.Identity); err != nil {
			return nil, CompletionObservation{}, err
		}
		if retention.Intent.PlanID != options.ExpectedPlanID {
			return nil, CompletionObservation{}, fmt.Errorf("%w: source retirement completion selector disagrees", ErrExecutionPolicy)
		}
		authority.observation = completionObservationFromRetention(retention.Intent)
	} else {
		journal, loadErr := loadExecutionJournalFromSubtree(ctx, target, subtree, options.Limits)
		if loadErr != nil {
			return nil, CompletionObservation{}, loadErr
		}
		state := journal.state
		if state.Intent.PlanID != options.ExpectedPlanID || state.Intent.OperationID != options.OperationID || !state.CompletePresent {
			return nil, CompletionObservation{}, fmt.Errorf("%w: source retirement completion is unavailable or disagrees with the selector", ErrExecutionPolicy)
		}
		for _, present := range state.DeletedPresent {
			if !present {
				return nil, CompletionObservation{}, fmt.Errorf("%w: source retirement completion is not terminal", ErrExecutionPolicy)
			}
		}
		scratchPath, _ := fsbind.PathFromComponents([]string{executionScratchDirectory})
		scratch, listErr := subtree.List(ctx, scratchPath, fsbind.ListLimits{MaxEntries: 2, MaxNameBytes: options.Limits.MaxPathBytes})
		if listErr != nil {
			return nil, CompletionObservation{}, classifyExecutionBindingError(listErr)
		}
		if !scratch.Complete || len(scratch.Entries) != 0 {
			return nil, CompletionObservation{}, fmt.Errorf("%w: terminal source retirement still has pending private state", ErrExecutionPolicy)
		}
		journalPath, _ := fsbind.PathFromComponents([]string{executionJournalDirectory})
		if err := subtree.CheckPaths(journalPath, scratchPath); err != nil {
			return nil, CompletionObservation{}, classifyExecutionBindingError(err)
		}
		intent := state.Intent
		authority.intent = &intent
		authority.observation = completionObservationFromJournal(state)
	}
	if err := target.Check(); err != nil {
		return nil, CompletionObservation{}, classifyExecutionBindingError(err)
	}
	verified := &VerifiedCompletion{authority: &authority}
	if !verified.Verified() {
		return nil, CompletionObservation{}, fmt.Errorf("%w: terminal source retirement completion is invalid", ErrExecutionIntegrity)
	}
	return verified, verified.Observation(), nil
}

// VerifyCurrentAbsence rebinds the live journal's exact original source
// parents within an explicit root scope and observes every retired name absent
// twice. Retained tombstones cannot authorize this read because their path
// details were intentionally pruned.
func VerifyCurrentAbsence(ctx context.Context, completion *VerifiedCompletion, searchRoots []string, allowNetwork bool) (*VerifiedCurrentAbsence, CurrentAbsenceObservation, error) {
	if completion == nil || !completion.Verified() || completion.authority.intent == nil {
		return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: live source retirement path authority is unavailable", ErrCurrentAbsenceUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, CurrentAbsenceObservation{}, err
	}
	started := time.Now().UTC()
	roots, scopeID, err := normalizeExecutionRoots(ctx, searchRoots, allowNetwork)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, CurrentAbsenceObservation{}, ctxErr
		}
		return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: source search-root scope could not be rebound", ErrCurrentAbsenceUnavailable)
	}
	intent := *completion.authority.intent
	if scopeID != intent.SearchScopeID {
		return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: source search-root scope disagrees with the retirement journal", ErrCurrentAbsenceUnavailable)
	}
	filesByParent := make(map[string][]int)
	for index, file := range intent.Files {
		full := filepath.Join(file.ParentPath, file.Name)
		if !withinExecutionRoots(roots, full) || sourcePathRef(full) != file.SourcePathRef {
			return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: retired source path lies outside the explicit scope", ErrCurrentAbsenceUnavailable)
		}
		filesByParent[file.ParentPath] = append(filesByParent[file.ParentPath], index)
	}
	parents := make([]string, 0, len(filesByParent))
	for parent := range filesByParent {
		parents = append(parents, parent)
	}
	sort.Strings(parents)
	for pass := 0; pass < 2; pass++ {
		for _, parent := range parents {
			if err := ctx.Err(); err != nil {
				return nil, CurrentAbsenceObservation{}, err
			}
			session, _, bindErr := fsbind.BindExisting(parent)
			if bindErr != nil {
				return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: retired source parent could not be rebound", ErrCurrentAbsenceUnavailable)
			}
			if session.Info().Identity.String() != intent.Files[filesByParent[parent][0]].ParentIdentity {
				_ = session.Close()
				return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: retired source parent identity changed", ErrCurrentAbsenceUnavailable)
			}
			for _, index := range filesByParent[parent] {
				_, inspectErr := session.InspectRoot(ctx, intent.Files[index].Name)
				switch {
				case inspectErr == nil:
					_ = session.Close()
					return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w", ErrRetiredNamePresent)
				case errors.Is(inspectErr, fsbind.ErrNotFound):
					continue
				case errors.Is(inspectErr, context.Canceled), errors.Is(inspectErr, context.DeadlineExceeded):
					_ = session.Close()
					return nil, CurrentAbsenceObservation{}, inspectErr
				default:
					_ = session.Close()
					return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: retired source namespace could not be observed", ErrCurrentAbsenceUnavailable)
				}
			}
			if err := session.Check(); err != nil {
				_ = session.Close()
				return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: retired source parent binding changed", ErrCurrentAbsenceUnavailable)
			}
			if err := session.Close(); err != nil {
				return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: retired source parent close failed", ErrCurrentAbsenceUnavailable)
			}
		}
	}
	ended := time.Now().UTC()
	observation := CurrentAbsenceObservation{
		OperationID: intent.OperationID.String(), PlanID: intent.PlanID, CompletionID: completion.authority.observation.CompletionID,
		SearchScopeID: intent.SearchScopeID, FilesChecked: len(intent.Files), BytesRetired: intent.ContentBytes,
		ParentDirectoriesChecked: len(parents), ObservedAtStart: started, ObservedAtEnd: ended,
		Assurance: "same_invocation_two_pass_identity_bound_retired_name_absence_bracketed_non_atomic",
	}
	observation.AbsenceID = currentAbsenceID(observation)
	verified := &VerifiedCurrentAbsence{authority: &currentAbsenceAuthority{completion: completion, observation: observation}}
	if !verified.Verified() {
		return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: current retired-source absence observation is invalid", ErrCurrentAbsenceUnavailable)
	}
	return verified, verified.Observation(), nil
}

func (verified *VerifiedCompletion) Verified() bool {
	if verified == nil || verified.authority == nil || !validCompletionObservation(verified.authority.observation) {
		return false
	}
	if verified.authority.intent != nil {
		intent := verified.authority.intent
		return intent.Validate() == nil && intent.OperationID.String() == verified.authority.observation.OperationID &&
			intent.PlanID == verified.authority.observation.PlanID && intent.SearchScopeID == verified.authority.observation.SearchScopeID
	}
	return verified.authority.observation.RetainedTombstone
}

func (verified *VerifiedCompletion) Observation() CompletionObservation {
	if !verified.Verified() {
		return CompletionObservation{}
	}
	return verified.authority.observation
}

func (verified *VerifiedCompletion) Matches(operation OperationID, planID, variantID, materializeOperationID,
	materializePlanID, activationOperationID, activationPlanID, clientCompletionID, finalObjectIdentity string) bool {
	if !verified.Verified() {
		return false
	}
	value := verified.authority.observation
	return value.OperationID == operation.String() && value.PlanID == planID && value.MetafileVariantID == variantID &&
		value.MaterializeOperationID == materializeOperationID && value.MaterializePlanID == materializePlanID &&
		value.ActivationOperationID == activationOperationID && value.ActivationPlanID == activationPlanID &&
		value.ClientCompletionID == clientCompletionID && value.FinalObjectIdentity == finalObjectIdentity
}

func (verified *VerifiedCurrentAbsence) Verified() bool {
	if verified == nil || verified.authority == nil || verified.authority.completion == nil || !verified.authority.completion.Verified() {
		return false
	}
	value := verified.authority.observation
	completion := verified.authority.completion.Observation()
	return validCurrentAbsenceObservation(value) && value.OperationID == completion.OperationID && value.PlanID == completion.PlanID &&
		value.CompletionID == completion.CompletionID && value.SearchScopeID == completion.SearchScopeID &&
		value.FilesChecked == completion.FilesRetired && value.BytesRetired == completion.BytesRetired
}

func (verified *VerifiedCurrentAbsence) Observation() CurrentAbsenceObservation {
	if !verified.Verified() {
		return CurrentAbsenceObservation{}
	}
	return verified.authority.observation
}

func (verified *VerifiedCurrentAbsence) Matches(completion *VerifiedCompletion) bool {
	if !verified.Verified() || completion == nil || !completion.Verified() {
		return false
	}
	left, right := verified.authority.completion.Observation(), completion.Observation()
	return left.OperationID == right.OperationID && left.PlanID == right.PlanID && left.CompletionID == right.CompletionID
}

func completionObservationFromJournal(state executionJournalState) CompletionObservation {
	intent, complete := state.Intent, state.Complete
	return CompletionObservation{
		OperationID: intent.OperationID.String(), PlanID: intent.PlanID, IntentID: state.IntentID, CompletionID: state.CompleteID,
		SearchScopeID: intent.SearchScopeID, MetafileVariantID: intent.MetafileVariantID,
		MaterializeOperationID: intent.MaterializeOperationID, MaterializePlanID: intent.MaterializePlanID,
		ActivationOperationID: intent.ActivationOperationID, ActivationPlanID: intent.ActivationPlanID,
		ClientCompletionID: intent.ClientCompletionID, CurrentClientUseID: intent.CurrentClientUseID,
		SourceSelectionID: intent.SourceSelectionID, TargetRootIdentity: intent.TargetRootIdentity,
		FinalObjectIdentity: complete.FinalObjectIdentity, ClientSnapshotID: complete.ClientSnapshotID,
		FilesRetired: complete.FilesRetired, BytesRetired: complete.BytesRetired, RetainedTombstone: false,
		Assurance: "same_invocation_bound_canonical_terminal_source_retirement_journal_read_without_current_absence_inference",
	}
}

func completionObservationFromRetention(marker ExecutionRetentionIntent) CompletionObservation {
	return CompletionObservation{
		OperationID: marker.OperationID.String(), PlanID: marker.PlanID, IntentID: marker.IntentID, CompletionID: marker.CompletionID,
		SearchScopeID: marker.SearchScopeID, MetafileVariantID: marker.MetafileVariantID,
		MaterializeOperationID: marker.MaterializeOperationID, MaterializePlanID: marker.MaterializePlanID,
		ActivationOperationID: marker.ActivationOperationID, ActivationPlanID: marker.ActivationPlanID,
		ClientCompletionID: marker.ClientCompletionID, CurrentClientUseID: marker.CurrentClientUseID,
		SourceSelectionID: marker.SourceSelectionID, TargetRootIdentity: marker.TargetRootIdentity,
		FinalObjectIdentity: marker.FinalObjectIdentity, ClientSnapshotID: marker.ClientSnapshotID,
		FilesRetired: marker.FilesRetired, BytesRetired: marker.BytesRetired, RetainedTombstone: true,
		Assurance: "same_invocation_bound_canonical_source_retirement_retention_tombstone_read_without_current_absence_inference",
	}
}

func validCompletionObservation(value CompletionObservation) bool {
	operation, operationErr := ParseOperationID(value.OperationID)
	derived, deriveErr := executionOperationID(value.PlanID)
	target, targetErr := fsbind.ParseIdentity(value.TargetRootIdentity)
	final, finalErr := fsbind.ParseIdentity(value.FinalObjectIdentity)
	if operationErr != nil || deriveErr != nil || operation != derived || targetErr != nil || finalErr != nil || target.IsZero() || final.IsZero() ||
		!canonicalSHA256ID(value.IntentID) || !canonicalSHA256ID(value.CompletionID) || !canonicalSHA256ID(value.SearchScopeID) ||
		!canonicalSHA256ID(value.MetafileVariantID) || !canonicalSHA256ID(value.MaterializeOperationID) || !canonicalPlanID(value.MaterializePlanID) ||
		!canonicalSHA256ID(value.ActivationOperationID) || !canonicalPlanID(value.ActivationPlanID) || !canonicalSHA256ID(value.ClientCompletionID) ||
		!canonicalSHA256ID(value.CurrentClientUseID) || !canonicalSHA256ID(value.SourceSelectionID) || !canonicalSHA256ID(value.ClientSnapshotID) ||
		value.FilesRetired <= 0 || value.FilesRetired > hardExecutionMaxFiles || value.BytesRetired <= 0 || value.BytesRetired > hardExecutionMaxContent {
		return false
	}
	if value.RetainedTombstone {
		return value.Assurance == "same_invocation_bound_canonical_source_retirement_retention_tombstone_read_without_current_absence_inference"
	}
	return value.Assurance == "same_invocation_bound_canonical_terminal_source_retirement_journal_read_without_current_absence_inference"
}

func validCurrentAbsenceObservation(value CurrentAbsenceObservation) bool {
	if _, err := ParseOperationID(value.OperationID); err != nil || !canonicalSHA256ID(value.PlanID) || !canonicalSHA256ID(value.CompletionID) ||
		!canonicalSHA256ID(value.AbsenceID) || !canonicalSHA256ID(value.SearchScopeID) || value.FilesChecked <= 0 ||
		value.FilesChecked > hardExecutionMaxFiles || value.BytesRetired <= 0 || value.BytesRetired > hardExecutionMaxContent ||
		value.ParentDirectoriesChecked <= 0 || value.ParentDirectoriesChecked > value.FilesChecked || value.ObservedAtStart.IsZero() ||
		value.ObservedAtEnd.Before(value.ObservedAtStart) || value.Assurance != "same_invocation_two_pass_identity_bound_retired_name_absence_bracketed_non_atomic" {
		return false
	}
	return currentAbsenceID(value) == value.AbsenceID
}

func currentAbsenceID(value CurrentAbsenceObservation) string {
	parts := []string{value.OperationID, value.PlanID, value.CompletionID, value.SearchScopeID,
		strconv.Itoa(value.FilesChecked), strconv.FormatInt(value.BytesRetired, 10), strconv.Itoa(value.ParentDirectoriesChecked),
		value.ObservedAtStart.UTC().Format(time.RFC3339Nano), value.ObservedAtEnd.UTC().Format(time.RFC3339Nano)}
	digest := sha256.Sum256([]byte("ptctl-source-retirement-current-absence-v1\x00" + strings.Join(parts, "\x00")))
	return markerIDPrefix + hex.EncodeToString(digest[:])
}
