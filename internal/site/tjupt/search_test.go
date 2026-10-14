package tjupt

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
)

// tjuptLiveSearchPage follows the live torrents.php table: nine top-level
// columns whose headers are labeled by text (类型, 标题, 发布者) or by icon
// classes, and rows whose name cell nests a torrentname table holding the
// title and, on its second row, the tags and promotion.
func tjuptLiveSearchPage(rows ...string) []byte {
	return []byte(`<html><body><table class="mainouter"><tr><td id="outer">
<a href='logout.php'>退出</a><form action="torrents.php"><input name="search"></form>
<table class="torrents" cellspacing="0" cellpadding="5" width="100%">
<tr> <td class="colhead" style="padding: 0">类型</td> <td class="colhead"><a href="?sort=1&amp;type=desc"> 标题 </a></td>
<td class="colhead"><a href="?sort=3&amp;type=desc"><img class="comments" src="pic/trans.gif" alt="comments" title="评论数" /></a></td>
<td class="colhead"><a href="?sort=4&amp;type=desc"><img class="time" src="pic/trans.gif" alt="time" title="存活时间" /></a></td>
<td class="colhead"><a href="?sort=5&amp;type=desc"><img class="size" src="pic/trans.gif" alt="size" title="大小" /></a></td>
<td class="colhead"><a href="?sort=7&amp;type=desc"><img class="seeders" src="pic/trans.gif" alt="seeders" title="种子数" /></a></td>
<td class="colhead"><a href="?sort=8&amp;type=desc"><img class="leechers" src="pic/trans.gif" alt="leechers" title="下载数" /></a></td>
<td class="colhead"><a href="?sort=6&amp;type=desc"><img class="snatched" src="pic/trans.gif" alt="snatched" title="完成数" /></a></td>
<td class="colhead"><a href="?sort=9&amp;type=desc"> 发布者 </a></td>
</tr>
` + strings.Join(rows, "\n") + `
</table></td></tr></table></body></html>`)
}

type liveRow struct {
	class, id, name, promotion, when, size, seeders, leechers, snatched string
}

func (row liveRow) String() string {
	return `<tr class="` + row.class + `">
<td class="rowfollow nowrap" valign="middle" style='padding: 0px'><a href="?search=x&amp;cat=408"><img class="c_software" src="pic/cattrans.gif" alt="软件" title="软件" /></a></td>
<td class="rowfollow" width="100%" align="left"><table class="torrentname" width="100%"><tr><td class="embedded"><a title="` + row.name + `" href="details.php?id=` + row.id + `&amp;hit=1"><b>` + row.name + `</b></a></td><td width="20" class="embedded" style="text-align: right" valign="middle"><a href="download.php?id=` + row.id + `"><img class="download" src="pic/trans.gif" alt="download" /></a></td></tr>
<tr><td class="embedded" colspan="2"><span class="tag">官方</span>` + row.promotion + `</td></tr></table></td>
<td class="rowfollow"><a href="comment.php?action=add&amp;pid=` + row.id + `&amp;type=torrent" title="添加评论">0</a></td>
<td class="rowfollow nowrap">` + row.when + `</td>
<td class="rowfollow">` + row.size + `</td>
<td class="rowfollow" align="center"><b><a href="details.php?id=` + row.id + `&amp;hit=1&amp;dllist=1#seeders">` + row.seeders + `</a></b></td>
<td class="rowfollow">` + row.leechers + `</td>
<td class="rowfollow"><a href="viewsnatches.php?id=` + row.id + `"><b>` + row.snatched + `</b></a></td>
<td class="rowfollow"><i>匿名</i></td>
</tr>`
}

func defaultLiveRow(id string) liveRow {
	return liveRow{class: "normal_bg", id: id, name: "Release " + id, when: "2026-10-14<br />17:39:19", size: "20.39<br />GiB", seeders: "8", leechers: "0", snatched: "1,216"}
}

const liveUploadPromotion = `[<font class='other_promotion' onmouseover="domTT_activate(this, event, 'content', '1.25倍上传', 'trail', false)">其他</font>]<span style='font-weight: bold;' title='1.25倍上传'><img class='arrowup' src='pic/trans.gif' alt='Upload Rate'>1.25X</span>[剩余时间：<span title="2026-10-16 05:40:19">1天6时</span>]`

func TestParseSearchReadsLiveColumns(t *testing.T) {
	promoted := defaultLiveRow("43")
	promoted.class, promoted.promotion = "sticky_bg", liveUploadPromotion
	promoted.when = `<span title="2024-01-01 00:00:00">1<br />年</span>`
	body := tjuptLiveSearchPage(defaultLiveRow("42").String(), promoted.String())
	pageURL, _ := url.Parse("https://www.tjupt.org/torrents.php?search=release")
	client := &accountFakeClient{body: body, finalURL: pageURL}
	adapter := New("")
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
	got, err := adapter.Search(context.Background(), mustCredential(t, "sid=secret"), "release")
	if err != nil || len(got) != 2 {
		t.Fatalf("got=%#v err=%v", got, err)
	}
	first := got[0]
	published := time.Date(2026, 10, 14, 9, 39, 19, 0, time.UTC)
	if first.Ref.RemoteID != "42" || first.Name != "Release 42" || first.Category != "408" || first.SizeBytes == nil || *first.SizeBytes != 21893595791 ||
		first.Seeders == nil || *first.Seeders != 8 || first.Leechers == nil || *first.Leechers != 0 || first.Snatches == nil || *first.Snatches != 1216 ||
		first.PublishedAt == nil || !first.PublishedAt.Equal(published) || first.PublishedAt.Location() != time.UTC ||
		first.Promotion == nil || *first.Promotion != (domain.Promotion{UploadFactor: "1", DownloadFactor: "1"}) {
		t.Fatalf("first=%#v", first)
	}
	second := got[1]
	ends := time.Date(2026, 10, 15, 21, 40, 19, 0, time.UTC)
	if second.Ref.RemoteID != "43" || second.PublishedAt == nil || !second.PublishedAt.Equal(time.Date(2023, 12, 31, 16, 0, 0, 0, time.UTC)) ||
		second.Promotion == nil || second.Promotion.UploadFactor != "1.25" || second.Promotion.DownloadFactor != "1" || second.Promotion.EndsAt == nil || !second.Promotion.EndsAt.Equal(ends) {
		t.Fatalf("second=%#v promotion=%#v", second, second.Promotion)
	}
}

func TestSearchPromotionMarkers(t *testing.T) {
	ends := time.Date(2026, 10, 15, 21, 40, 19, 0, time.UTC)
	tests := []struct {
		name, rowClass, cell string
		want                 *domain.Promotion
	}{
		{name: "none", want: &domain.Promotion{UploadFactor: "1", DownloadFactor: "1"}},
		{name: "live upload rate", cell: liveUploadPromotion, want: &domain.Promotion{UploadFactor: "1.25", DownloadFactor: "1", EndsAt: &ends}},
		{name: "live both rates", cell: `<font class='other_promotion'>其他</font><img class='arrowup' alt='Upload Rate'>2.00X <img class='arrowdown' alt='Download Rate'>0.50X`, want: &domain.Promotion{UploadFactor: "2", DownloadFactor: "0.5"}},
		{name: "word free", cell: `[<font class='free'>免费</font>]`, want: &domain.Promotion{UploadFactor: "1", DownloadFactor: "0"}},
		{name: "icon 2x free", cell: `<img class="pro_free2up" src="pic/trans.gif" alt="2X Free" />`, want: &domain.Promotion{UploadFactor: "2", DownloadFactor: "0"}},
		{name: "icon 30 percent", cell: `<img class="pro_30pctdown" src="pic/trans.gif" alt="30%" />`, want: &domain.Promotion{UploadFactor: "1", DownloadFactor: "0.3"}},
		{name: "row highlight", rowClass: "halfdown_bg", want: &domain.Promotion{UploadFactor: "1", DownloadFactor: "0.5"}},
		{name: "agreeing markers", rowClass: "twouphalfdown_bg", cell: `<font class='twouphalfdown'>2X 50%</font>`, want: &domain.Promotion{UploadFactor: "2", DownloadFactor: "0.5"}},
		{name: "normal ignores end time", cell: `[剩余时间：<span title="2026-10-16 05:40:19">1天</span>]`, want: &domain.Promotion{UploadFactor: "1", DownloadFactor: "1"}},
		{name: "conflicting markers", rowClass: "free_bg", cell: `<font class='twoup'>2X</font>`},
		{name: "unexplained promotion", cell: `<font class='other_promotion'>其他</font>`},
		{name: "unknown icon", cell: `<img class="pro_custom" alt="?">`},
		{name: "conflicting rates", cell: `<img alt='Upload Rate'>1.25X <img alt='Upload Rate'>2X`},
		{name: "invalid rate", cell: `<img alt='Upload Rate'>999X`},
	}
	for _, test := range tests {
		row := []byte(`<tr class="` + test.rowClass + `"><td>` + test.cell + `</td></tr>`)
		got := searchPromotion(row, []byte(test.cell))
		switch {
		case test.want == nil && got == nil:
		case test.want == nil || got == nil:
			t.Fatalf("%s: got=%#v want=%#v", test.name, got, test.want)
		case got.UploadFactor != test.want.UploadFactor || got.DownloadFactor != test.want.DownloadFactor ||
			(got.EndsAt == nil) != (test.want.EndsAt == nil) || got.EndsAt != nil && !got.EndsAt.Equal(*test.want.EndsAt):
			t.Fatalf("%s: got=%#v want=%#v", test.name, got, test.want)
		}
	}
}

func TestSearchFallsBackToSizeWhenColumnsAreUnrecognized(t *testing.T) {
	row := defaultLiveRow("42")
	duplicateHeader := strings.Replace(string(tjuptLiveSearchPage(row.String())), `<img class="leechers"`, `<img class="seeders"`, 1)
	extraCell := strings.Replace(string(tjuptLiveSearchPage(row.String())), `<td class="rowfollow"><i>匿名</i></td>`, `<td class="rowfollow"><i>匿名</i></td><td>extra</td>`, 1)
	for name, body := range map[string]string{"duplicate header icon": duplicateHeader, "cell count mismatch": extraCell} {
		got := parseSearch([]byte(body))
		if len(got) != 1 || got[0].SizeBytes == nil || *got[0].SizeBytes != 21893595791 || got[0].Seeders != nil || got[0].Snatches != nil ||
			got[0].Category != "" || got[0].PublishedAt != nil || got[0].Promotion != nil {
			t.Fatalf("%s: got=%#v", name, got)
		}
	}
}

func TestSearchCellValuesFailClosed(t *testing.T) {
	for _, test := range []struct {
		cell string
		want string
	}{
		{`<a href="?search=x&amp;cat=408">`, "408"},
		{`<a href='?cat=401'>`, "401"},
		{`<a href="?cat=0408">`, ""},
		{`<a href="?cat=408&amp;cat=409">`, ""},
		{`<a href="?cat=x">`, ""},
		{`<img class="c_software">`, ""},
	} {
		if got := categoryID([]byte(test.cell)); got != test.want {
			t.Fatalf("category %q = %q, want %q", test.cell, got, test.want)
		}
	}
	for _, cell := range []string{"yesterday", "2026-13-01<br />00:00:00", `<span title="2026-01-01 00:00:00">a</span><span title="2026-01-02 00:00:00">b</span>`} {
		if got := cellTime([]byte(cell)); got != nil {
			t.Fatalf("time %q = %v", cell, got)
		}
	}
	for text, want := range map[string]int{"0": 0, "1,216": 1216, "999999999": 999999999} {
		if got := siteCount(text); got == nil || *got != want {
			t.Fatalf("count %q = %v", text, got)
		}
	}
	for _, text := range []string{"", "-1", "1.5", "12,34", "1234567890", "1 2"} {
		if got := siteCount(text); got != nil {
			t.Fatalf("count %q = %d", text, *got)
		}
	}
}

func TestTopLevelCellsKeepsNestedTablesInTheirCell(t *testing.T) {
	cells := topLevelCells([]byte(`<tr><td>a</td><td><table><tr><td>b1</td><td>b2</td></tr></table></td><td>c</td></tr>`))
	if len(cells) != 3 || plainText(string(cells[0])) != "a" || plainText(string(cells[1])) != "b1 b2" || plainText(string(cells[2])) != "c" {
		t.Fatalf("cells=%q", cells)
	}
}

func TestSearchRecoversTruncatedNamesFromTheLinkTitle(t *testing.T) {
	long := "The.Lord.of.the.Rings.The.Fellowship.of.the.Ring.2001.EXTENDED.2160p.UHD.BluRay.REMUX.HDR.HEVC.DTS-X.7.1-CANARY &amp; Friends.Part.Two"
	full := strings.ReplaceAll(long, "&amp;", "&")
	cut := func(name string, keep int) string { return string([]rune(name)[:keep]) + ".." }
	tests := []struct {
		name, anchor, want string
	}{
		{"truncated", `<a title="` + long + `" href="details.php?id=42&amp;hit=1"><b>` + cut(long, 118) + `</b></a>`, full},
		{"cut after a space", `<a title="CANARY Long Name Part" href="details.php?id=42"><b>CANARY Long ..</b></a>`, "CANARY Long Name Part"},
		{"title with line breaks", "<a title=\"CANARY Long\n Name\" href=\"details.php?id=42\"><b>CANARY Lo..</b></a>", "CANARY Long Name"},
		{"tooltip mode has no title", `<a onmouseover="get_ext_info_ajax()" href="details.php?id=42&amp;hit=1"><b>CANARY Lo..</b></a>`, "CANARY Lo.."},
		{"unrelated title", `<a title="Other" href="details.php?id=42"><b>CANARY Lo..</b></a>`, "CANARY Lo.."},
		{"two titles", `<a title="CANARY Long" title="CANARY Loose" href="details.php?id=42"><b>CANARY Lo..</b></a>`, "CANARY Lo.."},
		{"name ending in dots", `<a title="CANARY Name.." href="details.php?id=42"><b>CANARY Name..</b></a>`, "CANARY Name.."},
		{"not truncated", `<a title="CANARY Longer" href="details.php?id=42"><b>CANARY Long</b></a>`, "CANARY Long"},
		{"only dots", `<a title="CANARY" href="details.php?id=42"><b>..</b></a>`, ".."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			row := strings.Replace(defaultLiveRow("42").String(), `<a title="Release 42" href="details.php?id=42&amp;hit=1"><b>Release 42</b></a>`, test.anchor, 1)
			got := parseSearch(tjuptLiveSearchPage(row))
			if len(got) != 1 || got[0].Name != test.want || got[0].Ref.RemoteID != "42" || got[0].SizeBytes == nil {
				t.Fatalf("got=%#v, want name %q", got, test.want)
			}
		})
	}
}

func TestSearchSkipsNonCanonicalTorrentIDs(t *testing.T) {
	for _, id := range []string{"042", "0", "12345678901234567890"} {
		row := strings.Replace(defaultLiveRow("42").String(), `href="details.php?id=42&amp;hit=1"><b>`, `href="details.php?id=`+id+`&amp;hit=1"><b>`, 1)
		if got := parseSearch(tjuptLiveSearchPage(row)); len(got) != 0 {
			t.Fatalf("id %q: got=%#v", id, got)
		}
	}
}

func TestSearchIgnoresTheSignedInUserLink(t *testing.T) {
	// Every page header links the signed-in user as userdetails.php?id=N,
	// which must never read as a torrent or shadow one with the same ID.
	header := `<a href='logout.php'>退出</a>`
	withUser := header + `<a href="userdetails.php?id=42" class="User_Name"><b>CANARY-USER</b></a>`
	body := []byte(strings.Replace(string(tjuptLiveSearchPage(defaultLiveRow("42").String())), header, withUser, 1))
	if got := parseSearch(body); len(got) != 1 || got[0].Ref.RemoteID != "42" || got[0].Name != "Release 42" {
		t.Fatalf("got=%#v", got)
	}
	empty := []byte(strings.Replace(string(tjuptLiveSearchPage(`<tr><td class="text">没有种子。请用准确的关键字重试。</td></tr>`)), header, withUser, 1))
	if got := parseSearch(empty); len(got) != 0 {
		t.Fatalf("empty page got=%#v", got)
	}
}
