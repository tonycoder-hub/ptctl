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
	"github.com/tonycoder-hub/ptctl/internal/clientremove"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/downloader/qbittorrent"
	"github.com/tonycoder-hub/ptctl/internal/downloader/transmission"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const (
	clientRemoveDefaultTimeout = 24 * time.Hour
	clientRemoveMaximumTimeout = 7 * 24 * time.Hour
)

type clientRemoveFlags struct {
	output               *string
	storeRoot            *string
	variantID            *string
	targetRoot           *string
	materializeOperation *string
	materializePlanID    *string
	activationOperation  *string
	activationPlanID     *string
	hostRoot             *string
	clientRoot           *string
	clientStyle          *string
	driver               *string
	endpoint             *string
	username             *string
	passwordStdin        *bool
	timeout              *time.Duration
}

type preparedClientRemove struct {
	output               string
	storeRoot            string
	artifactID           metastore.ArtifactID
	targetRoot           string
	materializeOperation materialize.OperationID
	materializePlanID    string
	activationOperation  clientactivate.OperationID
	activationPlanID     string
	hostRoot             string
	clientRoot           string
	clientWindows        bool
	clientConfigID       string
	adapter              downloader.ExistingJobRemovalDriver
	username             string
	timeout              time.Duration
}

func (a *app) clientRemove(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		a.clientRemoveHelp()
		return nil
	}
	switch args[0] {
	case "plan":
		return a.clientRemovePlan(args[1:])
	case "run":
		return a.clientRemoveRun(args[1:])
	case "resume":
		return a.clientRemoveResume(args[1:])
	case "status":
		return a.clientRemoveStatus(args[1:])
	default:
		return usageError("unknown client remove subcommand %q", args[0])
	}
}

func (a *app) clientRemoveHelp() {
	fmt.Fprint(a.stdout, `Usage:
  ptctl client remove plan --metafile-store DIR --metafile-variant ID --target PATH --materialize-operation ID --materialize-plan-id ID --activation-operation ID --activation-plan-id ID --host-root PATH --client-root PATH --client-style posix|windows --driver qbittorrent|transmission --url URL --username USER --password-stdin [--output table|json]
  ptctl client remove run [same selectors] --expect-removal-plan-id ID --acknowledge-client-removal [--output table|json]
  ptctl client remove resume [same selectors] --expect-removal-plan-id ID [--acknowledge-client-removal] [--acknowledge-repeat-removal] [--output table|json] OPERATION_ID
  ptctl client remove status --target PATH --expect-removal-plan-id ID [--output table|json] OPERATION_ID

This workflow removes exactly one currently verified downloader job while
explicitly retaining all local data. It never accepts a delete-data option,
never selects by name, never fans out to all jobs, and never retries a removal
automatically. qBittorrent sends deleteFiles=false; Transmission sends
delete-local-data=false or delete_local_data=false for its negotiated protocol.

Plan is read-only. Run repeats current typed identity, effective path, complete
file-layout, activation, and exact materialized-final proof before durably
journaling one request intent. An accepted response is not completion: the same
session must then observe the exact typed job absent, followed by another exact
filesystem verification. These observations are bracketed and non-atomic.

If the request result is unknown, resume first checks for exact absence and
does not attribute causality. It can repeat a still-present job removal only
with --acknowledge-client-removal and --acknowledge-repeat-removal. Status is
local-only historical journal inspection; it does not claim the job is still
absent or the final bytes are currently intact.
`)
}

func addClientRemoveFlags(fs *flag.FlagSet) *clientRemoveFlags {
	values := &clientRemoveFlags{}
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
	values.timeout = fs.Duration("timeout", clientRemoveDefaultTimeout, "shared local proof, journal, and downloader wall-clock budget")
	return values
}

func prepareClientRemoveFlags(fs *flag.FlagSet, values *clientRemoveFlags, command string, positional int) (preparedClientRemove, error) {
	var result preparedClientRemove
	if fs.NArg() != positional {
		return result, usageError("%s has an invalid positional argument count", command)
	}
	if err := validateOutput(*values.output); err != nil {
		return result, err
	}
	if *values.timeout <= 0 || *values.timeout > clientRemoveMaximumTimeout {
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
	adapter, err := newExistingJobRemovalDriver(*values.driver, *values.endpoint)
	if err != nil {
		return result, usageError("%s downloader endpoint is invalid", command)
	}
	clientConfigID, err := adapter.ClientConfigID(*values.username)
	if err != nil {
		return result, usageError("%s downloader configuration is invalid", command)
	}
	return preparedClientRemove{
		output: *values.output, storeRoot: *values.storeRoot, artifactID: artifactID, targetRoot: *values.targetRoot,
		materializeOperation: materializeOperation, materializePlanID: *values.materializePlanID,
		activationOperation: activationOperation, activationPlanID: *values.activationPlanID,
		hostRoot: *values.hostRoot, clientRoot: *values.clientRoot, clientWindows: windows,
		clientConfigID: clientConfigID, adapter: adapter, username: *values.username, timeout: *values.timeout,
	}, nil
}

func newExistingJobRemovalDriver(name, endpoint string) (downloader.ExistingJobRemovalDriver, error) {
	switch name {
	case downloader.DriverQBittorrent:
		return qbittorrent.New(endpoint)
	case downloader.DriverTransmission:
		return transmission.New(endpoint)
	default:
		return nil, fmt.Errorf("unsupported existing-job removal driver %q", name)
	}
}

func (a *app) prepareClientRemoveLocal(ctx context.Context, prepared preparedClientRemove) (*clientactivate.CurrentUseAuthority, error) {
	store, err := metastore.Open(prepared.storeRoot)
	if err != nil {
		return nil, fmt.Errorf("private metafile store could not be opened")
	}
	meta, ref, err := store.Load(ctx, prepared.artifactID, metastore.DefaultLimits())
	if errors.Is(err, metastore.ErrCorruptArtifact) {
		return nil, &integrityErr{message: "client removal metafile failed digest or parse validation"}
	}
	if err != nil {
		return nil, fmt.Errorf("client removal metafile could not be read")
	}
	if ref.ID != prepared.artifactID || meta.MetafileVariantID != prepared.artifactID.String() {
		return nil, &integrityErr{message: "client removal metafile identity disagrees"}
	}
	final, _, err := materialize.VerifyCurrentFinal(ctx, materialize.FinalProofOptions{
		Meta: meta, TargetRoot: prepared.targetRoot, OperationID: prepared.materializeOperation,
		ExpectedPlanID: prepared.materializePlanID, Limits: materialize.DefaultLimits(),
	})
	if errors.Is(err, materialize.ErrIntegrity) {
		return nil, &integrityErr{message: "client removal materialized final failed current exact verification"}
	}
	if err != nil {
		return nil, fmt.Errorf("client removal materialized final could not be verified")
	}
	activation, _, err := clientactivate.VerifyCompletion(ctx, clientactivate.CompletionProofOptions{
		TargetRoot: prepared.targetRoot, OperationID: prepared.activationOperation, ExpectedPlanID: prepared.activationPlanID,
	})
	if errors.Is(err, clientactivate.ErrIntegrity) {
		return nil, &integrityErr{message: "client removal activation journal failed integrity validation"}
	}
	if err != nil {
		return nil, fmt.Errorf("client removal terminal activation could not be verified")
	}
	authority, err := clientactivate.PrepareCurrentUse(final, activation, clientactivate.CurrentUseOptions{
		ClientConfigID: prepared.clientConfigID, HostRoot: prepared.hostRoot, ClientRoot: prepared.clientRoot,
		ClientWindows: prepared.clientWindows, FileLimits: downloader.DefaultJobFileLedgerLimits(),
	})
	if errors.Is(err, clientactivate.ErrIntegrity) {
		return nil, &integrityErr{message: "client removal selectors disagree with the terminal activation"}
	}
	if errors.Is(err, clientactivate.ErrPolicy) {
		return nil, &inconclusiveErr{message: "client removal selectors do not reproduce the reviewed activation"}
	}
	if err != nil {
		return nil, fmt.Errorf("client removal current-use authority could not be prepared")
	}
	return authority, nil
}

func (a *app) clientRemovePlan(args []string) error {
	fs := newFlagSet("client remove plan")
	values := addClientRemoveFlags(fs)
	if err := fs.Parse(args); err != nil {
		return usageError("client remove plan: %v", err)
	}
	prepared, err := prepareClientRemoveFlags(fs, values, "client remove plan", 0)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), prepared.timeout)
	defer cancel()
	authority, err := a.prepareClientRemoveLocal(ctx, prepared)
	if err != nil {
		return err
	}
	credential, err := readDownloaderCredential(a.stdin, prepared.username)
	if err != nil {
		return err
	}
	session, err := prepared.adapter.OpenExistingJobRemovalSession(ctx, credential)
	if err != nil {
		requests, _ := downloader.RequestsMadeFromError(err)
		report := clientremove.FailureReport("", "", "plan", err)
		report.RecordSessionRequests(requests)
		return a.finishClientRemove(prepared.output, report, err)
	}
	defer session.Close()
	descriptor, err := session.ReadExistingJobRemovalDescriptor(ctx)
	if err != nil {
		report := clientremove.FailureReport("", "", "plan", err)
		report.RecordSessionRequests(session.RequestsMade())
		return a.finishClientRemove(prepared.output, report, err)
	}
	current, _, err := clientactivate.VerifyCurrentUse(ctx, authority, session)
	if err != nil {
		report := clientremove.FailureReport("", "", "plan", err)
		report.RecordSessionRequests(session.RequestsMade())
		return a.finishClientRemove(prepared.output, report, err)
	}
	review, err := clientremove.BuildPlan(current, descriptor)
	if err != nil {
		report := clientremove.FailureReport("", "", "plan", err)
		report.RecordSessionRequests(session.RequestsMade())
		return a.finishClientRemove(prepared.output, report, err)
	}
	report, operationErr := clientremove.Preview(review)
	report.RecordSessionRequests(session.RequestsMade())
	return a.finishClientRemove(prepared.output, report, operationErr)
}

func (a *app) clientRemoveRun(args []string) error {
	fs := newFlagSet("client remove run")
	values := addClientRemoveFlags(fs)
	expected := fs.String("expect-removal-plan-id", "", "reviewed 24-hex client removal plan ID")
	acknowledge := fs.Bool("acknowledge-client-removal", false, "acknowledge one exact-job removal while retaining all data")
	if err := fs.Parse(args); err != nil {
		return usageError("client remove run: %v", err)
	}
	if !validMaterializePlanID(*expected) || !*acknowledge {
		return usageError("client remove run requires canonical --expect-removal-plan-id and --acknowledge-client-removal")
	}
	prepared, err := prepareClientRemoveFlags(fs, values, "client remove run", 0)
	if err != nil {
		return err
	}
	operation := clientremove.OperationIDForPlan(*expected)
	ctx, cancel := context.WithTimeout(context.Background(), prepared.timeout)
	defer cancel()
	status, statusErr := clientremove.Status(ctx, clientremove.StatusOptions{TargetRoot: prepared.targetRoot, OperationID: operation, ExpectedPlanID: *expected})
	if statusErr == nil || !errors.Is(statusErr, clientremove.ErrOperationNotFound) && !errors.Is(statusErr, clientremove.ErrInitializationIncomplete) {
		if statusErr == nil {
			statusErr = fmt.Errorf("%w: client removal operation already exists; use resume", clientremove.ErrPolicy)
			status = clientremove.WithFailure(status, statusErr)
		}
		return a.finishClientRemove(prepared.output, status, statusErr)
	}
	authority, err := a.prepareClientRemoveLocal(ctx, prepared)
	if err != nil {
		return err
	}
	credential, err := readDownloaderCredential(a.stdin, prepared.username)
	if err != nil {
		return err
	}
	session, err := prepared.adapter.OpenExistingJobRemovalSession(ctx, credential)
	if err != nil {
		requests, _ := downloader.RequestsMadeFromError(err)
		report := clientremove.FailureReport(*expected, operation, "run", err)
		report.RecordSessionRequests(requests)
		return a.finishClientRemove(prepared.output, report, err)
	}
	defer session.Close()
	descriptor, err := session.ReadExistingJobRemovalDescriptor(ctx)
	if err != nil {
		report := clientremove.FailureReport(*expected, operation, "run", err)
		report.RecordSessionRequests(session.RequestsMade())
		return a.finishClientRemove(prepared.output, report, err)
	}
	current, _, err := clientactivate.VerifyCurrentUse(ctx, authority, session)
	if err != nil {
		report := clientremove.FailureReport(*expected, operation, "run", err)
		report.RecordSessionRequests(session.RequestsMade())
		return a.finishClientRemove(prepared.output, report, err)
	}
	review, err := clientremove.BuildPlan(current, descriptor)
	if err != nil {
		report := clientremove.FailureReport(*expected, operation, "run", err)
		report.RecordSessionRequests(session.RequestsMade())
		return a.finishClientRemove(prepared.output, report, err)
	}
	report, operationErr := clientremove.Run(ctx, clientremove.RunOptions{
		TargetRoot: prepared.targetRoot, Prepared: review, ExpectedPlanID: *expected, Session: session, AcknowledgeRemoval: true,
	})
	report.RecordSessionRequests(session.RequestsMade())
	return a.finishClientRemove(prepared.output, report, operationErr)
}

func (a *app) clientRemoveResume(args []string) error {
	fs := newFlagSet("client remove resume")
	values := addClientRemoveFlags(fs)
	expected := fs.String("expect-removal-plan-id", "", "reviewed 24-hex client removal plan ID")
	acknowledge := fs.Bool("acknowledge-client-removal", false, "acknowledge one exact-job removal while retaining all data")
	repeat := fs.Bool("acknowledge-repeat-removal", false, "acknowledge repeating a prior inconclusive removal request")
	if err := fs.Parse(args); err != nil {
		return usageError("client remove resume: %v", err)
	}
	if fs.NArg() != 1 || !validMaterializePlanID(*expected) || *repeat && !*acknowledge {
		return usageError("client remove resume requires one operation, a canonical plan ID, and repeat acknowledgement paired with removal acknowledgement")
	}
	operation, err := clientremove.ParseOperationID(fs.Arg(0))
	if err != nil || clientremove.OperationIDForPlan(*expected) != operation {
		return usageError("client remove resume operation and plan IDs disagree")
	}
	prepared, err := prepareClientRemoveFlags(fs, values, "client remove resume", 1)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), prepared.timeout)
	defer cancel()
	preflight, preflightErr := clientremove.Status(ctx, clientremove.StatusOptions{
		TargetRoot: prepared.targetRoot, OperationID: operation, ExpectedPlanID: *expected,
	})
	initializationIncomplete := errors.Is(preflightErr, clientremove.ErrInitializationIncomplete)
	if preflightErr != nil && !initializationIncomplete {
		return a.finishClientRemove(prepared.output, preflight, preflightErr)
	}
	if initializationIncomplete && !*acknowledge {
		policyErr := fmt.Errorf("%w: interrupted initialization requires removal acknowledgement", clientremove.ErrPolicy)
		return a.finishClientRemove(prepared.output, clientremove.WithFailure(preflight, policyErr), policyErr)
	}
	if !initializationIncomplete && (preflight.Operation.Phase == "journaled" || preflight.Operation.Phase == "request_not_sent") && !*acknowledge {
		policyErr := fmt.Errorf("%w: operation has no removal attempt; acknowledgement is required", clientremove.ErrPolicy)
		return a.finishClientRemove(prepared.output, clientremove.WithFailure(preflight, policyErr), policyErr)
	}
	priorRequestMayNeedRepeat := preflight.Operation.Phase == "request_result_unknown" || preflight.Operation.Phase == "accepted_response_observed"
	if !initializationIncomplete && priorRequestMayNeedRepeat && !*repeat {
		unknownErr := fmt.Errorf("%w: explicit repeat acknowledgement is required if the job remains present", clientremove.ErrRequestUnknown)
		return a.finishClientRemove(prepared.output, clientremove.WithFailure(preflight, unknownErr), unknownErr)
	}
	authority, err := a.prepareClientRemoveLocal(ctx, prepared)
	if err != nil {
		return err
	}
	credential, err := readDownloaderCredential(a.stdin, prepared.username)
	if err != nil {
		return err
	}
	session, err := prepared.adapter.OpenExistingJobRemovalSession(ctx, credential)
	if err != nil {
		requests, _ := downloader.RequestsMadeFromError(err)
		report := clientremove.FailureReport(*expected, operation, "resume", err)
		report.RecordSessionRequests(requests)
		return a.finishClientRemove(prepared.output, report, err)
	}
	defer session.Close()
	if initializationIncomplete {
		descriptor, descriptorErr := session.ReadExistingJobRemovalDescriptor(ctx)
		if descriptorErr != nil {
			report := clientremove.FailureReport(*expected, operation, "resume_initialization", descriptorErr)
			report.RecordSessionRequests(session.RequestsMade())
			return a.finishClientRemove(prepared.output, report, descriptorErr)
		}
		current, _, currentErr := clientactivate.VerifyCurrentUse(ctx, authority, session)
		if currentErr != nil {
			report := clientremove.FailureReport(*expected, operation, "resume_initialization", currentErr)
			report.RecordSessionRequests(session.RequestsMade())
			return a.finishClientRemove(prepared.output, report, currentErr)
		}
		review, buildErr := clientremove.BuildPlan(current, descriptor)
		if buildErr != nil {
			report := clientremove.FailureReport(*expected, operation, "resume_initialization", buildErr)
			report.RecordSessionRequests(session.RequestsMade())
			return a.finishClientRemove(prepared.output, report, buildErr)
		}
		report, operationErr := clientremove.Run(ctx, clientremove.RunOptions{
			TargetRoot: prepared.targetRoot, Prepared: review, ExpectedPlanID: *expected, Session: session, AcknowledgeRemoval: true,
		})
		report.RecordSessionRequests(session.RequestsMade())
		return a.finishClientRemove(prepared.output, report, operationErr)
	}
	report, operationErr := clientremove.Resume(ctx, clientremove.ResumeOptions{
		TargetRoot: prepared.targetRoot, OperationID: operation, ExpectedPlanID: *expected, Authority: authority, Session: session,
		AcknowledgeRemoval: *acknowledge, AcknowledgeRepeat: *repeat,
	})
	report.RecordSessionRequests(session.RequestsMade())
	return a.finishClientRemove(prepared.output, report, operationErr)
}

func (a *app) clientRemoveStatus(args []string) error {
	fs := newFlagSet("client remove status")
	output := fs.String("output", "table", "table or json")
	target := fs.String("target", "", "materialized target root")
	expected := fs.String("expect-removal-plan-id", "", "reviewed 24-hex client removal plan ID")
	timeout := fs.Duration("timeout", time.Hour, "bounded private journal read timeout")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *target == "" || !validMaterializePlanID(*expected) {
		return usageError("client remove status requires --target, canonical --expect-removal-plan-id, and one OPERATION_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > time.Hour {
		return usageError("client remove status --timeout must be in (0,1h]")
	}
	operation, err := clientremove.ParseOperationID(fs.Arg(0))
	if err != nil || clientremove.OperationIDForPlan(*expected) != operation {
		return usageError("client remove status operation and plan IDs disagree")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, operationErr := clientremove.Status(ctx, clientremove.StatusOptions{TargetRoot: *target, OperationID: operation, ExpectedPlanID: *expected})
	return a.finishClientRemove(*output, report, operationErr)
}

func (a *app) finishClientRemove(output string, report clientremove.Report, operationErr error) error {
	if output == "json" {
		if err := writeJSON(a.stdout, report, nil); err != nil {
			return err
		}
	} else if output == "table" {
		if err := writeClientRemoveHuman(a.stdout, report); err != nil {
			return err
		}
	} else {
		return usageError("--output must be table or json")
	}
	if operationErr == nil {
		return nil
	}
	if errors.Is(operationErr, clientremove.ErrIntegrity) || errors.Is(operationErr, clientactivate.ErrIntegrity) ||
		errors.Is(operationErr, materialize.ErrIntegrity) {
		return &integrityErr{message: "client removal failed integrity validation; see report"}
	}
	if errors.Is(operationErr, clientremove.ErrPolicy) || errors.Is(operationErr, clientremove.ErrRequestUnknown) ||
		errors.Is(operationErr, clientremove.ErrOperationNotFound) || errors.Is(operationErr, clientremove.ErrInitializationIncomplete) ||
		errors.Is(operationErr, clientactivate.ErrPolicy) {
		return &inconclusiveErr{message: "client removal is not complete; see report"}
	}
	return fmt.Errorf("client removal was interrupted; see report")
}

func writeClientRemoveHuman(out io.Writer, report clientremove.Report) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nWRITES UNCERTAIN\t%t\nDOWNLOADER MUTATION REQUESTS\t%d\nDOWNLOADER SESSION REQUESTS\t%d\nOPERATION ID\t%s\nOPERATION STATUS\t%s\nPHASE\t%s\nRESUMABLE\t%t\n",
		terminalSafe(report.Outcome), terminalSafe(report.Effect), report.Writes.WritesPerformed, report.Writes.WritesUncertain,
		report.Writes.DownloaderRequests, report.Client.RequestsMade, terminalSafe(valueOrUnknown(report.Operation.ID)),
		terminalSafe(report.Operation.Status), terminalSafe(report.Operation.Phase), report.Operation.Resumable)
	fmt.Fprintln(w, "\nBLOCKERS")
	writeClientRemoveStrings(w, report.Blockers)
	fmt.Fprintf(w, "\nPLAN\nID\t%s\nEXPECTED ID\t%s\nMATCHES\t%t\nACTION\t%s\nDELETE LOCAL DATA\t%t\nDRIVER\t%s\nPROTOCOL\t%s\nREMOVE ROUTE\t%s\nCLIENT CONFIG\t%s\nPATH MAPPING\t%s\nUSE ID\t%s\nJOB ID\t%s\nFILE LAYOUT\t%s\nFILE SNAPSHOT\t%s\n",
		terminalSafe(valueOrUnknown(report.Plan.ID)), terminalSafe(materializeValueOr(report.Plan.ExpectedID, "not_requested")), report.Plan.Matches,
		terminalSafe(materializeValueOr(report.Plan.Data.Action, "not_observed")), report.Plan.Data.DeleteLocalData,
		terminalSafe(materializeValueOr(report.Plan.Data.Driver, "not_observed")), terminalSafe(materializeValueOr(report.Plan.Data.Removal.Protocol, "not_observed")),
		terminalSafe(materializeValueOr(report.Plan.Data.Removal.RemoveRouteID, "not_observed")), terminalSafe(valueOrUnknown(report.Plan.Data.ClientConfigID)),
		terminalSafe(valueOrUnknown(report.Plan.Data.PathMappingID)), terminalSafe(valueOrUnknown(report.Plan.Data.UseID)),
		terminalSafe(valueOrUnknown(report.Plan.Data.JobID)), terminalSafe(valueOrUnknown(report.Plan.Data.FileLayoutID)),
		terminalSafe(valueOrUnknown(report.Plan.Data.CompleteFileSnapshotID)))
	fmt.Fprintf(w, "\nCURRENT USE BEFORE REQUEST\nDRIVER\t%s\nUSE ID\t%s\nJOB ID\t%s\nJOB STATE\t%s\nJOB PROGRESS\t%.6f\nFILE LAYOUT\t%s\nALL FILES SELECTED\t%t\nALL FILES COMPLETE\t%t\nPROOF REQUESTS\t%d\nASSURANCE\t%s\n",
		terminalSafe(materializeValueOr(report.Before.Driver, "not_observed")), terminalSafe(valueOrUnknown(report.Before.UseID)),
		terminalSafe(valueOrUnknown(report.Before.JobID)), terminalSafe(materializeValueOr(report.Before.JobState, "not_observed")),
		report.Before.JobProgress, terminalSafe(valueOrUnknown(report.Before.FileLayoutID)), report.Before.AllSelected,
		report.Before.AllComplete, report.Before.RequestsMade, terminalSafe(materializeValueOr(report.Before.Assurance, "not_observed")))
	fmt.Fprintf(w, "\nMUTATION RECEIPT\nSTATUS\t%s\nCOMPLETE\t%t\nREQUESTS ATTEMPTED\t%d\nAUTOMATIC RETRIES\t%d\nREDIRECTS\t%d\nREQUEST BYTES\t%d\nREQUEST BYTES KNOWN\t%t\nSTOP REASON\t%s\n",
		terminalSafe(report.Mutation.Status), report.Mutation.Receipt.Complete, report.Mutation.Receipt.RequestsAttempted,
		report.Mutation.Receipt.AutomaticRetries, report.Mutation.Receipt.RedirectsFollowed, report.Mutation.Receipt.RequestBytes,
		report.Mutation.Receipt.RequestBytesKnown, terminalSafe(materializeValueOr(report.Mutation.Receipt.StopReason, "none")))
	fmt.Fprintf(w, "\nPOST-REQUEST EVIDENCE\nJOB ABSENCE\t%s\nABSENCE REQUESTS\t%d\nJOBS EXAMINED\t%d\nFINAL VARIANT\t%s\nFINAL IDENTITY\t%s\nBYTES VERIFIED\t%d\nCOMPLETION BASIS\t%s\nQUEUE EVIDENCE\t%s\nFILESYSTEM EVIDENCE\t%s\nATOMICITY\t%s\n",
		terminalSafe(report.Absence.Status), report.Absence.Observation.RequestsMade, report.Absence.Observation.JobsExamined,
		terminalSafe(valueOrUnknown(report.Final.MetafileVariantID)), terminalSafe(valueOrUnknown(report.Final.FinalObjectIdentity)),
		report.Final.BytesVerified, terminalSafe(report.Assurance.CompletionBasis), terminalSafe(report.Assurance.QueueEvidence),
		terminalSafe(report.Assurance.FilesystemEvidence), terminalSafe(report.Assurance.Atomicity))
	fmt.Fprintf(w, "\nPRIVATE JOURNAL WRITES\nDIRECTORIES CREATED\t%d\nFILES CREATED\t%d\nFILES REMOVED\t%d\nMARKER PUBLICATIONS\t%d\nMARKER BYTES\t%d\nDELETE LOCAL DATA REQUESTED\t%t\nRETRY POLICY\t%s\n",
		report.Writes.PrivateDirectoriesCreated, report.Writes.PrivateFilesCreated, report.Writes.PrivateFilesRemoved, report.Writes.MarkerPublications,
		report.Writes.MarkerBytesWritten, report.Assurance.DeleteLocalDataRequested, terminalSafe(report.Assurance.RequestRetryPolicy))
	fmt.Fprintln(w, "\nWARNINGS")
	writeClientRemoveStrings(w, report.Warnings)
	return w.Flush()
}

func writeClientRemoveStrings(out io.Writer, values []string) {
	if len(values) == 0 {
		fmt.Fprintln(out, "-\tnone")
		return
	}
	for _, value := range values {
		fmt.Fprintf(out, "-\t%s\n", terminalSafe(value))
	}
}
