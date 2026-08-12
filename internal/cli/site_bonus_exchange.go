package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/bonusexchange"
	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const (
	bonusExchangeDefaultTimeout  = 45 * time.Second
	bonusExchangeMaximumTimeout  = 5 * time.Minute
	bonusExchangeFinalizeTimeout = 15 * time.Second
)

type siteBonusExchangeReport struct {
	kind                   string
	Outcome                string                           `json:"outcome"`
	Effect                 []string                         `json:"effect"`
	WritesPerformed        int                              `json:"writes_performed"`
	FormSubmissionAttempts int                              `json:"form_submission_attempts"`
	Acknowledgement        siteBonusExchangeAcknowledgement `json:"acknowledgement"`
	Scope                  siteBonusExchangeScope           `json:"scope"`
	Operation              siteBonusExchangeOperation       `json:"operation"`
	Request                siteBonusExchangeRequest         `json:"request"`
	Persistence            siteBonusExchangePersistence     `json:"persistence"`
	Assurance              siteBonusExchangeAssurance       `json:"assurance"`
	Blockers               []siteDetailFinding              `json:"blockers"`
	Warnings               []string                         `json:"warnings"`
}

type siteBonusExchangeAcknowledgement struct {
	Provided bool   `json:"provided"`
	Scope    string `json:"scope"`
}

type siteBonusExchangeScope struct {
	SiteID           string `json:"site_id"`
	Selector         string `json:"selector"`
	ExpectedReviewID string `json:"expected_review_id"`
	Origin           string `json:"origin"`
	ReviewRouteID    string `json:"review_route_id"`
	ActionRouteID    string `json:"action_route_id"`
}

type siteBonusExchangeOperation struct {
	Status          string               `json:"status"`
	Complete        bool                 `json:"complete"`
	OperationID     string               `json:"operation_id,omitempty"`
	IntentRecord    *metastore.RecordRef `json:"intent_record,omitempty"`
	AttemptRecord   *metastore.RecordRef `json:"attempt_record,omitempty"`
	OutcomeRecord   *metastore.RecordRef `json:"outcome_record,omitempty"`
	RetentionRecord *metastore.RecordRef `json:"retention_record,omitempty"`
	ForgetRecord    *metastore.RecordRef `json:"forget_record,omitempty"`
	StopReason      string               `json:"stop_reason,omitempty"`
}

type siteBonusExchangeRequest struct {
	Status       string                    `json:"status"`
	RequestsMade int                       `json:"requests_made"`
	FreshReview  site.BonusReviewReceipt   `json:"fresh_review"`
	Submission   site.BonusExchangeReceipt `json:"submission"`
}

type siteBonusExchangePersistence struct {
	Status                string              `json:"status"`
	Store                 metastore.StoreInfo `json:"store"`
	IntentWrites          int                 `json:"intent_writes"`
	AttemptWrites         int                 `json:"attempt_writes"`
	OutcomeWrites         int                 `json:"outcome_writes"`
	AttemptRecordVerified bool                `json:"attempt_record_verified"`
	OutcomeRecordVerified bool                `json:"outcome_record_verified"`
	OutcomeDurable        bool                `json:"outcome_durable"`
	RetentionWrites       int                 `json:"retention_writes"`
	ForgetWrites          int                 `json:"forget_writes"`
	RecordsRemoved        int                 `json:"records_removed"`
	RecordsAlreadyAbsent  int                 `json:"records_already_absent"`
	RemovalDurable        bool                `json:"removal_durable"`
}

type siteBonusExchangeAssurance struct {
	FreshReviewReproduced        bool   `json:"fresh_review_reproduced"`
	ProcessLocalReviewAuthority  bool   `json:"process_local_review_authority"`
	AttemptMarkerDurable         bool   `json:"attempt_marker_durable"`
	ProcessLocalOutcomeAuthority bool   `json:"process_local_outcome_authority"`
	AttemptMarkerBlocksFuture    bool   `json:"attempt_marker_blocks_future_submissions"`
	SubmissionRequestBound       bool   `json:"submission_request_bound_verified"`
	AtMostOnceScope              string `json:"at_most_once_scope"`
	NoAutomaticRetry             bool   `json:"no_automatic_retry"`
	SerializedAuthority          bool   `json:"serialized_authority"`
	TerminalChainVerified        bool   `json:"terminal_chain_verified"`
	NonExecutableTombstone       bool   `json:"non_executable_tombstone"`
	NoNetworkAccess              bool   `json:"no_network_access"`
	HistoricalEvidenceErased     bool   `json:"historical_evidence_erased"`
}

type siteBonusExchangeListReport struct {
	kind            string
	Outcome         string                           `json:"outcome"`
	Effect          []string                         `json:"effect"`
	WritesPerformed int                              `json:"writes_performed"`
	Complete        bool                             `json:"complete"`
	Store           metastore.StoreInfo              `json:"store"`
	Limits          bonusexchange.Limits             `json:"limits"`
	Used            bonusexchange.OperationListUsage `json:"used"`
	Operations      []bonusexchange.OperationSummary `json:"operations"`
	StopReason      string                           `json:"stop_reason,omitempty"`
	Blockers        []siteDetailFinding              `json:"blockers"`
	Warnings        []string                         `json:"warnings"`
}

func (a *app) siteBonusExchange(args []string) error {
	if len(args) == 0 {
		return usageError("site bonus exchange requires prepare, submit, status, list, prune, or forget")
	}
	switch args[0] {
	case "prepare":
		return a.siteBonusExchangePrepare(args[1:])
	case "submit":
		return a.siteBonusExchangeSubmit(args[1:])
	case "status":
		return a.siteBonusExchangeStatus(args[1:])
	case "list":
		return a.siteBonusExchangeList(args[1:])
	case "prune":
		return a.siteBonusExchangePrune(args[1:])
	case "forget":
		return a.siteBonusExchangeForget(args[1:])
	case "-h", "--help", "help":
		a.siteBonusExchangeHelp()
		return nil
	default:
		return usageError("site bonus exchange requires prepare, submit, status, list, prune, or forget")
	}
}

func (a *app) siteBonusExchangeList(args []string) error {
	fs := newFlagSet("site bonus exchange list")
	output := fs.String("output", "table", "table or json")
	storeRoot := fs.String("state-store", "", "initialized private state store")
	timeout := fs.Duration("timeout", bonusExchangeDefaultTimeout, "local operation inventory timeout")
	defaults := bonusexchange.DefaultLimits()
	maxOperations := fs.Int("max-operations", defaults.MaxStatusRecords, "maximum live or retained operations returned")
	maxStateEntries := fs.Int("max-state-entries", defaults.MaxStatusEntries, "maximum private-store entries per inventory pass")
	maxStatePathBytes := fs.Int64("max-state-path-bytes", defaults.MaxStatusPathBytes, "maximum private-store path bytes per inventory pass")
	maxStateBytes := fs.Int64("max-state-bytes", defaults.MaxStatusBytes, "maximum aggregate bytes read per live or historical axis")
	if err := fs.Parse(args); err != nil {
		return usageError("site bonus exchange list: %v", err)
	}
	if fs.NArg() != 0 || *storeRoot == "" {
		return usageError("site bonus exchange list requires --state-store")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if err := validateBonusExchangeTimeout(*timeout); err != nil {
		return err
	}
	limits := defaults
	limits.MaxStatusRecords = *maxOperations
	limits.MaxStatusEntries = *maxStateEntries
	limits.MaxStatusPathBytes = *maxStatePathBytes
	limits.MaxStatusBytes = *maxStateBytes
	if err := limits.Validate(); err != nil {
		return usageError("site bonus exchange list limits are invalid")
	}
	report := newSiteBonusExchangeListReport(limits)
	store, err := metastore.Open(*storeRoot)
	if err != nil {
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: "state.store_open_failed", Message: "the private state store could not be opened"})
		return a.finishSiteBonusExchangeList(*output, report, bonusExchangePublicStateError(err, "open private state store failed"))
	}
	report.Store = store.Info()
	repository, err := bonusexchange.NewRepository(store, limits)
	if err != nil {
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: "state.repository_unavailable", Message: "the bonus exchange state repository is unavailable"})
		return a.finishSiteBonusExchangeList(*output, report, bonusExchangePublicStateError(err, "open bonus exchange repository failed"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	session, err := repository.OpenSession(ctx)
	if err != nil {
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: "state.session_open_failed", Message: "the bound state session could not be opened"})
		return a.finishSiteBonusExchangeList(*output, report, bonusExchangePublicStateError(err, "open bonus exchange state session failed"))
	}
	listed, listErr := session.ListOperations(ctx)
	report.Complete = listed.Complete
	report.Store = listed.Store
	report.Used = listed.Used
	report.Operations = listed.Operations
	report.StopReason = listed.StopReason
	closeErr := session.Close()
	if listErr != nil {
		report.Outcome = "incomplete"
		code := safeBonusExchangeListStop(listed.StopReason, listErr)
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: code, Message: "the bounded operation inventory could not be completely verified"})
		if errors.Is(listErr, bonusexchange.ErrStatusIncomplete) {
			return a.finishSiteBonusExchangeList(*output, report, &inconclusiveErr{message: "bonus exchange operation inventory is incomplete; see report"})
		}
		if errors.Is(listErr, bonusexchange.ErrCorruptExchange) || errors.Is(listErr, metastore.ErrCorruptRecord) {
			report.Outcome = "integrity_failed"
		}
		return a.finishSiteBonusExchangeList(*output, report, bonusExchangePublicStateError(listErr, "list bonus exchange operations failed"))
	}
	if closeErr != nil {
		report.Outcome = "incomplete"
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: "state.session_close_failed", Message: "the bound state session could not be closed cleanly"})
		return a.finishSiteBonusExchangeList(*output, report, fmt.Errorf("close bonus exchange state session failed"))
	}
	report.Outcome = "complete"
	return a.writeSiteBonusExchangeListReport(*output, report)
}

func (a *app) siteBonusExchangePrepare(args []string) error {
	fs := newFlagSet("site bonus exchange prepare")
	output := fs.String("output", "table", "table or json")
	storeRoot := fs.String("state-store", "", "initialized private state store")
	expectedReview := fs.String("expect-review-id", "", "exact review ID approved by the user")
	timeout := fs.Duration("timeout", bonusExchangeDefaultTimeout, "local preparation timeout")
	if err := fs.Parse(args); err != nil {
		return usageError("site bonus exchange prepare: %v", err)
	}
	if fs.NArg() != 2 || fs.Arg(0) == "" || fs.Arg(1) == "" || *storeRoot == "" || *expectedReview == "" {
		return usageError("site bonus exchange prepare requires --state-store, --expect-review-id, SITE, and OPTION")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if err := validateBonusExchangeTimeout(*timeout); err != nil {
		return err
	}
	if err := site.ValidateBonusReviewID(*expectedReview); err != nil {
		return usageError("--expect-review-id must be a canonical bonus review ID")
	}
	descriptor, _, config, err := a.bonusExchangeWriter(fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	report := newSiteBonusExchangeReport("site.bonus.exchange.prepare", descriptor.ID, fs.Arg(1), *expectedReview, config)
	report.Effect = []string{bonusexchange.PrepareEffect}
	report.Acknowledgement.Scope = "not_required_for_local_intent"
	report.Request.Status = "not_requested_local_prepare"

	store, err := metastore.Open(*storeRoot)
	if err != nil {
		addBonusExchangeBlocker(&report, "state.store_open_failed", "the private state store could not be opened")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "open private state store failed"))
	}
	report.Persistence.Store = store.Info()
	repository, err := bonusexchange.NewRepository(store, bonusexchange.DefaultLimits())
	if err != nil {
		addBonusExchangeBlocker(&report, "state.repository_unavailable", "the bonus exchange state repository is unavailable")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "open bonus exchange repository failed"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	intent, receipt, err := repository.Prepare(ctx, bonusexchange.PrepareInput{
		SiteID: descriptor.ID, Selector: fs.Arg(1), ExpectedReviewID: *expectedReview, Config: config,
		ReviewLimits: site.DefaultBonusReviewLimits(), ExchangeLimits: site.DefaultBonusExchangeLimits(),
	})
	report.WritesPerformed = receipt.WritesPerformed
	report.Persistence.IntentWrites = receipt.WritesPerformed
	if receipt.Record.ID != "" {
		copyRef := receipt.Record
		report.Operation.IntentRecord = &copyRef
	}
	if intent.OperationID != "" {
		report.Operation.OperationID = intent.OperationID.String()
	}
	if err != nil {
		report.Operation.Status = "preparation_incomplete"
		addBonusExchangeBlocker(&report, "state.intent_publication_failed", "the reviewed intent was not durably published")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "publish bonus exchange intent failed"))
	}
	report.Outcome = "prepared"
	report.Operation.Status = bonusexchange.StatePrepared
	report.Operation.Complete = true
	report.Persistence.Status = "intent_durable"
	return a.writeSiteBonusExchangeReport(*output, report)
}

func (a *app) siteBonusExchangeStatus(args []string) error {
	fs := newFlagSet("site bonus exchange status")
	output := fs.String("output", "table", "table or json")
	storeRoot := fs.String("state-store", "", "initialized private state store")
	intentValue := fs.String("intent-record", "", "sealed intent record ID")
	timeout := fs.Duration("timeout", bonusExchangeDefaultTimeout, "local status timeout")
	if err := fs.Parse(args); err != nil {
		return usageError("site bonus exchange status: %v", err)
	}
	if fs.NArg() != 0 || *storeRoot == "" || *intentValue == "" {
		return usageError("site bonus exchange status requires --state-store and --intent-record")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if err := validateBonusExchangeTimeout(*timeout); err != nil {
		return err
	}
	intentID, err := metastore.ParseRecordID(*intentValue)
	if err != nil {
		return usageError("--intent-record must be a canonical sealed record ID")
	}
	report := newSiteBonusExchangeReport("site.bonus.exchange.status", "unknown", "unknown", "", site.BonusExchangeConfig{})
	report.Effect = []string{bonusexchange.StatusEffect}
	report.Acknowledgement.Scope = "not_required_for_read_only_status"
	report.Operation.Status = "inspection_incomplete"
	report.Request.Status = "not_requested_read_only_status"
	copyRef := metastore.RecordRef{Kind: metastore.RecordKindSiteBonusExchangeIntentV1, ID: intentID}
	report.Operation.IntentRecord = &copyRef

	store, err := metastore.Open(*storeRoot)
	if err != nil {
		addBonusExchangeBlocker(&report, "state.store_open_failed", "the private state store could not be opened")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "open private state store failed"))
	}
	report.Persistence.Store = store.Info()
	repository, err := bonusexchange.NewRepository(store, bonusexchange.DefaultLimits())
	if err != nil {
		addBonusExchangeBlocker(&report, "state.repository_unavailable", "the bonus exchange state repository is unavailable")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "open bonus exchange repository failed"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	session, err := repository.OpenSession(ctx)
	if err != nil {
		addBonusExchangeBlocker(&report, "state.session_open_failed", "the bound state session could not be opened")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "open bonus exchange state session failed"))
	}
	status, statusErr := session.Status(ctx, intentID)
	applyBonusExchangeStatus(&report, status)
	closeErr := session.Close()
	if statusErr != nil {
		addBonusExchangeBlocker(&report, safeBonusExchangeStateStop(status.StopReason, statusErr), "the durable operation state could not be completely verified")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(statusErr, "read bonus exchange status failed"))
	}
	report.Outcome = status.State
	if closeErr != nil {
		addBonusExchangeBlocker(&report, "state.session_close_failed", "the bound state session could not be closed cleanly")
		return a.finishSiteBonusExchange(*output, report, fmt.Errorf("close bonus exchange state session failed"))
	}
	return a.writeSiteBonusExchangeReport(*output, report)
}

func (a *app) siteBonusExchangePrune(args []string) error {
	fs := newFlagSet("site bonus exchange prune")
	output := fs.String("output", "table", "table or json")
	storeRoot := fs.String("state-store", "", "initialized private state store")
	intentValue := fs.String("intent-record", "", "sealed intent record ID")
	operationValue := fs.String("operation-id", "", "full bonus exchange operation ID")
	acknowledge := fs.Bool("acknowledge-state-prune", false, "acknowledge deletion of this terminal executable state")
	timeout := fs.Duration("timeout", bonusExchangeDefaultTimeout, "local retention timeout")
	if err := fs.Parse(args); err != nil {
		return usageError("site bonus exchange prune: %v", err)
	}
	if fs.NArg() != 0 || *storeRoot == "" || *intentValue == "" || *operationValue == "" || !*acknowledge {
		return usageError("site bonus exchange prune requires --state-store, --intent-record, --operation-id, and --acknowledge-state-prune")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if err := validateBonusExchangeTimeout(*timeout); err != nil {
		return err
	}
	intentID, err := metastore.ParseRecordID(*intentValue)
	if err != nil {
		return usageError("--intent-record must be a canonical sealed record ID")
	}
	operationID, err := bonusexchange.ParseOperationID(*operationValue)
	if err != nil {
		return usageError("--operation-id must be a canonical bonus exchange operation ID")
	}
	report := newSiteBonusExchangeRetentionReport("site.bonus.exchange.prune", bonusexchange.PruneEffect)
	report.Acknowledgement = siteBonusExchangeAcknowledgement{Provided: true, Scope: "delete_exact_terminal_intent_attempt_outcome_and_retain_tombstone"}
	report.Operation.OperationID = operationID.String()
	intentRef := metastore.RecordRef{Kind: metastore.RecordKindSiteBonusExchangeIntentV1, ID: intentID}
	report.Operation.IntentRecord = &intentRef

	store, err := metastore.Open(*storeRoot)
	if err != nil {
		addBonusExchangeBlocker(&report, "state.store_open_failed", "the private state store could not be opened")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "open private state store failed"))
	}
	report.Persistence.Store = store.Info()
	repository, err := bonusexchange.NewRepository(store, bonusexchange.DefaultLimits())
	if err != nil {
		addBonusExchangeBlocker(&report, "state.repository_unavailable", "the bonus exchange state repository is unavailable")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "open bonus exchange repository failed"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	session, err := repository.OpenSession(ctx)
	if err != nil {
		addBonusExchangeBlocker(&report, "state.session_open_failed", "the bound state session could not be opened")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "open bonus exchange state session failed"))
	}
	retained, pruneErr := session.Prune(ctx, intentID, operationID)
	applyBonusExchangeRetentionReceipt(&report, retained)
	closeErr := session.Close()
	if pruneErr != nil {
		code := safeBonusExchangeRetentionStop(retained.StopReason, pruneErr)
		addBonusExchangeBlocker(&report, code, "the selected terminal operation could not be safely replaced by its tombstone")
		switch {
		case errors.Is(pruneErr, bonusexchange.ErrCorruptExchange), errors.Is(pruneErr, metastore.ErrCorruptRecord):
			report.Outcome = "integrity_failed"
			return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(pruneErr, "prune bonus exchange state failed"))
		case errors.Is(pruneErr, bonusexchange.ErrRetentionPolicy), errors.Is(pruneErr, bonusexchange.ErrInvalidExchange):
			report.Outcome = "blocked"
			return a.finishSiteBonusExchange(*output, report, &inconclusiveErr{message: "bonus exchange state pruning was blocked; see report"})
		case errors.Is(pruneErr, bonusexchange.ErrExchangeNotFound):
			report.Outcome = "not_found"
			return a.finishSiteBonusExchange(*output, report, fmt.Errorf("bonus exchange operation was not found"))
		default:
			report.Outcome = "incomplete"
			return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(pruneErr, "prune bonus exchange state failed"))
		}
	}
	if closeErr != nil {
		report.Outcome = "incomplete"
		addBonusExchangeBlocker(&report, "state.session_close_failed", "the bound state session could not be closed cleanly")
		return a.finishSiteBonusExchange(*output, report, fmt.Errorf("close bonus exchange state session failed"))
	}
	report.Outcome = "pruned"
	if retained.RetentionWrites == 0 && retained.RecordsRemoved == 0 && retained.WritesPerformed == 0 {
		report.Outcome = "already_pruned"
	}
	return a.writeSiteBonusExchangeReport(*output, report)
}

func (a *app) siteBonusExchangeForget(args []string) error {
	fs := newFlagSet("site bonus exchange forget")
	output := fs.String("output", "table", "table or json")
	storeRoot := fs.String("state-store", "", "initialized private state store")
	retentionValue := fs.String("retention-record", "", "sealed retention record ID")
	operationValue := fs.String("operation-id", "", "full bonus exchange operation ID")
	acknowledge := fs.Bool("acknowledge-history-forget", false, "acknowledge irreversible deletion of this tombstone")
	timeout := fs.Duration("timeout", bonusExchangeDefaultTimeout, "local historical-evidence deletion timeout")
	if err := fs.Parse(args); err != nil {
		return usageError("site bonus exchange forget: %v", err)
	}
	if fs.NArg() != 0 || *storeRoot == "" || *retentionValue == "" || *operationValue == "" || !*acknowledge {
		return usageError("site bonus exchange forget requires --state-store, --retention-record, --operation-id, and --acknowledge-history-forget")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if err := validateBonusExchangeTimeout(*timeout); err != nil {
		return err
	}
	retentionID, err := metastore.ParseRecordID(*retentionValue)
	if err != nil {
		return usageError("--retention-record must be a canonical sealed record ID")
	}
	operationID, err := bonusexchange.ParseOperationID(*operationValue)
	if err != nil {
		return usageError("--operation-id must be a canonical bonus exchange operation ID")
	}
	report := newSiteBonusExchangeRetentionReport("site.bonus.exchange.forget", bonusexchange.ForgetEffect)
	report.Acknowledgement = siteBonusExchangeAcknowledgement{Provided: true, Scope: "irreversibly_delete_exact_bonus_exchange_tombstone"}
	report.Operation.OperationID = operationID.String()
	retentionRef := metastore.RecordRef{Kind: metastore.RecordKindSiteBonusExchangeRetentionV1, ID: retentionID}
	report.Operation.RetentionRecord = &retentionRef

	store, err := metastore.Open(*storeRoot)
	if err != nil {
		addBonusExchangeBlocker(&report, "state.store_open_failed", "the private state store could not be opened")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "open private state store failed"))
	}
	report.Persistence.Store = store.Info()
	repository, err := bonusexchange.NewRepository(store, bonusexchange.DefaultLimits())
	if err != nil {
		addBonusExchangeBlocker(&report, "state.repository_unavailable", "the bonus exchange state repository is unavailable")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "open bonus exchange repository failed"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	session, err := repository.OpenSession(ctx)
	if err != nil {
		addBonusExchangeBlocker(&report, "state.session_open_failed", "the bound state session could not be opened")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "open bonus exchange state session failed"))
	}
	forgotten, forgetErr := session.Forget(ctx, retentionID, operationID)
	applyBonusExchangeForgetReceipt(&report, forgotten)
	closeErr := session.Close()
	if forgetErr != nil {
		code := safeBonusExchangeRetentionStop(forgotten.StopReason, forgetErr)
		addBonusExchangeBlocker(&report, code, "the selected tombstone could not be safely and completely erased")
		switch {
		case errors.Is(forgetErr, bonusexchange.ErrCorruptExchange), errors.Is(forgetErr, metastore.ErrCorruptRecord):
			report.Outcome = "integrity_failed"
			return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(forgetErr, "forget bonus exchange history failed"))
		case errors.Is(forgetErr, bonusexchange.ErrRetentionPolicy), errors.Is(forgetErr, bonusexchange.ErrInvalidExchange):
			report.Outcome = "blocked"
			return a.finishSiteBonusExchange(*output, report, &inconclusiveErr{message: "bonus exchange history deletion was blocked; see report"})
		case errors.Is(forgetErr, bonusexchange.ErrExchangeNotFound):
			report.Outcome = "unattributed_absence"
			return a.finishSiteBonusExchange(*output, report, fmt.Errorf("bonus exchange retention evidence was not found"))
		default:
			report.Outcome = "incomplete"
			return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(forgetErr, "forget bonus exchange history failed"))
		}
	}
	if closeErr != nil {
		report.Outcome = "incomplete"
		addBonusExchangeBlocker(&report, "state.session_close_failed", "the bound state session could not be closed cleanly")
		return a.finishSiteBonusExchange(*output, report, fmt.Errorf("close bonus exchange state session failed"))
	}
	report.Outcome = "forgotten"
	return a.writeSiteBonusExchangeReport(*output, report)
}

func (a *app) siteBonusExchangeSubmit(args []string) error {
	fs := newFlagSet("site bonus exchange submit")
	output := fs.String("output", "table", "table or json")
	storeRoot := fs.String("state-store", "", "initialized private state store")
	intentValue := fs.String("intent-record", "", "sealed intent record ID")
	expectedReview := fs.String("expect-review-id", "", "exact review ID approved by the user")
	cookieStdin := fs.Bool("cookie-stdin", false, "read one Cookie header value from stdin")
	acknowledge := fs.Bool("acknowledge-bonus-exchange", false, "acknowledge one bonus exchange form submission")
	timeout := fs.Duration("timeout", bonusExchangeDefaultTimeout, "fresh review and submission timeout")
	if err := fs.Parse(args); err != nil {
		return usageError("site bonus exchange submit: %v", err)
	}
	if fs.NArg() != 2 || fs.Arg(0) == "" || fs.Arg(1) == "" || *storeRoot == "" || *intentValue == "" || *expectedReview == "" {
		return usageError("site bonus exchange submit requires state store, intent record, expected review ID, SITE, and OPTION")
	}
	if !*cookieStdin || !*acknowledge {
		return usageError("site bonus exchange submit requires --cookie-stdin and --acknowledge-bonus-exchange")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if err := validateBonusExchangeTimeout(*timeout); err != nil {
		return err
	}
	intentID, err := metastore.ParseRecordID(*intentValue)
	if err != nil {
		return usageError("--intent-record must be a canonical sealed record ID")
	}
	if err := site.ValidateBonusReviewID(*expectedReview); err != nil {
		return usageError("--expect-review-id must be a canonical bonus review ID")
	}
	descriptor, writer, config, err := a.bonusExchangeWriter(fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	report := newSiteBonusExchangeReport("site.bonus.exchange.submit", descriptor.ID, fs.Arg(1), *expectedReview, config)
	report.Effect = []string{site.BonusReviewReadEffect, bonusexchange.ReserveEffect, site.BonusExchangeSubmitEffect, bonusexchange.OutcomeEffect}
	report.Acknowledgement = siteBonusExchangeAcknowledgement{Provided: true, Scope: "one_fresh_review_then_at_most_one_form_submission"}

	store, err := metastore.Open(*storeRoot)
	if err != nil {
		addBonusExchangeBlocker(&report, "state.store_open_failed", "the private state store could not be opened")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "open private state store failed"))
	}
	report.Persistence.Store = store.Info()
	repository, err := bonusexchange.NewRepository(store, bonusexchange.DefaultLimits())
	if err != nil {
		addBonusExchangeBlocker(&report, "state.repository_unavailable", "the bonus exchange state repository is unavailable")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "open bonus exchange repository failed"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	stateSession, err := repository.OpenSession(ctx)
	if err != nil {
		addBonusExchangeBlocker(&report, "state.session_open_failed", "the bound state session could not be opened")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "open bonus exchange state session failed"))
	}
	verifiedIntent, _, err := stateSession.LoadIntent(ctx, intentID)
	if err != nil {
		_ = stateSession.Close()
		addBonusExchangeBlocker(&report, "state.intent_invalid", "the selected intent could not be verified")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "load bonus exchange intent failed"))
	}
	intent, intentRef, _ := verifiedIntent.PublicCopy()
	if !bonusExchangeIntentMatchesInvocation(intent, descriptor.ID, fs.Arg(1), *expectedReview, config) {
		_ = stateSession.Close()
		addBonusExchangeBlocker(&report, "state.intent_selector_mismatch", "the sealed intent does not match the explicit invocation selectors")
		return a.finishSiteBonusExchange(*output, report, fmt.Errorf("bonus exchange intent does not match the invocation"))
	}
	report.Operation.OperationID = intent.OperationID.String()
	copyIntentRef := intentRef
	report.Operation.IntentRecord = &copyIntentRef
	preflight, err := stateSession.Status(ctx, intentID)
	applyBonusExchangeStatus(&report, preflight)
	if err != nil || !preflight.Complete {
		_ = stateSession.Close()
		addBonusExchangeBlocker(&report, safeBonusExchangeStateStop(preflight.StopReason, err), "the operation state could not be verified before credential access")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(err, "preflight bonus exchange state failed"))
	}
	if preflight.State != bonusexchange.StatePrepared {
		_ = stateSession.Close()
		addBonusExchangeBlocker(&report, "state.submission_already_reserved", "the intent already has a durable attempt marker and will not be submitted again")
		return a.finishSiteBonusExchange(*output, report, fmt.Errorf("bonus exchange submission is already reserved"))
	}

	credential, err := readEffectfulSiteCredential(a.stdin)
	if err != nil {
		_ = stateSession.Close()
		addBonusExchangeBlocker(&report, "credential.read_failed", "the site credential could not be read")
		return a.finishSiteBonusExchange(*output, report, fmt.Errorf("read site credential failed"))
	}
	siteSession, err := writer.OpenBonusExchangeSession(ctx, credential)
	if err != nil || siteSession == nil {
		_ = stateSession.Close()
		addBonusExchangeBlocker(&report, "site.session_open_failed", "the bounded bonus exchange session could not be opened")
		return a.finishSiteBonusExchange(*output, report, fmt.Errorf("open site bonus exchange session failed"))
	}

	reviewLimits := intent.ReviewLimits
	observedReview, reviewReceipt, reviewErr := siteSession.ReadFreshBonusReview(ctx, intent.Selector, reviewLimits, intent.ExchangeLimits)
	report.Request.RequestsMade = boundedFetchCounter(siteSession.RequestsMade(), intent.ExchangeLimits.MaxSessionRequests+1)
	report.Request.FreshReview = publicBonusReviewReceipt(intent.SiteID, intent.Selector, site.BonusReviewConfig{Origin: config.Origin, RouteID: config.ReviewRouteID}, reviewLimits, reviewReceipt)
	if reviewErr != nil || validateFreshBonusExchangeReview(intent, observedReview, reviewReceipt, report.Request.RequestsMade) != nil {
		_ = siteSession.Close()
		_ = stateSession.Close()
		report.Request.Status = "fresh_review_incomplete"
		addBonusExchangeBlocker(&report, "site.fresh_review_failed", "the approved review was not reproduced from a fresh site response")
		return a.finishSiteBonusExchange(*output, report, siteBonusReviewPublicError(ctx, reviewErr))
	}
	report.Request.Status = "fresh_review_complete"
	report.Assurance.FreshReviewReproduced = true
	report.Assurance.ProcessLocalReviewAuthority = true

	_, reserved, reserveReceipt, reserveErr := stateSession.ReserveAttempt(ctx, verifiedIntent, observedReview, reviewReceipt)
	report.WritesPerformed += reserveReceipt.WritesPerformed
	report.Persistence.AttemptWrites = reserveReceipt.WritesPerformed
	if reserveReceipt.Record.ID != "" {
		copyAttemptRef := reserveReceipt.Record
		report.Operation.AttemptRecord = &copyAttemptRef
	}
	if reserveErr != nil || reserved == nil || !reserveReceipt.Acquired {
		_ = siteSession.Close()
		finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), bonusExchangeFinalizeTimeout)
		postStatus, _ := stateSession.Status(finalizeCtx, intentID)
		finalizeCancel()
		applyBonusExchangeStatus(&report, postStatus)
		_ = stateSession.Close()
		addBonusExchangeBlocker(&report, "state.attempt_reservation_failed", "the at-most-once marker was not newly and durably acquired")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(reserveErr, "reserve bonus exchange attempt failed"))
	}
	report.Assurance.AttemptMarkerDurable = true
	report.Assurance.AttemptMarkerBlocksFuture = true
	report.Persistence.Status = "attempt_reserved"

	observedExchange, exchangeReceipt, submitErr := siteSession.SubmitFreshBonusExchange(ctx, observedReview, intent.ExpectedReviewID, intent.ExchangeLimits)
	report.Request.RequestsMade = boundedFetchCounter(siteSession.RequestsMade(), intent.ExchangeLimits.MaxSessionRequests+1)
	validObservation := validateBonusExchangeObservation(intent, observedExchange, exchangeReceipt, report.Request.RequestsMade) == nil
	if validObservation {
		report.Request.Submission = publicBonusExchangeReceipt(intent, exchangeReceipt)
		report.FormSubmissionAttempts = boundedFetchCounter(exchangeReceipt.Used.SubmissionRequestsAttempted, intent.ExchangeLimits.MaxSubmissionRequests+1)
		report.Request.Status = "complete"
		report.Assurance.ProcessLocalOutcomeAuthority = true
		report.Assurance.SubmissionRequestBound = true
	} else {
		report.Request.Submission = invalidBonusExchangeReceipt(intent, report.Request.RequestsMade)
		report.FormSubmissionAttempts = report.Request.Submission.Used.SubmissionRequestsAttempted
		report.Request.Status = "submission_receipt_invalid"
		report.Assurance.NoAutomaticRetry = false
	}

	var storeErr error
	if validObservation {
		finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), bonusExchangeFinalizeTimeout)
		_, outcomeReceipt, outcomeErr := stateSession.StoreOutcome(finalizeCtx, verifiedIntent, reserved, observedExchange, exchangeReceipt)
		report.WritesPerformed += outcomeReceipt.WritesPerformed
		report.Persistence.OutcomeWrites = outcomeReceipt.WritesPerformed
		if outcomeReceipt.Record.ID != "" {
			copyOutcomeRef := outcomeReceipt.Record
			report.Operation.OutcomeRecord = &copyOutcomeRef
		}
		storeErr = outcomeErr
		if outcomeErr == nil && outcomeReceipt.DurabilityConfirmed {
			report.Persistence.OutcomeDurable = true
			report.Persistence.Status = "outcome_durable"
		} else if outcomeErr == nil {
			storeErr = fmt.Errorf("bonus exchange outcome publication durability is unconfirmed")
		}
		finalizeCancel()
	} else {
		storeErr = fmt.Errorf("bonus exchange response authority is invalid")
	}
	closeSiteErr := siteSession.Close()
	finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), bonusExchangeFinalizeTimeout)
	postStatus, statusErr := stateSession.Status(finalizeCtx, intentID)
	applyBonusExchangeStatus(&report, postStatus)
	checkErr := stateSession.Check(finalizeCtx)
	closeStateErr := stateSession.Close()
	finalizeCancel()

	if storeErr != nil || statusErr != nil || checkErr != nil {
		report.Outcome = "submission_outcome_not_durable"
		report.Persistence.OutcomeDurable = false
		report.Persistence.Status = "outcome_publication_unverified"
		if statusErr != nil || checkErr != nil {
			// The current invocation still retains its historical publication
			// receipt, but it can no longer assert that the presently selected
			// store history contains the marker that future callers will see.
			report.Assurance.AttemptMarkerBlocksFuture = false
		}
		addBonusExchangeBlocker(&report, "state.outcome_persistence_failed", "the form result could not be durably linked to the attempt; the submission must not be retried")
		return a.finishSiteBonusExchange(*output, report, bonusExchangePublicStateError(firstBonusExchangeError(storeErr, statusErr, checkErr), "persist bonus exchange outcome failed"))
	}
	report.Request.Status = "complete"
	report.Persistence.OutcomeDurable = true
	report.Outcome = postStatus.State
	if closeSiteErr != nil || closeStateErr != nil {
		addBonusExchangeBlocker(&report, "session.close_failed", "a bounded session could not be closed cleanly")
		return a.finishSiteBonusExchange(*output, report, fmt.Errorf("close bonus exchange session failed"))
	}
	if submitErr != nil || postStatus.State != bonusexchange.StateConfirmed {
		addBonusExchangeBlocker(&report, "site.exchange_not_confirmed", "the durable outcome does not confirm that the exchange succeeded")
		return a.finishSiteBonusExchange(*output, report, bonusExchangeSubmissionPublicError(ctx, submitErr, postStatus.State))
	}
	return a.writeSiteBonusExchangeReport(*output, report)
}

func (a *app) bonusExchangeWriter(siteID, selector string) (domain.SiteDescriptor, site.BonusExchangeWriter, site.BonusExchangeConfig, error) {
	adapter, ok := a.registry.Get(siteID)
	if !ok {
		return domain.SiteDescriptor{}, nil, site.BonusExchangeConfig{}, usageError("unknown site %q", siteID)
	}
	descriptor := adapter.Descriptor()
	if !descriptor.Supports(domain.CapabilityBonusExchange) || !descriptor.SupportsAuth(domain.AuthMethodCookieHeader) {
		return descriptor, nil, site.BonusExchangeConfig{}, usageError("site %q does not support acknowledged bonus exchange", descriptor.ID)
	}
	writer, ok := adapter.(site.BonusExchangeWriter)
	if !ok {
		return descriptor, nil, site.BonusExchangeConfig{}, usageError("site %q declares bonus exchange without its typed port", descriptor.ID)
	}
	config, err := writer.BonusExchangeConfig()
	if err != nil || config.Validate() != nil {
		return descriptor, nil, site.BonusExchangeConfig{}, usageError("site bonus exchange configuration is unavailable")
	}
	descriptorOrigin, err := canonicalSiteDescriptorOrigin(descriptor.BaseURL)
	if err != nil || descriptorOrigin != config.Origin {
		return descriptor, nil, site.BonusExchangeConfig{}, usageError("site bonus exchange origin is unsafe")
	}
	if err := writer.ValidateBonusExchangeSelector(selector); err != nil {
		return descriptor, nil, site.BonusExchangeConfig{}, usageError("site bonus option is invalid")
	}
	return descriptor, writer, config, nil
}

func newSiteBonusExchangeReport(kind, siteID, selector, reviewID string, config site.BonusExchangeConfig) siteBonusExchangeReport {
	return siteBonusExchangeReport{
		kind: kind, Outcome: "blocked", Effect: []string{},
		Scope:     siteBonusExchangeScope{SiteID: siteID, Selector: selector, ExpectedReviewID: reviewID, Origin: config.Origin, ReviewRouteID: config.ReviewRouteID, ActionRouteID: config.ActionRouteID},
		Operation: siteBonusExchangeOperation{Status: "not_started"},
		Request: siteBonusExchangeRequest{
			Status:      "not_started",
			FreshReview: site.BonusReviewReceipt{Effect: site.BonusReviewReadEffect, SiteID: siteID, Selector: selector, Origin: config.Origin, RouteID: config.ReviewRouteID, Limits: site.DefaultBonusReviewLimits()},
			Submission:  site.BonusExchangeReceipt{Effect: site.BonusExchangeSubmitEffect, SiteID: siteID, Selector: selector, ExpectedReviewID: reviewID, FreshReviewID: reviewID, Origin: config.Origin, ReviewRouteID: config.ReviewRouteID, ActionRouteID: config.ActionRouteID, Limits: site.DefaultBonusExchangeLimits()},
		},
		Persistence: siteBonusExchangePersistence{Status: "not_started"},
		Assurance: siteBonusExchangeAssurance{
			AtMostOnceScope:     "one_prepared_operation_in_one_uncloned_private_store_history",
			NoAutomaticRetry:    true,
			SerializedAuthority: false,
		},
		Blockers: []siteDetailFinding{},
		Warnings: []string{
			"the deterministic attempt marker is written before the form POST; an existing marker is never retried",
			"at-most-once coordination is per prepared operation within one preserved, uncloned private-store history; do not roll back, duplicate, or manually delete its records; terminal prune and forget require their explicit acknowledged commands",
			"confirmed means an allowlisted site-local redirect was observed, not a signed statement or atomic balance proof",
			"after a POST, a missing durable outcome remains submission-unknown and requires status inspection, never automatic retry",
		},
	}
}

func newSiteBonusExchangeListReport(limits bonusexchange.Limits) siteBonusExchangeListReport {
	return siteBonusExchangeListReport{
		kind: "site.bonus.exchange.operation_list", Outcome: "blocked", Effect: []string{bonusexchange.ListEffect},
		Limits: limits, Operations: []bonusexchange.OperationSummary{}, Blockers: []siteDetailFinding{},
		Warnings: []string{
			"the list verifies bounded live intents and historical retention/forget markers; the full attempt/outcome transition remains not_inspected until an explicit intent record is passed to status",
			"inventory order is deterministic by intent record ID and is never a newest-operation selector",
			"each live or historical inventory uses a detected-stable non-atomic observation of one preserved private-store history",
		},
	}
}

func newSiteBonusExchangeRetentionReport(kind, effect string) siteBonusExchangeReport {
	report := newSiteBonusExchangeReport(kind, "unknown", "unknown", "", site.BonusExchangeConfig{})
	report.Effect = []string{effect}
	report.Request.Status = "not_requested_local_retention"
	report.Acknowledgement.Scope = "required_explicit_local_deletion_boundary"
	report.Persistence.Status = "inspection_not_started"
	report.Assurance.NoNetworkAccess = true
	report.Assurance.AtMostOnceScope = "historical_local_state_only_no_submission_authority"
	report.Warnings = []string{
		"prune is allowed only for a complete terminal outcome and retains one deterministic non-executable tombstone",
		"prepared and submission-unknown operations are never pruned because their durable records are still required to prevent an unsafe retry",
		"forget is a separate irreversible acknowledgement; after its final marker is deleted, later absence cannot prove that forgetting previously succeeded",
		"these commands never read a cookie, contact the site, or submit a form",
	}
	return report
}

func applyBonusExchangeRetentionReceipt(report *siteBonusExchangeReport, receipt bonusexchange.RetentionReceipt) {
	if report == nil {
		return
	}
	report.WritesPerformed = receipt.WritesPerformed
	report.Operation.Status = receipt.State
	report.Operation.Complete = receipt.Complete
	report.Operation.OperationID = receipt.OperationID.String()
	report.Operation.StopReason = receipt.StopReason
	if receipt.IntentRecordID != "" {
		ref := metastore.RecordRef{Kind: metastore.RecordKindSiteBonusExchangeIntentV1, ID: receipt.IntentRecordID}
		report.Operation.IntentRecord = &ref
	}
	if receipt.RetentionRecord.ID != "" {
		ref := receipt.RetentionRecord
		report.Operation.RetentionRecord = &ref
	}
	report.Scope.SiteID = valueOrUnknown(receipt.SiteID)
	report.Scope.Selector = valueOrUnknown(receipt.Selector)
	report.Scope.ExpectedReviewID = receipt.ExpectedReviewID
	report.Persistence.Store = receipt.Store
	report.Persistence.RetentionWrites = receipt.RetentionWrites
	report.Persistence.RecordsRemoved = receipt.RecordsRemoved
	report.Persistence.RecordsAlreadyAbsent = receipt.RecordsAlreadyAbsent
	report.Persistence.RemovalDurable = receipt.DurabilityConfirmed
	report.Persistence.Status = "retention_incomplete"
	if receipt.Complete {
		report.Persistence.Status = "terminal_tombstone_durable"
	}
	report.Assurance.TerminalChainVerified = safeTerminalBonusExchangeState(receipt.TerminalState)
	report.Assurance.NonExecutableTombstone = receipt.Complete && receipt.RetentionRecord.ID != ""
}

func applyBonusExchangeForgetReceipt(report *siteBonusExchangeReport, receipt bonusexchange.ForgetReceipt) {
	if report == nil {
		return
	}
	report.WritesPerformed = receipt.WritesPerformed
	report.Operation.Status = receipt.State
	report.Operation.Complete = receipt.Complete
	report.Operation.OperationID = receipt.OperationID.String()
	report.Operation.StopReason = receipt.StopReason
	if receipt.RetentionRecord.ID != "" {
		ref := receipt.RetentionRecord
		report.Operation.RetentionRecord = &ref
	}
	if receipt.ForgetRecord.ID != "" {
		ref := receipt.ForgetRecord
		report.Operation.ForgetRecord = &ref
	}
	report.Scope.SiteID = valueOrUnknown(receipt.SiteID)
	report.Scope.Selector = valueOrUnknown(receipt.Selector)
	report.Scope.ExpectedReviewID = receipt.ExpectedReviewID
	report.Persistence.Store = receipt.Store
	report.Persistence.ForgetWrites = receipt.ForgetWrites
	report.Persistence.RecordsRemoved = receipt.RecordsRemoved
	report.Persistence.RecordsAlreadyAbsent = receipt.RecordsAlreadyAbsent
	report.Persistence.RemovalDurable = receipt.DurabilityConfirmed
	report.Persistence.Status = "forget_incomplete"
	if receipt.Complete {
		report.Persistence.Status = "historical_evidence_erased"
	}
	report.Assurance.TerminalChainVerified = safeTerminalBonusExchangeState(receipt.TerminalState)
	report.Assurance.HistoricalEvidenceErased = receipt.Complete && receipt.DurabilityConfirmed
}

func safeTerminalBonusExchangeState(state string) bool {
	switch state {
	case bonusexchange.StateConfirmed, bonusexchange.StateRejected, bonusexchange.StateUnknown, bonusexchange.StateNotSubmitted:
		return true
	default:
		return false
	}
}

func applyBonusExchangeStatus(report *siteBonusExchangeReport, status bonusexchange.Status) {
	if report == nil {
		return
	}
	report.Operation.Status = status.State
	report.Operation.Complete = status.Complete
	report.Operation.StopReason = safeBonusExchangeStateStop(status.StopReason, nil)
	if status.Intent.OperationID != "" {
		report.Operation.OperationID = status.Intent.OperationID.String()
		report.Scope.SiteID = status.Intent.SiteID
		report.Scope.Selector = status.Intent.Selector
		report.Scope.ExpectedReviewID = status.Intent.ExpectedReviewID
		report.Scope.Origin = status.Intent.Origin
		report.Scope.ReviewRouteID = status.Intent.ReviewRouteID
		report.Scope.ActionRouteID = status.Intent.ActionRouteID
		report.Request.FreshReview.SiteID = status.Intent.SiteID
		report.Request.FreshReview.Selector = status.Intent.Selector
		report.Request.FreshReview.Origin = status.Intent.Origin
		report.Request.FreshReview.RouteID = status.Intent.ReviewRouteID
		report.Request.FreshReview.Limits = status.Intent.ReviewLimits
		report.Request.Submission.SiteID = status.Intent.SiteID
		report.Request.Submission.Selector = status.Intent.Selector
		report.Request.Submission.ExpectedReviewID = status.Intent.ExpectedReviewID
		report.Request.Submission.FreshReviewID = status.Intent.ExpectedReviewID
		report.Request.Submission.Origin = status.Intent.Origin
		report.Request.Submission.ReviewRouteID = status.Intent.ReviewRouteID
		report.Request.Submission.ActionRouteID = status.Intent.ActionRouteID
		report.Request.Submission.Limits = status.Intent.ExchangeLimits
	}
	if status.IntentRef.ID != "" {
		copyRef := status.IntentRef
		report.Operation.IntentRecord = &copyRef
	}
	if status.AttemptRef != nil {
		copyRef := *status.AttemptRef
		report.Operation.AttemptRecord = &copyRef
		report.Persistence.AttemptRecordVerified = status.Complete
		if status.Complete {
			report.Assurance.AttemptMarkerBlocksFuture = true
		}
	}
	if status.OutcomeRef != nil {
		copyRef := *status.OutcomeRef
		report.Operation.OutcomeRecord = &copyRef
		report.Persistence.OutcomeRecordVerified = status.Complete
		if status.Complete {
			report.Assurance.SubmissionRequestBound = true
		}
	}
	if status.RetentionRef != nil {
		copyRef := *status.RetentionRef
		report.Operation.RetentionRecord = &copyRef
		report.Assurance.NonExecutableTombstone = status.Complete
		report.Assurance.TerminalChainVerified = status.Complete
	}
	if status.ForgetRef != nil {
		copyRef := *status.ForgetRef
		report.Operation.ForgetRecord = &copyRef
	}
	if status.Store.StoreID != "" {
		report.Persistence.Store = status.Store
	}
	if status.Complete && report.Persistence.Status != "outcome_durable" {
		report.Persistence.Status = "state_verified"
	}
}

func bonusExchangeIntentMatchesInvocation(intent bonusexchange.IntentRecord, siteID, selector, reviewID string, config site.BonusExchangeConfig) bool {
	return intent.Validate() == nil && intent.SiteID == siteID && intent.Selector == selector && intent.ExpectedReviewID == reviewID &&
		intent.Origin == config.Origin && intent.ReviewRouteID == config.ReviewRouteID && intent.ActionRouteID == config.ActionRouteID &&
		intent.ReviewLimits == site.DefaultBonusReviewLimits() && intent.ExchangeLimits == site.DefaultBonusExchangeLimits() &&
		intent.InputMode == site.BonusReviewInputNone
}

func validateFreshBonusExchangeReview(intent bonusexchange.IntentRecord, observed *site.ObservedBonusReview, receipt site.BonusReviewReceipt, requests int) error {
	config := site.BonusReviewConfig{Origin: intent.Origin, RouteID: intent.ReviewRouteID}
	if validateSuccessfulBonusReview(intent.SiteID, intent.Selector, config, intent.ReviewLimits, observed, receipt, requests) != nil {
		return fmt.Errorf("fresh bonus review is invalid")
	}
	review := observed.PublicCopy()
	if review.ReviewID != intent.ExpectedReviewID || review.Availability != site.BonusReviewAvailabilityAvailable ||
		review.InputMode != site.BonusReviewInputNone || review.ActionMethod != "post" || review.ActionRouteID != intent.ActionRouteID {
		return fmt.Errorf("fresh bonus review does not reproduce the intent")
	}
	return nil
}

func validateBonusExchangeObservation(intent bonusexchange.IntentRecord, observed *site.ObservedBonusExchange, receipt site.BonusExchangeReceipt, requests int) error {
	if observed == nil || !observed.MatchesReceipt(receipt) || !observed.Matches(intent.SiteID, intent.Selector, intent.ExpectedReviewID, intent.Config()) ||
		site.ValidateBonusExchangeReceipt(receipt) != nil || receipt.Limits != intent.ExchangeLimits || requests != receipt.Used.TotalRequestsAttempted ||
		receipt.Used.ReviewRequestsAttempted != 1 || receipt.Used.SubmissionRequestsAttempted > 1 || receipt.Used.AutomaticRetries != 0 || receipt.Used.RedirectsFollowed != 0 {
		return fmt.Errorf("bonus exchange observation is invalid")
	}
	return nil
}

func publicBonusExchangeReceipt(intent bonusexchange.IntentRecord, receipt site.BonusExchangeReceipt) site.BonusExchangeReceipt {
	limits := intent.ExchangeLimits
	result := site.BonusExchangeReceipt{
		Effect: site.BonusExchangeSubmitEffect, SiteID: intent.SiteID, Selector: intent.Selector,
		ExpectedReviewID: intent.ExpectedReviewID, FreshReviewID: intent.ExpectedReviewID,
		Origin: intent.Origin, ReviewRouteID: intent.ReviewRouteID, ActionRouteID: intent.ActionRouteID,
		Complete: receipt.Complete, RequestAttempted: receipt.RequestAttempted, ResponseComplete: receipt.ResponseComplete,
		Outcome: safeBonusExchangeOutcome(receipt.Outcome), Limits: limits, StopReason: safeBonusExchangeSubmitStop(receipt.StopReason),
		Used: site.BonusExchangeUsage{
			TotalRequestsAttempted:      boundedFetchCounter(receipt.Used.TotalRequestsAttempted, limits.MaxSessionRequests+1),
			ReviewRequestsAttempted:     boundedFetchCounter(receipt.Used.ReviewRequestsAttempted, limits.MaxSessionRequests+1),
			SubmissionRequestsAttempted: boundedFetchCounter(receipt.Used.SubmissionRequestsAttempted, limits.MaxSubmissionRequests+1),
			AutomaticRetries:            boundedFetchCounter(receipt.Used.AutomaticRetries, limits.MaxSessionRequests+1),
			RedirectsFollowed:           boundedFetchCounter(receipt.Used.RedirectsFollowed, limits.MaxSessionRequests+1),
			FormFieldsSubmitted:         boundedFetchCounter(receipt.Used.FormFieldsSubmitted, limits.MaxFormFields+1),
			FormBytesSubmitted:          boundedFetchBytes(receipt.Used.FormBytesSubmitted, limits.MaxFormBytes+1),
			ResponseBytesRead:           boundedFetchBytes(receipt.Used.ResponseBytesRead, limits.MaxResponseBytes+1),
			ResponseBytesKnown:          receipt.Used.ResponseBytesKnown,
		},
	}
	if !receipt.ObservedAtStart.IsZero() && !receipt.ObservedAtEnd.IsZero() && !receipt.ObservedAtEnd.Before(receipt.ObservedAtStart) {
		result.ObservedAtStart = receipt.ObservedAtStart.UTC()
		result.ObservedAtEnd = receipt.ObservedAtEnd.UTC()
	}
	if result.Outcome == site.BonusExchangeOutcomeConfirmed && receipt.ConfirmationCode != "" {
		result.ConfirmationCode = "allowlisted_site_redirect"
	}
	return result
}

func invalidBonusExchangeReceipt(intent bonusexchange.IntentRecord, requestsMade int) site.BonusExchangeReceipt {
	limits := intent.ExchangeLimits
	total := boundedFetchCounter(requestsMade, limits.MaxSessionRequests+1)
	reviewRequests := 0
	if total > 0 {
		reviewRequests = 1
	}
	submissionRequests := boundedFetchCounter(total-reviewRequests, limits.MaxSubmissionRequests+1)
	return site.BonusExchangeReceipt{
		Effect: site.BonusExchangeSubmitEffect, SiteID: intent.SiteID, Selector: intent.Selector,
		ExpectedReviewID: intent.ExpectedReviewID, FreshReviewID: intent.ExpectedReviewID,
		Origin: intent.Origin, ReviewRouteID: intent.ReviewRouteID, ActionRouteID: intent.ActionRouteID,
		RequestAttempted: submissionRequests > 0, Outcome: site.BonusExchangeOutcomeUnknown,
		Limits: limits, StopReason: "site.exchange_receipt_invalid",
		Used: site.BonusExchangeUsage{
			TotalRequestsAttempted: total, ReviewRequestsAttempted: reviewRequests,
			SubmissionRequestsAttempted: submissionRequests,
		},
	}
}

func safeBonusExchangeOutcome(value string) string {
	switch value {
	case site.BonusExchangeOutcomeConfirmed, site.BonusExchangeOutcomeRejected, site.BonusExchangeOutcomeUnknown, site.BonusExchangeOutcomeNotSubmitted:
		return value
	default:
		return site.BonusExchangeOutcomeUnknown
	}
}

func safeBonusExchangeSubmitStop(value string) string {
	switch value {
	case "context_before_submission", "submission_not_sent", "submission_transport_uncertain", "submission_response_unrecognized", "server_duplicate_lock", "server_permission_rejected", "site.exchange_receipt_invalid":
		return value
	case "":
		return ""
	default:
		return "site.exchange_failed"
	}
}

func safeBonusExchangeStateStop(value string, err error) string {
	if value != "" {
		if strings.HasPrefix(value, "outcome_inventory_") || value == "outcome_byte_limit" {
			return value
		}
		return "state.inspection_failed"
	}
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, bonusexchange.ErrCorruptExchange), errors.Is(err, metastore.ErrCorruptRecord):
		return "state.integrity_failed"
	case errors.Is(err, bonusexchange.ErrStatusIncomplete):
		return "state.inventory_incomplete"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "state.context_cancelled"
	default:
		return "state.inspection_failed"
	}
}

func safeBonusExchangeListStop(value string, err error) string {
	switch {
	case value == "context_cancelled" || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return "state.context_cancelled"
	case value == "intent_inventory_changed":
		return "state.intent_inventory_changed"
	case value == "intent_byte_limit":
		return "state.intent_byte_limit"
	case strings.HasPrefix(value, "intent_inventory_"):
		return value
	case errors.Is(err, bonusexchange.ErrCorruptExchange), errors.Is(err, metastore.ErrCorruptRecord):
		return "state.integrity_failed"
	case errors.Is(err, bonusexchange.ErrStatusIncomplete):
		return "state.inventory_incomplete"
	default:
		return "state.inspection_failed"
	}
}

func safeBonusExchangeRetentionStop(value string, err error) string {
	switch value {
	case "context_cancelled", "retention_policy_blocked", "operation_not_found", "durability_unconfirmed",
		"removal_ambiguous", "inventory_incomplete", "integrity_failed", "operation_interrupted",
		"operation_not_terminal", "forget_in_progress", "prune_incomplete", "transition_interrupted",
		"retention_not_found_unattributed":
		return "state." + value
	}
	switch {
	case errors.Is(err, bonusexchange.ErrCorruptExchange), errors.Is(err, metastore.ErrCorruptRecord):
		return "state.integrity_failed"
	case errors.Is(err, bonusexchange.ErrRetentionPolicy):
		return "state.retention_policy_blocked"
	case errors.Is(err, bonusexchange.ErrInvalidExchange):
		return "state.selector_conflict"
	case errors.Is(err, bonusexchange.ErrExchangeNotFound), errors.Is(err, metastore.ErrRecordNotFound):
		return "state.operation_not_found"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "state.context_cancelled"
	case errors.Is(err, metastore.ErrRemovalDurabilityUnconfirmed), errors.Is(err, metastore.ErrDurabilityUnconfirmed):
		return "state.durability_unconfirmed"
	case errors.Is(err, metastore.ErrRemovalAmbiguous):
		return "state.removal_ambiguous"
	default:
		return "state.operation_interrupted"
	}
}

func bonusExchangePublicStateError(err error, fallback string) error {
	if err == nil {
		return fmt.Errorf("%s", fallback)
	}
	if errors.Is(err, bonusexchange.ErrCorruptExchange) || errors.Is(err, metastore.ErrCorruptRecord) || errors.Is(err, metastore.ErrCorruptArtifact) {
		return &integrityErr{message: fallback}
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("bonus exchange canceled: %w", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("bonus exchange timed out: %w", context.DeadlineExceeded)
	}
	return fmt.Errorf("%s", fallback)
}

func bonusExchangeSubmissionPublicError(ctx context.Context, err error, state string) error {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("bonus exchange stopped after durable attempt reservation: %w", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("bonus exchange timed out after durable attempt reservation: %w", context.DeadlineExceeded)
	}
	return fmt.Errorf("bonus exchange completed with durable state %s", terminalSafe(state))
}

func firstBonusExchangeError(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func validateBonusExchangeTimeout(value time.Duration) error {
	if value <= 0 || value > bonusExchangeMaximumTimeout {
		return usageError("--timeout must be greater than zero and at most %s", bonusExchangeMaximumTimeout)
	}
	return nil
}

func addBonusExchangeBlocker(report *siteBonusExchangeReport, code, message string) {
	if report == nil {
		return
	}
	report.Blockers = append(report.Blockers, siteDetailFinding{Code: code, Message: message})
}

func (a *app) finishSiteBonusExchange(output string, report siteBonusExchangeReport, operationErr error) error {
	if writeErr := a.writeSiteBonusExchangeReport(output, report); writeErr != nil {
		return writeErr
	}
	return operationErr
}

func (a *app) writeSiteBonusExchangeReport(output string, report siteBonusExchangeReport) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	return writeSiteBonusExchangeHuman(a.stdout, report)
}

func (a *app) finishSiteBonusExchangeList(output string, report siteBonusExchangeListReport, operationErr error) error {
	if writeErr := a.writeSiteBonusExchangeListReport(output, report); writeErr != nil {
		return writeErr
	}
	return operationErr
}

func (a *app) writeSiteBonusExchangeListReport(output string, report siteBonusExchangeListReport) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	return writeSiteBonusExchangeListHuman(a.stdout, report)
}

func writeSiteBonusExchangeListHuman(out io.Writer, report siteBonusExchangeListReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nCOMPLETE\t%t\n", terminalSafe(report.Outcome), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed, report.Complete)
	fmt.Fprintln(w, "\nBLOCKERS")
	if len(report.Blockers) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, blocker := range report.Blockers {
		fmt.Fprintf(w, "%s\t%s\n", terminalSafe(blocker.Code), terminalSafe(blocker.Message))
	}
	fmt.Fprintf(w, "\nSTORE ID\t%s\nSTOP REASON\t%s\n", terminalSafe(valueOrUnknown(report.Store.StoreID)), terminalSafe(valueOrUnknown(report.StopReason)))
	fmt.Fprintf(w, "\nINTENT INVENTORY PASSES\t%d\nINTENT VERIFICATION PASSES\t%d\nINTENT ENTRIES CONSIDERED\t%d\nHISTORICAL INVENTORY PASSES\t%d\nHISTORICAL ENTRIES CONSIDERED\t%d\nENTRY LIMIT\t%d per pass\nINTENTS MATCHED (ALL PASSES)\t%d\nRECORD LIMIT\t%d per kind/pass\nINTENTS READ\t%d\nINTENT BYTES\t%d\nHISTORICAL RECORDS READ\t%d\nHISTORICAL BYTES\t%d\nSTATE BYTE LIMIT\t%d aggregate per axis\nPATH BYTES LIMIT\t%d per pass\n", report.Used.InventoryPasses, report.Used.IntentVerificationPasses, report.Used.EntriesConsidered, report.Used.Retention.InventoryPasses, report.Used.Retention.EntriesConsidered, report.Limits.MaxStatusEntries, report.Used.IntentRecordsMatched, report.Limits.MaxStatusRecords, report.Used.IntentRecordsRead, report.Used.IntentBytesRead, report.Used.Retention.RecordsRead, report.Used.Retention.BytesRead, report.Limits.MaxStatusBytes, report.Limits.MaxStatusPathBytes)
	fmt.Fprintln(w, "\nOPERATIONS\nINTENT RECORD\tRETENTION RECORD\tFORGET RECORD\tOPERATION ID\tSITE\tOPTION\tEXPECTED REVIEW\tCREATED\tSTATUS")
	if len(report.Operations) == 0 {
		fmt.Fprintln(w, "-\t-\t-\t-\t-\t-\t-\t-\tnone")
	}
	for _, operation := range report.Operations {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", terminalSafe(operation.IntentRecord.ID.String()), terminalSafe(bonusRecordID(operation.RetentionRecord)), terminalSafe(bonusRecordID(operation.ForgetRecord)), terminalSafe(operation.OperationID.String()), terminalSafe(operation.SiteID), terminalSafe(operation.Selector), terminalSafe(operation.ExpectedReviewID), operation.CreatedAt.UTC().Format(time.RFC3339), terminalSafe(operation.Status))
	}
	fmt.Fprintln(w, "\nWARNINGS")
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
	}
	return w.Flush()
}

func writeSiteBonusExchangeHuman(out io.Writer, report siteBonusExchangeReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	submissionOutcome := valueOrUnknown(report.Request.Submission.Outcome)
	submissionStop := valueOrUnknown(report.Request.Submission.StopReason)
	if strings.HasPrefix(report.Request.Status, "not_requested_") {
		submissionOutcome = "-"
		submissionStop = "-"
	}
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nFORM SUBMISSION ATTEMPTS\t%d\nACKNOWLEDGED\t%t\n", terminalSafe(report.Outcome), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed, report.FormSubmissionAttempts, report.Acknowledgement.Provided)
	fmt.Fprintln(w, "\nBLOCKERS")
	if len(report.Blockers) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, blocker := range report.Blockers {
		fmt.Fprintf(w, "%s\t%s\n", terminalSafe(blocker.Code), terminalSafe(blocker.Message))
	}
	fmt.Fprintf(w, "\nSITE\t%s\nOPTION\t%s\nEXPECTED REVIEW\t%s\nORIGIN\t%s\nREVIEW ROUTE\t%s\nACTION ROUTE\t%s\n", terminalSafe(report.Scope.SiteID), terminalSafe(report.Scope.Selector), terminalSafe(report.Scope.ExpectedReviewID), terminalSafe(report.Scope.Origin), terminalSafe(report.Scope.ReviewRouteID), terminalSafe(report.Scope.ActionRouteID))
	fmt.Fprintf(w, "\nOPERATION STATUS\t%s\nOPERATION COMPLETE\t%t\nOPERATION ID\t%s\nINTENT RECORD\t%s\nATTEMPT RECORD\t%s\nOUTCOME RECORD\t%s\nRETENTION RECORD\t%s\nFORGET RECORD\t%s\n", terminalSafe(report.Operation.Status), report.Operation.Complete, terminalSafe(valueOrUnknown(report.Operation.OperationID)), terminalSafe(bonusRecordID(report.Operation.IntentRecord)), terminalSafe(bonusRecordID(report.Operation.AttemptRecord)), terminalSafe(bonusRecordID(report.Operation.OutcomeRecord)), terminalSafe(bonusRecordID(report.Operation.RetentionRecord)), terminalSafe(bonusRecordID(report.Operation.ForgetRecord)))
	if report.Operation.StopReason != "" {
		fmt.Fprintf(w, "OPERATION STOP\t%s\n", terminalSafe(report.Operation.StopReason))
	}
	fmt.Fprintf(w, "\nREQUEST STATUS\t%s\nREQUESTS MADE\t%d\nREVIEW REQUESTS\t%d\nSUBMISSION REQUESTS\t%d\nAUTOMATIC RETRIES\t%d\nREDIRECTS FOLLOWED\t%d\nSUBMISSION OUTCOME\t%s\nSUBMISSION STOP\t%s\n", terminalSafe(report.Request.Status), report.Request.RequestsMade, report.Request.Submission.Used.ReviewRequestsAttempted, report.Request.Submission.Used.SubmissionRequestsAttempted, report.Request.Submission.Used.AutomaticRetries, report.Request.Submission.Used.RedirectsFollowed, terminalSafe(submissionOutcome), terminalSafe(submissionStop))
	fmt.Fprintf(w, "\nPERSISTENCE\t%s\nSTORE ID\t%s\nINTENT WRITES\t%d\nATTEMPT WRITES\t%d\nOUTCOME WRITES\t%d\nRETENTION WRITES\t%d\nFORGET WRITES\t%d\nRECORDS REMOVED\t%d\nRECORDS ALREADY ABSENT\t%d\nREMOVAL DURABLE\t%t\nATTEMPT RECORD VERIFIED\t%t\nOUTCOME RECORD VERIFIED\t%t\nOUTCOME DURABLE THIS INVOCATION\t%t\n", terminalSafe(report.Persistence.Status), terminalSafe(valueOrUnknown(report.Persistence.Store.StoreID)), report.Persistence.IntentWrites, report.Persistence.AttemptWrites, report.Persistence.OutcomeWrites, report.Persistence.RetentionWrites, report.Persistence.ForgetWrites, report.Persistence.RecordsRemoved, report.Persistence.RecordsAlreadyAbsent, report.Persistence.RemovalDurable, report.Persistence.AttemptRecordVerified, report.Persistence.OutcomeRecordVerified, report.Persistence.OutcomeDurable)
	fmt.Fprintf(w, "\nFRESH REVIEW REPRODUCED\t%t\nPROCESS-LOCAL REVIEW AUTHORITY\t%t\nATTEMPT MARKER DURABLE THIS INVOCATION\t%t\nPROCESS-LOCAL OUTCOME AUTHORITY\t%t\nATTEMPT MARKER BLOCKS FUTURE SUBMISSIONS\t%t\nSUBMISSION REQUEST BOUND VERIFIED\t%t\nTERMINAL CHAIN VERIFIED\t%t\nNON-EXECUTABLE TOMBSTONE\t%t\nNO NETWORK ACCESS\t%t\nHISTORICAL EVIDENCE ERASED\t%t\nAT-MOST-ONCE SCOPE\t%s\nNO AUTOMATIC RETRY\t%t\nSERIALIZED AUTHORITY\t%t\n", report.Assurance.FreshReviewReproduced, report.Assurance.ProcessLocalReviewAuthority, report.Assurance.AttemptMarkerDurable, report.Assurance.ProcessLocalOutcomeAuthority, report.Assurance.AttemptMarkerBlocksFuture, report.Assurance.SubmissionRequestBound, report.Assurance.TerminalChainVerified, report.Assurance.NonExecutableTombstone, report.Assurance.NoNetworkAccess, report.Assurance.HistoricalEvidenceErased, terminalSafe(report.Assurance.AtMostOnceScope), report.Assurance.NoAutomaticRetry, report.Assurance.SerializedAuthority)
	fmt.Fprintln(w, "\nWARNINGS")
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
	}
	return w.Flush()
}

func bonusRecordID(ref *metastore.RecordRef) string {
	if ref == nil || ref.ID == "" {
		return "-"
	}
	return ref.ID.String()
}

func (a *app) siteBonusExchangeHelp() {
	fmt.Fprint(a.stdout, `Usage:
  ptctl site bonus exchange prepare --state-store DIR --expect-review-id ID [--output table|json] SITE OPTION
  ptctl site bonus exchange submit --state-store DIR --intent-record RECORD_ID --expect-review-id ID --cookie-stdin --acknowledge-bonus-exchange [--output table|json] SITE OPTION
  ptctl site bonus exchange status --state-store DIR --intent-record RECORD_ID [--output table|json]
  ptctl site bonus exchange list --state-store DIR [--max-operations N] [--output table|json]
  ptctl site bonus exchange prune --state-store DIR --intent-record RECORD_ID --operation-id ID --acknowledge-state-prune [--output table|json]
  ptctl site bonus exchange forget --state-store DIR --retention-record RECORD_ID --operation-id ID --acknowledge-history-forget [--output table|json]

Prepare writes only a private reviewed intent. Submit validates that intent and
the production adapter before reading stdin, performs one fresh bounded review,
durably publishes a deterministic at-most-once attempt marker, and only then
may submit the exact opaque form once. There are no redirects or retries.

After the POST, a bounded local finalization interval may persist the exact
adapter observation even if the network context ended. If no durable outcome
can be established, status remains submission-unknown and the attempt must not
be retried. Status is local and never reads a credential or contacts the site.
List performs bounded, detected-stable inventories of verified live intents and
historical retention/forget markers. It does not infer the complete transition
state and never chooses a latest operation.

Prune is a local, credential-free transition available only for an explicitly
selected complete terminal operation. It durably writes one deterministic,
non-executable tombstone before removing the exact outcome, attempt, and intent
records. Prepared and submission-unknown operations are never pruned.

Forget is a separate irreversible local transition with its own explicit
acknowledgement. It writes a deterministic crash-recovery marker before removing
the selected tombstone, then removes that marker last. Once complete, a later
absence is deliberately unattributed and is not reported as already forgotten.
`)
}
