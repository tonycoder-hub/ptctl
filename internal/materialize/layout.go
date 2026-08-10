package materialize

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

type LayoutFile struct {
	ManifestIndex int
	Components    []string
	Length        int64
}

type Layout struct {
	FinalName         string
	FinalRawBase64    string
	MultiFile         bool
	Files             []LayoutFile
	Directories       [][]string
	ContentBytes      int64
	ManifestPathBytes int64
	NamespaceObjects  int
	NamespaceBytes    int64
	Proof             string
}

// BuildLayout converts raw metafile paths into the deliberately conservative
// portable subset supported by materialize v1. Any file attribute, non-UTF-8
// component, separator ambiguity, or target namespace collision is rejected
// before the first filesystem write.
func BuildLayout(meta *metafile.MetaInfo, limits Limits) (Layout, error) {
	if meta == nil {
		return Layout{}, fmt.Errorf("%w: metafile is unavailable", ErrPolicy)
	}
	if err := limits.Validate(); err != nil {
		return Layout{}, err
	}
	if len(meta.Files) == 0 || len(meta.Files) > limits.MaxFiles {
		return Layout{}, fmt.Errorf("%w: manifest file budget is exceeded", ErrPolicy)
	}
	finalName, finalBase64, err := portableRawComponent(meta.NameRaw)
	if err != nil {
		return Layout{}, fmt.Errorf("%w: top-level target name is unsupported", ErrPolicy)
	}
	if hasReservedControlPrefix(finalName) {
		return Layout{}, fmt.Errorf("%w: top-level target name is reserved for materialize control state", ErrPolicy)
	}
	layout := Layout{
		FinalName: finalName, FinalRawBase64: finalBase64, MultiFile: meta.MultiFile,
		Files: []LayoutFile{}, Directories: [][]string{}, Proof: proofForMeta(meta),
	}
	if layout.Proof == "" {
		return Layout{}, fmt.Errorf("%w: metafile proof family is unsupported", ErrPolicy)
	}
	manifestRaw := make([][][]byte, 0, len(meta.Files))
	directorySet := make(map[[sha256.Size]byte][]string)
	if err := reserveNamespaceObject(&layout, []string{stageDirectoryName}, limits); err != nil {
		return Layout{}, err
	}
	for fileIndex, file := range meta.Files {
		if file.Attribute != "" {
			return Layout{}, fmt.Errorf("%w: manifest file attributes are unsupported by materialize v1", ErrPolicy)
		}
		if file.Length > limits.MaxScratchBytes {
			return Layout{}, fmt.Errorf("%w: a manifest file cannot fit in the private scratch budget", ErrPolicy)
		}
		if file.Length < 0 || file.Length > limits.MaxContentBytes-layout.ContentBytes {
			return Layout{}, fmt.Errorf("%w: manifest content budget is exceeded", ErrPolicy)
		}
		components := []string{finalName}
		rawComponents := [][]byte{append([]byte(nil), meta.NameRaw...)}
		if meta.MultiFile {
			if len(file.RawPath) == 0 {
				return Layout{}, fmt.Errorf("%w: multi-file manifest path is empty", ErrPolicy)
			}
			for _, raw := range file.RawPath {
				component, _, componentErr := portableRawComponent(raw)
				if componentErr != nil {
					return Layout{}, fmt.Errorf("%w: manifest file %d has an unsupported path component", ErrPolicy, fileIndex)
				}
				components = append(components, component)
				rawComponents = append(rawComponents, append([]byte(nil), raw...))
			}
		} else if len(meta.Files) != 1 {
			return Layout{}, fmt.Errorf("%w: single-file manifest has multiple files", ErrPolicy)
		}
		if err := fsbind.ValidatePathComponents(components); err != nil {
			return Layout{}, fmt.Errorf("%w: final path is outside the bound filesystem limits", ErrPolicy)
		}
		if err := fsbind.ValidatePathComponents(append([]string{stageDirectoryName}, components...)); err != nil {
			return Layout{}, fmt.Errorf("%w: staging path is outside the bound filesystem limits", ErrPolicy)
		}
		var pathBytes int64
		for _, raw := range rawComponents {
			if int64(len(raw)) > math.MaxInt64-pathBytes {
				return Layout{}, fmt.Errorf("%w: manifest path budget overflowed", ErrPolicy)
			}
			pathBytes += int64(len(raw))
		}
		if pathBytes > limits.MaxPathBytes-layout.ManifestPathBytes {
			return Layout{}, fmt.Errorf("%w: manifest path budget is exceeded", ErrPolicy)
		}
		layout.ManifestPathBytes += pathBytes
		layout.ContentBytes += file.Length
		if err := reserveNamespaceObject(&layout, components, limits); err != nil {
			return Layout{}, err
		}
		layout.Files = append(layout.Files, LayoutFile{ManifestIndex: fileIndex, Components: components, Length: file.Length})
		manifestRaw = append(manifestRaw, rawComponents)
		for depth := 1; depth < len(components); depth++ {
			prefix := components[:depth]
			key := namespacePathID(prefix)
			if _, exists := directorySet[key]; exists {
				continue
			}
			if len(directorySet) >= limits.MaxDirectories {
				return Layout{}, fmt.Errorf("%w: materialized directory budget is exceeded", ErrPolicy)
			}
			if err := reserveNamespaceObject(&layout, prefix, limits); err != nil {
				return Layout{}, err
			}
			directorySet[key] = append([]string(nil), prefix...)
		}
	}
	if err := storage.ValidateManifestPaths(manifestRaw, storage.CurrentSemantics()); err != nil {
		return Layout{}, fmt.Errorf("%w: target namespace is unsafe: %v", ErrPolicy, err)
	}
	for _, directory := range directorySet {
		layout.Directories = append(layout.Directories, directory)
	}
	if err := validateNamespaceDirectoryListBudgets(layout); err != nil {
		return Layout{}, err
	}
	sort.Slice(layout.Directories, func(i, j int) bool {
		left, right := layout.Directories[i], layout.Directories[j]
		if len(left) != len(right) {
			return len(left) < len(right)
		}
		for index := range left {
			if left[index] != right[index] {
				return left[index] < right[index]
			}
		}
		return false
	})
	return layout, nil
}

func validateNamespaceDirectoryListBudgets(layout Layout) error {
	children := make(map[[sha256.Size]byte]int, len(layout.Directories)+1)
	for _, directory := range layout.Directories {
		if len(directory) == 0 {
			return fmt.Errorf("%w: materialized directory path is empty", ErrPolicy)
		}
		children[namespacePathID(directory[:len(directory)-1])]++
	}
	for _, file := range layout.Files {
		if len(file.Components) == 0 {
			return fmt.Errorf("%w: materialized file path is empty", ErrPolicy)
		}
		children[namespacePathID(file.Components[:len(file.Components)-1])]++
	}
	return validateNamespaceChildCounts(children)
}

func validateNamespaceChildCounts(children map[[sha256.Size]byte]int) error {
	for _, count := range children {
		listLimits := fsbind.ListLimits{MaxEntries: count + 1, MaxNameBytes: 1}
		if err := listLimits.Validate(); err != nil {
			return fmt.Errorf("%w: a materialized directory exceeds the exact inventory budget", ErrPolicy)
		}
	}
	return nil
}

func NewIntent(meta *metafile.MetaInfo, layout Layout, planID, targetRootIdentity string, limits Limits) (Intent, error) {
	nonce, err := randomHex(32)
	if err != nil {
		return Intent{}, fmt.Errorf("create materialize operation nonce failed")
	}
	return newIntentWithNonce(meta, layout, planID, targetRootIdentity, nonce, limits)
}

func newIntentWithNonce(meta *metafile.MetaInfo, layout Layout, planID, targetRootIdentity, nonce string, limits Limits) (Intent, error) {
	if meta == nil {
		return Intent{}, fmt.Errorf("%w: metafile is unavailable", ErrInvalidIntent)
	}
	intent := Intent{
		Schema: IntentSchemaV1, MetafileVariantID: meta.MetafileVariantID,
		InfoHashV1: meta.InfoHashV1, InfoHashV2: meta.InfoHashV2, Nonce: nonce,
		PlanID: planID, Strategy: StrategyCopy, TargetRootIdentity: targetRootIdentity,
		FinalRawComponentsBase64: []string{layout.FinalRawBase64},
		MultiFile:                meta.MultiFile, ManifestFiles: len(layout.Files),
		ContentBytes: layout.ContentBytes, ManifestPathBytes: layout.ManifestPathBytes,
		NamespaceObjects: layout.NamespaceObjects, NamespaceBytes: layout.NamespaceBytes,
		Limits: limits,
	}
	if layout.FinalRawBase64 == "" || layout.MultiFile != meta.MultiFile || len(layout.Files) != len(meta.Files) || layout.Proof != proofForMeta(meta) ||
		layout.NamespaceObjects <= 0 || layout.NamespaceBytes <= 0 {
		return Intent{}, fmt.Errorf("%w: layout disagrees with the metafile", ErrInvalidIntent)
	}
	if err := intent.Validate(); err != nil {
		return Intent{}, err
	}
	return intent, nil
}

func reserveNamespaceObject(layout *Layout, components []string, limits Limits) error {
	if layout == nil || len(components) == 0 || layout.NamespaceObjects >= limits.MaxNamespaceObjects {
		return fmt.Errorf("%w: namespace object budget is exceeded", ErrPolicy)
	}
	// The budget deliberately charges both the stored layout path and the
	// staging/published audit views, including conservative component-reference
	// overhead. This prevents deep prefix sets from amplifying one manifest path
	// into unaccounted O(depth^2) memory.
	cost := int64(64 + (len(components)+1)*32)
	for _, component := range components {
		componentCost := int64(len(component)) * 2
		if componentCost > math.MaxInt64-cost {
			return fmt.Errorf("%w: namespace byte budget overflowed", ErrPolicy)
		}
		cost += componentCost
	}
	if cost > limits.MaxNamespaceBytes-layout.NamespaceBytes {
		return fmt.Errorf("%w: namespace byte budget is exceeded", ErrPolicy)
	}
	layout.NamespaceObjects++
	layout.NamespaceBytes += cost
	return nil
}

func namespacePathID(components []string) [sha256.Size]byte {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte("ptctl-materialize-namespace-path-v1\x00"))
	var length [8]byte
	for _, component := range components {
		binary.BigEndian.PutUint64(length[:], uint64(len(component)))
		_, _ = hasher.Write(length[:])
		_, _ = hasher.Write([]byte(component))
	}
	var result [sha256.Size]byte
	copy(result[:], hasher.Sum(nil))
	return result
}

func portableRawComponent(raw []byte) (string, string, error) {
	if !utf8.Valid(raw) {
		return "", "", fmt.Errorf("path component is not UTF-8")
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	if _, err := validateRawComponents([]string{encoded}); err != nil {
		return "", "", err
	}
	return string(raw), encoded, nil
}

func proofForMeta(meta *metafile.MetaInfo) string {
	switch {
	case meta.InfoHashV1 != "" && meta.InfoHashV2 != "":
		return ProofHybrid
	case meta.InfoHashV2 != "":
		return ProofV2
	case meta.InfoHashV1 != "":
		return ProofV1
	default:
		return ""
	}
}
