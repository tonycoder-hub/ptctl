package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"

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
	if value == "" || len(value) > maxClientPathBytes {
		return clientPath{}, fmt.Errorf("client path is empty or exceeds 32 KiB")
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
