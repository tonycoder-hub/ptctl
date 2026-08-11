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

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/downloader/qbittorrent"
	"github.com/tonycoder-hub/ptctl/internal/downloader/transmission"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const (
	clientActivateDefaultTimeout = 24 * time.Hour
	clientActivateMaximumTimeout = 7 * 24 * time.Hour
)

type clientActivateFlags struct {
	output               *string
	storeRoot            *string
	variantID            *string
	targetRoot           *string
	materializeOperation *string
	materializePlanID    *string
	adoptionOperation    *string
	adoptionPlanID       *string
	hostRoot             *string
	clientRoot           *string
	clientStyle          *string
	driver               *string
	endpoint             *string
	username             *string
	passwordStdin        *bool
	timeout              *time.Duration
	startAfterRecheck    *bool
	expectedPlanID       *string
	acknowledgeRecheck   *bool
	repeatRecheck        *bool
	acknowledgeStart     *bool
	repeatStart          *bool
}

type preparedClientActivate struct {
	output     string
	timeout    time.Duration
	targetRoot string
	authority  *clientactivate.PreparedAuthority
	adapter    downloader.ExistingJobControlDriver
	username   string
}

func (a *app) clientActivate(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		a.clientActivateHelp()
		return nil
	}
	switch args[0] {
	case "plan":
		return a.clientActivatePlan(args[1:])
	case "run":
		return a.clientActivateRun(args[1:])
	case "resume":
		return a.clientActivateResume(args[1:])
	case "status":
		return a.clientActivateStatus(args[1:])
	case "prune":
		return a.clientActivatePrune(args[1:])
	case "forget":
		return a.clientActivateForget(args[1:])
	default:
		return usageError("unknown client activate subcommand %q", args[0])
	}
}

func (a *app) clientActivateHelp() {
	fmt.Fprint(a.stdout, `Usage:
  ptctl client activate plan --metafile-store DIR --metafile-variant ID --target PATH --materialize-operation ID --materialize-plan-id ID --adoption-operation ID --adoption-plan-id ID --host-root PATH --client-root PATH --client-style posix|windows --driver qbittorrent|transmission --url URL --username USER --password-stdin [--start-after-recheck] [--output table|json]
  ptctl client activate run [same selectors] --expect-activation-plan-id ID --acknowledge-client-recheck [--output table|json]
  ptctl client activate resume [same selectors] --expect-activation-plan-id ID [--acknowledge-client-recheck --acknowledge-repeat-recheck] [--acknowledge-client-start --acknowledge-repeat-start] [--output table|json] OPERATION_ID
  ptctl client activate status --target PATH [--output table|json] OPERATION_ID
  ptctl client activate prune --target PATH --expect-activation-plan-id ID --acknowledge-operation-state-deletion [--output table|json] OPERATION_ID
  ptctl client activate forget --target PATH --expect-activation-plan-id ID --acknowledge-historical-evidence-deletion [--output table|json] OPERATION_ID

This workflow only targets one exact typed-infohash job established by a
canonical stopped-adoption journal and a current exact materialized-final
proof. Run durably records request intent before one downloader recheck POST.
Transmission activation is v1-only and uses its exact hash-string selector.

HTTP success is not recheck completion. Completion requires either a durable
checking observation followed by complete stopped state, or a bracketed
incomplete-to-complete transition. Unknown requests are never replayed without
explicit repeat acknowledgement. --start-after-recheck reviews an optional
start action; a later resume still requires --acknowledge-client-start and can
announce to trackers or transfer data. Each invocation sends at most one
effectful client POST. No source data is retired or deleted.

Prune is local-only. It seals one terminal activation journal into an exact
owner-private tombstone before deleting only that operation's original markers
and empty scratch. The tombstone remains usable by source-retirement proof.
Forget is a final, separately acknowledged local-only boundary. It publishes a
root-level recovery intent, removes one exact retained activation tombstone,
then removes that last intent. A repeated call can report only unattributed
absence; it cannot claim idempotent historical success.
`)
}

func addClientActivateFlags(fs *flag.FlagSet, execution bool) *clientActivateFlags {
	values := &clientActivateFlags{}
	values.output = fs.String("output", "table", "table or json")
	values.storeRoot = fs.String("metafile-store", "", "initialized private metafile store")
	values.variantID = fs.String("metafile-variant", "", "whole-metafile sha256 artifact ID")
	values.targetRoot = fs.String("target", "", "materialized target root")
	values.materializeOperation = fs.String("materialize-operation", "", "committed materialize operation ID")
	values.materializePlanID = fs.String("materialize-plan-id", "", "reviewed materialize plan ID")
	values.adoptionOperation = fs.String("adoption-operation", "", "completed stopped-adoption operation ID")
	values.adoptionPlanID = fs.String("adoption-plan-id", "", "reviewed stopped-adoption plan ID")
	values.hostRoot = fs.String("host-root", "", "host namespace root containing the target")
	values.clientRoot = fs.String("client-root", "", "downloader-visible namespace root")
	values.clientStyle = fs.String("client-style", "posix", "downloader path style: posix or windows")
	values.driver = fs.String("driver", "qbittorrent", "downloader driver: qbittorrent or transmission")
	values.endpoint = fs.String("url", "", "downloader API origin or Transmission RPC URL")
	values.username = fs.String("username", "", "downloader username")
	values.passwordStdin = fs.Bool("password-stdin", false, "read downloader password from stdin")
	values.timeout = fs.Duration("timeout", clientActivateDefaultTimeout, "shared proof, journal, and client wall-clock budget")
	values.startAfterRecheck = fs.Bool("start-after-recheck", false, "review an optional start after durable recheck completion")
	if execution {
		values.expectedPlanID = fs.String("expect-activation-plan-id", "", "reviewed 24-hex client activation plan ID")
		values.acknowledgeRecheck = fs.Bool("acknowledge-client-recheck", false, "acknowledge one downloader recheck request")
		values.repeatRecheck = fs.Bool("acknowledge-repeat-recheck", false, "acknowledge repeating an inconclusive recheck request")
		values.acknowledgeStart = fs.Bool("acknowledge-client-start", false, "acknowledge one downloader start request")
		values.repeatStart = fs.Bool("acknowledge-repeat-start", false, "acknowledge repeating an inconclusive start request")
	}
	return values
}

func prepareClientActivate(ctx context.Context, fs *flag.FlagSet, values *clientActivateFlags, command string, allowPositional bool) (preparedClientActivate, error) {
	var result preparedClientActivate
	if err := validateOutput(*values.output); err != nil {
		return result, err
	}
	if *values.timeout <= 0 || *values.timeout > clientActivateMaximumTimeout {
		return result, usageError("--timeout must be greater than zero and no more than 168h")
	}
	if (!allowPositional && fs.NArg() != 0) || (allowPositional && fs.NArg() != 1) {
		return result, usageError("%s has an invalid positional argument count", command)
	}
	if *values.storeRoot == "" || *values.variantID == "" || *values.targetRoot == "" || *values.materializeOperation == "" ||
		*values.materializePlanID == "" || *values.adoptionOperation == "" || *values.adoptionPlanID == "" ||
		*values.hostRoot == "" || *values.clientRoot == "" || *values.endpoint == "" || *values.username == "" {
		return result, usageError("%s requires every metafile, materialize, adoption, mapping, and downloader selector", command)
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
	adoptionOperation, err := clientadopt.ParseOperationID(*values.adoptionOperation)
	if err != nil || !validMaterializePlanID(*values.adoptionPlanID) {
		return result, usageError("%s requires canonical adoption operation and plan IDs", command)
	}
	windows := *values.clientStyle == "windows"
	if err := storage.ValidatePathMappingConfig(*values.hostRoot, *values.clientRoot, windows); err != nil {
		return result, usageError("%s path mapping is invalid: %v", command, err)
	}
	adapter, err := newExistingJobControlDriver(*values.driver, *values.endpoint)
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
	verifiedFinal, _, err := materialize.VerifyCurrentFinal(ctx, materialize.FinalProofOptions{Meta: meta, TargetRoot: *values.targetRoot,
		OperationID: materializeOperation, ExpectedPlanID: *values.materializePlanID, Limits: materialize.DefaultLimits()})
	if errors.Is(err, materialize.ErrIntegrity) {
		return result, &integrityErr{message: "materialized final failed current exact verification"}
	}
	if err != nil {
		return result, err
	}
	verifiedAdoption, _, err := clientadopt.VerifyCompletion(ctx, clientadopt.CompletionProofOptions{TargetRoot: *values.targetRoot,
		OperationID: adoptionOperation, ExpectedPlanID: *values.adoptionPlanID})
	if errors.Is(err, clientadopt.ErrIntegrity) {
		return result, &integrityErr{message: "stopped-adoption journal failed integrity validation"}
	}
	if err != nil {
		return result, err
	}
	authority, err := clientactivate.PrepareAuthority(verifiedFinal, verifiedAdoption, clientactivate.AuthorityOptions{
		Driver: *values.driver, ClientConfigID: clientConfigID, HostRoot: *values.hostRoot, ClientRoot: *values.clientRoot,
		ClientWindows: windows, FileLimits: downloader.DefaultJobFileLedgerLimits(),
	})
	if err != nil {
		return result, err
	}
	return preparedClientActivate{output: *values.output, timeout: *values.timeout, targetRoot: *values.targetRoot,
		authority: authority, adapter: adapter, username: *values.username}, nil
}

func newExistingJobControlDriver(name, endpoint string) (downloader.ExistingJobControlDriver, error) {
	switch name {
	case downloader.DriverQBittorrent:
		return qbittorrent.New(endpoint)
	case downloader.DriverTransmission:
		return transmission.New(endpoint)
	default:
		return nil, fmt.Errorf("unsupported existing-job control driver %q", name)
	}
}

func (a *app) clientActivatePlan(args []string) error {
	fs := newFlagSet("client activate plan")
	values := addClientActivateFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return usageError("client activate plan: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *values.timeout)
	defer cancel()
	prepared, err := prepareClientActivate(ctx, fs, values, "client activate plan", false)
	if err != nil {
		return err
	}
	credential, err := readDownloaderCredential(a.stdin, prepared.username)
	if err != nil {
		return err
	}
	session, err := prepared.adapter.OpenExistingJobMutationSession(ctx, credential)
	if err != nil {
		requests, _ := downloader.RequestsMadeFromError(err)
		report := clientactivate.FailureReport(prepared.authority, "", "plan", requests, err)
		return a.finishClientActivate(prepared.output, report, err)
	}
	defer session.Close()
	report, operationErr := clientactivate.Preview(ctx, prepared.authority, session, *values.startAfterRecheck)
	return a.finishClientActivate(prepared.output, report, operationErr)
}

func (a *app) clientActivateRun(args []string) error {
	fs := newFlagSet("client activate run")
	values := addClientActivateFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		return usageError("client activate run: %v", err)
	}
	if !validMaterializePlanID(*values.expectedPlanID) || !*values.acknowledgeRecheck || *values.repeatRecheck || *values.repeatStart || *values.acknowledgeStart {
		return usageError("client activate run requires a reviewed plan and recheck acknowledgement; start and repeat acknowledgements are resume-only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *values.timeout)
	defer cancel()
	prepared, err := prepareClientActivate(ctx, fs, values, "client activate run", false)
	if err != nil {
		return err
	}
	if err := clientactivate.PreflightRun(ctx, prepared.authority, *values.expectedPlanID); err != nil {
		report := clientactivate.FailureReport(prepared.authority, *values.expectedPlanID, "run", 0, err)
		return a.finishClientActivate(prepared.output, report, err)
	}
	credential, err := readDownloaderCredential(a.stdin, prepared.username)
	if err != nil {
		return err
	}
	session, err := prepared.adapter.OpenExistingJobMutationSession(ctx, credential)
	if err != nil {
		requests, _ := downloader.RequestsMadeFromError(err)
		report := clientactivate.FailureReport(prepared.authority, *values.expectedPlanID, "run", requests, err)
		return a.finishClientActivate(prepared.output, report, err)
	}
	defer session.Close()
	report, operationErr := clientactivate.Run(ctx, clientactivate.RunOptions{Authority: prepared.authority, ExpectedPlanID: *values.expectedPlanID,
		Session: session, StartAfterRecheck: *values.startAfterRecheck, AcknowledgeRecheck: true,
		AcknowledgeStart: *values.acknowledgeStart})
	return a.finishClientActivate(prepared.output, report, operationErr)
}

func (a *app) clientActivateResume(args []string) error {
	fs := newFlagSet("client activate resume")
	values := addClientActivateFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		return usageError("client activate resume: %v", err)
	}
	if fs.NArg() != 1 || !validMaterializePlanID(*values.expectedPlanID) {
		return usageError("client activate resume requires one OPERATION_ID and canonical --expect-activation-plan-id")
	}
	if *values.repeatRecheck && !*values.acknowledgeRecheck || *values.repeatStart && !*values.acknowledgeStart ||
		((*values.acknowledgeStart || *values.repeatStart) && !*values.startAfterRecheck) {
		return usageError("repeat acknowledgements require their base acknowledgement and start flags require --start-after-recheck")
	}
	operationID, err := clientactivate.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("client activate resume requires a canonical OPERATION_ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *values.timeout)
	defer cancel()
	prepared, err := prepareClientActivate(ctx, fs, values, "client activate resume", true)
	if err != nil {
		return err
	}
	preflight, preflightErr := clientactivate.PreflightResume(ctx, prepared.authority, prepared.targetRoot, operationID, *values.expectedPlanID)
	initializationIncomplete := errors.Is(preflightErr, clientactivate.ErrInitializationIncomplete)
	wantAction := clientactivate.ActionRecheckOnly
	if *values.startAfterRecheck {
		wantAction = clientactivate.ActionRecheckThenStart
	}
	if !initializationIncomplete && preflight.Plan.Action != "" && preflight.Plan.Action != wantAction {
		preflightErr = fmt.Errorf("%w: --start-after-recheck differs from the reviewed activation journal", clientactivate.ErrPolicy)
		report := clientactivate.FailureReport(prepared.authority, *values.expectedPlanID, "resume", 0, preflightErr)
		return a.finishClientActivate(prepared.output, report, preflightErr)
	}
	if preflight.Operation.Status == "retained" {
		return a.finishClientActivate(prepared.output, preflight, nil)
	}
	if preflight.Operation.Status == "pruning" || preflight.Operation.Status == "retention_initializing" {
		return a.finishClientActivate(prepared.output, preflight, preflightErr)
	}
	if preflightErr != nil && !initializationIncomplete {
		return a.finishClientActivate(prepared.output, preflight, preflightErr)
	}
	if (initializationIncomplete || preflight.Journal.RecheckAttempts == 0) && !*values.acknowledgeRecheck {
		preflightErr = fmt.Errorf("%w: operation has no recheck request intent; explicit acknowledgement is required", clientactivate.ErrPolicy)
		report := clientactivate.FailureReport(prepared.authority, *values.expectedPlanID, "resume", 0, preflightErr)
		return a.finishClientActivate(prepared.output, report, preflightErr)
	}
	credential, err := readDownloaderCredential(a.stdin, prepared.username)
	if err != nil {
		return err
	}
	session, err := prepared.adapter.OpenExistingJobMutationSession(ctx, credential)
	if err != nil {
		requests, _ := downloader.RequestsMadeFromError(err)
		report := clientactivate.FailureReport(prepared.authority, *values.expectedPlanID, "resume", requests, err)
		return a.finishClientActivate(prepared.output, report, err)
	}
	defer session.Close()
	report, operationErr := clientactivate.Resume(ctx, operationID, clientactivate.RunOptions{Authority: prepared.authority,
		ExpectedPlanID: *values.expectedPlanID, Session: session, StartAfterRecheck: *values.startAfterRecheck,
		AcknowledgeRecheck: *values.acknowledgeRecheck, RepeatRecheck: *values.repeatRecheck,
		AcknowledgeStart: *values.acknowledgeStart, RepeatStart: *values.repeatStart})
	return a.finishClientActivate(prepared.output, report, operationErr)
}

func (a *app) clientActivatePrune(args []string) error {
	fs := newFlagSet("client activate prune")
	output := fs.String("output", "table", "table or json")
	targetRoot := fs.String("target", "", "materialized target root")
	expectedPlanID := fs.String("expect-activation-plan-id", "", "reviewed 24-hex client activation plan ID")
	acknowledge := fs.Bool("acknowledge-operation-state-deletion", false, "acknowledge deletion of one terminal activation journal")
	timeout := fs.Duration("timeout", time.Hour, "bounded private retention transition timeout")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *targetRoot == "" {
		return usageError("client activate prune requires --target, --expect-activation-plan-id, acknowledgement, and one OPERATION_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if !validMaterializePlanID(*expectedPlanID) || !*acknowledge {
		return usageError("client activate prune requires canonical --expect-activation-plan-id and --acknowledge-operation-state-deletion")
	}
	if *timeout <= 0 || *timeout > time.Hour {
		return usageError("client activate prune --timeout must be in (0,1h]")
	}
	operationID, err := clientactivate.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("client activate prune requires a canonical OPERATION_ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, operationErr := clientactivate.Prune(ctx, clientactivate.PruneOptions{TargetRoot: *targetRoot, OperationID: operationID,
		ExpectedPlanID: *expectedPlanID, Acknowledge: true, Limits: clientactivate.DefaultRetentionLimits()})
	return a.finishClientActivateRetention(*output, report, operationErr)
}

func (a *app) clientActivateForget(args []string) error {
	fs := newFlagSet("client activate forget")
	output := fs.String("output", "table", "table or json")
	targetRoot := fs.String("target", "", "materialized target root")
	expectedPlanID := fs.String("expect-activation-plan-id", "", "reviewed 24-hex client activation plan ID")
	acknowledge := fs.Bool("acknowledge-historical-evidence-deletion", false, "acknowledge irreversible deletion of the retained activation tombstone and final historical attribution")
	timeout := fs.Duration("timeout", time.Minute, "historical-evidence deletion wall-clock budget")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *targetRoot == "" {
		return usageError("client activate forget requires --target, --expect-activation-plan-id, acknowledgement, and one OPERATION_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if !validMaterializePlanID(*expectedPlanID) || !*acknowledge {
		return usageError("client activate forget requires canonical --expect-activation-plan-id and --acknowledge-historical-evidence-deletion")
	}
	if *timeout <= 0 || *timeout > time.Hour {
		return usageError("client activate forget --timeout must be in (0,1h]")
	}
	operationID, err := clientactivate.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("client activate forget requires a canonical OPERATION_ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, operationErr := clientactivate.Forget(ctx, clientactivate.ForgetOptions{TargetRoot: *targetRoot, OperationID: operationID,
		ExpectedPlanID: *expectedPlanID, Acknowledge: true, Limits: clientactivate.DefaultForgetLimits()})
	return a.finishClientActivateForget(*output, report, operationErr)
}

func (a *app) clientActivateStatus(args []string) error {
	fs := newFlagSet("client activate status")
	output := fs.String("output", "table", "table or json")
	targetRoot := fs.String("target", "", "materialized target root")
	timeout := fs.Duration("timeout", time.Hour, "bounded private journal read timeout")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *targetRoot == "" {
		return usageError("client activate status requires --target and one OPERATION_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > time.Hour {
		return usageError("client activate status --timeout must be in (0,1h]")
	}
	operationID, err := clientactivate.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("client activate status requires a canonical OPERATION_ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, operationErr := clientactivate.Status(ctx, clientactivate.StatusOptions{TargetRoot: *targetRoot, OperationID: operationID})
	return a.finishClientActivate(*output, report, operationErr)
}

func (a *app) finishClientActivate(output string, report clientactivate.Report, operationErr error) error {
	if err := a.writeClientActivateReport(output, report); err != nil {
		return err
	}
	if operationErr == nil {
		return nil
	}
	if errors.Is(operationErr, clientactivate.ErrIntegrity) || errors.Is(operationErr, materialize.ErrIntegrity) || errors.Is(operationErr, clientadopt.ErrIntegrity) {
		return &integrityErr{message: "client activation failed integrity validation; see report"}
	}
	if errors.Is(operationErr, clientactivate.ErrPolicy) || errors.Is(operationErr, clientactivate.ErrRequestUnknown) ||
		errors.Is(operationErr, clientactivate.ErrOperationNotFound) || errors.Is(operationErr, clientactivate.ErrInitializationIncomplete) {
		return &inconclusiveErr{message: "client activation is not complete; see report"}
	}
	return fmt.Errorf("client activation was interrupted; see report")
}

func (a *app) finishClientActivateRetention(output string, report clientactivate.RetentionReport, operationErr error) error {
	if output == "json" {
		if err := writeJSON(a.stdout, report, nil); err != nil {
			return err
		}
	} else if err := writeClientActivateRetentionHuman(a.stdout, report); err != nil {
		return err
	}
	if operationErr == nil {
		return nil
	}
	if report.Outcome == clientactivate.RetentionOutcomeIntegrityFailed {
		return &integrityErr{message: "client activation retention failed integrity validation; see report"}
	}
	if report.Outcome == clientactivate.RetentionOutcomeBlocked {
		return &inconclusiveErr{message: "client activation retention is blocked; see report"}
	}
	return fmt.Errorf("client activation retention was interrupted; see report")
}

func (a *app) finishClientActivateForget(output string, report clientactivate.ForgetReport, operationErr error) error {
	if output == "json" {
		if err := writeJSON(a.stdout, report, nil); err != nil {
			return err
		}
	} else if err := writeClientActivateForgetHuman(a.stdout, report); err != nil {
		return err
	}
	if operationErr == nil && report.Outcome == clientactivate.ForgetOutcomeForgotten {
		return nil
	}
	if report.Outcome == clientactivate.ForgetOutcomeIntegrityFailed {
		return &integrityErr{message: "client activation historical evidence failed integrity validation; see report"}
	}
	if report.Outcome == clientactivate.ForgetOutcomeBlocked {
		return &inconclusiveErr{message: "client activation historical-evidence deletion is blocked; see report"}
	}
	if report.Outcome == clientactivate.ForgetOutcomeAbsentUnattributed {
		return fmt.Errorf("client activation historical evidence is absent without a remaining attribution marker; see report")
	}
	return fmt.Errorf("client activation historical-evidence deletion was interrupted or its durability is unconfirmed; see report")
}

func (a *app) writeClientActivateReport(output string, report clientactivate.Report) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	return writeClientActivateHuman(a.stdout, report)
}

func writeClientActivateHuman(out io.Writer, report clientactivate.Report) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nWRITES UNCERTAIN\t%t\nOPERATION ID\t%s\nOPERATION STATUS\t%s\nPHASE BEFORE\t%s\nPHASE AFTER\t%s\nRESUMABLE\t%t\n",
		terminalSafe(string(report.Outcome)), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed, report.WritesUncertain,
		terminalSafe(valueOrUnknown(report.Operation.ID)), terminalSafe(report.Operation.Status), terminalSafe(report.Operation.PhaseBefore),
		terminalSafe(report.Operation.PhaseAfter), report.Operation.Resumable)
	fmt.Fprintln(w, "\nBLOCKERS")
	writeClientActivateFindings(w, report.Blockers)
	fmt.Fprintf(w, "\nPLAN\nID\t%s\nEXPECTED ID\t%s\nMATCHES\t%t\nACTION\t%s\nDRIVER\t%s\nPROTOCOL\t%s\nRECHECK ROUTE\t%s\nSTART ROUTE\t%s\nCLIENT CONFIG\t%s\nPATH MAPPING\t%s\nPATH SEMANTICS\t%s\nJOB ID\t%s\nFILE LAYOUT\t%s\n",
		terminalSafe(valueOrUnknown(report.Plan.ID)), terminalSafe(materializeValueOr(report.Plan.ExpectedID, "not_requested")), report.Plan.Matches,
		terminalSafe(materializeValueOr(report.Plan.Action, "not_observed")), terminalSafe(materializeValueOr(report.Plan.Driver, "not_observed")),
		terminalSafe(materializeValueOr(report.Plan.Control.Protocol, "not_observed")),
		terminalSafe(materializeValueOr(report.Plan.Control.RecheckRouteID, "not_observed")), terminalSafe(materializeValueOr(report.Plan.Control.StartRouteID, "not_observed")),
		terminalSafe(materializeValueOr(report.Plan.ClientConfigID, "not_observed")), terminalSafe(materializeValueOr(report.Plan.PathMappingID, "not_observed")),
		terminalSafe(materializeValueOr(report.Plan.ClientPathSemantics, "not_observed")), terminalSafe(materializeValueOr(report.Plan.JobID, "not_observed")),
		terminalSafe(materializeValueOr(report.Plan.ExpectedFileLayoutID, "not_observed")))
	fmt.Fprintf(w, "\nMATERIALIZED FINAL / ADOPTION\nFINAL STATUS\t%s\nVARIANT\t%s\nFINAL IDENTITY\t%s\nBYTES VERIFIED\t%d\nFINAL ASSURANCE\t%s\nADOPTION STATUS\t%s\nADOPTION OPERATION\t%s\nADOPTION COMPLETION\t%s\nADOPTION ASSURANCE\t%s\n",
		terminalSafe(report.Final.Status), terminalSafe(report.Final.Observation.MetafileVariantID), terminalSafe(report.Final.Observation.FinalObjectIdentity),
		report.Final.Observation.BytesVerified, terminalSafe(report.Final.Observation.Assurance), terminalSafe(report.Adoption.Status),
		terminalSafe(report.Adoption.Observation.OperationID), terminalSafe(report.Adoption.Observation.CompletionID), terminalSafe(report.Adoption.Observation.Assurance))
	fmt.Fprintf(w, "\nCLIENT\nSTATUS\t%s\nREQUESTS MADE\t%d\nLEDGER READS\t%d\nFILE LEDGER READS\t%d\nIDENTITY\t%s\nJOB ID\t%s\nJOB STATE\t%s\nJOB PROGRESS\t%.6f\nFILE LAYOUT\t%s\nALL FILES SELECTED\t%t\nALL FILES COMPLETE\t%t\nACTION ATTEMPTED\t%s\nACTION COMPLETE\t%t\nAUTOMATIC RETRIES\t%d\nREDIRECTS\t%d\nASSURANCE\t%s\n",
		terminalSafe(report.Client.Status), report.Client.RequestsMade, report.Client.LedgerReads, report.Client.FileLedgerReads,
		terminalSafe(report.Client.IdentityStatus), terminalSafe(materializeValueOr(report.Client.JobID, "not_observed")),
		terminalSafe(materializeValueOr(report.Client.JobState, "not_observed")), report.Client.JobProgress,
		terminalSafe(materializeValueOr(report.Client.FileLayoutID, "not_observed")), report.Client.AllFilesSelected, report.Client.AllFilesComplete,
		terminalSafe(materializeValueOr(report.Client.ActionAttempted, "not_attempted")), report.Client.ActionReceipt.Complete,
		report.Client.ActionReceipt.AutomaticRetries, report.Client.ActionReceipt.RedirectsFollowed, terminalSafe(report.Client.Assurance))
	fmt.Fprintf(w, "\nJOURNAL / WRITES\nSTATUS\t%s\nINTENT DURABLE\t%t\nRECHECK ATTEMPTS\t%d\nRECHECK STARTED DURABLE\t%t\nRECHECK COMPLETION DURABLE\t%t\nSTART ATTEMPTS\t%d\nACTIVATION COMPLETION DURABLE\t%t\nPENDING RECOVERY MARKER\t%t\nRETENTION STATE\t%s\nRETENTION INTENT PRESENT\t%t\nRETENTION COMPLETE PRESENT\t%t\nRETENTION INTENT DURABLE\t%t\nRETENTION COMPLETE DURABLE\t%t\nOPERATION DIRECTORIES\t%d\nCONTROL DIRECTORIES\t%d\nTEMPORARY FILES\t%d\nTEMPORARY BYTES\t%d\nMARKER PUBLICATIONS\t%d\nTEMPORARY REMOVALS\t%d\nAMBIGUOUS PUBLICATIONS\t%d\n",
		terminalSafe(report.Journal.Status), report.Journal.IntentDurable, report.Journal.RecheckAttempts, report.Journal.RecheckStartedDurable,
		report.Journal.RecheckCompletionDurable, report.Journal.StartAttempts, report.Journal.ActivationCompletionDurable,
		report.Journal.PendingRecoveryMarker, terminalSafe(report.Journal.RetentionState), report.Journal.RetentionIntentPresent,
		report.Journal.RetentionCompletionPresent, report.Journal.RetentionIntentDurable, report.Journal.RetentionCompletionDurable,
		report.Writes.OperationDirectories, report.Writes.ControlDirectories,
		report.Writes.TemporaryFiles, report.Writes.TemporaryBytes, report.Writes.MarkerPublications,
		report.Writes.TemporaryRemovals, report.Writes.AmbiguousPublications)
	fmt.Fprintln(w, "\nISSUES")
	writeClientActivateFindings(w, report.Issues)
	fmt.Fprintln(w, "\nWARNINGS")
	if len(report.Warnings) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
	}
	return w.Flush()
}

func writeClientActivateRetentionHuman(out io.Writer, report clientactivate.RetentionReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nWRITES UNCERTAIN\t%t\nOPERATION ID\t%s\nOPERATION STATUS\t%s\nPHASE BEFORE\t%s\nPHASE AFTER\t%s\nRESUMABLE\t%t\n",
		terminalSafe(string(report.Outcome)), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed, report.WritesUncertain,
		terminalSafe(report.Operation.ID), terminalSafe(report.Operation.Status), terminalSafe(report.Operation.PhaseBefore), terminalSafe(report.Operation.PhaseAfter), report.Operation.Resumable)
	fmt.Fprintln(w, "\nBLOCKERS")
	writeClientActivateFindings(w, report.Blockers)
	fmt.Fprintf(w, "\nPLAN / TARGET\nPLAN ID\t%s\nEXPECTED ID\t%s\nMATCHES\t%t\nACTION\t%s\nEXPECTED ROOT IDENTITY\t%s\nOBSERVED ROOT IDENTITY\t%s\nROOT BOUND\t%t\nSTABILITY\t%s\n",
		terminalSafe(valueOrUnknown(report.Plan.ID)), terminalSafe(valueOrUnknown(report.Plan.ExpectedID)), report.Plan.Matches,
		terminalSafe(report.Plan.Action), terminalSafe(valueOrUnknown(report.Target.ExpectedRootIdentity)), terminalSafe(valueOrUnknown(report.Target.ObservedRootIdentity)),
		report.Target.RootIdentityBound, terminalSafe(report.Target.StabilityAssurance))
	fmt.Fprintf(w, "\nHISTORICAL PROOF\nBASIS\t%s\nACTIVATION INTENT\t%s\nTERMINAL MARKER\t%s\nMARKERS RECORDED\t%d\nAUTHORITY\t%t\nASSURANCE\t%s\n",
		terminalSafe(report.Proof.Basis), terminalSafe(valueOrUnknown(report.Proof.IntentID)), terminalSafe(valueOrUnknown(report.Proof.TerminalMarkerID)),
		report.Proof.MarkersRecorded, report.Proof.HistoricalAuthority, terminalSafe(report.Proof.Assurance))
	fmt.Fprintf(w, "\nRETENTION / WRITES\nSTATE\t%s\nINTENT MARKER\t%s\nCOMPLETE MARKER\t%s\nINTENT DURABLE\t%t\nCOMPLETION DURABLE\t%t\nEXACT TOMBSTONE\t%t\nPRUNE RESUMABLE\t%t\nCONTROL DIRECTORIES\t%d\nTEMPORARY FILES\t%d\nTEMPORARY BYTES\t%d\nMARKER PUBLICATIONS\t%d\nFILES REMOVED\t%d\nDIRECTORIES REMOVED\t%d\nBYTES REMOVED\t%d\n",
		terminalSafe(report.Markers.State), terminalSafe(valueOrUnknown(report.Markers.IntentMarkerID)), terminalSafe(valueOrUnknown(report.Markers.CompleteMarkerID)),
		report.Markers.IntentDurable, report.Markers.CompletionDurable, report.Markers.ExactTombstone, report.Markers.PruneResumable,
		report.Writes.ControlDirectoriesCreated, report.Writes.MarkerTemporaryFiles, report.Writes.MarkerTemporaryBytes,
		report.Writes.MarkerPublications, report.Writes.FilesRemoved, report.Writes.DirectoriesRemoved, report.Writes.BytesRemoved)
	fmt.Fprintln(w, "\nISSUES")
	writeClientActivateFindings(w, report.Issues)
	fmt.Fprintln(w, "\nWARNINGS")
	if len(report.Warnings) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
	}
	return w.Flush()
}

func writeClientActivateForgetHuman(out io.Writer, report clientactivate.ForgetReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nWRITES UNCERTAIN\t%t\nOPERATION ID\t%s\nOPERATION STATUS\t%s\nPHASE BEFORE\t%s\nPHASE AFTER\t%s\nRESUMABLE\t%t\n",
		terminalSafe(string(report.Outcome)), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed, report.WritesUncertain,
		terminalSafe(report.Operation.ID), terminalSafe(report.Operation.Status), terminalSafe(report.Operation.PhaseBefore), terminalSafe(report.Operation.PhaseAfter), report.Operation.Resumable)
	fmt.Fprintln(w, "\nBLOCKERS")
	writeClientActivateFindings(w, report.Blockers)
	fmt.Fprintf(w, "\nPLAN / TARGET\nPLAN ID\t%s\nEXPECTED ID\t%s\nMATCHES\t%t\nACTION\t%s\nEXPECTED ROOT IDENTITY\t%s\nOBSERVED ROOT IDENTITY\t%s\nROOT BOUND\t%t\nSTABILITY\t%s\n",
		terminalSafe(valueOrUnknown(report.Plan.ID)), terminalSafe(valueOrUnknown(report.Plan.ExpectedID)), report.Plan.Matches, terminalSafe(report.Plan.Action),
		terminalSafe(valueOrUnknown(report.Target.ExpectedRootIdentity)), terminalSafe(valueOrUnknown(report.Target.ObservedRootIdentity)),
		report.Target.RootIdentityBound, terminalSafe(report.Target.StabilityAssurance))
	fmt.Fprintf(w, "\nAUTHORITY\nSTATE\t%s\nFORGET MARKER\t%s\nMARKER DURABLE\t%t\nRETENTION INTENT\t%s\nRETENTION COMPLETE\t%s\nEXACT TOMBSTONE EVIDENCE AVAILABLE\t%t\nTARGET HISTORICAL EVIDENCE ERASED\t%t\n",
		terminalSafe(report.Authority.State), terminalSafe(valueOrUnknown(report.Authority.MarkerID)), report.Authority.MarkerDurable,
		terminalSafe(valueOrUnknown(report.Authority.RetentionIntentMarkerID)), terminalSafe(valueOrUnknown(report.Authority.RetentionCompleteMarkerID)),
		report.Authority.ExactTombstoneEvidenceAvailable, report.Authority.TargetHistoricalEvidenceErased)
	fmt.Fprintf(w, "\nHISTORICAL PROOF\nBASIS\t%s\nACTIVATION INTENT\t%s\nTERMINAL MARKER\t%s\nMARKERS RECORDED\t%d\nAUTHORITY\t%t\nASSURANCE\t%s\n",
		terminalSafe(report.Proof.Basis), terminalSafe(valueOrUnknown(report.Proof.IntentID)), terminalSafe(valueOrUnknown(report.Proof.TerminalMarkerID)),
		report.Proof.MarkersRecorded, report.Proof.HistoricalAuthority, terminalSafe(report.Proof.Assurance))
	fmt.Fprintf(w, "\nWRITE BREAKDOWN\nMARKER TEMPORARIES\t%d\nMARKER PUBLICATIONS\t%d\nAMBIGUOUS MARKER PUBLICATIONS\t%d\nREMOVAL ATTEMPTS\t%d\nFILES REMOVED\t%d\nDIRECTORIES REMOVED\t%d\nBYTES REMOVED\t%d\nAMBIGUOUS REMOVALS\t%d\nMAX MARKER BYTES\t%d\n",
		report.Writes.MarkerTemporaryFiles, report.Writes.MarkerPublications, report.Writes.AmbiguousMarkerPublications, report.Writes.RemovalAttempts,
		report.Writes.FilesRemoved, report.Writes.DirectoriesRemoved, report.Writes.BytesRemoved, report.Writes.AmbiguousRemovals, report.Limits.MaxMarkerBytes)
	fmt.Fprintln(w, "\nISSUES")
	writeClientActivateFindings(w, report.Issues)
	fmt.Fprintln(w, "\nWARNINGS")
	if len(report.Warnings) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
	}
	return w.Flush()
}

func writeClientActivateFindings(w io.Writer, findings []clientactivate.Finding) {
	if len(findings) == 0 {
		fmt.Fprintln(w, "-\tnone")
		return
	}
	for _, finding := range findings {
		fmt.Fprintf(w, "-\t%s\t%s\n", terminalSafe(finding.Code), terminalSafe(finding.Message))
	}
}
