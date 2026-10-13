package mteam

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

// siteTimeZone is how the API's zone-less "YYYY-MM-DD HH:MM:SS" timestamps
// are read. M-Team serves a UTC+8 audience and neither of its likely zones
// observes daylight saving, so a fixed offset avoids a tzdata dependency.
var siteTimeZone = time.FixedZone("UTC+8", 8*60*60)

const siteTimeLayout = "2006-01-02 15:04:05"

// decodeEnvelope classifies one API response. Every API answer is
// {code, message, data}; only code "0" carries data. The message is never
// surfaced because it is free text chosen by the server.
func decodeEnvelope(response httpguard.StrictResponse) (json.RawMessage, error) {
	switch {
	case response.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("M-Team API rejected the request: %w", site.ErrCredentialRejected)
	case response.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("M-Team API refused the request: %w", site.ErrRateLimited)
	case response.StatusCode >= 300 && response.StatusCode <= 399:
		return nil, fmt.Errorf("M-Team API redirect was rejected")
	case response.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("M-Team API returned HTTP %d", response.StatusCode)
	}
	switch response.MediaType {
	case "application/json", "text/plain", "":
	default:
		return nil, fmt.Errorf("M-Team API response was not JSON")
	}
	var envelope struct {
		Code flexText        `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response.Body, &envelope); err != nil || !envelope.Code.set {
		return nil, fmt.Errorf("M-Team API response was not recognized")
	}
	switch code := envelope.Code.value; code {
	case "0":
		return envelope.Data, nil
	case "401":
		return nil, fmt.Errorf("M-Team API rejected the request: %w", site.ErrCredentialRejected)
	case "4":
		return nil, fmt.Errorf("M-Team API refused the request: %w", site.ErrRateLimited)
	default:
		if !safeCode(code) {
			return nil, fmt.Errorf("M-Team API refused the request")
		}
		return nil, fmt.Errorf("M-Team API refused the request (code %s)", code)
	}
}

type profileResponse struct {
	Username    *string `json:"username"`
	MemberCount *struct {
		Uploaded   flexText `json:"uploaded"`
		Downloaded flexText `json:"downloaded"`
		ShareRate  flexText `json:"shareRate"`
		Bonus      flexText `json:"bonus"`
	} `json:"memberCount"`
}

// parseProfile maps /member/profile data. Username is required for an
// authenticated result; counters are optional and dropped when malformed.
func parseProfile(data json.RawMessage) (domain.AccountSnapshot, error) {
	var profile profileResponse
	if !isJSONObject(data) || json.Unmarshal(data, &profile) != nil {
		return domain.AccountSnapshot{}, fmt.Errorf("M-Team profile response was not recognized")
	}
	snapshot := domain.AccountSnapshot{SiteID: SiteID}
	if profile.Username != nil {
		username, ok := cleanText(*profile.Username, 128)
		if !ok {
			return domain.AccountSnapshot{}, fmt.Errorf("M-Team profile username was not recognized")
		}
		snapshot.Username = username
	}
	if count := profile.MemberCount; count != nil {
		snapshot.UploadedBytes = byteCount(count.Uploaded)
		snapshot.DownloadedBytes = byteCount(count.Downloaded)
		snapshot.Ratio = decimalText(count.ShareRate)
		snapshot.Bonus = decimalText(count.Bonus)
	}
	return snapshot, nil
}

type searchResponse struct {
	Data *[]searchItem `json:"data"`
}

type searchItem struct {
	ID          flexText `json:"id"`
	Name        *string  `json:"name"`
	Size        flexText `json:"size"`
	Category    flexText `json:"category"`
	CreatedDate flexText `json:"createdDate"`
	Status      *struct {
		Discount        flexText `json:"discount"`
		DiscountEndTime flexText `json:"discountEndTime"`
		Seeders         flexText `json:"seeders"`
		Leechers        flexText `json:"leechers"`
		TimesCompleted  flexText `json:"timesCompleted"`
	} `json:"status"`
}

// parseSearch maps /torrent/search data. A missing result list or any row
// without a canonical ID and a name fails the whole search rather than
// returning a silently partial or empty page.
func parseSearch(data json.RawMessage) ([]domain.TorrentSummary, error) {
	var response searchResponse
	if !isJSONObject(data) || json.Unmarshal(data, &response) != nil || response.Data == nil {
		return nil, fmt.Errorf("M-Team search response was not recognized; refusing to treat it as an empty result")
	}
	results := make([]domain.TorrentSummary, 0, len(*response.Data))
	seen := make(map[string]struct{}, len(*response.Data))
	for _, item := range *response.Data {
		if !item.ID.set || !canonicalID(item.ID.value) || item.Name == nil {
			return nil, fmt.Errorf("M-Team search row was not recognized")
		}
		name, ok := cleanText(*item.Name, 1024)
		if !ok || name == "" {
			return nil, fmt.Errorf("M-Team search row was not recognized")
		}
		if _, duplicate := seen[item.ID.value]; duplicate {
			continue
		}
		seen[item.ID.value] = struct{}{}
		summary := domain.TorrentSummary{
			Ref:         domain.TorrentRef{SiteID: SiteID, RemoteID: item.ID.value},
			Name:        name,
			SizeBytes:   byteCount(item.Size),
			PublishedAt: siteTime(item.CreatedDate),
		}
		if canonicalID(item.Category.value) {
			summary.Category = item.Category.value
		}
		if status := item.Status; status != nil {
			summary.Seeders = count(status.Seeders)
			summary.Leechers = count(status.Leechers)
			summary.Snatches = count(status.TimesCompleted)
			summary.Promotion = promotion(status.Discount.value, siteTime(status.DiscountEndTime))
		}
		results = append(results, summary)
	}
	return results, nil
}

// promotion maps the API's discount names to traffic factors. An absent
// discount means NORMAL, as on the site itself; an unknown name yields no
// claim rather than a guessed one.
func promotion(discount string, endsAt *time.Time) *domain.Promotion {
	factors := map[string][2]string{
		"":               {"1", "1"},
		"NORMAL":         {"1", "1"},
		"FREE":           {"1", "0"},
		"_2X_FREE":       {"2", "0"},
		"PERCENT_50":     {"1", "0.5"},
		"PERCENT_70":     {"1", "0.7"},
		"_2X":            {"2", "1"},
		"_2X_PERCENT_50": {"2", "0.5"},
	}
	pair, ok := factors[discount]
	if !ok {
		return nil
	}
	result := &domain.Promotion{UploadFactor: pair[0], DownloadFactor: pair[1]}
	if pair != [2]string{"1", "1"} {
		result.EndsAt = endsAt
	}
	return result
}

// flexText accepts a JSON string, number, or null, because the API encodes
// most numbers as strings but not consistently.
type flexText struct {
	value string
	set   bool
}

func (f *flexText) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	switch {
	case bytes.Equal(raw, []byte("null")):
		*f = flexText{}
		return nil
	case len(raw) > 0 && raw[0] == '"':
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		*f = flexText{value: text, set: true}
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return err
	}
	*f = flexText{value: number.String(), set: true}
	return nil
}

func isJSONObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

func canonicalID(value string) bool {
	if value == "" || len(value) > 20 || value[0] == '0' {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	return err == nil && parsed != 0
}

func byteCount(field flexText) *int64 {
	if !field.set || !plainDigits(field.value, 19) {
		return nil
	}
	parsed, err := strconv.ParseInt(field.value, 10, 64)
	if err != nil {
		return nil
	}
	return &parsed
}

func count(field flexText) *int {
	if !field.set || !plainDigits(field.value, 9) {
		return nil
	}
	parsed, err := strconv.Atoi(field.value)
	if err != nil {
		return nil
	}
	return &parsed
}

func plainDigits(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

// decimalText keeps a non-negative decimal such as a ratio or bonus balance
// verbatim, so no precision is invented or lost, and drops anything else.
func decimalText(field flexText) string {
	value := field.value
	if !field.set || value == "" || len(value) > 32 {
		return ""
	}
	whole, fraction, hasPoint := strings.Cut(value, ".")
	if !plainDigits(whole, 32) || (hasPoint && !plainDigits(fraction, 32)) {
		return ""
	}
	return value
}

func siteTime(field flexText) *time.Time {
	if !field.set || len(field.value) != len(siteTimeLayout) {
		return nil
	}
	parsed, err := time.ParseInLocation(siteTimeLayout, field.value, siteTimeZone)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func cleanText(value string, maximum int) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) > maximum || !utf8.ValidString(value) || hasControlRune(value) {
		return "", false
	}
	return value, true
}

func safeCode(code string) bool {
	if code == "" || len(code) > 16 {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
