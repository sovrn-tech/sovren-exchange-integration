package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/query"

	globalfeev1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/globalfee/v1"
	txqueryv1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/txquery/v1"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
)

// FailoverPolicy tunes the wrapper. Zero values take the defaults.
type FailoverPolicy struct {
	// FailureThreshold is the number of consecutive active-client failures
	// before the standby is promoted to active. Default 3.
	FailureThreshold int
	// HealthCheckTimeout bounds the standby NodeStatus health check performed
	// before serving a failed call from the standby. Default 5s.
	HealthCheckTimeout time.Duration
}

const (
	defaultFailureThreshold   = 3
	defaultHealthCheckTimeout = 5 * time.Second
)

// Failover serves every Client call from the active node, retrying transport
// failures against the health-checked standby, and promotes the standby after
// FailureThreshold consecutive active failures. Compare implements the
// FR-044 two-node disagreement checks.
type Failover struct {
	primary   Client
	secondary Client
	policy    FailoverPolicy
	logger    interface {
		Warn(msg string, args ...any)
	}

	mu          sync.Mutex
	active      int // 0 = primary, 1 = secondary
	consecFails int
}

// NewFailover wraps two clients. The returned *Failover satisfies Client.
func NewFailover(primary, secondary Client, policy FailoverPolicy) *Failover {
	if policy.FailureThreshold <= 0 {
		policy.FailureThreshold = defaultFailureThreshold
	}
	if policy.HealthCheckTimeout <= 0 {
		policy.HealthCheckTimeout = defaultHealthCheckTimeout
	}
	return &Failover{
		primary:   primary,
		secondary: secondary,
		policy:    policy,
		logger:    logging.New("client.failover"),
	}
}

var _ Client = (*Failover)(nil)

func (f *Failover) pair() (active, standby Client) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active == 0 {
		return f.primary, f.secondary
	}
	return f.secondary, f.primary
}

func (f *Failover) recordSuccess() {
	f.mu.Lock()
	f.consecFails = 0
	f.mu.Unlock()
}

func (f *Failover) recordFailure() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consecFails++
	if f.consecFails >= f.policy.FailureThreshold {
		f.active = 1 - f.active
		f.consecFails = 0
		f.logger.Warn("failover: promoted standby to active", "active_index", f.active)
	}
}

// failoverEligible: typed capability/answer errors and caller cancellations
// are definitive — retrying them on the standby would mask real state.
func failoverEligible(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	return !errors.Is(err, ErrUnsupported) &&
		!errors.Is(err, ErrSimulateUnavailable) &&
		!errors.Is(err, ErrNotFound)
}

func (f *Failover) standbyHealthy(ctx context.Context, standby Client) bool {
	hctx, cancel := context.WithTimeout(ctx, f.policy.HealthCheckTimeout)
	defer cancel()
	_, err := standby.NodeStatus(hctx)
	return err == nil
}

func (f *Failover) do(ctx context.Context, op func(Client) error) error {
	active, standby := f.pair()
	err := op(active)
	if err == nil {
		f.recordSuccess()
		return nil
	}
	if !failoverEligible(ctx, err) {
		return err
	}
	if !f.standbyHealthy(ctx, standby) {
		f.recordFailure()
		return err
	}
	f.recordFailure()
	return op(standby)
}

func (f *Failover) Account(ctx context.Context, addr string) (uint64, uint64, error) {
	var num, seq uint64
	err := f.do(ctx, func(c Client) error {
		var e error
		num, seq, e = c.Account(ctx, addr)
		return e
	})
	return num, seq, err
}

func (f *Failover) Balance(ctx context.Context, addr, denom string) (sdkmath.Int, error) {
	out := sdkmath.ZeroInt()
	err := f.do(ctx, func(c Client) error {
		var e error
		out, e = c.Balance(ctx, addr, denom)
		return e
	})
	return out, err
}

func (f *Failover) AllBalances(ctx context.Context, addr string) (sdk.Coins, error) {
	var out sdk.Coins
	err := f.do(ctx, func(c Client) error {
		var e error
		out, e = c.AllBalances(ctx, addr)
		return e
	})
	return out, err
}

func (f *Failover) DenomMetadata(ctx context.Context, denom string) (*bankv1beta1.Metadata, error) {
	var out *bankv1beta1.Metadata
	err := f.do(ctx, func(c Client) error {
		var e error
		out, e = c.DenomMetadata(ctx, denom)
		return e
	})
	return out, err
}

func (f *Failover) Tx(ctx context.Context, hash string) (*TxInfo, error) {
	var out *TxInfo
	err := f.do(ctx, func(c Client) error {
		var e error
		out, e = c.Tx(ctx, hash)
		return e
	})
	return out, err
}

func (f *Failover) BlockByHeight(ctx context.Context, height int64) (*Block, error) {
	var out *Block
	err := f.do(ctx, func(c Client) error {
		var e error
		out, e = c.BlockByHeight(ctx, height)
		return e
	})
	return out, err
}

func (f *Failover) LatestBlock(ctx context.Context) (*Block, error) {
	var out *Block
	err := f.do(ctx, func(c Client) error {
		var e error
		out, e = c.LatestBlock(ctx)
		return e
	})
	return out, err
}

func (f *Failover) BlockResults(ctx context.Context, height int64) (*BlockResults, error) {
	var out *BlockResults
	err := f.do(ctx, func(c Client) error {
		var e error
		out, e = c.BlockResults(ctx, height)
		return e
	})
	return out, err
}

func (f *Failover) NodeStatus(ctx context.Context) (*NodeStatus, error) {
	var out *NodeStatus
	err := f.do(ctx, func(c Client) error {
		var e error
		out, e = c.NodeStatus(ctx)
		return e
	})
	return out, err
}

func (f *Failover) Simulate(ctx context.Context, txBytes []byte) (*SimulateResult, error) {
	var out *SimulateResult
	err := f.do(ctx, func(c Client) error {
		var e error
		out, e = c.Simulate(ctx, txBytes)
		return e
	})
	return out, err
}

// Broadcast is not retried on the standby: a transport error after handing
// bytes to a node leaves the tx possibly in its mempool, and a blind resend
// can only be judged by the caller's sequence machinery.
func (f *Failover) Broadcast(ctx context.Context, txBytes []byte, mode BroadcastMode) (*BroadcastResult, error) {
	active, _ := f.pair()
	res, err := active.Broadcast(ctx, txBytes, mode)
	if err == nil {
		f.recordSuccess()
	} else if failoverEligible(ctx, err) {
		f.recordFailure()
	}
	return res, err
}

func (f *Failover) GlobalFeeParams(ctx context.Context) (*globalfeev1.Params, error) {
	var out *globalfeev1.Params
	err := f.do(ctx, func(c Client) error {
		var e error
		out, e = c.GlobalFeeParams(ctx)
		return e
	})
	return out, err
}

func (f *Failover) TxsByAddress(ctx context.Context, addr string, page *query.PageRequest, opts ...TxsByAddressOptions) (*txqueryv1.GetTxsByAddressResponse, error) {
	var out *txqueryv1.GetTxsByAddressResponse
	err := f.do(ctx, func(c Client) error {
		var e error
		out, e = c.TxsByAddress(ctx, addr, page, opts...)
		return e
	})
	return out, err
}

func (f *Failover) Probe(ctx context.Context) (ProbeResult, error) {
	active, _ := f.pair()
	return active.Probe(ctx)
}

func (f *Failover) Close() error {
	return errors.Join(f.primary.Close(), f.secondary.Close())
}

// --- Compare (FR-044) ---

type CompareKind string

const (
	CompareHeight          CompareKind = "height"
	CompareBlockHash       CompareKind = "block_hash"
	CompareTxResult        CompareKind = "tx_result"
	CompareAccountSequence CompareKind = "account_sequence"
	CompareBalance         CompareKind = "balance"
)

// CompareRequest selects which FR-044 disagreement checks to run. Zero-valued
// selectors are skipped.
type CompareRequest struct {
	Height bool
	// HeightTolerance is the allowed |primary-secondary| latest-height gap
	// before the height item counts as a mismatch. 0 = exact.
	HeightTolerance int64
	// BlockHashAtHeight compares BlockByHeight(h).Hash. 0 = skip.
	BlockHashAtHeight int64
	// TxHash compares Tx(hash) code+height. "" = skip.
	TxHash string
	// SequenceAddress compares Account(addr) sequence. "" = skip.
	SequenceAddress string
	// BalanceAddress + BalanceDenom compare Balance(addr, denom). "" = skip.
	BalanceAddress string
	BalanceDenom   string
}

// CompareItem is one per-check verdict. On a side error the item is a
// mismatch with the error recorded; a Tx both sides agree is absent matches.
type CompareItem struct {
	Kind         CompareKind
	Match        bool
	Primary      string
	Secondary    string
	PrimaryErr   string
	SecondaryErr string
}

type CompareResult struct {
	Items []CompareItem
}

func (r *CompareResult) AllMatch() bool {
	for _, it := range r.Items {
		if !it.Match {
			return false
		}
	}
	return true
}

func (r *CompareResult) Mismatches() []CompareItem {
	var out []CompareItem
	for _, it := range r.Items {
		if !it.Match {
			out = append(out, it)
		}
	}
	return out
}

// Compare runs the selected checks against primary and secondary directly
// (ignoring the active/standby designation).
func (f *Failover) Compare(ctx context.Context, req CompareRequest) (*CompareResult, error) {
	if !req.Height && req.BlockHashAtHeight == 0 && req.TxHash == "" && req.SequenceAddress == "" && req.BalanceAddress == "" {
		return nil, fmt.Errorf("empty compare request: no checks selected")
	}
	res := &CompareResult{}

	if req.Height {
		it := CompareItem{Kind: CompareHeight}
		pb, pe := f.primary.LatestBlock(ctx)
		sb, se := f.secondary.LatestBlock(ctx)
		if pe != nil {
			it.PrimaryErr = pe.Error()
		} else {
			it.Primary = strconv.FormatInt(pb.Height, 10)
		}
		if se != nil {
			it.SecondaryErr = se.Error()
		} else {
			it.Secondary = strconv.FormatInt(sb.Height, 10)
		}
		if pe == nil && se == nil {
			diff := pb.Height - sb.Height
			if diff < 0 {
				diff = -diff
			}
			it.Match = diff <= req.HeightTolerance
		}
		res.Items = append(res.Items, it)
	}

	if h := req.BlockHashAtHeight; h != 0 {
		it := CompareItem{Kind: CompareBlockHash}
		pb, pe := f.primary.BlockByHeight(ctx, h)
		sb, se := f.secondary.BlockByHeight(ctx, h)
		if pe != nil {
			it.PrimaryErr = pe.Error()
		} else {
			it.Primary = fmt.Sprintf("%X", pb.Hash)
		}
		if se != nil {
			it.SecondaryErr = se.Error()
		} else {
			it.Secondary = fmt.Sprintf("%X", sb.Hash)
		}
		if pe == nil && se == nil {
			it.Match = len(pb.Hash) > 0 && bytes.Equal(pb.Hash, sb.Hash)
		}
		res.Items = append(res.Items, it)
	}

	if req.TxHash != "" {
		it := CompareItem{Kind: CompareTxResult}
		pt, pe := f.primary.Tx(ctx, req.TxHash)
		st, se := f.secondary.Tx(ctx, req.TxHash)
		switch {
		case pe == nil && se == nil:
			it.Primary = fmt.Sprintf("code=%d height=%d", pt.Code, pt.Height)
			it.Secondary = fmt.Sprintf("code=%d height=%d", st.Code, st.Height)
			it.Match = pt.Code == st.Code && pt.Height == st.Height
		case errors.Is(pe, ErrNotFound) && errors.Is(se, ErrNotFound):
			it.Primary, it.Secondary = "not_found", "not_found"
			it.Match = true
		default:
			if pe != nil {
				it.PrimaryErr = pe.Error()
			} else {
				it.Primary = fmt.Sprintf("code=%d height=%d", pt.Code, pt.Height)
			}
			if se != nil {
				it.SecondaryErr = se.Error()
			} else {
				it.Secondary = fmt.Sprintf("code=%d height=%d", st.Code, st.Height)
			}
		}
		res.Items = append(res.Items, it)
	}

	if req.SequenceAddress != "" {
		it := CompareItem{Kind: CompareAccountSequence}
		_, pseq, pe := f.primary.Account(ctx, req.SequenceAddress)
		_, sseq, se := f.secondary.Account(ctx, req.SequenceAddress)
		if pe != nil {
			it.PrimaryErr = pe.Error()
		} else {
			it.Primary = strconv.FormatUint(pseq, 10)
		}
		if se != nil {
			it.SecondaryErr = se.Error()
		} else {
			it.Secondary = strconv.FormatUint(sseq, 10)
		}
		if pe == nil && se == nil {
			it.Match = pseq == sseq
		}
		res.Items = append(res.Items, it)
	}

	if req.BalanceAddress != "" {
		it := CompareItem{Kind: CompareBalance}
		pb, pe := f.primary.Balance(ctx, req.BalanceAddress, req.BalanceDenom)
		sb, se := f.secondary.Balance(ctx, req.BalanceAddress, req.BalanceDenom)
		if pe != nil {
			it.PrimaryErr = pe.Error()
		} else {
			it.Primary = pb.String()
		}
		if se != nil {
			it.SecondaryErr = se.Error()
		} else {
			it.Secondary = sb.String()
		}
		if pe == nil && se == nil {
			it.Match = pb.Equal(sb)
		}
		res.Items = append(res.Items, it)
	}

	return res, nil
}
