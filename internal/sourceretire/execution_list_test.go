package sourceretire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

func TestListExecutionOperationsIsBoundedAndNameOnly(t *testing.T) {
	root := executionListRoot(t)
	first := OperationID("sha256:" + strings.Repeat("1", 64))
	second := OperationID("sha256:" + strings.Repeat("2", 64))
	for _, operation := range []OperationID{second, first} {
		name, err := OperationDirectoryName(operation)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	canary := "PRIVATE-TARGET-NAME-CANARY"
	if err := os.WriteFile(filepath.Join(root, canary), []byte("not an operation"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ListExecutionOperations(context.Background(), root, DefaultExecutionOperationListLimits())
	if err != nil || !result.Complete || result.WritesPerformed != 0 || result.StopReason != "" ||
		len(result.Operations) != 2 || result.Operations[0].ID != first || result.Operations[1].ID != second ||
		result.Operations[0].Status != "not_inspected" || result.Warnings == nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(root)) || bytes.Contains(raw, []byte(canary)) {
		t.Fatalf("name-only listing leaked a path or unrelated name: %s", raw)
	}

	limits := DefaultExecutionOperationListLimits()
	limits.MaxOperations = 1
	limited, err := ListExecutionOperations(context.Background(), root, limits)
	if err != nil || limited.Complete || limited.StopReason != "max_operations" || len(limited.Operations) != 1 {
		t.Fatalf("limited=%#v err=%v", limited, err)
	}
}

func TestListExecutionOperationsRejectsMalformedReservedEntry(t *testing.T) {
	root := executionListRoot(t)
	if err := os.Mkdir(filepath.Join(root, materialize.SourceRetireOperationDirectoryPrefix+"not-a-digest"), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := ListExecutionOperations(context.Background(), root, DefaultExecutionOperationListLimits())
	if err != nil || result.Complete || result.StopReason != "invalid_operation_entry" || len(result.Operations) != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestListExecutionOperationsHonorsPreCancellationBeforeBinding(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := ListExecutionOperations(ctx, filepath.Join(t.TempDir(), "MISSING-MUST-NOT-BE-BOUND"), DefaultExecutionOperationListLimits())
	if !errors.Is(err, context.Canceled) || result.Complete || result.StopReason != "context_cancelled" || result.RootUsed.EntriesExamined != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func executionListRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	session, _, err := fsbind.BindExisting(root)
	if errors.Is(err, fsbind.ErrUnsupported) {
		t.Skipf("bound target-root listing is unsupported: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}
