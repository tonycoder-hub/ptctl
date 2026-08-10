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
