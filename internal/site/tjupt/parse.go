package tjupt

import (
	"bytes"
	"errors"
	"html"
	"io"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	xhtml "golang.org/x/net/html"

	"github.com/tonycoder-hub/ptctl/internal/domain"
)

// bonusPagePath is TJUPT's bonus exchange page. mybonusapps.php is the
// unrelated games page (茉莉园), which also states the balance but offers no
// exchange forms.
const bonusPagePath = "mybonus.php"

var (
	loginUserField  = regexp.MustCompile(`(?is)name\s*=\s*["']?username["']?`)
	loginPassField  = regexp.MustCompile(`(?is)name\s*=\s*["']?password["']?`)
	challengeMarker = regexp.MustCompile(`(?is)(cf-chl-|just a moment|attention required|captcha|cloudflare ray id)`)
	searchField     = regexp.MustCompile(`(?is)<input\b[^>]*name\s*=\s*["']?search["']?`)
	authenticatedUI = regexp.MustCompile(`(?is)<a\b[^>]*href\s*=\s*["']?[^"' >]*logout\.php(?:[?"' >])`)
	// TJUPT torrents.php prints lang_torrents std_try_again for an empty
	// search and std_nothing_found when the search string is PHP-empty.
	emptySearch  = regexp.MustCompile(`(?is)(没有找到(?:任何)?种子|没有符合条件的种子|没有种子。请用准确的关键字重试|沒有種子。請用準確的關鍵字重試|没有结果！|沒有結果！|no torrents? found|nothing found)`)
	titlePattern = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	// TJUPT's live mybonus.php states the balance as "用你的魔力值（当前N）".
	// Both forms need their exact separator, so rule text on the same page such
	// as "如果当前魔力值≥1000" is never read as the balance.
	balancePatterns = []*regexp.Regexp{
		regexp.MustCompile(`魔力值\s*[（(]\s*当前\s*([0-9][0-9,]*(?:\.[0-9]+)?)\s*[）)]`),
		regexp.MustCompile(`当前\s*魔力值\s*[:：]\s*([0-9][0-9,]*(?:\.[0-9]+)?)`),
	}
	detailsPattern = regexp.MustCompile(`(?is)<a\b[^>]*href\s*=\s*["']?([^"' >]*details\.php\?[^"' >]+)["']?[^>]*>(.*?)</a\s*>`)
	idPattern      = regexp.MustCompile(`(^|[?&])id=([0-9]+)(&|$)`)
	tagPattern     = regexp.MustCompile(`(?is)<[^>]+>`)
	scriptPattern  = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>|<style\b[^>]*>.*?</style\s*>`)
	spacePattern   = regexp.MustCompile(`\s+`)
	sizePattern    = regexp.MustCompile(`(?i)([0-9]+(\.[0-9]+)?)\s*(KiB|MiB|GiB|TiB|PiB|KB|MB|GB|TB|PB)\b`)
)

func classifyBonusPage(finalURL *url.URL, body []byte) (domain.AuthenticationState, string) {
	if isLoginPage(finalURL, body) {
		return domain.AuthenticationUnauthenticated, ""
	}
	if challengeMarker.Match(body) {
		return domain.AuthenticationIndeterminate, ""
	}
	if finalURL == nil || !strings.HasSuffix(strings.ToLower(finalURL.Path), "/"+bonusPagePath) || finalURL.RawQuery != "" || !utf8.Valid(body) {
		return domain.AuthenticationIndeterminate, ""
	}
	username := parseUsername(body)
	if username == "" || parseBonusBalance(body) == "" {
		return domain.AuthenticationIndeterminate, ""
	}
	return domain.AuthenticationAuthenticated, username
}

func classifySearchPage(finalURL *url.URL, body []byte) domain.AuthenticationState {
	if isLoginPage(finalURL, body) {
		return domain.AuthenticationUnauthenticated
	}
	if challengeMarker.Match(body) {
		return domain.AuthenticationIndeterminate
	}
	if finalURL == nil || !strings.HasSuffix(strings.ToLower(finalURL.Path), "/torrents.php") {
		return domain.AuthenticationIndeterminate
	}
	lower := strings.ToLower(string(body))
	if searchField.Match(body) && authenticatedUI.Match(body) && strings.Contains(lower, "torrents.php") && (detailsPattern.Match(body) || emptySearch.Match(body)) {
		return domain.AuthenticationAuthenticated
	}
	return domain.AuthenticationIndeterminate
}

func isLoginPage(finalURL *url.URL, body []byte) bool {
	if finalURL != nil && strings.HasSuffix(strings.ToLower(finalURL.Path), "/login.php") {
		return true
	}
	return loginUserField.Match(body) && loginPassField.Match(body)
}

func parseUsername(body []byte) string {
	match := titlePattern.FindSubmatch(body)
	if len(match) != 2 {
		return ""
	}
	title := plainText(string(match[1]))
	if split := strings.LastIndex(title, "::"); split >= 0 {
		title = strings.TrimSpace(title[split+2:])
	}
	end := strings.Index(title, "的魔力值")
	if end < 0 {
		return ""
	}
	username := strings.TrimSpace(title[:end])
	if !validAccountText(username, 256) {
		return ""
	}
	return username
}

func parseBonusBalance(body []byte) string {
	text := plainText(string(body))
	balance := ""
	for _, pattern := range balancePatterns {
		for _, match := range pattern.FindAllStringSubmatchIndex(text, -1) {
			if len(match) != 4 || match[2] < 0 || match[3] < match[2] {
				return ""
			}
			if match[3] < len(text) {
				next := text[match[3]]
				if (next >= '0' && next <= '9') || next == ',' || next == '.' {
					return ""
				}
			}
			// Every statement of the balance must agree; a page that states
			// two different balances is not trusted for either.
			value := canonicalBonusBalance(text[match[2]:match[3]])
			if value == "" || (balance != "" && value != balance) {
				return ""
			}
			balance = value
		}
	}
	return balance
}

func validAccountText(value string, maximum int) bool {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, current := range value {
		if unicode.IsControl(current) || unicode.In(current, unicode.Cf, unicode.Cs) {
			return false
		}
	}
	return true
}

func canonicalBonusBalance(value string) string {
	if value == "" || len(value) > 128 {
		return ""
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return ""
	}
	integer := parts[0]
	if strings.Contains(integer, ",") {
		groups := strings.Split(integer, ",")
		if len(groups[0]) < 1 || len(groups[0]) > 3 || !decimalDigits(groups[0]) {
			return ""
		}
		for _, group := range groups[1:] {
			if len(group) != 3 || !decimalDigits(group) {
				return ""
			}
		}
	} else if !decimalDigits(integer) {
		return ""
	}
	integer = strings.ReplaceAll(integer, ",", "")
	if len(parts) == 1 {
		return integer
	}
	if parts[1] == "" || !decimalDigits(parts[1]) {
		return ""
	}
	return integer + "." + parts[1]
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, current := range value {
		if current < '0' || current > '9' {
			return false
		}
	}
	return true
}

// searchRows returns the complete markup of each row of a NexusPHP
// <table class="torrents">. TJUPT's torrenttable (include/functions.php)
// nests a <table class="torrentname"> inside the name cell, so a non-greedy
// <tr>...</tr> match would stop at that nested row and lose the later size
// cell. Rows of nested tables stay inside their enclosing row. A tokenizer
// error other than EOF returns no rows, which the caller treats as an
// unrecognized page.
func searchRows(body []byte, limit int) [][]byte {
	tokenizer := xhtml.NewTokenizer(bytes.NewReader(body))
	rows := make([][]byte, 0, 32)
	var torrentTables []bool
	offset, start, rowDepth := 0, -1, 0
	closeRow := func(end int) {
		if start >= 0 {
			rows = append(rows, body[start:end])
			start = -1
		}
	}
	for len(rows) < limit {
		tokenType := tokenizer.Next()
		if tokenType == xhtml.ErrorToken {
			if !errors.Is(tokenizer.Err(), io.EOF) {
				return nil
			}
			closeRow(offset)
			break
		}
		begin := offset
		offset += len(tokenizer.Raw())
		if tokenType != xhtml.StartTagToken && tokenType != xhtml.EndTagToken {
			continue
		}
		name, hasAttributes := tokenizer.TagName()
		switch {
		case tokenType == xhtml.StartTagToken && string(name) == "table":
			torrentTables = append(torrentTables, hasAttributes && tagHasClass(tokenizer, "torrents"))
		case tokenType == xhtml.StartTagToken && string(name) == "tr":
			depth := len(torrentTables)
			if depth > 0 && torrentTables[depth-1] && (start < 0 || rowDepth == depth) {
				closeRow(begin)
				start, rowDepth = begin, depth
			}
		case tokenType == xhtml.EndTagToken && string(name) == "tr":
			if start >= 0 && rowDepth == len(torrentTables) {
				closeRow(offset)
			}
		case tokenType == xhtml.EndTagToken && string(name) == "table":
			if len(torrentTables) > 0 {
				if start >= 0 && rowDepth == len(torrentTables) {
					closeRow(begin)
				}
				torrentTables = torrentTables[:len(torrentTables)-1]
			}
		}
	}
	return rows
}

func tagHasClass(tokenizer *xhtml.Tokenizer, class string) bool {
	for {
		key, value, more := tokenizer.TagAttr()
		if string(key) == "class" {
			for _, field := range strings.Fields(string(value)) {
				if field == class {
					return true
				}
			}
		}
		if !more {
			return false
		}
	}
}

func plainText(raw string) string {
	raw = scriptPattern.ReplaceAllString(raw, " ")
	raw = tagPattern.ReplaceAllString(raw, " ")
	raw = html.UnescapeString(raw)
	raw = strings.ReplaceAll(raw, "\u00a0", " ")
	return strings.TrimSpace(spacePattern.ReplaceAllString(raw, " "))
}

func firstSize(cells []string) (int64, bool) {
	for _, cell := range cells {
		match := sizePattern.FindStringSubmatch(cell)
		if len(match) != 4 {
			continue
		}
		value, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			continue
		}
		unit := strings.ToUpper(match[3])
		// NexusPHP's mksize/mksize_compact label 1024-based units KB, MB,
		// GB, TB, and PB, so both spellings use base 1024 here.
		base := float64(1024)
		power := 1
		switch unit[0] {
		case 'K':
			power = 1
		case 'M':
			power = 2
		case 'G':
			power = 3
		case 'T':
			power = 4
		case 'P':
			power = 5
		}
		bytes := value * math.Pow(base, float64(power))
		if bytes >= 0 && bytes <= math.MaxInt64 {
			return int64(math.Round(bytes)), true
		}
	}
	return 0, false
}
