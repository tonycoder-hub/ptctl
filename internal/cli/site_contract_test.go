package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/mteam"
	"github.com/tonycoder-hub/ptctl/internal/site/tjupt"
)

// These tests pin the exact public output of credential-free site discovery
// commands for the built-in adapters. Site registration may be refactored, but
// this output is a user-visible contract.

const tjuptDescriptorJSON = `    {
      "id": "tjupt",
      "name": "TJUPT / 北洋园PT",
      "base_url": "https://www.tjupt.org/",
      "stability": "experimental",
      "auth_methods": [
        "cookie_header"
      ],
      "capabilities": [
        "auth.check",
        "account.read",
        "torrent.search",
        "bonus.catalog.read",
        "torrent.detail",
        "torrent.metafile.read_effectful",
        "bonus.offer.review",
        "bonus.exchange.submit_effectful",
        "auth.login_effectful"
      ]
    }`

const mteamDescriptorJSON = `    {
      "id": "mteam",
      "name": "M-Team / 馒头PT",
      "base_url": "https://api2.m-team.cc/",
      "stability": "experimental",
      "auth_methods": [
        "api_key_header"
      ],
      "capabilities": [
        "auth.check",
        "account.read",
        "torrent.search",
        "bonus.catalog.read",
        "torrent.detail",
        "torrent.metafile.read_effectful",
        "bonus.offer.review",
        "bonus.exchange.submit_effectful",
        "account.torrents.read"
      ]
    }`

func descriptorListJSON(entries ...string) string {
	return "{\n  \"schema\": \"ptctl.dev/v1\",\n  \"kind\": \"site.descriptor.list\",\n  \"data\": [\n" +
		strings.Join(entries, ",\n") + "\n  ]\n}\n"
}

func TestBuiltinSiteDiscoveryOutputIsStable(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"site", "list", "--output", "json"}, want: descriptorListJSON(mteamDescriptorJSON, tjuptDescriptorJSON)},
		{args: []string{"site", "capabilities", "--output", "json", "tjupt"}, want: descriptorListJSON(tjuptDescriptorJSON)},
		{args: []string{"site", "capabilities", "--output", "json", "mteam"}, want: descriptorListJSON(mteamDescriptorJSON)},
		{args: []string{"site", "list"}, want: "" +
			"ID     NAME           STABILITY     BASE URL                 CAPABILITIES\n" +
			"mteam  M-Team / 馒头PT  experimental  https://api2.m-team.cc/  9\n" +
			"tjupt  TJUPT / 北洋园PT  experimental  https://www.tjupt.org/   9\n"},
		{args: []string{"site", "capabilities", "tjupt"}, want: "" +
			"SITE   KIND        VALUE\n" +
			"tjupt  auth        cookie_header\n" +
			"tjupt  capability  auth.check\n" +
			"tjupt  capability  account.read\n" +
			"tjupt  capability  torrent.search\n" +
			"tjupt  capability  bonus.catalog.read\n" +
			"tjupt  capability  torrent.detail\n" +
			"tjupt  capability  torrent.metafile.read_effectful\n" +
			"tjupt  capability  bonus.offer.review\n" +
			"tjupt  capability  bonus.exchange.submit_effectful\n" +
			"tjupt  capability  auth.login_effectful\n"},
		{args: []string{"site", "capabilities", "mteam"}, want: "" +
			"SITE   KIND        VALUE\n" +
			"mteam  auth        api_key_header\n" +
			"mteam  capability  auth.check\n" +
			"mteam  capability  account.read\n" +
			"mteam  capability  torrent.search\n" +
			"mteam  capability  bonus.catalog.read\n" +
			"mteam  capability  torrent.detail\n" +
			"mteam  capability  torrent.metafile.read_effectful\n" +
			"mteam  capability  bonus.offer.review\n" +
			"mteam  capability  bonus.exchange.submit_effectful\n" +
			"mteam  capability  account.torrents.read\n"},
	}
	for _, test := range tests {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			reader := &trackingReader{}
			var out, errOut bytes.Buffer
			if code := Run(test.args, reader, &out, &errOut); code != 0 || errOut.Len() != 0 || reader.read {
				t.Fatalf("code=%d stderr=%q secretRead=%v", code, errOut.String(), reader.read)
			}
			if out.String() != test.want {
				t.Fatalf("stdout drifted:\n%s\nwant:\n%s", out.String(), test.want)
			}
		})
	}
}

func TestSiteDetailWarningsAreStable(t *testing.T) {
	for _, test := range []struct {
		origin, routeID, want string
	}{
		{tjupt.DetailOrigin, tjupt.DetailRouteID, "the single request is tracker-visible; the TJUPT route deliberately omits the view-counting hit parameter"},
		{mteam.DetailOrigin, mteam.DetailRouteID, "the single request is tracker-visible; the M-Team route never calls the separate view-counting endpoint"},
		{"https://fake.invalid", "fake.details_by_id.v1", "the single request is tracker-visible"},
	} {
		config, err := site.NewTorrentDetailConfig(test.origin, test.routeID)
		if err != nil {
			t.Fatal(err)
		}
		report := newSiteDetailReport(domain.TorrentRef{SiteID: "fakept", RemoteID: "42"}, config, site.DefaultTorrentDetailLimits())
		want := []string{
			"site detail fields are live site claims, not metafile identity or storage-content proof",
			test.want,
		}
		if strings.Join(report.Warnings, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%s warnings=%q", test.routeID, report.Warnings)
		}
	}
}
