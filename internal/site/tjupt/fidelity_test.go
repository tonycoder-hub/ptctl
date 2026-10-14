package tjupt

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

// The fixtures in this file reproduce the markup TJUPT's NexusPHP templates
// emit (torrenttable, mksize_compact, stdmsg and mybonusapps.php) so the
// parsers are held to the page shape the site really serves. Every value in
// them is synthetic; none was captured from a logged-in session.

func tjuptPage(title, content string) []byte {
	return []byte(`<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd">
<html xmlns="http://www.w3.org/1999/xhtml"><head><meta http-equiv="Content-Type" content="text/html; charset=utf-8" />
<title>` + title + `</title></head><body>
<table class="head" width="100%"><tr><td class="bottom" align="left"><span class="medium">欢迎回来, <a href="userdetails.php?id=1" class="User_Name"><b>CANARY-USER</b></a>  [<a href="logout.php">退出</a>]
[<a href="torrents.php?inclbookmarked=1&amp;allsec=1&amp;incldead=0">收藏</a>]
<font class='color_bonus'>魔力值 </font>[<a href="mybonus.php">使用</a>|<a href="mybonusapps.php">应用</a>]: 99,999.9
<font class="color_ratio">分享率:</font> 1.000</span></td></tr></table>
<table class="mainouter" width="100%" cellspacing="0" cellpadding="5" align="center"><tr><td id="outer" align="center" class="outer" style="padding-top: 20px; padding-bottom: 20px">
` + content + `
</td></tr></table></body></html>`)
}

func tjuptSearchForm(query string) string {
	return `<form method="get" name="searchbox" action="?"><table border="1" class="searchbox" cellspacing="0" width="940" cellpadding="5"><tbody><tr>
<td class="rowhead" align="right">关键字</td><td class="rowfollow" align="left"><input id="searchinput" name="search" type="text" value="` + query + `" autocomplete="off" style="width: 200px" />
<input type="submit" class="btn" value="给我搜" /></td></tr></tbody></table></form>`
}

const tjuptListingHeader = `<tr><td class="colhead" style="padding: 0">类型</td>
<td class="colhead"><a href="?search=CANARY&amp;sort=1&amp;type=asc">标题</a></td>
<td class="colhead"><a href="?search=CANARY&amp;sort=3&amp;type=desc"><img class="comments" src="pic/trans.gif" alt="comments" title="评论数" /></a></td>
<td class="colhead"><a href="?search=CANARY&amp;sort=4&amp;type=desc"><img class="time" src="pic/trans.gif" alt="time" title="存活时间" /></a></td>
<td class="colhead"><a href="?search=CANARY&amp;sort=5&amp;type=desc"><img class="size" src="pic/trans.gif" alt="size" title="大小" /></a></td>
<td class="colhead"><a href="?search=CANARY&amp;sort=7&amp;type=desc"><img class="seeders" src="pic/trans.gif" alt="seeders" title="种子数" /></a></td>
<td class="colhead"><a href="?search=CANARY&amp;sort=8&amp;type=desc"><img class="leechers" src="pic/trans.gif" alt="leechers" title="下载数" /></a></td>
<td class="colhead"><a href="?search=CANARY&amp;sort=6&amp;type=desc"><img class="snatched" src="pic/trans.gif" alt="snatched" title="完成数" /></a></td>
<td class="colhead"><a href="?search=CANARY&amp;sort=9&amp;type=desc">发布者</a></td></tr>`

func tjuptListing(header string, rows ...string) string {
	return `<table class="torrents" cellspacing="0" cellpadding="5" width="100%">` + header + strings.Join(rows, "\n") + `</table>`
}

// tjuptRow follows torrenttable(): an outer listing row whose second cell
// nests a "torrentname" table with the name link, promotion icon, subtitle,
// download links, a progress-bar row and a tag row.
func tjuptRow(id, linkAttrs, display, size string) string {
	return `<tr class="sp_bg">
<td class="rowfollow nowrap" valign="middle" style="padding: 0"><a href="?cat=401"><img src="pic/cattrans.gif" alt="Movies" title="电影" class="c_movies" /></a></td>
<td class="rowfollow" width="100%" align="left"><table class="torrentname" width="100%"><tr class="sticky_bg"><td class="embedded"  width="16px"><img class="sticky_1" src="pic/trans.gif" alt="Sticky" title="置顶至2026-10-20 00:00:00" /></td><td class="embedded">` +
		`<input type="checkbox" name="checked_torrent[]" value="` + id + `"> ` +
		`<a ` + linkAttrs + ` href="details.php?id=` + id + `&amp;hit=1"><b>` + display + `</b></a>` +
		`<img class="pro_free" src="pic/trans.gif" alt="Free" title="免费" /><br />CANARY 副标题 &amp; 说明</td>` +
		`<td width="40" class="embedded" style="text-align: right;" valign="middle"><a href="download.php?id=` + id + `"><img class="download" src="pic/trans.gif" style="padding-bottom: 2px;" alt="download" title="下载本种" /></a><a id="bookmark0" href="javascript: bookmark(` + id + `,0);"><img class="delbookmark" src="pic/trans.gif" alt="Unbookmarked" title="收藏" /></a></td>` +
		`</tr><tr><td class="embedded" align="center" width="16px"><img src="pic/seeding.png" align="absmiddle"></td><td class="embedded" align="left"><div class="probar_a1" title="做种中 100%"><div class="probar_b1" style="width:100%"></div></div></td><td class="embedded" align="right"></td>` +
		`</tr><tr><td class='embedded' align='left'></td><td class='embedded' align='center'><div id='tag' class='tag-group'><span class="tag tag-exclusive">禁转</span></div></td><td class='embedded' align='right'><a href="https://www.imdb.com/title/tt0000000/">7.0</a></td></tr></table></td>
<td class="rowfollow"><b><a href="details.php?id=` + id + `&amp;hit=1&amp;cmtpage=1#startcomments">3</a></b></td>
<td class="rowfollow nowrap"><span title="2026-10-01 12:00:00">2天<br />3时</span></td>
<td class="rowfollow">` + size + `</td>
<td class="rowfollow" align="center"><b><a href="details.php?id=` + id + `&amp;hit=1&amp;dllist=1#seeders">12</a></b></td>
<td class="rowfollow"><b><a href="details.php?id=` + id + `&amp;hit=1&amp;dllist=1#leechers">3</a></b></td>
<td class="rowfollow"><a href="viewsnatches.php?id=` + id + `"><b>45</b></a></td>
<td class="rowfollow"><i>匿名</i></td>
</tr>`
}

func tjuptSearchPage(listing string) []byte {
	return tjuptPage("北洋园PT :: 种子 - Powered by NexusPHP", tjuptSearchForm("CANARY")+listing)
}

func tjuptEmptySearchPage(heading, text string) []byte {
	return tjuptPage("北洋园PT :: 种子 - Powered by NexusPHP", tjuptSearchForm("CANARY")+
		`<table align="center" class="main" width="500" border="0" cellpadding="0" cellspacing="0"><tr><td class="embedded">
<h2>`+heading+`</h2>
<table width="100%" border="1" cellspacing="0" cellpadding="10"><tr><td class="text">`+text+`</td></tr></table></td></tr></table>`)
}

func tjuptBonusPage(username, balance, rows string) []byte {
	return tjuptPage("北洋园PT :: "+username+"的魔力值 - Powered by NexusPHP", `<table align="center" width="940" border="1" cellspacing="0" cellpadding="3">
<tr><td class="colhead" colspan="4" align="center"><font class="big">北洋园PT魔力值应用</font></td></tr>
<tr>
    <td class="text" align="center"
        colspan="4">用你的魔力值（当前`+balance+`）来玩儿吧！
        <br/> <b>如果按钮不可点，则说明你的魔力值不足以使用该应用。</b></td>
</tr>
<tr><td class="colhead" align="center">项目</td><td class="colhead" align="left">简介</td><td class="colhead" align="center">价格</td><td class="colhead" align="center">使用</td></tr>`+
		rows+`</table><br />`)
}

const tjuptBonusRows = `<tr><form action="?action=exchange" method="post"><td class="rowhead_center"><input type="hidden" name="option" value="1" /><b>1</b></td><td class="rowfollow" align='left'><h1>碰运气</h1>CANARY 说明</br></br><b>使用&nbsp;</b><input type="text" name="luckbonus" id="luckbonus" style="width: 80px" value="1000" />&nbsp;个魔力值！</td><td class="rowfollow nowrap" align='center'>最少10<br />最多1,000</td><td class="rowfollow" align="center"><input type="submit" name="submit" value="试试手气" /></td></form></tr>
<tr><form action="?action=exchange" method="post"><td class="rowhead_center"><input type="hidden" name="option" value="2" /><b>2</b></td><td class="rowfollow" align='left'><h1>CANARY 应用</h1>CANARY 说明</br></br></td><td class="rowfollow nowrap" align='center'>500</td><td class="rowfollow" align="center"><input type="submit" name="submit" value="使用" /></td></form></tr>`

func sizePointer(value int64) *int64 { return &value }

func TestParseSearchReadsTheNestedTorrentTable(t *testing.T) {
	full := "CANARY.Release.Name.2026.1080p.WEB-DL.H264.AAC-GROUP & Friends"
	body := tjuptSearchPage(tjuptListing(tjuptListingHeader,
		tjuptRow("101", `title="`+strings.ReplaceAll(full, "&", "&amp;")+`"`, strings.ReplaceAll(full[:30], "&", "&amp;")+"..", "1.50<br />GiB"),
		tjuptRow("102", `title="CANARY Short"`, "CANARY Short", "2.000<br />TiB"),
		tjuptRow("103", `onmouseover="domTT_activate(this, event, 'content', 'x');"`, "CANARY Tooltip Cut..", "700.00<br />MB"),
		tjuptRow("104", `title="Unrelated title text"`, "CANARY Own Text", "1,024.000<br />PiB"),
		tjuptRow("101", `title="duplicate"`, "duplicate", "1.00<br />KiB"),
	))
	if state := classifySearchPage(&url.URL{Path: "/torrents.php"}, body); state != domain.AuthenticationAuthenticated {
		t.Fatalf("state=%q", state)
	}
	got := parseSearch(body)
	want := []domain.TorrentSummary{
		{Ref: domain.TorrentRef{SiteID: SiteID, RemoteID: "101"}, Name: full, SizeBytes: sizePointer(1610612736)},
		{Ref: domain.TorrentRef{SiteID: SiteID, RemoteID: "102"}, Name: "CANARY Short", SizeBytes: sizePointer(2 << 40)},
		{Ref: domain.TorrentRef{SiteID: SiteID, RemoteID: "103"}, Name: "CANARY Tooltip Cut..", SizeBytes: sizePointer(700 << 20)},
		{Ref: domain.TorrentRef{SiteID: SiteID, RemoteID: "104"}, Name: "CANARY Own Text", SizeBytes: sizePointer(1 << 60)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("results:\n%s\nwant:\n%s", describeSummaries(got), describeSummaries(want))
	}
}

func TestParseSearchSizeColumnRules(t *testing.T) {
	headerWithoutSize := strings.Replace(tjuptListingHeader, `<img class="size" src="pic/trans.gif" alt="size" title="大小" />`, `大小`, 1)
	doubledSize := strings.Replace(tjuptListingHeader, `<img class="seeders"`, `<img class="size" alt="size" /><img class="seeders"`, 1)
	tests := []struct {
		name   string
		header string
		size   string
		want   *int64
	}{
		{name: "header column", header: tjuptListingHeader, size: "3.25<br />GiB", want: sizePointer(3489660928)},
		{name: "header column not a size", header: tjuptListingHeader, size: "--", want: nil},
		{name: "header column with trailing text", header: tjuptListingHeader, size: "3.25 GiB approx", want: nil},
		{name: "no size header falls back to first size cell", header: headerWithoutSize, size: "512.00<br />KiB", want: sizePointer(512 << 10)},
		{name: "no header at all", header: "", size: "1.00&nbsp;GB", want: sizePointer(1 << 30)},
		{name: "ambiguous header reports no size", header: doubledSize, size: "1.00<br />GiB", want: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := parseSearch(tjuptSearchPage(tjuptListing(test.header, tjuptRow("7", `title="CANARY"`, "CANARY", test.size))))
			if len(got) != 1 || !reflect.DeepEqual(got[0].SizeBytes, test.want) {
				t.Fatalf("results=%s", describeSummaries(got))
			}
		})
	}
}

func TestParseSearchIgnoresLinksOutsideTheListing(t *testing.T) {
	body := tjuptSearchPage(`<table><tr><td>热门: <a href="details.php?id=900&amp;hit=1">CANARY Sidebar</a> 9.00 GiB</td></tr></table>` +
		`<div><a href="details.php?id=901">CANARY Loose Link</a></div>` +
		tjuptListing(tjuptListingHeader, tjuptRow("5", `title="CANARY Listed"`, "CANARY Listed", "1.00<br />GiB")))
	got := parseSearch(body)
	if len(got) != 1 || got[0].Ref.RemoteID != "5" || got[0].Name != "CANARY Listed" {
		t.Fatalf("results=%s", describeSummaries(got))
	}
}

// Every page header links the signed-in user as userdetails.php?id=N. It is
// neither a result nor evidence of one, and must not shadow a torrent that
// happens to share the user's numeric ID.
func TestSearchIgnoresTheSignedInUserLink(t *testing.T) {
	pageURL := &url.URL{Path: "/torrents.php"}
	headerOnly := tjuptSearchPage("")
	if !strings.Contains(string(headerOnly), `href="userdetails.php?id=1"`) {
		t.Fatal("fixture lost the header user link")
	}
	if state := classifySearchPage(pageURL, headerOnly); state != domain.AuthenticationIndeterminate {
		t.Fatalf("header-only page state=%q", state)
	}
	if got := parseSearch(headerOnly); len(got) != 0 {
		t.Fatalf("header-only results=%s", describeSummaries(got))
	}
	got := parseSearch(tjuptSearchPage(tjuptListing(tjuptListingHeader, tjuptRow("1", `title="CANARY Torrent One"`, "CANARY Torrent One", "1.00<br />GiB"))))
	if len(got) != 1 || got[0].Ref.RemoteID != "1" || got[0].Name != "CANARY Torrent One" {
		t.Fatalf("results=%s", describeSummaries(got))
	}
}

func TestPrimaryDetailsIDSkipsSecondaryAndNonCanonicalLinks(t *testing.T) {
	for href, want := range map[string]string{
		"details.php?id=42&hit=1":                         "42",
		"details.php?id=42":                               "42",
		"/details.php?hit=1&id=42":                        "42",
		"https://www.tjupt.org/details.php?id=42&hit=1":   "42",
		"details.php?id=42&hit=1&cmtpage=1#startcomments": "",
		"details.php?id=42&hit=1&dllist=1#seeders":        "",
		"details.php?id=42#top":                           "",
		"details.php?id=042":                              "",
		"details.php?id=0":                                "",
		"details.php?id=1&id=2":                           "",
		"details.php?id=abc":                              "",
		"details.php?id=99999999999999999999999":          "",
		"details.php":                                     "",
		"xdetails.php?id=42":                              "",
		"userdetails.php?id=42":                           "",
		"details.php.bak?id=42":                           "",
		"details.php?id=42;x":                             "",
		"download.php?id=42":                              "",
	} {
		got, ok := primaryDetailsID(href)
		if got != want || ok != (want != "") {
			t.Errorf("primaryDetailsID(%q)=%q,%t want %q", href, got, ok, want)
		}
	}
}

func TestParseSizeUsesBinaryUnitsForEveryLabel(t *testing.T) {
	for text, want := range map[string]int64{
		"1.50 GiB":      1610612736,
		"1.50 GB":       1610612736,
		"1.5gb":         1610612736,
		"512.00 KiB":    512 << 10,
		"512.00 KB":     512 << 10,
		"1 MB":          1 << 20,
		"1.000 TB":      1 << 40,
		"1.000 PB":      1 << 50,
		"1,024.000 PiB": 1 << 60,
		"0.00 KiB":      0,
	} {
		got, ok := parseSize(text)
		if !ok || got != want {
			t.Errorf("parseSize(%q)=%d,%t want %d", text, got, ok, want)
		}
	}
	for _, text := range []string{"", "GiB", "1.5", "1.5 B", "1.5 EiB", "-1 GiB", "1,23 GiB", "1,2345 GiB", ",123 GiB", "1. GiB", "about 1.5 GiB", "1.5 GiB total", "8192 PiB", "99999999999999999999 PiB"} {
		if got, ok := parseSize(text); ok {
			t.Errorf("parseSize(%q)=%d accepted", text, got)
		}
	}
}

func TestClassifyTJUPTEmptySearchMessages(t *testing.T) {
	pageURL := &url.URL{Path: "/torrents.php"}
	for name, body := range map[string][]byte{
		"keyword search": tjuptEmptySearchPage(`搜索结果 "CANARY"`, "没有种子。请用准确的关键字重试。\n"),
		"empty listing":  tjuptEmptySearchPage("没有结果！", "没有种子:(\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if state := classifySearchPage(pageURL, body); state != domain.AuthenticationAuthenticated {
				t.Fatalf("state=%q", state)
			}
			if got := parseSearch(body); len(got) != 0 {
				t.Fatalf("results=%s", describeSummaries(got))
			}
		})
	}
}

func TestParseBonusBalanceReadsTheMybonusappsBanner(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "banner", text: "用你的魔力值（当前12,345.6）来玩儿吧！", want: "12345.6"},
		{name: "banner ascii parens", text: "用你的魔力值(当前 0.0)来玩儿吧", want: "0.0"},
		{name: "banner large", text: "用你的魔力值（当前1,234,567.8）", want: "1234567.8"},
		{name: "legacy wording", text: "当前魔力值：12,345.67", want: "12345.67"},
		{name: "both wordings agree", text: "用你的魔力值（当前1,000.0）<p>当前魔力值: 1000.0</p>", want: "1000.0"},
		{name: "wordings disagree", text: "用你的魔力值（当前1,000.0）<p>当前魔力值: 999.0</p>", want: ""},
		{name: "banners disagree", text: "用你的魔力值（当前1.0）用你的魔力值（当前2.0）", want: ""},
		{name: "malformed grouping", text: "用你的魔力值（当前12,34.5）", want: ""},
		{name: "trailing separator", text: "用你的魔力值（当前12.3.4）", want: ""},
		{name: "header bar only", text: "魔力值 [使用|应用]: 12,345.6", want: ""},
		{name: "no balance", text: "魔力值系统当前处于关闭中。", want: ""},
		{name: "too many statements", text: strings.Repeat("用你的魔力值（当前1.0）", maxBalanceMatches), want: ""},
	}
	for _, test := range tests {
		if got := parseBonusBalance([]byte("<div>" + test.text + "</div>")); got != test.want {
			t.Errorf("%s: parseBonusBalance=%q want %q", test.name, got, test.want)
		}
	}
}

func TestParseTheMybonusappsPage(t *testing.T) {
	pageURL := &url.URL{Scheme: "https", Host: "www.tjupt.org", Path: "/mybonusapps.php"}
	body := tjuptBonusPage("CANARY-USER", "12,345.6", tjuptBonusRows)
	state, username := classifyBonusPage(pageURL, body)
	if state != domain.AuthenticationAuthenticated || username != "CANARY-USER" || parseBonusBalance(body) != "12345.6" {
		t.Fatalf("state=%q username=%q balance=%q", state, username, parseBonusBalance(body))
	}
	rows := parseBonusRows(body)
	want := []domain.BonusCatalogRow{
		{Selector: "1", Columns: []string{"1", "碰运气 CANARY 说明 使用 个魔力值！", "最少10 最多1,000"}},
		{Selector: "2", Columns: []string{"2", "CANARY 应用 CANARY 说明", "500"}},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows=%#v", rows)
	}
}

func TestBonusFormActionRecognition(t *testing.T) {
	for form, want := range map[string]bool{
		`<form action="?action=exchange" method="post">`:  true,
		`<form action='?action=exchange'>`:                true,
		`<form action=?action=exchange method=post>`:      true,
		`<form action="?action=exchange"/>`:               true,
		`<form method="post" action="mybonusapps.php">`:   true,
		`<form action="mybonusapps.php?action=exchange">`: true,
		`<form action="?action=exchangeall">`:             false,
		`<form action="?action=viewluck">`:                false,
		`<form action="settings.php">`:                    false,
		`<form action="">`:                                false,
		`<form method="post">`:                            false,
		`<form action="?action=exchange&amp;x=1">`:        false,
		`<form data-x="?action=exchange" action="a.php">`: false,
	} {
		if got := bonusForm.MatchString(form); got != want {
			t.Errorf("bonusForm(%s)=%t want %t", form, got, want)
		}
	}
}

func TestBonusReviewReadsTheMybonusappsPage(t *testing.T) {
	body := tjuptBonusPage("CANARY-USER", "12,345.6", tjuptBonusRows)
	tests := []struct {
		selector  string
		inputMode string
	}{
		{selector: "1", inputMode: site.BonusReviewInputRequired},
		{selector: "2", inputMode: site.BonusReviewInputNone},
	}
	for _, test := range tests {
		t.Run(test.selector, func(t *testing.T) {
			client := &fakeGuardedClient{response: strictBonusResponse(body, time.Now().UTC())}
			adapter := New("")
			adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
			session, err := adapter.OpenBonusReviewSession(context.Background(), mustCredential(t, "sid=COOKIE-CANARY"))
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			observed, receipt, err := session.ReadBonusReview(context.Background(), test.selector, site.DefaultBonusReviewLimits())
			if err != nil {
				t.Fatalf("receipt=%#v err=%v", receipt, err)
			}
			review := observed.PublicCopy()
			if review.Balance != "12345.6" || review.Availability != site.BonusReviewAvailabilityAvailable || review.InputMode != test.inputMode ||
				review.ActionRouteID != BonusExchangeRouteID || !receipt.Complete {
				t.Fatalf("review=%#v receipt=%#v", review, receipt)
			}
		})
	}
}

// pageFakeClient answers adapter reads with one fixed page and records what
// was requested.
func newPageFakeClient(body []byte, finalURL string) *accountFakeClient {
	parsed, err := url.Parse(finalURL)
	if err != nil {
		panic(err)
	}
	return &accountFakeClient{body: body, finalURL: parsed}
}

func adapterWith(t *testing.T, client guardedClient, cookie string) *Adapter {
	t.Helper()
	adapter := New("")
	adapter.newClient = func(baseURL, gotCookie string, interval time.Duration) (guardedClient, error) {
		if baseURL != DefaultBaseURL || gotCookie != cookie || interval != 2*time.Second {
			t.Fatalf("factory base=%q cookie=%q interval=%s", baseURL, gotCookie, interval)
		}
		return client, nil
	}
	return adapter
}

func TestAdapterReadsAgainstTJUPTPages(t *testing.T) {
	const cookie = "sid=COOKIE-CANARY"
	credential := mustCredential(t, cookie)
	bonusURL := "https://www.tjupt.org/mybonusapps.php"
	searchURL := "https://www.tjupt.org/torrents.php?search=CANARY+query"

	t.Run("status", func(t *testing.T) {
		client := newPageFakeClient(tjuptBonusPage("CANARY-USER", "12,345.6", tjuptBonusRows), bonusURL)
		status, err := adapterWith(t, client, cookie).CheckSession(context.Background(), credential)
		if err != nil || status.State != domain.AuthenticationAuthenticated || status.Username != "CANARY-USER" || status.ObservedAt.IsZero() ||
			client.requests != 1 || client.closeCalls != 1 || client.path != "mybonusapps.php" || len(client.query) != 0 {
			t.Fatalf("status=%#v err=%v client=%#v", status, err, client)
		}
	})
	t.Run("account", func(t *testing.T) {
		client := newPageFakeClient(tjuptBonusPage("CANARY-USER", "12,345.6", tjuptBonusRows), bonusURL)
		snapshot, err := adapterWith(t, client, cookie).Account(context.Background(), credential)
		if err != nil || snapshot.Username != "CANARY-USER" || snapshot.Bonus != "12345.6" {
			t.Fatalf("snapshot=%#v err=%v", snapshot, err)
		}
	})
	t.Run("bonus catalog", func(t *testing.T) {
		client := newPageFakeClient(tjuptBonusPage("CANARY-USER", "12,345.6", tjuptBonusRows), bonusURL)
		catalog, err := adapterWith(t, client, cookie).BonusCatalog(context.Background(), credential)
		if err != nil || catalog.SiteID != SiteID || catalog.Balance != "12345.6" || len(catalog.Rows) != 2 || catalog.Rows[1].Selector != "2" || catalog.ObservedAt.IsZero() {
			t.Fatalf("catalog=%#v err=%v", catalog, err)
		}
	})
	t.Run("search", func(t *testing.T) {
		client := newPageFakeClient(tjuptSearchPage(tjuptListing(tjuptListingHeader, tjuptRow("11", `title="CANARY Result"`, "CANARY Result", "4.00<br />GiB"))), searchURL)
		results, err := adapterWith(t, client, cookie).Search(context.Background(), credential, "  CANARY query ")
		if err != nil || len(results) != 1 || results[0].Ref.RemoteID != "11" || results[0].SizeBytes == nil || *results[0].SizeBytes != 4<<30 ||
			client.requests != 1 || client.closeCalls != 1 || client.path != "torrents.php" || !reflect.DeepEqual(client.query, url.Values{"search": {"CANARY query"}}) {
			t.Fatalf("results=%s err=%v client=%#v", describeSummaries(results), err, client)
		}
	})
	t.Run("empty search", func(t *testing.T) {
		client := newPageFakeClient(tjuptEmptySearchPage(`搜索结果 "CANARY"`, "没有种子。请用准确的关键字重试。\n"), searchURL)
		results, err := adapterWith(t, client, cookie).Search(context.Background(), credential, "CANARY")
		if err != nil || results == nil || len(results) != 0 {
			t.Fatalf("results=%#v err=%v", results, err)
		}
	})
}

func TestAdapterReadsFailClosed(t *testing.T) {
	credential := mustCredential(t, "sid=COOKIE-CANARY")
	bonusURL := "https://www.tjupt.org/mybonusapps.php"
	searchURL := "https://www.tjupt.org/torrents.php?search=CANARY"
	loginPage := []byte(`<form action="takelogin.php" method="post"><input type="text" name="username"><input type="password" name="password"></form>`)
	challenge := []byte(`<title>Just a moment...</title><div id="cf-chl-widget">CANARY</div>`)
	const (
		unauthenticated   = "TJUPT session is not authenticated"
		unrecognizedPage  = "TJUPT returned an unrecognized search page; refusing to treat it as an empty result"
		unrecognizedBonus = "TJUPT returned an unrecognized bonus page; parser is fail-closed"
		emptyCatalog      = "TJUPT bonus page structure was not recognized; refusing an empty catalog"
	)
	searchTests := []struct {
		name    string
		body    []byte
		final   string
		wantErr string
	}{
		{name: "login redirect", body: loginPage, final: "https://www.tjupt.org/login.php?returnto=torrents.php", wantErr: unauthenticated},
		{name: "login form", body: loginPage, final: searchURL, wantErr: unauthenticated},
		{name: "challenge", body: challenge, final: searchURL, wantErr: unrecognizedPage},
		{name: "wrong path", body: tjuptSearchPage(tjuptListing(tjuptListingHeader, tjuptRow("1", "", "CANARY", "1.00<br />GiB"))), final: "https://www.tjupt.org/index.php", wantErr: unrecognizedPage},
		{name: "unknown empty wording", body: tjuptEmptySearchPage("CANARY", "什么也没有"), final: searchURL, wantErr: unrecognizedPage},
		{name: "header only", body: tjuptSearchPage(""), final: searchURL, wantErr: unrecognizedPage},
		{name: "details links but no result rows", body: tjuptSearchPage(`<div><a href="details.php?id=1&amp;hit=1&amp;cmtpage=1">CANARY</a></div>`), final: searchURL, wantErr: unrecognizedPage},
	}
	for _, test := range searchTests {
		t.Run("search "+test.name, func(t *testing.T) {
			client := newPageFakeClient(test.body, test.final)
			results, err := adapterWith(t, client, "sid=COOKIE-CANARY").Search(context.Background(), credential, "CANARY")
			if err == nil || err.Error() != test.wantErr || results != nil || client.requests != 1 || client.closeCalls != 1 {
				t.Fatalf("results=%#v err=%v client=%#v", results, err, client)
			}
		})
	}
	bonusTests := []struct {
		name       string
		body       []byte
		final      string
		wantState  domain.AuthenticationState
		catalogErr string
	}{
		{name: "login redirect", body: loginPage, final: "https://www.tjupt.org/login.php", wantState: domain.AuthenticationUnauthenticated, catalogErr: unauthenticated},
		{name: "challenge", body: challenge, final: bonusURL, wantState: domain.AuthenticationIndeterminate, catalogErr: unrecognizedBonus},
		{name: "wrong path", body: tjuptBonusPage("CANARY-USER", "1.0", tjuptBonusRows), final: "https://www.tjupt.org/mybonus.php", wantState: domain.AuthenticationIndeterminate, catalogErr: unrecognizedBonus},
		{name: "unexpected query", body: tjuptBonusPage("CANARY-USER", "1.0", tjuptBonusRows), final: bonusURL + "?action=viewluck", wantState: domain.AuthenticationIndeterminate, catalogErr: unrecognizedBonus},
		{name: "malformed balance", body: tjuptBonusPage("CANARY-USER", "1,00.0", tjuptBonusRows), final: bonusURL, wantState: domain.AuthenticationIndeterminate, catalogErr: unrecognizedBonus},
		{name: "system disabled", body: tjuptPage("北洋园PT :: CANARY-USER的魔力值", `<h2>对不起！</h2>魔力值系统当前处于关闭中。`), final: bonusURL, wantState: domain.AuthenticationIndeterminate, catalogErr: unrecognizedBonus},
		{name: "no exchange rows", body: tjuptBonusPage("CANARY-USER", "1.0", ""), final: bonusURL, wantState: domain.AuthenticationAuthenticated, catalogErr: emptyCatalog},
	}
	for _, test := range bonusTests {
		t.Run("bonus "+test.name, func(t *testing.T) {
			client := newPageFakeClient(test.body, test.final)
			status, err := adapterWith(t, client, "sid=COOKIE-CANARY").CheckSession(context.Background(), credential)
			if err != nil || status.State != test.wantState || (test.wantState != domain.AuthenticationAuthenticated && status.Username != "") {
				t.Fatalf("status=%#v err=%v", status, err)
			}
			client = newPageFakeClient(test.body, test.final)
			catalog, err := adapterWith(t, client, "sid=COOKIE-CANARY").BonusCatalog(context.Background(), credential)
			if err == nil || err.Error() != test.catalogErr || !reflect.DeepEqual(catalog, domain.BonusCatalog{}) {
				t.Fatalf("catalog=%#v err=%v", catalog, err)
			}
		})
	}
}

func TestAdapterReadPreflight(t *testing.T) {
	cookie := mustCredential(t, "sid=COOKIE-CANARY")
	apiKey, err := site.NewAPIKeyCredential("KEY-CANARY")
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	reads := map[string]func(*Adapter, context.Context, site.Credential) error{
		"status": func(adapter *Adapter, ctx context.Context, credential site.Credential) error {
			_, err := adapter.CheckSession(ctx, credential)
			return err
		},
		"search": func(adapter *Adapter, ctx context.Context, credential site.Credential) error {
			_, err := adapter.Search(ctx, credential, "CANARY")
			return err
		},
		"bonus": func(adapter *Adapter, ctx context.Context, credential site.Credential) error {
			_, err := adapter.BonusCatalog(ctx, credential)
			return err
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			factoryCalls := 0
			adapter := New("")
			adapter.newClient = func(string, string, time.Duration) (guardedClient, error) {
				factoryCalls++
				return nil, errors.New("factory COOKIE-CANARY")
			}
			if err := read(adapter, canceled, cookie); !errors.Is(err, context.Canceled) || factoryCalls != 0 {
				t.Fatalf("canceled err=%v factory=%d", err, factoryCalls)
			}
			if err := read(adapter, context.Background(), apiKey); err == nil || err.Error() != "TJUPT requires cookie_header authentication" || factoryCalls != 0 {
				t.Fatalf("api key err=%v factory=%d", err, factoryCalls)
			}
			if err := read(adapter, context.Background(), cookie); err == nil || factoryCalls != 1 {
				t.Fatalf("factory err=%v factory=%d", err, factoryCalls)
			}
			client := &accountFakeClient{err: errors.New("site request failed")}
			adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
			if err := read(adapter, context.Background(), cookie); err == nil || err.Error() != "site request failed" || client.requests != 1 || client.closeCalls != 1 {
				t.Fatalf("get err=%v client=%#v", err, client)
			}
		})
	}
	searchCalls := 0
	adapter := New("")
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) {
		searchCalls++
		return nil, errors.New("unexpected")
	}
	if _, err := adapter.Search(context.Background(), cookie, " \t"); err == nil || err.Error() != "search query is empty" || searchCalls != 0 {
		t.Fatalf("empty query err=%v calls=%d", err, searchCalls)
	}
}

func describeSummaries(summaries []domain.TorrentSummary) string {
	var out strings.Builder
	for _, summary := range summaries {
		size := "nil"
		if summary.SizeBytes != nil {
			size = strconv.FormatInt(*summary.SizeBytes, 10)
		}
		out.WriteString(summary.Ref.RemoteID + " | " + summary.Name + " | " + size + "\n")
	}
	return out.String()
}
