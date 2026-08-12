package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/clientstop"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/downloader/qbittorrent"
	"github.com/tonycoder-hub/ptctl/internal/downloader/transmission"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const (
	clientStopDefaultTimeout = 24 * time.Hour
	clientStopMaximumTimeout = 7 * 24 * time.Hour
)

type clientStopFlags struct {
	output, storeRoot, variantID, targetRoot, materializeOperation, materializePlanID *string
	activationOperation, activationPlanID, hostRoot, clientRoot, clientStyle          *string
	driver, endpoint, username                                                        *string
	passwordStdin                                                                     *bool
	timeout                                                                           *time.Duration
}

type preparedClientStop struct {
	output, storeRoot, targetRoot, materializePlanID, activationPlanID string
	hostRoot, clientRoot, clientConfigID, username                     string
	artifactID                                                         metastore.ArtifactID
	materializeOperation                                               materialize.OperationID
	activationOperation                                                clientactivate.OperationID
	clientWindows                                                      bool
	adapter                                                            downloader.ExistingJobStopDriver
	timeout                                                            time.Duration
}

func (a *app) clientStop(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		a.clientStopHelp()
		return nil
	}
	switch args[0] {
	case "plan":
		return a.clientStopPlan(args[1:])
	case "run":
		return a.clientStopRun(args[1:])
	case "resume":
		return a.clientStopResume(args[1:])
	case "status":
		return a.clientStopStatus(args[1:])
	default:
		return usageError("unknown client stop subcommand %q", args[0])
	}
}

func (a *app) clientStopHelp() {
	fmt.Fprint(a.stdout, `Usage:
  ptctl client stop plan --metafile-store DIR --metafile-variant ID --target PATH --materialize-operation ID --materialize-plan-id ID --activation-operation ID --activation-plan-id ID --host-root PATH --client-root PATH --client-style posix|windows --driver qbittorrent|transmission --url URL --username USER --password-stdin [--output table|json]
  ptctl client stop run [same selectors] --expect-stop-plan-id ID --acknowledge-client-stop [--output table|json]
  ptctl client stop resume [same selectors] --expect-stop-plan-id ID [--acknowledge-client-stop --acknowledge-repeat-stop] [--output table|json] OPERATION_ID
  ptctl client stop status --target PATH --expect-stop-plan-id ID [--output table|json] OPERATION_ID

Plan is read-only. Run proves one complete started exact typed job on the
reviewed materialized layout, journals intent, and sends one non-retried stop
request. A response is not completion: the same session must observe that same
job stopped with the same complete file layout, followed by exact final-file
reverification. Resume observes first; a second request requires both stop and
repeat acknowledgements. It may first recover one exact pending local marker;
this never sends a downloader request. Status is local-only historical journal
inspection. Stop journals have no prune/forget command in this slice.
`)
}

func addClientStopFlags(fs *flag.FlagSet) *clientStopFlags {
	values := &clientStopFlags{}
	values.output = fs.String("output", "table", "table or json")
	values.storeRoot = fs.String("metafile-store", "", "initialized private metafile store")
	values.variantID = fs.String("metafile-variant", "", "whole-metafile sha256 artifact ID")
	values.targetRoot = fs.String("target", "", "materialized target root")
	values.materializeOperation = fs.String("materialize-operation", "", "committed materialize operation ID")
	values.materializePlanID = fs.String("materialize-plan-id", "", "reviewed materialize plan ID")
	values.activationOperation = fs.String("activation-operation", "", "completed client activation operation ID")
	values.activationPlanID = fs.String("activation-plan-id", "", "reviewed client activation plan ID")
	values.hostRoot = fs.String("host-root", "", "host namespace root containing the target")
	values.clientRoot = fs.String("client-root", "", "downloader-visible namespace root")
	values.clientStyle = fs.String("client-style", "posix", "downloader path style: posix or windows")
	values.driver = fs.String("driver", downloader.DriverQBittorrent, "downloader driver: qbittorrent or transmission")
	values.endpoint = fs.String("url", "", "downloader API origin or Transmission RPC URL")
	values.username = fs.String("username", "", "downloader username")
	values.passwordStdin = fs.Bool("password-stdin", false, "read downloader password from stdin")
	values.timeout = fs.Duration("timeout", clientStopDefaultTimeout, "shared local proof, journal, and downloader wall-clock budget")
	return values
}

func prepareClientStopFlags(fs *flag.FlagSet, values *clientStopFlags, command string, positional int) (preparedClientStop, error) {
	var result preparedClientStop
	if fs.NArg() != positional {
		return result, usageError("%s has an invalid positional argument count", command)
	}
	if err := validateOutput(*values.output); err != nil {
		return result, err
	}
	if *values.timeout <= 0 || *values.timeout > clientStopMaximumTimeout {
		return result, usageError("--timeout must be greater than zero and no more than 168h")
	}
	if *values.storeRoot == "" || *values.variantID == "" || *values.targetRoot == "" || *values.materializeOperation == "" ||
		*values.materializePlanID == "" || *values.activationOperation == "" || *values.activationPlanID == "" ||
		*values.hostRoot == "" || *values.clientRoot == "" || *values.endpoint == "" || *values.username == "" {
		return result, usageError("%s requires every metafile, materialize, activation, mapping, and downloader selector", command)
	}
	if !*values.passwordStdin {
		return result, usageError("%s requires --password-stdin", command)
	}
	if *values.driver != downloader.DriverQBittorrent && *values.driver != downloader.DriverTransmission {
		return result, usageError("--driver must be qbittorrent or transmission")
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
	activationOperation, err := clientactivate.ParseOperationID(*values.activationOperation)
	if err != nil || !validMaterializePlanID(*values.activationPlanID) {
		return result, usageError("%s requires canonical activation operation and plan IDs", command)
	}
	windows := *values.clientStyle == "windows"
	if err := storage.ValidatePathMappingConfig(*values.hostRoot, *values.clientRoot, windows); err != nil {
		return result, usageError("%s path mapping is invalid: %v", command, err)
	}
	adapter, err := newExistingJobStopDriver(*values.driver, *values.endpoint)
	if err != nil {
		return result, usageError("%s downloader endpoint is invalid", command)
	}
	clientConfigID, err := adapter.ClientConfigID(*values.username)
	if err != nil {
		return result, usageError("%s downloader configuration is invalid", command)
	}
	return preparedClientStop{output: *values.output, storeRoot: *values.storeRoot, artifactID: artifactID,
		targetRoot: *values.targetRoot, materializeOperation: materializeOperation, materializePlanID: *values.materializePlanID,
		activationOperation: activationOperation, activationPlanID: *values.activationPlanID, hostRoot: *values.hostRoot,
		clientRoot: *values.clientRoot, clientWindows: windows, clientConfigID: clientConfigID, adapter: adapter,
		username: *values.username, timeout: *values.timeout}, nil
}

func newExistingJobStopDriver(name, endpoint string) (downloader.ExistingJobStopDriver, error) {
	switch name {
	case downloader.DriverQBittorrent:
		return qbittorrent.New(endpoint)
	case downloader.DriverTransmission:
		return transmission.New(endpoint)
	default:
		return nil, fmt.Errorf("unsupported existing-job stop driver %q", name)
	}
}

func (a *app) prepareClientStopLocal(ctx context.Context, prepared preparedClientStop) (*clientactivate.CurrentUseAuthority, error) {
	store, err := metastore.Open(prepared.storeRoot)
	if err != nil {
		return nil, fmt.Errorf("private metafile store could not be opened")
	}
	meta, ref, err := store.Load(ctx, prepared.artifactID, metastore.DefaultLimits())
	if errors.Is(err, metastore.ErrCorruptArtifact) {
		return nil, &integrityErr{message: "client stop metafile failed digest or parse validation"}
	}
	if err != nil {
		return nil, fmt.Errorf("client stop metafile could not be read")
	}
	if ref.ID != prepared.artifactID || meta.MetafileVariantID != prepared.artifactID.String() {
		return nil, &integrityErr{message: "client stop metafile identity disagrees"}
	}
	final, _, err := materialize.VerifyCurrentFinal(ctx, materialize.FinalProofOptions{Meta: meta, TargetRoot: prepared.targetRoot,
		OperationID: prepared.materializeOperation, ExpectedPlanID: prepared.materializePlanID, Limits: materialize.DefaultLimits()})
	if errors.Is(err, materialize.ErrIntegrity) {
		return nil, &integrityErr{message: "client stop materialized final failed current exact verification"}
	}
	if err != nil {
		return nil, fmt.Errorf("client stop materialized final could not be verified")
	}
	activation, _, err := clientactivate.VerifyCompletion(ctx, clientactivate.CompletionProofOptions{TargetRoot: prepared.targetRoot,
		OperationID: prepared.activationOperation, ExpectedPlanID: prepared.activationPlanID})
	if errors.Is(err, clientactivate.ErrIntegrity) {
		return nil, &integrityErr{message: "client stop activation journal failed integrity validation"}
	}
	if err != nil {
		return nil, fmt.Errorf("client stop terminal activation could not be verified")
	}
	authority, err := clientactivate.PrepareCurrentUse(final, activation, clientactivate.CurrentUseOptions{
		ClientConfigID: prepared.clientConfigID, HostRoot: prepared.hostRoot, ClientRoot: prepared.clientRoot,
		ClientWindows: prepared.clientWindows, FileLimits: downloader.DefaultJobFileLedgerLimits()})
	if errors.Is(err, clientactivate.ErrIntegrity) {
		return nil, &integrityErr{message: "client stop selectors disagree with terminal activation"}
	}
	if errors.Is(err, clientactivate.ErrPolicy) {
		return nil, &inconclusiveErr{message: "client stop selectors do not reproduce reviewed activation"}
	}
	return authority, err
}

func (a *app) openClientStop(ctx context.Context, prepared preparedClientStop, phase, expected string, operation clientstop.OperationID) (*clientactivate.CurrentUseAuthority, downloader.ExistingJobStopSession, error) {
	authority, err := a.prepareClientStopLocal(ctx, prepared)
	if err != nil {
		return nil, nil, err
	}
	credential, err := readDownloaderCredential(a.stdin, prepared.username)
	if err != nil {
		return nil, nil, err
	}
	session, err := prepared.adapter.OpenExistingJobStopSession(ctx, credential)
	if err != nil {
		requests, _ := downloader.RequestsMadeFromError(err)
		report := clientstop.FailureReport(expected, operation, phase, err)
		report.RecordSessionRequests(requests)
		return nil, nil, &clientStopReportedError{report: report, err: err, output: prepared.output}
	}
	return authority, session, nil
}

type clientStopReportedError struct {
	report clientstop.Report
	err    error
	output string
}

func (value *clientStopReportedError) Error() string {
	return "client stop session could not be opened"
}
func (value *clientStopReportedError) Unwrap() error { return value.err }

func (a *app) clientStopPlan(args []string) error {
	fs := newFlagSet("client stop plan")
	values := addClientStopFlags(fs)
	if err := fs.Parse(args); err != nil {
		return usageError("client stop plan: %v", err)
	}
	prepared, err := prepareClientStopFlags(fs, values, "client stop plan", 0)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), prepared.timeout)
	defer cancel()
	authority, session, err := a.openClientStop(ctx, prepared, "plan", "", "")
	if reported, ok := err.(*clientStopReportedError); ok {
		return a.finishClientStop(reported.output, reported.report, reported.err)
	}
	if err != nil {
		return err
	}
	defer session.Close()
	descriptor, err := session.ReadExistingJobStopDescriptor(ctx)
	if err != nil {
		report := clientstop.FailureReport("", "", "plan", err)
		report.RecordSessionRequests(session.RequestsMade())
		return a.finishClientStop(prepared.output, report, err)
	}
	current, _, err := clientactivate.VerifyCurrentUse(ctx, authority, session)
	if err != nil {
		report := clientstop.FailureReport("", "", "plan", err)
		report.RecordSessionRequests(session.RequestsMade())
		return a.finishClientStop(prepared.output, report, err)
	}
	review, err := clientstop.BuildPlan(current, descriptor)
	if err != nil {
		report := clientstop.FailureReport("", "", "plan", err)
		report.RecordSessionRequests(session.RequestsMade())
		return a.finishClientStop(prepared.output, report, err)
	}
	report, operationErr := clientstop.Preview(review)
	report.RecordSessionRequests(session.RequestsMade())
	return a.finishClientStop(prepared.output, report, operationErr)
}

func (a *app) clientStopRun(args []string) error {
	fs := newFlagSet("client stop run")
	values := addClientStopFlags(fs)
	expected := fs.String("expect-stop-plan-id", "", "reviewed 24-hex client stop plan ID")
	ack := fs.Bool("acknowledge-client-stop", false, "acknowledge one exact-job stop request")
	if err := fs.Parse(args); err != nil {
		return usageError("client stop run: %v", err)
	}
	if !validMaterializePlanID(*expected) || !*ack {
		return usageError("client stop run requires canonical --expect-stop-plan-id and --acknowledge-client-stop")
	}
	prepared, err := prepareClientStopFlags(fs, values, "client stop run", 0)
	if err != nil {
		return err
	}
	operation := clientstop.OperationIDForPlan(*expected)
	ctx, cancel := context.WithTimeout(context.Background(), prepared.timeout)
	defer cancel()
	status, statusErr := clientstop.Status(ctx, clientstop.StatusOptions{TargetRoot: prepared.targetRoot, OperationID: operation, ExpectedPlanID: *expected})
	if statusErr == nil || !errors.Is(statusErr, clientstop.ErrOperationNotFound) && !errors.Is(statusErr, clientstop.ErrInitializationIncomplete) {
		if statusErr == nil {
			statusErr = fmt.Errorf("%w: client stop operation already exists; use resume", clientstop.ErrPolicy)
			status = clientstop.WithFailure(status, statusErr)
		}
		return a.finishClientStop(prepared.output, status, statusErr)
	}
	authority, session, err := a.openClientStop(ctx, prepared, "run", *expected, operation)
	if reported, ok := err.(*clientStopReportedError); ok {
		return a.finishClientStop(reported.output, reported.report, reported.err)
	}
	if err != nil {
		return err
	}
	defer session.Close()
	descriptor, err := session.ReadExistingJobStopDescriptor(ctx)
	if err != nil {
		report := clientstop.FailureReport(*expected, operation, "run", err)
		report.RecordSessionRequests(session.RequestsMade())
		return a.finishClientStop(prepared.output, report, err)
	}
	current, _, err := clientactivate.VerifyCurrentUse(ctx, authority, session)
	if err != nil {
		report := clientstop.FailureReport(*expected, operation, "run", err)
		report.RecordSessionRequests(session.RequestsMade())
		return a.finishClientStop(prepared.output, report, err)
	}
	review, err := clientstop.BuildPlan(current, descriptor)
	if err != nil {
		report := clientstop.FailureReport(*expected, operation, "run", err)
		report.RecordSessionRequests(session.RequestsMade())
		return a.finishClientStop(prepared.output, report, err)
	}
	report, operationErr := clientstop.Run(ctx, clientstop.RunOptions{TargetRoot: prepared.targetRoot, Prepared: review,
		ExpectedPlanID: *expected, Session: session, AcknowledgeStop: true})
	report.RecordSessionRequests(session.RequestsMade())
	return a.finishClientStop(prepared.output, report, operationErr)
}

func (a *app) clientStopResume(args []string) error {
	fs := newFlagSet("client stop resume")
	values := addClientStopFlags(fs)
	expected := fs.String("expect-stop-plan-id", "", "reviewed 24-hex client stop plan ID")
	ack := fs.Bool("acknowledge-client-stop", false, "acknowledge one exact-job stop request")
	repeat := fs.Bool("acknowledge-repeat-stop", false, "acknowledge repeating an inconclusive stop request")
	if err := fs.Parse(args); err != nil {
		return usageError("client stop resume: %v", err)
	}
	if fs.NArg() != 1 || !validMaterializePlanID(*expected) || *repeat && !*ack {
		return usageError("client stop resume requires one operation, canonical plan ID, and repeat acknowledgement paired with stop acknowledgement")
	}
	operation, err := clientstop.ParseOperationID(fs.Arg(0))
	if err != nil || clientstop.OperationIDForPlan(*expected) != operation {
		return usageError("client stop resume operation and plan IDs disagree")
	}
	prepared, err := prepareClientStopFlags(fs, values, "client stop resume", 1)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), prepared.timeout)
	defer cancel()
	preflight, preflightErr := clientstop.Status(ctx, clientstop.StatusOptions{TargetRoot: prepared.targetRoot, OperationID: operation, ExpectedPlanID: *expected})
	initializationIncomplete := errors.Is(preflightErr, clientstop.ErrInitializationIncomplete)
	markerRecoveryRequired := errors.Is(preflightErr, clientstop.ErrMarkerRecoveryRequired)
	if preflightErr != nil && !initializationIncomplete && !markerRecoveryRequired {
		return a.finishClientStop(prepared.output, preflight, preflightErr)
	}
	if initializationIncomplete && !*ack || !initializationIncomplete && !markerRecoveryRequired && (preflight.Operation.Phase == "journaled" || preflight.Operation.Phase == "request_not_sent") && !*ack {
		policyErr := fmt.Errorf("%w: client stop acknowledgement is required", clientstop.ErrPolicy)
		return a.finishClientStop(prepared.output, clientstop.WithFailure(preflight, policyErr), policyErr)
	}
	authority, session, err := a.openClientStop(ctx, prepared, "resume", *expected, operation)
	if reported, ok := err.(*clientStopReportedError); ok {
		return a.finishClientStop(reported.output, reported.report, reported.err)
	}
	if err != nil {
		return err
	}
	defer session.Close()
	if initializationIncomplete {
		descriptor, descriptorErr := session.ReadExistingJobStopDescriptor(ctx)
		if descriptorErr != nil {
			report := clientstop.FailureReport(*expected, operation, "resume_initialization", descriptorErr)
			report.RecordSessionRequests(session.RequestsMade())
			return a.finishClientStop(prepared.output, report, descriptorErr)
		}
		current, _, currentErr := clientactivate.VerifyCurrentUse(ctx, authority, session)
		if currentErr != nil {
			report := clientstop.FailureReport(*expected, operation, "resume_initialization", currentErr)
			report.RecordSessionRequests(session.RequestsMade())
			return a.finishClientStop(prepared.output, report, currentErr)
		}
		review, buildErr := clientstop.BuildPlan(current, descriptor)
		if buildErr != nil {
			report := clientstop.FailureReport(*expected, operation, "resume_initialization", buildErr)
			report.RecordSessionRequests(session.RequestsMade())
			return a.finishClientStop(prepared.output, report, buildErr)
		}
		report, operationErr := clientstop.Run(ctx, clientstop.RunOptions{TargetRoot: prepared.targetRoot, Prepared: review, ExpectedPlanID: *expected, Session: session, AcknowledgeStop: true})
		report.RecordSessionRequests(session.RequestsMade())
		return a.finishClientStop(prepared.output, report, operationErr)
	}
	report, operationErr := clientstop.Resume(ctx, clientstop.ResumeOptions{TargetRoot: prepared.targetRoot, OperationID: operation,
		ExpectedPlanID: *expected, Authority: authority, Session: session, AcknowledgeStop: *ack, AcknowledgeRepeat: *repeat})
	report.RecordSessionRequests(session.RequestsMade())
	return a.finishClientStop(prepared.output, report, operationErr)
}

func (a *app) clientStopStatus(args []string) error {
	fs := newFlagSet("client stop status")
	output := fs.String("output", "table", "table or json")
	target := fs.String("target", "", "materialized target root")
	expected := fs.String("expect-stop-plan-id", "", "reviewed 24-hex client stop plan ID")
	timeout := fs.Duration("timeout", time.Hour, "bounded private journal read timeout")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *target == "" || !validMaterializePlanID(*expected) {
		return usageError("client stop status requires --target, canonical --expect-stop-plan-id, and one OPERATION_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > time.Hour {
		return usageError("client stop status --timeout must be in (0,1h]")
	}
	operation, err := clientstop.ParseOperationID(fs.Arg(0))
	if err != nil || clientstop.OperationIDForPlan(*expected) != operation {
		return usageError("client stop status operation and plan IDs disagree")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, operationErr := clientstop.Status(ctx, clientstop.StatusOptions{TargetRoot: *target, OperationID: operation, ExpectedPlanID: *expected})
	return a.finishClientStop(*output, report, operationErr)
}

func (a *app) finishClientStop(output string, report clientstop.Report, operationErr error) error {
	if output == "json" {
		if err := writeJSON(a.stdout, report, nil); err != nil {
			return err
		}
	} else if output == "table" {
		if err := writeClientStopHuman(a.stdout, report); err != nil {
			return err
		}
	} else {
		return usageError("--output must be table or json")
	}
	if operationErr == nil {
		return nil
	}
	if errors.Is(operationErr, clientstop.ErrIntegrity) || errors.Is(operationErr, clientactivate.ErrIntegrity) || errors.Is(operationErr, materialize.ErrIntegrity) {
		return &integrityErr{message: "client stop failed integrity validation; see report"}
	}
	if errors.Is(operationErr, clientstop.ErrPolicy) || errors.Is(operationErr, clientstop.ErrRequestUnknown) || errors.Is(operationErr, clientstop.ErrOperationNotFound) || errors.Is(operationErr, clientstop.ErrInitializationIncomplete) || errors.Is(operationErr, clientstop.ErrMarkerRecoveryRequired) || errors.Is(operationErr, clientactivate.ErrPolicy) {
		return &inconclusiveErr{message: "client stop is not complete; see report"}
	}
	return fmt.Errorf("client stop was interrupted; see report")
}

func writeClientStopHuman(out io.Writer, report clientstop.Report) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nWRITES UNCERTAIN\t%t\nDOWNLOADER MUTATION REQUESTS\t%d\nDOWNLOADER SESSION REQUESTS\t%d\nOPERATION ID\t%s\nOPERATION STATUS\t%s\nPHASE\t%s\nRESUMABLE\t%t\n",
		terminalSafe(report.Outcome), terminalSafe(report.Effect), report.Writes.WritesPerformed, report.Writes.WritesUncertain,
		report.Writes.DownloaderRequests, report.Client.RequestsMade, terminalSafe(valueOrUnknown(report.Operation.ID)), terminalSafe(report.Operation.Status), terminalSafe(report.Operation.Phase), report.Operation.Resumable)
	fmt.Fprintln(w, "\nBLOCKERS")
	writeClientStopStrings(w, report.Blockers)
	fmt.Fprintf(w, "\nPLAN\nID\t%s\nEXPECTED ID\t%s\nMATCHES\t%t\nACTION\t%s\nDRIVER\t%s\nPROTOCOL\t%s\nSTOP ROUTE\t%s\nJOB ID\t%s\nREVIEWED STATE\t%s\nFILE LAYOUT\t%s\nFILE SNAPSHOT\t%s\n",
		terminalSafe(valueOrUnknown(report.Plan.ID)), terminalSafe(materializeValueOr(report.Plan.ExpectedID, "not_requested")), report.Plan.Matches,
		terminalSafe(materializeValueOr(report.Plan.Data.Action, "not_observed")), terminalSafe(materializeValueOr(report.Plan.Data.Driver, "not_observed")),
		terminalSafe(materializeValueOr(report.Plan.Data.Stop.Protocol, "not_observed")), terminalSafe(materializeValueOr(report.Plan.Data.Stop.StopRouteID, "not_observed")),
		terminalSafe(valueOrUnknown(report.Plan.Data.JobID)), terminalSafe(materializeValueOr(report.Plan.Data.ReviewedJobState, "not_observed")),
		terminalSafe(valueOrUnknown(report.Plan.Data.FileLayoutID)), terminalSafe(valueOrUnknown(report.Plan.Data.CompleteFileSnapshotID)))
	fmt.Fprintf(w, "\nCURRENT USE BEFORE REQUEST\nJOB STATE\t%s\nJOB PROGRESS\t%.6f\nALL FILES SELECTED\t%t\nALL FILES COMPLETE\t%t\nPROOF REQUESTS\t%d\n",
		terminalSafe(materializeValueOr(report.Before.JobState, "not_observed")), report.Before.JobProgress, report.Before.AllSelected, report.Before.AllComplete, report.Before.RequestsMade)
	fmt.Fprintf(w, "\nMUTATION RECEIPT\nSTATUS\t%s\nCOMPLETE\t%t\nREQUESTS ATTEMPTED\t%d\nAUTOMATIC RETRIES\t%d\nREDIRECTS\t%d\nREQUEST BYTES\t%d\nREQUEST BYTES KNOWN\t%t\nSTOP REASON\t%s\n",
		terminalSafe(report.Mutation.Status), report.Mutation.Receipt.Complete, report.Mutation.Receipt.RequestsAttempted, report.Mutation.Receipt.AutomaticRetries,
		report.Mutation.Receipt.RedirectsFollowed, report.Mutation.Receipt.RequestBytes, report.Mutation.Receipt.RequestBytesKnown,
		terminalSafe(materializeValueOr(report.Mutation.Receipt.StopReason, "none")))
	fmt.Fprintf(w, "\nPOST-REQUEST EVIDENCE\nSTOPPED STATUS\t%s\nJOB STATE\t%s\nPROOF REQUESTS\t%d\nFINAL VARIANT\t%s\nFINAL IDENTITY\t%s\nBYTES VERIFIED\t%d\nCOMPLETION BASIS\t%s\nQUEUE EVIDENCE\t%s\nFILESYSTEM EVIDENCE\t%s\nATOMICITY\t%s\n",
		terminalSafe(report.Stopped.Status), terminalSafe(materializeValueOr(report.Stopped.Observation.JobState, "not_observed")), report.Stopped.Observation.RequestsMade,
		terminalSafe(valueOrUnknown(report.Final.MetafileVariantID)), terminalSafe(valueOrUnknown(report.Final.FinalObjectIdentity)), report.Final.BytesVerified,
		terminalSafe(report.Assurance.CompletionBasis), terminalSafe(report.Assurance.QueueEvidence), terminalSafe(report.Assurance.FilesystemEvidence), terminalSafe(report.Assurance.Atomicity))
	fmt.Fprintln(w, "\nWARNINGS")
	writeClientStopStrings(w, report.Warnings)
	return w.Flush()
}

func writeClientStopStrings(out io.Writer, values []string) {
	if len(values) == 0 {
		fmt.Fprintln(out, "-\tnone")
		return
	}
	for _, value := range values {
		fmt.Fprintf(out, "-\t%s\n", terminalSafe(value))
	}
}
