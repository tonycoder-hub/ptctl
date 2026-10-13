package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/tonycoder-hub/ptctl/internal/metafile"
)

func (a *app) torrent(args []string) error {
	if len(args) == 0 {
		return usageError("torrent subcommand is required")
	}
	switch args[0] {
	case "inspect":
		fs := newFlagSet("torrent inspect")
		output := fs.String("output", "table", "table or json")
		storeRoot := fs.String("metafile-store", "", "private metafile store root; pair with --metafile-variant")
		variantID := fs.String("metafile-variant", "", "whole-metafile sha256 artifact ID; pair with --metafile-store")
		if err := fs.Parse(args[1:]); err != nil {
			return usageError("torrent inspect: %v", err)
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		input, err := positionalMetafileInput("torrent inspect", fs.Args(), *storeRoot, *variantID, flagWasSet(fs, "metafile-store"), flagWasSet(fs, "metafile-variant"))
		if err != nil {
			return err
		}
		meta, err := loadMetafileInput(context.Background(), input)
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
		storeRoot := fs.String("metafile-store", "", "private metafile store root; pair with --metafile-variant")
		variantID := fs.String("metafile-variant", "", "whole-metafile sha256 artifact ID; pair with --metafile-store")
		if err := fs.Parse(args[1:]); err != nil {
			return usageError("torrent verify: %v", err)
		}
		if *content == "" {
			return usageError("torrent verify requires --content PATH")
		}
		if *output != "table" && *output != "json" {
			return usageError("--output must be table or json")
		}
		input, err := positionalMetafileInput("torrent verify", fs.Args(), *storeRoot, *variantID, flagWasSet(fs, "metafile-store"), flagWasSet(fs, "metafile-variant"))
		if err != nil {
			return err
		}
		meta, err := loadMetafileInput(context.Background(), input)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result, err := metafile.Verify(ctx, meta, *content)
		if err != nil {
			return err
		}
		if *output == "json" {
			err = writeJSON(a.stdout, result, nil)
		} else {
			err = writeVerifyHuman(a.stdout, result)
		}
		if err != nil {
			return err
		}
		if !result.Verified {
			return &integrityErr{message: fmt.Sprintf("content failed exact torrent verification (%d of %d piece proofs matched)", result.PiecesMatched, result.PiecesExpected)}
		}
		return nil
	default:
		return usageError("unknown torrent subcommand %q", args[0])
	}
}

func writeMetaHuman(out io.Writer, meta *metafile.MetaInfo) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "NAME\t%s\nVERSION\t%s\nVALIDATION\t%s\nMETAFILE VARIANT\t%s\nPRIVATE\t%t\nTOTAL\t%s (%d bytes)\nPIECE LENGTH\t%s\n", terminalSafe(meta.Name), terminalSafe(meta.Version), terminalSafe(meta.Validation), terminalSafe(meta.MetafileVariantID), meta.Private, humanBytes(meta.TotalLength), meta.TotalLength, humanBytes(meta.PieceLength))
	if meta.InfoHashV1 != "" {
		fmt.Fprintf(w, "V1 PIECES\t%d\n", meta.V1PieceCount)
	}
	if meta.InfoHashV1 != "" {
		fmt.Fprintf(w, "INFOHASH V1\t%s\n", terminalSafe(meta.InfoHashV1))
	}
	if meta.InfoHashV2 != "" {
		fmt.Fprintf(w, "INFOHASH V2\t%s\n", terminalSafe(meta.InfoHashV2))
	}
	for _, tracker := range meta.Trackers {
		fmt.Fprintf(w, "TRACKER ORIGIN\t%s\n", terminalSafe(tracker))
	}
	fmt.Fprintln(w, "\nBYTES\tPATH")
	for _, file := range meta.Files {
		fmt.Fprintf(w, "%d\t%s\n", file.Length, terminalSafe(strings.Join(file.Path, "/")))
	}
	return w.Flush()
}

func writeVerifyHuman(out io.Writer, result metafile.VerificationResult) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "VERIFIED\t%t\nVERSION\t%s\nEVIDENCE\t%s\nSNAPSHOT\t%s\nSTABILITY\t%s\nBYTES\t%d\nPHYSICAL FILES\t%d\nVIRTUAL PADDING\t%d\n", result.Verified, terminalSafe(result.Version), terminalSafe(result.Evidence), terminalSafe(result.SourceSnapshotID), terminalSafe(result.StabilityAssurance), result.BytesVerified, result.FilesChecked, result.PaddingBytes)
	for _, check := range result.Checks {
		status := "fail"
		if check.Verified {
			status = "pass"
		}
		fmt.Fprintf(w, "\nCHECK\t%s\nSTATUS\t%s\nEVIDENCE\t%s\nPIECES\t%d/%d\n", terminalSafe(check.Algorithm), status, terminalSafe(check.Evidence), check.PiecesMatched, check.PiecesExpected)
		if check.RootsExpected > 0 {
			fmt.Fprintf(w, "FILE ROOTS\t%d/%d\n", check.RootsMatched, check.RootsExpected)
		}
		if len(check.MismatchPieces) > 0 {
			fmt.Fprintf(w, "PROOF MISMATCHES\t%v", check.MismatchPieces)
			if check.MismatchOverflow > 0 {
				fmt.Fprintf(w, " (+%d more)", check.MismatchOverflow)
			}
			fmt.Fprintln(w)
		}
	}
	return w.Flush()
}
