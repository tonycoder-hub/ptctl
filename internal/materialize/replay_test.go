package materialize

import (
	"errors"
	"strings"
	"testing"
)

func appendTestEvent(t *testing.T, intent Intent, events []JournalEvent, event Event) []JournalEvent {
	t.Helper()
	operationID, err := OperationIDFor(intent)
	if err != nil {
		t.Fatal(err)
	}
	event.Schema = EventSchemaV1
	event.OperationID = operationID
	event.Sequence = len(events)
	if len(events) == 0 {
		event.Previous = operationID.String()
	} else {
		event.Previous = events[len(events)-1].ID.String()
	}
	_, id, err := EncodeEvent(event, intent.Limits)
	if err != nil {
		t.Fatal(err)
	}
	return append(events, JournalEvent{Event: event, ID: id})
}

func completeTestJournal(t *testing.T, intent Intent) []JournalEvent {
	t.Helper()
	var events []JournalEvent
	events = appendTestEvent(t, intent, events, Event{Phase: PhaseJournaled, ObjectIdentity: testFSIdentity("5")})
	events = appendTestEvent(t, intent, events, Event{Phase: PhaseStageCreated, ObjectIdentity: testFSIdentity("6")})
	first, second := 0, 1
	events = appendTestEvent(t, intent, events, Event{
		Phase: PhaseFileStaged, ManifestIndex: &first, Bytes: 3,
		ObjectIdentity: testFSIdentity("7"), ContentSHA256: strings.Repeat("a", 64),
	})
	events = appendTestEvent(t, intent, events, Event{
		Phase: PhaseFileStaged, ManifestIndex: &second, Bytes: 0,
		ObjectIdentity: testFSIdentity("8"), ContentSHA256: strings.Repeat("b", 64),
	})
	events = appendTestEvent(t, intent, events, Event{
		Phase: PhaseStageVerified, Bytes: 3, ObjectIdentity: testFSIdentity("9"), Proof: expectedProof(intent),
	})
	events = appendTestEvent(t, intent, events, Event{
		Phase: PhasePublishIntent, Bytes: 3, ObjectIdentity: testFSIdentity("9"),
	})
	events = appendTestEvent(t, intent, events, Event{
		Phase: PhasePublished, Bytes: 3, ObjectIdentity: testFSIdentity("9"),
	})
	events = appendTestEvent(t, intent, events, Event{
		Phase: PhaseFinalVerified, Bytes: 3, ObjectIdentity: testFSIdentity("9"), Proof: expectedProof(intent),
	})
	events = appendTestEvent(t, intent, events, Event{
		Phase: PhaseCommitted, Bytes: 3, ObjectIdentity: testFSIdentity("9"),
	})
	return events
}

func TestReplayCompleteJournal(t *testing.T) {
	intent := testIntent()
	events := completeTestJournal(t, intent)
	state, err := Replay(intent, events)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != PhaseCommitted || !state.Terminal || state.BytesStaged != 3 || len(state.StagedFiles) != 2 {
		t.Fatalf("unexpected replay state: %#v", state)
	}
}

func TestReplayRejectsTamperingBranchesAndGaps(t *testing.T) {
	intent := testIntent()
	tests := []func([]JournalEvent) []JournalEvent{
		func(events []JournalEvent) []JournalEvent {
			events[1].ID = EventID("sha256:" + strings.Repeat("f", 64))
			return events
		},
		func(events []JournalEvent) []JournalEvent {
			events[2].Event.Sequence++
			return events
		},
		func(events []JournalEvent) []JournalEvent {
			events[3].Event.Previous = events[0].ID.String()
			_, events[3].ID, _ = EncodeEvent(events[3].Event, intent.Limits)
			return events
		},
		func(events []JournalEvent) []JournalEvent {
			events[3].Event.ManifestIndex = events[2].Event.ManifestIndex
			_, events[3].ID, _ = EncodeEvent(events[3].Event, intent.Limits)
			return events[:4]
		},
		func(events []JournalEvent) []JournalEvent {
			events[4].Event.Bytes = 2
			_, events[4].ID, _ = EncodeEvent(events[4].Event, intent.Limits)
			return events[:5]
		},
		func(events []JournalEvent) []JournalEvent {
			return append(events, events[len(events)-1])
		},
	}
	for index, mutate := range tests {
		events := completeTestJournal(t, intent)
		if _, err := Replay(intent, mutate(events)); !errors.Is(err, ErrCorruptJournal) {
			t.Fatalf("case %d: expected corrupt journal, got %v", index, err)
		}
	}
}

func TestReplayRequiresProofForTypedMetafile(t *testing.T) {
	intent := testIntent()
	intent.InfoHashV2 = strings.Repeat("4", 64)
	events := completeTestJournal(t, intent)
	events[4].Event.Proof = ProofV1
	_, events[4].ID, _ = EncodeEvent(events[4].Event, intent.Limits)
	if _, err := Replay(intent, events[:5]); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("expected hybrid proof rejection, got %v", err)
	}
}

func TestReplayAbandonPreservesStagedByteAccounting(t *testing.T) {
	intent := testIntent()
	var events []JournalEvent
	events = appendTestEvent(t, intent, events, Event{Phase: PhaseJournaled, ObjectIdentity: testFSIdentity("5")})
	events = appendTestEvent(t, intent, events, Event{Phase: PhaseStageCreated, ObjectIdentity: testFSIdentity("6")})
	index := 0
	events = appendTestEvent(t, intent, events, Event{
		Phase: PhaseFileStaged, ManifestIndex: &index, Bytes: 3,
		ObjectIdentity: testFSIdentity("7"), ContentSHA256: strings.Repeat("a", 64),
	})
	events = appendTestEvent(t, intent, events, Event{Phase: PhaseAbandoned, Bytes: 3})
	state, err := Replay(intent, events)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != PhaseAbandoned || !state.Terminal {
		t.Fatalf("unexpected abandoned state: %#v", state)
	}
}
