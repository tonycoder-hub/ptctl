package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

type siteBonusReviewReport struct {
	kind            string                   `json:"-"`
	Outcome         string                   `json:"outcome"`
	Effect          string                   `json:"effect"`
	WritesPerformed int                      `json:"writes_performed"`
	FormSubmissions int                      `json:"form_submissions"`
	Scope           siteBonusReviewScope     `json:"scope"`
	Review          *domain.BonusOfferReview `json:"review,omitempty"`
	Request         siteBonusReviewRequest   `json:"request"`
	Assurance       siteBonusReviewAssurance `json:"assurance"`
	Blockers        []siteDetailFinding      `json:"blockers"`
	Warnings        []string                 `json:"warnings"`
}

type siteBonusReviewScope struct {
	SiteID   string `json:"site_id"`
	Selector string `json:"selector"`
	Origin   string `json:"origin"`
	RouteID  string `json:"route_id"`
}

type siteBonusReviewRequest struct {
	Status       string                  `json:"status"`
	RequestsMade int                     `json:"requests_made"`
	Receipt      site.BonusReviewReceipt `json:"receipt"`
}

type siteBonusReviewAssurance struct {
	ProcessLocalAuthority bool   `json:"process_local_authority"`
	SerializedAuthority   bool   `json:"serialized_authority"`
	FormSubmitted         bool   `json:"form_submitted"`
	EvidenceLevel         string `json:"evidence_level"`
}

func (a *app) siteBonusReview(args []string) error {
	fs := newFlagSet("site bonus review")
	output := fs.String("output", "table", "table or json")
	cookieStdin := fs.Bool("cookie-stdin", false, "read session Cookie header value from stdin")
	if err := fs.Parse(args); err != nil {
		return usageError("site bonus review: %v", err)
	}
	if fs.NArg() != 2 {
		return usageError("site bonus review requires SITE and OPTION")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if !*cookieStdin {
		return usageError("--cookie-stdin is required; credentials are never accepted in argv")
	}
	adapter, ok := a.registry.Get(fs.Arg(0))
	if !ok {
		return usageError("unknown site %q", fs.Arg(0))
	}
	descriptor := adapter.Descriptor()
	if !descriptor.Supports(domain.CapabilityBonusReview) {
		return usageError("site %q does not declare capability %q", descriptor.ID, domain.CapabilityBonusReview)
	}
	if !descriptor.SupportsAuth(domain.AuthMethodCookieHeader) {
		return usageError("site %q does not support cookie_header authentication", descriptor.ID)
	}
	reader, ok := adapter.(site.BonusReviewReader)
	if !ok {
		return usageError("site %q declares %q but does not implement its typed port", descriptor.ID, domain.CapabilityBonusReview)
	}
	config, err := reader.BonusReviewConfig()
	if err != nil || config.Validate() != nil {
		return usageError("site bonus review configuration is unavailable")
	}
	selector := fs.Arg(1)
	if err := reader.ValidateBonusReviewSelector(selector); err != nil {
		return usageError("site bonus option is invalid")
	}
	limits := site.DefaultBonusReviewLimits()
	if err := limits.Validate(); err != nil {
		return usageError("site bonus review limits are invalid")
	}
	report := newSiteBonusReviewReport(descriptor.ID, selector, config, limits)

	credential, err := readCredential(a.stdin)
	if err != nil {
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: "credential.unavailable", Message: "the site credential could not be read"})
		return a.finishSiteBonusReview(*output, report, fmt.Errorf("read site credential failed"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	session, err := reader.OpenBonusReviewSession(ctx, credential)
	if err != nil || session == nil {
		report.Request.Status = "incomplete"
		report.Request.Receipt.StopReason = "session_open_failed"
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: "site.session_open_failed", Message: "the bounded site bonus review session could not be opened"})
		return a.finishSiteBonusReview(*output, report, fmt.Errorf("open site bonus review session failed"))
	}
	observed, receipt, readErr := session.ReadBonusReview(ctx, selector, limits)
	report.Request.RequestsMade = boundedFetchCounter(session.RequestsMade(), limits.MaxRequests+1)
	report.Request.Receipt = publicBonusReviewReceipt(descriptor.ID, selector, config, limits, receipt)
	closeErr := session.Close()
	if readErr != nil {
		stop := safeBonusReviewStopReason(receipt.StopReason)
		if stop == "" {
			stop = "site.bonus_review_failed"
		}
		report.Request.Status = "incomplete"
		report.Request.Receipt.Complete = false
		report.Request.Receipt.StopReason = stop
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: stop, Message: "the site bonus offer review did not complete"})
		return a.finishSiteBonusReview(*output, report, siteBonusReviewPublicError(ctx, readErr))
	}
	if closeErr != nil {
		report.Request.Status = "incomplete"
		report.Request.Receipt.Complete = false
		report.Request.Receipt.StopReason = "session_close_failed"
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: "site.session_close_failed", Message: "the site bonus review session could not be closed cleanly"})
		return a.finishSiteBonusReview(*output, report, fmt.Errorf("close site bonus review session failed"))
	}
	if err := validateSuccessfulBonusReview(descriptor.ID, selector, config, limits, observed, receipt, report.Request.RequestsMade); err != nil {
		report.Request.Status = "incomplete"
		report.Request.Receipt.Complete = false
		report.Request.Receipt.StopReason = "receipt_inconsistent"
		report.Blockers = append(report.Blockers, siteDetailFinding{Code: "site.receipt_inconsistent", Message: "the adapter's bonus observation and request receipt did not agree"})
		return a.finishSiteBonusReview(*output, report, fmt.Errorf("site bonus review receipt was inconsistent"))
	}
	review := observed.PublicCopy()
	report.Outcome = "reviewed"
	report.Request.Status = "complete"
	report.Review = &review
	report.Assurance.ProcessLocalAuthority = true
	return a.writeSiteBonusReviewReport(*output, report)
}

func newSiteBonusReviewReport(siteID, selector string, config site.BonusReviewConfig, limits site.BonusReviewLimits) siteBonusReviewReport {
	return siteBonusReviewReport{
		kind:            "site.bonus.offer_review",
		Outcome:         "incomplete",
		Effect:          site.BonusReviewReadEffect,
		WritesPerformed: 0,
		FormSubmissions: 0,
		Scope:           siteBonusReviewScope{SiteID: siteID, Selector: selector, Origin: config.Origin, RouteID: config.RouteID},
		Request: siteBonusReviewRequest{
			Status: "not_started",
			Receipt: site.BonusReviewReceipt{
				Effect: site.BonusReviewReadEffect, SiteID: siteID, Selector: selector,
				Origin: config.Origin, RouteID: config.RouteID, Limits: limits,
			},
		},
		Assurance: siteBonusReviewAssurance{
			ProcessLocalAuthority: false,
			SerializedAuthority:   false,
			FormSubmitted:         false,
			EvidenceLevel:         "live_site_claim",
		},
		Blockers: []siteDetailFinding{},
		Warnings: []string{
			"the review is a live site claim and does not authorize or submit a purchase or redemption form",
			"the review ID excludes balance and time, is not replay authority, and must be reproduced from a fresh page before any future write",
			"the action route and retained text are static HTML claims; JavaScript and browser rendering/runtime behavior are not executed",
			"the single request is tracker-visible",
		},
	}
}

func validateSuccessfulBonusReview(siteID, selector string, config site.BonusReviewConfig, limits site.BonusReviewLimits, observed *site.ObservedBonusReview, receipt site.BonusReviewReceipt, requestsMade int) error {
	if observed == nil || !observed.MatchesReceipt(receipt) || !observed.Matches(siteID, selector, config.Origin, config.RouteID) {
		return fmt.Errorf("bonus review authority is invalid")
	}
	review := observed.PublicCopy()
	wantID, err := site.ComputeBonusReviewID(review)
	if err != nil || review.ReviewID != wantID || review.SiteID != siteID || review.Selector != selector ||
		config.Validate() != nil || limits.Validate() != nil || receipt.Effect != site.BonusReviewReadEffect || receipt.SiteID != siteID || receipt.Selector != selector ||
		receipt.Origin != config.Origin || receipt.RouteID != config.RouteID || receipt.Limits != limits || !receipt.Complete || receipt.StopReason != "" ||
		receipt.ObservedAtStart.IsZero() || receipt.ObservedAtEnd.IsZero() || receipt.ObservedAtEnd.Before(receipt.ObservedAtStart) ||
		requestsMade != 1 || receipt.Used.RequestsAttempted != 1 || receipt.Used.AutomaticRetries != 0 || receipt.Used.RedirectsFollowed != 0 ||
		!receipt.Used.ResponseBytesKnown || receipt.Used.ResponseBytesRead <= 0 || receipt.Used.ResponseBytesRead > limits.MaxResponseBytes ||
		receipt.Used.FormsExamined <= 0 || receipt.Used.FormsExamined > limits.MaxForms || receipt.Used.FieldsExamined < 0 || receipt.Used.FieldsExamined > limits.MaxFields ||
		receipt.Used.TokensExamined <= 0 || receipt.Used.TokensExamined > limits.MaxTokens || receipt.Used.VisibleTextBytes < 0 || receipt.Used.VisibleTextBytes > limits.MaxVisibleTextBytes {
		return fmt.Errorf("bonus review receipt is invalid")
	}
	return nil
}

func publicBonusReviewReceipt(siteID, selector string, config site.BonusReviewConfig, limits site.BonusReviewLimits, receipt site.BonusReviewReceipt) site.BonusReviewReceipt {
	result := site.BonusReviewReceipt{
		Effect: site.BonusReviewReadEffect, SiteID: siteID, Selector: selector,
		Origin: config.Origin, RouteID: config.RouteID, Limits: limits,
		Complete: receipt.Complete,
		Used: site.BonusReviewUsage{
			RequestsAttempted:  boundedFetchCounter(receipt.Used.RequestsAttempted, limits.MaxRequests+1),
			AutomaticRetries:   boundedFetchCounter(receipt.Used.AutomaticRetries, limits.MaxRequests+1),
			RedirectsFollowed:  boundedFetchCounter(receipt.Used.RedirectsFollowed, limits.MaxRequests+1),
			ResponseBytesRead:  boundedFetchBytes(receipt.Used.ResponseBytesRead, limits.MaxResponseBytes+1),
			ResponseBytesKnown: receipt.Used.ResponseBytesKnown,
			FormsExamined:      boundedFetchCounter(receipt.Used.FormsExamined, limits.MaxForms+1),
			FieldsExamined:     boundedFetchCounter(receipt.Used.FieldsExamined, limits.MaxFields+1),
			TokensExamined:     boundedFetchCounter(receipt.Used.TokensExamined, limits.MaxTokens+1),
			VisibleTextBytes:   boundedFetchBytes(receipt.Used.VisibleTextBytes, limits.MaxVisibleTextBytes+1),
		},
		StopReason: safeBonusReviewStopReason(receipt.StopReason),
	}
	if !receipt.ObservedAtStart.IsZero() && !receipt.ObservedAtEnd.IsZero() && !receipt.ObservedAtEnd.Before(receipt.ObservedAtStart) {
		result.ObservedAtStart = receipt.ObservedAtStart.UTC()
		result.ObservedAtEnd = receipt.ObservedAtEnd.UTC()
	}
	return result
}

func safeBonusReviewStopReason(value string) string {
	switch value {
	case "invalid_limits", "invalid_selector", "context_done", "session_closed", "request_budget_exhausted", "request_accounting_invalid", "site_request_failed", "authentication_required", "not_found", "rate_limited", "redirect_rejected", "http_status_rejected", "empty_response", "challenge_response", "content_type_rejected", "invalid_text_encoding", "response_accounting_invalid", "selector_not_found", "selector_ambiguous", "token_budget_exceeded", "form_budget_exceeded", "field_budget_exceeded", "visible_text_budget_exceeded", "invalid_html", "ambiguous_form_structure", "unrecognized_response", "unrecognized_offer", "session_open_failed", "session_close_failed", "receipt_inconsistent":
		return value
	case "":
		return ""
	default:
		return "site.bonus_review_failed"
	}
}

func siteBonusReviewPublicError(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("site bonus review canceled: %w", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("site bonus review timed out: %w", context.DeadlineExceeded)
	}
	return fmt.Errorf("site bonus review failed")
}

func (a *app) finishSiteBonusReview(output string, report siteBonusReviewReport, operationErr error) error {
	if writeErr := a.writeSiteBonusReviewReport(output, report); writeErr != nil {
		return writeErr
	}
	return operationErr
}

func (a *app) writeSiteBonusReviewReport(output string, report siteBonusReviewReport) error {
	if output == "json" {
		return writeJSON(a.stdout, report, nil)
	}
	return writeSiteBonusReviewHuman(a.stdout, report)
}

func writeSiteBonusReviewHuman(out io.Writer, report siteBonusReviewReport) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "OUTCOME\t%s\nEFFECT\t%s\nWRITES PERFORMED\t%d\nFORM SUBMISSIONS\t%d\n", terminalSafe(report.Outcome), terminalSafe(report.Effect), report.WritesPerformed, report.FormSubmissions)
	fmt.Fprintln(w, "\nBLOCKERS")
	if len(report.Blockers) == 0 {
		fmt.Fprintln(w, "-\tnone")
	}
	for _, blocker := range report.Blockers {
		fmt.Fprintf(w, "%s\t%s\n", terminalSafe(blocker.Code), terminalSafe(blocker.Message))
	}
	fmt.Fprintf(w, "\nSITE\t%s\nOPTION\t%s\nORIGIN\t%s\nREAD ROUTE\t%s\n", terminalSafe(report.Scope.SiteID), terminalSafe(report.Scope.Selector), terminalSafe(report.Scope.Origin), terminalSafe(report.Scope.RouteID))
	if report.Review != nil {
		review := report.Review
		fmt.Fprintf(w, "\nREVIEW ID\t%s\nFORM SHAPE ID\t%s\nBALANCE\t%s\nAVAILABILITY\t%s\nINPUT MODE\t%s\nACTION METHOD\t%s\nACTION ROUTE\t%s\n", terminalSafe(review.ReviewID), terminalSafe(review.FormShapeID), terminalSafe(valueOrUnknown(review.Balance)), terminalSafe(review.Availability), terminalSafe(review.InputMode), terminalSafe(review.ActionMethod), terminalSafe(review.ActionRouteID))
		for index, column := range review.Columns {
			fmt.Fprintf(w, "OFFER COLUMN %d\t%s\n", index+1, terminalSafe(column))
		}
		fmt.Fprintf(w, "EVIDENCE BASIS\t%s\n", terminalSafe(strings.Join(review.EvidenceBasis, ",")))
	}
	receipt := report.Request.Receipt
	fmt.Fprintf(w, "\nREQUEST STATUS\t%s\nREQUESTS MADE\t%d\nAUTOMATIC RETRIES\t%d\nREDIRECTS FOLLOWED\t%d\nRESPONSE BYTES\t%d\nRESPONSE BYTES KNOWN\t%t\nFORMS EXAMINED\t%d\nFIELDS EXAMINED\t%d\nTOKENS EXAMINED\t%d\nVISIBLE TEXT BYTES\t%d\n", terminalSafe(report.Request.Status), report.Request.RequestsMade, receipt.Used.AutomaticRetries, receipt.Used.RedirectsFollowed, receipt.Used.ResponseBytesRead, receipt.Used.ResponseBytesKnown, receipt.Used.FormsExamined, receipt.Used.FieldsExamined, receipt.Used.TokensExamined, receipt.Used.VisibleTextBytes)
	if receipt.StopReason != "" {
		fmt.Fprintf(w, "REQUEST STOP\t%s\n", terminalSafe(receipt.StopReason))
	}
	fmt.Fprintf(w, "PROCESS-LOCAL AUTHORITY\t%t\nSERIALIZED AUTHORITY\t%t\nFORM SUBMITTED\t%t\nEVIDENCE LEVEL\t%s\n", report.Assurance.ProcessLocalAuthority, report.Assurance.SerializedAuthority, report.Assurance.FormSubmitted, terminalSafe(report.Assurance.EvidenceLevel))
	if err := w.Flush(); err != nil {
		return err
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(out, "warning: %s\n", terminalSafe(warning))
	}
	return nil
}
