package seed

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

func TestDiscoverFindsScatteredRenamedV1FilesAndBuildsZeroWritePlan(t *testing.T) {
	meta := discoverV1MultiMeta(t)
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeSeedFile(t, filepath.Join(rootA, "renamed-one.dat"), []byte("abc"))
	writeSeedFile(t, filepath.Join(rootB, "renamed-two.dat"), []byte("def"))
	target := t.TempDir()
	before, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}

	result, err := Discover(context.Background(), meta, defaultDiscoverOptions([]string{rootB, rootA}, target))
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "verified_unique" || result.Selection.Status != "ready" || result.Handoff.Status != "ready" || result.BestEvidence != "verified" || len(result.Matches) != 1 || result.Plan == nil || !result.Handoff.PlanProduced {
		t.Fatalf("unexpected discovery: %#v", result)
	}
	if result.Matches[0].Layout != "scattered_set" || result.Scan.MatchUsed.FullVerifications != 1 {
		t.Fatalf("scattered layout was not exact-verified efficiently: %#v", result.Matches[0])
	}
	if result.Plan.SourceMode != "discovered_map" || len(result.Plan.Operations) != 2 {
		t.Fatalf("unexpected mapped plan: %#v", result.Plan)
	}
	for _, operation := range result.Plan.Operations {
		if !strings.HasPrefix(operation.Source, "local:") || strings.Contains(operation.Source, rootA) || strings.Contains(operation.Source, rootB) || filepath.IsAbs(operation.Target) {
			t.Fatalf("default report leaked an absolute path: %#v", operation)
		}
	}
	after, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatal("discover created target files or directories")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if containsDecodedString(decoded, rootA) || containsDecodedString(decoded, rootB) || containsDecodedString(decoded, target) {
		t.Fatalf("absolute path leaked from privacy-preserving JSON: %s", encoded)
	}
}

func TestDiscoverReportsVerifiedAmbiguityWithoutSelecting(t *testing.T) {
	content := []byte("same")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	first := t.TempDir()
	second := t.TempDir()
	writeSeedFile(t, filepath.Join(first, "copy-one"), content)
	writeSeedFile(t, filepath.Join(second, "copy-two"), content)
	options := defaultDiscoverOptions([]string{first, second}, "")
	result, err := Discover(context.Background(), meta, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "verified_ambiguous" || result.Selection.Status != "blocked" || result.Selection.SelectedID != "" || len(result.Matches) != 2 || result.Plan != nil || !hasDiscoveryBlocker(result.Blockers, "source.multiple_verified_matches") {
		t.Fatalf("verified ambiguity was not fail-closed: %#v", result)
	}
	for _, match := range result.Matches {
		if match.EvidenceLevel != "verified" {
			t.Fatalf("ambiguity downgraded exact evidence: %#v", match)
		}
	}
}

func TestDiscoverKeepsProvenAmbiguityWhenAlternativeRetentionStops(t *testing.T) {
	content := []byte("same")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	first := t.TempDir()
	second := t.TempDir()
	writeSeedFile(t, filepath.Join(first, "copy-one"), content)
	writeSeedFile(t, filepath.Join(second, "copy-two"), content)
	options := defaultDiscoverOptions([]string{first, second}, "")
	options.MatchLimits.MaxVerifiedLayouts = 1
	result, err := Discover(context.Background(), meta, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "verified_ambiguous" || result.Selection.Status != "blocked" || result.Scan.VerificationComplete || result.Scan.MatchUsed.VerifiedLayouts != 2 || len(result.Matches) != 1 || !slices.Contains(result.Scan.StopReasons, "max_verified_layouts") {
		t.Fatalf("retention budget erased proven ambiguity: %#v", result)
	}
}

func TestDiscoverEntryBudgetNeverClaimsNoMatchCompleteness(t *testing.T) {
	content := []byte("x")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	root := t.TempDir()
	writeSeedFile(t, filepath.Join(root, "correct"), content)
	writeSeedFile(t, filepath.Join(root, "unrelated"), []byte("z"))
	options := defaultDiscoverOptions([]string{root}, "")
	options.InventoryLimits.MaxEntries = 1
	result, err := Discover(context.Background(), meta, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "incomplete" || result.Scan.Complete || result.Selection.Status != "blocked" || !hasDiscoveryBlocker(result.Blockers, "scan.incomplete") {
		t.Fatalf("partial scan looked conclusive: %#v", result)
	}
}

func TestDiscoverPreservesVerifiedEvidenceWhenTargetIsBlocked(t *testing.T) {
	content := []byte("content")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	root := t.TempDir()
	writeSeedFile(t, filepath.Join(root, "renamed"), content)
	target := t.TempDir()
	writeSeedFile(t, filepath.Join(target, "source.bin"), []byte("conflict"))
	result, err := Discover(context.Background(), meta, defaultDiscoverOptions([]string{root}, target))
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "verified_unique" || len(result.Matches) != 1 || result.Matches[0].EvidenceLevel != "verified" || result.Selection.Status != "ready" || result.Selection.SelectedID == "" || result.Handoff.Status != "blocked" || result.Handoff.PlanProduced || !hasDiscoveryBlocker(result.Blockers, "plan.target_blocked") {
		t.Fatalf("target conflict erased or overstated evidence: %#v", result)
	}
}

func TestDiscoverV2RejectsSameSizeDecoyAndMapsClientPath(t *testing.T) {
	content := []byte("hello")
	rootHash := sha256.Sum256(content)
	meta, err := metafile.Parse(encode(map[string]any{"info": map[string]any{
		"file tree": map[string]any{"file.bin": map[string]any{"": map[string]any{
			"length": int64(len(content)), "pieces root": rootHash[:],
		}}},
		"meta version": int64(2), "name": "file.bin", "piece length": int64(16384),
	}}))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeSeedFile(t, filepath.Join(root, "a-decoy"), []byte("jello"))
	writeSeedFile(t, filepath.Join(root, "z-renamed"), content)
	options := defaultDiscoverOptions([]string{root}, "")
	options.ClientMapping = &ClientMappingOptions{HostRoot: root, ClientRoot: "/downloads"}
	result, err := Discover(context.Background(), meta, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "verified_unique" || result.Selection.Status != "ready" || result.Handoff.Status != "ready" || len(result.Matches) != 1 || result.Scan.MatchUsed.V2CandidatesRejected != 1 {
		t.Fatalf("v2 decoy was not exactly filtered: %#v", result)
	}
	match := result.Matches[0]
	if match.Mapping.Status != "complete" || len(match.Bindings) != 1 || match.Bindings[0].ClientPath != "/downloads/z-renamed" {
		t.Fatalf("unexpected lexical mapping: %#v", match)
	}
}

func TestDiscoverV2FindsScatteredMultiFileLayoutAndBuildsPlan(t *testing.T) {
	rootA := sha256.Sum256([]byte{'a'})
	rootB := sha256.Sum256([]byte{'b'})
	meta, err := metafile.Parse(encode(map[string]any{"info": map[string]any{
		"file tree": map[string]any{
			"a": map[string]any{"": map[string]any{"length": int64(1), "pieces root": rootA[:]}},
			"b": map[string]any{"": map[string]any{"length": int64(1), "pieces root": rootB[:]}},
		},
		"meta version": int64(2), "name": "bundle", "piece length": int64(16384),
	}}))
	if err != nil {
		t.Fatal(err)
	}
	first := t.TempDir()
	second := t.TempDir()
	writeSeedFile(t, filepath.Join(first, "renamed-a"), []byte{'a'})
	writeSeedFile(t, filepath.Join(second, "renamed-b"), []byte{'b'})
	result, err := Discover(context.Background(), meta, defaultDiscoverOptions([]string{second, first}, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "verified_unique" || len(result.Matches) != 1 || result.Matches[0].Layout != "scattered_set" || result.Plan == nil || len(result.Plan.Operations) != 2 {
		t.Fatalf("pure-v2 scattered discovery did not produce an exact plan: %#v", result)
	}
}

func TestDiscoverRootOrderProducesStableIDsAndJSON(t *testing.T) {
	meta := discoverV1MultiMeta(t)
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeSeedFile(t, filepath.Join(rootA, "one"), []byte("abc"))
	writeSeedFile(t, filepath.Join(rootB, "two"), []byte("def"))
	target := t.TempDir()
	first, err := Discover(context.Background(), meta, defaultDiscoverOptions([]string{rootB, rootA}, target))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Discover(context.Background(), meta, defaultDiscoverOptions([]string{rootA, rootB}, target))
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) || first.Selection.SelectedID == "" || first.Selection.SelectedID != second.Selection.SelectedID || first.Plan == nil || second.Plan == nil || first.Plan.ID != second.Plan.ID {
		t.Fatalf("root order changed discovery identity:\n%s\n%s", firstJSON, secondJSON)
	}
}

func TestDiscoverIncompleteRootDoesNotClaimVerifiedUnique(t *testing.T) {
	content := []byte("same")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	root := t.TempDir()
	writeSeedFile(t, filepath.Join(root, "copy"), content)
	missing := filepath.Join(t.TempDir(), "missing")
	result, err := Discover(context.Background(), meta, defaultDiscoverOptions([]string{missing, root}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "incomplete" || result.Selection.Status != "blocked" || len(result.Matches) != 1 || result.BestEvidence != "verified" || result.Scan.Complete {
		t.Fatalf("partial root incorrectly established uniqueness: %#v", result)
	}
}

func TestDiscoverTargetMappingFailureKeepsSourceSelectionAndMarksPlan(t *testing.T) {
	content := []byte("content")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	root := t.TempDir()
	writeSeedFile(t, filepath.Join(root, "copy"), content)
	target := t.TempDir()
	options := defaultDiscoverOptions([]string{root}, target)
	options.ClientMapping = &ClientMappingOptions{HostRoot: root, ClientRoot: "/downloads"}
	result, err := Discover(context.Background(), meta, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "verified_unique" || result.Selection.Status != "ready" || result.Handoff.Status != "blocked" || !result.Handoff.PlanProduced || result.Plan == nil || result.Plan.ClientMapping != "failed_outside_host_root" || !hasDiscoveryBlocker(result.Blockers, "mapping.target_outside_host_root") {
		t.Fatalf("target mapping failure contract is inconsistent: %#v", result)
	}
}

func TestDiscoverHybridBuildsConjunctiveMappedPlan(t *testing.T) {
	piece0Bytes := append([]byte{'a'}, make([]byte, 16383)...)
	piece0 := sha1.Sum(piece0Bytes)
	piece1 := sha1.Sum([]byte{'b'})
	v1Pieces := append(append([]byte(nil), piece0[:]...), piece1[:]...)
	rootA := sha256.Sum256([]byte{'a'})
	rootB := sha256.Sum256([]byte{'b'})
	meta, err := metafile.Parse(encode(map[string]any{"info": map[string]any{
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
	root := t.TempDir()
	writeSeedFile(t, filepath.Join(root, "renamed-a"), []byte{'a'})
	writeSeedFile(t, filepath.Join(root, "renamed-b"), []byte{'b'})
	result, err := Discover(context.Background(), meta, defaultDiscoverOptions([]string{root}, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "verified_unique" || result.Handoff.Status != "ready" || result.Plan == nil || len(result.Matches) != 1 || len(result.Matches[0].Verification.Checks) != 2 || !result.Matches[0].Verification.Verified {
		t.Fatalf("hybrid discovery did not preserve conjunctive proof: %#v", result)
	}
	paddingOps := 0
	for _, operation := range result.Plan.Operations {
		if operation.Kind == "padding" {
			paddingOps++
		}
	}
	if paddingOps != 1 {
		t.Fatalf("hybrid plan lost virtual padding: %#v", result.Plan)
	}
}

func TestDiscoveryScanEmptyCollectionsEncodeAsArrays(t *testing.T) {
	content := []byte("x")
	meta := discoverV1SingleMeta(t, "source.bin", content)
	root := t.TempDir()
	writeSeedFile(t, filepath.Join(root, "copy"), content)
	result, err := Discover(context.Background(), meta, defaultDiscoverOptions([]string{root}, ""))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"\"stop_reasons\":[]", "\"inventory_issues\":[]", "\"match_issues\":[]"} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("stable empty array %s missing from %s", field, encoded)
		}
	}
}

func TestDiscoverCandidateEdgeBudgetIsIncomplete(t *testing.T) {
	meta := discoverV1MultiMeta(t)
	root := t.TempDir()
	writeSeedFile(t, filepath.Join(root, "one"), []byte("abc"))
	writeSeedFile(t, filepath.Join(root, "two"), []byte("def"))
	options := defaultDiscoverOptions([]string{root}, "")
	options.MatchLimits.MaxCandidateEdges = 1
	result, err := Discover(context.Background(), meta, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "incomplete" || result.Scan.VerificationComplete || !slices.Contains(result.Scan.StopReasons, "max_candidate_edges") || len(result.Matches) != 0 {
		t.Fatalf("candidate-edge budget was not exposed: %#v", result)
	}
	if result.Scan.MatchUsed.CandidateEdgesConsidered != 2 {
		t.Fatalf("candidate-edge overflow sentinel was not bounded to N+1: %#v", result.Scan.MatchUsed)
	}
}

func TestBuildCandidateSetsBoundsRankingWorkAndHonorsCancellation(t *testing.T) {
	meta := discoverManySameSizeMeta(t, 20)
	root := t.TempDir()
	for index := 0; index < 20; index++ {
		writeSeedFile(t, filepath.Join(root, fmt.Sprintf("candidate-%02d", index)), []byte{'x'})
	}
	inventory, err := storage.InventoryCandidates(context.Background(), []string{root}, storage.InventoryOptions{
		Limits:      storage.DefaultInventoryLimits(),
		WantedSizes: []int64{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, incomplete := resolveInventoryCandidates(context.Background(), &inventory)
	if incomplete || len(resolved) != 20 {
		t.Fatalf("unexpected candidate resolution: incomplete=%t candidates=%d", incomplete, len(resolved))
	}
	limits := metafile.DefaultSourceMatchLimits()
	limits.MaxCandidateEdges = 7
	limits.MaxCandidatesPerFile = 3
	ranked := 0
	sets, _ := buildCandidateSetsRanked(context.Background(), meta, resolved, limits, false, func(file metafile.File, observation storage.FileObservation) int {
		ranked++
		return candidateRank(file, observation)
	})
	considered := 0
	for _, set := range sets {
		considered += set.EdgesConsidered
		if len(set.Candidates) > limits.MaxCandidatesPerFile+1 {
			t.Fatalf("candidate top-K exceeded its bounded capacity: %d", len(set.Candidates))
		}
	}
	if ranked != limits.MaxCandidateEdges+1 || considered != limits.MaxCandidateEdges+1 {
		t.Fatalf("candidate preparation did unbounded work: ranked=%d considered=%d", ranked, considered)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ranked = 0
	cancelledSets, _ := buildCandidateSetsRanked(ctx, meta, resolved, limits, false, func(metafile.File, storage.FileObservation) int {
		ranked++
		return 0
	})
	if ranked != 0 || len(cancelledSets) == 0 || cancelledSets[0].PreparationStopReason != "context_cancelled" {
		t.Fatalf("cancelled candidate preparation continued: ranked=%d sets=%#v", ranked, cancelledSets)
	}

	cancelledInventory := inventory
	cancelledInventory.LimitHits = append([]string(nil), inventory.LimitHits...)
	cancelledInventory.Issues = append([]storage.ScanIssue(nil), inventory.Issues...)
	resolvedAfterCancel, resolutionChanged := resolveInventoryCandidates(ctx, &cancelledInventory)
	if resolutionChanged || len(resolvedAfterCancel) != 0 || !slices.Contains(cancelledInventory.LimitHits, "context_cancelled") {
		t.Fatalf("cancelled candidate resolution was not attributed to timeout: %#v", cancelledInventory)
	}
	for _, issue := range cancelledInventory.Issues {
		if issue.Code == "scan.candidate_changed" {
			t.Fatalf("cancellation was misreported as a changed file: %#v", cancelledInventory.Issues)
		}
	}
}

func TestDiscoverRejectsManifestDepthBeforeStorageAccess(t *testing.T) {
	meta := discoverManySameSizeMeta(t, 20)
	missing := filepath.Join(t.TempDir(), "must-not-be-opened")
	options := defaultDiscoverOptions([]string{missing}, "")
	options.MatchLimits.MaxStates = 7
	result, err := Discover(context.Background(), meta, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "incomplete" || result.Scan.PathConfinement != "not_started_manifest_state_budget" || result.Scan.InventoryUsed.EntriesExamined != 0 || !slices.Contains(result.Scan.StopReasons, "max_candidate_states") {
		t.Fatalf("manifest depth was not rejected before storage access: %#v", result)
	}
}

func defaultDiscoverOptions(roots []string, target string) DiscoverOptions {
	return DiscoverOptions{
		SearchRoots:     roots,
		InventoryLimits: storage.DefaultInventoryLimits(),
		MatchLimits:     metafile.DefaultSourceMatchLimits(),
		TargetRoot:      target,
		Strategy:        "copy",
	}
}

func discoverV1MultiMeta(t *testing.T) *metafile.MetaInfo {
	t.Helper()
	data := []byte("abcdef")
	piece0 := sha1.Sum(data[:4])
	piece1 := sha1.Sum(data[4:])
	pieces := append(piece0[:], piece1[:]...)
	meta, err := metafile.Parse(encode(map[string]any{"info": map[string]any{
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

func discoverV1SingleMeta(t *testing.T, name string, content []byte) *metafile.MetaInfo {
	t.Helper()
	piece := sha1.Sum(content)
	meta, err := metafile.Parse(encode(map[string]any{"info": map[string]any{
		"length": int64(len(content)), "name": name, "piece length": int64(len(content)), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func discoverManySameSizeMeta(t *testing.T, count int) *metafile.MetaInfo {
	t.Helper()
	files := make([]any, count)
	for index := range files {
		files[index] = map[string]any{"length": int64(1), "path": []any{fmt.Sprintf("file-%02d", index)}}
	}
	meta, err := metafile.Parse(encode(map[string]any{"info": map[string]any{
		"files": files, "name": "bundle", "piece length": int64(1), "pieces": make([]byte, count*sha1.Size),
	}}))
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func writeSeedFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func hasDiscoveryBlocker(items []DiscoveryBlocker, code string) bool {
	for _, item := range items {
		if item.Code == code {
			return true
		}
	}
	return false
}

func containsDecodedString(value any, secret string) bool {
	switch typed := value.(type) {
	case string:
		return strings.Contains(typed, secret)
	case []any:
		for _, item := range typed {
			if containsDecodedString(item, secret) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if containsDecodedString(item, secret) {
				return true
			}
		}
	}
	return false
}
