package cli

import (
	"context"
	"errors"
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
	case "status", "account", "search", "bonus-catalog":
		return a.siteRead(args[0], args[1:])
	case "detail":
		return a.siteDetail(args[1:])
	case "login":
		return a.siteLogin(args[1:])
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
	if !*cookieStdin && !*apiKeyStdin {
		return usageError("%s is required; credentials are never accepted in argv", requiredCredentialFlag(a.registry, fs.Arg(0)))
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
	}[command]
	descriptor := adapter.Descriptor()
	if !descriptor.Supports(capability) {
		return usageError("site %q does not declare capability %q", descriptor.ID, capability)
	}
	method, readSecret := domain.AuthMethodCookieHeader, readCredential
	if *apiKeyStdin {
		method, readSecret = domain.AuthMethodAPIKeyHeader, readAPIKeyCredential
	}
	if !descriptor.SupportsAuth(method) {
		return usageError("site %q does not support %s authentication", descriptor.ID, method)
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
	case "bonus-catalog":
		reader, ok := adapter.(site.BonusCatalogReader)
		if !ok {
			return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, capability)
		}
		read = func(ctx context.Context, credential site.Credential) (any, error) {
			return reader.BonusCatalog(ctx, credential)
		}
		warnings = append(warnings, "catalog is read-only; this command never submits purchase or redemption forms")
	default:
		return usageError("unknown site read command %q", command)
	}
	credential, err := readSecret(a.stdin)
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

// requiredCredentialFlag names the stdin flag a site's read commands need. A
// site that only accepts API keys gets --api-key-stdin; every other site, and
// an unknown one, keeps the original --cookie-stdin message.
func requiredCredentialFlag(registry *site.Registry, id string) string {
	if adapter, ok := registry.Get(id); ok {
		descriptor := adapter.Descriptor()
		if descriptor.SupportsAuth(domain.AuthMethodAPIKeyHeader) && !descriptor.SupportsAuth(domain.AuthMethodCookieHeader) {
			return "--api-key-stdin"
		}
	}
	return "--cookie-stdin"
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
		if !reportsSwarmOrPromotion(items) {
			fmt.Fprintln(w, "REF\tNAME\tSIZE")
			for _, item := range items {
				fmt.Fprintf(w, "%s/%s\t%s\t%s\n", terminalSafe(item.Ref.SiteID), terminalSafe(item.Ref.RemoteID), terminalSafe(item.Name), searchSize(item))
			}
			break
		}
		fmt.Fprintln(w, "REF\tNAME\tSIZE\tSEEDERS\tLEECHERS\tPROMOTION")
		for _, item := range items {
			fmt.Fprintf(w, "%s/%s\t%s\t%s\t%s\t%s\t%s\n", terminalSafe(item.Ref.SiteID), terminalSafe(item.Ref.RemoteID), terminalSafe(item.Name), searchSize(item),
				optionalCount(item.Seeders), optionalCount(item.Leechers), terminalSafe(promotionLabel(item.Promotion)))
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
