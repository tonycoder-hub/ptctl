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

	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/downloader/qbittorrent"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const (
	clientAdoptDefaultTimeout = 24 * time.Hour
	clientAdoptMaximumTimeout = 7 * 24 * time.Hour
)

type clientAdoptFlags struct {
	output               *string
	storeRoot            *string
	variantID            *string
	targetRoot           *string
	materializeOperation *string
	materializePlanID    *string
	hostRoot             *string
	clientRoot           *string
	clientStyle          *string
	driver               *string
	endpoint             *string
	username             *string
	passwordStdin        *bool
	timeout              *time.Duration
	expectedAdoptionPlan *string
	acknowledgeAdd       *bool
	repeatAdd            *bool
}

type preparedClientAdopt struct {
	output   string
	timeout  time.Duration
	prepared *clientadopt.PreparedPlan
	payload  *metastore.ArtifactPayload
	adapter  *qbittorrent.Adapter
	username string
}

func (a *app) clientAdopt(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		a.clientAdoptHelp()
		return nil
	}
	switch args[0] {
	case "plan":
		return a.clientAdoptPlan(args[1:])
	case "run":
		return a.clientAdoptRun(args[1:])
	case "resume":
		return a.clientAdoptResume(args[1:])
	case "status":
		return a.clientAdoptStatus(args[1:])
	default:
		return usageError("unknown client adopt subcommand %q", args[0])
	}
}

func (a *app) clientAdoptHelp() {
	fmt.Fprint(a.stdout, `Usage:
  ptctl client adopt plan --metafile-store DIR --metafile-variant ID --target PATH --materialize-operation ID --materialize-plan-id ID --host-root PATH --client-root PATH --client-style posix|windows --driver qbittorrent --url URL --username USER --password-stdin [--output table|json]
  ptctl client adopt run  [same selectors] --expect-adoption-plan-id ID --acknowledge-client-add [--output table|json]
  ptctl client adopt resume [same selectors] --expect-adoption-plan-id ID [--acknowledge-client-add] [--acknowledge-repeat-add] [--output table|json] OPERATION_ID
  ptctl client adopt status --target PATH [--output table|json] OPERATION_ID

Version 1 only adds an absent exact typed-infohash job in stopped mode. It never
changes an existing job, moves content, rechecks, resumes, deletes, or retires
source data. Exact raw bytes must come from the private metafile store.

Run records a durable target-root-local request intent before its one add POST.
If the response is lost, resume first observes the queue and never repeats the
POST unless --acknowledge-repeat-add is explicit. A successful outcome remains
pending client recheck; qBittorrent cannot expose the stored private variant.
`)
}

func addClientAdoptFlags(fs *flag.FlagSet, execution bool) *clientAdoptFlags {
	values := &clientAdoptFlags{}
	values.output = fs.String("output", "table", "table or json")
	values.storeRoot = fs.String("metafile-store", "", "initialized private metafile store")
	values.variantID = fs.String("metafile-variant", "", "whole-metafile sha256 artifact ID")
	values.targetRoot = fs.String("target", "", "materialized target root")
	values.materializeOperation = fs.String("materialize-operation", "", "committed materialize operation ID")
	values.materializePlanID = fs.String("materialize-plan-id", "", "reviewed materialize plan ID")
	values.hostRoot = fs.String("host-root", "", "host namespace root containing the target")
	values.clientRoot = fs.String("client-root", "", "downloader-visible namespace root")
	values.clientStyle = fs.String("client-style", "posix", "downloader path style: posix or windows")
	values.driver = fs.String("driver", "qbittorrent", "downloader driver")
	values.endpoint = fs.String("url", "", "qBittorrent Web API origin")
	values.username = fs.String("username", "", "qBittorrent username")
	values.passwordStdin = fs.Bool("password-stdin", false, "read downloader password from stdin")
	values.timeout = fs.Duration("timeout", clientAdoptDefaultTimeout, "shared final-proof and client wall-clock budget")
	if execution {
		values.expectedAdoptionPlan = fs.String("expect-adoption-plan-id", "", "reviewed 24-hex client adoption plan ID")
		values.acknowledgeAdd = fs.Bool("acknowledge-client-add", false, "acknowledge one stopped qBittorrent add request")
		values.repeatAdd = fs.Bool("acknowledge-repeat-add", false, "acknowledge repeating a prior request whose result remains unknown")
	}
	return values
}

func prepareClientAdopt(ctx context.Context, fs *flag.FlagSet, values *clientAdoptFlags, command string, loadPayload, allowPositional bool) (preparedClientAdopt, error) {
	var result preparedClientAdopt
	if err := validateOutput(*values.output); err != nil {
		return result, err
	}
	if *values.timeout <= 0 || *values.timeout > clientAdoptMaximumTimeout {
		return result, usageError("--timeout must be greater than zero and no more than 168h")
	}
	if (!allowPositional && fs.NArg() != 0) || (allowPositional && fs.NArg() != 1) {
		return result, usageError("%s has an invalid positional argument count", command)
	}
	if *values.storeRoot == "" || *values.variantID == "" || *values.targetRoot == "" || *values.materializeOperation == "" ||
		*values.materializePlanID == "" || *values.hostRoot == "" || *values.clientRoot == "" || *values.endpoint == "" || *values.username == "" {
		return result, usageError("%s requires every metafile, materialize, mapping, and downloader selector", command)
	}
	if !*values.passwordStdin {
		return result, usageError("%s requires --password-stdin", command)
	}
	if *values.driver != "qbittorrent" {
		return result, usageError("--driver currently supports only qbittorrent")
	}
	if *values.clientStyle != "posix" && *values.clientStyle != "windows" {
		return result, usageError("--client-style must be posix or windows")
	}
	artifactID, err := metastore.ParseArtifactID(*values.variantID)
	if err != nil {
		return result, usageError("%s requires a canonical --metafile-variant", command)
	}
	materializeOperation, err := materialize.ParseOperationID(*values.materializeOperation)
	if err != nil || !validMaterializePlanID(*values.materializePlanID) {
		return result, usageError("%s requires canonical materialize operation and plan IDs", command)
	}
	windows := *values.clientStyle == "windows"
	if err := storage.ValidatePathMappingConfig(*values.hostRoot, *values.clientRoot, windows); err != nil {
		return result, usageError("%s path mapping is invalid: %v", command, err)
	}
	adapter, err := qbittorrent.New(*values.endpoint)
	if err != nil {
		return result, usageError("%s downloader endpoint is invalid", command)
	}
	clientConfigID, err := adapter.ClientConfigID(*values.username)
	if err != nil {
		return result, usageError("%s downloader configuration is invalid", command)
	}
	store, err := metastore.Open(*values.storeRoot)
	if err != nil {
		return result, err
	}
	meta, ref, err := store.Load(ctx, artifactID, metastore.DefaultLimits())
	if errors.Is(err, metastore.ErrCorruptArtifact) {
		return result, &integrityErr{message: "stored metafile failed digest or parse validation"}
	}
	if err != nil {
		return result, err
	}
	if ref.ID != artifactID || meta.MetafileVariantID != artifactID.String() {
		return result, &integrityErr{message: "stored metafile identity disagrees"}
	}
	verified, _, err := materialize.VerifyCurrentFinal(ctx, materialize.FinalProofOptions{
		Meta: meta, TargetRoot: *values.targetRoot, OperationID: materializeOperation,
		ExpectedPlanID: *values.materializePlanID, Limits: materialize.DefaultLimits(),
	})
	if errors.Is(err, materialize.ErrIntegrity) {
		return result, &integrityErr{message: "materialized final failed current exact verification"}
	}
	if err != nil {
		return result, err
	}
	prepared, err := clientadopt.BuildPlan(verified, clientadopt.PlanOptions{
		ClientConfigID: clientConfigID, HostRoot: *values.hostRoot, ClientRoot: *values.clientRoot, ClientWindows: windows,
	})
	if err != nil {
		return result, err
	}
	var payload *metastore.ArtifactPayload
	if loadPayload {
		loaded, loadErr := store.LoadPayload(ctx, artifactID, metastore.DefaultLimits())
		if errors.Is(loadErr, metastore.ErrCorruptArtifact) {
			return result, &integrityErr{message: "stored metafile payload failed digest or parse validation"}
		}
		if loadErr != nil {
			return result, loadErr
		}
		if !loaded.Matches(meta) {
			return result, &integrityErr{message: "stored metafile payload differs from parsed artifact"}
		}
		payload = loaded
	}
	return preparedClientAdopt{
		output: *values.output, timeout: *values.timeout, prepared: prepared, payload: payload,
		adapter: adapter, username: *values.username,
	}, nil
}

func (a *app) clientAdoptPlan(args []string) error {
	fs := newFlagSet("client adopt plan")
	values := addClientAdoptFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return usageError("client adopt plan: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *values.timeout)
	defer cancel()
	prepared, err := prepareClientAdopt(ctx, fs, values, "client adopt plan", false, false)
	if err != nil {
		return err
	}
	credential, err := readDownloaderCredential(a.stdin, prepared.username)
	if err != nil {
		return err
	}
	session, err := prepared.adapter.OpenReadSession(ctx, credential)
	if err != nil {
		requests, _ := downloader.RequestsMadeFromError(err)
		report := clientadopt.FailureReport(prepared.prepared, "", "plan", requests, err)
		return a.finishClientAdopt(prepared.output, report, err)
	}
	defer session.Close()
	report, operationErr := clientadopt.Preview(ctx, prepared.prepared, session)
	return a.finishClientAdopt(prepared.output, report, operationErr)
}

func (a *app) clientAdoptRun(args []string) error {
	fs := newFlagSet("client adopt run")
	values := addClientAdoptFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		return usageError("client adopt run: %v", err)
	}
	if !validMaterializePlanID(*values.expectedAdoptionPlan) || !*values.acknowledgeAdd || *values.repeatAdd {
		return usageError("client adopt run requires --expect-adoption-plan-id and --acknowledge-client-add; repeat acknowledgement is resume-only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *values.timeout)
	defer cancel()
	prepared, err := prepareClientAdopt(ctx, fs, values, "client adopt run", true, false)
	if err != nil {
		return err
	}
	if err := clientadopt.PreflightRun(ctx, prepared.prepared, *values.expectedAdoptionPlan, prepared.payload); err != nil {
		report := clientadopt.FailureReport(prepared.prepared, *values.expectedAdoptionPlan, "run", 0, err)
		return a.finishClientAdopt(prepared.output, report, err)
	}
	credential, err := readDownloaderCredential(a.stdin, prepared.username)
	if err != nil {
		return err
	}
	session, err := prepared.adapter.OpenMutationSession(ctx, credential)
	if err != nil {
		requests, _ := downloader.RequestsMadeFromError(err)
		report := clientadopt.FailureReport(prepared.prepared, *values.expectedAdoptionPlan, "run", requests, err)
		return a.finishClientAdopt(prepared.output, report, err)
	}
	defer session.Close()
	report, operationErr := clientadopt.Run(ctx, clientadopt.RunOptions{
		Prepared: prepared.prepared, ExpectedPlanID: *values.expectedAdoptionPlan, Metafile: prepared.payload,
		Session: session, AcknowledgeAdd: true,
	})
	return a.finishClientAdopt(prepared.output, report, operationErr)
}

func (a *app) clientAdoptResume(args []string) error {
	fs := newFlagSet("client adopt resume")
	values := addClientAdoptFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		return usageError("client adopt resume: %v", err)
	}
	if fs.NArg() != 1 || !validMaterializePlanID(*values.expectedAdoptionPlan) {
		return usageError("client adopt resume requires one OPERATION_ID and canonical --expect-adoption-plan-id")
	}
	if *values.repeatAdd && !*values.acknowledgeAdd {
		return usageError("--acknowledge-repeat-add also requires --acknowledge-client-add")
	}
	operationID, err := clientadopt.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("client adopt resume requires a canonical OPERATION_ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *values.timeout)
	defer cancel()
	prepared, err := prepareClientAdopt(ctx, fs, values, "client adopt resume", true, true)
	if err != nil {
		return err
	}
	// Journal selection and plan matching are checked before password stdin or
	// any downloader request. Status is strictly read-only.
	status, statusErr := clientadopt.Status(ctx, clientadopt.StatusOptions{TargetRoot: *values.targetRoot, OperationID: operationID})
	initializationIncomplete := errors.Is(statusErr, clientadopt.ErrInitializationIncomplete)
	if statusErr != nil && !initializationIncomplete || !initializationIncomplete && status.Plan.ID != prepared.prepared.PlanID() || operationID != prepared.prepared.OperationID() {
		if statusErr == nil {
			statusErr = fmt.Errorf("%w: operation belongs to a different adoption plan", clientadopt.ErrPolicy)
		}
		report := clientadopt.FailureReport(prepared.prepared, *values.expectedAdoptionPlan, "resume", 0, statusErr)
		return a.finishClientAdopt(prepared.output, report, statusErr)
	}
	if (initializationIncomplete || status.Journal.AttemptsRecorded == 0 && !status.Journal.CompletionDurable) && !*values.acknowledgeAdd {
		statusErr = fmt.Errorf("%w: this operation has no add request intent; resume requires explicit downloader-add acknowledgement", clientadopt.ErrPolicy)
		report := clientadopt.FailureReport(prepared.prepared, *values.expectedAdoptionPlan, "resume", 0, statusErr)
		return a.finishClientAdopt(prepared.output, report, statusErr)
	}
	credential, err := readDownloaderCredential(a.stdin, prepared.username)
	if err != nil {
		return err
	}
	session, err := prepared.adapter.OpenMutationSession(ctx, credential)
	if err != nil {
		requests, _ := downloader.RequestsMadeFromError(err)
		report := clientadopt.FailureReport(prepared.prepared, *values.expectedAdoptionPlan, "resume", requests, err)
		return a.finishClientAdopt(prepared.output, report, err)
	}
	defer session.Close()
	report, operationErr := clientadopt.Resume(ctx, operationID, clientadopt.RunOptions{
		Prepared: prepared.prepared, ExpectedPlanID: *values.expectedAdoptionPlan, Metafile: prepared.payload,
		Session: session, AcknowledgeAdd: *values.acknowledgeAdd, RepeatAdd: *values.repeatAdd,
	})
	return a.finishClientAdopt(prepared.output, report, operationErr)
}

func (a *app) clientAdoptStatus(args []string) error {
	fs := newFlagSet("client adopt status")
	output := fs.String("output", "table", "table or json")
	targetRoot := fs.String("target", "", "materialized target root")
	timeout := fs.Duration("timeout", time.Hour, "bounded private journal read timeout")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *targetRoot == "" {
		return usageError("client adopt status requires --target and one OPERATION_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > time.Hour {
		return usageError("client adopt status --timeout must be in (0,1h]")
	}
	operationID, err := clientadopt.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("client adopt status requires a canonical OPERATION_ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, operationErr := clientadopt.Status(ctx, clientadopt.StatusOptions{TargetRoot: *targetRoot, OperationID: operationID})
	return a.finishClientAdopt(*output, report, operationErr)
}

func (a *app) finishClientAdopt(output string, report clientadopt.Report, operationErr error) error {
	if err := a.writeClientAdoptReport(output, report); err != nil {
		return err
	}
	if operationErr == nil {
		return nil
	}
	if errors.Is(operationErr, clientadopt.ErrIntegrity) || errors.Is(operationErr, materialize.ErrIntegrity) {
		return &integrityErr{message: "client adoption failed integrity validation; see report"}
	}
	if errors.Is(operationErr, clientadopt.ErrPolicy) || errors.Is(operationErr, clientadopt.ErrRequestUnknown) ||
		errors.Is(operationErr, clientadopt.ErrOperationNotFound) || errors.Is(operationErr, clientadopt.ErrInitializationIncomplete) {
		return &inconclusiveErr{message: "client adoption is not complete; see report"}
	}
	return fmt.Errorf("client adoption was interrupted; see report")
}

func (a *app) writeClientAdoptReport(output string, report clientadopt.Report) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	return writeClientAdoptHuman(a.stdout, report)
}

func writeClientAdoptHuman(out io.Writer, report clientadopt.Report) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nWRITES UNCERTAIN\t%t\nOPERATION ID\t%s\nOPERATION STATUS\t%s\nPHASE BEFORE\t%s\nPHASE AFTER\t%s\nRESUMABLE\t%t\n",
		terminalSafe(string(report.Outcome)), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed, report.WritesUncertain,
		terminalSafe(valueOrUnknown(report.Operation.ID)), terminalSafe(report.Operation.Status), terminalSafe(report.Operation.PhaseBefore), terminalSafe(report.Operation.PhaseAfter), report.Operation.Resumable)
	fmt.Fprintln(w, "\nBLOCKERS")
	writeClientAdoptFindings(w, report.Blockers)
	fmt.Fprintf(w, "\nPLAN\nID\t%s\nEXPECTED ID\t%s\nMATCHES\t%t\nACTION\t%s\nCLIENT CONFIG\t%s\nPATH MAPPING\t%s\nPATH SEMANTICS\t%s\nSAVE PATH REF\t%s\nCONTENT PATH REF\t%s\n",
		terminalSafe(valueOrUnknown(report.Plan.ID)), terminalSafe(materializeValueOr(report.Plan.ExpectedID, "not_requested")), report.Plan.Matches,
		terminalSafe(report.Plan.Action), terminalSafe(report.Plan.ClientConfigID), terminalSafe(report.Plan.PathMappingID), terminalSafe(report.Plan.ClientPathSemantics),
		terminalSafe(report.Plan.ExpectedSavePathRef), terminalSafe(report.Plan.ExpectedContentPathRef))
	fmt.Fprintf(w, "\nMATERIALIZED FINAL\nSTATUS\t%s\nVARIANT\t%s\nMATERIALIZE OPERATION\t%s\nMATERIALIZE PLAN\t%s\nROOT IDENTITY\t%s\nFINAL IDENTITY\t%s\nBYTES VERIFIED\t%d\nASSURANCE\t%s\n",
		terminalSafe(report.Final.Status), terminalSafe(report.Final.Observation.MetafileVariantID), terminalSafe(report.Final.Observation.OperationID),
		terminalSafe(report.Final.Observation.MaterializePlanID), terminalSafe(report.Final.Observation.TargetRootIdentity), terminalSafe(report.Final.Observation.FinalObjectIdentity),
		report.Final.Observation.BytesVerified, terminalSafe(report.Final.Observation.Assurance))
	fmt.Fprintf(w, "\nCLIENT\nSTATUS\t%s\nREQUESTS MADE\t%d\nBEFORE IDENTITY\t%s\nAFTER IDENTITY\t%s\nADD ATTEMPTED\t%t\nADD COMPLETE\t%t\nAUTOMATIC RETRIES\t%d\nREDIRECTS\t%d\nJOB ID\t%s\nJOB STATE\t%s\nCONTENT PATH REF\t%s\nVARIANT RELATION\t%s\nASSURANCE\t%s\n",
		terminalSafe(report.Client.Status), report.Client.RequestsMade, terminalSafe(report.Client.BeforeIdentity), terminalSafe(report.Client.AfterIdentity),
		report.Client.AddAttempted, report.Client.AddReceipt.Complete, report.Client.AddReceipt.AutomaticRetries, report.Client.AddReceipt.RedirectsFollowed,
		terminalSafe(materializeValueOr(report.Client.JobID, "not_observed")), terminalSafe(materializeValueOr(report.Client.JobState, "not_observed")),
		terminalSafe(materializeValueOr(report.Client.ContentPathRef, "not_observed")), terminalSafe(report.Client.VariantRelation), terminalSafe(report.Client.Assurance))
	fmt.Fprintf(w, "\nJOURNAL / WRITES\nSTATUS\t%s\nINTENT DURABLE\t%t\nATTEMPTS RECORDED\t%d\nCOMPLETION DURABLE\t%t\nPENDING RECOVERY MARKER\t%t\nOPERATION DIRECTORIES\t%d\nCONTROL DIRECTORIES\t%d\nTEMPORARY FILES\t%d\nTEMPORARY BYTES\t%d\nMARKER PUBLICATIONS\t%d\nTEMPORARY REMOVALS\t%d\nAMBIGUOUS PUBLICATIONS\t%d\n",
		terminalSafe(report.Journal.Status), report.Journal.IntentDurable, report.Journal.AttemptsRecorded, report.Journal.CompletionDurable, report.Journal.PendingRecoveryMarker,
		report.Writes.OperationDirectories, report.Writes.ControlDirectories, report.Writes.TemporaryFiles, report.Writes.TemporaryBytes,
		report.Writes.MarkerPublications, report.Writes.TemporaryRemovals, report.Writes.AmbiguousPublications)
	fmt.Fprintln(w, "\nISSUES")
	writeClientAdoptFindings(w, report.Issues)
	fmt.Fprintln(w, "\nWARNINGS")
	if len(report.Warnings) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
	}
	return w.Flush()
}

func writeClientAdoptFindings(w io.Writer, findings []clientadopt.Finding) {
	if len(findings) == 0 {
		fmt.Fprintln(w, "-\tnone")
		return
	}
	for _, finding := range findings {
		fmt.Fprintf(w, "-\t%s\t%s\n", terminalSafe(finding.Code), terminalSafe(finding.Message))
	}
}
