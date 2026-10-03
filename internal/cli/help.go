package cli

import (
	"fmt"
	"strings"
)

func groupHelpRequested(args []string) bool {
	return len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help")
}

// Help topics use an explicit read-only allowlist. Never dispatch arbitrary
// command arguments from a help request, especially credential or write flags.
func (a *app) helpTopic(args []string) error {
	switch strings.Join(args, " ") {
	case "":
		a.help()
	case "config":
		fmt.Fprint(a.stdout, `Configuration (explicit inputs; no automatic discovery)

pt cli does not search your home/project directories for configuration or credentials.
There is no --config file or saved downloader-login profile.

Downloader reads:
  --driver qbittorrent|transmission  (default: qbittorrent)
  --url URL                         (required; no default endpoint)
  --username USER --password-stdin   (password comes only from stdin)
  HTTPS is required except for explicit numeric loopback HTTP addresses.

Files and paths:
  Relative paths are resolved from the current working directory.
  Single-file torrents need --content/--source pointing to the actual file.
  Multi-file torrents need the root containing their relative paths.
  Downloader path matching uses explicit --host-root, --client-root and --client-style.
  Metafile stores and storage profiles are opt-in explicit-path operations;
  torrent inspect/verify and exact-source reconciliation need no store setup.

Put flags before positional arguments. See 'pt help client list' and
'pt help reconcile report' for flags, or docs/READ_ONLY_QUICKSTART.md for examples.
`)
	case "torrent":
		fmt.Fprint(a.stdout, `Usage:
  pt torrent inspect [flags] FILE.torrent
  pt torrent verify --content PATH [flags] FILE.torrent

inspect reads metadata; verify proves exact v1/v2/hybrid content. Neither writes files.
Put flags before FILE.torrent. PATH is the actual file for single-file torrents,
or the containing root for multi-file torrents. Use --output json for automation.

Details: pt help torrent inspect | pt help torrent verify
`)
	case "client":
		fmt.Fprint(a.stdout, `Usage:
  pt client status --url URL --username USER --password-stdin [flags]
  pt client list --url URL --username USER --password-stdin [flags]

These commands read qBittorrent or Transmission state. They do not change jobs.
No endpoints or credentials are discovered automatically; see 'pt help config'.
Other client commands (adopt/activate/stop/remove) have separate mutation gates;
see 'pt help' for the complete command list.

Details: pt help client status | pt help client list
`)
	case "reconcile":
		fmt.Fprint(a.stdout, `Usage:
  pt reconcile report --torrent FILE.torrent --source PATH [flags]

Proves the selected files and compares explicitly supplied downloader/path ledgers.
A local-only report is partial: verified content alone is not a downloader match.
--require-reconciled returns 4 unless the report is consistent; the report is still printed.
report is read-only. The separate refresh-report command writes a storage-index snapshot.

Details: pt help reconcile report | pt help config
`)
	case "version":
		return a.version([]string{"--help"})
	case "torrent inspect":
		return a.torrent([]string{"inspect", "--help"})
	case "torrent verify":
		return a.torrent([]string{"verify", "--help"})
	case "client list":
		return a.client([]string{"list", "--help"})
	case "client status":
		return a.client([]string{"status", "--help"})
	case "reconcile report":
		return a.reconcileCommand([]string{"report", "--help"})
	default:
		return usageError("unknown help topic; use torrent, client, reconcile, config, version, or 'pt help' for the complete command list")
	}
	return nil
}
