package tjupt

import (
	"html"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"

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
	return strings.TrimSpace(title[:end])
}

func parseBonusBalance(body []byte) string {
	text := plainText(string(body))
	match := balancePattern.FindStringSubmatch(text)
	if len(match) != 3 {
		return ""
	}
	return strings.ReplaceAll(match[1], ",", "")
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
		rows = append(rows, domain.BonusCatalogRow{Columns: cells})
		if len(rows) >= 100 {
			break
		}
	}
	return rows
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
