package clientactivate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
)

type clientObservation struct {
	ledger             downloader.LedgerSnapshot
	files              downloader.JobFileLedgerSnapshot
	job                downloader.Torrent
	jobID              string
	fileLayoutID       string
	completeSnapshotID string
	allSelected        bool
	allComplete        bool
	ledgerRequestMade  bool
	fileRequestMade    bool
}

func observeClient(ctx context.Context, authority *PreparedAuthority, session downloader.LedgerSession) (clientObservation, error) {
	var result clientObservation
	if authority == nil || session == nil {
		return result, fmt.Errorf("%w: client observation authority is unavailable", ErrPolicy)
	}
	ledger, err := session.ReadLedger(ctx)
	result.ledger = ledger
	if err != nil {
		return result, err
	}
	if ledger.Driver != DriverQBittorrent || !ledger.Capabilities.ContentPath {
		return result, fmt.Errorf("%w: downloader ledger lacks the reviewed qBittorrent path capability", ErrIntegrity)
	}
	assessment, err := downloader.AssessLedgerIdentity(ledger, downloader.TypedIdentity{
		InfoHashV1: authority.final.InfoHashV1, InfoHashV2: authority.final.InfoHashV2,
	})
	if err != nil || assessment.Status != downloader.LedgerIdentityExactUnique || assessment.ExactJob == nil {
		if err == nil {
			err = fmt.Errorf("%w: downloader does not expose one unique exact typed job", ErrPolicy)
		}
		return result, err
	}
	job := *assessment.ExactJob
	job.State = normalizedClientState(job.State)
	result.job, result.jobID = job, opaqueJobID(job.Hash)
	if err := validateJobEnvelope(authority, job, result.jobID); err != nil {
		return result, err
	}
	if authority.final.MultiFile {
		if !ledger.Capabilities.JobFiles || !ledger.Capabilities.ContentPath {
			return result, fmt.Errorf("%w: downloader per-file layout capability is unavailable", ErrPolicy)
		}
		files, fileErr := session.ReadJobFiles(ctx, job.Hash, authority.fileLimits)
		result.files, result.fileRequestMade = files, true
		if fileErr != nil {
			return result, fileErr
		}
		layoutID, snapshotID, selected, complete, validateErr := validateFileLayout(authority, job, files)
		if validateErr != nil {
			return result, validateErr
		}
		if files.ObservedAtStart.Before(ledger.ObservedAtEnd) {
			return result, fmt.Errorf("%w: downloader file ledger predates its job ledger", ErrIntegrity)
		}
		result.ledger.ObservedAtEnd = files.ObservedAtEnd
		result.fileLayoutID, result.completeSnapshotID = layoutID, snapshotID
		result.allSelected, result.allComplete = selected, complete
	} else {
		layoutID, snapshotID, err := singleFileLayoutIdentity(authority, job)
		if err != nil {
			return result, err
		}
		result.fileLayoutID, result.completeSnapshotID = layoutID, snapshotID
		result.allSelected = true
		result.allComplete = job.Progress == 1 && (completeStoppedState(job.State) || startedState(job.State))
	}
	return result, nil
}

func (observed clientObservation) validateForPlan(authority *PreparedAuthority) error {
	if authority == nil || observed.job.Hash == "" || observed.jobID != authority.expectedJobID ||
		!canonicalSHA256ID(observed.fileLayoutID) || !stoppedState(observed.job.State) ||
		observed.fileLayoutID == "" || !observed.allSelected {
		return fmt.Errorf("%w: exact adopted job is not ready for a reviewed recheck plan", ErrPolicy)
	}
	return nil
}

func validateJobEnvelope(authority *PreparedAuthority, job downloader.Torrent, currentJobID string) error {
	if authority == nil || currentJobID == "" || currentJobID != authority.expectedJobID || job.SizeBytes != authority.final.ContentBytes ||
		math.IsNaN(job.Progress) || math.IsInf(job.Progress, 0) || job.Progress < 0 || job.Progress > 1 {
		return fmt.Errorf("%w: exact downloader job differs from the adopted identity or size", ErrIntegrity)
	}
	save, saveOK := authority.projection.SavePath()
	content, contentOK := authority.projection.ContentPath()
	if !saveOK || !contentOK {
		return fmt.Errorf("%w: projected client paths are unavailable", ErrIntegrity)
	}
	saveEqual, err := reconcile.EqualClientPaths(save, job.SavePath, authority.windows)
	if err != nil || !saveEqual {
		return fmt.Errorf("%w: downloader save path differs from the reviewed mapping", ErrIntegrity)
	}
	contentEqual, err := reconcile.EqualClientPaths(content, job.ContentPath, authority.windows)
	if err != nil || !contentEqual {
		return fmt.Errorf("%w: downloader content path differs from the reviewed mapping", ErrIntegrity)
	}
	return nil
}

type fileDigestRow struct {
	Index     int     `json:"index"`
	PathRef   string  `json:"path_ref"`
	SizeBytes int64   `json:"size_bytes"`
	Selection string  `json:"selection"`
	Progress  float64 `json:"progress,omitempty"`
	Complete  bool    `json:"complete,omitempty"`
}

func validateFileLayout(authority *PreparedAuthority, job downloader.Torrent, snapshot downloader.JobFileLedgerSnapshot) (string, string, bool, bool, error) {
	if authority == nil || !snapshot.Complete || snapshot.Driver != DriverQBittorrent || snapshot.JobKey != job.Hash ||
		snapshot.Limits != authority.fileLimits || snapshot.ObservedAtStart.IsZero() || snapshot.ObservedAtEnd.Before(snapshot.ObservedAtStart) ||
		len(snapshot.Files) != authority.final.ManifestFiles || len(snapshot.Files) > snapshot.Limits.MaxFiles ||
		snapshot.Used.FilesConsidered != len(snapshot.Files) || snapshot.Used.ResponseBytes <= 0 ||
		snapshot.Used.ResponseBytes > snapshot.Limits.MaxResponseBytes {
		return "", "", false, false, fmt.Errorf("%w: downloader file ledger is incomplete or invalid", ErrPolicy)
	}
	rows := make([]fileDigestRow, len(snapshot.Files))
	paths := make(map[int]string, len(snapshot.Files))
	allSelected, allComplete := true, true
	var observedPathBytes int64
	for position, file := range snapshot.Files {
		if file.Index != position || file.Index < 0 || file.Index >= authority.final.ManifestFiles ||
			math.IsNaN(file.Progress) || math.IsInf(file.Progress, 0) || file.Progress < 0 || file.Progress > 1 ||
			file.Complete && file.Progress != 1 ||
			(file.Selection != downloader.JobFileSelectionSelected && file.Selection != downloader.JobFileSelectionSkipped) {
			return "", "", false, false, fmt.Errorf("%w: downloader file ledger index or progress is invalid", ErrIntegrity)
		}
		for _, component := range file.RelativeComponents {
			observedPathBytes += int64(len(component))
			if observedPathBytes > snapshot.Limits.MaxPathBytes {
				return "", "", false, false, fmt.Errorf("%w: downloader file ledger path budget is invalid", ErrIntegrity)
			}
		}
		expectedPath, pathOK := authority.projection.FilePath(file.Index)
		expectedSize, sizeOK := authority.projection.FileSize(file.Index)
		if !pathOK || !sizeOK || file.SizeBytes != expectedSize {
			return "", "", false, false, fmt.Errorf("%w: downloader file size differs from the materialized layout", ErrIntegrity)
		}
		effective, err := reconcile.JoinClientPath(job.SavePath, file.RelativeComponents, authority.windows)
		if err != nil {
			return "", "", false, false, fmt.Errorf("%w: downloader effective file path is invalid", ErrIntegrity)
		}
		equal, err := reconcile.EqualClientPaths(expectedPath, effective, authority.windows)
		if err != nil || !equal {
			return "", "", false, false, fmt.Errorf("%w: downloader effective file path differs from the materialized layout", ErrIntegrity)
		}
		pathRef, err := reconcile.ClientPathReference(effective, authority.windows)
		if err != nil {
			return "", "", false, false, fmt.Errorf("%w: downloader effective file path is invalid", ErrIntegrity)
		}
		paths[file.Index] = effective
		rows[position] = fileDigestRow{Index: file.Index, PathRef: pathRef, SizeBytes: file.SizeBytes, Selection: string(file.Selection), Progress: file.Progress, Complete: file.Complete}
		if file.Selection != downloader.JobFileSelectionSelected {
			allSelected = false
		}
		if file.Selection != downloader.JobFileSelectionSelected || file.Progress != 1 || !file.Complete {
			allComplete = false
		}
	}
	if snapshot.Used.PathBytes != observedPathBytes {
		return "", "", false, false, fmt.Errorf("%w: downloader file ledger usage is contradictory", ErrIntegrity)
	}
	if err := reconcile.ValidateClientPathSet(paths, authority.windows); err != nil {
		return "", "", false, false, fmt.Errorf("%w: downloader file namespace is unsafe", ErrIntegrity)
	}
	layoutRows := make([]fileDigestRow, len(rows))
	for index, row := range rows {
		layoutRows[index] = fileDigestRow{Index: row.Index, PathRef: row.PathRef, SizeBytes: row.SizeBytes, Selection: row.Selection}
	}
	return digestRows("ptctl-client-activation-layout-v1\x00", layoutRows), digestRows("ptctl-client-activation-file-snapshot-v1\x00", rows), allSelected, allComplete, nil
}

func singleFileLayoutIdentity(authority *PreparedAuthority, job downloader.Torrent) (string, string, error) {
	pathRef, ok := authority.projection.FilePathRef(0)
	fileSize, sizeOK := authority.projection.FileSize(0)
	if !ok || !sizeOK || fileSize != job.SizeBytes {
		return "", "", fmt.Errorf("%w: single-file client layout differs from the materialized final", ErrIntegrity)
	}
	row := fileDigestRow{Index: 0, PathRef: pathRef, SizeBytes: fileSize, Selection: string(downloader.JobFileSelectionSelected)}
	snapshot := row
	snapshot.Progress, snapshot.Complete = job.Progress, job.Progress == 1 && (completeStoppedState(job.State) || startedState(job.State))
	return digestRows("ptctl-client-activation-layout-v1\x00", []fileDigestRow{row}), digestRows("ptctl-client-activation-file-snapshot-v1\x00", []fileDigestRow{snapshot}), nil
}

func digestRows(domain string, rows []fileDigestRow) string {
	raw, _ := json.Marshal(rows)
	digest := sha256.Sum256(append([]byte(domain), raw...))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func opaqueJobID(value string) string {
	digest := sha256.Sum256([]byte("ptctl-downloader-job-v1\x00" + value))
	return "sha256:" + hex.EncodeToString(digest[:])
}
