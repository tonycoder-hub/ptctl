package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

// torrentListAdapter is an API-key site that lists the account's own torrents.
type torrentListAdapter struct {
	apiKeySiteAdapter
	list        domain.AccountTorrentList
	query       site.AccountTorrentQuery
	validateErr error
	validated   []site.AccountTorrentQuery
}

func (*torrentListAdapter) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID: "api-key-test", Name: "API key test", BaseURL: "https://api.invalid/", Stability: "test",
		AuthMethods:  []domain.AuthMethod{domain.AuthMethodAPIKeyHeader},
		Capabilities: []domain.Capability{domain.CapabilityAuthCheck, domain.CapabilityAccountTorrents},
	}
}

func (adapter *torrentListAdapter) ValidateAccountTorrentQuery(query site.AccountTorrentQuery) error {
	adapter.validated = append(adapter.validated, query)
	return adapter.validateErr
}

func (adapter *torrentListAdapter) AccountTorrents(_ context.Context, credential site.Credential, query site.AccountTorrentQuery) (domain.AccountTorrentList, error) {
	adapter.record(credential)
	adapter.query = query
	return adapter.list, adapter.err
}

func sampleAccountTorrents() domain.AccountTorrentList {
	size, uploaded, downloaded, seed := int64(1<<30), int64(2048), int64(0), int64(93784)
	last := time.Date(2026, 10, 16, 4, 0, 0, 0, time.UTC)
	total, pages := 51, 2
	return domain.AccountTorrentList{
		SiteID: "api-key-test", State: domain.AccountTorrentsCompleted, Page: 2, TotalPages: &pages, Total: &total,
		Torrents: []domain.AccountTorrent{
			{Torrent: domain.TorrentSummary{Ref: domain.TorrentRef{SiteID: "api-key-test", RemoteID: "9001"}, Name: "Seeding canary", SizeBytes: &size},
				TrafficScope: domain.AccountTrafficTotal, UploadedBytes: &uploaded, DownloadedBytes: &downloaded, SeedTimeSeconds: &seed, LastActionAt: &last},
			{Torrent: domain.TorrentSummary{Ref: domain.TorrentRef{SiteID: "api-key-test", RemoteID: "9002"}, Name: "Bare\x1b[31m canary"}},
			{Torrent: domain.TorrentSummary{Ref: domain.TorrentRef{SiteID: "api-key-test", RemoteID: "9003"}, Name: "Session canary"},
				TrafficScope: domain.AccountTrafficCurrentSession, UploadedBytes: &uploaded},
		},
		ObservedAt: time.Date(2026, 10, 16, 5, 0, 0, 0, time.UTC),
	}
}

func TestSiteTorrentsReadsTheRequestedList(t *testing.T) {
	useTempConfigDir(t)
	adapter := &torrentListAdapter{list: sampleAccountTorrents()}
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader(apiKeyCanary + "\n"), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.siteRead("torrents", []string{"--api-key-stdin", "--state", "completed", "--page", "2", "api-key-test"}); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 1 || adapter.secret != apiKeyCanary || adapter.method != domain.AuthMethodAPIKeyHeader ||
		adapter.query != (site.AccountTorrentQuery{State: domain.AccountTorrentsCompleted, Page: 2}) || len(adapter.validated) != 1 {
		t.Fatalf("calls=%d query=%#v validated=%#v", adapter.calls, adapter.query, adapter.validated)
	}
	want := "" +
		"STATE              completed\n" +
		"PAGE               2 of 2\n" +
		"TOTAL              51\n" +
		"REF                NAME                 SIZE      TRAFFIC  UPLOADED  DOWNLOADED  SEED TIME  LAST ACTION\n" +
		"api-key-test/9001  Seeding canary       1.00 GiB  total    2.00 KiB  0 B         1d 2h 3m   2026-10-16T04:00:00Z\n" +
		"api-key-test/9002  Bare\\x1b[31m canary  unknown   -        -         -           -          -\n" +
		"api-key-test/9003  Session canary       unknown   session  2.00 KiB  -           -          -\n"
	if got := out.String(); got != want {
		t.Fatalf("table drifted:\n%s\nwant:\n%s", got, want)
	}

	// With the saved key and no flags, the seeding list's first page is read.
	writeSavedCredential(t, "api-key-test", domain.AuthMethodAPIKeyHeader, savedAPIKeyCanary+"\n")
	adapter = &torrentListAdapter{list: sampleAccountTorrents()}
	stdin := &trackingReader{}
	out.Reset()
	a = &app{stdin: stdin, stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.siteRead("torrents", []string{"--output", "json", "api-key-test"}); err != nil {
		t.Fatal(err)
	}
	if stdin.read || adapter.secret != savedAPIKeyCanary || adapter.query != (site.AccountTorrentQuery{State: domain.AccountTorrentsSeeding}) {
		t.Fatalf("read=%t query=%#v", stdin.read, adapter.query)
	}
	var envelope struct {
		Schema string                    `json:"schema"`
		Kind   string                    `json:"kind"`
		Data   domain.AccountTorrentList `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Schema != "ptctl.dev/v1" || envelope.Kind != "site.account.torrent.list" || len(envelope.Data.Torrents) != 3 ||
		envelope.Data.Torrents[2].TrafficScope != domain.AccountTrafficCurrentSession ||
		envelope.Data.Torrents[0].SeedTimeSeconds == nil || *envelope.Data.Torrents[0].SeedTimeSeconds != 93784 ||
		strings.Contains(out.String(), "CANARY") {
		t.Fatalf("json=%s", out.String())
	}
}

func TestSiteTorrentsRefusesBadInputBeforeStdin(t *testing.T) {
	useTempConfigDir(t)
	for _, test := range []struct {
		name    string
		adapter site.Adapter
		args    []string
		want    string
	}{
		{name: "unknown state", adapter: &torrentListAdapter{}, args: []string{"--state", "seek", "api-key-test"},
			want: `site torrents: invalid value "seek" for flag -state: state must be seeding, leeching, completed, incomplete, or uploaded`},
		{name: "page zero", adapter: &torrentListAdapter{}, args: []string{"--page", "0", "api-key-test"}, want: "--page must be 1 or more"},
		{name: "site refuses the query", adapter: &torrentListAdapter{validateErr: errors.New("page must be from 1 to 9")},
			args: []string{"--page", "10", "api-key-test"}, want: "page must be from 1 to 9"},
		{name: "site without the capability", adapter: &apiKeySiteAdapter{}, args: []string{"api-key-test"},
			want: `site "api-key-test" does not declare capability "account.torrents.read"`},
		{name: "search flag", adapter: &torrentListAdapter{}, args: []string{"--free", "api-key-test"},
			want: "site torrents: flag provided but not defined: -free"},
		{name: "extra argument", adapter: &torrentListAdapter{}, args: []string{"api-key-test", "seeding"},
			want: "site torrents requires exactly one SITE"},
		{name: "nothing saved", adapter: &torrentListAdapter{}, args: []string{"api-key-test"},
			want: `no saved API key for site "api-key-test"; run 'ptctl site login api-key-test' first, or pipe one with --api-key-stdin`},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &trackingReader{}
			var out bytes.Buffer
			a := &app{stdin: reader, stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(test.adapter)}
			var usage *usageErr
			if err := a.siteRead("torrents", test.args); !errors.As(err, &usage) || err.Error() != test.want || reader.read || out.Len() != 0 {
				t.Fatalf("err=%v read=%t stdout=%q", err, reader.read, out.String())
			}
			if lister, ok := test.adapter.(*torrentListAdapter); ok && lister.calls != 0 {
				t.Fatalf("site read %d times", lister.calls)
			}
		})
	}
}

func TestSiteTorrentsKeepsPublicErrorsSecretFree(t *testing.T) {
	useTempConfigDir(t)
	for _, cause := range []error{site.ErrCredentialRejected, site.ErrRateLimited, errors.New("CANARY server text")} {
		adapter := &torrentListAdapter{apiKeySiteAdapter: apiKeySiteAdapter{err: fmt.Errorf("wrapped: %w", cause)}}
		var out bytes.Buffer
		a := &app{stdin: strings.NewReader(apiKeyCanary), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
		err := a.siteRead("torrents", []string{"--api-key-stdin", "api-key-test"})
		want := "site torrents failed"
		if errors.Is(cause, site.ErrCredentialRejected) || errors.Is(cause, site.ErrRateLimited) {
			want = fmt.Sprintf("site torrents failed: %v", cause)
		}
		if err == nil || err.Error() != want || out.Len() != 0 {
			t.Fatalf("cause=%v err=%v stdout=%q", cause, err, out.String())
		}
	}
}

func TestSeedTimeRendersDaysHoursAndMinutes(t *testing.T) {
	for _, test := range []struct {
		seconds *int64
		want    string
	}{
		{nil, "-"},
		{int64Pointer(0), "0m"},
		{int64Pointer(59), "0m"},
		{int64Pointer(3599), "59m"},
		{int64Pointer(3600), "1h 0m"},
		{int64Pointer(86399), "23h 59m"},
		{int64Pointer(86400), "1d 0h 0m"},
		{int64Pointer(400 * 86400), "400d 0h 0m"},
	} {
		if got := seedTime(test.seconds); got != test.want {
			t.Fatalf("seedTime(%v)=%q want %q", test.seconds, got, test.want)
		}
	}
}

func int64Pointer(value int64) *int64 { return &value }

func TestSiteTorrentsShowsAnEmptyList(t *testing.T) {
	useTempConfigDir(t)
	zero := 0
	adapter := &torrentListAdapter{list: domain.AccountTorrentList{SiteID: "api-key-test", State: domain.AccountTorrentsSeeding, Page: 1,
		TotalPages: &zero, Total: &zero, Torrents: []domain.AccountTorrent{}}}
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader(apiKeyCanary), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.siteRead("torrents", []string{"--api-key-stdin", "api-key-test"}); err != nil {
		t.Fatal(err)
	}
	want := "" +
		"STATE  seeding\n" +
		"PAGE   1\n" +
		"TOTAL  0\n" +
		"REF    NAME  SIZE  TRAFFIC  UPLOADED  DOWNLOADED  SEED TIME  LAST ACTION\n"
	if got := out.String(); got != want {
		t.Fatalf("table drifted:\n%s\nwant:\n%s", got, want)
	}
}
