package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/bonusexchange"
	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

// A site that signs in with an API key, as M-Team does, reviews and
// exchanges bonus offers with that key alone.

func apiKeyBonusReview(t *testing.T) *fakeBonusReviewAdapter {
	adapter := successfulFakeBonusReview(t)
	adapter.authMethod = domain.AuthMethodAPIKeyHeader
	return adapter
}

func apiKeyBonusExchange(t *testing.T) *fakeBonusExchangeAdapter {
	adapter := successfulFakeBonusExchange(t)
	adapter.authMethod = domain.AuthMethodAPIKeyHeader
	return adapter
}

func TestSiteBonusReviewReadsAPipedOrSavedAPIKey(t *testing.T) {
	useTempConfigDir(t)
	adapter := apiKeyBonusReview(t)
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader(apiKeyCanary + "\n"), stdout: &out, registry: site.NewRegistry(adapter)}
	if err := a.siteBonusReview([]string{"--api-key-stdin", "--output", "json", "fakept", "1"}); err != nil ||
		adapter.opened != 1 || adapter.credential != apiKeyCanary || adapter.credentialMethod != domain.AuthMethodAPIKeyHeader ||
		strings.Contains(out.String(), "CANARY") {
		t.Fatalf("err=%v opened=%d method=%q stdout=%q", err, adapter.opened, adapter.credentialMethod, out.String())
	}

	writeSavedCredential(t, "fakept", domain.AuthMethodAPIKeyHeader, savedAPIKeyCanary+"\n")
	adapter = apiKeyBonusReview(t)
	stdin := &trackingReader{}
	out.Reset()
	a = &app{stdin: stdin, stdout: &out, registry: site.NewRegistry(adapter)}
	if err := a.siteBonusReview([]string{"--output", "json", "fakept", "1"}); err != nil || stdin.read ||
		adapter.credential != savedAPIKeyCanary || adapter.credentialMethod != domain.AuthMethodAPIKeyHeader || strings.Contains(out.String(), "CANARY") {
		t.Fatalf("err=%v read=%t method=%q stdout=%q", err, stdin.read, adapter.credentialMethod, out.String())
	}
}

func TestSiteBonusReviewCredentialFlagMisuseNeverReadsStdin(t *testing.T) {
	useTempConfigDir(t)
	for _, test := range []struct {
		name    string
		adapter *fakeBonusReviewAdapter
		args    []string
		want    string
	}{
		{name: "both flags", adapter: apiKeyBonusReview(t), args: []string{"--cookie-stdin", "--api-key-stdin", "fakept", "1"},
			want: "--cookie-stdin and --api-key-stdin are mutually exclusive"},
		{name: "cookie for an API key site", adapter: apiKeyBonusReview(t), args: []string{"--cookie-stdin", "fakept", "1"},
			want: `site "fakept" does not support cookie_header authentication`},
		{name: "API key for a cookie site", adapter: successfulFakeBonusReview(t), args: []string{"--api-key-stdin", "fakept", "1"},
			want: `site "fakept" does not support api_key_header authentication`},
		{name: "nothing saved", adapter: apiKeyBonusReview(t), args: []string{"fakept", "1"},
			want: `no saved API key for site "fakept"; run 'ptctl site login fakept' first, or pipe one with --api-key-stdin`},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &trackingReader{}
			var out bytes.Buffer
			a := &app{stdin: reader, stdout: &out, registry: site.NewRegistry(test.adapter)}
			var usage *usageErr
			if err := a.siteBonusReview(test.args); !errors.As(err, &usage) || err.Error() != test.want || reader.read || out.Len() != 0 || test.adapter.opened != 0 {
				t.Fatalf("err=%v read=%t opened=%d stdout=%q", err, reader.read, test.adapter.opened, out.String())
			}
		})
	}
}

func prepareAPIKeyBonusExchange(t *testing.T, adapter site.Adapter, reviewID string) (string, string) {
	t.Helper()
	stateRoot := initializedBonusExchangeStore(t)
	prepared := runBonusExchangeCLI(t, &app{stdin: &trackingReader{}, registry: site.NewRegistry(adapter)}, []string{
		"prepare", "--state-store", stateRoot, "--expect-review-id", reviewID, "--output", "json", "fakept", "1",
	}, false)
	return stateRoot, prepared.Operation.IntentRecord.ID.String()
}

func TestSiteBonusExchangeSubmitUsesAPipedOrSavedAPIKey(t *testing.T) {
	useTempConfigDir(t)
	adapter := apiKeyBonusExchange(t)
	stateRoot, intent := prepareAPIKeyBonusExchange(t, adapter, adapter.review.ReviewID)
	submitted := runBonusExchangeCLI(t, &app{stdin: strings.NewReader(apiKeyCanary + "\n"), registry: site.NewRegistry(adapter)}, []string{
		"submit", "--state-store", stateRoot, "--intent-record", intent, "--expect-review-id", adapter.review.ReviewID,
		"--api-key-stdin", "--acknowledge-bonus-exchange", "--output", "json", "fakept", "1",
	}, false)
	if submitted.Outcome != bonusexchange.StateConfirmed || adapter.opened != 1 || adapter.submissions != 1 ||
		adapter.credential != apiKeyCanary || adapter.credentialMethod != domain.AuthMethodAPIKeyHeader {
		t.Fatalf("submitted=%#v adapter=%#v", submitted, adapter)
	}
	assertDirectoryExcludesText(t, stateRoot, apiKeyCanary)

	writeSavedCredential(t, "fakept", domain.AuthMethodAPIKeyHeader, savedAPIKeyCanary+"\n")
	adapter = apiKeyBonusExchange(t)
	stateRoot, intent = prepareAPIKeyBonusExchange(t, adapter, adapter.review.ReviewID)
	stdin := &trackingReader{}
	submitted = runBonusExchangeCLI(t, &app{stdin: stdin, registry: site.NewRegistry(adapter)}, []string{
		"submit", "--state-store", stateRoot, "--intent-record", intent, "--expect-review-id", adapter.review.ReviewID,
		"--acknowledge-bonus-exchange", "--output", "json", "fakept", "1",
	}, false)
	if stdin.read || submitted.Outcome != bonusexchange.StateConfirmed || adapter.submissions != 1 ||
		adapter.credential != savedAPIKeyCanary || adapter.credentialMethod != domain.AuthMethodAPIKeyHeader {
		t.Fatalf("read=%t submitted=%#v adapter=%#v", stdin.read, submitted, adapter)
	}
	assertDirectoryExcludesText(t, stateRoot, savedAPIKeyCanary)
}

func TestSiteBonusExchangeAPIKeyRejectionsArePublicStopReasons(t *testing.T) {
	useTempConfigDir(t)
	for _, stop := range []string{"server_refused", "server_rate_limited", "server_authentication_rejected"} {
		adapter := apiKeyBonusExchange(t)
		adapter.submissionOutcome = site.BonusExchangeOutcomeRejected
		adapter.submissionStop = stop
		stateRoot, intent := prepareAPIKeyBonusExchange(t, adapter, adapter.review.ReviewID)
		var out bytes.Buffer
		a := &app{stdin: strings.NewReader(apiKeyCanary), stdout: &out, registry: site.NewRegistry(adapter)}
		err := a.siteBonusExchange([]string{
			"submit", "--state-store", stateRoot, "--intent-record", intent, "--expect-review-id", adapter.review.ReviewID,
			"--api-key-stdin", "--acknowledge-bonus-exchange", "--output", "json", "fakept", "1",
		})
		var envelope struct {
			Data siteBonusExchangeReport `json:"data"`
		}
		if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if err == nil || strings.Contains(out.String(), "CANARY") || envelope.Data.Outcome != bonusexchange.StateRejected ||
			envelope.Data.Request.Submission.StopReason != stop || adapter.submissions != 1 {
			t.Fatalf("stop=%s err=%v report=%#v", stop, err, envelope.Data)
		}
	}
}

func TestSiteBonusExchangeSubmitCredentialFlagMisuseNeverReadsStdin(t *testing.T) {
	useTempConfigDir(t)
	apiKeySite := apiKeyBonusExchange(t)
	passwordSite := &fakePasswordBonusExchangeAdapter{fakeBonusExchangeAdapter: successfulFakeBonusExchange(t)}
	for _, test := range []struct {
		name    string
		adapter site.Adapter
		flags   []string
		want    string
	}{
		{name: "API key and cookie", adapter: apiKeySite, flags: []string{"--api-key-stdin", "--cookie-stdin"},
			want: "site bonus exchange submit accepts one of --cookie-stdin, --api-key-stdin, or --credential-bundle-stdin"},
		{name: "API key and bundle", adapter: apiKeySite, flags: []string{"--api-key-stdin", "--credential-bundle-stdin"},
			want: "site bonus exchange submit accepts one of --cookie-stdin, --api-key-stdin, or --credential-bundle-stdin"},
		{name: "bundle for an API key site", adapter: apiKeySite, flags: []string{"--credential-bundle-stdin"},
			want: `site "fakept" bonus exchange takes only --api-key-stdin`},
		{name: "cookie for an API key site", adapter: apiKeySite, flags: []string{"--cookie-stdin"},
			want: `site "fakept" does not support cookie_header authentication`},
		{name: "API key for a password site", adapter: passwordSite, flags: []string{"--api-key-stdin"},
			want: `site "fakept" confirms bonus exchange with the account password; submit requires --credential-bundle-stdin`},
		{name: "nothing saved", adapter: apiKeySite, flags: nil,
			want: `no saved API key for site "fakept"; run 'ptctl site login fakept' first, or pipe one with --api-key-stdin`},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &trackingReader{}
			var out bytes.Buffer
			a := &app{stdin: reader, stdout: &out, registry: site.NewRegistry(test.adapter)}
			args := append([]string{
				"submit", "--state-store", "/nonexistent-ptctl-store", "--intent-record", "sha256:" + strings.Repeat("a", 64),
				"--expect-review-id", apiKeySite.review.ReviewID, "--acknowledge-bonus-exchange",
			}, append(test.flags, "fakept", "1")...)
			var usage *usageErr
			if err := a.siteBonusExchange(args); !errors.As(err, &usage) || err.Error() != test.want || reader.read || out.Len() != 0 {
				t.Fatalf("err=%v read=%t stdout=%q", err, reader.read, out.String())
			}
		})
	}
	if apiKeySite.opened != 0 || passwordSite.opened != 0 {
		t.Fatalf("a session opened: %d %d", apiKeySite.opened, passwordSite.opened)
	}
}
