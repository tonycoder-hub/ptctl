package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	DefaultMaxSearchRoots         = 8
	DefaultMaxScanDepth           = 32
	DefaultMaxScanDirectories     = 25_000
	DefaultMaxScanEntries         = 100_000
	DefaultMaxEntriesPerDirectory = 50_000
	DefaultMaxRetainedCandidates  = 10_000
	DefaultMaxRetainedPathBytes   = 16 << 20
	DefaultMaxScanIssues          = 50

	hardMaxSearchRoots         = 32
	hardMaxScanDepth           = 64
	hardMaxScanDirectories     = 100_000
	hardMaxScanEntries         = 500_000
	hardMaxEntriesPerDirectory = 200_000
	hardMaxRetainedCandidates  = 50_000
	hardMaxRetainedPathBytes   = 64 << 20
	hardMaxScanIssues          = 200
)

// InventoryLimits are mandatory safety budgets. A caller may tighten them but
// cannot disable them or raise them beyond the process hard limits.
type InventoryLimits struct {
	MaxRoots               int   `json:"max_roots"`
	MaxDepth               int   `json:"max_depth"`
	MaxDirectories         int   `json:"max_directories"`
	MaxEntries             int   `json:"max_entries"`
	MaxEntriesPerDirectory int   `json:"max_entries_per_directory"`
	MaxCandidates          int   `json:"max_retained_candidates"`
	MaxPathBytes           int64 `json:"max_retained_path_bytes"`
	MaxIssues              int   `json:"max_issues"`
}

func DefaultInventoryLimits() InventoryLimits {
	return InventoryLimits{
		MaxRoots:               DefaultMaxSearchRoots,
		MaxDepth:               DefaultMaxScanDepth,
		MaxDirectories:         DefaultMaxScanDirectories,
		MaxEntries:             DefaultMaxScanEntries,
		MaxEntriesPerDirectory: DefaultMaxEntriesPerDirectory,
		MaxCandidates:          DefaultMaxRetainedCandidates,
		MaxPathBytes:           DefaultMaxRetainedPathBytes,
		MaxIssues:              DefaultMaxScanIssues,
	}
}

func (limits InventoryLimits) Validate() error {
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
		{"max retained candidates", int64(limits.MaxCandidates), hardMaxRetainedCandidates},
		{"max retained path bytes", limits.MaxPathBytes, hardMaxRetainedPathBytes},
		{"max issues", int64(limits.MaxIssues), hardMaxScanIssues},
	}
	for _, check := range checks {
		if check.value <= 0 || check.value > check.hard {
			return fmt.Errorf("%s must be in 1..%d", check.name, check.hard)
		}
	}
	return nil
}

type InventoryOptions struct {
	Limits       InventoryLimits
	WantedSizes  []int64
	AllowNetwork bool
}

type SearchRootObservation struct {
	ID           string `json:"id"`
	InputPath    string `json:"input_path,omitempty"`
	ResolvedPath string `json:"resolved_path,omitempty"`
	FilesystemID string `json:"filesystem_id,omitempty"`
	Status       string `json:"status"`

	resolvedInfo os.FileInfo
	identity     string
}

type FileAlias struct {
	RootID                      string   `json:"root_id"`
	RelativePath                string   `json:"relative_path"`
	RelativeComponentsRawBase64 []string `json:"relative_components_raw_base64"`
}

// FileObservation is a scan-time observation, not a durable file identity.
// The unexported root and identity fields force callers to resolve and recheck
// the path before content is trusted.
type FileObservation struct {
	ObservationID               string      `json:"observation_id"`
	RootID                      string      `json:"root_id"`
	RelativePath                string      `json:"relative_path"`
	RelativeComponentsRawBase64 []string    `json:"relative_components_raw_base64"`
	SizeBytes                   int64       `json:"size_bytes"`
	ModifiedAt                  time.Time   `json:"modified_at"`
	Aliases                     []FileAlias `json:"aliases"`

	rootPath     string
	rootInfo     os.FileInfo
	rootIdentity string
	filesystemID string
	components   []string
	info         os.FileInfo
	identity     string
}

type InventoryStats struct {
	DirectoriesOpened  int   `json:"directories_opened"`
	EntriesExamined    int   `json:"entries_examined"`
	FilesExamined      int   `json:"files_examined"`
	CandidatesRetained int   `json:"candidates_retained"`
	RetainedPathBytes  int64 `json:"retained_path_bytes"`
	SymlinksSkipped    int   `json:"symlinks_skipped"`
	ReparseSkipped     int   `json:"reparse_points_skipped"`
	MountsSkipped      int   `json:"mounts_skipped"`
	SpecialSkipped     int   `json:"special_files_skipped"`
	AliasesCollapsed   int   `json:"aliases_collapsed"`
	IssueOverflow      int   `json:"issue_overflow"`
}

type ScanIssue struct {
	Code         string `json:"code"`
	RootID       string `json:"root_id,omitempty"`
	RelativePath string `json:"relative_path,omitempty"`
	Message      string `json:"message"`
}

type InventoryResult struct {
	Complete        bool                    `json:"complete"`
	PathConfinement string                  `json:"path_confinement"`
	Limits          InventoryLimits         `json:"limits"`
	Roots           []SearchRootObservation `json:"search_roots"`
	Candidates      []FileObservation       `json:"candidates"`
	Stats           InventoryStats          `json:"stats"`
	LimitHits       []string                `json:"limit_hits"`
	Issues          []ScanIssue             `json:"issues"`
	Warnings        []string                `json:"warnings"`
}

type directoryTask struct {
	rootIndex  int
	path       string
	components []string
	depth      int
	info       os.FileInfo
	identity   string
}

// InventoryCandidates performs a bounded metadata-only scan. It never follows
// symbolic links or reparse points, never crosses a filesystem boundary, and
// retains only regular files whose size is requested by the caller.
func InventoryCandidates(ctx context.Context, roots []string, options InventoryOptions) (InventoryResult, error) {
	result := InventoryResult{
		Complete:        true,
		PathConfinement: "scan_identity_guarded_best_effort_non_atomic",
		Limits:          options.Limits,
		Roots:           []SearchRootObservation{},
		Candidates:      []FileObservation{},
		LimitHits:       []string{},
		Issues:          []ScanIssue{},
		Warnings: []string{
			"metadata reads can update atime or trigger remote/cloud filesystem activity",
			"every proof open is bound to the scan-time file identity; root and path-component checks remain non-atomic",
		},
	}
	if err := options.Limits.Validate(); err != nil {
		return result, err
	}
	if len(roots) == 0 || len(roots) > options.Limits.MaxRoots {
		return result, fmt.Errorf("search roots must contain 1..%d paths", options.Limits.MaxRoots)
	}
	wanted := make(map[int64]struct{}, len(options.WantedSizes))
	for _, size := range options.WantedSizes {
		if size < 0 {
			return result, fmt.Errorf("wanted file size must not be negative")
		}
		wanted[size] = struct{}{}
	}
	prepared, preparationIssues, err := prepareSearchRoots(ctx, roots, options.AllowNetwork)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		result.Complete = false
		result.Roots = prepared
		for index := range result.Roots {
			if result.Roots[index].Status == "pending" {
				result.Roots[index].Status = "not_scanned_budget"
			}
		}
		sortScanIssues(preparationIssues)
		for _, issue := range preparationIssues {
			addScanIssue(&result, options.Limits.MaxIssues, issue)
		}
		addLimitHit(&result, "context_cancelled")
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Roots = prepared
	sortScanIssues(preparationIssues)
	if len(preparationIssues) > 0 {
		result.Complete = false
		for _, issue := range preparationIssues {
			addScanIssue(&result, options.Limits.MaxIssues, issue)
		}
	}
	if len(wanted) == 0 {
		for i := range result.Roots {
			if result.Roots[i].Status == "pending" {
				result.Roots[i].Status = "not_needed"
			}
		}
		sortScanIssues(result.Issues)
		return result, nil
	}
	seenIdentity := make(map[string]int)
	stopAll := false
	for rootIndex := range result.Roots {
		if result.Roots[rootIndex].Status == "unavailable" {
			continue
		}
		if stopAll {
			result.Roots[rootIndex].Status = "not_scanned_budget"
			continue
		}
		root := &result.Roots[rootIndex]
		root.Status = "scanning"
		stack := []directoryTask{{rootIndex: rootIndex, path: root.ResolvedPath, info: root.resolvedInfo, identity: root.identity}}
		for len(stack) > 0 {
			if err := ctx.Err(); err != nil {
				result.Complete = false
				addLimitHit(&result, "context_cancelled")
				root.Status = "incomplete"
				stopAll = true
				break
			}
			if result.Stats.DirectoriesOpened >= options.Limits.MaxDirectories {
				result.Complete = false
				addLimitHit(&result, "max_directories")
				root.Status = "incomplete"
				stopAll = true
				break
			}
			task := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			entries, readErr := readObservedDirectoryBounded(ctx, *root, task, options.Limits.MaxEntriesPerDirectory)
			result.Stats.DirectoriesOpened++
			if len(entries) > options.Limits.MaxEntries-result.Stats.EntriesExamined {
				result.Complete = false
				addLimitHit(&result, "max_entries")
				root.Status = "incomplete"
				stopAll = true
				break
			}
			result.Stats.EntriesExamined += len(entries)
			if errors.Is(readErr, errDirectoryEntryLimit) {
				result.Complete = false
				root.Status = "incomplete"
				addLimitHit(&result, "max_entries_per_directory")
				addScanIssue(&result, options.Limits.MaxIssues, ScanIssue{Code: "scan.directory_entry_budget", RootID: root.ID, RelativePath: displayRelative(task.components), Message: "directory exceeded the per-directory entry budget"})
				continue
			}
			if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
				result.Complete = false
				root.Status = "incomplete"
				addLimitHit(&result, "context_cancelled")
				stopAll = true
				break
			}
			if readErr != nil {
				result.Complete = false
				root.Status = "incomplete"
				addScanIssue(&result, options.Limits.MaxIssues, ScanIssue{Code: "scan.read_directory", RootID: root.ID, RelativePath: displayRelative(task.components), Message: "directory could not be read"})
				continue
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			children := make([]directoryTask, 0)
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					result.Complete = false
					addLimitHit(&result, "context_cancelled")
					root.Status = "incomplete"
					stopAll = true
					break
				}
				components := appendComponents(task.components, entry.Name())
				entryPath := filepath.Join(task.path, entry.Name())
				info, statErr := os.Lstat(entryPath)
				if statErr != nil {
					result.Complete = false
					root.Status = "incomplete"
					addScanIssue(&result, options.Limits.MaxIssues, ScanIssue{Code: "scan.lstat", RootID: root.ID, RelativePath: displayRelative(components), Message: "directory entry changed or could not be inspected"})
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
					root.Status = "incomplete"
					addScanIssue(&result, options.Limits.MaxIssues, ScanIssue{Code: "scan.filesystem_identity_unavailable", RootID: root.ID, RelativePath: displayRelative(components), Message: "directory entry filesystem identity could not be established"})
					continue
				}
				if entryFilesystemID != root.FilesystemID {
					result.Stats.MountsSkipped++
					continue
				}
				if info.IsDir() {
					if task.depth >= options.Limits.MaxDepth {
						result.Complete = false
						root.Status = "incomplete"
						addLimitHit(&result, "max_depth")
						continue
					}
					directoryIdentity, ok := fileIdentity(entryPath, info)
					if !ok {
						result.Complete = false
						root.Status = "incomplete"
						addScanIssue(&result, options.Limits.MaxIssues, ScanIssue{Code: "scan.directory_identity_unavailable", RootID: root.ID, RelativePath: displayRelative(components), Message: "directory identity could not be established"})
						continue
					}
					children = append(children, directoryTask{rootIndex: task.rootIndex, path: entryPath, components: components, depth: task.depth + 1, info: info, identity: directoryIdentity})
					continue
				}
				if !info.Mode().IsRegular() {
					result.Stats.SpecialSkipped++
					continue
				}
				result.Stats.FilesExamined++
				if _, ok := wanted[info.Size()]; !ok {
					continue
				}
				identity, ok := fileIdentity(entryPath, info)
				if !ok {
					result.Complete = false
					root.Status = "incomplete"
					addScanIssue(&result, options.Limits.MaxIssues, ScanIssue{Code: "scan.file_identity_unavailable", RootID: root.ID, RelativePath: displayRelative(components), Message: "regular file identity could not be established"})
					continue
				}
				confirmedInfo, confirmErr := os.Lstat(entryPath)
				if confirmErr != nil || IsLinkLike(confirmedInfo) || !confirmedInfo.Mode().IsRegular() || !os.SameFile(info, confirmedInfo) {
					result.Complete = false
					root.Status = "incomplete"
					addScanIssue(&result, options.Limits.MaxIssues, ScanIssue{Code: "scan.file_changed", RootID: root.ID, RelativePath: displayRelative(components), Message: "regular file changed while its identity was captured"})
					continue
				}
				if existing, ok := seenIdentity[identity]; ok {
					alias := makeAlias(root.ID, components)
					if len(result.Candidates[existing].Aliases) < 8 {
						aliasBytes := componentsByteLength(components)
						if aliasBytes > options.Limits.MaxPathBytes-result.Stats.RetainedPathBytes {
							result.Complete = false
							root.Status = "incomplete"
							addLimitHit(&result, "max_retained_path_bytes")
							stopAll = true
							break
						}
						result.Candidates[existing].Aliases = append(result.Candidates[existing].Aliases, alias)
						result.Stats.RetainedPathBytes += aliasBytes
					}
					result.Stats.AliasesCollapsed++
					continue
				}
				pathBytes := componentsByteLength(components)
				if len(result.Candidates) >= options.Limits.MaxCandidates || pathBytes > options.Limits.MaxPathBytes-result.Stats.RetainedPathBytes {
					result.Complete = false
					if len(result.Candidates) >= options.Limits.MaxCandidates {
						addLimitHit(&result, "max_retained_candidates")
					} else {
						addLimitHit(&result, "max_retained_path_bytes")
					}
					root.Status = "incomplete"
					stopAll = true
					break
				}
				candidate := makeFileObservation(*root, components, info, identity)
				seenIdentity[identity] = len(result.Candidates)
				result.Candidates = append(result.Candidates, candidate)
				result.Stats.RetainedPathBytes += pathBytes
				result.Stats.CandidatesRetained++
			}
			if stopAll {
				break
			}
			for i := len(children) - 1; i >= 0; i-- {
				stack = append(stack, children[i])
			}
		}
		if root.Status == "scanning" {
			root.Status = "complete"
		}
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		if result.Candidates[i].RootID != result.Candidates[j].RootID {
			return result.Candidates[i].RootID < result.Candidates[j].RootID
		}
		return rawComponentsKey(result.Candidates[i].components) < rawComponentsKey(result.Candidates[j].components)
	})
	sortScanIssues(result.Issues)
	return result, nil
}

var errDirectoryEntryLimit = errors.New("directory entry limit exceeded")

func readObservedDirectoryBounded(ctx context.Context, root SearchRootObservation, task directoryTask, limit int) ([]os.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if task.info == nil || task.identity == "" || root.resolvedInfo == nil {
		return nil, fmt.Errorf("directory task has no scan identity")
	}
	rootInfo, err := os.Lstat(root.ResolvedPath)
	if err != nil || IsLinkLike(rootInfo) || !rootInfo.IsDir() || !os.SameFile(root.resolvedInfo, rootInfo) {
		return nil, fmt.Errorf("search root changed before directory read")
	}
	current := root.ResolvedPath
	currentInfo := rootInfo
	for index, component := range task.components {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current = filepath.Join(current, component)
		currentInfo, err = os.Lstat(current)
		if err != nil {
			return nil, fmt.Errorf("inspect directory component %d: %w", index, err)
		}
		if IsLinkLike(currentInfo) || !currentInfo.IsDir() {
			return nil, fmt.Errorf("directory component %d changed type", index)
		}
	}
	currentIdentity, identityOK := fileIdentity(current, currentInfo)
	if !identityOK || currentIdentity != task.identity || !os.SameFile(task.info, currentInfo) {
		return nil, fmt.Errorf("directory identity changed before read")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := os.Open(current)
	if err != nil {
		return nil, err
	}
	openedInfo, err := directory.Stat()
	if err != nil {
		_ = directory.Close()
		return nil, err
	}
	openedIdentity, identityOK := openedFileIdentity(directory)
	if !identityOK || openedIdentity != task.identity || !openedInfo.IsDir() || !os.SameFile(task.info, openedInfo) {
		_ = directory.Close()
		return nil, fmt.Errorf("opened directory does not match the scan identity")
	}
	if err := ctx.Err(); err != nil {
		_ = directory.Close()
		return nil, err
	}
	entries, readErr := directory.ReadDir(limit + 1)
	closeErr := directory.Close()
	if len(entries) > limit {
		return entries, errDirectoryEntryLimit
	}
	if readErr != nil && readErr != io.EOF {
		return entries, readErr
	}
	if closeErr != nil {
		return entries, closeErr
	}
	return entries, nil
}

func prepareSearchRoots(ctx context.Context, paths []string, allowNetwork bool) ([]SearchRootObservation, []ScanIssue, error) {
	roots := make([]SearchRootObservation, 0, len(paths))
	issues := make([]ScanIssue, 0)
	validRoots := 0
	for inputIndex, input := range paths {
		if err := ctx.Err(); err != nil {
			return roots, issues, err
		}
		if input == "" {
			return nil, nil, fmt.Errorf("search root %d is empty", inputIndex+1)
		}
		abs, err := filepath.Abs(input)
		if err != nil {
			root := unavailableSearchRoot(input)
			roots = append(roots, root)
			issues = append(issues, ScanIssue{Code: "scan.root_unavailable", RootID: root.ID, RelativePath: ".", Message: "search root could not be resolved"})
			continue
		}
		abs = filepath.Clean(abs)
		if isNetworkPath(abs) && !allowNetwork {
			return nil, nil, fmt.Errorf("search root %d is a network path and requires --allow-network", inputIndex+1)
		}
		if err := ctx.Err(); err != nil {
			return roots, issues, err
		}
		info, err := os.Lstat(abs)
		if contextErr := ctx.Err(); contextErr != nil {
			return roots, issues, contextErr
		}
		if err != nil {
			root := unavailableSearchRoot(abs)
			roots = append(roots, root)
			issues = append(issues, ScanIssue{Code: "scan.root_unavailable", RootID: root.ID, RelativePath: ".", Message: "search root could not be inspected"})
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || isReparsePoint(info) {
			root := unavailableSearchRoot(abs)
			roots = append(roots, root)
			issues = append(issues, ScanIssue{Code: "scan.root_link_rejected", RootID: root.ID, RelativePath: ".", Message: "search root is a symbolic link or reparse point"})
			continue
		}
		if !info.IsDir() {
			root := unavailableSearchRoot(abs)
			roots = append(roots, root)
			issues = append(issues, ScanIssue{Code: "scan.root_not_directory", RootID: root.ID, RelativePath: ".", Message: "search root is not a directory"})
			continue
		}
		if err := ctx.Err(); err != nil {
			return roots, issues, err
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if contextErr := ctx.Err(); contextErr != nil {
			return roots, issues, contextErr
		}
		if err != nil {
			root := unavailableSearchRoot(abs)
			roots = append(roots, root)
			issues = append(issues, ScanIssue{Code: "scan.root_unavailable", RootID: root.ID, RelativePath: ".", Message: "search root could not be resolved without links"})
			continue
		}
		if isNetworkPath(resolved) && !allowNetwork {
			return nil, nil, fmt.Errorf("search root %d resolves to a network path and requires --allow-network", inputIndex+1)
		}
		if err := ctx.Err(); err != nil {
			return roots, issues, err
		}
		filesystemID, filesystemIDOK := filesystemIdentity(resolved, info)
		if contextErr := ctx.Err(); contextErr != nil {
			return roots, issues, contextErr
		}
		if !filesystemIDOK {
			root := unavailableSearchRoot(abs)
			roots = append(roots, root)
			issues = append(issues, ScanIssue{Code: "scan.root_identity_unavailable", RootID: root.ID, RelativePath: ".", Message: "search root filesystem identity could not be established"})
			continue
		}
		if err := ctx.Err(); err != nil {
			return roots, issues, err
		}
		rootIdentity, rootIdentityOK := fileIdentity(resolved, info)
		if contextErr := ctx.Err(); contextErr != nil {
			return roots, issues, contextErr
		}
		if !rootIdentityOK {
			root := unavailableSearchRoot(abs)
			roots = append(roots, root)
			issues = append(issues, ScanIssue{Code: "scan.root_identity_unavailable", RootID: root.ID, RelativePath: ".", Message: "search root object identity could not be established"})
			continue
		}
		digest := sha256.Sum256([]byte(canonicalPathKey(resolved) + "\x00" + filesystemID))
		roots = append(roots, SearchRootObservation{
			ID:           "local:" + hex.EncodeToString(digest[:12]),
			InputPath:    abs,
			ResolvedPath: resolved,
			FilesystemID: filesystemID,
			Status:       "pending",
			resolvedInfo: info,
			identity:     rootIdentity,
		})
		validRoots++
	}
	if err := ctx.Err(); err != nil {
		return roots, issues, err
	}
	if validRoots == 0 {
		return nil, nil, fmt.Errorf("no usable search roots")
	}
	sort.Slice(roots, func(i, j int) bool {
		if (roots[i].Status == "unavailable") != (roots[j].Status == "unavailable") {
			return roots[i].Status != "unavailable"
		}
		if roots[i].Status == "unavailable" {
			return roots[i].ID < roots[j].ID
		}
		return canonicalPathKey(roots[i].ResolvedPath) < canonicalPathKey(roots[j].ResolvedPath)
	})
	for i := range roots {
		for j := 0; j < i; j++ {
			if roots[i].ID == roots[j].ID {
				return nil, nil, fmt.Errorf("search root %s is duplicated", roots[i].ID)
			}
			if roots[i].Status == "unavailable" || roots[j].Status == "unavailable" {
				continue
			}
			if rootsOverlap(roots[j].ResolvedPath, roots[i].ResolvedPath) || (roots[j].identity != "" && roots[j].identity == roots[i].identity) {
				return nil, nil, fmt.Errorf("search roots %s and %s overlap", roots[j].ID, roots[i].ID)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return roots, issues, err
	}
	sortScanIssues(issues)
	return roots, issues, nil
}

func unavailableSearchRoot(path string) SearchRootObservation {
	clean := filepath.Clean(path)
	digest := sha256.Sum256([]byte("unavailable\x00" + canonicalPathKey(clean)))
	return SearchRootObservation{ID: "local:" + hex.EncodeToString(digest[:12]), InputPath: clean, Status: "unavailable"}
}

func rootsOverlap(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	if err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))) {
		return true
	}
	rel, err = filepath.Rel(b, a)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)))
}

func makeFileObservation(root SearchRootObservation, components []string, info os.FileInfo, identity string) FileObservation {
	componentsCopy := append([]string(nil), components...)
	digest := sha256.New()
	fmt.Fprintf(digest, "%s\x00%s\x00%s\x00%d\x00%d", root.ID, rawComponentsKey(components), identity, info.Size(), info.ModTime().UnixNano())
	return FileObservation{
		ObservationID:               "sha256:" + hex.EncodeToString(digest.Sum(nil)),
		RootID:                      root.ID,
		RelativePath:                displayRelative(components),
		RelativeComponentsRawBase64: base64Components(components),
		SizeBytes:                   info.Size(),
		ModifiedAt:                  info.ModTime().UTC(),
		Aliases:                     []FileAlias{},
		rootPath:                    root.ResolvedPath,
		rootInfo:                    root.resolvedInfo,
		rootIdentity:                root.identity,
		filesystemID:                root.FilesystemID,
		components:                  componentsCopy,
		info:                        info,
		identity:                    identity,
	}
}

func makeAlias(rootID string, components []string) FileAlias {
	return FileAlias{RootID: rootID, RelativePath: displayRelative(components), RelativeComponentsRawBase64: base64Components(components)}
}

// ResolveObservedRegular re-resolves an observation beneath its original root
// without following links and verifies file identity, size, and mtime.
func (observation FileObservation) ResolveObservedRegular() (string, error) {
	return observation.ResolveObservedRegularContext(context.Background())
}

// ResolveObservedRegularContext is the cancellable form used by bounded
// discovery. Cancellation is checked before each metadata operation; an
// individual filesystem call may still be uninterruptible.
func (observation FileObservation) ResolveObservedRegularContext(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if observation.rootPath == "" || observation.rootInfo == nil || observation.info == nil {
		return "", fmt.Errorf("file observation has no live scan identity")
	}
	rootInfo, err := os.Lstat(observation.rootPath)
	if err != nil {
		return "", fmt.Errorf("re-inspect search root: %w", err)
	}
	rootIdentity, rootIdentityOK := fileIdentity(observation.rootPath, rootInfo)
	rootMatches := rootIdentityOK && observation.rootIdentity != "" && rootIdentity == observation.rootIdentity
	if observation.rootIdentity == "" {
		rootMatches = os.SameFile(observation.rootInfo, rootInfo)
	}
	if !rootMatches || rootInfo.Mode()&os.ModeSymlink != 0 || isReparsePoint(rootInfo) || !rootInfo.IsDir() {
		return "", fmt.Errorf("search root changed after inventory")
	}
	current := observation.rootPath
	for index, component := range observation.components {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		invalidSeparator := strings.ContainsRune(component, filepath.Separator)
		if CurrentSemantics().Windows {
			invalidSeparator = strings.ContainsAny(component, "/\\")
		}
		if component == "" || component == "." || component == ".." || invalidSeparator || strings.IndexByte(component, 0) >= 0 {
			return "", fmt.Errorf("invalid observed path component %d", index)
		}
		current = filepath.Join(current, component)
		rel, relErr := filepath.Rel(observation.rootPath, current)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return "", fmt.Errorf("observed path escapes its search root")
		}
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return "", fmt.Errorf("re-inspect observed component %d: %w", index, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || isReparsePoint(info) {
			return "", fmt.Errorf("observed path became a symlink or reparse point")
		}
		if index < len(observation.components)-1 && !info.IsDir() {
			return "", fmt.Errorf("observed path prefix is no longer a directory")
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := os.Lstat(current)
	if err != nil {
		return "", fmt.Errorf("re-inspect observed file: %w", err)
	}
	filesystemID, ok := filesystemIdentity(current, info)
	if observation.filesystemID != "" && (!ok || filesystemID != observation.filesystemID) {
		return "", fmt.Errorf("observed file crossed a filesystem boundary")
	}
	currentIdentity, identityOK := fileIdentity(current, info)
	identityMatches := identityOK && observation.identity != "" && currentIdentity == observation.identity
	if observation.identity == "" {
		identityMatches = os.SameFile(observation.info, info)
	}
	if !info.Mode().IsRegular() || !identityMatches || info.Size() != observation.SizeBytes || !info.ModTime().Equal(observation.info.ModTime()) {
		return "", fmt.Errorf("observed file identity or metadata changed after inventory")
	}
	return filepath.Abs(current)
}

// OpenObservedRegular opens the exact file object captured by inventory. It
// checks root confinement before the open, binds the returned handle to the
// scan-time identity, and rechecks the component chain once more afterward.
// These checks detect ordinary replacement races but are not an atomic
// filesystem snapshot; callers must keep the non-atomic assurance visible.
func (observation FileObservation) OpenObservedRegular() (*os.File, error) {
	return observation.OpenObservedRegularContext(context.Background())
}

// OpenObservedRegularContext opens the scan-time object while preserving the
// discovery cancellation budget across component rechecks.
func (observation FileObservation) OpenObservedRegularContext(ctx context.Context) (*os.File, error) {
	path, err := observation.ResolveObservedRegularContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open observed file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat opened observation: %w", err)
	}
	openedIdentity, identityOK := openedFileIdentity(file)
	if !identityOK || openedIdentity != observation.identity || !info.Mode().IsRegular() || !os.SameFile(observation.info, info) || info.Size() != observation.SizeBytes || !info.ModTime().Equal(observation.info.ModTime()) {
		_ = file.Close()
		return nil, fmt.Errorf("opened file no longer matches the scan-time identity")
	}
	recheckedPath, err := observation.ResolveObservedRegularContext(ctx)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("recheck observed path after open: %w", err)
	}
	recheckedInfo, err := os.Lstat(recheckedPath)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("re-inspect observed path after open: %w", err)
	}
	if IsLinkLike(recheckedInfo) || !os.SameFile(info, recheckedInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("observed path changed while it was opened")
	}
	return file, nil
}

func (observation FileObservation) Basename() string {
	if len(observation.components) == 0 {
		return ""
	}
	return observation.components[len(observation.components)-1]
}

func (observation FileObservation) SortKey() string {
	return observation.RootID + "\x00" + rawComponentsKey(observation.components)
}

// IsLinkLike reports path objects that must not be traversed by inventory or
// verification code. On Windows this includes every reparse point, not only
// entries Go labels as ModeSymlink.
func IsLinkLike(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink != 0 || isReparsePoint(info)
}

func addLimitHit(result *InventoryResult, code string) {
	for _, existing := range result.LimitHits {
		if existing == code {
			return
		}
	}
	result.LimitHits = append(result.LimitHits, code)
}

func addScanIssue(result *InventoryResult, limit int, issue ScanIssue) {
	if len(result.Issues) < limit {
		result.Issues = append(result.Issues, issue)
	} else {
		result.Stats.IssueOverflow++
	}
}

func sortScanIssues(issues []ScanIssue) {
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].RootID != issues[j].RootID {
			return issues[i].RootID < issues[j].RootID
		}
		if issues[i].RelativePath != issues[j].RelativePath {
			return issues[i].RelativePath < issues[j].RelativePath
		}
		if issues[i].Code != issues[j].Code {
			return issues[i].Code < issues[j].Code
		}
		return issues[i].Message < issues[j].Message
	})
}

func appendComponents(components []string, component string) []string {
	result := make([]string, len(components)+1)
	copy(result, components)
	result[len(components)] = component
	return result
}

func componentsByteLength(components []string) int64 {
	var total int64
	for _, component := range components {
		if int64(len(component)) > int64(^uint64(0)>>1)-total {
			return int64(^uint64(0) >> 1)
		}
		total += int64(len(component))
	}
	return total
}

func base64Components(components []string) []string {
	result := make([]string, len(components))
	for i, component := range components {
		result[i] = base64.StdEncoding.EncodeToString([]byte(component))
	}
	return result
}

func displayRelative(components []string) string {
	if len(components) == 0 {
		return "."
	}
	return filepath.ToSlash(filepath.Join(components...))
}

func rawComponentsKey(components []string) string {
	return strings.Join(components, "\x00")
}

func canonicalPathKey(path string) string {
	path = filepath.Clean(path)
	if CurrentSemantics().CaseSensitive {
		return path
	}
	return strings.ToLower(path)
}
