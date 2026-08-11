package clientremove

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
	Intent       Intent
	IntentID     MarkerID
	Attempts     []Attempt
	AttemptIDs   []MarkerID
	Responses    map[int]Response
	ResponseIDs  map[int]MarkerID
	Completion   *Completion
	CompletionID MarkerID
	Pending      string
}

type journalHandle struct {
	session *fsbind.Session
	subtree *fsbind.Subtree
	state   journalState
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
		return "", fmt.Errorf("invalid client removal operation ID")
	}
	return materialize.ClientRemoveOperationDirectoryPrefix + strings.TrimPrefix(id.String(), "sha256:"), nil
}

func createJournal(ctx context.Context, targetRoot string, plan Plan, planID string) (*journalHandle, journalCreationReceipt, error) {
	var receipt journalCreationReceipt
	if err := ctx.Err(); err != nil {
		return nil, receipt, err
	}
	computed, err := PlanID(plan)
	if err != nil || computed != planID || targetRoot == "" {
		return nil, receipt, fmt.Errorf("%w: reviewed client removal plan differs", ErrPolicy)
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
		return failSession(fmt.Errorf("%w: target root identity differs from the client removal plan", ErrIntegrity))
	}
	operationID := OperationIDForPlan(planID)
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
	scratchPath, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	receipt.Mkdir, err = subtree.MkdirAll(ctx, scratchPath)
	if err != nil {
		return fail(err)
	}
	rootPath, _ := fsbind.PathFromComponents(nil)
	if err := subtree.SyncDirectory(ctx, rootPath); err != nil {
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
		return nil, recovery, fmt.Errorf("%w: client removal selector is invalid", ErrPolicy)
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
		return fail(fmt.Errorf("%w: client removal journal binding differs", ErrIntegrity))
	}
	handle.state = state
	if state.Pending != "" {
		if !recoverPending {
			return fail(ErrInitializationIncomplete)
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
	root, _ := fsbind.PathFromComponents(nil)
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	if err := subtree.CheckPaths(root, scratch); err != nil {
		return fail(classifyJournalError(err))
	}
	return handle, recovery, nil
}

func (handle *journalHandle) recoverInitialization(ctx context.Context, operationID OperationID, rootInfo fsbind.RootInfo, recoveryPlan *Plan) (journalRecoveryReceipt, error) {
	var receipt journalRecoveryReceipt
	root, _ := fsbind.PathFromComponents(nil)
	listing, err := handle.subtree.List(ctx, root, fsbind.ListLimits{MaxEntries: 4, MaxNameBytes: 8 << 10})
	if err != nil || !listing.Complete {
		if err == nil {
			err = fmt.Errorf("%w: client removal initialization namespace is incomplete", ErrIntegrity)
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
		return receipt, fmt.Errorf("%w: client removal initialization contains an unexpected object", ErrIntegrity)
	}
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	if _, inspectErr := handle.subtree.Inspect(ctx, scratch); errors.Is(inspectErr, fsbind.ErrNotFound) {
		if recoveryPlan == nil {
			return receipt, ErrInitializationIncomplete
		}
		receipt.Mkdir, err = handle.subtree.MkdirAll(ctx, scratch)
		if err != nil {
			return receipt, err
		}
		if err := handle.subtree.SyncDirectory(ctx, root); err != nil {
			return receipt, err
		}
	} else if inspectErr != nil {
		return receipt, classifyJournalError(inspectErr)
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
		planID, planErr := PlanID(*recoveryPlan)
		if planErr != nil {
			return receipt, planErr
		}
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
		return receipt, fmt.Errorf("%w: client removal initialization authority differs", ErrIntegrity)
	}
	if recoveryPlan != nil {
		expectedID, planErr := PlanID(*recoveryPlan)
		if planErr != nil || intent.PlanID != expectedID || intent.Plan != *recoveryPlan {
			return receipt, fmt.Errorf("%w: client removal initialization plan differs", ErrIntegrity)
		}
	}
	receipt.Marker, err = handle.writeMarker(ctx, intentFileName, raw)
	return receipt, err
}

func emptyJournalState(intent Intent, id MarkerID) journalState {
	return journalState{Intent: intent, IntentID: id, Attempts: []Attempt{}, AttemptIDs: []MarkerID{}, Responses: map[int]Response{}, ResponseIDs: map[int]MarkerID{}}
}

func (handle *journalHandle) loadState(ctx context.Context, operationID OperationID) (journalState, error) {
	intentPath, _ := fsbind.PathFromComponents([]string{intentFileName})
	if _, err := handle.subtree.Inspect(ctx, intentPath); errors.Is(err, fsbind.ErrNotFound) {
		if initializationErr := handle.validateUninitializedNamespace(ctx); initializationErr != nil {
			return journalState{}, initializationErr
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
			err = fmt.Errorf("%w: client removal intent selects another operation", ErrIntegrity)
		}
		return journalState{}, err
	}
	state := emptyJournalState(intent, intentID)
	observed := []namedMarkerObservation{{components: []string{intentFileName}, raw: raw}}
	root, _ := fsbind.PathFromComponents(nil)
	listing, err := handle.subtree.List(ctx, root, fsbind.ListLimits{MaxEntries: 16, MaxNameBytes: 8 << 10})
	if err != nil || !listing.Complete {
		if err == nil {
			err = fmt.Errorf("%w: client removal journal listing is incomplete", ErrIntegrity)
		}
		return journalState{}, classifyJournalError(err)
	}
	names := make([]string, 0, len(listing.Entries))
	for _, entry := range listing.Entries {
		switch {
		case entry.Name == operationLockEntryName && entry.Kind == string(fsbind.ObjectKindRegular):
		case entry.Name == scratchDirectory && entry.Kind == string(fsbind.ObjectKindDirectory):
		case entry.Name == intentFileName && entry.Kind == string(fsbind.ObjectKindRegular):
		case entry.Name == completionFileName && entry.Kind == string(fsbind.ObjectKindRegular):
			names = append(names, entry.Name)
		case isAttemptName(entry.Name) && entry.Kind == string(fsbind.ObjectKindRegular), isResponseName(entry.Name) && entry.Kind == string(fsbind.ObjectKindRegular):
			names = append(names, entry.Name)
		default:
			return journalState{}, fmt.Errorf("%w: client removal journal contains an unexpected entry", ErrIntegrity)
		}
	}
	sort.Strings(names)
	for sequence := 1; sequence <= maximumAttempts; sequence++ {
		attemptName := attemptFileName(sequence)
		if containsName(names, attemptName) {
			raw, readErr := handle.readNamedBytes(ctx, []string{attemptName})
			if readErr != nil {
				return journalState{}, readErr
			}
			attempt, id, decodeErr := decodeAttempt(bytes.NewReader(raw))
			if decodeErr != nil || validateNextAttempt(state, attempt) != nil {
				if decodeErr != nil {
					return journalState{}, decodeErr
				}
				return journalState{}, validateNextAttempt(state, attempt)
			}
			state.Attempts = append(state.Attempts, attempt)
			state.AttemptIDs = append(state.AttemptIDs, id)
			observed = append(observed, namedMarkerObservation{components: []string{attemptName}, raw: raw})
		}
		responseName := responseFileName(sequence)
		if containsName(names, responseName) {
			if len(state.Attempts) != sequence {
				return journalState{}, fmt.Errorf("%w: client removal response has no attempt", ErrIntegrity)
			}
			raw, readErr := handle.readNamedBytes(ctx, []string{responseName})
			if readErr != nil {
				return journalState{}, readErr
			}
			response, id, decodeErr := decodeResponse(bytes.NewReader(raw))
			if decodeErr != nil || validateResponseForAttempt(state, sequence, response) != nil {
				if decodeErr != nil {
					return journalState{}, decodeErr
				}
				return journalState{}, validateResponseForAttempt(state, sequence, response)
			}
			state.Responses[sequence] = response
			state.ResponseIDs[sequence] = id
			observed = append(observed, namedMarkerObservation{components: []string{responseName}, raw: raw})
		}
	}
	if containsName(names, completionFileName) {
		raw, readErr := handle.readNamedBytes(ctx, []string{completionFileName})
		if readErr != nil {
			return journalState{}, readErr
		}
		completion, id, decodeErr := decodeCompletion(bytes.NewReader(raw))
		if decodeErr != nil || validateCompletionForState(state, completion) != nil {
			if decodeErr != nil {
				return journalState{}, decodeErr
			}
			return journalState{}, validateCompletionForState(state, completion)
		}
		state.Completion, state.CompletionID = &completion, id
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

func (handle *journalHandle) confirmReadState(ctx context.Context, before []fsbind.Entry, pending string, observed []namedMarkerObservation) error {
	root, _ := fsbind.PathFromComponents(nil)
	listing, err := handle.subtree.List(ctx, root, fsbind.ListLimits{MaxEntries: 16, MaxNameBytes: 8 << 10})
	if err != nil || !listing.Complete {
		if err == nil {
			err = fmt.Errorf("%w: client removal journal listing is incomplete", ErrIntegrity)
		}
		return classifyJournalError(err)
	}
	if !sameJournalEntries(before, listing.Entries) {
		return fmt.Errorf("%w: client removal journal namespace changed while read", ErrIntegrity)
	}
	afterPending, err := handle.pendingName(ctx)
	if err != nil {
		return err
	}
	if afterPending != pending {
		return fmt.Errorf("%w: client removal scratch namespace changed while read", ErrIntegrity)
	}
	for _, marker := range observed {
		fresh, err := handle.readNamedBytes(ctx, marker.components)
		if err != nil {
			return err
		}
		if !bytes.Equal(fresh, marker.raw) {
			return fmt.Errorf("%w: client removal marker changed across the read bracket", ErrIntegrity)
		}
	}
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	if err := handle.subtree.CheckPaths(root, scratch); err != nil {
		return classifyJournalError(err)
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

func (handle *journalHandle) validateUninitializedNamespace(ctx context.Context) error {
	root, _ := fsbind.PathFromComponents(nil)
	listing, err := handle.subtree.List(ctx, root, fsbind.ListLimits{MaxEntries: 4, MaxNameBytes: 8 << 10})
	if err != nil || !listing.Complete {
		if err == nil {
			err = fmt.Errorf("%w: client removal initialization namespace is incomplete", ErrIntegrity)
		}
		return classifyJournalError(err)
	}
	for _, entry := range listing.Entries {
		if entry.Name == operationLockEntryName && entry.Kind == string(fsbind.ObjectKindRegular) {
			continue
		}
		if entry.Name == scratchDirectory && entry.Kind == string(fsbind.ObjectKindDirectory) {
			continue
		}
		return fmt.Errorf("%w: client removal intent is missing from a non-empty journal", ErrIntegrity)
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
		return fmt.Errorf("%w: client removal intent is missing before another marker", ErrIntegrity)
	}
	return nil
}

func validateNextAttempt(state journalState, attempt Attempt) error {
	sequence := len(state.Attempts) + 1
	if attempt.Validate() != nil || attempt.OperationID != state.Intent.OperationID || attempt.PlanID != state.Intent.PlanID || attempt.Sequence != sequence ||
		attempt.UseID != state.Intent.Plan.UseID || attempt.JobID != state.Intent.Plan.JobID || attempt.FileLayoutID != state.Intent.Plan.FileLayoutID ||
		attempt.FileSnapshotID != state.Intent.Plan.CompleteFileSnapshotID {
		return fmt.Errorf("%w: client removal attempt disagrees with the journal", ErrIntegrity)
	}
	if sequence > 1 && attempt.PreviousAttemptID != state.AttemptIDs[sequence-2].String() {
		return fmt.Errorf("%w: client removal attempt chain disagrees", ErrIntegrity)
	}
	return nil
}

func validateResponseForAttempt(state journalState, sequence int, response Response) error {
	if response.Validate() != nil || sequence <= 0 || sequence > len(state.Attempts) || response.OperationID != state.Intent.OperationID ||
		response.PlanID != state.Intent.PlanID || response.AttemptID != state.AttemptIDs[sequence-1] {
		return fmt.Errorf("%w: client removal response disagrees with the journal", ErrIntegrity)
	}
	switch state.Intent.Plan.Driver {
	case downloader.DriverQBittorrent:
		if response.RequestID != 0 {
			return fmt.Errorf("%w: qBittorrent removal response has an invalid request ID", ErrIntegrity)
		}
	case downloader.DriverTransmission:
		if response.RequestsAttempted == 1 && response.RequestID <= 0 {
			return fmt.Errorf("%w: Transmission removal response has an invalid request ID", ErrIntegrity)
		}
	default:
		return fmt.Errorf("%w: client removal response driver is invalid", ErrIntegrity)
	}
	return nil
}

func validateCompletionForState(state journalState, completion Completion) error {
	if completion.Validate() != nil || len(state.Attempts) == 0 || completion.OperationID != state.Intent.OperationID ||
		completion.PlanID != state.Intent.PlanID || completion.AttemptID != state.AttemptIDs[len(state.AttemptIDs)-1] ||
		completion.UseID != state.Intent.Plan.UseID || completion.JobID != state.Intent.Plan.JobID ||
		completion.FinalObjectIdentity != state.Intent.Plan.FinalObjectIdentity {
		return fmt.Errorf("%w: client removal completion disagrees with the journal", ErrIntegrity)
	}
	response, hasResponse := state.Responses[len(state.Attempts)]
	responseID := state.ResponseIDs[len(state.Attempts)]
	if completion.Basis == "accepted_response_then_exact_absence" {
		if !hasResponse || !response.Complete || completion.ResponseID != responseID.String() {
			return fmt.Errorf("%w: attributed client removal completion lacks an accepted response", ErrIntegrity)
		}
	} else if completion.ResponseID != "" && (!hasResponse || completion.ResponseID != responseID.String()) {
		return fmt.Errorf("%w: client removal completion response reference disagrees", ErrIntegrity)
	}
	return nil
}

func (handle *journalHandle) recoverPending(ctx context.Context, name string) (markerWriteReceipt, error) {
	var receipt markerWriteReceipt
	if !isPendingName(name) {
		return receipt, fmt.Errorf("%w: client removal scratch entry is invalid", ErrIntegrity)
	}
	destination := strings.TrimSuffix(name, ".pending")
	raw, err := handle.readNamedBytes(ctx, []string{scratchDirectory, name})
	if err != nil {
		return receipt, err
	}
	if err := validatePendingRaw(handle.state, destination, raw); err != nil {
		return receipt, err
	}
	return handle.writeMarker(ctx, destination, raw)
}

func validatePendingRaw(state journalState, destination string, raw []byte) error {
	switch {
	case destination == intentFileName:
		return fmt.Errorf("%w: published client removal intent is missing", ErrIntegrity)
	case isAttemptName(destination):
		value, _, err := decodeAttempt(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		return validateNextAttempt(state, value)
	case isResponseName(destination):
		sequence := parseSequence(destination)
		value, _, err := decodeResponse(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		return validateResponseForAttempt(state, sequence, value)
	case destination == completionFileName:
		value, _, err := decodeCompletion(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		return validateCompletionForState(state, value)
	default:
		return fmt.Errorf("%w: client removal pending marker is invalid", ErrIntegrity)
	}
}

func (handle *journalHandle) pendingName(ctx context.Context) (string, error) {
	path, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	listing, err := handle.subtree.List(ctx, path, fsbind.ListLimits{MaxEntries: 2, MaxNameBytes: 1024})
	if err != nil || !listing.Complete {
		if err == nil {
			err = fmt.Errorf("%w: client removal scratch listing is incomplete", ErrIntegrity)
		}
		return "", classifyJournalError(err)
	}
	if len(listing.Entries) == 0 {
		return "", nil
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Kind != string(fsbind.ObjectKindRegular) || !isPendingName(listing.Entries[0].Name) {
		return "", fmt.Errorf("%w: client removal scratch contains an unexpected entry", ErrIntegrity)
	}
	return listing.Entries[0].Name, nil
}

func (handle *journalHandle) appendAttempt(ctx context.Context, value Attempt) (markerWriteReceipt, MarkerID, error) {
	if err := validateNextAttempt(handle.state, value); err != nil || handle.state.Completion != nil {
		if err == nil {
			err = fmt.Errorf("%w: client removal is already complete", ErrPolicy)
		}
		return markerWriteReceipt{}, "", err
	}
	raw, id, err := encodeAttempt(value)
	if err != nil {
		return markerWriteReceipt{}, "", err
	}
	receipt, err := handle.writeMarker(ctx, attemptFileName(value.Sequence), raw)
	if err == nil {
		handle.state.Attempts = append(handle.state.Attempts, value)
		handle.state.AttemptIDs = append(handle.state.AttemptIDs, id)
	}
	return receipt, id, err
}

func (handle *journalHandle) appendResponse(ctx context.Context, sequence int, value Response) (markerWriteReceipt, MarkerID, error) {
	if err := validateResponseForAttempt(handle.state, sequence, value); err != nil {
		return markerWriteReceipt{}, "", err
	}
	if _, exists := handle.state.Responses[sequence]; exists {
		return markerWriteReceipt{}, "", fmt.Errorf("%w: client removal response already exists", ErrPolicy)
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
		return markerWriteReceipt{}, "", fmt.Errorf("%w: client removal completion already exists", ErrPolicy)
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

func (handle *journalHandle) writeMarker(ctx context.Context, destination string, raw []byte) (markerWriteReceipt, error) {
	receipt := markerWriteReceipt{}
	if handle == nil || handle.subtree == nil || len(raw) == 0 || int64(len(raw)) > maximumMarkerBytes || !knownMarkerName(destination) {
		return receipt, fmt.Errorf("%w: client removal marker publication input is invalid", ErrIntegrity)
	}
	pending := pendingName(destination)
	pendingPath, _ := fsbind.PathFromComponents([]string{scratchDirectory, pending})
	destinationPath, _ := fsbind.PathFromComponents([]string{destination})
	if object, inspectErr := handle.subtree.Inspect(ctx, destinationPath); inspectErr == nil {
		if object.Kind != fsbind.ObjectKindRegular {
			return receipt, fmt.Errorf("%w: client removal marker destination is unsafe", ErrIntegrity)
		}
		if _, err := handle.verifyNamedBytes(ctx, []string{destination}, raw); err != nil {
			return receipt, err
		}
		receipt.AlreadyPresent = true
		if pendingObject, pendingErr := handle.subtree.Inspect(ctx, pendingPath); pendingErr == nil {
			if pendingObject.Kind != fsbind.ObjectKindRegular {
				return receipt, fmt.Errorf("%w: client removal marker staging object is unsafe", ErrIntegrity)
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
		return receipt, handle.confirmMarkerNamespace(ctx)
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
			return receipt, fmt.Errorf("client removal marker staging could not be confirmed")
		}
		pendingObject = fsbind.ObjectInfo{Identity: info.Identity, Kind: fsbind.ObjectKindRegular, SizeBytes: info.SizeBytes}
	} else if pendingErr != nil {
		return receipt, classifyJournalError(pendingErr)
	} else if pendingObject.Kind != fsbind.ObjectKindRegular {
		return receipt, fmt.Errorf("%w: client removal marker staging object is unsafe", ErrIntegrity)
	}
	if _, err := handle.verifyNamedBytes(ctx, []string{scratchDirectory, pending}, raw); err != nil {
		if receipt.TemporaryCreated {
			receipt.TemporaryCreationUncertain = true
		}
		return receipt, err
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
				return receipt, handle.confirmMarkerNamespace(ctx)
			}
		}
		return receipt, err
	}
	if !publication.Published || !publication.SourceIdentity.Equal(pendingObject.Identity) || !publication.FinalIdentity.Equal(pendingObject.Identity) {
		receipt.PublicationUncertain = true
		return receipt, fsbind.ErrPublicationAmbiguous
	}
	if _, err := handle.verifyNamedBytes(ctx, []string{destination}, raw); err != nil {
		receipt.PublicationUncertain = true
		return receipt, err
	}
	root, _ := fsbind.PathFromComponents(nil)
	if err := handle.subtree.SyncDirectory(ctx, root); err != nil {
		receipt.PublicationUncertain = true
		return receipt, err
	}
	if err := handle.confirmMarkerNamespace(ctx); err != nil {
		receipt.PublicationUncertain = true
		return receipt, err
	}
	return receipt, nil
}

func (handle *journalHandle) confirmMarkerNamespace(ctx context.Context) error {
	pending, err := handle.pendingName(ctx)
	if err != nil {
		return err
	}
	if pending != "" {
		return fmt.Errorf("%w: client removal scratch marker remains after publication", ErrIntegrity)
	}
	root, _ := fsbind.PathFromComponents(nil)
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	if err := handle.subtree.CheckPaths(root, scratch); err != nil {
		return classifyJournalError(err)
	}
	return nil
}

func (handle *journalHandle) readNamedBytes(ctx context.Context, components []string) ([]byte, error) {
	path, err := fsbind.PathFromComponents(components)
	if err != nil {
		return nil, fmt.Errorf("%w: client removal marker path is invalid", ErrIntegrity)
	}
	object, err := handle.subtree.Inspect(ctx, path)
	if err != nil || object.Kind != fsbind.ObjectKindRegular || object.SizeBytes <= 0 || object.SizeBytes > maximumMarkerBytes {
		if err != nil {
			return nil, classifyJournalError(err)
		}
		return nil, fmt.Errorf("%w: client removal marker object is unsafe", ErrIntegrity)
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
	if int64(len(raw)) != object.SizeBytes || int64(len(raw)) > maximumMarkerBytes ||
		!before.Identity.Equal(object.Identity) || !after.Identity.Equal(object.Identity) || before.SizeBytes != after.SizeBytes {
		return nil, fmt.Errorf("%w: client removal marker changed while read", ErrIntegrity)
	}
	fresh, err := handle.subtree.Inspect(ctx, path)
	if err != nil {
		return nil, classifyJournalError(err)
	}
	if fresh.Kind != object.Kind || fresh.SizeBytes != object.SizeBytes || !fresh.Identity.Equal(object.Identity) {
		return nil, fmt.Errorf("%w: client removal marker name changed while read", ErrIntegrity)
	}
	return raw, nil
}

func (handle *journalHandle) verifyNamedBytes(ctx context.Context, components []string, expected []byte) (fsbind.Identity, error) {
	raw, err := handle.readNamedBytes(ctx, components)
	if err != nil {
		return fsbind.Identity{}, err
	}
	if !bytes.Equal(raw, expected) {
		return fsbind.Identity{}, fmt.Errorf("%w: client removal marker bytes differ", ErrIntegrity)
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
		return fmt.Errorf("%w: client removal journal binding is unsafe", ErrIntegrity)
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
	for sequence := 1; sequence <= maximumAttempts; sequence++ {
		if name == attemptFileName(sequence) {
			return true
		}
	}
	return false
}

func isResponseName(name string) bool {
	for sequence := 1; sequence <= maximumAttempts; sequence++ {
		if name == responseFileName(sequence) {
			return true
		}
	}
	return false
}

func parseSequence(name string) int {
	for sequence := 1; sequence <= maximumAttempts; sequence++ {
		if name == attemptFileName(sequence) || name == responseFileName(sequence) {
			return sequence
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
