package mteam

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const (
	userTorrentsPath     = "api/member/getUserTorrentList"
	userTorrentsPageSize = 50
	// maxUserTorrentsPage is the largest pageNumber the API accepts.
	maxUserTorrentsPage = 1000
)

// userTorrentTypes maps each list the adapter serves to the API's type name.
var userTorrentTypes = map[domain.AccountTorrentState]string{
	domain.AccountTorrentsSeeding:    "SEEDING",
	domain.AccountTorrentsLeeching:   "LEECHING",
	domain.AccountTorrentsCompleted:  "COMPLETED",
	domain.AccountTorrentsIncomplete: "INCOMPLETE",
	domain.AccountTorrentsUploaded:   "UPLOADED",
}

// ValidateAccountTorrentQuery accepts the seeding, leeching, completed,
// incomplete, and uploaded lists and a page from 1 to 1000.
func (a *Adapter) ValidateAccountTorrentQuery(query site.AccountTorrentQuery) error {
	if _, ok := userTorrentTypes[query.State]; !ok {
		return fmt.Errorf("M-Team torrent list state must be seeding, leeching, completed, incomplete, or uploaded")
	}
	if query.Page < 0 || query.Page > maxUserTorrentsPage {
		return fmt.Errorf("M-Team torrent list page must be from 1 to %d", maxUserTorrentsPage)
	}
	return nil
}

// AccountTorrents reads the account ID from the profile and then one page of
// the list, both on one client. Each row carries the torrent and the
// account's snatch record, the current peer, or both; the peer's address,
// peer ID, and client name are never decoded.
func (a *Adapter) AccountTorrents(ctx context.Context, credential site.Credential, query site.AccountTorrentQuery) (domain.AccountTorrentList, error) {
	if err := a.ValidateAccountTorrentQuery(query); err != nil {
		return domain.AccountTorrentList{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.AccountTorrentList{}, err
	}
	client, err := a.openClient(credential)
	if err != nil {
		return domain.AccountTorrentList{}, err
	}
	defer client.Close()
	data, err := post(ctx, client, profilePath, emptyFormMediaType, []byte(emptyFormBody))
	if err != nil {
		return domain.AccountTorrentList{}, err
	}
	userID, err := parseProfileID(data)
	if err != nil {
		return domain.AccountTorrentList{}, err
	}
	page := query.Page
	if page == 0 {
		page = 1
	}
	body, err := json.Marshal(userTorrentsRequest{UserID: userID, Type: userTorrentTypes[query.State], PageNumber: page, PageSize: userTorrentsPageSize})
	if err != nil {
		return domain.AccountTorrentList{}, fmt.Errorf("encode M-Team torrent list request failed")
	}
	data, err = post(ctx, client, userTorrentsPath, jsonMediaType, body)
	if err != nil {
		return domain.AccountTorrentList{}, err
	}
	list, err := parseUserTorrents(data)
	if err != nil {
		return domain.AccountTorrentList{}, err
	}
	list.State = query.State
	list.Page = page
	list.ObservedAt = time.Now().UTC()
	return list, nil
}

type userTorrentsRequest struct {
	UserID     string `json:"userid"`
	Type       string `json:"type"`
	PageNumber int    `json:"pageNumber"`
	PageSize   int    `json:"pageSize"`
}

type userTorrentsResponse struct {
	Total      flexText          `json:"total"`
	TotalPages flexText          `json:"totalPages"`
	Data       *[]userTorrentRow `json:"data"`
}

// userTorrentRow deliberately omits the peer's ip, ipv6, peerId, and agent.
type userTorrentRow struct {
	Torrent *searchItem `json:"torrent"`
	Peer    *struct {
		Uploaded   flexText `json:"uploaded"`
		Downloaded flexText `json:"downloaded"`
		LastAction flexText `json:"lastAction"`
	} `json:"peer"`
	Snatched *struct {
		UploadedReal   flexText `json:"uploadedReal"`
		DownloadedReal flexText `json:"downloadedReal"`
		SeedTime       flexText `json:"seedtime"`
		LastAction     flexText `json:"lastAction"`
	} `json:"snatched"`
}

// parseProfileID reads the account's numeric ID from /member/profile data.
func parseProfileID(data json.RawMessage) (string, error) {
	var profile struct {
		ID flexText `json:"id"`
	}
	if !isJSONObject(data) || json.Unmarshal(data, &profile) != nil || !canonicalID(profile.ID.value) {
		return "", fmt.Errorf("M-Team profile response had no account ID")
	}
	return profile.ID.value, nil
}

// parseUserTorrents maps /member/getUserTorrentList data. A missing list or
// any row without a recognizable torrent fails the whole page rather than
// returning a silently partial or empty one. Traffic comes from the snatch
// record when the API sends one, which counts real bytes over the account's
// whole history of the torrent, and otherwise from the current peer's report
// for its running session, which is all the seeding list carried when it was
// checked live.
func parseUserTorrents(data json.RawMessage) (domain.AccountTorrentList, error) {
	var response userTorrentsResponse
	if !isJSONObject(data) || json.Unmarshal(data, &response) != nil || response.Data == nil {
		return domain.AccountTorrentList{}, fmt.Errorf("M-Team torrent list response was not recognized; refusing to treat it as an empty list")
	}
	list := domain.AccountTorrentList{
		SiteID:     SiteID,
		Total:      count(response.Total),
		TotalPages: count(response.TotalPages),
		Torrents:   make([]domain.AccountTorrent, 0, len(*response.Data)),
	}
	seen := make(map[string]struct{}, len(*response.Data))
	for _, row := range *response.Data {
		if row.Torrent == nil {
			return domain.AccountTorrentList{}, fmt.Errorf("M-Team torrent list row was not recognized")
		}
		summary, ok := summaryFromItem(*row.Torrent)
		if !ok {
			return domain.AccountTorrentList{}, fmt.Errorf("M-Team torrent list row was not recognized")
		}
		if _, duplicate := seen[summary.Ref.RemoteID]; duplicate {
			continue
		}
		seen[summary.Ref.RemoteID] = struct{}{}
		item := domain.AccountTorrent{Torrent: summary}
		// Traffic comes from one source only, so the two spans never mix
		// in one row.
		if snatched := row.Snatched; snatched != nil {
			item.UploadedBytes = byteCount(snatched.UploadedReal)
			item.DownloadedBytes = byteCount(snatched.DownloadedReal)
			// Seconds follow the same non-negative integer rule as bytes.
			item.SeedTimeSeconds = byteCount(snatched.SeedTime)
			item.LastActionAt = siteTime(snatched.LastAction)
			if item.UploadedBytes != nil || item.DownloadedBytes != nil {
				item.TrafficScope = domain.AccountTrafficTotal
			}
		}
		if peer := row.Peer; peer != nil {
			if item.UploadedBytes == nil && item.DownloadedBytes == nil {
				item.UploadedBytes = byteCount(peer.Uploaded)
				item.DownloadedBytes = byteCount(peer.Downloaded)
				if item.UploadedBytes != nil || item.DownloadedBytes != nil {
					item.TrafficScope = domain.AccountTrafficCurrentSession
				}
			}
			if item.LastActionAt == nil {
				item.LastActionAt = siteTime(peer.LastAction)
			}
		}
		list.Torrents = append(list.Torrents, item)
	}
	return list, nil
}
