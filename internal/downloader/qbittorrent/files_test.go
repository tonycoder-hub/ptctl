package qbittorrent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

func TestJobFileLedgerLimits(t *testing.T) {
	defaults := downloader.DefaultJobFileLedgerLimits()
	if defaults.MaxFiles != 10_000 || defaults.MaxPathBytes != 16<<20 || defaults.MaxResponseBytes != 8<<20 {
		t.Fatalf("unexpected defaults: %#v", defaults)
	}
	if err := defaults.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, limits := range []downloader.JobFileLedgerLimits{
		{},
		{MaxFiles: 100_001, MaxPathBytes: 1, MaxResponseBytes: 1},
		{MaxFiles: 1, MaxPathBytes: (64 << 20) + 1, MaxResponseBytes: 1},
		{MaxFiles: 1, MaxPathBytes: 1, MaxResponseBytes: (32 << 20) + 1},
	} {
		if err := limits.Validate(); err == nil {
			t.Fatalf("accepted invalid limits: %#v", limits)
		}
	}
	if err := (downloader.JobFileLedgerLimits{MaxFiles: 100_000, MaxPathBytes: 64 << 20, MaxResponseBytes: 32 << 20}).Validate(); err != nil {
		t.Fatalf("rejected hard limits: %v", err)
	}
}

func TestReadJobFilesHappyPathEncodesOpaqueKeyAndCountsOneRequest(t *testing.T) {
	const opaqueKey = "opaque&hash=other+%?CANARY-JOB-KEY"
	body := []byte(`[
		{"index":7,"name":"bundle/z.bin","size":2,"progress":0.25,"priority":0,"is_seed":false,"availability":-1},
		{"index":2,"name":"bundle/a.bin","size":3,"progress":1,"priority":7,"is_seed":true}
	]`)
	var requests atomic.Int32
	queries := make(chan []string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/files":
			queries <- append([]string(nil), r.URL.Query()["hash"]...)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	session := openJobFileTestSession(t, server.URL)
	defer session.Close()
	limits := downloader.DefaultJobFileLedgerLimits()
	snapshot, err := session.ReadJobFiles(context.Background(), opaqueKey, limits)
	if err != nil {
		t.Fatal(err)
	}
	if got := <-queries; !slices.Equal(got, []string{opaqueKey}) {
		t.Fatalf("decoded hash query=%q", got)
	}
	if requests.Load() != 2 || session.RequestsMade() != 2 {
		t.Fatalf("requests server=%d session=%d, want login+one file GET", requests.Load(), session.RequestsMade())
	}
	if !snapshot.Complete || snapshot.Driver != "qbittorrent" || snapshot.JobKey != opaqueKey || snapshot.ObservedAtStart.IsZero() || snapshot.ObservedAtEnd.Before(snapshot.ObservedAtStart) || snapshot.Limits != limits || snapshot.Used.FilesConsidered != 2 || snapshot.Used.PathBytes != int64(len("bundle")+len("z.bin")+len("bundle")+len("a.bin")) || snapshot.Used.ResponseBytes != int64(len(body)) {
		t.Fatalf("invalid snapshot metadata: %#v", snapshot)
	}
	if len(snapshot.Files) != 2 || snapshot.Files[0].Index != 2 || snapshot.Files[1].Index != 7 || !slices.Equal(snapshot.Files[0].RelativeComponents, []string{"bundle", "a.bin"}) || snapshot.Files[0].Selection != downloader.JobFileSelectionSelected || !snapshot.Files[0].Complete || snapshot.Files[1].Selection != downloader.JobFileSelectionSkipped || snapshot.Files[1].Complete {
		t.Fatalf("unexpected normalized files: %#v", snapshot.Files)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(opaqueKey)) || bytes.Contains(encoded, []byte("job_key")) {
		t.Fatalf("serialized snapshot exposed opaque job authority: %s", encoded)
	}
}

func TestJobFileRequiredFieldsAndStrictTypes(t *testing.T) {
	required := []string{"index", "name", "size", "progress", "priority", "is_seed"}
	for _, field := range required {
		t.Run("missing_"+field, func(t *testing.T) {
			row := validJobFileRow(0, "x")
			delete(row, field)
			_, used, err := decodeJobFileTestBody(t, marshalJobFileRows(t, row), downloader.DefaultJobFileLedgerLimits())
			if err == nil || used.FilesConsidered != 1 {
				t.Fatalf("missing %s: used=%#v err=%v", field, used, err)
			}
		})
		t.Run("null_"+field, func(t *testing.T) {
			row := validJobFileRow(0, "x")
			row[field] = nil
			if _, _, err := decodeJobFileTestBody(t, marshalJobFileRows(t, row), downloader.DefaultJobFileLedgerLimits()); err == nil {
				t.Fatalf("accepted null %s", field)
			}
		})
	}

	tests := []struct {
		name  string
		field string
		value any
	}{
		{name: "index string", field: "index", value: "0"},
		{name: "index fraction", field: "index", value: 0.5},
		{name: "index negative", field: "index", value: -1},
		{name: "name number", field: "name", value: 1},
		{name: "size string", field: "size", value: "1"},
		{name: "size negative", field: "size", value: -1},
		{name: "progress string", field: "progress", value: "1"},
		{name: "progress negative", field: "progress", value: -0.1},
		{name: "progress above one", field: "progress", value: 1.1},
		{name: "priority string", field: "priority", value: "1"},
		{name: "is seed number", field: "is_seed", value: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			row := validJobFileRow(0, "x")
			row[test.field] = test.value
			if _, _, err := decodeJobFileTestBody(t, marshalJobFileRows(t, row), downloader.DefaultJobFileLedgerLimits()); err == nil {
				t.Fatalf("accepted %s=%v", test.field, test.value)
			}
		})
	}
	for _, body := range [][]byte{
		[]byte(`[ {"index":9223372036854775808,"name":"x","size":1,"progress":1,"priority":1,"is_seed":true} ]`),
		[]byte(`[ {"index":0,"name":"x","size":1,"progress":1e400,"priority":1,"is_seed":true} ]`),
	} {
		if _, _, err := decodeJobFileTestBody(t, body, downloader.DefaultJobFileLedgerLimits()); err == nil {
			t.Fatalf("accepted numeric boundary case: %s", body)
		}
	}
}

func TestJobFilePriorityNormalization(t *testing.T) {
	for _, test := range []struct {
		priority  int
		selection downloader.JobFileSelection
	}{
		{priority: 0, selection: downloader.JobFileSelectionSkipped},
		{priority: 1, selection: downloader.JobFileSelectionSelected},
		{priority: 6, selection: downloader.JobFileSelectionSelected},
		{priority: 7, selection: downloader.JobFileSelectionSelected},
	} {
		row := validJobFileRow(0, "x")
		row["priority"] = test.priority
		files, _, err := decodeJobFileTestBody(t, marshalJobFileRows(t, row), downloader.DefaultJobFileLedgerLimits())
		if err != nil || len(files) != 1 || files[0].Selection != test.selection {
			t.Fatalf("priority %d: files=%#v err=%v", test.priority, files, err)
		}
	}
	for _, priority := range []int{-1, 2, 5, 8} {
		row := validJobFileRow(0, "x")
		row["priority"] = priority
		if _, _, err := decodeJobFileTestBody(t, marshalJobFileRows(t, row), downloader.DefaultJobFileLedgerLimits()); err == nil {
			t.Fatalf("accepted priority %d", priority)
		}
	}
}

func TestJobFileDuplicateFieldsIndexesAndPaths(t *testing.T) {
	duplicateField := []byte(`[ {"index":0,"name":"x","name":"y","size":1,"progress":1,"priority":1,"is_seed":true} ]`)
	if _, _, err := decodeJobFileTestBody(t, duplicateField, downloader.DefaultJobFileLedgerLimits()); err == nil {
		t.Fatal("accepted duplicate field")
	}
	first := validJobFileRow(0, "a/x")
	second := validJobFileRow(0, "b/x")
	if _, _, err := decodeJobFileTestBody(t, marshalJobFileRows(t, first, second), downloader.DefaultJobFileLedgerLimits()); err == nil {
		t.Fatal("accepted duplicate index")
	}
	second = validJobFileRow(1, "a/x")
	if _, _, err := decodeJobFileTestBody(t, marshalJobFileRows(t, first, second), downloader.DefaultJobFileLedgerLimits()); err == nil {
		t.Fatal("accepted duplicate effective path")
	}
}

func TestJobFilePathsAreStrictRelativeSlashComponents(t *testing.T) {
	invalid := []string{
		"", "/absolute", "C:/absolute", `a\b`, "a//b", ".", "..", "a/./b", "a/../b", "a/\x00b", "a/\nb", "a/\u007fb", "a/�",
		strings.Repeat("x/", hardMaxJobFilePathDepth) + "x",
		strings.Repeat("x", hardMaxJobFilePathBytes+1),
	}
	for _, name := range invalid {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			row := validJobFileRow(0, name)
			if _, _, err := decodeJobFileTestBody(t, marshalJobFileRows(t, row), downloader.DefaultJobFileLedgerLimits()); err == nil {
				t.Fatalf("accepted invalid path %q", name)
			}
		})
	}
	row := validJobFileRow(0, "目录/😀.bin")
	files, _, err := decodeJobFileTestBody(t, marshalJobFileRows(t, row), downloader.DefaultJobFileLedgerLimits())
	if err != nil || !slices.Equal(files[0].RelativeComponents, []string{"目录", "😀.bin"}) {
		t.Fatalf("valid Unicode relative path failed: %#v %v", files, err)
	}
}

func TestStrictJSONStringsRejectInvalidEncodingAndUnpairedSurrogates(t *testing.T) {
	valid := marshalJobFileRows(t, validJobFileRow(0, "safe"))
	invalidUTF8 := bytes.Replace(valid, []byte("safe"), []byte{0xff}, 1)
	if _, _, err := decodeJobFileTestBody(t, invalidUTF8, downloader.DefaultJobFileLedgerLimits()); err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
	for _, quoted := range []string{`"\ud800"`, `"\udc00"`, `"\ud800\u0041"`} {
		body := rawJobFileNameBody(quoted)
		if _, _, err := decodeJobFileTestBody(t, body, downloader.DefaultJobFileLedgerLimits()); err == nil {
			t.Fatalf("accepted unpaired surrogate %s", quoted)
		}
	}
	files, _, err := decodeJobFileTestBody(t, rawJobFileNameBody(`"emoji-\ud83d\ude00"`), downloader.DefaultJobFileLedgerLimits())
	if err != nil || files[0].RelativeComponents[0] != "emoji-😀" {
		t.Fatalf("valid surrogate pair failed: %#v %v", files, err)
	}
	badKey := []byte(`[ {"\ud800":0,"index":0,"name":"x","size":1,"progress":1,"priority":1,"is_seed":true} ]`)
	if _, _, err := decodeJobFileTestBody(t, badKey, downloader.DefaultJobFileLedgerLimits()); err == nil {
		t.Fatal("accepted unpaired surrogate in field key")
	}
}

func TestRawTorrentStrictStringsCoverEveryStringFieldAndKeys(t *testing.T) {
	base := `{"hash":"opaque","magnet_uri":"","name":"name","size":1,"progress":1,"state":"uploading","save_path":"/save","content_path":"/save/name","downloaded":1,"uploaded":1}`
	values := map[string]string{
		"hash":         "opaque",
		"magnet_uri":   "",
		"name":         "name",
		"state":        "uploading",
		"save_path":    "/save",
		"content_path": "/save/name",
	}
	for field, value := range values {
		body := strings.Replace(base, fmt.Sprintf(`"%s":"%s"`, field, value), fmt.Sprintf(`"%s":"\ud800"`, field), 1)
		var item rawTorrent
		if err := json.Unmarshal([]byte(body), &item); err == nil {
			t.Fatalf("accepted unpaired surrogate in rawTorrent %s", field)
		}
	}
	withBadKey := strings.Replace(base, "{", `{"\ud800":0,`, 1)
	var item rawTorrent
	if err := json.Unmarshal([]byte(withBadKey), &item); err == nil {
		t.Fatal("accepted unpaired surrogate in rawTorrent field key")
	}
}

func TestJobFileLedgerBudgetsStopDeterministically(t *testing.T) {
	limits := downloader.DefaultJobFileLedgerLimits()
	limits.MaxFiles = 1
	first := string(marshalJobFileRows(t, validJobFileRow(0, "a")))
	first = strings.TrimSuffix(strings.TrimPrefix(first, "["), "]")
	body := []byte("[" + first + `,{"index":1,"name":"\ud800","size":1,"progress":1,"priority":1,"is_seed":true}]`)
	_, used, err := decodeJobFileTestBody(t, body, limits)
	if err == nil || used.FilesConsidered != 1 || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("file N+1 was decoded or mischarged: used=%#v err=%v", used, err)
	}

	limits = downloader.DefaultJobFileLedgerLimits()
	limits.MaxPathBytes = 3
	_, used, err = decodeJobFileTestBody(t, marshalJobFileRows(t, validJobFileRow(0, "abc"), validJobFileRow(1, "d")), limits)
	if err == nil || used.FilesConsidered != 2 || used.PathBytes != 3 {
		t.Fatalf("path budget usage=%#v err=%v", used, err)
	}

	tooManyFields := validJobFileRow(0, "x")
	extraFields := hardMaxJobFileFields - len(tooManyFields) + 1
	for index := 0; index < extraFields; index++ {
		tooManyFields[fmt.Sprintf("unknown_%02d", index)] = index
	}
	_, used, err = decodeJobFileTestBody(t, marshalJobFileRows(t, tooManyFields), downloader.DefaultJobFileLedgerLimits())
	if err == nil || used.FilesConsidered != 1 || !strings.Contains(err.Error(), "too many fields") {
		t.Fatalf("field limit usage=%#v err=%v", used, err)
	}

	nested := "0"
	for index := 0; index < hardMaxJobFileJSONDepth; index++ {
		nested = "[" + nested + "]"
	}
	deep := []byte(fmt.Sprintf(`[{"index":0,"name":"x","size":1,"progress":1,"priority":1,"is_seed":true,"unknown":%s}]`, nested))
	if _, _, err := decodeJobFileTestBody(t, deep, downloader.DefaultJobFileLedgerLimits()); err == nil {
		t.Fatal("accepted over-depth row")
	}

	largeName := strings.Repeat("x", hardMaxJobFileRowBytes)
	if _, _, err := decodeJobFileTestBody(t, marshalJobFileRows(t, validJobFileRow(0, largeName)), downloader.JobFileLedgerLimits{MaxFiles: 1, MaxPathBytes: 64 << 20, MaxResponseBytes: 32 << 20}); err == nil {
		t.Fatal("accepted oversized row")
	}
}

func TestReadJobFilesResponseBudgetReturnsIncompleteUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/auth/login" {
			_, _ = w.Write([]byte("Ok."))
			return
		}
		_, _ = w.Write([]byte("[]" + strings.Repeat(" ", 100)))
	}))
	defer server.Close()
	session := openJobFileTestSession(t, server.URL)
	defer session.Close()
	limits := downloader.JobFileLedgerLimits{MaxFiles: 1, MaxPathBytes: 1, MaxResponseBytes: 32}
	snapshot, err := session.ReadJobFiles(context.Background(), "opaque", limits)
	if err == nil || snapshot.Complete || len(snapshot.Files) != 0 || snapshot.Used.ResponseBytes != 33 || session.RequestsMade() != 2 {
		t.Fatalf("snapshot=%#v requests=%d err=%v", snapshot, session.RequestsMade(), err)
	}
}

func TestReadJobFilesCancellationRequestAccounting(t *testing.T) {
	var fileRequests atomic.Int32
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/auth/login" {
			_, _ = w.Write([]byte("Ok."))
			return
		}
		fileRequests.Add(1)
		_, _ = w.Write([]byte("["))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	session := openJobFileTestSession(t, server.URL)
	defer session.Close()

	preCancelled, cancel := context.WithCancel(context.Background())
	cancel()
	snapshot, err := session.ReadJobFiles(preCancelled, "opaque", downloader.DefaultJobFileLedgerLimits())
	if !errors.Is(err, context.Canceled) || snapshot.Complete || fileRequests.Load() != 0 || session.RequestsMade() != 1 {
		t.Fatalf("pre-cancel snapshot=%#v requests=%d/%d err=%v", snapshot, fileRequests.Load(), session.RequestsMade(), err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		snapshot downloader.JobFileLedgerSnapshot
		err      error
	}, 1)
	go func() {
		snapshot, err := session.ReadJobFiles(ctx, "opaque", downloader.DefaultJobFileLedgerLimits())
		result <- struct {
			snapshot downloader.JobFileLedgerSnapshot
			err      error
		}{snapshot: snapshot, err: err}
	}()
	<-started
	cancel()
	got := <-result
	if !errors.Is(got.err, context.Canceled) || got.snapshot.Complete || fileRequests.Load() != 1 || session.RequestsMade() != 2 {
		t.Fatalf("mid-read snapshot=%#v requests=%d/%d err=%v", got.snapshot, fileRequests.Load(), session.RequestsMade(), got.err)
	}
}

func TestReadJobFilesErrorsNeverExposeResponseOrOpaqueKey(t *testing.T) {
	const canary = "CANARY-REMOTE-FILE-BODY"
	const opaque = "CANARY-OPAQUE-JOB-KEY"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/auth/login" {
			_, _ = w.Write([]byte("Ok."))
			return
		}
		http.Error(w, canary, http.StatusInternalServerError)
	}))
	defer server.Close()
	session := openJobFileTestSession(t, server.URL)
	defer session.Close()
	snapshot, err := session.ReadJobFiles(context.Background(), opaque, downloader.DefaultJobFileLedgerLimits())
	encoded, marshalErr := json.Marshal(snapshot)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if err == nil || strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), opaque) || bytes.Contains(encoded, []byte(canary)) || bytes.Contains(encoded, []byte(opaque)) {
		t.Fatalf("secret-bearing response escaped: err=%v snapshot=%s", err, encoded)
	}
}

func TestReadJobFilesTransportErrorNeverExposesRequestURL(t *testing.T) {
	const opaque = "CANARY-OPAQUE-TRANSPORT-KEY"
	adapter, err := New("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	session := &readSession{
		adapter: adapter,
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("CANARY-TRANSPORT-ERROR %s", request.URL.String())
		})},
		transport: &http.Transport{},
	}
	snapshot, err := session.ReadJobFiles(context.Background(), opaque, downloader.DefaultJobFileLedgerLimits())
	encoded, marshalErr := json.Marshal(snapshot)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if err == nil || strings.Contains(err.Error(), opaque) || strings.Contains(err.Error(), "CANARY-TRANSPORT-ERROR") || strings.Contains(err.Error(), "127.0.0.1") || bytes.Contains(encoded, []byte(opaque)) || session.RequestsMade() != 1 {
		t.Fatalf("transport URL escaped: err=%v snapshot=%s requests=%d", err, encoded, session.RequestsMade())
	}
}

func TestReadSessionDoesNotTransparentlyRetryReplayableGET(t *testing.T) {
	var ledgerRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/auth/login":
			_, _ = writer.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			attempt := ledgerRequests.Add(1)
			if attempt == 1 {
				hijacker, ok := writer.(http.Hijacker)
				if !ok {
					t.Error("test server cannot hijack the connection")
					return
				}
				connection, _, err := hijacker.Hijack()
				if err != nil {
					t.Errorf("hijack connection: %v", err)
					return
				}
				_ = connection.Close()
				return
			}
			_, _ = writer.Write([]byte("[]"))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	session := openJobFileTestSession(t, server.URL)
	defer session.Close()
	_, err := session.ReadLedger(context.Background())
	if err == nil || ledgerRequests.Load() != 1 || session.RequestsMade() != 2 {
		t.Fatalf("replayable GET was retried or miscounted: requests=%d session=%d err=%v", ledgerRequests.Load(), session.RequestsMade(), err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func openJobFileTestSession(t *testing.T, endpoint string) downloader.LedgerSession {
	t.Helper()
	adapter, err := New(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := downloader.NewCredential("alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	session, err := adapter.OpenReadSession(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func validJobFileRow(index int, name string) map[string]any {
	return map[string]any{
		"index": index, "name": name, "size": int64(1), "progress": 1.0,
		"priority": 1, "is_seed": true,
	}
}

func marshalJobFileRows(t *testing.T, rows ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func decodeJobFileTestBody(t *testing.T, body []byte, limits downloader.JobFileLedgerLimits) ([]downloader.JobFile, downloader.JobFileLedgerUsage, error) {
	t.Helper()
	var used downloader.JobFileLedgerUsage
	files, err := decodeJobFileLedger(context.Background(), bytes.NewReader(body), limits, &used)
	return files, used, err
}

func rawJobFileNameBody(quotedName string) []byte {
	return []byte(fmt.Sprintf(`[{"index":0,"name":%s,"size":1,"progress":1,"priority":1,"is_seed":true}]`, quotedName))
}
