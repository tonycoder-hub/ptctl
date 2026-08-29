package materialize

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	defaultRetentionMaxObjects   = 25_000
	defaultRetentionMaxPathBytes = int64(48 << 20)
	defaultRetentionMaxBytes     = int64(3 << 40)
	defaultRetentionMaxMemory    = int64(64 << 20)
	defaultRetentionMaxDepth     = 256
	defaultRetentionMaxFindings  = 128

	hardRetentionMaxObjects   = 250_000
	hardRetentionMaxPathBytes = int64(256 << 20)
	hardRetentionMaxBytes     = int64(130 << 40)
	hardRetentionMaxMemory    = int64(512 << 20)
	hardRetentionMaxDepth     = 256
	hardRetentionMaxFindings  = 1_024

	// A bound directory listing retains an implementation-owned DirEntry plus
	// its platform name before the higher-level path budget can inspect it.
	// Reserve a deliberately conservative fixed amount per N+1 entry so the
	// memory limit also bounds that transient allocation, not only retained
	// retentionNode paths.
	retentionListEntryMemoryBytes = int64(2 << 10)
)

type RetentionLimits struct {
	MaxObjects     int   `json:"max_objects"`
	MaxPathBytes   int64 `json:"max_path_bytes"`
	MaxBytes       int64 `json:"max_bytes"`
	MaxMemoryBytes int64 `json:"max_memory_bytes"`
	MaxDepth       int   `json:"max_depth"`
	MaxFindings    int   `json:"max_findings"`
}

func DefaultRetentionLimits() RetentionLimits {
	return RetentionLimits{
		MaxObjects: defaultRetentionMaxObjects, MaxPathBytes: defaultRetentionMaxPathBytes,
		MaxBytes: defaultRetentionMaxBytes, MaxMemoryBytes: defaultRetentionMaxMemory,
		MaxDepth: defaultRetentionMaxDepth, MaxFindings: defaultRetentionMaxFindings,
	}
}

func (limits RetentionLimits) Validate() error {
	if limits.MaxObjects <= 0 || limits.MaxObjects > hardRetentionMaxObjects ||
		limits.MaxPathBytes <= 0 || limits.MaxPathBytes > hardRetentionMaxPathBytes ||
		limits.MaxBytes <= 0 || limits.MaxBytes > hardRetentionMaxBytes ||
		limits.MaxMemoryBytes <= 0 || limits.MaxMemoryBytes > hardRetentionMaxMemory ||
		limits.MaxDepth <= 0 || limits.MaxDepth > hardRetentionMaxDepth ||
		limits.MaxFindings <= 0 || limits.MaxFindings > hardRetentionMaxFindings {
		return fmt.Errorf("materialize retention limits are invalid")
	}
	return nil
}

type RetentionUsage struct {
	ObjectsConsidered          int   `json:"objects_considered"`
	PathBytesConsidered        int64 `json:"path_bytes_considered"`
	BytesConsidered            int64 `json:"bytes_considered"`
	MemoryBytesConsidered      int64 `json:"memory_bytes_considered"`
	DirectoryEntriesExamined   int   `json:"directory_entries_examined"`
	DirectoryNameBytesExamined int64 `json:"directory_name_bytes_examined"`
	RemovalAttempts            int   `json:"removal_attempts"`
	FilesRemoved               int   `json:"files_removed"`
	DirectoriesRemoved         int   `json:"directories_removed"`
	BytesRemoved               int64 `json:"bytes_removed"`
	AmbiguousRemovals          int   `json:"ambiguous_removals"`
}

type retentionNode struct {
	path     fsbind.Path
	identity fsbind.Identity
	kind     fsbind.ObjectKind
	size     int64
}

var retentionRemovalHook func(retentionNode, fsbind.Removal) error
var retentionInventoryDirectoryHook func([]string) error

func inventoryRetentionTrees(ctx context.Context, subtree *fsbind.Subtree, topNames []string, limits RetentionLimits) ([]retentionNode, RetentionUsage, error) {
	usage := RetentionUsage{}
	if subtree == nil || len(topNames) == 0 {
		return nil, usage, fmt.Errorf("%w: retention inventory roots are unavailable", ErrPolicy)
	}
	if err := limits.Validate(); err != nil {
		return nil, usage, err
	}
	nodes := make([]retentionNode, 0, min(limits.MaxObjects, len(topNames)*8))
	for _, name := range topNames {
		if err := ctx.Err(); err != nil {
			return nil, usage, err
		}
		if _, err := fsbind.PathFromComponents([]string{name}); err != nil {
			return nil, usage, fmt.Errorf("%w: retention inventory root is invalid", ErrPolicy)
		}
		if err := inventoryRetentionPath(ctx, subtree, []string{name}, limits, &usage, &nodes); err != nil {
			return nil, usage, err
		}
	}
	if err := subtree.Check(); err != nil {
		return nil, usage, err
	}
	return nodes, usage, nil
}

func inventoryRetentionPath(ctx context.Context, subtree *fsbind.Subtree, components []string, limits RetentionLimits, usage *RetentionUsage, nodes *[]retentionNode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(components) == 0 || len(components) > limits.MaxDepth {
		return fmt.Errorf("%w: retention namespace depth limit exceeded", ErrPolicy)
	}
	path, err := fsbind.PathFromComponents(components)
	if err != nil {
		return fmt.Errorf("%w: retention namespace path is unsafe", ErrIntegrity)
	}
	pathBytes := int64(0)
	for _, component := range components {
		if int64(len(component)) > math.MaxInt64-pathBytes {
			return fmt.Errorf("%w: retention namespace path budget overflow", ErrPolicy)
		}
		pathBytes += int64(len(component))
	}
	memoryBytes := int64(128 + len(components)*32)
	if pathBytes > math.MaxInt64-memoryBytes {
		return fmt.Errorf("%w: retention namespace memory budget overflow", ErrPolicy)
	}
	memoryBytes += pathBytes
	if usage.ObjectsConsidered >= limits.MaxObjects || pathBytes > limits.MaxPathBytes-usage.PathBytesConsidered {
		return fmt.Errorf("%w: retention namespace object or path budget exhausted", ErrPolicy)
	}
	if memoryBytes > limits.MaxMemoryBytes-usage.MemoryBytesConsidered {
		return fmt.Errorf("%w: retention namespace memory budget exhausted", ErrPolicy)
	}
	object, err := subtree.Inspect(ctx, path)
	if err != nil {
		return err
	}
	if object.Kind != fsbind.ObjectKindRegular && object.Kind != fsbind.ObjectKindDirectory || object.Identity.IsZero() || object.SizeBytes < 0 {
		return fmt.Errorf("%w: retention namespace object is unsafe", ErrIntegrity)
	}
	usage.ObjectsConsidered++
	usage.PathBytesConsidered += pathBytes
	usage.MemoryBytesConsidered += memoryBytes
	if object.Kind == fsbind.ObjectKindRegular {
		if object.SizeBytes > limits.MaxBytes-usage.BytesConsidered {
			return fmt.Errorf("%w: retention byte budget exhausted", ErrPolicy)
		}
		usage.BytesConsidered += object.SizeBytes
	}
	*nodes = append(*nodes, retentionNode{path: path, identity: object.Identity, kind: object.Kind, size: object.SizeBytes})
	if object.Kind != fsbind.ObjectKindDirectory {
		return nil
	}
	remainingObjects := limits.MaxObjects - usage.ObjectsConsidered
	remainingPaths := limits.MaxPathBytes - usage.PathBytesConsidered
	remainingMemory := limits.MaxMemoryBytes - usage.MemoryBytesConsidered
	// One extra entry is reserved for the list N+1 overflow sentinel.
	maxEntriesByMemory := remainingMemory/retentionListEntryMemoryBytes - 1
	if remainingObjects <= 0 || remainingPaths <= 0 || maxEntriesByMemory <= 0 {
		return fmt.Errorf("%w: retention directory cannot be completely inventoried", ErrPolicy)
	}
	maximum := fsbind.MaximumListLimits()
	listLimits := fsbind.ListLimits{
		MaxEntries:   min(maximum.MaxEntries, remainingObjects, int(maxEntriesByMemory)),
		MaxNameBytes: min(maximum.MaxNameBytes, remainingPaths),
	}
	listing, err := subtree.List(ctx, path, listLimits)
	if err != nil {
		return err
	}
	listingMemory := int64(listing.Used.EntriesExamined) * retentionListEntryMemoryBytes
	if listingMemory > limits.MaxMemoryBytes-usage.MemoryBytesConsidered {
		return fmt.Errorf("%w: retention directory memory budget exhausted", ErrPolicy)
	}
	usage.MemoryBytesConsidered += listingMemory
	if !listing.Complete {
		usage.DirectoryEntriesExamined += listing.Used.EntriesExamined
		usage.DirectoryNameBytesExamined += listing.Used.NameBytes
		return fmt.Errorf("%w: retention directory inventory limit exhausted", ErrPolicy)
	}
	usage.DirectoryEntriesExamined += listing.Used.EntriesExamined
	usage.DirectoryNameBytesExamined += listing.Used.NameBytes
	for _, entry := range listing.Entries {
		if entry.Kind != string(fsbind.ObjectKindRegular) && entry.Kind != string(fsbind.ObjectKindDirectory) {
			return fmt.Errorf("%w: retention namespace contains an unsafe object", ErrIntegrity)
		}
		child := make([]string, len(components)+1)
		copy(child, components)
		child[len(components)] = entry.Name
		before := len(*nodes)
		if err := inventoryRetentionPath(ctx, subtree, child, limits, usage, nodes); err != nil {
			return err
		}
		if len(*nodes) != before+1 && (*nodes)[before].kind != fsbind.ObjectKindDirectory {
			return fmt.Errorf("%w: retention namespace traversal is inconsistent", ErrIntegrity)
		}
		if (*nodes)[before].kind != fsbind.ObjectKind(entry.Kind) {
			return fmt.Errorf("%w: retention directory entry changed type", ErrIntegrity)
		}
	}
	if retentionInventoryDirectoryHook != nil {
		if err := retentionInventoryDirectoryHook(append([]string(nil), components...)); err != nil {
			return err
		}
	}
	after, err := subtree.Inspect(ctx, path)
	if err != nil {
		return classifyRetentionAuthorityError(err)
	}
	if after.Kind != fsbind.ObjectKindDirectory || !after.Identity.Equal(object.Identity) {
		return fmt.Errorf("%w: retention directory changed during inventory", ErrIntegrity)
	}
	return nil
}

func removeRetentionNodes(ctx context.Context, subtree *fsbind.Subtree, nodes []retentionNode, usage RetentionUsage) (RetentionUsage, error) {
	if subtree == nil {
		return usage, fmt.Errorf("%w: retention subtree is unavailable", ErrPolicy)
	}
	for index := len(nodes) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			return usage, err
		}
		node := nodes[index]
		usage.RemovalAttempts++
		var (
			receipt fsbind.Removal
			err     error
		)
		if node.kind == fsbind.ObjectKindRegular {
			receipt, err = subtree.RemoveRegularExact(ctx, node.path, node.identity, node.size)
		} else {
			receipt, err = subtree.RemoveEmptyDirectory(ctx, node.path, node.identity)
		}
		if receipt.Removed {
			if node.kind == fsbind.ObjectKindRegular {
				usage.FilesRemoved++
				if receipt.SizeBytes > math.MaxInt64-usage.BytesRemoved {
					return usage, fmt.Errorf("%w: retention removed-byte receipt overflow", ErrIntegrity)
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
		if retentionRemovalHook != nil {
			if hookErr := retentionRemovalHook(node, receipt); hookErr != nil {
				return usage, hookErr
			}
		}
	}
	return usage, nil
}
