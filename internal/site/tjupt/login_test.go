package tjupt

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

const (
	loginTestUser     = "CanaryUser"
	loginTestPassword = "PASSWORD-CANARY"
	loginTestSession  = "SESSION-CANARY.payload.sig"
)

type loginFakeClient struct {
	response    httpguard.StrictResponse
	cookies     []httpguard.SessionCookie
	err         error
	calls       int
	closeCalls  int
	path        string
	fields      []httpguard.FormField
	accept      string
	cookieNames []string
}

func (*loginFakeClient) Get(context.Context, string, url.Values) ([]byte, *url.URL, error) {
	panic("unexpected GET on the login client")
}

func (*loginFakeClient) GetOnce(context.Context, string, url.Values, string, int64, int64) (httpguard.StrictResponse, error) {
	panic("unexpected strict GET on the login client")
}

func (client *loginFakeClient) PostLoginFormOnce(_ context.Context, path string, fields []httpguard.FormField, accept string, _ int, _, _, _ int64, _ httpguard.RedirectClassifier, cookieNames []string) (httpguard.StrictResponse, []httpguard.SessionCookie, error) {
	client.calls++
	client.path = path
	client.fields = append([]httpguard.FormField(nil), fields...)
	client.accept = accept
	client.cookieNames = append([]string(nil), cookieNames...)
	return client.response, client.cookies, client.err
}

func (client *loginFakeClient) RequestsMade() int { return client.calls }
func (client *loginFakeClient) Close() error {
	client.closeCalls++
	return nil
}

func twoFactorPage() []byte {
	return []byte(`<html><head><title>北洋园PT :: 两步验证 - Powered by NexusPHP</title></head><body>
<h2>登录两步验证</h2>
<form name="verifyform" method="post" action="takelogin.php">
<input type="hidden" name="username" value="` + loginTestUser + `">
<input type="hidden" name="password" value="` + loginTestPassword + `">
<input type="hidden" name="logout" value="7">
请输入6位数字验证码 <input type="text" name="verify_code" maxlength="6">
<input type="submit" value="提交"></form>
<p>连续 10 次错误尝试会导致你的IP地址被禁用，你还有 <b>[9]</b> 次尝试机会</p></body></html>`)
}

func loginFailedPage() []byte {
	return []byte(`<html><head><title>北洋园PT :: 登录失败</title></head><body><h2>登录失败！</h2>
<p>用户名或密码不正确。你还有 [8] 次尝试机会</p></body></html>`)
}

func htmlLoginResponse(status int, body []byte) httpguard.StrictResponse {
	return httpguard.StrictResponse{StatusCode: status, MediaType: "text/html", Body: body, ResponseBytesKnown: true, ResponseBytesRead: int64(len(body))}
}

func issuedLoginClient(expires time.Time) *loginFakeClient {
	return &loginFakeClient{
		response: htmlLoginResponse(302, nil),
		cookies:  []httpguard.SessionCookie{httpguard.NewSessionCookie(loginSessionCookie, loginTestSession, expires)},
	}
}

type loginHarness struct {
	adapter    *Adapter
	login      *loginFakeClient
	verify     *accountFakeClient
	cookies    []string
	sleeps     []time.Duration
	factoryErr error
}

func newLoginHarness(t *testing.T, login *loginFakeClient, verifyPage []byte) *loginHarness {
	t.Helper()
	pageURL, err := url.Parse("https://www.tjupt.org/mybonus.php")
	if err != nil {
		t.Fatal(err)
	}
	harness := &loginHarness{adapter: New(""), login: login, verify: &accountFakeClient{body: verifyPage, finalURL: pageURL}}
	harness.adapter.newClient = func(baseURL, cookie string, interval time.Duration) (guardedClient, error) {
		if baseURL != DefaultBaseURL || interval != 2*time.Second {
			t.Fatalf("client base=%q interval=%v", baseURL, interval)
		}
		harness.cookies = append(harness.cookies, cookie)
		if harness.factoryErr != nil {
			return nil, harness.factoryErr
		}
		if cookie == "" {
			return harness.login, nil
		}
		return harness.verify, nil
	}
	harness.adapter.sleep = func(_ context.Context, delay time.Duration) error {
		harness.sleeps = append(harness.sleeps, delay)
		return nil
	}
	return harness
}

func loginCredentials(t *testing.T, code string) site.LoginCredentials {
	t.Helper()
	password, err := site.NewAccountPassword(loginTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := site.NewLoginCredentials(loginTestUser, password, code)
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

func assertNoLoginSecrets(t *testing.T, values ...any) {
	t.Helper()
	for _, value := range values {
		rendered := fmt.Sprintf("%v %+v %#v", value, value, value)
		for _, secret := range []string{loginTestPassword, "SESSION-CANARY", "123456"} {
			if strings.Contains(rendered, secret) {
				t.Fatalf("secret %q leaked: %s", secret, rendered)
			}
		}
	}
}

func TestLoginCapabilityOnlyForTrustedDefaultOrigin(t *testing.T) {
	adapter := New("")
	if !adapter.Descriptor().Supports(domain.CapabilityLogin) || !site.ImplementsCapability(adapter, domain.CapabilityLogin) {
		t.Fatal("default TJUPT adapter did not declare and implement auth.login_effectful")
	}
	config := adapter.LoginConfig()
	if fmt.Sprint(config.SessionDays) != "[1 7 14 30 90]" || config.DefaultSessionDays != 7 || !config.Allows(7) || config.Allows(0) || config.Allows(365) {
		t.Fatalf("config=%#v", config)
	}
	config.SessionDays[0] = 365
	if adapter.LoginConfig().Allows(365) {
		t.Fatal("LoginConfig returned shared session days")
	}
	custom := New("https://www.tjupt.org")
	if custom.Descriptor().Supports(domain.CapabilityLogin) {
		t.Fatal("non-exact origin declared login")
	}
	custom.newClient = func(string, string, time.Duration) (guardedClient, error) {
		t.Fatal("custom origin opened a login client")
		return nil, nil
	}
	if _, err := custom.Login(context.Background(), loginCredentials(t, ""), 7); err == nil {
		t.Fatal("custom origin attempted a login")
	}
}

func TestLoginWithTwoStepCodeVerifiesTheIssuedSession(t *testing.T) {
	expires := time.Date(2026, 10, 22, 15, 4, 33, 0, time.UTC)
	harness := newLoginHarness(t, issuedLoginClient(expires), accountPage(loginTestUser, "1,234.5"))
	result, err := harness.adapter.Login(context.Background(), loginCredentials(t, "123456"), 30)
	if err != nil {
		t.Fatal(err)
	}
	login := harness.login
	if login.calls != 1 || login.closeCalls != 1 || login.path != loginPath || login.accept != "text/html" || fmt.Sprint(login.cookieNames) != "[access_token]" {
		t.Fatalf("login client calls=%d closes=%d path=%q accept=%q names=%q", login.calls, login.closeCalls, login.path, login.accept, login.cookieNames)
	}
	wantFields := []httpguard.FormField{{Name: "username", Value: loginTestUser}, {Name: "password", Value: loginTestPassword}, {Name: "logout", Value: "30"}, {Name: "verify_code", Value: "123456"}}
	if fmt.Sprint(login.fields) != fmt.Sprint(wantFields) {
		t.Fatal("login form fields drifted")
	}
	if fmt.Sprint(harness.cookies) != fmt.Sprint([]string{"", "access_token=" + loginTestSession}) || harness.verify.requests != 1 || harness.verify.path != bonusPagePath {
		t.Fatalf("verification clients=%d verify requests=%d path=%q", len(harness.cookies), harness.verify.requests, harness.verify.path)
	}
	if fmt.Sprint(harness.sleeps) != "[2s]" {
		t.Fatalf("sleeps=%v", harness.sleeps)
	}
	if result.Session.Method() != domain.AuthMethodCookieHeader || result.Session.SecretValue() != "access_token="+loginTestSession ||
		result.Username != loginTestUser || result.ExpiresAt == nil || !result.ExpiresAt.Equal(expires) || result.RequestsAttempted != 2 {
		t.Fatalf("result username=%q expires=%v requests=%d", result.Username, result.ExpiresAt, result.RequestsAttempted)
	}
	assertNoLoginSecrets(t, result)
}

func TestLoginWithoutTwoStepFieldSendsThreeFields(t *testing.T) {
	harness := newLoginHarness(t, issuedLoginClient(time.Time{}), accountPage(loginTestUser, "1"))
	result, err := harness.adapter.Login(context.Background(), loginCredentials(t, ""), 7)
	if err != nil || len(harness.login.fields) != 3 || harness.login.fields[2] != (httpguard.FormField{Name: "logout", Value: "7"}) || result.ExpiresAt != nil {
		t.Fatalf("fields=%d expires=%v err=%v", len(harness.login.fields), result.ExpiresAt, err)
	}
}

func TestLoginStopsOnRecognizedRefusalsWithoutVerifying(t *testing.T) {
	nine, eight := 9, 8
	tests := []struct {
		name      string
		code      string
		response  httpguard.StrictResponse
		cookies   []httpguard.SessionCookie
		reason    string
		remaining *int
	}{
		{name: "two-step required", response: htmlLoginResponse(200, twoFactorPage()), reason: site.LoginTwoFactorRequired, remaining: &nine},
		{name: "two-step code rejected", code: "123456", response: htmlLoginResponse(200, twoFactorPage()), reason: site.LoginTwoFactorRejected, remaining: &nine},
		{name: "two-step page with a cookie", code: "123456", response: htmlLoginResponse(200, twoFactorPage()),
			cookies: []httpguard.SessionCookie{httpguard.NewSessionCookie(loginSessionCookie, loginTestSession, time.Time{})}, reason: site.LoginTwoFactorRejected, remaining: &nine},
		{name: "password rejected", response: htmlLoginResponse(200, loginFailedPage()), reason: site.LoginCredentialsRejected, remaining: &eight},
		{name: "unknown page", response: htmlLoginResponse(200, []byte(`<html><title>北洋园PT</title><p>你还有 [3] 次尝试机会</p></html>`)), reason: site.LoginResponseUnrecognized},
		{name: "redirect without cookie", response: htmlLoginResponse(302, nil), reason: site.LoginResponseUnrecognized},
		{name: "failure text on a redirect", response: htmlLoginResponse(302, loginFailedPage()), reason: site.LoginResponseUnrecognized},
		{name: "server error with cookie", response: htmlLoginResponse(500, nil),
			cookies: []httpguard.SessionCookie{httpguard.NewSessionCookie(loginSessionCookie, loginTestSession, time.Time{})}, reason: site.LoginResponseUnrecognized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := append([]byte(nil), test.response.Body...)
			test.response.Body = body
			harness := newLoginHarness(t, &loginFakeClient{response: test.response, cookies: test.cookies}, nil)
			result, err := harness.adapter.Login(context.Background(), loginCredentials(t, test.code), 7)
			var stopped *site.LoginError
			if !errors.As(err, &stopped) || stopped.Reason != test.reason {
				t.Fatalf("err=%v", err)
			}
			if (stopped.AttemptsRemaining == nil) != (test.remaining == nil) || (test.remaining != nil && *stopped.AttemptsRemaining != *test.remaining) {
				t.Fatalf("attempts remaining=%v want %v", stopped.AttemptsRemaining, test.remaining)
			}
			if harness.login.calls != 1 || len(harness.cookies) != 1 || harness.verify.requests != 0 || len(harness.sleeps) != 0 {
				t.Fatalf("login calls=%d clients=%d verify=%d sleeps=%d", harness.login.calls, len(harness.cookies), harness.verify.requests, len(harness.sleeps))
			}
			if result.Session != (site.Credential{}) || result.RequestsAttempted != 1 {
				t.Fatalf("result requests=%d", result.RequestsAttempted)
			}
			if strings.Trim(string(body), "\x00") != "" {
				t.Fatal("login response body was retained instead of cleared")
			}
			assertNoLoginSecrets(t, err, result)
		})
	}
}

func TestLoginRequiresTheVerificationReadToNameTheSameAccount(t *testing.T) {
	tests := []struct {
		name string
		page []byte
		err  error
	}{
		{name: "other account", page: accountPage("OtherUser", "1")},
		{name: "unrecognized page", page: []byte(`<html><title>maintenance</title></html>`)},
		{name: "read failed", err: fmt.Errorf("site request failed")},
		{name: "login redirect", err: &httpguard.RedirectError{ID: loginRedirectID}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newLoginHarness(t, issuedLoginClient(time.Time{}), test.page)
			harness.verify.err = test.err
			result, err := harness.adapter.Login(context.Background(), loginCredentials(t, ""), 7)
			var stopped *site.LoginError
			if !errors.As(err, &stopped) || stopped.Reason != site.LoginSessionUnverified || stopped.AttemptsRemaining != nil {
				t.Fatalf("err=%v", err)
			}
			if result.Session != (site.Credential{}) || result.Username != "" || result.RequestsAttempted != 2 || harness.verify.requests != 1 {
				t.Fatalf("result username=%q requests=%d verify=%d", result.Username, result.RequestsAttempted, harness.verify.requests)
			}
			assertNoLoginSecrets(t, err, result)
		})
	}
	harness := newLoginHarness(t, issuedLoginClient(time.Time{}), accountPage(strings.ToLower(loginTestUser), "1"))
	if result, err := harness.adapter.Login(context.Background(), loginCredentials(t, ""), 7); err != nil || result.Username != strings.ToLower(loginTestUser) {
		t.Fatalf("case-insensitive username: username=%q err=%v", result.Username, err)
	}
}

func TestLoginRejectsInvalidInputsBeforeAnyRequest(t *testing.T) {
	harness := newLoginHarness(t, issuedLoginClient(time.Time{}), nil)
	for _, test := range []struct {
		code string
		days int
	}{
		{days: 0}, {days: 2}, {days: 365}, {code: "12345", days: 7}, {code: "1234567", days: 7},
	} {
		if _, err := harness.adapter.Login(context.Background(), loginCredentials(t, test.code), test.days); err == nil {
			t.Fatalf("code=%q days=%d accepted", test.code, test.days)
		}
	}
	if _, err := harness.adapter.Login(context.Background(), site.LoginCredentials{}, 7); err == nil {
		t.Fatal("empty credentials accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := harness.adapter.Login(canceled, loginCredentials(t, ""), 7); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled err=%v", err)
	}
	if len(harness.cookies) != 0 || harness.login.calls != 0 {
		t.Fatalf("clients=%d calls=%d", len(harness.cookies), harness.login.calls)
	}
}

func TestLoginTransportFailureIsNotRetried(t *testing.T) {
	harness := newLoginHarness(t, &loginFakeClient{err: fmt.Errorf("site request failed"), response: httpguard.StrictResponse{Body: twoFactorPage()}}, nil)
	result, err := harness.adapter.Login(context.Background(), loginCredentials(t, "123456"), 7)
	var stopped *site.LoginError
	if !errors.As(err, &stopped) || stopped.Reason != site.LoginRequestFailed || stopped.AttemptsRemaining != nil ||
		harness.login.calls != 1 || harness.login.closeCalls != 1 || result.RequestsAttempted != 1 || harness.verify.requests != 0 {
		t.Fatalf("err=%v calls=%d requests=%d", err, harness.login.calls, result.RequestsAttempted)
	}
	assertNoLoginSecrets(t, err, result)

	for _, test := range []struct {
		postErr error
		want    error
	}{
		{postErr: fmt.Errorf("site request canceled: %w", context.DeadlineExceeded), want: context.DeadlineExceeded},
		{postErr: fmt.Errorf("site request canceled: %w", context.Canceled), want: context.Canceled},
		{postErr: fmt.Errorf("site request failed: %w", httpguard.ErrBlockedAddress), want: site.ErrAddressBlocked},
	} {
		harness = newLoginHarness(t, &loginFakeClient{err: test.postErr}, nil)
		if _, err := harness.adapter.Login(context.Background(), loginCredentials(t, ""), 7); !errors.Is(err, test.want) || errors.As(err, &stopped) || harness.login.calls != 1 {
			t.Fatalf("post err %v became %v", test.postErr, err)
		}
	}

	harness = newLoginHarness(t, issuedLoginClient(time.Time{}), nil)
	harness.factoryErr = fmt.Errorf("site URL must be an HTTPS location")
	if _, err := harness.adapter.Login(context.Background(), loginCredentials(t, ""), 7); err == nil || harness.login.calls != 0 {
		t.Fatalf("factory err=%v calls=%d", err, harness.login.calls)
	}

	readOnly := New("")
	readOnly.newClient = func(string, string, time.Duration) (guardedClient, error) { return &accountFakeClient{}, nil }
	if _, err := readOnly.Login(context.Background(), loginCredentials(t, ""), 7); err == nil || !strings.Contains(err.Error(), "login client") {
		t.Fatalf("client without login port err=%v", err)
	}
}

func TestLoginPauseHonorsCancellation(t *testing.T) {
	adapter := New("")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := adapter.pause(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("pause err=%v", err)
	}
	if err := adapter.pause(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("pause err=%v", err)
	}
}
