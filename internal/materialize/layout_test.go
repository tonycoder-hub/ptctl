package materialize

import (
	"crypto/sha256"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
)

func TestBuildLayoutMultiFileDeterministic(t *testing.T) {
	meta := &metafile.MetaInfo{
		NameRaw: []byte("top"), MultiFile: true, InfoHashV1: strings.Repeat("1", 40),
		Files: []metafile.File{
			{RawPath: [][]byte{[]byte("b"), []byte("two.bin")}, Length: 2},
			{RawPath: [][]byte{[]byte("a"), []byte("one.bin")}, Length: 1},
			{RawPath: [][]byte{[]byte("empty")}, Length: 0},
		},
	}
	layout, err := BuildLayout(meta, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if layout.FinalName != "top" || layout.ContentBytes != 3 || len(layout.Files) != 3 || len(layout.Directories) != 3 {
		t.Fatalf("unexpected layout: %#v", layout)
	}
	wantDirectories := [][]string{{"top"}, {"top", "a"}, {"top", "b"}}
	for index := range wantDirectories {
		if strings.Join(layout.Directories[index], "/") != strings.Join(wantDirectories[index], "/") {
			t.Fatalf("directory order differs: %#v", layout.Directories)
		}
	}
}

func TestNamespaceDirectoryInventoryBudgetMatchesFSBindBoundary(t *testing.T) {
	key := sha256.Sum256([]byte("directory"))
	maximum := fsbind.DefaultListLimits()
	for maximum.MaxEntries < 1_000_000 && (fsbind.ListLimits{MaxEntries: maximum.MaxEntries + 1, MaxNameBytes: 1}).Validate() == nil {
		maximum.MaxEntries++
	}
	if err := validateNamespaceChildCounts(map[[sha256.Size]byte]int{key: maximum.MaxEntries - 1}); err != nil {
		t.Fatalf("largest auditable directory was rejected: %v", err)
	}
	if err := validateNamespaceChildCounts(map[[sha256.Size]byte]int{key: maximum.MaxEntries}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("directory requiring an over-limit N+1 audit was accepted: %v", err)
	}
}

func TestNewIntentBindsLayoutPlanRootAndRandomNonce(t *testing.T) {
	meta := &metafile.MetaInfo{
		NameRaw: []byte("file"), MetafileVariantID: "sha256:" + strings.Repeat("a", 64),
		InfoHashV1: strings.Repeat("1", 40),
		Files:      []metafile.File{{RawPath: [][]byte{[]byte("file")}, Length: 1}},
	}
	layout, err := BuildLayout(meta, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	intent, err := newIntentWithNonce(meta, layout, strings.Repeat("2", 24), "fsbind-v1:"+strings.Repeat("3", 64), strings.Repeat("4", 64), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if intent.ContentBytes != 1 || intent.ManifestFiles != 1 || intent.ManifestPathBytes == 0 || intent.Nonce != strings.Repeat("4", 64) {
		t.Fatalf("unexpected intent: %#v", intent)
	}
	if _, err := OperationIDFor(intent); err != nil {
		t.Fatal(err)
	}
}

func TestBuildLayoutRejectsAttributesUnsafeBytesAndBudgets(t *testing.T) {
	base := func() *metafile.MetaInfo {
		return &metafile.MetaInfo{
			NameRaw: []byte("top"), MultiFile: true, InfoHashV2: strings.Repeat("2", 64),
			Files: []metafile.File{{RawPath: [][]byte{[]byte("file")}, Length: 1}},
		}
	}
	tests := []func(*metafile.MetaInfo, *Limits){
		func(meta *metafile.MetaInfo, _ *Limits) { meta.Files[0].Attribute = "p" },
		func(meta *metafile.MetaInfo, _ *Limits) { meta.Files[0].Attribute = "x" },
		func(meta *metafile.MetaInfo, _ *Limits) { meta.Files[0].RawPath[0] = []byte{0xff} },
		func(meta *metafile.MetaInfo, _ *Limits) { meta.Files[0].RawPath[0] = []byte("..") },
		func(meta *metafile.MetaInfo, _ *Limits) { meta.NameRaw = []byte("a\\b") },
		func(meta *metafile.MetaInfo, limits *Limits) { limits.MaxContentBytes = 1; meta.Files[0].Length = 2 },
		func(meta *metafile.MetaInfo, limits *Limits) { limits.MaxPathBytes = 1 },
		func(meta *metafile.MetaInfo, limits *Limits) {
			limits.MaxDirectories = 1
			meta.Files[0].RawPath = [][]byte{[]byte("nested"), []byte("file")}
		},
	}
	for index, mutate := range tests {
		meta, limits := base(), DefaultLimits()
		mutate(meta, &limits)
		if _, err := BuildLayout(meta, limits); !errors.Is(err, ErrPolicy) {
			t.Fatalf("case %d: expected policy error, got %v", index, err)
		}
	}
}

func TestBuildLayoutRejectsCaseCollisionOnWindowsSemantics(t *testing.T) {
	// This assertion is meaningful on the Windows CI leg; case-sensitive hosts
	// still exercise the same pair as a valid exact namespace.
	meta := &metafile.MetaInfo{
		NameRaw: []byte("top"), MultiFile: true, InfoHashV1: strings.Repeat("1", 40),
		Files: []metafile.File{
			{RawPath: [][]byte{[]byte("A")}, Length: 1},
			{RawPath: [][]byte{[]byte("a")}, Length: 1},
		},
	}
	_, err := BuildLayout(meta, DefaultLimits())
	if runtime.GOOS == "windows" && !errors.Is(err, ErrPolicy) {
		t.Fatalf("Windows case collision was accepted: %v", err)
	}
}

func TestBuildLayoutRejectsUnicodeSimpleFoldCollisionOnWindows(t *testing.T) {
	meta := &metafile.MetaInfo{
		NameRaw: []byte("top"), MultiFile: true, InfoHashV1: strings.Repeat("1", 40),
		Files: []metafile.File{
			{RawPath: [][]byte{[]byte("\u03c3")}, Length: 1},
			{RawPath: [][]byte{[]byte("\u03c2")}, Length: 1},
		},
	}
	_, err := BuildLayout(meta, DefaultLimits())
	if runtime.GOOS == "windows" && !errors.Is(err, ErrPolicy) {
		t.Fatalf("Windows Unicode simple-fold collision was accepted: %v", err)
	}
}

func TestBuildLayoutReservesEveryControlPrefix(t *testing.T) {
	for _, prefix := range []string{operationDirectoryPrefix, clientAdoptDirectoryPrefix, clientAdoptForgetMarkerPrefix, clientActivateDirectoryPrefix, clientActivateForgetMarkerPrefix, clientRemoveDirectoryPrefix, clientRemoveForgetMarkerPrefix, sourceRetireDirectoryPrefix, sourceRetireForgetMarkerPrefix} {
		name := prefix + "content"
		if runtime.GOOS == "windows" {
			name = strings.ToUpper(name)
		}
		meta := &metafile.MetaInfo{
			NameRaw:    []byte(name),
			InfoHashV1: strings.Repeat("1", 40),
			Files:      []metafile.File{{Length: 1}},
		}
		if _, err := BuildLayout(meta, DefaultLimits()); !errors.Is(err, ErrPolicy) {
			t.Fatalf("reserved control prefix %q was accepted: %v", prefix, err)
		}
	}
}

func TestSourceRetireReservedFamiliesDistinguishForgetMarkers(t *testing.T) {
	operation := SourceRetireOperationDirectoryPrefix + strings.Repeat("a", 64)
	forget := SourceRetireForgetMarkerPrefix + strings.Repeat("b", 64) + ".json"
	if runtime.GOOS == "windows" {
		operation, forget = strings.ToUpper(operation), strings.ToUpper(forget)
	}
	if !HasSourceRetireOperationPrefix(operation) || HasSourceRetireForgetMarkerPrefix(operation) ||
		HasSourceRetireOperationPrefix(forget) || !HasSourceRetireForgetMarkerPrefix(forget) {
		t.Fatalf("reserved source-retire families overlap: operation=%q forget=%q", operation, forget)
	}
}
