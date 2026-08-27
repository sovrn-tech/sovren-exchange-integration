package client

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
	"net/url"
	"strings"
	"sync/atomic"

	authv1beta1 "cosmossdk.io/api/cosmos/auth/v1beta1"
	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	sdkmath "cosmossdk.io/math"
	cmtbytes "github.com/cometbft/cometbft/libs/bytes"
	cmtjson "github.com/cometbft/cometbft/libs/json"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cosmos/gogoproto/proto"
	protov2 "google.golang.org/protobuf/proto"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/query"
	txsvc "github.com/cosmos/cosmos-sdk/types/tx"

	globalfeev1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/globalfee/v1"
	txqueryv1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/txquery/v1"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
)

// Tunneled service-query paths (abci_query grpc-router routes).
const (
	pathAuthAccount       = "/cosmos.auth.v1beta1.Query/Account"
	pathBankBalance       = "/cosmos.bank.v1beta1.Query/Balance"
	pathBankAllBalances   = "/cosmos.bank.v1beta1.Query/AllBalances"
	pathBankDenomMetadata = "/cosmos.bank.v1beta1.Query/DenomMetadata"
	pathTxSimulate        = "/cosmos.tx.v1beta1.Service/Simulate"
	pathGlobalFeeParams   = "/sovr.globalfee.v1.Query/Params"
	pathTxsByAddress      = "/sovr.txquery.v1.Query/GetTxsByAddress"
)

// WithHTTPClient sets the HTTP client the CometBFT-RPC transport uses.
// Ignored by NewGRPC.
func WithHTTPClient(hc *nethttp.Client) Option {
	return func(o *options) { o.httpClient = hc }
}

// NewCometRPC connects to a node's CometBFT RPC endpoint (port 26657) — the
// fallback transport (R4/R8). Service queries are tunneled via abci_query with
// proto-marshaled requests; broadcast uses broadcast_tx_sync/async; blocks,
// block results, tx lookup, and status are native JSON-RPC routes. The
// JSON-RPC core is kit-local (unary HTTP POST, CometBFT wire encoding via
// libs/json) so the transport stays free of websocket dependencies.
func NewCometRPC(rpcURL string, opts ...Option) (Client, error) {
	o := applyOptions(opts)
	u, err := url.Parse(rpcURL)
	if err != nil {
		return nil, fmt.Errorf("comet rpc url %q: %w", rpcURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("comet rpc url %q: scheme must be http or https", rpcURL)
	}
	hc := o.httpClient
	if hc == nil {
		hc = &nethttp.Client{}
	}
	return &cometClient{
		url:  strings.TrimSuffix(rpcURL, "/"),
		hc:   hc,
		opts: o,
	}, nil
}

type cometClient struct {
	url   string
	hc    *nethttp.Client
	opts  *options
	probe probeState
	idSeq atomic.Int64
}

type rpcEnvelopeError struct {
	Method  string
	Code    int
	Message string
	Data    string
}

func (e *rpcEnvelopeError) Error() string {
	return fmt.Sprintf("rpc %s: code=%d message=%s data=%s", e.Method, e.Code, e.Message, e.Data)
}

// rpcCall performs one CometBFT JSON-RPC request. Param values are encoded
// with CometBFT's libs/json (int64 as string, []byte as base64, HexBytes as
// hex) to match the node's reflection-based binder; results decode the same
// way — byte-compatible with the rpc/client/http wire format.
func (c *cometClient) rpcCall(ctx context.Context, method string, params map[string]any, out any) error {
	encoded := make(map[string]json.RawMessage, len(params))
	for k, v := range params {
		b, err := cmtjson.Marshal(v)
		if err != nil {
			return fmt.Errorf("rpc %s: marshal param %s: %w", method, k, err)
		}
		encoded[k] = b
	}
	reqBody, err := json.Marshal(struct {
		JSONRPC string                     `json:"jsonrpc"`
		ID      int64                      `json:"id"`
		Method  string                     `json:"method"`
		Params  map[string]json.RawMessage `json:"params"`
	}{"2.0", c.idSeq.Add(1), method, encoded})
	if err != nil {
		return fmt.Errorf("rpc %s: marshal request: %w", method, err)
	}
	httpReq, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodPost, c.url, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("rpc %s: %w", method, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		c.opts.countRPCErr(c.url)
		return fmt.Errorf("rpc %s: %w", method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.opts.countRPCErr(c.url)
		return fmt.Errorf("rpc %s: read response: %w", method, err)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		c.opts.countRPCErr(c.url)
		return fmt.Errorf("rpc %s: decode response (http %d): %w", method, resp.StatusCode, err)
	}
	if envelope.Error != nil {
		c.opts.countRPCErr(c.url)
		return &rpcEnvelopeError{Method: method, Code: envelope.Error.Code, Message: envelope.Error.Message, Data: envelope.Error.Data}
	}
	if out != nil {
		if err := cmtjson.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("rpc %s: decode result: %w", method, err)
		}
	}
	return nil
}

// isRouteMissing recognizes the baseapp grpc-router "no route" answer
// (sdkerrors.ErrUnknownRequest: codespace sdk, code 6) for a tunneled path.
func isRouteMissing(code uint32, codespace, log string) bool {
	if codespace == "sdk" && code == 6 {
		return true
	}
	l := strings.ToLower(log)
	return strings.Contains(l, "no route for query") || strings.Contains(l, "unknown query path")
}

func (c *cometClient) abciQueryRaw(ctx context.Context, path string, data []byte) ([]byte, error) {
	var res coretypes.ResultABCIQuery
	err := c.rpcCall(ctx, "abci_query", map[string]any{
		"path":   path,
		"data":   cmtbytes.HexBytes(data),
		"height": int64(0),
		"prove":  false,
	}, &res)
	if err != nil {
		return nil, fmt.Errorf("abci_query %s: %w", path, err)
	}
	r := res.Response
	if r.Code != 0 {
		c.opts.countRPCErr(c.url)
		if isRouteMissing(r.Code, r.Codespace, r.Log) {
			return nil, fmt.Errorf("abci_query %s: %s: %w", path, r.Log, ErrUnsupported)
		}
		if strings.Contains(strings.ToLower(r.Log), "not found") {
			return nil, fmt.Errorf("abci_query %s: %s: %w", path, r.Log, ErrNotFound)
		}
		return nil, fmt.Errorf("abci_query %s failed: code=%d codespace=%s log=%s", path, r.Code, r.Codespace, r.Log)
	}
	return r.Value, nil
}

func (c *cometClient) abciQuery(ctx context.Context, path string, req, resp proto.Message) error {
	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", path, err)
	}
	value, err := c.abciQueryRaw(ctx, path, data)
	if err != nil {
		return err
	}
	if err := proto.Unmarshal(value, resp); err != nil {
		return fmt.Errorf("unmarshal %s response: %w", path, err)
	}
	return nil
}

func (c *cometClient) abciQueryV2(ctx context.Context, path string, req, resp protov2.Message) error {
	data, err := protov2.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", path, err)
	}
	value, err := c.abciQueryRaw(ctx, path, data)
	if err != nil {
		return err
	}
	if err := protov2.Unmarshal(value, resp); err != nil {
		return fmt.Errorf("unmarshal %s response: %w", path, err)
	}
	return nil
}

func (c *cometClient) Account(ctx context.Context, addr string) (uint64, uint64, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	var resp authv1beta1.QueryAccountResponse
	if err := c.abciQueryV2(ctx, pathAuthAccount, &authv1beta1.QueryAccountRequest{Address: addr}, &resp); err != nil {
		return 0, 0, err
	}
	return accountFromAny(resp.GetAccount())
}

func (c *cometClient) Balance(ctx context.Context, addr, denom string) (sdkmath.Int, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	var resp bankv1beta1.QueryBalanceResponse
	if err := c.abciQueryV2(ctx, pathBankBalance, &bankv1beta1.QueryBalanceRequest{Address: addr, Denom: denom}, &resp); err != nil {
		return sdkmath.ZeroInt(), err
	}
	if resp.GetBalance() == nil {
		return sdkmath.ZeroInt(), nil
	}
	return intFromCoinAmount(resp.GetBalance().GetAmount())
}

func (c *cometClient) AllBalances(ctx context.Context, addr string) (sdk.Coins, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	var resp bankv1beta1.QueryAllBalancesResponse
	if err := c.abciQueryV2(ctx, pathBankAllBalances, &bankv1beta1.QueryAllBalancesRequest{Address: addr}, &resp); err != nil {
		return nil, err
	}
	return coinsFromPulsar(resp.GetBalances())
}

func (c *cometClient) DenomMetadata(ctx context.Context, denom string) (*bankv1beta1.Metadata, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	var resp bankv1beta1.QueryDenomMetadataResponse
	if err := c.abciQueryV2(ctx, pathBankDenomMetadata, &bankv1beta1.QueryDenomMetadataRequest{Denom: denom}, &resp); err != nil {
		return nil, err
	}
	return resp.GetMetadata(), nil
}

func (c *cometClient) Tx(ctx context.Context, hash string) (*TxInfo, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	hashBytes, err := hex.DecodeString(strings.TrimPrefix(strings.ToUpper(hash), "0X"))
	if err != nil {
		return nil, fmt.Errorf("invalid tx hash %q: %w", hash, err)
	}
	var res coretypes.ResultTx
	if err := c.rpcCall(ctx, "tx", map[string]any{"hash": hashBytes, "prove": false}, &res); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil, fmt.Errorf("tx %s: %w", hash, ErrNotFound)
		}
		return nil, err
	}
	return &TxInfo{
		Hash:      res.Hash.String(),
		Height:    res.Height,
		Code:      res.TxResult.Code,
		Codespace: res.TxResult.Codespace,
		RawLog:    res.TxResult.Log,
		GasWanted: res.TxResult.GasWanted,
		GasUsed:   res.TxResult.GasUsed,
		TxBytes:   res.Tx,
		Events:    eventsFromABCI(res.TxResult.Events),
	}, nil
}

func (c *cometClient) block(ctx context.Context, height *int64) (*Block, error) {
	params := map[string]any{}
	if height != nil {
		params["height"] = *height
	}
	var res coretypes.ResultBlock
	if err := c.rpcCall(ctx, "block", params, &res); err != nil {
		return nil, err
	}
	b := &Block{Hash: res.BlockID.Hash}
	if res.Block != nil {
		h := res.Block.Header
		b.ChainID = h.ChainID
		b.Height = h.Height
		b.Time = h.Time
		b.LastBlockHash = h.LastBlockID.Hash
		b.AppHash = h.AppHash
		b.Txs = make([][]byte, len(res.Block.Data.Txs))
		for i, tx := range res.Block.Data.Txs {
			b.Txs[i] = tx
		}
	}
	return b, nil
}

func (c *cometClient) BlockByHeight(ctx context.Context, height int64) (*Block, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	return c.block(ctx, &height)
}

func (c *cometClient) LatestBlock(ctx context.Context) (*Block, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	return c.block(ctx, nil)
}

func (c *cometClient) BlockResults(ctx context.Context, height int64) (*BlockResults, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	var res coretypes.ResultBlockResults
	if err := c.rpcCall(ctx, "block_results", map[string]any{"height": height}, &res); err != nil {
		return nil, err
	}
	out := &BlockResults{
		Height:              res.Height,
		FinalizeBlockEvents: eventsFromABCI(res.FinalizeBlockEvents),
		AppHash:             res.AppHash,
	}
	out.TxResults = make([]TxExecResult, 0, len(res.TxsResults))
	for _, tr := range res.TxsResults {
		if tr == nil {
			out.TxResults = append(out.TxResults, TxExecResult{})
			continue
		}
		out.TxResults = append(out.TxResults, TxExecResult{
			Code:      tr.Code,
			Codespace: tr.Codespace,
			Data:      tr.Data,
			Log:       tr.Log,
			GasWanted: tr.GasWanted,
			GasUsed:   tr.GasUsed,
			Events:    eventsFromABCI(tr.Events),
		})
	}
	return out, nil
}

func (c *cometClient) NodeStatus(ctx context.Context) (*NodeStatus, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	var res coretypes.ResultStatus
	if err := c.rpcCall(ctx, "status", nil, &res); err != nil {
		return nil, err
	}
	return &NodeStatus{
		ChainID:         res.NodeInfo.Network,
		LatestHeight:    res.SyncInfo.LatestBlockHeight,
		LatestBlockTime: res.SyncInfo.LatestBlockTime,
		LatestBlockHash: res.SyncInfo.LatestBlockHash,
		AppHash:         res.SyncInfo.LatestAppHash,
		CatchingUp:      res.SyncInfo.CatchingUp,
		EarliestHeight:  res.SyncInfo.EarliestBlockHeight,
	}, nil
}

func (c *cometClient) Simulate(ctx context.Context, txBytes []byte) (*SimulateResult, error) {
	if c.probe.simulateBlocked() {
		return nil, ErrSimulateUnavailable
	}
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	var resp txsvc.SimulateResponse
	if err := c.abciQuery(ctx, pathTxSimulate, &txsvc.SimulateRequest{TxBytes: txBytes}, &resp); err != nil {
		if errors.Is(err, ErrUnsupported) {
			c.probe.set(false)
			return nil, fmt.Errorf("%v: %w", err, ErrSimulateUnavailable)
		}
		return nil, err
	}
	res := &SimulateResult{}
	if resp.GasInfo != nil {
		res.GasWanted = resp.GasInfo.GasWanted
		res.GasUsed = resp.GasInfo.GasUsed
	}
	return res, nil
}

func (c *cometClient) Broadcast(ctx context.Context, txBytes []byte, mode BroadcastMode) (*BroadcastResult, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	var method string
	switch mode {
	case BroadcastSync, "":
		method = "broadcast_tx_sync"
	case BroadcastAsync:
		method = "broadcast_tx_async"
	default:
		return nil, fmt.Errorf("unknown broadcast mode %q", mode)
	}
	var res coretypes.ResultBroadcastTx
	if err := c.rpcCall(ctx, method, map[string]any{"tx": txBytes}, &res); err != nil {
		return nil, err
	}
	return &BroadcastResult{
		TxHash:    res.Hash.String(),
		Code:      res.Code,
		Codespace: res.Codespace,
		RawLog:    res.Log,
		Accepted:  res.Code == 0,
	}, nil
}

func (c *cometClient) GlobalFeeParams(ctx context.Context) (*globalfeev1.Params, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	var resp globalfeev1.QueryParamsResponse
	if err := c.abciQuery(ctx, pathGlobalFeeParams, &globalfeev1.QueryParamsRequest{}, &resp); err != nil {
		return nil, err
	}
	return &resp.Params, nil
}

func (c *cometClient) TxsByAddress(ctx context.Context, addr string, page *query.PageRequest, opts ...TxsByAddressOptions) (*txqueryv1.GetTxsByAddressResponse, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	var resp txqueryv1.GetTxsByAddressResponse
	if err := c.abciQuery(ctx, pathTxsByAddress, txsByAddressRequest(addr, page, opts...), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *cometClient) Probe(ctx context.Context) (ProbeResult, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	res := ProbeResult{}
	var status coretypes.ResultStatus
	if err := c.rpcCall(ctx, "status", nil, &status); err != nil {
		c.probe.set(false)
		return res, fmt.Errorf("probe: node unreachable at %s: %w", c.url, err)
	}
	res.NodeReachable = true
	// An empty Simulate request proves routability: a registered tunneled
	// route answers (even with an argument error); a missing one returns the
	// grpc-router "no route" answer.
	var sim txsvc.SimulateResponse
	err := c.abciQuery(ctx, pathTxSimulate, &txsvc.SimulateRequest{}, &sim)
	res.TxServiceRoutable = err == nil || !errors.Is(err, ErrUnsupported)
	c.probe.set(res.TxServiceRoutable)
	c.opts.logger.Info("probe complete",
		logging.FieldNodeEndpoint, c.url,
		"node_reachable", res.NodeReachable,
		"tx_service_routable", res.TxServiceRoutable)
	return res, nil
}

func (c *cometClient) Close() error {
	c.hc.CloseIdleConnections()
	return nil
}
