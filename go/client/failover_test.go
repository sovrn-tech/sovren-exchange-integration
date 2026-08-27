package client

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"

	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/query"
	"github.com/stretchr/testify/require"

	globalfeev1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/globalfee/v1"
	txqueryv1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/txquery/v1"
)

type fakeTx struct {
	code   uint32
	height int64
}

// scriptedClient is a fully in-memory Client for failover/Compare tests.
type scriptedClient struct {
	mu sync.Mutex

	name          string
	failTransport bool  // every op returns a transport error
	typedErr      error // if set, ops return it verbatim (no failover expected)
	statusErr     error // NodeStatus-only failure (health check)

	height     int64
	blockHash  map[int64][]byte
	sequence   uint64
	balance    sdkmath.Int
	txs        map[string]fakeTx
	callCounts map[string]int
}

func newScriptedClient(name string) *scriptedClient {
	return &scriptedClient{
		name:       name,
		height:     100,
		blockHash:  map[int64][]byte{},
		balance:    sdkmath.NewInt(1000),
		txs:        map[string]fakeTx{},
		callCounts: map[string]int{},
	}
}

func (s *scriptedClient) record(op string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callCounts[op]++
	if s.typedErr != nil {
		return s.typedErr
	}
	if s.failTransport {
		return fmt.Errorf("%s: connection refused", s.name)
	}
	return nil
}

func (s *scriptedClient) calls(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callCounts[op]
}

func (s *scriptedClient) Account(ctx context.Context, addr string) (uint64, uint64, error) {
	if err := s.record("Account"); err != nil {
		return 0, 0, err
	}
	return 1, s.sequence, nil
}

func (s *scriptedClient) Balance(ctx context.Context, addr, denom string) (sdkmath.Int, error) {
	if err := s.record("Balance"); err != nil {
		return sdkmath.ZeroInt(), err
	}
	return s.balance, nil
}

func (s *scriptedClient) AllBalances(ctx context.Context, addr string) (sdk.Coins, error) {
	if err := s.record("AllBalances"); err != nil {
		return nil, err
	}
	return sdk.Coins{sdk.Coin{Denom: "usovr", Amount: s.balance}}, nil
}

func (s *scriptedClient) DenomMetadata(ctx context.Context, denom string) (*bankv1beta1.Metadata, error) {
	if err := s.record("DenomMetadata"); err != nil {
		return nil, err
	}
	return &bankv1beta1.Metadata{Base: denom}, nil
}

func (s *scriptedClient) Tx(ctx context.Context, hash string) (*TxInfo, error) {
	if err := s.record("Tx"); err != nil {
		return nil, err
	}
	tx, ok := s.txs[hash]
	if !ok {
		return nil, fmt.Errorf("tx %s: %w", hash, ErrNotFound)
	}
	return &TxInfo{Hash: hash, Code: tx.code, Height: tx.height}, nil
}

func (s *scriptedClient) BlockByHeight(ctx context.Context, height int64) (*Block, error) {
	if err := s.record("BlockByHeight"); err != nil {
		return nil, err
	}
	h, ok := s.blockHash[height]
	if !ok {
		h = []byte{0xAA, byte(height)}
	}
	return &Block{Height: height, Hash: h, ChainID: "sovr-test"}, nil
}

func (s *scriptedClient) LatestBlock(ctx context.Context) (*Block, error) {
	if err := s.record("LatestBlock"); err != nil {
		return nil, err
	}
	return &Block{Height: s.height, Hash: []byte{0xAA, byte(s.height)}, ChainID: "sovr-test"}, nil
}

func (s *scriptedClient) BlockResults(ctx context.Context, height int64) (*BlockResults, error) {
	if err := s.record("BlockResults"); err != nil {
		return nil, err
	}
	return &BlockResults{Height: height}, nil
}

func (s *scriptedClient) NodeStatus(ctx context.Context) (*NodeStatus, error) {
	s.mu.Lock()
	s.callCounts["NodeStatus"]++
	statusErr := s.statusErr
	failTransport := s.failTransport
	height := s.height
	s.mu.Unlock()
	if statusErr != nil {
		return nil, statusErr
	}
	if failTransport {
		return nil, fmt.Errorf("%s: connection refused", s.name)
	}
	return &NodeStatus{ChainID: "sovr-test", LatestHeight: height}, nil
}

func (s *scriptedClient) Simulate(ctx context.Context, txBytes []byte) (*SimulateResult, error) {
	if err := s.record("Simulate"); err != nil {
		return nil, err
	}
	return &SimulateResult{GasWanted: 1, GasUsed: 1}, nil
}

func (s *scriptedClient) Broadcast(ctx context.Context, txBytes []byte, mode BroadcastMode) (*BroadcastResult, error) {
	if err := s.record("Broadcast"); err != nil {
		return nil, err
	}
	return &BroadcastResult{TxHash: "FEED", Accepted: true}, nil
}

func (s *scriptedClient) GlobalFeeParams(ctx context.Context) (*globalfeev1.Params, error) {
	if err := s.record("GlobalFeeParams"); err != nil {
		return nil, err
	}
	return &globalfeev1.Params{}, nil
}

func (s *scriptedClient) TxsByAddress(ctx context.Context, addr string, page *query.PageRequest, opts ...TxsByAddressOptions) (*txqueryv1.GetTxsByAddressResponse, error) {
	if err := s.record("TxsByAddress"); err != nil {
		return nil, err
	}
	return &txqueryv1.GetTxsByAddressResponse{Total: 1}, nil
}

func (s *scriptedClient) Probe(ctx context.Context) (ProbeResult, error) {
	if err := s.record("Probe"); err != nil {
		return ProbeResult{}, err
	}
	return ProbeResult{NodeReachable: true, TxServiceRoutable: true}, nil
}

func (s *scriptedClient) Close() error { return nil }

var _ Client = (*scriptedClient)(nil)

func TestFailoverServesFromStandbyAndPromotes(t *testing.T) {
	primary := newScriptedClient("primary")
	primary.failTransport = true
	primary.balance = sdkmath.NewInt(1)
	secondary := newScriptedClient("secondary")
	secondary.balance = sdkmath.NewInt(2)

	f := NewFailover(primary, secondary, FailoverPolicy{FailureThreshold: 1})
	ctx := context.Background()

	// Primary fails; standby is health-checked, serves, and is promoted.
	bal, err := f.Balance(ctx, "sovr1acct", "usovr")
	require.NoError(t, err)
	require.True(t, bal.Equal(sdkmath.NewInt(2)))
	require.Equal(t, 1, primary.calls("Balance"))
	require.Equal(t, 1, secondary.calls("Balance"))
	require.Equal(t, 1, secondary.calls("NodeStatus"))

	// Promoted: next call goes to the secondary first, primary untouched.
	bal, err = f.Balance(ctx, "sovr1acct", "usovr")
	require.NoError(t, err)
	require.True(t, bal.Equal(sdkmath.NewInt(2)))
	require.Equal(t, 1, primary.calls("Balance"))
	require.Equal(t, 2, secondary.calls("Balance"))
}

func TestFailoverThresholdDelaysPromotion(t *testing.T) {
	primary := newScriptedClient("primary")
	primary.failTransport = true
	secondary := newScriptedClient("secondary")

	f := NewFailover(primary, secondary, FailoverPolicy{FailureThreshold: 2})
	ctx := context.Background()

	// First failure: served from standby, active stays primary.
	_, err := f.LatestBlock(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, primary.calls("LatestBlock"))

	// Second failure crosses the threshold: promote.
	_, err = f.LatestBlock(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, primary.calls("LatestBlock"))

	// Third call served by the promoted secondary only.
	_, err = f.LatestBlock(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, primary.calls("LatestBlock"))
	require.Equal(t, 3, secondary.calls("LatestBlock"))
}

func TestFailoverStandbyUnhealthyReturnsOriginalError(t *testing.T) {
	primary := newScriptedClient("primary")
	primary.failTransport = true
	secondary := newScriptedClient("secondary")
	secondary.statusErr = errors.New("secondary down")

	f := NewFailover(primary, secondary, FailoverPolicy{FailureThreshold: 1})
	_, err := f.LatestBlock(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "primary")
	require.Equal(t, 0, secondary.calls("LatestBlock"))
}

func TestFailoverTypedErrorsNotFailedOver(t *testing.T) {
	primary := newScriptedClient("primary")
	primary.typedErr = fmt.Errorf("no txquery module: %w", ErrUnsupported)
	secondary := newScriptedClient("secondary")

	f := NewFailover(primary, secondary, FailoverPolicy{FailureThreshold: 1})
	_, err := f.TxsByAddress(context.Background(), "sovr1acct", nil)
	require.ErrorIs(t, err, ErrUnsupported)
	require.Equal(t, 0, secondary.calls("TxsByAddress"))
	require.Equal(t, 0, secondary.calls("NodeStatus"))

	primary.typedErr = fmt.Errorf("probe failed: %w", ErrSimulateUnavailable)
	_, err = f.Simulate(context.Background(), []byte("tx"))
	require.ErrorIs(t, err, ErrSimulateUnavailable)
	require.Equal(t, 0, secondary.calls("Simulate"))
}

func TestFailoverBroadcastNeverRetries(t *testing.T) {
	primary := newScriptedClient("primary")
	primary.failTransport = true
	secondary := newScriptedClient("secondary")

	f := NewFailover(primary, secondary, FailoverPolicy{FailureThreshold: 3})
	_, err := f.Broadcast(context.Background(), []byte("tx"), BroadcastSync)
	require.Error(t, err)
	require.Equal(t, 0, secondary.calls("Broadcast"))
}

func TestCompareAllMatch(t *testing.T) {
	primary := newScriptedClient("primary")
	secondary := newScriptedClient("secondary")
	for _, c := range []*scriptedClient{primary, secondary} {
		c.height = 50
		c.sequence = 9
		c.balance = sdkmath.NewInt(777)
		c.blockHash[40] = []byte{0xBB, 0x01}
		c.txs["CAFE"] = fakeTx{code: 0, height: 33}
	}

	f := NewFailover(primary, secondary, FailoverPolicy{})
	res, err := f.Compare(context.Background(), CompareRequest{
		Height:            true,
		BlockHashAtHeight: 40,
		TxHash:            "CAFE",
		SequenceAddress:   "sovr1acct",
		BalanceAddress:    "sovr1acct",
		BalanceDenom:      "usovr",
	})
	require.NoError(t, err)
	require.Len(t, res.Items, 5)
	require.True(t, res.AllMatch(), "items: %+v", res.Items)
	require.Empty(t, res.Mismatches())
}

func TestCompareMismatches(t *testing.T) {
	newPair := func() (*scriptedClient, *scriptedClient) {
		p := newScriptedClient("primary")
		s := newScriptedClient("secondary")
		for _, c := range []*scriptedClient{p, s} {
			c.height = 50
			c.sequence = 9
			c.balance = sdkmath.NewInt(777)
			c.blockHash[40] = []byte{0xBB, 0x01}
			c.txs["CAFE"] = fakeTx{code: 0, height: 33}
		}
		return p, s
	}
	ctx := context.Background()

	t.Run("height beyond tolerance", func(t *testing.T) {
		p, s := newPair()
		s.height = 53
		f := NewFailover(p, s, FailoverPolicy{})
		res, err := f.Compare(ctx, CompareRequest{Height: true, HeightTolerance: 2})
		require.NoError(t, err)
		require.Len(t, res.Items, 1)
		require.False(t, res.Items[0].Match)
		require.Equal(t, CompareHeight, res.Items[0].Kind)
		require.Equal(t, "50", res.Items[0].Primary)
		require.Equal(t, "53", res.Items[0].Secondary)
	})

	t.Run("height within tolerance", func(t *testing.T) {
		p, s := newPair()
		s.height = 52
		f := NewFailover(p, s, FailoverPolicy{})
		res, err := f.Compare(ctx, CompareRequest{Height: true, HeightTolerance: 2})
		require.NoError(t, err)
		require.True(t, res.AllMatch())
	})

	t.Run("block hash mismatch", func(t *testing.T) {
		p, s := newPair()
		s.blockHash[40] = []byte{0xDD, 0x02}
		f := NewFailover(p, s, FailoverPolicy{})
		res, err := f.Compare(ctx, CompareRequest{BlockHashAtHeight: 40})
		require.NoError(t, err)
		require.Len(t, res.Items, 1)
		require.False(t, res.Items[0].Match)
		require.Equal(t, CompareBlockHash, res.Items[0].Kind)
		require.Equal(t, hex.EncodeToString([]byte{0xBB, 0x01}), lowerHex(res.Items[0].Primary))
	})

	t.Run("tx code mismatch", func(t *testing.T) {
		p, s := newPair()
		s.txs["CAFE"] = fakeTx{code: 5, height: 33}
		f := NewFailover(p, s, FailoverPolicy{})
		res, err := f.Compare(ctx, CompareRequest{TxHash: "CAFE"})
		require.NoError(t, err)
		require.False(t, res.Items[0].Match)
		require.Equal(t, "code=0 height=33", res.Items[0].Primary)
		require.Equal(t, "code=5 height=33", res.Items[0].Secondary)
	})

	t.Run("tx missing on both matches", func(t *testing.T) {
		p, s := newPair()
		f := NewFailover(p, s, FailoverPolicy{})
		res, err := f.Compare(ctx, CompareRequest{TxHash: "0000"})
		require.NoError(t, err)
		require.True(t, res.Items[0].Match)
		require.Equal(t, "not_found", res.Items[0].Primary)
	})

	t.Run("tx missing on one side mismatches", func(t *testing.T) {
		p, s := newPair()
		delete(s.txs, "CAFE")
		f := NewFailover(p, s, FailoverPolicy{})
		res, err := f.Compare(ctx, CompareRequest{TxHash: "CAFE"})
		require.NoError(t, err)
		require.False(t, res.Items[0].Match)
		require.Contains(t, res.Items[0].SecondaryErr, "not found")
	})

	t.Run("sequence mismatch", func(t *testing.T) {
		p, s := newPair()
		s.sequence = 10
		f := NewFailover(p, s, FailoverPolicy{})
		res, err := f.Compare(ctx, CompareRequest{SequenceAddress: "sovr1acct"})
		require.NoError(t, err)
		require.False(t, res.Items[0].Match)
		require.Equal(t, CompareAccountSequence, res.Items[0].Kind)
		require.Equal(t, "9", res.Items[0].Primary)
		require.Equal(t, "10", res.Items[0].Secondary)
	})

	t.Run("balance mismatch", func(t *testing.T) {
		p, s := newPair()
		s.balance = sdkmath.NewInt(778)
		f := NewFailover(p, s, FailoverPolicy{})
		res, err := f.Compare(ctx, CompareRequest{BalanceAddress: "sovr1acct", BalanceDenom: "usovr"})
		require.NoError(t, err)
		require.False(t, res.Items[0].Match)
		require.Equal(t, CompareBalance, res.Items[0].Kind)
		require.Equal(t, "777", res.Items[0].Primary)
		require.Equal(t, "778", res.Items[0].Secondary)
	})

	t.Run("side error mismatches with error recorded", func(t *testing.T) {
		p, s := newPair()
		s.failTransport = true
		f := NewFailover(p, s, FailoverPolicy{})
		res, err := f.Compare(ctx, CompareRequest{Height: true})
		require.NoError(t, err)
		require.False(t, res.Items[0].Match)
		require.Contains(t, res.Items[0].SecondaryErr, "connection refused")
		require.Equal(t, "50", res.Items[0].Primary)
	})

	t.Run("empty request errors", func(t *testing.T) {
		p, s := newPair()
		f := NewFailover(p, s, FailoverPolicy{})
		_, err := f.Compare(ctx, CompareRequest{})
		require.Error(t, err)
	})
}

func lowerHex(s string) string {
	b, err := hex.DecodeString(s)
	if err != nil {
		return s
	}
	return hex.EncodeToString(b)
}
