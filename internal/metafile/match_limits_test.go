package metafile

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func TestMatchSourceCandidatesBoundsTotalCandidateEdges(t *testing.T) {
	meta := testScatteredV1Meta(t)
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	writeTestFile(t, first, []byte("abc"))
	writeTestFile(t, second, []byte("def"))
	limits := DefaultSourceMatchLimits()
	limits.MaxCandidateEdges = 1
	result, err := MatchSourceCandidates(context.Background(), meta, []FileCandidates{
		{FileIndex: 1, Candidates: []SourceCandidate{{ID: "second", Path: second}}},
		{FileIndex: 0, Candidates: []SourceCandidate{{ID: "first", Path: first}}},
	}, limits)
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || len(result.Matches) != 0 || result.Stats.CandidateEdgesConsidered != 2 || !slices.Contains(result.StopReasons, "max_candidate_edges") {
		t.Fatalf("candidate-edge budget was not fail-closed: %#v", result)
	}
}

func TestMatchSourceCandidatesCancellationCannotBecomeCompleteNoMatch(t *testing.T) {
	meta := testScatteredV1Meta(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := MatchSourceCandidates(ctx, meta, nil, DefaultSourceMatchLimits())
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || !slices.Contains(result.StopReasons, "context_cancelled") {
		t.Fatalf("cancelled empty candidate search looked conclusive: %#v", result)
	}
}
