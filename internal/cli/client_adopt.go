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
	"github.com/tonycoder-hub/ptctl/internal/downloader/transmission"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

const (
	clientAdoptDefaultTimeout = 24 * time.Hour
	clientAdoptMaximumTimeout = 7 * 24 * time.Hour
)

type clientAdoptFlags struct {
	output                 *string
	storeRoot              *string
	variantID              *string
	targetRoot             *string
	materializeOperation   *string
	materializePlanID      *string
	hostRoot               *string
	clientRoot             *string
	clientStyle            *string
	driver                 *string
	endpoint               *string
	username               *string
	passwordStdin          *bool
	adoptExistingStopped   *bool
	priorAdoptionOperation *string
	priorAdoptionPlanID    *string
	timeout                *time.Duration
	expectedAdoptionPlan   *string
	acknowledgeAdd         *bool
	acknowledgeExisting    *bool
	acknowledgeReAdoption  *bool
	repeatAdd              *bool
}

type preparedClientAdopt struct {
	output   string
	timeout  time.Duration
	prepared *clientadopt.PreparedPlan
	payload  *metastore.ArtifactPayload
	adapter  downloader.StoppedAddDriver
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
	case "prune":
		return a.clientAdoptPrune(args[1:])
	case "forget":
		return a.clientAdoptForget(args[1:])
	default:
		return usageError("unknown client adopt subcommand %q", args[0])
	}
}

func (a *app) clientAdoptHelp() {
	fmt.Fprint(a.stdout, `Usage:
  ptctl client adopt plan --metafile-store DIR --metafile-variant ID --target PATH --materialize-operation ID --materialize-plan-id ID --host-root PATH --client-root PATH --client-style posix|windows --driver qbittorrent|transmission --url URL --username USER --password-stdin [--adopt-existing-stopped | --prior-adoption-operation ID --prior-adoption-plan-id ID] [--output table|json]
  ptctl client adopt run  [same selectors] --expect-adoption-plan-id ID (--acknowledge-client-add [--acknowledge-client-re-adoption] | --adopt-existing-stopped --acknowledge-existing-stopped-adoption) [--output table|json]
  ptctl client adopt resume [same selectors] --expect-adoption-plan-id ID ([--acknowledge-client-add --acknowledge-repeat-add] [--acknowledge-client-re-adoption] | --adopt-existing-stopped --acknowledge-existing-stopped-adoption) [--output table|json] OPERATION_ID
  ptctl client adopt status --target PATH [--output table|json] OPERATION_ID
  ptctl client adopt prune --target PATH --expect-adoption-plan-id ID --acknowledge-operation-state-deletion [--output table|json] OPERATION_ID
  ptctl client adopt forget --target PATH --expect-adoption-plan-id ID --acknowledge-historical-evidence-deletion [--output table|json] OPERATION_ID

Version 1 has two explicit actions. The default adds an absent exact
typed-infohash job in stopped mode using exact raw bytes from the private
metafile store. --adopt-existing-stopped instead records an observation-only
lineage for one already-present exact stopped job at the verified final path.
That mode performs two same-session ledger reads around a fresh exact-final
verification and never constructs or submits the one-shot raw-metafile
mutation payload.

Neither action moves content, rechecks, resumes, deletes, or retires source
data. Observation-only adoption does not mutate the downloader and cannot
prove which private metafile wrapper originally created the existing job.

After a terminal exact job disappears, an explicit prior operation/plan pair
can authorize a new independent adoption lineage. The prior canonical
completion is read locally, the current complete queue must again prove exact
absence, and run/resume additionally require
--acknowledge-client-re-adoption. Prior evidence is retained; no latest
operation is inferred or forgotten automatically.

Run records a durable target-root-local request intent before its one add POST.
If the response is lost, resume first observes the queue and never repeats the
POST unless --acknowledge-repeat-add is explicit. A successful outcome remains
pending client recheck; neither client can prove its stored private variant.

Transmission adoption accepts only v1 metafiles because its audited RPC ledger
exposes a full SHA-1 identity but no typed v2 identity. Its historical
completion can enable only the matching v1 Transmission recheck/start port.

Prune is a separate local-only deletion boundary. It copies one terminal
canonical journal into an owner-private tombstone before deleting only that
operation's intent, request-attempt markers, completion, and empty scratch.
The tombstone remains usable as historical authority by client activation.
Forget is a final, separately acknowledged local-only boundary. It publishes a
root-level recovery intent, removes one exact retained adoption tombstone, then
removes that last intent. A repeated call reports only unattributed absence.
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
	values.driver = fs.String("driver", "qbittorrent", "downloader driver: qbittorrent or transmission")
	values.endpoint = fs.String("url", "", "downloader API origin or Transmission RPC URL")
	values.username = fs.String("username", "", "downloader username")
	values.passwordStdin = fs.Bool("password-stdin", false, "read downloader password from stdin")
	values.adoptExistingStopped = fs.Bool("adopt-existing-stopped", false, "observe and adopt one existing exact stopped job without submitting a metafile or mutating the downloader")
	values.priorAdoptionOperation = fs.String("prior-adoption-operation", "", "explicit terminal prior adoption operation authorizing re-adoption; pair with --prior-adoption-plan-id")
	values.priorAdoptionPlanID = fs.String("prior-adoption-plan-id", "", "reviewed prior adoption plan ID; pair with --prior-adoption-operation")
	values.timeout = fs.Duration("timeout", clientAdoptDefaultTimeout, "shared final-proof and client wall-clock budget")
	if execution {
		values.expectedAdoptionPlan = fs.String("expect-adoption-plan-id", "", "reviewed 24-hex client adoption plan ID")
		values.acknowledgeAdd = fs.Bool("acknowledge-client-add", false, "acknowledge one stopped downloader add request")
		values.acknowledgeExisting = fs.Bool("acknowledge-existing-stopped-adoption", false, "acknowledge recording an observation-only lineage for one existing exact stopped job")
		values.acknowledgeReAdoption = fs.Bool("acknowledge-client-re-adoption", false, "acknowledge a new stopped add lineage after the explicit prior terminal job disappeared")
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
	windows := *values.clientStyle == "windows"
	if err := storage.ValidatePathMappingConfig(*values.hostRoot, *values.clientRoot, windows); err != nil {
		return result, usageError("%s path mapping is invalid: %v", command, err)
	}
	adapter, err := newStoppedAddDriver(*values.driver, *values.endpoint)
	if err != nil {
		return result, usageError("%s downloader endpoint is invalid", command)
	}
	clientConfigID, err := adapter.ClientConfigID(*values.username)
	if err != nil {
		return result, usageError("%s downloader configuration is invalid", command)
	}
	priorOperationSet := flagWasSet(fs, "prior-adoption-operation")
	priorPlanSet := flagWasSet(fs, "prior-adoption-plan-id")
	if priorOperationSet != priorPlanSet || priorOperationSet && (*values.priorAdoptionOperation == "" || *values.priorAdoptionPlanID == "") {
		return result, usageError("%s requires --prior-adoption-operation and --prior-adoption-plan-id together", command)
	}
	if *values.adoptExistingStopped && priorOperationSet {
		return result, usageError("%s observation-only adoption cannot consume prior adoption-lineage selectors", command)
	}
	var priorCompletion *clientadopt.VerifiedCompletion
	if priorOperationSet {
		priorOperation, parseErr := clientadopt.ParseOperationID(*values.priorAdoptionOperation)
		if parseErr != nil || !validMaterializePlanID(*values.priorAdoptionPlanID) {
			return result, usageError("%s requires canonical prior adoption operation and plan IDs", command)
		}
		verifiedPrior, _, verifyErr := clientadopt.VerifyCompletion(ctx, clientadopt.CompletionProofOptions{
			TargetRoot: *values.targetRoot, OperationID: priorOperation, ExpectedPlanID: *values.priorAdoptionPlanID,
		})
		if errors.Is(verifyErr, clientadopt.ErrIntegrity) {
			return result, &integrityErr{message: "prior client adoption completion failed integrity validation"}
		}
		if verifyErr != nil {
			return result, &inconclusiveErr{message: "prior client adoption completion is unavailable"}
		}
		priorCompletion = verifiedPrior
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
		Driver: *values.driver, ClientConfigID: clientConfigID, HostRoot: *values.hostRoot, ClientRoot: *values.clientRoot, ClientWindows: windows,
		PriorCompletion: priorCompletion, AdoptExistingStopped: *values.adoptExistingStopped,
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

func newStoppedAddDriver(name, endpoint string) (downloader.StoppedAddDriver, error) {
	switch name {
	case downloader.DriverQBittorrent:
		return qbittorrent.New(endpoint)
	case downloader.DriverTransmission:
		return transmission.New(endpoint)
	default:
		return nil, fmt.Errorf("unsupported stopped-add downloader driver %q", name)
	}
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
	if !validMaterializePlanID(*values.expectedAdoptionPlan) || *values.repeatAdd {
		return usageError("client adopt run requires --expect-adoption-plan-id; repeat acknowledgement is resume-only")
	}
	priorRequested := flagWasSet(fs, "prior-adoption-operation") || flagWasSet(fs, "prior-adoption-plan-id")
	if *values.adoptExistingStopped {
		if !*values.acknowledgeExisting || *values.acknowledgeAdd || *values.acknowledgeReAdoption || priorRequested {
			return usageError("client adopt run observation mode requires --adopt-existing-stopped and --acknowledge-existing-stopped-adoption, and forbids add/re-adoption flags")
		}
	} else if *values.acknowledgeExisting || !*values.acknowledgeAdd {
		return usageError("client adopt run add mode requires --acknowledge-client-add and forbids --acknowledge-existing-stopped-adoption")
	} else if priorRequested != *values.acknowledgeReAdoption {
		return usageError("client adopt run requires --acknowledge-client-re-adoption exactly when prior adoption selectors are supplied")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *values.timeout)
	defer cancel()
	prepared, err := prepareClientAdopt(ctx, fs, values, "client adopt run", !*values.adoptExistingStopped, false)
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
	var session downloader.LedgerSession
	if *values.adoptExistingStopped {
		session, err = prepared.adapter.OpenReadSession(ctx, credential)
	} else {
		session, err = prepared.adapter.OpenMutationSession(ctx, credential)
	}
	if err != nil {
		requests, _ := downloader.RequestsMadeFromError(err)
		report := clientadopt.FailureReport(prepared.prepared, *values.expectedAdoptionPlan, "run", requests, err)
		return a.finishClientAdopt(prepared.output, report, err)
	}
	defer session.Close()
	report, operationErr := clientadopt.Run(ctx, clientadopt.RunOptions{
		Prepared: prepared.prepared, ExpectedPlanID: *values.expectedAdoptionPlan, Metafile: prepared.payload,
		Session: session, AcknowledgeAdd: *values.acknowledgeAdd, AcknowledgeExistingStopped: *values.acknowledgeExisting,
		AcknowledgeReAdoption: *values.acknowledgeReAdoption,
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
	priorRequested := flagWasSet(fs, "prior-adoption-operation") || flagWasSet(fs, "prior-adoption-plan-id")
	if *values.adoptExistingStopped {
		if !*values.acknowledgeExisting || *values.acknowledgeAdd || *values.repeatAdd || *values.acknowledgeReAdoption || priorRequested {
			return usageError("client adopt resume observation mode requires --adopt-existing-stopped and --acknowledge-existing-stopped-adoption, and forbids add/repeat/re-adoption flags")
		}
	} else if *values.acknowledgeExisting {
		return usageError("client adopt resume add mode forbids --acknowledge-existing-stopped-adoption")
	} else if priorRequested != *values.acknowledgeReAdoption {
		return usageError("client adopt resume requires --acknowledge-client-re-adoption exactly when prior adoption selectors are supplied")
	}
	operationID, err := clientadopt.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("client adopt resume requires a canonical OPERATION_ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *values.timeout)
	defer cancel()
	prepared, err := prepareClientAdopt(ctx, fs, values, "client adopt resume", !*values.adoptExistingStopped, true)
	if err != nil {
		return err
	}
	// Journal selection and plan matching are checked before password stdin or
	// any downloader request. Status is strictly read-only.
	status, statusErr := clientadopt.Status(ctx, clientadopt.StatusOptions{TargetRoot: *values.targetRoot, OperationID: operationID})
	initializationIncomplete := errors.Is(statusErr, clientadopt.ErrInitializationIncomplete)
	if statusErr != nil && !initializationIncomplete ||
		!initializationIncomplete && status.Plan.ID != prepared.prepared.PlanID() ||
		*values.expectedAdoptionPlan != prepared.prepared.PlanID() || operationID != prepared.prepared.OperationID() {
		if statusErr == nil {
			statusErr = fmt.Errorf("%w: operation belongs to a different adoption plan", clientadopt.ErrPolicy)
		}
		report := clientadopt.FailureReport(prepared.prepared, *values.expectedAdoptionPlan, "resume", 0, statusErr)
		return a.finishClientAdopt(prepared.output, report, statusErr)
	}
	status.Plan.ExpectedID = *values.expectedAdoptionPlan
	status.Plan.Matches = status.Plan.ID == *values.expectedAdoptionPlan
	if status.Operation.Status == "retained" {
		return a.finishClientAdopt(prepared.output, status, nil)
	}
	if status.Operation.Status == "pruning" || status.Operation.Status == "retention_initializing" {
		statusErr = fmt.Errorf("%w: explicit client adopt prune must complete the retention boundary", clientadopt.ErrPolicy)
		return a.finishClientAdopt(prepared.output, status, statusErr)
	}
	if status.Operation.Status == "forgetting" {
		statusErr = fmt.Errorf("%w: explicit client adopt forget must complete historical evidence deletion", clientadopt.ErrPolicy)
		return a.finishClientAdopt(prepared.output, status, statusErr)
	}
	if !*values.adoptExistingStopped &&
		(initializationIncomplete || status.Journal.AttemptsRecorded == 0 && !status.Journal.CompletionDurable) && !*values.acknowledgeAdd {
		statusErr = fmt.Errorf("%w: this operation has no add request intent; resume requires explicit downloader-add acknowledgement", clientadopt.ErrPolicy)
		report := clientadopt.FailureReport(prepared.prepared, *values.expectedAdoptionPlan, "resume", 0, statusErr)
		return a.finishClientAdopt(prepared.output, report, statusErr)
	}
	credential, err := readDownloaderCredential(a.stdin, prepared.username)
	if err != nil {
		return err
	}
	var session downloader.LedgerSession
	if *values.adoptExistingStopped {
		session, err = prepared.adapter.OpenReadSession(ctx, credential)
	} else {
		session, err = prepared.adapter.OpenMutationSession(ctx, credential)
	}
	if err != nil {
		requests, _ := downloader.RequestsMadeFromError(err)
		report := clientadopt.FailureReport(prepared.prepared, *values.expectedAdoptionPlan, "resume", requests, err)
		return a.finishClientAdopt(prepared.output, report, err)
	}
	defer session.Close()
	report, operationErr := clientadopt.Resume(ctx, operationID, clientadopt.RunOptions{
		Prepared: prepared.prepared, ExpectedPlanID: *values.expectedAdoptionPlan, Metafile: prepared.payload,
		Session: session, AcknowledgeAdd: *values.acknowledgeAdd, AcknowledgeExistingStopped: *values.acknowledgeExisting,
		AcknowledgeReAdoption: *values.acknowledgeReAdoption,
		RepeatAdd:             *values.repeatAdd,
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

func (a *app) clientAdoptPrune(args []string) error {
	fs := newFlagSet("client adopt prune")
	output := fs.String("output", "table", "table or json")
	targetRoot := fs.String("target", "", "materialized target root")
	expectedPlanID := fs.String("expect-adoption-plan-id", "", "reviewed 24-hex client adoption plan ID")
	acknowledge := fs.Bool("acknowledge-operation-state-deletion", false, "acknowledge deletion of one terminal adoption journal")
	timeout := fs.Duration("timeout", time.Hour, "bounded private retention transition timeout")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *targetRoot == "" {
		return usageError("client adopt prune requires --target, --expect-adoption-plan-id, acknowledgement, and one OPERATION_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if !validMaterializePlanID(*expectedPlanID) || !*acknowledge {
		return usageError("client adopt prune requires canonical --expect-adoption-plan-id and --acknowledge-operation-state-deletion")
	}
	if *timeout <= 0 || *timeout > time.Hour {
		return usageError("client adopt prune --timeout must be in (0,1h]")
	}
	operationID, err := clientadopt.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("client adopt prune requires a canonical OPERATION_ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, operationErr := clientadopt.Prune(ctx, clientadopt.PruneOptions{TargetRoot: *targetRoot, OperationID: operationID,
		ExpectedPlanID: *expectedPlanID, Acknowledge: true, Limits: clientadopt.DefaultRetentionLimits()})
	return a.finishClientAdoptRetention(*output, report, operationErr)
}

func (a *app) clientAdoptForget(args []string) error {
	fs := newFlagSet("client adopt forget")
	output := fs.String("output", "table", "table or json")
	targetRoot := fs.String("target", "", "materialized target root")
	expectedPlanID := fs.String("expect-adoption-plan-id", "", "reviewed 24-hex client adoption plan ID")
	acknowledge := fs.Bool("acknowledge-historical-evidence-deletion", false, "acknowledge irreversible deletion of the retained adoption tombstone and final historical attribution")
	timeout := fs.Duration("timeout", time.Minute, "historical-evidence deletion wall-clock budget")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *targetRoot == "" {
		return usageError("client adopt forget requires --target, --expect-adoption-plan-id, acknowledgement, and one OPERATION_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if !validMaterializePlanID(*expectedPlanID) || !*acknowledge {
		return usageError("client adopt forget requires canonical --expect-adoption-plan-id and --acknowledge-historical-evidence-deletion")
	}
	if *timeout <= 0 || *timeout > time.Hour {
		return usageError("client adopt forget --timeout must be in (0,1h]")
	}
	operationID, err := clientadopt.ParseOperationID(fs.Arg(0))
	if err != nil {
		return usageError("client adopt forget requires a canonical OPERATION_ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, operationErr := clientadopt.Forget(ctx, clientadopt.ForgetOptions{TargetRoot: *targetRoot, OperationID: operationID,
		ExpectedPlanID: *expectedPlanID, Acknowledge: true, Limits: clientadopt.DefaultForgetLimits()})
	return a.finishClientAdoptForget(*output, report, operationErr)
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

func (a *app) finishClientAdoptRetention(output string, report clientadopt.RetentionReport, operationErr error) error {
	if output == "json" {
		if err := writeJSON(a.stdout, report, nil); err != nil {
			return err
		}
	} else if err := writeClientAdoptRetentionHuman(a.stdout, report); err != nil {
		return err
	}
	if operationErr == nil {
		return nil
	}
	if errors.Is(operationErr, clientadopt.ErrIntegrity) {
		return &integrityErr{message: "client adoption retention failed integrity validation; see report"}
	}
	if errors.Is(operationErr, clientadopt.ErrPolicy) || errors.Is(operationErr, clientadopt.ErrOperationNotFound) {
		return &inconclusiveErr{message: "client adoption retention is blocked; see report"}
	}
	return fmt.Errorf("client adoption retention was interrupted; see report")
}

func (a *app) finishClientAdoptForget(output string, report clientadopt.ForgetReport, operationErr error) error {
	if output == "json" {
		if err := writeJSON(a.stdout, report, nil); err != nil {
			return err
		}
	} else if err := writeClientAdoptForgetHuman(a.stdout, report); err != nil {
		return err
	}
	if operationErr == nil && report.Outcome == clientadopt.ForgetOutcomeForgotten {
		return nil
	}
	if report.Outcome == clientadopt.ForgetOutcomeIntegrityFailed {
		return &integrityErr{message: "client adoption historical evidence failed integrity validation; see report"}
	}
	if report.Outcome == clientadopt.ForgetOutcomeBlocked {
		return &inconclusiveErr{message: "client adoption historical-evidence deletion is blocked; see report"}
	}
	if report.Outcome == clientadopt.ForgetOutcomeAbsentUnattributed {
		return fmt.Errorf("client adoption historical evidence is absent without a remaining attribution marker; see report")
	}
	return fmt.Errorf("client adoption historical-evidence deletion was interrupted or its durability is unconfirmed; see report")
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
	fmt.Fprintf(w, "\nPLAN\nID\t%s\nEXPECTED ID\t%s\nMATCHES\t%t\nACTION\t%s\nDRIVER\t%s\nCLIENT CONFIG\t%s\nPATH MAPPING\t%s\nPATH SEMANTICS\t%s\nSAVE PATH REF\t%s\nCONTENT PATH REF\t%s\n",
		terminalSafe(valueOrUnknown(report.Plan.ID)), terminalSafe(materializeValueOr(report.Plan.ExpectedID, "not_requested")), report.Plan.Matches,
		terminalSafe(report.Plan.Action), terminalSafe(report.Plan.Driver), terminalSafe(report.Plan.ClientConfigID), terminalSafe(report.Plan.PathMappingID), terminalSafe(report.Plan.ClientPathSemantics),
		terminalSafe(report.Plan.ExpectedSavePathRef), terminalSafe(report.Plan.ExpectedContentPathRef))
	if report.Plan.PriorAdoptionOperationID != "" {
		fmt.Fprintf(w, "PRIOR ADOPTION OPERATION\t%s\nPRIOR ADOPTION PLAN\t%s\nPRIOR COMPLETION\t%s\n",
			terminalSafe(report.Plan.PriorAdoptionOperationID), terminalSafe(report.Plan.PriorAdoptionPlanID), terminalSafe(report.Plan.PriorAdoptionCompletionID))
	}
	fmt.Fprintf(w, "\nMATERIALIZED FINAL\nSTATUS\t%s\nVARIANT\t%s\nMATERIALIZE OPERATION\t%s\nMATERIALIZE PLAN\t%s\nROOT IDENTITY\t%s\nFINAL IDENTITY\t%s\nBYTES VERIFIED\t%d\nASSURANCE\t%s\n",
		terminalSafe(report.Final.Status), terminalSafe(report.Final.Observation.MetafileVariantID), terminalSafe(report.Final.Observation.OperationID),
		terminalSafe(report.Final.Observation.MaterializePlanID), terminalSafe(report.Final.Observation.TargetRootIdentity), terminalSafe(report.Final.Observation.FinalObjectIdentity),
		report.Final.Observation.BytesVerified, terminalSafe(report.Final.Observation.Assurance))
	fmt.Fprintf(w, "\nCLIENT\nSTATUS\t%s\nREQUESTS MADE\t%d\nBEFORE IDENTITY\t%s\nAFTER IDENTITY\t%s\nADD ATTEMPTED\t%t\nADD COMPLETE\t%t\nAUTOMATIC RETRIES\t%d\nREDIRECTS\t%d\nJOB ID\t%s\nJOB STATE\t%s\nCONTENT PATH REF\t%s\nVARIANT RELATION\t%s\nASSURANCE\t%s\n",
		terminalSafe(report.Client.Status), report.Client.RequestsMade, terminalSafe(report.Client.BeforeIdentity), terminalSafe(report.Client.AfterIdentity),
		report.Client.AddAttempted, report.Client.AddReceipt.Complete, report.Client.AddReceipt.AutomaticRetries, report.Client.AddReceipt.RedirectsFollowed,
		terminalSafe(materializeValueOr(report.Client.JobID, "not_observed")), terminalSafe(materializeValueOr(report.Client.JobState, "not_observed")),
		terminalSafe(materializeValueOr(report.Client.ContentPathRef, "not_observed")), terminalSafe(report.Client.VariantRelation), terminalSafe(report.Client.Assurance))
	fmt.Fprintf(w, "\nJOURNAL / WRITES\nSTATUS\t%s\nINTENT DURABLE\t%t\nATTEMPTS RECORDED\t%d\nCOMPLETION DURABLE\t%t\nPENDING RECOVERY MARKER\t%t\nRETENTION STATE\t%s\nRETENTION INTENT PRESENT\t%t\nRETENTION COMPLETE PRESENT\t%t\nRETENTION INTENT DURABLE\t%t\nRETENTION COMPLETE DURABLE\t%t\nOPERATION DIRECTORIES\t%d\nCONTROL DIRECTORIES\t%d\nTEMPORARY FILES\t%d\nTEMPORARY BYTES\t%d\nMARKER PUBLICATIONS\t%d\nTEMPORARY REMOVALS\t%d\nAMBIGUOUS PUBLICATIONS\t%d\n",
		terminalSafe(report.Journal.Status), report.Journal.IntentDurable, report.Journal.AttemptsRecorded, report.Journal.CompletionDurable, report.Journal.PendingRecoveryMarker,
		terminalSafe(report.Journal.RetentionState), report.Journal.RetentionIntentPresent, report.Journal.RetentionCompletionPresent,
		report.Journal.RetentionIntentDurable, report.Journal.RetentionCompletionDurable,
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

func writeClientAdoptRetentionHuman(out io.Writer, report clientadopt.RetentionReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nWRITES UNCERTAIN\t%t\nOPERATION ID\t%s\nOPERATION STATUS\t%s\nPHASE BEFORE\t%s\nPHASE AFTER\t%s\nRESUMABLE\t%t\n",
		terminalSafe(string(report.Outcome)), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed, report.WritesUncertain,
		terminalSafe(report.Operation.ID), terminalSafe(report.Operation.Status), terminalSafe(report.Operation.PhaseBefore), terminalSafe(report.Operation.PhaseAfter), report.Operation.Resumable)
	fmt.Fprintln(w, "\nBLOCKERS")
	writeClientAdoptFindings(w, report.Blockers)
	fmt.Fprintf(w, "\nPLAN / TARGET\nPLAN ID\t%s\nEXPECTED ID\t%s\nMATCHES\t%t\nEXPECTED ROOT IDENTITY\t%s\nOBSERVED ROOT IDENTITY\t%s\nROOT BOUND\t%t\nSTABILITY\t%s\n",
		terminalSafe(valueOrUnknown(report.Plan.ID)), terminalSafe(valueOrUnknown(report.Plan.ExpectedID)), report.Plan.Matches,
		terminalSafe(valueOrUnknown(report.Target.ExpectedRootIdentity)), terminalSafe(valueOrUnknown(report.Target.ObservedRootIdentity)),
		report.Target.RootIdentityBound, terminalSafe(report.Target.StabilityAssurance))
	fmt.Fprintf(w, "\nHISTORICAL PROOF\nBASIS\t%s\nINTENT ID\t%s\nCOMPLETION ID\t%s\nATTEMPTS\t%d\nAUTHORITY\t%t\nASSURANCE\t%s\n",
		terminalSafe(report.Proof.Basis), terminalSafe(valueOrUnknown(report.Proof.IntentID)), terminalSafe(valueOrUnknown(report.Proof.CompletionID)),
		report.Proof.AttemptsRecorded, report.Proof.HistoricalAuthority, terminalSafe(report.Proof.Assurance))
	fmt.Fprintf(w, "\nRETENTION / WRITES\nSTATE\t%s\nINTENT MARKER\t%s\nCOMPLETE MARKER\t%s\nINTENT DURABLE\t%t\nCOMPLETION DURABLE\t%t\nEXACT TOMBSTONE\t%t\nPRUNE RESUMABLE\t%t\nCONTROL DIRECTORIES\t%d\nTEMPORARY FILES\t%d\nTEMPORARY BYTES\t%d\nMARKER PUBLICATIONS\t%d\nFILES REMOVED\t%d\nDIRECTORIES REMOVED\t%d\nBYTES REMOVED\t%d\n",
		terminalSafe(report.Markers.State), terminalSafe(valueOrUnknown(report.Markers.IntentMarkerID)), terminalSafe(valueOrUnknown(report.Markers.CompleteMarkerID)),
		report.Markers.IntentDurable, report.Markers.CompletionDurable, report.Markers.ExactTombstone, report.Markers.PruneResumable,
		report.Writes.ControlDirectoriesCreated, report.Writes.MarkerTemporaryFiles, report.Writes.MarkerTemporaryBytes,
		report.Writes.MarkerPublications, report.Writes.FilesRemoved, report.Writes.DirectoriesRemoved, report.Writes.BytesRemoved)
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

func writeClientAdoptForgetHuman(out io.Writer, report clientadopt.ForgetReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nWRITES UNCERTAIN\t%t\nOPERATION ID\t%s\nOPERATION STATUS\t%s\nPHASE BEFORE\t%s\nPHASE AFTER\t%s\nRESUMABLE\t%t\n",
		terminalSafe(string(report.Outcome)), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed, report.WritesUncertain,
		terminalSafe(report.Operation.ID), terminalSafe(report.Operation.Status), terminalSafe(report.Operation.PhaseBefore), terminalSafe(report.Operation.PhaseAfter), report.Operation.Resumable)
	fmt.Fprintln(w, "\nBLOCKERS")
	writeClientAdoptFindings(w, report.Blockers)
	fmt.Fprintf(w, "\nPLAN / TARGET\nPLAN ID\t%s\nEXPECTED ID\t%s\nMATCHES\t%t\nACTION\t%s\nEXPECTED ROOT IDENTITY\t%s\nOBSERVED ROOT IDENTITY\t%s\nROOT BOUND\t%t\nSTABILITY\t%s\n",
		terminalSafe(valueOrUnknown(report.Plan.ID)), terminalSafe(valueOrUnknown(report.Plan.ExpectedID)), report.Plan.Matches, terminalSafe(report.Plan.Action),
		terminalSafe(valueOrUnknown(report.Target.ExpectedRootIdentity)), terminalSafe(valueOrUnknown(report.Target.ObservedRootIdentity)),
		report.Target.RootIdentityBound, terminalSafe(report.Target.StabilityAssurance))
	fmt.Fprintf(w, "\nAUTHORITY\nSTATE\t%s\nFORGET MARKER\t%s\nMARKER DURABLE\t%t\nRETENTION INTENT\t%s\nRETENTION COMPLETE\t%s\nEXACT TOMBSTONE EVIDENCE AVAILABLE\t%t\nTARGET HISTORICAL EVIDENCE ERASED\t%t\n",
		terminalSafe(report.Authority.State), terminalSafe(valueOrUnknown(report.Authority.MarkerID)), report.Authority.MarkerDurable,
		terminalSafe(valueOrUnknown(report.Authority.RetentionIntentMarkerID)), terminalSafe(valueOrUnknown(report.Authority.RetentionCompleteMarkerID)),
		report.Authority.ExactTombstoneEvidenceAvailable, report.Authority.TargetHistoricalEvidenceErased)
	fmt.Fprintf(w, "\nHISTORICAL PROOF\nBASIS\t%s\nADOPTION INTENT\t%s\nCOMPLETION\t%s\nATTEMPTS\t%d\nAUTHORITY\t%t\nASSURANCE\t%s\n",
		terminalSafe(report.Proof.Basis), terminalSafe(valueOrUnknown(report.Proof.IntentID)), terminalSafe(valueOrUnknown(report.Proof.CompletionID)),
		report.Proof.AttemptsRecorded, report.Proof.HistoricalAuthority, terminalSafe(report.Proof.Assurance))
	fmt.Fprintf(w, "\nWRITE BREAKDOWN\nMARKER TEMPORARIES\t%d\nMARKER PUBLICATIONS\t%d\nAMBIGUOUS MARKER PUBLICATIONS\t%d\nREMOVAL ATTEMPTS\t%d\nFILES REMOVED\t%d\nDIRECTORIES REMOVED\t%d\nBYTES REMOVED\t%d\nAMBIGUOUS REMOVALS\t%d\nMAX MARKER BYTES\t%d\n",
		report.Writes.MarkerTemporaryFiles, report.Writes.MarkerPublications, report.Writes.AmbiguousMarkerPublications, report.Writes.RemovalAttempts,
		report.Writes.FilesRemoved, report.Writes.DirectoriesRemoved, report.Writes.BytesRemoved, report.Writes.AmbiguousRemovals, report.Limits.MaxMarkerBytes)
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
