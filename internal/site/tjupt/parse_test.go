package tjupt

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

// The live wording is shaped like TJUPT's mybonusapps.php: the balance is in
// the intro sentence, and the luck game's rule text mentions an unrelated
// "当前魔力值≥1000".
func TestParseBonusBalanceReadsLiveWordingAndIgnoresRuleText(t *testing.T) {
	const live = `<title>北洋园PT :: Alice的魔力值 - Powered by NexusPHP</title>
<span class='color_bonus'> 魔力值 </span>[<a href="mybonus.php">使用</a>|<a href="mybonusapps.php">茉莉园</a>]: <a href="bonus.php?show=log">1,439,363.7</a>
<table><tr><td class="text" colspan="4">用你的魔力值（当前1,439,363.7）来玩儿吧！</td></tr>
<tr><td><h1>碰运气</h1>如果当前魔力值≥1000，你可以用它来碰碰运气</td></tr></table>`
	tests := []struct{ name, body, want string }{
		{"live wording", live, "1439363.7"},
		{"rule text only", `<td><h1>碰运气</h1>如果当前魔力值≥1000，你可以用它来碰碰运气</td>`, ""},
		{"conflicting statements", `<td>用你的魔力值（当前12）来玩儿吧！</td><td>当前魔力值：13</td>`, ""},
		{"agreeing statements", `<td>用你的魔力值（当前1,000）来玩儿吧！</td><td>当前魔力值：1000</td>`, "1000"},
	}
	for _, test := range tests {
		if got := parseBonusBalance([]byte(test.body)); got != test.want {
			t.Fatalf("%s: balance = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestParseAccountAndBonusCatalog(t *testing.T) {
	body := []byte(`<!doctype html><html><head><title>北洋园PT :: Alice的魔力值 - Powered by NexusPHP</title></head>
	<body><div>当前魔力值：12,345.67</div><table>
<tr><td>上传量兑换</td><td>10 GiB</td><td>1000</td><td><form action="mybonusapps.php" method="post"><input type="hidden" name="option" value="1"><input type="submit" value="兑换"></form></td></tr>
	<tr><td>普通导航</td><td>不应进入目录</td></tr></table></body></html>`)
	if got := parseUsername(body); got != "Alice" {
		t.Fatalf("username = %q", got)
	}
	if got := parseBonusBalance(body); got != "12345.67" {
		t.Fatalf("balance = %q", got)
	}
	rows := parseBonusRows(body)
	if len(rows) != 1 || len(rows[0].Columns) < 3 {
		t.Fatalf("unexpected rows: %#v", rows)
	}
	if rows[0].Selector != "1" {
		t.Fatalf("bonus selector = %q", rows[0].Selector)
	}
	u, _ := url.Parse("https://www.tjupt.org/mybonusapps.php")
	state, username := classifyBonusPage(u, body)
	if state != domain.AuthenticationAuthenticated || username != "Alice" {
		t.Fatalf("bonus classification state=%q username=%q", state, username)
	}
}

func TestBonusCatalogSelectorHintRejectsAmbiguousControls(t *testing.T) {
	body := []byte(`<table><tr><td>A</td><td>100</td><td><form action="mybonusapps.php" method="post">
<input type="hidden" name="option" value="1"><input type="hidden" name="option" value="2"><input type="submit">
</form></td></tr></table>`)
	rows := parseBonusRows(body)
	if len(rows) != 1 || rows[0].Selector != "" {
		t.Fatalf("rows=%#v", rows)
	}
}

// tjuptSearchPage follows TJUPT's torrenttable (include/functions.php): the
// page sits in the NexusPHP layout table, and each torrent row nests a
// <table class="torrentname"> (sometimes with a second progress-bar row) in
// its name cell before the size cell that mksize_compact renders as
// "1.50<br />GB" with 1024-based units.
func tjuptSearchPage(rows ...string) []byte {
	return []byte(`<html><body><table class="mainouter" width="982"><tr><td id="outer" class="outer">
<a href="logout.php">退出</a><form action="torrents.php"><input name="search"></form>
<table class="torrents" cellspacing="0" cellpadding="5" width="100%">
<tr><td class="colhead"><a href="?sort=1&amp;type=desc">标题</a></td><td class="colhead"><a href="?sort=5&amp;type=desc">大小</a></td></tr>
` + strings.Join(rows, "\n") + `
</table></td></tr></table></body></html>`)
}

func tjuptSearchRow(id, name, small, progressRow, size string) string {
	return `<tr>
<td class="rowfollow nowrap" valign="middle" style="padding: 0px"><a href="?cat=401"><img class="c_movie" src="pic/cattrans.gif" alt="Movies" /></a></td>
<td class="rowfollow" width="100%" align="left"><table class="torrentname" width="100%"><tr><td class="embedded"  width="16px"></td><td class="embedded"><a title="` + name + `" href="details.php?id=` + id + `&amp;hit=1">` + name + `</a><br />` + small + `</td><td width="40" class="embedded" style="text-align: right; " valign="middle"><a href="download.php?id=` + id + `"><img class="download" src="pic/trans.gif" alt="download" /></a></td>
` + progressRow + `</tr></table></td>
<td class="rowfollow"><a href="details.php?id=` + id + `&amp;hit=1&amp;cmtpage=1#startcomments">3</a></td>
<td class="rowfollow nowrap"><span title="2024-01-01 00:00:00">1<br />天</span></td>
<td class="rowfollow">` + size + `</td>
<td class="rowfollow" align="center"><b><a href="details.php?id=` + id + `&amp;hit=1&amp;dllist=1#seeders">12</a></b></td>
</tr>`
}

func TestParseSearch(t *testing.T) {
	progress := `</tr><tr><td class="embedded" align="center" width="16px"><img src="pic/x.png" align="absmiddle"></td><td class="embedded" align="left"><div class="probar_a" title="100%"><div class="probar_b"></div></div></td><td class="embedded" align="right"></td>`
	body := tjuptSearchPage(
		tjuptSearchRow("42", "A Release 50GB", "Remux 4.20 GB", "", "1.50<br />GB"),
		tjuptSearchRow("43", "B Release", "", progress, "700.00<br />MB"),
	)
	got := parseSearch(body)
	if len(got) != 2 || got[0].Ref.RemoteID != "42" || got[0].Name != "A Release 50GB" || got[1].Ref.RemoteID != "43" || got[1].Name != "B Release" {
		t.Fatalf("unexpected results: %#v", got)
	}
	if got[0].SizeBytes == nil || *got[0].SizeBytes != 1610612736 {
		t.Fatalf("unexpected first size: %#v", got[0].SizeBytes)
	}
	if got[1].SizeBytes == nil || *got[1].SizeBytes != 734003200 {
		t.Fatalf("unexpected second size: %#v", got[1].SizeBytes)
	}
	u, _ := url.Parse("https://www.tjupt.org/torrents.php?search=release")
	if state := classifySearchPage(u, body); state != domain.AuthenticationAuthenticated {
		t.Fatalf("search page state = %q", state)
	}
}

func TestSearchRejectsUnrecognizedResultStructure(t *testing.T) {
	pageURL, _ := url.Parse("https://www.tjupt.org/torrents.php?search=release")
	credential, err := site.NewCookieCredential("sid=secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range [][]byte{
		[]byte(`<a href="logout.php">退出</a><form action="torrents.php"><input name="search"></form><table><tr><td><a href="details.php?id=42&amp;hit=1">A Release</a></td><td>1.50<br />GB</td></tr></table>`),
		tjuptSearchPage(),
	} {
		client := &accountFakeClient{body: body, finalURL: pageURL}
		adapter := New("")
		adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
		if got, err := adapter.Search(context.Background(), credential, "release"); err == nil || got != nil || client.requests != 1 {
			t.Fatalf("unrecognized result structure got=%#v err=%v requests=%d", got, err, client.requests)
		}
	}
	client := &accountFakeClient{body: tjuptSearchPage(tjuptSearchRow("42", "A Release", "", "", "1.50<br />GB")), finalURL: pageURL}
	adapter := New("")
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
	if got, err := adapter.Search(context.Background(), credential, "release"); err != nil || len(got) != 1 || got[0].Ref.RemoteID != "42" {
		t.Fatalf("search got=%#v err=%v", got, err)
	}
}

func TestPageClassificationFailsClosed(t *testing.T) {
	bonusURL, _ := url.Parse("https://www.tjupt.org/mybonusapps.php")
	for _, body := range [][]byte{
		[]byte(`<html><title>Maintenance</title></html>`),
		[]byte(`<html><title>Just a moment...</title><div id="cf-chl-widget"></div></html>`),
	} {
		state, _ := classifyBonusPage(bonusURL, body)
		if state != domain.AuthenticationIndeterminate {
			t.Fatalf("bonus page state = %q", state)
		}
		if state := classifySearchPage(bonusURL, body); state != domain.AuthenticationIndeterminate {
			t.Fatalf("search page state = %q", state)
		}
	}
}

func TestClassifyValidEmptySearch(t *testing.T) {
	u, _ := url.Parse("https://www.tjupt.org/torrents.php?search=none")
	body := []byte(`<html><a href="logout.php">退出</a><form action="torrents.php"><input name="search"></form><div>没有找到种子</div></html>`)
	if state := classifySearchPage(u, body); state != domain.AuthenticationAuthenticated {
		t.Fatalf("search page state = %q", state)
	}
	if got := parseSearch(body); len(got) != 0 {
		t.Fatalf("expected empty result, got %#v", got)
	}
}

// TestSearchAcceptsUpstreamEmptyMessages uses torrents.php's stdmsg markup with
// the lang_torrents std_try_again and std_nothing_found strings.
func TestSearchAcceptsUpstreamEmptyMessages(t *testing.T) {
	u, _ := url.Parse("https://www.tjupt.org/torrents.php?search=none")
	credential, err := site.NewCookieCredential("sid=secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"没有种子。请用准确的关键字重试。\n", "沒有種子。請用準確的關鍵字重試。\n", "没有结果！", "Nothing found! Try again with a refined search string.\n"} {
		body := []byte(`<html><a href="logout.php">退出</a><form action="torrents.php"><input name="search"></form><br /><table align="center" class="main" width="500" border="0" cellpadding="0" cellspacing="0"><tr><td class="embedded">
<h2>搜索结果 "none"</h2>
<table width="100%" border="1" cellspacing="0" cellpadding="10"><tr><td class="text">` + message + `</td></tr></table></td></tr></table>
</html>`)
		client := &accountFakeClient{body: body, finalURL: u}
		adapter := New("")
		adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
		if got, err := adapter.Search(context.Background(), credential, "none"); err != nil || got == nil || len(got) != 0 {
			t.Fatalf("%q: got=%#v err=%v", message, got, err)
		}
	}
}

func TestSearchShellWithoutAuthenticatedMarkerIsIndeterminate(t *testing.T) {
	u, _ := url.Parse("https://www.tjupt.org/torrents.php?search=none")
	body := []byte(`<html><form action="torrents.php"><input name="search"></form><div>没有找到种子</div></html>`)
	if state := classifySearchPage(u, body); state != domain.AuthenticationIndeterminate {
		t.Fatalf("search page state = %q", state)
	}
}

func TestSearchShellWithMaintenanceBodyIsIndeterminate(t *testing.T) {
	u, _ := url.Parse("https://www.tjupt.org/torrents.php?search=none")
	body := []byte(`<html><a href="logout.php">退出</a><form action="torrents.php"><input name="search"></form><h1>Maintenance</h1></html>`)
	if state := classifySearchPage(u, body); state != domain.AuthenticationIndeterminate {
		t.Fatalf("search page state = %q", state)
	}
}

func TestBonusCatalogRejectsUnrelatedForms(t *testing.T) {
	body := []byte(`<html><head><title>北洋园PT :: Alice的魔力值 - Powered by NexusPHP</title></head><body>
	<div>当前魔力值：12,345.67</div><table><tr><td>设置</td><td><form action="settings.php"><input name="option"></form></td></tr></table></body></html>`)
	if rows := parseBonusRows(body); len(rows) != 0 {
		t.Fatalf("unrelated form became bonus rows: %#v", rows)
	}
}

func TestDetectLoginPage(t *testing.T) {
	u, _ := url.Parse("https://www.tjupt.org/login.php?returnto=mybonusapps.php")
	if !isLoginPage(u, []byte(`<input name="username"><input name="password">`)) {
		t.Fatal("login page not detected")
	}
	state, _ := classifyBonusPage(u, []byte(`<input name="username"><input name="password">`))
	if state != domain.AuthenticationUnauthenticated {
		t.Fatalf("login state = %q", state)
	}
}
