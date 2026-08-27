package main

// In-process stub chain (offline scenarios N3/N4/C5/M1): a deterministic
// synthetic block sequence served through the kit's client.Client interface,
// so the adapter components under test run unmodified with no live chain.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/query"

	globalfeev1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/globalfee/v1"
	txqueryv1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/txquery/v1"
	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
)

// stubChain is a fixed synthetic chain: blocks 1..len(blocks) with a
// deterministic hash chain, optional per-height tx bytes and results.
type stubChain struct {
	chainID string

	mu       sync.Mutex
	txs      map[int64][][]byte
	results  map[int64][]client.TxExecResult
	tip      int64
	balances map[string]sdkmath.Int

	calls map[string]int
	// failing, when true, makes every call return a transport error
	// (failover drills).
	failing bool
}

func newStubChain(chainID string, tip int64) *stubChain {
	return &stubChain{
		chainID:  chainID,
		txs:      map[int64][][]byte{},
		results:  map[int64][]client.TxExecResult{},
		tip:      tip,
		balances: map[string]sdkmath.Int{},
		calls:    map[string]int{},
	}
}

func (s *stubChain) setFailing(v bool) {
	s.mu.Lock()
	s.failing = v
	s.mu.Unlock()
}

func (s *stubChain) callCount(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

func (s *stubChain) addTx(height int64, txBytes []byte, res client.TxExecResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txs[height] = append(s.txs[height], txBytes)
	s.results[height] = append(s.results[height], res)
}

func (s *stubChain) enter(op string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[op]++
	if s.failing {
		return fmt.Errorf("stub chain %s: simulated transport failure (%s)", s.chainID, op)
	}
	return nil
}

func (s *stubChain) hashAt(h int64) []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("cert-block-%s-%d", s.chainID, h)))
	return sum[:]
}

func (s *stubChain) blockAt(h int64) *client.Block {
	var last []byte
	if h > 1 {
		last = s.hashAt(h - 1)
	}
	s.mu.Lock()
	txs := append([][]byte{}, s.txs[h]...)
	s.mu.Unlock()
	return &client.Block{
		ChainID:       s.chainID,
		Height:        h,
		Hash:          s.hashAt(h),
		Time:          time.Unix(1700000000+h*5, 0).UTC(),
		LastBlockHash: last,
		Txs:           txs,
	}
}

var _ client.Client = (*stubChain)(nil)

func (s *stubChain) Account(ctx context.Context, addr string) (uint64, uint64, error) {
	if err := s.enter("Account"); err != nil {
		return 0, 0, err
	}
	return 1, 0, nil
}

func (s *stubChain) Balance(ctx context.Context, addr, denom string) (sdkmath.Int, error) {
	if err := s.enter("Balance"); err != nil {
		return sdkmath.ZeroInt(), err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.balances[addr]; ok {
		return v, nil
	}
	return sdkmath.ZeroInt(), nil
}

func (s *stubChain) AllBalances(ctx context.Context, addr string) (sdk.Coins, error) {
	b, err := s.Balance(ctx, addr, "usovr")
	if err != nil {
		return nil, err
	}
	return sdk.Coins{sdk.Coin{Denom: "usovr", Amount: b}}, nil
}

func (s *stubChain) DenomMetadata(ctx context.Context, denom string) (*bankv1beta1.Metadata, error) {
	if err := s.enter("DenomMetadata"); err != nil {
		return nil, err
	}
	return &bankv1beta1.Metadata{Base: "usovr", Display: "SOVR"}, nil
}

func (s *stubChain) Tx(ctx context.Context, hash string) (*client.TxInfo, error) {
	if err := s.enter("Tx"); err != nil {
		return nil, err
	}
	return nil, client.ErrNotFound
}

func (s *stubChain) BlockByHeight(ctx context.Context, height int64) (*client.Block, error) {
	if err := s.enter("BlockByHeight"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	tip := s.tip
	s.mu.Unlock()
	if height < 1 || height > tip {
		return nil, client.ErrNotFound
	}
	return s.blockAt(height), nil
}

func (s *stubChain) LatestBlock(ctx context.Context) (*client.Block, error) {
	if err := s.enter("LatestBlock"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	tip := s.tip
	s.mu.Unlock()
	return s.blockAt(tip), nil
}

func (s *stubChain) BlockResults(ctx context.Context, height int64) (*client.BlockResults, error) {
	if err := s.enter("BlockResults"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	res := append([]client.TxExecResult{}, s.results[height]...)
	s.mu.Unlock()
	return &client.BlockResults{Height: height, TxResults: res}, nil
}

func (s *stubChain) NodeStatus(ctx context.Context) (*client.NodeStatus, error) {
	if err := s.enter("NodeStatus"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	tip := s.tip
	s.mu.Unlock()
	return &client.NodeStatus{
		ChainID:         s.chainID,
		LatestHeight:    tip,
		LatestBlockTime: time.Now().UTC(),
		LatestBlockHash: s.hashAt(tip),
		EarliestHeight:  1,
	}, nil
}

func (s *stubChain) Simulate(ctx context.Context, txBytes []byte) (*client.SimulateResult, error) {
	if err := s.enter("Simulate"); err != nil {
		return nil, err
	}
	return &client.SimulateResult{GasWanted: 80000, GasUsed: 80000}, nil
}

func (s *stubChain) Broadcast(ctx context.Context, txBytes []byte, mode client.BroadcastMode) (*client.BroadcastResult, error) {
	if err := s.enter("Broadcast"); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(txBytes)
	return &client.BroadcastResult{TxHash: fmt.Sprintf("%X", sum[:]), Accepted: true}, nil
}

func (s *stubChain) GlobalFeeParams(ctx context.Context) (*globalfeev1.Params, error) {
	if err := s.enter("GlobalFeeParams"); err != nil {
		return nil, err
	}
	return &globalfeev1.Params{}, nil
}

func (s *stubChain) TxsByAddress(ctx context.Context, addr string, page *query.PageRequest, opts ...client.TxsByAddressOptions) (*txqueryv1.GetTxsByAddressResponse, error) {
	if err := s.enter("TxsByAddress"); err != nil {
		return nil, err
	}
	return &txqueryv1.GetTxsByAddressResponse{}, nil
}

func (s *stubChain) Probe(ctx context.Context) (client.ProbeResult, error) {
	if err := s.enter("Probe"); err != nil {
		return client.ProbeResult{}, err
	}
	return client.ProbeResult{NodeReachable: true, TxServiceRoutable: true}, nil
}

func (s *stubChain) Close() error { return nil }
