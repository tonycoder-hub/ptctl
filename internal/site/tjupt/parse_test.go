package tjupt

import (
	"net/url"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/domain"
)

func TestParseAccountAndBonusCatalog(t *testing.T) {
	body := []byte(`<!doctype html><html><head><title>北洋园PT :: Alice的魔力值 - Powered by NexusPHP</title></head>
	<body><div>当前魔力值：12,345.67</div><table>
	<tr><td>上传量兑换</td><td>10 GiB</td><td>1000</td><td><form action="mybonusapps.php" method="post"><input name="option" value="1"><input type="submit" value="兑换"></form></td></tr>
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
	u, _ := url.Parse("https://www.tjupt.org/mybonusapps.php")
	state, username := classifyBonusPage(u, body)
	if state != domain.AuthenticationAuthenticated || username != "Alice" {
		t.Fatalf("bonus classification state=%q username=%q", state, username)
	}
}

func TestParseSearch(t *testing.T) {
	body := []byte(`<a href="logout.php">退出</a><form action="torrents.php"><input name="search"></form><table><tr><td><a href="details.php?id=42&amp;hit=1">A Release 50GB</a></td><td>1.5 GiB</td></tr></table>`)
	got := parseSearch(body)
	if len(got) != 1 || got[0].Ref.RemoteID != "42" || got[0].Name != "A Release 50GB" {
		t.Fatalf("unexpected results: %#v", got)
	}
	if got[0].SizeBytes == nil || *got[0].SizeBytes != 1610612736 {
		t.Fatalf("unexpected size: %#v", got[0].SizeBytes)
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
