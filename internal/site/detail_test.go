package site

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
)

func TestTorrentDetailLimitsAndConfig(t *testing.T) {
	limits := DefaultTorrentDetailLimits()
	if limits.MaxRequests != 1 || limits.MaxResponseBytes != 4<<20 || limits.MaxResponseHeaderBytes != 64<<10 || limits.Validate() != nil {
		t.Fatalf("defaults=%#v", limits)
	}
	for _, invalid := range []TorrentDetailLimits{
		{},
		{MaxRequests: 2, MaxResponseBytes: 1, MaxResponseHeaderBytes: 1},
		{MaxRequests: 1, MaxResponseBytes: 8<<20 + 1, MaxResponseHeaderBytes: 1},
		{MaxRequests: 1, MaxResponseBytes: 1, MaxResponseHeaderBytes: 64<<10 + 1},
	} {
		if invalid.Validate() == nil {
			t.Fatalf("invalid limits accepted: %#v", invalid)
		}
	}
	config, err := NewTorrentDetailConfig("https://example.test", "example.details_by_id.v1")
	if err != nil || config.Validate() != nil {
		t.Fatalf("config=%#v err=%v", config, err)
	}
	for _, invalid := range [][2]string{
		{"http://example.test", "example.details.v1"},
		{"https://user@example.test", "example.details.v1"},
		{"https://example.test/path", "example.details.v1"},
		{"https://example.test", "bad route"},
	} {
		if _, err := NewTorrentDetailConfig(invalid[0], invalid[1]); err == nil {
			t.Fatalf("invalid config accepted: %#v", invalid)
		}
	}
}

func TestObservedTorrentDetailAuthorityIsProcessLocal(t *testing.T) {
	ref := domain.TorrentRef{SiteID: "example", RemoteID: "42"}
	start := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	receipt := TorrentDetailReceipt{
		Effect: TorrentDetailReadEffect, Ref: ref, Origin: "https://example.test", RouteID: "example.details_by_id.v1",
		ObservedAtStart: start, ObservedAtEnd: start.Add(time.Second), Complete: true,
		Limits: DefaultTorrentDetailLimits(),
		Used:   TorrentDetailUsage{RequestsAttempted: 1, ResponseBytesRead: 123, ResponseBytesKnown: true},
	}
	detail := domain.TorrentDetail{
		Ref: ref, DisplayTitle: "Release", DownloadReferenceObserved: true,
		EvidenceBasis: []string{DetailBasisAuthenticated, DetailBasisExactRef, DetailBasisOneRequest, DetailBasisSiteClaimOnly, DetailBasisDownloadRef},
	}
	observed, err := NewObservedTorrentDetail(detail, receipt)
	if err != nil || !observed.Verified() || !observed.MatchesReceipt(receipt) || !observed.Matches(ref, receipt.Origin, receipt.RouteID) {
		t.Fatalf("observed=%#v err=%v", observed, err)
	}
	public := observed.PublicCopy()
	public.EvidenceBasis[0] = "mutated"
	if observed.PublicCopy().EvidenceBasis[0] != DetailBasisAuthenticated {
		t.Fatal("public copy mutated opaque authority")
	}
	raw, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	var replay ObservedTorrentDetail
	if err := json.Unmarshal(raw, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Verified() || replay.MatchesReceipt(receipt) || replay.PublicCopy().DisplayTitle != "Release" {
		t.Fatalf("serialized replay recovered authority: %#v", replay)
	}
}
