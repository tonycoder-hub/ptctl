package storage

import (
	"fmt"
	"os"
	"path/filepath"
)

type ProbeResult struct {
	Path         string        `json:"path"`
	ResolvedPath string        `json:"resolved_path"`
	Exists       bool          `json:"exists"`
	Directory    bool          `json:"directory"`
	Semantics    PathSemantics `json:"conservative_semantics"`
	WriteProbe   string        `json:"write_probe"`
	RandomRead   bool          `json:"random_read"`
	SeedableView bool          `json:"seedable_view"`
	Warnings     []string      `json:"warnings,omitempty"`
}

// ProbeReadOnly intentionally performs no temporary writes. Filesystem
// features such as reflink and atomic rename remain unknown until a future,
// explicitly effectful probe is requested.
func ProbeReadOnly(path string) (ProbeResult, error) {
	result := ProbeResult{Path: path, Semantics: CurrentSemantics(), WriteProbe: "not_run"}
	abs, err := filepath.Abs(path)
	if err != nil {
		return result, fmt.Errorf("resolve storage path: %w", err)
	}
	result.Path = abs
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return result, fmt.Errorf("storage path does not exist")
		}
		return result, fmt.Errorf("inspect storage path: %w", err)
	}
	result.Exists = true
	result.Directory = info.IsDir()
	if !result.Directory {
		return result, fmt.Errorf("storage path is not a directory")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return result, fmt.Errorf("resolve storage path: %w", err)
	}
	result.ResolvedPath = resolved
	result.RandomRead = true
	result.SeedableView = true
	result.Warnings = append(result.Warnings, "write capabilities, atomic rename, reflink, hardlink, fsync, and remote consistency were not probed")
	return result, nil
}
