package mteam

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/site"
)

var _ site.OptionTorrentSearcher = (*Adapter)(nil)

func TestSearchWithOptionsSendsThemInOneRequest(t *testing.T) {
	client := &fakeClient{response: jsonResponse(searchFixture)}
	adapter, _ := fakeAdapter(client)
	results, err := adapter.SearchWithOptions(context.Background(), apiKey(t), "canary", site.SearchOptions{
		Page: 3, Mode: "adult", Categories: []string{"410", "424"},
	})
	if err != nil || len(results) != 3 {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	if len(client.calls) != 1 || client.calls[0].path != "api/torrent/search" {
		t.Fatalf("calls=%#v", client.calls)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(client.calls[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"mode": "adult", "keyword": "canary", "pageNumber": float64(3), "pageSize": float64(50),
		"categories": []any{"410", "424"},
	}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("search body=%s", client.calls[0].body)
	}
}

func TestSearchFreeOnlyAsksForFreeAndDropsCountedRows(t *testing.T) {
	client := &fakeClient{response: jsonResponse(searchFixture)}
	adapter, _ := fakeAdapter(client)
	results, err := adapter.SearchWithOptions(context.Background(), apiKey(t), "canary", site.SearchOptions{FreeOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(client.calls[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["discount"] != "FREE" || sent["mode"] != "normal" || sent["pageNumber"] != float64(1) {
		t.Fatalf("search body=%s", client.calls[0].body)
	}
	// The fixture holds one free row, one normal row, and one row with an
	// unknown discount; only the free row is certain to be free.
	if len(results) != 1 || results[0].Ref.RemoteID != "900001" {
		t.Fatalf("results=%#v", results)
	}
}

func TestSearchOptionsAreValidatedBeforeAnyRequest(t *testing.T) {
	tooMany := make([]string, maxSearchCategories+1)
	for index := range tooMany {
		tooMany[index] = fmt.Sprint(401 + index)
	}
	for _, options := range []site.SearchOptions{
		{Page: -1},
		{Page: maxSearchPage + 1},
		{Mode: "rss"},
		{Mode: "rankings"},
		{Categories: []string{"0"}},
		{Categories: []string{"4O1"}},
		{Categories: []string{"401", "401"}},
		{Categories: tooMany},
	} {
		if err := (&Adapter{}).ValidateSearchOptions(options); err == nil {
			t.Fatalf("options=%#v were accepted", options)
		}
		client := &fakeClient{response: jsonResponse(searchFixture)}
		adapter, record := fakeAdapter(client)
		if _, err := adapter.SearchWithOptions(context.Background(), apiKey(t), "canary", options); err == nil || record.opened != 0 || len(client.calls) != 0 {
			t.Fatalf("options=%#v err=%v opened=%d", options, err, record.opened)
		}
	}
	for _, options := range []site.SearchOptions{
		{},
		{Page: 1},
		{Page: maxSearchPage, Mode: "normal", Categories: tooMany[:maxSearchCategories], FreeOnly: true},
	} {
		if err := (&Adapter{}).ValidateSearchOptions(options); err != nil {
			t.Fatalf("options=%#v: %v", options, err)
		}
	}
}
