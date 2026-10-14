package tjupt

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/domain"
)

var canonicalBalance = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

func FuzzParseSearch(f *testing.F) {
	promoted := defaultLiveRow("43")
	promoted.class, promoted.promotion = "sticky_bg", liveUploadPromotion
	f.Add(tjuptLiveSearchPage(defaultLiveRow("42").String(), promoted.String()))
	f.Add(tjuptSearchPage(tjuptSearchRow("42", "A Release", "Remux", "", "1.50<br />GB")))
	f.Add([]byte(`<table class="torrents"><tr><td><a title="CANARY Long Name" href="details.php?id=7"><b>CANARY Lo..</b></a></td><td>1 GB</td></tr></table>`))
	f.Add([]byte(`<table class="torrents"><tr><td><table class="torrentname"><tr><td><a href="details.php?id=1">`))
	f.Fuzz(func(t *testing.T, body []byte) {
		results := parseSearch(body)
		if len(results) > 500 {
			t.Fatalf("%d results", len(results))
		}
		seen := make(map[string]struct{}, len(results))
		for _, result := range results {
			if err := validateCanonicalTorrentRef(result.Ref); err != nil {
				t.Fatalf("ref=%#v: %v", result.Ref, err)
			}
			if _, duplicate := seen[result.Ref.RemoteID]; duplicate {
				t.Fatalf("duplicate %q", result.Ref.RemoteID)
			}
			seen[result.Ref.RemoteID] = struct{}{}
			if result.Name == "" || strings.TrimSpace(spacePattern.ReplaceAllString(result.Name, " ")) != result.Name {
				t.Fatalf("name=%q", result.Name)
			}
			if utf8.Valid(body) && !utf8.ValidString(result.Name) {
				t.Fatalf("name=%q from valid UTF-8", result.Name)
			}
			for _, count := range []*int{result.Seeders, result.Leechers, result.Snatches} {
				if count != nil && *count < 0 {
					t.Fatalf("count=%d", *count)
				}
			}
			if result.SizeBytes != nil && *result.SizeBytes < 0 {
				t.Fatalf("size=%d", *result.SizeBytes)
			}
			if result.Category != "" && !canonicalPositiveID(result.Category) {
				t.Fatalf("category=%q", result.Category)
			}
			if result.PublishedAt != nil && result.PublishedAt.Location() != time.UTC {
				t.Fatalf("published=%v", result.PublishedAt)
			}
			if result.Promotion != nil && result.Promotion.EndsAt != nil && result.Promotion.EndsAt.Location() != time.UTC {
				t.Fatalf("ends=%v", result.Promotion.EndsAt)
			}
			if promotion := result.Promotion; promotion != nil && (canonicalFactor(promotion.UploadFactor) != promotion.UploadFactor || canonicalFactor(promotion.DownloadFactor) != promotion.DownloadFactor) {
				t.Fatalf("promotion=%#v", promotion)
			}
		}
		classifySearchPage(&url.URL{Path: "/torrents.php"}, body)
	})
}

func FuzzParseBonusPage(f *testing.F) {
	f.Add([]byte(`<title>北洋园PT :: CANARY的魔力值</title><a href="logout.php">退出</a><p>用你的魔力值（当前12,345.6）来玩儿吧！如果当前魔力值≥1000</p>`))
	f.Add(bonusReviewPage(`<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="submit"></form></tr>`))
	f.Add(accountPage("CANARY-USER", "1,00.0"))
	f.Fuzz(func(t *testing.T, body []byte) {
		if balance := parseBonusBalance(body); balance != "" && !canonicalBalance.MatchString(balance) {
			t.Fatalf("balance=%q", balance)
		}
		if rows, err := parseBonusCatalog(body); err == nil {
			seen := make(map[string]struct{}, len(rows))
			for _, row := range rows {
				if validateBonusSelector(row.Selector) != nil {
					t.Fatalf("selector=%q", row.Selector)
				}
				if _, duplicate := seen[row.Selector]; duplicate {
					t.Fatalf("duplicate selector %q", row.Selector)
				}
				seen[row.Selector] = struct{}{}
			}
		}
		state, username := classifyBonusPage(&url.URL{Path: "/" + bonusPagePath}, body)
		if username != "" && (state != domain.AuthenticationAuthenticated || !validAccountText(username, 256)) {
			t.Fatalf("state=%q username=%q", state, username)
		}
		block := parseInfoBlock(body)
		for _, count := range []*int{block.invites, block.seeding, block.leeching} {
			if count != nil && *count < 0 {
				t.Fatalf("count=%d", *count)
			}
		}
		if block.uploaded != nil && *block.uploaded < 0 {
			t.Fatalf("uploaded=%d", *block.uploaded)
		}
		if block.seedingPoints != "" && !canonicalBalance.MatchString(block.seedingPoints) {
			t.Fatalf("seeding points=%q", block.seedingPoints)
		}
	})
}

func FuzzFirstSize(f *testing.F) {
	for _, seed := range []string{"1.50 GiB", "700.00 MB", "1,024.00 PB", "0 KB", "8192 PiB", "20.39GiB"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		size, ok := firstSize([]string{text})
		if !ok && size != 0 || size < 0 {
			t.Fatalf("firstSize(%q)=%d,%v", text, size, ok)
		}
	})
}
