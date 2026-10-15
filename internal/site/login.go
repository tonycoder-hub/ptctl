package site

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Login stop reasons are public, secret-free codes. An adapter reports one of
// them through a *LoginError when the site answered with a page it recognizes,
// or when the login request itself did not complete; every other failure
// keeps the generic login failure line.
const (
	LoginCredentialsRejected  = "login_credentials_rejected"
	LoginTwoFactorRequired    = "login_two_factor_required"
	LoginTwoFactorRejected    = "login_two_factor_rejected"
	LoginResponseUnrecognized = "login_response_unrecognized"
	LoginSessionUnverified    = "login_session_unverified"
	// LoginRequestFailed means the login request failed in transport or its
	// response was rejected before classification. The site may still have
	// counted the attempt.
	LoginRequestFailed = "login_request_failed"
)

// LoginError reports a recognized login refusal by its public stop reason.
// AttemptsRemaining is the site's own count of failed logins left before it
// blocks the client, when the response stated one.
type LoginError struct {
	Reason            string
	AttemptsRemaining *int
}

func (err *LoginError) Error() string { return "site login stopped: " + err.Reason }

// LoginCredentials are the account username, password, and optional one-time
// second-factor code for one login. They are held only in process memory and
// format as redacted.
type LoginCredentials struct {
	username    string
	password    AccountPassword
	oneTimeCode *string
}

// NewLoginCredentials validates the shape of one login. An empty oneTimeCode
// means none was supplied; a supplied code is 4 to 10 ASCII digits.
func NewLoginCredentials(username string, password AccountPassword, oneTimeCode string) (LoginCredentials, error) {
	if username == "" || username != strings.TrimSpace(username) || len(username) > 256 || !utf8.ValidString(username) {
		return LoginCredentials{}, fmt.Errorf("login username is invalid")
	}
	for _, r := range username {
		if r < 0x20 || r == 0x7f {
			return LoginCredentials{}, fmt.Errorf("login username is invalid")
		}
	}
	if password.SecretValue() == "" {
		return LoginCredentials{}, fmt.Errorf("login password is invalid")
	}
	credentials := LoginCredentials{username: username, password: password}
	if oneTimeCode != "" {
		if len(oneTimeCode) < 4 || len(oneTimeCode) > 10 || strings.Trim(oneTimeCode, "0123456789") != "" {
			return LoginCredentials{}, fmt.Errorf("login one-time code is invalid")
		}
		credentials.oneTimeCode = &oneTimeCode
	}
	return credentials, nil
}

func (credentials LoginCredentials) Username() string          { return credentials.username }
func (credentials LoginCredentials) Password() AccountPassword { return credentials.password }

// OneTimeCode returns the second-factor code and whether one was supplied.
func (credentials LoginCredentials) OneTimeCode() (string, bool) {
	if credentials.oneTimeCode == nil {
		return "", false
	}
	return *credentials.oneTimeCode, true
}

func (credentials LoginCredentials) String() string { return "[REDACTED]" }
func (credentials LoginCredentials) GoString() string {
	return "site.LoginCredentials{[REDACTED]}"
}

// LoginConfig lists the session lengths, in days, a site's login form offers.
// The CLI validates --session-days against it before reading stdin.
type LoginConfig struct {
	SessionDays        []int
	DefaultSessionDays int
}

// Allows reports whether days is one of the offered session lengths.
func (config LoginConfig) Allows(days int) bool {
	for _, offered := range config.SessionDays {
		if offered == days {
			return true
		}
	}
	return false
}

// LoginResult is a verified login. Session is the new cookie credential and is
// never serialized; the remaining fields are safe to report.
type LoginResult struct {
	Session           Credential
	Username          string
	ExpiresAt         *time.Time
	RequestsAttempted int
}

// SessionLoginer signs in with an account password and returns a new session
// credential that the site has confirmed with an authenticated read. It sends
// a fixed, small number of requests and never retries, because failed logins
// count toward the site's lockout.
type SessionLoginer interface {
	LoginConfig() LoginConfig
	Login(context.Context, LoginCredentials, int) (LoginResult, error)
}
