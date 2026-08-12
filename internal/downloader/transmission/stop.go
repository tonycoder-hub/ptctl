package transmission

import (
	"context"
	"fmt"
	"net/http"
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
		return downloader.ExistingJobStopDescriptor{}, fmt.Errorf("Transmission read session is closed")
	}
	if s.stopDescriptorAttempted {
		s.mu.Unlock()
		return downloader.ExistingJobStopDescriptor{}, fmt.Errorf("Transmission stop descriptor was already requested")
	}
	s.stopDescriptorAttempted = true
	valueProtocol := s.protocol
	s.mu.Unlock()
	descriptor := downloader.ExistingJobStopDescriptor{
		Driver: downloader.DriverTransmission, StopRouteID: "transmission.torrent.stop.v1",
	}
	switch valueProtocol {
	case protocolLegacy:
		descriptor.Protocol = downloader.ControlProtocolTransmissionV5
	case protocolJSONRPC:
		descriptor.Protocol = downloader.ControlProtocolTransmissionV6
	default:
		return downloader.ExistingJobStopDescriptor{}, fmt.Errorf("Transmission stop protocol is unsupported")
	}
	if err := descriptor.Validate(); err != nil {
		return downloader.ExistingJobStopDescriptor{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return downloader.ExistingJobStopDescriptor{}, fmt.Errorf("Transmission read session is closed")
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
	id, token := s.nextID, s.csrfToken
	s.mu.Unlock()
	if closed || attempted || descriptor.Validate() != nil {
		return fail("stop_descriptor_unavailable", fmt.Errorf("Transmission stop session is unavailable"))
	}
	body, err := downloader.MarshalExistingJobStopRequest(descriptor, request.JobKey, id)
	if err != nil {
		return fail("job_locator_invalid", fmt.Errorf("Transmission stop selector is invalid"))
	}
	s.mu.Lock()
	if s.closed || s.stopAttempted || s.stopDescriptor != descriptor || s.nextID != id {
		s.mu.Unlock()
		return fail("session_unavailable", fmt.Errorf("Transmission stop session is unavailable"))
	}
	s.stopAttempted = true
	s.nextID++
	s.mu.Unlock()
	requestsBefore := s.RequestsMade()
	response, requestErr := s.execute(ctx, body, token, maxControlResponseBytes)
	requestDelta := s.RequestsMade() - requestsBefore
	if requestDelta == 1 {
		receipt.RequestsAttempted = 1
		receipt.RequestBytes = int64(len(body))
		receipt.RequestBytesKnown = true
		receipt.RequestID = id
	}
	if requestDelta < 0 || requestDelta > 1 {
		return fail("session_unavailable", fmt.Errorf("Transmission stop request count is invalid"))
	}
	if requestErr != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return fail("context_cancelled", contextErr)
		}
		return fail("transport_failed", requestErr)
	}
	if response.status == http.StatusConflict {
		return fail("csrf_expired", fmt.Errorf("Transmission CSRF session expired; automatic replay is disabled"))
	}
	if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden {
		return fail("http_rejected", fmt.Errorf("Transmission rejected the credentials"))
	}
	if response.status != http.StatusOK {
		return fail("http_rejected", fmt.Errorf("Transmission rejected the stop request"))
	}
	result, err := decodeRPCResult(response.body, stopProtocol(descriptor), id)
	if err != nil {
		return fail("response_invalid", err)
	}
	fields, err := decodeObject(result, 1)
	if err != nil || len(fields) != 0 {
		return fail("response_invalid", fmt.Errorf("Transmission stop response was not accepted"))
	}
	if err := ctx.Err(); err != nil {
		return fail("context_cancelled", err)
	}
	receipt.Complete = true
	receipt.ObservedAtEnd = time.Now().UTC()
	return receipt, nil
}

func stopProtocol(descriptor downloader.ExistingJobStopDescriptor) protocol {
	if descriptor.Protocol == downloader.ControlProtocolTransmissionV6 {
		return protocolJSONRPC
	}
	return protocolLegacy
}
