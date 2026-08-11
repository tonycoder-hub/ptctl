package transmission

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

const (
	maxAddMetafileBytes = int64(32 << 20)
	maxAddSavePathBytes = 32 << 10
	maxAddResponseBytes = int64(64 << 10)
)

var errAddPayloadLength = errors.New("Transmission add payload length differs")

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

type addBodyResult struct {
	rawBytes int64
	complete bool
	err      error
}

func (a *Adapter) OpenMutationSession(ctx context.Context, credential downloader.Credential) (downloader.MutationSession, error) {
	session, err := a.openReadSession(ctx, credential)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// ClientConfigID binds a reviewed adoption plan to one exact normalized RPC
// endpoint and username without serializing either value into the journal.
func (a *Adapter) ClientConfigID(username string) (string, error) {
	if a == nil || a.endpoint == nil || username == "" || !validBasicUsername(username) {
		return "", fmt.Errorf("Transmission client configuration is invalid")
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"ptctl-transmission-client-config-v1", canonicalRPCEndpoint(a.endpoint), username,
	}, "\x00")))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func canonicalRPCEndpoint(endpoint *url.URL) string {
	scheme := strings.ToLower(endpoint.Scheme)
	hostname := strings.ToLower(endpoint.Hostname())
	port := endpoint.Port()
	if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		port = ""
	}
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	}
	return scheme + "://" + host + endpoint.EscapedPath()
}

func (s *readSession) AddStopped(ctx context.Context, request downloader.AddStoppedRequest) (downloader.MutationReceipt, error) {
	receipt := downloader.MutationReceipt{Effect: "submit_exact_metafile_stopped", ObservedAtStart: time.Now().UTC()}
	fail := func(reason string, err error) (downloader.MutationReceipt, error) {
		receipt.StopReason = reason
		receipt.ObservedAtEnd = time.Now().UTC()
		return receipt, err
	}
	if err := ctx.Err(); err != nil {
		return fail("context_cancelled", err)
	}
	if request.Metafile == nil {
		return fail("payload_invalid", fmt.Errorf("Transmission add payload is invalid"))
	}
	if err := request.Identity.Validate(); err != nil || request.Identity.InfoHashV1 == "" || request.Identity.InfoHashV2 != "" {
		return fail("payload_invalid", fmt.Errorf("Transmission add identity is invalid"))
	}
	payloadBytes, variantID := request.Metafile.SizeBytes(), request.Metafile.VariantID()
	if payloadBytes <= 0 || payloadBytes > maxAddMetafileBytes || !canonicalAddVariantID(variantID) {
		return fail("payload_invalid", fmt.Errorf("Transmission add payload is invalid"))
	}
	if err := validateAddSavePath(request.SavePath); err != nil {
		return fail("save_path_invalid", err)
	}
	s.mu.Lock()
	if s.closed || s.addAttempted {
		s.mu.Unlock()
		return fail("session_unavailable", fmt.Errorf("Transmission stopped-add session is unavailable"))
	}
	s.addAttempted = true
	id := s.nextID
	s.nextID++
	valueProtocol, token := s.protocol, s.csrfToken
	s.mu.Unlock()

	source, err := request.Metafile.Open()
	if err != nil {
		return fail("payload_unavailable", fmt.Errorf("Transmission add payload is unavailable"))
	}
	prefix, suffix, err := addJSONEnvelope(valueProtocol, request.SavePath, id)
	if err != nil {
		return fail("request_build_failed", fmt.Errorf("build Transmission add request"))
	}
	encodedBytes := int64(base64.StdEncoding.EncodedLen(int(payloadBytes)))
	contentLength := int64(len(prefix)) + encodedBytes + int64(len(suffix))
	pipeReader, pipeWriter := io.Pipe()
	streamDone := make(chan addBodyResult, 1)
	go streamAddBody(pipeWriter, prefix, suffix, source, payloadBytes, streamDone)
	requestsBefore := s.RequestsMade()
	response, requestErr := s.executeReader(ctx, pipeReader, contentLength, token, maxAddResponseBytes)
	_ = pipeReader.CloseWithError(io.ErrClosedPipe)
	stream := <-streamDone
	requestDelta := s.RequestsMade() - requestsBefore
	if requestDelta == 1 {
		receipt.RequestsAttempted = 1
	}
	if stream.complete && stream.rawBytes == payloadBytes {
		receipt.BytesSubmitted = stream.rawBytes
		receipt.BytesSubmittedKnown = true
	}
	if requestDelta < 0 || requestDelta > 1 {
		return fail("session_unavailable", fmt.Errorf("Transmission add request count is invalid"))
	}
	if requestErr != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return fail("context_cancelled", contextErr)
		}
		return fail("transport_failed", requestErr)
	}
	if stream.err != nil || !stream.complete || stream.rawBytes != payloadBytes {
		return fail("payload_invalid", fmt.Errorf("Transmission add payload could not be submitted exactly"))
	}
	if response.status == http.StatusConflict {
		return fail("csrf_expired", fmt.Errorf("Transmission CSRF session expired; automatic replay is disabled"))
	}
	if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden {
		return fail("http_rejected", fmt.Errorf("Transmission rejected the credentials"))
	}
	if response.status != http.StatusOK {
		return fail("http_rejected", fmt.Errorf("Transmission rejected the add request"))
	}
	result, err := decodeRPCResult(response.body, valueProtocol, id)
	if err != nil {
		return fail("response_invalid", err)
	}
	added, duplicate, addedHash, err := decodeTorrentAddResult(result, valueProtocol)
	if err != nil {
		return fail("response_invalid", err)
	}
	if addedHash != request.Identity.InfoHashV1 {
		return fail("response_invalid", fmt.Errorf("Transmission added torrent identity differs from the request"))
	}
	if duplicate {
		return fail("already_exists", fmt.Errorf("Transmission reported an existing duplicate torrent"))
	}
	if !added {
		return fail("response_invalid", fmt.Errorf("Transmission add response was not accepted"))
	}
	if err := ctx.Err(); err != nil {
		return fail("context_cancelled", err)
	}
	receipt.Complete = true
	receipt.ObservedAtEnd = time.Now().UTC()
	return receipt, nil
}

func streamAddBody(writer *io.PipeWriter, prefix, suffix []byte, source io.Reader, expected int64, done chan<- addBodyResult) {
	result := addBodyResult{}
	exact := &exactMetafileReader{reader: source, remaining: expected}
	if _, err := writer.Write(prefix); err != nil {
		result.err = err
	} else {
		encoder := base64.NewEncoder(base64.StdEncoding, writer)
		result.rawBytes, result.err = io.Copy(encoder, exact)
		closeErr := encoder.Close()
		if result.err == nil {
			result.err = closeErr
		}
		if result.err == nil {
			_, result.err = writer.Write(suffix)
		}
	}
	result.complete = result.err == nil && exact.finished && result.rawBytes == expected
	done <- result
	_ = writer.CloseWithError(result.err)
}

func addJSONEnvelope(valueProtocol protocol, savePath string, id int64) ([]byte, []byte, error) {
	encodedPath, err := json.Marshal(savePath)
	if err != nil {
		return nil, nil, err
	}
	encodedID := strconv.FormatInt(id, 10)
	if valueProtocol == protocolJSONRPC {
		prefix := []byte(`{"jsonrpc":"2.0","method":"torrent_add","params":{"download_dir":` + string(encodedPath) + `,"metainfo":"`)
		suffix := []byte(`","paused":true},"id":` + encodedID + `}`)
		return prefix, suffix, nil
	}
	if valueProtocol == protocolLegacy {
		prefix := []byte(`{"method":"torrent-add","arguments":{"download-dir":` + string(encodedPath) + `,"metainfo":"`)
		suffix := []byte(`","paused":true},"tag":` + encodedID + `}`)
		return prefix, suffix, nil
	}
	return nil, nil, fmt.Errorf("unsupported Transmission RPC protocol")
}

func decodeTorrentAddResult(result json.RawMessage, valueProtocol protocol) (added, duplicate bool, hash string, err error) {
	fields, err := decodeObject(result, 2)
	if err != nil || len(fields) != 1 {
		return false, false, "", fmt.Errorf("decode Transmission add result")
	}
	addedKey, duplicateKey, hashKey := "torrent-added", "torrent-duplicate", "hashString"
	if valueProtocol == protocolJSONRPC {
		addedKey, duplicateKey, hashKey = "torrent_added", "torrent_duplicate", "hash_string"
	}
	var raw json.RawMessage
	if value, exists := fields[addedKey]; exists {
		added, raw = true, value
	} else if value, exists := fields[duplicateKey]; exists {
		duplicate, raw = true, value
	} else {
		return false, false, "", fmt.Errorf("Transmission add result is unsupported")
	}
	job, err := decodeObject(raw, 4)
	if err != nil || len(job) != 3 {
		return false, false, "", fmt.Errorf("decode Transmission added torrent")
	}
	id, idErr := requiredInt64(job, "id")
	name, nameErr := requiredString(job, "name")
	hashValue, hashErr := requiredString(job, hashKey)
	if idErr != nil || id < 0 || nameErr != nil || !validSimpleName(name) || hashErr != nil {
		return false, false, "", fmt.Errorf("Transmission added torrent is invalid")
	}
	normalizedHash, err := normalizeV1Hash(hashValue)
	if err != nil {
		return false, false, "", fmt.Errorf("Transmission added torrent identity is invalid")
	}
	return added, duplicate, normalizedHash, nil
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
		return fmt.Errorf("Transmission add save path is invalid")
	}
	for _, character := range value {
		if character == 0 || character < 0x20 || character == 0x7f {
			return fmt.Errorf("Transmission add save path is invalid")
		}
	}
	return nil
}
