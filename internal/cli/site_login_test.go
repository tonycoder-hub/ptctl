package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const loginSessionCanary = "access_token=SESSION-CANARY"

type fakeLoginAdapter struct {
	result      site.LoginResult
	err         error
	calls       int
	username    string
	password    string
	code        string
	hasCode     bool
	sessionDays int
}

func (*fakeLoginAdapter) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID: "fakept", Name: "Fake PT", BaseURL: "https://fake.invalid/", Stability: "test",
		AuthMethods:  []domain.AuthMethod{domain.AuthMethodCookieHeader},
		Capabilities: []domain.Capability{domain.CapabilityLogin},
	}
}

func (*fakeLoginAdapter) LoginConfig() site.LoginConfig {
	return site.LoginConfig{SessionDays: []int{1, 7, 30}, DefaultSessionDays: 7}
}

func (adapter *fakeLoginAdapter) Login(_ context.Context, credentials site.LoginCredentials, sessionDays int) (site.LoginResult, error) {
	adapter.calls++
	adapter.username = credentials.Username()
	adapter.password = credentials.Password().SecretValue()
	adapter.code, adapter.hasCode = credentials.OneTimeCode()
	adapter.sessionDays = sessionDays
	return adapter.result, adapter.err
}

type readOnlyFakeSite struct{}

func (readOnlyFakeSite) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID: "readonly", Name: "Read Only", BaseURL: "https://readonly.invalid/", Stability: "test",
		AuthMethods: []domain.AuthMethod{domain.AuthMethodCookieHeader},
	}
}

func successfulLoginAdapter(t *testing.T) *fakeLoginAdapter {
	t.Helper()
	session, err := site.NewCookieCredential(loginSessionCanary)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Date(2026, 10, 22, 15, 4, 33, 0, time.UTC)
	return &fakeLoginAdapter{result: site.LoginResult{Session: session, Username: "CanaryUser", ExpiresAt: &expires, RequestsAttempted: 2}}
}

func loginBundle(fields ...string) string {
	return `{"schema":"ptctl.credentials/v1","site_username":"CanaryUser","site_password":"PASSWORD-CANARY"` + strings.Join(fields, "") + `}`
}

func runSiteLogin(adapter site.Adapter, stdin io.Reader, stdout io.Writer, args ...string) (int, string) {
	var errOut bytes.Buffer
	a := &app{stdin: stdin, stdout: stdout, stderr: &errOut, registry: site.NewRegistry(adapter, readOnlyFakeSite{})}
	code := runWithApp(append([]string{"site", "login"}, args...), a)
	return code, errOut.String()
}

func assertNoLoginLeak(t *testing.T, outputs ...string) {
	t.Helper()
	for _, output := range outputs {
		for _, secret := range []string{"PASSWORD-CANARY", "SESSION-CANARY", "123456", "654321"} {
			if strings.Contains(output, secret) {
				t.Fatalf("secret %q leaked: %q", secret, output)
			}
		}
	}
}

func TestSiteLoginWritesOnlyTheSessionCookieToStdout(t *testing.T) {
	adapter := successfulLoginAdapter(t)
	var out bytes.Buffer
	code, stderr := runSiteLogin(adapter, strings.NewReader(loginBundle(`,"site_totp":" 123456\n"`)), &out, "--credential-bundle-stdin", "fakept")
	if code != 0 || out.String() != loginSessionCanary+"\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr)
	}
	if stderr != "site login succeeded: site=fakept username=CanaryUser session_expires=2026-10-22T15:04:33Z requests=2\n" {
		t.Fatalf("stderr=%q", stderr)
	}
	if adapter.calls != 1 || adapter.username != "CanaryUser" || adapter.password != "PASSWORD-CANARY" || !adapter.hasCode || adapter.code != "123456" || adapter.sessionDays != 7 {
		t.Fatalf("login calls=%d username=%q days=%d hasCode=%v", adapter.calls, adapter.username, adapter.sessionDays, adapter.hasCode)
	}

	adapter = successfulLoginAdapter(t)
	adapter.result.ExpiresAt = nil
	out.Reset()
	code, stderr = runSiteLogin(adapter, strings.NewReader(loginBundle()), &out, "--credential-bundle-stdin", "--session-days", "30", "fakept")
	if code != 0 || adapter.hasCode || adapter.sessionDays != 30 || !strings.Contains(stderr, "session_expires=unknown") {
		t.Fatalf("code=%d hasCode=%v days=%d stderr=%q", code, adapter.hasCode, adapter.sessionDays, stderr)
	}
}

func TestSiteLoginUsageErrorsNeverReadStdin(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"fakept"},
		{"--credential-bundle-stdin"},
		{"--credential-bundle-stdin", "fakept", "extra"},
		{"--credential-bundle-stdin", "--password", "PASSWORD-CANARY", "fakept"},
		{"--credential-bundle-stdin", "unknown"},
		{"--credential-bundle-stdin", "readonly"},
		{"--credential-bundle-stdin", "--session-days", "2", "fakept"},
		{"--credential-bundle-stdin", "--session-days", "-7", "fakept"},
	} {
		adapter := successfulLoginAdapter(t)
		reader := &trackingReader{}
		var out bytes.Buffer
		code, stderr := runSiteLogin(adapter, reader, &out, args...)
		if code != 2 || reader.read || adapter.calls != 0 || out.Len() != 0 {
			t.Fatalf("args=%q code=%d read=%v calls=%d stderr=%q", args, code, reader.read, adapter.calls, stderr)
		}
		assertNoLoginLeak(t, stderr)
	}
	reader := &trackingReader{}
	var out, errOut bytes.Buffer
	if code := Run([]string{"site", "login", "--credential-bundle-stdin", "mteam"}, reader, &out, &errOut); code != 2 || reader.read ||
		!strings.Contains(errOut.String(), `site "mteam" does not declare capability "auth.login_effectful"`) {
		t.Fatalf("mteam code=%d read=%v stderr=%q", code, reader.read, errOut.String())
	}
}

func TestSiteLoginRejectsInvalidBundlesWithoutContactingTheSite(t *testing.T) {
	tests := []struct {
		bundle string
		want   string
	}{
		{bundle: "", want: "site login credential bundle is invalid"},
		{bundle: `{"schema":"ptctl.credentials/v1","site_username":"CanaryUser"}`, want: "site login credential bundle is invalid"},
		{bundle: loginBundle(`,"site_cookie":"SESSION-CANARY"`), want: "site login credential bundle is invalid"},
		{bundle: loginBundle(`,"site_totp":"123456","site_totp":"654321"`), want: "site login credential bundle is invalid"},
		{bundle: strings.Replace(loginBundle(), "ptctl.credentials/v1", "ptctl.credentials/v2", 1), want: "site login credential bundle schema is unsupported"},
		{bundle: loginBundle(`,"site_totp":"  "`), want: "site login one-time code is invalid"},
		{bundle: loginBundle(`,"site_totp":"12a456"`), want: "site login one-time code is invalid"},
		{bundle: strings.Replace(loginBundle(), `"CanaryUser"`, `" CanaryUser"`, 1), want: "site login username is invalid"},
		{bundle: strings.Replace(loginBundle(), `"PASSWORD-CANARY"`, `""`, 1), want: "site login account password is invalid"},
	}
	for _, test := range tests {
		adapter := successfulLoginAdapter(t)
		var out bytes.Buffer
		code, stderr := runSiteLogin(adapter, strings.NewReader(test.bundle), &out, "--credential-bundle-stdin", "fakept")
		if code != 1 || adapter.calls != 0 || out.Len() != 0 || stderr != "error: "+test.want+"\n" {
			t.Fatalf("bundle=%q code=%d calls=%d stderr=%q", test.bundle, code, adapter.calls, stderr)
		}
	}
}

func TestSiteLoginFailuresUseFixedPublicText(t *testing.T) {
	nine := 9
	tests := []struct {
		err  error
		want string
	}{
		{err: &site.LoginError{Reason: site.LoginTwoFactorRejected, AttemptsRemaining: &nine}, want: "site login stopped: login_two_factor_rejected (the site reports 9 login attempts left before it blocks this IP)"},
		{err: &site.LoginError{Reason: site.LoginTwoFactorRequired}, want: "site login stopped: login_two_factor_required"},
		{err: &site.LoginError{Reason: site.LoginCredentialsRejected}, want: "site login stopped: login_credentials_rejected"},
		{err: &site.LoginError{Reason: site.LoginSessionUnverified}, want: "site login stopped: login_session_unverified"},
		{err: &site.LoginError{Reason: site.LoginRequestFailed}, want: "site login stopped: login_request_failed"},
		{err: fmt.Errorf("TJUPT login request failed: %w", site.ErrAddressBlocked), want: "site login failed: " + site.ErrAddressBlocked.Error()},
		{err: &site.LoginError{Reason: "PASSWORD-CANARY"}, want: "site login failed"},
		{err: fmt.Errorf("post password=PASSWORD-CANARY cookie=SESSION-CANARY"), want: "site login failed"},
		{err: fmt.Errorf("wrapped: %w", site.ErrRateLimited), want: "site login failed: " + site.ErrRateLimited.Error()},
		{err: fmt.Errorf("site request canceled: %w", context.DeadlineExceeded), want: "site login timed out: context deadline exceeded"},
	}
	for _, test := range tests {
		adapter := successfulLoginAdapter(t)
		adapter.err = test.err
		var out bytes.Buffer
		code, stderr := runSiteLogin(adapter, strings.NewReader(loginBundle(`,"site_totp":"123456"`)), &out, "--credential-bundle-stdin", "fakept")
		if code != 1 || out.Len() != 0 || stderr != "error: "+test.want+"\n" {
			t.Fatalf("err=%v code=%d stdout=%q stderr=%q", test.err, code, out.String(), stderr)
		}
		assertNoLoginLeak(t, stderr)
	}
}

func TestSiteLoginRefusesUnsafeStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file modes do not express group or other access")
	}
	dir := t.TempDir()
	shared, err := os.OpenFile(filepath.Join(dir, "shared-cookie"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	if err := shared.Chmod(0o644); err != nil {
		t.Fatal(err)
	}
	device, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	for _, stdout := range []*os.File{shared, device} {
		adapter := successfulLoginAdapter(t)
		reader := &trackingReader{}
		code, stderr := runSiteLogin(adapter, reader, stdout, "--credential-bundle-stdin", "fakept")
		if code != 2 || reader.read || adapter.calls != 0 || !strings.Contains(stderr, "refusing to write a session cookie") {
			t.Fatalf("stdout=%s code=%d read=%v stderr=%q", stdout.Name(), code, reader.read, stderr)
		}
	}

	private, err := os.OpenFile(filepath.Join(dir, "private-cookie"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := private.Chmod(0o600); err != nil {
		t.Fatal(err)
	}
	adapter := successfulLoginAdapter(t)
	code, stderr := runSiteLogin(adapter, strings.NewReader(loginBundle()), private, "--credential-bundle-stdin", "fakept")
	if closeErr := private.Close(); code != 0 || closeErr != nil {
		t.Fatalf("private file code=%d stderr=%q close=%v", code, stderr, closeErr)
	}
	written, err := os.ReadFile(filepath.Join(dir, "private-cookie"))
	if err != nil || string(written) != loginSessionCanary+"\n" {
		t.Fatalf("private file content mismatch err=%v", err)
	}
	if session, err := readCredential(bytes.NewReader(written)); err != nil || session.SecretValue() != loginSessionCanary {
		t.Fatalf("written cookie is not readable by --cookie-stdin: err=%v", err)
	}
}

// fakeTerminal answers prompts in order and records whether each read had
// echo off. It reports EOF once the answers run out.
type fakeTerminal struct {
	answers []string
	secret  []bool
}

func (terminal *fakeTerminal) next(secret bool) (string, error) {
	terminal.secret = append(terminal.secret, secret)
	if len(terminal.answers) == 0 {
		return "", io.EOF
	}
	answer := terminal.answers[0]
	terminal.answers = terminal.answers[1:]
	return answer, nil
}

func (terminal *fakeTerminal) ReadLine() (string, error) { return terminal.next(false) }

func (terminal *fakeTerminal) ReadSecret(showPrompt func()) (string, error) {
	showPrompt()
	return terminal.next(true)
}

// useTempConfigDir points the user config directory at a temporary directory
// on every platform and returns the default cookie path for fakept.
func useTempConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("AppData", dir)
	config, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(config, "ptctl", "sessions", "fakept.cookie")
}

func runInteractiveLogin(adapter site.Adapter, terminal *fakeTerminal, stdout io.Writer, args ...string) (int, string) {
	var errOut bytes.Buffer
	a := &app{
		stdin: &trackingReader{}, stdout: stdout, stderr: &errOut,
		registry: site.NewRegistry(adapter, readOnlyFakeSite{}),
		terminal: func(io.Reader) (promptTerminal, bool) { return terminal, terminal != nil },
	}
	code := runWithApp(append([]string{"site", "login"}, args...), a)
	return code, errOut.String()
}

func assertOnlyEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("%s holds %q, want %q", dir, names, want)
	}
}

func TestSiteLoginPromptsByDefaultAndSavesTheCookieOwnerOnly(t *testing.T) {
	cookiePath := useTempConfigDir(t)
	adapter := successfulLoginAdapter(t)
	terminal := &fakeTerminal{answers: []string{" CanaryUser ", "PASSWORD-CANARY", " 123 456 "}}
	var out bytes.Buffer
	code, stderr := runInteractiveLogin(adapter, terminal, &out, "fakept")
	if code != 0 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr)
	}
	if fmt.Sprint(terminal.secret) != "[false true true]" {
		t.Fatalf("echo-off reads=%v, want username echoed and password and code hidden", terminal.secret)
	}
	if adapter.calls != 1 || adapter.username != "CanaryUser" || adapter.password != "PASSWORD-CANARY" || !adapter.hasCode || adapter.code != "123456" || adapter.sessionDays != 7 {
		t.Fatalf("login calls=%d username=%q days=%d hasCode=%v", adapter.calls, adapter.username, adapter.sessionDays, adapter.hasCode)
	}
	written, err := os.ReadFile(cookiePath)
	if err != nil || string(written) != loginSessionCanary+"\n" {
		t.Fatalf("cookie file content mismatch err=%v", err)
	}
	if session, err := readCredential(bytes.NewReader(written)); err != nil || session.SecretValue() != loginSessionCanary {
		t.Fatalf("saved cookie is not readable by --cookie-stdin: err=%v", err)
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{cookiePath: 0o600, filepath.Dir(cookiePath): 0o700} {
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
				t.Fatalf("%s mode=%v err=%v, want %v", path, info.Mode().Perm(), err, want)
			}
		}
	}
	assertOnlyEntries(t, filepath.Dir(cookiePath), "fakept.cookie")
	for _, want := range []string{
		"Username: ", "Password (hidden): ", "Two-step code (hidden; press Enter if none): ",
		"site login succeeded: site=fakept username=CanaryUser session_expires=2026-10-22T15:04:33Z requests=2\n",
		"session cookie saved to " + cookiePath + " (owner-only)\n",
		"use it with: ",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr lacks %q: %q", want, stderr)
		}
	}
	assertNoLoginLeak(t, stderr)

	// A second login replaces the file, and a looser mode does not survive.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(cookiePath, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	adapter = successfulLoginAdapter(t)
	terminal = &fakeTerminal{answers: []string{"CanaryUser", "PASSWORD-CANARY", ""}}
	code, stderr = runInteractiveLogin(adapter, terminal, &out, "--session-days", "30", "fakept")
	if code != 0 || adapter.hasCode || adapter.sessionDays != 30 {
		t.Fatalf("relogin code=%d hasCode=%v days=%d stderr=%q", code, adapter.hasCode, adapter.sessionDays, stderr)
	}
	if info, err := os.Stat(cookiePath); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("replaced cookie file err=%v", err)
	}
	assertOnlyEntries(t, filepath.Dir(cookiePath), "fakept.cookie")
}

func TestSiteLoginReportsJSONWithoutTheCookie(t *testing.T) {
	useTempConfigDir(t)
	dir := t.TempDir()
	for _, mode := range []string{"prompt", "bundle"} {
		adapter := successfulLoginAdapter(t)
		cookiePath := filepath.Join(dir, mode+".cookie")
		var out bytes.Buffer
		var code int
		var stderr string
		if mode == "prompt" {
			terminal := &fakeTerminal{answers: []string{"CanaryUser", "PASSWORD-CANARY", "123456"}}
			code, stderr = runInteractiveLogin(adapter, terminal, &out, "--cookie-file", cookiePath, "--output", "json", "fakept")
		} else {
			code, stderr = runSiteLogin(adapter, strings.NewReader(loginBundle()), &out, "--credential-bundle-stdin", "--cookie-file", cookiePath, "--output", "json", "fakept")
		}
		want := `{
  "schema": "ptctl.dev/v1",
  "kind": "site.login",
  "data": {
    "site_id": "fakept",
    "username": "CanaryUser",
    "session_days": 7,
    "session_expires_at": "2026-10-22T15:04:33Z",
    "cookie_file": ` + strconv.Quote(cookiePath) + `,
    "requests_attempted": 2
  }
}
`
		if code != 0 || out.String() != want || strings.Contains(stderr, "site login succeeded") {
			t.Fatalf("%s: code=%d stdout=%s stderr=%q", mode, code, out.String(), stderr)
		}
		if written, err := os.ReadFile(cookiePath); err != nil || string(written) != loginSessionCanary+"\n" {
			t.Fatalf("%s: cookie file content mismatch err=%v", mode, err)
		}
		assertNoLoginLeak(t, out.String(), stderr)
	}
	assertOnlyEntries(t, dir, "bundle.cookie", "prompt.cookie")
}

func TestSiteLoginInteractiveUsageErrorsNeverPrompt(t *testing.T) {
	useTempConfigDir(t)
	dir := t.TempDir()
	for _, args := range [][]string{
		{"--cookie-file", dir, "fakept"},
		{"--cookie-file", filepath.Join(dir, "missing", "fakept.cookie"), "fakept"},
		{"--output", "yaml", "fakept"},
		{"--session-days", "2", "fakept"},
		{"unknown"},
		{"readonly"},
		{"--credential-bundle-stdin", "--output", "json", "fakept"},
	} {
		adapter := successfulLoginAdapter(t)
		terminal := &fakeTerminal{answers: []string{"CanaryUser", "PASSWORD-CANARY", ""}}
		var out bytes.Buffer
		code, stderr := runInteractiveLogin(adapter, terminal, &out, args...)
		if code != 2 || len(terminal.secret) != 0 || adapter.calls != 0 || out.Len() != 0 || strings.Contains(stderr, "Username:") {
			t.Fatalf("args=%q code=%d prompts=%d calls=%d stderr=%q", args, code, len(terminal.secret), adapter.calls, stderr)
		}
	}
	assertOnlyEntries(t, dir)

	adapter := successfulLoginAdapter(t)
	var out bytes.Buffer
	code, stderr := runInteractiveLogin(adapter, nil, &out, "fakept")
	if code != 2 || adapter.calls != 0 || !strings.Contains(stderr, "pipe the credentials with --credential-bundle-stdin") {
		t.Fatalf("no terminal code=%d calls=%d stderr=%q", code, adapter.calls, stderr)
	}
}

func TestSiteLoginInteractiveFailuresKeepNoCookieFile(t *testing.T) {
	cookiePath := useTempConfigDir(t)
	tests := []struct {
		answers []string
		err     error
		want    string
	}{
		{answers: []string{"CanaryUser"}, want: "site login canceled before anything was sent"},
		{answers: []string{"", "PASSWORD-CANARY", ""}, want: "site login username is invalid"},
		{answers: []string{"CanaryUser", "", ""}, want: "site login account password is invalid"},
		{answers: []string{"CanaryUser", "PASSWORD-CANARY", "12a456"}, want: "site login one-time code is invalid"},
		{answers: []string{"CanaryUser", "PASSWORD-CANARY", "123456"}, err: &site.LoginError{Reason: site.LoginTwoFactorRejected}, want: "site login stopped: login_two_factor_rejected"},
	}
	for _, test := range tests {
		adapter := successfulLoginAdapter(t)
		adapter.err = test.err
		var out bytes.Buffer
		code, stderr := runInteractiveLogin(adapter, &fakeTerminal{answers: test.answers}, &out, "fakept")
		if code != 1 || out.Len() != 0 || !strings.HasSuffix(stderr, "error: "+test.want+"\n") {
			t.Fatalf("answers=%q code=%d stderr=%q", test.answers, code, stderr)
		}
		if wantCalls := map[bool]int{true: 1, false: 0}[test.err != nil]; adapter.calls != wantCalls {
			t.Fatalf("answers=%q login calls=%d", test.answers, adapter.calls)
		}
		assertOnlyEntries(t, filepath.Dir(cookiePath))
		assertNoLoginLeak(t, stderr)
	}

	// A failed login leaves an earlier cookie file untouched.
	if err := os.WriteFile(cookiePath, []byte("access_token=OLD\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := successfulLoginAdapter(t)
	adapter.err = &site.LoginError{Reason: site.LoginCredentialsRejected}
	if code, stderr := runInteractiveLogin(adapter, &fakeTerminal{answers: []string{"CanaryUser", "PASSWORD-CANARY", ""}}, io.Discard, "fakept"); code != 1 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if written, err := os.ReadFile(cookiePath); err != nil || string(written) != "access_token=OLD\n" {
		t.Fatalf("earlier cookie file changed: %q err=%v", written, err)
	}
	assertOnlyEntries(t, filepath.Dir(cookiePath), "fakept.cookie")
}

func TestSiteLoginHelpExplainsEveryFlag(t *testing.T) {
	for _, args := range [][]string{{"site", "login", "--help"}, {"help", "login"}, {"help", "site", "login"}} {
		reader := &trackingReader{}
		var out, errOut bytes.Buffer
		if code := Run(args, reader, &out, &errOut); code != 0 || reader.read || errOut.Len() != 0 || out.String() != siteLoginHelp {
			t.Fatalf("args=%q code=%d stderr=%q", args, code, errOut.String())
		}
	}
	for _, want := range []string{
		"--cookie-file PATH", "--session-days N", "--output table|json", "--credential-bundle-stdin",
		`"schema":"ptctl.credentials/v1"`, "site_totp", "<user config dir>/ptctl/sessions/SITE.cookie",
		site.LoginCredentialsRejected, site.LoginTwoFactorRequired, site.LoginTwoFactorRejected,
		site.LoginResponseUnrecognized, site.LoginSessionUnverified, site.LoginRequestFailed,
	} {
		if !strings.Contains(siteLoginHelp, want) {
			t.Fatalf("site login help lacks %q", want)
		}
	}
}
