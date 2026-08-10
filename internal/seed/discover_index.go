package seed

import (
	"context"
	"fmt"
	"sort"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/storage"
	"github.com/tonycoder-hub/ptctl/internal/storageindex"
)

// DiscoverFromIndex runs the ordinary authoritative torrent matcher over live
// reobservations selected from one sealed historical snapshot. It deliberately
// never returns verified_unique or a plan: the snapshot cannot prove that the
// current filesystem has no new or renamed alternatives outside its old rows.
func DiscoverFromIndex(ctx context.Context, meta *metafile.MetaInfo, profile storageindex.Profile, indexed storageindex.CandidateResult, options DiscoverOptions) (DiscoveryResult, error) {
	result := newDiscoveryResult(meta, options)
	if meta == nil {
		return result, fmt.Errorf("metafile is nil")
	}
	if err := profile.Validate(); err != nil {
		return result, err
	}
	if indexed.ProfileID != profile.ID || !indexed.HistoricalSnapshotVerified {
		return result, fmt.Errorf("sealed storage index candidates are unavailable or unverified")
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
	result.Warnings = append(result.Warnings, indexed.Warnings...)
	result.Warnings = append(result.Warnings,
		"historical locators were reobserved and any listed matches were content-verified in this invocation",
		"zero writes were intentionally performed; refresh is a separate explicit command",
	)
	result.Blockers = deduplicateBlockers(result.Blockers)
	return result, nil
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
