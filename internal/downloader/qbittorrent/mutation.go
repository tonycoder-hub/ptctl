package qbittorrent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

const (
	maxAddMetafileBytes = int64(32 << 20)
	maxAddSavePathBytes = 32 << 10
	maxAddResponseBytes = int64(1024)
)

var errAddPayloadLength = errors.New("qBittorrent add payload length differs")

type exactMetafileReader struct {
	reader    io.Reader
	remaining int64
	finished  bool
}

func (reader *exactMetafileReader) Read(buffer []byte) (int, error) {
	if reader == nil || reader.reader == nil || reader.remaining < 0 {
		return 0, errAddPayloadLength
	}
	if len(buffer) == 0 {
		return 0, nil
	}
	if reader.finished {
		return 0, io.EOF
	}
	if reader.remaining > 0 {
		if int64(len(buffer)) > reader.remaining {
			buffer = buffer[:reader.remaining]
		}
		count, err := reader.reader.Read(buffer)
		reader.remaining -= int64(count)
		if reader.remaining < 0 {
			return count, errAddPayloadLength
		}
		if errors.Is(err, io.EOF) {
			if reader.remaining != 0 {
				return count, io.ErrUnexpectedEOF
			}
			reader.finished = true
			return count, nil
		}
		if err != nil {
			return count, err
		}
		if count == 0 {
			return 0, io.ErrNoProgress
		}
		return count, nil
	}
	var probe [1]byte
	count, err := reader.reader.Read(probe[:])
	if count != 0 {
		return 0, errAddPayloadLength
	}
	if errors.Is(err, io.EOF) {
		reader.finished = true
		return 0, io.EOF
	}
	if err != nil {
		return 0, err
	}
	return 0, io.ErrNoProgress
}

func (a *Adapter) OpenMutationSession(ctx context.Context, credential downloader.Credential) (downloader.MutationSession, error) {
	session, err := a.openReadSession(ctx, credential)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// ClientConfigID binds an adoption plan to one normalized qB origin and user
// without serializing either value into the plan or journal.
func (a *Adapter) ClientConfigID(username string) (string, error) {
	if a == nil || a.base == nil || username == "" || len(username) > 1024 || !utf8.ValidString(username) {
		return "", fmt.Errorf("qBittorrent client configuration is invalid")
	}
	for _, character := range username {
		if character == 0 || character < 0x20 || character == 0x7f {
			return "", fmt.Errorf("qBittorrent client configuration is invalid")
		}
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"ptctl-qbittorrent-client-config-v1", origin(a.base), username,
	}, "\x00")))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (s *readSession) AddStopped(ctx context.Context, request downloader.AddStoppedRequest) (downloader.MutationReceipt, error) {
	receipt := downloader.MutationReceipt{
		Effect: "submit_exact_metafile_stopped", ObservedAtStart: time.Now().UTC(),
	}
	fail := func(reason string, err error) (downloader.MutationReceipt, error) {
		receipt.StopReason = reason
		receipt.ObservedAtEnd = time.Now().UTC()
		return receipt, err
	}
	if err := ctx.Err(); err != nil {
		return fail("context_cancelled", err)
	}
	if request.Metafile == nil {
		return fail("payload_invalid", fmt.Errorf("qBittorrent add payload is invalid"))
	}
	payloadBytes, variantID := request.Metafile.SizeBytes(), request.Metafile.VariantID()
	if payloadBytes <= 0 || payloadBytes > maxAddMetafileBytes || !canonicalAddVariantID(variantID) {
		return fail("payload_invalid", fmt.Errorf("qBittorrent add payload is invalid"))
	}
	if err := validateAddSavePath(request.SavePath); err != nil {
		return fail("save_path_invalid", err)
	}
	reader, err := request.Metafile.Open()
	if err != nil {
		return fail("payload_unavailable", fmt.Errorf("qBittorrent add payload is unavailable"))
	}
	prefix, suffix, contentType, err := addMultipartEnvelope(request.SavePath)
	if err != nil {
		return fail("request_build_failed", fmt.Errorf("build qBittorrent add request failed"))
	}
	contentLength := int64(len(prefix)) + payloadBytes + int64(len(suffix))
	body := io.MultiReader(bytes.NewReader(prefix), &exactMetafileReader{reader: reader, remaining: payloadBytes}, bytes.NewReader(suffix))
	target := s.adapter.resolve("api/v2/torrents/add")
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), body)
	if err != nil {
		return fail("request_build_failed", fmt.Errorf("build qBittorrent add request failed"))
	}
	httpRequest.ContentLength = contentLength
	httpRequest.Close = true
	httpRequest.Header.Set("Content-Type", contentType)
	httpRequest.Header.Set("Accept", "text/plain")
	httpRequest.Header.Set("Referer", origin(s.adapter.base)+"/")
	httpRequest.Header.Set("User-Agent", "ptctl/0.1")
	if err := s.beginRequest(ctx); err != nil {
		return fail("session_unavailable", err)
	}
	receipt.RequestsAttempted = 1
	response, err := s.client.Do(httpRequest)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return fail("context_cancelled", contextErr)
		}
		return fail("transport_failed", fmt.Errorf("qBittorrent add request failed"))
	}
	defer response.Body.Close()
	rawResponse, readErr := io.ReadAll(io.LimitReader(response.Body, maxAddResponseBytes+1))
	if readErr != nil {
		return fail("response_read_failed", fmt.Errorf("read qBittorrent add response failed"))
	}
	if response.StatusCode != http.StatusOK {
		return fail("http_rejected", fmt.Errorf("qBittorrent rejected the add request"))
	}
	if int64(len(rawResponse)) > maxAddResponseBytes || strings.TrimSpace(string(rawResponse)) != "Ok." {
		return fail("response_invalid", fmt.Errorf("qBittorrent add response was not accepted"))
	}
	if err := ctx.Err(); err != nil {
		return fail("context_cancelled", err)
	}
	receipt.Complete = true
	receipt.BytesSubmitted = payloadBytes
	receipt.BytesSubmittedKnown = true
	receipt.ObservedAtEnd = time.Now().UTC()
	return receipt, nil
}

func canonicalAddVariantID(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	digest := strings.TrimPrefix(value, "sha256:")
	if strings.ToLower(digest) != digest {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size
}

func validateAddSavePath(value string) error {
	if value == "" || len(value) > maxAddSavePathBytes || !utf8.ValidString(value) || strings.ContainsRune(value, utf8.RuneError) {
		return fmt.Errorf("qBittorrent add save path is invalid")
	}
	for _, character := range value {
		if character == 0 || character < 0x20 || character == 0x7f {
			return fmt.Errorf("qBittorrent add save path is invalid")
		}
	}
	return nil
}

func addMultipartEnvelope(savePath string) ([]byte, []byte, string, error) {
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for _, field := range []struct{ name, value string }{
		{"savepath", savePath}, {"paused", "true"}, {"stopped", "true"}, {"skip_checking", "false"},
	} {
		if err := writer.WriteField(field.name, field.value); err != nil {
			return nil, nil, "", err
		}
	}
	if _, err := writer.CreateFormFile("torrents", "metafile.torrent"); err != nil {
		return nil, nil, "", err
	}
	prefixLength := buffer.Len()
	if err := writer.Close(); err != nil {
		return nil, nil, "", err
	}
	raw := buffer.Bytes()
	return append([]byte(nil), raw[:prefixLength]...), append([]byte(nil), raw[prefixLength:]...), writer.FormDataContentType(), nil
}
