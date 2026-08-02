package storage

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

type PathSemantics struct {
	Windows              bool `json:"windows"`
	CaseSensitive        bool `json:"case_sensitive"`
	UnicodeNormalization bool `json:"unicode_normalization"`
}

func CurrentSemantics() PathSemantics {
	switch runtime.GOOS {
	case "windows":
		return PathSemantics{Windows: true, CaseSensitive: false, UnicodeNormalization: true}
	case "darwin":
		return PathSemantics{CaseSensitive: false, UnicodeNormalization: true}
	default:
		return PathSemantics{CaseSensitive: true}
	}
}

func ValidateComponents(components [][]byte, semantics PathSemantics) error {
	if len(components) == 0 {
		return fmt.Errorf("path has no components")
	}
	for index, raw := range components {
		if len(raw) == 0 {
			return fmt.Errorf("path component %d is empty", index)
		}
		if strings.IndexByte(string(raw), 0) >= 0 || strings.ContainsAny(string(raw), "/\\") {
			return fmt.Errorf("path component %d contains a separator or NUL", index)
		}
		if string(raw) == "." || string(raw) == ".." {
			return fmt.Errorf("path component %d is %q", index, raw)
		}
		if semantics.Windows {
			if !utf8.Valid(raw) {
				return fmt.Errorf("Windows path component %d is not valid UTF-8 (raw base64 %s)", index, base64.StdEncoding.EncodeToString(raw))
			}
			component := string(raw)
			if strings.Contains(component, ":") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
				return fmt.Errorf("Windows path component %d has a forbidden colon or trailing dot/space", index)
			}
			if windowsReserved(component) {
				return fmt.Errorf("Windows path component %d uses a reserved device name", index)
			}
		}
		if semantics.UnicodeNormalization && utf8.Valid(raw) {
			for _, r := range string(raw) {
				if unicode.Is(unicode.Mn, r) {
					return fmt.Errorf("path component %d contains combining marks; normalization would be ambiguous", index)
				}
			}
		}
	}
	return nil
}

func ValidateManifestPaths(paths [][][]byte, semantics PathSemantics) error {
	seen := make(map[string]int, len(paths))
	for index, components := range paths {
		if err := ValidateComponents(components, semantics); err != nil {
			return fmt.Errorf("file %d: %w", index, err)
		}
		parts := make([]string, len(components))
		for i, component := range components {
			parts[i] = string(component)
			if !semantics.CaseSensitive {
				parts[i] = strings.ToLower(parts[i])
			}
		}
		key := strings.Join(parts, "\x00")
		if previous, ok := seen[key]; ok {
			return fmt.Errorf("files %d and %d collide under target path semantics", previous, index)
		}
		seen[key] = index
	}
	return nil
}

// SecureJoinExisting joins an existing path while refusing symlink traversal.
// It is intentionally conservative: callers can choose a different root
// instead of weakening this check.
func SecureJoinExisting(root string, components [][]byte, semantics PathSemantics) (string, error) {
	if err := ValidateComponents(components, semantics); err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	rootInfo, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("resolve root symlinks: %w", err)
	}
	current := rootInfo
	for index, raw := range components {
		current = filepath.Join(current, string(raw))
		rel, err := filepath.Rel(rootInfo, current)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return "", fmt.Errorf("path component %d escapes the storage root", index)
		}
		info, err := filepath.EvalSymlinks(current)
		if err != nil {
			return "", fmt.Errorf("resolve path component %d: %w", index, err)
		}
		resolvedRel, err := filepath.Rel(rootInfo, info)
		if err != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) || filepath.IsAbs(resolvedRel) {
			return "", fmt.Errorf("path component %d resolves outside the storage root", index)
		}
		current = info
	}
	return current, nil
}

func PlannedJoin(root string, components [][]byte, semantics PathSemantics) (string, error) {
	if err := ValidateComponents(components, semantics); err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	parts := make([]string, 0, len(components)+1)
	parts = append(parts, absRoot)
	for _, component := range components {
		parts = append(parts, string(component))
	}
	target := filepath.Join(parts...)
	rel, err := filepath.Rel(absRoot, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("planned path escapes the storage root")
	}
	return target, nil
}

type PathMapping struct {
	HostRoot   string `json:"host_root"`
	ClientRoot string `json:"client_root"`
	HostPath   string `json:"host_path"`
	ClientPath string `json:"client_path"`
}

func MapHostToClient(hostRoot, hostPath, clientRoot string, clientWindows bool) (PathMapping, error) {
	root, err := filepath.Abs(hostRoot)
	if err != nil {
		return PathMapping{}, fmt.Errorf("resolve host root: %w", err)
	}
	path, err := filepath.Abs(hostPath)
	if err != nil {
		return PathMapping{}, fmt.Errorf("resolve host path: %w", err)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return PathMapping{}, fmt.Errorf("host path is outside the configured root")
	}
	if rel == "." {
		rel = ""
	}
	var clientPath string
	if clientWindows {
		clientPath = windowsJoin(clientRoot, strings.Split(filepath.ToSlash(rel), "/")...)
	} else {
		clientPath = strings.TrimRight(strings.ReplaceAll(clientRoot, "\\", "/"), "/")
		if rel != "" {
			clientPath += "/" + strings.TrimLeft(filepath.ToSlash(rel), "/")
		}
	}
	return PathMapping{HostRoot: root, ClientRoot: clientRoot, HostPath: path, ClientPath: clientPath}, nil
}

func windowsReserved(component string) bool {
	base := component
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	base = strings.ToUpper(base)
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return true
	}
	return false
}

func windowsJoin(root string, components ...string) string {
	result := strings.TrimRight(strings.ReplaceAll(root, "/", "\\"), "\\")
	for _, component := range components {
		if component == "" || component == "." {
			continue
		}
		result += "\\" + strings.ReplaceAll(component, "/", "\\")
	}
	return result
}
