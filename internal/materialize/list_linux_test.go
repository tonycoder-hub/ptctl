package materialize

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A shared target root routinely holds unrelated user files. A name that no
// bound path can represent cannot be a ptctl operation, so operation discovery
// reports it instead of failing the whole listing.
func TestListOperationsSkipsUnrepresentableUnrelatedRootNames(t *testing.T) {
	ctx := context.Background()
	_, _, _, targetRoot, operationID := interruptedMaterialize(t, PhaseFileStaged)
	for _, name := range []string{"legacy-\xff\xfe.bin", "control-\x01.bin"} {
		if err := os.WriteFile(filepath.Join(targetRoot, name), []byte("user"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := ListOperations(ctx, targetRoot, DefaultOperationListLimits())
	if err != nil || !result.Complete || len(result.Operations) != 1 || result.Operations[0].ID != operationID {
		t.Fatalf("discovery = %#v, %v", result, err)
	}
	if result.RootUsed.Unrepresentable != 2 || result.Writes != 0 {
		t.Fatalf("skipped entries were not accounted: %#v", result.RootUsed)
	}
	warned := false
	for _, warning := range result.Warnings {
		warned = warned || strings.Contains(warning, "2 target root entries")
	}
	if !warned {
		t.Fatalf("skipped entries were not reported: %#v", result.Warnings)
	}

	limits := DefaultOperationListLimits()
	limits.MaxRootEntries = result.RootUsed.EntriesExamined - 1
	bounded, err := ListOperations(ctx, targetRoot, limits)
	if err != nil || bounded.Complete || bounded.StopReason != "max_entries" {
		t.Fatalf("skipped entries escaped the root entry budget: %#v, %v", bounded, err)
	}
}
