package sourceretire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	ParentCleanupPlanSchemaV1 = "ptctl.source-retirement-parent-cleanup-plan/v1"

	ParentCleanupOutcomeEligible       = "eligible_for_separate_cleanup_review"
	ParentCleanupOutcomeNothingToClean = "nothing_to_clean"
	ParentCleanupOutcomeBlocked        = "blocked"
	ParentCleanupOutcomeIncomplete     = "incomplete"
	ParentCleanupOutcomeIntegrity      = "integrity_failed"

	parentCleanupPlanDomain = "ptctl-source-retirement-parent-cleanup-plan-v1\x00"
	parentCleanupPathDomain = "ptctl-source-retirement-parent-cleanup-path-v1\x00"

	defaultParentCleanupMaxParents       = 10_000
	hardParentCleanupMaxParents          = hardExecutionMaxFiles
	defaultParentCleanupMaxPathBytes     = int64(8 << 20)
	hardParentCleanupMaxPathBytes        = int64(64 << 20)
	defaultParentCleanupMaxEntryNameByte = int64(64 << 10)
)

type ParentCleanupLimits struct {
	MaxParents        int   `json:"max_parents"`
	MaxPathBytes      int64 `json:"max_path_bytes"`
	MaxEntryNameBytes int64 `json:"max_entry_name_bytes"`
}

func DefaultParentCleanupLimits() ParentCleanupLimits {
	return ParentCleanupLimits{
		MaxParents:        defaultParentCleanupMaxParents,
		MaxPathBytes:      defaultParentCleanupMaxPathBytes,
		MaxEntryNameBytes: defaultParentCleanupMaxEntryNameByte,
	}
}

func (limits ParentCleanupLimits) Validate() error {
	if limits.MaxParents <= 0 || limits.MaxParents > hardParentCleanupMaxParents ||
		limits.MaxPathBytes <= 0 || limits.MaxPathBytes > hardParentCleanupMaxPathBytes ||
		(fsbind.ListLimits{MaxEntries: 1, MaxNameBytes: limits.MaxEntryNameBytes}).Validate() != nil {
		return fmt.Errorf("%w: parent-cleanup limits are invalid", ErrExecutionPolicy)
	}
	return nil
}

type ParentCleanupOptions struct {
	TargetRoot        string
	OperationID       OperationID
	ExpectedPlanID    string
	SearchRoots       []string
	AllowNetwork      bool
	Limits            ParentCleanupLimits
	RetirementLimits  ExecutionLimits
	ShowAbsolutePaths bool
}

type ParentCleanupDirectory struct {
	Sequence          int    `json:"sequence"`
	ParentPathRef     string `json:"parent_path_ref"`
	ParentPath        string `json:"parent_path,omitempty"`
	ParentIdentity    string `json:"parent_identity"`
	RetiredFiles      int    `json:"retired_files"`
	Status            string `json:"status"`
	ObservationPasses int    `json:"observation_passes"`
	EntriesObserved   int    `json:"entries_observed"`
}

type ParentCleanupPlan struct {
	Schema                 string                   `json:"schema"`
	ID                     string                   `json:"id,omitempty"`
	CleanupAuthority       string                   `json:"cleanup_authority"`
	RetirementOperationID  string                   `json:"retirement_operation_id"`
	RetirementPlanID       string                   `json:"retirement_plan_id"`
	RetirementCompletionID string                   `json:"retirement_completion_id"`
	CurrentAbsenceID       string                   `json:"current_absence_id"`
	SearchScopeID          string                   `json:"search_scope_id"`
	RetiredFiles           int                      `json:"retired_files"`
	ParentsConsidered      int                      `json:"parents_considered"`
	CandidateParents       int                      `json:"candidate_parents"`
	RetainedParents        int                      `json:"retained_parents"`
	ProtectedRoots         int                      `json:"protected_roots"`
	AbsolutePathsShown     bool                     `json:"absolute_paths_shown"`
	Directories            []ParentCleanupDirectory `json:"directories"`
	EvidenceBasis          []string                 `json:"evidence_basis"`
}

type ParentCleanupUsage struct {
	ParentsConsidered int   `json:"parents_considered"`
	ParentPathBytes   int64 `json:"parent_path_bytes"`
	DirectoryReads    int   `json:"directory_reads"`
	EntriesObserved   int   `json:"entries_observed"`
	EntryNameBytes    int64 `json:"entry_name_bytes"`
}

type parentCleanupState struct {
	path     string
	identity string
	files    int
}

type verifiedParentCleanupAuthority struct {
	planID                 string
	targetRootIdentity     string
	retirementOperationID  OperationID
	retirementPlanID       string
	retirementCompletionID string
	searchScopeID          string
	candidates             []parentCleanupState
}

type ParentCleanupReport struct {
	Outcome               string              `json:"outcome"`
	Effect                []string            `json:"effect"`
	WritesPerformed       int                 `json:"writes_performed"`
	DeletionPerformed     bool                `json:"deletion_performed"`
	Plan                  ParentCleanupPlan   `json:"plan"`
	Limits                ParentCleanupLimits `json:"limits"`
	Used                  ParentCleanupUsage  `json:"used"`
	ParentObservedAtStart time.Time           `json:"parent_observed_at_start,omitempty"`
	ParentObservedAtEnd   time.Time           `json:"parent_observed_at_end,omitempty"`
	Blockers              []Finding           `json:"blockers"`
	Issues                []Finding           `json:"issues"`
	Warnings              []string            `json:"warnings"`
	authority             *verifiedParentCleanupAuthority
}

func newParentCleanupReport(options ParentCleanupOptions) ParentCleanupReport {
	return ParentCleanupReport{
		Outcome: ParentCleanupOutcomeIncomplete,
		Effect:  []string{},
		Plan: ParentCleanupPlan{
			Schema:                ParentCleanupPlanSchemaV1,
			CleanupAuthority:      "none",
			RetirementOperationID: options.OperationID.String(),
			RetirementPlanID:      options.ExpectedPlanID,
			Directories:           []ParentCleanupDirectory{},
			EvidenceBasis:         []string{},
		},
		Limits:   options.Limits,
		Blockers: []Finding{},
		Issues:   []Finding{},
		Warnings: []string{
			"the plan is zero-write review evidence and grants no directory-deletion authority",
			"only exact immediate parents from the live terminal retirement journal are considered; search roots and higher ancestors are never cleanup candidates",
			"empty-directory observations are identity-bound and bracketed non-atomic; another process may change a parent after the final read",
			"parent path and filesystem identity references are stable pseudonyms and may be dictionary-guessable; they are not anonymization",
			"pruning the source-retirement journal intentionally removes the path authority required by this planner",
		},
	}
}

// BuildParentCleanupPlan reads one exact terminal source-retirement journal,
// re-proves every retired name absent inside the same explicit root scope, and
// identifies only immediate parent directories that are still the journaled
// objects and are observed empty twice. It never opens a directory for write,
// removes a name, or grants execution authority.
func BuildParentCleanupPlan(ctx context.Context, options ParentCleanupOptions) (ParentCleanupReport, error) {
	report := newParentCleanupReport(options)
	if options.Limits.Validate() != nil || options.RetirementLimits.Validate() != nil || options.TargetRoot == "" ||
		!canonicalSHA256ID(options.ExpectedPlanID) || len(options.SearchRoots) == 0 {
		report.Outcome = ParentCleanupOutcomeBlocked
		report.Blockers = append(report.Blockers, Finding{Code: "cleanup.selector_invalid", Message: "the parent-cleanup selector or limits are invalid"})
		report.finalizeParentCleanup()
		return report, nil
	}
	derived, deriveErr := executionOperationID(options.ExpectedPlanID)
	parsed, parseErr := ParseOperationID(options.OperationID.String())
	if deriveErr != nil || parseErr != nil || parsed != options.OperationID || derived != options.OperationID {
		report.Outcome = ParentCleanupOutcomeBlocked
		report.Blockers = append(report.Blockers, Finding{Code: "cleanup.selector_invalid", Message: "the parent-cleanup operation and retirement plan selectors disagree"})
		report.finalizeParentCleanup()
		return report, nil
	}
	if err := ctx.Err(); err != nil {
		report.Issues = append(report.Issues, Finding{Code: "cleanup.context_cancelled", Message: "parent-cleanup planning was interrupted"})
		report.finalizeParentCleanup()
		return report, err
	}

	report.Effect = append(report.Effect, "read_private_source_retirement_operation_state")
	completion, completionObservation, err := VerifyCompletion(ctx, CompletionProofOptions{
		TargetRoot: options.TargetRoot, OperationID: options.OperationID,
		ExpectedPlanID: options.ExpectedPlanID, Limits: options.RetirementLimits,
	})
	if err != nil {
		return failParentCleanupCompletion(&report, err)
	}
	report.Plan.RetirementCompletionID = completionObservation.CompletionID
	report.Plan.SearchScopeID = completionObservation.SearchScopeID
	report.Plan.RetiredFiles = completionObservation.FilesRetired
	if completion == nil || !completion.Verified() || completion.authority == nil || completion.authority.intent == nil {
		report.Outcome = ParentCleanupOutcomeBlocked
		report.Blockers = append(report.Blockers, Finding{Code: "cleanup.live_path_authority_unavailable", Message: "the selected terminal retirement state no longer carries live parent-path authority"})
		report.finalizeParentCleanup()
		return report, nil
	}
	parents, parentPathBytes, parentsConsidered, budgetExceeded, parentErr := collectParentCleanupStates(completion.authority.intent.Files, options.Limits)
	report.Used.ParentsConsidered = parentsConsidered
	report.Used.ParentPathBytes = parentPathBytes
	if parentErr != nil {
		report.Outcome = ParentCleanupOutcomeIntegrity
		report.Blockers = append(report.Blockers, Finding{Code: "cleanup.parent_identity_inconsistent", Message: "the retirement journal records conflicting identities for one parent"})
		report.finalizeParentCleanup()
		return report, parentErr
	}
	if len(parents) == 0 || budgetExceeded {
		report.Blockers = append(report.Blockers, Finding{Code: "cleanup.parent_budget_exhausted", Message: "the terminal retirement parent set exceeds the cleanup-plan budget"})
		report.finalizeParentCleanup()
		return report, nil
	}

	report.Effect = append(report.Effect, "read_retired_source_name_absence")
	absence, absenceObservation, err := VerifyCurrentAbsence(ctx, completion, options.SearchRoots, options.AllowNetwork)
	if err != nil {
		switch {
		case errors.Is(err, ErrRetiredNamePresent):
			report.Outcome = ParentCleanupOutcomeBlocked
			report.Blockers = append(report.Blockers, Finding{Code: "cleanup.retired_name_present", Message: "an exact retired source name is currently present"})
			report.finalizeParentCleanup()
			return report, nil
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			report.Issues = append(report.Issues, Finding{Code: "cleanup.context_cancelled", Message: "retired-name absence observation was interrupted"})
			report.finalizeParentCleanup()
			return report, err
		case errors.Is(err, ErrExecutionIntegrity):
			report.Outcome = ParentCleanupOutcomeIntegrity
			report.Blockers = append(report.Blockers, Finding{Code: "cleanup.retirement_integrity_failed", Message: "the source-retirement authority failed integrity validation"})
			report.finalizeParentCleanup()
			return report, err
		default:
			report.Issues = append(report.Issues, Finding{Code: "cleanup.current_absence_incomplete", Message: "current retired-name absence could not be proved"})
			report.finalizeParentCleanup()
			return report, err
		}
	}
	if absence == nil || !absence.Verified() || !absence.Matches(completion) {
		report.Outcome = ParentCleanupOutcomeIntegrity
		report.Blockers = append(report.Blockers, Finding{Code: "cleanup.absence_authority_mismatch", Message: "the current absence authority does not match the terminal retirement"})
		report.finalizeParentCleanup()
		return report, fmt.Errorf("%w: current absence authority mismatch", ErrExecutionIntegrity)
	}
	report.Plan.CurrentAbsenceID = absenceObservation.AbsenceID

	roots, scopeID, err := normalizeExecutionRoots(ctx, options.SearchRoots, options.AllowNetwork)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			report.Issues = append(report.Issues, Finding{Code: "cleanup.context_cancelled", Message: "source search-root rebinding was interrupted"})
			report.finalizeParentCleanup()
			return report, err
		}
		report.Issues = append(report.Issues, Finding{Code: "cleanup.root_scope_reobserve_failed", Message: "the source search-root scope could not be rebound for parent observation"})
		report.finalizeParentCleanup()
		return report, err
	}
	if scopeID != report.Plan.SearchScopeID {
		report.Outcome = ParentCleanupOutcomeIntegrity
		report.Blockers = append(report.Blockers, Finding{Code: "cleanup.root_scope_changed", Message: "the rebound search-root scope disagrees with the terminal retirement"})
		report.finalizeParentCleanup()
		return report, fmt.Errorf("%w: source search-root scope changed", ErrExecutionIntegrity)
	}

	report.ParentObservedAtStart = time.Now().UTC()
	report.Effect = append(report.Effect, "read_retired_source_parent_namespaces")
	emptyCandidates := make([]int, 0, len(parents))
	for sequence, parent := range parents {
		if err := ctx.Err(); err != nil {
			report.Issues = append(report.Issues, Finding{Code: "cleanup.context_cancelled", Message: "parent namespace observation was interrupted"})
			report.finalizeParentCleanup()
			return report, err
		}
		directory := ParentCleanupDirectory{
			Sequence: sequence, ParentPathRef: parentCleanupPathRef(parent.path), ParentIdentity: parent.identity,
			RetiredFiles: parent.files,
		}
		if options.ShowAbsolutePaths {
			directory.ParentPath = parent.path
		}
		if exactCleanupRoot(roots, parent.path) {
			directory.Status = "protected_search_root"
			report.Plan.ProtectedRoots++
			report.Plan.Directories = append(report.Plan.Directories, directory)
			continue
		}
		empty, used, nameBytes, observeErr := observeRetiredParentEmpty(ctx, parent.path, parent.identity, options.Limits.MaxEntryNameBytes)
		report.Used.DirectoryReads++
		report.Used.EntriesObserved += used
		report.Used.EntryNameBytes += nameBytes
		directory.ObservationPasses = 1
		directory.EntriesObserved = used
		if observeErr != nil {
			directory.Status = "observation_incomplete"
			report.Plan.Directories = append(report.Plan.Directories, directory)
			if errors.Is(observeErr, context.Canceled) || errors.Is(observeErr, context.DeadlineExceeded) {
				report.Issues = append(report.Issues, Finding{Code: "cleanup.context_cancelled", Message: "parent namespace observation was interrupted"})
			} else {
				report.Issues = append(report.Issues, Finding{Code: "cleanup.parent_observation_incomplete", Message: "an exact retired-source parent could not be safely observed"})
			}
			report.finalizeParentCleanup()
			return report, observeErr
		}
		if !empty {
			directory.Status = "retained_nonempty"
			report.Plan.RetainedParents++
		} else {
			directory.Status = "empty_first_observation"
			emptyCandidates = append(emptyCandidates, len(report.Plan.Directories))
		}
		report.Plan.Directories = append(report.Plan.Directories, directory)
	}

	for _, index := range emptyCandidates {
		directory := &report.Plan.Directories[index]
		parent := parents[directory.Sequence]
		if parentCleanupObservationHook != nil {
			parentCleanupObservationHook("before_second_observation", parent.path)
		}
		empty, used, nameBytes, observeErr := observeRetiredParentEmpty(ctx, parent.path, parent.identity, options.Limits.MaxEntryNameBytes)
		report.Used.DirectoryReads++
		report.Used.EntriesObserved += used
		report.Used.EntryNameBytes += nameBytes
		directory.ObservationPasses = 2
		directory.EntriesObserved += used
		if observeErr != nil || !empty {
			if errors.Is(observeErr, context.Canceled) || errors.Is(observeErr, context.DeadlineExceeded) {
				directory.Status = "observation_incomplete"
				report.Issues = append(report.Issues, Finding{Code: "cleanup.context_cancelled", Message: "the second parent namespace observation was interrupted"})
			} else {
				directory.Status = "unstable"
				report.Issues = append(report.Issues, Finding{Code: "cleanup.parent_unstable", Message: "an empty parent changed across the two-pass observation"})
			}
			report.finalizeParentCleanup()
			if observeErr != nil {
				return report, observeErr
			}
			return report, nil
		}
		directory.Status = "empty_stable_candidate"
		report.Plan.CandidateParents++
	}
	report.ParentObservedAtEnd = time.Now().UTC()
	report.Plan.ParentsConsidered = len(report.Plan.Directories)
	report.Plan.AbsolutePathsShown = options.ShowAbsolutePaths
	report.Plan.EvidenceBasis = []string{
		"canonical_terminal_source_retirement_journal_read",
		"same_invocation_two_pass_identity_bound_retired_name_absence",
		"same_invocation_two_pass_identity_bound_empty_immediate_parent_observation",
		"search_roots_and_ancestors_excluded",
		"bracketed_non_atomic",
	}
	report.Plan.ID, err = parentCleanupPlanID(report.Plan)
	if err != nil || report.Plan.Validate() != nil {
		report.Outcome = ParentCleanupOutcomeIntegrity
		report.Blockers = append(report.Blockers, Finding{Code: "cleanup.plan_canonicalization_failed", Message: "the parent-cleanup plan could not be canonicalized"})
		report.Plan.ID = ""
		report.finalizeParentCleanup()
		return report, fmt.Errorf("%w: parent-cleanup plan canonicalization failed", ErrExecutionIntegrity)
	}
	if report.Plan.CandidateParents > 0 {
		report.Outcome = ParentCleanupOutcomeEligible
		candidates := make([]parentCleanupState, 0, report.Plan.CandidateParents)
		for _, directory := range report.Plan.Directories {
			if directory.Status == "empty_stable_candidate" {
				candidates = append(candidates, parents[directory.Sequence])
			}
		}
		report.authority = &verifiedParentCleanupAuthority{
			planID: report.Plan.ID, targetRootIdentity: completion.authority.intent.TargetRootIdentity,
			retirementOperationID: options.OperationID,
			retirementPlanID:      options.ExpectedPlanID, retirementCompletionID: report.Plan.RetirementCompletionID,
			searchScopeID: report.Plan.SearchScopeID, candidates: candidates,
		}
	} else {
		report.Outcome = ParentCleanupOutcomeNothingToClean
	}
	report.finalizeParentCleanup()
	return report, nil
}

func collectParentCleanupStates(files []IntentFile, limits ParentCleanupLimits) ([]parentCleanupState, int64, int, bool, error) {
	parentsByPath := make(map[string]parentCleanupState)
	pathsByIdentity := make(map[string]string)
	var pathBytes int64
	for _, file := range files {
		state, exists := parentsByPath[file.ParentPath]
		if exists && state.identity != file.ParentIdentity {
			return nil, 0, len(parentsByPath), false, fmt.Errorf("%w: parent identity is inconsistent", ErrExecutionIntegrity)
		}
		if !exists {
			if previous, duplicate := pathsByIdentity[file.ParentIdentity]; duplicate && previous != file.ParentPath {
				return nil, 0, len(parentsByPath), false, fmt.Errorf("%w: parent identity is reused by multiple paths", ErrExecutionIntegrity)
			}
			if len(parentsByPath) >= limits.MaxParents {
				return nil, pathBytes, len(parentsByPath) + 1, true, nil
			}
			pathBytes += int64(len(file.ParentPath))
			if pathBytes < 0 {
				return nil, 0, len(parentsByPath) + 1, false, fmt.Errorf("%w: parent path byte total overflowed", ErrExecutionIntegrity)
			}
			if pathBytes > limits.MaxPathBytes {
				return nil, pathBytes, len(parentsByPath) + 1, true, nil
			}
		}
		state.path, state.identity, state.files = file.ParentPath, file.ParentIdentity, state.files+1
		parentsByPath[file.ParentPath] = state
		pathsByIdentity[file.ParentIdentity] = file.ParentPath
	}
	parents := make([]parentCleanupState, 0, len(parentsByPath))
	for _, state := range parentsByPath {
		parents = append(parents, state)
	}
	sort.Slice(parents, func(i, j int) bool { return parents[i].path < parents[j].path })
	return parents, pathBytes, len(parents), false, nil
}

var parentCleanupObservationHook func(stage, parent string)

func observeRetiredParentEmpty(ctx context.Context, parent, expectedIdentity string, maxNameBytes int64) (bool, int, int64, error) {
	if parentCleanupObservationHook != nil {
		parentCleanupObservationHook("before_parent_open", parent)
	}
	session, _, err := fsbind.BindExisting(parent)
	if err != nil {
		return false, 0, 0, err
	}
	if session.Info().Identity.String() != expectedIdentity {
		_ = session.Close()
		return false, 0, 0, fmt.Errorf("%w: retired parent identity changed", ErrCurrentAbsenceUnavailable)
	}
	listing, listErr := session.ListRoot(ctx, fsbind.ListLimits{MaxEntries: 1, MaxNameBytes: maxNameBytes})
	used := listing.Used.EntriesExamined
	nameBytes := listing.Used.NameBytes
	checkErr := session.Check()
	closeErr := session.Close()
	if listErr != nil {
		return false, used, nameBytes, listErr
	}
	if checkErr != nil {
		return false, used, nameBytes, checkErr
	}
	if closeErr != nil {
		return false, used, nameBytes, fmt.Errorf("close retired parent observation failed")
	}
	if listing.Complete && used == 0 && len(listing.Entries) == 0 {
		return true, 0, 0, nil
	}
	if used > 0 {
		return false, used, nameBytes, nil
	}
	return false, used, nameBytes, fmt.Errorf("retired parent emptiness observation is incomplete")
}

func exactCleanupRoot(roots []string, parent string) bool {
	for _, root := range roots {
		relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(parent))
		if err == nil && relative == "." {
			return true
		}
	}
	return false
}

func parentCleanupPathRef(path string) string {
	digest := sha256.Sum256([]byte(parentCleanupPathDomain + filepath.Clean(path)))
	return markerIDPrefix + hex.EncodeToString(digest[:])
}

func parentCleanupPlanID(plan ParentCleanupPlan) (string, error) {
	copyPlan := plan
	copyPlan.ID = ""
	// CurrentAbsenceID intentionally binds one timestamped observation. The
	// reviewed plan must instead be reproducible by a later invocation that
	// repeats the same proof, so the observation remains report evidence but is
	// not part of the deterministic cleanup-plan identity.
	copyPlan.CurrentAbsenceID = ""
	copyPlan.AbsolutePathsShown = false
	copyPlan.Directories = append([]ParentCleanupDirectory(nil), plan.Directories...)
	for index := range copyPlan.Directories {
		copyPlan.Directories[index].ParentPath = ""
	}
	raw, err := json.Marshal(copyPlan)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(parentCleanupPlanDomain), raw...))
	return markerIDPrefix + hex.EncodeToString(digest[:]), nil
}

func (plan ParentCleanupPlan) Validate() error {
	operation, operationErr := ParseOperationID(plan.RetirementOperationID)
	derived, deriveErr := executionOperationID(plan.RetirementPlanID)
	expectedEvidence := []string{
		"canonical_terminal_source_retirement_journal_read",
		"same_invocation_two_pass_identity_bound_retired_name_absence",
		"same_invocation_two_pass_identity_bound_empty_immediate_parent_observation",
		"search_roots_and_ancestors_excluded",
		"bracketed_non_atomic",
	}
	if plan.Schema != ParentCleanupPlanSchemaV1 || plan.CleanupAuthority != "none" ||
		operationErr != nil || deriveErr != nil || operation != derived ||
		!canonicalSHA256ID(plan.RetirementCompletionID) || !canonicalSHA256ID(plan.CurrentAbsenceID) ||
		!canonicalSHA256ID(plan.SearchScopeID) || plan.RetiredFiles <= 0 || plan.RetiredFiles > hardExecutionMaxFiles || plan.ParentsConsidered <= 0 ||
		plan.ParentsConsidered != len(plan.Directories) || plan.CandidateParents < 0 ||
		plan.RetainedParents < 0 || plan.ProtectedRoots < 0 ||
		plan.CandidateParents+plan.RetainedParents+plan.ProtectedRoots != plan.ParentsConsidered ||
		len(plan.EvidenceBasis) != len(expectedEvidence) {
		return fmt.Errorf("parent-cleanup plan is invalid")
	}
	for index := range expectedEvidence {
		if plan.EvidenceBasis[index] != expectedEvidence[index] {
			return fmt.Errorf("parent-cleanup evidence basis is invalid")
		}
	}
	seenRefs := make(map[string]struct{}, len(plan.Directories))
	seenIdentities := make(map[string]struct{}, len(plan.Directories))
	retiredFiles, candidates, retained, protected := 0, 0, 0, 0
	for index, directory := range plan.Directories {
		identity, identityErr := fsbind.ParseIdentity(directory.ParentIdentity)
		if directory.Sequence != index || !canonicalSHA256ID(directory.ParentPathRef) || identityErr != nil || identity.IsZero() || directory.RetiredFiles <= 0 {
			return fmt.Errorf("parent-cleanup directory is invalid")
		}
		if _, duplicate := seenRefs[directory.ParentPathRef]; duplicate {
			return fmt.Errorf("parent-cleanup directory path is duplicated")
		}
		if _, duplicate := seenIdentities[directory.ParentIdentity]; duplicate {
			return fmt.Errorf("parent-cleanup directory identity is duplicated")
		}
		seenRefs[directory.ParentPathRef], seenIdentities[directory.ParentIdentity] = struct{}{}, struct{}{}
		retiredFiles += directory.RetiredFiles
		if plan.AbsolutePathsShown {
			if directory.ParentPath == "" || !filepath.IsAbs(directory.ParentPath) || filepath.Clean(directory.ParentPath) != directory.ParentPath ||
				parentCleanupPathRef(directory.ParentPath) != directory.ParentPathRef {
				return fmt.Errorf("parent-cleanup path is invalid")
			}
		} else if directory.ParentPath != "" {
			return fmt.Errorf("parent-cleanup path disclosure is inconsistent")
		}
		switch directory.Status {
		case "empty_stable_candidate":
			if directory.ObservationPasses != 2 || directory.EntriesObserved != 0 {
				return fmt.Errorf("parent-cleanup candidate evidence is invalid")
			}
			candidates++
		case "retained_nonempty":
			if directory.ObservationPasses != 1 || directory.EntriesObserved <= 0 {
				return fmt.Errorf("parent-cleanup retained evidence is invalid")
			}
			retained++
		case "protected_search_root":
			if directory.ObservationPasses != 0 || directory.EntriesObserved != 0 {
				return fmt.Errorf("parent-cleanup protected-root evidence is invalid")
			}
			protected++
		default:
			return fmt.Errorf("parent-cleanup directory status is invalid")
		}
	}
	if retiredFiles != plan.RetiredFiles || candidates != plan.CandidateParents || retained != plan.RetainedParents || protected != plan.ProtectedRoots {
		return fmt.Errorf("parent-cleanup retired file total disagrees")
	}
	expected, err := parentCleanupPlanID(plan)
	if err != nil || expected != plan.ID {
		return fmt.Errorf("parent-cleanup plan identity is invalid")
	}
	return nil
}

func failParentCleanupCompletion(report *ParentCleanupReport, err error) (ParentCleanupReport, error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		report.Issues = append(report.Issues, Finding{Code: "cleanup.context_cancelled", Message: "terminal retirement inspection was interrupted"})
	case errors.Is(err, ErrExecutionIntegrity):
		report.Outcome = ParentCleanupOutcomeIntegrity
		report.Blockers = append(report.Blockers, Finding{Code: "cleanup.retirement_integrity_failed", Message: "the terminal source-retirement journal failed integrity validation"})
	case errors.Is(err, ErrExecutionPolicy):
		report.Outcome = ParentCleanupOutcomeBlocked
		report.Blockers = append(report.Blockers, Finding{Code: "cleanup.terminal_retirement_unavailable", Message: "the selected source-retirement operation is not a usable terminal journal"})
	default:
		report.Issues = append(report.Issues, Finding{Code: "cleanup.retirement_inspection_incomplete", Message: "the terminal source-retirement journal could not be inspected"})
	}
	report.finalizeParentCleanup()
	return *report, err
}

func (report *ParentCleanupReport) finalizeParentCleanup() {
	if report.Effect == nil {
		report.Effect = []string{}
	}
	sort.Strings(report.Effect)
	if report.Plan.Directories == nil {
		report.Plan.Directories = []ParentCleanupDirectory{}
	}
	if report.Plan.EvidenceBasis == nil {
		report.Plan.EvidenceBasis = []string{}
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
