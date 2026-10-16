package mteam

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/httpguard"
)

// detailFixture mirrors the shape api/torrent/detail returned for a public
// software torrent; every value is synthetic.
const detailFixture = `{"code":"0","message":"SUCCESS","data":{
	"id":"900001","createdDate":"2026-10-01 20:30:00","name":"Canary.Tool.2026.win+linux","smallDescr":"",
	"author":"CANARY","category":"422","numfiles":"3","size":"53558152","anonymous":false,"infoHash":null,
	"status":{"id":"900001","discount":"NORMAL","discountEndTime":null,"timesCompleted":"322","views":"100","hits":"1",
		"seeders":"24","leechers":"0","banned":false,"visible":true},
	"descr":"synthetic description","originFileName":"canary.torrent"}}`

func observedJSON(body string) httpguard.StrictResponse {
	start := time.Date(2026, 10, 16, 6, 0, 0, 0, time.UTC)
	return httpguard.StrictResponse{
		StatusCode: http.StatusOK, MediaType: "application/json", Body: []byte(body),
		ObservedAtStart: start, ObservedAtEnd: start.Add(time.Second),
		ResponseBytesRead: int64(len(body)), ResponseBytesKnown: true,
	}
}

func detailRef(id string) domain.TorrentRef {
	return domain.TorrentRef{SiteID: SiteID, RemoteID: id}
}

func openDetail(t *testing.T, client *fakeClient) site.TorrentDetailSession {
	t.Helper()
	adapter, record := fakeAdapter(client)
	session, err := adapter.OpenTorrentDetailSession(context.Background(), apiKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if record.opened != 1 || record.baseURL != DefaultBaseURL || record.apiKey != testAPIKey || record.interval != minRequestInterval {
		t.Fatalf("factory=%#v", record)
	}
	return session
}

func TestDetailConfigIsPinned(t *testing.T) {
	config, err := New().TorrentDetailConfig()
	if err != nil || config.Origin != "https://api2.m-team.cc" || config.RouteID != "mteam.torrent_detail_api.v1" {
		t.Fatalf("config=%#v err=%v", config, err)
	}
}

func TestDetailSendsOneFormPostAndMapsFields(t *testing.T) {
	client := &fakeClient{response: observedJSON(detailFixture)}
	session := openDetail(t, client)
	limits := site.DefaultTorrentDetailLimits()
	observed, receipt, err := session.ReadTorrentDetail(context.Background(), detailRef("900001"), limits)
	if err != nil {
		t.Fatalf("err=%v receipt=%#v", err, receipt)
	}
	want := fakeCall{
		path: "api/torrent/detail", contentType: "multipart/form-data; boundary=ptctlMTeamFormBoundary", accept: apiAccept,
		body:       "--ptctlMTeamFormBoundary\r\nContent-Disposition: form-data; name=\"id\"\r\n\r\n900001\r\n--ptctlMTeamFormBoundary--\r\n",
		maxRequest: maxRequestBytes, maxBody: limits.MaxResponseBytes, maxHeader: limits.MaxResponseHeaderBytes,
	}
	if len(client.calls) != 1 || client.calls[0] != want {
		t.Fatalf("calls=%#v", client.calls)
	}
	i := func(v int) *int { return &v }
	size := int64(53558152)
	wantDetail := domain.TorrentDetail{
		Ref: detailRef("900001"), DisplayTitle: "Canary.Tool.2026.win+linux",
		Seeders: i(24), Leechers: i(0), SizeBytes: &size, Snatches: i(322),
		EvidenceBasis: []string{site.DetailBasisAuthenticated, site.DetailBasisExactRef, site.DetailBasisOneRequest, site.DetailBasisSiteClaimOnly},
	}
	if got := observed.PublicCopy(); !reflect.DeepEqual(got, wantDetail) {
		t.Fatalf("detail=%#v", got)
	}
	if !receipt.Complete || receipt.StopReason != "" || receipt.Used.RequestsAttempted != 1 || receipt.Origin != Origin || receipt.RouteID != DetailRouteID ||
		!observed.MatchesReceipt(receipt) || !observed.Matches(detailRef("900001"), Origin, DetailRouteID) {
		t.Fatalf("receipt=%#v", receipt)
	}
	if err := session.Close(); err != nil || !client.closed {
		t.Fatalf("close err=%v closed=%v", err, client.closed)
	}
}

func TestDetailDropsMalformedCountersButRequiresTheIDEchoAndName(t *testing.T) {
	client := &fakeClient{response: observedJSON(`{"code":0,"data":{"id":900001,"name":" Canary ","size":"-1","status":{"seeders":"x","leechers":-2}}}`)}
	observed, _, err := openDetail(t, client).ReadTorrentDetail(context.Background(), detailRef("900001"), site.DefaultTorrentDetailLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got := observed.PublicCopy(); got.DisplayTitle != "Canary" || got.SizeBytes != nil || got.Seeders != nil || got.Leechers != nil || got.Snatches != nil {
		t.Fatalf("detail=%#v", got)
	}
	for _, body := range []string{
		`{"code":"0","data":{"id":"900002","name":"Canary"}}`,
		`{"code":"0","data":{"name":"Canary"}}`,
		`{"code":"0","data":{"id":"900001"}}`,
		`{"code":"0","data":{"id":"900001","name":"  "}}`,
		`{"code":"0","data":{"id":"900001","name":"Canary\u0007"}}`,
		`{"code":"0","data":null}`,
		`{"code":"0","data":[]}`,
		`{"code":"0"}`,
	} {
		client := &fakeClient{response: observedJSON(body)}
		_, receipt, err := openDetail(t, client).ReadTorrentDetail(context.Background(), detailRef("900001"), site.DefaultTorrentDetailLimits())
		if err == nil || receipt.StopReason != "unrecognized_response" || receipt.Complete || len(client.calls) != 1 {
			t.Fatalf("body=%s stop=%q err=%v", body, receipt.StopReason, err)
		}
	}
}

func TestDetailClassifiesRefusalsWithoutEchoingServerText(t *testing.T) {
	status := func(code int, mediaType, body string) httpguard.StrictResponse {
		response := observedJSON(body)
		response.StatusCode, response.MediaType = code, mediaType
		return response
	}
	tests := []struct {
		name     string
		response httpguard.StrictResponse
		stop     string
		cause    error
	}{
		{name: "not found", response: observedJSON(`{"code":"1","message":"種子未找到","data":null}`), stop: "not_found"},
		{name: "not found simplified", response: observedJSON(`{"code":"1","message":" 种子未找到 "}`), stop: "not_found"},
		{name: "other code 1 refusal", response: observedJSON(`{"code":"1","message":"CANARY-SERVER-TEXT","data":null}`), stop: "unrecognized_response"},
		{name: "not found text with data", response: observedJSON(`{"code":"1","message":"種子未找到","data":{"id":"900001"}}`), stop: "unrecognized_response"},
		{name: "not found text other code", response: observedJSON(`{"code":"2","message":"種子未找到"}`), stop: "unrecognized_response"},
		{name: "code 401", response: observedJSON(`{"code":"401","message":"CANARY-SERVER-TEXT"}`), stop: "authentication_required", cause: site.ErrCredentialRejected},
		{name: "code 4", response: observedJSON(`{"code":4,"message":"CANARY-SERVER-TEXT"}`), stop: "rate_limited", cause: site.ErrRateLimited},
		{name: "http 401", response: status(http.StatusUnauthorized, "application/json", `{}`), stop: "authentication_required", cause: site.ErrCredentialRejected},
		{name: "http 403", response: status(http.StatusForbidden, "text/html", `CANARY-SERVER-TEXT`), stop: "authentication_required", cause: site.ErrCredentialRejected},
		{name: "http 404", response: status(http.StatusNotFound, "text/html", `CANARY-SERVER-TEXT`), stop: "not_found"},
		{name: "http 429", response: status(http.StatusTooManyRequests, "application/json", `{}`), stop: "rate_limited", cause: site.ErrRateLimited},
		{name: "redirect", response: status(http.StatusFound, "text/html", `CANARY-SERVER-TEXT`), stop: "redirect_rejected"},
		{name: "server error", response: status(http.StatusBadGateway, "text/html", `CANARY-SERVER-TEXT`), stop: "http_status_rejected"},
		{name: "html", response: status(http.StatusOK, "text/html", `<html>CANARY-SERVER-TEXT</html>`), stop: "content_type_rejected"},
		{name: "invalid utf-8", response: observedJSON("{\"code\":\"0\",\"message\":\"\xff\"}"), stop: "invalid_text_encoding"},
		{name: "not json", response: observedJSON(`CANARY-SERVER-TEXT`), stop: "unrecognized_response"},
		{name: "no code", response: observedJSON(`{"message":"CANARY-SERVER-TEXT"}`), stop: "unrecognized_response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeClient{response: test.response}
			observed, receipt, err := openDetail(t, client).ReadTorrentDetail(context.Background(), detailRef("900001"), site.DefaultTorrentDetailLimits())
			if err == nil || observed != nil || receipt.StopReason != test.stop || receipt.Complete || len(client.calls) != 1 {
				t.Fatalf("stop=%q want %q err=%v", receipt.StopReason, test.stop, err)
			}
			if test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("err=%v does not wrap %v", err, test.cause)
			}
			if strings.Contains(err.Error(), "CANARY") || strings.Contains(err.Error(), "種子") || strings.Contains(err.Error(), "种子") {
				t.Fatalf("err leaked server text: %v", err)
			}
		})
	}
	client := &fakeClient{response: observedJSON("")}
	if _, receipt, err := openDetail(t, client).ReadTorrentDetail(context.Background(), detailRef("900001"), site.DefaultTorrentDetailLimits()); err == nil || receipt.StopReason != "empty_response" {
		t.Fatalf("empty stop=%q err=%v", receipt.StopReason, err)
	}
}

func TestDetailSessionAllowsOneRequestAndFailsClosed(t *testing.T) {
	limits := site.DefaultTorrentDetailLimits()
	client := &fakeClient{response: observedJSON(detailFixture)}
	session := openDetail(t, client)
	if _, _, err := session.ReadTorrentDetail(context.Background(), detailRef("900001"), limits); err != nil {
		t.Fatal(err)
	}
	if _, receipt, err := session.ReadTorrentDetail(context.Background(), detailRef("900001"), limits); err == nil || receipt.StopReason != "request_budget_exhausted" || len(client.calls) != 1 {
		t.Fatalf("second read stop=%q err=%v calls=%d", receipt.StopReason, err, len(client.calls))
	}

	failing := &fakeClient{err: errors.New("CANARY transport detail")}
	_, receipt, err := openDetail(t, failing).ReadTorrentDetail(context.Background(), detailRef("900001"), limits)
	if err == nil || receipt.StopReason != "site_request_failed" || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("transport stop=%q err=%v", receipt.StopReason, err)
	}
	canceled := &fakeClient{err: context.Canceled}
	if _, receipt, err := openDetail(t, canceled).ReadTorrentDetail(context.Background(), detailRef("900001"), limits); !errors.Is(err, context.Canceled) || receipt.StopReason != "context_done" {
		t.Fatalf("canceled stop=%q err=%v", receipt.StopReason, err)
	}

	unknownBytes := observedJSON(detailFixture)
	unknownBytes.ResponseBytesKnown = false
	if _, receipt, err := openDetail(t, &fakeClient{response: unknownBytes}).ReadTorrentDetail(context.Background(), detailRef("900001"), limits); err == nil || receipt.StopReason != "response_accounting_invalid" {
		t.Fatalf("accounting stop=%q err=%v", receipt.StopReason, err)
	}

	for _, test := range []struct {
		ref    domain.TorrentRef
		limits site.TorrentDetailLimits
		stop   string
	}{
		{ref: detailRef("0900001"), limits: limits, stop: "invalid_reference"},
		{ref: domain.TorrentRef{SiteID: "tjupt", RemoteID: "900001"}, limits: limits, stop: "invalid_reference"},
		{ref: detailRef("900001"), limits: site.TorrentDetailLimits{MaxRequests: 2, MaxResponseBytes: 1, MaxResponseHeaderBytes: 1}, stop: "invalid_limits"},
	} {
		client := &fakeClient{response: observedJSON(detailFixture)}
		if _, receipt, err := openDetail(t, client).ReadTorrentDetail(context.Background(), test.ref, test.limits); err == nil || receipt.StopReason != test.stop || len(client.calls) != 0 {
			t.Fatalf("ref=%#v stop=%q want %q calls=%d", test.ref, receipt.StopReason, test.stop, len(client.calls))
		}
	}
}

func TestDetailOpenRejectsForeignCredentialsBeforeAnyClient(t *testing.T) {
	cookie, err := site.NewCookieCredential("sid=CANARY")
	if err != nil {
		t.Fatal(err)
	}
	malformed, err := site.NewAPIKeyCredential("bad key CANARY")
	if err != nil {
		t.Fatal(err)
	}
	for _, credential := range []site.Credential{cookie, malformed} {
		adapter, record := fakeAdapter(&fakeClient{})
		if session, err := adapter.OpenTorrentDetailSession(context.Background(), credential); err == nil || session != nil || record.opened != 0 || strings.Contains(err.Error(), "CANARY") {
			t.Fatalf("method=%s session=%v err=%v opened=%d", credential.Method(), session, err, record.opened)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	adapter, record := fakeAdapter(&fakeClient{})
	if _, err := adapter.OpenTorrentDetailSession(ctx, apiKey(t)); !errors.Is(err, context.Canceled) || record.opened != 0 {
		t.Fatalf("canceled open err=%v opened=%d", err, record.opened)
	}
}
