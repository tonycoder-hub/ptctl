package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const (
	siteMetafileDefaultTimeout = 45 * time.Second
	siteMetafileMaxTimeout     = 5 * time.Minute
)

type siteMetafileFetchReport struct {
	kind            string
	Outcome         string                      `json:"outcome"`
	Effect          []string                    `json:"effect"`
	WritesPerformed int                         `json:"writes_performed"`
	Acknowledgement siteMetafileAcknowledgement `json:"acknowledgement"`
	Site            siteMetafileSiteSummary     `json:"site"`
	Binding         siteMetafileBindingSummary  `json:"binding"`
	Request         siteMetafileRequestSummary  `json:"request"`
	StoreOperation  metafileStoreReport         `json:"store_operation"`
	Blockers        []string                    `json:"blockers"`
	Issues          []string                    `json:"issues"`
	Warnings        []string                    `json:"warnings"`
}

type siteMetafileAcknowledgement struct {
	Provided bool   `json:"provided"`
	Scope    string `json:"scope"`
}

type siteMetafileSiteSummary struct {
	Ref            domain.TorrentRef `json:"ref"`
	ExpectedOrigin string            `json:"expected_origin"`
	ExpectedRoute  string            `json:"expected_route_id"`
}

type siteMetafileBindingSummary struct {
	Status        string                          `json:"status"`
	EvidenceLevel string                          `json:"evidence_level"`
	EvidenceBasis []string                        `json:"evidence_basis"`
	Observation   *domain.SiteMetafileObservation `json:"observation,omitempty"`
	BlockerCodes  []string                        `json:"blocker_codes"`
}

type siteMetafileRequestSummary struct {
	Status       string                    `json:"status"`
	RequestsMade int                       `json:"requests_made"`
	Retries      int                       `json:"retries"`
	Redirects    int                       `json:"redirects"`
	Receipt      site.MetafileFetchReceipt `json:"receipt"`
}

func (a *app) siteMetafileFetch(args []string) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		a.siteMetafileFetchHelp()
		return nil
	}
	fs := newFlagSet("site metafile fetch")
	output := fs.String("output", "table", "table or json")
	cookieStdin := fs.Bool("cookie-stdin", false, "read the session Cookie header value from stdin")
	acknowledge := fs.Bool("acknowledge-site-effect", false, "acknowledge that this GET may be recorded as a torrent download")
	storeRoot := fs.String("metafile-store", "", "initialized private metafile store root")
	showAbsolute := fs.Bool("show-absolute-paths", false, "include the absolute store root in output; object paths are never shown")
	defaults := site.DefaultMetafileFetchLimits()
	maxMetafileBytes := fs.Int64("max-metafile-bytes", defaults.MaxResponseBytes, "maximum response bytes; hard maximum 32 MiB")
	timeout := fs.Duration("timeout", siteMetafileDefaultTimeout, "shared fetch and private-store import timeout")
	if err := fs.Parse(args); err != nil {
		return usageError("site metafile fetch: %v", err)
	}
	if fs.NArg() != 2 || fs.Arg(0) == "" || fs.Arg(1) == "" {
		return usageError("site metafile fetch requires SITE and REMOTE_ID")
	}
	if !*cookieStdin {
		return usageError("--cookie-stdin is required; credentials are never accepted in argv")
	}
	if !*acknowledge {
		return usageError("--acknowledge-site-effect is required because the GET may be recorded as a torrent download")
	}
	if *storeRoot == "" {
		return usageError("--metafile-store DIR is required")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > siteMetafileMaxTimeout {
		return usageError("--timeout must be greater than zero and at most %s", siteMetafileMaxTimeout)
	}
	fetchLimits := defaults
	fetchLimits.MaxResponseBytes = *maxMetafileBytes
	if err := fetchLimits.Validate(); err != nil {
		return usageError("invalid metafile fetch limits")
	}
	storeLimits := metastore.DefaultLimits()
	if fetchLimits.MaxResponseBytes > storeLimits.MaxArtifactBytes {
		return usageError("--max-metafile-bytes exceeds the private store artifact limit")
	}

	adapter, ok := a.registry.Get(fs.Arg(0))
	if !ok {
		return fmt.Errorf("unknown site %q", fs.Arg(0))
	}
	descriptor := adapter.Descriptor()
	if !descriptor.Supports(domain.CapabilityMetafile) {
		return fmt.Errorf("site %q does not declare capability %q", descriptor.ID, domain.CapabilityMetafile)
	}
	if !descriptor.SupportsAuth(domain.AuthMethodCookieHeader) {
		return fmt.Errorf("site %q does not support cookie_header authentication", descriptor.ID)
	}
	fetcher, ok := adapter.(site.MetafileFetcher)
	if !ok {
		return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, domain.CapabilityMetafile)
	}
	config, err := fetcher.MetafileFetchConfig()
	if err != nil || config.Validate() != nil {
		return fmt.Errorf("site %q has an invalid metafile-fetch configuration", descriptor.ID)
	}
	descriptorOrigin, err := canonicalSiteDescriptorOrigin(descriptor.BaseURL)
	if err != nil || descriptorOrigin != config.Origin {
		return fmt.Errorf("site %q has an unsafe metafile-fetch origin", descriptor.ID)
	}
	ref := domain.TorrentRef{SiteID: descriptor.ID, RemoteID: fs.Arg(1)}
	if err := fetcher.ValidateMetafileRef(ref); err != nil {
		return usageError("site metafile fetch: invalid SITE/REMOTE_ID")
	}

	report := newSiteMetafileFetchReport(ref, config, fetchLimits, storeLimits, *showAbsolute, *storeRoot)
	store, err := metastore.Open(*storeRoot)
	if err != nil {
		report.Outcome = "blocked"
		report.Blockers = append(report.Blockers, "store.open_failed")
		report.StoreOperation = failedMetafileStoreReport("metafile.store.import", "write_private_metafile_store", storeLimits, "store.open_failed", *showAbsolute, *storeRoot, "")
		return a.finishSiteMetafileFetch(*output, report, err)
	}
	report.StoreOperation.Store = storeSummary(store.Info(), *showAbsolute)

	credential, err := readEffectfulSiteCredential(a.stdin)
	if err != nil {
		report.Outcome = "blocked"
		report.Blockers = append(report.Blockers, "credential.read_failed")
		return a.finishSiteMetafileFetch(*output, report, fmt.Errorf("read site credential failed"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	session, err := fetcher.OpenMetafileFetchSession(ctx, credential)
	if err != nil {
		report.Outcome = "blocked"
		report.Blockers = append(report.Blockers, "site.session_open_failed")
		return a.finishSiteMetafileFetch(*output, report, fmt.Errorf("open site metafile session failed"))
	}

	fetched, receipt, fetchErr := session.FetchMetafile(ctx, ref, fetchLimits)
	report.Request.RequestsMade = session.RequestsMade()
	report.Request.Receipt = publicMetafileFetchReceipt(ref, config, fetchLimits, receipt, false)
	report.Request.Retries = report.Request.Receipt.Used.AutomaticRetries
	report.Request.Redirects = report.Request.Receipt.Used.RedirectsFollowed
	closeErr := session.Close()
	if fetchErr != nil {
		report.Outcome = "blocked"
		report.Request.Status = "blocked"
		code := report.Request.Receipt.StopReason
		if code == "" {
			code = siteMetafileStopReason(ctx, fetchErr)
		}
		report.Blockers = append(report.Blockers, code)
		return a.finishSiteMetafileFetch(*output, report, siteMetafileFetchPublicError(ctx, fetchErr))
	}
	if closeErr != nil {
		report.Outcome = "blocked"
		report.Request.Status = "blocked"
		report.Blockers = append(report.Blockers, "site.session_close_failed")
		return a.finishSiteMetafileFetch(*output, report, fmt.Errorf("close site metafile session failed"))
	}
	report.Request.Status = "complete"
	if fetched == nil || !receipt.Complete {
		report.Outcome = "blocked"
		report.Blockers = append(report.Blockers, "site.response_incomplete")
		return a.finishSiteMetafileFetch(*output, report, fmt.Errorf("site metafile response was incomplete"))
	}
	if err := validateSuccessfulMetafileFetch(ref, config, fetchLimits, fetched, receipt, report.Request.RequestsMade); err != nil {
		report.Outcome = "blocked"
		report.Request.Status = "blocked"
		report.Blockers = append(report.Blockers, "site.fetch_receipt_invalid")
		return a.finishSiteMetafileFetch(*output, report, fmt.Errorf("site metafile fetch receipt was inconsistent"))
	}
	report.Request.Receipt = publicMetafileFetchReceipt(ref, config, fetchLimits, receipt, true)

	raw, err := readFetchedMetafile(fetched, fetchLimits.MaxResponseBytes)
	if err != nil {
		report.Outcome = "blocked"
		report.Blockers = append(report.Blockers, "site.response_authority_unavailable")
		return a.finishSiteMetafileFetch(*output, report, fmt.Errorf("site metafile response authority is unavailable"))
	}
	parsed, err := metafile.Parse(raw)
	if err != nil || !parsed.Private {
		report.Outcome = "integrity_failed"
		code := "artifact.invalid_metafile"
		if err == nil {
			code = "artifact.not_private"
		}
		report.Blockers = append(report.Blockers, code)
		report.Issues = append(report.Issues, code)
		report.StoreOperation = failedMetafileStoreReport("metafile.store.import", "write_private_metafile_store", storeLimits, code, *showAbsolute, *storeRoot, "")
		report.StoreOperation.Store = storeSummary(store.Info(), *showAbsolute)
		if writeErr := a.writeSiteMetafileFetchReport(*output, report); writeErr != nil {
			return writeErr
		}
		return &integrityErr{message: "site response was not a strictly valid private metafile"}
	}

	storedMeta, artifactRef, importReceipt, importErr := store.Import(ctx, bytes.NewReader(raw), storeLimits)
	storeReport, outcome, blocker, mappedErr := siteFetchStoreOperation(store, storedMeta, artifactRef, importReceipt, importErr, storeLimits, *showAbsolute, *storeRoot)
	report.StoreOperation = storeReport
	report.WritesPerformed = importReceipt.WritesPerformed
	report.Outcome = outcome
	if blocker != "" {
		report.Blockers = append(report.Blockers, blocker)
		report.Issues = append(report.Issues, blocker)
	}

	if artifactRef.ID != "" && importReceipt.BytesConsumed == int64(len(raw)) {
		binding, bindErr := fetched.BindImported(artifactRef.MetafileVariantID, importReceipt.BytesConsumed)
		if bindErr == nil && binding != nil && binding.Matches(ref, config.Origin, config.RouteID, artifactRef.MetafileVariantID) {
			observation := binding.PublicCopy()
			if observation.Origin != config.Origin || observation.RouteID != config.RouteID || observation.ResponseBytes != receipt.Used.ResponseBytesRead || !observation.ObservedAtStart.Equal(receipt.ObservedAtStart) || !observation.ObservedAtEnd.Equal(receipt.ObservedAtEnd) {
				report.Outcome = "published_post_commit_failure"
				report.Blockers = append(report.Blockers, "binding.observation_receipt_disagrees")
				mappedErr = fmt.Errorf("site metafile observation disagrees with its request receipt")
			} else {
				report.Binding = siteMetafileBindingSummary{
					Status: "observed_exact_variant", EvidenceLevel: "direct_observation",
					EvidenceBasis: []string{"effectful_same_origin_get_for_remote_id", "complete_bounded_response", "whole_response_sha256_equals_metafile_variant_id"},
					Observation:   &observation, BlockerCodes: []string{},
				}
			}
		} else if importErr == nil {
			report.Outcome = "published_post_commit_failure"
			report.Blockers = append(report.Blockers, "binding.exact_variant_not_established")
			mappedErr = fmt.Errorf("site metafile binding could not be established")
		}
	}
	if importErr == nil && (storedMeta == nil || storedMeta.MetafileVariantID != parsed.MetafileVariantID || artifactRef.MetafileVariantID != parsed.MetafileVariantID) {
		report.Outcome = "published_post_commit_failure"
		report.Blockers = append(report.Blockers, "binding.import_identity_disagrees")
		mappedErr = fmt.Errorf("site metafile import identity disagrees with the fetched response")
	}
	if mappedErr != nil {
		return a.finishSiteMetafileFetch(*output, report, mappedErr)
	}
	return a.writeSiteMetafileFetchReport(*output, report)
}

func (a *app) siteMetafileFetchHelp() {
	fmt.Fprint(a.stdout, `Usage:
  ptctl site metafile fetch --cookie-stdin --acknowledge-site-effect --metafile-store DIR [flags] SITE REMOTE_ID

This command performs one explicitly acknowledged, effectful torrent-metafile GET
and imports the exact bounded response into an initialized private metafile store.
It never follows redirects, retries, prints raw bytes, or writes a site binding
sidecar. A direct site observation and store durability are reported separately.

Flags:
  --acknowledge-site-effect  acknowledge that the GET may count as a download
  --cookie-stdin             read one Cookie header value from stdin
  --max-metafile-bytes N     response byte limit (default 8 MiB; max 32 MiB)
  --metafile-store DIR       initialized private metafile store
  --output table|json        output format
  --show-absolute-paths      show the store root; object paths remain hidden
  --timeout DURATION         shared fetch/import timeout (default 45s; max 5m)
`)
}

func newSiteMetafileFetchReport(ref domain.TorrentRef, config site.MetafileFetchConfig, fetchLimits site.MetafileFetchLimits, storeLimits metastore.Limits, showAbsolute bool, storeRoot string) siteMetafileFetchReport {
	storeOperation := failedMetafileStoreReport("metafile.store.import", "write_private_metafile_store", storeLimits, "store.not_attempted", showAbsolute, storeRoot, "")
	return siteMetafileFetchReport{
		kind: "site.metafile.fetch", Outcome: "blocked",
		Effect:          []string{site.MetafileFetchEffect, "write_private_metafile_store"},
		Acknowledgement: siteMetafileAcknowledgement{Provided: true, Scope: "one_remote_metafile_get"},
		Site:            siteMetafileSiteSummary{Ref: ref, ExpectedOrigin: config.Origin, ExpectedRoute: config.RouteID},
		Binding:         siteMetafileBindingSummary{Status: "declared_unbound", EvidenceLevel: "declared", EvidenceBasis: []string{"user_supplied_site_reference"}, BlockerCodes: []string{"binding.exact_variant_not_established"}},
		Request:         siteMetafileRequestSummary{Status: "not_started", Receipt: site.MetafileFetchReceipt{Effect: site.MetafileFetchEffect, Ref: ref, Origin: config.Origin, RouteID: config.RouteID, Limits: fetchLimits}},
		StoreOperation:  storeOperation,
		Blockers:        []string{}, Issues: []string{},
		Warnings: []string{"the site observation is a direct non-atomic HTTPS observation, not a site signature", "no persistent site-to-variant binding is written", "the private metafile store is permission-isolated, not encrypted"},
	}
}

func siteFetchStoreOperation(store *metastore.Store, meta *metafile.MetaInfo, ref metastore.ArtifactRef, receipt metastore.ImportReceipt, importErr error, limits metastore.Limits, showAbsolute bool, storeRoot string) (metafileStoreReport, string, string, error) {
	if importErr == nil {
		status, outcome, publication := "stored", "stored", "confirmed_this_invocation"
		if receipt.AlreadyPresent {
			status, outcome, publication = "already_present", "already_present", "historical_publication_unobservable"
		}
		artifact := artifactSummary(ref, status)
		return successfulMetafileStoreReport("metafile.store.import", outcome, receipt.Effect, receipt.WritesPerformed, receipt.Store, &artifact, meta, publication, limits, receipt.BytesConsumed, showAbsolute, storeRoot, ""), outcome, "", nil
	}
	if errors.Is(importErr, metastore.ErrDurabilityUnconfirmed) {
		artifact := artifactSummary(ref, "published_durability_unconfirmed")
		artifact.ExactOriginalBytes = false
		report := successfulMetafileStoreReport("metafile.store.import", "published_durability_unconfirmed", receipt.Effect, receipt.WritesPerformed, receipt.Store, &artifact, meta, "published_durability_unconfirmed", limits, receipt.BytesConsumed, showAbsolute, storeRoot, "")
		report.Assurance.DigestVerified = false
		report.Assurance.PrivacyEnforced = false
		report.Blockers = append(report.Blockers, "store.durability_unconfirmed")
		report.Issues = append(report.Issues, "store.durability_unconfirmed")
		return report, "published_durability_unconfirmed", "store.durability_unconfirmed", importErr
	}
	if receipt.WritesPerformed > 0 && ref.ID != "" {
		status, outcome, code := "published_post_commit_failure", "published_post_commit_failure", "store.post_commit_failure"
		if errors.Is(importErr, metastore.ErrCorruptArtifact) {
			status, outcome, code = "published_integrity_failed", "integrity_failed", "artifact.corrupt"
		}
		artifact := artifactSummary(ref, status)
		artifact.ExactOriginalBytes = false
		report := successfulMetafileStoreReport("metafile.store.import", outcome, receipt.Effect, receipt.WritesPerformed, receipt.Store, &artifact, meta, "published_post_commit_failure", limits, receipt.BytesConsumed, showAbsolute, storeRoot, "")
		report.Assurance.DigestVerified = false
		report.Assurance.PrivacyEnforced = false
		report.Blockers = append(report.Blockers, code)
		report.Issues = append(report.Issues, code)
		if errors.Is(importErr, metastore.ErrCorruptArtifact) {
			return report, outcome, code, &integrityErr{message: "published metafile failed final digest or parse validation"}
		}
		return report, outcome, code, importErr
	}
	code, outcome := "store.import_failed", "blocked"
	if errors.Is(importErr, metastore.ErrInvalidMetafile) {
		code, outcome = "artifact.invalid_metafile", "integrity_failed"
	} else if errors.Is(importErr, metastore.ErrCorruptArtifact) {
		code, outcome = "artifact.corrupt", "integrity_failed"
	}
	report := failedMetafileStoreReport("metafile.store.import", "write_private_metafile_store", limits, code, showAbsolute, storeRoot, "")
	report.Outcome = outcome
	report.Store = storeSummary(store.Info(), showAbsolute)
	report.Used.ArtifactBytes = receipt.BytesConsumed
	if strings.HasPrefix(code, "artifact.") {
		return report, outcome, code, &integrityErr{message: "metafile fetch import failed strict validation or found a corrupt stored artifact"}
	}
	return report, outcome, code, importErr
}

func readFetchedMetafile(fetched *site.FetchedMetafile, maxBytes int64) ([]byte, error) {
	reader, err := fetched.Reader()
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil || int64(len(raw)) > maxBytes || int64(len(raw)) != fetched.SizeBytes() {
		return nil, fmt.Errorf("fetched metafile authority is inconsistent")
	}
	return raw, nil
}

func readEffectfulSiteCredential(reader io.Reader) (site.Credential, error) {
	if err := rejectTTYSecret(reader); err != nil {
		return site.Credential{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, (64<<10)+3))
	if err != nil {
		return site.Credential{}, fmt.Errorf("read site credential failed")
	}
	if bytes.HasSuffix(raw, []byte("\r\n")) {
		raw = raw[:len(raw)-2]
	} else if bytes.HasSuffix(raw, []byte("\n")) {
		raw = raw[:len(raw)-1]
	}
	if len(raw) == 0 || len(raw) > 64<<10 || raw[0] == ' ' || raw[len(raw)-1] == ' ' || raw[0] == '\t' || raw[len(raw)-1] == '\t' {
		return site.Credential{}, fmt.Errorf("site credential is not a canonical Cookie header value")
	}
	for _, value := range raw {
		if value < 0x20 || value > 0x7e {
			return site.Credential{}, fmt.Errorf("site credential is not a canonical ASCII Cookie header value")
		}
	}
	return site.NewCookieCredential(string(raw))
}

func siteMetafileStopReason(ctx context.Context, err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return "context_cancelled"
	}
	return "site.fetch_failed"
}

func siteMetafileFetchPublicError(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return fmt.Errorf("site metafile fetch was canceled")
	}
	return fmt.Errorf("site metafile fetch failed")
}

func canonicalSiteDescriptorOrigin(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("site descriptor origin is invalid")
	}
	return "https://" + parsed.Host, nil
}

func validateSuccessfulMetafileFetch(ref domain.TorrentRef, config site.MetafileFetchConfig, limits site.MetafileFetchLimits, fetched *site.FetchedMetafile, receipt site.MetafileFetchReceipt, requestsMade int) error {
	if fetched == nil || config.Validate() != nil || receipt.Effect != site.MetafileFetchEffect || receipt.Ref != ref || receipt.Origin != config.Origin || receipt.RouteID != config.RouteID || receipt.Limits != limits || receipt.StopReason != "" || !receipt.Complete {
		return fmt.Errorf("fetch receipt identity is invalid")
	}
	if requestsMade != 1 || receipt.Used.RequestsAttempted != 1 || receipt.Used.AutomaticRetries != 0 || receipt.Used.RedirectsFollowed != 0 {
		return fmt.Errorf("fetch request accounting is invalid")
	}
	if receipt.ObservedAtStart.IsZero() || receipt.ObservedAtEnd.IsZero() || receipt.ObservedAtEnd.Before(receipt.ObservedAtStart) {
		return fmt.Errorf("fetch observation interval is invalid")
	}
	if !receipt.Used.ResponseBytesKnown || receipt.Used.ResponseBytesRead <= 0 || receipt.Used.ResponseBytesRead != fetched.SizeBytes() || receipt.Used.ResponseBytesRead > limits.MaxResponseBytes {
		return fmt.Errorf("fetch response accounting is invalid")
	}
	if !fetched.MatchesReceipt(receipt) {
		return fmt.Errorf("fetch response authority disagrees with its receipt")
	}
	return nil
}

func publicMetafileFetchReceipt(ref domain.TorrentRef, config site.MetafileFetchConfig, limits site.MetafileFetchLimits, receipt site.MetafileFetchReceipt, includeAuthority bool) site.MetafileFetchReceipt {
	public := site.MetafileFetchReceipt{
		Effect: site.MetafileFetchEffect, Ref: ref, Origin: config.Origin, RouteID: config.RouteID, Limits: limits,
		Used: site.MetafileFetchUsage{
			RequestsAttempted:  boundedFetchCounter(receipt.Used.RequestsAttempted, limits.MaxRequests+1),
			AutomaticRetries:   boundedFetchCounter(receipt.Used.AutomaticRetries, limits.MaxRequests+1),
			RedirectsFollowed:  boundedFetchCounter(receipt.Used.RedirectsFollowed, limits.MaxRequests+1),
			ResponseBytesRead:  boundedFetchBytes(receipt.Used.ResponseBytesRead, limits.MaxResponseBytes+1),
			ResponseBytesKnown: receipt.Used.ResponseBytesKnown && receipt.Used.ResponseBytesRead >= 0 && receipt.Used.ResponseBytesRead <= limits.MaxResponseBytes+1,
		},
		StopReason: publicMetafileStopReason(receipt.StopReason),
	}
	if !receipt.ObservedAtStart.IsZero() && !receipt.ObservedAtEnd.IsZero() && !receipt.ObservedAtEnd.Before(receipt.ObservedAtStart) {
		public.ObservedAtStart = receipt.ObservedAtStart.UTC()
		public.ObservedAtEnd = receipt.ObservedAtEnd.UTC()
	}
	if includeAuthority {
		public.Complete = receipt.Complete
		public.StopReason = ""
	}
	return public
}

func boundedFetchCounter(value, maximum int) int {
	if value < 0 {
		return 0
	}
	if value > maximum {
		return maximum
	}
	return value
}

func boundedFetchBytes(value, maximum int64) int64 {
	if value < 0 {
		return 0
	}
	if value > maximum {
		return maximum
	}
	return value
}

func publicMetafileStopReason(value string) string {
	switch value {
	case "invalid_limits", "invalid_reference", "context_done", "session_closed", "request_budget_exhausted", "request_accounting_invalid", "site_request_failed", "authentication_required", "rate_limited", "redirect_rejected", "http_status_rejected", "empty_response", "challenge_response", "unrecognized_response", "content_type_rejected", "response_authority_invalid", "context_cancelled":
		return value
	case "":
		return ""
	default:
		return "site.fetch_failed"
	}
}

func (a *app) finishSiteMetafileFetch(output string, report siteMetafileFetchReport, operationErr error) error {
	if writeErr := a.writeSiteMetafileFetchReport(output, report); writeErr != nil {
		return writeErr
	}
	return operationErr
}

func (a *app) writeSiteMetafileFetchReport(output string, report siteMetafileFetchReport) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	return writeSiteMetafileFetchHuman(a.stdout, report)
}

func writeSiteMetafileFetchHuman(out io.Writer, report siteMetafileFetchReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nSITE EFFECT ACKNOWLEDGED\t%t\n", terminalSafe(report.Outcome), terminalSafe(strings.Join(report.Effect, ",")), report.WritesPerformed, report.Acknowledgement.Provided)
	fmt.Fprintln(w, "\nBLOCKERS")
	if len(report.Blockers) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, blocker := range report.Blockers {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(blocker))
	}
	fmt.Fprintf(w, "\nSITE\t%s\nREMOTE ID\t%s\nEXPECTED ORIGIN\t%s\nEXPECTED ROUTE\t%s\nBINDING\t%s\nEVIDENCE\t%s\nREQUEST\t%s\nREQUESTS MADE\t%d\nAUTOMATIC RETRIES\t%d\nREDIRECTS FOLLOWED\t%d\n", terminalSafe(report.Site.Ref.SiteID), terminalSafe(report.Site.Ref.RemoteID), terminalSafe(report.Site.ExpectedOrigin), terminalSafe(report.Site.ExpectedRoute), terminalSafe(report.Binding.Status), terminalSafe(report.Binding.EvidenceLevel), terminalSafe(report.Request.Status), report.Request.RequestsMade, report.Request.Retries, report.Request.Redirects)
	if report.Request.Receipt.StopReason != "" {
		fmt.Fprintf(w, "REQUEST STOP\t%s\n", terminalSafe(report.Request.Receipt.StopReason))
	}
	if report.Binding.Observation != nil {
		fmt.Fprintf(w, "OBSERVED VARIANT\t%s\nOBSERVED ORIGIN\t%s\nADAPTER ROUTE\t%s\n", terminalSafe(report.Binding.Observation.MetafileVariantID), terminalSafe(report.Binding.Observation.Origin), terminalSafe(report.Binding.Observation.RouteID))
	}
	store := report.StoreOperation
	fmt.Fprintf(w, "\nSTORE OUTCOME\t%s\nSTORE EFFECT\t%s\nSTORE WRITES\t%d\nPUBLICATION ASSURANCE\t%s\nDIGEST VERIFIED\t%t\nSTRICTLY PARSED\t%t\nATOMIC NO-CLOBBER\t%t\nDURABILITY CONFIRMED\t%t\nPRIVACY ENFORCED\t%t\nENCRYPTION\t%s\n", terminalSafe(store.Outcome), terminalSafe(store.Effect), store.WritesPerformed, terminalSafe(store.Assurance.Publication), store.Assurance.DigestVerified, store.Assurance.StrictlyParsed, store.Assurance.AtomicNoClobber, store.Assurance.DurabilityConfirmed, store.Assurance.PrivacyEnforced, terminalSafe(store.Assurance.Encryption))
	fmt.Fprintf(w, "STORE ID\t%s\nSTORE FORMAT\t%s\nPRIVACY\t%s\nCOMMIT ASSURANCE\t%s\n", terminalSafe(store.Store.ID), terminalSafe(store.Store.Format), terminalSafe(store.Store.Privacy), terminalSafe(store.Store.CommitAssurance))
	if store.Paths.Store != "" {
		fmt.Fprintf(w, "STORE PATH\t%s\n", terminalSafe(store.Paths.Store))
	}
	if store.Artifact != nil {
		fmt.Fprintf(w, "ARTIFACT\t%s\nARTIFACT STATUS\t%s\nMETAFILE VARIANT\t%s\nVERSION\t%s\nARTIFACT BYTES\t%d\n", terminalSafe(store.Artifact.ID), terminalSafe(store.Artifact.Status), terminalSafe(store.Artifact.MetafileVariantID), terminalSafe(store.Artifact.Version), store.Artifact.SizeBytes)
		if store.Artifact.InfoHashV1 != "" {
			fmt.Fprintf(w, "INFOHASH V1\t%s\n", terminalSafe(store.Artifact.InfoHashV1))
		}
		if store.Artifact.InfoHashV2 != "" {
			fmt.Fprintf(w, "INFOHASH V2\t%s\n", terminalSafe(store.Artifact.InfoHashV2))
		}
	}
	if store.Metafile != nil {
		fmt.Fprintf(w, "METAFILE VERSION\t%s\nPRIVATE\t%t\nMULTI-FILE\t%t\nCONTENT BYTES\t%d\nFILES\t%d\n", terminalSafe(store.Metafile.Version), store.Metafile.Private, store.Metafile.MultiFile, store.Metafile.TotalLength, store.Metafile.FileCount)
	}
	fmt.Fprintf(w, "\nREQUEST LIMIT\t%d\nRESPONSE LIMIT\t%d bytes\nHEADER LIMIT\t%d bytes\n", report.Request.Receipt.Limits.MaxRequests, report.Request.Receipt.Limits.MaxResponseBytes, report.Request.Receipt.Limits.MaxResponseHeaderBytes)
	if report.Request.Receipt.Used.ResponseBytesKnown {
		fmt.Fprintf(w, "RESPONSE USED\t%d bytes\n", report.Request.Receipt.Used.ResponseBytesRead)
	} else {
		fmt.Fprintln(w, "RESPONSE USED\tunknown")
	}
	fmt.Fprintf(w, "STORE LIMIT\t%d bytes\n", store.Limits.MaxArtifactBytes)
	if store.Used.ArtifactBytesKnown {
		fmt.Fprintf(w, "STORE USED\t%d bytes\n", store.Used.ArtifactBytes)
	} else {
		fmt.Fprintln(w, "STORE USED\tunknown")
	}
	fmt.Fprintln(w, "\nISSUES")
	if len(report.Issues) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, issue := range report.Issues {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(issue))
	}
	fmt.Fprintln(w, "\nWARNINGS")
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "-\t%s\n", terminalSafe(warning))
	}
	return w.Flush()
}
