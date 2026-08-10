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
	baseURL   string
	newClient guardedClientFactory
}

type guardedClient interface {
	Get(context.Context, string, url.Values) ([]byte, *url.URL, error)
	GetOnce(context.Context, string, url.Values, string, int64, int64) (httpguard.StrictResponse, error)
	RequestsMade() int
	Close() error
}

type guardedClientFactory func(string, string, time.Duration) (guardedClient, error)

func newGuardedClient(baseURL, cookie string, minInterval time.Duration) (guardedClient, error) {
	return httpguard.New(baseURL, cookie, minInterval)
}

func New(baseURL string) *Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Adapter{baseURL: baseURL, newClient: newGuardedClient}
}

func (a *Adapter) Descriptor() domain.SiteDescriptor {
	capabilities := []domain.Capability{
		domain.CapabilityAuthCheck,
		domain.CapabilitySearch,
		domain.CapabilityBonusRead,
	}
	if a.baseURL == DefaultBaseURL {
		capabilities = append(capabilities, domain.CapabilityMetafile)
	}
	return domain.SiteDescriptor{
		ID:           "tjupt",
		Name:         "TJUPT / 北洋园PT",
		BaseURL:      a.baseURL,
		Stability:    "experimental",
		AuthMethods:  []domain.AuthMethod{domain.AuthMethodCookieHeader},
		Capabilities: capabilities,
	}
}

func (a *Adapter) CheckSession(ctx context.Context, credential site.Credential) (domain.SessionStatus, error) {
	body, finalURL, err := a.get(ctx, credential, "mybonusapps.php", nil)
	if err != nil {
		return domain.SessionStatus{}, err
	}
	state, username := classifyBonusPage(finalURL, body)
	return domain.SessionStatus{State: state, Username: username, ObservedAt: time.Now().UTC()}, nil
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
	switch classifySearchPage(finalURL, body) {
	case domain.AuthenticationUnauthenticated:
		return nil, fmt.Errorf("TJUPT session is not authenticated")
	case domain.AuthenticationIndeterminate:
		return nil, fmt.Errorf("TJUPT returned an unrecognized search page; refusing to treat it as an empty result")
	}
	return parseSearch(body), nil
}

func (a *Adapter) BonusCatalog(ctx context.Context, credential site.Credential) (domain.BonusCatalog, error) {
	body, finalURL, err := a.get(ctx, credential, "mybonusapps.php", nil)
	if err != nil {
		return domain.BonusCatalog{}, err
	}
	state, _ := classifyBonusPage(finalURL, body)
	if state == domain.AuthenticationUnauthenticated {
		return domain.BonusCatalog{}, fmt.Errorf("TJUPT session is not authenticated")
	}
	if state != domain.AuthenticationAuthenticated {
		return domain.BonusCatalog{}, fmt.Errorf("TJUPT returned an unrecognized bonus page; parser is fail-closed")
	}
	rows := parseBonusRows(body)
	if len(rows) == 0 {
		return domain.BonusCatalog{}, fmt.Errorf("TJUPT bonus page structure was not recognized; refusing an empty catalog")
	}
	return domain.BonusCatalog{
		SiteID:     "tjupt",
		Balance:    parseBonusBalance(body),
		Rows:       rows,
		ObservedAt: time.Now().UTC(),
	}, nil
}

func (a *Adapter) get(ctx context.Context, credential site.Credential, path string, query url.Values) ([]byte, *url.URL, error) {
	if credential.Method() != domain.AuthMethodCookieHeader {
		return nil, nil, fmt.Errorf("TJUPT requires cookie_header authentication")
	}
	client, err := a.clientFactory()(a.baseURL, credential.SecretValue(), 2*time.Second)
	if err != nil {
		return nil, nil, err
	}
	defer client.Close()
	return client.Get(ctx, path, query)
}

func (a *Adapter) clientFactory() guardedClientFactory {
	if a.newClient != nil {
		return a.newClient
	}
	return newGuardedClient
}
