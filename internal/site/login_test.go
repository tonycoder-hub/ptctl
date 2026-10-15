package site_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/site"
)

func TestNewLoginCredentialsValidatesShapeAndRedacts(t *testing.T) {
	password, err := site.NewAccountPassword("PASSWORD-CANARY")
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := site.NewLoginCredentials("CanaryUser", password, "987654")
	if err != nil {
		t.Fatal(err)
	}
	code, ok := credentials.OneTimeCode()
	if credentials.Username() != "CanaryUser" || credentials.Password().SecretValue() != "PASSWORD-CANARY" || !ok || code != "987654" {
		t.Fatal("login credential accessors drifted")
	}
	wrapped := struct{ Credentials site.LoginCredentials }{credentials}
	for _, rendered := range []string{
		fmt.Sprintf("%v %+v %#v %s", credentials, credentials, credentials, credentials),
		fmt.Sprintf("%v %+v %#v", wrapped, wrapped, wrapped),
	} {
		if strings.Contains(rendered, "PASSWORD-CANARY") || strings.Contains(rendered, "987654") {
			t.Fatalf("login credentials leaked: %s", rendered)
		}
	}

	without, err := site.NewLoginCredentials("CanaryUser", password, "")
	if err != nil {
		t.Fatal(err)
	}
	if code, ok := without.OneTimeCode(); ok || code != "" {
		t.Fatalf("absent code reported as %q", code)
	}

	for _, test := range []struct {
		username string
		password site.AccountPassword
		code     string
	}{
		{username: "", password: password},
		{username: " CanaryUser", password: password},
		{username: "Canary\tUser", password: password},
		{username: strings.Repeat("u", 257), password: password},
		{username: "\xff", password: password},
		{username: "CanaryUser"},
		{username: "CanaryUser", password: password, code: "123"},
		{username: "CanaryUser", password: password, code: "12345678901"},
		{username: "CanaryUser", password: password, code: "12 456"},
		{username: "CanaryUser", password: password, code: "１２３４５６"},
	} {
		if _, err := site.NewLoginCredentials(test.username, test.password, test.code); err == nil {
			t.Fatalf("username=%q code=%q accepted", test.username, test.code)
		} else if strings.Contains(err.Error(), test.code) && test.code != "" {
			t.Fatalf("error echoed the code: %v", err)
		}
	}
}

func TestLoginConfigAllowsOnlyOfferedDays(t *testing.T) {
	config := site.LoginConfig{SessionDays: []int{1, 7}, DefaultSessionDays: 7}
	if !config.Allows(1) || !config.Allows(7) || config.Allows(0) || config.Allows(14) || (site.LoginConfig{}).Allows(0) {
		t.Fatal("LoginConfig.Allows drifted")
	}
	if got := (&site.LoginError{Reason: site.LoginTwoFactorRequired}).Error(); got != "site login stopped: login_two_factor_required" {
		t.Fatalf("LoginError text=%q", got)
	}
}
