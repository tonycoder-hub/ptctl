package transmission

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
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
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

const (
	defaultRPCPath       = "/transmission/rpc"
	maxLedgerBodyBytes   = int64(8 << 20)
	maxLedgerJobs        = 25_000
	maxHeaderBytes       = 64 << 10
	maxSessionBodyBytes  = int64(64 << 10)
	maxSessionTokenBytes = 4 << 10
	maxVersionBytes      = 256
)

type protocol uint8

const (
	protocolLegacy protocol = iota + 1
	protocolJSONRPC
)

type Adapter struct {
	endpoint *url.URL
}

type readSession struct {
	adapter    *Adapter
	client     *http.Client
	transport  *http.Transport
	credential downloader.Credential

	mu           sync.Mutex
	requestsMade int
	closed       bool
	csrfToken    string
	protocol     protocol
	nextID       int64
	version      string
	rpcVersion   string
	addAttempted bool
}

type openSessionError struct {
	err      error
	requests int
}

func (err *openSessionError) Error() string     { return err.err.Error() }
func (err *openSessionError) Unwrap() error     { return err.err }
func (err *openSessionError) RequestsMade() int { return err.requests }

var (
	_ downloader.Driver           = (*Adapter)(nil)
	_ downloader.LedgerDriver     = (*Adapter)(nil)
	_ downloader.StoppedAddDriver = (*Adapter)(nil)
	_ downloader.LedgerSession    = (*readSession)(nil)
	_ downloader.MutationSession  = (*readSession)(nil)
)

func New(endpoint string) (*Adapter, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid Transmission endpoint")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Hostname() == "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("Transmission endpoint must be an RPC URL without user info, query, or fragment")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && numericLoopback(parsed.Hostname())) {
		return nil, fmt.Errorf("Transmission endpoint must use HTTPS; plain HTTP is allowed only for an explicit loopback address")
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = defaultRPCPath
	}
	if !strings.HasPrefix(parsed.EscapedPath(), "/") || strings.HasSuffix(parsed.Path, "/") || strings.Contains(parsed.EscapedPath(), "//") {
		return nil, fmt.Errorf("Transmission endpoint must name one RPC path")
	}
	return &Adapter{endpoint: parsed}, nil
}

func (a *Adapter) OpenReadSession(ctx context.Context, credential downloader.Credential) (downloader.LedgerSession, error) {
	session, err := a.openReadSession(ctx, credential)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (a *Adapter) openReadSession(ctx context.Context, credential downloader.Credential) (*readSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, countedOpenError(err, 0)
	}
	if !validBasicUsername(credential.UsernameValue()) {
		return nil, countedOpenError(fmt.Errorf("invalid Transmission username"), 0)
	}
	transport := &http.Transport{
		Proxy:                  nil,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  15 * time.Second,
		MaxResponseHeaderBytes: maxHeaderBytes,
		DisableKeepAlives:      true,
		ForceAttemptHTTP2:      false,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("Transmission redirects are blocked")
		},
	}
	session := &readSession{adapter: a, client: client, transport: transport, credential: credential, nextID: 1}
	if err := session.bootstrap(ctx); err != nil {
		requests := session.RequestsMade()
		_ = session.Close()
		return nil, countedOpenError(err, requests)
	}
	return session, nil
}

func countedOpenError(err error, requests int) error {
	return &openSessionError{err: err, requests: requests}
}

func (a *Adapter) Status(ctx context.Context, credential downloader.Credential) (downloader.Status, error) {
	session, err := a.openReadSession(ctx, credential)
	if err != nil {
		return downloader.Status{}, err
	}
	defer session.Close()
	return downloader.Status{
		Driver:        downloader.DriverTransmission,
		Endpoint:      a.endpoint.Scheme + "://" + a.endpoint.Host,
		Version:       session.version,
		WebAPIVersion: session.rpcVersion,
		ObservedAt:    time.Now().UTC(),
	}, nil
}

func (a *Adapter) Torrents(ctx context.Context, credential downloader.Credential) ([]downloader.Torrent, error) {
	session, err := a.openReadSession(ctx, credential)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	snapshot, err := session.ReadLedger(ctx)
	if err != nil {
		return nil, err
	}
	return snapshot.Jobs, nil
}

func (s *readSession) bootstrap(ctx context.Context) error {
	probe, err := marshalLegacyRequest("session-get", map[string]any{"fields": []string{"version", "rpc-version-semver", "rpc-version"}}, 1)
	if err != nil {
		return fmt.Errorf("build Transmission version probe")
	}
	response, err := s.execute(ctx, probe, "", maxSessionBodyBytes)
	if err != nil {
		return err
	}
	if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden {
		return fmt.Errorf("Transmission rejected the credentials")
	}
	if response.status != http.StatusConflict {
		return fmt.Errorf("Transmission did not provide the required CSRF version handshake")
	}
	if response.sessionID == "" || len(response.sessionID) > maxSessionTokenBytes || !safeHeaderValue(response.sessionID) {
		return fmt.Errorf("Transmission returned an invalid CSRF session token")
	}
	s.csrfToken = response.sessionID
	if response.rpcVersion == "" {
		s.protocol = protocolLegacy
	} else {
		major, _, _, parseErr := parseSemver(response.rpcVersion)
		if parseErr != nil || major != 6 {
			return fmt.Errorf("Transmission RPC protocol is unsupported")
		}
		s.protocol = protocolJSONRPC
	}
	result, _, err := s.call(ctx, "session-get", "session_get", map[string]any{
		"fields": sessionFields(s.protocol),
	}, maxSessionBodyBytes)
	if err != nil {
		return err
	}
	version, rpcVersion, err := decodeSessionVersion(result, s.protocol)
	if err != nil {
		return err
	}
	major, minor, _, err := parseSemver(rpcVersion)
	if err != nil || s.protocol == protocolLegacy && (major != 5 || minor != 3) || s.protocol == protocolJSONRPC && major != 6 {
		return fmt.Errorf("Transmission RPC protocol is unsupported")
	}
	if response.rpcVersion != "" && response.rpcVersion != rpcVersion {
		return fmt.Errorf("Transmission RPC version changed during the handshake")
	}
	if s.RequestsMade() != 2 {
		return fmt.Errorf("Transmission handshake request count is invalid")
	}
	s.version = version
	s.rpcVersion = rpcVersion
	return nil
}

func sessionFields(value protocol) []string {
	if value == protocolJSONRPC {
		return []string{"version", "rpc_version_semver", "rpc_version"}
	}
	return []string{"version", "rpc-version-semver", "rpc-version"}
}

type rawHTTPResponse struct {
	status     int
	sessionID  string
	rpcVersion string
	body       []byte
}

func (s *readSession) execute(ctx context.Context, body []byte, token string, maxBody int64) (rawHTTPResponse, error) {
	return s.executeReader(ctx, bytes.NewReader(body), int64(len(body)), token, maxBody)
}

func (s *readSession) executeReader(ctx context.Context, body io.Reader, contentLength int64, token string, maxBody int64) (rawHTTPResponse, error) {
	if maxBody <= 0 || maxBody > 32<<20 {
		return rawHTTPResponse{}, fmt.Errorf("invalid Transmission response limit")
	}
	if body == nil || contentLength < 0 || contentLength > 64<<20 {
		return rawHTTPResponse{}, fmt.Errorf("invalid Transmission request body")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.adapter.endpoint.String(), body)
	if err != nil {
		return rawHTTPResponse{}, fmt.Errorf("build Transmission request")
	}
	req.ContentLength = contentLength
	req.Close = true
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ptctl/0.1")
	if token != "" {
		req.Header.Set("X-Transmission-Session-Id", token)
	}
	req.SetBasicAuth(s.credential.UsernameValue(), s.credential.PasswordValue())
	if err := s.beginRequest(ctx); err != nil {
		return rawHTTPResponse{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return rawHTTPResponse{}, contextErr
		}
		return rawHTTPResponse{}, fmt.Errorf("Transmission request failed")
	}
	defer resp.Body.Close()
	result := rawHTTPResponse{
		status:     resp.StatusCode,
		sessionID:  resp.Header.Get("X-Transmission-Session-Id"),
		rpcVersion: resp.Header.Get("X-Transmission-Rpc-Version"),
	}
	if len(result.sessionID) > maxSessionTokenBytes || len(result.rpcVersion) > maxVersionBytes ||
		result.sessionID != "" && !safeHeaderValue(result.sessionID) || result.rpcVersion != "" && !safeHeaderValue(result.rpcVersion) {
		return rawHTTPResponse{}, fmt.Errorf("Transmission returned an invalid response header")
	}
	if resp.StatusCode == http.StatusOK {
		if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
			return rawHTTPResponse{}, fmt.Errorf("Transmission returned an encoded response")
		}
		if resp.Header.Get("Content-Range") != "" {
			return rawHTTPResponse{}, fmt.Errorf("Transmission returned a partial response")
		}
		contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
		if contentType != "" {
			mediaType, _, parseErr := mime.ParseMediaType(contentType)
			if parseErr != nil || mediaType != "application/json" {
				return rawHTTPResponse{}, fmt.Errorf("Transmission returned a non-JSON response")
			}
		}
	}
	if resp.ContentLength > maxBody {
		return rawHTTPResponse{}, fmt.Errorf("Transmission response exceeded its byte limit")
	}
	limited := &io.LimitedReader{R: resp.Body, N: maxBody + 1}
	payload, readErr := io.ReadAll(limited)
	if readErr != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return rawHTTPResponse{}, contextErr
		}
		return rawHTTPResponse{}, fmt.Errorf("read Transmission response")
	}
	if int64(len(payload)) > maxBody {
		return rawHTTPResponse{}, fmt.Errorf("Transmission response exceeded its byte limit")
	}
	if err := ctx.Err(); err != nil {
		return rawHTTPResponse{}, err
	}
	result.body = payload
	return result, nil
}

func (s *readSession) call(ctx context.Context, legacyMethod, rpcMethod string, arguments map[string]any, maxBody int64) (json.RawMessage, int64, error) {
	s.mu.Lock()
	id := s.nextID
	s.nextID++
	valueProtocol := s.protocol
	token := s.csrfToken
	s.mu.Unlock()
	var body []byte
	var err error
	if valueProtocol == protocolJSONRPC {
		body, err = marshalJSONRPCRequest(rpcMethod, arguments, id)
	} else {
		body, err = marshalLegacyRequest(legacyMethod, arguments, id)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("build Transmission RPC request")
	}
	response, err := s.execute(ctx, body, token, maxBody)
	if err != nil {
		return nil, 0, err
	}
	if response.status == http.StatusConflict {
		return nil, int64(len(response.body)), fmt.Errorf("Transmission CSRF session expired; automatic replay is disabled")
	}
	if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden {
		return nil, int64(len(response.body)), fmt.Errorf("Transmission rejected the credentials")
	}
	if response.status != http.StatusOK {
		return nil, int64(len(response.body)), fmt.Errorf("Transmission returned HTTP %d", response.status)
	}
	result, err := decodeRPCResult(response.body, valueProtocol, id)
	return result, int64(len(response.body)), err
}

func marshalLegacyRequest(method string, arguments map[string]any, id int64) ([]byte, error) {
	return json.Marshal(struct {
		Method    string         `json:"method"`
		Arguments map[string]any `json:"arguments"`
		Tag       int64          `json:"tag"`
	}{Method: method, Arguments: arguments, Tag: id})
}

func marshalJSONRPCRequest(method string, params map[string]any, id int64) ([]byte, error) {
	return json.Marshal(struct {
		JSONRPC string         `json:"jsonrpc"`
		Method  string         `json:"method"`
		Params  map[string]any `json:"params"`
		ID      int64          `json:"id"`
	}{JSONRPC: "2.0", Method: method, Params: params, ID: id})
}

func (s *readSession) RequestsMade() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requestsMade
}

func (s *readSession) beginRequest(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("Transmission read session is closed")
	}
	s.requestsMade++
	return nil
}

func (s *readSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.transport.CloseIdleConnections()
	return nil
}

func numericLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func safeHeaderValue(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

func validBasicUsername(value string) bool {
	if len(value) > 4<<10 || strings.ContainsRune(value, ':') || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character == utf8.RuneError || character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func parseSemver(value string) (int, int, int, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("invalid semver")
	}
	values := [3]int{}
	for index, part := range parts {
		if part == "" || len(part) > 9 || len(part) > 1 && part[0] == '0' {
			return 0, 0, 0, fmt.Errorf("invalid semver")
		}
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 0 {
			return 0, 0, 0, fmt.Errorf("invalid semver")
		}
		values[index] = parsed
	}
	return values[0], values[1], values[2], nil
}
