package materialize

import (
	"fmt"
	"math"
)

// JournalEvent pairs an event with the content-derived identifier used by the
// on-disk filename and the next hash-chain link.
type JournalEvent struct {
	Event Event
	ID    EventID
}

type StagedFileState struct {
	ManifestIndex  int
	Bytes          int64
	ObjectIdentity string
	ContentSHA256  string
}

// ReplayState is derived exclusively from a complete, strictly linear
// journal. Callers must still inspect filesystem identities and reverify bytes
// before performing any transition.
type ReplayState struct {
	OperationID            OperationID
	OperationRootIdentity  string
	LastEventID            EventID
	LastSequence           int
	Phase                  Phase
	StagedFiles            []StagedFileState
	BytesStaged            int64
	StageContainerIdentity string
	StageIdentity          string
	FinalIdentity          string
	Proof                  string
	Terminal               bool
}

func Replay(intent Intent, events []JournalEvent) (ReplayState, error) {
	if err := intent.Validate(); err != nil {
		return ReplayState{}, err
	}
	operationID, err := OperationIDFor(intent)
	if err != nil {
		return ReplayState{}, err
	}
	if len(events) == 0 || len(events) > intent.Limits.MaxJournalEvents {
		return ReplayState{}, fmt.Errorf("%w: journal event count is invalid", ErrCorruptJournal)
	}
	state := ReplayState{
		OperationID:  operationID,
		LastSequence: -1,
		StagedFiles:  []StagedFileState{},
	}
	for position, record := range events {
		if err := replayOne(intent, &state, record, position); err != nil {
			return ReplayState{}, err
		}
	}
	return state, nil
}

func replayOne(intent Intent, state *ReplayState, record JournalEvent, position int) error {
	event := record.Event
	if state == nil || event.OperationID != state.OperationID || event.Sequence != position {
		return fmt.Errorf("%w: event sequence or operation identity disagrees", ErrCorruptJournal)
	}
	_, computedID, encodeErr := EncodeEvent(event, intent.Limits)
	if encodeErr != nil || record.ID == "" || computedID != record.ID {
		return fmt.Errorf("%w: event identifier disagrees with canonical content", ErrCorruptJournal)
	}
	expectedPrevious := state.OperationID.String()
	if position > 0 {
		expectedPrevious = state.LastEventID.String()
	}
	if event.Previous != expectedPrevious {
		return fmt.Errorf("%w: journal hash chain is broken", ErrCorruptJournal)
	}
	if err := applyEvent(intent, state, event); err != nil {
		return err
	}
	state.LastEventID = record.ID
	state.LastSequence = position
	return nil
}

func applyEvent(intent Intent, state *ReplayState, event Event) error {
	if state.Terminal {
		return fmt.Errorf("%w: event follows a terminal transition", ErrCorruptJournal)
	}
	switch event.Phase {
	case PhaseJournaled:
		if state.LastSequence != -1 {
			return transitionError(state.Phase, event.Phase)
		}
		state.OperationRootIdentity = event.ObjectIdentity
	case PhaseStageCreated:
		if state.Phase != PhaseJournaled {
			return transitionError(state.Phase, event.Phase)
		}
		state.StageContainerIdentity = event.ObjectIdentity
	case PhaseFileStaged:
		if state.Phase != PhaseStageCreated && state.Phase != PhaseFileStaged {
			return transitionError(state.Phase, event.Phase)
		}
		expectedIndex := len(state.StagedFiles)
		if event.ManifestIndex == nil || *event.ManifestIndex != expectedIndex || expectedIndex >= intent.ManifestFiles {
			return fmt.Errorf("%w: staged files are not a complete ordered manifest prefix", ErrCorruptJournal)
		}
		if event.Bytes > math.MaxInt64-state.BytesStaged || state.BytesStaged+event.Bytes > intent.ContentBytes {
			return fmt.Errorf("%w: staged byte total is invalid", ErrCorruptJournal)
		}
		state.StagedFiles = append(state.StagedFiles, StagedFileState{
			ManifestIndex: *event.ManifestIndex, Bytes: event.Bytes,
			ObjectIdentity: event.ObjectIdentity, ContentSHA256: event.ContentSHA256,
		})
		state.BytesStaged += event.Bytes
	case PhaseStageVerified:
		if state.Phase != PhaseStageCreated && state.Phase != PhaseFileStaged {
			return transitionError(state.Phase, event.Phase)
		}
		if len(state.StagedFiles) != intent.ManifestFiles || state.BytesStaged != intent.ContentBytes || event.Bytes != intent.ContentBytes || event.Proof != expectedProof(intent) {
			return fmt.Errorf("%w: stage verification does not cover the manifest", ErrCorruptJournal)
		}
		state.StageIdentity = event.ObjectIdentity
		state.Proof = event.Proof
	case PhasePublishIntent:
		if state.Phase != PhaseStageVerified || event.ObjectIdentity != state.StageIdentity || event.Bytes != intent.ContentBytes {
			return transitionError(state.Phase, event.Phase)
		}
	case PhasePublished:
		if state.Phase != PhasePublishIntent || event.ObjectIdentity != state.StageIdentity || event.Bytes != intent.ContentBytes {
			return transitionError(state.Phase, event.Phase)
		}
		state.FinalIdentity = event.ObjectIdentity
	case PhaseFinalVerified:
		if state.Phase != PhasePublished || event.ObjectIdentity != state.FinalIdentity || event.Bytes != intent.ContentBytes || event.Proof != expectedProof(intent) {
			return transitionError(state.Phase, event.Phase)
		}
		state.Proof = event.Proof
	case PhaseCommitted:
		if state.Phase != PhaseFinalVerified || event.ObjectIdentity != state.FinalIdentity || event.Bytes != intent.ContentBytes {
			return transitionError(state.Phase, event.Phase)
		}
		state.Terminal = true
	case PhaseAbandoned:
		if state.Phase != PhaseJournaled && state.Phase != PhaseStageCreated && state.Phase != PhaseFileStaged && state.Phase != PhaseStageVerified {
			return transitionError(state.Phase, event.Phase)
		}
		if event.Bytes != state.BytesStaged {
			return fmt.Errorf("%w: abandoned byte total disagrees", ErrCorruptJournal)
		}
		state.Terminal = true
	default:
		return fmt.Errorf("%w: phase is unsupported", ErrCorruptJournal)
	}
	state.Phase = event.Phase
	return nil
}

func transitionError(before, after Phase) error {
	return fmt.Errorf("%w: transition %q -> %q is invalid", ErrCorruptJournal, before, after)
}

func expectedProof(intent Intent) string {
	switch {
	case intent.InfoHashV1 != "" && intent.InfoHashV2 != "":
		return ProofHybrid
	case intent.InfoHashV2 != "":
		return ProofV2
	default:
		return ProofV1
	}
}
