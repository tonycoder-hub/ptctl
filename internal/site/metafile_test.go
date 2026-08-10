package site_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

type externalFetcher struct{}

func (externalFetcher) ValidateMetafileRef(domain.TorrentRef) error { return nil }
func (externalFetcher) MetafileFetchConfig() (site.MetafileFetchConfig, error) {
	return site.NewMetafileFetchConfig("https://fake.example", "fake.download.v1")
}
func (externalFetcher) OpenMetafileFetchSession(context.Context, site.Credential) (site.MetafileFetchSession, error) {
	return nil, nil
}

var _ site.MetafileFetcher = externalFetcher{}

func TestDefaultMetafileFetchLimitsAreBounded(t *testing.T) {
	limits := site.DefaultMetafileFetchLimits()
	if limits.MaxRequests != 1 || limits.MaxResponseBytes != 8<<20 || limits.MaxResponseHeaderBytes != 64<<10 {
		t.Fatalf("unexpected defaults: %#v", limits)
	}
	if err := limits.Validate(); err != nil {
		t.Fatalf("default limits: %v", err)
	}
	for _, invalid := range []site.MetafileFetchLimits{
		{},
		{MaxRequests: 2, MaxResponseBytes: 1, MaxResponseHeaderBytes: 1},
		{MaxRequests: 1, MaxResponseBytes: 0, MaxResponseHeaderBytes: 1},
		{MaxRequests: 1, MaxResponseBytes: (32 << 20) + 1, MaxResponseHeaderBytes: 1},
		{MaxRequests: 1, MaxResponseBytes: 1, MaxResponseHeaderBytes: (64 << 10) + 1},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid limits accepted: %#v", invalid)
		}
	}
}

func TestMetafileFetchConfigIsCanonicalAndCredentialFree(t *testing.T) {
	config, err := site.NewMetafileFetchConfig("https://fake.example", "fake.download.v1")
	if err != nil || config.Origin != "https://fake.example" || config.RouteID != "fake.download.v1" || config.Validate() != nil {
		t.Fatalf("config=%#v err=%v", config, err)
	}
	for _, invalid := range []site.MetafileFetchConfig{
		{},
		{Origin: "http://fake.example", RouteID: "fake.download.v1"},
		{Origin: "https://fake.example/", RouteID: "fake.download.v1"},
		{Origin: "https://fake.example", RouteID: "cookie=value"},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid config accepted: %#v", invalid)
		}
	}
}

func TestFetchedMetafileIsOpaqueImmutableAndExactlyBindable(t *testing.T) {
	const canary = "CANARY-PRIVATE-ANNOUNCE-PASSKEY"
	raw := []byte("d8:announce35:https://tracker.invalid/" + canary + "e")
	original := append([]byte(nil), raw...)
	ref := domain.TorrentRef{SiteID: "fake", RemoteID: "42"}
	start := time.Date(2026, 8, 10, 1, 2, 3, 0, time.FixedZone("test", 8*60*60))
	end := start.Add(time.Second)
	fetched, err := site.NewFetchedMetafile(ref, "https://fake.example", "fake.download.v1", start, end, raw)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = 'x'
	reader, err := fetched.Reader()
	if err != nil {
		t.Fatal(err)
	}
	if _, exposesBackingSlice := reader.(io.WriterTo); exposesBackingSlice {
		t.Fatal("opaque reader exposed a WriterTo path to its private backing slice")
	}
	readerJSON, _ := json.Marshal(reader)
	if strings.Contains(fmt.Sprintf("%v %#v", reader, reader), canary) || strings.Contains(string(readerJSON), canary) {
		t.Fatal("opaque reader formatting disclosed response content")
	}
	readBack, err := io.ReadAll(reader)
	if err != nil || string(readBack) != string(original) {
		t.Fatalf("reader changed with caller buffer: %q err=%v", readBack, err)
	}
	if fetched.SizeBytes() != int64(len(original)) {
		t.Fatalf("size=%d", fetched.SizeBytes())
	}
	receipt := site.MetafileFetchReceipt{
		Effect:          site.MetafileFetchEffect,
		Ref:             ref,
		Origin:          "https://fake.example",
		RouteID:         "fake.download.v1",
		ObservedAtStart: start.UTC(),
		ObservedAtEnd:   end.UTC(),
		Complete:        true,
		Limits:          site.DefaultMetafileFetchLimits(),
		Used: site.MetafileFetchUsage{
			RequestsAttempted:  1,
			ResponseBytesRead:  int64(len(original)),
			ResponseBytesKnown: true,
		},
	}
	if !fetched.MatchesReceipt(receipt) {
		t.Fatal("exact receipt did not match private response authority")
	}
	changed := receipt
	changed.RouteID = "fake.other.v1"
	if fetched.MatchesReceipt(changed) {
		t.Fatal("route-mismatched receipt was accepted")
	}
	changed = receipt
	changed.Used.AutomaticRetries = 1
	if fetched.MatchesReceipt(changed) {
		t.Fatal("retried receipt was accepted")
	}
	changed = receipt
	changed.Used.ResponseBytesRead--
	if fetched.MatchesReceipt(changed) {
		t.Fatal("partial receipt was accepted")
	}

	encoded, err := json.Marshal(fetched)
	if err != nil || strings.Contains(string(encoded), canary) || string(encoded) != `"[REDACTED_PRIVATE_METAFILE]"` {
		t.Fatalf("opaque JSON=%s err=%v", encoded, err)
	}
	encodedValue, _ := json.Marshal(*fetched)
	if strings.Contains(fetched.String(), canary) || strings.Contains(fetched.GoString(), canary) ||
		strings.Contains(fmt.Sprintf("%v %#v", *fetched, *fetched), canary) || strings.Contains(string(encodedValue), canary) {
		t.Fatal("opaque stringer disclosed content")
	}

	digest := sha256.Sum256(original)
	variantID := "sha256:" + hex.EncodeToString(digest[:])
	if _, err := fetched.BindImported(variantID, int64(len(original)-1)); err == nil {
		t.Fatal("partial import created a binding")
	}
	if _, err := fetched.BindImported("sha256:"+strings.Repeat("0", 64), int64(len(original))); err == nil {
		t.Fatal("different variant created a binding")
	}
	binding, err := fetched.BindImported(variantID, int64(len(original)))
	if err != nil {
		t.Fatal(err)
	}
	observation := binding.PublicCopy()
	if observation.Ref != ref || observation.Origin != "https://fake.example" || observation.RouteID != "fake.download.v1" ||
		observation.MetafileVariantID != variantID || observation.Basis != site.MetafileObservationExactFetchBasis ||
		observation.ResponseBytes != int64(len(original)) || !observation.ObservedAtStart.Equal(start) || !observation.ObservedAtEnd.Equal(end) {
		t.Fatalf("unexpected observation: %#v", observation)
	}
	if !binding.Matches(ref, observation.Origin, observation.RouteID, variantID) ||
		binding.Matches(ref, "https://other.example", observation.RouteID, variantID) ||
		binding.Matches(ref, observation.Origin, "other.route", variantID) ||
		binding.Matches(domain.TorrentRef{SiteID: "fake", RemoteID: "43"}, observation.Origin, observation.RouteID, variantID) {
		t.Fatal("binding match did not cover all observed authority fields")
	}
	bindingJSON, _ := json.Marshal(binding)
	bindingValueJSON, _ := json.Marshal(*binding)
	if strings.Contains(string(bindingJSON), canary) || strings.Contains(string(bindingValueJSON), canary) ||
		strings.Contains(fmt.Sprintf("%v %#v", *binding, *binding), canary) || string(bindingJSON) != `"[REDACTED_METAFILE_BINDING]"` {
		t.Fatalf("opaque binding JSON=%s", bindingJSON)
	}
	publicJSON, _ := json.Marshal(observation)
	if strings.Contains(string(publicJSON), canary) {
		t.Fatal("public observation disclosed response content")
	}
}

func TestFetchedMetafileConstructorRejectsUntrustedMetadata(t *testing.T) {
	validRef := domain.TorrentRef{SiteID: "fake", RemoteID: "1"}
	start := time.Now().UTC()
	tests := []struct {
		name   string
		ref    domain.TorrentRef
		origin string
		route  string
		start  time.Time
		end    time.Time
		raw    []byte
	}{
		{name: "empty ref", ref: domain.TorrentRef{}, origin: "https://fake.example", route: "fake.v1", start: start, end: start, raw: []byte("x")},
		{name: "control ref", ref: domain.TorrentRef{SiteID: "fake", RemoteID: "1\n"}, origin: "https://fake.example", route: "fake.v1", start: start, end: start, raw: []byte("x")},
		{name: "origin path", ref: validRef, origin: "https://fake.example/", route: "fake.v1", start: start, end: start, raw: []byte("x")},
		{name: "origin user", ref: validRef, origin: "https://user@fake.example", route: "fake.v1", start: start, end: start, raw: []byte("x")},
		{name: "origin uppercase", ref: validRef, origin: "https://FAKE.example", route: "fake.v1", start: start, end: start, raw: []byte("x")},
		{name: "default port", ref: validRef, origin: "https://fake.example:443", route: "fake.v1", start: start, end: start, raw: []byte("x")},
		{name: "route", ref: validRef, origin: "https://fake.example", route: "Fake/Route", start: start, end: start, raw: []byte("x")},
		{name: "zero time", ref: validRef, origin: "https://fake.example", route: "fake.v1", start: time.Time{}, end: start, raw: []byte("x")},
		{name: "reversed time", ref: validRef, origin: "https://fake.example", route: "fake.v1", start: start, end: start.Add(-time.Second), raw: []byte("x")},
		{name: "empty response", ref: validRef, origin: "https://fake.example", route: "fake.v1", start: start, end: start, raw: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := site.NewFetchedMetafile(test.ref, test.origin, test.route, test.start, test.end, test.raw); err == nil {
				t.Fatal("invalid constructor input accepted")
			}
		})
	}
}
