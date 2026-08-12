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

func (a *Adapter) OpenExistingJobStopSession(ctx context.Context, credential downloader.Credential) (downloader.ExistingJobStopSession, error) {
	session, err := a.openReadSession(ctx, credential)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (s *readSession) ReadExistingJobStopDescriptor(ctx context.Context) (downloader.ExistingJobStopDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return downloader.ExistingJobStopDescriptor{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return downloader.ExistingJobStopDescriptor{}, fmt.Errorf("qBittorrent read session is closed")
	}
	if s.stopDescriptorAttempted {
		s.mu.Unlock()
		return downloader.ExistingJobStopDescriptor{}, fmt.Errorf("qBittorrent stop descriptor was already requested")
	}
	s.stopDescriptorAttempted = true
	s.mu.Unlock()

	raw, err := s.get(ctx, "api/v2/app/version", maxControlVersionBytes)
	if err != nil {
		return downloader.ExistingJobStopDescriptor{}, err
	}
	major, err := parseSupportedQBittorrentMajor(string(raw))
	if err != nil {
		return downloader.ExistingJobStopDescriptor{}, err
	}
	descriptor := downloader.ExistingJobStopDescriptor{Driver: downloader.DriverQBittorrent}
	switch major {
	case 4:
		descriptor.Protocol = downloader.ControlProtocolQBittorrentV4
		descriptor.StopRouteID = "qbittorrent.torrents.pause.v1"
	case 5:
		descriptor.Protocol = downloader.ControlProtocolQBittorrentV5
		descriptor.StopRouteID = "qbittorrent.torrents.stop.v1"
	default:
		return downloader.ExistingJobStopDescriptor{}, fmt.Errorf("qBittorrent stop protocol is unsupported")
	}
	if err := descriptor.Validate(); err != nil {
		return downloader.ExistingJobStopDescriptor{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return downloader.ExistingJobStopDescriptor{}, fmt.Errorf("qBittorrent read session is closed")
	}
	s.stopDescriptor = descriptor
	return descriptor, nil
}

func (s *readSession) Stop(ctx context.Context, request downloader.ExistingJobMutationRequest) (downloader.ExistingJobMutationReceipt, error) {
	receipt := downloader.ExistingJobMutationReceipt{Effect: downloader.StopEffect, ObservedAtStart: time.Now().UTC()}
	fail := func(reason string, err error) (downloader.ExistingJobMutationReceipt, error) {
		receipt.StopReason = reason
		receipt.ObservedAtEnd = time.Now().UTC()
		return receipt, err
	}
	if err := ctx.Err(); err != nil {
		return fail("context_cancelled", err)
	}
	s.mu.Lock()
	descriptor, closed, attempted := s.stopDescriptor, s.closed, s.stopAttempted
	s.mu.Unlock()
	if closed || attempted || descriptor.Validate() != nil {
		return fail("stop_descriptor_unavailable", fmt.Errorf("qBittorrent stop session is unavailable"))
	}
	body, err := downloader.MarshalExistingJobStopRequest(descriptor, request.JobKey, 0)
	if err != nil {
		return fail("job_locator_invalid", fmt.Errorf("qBittorrent stop selector is invalid"))
	}
	s.mu.Lock()
	if s.closed || s.stopAttempted || s.stopDescriptor != descriptor {
		s.mu.Unlock()
		return fail("session_unavailable", fmt.Errorf("qBittorrent stop session is unavailable"))
	}
	s.stopAttempted = true
	s.mu.Unlock()
	path := "api/v2/torrents/pause"
	if descriptor.Protocol == downloader.ControlProtocolQBittorrentV5 {
		path = "api/v2/torrents/stop"
	}
	target := s.adapter.resolve(path)
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return fail("request_build_failed", fmt.Errorf("build qBittorrent stop request failed"))
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
		return fail("transport_failed", fmt.Errorf("qBittorrent stop request failed"))
	}
	defer response.Body.Close()
	rawResponse, readErr := io.ReadAll(io.LimitReader(response.Body, maxControlResponseBytes+1))
	if readErr != nil {
		return fail("response_read_failed", fmt.Errorf("read qBittorrent stop response failed"))
	}
	if response.StatusCode != http.StatusOK {
		return fail("http_rejected", fmt.Errorf("qBittorrent rejected the stop request"))
	}
	trimmed := strings.TrimSpace(string(rawResponse))
	if int64(len(rawResponse)) > maxControlResponseBytes || trimmed != "" && trimmed != "Ok." {
		return fail("response_invalid", fmt.Errorf("qBittorrent stop response was not accepted"))
	}
	if err := ctx.Err(); err != nil {
		return fail("context_cancelled", err)
	}
	receipt.Complete = true
	receipt.ObservedAtEnd = time.Now().UTC()
	return receipt, nil
}
