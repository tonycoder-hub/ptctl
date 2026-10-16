package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

// optionSearchAdapter is an API-key site whose search takes options.
type optionSearchAdapter struct {
	apiKeySiteAdapter
	options      site.SearchOptions
	optionCalls  int
	plainCalls   int
	validateErr  error
	validateSeen []site.SearchOptions
}

func (adapter *optionSearchAdapter) Search(ctx context.Context, credential site.Credential, query string) ([]domain.TorrentSummary, error) {
	adapter.plainCalls++
	return adapter.apiKeySiteAdapter.Search(ctx, credential, query)
}

func (adapter *optionSearchAdapter) ValidateSearchOptions(options site.SearchOptions) error {
	adapter.validateSeen = append(adapter.validateSeen, options)
	return adapter.validateErr
}

func (adapter *optionSearchAdapter) SearchWithOptions(_ context.Context, credential site.Credential, _ string, options site.SearchOptions) ([]domain.TorrentSummary, error) {
	adapter.record(credential)
	adapter.optionCalls++
	adapter.options = options
	return adapter.results, adapter.err
}

func TestSiteSearchPassesOptionsToTheSite(t *testing.T) {
	adapter := &optionSearchAdapter{}
	var out bytes.Buffer
	a := &app{stdin: strings.NewReader(apiKeyCanary), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.siteRead("search", []string{
		"--api-key-stdin", "--page", "2", "--mode", "adult", "--category", "410,424", "--category", " 431 ", "--free", "--output", "json", "api-key-test", "canary",
	}); err != nil {
		t.Fatal(err)
	}
	want := site.SearchOptions{Page: 2, Mode: "adult", Categories: []string{"410", "424", "431"}, FreeOnly: true}
	if adapter.optionCalls != 1 || adapter.plainCalls != 0 || !reflect.DeepEqual(adapter.options, want) || adapter.secret != apiKeyCanary {
		t.Fatalf("option calls=%d plain=%d options=%#v", adapter.optionCalls, adapter.plainCalls, adapter.options)
	}

	// Without options the site's plain search runs, as before.
	adapter = &optionSearchAdapter{}
	a = &app{stdin: strings.NewReader(apiKeyCanary), stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(adapter)}
	if err := a.siteRead("search", []string{"--api-key-stdin", "--output", "json", "api-key-test", "canary"}); err != nil {
		t.Fatal(err)
	}
	if adapter.optionCalls != 0 || adapter.plainCalls != 1 || len(adapter.validateSeen) != 0 {
		t.Fatalf("option calls=%d plain=%d", adapter.optionCalls, adapter.plainCalls)
	}
}

func TestSiteSearchOptionsAreRefusedBeforeStdin(t *testing.T) {
	for _, test := range []struct {
		name    string
		adapter site.Adapter
		args    []string
		want    string
	}{
		{name: "site without options", adapter: &apiKeySiteAdapter{}, args: []string{"--page", "2"},
			want: `site "api-key-test" does not support --page, --mode, --category, or --free`},
		{name: "page zero", adapter: &optionSearchAdapter{}, args: []string{"--page", "0"}, want: "--page must be 1 or more"},
		{name: "negative page", adapter: &optionSearchAdapter{}, args: []string{"--page", "-1"}, want: "--page must be 1 or more"},
		{name: "empty category", adapter: &optionSearchAdapter{}, args: []string{"--category", "401,"},
			want: `site search: invalid value "401," for flag -category: category must not be empty`},
		{name: "site rejects", adapter: &optionSearchAdapter{validateErr: errors.New("search mode must be normal or adult")}, args: []string{"--mode", "rss"},
			want: "search mode must be normal or adult"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &trackingReader{}
			var out bytes.Buffer
			a := &app{stdin: reader, stdout: &out, stderr: ioDiscard{}, registry: site.NewRegistry(test.adapter)}
			args := append(append([]string{"--api-key-stdin"}, test.args...), "api-key-test", "canary")
			err := a.siteRead("search", args)
			var usage *usageErr
			if !errors.As(err, &usage) || err.Error() != test.want || reader.read || out.Len() != 0 {
				t.Fatalf("err=%v read=%t stdout=%q", err, reader.read, out.String())
			}
		})
	}
}

func TestSearchOptionFlagsBelongOnlyToSearch(t *testing.T) {
	for _, command := range []string{"status", "account", "bonus-catalog"} {
		for _, flag := range []string{"--page=2", "--mode=adult", "--category=401", "--free"} {
			reader := &trackingReader{}
			a := &app{stdin: reader, stdout: ioDiscard{}, stderr: ioDiscard{}, registry: site.NewRegistry(&optionSearchAdapter{})}
			err := a.siteRead(command, []string{"--api-key-stdin", flag, "api-key-test"})
			var usage *usageErr
			if !errors.As(err, &usage) || !strings.Contains(err.Error(), fmt.Sprintf("flag provided but not defined: -%s", strings.TrimPrefix(strings.SplitN(flag, "=", 2)[0], "--"))) || reader.read {
				t.Fatalf("%s %s err=%v read=%t", command, flag, err, reader.read)
			}
		}
	}
}
