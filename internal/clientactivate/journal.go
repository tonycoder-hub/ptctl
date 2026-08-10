package clientactivate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
)

const (
	intentFileName            = "intent.json"
	recheckStartedFileName    = "recheck-started.json"
	recheckCompletionFileName = "recheck-complete.json"
	activationCompletionName  = "activation-complete.json"
	scratchDirectory          = "scratch"
	operationLockEntryName    = ".fsbind-operation.lock"
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
	RecheckAttempts        []Attempt
	RecheckAttemptIDs      []MarkerID
	RecheckStarted         *RecheckStarted
	RecheckStartedID       MarkerID
	RecheckCompletion      *RecheckCompletion
	RecheckCompletionID    MarkerID
	StartAttempts          []Attempt
	StartAttemptIDs        []MarkerID
	ActivationCompletion   *ActivationCompletion
	ActivationCompletionID MarkerID
	Pending                string
	Durable                bool
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
		return "", fmt.Errorf("invalid client activation operation ID")
	}
	return materialize.ClientActivateOperationDirectoryPrefix + strings.TrimPrefix(id.String(), "sha256:"), nil
}

func createJournal(ctx context.Context, targetRoot string, plan Plan, planID string) (*journalHandle, journalCreationReceipt, error) {
	var receipt journalCreationReceipt
	if err := ctx.Err(); err != nil {
		return nil, receipt, err
	}
	computed, err := PlanID(plan)
	if err != nil || computed != planID || targetRoot == "" {
		return nil, receipt, fmt.Errorf("%w: reviewed activation plan differs", ErrPolicy)
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
		return failSession(fmt.Errorf("%w: target root identity differs from the activation plan", ErrIntegrity))
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
	handle.state = journalState{Intent: intent, IntentID: id, RecheckAttempts: []Attempt{}, RecheckAttemptIDs: []MarkerID{}, StartAttempts: []Attempt{}, StartAttemptIDs: []MarkerID{}, Durable: true}
	return handle, receipt, nil
}

func openJournal(ctx context.Context, targetRoot string, operationID OperationID, recoverPending bool, recoveryPlan *Plan) (*journalHandle, journalRecoveryReceipt, error) {
	var recovery journalRecoveryReceipt
	if err := ctx.Err(); err != nil {
		return nil, recovery, err
	}
	if _, err := ParseOperationID(operationID.String()); err != nil || targetRoot == "" {
		return nil, recovery, fmt.Errorf("%w: activation operation selector is invalid", ErrPolicy)
	}
	absolute, err := filepath.Abs(targetRoot)
	if err != nil {
		return nil, recovery, fmt.Errorf("%w: target root is invalid", ErrPolicy)
	}
	session, rootInfo, err := fsbind.BindExisting(filepath.Clean(absolute))
	if err != nil {
		return nil, recovery, err
	}
	directoryName, _ := operationDirectoryName(operationID)
	object, err := session.InspectRoot(ctx, directoryName)
	if errors.Is(err, fsbind.ErrNotFound) {
		_ = session.Close()
		return nil, recovery, fmt.Errorf("%w: explicit activation operation does not exist", ErrOperationNotFound)
	}
	if err != nil {
		_ = session.Close()
		return nil, recovery, classifyJournalError(err)
	}
	if object.Kind != fsbind.ObjectKindDirectory {
		_ = session.Close()
		return nil, recovery, fmt.Errorf("%w: activation operation directory is unsafe", ErrIntegrity)
	}
	subtree, err := session.OpenPrivateSubtreeObserved(directoryName)
	if err != nil {
		_ = session.Close()
		return nil, recovery, classifyJournalError(err)
	}
	handle := &journalHandle{session: session, subtree: subtree}
	state, err := handle.readState(ctx)
	if errors.Is(err, ErrInitializationIncomplete) && recoverPending && recoveryPlan != nil {
		recovery, err = handle.recoverInitialization(ctx, operationID, rootInfo, *recoveryPlan)
		if err == nil {
			state, err = handle.readState(ctx)
		}
	}
	if err != nil {
		_ = handle.Close()
		return nil, recovery, err
	}
	if state.Intent.Plan.TargetRootIdentity != rootInfo.Identity.String() {
		_ = handle.Close()
		return nil, recovery, fmt.Errorf("%w: activation journal belongs to a different target root", ErrIntegrity)
	}
	handle.state = state
	if !recoverPending {
		return handle, recovery, nil
	}
	if state.Pending != "" {
		receipt, recoverErr := handle.recoverPending(ctx, state.Pending)
		recovery.Marker = mergeMarkerReceipts(recovery.Marker, receipt)
		if recoverErr != nil {
			_ = handle.Close()
			return nil, recovery, recoverErr
		}
		state, err = handle.readState(ctx)
		if err != nil {
			_ = handle.Close()
			return nil, recovery, err
		}
		handle.state = state
	}
	if err := handle.confirmDurability(ctx); err != nil {
		_ = handle.Close()
		return nil, recovery, err
	}
	handle.state.Durable = true
	return handle, recovery, nil
}

func (handle *journalHandle) recoverInitialization(ctx context.Context, operationID OperationID, rootInfo fsbind.RootInfo, plan Plan) (journalRecoveryReceipt, error) {
	var receipt journalRecoveryReceipt
	computed, err := PlanID(plan)
	if err != nil || OperationIDForPlan(computed) != operationID || plan.TargetRootIdentity != rootInfo.Identity.String() {
		return receipt, fmt.Errorf("%w: activation initialization plan differs", ErrIntegrity)
	}
	root, _ := fsbind.PathFromComponents(nil)
	listing, err := handle.subtree.List(ctx, root, fsbind.ListLimits{MaxEntries: 3, MaxNameBytes: 1 << 20})
	if err != nil {
		return receipt, classifyJournalError(err)
	}
	if !listing.Complete {
		return receipt, fmt.Errorf("%w: activation initialization namespace is incomplete", ErrIntegrity)
	}
	for _, entry := range listing.Entries {
		if entry.Name == operationLockEntryName && entry.Kind == string(fsbind.ObjectKindRegular) {
			continue
		}
		if entry.Name != scratchDirectory || entry.Kind != string(fsbind.ObjectKindDirectory) {
			return receipt, fmt.Errorf("%w: activation initialization contains an unexpected object", ErrIntegrity)
		}
	}
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	receipt.Mkdir, err = handle.subtree.MkdirAll(ctx, scratch)
	if err != nil {
		return receipt, err
	}
	if err := handle.subtree.SyncDirectory(ctx, root); err != nil {
		return receipt, err
	}
	intent := Intent{Schema: IntentSchemaV1, OperationID: operationID, OperationRootIdentity: handle.subtree.Identity().String(), PlanID: computed, Plan: plan}
	raw, _, err := encodeIntent(intent)
	if err != nil {
		return receipt, err
	}
	receipt.Marker, err = handle.writeMarker(ctx, intentFileName, raw)
	return receipt, err
}

func (handle *journalHandle) confirmDurability(ctx context.Context) error {
	root, _ := fsbind.PathFromComponents(nil)
	if err := handle.subtree.SyncDirectory(ctx, root); err != nil {
		return classifyJournalError(err)
	}
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	if err := handle.subtree.CheckPaths(root, scratch); err != nil {
		return classifyJournalError(err)
	}
	return nil
}

func (handle *journalHandle) readState(ctx context.Context) (journalState, error) {
	state := journalState{RecheckAttempts: []Attempt{}, RecheckAttemptIDs: []MarkerID{}, StartAttempts: []Attempt{}, StartAttemptIDs: []MarkerID{}}
	root, _ := fsbind.PathFromComponents(nil)
	listing, err := handle.subtree.List(ctx, root, fsbind.ListLimits{MaxEntries: 2*maximumActionAttempts + 8, MaxNameBytes: 1 << 20})
	if err != nil {
		return state, classifyJournalError(err)
	}
	if !listing.Complete {
		return state, fmt.Errorf("%w: activation journal inventory is incomplete", ErrIntegrity)
	}
	regular := make(map[string]bool)
	for _, entry := range listing.Entries {
		if entry.Name == operationLockEntryName && entry.Kind == string(fsbind.ObjectKindRegular) {
			continue
		}
		if entry.Name == scratchDirectory && entry.Kind == string(fsbind.ObjectKindDirectory) {
			continue
		}
		if entry.Kind != string(fsbind.ObjectKindRegular) || !knownMarkerName(entry.Name) {
			return state, fmt.Errorf("%w: activation journal contains an unexpected object", ErrIntegrity)
		}
		regular[entry.Name] = true
	}
	pending, err := handle.readPendingName(ctx)
	if err != nil {
		return state, err
	}
	state.Pending = pending
	if !regular[intentFileName] {
		if len(regular) == 0 && (pending == "" || pending == pendingName(intentFileName)) {
			return state, fmt.Errorf("%w: activation intent is not initialized", ErrInitializationIncomplete)
		}
		return state, fmt.Errorf("%w: activation intent is missing", ErrIntegrity)
	}
	observed := []namedMarkerObservation{}
	raw, err := handle.readNamedBytes(ctx, []string{intentFileName})
	if err != nil {
		return state, err
	}
	state.Intent, state.IntentID, err = decodeIntent(bytes.NewReader(raw))
	if err != nil || state.Intent.OperationRootIdentity != handle.subtree.Identity().String() {
		return state, fmt.Errorf("%w: activation intent authority differs", ErrIntegrity)
	}
	observed = append(observed, namedMarkerObservation{components: []string{intentFileName}, raw: raw})
	for sequence := 1; sequence <= maximumActionAttempts; sequence++ {
		name := attemptFileName(AttemptActionRecheck, sequence)
		if !regular[name] {
			if laterAttemptExists(regular, AttemptActionRecheck, sequence+1) {
				return state, fmt.Errorf("%w: recheck attempts are not contiguous", ErrIntegrity)
			}
			break
		}
		attempt, id, markerRaw, readErr := handle.readAttempt(ctx, name)
		if readErr != nil {
			return state, readErr
		}
		if attempt.Action != AttemptActionRecheck || attempt.OperationID != state.Intent.OperationID || attempt.PlanID != state.Intent.PlanID || attempt.Sequence != sequence ||
			attempt.PrerequisiteID != state.Intent.Plan.AdoptionCompletionID || attempt.JobID != state.Intent.Plan.JobID || attempt.FileLayoutID != state.Intent.Plan.ExpectedFileLayoutID {
			return state, fmt.Errorf("%w: recheck attempt disagrees with the intent", ErrIntegrity)
		}
		if sequence > 1 && attempt.PreviousAttemptID != state.RecheckAttemptIDs[len(state.RecheckAttemptIDs)-1].String() {
			return state, fmt.Errorf("%w: recheck attempt chain differs", ErrIntegrity)
		}
		if sequence > 1 && attempt.ObservedAtStart.Before(state.RecheckAttempts[len(state.RecheckAttempts)-1].ObservedAtEnd) {
			return state, fmt.Errorf("%w: recheck attempt observations are not ordered", ErrIntegrity)
		}
		state.RecheckAttempts = append(state.RecheckAttempts, attempt)
		state.RecheckAttemptIDs = append(state.RecheckAttemptIDs, id)
		observed = append(observed, namedMarkerObservation{components: []string{name}, raw: markerRaw})
	}
	if regular[recheckStartedFileName] {
		if len(state.RecheckAttemptIDs) == 0 {
			return state, fmt.Errorf("%w: recheck-started marker has no request", ErrIntegrity)
		}
		raw, err = handle.readNamedBytes(ctx, []string{recheckStartedFileName})
		if err != nil {
			return state, err
		}
		value, id, decodeErr := decodeRecheckStarted(bytes.NewReader(raw))
		if decodeErr != nil {
			return state, decodeErr
		}
		if value.OperationID != state.Intent.OperationID || value.PlanID != state.Intent.PlanID ||
			value.AttemptID != state.RecheckAttemptIDs[len(state.RecheckAttemptIDs)-1] || value.JobID != state.Intent.Plan.JobID ||
			value.FileLayoutID != state.Intent.Plan.ExpectedFileLayoutID || value.FinalObjectIdentity != state.Intent.Plan.FinalObjectIdentity ||
			value.ObservedAtStart.Before(state.RecheckAttempts[len(state.RecheckAttempts)-1].ObservedAtEnd) {
			return state, fmt.Errorf("%w: recheck-started marker disagrees with the intent", ErrIntegrity)
		}
		state.RecheckStarted, state.RecheckStartedID = &value, id
		observed = append(observed, namedMarkerObservation{components: []string{recheckStartedFileName}, raw: raw})
	}
	if regular[recheckCompletionFileName] {
		if len(state.RecheckAttemptIDs) == 0 {
			return state, fmt.Errorf("%w: recheck completion has no request", ErrIntegrity)
		}
		raw, err = handle.readNamedBytes(ctx, []string{recheckCompletionFileName})
		if err != nil {
			return state, err
		}
		value, id, decodeErr := decodeRecheckCompletion(bytes.NewReader(raw))
		if decodeErr != nil {
			return state, decodeErr
		}
		if value.OperationID != state.Intent.OperationID || value.PlanID != state.Intent.PlanID ||
			value.AttemptID != state.RecheckAttemptIDs[len(state.RecheckAttemptIDs)-1] || value.JobID != state.Intent.Plan.JobID ||
			value.FileLayoutID != state.Intent.Plan.ExpectedFileLayoutID || value.FinalObjectIdentity != state.Intent.Plan.FinalObjectIdentity {
			return state, fmt.Errorf("%w: recheck completion disagrees with the intent", ErrIntegrity)
		}
		if value.StartedID != "" && (state.RecheckStarted == nil || value.StartedID != state.RecheckStartedID.String()) {
			return state, fmt.Errorf("%w: recheck completion started observation differs", ErrIntegrity)
		}
		previousEnd := state.RecheckAttempts[len(state.RecheckAttempts)-1].ObservedAtEnd
		if value.Basis == "durable_checking_observation_then_complete" {
			if state.RecheckStarted == nil {
				return state, fmt.Errorf("%w: recheck completion lacks its checking observation", ErrIntegrity)
			}
			previousEnd = state.RecheckStarted.ObservedAtEnd
		}
		if value.ObservedAtStart.Before(previousEnd) {
			return state, fmt.Errorf("%w: recheck completion observations are not ordered", ErrIntegrity)
		}
		state.RecheckCompletion, state.RecheckCompletionID = &value, id
		observed = append(observed, namedMarkerObservation{components: []string{recheckCompletionFileName}, raw: raw})
	}
	for sequence := 1; sequence <= maximumActionAttempts; sequence++ {
		name := attemptFileName(AttemptActionStart, sequence)
		if !regular[name] {
			if laterAttemptExists(regular, AttemptActionStart, sequence+1) {
				return state, fmt.Errorf("%w: start attempts are not contiguous", ErrIntegrity)
			}
			break
		}
		if state.Intent.Plan.Action != ActionRecheckThenStart || state.RecheckCompletion == nil {
			return state, fmt.Errorf("%w: start attempt has no reviewed recheck completion", ErrIntegrity)
		}
		attempt, id, markerRaw, readErr := handle.readAttempt(ctx, name)
		if readErr != nil {
			return state, readErr
		}
		if attempt.Action != AttemptActionStart || attempt.OperationID != state.Intent.OperationID || attempt.PlanID != state.Intent.PlanID || attempt.Sequence != sequence ||
			attempt.PrerequisiteID != state.RecheckCompletionID.String() || attempt.JobID != state.Intent.Plan.JobID || attempt.FileLayoutID != state.Intent.Plan.ExpectedFileLayoutID {
			return state, fmt.Errorf("%w: start attempt disagrees with the intent", ErrIntegrity)
		}
		if sequence > 1 && attempt.PreviousAttemptID != state.StartAttemptIDs[len(state.StartAttemptIDs)-1].String() {
			return state, fmt.Errorf("%w: start attempt chain differs", ErrIntegrity)
		}
		previousEnd := state.RecheckCompletion.ObservedAtEnd
		reusedCompletionObservation := sequence == 1 &&
			attempt.ObservedAtStart.Equal(state.RecheckCompletion.ObservedAtStart) &&
			attempt.ObservedAtEnd.Equal(state.RecheckCompletion.ObservedAtEnd)
		if sequence > 1 {
			previousEnd = state.StartAttempts[len(state.StartAttempts)-1].ObservedAtEnd
		}
		if !reusedCompletionObservation && attempt.ObservedAtStart.Before(previousEnd) {
			return state, fmt.Errorf("%w: start attempt observations are not ordered", ErrIntegrity)
		}
		state.StartAttempts = append(state.StartAttempts, attempt)
		state.StartAttemptIDs = append(state.StartAttemptIDs, id)
		observed = append(observed, namedMarkerObservation{components: []string{name}, raw: markerRaw})
	}
	if regular[activationCompletionName] {
		if len(state.StartAttemptIDs) == 0 || state.RecheckCompletion == nil {
			return state, fmt.Errorf("%w: activation completion has no start request", ErrIntegrity)
		}
		raw, err = handle.readNamedBytes(ctx, []string{activationCompletionName})
		if err != nil {
			return state, err
		}
		value, id, decodeErr := decodeActivationCompletion(bytes.NewReader(raw))
		if decodeErr != nil {
			return state, decodeErr
		}
		if value.OperationID != state.Intent.OperationID || value.PlanID != state.Intent.PlanID ||
			value.RecheckCompletionID != state.RecheckCompletionID || value.StartAttemptID != state.StartAttemptIDs[len(state.StartAttemptIDs)-1] ||
			value.JobID != state.Intent.Plan.JobID || value.FileLayoutID != state.Intent.Plan.ExpectedFileLayoutID ||
			value.FinalObjectIdentity != state.Intent.Plan.FinalObjectIdentity ||
			value.ObservedAtStart.Before(state.StartAttempts[len(state.StartAttempts)-1].ObservedAtEnd) {
			return state, fmt.Errorf("%w: activation completion disagrees with the intent", ErrIntegrity)
		}
		state.ActivationCompletion, state.ActivationCompletionID = &value, id
		observed = append(observed, namedMarkerObservation{components: []string{activationCompletionName}, raw: raw})
	}
	if state.RecheckStarted != nil && len(state.RecheckAttemptIDs) > 0 && state.RecheckStarted.AttemptID != state.RecheckAttemptIDs[len(state.RecheckAttemptIDs)-1] {
		return state, fmt.Errorf("%w: recheck attempts continued after checking was observed", ErrIntegrity)
	}
	if err := handle.confirmReadState(ctx, listing.Entries, pending, observed); err != nil {
		return state, err
	}
	return state, nil
}

func (handle *journalHandle) readAttempt(ctx context.Context, name string) (Attempt, MarkerID, []byte, error) {
	raw, err := handle.readNamedBytes(ctx, []string{name})
	if err != nil {
		return Attempt{}, "", nil, err
	}
	value, id, err := decodeAttempt(bytes.NewReader(raw))
	return value, id, raw, err
}

func knownMarkerName(name string) bool {
	if name == intentFileName || name == recheckStartedFileName || name == recheckCompletionFileName || name == activationCompletionName {
		return true
	}
	for sequence := 1; sequence <= maximumActionAttempts; sequence++ {
		if name == attemptFileName(AttemptActionRecheck, sequence) || name == attemptFileName(AttemptActionStart, sequence) {
			return true
		}
	}
	return false
}

func laterAttemptExists(regular map[string]bool, action string, from int) bool {
	for sequence := from; sequence <= maximumActionAttempts; sequence++ {
		if regular[attemptFileName(action, sequence)] {
			return true
		}
	}
	return false
}

func (handle *journalHandle) confirmReadState(ctx context.Context, before []fsbind.Entry, pending string, observed []namedMarkerObservation) error {
	root, _ := fsbind.PathFromComponents(nil)
	listing, err := handle.subtree.List(ctx, root, fsbind.ListLimits{MaxEntries: 2*maximumActionAttempts + 8, MaxNameBytes: 1 << 20})
	if err != nil {
		return classifyJournalError(err)
	}
	if !listing.Complete || !sameEntries(before, listing.Entries) {
		return fmt.Errorf("%w: activation namespace changed while read", ErrIntegrity)
	}
	afterPending, err := handle.readPendingName(ctx)
	if err != nil {
		return err
	}
	if afterPending != pending {
		return fmt.Errorf("%w: activation scratch namespace changed while read", ErrIntegrity)
	}
	for _, marker := range observed {
		fresh, err := handle.readNamedBytes(ctx, marker.components)
		if err != nil {
			return err
		}
		if !bytes.Equal(fresh, marker.raw) {
			return fmt.Errorf("%w: activation marker changed across the read bracket", ErrIntegrity)
		}
	}
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	if err := handle.subtree.CheckPaths(root, scratch); err != nil {
		return classifyJournalError(err)
	}
	return nil
}

func sameEntries(left, right []fsbind.Entry) bool {
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

func (handle *journalHandle) readPendingName(ctx context.Context) (string, error) {
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	listing, err := handle.subtree.List(ctx, scratch, fsbind.ListLimits{MaxEntries: 2, MaxNameBytes: 1 << 20})
	if errors.Is(err, fsbind.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", classifyJournalError(err)
	}
	if !listing.Complete || len(listing.Entries) > 1 {
		return "", fmt.Errorf("%w: activation scratch inventory is incomplete", ErrIntegrity)
	}
	if len(listing.Entries) == 0 {
		return "", nil
	}
	entry := listing.Entries[0]
	if entry.Kind != string(fsbind.ObjectKindRegular) || !isPendingName(entry.Name) {
		return "", fmt.Errorf("%w: activation scratch contains an unexpected object", ErrIntegrity)
	}
	return entry.Name, nil
}

func (handle *journalHandle) recoverPending(ctx context.Context, name string) (markerWriteReceipt, error) {
	destination := strings.TrimSuffix(name, ".pending")
	raw, err := handle.readNamedBytes(ctx, []string{scratchDirectory, name})
	if err != nil {
		return markerWriteReceipt{}, err
	}
	if err := handle.validatePendingDestination(destination, raw); err != nil {
		return markerWriteReceipt{}, err
	}
	return handle.writeMarker(ctx, destination, raw)
}

func (handle *journalHandle) validatePendingDestination(destination string, raw []byte) error {
	state := handle.state
	switch {
	case destination == intentFileName:
		value, _, err := decodeIntent(bytes.NewReader(raw))
		if err != nil || value.OperationID != OperationIDForPlan(value.PlanID) || value.OperationRootIdentity != handle.subtree.Identity().String() {
			return fmt.Errorf("%w: pending activation intent is invalid", ErrIntegrity)
		}
	case strings.HasPrefix(destination, "recheck-attempt-"):
		value, _, err := decodeAttempt(bytes.NewReader(raw))
		if err != nil || value.Action != AttemptActionRecheck || value.Sequence != len(state.RecheckAttempts)+1 || destination != attemptFileName(value.Action, value.Sequence) || value.PlanID != state.Intent.PlanID {
			return fmt.Errorf("%w: pending recheck attempt is invalid", ErrIntegrity)
		}
	case destination == recheckStartedFileName:
		value, _, err := decodeRecheckStarted(bytes.NewReader(raw))
		if err != nil || len(state.RecheckAttemptIDs) == 0 || value.AttemptID != state.RecheckAttemptIDs[len(state.RecheckAttemptIDs)-1] {
			return fmt.Errorf("%w: pending recheck-started marker is invalid", ErrIntegrity)
		}
	case destination == recheckCompletionFileName:
		value, _, err := decodeRecheckCompletion(bytes.NewReader(raw))
		if err != nil || len(state.RecheckAttemptIDs) == 0 || value.AttemptID != state.RecheckAttemptIDs[len(state.RecheckAttemptIDs)-1] {
			return fmt.Errorf("%w: pending recheck completion is invalid", ErrIntegrity)
		}
	case strings.HasPrefix(destination, "start-attempt-"):
		value, _, err := decodeAttempt(bytes.NewReader(raw))
		if err != nil || state.RecheckCompletion == nil || value.Action != AttemptActionStart || value.Sequence != len(state.StartAttempts)+1 || destination != attemptFileName(value.Action, value.Sequence) {
			return fmt.Errorf("%w: pending start attempt is invalid", ErrIntegrity)
		}
	case destination == activationCompletionName:
		value, _, err := decodeActivationCompletion(bytes.NewReader(raw))
		if err != nil || state.RecheckCompletion == nil || len(state.StartAttemptIDs) == 0 || value.StartAttemptID != state.StartAttemptIDs[len(state.StartAttemptIDs)-1] {
			return fmt.Errorf("%w: pending activation completion is invalid", ErrIntegrity)
		}
	default:
		return fmt.Errorf("%w: pending activation marker destination is invalid", ErrIntegrity)
	}
	return nil
}

func (handle *journalHandle) appendAttempt(ctx context.Context, action string, observed clientObservation) (markerWriteReceipt, MarkerID, error) {
	attempts, ids := handle.state.RecheckAttempts, handle.state.RecheckAttemptIDs
	prerequisite := handle.state.Intent.Plan.AdoptionCompletionID
	if action == AttemptActionStart {
		attempts, ids = handle.state.StartAttempts, handle.state.StartAttemptIDs
		if handle.state.RecheckCompletion == nil {
			return markerWriteReceipt{}, "", fmt.Errorf("%w: start has no recheck completion", ErrPolicy)
		}
		prerequisite = handle.state.RecheckCompletionID.String()
	}
	sequence := len(attempts) + 1
	if sequence > maximumActionAttempts {
		return markerWriteReceipt{}, "", fmt.Errorf("%w: maximum explicit %s attempts reached", ErrPolicy, action)
	}
	if len(attempts) > 0 && observed.ledger.ObservedAtStart.Before(attempts[len(attempts)-1].ObservedAtEnd) {
		return markerWriteReceipt{}, "", fmt.Errorf("%w: activation attempt observations are not ordered", ErrIntegrity)
	}
	if action == AttemptActionStart && len(attempts) == 0 {
		reusedCompletionObservation := observed.ledger.ObservedAtStart.Equal(handle.state.RecheckCompletion.ObservedAtStart) &&
			observed.ledger.ObservedAtEnd.Equal(handle.state.RecheckCompletion.ObservedAtEnd)
		if !reusedCompletionObservation && observed.ledger.ObservedAtStart.Before(handle.state.RecheckCompletion.ObservedAtEnd) {
			return markerWriteReceipt{}, "", fmt.Errorf("%w: start observation predates recheck completion", ErrIntegrity)
		}
	}
	previous := ""
	if len(ids) > 0 {
		previous = ids[len(ids)-1].String()
	}
	attempt := Attempt{
		Schema: AttemptSchemaV1, OperationID: handle.state.Intent.OperationID, Action: action, Sequence: sequence,
		PreviousAttemptID: previous, PlanID: handle.state.Intent.PlanID, PrerequisiteID: prerequisite,
		ObservedAtStart: observed.ledger.ObservedAtStart, ObservedAtEnd: observed.ledger.ObservedAtEnd,
		JobID: observed.jobID, BeforeState: observed.job.State, BeforeProgress: observed.job.Progress, FileLayoutID: observed.fileLayoutID,
	}
	raw, id, err := encodeAttempt(attempt)
	if err != nil {
		return markerWriteReceipt{}, "", err
	}
	receipt, err := handle.writeMarker(ctx, attemptFileName(action, sequence), raw)
	if err != nil {
		return receipt, "", err
	}
	if action == AttemptActionStart {
		handle.state.StartAttempts = append(handle.state.StartAttempts, attempt)
		handle.state.StartAttemptIDs = append(handle.state.StartAttemptIDs, id)
	} else {
		handle.state.RecheckAttempts = append(handle.state.RecheckAttempts, attempt)
		handle.state.RecheckAttemptIDs = append(handle.state.RecheckAttemptIDs, id)
	}
	return receipt, id, nil
}

func (handle *journalHandle) appendRecheckStarted(ctx context.Context, value RecheckStarted) (markerWriteReceipt, MarkerID, error) {
	raw, id, err := encodeRecheckStarted(value)
	if err != nil {
		return markerWriteReceipt{}, "", err
	}
	receipt, err := handle.writeMarker(ctx, recheckStartedFileName, raw)
	if err == nil {
		handle.state.RecheckStarted, handle.state.RecheckStartedID = &value, id
	}
	return receipt, id, err
}

func (handle *journalHandle) appendRecheckCompletion(ctx context.Context, value RecheckCompletion) (markerWriteReceipt, MarkerID, error) {
	raw, id, err := encodeRecheckCompletion(value)
	if err != nil {
		return markerWriteReceipt{}, "", err
	}
	receipt, err := handle.writeMarker(ctx, recheckCompletionFileName, raw)
	if err == nil {
		handle.state.RecheckCompletion, handle.state.RecheckCompletionID = &value, id
	}
	return receipt, id, err
}

func (handle *journalHandle) appendActivationCompletion(ctx context.Context, value ActivationCompletion) (markerWriteReceipt, MarkerID, error) {
	raw, id, err := encodeActivationCompletion(value)
	if err != nil {
		return markerWriteReceipt{}, "", err
	}
	receipt, err := handle.writeMarker(ctx, activationCompletionName, raw)
	if err == nil {
		handle.state.ActivationCompletion, handle.state.ActivationCompletionID = &value, id
	}
	return receipt, id, err
}

func (handle *journalHandle) writeMarker(ctx context.Context, destination string, raw []byte) (markerWriteReceipt, error) {
	receipt := markerWriteReceipt{}
	if handle == nil || handle.subtree == nil || int64(len(raw)) <= 0 || int64(len(raw)) > maximumMarkerBytes || !knownMarkerName(destination) {
		return receipt, fmt.Errorf("%w: activation marker publication input is invalid", ErrIntegrity)
	}
	pending := pendingName(destination)
	pendingPath, _ := fsbind.PathFromComponents([]string{scratchDirectory, pending})
	destinationPath, _ := fsbind.PathFromComponents([]string{destination})
	object, inspectErr := handle.subtree.Inspect(ctx, destinationPath)
	if inspectErr == nil {
		if object.Kind != fsbind.ObjectKindRegular {
			return receipt, fmt.Errorf("%w: activation marker destination is unsafe", ErrIntegrity)
		}
		if _, err := handle.verifyNamedBytes(ctx, []string{destination}, raw); err != nil {
			return receipt, err
		}
		receipt.AlreadyPresent = true
		if pendingObject, pendingErr := handle.subtree.Inspect(ctx, pendingPath); pendingErr == nil {
			if pendingObject.Kind != fsbind.ObjectKindRegular {
				return receipt, fmt.Errorf("%w: activation marker staging object is unsafe", ErrIntegrity)
			}
			if _, err := handle.verifyNamedBytes(ctx, []string{scratchDirectory, pending}, raw); err != nil {
				return receipt, err
			}
			removal, removeErr := handle.subtree.RemoveRegularExact(ctx, pendingPath, pendingObject.Identity, pendingObject.SizeBytes)
			receipt.TemporaryRemovalAttempted = removal.Attempted
			receipt.TemporaryRemovalUncertain = removeErr != nil || (removal.Attempted && (!removal.Removed || removal.Durability != "confirmed"))
			if removeErr != nil {
				return receipt, removeErr
			}
			receipt.TemporaryRemoved = removal.Removed
		} else if !errors.Is(pendingErr, fsbind.ErrNotFound) {
			return receipt, classifyJournalError(pendingErr)
		}
		if err := handle.confirmMarkerNamespace(ctx); err != nil {
			return receipt, err
		}
		return receipt, nil
	}
	if !errors.Is(inspectErr, fsbind.ErrNotFound) {
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
			return receipt, writeErr
		}
		if syncErr != nil || infoErr != nil || closeErr != nil || written != int64(len(raw)) || info.SizeBytes != int64(len(raw)) {
			return receipt, fmt.Errorf("activation marker staging could not be confirmed")
		}
		pendingObject = fsbind.ObjectInfo{Identity: info.Identity, Kind: fsbind.ObjectKindRegular, SizeBytes: info.SizeBytes}
	} else if pendingErr != nil {
		return receipt, classifyJournalError(pendingErr)
	} else if pendingObject.Kind != fsbind.ObjectKindRegular {
		return receipt, fmt.Errorf("%w: activation marker staging object is unsafe", ErrIntegrity)
	}
	if _, err := handle.verifyNamedBytes(ctx, []string{scratchDirectory, pending}, raw); err != nil {
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
				receipt.TemporaryRemovalUncertain = removeErr != nil || (removal.Attempted && (!removal.Removed || removal.Durability != "confirmed"))
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
	pending, err := handle.readPendingName(ctx)
	if err != nil {
		return err
	}
	if pending != "" {
		return fmt.Errorf("%w: activation scratch marker remains after publication", ErrIntegrity)
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
		return nil, fmt.Errorf("%w: activation marker path is invalid", ErrIntegrity)
	}
	object, err := handle.subtree.Inspect(ctx, path)
	if err != nil || object.Kind != fsbind.ObjectKindRegular || object.SizeBytes <= 0 || object.SizeBytes > maximumMarkerBytes {
		if err != nil {
			return nil, classifyJournalError(err)
		}
		return nil, fmt.Errorf("%w: activation marker object is unsafe", ErrIntegrity)
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
		return nil, fmt.Errorf("%w: activation marker changed while read", ErrIntegrity)
	}
	fresh, err := handle.subtree.Inspect(ctx, path)
	if err != nil {
		return nil, classifyJournalError(err)
	}
	if fresh.Kind != object.Kind || fresh.SizeBytes != object.SizeBytes || !fresh.Identity.Equal(object.Identity) {
		return nil, fmt.Errorf("%w: activation marker name changed while read", ErrIntegrity)
	}
	return raw, nil
}

func (handle *journalHandle) verifyNamedBytes(ctx context.Context, components []string, expected []byte) (fsbind.Identity, error) {
	raw, err := handle.readNamedBytes(ctx, components)
	if err != nil {
		return fsbind.Identity{}, err
	}
	if !bytes.Equal(raw, expected) {
		return fsbind.Identity{}, fmt.Errorf("%w: activation marker bytes differ", ErrIntegrity)
	}
	path, _ := fsbind.PathFromComponents(components)
	object, err := handle.subtree.Inspect(ctx, path)
	if err != nil {
		return fsbind.Identity{}, classifyJournalError(err)
	}
	return object.Identity, nil
}

func (handle *journalHandle) readPendingMarker(ctx context.Context, name string) ([]byte, error) {
	return handle.readNamedBytes(ctx, []string{scratchDirectory, name})
}

func attemptFileName(action string, sequence int) string {
	return fmt.Sprintf("%s-attempt-%02d.json", action, sequence)
}
func pendingName(destination string) string { return destination + ".pending" }

func isPendingName(name string) bool {
	if name == pendingName(intentFileName) || name == pendingName(recheckStartedFileName) || name == pendingName(recheckCompletionFileName) || name == pendingName(activationCompletionName) {
		return true
	}
	for sequence := 1; sequence <= maximumActionAttempts; sequence++ {
		if name == pendingName(attemptFileName(AttemptActionRecheck, sequence)) || name == pendingName(attemptFileName(AttemptActionStart, sequence)) {
			return true
		}
	}
	return false
}

func mergeMarkerReceipts(left, right markerWriteReceipt) markerWriteReceipt {
	return markerWriteReceipt{
		TemporaryCreated:           left.TemporaryCreated || right.TemporaryCreated,
		TemporaryCreationUncertain: left.TemporaryCreationUncertain || right.TemporaryCreationUncertain,
		BytesWritten:               left.BytesWritten + right.BytesWritten,
		Publication: func() fsbind.Publication {
			if right.Publication.Attempted {
				return right.Publication
			}
			return left.Publication
		}(),
		PublicationUncertain:      left.PublicationUncertain || right.PublicationUncertain,
		AlreadyPresent:            left.AlreadyPresent || right.AlreadyPresent,
		TemporaryRemovalAttempted: left.TemporaryRemovalAttempted || right.TemporaryRemovalAttempted,
		TemporaryRemoved:          left.TemporaryRemoved || right.TemporaryRemoved,
		TemporaryRemovalUncertain: left.TemporaryRemovalUncertain || right.TemporaryRemovalUncertain,
	}
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
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func classifyJournalError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, fsbind.ErrBusy) {
		return err
	}
	if errors.Is(err, fsbind.ErrNotFound) || errors.Is(err, fsbind.ErrUnsafeObject) || errors.Is(err, fsbind.ErrBindingChanged) || errors.Is(err, fsbind.ErrCrossFilesystem) {
		return fmt.Errorf("%w: activation journal authority changed", ErrIntegrity)
	}
	return err
}
