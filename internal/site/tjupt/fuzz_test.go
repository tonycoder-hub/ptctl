package tjupt

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

var canonicalBalance = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

func FuzzParseSearch(f *testing.F) {
	f.Add(tjuptSearchPage(tjuptListing(tjuptListingHeader,
		tjuptRow("101", `title="CANARY Full Name"`, "CANARY Fu..", "1.50<br />GiB"),
		tjuptRow("102", "", "CANARY Short", "700.00<br />MB"))))
	f.Add(tjuptSearchPage(tjuptListing("", tjuptRow("7", `title="x"`, "x", "1 GB"))))
	f.Add(tjuptEmptySearchPage("没有结果！", "没有种子:("))
	f.Add([]byte(`<table><tr><td><a href="details.php?id=42&amp;hit=1">A</a></td><td>1.5 GiB</td></tr></table>`))
	f.Add([]byte(`<table class="torrents"><tr><td><table class="torrentname"><tr><td><a href="details.php?id=1">`))
	f.Fuzz(func(t *testing.T, body []byte) {
		results := parseSearch(body)
		if len(results) > maxSearchResults {
			t.Fatalf("%d results", len(results))
		}
		seen := make(map[string]struct{}, len(results))
		for _, result := range results {
			if err := validateCanonicalTorrentRef(result.Ref); err != nil {
				t.Fatalf("ref=%#v", result.Ref)
			}
			if _, duplicate := seen[result.Ref.RemoteID]; duplicate {
				t.Fatalf("duplicate %q", result.Ref.RemoteID)
			}
			seen[result.Ref.RemoteID] = struct{}{}
			if result.Name == "" || collapseSpace(result.Name) != result.Name {
				t.Fatalf("name=%q", result.Name)
			}
			if utf8.Valid(body) && !utf8.ValidString(result.Name) {
				t.Fatalf("name=%q from valid UTF-8", result.Name)
			}
			if result.SizeBytes != nil && *result.SizeBytes < 0 {
				t.Fatalf("size=%d", *result.SizeBytes)
			}
		}
		classifySearchPage(&url.URL{Path: "/torrents.php"}, body)
	})
}

func FuzzParseBonusPage(f *testing.F) {
	f.Add(tjuptBonusPage("CANARY-USER", "12,345.6", tjuptBonusRows))
	f.Add(tjuptBonusPage("CANARY-USER", "1,00.0", ""))
	f.Add([]byte(`<title>PT :: A的魔力值</title><div>当前魔力值：12,345.67</div><table><tr><form action="mybonusapps.php"><td><input type="hidden" name="option" value="1">A</td><td>1</td></form></tr></table>`))
	f.Fuzz(func(t *testing.T, body []byte) {
		if balance := parseBonusBalance(body); balance != "" && !canonicalBalance.MatchString(balance) {
			t.Fatalf("balance=%q", balance)
		}
		rows := parseBonusRows(body)
		if len(rows) > 100 {
			t.Fatalf("%d rows", len(rows))
		}
		for _, row := range rows {
			if row.Selector != "" && validateBonusSelector(row.Selector) != nil {
				t.Fatalf("selector=%q", row.Selector)
			}
			if len(row.Columns) < 2 {
				t.Fatalf("columns=%q", row.Columns)
			}
			for _, column := range row.Columns {
				if column == "" {
					t.Fatalf("columns=%q", row.Columns)
				}
			}
		}
		state, username := classifyBonusPage(&url.URL{Path: "/mybonusapps.php"}, body)
		if username != "" && (state != "authenticated" || !validAccountText(username, 256)) {
			t.Fatalf("state=%q username=%q", state, username)
		}
	})
}

func FuzzParseSize(f *testing.F) {
	for _, seed := range []string{"1.50 GiB", "700.00 MB", "1,024.000 PiB", "0 KB", "8192 PiB", "1,23 GiB"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		size, ok := parseSize(text)
		if !ok {
			if size != 0 {
				t.Fatalf("rejected %q with size %d", text, size)
			}
			return
		}
		if size < 0 || strings.TrimSpace(text) != text {
			t.Fatalf("parseSize(%q)=%d", text, size)
		}
	})
}
