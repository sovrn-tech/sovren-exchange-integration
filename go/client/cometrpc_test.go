package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"testing"
	"time"

	authv1beta1 "cosmossdk.io/api/cosmos/auth/v1beta1"
	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	basev1beta1 "cosmossdk.io/api/cosmos/base/v1beta1"
	sdkmath "cosmossdk.io/math"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/crypto/ed25519"
	cmtbytes "github.com/cometbft/cometbft/libs/bytes"
	cmtjson "github.com/cometbft/cometbft/libs/json"
	"github.com/cometbft/cometbft/p2p"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	cmttypes "github.com/cometbft/cometbft/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txsvc "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/require"
	protov2 "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	globalfeev1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/globalfee/v1"
	txqueryv1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/txquery/v1"
)

// fakeCometNode is an in-process CometBFT JSON-RPC node: native routes for
// status/block/block_results/tx/broadcast plus a baseapp-style grpc query
// router behind abci_query. Handlers proto-unmarshal tunneled requests, so a
// passing test proves the client's tunneled-path encoding round-trips.
type fakeCometNode struct {
	t                *testing.T
	chainID          string
	height           int64
	txServiceEnabled bool
	txQueryEnabled   bool
	broadcastModes   []string
	lastTxsByAddress *txqueryv1.GetTxsByAddressRequest
}

func (f *fakeCometNode) handler() nethttp.HandlerFunc {
	return func(w nethttp.ResponseWriter, r *nethttp.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(f.t, err)
		var req struct {
			ID     json.RawMessage            `json:"id"`
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		require.NoError(f.t, json.Unmarshal(body, &req))

		result, rpcErr := f.dispatch(req.Method, req.Params)
		w.Header().Set("Content-Type", "application/json")
		if rpcErr != nil {
			resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"Internal error","data":%q}}`, req.ID, rpcErr.Error())
			_, _ = w.Write([]byte(resp))
			return
		}
		encoded, err := cmtjson.Marshal(result)
		require.NoError(f.t, err)
		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, encoded)
		_, _ = w.Write([]byte(resp))
	}
}

func (f *fakeCometNode) param(params map[string]json.RawMessage, key string, out any) {
	raw, ok := params[key]
	require.True(f.t, ok, "missing param %s", key)
	require.NoError(f.t, cmtjson.Unmarshal(raw, out))
}

func (f *fakeCometNode) dispatch(method string, params map[string]json.RawMessage) (any, error) {
	switch method {
	case "status":
		return f.status(), nil
	case "abci_query":
		var path string
		var data cmtbytes.HexBytes
		f.param(params, "path", &path)
		f.param(params, "data", &data)
		return f.abciQuery(path, data), nil
	case "block":
		height := f.height
		if raw, ok := params["height"]; ok {
			var h int64
			require.NoError(f.t, cmtjson.Unmarshal(raw, &h))
			height = h
		}
		return f.block(height), nil
	case "block_results":
		var h int64
		f.param(params, "height", &h)
		return f.blockResults(h), nil
	case "tx":
		var hash []byte
		f.param(params, "hash", &hash)
		if string(hash) != string([]byte{0xAB, 0xCD}) {
			return nil, fmt.Errorf("tx (%X) not found", hash)
		}
		return &coretypes.ResultTx{
			Hash:   cmtbytes.HexBytes(hash),
			Height: 42,
			TxResult: abci.ExecTxResult{
				Code:      0,
				GasWanted: 200000,
				GasUsed:   150000,
				Events: []abci.Event{{
					Type:       "transfer",
					Attributes: []abci.EventAttribute{{Key: "amount", Value: "1usovr", Index: true}},
				}},
			},
			Tx: cmttypes.Tx("raw-tx-bytes"),
		}, nil
	case "broadcast_tx_sync", "broadcast_tx_async":
		f.broadcastModes = append(f.broadcastModes, method)
		var tx cmttypes.Tx
		f.param(params, "tx", &tx)
		res := &coretypes.ResultBroadcastTx{Hash: cmtbytes.HexBytes{0xFE, 0xED}}
		if string(tx) == "reject-me" {
			res.Code = 5
			res.Codespace = "sdk"
			res.Log = "insufficient funds"
		}
		return res, nil
	default:
		return nil, fmt.Errorf("unknown method %s", method)
	}
}

func (f *fakeCometNode) status() *coretypes.ResultStatus {
	return &coretypes.ResultStatus{
		NodeInfo: p2p.DefaultNodeInfo{Network: f.chainID, DefaultNodeID: "aaaa", Other: p2p.DefaultNodeInfoOther{TxIndex: "on"}},
		SyncInfo: coretypes.SyncInfo{
			LatestBlockHash:     cmtbytes.HexBytes{0xAA, byte(f.height)},
			LatestBlockHeight:   f.height,
			LatestBlockTime:     testBlockTime,
			EarliestBlockHeight: 1,
			CatchingUp:          false,
		},
		ValidatorInfo: coretypes.ValidatorInfo{PubKey: ed25519.GenPrivKey().PubKey(), VotingPower: 10},
	}
}

func (f *fakeCometNode) block(height int64) *coretypes.ResultBlock {
	return &coretypes.ResultBlock{
		BlockID: cmttypes.BlockID{Hash: cmtbytes.HexBytes{0xAA, byte(height)}},
		Block: &cmttypes.Block{
			Header: cmttypes.Header{
				ChainID:     f.chainID,
				Height:      height,
				Time:        testBlockTime,
				LastBlockID: cmttypes.BlockID{Hash: cmtbytes.HexBytes{0xAA, byte(height - 1)}},
				AppHash:     cmtbytes.HexBytes{0x01},
			},
			Data: cmttypes.Data{Txs: cmttypes.Txs{cmttypes.Tx("tx-1")}},
		},
	}
}

func (f *fakeCometNode) blockResults(height int64) *coretypes.ResultBlockResults {
	return &coretypes.ResultBlockResults{
		Height: height,
		TxsResults: []*abci.ExecTxResult{
			{Code: 0, GasWanted: 100, GasUsed: 90, Events: []abci.Event{{Type: "transfer"}}},
			{Code: 11, Codespace: "sdk", Log: "out of gas"},
		},
		FinalizeBlockEvents: []abci.Event{{
			Type:       "mint",
			Attributes: []abci.EventAttribute{{Key: "amount", Value: "5usovr"}},
		}},
		AppHash: []byte{0x02},
	}
}

func routeMissing(path string) *abci.ResponseQuery {
	return &abci.ResponseQuery{Code: 6, Codespace: "sdk", Log: fmt.Sprintf("no route for query: %s", path)}
}

func (f *fakeCometNode) abciQuery(path string, data []byte) *coretypes.ResultABCIQuery {
	resp := func(m interface{ Marshal() ([]byte, error) }) *coretypes.ResultABCIQuery {
		value, err := m.Marshal()
		require.NoError(f.t, err)
		return &coretypes.ResultABCIQuery{Response: abci.ResponseQuery{Code: 0, Value: value}}
	}
	respV2 := func(m protov2.Message) *coretypes.ResultABCIQuery {
		value, err := protov2.Marshal(m)
		require.NoError(f.t, err)
		return &coretypes.ResultABCIQuery{Response: abci.ResponseQuery{Code: 0, Value: value}}
	}
	switch path {
	case pathAuthAccount:
		var req authv1beta1.QueryAccountRequest
		require.NoError(f.t, protov2.Unmarshal(data, &req))
		if req.GetAddress() != "sovr1acct" {
			return &coretypes.ResultABCIQuery{Response: abci.ResponseQuery{
				Code: 22, Codespace: "sdk", Log: fmt.Sprintf("account %s not found", req.GetAddress()),
			}}
		}
		acc, err := protov2.Marshal(&authv1beta1.BaseAccount{Address: req.GetAddress(), AccountNumber: 7, Sequence: 42})
		require.NoError(f.t, err)
		return respV2(&authv1beta1.QueryAccountResponse{Account: &anypb.Any{TypeUrl: "/cosmos.auth.v1beta1.BaseAccount", Value: acc}})
	case pathBankBalance:
		var req bankv1beta1.QueryBalanceRequest
		require.NoError(f.t, protov2.Unmarshal(data, &req))
		require.Equal(f.t, "usovr", req.GetDenom())
		return respV2(&bankv1beta1.QueryBalanceResponse{Balance: &basev1beta1.Coin{Denom: req.GetDenom(), Amount: "12345"}})
	case pathBankAllBalances:
		var req bankv1beta1.QueryAllBalancesRequest
		require.NoError(f.t, protov2.Unmarshal(data, &req))
		return respV2(&bankv1beta1.QueryAllBalancesResponse{Balances: []*basev1beta1.Coin{
			{Denom: "other", Amount: "9"},
			{Denom: "usovr", Amount: "12345"},
		}})
	case pathBankDenomMetadata:
		var req bankv1beta1.QueryDenomMetadataRequest
		require.NoError(f.t, protov2.Unmarshal(data, &req))
		return respV2(&bankv1beta1.QueryDenomMetadataResponse{Metadata: &bankv1beta1.Metadata{Base: req.GetDenom(), Symbol: "SOVR"}})
	case pathTxSimulate:
		if !f.txServiceEnabled {
			return &coretypes.ResultABCIQuery{Response: *routeMissing(path)}
		}
		var req txsvc.SimulateRequest
		require.NoError(f.t, proto.Unmarshal(data, &req))
		if len(req.TxBytes) == 0 {
			return &coretypes.ResultABCIQuery{Response: abci.ResponseQuery{Code: 18, Codespace: "sdk", Log: "invalid empty tx bytes"}}
		}
		return resp(&txsvc.SimulateResponse{GasInfo: &sdk.GasInfo{GasWanted: 200000, GasUsed: 123456}})
	case pathGlobalFeeParams:
		var req globalfeev1.QueryParamsRequest
		require.NoError(f.t, proto.Unmarshal(data, &req))
		return resp(&globalfeev1.QueryParamsResponse{Params: globalfeev1.Params{
			MinimumGasPrices: sdk.DecCoins{sdk.NewDecCoinFromDec("usovr", sdkmath.LegacyMustNewDecFromStr("0.001"))},
		}})
	case pathTxsByAddress:
		if !f.txQueryEnabled {
			return &coretypes.ResultABCIQuery{Response: *routeMissing(path)}
		}
		var req txqueryv1.GetTxsByAddressRequest
		require.NoError(f.t, proto.Unmarshal(data, &req))
		require.Equal(f.t, "sovr1acct", req.Address)
		f.lastTxsByAddress = &req
		return resp(&txqueryv1.GetTxsByAddressResponse{Total: 2})
	default:
		return &coretypes.ResultABCIQuery{Response: *routeMissing(path)}
	}
}

func startCometNode(t *testing.T, node *fakeCometNode) (Client, *fakeCometNode) {
	t.Helper()
	node.t = t
	if node.chainID == "" {
		node.chainID = "sovr-test"
	}
	if node.height == 0 {
		node.height = 10
	}
	srv := httptest.NewServer(node.handler())
	t.Cleanup(srv.Close)
	c, err := NewCometRPC(srv.URL, WithTimeout(5*time.Second))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c, node
}

func TestCometRPCURLValidation(t *testing.T) {
	_, err := NewCometRPC("not a url")
	require.Error(t, err)
	_, err = NewCometRPC("ftp://example.com")
	require.Error(t, err)
}

func TestCometAccountTunnel(t *testing.T) {
	c, _ := startCometNode(t, &fakeCometNode{txServiceEnabled: true})
	ctx := context.Background()

	num, seq, err := c.Account(ctx, "sovr1acct")
	require.NoError(t, err)
	require.Equal(t, uint64(7), num)
	require.Equal(t, uint64(42), seq)

	_, _, err = c.Account(ctx, "sovr1missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestCometBankTunnel(t *testing.T) {
	c, _ := startCometNode(t, &fakeCometNode{txServiceEnabled: true})
	ctx := context.Background()

	bal, err := c.Balance(ctx, "sovr1acct", "usovr")
	require.NoError(t, err)
	require.True(t, bal.Equal(sdkmath.NewInt(12345)))

	all, err := c.AllBalances(ctx, "sovr1acct")
	require.NoError(t, err)
	require.Len(t, all, 2)

	md, err := c.DenomMetadata(ctx, "usovr")
	require.NoError(t, err)
	require.Equal(t, "usovr", md.GetBase())
}

func TestCometCustomModuleTunnel(t *testing.T) {
	c, _ := startCometNode(t, &fakeCometNode{txServiceEnabled: true, txQueryEnabled: true})
	ctx := context.Background()

	fee, err := c.GlobalFeeParams(ctx)
	require.NoError(t, err)
	require.Equal(t, "0.001000000000000000usovr", fee.MinimumGasPrices.String())

	txs, err := c.TxsByAddress(ctx, "sovr1acct", nil)
	require.NoError(t, err)
	require.Equal(t, uint64(2), txs.Total)
}

func TestCometTxsByAddressDateOptions(t *testing.T) {
	node := &fakeCometNode{txServiceEnabled: true, txQueryEnabled: true}
	c, _ := startCometNode(t, node)
	txs, err := c.TxsByAddress(context.Background(), "sovr1acct", nil, TxsByAddressOptions{
		StartDate: "2026-07-29",
		EndDate:   "2026-08-03",
	})
	require.NoError(t, err)
	require.Equal(t, uint64(2), txs.Total)
	require.NotNil(t, node.lastTxsByAddress)
	require.Equal(t, "2026-07-29", node.lastTxsByAddress.StartDate)
	require.Equal(t, "2026-08-03", node.lastTxsByAddress.EndDate)
}

func TestCometTxsByAddressUnsupported(t *testing.T) {
	c, _ := startCometNode(t, &fakeCometNode{txServiceEnabled: true, txQueryEnabled: false})
	_, err := c.TxsByAddress(context.Background(), "sovr1acct", nil)
	require.ErrorIs(t, err, ErrUnsupported)
}

func TestCometSimulate(t *testing.T) {
	c, _ := startCometNode(t, &fakeCometNode{txServiceEnabled: true})
	sim, err := c.Simulate(context.Background(), []byte("some-tx"))
	require.NoError(t, err)
	require.Equal(t, uint64(200000), sim.GasWanted)
	require.Equal(t, uint64(123456), sim.GasUsed)
}

func TestCometSimulateUnavailable(t *testing.T) {
	c, _ := startCometNode(t, &fakeCometNode{txServiceEnabled: false})
	ctx := context.Background()

	_, err := c.Simulate(ctx, []byte("some-tx"))
	require.ErrorIs(t, err, ErrSimulateUnavailable)

	// After the failed attempt the probe state short-circuits.
	_, err = c.Simulate(ctx, []byte("some-tx"))
	require.ErrorIs(t, err, ErrSimulateUnavailable)
}

func TestCometProbe(t *testing.T) {
	enabled, _ := startCometNode(t, &fakeCometNode{txServiceEnabled: true})
	probe, err := enabled.Probe(context.Background())
	require.NoError(t, err)
	require.True(t, probe.NodeReachable)
	require.True(t, probe.TxServiceRoutable)

	disabled, _ := startCometNode(t, &fakeCometNode{txServiceEnabled: false})
	probe, err = disabled.Probe(context.Background())
	require.NoError(t, err)
	require.True(t, probe.NodeReachable)
	require.False(t, probe.TxServiceRoutable)

	_, err = disabled.Simulate(context.Background(), []byte("some-tx"))
	require.ErrorIs(t, err, ErrSimulateUnavailable)
}

func TestCometProbeUnreachable(t *testing.T) {
	c, err := NewCometRPC("http://127.0.0.1:1", WithTimeout(500*time.Millisecond))
	require.NoError(t, err)
	probe, perr := c.Probe(context.Background())
	require.Error(t, perr)
	require.False(t, probe.NodeReachable)

	_, err = c.Simulate(context.Background(), []byte("some-tx"))
	require.ErrorIs(t, err, ErrSimulateUnavailable)
}

func TestCometBlocksAndStatus(t *testing.T) {
	c, _ := startCometNode(t, &fakeCometNode{txServiceEnabled: true})
	ctx := context.Background()

	latest, err := c.LatestBlock(ctx)
	require.NoError(t, err)
	require.Equal(t, "sovr-test", latest.ChainID)
	require.Equal(t, int64(10), latest.Height)
	require.Equal(t, []byte{0xAA, 10}, latest.Hash)
	require.Equal(t, []byte{0xAA, 9}, latest.LastBlockHash)
	require.True(t, latest.Time.Equal(testBlockTime))
	require.Equal(t, [][]byte{[]byte("tx-1")}, latest.Txs)

	blk, err := c.BlockByHeight(ctx, 5)
	require.NoError(t, err)
	require.Equal(t, int64(5), blk.Height)
	require.Equal(t, []byte{0xAA, 5}, blk.Hash)

	st, err := c.NodeStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, "sovr-test", st.ChainID)
	require.Equal(t, int64(10), st.LatestHeight)
	require.Equal(t, int64(1), st.EarliestHeight)
	require.False(t, st.CatchingUp)
}

func TestCometBlockResults(t *testing.T) {
	c, _ := startCometNode(t, &fakeCometNode{txServiceEnabled: true})
	res, err := c.BlockResults(context.Background(), 9)
	require.NoError(t, err)
	require.Equal(t, int64(9), res.Height)
	require.Len(t, res.TxResults, 2)
	require.Equal(t, uint32(0), res.TxResults[0].Code)
	require.Equal(t, "transfer", res.TxResults[0].Events[0].Type)
	require.Equal(t, uint32(11), res.TxResults[1].Code)
	require.Equal(t, "out of gas", res.TxResults[1].Log)
	require.Len(t, res.FinalizeBlockEvents, 1)
	require.Equal(t, "mint", res.FinalizeBlockEvents[0].Type)
	require.Equal(t, []byte{0x02}, res.AppHash)
}

func TestCometTx(t *testing.T) {
	c, _ := startCometNode(t, &fakeCometNode{txServiceEnabled: true})
	ctx := context.Background()

	info, err := c.Tx(ctx, "abcd")
	require.NoError(t, err)
	require.Equal(t, "ABCD", info.Hash)
	require.Equal(t, int64(42), info.Height)
	require.Equal(t, []byte("raw-tx-bytes"), info.TxBytes)
	require.Len(t, info.Events, 1)

	_, err = c.Tx(ctx, "0000")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = c.Tx(ctx, "zz")
	require.Error(t, err)
}

func TestCometBroadcast(t *testing.T) {
	c, node := startCometNode(t, &fakeCometNode{txServiceEnabled: true})
	ctx := context.Background()

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

	require.Equal(t, []string{"broadcast_tx_sync", "broadcast_tx_sync", "broadcast_tx_async"}, node.broadcastModes)

	_, err = c.Broadcast(ctx, []byte("good-tx"), "commit")
	require.Error(t, err)
}
