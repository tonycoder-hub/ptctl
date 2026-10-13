package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

// These tests pin the exact public output of credential-free site discovery
// commands for the built-in adapters. Site registration may be refactored, but
// this output is a user-visible contract.

const builtinSiteDescriptorJSON = `{
  "schema": "ptctl.dev/v1",
  "kind": "site.descriptor.list",
  "data": [
    {
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
        "bonus.exchange.submit_effectful"
      ]
    }
  ]
}
`

func TestBuiltinSiteDiscoveryOutputIsStable(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"site", "list", "--output", "json"}, want: builtinSiteDescriptorJSON},
		{args: []string{"site", "capabilities", "--output", "json", "tjupt"}, want: builtinSiteDescriptorJSON},
		{args: []string{"site", "list"}, want: "" +
			"ID     NAME           STABILITY     BASE URL                CAPABILITIES\n" +
			"tjupt  TJUPT / 北洋园PT  experimental  https://www.tjupt.org/  8\n"},
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
			"tjupt  capability  bonus.exchange.submit_effectful\n"},
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
	config, err := site.NewTorrentDetailConfig("https://fake.invalid", "fake.details_by_id.v1")
	if err != nil {
		t.Fatal(err)
	}
	report := newSiteDetailReport(domain.TorrentRef{SiteID: "fakept", RemoteID: "42"}, config, site.DefaultTorrentDetailLimits())
	want := []string{
		"site detail fields are live site claims, not metafile identity or storage-content proof",
		"the single request is tracker-visible; the TJUPT route deliberately omits the view-counting hit parameter",
	}
	if strings.Join(report.Warnings, "\n") != strings.Join(want, "\n") {
		t.Fatalf("warnings=%q", report.Warnings)
	}
}
