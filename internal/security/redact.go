package security

import (
	"net/url"
	"regexp"
	"strings"
)

const redacted = "[REDACTED]"

var (
	headerSecret = regexp.MustCompile(`(?i)(authorization|proxy-authorization|cookie|set-cookie|x-api-key)\s*[:=]\s*[^\r\n]+`)
	// jsonSecret replaces the whole quoted string or string array of a quoted
	// secret-bearing key, including escaped quotes inside the value. Generic
	// words such as token and secret are only treated as keys when quoted.
	jsonSecret   = regexp.MustCompile(`(?i)("(?:passkey|authkey|torrent_pass|rsskey|token|access_token|refresh_token|api[_-]?token|secret|client_secret|c_secure_pass|sid|password|passwd|cookie|set-cookie|authorization|proxy-authorization|x-api-key|api[_-]?key)"\s*:\s*)("(?:[^"\\]|\\.)*"|\[[^\]]*\])`)
	querySecret  = regexp.MustCompile(`(?i)(passkey|pass%6bey|authkey|auth%6bey|torrent_pass|torrent%5fpass|rsskey|rss%6bey|sid|token|api[_-]?key|password|passwd|cookie|c_secure_pass)(=|%3d)([^&\s]+)`)
	fieldSecret  = regexp.MustCompile(`(?i)((?:["']?(?:password|passwd|cookie|sid|authorization|proxy-authorization|x-api-key|api[_-]?key|passkey|authkey|torrent_pass|rsskey|access_token|refresh_token|c_secure_pass)["']?)\s*[:=]\s*["']?)([^"'\s,}&]+)`)
	bearerSecret = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/-]{8,}=*`)
	announceURL  = regexp.MustCompile(`(?i)https?://[^\s"']+/(announce|download\.php)(\?[^\s"']*)?`)
)

// Redact is a final safety net for diagnostics. Sensitive values should not be
// formatted in the first place; this function protects error boundaries.
func Redact(s string) string {
	s = headerSecret.ReplaceAllStringFunc(s, func(match string) string {
		if i := strings.IndexAny(match, ":="); i >= 0 {
			return match[:i+1] + " " + redacted
		}
		return redacted
	})
	s = jsonSecret.ReplaceAllString(s, `$1"`+redacted+`"`)
	s = querySecret.ReplaceAllString(s, "$1$2"+redacted)
	s = fieldSecret.ReplaceAllString(s, "$1"+redacted)
	s = bearerSecret.ReplaceAllString(s, "$1 "+redacted)
	s = announceURL.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return redacted
		}
		return u.Scheme + "://" + u.Host + "/" + redacted
	})
	return s
}

func RedactedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return redacted
	}
	if u.User != nil {
		u.User = url.User(redacted)
	}
	q := u.Query()
	for key := range q {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "key") || strings.Contains(lower, "token") || strings.Contains(lower, "pass") || lower == "cookie" {
			q.Set(key, redacted)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
