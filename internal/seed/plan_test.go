package seed

import (
	"bytes"
	"context"
	"crypto/sha1"
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
	if plan.ID == "" || plan.Evidence != "source_snapshot:v1_piece_verified" || plan.Readiness != "layout_only" || len(plan.Operations) != 2 || !plan.Verification.Verified || len(plan.Blockers) == 0 {
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
