package sourceretire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

var parentCleanupRetentionTransitionHook func(string) error

type parentCleanupRetentionState struct {
	DirectoryPresent bool
	IntentPresent    bool
	Intent           ParentCleanupRetentionIntent
	IntentID         ParentCleanupRetentionMarkerID
	CompletePresent  bool
	Complete         ParentCleanupRetentionComplete
	CompleteID       ParentCleanupRetentionMarkerID
	Entries          []fsbind.Entry
}

func loadParentCleanupRetentionState(ctx context.Context, subtree *fsbind.Subtree, operation ParentCleanupOperationID, targetIdentity fsbind.Identity) (parentCleanupRetentionState, error) {
	state := parentCleanupRetentionState{Entries: []fsbind.Entry{}}
	if subtree == nil || targetIdentity.IsZero() {
		return state, fmt.Errorf("%w: parent-cleanup retention authority is unavailable", ErrExecutionIntegrity)
	}
	directoryPath, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory})
	directory, err := subtree.Inspect(ctx, directoryPath)
	if errors.Is(err, fsbind.ErrNotFound) {
		return state, nil
	}
	if err != nil {
		return state, classifyExecutionBindingError(err)
	}
	state.DirectoryPresent = true
	if directory.Kind != fsbind.ObjectKindDirectory || directory.Identity.IsZero() {
		return state, fmt.Errorf("%w: parent-cleanup retention directory is unsafe", ErrExecutionIntegrity)
	}
	listing, err := subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 5, MaxNameBytes: 1 << 20})
	if err != nil {
		return state, classifyExecutionBindingError(err)
	}
	if !listing.Complete {
		return state, fmt.Errorf("%w: parent-cleanup retention inventory is incomplete", ErrExecutionIntegrity)
	}
	state.Entries = append(state.Entries, listing.Entries...)
	for _, entry := range listing.Entries {
		if entry.Kind != string(fsbind.ObjectKindRegular) {
			return state, fmt.Errorf("%w: parent-cleanup retention object is unsafe", ErrExecutionIntegrity)
		}
	}
	intentPath, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory, parentCleanupRetentionIntentFile})
	if parentCleanupRetentionEntryPresent(listing.Entries, parentCleanupRetentionIntentFile) {
		raw, _, readErr := readParentCleanupRetentionNamedBytes(ctx, subtree, intentPath, maximumParentCleanupRetentionMarker)
		if readErr != nil {
			return state, readErr
		}
		intent, id, decodeErr := DecodeParentCleanupRetentionIntent(bytes.NewReader(raw))
		if decodeErr != nil || intent.OperationID != operation || intent.OperationRootIdentity != subtree.Identity().String() ||
			intent.TargetRootIdentity != targetIdentity.String() {
			return state, fmt.Errorf("%w: parent-cleanup retention intent is bound to another operation", ErrExecutionIntegrity)
		}
		state.IntentPresent, state.Intent, state.IntentID = true, intent, id
	}
	completePath, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory, parentCleanupRetentionCompleteFile})
	if parentCleanupRetentionEntryPresent(listing.Entries, parentCleanupRetentionCompleteFile) {
		if !state.IntentPresent {
			return state, fmt.Errorf("%w: parent-cleanup retention completion exists without an intent", ErrExecutionIntegrity)
		}
		raw, _, readErr := readParentCleanupRetentionNamedBytes(ctx, subtree, completePath, maximumParentCleanupRetentionMarker)
		if readErr != nil {
			return state, readErr
		}
		complete, id, decodeErr := DecodeParentCleanupRetentionComplete(bytes.NewReader(raw))
		if decodeErr != nil || complete.OperationID != operation || complete.OperationRootIdentity != subtree.Identity().String() ||
			complete.TargetRootIdentity != targetIdentity.String() || complete.IntentMarkerID != state.IntentID {
			return state, fmt.Errorf("%w: parent-cleanup retention completion disagrees with its intent", ErrExecutionIntegrity)
		}
		state.CompletePresent, state.Complete, state.CompleteID = true, complete, id
	}
	if err := subtree.CheckPaths(directoryPath); err != nil {
		return state, classifyExecutionBindingError(err)
	}
	return state, nil
}

func parentCleanupRetentionEntryPresent(entries []fsbind.Entry, name string) bool {
	for _, entry := range entries {
		if entry.Name == name {
			return true
		}
	}
	return false
}

func ensureParentCleanupRetentionIntent(ctx context.Context, subtree *fsbind.Subtree, marker ParentCleanupRetentionIntent) (ParentCleanupRetentionMarkerID, executionRetentionMarkerReceipt, error) {
	raw, id, err := EncodeParentCleanupRetentionIntent(marker)
	if err != nil {
		return "", executionRetentionMarkerReceipt{}, err
	}
	return ensureParentCleanupRetentionMarker(ctx, subtree, parentCleanupRetentionIntentFile, "intent", raw, id)
}

func ensureParentCleanupRetentionComplete(ctx context.Context, subtree *fsbind.Subtree, marker ParentCleanupRetentionComplete) (ParentCleanupRetentionMarkerID, executionRetentionMarkerReceipt, error) {
	raw, id, err := EncodeParentCleanupRetentionComplete(marker)
	if err != nil {
		return "", executionRetentionMarkerReceipt{}, err
	}
	return ensureParentCleanupRetentionMarker(ctx, subtree, parentCleanupRetentionCompleteFile, "complete", raw, id)
}

func ensureParentCleanupRetentionMarker(ctx context.Context, subtree *fsbind.Subtree, destinationName, purpose string, raw []byte, id ParentCleanupRetentionMarkerID) (ParentCleanupRetentionMarkerID, executionRetentionMarkerReceipt, error) {
	receipt := executionRetentionMarkerReceipt{}
	if subtree == nil || len(raw) == 0 || int64(len(raw)) > maximumParentCleanupRetentionMarker {
		return "", receipt, fmt.Errorf("%w: parent-cleanup retention marker input is unavailable", ErrExecutionPolicy)
	}
	if _, err := ParseParentCleanupRetentionMarkerID(id.String()); err != nil {
		return "", receipt, err
	}
	directoryPath, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory})
	directory, inspectErr := subtree.Inspect(ctx, directoryPath)
	switch {
	case errors.Is(inspectErr, fsbind.ErrNotFound):
		mkdir, err := subtree.MkdirAll(ctx, directoryPath)
		receipt.DirectoryCreated, receipt.DirectoryDurability = mkdir.DirectoriesCreated != 0, mkdir.Durability
		if err != nil {
			return "", receipt, err
		}
	case inspectErr != nil:
		return "", receipt, inspectErr
	case directory.Kind != fsbind.ObjectKindDirectory:
		return "", receipt, fmt.Errorf("%w: parent-cleanup retention directory is unsafe", ErrExecutionIntegrity)
	}
	destination, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory, destinationName})
	temporaryName := parentCleanupRetentionTemporaryName(purpose, id)
	temporary, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory, temporaryName})
	if identity, err := verifyParentCleanupRetentionNamedBytes(ctx, subtree, destination, raw); err == nil {
		receipt.AlreadyPresent = true
		if tempInfo, tempErr := subtree.Inspect(ctx, temporary); tempErr == nil {
			if tempInfo.Kind != fsbind.ObjectKindRegular {
				return id, receipt, fmt.Errorf("%w: parent-cleanup retention temporary is unsafe", ErrExecutionIntegrity)
			}
			if _, verifyErr := verifyParentCleanupRetentionNamedBytes(ctx, subtree, temporary, raw); verifyErr != nil {
				return id, receipt, verifyErr
			}
			removal, removeErr := subtree.RemoveRegularExact(ctx, temporary, tempInfo.Identity, int64(len(raw)))
			receipt.TemporaryRemoval = removal
			if removeErr != nil {
				return id, receipt, removeErr
			}
		} else if !errors.Is(tempErr, fsbind.ErrNotFound) {
			return id, receipt, tempErr
		}
		if identity.IsZero() {
			return id, receipt, fmt.Errorf("%w: parent-cleanup retention marker identity is unavailable", ErrExecutionIntegrity)
		}
		if err := confirmParentCleanupRetentionDurability(ctx, subtree); err != nil {
			return id, receipt, err
		}
		return id, receipt, nil
	} else if !errors.Is(err, fsbind.ErrNotFound) {
		return "", receipt, err
	}

	tempInfo, tempErr := subtree.Inspect(ctx, temporary)
	if errors.Is(tempErr, fsbind.ErrNotFound) {
		file, err := subtree.CreateRegular(ctx, temporary)
		if err != nil {
			return "", receipt, err
		}
		receipt.TemporaryCreated = true
		written, writeErr := writeExecutionBytes(ctx, file, raw)
		receipt.TemporaryBytesWritten = written
		if writeErr == nil {
			writeErr = file.Sync()
		}
		info, infoErr := file.Info()
		closeErr := file.Close()
		if writeErr != nil {
			return "", receipt, writeErr
		}
		if infoErr != nil {
			return "", receipt, infoErr
		}
		if closeErr != nil {
			return "", receipt, closeErr
		}
		if info.SizeBytes != int64(len(raw)) || info.Identity.IsZero() {
			return "", receipt, fmt.Errorf("%w: parent-cleanup retention temporary changed", ErrExecutionIntegrity)
		}
		tempInfo = fsbind.ObjectInfo{Identity: info.Identity, Kind: fsbind.ObjectKindRegular, SizeBytes: info.SizeBytes}
	} else if tempErr != nil {
		return "", receipt, tempErr
	} else if tempInfo.Kind != fsbind.ObjectKindRegular {
		return "", receipt, fmt.Errorf("%w: parent-cleanup retention temporary is unsafe", ErrExecutionIntegrity)
	}
	if _, err := verifyParentCleanupRetentionNamedBytes(ctx, subtree, temporary, raw); err != nil {
		return "", receipt, err
	}
	publication, publishErr := subtree.CommitRegularNoReplace(ctx, temporary, destination)
	receipt.Publication = publication
	if publishErr != nil {
		return id, receipt, publishErr
	}
	if !publication.Published || publication.Durability != fsbind.DurabilityConfirmed ||
		!publication.SourceIdentity.Equal(tempInfo.Identity) || !publication.FinalIdentity.Equal(tempInfo.Identity) {
		return id, receipt, fsbind.ErrPublicationAmbiguous
	}
	if _, err := verifyParentCleanupRetentionNamedBytes(ctx, subtree, destination, raw); err != nil {
		return id, receipt, err
	}
	if err := confirmParentCleanupRetentionDurability(ctx, subtree); err != nil {
		return id, receipt, err
	}
	return id, receipt, nil
}

func parentCleanupRetentionTemporaryName(purpose string, id ParentCleanupRetentionMarkerID) string {
	return purpose + "-" + strings.TrimPrefix(id.String(), markerIDPrefix) + ".pending"
}

func readParentCleanupRetentionNamedBytes(ctx context.Context, subtree *fsbind.Subtree, path fsbind.Path, limit int64) ([]byte, fsbind.ObjectInfo, error) {
	journal := &parentCleanupJournal{subtree: subtree}
	return journal.readNamedBytes(ctx, path, limit)
}

func verifyParentCleanupRetentionNamedBytes(ctx context.Context, subtree *fsbind.Subtree, path fsbind.Path, expected []byte) (fsbind.Identity, error) {
	raw, object, err := readParentCleanupRetentionNamedBytes(ctx, subtree, path, int64(len(expected)))
	if err != nil {
		return fsbind.Identity{}, err
	}
	if !bytes.Equal(raw, expected) || object.Identity.IsZero() {
		return fsbind.Identity{}, fmt.Errorf("%w: parent-cleanup retention marker bytes disagree", ErrExecutionIntegrity)
	}
	return object.Identity, nil
}

func confirmParentCleanupRetentionDurability(ctx context.Context, subtree *fsbind.Subtree) error {
	directoryPath, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory})
	if err := subtree.SyncDirectory(ctx, directoryPath); err != nil {
		return classifyExecutionBindingError(err)
	}
	if err := subtree.SyncDirectory(ctx, fsbind.Path{}); err != nil {
		return classifyExecutionBindingError(err)
	}
	return classifyExecutionBindingError(subtree.CheckPaths(fsbind.Path{}, directoryPath))
}

func auditParentCleanupRetentionControl(ctx context.Context, subtree *fsbind.Subtree, state parentCleanupRetentionState, allowPending bool) error {
	if !state.DirectoryPresent {
		return nil
	}
	directoryPath, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory})
	listing, err := subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 5, MaxNameBytes: 1 << 20})
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	if !listing.Complete {
		return fmt.Errorf("%w: parent-cleanup retention inventory is incomplete", ErrExecutionIntegrity)
	}
	allowed := make(map[string]bool, 4)
	if state.IntentPresent {
		allowed[parentCleanupRetentionIntentFile] = true
		allowed[parentCleanupRetentionTemporaryName("intent", state.IntentID)] = allowPending
	}
	if state.CompletePresent {
		allowed[parentCleanupRetentionCompleteFile] = true
		allowed[parentCleanupRetentionTemporaryName("complete", state.CompleteID)] = allowPending
	}
	for _, entry := range listing.Entries {
		if !allowed[entry.Name] {
			if !state.IntentPresent && allowPending && strings.HasPrefix(entry.Name, "intent-") && strings.HasSuffix(entry.Name, ".pending") && len(entry.Name) == len("intent-")+64+len(".pending") {
				continue
			}
			return fmt.Errorf("%w: parent-cleanup retention namespace contains an unexpected object", ErrExecutionIntegrity)
		}
	}
	if state.IntentPresent && !parentCleanupRetentionEntryPresent(listing.Entries, parentCleanupRetentionIntentFile) ||
		state.CompletePresent && !parentCleanupRetentionEntryPresent(listing.Entries, parentCleanupRetentionCompleteFile) {
		return fmt.Errorf("%w: parent-cleanup retention namespace is incomplete", ErrExecutionIntegrity)
	}
	if state.IntentPresent {
		raw, id, encodeErr := EncodeParentCleanupRetentionIntent(state.Intent)
		if encodeErr != nil || id != state.IntentID {
			return fmt.Errorf("%w: parent-cleanup retention intent identity changed", ErrExecutionIntegrity)
		}
		path, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory, parentCleanupRetentionIntentFile})
		if _, err := verifyParentCleanupRetentionNamedBytes(ctx, subtree, path, raw); err != nil {
			return err
		}
		pending := parentCleanupRetentionTemporaryName("intent", id)
		if allowPending && parentCleanupRetentionEntryPresent(listing.Entries, pending) {
			pendingPath, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory, pending})
			if _, err := verifyParentCleanupRetentionNamedBytes(ctx, subtree, pendingPath, raw); err != nil {
				return err
			}
		}
	}
	if state.CompletePresent {
		raw, id, encodeErr := EncodeParentCleanupRetentionComplete(state.Complete)
		if encodeErr != nil || id != state.CompleteID {
			return fmt.Errorf("%w: parent-cleanup retention completion identity changed", ErrExecutionIntegrity)
		}
		path, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory, parentCleanupRetentionCompleteFile})
		if _, err := verifyParentCleanupRetentionNamedBytes(ctx, subtree, path, raw); err != nil {
			return err
		}
		pending := parentCleanupRetentionTemporaryName("complete", id)
		if allowPending && parentCleanupRetentionEntryPresent(listing.Entries, pending) {
			pendingPath, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory, pending})
			if _, err := verifyParentCleanupRetentionNamedBytes(ctx, subtree, pendingPath, raw); err != nil {
				return err
			}
		}
	}
	after, err := subtree.List(ctx, directoryPath, fsbind.ListLimits{MaxEntries: 5, MaxNameBytes: 1 << 20})
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	if !after.Complete || !sameExecutionRetentionEntries(listing.Entries, after.Entries) {
		return fmt.Errorf("%w: parent-cleanup retention namespace changed during observation", ErrExecutionIntegrity)
	}
	return classifyExecutionBindingError(subtree.CheckPaths(directoryPath))
}

func inventoryParentCleanupRetentionState(ctx context.Context, subtree *fsbind.Subtree, marker ParentCleanupRetentionIntent, limits ExecutionRetentionLimits, requireHeavy bool) ([]executionRetentionNode, ExecutionRetentionUsage, error) {
	usage := ExecutionRetentionUsage{}
	if subtree == nil || marker.Validate() != nil || limits.Validate() != nil {
		return nil, usage, fmt.Errorf("%w: parent-cleanup retention inventory input is invalid", ErrExecutionPolicy)
	}
	root, err := subtree.List(ctx, fsbind.Path{}, fsbind.ListLimits{MaxEntries: 5, MaxNameBytes: 1 << 20})
	if err != nil {
		return nil, usage, classifyExecutionBindingError(err)
	}
	if !root.Complete {
		return nil, usage, fmt.Errorf("%w: parent-cleanup operation inventory is incomplete", ErrExecutionIntegrity)
	}
	usage.DirectoryEntriesExamined += root.Used.EntriesExamined
	present := map[string]bool{}
	lockPresent := false
	for _, entry := range root.Entries {
		switch entry.Name {
		case ".fsbind-operation.lock":
			lockPresent = entry.Kind == string(fsbind.ObjectKindRegular)
		case parentCleanupJournalDirectory, parentCleanupScratchDirectory, parentCleanupRetentionDirectory:
			if entry.Kind != string(fsbind.ObjectKindDirectory) {
				return nil, usage, fmt.Errorf("%w: parent-cleanup control directory is unsafe", ErrExecutionIntegrity)
			}
			present[entry.Name] = true
		default:
			return nil, usage, fmt.Errorf("%w: parent-cleanup operation contains an unexpected object", ErrExecutionIntegrity)
		}
	}
	if !lockPresent || (!requireHeavy && !present[parentCleanupRetentionDirectory]) ||
		(requireHeavy && (!present[parentCleanupJournalDirectory] || !present[parentCleanupScratchDirectory])) {
		return nil, usage, fmt.Errorf("%w: parent-cleanup operation state is incomplete before pruning", ErrExecutionIntegrity)
	}
	capacityByMemory := int(limits.MaxMemoryBytes / 192)
	nodes := make([]executionRetentionNode, 0, min(limits.MaxObjects, 2*marker.ParentsRemoved+2, capacityByMemory))
	paths := []fsbind.Path{fsbind.Path{}}
	if present[parentCleanupJournalDirectory] {
		path, _ := fsbind.PathFromComponents([]string{parentCleanupJournalDirectory})
		paths = append(paths, path)
		if err := addParentCleanupRetentionDirectory(ctx, subtree, path, parentCleanupJournalDirectory, marker, limits, &usage, &nodes, true); err != nil {
			return nil, usage, err
		}
	}
	if present[parentCleanupScratchDirectory] {
		path, _ := fsbind.PathFromComponents([]string{parentCleanupScratchDirectory})
		paths = append(paths, path)
		if err := addParentCleanupRetentionDirectory(ctx, subtree, path, parentCleanupScratchDirectory, marker, limits, &usage, &nodes, false); err != nil {
			return nil, usage, err
		}
	}
	if present[parentCleanupRetentionDirectory] {
		path, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory})
		paths = append(paths, path)
	}
	if err := subtree.CheckPaths(paths...); err != nil {
		return nil, usage, classifyExecutionBindingError(err)
	}
	return nodes, usage, nil
}

func addParentCleanupRetentionDirectory(ctx context.Context, subtree *fsbind.Subtree, path fsbind.Path, name string, marker ParentCleanupRetentionIntent,
	limits ExecutionRetentionLimits, usage *ExecutionRetentionUsage, nodes *[]executionRetentionNode, allowMarkers bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := subtree.Inspect(ctx, path)
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	if directory.Kind != fsbind.ObjectKindDirectory || directory.Identity.IsZero() {
		return fmt.Errorf("%w: parent-cleanup operation directory is unsafe", ErrExecutionIntegrity)
	}
	if err := chargeExecutionRetentionObject(usage, limits, int64(len(name)), 0); err != nil {
		return err
	}
	*nodes = append(*nodes, executionRetentionNode{path: path, identity: directory.Identity, kind: fsbind.ObjectKindDirectory})
	remainingObjects := limits.MaxObjects - usage.ObjectsConsidered
	remainingPath := limits.MaxPathBytes - usage.PathBytesConsidered
	remainingMemory := limits.MaxMemoryBytes - usage.MemoryBytesConsidered
	maximum := fsbind.MaximumListLimits()
	maxByMemory := int(remainingMemory/executionRetentionEntryMemoryBytes) - 1
	if remainingObjects <= 0 || remainingPath <= 0 || maxByMemory <= 0 {
		return fmt.Errorf("%w: parent-cleanup retention directory budget is exhausted", ErrExecutionPolicy)
	}
	listing, err := subtree.List(ctx, path, fsbind.ListLimits{MaxEntries: min(maximum.MaxEntries, remainingObjects, maxByMemory), MaxNameBytes: min(maximum.MaxNameBytes, remainingPath)})
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	listingMemory := int64(listing.Used.EntriesExamined) * executionRetentionEntryMemoryBytes
	if listingMemory > limits.MaxMemoryBytes-usage.MemoryBytesConsidered {
		return fmt.Errorf("%w: parent-cleanup retention listing memory budget is exhausted", ErrExecutionPolicy)
	}
	usage.MemoryBytesConsidered += listingMemory
	usage.DirectoryEntriesExamined += listing.Used.EntriesExamined
	if !listing.Complete {
		return fmt.Errorf("%w: parent-cleanup retention directory inventory limit is exhausted", ErrExecutionPolicy)
	}
	if !allowMarkers && len(listing.Entries) != 0 {
		return fmt.Errorf("%w: completed parent-cleanup scratch is not empty", ErrExecutionIntegrity)
	}
	for _, entry := range listing.Entries {
		if entry.Kind != string(fsbind.ObjectKindRegular) || !validRetainedParentCleanupJournalName(entry.Name, marker.ParentsRemoved) {
			return fmt.Errorf("%w: parent-cleanup journal contains an unexpected retention object", ErrExecutionIntegrity)
		}
		child, pathErr := fsbind.PathFromComponents([]string{name, entry.Name})
		if pathErr != nil {
			return fmt.Errorf("%w: parent-cleanup retention path is unsafe", ErrExecutionIntegrity)
		}
		object, inspectErr := subtree.Inspect(ctx, child)
		if inspectErr != nil {
			return classifyExecutionBindingError(inspectErr)
		}
		if object.Kind != fsbind.ObjectKindRegular || object.Identity.IsZero() || object.SizeBytes < 0 {
			return fmt.Errorf("%w: parent-cleanup retention object is unsafe", ErrExecutionIntegrity)
		}
		if err := chargeExecutionRetentionObject(usage, limits, int64(len(name)+len(entry.Name)), object.SizeBytes); err != nil {
			return err
		}
		*nodes = append(*nodes, executionRetentionNode{path: child, identity: object.Identity, kind: fsbind.ObjectKindRegular, size: object.SizeBytes})
	}
	after, err := subtree.Inspect(ctx, path)
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	if after.Kind != fsbind.ObjectKindDirectory || !after.Identity.Equal(directory.Identity) {
		return fmt.Errorf("%w: parent-cleanup directory changed during retention inventory", ErrExecutionIntegrity)
	}
	return nil
}

func validRetainedParentCleanupJournalName(name string, parents int) bool {
	if name == parentCleanupIntentFile || name == parentCleanupCompleteFile {
		return true
	}
	for _, prefix := range []string{"attempt-", "removed-"} {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".json") && len(name) == len(prefix)+6+len(".json") {
			sequence, err := strconv.Atoi(name[len(prefix) : len(prefix)+6])
			return err == nil && sequence >= 0 && sequence < parents && fmt.Sprintf("%06d", sequence) == name[len(prefix):len(prefix)+6]
		}
	}
	return false
}

func removeParentCleanupRetentionState(ctx context.Context, subtree *fsbind.Subtree, nodes []executionRetentionNode, usage ExecutionRetentionUsage) (ExecutionRetentionUsage, error) {
	for index := len(nodes) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			return usage, err
		}
		node := nodes[index]
		usage.RemovalAttempts++
		var receipt fsbind.Removal
		var err error
		if node.kind == fsbind.ObjectKindRegular {
			receipt, err = subtree.RemoveRegularExact(ctx, node.path, node.identity, node.size)
		} else {
			receipt, err = subtree.RemoveEmptyDirectory(ctx, node.path, node.identity)
		}
		if receipt.Removed {
			if node.kind == fsbind.ObjectKindRegular {
				usage.FilesRemoved++
				if receipt.SizeBytes > math.MaxInt64-usage.BytesRemoved {
					return usage, fmt.Errorf("%w: parent-cleanup removed-byte receipt overflow", ErrExecutionIntegrity)
				}
				usage.BytesRemoved += receipt.SizeBytes
			} else {
				usage.DirectoriesRemoved++
			}
		}
		if errors.Is(err, fsbind.ErrRemovalAmbiguous) {
			usage.AmbiguousRemovals++
		}
		if err != nil {
			return usage, err
		}
	}
	return usage, nil
}

func openParentCleanupRetentionSubtree(target *fsbind.Session, operation ParentCleanupOperationID) (*fsbind.Subtree, error) {
	name, err := ParentCleanupOperationDirectoryName(operation)
	if err != nil {
		return nil, err
	}
	subtree, err := target.OpenPrivateSubtreeObserved(name)
	if errors.Is(err, fsbind.ErrNotFound) {
		return nil, ErrOperationNotFound
	}
	return subtree, err
}

func auditParentCleanupRetentionBeforeIntent(ctx context.Context, subtree *fsbind.Subtree, state parentCleanupRetentionState, marker ParentCleanupRetentionIntent) error {
	if !state.DirectoryPresent {
		return nil
	}
	raw, id, err := EncodeParentCleanupRetentionIntent(marker)
	if err != nil {
		return err
	}
	expectedPending := parentCleanupRetentionTemporaryName("intent", id)
	if len(state.Entries) > 1 {
		return fmt.Errorf("%w: unsealed parent-cleanup retention directory is not empty", ErrExecutionIntegrity)
	}
	if len(state.Entries) == 1 {
		entry := state.Entries[0]
		if entry.Name != expectedPending || entry.Kind != string(fsbind.ObjectKindRegular) {
			return fmt.Errorf("%w: unsealed parent-cleanup retention directory contains an unexpected object", ErrExecutionIntegrity)
		}
		pending, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory, expectedPending})
		if _, err := verifyParentCleanupRetentionNamedBytes(ctx, subtree, pending, raw); err != nil {
			return err
		}
	}
	path, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory})
	return classifyExecutionBindingError(subtree.CheckPaths(path))
}

func auditParentCleanupRetentionHeavyAbsent(ctx context.Context, subtree *fsbind.Subtree) error {
	root, err := subtree.List(ctx, fsbind.Path{}, fsbind.ListLimits{MaxEntries: 3, MaxNameBytes: 1 << 20})
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	if !root.Complete || len(root.Entries) != 2 {
		return fmt.Errorf("%w: pruned parent-cleanup namespace is not exact", ErrExecutionIntegrity)
	}
	seenLock, seenRetention := false, false
	for _, entry := range root.Entries {
		switch entry.Name {
		case ".fsbind-operation.lock":
			seenLock = entry.Kind == string(fsbind.ObjectKindRegular)
		case parentCleanupRetentionDirectory:
			seenRetention = entry.Kind == string(fsbind.ObjectKindDirectory)
		}
	}
	if !seenLock || !seenRetention {
		return fmt.Errorf("%w: pruned parent-cleanup namespace is invalid", ErrExecutionIntegrity)
	}
	path, _ := fsbind.PathFromComponents([]string{parentCleanupRetentionDirectory})
	return classifyExecutionBindingError(subtree.CheckPaths(fsbind.Path{}, path))
}

func verifyParentCleanupRetentionTombstone(ctx context.Context, subtree *fsbind.Subtree, operation ParentCleanupOperationID, targetIdentity fsbind.Identity) error {
	state, err := loadParentCleanupRetentionState(ctx, subtree, operation, targetIdentity)
	if err != nil {
		return err
	}
	if !state.IntentPresent || !state.CompletePresent {
		return fmt.Errorf("%w: parent-cleanup retention tombstone is incomplete", ErrExecutionIntegrity)
	}
	if err := auditParentCleanupRetentionHeavyAbsent(ctx, subtree); err != nil {
		return err
	}
	if err := auditParentCleanupRetentionControl(ctx, subtree, state, false); err != nil {
		return err
	}
	return auditParentCleanupRetentionHeavyAbsent(ctx, subtree)
}

// PruneParentCleanup deletes only one terminal cleanup operation's private
// attempt/removal journal after sealing a no-path historical tombstone.
func PruneParentCleanup(ctx context.Context, options ParentCleanupPruneOptions) (ParentCleanupRetentionReport, error) {
	report := newParentCleanupRetentionReport(options)
	parsed, parseErr := ParseParentCleanupOperationID(options.OperationID.String())
	derived, deriveErr := ParentCleanupOperationIDForPlanID(options.ExpectedCleanupPlanID)
	if options.TargetRoot == "" || parseErr != nil || deriveErr != nil || parsed != options.OperationID || derived != options.OperationID ||
		options.JournalLimits.Validate() != nil || options.RetentionLimits.Validate() != nil {
		return parentCleanupRetentionBlocked(&report, "prune.selector_invalid", "parent-cleanup prune selectors or limits are invalid")
	}
	if !options.Acknowledge {
		return parentCleanupRetentionBlocked(&report, "prune.acknowledgement_required", "parent-cleanup prune requires its dedicated operation-state deletion acknowledgement")
	}
	if err := ctx.Err(); err != nil {
		return mapParentCleanupRetentionError(&report, err, "parent-cleanup prune was interrupted")
	}
	absolute, err := filepath.Abs(options.TargetRoot)
	if err != nil {
		return parentCleanupRetentionBlocked(&report, "target.invalid_root", "the parent-cleanup target root is invalid")
	}
	target, rootInfo, err := fsbind.BindExisting(filepath.Clean(absolute))
	if err != nil {
		return mapParentCleanupRetentionError(&report, err, "the parent-cleanup target root could not be bound")
	}
	defer target.Close()
	report.Target = ExecutionRetentionTargetReport{ObservedRootIdentity: rootInfo.Identity.String(), RootIdentityBound: true, StabilityAssurance: "identity_bound_bracketed_non_atomic"}
	forgetting, forgetErr := inspectParentCleanupForgetControl(ctx, target, options.OperationID, options.ExpectedCleanupPlanID, nil)
	if forgetErr != nil {
		return mapParentCleanupRetentionError(&report, forgetErr, "the parent-cleanup forget boundary could not be inspected")
	}
	if forgetting {
		return parentCleanupRetentionBlocked(&report, "prune.forget_in_progress", "the exact parent-cleanup tombstone is already crossing its separately acknowledged forget boundary")
	}
	subtree, err := openParentCleanupRetentionSubtree(target, options.OperationID)
	if err != nil {
		return mapParentCleanupRetentionError(&report, err, "the explicit parent-cleanup operation does not exist")
	}
	defer subtree.Close()
	state, err := loadParentCleanupRetentionState(ctx, subtree, options.OperationID, rootInfo.Identity)
	if err != nil {
		return mapParentCleanupRetentionError(&report, err, "parent-cleanup retention state could not be loaded")
	}
	var marker ParentCleanupRetentionIntent
	if state.IntentPresent {
		marker = state.Intent
		if err := selectParentCleanupRetentionMarker(&report, marker, options, rootInfo.Identity, subtree.Identity()); err != nil {
			return mapParentCleanupRetentionError(&report, err, "parent-cleanup retention selector disagrees")
		}
		report.Markers.State = "intent_visible_durability_unconfirmed"
		report.Markers.IntentMarkerID = state.IntentID.String()
		report.Markers.PruneResumable = true
		if handle, journalErr := loadParentCleanupJournalFromSubtree(ctx, target, subtree, options.JournalLimits); journalErr == nil {
			if err := validateParentCleanupRetentionAgainstJournal(marker, handle); err != nil {
				return mapParentCleanupRetentionError(&report, err, "the parent-cleanup retention marker disagrees with its terminal journal")
			}
		}
		if err := confirmParentCleanupRetentionDurability(ctx, subtree); err != nil {
			return mapParentCleanupRetentionError(&report, err, "the parent-cleanup retention intent durability could not be confirmed")
		}
		report.Markers.IntentDurable = true
		intentID, receipt, ensureErr := ensureParentCleanupRetentionIntent(ctx, subtree, marker)
		report.recordParentCleanupRetentionMarker(receipt)
		if ensureErr != nil {
			return mapParentCleanupRetentionError(&report, ensureErr, "the parent-cleanup retention intent could not be re-established")
		}
		state.IntentID = intentID
		if state.CompletePresent {
			completeID, completeReceipt, completeErr := ensureParentCleanupRetentionComplete(ctx, subtree, state.Complete)
			report.recordParentCleanupRetentionMarker(completeReceipt)
			if completeErr != nil {
				return mapParentCleanupRetentionError(&report, completeErr, "the parent-cleanup retention completion could not be re-established")
			}
			state.CompleteID = completeID
			if err := verifyParentCleanupRetentionTombstone(ctx, subtree, options.OperationID, rootInfo.Identity); err != nil {
				return mapParentCleanupRetentionError(&report, err, "the parent-cleanup retention tombstone is invalid")
			}
			report.Markers = ParentCleanupRetentionMarkerReport{State: "complete", IntentMarkerID: state.IntentID.String(), CompleteMarkerID: state.CompleteID.String(), IntentDurable: true, CompletionDurable: true, ExactTombstone: true}
			report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "retained", "retained", false
			if report.WritesPerformed == 0 {
				report.Outcome = ParentCleanupRetentionOutcomeAlreadyPruned
			} else {
				report.Outcome = ParentCleanupRetentionOutcomePruned
			}
			report.finalize()
			return report, nil
		}
		fresh, loadErr := loadParentCleanupRetentionState(ctx, subtree, options.OperationID, rootInfo.Identity)
		if loadErr != nil {
			return mapParentCleanupRetentionError(&report, loadErr, "the parent-cleanup retention intent could not be reloaded")
		}
		if err := auditParentCleanupRetentionControl(ctx, subtree, fresh, true); err != nil {
			return mapParentCleanupRetentionError(&report, err, "parent-cleanup retention control namespace is invalid")
		}
		state = fresh
	} else {
		handle, loadErr := loadParentCleanupJournalFromSubtree(ctx, target, subtree, options.JournalLimits)
		if loadErr != nil {
			return mapParentCleanupRetentionError(&report, loadErr, "the terminal parent-cleanup journal could not be loaded")
		}
		if !handle.state.CompletePresent {
			return parentCleanupRetentionBlocked(&report, "operation.not_terminal", "only a parent-cleanup operation with a durable completion may be pruned")
		}
		marker, err = prepareParentCleanupRetentionIntent(handle, options, rootInfo.Identity, &report)
		if err != nil {
			return mapParentCleanupRetentionError(&report, err, "the terminal parent-cleanup journal could not authorize pruning")
		}
		if err := auditParentCleanupRetentionBeforeIntent(ctx, subtree, state, marker); err != nil {
			return mapParentCleanupRetentionError(&report, err, "the parent-cleanup retention boundary is not clean")
		}
	}
	nodes, usage, inventoryErr := inventoryParentCleanupRetentionState(ctx, subtree, marker, options.RetentionLimits, !state.IntentPresent)
	report.Used = usage
	if inventoryErr != nil {
		return mapParentCleanupRetentionError(&report, inventoryErr, "parent-cleanup private state could not be completely inventoried")
	}
	if !state.IntentPresent {
		intentID, receipt, publishErr := ensureParentCleanupRetentionIntent(ctx, subtree, marker)
		report.recordParentCleanupRetentionMarker(receipt)
		if publishErr != nil {
			if intentID != "" && (receipt.AlreadyPresent || receipt.Publication.Published) {
				report.Markers.State, report.Markers.IntentMarkerID = "intent_publication_observed_unverified", intentID.String()
			} else if receipt.Publication.Attempted {
				report.Markers.State = "intent_publication_ambiguous"
			}
			return mapParentCleanupRetentionError(&report, publishErr, "parent-cleanup retention intent publication failed")
		}
		state, err = loadParentCleanupRetentionState(ctx, subtree, options.OperationID, rootInfo.Identity)
		if err != nil || !state.IntentPresent || state.IntentID != intentID {
			return mapParentCleanupRetentionError(&report, fmt.Errorf("%w: parent-cleanup retention intent could not be reloaded", ErrExecutionIntegrity), "parent-cleanup retention intent could not be reloaded")
		}
		if err := auditParentCleanupRetentionControl(ctx, subtree, state, true); err != nil {
			return mapParentCleanupRetentionError(&report, err, "parent-cleanup retention control namespace is invalid")
		}
		report.Markers.State, report.Markers.IntentMarkerID, report.Markers.IntentDurable, report.Markers.PruneResumable = "intent_published", state.IntentID.String(), true, true
		if parentCleanupRetentionTransitionHook != nil {
			if hookErr := parentCleanupRetentionTransitionHook("intent_published"); hookErr != nil {
				return mapParentCleanupRetentionError(&report, hookErr, "parent-cleanup pruning stopped after its durable intent")
			}
		}
	}
	report.Markers.State, report.Markers.IntentMarkerID, report.Markers.IntentDurable, report.Markers.PruneResumable = "intent_published", state.IntentID.String(), true, true
	removed, removeErr := removeParentCleanupRetentionState(ctx, subtree, nodes, report.Used)
	report.recordParentCleanupRetentionRemovals(removed)
	if removeErr != nil {
		return mapParentCleanupRetentionError(&report, removeErr, "parent-cleanup private-state deletion was interrupted")
	}
	if err := auditParentCleanupRetentionHeavyAbsent(ctx, subtree); err != nil {
		return mapParentCleanupRetentionError(&report, err, "parent-cleanup private state remains after pruning")
	}
	if parentCleanupRetentionTransitionHook != nil {
		if hookErr := parentCleanupRetentionTransitionHook("heavy_state_removed"); hookErr != nil {
			return mapParentCleanupRetentionError(&report, hookErr, "parent-cleanup pruning stopped after private-state removal")
		}
	}
	complete := ParentCleanupRetentionComplete{Schema: ParentCleanupRetentionCompleteSchemaV1, OperationID: options.OperationID,
		OperationRootIdentity: subtree.Identity().String(), TargetRootIdentity: rootInfo.Identity.String(), IntentMarkerID: state.IntentID}
	completeID, receipt, completeErr := ensureParentCleanupRetentionComplete(ctx, subtree, complete)
	report.recordParentCleanupRetentionMarker(receipt)
	if completeErr != nil {
		if completeID != "" && (receipt.AlreadyPresent || receipt.Publication.Published) {
			report.Markers.State, report.Markers.CompleteMarkerID = "completion_publication_observed_unverified", completeID.String()
		} else if receipt.Publication.Attempted {
			report.Markers.State = "completion_publication_ambiguous"
		}
		return mapParentCleanupRetentionError(&report, completeErr, "parent-cleanup retention completion publication failed")
	}
	if err := verifyParentCleanupRetentionTombstone(ctx, subtree, options.OperationID, rootInfo.Identity); err != nil {
		return mapParentCleanupRetentionError(&report, err, "parent-cleanup retention tombstone could not be reverified")
	}
	report.Markers = ParentCleanupRetentionMarkerReport{State: "complete", IntentMarkerID: state.IntentID.String(), CompleteMarkerID: completeID.String(), IntentDurable: true, CompletionDurable: true, ExactTombstone: true}
	report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = "retained", "retained", false
	report.Outcome = ParentCleanupRetentionOutcomePruned
	report.finalize()
	return report, nil
}

func validateParentCleanupRetentionAgainstJournal(marker ParentCleanupRetentionIntent, handle *parentCleanupJournal) error {
	if handle == nil || handle.target == nil || !handle.state.CompletePresent {
		return fmt.Errorf("%w: terminal parent-cleanup journal is unavailable", ErrExecutionIntegrity)
	}
	expected, err := prepareParentCleanupRetentionIntent(handle, ParentCleanupPruneOptions{
		OperationID: marker.OperationID, ExpectedCleanupPlanID: marker.CleanupPlanID,
	}, handle.target.Info().Identity, &ParentCleanupRetentionReport{})
	if err != nil || expected != marker {
		return fmt.Errorf("%w: parent-cleanup retention intent disagrees with its terminal journal", ErrExecutionIntegrity)
	}
	return nil
}

func prepareParentCleanupRetentionIntent(handle *parentCleanupJournal, options ParentCleanupPruneOptions, targetIdentity fsbind.Identity, report *ParentCleanupRetentionReport) (ParentCleanupRetentionIntent, error) {
	if handle == nil || handle.subtree == nil || !handle.state.CompletePresent {
		return ParentCleanupRetentionIntent{}, fmt.Errorf("%w: terminal parent-cleanup authority is unavailable", ErrExecutionIntegrity)
	}
	intent, complete := handle.state.Intent, handle.state.Complete
	if intent.CleanupPlanID != options.ExpectedCleanupPlanID {
		return ParentCleanupRetentionIntent{}, fmt.Errorf("%w: reviewed cleanup plan does not select this operation", ErrExecutionPolicy)
	}
	retiredFiles := 0
	for _, directory := range intent.Directories {
		if directory.RetiredFiles > hardExecutionMaxFiles-retiredFiles {
			return ParentCleanupRetentionIntent{}, fmt.Errorf("%w: parent-cleanup retained-file count overflows", ErrExecutionIntegrity)
		}
		retiredFiles += directory.RetiredFiles
	}
	marker := ParentCleanupRetentionIntent{Schema: ParentCleanupRetentionIntentSchemaV1, OperationID: intent.OperationID,
		OperationRootIdentity: handle.subtree.Identity().String(), TargetRootIdentity: targetIdentity.String(),
		CleanupPlanID: intent.CleanupPlanID, IntentID: handle.state.IntentID, CompletionID: handle.state.CompleteID,
		Basis: ParentCleanupRetentionBasisComplete, RetirementOperationID: intent.RetirementOperationID,
		RetirementPlanID: intent.RetirementPlanID, RetirementCompletionID: intent.RetirementCompletionID,
		SearchScopeID: intent.SearchScopeID, ParentsRemoved: complete.ParentsRemoved, RetiredFiles: retiredFiles}
	if err := marker.Validate(); err != nil {
		return ParentCleanupRetentionIntent{}, err
	}
	if err := selectParentCleanupRetentionMarker(report, marker, options, targetIdentity, handle.subtree.Identity()); err != nil {
		return ParentCleanupRetentionIntent{}, err
	}
	return marker, nil
}

func selectParentCleanupRetentionMarker(report *ParentCleanupRetentionReport, marker ParentCleanupRetentionIntent, options ParentCleanupPruneOptions, targetIdentity, operationIdentity fsbind.Identity) error {
	if marker.Validate() != nil || marker.OperationID != options.OperationID || marker.CleanupPlanID != options.ExpectedCleanupPlanID ||
		marker.TargetRootIdentity != targetIdentity.String() || marker.OperationRootIdentity != operationIdentity.String() {
		return fmt.Errorf("%w: parent-cleanup retention selector disagrees", ErrExecutionPolicy)
	}
	report.Operation = ParentCleanupExecutionOperationReport{ID: marker.OperationID.String(), PlanID: marker.CleanupPlanID, Status: "pruning", Phase: "complete", IntentID: marker.IntentID, CompletionID: marker.CompletionID}
	report.Target.ExpectedRootIdentity = marker.TargetRootIdentity
	report.Proof = ParentCleanupRetentionProofReport{Basis: marker.Basis, IntentID: marker.IntentID, TerminalCompletionID: marker.CompletionID,
		RetirementOperationID: marker.RetirementOperationID.String(), RetirementPlanID: marker.RetirementPlanID,
		RetirementCompletionID: marker.RetirementCompletionID, SearchScopeID: marker.SearchScopeID,
		ParentsRemoved: marker.ParentsRemoved, RetiredFiles: marker.RetiredFiles, HistoricalAuthority: true,
		Assurance: "historical_exact_terminal_parent_cleanup_bound_to_private_retention_marker"}
	return nil
}

func applyParentCleanupRetentionControl(ctx context.Context, target *fsbind.Session, operation ParentCleanupOperationID, report *ParentCleanupExecutionReport) (bool, error) {
	subtree, err := openParentCleanupRetentionSubtree(target, operation)
	if errors.Is(err, ErrOperationNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer subtree.Close()
	state, err := loadParentCleanupRetentionState(ctx, subtree, operation, target.Info().Identity)
	if err != nil {
		return state.DirectoryPresent, err
	}
	if !state.DirectoryPresent {
		return false, nil
	}
	report.addEffect("read_private_parent_cleanup_retention_state")
	report.Directories = []ParentCleanupExecutionDirectoryReport{}
	report.Operation = ParentCleanupExecutionOperationReport{ID: operation.String(), Status: "retention_initializing", Phase: "retention_initializing"}
	if !state.IntentPresent {
		if err := auditParentCleanupRetentionControl(ctx, subtree, state, true); err != nil {
			return true, err
		}
		report.Outcome, report.Operation.Resumable = ParentCleanupExecutionOutcomeIncomplete, false
		report.addBlocker("operation.prune_required", "a reserved parent-cleanup retention boundary exists; only explicit prune may recover it")
		report.finalize()
		return true, nil
	}
	marker := state.Intent
	report.Target.ExpectedRootIdentity = marker.TargetRootIdentity
	report.Operation.PlanID, report.Operation.IntentID, report.Operation.CompletionID = marker.CleanupPlanID, marker.IntentID, marker.CompletionID
	report.RetirementOperationID, report.RetirementPlanID, report.RetirementCompletionID, report.SearchScopeID = marker.RetirementOperationID.String(), marker.RetirementPlanID, marker.RetirementCompletionID, marker.SearchScopeID
	report.Used.ParentsConsidered = marker.ParentsRemoved
	report.Warnings = append(report.Warnings, "heavy parent-cleanup journal state is being pruned or has been replaced by a no-path historical tombstone")
	if state.CompletePresent {
		if err := verifyParentCleanupRetentionTombstone(ctx, subtree, operation, target.Info().Identity); err != nil {
			return true, err
		}
		report.Outcome, report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = ParentCleanupExecutionOutcomeAlreadyRemoved, "retained", "retained", false
		report.Warnings = append(report.Warnings, "retained status is historical only and does not prove that a removed parent remains absent now")
		report.finalize()
		return true, nil
	}
	if err := auditParentCleanupRetentionControl(ctx, subtree, state, true); err != nil {
		return true, err
	}
	report.Outcome, report.Operation.Status, report.Operation.Phase, report.Operation.Resumable = ParentCleanupExecutionOutcomePartial, "pruning", "retention_intent_recorded", false
	report.addBlocker("operation.prune_required", "retention intent is durable; only explicit parent-cleanup prune may continue deletion of private state")
	report.finalize()
	return true, nil
}

func (report *ParentCleanupRetentionReport) recordParentCleanupRetentionMarker(receipt executionRetentionMarkerReceipt) {
	if receipt.DirectoryCreated {
		report.WritesPerformed++
		report.Writes.ControlDirectoriesCreated++
		if receipt.DirectoryDurability != fsbind.DurabilityConfirmed {
			report.WritesUncertain = true
		}
	}
	if receipt.TemporaryCreated {
		report.WritesPerformed++
		report.Writes.MarkerTemporaryFiles++
		report.Writes.MarkerTemporaryBytes += receipt.TemporaryBytesWritten
	}
	if receipt.Publication.Attempted {
		report.Writes.MarkerPublicationAttempts++
	}
	if receipt.Publication.Published {
		report.WritesPerformed++
		report.Writes.MarkerPublications++
		if receipt.Publication.Durability != fsbind.DurabilityConfirmed {
			report.WritesUncertain = true
		}
	}
	if receipt.TemporaryRemoval.Removed {
		report.WritesPerformed++
		report.Writes.MarkerTemporaryRemovals++
		if receipt.TemporaryRemoval.Durability != fsbind.DurabilityConfirmed {
			report.WritesUncertain = true
		}
	}
}

func (report *ParentCleanupRetentionReport) recordParentCleanupRetentionRemovals(usage ExecutionRetentionUsage) {
	report.Used = usage
	report.Writes.RemovalAttempts, report.Writes.FilesRemoved, report.Writes.DirectoriesRemoved = usage.RemovalAttempts, usage.FilesRemoved, usage.DirectoriesRemoved
	report.Writes.BytesRemoved, report.Writes.AmbiguousRemovals = usage.BytesRemoved, usage.AmbiguousRemovals
	report.WritesPerformed += usage.FilesRemoved + usage.DirectoriesRemoved
	if usage.AmbiguousRemovals != 0 {
		report.WritesUncertain = true
	}
}

func parentCleanupRetentionBlocked(report *ParentCleanupRetentionReport, code, message string) (ParentCleanupRetentionReport, error) {
	report.Outcome = ParentCleanupRetentionOutcomeBlocked
	report.Blockers = append(report.Blockers, Finding{Code: code, Message: message})
	report.finalize()
	return *report, nil
}

func mapParentCleanupRetentionError(report *ParentCleanupRetentionReport, err error, message string) (ParentCleanupRetentionReport, error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		report.Outcome = ParentCleanupRetentionOutcomeInterrupted
		report.Issues = append(report.Issues, Finding{Code: "prune.interrupted", Message: message})
	case errors.Is(err, ErrExecutionPolicy), errors.Is(err, ErrOperationNotFound), errors.Is(err, fsbind.ErrUnsupported), errors.Is(err, fsbind.ErrBusy):
		report.Outcome = ParentCleanupRetentionOutcomeBlocked
		report.Blockers = append(report.Blockers, Finding{Code: "prune.policy_blocked", Message: message})
	case errors.Is(err, ErrExecutionIntegrity), errors.Is(err, fsbind.ErrNotFound), errors.Is(err, fsbind.ErrUnsafeObject), errors.Is(err, fsbind.ErrBindingChanged), errors.Is(err, fsbind.ErrCrossFilesystem):
		report.Outcome, report.Operation.Resumable, report.Markers.PruneResumable = ParentCleanupRetentionOutcomeIntegrityFailed, false, false
		report.Issues = append(report.Issues, Finding{Code: "prune.integrity_failed", Message: message})
		err = fmt.Errorf("%w: %s", ErrExecutionIntegrity, message)
	default:
		report.Outcome = ParentCleanupRetentionOutcomeInterrupted
		report.Issues = append(report.Issues, Finding{Code: "prune.operation_failed", Message: message})
		if errors.Is(err, fsbind.ErrPublicationAmbiguous) || errors.Is(err, fsbind.ErrRemovalAmbiguous) || errors.Is(err, fsbind.ErrDurabilityUnconfirmed) {
			report.WritesUncertain = true
		}
	}
	report.finalize()
	return *report, err
}

func (report *ParentCleanupRetentionReport) finalize() {
	if report.Effect == nil {
		report.Effect = []string{}
	}
	if report.Blockers == nil {
		report.Blockers = []Finding{}
	}
	if report.Issues == nil {
		report.Issues = []Finding{}
	}
	if report.Warnings == nil {
		report.Warnings = []string{}
	}
	sort.Strings(report.Effect)
	sort.Slice(report.Blockers, func(i, j int) bool {
		if report.Blockers[i].Code != report.Blockers[j].Code {
			return report.Blockers[i].Code < report.Blockers[j].Code
		}
		return report.Blockers[i].Message < report.Blockers[j].Message
	})
	sort.Slice(report.Issues, func(i, j int) bool {
		if report.Issues[i].Code != report.Issues[j].Code {
			return report.Issues[i].Code < report.Issues[j].Code
		}
		return report.Issues[i].Message < report.Issues[j].Message
	})
	sort.Strings(report.Warnings)
}
