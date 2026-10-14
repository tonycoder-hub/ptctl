package tjupt

import (
	"bytes"
	"errors"
	"html"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"

	"github.com/tonycoder-hub/ptctl/internal/domain"
)

// siteTimeZone is how TJUPT's zone-less "YYYY-MM-DD HH:MM:SS" timestamps are
// read: the site runs on China Standard Time, which has no daylight saving.
var siteTimeZone = time.FixedZone("UTC+8", 8*60*60)

const siteTimeLayout = "2006-01-02 15:04:05"

var (
	headerIcon     = regexp.MustCompile(`(?is)<img\b[^>]*\bclass\s*=\s*["']?(comments|time|size|seeders|leechers|snatched)["'\s/>]`)
	anchorHref     = regexp.MustCompile(`(?is)<a\b[^>]*\bhref\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	titledDateTime = regexp.MustCompile(`(?is)\btitle\s*=\s*["']([0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2})["']`)
	// TJUPT prints a promotion's traffic factors as an arrow icon followed by
	// the factor, for example <img alt='Upload Rate'>1.25X.
	uploadRate   = regexp.MustCompile(`(?is)<img\b[^>]*\balt\s*=\s*["']Upload Rate["'][^>]*>\s*([0-9]{1,3}(?:\.[0-9]{1,4})?)\s*X`)
	downloadRate = regexp.MustCompile(`(?is)<img\b[^>]*\balt\s*=\s*["']Download Rate["'][^>]*>\s*([0-9]{1,3}(?:\.[0-9]{1,4})?)\s*X`)
	fontClass    = regexp.MustCompile(`(?is)<font\b[^>]*\bclass\s*=\s*["']([a-z0-9_]+)["']`)
	iconClass    = regexp.MustCompile(`(?is)<img\b[^>]*\bclass\s*=\s*["'](pro_[a-z0-9_]+)["']`)
	rowClass     = regexp.MustCompile(`(?is)^\s*<tr\b[^>]*\bclass\s*=\s*["']([a-z0-9_ ]+)["']`)
	promotionEnd = regexp.MustCompile(`(?s)剩余时间\s*[:：]?\s*<span\b[^>]*\btitle\s*=\s*["']([0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2})["']`)
)

// legacyPromotions maps the promotion markers of TJUPT's NexusPHP-derived
// get_torrent_promotion_append and get_torrent_bg_color: the word-mode font
// class, the icon-mode image class, and the highlighted row class.
var legacyPromotions = map[string][2]string{
	"free":                 {"1", "0"},
	"twoup":                {"2", "1"},
	"twoupfree":            {"2", "0"},
	"halfdown":             {"1", "0.5"},
	"twouphalfdown":        {"2", "0.5"},
	"thirtypercent":        {"1", "0.3"},
	"pro_free":             {"1", "0"},
	"pro_2up":              {"2", "1"},
	"pro_free2up":          {"2", "0"},
	"pro_50pctdown":        {"1", "0.5"},
	"pro_50pctdown2up":     {"2", "0.5"},
	"pro_30pctdown":        {"1", "0.3"},
	"free_bg":              {"1", "0"},
	"twoup_bg":             {"2", "1"},
	"twoupfree_bg":         {"2", "0"},
	"halfdown_bg":          {"1", "0.5"},
	"twouphalfdown_bg":     {"2", "0.5"},
	"thirtypercentdown_bg": {"1", "0.3"},
}

// searchColumns maps the torrent table's header to top-level cell indices.
// TJUPT labels most columns with an icon class instead of text. A column the
// header does not identify stays -1, and its field stays absent.
type searchColumns struct {
	count                                                    int
	category, time, size, seeders, leechers, snatched, other int
}

func parseSearch(body []byte) []domain.TorrentSummary {
	results := make([]domain.TorrentSummary, 0, 32)
	seen := make(map[string]struct{})
	rows := searchRows(body, 500)
	columns, mapped := searchHeader(rows)
	for _, row := range rows {
		link := detailsPattern.FindSubmatch(row)
		if len(link) != 3 {
			continue
		}
		href := html.UnescapeString(string(link[1]))
		id := idPattern.FindStringSubmatch(href)
		// A ref must round-trip into site detail and metafile fetch, which
		// accept only canonical IDs.
		if len(id) != 4 || !canonicalPositiveID(id[2]) {
			continue
		}
		if _, exists := seen[id[2]]; exists {
			continue
		}
		name := fullTorrentName(link[0], plainText(string(link[2])))
		if name == "" {
			continue
		}
		seen[id[2]] = struct{}{}
		summary := domain.TorrentSummary{Ref: domain.TorrentRef{SiteID: SiteID, RemoteID: id[2]}, Name: name}
		cells := topLevelCells(row)
		nameCell := -1
		for index, cell := range cells {
			if detailsPattern.Match(cell) {
				nameCell = index
				break
			}
		}
		if mapped && len(cells) == columns.count && nameCell >= 0 {
			fillSearchColumns(&summary, row, cells, nameCell, columns)
		} else if nameCell >= 0 {
			// Without a recognized header, keep only the size: it is the one
			// later cell whose unit makes it self-describing.
			for _, cell := range cells[nameCell+1:] {
				if size, ok := firstSize([]string{plainText(string(cell))}); ok {
					summary.SizeBytes = &size
					break
				}
			}
		}
		results = append(results, summary)
	}
	return results
}

// fullTorrentName undoes torrenttable's truncation. A name longer than the
// list allows (105 to 150 characters, by font size and subtitle) is shown cut
// short with "..", while the link's title keeps the full name unless the user
// enabled name tooltips. The title replaces the shown name only when it
// extends exactly what was shown.
func fullTorrentName(anchor []byte, shown string) string {
	prefix := strings.TrimSpace(strings.TrimSuffix(shown, ".."))
	if prefix == shown || prefix == "" {
		return shown
	}
	title, ok := anchorTitle(anchor)
	if !ok || !strings.HasPrefix(title, prefix) {
		return shown
	}
	return title
}

// anchorTitle returns the opening <a> tag's sole title attribute, already
// unescaped, with whitespace collapsed as plainText does for shown text.
func anchorTitle(anchor []byte) (string, bool) {
	tokenizer := xhtml.NewTokenizer(bytes.NewReader(anchor))
	if tokenizer.Next() != xhtml.StartTagToken {
		return "", false
	}
	name, more := tokenizer.TagName()
	if string(name) != "a" {
		return "", false
	}
	title, found := "", false
	for more {
		var key, value []byte
		key, value, more = tokenizer.TagAttr()
		if string(key) != "title" {
			continue
		}
		if found {
			return "", false
		}
		title, found = string(value), true
	}
	title = strings.TrimSpace(spacePattern.ReplaceAllString(strings.ReplaceAll(title, "\u00a0", " "), " "))
	return title, found && title != ""
}

func fillSearchColumns(summary *domain.TorrentSummary, row []byte, cells [][]byte, nameCell int, columns searchColumns) {
	if columns.category >= 0 {
		summary.Category = categoryID(cells[columns.category])
	}
	if columns.time >= 0 {
		summary.PublishedAt = cellTime(cells[columns.time])
	}
	if columns.size >= 0 {
		if size, ok := firstSize([]string{plainText(string(cells[columns.size]))}); ok {
			summary.SizeBytes = &size
		}
	}
	if columns.seeders >= 0 {
		summary.Seeders = siteCount(plainText(string(cells[columns.seeders])))
	}
	if columns.leechers >= 0 {
		summary.Leechers = siteCount(plainText(string(cells[columns.leechers])))
	}
	if columns.snatched >= 0 {
		summary.Snatches = siteCount(plainText(string(cells[columns.snatched])))
	}
	summary.Promotion = searchPromotion(row, cells[nameCell])
}

// searchHeader finds the torrent table's header row and maps its columns.
// Every icon must appear at most once; an ambiguous header maps nothing.
func searchHeader(rows [][]byte) (searchColumns, bool) {
	for _, row := range rows {
		if !bytes.Contains(row, []byte("colhead")) {
			continue
		}
		cells := topLevelCells(row)
		columns := searchColumns{count: len(cells), category: -1, time: -1, size: -1, seeders: -1, leechers: -1, snatched: -1, other: -1}
		for index, cell := range cells {
			text := plainText(string(cell))
			matches := headerIcon.FindAllSubmatch(cell, 2)
			if len(matches) > 1 {
				return searchColumns{}, false
			}
			target := &columns.other
			switch {
			case len(matches) == 1:
				switch string(matches[0][1]) {
				case "time":
					target = &columns.time
				case "size":
					target = &columns.size
				case "seeders":
					target = &columns.seeders
				case "leechers":
					target = &columns.leechers
				case "snatched":
					target = &columns.snatched
				}
			case text == "类型" || text == "類型":
				target = &columns.category
			}
			if target == &columns.other {
				continue
			}
			if *target != -1 {
				return searchColumns{}, false
			}
			*target = index
		}
		return columns, columns.count > 0
	}
	return searchColumns{}, false
}

// topLevelCells returns the markup inside each cell of one table row. Cells of
// tables nested inside a cell, such as TJUPT's torrentname table, stay part of
// their enclosing cell. A tokenizer error returns no cells.
func topLevelCells(row []byte) [][]byte {
	tokenizer := xhtml.NewTokenizer(bytes.NewReader(row))
	cells := make([][]byte, 0, 12)
	offset, start, tables := 0, -1, 0
	closeCell := func(end int) {
		if start >= 0 {
			cells = append(cells, row[start:end])
			start = -1
		}
	}
	for len(cells) < 64 {
		tokenType := tokenizer.Next()
		if tokenType == xhtml.ErrorToken {
			if !errors.Is(tokenizer.Err(), io.EOF) {
				return nil
			}
			closeCell(offset)
			break
		}
		begin := offset
		offset += len(tokenizer.Raw())
		if tokenType != xhtml.StartTagToken && tokenType != xhtml.EndTagToken {
			continue
		}
		name, _ := tokenizer.TagName()
		switch string(name) {
		case "table":
			if tokenType == xhtml.StartTagToken {
				tables++
			} else if tables > 0 {
				tables--
			}
		case "td", "th":
			if tables != 0 {
				continue
			}
			closeCell(begin)
			if tokenType == xhtml.StartTagToken {
				start = offset
			}
		case "tr":
			if tables == 0 && tokenType == xhtml.EndTagToken {
				closeCell(begin)
			}
		}
	}
	return cells
}

// categoryID returns the canonical category ID of the category cell's
// filter link, such as cat=408. The site's display name is not retained.
func categoryID(cell []byte) string {
	match := anchorHref.FindSubmatch(cell)
	if len(match) != 3 {
		return ""
	}
	raw := string(match[1])
	if raw == "" {
		raw = string(match[2])
	}
	parsed, err := url.Parse(html.UnescapeString(raw))
	if err != nil {
		return ""
	}
	values := parsed.Query()["cat"]
	if len(values) != 1 || !canonicalPositiveID(values[0]) {
		return ""
	}
	return values[0]
}

// cellTime reads the publication time cell, which shows either the absolute
// time or, depending on the user's settings, a relative time whose title
// attribute carries the absolute one.
func cellTime(cell []byte) *time.Time {
	if value := siteTime(plainText(string(cell))); value != nil {
		return value
	}
	matches := titledDateTime.FindAllSubmatch(cell, 2)
	if len(matches) != 1 {
		return nil
	}
	return siteTime(string(matches[0][1]))
}

func siteTime(value string) *time.Time {
	if len(value) != len(siteTimeLayout) {
		return nil
	}
	parsed, err := time.ParseInLocation(siteTimeLayout, value, siteTimeZone)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

// searchPromotion reads one result's traffic factors. Rows without any
// promotion marker are normal traffic. Markers that disagree, or a promotion
// label whose factors are not shown, yield no claim rather than a guess.
func searchPromotion(row, nameCell []byte) *domain.Promotion {
	var claims [][2]string
	unexplained := false
	if match := rowClass.FindSubmatch(row); len(match) == 2 {
		for _, class := range strings.Fields(strings.ToLower(string(match[1]))) {
			if pair, ok := legacyPromotions[class]; ok && strings.HasSuffix(class, "_bg") {
				claims = append(claims, pair)
			}
		}
	}
	for _, match := range fontClass.FindAllSubmatch(nameCell, 8) {
		class := strings.ToLower(string(match[1]))
		if pair, ok := legacyPromotions[class]; ok && !strings.HasPrefix(class, "pro_") && !strings.HasSuffix(class, "_bg") {
			claims = append(claims, pair)
		} else if strings.HasSuffix(class, "_promotion") {
			unexplained = true
		}
	}
	for _, match := range iconClass.FindAllSubmatch(nameCell, 8) {
		class := strings.ToLower(string(match[1]))
		if pair, ok := legacyPromotions[class]; ok {
			claims = append(claims, pair)
		} else {
			unexplained = true
		}
	}
	up, upOK := promotionFactor(uploadRate, nameCell)
	down, downOK := promotionFactor(downloadRate, nameCell)
	if !upOK || !downOK {
		return nil
	}
	if up != "" || down != "" {
		if up == "" {
			up = "1"
		}
		if down == "" {
			down = "1"
		}
		claims = append(claims, [2]string{up, down})
		unexplained = false
	}
	if unexplained {
		return nil
	}
	if len(claims) == 0 {
		return &domain.Promotion{UploadFactor: "1", DownloadFactor: "1"}
	}
	for _, claim := range claims[1:] {
		if claim != claims[0] {
			return nil
		}
	}
	result := &domain.Promotion{UploadFactor: claims[0][0], DownloadFactor: claims[0][1]}
	if claims[0] != [2]string{"1", "1"} {
		ends := promotionEnd.FindAllSubmatch(nameCell, 3)
		if len(ends) == 1 {
			result.EndsAt = siteTime(string(ends[0][1]))
		}
	}
	return result
}

// promotionFactor returns the canonical factor a pattern shows, "" when the
// cell shows none, and false when the cell shows conflicting or invalid ones.
func promotionFactor(pattern *regexp.Regexp, cell []byte) (string, bool) {
	factor := ""
	for _, match := range pattern.FindAllSubmatch(cell, 4) {
		value := canonicalFactor(string(match[1]))
		if value == "" || factor != "" && value != factor {
			return "", false
		}
		factor = value
	}
	return factor, true
}

func canonicalFactor(value string) string {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < 0 || parsed > 100 {
		return ""
	}
	return strconv.FormatFloat(parsed, 'f', -1, 64)
}

// siteCount reads a non-negative count, allowing NexusPHP's thousands
// separators.
func siteCount(text string) *int {
	value := canonicalBonusBalance(text)
	if value == "" || strings.Contains(value, ".") || len(value) > 9 {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return nil
	}
	return &parsed
}

func canonicalPositiveID(value string) bool {
	if value == "" || len(value) > 19 || value[0] == '0' {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
