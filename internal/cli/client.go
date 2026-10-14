package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/downloader/qbittorrent"
	"github.com/tonycoder-hub/ptctl/internal/downloader/transmission"
)

type readOnlyDownloaderDriver interface {
	downloader.Driver
	configuredLedgerDriver
}

type configuredLedgerDriver interface {
	downloader.LedgerDriver
	ClientConfigID(username string) (string, error)
}

func newReadOnlyDownloaderDriver(name, endpoint string) (readOnlyDownloaderDriver, error) {
	switch name {
	case downloader.DriverQBittorrent:
		return qbittorrent.New(endpoint)
	case downloader.DriverTransmission:
		return transmission.New(endpoint)
	default:
		return nil, fmt.Errorf("unsupported read-only downloader driver %q", name)
	}
}

func (a *app) client(args []string) error {
	if groupHelpRequested(args) {
		return a.helpTopic([]string{"client"})
	}
	if len(args) > 0 && args[0] == "adopt" {
		return a.clientAdopt(args[1:])
	}
	if len(args) > 0 && args[0] == "activate" {
		return a.clientActivate(args[1:])
	}
	if len(args) > 0 && args[0] == "remove" {
		return a.clientRemove(args[1:])
	}
	if len(args) > 0 && args[0] == "stop" {
		return a.clientStop(args[1:])
	}
	if len(args) == 0 || (args[0] != "status" && args[0] != "list") {
		return usageError("client requires status, list, adopt, activate, stop, or remove")
	}
	command := args[0]
	fs := newFlagSet("client " + command)
	output := fs.String("output", "table", "table or json")
	driverName := fs.String("driver", downloader.DriverQBittorrent, "read-only downloader driver: qbittorrent or transmission")
	endpoint := fs.String("url", "", "downloader API origin or RPC URL")
	username := fs.String("username", "", "downloader username")
	passwordStdin := fs.Bool("password-stdin", false, "read password from stdin")
	if done, err := a.parseReadOnlyFlags(fs, args[1:], "client "+command+" --driver qbittorrent|transmission --url URL --username USER --password-stdin [--output table|json]"); done || err != nil {
		return err
	}
	if fs.NArg() != 0 || *endpoint == "" {
		return usageError("client %s requires --url and no positional arguments", command)
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if _, ok := downloader.DescribeLedgerDriver(*driverName); !ok {
		return usageError("--driver must be qbittorrent or transmission for read-only client commands")
	}
	if !*passwordStdin {
		return usageError("--password-stdin is required; downloader passwords are never accepted in argv")
	}
	driver, err := newReadOnlyDownloaderDriver(*driverName, *endpoint)
	if err != nil {
		return err
	}
	credential, err := readDownloaderCredential(a.stdin, *username)
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

func writeClientHuman(out io.Writer, command string, data any) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if command == "status" {
		status := data.(downloader.Status)
		fmt.Fprintf(w, "DRIVER\t%s\nENDPOINT\t%s\nVERSION\t%s\nWEB API\t%s\nOBSERVED\t%s\n", terminalSafe(status.Driver), terminalSafe(status.Endpoint), terminalSafe(status.Version), terminalSafe(status.WebAPIVersion), status.ObservedAt.Format(time.RFC3339))
		return w.Flush()
	}
	fmt.Fprintln(w, "HASH\tPROGRESS\tSTATE\tSIZE\tNAME\tSAVE PATH")
	for _, item := range data.([]downloader.Torrent) {
		fmt.Fprintf(w, "%s\t%.1f%%\t%s\t%s\t%s\t%s\n", terminalSafe(item.Hash), item.Progress*100, terminalSafe(item.State), humanBytes(item.SizeBytes), terminalSafe(item.Name), terminalSafe(item.SavePath))
	}
	return w.Flush()
}
