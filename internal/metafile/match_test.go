package metafile

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestVerifySourceMapSupportsScatteredV1Files(t *testing.T) {
	meta := testScatteredV1Meta(t)
	first := filepath.Join(t.TempDir(), "renamed-one.bin")
	second := filepath.Join(t.TempDir(), "renamed-two.bin")
	writeTestFile(t, first, []byte("abc"))
	writeTestFile(t, second, []byte("def"))
	verified, err := VerifySourceMap(context.Background(), meta, SourceMap{Bindings: []SourceBinding{
		{FileIndex: 1, Path: second},
		{FileIndex: 0, Path: first},
	}})
	if err != nil {
		t.Fatal(err)
	}
	result := verified.Result()
	if !result.Verified || result.PiecesMatched != 2 || result.BytesVerified != 6 {
		t.Fatalf("unexpected scattered verification: %#v", result)
	}
	if path, ok := verified.Path(0); !ok || path != first {
		t.Fatalf("normalized source binding missing: path=%q ok=%t", path, ok)
	}
}

func TestMatchSourceCandidatesPrunesV1PiecePrefixes(t *testing.T) {
	meta := testScatteredV1Meta(t)
	goodA := filepath.Join(t.TempDir(), "good-a")
	goodB := filepath.Join(t.TempDir(), "good-b")
	badA := filepath.Join(t.TempDir(), "bad-a")
	badB := filepath.Join(t.TempDir(), "bad-b")
	writeTestFile(t, goodA, []byte("abc"))
	writeTestFile(t, goodB, []byte("def"))
	writeTestFile(t, badA, []byte("xxx"))
	writeTestFile(t, badB, []byte("yyy"))

	result, err := MatchSourceCandidates(context.Background(), meta, []FileCandidates{
		{FileIndex: 0, Candidates: []SourceCandidate{{ID: "a-bad", Path: badA}, {ID: "a-good", Path: goodA}}},
		{FileIndex: 1, Candidates: []SourceCandidate{{ID: "b-bad", Path: badB}, {ID: "b-good", Path: goodB}}},
	}, DefaultSourceMatchLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.Matches) != 1 || result.Stats.FullVerifications != 1 {
		t.Fatalf("unexpected match result: %#v", result)
	}
	if result.Stats.StatesExplored > 6 || result.Stats.LayoutsRejected < 3 {
		t.Fatalf("v1 solver did not prune at piece boundaries: %#v", result.Stats)
	}
	bindings := result.Matches[0].Bindings
	if len(bindings) != 2 || bindings[0].CandidateID != "a-good" || bindings[1].CandidateID != "b-good" {
		t.Fatalf("wrong source map selected: %#v", bindings)
	}
}

func TestMatchSourceCandidatesUsesExactV2RootFilter(t *testing.T) {
	content := []byte("hello")
	root, _ := testV2BlockRoot(content)
	meta, err := Parse(testV2SingleFileTorrent("file.bin", int64(len(content)), 16384, root, nil))
	if err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(t.TempDir(), "renamed-good")
	bad := filepath.Join(t.TempDir(), "renamed-bad")
	writeTestFile(t, good, content)
	writeTestFile(t, bad, []byte("jello"))
	result, err := MatchSourceCandidates(context.Background(), meta, []FileCandidates{{FileIndex: 0, Candidates: []SourceCandidate{
		{ID: "bad", Path: bad}, {ID: "good", Path: good},
	}}}, DefaultSourceMatchLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.Matches) != 1 || result.Stats.V2RootsObserved != 2 || result.Stats.V2CandidatesRejected != 1 || result.Stats.FullVerifications != 1 {
		t.Fatalf("unexpected v2 filtering: %#v", result)
	}
	if got := result.Matches[0].Bindings[0].CandidateID; got != "good" {
		t.Fatalf("wrong v2 candidate matched: %q", got)
	}
}

func TestMatchSourceCandidatesCarriesIdentityBoundOpenerThroughV2Proof(t *testing.T) {
	content := []byte("hello")
	root, _ := testV2BlockRoot(content)
	meta, err := Parse(testV2SingleFileTorrent("file.bin", int64(len(content)), 16384, root, nil))
	if err != nil {
		t.Fatal(err)
	}
	actual := filepath.Join(t.TempDir(), "identity-bound")
	writeTestFile(t, actual, content)
	opens := 0
	result, err := MatchSourceCandidates(context.Background(), meta, []FileCandidates{{
		FileIndex: 0,
		Candidates: []SourceCandidate{{
			ID: "bound", Path: actual, Open: func() (*os.File, error) {
				opens++
				return os.Open(actual)
			},
		}},
	}}, DefaultSourceMatchLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.Matches) != 1 || result.Stats.V2RootsObserved != 1 || !result.Matches[0].Verification.Verified || opens == 0 {
		t.Fatalf("identity-bound opener was not used by filtering and final proof: %#v", result)
	}
}

func TestMatchSourceCandidatesStopsBeforeProofBudgetOverrun(t *testing.T) {
	content := []byte("hello")
	root, _ := testV2BlockRoot(content)
	meta, err := Parse(testV2SingleFileTorrent("file.bin", int64(len(content)), 16384, root, nil))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "file")
	writeTestFile(t, path, content)
	limits := DefaultSourceMatchLimits()
	limits.MaxProofWorkBytes = int64(len(content) - 1)
	result, err := MatchSourceCandidates(context.Background(), meta, []FileCandidates{{FileIndex: 0, Candidates: []SourceCandidate{{ID: "candidate", Path: path}}}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || len(result.Matches) != 0 || result.Stats.V2RootsObserved != 0 || result.Stats.ProofWorkBytesCharged != 0 || !slices.Contains(result.StopReasons, "max_proof_work_bytes") {
		t.Fatalf("proof budget was not fail-closed: %#v", result)
	}
}

func TestMatchSourceCandidatesReportsVerifiedAmbiguity(t *testing.T) {
	content := []byte("same")
	piece := sha1.Sum(content)
	meta, err := Parse(bencode(map[string]any{"info": map[string]any{
		"length": int64(len(content)), "name": "source.bin", "piece length": int64(len(content)), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	writeTestFile(t, first, content)
	writeTestFile(t, second, content)
	result, err := MatchSourceCandidates(context.Background(), meta, []FileCandidates{{FileIndex: 0, Candidates: []SourceCandidate{
		{ID: "first", Path: first}, {ID: "second", Path: second},
	}}}, DefaultSourceMatchLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.Matches) != 2 {
		t.Fatalf("distinct exact copies were not preserved as ambiguity: %#v", result)
	}
}

func TestMatchSourceCandidatesVerifiedLayoutLimitCountsOverflow(t *testing.T) {
	content := []byte("same")
	piece := sha1.Sum(content)
	meta, err := Parse(bencode(map[string]any{"info": map[string]any{
		"length": int64(len(content)), "name": "source.bin", "piece length": int64(len(content)), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	writeTestFile(t, first, content)
	writeTestFile(t, second, content)
	limits := DefaultSourceMatchLimits()
	limits.MaxVerifiedLayouts = 1

	t.Run("unique_at_limit_is_complete", func(t *testing.T) {
		result, err := MatchSourceCandidates(context.Background(), meta, []FileCandidates{{FileIndex: 0, Candidates: []SourceCandidate{
			{ID: "first", Path: first},
		}}}, limits)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Complete || len(result.Matches) != 1 || result.Stats.VerifiedLayouts != 1 || slices.Contains(result.StopReasons, "max_verified_layouts") {
			t.Fatalf("the retained-layout limit was mistaken for overflow: %#v", result)
		}
	})

	t.Run("second_verified_layout_is_fail_closed", func(t *testing.T) {
		result, err := MatchSourceCandidates(context.Background(), meta, []FileCandidates{{FileIndex: 0, Candidates: []SourceCandidate{
			{ID: "first", Path: first}, {ID: "second", Path: second},
		}}}, limits)
		if err != nil {
			t.Fatal(err)
		}
		if result.Complete || len(result.Matches) != 1 || result.Stats.VerifiedLayouts != 2 || result.Stats.FullVerifications != 2 || !slices.Contains(result.StopReasons, "max_verified_layouts") {
			t.Fatalf("verified-layout overflow was not detected after retaining one layout: %#v", result)
		}
	})
}

func TestMatchSourceCandidatesNeverExceedsStateBudget(t *testing.T) {
	content := []byte("same")
	piece := sha1.Sum(content)
	meta, err := Parse(bencode(map[string]any{"info": map[string]any{
		"length": int64(len(content)), "name": "source.bin", "piece length": int64(len(content)), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}
	candidates := make([]SourceCandidate, 0, 3)
	for _, id := range []string{"a", "b", "c"} {
		path := filepath.Join(t.TempDir(), id)
		writeTestFile(t, path, content)
		candidates = append(candidates, SourceCandidate{ID: id, Path: path})
	}
	limits := DefaultSourceMatchLimits()
	limits.MaxStates = 2
	result, err := MatchSourceCandidates(context.Background(), meta, []FileCandidates{{FileIndex: 0, Candidates: candidates}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || result.Stats.StatesExplored != 2 || result.Stats.StatesExplored > limits.MaxStates || !slices.Contains(result.StopReasons, "max_candidate_states") {
		t.Fatalf("state budget was exceeded or hidden: %#v", result)
	}
}

func TestMatchSourceCandidatesCountsVirtualManifestTransitions(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			meta := testEmptyMultiFileMeta(t, version, 2)
			limits := DefaultSourceMatchLimits()
			limits.MaxStates = 2
			result, err := MatchSourceCandidates(context.Background(), meta, nil, limits)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Complete || result.Stats.StatesExplored != 2 || len(result.Matches) != 1 {
				t.Fatalf("empty manifest transitions were not counted exactly once: %#v", result)
			}
		})
	}
}

func TestMatchSourceCandidatesRejectsManifestDeeperThanStateBudgetBeforeSearch(t *testing.T) {
	meta := testEmptyMultiFileMeta(t, "v2", 3)
	limits := DefaultSourceMatchLimits()
	limits.MaxStates = 2
	result, err := MatchSourceCandidates(context.Background(), meta, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || result.Stats.StatesExplored != 0 || result.Stats.ProofWorkBytesCharged != 0 || len(result.Matches) != 0 || !slices.Contains(result.StopReasons, "max_candidate_states") {
		t.Fatalf("over-deep manifest entered recursive search: %#v", result)
	}
}

func TestMatchSourceCandidatesChargesLargePaddingBeforeHashing(t *testing.T) {
	const paddingLength = int64(64 << 20)
	meta, err := Parse(bencode(map[string]any{"info": map[string]any{
		"files": []any{map[string]any{"attr": "p", "length": paddingLength, "path": []any{".pad", "large"}}},
		"name":  "bundle", "piece length": paddingLength, "pieces": make([]byte, sha1.Size),
	}}))
	if err != nil {
		t.Fatal(err)
	}
	limits := DefaultSourceMatchLimits()
	limits.MaxProofWorkBytes = 1
	result, err := MatchSourceCandidates(context.Background(), meta, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || result.Stats.StatesExplored != 1 || result.Stats.ProofWorkBytesCharged != 0 || result.Stats.FullVerifications != 0 || !slices.Contains(result.StopReasons, "max_proof_work_bytes") {
		t.Fatalf("large virtual padding began work before its budget reservation: %#v", result)
	}
}

func TestMatchSourceCandidatesChargesPaddingAgainForAuthorityProof(t *testing.T) {
	padding := []byte{0, 0, 0, 0}
	piece := sha1.Sum(padding)
	meta, err := Parse(bencode(map[string]any{"info": map[string]any{
		"files": []any{map[string]any{"attr": "p", "length": int64(len(padding)), "path": []any{".pad", "4"}}},
		"name":  "bundle", "piece length": int64(len(padding)), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}

	limits := DefaultSourceMatchLimits()
	limits.MaxProofWorkBytes = int64(len(padding))
	blocked, err := MatchSourceCandidates(context.Background(), meta, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Complete || blocked.Stats.ProofWorkBytesCharged != int64(len(padding)) || blocked.Stats.FullVerifications != 0 || len(blocked.Matches) != 0 || !slices.Contains(blocked.StopReasons, "max_proof_work_bytes") {
		t.Fatalf("authority proof performed uncharged padding work: %#v", blocked)
	}

	limits.MaxProofWorkBytes = int64(2 * len(padding))
	complete, err := MatchSourceCandidates(context.Background(), meta, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !complete.Complete || complete.Stats.ProofWorkBytesCharged != int64(2*len(padding)) || complete.Stats.FullVerifications != 1 || len(complete.Matches) != 1 {
		t.Fatalf("fully budgeted padding proof did not complete: %#v", complete)
	}
}

func TestMatchSourceCandidatesManySingleCandidateFilesRespectDepthBudget(t *testing.T) {
	const fileCount = 512
	content := make([]byte, fileCount)
	for i := range content {
		content[i] = 'x'
	}
	piece := sha1.Sum(content)
	files := make([]any, 0, fileCount)
	sets := make([]FileCandidates, 0, fileCount)
	path := filepath.Join(t.TempDir(), "one-byte-source")
	writeTestFile(t, path, []byte{'x'})
	for i := 0; i < fileCount; i++ {
		files = append(files, map[string]any{"length": int64(1), "path": []any{fmt.Sprintf("file-%04d", i)}})
		sets = append(sets, FileCandidates{FileIndex: i, Candidates: []SourceCandidate{{ID: "shared", Path: path}}})
	}
	meta, err := Parse(bencode(map[string]any{"info": map[string]any{
		"files": files, "name": "bundle", "piece length": int64(fileCount), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}

	limits := DefaultSourceMatchLimits()
	limits.MaxStates = fileCount
	complete, err := MatchSourceCandidates(context.Background(), meta, sets, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !complete.Complete || complete.Stats.StatesExplored != fileCount || len(complete.Matches) != 1 || len(complete.Matches[0].Bindings) != fileCount {
		t.Fatalf("single-path deep layout did not use one state per manifest file: %#v", complete)
	}

	limits.MaxStates = fileCount - 1
	blocked, err := MatchSourceCandidates(context.Background(), meta, sets, limits)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Complete || blocked.Stats.StatesExplored != 0 || blocked.Stats.ProofWorkBytesCharged != 0 || len(blocked.Matches) != 0 || !slices.Contains(blocked.StopReasons, "max_candidate_states") {
		t.Fatalf("manifest depth limit did not stop before proof search: %#v", blocked)
	}
}

func TestVerifySourceMapSynthesizesEmptyFiles(t *testing.T) {
	meta, err := Parse(bencode(map[string]any{"info": map[string]any{
		"length": int64(0), "name": "empty", "piece length": int64(1), "pieces": []byte{},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifySourceMap(context.Background(), meta, SourceMap{})
	if err != nil || !verified.Result().Verified || len(verified.Bindings()) != 0 {
		t.Fatalf("empty source observation=%#v err=%v", verified, err)
	}
}

func TestMatchSourceCandidatesHybridMappedLayoutUsesConjunctiveProof(t *testing.T) {
	piece0Bytes := append([]byte{'a'}, make([]byte, 16383)...)
	piece0 := sha1.Sum(piece0Bytes)
	piece1 := sha1.Sum([]byte{'b'})
	v1Pieces := append(append([]byte(nil), piece0[:]...), piece1[:]...)
	rootA := sha256.Sum256([]byte{'a'})
	rootB := sha256.Sum256([]byte{'b'})
	meta, err := Parse(bencode(map[string]any{"info": map[string]any{
		"file tree": map[string]any{
			"a": map[string]any{"": map[string]any{"length": int64(1), "pieces root": rootA[:]}},
			"b": map[string]any{"": map[string]any{"length": int64(1), "pieces root": rootB[:]}},
		},
		"files": []any{
			map[string]any{"length": int64(1), "path": []any{"a"}},
			map[string]any{"attr": "p", "length": int64(16383), "path": []any{".pad", "16383"}},
			map[string]any{"length": int64(1), "path": []any{"b"}},
		},
		"meta version": int64(2), "name": "bundle", "piece length": int64(16384), "pieces": v1Pieces,
	}}))
	if err != nil {
		t.Fatal(err)
	}
	goodA := filepath.Join(t.TempDir(), "renamed-a")
	goodB := filepath.Join(t.TempDir(), "renamed-b")
	badA := filepath.Join(t.TempDir(), "bad-a")
	badB := filepath.Join(t.TempDir(), "bad-b")
	writeTestFile(t, goodA, []byte{'a'})
	writeTestFile(t, goodB, []byte{'b'})
	writeTestFile(t, badA, []byte{'x'})
	writeTestFile(t, badB, []byte{'y'})
	result, err := MatchSourceCandidates(context.Background(), meta, []FileCandidates{
		{FileIndex: 0, Candidates: []SourceCandidate{{ID: "a-bad", Path: badA}, {ID: "a-good", Path: goodA}}},
		{FileIndex: 2, Candidates: []SourceCandidate{{ID: "b-bad", Path: badB}, {ID: "b-good", Path: goodB}}},
	}, DefaultSourceMatchLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.Matches) != 1 || result.Stats.V2CandidatesRejected != 2 {
		t.Fatalf("hybrid v2 pruning failed: %#v", result)
	}
	verification := result.Matches[0].Verification
	if !verification.Verified || len(verification.Checks) != 2 || !verification.Checks[0].Verified || !verification.Checks[1].Verified || verification.PaddingBytes != 16383 || verification.ProofStreamBytes != 16385 {
		t.Fatalf("hybrid mapped proof was not conjunctive: %#v", verification)
	}
}

func testScatteredV1Meta(t *testing.T) *MetaInfo {
	t.Helper()
	data := []byte("abcdef")
	piece0 := sha1.Sum(data[:4])
	piece1 := sha1.Sum(data[4:])
	pieces := append(piece0[:], piece1[:]...)
	meta, err := Parse(bencode(map[string]any{"info": map[string]any{
		"files": []any{
			map[string]any{"length": int64(3), "path": []any{"a.bin"}},
			map[string]any{"length": int64(3), "path": []any{"b.bin"}},
		},
		"name": "bundle", "piece length": int64(4), "pieces": pieces,
	}}))
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func testEmptyMultiFileMeta(t *testing.T, version string, count int) *MetaInfo {
	t.Helper()
	info := map[string]any{"name": "bundle"}
	switch version {
	case "v1":
		files := make([]any, 0, count)
		for i := 0; i < count; i++ {
			files = append(files, map[string]any{"length": int64(0), "path": []any{fmt.Sprintf("empty-%04d", i)}})
		}
		info["files"] = files
		info["piece length"] = int64(1)
		info["pieces"] = []byte{}
	case "v2":
		tree := make(map[string]any, count)
		for i := 0; i < count; i++ {
			tree[fmt.Sprintf("empty-%04d", i)] = map[string]any{"": map[string]any{"length": int64(0)}}
		}
		info["file tree"] = tree
		info["meta version"] = int64(2)
		info["piece length"] = int64(16384)
	default:
		t.Fatalf("unsupported empty metafile version %q", version)
	}
	meta, err := Parse(bencode(map[string]any{"info": info}))
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func writeTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}
