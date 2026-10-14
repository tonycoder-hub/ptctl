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
	"golang.org/x/net/html/atom"

	"github.com/tonycoder-hub/ptctl/internal/domain"
)

var (
	loginUserField  = regexp.MustCompile(`(?is)name\s*=\s*["']?username["']?`)
	loginPassField  = regexp.MustCompile(`(?is)name\s*=\s*["']?password["']?`)
	challengeMarker = regexp.MustCompile(`(?is)(cf-chl-|just a moment|attention required|captcha|cloudflare ray id)`)
	searchField     = regexp.MustCompile(`(?is)<input\b[^>]*name\s*=\s*["']?search["']?`)
	authenticatedUI = regexp.MustCompile(`(?is)<a\b[^>]*href\s*=\s*["']?[^"' >]*logout\.php(?:[?"' >])`)
	// The first three phrases are TJUPT's own torrents.php wording: a keyword
	// search with no hits, and a listing with nothing in it.
	emptySearch = regexp.MustCompile(`(?is)(没有种子。请用准确的关键字重试|没有结果！|没有种子:\(|没有找到(?:任何)?种子|没有符合条件的种子|no torrents? found|nothing found)`)
	// TJUPT's exchange forms post to "?action=exchange" on the same page.
	bonusForm    = regexp.MustCompile(`(?is)<form\b[^>]*action\s*=\s*["']?(?:[^"' >]*mybonusapps\.php[^"' >]*|\?action=exchange)(?:["'\s][^>]*)?/?>`)
	bonusOption  = regexp.MustCompile(`(?is)<input\b[^>]*name\s*=\s*["']?option["']?`)
	titlePattern = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	rowPattern   = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr\s*>`)
	cellPattern  = regexp.MustCompile(`(?is)<t[dh]\b[^>]*>(.*?)</t[dh]\s*>`)
	// details.php must be the whole file name: every page header links the
	// signed-in user as "userdetails.php?id=N".
	detailsLink   = regexp.MustCompile(`(?is)<a\b[^>]*\shref\s*=\s*["']?(?:[^"' >]*/)?details\.php\?`)
	tagPattern    = regexp.MustCompile(`(?is)<[^>]+>`)
	scriptPattern = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>|<style\b[^>]*>.*?</style\s*>`)
	spacePattern  = regexp.MustCompile(`\s+`)
	// NexusPHP prints sizes with 1024 divisors whether it labels them "GB"
	// (2016 templates) or "GiB" (2019 templates).
	sizePattern = regexp.MustCompile(`(?i)^([0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)(\.[0-9]+)?\s*(KB|KiB|MB|MiB|GB|GiB|TB|TiB|PB|PiB)$`)
)

// balancePatterns are the two ways a bonus page states the balance: the
// mybonusapps.php banner "用你的魔力值（当前12,345.6）来玩儿吧！" and the
// "当前魔力值：12,345.6" wording used elsewhere in NexusPHP.
var balancePatterns = []*regexp.Regexp{
	regexp.MustCompile(`魔力值\s*[（(]\s*当前\s*([0-9][0-9,]*(\.[0-9]+)?)`),
	regexp.MustCompile(`当前\s*魔力值[^0-9]{0,32}([0-9][0-9,]*(\.[0-9]+)?)`),
}

const (
	maxBalanceMatches = 16
	maxSearchResults  = 500
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
	if finalURL == nil || !strings.HasSuffix(strings.ToLower(finalURL.Path), "/torrents.php") || !utf8.Valid(body) {
		return domain.AuthenticationIndeterminate
	}
	lower := strings.ToLower(string(body))
	if searchField.Match(body) && authenticatedUI.Match(body) && strings.Contains(lower, "torrents.php") && (detailsLink.Match(body) || emptySearch.Match(body)) {
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

// parseBonusBalance returns the balance only when every statement of it on
// the page is well formed and they all agree.
func parseBonusBalance(body []byte) string {
	text := plainText(string(body))
	balance := ""
	matches := 0
	for _, pattern := range balancePatterns {
		for _, match := range pattern.FindAllStringSubmatchIndex(text, maxBalanceMatches) {
			matches++
			if matches >= maxBalanceMatches || len(match) != 6 || match[2] < 0 || match[3] < match[2] {
				return ""
			}
			if match[3] < len(text) {
				next := text[match[3]]
				if (next >= '0' && next <= '9') || next == ',' || next == '.' {
					return ""
				}
			}
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

// parseSearch reads torrents.php as a document because each TJUPT result row
// nests a "torrentname" table: the name, promotion tags and progress bar sit
// in inner rows, and size, seeders and the rest sit in the outer row.
func parseSearch(body []byte) []domain.TorrentSummary {
	document, err := xhtml.Parse(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	// When the page has a "torrents" listing, links elsewhere (news, sidebars)
	// are not results.
	listed := findElement(document, func(node *xhtml.Node) bool {
		return node.DataAtom == atom.Table && hasClass(node, "torrents")
	}) != nil
	results := make([]domain.TorrentSummary, 0, 32)
	seen := make(map[string]struct{})
	sizeColumns := make(map[*xhtml.Node]int)
	walkElements(document, func(node *xhtml.Node) bool {
		if len(results) >= maxSearchResults {
			return false
		}
		if node.DataAtom != atom.A {
			return true
		}
		id, ok := primaryDetailsID(attribute(node, "href"))
		if !ok {
			return true
		}
		if _, exists := seen[id]; exists {
			return true
		}
		row, table := torrentRow(node)
		if row == nil || (listed && !hasClass(table, "torrents")) {
			return true
		}
		name := torrentName(node)
		if name == "" {
			return true
		}
		seen[id] = struct{}{}
		summary := domain.TorrentSummary{Ref: domain.TorrentRef{SiteID: SiteID, RemoteID: id}, Name: name}
		column, known := sizeColumns[table]
		if !known {
			column = sizeColumn(table)
			sizeColumns[table] = column
		}
		if size, ok := rowSize(row, node, column); ok {
			summary.SizeBytes = &size
		}
		results = append(results, summary)
		return true
	})
	return results
}

// primaryDetailsID accepts the name link "details.php?id=N[&hit=1]" and
// skips the comment and peer-list links that point at the same page.
func primaryDetailsID(href string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(href))
	if err != nil || parsed.Fragment != "" {
		return "", false
	}
	if path := strings.ToLower(parsed.Path); path != "details.php" && !strings.HasSuffix(path, "/details.php") {
		return "", false
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || query.Has("cmtpage") || query.Has("dllist") || len(query["id"]) != 1 {
		return "", false
	}
	id := query.Get("id")
	if validateCanonicalTorrentRef(domain.TorrentRef{SiteID: SiteID, RemoteID: id}) != nil {
		return "", false
	}
	return id, true
}

// torrentRow returns the outermost-but-one row around link: the row of the
// listing table rather than a row of the nested "torrentname" table.
func torrentRow(link *xhtml.Node) (*xhtml.Node, *xhtml.Node) {
	var row *xhtml.Node
	for node := link.Parent; node != nil; node = node.Parent {
		switch node.DataAtom {
		case atom.Tr:
			if row == nil {
				row = node
			}
		case atom.Table:
			if hasClass(node, "torrentname") {
				row = nil
			} else if row != nil {
				return row, node
			}
		}
	}
	return nil, nil
}

// torrentName prefers the link title, which TJUPT fills with the full name
// when the visible text is cut to fit and ends in "..".
func torrentName(link *xhtml.Node) string {
	text := collapseSpace(nodeText(link))
	title := collapseSpace(attribute(link, "title"))
	if title == "" || text == "" || title == text {
		return text
	}
	if prefix := strings.TrimSpace(strings.TrimSuffix(text, "..")); prefix != text && prefix != "" && strings.HasPrefix(title, prefix) {
		return title
	}
	return text
}

const (
	noSizeColumn        = -1
	ambiguousSizeColumn = -2
)

// sizeColumn finds the "colhead" header cell carrying TJUPT's size icon.
func sizeColumn(table *xhtml.Node) int {
	column := noSizeColumn
	for _, row := range tableRows(table) {
		for index, cell := range rowCells(row) {
			if !hasClass(cell, "colhead") {
				continue
			}
			icon := findElement(cell, func(node *xhtml.Node) bool {
				return node.DataAtom == atom.Img && (hasClass(node, "size") || strings.EqualFold(attribute(node, "alt"), "size"))
			})
			if icon == nil {
				continue
			}
			if column != noSizeColumn {
				return ambiguousSizeColumn
			}
			column = index
		}
	}
	return column
}

// rowSize reads the size from the header's size column. Without a header it
// takes the first later cell whose whole text is a size; with an ambiguous
// header it reports no size.
func rowSize(row, link *xhtml.Node, column int) (int64, bool) {
	cells := rowCells(row)
	if column == ambiguousSizeColumn {
		return 0, false
	}
	if column >= 0 {
		if column >= len(cells) {
			return 0, false
		}
		return parseSize(collapseSpace(nodeText(cells[column])))
	}
	nameCell := -1
	for index, cell := range cells {
		if contains(cell, link) {
			nameCell = index
			break
		}
	}
	if nameCell < 0 {
		return 0, false
	}
	for _, cell := range cells[nameCell+1:] {
		if size, ok := parseSize(collapseSpace(nodeText(cell))); ok {
			return size, true
		}
	}
	return 0, false
}

func tableRows(table *xhtml.Node) []*xhtml.Node {
	var rows []*xhtml.Node
	for child := table.FirstChild; child != nil; child = child.NextSibling {
		switch child.DataAtom {
		case atom.Tr:
			rows = append(rows, child)
		case atom.Thead, atom.Tbody, atom.Tfoot:
			for row := child.FirstChild; row != nil; row = row.NextSibling {
				if row.DataAtom == atom.Tr {
					rows = append(rows, row)
				}
			}
		}
	}
	return rows
}

func rowCells(row *xhtml.Node) []*xhtml.Node {
	var cells []*xhtml.Node
	for child := row.FirstChild; child != nil; child = child.NextSibling {
		if child.DataAtom == atom.Td || child.DataAtom == atom.Th {
			cells = append(cells, child)
		}
	}
	return cells
}

// walkElements visits elements in document order without recursion, so a
// deeply nested page cannot exhaust the stack. visit returns false to stop.
func walkElements(root *xhtml.Node, visit func(*xhtml.Node) bool) {
	stack := []*xhtml.Node{root}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if node.Type == xhtml.ElementNode && !visit(node) {
			return
		}
		if node.Type == xhtml.ElementNode && (node.DataAtom == atom.Script || node.DataAtom == atom.Style) {
			continue
		}
		for child := node.LastChild; child != nil; child = child.PrevSibling {
			stack = append(stack, child)
		}
	}
}

func findElement(root *xhtml.Node, match func(*xhtml.Node) bool) *xhtml.Node {
	var found *xhtml.Node
	walkElements(root, func(node *xhtml.Node) bool {
		if match(node) {
			found = node
			return false
		}
		return true
	})
	return found
}

// nodeText joins the text under root with spaces, the way plainText turns
// tags into spaces, and leaves out script and style bodies.
func nodeText(root *xhtml.Node) string {
	var text strings.Builder
	stack := []*xhtml.Node{root}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch node.Type {
		case xhtml.TextNode:
			text.WriteString(node.Data)
			text.WriteByte(' ')
			continue
		case xhtml.ElementNode:
			if node.DataAtom == atom.Script || node.DataAtom == atom.Style {
				continue
			}
			text.WriteByte(' ')
		}
		for child := node.LastChild; child != nil; child = child.PrevSibling {
			stack = append(stack, child)
		}
	}
	return text.String()
}

func contains(root, target *xhtml.Node) bool {
	for node := target; node != nil; node = node.Parent {
		if node == root {
			return true
		}
	}
	return false
}

func attribute(node *xhtml.Node, name string) string {
	for _, attr := range node.Attr {
		if attr.Namespace == "" && strings.EqualFold(attr.Key, name) {
			return attr.Val
		}
	}
	return ""
}

func hasClass(node *xhtml.Node, class string) bool {
	for _, field := range strings.Fields(attribute(node, "class")) {
		if field == class {
			return true
		}
	}
	return false
}

func collapseSpace(value string) string {
	value = strings.ReplaceAll(value, "\u00a0", " ")
	return strings.TrimSpace(spacePattern.ReplaceAllString(value, " "))
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

// parseSize reads a whole cell such as "1.50 GiB" or "1,234.567 TB". The
// site rounds what it prints, so the result is the displayed value in bytes,
// not the exact torrent size.
func parseSize(text string) (int64, bool) {
	match := sizePattern.FindStringSubmatch(text)
	if len(match) != 4 {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.ReplaceAll(match[1], ",", "")+match[2], 64)
	if err != nil {
		return 0, false
	}
	power := strings.IndexByte("KMGTP", strings.ToUpper(match[3])[0]) + 1
	bytes := value * math.Pow(1024, float64(power))
	if bytes < 0 || bytes >= math.MaxInt64 {
		return 0, false
	}
	return int64(math.Round(bytes)), true
}
