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
	titlePattern    = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	usernameInTitle = regexp.MustCompile(`(?i)(PT\s*::\s*)?(.+?)\s*的魔力值`)
	balancePattern  = regexp.MustCompile(`(?i)(当前\s*)?(魔力值|bonus)[^0-9]{0,32}([0-9][0-9,]*(\.[0-9]+)?)`)
	rowPattern      = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr\s*>`)
	cellPattern     = regexp.MustCompile(`(?is)<t[dh]\b[^>]*>(.*?)</t[dh]\s*>`)
	detailsPattern  = regexp.MustCompile(`(?is)<a\b[^>]*href\s*=\s*["']?([^"' >]*details\.php\?[^"' >]+)["']?[^>]*>(.*?)</a\s*>`)
	idPattern       = regexp.MustCompile(`(^|[?&])id=([0-9]+)(&|$)`)
	tagPattern      = regexp.MustCompile(`(?is)<[^>]+>`)
	scriptPattern   = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>|<style\b[^>]*>.*?</style\s*>`)
	spacePattern    = regexp.MustCompile(`\s+`)
	sizePattern     = regexp.MustCompile(`(?i)([0-9]+(\.[0-9]+)?)\s*(KiB|MiB|GiB|TiB|KB|MB|GB|TB)\b`)
)

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
	if len(match) != 5 {
		return ""
	}
	return strings.ReplaceAll(match[3], ",", "")
}

func parseBonusRows(body []byte) []domain.BonusCatalogRow {
	rows := make([]domain.BonusCatalogRow, 0, 16)
	for _, match := range rowPattern.FindAllSubmatch(body, 200) {
		raw := string(match[1])
		lower := strings.ToLower(raw)
		if !strings.Contains(lower, "<form") && !strings.Contains(lower, "mybonusapps.php") {
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
		if size, ok := firstSize(cellsFromRow(string(row[1]))); ok {
			summary.SizeBytes = &size
		}
		results = append(results, summary)
	}
	return results
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
