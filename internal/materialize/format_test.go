package materialize

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

type materializeErrorReader struct {
	err error
}

func (reader materializeErrorReader) Read([]byte) (int, error) {
	return 0, reader.err
}

func testIntent() Intent {
	return Intent{
		Schema: IntentSchemaV1, MetafileVariantID: "sha256:" + strings.Repeat("1", 64),
		InfoHashV1: strings.Repeat("2", 40), PlanID: strings.Repeat("3", 24),
		Nonce:    strings.Repeat("4", 64),
		Strategy: StrategyCopy, TargetRootIdentity: testFSIdentity("1"),
		FinalRawComponentsBase64: []string{"dG9ycmVudA=="},
		MultiFile:                true, ManifestFiles: 2, ContentBytes: 3,
		ManifestPathBytes: 20, NamespaceObjects: 3, NamespaceBytes: 256, Limits: DefaultLimits(),
	}
}

func TestIntentCanonicalRoundTripAndStableOperationID(t *testing.T) {
	intent := testIntent()
	raw, err := EncodeIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeIntent(bytes.NewReader(raw), intent.Limits)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.MetafileVariantID != intent.MetafileVariantID || decoded.PlanID != intent.PlanID {
		t.Fatalf("decoded intent differs: %#v", decoded)
	}
	first, err := OperationIDFor(intent)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OperationIDFor(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("operation ID changed: %q != %q", first, second)
	}
	if _, err := ParseOperationID(first.String()); err != nil {
		t.Fatal(err)
	}
}

func TestJournalDecodersPreserveReadErrorsWithoutClaimingCorruption(t *testing.T) {
	sentinel := errors.New("transient read failure")
	if _, err := DecodeIntent(materializeErrorReader{err: sentinel}, DefaultLimits()); !errors.Is(err, sentinel) || errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("intent read error was converted into journal corruption: %v", err)
	}
	if _, _, err := DecodeEvent(materializeErrorReader{err: sentinel}, DefaultLimits()); !errors.Is(err, sentinel) || errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("event read error was converted into journal corruption: %v", err)
	}
}

func TestIntentRejectsInvalidTypedIdentityAndPath(t *testing.T) {
	tests := []func(*Intent){
		func(v *Intent) { v.InfoHashV1 = strings.Repeat("A", 40) },
		func(v *Intent) { v.InfoHashV1, v.InfoHashV2 = "", "" },
		func(v *Intent) { v.InfoHashV2 = strings.Repeat("a", 63) },
		func(v *Intent) { v.MetafileVariantID = strings.Repeat("a", 64) },
		func(v *Intent) { v.FinalRawComponentsBase64 = []string{"Li4="} },
		func(v *Intent) { v.FinalRawComponentsBase64 = []string{"YVxi"} },
	}
	for index, mutate := range tests {
		intent := testIntent()
		mutate(&intent)
		if _, err := EncodeIntent(intent); !errors.Is(err, ErrInvalidIntent) {
			t.Fatalf("case %d: expected invalid intent, got %v", index, err)
		}
	}
}

func TestDecodeIntentRejectsNonCanonicalJSON(t *testing.T) {
	intent := testIntent()
	raw, err := EncodeIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	cases := [][]byte{
		bytes.Replace(raw, []byte(`"schema":`), []byte(`"schema":"ptctl.materialize-intent/v1","schema":`), 1),
		bytes.Replace(raw, []byte(`"schema":`), []byte(`"unknown":1,"schema":`), 1),
		append(append([]byte(nil), raw...), ' '),
		bytes.TrimSuffix(raw, []byte{'\n'}),
	}
	for index, candidate := range cases {
		if _, err := DecodeIntent(bytes.NewReader(candidate), intent.Limits); !errors.Is(err, ErrCorruptJournal) {
			t.Fatalf("case %d: expected corrupt journal, got %v", index, err)
		}
	}
}

func TestEventCanonicalRoundTrip(t *testing.T) {
	intent := testIntent()
	operationID, err := OperationIDFor(intent)
	if err != nil {
		t.Fatal(err)
	}
	event := Event{
		Schema: EventSchemaV1, OperationID: operationID, Sequence: 0,
		Previous: operationID.String(), Phase: PhaseJournaled, ObjectIdentity: testFSIdentity("2"),
	}
	raw, id, err := EncodeEvent(event, intent.Limits)
	if err != nil {
		t.Fatal(err)
	}
	decoded, decodedID, err := DecodeEvent(bytes.NewReader(raw), intent.Limits)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != event || decodedID != id {
		t.Fatalf("event round trip differs: %#v %#v", decoded, decodedID)
	}
	if _, err := ParseEventID(id.String()); err != nil {
		t.Fatal(err)
	}
}

func TestEventRejectsNonCanonicalOrOversizedObjectIdentity(t *testing.T) {
	operationID, err := OperationIDFor(testIntent())
	if err != nil {
		t.Fatal(err)
	}
	index := 0
	for _, identity := range []string{"not-an-identity", strings.Repeat("x", 64<<10)} {
		event := Event{
			Schema: EventSchemaV1, OperationID: operationID, Sequence: 1,
			Previous: "sha256:" + strings.Repeat("a", 64), Phase: PhaseFileStaged,
			ManifestIndex: &index, Bytes: 1, ObjectIdentity: identity,
			ContentSHA256: strings.Repeat("b", 64),
		}
		if err := event.Validate(); err == nil || !errors.Is(err, ErrCorruptJournal) {
			t.Fatalf("unsafe object identity was accepted: %d bytes, %v", len(identity), err)
		}
	}
}

func TestOperationAndEventNamesAreCanonical(t *testing.T) {
	intent := testIntent()
	operationID, err := OperationIDFor(intent)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := OperationDirectoryName(operationID)
	if err != nil {
		t.Fatal(err)
	}
	parsedOperation, err := ParseOperationDirectoryName(directory)
	if err != nil || parsedOperation != operationID {
		t.Fatalf("operation directory round trip failed: %q %v", parsedOperation, err)
	}
	event := Event{Schema: EventSchemaV1, OperationID: operationID, Sequence: 0, Previous: operationID.String(), Phase: PhaseJournaled, ObjectIdentity: testFSIdentity("2")}
	_, eventID, err := EncodeEvent(event, intent.Limits)
	if err != nil {
		t.Fatal(err)
	}
	name, err := EventFileName(0, eventID)
	if err != nil {
		t.Fatal(err)
	}
	sequence, parsedEvent, err := ParseEventFileName(name)
	if err != nil || sequence != 0 || parsedEvent != eventID {
		t.Fatalf("event filename round trip failed: %d %q %v", sequence, parsedEvent, err)
	}
	for _, bad := range []string{
		".ptctl-materialize-" + strings.Repeat("A", 64),
		"event-0-" + strings.Repeat("a", 64) + ".json",
		"event-000000000-" + strings.Repeat("A", 64) + ".json",
		"event-000000000-" + strings.Repeat("a", 64) + ".tmp",
	} {
		if strings.HasPrefix(bad, operationDirectoryPrefix) {
			if _, err := ParseOperationDirectoryName(bad); err == nil {
				t.Fatalf("accepted noncanonical operation directory %q", bad)
			}
		} else if _, _, err := ParseEventFileName(bad); err == nil {
			t.Fatalf("accepted noncanonical event filename %q", bad)
		}
	}
}

func testFSIdentity(digit string) string {
	return "fsbind-v1:" + strings.Repeat(digit, 64)
}
