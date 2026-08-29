package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStreamRegularFileInventoryIsDeterministicAndDoesNotCollapseHardlinks(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"a", "z"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"b.bin":                       "root-b",
		"c.bin":                       "root-c",
		filepath.Join("a", "one.bin"): "child-a",
		filepath.Join("z", "two.bin"): "child-z",
	}
	for relative, content := range files {
		if err := os.WriteFile(filepath.Join(root, relative), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	hardlinkCreated := os.Link(filepath.Join(root, "b.bin"), filepath.Join(root, "alias.bin")) == nil
	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkCreated := os.Symlink(outside, filepath.Join(root, "linked.bin")) == nil

	run := func() (FullInventoryResult, []RegularFileIndexEntry) {
		t.Helper()
		records := []RegularFileIndexEntry{}
		result, err := StreamRegularFileInventory(context.Background(), []FullInventoryRoot{{ID: "root:test", Path: root}}, FullInventoryOptions{Limits: DefaultFullInventoryLimits()}, func(record RegularFileIndexEntry) error {
			records = append(records, record)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return result, records
	}

	firstResult, first := run()
	secondResult, second := run()
	if !firstResult.Complete || !secondResult.Complete {
		t.Fatalf("complete tree was reported incomplete: first=%#v second=%#v", firstResult, secondResult)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("inventory order or records changed between scans:\nfirst=%#v\nsecond=%#v", first, second)
	}
	want := []string{"a/one.bin", "b.bin", "c.bin", "z/two.bin"}
	if hardlinkCreated {
		want = []string{"a/one.bin", "alias.bin", "b.bin", "c.bin", "z/two.bin"}
	}
	got := make([]string, len(first))
	for index, record := range first {
		got[index] = decodeIndexRecordPath(t, record)
		if record.RootID != "root:test" || record.SizeBytes < 0 || record.ModifiedUnixNanos <= 0 {
			t.Fatalf("record lacks bounded index metadata: %#v", record)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected deterministic traversal order: got=%#v want=%#v", got, want)
	}
	if firstResult.Stats.FilesEmitted != len(want) || firstResult.Stats.RegularFilesSeen != len(want) {
		t.Fatalf("regular-file accounting does not match callback records: %#v", firstResult.Stats)
	}
	if symlinkCreated && firstResult.Stats.SymlinksSkipped == 0 && firstResult.Stats.ReparseSkipped == 0 {
		t.Fatalf("link was not skipped: %#v", firstResult.Stats)
	}
	if len(firstResult.Roots) != 1 || firstResult.Roots[0].ID != "root:test" || firstResult.Roots[0].Status != "complete" || firstResult.Roots[0].FilesystemIdentityHint == "" || firstResult.Roots[0].RootIdentityHint == "" {
		t.Fatalf("declared root identity was not captured without paths: %#v", firstResult.Roots)
	}
	encoded, err := json.Marshal(firstResult)
	if err != nil {
		t.Fatal(err)
	}
	var publicResult any
	if err := json.Unmarshal(encoded, &publicResult); err != nil {
		t.Fatal(err)
	}
	if jsonStringContains(publicResult, root) {
		t.Fatalf("full inventory result leaked its runtime root path: %s", encoded)
	}
	rootInputJSON, err := json.Marshal(FullInventoryRoot{ID: "root:test", Path: root})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rootInputJSON), root) {
		t.Fatalf("runtime root path was serializable through FullInventoryRoot: %s", rootInputJSON)
	}
}

func TestStreamRegularFileInventoryLimitsAreStructured(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.bin", "b.bin"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("files", func(t *testing.T) {
		limits := DefaultFullInventoryLimits()
		limits.MaxFiles = 1
		var records []RegularFileIndexEntry
		result, err := StreamRegularFileInventory(context.Background(), []FullInventoryRoot{{ID: "root:test", Path: root}}, FullInventoryOptions{Limits: limits}, func(record RegularFileIndexEntry) error {
			records = append(records, record)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Complete || len(records) != 1 || result.Stats.FilesEmitted != 1 || !slices.Contains(result.LimitHits, "max_files") || !slices.Contains(result.StopReasons, "budget_exhausted") {
			t.Fatalf("max-files stop was not represented: result=%#v records=%#v", result, records)
		}
	})

	t.Run("entries", func(t *testing.T) {
		limits := DefaultFullInventoryLimits()
		limits.MaxEntries = 1
		limits.MaxFiles = 2
		calls := 0
		result, err := StreamRegularFileInventory(context.Background(), []FullInventoryRoot{{ID: "root:test", Path: root}}, FullInventoryOptions{Limits: limits}, func(RegularFileIndexEntry) error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Complete || calls != 0 || result.Stats.EntriesExamined != 2 || !slices.Contains(result.LimitHits, "max_entries") || !slices.Contains(result.StopReasons, "budget_exhausted") {
			t.Fatalf("max-entries stop was not fail-closed: %#v", result)
		}
	})

	t.Run("per directory", func(t *testing.T) {
		limits := DefaultFullInventoryLimits()
		limits.MaxEntries = 10
		limits.MaxEntriesPerDirectory = 1
		calls := 0
		result, err := StreamRegularFileInventory(context.Background(), []FullInventoryRoot{{ID: "root:test", Path: root}}, FullInventoryOptions{Limits: limits}, func(RegularFileIndexEntry) error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Complete || calls != 0 || result.Stats.EntriesExamined != 2 || !slices.Contains(result.LimitHits, "max_entries_per_directory") || len(result.Issues) != 1 || result.Issues[0].Code != "scan.directory_entry_budget" {
			t.Fatalf("per-directory stop retained an arbitrary partial listing: %#v", result)
		}
	})

	t.Run("per-directory sentinels consume the global entry budget", func(t *testing.T) {
		nestedRoot := t.TempDir()
		for _, directory := range []string{"a", "b"} {
			for _, name := range []string{"1", "2", "3"} {
				if err := os.MkdirAll(filepath.Join(nestedRoot, directory), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(nestedRoot, directory, name), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		limits := DefaultFullInventoryLimits()
		limits.MaxEntries = 7
		limits.MaxEntriesPerDirectory = 2
		result, err := StreamRegularFileInventory(context.Background(), []FullInventoryRoot{{ID: "root:test", Path: nestedRoot}}, FullInventoryOptions{Limits: limits}, func(RegularFileIndexEntry) error {
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Complete || result.Stats.EntriesExamined != 8 || !slices.Contains(result.LimitHits, "max_entries") || !slices.Contains(result.StopReasons, "budget_exhausted") {
			t.Fatalf("N+1 directory observations bypassed the global entry budget: %#v", result)
		}
	})

	t.Run("path bytes", func(t *testing.T) {
		limits := DefaultFullInventoryLimits()
		limits.MaxPathBytes = 1
		calls := 0
		result, err := StreamRegularFileInventory(context.Background(), []FullInventoryRoot{{ID: "root:test", Path: root}}, FullInventoryOptions{Limits: limits}, func(RegularFileIndexEntry) error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Complete || calls != 0 || !slices.Contains(result.LimitHits, "max_path_bytes") || !slices.Contains(result.StopReasons, "budget_exhausted") {
			t.Fatalf("path-byte stop was not represented: %#v", result)
		}
	})
}

func TestStreamRegularFileInventoryCancellationIsStructuredAndClosesHandles(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.bin", "b.bin"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	result, err := StreamRegularFileInventory(ctx, []FullInventoryRoot{{ID: "root:test", Path: root}}, FullInventoryOptions{Limits: DefaultFullInventoryLimits()}, func(RegularFileIndexEntry) error {
		calls++
		cancel()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || calls != 1 || result.Stats.FilesEmitted != 1 || !slices.Contains(result.LimitHits, "context_cancelled") || !slices.Contains(result.StopReasons, "context_cancelled") {
		t.Fatalf("callback cancellation was not represented as a partial accepted stream: %#v", result)
	}
	if err := os.Rename(root, filepath.Join(parent, "renamed")); err != nil {
		t.Fatalf("scanner retained a filesystem handle after cancellation: %v", err)
	}
}

func TestStreamRegularFileInventoryCallbackErrorReturnsPartialResultAndClosesHandles(t *testing.T) {
	parent := t.TempDir()
	rootA := filepath.Join(parent, "a")
	rootB := filepath.Join(parent, "b")
	for _, root := range []string{rootA, rootB} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "x.bin"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sinkErr := errors.New("sink stopped")
	result, err := StreamRegularFileInventory(context.Background(), []FullInventoryRoot{{ID: "root:b", Path: rootB}, {ID: "root:a", Path: rootA}}, FullInventoryOptions{Limits: DefaultFullInventoryLimits()}, func(RegularFileIndexEntry) error {
		return sinkErr
	})
	if !errors.Is(err, sinkErr) {
		t.Fatalf("callback error was not preserved: %v", err)
	}
	if result.Complete || result.Stats.FilesEmitted != 0 || !slices.Contains(result.StopReasons, "callback_failed") {
		t.Fatalf("callback failure did not return an auditable partial result: %#v", result)
	}
	statuses := map[string]int{}
	for _, root := range result.Roots {
		statuses[root.Status]++
	}
	if statuses["incomplete"] != 1 || statuses["not_scanned_callback"] != 1 || statuses["pending"] != 0 {
		t.Fatalf("remaining roots were not closed out after callback failure: %#v", statuses)
	}
	if result.Roots[0].ID != "root:a" || result.Roots[1].ID != "root:b" {
		t.Fatalf("declared roots were not scanned in stable ID order: %#v", result.Roots)
	}
	for index, root := range []string{rootA, rootB} {
		if err := os.Rename(root, filepath.Join(parent, string(rune('c'+index)))); err != nil {
			t.Fatalf("scanner retained a filesystem handle after callback failure: %v", err)
		}
	}
}

func TestStreamRegularFileInventoryOrdersDeclaredIDsBeforePaths(t *testing.T) {
	parent := t.TempDir()
	pathA := filepath.Join(parent, "a-path")
	pathZ := filepath.Join(parent, "z-path")
	for _, path := range []string{pathA, pathZ} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "x.bin"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var rootIDs []string
	result, err := StreamRegularFileInventory(context.Background(), []FullInventoryRoot{{ID: "root:z", Path: pathA}, {ID: "root:a", Path: pathZ}}, FullInventoryOptions{Limits: DefaultFullInventoryLimits()}, func(record RegularFileIndexEntry) error {
		rootIDs = append(rootIDs, record.RootID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || !reflect.DeepEqual(rootIDs, []string{"root:a", "root:z"}) || result.Roots[0].ID != "root:a" || result.Roots[1].ID != "root:z" {
		t.Fatalf("stream cannot feed a root-ID-sorted encoder directly: result=%#v records=%#v", result, rootIDs)
	}
}

func TestStreamRegularFileInventoryRemapsUnavailableRootIssues(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	result, err := StreamRegularFileInventory(context.Background(), []FullInventoryRoot{{ID: "root:missing", Path: missing}, {ID: "root:live", Path: root}}, FullInventoryOptions{Limits: DefaultFullInventoryLimits()}, func(RegularFileIndexEntry) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || len(result.Issues) != 1 || result.Issues[0].RootID != "root:missing" || result.Issues[0].Code != "scan.root_unavailable" {
		t.Fatalf("unavailable-root issue did not retain its declared identity: %#v", result)
	}
	statuses := map[string]string{}
	for _, observed := range result.Roots {
		statuses[observed.ID] = observed.Status
	}
	if statuses["root:live"] != "complete" || statuses["root:missing"] != "unavailable" {
		t.Fatalf("declared root statuses are ambiguous: %#v", result.Roots)
	}
}

func TestReobserveIndexedRegularBuildsFreshLiveAuthority(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "x.bin")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var record RegularFileIndexEntry
	declaredRoot := FullInventoryRoot{ID: "root:test", Path: root}
	result, err := StreamRegularFileInventory(context.Background(), []FullInventoryRoot{declaredRoot}, FullInventoryOptions{Limits: DefaultFullInventoryLimits()}, func(candidate RegularFileIndexEntry) error {
		record = candidate
		return nil
	})
	if err != nil || !result.Complete || result.Stats.FilesEmitted != 1 {
		t.Fatalf("initial inventory result=%#v err=%v", result, err)
	}
	backup := filepath.Join(root, "old.bin")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	modifiedAt := time.Unix(0, record.ModifiedUnixNanos)
	if err := os.Chtimes(path, modifiedAt, modifiedAt); err != nil {
		t.Fatal(err)
	}

	reobservation, err := ReobserveIndexedRegular(context.Background(), declaredRoot, record.RelativeComponentsRawBase64, false)
	if err != nil {
		t.Fatal(err)
	}
	if record.IdentityHint != "" && reobservation.FileIdentityHint == record.IdentityHint {
		t.Fatal("live reobservation reused the stale index identity hint")
	}
	if reobservation.Observation.RootID != declaredRoot.ID || reobservation.Observation.ObservationID == "" {
		t.Fatalf("live observation was not bound to the declared root ID: %#v", reobservation.Observation)
	}
	if len(result.Roots) != 1 || reobservation.FilesystemIdentityHint != result.Roots[0].FilesystemIdentityHint || reobservation.RootIdentityHint != result.Roots[0].RootIdentityHint {
		t.Fatalf("unchanged root hints drifted across live reobservation: scan=%#v live=%#v", result.Roots, reobservation)
	}
	file, err := reobservation.Observation.OpenObservedRegular()
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || string(content) != "new" {
		t.Fatalf("fresh observation did not bind the replacement: content=%q read=%v close=%v", content, readErr, closeErr)
	}
}

func TestReobserveIndexedRegularRejectsUnsafeOrStaleLocator(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "x.bin")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, components := range map[string][]string{
		"empty":         nil,
		"non canonical": {"eA"},
		"parent":        {base64.StdEncoding.EncodeToString([]byte(".."))},
		"separator":     {base64.StdEncoding.EncodeToString([]byte("child" + string(filepath.Separator) + "x.bin"))},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReobserveIndexedRegular(context.Background(), FullInventoryRoot{ID: "root:test", Path: root}, components, false); err == nil {
				t.Fatalf("unsafe indexed components were accepted: %#v", components)
			}
		})
	}

	encoded := []string{base64.StdEncoding.EncodeToString([]byte("x.bin"))}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReobserveIndexedRegular(ctx, FullInventoryRoot{ID: "root:test", Path: root}, encoded, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled reobservation returned %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReobserveIndexedRegular(context.Background(), FullInventoryRoot{ID: "root:test", Path: root}, encoded, false); err == nil {
		t.Fatal("stale indexed path was treated as live authority")
	}
}

func TestFullInventoryLimitsValidateIndependentEntryAndFileBudgets(t *testing.T) {
	limits := DefaultFullInventoryLimits()
	limits.MaxEntries = 1
	limits.MaxFiles = 2
	if err := limits.Validate(); err != nil {
		t.Fatalf("independent entry and emitted-file limits were rejected: %v", err)
	}
	limits.MaxFiles = 0
	if err := limits.Validate(); err == nil {
		t.Fatal("zero max-files budget was accepted")
	}
}

func TestFullInventoryPreScanErrorsNeverClaimCompleteness(t *testing.T) {
	limits := DefaultFullInventoryLimits()
	missing := filepath.Join(t.TempDir(), "missing")
	result, err := StreamRegularFileInventory(context.Background(), []FullInventoryRoot{{ID: "root:missing", Path: missing}}, FullInventoryOptions{Limits: limits}, func(RegularFileIndexEntry) error { return nil })
	if err == nil || result.Complete {
		t.Fatalf("unavailable root claimed a complete inventory: result=%#v err=%v", result, err)
	}

	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	result, err = StreamRegularFileInventory(context.Background(), []FullInventoryRoot{{ID: "root:parent", Path: parent}, {ID: "root:child", Path: child}}, FullInventoryOptions{Limits: limits}, func(RegularFileIndexEntry) error { return nil })
	if err == nil || result.Complete {
		t.Fatalf("overlapping roots claimed a complete inventory: result=%#v err=%v", result, err)
	}

	invalid := limits
	invalid.MaxEntries = 0
	result, err = StreamRegularFileInventory(context.Background(), []FullInventoryRoot{{ID: "root:parent", Path: parent}}, FullInventoryOptions{Limits: invalid}, func(RegularFileIndexEntry) error { return nil })
	if err == nil || result.Complete {
		t.Fatalf("invalid direct API input claimed a complete inventory: result=%#v err=%v", result, err)
	}
}

func TestStreamRegularFileInventoryValidatesDeclaredRootsBeforeScanning(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	limits := DefaultFullInventoryLimits()
	for name, roots := range map[string][]FullInventoryRoot{
		"empty id":       {{Path: root}},
		"duplicate id":   {{ID: "root:same", Path: root}, {ID: "root:same", Path: other}},
		"duplicate path": {{ID: "root:a", Path: root}, {ID: "root:b", Path: root}},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			if _, err := StreamRegularFileInventory(context.Background(), roots, FullInventoryOptions{Limits: limits}, func(RegularFileIndexEntry) error {
				calls++
				return nil
			}); err == nil {
				t.Fatalf("invalid root declarations were accepted: %#v", roots)
			}
			if calls != 0 {
				t.Fatal("invalid root declarations reached the streaming callback")
			}
		})
	}
}

func TestStreamRegularFileInventoryPreCancellationClosesEveryDeclaredRoot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	roots := []FullInventoryRoot{{ID: "root:b", Path: filepath.Join(t.TempDir(), "missing-b")}, {ID: "root:a", Path: filepath.Join(t.TempDir(), "missing-a")}}
	calls := 0
	result, err := StreamRegularFileInventory(ctx, roots, FullInventoryOptions{Limits: DefaultFullInventoryLimits()}, func(RegularFileIndexEntry) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || calls != 0 || len(result.Roots) != len(roots) || !slices.Contains(result.LimitHits, "context_cancelled") {
		t.Fatalf("preparation cancellation was not fully scoped: %#v", result)
	}
	for index, root := range result.Roots {
		if root.Status != "not_scanned_budget" || root.ID != []string{"root:a", "root:b"}[index] || root.FilesystemIdentityHint != "" || root.RootIdentityHint != "" {
			t.Fatalf("unprepared root was not represented safely: %#v", result.Roots)
		}
	}
}

func decodeIndexRecordPath(t *testing.T, record RegularFileIndexEntry) string {
	t.Helper()
	components := make([]string, len(record.RelativeComponentsRawBase64))
	for index, encoded := range record.RelativeComponentsRawBase64 {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("record component %d is invalid base64: %v", index, err)
		}
		components[index] = string(raw)
	}
	return filepath.ToSlash(filepath.Join(components...))
}

func jsonStringContains(value any, target string) bool {
	switch typed := value.(type) {
	case string:
		return strings.Contains(typed, target)
	case []any:
		for _, item := range typed {
			if jsonStringContains(item, target) {
				return true
			}
		}
	case map[string]any:
		for key, item := range typed {
			if strings.Contains(key, target) || jsonStringContains(item, target) {
				return true
			}
		}
	}
	return false
}
