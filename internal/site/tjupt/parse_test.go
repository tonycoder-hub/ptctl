package tjupt

import (
	"net/url"
	"testing"
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
}

func TestParseSearch(t *testing.T) {
	body := []byte(`<table><tr><td><a href="details.php?id=42&amp;hit=1">A Release</a></td><td>1.5 GiB</td></tr></table>`)
	got := parseSearch(body)
	if len(got) != 1 || got[0].Ref.RemoteID != "42" || got[0].Name != "A Release" {
		t.Fatalf("unexpected results: %#v", got)
	}
	if got[0].SizeBytes == nil || *got[0].SizeBytes != 1610612736 {
		t.Fatalf("unexpected size: %#v", got[0].SizeBytes)
	}
}

func TestDetectLoginPage(t *testing.T) {
	u, _ := url.Parse("https://www.tjupt.org/login.php?returnto=mybonusapps.php")
	if !isLoginPage(u, []byte(`<input name="username"><input name="password">`)) {
		t.Fatal("login page not detected")
	}
}
