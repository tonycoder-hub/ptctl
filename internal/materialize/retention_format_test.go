package materialize

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/fsbind"
)

func TestRetentionMarkersAreCanonicalAndKindSeparated(t *testing.T) {
	operationIdentity := identityFromTestRaw(811)
	targetIdentity := identityFromTestRaw(812)
	finalIdentity := identityFromTestRaw(813)
	intent := RetentionIntent{
		Schema: RetentionIntentSchemaV1, OperationID: retentionTestOperationID(41),
		OperationRootIdentity: operationIdentity.String(), TargetRootIdentity: targetIdentity.String(),
		IntentSHA256: retentionTestSHA256ID(42), TerminalEventID: EventID(retentionTestSHA256ID(43)), TerminalPhase: PhaseCommitted,
		PlanID: strings.Repeat("a", 24), MetafileVariantID: retentionTestSHA256ID(44), Basis: RetentionBasisCommitted,
		FinalObjectIdentity: finalIdentity.String(),
	}
	raw, intentID, err := EncodeRetentionIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	decoded, decodedID, err := DecodeRetentionIntent(bytes.NewReader(raw))
	if err != nil || decoded != intent || decodedID != intentID {
		t.Fatalf("intent round trip: decoded=%#v id=%q err=%v", decoded, decodedID, err)
	}
	complete := RetentionComplete{
		Schema: RetentionCompleteSchemaV1, OperationID: intent.OperationID,
		OperationRootIdentity: operationIdentity.String(), TargetRootIdentity: targetIdentity.String(), IntentMarkerID: intentID,
	}
	completeRaw, completeID, err := EncodeRetentionComplete(complete)
	if err != nil {
		t.Fatal(err)
	}
	decodedComplete, decodedCompleteID, err := DecodeRetentionComplete(bytes.NewReader(completeRaw))
	if err != nil || decodedComplete != complete || decodedCompleteID != completeID || completeID == intentID {
		t.Fatalf("completion round trip or domain separation failed: decoded=%#v id=%q err=%v", decodedComplete, decodedCompleteID, err)
	}
}

func TestRetentionIntentRejectsContradictoryBasis(t *testing.T) {
	base := RetentionIntent{
		Schema: RetentionIntentSchemaV1, OperationID: retentionTestOperationID(51),
		OperationRootIdentity: identityFromTestRaw(851).String(), TargetRootIdentity: identityFromTestRaw(852).String(),
		IntentSHA256: retentionTestSHA256ID(52), TerminalEventID: EventID(retentionTestSHA256ID(53)), TerminalPhase: PhaseAbandoned,
		PlanID: strings.Repeat("b", 24), MetafileVariantID: retentionTestSHA256ID(54), Basis: RetentionBasisAbandoned,
	}
	if _, _, err := EncodeRetentionIntent(base); err != nil {
		t.Fatal(err)
	}
	bad := base
	bad.FinalObjectIdentity = identityFromTestRaw(853).String()
	if _, _, err := EncodeRetentionIntent(bad); err == nil {
		t.Fatal("abandoned retention marker carried a final identity")
	}
	bad = base
	bad.TerminalPhase = PhaseCommitted
	bad.Basis = RetentionBasisCommitted
	if _, _, err := EncodeRetentionIntent(bad); err == nil {
		t.Fatal("committed retention marker omitted its final identity")
	}
}

func TestRetentionDecoderRejectsDuplicateUnknownTrailingAndOversizedData(t *testing.T) {
	marker := RetentionIntent{
		Schema: RetentionIntentSchemaV1, OperationID: retentionTestOperationID(61),
		OperationRootIdentity: identityFromTestRaw(861).String(), TargetRootIdentity: identityFromTestRaw(862).String(),
		IntentSHA256: retentionTestSHA256ID(62), TerminalEventID: EventID(retentionTestSHA256ID(63)), TerminalPhase: PhaseAbandoned,
		PlanID: strings.Repeat("c", 24), MetafileVariantID: retentionTestSHA256ID(64), Basis: RetentionBasisAbandoned,
	}
	raw, _, err := EncodeRetentionIntent(marker)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := bytes.Replace(raw, []byte(`"schema":`), []byte(`"schema":"ptctl.materialize-retention-intent/v1","schema":`), 1)
	unknown := bytes.Replace(raw, []byte(`"schema":`), []byte(`"unknown":true,"schema":`), 1)
	for name, candidate := range map[string][]byte{
		"duplicate": duplicate,
		"unknown":   unknown,
		"trailing":  append(bytes.Clone(raw), []byte("{}")...),
		"oversized": bytes.Repeat([]byte{'x'}, int(maxRetentionMarkerBytes)+1),
	} {
		if _, _, err := DecodeRetentionIntent(bytes.NewReader(candidate)); err == nil {
			t.Fatalf("%s retention marker was accepted", name)
		}
	}
}

func identityFromTestRaw(seed uint64) fsbind.Identity {
	identity, err := fsbind.ParseIdentity(fmt.Sprintf("fsbind-v1:%064x", seed))
	if err != nil {
		panic(err)
	}
	return identity
}

func retentionTestSHA256ID(seed uint64) string {
	return fmt.Sprintf("sha256:%064x", seed)
}

func retentionTestOperationID(seed uint64) OperationID {
	return OperationID(retentionTestSHA256ID(seed))
}
