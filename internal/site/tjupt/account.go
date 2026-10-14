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

const accountPath = bonusPagePath

var _ site.AccountReader = (*Adapter)(nil)

// Account returns the account fields that the authenticated bonus page
// proves: the username and balance from the page itself, and the class,
// uploaded total, active torrents, seeding points, and invites from the user
// panel. The panel's fields are optional and stay absent when it does not
// state them unambiguously. TJUPT's panel shows no downloaded total or ratio,
// so those fields are never set.
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
		if errors.Is(err, errLoginRedirect) {
			return domain.AccountSnapshot{}, fmt.Errorf("TJUPT session is not authenticated")
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
		panel := parseInfoBlock(body)
		return domain.AccountSnapshot{
			SiteID:        SiteID,
			Username:      username,
			UploadedBytes: panel.uploaded,
			Bonus:         bonus,
			Seeding:       panel.seeding,
			Leeching:      panel.leeching,
			UserClass:     panel.class,
			SeedingPoints: panel.seedingPoints,
			Invites:       panel.invites,
			ObservedAt:    time.Now().UTC(),
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
