package materialize

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

func TestRunMaterializesSingleFileWithDoubleExactVerification(t *testing.T) {
	ctx := context.Background()
	content := []byte("materialize me")
	meta := materializeSingleV1Meta(t, "final.bin", content)
	searchRoot := t.TempDir()
	sourcePath := filepath.Join(searchRoot, "renamed-source")
	if err := os.WriteFile(sourcePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	preflightMaterializeFilesystem(t, targetRoot)
	discovery := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	if discovery.Plan == nil {
		t.Fatal("discovery did not produce the reviewed plan")
	}
	report, err := Run(ctx, RunOptions{
		Meta: meta, Discovery: &discovery, TargetRoot: targetRoot,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != OutcomeMaterializedVerified || report.Operation.PhaseAfter != string(PhaseCommitted) ||
		!report.Target.StageContentVerified || !report.Target.FinalContentVerified || !report.Target.DurabilityConfirmed {
		t.Fatalf("unexpected materialize report: %#v", report)
	}
	raw, err := os.ReadFile(filepath.Join(targetRoot, "final.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, content) {
		t.Fatalf("published bytes differ: %q", raw)
	}
	sourceRaw, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Equal(sourceRaw, content) {
		t.Fatal("source was changed by copy-only materialization")
	}
}

func TestRunPlanMismatchPerformsNoTargetWrite(t *testing.T) {
	ctx := context.Background()
	content := []byte("content")
	meta := materializeSingleV1Meta(t, "final.bin", content)
	searchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(searchRoot, "source"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	preflightMaterializeFilesystem(t, targetRoot)
	discovery := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	before, err := os.ReadDir(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(ctx, RunOptions{
		Meta: meta, Discovery: &discovery, TargetRoot: targetRoot,
		ExpectedPlanID: "000000000000000000000000", Limits: DefaultLimits(),
	})
	if err == nil || report.Outcome != OutcomeBlocked || report.WritesPerformed != 0 {
		t.Fatalf("plan mismatch was not a zero-write policy block: %#v %v", report, err)
	}
	after, err := os.ReadDir(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatal("plan mismatch created target state")
	}
}

func TestResumeAcrossDurableJournalBoundaries(t *testing.T) {
	for _, stopPhase := range []Phase{PhaseJournaled, PhaseFileStaged, PhasePublishIntent, PhasePublished, PhaseFinalVerified} {
		t.Run(string(stopPhase), func(t *testing.T) {
			ctx := context.Background()
			content := []byte("resume content")
			meta := materializeSingleV1Meta(t, "final.bin", content)
			searchRoot := t.TempDir()
			if err := os.WriteFile(filepath.Join(searchRoot, "source"), content, 0o600); err != nil {
				t.Fatal(err)
			}
			targetRoot := t.TempDir()
			preflightMaterializeFilesystem(t, targetRoot)
			discovery := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
			crash := errors.New("injected crash boundary")
			transitionHook = func(phase Phase) error {
				if phase == stopPhase {
					return crash
				}
				return nil
			}
			report, err := Run(ctx, RunOptions{
				Meta: meta, Discovery: &discovery, TargetRoot: targetRoot,
				ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
			})
			transitionHook = nil
			t.Cleanup(func() { transitionHook = nil })
			if !errors.Is(err, crash) || report.Operation.ID == "" || report.Operation.PhaseAfter != string(stopPhase) {
				t.Fatalf("operation did not stop at durable phase %q: %#v %v", stopPhase, report, err)
			}
			operationID, err := ParseOperationID(report.Operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			var resumeDiscovery *seed.DiscoveryResult
			if stopPhase == PhaseJournaled || stopPhase == PhaseFileStaged {
				fresh := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
				resumeDiscovery = &fresh
			}
			resumed, err := Resume(ctx, ResumeOptions{
				Meta: meta, Discovery: resumeDiscovery, TargetRoot: targetRoot,
				OperationID: operationID, ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if resumed.Outcome != OutcomeMaterializedVerified || resumed.Operation.PhaseAfter != string(PhaseCommitted) {
				t.Fatalf("resume did not commit: %#v", resumed)
			}
			raw, err := os.ReadFile(filepath.Join(targetRoot, "final.bin"))
			if err != nil || !bytes.Equal(raw, content) {
				t.Fatalf("resumed final bytes differ: %q %v", raw, err)
			}
		})
	}
}

func TestResumeRejectsChangedJournaledStageBeforeFurtherWrites(t *testing.T) {
	ctx := context.Background()
	content := []byte("unchanged source")
	meta := materializeSingleV1Meta(t, "final.bin", content)
	searchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(searchRoot, "source"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	preflightMaterializeFilesystem(t, targetRoot)
	discovery := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	crash := errors.New("stop after file")
	transitionHook = func(phase Phase) error {
		if phase == PhaseFileStaged {
			return crash
		}
		return nil
	}
	report, err := Run(ctx, RunOptions{
		Meta: meta, Discovery: &discovery, TargetRoot: targetRoot,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	transitionHook = nil
	t.Cleanup(func() { transitionHook = nil })
	if !errors.Is(err, crash) {
		t.Fatal(err)
	}
	operationID, _ := ParseOperationID(report.Operation.ID)
	directoryName, _ := OperationDirectoryName(operationID)
	stagePath := filepath.Join(targetRoot, directoryName, stageDirectoryName, "final.bin")
	if err := os.WriteFile(stagePath, []byte("tampered source"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	resumed, err := Resume(ctx, ResumeOptions{
		Meta: meta, Discovery: &fresh, TargetRoot: targetRoot,
		OperationID: operationID, ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err == nil || resumed.Outcome != OutcomeIntegrityFailed {
		t.Fatalf("changed staged bytes were not rejected: %#v %v", resumed, err)
	}
	if _, err := os.Lstat(filepath.Join(targetRoot, "final.bin")); !os.IsNotExist(err) {
		t.Fatal("integrity failure published a final layout")
	}
}

func TestResumeRecoversRenameBeforePublishedEvent(t *testing.T) {
	ctx := context.Background()
	content := []byte("rename boundary")
	meta := materializeSingleV1Meta(t, "final.bin", content)
	searchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(searchRoot, "source"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	preflightMaterializeFilesystem(t, targetRoot)
	discovery := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	crash := errors.New("crash after namespace publication")
	namespacePublishHook = func(fsbind.Publication) error { return crash }
	report, err := Run(ctx, RunOptions{
		Meta: meta, Discovery: &discovery, TargetRoot: targetRoot,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	namespacePublishHook = nil
	t.Cleanup(func() { namespacePublishHook = nil })
	if !errors.Is(err, crash) || report.Operation.PhaseAfter != string(PhasePublishIntent) ||
		report.Target.Publication != "published" {
		t.Fatalf("rename boundary was not preserved: %#v %v", report, err)
	}
	if _, err := os.Stat(filepath.Join(targetRoot, "final.bin")); err != nil {
		t.Fatal("published final was hidden after injected crash")
	}
	operationID, _ := ParseOperationID(report.Operation.ID)
	status, statusErr := Status(ctx, ControlOptions{TargetRoot: targetRoot, OperationID: operationID, Limits: DefaultLimits()})
	if statusErr != nil || status.Target.Publication != "historical_publication_uncertain" || status.Operation.PhaseAfter != string(PhasePublishIntent) {
		t.Fatalf("status denied the publish-intent crash window: %#v %v", status, statusErr)
	}
	resumed, err := Resume(ctx, ResumeOptions{
		Meta: meta, TargetRoot: targetRoot, OperationID: operationID,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Outcome != OutcomeMaterializedVerified || resumed.Operation.PhaseAfter != string(PhaseCommitted) ||
		resumed.Target.Publication != "committed" {
		t.Fatalf("rename recovery did not commit: %#v", resumed)
	}
}

func TestRunMaterializesScatteredV1V2AndHybridLayouts(t *testing.T) {
	for _, test := range []struct {
		name string
		meta func(*testing.T) *metafile.MetaInfo
	}{
		{"v1", materializeMultiV1Meta},
		{"v2", materializeMultiV2Meta},
		{"hybrid", materializeMultiHybridMeta},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			meta := test.meta(t)
			contentA := []byte("abc")
			if test.name == "hybrid" {
				contentA = bytes.Repeat([]byte{'a'}, 16384)
			}
			contentB := []byte("def")
			searchRoot := t.TempDir()
			if err := os.WriteFile(filepath.Join(searchRoot, "renamed-a"), contentA, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(searchRoot, "renamed-b"), contentB, 0o600); err != nil {
				t.Fatal(err)
			}
			targetRoot := t.TempDir()
			preflightMaterializeFilesystem(t, targetRoot)
			discovery := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
			report, err := Run(ctx, RunOptions{
				Meta: meta, Discovery: &discovery, TargetRoot: targetRoot,
				ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
			})
			if err != nil {
				t.Fatalf("materialize %s failed: %#v %v", test.name, report, err)
			}
			if report.Outcome != OutcomeMaterializedVerified || report.Operation.PhaseAfter != string(PhaseCommitted) ||
				!report.Target.StageContentVerified || !report.Target.FinalContentVerified {
				t.Fatalf("unexpected %s materialize result: %#v", test.name, report)
			}
			for name, want := range map[string][]byte{"a": contentA, "b": contentB} {
				raw, readErr := os.ReadFile(filepath.Join(targetRoot, "bundle", name))
				if readErr != nil || !bytes.Equal(raw, want) {
					t.Fatalf("%s final %s differs: %q %v", test.name, name, raw, readErr)
				}
			}
		})
	}
}

func TestRunPhysicallyCreatesEmptyManifestFile(t *testing.T) {
	ctx := context.Background()
	meta := materializeMultiV1WithEmptyMeta(t)
	searchRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(searchRoot, "renamed-a"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	preflightMaterializeFilesystem(t, targetRoot)
	discovery := discoverForMaterialize(t, ctx, meta, searchRoot, targetRoot)
	report, err := Run(ctx, RunOptions{
		Meta: meta, Discovery: &discovery, TargetRoot: targetRoot,
		ExpectedPlanID: discovery.Plan.ID, Limits: DefaultLimits(),
	})
	if err != nil || report.Outcome != OutcomeMaterializedVerified {
		t.Fatalf("empty-file layout failed: %#v %v", report, err)
	}
	info, err := os.Stat(filepath.Join(targetRoot, "bundle", "empty"))
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		t.Fatalf("empty manifest file was not physically materialized: %#v %v", info, err)
	}
}

func TestRunRejectsManifestAttributesBeforeTargetWrite(t *testing.T) {
	piece := sha1.Sum([]byte("x"))
	meta, err := metafile.Parse(testBencode(map[string]any{"info": map[string]any{
		"files": []any{map[string]any{"attr": "x", "length": int64(1), "path": []any{"file"}}},
		"name":  "bundle", "piece length": int64(1), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	before, err := os.ReadDir(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), RunOptions{
		Meta: meta, TargetRoot: targetRoot, ExpectedPlanID: "000000000000000000000000",
		Limits: DefaultLimits(),
	})
	if err == nil || report.Outcome != OutcomeBlocked || report.WritesPerformed != 0 {
		t.Fatalf("manifest attributes were not a zero-write policy rejection: %#v %v", report, err)
	}
	after, err := os.ReadDir(targetRoot)
	if err != nil || len(before) != len(after) {
		t.Fatal("unsupported manifest changed the target root")
	}
}

func preflightMaterializeFilesystem(t *testing.T, root string) {
	t.Helper()
	session, _, err := fsbind.BindExisting(root)
	if errors.Is(err, fsbind.ErrUnsupported) {
		t.Skip("test filesystem is outside the materialize v1 allowlist")
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func discoverForMaterialize(t *testing.T, ctx context.Context, meta *metafile.MetaInfo, searchRoot, targetRoot string) seed.DiscoveryResult {
	t.Helper()
	result, err := seed.Discover(ctx, meta, seed.DiscoverOptions{
		SearchRoots: []string{searchRoot}, InventoryLimits: storage.DefaultInventoryLimits(),
		MatchLimits: metafile.DefaultSourceMatchLimits(), TimeBudget: time.Minute,
		TargetRoot: targetRoot, Strategy: StrategyCopy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "verified_unique" {
		t.Fatalf("unexpected source outcome: %#v", result)
	}
	return result
}

func materializeSingleV1Meta(t *testing.T, name string, content []byte) *metafile.MetaInfo {
	t.Helper()
	piece := sha1.Sum(content)
	raw := testBencode(map[string]any{"info": map[string]any{
		"length": int64(len(content)), "name": name,
		"piece length": int64(len(content)), "pieces": piece[:],
	}})
	meta, err := metafile.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func materializeMultiV1Meta(t *testing.T) *metafile.MetaInfo {
	t.Helper()
	first := sha1.Sum([]byte("abcd"))
	second := sha1.Sum([]byte("ef"))
	pieces := append(append([]byte(nil), first[:]...), second[:]...)
	return parseMaterializeMeta(t, map[string]any{
		"files": []any{
			map[string]any{"length": int64(3), "path": []any{"a"}},
			map[string]any{"length": int64(3), "path": []any{"b"}},
		},
		"name": "bundle", "piece length": int64(4), "pieces": pieces,
	})
}

func materializeMultiV2Meta(t *testing.T) *metafile.MetaInfo {
	t.Helper()
	rootA := sha256.Sum256([]byte("abc"))
	rootB := sha256.Sum256([]byte("def"))
	return parseMaterializeMeta(t, map[string]any{
		"file tree": map[string]any{
			"a": map[string]any{"": map[string]any{"length": int64(3), "pieces root": rootA[:]}},
			"b": map[string]any{"": map[string]any{"length": int64(3), "pieces root": rootB[:]}},
		},
		"meta version": int64(2), "name": "bundle", "piece length": int64(16384),
	})
}

func materializeMultiHybridMeta(t *testing.T) *metafile.MetaInfo {
	t.Helper()
	contentA := bytes.Repeat([]byte{'a'}, 16384)
	firstPiece := sha1.Sum(contentA)
	secondPiece := sha1.Sum([]byte("def"))
	pieces := append(append([]byte(nil), firstPiece[:]...), secondPiece[:]...)
	rootA := sha256.Sum256(contentA)
	rootB := sha256.Sum256([]byte("def"))
	return parseMaterializeMeta(t, map[string]any{
		"file tree": map[string]any{
			"a": map[string]any{"": map[string]any{"length": int64(len(contentA)), "pieces root": rootA[:]}},
			"b": map[string]any{"": map[string]any{"length": int64(3), "pieces root": rootB[:]}},
		},
		"files": []any{
			map[string]any{"length": int64(len(contentA)), "path": []any{"a"}},
			map[string]any{"length": int64(3), "path": []any{"b"}},
		},
		"meta version": int64(2), "name": "bundle", "piece length": int64(16384), "pieces": pieces,
	})
}

func materializeMultiV1WithEmptyMeta(t *testing.T) *metafile.MetaInfo {
	t.Helper()
	piece := sha1.Sum([]byte("a"))
	return parseMaterializeMeta(t, map[string]any{
		"files": []any{
			map[string]any{"length": int64(1), "path": []any{"a"}},
			map[string]any{"length": int64(0), "path": []any{"empty"}},
		},
		"name": "bundle", "piece length": int64(1), "pieces": piece[:],
	})
}

func parseMaterializeMeta(t *testing.T, info map[string]any) *metafile.MetaInfo {
	t.Helper()
	meta, err := metafile.Parse(testBencode(map[string]any{"info": info}))
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func testBencode(value any) []byte {
	var output bytes.Buffer
	var encode func(any)
	encode = func(current any) {
		switch typed := current.(type) {
		case string:
			fmt.Fprintf(&output, "%d:%s", len(typed), typed)
		case []byte:
			fmt.Fprintf(&output, "%d:", len(typed))
			output.Write(typed)
		case int64:
			fmt.Fprintf(&output, "i%de", typed)
		case []any:
			output.WriteByte('l')
			for _, item := range typed {
				encode(item)
			}
			output.WriteByte('e')
		case map[string]any:
			output.WriteByte('d')
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				encode(key)
				encode(typed[key])
			}
			output.WriteByte('e')
		default:
			panic(fmt.Sprintf("unsupported bencode test type %T", current))
		}
	}
	encode(value)
	return output.Bytes()
}
