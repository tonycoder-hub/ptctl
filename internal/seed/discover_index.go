package seed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/storage"
	"github.com/tonycoder-hub/ptctl/internal/storageindex"
)

// DiscoverFromIndex runs the ordinary authoritative torrent matcher over live
// reobservations selected from one sealed historical snapshot. Without an
// explicit match selector it never returns authority or a plan because the
// snapshot cannot prove current uniqueness. With a selector, it may retain
// same-invocation authority for exactly that live-reverified assignment; this
// is explicit choice, never a negative or uniqueness proof.
func DiscoverFromIndex(ctx context.Context, meta *metafile.MetaInfo, profile storageindex.Profile, indexed storageindex.CandidateResult, options DiscoverOptions) (DiscoveryResult, error) {
	result := newDiscoveryResult(meta, options)
	if meta == nil {
		return result, fmt.Errorf("metafile is nil")
	}
	if err := profile.Validate(); err != nil {
		return result, err
	}
	if !indexed.HasLiveAuthority(profile) {
		return result, fmt.Errorf("sealed storage index candidates are unavailable or unverified")
	}
	if options.ExplicitSourceMatchID != "" && !canonicalSourceMatchID(options.ExplicitSourceMatchID) {
		return result, fmt.Errorf("explicit source match identity is invalid")
	}
	if err := options.InventoryLimits.Validate(); err != nil {
		return result, err
	}
	if err := options.MatchLimits.Validate(); err != nil {
		return result, err
	}
	if options.Strategy == "" {
		options.Strategy = "copy"
	}
	if options.Strategy != "copy" {
		return result, fmt.Errorf("the alpha supports only the copy strategy")
	}
	if options.ClientMapping != nil {
		if err := storage.ValidatePathMappingConfig(options.ClientMapping.HostRoot, options.ClientMapping.ClientRoot, options.ClientMapping.ClientWindows); err != nil {
			return result, fmt.Errorf("invalid host-to-client mapping: %w", err)
		}
	}

	resolved := make([]resolvedCandidate, 0, len(indexed.Candidates))
	for _, candidate := range indexed.Candidates {
		resolved = append(resolved, resolvedCandidate{observation: candidate.Observation, path: candidate.ResolvedPath})
	}
	sets, files := buildCandidateSets(ctx, meta, resolved, options.MatchLimits, options.ShowAbsolutePaths)
	matchResult, err := metafile.MatchSourceCandidates(ctx, meta, sets, options.MatchLimits)
	if err != nil {
		return result, err
	}
	if !indexed.Complete {
		matchResult.Complete = false
		for _, reason := range indexed.StopReasons {
			matchResult.StopReasons = appendUnique(matchResult.StopReasons, "index."+reason)
		}
	}

	inventory := indexedInventoryProjection(profile, indexed, options.InventoryLimits)
	result.Scan = buildDiscoveryScan(inventory, matchResult, options.ShowAbsolutePaths)
	result.Scan.TimeBudgetMillis = options.TimeBudget.Milliseconds()
	result.Files = files
	observationByID := make(map[string]resolvedCandidate, len(resolved))
	for _, candidate := range resolved {
		observationByID[candidate.observation.ObservationID] = candidate
	}
	result.Matches = make([]DiscoveryMatch, 0, len(matchResult.Matches))
	for _, match := range matchResult.Matches {
		public := buildDiscoveryMatch(meta, match, observationByID, options)
		public.Verification = public.Verification.PublicCopy()
		result.Matches = append(result.Matches, public)
	}
	result.Warnings = append(result.Warnings, indexed.Warnings...)
	result.Warnings = append(result.Warnings,
		"historical locators were reobserved and any listed matches were content-verified in this invocation",
		"zero writes were intentionally performed; refresh is a separate explicit command",
	)
	if options.ExplicitSourceMatchID != "" {
		return finishExplicitIndexedSelection(ctx, meta, profile, indexed, options, resolved, matchResult, result)
	}

	result.SourceOutcome = "incomplete"
	result.Selection = DiscoverySelection{Status: "blocked"}
	result.BestEvidence = "none"
	if len(result.Matches) > 0 {
		result.BestEvidence = "verified"
	} else if len(indexed.Candidates) > 0 {
		result.BestEvidence = "candidate"
	}
	result.Blockers = append(result.Blockers,
		DiscoveryBlocker{Code: "index.historical_scope", Message: "the sealed snapshot proves only a past inventory; it cannot establish current uniqueness or absence"},
		DiscoveryBlocker{Code: "scan.incomplete", Message: "a complete live scan of the current profile roots is required for source selection"},
	)
	knownVerified := matchResult.Stats.VerifiedLayouts
	if knownVerified < len(result.Matches) {
		knownVerified = len(result.Matches)
	}
	if knownVerified >= 2 {
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "source.multiple_verified_matches", Message: fmt.Sprintf("at least %d historical locators passed current exact verification", knownVerified)})
	} else if knownVerified == 0 {
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "source.no_verified_index_candidate", Message: "no retained historical locator passed current exact torrent verification; this is not a current-filesystem not-found proof"})
	}
	if !matchResult.Complete {
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "source.verification_incomplete", Message: "indexed candidate verification stopped at a configured safety budget or changing file"})
	}
	if options.ClientMapping != nil || options.TargetRoot != "" {
		result.Handoff.Status = "blocked"
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "handoff.current_uniqueness_required", Message: "snapshot-only discovery never produces a materialization or client handoff plan"})
	}
	result.Plan = nil
	result.Blockers = deduplicateBlockers(result.Blockers)
	return result, nil
}

// DiscoverFromCurrentIndex runs the ordinary authoritative matcher over one
// candidate set linked to a complete storage-profile refresh in this same
// invocation. It can therefore establish current-search uniqueness or absence
// with the same bracketed, non-atomic assurance as ordinary live discovery.
// Reading the published generation again later must use DiscoverFromIndex and
// loses this authority.
func DiscoverFromCurrentIndex(ctx context.Context, meta *metafile.MetaInfo, profile storageindex.Profile, indexed storageindex.CandidateResult, options DiscoverOptions) (DiscoveryResult, error) {
	result := newDiscoveryResult(meta, options)
	if meta == nil {
		return result, fmt.Errorf("metafile is nil")
	}
	if err := profile.Validate(); err != nil {
		return result, err
	}
	if !indexed.HasLiveAuthority(profile) || !indexed.CurrentSearchComplete || indexed.CurrentSearch == nil || indexed.CurrentSearch.Status != "complete" {
		return result, fmt.Errorf("same-invocation complete storage-index refresh authority is unavailable")
	}
	if options.ExplicitSourceMatchID != "" {
		return result, fmt.Errorf("explicit source selection is unavailable for a complete current-refresh search")
	}
	if err := validateIndexedDiscoverOptions(&options); err != nil {
		return result, err
	}
	if len(meta.Files) > options.MatchLimits.MaxStates {
		return manifestStateBudgetDiscovery(ctx, meta, options, "current-index discovery stopped before candidate preparation or content access; zero writes were performed")
	}

	resolved := indexedResolvedCandidates(indexed)
	sets, files := buildCandidateSets(ctx, meta, resolved, options.MatchLimits, options.ShowAbsolutePaths)
	matchResult, err := metafile.MatchSourceCandidates(ctx, meta, sets, options.MatchLimits)
	if err != nil {
		return result, err
	}
	if !indexed.Complete || !indexed.CurrentSearchComplete {
		matchResult.Complete = false
		for _, reason := range indexed.StopReasons {
			matchResult.StopReasons = appendUnique(matchResult.StopReasons, "index."+reason)
		}
	}

	inventory := currentIndexedInventoryProjection(indexed, options.InventoryLimits)
	result.Scan = buildDiscoveryScan(inventory, matchResult, options.ShowAbsolutePaths)
	result.Scan.TimeBudgetMillis = options.TimeBudget.Milliseconds()
	result.Files = files
	observationByID := indexedObservationMap(resolved)
	result.Matches = make([]DiscoveryMatch, 0, len(matchResult.Matches))
	for _, match := range matchResult.Matches {
		result.Matches = append(result.Matches, buildDiscoveryMatch(meta, match, observationByID, options))
	}
	result.Warnings = append(result.Warnings, indexed.Warnings...)
	result.Warnings = append(result.Warnings,
		"the complete profile refresh and exact content verification occurred in this invocation; the observation is bracketed and non-atomic",
		"the published generation becomes historical candidate evidence after this invocation and cannot later prove uniqueness or absence",
	)

	result.BestEvidence = "none"
	if len(result.Matches) > 0 {
		result.BestEvidence = "verified"
	} else if len(indexed.Candidates) > 0 {
		result.BestEvidence = "candidate"
	}
	knownVerified := matchResult.Stats.VerifiedLayouts
	if knownVerified < len(result.Matches) {
		knownVerified = len(result.Matches)
	}
	result.Selection.Status = "blocked"
	switch {
	case knownVerified >= 2:
		result.SourceOutcome = "verified_ambiguous"
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "source.multiple_verified_matches", Message: fmt.Sprintf("at least %d distinct source assignments passed exact verification", knownVerified)})
	case !result.Scan.Complete || !result.Scan.VerificationComplete:
		result.SourceOutcome = "incomplete"
	case len(matchResult.Matches) == 1:
		selected := matchResult.Matches[0]
		scopeID := indexedSourceSelectionID(profile.ID, indexed.SnapshotID, indexed.DescriptorRecordID.String(), selected.ID)
		result.SourceOutcome = "verified_unique"
		result.Selection = DiscoverySelection{Status: "ready", SelectedID: selected.ID, Basis: "same_invocation_complete_index_refresh_unique_exact_match", ScopeID: scopeID}
		result.verifiedSource = selected.Source
		result.verifiedSelectionID = selected.ID
		result.verifiedSourceMode = "indexed_current_unique"
		result.verifiedScopeID = scopeID
	default:
		result.SourceOutcome = "not_found"
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "source.no_verified_match", Message: "no source assignment from the complete current profile refresh passed exact torrent verification"})
	}
	if !result.Scan.Complete {
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "scan.incomplete", Message: "the same-invocation storage-profile refresh or candidate reobservation was incomplete; uniqueness cannot be established"})
	}
	if !result.Scan.VerificationComplete {
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "source.verification_incomplete", Message: "candidate verification stopped at a configured safety budget or changing file"})
	}

	handoffRequested := options.ClientMapping != nil || options.TargetRoot != ""
	if handoffRequested {
		result.Handoff.Status = "blocked"
	}
	if options.ClientMapping != nil && options.TargetRoot == "" && result.SourceOutcome == "verified_unique" && len(result.Matches) == 1 {
		if result.Matches[0].Mapping.Status == "complete" {
			result.Handoff.Status = "ready"
		} else if len(result.Matches[0].Blockers) == 0 {
			result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "mapping.partial", Message: "the verified source could not be represented in the requested client namespace"})
		} else {
			result.Blockers = append(result.Blockers, result.Matches[0].Blockers...)
		}
	}
	if options.TargetRoot != "" && result.SourceOutcome == "verified_unique" && len(matchResult.Matches) == 1 {
		plan, planErr := buildMaterializePlanFromIndexedSelection(ctx, meta, matchResult.Matches[0].Source, options.TargetRoot, options.Strategy, result.verifiedScopeID)
		if planErr != nil {
			result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "plan.target_blocked", Message: "the requested target layout could not be planned safely"})
		} else {
			mappingFailed := false
			if options.ClientMapping != nil {
				mapped, mapErr := MapPlanTargets(plan, options.ClientMapping.HostRoot, options.ClientMapping.ClientRoot, options.ClientMapping.ClientWindows)
				if mapErr != nil {
					mappingFailed = true
					blocker, status := mappingFailure(mapErr, true)
					result.Blockers = append(result.Blockers, blocker)
					plan.ClientMapping = status
					plan.Blockers = append(plan.Blockers, blocker.Message)
					plan.ID = planID(plan)
				} else {
					plan = mapped
				}
			}
			result.Plan = publicDiscoveryPlan(meta, plan, observationByID, options.ShowAbsolutePaths)
			result.Handoff.PlanProduced = true
			if !mappingFailed {
				result.Handoff.Status = "ready"
			}
		}
	}
	if handoffRequested && result.SourceOutcome != "verified_unique" {
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "handoff.source_selection_blocked", Message: "the requested handoff requires one complete, uniquely verified source assignment"})
	}
	result.Blockers = deduplicateBlockers(result.Blockers)
	return result, nil
}

// RefreshAndDiscoverFromIndex is the explicit effectful boundary which writes
// one complete immutable index generation and immediately consumes its
// process-local refresh authority for current source discovery. The published
// records remain useful as historical candidates, but cannot recreate the
// current-search authority after this call.
func RefreshAndDiscoverFromIndex(
	ctx context.Context,
	repository *storageindex.Repository,
	meta *metafile.MetaInfo,
	profile storageindex.Profile,
	candidateLimits storageindex.CandidateLimits,
	options DiscoverOptions,
	refreshOptions storageindex.RefreshOptions,
) (DiscoveryResult, error) {
	result := newDiscoveryResult(meta, options)
	if meta == nil {
		return result, fmt.Errorf("metafile is nil")
	}
	if err := profile.Validate(); err != nil {
		return result, err
	}
	if options.ExplicitSourceMatchID != "" {
		return result, fmt.Errorf("explicit source selection is unavailable for refresh discovery")
	}
	if err := validateIndexedDiscoverOptions(&options); err != nil {
		return result, err
	}
	if err := candidateLimits.Validate(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(meta.Files) > options.MatchLimits.MaxStates {
		blocked, err := manifestStateBudgetDiscovery(ctx, meta, options, "refresh discovery stopped before filesystem inventory or sealed-record publication; zero writes were performed")
		blocked.Effect = "read_storage_metadata+write_private_storage_index+read_content"
		blocked.IndexRefresh = &DiscoveryIndexRefresh{
			Status: "not_started", Effect: "read_storage_metadata+write_private_storage_index", WritesPerformed: 0,
			ProfileID: profile.ID, ProfileRevision: profile.Revision, ScanComplete: false,
			StopReasons: []string{"max_candidate_states"}, Assurance: "not_started_before_publication",
			CurrentSearchStatus: "not_started", CurrentSearchAssurance: "not_started_before_publication",
			CandidateLimits: candidateLimits, CandidateStopReasons: []string{"max_candidate_states"},
		}
		blocked.bindIndexRefreshAuthority()
		return blocked, err
	}

	refresh, indexed, refreshErr := repository.RefreshAndLoadCandidates(
		ctx, profile, manifestWantedSizes(meta), candidateLimits, refreshOptions,
	)
	result.IndexRefresh = discoveryIndexRefresh(refresh, profile)
	result.Effect = "read_storage_metadata+write_private_storage_index+read_content"
	result.WritesPerformed = refresh.WritesPerformed
	if refresh.Status != "stored" || !refresh.HasLiveAuthority(profile) {
		result = incompleteRefreshDiscovery(result, meta, refresh, options, "index_refresh_incomplete")
		result.bindIndexRefreshAuthority()
		return result, refreshErr
	}
	if refreshErr != nil {
		reason := "current_candidate_load_failed"
		if indexed.HasLiveAuthority(profile) {
			reason = "current_candidate_binding_failed"
		}
		result.IndexRefresh = incompleteCurrentSearchReceipt(refresh, profile, indexed, reason)
		result = incompleteRefreshDiscovery(result, meta, refresh, options, reason)
		result.bindIndexRefreshAuthority()
		return result, refreshErr
	}
	if !indexed.HasLiveAuthority(profile) || indexed.CurrentSearch == nil {
		result.IndexRefresh = incompleteCurrentSearchReceipt(refresh, profile, indexed, "current_candidate_authority_unavailable")
		result = incompleteRefreshDiscovery(result, meta, refresh, options, "current_candidate_authority_unavailable")
		result.bindIndexRefreshAuthority()
		return result, nil
	}
	if !indexed.CurrentSearchComplete {
		result.IndexRefresh = discoveryIndexRefreshWithCandidates(refresh, profile, indexed)
		result = incompleteCurrentCandidateDiscovery(result, indexed, options)
		result.bindIndexRefreshAuthority()
		return result, nil
	}

	result, err := DiscoverFromCurrentIndex(ctx, meta, profile, indexed, options)
	result.IndexRefresh = discoveryIndexRefreshWithCandidates(refresh, profile, indexed)
	result.Effect = "read_storage_metadata+write_private_storage_index+read_content"
	result.WritesPerformed = refresh.WritesPerformed
	result.bindIndexRefreshAuthority()
	return result, err
}

func incompleteCurrentSearchReceipt(refresh storageindex.RefreshResult, profile storageindex.Profile, indexed storageindex.CandidateResult, reason string) *DiscoveryIndexRefresh {
	result := discoveryIndexRefreshWithCandidates(refresh, profile, indexed)
	result.CurrentSearchStatus = "incomplete"
	result.CurrentSearchAssurance = reason
	result.CandidateStopReasons = appendUnique(result.CandidateStopReasons, reason)
	return result
}

func discoveryIndexRefreshWithCandidates(refresh storageindex.RefreshResult, profile storageindex.Profile, indexed storageindex.CandidateResult) *DiscoveryIndexRefresh {
	result := discoveryIndexRefresh(refresh, profile)
	result.CandidateLimits = indexed.Limits
	result.CandidateUsed = indexed.Stats
	result.CandidateStopReasons = append([]string{}, indexed.StopReasons...)
	if indexed.CurrentSearch == nil {
		result.CurrentSearchStatus = "unavailable"
		result.CurrentSearchAssurance = "unavailable"
		return result
	}
	result.CurrentSearchStatus = indexed.CurrentSearch.Status
	result.CurrentSearchObservedAtEnd = indexed.CurrentSearch.ObservedAtEnd
	result.CurrentSearchAssurance = indexed.CurrentSearch.Stability
	return result
}

func incompleteCurrentCandidateDiscovery(result DiscoveryResult, indexed storageindex.CandidateResult, options DiscoverOptions) DiscoveryResult {
	inventory := currentIndexedInventoryProjection(indexed, options.InventoryLimits)
	inventory.Complete = false
	inventory.LimitHits = appendUnique(inventory.LimitHits, "current_candidate_search_incomplete")
	matches := metafile.SourceMatchResult{
		Complete: false, Limits: options.MatchLimits, StopReasons: []string{"current_candidate_search_incomplete"},
		Issues: []metafile.SourceMatchIssue{}, Matches: []metafile.SourceMatch{},
	}
	result.Scan = buildDiscoveryScan(inventory, matches, options.ShowAbsolutePaths)
	result.Scan.TimeBudgetMillis = options.TimeBudget.Milliseconds()
	result.SourceOutcome = "incomplete"
	result.Selection.Status = "blocked"
	result.BestEvidence = "none"
	result.Blockers = append(result.Blockers,
		DiscoveryBlocker{Code: "index.current_candidate_search_incomplete", Message: "the complete index refresh was published, but bounded live candidate reobservation did not remain complete"},
		DiscoveryBlocker{Code: "source.verification_incomplete", Message: "source verification could not begin from one complete same-invocation candidate set"},
	)
	if options.TargetRoot != "" || options.ClientMapping != nil {
		result.Handoff.Status = "blocked"
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "handoff.source_selection_blocked", Message: "the requested handoff requires one complete, uniquely verified source assignment"})
	}
	result.Warnings = append(result.Warnings, indexed.Warnings...)
	result.Warnings = append(result.Warnings, "the published generation is historical candidate evidence only; this invocation did not establish current uniqueness or absence")
	result.Blockers = deduplicateBlockers(result.Blockers)
	return result
}

func discoveryIndexRefresh(refresh storageindex.RefreshResult, profile storageindex.Profile) *DiscoveryIndexRefresh {
	assurance := "incomplete"
	if refresh.HasLiveAuthority(profile) {
		assurance = "same_invocation_complete_generation_published_and_revalidated"
	}
	return &DiscoveryIndexRefresh{
		Status: refresh.Status, Effect: refresh.Effect, WritesPerformed: refresh.WritesPerformed,
		ProfileID: refresh.ProfileID, ProfileRevision: refresh.ProfileRevision, Generation: refresh.Generation,
		SnapshotID: refresh.SnapshotID, ObservedAtStart: refresh.ObservedAtStart, ObservedAtEnd: refresh.ObservedAtEnd,
		DataRecord: refresh.DataRecord, DescriptorRecord: refresh.DescriptorRecord,
		DataPublication: refresh.DataPublication, DescriptorPublication: refresh.DescriptorPublication,
		ScanComplete: refresh.Scan.Complete, ScanLimits: refresh.Scan.Limits, ScanUsed: refresh.Scan.Stats,
		StopReasons: append([]string{}, refresh.StopReasons...), Assurance: assurance,
		CurrentSearchStatus: "not_started", CurrentSearchAssurance: "not_started",
		CandidateStopReasons: []string{},
	}
}

func incompleteRefreshDiscovery(result DiscoveryResult, meta *metafile.MetaInfo, refresh storageindex.RefreshResult, options DiscoverOptions, reason string) DiscoveryResult {
	inventory := refreshInventoryProjection(refresh, options.InventoryLimits)
	inventory.Complete = false
	inventory.LimitHits = appendUnique(inventory.LimitHits, reason)
	matches := metafile.SourceMatchResult{
		Complete: false, Limits: options.MatchLimits, StopReasons: []string{reason},
		Issues: []metafile.SourceMatchIssue{}, Matches: []metafile.SourceMatch{},
	}
	result.Scan = buildDiscoveryScan(inventory, matches, options.ShowAbsolutePaths)
	result.Scan.TimeBudgetMillis = options.TimeBudget.Milliseconds()
	result.SourceOutcome = "incomplete"
	result.Selection.Status = "blocked"
	result.BestEvidence = "none"
	result.Blockers = append(result.Blockers,
		DiscoveryBlocker{Code: "index." + reason, Message: "the index refresh and live candidate reobservation did not establish one complete current search"},
		DiscoveryBlocker{Code: "source.verification_incomplete", Message: "source verification could not begin from one complete same-invocation candidate set"},
	)
	if options.TargetRoot != "" || options.ClientMapping != nil {
		result.Handoff.Status = "blocked"
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "handoff.source_selection_blocked", Message: "the requested handoff requires one complete, uniquely verified source assignment"})
	}
	result.Warnings = append(result.Warnings,
		"any published index generation is historical candidate evidence only; this incomplete invocation did not establish current uniqueness or absence",
	)
	result.Blockers = deduplicateBlockers(result.Blockers)
	return result
}

func refreshInventoryProjection(refresh storageindex.RefreshResult, limits storage.InventoryLimits) storage.InventoryResult {
	roots := make([]storage.SearchRootObservation, len(refresh.Scan.Roots))
	for index, root := range refresh.Scan.Roots {
		roots[index] = storage.SearchRootObservation{ID: root.ID, Status: root.Status}
	}
	issues := make([]storage.ScanIssue, 0, len(refresh.Scan.Issues))
	for _, issue := range refresh.Scan.Issues {
		issues = append(issues, storage.ScanIssue{Code: "index." + issue.Code, RootID: issue.RootID, Message: "storage index refresh observation was incomplete"})
	}
	limitHits := append([]string(nil), refresh.Scan.LimitHits...)
	for _, reason := range refresh.Scan.StopReasons {
		limitHits = appendUnique(limitHits, reason)
	}
	for _, reason := range refresh.StopReasons {
		limitHits = appendUnique(limitHits, reason)
	}
	return storage.InventoryResult{
		Complete: refresh.Scan.Complete, PathConfinement: refresh.Scan.PathConfinement, Limits: limits,
		Roots: roots, Candidates: []storage.FileObservation{}, Stats: inventoryStatsFromFull(refresh.Scan.Stats),
		LimitHits: limitHits, Issues: issues, Warnings: append([]string(nil), refresh.Scan.Warnings...),
	}
}

func inventoryStatsFromFull(stats storage.FullInventoryStats) storage.InventoryStats {
	return storage.InventoryStats{
		DirectoriesOpened: stats.DirectoriesOpened, EntriesExamined: stats.EntriesExamined,
		FilesExamined: stats.RegularFilesSeen, CandidatesRetained: stats.FilesEmitted,
		RetainedPathBytes: stats.EmittedPathBytes, SymlinksSkipped: stats.SymlinksSkipped,
		ReparseSkipped: stats.ReparseSkipped, MountsSkipped: stats.MountsSkipped,
		SpecialSkipped: stats.SpecialSkipped, IssueOverflow: stats.IssueOverflow,
	}
}

func validateIndexedDiscoverOptions(options *DiscoverOptions) error {
	if options.Strategy == "" {
		options.Strategy = "copy"
	}
	if options.Strategy != "copy" {
		return fmt.Errorf("the alpha supports only the copy strategy")
	}
	if err := options.InventoryLimits.Validate(); err != nil {
		return err
	}
	if err := options.MatchLimits.Validate(); err != nil {
		return err
	}
	if options.ClientMapping != nil {
		if err := storage.ValidatePathMappingConfig(options.ClientMapping.HostRoot, options.ClientMapping.ClientRoot, options.ClientMapping.ClientWindows); err != nil {
			return fmt.Errorf("invalid host-to-client mapping: %w", err)
		}
	}
	return nil
}

func indexedResolvedCandidates(indexed storageindex.CandidateResult) []resolvedCandidate {
	resolved := make([]resolvedCandidate, 0, len(indexed.Candidates))
	for _, candidate := range indexed.Candidates {
		resolved = append(resolved, resolvedCandidate{observation: candidate.Observation, path: candidate.ResolvedPath})
	}
	return resolved
}

func indexedObservationMap(resolved []resolvedCandidate) map[string]resolvedCandidate {
	result := make(map[string]resolvedCandidate, len(resolved))
	for _, candidate := range resolved {
		result[candidate.observation.ObservationID] = candidate
	}
	return result
}

func currentIndexedInventoryProjection(indexed storageindex.CandidateResult, limits storage.InventoryLimits) storage.InventoryResult {
	current := indexed.CurrentSearch
	limits.MaxRoots = current.Limits.MaxRoots
	limits.MaxDepth = current.Limits.MaxDepth
	limits.MaxDirectories = current.Limits.MaxDirectories
	limits.MaxEntries = current.Limits.MaxEntries
	limits.MaxEntriesPerDirectory = current.Limits.MaxEntriesPerDirectory
	limits.MaxCandidates = indexed.Limits.MaxCandidates
	limits.MaxPathBytes = indexed.Limits.MaxPathBytes
	limits.MaxIssues = indexed.Limits.MaxIssues
	roots := make([]storage.SearchRootObservation, len(current.Roots))
	for index, root := range current.Roots {
		roots[index] = storage.SearchRootObservation{ID: root.ID, Status: root.Status}
	}
	issues := make([]storage.ScanIssue, 0, len(indexed.Issues))
	for _, issue := range indexed.Issues {
		issues = append(issues, storage.ScanIssue{Code: "index." + issue.Code, RootID: issue.RootID, Message: "same-invocation index candidate could not be reused safely"})
	}
	limitHits := []string{}
	for _, reason := range indexed.StopReasons {
		limitHits = appendUnique(limitHits, "index."+reason)
	}
	return storage.InventoryResult{
		Complete:        indexed.CurrentSearchComplete && indexed.Complete,
		PathConfinement: current.PathConfinement,
		Limits:          limits, Roots: roots, Candidates: []storage.FileObservation{},
		Stats: func() storage.InventoryStats {
			stats := inventoryStatsFromFull(current.Stats)
			stats.CandidatesRetained = len(indexed.Candidates)
			stats.RetainedPathBytes = indexed.Stats.RetainedPathBytes
			stats.IssueOverflow += indexed.Stats.IssueOverflow
			return stats
		}(),
		LimitHits: limitHits, Issues: issues, Warnings: append([]string{}, indexed.Warnings...),
	}
}

func finishExplicitIndexedSelection(ctx context.Context, meta *metafile.MetaInfo, profile storageindex.Profile, indexed storageindex.CandidateResult, options DiscoverOptions, resolved []resolvedCandidate, matches metafile.SourceMatchResult, result DiscoveryResult) (DiscoveryResult, error) {
	result.SourceOutcome = "incomplete"
	result.Selection = DiscoverySelection{Status: "blocked", SelectedID: options.ExplicitSourceMatchID, Basis: "explicit_historical_locator_live_verification"}
	result.BestEvidence = "none"
	if len(result.Matches) > 0 {
		result.BestEvidence = "verified"
	} else if len(indexed.Candidates) > 0 {
		result.BestEvidence = "candidate"
	}
	selectedIndex := -1
	for index := range matches.Matches {
		if matches.Matches[index].ID == options.ExplicitSourceMatchID {
			selectedIndex = index
			break
		}
	}
	if selectedIndex < 0 {
		result.Blockers = append(result.Blockers,
			DiscoveryBlocker{Code: "source.selected_match_unavailable", Message: "the explicitly selected historical source assignment did not pass current exact verification"},
			DiscoveryBlocker{Code: "index.historical_scope", Message: "the sealed snapshot cannot prove current absence when the selected locator is unavailable"},
		)
		if !matches.Complete {
			result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "source.verification_incomplete", Message: "indexed candidate verification stopped before the selected assignment could be established"})
		}
		if options.TargetRoot != "" || options.ClientMapping != nil {
			result.Handoff.Status = "blocked"
		}
		result.Plan = nil
		result.Blockers = deduplicateBlockers(result.Blockers)
		return result, nil
	}

	selected := matches.Matches[selectedIndex]
	scopeID := indexedSourceSelectionID(profile.ID, indexed.SnapshotID, indexed.DescriptorRecordID.String(), selected.ID)
	result.SourceOutcome = "verified_selected"
	result.Selection = DiscoverySelection{
		Status: "ready_explicit", SelectedID: selected.ID,
		Basis: "explicit_historical_locator_live_verification", ScopeID: scopeID,
	}
	result.BestEvidence = "verified"
	result.verifiedSource = selected.Source
	result.verifiedSelectionID = selected.ID
	result.verifiedSourceMode = "indexed_explicit"
	result.verifiedScopeID = scopeID
	if options.ClientMapping != nil || options.TargetRoot != "" {
		result.Handoff.Status = "blocked"
	}
	result.Warnings = append(result.Warnings,
		"the selected source assignment was explicitly chosen and exactly verified; it is not a current-filesystem uniqueness proof",
		"new, renamed, or unindexed alternative copies do not invalidate an explicitly selected exact source, but they remain unobserved",
	)
	if !matches.Complete {
		result.Warnings = append(result.Warnings, "other historical candidate assignments were not exhaustively evaluated; only the explicit selected assignment is authorized")
	}

	selectedPublic := findDiscoveryMatch(result.Matches, selected.ID)
	if options.ClientMapping != nil && options.TargetRoot == "" {
		if selectedPublic != nil && selectedPublic.Mapping.Status == "complete" {
			result.Handoff.Status = "ready"
		} else {
			result.Handoff.Status = "blocked"
			if selectedPublic != nil {
				result.Blockers = append(result.Blockers, selectedPublic.Blockers...)
			}
			if len(result.Blockers) == 0 {
				result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "mapping.partial", Message: "the selected verified source could not be represented in the requested client namespace"})
			}
		}
	}
	if options.TargetRoot != "" {
		plan, err := buildMaterializePlanFromIndexedSelection(ctx, meta, selected.Source, options.TargetRoot, options.Strategy, scopeID)
		if err != nil {
			result.Handoff.Status = "blocked"
			result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "plan.target_blocked", Message: "the requested target layout could not be planned safely for the selected source"})
		} else {
			mappingFailed := false
			if options.ClientMapping != nil {
				mapped, mapErr := MapPlanTargets(plan, options.ClientMapping.HostRoot, options.ClientMapping.ClientRoot, options.ClientMapping.ClientWindows)
				if mapErr != nil {
					mappingFailed = true
					blocker, status := mappingFailure(mapErr, true)
					result.Blockers = append(result.Blockers, blocker)
					plan.ClientMapping = status
					plan.Blockers = append(plan.Blockers, blocker.Message)
					plan.ID = planID(plan)
				} else {
					plan = mapped
				}
			}
			observationByID := make(map[string]resolvedCandidate, len(resolved))
			for _, candidate := range resolved {
				observationByID[candidate.observation.ObservationID] = candidate
			}
			result.Plan = publicDiscoveryPlan(meta, plan, observationByID, options.ShowAbsolutePaths)
			result.Handoff.PlanProduced = true
			if !mappingFailed {
				result.Handoff.Status = "ready"
			}
		}
	}
	result.Blockers = deduplicateBlockers(result.Blockers)
	return result, nil
}

func findDiscoveryMatch(matches []DiscoveryMatch, id string) *DiscoveryMatch {
	for index := range matches {
		if matches[index].ID == id {
			return &matches[index]
		}
	}
	return nil
}

func canonicalSourceMatchID(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size
}

func canonicalIndexedSourceSelectionID(value string) bool {
	return canonicalSourceMatchID(value)
}

func indexedSourceSelectionID(profileID, snapshotID, descriptorRecordID, matchID string) string {
	digest := sha256.New()
	_, _ = io.WriteString(digest, "ptctl-indexed-source-selection-v1\x00")
	for _, value := range []string{profileID, snapshotID, descriptorRecordID, matchID} {
		_, _ = fmt.Fprintf(digest, "%d:", len(value))
		_, _ = io.WriteString(digest, value)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func indexedInventoryProjection(profile storageindex.Profile, indexed storageindex.CandidateResult, limits storage.InventoryLimits) storage.InventoryResult {
	roots := make([]storage.SearchRootObservation, len(profile.Roots))
	for index, root := range profile.Roots {
		roots[index] = storage.SearchRootObservation{ID: root.ID, Status: "historical_snapshot_reobserved"}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })
	issues := make([]storage.ScanIssue, 0, len(indexed.Issues))
	for _, issue := range indexed.Issues {
		issues = append(issues, storage.ScanIssue{Code: "index." + issue.Code, RootID: issue.RootID, Message: "historical index locator could not be reused safely"})
	}
	stops := []string{"historical_snapshot_not_current_search"}
	for _, reason := range indexed.StopReasons {
		stops = appendUnique(stops, "index."+reason)
	}
	return storage.InventoryResult{
		Complete: false, PathConfinement: "historical_snapshot_live_reobserved_best_effort_non_atomic", Limits: limits,
		Roots: roots, Candidates: []storage.FileObservation{},
		Stats: storage.InventoryStats{
			FilesExamined: indexed.Stats.SnapshotFilesConsidered, CandidatesRetained: len(indexed.Candidates),
			RetainedPathBytes: indexed.Stats.RetainedPathBytes, IssueOverflow: indexed.Stats.IssueOverflow,
		},
		LimitHits: stops, Issues: issues, Warnings: append([]string{}, indexed.Warnings...),
	}
}
