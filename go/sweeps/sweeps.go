package sweeps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	sdkmath "cosmossdk.io/math"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/gogoproto/proto"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/sequences"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/tx"
)

// Chain is the subset of client.Client the engine needs; *client.Client
// implementations and the failover wrapper satisfy it.
type Chain interface {
	Account(ctx context.Context, addr string) (accountNumber, sequence uint64, err error)
	Balance(ctx context.Context, addr, denom string) (sdkmath.Int, error)
	Simulate(ctx context.Context, txBytes []byte) (*client.SimulateResult, error)
	Broadcast(ctx context.Context, txBytes []byte, mode client.BroadcastMode) (*client.BroadcastResult, error)
	Tx(ctx context.Context, hash string) (*client.TxInfo, error)
	NodeStatus(ctx context.Context) (*client.NodeStatus, error)
}

// Engine drives SweepJobs through the §7 state machine. Safe for concurrent
// use: every status write goes through the repository layer, which enforces
// the legal transitions; concurrent drivers lose optimistically with
// ErrStatusConflict instead of double-executing, and the partial-unique
// constraint plus the sequence-reservation UNIQUE keys are the last-line
// exactly-once guarantees.
type Engine struct {
	store   storage.Store
	chain   Chain
	seq     *sequences.Manager
	signer  signer.TransactionSigner
	cfg     Config
	pcfg    parsedConfig
	logger  *slog.Logger
	metrics *metrics.Set
}

// Option configures an Engine.
type Option func(*Engine)

// WithLogger replaces the default logger.
func WithLogger(l *slog.Logger) Option { return func(e *Engine) { e.logger = l } }

// WithMetrics attaches the adapter metric set (sweeps-deferred counter).
func WithMetrics(s *metrics.Set) Option { return func(e *Engine) { e.metrics = s } }

// New validates cfg and builds an Engine. sg may be nil only under
// CUSTODY_ABSTRACTED (no transaction is ever emitted).
func New(store storage.Store, chain Chain, seq *sequences.Manager, sg signer.TransactionSigner, cfg Config, opts ...Option) (*Engine, error) {
	pcfg, err := cfg.parse()
	if err != nil {
		return nil, fmt.Errorf("sweeps: config: %w", err)
	}
	if cfg.BroadcastTimeout <= 0 {
		cfg.BroadcastTimeout = 15 * time.Second
	}
	e := &Engine{
		store: store, chain: chain, seq: seq, signer: sg,
		cfg: cfg, pcfg: pcfg,
		logger: logging.New("sweeps"),
	}
	for _, o := range opts {
		o(e)
	}
	return e, nil
}

// destination returns the MsgSend recipient for a job: the hot wallet for
// customer sweeps, the parent's source address for funding legs.
func (e *Engine) destination(j storage.SweepJob) string { return j.HotWalletAddress }

// reserveFor returns the balance the strategy leaves behind at the source.
func (e *Engine) reserveFor(j storage.SweepJob) sdkmath.Int {
	if !IsFundingJob(j) && j.Strategy == storage.StrategyFeeReserve {
		return e.pcfg.feeReserve
	}
	return sdkmath.ZeroInt()
}

func (e *Engine) keyRef(source string) string {
	if e.cfg.KeyRefForSource != nil {
		return e.cfg.KeyRefForSource(source)
	}
	return source
}

// senderPubKey fetches the source's 33-byte compressed secp256k1 public key
// through the signer boundary (required before sign-doc production — the key
// is embedded in AuthInfo, including in simulation bytes).
func (e *Engine) senderPubKey(ctx context.Context, source string) ([]byte, error) {
	if e.signer == nil {
		return nil, ErrNoSigner
	}
	res, err := e.signer.GetPublicKey(ctx, signer.PublicKeyRequest{KeyRef: e.keyRef(source)})
	if err != nil {
		if errors.Is(err, signer.ErrSignerUnavailable) {
			return nil, fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
		}
		return nil, fmt.Errorf("sweeps: signer public key: %w", err)
	}
	return res.PublicKeyCompressed, nil
}

func (e *Engine) countDeferred() {
	if e.metrics != nil {
		e.metrics.SweepsDeferred.WithLabelValues(e.cfg.ChainID).Inc()
	}
}

// countFeeFunding records a confirmed FEE_FUND funding leg's spend so the
// AbnormalFeeFundingVolume alert can watch the fee wallet's drain rate.
func (e *Engine) countFeeFunding(amount sdkmath.Int) {
	if e.metrics != nil && amount.IsPositive() {
		e.metrics.FeeFundingUsovr.WithLabelValues(e.cfg.ChainID).Add(float64(amount.Int64()))
	}
}

// recordFeeFundingSpend durably records a confirmed FEE_FUND funding leg's spend
// (fee wallet → deposit address) inside the caller's confirm transaction, so the
// fee-wallet spend cap reads authoritative spend the instant the leg confirms
// rather than waiting on the deposit scanner. A non-funding sweep is a no-op; a
// re-confirm (ErrDuplicate on the (chain,tx) key) is tolerated as a no-op.
func (e *Engine) recordFeeFundingSpend(ctx context.Context, st storage.Store, j storage.SweepJob, height uint64) error {
	if !IsFundingJob(j) || j.TxHash == nil {
		return nil
	}
	_, err := st.Ledger().AppendFeeFundingSpend(ctx, storage.FeeFundingSpend{
		ChainID:          j.ChainID,
		TxHash:           *j.TxHash,
		FeeWalletAddress: j.SourceAddress,
		AmountBaseUnits:  j.AmountBaseUnits,
		BlockHeight:      height,
		CreatedAt:        time.Now().UTC(),
	})
	if err != nil && !errors.Is(err, storage.ErrDuplicate) {
		return err
	}
	return nil
}

// sweepPaused loads the FR-051 controls and reports the sweep-flow pause.
func (e *Engine) controls(ctx context.Context) (storage.OperationalControls, error) {
	return e.store.Controls().Get(ctx, e.cfg.ChainID)
}

// estimateFee simulates a MsgSend source→dest for amount and returns
// (fee, gasLimit) under the configured adjustment, price, and
// simulate-unavailable policy. MsgSend gas DEPENDS on the amount — a
// full-balance send deletes the sender's coin record where a partial send
// rewrites it, and digit widths change the tx size — so callers must
// simulate the amount the transaction will actually carry: execution
// (Prepare/Revisit) passes the job's amount, and plan-time callers go
// through planFeeAmount's fixed point.
func (e *Engine) estimateFee(ctx context.Context, source, dest string, amount sdkmath.Int, accountNumber, sequence uint64) (sdkmath.Int, uint64, error) {
	simBytes, err := e.simulationTxBytes(ctx, source, dest, amount, accountNumber, sequence)
	if err != nil {
		if errors.Is(err, ErrSignerUnavailable) || errors.Is(err, ErrNoSigner) {
			return sdkmath.Int{}, 0, err
		}
		return sdkmath.Int{}, 0, fmt.Errorf("sweeps: simulation encoding: %w", err)
	}
	var gasLimit uint64
	res, simErr := e.chain.Simulate(ctx, simBytes)
	switch {
	case simErr == nil:
		if res.GasUsed == 0 {
			return sdkmath.Int{}, 0, fmt.Errorf("sweeps: simulation returned zero gas")
		}
		if gasLimit, err = e.pcfg.gasAdj.ceilMulU64(res.GasUsed); err != nil {
			return sdkmath.Int{}, 0, fmt.Errorf("sweeps: gas adjustment: %w", err)
		}
	case errors.Is(simErr, client.ErrSimulateUnavailable):
		if e.cfg.SimulateUnavailable == SimulateQueue {
			return sdkmath.Int{}, 0, ErrSimulationUnavailable
		}
		gasLimit = e.cfg.StaticGasLimit
	default:
		return sdkmath.Int{}, 0, fmt.Errorf("sweeps: simulate: %w", simErr)
	}
	fee := sdkmath.NewIntFromBigInt(e.pcfg.gasPrice.ceilMulBig(sdkmath.NewIntFromUint64(gasLimit).BigInt()))
	return fee, gasLimit, nil
}

// simulationTxBytes assembles TxRaw bytes with an empty signature for
// Simulate, from the same builder that will produce the signed bytes. The
// source public key comes from the signer boundary — embedded in the
// simulated AuthInfo exactly as it will be in the signed transaction.
func (e *Engine) simulationTxBytes(ctx context.Context, source, dest string, amount sdkmath.Int, accountNumber, sequence uint64) ([]byte, error) {
	unsigned, err := tx.BuildMsgSend(source, dest, amount.String(), "")
	if err != nil {
		return nil, err
	}
	pubKey, err := e.senderPubKey(ctx, source)
	if err != nil {
		return nil, err
	}
	provisionalGas := e.cfg.StaticGasLimit
	if provisionalGas == 0 {
		provisionalGas = 200000
	}
	signDocBytes, _, err := unsigned.SignDoc(e.cfg.ChainID, accountNumber, sequence, tx.Fee{AmountBaseUnits: "0", GasLimit: provisionalGas}, pubKey)
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

// defer moves a PENDING job to DEFERRED, releasing a still-unsigned
// reservation when one is bound. Deferral is surfaced (counter + warn log)
// and revisited only when conditions change — never a retry loop.
func (e *Engine) deferJob(ctx context.Context, j storage.SweepJob, reason string) error {
	err := e.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if err := st.Sweeps().UpdateStatus(ctx, j.SweepID, storage.SweepPending, storage.SweepDeferred, storage.SweepUpdate{}); err != nil {
			return err
		}
		return e.releaseUnsignedReservation(ctx, st, j.SweepID)
	})
	if err != nil {
		return err
	}
	e.countDeferred()
	e.logger.Warn("sweep deferred",
		logging.FieldChainID, e.cfg.ChainID,
		logging.FieldSweepID, j.SweepID,
		logging.FieldAddress, j.SourceAddress,
		"reason", reason,
	)
	return fmt.Errorf("%w: %s", ErrDeferred, reason)
}

// releaseUnsignedReservation releases the job's reservation iff it is still
// RESERVED (nothing signed). SIGNED/BROADCAST reservations are never
// released from here — signed bytes may still redeem the sequence.
func (e *Engine) releaseUnsignedReservation(ctx context.Context, st storage.Store, sweepID string) error {
	res, err := st.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkSweep, ID: sweepID})
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if res.Status != storage.SequenceReserved {
		return nil
	}
	err = st.Sequences().UpdateStatus(ctx, res.ID, storage.SequenceReserved, storage.SequenceReleased)
	if errors.Is(err, storage.ErrStatusConflict) {
		return nil
	}
	return err
}

// quarantineReservation moves the job's reservation to
// RECONCILIATION_REQUIRED, tolerating races with reconciliation.
func (e *Engine) quarantineReservation(ctx context.Context, st storage.Store, sweepID string, from storage.SequenceReservationStatus) error {
	res, err := st.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkSweep, ID: sweepID})
	if err != nil {
		return err
	}
	if res.Status == storage.SequenceReconciliationRequired || res.Status == storage.SequenceConsumed {
		return nil
	}
	err = st.Sequences().UpdateStatus(ctx, res.ID, from, storage.SequenceReconciliationRequired)
	if errors.Is(err, storage.ErrStatusConflict) {
		return nil
	}
	return err
}

// advanceReservation moves the job's reservation from → to, treating a
// concurrent identical move as success.
func (e *Engine) advanceReservation(ctx context.Context, st storage.Store, sweepID string, from, to storage.SequenceReservationStatus) error {
	res, err := st.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkSweep, ID: sweepID})
	if err != nil {
		return err
	}
	if res.Status == to {
		return nil
	}
	err = st.Sequences().UpdateStatus(ctx, res.ID, from, to)
	if errors.Is(err, storage.ErrStatusConflict) {
		return nil
	}
	return err
}

// consumeReservation marks the job's reservation CONSUMED from whichever
// live status it holds (inclusion on chain is definitive).
func (e *Engine) consumeReservation(ctx context.Context, st storage.Store, sweepID string) error {
	res, err := st.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkSweep, ID: sweepID})
	if err != nil {
		return err
	}
	switch res.Status {
	case storage.SequenceConsumed:
		return nil
	case storage.SequenceSigned, storage.SequenceBroadcast, storage.SequenceReconciliationRequired:
		err := st.Sequences().UpdateStatus(ctx, res.ID, res.Status, storage.SequenceConsumed)
		if errors.Is(err, storage.ErrStatusConflict) {
			return nil
		}
		return err
	default:
		return fmt.Errorf("%w: reservation for sweep %s is %s at inclusion", storage.ErrStatusConflict, sweepID, res.Status)
	}
}

// markDepositsSwept flips the job's covered deposits SWEEP_PENDING → SWEPT
// with the sweep transaction hash (data model §3b). Already-swept deposits
// and concurrent flips are tolerated.
func (e *Engine) markDepositsSwept(ctx context.Context, st storage.Store, j storage.SweepJob, txHash string) error {
	for _, id := range j.DepositIDs {
		d, err := st.Deposits().GetByID(ctx, id)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return err
		}
		if d.Status != storage.DepositSweepPending {
			continue
		}
		err = st.Deposits().UpdateStatus(ctx, id, storage.DepositSweepPending, storage.DepositSwept,
			storage.DepositUpdate{SweepTxHash: &txHash})
		if err != nil && !errors.Is(err, storage.ErrStatusConflict) {
			return err
		}
	}
	return nil
}
