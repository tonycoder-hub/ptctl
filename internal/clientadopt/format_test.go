package clientadopt

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type markerErrorReader struct{ err error }

func (reader markerErrorReader) Read([]byte) (int, error) { return 0, reader.err }

func TestIntentCanonicalFormatRejectsAmbiguousOrExtendedJSON(t *testing.T) {
	intent := validFormatIntent(t)
	raw, expectedID, err := encodeIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	decoded, actualID, err := decodeIntent(bytes.NewReader(raw))
	if err != nil || actualID != expectedID || !reflect.DeepEqual(decoded, intent) {
		t.Fatalf("decoded=%#v id=%q err=%v", decoded, actualID, err)
	}

	unknown := append([]byte(nil), raw[:len(raw)-2]...)
	unknown = append(unknown, []byte(",\"unknown\":true}\n")...)
	deep := []byte(`{"unknown":` + strings.Repeat("[", 40) + `0` + strings.Repeat("]", 40) + "}\n")
	wide := []byte(`{"unknown":[` + strings.Repeat("0,", 512) + `0]}\n`)
	tests := map[string][]byte{
		"duplicate_key":      bytes.Replace(raw, []byte(`{"schema":`), []byte(`{"schema":"`+IntentSchemaV1+`","schema":`), 1),
		"unknown_field":      unknown,
		"leading_space":      append([]byte{' '}, raw...),
		"trailing_json":      append(append([]byte(nil), raw...), []byte("{}\n")...),
		"unpaired_surrogate": bytes.Replace(raw, []byte(IntentSchemaV1), []byte(`\ud800`), 1),
		"oversized":          bytes.Repeat([]byte{'x'}, int(maximumMarkerBytes)+1),
		"excessive_depth":    deep,
		"excessive_nodes":    wide,
	}
	for name, malformed := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := decodeIntent(bytes.NewReader(malformed)); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("expected integrity rejection, got %v", err)
			}
		})
	}
}

func TestMarkerReadFailureRemainsOperational(t *testing.T) {
	sentinel := errors.New("synthetic read failure")
	_, _, err := decodeIntent(markerErrorReader{err: sentinel})
	if !errors.Is(err, sentinel) || errors.Is(err, ErrIntegrity) {
		t.Fatalf("read error was reclassified as integrity: %v", err)
	}
}

func TestPlanDriverIdentityCapabilitiesAreFailClosed(t *testing.T) {
	base := validFormatIntent(t).Plan
	base.Driver = DriverTransmission
	if err := base.Validate(); err != nil {
		t.Fatalf("Transmission v1 plan rejected: %v", err)
	}
	base.InfoHashV2 = strings.Repeat("a", 64)
	if err := base.Validate(); err == nil {
		t.Fatal("Transmission hybrid plan was accepted without typed v2 ledger authority")
	}
	base.InfoHashV1 = ""
	if err := base.Validate(); err == nil {
		t.Fatal("Transmission pure-v2 plan was accepted")
	}
	base.Driver = DriverQBittorrent
	if err := base.Validate(); err != nil {
		t.Fatalf("qBittorrent pure-v2 plan rejected: %v", err)
	}
}

func validFormatIntent(t *testing.T) Intent {
	t.Helper()
	shaID := func(character string) string { return "sha256:" + strings.Repeat(character, 64) }
	identity := func(character string) string { return "fsbind-v1:" + strings.Repeat(character, 64) }
	plan := Plan{
		Schema: PlanSchemaV1, Action: ActionAddStopped, Driver: DriverQBittorrent,
		ClientConfigID: shaID("1"), PathMappingID: shaID("2"), ClientPathSemantics: "posix_exact",
		ExpectedSavePathRef: shaID("3"), ExpectedContentPathRef: shaID("4"), MetafileVariantID: shaID("5"),
		MetafileBytes: 128, InfoHashV1: strings.Repeat("6", 40), MaterializeOperationID: shaID("7"),
		MaterializePlanID: strings.Repeat("8", 24), TargetRootIdentity: identity("9"), FinalObjectIdentity: identity("a"),
		ManifestFiles: 1, ContentBytes: 64,
	}
	planID, err := PlanID(plan)
	if err != nil {
		t.Fatal(err)
	}
	return Intent{
		Schema: IntentSchemaV1, OperationID: OperationIDForPlan(planID), OperationRootIdentity: identity("b"),
		PlanID: planID, Plan: plan,
	}
}
