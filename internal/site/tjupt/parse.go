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

var (
	loginUserField  = regexp.MustCompile(`(?is)name\s*=\s*["']?username["']?`)
	loginPassField  = regexp.MustCompile(`(?is)name\s*=\s*["']?password["']?`)
	challengeMarker = regexp.MustCompile(`(?is)(cf-chl-|just a moment|attention required|captcha|cloudflare ray id)`)
	searchField     = regexp.MustCompile(`(?is)<input\b[^>]*name\s*=\s*["']?search["']?`)
	authenticatedUI = regexp.MustCompile(`(?is)<a\b[^>]*href\s*=\s*["']?[^"' >]*logout\.php(?:[?"' >])`)
	emptySearch     = regexp.MustCompile(`(?is)(没有找到(?:任何)?种子|没有符合条件的种子|no torrents? found|nothing found)`)
	bonusForm       = regexp.MustCompile(`(?is)<form\b[^>]*action\s*=\s*["']?[^"' >]*mybonusapps\.php[^"' >]*["']?[^>]*>`)
	bonusOption     = regexp.MustCompile(`(?is)<input\b[^>]*name\s*=\s*["']?option["']?`)
	titlePattern    = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	usernameInTitle = regexp.MustCompile(`(?i)(PT\s*::\s*)?(.+?)\s*的魔力值`)
	balancePattern  = regexp.MustCompile(`当前\s*魔力值[^0-9]{0,32}([0-9][0-9,]*(\.[0-9]+)?)`)
	rowPattern      = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr\s*>`)
	cellPattern     = regexp.MustCompile(`(?is)<t[dh]\b[^>]*>(.*?)</t[dh]\s*>`)
	detailsPattern  = regexp.MustCompile(`(?is)<a\b[^>]*href\s*=\s*["']?([^"' >]*details\.php\?[^"' >]+)["']?[^>]*>(.*?)</a\s*>`)
	idPattern       = regexp.MustCompile(`(^|[?&])id=([0-9]+)(&|$)`)
	tagPattern      = regexp.MustCompile(`(?is)<[^>]+>`)
	scriptPattern   = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>|<style\b[^>]*>.*?</style\s*>`)
	spacePattern    = regexp.MustCompile(`\s+`)
	sizePattern     = regexp.MustCompile(`(?i)([0-9]+(\.[0-9]+)?)\s*(KiB|MiB|GiB|TiB|KB|MB|GB|TB)\b`)
)

func classifyBonusPage(finalURL *url.URL, body []byte) (domain.AuthenticationState, string) {
	if isLoginPage(finalURL, body) {
		return domain.AuthenticationUnauthenticated, ""
	}
	if challengeMarker.Match(body) {
		return domain.AuthenticationIndeterminate, ""
	}
	if finalURL == nil || !strings.HasSuffix(strings.ToLower(finalURL.Path), "/mybonusapps.php") || finalURL.RawQuery != "" || !utf8.Valid(body) {
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
	match := balancePattern.FindStringSubmatchIndex(text)
	if len(match) != 6 || match[2] < 0 || match[3] < match[2] {
		return ""
	}
	if match[3] < len(text) {
		next := text[match[3]]
		if (next >= '0' && next <= '9') || next == ',' || next == '.' {
			return ""
		}
	}
	return canonicalBonusBalance(text[match[2]:match[3]])
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

func parseBonusRows(body []byte) []domain.BonusCatalogRow {
	rows := make([]domain.BonusCatalogRow, 0, 16)
	for _, match := range rowPattern.FindAllSubmatch(body, 200) {
		raw := string(match[1])
		if !bonusForm.MatchString(raw) || !bonusOption.MatchString(raw) {
			continue
		}
		cells := cellsFromRow(raw)
		if len(cells) < 2 {
			continue
		}
		rows = append(rows, domain.BonusCatalogRow{Selector: bonusSelectorFromRow(raw), Columns: cells})
		if len(rows) >= 100 {
			break
		}
	}
	return rows
}

func bonusSelectorFromRow(raw string) string {
	tokenizer := xhtml.NewTokenizer(bytes.NewBufferString(raw))
	selector := ""
	count := 0
	complete := false
	for tokens := 0; tokens < 512; tokens++ {
		tokenType := tokenizer.Next()
		if tokenType == xhtml.ErrorToken {
			if errors.Is(tokenizer.Err(), io.EOF) {
				complete = true
				break
			}
			return ""
		}
		if tokenType != xhtml.StartTagToken && tokenType != xhtml.SelfClosingTagToken {
			continue
		}
		token := tokenizer.Token()
		if !strings.EqualFold(token.Data, "input") {
			continue
		}
		attrs, err := strictBonusAttributes(token.Attr)
		if err != nil || attrs["name"] != "option" {
			if err != nil {
				return ""
			}
			continue
		}
		count++
		if strings.ToLower(attrs["type"]) != "hidden" || validateBonusSelector(attrs["value"]) != nil {
			return ""
		}
		selector = attrs["value"]
	}
	if !complete || count != 1 {
		return ""
	}
	return selector
}

func parseSearch(body []byte) []domain.TorrentSummary {
	results := make([]domain.TorrentSummary, 0, 32)
	seen := make(map[string]struct{})
	for _, row := range rowPattern.FindAllSubmatch(body, 500) {
		link := detailsPattern.FindSubmatch(row[1])
		if len(link) != 3 {
			continue
		}
		href := html.UnescapeString(string(link[1]))
		id := idPattern.FindStringSubmatch(href)
		if len(id) != 4 {
			continue
		}
		if _, exists := seen[id[2]]; exists {
			continue
		}
		name := plainText(string(link[2]))
		if name == "" {
			continue
		}
		seen[id[2]] = struct{}{}
		summary := domain.TorrentSummary{Ref: domain.TorrentRef{SiteID: "tjupt", RemoteID: id[2]}, Name: name}
		if size, ok := sizeAfterDetailsCell(string(row[1])); ok {
			summary.SizeBytes = &size
		}
		results = append(results, summary)
	}
	return results
}

func sizeAfterDetailsCell(raw string) (int64, bool) {
	cells := cellPattern.FindAllStringSubmatch(raw, 64)
	detailsCell := -1
	for i, cell := range cells {
		if detailsPattern.MatchString(cell[1]) {
			detailsCell = i
			break
		}
	}
	if detailsCell < 0 {
		return 0, false
	}
	for _, cell := range cells[detailsCell+1:] {
		if value, ok := firstSize([]string{plainText(cell[1])}); ok {
			return value, true
		}
	}
	return 0, false
}

func cellsFromRow(raw string) []string {
	matches := cellPattern.FindAllStringSubmatch(raw, 64)
	cells := make([]string, 0, len(matches))
	for _, match := range matches {
		text := plainText(match[1])
		if text != "" {
			cells = append(cells, text)
		}
	}
	return cells
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
		base := float64(1000)
		if strings.Contains(unit, "IB") {
			base = 1024
		}
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
		}
		bytes := value * math.Pow(base, float64(power))
		if bytes >= 0 && bytes <= math.MaxInt64 {
			return int64(math.Round(bytes)), true
		}
	}
	return 0, false
}
