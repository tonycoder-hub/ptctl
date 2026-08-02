package qbittorrent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/security"
)

type Adapter struct {
	base *url.URL
}

func New(endpoint string) (*Adapter, error) {
	base, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse qBittorrent endpoint: %w", err)
	}
	if base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Hostname() == "" {
		return nil, fmt.Errorf("qBittorrent endpoint must be an origin without user info, query, or fragment")
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && loopbackHost(base.Hostname())) {
		return nil, fmt.Errorf("qBittorrent endpoint must use HTTPS; plain HTTP is allowed only for an explicit loopback address")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/"
	return &Adapter{base: base}, nil
}

func (a *Adapter) Status(ctx context.Context, credential downloader.Credential) (downloader.Status, error) {
	client, err := a.authenticatedClient(ctx, credential)
	if err != nil {
		return downloader.Status{}, err
	}
	version, err := a.getText(ctx, client, "api/v2/app/version")
	if err != nil {
		return downloader.Status{}, err
	}
	webAPI, err := a.getText(ctx, client, "api/v2/app/webapiVersion")
	if err != nil {
		return downloader.Status{}, err
	}
	return downloader.Status{
		Driver:        "qbittorrent",
		Endpoint:      origin(a.base),
		Version:       strings.TrimSpace(version),
		WebAPIVersion: strings.TrimSpace(webAPI),
		ObservedAt:    time.Now().UTC(),
	}, nil
}

func (a *Adapter) Torrents(ctx context.Context, credential downloader.Credential) ([]downloader.Torrent, error) {
	client, err := a.authenticatedClient(ctx, credential)
	if err != nil {
		return nil, err
	}
	body, err := a.get(ctx, client, "api/v2/torrents/info", 8<<20)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Hash       string  `json:"hash"`
		Name       string  `json:"name"`
		Size       int64   `json:"size"`
		Progress   float64 `json:"progress"`
		State      string  `json:"state"`
		SavePath   string  `json:"save_path"`
		Downloaded int64   `json:"downloaded"`
		Uploaded   int64   `json:"uploaded"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode qBittorrent torrent list: %w", err)
	}
	if len(raw) > 100_000 {
		return nil, fmt.Errorf("qBittorrent returned too many torrents")
	}
	items := make([]downloader.Torrent, 0, len(raw))
	for _, item := range raw {
		items = append(items, downloader.Torrent{
			Hash: item.Hash, Name: item.Name, SizeBytes: item.Size, Progress: item.Progress,
			State: item.State, SavePath: item.SavePath, Downloaded: item.Downloaded, Uploaded: item.Uploaded,
		})
	}
	return items, nil
}

func (a *Adapter) authenticatedClient(ctx context.Context, credential downloader.Credential) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create qBittorrent cookie jar: %w", err)
	}
	transport := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConnsPerHost:   1,
	}
	client := &http.Client{
		Jar:       jar,
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !sameOrigin(req.URL, a.base) {
				return fmt.Errorf("qBittorrent cross-origin redirect blocked")
			}
			return nil
		},
	}
	form := url.Values{"username": {credential.UsernameValue()}, "password": {credential.PasswordValue()}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.resolve("api/v2/auth/login").String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build qBittorrent login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", origin(a.base)+"/")
	req.Header.Set("User-Agent", "ptctl/0.1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qBittorrent login failed: %s", security.Redact(err.Error()))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1025))
	if err != nil {
		return nil, fmt.Errorf("read qBittorrent login response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || len(body) > 1024 || strings.TrimSpace(string(body)) != "Ok." {
		return nil, fmt.Errorf("qBittorrent rejected the login")
	}
	return client, nil
}

func (a *Adapter) getText(ctx context.Context, client *http.Client, path string) (string, error) {
	body, err := a.get(ctx, client, path, 64<<10)
	return string(body), err
}

func (a *Adapter) get(ctx context.Context, client *http.Client, path string, maxBody int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.resolve(path).String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build qBittorrent request: %w", err)
	}
	req.Header.Set("Accept", "application/json,text/plain;q=0.9")
	req.Header.Set("Referer", origin(a.base)+"/")
	req.Header.Set("User-Agent", "ptctl/0.1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qBittorrent read failed: %s", security.Redact(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qBittorrent returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read qBittorrent response: %w", err)
	}
	if int64(len(body)) > maxBody {
		return nil, fmt.Errorf("qBittorrent response exceeded %d bytes", maxBody)
	}
	return body, nil
}

func (a *Adapter) resolve(path string) *url.URL {
	return a.base.ResolveReference(&url.URL{Path: path})
}

func origin(u *url.URL) string { return u.Scheme + "://" + u.Host }

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
