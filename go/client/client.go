// Package client is the kit's chain-access layer: one Client interface served
// by two transports — gRPC against an exchange-run node (NewGRPC) and a
// CometBFT-RPC fallback that tunnels service queries through abci_query
// (NewCometRPC; R4/R8) — plus a health-checked failover wrapper (NewFailover)
// and the network-manifest loader (LoadManifest).
package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	nethttp "net/http"
	"strings"
	"sync"
	"time"

	authv1beta1 "cosmossdk.io/api/cosmos/auth/v1beta1"
	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	basev1beta1 "cosmossdk.io/api/cosmos/base/v1beta1"
	vestingv1beta1 "cosmossdk.io/api/cosmos/vesting/v1beta1"
	sdkmath "cosmossdk.io/math"
	abci "github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/query"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"

	globalfeev1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/globalfee/v1"
	txqueryv1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/txquery/v1"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
)

var (
	// ErrUnsupported is returned when the target node or transport cannot
	// serve the operation (e.g. TxsByAddress on a node without sovr.txquery,
	// BlockResults over pure gRPC).
	ErrUnsupported = errors.New("operation not supported by this node or transport")

	// ErrSimulateUnavailable is returned by Simulate when the tunneled
	// cosmos.tx.v1beta1.Service routes are not registered on the node (node
	// runs without grpc.enable/api.enable — R4) or when a prior Probe
	// determined the tx service is not routable.
	ErrSimulateUnavailable = errors.New("simulate unavailable: tx service not routable on this node")

	// ErrNotFound is returned for lookups whose subject does not exist
	// (unknown account, unindexed tx hash).
	ErrNotFound = errors.New("not found")
)

// BroadcastMode selects the CheckTx wait behaviour. Empty means BroadcastSync.
type BroadcastMode string

const (
	BroadcastSync  BroadcastMode = "sync"
	BroadcastAsync BroadcastMode = "async"
)

// Block is the transport-neutral block header view the kit consumes.
type Block struct {
	ChainID       string
	Height        int64
	Hash          []byte
	Time          time.Time
	LastBlockHash []byte
	AppHash       []byte
	Txs           [][]byte
}

type EventAttribute struct {
	Key   string
	Value string
	Index bool
}

type Event struct {
	Type       string
	Attributes []EventAttribute
}

// TxExecResult is one tx's execution result inside BlockResults.
type TxExecResult struct {
	Code      uint32
	Codespace string
	Data      []byte
	Log       string
	GasWanted int64
	GasUsed   int64
	Events    []Event
}

type BlockResults struct {
	Height              int64
	TxResults           []TxExecResult
	FinalizeBlockEvents []Event
	AppHash             []byte
}

type TxInfo struct {
	Hash      string
	Height    int64
	Code      uint32
	Codespace string
	RawLog    string
	GasWanted int64
	GasUsed   int64
	TxBytes   []byte
	Events    []Event
}

// NodeStatus is the transport-neutral node health view. EarliestHeight is 0
// when the transport cannot report it (gRPC).
type NodeStatus struct {
	ChainID         string
	LatestHeight    int64
	LatestBlockTime time.Time
	LatestBlockHash []byte
	AppHash         []byte
	CatchingUp      bool
	EarliestHeight  int64
}

type SimulateResult struct {
	GasWanted uint64
	GasUsed   uint64
}

// BroadcastResult distinguishes CheckTx-rejected from accepted: Accepted is
// true iff CheckTx passed (Code == 0). A CheckTx rejection is a result, not a
// transport error.
type BroadcastResult struct {
	TxHash    string
	Code      uint32
	Codespace string
	RawLog    string
	Accepted  bool
}

// ProbeResult distinguishes "node reachable" from "tx service routable" (R4:
// the tunneled cosmos.tx.v1beta1.Service routes require grpc.enable or
// api.enable on the node).
type ProbeResult struct {
	NodeReachable     bool
	TxServiceRoutable bool
}

// Client is the single interface both transports satisfy (contract:
// go-client-api.md §client).
type Client interface {
	Account(ctx context.Context, addr string) (accountNumber, sequence uint64, err error)
	Balance(ctx context.Context, addr, denom string) (sdkmath.Int, error)
	AllBalances(ctx context.Context, addr string) (sdk.Coins, error)
	DenomMetadata(ctx context.Context, denom string) (*bankv1beta1.Metadata, error)
	Tx(ctx context.Context, hash string) (*TxInfo, error)
	BlockByHeight(ctx context.Context, height int64) (*Block, error)
	LatestBlock(ctx context.Context) (*Block, error)
	BlockResults(ctx context.Context, height int64) (*BlockResults, error)
	NodeStatus(ctx context.Context) (*NodeStatus, error)
	Simulate(ctx context.Context, txBytes []byte) (*SimulateResult, error)
	Broadcast(ctx context.Context, txBytes []byte, mode BroadcastMode) (*BroadcastResult, error)
	GlobalFeeParams(ctx context.Context) (*globalfeev1.Params, error)
	// TxsByAddress returns merged sender-OR-recipient history for addr.
	// Optional TxsByAddressOptions may set order_by and UTC start_date/end_date
	// (YYYY-MM-DD) matching sovr.txquery.v1.GetTxsByAddressRequest.
	TxsByAddress(ctx context.Context, addr string, page *query.PageRequest, opts ...TxsByAddressOptions) (*txqueryv1.GetTxsByAddressResponse, error)
	Probe(ctx context.Context) (ProbeResult, error)
	Close() error
}

// TxsByAddressOptions are optional filters for Client.TxsByAddress.
// Zero value keeps server defaults (ASC order, no date window).
type TxsByAddressOptions struct {
	// OrderBy is passed through to the chain query. ORDER_BY_UNSPECIFIED
	// (zero) leaves the server default (ASC).
	OrderBy txtypes.OrderBy
	// StartDate is an inclusive lower bound as YYYY-MM-DD (UTC). Empty = none.
	StartDate string
	// EndDate is an inclusive upper bound as YYYY-MM-DD (UTC). Empty = none.
	EndDate string
}

// txsByAddressRequest builds the wire request from the high-level API args.
func txsByAddressRequest(addr string, page *query.PageRequest, opts ...TxsByAddressOptions) *txqueryv1.GetTxsByAddressRequest {
	req := &txqueryv1.GetTxsByAddressRequest{Address: addr, Pagination: page}
	if len(opts) == 0 {
		return req
	}
	o := opts[0]
	req.OrderBy = o.OrderBy
	req.StartDate = o.StartDate
	req.EndDate = o.EndDate
	return req
}

type options struct {
	timeout    time.Duration
	logger     *slog.Logger
	metrics    *metrics.Set
	chainID    string
	dialOpts   []grpc.DialOption
	httpClient *nethttp.Client
}

type Option func(*options)

// WithTimeout bounds every individual call. Zero disables the bound.
func WithTimeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

func WithMetrics(s *metrics.Set) Option { return func(o *options) { o.metrics = s } }

// WithChainID sets the chain_id label on emitted metrics.
func WithChainID(chainID string) Option { return func(o *options) { o.chainID = chainID } }

// WithGRPCDialOptions appends dial options for NewGRPC (TLS credentials,
// test dialers). Ignored by NewCometRPC.
func WithGRPCDialOptions(dialOpts ...grpc.DialOption) Option {
	return func(o *options) { o.dialOpts = append(o.dialOpts, dialOpts...) }
}

func applyOptions(opts []Option) *options {
	o := &options{logger: logging.New("client")}
	for _, fn := range opts {
		fn(o)
	}
	return o
}

func (o *options) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if o.timeout > 0 {
		return context.WithTimeout(ctx, o.timeout)
	}
	return ctx, func() {}
}

func (o *options) countGRPCErr(endpoint string) {
	if o.metrics != nil {
		o.metrics.GRPCErrors.WithLabelValues(o.chainID, endpoint).Inc()
	}
}

func (o *options) countRPCErr(endpoint string) {
	if o.metrics != nil {
		o.metrics.RPCErrors.WithLabelValues(o.chainID, endpoint).Inc()
	}
}

// probeState caches the last Probe outcome; a probe-failed client
// short-circuits Simulate with ErrSimulateUnavailable.
type probeState struct {
	mu       sync.Mutex
	probed   bool
	routable bool
}

func (p *probeState) set(routable bool) {
	p.mu.Lock()
	p.probed = true
	p.routable = routable
	p.mu.Unlock()
}

func (p *probeState) simulateBlocked() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probed && !p.routable
}

// accountFromAny unpacks any chain account flavour (base, module, vesting)
// from a Query/Account Any by message name; the chain's "/…"-prefixed type
// URLs and anypb's "type.googleapis.com/…" both resolve to the same name.
func accountFromAny(a *anypb.Any) (accountNumber, sequence uint64, err error) {
	if a == nil {
		return 0, 0, ErrNotFound
	}
	name := a.GetTypeUrl()
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	fromBase := func(b *authv1beta1.BaseAccount) (uint64, uint64, error) {
		if b == nil {
			return 0, 0, fmt.Errorf("account %s: missing embedded base account", a.GetTypeUrl())
		}
		return b.GetAccountNumber(), b.GetSequence(), nil
	}
	fromVesting := func(v *vestingv1beta1.BaseVestingAccount) (uint64, uint64, error) {
		if v == nil {
			return 0, 0, fmt.Errorf("account %s: missing embedded base vesting account", a.GetTypeUrl())
		}
		return fromBase(v.GetBaseAccount())
	}
	switch name {
	case "cosmos.auth.v1beta1.BaseAccount":
		var m authv1beta1.BaseAccount
		if err := a.UnmarshalTo(&m); err != nil {
			return 0, 0, err
		}
		return fromBase(&m)
	case "cosmos.auth.v1beta1.ModuleAccount":
		var m authv1beta1.ModuleAccount
		if err := a.UnmarshalTo(&m); err != nil {
			return 0, 0, err
		}
		return fromBase(m.GetBaseAccount())
	case "cosmos.vesting.v1beta1.BaseVestingAccount":
		var m vestingv1beta1.BaseVestingAccount
		if err := a.UnmarshalTo(&m); err != nil {
			return 0, 0, err
		}
		return fromVesting(&m)
	case "cosmos.vesting.v1beta1.ContinuousVestingAccount":
		var m vestingv1beta1.ContinuousVestingAccount
		if err := a.UnmarshalTo(&m); err != nil {
			return 0, 0, err
		}
		return fromVesting(m.GetBaseVestingAccount())
	case "cosmos.vesting.v1beta1.DelayedVestingAccount":
		var m vestingv1beta1.DelayedVestingAccount
		if err := a.UnmarshalTo(&m); err != nil {
			return 0, 0, err
		}
		return fromVesting(m.GetBaseVestingAccount())
	case "cosmos.vesting.v1beta1.PeriodicVestingAccount":
		var m vestingv1beta1.PeriodicVestingAccount
		if err := a.UnmarshalTo(&m); err != nil {
			return 0, 0, err
		}
		return fromVesting(m.GetBaseVestingAccount())
	case "cosmos.vesting.v1beta1.PermanentLockedAccount":
		var m vestingv1beta1.PermanentLockedAccount
		if err := a.UnmarshalTo(&m); err != nil {
			return 0, 0, err
		}
		return fromVesting(m.GetBaseVestingAccount())
	default:
		return 0, 0, fmt.Errorf("unknown account type %s: %w", a.GetTypeUrl(), ErrUnsupported)
	}
}

// intFromCoinAmount parses a proto coin amount string (integer-only money
// path: sdkmath.Int, never float).
func intFromCoinAmount(amount string) (sdkmath.Int, error) {
	if amount == "" {
		return sdkmath.ZeroInt(), nil
	}
	v, ok := sdkmath.NewIntFromString(amount)
	if !ok {
		return sdkmath.ZeroInt(), fmt.Errorf("invalid coin amount %q", amount)
	}
	return v, nil
}

func coinsFromPulsar(in []*basev1beta1.Coin) (sdk.Coins, error) {
	out := make(sdk.Coins, 0, len(in))
	for _, c := range in {
		amt, err := intFromCoinAmount(c.GetAmount())
		if err != nil {
			return nil, err
		}
		out = append(out, sdk.Coin{Denom: c.GetDenom(), Amount: amt})
	}
	return out, nil
}

func eventsFromABCI(evs []abci.Event) []Event {
	if len(evs) == 0 {
		return nil
	}
	out := make([]Event, 0, len(evs))
	for _, ev := range evs {
		attrs := make([]EventAttribute, 0, len(ev.Attributes))
		for _, a := range ev.Attributes {
			attrs = append(attrs, EventAttribute{Key: a.Key, Value: a.Value, Index: a.Index})
		}
		out = append(out, Event{Type: ev.Type, Attributes: attrs})
	}
	return out
}
