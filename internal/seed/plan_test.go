package seed

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
)

func TestBuildMaterializePlanIsVerifiedAndZeroWrite(t *testing.T) {
	content := []byte("abcdef")
	piece0 := sha1.Sum(content[:4])
	piece1 := sha1.Sum(content[4:])
	pieces := append(piece0[:], piece1[:]...)
	doc := encode(map[string]any{"info": map[string]any{
		"files": []any{
			map[string]any{"length": int64(3), "path": []any{"a.bin"}},
			map[string]any{"length": int64(3), "path": []any{"b.bin"}},
		},
		"name":         "bundle",
		"piece length": int64(4),
		"pieces":       pieces,
	}})
	meta, err := metafile.Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.bin"), content[:3], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "b.bin"), content[3:], 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildMaterializePlan(context.Background(), meta, source, target, "copy")
	if err != nil {
		t.Fatal(err)
	}
	if plan.ID == "" || plan.Evidence != "source_observation:v1_piece_verified" || plan.Effect != "none" || plan.ReadyToApply || plan.Readiness != "layout_only" || len(plan.Operations) != 2 || !plan.Verification.Verified || len(plan.Blockers) == 0 {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	for _, operation := range plan.Operations {
		if operation.SourcePrecondition == nil {
			t.Fatalf("operation lacks source precondition: %#v", operation)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "bundle")); !os.IsNotExist(err) {
		t.Fatalf("plan wrote to target: %v", err)
	}
}

func TestBuildMaterializePlanSupportsV2AndHybrid(t *testing.T) {
	content := []byte("verified")
	v2Root := sha256.Sum256(content)
	v1Piece := sha1.Sum(content)
	tests := []struct {
		name     string
		hybrid   bool
		evidence string
	}{
		{name: "v2", evidence: "source_observation:v2_merkle_verified"},
		{name: "hybrid", hybrid: true, evidence: "source_observation:single_pass_v1_piece_and_v2_merkle_verified"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := map[string]any{
				"file tree": map[string]any{"verified.bin": map[string]any{"": map[string]any{
					"length": int64(len(content)), "pieces root": v2Root[:],
				}}},
				"meta version": int64(2), "name": "verified.bin", "piece length": int64(16384),
			}
			if test.hybrid {
				info["length"] = int64(len(content))
				info["pieces"] = v1Piece[:]
			}
			meta, err := metafile.Parse(encode(map[string]any{"info": info}))
			if err != nil {
				t.Fatal(err)
			}
			sourceDir := t.TempDir()
			source := filepath.Join(sourceDir, "verified.bin")
			if err := os.WriteFile(source, content, 0o600); err != nil {
				t.Fatal(err)
			}
			target := t.TempDir()
			plan, err := BuildMaterializePlan(context.Background(), meta, source, target, "copy")
			if err != nil {
				t.Fatal(err)
			}
			if plan.Evidence != test.evidence || plan.Effect != "none" || plan.ReadyToApply || plan.InfoHashV2 == "" || plan.Verification.Verified != true || len(plan.Verification.Checks) == 0 {
				t.Fatalf("unexpected %s plan: %#v", test.name, plan)
			}
			if test.hybrid && plan.InfoHashV1 == "" {
				t.Fatalf("hybrid plan has no v1 infohash: %#v", plan)
			}
			if _, err := os.Stat(filepath.Join(target, "verified.bin")); !os.IsNotExist(err) {
				t.Fatalf("plan wrote to target: %v", err)
			}
		})
	}
}

func TestPlanIDNormalizesEquivalentSourcePaths(t *testing.T) {
	content := []byte("x")
	piece := sha1.Sum(content)
	meta, err := metafile.Parse(encode(map[string]any{"info": map[string]any{
		"length": int64(1), "name": "x", "piece length": int64(1), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}
	sourceDirectory := t.TempDir()
	absoluteSource := filepath.Join(sourceDirectory, "x")
	if err := os.WriteFile(absoluteSource, content, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sourceDirectory)
	relativeSource := "x"
	target := t.TempDir()
	absolutePlan, err := BuildMaterializePlan(context.Background(), meta, absoluteSource, target, "copy")
	if err != nil {
		t.Fatal(err)
	}
	relativePlan, err := BuildMaterializePlan(context.Background(), meta, relativeSource, target, "copy")
	if err != nil {
		t.Fatal(err)
	}
	if absolutePlan.ID != relativePlan.ID || absolutePlan.SourceRoot != relativePlan.SourceRoot {
		t.Fatalf("equivalent paths changed plan identity: absolute=%#v relative=%#v", absolutePlan, relativePlan)
	}
}

func TestBuildMaterializePlanFromVerifiedScatteredSources(t *testing.T) {
	content := []byte("abcdef")
	piece0 := sha1.Sum(content[:4])
	piece1 := sha1.Sum(content[4:])
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
	first := filepath.Join(t.TempDir(), "renamed-a")
	second := filepath.Join(t.TempDir(), "renamed-b")
	if err := os.WriteFile(first, content[:3], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, content[3:], 0o600); err != nil {
		t.Fatal(err)
	}
	verified, err := metafile.VerifySourceMap(context.Background(), meta, metafile.SourceMap{Bindings: []metafile.SourceBinding{
		{FileIndex: 0, Path: first}, {FileIndex: 1, Path: second},
	}})
	if err != nil || !verified.Result().Verified {
		t.Fatalf("verified=%#v err=%v", verified, err)
	}
	target := t.TempDir()
	plan, err := BuildMaterializePlanFromVerified(context.Background(), meta, verified, target, "copy")
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceMode != "discovered_map" || plan.SourceRoot != "" || len(plan.Operations) != 2 || plan.Operations[0].Source != first || plan.Operations[1].Source != second || plan.Operations[0].ManifestIndex != 0 || plan.Operations[1].ManifestIndex != 1 {
		t.Fatalf("unexpected scattered plan: %#v", plan)
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatalf("mapped plan mutated target: entries=%v err=%v", entries, err)
	}
}

func TestBuildMaterializePlanUsesVirtualEmptyOperation(t *testing.T) {
	meta, err := metafile.Parse(encode(map[string]any{"info": map[string]any{
		"length": int64(0), "name": "empty", "piece length": int64(1), "pieces": []byte{},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	verified, err := metafile.VerifySourceMap(context.Background(), meta, metafile.SourceMap{})
	if err != nil || !verified.Result().Verified {
		t.Fatalf("verified=%#v err=%v", verified, err)
	}
	plan, err := BuildMaterializePlanFromVerified(context.Background(), meta, verified, t.TempDir(), "copy")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Operations) != 1 || plan.Operations[0].Kind != "empty" || plan.Operations[0].Source != "" || plan.EstimatedRead != 0 || plan.EstimatedWrite != 0 {
		t.Fatalf("unexpected empty plan: %#v", plan)
	}
}

func TestBuildMaterializePlanRejectsVerifiedTokenFromDifferentVariant(t *testing.T) {
	content := []byte("content")
	piece := sha1.Sum(content)
	first, err := metafile.Parse(encode(map[string]any{"info": map[string]any{
		"length": int64(len(content)), "name": "first.bin", "piece length": int64(len(content)), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := metafile.Parse(encode(map[string]any{"info": map[string]any{
		"length": int64(len(content)), "name": "second.bin", "piece length": int64(len(content)), "pieces": piece[:],
	}}))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "renamed")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	verified, err := metafile.VerifySourceMap(context.Background(), first, metafile.SourceMap{Bindings: []metafile.SourceBinding{{FileIndex: 0, Path: path}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildMaterializePlanFromVerified(context.Background(), second, verified, t.TempDir(), "copy"); err == nil {
		t.Fatal("plan accepted a verification token from another metafile variant")
	}
}

func encode(value any) []byte {
	var out bytes.Buffer
	encodeInto(&out, value)
	return out.Bytes()
}

func encodeInto(out *bytes.Buffer, value any) {
	switch v := value.(type) {
	case string:
		out.WriteString(strconv.Itoa(len(v)))
		out.WriteByte(':')
		out.WriteString(v)
	case []byte:
		out.WriteString(strconv.Itoa(len(v)))
		out.WriteByte(':')
		out.Write(v)
	case int64:
		out.WriteByte('i')
		out.WriteString(strconv.FormatInt(v, 10))
		out.WriteByte('e')
	case []any:
		out.WriteByte('l')
		for _, item := range v {
			encodeInto(out, item)
		}
		out.WriteByte('e')
	case map[string]any:
		out.WriteByte('d')
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			encodeInto(out, key)
			encodeInto(out, v[key])
		}
		out.WriteByte('e')
	default:
		panic("unsupported test type")
	}
}
