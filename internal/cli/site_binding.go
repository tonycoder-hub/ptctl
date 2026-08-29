package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/sitebinding"
)

const (
	siteBindingReadDefaultTimeout = time.Minute
	siteBindingReadMaxTimeout     = time.Hour
)

type siteBindingListAssurance struct {
	Evidence          string `json:"evidence"`
	RecordsVerified   bool   `json:"records_verified"`
	ArtifactsVerified bool   `json:"artifacts_verified"`
	Selection         string `json:"selection"`
}

type siteBindingListReport struct {
	kind            string                    `json:"-"`
	Outcome         string                    `json:"outcome"`
	Effect          string                    `json:"effect"`
	WritesPerformed int                       `json:"writes_performed"`
	Complete        bool                      `json:"complete"`
	Store           metastore.StoreInfo       `json:"store"`
	Limits          sitebinding.ListLimits    `json:"limits"`
	Used            metastore.RecordListUsage `json:"used"`
	Bindings        []metastore.RecordRef     `json:"bindings"`
	StopReason      string                    `json:"stop_reason,omitempty"`
	Assurance       siteBindingListAssurance  `json:"assurance"`
	Blockers        []string                  `json:"blockers"`
	Warnings        []string                  `json:"warnings"`
}

type siteBindingInspectAssurance struct {
	RecordDigestVerified      bool `json:"record_digest_verified"`
	ArtifactDigestVerified    bool `json:"artifact_digest_verified"`
	ArtifactStrictlyParsed    bool `json:"artifact_strictly_parsed"`
	PrivateArtifactVerified   bool `json:"private_artifact_verified"`
	SameStoreOperationBound   bool `json:"same_store_operation_bound"`
	AdapterProvenanceAccepted bool `json:"adapter_provenance_accepted"`
	ProcessLocalProofObserved bool `json:"process_local_proof_observed"`
}

type siteBindingInspectReport struct {
	kind            string                      `json:"-"`
	Outcome         string                      `json:"outcome"`
	Effect          string                      `json:"effect"`
	WritesPerformed int                         `json:"writes_performed"`
	Complete        bool                        `json:"complete"`
	RecordID        metastore.RecordID          `json:"record_id"`
	Receipt         sitebinding.LoadReceipt     `json:"receipt"`
	Binding         *sitebinding.PublicBinding  `json:"binding,omitempty"`
	Assurance       siteBindingInspectAssurance `json:"assurance"`
	Blockers        []string                    `json:"blockers"`
	Warnings        []string                    `json:"warnings"`
}

func (a *app) siteMetafileBinding(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		a.siteMetafileBindingHelp()
		return nil
	}
	switch args[0] {
	case "list":
		return a.siteMetafileBindingList(args[1:])
	case "inspect":
		return a.siteMetafileBindingInspect(args[1:])
	default:
		return usageError("unknown site metafile binding subcommand %q", args[0])
	}
}

func (a *app) siteMetafileBindingHelp() {
	fmt.Fprint(a.stdout, `Usage:
  ptctl site metafile binding list --metafile-store DIR [--output table|json] [limits]
  ptctl site metafile binding inspect --metafile-store DIR [--output table|json] RECORD_ID

List performs one bounded name-only inventory and returns unverified record
locators in deterministic ID order. It never reads binding payloads, selects a
latest observation, or verifies linked artifacts. Inspect accepts exactly one
explicit record ID and jointly verifies its canonical binding and referenced
private metafile artifact under one bound store session. Both commands are
read-only, require no site credential, and never contact a site.
`)
}

func (a *app) siteMetafileBindingList(args []string) error {
	fs := newFlagSet("site metafile binding list")
	output := fs.String("output", "table", "table or json")
	storeRoot := fs.String("metafile-store", "", "initialized private metafile store root")
	timeout := fs.Duration("timeout", siteBindingReadDefaultTimeout, "bounded private-store inventory time budget")
	defaults := sitebinding.DefaultListLimits()
	maxEntries := fs.Int("max-store-entries", defaults.MaxEntries, "maximum private objects examined")
	maxBindings := fs.Int("max-bindings", defaults.MaxBindings, "maximum binding record locators retained")
	maxPathBytes := fs.Int64("max-store-path-bytes", defaults.MaxPathBytes, "maximum private object-name bytes examined")
	if handled, err := parseStorageFlags(a, fs, args,
		"ptctl site metafile binding list --metafile-store DIR [flags]",
		"Performs one bounded locator inventory. Returned IDs are candidates only; no record or artifact content is verified and no latest record is selected."); handled || err != nil {
		return err
	}
	if fs.NArg() != 0 || *storeRoot == "" {
		return usageError("site metafile binding list requires --metafile-store and flags only")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > siteBindingReadMaxTimeout {
		return usageError("--timeout must be greater than zero and no more than 1h")
	}
	limits := defaults
	limits.MaxEntries = *maxEntries
	limits.MaxBindings = *maxBindings
	limits.MaxPathBytes = *maxPathBytes
	if err := limits.Validate(); err != nil {
		return usageError("site metafile binding list: %v", err)
	}
	report := newSiteBindingListReport(limits)
	store, err := metastore.Open(*storeRoot)
	if err != nil {
		report.Blockers = append(report.Blockers, "binding.store_unavailable")
		if writeErr := writeSiteBindingListReport(a.stdout, *output, report); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("open site metafile binding store failed")
	}
	repository, err := sitebinding.NewRepository(store, sitebinding.DefaultLimits())
	if err != nil {
		report.Store = store.Info()
		report.Blockers = append(report.Blockers, "binding.repository_unavailable")
		if writeErr := writeSiteBindingListReport(a.stdout, *output, report); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("open site metafile binding repository failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	listed, listErr := repository.ListLocators(ctx, limits)
	report.Store = listed.Store
	report.Complete = listed.Complete
	report.Used = listed.Used
	report.Bindings = append(report.Bindings, listed.Bindings...)
	report.StopReason = safeSiteBindingListStopReason(listed.StopReason)
	if listed.Complete {
		report.Outcome = "complete"
	} else {
		report.Outcome = "incomplete"
		code := "binding.list_incomplete"
		if report.StopReason != "" {
			code = "binding." + report.StopReason
		}
		report.Blockers = append(report.Blockers, code)
	}
	if listErr != nil && errors.Is(listErr, metastore.ErrCorruptRecord) {
		report.Outcome = "integrity_failed"
		report.Blockers = appendUniqueString(report.Blockers, "binding.store_integrity_failed")
	}
	if writeErr := writeSiteBindingListReport(a.stdout, *output, report); writeErr != nil {
		return writeErr
	}
	if listErr != nil {
		if errors.Is(listErr, metastore.ErrCorruptRecord) {
			return &integrityErr{message: "the private store object namespace failed integrity verification"}
		}
		if errors.Is(listErr, context.Canceled) || errors.Is(listErr, context.DeadlineExceeded) {
			return fmt.Errorf("site metafile binding list was canceled")
		}
		return fmt.Errorf("site metafile binding list failed")
	}
	if !listed.Complete {
		return &inconclusiveErr{message: "site metafile binding locator inventory is incomplete"}
	}
	return nil
}

func (a *app) siteMetafileBindingInspect(args []string) error {
	fs := newFlagSet("site metafile binding inspect")
	output := fs.String("output", "table", "table or json")
	storeRoot := fs.String("metafile-store", "", "initialized private metafile store root")
	timeout := fs.Duration("timeout", siteBindingReadDefaultTimeout, "joint binding and artifact verification time budget")
	if handled, err := parseStorageFlags(a, fs, args,
		"ptctl site metafile binding inspect --metafile-store DIR [flags] RECORD_ID",
		"Jointly verifies one explicit sealed binding record and its exact private metafile artifact. The observation is historical and is not current site state."); handled || err != nil {
		return err
	}
	if fs.NArg() != 1 || *storeRoot == "" {
		return usageError("site metafile binding inspect requires --metafile-store and one RECORD_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > siteBindingReadMaxTimeout {
		return usageError("--timeout must be greater than zero and no more than 1h")
	}
	recordID, err := metastore.ParseRecordID(fs.Arg(0))
	if err != nil {
		return usageError("site metafile binding inspect requires a canonical sealed RECORD_ID")
	}
	report := newSiteBindingInspectReport(recordID)
	store, err := metastore.Open(*storeRoot)
	if err != nil {
		report.Blockers = append(report.Blockers, "binding.store_unavailable")
		if writeErr := writeSiteBindingInspectReport(a.stdout, *output, report); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("open site metafile binding store failed")
	}
	repository, err := sitebinding.NewRepository(store, sitebinding.DefaultLimits())
	if err != nil {
		report.Receipt.Store = store.Info()
		report.Blockers = append(report.Blockers, "binding.repository_unavailable")
		if writeErr := writeSiteBindingInspectReport(a.stdout, *output, report); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("open site metafile binding repository failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	verified, receipt, loadErr := repository.Load(ctx, recordID)
	report.Receipt = receipt
	if loadErr == nil && (verified == nil || !verified.Verified() || !receipt.Complete) {
		loadErr = fmt.Errorf("site metafile binding load result is inconsistent")
	}
	if loadErr == nil {
		public := verified.PublicCopy()
		report.Binding = &public
		report.Complete = receipt.Complete
		report.Assurance.RecordDigestVerified = true
		report.Assurance.ArtifactDigestVerified = true
		report.Assurance.ArtifactStrictlyParsed = true
		report.Assurance.PrivateArtifactVerified = true
		report.Assurance.SameStoreOperationBound = true
		report.Assurance.ProcessLocalProofObserved = true
		if adapterErr := validateSiteBindingAdapter(a.registry, verified); adapterErr != nil {
			report.Outcome = "blocked"
			report.Blockers = append(report.Blockers, "binding.adapter_provenance_mismatch")
			loadErr = &inconclusiveErr{message: "site metafile binding adapter provenance is unavailable"}
		} else {
			report.Outcome = "verified"
			report.Assurance.AdapterProvenanceAccepted = true
		}
	} else {
		code, outcome := siteBindingInspectFailure(loadErr)
		report.Outcome = outcome
		report.Blockers = append(report.Blockers, code)
	}
	if writeErr := writeSiteBindingInspectReport(a.stdout, *output, report); writeErr != nil {
		return writeErr
	}
	if loadErr == nil {
		return nil
	}
	var inconclusive *inconclusiveErr
	if errors.As(loadErr, &inconclusive) {
		return loadErr
	}
	if errors.Is(loadErr, sitebinding.ErrCorruptBinding) || errors.Is(loadErr, sitebinding.ErrInvalidBinding) || errors.Is(loadErr, metastore.ErrCorruptRecord) ||
		errors.Is(loadErr, metastore.ErrCorruptArtifact) || errors.Is(loadErr, metastore.ErrRecordConsumerIncomplete) {
		return &integrityErr{message: "the sealed site binding or linked private metafile failed integrity verification"}
	}
	if errors.Is(loadErr, context.Canceled) || errors.Is(loadErr, context.DeadlineExceeded) {
		return fmt.Errorf("site metafile binding inspection was canceled")
	}
	if errors.Is(loadErr, metastore.ErrRecordNotFound) {
		return fmt.Errorf("site metafile binding record was not found")
	}
	return fmt.Errorf("site metafile binding inspection failed")
}

func newSiteBindingListReport(limits sitebinding.ListLimits) siteBindingListReport {
	return siteBindingListReport{
		kind: "site.metafile.binding.list", Outcome: "incomplete",
		Effect: "list_private_site_metafile_binding_locators", Limits: limits,
		Bindings: []metastore.RecordRef{}, Blockers: []string{},
		Warnings: []string{
			"listed record IDs are unverified locators; inspect one explicit ID before treating it as historical provenance",
			"the list is sorted by record ID and never selects a latest observation",
		},
		Assurance: siteBindingListAssurance{
			Evidence: "bounded_name_only_locator_inventory", Selection: "none",
		},
	}
}

func newSiteBindingInspectReport(recordID metastore.RecordID) siteBindingInspectReport {
	return siteBindingInspectReport{
		kind: "site.metafile.binding.inspect", Outcome: "incomplete",
		Effect: "read_verified_site_metafile_binding", RecordID: recordID,
		Blockers: []string{}, Warnings: []string{
			"the verified binding is one historical exact-response observation, not current site state",
			"the JSON report is authority-free; reconciliation must explicitly reload the record in its own invocation",
		},
	}
}

func safeSiteBindingListStopReason(value string) string {
	switch value {
	case "", "entry_limit", "record_limit", "path_limit", "context_cancelled":
		return value
	default:
		return "list_failed"
	}
}

func siteBindingInspectFailure(err error) (string, string) {
	switch {
	case errors.Is(err, sitebinding.ErrCorruptBinding), errors.Is(err, sitebinding.ErrInvalidBinding), errors.Is(err, metastore.ErrCorruptRecord),
		errors.Is(err, metastore.ErrCorruptArtifact), errors.Is(err, metastore.ErrRecordConsumerIncomplete):
		return "binding.integrity_failed", "integrity_failed"
	case errors.Is(err, metastore.ErrRecordNotFound):
		return "binding.record_not_found", "not_found"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "binding.context_cancelled", "incomplete"
	default:
		return "binding.load_failed", "incomplete"
	}
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func writeSiteBindingListReport(out io.Writer, output string, report siteBindingListReport) error {
	if output == "json" {
		return writeJSON(out, report, nil)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES\t%d\nCOMPLETE\t%t\nSTORE\t%s\nEVIDENCE\t%s\nSELECTION\t%s\n",
		terminalSafe(report.Outcome), terminalSafe(report.Effect), report.WritesPerformed, report.Complete,
		terminalSafe(report.Store.StoreID), terminalSafe(report.Assurance.Evidence), terminalSafe(report.Assurance.Selection))
	if report.StopReason != "" {
		fmt.Fprintf(w, "STOP REASON\t%s\n", terminalSafe(report.StopReason))
	}
	if len(report.Blockers) > 0 {
		fmt.Fprintln(w, "\nBLOCKERS")
		for _, blocker := range report.Blockers {
			fmt.Fprintf(w, "%s\n", terminalSafe(blocker))
		}
	}
	fmt.Fprintln(w, "\nBINDING RECORD LOCATORS")
	fmt.Fprintln(w, "RECORD ID\tBYTES\tEVIDENCE")
	for _, binding := range report.Bindings {
		fmt.Fprintf(w, "%s\t%d\tname_only_unverified_locator\n", terminalSafe(binding.ID.String()), binding.SizeBytes)
	}
	if len(report.Bindings) == 0 {
		fmt.Fprintln(w, "-\t0\tnone")
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(out, "warning: %s\n", terminalSafe(warning))
	}
	return nil
}

func writeSiteBindingInspectReport(out io.Writer, output string, report siteBindingInspectReport) error {
	if output == "json" {
		return writeJSON(out, report, nil)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES\t%d\nCOMPLETE\t%t\nRECORD\t%s\nSTORE\t%s\n",
		terminalSafe(report.Outcome), terminalSafe(report.Effect), report.WritesPerformed, report.Complete,
		terminalSafe(report.RecordID.String()), terminalSafe(report.Receipt.Store.StoreID))
	if len(report.Blockers) > 0 {
		fmt.Fprintln(w, "\nBLOCKERS")
		for _, blocker := range report.Blockers {
			fmt.Fprintf(w, "%s\n", terminalSafe(blocker))
		}
	}
	if report.Binding != nil {
		record := report.Binding.Record
		fmt.Fprintln(w, "\nHISTORICAL SITE BINDING")
		fmt.Fprintf(w, "SITE\t%s\nREMOTE ID\t%s\nVARIANT\t%s\nARTIFACT\t%s\nORIGIN\t%s\nROUTE\t%s\nOBSERVED START\t%s\nOBSERVED END\t%s\n",
			terminalSafe(record.SiteID), terminalSafe(record.RemoteID), terminalSafe(record.MetafileVariantID), terminalSafe(record.ArtifactID.String()),
			terminalSafe(record.Origin), terminalSafe(record.RouteID), terminalSafe(record.ObservedAtStart.Format(time.RFC3339Nano)), terminalSafe(record.ObservedAtEnd.Format(time.RFC3339Nano)))
	}
	fmt.Fprintln(w, "\nASSURANCE")
	fmt.Fprintf(w, "RECORD DIGEST VERIFIED\t%t\nARTIFACT DIGEST VERIFIED\t%t\nARTIFACT STRICTLY PARSED\t%t\nPRIVATE ARTIFACT VERIFIED\t%t\nSAME STORE OPERATION BOUND\t%t\nADAPTER PROVENANCE ACCEPTED\t%t\nPROCESS-LOCAL PROOF OBSERVED\t%t\n",
		report.Assurance.RecordDigestVerified, report.Assurance.ArtifactDigestVerified,
		report.Assurance.ArtifactStrictlyParsed, report.Assurance.PrivateArtifactVerified,
		report.Assurance.SameStoreOperationBound, report.Assurance.AdapterProvenanceAccepted,
		report.Assurance.ProcessLocalProofObserved)
	if err := w.Flush(); err != nil {
		return err
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(out, "warning: %s\n", terminalSafe(warning))
	}
	return nil
}
