package sourceretire

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

type ExecutionOperationListLimits struct {
	MaxRootEntries int   `json:"max_root_entries"`
	MaxOperations  int   `json:"max_operations"`
	MaxNameBytes   int64 `json:"max_name_bytes"`
}

func DefaultExecutionOperationListLimits() ExecutionOperationListLimits {
	return ExecutionOperationListLimits{MaxRootEntries: 10_000, MaxOperations: 256, MaxNameBytes: 16 << 20}
}

func (limits ExecutionOperationListLimits) Validate() error {
	if limits.MaxRootEntries <= 0 || limits.MaxRootEntries > 100_000 ||
		limits.MaxOperations <= 0 || limits.MaxOperations > 4_096 ||
		limits.MaxNameBytes <= 0 || limits.MaxNameBytes > 64<<20 {
		return fmt.Errorf("%w: source retirement operation list limits are invalid", ErrExecutionPolicy)
	}
	return nil
}

type ExecutionOperationSummary struct {
	ID     OperationID `json:"operation_id"`
	Status string      `json:"status"`
}

type ExecutionOperationListResult struct {
	Complete        bool                         `json:"complete"`
	Effect          string                       `json:"effect"`
	WritesPerformed int                          `json:"writes_performed"`
	Limits          ExecutionOperationListLimits `json:"limits"`
	RootUsed        fsbind.ListUsage             `json:"root_used"`
	Operations      []ExecutionOperationSummary  `json:"operations"`
	StopReason      string                       `json:"stop_reason,omitempty"`
	Warnings        []string                     `json:"warnings"`
}

// ListExecutionOperations performs one bounded, name-only target-root
// inventory. It deliberately does not inspect journals or select an operation.
func ListExecutionOperations(ctx context.Context, targetRoot string, limits ExecutionOperationListLimits) (ExecutionOperationListResult, error) {
	result := ExecutionOperationListResult{
		Effect:     "read_source_retirement_operation_names",
		Limits:     limits,
		Operations: []ExecutionOperationSummary{},
		Warnings: []string{
			"listing never selects a latest operation; resume, status, prune, and forget require an explicit full operation ID",
			"not_inspected rows do not claim journal integrity, terminal state, or current source-name absence",
		},
	}
	if err := limits.Validate(); err != nil {
		result.StopReason = "invalid_limits"
		return result, err
	}
	if targetRoot == "" {
		result.StopReason = "invalid_target_root"
		return result, fmt.Errorf("%w: target root is empty", ErrExecutionPolicy)
	}
	if err := ctx.Err(); err != nil {
		result.StopReason = "context_cancelled"
		return result, err
	}
	absolute, err := filepath.Abs(targetRoot)
	if err != nil {
		result.StopReason = "invalid_target_root"
		return result, fmt.Errorf("%w: target root is invalid", ErrExecutionPolicy)
	}
	session, _, err := fsbind.BindExisting(filepath.Clean(absolute))
	if err != nil {
		result.StopReason = "target_root_unavailable"
		return result, fmt.Errorf("%w: target root cannot be bound", ErrExecutionPolicy)
	}
	defer session.Close()

	listing, err := session.ListRoot(ctx, fsbind.ListLimits{MaxEntries: limits.MaxRootEntries, MaxNameBytes: limits.MaxNameBytes})
	result.RootUsed = listing.Used
	if err != nil {
		result.StopReason = listing.StopReason
		return result, err
	}
	if !listing.Complete {
		result.StopReason = listing.StopReason
		return result, nil
	}
	operations := make(map[OperationID]string)
	for _, entry := range listing.Entries {
		var operationID OperationID
		var status string
		switch {
		case materialize.HasSourceRetireForgetMarkerPrefix(entry.Name):
			parsed, parseErr := ParseExecutionForgetRootName(entry.Name)
			if parseErr != nil || entry.Kind != string(fsbind.ObjectKindRegular) {
				result.StopReason = "invalid_operation_entry"
				return result, nil
			}
			operationID, status = parsed, "forget_in_progress_not_inspected"
		case materialize.HasSourceRetireOperationPrefix(entry.Name):
			parsed, parseErr := ParseOperationDirectoryName(entry.Name)
			if parseErr != nil || entry.Kind != string(fsbind.ObjectKindDirectory) {
				result.StopReason = "invalid_operation_entry"
				return result, nil
			}
			operationID, status = parsed, "not_inspected"
		default:
			continue
		}
		if prior, exists := operations[operationID]; exists {
			if prior == "not_inspected" && status == "forget_in_progress_not_inspected" {
				operations[operationID] = status
			}
			continue
		}
		if len(operations) >= limits.MaxOperations {
			result.StopReason = "max_operations"
			appendExecutionOperationSummaries(&result, operations)
			return result, nil
		}
		operations[operationID] = status
	}
	appendExecutionOperationSummaries(&result, operations)
	result.Complete = true
	return result, nil
}

func appendExecutionOperationSummaries(result *ExecutionOperationListResult, operations map[OperationID]string) {
	ids := make([]OperationID, 0, len(operations))
	for id := range operations {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left].String() < ids[right].String() })
	for _, id := range ids {
		result.Operations = append(result.Operations, ExecutionOperationSummary{ID: id, Status: operations[id]})
	}
}
