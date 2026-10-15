package tjupt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

// TJUPT's login.php posts username, password, and a session length ("logout",
// in days) to takelogin.php. An account with two-step verification gets a
// verifyform page back that re-posts the same fields plus verify_code, so one
// POST that already carries verify_code completes the login. A successful
// login sets a single access_token cookie that lasts the chosen number of days.
const (
	loginPath                   = "takelogin.php"
	loginSessionCookie          = "access_token"
	loginAccept                 = "text/html"
	loginDefaultSessionDays     = 7
	loginMaxFormFields          = 4
	loginMaxFormBytes           = 4 << 10
	loginMaxResponseBytes       = 1 << 20
	loginMaxResponseHeaderBytes = 32 << 10
	// loginVerifyDelay spaces the verification read after the login POST.
	loginVerifyDelay = 2 * time.Second
)

var loginSessionDays = []int{1, 7, 14, 30, 90}

var (
	twoFactorForm     = regexp.MustCompile(`(?is)<form\b[^>]*name\s*=\s*["']?verifyform["']?`)
	twoFactorField    = regexp.MustCompile(`(?is)<input\b[^>]*name\s*=\s*["']?verify_code["']?`)
	loginFailedMarker = regexp.MustCompile(`(?i)(登录失败|登錄失敗|login failed)`)
	attemptsRemaining = regexp.MustCompile(`你还有\s*\[?\s*([0-9]{1,3})\s*\]?\s*次尝试机会`)
)

type guardedLoginClient interface {
	guardedClient
	PostLoginFormOnce(context.Context, string, []httpguard.FormField, string, int, int64, int64, int64, httpguard.RedirectClassifier, []string) (httpguard.StrictResponse, []httpguard.SessionCookie, error)
}

// The production client must keep the login port, or Login fails closed.
var _ guardedLoginClient = loginRedirectClient{}

func (a *Adapter) LoginConfig() site.LoginConfig {
	return site.LoginConfig{SessionDays: append([]int(nil), loginSessionDays...), DefaultSessionDays: loginDefaultSessionDays}
}

// Login sends exactly one login POST and, only when TJUPT issues a session
// cookie, one authenticated read that must name the same account. It never
// retries: TJUPT blocks the client IP after ten failed attempts. The login
// response body is discarded after classification because TJUPT's two-step
// page echoes the password back in a hidden field.
func (a *Adapter) Login(ctx context.Context, credentials site.LoginCredentials, sessionDays int) (site.LoginResult, error) {
	var result site.LoginResult
	if !a.productionOrigin() {
		return result, fmt.Errorf("TJUPT login requires the default origin")
	}
	if !a.LoginConfig().Allows(sessionDays) {
		return result, fmt.Errorf("TJUPT login session length is not offered")
	}
	username, password := credentials.Username(), credentials.Password().SecretValue()
	if username == "" || password == "" {
		return result, fmt.Errorf("TJUPT login credentials are incomplete")
	}
	code, hasCode := credentials.OneTimeCode()
	if hasCode && (len(code) != 6 || strings.Trim(code, "0123456789") != "") {
		return result, fmt.Errorf("TJUPT two-step code must be 6 digits")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	rawClient, err := a.clientFactory()(a.baseURL, "", 2*time.Second)
	if err != nil {
		return result, err
	}
	client, ok := rawClient.(guardedLoginClient)
	if !ok {
		_ = rawClient.Close()
		return result, fmt.Errorf("open TJUPT login client failed")
	}
	fields := []httpguard.FormField{
		{Name: "username", Value: username},
		{Name: "password", Value: password},
		{Name: "logout", Value: strconv.Itoa(sessionDays)},
	}
	if hasCode {
		fields = append(fields, httpguard.FormField{Name: "verify_code", Value: code})
	}
	// The redirect after a successful login is neither followed nor needed:
	// the cookie and the verification read decide the outcome.
	response, cookies, postErr := client.PostLoginFormOnce(ctx, loginPath, fields, loginAccept, loginMaxFormFields, loginMaxFormBytes, loginMaxResponseBytes, loginMaxResponseHeaderBytes, nil, []string{loginSessionCookie})
	result.RequestsAttempted = client.RequestsMade()
	_ = client.Close()
	outcome := classifyLoginResponse(response, len(cookies) == 1)
	clear(response.Body)
	if postErr != nil {
		switch {
		case errors.Is(postErr, context.Canceled) || errors.Is(postErr, context.DeadlineExceeded):
			return result, postErr
		case errors.Is(postErr, httpguard.ErrBlockedAddress):
			return result, fmt.Errorf("TJUPT login request failed: %w", site.ErrAddressBlocked)
		}
		return result, &site.LoginError{Reason: site.LoginRequestFailed}
	}
	switch outcome.reason {
	case "":
	case site.LoginTwoFactorRequired:
		if hasCode {
			outcome.reason = site.LoginTwoFactorRejected
		}
		return result, &site.LoginError{Reason: outcome.reason, AttemptsRemaining: outcome.attemptsRemaining}
	default:
		return result, &site.LoginError{Reason: outcome.reason, AttemptsRemaining: outcome.attemptsRemaining}
	}

	cookie := cookies[0]
	session, err := site.NewCookieCredential(loginSessionCookie + "=" + cookie.Value())
	if err != nil {
		return result, &site.LoginError{Reason: site.LoginResponseUnrecognized}
	}
	if err := a.pause(ctx, loginVerifyDelay); err != nil {
		return result, err
	}
	result.RequestsAttempted++
	status, err := a.CheckSession(ctx, session)
	if err != nil || status.State != domain.AuthenticationAuthenticated || !strings.EqualFold(status.Username, username) {
		return result, &site.LoginError{Reason: site.LoginSessionUnverified}
	}
	result.Session = session
	result.Username = status.Username
	if !cookie.Expires.IsZero() {
		expires := cookie.Expires.UTC()
		result.ExpiresAt = &expires
	}
	return result, nil
}

type loginOutcome struct {
	// reason is empty when TJUPT issued a session cookie worth verifying.
	reason            string
	attemptsRemaining *int
}

// classifyLoginResponse recognizes the two-step page and the failure page
// before trusting an issued cookie, so a page that both sets a cookie and
// refuses the login is never treated as a login.
func classifyLoginResponse(response httpguard.StrictResponse, cookieIssued bool) loginOutcome {
	body := response.Body
	outcome := loginOutcome{attemptsRemaining: parseAttemptsRemaining(body)}
	switch {
	case response.MediaType == "text/html" && twoFactorForm.Match(body) && twoFactorField.Match(body):
		outcome.reason = site.LoginTwoFactorRequired
	case response.MediaType == "text/html" && response.StatusCode == 200 && !cookieIssued && loginFailedMarker.Match(body):
		outcome.reason = site.LoginCredentialsRejected
	case cookieIssued && response.StatusCode >= 200 && response.StatusCode <= 399:
		outcome.attemptsRemaining = nil
	default:
		outcome.reason = site.LoginResponseUnrecognized
		outcome.attemptsRemaining = nil
	}
	return outcome
}

func parseAttemptsRemaining(body []byte) *int {
	if len(body) == 0 || !bytes.Contains(body, []byte("次尝试机会")) {
		return nil
	}
	text := spacePattern.ReplaceAllString(tagPattern.ReplaceAllString(string(body), " "), " ")
	match := attemptsRemaining.FindStringSubmatch(text)
	if match == nil {
		return nil
	}
	remaining, err := strconv.Atoi(match[1])
	if err != nil {
		return nil
	}
	return &remaining
}

func (a *Adapter) pause(ctx context.Context, delay time.Duration) error {
	if a.sleep != nil {
		return a.sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
