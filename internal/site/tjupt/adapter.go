package tjupt

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

const DefaultBaseURL = "https://www.tjupt.org/"

// Adapter implements the conservative, read-only subset of TJUPT. It never
// submits a form and does not retry requests automatically.
type Adapter struct {
	baseURL string
}

func New(baseURL string) *Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Adapter{baseURL: baseURL}
}

func (a *Adapter) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID:      "tjupt",
		Name:    "TJUPT / 北洋园PT",
		BaseURL: a.baseURL,
		Capabilities: []domain.Capability{
			domain.CapabilityAuthCheck,
			domain.CapabilityAccountRead,
			domain.CapabilitySearch,
			domain.CapabilityBonusRead,
		},
	}
}

func (a *Adapter) CheckSession(ctx context.Context, credential site.Credential) (domain.SessionStatus, error) {
	body, finalURL, err := a.get(ctx, credential, "mybonusapps.php", nil)
	if err != nil {
		return domain.SessionStatus{}, err
	}
	status := domain.SessionStatus{ObservedAt: time.Now().UTC()}
	status.Authenticated = !isLoginPage(finalURL, body)
	if status.Authenticated {
		status.Username = parseUsername(body)
	}
	return status, nil
}

func (a *Adapter) Account(ctx context.Context, credential site.Credential) (domain.AccountSnapshot, error) {
	body, finalURL, err := a.get(ctx, credential, "mybonusapps.php", nil)
	if err != nil {
		return domain.AccountSnapshot{}, err
	}
	if isLoginPage(finalURL, body) {
		return domain.AccountSnapshot{}, fmt.Errorf("TJUPT session is not authenticated")
	}
	return domain.AccountSnapshot{
		SiteID:     "tjupt",
		Username:   parseUsername(body),
		Bonus:      parseBonusBalance(body),
		ObservedAt: time.Now().UTC(),
	}, nil
}

func (a *Adapter) Search(ctx context.Context, credential site.Credential, query string) ([]domain.TorrentSummary, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("search query is empty")
	}
	body, finalURL, err := a.get(ctx, credential, "torrents.php", url.Values{"search": {query}})
	if err != nil {
		return nil, err
	}
	if isLoginPage(finalURL, body) {
		return nil, fmt.Errorf("TJUPT session is not authenticated")
	}
	return parseSearch(body), nil
}

func (a *Adapter) BonusCatalog(ctx context.Context, credential site.Credential) (domain.BonusCatalog, error) {
	body, finalURL, err := a.get(ctx, credential, "mybonusapps.php", nil)
	if err != nil {
		return domain.BonusCatalog{}, err
	}
	if isLoginPage(finalURL, body) {
		return domain.BonusCatalog{}, fmt.Errorf("TJUPT session is not authenticated")
	}
	return domain.BonusCatalog{
		SiteID:     "tjupt",
		Balance:    parseBonusBalance(body),
		Rows:       parseBonusRows(body),
		ObservedAt: time.Now().UTC(),
	}, nil
}

func (a *Adapter) get(ctx context.Context, credential site.Credential, path string, query url.Values) ([]byte, *url.URL, error) {
	client, err := httpguard.New(a.baseURL, credential.CookieValue(), 2*time.Second)
	if err != nil {
		return nil, nil, err
	}
	return client.Get(ctx, path, query)
}
