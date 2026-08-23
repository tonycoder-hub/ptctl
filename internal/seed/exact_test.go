package seed

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
)

func TestObserveExactSourceRetainsPhysicalEmptyBindingWithoutUniquenessClaim(t *testing.T) {
	content := []byte("x")
	piece := sha1.Sum(content)
	meta, err := metafile.Parse(encode(map[string]any{"info": map[string]any{
		"files": []any{
			map[string]any{"length": int64(1), "path": []any{"data.bin"}},
			map[string]any{"length": int64(0), "path": []any{"empty.bin"}},
		},
		"name": "bundle", "piece length": int64(1), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(root, "empty.bin")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ObserveExactSource(context.Background(), meta, root, ExactSourceOptions{TimeBudget: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceOutcome != "verified_exact_root" || result.Selection.Status != "ready_exact" || result.Selection.SelectedID == "" ||
		!result.Scan.Complete || !result.Scan.VerificationComplete || result.BestEvidence != "verified" || len(result.Matches) != 1 {
		t.Fatalf("unexpected exact source result: %#v", result)
	}
	if len(result.Files) != 2 || result.Files[1].Requirement != "physical_empty_source" || result.Files[1].CandidateCount != 1 || len(result.Matches[0].Bindings) != 2 {
		t.Fatalf("empty physical layout was not represented: files=%#v match=%#v", result.Files, result.Matches[0])
	}
	verified, ok := result.VerifiedSource(meta)
	if !ok {
		t.Fatal("exact result did not retain its process-local proof")
	}
	physicalEmptyPath, err := filepath.EvalSymlinks(emptyPath)
	if err != nil {
		t.Fatal(err)
	}
	if path, ok := verified.Path(1); !ok || path != physicalEmptyPath {
		t.Fatalf("empty binding mismatch: path=%q ok=%t", path, ok)
	}
	if mode, ok := result.VerifiedSourceMode(meta); !ok || mode != "exact_root" {
		t.Fatalf("exact authority mode mismatch: mode=%q ok=%t", mode, ok)
	}
	public := result.PublicReportCopy()
	if _, ok := public.VerifiedSource(meta); ok {
		t.Fatal("public exact source report retained filesystem authority")
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), root) || strings.Contains(string(encoded), emptyPath) {
		t.Fatalf("default exact source report leaked an absolute path: %s", encoded)
	}
}

func TestObserveExactSourceHonorsCancellationBeforeFilesystemRead(t *testing.T) {
	piece := sha1.Sum([]byte("x"))
	meta, err := metafile.Parse(encode(map[string]any{"info": map[string]any{
		"length": int64(1), "name": "x.bin", "piece length": int64(1), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := ObserveExactSource(ctx, meta, filepath.Join(t.TempDir(), "must-not-exist"), ExactSourceOptions{})
	if !errors.Is(err, context.Canceled) || result.Scan.Complete || !contains(result.Scan.StopReasons, "context_cancelled") {
		t.Fatalf("pre-cancel was not preserved: result=%#v err=%v", result, err)
	}
}

func TestObserveExactSourceRequiresPhysicalEmptyManifestName(t *testing.T) {
	content := []byte("x")
	piece := sha1.Sum(content)
	meta, err := metafile.Parse(encode(map[string]any{"info": map[string]any{
		"files": []any{
			map[string]any{"length": int64(1), "path": []any{"data.bin"}},
			map[string]any{"length": int64(0), "path": []any{"empty.bin"}},
		},
		"name": "bundle", "piece length": int64(1), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ObserveExactSource(context.Background(), meta, root, ExactSourceOptions{})
	if err == nil || result.SourceOutcome == "verified_exact_root" || result.Scan.Complete {
		t.Fatalf("missing physical empty file was accepted: result=%#v err=%v", result, err)
	}
}

func contains(items []string, wanted string) bool {
	for _, item := range items {
		if item == wanted {
			return true
		}
	}
	return false
}
