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

// The live wording is shaped like TJUPT's bonus pages: mybonus.php states the
// balance as "用你的魔力值（当前N）换东东！", and the luck game's rule text on
// mybonusapps.php mentions an unrelated "当前魔力值≥1000".
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
	body := bonusReviewPage(tjuptBonusRow("1", "1", "上传量兑换", "用魔力值兑换 10.0 GiB 上传量。", "1,000", `<input type="submit" name="submit" value="交换" />`) +
		`<tr><td>普通导航</td><td>不应进入目录</td></tr>`)
	if got := parseUsername(body); got != "Alice" {
		t.Fatalf("username = %q", got)
	}
	if got := parseBonusBalance(body); got != "12345.67" {
		t.Fatalf("balance = %q", got)
	}
	rows, err := parseBonusCatalog(body)
	if err != nil || len(rows) != 1 || rows[0].Selector != "1" || strings.Join(rows[0].Columns, "|") != "上传量兑换|1,000|交换|available" {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
	u, _ := url.Parse("https://www.tjupt.org/mybonus.php")
	state, username := classifyBonusPage(u, body)
	if state != domain.AuthenticationAuthenticated || username != "Alice" {
		t.Fatalf("bonus classification state=%q username=%q", state, username)
	}
}

func TestBonusCatalogRejectsAmbiguousSelectors(t *testing.T) {
	for name, rows := range map[string]string{
		"two selectors in one form": `<tr><form action="?action=exchange" method="post"><td>A</td><td>100</td><input type="hidden" name="option" value="1"><input type="hidden" name="option" value="2"><input type="submit"></form></tr>`,
		"repeated selector":         tjuptBonusRow("1", "1", "A", "a", "100", `<input type="submit">`) + tjuptBonusRow("1", "2", "B", "b", "200", `<input type="submit">`),
		"visible selector":          `<tr><td>设置</td><td><form action="settings.php"><input name="option"></form></td></tr>`,
	} {
		if got, err := parseBonusCatalog(bonusReviewPage(rows)); err == nil || bonusParseStopReason(err) != "selector_ambiguous" {
			t.Fatalf("%s: rows=%#v err=%v", name, got, err)
		}
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
	gamesURL, _ := url.Parse("https://www.tjupt.org/mybonusapps.php")
	if state, _ := classifyBonusPage(gamesURL, bonusReviewPage("")); state != domain.AuthenticationIndeterminate {
		t.Fatalf("games page state = %q", state)
	}
	bonusURL, _ := url.Parse("https://www.tjupt.org/mybonus.php")
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

func TestBonusCatalogSkipsUnrelatedForms(t *testing.T) {
	body := bonusReviewPage(`<tr><td>设置</td><td><form action="settings.php"><input name="theme"></form></td></tr>`)
	if rows, err := parseBonusCatalog(body); err == nil || bonusParseStopReason(err) != "selector_not_found" {
		t.Fatalf("unrelated form became bonus rows: %#v err=%v", rows, err)
	}
	body = bonusReviewPage(`<tr><td>设置</td><td><form action="settings.php"><input name="theme"></form></td></tr>` + tjuptBonusRow("3", "3", "C", "c", "300", `<input type="submit" value="交换">`))
	if rows, err := parseBonusCatalog(body); err != nil || len(rows) != 1 || rows[0].Selector != "3" {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
}

// tjuptBonusRow follows mybonus.php: each offer is a <tr> whose <form> wraps
// the display-number, description, price, and submit cells, and whose name is
// the description cell's <h1>.
func tjuptBonusRow(option, display, name, description, price, submit string) string {
	return `<tr><form action="?action=exchange" method="post"><td class="rowhead_center"><input type="hidden" name="option" value="` + option + `" /><b>` + display + `</b></td><td class="rowfollow" align='left'><h1>` + name + `</h1>` + description + `</td><td class="rowfollow" align='center'>` + price + `</td><td class="rowfollow" align="center">` + submit + `</td></form></tr>`
}

func TestDetectLoginPage(t *testing.T) {
	u, _ := url.Parse("https://www.tjupt.org/login.php?returnto=mybonus.php")
	if !isLoginPage(u, []byte(`<input name="username"><input name="password">`)) {
		t.Fatal("login page not detected")
	}
	state, _ := classifyBonusPage(u, []byte(`<input name="username"><input name="password">`))
	if state != domain.AuthenticationUnauthenticated {
		t.Fatalf("login state = %q", state)
	}
}
