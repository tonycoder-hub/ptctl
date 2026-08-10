package clientadopt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const (
	intentFileName     = "intent.json"
	completionFileName = "complete.json"
	scratchDirectory   = "scratch"
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
	Completion             *Completion
	CompletionID           MarkerID
	Pending                string
	Durable                bool
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

func createJournal(ctx context.Context, targetRoot string, plan Plan, planID string) (*journalHandle, journalCreationReceipt, error) {
	var receipt journalCreationReceipt
	if err := ctx.Err(); err != nil {
		return nil, receipt, err
	}
	if err := plan.Validate(); err != nil {
		return nil, receipt, err
	}
	computed, err := PlanID(plan)
	if err != nil || computed != planID {
		return nil, receipt, fmt.Errorf("%w: reviewed adoption plan differs", ErrPolicy)
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
		return failSession(fmt.Errorf("%w: target root identity differs from the reviewed plan", ErrIntegrity))
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
	intent := Intent{
		Schema: IntentSchemaV1, OperationID: operationID, OperationRootIdentity: subtree.Identity().String(),
		PlanID: planID, Plan: plan,
	}
	raw, id, err := encodeIntent(intent)
	if err != nil {
		return fail(err)
	}
	receipt.Intent, err = handle.writeMarker(ctx, intentFileName, raw, id)
	if err != nil {
		return fail(err)
	}
	handle.state = journalState{Intent: intent, IntentID: id, Attempts: []Attempt{}, AttemptIDs: []MarkerID{}, Durable: true}
	return handle, receipt, nil
}

func openJournal(ctx context.Context, targetRoot string, operationID OperationID, recoverPending bool, recoveryPlan *Plan) (*journalHandle, journalRecoveryReceipt, error) {
	var recovery journalRecoveryReceipt
	if err := ctx.Err(); err != nil {
		return nil, recovery, err
	}
	if _, err := ParseOperationID(operationID.String()); err != nil || targetRoot == "" {
		return nil, recovery, fmt.Errorf("%w: adoption operation selector is invalid", ErrPolicy)
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
		return nil, recovery, fmt.Errorf("%w: explicit adoption operation does not exist", ErrOperationNotFound)
	}
	if err != nil || object.Kind != fsbind.ObjectKindDirectory {
		_ = session.Close()
		return nil, recovery, fmt.Errorf("%w: adoption operation directory is unsafe", ErrIntegrity)
	}
	subtree, err := session.OpenPrivateSubtreeObserved(directoryName)
	if err != nil {
		_ = session.Close()
		return nil, recovery, classifyJournalError(err)
	}
	handle := &journalHandle{session: session, subtree: subtree}
	retention, retentionErr := loadRetentionState(ctx, handle, operationID, rootInfo.Identity)
	if retentionErr != nil {
		_ = handle.Close()
		return nil, recovery, retentionErr
	}
	if retention.DirectoryPresent {
		limits := DefaultRetentionLimits()
		switch {
		case retention.IntentID != "":
			// A retained completion is authority only while the operation root
			// still has the exact namespace promised by the retained intent.
			// Before the intent is durable the complete legacy journal must
			// remain; after it is durable deletion may be partially complete.
			if _, auditErr := auditRetentionNamespace(ctx, handle, retention.Intent, limits, true,
				retention.IntentPresent, retention.CompletePresent); auditErr != nil {
				_ = handle.Close()
				return nil, recovery, auditErr
			}
		case !retention.IntentPresent:
			// Creating the reserved directory is the first prune write. Until a
			// canonical intent exists, bind the boundary to an independently
			// valid live journal rather than treating an empty directory as
			// retained authority.
			if _, liveErr := handle.readStateForRetention(ctx); liveErr != nil {
				_ = handle.Close()
				return nil, recovery, liveErr
			}
		}
		handle.retention = retention
		handle.state = retention.journalState()
		return handle, recovery, nil
	}
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
		return nil, recovery, fmt.Errorf("%w: adoption journal belongs to a different target root", ErrIntegrity)
	}
	handle.state = state
	if !recoverPending {
		return handle, recovery, nil
	}
	if state.Pending == "" {
		if err := handle.confirmDurability(ctx); err != nil {
			_ = handle.Close()
			return nil, recovery, err
		}
		handle.state.Durable = true
		return handle, recovery, nil
	}
	receipt, err := handle.recoverPending(ctx, state.Pending)
	recovery.Marker = mergeMarkerWriteReceipts(recovery.Marker, receipt)
	if err != nil {
		_ = handle.Close()
		return nil, recovery, err
	}
	state, err = handle.readState(ctx)
	if err != nil {
		_ = handle.Close()
		return nil, recovery, err
	}
	if state.Intent.Plan.TargetRootIdentity != rootInfo.Identity.String() {
		_ = handle.Close()
		return nil, recovery, fmt.Errorf("%w: recovered adoption journal belongs to a different target root", ErrIntegrity)
	}
	handle.state = state
	if err := handle.confirmDurability(ctx); err != nil {
		_ = handle.Close()
		return nil, recovery, err
	}
	handle.state.Durable = true
	return handle, recovery, nil
}

func (handle *journalHandle) confirmDurability(ctx context.Context) error {
	if handle == nil || handle.subtree == nil {
		return fmt.Errorf("%w: adoption journal authority is unavailable", ErrIntegrity)
	}
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

func (handle *journalHandle) recoverInitialization(ctx context.Context, operationID OperationID, rootInfo fsbind.RootInfo, plan Plan) (journalRecoveryReceipt, error) {
	var receipt journalRecoveryReceipt
	if handle == nil || handle.subtree == nil || plan.Validate() != nil || plan.TargetRootIdentity != rootInfo.Identity.String() {
		return receipt, fmt.Errorf("%w: incomplete adoption journal cannot be rebound to the reviewed plan", ErrIntegrity)
	}
	planID, err := PlanID(plan)
	if err != nil || OperationIDForPlan(planID) != operationID {
		return receipt, fmt.Errorf("%w: incomplete adoption journal has a different deterministic selector", ErrPolicy)
	}
	root, _ := fsbind.PathFromComponents(nil)
	listing, err := handle.subtree.List(ctx, root, fsbind.ListLimits{MaxEntries: 3, MaxNameBytes: 1 << 20})
	if err != nil {
		return receipt, classifyJournalError(err)
	}
	if !listing.Complete {
		return receipt, fmt.Errorf("%w: incomplete adoption journal namespace exceeds its recovery budget", ErrIntegrity)
	}
	scratchPresent := false
	for _, entry := range listing.Entries {
		switch {
		case entry.Name == ".fsbind-operation.lock" && entry.Kind == string(fsbind.ObjectKindRegular):
		case entry.Name == scratchDirectory && entry.Kind == string(fsbind.ObjectKindDirectory):
			scratchPresent = true
		default:
			return receipt, fmt.Errorf("%w: incomplete adoption journal contains an unexpected object", ErrIntegrity)
		}
	}
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	if scratchPresent {
		scratchListing, listErr := handle.subtree.List(ctx, scratch, fsbind.ListLimits{MaxEntries: 1, MaxNameBytes: maximumMarkerBytes})
		if listErr != nil {
			return receipt, classifyJournalError(listErr)
		}
		if !scratchListing.Complete || len(scratchListing.Entries) != 0 {
			return receipt, fmt.Errorf("%w: incomplete adoption scratch namespace is not empty", ErrIntegrity)
		}
	} else {
		receipt.Mkdir, err = handle.subtree.MkdirAll(ctx, scratch)
		if err != nil {
			return receipt, err
		}
		if err := handle.subtree.SyncDirectory(ctx, root); err != nil {
			return receipt, err
		}
	}
	intent := Intent{
		Schema: IntentSchemaV1, OperationID: operationID, OperationRootIdentity: handle.subtree.Identity().String(),
		PlanID: planID, Plan: plan,
	}
	raw, id, err := encodeIntent(intent)
	if err != nil {
		return receipt, err
	}
	receipt.Marker, err = handle.writeMarker(ctx, intentFileName, raw, id)
	return receipt, err
}

func mergeMarkerWriteReceipts(left, right markerWriteReceipt) markerWriteReceipt {
	// Initialization recovery and pending-marker recovery cannot both publish
	// separate markers in one open: recovering initialization produces a
	// complete intent with no pending marker. Keep this defensive merge so any
	// future extension cannot silently drop a write receipt.
	if left == (markerWriteReceipt{}) {
		return right
	}
	if right == (markerWriteReceipt{}) {
		return left
	}
	left.TemporaryCreated = left.TemporaryCreated || right.TemporaryCreated
	left.TemporaryCreationUncertain = left.TemporaryCreationUncertain || right.TemporaryCreationUncertain
	left.BytesWritten += right.BytesWritten
	left.PublicationUncertain = true
	left.TemporaryRemovalAttempted = left.TemporaryRemovalAttempted || right.TemporaryRemovalAttempted
	left.TemporaryRemoved = left.TemporaryRemoved || right.TemporaryRemoved
	left.TemporaryRemovalUncertain = left.TemporaryRemovalUncertain || right.TemporaryRemovalUncertain
	return left
}

func (handle *journalHandle) readState(ctx context.Context) (journalState, error) {
	return handle.readStateWithRetention(ctx, false)
}

func (handle *journalHandle) readStateForRetention(ctx context.Context) (journalState, error) {
	return handle.readStateWithRetention(ctx, true)
}

func (handle *journalHandle) readStateWithRetention(ctx context.Context, allowRetention bool) (journalState, error) {
	state := journalState{Attempts: []Attempt{}, AttemptIDs: []MarkerID{}}
	observed := make([]namedMarkerObservation, 0, maximumAttempts+2)
	root, _ := fsbind.PathFromComponents(nil)
	listing, err := handle.subtree.List(ctx, root, fsbind.ListLimits{MaxEntries: maximumAttempts + 5, MaxNameBytes: 1 << 20})
	if err != nil {
		return state, classifyJournalError(err)
	}
	if !listing.Complete {
		return state, fmt.Errorf("%w: adoption operation inventory is incomplete", ErrIntegrity)
	}
	regular := make(map[string]bool)
	scratchPresent := false
	for _, entry := range listing.Entries {
		switch {
		case entry.Name == ".fsbind-operation.lock" && entry.Kind == string(fsbind.ObjectKindRegular):
		case entry.Name == scratchDirectory && entry.Kind == string(fsbind.ObjectKindDirectory):
			scratchPresent = true
		case allowRetention && entry.Name == retentionDirectoryName && entry.Kind == string(fsbind.ObjectKindDirectory):
		case entry.Kind == string(fsbind.ObjectKindRegular) && (entry.Name == intentFileName || entry.Name == completionFileName || isAttemptFileName(entry.Name)):
			regular[entry.Name] = true
		default:
			return state, fmt.Errorf("%w: adoption operation contains an unexpected object", ErrIntegrity)
		}
	}
	if !scratchPresent {
		if len(regular) == 0 {
			return state, fmt.Errorf("%w: adoption scratch and intent are not initialized", ErrInitializationIncomplete)
		}
		return state, fmt.Errorf("%w: adoption scratch directory is missing", ErrIntegrity)
	}
	pending, err := handle.readPendingName(ctx)
	if err != nil {
		return state, err
	}
	state.Pending = pending
	if !regular[intentFileName] {
		if pending == pendingName(intentFileName) {
			if len(regular) != 0 {
				return state, fmt.Errorf("%w: adoption markers exist before the intent", ErrIntegrity)
			}
			raw, readErr := handle.readNamedBytes(ctx, []string{scratchDirectory, pending})
			if readErr != nil {
				return state, readErr
			}
			intent, _, decodeErr := decodeIntent(bytes.NewReader(raw))
			if decodeErr != nil || intent.OperationID != OperationIDForPlan(intent.PlanID) || intent.OperationRootIdentity != handle.subtree.Identity().String() {
				return state, fmt.Errorf("%w: pending adoption intent authority differs", ErrIntegrity)
			}
			state.Intent = intent
			observed = append(observed, namedMarkerObservation{components: []string{scratchDirectory, pending}, raw: raw})
			if err := handle.confirmReadState(ctx, listing.Entries, pending, observed); err != nil {
				return state, err
			}
			return state, nil
		}
		if len(regular) == 0 && pending == "" {
			return state, fmt.Errorf("%w: adoption intent is not initialized", ErrInitializationIncomplete)
		}
		return state, fmt.Errorf("%w: adoption intent is missing", ErrIntegrity)
	}
	raw, err := handle.readNamedBytes(ctx, []string{intentFileName})
	if err != nil {
		return state, err
	}
	state.Intent, state.IntentID, err = decodeIntent(bytes.NewReader(raw))
	if err != nil || state.Intent.OperationID.String() == "" || state.Intent.OperationRootIdentity != handle.subtree.Identity().String() {
		return state, fmt.Errorf("%w: adoption intent authority differs", ErrIntegrity)
	}
	observed = append(observed, namedMarkerObservation{components: []string{intentFileName}, raw: raw})
	for sequence := 1; sequence <= maximumAttempts; sequence++ {
		name := attemptFileName(sequence)
		if !regular[name] {
			for later := sequence + 1; later <= maximumAttempts; later++ {
				if regular[attemptFileName(later)] {
					return state, fmt.Errorf("%w: adoption attempts are not contiguous", ErrIntegrity)
				}
			}
			break
		}
		raw, err := handle.readNamedBytes(ctx, []string{name})
		if err != nil {
			return state, err
		}
		attempt, id, err := decodeAttempt(bytes.NewReader(raw))
		if err != nil || attempt.OperationID != state.Intent.OperationID || attempt.PlanID != state.Intent.PlanID || attempt.Sequence != sequence ||
			attempt.MetafileVariantID != state.Intent.Plan.MetafileVariantID || attempt.MetafileBytes != state.Intent.Plan.MetafileBytes {
			return state, fmt.Errorf("%w: adoption attempt disagrees with the intent", ErrIntegrity)
		}
		if sequence > 1 && attempt.PreviousAttemptID != state.AttemptIDs[len(state.AttemptIDs)-1].String() {
			return state, fmt.Errorf("%w: adoption attempt chain differs", ErrIntegrity)
		}
		state.Attempts = append(state.Attempts, attempt)
		state.AttemptIDs = append(state.AttemptIDs, id)
		observed = append(observed, namedMarkerObservation{components: []string{name}, raw: raw})
	}
	if regular[completionFileName] {
		if len(state.Attempts) == 0 {
			return state, fmt.Errorf("%w: adoption completion has no request intent", ErrIntegrity)
		}
		raw, err := handle.readNamedBytes(ctx, []string{completionFileName})
		if err != nil {
			return state, err
		}
		completion, id, err := decodeCompletion(bytes.NewReader(raw))
		if err != nil || completion.OperationID != state.Intent.OperationID || completion.PlanID != state.Intent.PlanID ||
			completion.AttemptID != state.AttemptIDs[len(state.AttemptIDs)-1] || completion.ContentPathRef != state.Intent.Plan.ExpectedContentPathRef ||
			completion.FinalObjectIdentity != state.Intent.Plan.FinalObjectIdentity {
			return state, fmt.Errorf("%w: adoption completion disagrees with the intent", ErrIntegrity)
		}
		state.Completion, state.CompletionID = &completion, id
		observed = append(observed, namedMarkerObservation{components: []string{completionFileName}, raw: raw})
	}
	if err := handle.confirmReadState(ctx, listing.Entries, pending, observed); err != nil {
		return state, err
	}
	return state, nil
}

func (handle *journalHandle) confirmReadState(ctx context.Context, rootBefore []fsbind.Entry, pendingBefore string, observed []namedMarkerObservation) error {
	root, _ := fsbind.PathFromComponents(nil)
	listing, err := handle.subtree.List(ctx, root, fsbind.ListLimits{MaxEntries: maximumAttempts + 5, MaxNameBytes: 1 << 20})
	if err != nil {
		return classifyJournalError(err)
	}
	if !listing.Complete || !sameJournalEntries(rootBefore, listing.Entries) {
		return fmt.Errorf("%w: adoption operation namespace changed while read", ErrIntegrity)
	}
	pendingAfter, err := handle.readPendingName(ctx)
	if err != nil {
		return err
	}
	if pendingAfter != pendingBefore {
		return fmt.Errorf("%w: adoption scratch namespace changed while read", ErrIntegrity)
	}
	for _, marker := range observed {
		fresh, err := handle.readNamedBytes(ctx, marker.components)
		if err != nil {
			return err
		}
		if !bytes.Equal(fresh, marker.raw) {
			return fmt.Errorf("%w: adoption marker changed across the read bracket", ErrIntegrity)
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

func (handle *journalHandle) readPendingName(ctx context.Context) (string, error) {
	scratch, _ := fsbind.PathFromComponents([]string{scratchDirectory})
	listing, err := handle.subtree.List(ctx, scratch, fsbind.ListLimits{MaxEntries: 2, MaxNameBytes: 1 << 20})
	if err != nil {
		return "", classifyJournalError(err)
	}
	if !listing.Complete || len(listing.Entries) > 1 {
		return "", fmt.Errorf("%w: adoption scratch inventory is incomplete", ErrIntegrity)
	}
	if len(listing.Entries) == 0 {
		return "", nil
	}
	entry := listing.Entries[0]
	if entry.Kind != string(fsbind.ObjectKindRegular) || !isPendingName(entry.Name) {
		return "", fmt.Errorf("%w: adoption scratch contains an unexpected object", ErrIntegrity)
	}
	return entry.Name, nil
}

func (handle *journalHandle) recoverPending(ctx context.Context, name string) (markerWriteReceipt, error) {
	destination := strings.TrimSuffix(name, ".pending")
	raw, err := handle.readNamedBytes(ctx, []string{scratchDirectory, name})
	if err != nil {
		return markerWriteReceipt{}, err
	}
	var id MarkerID
	switch {
	case destination == intentFileName:
		intent, parsedID, decodeErr := decodeIntent(bytes.NewReader(raw))
		if decodeErr != nil || intent.OperationID != OperationIDForPlan(intent.PlanID) || intent.OperationRootIdentity != handle.subtree.Identity().String() {
			return markerWriteReceipt{}, fmt.Errorf("%w: pending adoption intent is invalid", ErrIntegrity)
		}
		id = parsedID
	case isAttemptFileName(destination):
		attempt, parsedID, decodeErr := decodeAttempt(bytes.NewReader(raw))
		if decodeErr != nil || attempt.OperationID != handle.state.Intent.OperationID || attempt.PlanID != handle.state.Intent.PlanID ||
			attempt.Sequence != len(handle.state.Attempts)+1 || destination != attemptFileName(attempt.Sequence) {
			return markerWriteReceipt{}, fmt.Errorf("%w: pending adoption attempt is invalid", ErrIntegrity)
		}
		if attempt.Sequence > 1 && attempt.PreviousAttemptID != handle.state.AttemptIDs[len(handle.state.AttemptIDs)-1].String() {
			return markerWriteReceipt{}, fmt.Errorf("%w: pending adoption attempt chain differs", ErrIntegrity)
		}
		id = parsedID
	case destination == completionFileName:
		completion, parsedID, decodeErr := decodeCompletion(bytes.NewReader(raw))
		if decodeErr != nil || len(handle.state.AttemptIDs) == 0 || completion.AttemptID != handle.state.AttemptIDs[len(handle.state.AttemptIDs)-1] ||
			completion.OperationID != handle.state.Intent.OperationID || completion.PlanID != handle.state.Intent.PlanID {
			return markerWriteReceipt{}, fmt.Errorf("%w: pending adoption completion is invalid", ErrIntegrity)
		}
		id = parsedID
	default:
		return markerWriteReceipt{}, fmt.Errorf("%w: pending adoption marker has an invalid destination", ErrIntegrity)
	}
	return handle.writeMarker(ctx, destination, raw, id)
}

func (handle *journalHandle) appendAttempt(ctx context.Context, assessment downloaderAssessment) (markerWriteReceipt, error) {
	sequence := len(handle.state.Attempts) + 1
	if sequence > maximumAttempts {
		return markerWriteReceipt{}, fmt.Errorf("%w: maximum explicit add attempts reached", ErrPolicy)
	}
	previous := ""
	if sequence > 1 {
		previous = handle.state.AttemptIDs[len(handle.state.AttemptIDs)-1].String()
	}
	attempt := Attempt{
		Schema: AttemptSchemaV1, OperationID: handle.state.Intent.OperationID, Sequence: sequence,
		PreviousAttemptID: previous, PlanID: handle.state.Intent.PlanID,
		ObservedAtStart: assessment.started, ObservedAtEnd: assessment.ended,
		BeforeStatus: string(assessment.status), MetafileVariantID: handle.state.Intent.Plan.MetafileVariantID,
		MetafileBytes: handle.state.Intent.Plan.MetafileBytes,
	}
	raw, id, err := encodeAttempt(attempt)
	if err != nil {
		return markerWriteReceipt{}, err
	}
	receipt, err := handle.writeMarker(ctx, attemptFileName(sequence), raw, id)
	if err != nil {
		return receipt, err
	}
	handle.state.Attempts = append(handle.state.Attempts, attempt)
	handle.state.AttemptIDs = append(handle.state.AttemptIDs, id)
	return receipt, nil
}

func (handle *journalHandle) appendCompletion(ctx context.Context, completion Completion) (markerWriteReceipt, error) {
	raw, id, err := encodeCompletion(completion)
	if err != nil {
		return markerWriteReceipt{}, err
	}
	receipt, err := handle.writeMarker(ctx, completionFileName, raw, id)
	if err != nil {
		return receipt, err
	}
	handle.state.Completion = &completion
	handle.state.CompletionID = id
	return receipt, nil
}

func (handle *journalHandle) writeMarker(ctx context.Context, destination string, raw []byte, id MarkerID) (markerWriteReceipt, error) {
	receipt := markerWriteReceipt{}
	if handle == nil || handle.subtree == nil || int64(len(raw)) <= 0 || int64(len(raw)) > maximumMarkerBytes {
		return receipt, fmt.Errorf("%w: adoption marker publication input is invalid", ErrIntegrity)
	}
	pending := pendingName(destination)
	pendingPath, _ := fsbind.PathFromComponents([]string{scratchDirectory, pending})
	destinationPath, _ := fsbind.PathFromComponents([]string{destination})
	object, inspectErr := handle.subtree.Inspect(ctx, destinationPath)
	if inspectErr == nil {
		if object.Kind != fsbind.ObjectKindRegular {
			return receipt, fmt.Errorf("%w: adoption marker destination is unsafe", ErrIntegrity)
		}
		if _, err := handle.verifyNamedBytes(ctx, []string{destination}, raw); err != nil {
			return receipt, err
		}
		receipt.AlreadyPresent = true
		if pendingObject, pendingErr := handle.subtree.Inspect(ctx, pendingPath); pendingErr == nil {
			if pendingObject.Kind != fsbind.ObjectKindRegular {
				return receipt, fmt.Errorf("%w: adoption marker staging object is unsafe", ErrIntegrity)
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
			return receipt, fmt.Errorf("adoption marker staging could not be confirmed")
		}
		pendingObject = fsbind.ObjectInfo{Identity: info.Identity, Kind: fsbind.ObjectKindRegular, SizeBytes: info.SizeBytes}
	} else if pendingErr != nil {
		return receipt, classifyJournalError(pendingErr)
	} else if pendingObject.Kind != fsbind.ObjectKindRegular {
		return receipt, fmt.Errorf("%w: adoption marker staging object is unsafe", ErrIntegrity)
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
				return receipt, removeErr
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
	if err := handle.subtree.CheckPaths(root); err != nil {
		receipt.PublicationUncertain = true
		return receipt, classifyJournalError(err)
	}
	_ = id // retained in the canonical bytes and filename-independent digest.
	return receipt, nil
}

func (handle *journalHandle) readNamedBytes(ctx context.Context, components []string) ([]byte, error) {
	path, err := fsbind.PathFromComponents(components)
	if err != nil {
		return nil, fmt.Errorf("%w: adoption marker path is invalid", ErrIntegrity)
	}
	object, err := handle.subtree.Inspect(ctx, path)
	if err != nil || object.Kind != fsbind.ObjectKindRegular || object.SizeBytes <= 0 || object.SizeBytes > maximumMarkerBytes {
		if err != nil {
			return nil, classifyJournalError(err)
		}
		return nil, fmt.Errorf("%w: adoption marker object is unsafe", ErrIntegrity)
	}
	file, err := handle.subtree.OpenRegular(ctx, path)
	if err != nil {
		return nil, classifyJournalError(err)
	}
	before, beforeErr := file.Info()
	raw, readErr := io.ReadAll(io.LimitReader(file, maximumMarkerBytes+1))
	after, afterErr := file.Info()
	closeErr := file.Close()
	if readErr != nil || beforeErr != nil || afterErr != nil || closeErr != nil {
		for _, candidate := range []error{readErr, beforeErr, afterErr, closeErr} {
			if candidate != nil {
				return nil, candidate
			}
		}
	}
	if int64(len(raw)) != object.SizeBytes || int64(len(raw)) > maximumMarkerBytes ||
		!before.Identity.Equal(object.Identity) || !after.Identity.Equal(object.Identity) || before.SizeBytes != after.SizeBytes {
		return nil, fmt.Errorf("%w: adoption marker changed while read", ErrIntegrity)
	}
	fresh, err := handle.subtree.Inspect(ctx, path)
	if err != nil || fresh.Kind != object.Kind || fresh.SizeBytes != object.SizeBytes || !fresh.Identity.Equal(object.Identity) {
		return nil, fmt.Errorf("%w: adoption marker name changed while read", ErrIntegrity)
	}
	return raw, nil
}

func (handle *journalHandle) verifyNamedBytes(ctx context.Context, components []string, expected []byte) (fsbind.Identity, error) {
	raw, err := handle.readNamedBytes(ctx, components)
	if err != nil {
		return fsbind.Identity{}, err
	}
	if !bytes.Equal(raw, expected) {
		return fsbind.Identity{}, fmt.Errorf("%w: adoption marker bytes differ", ErrIntegrity)
	}
	path, _ := fsbind.PathFromComponents(components)
	object, err := handle.subtree.Inspect(ctx, path)
	if err != nil {
		return fsbind.Identity{}, classifyJournalError(err)
	}
	return object.Identity, nil
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
		return fmt.Errorf("%w: adoption journal authority changed", ErrIntegrity)
	}
	return err
}

func attemptFileName(sequence int) string { return fmt.Sprintf("attempt-%02d.json", sequence) }

func isAttemptFileName(name string) bool {
	for sequence := 1; sequence <= maximumAttempts; sequence++ {
		if name == attemptFileName(sequence) {
			return true
		}
	}
	return false
}

func pendingName(destination string) string { return destination + ".pending" }

func isPendingName(name string) bool {
	if name == pendingName(intentFileName) || name == pendingName(completionFileName) {
		return true
	}
	for sequence := 1; sequence <= maximumAttempts; sequence++ {
		if name == pendingName(attemptFileName(sequence)) {
			return true
		}
	}
	return false
}

type downloaderAssessment struct {
	status  downloader.LedgerIdentityStatus
	started time.Time
	ended   time.Time
	result  downloader.LedgerIdentityAssessment
}
