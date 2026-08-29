package clientstop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

const (
	intentFileName         = "intent.json"
	completionFileName     = "completion.json"
	scratchDirectory       = "scratch"
	operationLockEntryName = ".fsbind-operation.lock"
)

type markerWriteReceipt struct {
	TemporaryCreated           bool
	TemporaryCreationUncertain bool
	BytesWritten               int64
	Publication                fsbind.Publication
	PublicationUncertain       bool
	AlreadyPresent             bool
	TemporaryRemovalAttempted  bool
	TemporaryRemoved           bool
	TemporaryRemovalUncertain  bool
}

type journalCreationReceipt struct {
	Subtree fsbind.Creation
	Mkdir   fsbind.MkdirReceipt
	Intent  markerWriteReceipt
}

type journalRecoveryReceipt struct {
	Mkdir  fsbind.MkdirReceipt
	Marker markerWriteReceipt
}

type journalState struct {
	Intent                 Intent
	IntentID               MarkerID
	Attempts               []Attempt
	AttemptIDs             []MarkerID
	Responses              map[int]Response
	ResponseIDs            map[int]MarkerID
	Completion             *Completion
	CompletionID           MarkerID
	Pending                string
	Retained               bool
	RetentionIntentDurable bool
	RetentionComplete      bool
	RetentionIntentID      RetentionMarkerID
	RetentionCompleteID    RetentionMarkerID
}

type journalHandle struct {
	session   *fsbind.Session
	subtree   *fsbind.Subtree
	state     journalState
	retention retentionState
}

type namedMarkerObservation struct {
	components []string
	raw        []byte
}

func (handle *journalHandle) Close() error {
	if handle == nil {
		return nil
	}
	var first error
	if handle.subtree != nil {
		first = handle.subtree.Close()
	}
	if handle.session != nil {
		if err := handle.session.Close(); first == nil {
			first = err
		}
	}
	return first
}

func operationDirectoryName(id OperationID) (string, error) {
	parsed, err := ParseOperationID(id.String())
	if err != nil || parsed != id {
		return "", fmt.Errorf("invalid client stop operation ID")
	}
	return materialize.ClientStopOperationDirectoryPrefix + strings.TrimPrefix(id.String(), "sha256:"), nil
}

func createJournal(ctx context.Context, targetRoot string, plan Plan, planID string) (*journalHandle, journalCreationReceipt, error) {
	var receipt journalCreationReceipt
	if err := ctx.Err(); err != nil {
		return nil, receipt, err
	}
	computed, err := PlanID(plan)
	if err != nil || computed != planID || targetRoot == "" {
		return nil, receipt, fmt.Errorf("%w: reviewed client stop plan differs", ErrPolicy)
	}
	absolute, err := filepath.Abs(targetRoot)
	if err != nil {
		return nil, receipt, fmt.Errorf("%w: target root is invalid", ErrPolicy)
	}
	session, rootInfo, err := fsbind.BindExisting(filepath.Clean(absolute))
	if err != nil {
		return nil, receipt, err
	}
	failSession := func(err error) (*journalHandle, journalCreationReceipt, error) {
		_ = session.Close()
		return nil, receipt, err
	}
	if rootInfo.Identity.String() != plan.TargetRootIdentity {
		return failSession(fmt.Errorf("%w: target root identity differs from the client stop plan", ErrIntegrity))
	}
	operationID := OperationIDForPlan(planID)
	if pending, inspectErr := inspectForgetControl(ctx, session, operationID, planID); inspectErr != nil {
		return failSession(inspectErr)
	} else if pending != nil {
		return failSession(pending)
	}
	directoryName, err := operationDirectoryName(operationID)
	if err != nil {
		return failSession(err)
	}
	if err := ctx.Err(); err != nil {
		return failSession(err)
	}
	subtree, creation, err := session.CreatePrivateSubtreeWithReceipt(directoryName)
	receipt.Subtree = creation
	if err != nil {
		return failSession(err)
	}
	handle := &journalHandle{session: session, subtree: subtree}
	fail := func(err error) (*journalHandle, journalCreationReceipt, error) {
		_ = handle.Close()
		return nil, receipt, err
	}
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	receipt.Mkdir, err = subtree.MkdirAll(ctx, scratch)
	if err != nil {
		return fail(err)
	}
	root, _ := fsbind.PathFromComponents(nil)
	if err := subtree.SyncDirectory(ctx, root); err != nil {
		return fail(err)
	}
	intent := Intent{Schema: IntentSchemaV1, OperationID: operationID, OperationRootIdentity: subtree.Identity().String(), PlanID: planID, Plan: plan}
	raw, id, err := encodeIntent(intent)
	if err != nil {
		return fail(err)
	}
	receipt.Intent, err = handle.writeMarker(ctx, intentFileName, raw)
	if err != nil {
		return fail(err)
	}
	handle.state = emptyJournalState(intent, id)
	return handle, receipt, nil
}

func openJournal(ctx context.Context, targetRoot string, operationID OperationID, recoverPending bool, recoveryPlan *Plan) (*journalHandle, journalRecoveryReceipt, error) {
	var recovery journalRecoveryReceipt
	if err := ctx.Err(); err != nil {
		return nil, recovery, err
	}
	if _, err := ParseOperationID(operationID.String()); err != nil || targetRoot == "" {
		return nil, recovery, fmt.Errorf("%w: client stop selector is invalid", ErrPolicy)
	}
	absolute, err := filepath.Abs(targetRoot)
	if err != nil {
		return nil, recovery, fmt.Errorf("%w: target root is invalid", ErrPolicy)
	}
	session, rootInfo, err := fsbind.BindExisting(filepath.Clean(absolute))
	if err != nil {
		return nil, recovery, err
	}
	failSession := func(err error) (*journalHandle, journalRecoveryReceipt, error) {
		_ = session.Close()
		return nil, recovery, err
	}
	if pending, inspectErr := inspectForgetControl(ctx, session, operationID, ""); inspectErr != nil {
		return failSession(inspectErr)
	} else if pending != nil {
		return failSession(pending)
	}
	directoryName, err := operationDirectoryName(operationID)
	if err != nil {
		return failSession(err)
	}
	subtree, err := session.OpenPrivateSubtreeObserved(directoryName)
	if errors.Is(err, fsbind.ErrNotFound) {
		return failSession(ErrOperationNotFound)
	}
	if err != nil {
		return failSession(classifyJournalError(err))
	}
	handle := &journalHandle{session: session, subtree: subtree}
	fail := func(err error) (*journalHandle, journalRecoveryReceipt, error) {
		_ = handle.Close()
		return nil, recovery, err
	}
	retention, retentionErr := loadRetentionState(ctx, handle, operationID, rootInfo.Identity)
	if retentionErr != nil {
		return fail(retentionErr)
	}
	if retention.ForgetPending {
		return fail(&forgetInProgressError{marker: retention.ForgetIntent, markerID: retention.ForgetID})
	}
	if retention.DirectoryPresent {
		limits := DefaultRetentionLimits()
		switch {
		case retention.IntentID != "":
			if _, auditErr := auditRetentionNamespace(ctx, handle, retention.Intent, limits, true,
				retention.IntentPresent, retention.CompletePresent); auditErr != nil {
				return fail(auditErr)
			}
		default:
			if _, liveErr := handle.readStateForRetention(ctx, operationID); liveErr != nil {
				return fail(liveErr)
			}
		}
		handle.retention = retention
		handle.state = retention.journalState()
		return handle, recovery, nil
	}
	state, err := handle.loadState(ctx, operationID)
	if errors.Is(err, ErrInitializationIncomplete) && recoverPending {
		recovery, err = handle.recoverInitialization(ctx, operationID, rootInfo, recoveryPlan)
		if err == nil {
			state, err = handle.loadState(ctx, operationID)
		}
	}
	if err != nil {
		return fail(err)
	}
	if state.Intent.Plan.TargetRootIdentity != rootInfo.Identity.String() || state.Intent.OperationRootIdentity != subtree.Identity().String() {
		return fail(fmt.Errorf("%w: client stop journal binding differs", ErrIntegrity))
	}
	handle.state = state
	if state.Pending != "" {
		if !recoverPending {
			return fail(ErrMarkerRecoveryRequired)
		}
		recovery.Marker, err = handle.recoverPending(ctx, state.Pending)
		if err != nil {
			return fail(err)
		}
		state, err = handle.loadState(ctx, operationID)
		if err != nil || state.Pending != "" {
			if err == nil {
				err = ErrInitializationIncomplete
			}
			return fail(err)
		}
		handle.state = state
	}
	if recoverPending {
		if err := handle.confirmDurability(ctx); err != nil {
			return fail(err)
		}
	}
	return handle, recovery, nil
}

func (handle *journalHandle) recoverInitialization(ctx context.Context, operationID OperationID, rootInfo fsbind.RootInfo, recoveryPlan *Plan) (journalRecoveryReceipt, error) {
	var receipt journalRecoveryReceipt
	root, _ := fsbind.PathFromComponents(nil)
	listing, err := handle.subtree.List(ctx, root, fsbind.ListLimits{MaxEntries: 4, MaxNameBytes: 8 << 10})
	if err != nil || !listing.Complete {
		if err == nil {
			err = fmt.Errorf("%w: client stop initialization namespace is incomplete", ErrIntegrity)
		}
		return receipt, classifyJournalError(err)
	}
	for _, entry := range listing.Entries {
		if entry.Name == operationLockEntryName && entry.Kind == string(fsbind.ObjectKindRegular) {
			continue
		}
		if entry.Name == scratchDirectory && entry.Kind == string(fsbind.ObjectKindDirectory) {
			continue
		}
		return receipt, fmt.Errorf("%w: client stop initialization contains an unexpected object", ErrIntegrity)
	}
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	if _, err := handle.subtree.Inspect(ctx, scratch); errors.Is(err, fsbind.ErrNotFound) {
		if recoveryPlan == nil {
			return receipt, ErrInitializationIncomplete
		}
		var makeErr error
		receipt.Mkdir, makeErr = handle.subtree.MkdirAll(ctx, scratch)
		if makeErr != nil {
			return receipt, makeErr
		}
		if err := handle.subtree.SyncDirectory(ctx, root); err != nil {
			return receipt, err
		}
	} else if err != nil {
		return receipt, classifyJournalError(err)
	}
	pending, err := handle.pendingName(ctx)
	if err != nil {
		return receipt, err
	}
	var intent Intent
	var raw []byte
	if pending == pendingName(intentFileName) {
		raw, err = handle.readNamedBytes(ctx, []string{scratchDirectory, pending})
		if err != nil {
			return receipt, err
		}
		intent, _, err = decodeIntent(bytes.NewReader(raw))
		if err != nil {
			return receipt, err
		}
	} else if pending == "" && recoveryPlan != nil {
		planID := mustPlanID(*recoveryPlan)
		intent = Intent{Schema: IntentSchemaV1, OperationID: operationID, OperationRootIdentity: handle.subtree.Identity().String(), PlanID: planID, Plan: *recoveryPlan}
		raw, _, err = encodeIntent(intent)
		if err != nil {
			return receipt, err
		}
	} else {
		return receipt, ErrInitializationIncomplete
	}
	if intent.OperationID != operationID || intent.OperationRootIdentity != handle.subtree.Identity().String() ||
		intent.Plan.TargetRootIdentity != rootInfo.Identity.String() {
		return receipt, fmt.Errorf("%w: client stop initialization authority differs", ErrIntegrity)
	}
	if recoveryPlan != nil {
		expectedID, planErr := PlanID(*recoveryPlan)
		if planErr != nil || intent.PlanID != expectedID || intent.Plan != *recoveryPlan {
			return receipt, fmt.Errorf("%w: client stop initialization plan differs", ErrIntegrity)
		}
	}
	receipt.Marker, err = handle.writeMarker(ctx, intentFileName, raw)
	return receipt, err
}

func mustPlanID(plan Plan) string { value, _ := PlanID(plan); return value }

func emptyJournalState(intent Intent, id MarkerID) journalState {
	return journalState{Intent: intent, IntentID: id, Attempts: []Attempt{}, AttemptIDs: []MarkerID{}, Responses: map[int]Response{}, ResponseIDs: map[int]MarkerID{}}
}

func (handle *journalHandle) loadState(ctx context.Context, operationID OperationID) (journalState, error) {
	return handle.loadStateWithRetention(ctx, operationID, false)
}

func (handle *journalHandle) readStateForRetention(ctx context.Context, operationID OperationID) (journalState, error) {
	if handle == nil || handle.subtree == nil {
		return journalState{}, fmt.Errorf("%w: client stop journal authority is unavailable", ErrIntegrity)
	}
	return handle.loadStateWithRetention(ctx, operationID, true)
}

func (handle *journalHandle) loadStateWithRetention(ctx context.Context, operationID OperationID, allowRetention bool) (journalState, error) {
	intentPath, _ := fsbind.PathFromComponents([]string{intentFileName})
	if _, err := handle.subtree.Inspect(ctx, intentPath); errors.Is(err, fsbind.ErrNotFound) {
		if validation := handle.validateUninitializedNamespace(ctx); validation != nil {
			return journalState{}, validation
		}
		return journalState{}, ErrInitializationIncomplete
	} else if err != nil {
		return journalState{}, classifyJournalError(err)
	}
	raw, err := handle.readNamedBytes(ctx, []string{intentFileName})
	if err != nil {
		return journalState{}, err
	}
	intent, intentID, err := decodeIntent(bytes.NewReader(raw))
	if err != nil || intent.OperationID != operationID {
		if err == nil {
			err = fmt.Errorf("%w: client stop intent selects another operation", ErrIntegrity)
		}
		return journalState{}, err
	}
	state := emptyJournalState(intent, intentID)
	observed := []namedMarkerObservation{{components: []string{intentFileName}, raw: raw}}
	root, _ := fsbind.PathFromComponents(nil)
	listing, err := handle.subtree.List(ctx, root, fsbind.ListLimits{MaxEntries: 16, MaxNameBytes: 8 << 10})
	if err != nil || !listing.Complete {
		if err == nil {
			err = fmt.Errorf("%w: client stop journal listing is incomplete", ErrIntegrity)
		}
		return journalState{}, classifyJournalError(err)
	}
	names := make([]string, 0, len(listing.Entries))
	for _, entry := range listing.Entries {
		switch {
		case entry.Name == operationLockEntryName && entry.Kind == string(fsbind.ObjectKindRegular):
		case entry.Name == scratchDirectory && entry.Kind == string(fsbind.ObjectKindDirectory):
		case allowRetention && entry.Name == retentionDirectoryName && entry.Kind == string(fsbind.ObjectKindDirectory):
		case entry.Name == intentFileName && entry.Kind == string(fsbind.ObjectKindRegular):
		case entry.Name == completionFileName && entry.Kind == string(fsbind.ObjectKindRegular):
			names = append(names, entry.Name)
		case isAttemptName(entry.Name) && entry.Kind == string(fsbind.ObjectKindRegular), isResponseName(entry.Name) && entry.Kind == string(fsbind.ObjectKindRegular):
			names = append(names, entry.Name)
		default:
			return journalState{}, fmt.Errorf("%w: client stop journal contains an unexpected entry", ErrIntegrity)
		}
	}
	sort.Strings(names)
	for sequence := 1; sequence <= maximumAttempts; sequence++ {
		if name := attemptFileName(sequence); containsName(names, name) {
			raw, readErr := handle.readNamedBytes(ctx, []string{name})
			if readErr != nil {
				return journalState{}, readErr
			}
			value, id, decodeErr := decodeAttempt(bytes.NewReader(raw))
			if decodeErr != nil {
				return journalState{}, decodeErr
			}
			if validateErr := validateNextAttempt(state, value); validateErr != nil {
				return journalState{}, validateErr
			}
			state.Attempts, state.AttemptIDs = append(state.Attempts, value), append(state.AttemptIDs, id)
			observed = append(observed, namedMarkerObservation{components: []string{name}, raw: raw})
		}
		if name := responseFileName(sequence); containsName(names, name) {
			if len(state.Attempts) != sequence {
				return journalState{}, fmt.Errorf("%w: client stop response has no attempt", ErrIntegrity)
			}
			raw, readErr := handle.readNamedBytes(ctx, []string{name})
			if readErr != nil {
				return journalState{}, readErr
			}
			value, id, decodeErr := decodeResponse(bytes.NewReader(raw))
			if decodeErr != nil {
				return journalState{}, decodeErr
			}
			if validateErr := validateResponseForAttempt(state, sequence, value); validateErr != nil {
				return journalState{}, validateErr
			}
			state.Responses[sequence], state.ResponseIDs[sequence] = value, id
			observed = append(observed, namedMarkerObservation{components: []string{name}, raw: raw})
		}
	}
	if containsName(names, completionFileName) {
		raw, readErr := handle.readNamedBytes(ctx, []string{completionFileName})
		if readErr != nil {
			return journalState{}, readErr
		}
		value, id, decodeErr := decodeCompletion(bytes.NewReader(raw))
		if decodeErr != nil {
			return journalState{}, decodeErr
		}
		if validateErr := validateCompletionForState(state, value); validateErr != nil {
			return journalState{}, validateErr
		}
		state.Completion, state.CompletionID = &value, id
		observed = append(observed, namedMarkerObservation{components: []string{completionFileName}, raw: raw})
	}
	state.Pending, err = handle.pendingName(ctx)
	if err != nil {
		return journalState{}, err
	}
	if err := handle.confirmReadState(ctx, listing.Entries, state.Pending, observed); err != nil {
		return journalState{}, err
	}
	return state, nil
}

func (handle *journalHandle) validateUninitializedNamespace(ctx context.Context) error {
	root, _ := fsbind.PathFromComponents(nil)
	listing, err := handle.subtree.List(ctx, root, fsbind.ListLimits{MaxEntries: 4, MaxNameBytes: 8 << 10})
	if err != nil || !listing.Complete {
		if err == nil {
			err = fmt.Errorf("%w: client stop initialization namespace is incomplete", ErrIntegrity)
		}
		return classifyJournalError(err)
	}
	for _, entry := range listing.Entries {
		if entry.Name == operationLockEntryName && entry.Kind == string(fsbind.ObjectKindRegular) || entry.Name == scratchDirectory && entry.Kind == string(fsbind.ObjectKindDirectory) {
			continue
		}
		return fmt.Errorf("%w: client stop intent is missing from a non-empty journal", ErrIntegrity)
	}
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	if _, inspectErr := handle.subtree.Inspect(ctx, scratch); errors.Is(inspectErr, fsbind.ErrNotFound) {
		return nil
	} else if inspectErr != nil {
		return classifyJournalError(inspectErr)
	}
	pending, err := handle.pendingName(ctx)
	if err != nil {
		return err
	}
	if pending != "" && pending != pendingName(intentFileName) {
		return fmt.Errorf("%w: client stop intent is missing before another marker", ErrIntegrity)
	}
	return nil
}

func (handle *journalHandle) confirmDurability(ctx context.Context) error {
	root, _ := fsbind.PathFromComponents(nil)
	if err := handle.subtree.SyncDirectory(ctx, root); err != nil {
		return classifyJournalError(err)
	}
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	return classifyJournalError(handle.subtree.CheckPaths(root, scratch))
}

func (handle *journalHandle) confirmReadState(ctx context.Context, before []fsbind.Entry, pending string, observed []namedMarkerObservation) error {
	root, _ := fsbind.PathFromComponents(nil)
	listing, err := handle.subtree.List(ctx, root, fsbind.ListLimits{MaxEntries: 16, MaxNameBytes: 8 << 10})
	if err != nil || !listing.Complete {
		if err == nil {
			err = fmt.Errorf("%w: client stop journal listing is incomplete", ErrIntegrity)
		}
		return classifyJournalError(err)
	}
	if len(before) != len(listing.Entries) {
		return fmt.Errorf("%w: client stop journal namespace changed while read", ErrIntegrity)
	}
	for index := range before {
		if before[index] != listing.Entries[index] {
			return fmt.Errorf("%w: client stop journal namespace changed while read", ErrIntegrity)
		}
	}
	afterPending, err := handle.pendingName(ctx)
	if err != nil || afterPending != pending {
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: client stop scratch namespace changed while read", ErrIntegrity)
	}
	for _, marker := range observed {
		fresh, err := handle.readNamedBytes(ctx, marker.components)
		if err != nil {
			return err
		}
		if !bytes.Equal(fresh, marker.raw) {
			return fmt.Errorf("%w: client stop marker changed across the read bracket", ErrIntegrity)
		}
	}
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	return classifyJournalError(handle.subtree.CheckPaths(root, scratch))
}

func validateNextAttempt(state journalState, attempt Attempt) error {
	sequence := len(state.Attempts) + 1
	if attempt.Validate() != nil || attempt.OperationID != state.Intent.OperationID || attempt.PlanID != state.Intent.PlanID ||
		attempt.Sequence != sequence || attempt.UseID != state.Intent.Plan.UseID || attempt.JobID != state.Intent.Plan.JobID ||
		attempt.FileLayoutID != state.Intent.Plan.FileLayoutID || attempt.FileSnapshotID != state.Intent.Plan.CompleteFileSnapshotID {
		return fmt.Errorf("%w: client stop attempt disagrees with the journal", ErrIntegrity)
	}
	if sequence > 1 && attempt.PreviousAttemptID != state.AttemptIDs[sequence-2].String() {
		return fmt.Errorf("%w: client stop attempt chain disagrees", ErrIntegrity)
	}
	if sequence > 1 {
		previous := state.Attempts[sequence-2]
		if attempt.ObservedAtStart.Before(previous.ObservedAtEnd) {
			return fmt.Errorf("%w: client stop attempt time order disagrees", ErrIntegrity)
		}
		if response, exists := state.Responses[sequence-1]; exists && attempt.ObservedAtStart.Before(response.ObservedAtEnd) {
			return fmt.Errorf("%w: client stop attempt precedes the prior response", ErrIntegrity)
		}
	}
	return nil
}

func sameJournalEntries(left, right []fsbind.Entry) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validateResponseForAttempt(state journalState, sequence int, response Response) error {
	if response.Validate() != nil || sequence <= 0 || sequence > len(state.Attempts) || response.OperationID != state.Intent.OperationID ||
		response.PlanID != state.Intent.PlanID || response.AttemptID != state.AttemptIDs[sequence-1] ||
		response.ObservedAtStart.Before(state.Attempts[sequence-1].ObservedAtEnd) {
		return fmt.Errorf("%w: client stop response disagrees with the journal", ErrIntegrity)
	}
	switch state.Intent.Plan.Driver {
	case downloader.DriverQBittorrent:
		if response.RequestID != 0 {
			return fmt.Errorf("%w: qBittorrent stop response has an invalid request ID", ErrIntegrity)
		}
	case downloader.DriverTransmission:
		if response.RequestsAttempted == 1 && response.RequestID <= 0 {
			return fmt.Errorf("%w: Transmission stop response has an invalid request ID", ErrIntegrity)
		}
	default:
		return fmt.Errorf("%w: client stop response driver is invalid", ErrIntegrity)
	}
	return nil
}

func validateCompletionForState(state journalState, completion Completion) error {
	if completion.Validate() != nil || len(state.Attempts) == 0 || completion.OperationID != state.Intent.OperationID ||
		completion.PlanID != state.Intent.PlanID ||
		completion.UseID != state.Intent.Plan.UseID || completion.JobID != state.Intent.Plan.JobID ||
		completion.FileLayoutID != state.Intent.Plan.FileLayoutID || completion.CompleteFileSnapshotID != state.Intent.Plan.CompleteFileSnapshotID ||
		completion.FinalObjectIdentity != state.Intent.Plan.FinalObjectIdentity {
		return fmt.Errorf("%w: client stop completion disagrees with the journal", ErrIntegrity)
	}
	attemptIndex := -1
	for index := range state.AttemptIDs {
		if state.AttemptIDs[index] == completion.AttemptID {
			attemptIndex = index
			break
		}
	}
	if attemptIndex < 0 {
		return fmt.Errorf("%w: client stop completion attempt is absent", ErrIntegrity)
	}
	for index := attemptIndex + 1; index < len(state.Attempts); index++ {
		response, exists := state.Responses[index+1]
		if !exists || response.RequestsAttempted != 0 {
			return fmt.Errorf("%w: client stop completion skipped a possibly effectful attempt", ErrIntegrity)
		}
	}
	response, hasResponse := state.Responses[attemptIndex+1]
	responseID := state.ResponseIDs[attemptIndex+1]
	latestBoundary := state.Attempts[len(state.Attempts)-1].ObservedAtEnd
	if latestResponse, exists := state.Responses[len(state.Attempts)]; exists && latestResponse.ObservedAtEnd.After(latestBoundary) {
		latestBoundary = latestResponse.ObservedAtEnd
	}
	if completion.ObservedAtStart.Before(latestBoundary) {
		return fmt.Errorf("%w: client stop completion time order disagrees", ErrIntegrity)
	}
	if completion.Basis == "accepted_response_then_exact_stopped" {
		if !hasResponse || !response.Complete || completion.ResponseID != responseID.String() {
			return fmt.Errorf("%w: attributed client stop completion lacks an accepted response", ErrIntegrity)
		}
	} else {
		if hasResponse {
			if response.RequestsAttempted != 1 || completion.ResponseID != responseID.String() {
				return fmt.Errorf("%w: unattributed client stop completion lacks an unknown effectful request", ErrIntegrity)
			}
		} else if completion.ResponseID != "" {
			return fmt.Errorf("%w: client stop completion response reference disagrees", ErrIntegrity)
		}
	}
	return nil
}

func (handle *journalHandle) appendAttempt(ctx context.Context, value Attempt) (markerWriteReceipt, MarkerID, error) {
	if err := validateNextAttempt(handle.state, value); err != nil || handle.state.Completion != nil {
		if err == nil {
			err = fmt.Errorf("%w: client stop is already complete", ErrPolicy)
		}
		return markerWriteReceipt{}, "", err
	}
	raw, id, err := encodeAttempt(value)
	if err != nil {
		return markerWriteReceipt{}, "", err
	}
	receipt, err := handle.writeMarker(ctx, attemptFileName(value.Sequence), raw)
	if err == nil {
		handle.state.Attempts, handle.state.AttemptIDs = append(handle.state.Attempts, value), append(handle.state.AttemptIDs, id)
	}
	return receipt, id, err
}

func (handle *journalHandle) appendResponse(ctx context.Context, sequence int, value Response) (markerWriteReceipt, MarkerID, error) {
	if err := validateResponseForAttempt(handle.state, sequence, value); err != nil {
		return markerWriteReceipt{}, "", err
	}
	if _, exists := handle.state.Responses[sequence]; exists {
		return markerWriteReceipt{}, "", fmt.Errorf("%w: client stop response already exists", ErrPolicy)
	}
	raw, id, err := encodeResponse(value)
	if err != nil {
		return markerWriteReceipt{}, "", err
	}
	receipt, err := handle.writeMarker(ctx, responseFileName(sequence), raw)
	if err == nil {
		handle.state.Responses[sequence], handle.state.ResponseIDs[sequence] = value, id
	}
	return receipt, id, err
}

func (handle *journalHandle) appendCompletion(ctx context.Context, value Completion) (markerWriteReceipt, MarkerID, error) {
	if handle.state.Completion != nil {
		return markerWriteReceipt{}, "", fmt.Errorf("%w: client stop completion already exists", ErrPolicy)
	}
	if err := validateCompletionForState(handle.state, value); err != nil {
		return markerWriteReceipt{}, "", err
	}
	raw, id, err := encodeCompletion(value)
	if err != nil {
		return markerWriteReceipt{}, "", err
	}
	receipt, err := handle.writeMarker(ctx, completionFileName, raw)
	if err == nil {
		handle.state.Completion, handle.state.CompletionID = &value, id
	}
	return receipt, id, err
}

func (handle *journalHandle) recoverPending(ctx context.Context, name string) (markerWriteReceipt, error) {
	if !isPendingName(name) {
		return markerWriteReceipt{}, fmt.Errorf("%w: client stop scratch entry is invalid", ErrIntegrity)
	}
	destination := strings.TrimSuffix(name, ".pending")
	raw, err := handle.readNamedBytes(ctx, []string{scratchDirectory, name})
	if err != nil {
		return markerWriteReceipt{}, err
	}
	destinationPath, _ := fsbind.PathFromComponents([]string{destination})
	if object, inspectErr := handle.subtree.Inspect(ctx, destinationPath); inspectErr == nil {
		if object.Kind != fsbind.ObjectKindRegular {
			return markerWriteReceipt{}, fmt.Errorf("%w: published client stop marker is unsafe", ErrIntegrity)
		}
		if _, verifyErr := handle.verifyNamedBytes(ctx, []string{destination}, raw); verifyErr != nil {
			return markerWriteReceipt{}, verifyErr
		}
		return handle.writeMarker(ctx, destination, raw)
	} else if !errors.Is(inspectErr, fsbind.ErrNotFound) {
		return markerWriteReceipt{}, classifyJournalError(inspectErr)
	}
	if err := validatePendingRaw(handle.state, destination, raw); err != nil {
		return markerWriteReceipt{}, err
	}
	return handle.writeMarker(ctx, destination, raw)
}

func validatePendingRaw(state journalState, destination string, raw []byte) error {
	switch {
	case destination == intentFileName:
		return fmt.Errorf("%w: published client stop intent is missing", ErrIntegrity)
	case isAttemptName(destination):
		value, _, err := decodeAttempt(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		return validateNextAttempt(state, value)
	case isResponseName(destination):
		value, _, err := decodeResponse(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		return validateResponseForAttempt(state, parseSequence(destination), value)
	case destination == completionFileName:
		value, _, err := decodeCompletion(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		return validateCompletionForState(state, value)
	default:
		return fmt.Errorf("%w: client stop pending marker is invalid", ErrIntegrity)
	}
}

func (handle *journalHandle) writeMarker(ctx context.Context, destination string, raw []byte) (markerWriteReceipt, error) {
	var receipt markerWriteReceipt
	if handle == nil || handle.subtree == nil || len(raw) == 0 || int64(len(raw)) > maximumMarkerBytes || !knownMarkerName(destination) {
		return receipt, fmt.Errorf("%w: client stop marker publication input is invalid", ErrIntegrity)
	}
	pending := pendingName(destination)
	pendingPath, _ := fsbind.PathFromComponents([]string{scratchDirectory, pending})
	destinationPath, _ := fsbind.PathFromComponents([]string{destination})
	if object, inspectErr := handle.subtree.Inspect(ctx, destinationPath); inspectErr == nil {
		if object.Kind != fsbind.ObjectKindRegular {
			return receipt, fmt.Errorf("%w: client stop marker destination is unsafe", ErrIntegrity)
		}
		if _, err := handle.verifyNamedBytes(ctx, []string{destination}, raw); err != nil {
			return receipt, err
		}
		receipt.AlreadyPresent = true
		if pendingObject, pendingErr := handle.subtree.Inspect(ctx, pendingPath); pendingErr == nil {
			if pendingObject.Kind != fsbind.ObjectKindRegular {
				return receipt, fmt.Errorf("%w: client stop marker staging object is unsafe", ErrIntegrity)
			}
			if _, err := handle.verifyNamedBytes(ctx, []string{scratchDirectory, pending}, raw); err != nil {
				return receipt, err
			}
			removal, removeErr := handle.subtree.RemoveRegularExact(ctx, pendingPath, pendingObject.Identity, pendingObject.SizeBytes)
			receipt.TemporaryRemovalAttempted = removal.Attempted
			receipt.TemporaryRemovalUncertain = removeErr != nil || removal.Attempted && (!removal.Removed || removal.Durability != fsbind.DurabilityConfirmed)
			if removeErr != nil {
				return receipt, removeErr
			}
			receipt.TemporaryRemoved = removal.Removed
		} else if !errors.Is(pendingErr, fsbind.ErrNotFound) {
			return receipt, classifyJournalError(pendingErr)
		}
		if err := handle.confirmMarkerNamespace(ctx); err != nil {
			receipt.PublicationUncertain = true
			return receipt, err
		}
		return receipt, nil
	} else if !errors.Is(inspectErr, fsbind.ErrNotFound) {
		return receipt, classifyJournalError(inspectErr)
	}
	pendingObject, pendingErr := handle.subtree.Inspect(ctx, pendingPath)
	if errors.Is(pendingErr, fsbind.ErrNotFound) {
		file, err := handle.subtree.CreateRegular(ctx, pendingPath)
		if err != nil {
			if object, inspectErr := handle.subtree.Inspect(ctx, pendingPath); inspectErr == nil && object.Kind == fsbind.ObjectKindRegular {
				receipt.TemporaryCreationUncertain = true
			}
			return receipt, err
		}
		receipt.TemporaryCreated = true
		written, writeErr := writeAll(ctx, file, raw)
		receipt.BytesWritten = written
		syncErr := file.Sync()
		info, infoErr := file.Info()
		closeErr := file.Close()
		if writeErr != nil {
			receipt.TemporaryCreationUncertain = true
			return receipt, writeErr
		}
		if syncErr != nil || infoErr != nil || closeErr != nil || written != int64(len(raw)) || info.SizeBytes != int64(len(raw)) {
			receipt.TemporaryCreationUncertain = true
			return receipt, fmt.Errorf("client stop marker staging could not be confirmed")
		}
		pendingObject = fsbind.ObjectInfo{Identity: info.Identity, Kind: fsbind.ObjectKindRegular, SizeBytes: info.SizeBytes}
	} else if pendingErr != nil {
		return receipt, classifyJournalError(pendingErr)
	} else if pendingObject.Kind != fsbind.ObjectKindRegular {
		return receipt, fmt.Errorf("%w: client stop marker staging object is unsafe", ErrIntegrity)
	}
	if _, err := handle.verifyNamedBytes(ctx, []string{scratchDirectory, pending}, raw); err != nil {
		if receipt.TemporaryCreated {
			receipt.TemporaryCreationUncertain = true
		}
		return receipt, err
	}
	root, _ := fsbind.PathFromComponents(nil)
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	if err := handle.subtree.CheckPaths(root, scratch); err != nil {
		return receipt, classifyJournalError(err)
	}
	publication, err := handle.subtree.CommitRegularNoReplace(ctx, pendingPath, destinationPath)
	receipt.Publication = publication
	if err != nil {
		if publication.Attempted && (publication.Published || errors.Is(err, fsbind.ErrPublicationAmbiguous)) {
			receipt.PublicationUncertain = true
		}
		if errors.Is(err, fsbind.ErrAlreadyExists) {
			if _, verifyErr := handle.verifyNamedBytes(ctx, []string{destination}, raw); verifyErr == nil {
				receipt.AlreadyPresent = true
				removal, removeErr := handle.subtree.RemoveRegularExact(ctx, pendingPath, pendingObject.Identity, pendingObject.SizeBytes)
				receipt.TemporaryRemovalAttempted = removal.Attempted
				receipt.TemporaryRemovalUncertain = removeErr != nil || removal.Attempted && (!removal.Removed || removal.Durability != fsbind.DurabilityConfirmed)
				if removeErr == nil && removal.Removed {
					receipt.TemporaryRemoved = true
				}
				if removeErr != nil {
					return receipt, removeErr
				}
				if confirmErr := handle.confirmMarkerNamespace(ctx); confirmErr != nil {
					receipt.PublicationUncertain = true
					return receipt, confirmErr
				}
				return receipt, nil
			}
		}
		return receipt, classifyJournalError(err)
	}
	if !publication.Published || !publication.SourceIdentity.Equal(pendingObject.Identity) || !publication.FinalIdentity.Equal(pendingObject.Identity) {
		receipt.PublicationUncertain = true
		return receipt, fsbind.ErrPublicationAmbiguous
	}
	if _, err := handle.verifyNamedBytes(ctx, []string{destination}, raw); err != nil {
		receipt.PublicationUncertain = true
		return receipt, err
	}
	if err := handle.subtree.SyncDirectory(ctx, root); err != nil {
		receipt.PublicationUncertain = true
		return receipt, classifyJournalError(err)
	}
	if err := handle.confirmMarkerNamespace(ctx); err != nil {
		receipt.PublicationUncertain = true
		return receipt, err
	}
	return receipt, nil
}

func (handle *journalHandle) pendingName(ctx context.Context) (string, error) {
	path, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	listing, err := handle.subtree.List(ctx, path, fsbind.ListLimits{MaxEntries: 2, MaxNameBytes: 1024})
	if err != nil || !listing.Complete {
		if err == nil {
			err = fmt.Errorf("%w: client stop scratch listing is incomplete", ErrIntegrity)
		}
		return "", classifyJournalError(err)
	}
	if len(listing.Entries) == 0 {
		return "", nil
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Kind != string(fsbind.ObjectKindRegular) || !isPendingName(listing.Entries[0].Name) {
		return "", fmt.Errorf("%w: client stop scratch contains an unexpected entry", ErrIntegrity)
	}
	return listing.Entries[0].Name, nil
}

func (handle *journalHandle) confirmMarkerNamespace(ctx context.Context) error {
	pending, err := handle.pendingName(ctx)
	if err != nil {
		return err
	}
	if pending != "" {
		return fmt.Errorf("%w: client stop scratch marker remains after publication", ErrIntegrity)
	}
	root, _ := fsbind.PathFromComponents(nil)
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	return classifyJournalError(handle.subtree.CheckPaths(root, scratch))
}

func (handle *journalHandle) readNamedBytes(ctx context.Context, components []string) ([]byte, error) {
	path, err := fsbind.PathFromComponents(components)
	if err != nil {
		return nil, fmt.Errorf("%w: client stop marker path is invalid", ErrIntegrity)
	}
	object, err := handle.subtree.Inspect(ctx, path)
	if err != nil || object.Kind != fsbind.ObjectKindRegular || object.SizeBytes <= 0 || object.SizeBytes > maximumMarkerBytes {
		if err != nil {
			return nil, classifyJournalError(err)
		}
		return nil, fmt.Errorf("%w: client stop marker object is unsafe", ErrIntegrity)
	}
	file, err := handle.subtree.OpenRegular(ctx, path)
	if err != nil {
		return nil, classifyJournalError(err)
	}
	before, beforeErr := file.Info()
	raw, readErr := io.ReadAll(io.LimitReader(file, maximumMarkerBytes+1))
	after, afterErr := file.Info()
	closeErr := file.Close()
	for _, candidate := range []error{readErr, beforeErr, afterErr, closeErr} {
		if candidate != nil {
			return nil, classifyJournalError(candidate)
		}
	}
	if int64(len(raw)) != object.SizeBytes || !before.Identity.Equal(object.Identity) || !after.Identity.Equal(object.Identity) || before.SizeBytes != after.SizeBytes {
		return nil, fmt.Errorf("%w: client stop marker changed while read", ErrIntegrity)
	}
	fresh, err := handle.subtree.Inspect(ctx, path)
	if err != nil || fresh.Kind != object.Kind || fresh.SizeBytes != object.SizeBytes || !fresh.Identity.Equal(object.Identity) {
		if err != nil {
			return nil, classifyJournalError(err)
		}
		return nil, fmt.Errorf("%w: client stop marker name changed while read", ErrIntegrity)
	}
	return raw, nil
}

func (handle *journalHandle) verifyNamedBytes(ctx context.Context, components []string, expected []byte) (fsbind.Identity, error) {
	raw, err := handle.readNamedBytes(ctx, components)
	if err != nil {
		return fsbind.Identity{}, err
	}
	if !bytes.Equal(raw, expected) {
		return fsbind.Identity{}, fmt.Errorf("%w: client stop marker bytes differ", ErrIntegrity)
	}
	path, _ := fsbind.PathFromComponents(components)
	object, err := handle.subtree.Inspect(ctx, path)
	if err != nil {
		return fsbind.Identity{}, classifyJournalError(err)
	}
	return object.Identity, nil
}

func classifyJournalError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, fsbind.ErrUnsafeObject) || errors.Is(err, fsbind.ErrBindingChanged) || errors.Is(err, fsbind.ErrCrossFilesystem) {
		return fmt.Errorf("%w: client stop journal binding is unsafe", ErrIntegrity)
	}
	return err
}

func writeAll(ctx context.Context, writer io.Writer, raw []byte) (int64, error) {
	var written int64
	for len(raw) > 0 {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		count, err := writer.Write(raw)
		written += int64(count)
		raw = raw[count:]
		if err != nil {
			return written, err
		}
		if count == 0 {
			return written, io.ErrNoProgress
		}
	}
	return written, nil
}

func attemptFileName(sequence int) string   { return fmt.Sprintf("attempt-%02d.json", sequence) }
func responseFileName(sequence int) string  { return fmt.Sprintf("response-%02d.json", sequence) }
func pendingName(destination string) string { return destination + ".pending" }
func isAttemptName(name string) bool {
	for i := 1; i <= maximumAttempts; i++ {
		if name == attemptFileName(i) {
			return true
		}
	}
	return false
}
func isResponseName(name string) bool {
	for i := 1; i <= maximumAttempts; i++ {
		if name == responseFileName(i) {
			return true
		}
	}
	return false
}
func parseSequence(name string) int {
	for i := 1; i <= maximumAttempts; i++ {
		if name == attemptFileName(i) || name == responseFileName(i) {
			return i
		}
	}
	return 0
}
func knownMarkerName(name string) bool {
	return name == intentFileName || name == completionFileName || isAttemptName(name) || isResponseName(name)
}
func isPendingName(name string) bool {
	return strings.HasSuffix(name, ".pending") && knownMarkerName(strings.TrimSuffix(name, ".pending"))
}
func containsName(names []string, wanted string) bool {
	index := sort.SearchStrings(names, wanted)
	return index < len(names) && names[index] == wanted
}
