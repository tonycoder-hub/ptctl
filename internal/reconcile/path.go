package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const maxClientPathBytes = 32 << 10

type clientPath struct {
	windows bool
	volume  string
	parts   []string
}

// PathMappingOptions is the invocation-scoped host/client namespace mapping
// used to project a process-local verified source. It is configuration, not
// evidence that the downloader can actually access the projected path.
type PathMappingOptions struct {
	HostRoot      string
	ClientRoot    string
	ClientWindows bool
}

func parseClientPath(value string, windows bool) (clientPath, error) {
	if value == "" || len(value) > maxClientPathBytes || !utf8.ValidString(value) || strings.ContainsRune(value, utf8.RuneError) {
		return clientPath{}, fmt.Errorf("client path is invalid or exceeds 32 KiB")
	}
	for _, r := range value {
		if r == 0 || r < 0x20 || r == 0x7f {
			return clientPath{}, fmt.Errorf("client path contains a control character")
		}
	}
	if windows {
		return parseWindowsClientPath(value)
	}
	if !strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return clientPath{}, fmt.Errorf("POSIX client path is not absolute")
	}
	parts, err := strictPathParts(strings.TrimPrefix(value, "/"), "/", false)
	if err != nil {
		return clientPath{}, err
	}
	return clientPath{parts: parts}, nil
}

func parseClientRelativeComponents(components []string, windows bool) ([]string, error) {
	if len(components) == 0 || len(components) > 128 {
		return nil, fmt.Errorf("client relative path has no components or exceeds 128 components")
	}
	total := 0
	raw := make([][]byte, len(components))
	for index, component := range components {
		if component == "" || !utf8.ValidString(component) || strings.ContainsRune(component, utf8.RuneError) || strings.ContainsAny(component, "/\\") {
			return nil, fmt.Errorf("client relative path component %d is invalid", index)
		}
		total += len(component)
		if total > maxClientPathBytes {
			return nil, fmt.Errorf("client relative path exceeds 32 KiB")
		}
		raw[index] = []byte(component)
	}
	if err := storage.ValidateComponents(raw, storage.PathSemantics{Windows: windows, CaseSensitive: true}); err != nil {
		return nil, err
	}
	return append([]string(nil), components...), nil
}

func parseWindowsClientPath(value string) (clientPath, error) {
	value = strings.ReplaceAll(value, "/", "\\")
	if strings.HasPrefix(value, "\\\\") {
		parts, err := strictPathParts(strings.TrimPrefix(value, "\\\\"), "\\", true)
		if err != nil || len(parts) < 2 {
			return clientPath{}, fmt.Errorf("Windows UNC client path is invalid")
		}
		return clientPath{windows: true, volume: "\\\\" + parts[0] + "\\" + parts[1], parts: parts[2:]}, nil
	}
	if len(value) < 3 || !isASCIILetter(value[0]) || value[1] != ':' || value[2] != '\\' {
		return clientPath{}, fmt.Errorf("Windows client path is not drive-absolute")
	}
	parts, err := strictPathParts(value[3:], "\\", true)
	if err != nil {
		return clientPath{}, err
	}
	return clientPath{windows: true, volume: strings.ToUpper(value[:2]), parts: parts}, nil
}

func strictPathParts(value, separator string, windows bool) ([]string, error) {
	if value == "" {
		return []string{}, nil
	}
	if strings.HasSuffix(value, separator) {
		value = strings.TrimSuffix(value, separator)
	}
	if value == "" {
		return []string{}, nil
	}
	parts := strings.Split(value, separator)
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("client path contains an empty or relative component")
		}
		if windows {
			if strings.ContainsAny(part, `<>:"|?*`) || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
				return nil, fmt.Errorf("Windows client path contains an invalid component")
			}
			for _, r := range part {
				if unicode.IsControl(r) {
					return nil, fmt.Errorf("Windows client path contains a control character")
				}
			}
		}
	}
	return parts, nil
}

func (path clientPath) equal(other clientPath) bool {
	if path.windows != other.windows || len(path.parts) != len(other.parts) {
		return false
	}
	if path.windows {
		if path.volume != other.volume {
			return false
		}
		for i := range path.parts {
			if path.parts[i] != other.parts[i] {
				return false
			}
		}
		return true
	}
	if path.volume != other.volume {
		return false
	}
	for i := range path.parts {
		if path.parts[i] != other.parts[i] {
			return false
		}
	}
	return true
}

func (path clientPath) canonical() string {
	if !path.windows {
		return "/" + strings.Join(path.parts, "/")
	}
	volume := path.volume
	parts := append([]string(nil), path.parts...)
	if len(parts) == 0 {
		if strings.HasPrefix(volume, "\\\\") {
			return volume
		}
		return volume + "\\"
	}
	if strings.HasPrefix(volume, "\\\\") {
		return volume + "\\" + strings.Join(parts, "\\")
	}
	return volume + "\\" + strings.Join(parts, "\\")
}

func (path clientPath) public(show bool) string {
	if show {
		return path.canonical()
	}
	digest := sha256.Sum256([]byte("ptctl-client-path-v1\x00" + path.canonical()))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (path clientPath) joinRelative(components []string) clientPath {
	result := path
	result.parts = append(append([]string(nil), path.parts...), components...)
	return result
}

func (path clientPath) within(root clientPath) bool {
	if path.windows != root.windows || path.volume != root.volume || len(path.parts) < len(root.parts) {
		return false
	}
	for index := range root.parts {
		if path.parts[index] != root.parts[index] {
			return false
		}
	}
	return true
}

func validateClientPathSet(paths map[int]clientPath) error {
	type item struct {
		index int
		path  clientPath
		key   string
	}
	items := make([]item, 0, len(paths))
	seen := make(map[string]int, len(paths))
	for index, value := range paths {
		collisionKey := clientNamespaceKey(value)
		if previous, exists := seen[collisionKey]; exists {
			return fmt.Errorf("client file paths %d and %d collide under declared namespace semantics", previous, index)
		}
		seen[collisionKey] = index
		items = append(items, item{index: index, path: value, key: collisionKey})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })
	for index := 1; index < len(items); index++ {
		if clientPathPrefix(items[index-1].path, items[index].path) {
			return fmt.Errorf("client file paths %d and %d have a file/directory prefix collision", items[index-1].index, items[index].index)
		}
	}
	return nil
}

func clientPathPrefix(parent, child clientPath) bool {
	if parent.windows != child.windows || len(parent.parts) >= len(child.parts) {
		return false
	}
	leftVolume, rightVolume := parent.volume, child.volume
	if parent.windows {
		leftVolume, rightVolume = windowsSimpleFoldKey(leftVolume), windowsSimpleFoldKey(rightVolume)
	}
	if leftVolume != rightVolume {
		return false
	}
	for index := range parent.parts {
		left, right := parent.parts[index], child.parts[index]
		if parent.windows {
			left, right = windowsSimpleFoldKey(left), windowsSimpleFoldKey(right)
		}
		if left != right {
			return false
		}
	}
	return true
}

func clientNamespaceKey(path clientPath) string {
	volume := path.volume
	parts := append([]string(nil), path.parts...)
	if path.windows {
		volume = windowsSimpleFoldKey(volume)
		for index := range parts {
			parts[index] = windowsSimpleFoldKey(parts[index])
		}
	}
	return volume + "\x00" + strings.Join(parts, "\x00")
}

func windowsSimpleFoldKey(value string) string {
	var result strings.Builder
	result.Grow(len(value))
	for _, character := range value {
		minimum := character
		for folded := unicode.SimpleFold(character); folded != character; folded = unicode.SimpleFold(folded) {
			if folded < minimum {
				minimum = folded
			}
		}
		result.WriteRune(minimum)
	}
	return result.String()
}

func expectedClientContentPath(meta *metafile.MetaInfo, source *metafile.VerifiedSource, mapping PathMappingOptions) (clientPath, string, error) {
	if meta == nil || source == nil || !source.Matches(meta) {
		return clientPath{}, "unavailable", fmt.Errorf("there is no matching process-local verified source")
	}
	if meta.MultiFile {
		return clientPath{}, "client_file_layout_unobservable", fmt.Errorf("multi-file client layout is not observable")
	}
	hostPath, ok := source.Path(0)
	if !ok {
		return clientPath{}, "unavailable", fmt.Errorf("single-file source has no physical binding")
	}
	projected, err := storage.MapHostToClient(mapping.HostRoot, hostPath, mapping.ClientRoot, mapping.ClientWindows)
	if err != nil {
		return clientPath{}, "mapping_incomplete", err
	}
	parsed, err := parseClientPath(projected.ClientPath, mapping.ClientWindows)
	if err != nil {
		return clientPath{}, "mapping_incomplete", err
	}
	return parsed, "single_file", nil
}

func isASCIILetter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}
