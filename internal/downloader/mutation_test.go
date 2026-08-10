package downloader

import (
	"strings"
	"testing"
	"time"
)

func TestAssessLedgerIdentityFailClosedMatrix(t *testing.T) {
	const (
		v1      = "1111111111111111111111111111111111111111"
		otherV1 = "2222222222222222222222222222222222222222"
		v2      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		otherV2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	valid := func(key, hashV1, hashV2 string) Torrent {
		return Torrent{Hash: key, InfoHashV1: hashV1, InfoHashV2: hashV2, IdentityStatus: IdentityStatusValid}
	}
	tests := []struct {
		name     string
		identity TypedIdentity
		jobs     []Torrent
		want     LedgerIdentityStatus
		count    int
	}{
		{name: "v1 absent among unrelated typed jobs", identity: TypedIdentity{InfoHashV1: v1}, jobs: []Torrent{valid("other", otherV1, "")}, want: LedgerIdentityAbsent},
		{name: "v1 exact", identity: TypedIdentity{InfoHashV1: v1}, jobs: []Torrent{valid("exact", v1, "")}, want: LedgerIdentityExactUnique, count: 1},
		{name: "duplicate exact jobs ambiguous", identity: TypedIdentity{InfoHashV1: v1}, jobs: []Torrent{valid("a", v1, ""), valid("b", v1, "")}, want: LedgerIdentityAmbiguous, count: 2},
		{name: "hybrid requires both families", identity: TypedIdentity{InfoHashV1: v1, InfoHashV2: v2}, jobs: []Torrent{valid("partial", v1, "")}, want: LedgerIdentityIncomplete},
		{name: "hybrid family disagreement conflicts", identity: TypedIdentity{InfoHashV1: v1, InfoHashV2: v2}, jobs: []Torrent{valid("conflict", v1, otherV2)}, want: LedgerIdentityConflict},
		{name: "hybrid exact", identity: TypedIdentity{InfoHashV1: v1, InfoHashV2: v2}, jobs: []Torrent{valid("exact", v1, v2)}, want: LedgerIdentityExactUnique, count: 1},
		{name: "pure v2 exact", identity: TypedIdentity{InfoHashV2: v2}, jobs: []Torrent{valid("exact", "", v2)}, want: LedgerIdentityExactUnique, count: 1},
		{name: "opaque generic hash never proves identity", identity: TypedIdentity{InfoHashV2: v2}, jobs: []Torrent{{Hash: strings.Repeat("a", 40), IdentityStatus: IdentityStatusUnavailable}}, want: LedgerIdentityIncomplete},
		{name: "unavailable unrelated row blocks absence", identity: TypedIdentity{InfoHashV1: v1}, jobs: []Torrent{{Hash: "unknown", IdentityStatus: IdentityStatusUnavailable}}, want: LedgerIdentityIncomplete},
		{name: "invalid unrelated row blocks absence", identity: TypedIdentity{InfoHashV1: v1}, jobs: []Torrent{{Hash: "invalid", IdentityStatus: IdentityStatusInvalid}}, want: LedgerIdentityIncomplete},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			snapshot := LedgerSnapshot{
				Driver: "test", ObservedAtStart: now, ObservedAtEnd: now.Add(time.Millisecond), Complete: true,
				Capabilities: LedgerCapabilities{TypedInfoHashes: true}, Jobs: test.jobs,
			}
			got, err := AssessLedgerIdentity(snapshot, test.identity)
			if err != nil || got.Status != test.want || got.ExactJobCount != test.count {
				t.Fatalf("assessment=%#v err=%v", got, err)
			}
			if test.want == LedgerIdentityExactUnique && got.ExactJob == nil {
				t.Fatal("unique exact result omitted the process-local job")
			}
			if test.want != LedgerIdentityExactUnique && got.ExactJob != nil {
				t.Fatal("non-unique result retained an exact job authority")
			}
		})
	}
}

func TestAssessLedgerIdentityRejectsContradictoryOrIncompleteSnapshots(t *testing.T) {
	now := time.Now().UTC()
	identity := TypedIdentity{InfoHashV1: "1111111111111111111111111111111111111111"}
	tests := []LedgerSnapshot{
		{Driver: "test", ObservedAtStart: now, ObservedAtEnd: now, Complete: false, Capabilities: LedgerCapabilities{TypedInfoHashes: true}},
		{Driver: "test", ObservedAtStart: now, ObservedAtEnd: now, Complete: true},
		{Driver: "test", ObservedAtStart: now, ObservedAtEnd: now, Complete: true, Capabilities: LedgerCapabilities{TypedInfoHashes: true}, Jobs: []Torrent{{Hash: "x", InfoHashV1: identity.InfoHashV1, IdentityStatus: IdentityStatusUnavailable}}},
		{Driver: "test", ObservedAtStart: now, ObservedAtEnd: now, Complete: true, Capabilities: LedgerCapabilities{TypedInfoHashes: true}, Jobs: []Torrent{{Hash: "same", InfoHashV1: identity.InfoHashV1, IdentityStatus: IdentityStatusValid}, {Hash: "same", InfoHashV1: identity.InfoHashV1, IdentityStatus: IdentityStatusValid}}},
	}
	for index, snapshot := range tests {
		result, err := AssessLedgerIdentity(snapshot, identity)
		if err == nil || result.Status != LedgerIdentityIncomplete || result.ExactJob != nil {
			t.Fatalf("case %d accepted invalid snapshot: %#v err=%v", index, result, err)
		}
	}
}
