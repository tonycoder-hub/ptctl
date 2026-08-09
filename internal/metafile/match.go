package metafile

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	DefaultMaxCandidatesPerFile = 64
	DefaultMaxCandidateEdges    = 50_000
	DefaultMaxCandidateStates   = 10_000
	DefaultMaxVerifiedLayouts   = 20
	DefaultMaxProofWorkBytes    = int64(1) << 40 // 1 TiB
	DefaultMaxMatchIssues       = 50

	hardMaxCandidatesPerFile = 256
	hardMaxCandidateEdges    = 250_000
	hardMaxCandidateStates   = 100_000
	hardMaxVerifiedLayouts   = 100
	hardMaxProofWorkBytes    = int64(1) << 50 // 1 PiB
	hardMaxMatchIssues       = 200
)

type SourceCandidate struct {
	ID   string                   `json:"id"`
	Rank int                      `json:"rank"`
	Path string                   `json:"-"`
	Open func() (*os.File, error) `json:"-"`
}

type FileCandidates struct {
	FileIndex             int               `json:"file_index"`
	Candidates            []SourceCandidate `json:"candidates"`
	EdgesConsidered       int               `json:"-"`
	PreparationStopReason string            `json:"-"`
}

type SourceMatchLimits struct {
	MaxCandidatesPerFile int   `json:"max_candidates_per_file"`
	MaxCandidateEdges    int   `json:"max_candidate_edges"`
	MaxStates            int   `json:"max_candidate_states"`
	MaxVerifiedLayouts   int   `json:"max_verified_layouts"`
	MaxProofWorkBytes    int64 `json:"max_proof_work_bytes"`
	MaxIssues            int   `json:"max_issues"`
}

func DefaultSourceMatchLimits() SourceMatchLimits {
	return SourceMatchLimits{
		MaxCandidatesPerFile: DefaultMaxCandidatesPerFile,
		MaxCandidateEdges:    DefaultMaxCandidateEdges,
		MaxStates:            DefaultMaxCandidateStates,
		MaxVerifiedLayouts:   DefaultMaxVerifiedLayouts,
		MaxProofWorkBytes:    DefaultMaxProofWorkBytes,
		MaxIssues:            DefaultMaxMatchIssues,
	}
}

func (limits SourceMatchLimits) Validate() error {
	checks := []struct {
		name  string
		value int64
		hard  int64
	}{
		{"max candidates per file", int64(limits.MaxCandidatesPerFile), hardMaxCandidatesPerFile},
		{"max candidate edges", int64(limits.MaxCandidateEdges), hardMaxCandidateEdges},
		{"max candidate states", int64(limits.MaxStates), hardMaxCandidateStates},
		{"max verified layouts", int64(limits.MaxVerifiedLayouts), hardMaxVerifiedLayouts},
		{"max proof work bytes", limits.MaxProofWorkBytes, hardMaxProofWorkBytes},
		{"max match issues", int64(limits.MaxIssues), hardMaxMatchIssues},
	}
	for _, check := range checks {
		if check.value <= 0 || check.value > check.hard {
			return fmt.Errorf("%s must be in 1..%d", check.name, check.hard)
		}
	}
	return nil
}

type SourceMatchStats struct {
	CandidateEdgesConsidered int   `json:"candidate_edges_considered"`
	V2RootsObserved          int   `json:"v2_roots_observed"`
	V2CandidatesRejected     int   `json:"v2_candidates_rejected"`
	StatesExplored           int   `json:"states_explored"`
	FullVerifications        int   `json:"full_verifications"`
	LayoutsRejected          int   `json:"layouts_rejected"`
	VerifiedLayouts          int   `json:"verified_layouts"`
	ProofWorkBytesCharged    int64 `json:"proof_work_bytes_charged"`
	IssueOverflow            int   `json:"issue_overflow"`
}

type SourceMatchIssue struct {
	Code        string `json:"code"`
	CandidateID string `json:"candidate_id,omitempty"`
	Message     string `json:"message"`
}

type MatchedBinding struct {
	FileIndex   int    `json:"file_index"`
	CandidateID string `json:"candidate_id"`
}

type SourceMatch struct {
	ID           string             `json:"id"`
	Bindings     []MatchedBinding   `json:"bindings"`
	Verification VerificationResult `json:"verification"`
	Source       *VerifiedSource    `json:"-"`
}

type SourceMatchResult struct {
	Complete    bool               `json:"complete"`
	Limits      SourceMatchLimits  `json:"limits"`
	Stats       SourceMatchStats   `json:"stats"`
	StopReasons []string           `json:"stop_reasons"`
	Issues      []SourceMatchIssue `json:"issues"`
	Matches     []SourceMatch      `json:"matches"`
}

type candidateCacheEntry struct {
	path   string
	length int64
	root   string
	valid  bool
}

type sourceMatcher struct {
	ctx      context.Context
	meta     *MetaInfo
	limits   SourceMatchLimits
	result   SourceMatchResult
	byFile   map[int][]SourceCandidate
	v2Cache  map[string]candidateCacheEntry
	bindings []SourceBinding
	matched  []MatchedBinding
	stop     bool
}

// MatchSourceCandidates searches source assignments without materializing a
// Cartesian product. v2 roots are exact per-file filters; v1-capable torrents
// are explored in manifest order and rejected as soon as a completed piece
// mismatches. Every returned layout then passes the ordinary mapped verifier.
func MatchSourceCandidates(ctx context.Context, meta *MetaInfo, sets []FileCandidates, limits SourceMatchLimits) (SourceMatchResult, error) {
	result := SourceMatchResult{Complete: true, Limits: limits, StopReasons: []string{}, Issues: []SourceMatchIssue{}, Matches: []SourceMatch{}}
	if meta == nil {
		return result, fmt.Errorf("metafile is nil")
	}
	if err := limits.Validate(); err != nil {
		return result, err
	}
	// Every manifest entry consumes at least one solver transition. Reject an
	// impossible depth before allocating binding prefixes, filtering, hashing,
	// or recursive search.
	if len(meta.Files) > limits.MaxStates {
		result.Complete = false
		result.StopReasons = append(result.StopReasons, "max_candidate_states")
		return result, nil
	}
	matcher := &sourceMatcher{
		ctx:      ctx,
		meta:     meta,
		limits:   limits,
		result:   result,
		byFile:   make(map[int][]SourceCandidate),
		v2Cache:  make(map[string]candidateCacheEntry),
		bindings: make([]SourceBinding, 0, len(meta.Files)),
		matched:  make([]MatchedBinding, 0, len(meta.Files)),
	}
	if err := matcher.prepare(sets); err != nil {
		return matcher.result, err
	}
	if err := ctx.Err(); err != nil {
		matcher.incomplete("context_cancelled")
		matcher.stop = true
	}
	if matcher.stop {
		return matcher.finish(), nil
	}
	if meta.InfoHashV2 != "" {
		matcher.filterV2()
	}
	if matcher.stop || matcher.hasMissingCandidates() {
		return matcher.finish(), nil
	}
	if meta.InfoHashV1 != "" {
		matcher.searchV1(0, newV1CandidateState())
	} else {
		matcher.searchV2Layouts(0)
	}
	return matcher.finish(), nil
}

func (matcher *sourceMatcher) prepare(sets []FileCandidates) error {
	if len(sets) > len(matcher.meta.Files) {
		return fmt.Errorf("candidate set count exceeds the manifest file count")
	}
	sets = append([]FileCandidates(nil), sets...)
	sort.Slice(sets, func(i, j int) bool { return sets[i].FileIndex < sets[j].FileIndex })
	seenSet := make(map[int]struct{}, len(sets))
	seenID := make(map[string]candidateCacheEntry)
	edgesRemaining := matcher.limits.MaxCandidateEdges
	for _, set := range sets {
		if set.FileIndex < 0 || set.FileIndex >= len(matcher.meta.Files) {
			return fmt.Errorf("candidate file index %d is outside the manifest", set.FileIndex)
		}
		file := matcher.meta.Files[set.FileIndex]
		if strings.Contains(file.Attribute, "p") || file.Length == 0 {
			return fmt.Errorf("manifest file %d does not require candidates", set.FileIndex)
		}
		if _, exists := seenSet[set.FileIndex]; exists {
			return fmt.Errorf("candidate set for manifest file %d is duplicated", set.FileIndex)
		}
		seenSet[set.FileIndex] = struct{}{}
		if len(set.Candidates) > hardMaxCandidatesPerFile+1 {
			return fmt.Errorf("candidate set for manifest file %d exceeds the process input cap", set.FileIndex)
		}
		edgesConsidered := set.EdgesConsidered
		if edgesConsidered == 0 && set.PreparationStopReason == "" {
			edgesConsidered = len(set.Candidates)
		}
		if edgesConsidered < 0 || edgesConsidered < len(set.Candidates) || edgesConsidered > hardMaxCandidateEdges+1 {
			return fmt.Errorf("candidate edge count for manifest file %d is inconsistent", set.FileIndex)
		}
		if set.PreparationStopReason != "" {
			matcher.incomplete(set.PreparationStopReason)
			if set.PreparationStopReason == "context_cancelled" {
				matcher.stop = true
			}
		}
		allowedEdges := edgesConsidered
		if allowedEdges > edgesRemaining {
			allowedEdges = edgesRemaining
			matcher.incomplete("max_candidate_edges")
		}
		if matcher.result.Stats.CandidateEdgesConsidered <= matcher.limits.MaxCandidateEdges {
			increment := edgesConsidered
			capacity := matcher.limits.MaxCandidateEdges + 1 - matcher.result.Stats.CandidateEdgesConsidered
			if increment > capacity {
				increment = capacity
			}
			matcher.result.Stats.CandidateEdgesConsidered += increment
		}
		if edgesRemaining == 0 || matcher.stop {
			matcher.byFile[set.FileIndex] = []SourceCandidate{}
			continue
		}
		candidates := append([]SourceCandidate(nil), set.Candidates...)
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].Rank != candidates[j].Rank {
				return candidates[i].Rank < candidates[j].Rank
			}
			if candidates[i].ID != candidates[j].ID {
				return candidates[i].ID < candidates[j].ID
			}
			return candidates[i].Path < candidates[j].Path
		})
		unique := candidates[:0]
		for _, candidate := range candidates {
			if candidate.ID == "" || candidate.Path == "" {
				return fmt.Errorf("candidate for manifest file %d has an empty ID or path", set.FileIndex)
			}
			if !filepath.IsAbs(candidate.Path) {
				return fmt.Errorf("candidate %q for manifest file %d is not absolute", candidate.ID, set.FileIndex)
			}
			if existing, ok := seenID[candidate.ID]; ok {
				if existing.path != candidate.Path || existing.length != file.Length {
					return fmt.Errorf("candidate ID %q refers to inconsistent paths or lengths", candidate.ID)
				}
			} else {
				seenID[candidate.ID] = candidateCacheEntry{path: candidate.Path, length: file.Length}
			}
			if len(unique) > 0 && unique[len(unique)-1].ID == candidate.ID {
				continue
			}
			unique = append(unique, candidate)
		}
		if len(unique) > matcher.limits.MaxCandidatesPerFile {
			unique = unique[:matcher.limits.MaxCandidatesPerFile]
			matcher.incomplete("max_candidates_per_file")
		}
		if len(unique) > allowedEdges {
			unique = unique[:allowedEdges]
		}
		edgesRemaining -= allowedEdges
		matcher.byFile[set.FileIndex] = unique
	}
	for fileIndex, file := range matcher.meta.Files {
		if strings.Contains(file.Attribute, "p") || file.Length == 0 {
			continue
		}
		if _, ok := matcher.byFile[fileIndex]; !ok {
			matcher.byFile[fileIndex] = []SourceCandidate{}
		}
	}
	return nil
}

func (matcher *sourceMatcher) filterV2() {
	for fileIndex, file := range matcher.meta.Files {
		if matcher.stop {
			return
		}
		if strings.Contains(file.Attribute, "p") || file.Length == 0 {
			continue
		}
		filtered := make([]SourceCandidate, 0, len(matcher.byFile[fileIndex]))
		for _, candidate := range matcher.byFile[fileIndex] {
			if matcher.stop {
				break
			}
			entry, cached := matcher.v2Cache[candidate.ID]
			if !cached {
				if !matcher.charge(file.Length, "max_proof_work_bytes") {
					break
				}
				observation, err := observeV2FileRoot(matcher.ctx, candidate.Path, file.Length, candidate.Open)
				matcher.result.Stats.V2RootsObserved++
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						matcher.incomplete("context_cancelled")
						matcher.stop = true
						break
					}
					matcher.incomplete("candidate_io_error")
					matcher.addIssue(SourceMatchIssue{Code: "candidate.v2_observation", CandidateID: candidate.ID, Message: "candidate changed or could not be read"})
					matcher.v2Cache[candidate.ID] = candidateCacheEntry{path: candidate.Path, length: file.Length, valid: false}
					continue
				}
				entry = candidateCacheEntry{path: candidate.Path, length: file.Length, root: observation.PiecesRoot, valid: true}
				matcher.v2Cache[candidate.ID] = entry
			}
			if entry.valid && entry.root == file.PiecesRoot {
				filtered = append(filtered, candidate)
			} else if entry.valid {
				matcher.result.Stats.V2CandidatesRejected++
			}
		}
		matcher.byFile[fileIndex] = filtered
	}
}

func (matcher *sourceMatcher) hasMissingCandidates() bool {
	for fileIndex, file := range matcher.meta.Files {
		if strings.Contains(file.Attribute, "p") || file.Length == 0 {
			continue
		}
		if len(matcher.byFile[fileIndex]) == 0 {
			return true
		}
	}
	return false
}

type v1CandidateState struct {
	pieceIndex int
	pieceBytes int64
	hasher     hash.Hash
}

func newV1CandidateState() *v1CandidateState {
	return &v1CandidateState{hasher: sha1.New()}
}

func cloneV1CandidateState(state *v1CandidateState) (*v1CandidateState, error) {
	marshaler, ok := state.hasher.(encoding.BinaryMarshaler)
	if !ok {
		return nil, fmt.Errorf("SHA-1 implementation cannot snapshot candidate state")
	}
	encoded, err := marshaler.MarshalBinary()
	if err != nil {
		return nil, err
	}
	clonedHasher := sha1.New()
	unmarshaler, ok := clonedHasher.(encoding.BinaryUnmarshaler)
	if !ok {
		return nil, fmt.Errorf("SHA-1 implementation cannot restore candidate state")
	}
	if err := unmarshaler.UnmarshalBinary(encoded); err != nil {
		return nil, err
	}
	return &v1CandidateState{pieceIndex: state.pieceIndex, pieceBytes: state.pieceBytes, hasher: clonedHasher}, nil
}

func (matcher *sourceMatcher) searchV1(fileIndex int, state *v1CandidateState) {
	if matcher.stop {
		return
	}
	if err := matcher.ctx.Err(); err != nil {
		matcher.incomplete("context_cancelled")
		matcher.stop = true
		return
	}
	if fileIndex == len(matcher.meta.Files) {
		completed, ok, err := matcher.finishV1State(state)
		if err != nil {
			matcher.incomplete("v1_state_error")
			matcher.addIssue(SourceMatchIssue{Code: "solver.v1_state", Message: "candidate proof state could not be finalized"})
			matcher.stop = true
			return
		}
		_ = completed
		if ok {
			matcher.verifyLayout()
		} else {
			matcher.result.Stats.LayoutsRejected++
		}
		return
	}
	file := matcher.meta.Files[fileIndex]
	if strings.Contains(file.Attribute, "p") {
		if !matcher.enterState() {
			return
		}
		// Padding is virtual, but hashing it is real proof work. Reserve the
		// entire transition before allocating a buffer or hashing any zeroes.
		if !matcher.charge(file.Length, "max_proof_work_bytes") {
			return
		}
		next, ok, err := matcher.advanceV1Zeros(state, file.Length)
		if err != nil {
			matcher.incomplete("v1_padding_error")
			matcher.addIssue(SourceMatchIssue{Code: "solver.v1_padding", Message: "virtual padding could not be evaluated"})
			matcher.stop = true
			return
		}
		if ok {
			matcher.searchV1(fileIndex+1, next)
		} else {
			matcher.result.Stats.LayoutsRejected++
		}
		return
	}
	if file.Length == 0 {
		if !matcher.enterState() {
			return
		}
		matcher.searchV1(fileIndex+1, state)
		return
	}
	for _, candidate := range matcher.byFile[fileIndex] {
		if matcher.stop || !matcher.enterState() {
			return
		}
		if !matcher.charge(file.Length, "max_proof_work_bytes") {
			return
		}
		next, ok, err := matcher.advanceV1File(state, candidate, file.Length)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				matcher.incomplete("context_cancelled")
				matcher.stop = true
				return
			}
			matcher.incomplete("candidate_io_error")
			matcher.addIssue(SourceMatchIssue{Code: "candidate.v1_observation", CandidateID: candidate.ID, Message: "candidate changed or could not be read"})
			continue
		}
		if !ok {
			matcher.result.Stats.LayoutsRejected++
			continue
		}
		matcher.pushBinding(fileIndex, candidate)
		matcher.searchV1(fileIndex+1, next)
		matcher.popBinding()
	}
}

func (matcher *sourceMatcher) searchV2Layouts(fileIndex int) {
	if matcher.stop {
		return
	}
	if err := matcher.ctx.Err(); err != nil {
		matcher.incomplete("context_cancelled")
		matcher.stop = true
		return
	}
	if fileIndex == len(matcher.meta.Files) {
		matcher.verifyLayout()
		return
	}
	file := matcher.meta.Files[fileIndex]
	if strings.Contains(file.Attribute, "p") || file.Length == 0 {
		if !matcher.enterState() {
			return
		}
		matcher.searchV2Layouts(fileIndex + 1)
		return
	}
	for _, candidate := range matcher.byFile[fileIndex] {
		if matcher.stop || !matcher.enterState() {
			return
		}
		matcher.pushBinding(fileIndex, candidate)
		matcher.searchV2Layouts(fileIndex + 1)
		matcher.popBinding()
	}
}

func (matcher *sourceMatcher) verifyLayout() {
	if matcher.stop {
		return
	}
	if !matcher.charge(matcher.fullVerificationWorkBytes(), "max_proof_work_bytes") {
		return
	}
	matcher.result.Stats.FullVerifications++
	verified, err := VerifySourceMap(matcher.ctx, matcher.meta, SourceMap{Bindings: matcher.bindings})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			matcher.incomplete("context_cancelled")
			matcher.stop = true
			return
		}
		matcher.incomplete("candidate_io_error")
		matcher.addIssue(SourceMatchIssue{Code: "candidate.final_verification", Message: "source layout changed or could not be read during final verification"})
		return
	}
	verification := verified.Result()
	if !verification.Verified {
		matcher.result.Stats.LayoutsRejected++
		return
	}
	matcher.result.Stats.VerifiedLayouts++
	if matcher.result.Stats.VerifiedLayouts > matcher.limits.MaxVerifiedLayouts {
		matcher.incomplete("max_verified_layouts")
		matcher.stop = true
		return
	}
	matchBindings := append([]MatchedBinding(nil), matcher.matched...)
	sort.Slice(matchBindings, func(i, j int) bool { return matchBindings[i].FileIndex < matchBindings[j].FileIndex })
	match := SourceMatch{
		ID:           sourceMatchID(matcher.meta.MetafileVariantID, matchBindings),
		Bindings:     matchBindings,
		Verification: verification,
		Source:       verified,
	}
	matcher.result.Matches = append(matcher.result.Matches, match)
}

func (matcher *sourceMatcher) advanceV1File(state *v1CandidateState, candidate SourceCandidate, length int64) (*v1CandidateState, bool, error) {
	specs, err := preflight(matcher.ctx, []fileSpec{{path: candidate.Path, length: length, open: candidate.Open}})
	if err != nil {
		return nil, false, err
	}
	spec := specs[0]
	file, err := openFileSpec(spec)
	if err != nil {
		return nil, false, err
	}
	before, err := statOpenedContentPath(spec.path, file)
	if err != nil {
		_ = file.Close()
		return nil, false, err
	}
	if !os.SameFile(spec.infoBefore, before) || before.Size() != spec.sizeBefore || !before.ModTime().Equal(spec.modBefore) {
		_ = file.Close()
		return nil, false, fmt.Errorf("candidate changed before hashing")
	}
	next, err := cloneV1CandidateState(state)
	if err != nil {
		_ = file.Close()
		return nil, false, err
	}
	buffer := make([]byte, 256<<10)
	remaining := length
	matched := true
	for remaining > 0 && matched {
		if err := matcher.ctx.Err(); err != nil {
			_ = file.Close()
			return nil, false, err
		}
		chunk := int64(len(buffer))
		if remaining < chunk {
			chunk = remaining
		}
		n, readErr := io.ReadFull(file, buffer[:int(chunk)])
		if readErr != nil {
			_ = file.Close()
			return nil, false, readErr
		}
		matched, err = matcher.feedV1(next, buffer[:n])
		if err != nil {
			_ = file.Close()
			return nil, false, err
		}
		remaining -= int64(n)
	}
	if matched {
		var extra [1]byte
		if n, readErr := file.Read(extra[:]); n != 0 || (readErr != nil && readErr != io.EOF) {
			_ = file.Close()
			return nil, false, fmt.Errorf("candidate exceeds expected length")
		}
	}
	after, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return nil, false, statErr
	}
	if !os.SameFile(before, after) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return nil, false, fmt.Errorf("candidate changed while hashing")
	}
	if closeErr != nil {
		return nil, false, closeErr
	}
	return next, matched, nil
}

func (matcher *sourceMatcher) advanceV1Zeros(state *v1CandidateState, count int64) (*v1CandidateState, bool, error) {
	next, err := cloneV1CandidateState(state)
	if err != nil {
		return nil, false, err
	}
	buffer := make([]byte, 16<<10)
	for count > 0 {
		if err := matcher.ctx.Err(); err != nil {
			return nil, false, err
		}
		chunk := int64(len(buffer))
		if count < chunk {
			chunk = count
		}
		matched, err := matcher.feedV1(next, buffer[:int(chunk)])
		if err != nil || !matched {
			return next, matched, err
		}
		count -= chunk
	}
	return next, true, nil
}

func (matcher *sourceMatcher) feedV1(state *v1CandidateState, data []byte) (bool, error) {
	for len(data) > 0 {
		remaining := matcher.meta.PieceLength - state.pieceBytes
		if remaining <= 0 {
			return false, fmt.Errorf("invalid candidate piece boundary")
		}
		chunk := len(data)
		if int64(chunk) > remaining {
			chunk = int(remaining)
		}
		if _, err := state.hasher.Write(data[:chunk]); err != nil {
			return false, err
		}
		state.pieceBytes += int64(chunk)
		data = data[chunk:]
		if state.pieceBytes == matcher.meta.PieceLength {
			if !matcher.finishV1Piece(state) {
				return false, nil
			}
		}
	}
	return true, nil
}

func (matcher *sourceMatcher) finishV1Piece(state *v1CandidateState) bool {
	if state.pieceIndex >= len(matcher.meta.pieceHashes) {
		return false
	}
	actualBytes := state.hasher.Sum(nil)
	var actual [sha1.Size]byte
	copy(actual[:], actualBytes)
	if actual != matcher.meta.pieceHashes[state.pieceIndex] {
		return false
	}
	state.pieceIndex++
	state.pieceBytes = 0
	state.hasher.Reset()
	return true
}

func (matcher *sourceMatcher) finishV1State(state *v1CandidateState) (*v1CandidateState, bool, error) {
	completed, err := cloneV1CandidateState(state)
	if err != nil {
		return nil, false, err
	}
	if completed.pieceBytes > 0 && !matcher.finishV1Piece(completed) {
		return completed, false, nil
	}
	return completed, completed.pieceIndex == len(matcher.meta.pieceHashes), nil
}

// fullVerificationWorkBytes returns the byte work performed by the authority
// verifier. A v1-capable proof hashes virtual padding as well as reading
// physical content; a pure v2 proof reads only physical files.
func (matcher *sourceMatcher) fullVerificationWorkBytes() int64 {
	var total int64
	for _, file := range matcher.meta.Files {
		if file.Length == 0 || (matcher.meta.InfoHashV1 == "" && strings.Contains(file.Attribute, "p")) {
			continue
		}
		if total > int64(^uint64(0)>>1)-file.Length {
			return int64(^uint64(0) >> 1)
		}
		total += file.Length
	}
	return total
}

func (matcher *sourceMatcher) charge(bytes int64, reason string) bool {
	if bytes < 0 || bytes > matcher.limits.MaxProofWorkBytes-matcher.result.Stats.ProofWorkBytesCharged {
		matcher.incomplete(reason)
		matcher.stop = true
		return false
	}
	matcher.result.Stats.ProofWorkBytesCharged += bytes
	return true
}

func (matcher *sourceMatcher) enterState() bool {
	if err := matcher.ctx.Err(); err != nil {
		matcher.incomplete("context_cancelled")
		matcher.stop = true
		return false
	}
	if matcher.result.Stats.StatesExplored >= matcher.limits.MaxStates {
		matcher.incomplete("max_candidate_states")
		matcher.stop = true
		return false
	}
	matcher.result.Stats.StatesExplored++
	return true
}

func (matcher *sourceMatcher) incomplete(reason string) {
	matcher.result.Complete = false
	for _, existing := range matcher.result.StopReasons {
		if existing == reason {
			return
		}
	}
	matcher.result.StopReasons = append(matcher.result.StopReasons, reason)
}

func (matcher *sourceMatcher) addIssue(issue SourceMatchIssue) {
	if len(matcher.result.Issues) < matcher.limits.MaxIssues {
		matcher.result.Issues = append(matcher.result.Issues, issue)
	} else {
		matcher.result.Stats.IssueOverflow++
	}
}

func (matcher *sourceMatcher) finish() SourceMatchResult {
	sort.Strings(matcher.result.StopReasons)
	sort.Slice(matcher.result.Matches, func(i, j int) bool { return matcher.result.Matches[i].ID < matcher.result.Matches[j].ID })
	return matcher.result
}

func (matcher *sourceMatcher) pushBinding(fileIndex int, candidate SourceCandidate) {
	matcher.bindings = append(matcher.bindings, SourceBinding{FileIndex: fileIndex, Path: candidate.Path, Open: candidate.Open})
	matcher.matched = append(matcher.matched, MatchedBinding{FileIndex: fileIndex, CandidateID: candidate.ID})
}

func (matcher *sourceMatcher) popBinding() {
	matcher.bindings = matcher.bindings[:len(matcher.bindings)-1]
	matcher.matched = matcher.matched[:len(matcher.matched)-1]
}

func sourceMatchID(variantID string, bindings []MatchedBinding) string {
	digest := sha256.New()
	fmt.Fprintln(digest, variantID)
	for _, binding := range bindings {
		fmt.Fprintf(digest, "%d\x00%s\n", binding.FileIndex, binding.CandidateID)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}
