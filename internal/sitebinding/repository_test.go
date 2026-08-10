package sitebinding

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

func TestSealLoadSelectAndJSONRoundTripLosesAuthority(t *testing.T) {
	fixture := newBindingFixture(t, true)
	recordRef, sealed, err := fixture.repository.Seal(context.Background(), fixture.observed, fixture.fetch, fixture.artifact)
	if err != nil || recordRef.ID == "" || sealed.WritesPerformed != 1 || sealed.AlreadyPresent ||
		sealed.Record != recordRef || sealed.Artifact.ID != fixture.artifact.ID || !sealed.Artifact.RequirePrivate ||
		sealed.Store != fixture.store.Info() {
		t.Fatalf("Seal: ref=%+v receipt=%+v err=%v", recordRef, sealed, err)
	}
	repeatedRef, repeated, err := fixture.repository.Seal(context.Background(), fixture.observed, fixture.fetch, fixture.artifact)
	if err != nil || repeatedRef != recordRef || repeated.WritesPerformed != 0 || !repeated.AlreadyPresent {
		t.Fatalf("idempotent Seal: ref=%+v receipt=%+v err=%v", repeatedRef, repeated, err)
	}

	verified, loaded, err := fixture.repository.Load(context.Background(), recordRef.ID)
	if err != nil || !verified.Verified() || loaded.Selected || !loaded.Complete || loaded.Record != recordRef ||
		loaded.Artifact != fixture.artifact || loaded.RecordBytesRead != recordRef.SizeBytes ||
		loaded.ConsumerBytesRead != recordRef.SizeBytes || !verified.Matches(recordRef.ID, fixture.ref, fixture.artifact.ID.String()) {
		t.Fatalf("Load: verified=%v receipt=%+v err=%v", verified, loaded, err)
	}
	public := verified.PublicCopy()
	if public.RecordRef != recordRef || public.Artifact != fixture.artifact || public.Store != fixture.store.Info() ||
		public.Record.SiteID != fixture.ref.SiteID || public.Record.RemoteID != fixture.ref.RemoteID || public.Record.Validate() != nil {
		t.Fatalf("unexpected public copy: %+v", public)
	}
	selected, selection, err := fixture.repository.Select(context.Background(), recordRef.ID, fixture.ref, fixture.artifact.ID.String())
	if err != nil || !selection.Complete || !selection.Selected || !selected.Verified() {
		t.Fatalf("Select: selected=%v receipt=%+v err=%v", selected, selection, err)
	}
	if mismatched, mismatchReceipt, err := fixture.repository.Select(context.Background(), recordRef.ID, domain.TorrentRef{SiteID: fixture.ref.SiteID, RemoteID: "999"}, fixture.artifact.ID.String()); !errors.Is(err, ErrBindingMismatch) || mismatched != nil || !mismatchReceipt.Complete || mismatchReceipt.Selected {
		t.Fatalf("mismatched Select: binding=%v receipt=%+v err=%v", mismatched, mismatchReceipt, err)
	}

	encoded, err := json.Marshal(verified)
	if err != nil {
		t.Fatal(err)
	}
	var replay VerifiedSiteBinding
	if err := json.Unmarshal(encoded, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Verified() || replay.Matches(recordRef.ID, fixture.ref, fixture.artifact.ID.String()) || replay.PublicCopy() != public {
		t.Fatalf("JSON round trip retained or changed authority: %+v", replay.PublicCopy())
	}
}

func TestSealRequiresProcessAuthorityExactReceiptAndSameStore(t *testing.T) {
	fixture := newBindingFixture(t, true)
	assertRejected := func(name string, observed *site.ObservedMetafileBinding, fetch site.MetafileFetchReceipt, artifact metastore.ArtifactRef) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			ref, receipt, err := fixture.repository.Seal(context.Background(), observed, fetch, artifact)
			if !errors.Is(err, ErrInvalidBinding) || ref.ID != "" || receipt.WritesPerformed != 0 {
				t.Fatalf("Seal accepted invalid authority: ref=%+v receipt=%+v err=%v", ref, receipt, err)
			}
		})
	}
	assertRejected("nil", nil, fixture.fetch, fixture.artifact)
	assertRejected("public_copy_cannot_reauthorize", &site.ObservedMetafileBinding{}, fixture.fetch, fixture.artifact)

	mutations := map[string]func(*site.MetafileFetchReceipt){
		"effect": func(value *site.MetafileFetchReceipt) { value.Effect = "different" },
		"ref":    func(value *site.MetafileFetchReceipt) { value.Ref.RemoteID = "999" },
		"origin": func(value *site.MetafileFetchReceipt) { value.Origin = "https://example.invalid" },
		"route":  func(value *site.MetafileFetchReceipt) { value.RouteID = "different" },
		"start": func(value *site.MetafileFetchReceipt) {
			value.ObservedAtStart = value.ObservedAtStart.Add(time.Nanosecond)
		},
		"end":           func(value *site.MetafileFetchReceipt) { value.ObservedAtEnd = value.ObservedAtEnd.Add(time.Nanosecond) },
		"incomplete":    func(value *site.MetafileFetchReceipt) { value.Complete = false },
		"stop":          func(value *site.MetafileFetchReceipt) { value.StopReason = "CANARY-STOP" },
		"requests":      func(value *site.MetafileFetchReceipt) { value.Used.RequestsAttempted = 0 },
		"retries":       func(value *site.MetafileFetchReceipt) { value.Used.AutomaticRetries = 1 },
		"redirects":     func(value *site.MetafileFetchReceipt) { value.Used.RedirectsFollowed = 1 },
		"bytes_unknown": func(value *site.MetafileFetchReceipt) { value.Used.ResponseBytesKnown = false },
		"bytes":         func(value *site.MetafileFetchReceipt) { value.Used.ResponseBytesRead-- },
	}
	for name, mutate := range mutations {
		changed := fixture.fetch
		mutate(&changed)
		assertRejected(name, fixture.observed, changed, fixture.artifact)
	}

	wrongArtifact := fixture.artifact
	wrongArtifact.SizeBytes++
	assertRejected("artifact", fixture.observed, fixture.fetch, wrongArtifact)

	otherStore := newStore(t)
	otherRepository, err := NewRepository(otherStore, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	ref, receipt, err := otherRepository.Seal(context.Background(), fixture.observed, fixture.fetch, fixture.artifact)
	if err == nil || ref.ID != "" || receipt.WritesPerformed != 0 {
		t.Fatalf("cross-store artifact accepted: ref=%+v receipt=%+v err=%v", ref, receipt, err)
	}
	listed, listErr := otherStore.ListRecords(context.Background(), metastore.RecordKindSiteMetafileBindingV1, metastore.DefaultRecordLimits())
	if listErr != nil || len(listed.Records) != 0 {
		t.Fatalf("cross-store failure wrote a record: result=%+v err=%v", listed, listErr)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	ref, receipt, err = fixture.repository.Seal(canceled, fixture.observed, fixture.fetch, fixture.artifact)
	if !errors.Is(err, context.Canceled) || ref.ID != "" || receipt.WritesPerformed != 0 {
		t.Fatalf("pre-cancel Seal: ref=%+v receipt=%+v err=%v", ref, receipt, err)
	}
}

func TestCanonicalRecordFormatIsStrictAndBounded(t *testing.T) {
	fixture := newBindingFixture(t, true)
	record := recordFromObservation(fixture.observed.PublicCopy(), fixture.fetch, fixture.artifact)
	raw, err := EncodeRecord(record, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRecord(bytes.NewReader(raw), DefaultLimits())
	if err != nil || decoded != record {
		t.Fatalf("DecodeRecord: record=%+v err=%v", decoded, err)
	}
	canary := "PRIVATE-UNKNOWN-FIELD-CANARY"
	unknown := append(append([]byte(nil), raw[:len(raw)-2]...), []byte(`,"`+canary+`":"secret"}`+"\n")...)
	duplicate := append([]byte(`{"schema":"`+SchemaV1+`",`), raw[1:]...)
	nonCanonical := append([]byte(" "), raw...)
	tests := map[string][]byte{
		"empty": {}, "missing_newline": raw[:len(raw)-1], "extra_newline": append(append([]byte(nil), raw...), '\n'),
		"unknown": unknown, "duplicate": duplicate, "whitespace": nonCanonical,
	}
	for name, candidate := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeRecord(bytes.NewReader(candidate), DefaultLimits())
			if !errors.Is(err, ErrCorruptBinding) || strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid record result: err=%v", err)
			}
		})
	}
	invalid := record
	invalid.FetchUsage.AutomaticRetries = 1
	if _, err := EncodeRecord(invalid, DefaultLimits()); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("invalid record encoded: %v", err)
	}
	tiny := Limits{MaxRecordBytes: int64(len(raw) - 1)}
	if _, err := EncodeRecord(record, tiny); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("encode byte budget not enforced: %v", err)
	}
	if _, err := DecodeRecord(bytes.NewReader(raw), tiny); !errors.Is(err, ErrCorruptBinding) {
		t.Fatalf("decode byte budget not enforced: %v", err)
	}
}

func TestLoadRejectsMalformedAndUnavailableLinkedArtifact(t *testing.T) {
	fixture := newBindingFixture(t, true)
	canary := "PRIVATE-RECORD-CONTENT-CANARY"
	malformed := []byte(`{"schema":"` + canary + `"}` + "\n")
	malformedRef, _, err := fixture.store.ImportRecord(context.Background(), metastore.RecordKindSiteMetafileBindingV1, bytes.NewReader(malformed), metastore.DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}
	verified, receipt, err := fixture.repository.Load(context.Background(), malformedRef.ID)
	if !errors.Is(err, ErrCorruptBinding) || verified != nil || receipt.Complete || strings.Contains(err.Error(), canary) {
		t.Fatalf("malformed Load: binding=%v receipt=%+v err=%v", verified, receipt, err)
	}

	record := recordFromObservation(fixture.observed.PublicCopy(), fixture.fetch, fixture.artifact)
	record.ArtifactID = metastore.ArtifactID("sha256:" + strings.Repeat("1", 64))
	record.MetafileVariantID = record.ArtifactID.String()
	canonical, err := EncodeRecord(record, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	missingRef, _, err := fixture.store.ImportRecord(context.Background(), metastore.RecordKindSiteMetafileBindingV1, bytes.NewReader(canonical), metastore.DefaultRecordLimits())
	if err != nil {
		t.Fatal(err)
	}
	verified, receipt, err = fixture.repository.Load(context.Background(), missingRef.ID)
	if err == nil || verified != nil || receipt.Complete {
		t.Fatalf("missing linked artifact accepted: binding=%v receipt=%+v err=%v", verified, receipt, err)
	}
}

func TestLoadClassifiesPublicLinkedArtifactAsCorruptArtifact(t *testing.T) {
	fixture := newBindingFixture(t, false)
	record := recordFromObservation(fixture.observed.PublicCopy(), fixture.fetch, fixture.artifact)
	canonical, err := EncodeRecord(record, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	recordRef, _, err := fixture.store.ImportRecord(
		context.Background(), metastore.RecordKindSiteMetafileBindingV1,
		bytes.NewReader(canonical), metastore.DefaultRecordLimits(),
	)
	if err != nil {
		t.Fatal(err)
	}
	verified, receipt, err := fixture.repository.Load(context.Background(), recordRef.ID)
	if !errors.Is(err, metastore.ErrCorruptArtifact) || verified != nil || receipt.Complete {
		t.Fatalf("public linked artifact classification: binding=%v receipt=%+v err=%v", verified, receipt, err)
	}
}

func TestExplicitRecordIDsDoNotImplyLatestSelection(t *testing.T) {
	fixture := newBindingFixture(t, true)
	first, _, err := fixture.repository.Seal(context.Background(), fixture.observed, fixture.fetch, fixture.artifact)
	if err != nil {
		t.Fatal(err)
	}
	secondStart := fixture.fetch.ObservedAtStart.Add(time.Hour)
	secondEnd := fixture.fetch.ObservedAtEnd.Add(time.Hour)
	fetched, err := site.NewFetchedMetafile(fixture.ref, fixture.fetch.Origin, fixture.fetch.RouteID, secondStart, secondEnd, fixture.raw)
	if err != nil {
		t.Fatal(err)
	}
	secondFetch := fixture.fetch
	secondFetch.ObservedAtStart = secondStart
	secondFetch.ObservedAtEnd = secondEnd
	secondObserved, err := fetched.BindImported(fixture.artifact.MetafileVariantID, fixture.artifact.SizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := fixture.repository.Seal(context.Background(), secondObserved, secondFetch, fixture.artifact)
	if err != nil || second.ID == first.ID {
		t.Fatalf("second explicit record: first=%s second=%s err=%v", first.ID, second.ID, err)
	}
	firstBinding, _, err := fixture.repository.Load(context.Background(), first.ID)
	if err != nil || !firstBinding.PublicCopy().Record.ObservedAtStart.Equal(fixture.fetch.ObservedAtStart) {
		t.Fatalf("explicit first load selected another record: %v", err)
	}
}

type bindingFixture struct {
	store      *metastore.Store
	repository *Repository
	raw        []byte
	ref        domain.TorrentRef
	artifact   metastore.ArtifactRef
	fetch      site.MetafileFetchReceipt
	observed   *site.ObservedMetafileBinding
}

func newBindingFixture(t *testing.T, private bool) bindingFixture {
	t.Helper()
	store := newStore(t)
	repository, err := NewRepository(store, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	raw := bindingTestMetafile(private)
	_, artifact, imported, err := store.Import(context.Background(), bytes.NewReader(raw), metastore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.TorrentRef{SiteID: "tjupt", RemoteID: "12345"}
	start := time.Date(2026, 8, 10, 2, 3, 4, 123456789, time.UTC)
	end := start.Add(200 * time.Millisecond)
	fetched, err := site.NewFetchedMetafile(ref, "https://www.tjupt.org", "torrent.download.v1", start, end, raw)
	if err != nil {
		t.Fatal(err)
	}
	limits := site.DefaultMetafileFetchLimits()
	fetch := site.MetafileFetchReceipt{
		Effect: site.MetafileFetchEffect, Ref: ref, Origin: "https://www.tjupt.org", RouteID: "torrent.download.v1",
		ObservedAtStart: start, ObservedAtEnd: end, Complete: true, Limits: limits,
		Used: site.MetafileFetchUsage{RequestsAttempted: 1, ResponseBytesRead: int64(len(raw)), ResponseBytesKnown: true},
	}
	observed, err := fetched.BindImported(artifact.MetafileVariantID, imported.BytesConsumed)
	if err != nil || !observed.MatchesReceipt(fetch) {
		t.Fatalf("fixture binding: observed=%v err=%v", observed, err)
	}
	return bindingFixture{store: store, repository: repository, raw: raw, ref: ref, artifact: artifact, fetch: fetch, observed: observed}
}

func newStore(t *testing.T) *metastore.Store {
	t.Helper()
	physical, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(physical, "store")
	store, _, err := metastore.Init(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("store retained a resource handle: %v", err)
		}
	})
	return store
}

func bindingTestMetafile(private bool) []byte {
	content := []byte("binding fixture content")
	piece := sha1.Sum(content)
	info := map[string]any{
		"length": int64(len(content)), "name": "binding.bin", "piece length": int64(len(content)), "pieces": piece[:],
	}
	if private {
		info["private"] = int64(1)
	}
	return bindingTestBencode(map[string]any{
		"announce": "https://tracker.invalid/announce?passkey=PRIVATE-PASSKEY-CANARY", "info": info,
	})
}

func bindingTestBencode(value any) []byte {
	var output bytes.Buffer
	var encode func(any)
	encode = func(current any) {
		switch typed := current.(type) {
		case string:
			output.WriteString(strconv.Itoa(len(typed)))
			output.WriteByte(':')
			output.WriteString(typed)
		case []byte:
			output.WriteString(strconv.Itoa(len(typed)))
			output.WriteByte(':')
			output.Write(typed)
		case int64:
			output.WriteByte('i')
			output.WriteString(strconv.FormatInt(typed, 10))
			output.WriteByte('e')
		case map[string]any:
			output.WriteByte('d')
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				encode(key)
				encode(typed[key])
			}
			output.WriteByte('e')
		default:
			panic(fmt.Sprintf("unsupported fixture type %T", current))
		}
	}
	encode(value)
	return output.Bytes()
}
