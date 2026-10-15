package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const siteLoginTimeout = 45 * time.Second

// siteLogin signs in with an account password from stdin and writes the new
// session cookie, and nothing else, to stdout, so it can be redirected into a
// private file that later --cookie-stdin commands read. Everything a person
// needs to see goes to stderr.
func (a *app) siteLogin(args []string) error {
	fs := newFlagSet("site login")
	bundleStdin := fs.Bool("credential-bundle-stdin", false, "read a ptctl.credentials/v1 bundle with site_username, site_password, and optional site_totp from stdin")
	sessionDays := fs.Int("session-days", 0, "session length in days; must be one the site offers (default: the site's default)")
	if err := fs.Parse(args); err != nil {
		return usageError("site login: %v", err)
	}
	if fs.NArg() != 1 {
		return usageError("site login requires exactly one SITE")
	}
	if !*bundleStdin {
		return usageError("--credential-bundle-stdin is required; credentials are never accepted in argv")
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
	if err := rejectUnsafeCookieOutput(a.stdout); err != nil {
		return usageError("%v", err)
	}
	credentials, err := readLoginCredentialBundle(a.stdin)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), siteLoginTimeout)
	defer cancel()
	result, err := loginer.Login(ctx, credentials, days)
	if err != nil {
		return siteLoginPublicError(ctx, err)
	}
	if _, err := io.WriteString(a.stdout, result.Session.SecretValue()+"\n"); err != nil {
		return fmt.Errorf("write site session cookie to stdout failed")
	}
	expires := "unknown"
	if result.ExpiresAt != nil {
		expires = result.ExpiresAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(a.stderr, "site login succeeded: site=%s username=%s session_expires=%s requests=%d\n",
		terminalSafe(descriptor.ID), terminalSafe(result.Username), expires, result.RequestsAttempted)
	return nil
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
		return fmt.Errorf("refusing to write a session cookie to a terminal or device; redirect stdout to a private file or a pipe")
	}
	if runtime.GOOS != "windows" && info.Mode().IsRegular() && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("refusing to write a session cookie to a file other users can access; create it with umask 077")
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
