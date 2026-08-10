package storage

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultMaxFullInventoryFiles = DefaultMaxScanEntries
	hardMaxFullInventoryFiles    = hardMaxScanEntries
)

// FullInventoryLimits bounds a metadata-only scan used to build a persistent
// candidate index. Unlike InventoryLimits, MaxFiles counts every emitted
// regular-file path rather than only files matching a requested size.
type FullInventoryLimits struct {
	MaxRoots               int   `json:"max_roots"`
	MaxDepth               int   `json:"max_depth"`
	MaxDirectories         int   `json:"max_directories"`
	MaxEntries             int   `json:"max_entries"`
	MaxEntriesPerDirectory int   `json:"max_entries_per_directory"`
	MaxFiles               int   `json:"max_files"`
	MaxPathBytes           int64 `json:"max_path_bytes"`
	MaxIssues              int   `json:"max_issues"`
}

func DefaultFullInventoryLimits() FullInventoryLimits {
	return FullInventoryLimits{
		MaxRoots:               DefaultMaxSearchRoots,
		MaxDepth:               DefaultMaxScanDepth,
		MaxDirectories:         DefaultMaxScanDirectories,
		MaxEntries:             DefaultMaxScanEntries,
		MaxEntriesPerDirectory: DefaultMaxEntriesPerDirectory,
		MaxFiles:               DefaultMaxFullInventoryFiles,
		MaxPathBytes:           DefaultMaxRetainedPathBytes,
		MaxIssues:              DefaultMaxScanIssues,
	}
}

func (limits FullInventoryLimits) Validate() error {
	checks := []struct {
		name  string
		value int64
		hard  int64
	}{
		{"max roots", int64(limits.MaxRoots), hardMaxSearchRoots},
		{"max depth", int64(limits.MaxDepth), hardMaxScanDepth},
		{"max directories", int64(limits.MaxDirectories), hardMaxScanDirectories},
		{"max entries", int64(limits.MaxEntries), hardMaxScanEntries},
		{"max entries per directory", int64(limits.MaxEntriesPerDirectory), hardMaxEntriesPerDirectory},
		{"max files", int64(limits.MaxFiles), hardMaxFullInventoryFiles},
		{"max path bytes", limits.MaxPathBytes, hardMaxRetainedPathBytes},
		{"max issues", int64(limits.MaxIssues), hardMaxScanIssues},
	}
	for _, check := range checks {
		if check.value <= 0 || check.value > check.hard {
			return fmt.Errorf("%s must be in 1..%d", check.name, check.hard)
		}
	}
	return nil
}

type FullInventoryOptions struct {
	Limits       FullInventoryLimits
	AllowNetwork bool
}

// FullInventoryRoot binds a caller-declared stable ID to one live profile
// root. Path is runtime reopen authority and is never copied into a result.
type FullInventoryRoot struct {
	ID   string `json:"root_id"`
	Path string `json:"-"`
}

// RegularFileIndexEntry is safe to retain as an index candidate, not as proof
// that the same file still exists. IdentityHint is deliberately non-authority;
// ReobserveIndexedRegular never accepts it as input.
type RegularFileIndexEntry struct {
	RootID                      string   `json:"root_id"`
	RelativeComponentsRawBase64 []string `json:"relative_components_raw_base64"`
	SizeBytes                   int64    `json:"size_bytes"`
	ModifiedUnixNanos           int64    `json:"modified_unix_nanos"`
	IdentityHint                string   `json:"identity_hint,omitempty"`
}

// FullInventoryRootObservation contains only non-authoritative identity hints;
// profile paths are intentionally absent so this can be persisted safely.
type FullInventoryRootObservation struct {
	ID                     string `json:"root_id"`
	Status                 string `json:"status"`
	FilesystemIdentityHint string `json:"filesystem_identity_hint,omitempty"`
	RootIdentityHint       string `json:"root_identity_hint,omitempty"`
}

type FullInventoryStats struct {
	DirectoriesOpened        int   `json:"directories_opened"`
	EntriesExamined          int   `json:"entries_examined"`
	TraversalNameBytes       int64 `json:"traversal_name_bytes"`
	RegularFilesSeen         int   `json:"regular_files_seen"`
	FilesEmitted             int   `json:"files_emitted"`
	EmittedPathBytes         int64 `json:"emitted_path_bytes"`
	SymlinksSkipped          int   `json:"symlinks_skipped"`
	ReparseSkipped           int   `json:"reparse_points_skipped"`
	MountsSkipped            int   `json:"mounts_skipped"`
	SpecialSkipped           int   `json:"special_files_skipped"`
	IdentityHintsUnavailable int   `json:"identity_hints_unavailable"`
	IssueOverflow            int   `json:"issue_overflow"`
}

type FullInventoryResult struct {
	Complete        bool                           `json:"complete"`
	PathConfinement string                         `json:"path_confinement"`
	Limits          FullInventoryLimits            `json:"limits"`
	Roots           []FullInventoryRootObservation `json:"search_roots"`
	Stats           FullInventoryStats             `json:"stats"`
	LimitHits       []string                       `json:"limit_hits"`
	StopReasons     []string                       `json:"stop_reasons"`
	Issues          []ScanIssue                    `json:"issues"`
	Warnings        []string                       `json:"warnings"`
}

// A directory task opens one bounded listing. Entry tasks preserve that
// listing's bytewise lexical order without retaining any emitted file record.
type fullInventoryTraversalTask struct {
	directory        bool
	directoryTask    directoryTask
	parentPath       string
	parentComponents []string
	parentDepth      int
	entryName        string
}

const maxFullInventoryRootIDBytes = 512

func validateFullInventoryRoots(roots []FullInventoryRoot, maxRoots int) error {
	if len(roots) == 0 || len(roots) > maxRoots {
		return fmt.Errorf("full inventory roots must contain 1..%d declarations", maxRoots)
	}
	seenIDs := make(map[string]struct{}, len(roots))
	seenPaths := make(map[string]struct{}, len(roots))
	for index, root := range roots {
		if err := validateFullInventoryRoot(root); err != nil {
			return fmt.Errorf("full inventory root %d: %w", index+1, err)
		}
		if _, exists := seenIDs[root.ID]; exists {
			return fmt.Errorf("full inventory root ID %q is duplicated", root.ID)
		}
		seenIDs[root.ID] = struct{}{}
		pathKey, err := declaredFullInventoryPathKey(root.Path)
		if err != nil {
			return fmt.Errorf("full inventory root %q path cannot be resolved", root.ID)
		}
		if _, exists := seenPaths[pathKey]; exists {
			return fmt.Errorf("full inventory root paths are duplicated")
		}
		seenPaths[pathKey] = struct{}{}
	}
	return nil
}

func validateFullInventoryRoot(root FullInventoryRoot) error {
	if len(root.ID) == 0 || len(root.ID) > maxFullInventoryRootIDBytes || !utf8.ValidString(root.ID) || strings.IndexByte(root.ID, 0) >= 0 || strings.TrimSpace(root.ID) != root.ID {
		return fmt.Errorf("declared ID must be a stable non-empty UTF-8 value of at most %d bytes without surrounding whitespace", maxFullInventoryRootIDBytes)
	}
	if root.Path == "" || strings.IndexByte(root.Path, 0) >= 0 {
		return fmt.Errorf("path is empty or invalid")
	}
	return nil
}

func declaredFullInventoryPathKey(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return canonicalPathKey(filepath.Clean(absolute)), nil
}

func prepareDeclaredFullInventoryRoots(ctx context.Context, declarations []FullInventoryRoot, allowNetwork bool) ([]SearchRootObservation, []ScanIssue, error) {
	paths := make([]string, len(declarations))
	declaredByPath := make(map[string]string, len(declarations))
	for index, declaration := range declarations {
		paths[index] = declaration.Path
		pathKey, err := declaredFullInventoryPathKey(declaration.Path)
		if err != nil {
			return nil, nil, err
		}
		declaredByPath[pathKey] = declaration.ID
	}

	prepared, issues, err := prepareSearchRoots(ctx, paths, allowNetwork)
	runtimeToDeclared := make(map[string]string, len(prepared))
	for index := range prepared {
		pathKey, pathErr := declaredFullInventoryPathKey(prepared[index].InputPath)
		if pathErr != nil {
			return nil, nil, fmt.Errorf("map prepared full inventory root: %w", pathErr)
		}
		declaredID, ok := declaredByPath[pathKey]
		if !ok {
			return nil, nil, fmt.Errorf("prepared full inventory root is outside its declaration")
		}
		runtimeToDeclared[prepared[index].ID] = declaredID
		prepared[index].ID = declaredID
	}
	for index := range issues {
		if issues[index].RootID == "" {
			continue
		}
		declaredID, ok := runtimeToDeclared[issues[index].RootID]
		if !ok {
			return nil, nil, fmt.Errorf("prepared full inventory issue is outside its declaration")
		}
		issues[index].RootID = declaredID
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		seen := make(map[string]struct{}, len(prepared))
		for _, root := range prepared {
			seen[root.ID] = struct{}{}
		}
		for _, declaration := range declarations {
			if _, ok := seen[declaration.ID]; !ok {
				prepared = append(prepared, SearchRootObservation{ID: declaration.ID, Status: "pending"})
			}
		}
	}
	sort.Slice(prepared, func(i, j int) bool { return prepared[i].ID < prepared[j].ID })
	sortScanIssues(issues)
	return prepared, issues, err
}

func fullInventoryRootObservations(prepared []SearchRootObservation) []FullInventoryRootObservation {
	result := make([]FullInventoryRootObservation, len(prepared))
	for index, root := range prepared {
		result[index] = FullInventoryRootObservation{
			ID:                     root.ID,
			Status:                 root.Status,
			FilesystemIdentityHint: root.FilesystemID,
			RootIdentityHint:       root.identity,
		}
	}
	return result
}

// StreamRegularFileInventory performs a deterministic, metadata-only full
// inventory. Declared roots and raw path components are emitted in bytewise
// lexical order, suitable for a sorted streaming index encoder. It retains
// only bounded directory listings and traversal tasks; regular-file records
// are handed to emit one at a time. Filesystem and budget stops are represented
// by an incomplete result. A sink error returns the partial result together
// with a wrapped error.
func StreamRegularFileInventory(ctx context.Context, roots []FullInventoryRoot, options FullInventoryOptions, emit func(RegularFileIndexEntry) error) (FullInventoryResult, error) {
	result := newFullInventoryResult(options.Limits)
	if err := options.Limits.Validate(); err != nil {
		return result, err
	}
	if err := validateFullInventoryRoots(roots, options.Limits.MaxRoots); err != nil {
		return result, err
	}
	if emit == nil {
		return result, fmt.Errorf("regular-file inventory callback is nil")
	}

	prepared, preparationIssues, err := prepareDeclaredFullInventoryRoots(ctx, roots, options.AllowNetwork)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		result.Roots = fullInventoryRootObservations(prepared)
		result.Complete = false
		for index := range result.Roots {
			if result.Roots[index].Status == "pending" {
				result.Roots[index].Status = "not_scanned_budget"
			}
		}
		for _, issue := range preparationIssues {
			addFullInventoryIssue(&result, issue)
		}
		addFullInventoryLimitHit(&result, "context_cancelled")
		addFullInventoryStop(&result, "context_cancelled")
		finishFullInventoryResult(&result)
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Roots = fullInventoryRootObservations(prepared)
	result.Complete = true
	for _, issue := range preparationIssues {
		result.Complete = false
		addFullInventoryIssue(&result, issue)
	}

	stopAll := false
	stopStatus := "not_scanned_budget"
	for rootIndex := range prepared {
		root := &prepared[rootIndex]
		reportedRoot := &result.Roots[rootIndex]
		if reportedRoot.Status == "unavailable" {
			continue
		}
		if stopAll {
			reportedRoot.Status = stopStatus
			continue
		}
		reportedRoot.Status = "scanning"
		stack := []fullInventoryTraversalTask{{directory: true, directoryTask: directoryTask{rootIndex: rootIndex, path: root.ResolvedPath, info: root.resolvedInfo, identity: root.identity}}}
		for len(stack) > 0 {
			if ctx.Err() != nil {
				markFullInventoryCancelled(&result, reportedRoot)
				stopAll = true
				break
			}

			walkTask := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if walkTask.directory {
				if result.Stats.DirectoriesOpened >= options.Limits.MaxDirectories {
					result.Complete = false
					reportedRoot.Status = "incomplete"
					addFullInventoryLimitHit(&result, "max_directories")
					addFullInventoryStop(&result, "budget_exhausted")
					stopAll = true
					break
				}
				directory := walkTask.directoryTask
				remainingEntries := options.Limits.MaxEntries - result.Stats.EntriesExamined
				if remainingEntries <= 0 {
					result.Complete = false
					reportedRoot.Status = "incomplete"
					addFullInventoryLimitHit(&result, "max_entries")
					addFullInventoryStop(&result, "budget_exhausted")
					stopAll = true
					break
				}
				readLimit := min(options.Limits.MaxEntriesPerDirectory, remainingEntries)
				entries, readErr := readObservedDirectoryBounded(ctx, *root, directory, readLimit)
				result.Stats.DirectoriesOpened++
				if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) || ctx.Err() != nil {
					markFullInventoryCancelled(&result, reportedRoot)
					stopAll = true
					break
				}
				// ReadDir deliberately observes N+1 entries to prove a limit was
				// exceeded. Charge every observed entry, including that sentinel,
				// against the global budget.
				result.Stats.EntriesExamined += len(entries)
				if !chargeFullInventoryTraversalNames(&result, entries, options.Limits.MaxPathBytes) {
					result.Complete = false
					reportedRoot.Status = "incomplete"
					addFullInventoryLimitHit(&result, "max_path_bytes")
					addFullInventoryStop(&result, "budget_exhausted")
					stopAll = true
					break
				}
				switch {
				case errors.Is(readErr, errDirectoryEntryLimit):
					result.Complete = false
					reportedRoot.Status = "incomplete"
					if remainingEntries <= options.Limits.MaxEntriesPerDirectory {
						addFullInventoryLimitHit(&result, "max_entries")
						addFullInventoryStop(&result, "budget_exhausted")
						stopAll = true
						break
					}
					addFullInventoryLimitHit(&result, "max_entries_per_directory")
					addFullInventoryIssue(&result, ScanIssue{Code: "scan.directory_entry_budget", RootID: root.ID, RelativePath: displayRelative(directory.components), Message: "directory exceeded the per-directory entry budget"})
					continue
				case readErr != nil:
					result.Complete = false
					reportedRoot.Status = "incomplete"
					addFullInventoryIssue(&result, ScanIssue{Code: "scan.read_directory", RootID: root.ID, RelativePath: displayRelative(directory.components), Message: "directory could not be read"})
					continue
				}
				if stopAll {
					break
				}
				sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
				for index := len(entries) - 1; index >= 0; index-- {
					stack = append(stack, fullInventoryTraversalTask{
						parentPath:       directory.path,
						parentComponents: directory.components,
						parentDepth:      directory.depth,
						entryName:        entries[index].Name(),
					})
				}
				continue
			}

			components := appendComponents(walkTask.parentComponents, walkTask.entryName)
			entryPath := filepath.Join(walkTask.parentPath, walkTask.entryName)
			info, statErr := os.Lstat(entryPath)
			if statErr != nil {
				result.Complete = false
				reportedRoot.Status = "incomplete"
				addFullInventoryIssue(&result, ScanIssue{Code: "scan.lstat", RootID: root.ID, RelativePath: displayRelative(components), Message: "directory entry changed or could not be inspected"})
				continue
			}
			if info.Mode()&os.ModeSymlink != 0 {
				result.Stats.SymlinksSkipped++
				continue
			}
			if isReparsePoint(info) {
				result.Stats.ReparseSkipped++
				continue
			}
			entryFilesystemID, identityOK := filesystemIdentity(entryPath, info)
			if !identityOK {
				result.Complete = false
				reportedRoot.Status = "incomplete"
				addFullInventoryIssue(&result, ScanIssue{Code: "scan.filesystem_identity_unavailable", RootID: root.ID, RelativePath: displayRelative(components), Message: "directory entry filesystem identity could not be established"})
				continue
			}
			if entryFilesystemID != root.FilesystemID {
				result.Stats.MountsSkipped++
				continue
			}
			if info.IsDir() {
				if walkTask.parentDepth >= options.Limits.MaxDepth {
					result.Complete = false
					reportedRoot.Status = "incomplete"
					addFullInventoryLimitHit(&result, "max_depth")
					continue
				}
				directoryIdentity, ok := fileIdentity(entryPath, info)
				if !ok {
					result.Complete = false
					reportedRoot.Status = "incomplete"
					addFullInventoryIssue(&result, ScanIssue{Code: "scan.directory_identity_unavailable", RootID: root.ID, RelativePath: displayRelative(components), Message: "directory identity could not be established"})
					continue
				}
				stack = append(stack, fullInventoryTraversalTask{directory: true, directoryTask: directoryTask{rootIndex: rootIndex, path: entryPath, components: components, depth: walkTask.parentDepth + 1, info: info, identity: directoryIdentity}})
				continue
			}
			if !info.Mode().IsRegular() {
				result.Stats.SpecialSkipped++
				continue
			}

			result.Stats.RegularFilesSeen++
			if result.Stats.FilesEmitted >= options.Limits.MaxFiles {
				result.Complete = false
				reportedRoot.Status = "incomplete"
				addFullInventoryLimitHit(&result, "max_files")
				addFullInventoryStop(&result, "budget_exhausted")
				stopAll = true
				break
			}
			pathBytes := componentsByteLength(components)
			if pathBytes > options.Limits.MaxPathBytes-result.Stats.EmittedPathBytes {
				result.Complete = false
				reportedRoot.Status = "incomplete"
				addFullInventoryLimitHit(&result, "max_path_bytes")
				addFullInventoryStop(&result, "budget_exhausted")
				stopAll = true
				break
			}
			confirmedInfo, confirmErr := os.Lstat(entryPath)
			if confirmErr != nil || IsLinkLike(confirmedInfo) || !confirmedInfo.Mode().IsRegular() || !os.SameFile(info, confirmedInfo) || info.Size() != confirmedInfo.Size() || !info.ModTime().Equal(confirmedInfo.ModTime()) {
				result.Complete = false
				reportedRoot.Status = "incomplete"
				addFullInventoryIssue(&result, ScanIssue{Code: "scan.file_changed", RootID: root.ID, RelativePath: displayRelative(components), Message: "regular file changed while its index record was captured"})
				continue
			}
			modifiedAt := confirmedInfo.ModTime().UTC().Round(0)
			modifiedUnixNanos := modifiedAt.UnixNano()
			if modifiedUnixNanos < 0 || !time.Unix(0, modifiedUnixNanos).UTC().Equal(modifiedAt) {
				result.Complete = false
				reportedRoot.Status = "incomplete"
				addFullInventoryIssue(&result, ScanIssue{Code: "scan.file_mtime_unsupported", RootID: root.ID, RelativePath: displayRelative(components), Message: "regular file modification time cannot be represented by the index format"})
				continue
			}
			identityHint, hintOK := fileIdentity(entryPath, confirmedInfo)
			if !hintOK {
				identityHint = ""
				result.Stats.IdentityHintsUnavailable++
			}
			record := RegularFileIndexEntry{
				RootID:                      root.ID,
				RelativeComponentsRawBase64: base64Components(components),
				SizeBytes:                   confirmedInfo.Size(),
				ModifiedUnixNanos:           modifiedUnixNanos,
				IdentityHint:                identityHint,
			}
			if ctx.Err() != nil {
				markFullInventoryCancelled(&result, reportedRoot)
				stopAll = true
				break
			}
			emitErr := emit(record)
			if emitErr != nil {
				result.Complete = false
				reportedRoot.Status = "incomplete"
				addFullInventoryStop(&result, "callback_failed")
				stopAll = true
				stopStatus = "not_scanned_callback"
				markPendingFullInventoryRoots(&result, stopStatus)
				finishFullInventoryResult(&result)
				return result, fmt.Errorf("stream regular-file inventory callback: %w", emitErr)
			}
			result.Stats.FilesEmitted++
			result.Stats.EmittedPathBytes += pathBytes
			if ctx.Err() != nil {
				markFullInventoryCancelled(&result, reportedRoot)
				stopAll = true
				break
			}
		}
		if reportedRoot.Status == "scanning" {
			reportedRoot.Status = "complete"
		}
	}

	if stopAll {
		markPendingFullInventoryRoots(&result, stopStatus)
	}
	finishFullInventoryResult(&result)
	return result, nil
}

func chargeFullInventoryTraversalNames(result *FullInventoryResult, entries []os.DirEntry, limit int64) bool {
	for _, entry := range entries {
		nameBytes := int64(len(entry.Name()))
		result.Stats.TraversalNameBytes += nameBytes
		if result.Stats.TraversalNameBytes > limit {
			return false
		}
	}
	return true
}

// IndexedReobservation carries freshly observed hints beside live proof-open
// authority. Callers may compare these hints with an index, but this function
// never accepts or trusts stored hints.
type IndexedReobservation struct {
	Observation            FileObservation `json:"observation"`
	FilesystemIdentityHint string          `json:"filesystem_identity_hint"`
	RootIdentityHint       string          `json:"root_identity_hint"`
	FileIdentityHint       string          `json:"file_identity_hint"`
}

// ReobserveIndexedRegular constructs fresh live authority from a declared
// profile root and raw relative components. The resulting FileObservation IDs
// are bound to the caller-declared root ID.
func ReobserveIndexedRegular(ctx context.Context, declaredRoot FullInventoryRoot, relativeComponentsRawBase64 []string, allowNetwork bool) (IndexedReobservation, error) {
	if err := ctx.Err(); err != nil {
		return IndexedReobservation{}, err
	}
	if err := validateFullInventoryRoot(declaredRoot); err != nil {
		return IndexedReobservation{}, fmt.Errorf("indexed storage root: %w", err)
	}
	if len(relativeComponentsRawBase64) == 0 || len(relativeComponentsRawBase64) > hardMaxScanDepth+1 {
		return IndexedReobservation{}, fmt.Errorf("indexed relative path must contain 1..%d components", hardMaxScanDepth+1)
	}
	components := make([]string, len(relativeComponentsRawBase64))
	for index, encoded := range relativeComponentsRawBase64 {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || base64.StdEncoding.EncodeToString(raw) != encoded {
			return IndexedReobservation{}, fmt.Errorf("indexed relative component %d is not canonical base64", index)
		}
		component := string(raw)
		invalidSeparator := strings.ContainsRune(component, filepath.Separator)
		if CurrentSemantics().Windows {
			invalidSeparator = strings.ContainsAny(component, "/\\") || !utf8.Valid(raw)
		}
		if component == "" || component == "." || component == ".." || invalidSeparator || strings.IndexByte(component, 0) >= 0 {
			return IndexedReobservation{}, fmt.Errorf("indexed relative component %d is unsafe", index)
		}
		components[index] = component
	}
	if componentsByteLength(components) > hardMaxRetainedPathBytes {
		return IndexedReobservation{}, fmt.Errorf("indexed relative path exceeds %d bytes", hardMaxRetainedPathBytes)
	}

	prepared, _, err := prepareDeclaredFullInventoryRoots(ctx, []FullInventoryRoot{declaredRoot}, allowNetwork)
	if err != nil {
		return IndexedReobservation{}, fmt.Errorf("prepare indexed storage root: %w", err)
	}
	if len(prepared) != 1 || prepared[0].Status == "unavailable" {
		return IndexedReobservation{}, fmt.Errorf("indexed storage root is unavailable")
	}
	root := prepared[0]
	current := root.ResolvedPath
	var info os.FileInfo
	for index, component := range components {
		if err := ctx.Err(); err != nil {
			return IndexedReobservation{}, err
		}
		current = filepath.Join(current, component)
		relative, relErr := filepath.Rel(root.ResolvedPath, current)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return IndexedReobservation{}, fmt.Errorf("indexed relative path escapes its storage root")
		}
		info, err = os.Lstat(current)
		if err != nil {
			return IndexedReobservation{}, fmt.Errorf("reobserve indexed component %d: %w", index, err)
		}
		if IsLinkLike(info) {
			return IndexedReobservation{}, fmt.Errorf("indexed path component %d is a symbolic link or reparse point", index)
		}
		if index < len(components)-1 && !info.IsDir() {
			return IndexedReobservation{}, fmt.Errorf("indexed path component %d is not a directory", index)
		}
		filesystemID, ok := filesystemIdentity(current, info)
		if !ok || filesystemID != root.FilesystemID {
			return IndexedReobservation{}, fmt.Errorf("indexed path component %d crossed or lost its filesystem identity", index)
		}
	}
	if info == nil || !info.Mode().IsRegular() {
		return IndexedReobservation{}, fmt.Errorf("indexed path does not name a regular file")
	}
	identity, ok := fileIdentity(current, info)
	if !ok {
		return IndexedReobservation{}, fmt.Errorf("live indexed file identity is unavailable")
	}
	confirmedInfo, err := os.Lstat(current)
	if err != nil || IsLinkLike(confirmedInfo) || !confirmedInfo.Mode().IsRegular() || !os.SameFile(info, confirmedInfo) || info.Size() != confirmedInfo.Size() || !info.ModTime().Equal(confirmedInfo.ModTime()) {
		return IndexedReobservation{}, fmt.Errorf("indexed regular file changed while it was reobserved")
	}
	confirmedIdentity, ok := fileIdentity(current, confirmedInfo)
	if !ok || confirmedIdentity != identity {
		return IndexedReobservation{}, fmt.Errorf("indexed regular file identity changed while it was reobserved")
	}
	if err := ctx.Err(); err != nil {
		return IndexedReobservation{}, err
	}
	return IndexedReobservation{
		Observation:            makeFileObservation(root, components, confirmedInfo, confirmedIdentity),
		FilesystemIdentityHint: root.FilesystemID,
		RootIdentityHint:       root.identity,
		FileIdentityHint:       confirmedIdentity,
	}, nil
}

func newFullInventoryResult(limits FullInventoryLimits) FullInventoryResult {
	return FullInventoryResult{
		Complete:        false,
		PathConfinement: "scan_identity_guarded_best_effort_non_atomic",
		Limits:          limits,
		Roots:           []FullInventoryRootObservation{},
		LimitHits:       []string{},
		StopReasons:     []string{},
		Issues:          []ScanIssue{},
		Warnings: []string{
			"metadata reads can update atime or trigger remote/cloud filesystem activity",
			"filesystem, root, and file identity hints are stale-detection hints, never reopen authority",
		},
	}
}

func markFullInventoryCancelled(result *FullInventoryResult, root *FullInventoryRootObservation) {
	result.Complete = false
	root.Status = "incomplete"
	addFullInventoryLimitHit(result, "context_cancelled")
	addFullInventoryStop(result, "context_cancelled")
}

func markPendingFullInventoryRoots(result *FullInventoryResult, status string) {
	for index := range result.Roots {
		if result.Roots[index].Status == "pending" {
			result.Roots[index].Status = status
		}
	}
}

func addFullInventoryLimitHit(result *FullInventoryResult, code string) {
	for _, existing := range result.LimitHits {
		if existing == code {
			return
		}
	}
	result.LimitHits = append(result.LimitHits, code)
}

func addFullInventoryStop(result *FullInventoryResult, code string) {
	for _, existing := range result.StopReasons {
		if existing == code {
			return
		}
	}
	result.StopReasons = append(result.StopReasons, code)
}

func addFullInventoryIssue(result *FullInventoryResult, issue ScanIssue) {
	if len(result.Issues) < result.Limits.MaxIssues {
		result.Issues = append(result.Issues, issue)
	} else {
		result.Stats.IssueOverflow++
	}
}

func finishFullInventoryResult(result *FullInventoryResult) {
	sort.Strings(result.LimitHits)
	sort.Strings(result.StopReasons)
	sortScanIssues(result.Issues)
}
