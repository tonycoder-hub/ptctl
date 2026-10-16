package mteam

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

// userTorrentsFixture is synthetic. It mirrors the shape of
// api/member/getUserTorrentList: a seeding row with a peer, a completed row
// with a snatch record, a row with both whose traffic must not mix, and a
// repeated torrent. PEER-SECRET marks peer fields that must never be decoded.
const userTorrentsFixture = `{"code":"0","message":"SUCCESS","data":{"pageNumber":"1","pageSize":"50","total":"3","totalPages":"1","data":[
	{"torrent":{"id":"9001","name":"Seeding CANARY","size":"1073741824","category":"401",
		"status":{"discount":"FREE","discountEndTime":"2026-10-20 08:00:00","seeders":"12","leechers":"3","timesCompleted":"40"}},
	 "peer":{"ip":"PEER-SECRET-IP","ipv6":"PEER-SECRET-IPV6","peerId":"PEER-SECRET-ID","agent":"PEER-SECRET-AGENT",
		"uploaded":"2048","downloaded":"0","lastAction":"2026-10-16 12:00:00","left":null},
	 "snatched":null,"seek":null},
	{"torrent":{"id":"9002","name":"Completed CANARY","size":"4096"},
	 "peer":null,
	 "snatched":{"uploaded":"999","uploadedReal":"8192","download":"0","downloadedReal":"4096","seedtime":"3600","leechtime":"60",
		"lastAction":"2026-10-15 20:00:00","userid":"100001"},
	 "seek":null},
	{"torrent":{"id":"9003","name":"Both CANARY","size":"10"},
	 "peer":{"ip":"PEER-SECRET-IP","uploaded":"1","downloaded":"2","lastAction":"2026-10-16 01:00:00"},
	 "snatched":{"uploadedReal":"30","seedtime":"5"}},
	{"torrent":{"id":"9001","name":"Repeated CANARY","size":"1"},"peer":null,"snatched":null}
]}}`

func utc(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func int64Value(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func TestAccountTorrentsReadsProfileThenListOnOneClient(t *testing.T) {
	client := &scriptedClient{steps: steps(jsonResponse(profileFixture), jsonResponse(userTorrentsFixture))}
	adapter, record := scriptedAdapter(client)
	list, err := adapter.AccountTorrents(context.Background(), apiKey(t), site.AccountTorrentQuery{State: domain.AccountTorrentsSeeding})
	if err != nil {
		t.Fatal(err)
	}
	if record.opened != 1 || record.baseURL != DefaultBaseURL || record.apiKey != testAPIKey || record.interval != minRequestInterval ||
		!client.closed || len(client.calls) != 2 {
		t.Fatalf("record=%#v calls=%d closed=%t", record, len(client.calls), client.closed)
	}
	profile, request := client.calls[0], client.calls[1]
	if profile.path != profilePath || profile.body != emptyFormBody || profile.effect ||
		request.path != userTorrentsPath || request.contentType != jsonMediaType || request.effect || request.accept != apiAccept ||
		request.body != `{"userid":"100001","type":"SEEDING","pageNumber":1,"pageSize":50}` ||
		request.maxRequest != maxRequestBytes || request.maxBody != maxResponseBytes || request.maxHeader != maxResponseHeaderBytes {
		t.Fatalf("calls=%#v", client.calls)
	}
	if list.SiteID != SiteID || list.State != domain.AccountTorrentsSeeding || list.Page != 1 || list.ObservedAt.IsZero() ||
		list.Total == nil || *list.Total != 3 || list.TotalPages == nil || *list.TotalPages != 1 || len(list.Torrents) != 3 {
		t.Fatalf("list=%#v", list)
	}
	type row struct {
		id                   string
		scope                domain.AccountTrafficScope
		uploaded, downloaded any
		seedTime             any
		lastAction           time.Time
	}
	want := []row{
		{id: "9001", scope: domain.AccountTrafficCurrentSession, uploaded: int64(2048), downloaded: int64(0), seedTime: nil,
			lastAction: utc("2026-10-16T04:00:00Z")},
		{id: "9002", scope: domain.AccountTrafficTotal, uploaded: int64(8192), downloaded: int64(4096), seedTime: int64(3600),
			lastAction: utc("2026-10-15T12:00:00Z")},
		// The snatch record wins, so the peer's session traffic does not fill
		// its missing download; only the last action falls back to the peer.
		{id: "9003", scope: domain.AccountTrafficTotal, uploaded: int64(30), downloaded: nil, seedTime: int64(5),
			lastAction: utc("2026-10-15T17:00:00Z")},
	}
	for i, item := range list.Torrents {
		got := row{id: item.Torrent.Ref.RemoteID, scope: item.TrafficScope, uploaded: int64Value(item.UploadedBytes),
			downloaded: int64Value(item.DownloadedBytes), seedTime: int64Value(item.SeedTimeSeconds)}
		if item.LastActionAt != nil {
			got.lastAction = *item.LastActionAt
		}
		if got != want[i] || item.Torrent.Ref.SiteID != SiteID {
			t.Fatalf("row %d=%#v want %#v", i, got, want[i])
		}
	}
	seeding := list.Torrents[0].Torrent
	if seeding.Name != "Seeding CANARY" || seeding.SizeBytes == nil || *seeding.SizeBytes != 1<<30 || seeding.Category != "401" ||
		seeding.Seeders == nil || *seeding.Seeders != 12 || seeding.Promotion == nil || seeding.Promotion.DownloadFactor != "0" {
		t.Fatalf("seeding torrent=%#v", seeding)
	}
	encoded, err := json.Marshal(list)
	if err != nil || strings.Contains(string(encoded), "PEER-SECRET") || strings.Contains(string(encoded), "100001") {
		t.Fatalf("list leaks peer or account fields: %s err=%v", encoded, err)
	}
}

func TestAccountTorrentsMapsEveryStateAndPage(t *testing.T) {
	for _, test := range []struct {
		state domain.AccountTorrentState
		page  int
		body  string
	}{
		{domain.AccountTorrentsSeeding, 0, `{"userid":"100001","type":"SEEDING","pageNumber":1,"pageSize":50}`},
		{domain.AccountTorrentsLeeching, 2, `{"userid":"100001","type":"LEECHING","pageNumber":2,"pageSize":50}`},
		{domain.AccountTorrentsCompleted, 1, `{"userid":"100001","type":"COMPLETED","pageNumber":1,"pageSize":50}`},
		{domain.AccountTorrentsIncomplete, 1000, `{"userid":"100001","type":"INCOMPLETE","pageNumber":1000,"pageSize":50}`},
		{domain.AccountTorrentsUploaded, 3, `{"userid":"100001","type":"UPLOADED","pageNumber":3,"pageSize":50}`},
	} {
		client := &scriptedClient{steps: steps(jsonResponse(profileFixture), jsonResponse(`{"code":"0","data":{"data":[]}}`))}
		adapter, _ := scriptedAdapter(client)
		list, err := adapter.AccountTorrents(context.Background(), apiKey(t), site.AccountTorrentQuery{State: test.state, Page: test.page})
		wantPage := test.page
		if wantPage == 0 {
			wantPage = 1
		}
		if err != nil || len(client.calls) != 2 || client.calls[1].body != test.body || list.State != test.state || list.Page != wantPage ||
			list.Torrents == nil || len(list.Torrents) != 0 || list.Total != nil || list.TotalPages != nil {
			t.Fatalf("%s: list=%#v err=%v calls=%#v", test.state, list, err, client.calls)
		}
	}
}

func TestAccountTorrentQueryValidation(t *testing.T) {
	adapter := New()
	for _, query := range []site.AccountTorrentQuery{
		{State: domain.AccountTorrentsSeeding},
		{State: domain.AccountTorrentsLeeching, Page: 1},
		{State: domain.AccountTorrentsCompleted, Page: 1000},
		{State: domain.AccountTorrentsIncomplete},
		{State: domain.AccountTorrentsUploaded},
	} {
		if err := adapter.ValidateAccountTorrentQuery(query); err != nil {
			t.Fatalf("%+v rejected: %v", query, err)
		}
	}
	for _, query := range []site.AccountTorrentQuery{
		{},
		{State: "SEEDING"},
		{State: "seek"},
		{State: "hitrun"},
		{State: domain.AccountTorrentsSeeding, Page: -1},
		{State: domain.AccountTorrentsSeeding, Page: 1001},
	} {
		if err := adapter.ValidateAccountTorrentQuery(query); err == nil {
			t.Fatalf("%+v accepted", query)
		}
		client := &scriptedClient{}
		scripted, record := scriptedAdapter(client)
		if _, err := scripted.AccountTorrents(context.Background(), apiKey(t), query); err == nil || record.opened != 0 {
			t.Fatalf("%+v read err=%v opened=%d", query, err, record.opened)
		}
	}
}

func TestAccountTorrentsFailClosed(t *testing.T) {
	seeding := site.AccountTorrentQuery{State: domain.AccountTorrentsSeeding}
	for _, test := range []struct {
		name    string
		profile string
		list    string
		calls   int
		cause   error
	}{
		{name: "rejected key", profile: `{"code":"401","message":"CANARY","data":null}`, calls: 1, cause: site.ErrCredentialRejected},
		{name: "profile without an ID", profile: `{"code":"0","data":{"username":"canary_user"}}`, calls: 1},
		{name: "profile with a bad ID", profile: `{"code":"0","data":{"id":"01","username":"canary_user"}}`, calls: 1},
		{name: "rate limited", profile: profileFixture, list: `{"code":"4","message":"CANARY","data":null}`, calls: 2, cause: site.ErrRateLimited},
		{name: "refused", profile: profileFixture, list: `{"code":"1","message":"CANARY refusal","data":null}`, calls: 2},
		{name: "no data", profile: profileFixture, list: `{"code":"0","data":null}`, calls: 2},
		{name: "no list", profile: profileFixture, list: `{"code":"0","data":{"total":"0"}}`, calls: 2},
		{name: "list is not an object", profile: profileFixture, list: `{"code":"0","data":[]}`, calls: 2},
		{name: "row without a torrent", profile: profileFixture, list: `{"code":"0","data":{"data":[{"peer":{"uploaded":"1"}}]}}`, calls: 2},
		{name: "torrent with a bad ID", profile: profileFixture, list: `{"code":"0","data":{"data":[{"torrent":{"id":"01","name":"CANARY"}}]}}`, calls: 2},
		{name: "torrent without a name", profile: profileFixture, list: `{"code":"0","data":{"data":[{"torrent":{"id":"1"}}]}}`, calls: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &scriptedClient{steps: steps(jsonResponse(test.profile), jsonResponse(test.list))}
			adapter, _ := scriptedAdapter(client)
			list, err := adapter.AccountTorrents(context.Background(), apiKey(t), seeding)
			if err == nil || len(list.Torrents) != 0 || len(client.calls) != test.calls || !client.closed || strings.Contains(err.Error(), "CANARY") {
				t.Fatalf("list=%#v err=%v calls=%d", list, err, len(client.calls))
			}
			if test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("err=%v want %v", err, test.cause)
			}
		})
	}
}

func TestAccountTorrentsRefuseForeignCredentialsBeforeAnyRequest(t *testing.T) {
	seeding := site.AccountTorrentQuery{State: domain.AccountTorrentsSeeding}
	cookie, err := site.NewCookieCredential("sid=CANARY")
	if err != nil {
		t.Fatal(err)
	}
	malformed, err := site.NewAPIKeyCredential("KEY WITH SPACE")
	if err != nil {
		t.Fatal(err)
	}
	for _, credential := range []site.Credential{cookie, malformed} {
		client := &scriptedClient{}
		adapter, record := scriptedAdapter(client)
		if _, err := adapter.AccountTorrents(context.Background(), credential, seeding); err == nil || record.opened != 0 {
			t.Fatalf("method=%s err=%v opened=%d", credential.Method(), err, record.opened)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &scriptedClient{}
	adapter, record := scriptedAdapter(client)
	if _, err := adapter.AccountTorrents(ctx, apiKey(t), seeding); !errors.Is(err, context.Canceled) || record.opened != 0 {
		t.Fatalf("err=%v opened=%d", err, record.opened)
	}
}
