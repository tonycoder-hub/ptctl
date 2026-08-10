package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

type siteDetailReport struct {
	kind            string                `json:"-"`
	Outcome         string                `json:"outcome"`
	Effect          string                `json:"effect"`
	WritesPerformed int                   `json:"writes_performed"`
	Scope           siteDetailScope       `json:"scope"`
	Observation     *domain.TorrentDetail `json:"observation,omitempty"`
	Request         siteDetailRequest     `json:"request"`
	Blockers        []siteDetailFinding   `json:"blockers"`
	Warnings        []string              `json:"warnings"`
}

type siteDetailScope struct {
	Ref     domain.TorrentRef `json:"ref"`
	Origin  string            `json:"origin"`
	RouteID string            `json:"route_id"`
}

type siteDetailRequest struct {
	Status       string                    `json:"status"`
	RequestsMade int                       `json:"requests_made"`
	Receipt      site.TorrentDetailReceipt `json:"receipt"`
}

type siteDetailFinding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (a *app) siteDetail(args []string) error {
	fs := newFlagSet("site detail")
	output := fs.String("output", "table", "table or json")
	cookieStdin := fs.Bool("cookie-stdin", false, "read session Cookie header value from stdin")
	if err := fs.Parse(args); err != nil {
		return usageError("site detail: %v", err)
	}
	if fs.NArg() != 2 {
		return usageError("site detail requires SITE and REMOTE_ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if !*cookieStdin {
		return usageError("--cookie-stdin is required; credentials are never accepted in argv")
	}
	adapter, ok := a.registry.Get(fs.Arg(0))
	if !ok {
		return fmt.Errorf("unknown site %q", fs.Arg(0))
	}
	descriptor := adapter.Descriptor()
	if !descriptor.Supports(domain.CapabilityDetail) {
		return fmt.Errorf("site %q does not declare capability %q", descriptor.ID, domain.CapabilityDetail)
	}
	if !descriptor.SupportsAuth(domain.AuthMethodCookieHeader) {
		return fmt.Errorf("site %q does not support cookie_header authentication", descriptor.ID)
	}
	reader, ok := adapter.(site.TorrentDetailReader)
	if !ok {
		return fmt.Errorf("site %q declares %q but does not implement its typed port", descriptor.ID, domain.CapabilityDetail)
	}
	config, err := reader.TorrentDetailConfig()
	if err != nil || config.Validate() != nil {
		return fmt.Errorf("site torrent detail configuration is unavailable")
	}
	ref := domain.TorrentRef{SiteID: descriptor.ID, RemoteID: fs.Arg(1)}
	if err := reader.ValidateTorrentDetailRef(ref); err != nil {
		return usageError("site detail remote reference is invalid")
	}
	limits := site.DefaultTorrentDetailLimits()
	report := newSiteDetailReport(ref, config, limits)

	credential, err := readCredential(a.stdin)
	if err != nil {
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: "credential.unavailable", Message: "the site credential could not be read"})
		return a.finishSiteDetail(*output, report, fmt.Errorf("read site credential failed"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	session, err := reader.OpenTorrentDetailSession(ctx, credential)
	if err != nil {
		report.Request.Status = "incomplete"
		report.Request.Receipt.StopReason = "session_open_failed"
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: "site.session_open_failed", Message: "the bounded site detail session could not be opened"})
		return a.finishSiteDetail(*output, report, fmt.Errorf("open site torrent detail session failed"))
	}
	detail, receipt, readErr := session.ReadTorrentDetail(ctx, ref, limits)
	report.Request.RequestsMade = boundedFetchCounter(session.RequestsMade(), limits.MaxRequests+1)
	report.Request.Receipt = publicTorrentDetailReceipt(ref, config, limits, receipt)
	closeErr := session.Close()
	if readErr != nil {
		stop := safeTorrentDetailStopReason(receipt.StopReason)
		if stop == "" {
			stop = "site.detail_failed"
		}
		report.Request.Status = "incomplete"
		report.Request.Receipt.Complete = false
		report.Request.Receipt.StopReason = stop
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: stop, Message: "the site torrent detail observation did not complete"})
		return a.finishSiteDetail(*output, report, siteDetailPublicError(ctx, readErr))
	}
	if closeErr != nil {
		report.Request.Status = "incomplete"
		report.Request.Receipt.Complete = false
		report.Request.Receipt.StopReason = "session_close_failed"
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: "site.session_close_failed", Message: "the site detail session could not be closed cleanly"})
		return a.finishSiteDetail(*output, report, fmt.Errorf("close site torrent detail session failed"))
	}
	if err := validateSuccessfulTorrentDetail(ref, config, limits, detail, receipt, report.Request.RequestsMade); err != nil {
		report.Request.Status = "incomplete"
		report.Request.Receipt.Complete = false
		report.Request.Receipt.StopReason = "receipt_inconsistent"
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: "site.receipt_inconsistent", Message: "the adapter's detail observation and request receipt did not agree"})
		return a.finishSiteDetail(*output, report, fmt.Errorf("site torrent detail receipt was inconsistent"))
	}
	report.Outcome = "observed"
	report.Request.Status = "complete"
	report.Observation = &detail
	return a.writeSiteDetailReport(*output, report)
}

func newSiteDetailReport(ref domain.TorrentRef, config site.TorrentDetailConfig, limits site.TorrentDetailLimits) siteDetailReport {
	return siteDetailReport{
		kind:            "site.torrent.detail",
		Outcome:         "incomplete",
		Effect:          site.TorrentDetailReadEffect,
		WritesPerformed: 0,
		Scope:           siteDetailScope{Ref: ref, Origin: config.Origin, RouteID: config.RouteID},
		Request: siteDetailRequest{
			Status: "not_started",
			Receipt: site.TorrentDetailReceipt{
				Effect: site.TorrentDetailReadEffect,
				Ref:    ref, Origin: config.Origin, RouteID: config.RouteID, Limits: limits,
			},
		},
		Blockers: []siteDetailFinding{},
		Warnings: []string{
			"site detail fields are live site claims, not metafile identity or storage-content proof",
			"the single request is tracker-visible; the TJUPT route deliberately omits the view-counting hit parameter",
		},
	}
}

func validateSuccessfulTorrentDetail(ref domain.TorrentRef, config site.TorrentDetailConfig, limits site.TorrentDetailLimits, detail domain.TorrentDetail, receipt site.TorrentDetailReceipt, requestsMade int) error {
	if config.Validate() != nil || limits.Validate() != nil || detail.Ref != ref || receipt.Effect != site.TorrentDetailReadEffect || receipt.Ref != ref ||
		receipt.Origin != config.Origin || receipt.RouteID != config.RouteID || receipt.Limits != limits || !receipt.Complete || receipt.StopReason != "" ||
		receipt.ObservedAtStart.IsZero() || receipt.ObservedAtEnd.IsZero() || receipt.ObservedAtEnd.Before(receipt.ObservedAtStart) ||
		requestsMade != 1 || receipt.Used.RequestsAttempted != 1 || receipt.Used.AutomaticRetries != 0 || receipt.Used.RedirectsFollowed != 0 ||
		!receipt.Used.ResponseBytesKnown || receipt.Used.ResponseBytesRead <= 0 || receipt.Used.ResponseBytesRead > limits.MaxResponseBytes ||
		!validPublicDetailTitle(detail.DisplayTitle) || detail.Seeders != nil && *detail.Seeders < 0 || detail.Leechers != nil && *detail.Leechers < 0 {
		return fmt.Errorf("detail receipt is invalid")
	}
	wantBasis := []string{site.DetailBasisAuthenticated, site.DetailBasisExactRef, site.DetailBasisOneRequest, site.DetailBasisSiteClaimOnly}
	if detail.DownloadReferenceObserved {
		wantBasis = append(wantBasis, site.DetailBasisDownloadRef)
	}
	if len(detail.EvidenceBasis) != len(wantBasis) {
		return fmt.Errorf("detail evidence is invalid")
	}
	for index := range wantBasis {
		if detail.EvidenceBasis[index] != wantBasis[index] {
			return fmt.Errorf("detail evidence is invalid")
		}
	}
	return nil
}

func validPublicDetailTitle(value string) bool {
	if value == "" || len(value) > 4<<10 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func publicTorrentDetailReceipt(ref domain.TorrentRef, config site.TorrentDetailConfig, limits site.TorrentDetailLimits, receipt site.TorrentDetailReceipt) site.TorrentDetailReceipt {
	result := site.TorrentDetailReceipt{
		Effect: site.TorrentDetailReadEffect, Ref: ref, Origin: config.Origin, RouteID: config.RouteID, Limits: limits,
		Complete: receipt.Complete,
		Used: site.TorrentDetailUsage{
			RequestsAttempted:  boundedFetchCounter(receipt.Used.RequestsAttempted, limits.MaxRequests+1),
			AutomaticRetries:   boundedFetchCounter(receipt.Used.AutomaticRetries, limits.MaxRequests+1),
			RedirectsFollowed:  boundedFetchCounter(receipt.Used.RedirectsFollowed, limits.MaxRequests+1),
			ResponseBytesRead:  boundedFetchBytes(receipt.Used.ResponseBytesRead, limits.MaxResponseBytes+1),
			ResponseBytesKnown: receipt.Used.ResponseBytesKnown,
		},
		StopReason: safeTorrentDetailStopReason(receipt.StopReason),
	}
	if !receipt.ObservedAtStart.IsZero() && !receipt.ObservedAtEnd.IsZero() && !receipt.ObservedAtEnd.Before(receipt.ObservedAtStart) {
		result.ObservedAtStart = receipt.ObservedAtStart.UTC()
		result.ObservedAtEnd = receipt.ObservedAtEnd.UTC()
	}
	return result
}

func safeTorrentDetailStopReason(value string) string {
	switch value {
	case "invalid_limits", "invalid_reference", "context_done", "session_closed", "request_budget_exhausted", "request_accounting_invalid", "site_request_failed", "authentication_required", "not_found", "rate_limited", "redirect_rejected", "http_status_rejected", "empty_response", "challenge_response", "unrecognized_response", "content_type_rejected", "invalid_text_encoding", "response_accounting_invalid", "session_open_failed", "session_close_failed", "receipt_inconsistent":
		return value
	case "":
		return ""
	default:
		return "site.detail_failed"
	}
}

func siteDetailPublicError(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("site torrent detail read canceled: %w", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("site torrent detail read timed out: %w", context.DeadlineExceeded)
	}
	return fmt.Errorf("site torrent detail read failed")
}

func (a *app) finishSiteDetail(output string, report siteDetailReport, operationErr error) error {
	if writeErr := a.writeSiteDetailReport(output, report); writeErr != nil {
		return writeErr
	}
	return operationErr
}

func (a *app) writeSiteDetailReport(output string, report siteDetailReport) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	return writeSiteDetailHuman(a.stdout, report)
}

func writeSiteDetailHuman(out io.Writer, report siteDetailReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\n", terminalSafe(report.Outcome), terminalSafe(report.Effect), report.WritesPerformed)
	fmt.Fprintln(w, "\nBLOCKERS")
	if len(report.Blockers) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, blocker := range report.Blockers {
		fmt.Fprintf(w, "%s\t%s\n", terminalSafe(blocker.Code), terminalSafe(blocker.Message))
	}
	fmt.Fprintf(w, "\nSITE\t%s\nREMOTE ID\t%s\nORIGIN\t%s\nROUTE\t%s\n", terminalSafe(report.Scope.Ref.SiteID), terminalSafe(report.Scope.Ref.RemoteID), terminalSafe(report.Scope.Origin), terminalSafe(report.Scope.RouteID))
	if report.Observation != nil {
		detail := report.Observation
		fmt.Fprintf(w, "\nDETAIL STATUS\tobserved\nDISPLAY TITLE\t%s\nDOWNLOAD REFERENCE\t%t\n", terminalSafe(detail.DisplayTitle), detail.DownloadReferenceObserved)
		if detail.Seeders != nil {
			fmt.Fprintf(w, "SEEDERS\t%d\n", *detail.Seeders)
		}
		if detail.Leechers != nil {
			fmt.Fprintf(w, "LEECHERS\t%d\n", *detail.Leechers)
		}
		fmt.Fprintf(w, "EVIDENCE BASIS\t%s\n", terminalSafe(strings.Join(detail.EvidenceBasis, ",")))
	}
	receipt := report.Request.Receipt
	fmt.Fprintf(w, "\nREQUEST STATUS\t%s\nREQUESTS MADE\t%d\nAUTOMATIC RETRIES\t%d\nREDIRECTS FOLLOWED\t%d\nRESPONSE BYTES\t%d\nRESPONSE BYTES KNOWN\t%t\n", terminalSafe(report.Request.Status), report.Request.RequestsMade, receipt.Used.AutomaticRetries, receipt.Used.RedirectsFollowed, receipt.Used.ResponseBytesRead, receipt.Used.ResponseBytesKnown)
	if receipt.StopReason != "" {
		fmt.Fprintf(w, "REQUEST STOP\t%s\n", terminalSafe(receipt.StopReason))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(out, "warning: %s\n", terminalSafe(warning))
	}
	return nil
}
