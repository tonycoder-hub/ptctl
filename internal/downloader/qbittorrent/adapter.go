package qbittorrent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

const (
	maxTorrentListBody = int64(8 << 20)
	maxTorrentJobs     = 25_000
	maxMagnetBytes     = 64 << 10
	maxMagnetPairs     = 256
	maxMagnetXT        = 8
	maxMagnetXTBytes   = 256
	maxTorrentFields   = 256

	maxOpaqueJobKeyBytes = 256
	maxJobNameBytes      = 64 << 10
	maxJobStateBytes     = 4 << 10
	maxJobPathBytes      = 64 << 10
)

const (
	evidenceMagnetBTIHHex    = "magnet_xt_btih_hex"
	evidenceMagnetBTIHBase32 = "magnet_xt_btih_base32"
	evidenceMagnetBTMHSHA256 = "magnet_xt_btmh_sha256"

	issueMagnetFormat       = "identity.magnet.invalid_format"
	issueMagnetTooLarge     = "identity.magnet.too_large"
	issueMagnetTooManyPairs = "identity.magnet.too_many_pairs"
	issueMagnetTooManyXT    = "identity.magnet.too_many_xt"
	issueMagnetXTTooLarge   = "identity.magnet.xt_too_large"
	issueMagnetXTEscape     = "identity.magnet.xt_invalid_escape"
	issueMagnetXTUnsafe     = "identity.magnet.xt_unsafe_character"
	issueMagnetBTIH         = "identity.magnet.btih_malformed"
	issueMagnetBTMH         = "identity.magnet.btmh_malformed"
	issueMagnetBTIHConflict = "identity.magnet.btih_conflict"
	issueMagnetBTMHConflict = "identity.magnet.btmh_conflict"
)

type Adapter struct {
	base *url.URL
}

type readSession struct {
	adapter   *Adapter
	client    *http.Client
	transport *http.Transport

	mu           sync.Mutex
	requestsMade int
	closed       bool
}

type rawTorrent struct {
	ctx         context.Context
	Hash        string  `json:"hash"`
	MagnetURI   string  `json:"magnet_uri"`
	Name        string  `json:"name"`
	Size        int64   `json:"size"`
	Progress    float64 `json:"progress"`
	State       string  `json:"state"`
	SavePath    string  `json:"save_path"`
	ContentPath string  `json:"content_path"`
	Downloaded  int64   `json:"downloaded"`
	Uploaded    int64   `json:"uploaded"`
}

func (item *rawTorrent) UnmarshalJSON(data []byte) error {
	if err := validateStrictJSONStrings(data, hardMaxTorrentJSONDepth); err != nil {
		return fmt.Errorf("decode qBittorrent torrent object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode qBittorrent torrent object")
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return fmt.Errorf("decode qBittorrent torrent object")
	}
	seen := make(map[string]struct{})
	fields := 0
	for decoder.More() {
		if item.ctx != nil {
			if err := item.ctx.Err(); err != nil {
				return err
			}
		}
		fields++
		if fields > maxTorrentFields {
			return fmt.Errorf("qBittorrent torrent object has too many fields")
		}
		token, err = decoder.Token()
		if err != nil {
			return fmt.Errorf("decode qBittorrent torrent object")
		}
		key, ok := token.(string)
		if !ok || !validDecodedJSONFieldKey(key) {
			return fmt.Errorf("decode qBittorrent torrent object")
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("qBittorrent torrent object contains a duplicate field")
		}
		seen[key] = struct{}{}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return fmt.Errorf("decode qBittorrent torrent object field")
		}
		var stringTarget *string
		switch key {
		case "hash":
			stringTarget = &item.Hash
		case "magnet_uri":
			stringTarget = &item.MagnetURI
		case "name":
			stringTarget = &item.Name
		case "size":
			if err := decodeJSONInt64(raw, &item.Size); err != nil {
				return fmt.Errorf("decode qBittorrent torrent object field")
			}
		case "progress":
			if err := decodeJSONFloat64(raw, &item.Progress); err != nil {
				return fmt.Errorf("decode qBittorrent torrent object field")
			}
		case "state":
			stringTarget = &item.State
		case "save_path":
			stringTarget = &item.SavePath
		case "content_path":
			stringTarget = &item.ContentPath
		case "downloaded":
			if err := decodeJSONInt64(raw, &item.Downloaded); err != nil {
				return fmt.Errorf("decode qBittorrent torrent object field")
			}
		case "uploaded":
			if err := decodeJSONInt64(raw, &item.Uploaded); err != nil {
				return fmt.Errorf("decode qBittorrent torrent object field")
			}
		}
		if stringTarget != nil {
			value, err := decodeStrictJSONStringValue(raw)
			if err != nil {
				return fmt.Errorf("decode qBittorrent torrent object field")
			}
			*stringTarget = value
		}
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return fmt.Errorf("decode qBittorrent torrent object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("decode qBittorrent torrent object")
	}
	return nil
}

type magnetIdentity struct {
	v1       string
	v2       string
	status   downloader.IdentityStatus
	evidence []string
	issues   []string
}

type openSessionError struct {
	err      error
	requests int
}

func (err *openSessionError) Error() string { return err.err.Error() }
func (err *openSessionError) Unwrap() error { return err.err }
func (err *openSessionError) RequestsMade() int {
	return err.requests
}

var (
	_ downloader.Driver        = (*Adapter)(nil)
	_ downloader.LedgerSession = (*readSession)(nil)
)

func New(endpoint string) (*Adapter, error) {
	base, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid qBittorrent endpoint")
	}
	if base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Hostname() == "" || base.Opaque != "" || base.EscapedPath() != "" && base.EscapedPath() != "/" {
		return nil, fmt.Errorf("qBittorrent endpoint must be an origin without user info, query, or fragment")
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && loopbackHost(base.Hostname())) {
		return nil, fmt.Errorf("qBittorrent endpoint must use HTTPS; plain HTTP is allowed only for an explicit loopback address")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/"
	return &Adapter{base: base}, nil
}

// OpenReadSession performs one login and returns a reusable, read-only ledger
// session. It neither retries nor logs out implicitly.
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
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, countedOpenError(fmt.Errorf("create qBittorrent cookie jar: %w", err), 0)
	}
	transport := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	client := &http.Client{
		Jar:       jar,
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("qBittorrent redirects are blocked")
		},
	}
	session := &readSession{adapter: a, client: client, transport: transport}
	if err := session.login(ctx, credential); err != nil {
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
	version, err := session.getText(ctx, "api/v2/app/version")
	if err != nil {
		return downloader.Status{}, err
	}
	webAPI, err := session.getText(ctx, "api/v2/app/webapiVersion")
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

// Torrents is the compatibility wrapper for callers that do not need ledger
// timing or capabilities. It still uses exactly one login and one list read.
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

func (s *readSession) ReadLedger(ctx context.Context) (downloader.LedgerSnapshot, error) {
	started := time.Now().UTC()
	body, err := s.get(ctx, "api/v2/torrents/info", maxTorrentListBody)
	if err != nil {
		return downloader.LedgerSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return downloader.LedgerSnapshot{}, err
	}
	jobs, err := decodeTorrentLedgerContext(ctx, body, maxTorrentJobs)
	if err != nil {
		return downloader.LedgerSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return downloader.LedgerSnapshot{}, err
	}
	return downloader.LedgerSnapshot{
		Driver:          "qbittorrent",
		ObservedAtStart: started,
		ObservedAtEnd:   time.Now().UTC(),
		Complete:        true,
		Capabilities: downloader.LedgerCapabilities{
			TypedInfoHashes: true,
			ContentPath:     true,
			RawMetafile:     false,
			JobFiles:        true,
		},
		Jobs: jobs,
	}, nil
}

func (s *readSession) RequestsMade() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requestsMade
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

func (s *readSession) login(ctx context.Context, credential downloader.Credential) error {
	form := url.Values{"username": {credential.UsernameValue()}, "password": {credential.PasswordValue()}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.adapter.resolve("api/v2/auth/login").String(), strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build qBittorrent login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", origin(s.adapter.base)+"/")
	req.Header.Set("User-Agent", "ptctl/0.1")
	if err := s.beginRequest(ctx); err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return fmt.Errorf("qBittorrent login failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1025))
	if err != nil {
		return fmt.Errorf("read qBittorrent login response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || len(body) > 1024 || strings.TrimSpace(string(body)) != "Ok." {
		return fmt.Errorf("qBittorrent rejected the login")
	}
	return nil
}

func (s *readSession) getText(ctx context.Context, path string) (string, error) {
	body, err := s.get(ctx, path, 64<<10)
	return string(body), err
}

func (s *readSession) get(ctx context.Context, path string, maxBody int64) ([]byte, error) {
	resp, err := s.getResponse(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read qBittorrent response: %w", err)
	}
	if int64(len(body)) > maxBody {
		return nil, fmt.Errorf("qBittorrent response exceeded %d bytes", maxBody)
	}
	return body, nil
}

func (s *readSession) getResponse(ctx context.Context, path string, query url.Values) (*http.Response, error) {
	target := s.adapter.resolve(path)
	if query != nil {
		target.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build qBittorrent request: %w", err)
	}
	req.Header.Set("Accept", "application/json,text/plain;q=0.9")
	req.Header.Set("Referer", origin(s.adapter.base)+"/")
	req.Header.Set("User-Agent", "ptctl/0.1")
	if err := s.beginRequest(ctx); err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, fmt.Errorf("qBittorrent read failed")
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("qBittorrent returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func (s *readSession) beginRequest(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("qBittorrent read session is closed")
	}
	s.requestsMade++
	return nil
}

func decodeTorrentLedger(body []byte) ([]downloader.Torrent, error) {
	return decodeTorrentLedgerContext(context.Background(), body, maxTorrentJobs)
}

func decodeTorrentLedgerLimited(body []byte, maxJobs int) ([]downloader.Torrent, error) {
	return decodeTorrentLedgerContext(context.Background(), body, maxJobs)
}

func decodeTorrentLedgerContext(ctx context.Context, body []byte, maxJobs int) ([]downloader.Torrent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxJobs <= 0 || maxJobs > maxTorrentJobs {
		return nil, fmt.Errorf("invalid qBittorrent torrent limit")
	}
	trimmed := bytes.TrimSpace(body)
	if !utf8.Valid(trimmed) || len(trimmed) < 2 || trimmed[0] != '[' || trimmed[len(trimmed)-1] != ']' {
		return nil, fmt.Errorf("decode qBittorrent torrent list: response is not a JSON array")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, fmt.Errorf("decode qBittorrent torrent list")
	}
	initialCapacity := min(maxJobs, min(1024, len(trimmed)/128))
	jobs := make([]downloader.Torrent, 0, initialCapacity)
	seenJobKeys := make(map[string]struct{}, initialCapacity)
	for index := 0; decoder.More(); index++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if index >= maxJobs {
			return nil, fmt.Errorf("qBittorrent returned too many torrents")
		}
		item := rawTorrent{ctx: ctx}
		if err := decoder.Decode(&item); err != nil {
			return nil, fmt.Errorf("decode qBittorrent torrent list: %w", err)
		}
		job, err := normalizeTorrent(index, item)
		if err != nil {
			return nil, err
		}
		if _, exists := seenJobKeys[job.Hash]; exists {
			return nil, fmt.Errorf("qBittorrent torrent list contains a duplicate opaque job key")
		}
		seenJobKeys[job.Hash] = struct{}{}
		jobs = append(jobs, job)
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim(']') {
		return nil, fmt.Errorf("decode qBittorrent torrent list")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("decode qBittorrent torrent list")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(jobs, func(i, j int) bool {
		left, right := jobs[i], jobs[j]
		if left.Hash != right.Hash {
			return left.Hash < right.Hash
		}
		if left.InfoHashV1 != right.InfoHashV1 {
			return left.InfoHashV1 < right.InfoHashV1
		}
		if left.InfoHashV2 != right.InfoHashV2 {
			return left.InfoHashV2 < right.InfoHashV2
		}
		if left.ContentPath != right.ContentPath {
			return left.ContentPath < right.ContentPath
		}
		if left.SavePath != right.SavePath {
			return left.SavePath < right.SavePath
		}
		return left.Name < right.Name
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return jobs, nil
}

func normalizeTorrent(index int, item rawTorrent) (downloader.Torrent, error) {
	if item.Hash == "" || len(item.Hash) > maxOpaqueJobKeyBytes || hasUnsafeJobKeyByte(item.Hash) {
		return downloader.Torrent{}, fmt.Errorf("qBittorrent torrent %d has an invalid opaque job key", index)
	}
	if item.Name == "" || len(item.Name) > maxJobNameBytes || !validControlSafeUTF8(item.Name) {
		return downloader.Torrent{}, fmt.Errorf("qBittorrent torrent %d has an invalid name", index)
	}
	if len(item.State) > maxJobStateBytes || !validControlSafeUTF8(item.State) {
		return downloader.Torrent{}, fmt.Errorf("qBittorrent torrent %d has an invalid state", index)
	}
	if len(item.SavePath) > maxJobPathBytes || !validControlSafeUTF8(item.SavePath) {
		return downloader.Torrent{}, fmt.Errorf("qBittorrent torrent %d has an invalid save path", index)
	}
	if len(item.ContentPath) > maxJobPathBytes || !validControlSafeUTF8(item.ContentPath) {
		return downloader.Torrent{}, fmt.Errorf("qBittorrent torrent %d has an invalid content path", index)
	}
	if item.Size < 0 || item.Downloaded < 0 || item.Uploaded < 0 {
		return downloader.Torrent{}, fmt.Errorf("qBittorrent torrent %d has invalid byte counters", index)
	}
	if math.IsNaN(item.Progress) || math.IsInf(item.Progress, 0) || item.Progress < 0 || item.Progress > 1 {
		return downloader.Torrent{}, fmt.Errorf("qBittorrent torrent %d has invalid progress", index)
	}
	identity := parseMagnetIdentity(item.MagnetURI)
	state := normalizedTorrentState(item.State)
	return downloader.Torrent{
		Hash:             item.Hash,
		InfoHashV1:       identity.v1,
		InfoHashV2:       identity.v2,
		IdentityStatus:   identity.status,
		IdentityEvidence: append([]string{}, identity.evidence...),
		IdentityIssues:   append([]string{}, identity.issues...),
		Name:             item.Name,
		SizeBytes:        item.Size,
		Progress:         item.Progress,
		State:            state,
		SavePath:         item.SavePath,
		ContentPath:      item.ContentPath,
		Downloaded:       item.Downloaded,
		Uploaded:         item.Uploaded,
	}, nil
}

func hasUnsafeJobKeyByte(value string) bool {
	for _, b := range []byte(value) {
		if b < 0x21 || b > 0x7e {
			return true
		}
	}
	return false
}

func parseMagnetIdentity(raw string) magnetIdentity {
	result := magnetIdentity{
		status:   downloader.IdentityStatusUnavailable,
		evidence: []string{},
		issues:   []string{},
	}
	if raw == "" {
		return result
	}
	if len(raw) > maxMagnetBytes {
		return invalidMagnetIdentity(issueMagnetTooLarge)
	}
	const prefix = "magnet:?"
	if len(raw) < len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) || strings.Contains(raw[len(prefix):], "#") {
		return invalidMagnetIdentity(issueMagnetFormat)
	}
	query := raw[len(prefix):]
	if !validRawMagnetQuery(query) {
		return invalidMagnetIdentity(issueMagnetFormat)
	}
	pairs := 0
	if query != "" {
		pairs = 1
		for index := 0; index < len(query); index++ {
			if query[index] == '&' {
				pairs++
			}
		}
	}
	if pairs > maxMagnetPairs {
		return invalidMagnetIdentity(issueMagnetTooManyPairs)
	}
	xtCount := 0
	for len(query) > 0 {
		pair := query
		if separator := strings.IndexByte(query, '&'); separator >= 0 {
			pair = query[:separator]
			query = query[separator+1:]
		} else {
			query = ""
		}
		key, value, _ := strings.Cut(pair, "=")
		isXT, safeKey := classifyMagnetQueryKey(key)
		if !safeKey {
			return invalidMagnetIdentity(issueMagnetFormat)
		}
		if !isXT {
			continue
		}
		xtCount++
		if xtCount > maxMagnetXT {
			return invalidMagnetIdentity(issueMagnetTooManyXT)
		}
		if len(value) > maxMagnetXTBytes {
			result.addIssue(issueMagnetXTTooLarge)
			continue
		}
		decoded, err := url.QueryUnescape(value)
		if err != nil {
			result.addIssue(issueMagnetXTEscape)
			continue
		}
		if len(decoded) > maxMagnetXTBytes {
			result.addIssue(issueMagnetXTTooLarge)
			continue
		}
		if !validDecodedXT(decoded) {
			result.addIssue(issueMagnetXTUnsafe)
			continue
		}
		result.observeXT(decoded)
	}
	if len(result.issues) > 0 {
		result.status = downloader.IdentityStatusInvalid
		result.v1 = ""
		result.v2 = ""
		result.evidence = []string{}
		sort.Strings(result.issues)
		return result
	}
	if result.v1 != "" || result.v2 != "" {
		result.status = downloader.IdentityStatusValid
		sort.Strings(result.evidence)
	}
	return result
}

func classifyMagnetQueryKey(raw string) (bool, bool) {
	if raw == "" || len(raw) > 256 {
		return false, false
	}
	decoded, err := url.QueryUnescape(raw)
	if err != nil || decoded == "" || len(decoded) > 256 {
		return false, false
	}
	for index := 0; index < len(decoded); index++ {
		value := decoded[index]
		if value < 0x21 || value > 0x7e || value == '&' || value == '=' || value == ';' {
			return false, false
		}
	}
	return strings.EqualFold(decoded, "xt"), true
}

func validRawMagnetQuery(raw string) bool {
	if raw != "" && (raw[0] == '&' || raw[len(raw)-1] == '&' || strings.Contains(raw, "&&")) {
		return false
	}
	for index := 0; index < len(raw); index++ {
		value := raw[index]
		if value < 0x21 || value > 0x7e || value == ';' {
			return false
		}
		if value == '%' {
			if index+2 >= len(raw) || !isHexByte(raw[index+1]) || !isHexByte(raw[index+2]) {
				return false
			}
			index += 2
		}
	}
	return true
}

func validDecodedXT(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e || value[index] == ';' {
			return false
		}
	}
	return true
}

func normalizedTorrentState(value string) string {
	switch value {
	case "error", "missingFiles", "uploading", "pausedUP", "queuedUP", "stalledUP", "checkingUP", "forcedUP", "stoppedUP",
		"allocating", "downloading", "metaDL", "forcedMetaDL", "pausedDL", "queuedDL", "stalledDL", "checkingDL", "forcedDL", "stoppedDL",
		"checkingResumeData", "moving", "unknown":
		return value
	default:
		return "unknown"
	}
}

func isHexByte(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func invalidMagnetIdentity(code string) magnetIdentity {
	return magnetIdentity{status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{code}}
}

func (result *magnetIdentity) observeXT(value string) {
	lower := strings.ToLower(value)
	const btihPrefix = "urn:btih:"
	const btmhPrefix = "urn:btmh:"
	switch {
	case strings.HasPrefix(lower, btihPrefix):
		payload := value[len(btihPrefix):]
		var digest []byte
		var evidence string
		switch len(payload) {
		case 40:
			decoded, err := hex.DecodeString(payload)
			if err != nil || len(decoded) != 20 {
				result.addIssue(issueMagnetBTIH)
				return
			}
			digest = decoded
			evidence = evidenceMagnetBTIHHex
		case 32:
			decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(payload))
			if err != nil || len(decoded) != 20 {
				result.addIssue(issueMagnetBTIH)
				return
			}
			digest = decoded
			evidence = evidenceMagnetBTIHBase32
		default:
			result.addIssue(issueMagnetBTIH)
			return
		}
		result.setV1(hex.EncodeToString(digest), evidence)
	case strings.HasPrefix(lower, btmhPrefix):
		payload := value[len(btmhPrefix):]
		if len(payload) != 68 || !strings.EqualFold(payload[:4], "1220") {
			result.addIssue(issueMagnetBTMH)
			return
		}
		digest, err := hex.DecodeString(payload[4:])
		if err != nil || len(digest) != 32 {
			result.addIssue(issueMagnetBTMH)
			return
		}
		result.setV2(hex.EncodeToString(digest), evidenceMagnetBTMHSHA256)
	}
}

func (result *magnetIdentity) setV1(value, evidence string) {
	if result.v1 != "" && result.v1 != value {
		result.addIssue(issueMagnetBTIHConflict)
		return
	}
	result.v1 = value
	result.addEvidence(evidence)
}

func (result *magnetIdentity) setV2(value, evidence string) {
	if result.v2 != "" && result.v2 != value {
		result.addIssue(issueMagnetBTMHConflict)
		return
	}
	result.v2 = value
	result.addEvidence(evidence)
}

func (result *magnetIdentity) addEvidence(value string) {
	for _, existing := range result.evidence {
		if existing == value {
			return
		}
	}
	result.evidence = append(result.evidence, value)
}

func (result *magnetIdentity) addIssue(value string) {
	for _, existing := range result.issues {
		if existing == value {
			return
		}
	}
	result.issues = append(result.issues, value)
}

func (a *Adapter) resolve(path string) *url.URL {
	return a.base.ResolveReference(&url.URL{Path: path})
}

func origin(u *url.URL) string { return u.Scheme + "://" + u.Host }

func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
