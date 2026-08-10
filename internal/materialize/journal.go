package materialize

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

const operationLockEntryName = ".fsbind-operation.lock"

var journalScratchCreatedHook func(string) error
var journalScratchMutationHook func(context.Context, *journal, string, fsbind.Path, []byte) error
var journalReplayReadHook func(string, int) error
var journalScratchObserveHook func(string) error

type journalWriteReceipt struct {
	ScratchCreated      bool
	ScratchBytesWritten int64
	Attempted           bool
	Ambiguous           bool
	Published           bool
	Durability          string
	Identity            string
}

type journalCreationReceipt struct {
	SubtreeCreated      bool
	SubtreeDurability   string
	DirectoriesCreated  int
	DirectoryDurability string
	DurableReplayPhase  Phase
	Objects             []journalWriteReceipt
}

type journal struct {
	intent          Intent
	state           ReplayState
	session         *fsbind.Session
	subtree         *fsbind.Subtree
	scratchEntries  int
	scratchBytes    int64
	scratchObserved bool
}

func createJournal(ctx context.Context, session *fsbind.Session, intent Intent) (*journal, journalCreationReceipt, error) {
	var creation journalCreationReceipt
	if session == nil {
		return nil, creation, fmt.Errorf("target root session is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, creation, err
	}
	if err := intent.Validate(); err != nil {
		return nil, creation, err
	}
	operationID, err := OperationIDFor(intent)
	if err != nil {
		return nil, creation, err
	}
	directoryName, err := OperationDirectoryName(operationID)
	if err != nil {
		return nil, creation, err
	}
	if err := ctx.Err(); err != nil {
		return nil, creation, err
	}
	subtree, subtreeReceipt, err := session.CreatePrivateSubtreeWithReceipt(directoryName)
	creation.SubtreeCreated = subtreeReceipt.Created
	creation.SubtreeDurability = subtreeReceipt.Durability
	if err != nil {
		return nil, creation, err
	}
	handle := &journal{
		intent: intent, session: session, subtree: subtree,
		state:           ReplayState{OperationID: operationID, LastSequence: -1, StagedFiles: []StagedFileState{}},
		scratchObserved: true,
	}
	fail := func(err error) (*journal, journalCreationReceipt, error) {
		_ = subtree.Close()
		return nil, creation, err
	}
	for _, directory := range []string{journalDirectoryName, scratchDirectoryName, stageDirectoryName} {
		path, pathErr := fsbind.PathFromComponents([]string{directory})
		if pathErr != nil {
			return fail(pathErr)
		}
		mkdirReceipt, mkdirErr := subtree.MkdirAll(ctx, path)
		creation.DirectoriesCreated += mkdirReceipt.DirectoriesCreated
		if mkdirReceipt.Durability != "" {
			creation.DirectoryDurability = mkdirReceipt.Durability
		}
		if mkdirErr != nil {
			return fail(mkdirErr)
		}
	}
	rootPath, _ := fsbind.PathFromComponents(nil)
	if err := subtree.SyncDirectory(ctx, rootPath); err != nil {
		return fail(err)
	}
	encodedIntent, err := EncodeIntent(intent)
	if err != nil {
		return fail(err)
	}
	intentDestination, _ := fsbind.PathFromComponents([]string{intentFileName})
	receipt, err := handle.commitBytes(ctx, "intent", intentDestination, encodedIntent)
	creation.Objects = append(creation.Objects, receipt)
	if err != nil {
		return fail(err)
	}
	initial := Event{Phase: PhaseJournaled, ObjectIdentity: subtree.Identity().String()}
	receipt, err = handle.append(ctx, initial)
	creation.Objects = append(creation.Objects, receipt)
	if err != nil {
		return fail(err)
	}
	creation.DurableReplayPhase = PhaseJournaled
	if err := checkTransitionHook(PhaseJournaled); err != nil {
		return fail(err)
	}
	stagePath, _ := fsbind.PathFromComponents([]string{stageDirectoryName})
	stage, err := subtree.Inspect(ctx, stagePath)
	if err != nil || stage.Kind != fsbind.ObjectKindDirectory {
		if err == nil {
			err = fsbind.ErrUnsafeObject
		}
		return fail(err)
	}
	receipt, err = handle.append(ctx, Event{Phase: PhaseStageCreated, ObjectIdentity: stage.Identity.String()})
	creation.Objects = append(creation.Objects, receipt)
	if err != nil {
		return fail(err)
	}
	creation.DurableReplayPhase = PhaseStageCreated
	if err := checkTransitionHook(PhaseStageCreated); err != nil {
		return fail(err)
	}
	return handle, creation, nil
}

func openJournal(ctx context.Context, session *fsbind.Session, operationID OperationID, limits Limits) (*journal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if session == nil {
		return nil, fmt.Errorf("target root session is unavailable")
	}
	if _, err := ParseOperationID(operationID.String()); err != nil {
		return nil, err
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	directoryName, err := OperationDirectoryName(operationID)
	if err != nil {
		return nil, err
	}
	operationObject, inspectErr := session.InspectRoot(ctx, directoryName)
	if errors.Is(inspectErr, fsbind.ErrNotFound) {
		return nil, fmt.Errorf("%w: explicit operation does not exist", ErrOperationNotFound)
	}
	if inspectErr != nil {
		return nil, classifyJournalReadError(inspectErr, "explicit operation is unsafe")
	}
	if operationObject.Kind != fsbind.ObjectKindDirectory {
		return nil, fmt.Errorf("%w: explicit operation is not a private directory", ErrCorruptJournal)
	}
	subtree, err := session.OpenPrivateSubtreeObserved(directoryName)
	if err != nil {
		return nil, classifyJournalReadError(err, "operation lock or subtree is unavailable")
	}
	fail := func(err error) (*journal, error) {
		_ = subtree.Close()
		return nil, err
	}
	intentPath, _ := fsbind.PathFromComponents([]string{intentFileName})
	intentFile, err := subtree.OpenRegular(ctx, intentPath)
	if err != nil {
		return fail(classifyJournalReadError(err, "intent is unavailable"))
	}
	intentBefore, err := intentFile.Info()
	if err != nil {
		_ = intentFile.Close()
		return fail(classifyJournalReadError(err, "inspect intent failed"))
	}
	intent, decodeErr := DecodeIntent(intentFile, limits)
	intentAfter, statErr := intentFile.Info()
	closeErr := intentFile.Close()
	if decodeErr != nil {
		if !errors.Is(decodeErr, ErrCorruptJournal) {
			return fail(classifyJournalReadError(decodeErr, "read intent failed"))
		}
		return fail(fmt.Errorf("%w: intent is unstable or invalid", ErrCorruptJournal))
	}
	if statErr != nil {
		return fail(classifyJournalReadError(statErr, "reinspect intent failed"))
	}
	if closeErr != nil {
		return fail(closeErr)
	}
	if !intentBefore.Identity.Equal(intentAfter.Identity) || intentBefore.SizeBytes != intentAfter.SizeBytes ||
		!intentBefore.Modified.Equal(intentAfter.Modified) {
		return fail(fmt.Errorf("%w: intent is unstable or invalid", ErrCorruptJournal))
	}
	canonicalIntent, encodeErr := EncodeIntent(intent)
	if encodeErr != nil {
		return fail(fmt.Errorf("%w: intent cannot be canonically re-encoded", ErrCorruptJournal))
	}
	if journalReplayReadHook != nil {
		if hookErr := journalReplayReadHook("intent", -1); hookErr != nil {
			return fail(hookErr)
		}
	}
	if err := verifySubtreeNamedBytes(ctx, subtree, intentPath, canonicalIntent, intentAfter.Identity); err != nil {
		return fail(err)
	}
	computedID, err := OperationIDFor(intent)
	if err != nil || computedID != operationID || intent.TargetRootIdentity != session.Info().Identity.String() {
		return fail(fmt.Errorf("%w: intent identity disagrees with the bound target", ErrCorruptJournal))
	}
	rootPath, _ := fsbind.PathFromComponents(nil)
	rootListing, err := subtree.List(ctx, rootPath, fsbind.ListLimits{MaxEntries: 16, MaxNameBytes: 1 << 20})
	if err != nil {
		return fail(classifyJournalReadError(err, "operation root inventory failed"))
	}
	if !rootListing.Complete {
		return fail(fmt.Errorf("%w: operation root inventory is incomplete", ErrCorruptJournal))
	}
	allowed := map[string]string{
		operationLockEntryName: "regular", intentFileName: "regular",
		journalDirectoryName: "directory", scratchDirectoryName: "directory", stageDirectoryName: "directory",
	}
	for _, entry := range rootListing.Entries {
		if allowed[entry.Name] != entry.Kind {
			return fail(fmt.Errorf("%w: operation root contains an unexpected object", ErrCorruptJournal))
		}
		delete(allowed, entry.Name)
	}
	if len(allowed) != 0 {
		return fail(fmt.Errorf("%w: operation root is incomplete", ErrCorruptJournal))
	}
	journalPath, _ := fsbind.PathFromComponents([]string{journalDirectoryName})
	listing, err := subtree.List(ctx, journalPath, fsbind.ListLimits{
		MaxEntries: limits.MaxJournalEvents, MaxNameBytes: int64(limits.MaxJournalEvents) * 96,
	})
	if err != nil {
		return fail(classifyJournalReadError(err, "journal inventory failed"))
	}
	if !listing.Complete || len(listing.Entries) == 0 {
		return fail(fmt.Errorf("%w: journal inventory is incomplete", ErrCorruptJournal))
	}
	handle := &journal{
		intent: intent, session: session, subtree: subtree,
		state: ReplayState{OperationID: operationID, LastSequence: -1, StagedFiles: []StagedFileState{}},
	}
	for position, entry := range listing.Entries {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if entry.Kind != "regular" {
			return fail(fmt.Errorf("%w: journal contains a non-regular event", ErrCorruptJournal))
		}
		sequence, filenameID, parseErr := ParseEventFileName(entry.Name)
		if parseErr != nil || sequence != position {
			return fail(fmt.Errorf("%w: journal filenames are not a complete sequence", ErrCorruptJournal))
		}
		path, _ := fsbind.PathFromComponents([]string{journalDirectoryName, entry.Name})
		file, openErr := subtree.OpenRegular(ctx, path)
		if openErr != nil {
			return fail(classifyJournalReadError(openErr, "journal event is unavailable"))
		}
		before, infoErr := file.Info()
		event, decodedID, decodeErr := DecodeEvent(file, limits)
		after, afterErr := file.Info()
		closeErr := file.Close()
		if infoErr != nil {
			return fail(classifyJournalReadError(infoErr, "inspect journal event failed"))
		}
		if decodeErr != nil {
			if !errors.Is(decodeErr, ErrCorruptJournal) {
				return fail(classifyJournalReadError(decodeErr, "read journal event failed"))
			}
			return fail(fmt.Errorf("%w: journal event is unstable or invalid", ErrCorruptJournal))
		}
		if decodedID != filenameID {
			return fail(fmt.Errorf("%w: journal event is unstable or invalid", ErrCorruptJournal))
		}
		if afterErr != nil {
			return fail(classifyJournalReadError(afterErr, "reinspect journal event failed"))
		}
		if closeErr != nil {
			return fail(closeErr)
		}
		if !before.Identity.Equal(after.Identity) || before.SizeBytes != after.SizeBytes || !before.Modified.Equal(after.Modified) {
			return fail(fmt.Errorf("%w: journal event is unstable or invalid", ErrCorruptJournal))
		}
		canonicalEvent, canonicalID, encodeErr := EncodeEvent(event, limits)
		if encodeErr != nil || canonicalID != decodedID {
			return fail(fmt.Errorf("%w: journal event cannot be canonically re-encoded", ErrCorruptJournal))
		}
		if journalReplayReadHook != nil {
			if hookErr := journalReplayReadHook("event", position); hookErr != nil {
				return fail(hookErr)
			}
		}
		if err := verifySubtreeNamedBytes(ctx, subtree, path, canonicalEvent, after.Identity); err != nil {
			return fail(err)
		}
		if replayErr := replayOne(intent, &handle.state, JournalEvent{Event: event, ID: decodedID}, position); replayErr != nil {
			return fail(replayErr)
		}
	}
	if handle.state.OperationRootIdentity != subtree.Identity().String() {
		return fail(fmt.Errorf("%w: operation subtree identity disagrees with its journal", ErrCorruptJournal))
	}
	if handle.state.StageContainerIdentity != "" {
		if err := verifyStageContainer(ctx, handle); err != nil {
			return fail(err)
		}
	}
	if err := handle.observeScratch(ctx); err != nil {
		return fail(err)
	}
	if err := handle.checkControlPaths("operation directory binding changed during replay"); err != nil {
		return fail(err)
	}
	return handle, nil
}

func classifyJournalReadError(err error, message string) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, fsbind.ErrBusy) {
		return err
	}
	if errors.Is(err, fsbind.ErrNotFound) || errors.Is(err, fsbind.ErrUnsafeObject) ||
		errors.Is(err, fsbind.ErrBindingChanged) || errors.Is(err, fsbind.ErrCrossFilesystem) {
		return fmt.Errorf("%w: %s", ErrCorruptJournal, message)
	}
	return err
}

func (journal *journal) append(ctx context.Context, event Event) (journalWriteReceipt, error) {
	if journal == nil || journal.subtree == nil {
		return journalWriteReceipt{}, fmt.Errorf("%w: journal handle is unavailable", ErrCorruptJournal)
	}
	event.Schema = EventSchemaV1
	event.OperationID = journal.state.OperationID
	event.Sequence = journal.state.LastSequence + 1
	if event.Sequence == 0 {
		event.Previous = journal.state.OperationID.String()
	} else {
		event.Previous = journal.state.LastEventID.String()
	}
	raw, id, err := EncodeEvent(event, journal.intent.Limits)
	if err != nil {
		return journalWriteReceipt{}, err
	}
	preview := journal.state
	if err := replayOne(journal.intent, &preview, JournalEvent{Event: event, ID: id}, event.Sequence); err != nil {
		return journalWriteReceipt{}, err
	}
	name, err := EventFileName(event.Sequence, id)
	if err != nil {
		return journalWriteReceipt{}, err
	}
	destination, _ := fsbind.PathFromComponents([]string{journalDirectoryName, name})
	receipt, err := journal.commitBytes(ctx, fmt.Sprintf("event-%09d", event.Sequence), destination, raw)
	if err != nil {
		return receipt, err
	}
	if err := replayOne(journal.intent, &journal.state, JournalEvent{Event: event, ID: id}, event.Sequence); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func (journal *journal) confirmReplayDurability(ctx context.Context) error {
	if journal == nil || journal.subtree == nil {
		return fmt.Errorf("%w: journal handle is unavailable", ErrCorruptJournal)
	}
	journalPath, _ := fsbind.PathFromComponents([]string{journalDirectoryName})
	if err := journal.subtree.SyncDirectory(ctx, journalPath); err != nil {
		return classifyJournalReadError(err, "journal directory changed during durability confirmation")
	}
	rootPath, _ := fsbind.PathFromComponents(nil)
	if err := journal.subtree.SyncDirectory(ctx, rootPath); err != nil {
		return classifyJournalReadError(err, "operation directory changed during durability confirmation")
	}
	return journal.checkControlPaths("operation directory binding changed during journal durability confirmation")
}

func (journal *journal) checkControlPaths(message string) error {
	if journal == nil || journal.subtree == nil {
		return fmt.Errorf("%w: journal handle is unavailable", ErrCorruptJournal)
	}
	journalPath, _ := fsbind.PathFromComponents([]string{journalDirectoryName})
	scratchPath, _ := fsbind.PathFromComponents([]string{scratchDirectoryName})
	if err := journal.subtree.CheckPaths(journalPath, scratchPath); err != nil {
		return classifyJournalReadError(err, message)
	}
	return nil
}

func (journal *journal) commitBytes(ctx context.Context, purpose string, destination fsbind.Path, raw []byte) (journalWriteReceipt, error) {
	receipt := journalWriteReceipt{}
	if journal == nil || journal.subtree == nil {
		return receipt, fmt.Errorf("%w: journal handle is unavailable", ErrCorruptJournal)
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if err := journal.checkControlPaths("operation directory binding changed before journal write"); err != nil {
		return receipt, err
	}
	if int64(len(raw)) > journal.intent.Limits.MaxEventBytes {
		return receipt, fmt.Errorf("%w: journal payload exceeds its byte limit", ErrCorruptJournal)
	}
	nonce, err := randomHex(16)
	if err != nil {
		return receipt, fmt.Errorf("create journal scratch name failed")
	}
	temp, err := fsbind.PathFromComponents([]string{scratchDirectoryName, purpose + "-" + nonce + ".pending"})
	if err != nil {
		return receipt, err
	}
	if err := journal.reserveScratch(ctx, int64(len(raw))); err != nil {
		return receipt, err
	}
	file, err := journal.subtree.CreateRegular(ctx, temp)
	if err != nil {
		return receipt, err
	}
	receipt.ScratchCreated = true
	written, writeErr := writeFullContext(ctx, file, raw)
	receipt.ScratchBytesWritten = written
	if writeErr == nil {
		writeErr = file.Sync()
	}
	info, infoErr := file.Info()
	closeErr := file.Close()
	if writeErr != nil {
		return receipt, writeErr
	}
	if closeErr != nil {
		return receipt, closeErr
	}
	if infoErr != nil {
		return receipt, infoErr
	}
	if info.SizeBytes != int64(len(raw)) || info.Identity.IsZero() {
		return receipt, fmt.Errorf("%w: journal scratch object changed", ErrCorruptJournal)
	}
	if journalScratchCreatedHook != nil {
		if err := journalScratchCreatedHook(purpose); err != nil {
			return receipt, err
		}
	}
	if journalScratchMutationHook != nil {
		if err := journalScratchMutationHook(ctx, journal, purpose, temp, raw); err != nil {
			return receipt, err
		}
	}
	publication, commitErr := runPublication("journal", func() (fsbind.Publication, error) {
		return journal.subtree.CommitRegularNoReplace(ctx, temp, destination)
	})
	receipt.Attempted = publication.Attempted
	receipt.Ambiguous = publicationResultAmbiguous(publication, commitErr)
	receipt.Published = publication.Published
	receipt.Durability = publication.Durability
	receipt.Identity = publication.FinalIdentity.String()
	if publication.Published {
		journal.releaseScratch(int64(len(raw)))
	}
	if commitErr != nil {
		if errors.Is(commitErr, fsbind.ErrNotFound) || errors.Is(commitErr, fsbind.ErrUnsafeObject) ||
			errors.Is(commitErr, fsbind.ErrBindingChanged) || errors.Is(commitErr, fsbind.ErrCrossFilesystem) {
			return receipt, errors.Join(fmt.Errorf("%w: operation binding changed during journal publication", ErrCorruptJournal), commitErr)
		}
		return receipt, commitErr
	}
	if !publication.Published {
		return receipt, fsbind.ErrPublicationAmbiguous
	}
	if !publication.SourceIdentity.Equal(info.Identity) || !publication.FinalIdentity.Equal(info.Identity) {
		return receipt, fmt.Errorf("%w: published journal object identity disagrees with the staged bytes", ErrCorruptJournal)
	}
	if publication.Durability != fsbind.DurabilityConfirmed {
		return receipt, fsbind.ErrDurabilityUnconfirmed
	}
	if err := journal.checkControlPaths("operation directory binding changed after journal write"); err != nil {
		return receipt, err
	}
	if err := journal.verifyCommittedBytes(ctx, destination, raw, info.Identity); err != nil {
		return receipt, err
	}
	if err := journal.checkControlPaths("operation directory binding changed after journal verification"); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func (journal *journal) verifyCommittedBytes(ctx context.Context, destination fsbind.Path, expected []byte, expectedIdentity fsbind.Identity) error {
	return verifySubtreeNamedBytes(ctx, journal.subtree, destination, expected, expectedIdentity)
}

func verifySubtreeNamedBytes(ctx context.Context, subtree *fsbind.Subtree, destination fsbind.Path, expected []byte, expectedIdentity fsbind.Identity) error {
	if subtree == nil || expectedIdentity.IsZero() {
		return fmt.Errorf("%w: journal object authority is unavailable", ErrCorruptJournal)
	}
	file, err := subtree.OpenRegular(ctx, destination)
	if err != nil {
		return classifyJournalReadError(err, "published journal object is unavailable")
	}
	before, beforeErr := file.Info()
	if beforeErr != nil {
		_ = file.Close()
		return classifyJournalReadError(beforeErr, "inspect published journal object failed")
	}
	observed := make([]byte, len(expected))
	reader := &contextExactReader{ctx: ctx, reader: file, remaining: int64(len(expected))}
	_, readErr := io.ReadFull(reader, observed)
	contentMismatch := errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF)
	var extra [1]byte
	if readErr == nil {
		n, extraErr := file.Read(extra[:])
		switch {
		case n != 0:
			contentMismatch = true
		case errors.Is(extraErr, io.EOF):
		case extraErr != nil:
			readErr = extraErr
		default:
			readErr = io.ErrNoProgress
		}
	}
	after, afterErr := file.Info()
	closeErr := file.Close()
	if contentMismatch {
		return fmt.Errorf("%w: published journal object has different bytes", ErrCorruptJournal)
	}
	if readErr != nil || afterErr != nil || closeErr != nil {
		if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
			return readErr
		}
		if readErr != nil {
			return readErr
		}
		if afterErr != nil {
			return classifyJournalReadError(afterErr, "reinspect published journal object failed")
		}
		return closeErr
	}
	if !before.Identity.Equal(expectedIdentity) || !after.Identity.Equal(expectedIdentity) ||
		before.SizeBytes != int64(len(expected)) || after.SizeBytes != before.SizeBytes ||
		!before.Modified.Equal(after.Modified) || !bytes.Equal(observed, expected) {
		return fmt.Errorf("%w: published journal object disagrees with intended bytes", ErrCorruptJournal)
	}
	named, inspectErr := subtree.Inspect(ctx, destination)
	if inspectErr != nil {
		return classifyJournalReadError(inspectErr, "published journal object name is unavailable")
	}
	if named.Kind != fsbind.ObjectKindRegular || named.SizeBytes != int64(len(expected)) || !named.Identity.Equal(expectedIdentity) {
		return fmt.Errorf("%w: published journal object name disagrees with intended bytes", ErrCorruptJournal)
	}
	return nil
}

func (journal *journal) observeScratch(ctx context.Context) error {
	if journal == nil || journal.subtree == nil {
		return fmt.Errorf("%w: journal handle is unavailable", ErrCorruptJournal)
	}
	if journal.scratchObserved {
		return nil
	}
	limits := journal.intent.Limits
	path, _ := fsbind.PathFromComponents([]string{scratchDirectoryName})
	listing, err := journal.subtree.List(ctx, path, fsbind.ListLimits{
		MaxEntries:   limits.MaxScratchEntries,
		MaxNameBytes: int64(limits.MaxScratchEntries) * 128,
	})
	if err != nil {
		return classifyJournalReadError(err, "retained scratch inventory failed")
	}
	if !listing.Complete {
		return scratchCapacityError("retained scratch entry budget is exceeded")
	}
	var total int64
	for _, entry := range listing.Entries {
		if entry.Kind != "regular" || !validScratchEntryName(entry.Name) {
			return fmt.Errorf("%w: scratch contains an unexpected object", ErrCorruptJournal)
		}
		filePath, _ := fsbind.PathFromComponents([]string{scratchDirectoryName, entry.Name})
		file, openErr := journal.subtree.OpenRegular(ctx, filePath)
		if openErr != nil {
			return classifyJournalReadError(openErr, "retained scratch object is unavailable")
		}
		before, beforeErr := file.Info()
		after, afterErr := file.Info()
		closeErr := file.Close()
		if beforeErr != nil {
			return classifyJournalReadError(beforeErr, "inspect retained scratch object failed")
		}
		if afterErr != nil {
			return classifyJournalReadError(afterErr, "reinspect retained scratch object failed")
		}
		if closeErr != nil {
			return closeErr
		}
		if before.SizeBytes < 0 || !before.Identity.Equal(after.Identity) || before.SizeBytes != after.SizeBytes || !before.Modified.Equal(after.Modified) {
			return fmt.Errorf("%w: retained scratch object is unstable", ErrCorruptJournal)
		}
		if journalScratchObserveHook != nil {
			if hookErr := journalScratchObserveHook(entry.Name); hookErr != nil {
				return hookErr
			}
		}
		named, namedErr := journal.subtree.Inspect(ctx, filePath)
		if namedErr != nil {
			return classifyJournalReadError(namedErr, "retained scratch object name changed")
		}
		if named.Kind != fsbind.ObjectKindRegular || named.SizeBytes != before.SizeBytes || !named.Identity.Equal(before.Identity) {
			return fmt.Errorf("%w: retained scratch object name changed", ErrCorruptJournal)
		}
		if before.SizeBytes > limits.MaxScratchBytes-total {
			return scratchCapacityError("retained scratch byte budget is exceeded")
		}
		total += before.SizeBytes
	}
	journal.scratchEntries = len(listing.Entries)
	journal.scratchBytes = total
	journal.scratchObserved = true
	return journal.checkControlPaths("operation directory binding changed during scratch inventory")
}

func (journal *journal) reserveScratch(ctx context.Context, bytes int64) error {
	if bytes < 0 {
		return fmt.Errorf("%w: scratch reservation is invalid", ErrPolicy)
	}
	journal.scratchObserved = false
	if err := journal.observeScratch(ctx); err != nil {
		return err
	}
	if journal.scratchEntries >= journal.intent.Limits.MaxScratchEntries ||
		bytes > journal.intent.Limits.MaxScratchBytes-journal.scratchBytes {
		return scratchCapacityError("retained scratch budget is exhausted")
	}
	journal.scratchEntries++
	journal.scratchBytes += bytes
	return nil
}

func scratchCapacityError(message string) error {
	return fmt.Errorf("%w: %w (%s)", ErrPolicy, errScratchCapacity, message)
}

func scratchCapacityDefinitelyExhausted(journal *journal) bool {
	if journal == nil {
		return false
	}
	return journal.scratchEntries >= journal.intent.Limits.MaxScratchEntries ||
		journal.scratchBytes >= journal.intent.Limits.MaxScratchBytes
}

func (journal *journal) releaseScratch(bytes int64) {
	if journal == nil || journal.scratchEntries <= 0 || bytes < 0 || bytes > journal.scratchBytes {
		return
	}
	journal.scratchEntries--
	journal.scratchBytes -= bytes
}

func validScratchEntryName(name string) bool {
	if !strings.HasSuffix(name, ".pending") {
		return false
	}
	base := strings.TrimSuffix(name, ".pending")
	nonceAt := strings.LastIndexByte(base, '-')
	if nonceAt <= 0 || len(base)-nonceAt-1 != 32 {
		return false
	}
	if _, err := hex.DecodeString(base[nonceAt+1:]); err != nil {
		return false
	}
	purpose := base[:nonceAt]
	if purpose == "intent" {
		return true
	}
	for _, prefix := range []string{"event-", "copy-"} {
		if strings.HasPrefix(purpose, prefix) {
			sequence := strings.TrimPrefix(purpose, prefix)
			return len(sequence) == 9 && strings.Trim(sequence, "0123456789") == ""
		}
	}
	return false
}

func writeFullContext(ctx context.Context, writer io.Writer, raw []byte) (int64, error) {
	var total int64
	for len(raw) > 0 {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		written, err := writer.Write(raw)
		if written > 0 {
			raw = raw[written:]
			total += int64(written)
		}
		if err != nil {
			return total, err
		}
		if written == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func verifyStageContainer(ctx context.Context, journal *journal) error {
	if journal == nil || journal.subtree == nil || journal.state.StageContainerIdentity == "" {
		return fmt.Errorf("%w: staging container authority is unavailable", ErrCorruptJournal)
	}
	expected, err := fsbind.ParseIdentity(journal.state.StageContainerIdentity)
	if err != nil || expected.IsZero() {
		return fmt.Errorf("%w: staging container identity is invalid", ErrCorruptJournal)
	}
	path, _ := fsbind.PathFromComponents([]string{stageDirectoryName})
	observed, err := journal.subtree.Inspect(ctx, path)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if errors.Is(err, fsbind.ErrNotFound) || errors.Is(err, fsbind.ErrUnsafeObject) ||
			errors.Is(err, fsbind.ErrBindingChanged) || errors.Is(err, fsbind.ErrCrossFilesystem) {
			return fmt.Errorf("%w: staging container changed", ErrIntegrity)
		}
		return err
	}
	if observed.Kind != fsbind.ObjectKindDirectory || !observed.Identity.Equal(expected) {
		return fmt.Errorf("%w: staging container changed", ErrIntegrity)
	}
	return nil
}

func randomHex(bytes int) (string, error) {
	if bytes <= 0 {
		return "", errors.New("random byte count is invalid")
	}
	raw := make([]byte, bytes)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func operationIDFromDirectoryEntry(name string) (OperationID, bool) {
	if !strings.HasPrefix(name, operationDirectoryPrefix) {
		return "", false
	}
	id, err := ParseOperationDirectoryName(name)
	return id, err == nil
}
