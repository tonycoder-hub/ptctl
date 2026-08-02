package httpguard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/security"
)

const defaultMaxBody = 8 << 20

type Client struct {
	base        *url.URL
	cookie      string
	http        *http.Client
	minInterval time.Duration
	maxBody     int64
	mu          sync.Mutex
	lastRequest time.Time
}

func New(baseURL, cookie string, minInterval time.Duration) (*Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse site URL: %w", err)
	}
	if base.Scheme != "https" || base.Hostname() == "" || base.User != nil {
		return nil, fmt.Errorf("site URL must be an HTTPS origin without user info")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/"
	base.RawQuery = ""
	base.Fragment = ""

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   1,
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid dial address")
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve site host: %w", err)
		}
		var lastErr error
		for _, ip := range ips {
			if !publicIP(ip) {
				continue
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr != nil {
			return nil, fmt.Errorf("connect to site host: %w", lastErr)
		}
		return nil, fmt.Errorf("site host resolved only to blocked address ranges")
	}

	c := &Client{base: base, cookie: cookie, minInterval: minInterval, maxBody: defaultMaxBody}
	c.http = &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Hostname(), base.Hostname()) || effectivePort(req.URL) != effectivePort(base) {
				return errors.New("cross-origin or HTTPS-downgrade redirect blocked")
			}
			return nil
		},
	}
	return c, nil
}

func (c *Client) Get(ctx context.Context, relativePath string, query url.Values) ([]byte, *url.URL, error) {
	if strings.HasPrefix(relativePath, "/") || strings.Contains(relativePath, "\\") {
		return nil, nil, fmt.Errorf("site path must be a relative URL path")
	}
	ref, err := url.Parse(relativePath)
	if err != nil || ref.IsAbs() || ref.Host != "" {
		return nil, nil, fmt.Errorf("invalid site path")
	}
	target := c.base.ResolveReference(ref)
	if target.Scheme != "https" || !strings.EqualFold(target.Hostname(), c.base.Hostname()) || effectivePort(target) != effectivePort(c.base) {
		return nil, nil, fmt.Errorf("site request escaped configured origin")
	}
	target.RawQuery = query.Encode()

	if err := c.wait(ctx); err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build site request: %w", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.1")
	req.Header.Set("User-Agent", "ptctl/0.1 (+https://github.com/tonycoder-hub/ptctl; conservative-read-only-client)")
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("site read failed: %s", security.Redact(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, resp.Request.URL, fmt.Errorf("site rate limited the request (HTTP 429); retry manually after the server-specified interval")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, resp.Request.URL, fmt.Errorf("site returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return nil, resp.Request.URL, fmt.Errorf("read site response: %w", err)
	}
	if int64(len(body)) > c.maxBody {
		return nil, resp.Request.URL, fmt.Errorf("site response exceeded %d bytes", c.maxBody)
	}
	return body, resp.Request.URL, nil
}

func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	wait := c.minInterval - time.Since(c.lastRequest)
	if c.lastRequest.IsZero() || wait <= 0 {
		c.lastRequest = time.Now()
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		c.lastRequest = time.Now()
		return nil
	}
}

func publicIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, network := range blockedNetworks {
		if network.Contains(ip) {
			return false
		}
	}
	return true
}

var blockedNetworks = mustNetworks(
	"0.0.0.0/8",
	"100.64.0.0/10",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"240.0.0.0/4",
	"2001:db8::/32",
)

func mustNetworks(cidrs ...string) []*net.IPNet {
	result := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		result = append(result, network)
	}
	return result
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return strconv.Itoa(80)
}
