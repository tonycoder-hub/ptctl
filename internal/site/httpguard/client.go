package httpguard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/security"
)

const (
	defaultMaxBody          = 8 << 20
	hardMaxResponseHeaders  = 64 << 10
	maxPublicDialCandidates = 4
)

// StrictResponse deliberately omits the response URL and headers. Adapters
// receive only bounded bytes and normalized metadata that are safe to classify.
type StrictResponse struct {
	StatusCode         int
	MediaType          string
	Body               []byte
	ObservedAtStart    time.Time
	ObservedAtEnd      time.Time
	ResponseBytesRead  int64
	ResponseBytesKnown bool
}

type Client struct {
	base        *url.URL
	cookie      string
	http        *http.Client
	strictHTTP  *http.Client
	minInterval time.Duration
	maxBody     int64
	mu          sync.Mutex
	lastRequest time.Time
	requestMu   sync.Mutex
	requests    int
}

func New(baseURL, cookie string, minInterval time.Duration) (*Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse site URL failed")
	}
	if base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("site URL must be an HTTPS location without user info, query, or fragment")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/"

	transport := newTransport(false)
	strictTransport := newTransport(true)

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
	c.strictHTTP = &http.Client{
		Transport: strictTransport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return c, nil
}

func newTransport(strict bool) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	transport := &http.Transport{
		Proxy:                  nil,
		ForceAttemptHTTP2:      !strict,
		TLSClientConfig:        tlsConfig,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  20 * time.Second,
		IdleConnTimeout:        30 * time.Second,
		MaxIdleConns:           2,
		MaxIdleConnsPerHost:    1,
		MaxResponseHeaderBytes: hardMaxResponseHeaders,
		DisableCompression:     true,
		DisableKeepAlives:      strict,
	}
	if strict {
		// A dedicated HTTP/1.1 transport with no connection reuse makes the one
		// effect map to one RoundTrip. In particular, it cannot transparently
		// replay an idempotent GET on a stale pooled connection.
		tlsConfig.NextProtos = []string{"http/1.1"}
		transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
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
		for _, ip := range publicDialCandidates(ips) {
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
	return transport
}

func (c *Client) Get(ctx context.Context, relativePath string, query url.Values) ([]byte, *url.URL, error) {
	target, err := c.target(relativePath, query)
	if err != nil {
		return nil, nil, err
	}

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

	c.recordRequest()
	resp, err := c.http.Do(req)
	if err != nil {
		message := err.Error()
		if urlErr, ok := err.(*url.Error); ok {
			message = urlErr.Err.Error()
		}
		return nil, nil, fmt.Errorf("site read failed: %s", security.Redact(message))
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

// GetOnce performs one non-following, non-retrying GET and bounds both
// response headers and body. The caller should use a freshly-created Client;
// Fetch sessions enforce that this method is invoked at most once.
func (c *Client) GetOnce(ctx context.Context, relativePath string, query url.Values, accept string, maxBody, maxHeaderBytes int64) (StrictResponse, error) {
	var result StrictResponse
	if maxBody <= 0 || maxBody > 32<<20 || maxHeaderBytes <= 0 || maxHeaderBytes > hardMaxResponseHeaders {
		return result, fmt.Errorf("site response budget is invalid")
	}
	if accept == "" || !asciiHeaderValue(accept) {
		return result, fmt.Errorf("site accept header is invalid")
	}
	target, err := c.target(relativePath, query)
	if err != nil {
		return result, err
	}
	if err := c.wait(ctx); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return result, fmt.Errorf("build site request failed")
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "ptctl/0.1 (+https://github.com/tonycoder-hub/ptctl; conservative-read-only-client)")
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}
	// The dedicated fresh H1-only transport neither pools the connection nor
	// follows redirects, so this effect cannot be replayed by the transport.
	req.Close = true
	if c.strictHTTP == nil {
		return result, fmt.Errorf("strict site transport is unavailable")
	}

	result.ObservedAtStart = time.Now().UTC()
	c.recordRequest()
	resp, err := c.strictHTTP.Do(req)
	if err != nil {
		result.ObservedAtEnd = time.Now().UTC()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, fmt.Errorf("site request canceled: %w", ctxErr)
		}
		return result, fmt.Errorf("site request failed")
	}
	defer resp.Body.Close()
	result.StatusCode = resp.StatusCode
	if responseHeaderSize(resp.Header) > maxHeaderBytes {
		result.ObservedAtEnd = time.Now().UTC()
		return result, fmt.Errorf("site response headers exceeded the configured budget")
	}
	if len(resp.Header.Values("Content-Range")) != 0 {
		result.ObservedAtEnd = time.Now().UTC()
		return result, fmt.Errorf("partial site response was rejected")
	}
	encodings := resp.Header.Values("Content-Encoding")
	if len(encodings) > 1 || (len(encodings) == 1 && !strings.EqualFold(strings.TrimSpace(encodings[0]), "identity")) || resp.Uncompressed {
		result.ObservedAtEnd = time.Now().UTC()
		return result, fmt.Errorf("encoded site response was rejected")
	}
	if len(resp.TransferEncoding) > 1 || (len(resp.TransferEncoding) == 1 && !strings.EqualFold(resp.TransferEncoding[0], "chunked")) {
		result.ObservedAtEnd = time.Now().UTC()
		return result, fmt.Errorf("encoded site transfer was rejected")
	}
	if len(resp.Trailer) != 0 {
		result.ObservedAtEnd = time.Now().UTC()
		return result, fmt.Errorf("site response trailers were rejected")
	}
	contentTypes := resp.Header.Values("Content-Type")
	if len(contentTypes) > 1 {
		result.ObservedAtEnd = time.Now().UTC()
		return result, fmt.Errorf("site response content type was invalid")
	}
	if len(contentTypes) == 1 && strings.TrimSpace(contentTypes[0]) != "" {
		rawType := strings.TrimSpace(contentTypes[0])
		mediaType, _, parseErr := mime.ParseMediaType(rawType)
		if parseErr != nil {
			result.ObservedAtEnd = time.Now().UTC()
			return result, fmt.Errorf("site response content type was invalid")
		}
		result.MediaType = strings.ToLower(mediaType)
	}
	if resp.ContentLength > maxBody {
		result.ObservedAtEnd = time.Now().UTC()
		return result, fmt.Errorf("site response exceeded the configured budget")
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	result.ResponseBytesRead = int64(len(body))
	result.ObservedAtEnd = time.Now().UTC()
	if readErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, fmt.Errorf("site response read canceled: %w", ctxErr)
		}
		return result, fmt.Errorf("site response read failed")
	}
	if int64(len(body)) > maxBody {
		return result, fmt.Errorf("site response exceeded the configured budget")
	}
	result.ResponseBytesKnown = true
	result.Body = body
	return result, nil
}

func (c *Client) RequestsMade() int {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	return c.requests
}

func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
	if c.strictHTTP != nil {
		c.strictHTTP.CloseIdleConnections()
	}
	return nil
}

func (c *Client) recordRequest() {
	c.requestMu.Lock()
	c.requests++
	c.requestMu.Unlock()
}

func (c *Client) target(relativePath string, query url.Values) (*url.URL, error) {
	if strings.HasPrefix(relativePath, "/") || strings.Contains(relativePath, "\\") {
		return nil, fmt.Errorf("site path must be a relative URL path")
	}
	ref, err := url.Parse(relativePath)
	if err != nil || ref.IsAbs() || ref.Host != "" || ref.RawQuery != "" || ref.Fragment != "" {
		return nil, fmt.Errorf("invalid site path")
	}
	target := c.base.ResolveReference(ref)
	if target.Scheme != "https" || !strings.EqualFold(target.Hostname(), c.base.Hostname()) || effectivePort(target) != effectivePort(c.base) {
		return nil, fmt.Errorf("site request escaped configured origin")
	}
	target.RawQuery = query.Encode()
	return target, nil
}

func responseHeaderSize(header http.Header) int64 {
	// Transport applies the 64 KiB hard bound. This deterministic decompressed
	// accounting additionally enforces any smaller caller budget.
	var total int64 = 2
	for key, values := range header {
		for _, value := range values {
			total += int64(len(key) + len(value) + 4)
		}
	}
	return total
}

func asciiHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
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

func publicDialCandidates(ips []net.IP) []net.IP {
	result := make([]net.IP, 0, maxPublicDialCandidates)
	for _, ip := range ips {
		if !publicIP(ip) {
			continue
		}
		result = append(result, ip)
		if len(result) == maxPublicDialCandidates {
			break
		}
	}
	return result
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
