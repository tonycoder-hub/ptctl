package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/bonusexchange"
	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const (
	savedCookieCanary   = "sid=SAVED-COOKIE-CANARY"
	savedAPIKeyCanary   = "SAVED-API-KEY-CANARY"
	terminalPasswordCan = "PASSWORD-TERMINAL-CANARY"
)

// writeSavedCredential saves content where 'site login' would for siteID,
// owner-only, inside the temporary config directory of the test.
func writeSavedCredential(t *testing.T, siteID string, method domain.AuthMethod, content string) string {
	t.Helper()
	path, err := savedCredentialPath(siteID, method)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func authenticatedAPIKeySite() *apiKeySiteAdapter {
	return &apiKeySiteAdapter{status: domain.SessionStatus{
		State: domain.AuthenticationAuthenticated, Username: "Alice", ObservedAt: time.Date(2026, 10, 16, 1, 2, 3, 0, time.UTC),
	}}
}

func TestSavedCredentialServesEverySiteCommandWithoutReadingStdin(t *testing.T) {
	useTempConfigDir(t)
	writeSavedCredential(t, "api-key-test", domain.AuthMethodAPIKeyHeader, savedAPIKeyCanary+"\n")
	writeSavedCredential(t, "fakept", domain.AuthMethodAPIKeyHeader, savedAPIKeyCanary+"\r\n")

	for _, args := range [][]string{
		{"status", "--output", "json", "api-key-test"},
		{"search", "--output", "json", "api-key-test", "query"},
	} {
		adapter := authenticatedAPIKeySite()
		stdin := &trackingReader{}
		var out, errOut bytes.Buffer
		a := &app{stdin: stdin, stdout: &out, stderr: &errOut, registry: site.NewRegistry(adapter)}
		if err := a.siteRead(args[0], args[1:]); err != nil {
			t.Fatalf("%s: %v", args[0], err)
		}
		if stdin.read || adapter.calls != 1 || adapter.method != domain.AuthMethodAPIKeyHeader || adapter.secret != savedAPIKeyCanary ||
			strings.Contains(out.String()+errOut.String(), "CANARY") {
			t.Fatalf("%s: read=%t calls=%d method=%q stdout=%q stderr=%q", args[0], stdin.read, adapter.calls, adapter.method, out.String(), errOut.String())
		}
	}

	detail := successfulFakeTorrentDetail(t)
	detail.authMethod = domain.AuthMethodAPIKeyHeader
	stdin := &trackingReader{}
	var out bytes.Buffer
	a := &app{stdin: stdin, stdout: &out, stderr: &bytes.Buffer{}, registry: site.NewRegistry(detail)}
	if err := a.siteDetail([]string{"--output", "json", "fakept", "42"}); err != nil || stdin.read || detail.opened != 1 ||
		detail.credential != savedAPIKeyCanary || strings.Contains(out.String(), "CANARY") {
		t.Fatalf("detail err=%v read=%t opened=%d stdout=%q", err, stdin.read, detail.opened, out.String())
	}

	// A cookie site finds its own file; the API key saved for the same site
	// is never sent in its place.
	writeSavedCredential(t, "fakept", domain.AuthMethodCookieHeader, savedCookieCanary+"\n")
	review := successfulFakeBonusReview(t)
	stdin, out = &trackingReader{}, bytes.Buffer{}
	a = &app{stdin: stdin, stdout: &out, stderr: &bytes.Buffer{}, registry: site.NewRegistry(review)}
	if err := a.siteBonusReview([]string{"--output", "json", "fakept", "1"}); err != nil || stdin.read || review.opened != 1 ||
		review.credential != savedCookieCanary || strings.Contains(out.String(), "CANARY") {
		t.Fatalf("bonus review err=%v read=%t opened=%d stdout=%q", err, stdin.read, review.opened, out.String())
	}

	storeRoot := filepath.Join(physicalCLITempDir(t), "store")
	if _, _, err := metastore.Init(storeRoot); err != nil {
		t.Fatal(err)
	}
	fetch := &fakeMetafileFetchAdapter{raw: sitePrivateTrackerMetafile("saved.bin", []byte("x"), "SAVED-PASSKEY-CANARY")}
	stdin, out = &trackingReader{}, bytes.Buffer{}
	a = &app{stdin: stdin, stdout: &out, stderr: &bytes.Buffer{}, registry: site.NewRegistry(fetch)}
	if err := a.siteMetafileFetch([]string{"--acknowledge-site-effect", "--metafile-store", storeRoot, "--output", "json", "fakept", "42"}); err != nil ||
		stdin.read || fetch.opened != 1 || fetch.lastCredential != savedCookieCanary || strings.Contains(out.String(), "CANARY") {
		t.Fatalf("metafile fetch err=%v read=%t opened=%d stdout=%q", err, stdin.read, fetch.opened, out.String())
	}
	assertDirectoryExcludesText(t, storeRoot, savedCookieCanary)
}

func TestExplicitCredentialFlagWinsOverTheSavedOne(t *testing.T) {
	useTempConfigDir(t)
	writeSavedCredential(t, "api-key-test", domain.AuthMethodAPIKeyHeader, savedAPIKeyCanary+"\n")
	adapter := authenticatedAPIKeySite()
	a := &app{stdin: strings.NewReader(apiKeyCanary + "\n"), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, registry: site.NewRegistry(adapter)}
	if err := a.siteRead("status", []string{"--api-key-stdin", "api-key-test"}); err != nil || adapter.secret != apiKeyCanary {
		t.Fatalf("err=%v explicit key sent=%t", err, adapter.secret == apiKeyCanary)
	}
}

func TestSavedCredentialThatIsUnsafeOrInvalidIsRefusedWithoutDisclosure(t *testing.T) {
	type savedCase struct {
		name  string
		setup func(t *testing.T, path string)
		want  string
	}
	write := func(content string, mode os.FileMode) func(*testing.T, string) {
		return func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(content), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []savedCase{
		{name: "empty", setup: write("", 0o600), want: "is invalid; sign in again with 'ptctl site login'"},
		{name: "blank line", setup: write("\n", 0o600), want: "is invalid; sign in again with 'ptctl site login'"},
		{name: "inner space", setup: write("SAVED-API KEY-CANARY\n", 0o600), want: "is invalid; sign in again with 'ptctl site login'"},
		{name: "two lines", setup: write(savedAPIKeyCanary+"\nSECOND-LINE-CANARY\n", 0o600), want: "is invalid; sign in again with 'ptctl site login'"},
		{name: "extra newline", setup: write(savedAPIKeyCanary+"\n\n", 0o600), want: "is invalid; sign in again with 'ptctl site login'"},
		{name: "oversized", setup: write(strings.Repeat("A", maxSavedCredentialBytes+1), 0o600), want: "is invalid; sign in again with 'ptctl site login'"},
		{name: "directory", setup: func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, want: "it is not a regular file"},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases,
			savedCase{name: "group readable", setup: write(savedAPIKeyCanary+"\n", 0o640), want: "other users can access it; make it owner-only (chmod 600) or sign in again"},
			savedCase{name: "symlink", setup: func(t *testing.T, path string) {
				target := filepath.Join(t.TempDir(), "target")
				write(savedAPIKeyCanary+"\n", 0o600)(t, target)
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}, want: "it is not a regular file"},
		)
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			useTempConfigDir(t)
			path := writeSavedCredential(t, "api-key-test", domain.AuthMethodAPIKeyHeader, "")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			test.setup(t, path)
			adapter := authenticatedAPIKeySite()
			stdin := &trackingReader{}
			var out, errOut bytes.Buffer
			a := &app{stdin: stdin, stdout: &out, stderr: &errOut, registry: site.NewRegistry(adapter)}
			code := runWithApp([]string{"site", "status", "api-key-test"}, a)
			if code != 1 || stdin.read || adapter.calls != 0 || out.Len() != 0 ||
				!strings.Contains(errOut.String(), "saved API key "+path) || !strings.Contains(errOut.String(), test.want) ||
				strings.Contains(errOut.String(), "CANARY") || strings.Contains(errOut.String(), "AAAA") {
				t.Fatalf("code=%d read=%t calls=%d stdout=%q stderr=%q", code, stdin.read, adapter.calls, out.String(), errOut.String())
			}
		})
	}
}

func TestSiteLoginPromptsForAnAPIKeyAndSavesItOnlyAfterTheCheck(t *testing.T) {
	useTempConfigDir(t)
	path, err := savedCredentialPath("api-key-test", domain.AuthMethodAPIKeyHeader)
	if err != nil {
		t.Fatal(err)
	}
	adapter := authenticatedAPIKeySite()
	terminal := &fakeTerminal{answers: []string{" " + apiKeyCanary + " \n"}}
	var out bytes.Buffer
	code, stderr := runInteractiveLogin(adapter, terminal, &out, "api-key-test")
	if code != 0 || out.Len() != 0 || fmt.Sprint(terminal.secret) != "[true]" || adapter.calls != 1 ||
		adapter.method != domain.AuthMethodAPIKeyHeader || adapter.secret != apiKeyCanary {
		t.Fatalf("code=%d stdout=%q hidden=%v calls=%d stderr=%q", code, out.String(), terminal.secret, adapter.calls, stderr)
	}
	if written, err := os.ReadFile(path); err != nil || string(written) != apiKeyCanary+"\n" {
		t.Fatalf("saved API key content mismatch err=%v", err)
	}
	if runtime.GOOS != "windows" {
		for check, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
			if info, err := os.Stat(check); err != nil || info.Mode().Perm() != want {
				t.Fatalf("%s err=%v, want mode %v", check, err, want)
			}
		}
	}
	assertOnlyEntries(t, filepath.Dir(path), "api-key-test.api-key")
	if want := "Saving the API key for API key test; one read checks it first.\nAPI key (hidden): \n" +
		"site login succeeded: site=api-key-test username=Alice requests=1\n" +
		"API key saved to " + path + " (owner-only)\n" +
		"site commands now use it when no --api-key-stdin is given, for example: ptctl site status api-key-test\n"; stderr != want {
		t.Fatalf("stderr=%q\nwant  %q", stderr, want)
	}

	// After login, the plain command works.
	stdin := &trackingReader{}
	status := authenticatedAPIKeySite()
	a := &app{stdin: stdin, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, registry: site.NewRegistry(status)}
	if err := a.siteRead("status", []string{"api-key-test"}); err != nil || stdin.read || status.secret != apiKeyCanary {
		t.Fatalf("status after login err=%v read=%t", err, stdin.read)
	}
}

func TestSiteLoginSavesAPipedAPIKeyAndReportsJSON(t *testing.T) {
	useTempConfigDir(t)
	path, err := savedCredentialPath("api-key-test", domain.AuthMethodAPIKeyHeader)
	if err != nil {
		t.Fatal(err)
	}
	adapter := authenticatedAPIKeySite()
	var out bytes.Buffer
	code, stderr := runSiteLogin(adapter, strings.NewReader(apiKeyCanary+"\n"), &out, "--api-key-stdin", "--output", "json", "api-key-test")
	if code != 0 || stderr != "" || adapter.calls != 1 || adapter.secret != apiKeyCanary || strings.Contains(out.String(), "CANARY") {
		t.Fatalf("code=%d calls=%d stdout=%q stderr=%q", code, adapter.calls, out.String(), stderr)
	}
	var envelope struct {
		Kind string                     `json:"kind"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	wantPath, _ := json.Marshal(path)
	if envelope.Kind != "site.login" || string(envelope.Data["site_id"]) != `"api-key-test"` || string(envelope.Data["username"]) != `"Alice"` ||
		string(envelope.Data["api_key_file"]) != string(wantPath) || string(envelope.Data["requests_attempted"]) != "1" || len(envelope.Data) != 4 {
		t.Fatalf("report=%s", out.String())
	}
	if written, err := os.ReadFile(path); err != nil || string(written) != apiKeyCanary+"\n" {
		t.Fatalf("saved API key content mismatch err=%v", err)
	}
}

func TestSiteLoginSavesNothingWhenTheCheckFails(t *testing.T) {
	tests := []struct {
		state domain.AuthenticationState
		err   error
		want  string
	}{
		{state: domain.AuthenticationUnauthenticated, want: "site login stopped: login_credentials_rejected"},
		{state: domain.AuthenticationIndeterminate, want: "site login stopped: login_session_unverified"},
		{err: fmt.Errorf("status: %w", site.ErrCredentialRejected), want: "site login failed: " + site.ErrCredentialRejected.Error()},
		{err: fmt.Errorf("status: %w", site.ErrRateLimited), want: "site login failed: " + site.ErrRateLimited.Error()},
		{err: errors.New("body api_key=" + apiKeyCanary), want: "site login failed"},
	}
	for _, test := range tests {
		useTempConfigDir(t)
		path := writeSavedCredential(t, "api-key-test", domain.AuthMethodAPIKeyHeader, savedAPIKeyCanary+"\n")
		adapter := &apiKeySiteAdapter{status: domain.SessionStatus{State: test.state}, err: test.err}
		var out bytes.Buffer
		code, stderr := runSiteLogin(adapter, strings.NewReader(apiKeyCanary), &out, "--api-key-stdin", "api-key-test")
		if code != 1 || adapter.calls != 1 || out.Len() != 0 || stderr != "error: "+test.want+"\n" {
			t.Fatalf("want=%q code=%d calls=%d stdout=%q stderr=%q", test.want, code, adapter.calls, out.String(), stderr)
		}
		// The previous saved key is left exactly as it was.
		if written, err := os.ReadFile(path); err != nil || string(written) != savedAPIKeyCanary+"\n" {
			t.Fatalf("want=%q replaced the saved key: err=%v", test.want, err)
		}
		assertOnlyEntries(t, filepath.Dir(path), "api-key-test.api-key")
	}
}

func TestSiteLoginSaveModeUsageErrorsNeverReadStdin(t *testing.T) {
	cookieFile := filepath.Join(t.TempDir(), "cookie")
	tests := []struct {
		adapter site.Adapter
		args    []string
		want    string
	}{
		{authenticatedAPIKeySite(), []string{"--api-key-stdin", "--cookie-stdin", "api-key-test"}, "site login accepts only one of --credential-bundle-stdin, --cookie-stdin, and --api-key-stdin"},
		{authenticatedAPIKeySite(), []string{"--api-key-stdin", "--credential-bundle-stdin", "api-key-test"}, "site login accepts only one of"},
		{authenticatedAPIKeySite(), []string{"--api-key-stdin", "--cookie-file", cookieFile, "api-key-test"}, "--cookie-file and --session-days apply only to a password login"},
		{authenticatedAPIKeySite(), []string{"--session-days", "7", "api-key-test"}, "--cookie-file and --session-days apply only to a password login"},
		{authenticatedAPIKeySite(), []string{"--cookie-stdin", "api-key-test"}, `site "api-key-test" does not support cookie_header authentication`},
		{authenticatedAPIKeySite(), []string{"api-key-test"}, "site login prompts on an interactive terminal; without one, pipe the API key with --api-key-stdin"},
		{successfulLoginAdapter(t), []string{"--api-key-stdin", "fakept"}, `site "fakept" does not support api_key_header authentication`},
		{successfulLoginAdapter(t), []string{"--cookie-stdin", "fakept"}, `site "fakept" cannot verify a saved credential because it does not declare capability "auth.check"`},
	}
	for _, test := range tests {
		useTempConfigDir(t)
		reader := &trackingReader{}
		var out bytes.Buffer
		code, stderr := runSiteLogin(test.adapter, reader, &out, test.args...)
		if code != 2 || reader.read || out.Len() != 0 || !strings.Contains(stderr, test.want) {
			t.Fatalf("args=%q code=%d read=%t stderr=%q", test.args, code, reader.read, stderr)
		}
		if api, ok := test.adapter.(*apiKeySiteAdapter); ok && api.calls != 0 {
			t.Fatalf("args=%q contacted the site", test.args)
		}
		if _, err := os.Stat(cookieFile); !os.IsNotExist(err) {
			t.Fatalf("args=%q wrote the cookie file", test.args)
		}
	}

	useTempConfigDir(t)
	path, err := savedCredentialPath("api-key-test", domain.AuthMethodAPIKeyHeader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	adapter, reader := authenticatedAPIKeySite(), &trackingReader{}
	code, stderr := runSiteLogin(adapter, reader, &bytes.Buffer{}, "--api-key-stdin", "api-key-test")
	if code != 2 || reader.read || adapter.calls != 0 ||
		!strings.Contains(stderr, "refusing to replace saved API key "+path+" because it is not a regular file") {
		t.Fatalf("directory destination code=%d read=%t stderr=%q", code, reader.read, stderr)
	}
}

func TestSiteLogoutDeletesOnlyTheSavedCredentials(t *testing.T) {
	useTempConfigDir(t)
	run := func(args ...string) (int, string, string, *fakeLoginAdapter) {
		adapter := successfulLoginAdapter(t)
		var out, errOut bytes.Buffer
		a := &app{stdin: &trackingReader{}, stdout: &out, stderr: &errOut, registry: site.NewRegistry(adapter)}
		code := runWithApp(append([]string{"site", "logout"}, args...), a)
		if adapter.calls != 0 {
			t.Fatalf("logout contacted the site")
		}
		return code, out.String(), errOut.String(), adapter
	}

	cookie := writeSavedCredential(t, "fakept", domain.AuthMethodCookieHeader, savedCookieCanary+"\n")
	apiKey := writeSavedCredential(t, "fakept", domain.AuthMethodAPIKeyHeader, savedAPIKeyCanary+"\n")
	code, stdout, stderr, _ := run("fakept")
	if want := "deleted saved session cookie " + cookie + "\ndeleted saved API key " + apiKey + "\n" +
		"nothing was sent to the site; a session cookie stays valid there until it expires, and an API key until you revoke it\n"; code != 0 || stdout != "" || stderr != want {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	assertOnlyEntries(t, filepath.Dir(cookie))
	if code, stdout, stderr, _ = run("fakept"); code != 0 || stdout != "" || stderr != "no saved credential for site fakept\n" {
		t.Fatalf("second logout code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	cookie = writeSavedCredential(t, "fakept", domain.AuthMethodCookieHeader, savedCookieCanary+"\n")
	for _, want := range [][]string{{cookie}, {}} {
		code, stdout, stderr, _ = run("--output", "json", "fakept")
		var envelope struct {
			Kind string           `json:"kind"`
			Data siteLogoutReport `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil || code != 0 || stderr != "" || envelope.Kind != "site.logout" ||
			envelope.Data.SiteID != "fakept" || envelope.Data.RemovedFiles == nil || strings.Join(envelope.Data.RemovedFiles, ",") != strings.Join(want, ",") {
			t.Fatalf("code=%d stdout=%q stderr=%q err=%v", code, stdout, stderr, err)
		}
	}

	if err := os.Mkdir(cookie, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr, _ = run("fakept"); code != 1 || stdout != "" ||
		stderr != "error: refusing to delete saved session cookie "+cookie+" because it is not a regular file\n" {
		t.Fatalf("directory code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if info, err := os.Stat(cookie); err != nil || !info.IsDir() {
		t.Fatalf("the directory was removed: %v", err)
	}
	if err := os.Remove(cookie); err != nil {
		t.Fatal(err)
	}

	if runtime.GOOS != "windows" {
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(target, []byte(savedCookieCanary+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, cookie); err != nil {
			t.Fatal(err)
		}
		if code, _, stderr, _ = run("fakept"); code != 0 || !strings.Contains(stderr, "deleted saved session cookie "+cookie) {
			t.Fatalf("symlink code=%d stderr=%q", code, stderr)
		}
		if _, err := os.Lstat(cookie); !os.IsNotExist(err) {
			t.Fatalf("the symlink is still there: %v", err)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("the symlink target was deleted: %v", err)
		}
	}

	for _, args := range [][]string{{}, {"fakept", "extra"}, {"unknown"}, {"--output", "yaml", "fakept"}} {
		if code, stdout, _, _ = run(args...); code != 2 || stdout != "" {
			t.Fatalf("args=%q code=%d stdout=%q", args, code, stdout)
		}
	}
}

func TestSiteBonusExchangeSubmitUsesTheSavedCookieAndAsksForThePassword(t *testing.T) {
	useTempConfigDir(t)
	writeSavedCredential(t, "fakept", domain.AuthMethodCookieHeader, savedCookieCanary+"\n")
	adapter := &fakePasswordBonusExchangeAdapter{fakeBonusExchangeAdapter: successfulFakeBonusExchange(t)}
	adapter.confirmationCode = "fake.bonus_exchange.confirm" + site.BonusExchangeMessageConfirmationSuffix
	stateRoot := initializedBonusExchangeStore(t)
	reviewID := adapter.review.ReviewID
	prepared := runBonusExchangeCLI(t, &app{stdin: &trackingReader{}, registry: site.NewRegistry(adapter)}, []string{
		"prepare", "--state-store", stateRoot, "--expect-review-id", reviewID, "--output", "json", "fakept", "1",
	}, false)
	intent := prepared.Operation.IntentRecord.ID.String()
	stdin := &trackingReader{}
	submit := func(terminal *fakeTerminal, stdout, stderr *bytes.Buffer, flags ...string) error {
		a := &app{stdin: stdin, stdout: stdout, stderr: stderr, registry: site.NewRegistry(adapter),
			terminal: func(io.Reader) (promptTerminal, bool) { return terminal, true }}
		return a.siteBonusExchange(append([]string{
			"submit", "--state-store", stateRoot, "--intent-record", intent, "--expect-review-id", reviewID, "--output", "json",
		}, append(flags, "fakept", "1")...))
	}

	// The acknowledgement is still required, and without it nothing is asked.
	var out, errOut bytes.Buffer
	none := &fakeTerminal{}
	var usage *usageErr
	if err := submit(none, &out, &errOut); !errors.As(err, &usage) || err.Error() != "site bonus exchange submit requires --acknowledge-bonus-exchange" ||
		len(none.secret) != 0 || out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("err=%v asked=%d stdout=%q stderr=%q", err, len(none.secret), out.String(), errOut.String())
	}
	// A terminal that gives no password stops before any session opens.
	if err := submit(none, &out, &errOut, "--acknowledge-bonus-exchange"); err == nil ||
		err.Error() != "read the account password from the terminal failed; nothing was submitted" || out.Len() != 0 || adapter.opened != 0 {
		t.Fatalf("err=%v opened=%d stdout=%q", err, adapter.opened, out.String())
	}
	if status := runBonusExchangeCLI(t, &app{stdin: &trackingReader{}, registry: site.NewRegistry(adapter)}, []string{
		"status", "--state-store", stateRoot, "--intent-record", intent, "--output", "json",
	}, false); status.Outcome != bonusexchange.StatePrepared || status.Operation.AttemptRecord != nil {
		t.Fatalf("a failed prompt changed durable state: %#v", status)
	}

	terminal := &fakeTerminal{answers: []string{terminalPasswordCan}}
	out.Reset()
	errOut.Reset()
	if err := submit(terminal, &out, &errOut, "--acknowledge-bonus-exchange"); err != nil {
		t.Fatalf("err=%v stdout=%s", err, out.String())
	}
	if stdin.read || fmt.Sprint(terminal.secret) != "[true]" || adapter.passwordOpens != 1 || adapter.cookieOnlyOpens != 0 || adapter.submissions != 1 ||
		adapter.credential != savedCookieCanary || adapter.password != terminalPasswordCan {
		t.Fatalf("read=%t hidden=%v adapter=%#v", stdin.read, terminal.secret, adapter)
	}
	if errOut.String() != "Fake PT confirms this exchange with the account password (hidden, not saved): \n" || strings.Contains(out.String(), "CANARY") {
		t.Fatalf("stderr=%q stdout=%s", errOut.String(), out.String())
	}
	var envelope struct {
		Data siteBonusExchangeReport `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || envelope.Data.Outcome != bonusexchange.StateConfirmed {
		t.Fatalf("err=%v report=%s", err, out.String())
	}
	assertDirectoryExcludesText(t, stateRoot, savedCookieCanary)
	assertDirectoryExcludesText(t, stateRoot, terminalPasswordCan)
}

func TestSiteBonusExchangeSubmitUsesTheSavedCookieWithoutATerminal(t *testing.T) {
	useTempConfigDir(t)
	writeSavedCredential(t, "fakept", domain.AuthMethodCookieHeader, savedCookieCanary+"\n")
	adapter := successfulFakeBonusExchange(t)
	stateRoot := initializedBonusExchangeStore(t)
	reviewID := adapter.review.ReviewID
	prepared := runBonusExchangeCLI(t, &app{stdin: &trackingReader{}, registry: site.NewRegistry(adapter)}, []string{
		"prepare", "--state-store", stateRoot, "--expect-review-id", reviewID, "--output", "json", "fakept", "1",
	}, false)
	stdin := &trackingReader{}
	submitted := runBonusExchangeCLI(t, &app{stdin: stdin, registry: site.NewRegistry(adapter)}, []string{
		"submit", "--state-store", stateRoot, "--intent-record", prepared.Operation.IntentRecord.ID.String(),
		"--expect-review-id", reviewID, "--acknowledge-bonus-exchange", "--output", "json", "fakept", "1",
	}, false)
	if stdin.read || submitted.Outcome != bonusexchange.StateConfirmed || adapter.opened != 1 || adapter.submissions != 1 || adapter.credential != savedCookieCanary {
		t.Fatalf("read=%t submitted=%#v adapter=%#v", stdin.read, submitted, adapter)
	}
	assertDirectoryExcludesText(t, stateRoot, savedCookieCanary)
}
