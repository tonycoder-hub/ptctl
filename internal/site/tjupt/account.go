package tjupt

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const accountPath = "mybonusapps.php"

var _ site.AccountReader = (*Adapter)(nil)

// Account returns the bounded public account fields that the authenticated
// bonus page itself proves. TJUPT does not expose uploaded/downloaded totals,
// ratio, or peer counts through this adapter, so those optional fields remain
// absent rather than being inferred from unrelated markup.
func (a *Adapter) Account(ctx context.Context, credential site.Credential) (domain.AccountSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.AccountSnapshot{}, err
	}
	body, finalURL, err := a.get(ctx, credential, accountPath, nil)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return domain.AccountSnapshot{}, fmt.Errorf("TJUPT account read canceled: %w", context.Canceled)
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return domain.AccountSnapshot{}, fmt.Errorf("TJUPT account read timed out: %w", context.DeadlineExceeded)
		}
		return domain.AccountSnapshot{}, fmt.Errorf("TJUPT account read failed")
	}
	if !matchesAccountURL(a.baseURL, finalURL) {
		return domain.AccountSnapshot{}, fmt.Errorf("TJUPT returned an unrecognized account page; parser is fail-closed")
	}
	state, username := classifyBonusPage(finalURL, body)
	switch state {
	case domain.AuthenticationUnauthenticated:
		return domain.AccountSnapshot{}, fmt.Errorf("TJUPT session is not authenticated")
	case domain.AuthenticationAuthenticated:
		bonus := parseBonusBalance(body)
		if !validAccountText(username, 256) || bonus == "" {
			return domain.AccountSnapshot{}, fmt.Errorf("TJUPT account page fields were invalid")
		}
		return domain.AccountSnapshot{
			SiteID:     "tjupt",
			Username:   username,
			Bonus:      bonus,
			ObservedAt: time.Now().UTC(),
		}, nil
	default:
		return domain.AccountSnapshot{}, fmt.Errorf("TJUPT returned an unrecognized account page; parser is fail-closed")
	}
}

func matchesAccountURL(baseURL string, observed *url.URL) bool {
	base, err := url.Parse(baseURL)
	if err != nil || observed == nil {
		return false
	}
	reference, err := url.Parse(accountPath)
	if err != nil {
		return false
	}
	expected := base.ResolveReference(reference)
	return strings.EqualFold(expected.Scheme, observed.Scheme) && strings.EqualFold(expected.Host, observed.Host) &&
		expected.Path == observed.Path && observed.RawPath == "" && observed.RawQuery == "" && !observed.ForceQuery &&
		observed.Fragment == "" && observed.User == nil
}
