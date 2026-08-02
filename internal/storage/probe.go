package storage

import (
	"fmt"
	"os"
	"path/filepath"
)

type ProbeResult struct {
	Path              string        `json:"path"`
	ResolvedPath      string        `json:"resolved_path"`
	Exists            bool          `json:"exists"`
	Directory         bool          `json:"directory"`
	Semantics         PathSemantics `json:"conservative_semantics"`
	SemanticsEvidence string        `json:"semantics_evidence"`
	WriteProbe        string        `json:"write_probe"`
	RandomRead        string        `json:"random_read"`
	SeedableView      string        `json:"seedable_view"`
	Warnings          []string      `json:"warnings,omitempty"`
}

// ProbeReadOnly intentionally performs no temporary writes. Filesystem
// features such as reflink and atomic rename remain unknown until a future,
// explicitly effectful probe is requested.
func ProbeReadOnly(path string) (ProbeResult, error) {
	result := ProbeResult{Path: path, Semantics: CurrentSemantics(), SemanticsEvidence: "host_os_assumption", WriteProbe: "not_run"}
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
	result.RandomRead = "unknown"
	result.SeedableView = "unknown"
	result.Warnings = append(result.Warnings, "write capabilities, atomic rename, reflink, hardlink, fsync, and remote consistency were not probed")
	result.Warnings = append(result.Warnings, "case and Unicode path semantics were inferred from the host OS, not measured for this storage root")
	return result, nil
}
