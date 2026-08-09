package qbittorrent

import (
	"context"
	"encoding/base32"
	"encoding/hex"
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

func TestEndpointPolicy(t *testing.T) {
	allowed := []string{"https://seedbox.example", "http://127.0.0.1:8080", "http://[::1]:8080"}
	for _, endpoint := range allowed {
		if _, err := New(endpoint); err != nil {
			t.Fatalf("%s should be allowed: %v", endpoint, err)
		}
	}
	blocked := []string{"http://localhost:8080", "http://192.168.1.2:8080", "http://seedbox.example", "https://user:pass@example.com", "https://example.com/private-token", "ftp://example.com"}
	for _, endpoint := range blocked {
		if _, err := New(endpoint); err == nil {
			t.Fatalf("%s should be blocked", endpoint)
		}
	}
	const endpointCanary = "ENDPOINT-SECRET-CANARY"
	if _, err := New("https://example.invalid/%zz" + endpointCanary); err == nil || strings.Contains(err.Error(), endpointCanary) {
		t.Fatalf("malformed endpoint error leaked input: %v", err)
	}
}

func TestStatusAuthenticatesAndReadsVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			if r.Method != http.MethodPost || r.FormValue("username") != "alice" || r.FormValue("password") != "secret" {
				http.Error(w, "bad login", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/", HttpOnly: true})
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/app/version":
			if _, err := r.Cookie("SID"); err != nil {
				http.Error(w, "missing session", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("v5.0.0"))
		case "/api/v2/app/webapiVersion":
			_, _ = w.Write([]byte("2.11.0"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := downloader.NewCredential("alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	status, err := adapter.Status(context.Background(), credential)
	if err != nil || status.Version != "v5.0.0" || status.WebAPIVersion != "2.11.0" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestLoginBodyIsNotReplayedAcrossOriginRedirect(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests.Add(1)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	adapter, err := New(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := downloader.NewCredential("alice", "CANARY-QBIT-PASSWORD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Status(context.Background(), credential); err == nil {
		t.Fatal("expected cross-origin redirect rejection")
	} else if strings.Contains(err.Error(), "CANARY-QBIT-PASSWORD") {
		t.Fatalf("credential leaked through redirect error: %v", err)
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("redirect target received %d requests", targetRequests.Load())
	}
}

func TestSameOriginRedirectCannotHideExtraRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, "/redirect-target", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	adapter, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := downloader.NewCredential("alice", "CANARY-QBIT-PASSWORD")
	if err != nil {
		t.Fatal(err)
	}
	session, openErr := adapter.OpenReadSession(context.Background(), credential)
	if openErr == nil || session != nil {
		t.Fatal("redirected login was accepted")
	}
	if requests.Load() != 1 {
		t.Fatalf("redirect caused %d requests, want exactly one attempted login", requests.Load())
	}
	if got, ok := downloader.RequestsMadeFromError(openErr); !ok || got != 1 {
		t.Fatalf("redirect request count=(%d, %t), want (1, true)", got, ok)
	}
}

func TestOpenReadSessionErrorRequestCounts(t *testing.T) {
	credential, err := downloader.NewCredential("alice", "CANARY-QBIT-PASSWORD")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("pre-cancelled context makes no request", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			requests.Add(1)
		}))
		defer server.Close()
		adapter, err := New(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		session, openErr := adapter.OpenReadSession(ctx, credential)
		if openErr == nil {
			t.Fatal("pre-cancelled open succeeded")
		}
		if session != nil {
			t.Fatal("failed open returned a session")
		}
		if !errors.Is(openErr, context.Canceled) {
			t.Fatalf("open error lost cancellation cause: %v", openErr)
		}
		if got, ok := downloader.RequestsMadeFromError(openErr); !ok || got != 0 {
			t.Fatalf("request count=(%d, %t), want (0, true)", got, ok)
		}
		if requests.Load() != 0 {
			t.Fatalf("pre-cancelled open made %d requests", requests.Load())
		}
	})

	t.Run("rejected login counts one request without leaking response", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if r.URL.Path != "/api/v2/auth/login" {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "CANARY-QBIT-LOGIN-RESPONSE", http.StatusUnauthorized)
		}))
		defer server.Close()
		adapter, err := New(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		session, openErr := adapter.OpenReadSession(context.Background(), credential)
		if openErr == nil {
			t.Fatal("rejected login succeeded")
		}
		if session != nil {
			t.Fatal("failed open returned a session")
		}
		if got, ok := downloader.RequestsMadeFromError(openErr); !ok || got != 1 {
			t.Fatalf("request count=(%d, %t), want (1, true)", got, ok)
		}
		if requests.Load() != 1 {
			t.Fatalf("login made %d server requests, want 1", requests.Load())
		}
		if strings.Contains(openErr.Error(), "CANARY") {
			t.Fatalf("login error leaked credential or response: %v", openErr)
		}
	})

	t.Run("network login failure counts one attempt", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		endpoint := server.URL
		server.Close()
		adapter, err := New(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		session, openErr := adapter.OpenReadSession(context.Background(), credential)
		if openErr == nil {
			t.Fatal("network failure unexpectedly succeeded")
		}
		if session != nil {
			t.Fatal("failed open returned a session")
		}
		if got, ok := downloader.RequestsMadeFromError(openErr); !ok || got != 1 {
			t.Fatalf("request count=(%d, %t), want (1, true)", got, ok)
		}
		if strings.Contains(openErr.Error(), "CANARY-QBIT-PASSWORD") {
			t.Fatalf("network error leaked credential: %v", openErr)
		}
	})

	if got, ok := downloader.RequestsMadeFromError(errors.New("ordinary error")); ok || got != 0 {
		t.Fatalf("ordinary error request count=(%d, %t), want (0, false)", got, ok)
	}
}

func TestParseMagnetIdentityMatrix(t *testing.T) {
	v1Bytes, err := hex.DecodeString(strings.Repeat("ab", 20))
	if err != nil {
		t.Fatal(err)
	}
	v1Hex := hex.EncodeToString(v1Bytes)
	v1Base32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(v1Bytes)
	v1Other := strings.Repeat("cd", 20)
	v2Hex := strings.Repeat("12", 32)
	v2Other := strings.Repeat("34", 32)

	tests := []struct {
		name     string
		magnet   string
		status   downloader.IdentityStatus
		v1       string
		v2       string
		evidence []string
		issues   []string
	}{
		{name: "missing", status: downloader.IdentityStatusUnavailable, evidence: []string{}, issues: []string{}},
		{name: "unknown xt ignored", magnet: "magnet:?xt=urn:example:opaque&tr=https%3A%2F%2FCANARY.invalid", status: downloader.IdentityStatusUnavailable, evidence: []string{}, issues: []string{}},
		{name: "btih hex", magnet: "magnet:?xt=urn:btih:" + strings.ToUpper(v1Hex), status: downloader.IdentityStatusValid, v1: v1Hex, evidence: []string{evidenceMagnetBTIHHex}, issues: []string{}},
		{name: "btih base32", magnet: "magnet:?xt=urn:btih:" + strings.ToLower(v1Base32), status: downloader.IdentityStatusValid, v1: v1Hex, evidence: []string{evidenceMagnetBTIHBase32}, issues: []string{}},
		{name: "btmh sha256", magnet: "magnet:?xt=urn:btmh:1220" + strings.ToUpper(v2Hex), status: downloader.IdentityStatusValid, v2: v2Hex, evidence: []string{evidenceMagnetBTMHSHA256}, issues: []string{}},
		{name: "both families", magnet: "magnet:?xt=urn:btih:" + v1Hex + "&xt=urn:btmh:1220" + v2Hex, status: downloader.IdentityStatusValid, v1: v1Hex, v2: v2Hex, evidence: []string{evidenceMagnetBTIHHex, evidenceMagnetBTMHSHA256}, issues: []string{}},
		{name: "percent encoded xt", magnet: "magnet:?xt=urn%3Abtih%3A" + v1Hex, status: downloader.IdentityStatusValid, v1: v1Hex, evidence: []string{evidenceMagnetBTIHHex}, issues: []string{}},
		{name: "percent encoded xt key", magnet: "magnet:?%78t=urn:btih:" + v1Hex, status: downloader.IdentityStatusValid, v1: v1Hex, evidence: []string{evidenceMagnetBTIHHex}, issues: []string{}},
		{name: "same duplicate deduplicated", magnet: "magnet:?xt=urn:btih:" + v1Hex + "&xt=urn:btih:" + v1Hex, status: downloader.IdentityStatusValid, v1: v1Hex, evidence: []string{evidenceMagnetBTIHHex}, issues: []string{}},
		{name: "conflicting btih", magnet: "magnet:?xt=urn:btih:" + v1Hex + "&xt=urn:btih:" + v1Other, status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetBTIHConflict}},
		{name: "case variant key cannot hide conflict", magnet: "magnet:?xt=urn:btih:" + v1Hex + "&XT=urn:btih:" + v1Other, status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetBTIHConflict}},
		{name: "encoded case variant key cannot hide conflict", magnet: "magnet:?xt=urn:btih:" + v1Hex + "&%58%54=urn:btih:" + v1Other, status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetBTIHConflict}},
		{name: "conflicting btmh", magnet: "magnet:?xt=urn:btmh:1220" + v2Hex + "&xt=urn:btmh:1220" + v2Other, status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetBTMHConflict}},
		{name: "supported malformed btih poisons row", magnet: "magnet:?xt=urn:btih:not-a-hash&xt=urn:btmh:1220" + v2Hex, status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetBTIH}},
		{name: "supported malformed btmh poisons row", magnet: "magnet:?xt=urn:btmh:1114" + v2Hex, status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetBTMH}},
		{name: "malformed xt escape", magnet: "magnet:?xt=%zz", status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetFormat}},
		{name: "malformed unrelated escape rejects URI", magnet: "magnet:?xt=urn:btih:" + v1Hex + "&tr=%zzCANARY", status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetFormat}},
		{name: "raw whitespace rejects URI", magnet: "magnet:?xt=urn:btih:" + v1Hex + " &tr=CANARY", status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetFormat}},
		{name: "semicolon query separator rejected", magnet: "magnet:?xt=urn:btih:" + v1Hex + ";XT=urn:btih:" + v1Other, status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetFormat}},
		{name: "trailing empty query pair rejected", magnet: "magnet:?xt=urn:btih:" + v1Hex + "&", status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetFormat}},
		{name: "decoded leading whitespace poisons row", magnet: "magnet:?xt=%20urn:btih:" + v1Other + "&xt=urn:btih:" + v1Hex, status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetXTUnsafe}},
		{name: "plus whitespace poisons row", magnet: "magnet:?xt=+urn:btih:" + v1Other + "&xt=urn:btih:" + v1Hex, status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetXTUnsafe}},
		{name: "unicode whitespace poisons row", magnet: "magnet:?xt=%C2%A0urn:btih:" + v1Other + "&xt=urn:btih:" + v1Hex, status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetXTUnsafe}},
		{name: "unsafe decoded query key poisons row", magnet: "magnet:?%00xt=urn:btih:" + v1Other + "&xt=urn:btih:" + v1Hex, status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetFormat}},
		{name: "raw non ascii query rejects URI", magnet: "magnet:?dn=\u00a0&xt=urn:btih:" + v1Hex, status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetFormat}},
		{name: "not magnet", magnet: "https://CANARY.invalid/?xt=urn:btih:" + v1Hex, status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetFormat}},
		{name: "fragment rejected", magnet: "magnet:?xt=urn:btih:" + v1Hex + "#CANARY", status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetFormat}},
		{name: "too many pairs", magnet: "magnet:?" + strings.TrimSuffix(strings.Repeat("x=1&", maxMagnetPairs+1), "&"), status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetTooManyPairs}},
		{name: "too many xt", magnet: "magnet:?" + strings.TrimSuffix(strings.Repeat("xt=urn:example:x&", maxMagnetXT+1), "&"), status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetTooManyXT}},
		{name: "oversized xt", magnet: "magnet:?xt=" + strings.Repeat("x", maxMagnetXTBytes+1), status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetXTTooLarge}},
		{name: "oversized magnet", magnet: "magnet:?" + strings.Repeat("x", maxMagnetBytes), status: downloader.IdentityStatusInvalid, evidence: []string{}, issues: []string{issueMagnetTooLarge}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := parseMagnetIdentity(test.magnet)
			if got.status != test.status || got.v1 != test.v1 || got.v2 != test.v2 || !slices.Equal(got.evidence, test.evidence) || !slices.Equal(got.issues, test.issues) {
				t.Fatalf("identity=%#v, want status=%q v1=%q v2=%q evidence=%v issues=%v", got, test.status, test.v1, test.v2, test.evidence, test.issues)
			}
		})
	}
}

func TestOpaqueJobKeyIsNeverInferredAsAnInfoHash(t *testing.T) {
	const opaque = "0123456789abcdef0123456789abcdef01234567"
	job, err := normalizeTorrent(0, rawTorrent{Hash: opaque, Name: "job"})
	if err != nil {
		t.Fatal(err)
	}
	if job.Hash != opaque || job.InfoHashV1 != "" || job.InfoHashV2 != "" || job.IdentityStatus != downloader.IdentityStatusUnavailable || len(job.IdentityEvidence) != 0 || len(job.IdentityIssues) != 0 {
		t.Fatalf("opaque job key was assigned hash semantics: %#v", job)
	}
}

func TestReadLedgerReusesLoginParsesTypedFieldsAndSorts(t *testing.T) {
	const (
		v1 = "0123456789abcdef0123456789abcdef01234567"
		v2 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "synthetic", Path: "/", HttpOnly: true})
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			if _, err := r.Cookie("SID"); err != nil {
				http.Error(w, "missing session", http.StatusUnauthorized)
				return
			}
			_, _ = fmt.Fprintf(w, `[
				{"hash":"z-opaque","name":"second","size":2,"progress":0.5,"state":"downloading","save_path":"/save","content_path":"/save/second","downloaded":1,"uploaded":0,"magnet_uri":"magnet:?xt=urn:btmh:1220%s&tr=https%%3A%%2F%%2FCANARY-TRACKER.invalid"},
				{"hash":"a-opaque","name":"first","size":1,"progress":1,"state":"uploading","save_path":"/save","content_path":"/save/first","downloaded":1,"uploaded":3,"magnet_uri":"magnet:?xt=urn:btih:%s"}
			]`, v2, v1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter, err := New(server.URL)
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
	if session.RequestsMade() != 1 {
		t.Fatalf("login request count=%d", session.RequestsMade())
	}
	first, err := session.ReadLedger(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.ReadLedger(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 || session.RequestsMade() != 3 {
		t.Fatalf("requests server=%d session=%d", requests.Load(), session.RequestsMade())
	}
	if !first.Complete || first.Driver != "qbittorrent" || first.ObservedAtStart.IsZero() || first.ObservedAtEnd.Before(first.ObservedAtStart) || !first.Capabilities.TypedInfoHashes || !first.Capabilities.ContentPath || first.Capabilities.RawMetafile {
		t.Fatalf("invalid ledger metadata: %#v", first)
	}
	if len(first.Jobs) != 2 || first.Jobs[0].Hash != "a-opaque" || first.Jobs[1].Hash != "z-opaque" || first.Jobs[0].InfoHashV1 != v1 || first.Jobs[1].InfoHashV2 != v2 || first.Jobs[0].ContentPath != "/save/first" || first.Jobs[1].IdentityStatus != downloader.IdentityStatusValid {
		t.Fatalf("unexpected normalized jobs: %#v", first.Jobs)
	}
	firstJSON, err := json.Marshal(first.Jobs)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second.Jobs)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) || strings.Contains(string(firstJSON), "CANARY") || strings.Contains(string(firstJSON), "magnet_uri") || strings.Contains(string(firstJSON), "tracker") {
		t.Fatalf("ledger was unstable or leaked raw URI data: %s", firstJSON)
	}
	beforeClose := requests.Load()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != beforeClose {
		t.Fatal("Close performed a network request")
	}
	if _, err := session.ReadLedger(context.Background()); err == nil {
		t.Fatal("closed session accepted another read")
	}
}

func TestReadLedgerCancellationAndNoRetry(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/api/v2/auth/login" {
			_, _ = w.Write([]byte("Ok."))
			return
		}
		http.Error(w, "CANARY-TRACKER-BODY", http.StatusInternalServerError)
	}))
	defer server.Close()
	adapter, err := New(server.URL)
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
	defer session.Close()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.ReadLedger(cancelled); err == nil {
		t.Fatal("cancelled read succeeded")
	}
	if requests.Load() != 1 || session.RequestsMade() != 1 {
		t.Fatalf("cancelled read made a request: server=%d session=%d", requests.Load(), session.RequestsMade())
	}
	if _, err := session.ReadLedger(context.Background()); err == nil {
		t.Fatal("HTTP failure was accepted")
	} else if strings.Contains(err.Error(), "CANARY-TRACKER-BODY") {
		t.Fatalf("response body leaked through error: %v", err)
	}
	if requests.Load() != 2 || session.RequestsMade() != 2 {
		t.Fatalf("read retried: server=%d session=%d", requests.Load(), session.RequestsMade())
	}
}

func TestReadLedgerBodyAndRowValidationBudgets(t *testing.T) {
	credential, err := downloader.NewCredential("alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "oversized body", body: strings.Repeat(" ", int(maxTorrentListBody)+1)},
		{name: "invalid utf8 body", body: string([]byte{'[', '{', '"', 'x', '"', ':', '"', 0xff, '"', '}', ']'})},
		{name: "invalid progress", body: `[{"hash":"opaque","name":"x","progress":2}]`},
		{name: "missing opaque key", body: `[{"name":"x","progress":0}]`},
		{name: "duplicate opaque key", body: `[{"hash":"opaque","name":"x"},{"hash":"opaque","name":"y"}]`},
		{name: "duplicate identity field", body: `[{"hash":"opaque","name":"x","magnet_uri":"magnet:?xt=urn:btih:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","magnet_uri":"magnet:?xt=urn:btih:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]`},
		{name: "duplicate path field", body: `[{"hash":"opaque","name":"x","content_path":"/safe","content_path":"/other"}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v2/auth/login" {
					_, _ = w.Write([]byte("Ok."))
					return
				}
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			adapter, err := New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			session, err := adapter.OpenReadSession(context.Background(), credential)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if _, err := session.ReadLedger(context.Background()); err == nil {
				t.Fatal("invalid ledger was accepted")
			}
		})
	}
}

func TestDecodeTorrentLedgerAppliesJobLimitBeforeDecodingNextRow(t *testing.T) {
	body := []byte(`[
		{"hash":"one","name":"one"},
		{"hash":"two","name":"two"},
		{"hash":"CANARY-THIRD-ROW","name":false}
	]`)
	if _, err := decodeTorrentLedgerLimited(body, 2); err == nil || !strings.Contains(err.Error(), "too many torrents") || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("streaming limit was not enforced before row N+1: %v", err)
	}
}

func TestTorrentObjectFieldBudgetIsHardBound(t *testing.T) {
	var body strings.Builder
	body.WriteString(`[{"hash":"opaque","name":"job"`)
	for index := 0; index < maxTorrentFields-1; index++ {
		_, _ = fmt.Fprintf(&body, `,"u%d":0`, index)
	}
	body.WriteString("}]")
	if _, err := decodeTorrentLedger([]byte(body.String())); err == nil || !strings.Contains(err.Error(), "too many fields") {
		t.Fatalf("object field limit was not enforced: %v", err)
	}
}

func TestUnknownTorrentStateIsNeverRetainedVerbatim(t *testing.T) {
	const canary = "CANARY-QBIT-STATE-SECRET"
	job, err := normalizeTorrent(0, rawTorrent{Hash: "opaque", Name: "job", State: canary})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "unknown" || strings.Contains(string(encoded), canary) {
		t.Fatalf("unknown state escaped normalization: %s", encoded)
	}
}
