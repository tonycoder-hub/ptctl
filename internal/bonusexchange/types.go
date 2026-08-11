// Package bonusexchange implements the durable, at-most-once state machine
// around one explicitly reviewed site bonus exchange. It never interprets a
// public review as authority: submit must reproduce the review from a fresh
// site response before the deterministic attempt marker is published.
package bonusexchange

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/metastore"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const (
	IntentSchemaV1  = "ptctl.site-bonus-exchange-intent/v1"
	AttemptSchemaV1 = "ptctl.site-bonus-exchange-attempt/v1"
	OutcomeSchemaV1 = "ptctl.site-bonus-exchange-outcome/v1"

	StatePrepared               = "prepared"
	StateAttemptReservedUnknown = "attempt_reserved_submission_unknown"
	StateConfirmed              = "confirmed"
	StateRejected               = "rejected"
	StateUnknown                = "unknown"
	StateNotSubmitted           = "not_submitted"
	StateInspectionIncomplete   = "inspection_incomplete"

	PrepareEffect = "write_private_site_bonus_exchange_intent"
	LoadEffect    = "read_private_site_bonus_exchange_intent"
	ReserveEffect = "write_private_site_bonus_exchange_attempt"
	OutcomeEffect = "write_private_site_bonus_exchange_outcome"
	StatusEffect  = "read_private_site_bonus_exchange_status"

	defaultMaxRecordBytes = int64(64 << 10)
	hardMaxRecordBytes    = int64(256 << 10)
	defaultMaxStatusBytes = int64(16 << 20)
	hardMaxStatusBytes    = int64(64 << 20)
)

var (
	ErrInvalidExchange        = errors.New("bonus exchange record is invalid")
	ErrCorruptExchange        = errors.New("bonus exchange state is corrupt")
	ErrAttemptAlreadyReserved = errors.New("bonus exchange attempt is already reserved")
	ErrStatusIncomplete       = errors.New("bonus exchange status is incomplete")
)

type Limits struct {
	MaxRecordBytes     int64 `json:"max_record_bytes"`
	MaxStatusEntries   int   `json:"max_status_entries"`
	MaxStatusRecords   int   `json:"max_status_records"`
	MaxStatusPathBytes int64 `json:"max_status_path_bytes"`
	MaxStatusBytes     int64 `json:"max_status_bytes"`
}

func DefaultLimits() Limits {
	records := metastore.DefaultRecordLimits()
	return Limits{
		MaxRecordBytes:     defaultMaxRecordBytes,
		MaxStatusEntries:   records.MaxEntries,
		MaxStatusRecords:   records.MaxRecords,
		MaxStatusPathBytes: records.MaxPathBytes,
		MaxStatusBytes:     defaultMaxStatusBytes,
	}
}

func (limits Limits) Validate() error {
	if limits.MaxRecordBytes <= 0 || limits.MaxRecordBytes > hardMaxRecordBytes ||
		limits.MaxStatusBytes <= 0 || limits.MaxStatusBytes > hardMaxStatusBytes {
		return fmt.Errorf("bonus exchange byte budget is outside the supported range")
	}
	records := metastore.DefaultRecordLimits()
	records.MaxRecordBytes = limits.MaxRecordBytes
	records.MaxEntries = limits.MaxStatusEntries
	records.MaxRecords = limits.MaxStatusRecords
	records.MaxPathBytes = limits.MaxStatusPathBytes
	if err := records.Validate(); err != nil {
		return fmt.Errorf("bonus exchange status budget is outside the supported range")
	}
	return nil
}

type OperationID string

func NewOperationID() (OperationID, error) {
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("bonus exchange operation entropy is unavailable")
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("ptctl-site-bonus-exchange-operation-v1\x00"))
	_, _ = hash.Write(entropy[:])
	return OperationID("sha256:" + hex.EncodeToString(hash.Sum(nil))), nil
}

func ParseOperationID(value string) (OperationID, error) {
	if !validSHA256ID(value) {
		return "", fmt.Errorf("bonus exchange operation ID is invalid")
	}
	return OperationID(value), nil
}

func (id OperationID) String() string { return string(id) }

type IntentRecord struct {
	Schema           string                   `json:"schema"`
	OperationID      OperationID              `json:"operation_id"`
	SiteID           string                   `json:"site_id"`
	Selector         string                   `json:"selector"`
	ExpectedReviewID string                   `json:"expected_review_id"`
	Origin           string                   `json:"origin"`
	ReviewRouteID    string                   `json:"review_route_id"`
	ActionRouteID    string                   `json:"action_route_id"`
	InputMode        string                   `json:"input_mode"`
	CreatedAt        time.Time                `json:"created_at"`
	ReviewLimits     site.BonusReviewLimits   `json:"review_limits"`
	ExchangeLimits   site.BonusExchangeLimits `json:"exchange_limits"`
}

func (record IntentRecord) Validate() error {
	operationID, operationErr := ParseOperationID(record.OperationID.String())
	config, configErr := site.NewBonusExchangeConfig(record.Origin, record.ReviewRouteID, record.ActionRouteID)
	if record.Schema != IntentSchemaV1 || operationErr != nil || operationID != record.OperationID ||
		!safeText(record.SiteID, 128) || !safeText(record.Selector, 128) || site.ValidateBonusReviewID(record.ExpectedReviewID) != nil ||
		configErr != nil || config.Validate() != nil || record.InputMode != site.BonusReviewInputNone ||
		record.CreatedAt.IsZero() || record.CreatedAt.Location() != time.UTC || record.ReviewLimits.Validate() != nil || record.ExchangeLimits.Validate() != nil {
		return fmt.Errorf("%w: intent value is invalid", ErrInvalidExchange)
	}
	return nil
}

func (record IntentRecord) Config() site.BonusExchangeConfig {
	return site.BonusExchangeConfig{Origin: record.Origin, ReviewRouteID: record.ReviewRouteID, ActionRouteID: record.ActionRouteID}
}

type AttemptRecord struct {
	Schema           string             `json:"schema"`
	OperationID      OperationID        `json:"operation_id"`
	IntentRecordID   metastore.RecordID `json:"intent_record_id"`
	SiteID           string             `json:"site_id"`
	Selector         string             `json:"selector"`
	ExpectedReviewID string             `json:"expected_review_id"`
	Origin           string             `json:"origin"`
	ReviewRouteID    string             `json:"review_route_id"`
	ActionRouteID    string             `json:"action_route_id"`
}

func (record AttemptRecord) Validate() error {
	operationID, operationErr := ParseOperationID(record.OperationID.String())
	intentID, intentErr := metastore.ParseRecordID(record.IntentRecordID.String())
	config, configErr := site.NewBonusExchangeConfig(record.Origin, record.ReviewRouteID, record.ActionRouteID)
	if record.Schema != AttemptSchemaV1 || operationErr != nil || operationID != record.OperationID || intentErr != nil || intentID != record.IntentRecordID ||
		!safeText(record.SiteID, 128) || !safeText(record.Selector, 128) || site.ValidateBonusReviewID(record.ExpectedReviewID) != nil || configErr != nil || config.Validate() != nil {
		return fmt.Errorf("%w: attempt value is invalid", ErrInvalidExchange)
	}
	return nil
}

func (record AttemptRecord) MatchesIntent(intent IntentRecord, ref metastore.RecordRef) bool {
	return record.Validate() == nil && intent.Validate() == nil && ref.Kind == metastore.RecordKindSiteBonusExchangeIntentV1 &&
		record.IntentRecordID == ref.ID && record.OperationID == intent.OperationID && record.SiteID == intent.SiteID &&
		record.Selector == intent.Selector && record.ExpectedReviewID == intent.ExpectedReviewID && record.Origin == intent.Origin &&
		record.ReviewRouteID == intent.ReviewRouteID && record.ActionRouteID == intent.ActionRouteID
}

type OutcomeRecord struct {
	Schema          string                    `json:"schema"`
	OperationID     OperationID               `json:"operation_id"`
	IntentRecordID  metastore.RecordID        `json:"intent_record_id"`
	AttemptRecordID metastore.RecordID        `json:"attempt_record_id"`
	RecordedAt      time.Time                 `json:"recorded_at"`
	Receipt         site.BonusExchangeReceipt `json:"receipt"`
}

func (record OutcomeRecord) Validate() error {
	operationID, operationErr := ParseOperationID(record.OperationID.String())
	intentID, intentErr := metastore.ParseRecordID(record.IntentRecordID.String())
	attemptID, attemptErr := metastore.ParseRecordID(record.AttemptRecordID.String())
	if record.Schema != OutcomeSchemaV1 || operationErr != nil || operationID != record.OperationID ||
		intentErr != nil || intentID != record.IntentRecordID || attemptErr != nil || attemptID != record.AttemptRecordID ||
		record.RecordedAt.IsZero() || record.RecordedAt.Location() != time.UTC || site.ValidateBonusExchangeReceipt(record.Receipt) != nil {
		return fmt.Errorf("%w: outcome value is invalid", ErrInvalidExchange)
	}
	return nil
}

func (record OutcomeRecord) Matches(intent IntentRecord, intentRef metastore.RecordRef, attempt AttemptRecord, attemptRef metastore.RecordRef) bool {
	return record.Validate() == nil && attempt.MatchesIntent(intent, intentRef) && attemptRef.Kind == metastore.RecordKindSiteBonusExchangeAttemptV1 &&
		record.OperationID == intent.OperationID && record.IntentRecordID == intentRef.ID && record.AttemptRecordID == attemptRef.ID &&
		record.Receipt.SiteID == intent.SiteID && record.Receipt.Selector == intent.Selector &&
		record.Receipt.ExpectedReviewID == intent.ExpectedReviewID && record.Receipt.FreshReviewID == intent.ExpectedReviewID &&
		record.Receipt.Origin == intent.Origin && record.Receipt.ReviewRouteID == intent.ReviewRouteID && record.Receipt.ActionRouteID == intent.ActionRouteID &&
		record.Receipt.Limits == intent.ExchangeLimits
}

type PrepareReceipt struct {
	Effect          string              `json:"effect"`
	WritesPerformed int                 `json:"writes_performed"`
	Record          metastore.RecordRef `json:"record"`
	OperationID     OperationID         `json:"operation_id"`
	Store           metastore.StoreInfo `json:"store"`
}

type LoadReceipt struct {
	Effect            string              `json:"effect"`
	Complete          bool                `json:"complete"`
	RecordBytesRead   int64               `json:"record_bytes_read"`
	ConsumerBytesRead int64               `json:"consumer_bytes_read"`
	Record            metastore.RecordRef `json:"record"`
	Store             metastore.StoreInfo `json:"store"`
}

type ReserveReceipt struct {
	Effect          string              `json:"effect"`
	WritesPerformed int                 `json:"writes_performed"`
	AlreadyPresent  bool                `json:"already_present"`
	Acquired        bool                `json:"acquired"`
	Record          metastore.RecordRef `json:"record"`
	Store           metastore.StoreInfo `json:"store"`
}

type OutcomeReceipt struct {
	Effect              string              `json:"effect"`
	WritesPerformed     int                 `json:"writes_performed"`
	AlreadyPresent      bool                `json:"already_present"`
	DurabilityConfirmed bool                `json:"durability_confirmed"`
	Record              metastore.RecordRef `json:"record"`
	Store               metastore.StoreInfo `json:"store"`
}

type StatusUsage struct {
	OutcomeEntriesConsidered int   `json:"outcome_entries_considered"`
	OutcomeRecordsRead       int   `json:"outcome_records_read"`
	OutcomeBytesRead         int64 `json:"outcome_bytes_read"`
}

type Status struct {
	Effect     string               `json:"effect"`
	Complete   bool                 `json:"complete"`
	State      string               `json:"state"`
	Intent     IntentRecord         `json:"intent"`
	IntentRef  metastore.RecordRef  `json:"intent_record"`
	Attempt    *AttemptRecord       `json:"attempt,omitempty"`
	AttemptRef *metastore.RecordRef `json:"attempt_record,omitempty"`
	Outcome    *OutcomeRecord       `json:"outcome,omitempty"`
	OutcomeRef *metastore.RecordRef `json:"outcome_record,omitempty"`
	Used       StatusUsage          `json:"used"`
	Store      metastore.StoreInfo  `json:"store"`
	StopReason string               `json:"stop_reason,omitempty"`
}

type verifiedIntentAuthority struct {
	session     *Session
	storeID     string
	intentID    metastore.RecordID
	operationID OperationID
}

type VerifiedIntent struct {
	record    IntentRecord
	ref       metastore.RecordRef
	store     metastore.StoreInfo
	authority *verifiedIntentAuthority
}

type reservedAttemptAuthority struct {
	session   *Session
	intent    *verifiedIntentAuthority
	storeID   string
	attemptID metastore.RecordID

	mu              sync.Mutex
	observedOutcome *site.ObservedBonusExchange
	receipt         site.BonusExchangeReceipt
}

// ReservedAttempt is the process-local authority created only when this
// invocation durably publishes the deterministic at-most-once marker. A
// marker found on disk after a restart is deliberately not sufficient to
// recreate it, because the remote submission may already have happened.
type ReservedAttempt struct {
	record    AttemptRecord
	ref       metastore.RecordRef
	store     metastore.StoreInfo
	authority *reservedAttemptAuthority
}

func (attempt *ReservedAttempt) PublicCopy() (AttemptRecord, metastore.RecordRef, metastore.StoreInfo) {
	if attempt == nil {
		return AttemptRecord{}, metastore.RecordRef{}, metastore.StoreInfo{}
	}
	return attempt.record, attempt.ref, attempt.store
}

func (attempt *ReservedAttempt) Verified() bool {
	return attempt != nil && attempt.authority != nil && attempt.authority.session != nil && attempt.authority.intent != nil &&
		attempt.record.Validate() == nil && attempt.ref.Kind == metastore.RecordKindSiteBonusExchangeAttemptV1 &&
		attempt.ref.ID == attempt.authority.attemptID && attempt.store.StoreID == attempt.authority.storeID
}

func (attempt ReservedAttempt) String() string { return "[RESERVED_BONUS_EXCHANGE_ATTEMPT]" }
func (attempt ReservedAttempt) GoString() string {
	return "bonusexchange.ReservedAttempt{[REDACTED_AUTHORITY]}"
}

func (intent *VerifiedIntent) PublicCopy() (IntentRecord, metastore.RecordRef, metastore.StoreInfo) {
	if intent == nil {
		return IntentRecord{}, metastore.RecordRef{}, metastore.StoreInfo{}
	}
	return intent.record, intent.ref, intent.store
}

func (intent *VerifiedIntent) Verified() bool {
	return intent != nil && intent.authority != nil && intent.authority.session != nil && intent.record.Validate() == nil &&
		intent.store.StoreID == intent.authority.storeID && intent.ref.ID == intent.authority.intentID &&
		intent.record.OperationID == intent.authority.operationID && intent.ref.Kind == metastore.RecordKindSiteBonusExchangeIntentV1
}

func (intent VerifiedIntent) String() string { return "[VERIFIED_BONUS_EXCHANGE_INTENT]" }
func (intent VerifiedIntent) GoString() string {
	return "bonusexchange.VerifiedIntent{[REDACTED_AUTHORITY]}"
}

func validSHA256ID(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size
}

func safeText(value string, maximum int) bool {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
