package materialize

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

type ControlOptions struct {
	TargetRoot  string
	OperationID OperationID
	Limits      Limits
}

const abandonRetainsBytesWarning = "staged and scratch bytes were retained; abandon performs no deletion"

type OperationListLimits struct {
	MaxRootEntries int   `json:"max_root_entries"`
	MaxOperations  int   `json:"max_operations"`
	MaxNameBytes   int64 `json:"max_name_bytes"`
}

func DefaultOperationListLimits() OperationListLimits {
	return OperationListLimits{MaxRootEntries: 10_000, MaxOperations: 256, MaxNameBytes: 16 << 20}
}

func (limits OperationListLimits) Validate() error {
	if limits.MaxRootEntries <= 0 || limits.MaxRootEntries > 100_000 ||
		limits.MaxOperations <= 0 || limits.MaxOperations > 4_096 ||
		limits.MaxNameBytes <= 0 || limits.MaxNameBytes > 64<<20 {
		return fmt.Errorf("materialize operation list limits are invalid")
	}
	return nil
}

type OperationSummary struct {
	ID     OperationID `json:"operation_id"`
	Status string      `json:"status"`
}

type OperationListResult struct {
	Complete   bool                `json:"complete"`
	Effect     string              `json:"effect"`
	Writes     int                 `json:"writes_performed"`
	Limits     OperationListLimits `json:"limits"`
	RootUsed   fsbind.ListUsage    `json:"root_used"`
	Operations []OperationSummary  `json:"operations"`
	StopReason string              `json:"stop_reason,omitempty"`
	Warnings   []string            `json:"warnings"`
}

func Status(ctx context.Context, options ControlOptions) (Report, error) {
	report := newReport("", "", options.Limits)
	report.Effect = []string{"read_private_materialize_journal"}
	if err := options.Limits.Validate(); err != nil {
		report.Outcome = OutcomeBlocked
		return report, fmt.Errorf("%w: invalid limits", ErrPolicy)
	}
	if _, err := ParseOperationID(options.OperationID.String()); err != nil {
		report.Outcome = OutcomeBlocked
		return report, fmt.Errorf("%w: invalid operation ID", ErrPolicy)
	}
	if options.TargetRoot == "" {
		report.Outcome = OutcomeBlocked
		return report, fmt.Errorf("%w: target root is empty", ErrPolicy)
	}
	setOperationInspectionSelector(&report, options.OperationID)
	report.Source = SourceReport{Mode: "not_requested", Outcome: "not_requested"}
	targetRoot, err := filepath.Abs(options.TargetRoot)
	if err != nil {
		report.Outcome = OutcomeBlocked
		return report, fmt.Errorf("%w: invalid target root", ErrPolicy)
	}
	session, rootInfo, err := fsbind.BindExisting(filepath.Clean(targetRoot))
	if err != nil {
		report.Outcome = OutcomeBlocked
		return report, fmt.Errorf("%w: bind target root: %v", ErrPolicy, err)
	}
	defer session.Close()
	handle, err := openJournal(ctx, session, options.OperationID, options.Limits)
	if err != nil {
		classifyJournalOpenReport(&report, err)
		return report, err
	}
	defer handle.subtree.Close()
	report = reportFromJournal(handle, options.Limits)
	report.Effect = []string{"read_private_materialize_journal"}
	report.Target.RootIdentityBound = true
	report.Target.ExpectedRootIdentity = handle.intent.TargetRootIdentity
	report.Target.ObservedRootIdentity = rootInfo.Identity.String()
	report.Target.SameFilesystemStage = handle.state.StageContainerIdentity != ""
	report.Target.NoClobberCapable = true
	report.Target.StabilityAssurance = "historical_journal_only"
	setHistoricalPublicationFromPhase(&report, handle.state.Phase)
	switch handle.state.Phase {
	case PhaseCommitted:
		report.Outcome = OutcomeAlreadyCommitted
		report.Operation.Status = "terminal"
		report.Operation.Resumable = false
		report.Target.Publication = "historical_commit_only"
	case PhaseAbandoned:
		report.Outcome = OutcomeAbandoned
		report.Operation.Status = "terminal"
		report.Operation.Resumable = false
		report.Warnings = append(report.Warnings, abandonRetainsBytesWarning)
	default:
		if report.Operation.Status == "scratch_capacity_blocked" {
			report.Outcome = OutcomeBlocked
		} else {
			report.Outcome = OutcomeActive
		}
	}
	return report, nil
}

func Abandon(ctx context.Context, options ControlOptions) (Report, error) {
	report := newReport("", "", options.Limits)
	if err := options.Limits.Validate(); err != nil {
		report.Outcome = OutcomeBlocked
		return report, fmt.Errorf("%w: invalid limits", ErrPolicy)
	}
	if _, err := ParseOperationID(options.OperationID.String()); err != nil {
		report.Outcome = OutcomeBlocked
		return report, fmt.Errorf("%w: invalid operation ID", ErrPolicy)
	}
	if options.TargetRoot == "" {
		report.Outcome = OutcomeBlocked
		return report, fmt.Errorf("%w: target root is empty", ErrPolicy)
	}
	setOperationInspectionSelector(&report, options.OperationID)
	report.Source = SourceReport{Mode: "not_requested", Outcome: "not_requested"}
	targetRoot, err := filepath.Abs(options.TargetRoot)
	if err != nil {
		report.Outcome = OutcomeBlocked
		return report, fmt.Errorf("%w: invalid target root", ErrPolicy)
	}
	session, rootInfo, err := fsbind.BindExisting(filepath.Clean(targetRoot))
	if err != nil {
		report.Outcome = OutcomeBlocked
		return report, fmt.Errorf("%w: bind target root: %v", ErrPolicy, err)
	}
	defer session.Close()
	handle, err := openJournal(ctx, session, options.OperationID, options.Limits)
	if err != nil {
		classifyJournalOpenReport(&report, err)
		return report, err
	}
	defer handle.subtree.Close()
	report = reportFromJournal(handle, options.Limits)
	report.Effect = []string{"write_private_materialize_journal"}
	report.Target.RootIdentityBound = true
	report.Target.ExpectedRootIdentity = handle.intent.TargetRootIdentity
	report.Target.ObservedRootIdentity = rootInfo.Identity.String()
	report.Target.SameFilesystemStage = handle.state.StageContainerIdentity != ""
	report.Target.NoClobberCapable = true
	setHistoricalPublicationFromPhase(&report, handle.state.Phase)
	if handle.state.Phase == PhaseAbandoned {
		report.Outcome = OutcomeAbandoned
		report.Operation.Status = "terminal"
		report.Operation.Resumable = false
		report.Warnings = append(report.Warnings, abandonRetainsBytesWarning)
		return report, nil
	}
	if handle.state.Phase == PhaseCommitted || handle.state.Phase == PhasePublishIntent ||
		handle.state.Phase == PhasePublished || handle.state.Phase == PhaseFinalVerified {
		report.Outcome = OutcomeBlocked
		report.addBlocker("operation.cannot_abandon", "published or publication-uncertain operations cannot be abandoned")
		return report, fmt.Errorf("%w: operation phase cannot be abandoned", ErrPolicy)
	}
	if scratchCapacityDefinitelyExhausted(handle) {
		err := scratchCapacityError("retained scratch prevents the abandon journal transition")
		markScratchCapacityBlocked(&report)
		report.Outcome = OutcomeBlocked
		return report, err
	}
	if err := handle.confirmReplayDurability(ctx); err != nil {
		report.addIssue("journal.durability_unconfirmed", "the visible journal could not be durably reconfirmed before abandon", nil)
		classifyExecutionError(&report, err)
		return report, err
	}
	report.Writes.JournalDurabilityConfirms++
	receipt, err := handle.append(ctx, Event{Phase: PhaseAbandoned, Bytes: handle.state.BytesStaged})
	report.recordJournalWrite(receipt, true)
	report.Used.ScratchEntries = handle.scratchEntries
	report.Used.ScratchBytes = handle.scratchBytes
	report.Operation.PhaseAfter = string(handle.state.Phase)
	if err != nil {
		classifyExecutionError(&report, err)
		return report, err
	}
	report.Outcome = OutcomeAbandoned
	report.Operation.Status = "terminal"
	report.Operation.Resumable = false
	report.Warnings = append(report.Warnings, abandonRetainsBytesWarning)
	return report, nil
}

func ListOperations(ctx context.Context, targetRoot string, limits OperationListLimits) (OperationListResult, error) {
	result := OperationListResult{
		Effect: "read_target_operation_names", Limits: limits,
		Operations: []OperationSummary{}, Warnings: []string{
			"listing never selects a latest operation; resume, status, and abandon require an explicit full operation ID",
		},
	}
	if err := limits.Validate(); err != nil {
		return result, err
	}
	if targetRoot == "" {
		result.Complete = false
		result.StopReason = "invalid_target_root"
		return result, fmt.Errorf("%w: target root is empty", ErrPolicy)
	}
	absolute, err := filepath.Abs(targetRoot)
	if err != nil {
		return result, fmt.Errorf("%w: invalid target root", ErrPolicy)
	}
	session, _, err := fsbind.BindExisting(filepath.Clean(absolute))
	if err != nil {
		return result, fmt.Errorf("%w: bind target root: %v", ErrPolicy, err)
	}
	defer session.Close()
	listing, err := session.ListRoot(ctx, fsbind.ListLimits{
		MaxEntries: limits.MaxRootEntries, MaxNameBytes: limits.MaxNameBytes,
	})
	result.RootUsed = listing.Used
	if err != nil {
		return result, err
	}
	if !listing.Complete {
		result.StopReason = listing.StopReason
		return result, nil
	}
	for _, entry := range listing.Entries {
		if !hasOperationDirectoryPrefix(entry.Name) {
			continue
		}
		operationID, valid := operationIDFromDirectoryEntry(entry.Name)
		if !valid || entry.Kind != "directory" {
			result.StopReason = "invalid_operation_entry"
			return result, nil
		}
		if len(result.Operations) >= limits.MaxOperations {
			result.StopReason = "max_operations"
			return result, nil
		}
		result.Operations = append(result.Operations, OperationSummary{ID: operationID, Status: "not_inspected"})
	}
	result.Complete = true
	return result, nil
}

func reportFromJournal(handle *journal, limits Limits) Report {
	report := newReport(handle.intent.MetafileVariantID, "", limits)
	report.Plan.ObservedID = handle.intent.PlanID
	report.Plan.Matches = false
	report.Operation = OperationReport{
		ID: handle.state.OperationID.String(), Status: "active",
		PhaseBefore: string(handle.state.Phase), PhaseAfter: string(handle.state.Phase),
		Resumable: !handle.state.Terminal,
	}
	report.Source = SourceReport{Mode: "not_requested", Outcome: "not_requested"}
	report.Used.ScratchEntries = handle.scratchEntries
	report.Used.ScratchBytes = handle.scratchBytes
	if !handle.state.Terminal && scratchCapacityDefinitelyExhausted(handle) {
		markScratchCapacityBlocked(&report)
	}
	return report
}

func isControlPolicyError(err error) bool {
	return errors.Is(err, ErrPolicy) || errors.Is(err, fsbind.ErrUnsupported)
}
