// Package withdrawals implements the data-model §5 withdrawal state machine
// over storage.Store: the FR-032 pre-sign checklist, the externally supplied
// compliance gate, idempotency-key single execution (FR-033), simulation and
// ceiling-rounded fee calculation (FR-040), and the FR-035 broadcaster
// (broadcast.go) with its eight status distinctions.
//
// Every status write goes through the repository layer, which enforces the
// legal transitions; concurrent drivers lose optimistically with
// ErrStatusConflict instead of double-executing. Sequences come exclusively
// from sequences.Manager (FR-034); signing crosses only the
// signer.TransactionSigner boundary; signed bytes are persisted at SIGNED so
// recovery can rebroadcast identical bytes and never re-sign.
package withdrawals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	sdkmath "cosmossdk.io/math"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/gogoproto/proto"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/sequences"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/tx"
)

var (
	// ErrInvalidRequest reports a request rejected before any record exists.
	ErrInvalidRequest = errors.New("withdrawals: invalid request")

	// ErrAwaitingCompliance reports a withdrawal blocked on the externally
	// supplied compliance gate (FR-031); the kit never decides it.
	ErrAwaitingCompliance = errors.New("withdrawals: awaiting external compliance approval")

	// ErrBelowMinimum reports an amount under the configured exchange minimum.
	ErrBelowMinimum = errors.New("withdrawals: amount below configured minimum")

	// ErrInsufficientSpendable reports a hot wallet that cannot cover
	// amount + max fee.
	ErrInsufficientSpendable = errors.New("withdrawals: insufficient spendable balance")

	// ErrSimulationUnavailable reports the simulate-unavailable queue policy:
	// the withdrawal stays queued at TRANSACTION_BUILT until simulation
	// returns or the operator opts into the static policy.
	ErrSimulationUnavailable = errors.New("withdrawals: simulation unavailable; withdrawal held (simulate_unavailable: queue)")

	// ErrFeeExceedsMax reports a computed fee above max_fee_usovr; the
	// withdrawal is quarantined, never broadcast with an unbounded fee.
	ErrFeeExceedsMax = errors.New("withdrawals: computed fee exceeds max_fee_usovr")

	// ErrPaused reports an operational-controls pause for the needed flow.
	ErrPaused = errors.New("withdrawals: flow paused by operational controls")

	// ErrQuarantined reports a withdrawal moved to REVIEW_REQUIRED by the
	// current call; details are on the review-queue item.
	ErrQuarantined = errors.New("withdrawals: quarantined as REVIEW_REQUIRED")

	// ErrSignerUnavailable reports a signer outage: the withdrawal stays
	// queued (graceful degradation — nothing unsigned is ever broadcast).
	ErrSignerUnavailable = errors.New("withdrawals: signer unavailable; withdrawal remains queued")
)

var digitsRe = regexp.MustCompile(`^[0-9]+$`)

// Chain is the subset of client.Client the workflow needs; *client.Client
// implementations and the failover wrapper satisfy it.
type Chain interface {
	Balance(ctx context.Context, addr, denom string) (sdkmath.Int, error)
	Simulate(ctx context.Context, txBytes []byte) (*client.SimulateResult, error)
	Broadcast(ctx context.Context, txBytes []byte, mode client.BroadcastMode) (*client.BroadcastResult, error)
	Tx(ctx context.Context, hash string) (*client.TxInfo, error)
	NodeStatus(ctx context.Context) (*client.NodeStatus, error)
}

// SimulatePolicy selects the behaviour when the node cannot serve Simulate
// (contracts/adapter-config-and-ops.md `simulate_unavailable`).
type SimulatePolicy string

const (
	// SimulateQueue holds withdrawals and alerts (the safe default).
	SimulateQueue SimulatePolicy = "queue"
	// SimulateStatic uses the configured static MsgSend gas, still bounded
	// by max_fee_usovr. Explicit opt-in.
	SimulateStatic SimulatePolicy = "static"
)

// Config carries every economic value as a string: nothing here may be
// hard-coded in transaction logic (FR-040).
type Config struct {
	ChainID string

	// MinimumWithdrawalUsovr is an integer base-unit string.
	MinimumWithdrawalUsovr string
	// MaxFeeUsovr is an integer base-unit string bounding every fee under
	// both simulate policies.
	MaxFeeUsovr string
	// GasAdjustment is a decimal string (e.g. "1.3") applied to simulated
	// gas with ceiling rounding.
	GasAdjustment string
	// GasPriceUsovr is a decimal usovr-per-gas string (e.g. "0.025"); it
	// must be at or above the live x/globalfee floor.
	GasPriceUsovr string

	SimulateUnavailable SimulatePolicy
	// StaticGasLimit is required when SimulateUnavailable is SimulateStatic.
	StaticGasLimit uint64

	// BroadcastTimeout is the age after which a BROADCAST withdrawal whose
	// transaction cannot be found enters the unknown-after-timeout flow.
	BroadcastTimeout time.Duration
	// Confirmations is the depth required for CONFIRMED. The adapter passes the
	// shared scanner default (deposits.DefaultConfirmations = 2, range 1..12).
	Confirmations uint64

	// ProhibitedDestinations are strict-validation blocklist entries
	// (module accounts, sanctioned addresses).
	ProhibitedDestinations []string

	// KeyRefForSource maps a source address to the signer's opaque key
	// handle. Nil means the source address itself is the key ref.
	KeyRefForSource func(sourceAddress string) string
}

type parsedConfig struct {
	minWithdrawal sdkmath.Int
	maxFee        sdkmath.Int
	gasAdjustment decimal
	gasPrice      decimal
}

func (c Config) parse() (parsedConfig, error) {
	var p parsedConfig
	if c.ChainID == "" {
		return p, fmt.Errorf("%w: chain ID required", ErrInvalidRequest)
	}
	var err error
	if p.minWithdrawal, err = parseBaseUnits(c.MinimumWithdrawalUsovr); err != nil {
		return p, fmt.Errorf("minimum_withdrawal_usovr: %w", err)
	}
	if p.maxFee, err = parseBaseUnits(c.MaxFeeUsovr); err != nil {
		return p, fmt.Errorf("max_fee_usovr: %w", err)
	}
	if p.gasAdjustment, err = parseDecimal(c.GasAdjustment); err != nil {
		return p, fmt.Errorf("gas_adjustment: %w", err)
	}
	if p.gasAdjustment.isZero() {
		return p, fmt.Errorf("gas_adjustment must be positive")
	}
	if p.gasPrice, err = parseDecimal(c.GasPriceUsovr); err != nil {
		return p, fmt.Errorf("gas_price_usovr: %w", err)
	}
	switch c.SimulateUnavailable {
	case SimulateQueue, SimulateStatic:
	case "":
		return p, fmt.Errorf("simulate_unavailable policy required (queue|static)")
	default:
		return p, fmt.Errorf("unknown simulate_unavailable policy %q", c.SimulateUnavailable)
	}
	if c.SimulateUnavailable == SimulateStatic && c.StaticGasLimit == 0 {
		return p, fmt.Errorf("static_gas_limit required with simulate_unavailable: static")
	}
	if c.Confirmations == 0 {
		return p, fmt.Errorf("confirmations must be at least 1")
	}
	return p, nil
}

func parseBaseUnits(s string) (sdkmath.Int, error) {
	if s == "" || !digitsRe.MatchString(s) {
		return sdkmath.Int{}, fmt.Errorf("expected a base-10 integer string, got %q", s)
	}
	n, ok := sdkmath.NewIntFromString(s)
	if !ok {
		return sdkmath.Int{}, fmt.Errorf("integer out of range: %q", s)
	}
	return n, nil
}

// Workflow drives withdrawal records through the §5 state machine.
type Workflow struct {
	store   storage.Store
	chain   Chain
	seq     *sequences.Manager
	signer  signer.TransactionSigner
	cfg     Config
	pcfg    parsedConfig
	deny    address.PSet
	logger  *slog.Logger
	metrics *metrics.Set
}

// Option configures a Workflow.
type Option func(*Workflow)

// WithLogger replaces the default logger.
func WithLogger(l *slog.Logger) Option { return func(w *Workflow) { w.logger = l } }

// WithMetrics attaches the adapter metric set.
func WithMetrics(s *metrics.Set) Option { return func(w *Workflow) { w.metrics = s } }

// New validates cfg and builds a Workflow.
func New(store storage.Store, chain Chain, seq *sequences.Manager, sg signer.TransactionSigner, cfg Config, opts ...Option) (*Workflow, error) {
	pcfg, err := cfg.parse()
	if err != nil {
		return nil, fmt.Errorf("withdrawals: config: %w", err)
	}
	if cfg.BroadcastTimeout <= 0 {
		cfg.BroadcastTimeout = 15 * time.Second
	}
	w := &Workflow{
		store: store, chain: chain, seq: seq, signer: sg,
		cfg: cfg, pcfg: pcfg,
		deny:   address.NewPSet(cfg.ProhibitedDestinations...),
		logger: logging.New("withdrawals"),
	}
	for _, o := range opts {
		o(w)
	}
	return w, nil
}

// Request is an exchange-side withdrawal instruction. AmountBaseUnits is an
// integer usovr string; the idempotency key comes from the exchange's
// upstream system (FR-033).
type Request struct {
	WithdrawalID       string
	IdempotencyKey     string
	SourceAddress      string
	DestinationAddress string
	AmountBaseUnits    string
	Memo               string
}

// Submit records a withdrawal in REQUESTED. A duplicate idempotency key
// returns the ORIGINAL record unchanged — a second signed or broadcast
// transaction for the same key is impossible from this layer down.
func (w *Workflow) Submit(ctx context.Context, req Request) (storage.WithdrawalRecord, error) {
	if req.WithdrawalID == "" || req.IdempotencyKey == "" {
		return storage.WithdrawalRecord{}, fmt.Errorf("%w: withdrawal ID and idempotency key required", ErrInvalidRequest)
	}
	amount, err := parseBaseUnits(req.AmountBaseUnits)
	if err != nil {
		return storage.WithdrawalRecord{}, fmt.Errorf("%w: amount: %v", ErrInvalidRequest, err)
	}
	if !amount.IsPositive() {
		return storage.WithdrawalRecord{}, fmt.Errorf("%w: amount must be positive", ErrInvalidRequest)
	}
	rec, err := w.store.Withdrawals().Create(ctx, storage.WithdrawalRecord{
		WithdrawalID:       req.WithdrawalID,
		IdempotencyKey:     req.IdempotencyKey,
		ChainID:            w.cfg.ChainID,
		SourceAddress:      req.SourceAddress,
		DestinationAddress: req.DestinationAddress,
		Denom:              storage.BaseDenom,
		AmountBaseUnits:    amount,
		Memo:               req.Memo,
		SignMode:           storage.SignModeDirect,
		Status:             storage.WithdrawalRequested,
	})
	if errors.Is(err, storage.ErrDuplicate) {
		if orig, getErr := w.store.Withdrawals().GetByIdempotencyKey(ctx, req.IdempotencyKey); getErr == nil {
			return orig, nil
		}
		return storage.WithdrawalRecord{}, err
	}
	if err != nil {
		return storage.WithdrawalRecord{}, err
	}
	if w.metrics != nil {
		w.metrics.WithdrawalsRequested.WithLabelValues(w.cfg.ChainID).Inc()
	}
	return rec, nil
}

// ValidateAddress moves REQUESTED → ADDRESS_VALIDATED after strict
// destination validation including the prohibited set (FR-032 item 1).
func (w *Workflow) ValidateAddress(ctx context.Context, withdrawalID string) error {
	rec, err := w.store.Withdrawals().Get(ctx, withdrawalID)
	if err != nil {
		return err
	}
	res := address.ValidateAccountAddressStrict(rec.DestinationAddress, w.deny)
	if !res.Valid || res.NormalizedAddress != rec.DestinationAddress {
		return w.quarantine(ctx, rec.WithdrawalID, rec.Status,
			fmt.Sprintf("destination rejected: %s (%s)", res.ErrorCode, res.ErrorMessage))
	}
	if src := address.ValidateAccountAddress(rec.SourceAddress); !src.Valid || src.NormalizedAddress != rec.SourceAddress {
		return w.quarantine(ctx, rec.WithdrawalID, rec.Status,
			fmt.Sprintf("source rejected: %s (%s)", src.ErrorCode, src.ErrorMessage))
	}
	return w.store.Withdrawals().UpdateStatus(ctx, withdrawalID,
		storage.WithdrawalRequested, storage.WithdrawalAddressValidated, storage.WithdrawalUpdate{})
}

// ApproveCompliance records the EXTERNALLY supplied compliance decision
// (FR-031): the kit blocks until this is called and never decides itself.
func (w *Workflow) ApproveCompliance(ctx context.Context, withdrawalID string) error {
	return w.store.Withdrawals().UpdateStatus(ctx, withdrawalID,
		storage.WithdrawalAddressValidated, storage.WithdrawalComplianceApproved, storage.WithdrawalUpdate{})
}

// accountLocker is the per-source serialization primitive both shipped
// storage backends expose (Postgres row lock on chain_account_locks; SQLite
// single-writer). ReserveFunds takes it so concurrent reserves for one source
// cannot both read a stale committed sum and jointly over-commit the balance.
type accountLocker interface {
	AcquireAccountLock(ctx context.Context, chainID, sourceAddress string) error
}

// ReserveFunds moves COMPLIANCE_APPROVED → FUNDS_RESERVED after the minimum
// check and the spendable-balance check (FR-032 items 3–4). The balance must
// cover this withdrawal's amount + max fee PLUS every amount + max fee already
// committed by other in-flight withdrawals from the same source. The
// committed-sum read, the balance check, and the status flip run inside a
// per-source critical section (the same account lock the sequence manager
// uses), so N individually-affordable concurrent reserves cannot each pass
// against the same raw balance and be signed for more than the wallet holds.
// The max-fee cap is the conservative per-withdrawal reservation — the true
// fee is unknown until simulation.
func (w *Workflow) ReserveFunds(ctx context.Context, withdrawalID string) error {
	rec, err := w.store.Withdrawals().Get(ctx, withdrawalID)
	if err != nil {
		return err
	}
	if rec.Status != storage.WithdrawalComplianceApproved {
		if rec.Status == storage.WithdrawalAddressValidated {
			return ErrAwaitingCompliance
		}
		return fmt.Errorf("%w: withdrawal %s is %s", storage.ErrStatusConflict, withdrawalID, rec.Status)
	}
	if rec.AmountBaseUnits.LT(w.pcfg.minWithdrawal) {
		return fmt.Errorf("%w: %s < %s", ErrBelowMinimum, rec.AmountBaseUnits, w.pcfg.minWithdrawal)
	}
	// Chain truth is read outside the critical section (a network round-trip
	// must not be held under the global SQLite write lock); the sequence
	// manager reads account state the same way.
	balance, err := w.chain.Balance(ctx, rec.SourceAddress, storage.BaseDenom)
	if err != nil {
		return fmt.Errorf("withdrawals: balance query: %w", err)
	}
	thisReservation := rec.AmountBaseUnits.Add(w.pcfg.maxFee)

	return w.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		locker, ok := st.(accountLocker)
		if !ok {
			return sequences.ErrNoAccountLock
		}
		if err := locker.AcquireAccountLock(ctx, rec.ChainID, rec.SourceAddress); err != nil {
			return err
		}
		count, committedAmount, err := st.Withdrawals().SumCommittedBySource(ctx, rec.ChainID, rec.SourceAddress)
		if err != nil {
			return err
		}
		committed := committedAmount.Add(w.pcfg.maxFee.MulRaw(count))
		if balance.LT(committed.Add(thisReservation)) {
			return fmt.Errorf("%w: balance %s < committed %s + amount %s + max fee %s",
				ErrInsufficientSpendable, balance, committed, rec.AmountBaseUnits, w.pcfg.maxFee)
		}
		return st.Withdrawals().UpdateStatus(ctx, withdrawalID,
			storage.WithdrawalComplianceApproved, storage.WithdrawalFundsReserved, storage.WithdrawalUpdate{})
	})
}

// ReserveSequence moves FUNDS_RESERVED → SEQUENCE_RESERVED with the single
// reservation bound to this withdrawal (FR-032 item 5, FR-034).
func (w *Workflow) ReserveSequence(ctx context.Context, withdrawalID string) error {
	rec, err := w.store.Withdrawals().Get(ctx, withdrawalID)
	if err != nil {
		return err
	}
	res, err := w.seq.Reserve(ctx, rec.ChainID, rec.SourceAddress,
		storage.WorkRef{Kind: storage.WorkWithdrawal, ID: rec.WithdrawalID})
	if err != nil {
		if errors.Is(err, sequences.ErrReleasedSlotUnusable) {
			return w.quarantine(ctx, rec.WithdrawalID, rec.Status, err.Error())
		}
		return err
	}
	return w.store.Withdrawals().UpdateStatus(ctx, withdrawalID,
		storage.WithdrawalFundsReserved, storage.WithdrawalSequenceReserved, storage.WithdrawalUpdate{
			AccountNumber: &res.AccountNumber,
			Sequence:      &res.Sequence,
		})
}

// Build moves SEQUENCE_RESERVED → TRANSACTION_BUILT after constructing the
// single-MsgSend transaction (FR-036) from the approved record.
func (w *Workflow) Build(ctx context.Context, withdrawalID string) error {
	rec, err := w.store.Withdrawals().Get(ctx, withdrawalID)
	if err != nil {
		return err
	}
	if _, err := tx.BuildMsgSend(rec.SourceAddress, rec.DestinationAddress, rec.AmountBaseUnits.String(), rec.Memo); err != nil {
		return w.quarantine(ctx, rec.WithdrawalID, rec.Status, "build rejected: "+err.Error())
	}
	return w.store.Withdrawals().UpdateStatus(ctx, withdrawalID,
		storage.WithdrawalSequenceReserved, storage.WithdrawalTransactionBuilt, storage.WithdrawalUpdate{})
}

// Simulate moves TRANSACTION_BUILT → TRANSACTION_SIMULATED, persisting
// gas_wanted (simulated), gas_limit (× adjustment, ceil) and the fee
// (gas_limit × gas price, ceil), bounded by max_fee_usovr (FR-032 items
// 6–7, FR-040). When the node cannot simulate, the configured
// simulate_unavailable policy applies: queue holds the withdrawal
// (ErrSimulationUnavailable), static uses the configured MsgSend gas.
func (w *Workflow) Simulate(ctx context.Context, withdrawalID string) error {
	rec, err := w.store.Withdrawals().Get(ctx, withdrawalID)
	if err != nil {
		return err
	}
	if rec.Status != storage.WithdrawalTransactionBuilt {
		return fmt.Errorf("%w: withdrawal %s is %s", storage.ErrStatusConflict, withdrawalID, rec.Status)
	}
	if rec.AccountNumber == nil || rec.Sequence == nil {
		return fmt.Errorf("%w: withdrawal %s has no bound sequence", storage.ErrInvalidRecord, withdrawalID)
	}

	var gasWanted, gasLimit uint64
	simTxBytes, err := w.simulationTxBytes(ctx, rec)
	if err != nil {
		if errors.Is(err, ErrSignerUnavailable) {
			return err
		}
		return w.quarantine(ctx, rec.WithdrawalID, rec.Status, "simulation encoding failed: "+err.Error())
	}
	simRes, simErr := w.chain.Simulate(ctx, simTxBytes)
	switch {
	case simErr == nil:
		gasWanted = simRes.GasUsed
		gasLimit, err = gasLimitFor(simRes.GasUsed, w.pcfg.gasAdjustment)
		if err != nil {
			return w.quarantine(ctx, rec.WithdrawalID, rec.Status, "gas adjustment failed: "+err.Error())
		}
	case errors.Is(simErr, client.ErrSimulateUnavailable):
		if w.cfg.SimulateUnavailable == SimulateQueue {
			w.logger.Warn("simulation unavailable; withdrawal held",
				logging.FieldChainID, rec.ChainID, logging.FieldWithdrawalID, rec.WithdrawalID)
			return ErrSimulationUnavailable
		}
		gasWanted, gasLimit = w.cfg.StaticGasLimit, w.cfg.StaticGasLimit
	default:
		return fmt.Errorf("withdrawals: simulate: %w", simErr)
	}

	fee := feeFor(gasLimit, w.pcfg.gasPrice)
	if fee.GT(w.pcfg.maxFee) {
		qErr := w.quarantine(ctx, rec.WithdrawalID, rec.Status,
			fmt.Sprintf("fee %s exceeds max_fee_usovr %s", fee, w.pcfg.maxFee))
		if qErr != nil && !errors.Is(qErr, ErrQuarantined) {
			return qErr
		}
		return fmt.Errorf("%w: fee %s > %s", ErrFeeExceedsMax, fee, w.pcfg.maxFee)
	}
	return w.store.Withdrawals().UpdateStatus(ctx, withdrawalID,
		storage.WithdrawalTransactionBuilt, storage.WithdrawalTransactionSimulated, storage.WithdrawalUpdate{
			GasWanted:          &gasWanted,
			GasLimit:           &gasLimit,
			FeeAmountBaseUnits: &fee,
		})
}

// simulationTxBytes assembles TxRaw bytes with an empty signature for
// Simulate, from the same builder that will produce the signed bytes. The
// sender public key comes from the signer boundary — it is embedded in the
// simulated AuthInfo exactly as it will be in the signed transaction.
func (w *Workflow) simulationTxBytes(ctx context.Context, rec storage.WithdrawalRecord) ([]byte, error) {
	unsigned, err := tx.BuildMsgSend(rec.SourceAddress, rec.DestinationAddress, rec.AmountBaseUnits.String(), rec.Memo)
	if err != nil {
		return nil, err
	}
	pubKey, err := w.senderPubKey(ctx, rec.SourceAddress)
	if err != nil {
		return nil, err
	}
	// Provisional zero fee: simulation charges nothing and gas metering does
	// not depend on the declared fee.
	provisionalGas := w.cfg.StaticGasLimit
	if provisionalGas == 0 {
		provisionalGas = 200000
	}
	signDocBytes, _, err := unsigned.SignDoc(rec.ChainID, *rec.AccountNumber, *rec.Sequence, tx.Fee{AmountBaseUnits: "0", GasLimit: provisionalGas}, pubKey)
	if err != nil {
		return nil, err
	}
	var doc txtypes.SignDoc
	if err := proto.Unmarshal(signDocBytes, &doc); err != nil {
		return nil, err
	}
	return proto.Marshal(&txtypes.TxRaw{
		BodyBytes:     doc.BodyBytes,
		AuthInfoBytes: doc.AuthInfoBytes,
		Signatures:    [][]byte{{}},
	})
}

// Sign moves TRANSACTION_SIMULATED → SIGNED. It re-runs the FR-032
// checklist tail against the exact sign-doc bytes: the summary derived from
// those bytes (tx.DeriveSummary inside SignDoc) must match the approved
// record field-for-field, including the chain ID. The signer's response is
// verified (signature valid over the doc, public key derives the sender)
// before anything is persisted; failure quarantines and never broadcasts.
func (w *Workflow) Sign(ctx context.Context, withdrawalID string) error {
	rec, err := w.store.Withdrawals().Get(ctx, withdrawalID)
	if err != nil {
		return err
	}
	if rec.Status != storage.WithdrawalTransactionSimulated {
		return fmt.Errorf("%w: withdrawal %s is %s", storage.ErrStatusConflict, withdrawalID, rec.Status)
	}
	controls, err := w.store.Controls().Get(ctx, rec.ChainID)
	if err != nil {
		return err
	}
	if controls.SigningPaused {
		return fmt.Errorf("%w: signing", ErrPaused)
	}

	ref := storage.WorkRef{Kind: storage.WorkWithdrawal, ID: rec.WithdrawalID}
	res, err := w.store.Sequences().GetByWorkRef(ctx, ref)
	if err != nil {
		return err
	}
	if res.Status != storage.SequenceReserved {
		return w.quarantine(ctx, rec.WithdrawalID, rec.Status,
			fmt.Sprintf("sequence reservation is %s, not RESERVED; reconcile before signing", res.Status))
	}
	if rec.AccountNumber == nil || rec.Sequence == nil || rec.FeeAmountBaseUnits == nil || rec.GasLimit == nil ||
		*rec.AccountNumber != res.AccountNumber || *rec.Sequence != res.Sequence {
		return w.quarantine(ctx, rec.WithdrawalID, rec.Status, "record does not match its bound sequence reservation")
	}

	unsigned, err := tx.BuildMsgSend(rec.SourceAddress, rec.DestinationAddress, rec.AmountBaseUnits.String(), rec.Memo)
	if err != nil {
		return w.quarantine(ctx, rec.WithdrawalID, rec.Status, "rebuild rejected: "+err.Error())
	}
	// The sender public key is required BEFORE sign-doc production: it is
	// embedded in AuthInfo.SignerInfos[0].PublicKey inside the signed bytes.
	pubKey, err := w.senderPubKey(ctx, rec.SourceAddress)
	if err != nil {
		if errors.Is(err, ErrSignerUnavailable) {
			return err
		}
		w.countFailed(rec.ChainID, "pubkey")
		return w.quarantine(ctx, rec.WithdrawalID, rec.Status, "sender public key fetch failed: "+err.Error())
	}
	signDocBytes, summary, err := unsigned.SignDoc(rec.ChainID, *rec.AccountNumber, *rec.Sequence, tx.Fee{
		AmountBaseUnits: rec.FeeAmountBaseUnits.String(),
		GasLimit:        *rec.GasLimit,
	}, pubKey)
	if err != nil {
		return w.quarantine(ctx, rec.WithdrawalID, rec.Status, "sign doc encoding failed: "+err.Error())
	}
	if mismatch := w.summaryMismatch(rec, summary); mismatch != "" {
		return w.quarantine(ctx, rec.WithdrawalID, rec.Status, "sign doc does not match approved request: "+mismatch)
	}

	sigRes, err := w.signer.Sign(ctx, signer.SigningRequest{
		KeyRef:       w.keyRef(rec.SourceAddress),
		SignMode:     signer.SignModeDirect,
		SignDocBytes: signDocBytes,
		Summary:      summary,
	})
	if err != nil {
		if errors.Is(err, signer.ErrSignerUnavailable) {
			w.logger.Warn("signer unavailable; withdrawal queued",
				logging.FieldChainID, rec.ChainID, logging.FieldWithdrawalID, rec.WithdrawalID)
			return fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
		}
		w.countFailed(rec.ChainID, "sign")
		return w.quarantine(ctx, rec.WithdrawalID, rec.Status,
			fmt.Sprintf("signer refused (%s)", signer.CodeOf(err)))
	}

	// Adapter-side verification at the trust boundary: signature valid over
	// SHA-256(signDocBytes) AND public key derives the sender. Failure means
	// an ambiguous signer outcome — a signature exists, so quarantine the
	// withdrawal AND move the reservation out of RESERVED atomically.
	if err := VerifySignedResponse(signDocBytes, sigRes, rec.SourceAddress); err != nil {
		w.countFailed(rec.ChainID, "verify")
		return w.quarantinePostSignature(ctx, rec.WithdrawalID, rec.Status, res.ID, "signed response verification failed: "+err.Error())
	}
	signedTxBytes, txHash, err := tx.Assemble(unsigned, tx.SignatureResponse{
		Signature:        sigRes.Signature,
		PubKeyCompressed: sigRes.PubKeyCompressed,
	})
	if err != nil {
		w.countFailed(rec.ChainID, "assemble")
		return w.quarantinePostSignature(ctx, rec.WithdrawalID, rec.Status, res.ID, "assembly failed: "+err.Error())
	}

	return w.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if err := st.Withdrawals().UpdateStatus(ctx, withdrawalID,
			storage.WithdrawalTransactionSimulated, storage.WithdrawalSigned, storage.WithdrawalUpdate{
				SignedTxBytes: signedTxBytes,
				TxHash:        &txHash,
			}); err != nil {
			return err
		}
		return st.Sequences().UpdateStatus(ctx, res.ID, storage.SequenceReserved, storage.SequenceSigned)
	})
}

// keyRef maps a source address to the signer's opaque key handle.
func (w *Workflow) keyRef(source string) string {
	if w.cfg.KeyRefForSource != nil {
		return w.cfg.KeyRefForSource(source)
	}
	return source
}

// senderPubKey fetches the sender's 33-byte compressed secp256k1 public key
// through the signer boundary (required before sign-doc production — the key
// is embedded in AuthInfo). Signer outages surface as ErrSignerUnavailable so
// callers keep the withdrawal queued instead of quarantining.
func (w *Workflow) senderPubKey(ctx context.Context, source string) ([]byte, error) {
	res, err := w.signer.GetPublicKey(ctx, signer.PublicKeyRequest{KeyRef: w.keyRef(source)})
	if err != nil {
		if errors.Is(err, signer.ErrSignerUnavailable) {
			w.logger.Warn("signer unavailable; public key not fetched",
				logging.FieldChainID, w.cfg.ChainID, logging.FieldAddress, source)
			return nil, fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
		}
		return nil, fmt.Errorf("withdrawals: signer public key: %w", err)
	}
	return res.PublicKeyCompressed, nil
}

// summaryMismatch compares the summary derived from the sign-doc bytes to
// the approved record (FR-032 body-matches-approved + chain-ID check).
func (w *Workflow) summaryMismatch(rec storage.WithdrawalRecord, s signer.SigningSummary) string {
	checks := []struct{ field, doc, approved string }{
		{"chain_id", s.ChainID, w.cfg.ChainID},
		{"record_chain_id", s.ChainID, rec.ChainID},
		{"sender", s.SenderAddress, rec.SourceAddress},
		{"recipient", s.RecipientAddress, rec.DestinationAddress},
		{"amount", s.AmountBaseUnits, rec.AmountBaseUnits.String()},
		{"denom", s.Denom, rec.Denom},
		{"memo", s.Memo, rec.Memo},
		{"fee", s.FeeBaseUnits, rec.FeeAmountBaseUnits.String()},
		{"sequence", s.Sequence, fmt.Sprintf("%d", *rec.Sequence)},
		{"account_number", s.AccountNumber, fmt.Sprintf("%d", *rec.AccountNumber)},
		{"message_type", s.MessageType, signer.MsgTypeBankSend},
	}
	for _, c := range checks {
		if c.doc != c.approved {
			return c.field
		}
	}
	return ""
}

// Cancel moves any pre-SIGNED withdrawal to CANCELLED, releasing its
// sequence reservation when one is bound and still RESERVED.
func (w *Workflow) Cancel(ctx context.Context, withdrawalID string) error {
	rec, err := w.store.Withdrawals().Get(ctx, withdrawalID)
	if err != nil {
		return err
	}
	return w.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if err := st.Withdrawals().UpdateStatus(ctx, withdrawalID, rec.Status, storage.WithdrawalCancelled, storage.WithdrawalUpdate{}); err != nil {
			return err
		}
		res, err := st.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: withdrawalID})
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if res.Status == storage.SequenceReserved {
			return st.Sequences().UpdateStatus(ctx, res.ID, storage.SequenceReserved, storage.SequenceReleased)
		}
		return nil
	})
}

// quarantinePostSignature is quarantine for the two signer-outcome branches
// where the signer has already produced a signature (verify-failure and
// assemble-failure): it flips the withdrawal to REVIEW_REQUIRED, moves the
// bound reservation RESERVED -> RECONCILIATION_REQUIRED, and opens the review
// row — all in ONE transaction, returning ErrQuarantined. The reservation move
// is fatal (never swallowed): a signature now exists, so the slot MUST leave
// RESERVED atomically with the withdrawal flip. Otherwise a dropped best-effort
// move could strand the withdrawal in REVIEW_REQUIRED with a RESERVED
// reservation and an outstanding redeemable signature, which review resolution
// would wrongly treat as pre-sign and release (double-obligation).
func (w *Workflow) quarantinePostSignature(ctx context.Context, withdrawalID string, from storage.WithdrawalStatus, reservationID int64, reason string) error {
	err := w.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if err := st.Withdrawals().UpdateStatus(ctx, withdrawalID, from, storage.WithdrawalReviewRequired, storage.WithdrawalUpdate{}); err != nil {
			return err
		}
		if err := st.Sequences().UpdateStatus(ctx, reservationID, storage.SequenceReserved, storage.SequenceReconciliationRequired); err != nil {
			return err
		}
		_, err := st.Review().Open(ctx, storage.ReviewItem{
			ChainID: w.cfg.ChainID,
			Kind:    storage.ReviewKindWithdrawal,
			RefID:   withdrawalID,
			Reason:  reason,
		})
		return err
	})
	if err != nil {
		return err
	}
	w.logger.Warn("withdrawal quarantined (post-signature)",
		logging.FieldChainID, w.cfg.ChainID,
		logging.FieldWithdrawalID, withdrawalID,
		"reason", reason,
	)
	return fmt.Errorf("%w: %s", ErrQuarantined, reason)
}

// quarantine flips the withdrawal to REVIEW_REQUIRED and opens a
// review-queue item atomically, returning ErrQuarantined.
func (w *Workflow) quarantine(ctx context.Context, withdrawalID string, from storage.WithdrawalStatus, reason string) error {
	err := w.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if err := st.Withdrawals().UpdateStatus(ctx, withdrawalID, from, storage.WithdrawalReviewRequired, storage.WithdrawalUpdate{}); err != nil {
			return err
		}
		_, err := st.Review().Open(ctx, storage.ReviewItem{
			ChainID: w.cfg.ChainID,
			Kind:    storage.ReviewKindWithdrawal,
			RefID:   withdrawalID,
			Reason:  reason,
		})
		return err
	})
	if err != nil {
		return err
	}
	w.logger.Warn("withdrawal quarantined",
		logging.FieldChainID, w.cfg.ChainID,
		logging.FieldWithdrawalID, withdrawalID,
		"reason", reason,
	)
	return fmt.Errorf("%w: %s", ErrQuarantined, reason)
}

func (w *Workflow) countFailed(chainID, stage string) {
	if w.metrics != nil {
		w.metrics.WithdrawalsFailed.WithLabelValues(chainID, stage).Inc()
	}
}
