package seed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

type ClientMappingOptions struct {
	HostRoot      string
	ClientRoot    string
	ClientWindows bool
}

type DiscoverOptions struct {
	SearchRoots       []string
	InventoryLimits   storage.InventoryLimits
	MatchLimits       metafile.SourceMatchLimits
	AllowNetwork      bool
	ShowAbsolutePaths bool
	TimeBudget        time.Duration
	TargetRoot        string
	Strategy          string
	ClientMapping     *ClientMappingOptions
}

type DiscoveryTorrent struct {
	Name              string `json:"name"`
	Version           string `json:"version"`
	MetafileVariantID string `json:"metafile_variant_id"`
	InfoHashV1        string `json:"info_hash_v1,omitempty"`
	InfoHashV2        string `json:"info_hash_v2,omitempty"`
	PhysicalBytes     int64  `json:"physical_bytes"`
	Files             int    `json:"files"`
}

type DiscoverySelection struct {
	Status     string `json:"status"`
	SelectedID string `json:"selected_id,omitempty"`
}

// DiscoveryHandoff describes only the optional read-only path projection or
// layout-plan handoff. It is deliberately separate from cryptographic source
// selection and never means that an apply operation is available.
type DiscoveryHandoff struct {
	Status       string `json:"status"`
	PlanProduced bool   `json:"plan_produced"`
}

type DiscoveryBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type DiscoveryRoot struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	InputPath    string `json:"input_path,omitempty"`
	ResolvedPath string `json:"resolved_path,omitempty"`
}

type DiscoveryScan struct {
	Complete             bool                        `json:"complete"`
	VerificationComplete bool                        `json:"verification_complete"`
	TimeBudgetMillis     int64                       `json:"time_budget_millis"`
	PathConfinement      string                      `json:"path_confinement"`
	SearchRoots          []DiscoveryRoot             `json:"search_roots"`
	InventoryLimits      storage.InventoryLimits     `json:"inventory_limits"`
	MatchLimits          metafile.SourceMatchLimits  `json:"match_limits"`
	InventoryUsed        storage.InventoryStats      `json:"inventory_used"`
	MatchUsed            metafile.SourceMatchStats   `json:"match_used"`
	StopReasons          []string                    `json:"stop_reasons"`
	InventoryIssues      []storage.ScanIssue         `json:"inventory_issues"`
	MatchIssues          []metafile.SourceMatchIssue `json:"match_issues"`
}

type DiscoveryCandidate struct {
	ID                          string   `json:"id"`
	RootID                      string   `json:"root_id"`
	RelativePath                string   `json:"relative_path"`
	RelativeComponentsRawBase64 []string `json:"relative_components_raw_base64"`
	AbsolutePath                string   `json:"absolute_path,omitempty"`
	EvidenceLevel               string   `json:"evidence_level"`
	EvidenceBasis               string   `json:"evidence_basis"`
	MatchRank                   string   `json:"match_rank"`
}

type DiscoveryFile struct {
	FileIndex           int                  `json:"file_index"`
	TorrentPath         string               `json:"torrent_path"`
	Length              int64                `json:"length"`
	Requirement         string               `json:"requirement"`
	CandidateCount      int                  `json:"candidate_count"`
	CandidatesTruncated bool                 `json:"candidates_truncated"`
	Candidates          []DiscoveryCandidate `json:"candidates"`
}

type DiscoveryCoverage struct {
	FilesFound    int   `json:"files_found"`
	FilesExpected int   `json:"files_expected"`
	BytesFound    int64 `json:"bytes_found"`
	BytesExpected int64 `json:"bytes_expected"`
}

type DiscoveryBinding struct {
	FileIndex                   int      `json:"file_index"`
	TorrentPath                 string   `json:"torrent_path"`
	CandidateID                 string   `json:"candidate_id"`
	RootID                      string   `json:"root_id"`
	RelativePath                string   `json:"relative_path"`
	RelativeComponentsRawBase64 []string `json:"relative_components_raw_base64"`
	AbsolutePath                string   `json:"absolute_path,omitempty"`
	ClientPath                  string   `json:"client_path,omitempty"`
}

type DiscoveryMapping struct {
	Status          string `json:"status"`
	Evidence        string `json:"evidence,omitempty"`
	ClientReachable string `json:"client_reachable,omitempty"`
}

type DiscoveryMatch struct {
	ID            string                      `json:"id"`
	EvidenceLevel string                      `json:"evidence_level"`
	EvidenceBasis []string                    `json:"evidence_basis"`
	Layout        string                      `json:"layout"`
	BasenameMatch *bool                       `json:"basename_match,omitempty"`
	Coverage      DiscoveryCoverage           `json:"coverage"`
	Bindings      []DiscoveryBinding          `json:"bindings"`
	Verification  metafile.VerificationResult `json:"verification"`
	Mapping       DiscoveryMapping            `json:"mapping"`
	Blockers      []DiscoveryBlocker          `json:"blockers"`
}

type DiscoveryPlanOperation struct {
	ManifestIndex int                          `json:"manifest_index"`
	TorrentPath   string                       `json:"torrent_path"`
	Kind          string                       `json:"kind"`
	Source        string                       `json:"source,omitempty"`
	Target        string                       `json:"target"`
	ClientTarget  string                       `json:"client_target,omitempty"`
	Bytes         int64                        `json:"bytes"`
	Precondition  *metafile.SourcePrecondition `json:"source_precondition,omitempty"`
}

type DiscoveryPlan struct {
	ID            string                   `json:"id"`
	Effect        string                   `json:"effect"`
	ReadyToApply  bool                     `json:"ready_to_apply"`
	Readiness     string                   `json:"readiness"`
	Evidence      string                   `json:"evidence"`
	SourceMode    string                   `json:"source_mode"`
	Strategy      string                   `json:"strategy"`
	ClientMapping string                   `json:"client_mapping"`
	Operations    []DiscoveryPlanOperation `json:"operations"`
	Warnings      []string                 `json:"warnings"`
	Blockers      []string                 `json:"blockers"`
}

type DiscoveryResult struct {
	Effect              string             `json:"effect"`
	WritesPerformed     int                `json:"writes_performed"`
	AbsolutePathsShown  bool               `json:"absolute_paths_shown"`
	Torrent             DiscoveryTorrent   `json:"torrent"`
	SourceOutcome       string             `json:"source_outcome"`
	Selection           DiscoverySelection `json:"selection"`
	Handoff             DiscoveryHandoff   `json:"handoff"`
	BestEvidence        string             `json:"best_evidence"`
	Scan                DiscoveryScan      `json:"scan"`
	Files               []DiscoveryFile    `json:"files"`
	Matches             []DiscoveryMatch   `json:"matches"`
	Plan                *DiscoveryPlan     `json:"plan,omitempty"`
	Blockers            []DiscoveryBlocker `json:"blockers"`
	Warnings            []string           `json:"warnings"`
	verifiedSource      *metafile.VerifiedSource
	verifiedSelectionID string
}

// VerifiedSource returns the process-local proof retained by this discovery
// invocation. It is intentionally omitted from JSON, so a serialized report
// cannot be replayed later as content authority.
func (result *DiscoveryResult) VerifiedSource(meta *metafile.MetaInfo) (*metafile.VerifiedSource, bool) {
	if result == nil || result.SourceOutcome != "verified_unique" || result.verifiedSource == nil ||
		result.Selection.SelectedID == "" || result.Selection.SelectedID != result.verifiedSelectionID || !result.verifiedSource.Matches(meta) {
		return nil, false
	}
	return result.verifiedSource, true
}

// PublicReportCopy removes the process-local capability retained by Discover.
// The returned value can be embedded in a report or serialized without giving
// callers a way to recover absolute verified source paths through
// VerifiedSource. Public fields are sanitized separately by the presentation
// layer according to its path-disclosure policy.
func (result DiscoveryResult) PublicReportCopy() DiscoveryResult {
	result.verifiedSource = nil
	result.verifiedSelectionID = ""
	return result
}

type resolvedCandidate struct {
	observation storage.FileObservation
	path        string
	rank        int
}

// Discover performs bounded metadata and content reads only. It never creates
// an index, temporary layout, link, directory, or target file.
func Discover(ctx context.Context, meta *metafile.MetaInfo, options DiscoverOptions) (DiscoveryResult, error) {
	result := newDiscoveryResult(meta, options)
	if meta == nil {
		return result, fmt.Errorf("metafile is nil")
	}
	if len(options.SearchRoots) == 0 {
		return result, fmt.Errorf("at least one search root is required")
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
	if err := options.InventoryLimits.Validate(); err != nil {
		return result, err
	}
	if err := options.MatchLimits.Validate(); err != nil {
		return result, err
	}
	if len(meta.Files) > options.MatchLimits.MaxStates {
		matchResult, matchErr := metafile.MatchSourceCandidates(ctx, meta, nil, options.MatchLimits)
		if matchErr != nil {
			return result, matchErr
		}
		inventory := storage.InventoryResult{
			Complete:        false,
			PathConfinement: "not_started_manifest_state_budget",
			Limits:          options.InventoryLimits,
			Roots:           []storage.SearchRootObservation{},
			Candidates:      []storage.FileObservation{},
			LimitHits:       []string{},
			Issues:          []storage.ScanIssue{},
			Warnings:        []string{},
		}
		result.Scan = buildDiscoveryScan(inventory, matchResult, options.ShowAbsolutePaths)
		result.Scan.TimeBudgetMillis = options.TimeBudget.Milliseconds()
		result.Blockers = append(result.Blockers,
			DiscoveryBlocker{Code: "scan.not_started", Message: "storage scanning was not started because the manifest exceeds the candidate-state budget"},
			DiscoveryBlocker{Code: "source.verification_incomplete", Message: "candidate verification could not start within the configured state budget"},
		)
		if options.TargetRoot != "" || options.ClientMapping != nil {
			result.Handoff.Status = "blocked"
			result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "handoff.source_selection_blocked", Message: "the requested handoff requires one complete, uniquely verified source assignment"})
		}
		result.Warnings = append(result.Warnings, "discovery stopped before search-root inventory or content access; zero writes were intentionally performed")
		return result, nil
	}
	wantedSizes := manifestWantedSizes(meta)
	inventory, err := storage.InventoryCandidates(ctx, options.SearchRoots, storage.InventoryOptions{
		Limits:       options.InventoryLimits,
		WantedSizes:  wantedSizes,
		AllowNetwork: options.AllowNetwork,
	})
	if err != nil {
		return result, err
	}

	resolved, resolutionChanged := resolveInventoryCandidates(ctx, &inventory)
	sets, files := buildCandidateSets(ctx, meta, resolved, options.MatchLimits, options.ShowAbsolutePaths)
	matchResult, err := metafile.MatchSourceCandidates(ctx, meta, sets, options.MatchLimits)
	if err != nil {
		return result, err
	}
	if resolutionChanged {
		matchResult.Complete = false
		matchResult.StopReasons = appendUnique(matchResult.StopReasons, "candidate_resolution_changed")
	}

	result.Scan = buildDiscoveryScan(inventory, matchResult, options.ShowAbsolutePaths)
	result.Scan.TimeBudgetMillis = options.TimeBudget.Milliseconds()
	result.Files = files
	observationByID := make(map[string]resolvedCandidate, len(resolved))
	for _, candidate := range resolved {
		observationByID[candidate.observation.ObservationID] = candidate
	}
	result.Matches = make([]DiscoveryMatch, 0, len(matchResult.Matches))
	for _, match := range matchResult.Matches {
		result.Matches = append(result.Matches, buildDiscoveryMatch(meta, match, observationByID, options))
	}

	result.BestEvidence = "none"
	if len(result.Matches) > 0 {
		result.BestEvidence = "verified"
	} else if inventory.Stats.CandidatesRetained > 0 {
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
	case len(result.Matches) == 1:
		result.SourceOutcome = "verified_unique"
		result.Selection.Status = "ready"
		result.Selection.SelectedID = result.Matches[0].ID
		result.verifiedSource = matchResult.Matches[0].Source
		result.verifiedSelectionID = result.Selection.SelectedID
	default:
		result.SourceOutcome = "not_found"
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "source.no_verified_match", Message: "no retained source assignment passed exact torrent verification"})
	}
	if !result.Scan.Complete {
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "scan.incomplete", Message: "the storage scan was incomplete; uniqueness cannot be established"})
	}
	if !result.Scan.VerificationComplete {
		result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "source.verification_incomplete", Message: "candidate verification stopped at a configured safety budget or changing file"})
	}

	handoffRequested := options.ClientMapping != nil || options.TargetRoot != ""
	if handoffRequested {
		result.Handoff.Status = "blocked"
	}
	if options.ClientMapping != nil && options.TargetRoot == "" {
		if result.SourceOutcome == "verified_unique" && len(result.Matches) == 1 && result.Matches[0].Mapping.Status == "complete" {
			result.Handoff.Status = "ready"
		} else if result.SourceOutcome == "verified_unique" && len(result.Matches) == 1 {
			if len(result.Matches[0].Blockers) == 0 {
				result.Blockers = append(result.Blockers, DiscoveryBlocker{Code: "mapping.partial", Message: "the verified source could not be represented in the requested client namespace"})
			} else {
				result.Blockers = append(result.Blockers, result.Matches[0].Blockers...)
			}
		}
	}

	if options.TargetRoot != "" && result.SourceOutcome == "verified_unique" && len(matchResult.Matches) == 1 {
		plan, planErr := BuildMaterializePlanFromVerified(ctx, meta, matchResult.Matches[0].Source, options.TargetRoot, options.Strategy)
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

	result.Warnings = append(result.Warnings, inventory.Warnings...)
	result.Warnings = append(result.Warnings, "discovery performs metadata and content reads; zero writes were intentionally performed")
	result.Blockers = deduplicateBlockers(result.Blockers)
	return result, nil
}

func newDiscoveryResult(meta *metafile.MetaInfo, options DiscoverOptions) DiscoveryResult {
	result := DiscoveryResult{
		Effect:             "read_metadata+read_content",
		WritesPerformed:    0,
		AbsolutePathsShown: options.ShowAbsolutePaths,
		Selection:          DiscoverySelection{Status: "blocked"},
		SourceOutcome:      "incomplete",
		Handoff:            DiscoveryHandoff{Status: "not_requested"},
		BestEvidence:       "none",
		Files:              []DiscoveryFile{},
		Matches:            []DiscoveryMatch{},
		Blockers:           []DiscoveryBlocker{},
		Warnings:           []string{},
	}
	if meta != nil {
		result.Torrent = DiscoveryTorrent{
			Name:              meta.Name,
			Version:           meta.Version,
			MetafileVariantID: meta.MetafileVariantID,
			InfoHashV1:        meta.InfoHashV1,
			InfoHashV2:        meta.InfoHashV2,
			PhysicalBytes:     physicalManifestBytes(meta),
			Files:             len(meta.Files),
		}
	}
	return result
}

func manifestWantedSizes(meta *metafile.MetaInfo) []int64 {
	seen := make(map[int64]struct{})
	for _, file := range meta.Files {
		if strings.Contains(file.Attribute, "p") || file.Length == 0 {
			continue
		}
		seen[file.Length] = struct{}{}
	}
	result := make([]int64, 0, len(seen))
	for size := range seen {
		result = append(result, size)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func resolveInventoryCandidates(ctx context.Context, inventory *storage.InventoryResult) ([]resolvedCandidate, bool) {
	result := make([]resolvedCandidate, 0, len(inventory.Candidates))
	changed := false
	for _, observation := range inventory.Candidates {
		if err := ctx.Err(); err != nil {
			inventory.Complete = false
			inventory.LimitHits = appendUnique(inventory.LimitHits, "context_cancelled")
			return result, false
		}
		path, err := observation.ResolveObservedRegularContext(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				inventory.Complete = false
				inventory.LimitHits = appendUnique(inventory.LimitHits, "context_cancelled")
				return result, false
			}
			changed = true
			inventory.Complete = false
			inventory.LimitHits = appendUnique(inventory.LimitHits, "candidate_resolution_changed")
			if len(inventory.Issues) < inventory.Limits.MaxIssues {
				inventory.Issues = append(inventory.Issues, storage.ScanIssue{Code: "scan.candidate_changed", RootID: observation.RootID, RelativePath: observation.RelativePath, Message: "candidate changed after inventory"})
			} else {
				inventory.Stats.IssueOverflow++
			}
			continue
		}
		result = append(result, resolvedCandidate{observation: observation, path: path})
	}
	return result, changed
}

func buildCandidateSets(ctx context.Context, meta *metafile.MetaInfo, resolved []resolvedCandidate, limits metafile.SourceMatchLimits, showAbsolute bool) ([]metafile.FileCandidates, []DiscoveryFile) {
	return buildCandidateSetsRanked(ctx, meta, resolved, limits, showAbsolute, candidateRank)
}

func buildCandidateSetsRanked(
	ctx context.Context,
	meta *metafile.MetaInfo,
	resolved []resolvedCandidate,
	limits metafile.SourceMatchLimits,
	showAbsolute bool,
	rankCandidate func(metafile.File, storage.FileObservation) int,
) ([]metafile.FileCandidates, []DiscoveryFile) {
	ordered := append([]resolvedCandidate(nil), resolved...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].observation.SortKey() < ordered[j].observation.SortKey()
	})
	bySize := make(map[int64][]resolvedCandidate)
	for _, candidate := range ordered {
		bySize[candidate.observation.SizeBytes] = append(bySize[candidate.observation.SizeBytes], candidate)
	}
	sets := make([]metafile.FileCandidates, 0, len(meta.Files))
	files := make([]DiscoveryFile, 0, len(meta.Files))
	edgeCapacity := limits.MaxCandidateEdges + 1
	topLimit := limits.MaxCandidatesPerFile + 1
	for fileIndex, file := range meta.Files {
		discoveryFile := DiscoveryFile{FileIndex: fileIndex, TorrentPath: strings.Join(file.Path, "/"), Length: file.Length, Candidates: []DiscoveryCandidate{}}
		if strings.Contains(file.Attribute, "p") {
			discoveryFile.Requirement = "virtual_padding"
			files = append(files, discoveryFile)
			continue
		}
		if file.Length == 0 {
			discoveryFile.Requirement = "virtual_empty"
			files = append(files, discoveryFile)
			continue
		}
		discoveryFile.Requirement = "physical_source"
		bucket := bySize[file.Length]
		discoveryFile.CandidateCount = len(bucket)
		stopReason := ""
		considerLimit := 0
		if err := ctx.Err(); err != nil {
			stopReason = "context_cancelled"
		} else {
			considerLimit = len(bucket)
			if considerLimit > edgeCapacity {
				considerLimit = edgeCapacity
			}
		}
		top := make([]resolvedCandidate, 0, minInt(considerLimit, topLimit))
		considered := 0
		for candidateIndex := 0; candidateIndex < considerLimit; candidateIndex++ {
			if err := ctx.Err(); err != nil {
				stopReason = "context_cancelled"
				break
			}
			candidate := bucket[candidateIndex]
			candidate.rank = rankCandidate(file, candidate.observation)
			top = insertRankedCandidate(top, candidate, topLimit)
			considered++
		}
		edgeCapacity -= considered
		if stopReason == "" && considered < len(bucket) {
			stopReason = "max_candidate_edges"
		}
		discoveryFile.CandidatesTruncated = considered < len(bucket) || len(top) > limits.MaxCandidatesPerFile
		visible := len(top)
		if visible > limits.MaxCandidatesPerFile {
			visible = limits.MaxCandidatesPerFile
		}
		set := metafile.FileCandidates{
			FileIndex:             fileIndex,
			Candidates:            make([]metafile.SourceCandidate, 0, len(top)),
			EdgesConsidered:       considered,
			PreparationStopReason: stopReason,
		}
		for candidateIndex, candidate := range top {
			observation := candidate.observation
			set.Candidates = append(set.Candidates, metafile.SourceCandidate{
				ID:   observation.ObservationID,
				Rank: candidate.rank,
				Path: candidate.path,
				Open: func() (*os.File, error) {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					return observation.OpenObservedRegularContext(ctx)
				},
			})
			if candidateIndex < visible {
				discoveryFile.Candidates = append(discoveryFile.Candidates, publicCandidate(candidate, showAbsolute))
			}
		}
		sets = append(sets, set)
		files = append(files, discoveryFile)
	}
	return sets, files
}

func insertRankedCandidate(items []resolvedCandidate, candidate resolvedCandidate, limit int) []resolvedCandidate {
	position := sort.Search(len(items), func(index int) bool {
		return !resolvedCandidateLess(items[index], candidate)
	})
	if position >= limit {
		return items
	}
	items = append(items, resolvedCandidate{})
	copy(items[position+1:], items[position:])
	items[position] = candidate
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func resolvedCandidateLess(left, right resolvedCandidate) bool {
	if left.rank != right.rank {
		return left.rank < right.rank
	}
	return left.observation.SortKey() < right.observation.SortKey()
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func candidateRank(file metafile.File, observation storage.FileObservation) int {
	raw := observation.RelativeComponentsRawBase64
	if len(raw) >= len(file.PathRawBase64) {
		suffix := raw[len(raw)-len(file.PathRawBase64):]
		if equalStrings(suffix, file.PathRawBase64) {
			return 0
		}
	}
	if len(raw) > 0 && len(file.PathRawBase64) > 0 && raw[len(raw)-1] == file.PathRawBase64[len(file.PathRawBase64)-1] {
		return 1
	}
	if strings.EqualFold(observation.Basename(), filepath.Base(strings.Join(file.Path, string(filepath.Separator)))) {
		return 2
	}
	return 3
}

func publicCandidate(candidate resolvedCandidate, showAbsolute bool) DiscoveryCandidate {
	result := DiscoveryCandidate{
		ID:                          candidate.observation.ObservationID,
		RootID:                      candidate.observation.RootID,
		RelativePath:                candidate.observation.RelativePath,
		RelativeComponentsRawBase64: append([]string(nil), candidate.observation.RelativeComponentsRawBase64...),
		EvidenceLevel:               "candidate",
		EvidenceBasis:               "exact_size",
		MatchRank:                   candidateRankName(candidate.rank),
	}
	if showAbsolute {
		result.AbsolutePath = candidate.path
	}
	return result
}

func buildDiscoveryScan(inventory storage.InventoryResult, matches metafile.SourceMatchResult, showAbsolute bool) DiscoveryScan {
	roots := make([]DiscoveryRoot, len(inventory.Roots))
	for i, root := range inventory.Roots {
		roots[i] = DiscoveryRoot{ID: root.ID, Status: root.Status}
		if showAbsolute {
			roots[i].InputPath = root.InputPath
			roots[i].ResolvedPath = root.ResolvedPath
		}
	}
	reasons := append([]string{}, inventory.LimitHits...)
	for _, reason := range matches.StopReasons {
		reasons = appendUnique(reasons, reason)
	}
	sort.Strings(reasons)
	return DiscoveryScan{
		Complete:             inventory.Complete,
		VerificationComplete: matches.Complete,
		PathConfinement:      inventory.PathConfinement,
		SearchRoots:          roots,
		InventoryLimits:      inventory.Limits,
		MatchLimits:          matches.Limits,
		InventoryUsed:        inventory.Stats,
		MatchUsed:            matches.Stats,
		StopReasons:          reasons,
		InventoryIssues:      append([]storage.ScanIssue{}, inventory.Issues...),
		MatchIssues:          append([]metafile.SourceMatchIssue{}, matches.Issues...),
	}
}

func buildDiscoveryMatch(meta *metafile.MetaInfo, match metafile.SourceMatch, observations map[string]resolvedCandidate, options DiscoverOptions) DiscoveryMatch {
	result := DiscoveryMatch{
		ID:            match.ID,
		EvidenceLevel: "verified",
		EvidenceBasis: []string{},
		Coverage: DiscoveryCoverage{
			FilesExpected: nonPaddingFileCount(meta),
			FilesFound:    nonPaddingFileCount(meta),
			BytesExpected: physicalManifestBytes(meta),
			BytesFound:    physicalManifestBytes(meta),
		},
		Bindings:     []DiscoveryBinding{},
		Verification: match.Verification,
		Mapping:      DiscoveryMapping{Status: "not_requested"},
		Blockers:     []DiscoveryBlocker{},
	}
	for _, check := range match.Verification.Checks {
		result.EvidenceBasis = append(result.EvidenceBasis, check.Algorithm)
	}
	for _, binding := range match.Bindings {
		candidate, ok := observations[binding.CandidateID]
		if !ok {
			continue
		}
		discoveryBinding := DiscoveryBinding{
			FileIndex:                   binding.FileIndex,
			TorrentPath:                 strings.Join(meta.Files[binding.FileIndex].Path, "/"),
			CandidateID:                 binding.CandidateID,
			RootID:                      candidate.observation.RootID,
			RelativePath:                candidate.observation.RelativePath,
			RelativeComponentsRawBase64: append([]string(nil), candidate.observation.RelativeComponentsRawBase64...),
		}
		if options.ShowAbsolutePaths {
			discoveryBinding.AbsolutePath = candidate.path
		}
		if options.ClientMapping != nil && options.TargetRoot == "" {
			mapping, err := storage.MapHostToClient(options.ClientMapping.HostRoot, candidate.path, options.ClientMapping.ClientRoot, options.ClientMapping.ClientWindows)
			if err != nil {
				result.Mapping.Status = "partial"
				blocker, status := mappingFailure(err, false)
				result.Mapping.Status = status
				result.Blockers = append(result.Blockers, blocker)
			} else {
				discoveryBinding.ClientPath = mapping.ClientPath
			}
		}
		result.Bindings = append(result.Bindings, discoveryBinding)
	}
	if options.ClientMapping != nil && options.TargetRoot == "" && result.Mapping.Status == "not_requested" {
		result.Mapping = DiscoveryMapping{Status: "complete", Evidence: "lexical_only", ClientReachable: "unknown"}
	} else if options.ClientMapping != nil && options.TargetRoot == "" {
		result.Mapping.Evidence = "lexical_only"
		result.Mapping.ClientReachable = "unknown"
	}
	result.Layout = classifyLayout(meta, result.Bindings)
	if !meta.MultiFile && len(result.Bindings) == 1 {
		matches := candidateRank(meta.Files[0], observations[result.Bindings[0].CandidateID].observation) <= 1
		result.BasenameMatch = &matches
	}
	return result
}

func mappingFailure(err error, target bool) (DiscoveryBlocker, string) {
	scope := "verified source"
	codePrefix := "mapping.source"
	if target {
		scope = "planned target"
		codePrefix = "mapping.target"
	}
	switch {
	case errors.Is(err, storage.ErrHostPathOutsideRoot):
		return DiscoveryBlocker{Code: codePrefix + "_outside_host_root", Message: "the " + scope + " is outside the configured host namespace root"}, "failed_outside_host_root"
	case errors.Is(err, storage.ErrClientPathUnrepresentable):
		return DiscoveryBlocker{Code: codePrefix + "_unrepresentable", Message: "the " + scope + " cannot be represented under the configured client path semantics"}, "failed_unrepresentable"
	case errors.Is(err, storage.ErrInvalidClientRoot):
		return DiscoveryBlocker{Code: "mapping.invalid_client_root", Message: "the configured client namespace root is invalid"}, "failed_invalid_client_root"
	default:
		return DiscoveryBlocker{Code: codePrefix + "_failed", Message: "the " + scope + " could not be mapped to the client namespace"}, "failed"
	}
}

func publicDiscoveryPlan(meta *metafile.MetaInfo, plan Plan, observations map[string]resolvedCandidate, showAbsolute bool) *DiscoveryPlan {
	sourceLabels := make(map[string]string)
	for _, candidate := range observations {
		label := candidate.observation.RootID + ":" + candidate.observation.RelativePath
		if showAbsolute {
			label = candidate.path
		}
		sourceLabels[filepath.Clean(candidate.path)] = label
	}
	result := &DiscoveryPlan{
		ID:            plan.ID,
		Effect:        plan.Effect,
		ReadyToApply:  plan.ReadyToApply,
		Readiness:     plan.Readiness,
		Evidence:      plan.Evidence,
		SourceMode:    plan.SourceMode,
		Strategy:      plan.Strategy,
		ClientMapping: plan.ClientMapping,
		Operations:    []DiscoveryPlanOperation{},
		Warnings:      append([]string{}, plan.Warnings...),
		Blockers:      append([]string{}, plan.Blockers...),
	}
	for _, operation := range plan.Operations {
		source := sourceLabels[filepath.Clean(operation.Source)]
		if operation.Source != "" && source == "" {
			source = "verified-source:" + fmt.Sprint(operation.ManifestIndex)
		}
		target := publicTargetPath(meta, operation.ManifestIndex)
		if showAbsolute {
			target = operation.Target
		}
		result.Operations = append(result.Operations, DiscoveryPlanOperation{
			ManifestIndex: operation.ManifestIndex,
			TorrentPath:   operation.TorrentPath,
			Kind:          operation.Kind,
			Source:        source,
			Target:        target,
			ClientTarget:  operation.ClientTarget,
			Bytes:         operation.Bytes,
			Precondition:  operation.SourcePrecondition,
		})
	}
	return result
}

func publicTargetPath(meta *metafile.MetaInfo, fileIndex int) string {
	if fileIndex < 0 || fileIndex >= len(meta.Files) {
		return ""
	}
	if meta.MultiFile {
		return strings.Join(append([]string{meta.Name}, meta.Files[fileIndex].Path...), "/")
	}
	return meta.Name
}

func classifyLayout(meta *metafile.MetaInfo, bindings []DiscoveryBinding) string {
	if !meta.MultiFile {
		return "single_file"
	}
	if len(bindings) == 0 {
		return "scattered_set"
	}
	rootID := bindings[0].RootID
	var commonPrefix []string
	for i, binding := range bindings {
		if binding.RootID != rootID || binding.FileIndex < 0 || binding.FileIndex >= len(meta.Files) {
			return "scattered_set"
		}
		raw := binding.RelativeComponentsRawBase64
		manifest := meta.Files[binding.FileIndex].PathRawBase64
		if len(raw) < len(manifest) || !equalStrings(raw[len(raw)-len(manifest):], manifest) {
			return "scattered_set"
		}
		prefix := raw[:len(raw)-len(manifest)]
		if i == 0 {
			commonPrefix = append([]string(nil), prefix...)
		} else if !equalStrings(commonPrefix, prefix) {
			return "scattered_set"
		}
	}
	return "cohesive_root"
}

func physicalManifestBytes(meta *metafile.MetaInfo) int64 {
	var total int64
	for _, file := range meta.Files {
		if strings.Contains(file.Attribute, "p") {
			continue
		}
		total += file.Length
	}
	return total
}

func nonPaddingFileCount(meta *metafile.MetaInfo) int {
	count := 0
	for _, file := range meta.Files {
		if !strings.Contains(file.Attribute, "p") {
			count++
		}
	}
	return count
}

func candidateRankName(rank int) string {
	switch rank {
	case 0:
		return "exact_relative_suffix"
	case 1:
		return "exact_basename"
	case 2:
		return "case_folded_basename"
	default:
		return "size_only"
	}
}

func appendUnique(items []string, item string) []string {
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	return append(items, item)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func deduplicateBlockers(items []DiscoveryBlocker) []DiscoveryBlocker {
	seen := make(map[string]struct{}, len(items))
	result := make([]DiscoveryBlocker, 0, len(items))
	for _, item := range items {
		if _, ok := seen[item.Code]; ok {
			continue
		}
		seen[item.Code] = struct{}{}
		result = append(result, item)
	}
	return result
}
