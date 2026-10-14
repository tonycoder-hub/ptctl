package tjupt

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

type accountFakeClient struct {
	body       []byte
	finalURL   *url.URL
	err        error
	requests   int
	closeCalls int
	path       string
	query      url.Values
}

func (client *accountFakeClient) Get(_ context.Context, path string, query url.Values) ([]byte, *url.URL, error) {
	client.requests++
	client.path = path
	client.query = query
	return append([]byte(nil), client.body...), client.finalURL, client.err
}

func (*accountFakeClient) GetOnce(context.Context, string, url.Values, string, int64, int64) (httpguard.StrictResponse, error) {
	panic("unexpected strict GET")
}

func (client *accountFakeClient) RequestsMade() int { return client.requests }

func (client *accountFakeClient) Close() error {
	client.closeCalls++
	return nil
}

func TestAccountCapabilityAndPartialSnapshot(t *testing.T) {
	const cookie = "sid=COOKIE-CANARY"
	pageURL, err := url.Parse("https://www.tjupt.org/mybonus.php")
	if err != nil {
		t.Fatal(err)
	}
	client := &accountFakeClient{body: accountPage("Alice", "12,345.67"), finalURL: pageURL}
	adapter := New("")
	if !adapter.Descriptor().Supports(domain.CapabilityAccountRead) {
		t.Fatal("TJUPT adapter did not declare account.read")
	}
	if _, ok := any(adapter).(site.AccountReader); !ok {
		t.Fatal("TJUPT adapter did not implement AccountReader")
	}
	adapter.newClient = func(baseURL, gotCookie string, interval time.Duration) (guardedClient, error) {
		if baseURL != DefaultBaseURL || gotCookie != cookie || interval != 2*time.Second {
			t.Fatalf("factory base=%q cookie=%q interval=%s", baseURL, gotCookie, interval)
		}
		return client, nil
	}
	credential, err := site.NewCookieCredential(cookie)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	snapshot, err := adapter.Account(context.Background(), credential)
	after := time.Now().UTC()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SiteID != "tjupt" || snapshot.Username != "Alice" || snapshot.Bonus != "12345.67" ||
		snapshot.UploadedBytes != nil || snapshot.DownloadedBytes != nil || snapshot.Ratio != "" || snapshot.Seeding != nil || snapshot.Leeching != nil ||
		snapshot.UserClass != "" || snapshot.SeedingPoints != "" || snapshot.Invites != nil ||
		snapshot.ObservedAt.Before(before) || snapshot.ObservedAt.After(after) || snapshot.ObservedAt.Location() != time.UTC {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	if client.requests != 1 || client.closeCalls != 1 || client.path != accountPath || len(client.query) != 0 {
		t.Fatalf("client=%#v", client)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), cookie) || strings.Contains(string(raw), "mybonus.php") {
		t.Fatalf("snapshot disclosed request authority: %s", raw)
	}
}

func TestAccountReadsUserPanel(t *testing.T) {
	pageURL, _ := url.Parse("https://www.tjupt.org/mybonus.php")
	client := &accountFakeClient{body: accountPanelPage(tjuptInfoBlock("1,024", "3")), finalURL: pageURL}
	adapter := New("")
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
	snapshot, err := adapter.Account(context.Background(), mustCredential(t, "sid=secret"))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Username != "Alice" || snapshot.Bonus != "12345.67" || snapshot.UserClass != "威震一方" || snapshot.SeedingPoints != "9876.5" ||
		snapshot.UploadedBytes == nil || *snapshot.UploadedBytes != 3848290697216 || snapshot.Invites == nil || *snapshot.Invites != 3 ||
		snapshot.Seeding == nil || *snapshot.Seeding != 1024 || snapshot.Leeching == nil || *snapshot.Leeching != 3 ||
		snapshot.DownloadedBytes != nil || snapshot.Ratio != "" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	raw, _ := json.Marshal(snapshot)
	for _, private := range []string{"NETWORK-CANARY", "AVATAR-CANARY", "UID-CANARY", "H&R"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("snapshot kept %q: %s", private, raw)
		}
	}
}

func TestUserPanelFieldsAreOptionalAndUnambiguous(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		check func(infoBlock) bool
	}{
		{name: "no panel", body: `<table><tr><td>上传量: 1.00 TiB 等级: [X]</td></tr></table>`, check: func(block infoBlock) bool { return block == infoBlock{} }},
		{name: "two panels", body: tjuptInfoBlock("1", "0") + tjuptInfoBlock("1", "0"), check: func(block infoBlock) bool { return block == infoBlock{} }},
		{name: "unclosed panel", body: strings.TrimSuffix(tjuptInfoBlock("1", "0"), "</table>"), check: func(block infoBlock) bool { return block == infoBlock{} }},
		{name: "conflicting seeding", body: strings.Replace(tjuptInfoBlock("1", "0"), "</td></tr></table></td>", `<img class="arrowup" alt="Torrents seeding" src="pic/trans.gif"/>2</td></tr></table></td>`, 1), check: func(block infoBlock) bool {
			return block.seeding == nil && block.leeching != nil && block.class == "威震一方"
		}},
		{name: "malformed counts", body: tjuptInfoBlock("1,30", "x"), check: func(block infoBlock) bool {
			return block.seeding == nil && block.leeching == nil && block.uploaded != nil
		}},
		{name: "control rune in class", body: strings.Replace(tjuptInfoBlock("1", "0"), "威震一方", "威震&#7;一方", 1), check: func(block infoBlock) bool { return block.class == "" && block.invites != nil }},
	}
	for _, test := range tests {
		if block := parseInfoBlock([]byte(test.body)); !test.check(block) {
			t.Fatalf("%s: block=%#v", test.name, block)
		}
	}
}

func TestAccountFailsClosedOnUnrecognizedOrUnsafePages(t *testing.T) {
	accountURL, _ := url.Parse("https://www.tjupt.org/mybonus.php")
	wrongURL, _ := url.Parse("https://www.tjupt.org/index.php")
	wrongOrigin, _ := url.Parse("https://example.invalid/mybonus.php")
	queryURL, _ := url.Parse("https://www.tjupt.org/mybonus.php?unexpected=1")
	loginURL, _ := url.Parse("https://www.tjupt.org/login.php")
	tests := []struct {
		name string
		body []byte
		url  *url.URL
	}{
		{name: "invalid UTF-8", body: append(accountPage("Alice", "1"), 0xff), url: accountURL},
		{name: "wrong route", body: accountPage("Alice", "1"), url: wrongURL},
		{name: "wrong origin", body: accountPage("Alice", "1"), url: wrongOrigin},
		{name: "unexpected query", body: accountPage("Alice", "1"), url: queryURL},
		{name: "missing username", body: []byte(`<div>&#24403;&#21069;&#39764;&#21147;&#20540;: 1</div>`), url: accountURL},
		{name: "missing balance", body: []byte(`<title>PT :: Alice&#30340;&#39764;&#21147;&#20540;</title>`), url: accountURL},
		{name: "malformed thousands", body: accountPage("Alice", "12,34"), url: accountURL},
		{name: "trailing decimal", body: accountPage("Alice", "12.34.56"), url: accountURL},
		{name: "oversized username", body: accountPage(strings.Repeat("A", 257), "1"), url: accountURL},
		{name: "login", body: []byte(`<input name="username"><input name="password">`), url: loginURL},
		{name: "challenge", body: []byte(`Just a moment <div id="cf-chl-widget"></div>`), url: accountURL},
	}
	credential, err := site.NewCookieCredential("sid=secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &accountFakeClient{body: test.body, finalURL: test.url}
			adapter := New("")
			adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
			snapshot, err := adapter.Account(context.Background(), credential)
			if err == nil || snapshot != (domain.AccountSnapshot{}) || client.requests != 1 || client.closeCalls != 1 {
				t.Fatalf("snapshot=%#v err=%v client=%#v", snapshot, err, client)
			}
		})
	}
}

func TestAccountPreflightAndErrorsDoNotLeakCredential(t *testing.T) {
	adapter := New("")
	factoryCalls := 0
	adapter.newClient = func(string, string, time.Duration) (guardedClient, error) {
		factoryCalls++
		return &accountFakeClient{err: errors.New("COOKIE-CANARY RESPONSE-CANARY")}, nil
	}
	credential, err := site.NewCookieCredential("sid=COOKIE-CANARY")
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.Account(canceled, credential); !errors.Is(err, context.Canceled) || factoryCalls != 0 {
		t.Fatalf("pre-canceled err=%v factoryCalls=%d", err, factoryCalls)
	}
	if _, err := adapter.Account(context.Background(), credential); err == nil || strings.Contains(err.Error(), "CANARY") || factoryCalls != 1 {
		t.Fatalf("request err=%v factoryCalls=%d", err, factoryCalls)
	}
	other, err := site.NewCredential(domain.AuthMethod("other"), "COOKIE-CANARY")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Account(context.Background(), other); err == nil || strings.Contains(err.Error(), "CANARY") || factoryCalls != 1 {
		t.Fatalf("auth method err=%v factoryCalls=%d", err, factoryCalls)
	}
}

func TestLoginRedirectIsAnAuthenticationFailure(t *testing.T) {
	base, err := url.Parse(DefaultBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	// TJUPT's loggedinorreturn answers an expired cookie with a relative
	// "Location: login.php?returnto=<page>"; NexusPHP forks may send it absolute.
	for _, raw := range []string{"login.php?returnto=mybonus.php", "https://www.tjupt.org/login.php?returnto=torrents.php%3Fsearch%3Dx", "/LOGIN.PHP"} {
		reference, _ := url.Parse(raw)
		if id, ok := classifyLoginRedirect(base.ResolveReference(reference)); !ok || id != loginRedirectID {
			t.Fatalf("%s: id=%q ok=%v", raw, id, ok)
		}
	}
	for _, raw := range []string{"mybonus.php?do=upload", "/login.php.bak", "/takelogin.php", "/login.php/extra"} {
		reference, _ := url.Parse(raw)
		if id, ok := classifyLoginRedirect(base.ResolveReference(reference)); ok || id != "" {
			t.Fatalf("%s: id=%q ok=%v", raw, id, ok)
		}
	}
	if id, ok := classifyLoginRedirect(nil); ok || id != "" {
		t.Fatalf("nil: id=%q ok=%v", id, ok)
	}

	production, err := newGuardedClient(DefaultBaseURL, "sid=secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := production.(loginRedirectClient); !ok {
		t.Fatalf("production client %T does not classify login redirects", production)
	}
	if _, ok := production.(guardedExchangeClient); !ok {
		t.Fatalf("production client %T lost the exchange port", production)
	}
	_ = production.Close()

	credential, err := site.NewCookieCredential("sid=secret")
	if err != nil {
		t.Fatal(err)
	}
	withErr := func(err error) (*Adapter, *accountFakeClient) {
		client := &accountFakeClient{err: err}
		adapter := New("")
		adapter.newClient = func(string, string, time.Duration) (guardedClient, error) { return client, nil }
		return adapter, client
	}
	adapter, client := withErr(&httpguard.RedirectError{ID: loginRedirectID})
	status, err := adapter.CheckSession(context.Background(), credential)
	if err != nil || status.State != domain.AuthenticationUnauthenticated || status.Username != "" || status.ObservedAt.IsZero() || client.requests != 1 || client.closeCalls != 1 {
		t.Fatalf("check status=%#v err=%v client=%#v", status, err, client)
	}
	adapter, client = withErr(&httpguard.RedirectError{ID: loginRedirectID})
	if _, err := adapter.Search(context.Background(), credential, "query"); err == nil || err.Error() != "TJUPT session is not authenticated" || client.requests != 1 {
		t.Fatalf("search err=%v client=%#v", err, client)
	}
	adapter, client = withErr(&httpguard.RedirectError{ID: loginRedirectID})
	if _, err := adapter.BonusCatalog(context.Background(), credential); err == nil || err.Error() != "TJUPT session is not authenticated" || client.requests != 1 {
		t.Fatalf("catalog err=%v client=%#v", err, client)
	}
	adapter, client = withErr(&httpguard.RedirectError{ID: loginRedirectID})
	if _, err := adapter.Account(context.Background(), credential); err == nil || err.Error() != "TJUPT session is not authenticated" || client.requests != 1 {
		t.Fatalf("account err=%v client=%#v", err, client)
	}
	for _, other := range []error{&httpguard.RedirectError{ID: "tjupt.other"}, errors.New("site redirect was rejected")} {
		adapter, client = withErr(other)
		if status, err := adapter.CheckSession(context.Background(), credential); err == nil || status.State != "" || client.requests != 1 {
			t.Fatalf("other redirect status=%#v err=%v", status, err)
		}
	}
}

// tjuptInfoBlock follows the live user panel: a nested table whose cell
// carries the class, balance, seeding points, invites, uploaded total, and
// active torrent icons, then a 访问网络 line that ptctl must never keep.
func tjuptInfoBlock(seeding, leeching string) string {
	return `<table id="info_block" cellpadding="4" cellspacing="0" border="0" width="100%"><tr><td><table width="100%"><tr>
<td class="bottom"><a href="usercp.php?action=personal#avatar"><img src="https://tjupt.org/bitbucket/AVATAR-CANARY.jpg" id="myavatar" alt="myavatar"></a></td>
<td class="bottom" style="text-align: left">欢迎, <span class="nowrap"><a href="userdetails.php?id=UID-CANARY" class='VeteranUser_Name'><b><span>Alice</span></b></a></span> [<a href='logout.php'>退出</a>]
<span class="color_bonus"> 等级: </span>[<a href="/classes.php#6"><span class='VeteranUser_Name' style='font-weight: bold'>威震一方</span></a>]
<span class='color_bonus'> 魔力值  </span>[<a href="mybonus.php">使用</a>|<a href="mybonusapps.php">茉莉园</a>]: <a href="bonus.php?show=log">12,345.67</a>
<span class='color_bonus'>做种积分: </span><a href="bonus.php?show=description">9,876.5</a>
<span class='color_invite'> 邀请: </span> <a href="invite.php?id=UID-CANARY" title="邀请码数量">3</a><br/>
<span class="color_bonus">H&amp;R积分 </span>[<a href="/hnr_bonus.php">日志</a>]: <a href="/hnr_bonus.php">100</a>
<span class='color_uploaded'>上传量:</span> 3.5 TiB <span class="color_active"> 上传排名:</span> <a href="/topten.php">7</a>&nbsp;
<span class='color_active'>活动种子:</span> <img class="arrowup" alt="Torrents seeding" title="当前做种数" src="pic/trans.gif"/>` + seeding + ` <img class="arrowdown" alt="Torrents leeching" title="当前下载数" src="pic/trans.gif"/>` + leeching + `&nbsp;
<span class='color_connectable'>访问网络: </span>NETWORK-CANARY 天津 IPv4
</td></tr></table></td></tr></table>`
}

func accountPanelPage(panel string) []byte {
	return []byte(`<html><head><title>北洋园PT :: Alice的魔力值 - Powered by NexusPHP</title></head><body>` + panel + `<table><tr><td class="text">用你的魔力值（当前12,345.67）换东东！</td></tr></table></body></html>`)
}

func accountPage(username, balance string) []byte {
	return []byte(`<html><head><title>PT :: ` + username + `&#30340;&#39764;&#21147;&#20540;</title></head><body><div>&#24403;&#21069;&#39764;&#21147;&#20540;: ` + balance + `</div></body></html>`)
}
