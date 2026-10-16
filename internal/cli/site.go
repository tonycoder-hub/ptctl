package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

func (a *app) site(args []string) error {
	if len(args) == 0 {
		return usageError("site subcommand is required")
	}
	switch args[0] {
	case "list":
		return a.siteList(args[1:])
	case "capabilities":
		return a.siteCapabilities(args[1:])
	case "status", "account", "search", "bonus-catalog", "torrents":
		return a.siteRead(args[0], args[1:])
	case "detail":
		return a.siteDetail(args[1:])
	case "login":
		return a.siteLogin(args[1:])
	case "logout":
		return a.siteLogout(args[1:])
	case "bonus":
		if len(args) >= 2 {
			switch args[1] {
			case "review":
				return a.siteBonusReview(args[2:])
			case "exchange":
				return a.siteBonusExchange(args[2:])
			}
		}
		return usageError("site bonus requires review or exchange")
	case "metafile":
		if len(args) >= 2 {
			switch args[1] {
			case "fetch":
				return a.siteMetafileFetch(args[2:])
			case "binding":
				return a.siteMetafileBinding(args[2:])
			}
		}
		return usageError("site metafile requires fetch or binding")
	default:
		return usageError("unknown site subcommand %q", args[0])
	}
}

func (a *app) siteList(args []string) error {
	fs := newFlagSet("site list")
	output := fs.String("output", "table", "table or json")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usageError("site list accepts only --output")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	items := a.registry.Descriptors()
	if *output == "json" {
		return writeJSON(a.stdout, items, nil)
	}
	if *output != "table" {
		return usageError("--output must be table or json")
	}
	w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSTABILITY\tBASE URL\tCAPABILITIES")
	for _, item := range items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", terminalSafe(item.ID), terminalSafe(item.Name), terminalSafe(item.Stability), terminalSafe(item.BaseURL), len(item.Capabilities))
	}
	return w.Flush()
}

func (a *app) siteCapabilities(args []string) error {
	fs := newFlagSet("site capabilities")
	output := fs.String("output", "table", "table or json")
	if err := fs.Parse(args); err != nil || fs.NArg() > 1 {
		return usageError("site capabilities accepts zero or one SITE")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	items := a.registry.Descriptors()
	if fs.NArg() == 1 {
		driver, ok := a.registry.Get(fs.Arg(0))
		if !ok {
			return usageError("unknown site %q", fs.Arg(0))
		}
		items = []domain.SiteDescriptor{driver.Descriptor()}
	}
	if *output == "json" {
		return writeJSON(a.stdout, items, nil)
	}
	if *output != "table" {
		return usageError("--output must be table or json")
	}
	w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SITE\tKIND\tVALUE")
	for _, item := range items {
		for _, method := range item.AuthMethods {
			fmt.Fprintf(w, "%s\tauth\t%s\n", terminalSafe(item.ID), terminalSafe(string(method)))
		}
		for _, capability := range item.Capabilities {
			fmt.Fprintf(w, "%s\tcapability\t%s\n", terminalSafe(item.ID), terminalSafe(string(capability)))
		}
	}
	return w.Flush()
}

func (a *app) siteRead(command string, args []string) error {
	fs := newFlagSet("site " + command)
	output := fs.String("output", "table", "table or json")
	cookieStdin := fs.Bool("cookie-stdin", false, "read session Cookie header value from stdin")
	apiKeyStdin := fs.Bool("api-key-stdin", false, "read a site-issued API key from stdin")
	var searchOptions site.SearchOptions
	if command == "search" {
		fs.IntVar(&searchOptions.Page, "page", 0, "result page, from 1 (default: the first page)")
		fs.StringVar(&searchOptions.Mode, "mode", "", "site search area, such as normal or adult on M-Team")
		fs.Var((*categoryListFlag)(&searchOptions.Categories), "category", "keep only this site category ID; repeat or separate with commas")
		fs.BoolVar(&searchOptions.FreeOnly, "free", false, "keep only torrents whose download is not counted")
	}
	torrentQuery := site.AccountTorrentQuery{State: domain.AccountTorrentsSeeding}
	if command == "torrents" {
		fs.Var((*accountTorrentStateFlag)(&torrentQuery.State), "state", "list to read: seeding, leeching, completed, incomplete, or uploaded (default seeding)")
		fs.IntVar(&torrentQuery.Page, "page", 0, "result page, from 1 (default: the first page)")
	}
	if err := fs.Parse(args); err != nil {
		return usageError("site %s: %v", command, err)
	}
	if fs.NArg() < 1 {
		return usageError("site %s requires SITE", command)
	}
	if command != "search" && fs.NArg() != 1 {
		return usageError("site %s requires exactly one SITE", command)
	}
	if command == "search" && fs.NArg() < 2 {
		return usageError("site search requires SITE and QUERY")
	}
	if *cookieStdin && *apiKeyStdin {
		return usageError("--cookie-stdin and --api-key-stdin are mutually exclusive")
	}
	adapter, ok := a.registry.Get(fs.Arg(0))
	if !ok {
		return usageError("unknown site %q", fs.Arg(0))
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	capability := map[string]domain.Capability{
		"status":        domain.CapabilityAuthCheck,
		"account":       domain.CapabilityAccountRead,
		"search":        domain.CapabilitySearch,
		"bonus-catalog": domain.CapabilityBonusRead,
		"torrents":      domain.CapabilityAccountTorrents,
	}[command]
	descriptor := adapter.Descriptor()
	if !descriptor.Supports(capability) {
		return usageError("site %q does not declare capability %q", descriptor.ID, capability)
	}
	source, err := siteCredentialSourceFor(descriptor, *cookieStdin, *apiKeyStdin)
	if err != nil {
		return err
	}
	readSecret := readCredential
	if source.method == domain.AuthMethodAPIKeyHeader {
		readSecret = readAPIKeyCredential
	}
	var read func(context.Context, site.Credential) (any, error)
	var warnings []string
	switch command {
	case "status":
		reader, ok := adapter.(site.AuthChecker)
		if !ok {
			return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, capability)
		}
		read = func(ctx context.Context, credential site.Credential) (any, error) {
			return reader.CheckSession(ctx, credential)
		}
	case "account":
		reader, ok := adapter.(site.AccountReader)
		if !ok {
			return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, capability)
		}
		read = func(ctx context.Context, credential site.Credential) (any, error) {
			return reader.Account(ctx, credential)
		}
	case "search":
		query := strings.TrimSpace(strings.Join(fs.Args()[1:], " "))
		if query == "" {
			return usageError("site search query is empty")
		}
		reader, ok := adapter.(site.TorrentSearcher)
		if !ok {
			return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, capability)
		}
		read = func(ctx context.Context, credential site.Credential) (any, error) {
			return reader.Search(ctx, credential, query)
		}
		pageSet := false
		fs.Visit(func(flag *flag.Flag) { pageSet = pageSet || flag.Name == "page" })
		if pageSet && searchOptions.Page < 1 {
			return usageError("--page must be 1 or more")
		}
		if !searchOptions.IsZero() {
			optionReader, ok := adapter.(site.OptionTorrentSearcher)
			if !ok {
				return usageError("site %q does not support --page, --mode, --category, or --free", descriptor.ID)
			}
			if err := optionReader.ValidateSearchOptions(searchOptions); err != nil {
				return usageError("%v", err)
			}
			read = func(ctx context.Context, credential site.Credential) (any, error) {
				return optionReader.SearchWithOptions(ctx, credential, query, searchOptions)
			}
		}
	case "bonus-catalog":
		reader, ok := adapter.(site.BonusCatalogReader)
		if !ok {
			return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, capability)
		}
		read = func(ctx context.Context, credential site.Credential) (any, error) {
			return reader.BonusCatalog(ctx, credential)
		}
		warnings = append(warnings, "catalog is read-only; this command never submits purchase or redemption forms")
	case "torrents":
		lister, ok := adapter.(site.AccountTorrentLister)
		if !ok {
			return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, capability)
		}
		pageSet := false
		fs.Visit(func(flag *flag.Flag) { pageSet = pageSet || flag.Name == "page" })
		if pageSet && torrentQuery.Page < 1 {
			return usageError("--page must be 1 or more")
		}
		if err := lister.ValidateAccountTorrentQuery(torrentQuery); err != nil {
			return usageError("%v", err)
		}
		read = func(ctx context.Context, credential site.Credential) (any, error) {
			return lister.AccountTorrents(ctx, credential, torrentQuery)
		}
	default:
		return usageError("unknown site read command %q", command)
	}
	if err := source.locateSaved(descriptor.ID); err != nil {
		return err
	}
	credential, err := a.readSiteCredential(source, readSecret)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	data, err := read(ctx, credential)
	if err != nil {
		return siteReadPublicError(ctx, command, err)
	}
	if *output == "json" {
		return writeJSON(a.stdout, data, warnings)
	}
	return writeSiteHuman(a.stdout, command, data, warnings)
}

func siteReadPublicError(ctx context.Context, command string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("site %s canceled: %w", command, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("site %s timed out: %w", command, context.DeadlineExceeded)
	}
	// Adapters opt in to these secret-free causes; everything else keeps the
	// generic line.
	for _, cause := range []error{site.ErrCredentialMalformed, site.ErrCredentialRejected, site.ErrRateLimited, site.ErrAddressBlocked} {
		if errors.Is(err, cause) {
			return fmt.Errorf("site %s failed: %v", command, cause)
		}
	}
	return fmt.Errorf("site %s failed", command)
}

func writeSiteHuman(out io.Writer, command string, data any, warnings []string) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	switch command {
	case "status":
		status := data.(domain.SessionStatus)
		fmt.Fprintf(w, "AUTHENTICATION\t%s\nUSERNAME\t%s\nOBSERVED\t%s\n", terminalSafe(string(status.State)), terminalSafe(status.Username), status.ObservedAt.Format(time.RFC3339))
	case "account":
		account := data.(domain.AccountSnapshot)
		fmt.Fprintf(w, "SITE\t%s\nUSERNAME\t%s\n", terminalSafe(account.SiteID), terminalSafe(account.Username))
		if account.UserClass != "" {
			fmt.Fprintf(w, "CLASS\t%s\n", terminalSafe(account.UserClass))
		}
		if account.UploadedBytes != nil {
			fmt.Fprintf(w, "UPLOADED\t%s\n", humanBytes(*account.UploadedBytes))
		}
		if account.DownloadedBytes != nil {
			fmt.Fprintf(w, "DOWNLOADED\t%s\n", humanBytes(*account.DownloadedBytes))
		}
		if account.Ratio != "" {
			fmt.Fprintf(w, "RATIO\t%s\n", terminalSafe(account.Ratio))
		}
		if account.Seeding != nil {
			fmt.Fprintf(w, "SEEDING\t%d\n", *account.Seeding)
		}
		if account.Leeching != nil {
			fmt.Fprintf(w, "LEECHING\t%d\n", *account.Leeching)
		}
		if account.SeedingPoints != "" {
			fmt.Fprintf(w, "SEEDING POINTS\t%s\n", terminalSafe(account.SeedingPoints))
		}
		if account.Invites != nil {
			fmt.Fprintf(w, "INVITES\t%d\n", *account.Invites)
		}
		fmt.Fprintf(w, "BONUS\t%s\nOBSERVED\t%s\n", terminalSafe(valueOrUnknown(account.Bonus)), account.ObservedAt.Format(time.RFC3339))
	case "search":
		items := data.([]domain.TorrentSummary)
		// Category names come last so the leading columns keep their places.
		categoryHeader, category := "", func(domain.TorrentSummary) string { return "" }
		if reportsCategoryName(items) {
			categoryHeader = "\tCATEGORY"
			category = func(item domain.TorrentSummary) string { return "\t" + terminalSafe(valueOrDash(item.CategoryName)) }
		}
		if !reportsSwarmOrPromotion(items) {
			fmt.Fprintln(w, "REF\tNAME\tSIZE"+categoryHeader)
			for _, item := range items {
				fmt.Fprintf(w, "%s/%s\t%s\t%s%s\n", terminalSafe(item.Ref.SiteID), terminalSafe(item.Ref.RemoteID), terminalSafe(item.Name), searchSize(item), category(item))
			}
			break
		}
		fmt.Fprintln(w, "REF\tNAME\tSIZE\tSEEDERS\tLEECHERS\tPROMOTION"+categoryHeader)
		for _, item := range items {
			fmt.Fprintf(w, "%s/%s\t%s\t%s\t%s\t%s\t%s%s\n", terminalSafe(item.Ref.SiteID), terminalSafe(item.Ref.RemoteID), terminalSafe(item.Name), searchSize(item),
				optionalCount(item.Seeders), optionalCount(item.Leechers), terminalSafe(promotionLabel(item.Promotion)), category(item))
		}
	case "bonus-catalog":
		catalog := data.(domain.BonusCatalog)
		fmt.Fprintf(w, "BALANCE\t%s\nOPTION\tOFFER\n", terminalSafe(valueOrUnknown(catalog.Balance)))
		for index, row := range catalog.Rows {
			selector := row.Selector
			if selector == "" {
				selector = fmt.Sprintf("unknown-%d", index+1)
			}
			fmt.Fprintf(w, "%s\t%s\n", terminalSafe(selector), terminalSafe(strings.Join(row.Columns, " | ")))
		}
	case "torrents":
		list := data.(domain.AccountTorrentList)
		page := fmt.Sprint(list.Page)
		// An empty list has zero pages; "1 of 0" would read as an error.
		if list.TotalPages != nil && *list.TotalPages > 0 {
			page += fmt.Sprintf(" of %d", *list.TotalPages)
		}
		fmt.Fprintf(w, "STATE\t%s\nPAGE\t%s\n", terminalSafe(string(list.State)), page)
		if list.Total != nil {
			fmt.Fprintf(w, "TOTAL\t%d\n", *list.Total)
		}
		fmt.Fprintln(w, "REF\tNAME\tSIZE\tTRAFFIC\tUPLOADED\tDOWNLOADED\tSEED TIME\tLAST ACTION")
		for _, item := range list.Torrents {
			fmt.Fprintf(w, "%s/%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", terminalSafe(item.Torrent.Ref.SiteID), terminalSafe(item.Torrent.Ref.RemoteID),
				terminalSafe(item.Torrent.Name), searchSize(item.Torrent), trafficScope(item.TrafficScope),
				optionalBytes(item.UploadedBytes), optionalBytes(item.DownloadedBytes), seedTime(item.SeedTimeSeconds), optionalTime(item.LastActionAt))
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, warning := range warnings {
		fmt.Fprintf(out, "warning: %s\n", terminalSafe(warning))
	}
	return nil
}

// reportsSwarmOrPromotion decides whether the search table grows extra
// columns. Sites that never report these fields keep the original layout.
func reportsSwarmOrPromotion(items []domain.TorrentSummary) bool {
	for _, item := range items {
		if item.Seeders != nil || item.Leechers != nil || item.Promotion != nil {
			return true
		}
	}
	return false
}

// reportsCategoryName decides whether the search table grows a CATEGORY
// column. Sites whose adapters report only category IDs keep their layout.
func reportsCategoryName(items []domain.TorrentSummary) bool {
	for _, item := range items {
		if item.CategoryName != "" {
			return true
		}
	}
	return false
}

func searchSize(item domain.TorrentSummary) string {
	if item.SizeBytes == nil {
		return "unknown"
	}
	return humanBytes(*item.SizeBytes)
}

func optionalCount(value *int) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprint(*value)
}

// promotionLabel renders traffic factors, for example "free" or
// "up 2x, down 0.5x until 2026-10-15T00:00:00Z". Normal traffic prints "-";
// a promotion the adapter could not recognize prints "unknown".
func promotionLabel(promotion *domain.Promotion) string {
	if promotion == nil {
		return "unknown"
	}
	var parts []string
	if promotion.UploadFactor != "1" {
		parts = append(parts, "up "+promotion.UploadFactor+"x")
	}
	switch promotion.DownloadFactor {
	case "1":
	case "0":
		parts = append(parts, "free")
	default:
		parts = append(parts, "down "+promotion.DownloadFactor+"x")
	}
	if len(parts) == 0 {
		return "-"
	}
	label := strings.Join(parts, ", ")
	if promotion.EndsAt != nil {
		label += " until " + promotion.EndsAt.UTC().Format(time.RFC3339)
	}
	return label
}

func optionalBytes(value *int64) string {
	if value == nil {
		return "-"
	}
	return humanBytes(*value)
}

func optionalTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}

// trafficScope names the span a row's traffic covers: "total" for the
// account's whole history, "session" for the client's running session.
func trafficScope(scope domain.AccountTrafficScope) string {
	switch scope {
	case domain.AccountTrafficTotal:
		return "total"
	case domain.AccountTrafficCurrentSession:
		return "session"
	case "":
		return "-"
	}
	return "unknown"
}

// seedTime renders whole seconds as days, hours, and minutes, for example
// "2d 3h 4m"; anything under a minute prints "0m".
func seedTime(seconds *int64) string {
	if seconds == nil {
		return "-"
	}
	minutes := *seconds / 60
	days, hours := minutes/(24*60), minutes/60%24
	minutes %= 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

// accountTorrentStateFlag accepts only the lists the CLI names, so a typo is
// a usage error before any site is consulted.
type accountTorrentStateFlag domain.AccountTorrentState

func (state *accountTorrentStateFlag) String() string { return string(*state) }

func (state *accountTorrentStateFlag) Set(value string) error {
	switch parsed := domain.AccountTorrentState(value); parsed {
	case domain.AccountTorrentsSeeding, domain.AccountTorrentsLeeching, domain.AccountTorrentsCompleted,
		domain.AccountTorrentsIncomplete, domain.AccountTorrentsUploaded:
		*state = accountTorrentStateFlag(parsed)
		return nil
	}
	return fmt.Errorf("state must be seeding, leeching, completed, incomplete, or uploaded")
}

// categoryListFlag collects site category IDs from repeated or
// comma-separated --category values.
type categoryListFlag []string

func (values *categoryListFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *categoryListFlag) Set(value string) error {
	for _, category := range strings.Split(value, ",") {
		category = strings.TrimSpace(category)
		if category == "" {
			return fmt.Errorf("category must not be empty")
		}
		*values = append(*values, category)
	}
	return nil
}
