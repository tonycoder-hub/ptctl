package materialize

import (
	"context"
	"errors"
	"fmt"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

type namespaceView interface {
	Inspect(context.Context, fsbind.Path) (fsbind.ObjectInfo, error)
	List(context.Context, fsbind.Path, fsbind.ListLimits) (fsbind.ListResult, error)
	Check() error
}

type expectedNamespaceObject struct {
	components []string
	kind       fsbind.ObjectKind
	size       int64
	identity   fsbind.Identity
}

type namespaceObservation struct {
	kind     fsbind.ObjectKind
	size     int64
	identity fsbind.Identity
}

// namespaceSnapshot deliberately retains only a fixed-size observation vector.
// Paths and directory-prefix strings stay in the bounded Layout and are never
// duplicated into both sides of the verification bracket.
type namespaceSnapshot struct {
	observations []namespaceObservation
	topIdentity  fsbind.Identity
}

func auditStageNamespace(ctx context.Context, layout Layout, journal *journal) (namespaceSnapshot, error) {
	if journal == nil || len(journal.state.StagedFiles) != len(layout.Files) {
		return namespaceSnapshot{}, fmt.Errorf("%w: staged file journal is incomplete", ErrCorruptJournal)
	}
	stageIdentity, err := fsbind.ParseIdentity(journal.state.StageContainerIdentity)
	if err != nil {
		return namespaceSnapshot{}, fmt.Errorf("%w: staging container identity is invalid", ErrCorruptJournal)
	}
	expected := make([]expectedNamespaceObject, 0, layout.NamespaceObjects)
	expected = append(expected, expectedNamespaceObject{
		components: []string{stageDirectoryName}, kind: fsbind.ObjectKindDirectory, identity: stageIdentity,
	})
	verifiedStage := fsbind.Identity{}
	topIndex := -1
	if journal.state.Phase == PhaseStageVerified || journal.state.Phase == PhasePublishIntent {
		verifiedStage, err = fsbind.ParseIdentity(journal.state.StageIdentity)
		if err != nil {
			return namespaceSnapshot{}, fmt.Errorf("%w: verified stage identity is invalid", ErrCorruptJournal)
		}
	}
	for _, directory := range layout.Directories {
		components := append([]string{stageDirectoryName}, directory...)
		object := expectedNamespaceObject{components: components, kind: fsbind.ObjectKindDirectory}
		if len(directory) == 1 && directory[0] == layout.FinalName && !verifiedStage.IsZero() {
			object.identity = verifiedStage
		}
		expected = append(expected, object)
		if len(directory) == 1 && directory[0] == layout.FinalName {
			topIndex = len(expected) - 1
		}
	}
	for _, file := range layout.Files {
		recorded := journal.state.StagedFiles[file.ManifestIndex]
		identity, parseErr := fsbind.ParseIdentity(recorded.ObjectIdentity)
		if parseErr != nil {
			return namespaceSnapshot{}, fmt.Errorf("%w: staged file identity is invalid", ErrCorruptJournal)
		}
		if !layout.MultiFile && !verifiedStage.IsZero() && !identity.Equal(verifiedStage) {
			return namespaceSnapshot{}, fmt.Errorf("%w: verified single-file stage identity disagrees", ErrIntegrity)
		}
		expected = append(expected, expectedNamespaceObject{
			components: append([]string{stageDirectoryName}, file.Components...),
			kind:       fsbind.ObjectKindRegular, size: file.Length, identity: identity,
		})
		if !layout.MultiFile {
			topIndex = len(expected) - 1
		}
	}
	if topIndex < 0 {
		return namespaceSnapshot{}, fmt.Errorf("%w: staging top-level object is missing", ErrCorruptJournal)
	}
	return auditExactNamespace(ctx, journal.subtree, expected, topIndex, journal.intent.Limits)
}

func auditPublishedNamespace(ctx context.Context, layout Layout, journal *journal, published *fsbind.Published) (namespaceSnapshot, error) {
	if journal == nil || published == nil || len(journal.state.StagedFiles) != len(layout.Files) {
		return namespaceSnapshot{}, fmt.Errorf("%w: published namespace authority is incomplete", ErrCorruptJournal)
	}
	finalIdentity, err := fsbind.ParseIdentity(journal.state.FinalIdentity)
	if err != nil {
		return namespaceSnapshot{}, fmt.Errorf("%w: final identity is invalid", ErrCorruptJournal)
	}
	rootKind := fsbind.ObjectKindRegular
	if layout.MultiFile {
		rootKind = fsbind.ObjectKindDirectory
	}
	expected := make([]expectedNamespaceObject, 0, layout.NamespaceObjects)
	expected = append(expected, expectedNamespaceObject{kind: rootKind, identity: finalIdentity})
	if !layout.MultiFile {
		if len(layout.Files) != 1 || journal.state.StagedFiles[0].ObjectIdentity != finalIdentity.String() {
			return namespaceSnapshot{}, fmt.Errorf("%w: single-file publication identity disagrees", ErrIntegrity)
		}
		expected[0].size = layout.Files[0].Length
	}
	if layout.MultiFile {
		for _, directory := range layout.Directories {
			if len(directory) <= 1 {
				continue
			}
			expected = append(expected, expectedNamespaceObject{
				components: directory[1:], kind: fsbind.ObjectKindDirectory,
			})
		}
	}
	for _, file := range layout.Files {
		if !layout.MultiFile {
			continue
		}
		recorded := journal.state.StagedFiles[file.ManifestIndex]
		identity, parseErr := fsbind.ParseIdentity(recorded.ObjectIdentity)
		if parseErr != nil {
			return namespaceSnapshot{}, fmt.Errorf("%w: staged file identity is invalid", ErrCorruptJournal)
		}
		expected = append(expected, expectedNamespaceObject{
			components: file.Components[1:], kind: fsbind.ObjectKindRegular,
			size: file.Length, identity: identity,
		})
	}
	return auditExactNamespace(ctx, published, expected, 0, journal.intent.Limits)
}

// auditCurrentPublishedNamespace proves the exact current namespace without
// relying on retained staging-file identities. It is used after a materialize
// journal has been pruned: expected names, kinds, and sizes come from the exact
// metafile layout, while the two-sided snapshot comparison below still binds
// every observed object identity for this invocation.
func auditCurrentPublishedNamespace(ctx context.Context, layout Layout, published *fsbind.Published, expectedFinal fsbind.Identity, limits Limits) (namespaceSnapshot, error) {
	if published == nil || expectedFinal.IsZero() {
		return namespaceSnapshot{}, fmt.Errorf("%w: current publication authority is incomplete", ErrIntegrity)
	}
	rootKind := fsbind.ObjectKindRegular
	if layout.MultiFile {
		rootKind = fsbind.ObjectKindDirectory
	}
	expected := make([]expectedNamespaceObject, 0, layout.NamespaceObjects)
	expected = append(expected, expectedNamespaceObject{kind: rootKind, identity: expectedFinal})
	if !layout.MultiFile {
		if len(layout.Files) != 1 {
			return namespaceSnapshot{}, fmt.Errorf("%w: single-file layout is invalid", ErrIntegrity)
		}
		expected[0].size = layout.Files[0].Length
	} else {
		for _, directory := range layout.Directories {
			if len(directory) <= 1 {
				continue
			}
			expected = append(expected, expectedNamespaceObject{components: directory[1:], kind: fsbind.ObjectKindDirectory})
		}
		for _, file := range layout.Files {
			expected = append(expected, expectedNamespaceObject{
				components: file.Components[1:], kind: fsbind.ObjectKindRegular, size: file.Length,
			})
		}
	}
	return auditExactNamespace(ctx, published, expected, 0, limits)
}

func auditExactNamespace(ctx context.Context, view namespaceView, expected []expectedNamespaceObject, topIndex int, limits Limits) (namespaceSnapshot, error) {
	if view == nil || len(expected) == 0 || len(expected) > limits.MaxNamespaceObjects || topIndex < 0 || topIndex >= len(expected) {
		return namespaceSnapshot{}, fmt.Errorf("%w: namespace expectation is unavailable or outside its budget", ErrCorruptJournal)
	}
	if err := checkNamespaceView(view); err != nil {
		return namespaceSnapshot{}, err
	}
	objects := make(map[[32]byte]int, len(expected))
	children := make(map[[32]byte]map[string]fsbind.ObjectKind)
	for index, object := range expected {
		key := namespacePathID(object.components)
		if _, duplicate := objects[key]; duplicate {
			return namespaceSnapshot{}, fmt.Errorf("%w: namespace expectation is duplicated", ErrCorruptJournal)
		}
		objects[key] = index
		if len(object.components) == 0 {
			continue
		}
		parent := namespacePathID(object.components[:len(object.components)-1])
		if children[parent] == nil {
			children[parent] = make(map[string]fsbind.ObjectKind)
		}
		name := object.components[len(object.components)-1]
		if _, duplicate := children[parent][name]; duplicate {
			return namespaceSnapshot{}, fmt.Errorf("%w: namespace child expectation is duplicated", ErrCorruptJournal)
		}
		children[parent][name] = object.kind
	}
	snapshot := namespaceSnapshot{observations: make([]namespaceObservation, 0, len(expected))}
	for index, want := range expected {
		if err := ctx.Err(); err != nil {
			return namespaceSnapshot{}, err
		}
		path, err := fsbind.PathFromComponents(want.components)
		if err != nil {
			return namespaceSnapshot{}, fmt.Errorf("%w: expected namespace path is outside fsbind limits", ErrPolicy)
		}
		observed, err := view.Inspect(ctx, path)
		if err != nil {
			return namespaceSnapshot{}, classifyNamespaceObservationError(err)
		}
		if observed.Kind != want.kind || (want.kind == fsbind.ObjectKindRegular && observed.SizeBytes != want.size) ||
			(!want.identity.IsZero() && !observed.Identity.Equal(want.identity)) {
			return namespaceSnapshot{}, fmt.Errorf("%w: exact namespace object differs", ErrIntegrity)
		}
		snapshot.observations = append(snapshot.observations, namespaceObservation{
			kind: observed.Kind, size: observed.SizeBytes, identity: observed.Identity,
		})
		if index == topIndex {
			snapshot.topIdentity = observed.Identity
		}
		if want.kind != fsbind.ObjectKindDirectory {
			continue
		}
		wantChildren := children[namespacePathID(want.components)]
		maximum := len(wantChildren) + 1
		if maximum < 1 {
			maximum = 1
		}
		listing, listErr := view.List(ctx, path, fsbind.ListLimits{
			MaxEntries: maximum, MaxNameBytes: limits.MaxNamespaceBytes,
		})
		if listErr != nil {
			return namespaceSnapshot{}, classifyNamespaceObservationError(listErr)
		}
		if !listing.Complete || len(listing.Entries) != len(wantChildren) {
			return namespaceSnapshot{}, fmt.Errorf("%w: exact namespace directory inventory differs", ErrIntegrity)
		}
		for _, entry := range listing.Entries {
			kind, ok := wantChildren[entry.Name]
			if !ok || entry.Kind != string(kind) {
				return namespaceSnapshot{}, fmt.Errorf("%w: exact namespace contains an unexpected object", ErrIntegrity)
			}
		}
	}
	if err := checkNamespaceView(view); err != nil {
		return namespaceSnapshot{}, err
	}
	return snapshot, nil
}

func checkNamespaceView(view namespaceView) error {
	if err := view.Check(); err != nil {
		return classifyNamespaceObservationError(err)
	}
	return nil
}

func classifyNamespaceObservationError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, fsbind.ErrNotFound) || errors.Is(err, fsbind.ErrUnsafeObject) ||
		errors.Is(err, fsbind.ErrBindingChanged) || errors.Is(err, fsbind.ErrCrossFilesystem) {
		return fmt.Errorf("%w: exact namespace changed", ErrIntegrity)
	}
	return err
}

func sameNamespaceSnapshot(left, right namespaceSnapshot) bool {
	if len(left.observations) != len(right.observations) || !left.topIdentity.Equal(right.topIdentity) {
		return false
	}
	for index, first := range left.observations {
		second := right.observations[index]
		if first.kind != second.kind || first.size != second.size || !first.identity.Equal(second.identity) {
			return false
		}
	}
	return true
}
