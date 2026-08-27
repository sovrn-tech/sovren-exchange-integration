// Package sweeps implements the data-model §7 sweep state machine over
// storage.Store with withdrawal-grade durability (FR-037–039):
//
//   - Exactly-once: the DB partial-unique constraint (at most one
//     non-terminal SweepJob per (chain_id, source_address)) is honored —
//     storage.ErrActiveSweepExists means a live sweep exists and NO second
//     one is ever started, no matter how many balance snapshots arrive. The
//     FR-039 idempotency key SWEEP:{chain_id}:{source}:{balance}:{height}
//     is the subordinate request-dedup layer.
//   - Durability: sweeps take a sequences.Manager reservation via
//     work_ref{kind: SWEEP}, persist the signed bytes at SIGNED, and follow
//     the same unknown-outcome semantics as withdrawals — search first,
//     rebroadcast the identical persisted bytes, quarantine the reservation
//     on ambiguity. Never re-sign, never auto-release.
//   - Strategies (FR-038): FEE_RESERVE, FEE_FUND, THRESHOLD_ONLY,
//     CUSTODY_ABSTRACTED. FEE_FUND emits the fee-wallet funding MsgSend
//     through this same engine (its own SweepJob + reservation + persisted
//     bytes); the scanner ledger-classifies that transfer FEE_FUNDING —
//     internal, never a customer credit.
//   - Insufficient fee headroom defers the job (PENDING → DEFERRED): no
//     retry loop — the deferral is counted (sovren_sweeps_deferred_total),
//     logged, and revisited only when conditions change (Revisit). A
//     deferred FEE_RESERVE/THRESHOLD_ONLY job whose stored amount is stale
//     (the fee moved between plan and execution) is terminal-CANCELLED by
//     Revisit once a fresh plan at current conditions passes, so the next
//     Plan pass re-creates it with a recomputed amount — never a
//     permanently stuck DEFERRED at a static balance.
//
// Every economic threshold (minimum_sweep_amount_usovr,
// maximum_fee_percentage_for_sweep, fee_reserve_usovr) is configuration,
// never a code constant (FR-038/FR-040).
package sweeps

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	sdkmath "cosmossdk.io/math"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

var (
	// ErrPaused reports an operational-controls pause for the sweep flow
	// (or the global signing/broadcast switches at those steps).
	ErrPaused = errors.New("sweeps: flow paused by operational controls")

	// ErrDeferred reports a sweep moved to DEFERRED by the current call:
	// fee headroom, fee percentage, or minimum-amount conditions failed.
	// The job is revisited when conditions change — never retried in a loop.
	ErrDeferred = errors.New("sweeps: sweep deferred (insufficient fee headroom or uneconomical)")

	// ErrFundingPending reports a FEE_FUND sweep waiting on its fee-wallet
	// funding transaction; the sweep stays PENDING until it confirms.
	ErrFundingPending = errors.New("sweeps: fee funding transfer in flight")

	// ErrNoFeeWallet reports the FEE_FUND strategy without an active
	// FEE_WALLET entry in the watch set — a configuration problem.
	ErrNoFeeWallet = errors.New("sweeps: FEE_FUND requires an active FEE_WALLET watched address")

	// ErrSimulationUnavailable reports the simulate-unavailable queue
	// policy: sweep processing holds (alert emitted), never guesses gas.
	ErrSimulationUnavailable = errors.New("sweeps: simulation unavailable; sweep held (simulate_unavailable: queue)")

	// ErrSignerUnavailable reports a signer outage: the sweep stays queued
	// at BUILT; nothing unsigned is ever broadcast.
	ErrSignerUnavailable = errors.New("sweeps: signer unavailable; sweep remains queued")

	// ErrQuarantined reports an ambiguous signer/broadcast outcome: the
	// sequence reservation is RECONCILIATION_REQUIRED and the sweep holds
	// its non-terminal status (still blocking any second live sweep) until
	// Recover rebroadcasts the persisted bytes or an operator resolves it.
	ErrQuarantined = errors.New("sweeps: quarantined; reservation requires reconciliation")

	// ErrNoSigner reports a transaction-emitting strategy configured
	// without a signer.
	ErrNoSigner = errors.New("sweeps: no signer configured")
)

// SimulatePolicy selects the behaviour when the node cannot serve Simulate
// (contracts/adapter-config-and-ops.md `simulate_unavailable`).
type SimulatePolicy string

const (
	// SimulateQueue holds sweeps and alerts (the safe default).
	SimulateQueue SimulatePolicy = "queue"
	// SimulateStatic uses the configured static MsgSend gas. Explicit opt-in.
	SimulateStatic SimulatePolicy = "static"
)

// fundingIdemPrefix marks fee-wallet funding jobs; their idempotency key is
// FEEFUND:{chain_id}:{fee_wallet}:{parent_sweep_id} — one funding per parent
// sweep, ever (no funding retry loop).
const fundingIdemPrefix = "FEEFUND:"

// Config carries every economic value as a string; nothing here may be
// hard-coded in sweep logic (FR-038/FR-040).
type Config struct {
	ChainID string

	// Strategy is the configured fee-handling strategy (FR-038).
	Strategy storage.SweepStrategy
	// HotWallet is the sweep destination (required unless
	// CUSTODY_ABSTRACTED).
	HotWallet string

	// MinimumSweepAmountUsovr gates sweep creation and the final swept
	// amount (integer base-unit string).
	MinimumSweepAmountUsovr string
	// MaximumFeePercentageForSweep is a decimal percentage string
	// (e.g. "1.0"); a sweep whose fee exceeds amount × pct/100 defers.
	MaximumFeePercentageForSweep string
	// FeeReserveUsovr is the balance left behind under FEE_RESERVE
	// (integer base-unit string).
	FeeReserveUsovr string

	// FeeWalletMaxSpendUsovr caps the fee wallet's cumulative FEE_FUND funding
	// spend within FeeWalletSpendWindowBlocks recent blocks (integer base-unit
	// string; "" or "0" disables the cap). A funding leg that would breach the
	// cap DEFERS the sweep instead of spending — a guardrail so a dust flood or
	// an address-derivation bug cannot drain the fee wallet one economical leg
	// at a time (recommended for the FEE_FUND model; off by default).
	FeeWalletMaxSpendUsovr string
	// FeeWalletSpendWindowBlocks is the rolling block window the cap sums over;
	// required (> 0) when FeeWalletMaxSpendUsovr is set.
	FeeWalletSpendWindowBlocks uint64

	// GasAdjustment is a decimal string applied to simulated gas with
	// ceiling rounding; GasPriceUsovr is the decimal usovr-per-gas price.
	GasAdjustment string
	GasPriceUsovr string

	SimulateUnavailable SimulatePolicy
	// StaticGasLimit is required when SimulateUnavailable is SimulateStatic.
	StaticGasLimit uint64

	// BroadcastTimeout is the age after which a BROADCAST sweep whose
	// transaction cannot be found enters the unknown flow.
	BroadcastTimeout time.Duration
	// Confirmations is the depth required for CONFIRMED.
	Confirmations uint64

	// KeyRefForSource maps a source address to the signer's opaque key
	// handle. Nil means the source address itself is the key ref.
	KeyRefForSource func(sourceAddress string) string
}

type parsedConfig struct {
	minSweep          sdkmath.Int
	maxFeePct         decimal
	feeReserve        sdkmath.Int
	feeWalletMaxSpend sdkmath.Int // zero = cap disabled
	gasAdj            decimal
	gasPrice          decimal
}

var digitsRe = regexp.MustCompile(`^[0-9]+$`)

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

func (c Config) parse() (parsedConfig, error) {
	var p parsedConfig
	if c.ChainID == "" {
		return p, fmt.Errorf("chain ID required")
	}
	if !c.Strategy.Valid() {
		return p, fmt.Errorf("strategy %q is not one of %v", c.Strategy, storage.AllSweepStrategies)
	}
	if c.Strategy != storage.StrategyCustodyAbstract && c.HotWallet == "" {
		return p, fmt.Errorf("hot_wallet required for strategy %s", c.Strategy)
	}
	var err error
	if p.minSweep, err = parseBaseUnits(c.MinimumSweepAmountUsovr); err != nil {
		return p, fmt.Errorf("minimum_sweep_amount_usovr: %w", err)
	}
	if p.maxFeePct, err = parseDecimal(c.MaximumFeePercentageForSweep); err != nil {
		return p, fmt.Errorf("maximum_fee_percentage_for_sweep: %w", err)
	}
	if p.feeReserve, err = parseBaseUnits(c.FeeReserveUsovr); err != nil {
		return p, fmt.Errorf("fee_reserve_usovr: %w", err)
	}
	// Optional fee-wallet spend cap (guards the FEE_FUND funding path).
	p.feeWalletMaxSpend = sdkmath.ZeroInt()
	if s := c.FeeWalletMaxSpendUsovr; s != "" && s != "0" {
		if p.feeWalletMaxSpend, err = parseBaseUnits(s); err != nil {
			return p, fmt.Errorf("fee_wallet_max_spend_usovr: %w", err)
		}
		if c.FeeWalletSpendWindowBlocks == 0 {
			return p, fmt.Errorf("fee_wallet_spend_window_blocks must be > 0 when fee_wallet_max_spend_usovr is set")
		}
	}
	if c.Strategy == storage.StrategyCustodyAbstract {
		// No transaction is ever emitted; gas parameters are unused.
		return p, nil
	}
	if p.gasAdj, err = parseDecimal(c.GasAdjustment); err != nil {
		return p, fmt.Errorf("gas_adjustment: %w", err)
	}
	if p.gasAdj.isZero() {
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

// IdempotencyKey is the FR-039 sweep request-dedup key:
// SWEEP:{chain_id}:{source_address}:{balance_snapshot_usovr}:{snapshot_height}.
func IdempotencyKey(chainID, source string, balance sdkmath.Int, height uint64) string {
	return fmt.Sprintf("SWEEP:%s:%s:%s:%d", chainID, source, balance, height)
}

// FundingIdempotencyKey dedups the fee-wallet funding transfer to exactly
// one per parent sweep — a funding MsgSend can never be emitted twice for
// the same sweep, in any crash order.
func FundingIdempotencyKey(chainID, feeWallet, parentSweepID string) string {
	return fundingIdemPrefix + chainID + ":" + feeWallet + ":" + parentSweepID
}

// IsFundingJob reports whether j is a fee-wallet funding leg rather than a
// customer-address sweep.
func IsFundingJob(j storage.SweepJob) bool {
	return strings.HasPrefix(j.IdempotencyKey, fundingIdemPrefix)
}

// sweepIDFor derives a deterministic job ID from the idempotency key, so a
// replayed snapshot regenerates the same identity end to end.
func sweepIDFor(idempotencyKey string) string {
	sum := sha256.Sum256([]byte(idempotencyKey))
	return "SWP-" + strings.ToUpper(hex.EncodeToString(sum[:8]))
}

func fundingSweepID(parentSweepID string) string { return "FUND-" + parentSweepID }

// ---------------------------------------------------------------------------
// Exact decimal arithmetic (no floats in money paths — FR-017): num/den with
// den a power of 10, ceiling rounding on every multiply.
// ---------------------------------------------------------------------------

var decRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

type decimal struct {
	num *big.Int
	den *big.Int
}

func parseDecimal(s string) (decimal, error) {
	if !decRe.MatchString(s) {
		return decimal{}, fmt.Errorf("invalid decimal %q: expected digits with optional fraction", s)
	}
	whole, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		whole, frac = s[:i], s[i+1:]
	}
	num, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok {
		return decimal{}, fmt.Errorf("invalid decimal %q", s)
	}
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(frac))), nil)
	return decimal{num: num, den: den}, nil
}

func (d decimal) isZero() bool { return d.num == nil || d.num.Sign() == 0 }

// ceilMulBig returns ceil(n × d).
func (d decimal) ceilMulBig(n *big.Int) *big.Int {
	prod := new(big.Int).Mul(n, d.num)
	prod.Add(prod, new(big.Int).Sub(d.den, big.NewInt(1)))
	return prod.Quo(prod, d.den)
}

// ceilMulU64 returns ceil(n × d) as uint64, erroring on overflow.
func (d decimal) ceilMulU64(n uint64) (uint64, error) {
	v := d.ceilMulBig(new(big.Int).SetUint64(n))
	if !v.IsUint64() {
		return 0, fmt.Errorf("value %s exceeds uint64", v)
	}
	return v.Uint64(), nil
}

// feeWithinPercentage reports fee ≤ amount × pct/100, exactly.
func feeWithinPercentage(fee, amount sdkmath.Int, pct decimal) bool {
	lhs := new(big.Int).Mul(fee.BigInt(), big.NewInt(100))
	lhs.Mul(lhs, pct.den)
	rhs := new(big.Int).Mul(amount.BigInt(), pct.num)
	return lhs.Cmp(rhs) <= 0
}
