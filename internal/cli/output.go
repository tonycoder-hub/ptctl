package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/clientactivate"
	"github.com/tonycoder-hub/ptctl/internal/clientadopt"
	"github.com/tonycoder-hub/ptctl/internal/clientremove"
	"github.com/tonycoder-hub/ptctl/internal/clientstop"
	"github.com/tonycoder-hub/ptctl/internal/domain"
	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/materialize"
	"github.com/tonycoder-hub/ptctl/internal/metafile"
	"github.com/tonycoder-hub/ptctl/internal/reconcile"
	"github.com/tonycoder-hub/ptctl/internal/seed"
	"github.com/tonycoder-hub/ptctl/internal/sourceretire"
	"github.com/tonycoder-hub/ptctl/internal/storage"
)

type envelope struct {
	Schema   string   `json:"schema"`
	Kind     string   `json:"kind"`
	Data     any      `json:"data"`
	Warnings []string `json:"warnings,omitempty"`
}

func writeJSON(out io.Writer, data any, warnings []string) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(envelope{Schema: "ptctl.dev/v1", Kind: jsonKind(data), Data: data, Warnings: warnings})
}

func jsonKind(data any) string {
	switch typed := data.(type) {
	case map[string]string:
		return "version"
	case []domain.SiteDescriptor:
		return "site.descriptor.list"
	case domain.SessionStatus:
		return "site.session"
	case domain.AccountSnapshot:
		return "site.account"
	case []domain.TorrentSummary:
		return "site.torrent.list"
	case siteDetailReport:
		return typed.kind
	case siteBonusReviewReport:
		return typed.kind
	case siteBonusExchangeReport:
		return typed.kind
	case siteBonusExchangeListReport:
		return typed.kind
	case domain.BonusCatalog:
		return "site.bonus.catalog"
	case siteLoginReport:
		return "site.login"
	case *metafile.MetaInfo:
		return "metafile.manifest"
	case metafileStoreReport:
		return typed.kind
	case siteMetafileFetchReport:
		return typed.kind
	case siteBindingListReport:
		return typed.kind
	case siteBindingInspectReport:
		return typed.kind
	case storageProfileReport:
		return typed.kind
	case storageIndexReport:
		return typed.kind
	case storageIndexInspectReport:
		return typed.kind
	case storageIndexPruneReport:
		return typed.kind
	case metafile.VerificationResult:
		return "content.verification"
	case storage.ProbeResult:
		return "storage.probe"
	case storage.PathMapping:
		return "storage.path_mapping"
	case downloader.Status:
		return "downloader.status"
	case []downloader.Torrent:
		return "downloader.torrent.list"
	case seed.Plan:
		return "content.layout_plan"
	case seed.DiscoveryResult:
		return "content.source_discovery"
	case materialize.Report:
		return "content.materialization"
	case materialize.OperationListResult:
		return "content.materialization.operation_list"
	case materialize.RetentionReport:
		return "content.materialization.retention"
	case materialize.ForgetReport:
		return "content.materialization.forget"
	case clientadopt.Report:
		return "client.adoption"
	case clientadopt.RetentionReport:
		return "client.adoption.retention"
	case clientadopt.ForgetReport:
		return "client.adoption.forget"
	case clientactivate.Report:
		return "client.activation"
	case clientactivate.RetentionReport:
		return "client.activation.retention"
	case clientactivate.ForgetReport:
		return "client.activation.forget"
	case clientremove.Report:
		return "client.removal"
	case clientremove.RetentionReport:
		return "client.removal.retention"
	case clientremove.ForgetReport:
		return "client.removal.forget"
	case clientstop.Report:
		return "client.stop"
	case clientstop.RetentionReport:
		return "client.stop.retention"
	case clientstop.ForgetReport:
		return "client.stop.forget"
	case sourceretire.Report:
		return "content.source_retirement_plan"
	case sourceretire.ExecutionReport:
		return "content.source_retirement"
	case sourceretire.ExecutionOperationListResult:
		return "content.source_retirement.operation_list"
	case sourceretire.ExecutionRetentionReport:
		return "content.source_retirement.retention"
	case sourceretire.ExecutionForgetReport:
		return "content.source_retirement.forget"
	case sourceretire.ParentCleanupReport:
		return "content.source_retirement.parent_cleanup_plan"
	case sourceretire.ParentCleanupExecutionReport:
		return "content.source_retirement.parent_cleanup"
	case sourceretire.ParentCleanupRetentionReport:
		return "content.source_retirement.parent_cleanup.retention"
	case sourceretire.ParentCleanupForgetReport:
		return "content.source_retirement.parent_cleanup.forget"
	case reconcile.Report:
		return "ledger.reconciliation"
	default:
		return "unknown"
	}
}

func shortID(value string) string {
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func humanBytes(value int64) string {
	if value < 1024 {
		return strconv.FormatInt(value, 10) + " B"
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	amount := float64(value)
	unit := "B"
	for _, candidate := range units {
		amount /= 1024
		unit = candidate
		if amount < 1024 {
			break
		}
	}
	return strconv.FormatFloat(amount, 'f', 2, 64) + " " + unit
}

func terminalSafe(value string) string {
	var out strings.Builder
	for _, r := range value {
		if !unsafeTerminalRune(r) {
			out.WriteRune(r)
			continue
		}
		switch r {
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r <= 0xff {
				fmt.Fprintf(&out, `\x%02x`, r)
			} else if r <= 0xffff {
				fmt.Fprintf(&out, `\u%04x`, r)
			} else {
				fmt.Fprintf(&out, `\U%08x`, r)
			}
		}
	}
	return out.String()
}

func unsafeTerminalRune(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x061c || r == 0x200e || r == 0x200f ||
		(r >= 0x202a && r <= 0x202e) || r == 0x2028 || r == 0x2029 || (r >= 0x2066 && r <= 0x2069)
}

func valueOrUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
