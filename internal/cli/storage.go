package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/tonycoder-hub/ptctl/internal/storage"
)

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
		if err := validateOutput(*output); err != nil {
			return err
		}
		result, err := storage.ProbeReadOnly(fs.Arg(0))
		if err != nil {
			return err
		}
		if *output == "json" {
			return writeJSON(a.stdout, result, nil)
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
		if err := validateOutput(*output); err != nil {
			return err
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
		fmt.Fprintf(a.stdout, "%s\n", terminalSafe(result.ClientPath))
		return nil
	case "profile":
		return a.storageProfileCommand(args[1:])
	case "index":
		return a.storageIndexCommand(args[1:])
	default:
		return usageError("unknown storage subcommand %q", args[0])
	}
}

func writeProbeHuman(out io.Writer, result storage.ProbeResult) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "PATH\t%s\nRESOLVED\t%s\nDIRECTORY\t%t\nSEMANTICS EVIDENCE\t%s\nRANDOM READ\t%s\nSEEDABLE VIEW\t%s\nWRITE PROBE\t%s\n", terminalSafe(result.Path), terminalSafe(result.ResolvedPath), result.Directory, terminalSafe(result.SemanticsEvidence), terminalSafe(result.RandomRead), terminalSafe(result.SeedableView), terminalSafe(result.WriteProbe))
	for _, warning := range result.Warnings {
		fmt.Fprintf(w, "WARNING\t%s\n", terminalSafe(warning))
	}
	return w.Flush()
}
