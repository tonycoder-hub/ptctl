package security

import (
	"net/url"
	"regexp"
	"strings"
)

const redacted = "[REDACTED]"

var (
	headerSecret = regexp.MustCompile(`(?i)(authorization|proxy-authorization|cookie|set-cookie)\s*[:=]\s*[^\r\n]+`)
	querySecret  = regexp.MustCompile(`(?i)(passkey|pass%6bey|authkey|auth%6bey|torrent_pass|rsskey|rss%6bey|sid|token|api[_-]?key|password|passwd|cookie)=([^&\s]+)`)
	fieldSecret  = regexp.MustCompile(`(?i)((?:["']?(?:password|passwd|cookie|sid|authorization|proxy-authorization)["']?)\s*[:=]\s*["']?)([^"'\s,}&]+)`)
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
	s = querySecret.ReplaceAllString(s, "$1="+redacted)
	s = fieldSecret.ReplaceAllString(s, "$1"+redacted)
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
