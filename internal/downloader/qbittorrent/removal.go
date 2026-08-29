package qbittorrent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

func (a *Adapter) OpenExistingJobRemovalSession(ctx context.Context, credential downloader.Credential) (downloader.ExistingJobRemovalSession, error) {
	session, err := a.openReadSession(ctx, credential)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (s *readSession) ReadExistingJobRemovalDescriptor(ctx context.Context) (downloader.ExistingJobRemovalDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return downloader.ExistingJobRemovalDescriptor{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return downloader.ExistingJobRemovalDescriptor{}, fmt.Errorf("qBittorrent read session is closed")
	}
	if s.removalDescriptorAttempted {
		s.mu.Unlock()
		return downloader.ExistingJobRemovalDescriptor{}, fmt.Errorf("qBittorrent removal descriptor was already requested")
	}
	s.removalDescriptorAttempted = true
	s.mu.Unlock()

	raw, err := s.get(ctx, "api/v2/app/version", maxControlVersionBytes)
	if err != nil {
		return downloader.ExistingJobRemovalDescriptor{}, err
	}
	major, err := parseSupportedQBittorrentMajor(string(raw))
	if err != nil {
		return downloader.ExistingJobRemovalDescriptor{}, err
	}
	descriptor := downloader.ExistingJobRemovalDescriptor{
		Driver: downloader.DriverQBittorrent, RemoveRouteID: "qbittorrent.torrents.delete_keep_data.v1",
	}
	switch major {
	case 4:
		descriptor.Protocol = downloader.ControlProtocolQBittorrentV4
	case 5:
		descriptor.Protocol = downloader.ControlProtocolQBittorrentV5
	default:
		return downloader.ExistingJobRemovalDescriptor{}, fmt.Errorf("qBittorrent removal protocol is unsupported")
	}
	if err := descriptor.Validate(); err != nil {
		return downloader.ExistingJobRemovalDescriptor{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return downloader.ExistingJobRemovalDescriptor{}, fmt.Errorf("qBittorrent read session is closed")
	}
	s.removalDescriptor = descriptor
	return descriptor, nil
}

func (s *readSession) RemoveKeepData(ctx context.Context, request downloader.ExistingJobMutationRequest) (downloader.ExistingJobMutationReceipt, error) {
	receipt := downloader.ExistingJobMutationReceipt{Effect: downloader.RemoveEffectKeepData, ObservedAtStart: time.Now().UTC()}
	fail := func(reason string, err error) (downloader.ExistingJobMutationReceipt, error) {
		receipt.StopReason = reason
		receipt.ObservedAtEnd = time.Now().UTC()
		return receipt, err
	}
	if err := ctx.Err(); err != nil {
		return fail("context_cancelled", err)
	}
	s.mu.Lock()
	descriptor, closed, attempted := s.removalDescriptor, s.closed, s.removeAttempted
	s.mu.Unlock()
	if closed || attempted || descriptor.Validate() != nil {
		return fail("removal_descriptor_unavailable", fmt.Errorf("qBittorrent removal session is unavailable"))
	}
	body, err := downloader.MarshalExistingJobRemovalRequest(descriptor, request.JobKey, 0)
	if err != nil {
		return fail("job_locator_invalid", fmt.Errorf("qBittorrent removal selector is invalid"))
	}
	s.mu.Lock()
	if s.closed || s.removeAttempted || s.removalDescriptor != descriptor {
		s.mu.Unlock()
		return fail("session_unavailable", fmt.Errorf("qBittorrent removal session is unavailable"))
	}
	s.removeAttempted = true
	s.mu.Unlock()
	target := s.adapter.resolve("api/v2/torrents/delete")
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return fail("request_build_failed", fmt.Errorf("build qBittorrent removal request failed"))
	}
	httpRequest.Close = true
	httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpRequest.Header.Set("Accept", "text/plain")
	httpRequest.Header.Set("Referer", origin(s.adapter.base)+"/")
	httpRequest.Header.Set("User-Agent", "ptctl/0.1")
	if err := s.beginRequest(ctx); err != nil {
		return fail("session_unavailable", err)
	}
	receipt.RequestsAttempted = 1
	receipt.RequestBytes = int64(len(body))
	receipt.RequestBytesKnown = true
	response, err := s.client.Do(httpRequest)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return fail("context_cancelled", contextErr)
		}
		return fail("transport_failed", fmt.Errorf("qBittorrent removal request failed"))
	}
	defer response.Body.Close()
	rawResponse, readErr := io.ReadAll(io.LimitReader(response.Body, maxControlResponseBytes+1))
	if readErr != nil {
		return fail("response_read_failed", fmt.Errorf("read qBittorrent removal response failed"))
	}
	if response.StatusCode != http.StatusOK {
		return fail("http_rejected", fmt.Errorf("qBittorrent rejected the removal request"))
	}
	trimmed := strings.TrimSpace(string(rawResponse))
	if int64(len(rawResponse)) > maxControlResponseBytes || trimmed != "" && trimmed != "Ok." {
		return fail("response_invalid", fmt.Errorf("qBittorrent removal response was not accepted"))
	}
	if err := ctx.Err(); err != nil {
		return fail("context_cancelled", err)
	}
	receipt.Complete = true
	receipt.ObservedAtEnd = time.Now().UTC()
	return receipt, nil
}
