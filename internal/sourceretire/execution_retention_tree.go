package sourceretire

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const executionRetentionEntryMemoryBytes = int64(2 << 10)

type executionRetentionNode struct {
	path     fsbind.Path
	identity fsbind.Identity
	kind     fsbind.ObjectKind
	size     int64
}

func inventoryExecutionRetentionState(ctx context.Context, subtree *fsbind.Subtree, marker ExecutionRetentionIntent, limits ExecutionRetentionLimits, requireBoth bool) ([]executionRetentionNode, ExecutionRetentionUsage, error) {
	usage := ExecutionRetentionUsage{}
	if subtree == nil || marker.Validate() != nil || limits.Validate() != nil {
		return nil, usage, fmt.Errorf("%w: source retirement retention inventory input is invalid", ErrExecutionPolicy)
	}
	rootPath, _ := fsbind.PathFromComponents(nil)
	root, err := subtree.List(ctx, rootPath, fsbind.ListLimits{MaxEntries: 5, MaxNameBytes: 1 << 20})
	if err != nil {
		return nil, usage, classifyExecutionBindingError(err)
	}
	if !root.Complete {
		return nil, usage, fmt.Errorf("%w: source retirement operation-root inventory is incomplete", ErrExecutionIntegrity)
	}
	usage.DirectoryEntriesExamined += root.Used.EntriesExamined
	present := map[string]bool{}
	lockPresent := false
	for _, entry := range root.Entries {
		switch entry.Name {
		case ".fsbind-operation.lock":
			if entry.Kind != string(fsbind.ObjectKindRegular) {
				return nil, usage, fmt.Errorf("%w: source retirement operation lock is unsafe", ErrExecutionIntegrity)
			}
			lockPresent = true
		case executionJournalDirectory, executionScratchDirectory, executionRetentionDirectory:
			if entry.Kind != string(fsbind.ObjectKindDirectory) {
				return nil, usage, fmt.Errorf("%w: source retirement operation control directory is unsafe", ErrExecutionIntegrity)
			}
			present[entry.Name] = true
		default:
			return nil, usage, fmt.Errorf("%w: source retirement operation root contains an unexpected object", ErrExecutionIntegrity)
		}
	}
	if !lockPresent || (!requireBoth && !present[executionRetentionDirectory]) ||
		(requireBoth && (!present[executionJournalDirectory] || !present[executionScratchDirectory])) {
		return nil, usage, fmt.Errorf("%w: source retirement operation state is incomplete before pruning", ErrExecutionIntegrity)
	}
	capacityByMemory := int(limits.MaxMemoryBytes / 192)
	nodes := make([]executionRetentionNode, 0, min(limits.MaxObjects, 2*marker.FilesRetired+2, capacityByMemory))
	paths := []fsbind.Path{rootPath}
	if present[executionJournalDirectory] {
		journalPath, _ := fsbind.PathFromComponents([]string{executionJournalDirectory})
		paths = append(paths, journalPath)
		if err := addExecutionRetentionDirectory(ctx, subtree, journalPath, executionJournalDirectory, marker, limits, &usage, &nodes, true); err != nil {
			return nil, usage, err
		}
	}
	if present[executionScratchDirectory] {
		scratchPath, _ := fsbind.PathFromComponents([]string{executionScratchDirectory})
		paths = append(paths, scratchPath)
		if err := addExecutionRetentionDirectory(ctx, subtree, scratchPath, executionScratchDirectory, marker, limits, &usage, &nodes, false); err != nil {
			return nil, usage, err
		}
	}
	if present[executionRetentionDirectory] {
		retentionPath, _ := fsbind.PathFromComponents([]string{executionRetentionDirectory})
		paths = append(paths, retentionPath)
	}
	if err := subtree.CheckPaths(paths...); err != nil {
		return nil, usage, classifyExecutionBindingError(err)
	}
	return nodes, usage, nil
}

func addExecutionRetentionDirectory(ctx context.Context, subtree *fsbind.Subtree, path fsbind.Path, name string, marker ExecutionRetentionIntent,
	limits ExecutionRetentionLimits, usage *ExecutionRetentionUsage, nodes *[]executionRetentionNode, allowMarkers bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := subtree.Inspect(ctx, path)
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	if directory.Kind != fsbind.ObjectKindDirectory || directory.Identity.IsZero() {
		return fmt.Errorf("%w: source retirement operation directory is unsafe", ErrExecutionIntegrity)
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
		return fmt.Errorf("%w: source retirement retention directory budget is exhausted", ErrExecutionPolicy)
	}
	listLimits := fsbind.ListLimits{MaxEntries: min(maximum.MaxEntries, remainingObjects, maxByMemory), MaxNameBytes: min(maximum.MaxNameBytes, remainingPath)}
	listing, err := subtree.List(ctx, path, listLimits)
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	listingMemory := int64(listing.Used.EntriesExamined) * executionRetentionEntryMemoryBytes
	if listingMemory > limits.MaxMemoryBytes-usage.MemoryBytesConsidered {
		return fmt.Errorf("%w: source retirement retention listing memory budget is exhausted", ErrExecutionPolicy)
	}
	usage.MemoryBytesConsidered += listingMemory
	usage.DirectoryEntriesExamined += listing.Used.EntriesExamined
	if !listing.Complete {
		return fmt.Errorf("%w: source retirement retention directory inventory limit is exhausted", ErrExecutionPolicy)
	}
	if !allowMarkers && len(listing.Entries) != 0 {
		return fmt.Errorf("%w: completed source retirement scratch is not empty", ErrExecutionIntegrity)
	}
	for _, entry := range listing.Entries {
		if entry.Kind != string(fsbind.ObjectKindRegular) || !validRetainedExecutionJournalName(entry.Name, marker.FilesRetired) {
			return fmt.Errorf("%w: source retirement journal contains an unexpected retention object", ErrExecutionIntegrity)
		}
		childPath, pathErr := fsbind.PathFromComponents([]string{name, entry.Name})
		if pathErr != nil {
			return fmt.Errorf("%w: source retirement retention path is unsafe", ErrExecutionIntegrity)
		}
		object, inspectErr := subtree.Inspect(ctx, childPath)
		if inspectErr != nil {
			return classifyExecutionBindingError(inspectErr)
		}
		if object.Kind != fsbind.ObjectKindRegular || object.Identity.IsZero() || object.SizeBytes < 0 {
			return fmt.Errorf("%w: source retirement retention object is unsafe", ErrExecutionIntegrity)
		}
		pathBytes := int64(len(name) + len(entry.Name))
		if err := chargeExecutionRetentionObject(usage, limits, pathBytes, object.SizeBytes); err != nil {
			return err
		}
		*nodes = append(*nodes, executionRetentionNode{path: childPath, identity: object.Identity, kind: fsbind.ObjectKindRegular, size: object.SizeBytes})
	}
	after, err := subtree.Inspect(ctx, path)
	if err != nil {
		return classifyExecutionBindingError(err)
	}
	if after.Kind != fsbind.ObjectKindDirectory || !after.Identity.Equal(directory.Identity) {
		return fmt.Errorf("%w: source retirement operation directory changed during retention inventory", ErrExecutionIntegrity)
	}
	return nil
}

func chargeExecutionRetentionObject(usage *ExecutionRetentionUsage, limits ExecutionRetentionLimits, pathBytes, size int64) error {
	if usage == nil || pathBytes < 0 || size < 0 || usage.ObjectsConsidered >= limits.MaxObjects ||
		pathBytes > limits.MaxPathBytes-usage.PathBytesConsidered || size > limits.MaxBytes-usage.BytesConsidered {
		return fmt.Errorf("%w: source retirement retention object, path, or byte budget is exhausted", ErrExecutionPolicy)
	}
	memory := int64(192) + pathBytes
	if memory > limits.MaxMemoryBytes-usage.MemoryBytesConsidered {
		return fmt.Errorf("%w: source retirement retention memory budget is exhausted", ErrExecutionPolicy)
	}
	usage.ObjectsConsidered++
	usage.PathBytesConsidered += pathBytes
	usage.BytesConsidered += size
	usage.MemoryBytesConsidered += memory
	return nil
}

func validRetainedExecutionJournalName(name string, files int) bool {
	if name == executionIntentFile || name == executionCompleteFile {
		return true
	}
	for _, prefix := range []string{"attempt-", "deleted-"} {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".json") && len(name) == len(prefix)+6+len(".json") {
			sequence, err := strconv.Atoi(name[len(prefix) : len(prefix)+6])
			return err == nil && sequence >= 0 && sequence < files && fmt.Sprintf("%06d", sequence) == name[len(prefix):len(prefix)+6]
		}
	}
	return false
}

func removeExecutionRetentionState(ctx context.Context, subtree *fsbind.Subtree, nodes []executionRetentionNode, usage ExecutionRetentionUsage) (ExecutionRetentionUsage, error) {
	if subtree == nil {
		return usage, fmt.Errorf("%w: source retirement retention subtree is unavailable", ErrExecutionPolicy)
	}
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
					return usage, fmt.Errorf("%w: source retirement retention removed-byte receipt overflow", ErrExecutionIntegrity)
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
