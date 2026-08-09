package storage

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrHostPathOutsideRoot       = errors.New("host path is outside the configured root")
	ErrInvalidClientRoot         = errors.New("invalid client namespace root")
	ErrClientPathUnrepresentable = errors.New("host-relative path cannot be represented by the client")
)

type PathSemantics struct {
	Windows              bool `json:"windows"`
	CaseSensitive        bool `json:"case_sensitive"`
	UnicodeNormalization bool `json:"unicode_normalization"`
}

func CurrentSemantics() PathSemantics {
	switch runtime.GOOS {
	case "windows":
		return PathSemantics{Windows: true, CaseSensitive: false}
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
		for _, ch := range raw {
			if ch < 0x20 || ch == 0x7f {
				return fmt.Errorf("path component %d contains a control character", index)
			}
		}
		if string(raw) == "." || string(raw) == ".." {
			return fmt.Errorf("path component %d is %q", index, raw)
		}
		if semantics.Windows {
			if !utf8.Valid(raw) {
				return fmt.Errorf("Windows path component %d is not valid UTF-8 (raw base64 %s)", index, base64.StdEncoding.EncodeToString(raw))
			}
			component := string(raw)
			if strings.ContainsAny(component, `<>:"|?*`) || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
				return fmt.Errorf("Windows path component %d has a forbidden character or trailing dot/space", index)
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
	type indexedKey struct {
		key   string
		index int
	}
	keys := make([]indexedKey, 0, len(paths))
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
		keys = append(keys, indexedKey{key: key, index: index})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].key < keys[j].key })
	for i := 1; i < len(keys); i++ {
		if strings.HasPrefix(keys[i].key, keys[i-1].key+"\x00") {
			return fmt.Errorf("files %d and %d collide because one path is a prefix of the other", keys[i-1].index, keys[i].index)
		}
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
	rootLstat, err := os.Lstat(absRoot)
	if err != nil {
		return "", fmt.Errorf("inspect root: %w", err)
	}
	if IsLinkLike(rootLstat) {
		return "", fmt.Errorf("storage root is a symbolic link or reparse point")
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("resolve root symlinks: %w", err)
	}
	current := resolvedRoot
	for index, raw := range components {
		current = filepath.Join(current, string(raw))
		rel, err := filepath.Rel(resolvedRoot, current)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return "", fmt.Errorf("path component %d escapes the storage root", index)
		}
		entryInfo, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("inspect path component %d: %w", index, err)
		}
		if IsLinkLike(entryInfo) {
			return "", fmt.Errorf("path component %d is a symbolic link or reparse point", index)
		}
		info, err := filepath.EvalSymlinks(current)
		if err != nil {
			return "", fmt.Errorf("resolve path component %d: %w", index, err)
		}
		resolvedRel, err := filepath.Rel(resolvedRoot, info)
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
	cleanClientRoot, err := validateClientRoot(clientRoot, clientWindows)
	if err != nil {
		return PathMapping{}, fmt.Errorf("%w: %v", ErrInvalidClientRoot, err)
	}
	inputRoot, err := filepath.Abs(hostRoot)
	if err != nil {
		return PathMapping{}, fmt.Errorf("resolve host root: %w", err)
	}
	root, err := filepath.EvalSymlinks(inputRoot)
	if err != nil {
		return PathMapping{}, fmt.Errorf("resolve host root aliases: %w", err)
	}
	inputPath, err := filepath.Abs(hostPath)
	if err != nil {
		return PathMapping{}, fmt.Errorf("resolve host path: %w", err)
	}
	path := inputPath
	var rel string
	resolvedPath, resolveErr := filepath.EvalSymlinks(inputPath)
	if resolveErr == nil {
		path = resolvedPath
		rel, err = confinedRelativePath(root, path)
		if err != nil {
			return PathMapping{}, err
		}
	} else {
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return PathMapping{}, fmt.Errorf("resolve host path aliases: %w", resolveErr)
		}
		if rel, err = confinedRelativePath(root, inputPath); err != nil {
			rel, err = confinedRelativePath(inputRoot, inputPath)
			if err != nil {
				return PathMapping{}, err
			}
		}
		path, err = resolvePlannedHostPath(root, filepath.Join(root, rel))
		if err != nil {
			return PathMapping{}, err
		}
		rel, err = confinedRelativePath(root, path)
		if err != nil {
			return PathMapping{}, err
		}
	}
	if rel == "." {
		rel = ""
	}
	relParts := []string{}
	if rel != "" {
		relParts = strings.Split(filepath.ToSlash(rel), "/")
		rawParts := make([][]byte, len(relParts))
		for i, part := range relParts {
			rawParts[i] = []byte(part)
		}
		if err := ValidateComponents(rawParts, PathSemantics{Windows: clientWindows, CaseSensitive: !clientWindows}); err != nil {
			return PathMapping{}, fmt.Errorf("%w: %v", ErrClientPathUnrepresentable, err)
		}
	}
	var clientPath string
	if clientWindows {
		clientPath = windowsJoin(cleanClientRoot, relParts...)
	} else {
		clientPath = cleanClientRoot
		if len(relParts) > 0 {
			clientPath = pathpkg.Join(append([]string{cleanClientRoot}, relParts...)...)
		}
	}
	return PathMapping{HostRoot: root, ClientRoot: cleanClientRoot, HostPath: path, ClientPath: clientPath}, nil
}

func confinedRelativePath(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", ErrHostPathOutsideRoot
	}
	return rel, nil
}

// resolvePlannedHostPath resolves the nearest existing ancestor of a path that
// does not exist yet. This prevents an existing symlink in the planned path
// prefix from silently changing the namespace being mapped.
func resolvePlannedHostPath(root, path string) (string, error) {
	current := path
	missing := make([]string, 0, 4)
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if len(missing) > 0 && (!info.IsDir() || IsLinkLike(info)) {
				return "", fmt.Errorf("planned host path has a non-directory or link-like prefix")
			}
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", fmt.Errorf("resolve planned host path prefix: %w", err)
			}
			if _, err := confinedRelativePath(root, resolved); err != nil {
				return "", err
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			if _, err := confinedRelativePath(root, resolved); err != nil {
				return "", err
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect planned host path prefix: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", ErrHostPathOutsideRoot
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// ValidatePathMappingConfig validates the namespace roots before an expensive
// discovery scan. It performs metadata reads only.
func ValidatePathMappingConfig(hostRoot, clientRoot string, clientWindows bool) error {
	if _, err := validateClientRoot(clientRoot, clientWindows); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidClientRoot, err)
	}
	if hostRoot == "" {
		return fmt.Errorf("host root is empty")
	}
	abs, err := filepath.Abs(hostRoot)
	if err != nil {
		return fmt.Errorf("resolve host root: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return fmt.Errorf("inspect host root: %w", err)
	}
	if !info.IsDir() || IsLinkLike(info) {
		return fmt.Errorf("host root must be a directory and not a symbolic link or reparse point")
	}
	return nil
}

func validateClientRoot(root string, windows bool) (string, error) {
	if root == "" || strings.IndexByte(root, 0) >= 0 {
		return "", fmt.Errorf("client root is empty or contains NUL")
	}
	if !windows {
		if strings.Contains(root, "\\") {
			return "", fmt.Errorf("POSIX client root must not contain backslashes")
		}
		clean := pathpkg.Clean(root)
		if !strings.HasPrefix(clean, "/") || clean != root {
			return "", fmt.Errorf("POSIX client root must be a clean absolute path")
		}
		return clean, nil
	}
	normalized := strings.ReplaceAll(root, "/", "\\")
	isDriveAbsolute := len(normalized) >= 3 && ((normalized[0] >= 'A' && normalized[0] <= 'Z') || (normalized[0] >= 'a' && normalized[0] <= 'z')) && normalized[1] == ':' && normalized[2] == '\\'
	isUNC := strings.HasPrefix(normalized, "\\\\")
	if !isDriveAbsolute && !isUNC {
		return "", fmt.Errorf("Windows client root must be drive-absolute or UNC")
	}
	if isDriveAbsolute && len(normalized) == 3 {
		return normalized, nil
	}
	clean := strings.TrimSuffix(normalized, "\\")
	var parts []string
	if isUNC {
		parts = strings.Split(strings.TrimPrefix(clean, "\\\\"), "\\")
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return "", fmt.Errorf("UNC client root requires non-empty server and share components")
		}
	} else {
		parts = strings.Split(strings.TrimPrefix(clean[3:], "\\"), "\\")
	}
	rawParts := make([][]byte, len(parts))
	for i, part := range parts {
		if part == "" {
			return "", fmt.Errorf("Windows client root contains an empty path component")
		}
		rawParts[i] = []byte(part)
	}
	if len(rawParts) > 0 {
		if err := ValidateComponents(rawParts, PathSemantics{Windows: true, CaseSensitive: false}); err != nil {
			return "", fmt.Errorf("invalid Windows client root: %w", err)
		}
	}
	return clean, nil
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
	if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		suffix := []rune(base[3:])
		if len(suffix) == 1 && ((suffix[0] >= '1' && suffix[0] <= '9') || suffix[0] == '¹' || suffix[0] == '²' || suffix[0] == '³') {
			return true
		}
	}
	return false
}

func windowsJoin(root string, components ...string) string {
	result := strings.ReplaceAll(root, "/", "\\")
	if !(len(result) == 3 && result[1] == ':' && result[2] == '\\') {
		result = strings.TrimRight(result, "\\")
	}
	for _, component := range components {
		if component == "" || component == "." {
			continue
		}
		if !strings.HasSuffix(result, "\\") {
			result += "\\"
		}
		result += strings.ReplaceAll(component, "/", "\\")
	}
	return result
}
