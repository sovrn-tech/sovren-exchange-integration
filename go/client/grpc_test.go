package client

import (
	"context"
	"net"
	"testing"
	"time"

	authv1beta1 "cosmossdk.io/api/cosmos/auth/v1beta1"
	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	tmv1beta1 "cosmossdk.io/api/cosmos/base/tendermint/v1beta1"
	basev1beta1 "cosmossdk.io/api/cosmos/base/v1beta1"
	tmtypespb "cosmossdk.io/api/tendermint/types"
	sdkmath "cosmossdk.io/math"
	abci "github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txsvc "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	protov2 "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	globalfeev1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/globalfee/v1"
	txqueryv1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/txquery/v1"
)

var testBlockTime = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

// chainAny packs a pulsar message the way the chain does: "/<name>" type URL.
func chainAny(t *testing.T, name string, m protov2.Message) *anypb.Any {
	t.Helper()
	val, err := protov2.Marshal(m)
	require.NoError(t, err)
	return &anypb.Any{TypeUrl: "/" + name, Value: val}
}

type fakeAuthServer struct {
	authv1beta1.UnimplementedQueryServer
	accounts map[string]*anypb.Any
}

func (s *fakeAuthServer) Account(ctx context.Context, req *authv1beta1.QueryAccountRequest) (*authv1beta1.QueryAccountResponse, error) {
	a, ok := s.accounts[req.GetAddress()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "account %s not found", req.GetAddress())
	}
	return &authv1beta1.QueryAccountResponse{Account: a}, nil
}

type fakeBankServer struct {
	bankv1beta1.UnimplementedQueryServer
	balances map[string][]*basev1beta1.Coin
}

func (s *fakeBankServer) Balance(ctx context.Context, req *bankv1beta1.QueryBalanceRequest) (*bankv1beta1.QueryBalanceResponse, error) {
	for _, c := range s.balances[req.GetAddress()] {
		if c.GetDenom() == req.GetDenom() {
			return &bankv1beta1.QueryBalanceResponse{Balance: c}, nil
		}
	}
	return &bankv1beta1.QueryBalanceResponse{Balance: &basev1beta1.Coin{Denom: req.GetDenom(), Amount: "0"}}, nil
}

func (s *fakeBankServer) AllBalances(ctx context.Context, req *bankv1beta1.QueryAllBalancesRequest) (*bankv1beta1.QueryAllBalancesResponse, error) {
	return &bankv1beta1.QueryAllBalancesResponse{Balances: s.balances[req.GetAddress()]}, nil
}

func (s *fakeBankServer) DenomMetadata(ctx context.Context, req *bankv1beta1.QueryDenomMetadataRequest) (*bankv1beta1.QueryDenomMetadataResponse, error) {
	return &bankv1beta1.QueryDenomMetadataResponse{Metadata: &bankv1beta1.Metadata{
		Base:    req.GetDenom(),
		Display: "SOVR",
		Symbol:  "SOVR",
	}}, nil
}

type fakeTendermintServer struct {
	tmv1beta1.UnimplementedServiceServer
	chainID string
	height  int64
	syncing bool
}

func (s *fakeTendermintServer) sdkBlock(height int64) (*tmtypespb.BlockID, *tmv1beta1.Block) {
	return &tmtypespb.BlockID{Hash: []byte{0xAA, byte(height)}},
		&tmv1beta1.Block{
			Header: &tmv1beta1.Header{
				ChainId:     s.chainID,
				Height:      height,
				Time:        timestamppb.New(testBlockTime),
				LastBlockId: &tmtypespb.BlockID{Hash: []byte{0xAA, byte(height - 1)}},
				AppHash:     []byte{0x01},
			},
			Data: &tmtypespb.Data{Txs: [][]byte{[]byte("tx-1")}},
		}
}

func (s *fakeTendermintServer) GetLatestBlock(ctx context.Context, req *tmv1beta1.GetLatestBlockRequest) (*tmv1beta1.GetLatestBlockResponse, error) {
	id, b := s.sdkBlock(s.height)
	return &tmv1beta1.GetLatestBlockResponse{BlockId: id, SdkBlock: b}, nil
}

func (s *fakeTendermintServer) GetBlockByHeight(ctx context.Context, req *tmv1beta1.GetBlockByHeightRequest) (*tmv1beta1.GetBlockByHeightResponse, error) {
	id, b := s.sdkBlock(req.GetHeight())
	return &tmv1beta1.GetBlockByHeightResponse{BlockId: id, SdkBlock: b}, nil
}

func (s *fakeTendermintServer) GetSyncing(ctx context.Context, req *tmv1beta1.GetSyncingRequest) (*tmv1beta1.GetSyncingResponse, error) {
	return &tmv1beta1.GetSyncingResponse{Syncing: s.syncing}, nil
}

type fakeTxServer struct {
	txsvc.UnimplementedServiceServer
}

func (s *fakeTxServer) Simulate(ctx context.Context, req *txsvc.SimulateRequest) (*txsvc.SimulateResponse, error) {
	if len(req.TxBytes) == 0 {
		return nil, status.Error(codes.InvalidArgument, "empty txBytes is not allowed")
	}
	return &txsvc.SimulateResponse{GasInfo: &sdk.GasInfo{GasWanted: 200000, GasUsed: 123456}}, nil
}

func (s *fakeTxServer) GetTx(ctx context.Context, req *txsvc.GetTxRequest) (*txsvc.GetTxResponse, error) {
	if req.Hash != "ABCD" {
		return nil, status.Errorf(codes.NotFound, "tx %s not found", req.Hash)
	}
	return &txsvc.GetTxResponse{TxResponse: &sdk.TxResponse{
		TxHash:    "ABCD",
		Height:    42,
		Code:      0,
		GasWanted: 200000,
		GasUsed:   150000,
		RawLog:    "",
		Events: []abci.Event{{
			Type:       "transfer",
			Attributes: []abci.EventAttribute{{Key: "amount", Value: "1usovr", Index: true}},
		}},
	}}, nil
}

func (s *fakeTxServer) BroadcastTx(ctx context.Context, req *txsvc.BroadcastTxRequest) (*txsvc.BroadcastTxResponse, error) {
	if req.Mode != txsvc.BroadcastMode_BROADCAST_MODE_SYNC && req.Mode != txsvc.BroadcastMode_BROADCAST_MODE_ASYNC {
		return nil, status.Errorf(codes.InvalidArgument, "unexpected mode %v", req.Mode)
	}
	resp := &sdk.TxResponse{TxHash: "FEED"}
	if string(req.TxBytes) == "reject-me" {
		resp.Code = 5
		resp.Codespace = "sdk"
		resp.RawLog = "insufficient funds"
	}
	return &txsvc.BroadcastTxResponse{TxResponse: resp}, nil
}

type fakeGlobalFeeServer struct {
	globalfeev1.UnimplementedQueryServer
}

func (s *fakeGlobalFeeServer) Params(ctx context.Context, req *globalfeev1.QueryParamsRequest) (*globalfeev1.QueryParamsResponse, error) {
	return &globalfeev1.QueryParamsResponse{Params: globalfeev1.Params{
		MinimumGasPrices: sdk.DecCoins{sdk.NewDecCoinFromDec("usovr", sdkmath.LegacyMustNewDecFromStr("0.001"))},
	}}, nil
}

type fakeTxQueryServer struct {
	txqueryv1.UnimplementedQueryServer
	last *txqueryv1.GetTxsByAddressRequest
}

func (s *fakeTxQueryServer) GetTxsByAddress(ctx context.Context, req *txqueryv1.GetTxsByAddressRequest) (*txqueryv1.GetTxsByAddressResponse, error) {
	s.last = req
	if req.Address == "" {
		return nil, status.Error(codes.InvalidArgument, "empty address")
	}
	return &txqueryv1.GetTxsByAddressResponse{Total: 2}, nil
}

// startGRPCServer serves the given registrations over bufconn and returns a
// connected kit client.
func startGRPCServer(t *testing.T, register func(*grpc.Server)) Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := NewGRPC("passthrough:///bufnet",
		WithTimeout(5*time.Second),
		WithGRPCDialOptions(grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		})),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func startFullGRPCServer(t *testing.T) Client {
	t.Helper()
	baseAcc := &authv1beta1.BaseAccount{Address: "sovr1acct", AccountNumber: 7, Sequence: 42}
	modAcc := &authv1beta1.ModuleAccount{
		BaseAccount: &authv1beta1.BaseAccount{Address: "sovr1module", AccountNumber: 3, Sequence: 0},
		Name:        "distribution",
	}
	return startGRPCServer(t, func(s *grpc.Server) {
		authv1beta1.RegisterQueryServer(s, &fakeAuthServer{accounts: map[string]*anypb.Any{
			"sovr1acct":   chainAny(t, "cosmos.auth.v1beta1.BaseAccount", baseAcc),
			"sovr1module": chainAny(t, "cosmos.auth.v1beta1.ModuleAccount", modAcc),
		}})
		bankv1beta1.RegisterQueryServer(s, &fakeBankServer{balances: map[string][]*basev1beta1.Coin{
			"sovr1acct": {
				{Denom: "usovr", Amount: "12345"},
				{Denom: "other", Amount: "9"},
			},
		}})
		tmv1beta1.RegisterServiceServer(s, &fakeTendermintServer{chainID: "sovr-test", height: 10})
		txsvc.RegisterServiceServer(s, &fakeTxServer{})
		globalfeev1.RegisterQueryServer(s, &fakeGlobalFeeServer{})
		txqueryv1.RegisterQueryServer(s, &fakeTxQueryServer{})
	})
}

func TestGRPCAccount(t *testing.T) {
	c := startFullGRPCServer(t)
	ctx := context.Background()

	num, seq, err := c.Account(ctx, "sovr1acct")
	require.NoError(t, err)
	require.Equal(t, uint64(7), num)
	require.Equal(t, uint64(42), seq)

	num, seq, err = c.Account(ctx, "sovr1module")
	require.NoError(t, err)
	require.Equal(t, uint64(3), num)
	require.Equal(t, uint64(0), seq)

	_, _, err = c.Account(ctx, "sovr1missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestGRPCBankQueries(t *testing.T) {
	c := startFullGRPCServer(t)
	ctx := context.Background()

	bal, err := c.Balance(ctx, "sovr1acct", "usovr")
	require.NoError(t, err)
	require.True(t, bal.Equal(sdkmath.NewInt(12345)))

	zero, err := c.Balance(ctx, "sovr1empty", "usovr")
	require.NoError(t, err)
	require.True(t, zero.IsZero())

	all, err := c.AllBalances(ctx, "sovr1acct")
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, "usovr", all[0].Denom)
	require.True(t, all[0].Amount.Equal(sdkmath.NewInt(12345)))

	md, err := c.DenomMetadata(ctx, "usovr")
	require.NoError(t, err)
	require.Equal(t, "usovr", md.GetBase())
	require.Equal(t, "SOVR", md.GetSymbol())
}

func TestGRPCBlocksAndStatus(t *testing.T) {
	c := startFullGRPCServer(t)
	ctx := context.Background()

	latest, err := c.LatestBlock(ctx)
	require.NoError(t, err)
	require.Equal(t, "sovr-test", latest.ChainID)
	require.Equal(t, int64(10), latest.Height)
	require.Equal(t, []byte{0xAA, 10}, latest.Hash)
	require.Equal(t, []byte{0xAA, 9}, latest.LastBlockHash)
	require.Equal(t, testBlockTime, latest.Time)
	require.Equal(t, [][]byte{[]byte("tx-1")}, latest.Txs)

	blk, err := c.BlockByHeight(ctx, 5)
	require.NoError(t, err)
	require.Equal(t, int64(5), blk.Height)
	require.Equal(t, []byte{0xAA, 5}, blk.Hash)

	st, err := c.NodeStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, "sovr-test", st.ChainID)
	require.Equal(t, int64(10), st.LatestHeight)
	require.False(t, st.CatchingUp)
}

func TestGRPCTxAndBroadcast(t *testing.T) {
	c := startFullGRPCServer(t)
	ctx := context.Background()

	info, err := c.Tx(ctx, "ABCD")
	require.NoError(t, err)
	require.Equal(t, "ABCD", info.Hash)
	require.Equal(t, int64(42), info.Height)
	require.Equal(t, uint32(0), info.Code)
	require.Len(t, info.Events, 1)
	require.Equal(t, "transfer", info.Events[0].Type)
	require.Equal(t, "amount", info.Events[0].Attributes[0].Key)

	_, err = c.Tx(ctx, "0000")
	require.ErrorIs(t, err, ErrNotFound)

	ok, err := c.Broadcast(ctx, []byte("good-tx"), BroadcastSync)
	require.NoError(t, err)
	require.True(t, ok.Accepted)
	require.Equal(t, "FEED", ok.TxHash)

	rejected, err := c.Broadcast(ctx, []byte("reject-me"), "")
	require.NoError(t, err)
	require.False(t, rejected.Accepted)
	require.Equal(t, uint32(5), rejected.Code)
	require.Equal(t, "insufficient funds", rejected.RawLog)

	async, err := c.Broadcast(ctx, []byte("good-tx"), BroadcastAsync)
	require.NoError(t, err)
	require.True(t, async.Accepted)

	_, err = c.Broadcast(ctx, []byte("good-tx"), "commit")
	require.Error(t, err)
}

func TestGRPCSimulateAndProbe(t *testing.T) {
	c := startFullGRPCServer(t)
	ctx := context.Background()

	probe, err := c.Probe(ctx)
	require.NoError(t, err)
	require.True(t, probe.NodeReachable)
	require.True(t, probe.TxServiceRoutable)

	sim, err := c.Simulate(ctx, []byte("some-tx"))
	require.NoError(t, err)
	require.Equal(t, uint64(200000), sim.GasWanted)
	require.Equal(t, uint64(123456), sim.GasUsed)
}

func TestGRPCCustomModuleQueries(t *testing.T) {
	c := startFullGRPCServer(t)
	ctx := context.Background()

	fee, err := c.GlobalFeeParams(ctx)
	require.NoError(t, err)
	require.Equal(t, "0.001000000000000000usovr", fee.MinimumGasPrices.String())

	txs, err := c.TxsByAddress(ctx, "sovr1acct", nil)
	require.NoError(t, err)
	require.Equal(t, uint64(2), txs.Total)
}

func TestGRPCTxsByAddressDateOptions(t *testing.T) {
	txq := &fakeTxQueryServer{}
	c := startGRPCServer(t, func(s *grpc.Server) {
		txqueryv1.RegisterQueryServer(s, txq)
	})
	_, err := c.TxsByAddress(context.Background(), "sovr1acct", nil, TxsByAddressOptions{
		StartDate: "2026-07-01",
		EndDate:   "2026-08-03",
	})
	require.NoError(t, err)
	require.NotNil(t, txq.last)
	require.Equal(t, "2026-07-01", txq.last.StartDate)
	require.Equal(t, "2026-08-03", txq.last.EndDate)
}

func TestGRPCBlockResultsUnsupported(t *testing.T) {
	c := startFullGRPCServer(t)
	_, err := c.BlockResults(context.Background(), 5)
	require.ErrorIs(t, err, ErrUnsupported)
}

func TestGRPCTxServiceNotRoutable(t *testing.T) {
	// Node with tendermint service only: reachable, but no tx service and no
	// sovr.txquery module.
	c := startGRPCServer(t, func(s *grpc.Server) {
		tmv1beta1.RegisterServiceServer(s, &fakeTendermintServer{chainID: "sovr-test", height: 10})
	})
	ctx := context.Background()

	probe, err := c.Probe(ctx)
	require.NoError(t, err)
	require.True(t, probe.NodeReachable)
	require.False(t, probe.TxServiceRoutable)

	// Probe-failed client short-circuits Simulate with the typed error.
	_, err = c.Simulate(ctx, []byte("some-tx"))
	require.ErrorIs(t, err, ErrSimulateUnavailable)

	_, err = c.TxsByAddress(ctx, "sovr1acct", nil)
	require.ErrorIs(t, err, ErrUnsupported)
}

func TestGRPCSimulateUnimplementedWithoutProbe(t *testing.T) {
	c := startGRPCServer(t, func(s *grpc.Server) {
		tmv1beta1.RegisterServiceServer(s, &fakeTendermintServer{chainID: "sovr-test", height: 10})
	})
	_, err := c.Simulate(context.Background(), []byte("some-tx"))
	require.ErrorIs(t, err, ErrSimulateUnavailable)
}
