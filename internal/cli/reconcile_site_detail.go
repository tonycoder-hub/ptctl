package cli

import (
	"context"

	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

func readReconciliationSiteDetail(ctx context.Context, reader site.TorrentDetailReader, config site.TorrentDetailConfig, ref domain.TorrentRef, credential site.Credential) reconcile.SiteDetailSelection {
	selection := reconcile.SiteDetailSelection{Requested: true, Config: config}
	limits := site.DefaultTorrentDetailLimits()
	if reader == nil || config.Validate() != nil || reader.ValidateTorrentDetailRef(ref) != nil {
		selection.StopReason = "site_detail_receipt_inconsistent"
		return selection
	}
	session, err := reader.OpenTorrentDetailSession(ctx, credential)
	if err != nil || session == nil {
		selection.StopReason = "session_open_failed"
		return selection
	}
	observed, receipt, readErr := session.ReadTorrentDetail(ctx, ref, limits)
	selection.Receipt = receipt
	selection.RequestsMade = boundedFetchCounter(session.RequestsMade(), limits.MaxRequests+1)
	closeErr := session.Close()
	if readErr != nil {
		selection.StopReason = safeTorrentDetailStopReason(receipt.StopReason)
		if selection.StopReason == "" {
			selection.StopReason = "site_detail_observation_incomplete"
		}
		return selection
	}
	if closeErr != nil {
		selection.StopReason = "session_close_failed"
		return selection
	}
	if validateSuccessfulTorrentDetail(ref, config, limits, observed, receipt, selection.RequestsMade) != nil {
		selection.StopReason = "receipt_inconsistent"
		return selection
	}
	selection.Observed = observed
	return selection
}
