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
  pt site login (--cookie-stdin | --api-key-stdin) [--output table|json] SITE

Signs in to SITE once, so the site commands work without a credential flag.
After it, for example:
  pt site status SITE
  pt site search SITE QUERY
Each site command uses the saved credential unless --cookie-stdin or
--api-key-stdin pipes one in; an explicit flag always wins. 'pt site logout
SITE' deletes the saved credential.

Sites that list the auth.login_effectful capability (TJUPT) sign in with the
account password. By default the command prompts on this terminal for the
username, the password, and the two-step code (press Enter if the account has
none). The password and code are not shown as you type. It sends one login
request, then one read that confirms the new session, and never retries. The
session cookie is saved, readable only by you, to
  <user config dir>/ptctl/sessions/SITE.cookie
and the exact path is printed. The password itself is never saved.

Sites without a password login (M-Team) prompt for their API key instead, with
echo off, check it with one session read, and save it, readable only by you,
to
  <user config dir>/ptctl/sessions/SITE.api-key
The same works for a session cookie you already have, with --cookie-stdin.
The saved file holds the credential in plain text; anyone who can read your
account's files can use it.

Failed logins count toward the site's lockout (TJUPT blocks the IP after ten),
so never run login in a loop.

Flags:
  --cookie-file PATH
      Password login only: save the cookie to PATH instead. PATH is replaced
      atomically and is readable only by you; its directory must already
      exist. Site commands do not read PATH by themselves; pipe it with
      --cookie-stdin.
  --session-days N
      Password login only: session length in days, one the site offers; an
      invalid value lists them. The default is the site's own default
      (TJUPT: 7).
  --output table|json
      json prints a site.login report on stdout with site_id, username,
      session_days, session_expires_at, cookie_file or api_key_file, and
      requests_attempted. It never contains the credential.
  --credential-bundle-stdin
      For scripts and agents: read the credentials from stdin as one strict
      JSON object instead of prompting:
        {"schema":"ptctl.credentials/v1","site_username":"...",
         "site_password":"...","site_totp":"123456"}
      Leave out site_totp when the account has no two-step verification. A
      terminal on stdin is refused. Without --cookie-file the cookie, and
      nothing else, is written to stdout, which must be a pipe or an
      owner-only file, and --output json is refused.
  --cookie-stdin, --api-key-stdin
      For scripts and agents: read the session cookie or API key to save from
      stdin instead of prompting. A terminal on stdin is refused.

Agent examples, with no secret in any argv:
  jq -n --arg user "$SITE_USER" --rawfile password "$PASSWORD_FILE" \
        --rawfile totp <(op item get "$OP_ITEM" --otp) \
     '{schema: "ptctl.credentials/v1", site_username: $user,
       site_password: ($password | rtrimstr("\n")),
       site_totp: ($totp | rtrimstr("\n"))}' |
    pt site login --credential-bundle-stdin --cookie-file "$COOKIE_FILE" --output json tjupt
  pt site login --api-key-stdin --output json mteam < "$API_KEY_FILE"

Exit status:
  0  Logged in. Text output reports the account, the expiry, and the saved
     file on stderr.
  1  "site login stopped: REASON", where REASON is one of
       login_credentials_rejected   the site refused the username or password,
                                    or the cookie or API key to save
       login_two_factor_required    the account needs a two-step code
       login_two_factor_rejected    the code was wrong or expired
       login_response_unrecognized  the site answered with an unknown page
       login_session_unverified     the new cookie did not show the same
                                    account, or the site's answer did not
                                    confirm the cookie or API key to save
       login_request_failed         the request failed; the site may still
                                    have counted the attempt
     The line may end with "(the site reports N login attempts left before it
     blocks this IP)". Other failures print "site login failed" with an
     optional ": CAUSE". Nothing is saved after a failure.
  2  Usage error. Nothing was read or sent.
`

// siteLoginReport is the --output json result of a verified login. It says
// where the session cookie went and never contains the cookie itself.
// A password login always has a session length and, in json mode, a cookie
// file, so omitempty never drops either from its report. Saving an existing
// credential has no session length and names an api_key_file instead of a
// cookie_file for an API key.
type siteLoginReport struct {
	SiteID            string     `json:"site_id"`
	Username          string     `json:"username"`
	SessionDays       int        `json:"session_days,omitempty"`
	SessionExpiresAt  *time.Time `json:"session_expires_at,omitempty"`
	CookieFile        string     `json:"cookie_file,omitempty"`
	APIKeyFile        string     `json:"api_key_file,omitempty"`
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
	cookieStdin := fs.Bool("cookie-stdin", false, "save a Cookie header value read from stdin, after one verifying read, instead of signing in with the password")
	apiKeyStdin := fs.Bool("api-key-stdin", false, "save a site-issued API key read from stdin, after one verifying read, instead of prompting for it")
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
	if countTrue(*bundleStdin, *cookieStdin, *apiKeyStdin) > 1 {
		return usageError("site login accepts only one of --credential-bundle-stdin, --cookie-stdin, and --api-key-stdin")
	}
	if *bundleStdin && *cookieFile == "" && *output == "json" {
		return usageError("--output json needs --cookie-file, because --credential-bundle-stdin otherwise writes the session cookie to stdout")
	}
	adapter, ok := a.registry.Get(fs.Arg(0))
	if !ok {
		return usageError("unknown site %q", fs.Arg(0))
	}
	descriptor := adapter.Descriptor()
	if *cookieStdin || *apiKeyStdin || (!*bundleStdin && !descriptor.Supports(domain.CapabilityLogin)) {
		if *cookieFile != "" || *sessionDays != 0 {
			return usageError("--cookie-file and --session-days apply only to a password login")
		}
		return a.siteSaveCredential(adapter, descriptor, *cookieStdin, *apiKeyStdin, *output)
	}
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
	if pending != nil && defaultDestination {
		fmt.Fprintf(a.stderr, "session cookie saved to %s (owner-only)\nsite commands now use it when no --cookie-stdin is given, for example: ptctl site status %s\n",
			terminalSafe(destination), terminalSafe(descriptor.ID))
	} else if pending != nil {
		fmt.Fprintf(a.stderr, "session cookie saved to %s (owner-only)\nuse it with: %s\n",
			terminalSafe(destination), terminalSafe(cookieUseHint(descriptor.ID, destination)))
	}
	return nil
}

// siteSaveCredential verifies a cookie or API key the user already has with
// one session read, then saves it where site commands find it. It prompts on
// the terminal with echo off unless --cookie-stdin or --api-key-stdin pipes
// the credential in.
func (a *app) siteSaveCredential(adapter site.Adapter, descriptor domain.SiteDescriptor, cookieStdin, apiKeyStdin bool, output string) error {
	source, err := siteCredentialSourceFor(descriptor, cookieStdin, apiKeyStdin)
	if err != nil {
		return err
	}
	label, flagName := savedCredentialLabel(source.method), credentialStdinFlag(source.method)
	if !descriptor.Supports(domain.CapabilityAuthCheck) {
		return usageError("site %q cannot verify a saved credential because it does not declare capability %q", descriptor.ID, domain.CapabilityAuthCheck)
	}
	checker, ok := adapter.(site.AuthChecker)
	if !ok {
		return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, domain.CapabilityAuthCheck)
	}
	prompt := !cookieStdin && !apiKeyStdin
	var terminal promptTerminal
	if prompt {
		if terminal, ok = a.promptTerminal(); !ok {
			return usageError("site login prompts on an interactive terminal; without one, pipe the %s with %s", label, flagName)
		}
	}
	destination, err := savedCredentialPath(descriptor.ID, source.method)
	if err == nil {
		destination, err = filepath.Abs(destination)
	}
	if err != nil {
		return usageError("no user config directory for the saved %s", label)
	}
	if info, err := os.Lstat(destination); err == nil && !info.Mode().IsRegular() {
		return usageError("refusing to replace saved %s %s because it is not a regular file", label, destination)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return usageError("inspect saved %s %s failed", label, destination)
	}

	var credential site.Credential
	if prompt {
		fmt.Fprintf(a.stderr, "Saving the %s for %s; one read checks it first.\n", label, terminalSafe(descriptor.Name))
		question := "Cookie header value (hidden): "
		if source.method == domain.AuthMethodAPIKeyHeader {
			question = "API key (hidden): "
		}
		value, readErr := terminal.ReadSecret(func() { fmt.Fprint(a.stderr, question) })
		fmt.Fprintln(a.stderr)
		if readErr != nil {
			return loginPromptError(readErr)
		}
		value = strings.TrimSpace(value)
		if source.method == domain.AuthMethodAPIKeyHeader {
			credential, err = canonicalAPIKeyCredential([]byte(value))
		} else {
			credential, err = canonicalCookieCredential([]byte(value))
		}
	} else if source.method == domain.AuthMethodAPIKeyHeader {
		credential, err = readEffectfulAPIKeyCredential(a.stdin)
	} else {
		credential, err = readEffectfulSiteCredential(a.stdin)
	}
	if err != nil {
		return err
	}
	pending, err := prepareSessionCookieFile(destination, true)
	if err != nil {
		return err
	}
	defer pending.discard()

	ctx, cancel := context.WithTimeout(context.Background(), siteLoginTimeout)
	defer cancel()
	status, err := checker.CheckSession(ctx, credential)
	if err != nil {
		for _, cause := range []error{site.ErrCredentialMalformed, site.ErrCredentialRejected} {
			if errors.Is(err, cause) {
				return fmt.Errorf("site login failed: %v", cause)
			}
		}
		return siteLoginPublicError(ctx, err)
	}
	switch status.State {
	case domain.AuthenticationAuthenticated:
	case domain.AuthenticationUnauthenticated:
		return fmt.Errorf("site login stopped: %s", site.LoginCredentialsRejected)
	default:
		return fmt.Errorf("site login stopped: %s", site.LoginSessionUnverified)
	}
	if err := pending.commit(credential.SecretValue()); err != nil {
		return err
	}
	if output == "json" {
		report := siteLoginReport{SiteID: descriptor.ID, Username: status.Username, RequestsAttempted: 1}
		if source.method == domain.AuthMethodAPIKeyHeader {
			report.APIKeyFile = destination
		} else {
			report.CookieFile = destination
		}
		return writeJSON(a.stdout, report, nil)
	}
	fmt.Fprintf(a.stderr, "site login succeeded: site=%s username=%s requests=1\n%s saved to %s (owner-only)\nsite commands now use it when no %s is given, for example: ptctl site status %s\n",
		terminalSafe(descriptor.ID), terminalSafe(status.Username), label, terminalSafe(destination), flagName, terminalSafe(descriptor.ID))
	return nil
}

func countTrue(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
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
		return "", fmt.Errorf("refusing to replace %s with a session cookie because it is not a regular file", resolved)
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

const siteLogoutHelp = `Usage:
  pt site logout [--output table|json] SITE

Deletes the session cookie or API key 'pt site login' saved for SITE, so site
commands stop using it. Nothing is sent to the site: a session cookie stays
valid on the site until it expires, and an API key stays valid until you
revoke it in the site's own settings. A file saved elsewhere with
--cookie-file is not touched.

Exit status:
  0  The saved credential was deleted, or there was none.
  1  A saved credential path is not a regular file, or it could not be deleted.
  2  Usage error. Nothing was deleted.
`

// siteLogoutReport is the --output json result of site logout.
type siteLogoutReport struct {
	SiteID       string   `json:"site_id"`
	RemovedFiles []string `json:"removed_files"`
}

// siteLogout deletes the credentials 'site login' saved for one site. It
// never contacts the site.
func (a *app) siteLogout(args []string) error {
	fs := newFlagSet("site logout")
	output := fs.String("output", "table", "table or json")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(a.stdout, siteLogoutHelp)
			return nil
		}
		return usageError("site logout: %v", err)
	}
	if fs.NArg() != 1 {
		return usageError("site logout requires exactly one SITE")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	adapter, ok := a.registry.Get(fs.Arg(0))
	if !ok {
		return usageError("unknown site %q", fs.Arg(0))
	}
	descriptor := adapter.Descriptor()
	report := siteLogoutReport{SiteID: descriptor.ID, RemovedFiles: []string{}}
	for _, method := range []domain.AuthMethod{domain.AuthMethodCookieHeader, domain.AuthMethodAPIKeyHeader} {
		path, err := savedCredentialPath(descriptor.ID, method)
		if err != nil {
			return usageError("no user config directory for saved credentials")
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect saved %s %s failed", savedCredentialLabel(method), path)
		}
		// A symlink is deleted itself, never the file it points to.
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("refusing to delete saved %s %s because it is not a regular file", savedCredentialLabel(method), path)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("delete saved %s %s failed", savedCredentialLabel(method), path)
		}
		report.RemovedFiles = append(report.RemovedFiles, path)
		if *output != "json" {
			fmt.Fprintf(a.stderr, "deleted saved %s %s\n", savedCredentialLabel(method), terminalSafe(path))
		}
	}
	if *output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	if len(report.RemovedFiles) == 0 {
		fmt.Fprintf(a.stderr, "no saved credential for site %s\n", terminalSafe(descriptor.ID))
		return nil
	}
	fmt.Fprintln(a.stderr, "nothing was sent to the site; a session cookie stays valid there until it expires, and an API key until you revoke it")
	return nil
}
