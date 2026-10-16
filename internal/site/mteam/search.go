package mteam

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const (
	// maxSearchPage is the largest pageNumber the API accepts.
	maxSearchPage       = 1000
	maxSearchCategories = 32
	// freeDiscount asks the API for torrents whose download is not counted.
	freeDiscount = "FREE"
)

// searchModes are the API search areas a caller may pick. "normal" holds
// films, series, music, and the rest; "adult" holds the adult categories.
var searchModes = map[string]bool{"normal": true, "adult": true}

func (a *Adapter) Search(ctx context.Context, credential site.Credential, query string) ([]domain.TorrentSummary, error) {
	return a.SearchWithOptions(ctx, credential, query, site.SearchOptions{})
}

// ValidateSearchOptions accepts a page from 1 to 1000, the normal or adult
// mode, and at most 32 distinct category IDs.
func (a *Adapter) ValidateSearchOptions(options site.SearchOptions) error {
	if options.Page < 0 || options.Page > maxSearchPage {
		return fmt.Errorf("M-Team search page must be from 1 to %d", maxSearchPage)
	}
	if options.Mode != "" && !searchModes[options.Mode] {
		return fmt.Errorf("M-Team search mode must be normal or adult")
	}
	if len(options.Categories) > maxSearchCategories {
		return fmt.Errorf("M-Team search accepts at most %d categories", maxSearchCategories)
	}
	seen := make(map[string]bool, len(options.Categories))
	for _, category := range options.Categories {
		if !canonicalID(category) {
			return fmt.Errorf("M-Team search category must be a numeric category ID")
		}
		if seen[category] {
			return fmt.Errorf("M-Team search category %s is repeated", category)
		}
		seen[category] = true
	}
	return nil
}

// SearchWithOptions runs one search request. FreeOnly asks the API for free
// torrents and also drops any returned row whose download is counted, so a
// filtered page can hold fewer rows than an unfiltered one.
func (a *Adapter) SearchWithOptions(ctx context.Context, credential site.Credential, query string, options site.SearchOptions) ([]domain.TorrentSummary, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("search query is empty")
	}
	if len(query) > maxQueryBytes || !utf8.ValidString(query) || hasControlRune(query) {
		return nil, fmt.Errorf("M-Team search query must be at most %d bytes of printable UTF-8", maxQueryBytes)
	}
	if err := a.ValidateSearchOptions(options); err != nil {
		return nil, err
	}
	request := searchRequest{Mode: "normal", Keyword: query, PageNumber: 1, PageSize: searchPageSize, Categories: options.Categories}
	if options.Page > 0 {
		request.PageNumber = options.Page
	}
	if options.Mode != "" {
		request.Mode = options.Mode
	}
	if options.FreeOnly {
		request.Discount = freeDiscount
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode M-Team search request failed")
	}
	data, err := a.call(ctx, credential, searchPath, jsonMediaType, body)
	if err != nil {
		return nil, err
	}
	results, err := parseSearch(data)
	if err != nil || !options.FreeOnly {
		return results, err
	}
	free := results[:0]
	for _, result := range results {
		if result.Promotion != nil && result.Promotion.DownloadFactor == "0" {
			free = append(free, result)
		}
	}
	return free, nil
}

type searchRequest struct {
	Mode       string   `json:"mode"`
	Keyword    string   `json:"keyword"`
	PageNumber int      `json:"pageNumber"`
	PageSize   int      `json:"pageSize"`
	Categories []string `json:"categories,omitempty"`
	Discount   string   `json:"discount,omitempty"`
}
