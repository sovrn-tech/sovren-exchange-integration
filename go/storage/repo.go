// Package storage defines the kit's persistence contract: shared domain
// types mirroring specs/008-exchange-integration-kit/data-model.md and the
// repository interfaces implemented by storage/sqlite and storage/postgres.
//
// Every uniqueness rule in the data model is a real database constraint
// (R7); the repository layer additionally enforces the legal state-machine
// transitions (see transitions.go) — implementations MUST call the
// Validate*Transition helpers before any status write.
//
// All token amounts are integer base units (usovr) carried as math.Int;
// floats never appear in a money path (FR-017).
package storage

import (
	"context"
	"time"

	sdkmath "cosmossdk.io/math"
)

// BaseDenom is the only denom the kit records; non-usovr coins are filtered
// pre-insert and never create records.
const BaseDenom = "usovr"

// SignModeDirect is the only sign mode the kit produces (R4).
const SignModeDirect = "SIGN_MODE_DIRECT"

// ---------------------------------------------------------------------------
// Enums (string values are the persisted representation; they mirror the
// data model verbatim and are a compatibility contract with the schema CHECK
// constraints — migrations_test cross-checks the two sets).
// ---------------------------------------------------------------------------

// LedgerEntryKind distinguishes tx-attributed ledger rows from block-scoped
// event rows (finalize_block_events carry no tx hash — data model §3).
type LedgerEntryKind string

const (
	LedgerKindTx         LedgerEntryKind = "TX"
	LedgerKindBlockEvent LedgerEntryKind = "BLOCK_EVENT"
)

// AllLedgerEntryKinds lists every valid LedgerEntryKind.
var AllLedgerEntryKinds = []LedgerEntryKind{LedgerKindTx, LedgerKindBlockEvent}

// Valid reports whether k is a known kind.
func (k LedgerEntryKind) Valid() bool { return enumValid(k, AllLedgerEntryKinds) }

// LedgerDirection is the flow direction relative to the watched address.
type LedgerDirection string

const (
	DirectionIn  LedgerDirection = "IN"
	DirectionOut LedgerDirection = "OUT"
)

// AllLedgerDirections lists every valid LedgerDirection.
var AllLedgerDirections = []LedgerDirection{DirectionIn, DirectionOut}

// Valid reports whether d is a known direction.
func (d LedgerDirection) Valid() bool { return enumValid(d, AllLedgerDirections) }

// Classification is the ledger-entry classification (data model §3).
type Classification string

const (
	ClassExternalDeposit    Classification = "EXTERNAL_DEPOSIT"
	ClassInternalTransfer   Classification = "INTERNAL_TRANSFER"
	ClassFeeFunding         Classification = "FEE_FUNDING"
	ClassSweep              Classification = "SWEEP"
	ClassWithdrawal         Classification = "WITHDRAWAL"
	ClassFeeDeduction       Classification = "FEE_DEDUCTION"
	ClassUnattributedReview Classification = "UNATTRIBUTED_REVIEW"
)

// AllClassifications lists every valid Classification.
var AllClassifications = []Classification{
	ClassExternalDeposit, ClassInternalTransfer, ClassFeeFunding,
	ClassSweep, ClassWithdrawal, ClassFeeDeduction, ClassUnattributedReview,
}

// Valid reports whether c is a known classification.
func (c Classification) Valid() bool { return enumValid(c, AllClassifications) }

// DepositStatus is the customer-credit state machine status (data model §3b).
type DepositStatus string

const (
	DepositDiscovered            DepositStatus = "DISCOVERED"
	DepositValidated             DepositStatus = "VALIDATED"
	DepositAwaitingConfirmations DepositStatus = "AWAITING_CONFIRMATIONS"
	DepositCreditable            DepositStatus = "CREDITABLE"
	DepositCredited              DepositStatus = "CREDITED"
	DepositSweepPending          DepositStatus = "SWEEP_PENDING"
	DepositSwept                 DepositStatus = "SWEPT"
	DepositRejected              DepositStatus = "REJECTED"
	DepositReviewRequired        DepositStatus = "REVIEW_REQUIRED"
	DepositOrphaned              DepositStatus = "ORPHANED"
	DepositDuplicate             DepositStatus = "DUPLICATE"
	DepositBelowMinimum          DepositStatus = "BELOW_MINIMUM"
	DepositSuspended             DepositStatus = "SUSPENDED"
)

// AllDepositStatuses lists every valid DepositStatus.
var AllDepositStatuses = []DepositStatus{
	DepositDiscovered, DepositValidated, DepositAwaitingConfirmations,
	DepositCreditable, DepositCredited, DepositSweepPending, DepositSwept,
	DepositRejected, DepositReviewRequired, DepositOrphaned,
	DepositDuplicate, DepositBelowMinimum, DepositSuspended,
}

// Valid reports whether s is a known status.
func (s DepositStatus) Valid() bool { return enumValid(s, AllDepositStatuses) }

// WithdrawalStatus is the withdrawal state machine status (data model §5).
type WithdrawalStatus string

const (
	WithdrawalRequested            WithdrawalStatus = "REQUESTED"
	WithdrawalAddressValidated     WithdrawalStatus = "ADDRESS_VALIDATED"
	WithdrawalComplianceApproved   WithdrawalStatus = "COMPLIANCE_APPROVED"
	WithdrawalFundsReserved        WithdrawalStatus = "FUNDS_RESERVED"
	WithdrawalSequenceReserved     WithdrawalStatus = "SEQUENCE_RESERVED"
	WithdrawalTransactionBuilt     WithdrawalStatus = "TRANSACTION_BUILT"
	WithdrawalTransactionSimulated WithdrawalStatus = "TRANSACTION_SIMULATED"
	WithdrawalSigned               WithdrawalStatus = "SIGNED"
	WithdrawalBroadcast            WithdrawalStatus = "BROADCAST"
	WithdrawalIncluded             WithdrawalStatus = "INCLUDED"
	WithdrawalConfirmed            WithdrawalStatus = "CONFIRMED"
	WithdrawalFailed               WithdrawalStatus = "FAILED"
	WithdrawalCancelled            WithdrawalStatus = "CANCELLED"
	WithdrawalReviewRequired       WithdrawalStatus = "REVIEW_REQUIRED"
)

// AllWithdrawalStatuses lists every valid WithdrawalStatus.
var AllWithdrawalStatuses = []WithdrawalStatus{
	WithdrawalRequested, WithdrawalAddressValidated, WithdrawalComplianceApproved,
	WithdrawalFundsReserved, WithdrawalSequenceReserved, WithdrawalTransactionBuilt,
	WithdrawalTransactionSimulated, WithdrawalSigned, WithdrawalBroadcast,
	WithdrawalIncluded, WithdrawalConfirmed, WithdrawalFailed,
	WithdrawalCancelled, WithdrawalReviewRequired,
}

// Valid reports whether s is a known status.
func (s WithdrawalStatus) Valid() bool { return enumValid(s, AllWithdrawalStatuses) }

// SweepStatus is the sweep-job state machine status (data model §7).
type SweepStatus string

const (
	SweepPending   SweepStatus = "PENDING"
	SweepBuilt     SweepStatus = "BUILT"
	SweepSigned    SweepStatus = "SIGNED"
	SweepBroadcast SweepStatus = "BROADCAST"
	SweepConfirmed SweepStatus = "CONFIRMED"
	SweepDeferred  SweepStatus = "DEFERRED"
	SweepFailed    SweepStatus = "FAILED"
	SweepCancelled SweepStatus = "CANCELLED"
)

// AllSweepStatuses lists every valid SweepStatus.
var AllSweepStatuses = []SweepStatus{
	SweepPending, SweepBuilt, SweepSigned, SweepBroadcast,
	SweepConfirmed, SweepDeferred, SweepFailed, SweepCancelled,
}

// TerminalSweepStatuses is the terminal set backing the partial-unique
// constraint: at most one NON-terminal sweep per (chain_id, source_address).
var TerminalSweepStatuses = []SweepStatus{SweepConfirmed, SweepFailed, SweepCancelled}

// Valid reports whether s is a known status.
func (s SweepStatus) Valid() bool { return enumValid(s, AllSweepStatuses) }

// Terminal reports whether s is outside the partial-unique scope.
func (s SweepStatus) Terminal() bool { return enumValid(s, TerminalSweepStatuses) }

// SequenceReservationStatus is the reservation state machine status
// (data model §6).
type SequenceReservationStatus string

const (
	SequenceReserved               SequenceReservationStatus = "RESERVED"
	SequenceSigned                 SequenceReservationStatus = "SIGNED"
	SequenceBroadcast              SequenceReservationStatus = "BROADCAST"
	SequenceConsumed               SequenceReservationStatus = "CONSUMED"
	SequenceReleased               SequenceReservationStatus = "RELEASED"
	SequenceReconciliationRequired SequenceReservationStatus = "RECONCILIATION_REQUIRED"
)

// AllSequenceReservationStatuses lists every valid SequenceReservationStatus.
var AllSequenceReservationStatuses = []SequenceReservationStatus{
	SequenceReserved, SequenceSigned, SequenceBroadcast,
	SequenceConsumed, SequenceReleased, SequenceReconciliationRequired,
}

// Valid reports whether s is a known status.
func (s SequenceReservationStatus) Valid() bool {
	return enumValid(s, AllSequenceReservationStatuses)
}

// WorkKind identifies the work item owning a sequence reservation.
type WorkKind string

const (
	WorkWithdrawal WorkKind = "WITHDRAWAL"
	WorkSweep      WorkKind = "SWEEP"
)

// AllWorkKinds lists every valid WorkKind.
var AllWorkKinds = []WorkKind{WorkWithdrawal, WorkSweep}

// Valid reports whether k is a known kind.
func (k WorkKind) Valid() bool { return enumValid(k, AllWorkKinds) }

// WorkRef binds a sequence reservation to exactly one work item
// (UNIQUE(work_kind, work_id) — one reservation per work item).
type WorkRef struct {
	Kind WorkKind
	ID   string
}

// SweepStrategy selects the fee-handling strategy for sweeps (FR-038).
type SweepStrategy string

const (
	StrategyFeeReserve      SweepStrategy = "FEE_RESERVE"
	StrategyFeeFund         SweepStrategy = "FEE_FUND"
	StrategyThresholdOnly   SweepStrategy = "THRESHOLD_ONLY"
	StrategyCustodyAbstract SweepStrategy = "CUSTODY_ABSTRACTED"
)

// AllSweepStrategies lists every valid SweepStrategy.
var AllSweepStrategies = []SweepStrategy{
	StrategyFeeReserve, StrategyFeeFund, StrategyThresholdOnly, StrategyCustodyAbstract,
}

// Valid reports whether s is a known strategy.
func (s SweepStrategy) Valid() bool { return enumValid(s, AllSweepStrategies) }

// WatchedAddressKind drives scanner attribution and sweep/reconciliation
// scoping (data model §9).
type WatchedAddressKind string

const (
	WatchCustomerDeposit WatchedAddressKind = "CUSTOMER_DEPOSIT"
	WatchHotWallet       WatchedAddressKind = "HOT_WALLET"
	WatchColdWallet      WatchedAddressKind = "COLD_WALLET"
	WatchFeeWallet       WatchedAddressKind = "FEE_WALLET"
	WatchOmnibus         WatchedAddressKind = "OMNIBUS"
)

// AllWatchedAddressKinds lists every valid WatchedAddressKind.
var AllWatchedAddressKinds = []WatchedAddressKind{
	WatchCustomerDeposit, WatchHotWallet, WatchColdWallet, WatchFeeWallet, WatchOmnibus,
}

// Valid reports whether k is a known kind.
func (k WatchedAddressKind) Valid() bool { return enumValid(k, AllWatchedAddressKinds) }

// ChainReviewTrigger is the FR-044 trigger taxonomy (data model §11).
type ChainReviewTrigger string

const (
	TriggerBlockHashMismatch   ChainReviewTrigger = "BLOCK_HASH_MISMATCH"
	TriggerQueryResultMismatch ChainReviewTrigger = "QUERY_RESULT_MISMATCH"
	TriggerHeightDivergence    ChainReviewTrigger = "HEIGHT_DIVERGENCE"
	TriggerChainHalt           ChainReviewTrigger = "CHAIN_HALT"
	TriggerWrongChainID        ChainReviewTrigger = "WRONG_CHAIN_ID"
	TriggerUpgradeWindow       ChainReviewTrigger = "UPGRADE_WINDOW"
)

// AllChainReviewTriggers lists every valid ChainReviewTrigger.
var AllChainReviewTriggers = []ChainReviewTrigger{
	TriggerBlockHashMismatch, TriggerQueryResultMismatch, TriggerHeightDivergence,
	TriggerChainHalt, TriggerWrongChainID, TriggerUpgradeWindow,
}

// Valid reports whether t is a known trigger.
func (t ChainReviewTrigger) Valid() bool { return enumValid(t, AllChainReviewTriggers) }

// ReconciliationKind is the report cadence kind (data model §8).
type ReconciliationKind string

const (
	ReconTxNearRealTime ReconciliationKind = "TX_NEAR_REAL_TIME"
	ReconWalletHourly   ReconciliationKind = "WALLET_HOURLY"
	ReconAddressDaily   ReconciliationKind = "ADDRESS_DAILY"
	ReconManual         ReconciliationKind = "MANUAL"
)

// AllReconciliationKinds lists every valid ReconciliationKind.
var AllReconciliationKinds = []ReconciliationKind{
	ReconTxNearRealTime, ReconWalletHourly, ReconAddressDaily, ReconManual,
}

// Valid reports whether k is a known kind.
func (k ReconciliationKind) Valid() bool { return enumValid(k, AllReconciliationKinds) }

// ReviewItemKind identifies which entity a review-queue item references.
type ReviewItemKind string

const (
	ReviewKindDeposit     ReviewItemKind = "DEPOSIT"
	ReviewKindWithdrawal  ReviewItemKind = "WITHDRAWAL"
	ReviewKindLedgerEntry ReviewItemKind = "LEDGER_ENTRY"
)

// AllReviewItemKinds lists every valid ReviewItemKind.
var AllReviewItemKinds = []ReviewItemKind{
	ReviewKindDeposit, ReviewKindWithdrawal, ReviewKindLedgerEntry,
}

// Valid reports whether k is a known kind.
func (k ReviewItemKind) Valid() bool { return enumValid(k, AllReviewItemKinds) }

// ControlFlow names one independently pausable flow (FR-051).
type ControlFlow string

const (
	FlowCredit    ControlFlow = "credit"
	FlowSigning   ControlFlow = "signing"
	FlowBroadcast ControlFlow = "broadcast"
	FlowSweep     ControlFlow = "sweep"
)

// AllControlFlows lists every valid ControlFlow.
var AllControlFlows = []ControlFlow{FlowCredit, FlowSigning, FlowBroadcast, FlowSweep}

// Valid reports whether f is a known flow.
func (f ControlFlow) Valid() bool { return enumValid(f, AllControlFlows) }

func enumValid[T comparable](v T, all []T) bool {
	for _, a := range all {
		if v == a {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Domain records
// ---------------------------------------------------------------------------

// LedgerEntry is one immutable, append-only ChainTransferLedger row (data
// model §3). Rows are never mutated; corrections append superseding entries.
//
// Identity: kind TX ⇒ (ChainID, TxHash, MessageIndex, OpIndex) UNIQUE;
// kind BLOCK_EVENT ⇒ (ChainID, BlockHeight, EventIndex) UNIQUE, with no tx
// attribution (TxHash empty, MessageIndex/OpIndex unused).
type LedgerEntry struct {
	ID              int64
	ChainID         string
	Kind            LedgerEntryKind
	TxHash          string
	MessageIndex    uint32
	OpIndex         uint32
	BlockHeight     uint64
	EventIndex      uint32
	Direction       LedgerDirection
	Address         string
	CounterpartySet []string
	AmountBaseUnits sdkmath.Int
	Denom           string
	TxCode          uint32
	Classification  Classification
	CreatedAt       time.Time
}

// FeeOutflow is one FEE_DEDUCTION event-truth record (data model §8a):
// recorded iff the fee-deduction ante event was emitted and the resolved
// payer is watched. UNIQUE(ChainID, TxHash).
type FeeOutflow struct {
	ID           int64
	ChainID      string
	TxHash       string
	PayerAddress string
	FeeBaseUnits sdkmath.Int
	TxCode       uint32
	BlockHeight  uint64
	CreatedAt    time.Time
}

// FeeFundingSpend is a durable, confirm-time record of one FEE_FUND fee-wallet
// spend (a confirmed funding leg: fee wallet → deposit address). It is written
// by the sweeper atomically with the funding leg's confirmation, NOT by the
// deposit scanner, so the fee-wallet spend cap reads authoritative spend the
// instant a leg confirms rather than waiting for the scanner to catch up.
// UNIQUE(ChainID, TxHash): one funding tx is one spend.
type FeeFundingSpend struct {
	ID               int64
	ChainID          string
	TxHash           string
	FeeWalletAddress string
	AmountBaseUnits  sdkmath.Int
	BlockHeight      uint64
	CreatedAt        time.Time
}

// DepositRecord is the customer-credit state machine record (data model §3b),
// derived exclusively from EXTERNAL_DEPOSIT ledger entries.
// UNIQUE(ChainID, TxHash, MessageIndex, CoinIndex, RecipientAddress) (FR-024).
type DepositRecord struct {
	ID               int64
	ChainID          string
	TxHash           string
	MessageIndex     uint32
	CoinIndex        uint32
	BlockHeight      uint64
	BlockTimestamp   time.Time
	SenderAddress    *string // nil when the input set is ambiguous (MsgMultiSend)
	RecipientAddress string
	Denom            string
	AmountBaseUnits  sdkmath.Int
	Memo             string
	TxCode           uint32
	TxLog            string
	Status           DepositStatus
	// PriorStatus is set while Status == SUSPENDED; resume returns to it.
	PriorStatus *DepositStatus
	CreditedAt  *time.Time
	SweepTxHash *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ScannerCheckpoint is the per-chain scan cursor (data model §4). Advancing
// it MUST happen in the same transaction as the block's deposit/ledger
// writes (FR-026) — use Store.WithTx.
type ScannerCheckpoint struct {
	ChainID                  string
	LastFullyProcessedHeight uint64
	LastObservedBlockHash    string
	UpdatedAt                time.Time
}

// WithdrawalRecord is the withdrawal state machine record (data model §5).
// PK WithdrawalID; UNIQUE IdempotencyKey (FR-033). SignedTxBytes is persisted
// at SIGNED so reconciliation rebroadcasts the exact same bytes, never
// re-signs.
type WithdrawalRecord struct {
	WithdrawalID       string
	IdempotencyKey     string
	ChainID            string
	SourceAddress      string
	DestinationAddress string
	Denom              string
	AmountBaseUnits    sdkmath.Int
	Memo               string
	AccountNumber      *uint64
	Sequence           *uint64
	GasWanted          *uint64
	GasLimit           *uint64
	FeeAmountBaseUnits *sdkmath.Int
	SignMode           string
	SignedTxBytes      []byte
	TxHash             *string
	BlockHeight        *uint64
	TxCode             *uint32
	RawLog             string
	Status             WithdrawalStatus
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// SequenceReservation binds one (chain, account, sequence) slot to one work
// item (data model §6). UNIQUE(ChainID, SourceAddress, Sequence) is the
// last-line double-spend guarantee; UNIQUE(WorkRef) gives one reservation per
// work item across kinds.
type SequenceReservation struct {
	ID            int64
	ChainID       string
	SourceAddress string
	AccountNumber uint64
	Sequence      uint64
	WorkRef       WorkRef
	Status        SequenceReservationStatus
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// SweepJob is the sweep state machine record (data model §7). At most one
// non-terminal job per (ChainID, SourceAddress) — DB partial-unique;
// IdempotencyKey UNIQUE is the subordinate request-dedup layer (FR-039).
type SweepJob struct {
	SweepID             string
	IdempotencyKey      string
	ChainID             string
	SourceAddress       string
	HotWalletAddress    string
	Strategy            SweepStrategy
	AmountBaseUnits     sdkmath.Int
	FeeReserveBaseUnits sdkmath.Int
	DepositIDs          []int64
	SignedTxBytes       []byte
	TxHash              *string
	TxCode              *uint32
	Status              SweepStatus
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// WatchedAddress is one exchange-controlled address (data model §9).
// UNIQUE(ChainID, Address). CustomerRef is an opaque exchange-side
// reference; the kit never stores PII.
type WatchedAddress struct {
	ChainID      string
	Address      string
	Kind         WatchedAddressKind
	CustomerRef  string
	MemoRequired bool
	Active       bool
}

// OperationalControls is the singleton-per-chain pause switchboard (FR-051,
// data model §10).
type OperationalControls struct {
	ChainID           string
	CreditPaused      bool
	SigningPaused     bool
	BroadcastPaused   bool
	SweepPaused       bool
	ScanWithoutCredit bool
	ResumeFromHeight  *uint64
	UpdatedAt         time.Time
}

// ControlsAuditEntry records one control flip (who/when/why — FR-051).
type ControlsAuditEntry struct {
	ID       int64
	ChainID  string
	Field    string
	OldValue string
	NewValue string
	Actor    string
	Reason   string
	At       time.Time
}

// ControlsUpdate is a partial update of OperationalControls; nil fields are
// left unchanged. ClearResumeFromHeight wins over ResumeFromHeight.
type ControlsUpdate struct {
	CreditPaused          *bool
	SigningPaused         *bool
	BroadcastPaused       *bool
	SweepPaused           *bool
	ScanWithoutCredit     *bool
	ResumeFromHeight      *uint64
	ClearResumeFromHeight bool
}

// NodeObservation is one node's view inside a ChainReviewCondition.
type NodeObservation struct {
	Endpoint string
	Height   uint64
	Value    string // block hash or query-result digest, trigger-dependent
}

// ChainReviewCondition is an FR-044 condition (data model §11). While one is
// open for a chain, the FR-023 crediting gate is closed.
type ChainReviewCondition struct {
	ConditionID string
	ChainID     string
	Trigger     ChainReviewTrigger
	NodeA       NodeObservation
	NodeB       NodeObservation
	OpenedAt    time.Time
	ResolvedAt  *time.Time
	Resolution  string
}

// ReviewItem is one operator review-queue entry (FR-030/FR-044 surfaces;
// admin API /v1/review-queue).
type ReviewItem struct {
	ID         int64
	ChainID    string
	Kind       ReviewItemKind
	RefID      string // deposit ID, withdrawal ID, or ledger-entry ID
	Reason     string
	OpenedAt   time.Time
	ResolvedAt *time.Time
	Resolution string
}

// ReconciliationEntry is one address line of a report (data model §8):
// expected is computed from the ledger (Σ inflows − Σ outflows, including
// FEE_DEDUCTION), never from credit status.
type ReconciliationEntry struct {
	Address                 string
	ExpectedBaseUnits       sdkmath.Int
	ObservedBaseUnits       sdkmath.Int
	Difference              sdkmath.Int
	EarliestSuspectedHeight uint64
	RelatedTxHashes         []string
	RecommendedRescanHeight uint64
}

// ReconciliationReport is a persisted FR-046 report.
type ReconciliationReport struct {
	ReportID         string
	ChainID          string
	Kind             ReconciliationKind
	PeriodStart      time.Time
	PeriodEnd        time.Time
	GeneratedAt      time.Time
	Entries          []ReconciliationEntry
	DiscrepancyCount int
}

// OutboxEvent is one transactional-outbox row: written in the same DB
// transaction as the state flip it announces (e.g. deposit CREDITED — data
// model §3b invariant), dispatched at-least-once afterwards. DedupKey, when
// non-empty, is UNIQUE and makes the emission exactly-once per key.
type OutboxEvent struct {
	ID           int64
	ChainID      string
	Topic        string
	DedupKey     string
	Payload      []byte
	CreatedAt    time.Time
	DispatchedAt *time.Time
}

// ---------------------------------------------------------------------------
// Partial updates (nil field = unchanged)
// ---------------------------------------------------------------------------

// DepositUpdate carries the writable fields of a deposit status change.
type DepositUpdate struct {
	CreditedAt  *time.Time
	SweepTxHash *string
	TxLog       *string
}

// WithdrawalUpdate carries the writable fields of a withdrawal status change.
type WithdrawalUpdate struct {
	AccountNumber      *uint64
	Sequence           *uint64
	GasWanted          *uint64
	GasLimit           *uint64
	FeeAmountBaseUnits *sdkmath.Int
	SignedTxBytes      []byte
	TxHash             *string
	BlockHeight        *uint64
	TxCode             *uint32
	RawLog             *string
}

// SweepUpdate carries the writable fields of a sweep status change.
type SweepUpdate struct {
	SignedTxBytes []byte
	TxHash        *string
	TxCode        *uint32
	DepositIDs    []int64 // non-nil replaces the covered-deposit set
}

// LedgerQuery selects ledger entries for an address over a height range.
// Zero ToHeight means unbounded; AfterID is the pagination cursor.
type LedgerQuery struct {
	ChainID    string
	Address    string
	FromHeight uint64
	ToHeight   uint64
	AfterID    int64
	Limit      int
}

// ---------------------------------------------------------------------------
// Repositories
// ---------------------------------------------------------------------------

// LedgerRepo persists the ChainTransferLedger and its FEE_DEDUCTION sibling
// table. Append-only: no update or delete methods exist by design.
type LedgerRepo interface {
	// Append inserts one entry; a unique-key hit returns ErrDuplicate.
	Append(ctx context.Context, e LedgerEntry) (LedgerEntry, error)
	GetTxEntry(ctx context.Context, chainID, txHash string, messageIndex, opIndex uint32) (LedgerEntry, error)
	GetBlockEventEntry(ctx context.Context, chainID string, blockHeight uint64, eventIndex uint32) (LedgerEntry, error)
	List(ctx context.Context, q LedgerQuery) ([]LedgerEntry, error)

	// AppendFeeOutflow inserts one FEE_DEDUCTION record; a (chain_id, tx_hash)
	// hit returns ErrDuplicate.
	AppendFeeOutflow(ctx context.Context, f FeeOutflow) (FeeOutflow, error)
	ListFeeOutflows(ctx context.Context, chainID, payerAddress string, fromHeight, toHeight uint64) ([]FeeOutflow, error)

	// AppendFeeFundingSpend records one confirmed FEE_FUND fee-wallet spend; a
	// (chain_id, tx_hash) hit returns ErrDuplicate (a re-confirm is a no-op).
	AppendFeeFundingSpend(ctx context.Context, s FeeFundingSpend) (FeeFundingSpend, error)
	// SumFeeFundingSpend totals confirmed fee-wallet FEE_FUND spend over the
	// inclusive [fromHeight, toHeight] block window (toHeight 0 = open-ended).
	SumFeeFundingSpend(ctx context.Context, chainID, feeWalletAddress string, fromHeight, toHeight uint64) (sdkmath.Int, error)
}

// DepositRepo persists DepositRecords and enforces the §3b state machine.
type DepositRepo interface {
	// Insert creates a deposit; a unique-key hit returns ErrDuplicate (the
	// caller records the DUPLICATE observation for metrics — never re-credits).
	Insert(ctx context.Context, d DepositRecord) (DepositRecord, error)
	Get(ctx context.Context, chainID, txHash string, messageIndex, coinIndex uint32, recipientAddress string) (DepositRecord, error)
	GetByID(ctx context.Context, id int64) (DepositRecord, error)
	ListByStatus(ctx context.Context, chainID string, status DepositStatus, limit int) ([]DepositRecord, error)

	// UpdateStatus moves a deposit from → to, applying set. It returns
	// ErrStatusConflict when the stored status differs from `from`,
	// ErrIllegalTransition when the (from, to) pair is not legal, including
	// resuming SUSPENDED to anything but the recorded prior status.
	UpdateStatus(ctx context.Context, id int64, from, to DepositStatus, set DepositUpdate) error
}

// CheckpointRepo persists ScannerCheckpoints. Set MUST be called inside the
// same Store.WithTx transaction as the block's record writes (FR-026).
type CheckpointRepo interface {
	Get(ctx context.Context, chainID string) (ScannerCheckpoint, error)
	Set(ctx context.Context, cp ScannerCheckpoint) error
}

// WithdrawalRepo persists WithdrawalRecords and enforces the §5 state machine.
type WithdrawalRepo interface {
	// Create inserts a withdrawal; an idempotency-key hit returns
	// ErrDuplicate (FR-033 — resolve the original via GetByIdempotencyKey).
	Create(ctx context.Context, w WithdrawalRecord) (WithdrawalRecord, error)
	Get(ctx context.Context, withdrawalID string) (WithdrawalRecord, error)
	GetByIdempotencyKey(ctx context.Context, idempotencyKey string) (WithdrawalRecord, error)
	ListByStatus(ctx context.Context, chainID string, status WithdrawalStatus, limit int) ([]WithdrawalRecord, error)

	// UpdateStatus moves a withdrawal from → to, applying set; semantics as
	// DepositRepo.UpdateStatus.
	UpdateStatus(ctx context.Context, withdrawalID string, from, to WithdrawalStatus, set WithdrawalUpdate) error

	// SumCommittedBySource reports the count and summed AmountBaseUnits of the
	// withdrawals for (chainID, sourceAddress) that currently hold a funds
	// reservation — status in {FUNDS_RESERVED, SEQUENCE_RESERVED,
	// TRANSACTION_BUILT, TRANSACTION_SIMULATED, SIGNED, BROADCAST}; REQUESTED /
	// ADDRESS_VALIDATED / COMPLIANCE_APPROVED (not yet reserved) and the
	// terminal states (CONFIRMED / FAILED / CANCELLED) are excluded. A
	// REVIEW_REQUIRED record remains committed when signed_tx_bytes is present:
	// an uncertain signed or broadcast transaction may still spend the funds.
	// Pre-sign review records do not hold a reservation. The caller adds count ×
	// its configured max-fee cap to obtain
	// the conservative reserved total; the on-chain balance must cover that
	// total plus the new withdrawal before FUNDS_RESERVED, so concurrent
	// reserves from one source cannot jointly over-commit the wallet.
	SumCommittedBySource(ctx context.Context, chainID, sourceAddress string) (count int64, sumAmount sdkmath.Int, err error)
}

// SequenceRepo persists SequenceReservations. Implementations serialize
// reservation for one (chain_id, source_address) — Postgres via
// SELECT … FOR UPDATE on the chain_account_locks row (created on demand),
// SQLite via the single-writer connection + BEGIN IMMEDIATE. The UNIQUE
// constraints remain the last-line guarantee in both.
type SequenceRepo interface {
	// Reserve inserts a reservation in status RESERVED. A (chain, source,
	// sequence) hit on a live row returns ErrDuplicate; a hit on a RELEASED
	// row reclaims it (new WorkRef, back to RESERVED). A WorkRef hit returns
	// ErrDuplicate.
	Reserve(ctx context.Context, r SequenceReservation) (SequenceReservation, error)
	GetByWorkRef(ctx context.Context, ref WorkRef) (SequenceReservation, error)
	// ListUnconsumed returns every reservation not in a terminal status for
	// startup/mismatch reconciliation (§6 reconciliation rule).
	ListUnconsumed(ctx context.Context, chainID, sourceAddress string) ([]SequenceReservation, error)

	// UpdateStatus moves a reservation from → to; semantics as
	// DepositRepo.UpdateStatus. Note RELEASED is only reachable from
	// RESERVED or RECONCILIATION_REQUIRED — never from SIGNED/BROADCAST
	// (signed bytes may still redeem the sequence).
	UpdateStatus(ctx context.Context, id int64, from, to SequenceReservationStatus) error
}

// SweepRepo persists SweepJobs and enforces the §7 state machine.
type SweepRepo interface {
	// Create inserts a sweep in status PENDING. An existing non-terminal job
	// for (chain_id, source_address) returns ErrActiveSweepExists; an
	// idempotency-key hit returns ErrDuplicate.
	Create(ctx context.Context, j SweepJob) (SweepJob, error)
	Get(ctx context.Context, sweepID string) (SweepJob, error)
	GetByIdempotencyKey(ctx context.Context, idempotencyKey string) (SweepJob, error)
	// GetActive returns the single non-terminal job for the account, or
	// ErrNotFound.
	GetActive(ctx context.Context, chainID, sourceAddress string) (SweepJob, error)
	ListByStatus(ctx context.Context, chainID string, status SweepStatus, limit int) ([]SweepJob, error)

	// UpdateStatus moves a sweep from → to, applying set; semantics as
	// DepositRepo.UpdateStatus.
	UpdateStatus(ctx context.Context, sweepID string, from, to SweepStatus, set SweepUpdate) error
}

// WatchRepo persists the exchange-controlled address set.
type WatchRepo interface {
	Upsert(ctx context.Context, w WatchedAddress) error
	Get(ctx context.Context, chainID, address string) (WatchedAddress, error)
	ListActive(ctx context.Context, chainID string) ([]WatchedAddress, error)
	SetActive(ctx context.Context, chainID, address string, active bool) error
}

// ControlsRepo persists OperationalControls; every flip is audit-logged.
type ControlsRepo interface {
	// Get returns the controls row, or the all-false zero value when none
	// exists yet (controls default to running).
	Get(ctx context.Context, chainID string) (OperationalControls, error)
	// Apply performs a partial update and writes one audit entry per changed
	// field, atomically.
	Apply(ctx context.Context, chainID string, u ControlsUpdate, actor, reason string) (OperationalControls, error)
	ListAudit(ctx context.Context, chainID string, limit int) ([]ControlsAuditEntry, error)
}

// ReviewRepo persists the operator review queue.
type ReviewRepo interface {
	Open(ctx context.Context, item ReviewItem) (ReviewItem, error)
	Get(ctx context.Context, id int64) (ReviewItem, error)
	ListOpen(ctx context.Context, chainID string, limit int) ([]ReviewItem, error)
	// Resolve stamps ResolvedAt/Resolution; resolving an already-resolved
	// item returns ErrStatusConflict.
	Resolve(ctx context.Context, id int64, resolution string, at time.Time) error
}

// ChainReviewRepo persists FR-044 ChainReviewConditions.
type ChainReviewRepo interface {
	Open(ctx context.Context, c ChainReviewCondition) (ChainReviewCondition, error)
	Get(ctx context.Context, conditionID string) (ChainReviewCondition, error)
	// HasOpen backs the FR-023 crediting gate.
	HasOpen(ctx context.Context, chainID string) (bool, error)
	ListOpen(ctx context.Context, chainID string) ([]ChainReviewCondition, error)
	Resolve(ctx context.Context, conditionID, resolution string, at time.Time) error
}

// ReconRepo persists ReconciliationReports.
type ReconRepo interface {
	SaveReport(ctx context.Context, r ReconciliationReport) error
	GetReport(ctx context.Context, reportID string) (ReconciliationReport, error)
	ListReports(ctx context.Context, chainID string, kind ReconciliationKind, limit int) ([]ReconciliationReport, error)
}

// OutboxRepo persists the transactional outbox.
type OutboxRepo interface {
	// Enqueue inserts an event; a DedupKey hit returns ErrDuplicate.
	Enqueue(ctx context.Context, ev OutboxEvent) (OutboxEvent, error)
	ListPending(ctx context.Context, limit int) ([]OutboxEvent, error)
	MarkDispatched(ctx context.Context, id int64, at time.Time) error
}

// ---------------------------------------------------------------------------
// Store / transactional boundary
// ---------------------------------------------------------------------------

// Tx is the transactional boundary. WithTx runs fn inside one database
// transaction: every repo call made through fn's Store is atomic with the
// others — commit on nil, rollback on error or panic. Required for the
// multi-record invariants: checkpoint advance + block record writes
// (FR-026), deposit CREDITED flip + outbox emission (§3b), sweep create +
// sequence reserve. Implementations run nested WithTx calls inside the
// already-open transaction.
type Tx interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, s Store) error) error
}

// Store aggregates the repositories over one backend (SQLite or Postgres).
type Store interface {
	Tx

	Ledger() LedgerRepo
	Deposits() DepositRepo
	Checkpoints() CheckpointRepo
	Withdrawals() WithdrawalRepo
	Sequences() SequenceRepo
	Sweeps() SweepRepo
	Watch() WatchRepo
	Controls() ControlsRepo
	Review() ReviewRepo
	ChainReview() ChainReviewRepo
	Recon() ReconRepo
	Outbox() OutboxRepo

	Close() error
}
