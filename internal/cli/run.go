package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/downloader/qbittorrent"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/security"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/site"
	"github.com/tonycoder-hub/ptctl/internal/site/tjupt"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

var (
	Version = "dev"
	Commit  = "unknown"
)

type app struct {
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer
	registry *site.Registry
}

type envelope struct {
	Schema   string   `json:"schema"`
	Data     any      `json:"data"`
	Warnings []string `json:"warnings,omitempty"`
}

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{
		stdin: stdin, stdout: stdout, stderr: stderr,
		registry: site.NewRegistry(tjupt.New("")),
	}
	if len(args) == 0 {
		a.help()
		return 0
	}
	var err error
	switch args[0] {
	case "help", "-h", "--help":
		a.help()
		return 0
	case "version":
		err = a.version(args[1:])
	case "site":
		err = a.site(args[1:])
	case "torrent":
		err = a.torrent(args[1:])
	case "storage":
		err = a.storage(args[1:])
	case "client":
		err = a.client(args[1:])
	case "seed":
		err = a.seed(args[1:])
	default:
		err = usageError("unknown command %q", args[0])
	}
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "error: %s\n", security.Redact(err.Error()))
	var usage *usageErr
	if errors.As(err, &usage) {
		fmt.Fprintln(stderr, "run 'ptctl help' for usage")
		return 2
	}
	return 1
}

func (a *app) help() {
	fmt.Fprint(a.stdout, `ptctl — a conservative private-tracker content CLI

Usage:
  ptctl site list [--output table|json]
  ptctl site capabilities [--output table|json] [SITE]
  ptctl site status --cookie-stdin [--output table|json] SITE
  ptctl site account --cookie-stdin [--output table|json] SITE
  ptctl site search --cookie-stdin [--output table|json] SITE QUERY...
  ptctl site bonus-catalog --cookie-stdin [--output table|json] SITE

  ptctl torrent inspect [--output table|json] FILE.torrent
  ptctl torrent verify --content PATH [--output table|json] FILE.torrent

  ptctl storage probe [--output table|json] PATH
  ptctl storage map --host-root PATH --client-root PATH [--client-style posix|windows] HOST_PATH

  ptctl client status --url URL --username USER --password-stdin [--output table|json]
  ptctl client list --url URL --username USER --password-stdin [--output table|json]

  ptctl seed plan --torrent FILE.torrent --source PATH --target PATH [--output table|json]
  ptctl version [--output table|json]

Safety defaults:
  * Site operations are GET-only, rate-limited, same-origin HTTPS reads.
  * Session cookies are accepted only through stdin and are never persisted.
  * .torrent tracker URLs are reduced to origins; passkeys are never printed.
  * Seed materialization emits a verified plan only; v0.1 performs no writes.
`)
}

func (a *app) version(args []string) error {
	fs := newFlagSet("version")
	output := fs.String("output", "table", "table or json")
	if err := fs.Parse(args); err != nil {
		return usageError("version: %v", err)
	}
	if fs.NArg() != 0 {
		return usageError("version takes no positional arguments")
	}
	data := map[string]string{"version": Version, "commit": Commit}
	if *output == "json" {
		return writeJSON(a.stdout, data, nil)
	}
	if *output != "table" {
		return usageError("--output must be table or json")
	}
	fmt.Fprintf(a.stdout, "ptctl %s (%s)\n", Version, Commit)
	return nil
}

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
	default:
		return usageError("unknown site subcommand %q", args[0])
	}
}

func (a *app) client(args []string) error {
	if len(args) == 0 || (args[0] != "status" && args[0] != "list") {
		return usageError("client requires status or list")
	}
	command := args[0]
	fs := newFlagSet("client " + command)
	output := fs.String("output", "table", "table or json")
	endpoint := fs.String("url", "", "qBittorrent Web API origin")
	username := fs.String("username", "", "qBittorrent username")
	passwordStdin := fs.Bool("password-stdin", false, "read password from stdin")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *endpoint == "" {
		return usageError("client %s requires --url and no positional arguments", command)
	}
	if !*passwordStdin {
		return usageError("--password-stdin is required; downloader passwords are never accepted in argv")
	}
	credential, err := readDownloaderCredential(a.stdin, *username)
	if err != nil {
		return err
	}
	driver, err := qbittorrent.New(*endpoint)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var data any
	if command == "status" {
		data, err = driver.Status(ctx, credential)
	} else {
		data, err = driver.Torrents(ctx, credential)
	}
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(a.stdout, data, nil)
	}
	if *output != "table" {
		return usageError("--output must be table or json")
	}
	return writeClientHuman(a.stdout, command, data)
}

func (a *app) siteList(args []string) error {
	fs := newFlagSet("site list")
	output := fs.String("output", "table", "table or json")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usageError("site list accepts only --output")
	}
	items := a.registry.Descriptors()
	if *output == "json" {
		return writeJSON(a.stdout, items, nil)
	}
	if *output != "table" {
		return usageError("--output must be table or json")
	}
	w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tBASE URL\tCAPABILITIES")
	for _, item := range items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", item.ID, item.Name, item.BaseURL, len(item.Capabilities))
	}
	return w.Flush()
}

func (a *app) siteCapabilities(args []string) error {
	fs := newFlagSet("site capabilities")
	output := fs.String("output", "table", "table or json")
	if err := fs.Parse(args); err != nil || fs.NArg() > 1 {
		return usageError("site capabilities accepts zero or one SITE")
	}
	items := a.registry.Descriptors()
	if fs.NArg() == 1 {
		driver, ok := a.registry.Get(fs.Arg(0))
		if !ok {
			return fmt.Errorf("unknown site %q", fs.Arg(0))
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
	fmt.Fprintln(w, "SITE\tCAPABILITY")
	for _, item := range items {
		for _, capability := range item.Capabilities {
			fmt.Fprintf(w, "%s\t%s\n", item.ID, capability)
		}
	}
	return w.Flush()
}

func (a *app) siteRead(command string, args []string) error {
	fs := newFlagSet("site " + command)
	output := fs.String("output", "table", "table or json")
	cookieStdin := fs.Bool("cookie-stdin", false, "read session Cookie header value from stdin")
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
	if !*cookieStdin {
		return usageError("--cookie-stdin is required; credentials are never accepted in argv")
	}
	driver, ok := a.registry.Get(fs.Arg(0))
	if !ok {
		return fmt.Errorf("unknown site %q", fs.Arg(0))
	}
	credential, err := readCredential(a.stdin)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	var data any
	var warnings []string
	switch command {
	case "status":
		data, err = driver.CheckSession(ctx, credential)
	case "account":
		data, err = driver.Account(ctx, credential)
	case "search":
		data, err = driver.Search(ctx, credential, strings.Join(fs.Args()[1:], " "))
	case "bonus-catalog":
		data, err = driver.BonusCatalog(ctx, credential)
		warnings = append(warnings, "catalog is read-only; ptctl never submits purchase or redemption forms")
	}
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(a.stdout, data, warnings)
	}
	if *output != "table" {
		return usageError("--output must be table or json")
	}
	return writeSiteHuman(a.stdout, command, data, warnings)
}

func (a *app) torrent(args []string) error {
	if len(args) == 0 {
		return usageError("torrent subcommand is required")
	}
	switch args[0] {
	case "inspect":
		fs := newFlagSet("torrent inspect")
		output := fs.String("output", "table", "table or json")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
			return usageError("torrent inspect requires one FILE.torrent")
		}
		meta, err := metafile.Read(fs.Arg(0))
		if err != nil {
			return err
		}
		if *output == "json" {
			return writeJSON(a.stdout, meta, nil)
		}
		if *output != "table" {
			return usageError("--output must be table or json")
		}
		return writeMetaHuman(a.stdout, meta)
	case "verify":
		fs := newFlagSet("torrent verify")
		output := fs.String("output", "table", "table or json")
		content := fs.String("content", "", "exact file (single-file) or torrent root directory (multi-file)")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 || *content == "" {
			return usageError("torrent verify requires --content PATH and one FILE.torrent")
		}
		meta, err := metafile.Read(fs.Arg(0))
		if err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result, err := metafile.VerifyV1(ctx, meta, *content)
		if err != nil {
			return err
		}
		if *output == "json" {
			return writeJSON(a.stdout, result, nil)
		}
		if *output != "table" {
			return usageError("--output must be table or json")
		}
		return writeVerifyHuman(a.stdout, result)
	default:
		return usageError("unknown torrent subcommand %q", args[0])
	}
}

func (a *app) storage(args []string) error {
	if len(args) == 0 {
		return usageError("storage subcommand is required")
	}
	switch args[0] {
	case "probe":
		fs := newFlagSet("storage probe")
		output := fs.String("output", "table", "table or json")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
			return usageError("storage probe requires one PATH")
		}
		result, err := storage.ProbeReadOnly(fs.Arg(0))
		if err != nil {
			return err
		}
		if *output == "json" {
			return writeJSON(a.stdout, result, result.Warnings)
		}
		if *output != "table" {
			return usageError("--output must be table or json")
		}
		return writeProbeHuman(a.stdout, result)
	case "map":
		fs := newFlagSet("storage map")
		output := fs.String("output", "table", "table or json")
		hostRoot := fs.String("host-root", "", "host-visible storage root")
		clientRoot := fs.String("client-root", "", "downloader-visible storage root")
		clientStyle := fs.String("client-style", "posix", "posix or windows")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 || *hostRoot == "" || *clientRoot == "" {
			return usageError("storage map requires --host-root, --client-root, and HOST_PATH")
		}
		if *clientStyle != "posix" && *clientStyle != "windows" {
			return usageError("--client-style must be posix or windows")
		}
		result, err := storage.MapHostToClient(*hostRoot, fs.Arg(0), *clientRoot, *clientStyle == "windows")
		if err != nil {
			return err
		}
		if *output == "json" {
			return writeJSON(a.stdout, result, nil)
		}
		if *output != "table" {
			return usageError("--output must be table or json")
		}
		fmt.Fprintf(a.stdout, "%s\n", result.ClientPath)
		return nil
	default:
		return usageError("unknown storage subcommand %q", args[0])
	}
}

func (a *app) seed(args []string) error {
	if len(args) == 0 || args[0] != "plan" {
		return usageError("seed supports only the plan subcommand in v0.1")
	}
	fs := newFlagSet("seed plan")
	output := fs.String("output", "table", "table or json")
	torrentPath := fs.String("torrent", "", "metafile path")
	source := fs.String("source", "", "verified source content")
	target := fs.String("target", "", "target storage root")
	strategy := fs.String("strategy", "copy", "copy")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *torrentPath == "" || *source == "" || *target == "" {
		return usageError("seed plan requires --torrent, --source, and --target")
	}
	meta, err := metafile.Read(*torrentPath)
	if err != nil {
		return err
	}
	plan, err := seed.BuildMaterializePlan(context.Background(), meta, *source, *target, *strategy)
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(a.stdout, plan, plan.Warnings)
	}
	if *output != "table" {
		return usageError("--output must be table or json")
	}
	return writePlanHuman(a.stdout, plan)
}

func readCredential(reader io.Reader) (site.Credential, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	if err != nil {
		return site.Credential{}, fmt.Errorf("read session credential from stdin: %w", err)
	}
	if len(raw) > 64<<10 {
		return site.Credential{}, fmt.Errorf("session credential exceeds 64 KiB")
	}
	value := strings.TrimSpace(string(raw))
	return site.NewCookieCredential(value)
}

func readDownloaderCredential(reader io.Reader, username string) (downloader.Credential, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	if err != nil {
		return downloader.Credential{}, fmt.Errorf("read downloader password from stdin: %w", err)
	}
	if len(raw) > 64<<10 {
		return downloader.Credential{}, fmt.Errorf("downloader password exceeds 64 KiB")
	}
	return downloader.NewCredential(username, strings.TrimRight(string(raw), "\r\n"))
}

func writeJSON(out io.Writer, data any, warnings []string) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(envelope{Schema: "ptctl.dev/v1", Data: data, Warnings: warnings})
}

func writeSiteHuman(out io.Writer, command string, data any, warnings []string) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	switch command {
	case "status":
		status := data.(domain.SessionStatus)
		fmt.Fprintf(w, "AUTHENTICATED\t%t\nUSERNAME\t%s\nOBSERVED\t%s\n", status.Authenticated, status.Username, status.ObservedAt.Format(time.RFC3339))
	case "account":
		account := data.(domain.AccountSnapshot)
		fmt.Fprintf(w, "SITE\t%s\nUSERNAME\t%s\nBONUS\t%s\nOBSERVED\t%s\n", account.SiteID, account.Username, valueOrUnknown(account.Bonus), account.ObservedAt.Format(time.RFC3339))
	case "search":
		fmt.Fprintln(w, "REF\tNAME\tSIZE")
		for _, item := range data.([]domain.TorrentSummary) {
			size := "unknown"
			if item.SizeBytes != nil {
				size = humanBytes(*item.SizeBytes)
			}
			fmt.Fprintf(w, "%s/%s\t%s\t%s\n", item.Ref.SiteID, item.Ref.RemoteID, item.Name, size)
		}
	case "bonus-catalog":
		catalog := data.(domain.BonusCatalog)
		fmt.Fprintf(w, "BALANCE\t%s\n", valueOrUnknown(catalog.Balance))
		for index, row := range catalog.Rows {
			fmt.Fprintf(w, "%d\t%s\n", index+1, strings.Join(row.Columns, " | "))
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, warning := range warnings {
		fmt.Fprintf(out, "warning: %s\n", warning)
	}
	return nil
}

func writeMetaHuman(out io.Writer, meta *metafile.MetaInfo) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "NAME\t%s\nVERSION\t%s\nPRIVATE\t%t\nTOTAL\t%s (%d bytes)\nPIECES\t%d × %s\n", meta.Name, meta.Version, meta.Private, humanBytes(meta.TotalLength), meta.TotalLength, meta.PieceCount, humanBytes(meta.PieceLength))
	if meta.InfoHashV1 != "" {
		fmt.Fprintf(w, "INFOHASH V1\t%s\n", meta.InfoHashV1)
	}
	if meta.InfoHashV2 != "" {
		fmt.Fprintf(w, "INFOHASH V2\t%s\n", meta.InfoHashV2)
	}
	for _, tracker := range meta.Trackers {
		fmt.Fprintf(w, "TRACKER ORIGIN\t%s\n", tracker)
	}
	fmt.Fprintln(w, "\nBYTES\tPATH")
	for _, file := range meta.Files {
		fmt.Fprintf(w, "%d\t%s\n", file.Length, strings.Join(file.Path, "/"))
	}
	return w.Flush()
}

func writeVerifyHuman(out io.Writer, result metafile.VerificationResult) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "VERIFIED\t%t\nEVIDENCE\t%s\nBYTES\t%d\nFILES\t%d\nPIECES\t%d/%d\n", result.Verified, result.Evidence, result.BytesVerified, result.FilesChecked, result.PiecesMatched, result.PiecesExpected)
	if len(result.MismatchPieces) > 0 {
		fmt.Fprintf(w, "MISMATCHES\t%v", result.MismatchPieces)
		if result.MismatchOverflow > 0 {
			fmt.Fprintf(w, " (+%d more)", result.MismatchOverflow)
		}
		fmt.Fprintln(w)
	}
	return w.Flush()
}

func writeProbeHuman(out io.Writer, result storage.ProbeResult) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "PATH\t%s\nRESOLVED\t%s\nDIRECTORY\t%t\nRANDOM READ\t%t\nSEEDABLE VIEW\t%t\nWRITE PROBE\t%s\n", result.Path, result.ResolvedPath, result.Directory, result.RandomRead, result.SeedableView, result.WriteProbe)
	for _, warning := range result.Warnings {
		fmt.Fprintf(w, "WARNING\t%s\n", warning)
	}
	return w.Flush()
}

func writeClientHuman(out io.Writer, command string, data any) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if command == "status" {
		status := data.(downloader.Status)
		fmt.Fprintf(w, "DRIVER\t%s\nENDPOINT\t%s\nVERSION\t%s\nWEB API\t%s\nOBSERVED\t%s\n", status.Driver, status.Endpoint, status.Version, status.WebAPIVersion, status.ObservedAt.Format(time.RFC3339))
		return w.Flush()
	}
	fmt.Fprintln(w, "HASH\tPROGRESS\tSTATE\tSIZE\tNAME\tSAVE PATH")
	for _, item := range data.([]downloader.Torrent) {
		fmt.Fprintf(w, "%s\t%.1f%%\t%s\t%s\t%s\t%s\n", item.Hash, item.Progress*100, item.State, humanBytes(item.SizeBytes), item.Name, item.SavePath)
	}
	return w.Flush()
}

func writePlanHuman(out io.Writer, plan seed.Plan) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "PLAN\t%s\nTORRENT\t%s\nEVIDENCE\t%s (%d/%d pieces)\nSTRATEGY\t%s\nSOURCE\t%s\nTARGET\t%s\nREAD\t%s\nWRITE\t%s\n", plan.ID, plan.TorrentName, plan.Evidence, plan.Verification.PiecesMatched, plan.Verification.PiecesExpected, plan.Strategy, plan.SourceRoot, plan.TargetRoot, humanBytes(plan.EstimatedRead), humanBytes(plan.EstimatedWrite))
	fmt.Fprintln(w, "\nACTION\tBYTES\tSOURCE\tTARGET")
	for _, operation := range plan.Operations {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", operation.Kind, operation.Bytes, operation.Source, operation.Target)
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintf(w, "WARNING\t\t\t%s\n", warning)
	}
	return w.Flush()
}

func humanBytes(value int64) string {
	if value < 1024 {
		return strconv.FormatInt(value, 10) + " B"
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	amount := float64(value)
	unit := "B"
	for _, candidate := range units {
		amount /= 1024
		unit = candidate
		if amount < 1024 {
			break
		}
	}
	return strconv.FormatFloat(amount, 'f', 2, 64) + " " + unit
}

func valueOrUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

type usageErr struct{ message string }

func (e *usageErr) Error() string { return e.message }

func usageError(format string, args ...any) error {
	return &usageErr{message: fmt.Sprintf(format, args...)}
}
