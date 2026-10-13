// Package mteam implements conservative M-Team (TP :: mTorrent) reads through
// the site's official JSON API and a user-issued access token.
package mteam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

const (
	// SiteID is persisted in refs. Never change it.
	SiteID = "mteam"
	// Origin is the API gateway every supported read uses. The browsing site
	// lives on other domains and is never contacted.
	Origin = "https://api2.m-team.cc"
	// DefaultBaseURL is the only base URL the adapter ever binds.
	DefaultBaseURL = Origin + "/"

	apiKeyHeader = "x-api-key"
	apiAccept    = "application/json, text/plain, */*"

	profilePath = "api/member/profile"
	searchPath  = "api/torrent/search"

	// The site front end submits most endpoints as multipart forms; an empty
	// form is just the closing boundary.
	formBoundary       = "ptctlMTeamFormBoundary"
	emptyFormBody      = "--" + formBoundary + "--\r\n"
	emptyFormMediaType = "multipart/form-data; boundary=" + formBoundary
	jsonMediaType      = "application/json"

	searchPageSize = 50
	maxQueryBytes  = 256

	// The site asks API clients to leave at least three seconds between calls.
	minRequestInterval     = 3 * time.Second
	maxRequestBytes        = 16 << 10
	maxResponseBytes       = 4 << 20
	maxResponseHeaderBytes = 32 << 10
)

// Adapter reads M-Team through one strict POST per command. It never retries,
// never follows redirects, and never echoes the API key or a response body.
type Adapter struct {
	newClient guardedClientFactory
}

type guardedClient interface {
	PostReadOnce(context.Context, string, string, []byte, string, int64, int64, int64) (httpguard.StrictResponse, error)
	Close() error
}

type guardedClientFactory func(baseURL, apiKey string, minInterval time.Duration) (guardedClient, error)

func newGuardedClient(baseURL, apiKey string, minInterval time.Duration) (guardedClient, error) {
	client, err := httpguard.NewWithHeaderCredential(baseURL, apiKeyHeader, apiKey, minInterval)
	if err != nil {
		// Return an untyped nil so callers never see a non-nil interface
		// wrapping a nil *httpguard.Client.
		return nil, err
	}
	return client, nil
}

func New() *Adapter {
	return &Adapter{newClient: newGuardedClient}
}

func (a *Adapter) Descriptor() domain.SiteDescriptor {
	return domain.SiteDescriptor{
		ID:          SiteID,
		Name:        "M-Team / 馒头PT",
		BaseURL:     DefaultBaseURL,
		Stability:   "experimental",
		AuthMethods: []domain.AuthMethod{domain.AuthMethodAPIKeyHeader},
		Capabilities: []domain.Capability{
			domain.CapabilityAuthCheck,
			domain.CapabilityAccountRead,
			domain.CapabilitySearch,
		},
	}
}

func (a *Adapter) CheckSession(ctx context.Context, credential site.Credential) (domain.SessionStatus, error) {
	data, err := a.call(ctx, credential, profilePath, emptyFormMediaType, []byte(emptyFormBody))
	if errors.Is(err, site.ErrCredentialRejected) {
		return domain.SessionStatus{State: domain.AuthenticationUnauthenticated, ObservedAt: time.Now().UTC()}, nil
	}
	if err != nil {
		return domain.SessionStatus{}, err
	}
	profile, err := parseProfile(data)
	if err != nil || profile.Username == "" {
		return domain.SessionStatus{State: domain.AuthenticationIndeterminate, ObservedAt: time.Now().UTC()}, nil
	}
	return domain.SessionStatus{State: domain.AuthenticationAuthenticated, Username: profile.Username, ObservedAt: time.Now().UTC()}, nil
}

func (a *Adapter) Account(ctx context.Context, credential site.Credential) (domain.AccountSnapshot, error) {
	data, err := a.call(ctx, credential, profilePath, emptyFormMediaType, []byte(emptyFormBody))
	if err != nil {
		return domain.AccountSnapshot{}, err
	}
	profile, err := parseProfile(data)
	if err != nil {
		return domain.AccountSnapshot{}, err
	}
	if profile.Username == "" {
		return domain.AccountSnapshot{}, fmt.Errorf("M-Team profile response had no username; refusing a partial account")
	}
	profile.ObservedAt = time.Now().UTC()
	return profile, nil
}

func (a *Adapter) Search(ctx context.Context, credential site.Credential, query string) ([]domain.TorrentSummary, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("search query is empty")
	}
	if len(query) > maxQueryBytes || !utf8.ValidString(query) || hasControlRune(query) {
		return nil, fmt.Errorf("M-Team search query must be at most %d bytes of printable UTF-8", maxQueryBytes)
	}
	body, err := json.Marshal(searchRequest{Mode: "normal", Keyword: query, PageNumber: 1, PageSize: searchPageSize})
	if err != nil {
		return nil, fmt.Errorf("encode M-Team search request failed")
	}
	data, err := a.call(ctx, credential, searchPath, jsonMediaType, body)
	if err != nil {
		return nil, err
	}
	return parseSearch(data)
}

type searchRequest struct {
	Mode       string `json:"mode"`
	Keyword    string `json:"keyword"`
	PageNumber int    `json:"pageNumber"`
	PageSize   int    `json:"pageSize"`
}

// call performs exactly one API request on a fresh client and returns the
// envelope's data member when the API reports success.
func (a *Adapter) call(ctx context.Context, credential site.Credential, path, contentType string, body []byte) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if credential.Method() != domain.AuthMethodAPIKeyHeader {
		return nil, fmt.Errorf("M-Team requires api_key_header authentication")
	}
	if !validAPIKey(credential.SecretValue()) {
		return nil, fmt.Errorf("M-Team API key is malformed: %w", site.ErrCredentialMalformed)
	}
	client, err := a.clientFactory()(DefaultBaseURL, credential.SecretValue(), minRequestInterval)
	if err != nil || client == nil {
		return nil, fmt.Errorf("open M-Team API client failed")
	}
	defer client.Close()
	response, err := client.PostReadOnce(ctx, path, contentType, body, apiAccept, maxRequestBytes, maxResponseBytes, maxResponseHeaderBytes)
	if err != nil {
		return nil, requestError(err)
	}
	return decodeEnvelope(response)
}

func requestError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("M-Team API request stopped: %w", context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("M-Team API request stopped: %w", context.DeadlineExceeded)
	case errors.Is(err, httpguard.ErrBlockedAddress):
		return fmt.Errorf("M-Team API request failed: %w", site.ErrAddressBlocked)
	}
	return fmt.Errorf("M-Team API request failed")
}

func (a *Adapter) clientFactory() guardedClientFactory {
	if a != nil && a.newClient != nil {
		return a.newClient
	}
	return newGuardedClient
}

// validAPIKey accepts only visible ASCII. The key is sent verbatim in one
// header, so anything else is a paste error rather than a token.
func validAPIKey(key string) bool {
	if key == "" || len(key) > 1024 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

func hasControlRune(value string) bool {
	for _, r := range value {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return true
		}
	}
	return false
}
