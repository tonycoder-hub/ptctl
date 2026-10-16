package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const siteLoginTimeout = 45 * time.Second

const siteLoginHelp = `Usage:
  pt site login [--cookie-file PATH] [--session-days N] [--output table|json] SITE
  pt site login --credential-bundle-stdin [--cookie-file PATH] [--session-days N] [--output table|json] SITE

Signs in to SITE with the account password and saves a new session cookie for
the --cookie-stdin commands. Sites that list the auth.login_effectful
capability support it; see 'pt site capabilities SITE'.

By default the command prompts on this terminal for the username, the
password, and the two-step code (press Enter if the account has none). The
password and code are not shown as you type. It sends one login request, then
one read that confirms the new session, and never retries. The cookie is saved,
readable only by you, to
  <user config dir>/ptctl/sessions/SITE.cookie
and the exact path is printed. Nothing reads that file automatically; pass it
on stdin, for example:
  pt site status --cookie-stdin SITE < PATH

Failed logins count toward the site's lockout (TJUPT blocks the IP after ten),
so never run login in a loop.

Flags:
  --cookie-file PATH
      Save the cookie to PATH instead. PATH is replaced atomically and is
      readable only by you; its directory must already exist.
  --session-days N
      Session length in days, one the site offers; an invalid value lists them.
      The default is the site's own default (TJUPT: 7).
  --output table|json
      json prints a site.login report on stdout with site_id, username,
      session_days, session_expires_at, cookie_file, and requests_attempted.
      It never contains the cookie.
  --credential-bundle-stdin
      For scripts and agents: read the credentials from stdin as one strict
      JSON object instead of prompting:
        {"schema":"ptctl.credentials/v1","site_username":"...",
         "site_password":"...","site_totp":"123456"}
      Leave out site_totp when the account has no two-step verification. A
      terminal on stdin is refused. Without --cookie-file the cookie, and
      nothing else, is written to stdout, which must be a pipe or an
      owner-only file, and --output json is refused.

Agent example, with no secret in any argv:
  jq -n --arg user "$SITE_USER" --rawfile password "$PASSWORD_FILE" \
        --rawfile totp <(op item get "$OP_ITEM" --otp) \
     '{schema: "ptctl.credentials/v1", site_username: $user,
       site_password: ($password | rtrimstr("\n")),
       site_totp: ($totp | rtrimstr("\n"))}' |
    pt site login --credential-bundle-stdin --cookie-file "$COOKIE_FILE" --output json tjupt

Exit status:
  0  Logged in. Text output reports the account, the expiry, and the cookie
     file on stderr.
  1  "site login stopped: REASON", where REASON is one of
       login_credentials_rejected   the site refused the username or password
       login_two_factor_required    the account needs a two-step code
       login_two_factor_rejected    the code was wrong or expired
       login_response_unrecognized  the site answered with an unknown page
       login_session_unverified     the new cookie did not show the same account
       login_request_failed         the request failed; the site may still
                                    have counted the attempt
     The line may end with "(the site reports N login attempts left before it
     blocks this IP)". Other failures print "site login failed" with an
     optional ": CAUSE".
  2  Usage error. Nothing was read or sent.
`

// siteLoginReport is the --output json result of a verified login. It says
// where the session cookie went and never contains the cookie itself.
type siteLoginReport struct {
	SiteID            string     `json:"site_id"`
	Username          string     `json:"username"`
	SessionDays       int        `json:"session_days"`
	SessionExpiresAt  *time.Time `json:"session_expires_at,omitempty"`
	CookieFile        string     `json:"cookie_file"`
	RequestsAttempted int        `json:"requests_attempted"`
}

// siteLogin signs in with an account password and saves the new session
// cookie. By default it prompts on the terminal and saves the cookie to an
// owner-only file. --credential-bundle-stdin reads the credentials from stdin
// instead and, without --cookie-file, writes the cookie and nothing else to
// stdout, so it can be piped. Everything a person needs to see goes to stderr.
func (a *app) siteLogin(args []string) error {
	fs := newFlagSet("site login")
	bundleStdin := fs.Bool("credential-bundle-stdin", false, "read a ptctl.credentials/v1 bundle with site_username, site_password, and optional site_totp from stdin instead of prompting")
	cookieFile := fs.String("cookie-file", "", "save the session cookie to this owner-only file")
	sessionDays := fs.Int("session-days", 0, "session length in days; must be one the site offers (default: the site's default)")
	output := fs.String("output", "table", "table or json")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(a.stdout, siteLoginHelp)
			return nil
		}
		return usageError("site login: %v", err)
	}
	if fs.NArg() != 1 {
		return usageError("site login requires exactly one SITE")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *bundleStdin && *cookieFile == "" && *output == "json" {
		return usageError("--output json needs --cookie-file, because --credential-bundle-stdin otherwise writes the session cookie to stdout")
	}
	adapter, ok := a.registry.Get(fs.Arg(0))
	if !ok {
		return usageError("unknown site %q", fs.Arg(0))
	}
	descriptor := adapter.Descriptor()
	if !descriptor.Supports(domain.CapabilityLogin) {
		return usageError("site %q does not declare capability %q", descriptor.ID, domain.CapabilityLogin)
	}
	loginer, ok := adapter.(site.SessionLoginer)
	if !ok {
		return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, domain.CapabilityLogin)
	}
	config := loginer.LoginConfig()
	days := *sessionDays
	if days == 0 {
		days = config.DefaultSessionDays
	}
	if !config.Allows(days) {
		return usageError("--session-days must be one of %s for site %q", joinDays(config.SessionDays), descriptor.ID)
	}
	var terminal promptTerminal
	if !*bundleStdin {
		if terminal, ok = a.promptTerminal(); !ok {
			return usageError("site login prompts on an interactive terminal; without one, pipe the credentials with --credential-bundle-stdin (see 'pt help login')")
		}
	}
	destination, defaultDestination := *cookieFile, false
	if destination == "" && !*bundleStdin {
		dir, err := os.UserConfigDir()
		if err != nil {
			return usageError("no user config directory for the session cookie; pass --cookie-file PATH")
		}
		destination, defaultDestination = filepath.Join(dir, "ptctl", "sessions", descriptor.ID+".cookie"), true
	}
	if destination != "" {
		var err error
		if destination, err = checkSessionCookiePath(destination, !defaultDestination); err != nil {
			return usageError("%v", err)
		}
	} else if err := rejectUnsafeCookieOutput(a.stdout); err != nil {
		return usageError("%v", err)
	}

	var credentials site.LoginCredentials
	var err error
	if *bundleStdin {
		credentials, err = readLoginCredentialBundle(a.stdin)
	} else {
		credentials, err = a.promptLoginCredentials(terminal, descriptor)
	}
	if err != nil {
		return err
	}
	var pending *pendingCookieFile
	if destination != "" {
		if pending, err = prepareSessionCookieFile(destination, defaultDestination); err != nil {
			return err
		}
		defer pending.discard()
	}

	ctx, cancel := context.WithTimeout(context.Background(), siteLoginTimeout)
	defer cancel()
	result, err := loginer.Login(ctx, credentials, days)
	if err != nil {
		return siteLoginPublicError(ctx, err)
	}
	if pending == nil {
		if _, err := io.WriteString(a.stdout, result.Session.SecretValue()+"\n"); err != nil {
			return fmt.Errorf("write site session cookie to stdout failed")
		}
	} else if err := pending.commit(result.Session.SecretValue()); err != nil {
		return err
	}
	if *output == "json" {
		report := siteLoginReport{SiteID: descriptor.ID, Username: result.Username, SessionDays: days, CookieFile: destination, RequestsAttempted: result.RequestsAttempted}
		if result.ExpiresAt != nil {
			expires := result.ExpiresAt.UTC()
			report.SessionExpiresAt = &expires
		}
		return writeJSON(a.stdout, report, nil)
	}
	expires := "unknown"
	if result.ExpiresAt != nil {
		expires = result.ExpiresAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(a.stderr, "site login succeeded: site=%s username=%s session_expires=%s requests=%d\n",
		terminalSafe(descriptor.ID), terminalSafe(result.Username), expires, result.RequestsAttempted)
	if pending != nil {
		fmt.Fprintf(a.stderr, "session cookie saved to %s (owner-only)\nuse it with: %s\n",
			terminalSafe(destination), terminalSafe(cookieUseHint(descriptor.ID, destination)))
	}
	return nil
}

// promptLoginCredentials asks for the username with echo, then for the
// password and the optional one-time code with echo off. Prompts go to stderr.
func (a *app) promptLoginCredentials(terminal promptTerminal, descriptor domain.SiteDescriptor) (site.LoginCredentials, error) {
	fmt.Fprintf(a.stderr, "Signing in to %s with one login request; failed logins count toward the site's lockout.\n", terminalSafe(descriptor.Name))
	fmt.Fprint(a.stderr, "Username: ")
	username, err := terminal.ReadLine()
	if err != nil {
		return site.LoginCredentials{}, loginPromptError(err)
	}
	password, err := terminal.ReadSecret(func() { fmt.Fprint(a.stderr, "Password (hidden): ") })
	fmt.Fprintln(a.stderr)
	if err != nil {
		return site.LoginCredentials{}, loginPromptError(err)
	}
	code, err := terminal.ReadSecret(func() { fmt.Fprint(a.stderr, "Two-step code (hidden; press Enter if none): ") })
	fmt.Fprintln(a.stderr)
	if err != nil {
		return site.LoginCredentials{}, loginPromptError(err)
	}
	accountPassword, err := site.NewAccountPassword(password)
	if err != nil {
		return site.LoginCredentials{}, fmt.Errorf("site login account password is invalid")
	}
	// Authenticator apps often show a code as "123 456".
	code = strings.Join(strings.Fields(code), "")
	credentials, err := site.NewLoginCredentials(strings.TrimSpace(username), accountPassword, code)
	if err != nil {
		return site.LoginCredentials{}, fmt.Errorf("site %v", err)
	}
	return credentials, nil
}

func loginPromptError(err error) error {
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("site login canceled before anything was sent")
	}
	return fmt.Errorf("read site login input from the terminal failed")
}

// checkSessionCookiePath resolves the cookie file path before anything is
// read or sent. It refuses an existing directory, symlink, or other
// non-regular file, and an explicit path whose directory does not exist.
func checkSessionCookiePath(path string, explicit bool) (string, error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("--cookie-file path is invalid")
	}
	info, err := os.Lstat(resolved)
	switch {
	case err == nil && !info.Mode().IsRegular():
		return "", fmt.Errorf("refusing to replace %s with a session cookie: it is not a regular file", resolved)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("inspect session cookie file %s failed", resolved)
	}
	if explicit {
		if parent, err := os.Stat(filepath.Dir(resolved)); err != nil || !parent.IsDir() {
			return "", fmt.Errorf("--cookie-file directory %s does not exist", filepath.Dir(resolved))
		}
	}
	return resolved, nil
}

// pendingCookieFile is an owner-only temporary file next to the cookie file.
// It is created before the login request, so an unwritable destination never
// costs a login attempt, and replaces the cookie file only after the login is
// verified.
type pendingCookieFile struct {
	temp *os.File
	path string
}

func prepareSessionCookieFile(path string, createDir bool) (*pendingCookieFile, error) {
	dir := filepath.Dir(path)
	if createDir {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create session cookie directory %s failed", dir)
		}
	}
	// CreateTemp opens the file with mode 0600.
	temp, err := os.CreateTemp(dir, ".ptctl-session-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create a session cookie file in %s failed", dir)
	}
	return &pendingCookieFile{temp: temp, path: path}, nil
}

func (pending *pendingCookieFile) commit(cookie string) error {
	name := pending.temp.Name()
	_, err := io.WriteString(pending.temp, cookie+"\n")
	if err == nil {
		err = pending.temp.Sync()
	}
	if closeErr := pending.temp.Close(); err == nil {
		err = closeErr
	}
	pending.temp = nil
	if err == nil {
		err = os.Rename(name, pending.path)
	}
	if err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("save the session cookie to %s failed; the new session was not kept", pending.path)
	}
	return nil
}

func (pending *pendingCookieFile) discard() {
	if pending.temp == nil {
		return
	}
	name := pending.temp.Name()
	_ = pending.temp.Close()
	_ = os.Remove(name)
	pending.temp = nil
}

// cookieUseHint is a follow-up command that reads the saved cookie, quoted
// for the platform's usual shell.
func cookieUseHint(siteID, path string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`Get-Content "%s" | ptctl site status --cookie-stdin %s`, path, siteID)
	}
	return fmt.Sprintf("ptctl site status --cookie-stdin %s < '%s'", siteID, strings.ReplaceAll(path, "'", `'\''`))
}

func joinDays(days []int) string {
	parts := make([]string, len(days))
	for i, day := range days {
		parts[i] = strconv.Itoa(day)
	}
	return strings.Join(parts, ", ")
}

// rejectUnsafeCookieOutput refuses to print a session cookie to a terminal or
// another device, or into a regular file that group or other users can read.
// Pipes are allowed. Windows file modes do not express ownership, so the file
// check only runs elsewhere.
func rejectUnsafeCookieOutput(writer io.Writer) error {
	file, ok := writer.(*os.File)
	if !ok {
		return nil
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect session cookie output failed")
	}
	if info.Mode()&(os.ModeDevice|os.ModeCharDevice) != 0 {
		return fmt.Errorf("refusing to write a session cookie to a terminal or device; redirect stdout to a private file or a pipe, or use --cookie-file")
	}
	if runtime.GOOS != "windows" && info.Mode().IsRegular() && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("refusing to write a session cookie to a file other users can access; create it with umask 077, or use --cookie-file")
	}
	return nil
}

// readLoginCredentialBundle reads the account username, password, and
// optional one-time code for one login, as one ptctl.credentials/v1 bundle on
// stdin.
func readLoginCredentialBundle(reader io.Reader) (site.LoginCredentials, error) {
	if err := rejectTTYSecret(reader); err != nil {
		return site.LoginCredentials{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxCredentialBundleBytes+1))
	if err != nil {
		return site.LoginCredentials{}, fmt.Errorf("read site login credential bundle from stdin failed")
	}
	if len(raw) == 0 || len(raw) > maxCredentialBundleBytes || !utf8.Valid(raw) {
		return site.LoginCredentials{}, fmt.Errorf("site login credential bundle is invalid")
	}
	values, err := decodeCredentialBundleOptional(raw, []string{"schema", "site_username", "site_password"}, []string{"site_totp"})
	if err != nil {
		return site.LoginCredentials{}, fmt.Errorf("site login credential bundle is invalid")
	}
	if values["schema"] != reconciliationCredentialSchema {
		return site.LoginCredentials{}, fmt.Errorf("site login credential bundle schema is unsupported")
	}
	password, err := site.NewAccountPassword(values["site_password"])
	if err != nil {
		return site.LoginCredentials{}, fmt.Errorf("site login account password is invalid")
	}
	code, hasCode := values["site_totp"]
	if hasCode {
		// Command substitution and password managers can leave surrounding
		// whitespace; an empty code is still invalid rather than "none".
		code = strings.TrimSpace(code)
		if code == "" {
			return site.LoginCredentials{}, fmt.Errorf("site login one-time code is invalid")
		}
	}
	credentials, err := site.NewLoginCredentials(values["site_username"], password, code)
	if err != nil {
		// NewLoginCredentials errors are fixed texts that name the field only.
		return site.LoginCredentials{}, fmt.Errorf("site %v", err)
	}
	return credentials, nil
}

func siteLoginPublicError(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("site login canceled: %w", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("site login timed out: %w", context.DeadlineExceeded)
	}
	var stopped *site.LoginError
	if errors.As(err, &stopped) && knownLoginStopReason(stopped.Reason) {
		if stopped.AttemptsRemaining != nil {
			return fmt.Errorf("site login stopped: %s (the site reports %d login attempts left before it blocks this IP)", stopped.Reason, *stopped.AttemptsRemaining)
		}
		return fmt.Errorf("site login stopped: %s", stopped.Reason)
	}
	for _, cause := range []error{site.ErrRateLimited, site.ErrAddressBlocked} {
		if errors.Is(err, cause) {
			return fmt.Errorf("site login failed: %v", cause)
		}
	}
	return fmt.Errorf("site login failed")
}

func knownLoginStopReason(reason string) bool {
	switch reason {
	case site.LoginCredentialsRejected, site.LoginTwoFactorRequired, site.LoginTwoFactorRejected, site.LoginResponseUnrecognized, site.LoginSessionUnverified, site.LoginRequestFailed:
		return true
	}
	return false
}
