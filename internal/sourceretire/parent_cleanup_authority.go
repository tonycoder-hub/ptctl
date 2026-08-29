package sourceretire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

var (
	// ErrCurrentParentAbsenceUnavailable means the historical parent cleanup
	// could not be rebound to its original namespace for a current observation.
	// It does not imply that the canonical cleanup journal is corrupt.
	ErrCurrentParentAbsenceUnavailable = errors.New("current removed-parent absence is unavailable")
	// ErrRemovedParentPresent means a complete current observation found at
	// least one explicitly removed parent name again. It is a current-state
	// conflict, not evidence that the historical cleanup journal is corrupt.
	ErrRemovedParentPresent = errors.New("a removed parent name is currently present")
)

// ParentCleanupCompletionProofOptions selects one explicit terminal
// parent-cleanup operation. Reading the journal or retained tombstone is local
// and read-only.
type ParentCleanupCompletionProofOptions struct {
	TargetRoot            string
	OperationID           ParentCleanupOperationID
	ExpectedCleanupPlanID string
	Limits                ParentCleanupExecutionLimits
}

// ParentCleanupCompletionObservation is a public, non-authoritative view of
// one canonical terminal parent-cleanup journal or retained tombstone. It is
// historical evidence and does not assert that a removed parent remains absent.
type ParentCleanupCompletionObservation struct {
	OperationID            string `json:"operation_id"`
	CleanupPlanID          string `json:"cleanup_plan_id"`
	IntentID               string `json:"intent_id"`
	CompletionID           string `json:"completion_id"`
	RetirementOperationID  string `json:"retirement_operation_id"`
	RetirementPlanID       string `json:"retirement_plan_id"`
	RetirementCompletionID string `json:"retirement_completion_id"`
	SearchScopeID          string `json:"search_scope_id"`
	TargetRootIdentity     string `json:"target_root_identity"`
	ParentsRemoved         int    `json:"parents_removed"`
	RetiredFiles           int    `json:"retired_files"`
	RetainedTombstone      bool   `json:"retained_tombstone"`
	Assurance              string `json:"assurance"`
}

// ParentCleanupCurrentAbsenceObservation describes a same-invocation,
// two-pass observation of the exact removed parent names. Paths are
// deliberately absent from the public DTO.
type ParentCleanupCurrentAbsenceObservation struct {
	OperationID     string    `json:"operation_id"`
	CleanupPlanID   string    `json:"cleanup_plan_id"`
	CompletionID    string    `json:"completion_id"`
	AbsenceID       string    `json:"absence_id"`
	SearchScopeID   string    `json:"search_scope_id"`
	ParentsChecked  int       `json:"parents_checked"`
	RetiredFiles    int       `json:"retired_files"`
	ObservedAtStart time.Time `json:"observed_at_start"`
	ObservedAtEnd   time.Time `json:"observed_at_end"`
	Assurance       string    `json:"assurance"`
}

type parentCleanupCompletionAuthority struct {
	observation ParentCleanupCompletionObservation
	intent      *ParentCleanupIntent
}

// VerifiedParentCleanupCompletion is process-local authority that one
// canonical terminal parent-cleanup record was read from the bound target root.
// JSON cannot recreate this authority.
type VerifiedParentCleanupCompletion struct {
	authority *parentCleanupCompletionAuthority
}

type parentCleanupCurrentAbsenceAuthority struct {
	completion  *VerifiedParentCleanupCompletion
	observation ParentCleanupCurrentAbsenceObservation
}

// VerifiedParentCleanupCurrentAbsence is process-local authority for the
// bounded current absence observation. It remains sequential and non-atomic.
type VerifiedParentCleanupCurrentAbsence struct {
	authority *parentCleanupCurrentAbsenceAuthority
}

// VerifyParentCleanupCompletion reads either the live terminal journal or the
// exact retained tombstone. A retained tombstone deliberately carries no path
// authority, so it cannot produce VerifiedParentCleanupCurrentAbsence.
func VerifyParentCleanupCompletion(ctx context.Context, options ParentCleanupCompletionProofOptions) (*VerifiedParentCleanupCompletion, ParentCleanupCompletionObservation, error) {
	if options.TargetRoot == "" || options.Limits.Validate() != nil || !canonicalSHA256ID(options.ExpectedCleanupPlanID) {
		return nil, ParentCleanupCompletionObservation{}, fmt.Errorf("%w: parent-cleanup completion selector is invalid", ErrExecutionPolicy)
	}
	derived, deriveErr := ParentCleanupOperationIDForPlanID(options.ExpectedCleanupPlanID)
	parsed, parseErr := ParseParentCleanupOperationID(options.OperationID.String())
	if deriveErr != nil || parseErr != nil || parsed != options.OperationID || derived != options.OperationID {
		return nil, ParentCleanupCompletionObservation{}, fmt.Errorf("%w: parent-cleanup completion selector is invalid", ErrExecutionPolicy)
	}
	if err := ctx.Err(); err != nil {
		return nil, ParentCleanupCompletionObservation{}, err
	}
	target, targetInfo, err := fsbind.BindExisting(options.TargetRoot)
	if err != nil {
		return nil, ParentCleanupCompletionObservation{}, err
	}
	defer target.Close()
	subtree, err := openParentCleanupRetentionSubtree(target, options.OperationID)
	if err != nil {
		return nil, ParentCleanupCompletionObservation{}, err
	}
	defer subtree.Close()

	retention, err := loadParentCleanupRetentionState(ctx, subtree, options.OperationID, targetInfo.Identity)
	if err != nil {
		return nil, ParentCleanupCompletionObservation{}, err
	}
	var authority parentCleanupCompletionAuthority
	if retention.DirectoryPresent {
		if !retention.IntentPresent || !retention.CompletePresent {
			if auditErr := auditParentCleanupRetentionControl(ctx, subtree, retention, true); auditErr != nil {
				return nil, ParentCleanupCompletionObservation{}, auditErr
			}
			return nil, ParentCleanupCompletionObservation{}, fmt.Errorf("%w: parent-cleanup retention is not terminal", ErrExecutionPolicy)
		}
		if err := verifyParentCleanupRetentionTombstone(ctx, subtree, options.OperationID, targetInfo.Identity); err != nil {
			return nil, ParentCleanupCompletionObservation{}, err
		}
		if retention.Intent.CleanupPlanID != options.ExpectedCleanupPlanID {
			return nil, ParentCleanupCompletionObservation{}, fmt.Errorf("%w: parent-cleanup completion selector disagrees", ErrExecutionPolicy)
		}
		authority.observation = parentCleanupCompletionObservationFromRetention(retention.Intent)
	} else {
		journal, loadErr := loadParentCleanupJournalFromSubtree(ctx, target, subtree, options.Limits)
		if loadErr != nil {
			return nil, ParentCleanupCompletionObservation{}, loadErr
		}
		state := journal.state
		if state.Intent.CleanupPlanID != options.ExpectedCleanupPlanID || state.Intent.OperationID != options.OperationID || !state.CompletePresent {
			return nil, ParentCleanupCompletionObservation{}, fmt.Errorf("%w: parent-cleanup completion is unavailable or disagrees with the selector", ErrExecutionPolicy)
		}
		for _, present := range state.RemovedPresent {
			if !present {
				return nil, ParentCleanupCompletionObservation{}, fmt.Errorf("%w: parent cleanup is not terminal", ErrExecutionPolicy)
			}
		}
		scratchPath, _ := fsbind.PathFromComponents([]string{parentCleanupScratchDirectory})
		scratch, listErr := subtree.List(ctx, scratchPath, fsbind.ListLimits{MaxEntries: 2, MaxNameBytes: options.Limits.MaxPathBytes})
		if listErr != nil {
			return nil, ParentCleanupCompletionObservation{}, classifyExecutionBindingError(listErr)
		}
		if !scratch.Complete || len(scratch.Entries) != 0 {
			return nil, ParentCleanupCompletionObservation{}, fmt.Errorf("%w: terminal parent cleanup still has pending private state", ErrExecutionPolicy)
		}
		journalPath, _ := fsbind.PathFromComponents([]string{parentCleanupJournalDirectory})
		if err := subtree.CheckPaths(journalPath, scratchPath); err != nil {
			return nil, ParentCleanupCompletionObservation{}, classifyExecutionBindingError(err)
		}
		intent := state.Intent
		authority.intent = &intent
		authority.observation = parentCleanupCompletionObservationFromJournal(state)
	}
	if err := target.Check(); err != nil {
		return nil, ParentCleanupCompletionObservation{}, classifyExecutionBindingError(err)
	}
	verified := &VerifiedParentCleanupCompletion{authority: &authority}
	if !verified.Verified() {
		return nil, ParentCleanupCompletionObservation{}, fmt.Errorf("%w: terminal parent-cleanup completion is invalid", ErrExecutionIntegrity)
	}
	return verified, verified.Observation(), nil
}

// VerifyCurrentParentCleanupAbsence rebinds the live journal's exact removed
// parent names within an explicit root scope and observes every name absent
// twice. Retained tombstones cannot authorize this read because path details
// were intentionally pruned.
func VerifyCurrentParentCleanupAbsence(ctx context.Context, completion *VerifiedParentCleanupCompletion, searchRoots []string, allowNetwork bool) (*VerifiedParentCleanupCurrentAbsence, ParentCleanupCurrentAbsenceObservation, error) {
	if completion == nil || !completion.Verified() || completion.authority.intent == nil {
		return nil, ParentCleanupCurrentAbsenceObservation{}, fmt.Errorf("%w: live parent-cleanup path authority is unavailable", ErrCurrentParentAbsenceUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, ParentCleanupCurrentAbsenceObservation{}, err
	}
	started := time.Now().UTC()
	roots, scopeID, err := normalizeExecutionRoots(ctx, searchRoots, allowNetwork)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ParentCleanupCurrentAbsenceObservation{}, ctxErr
		}
		return nil, ParentCleanupCurrentAbsenceObservation{}, fmt.Errorf("%w: parent-cleanup search-root scope could not be rebound", ErrCurrentParentAbsenceUnavailable)
	}
	intent := *completion.authority.intent
	if scopeID != intent.SearchScopeID || validateParentCleanupIntentScope(intent, roots) != nil {
		return nil, ParentCleanupCurrentAbsenceObservation{}, fmt.Errorf("%w: parent-cleanup source scope disagrees with the terminal journal", ErrCurrentParentAbsenceUnavailable)
	}
	for pass := 0; pass < 2; pass++ {
		for _, directory := range intent.Directories {
			if err := observeRemovedParentNameAbsent(ctx, directory, roots); err != nil {
				return nil, ParentCleanupCurrentAbsenceObservation{}, err
			}
		}
	}
	ended := time.Now().UTC()
	observation := ParentCleanupCurrentAbsenceObservation{
		OperationID: intent.OperationID.String(), CleanupPlanID: intent.CleanupPlanID,
		CompletionID: completion.authority.observation.CompletionID, SearchScopeID: intent.SearchScopeID,
		ParentsChecked: len(intent.Directories), RetiredFiles: parentCleanupRetiredFiles(intent.Directories),
		ObservedAtStart: started, ObservedAtEnd: ended,
		Assurance: "same_invocation_two_pass_identity_bound_removed_parent_name_absence_bracketed_non_atomic",
	}
	observation.AbsenceID = parentCleanupCurrentAbsenceID(observation)
	verified := &VerifiedParentCleanupCurrentAbsence{authority: &parentCleanupCurrentAbsenceAuthority{completion: completion, observation: observation}}
	if !verified.Verified() {
		return nil, ParentCleanupCurrentAbsenceObservation{}, fmt.Errorf("%w: current removed-parent absence observation is invalid", ErrCurrentParentAbsenceUnavailable)
	}
	return verified, verified.Observation(), nil
}

// BindCurrentRetiredNameAbsenceFromParentCleanup combines three process-local
// capabilities without another filesystem read: the terminal retirement, its
// exact descendant parent-cleanup lineage, and the cleanup's current two-pass
// parent-name absence. An absent parent name necessarily makes every retired
// child name recorded under that parent absent, but the observation remains
// sequential and non-atomic.
func BindCurrentRetiredNameAbsenceFromParentCleanup(retirement *VerifiedCompletion, cleanup *VerifiedParentCleanupCompletion, absence *VerifiedParentCleanupCurrentAbsence) (*VerifiedCurrentAbsence, CurrentAbsenceObservation, error) {
	if retirement == nil || !retirement.Verified() || cleanup == nil || !cleanup.Verified() || absence == nil || !absence.Verified() ||
		!absence.Matches(cleanup) {
		return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: parent-cleanup absence cannot be bound to the retirement", ErrCurrentAbsenceUnavailable)
	}
	retired, cleaned, current := retirement.Observation(), cleanup.Observation(), absence.Observation()
	if cleaned.RetirementOperationID != retired.OperationID || cleaned.RetirementPlanID != retired.PlanID ||
		cleaned.RetirementCompletionID != retired.CompletionID || cleaned.SearchScopeID != retired.SearchScopeID ||
		cleaned.RetiredFiles != retired.FilesRetired || current.ParentsChecked != cleaned.ParentsRemoved ||
		current.RetiredFiles != retired.FilesRetired {
		return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: parent-cleanup absence lineage disagrees with the retirement", ErrCurrentAbsenceUnavailable)
	}
	observation := CurrentAbsenceObservation{
		OperationID: retired.OperationID, PlanID: retired.PlanID, CompletionID: retired.CompletionID,
		SearchScopeID: retired.SearchScopeID, FilesChecked: retired.FilesRetired, BytesRetired: retired.BytesRetired,
		ParentDirectoriesChecked: current.ParentsChecked, ObservedAtStart: current.ObservedAtStart, ObservedAtEnd: current.ObservedAtEnd,
		Assurance: "same_invocation_two_pass_identity_bound_removed_parent_absence_implies_retired_name_absence_bracketed_non_atomic",
	}
	observation.AbsenceID = currentAbsenceID(observation)
	verified := &VerifiedCurrentAbsence{authority: &currentAbsenceAuthority{completion: retirement, observation: observation}}
	if !verified.Verified() {
		return nil, CurrentAbsenceObservation{}, fmt.Errorf("%w: parent-cleanup-derived retired-name absence is invalid", ErrCurrentAbsenceUnavailable)
	}
	return verified, verified.Observation(), nil
}

func observeRemovedParentNameAbsent(ctx context.Context, directory ParentCleanupIntentDirectory, roots []string) error {
	if err := validateCleanupParentPath(directory.ParentPath, directory.ParentPathRef, roots); err != nil {
		return fmt.Errorf("%w: removed parent path is outside the explicit scope", ErrCurrentParentAbsenceUnavailable)
	}
	grandparent, name := filepath.Dir(directory.ParentPath), filepath.Base(directory.ParentPath)
	session, _, err := fsbind.BindExisting(grandparent)
	if err != nil {
		return fmt.Errorf("%w: removed parent namespace could not be rebound", ErrCurrentParentAbsenceUnavailable)
	}
	closed := false
	defer func() {
		if !closed {
			_ = session.Close()
		}
	}()
	_, inspectErr := session.InspectRoot(ctx, name)
	switch {
	case inspectErr == nil:
		return fmt.Errorf("%w", ErrRemovedParentPresent)
	case errors.Is(inspectErr, fsbind.ErrNotFound):
	case errors.Is(inspectErr, context.Canceled), errors.Is(inspectErr, context.DeadlineExceeded):
		return inspectErr
	default:
		return fmt.Errorf("%w: removed parent namespace could not be observed", ErrCurrentParentAbsenceUnavailable)
	}
	if err := session.Check(); err != nil {
		return fmt.Errorf("%w: removed parent namespace binding changed", ErrCurrentParentAbsenceUnavailable)
	}
	closeErr := session.Close()
	closed = true
	if closeErr != nil {
		return fmt.Errorf("%w: removed parent namespace close failed", ErrCurrentParentAbsenceUnavailable)
	}
	return nil
}

func (verified *VerifiedParentCleanupCompletion) Verified() bool {
	if verified == nil || verified.authority == nil || !validParentCleanupCompletionObservation(verified.authority.observation) {
		return false
	}
	if verified.authority.intent != nil {
		intent := verified.authority.intent
		return intent.Validate() == nil && intent.OperationID.String() == verified.authority.observation.OperationID &&
			intent.CleanupPlanID == verified.authority.observation.CleanupPlanID && intent.SearchScopeID == verified.authority.observation.SearchScopeID
	}
	return verified.authority.observation.RetainedTombstone
}

func (verified *VerifiedParentCleanupCompletion) Observation() ParentCleanupCompletionObservation {
	if !verified.Verified() {
		return ParentCleanupCompletionObservation{}
	}
	return verified.authority.observation
}

func (verified *VerifiedParentCleanupCompletion) Matches(operation ParentCleanupOperationID, planID string, retirement *VerifiedCompletion) bool {
	if !verified.Verified() || retirement == nil || !retirement.Verified() {
		return false
	}
	value, retired := verified.authority.observation, retirement.Observation()
	return value.OperationID == operation.String() && value.CleanupPlanID == planID &&
		value.RetirementOperationID == retired.OperationID && value.RetirementPlanID == retired.PlanID &&
		value.RetirementCompletionID == retired.CompletionID && value.SearchScopeID == retired.SearchScopeID
}

func (verified *VerifiedParentCleanupCurrentAbsence) Verified() bool {
	if verified == nil || verified.authority == nil || verified.authority.completion == nil || !verified.authority.completion.Verified() {
		return false
	}
	value, completion := verified.authority.observation, verified.authority.completion.Observation()
	return validParentCleanupCurrentAbsenceObservation(value) && value.OperationID == completion.OperationID &&
		value.CleanupPlanID == completion.CleanupPlanID && value.CompletionID == completion.CompletionID &&
		value.SearchScopeID == completion.SearchScopeID && value.ParentsChecked == completion.ParentsRemoved &&
		value.RetiredFiles == completion.RetiredFiles
}

func (verified *VerifiedParentCleanupCurrentAbsence) Observation() ParentCleanupCurrentAbsenceObservation {
	if !verified.Verified() {
		return ParentCleanupCurrentAbsenceObservation{}
	}
	return verified.authority.observation
}

func (verified *VerifiedParentCleanupCurrentAbsence) Matches(completion *VerifiedParentCleanupCompletion) bool {
	if !verified.Verified() || completion == nil || !completion.Verified() {
		return false
	}
	left, right := verified.authority.completion.Observation(), completion.Observation()
	return left.OperationID == right.OperationID && left.CleanupPlanID == right.CleanupPlanID && left.CompletionID == right.CompletionID
}

func parentCleanupCompletionObservationFromJournal(state parentCleanupJournalState) ParentCleanupCompletionObservation {
	intent := state.Intent
	return ParentCleanupCompletionObservation{
		OperationID: intent.OperationID.String(), CleanupPlanID: intent.CleanupPlanID, IntentID: state.IntentID,
		CompletionID: state.CompleteID, RetirementOperationID: intent.RetirementOperationID.String(),
		RetirementPlanID: intent.RetirementPlanID, RetirementCompletionID: intent.RetirementCompletionID,
		SearchScopeID: intent.SearchScopeID, TargetRootIdentity: intent.TargetRootIdentity,
		ParentsRemoved: state.Complete.ParentsRemoved, RetiredFiles: parentCleanupRetiredFiles(intent.Directories),
		RetainedTombstone: false,
		Assurance:         "same_invocation_bound_canonical_terminal_parent_cleanup_journal_read_without_current_absence_inference",
	}
}

func parentCleanupCompletionObservationFromRetention(marker ParentCleanupRetentionIntent) ParentCleanupCompletionObservation {
	return ParentCleanupCompletionObservation{
		OperationID: marker.OperationID.String(), CleanupPlanID: marker.CleanupPlanID, IntentID: marker.IntentID,
		CompletionID: marker.CompletionID, RetirementOperationID: marker.RetirementOperationID.String(),
		RetirementPlanID: marker.RetirementPlanID, RetirementCompletionID: marker.RetirementCompletionID,
		SearchScopeID: marker.SearchScopeID, TargetRootIdentity: marker.TargetRootIdentity,
		ParentsRemoved: marker.ParentsRemoved, RetiredFiles: marker.RetiredFiles, RetainedTombstone: true,
		Assurance: "same_invocation_bound_canonical_parent_cleanup_retention_tombstone_read_without_current_absence_inference",
	}
}

func parentCleanupRetiredFiles(directories []ParentCleanupIntentDirectory) int {
	total := 0
	for _, directory := range directories {
		if directory.RetiredFiles <= 0 || total > hardExecutionMaxFiles-directory.RetiredFiles {
			return 0
		}
		total += directory.RetiredFiles
	}
	return total
}

func validParentCleanupCompletionObservation(value ParentCleanupCompletionObservation) bool {
	operation, operationErr := ParseParentCleanupOperationID(value.OperationID)
	derived, deriveErr := ParentCleanupOperationIDForPlanID(value.CleanupPlanID)
	retirement, retirementErr := ParseOperationID(value.RetirementOperationID)
	expectedRetirement, retirementDeriveErr := executionOperationID(value.RetirementPlanID)
	target, targetErr := fsbind.ParseIdentity(value.TargetRootIdentity)
	if operationErr != nil || deriveErr != nil || operation != derived || retirementErr != nil || retirementDeriveErr != nil ||
		retirement != expectedRetirement || targetErr != nil || target.IsZero() || !canonicalSHA256ID(value.IntentID) ||
		!canonicalSHA256ID(value.CompletionID) || !canonicalSHA256ID(value.RetirementCompletionID) || !canonicalSHA256ID(value.SearchScopeID) ||
		value.ParentsRemoved <= 0 || value.ParentsRemoved > hardParentCleanupExecutionMaxParents ||
		value.RetiredFiles <= 0 || value.RetiredFiles > hardExecutionMaxFiles {
		return false
	}
	if value.RetainedTombstone {
		return value.Assurance == "same_invocation_bound_canonical_parent_cleanup_retention_tombstone_read_without_current_absence_inference"
	}
	return value.Assurance == "same_invocation_bound_canonical_terminal_parent_cleanup_journal_read_without_current_absence_inference"
}

func validParentCleanupCurrentAbsenceObservation(value ParentCleanupCurrentAbsenceObservation) bool {
	if _, err := ParseParentCleanupOperationID(value.OperationID); err != nil || !canonicalSHA256ID(value.CleanupPlanID) ||
		!canonicalSHA256ID(value.CompletionID) || !canonicalSHA256ID(value.AbsenceID) || !canonicalSHA256ID(value.SearchScopeID) ||
		value.ParentsChecked <= 0 || value.ParentsChecked > hardParentCleanupExecutionMaxParents ||
		value.RetiredFiles <= 0 || value.RetiredFiles > hardExecutionMaxFiles || value.ObservedAtStart.IsZero() ||
		value.ObservedAtEnd.Before(value.ObservedAtStart) ||
		value.Assurance != "same_invocation_two_pass_identity_bound_removed_parent_name_absence_bracketed_non_atomic" {
		return false
	}
	return parentCleanupCurrentAbsenceID(value) == value.AbsenceID
}

func parentCleanupCurrentAbsenceID(value ParentCleanupCurrentAbsenceObservation) string {
	parts := []string{value.OperationID, value.CleanupPlanID, value.CompletionID, value.SearchScopeID,
		strconv.Itoa(value.ParentsChecked), strconv.Itoa(value.RetiredFiles),
		value.ObservedAtStart.UTC().Format(time.RFC3339Nano), value.ObservedAtEnd.UTC().Format(time.RFC3339Nano)}
	digest := sha256.Sum256([]byte("ptctl-parent-cleanup-current-absence-v1\x00" + strings.Join(parts, "\x00")))
	return markerIDPrefix + hex.EncodeToString(digest[:])
}
