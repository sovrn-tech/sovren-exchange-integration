package client

import (
	"context"
	"fmt"

	authv1beta1 "cosmossdk.io/api/cosmos/auth/v1beta1"
	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	tmv1beta1 "cosmossdk.io/api/cosmos/base/tendermint/v1beta1"
	tmtypespb "cosmossdk.io/api/tendermint/types"
	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/query"
	txsvc "github.com/cosmos/cosmos-sdk/types/tx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	globalfeev1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/globalfee/v1"
	txqueryv1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/txquery/v1"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
)

// NewGRPC connects to an exchange-run node's gRPC endpoint (port 9090).
// Transport security defaults to insecure (exchange-local node); supply TLS
// credentials via WithGRPCDialOptions.
func NewGRPC(target string, opts ...Option) (Client, error) {
	o := applyOptions(opts)
	dialOpts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, o.dialOpts...)
	conn, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", target, err)
	}
	return &grpcClient{
		target: target,
		conn:   conn,
		opts:   o,
		txSvc:  txsvc.NewServiceClient(conn),
		authQ:  authv1beta1.NewQueryClient(conn),
		bankQ:  bankv1beta1.NewQueryClient(conn),
		cmtSvc: tmv1beta1.NewServiceClient(conn),
		feeQ:   globalfeev1.NewQueryClient(conn),
		txqQ:   txqueryv1.NewQueryClient(conn),
	}, nil
}

type grpcClient struct {
	target string
	conn   *grpc.ClientConn
	opts   *options
	probe  probeState

	txSvc  txsvc.ServiceClient
	authQ  authv1beta1.QueryClient
	bankQ  bankv1beta1.QueryClient
	cmtSvc tmv1beta1.ServiceClient
	feeQ   globalfeev1.QueryClient
	txqQ   txqueryv1.QueryClient
}

func (c *grpcClient) mapErr(err error) error {
	if err == nil {
		return nil
	}
	c.opts.countGRPCErr(c.target)
	switch status.Code(err) {
	case codes.Unimplemented:
		return fmt.Errorf("%v: %w", err, ErrUnsupported)
	case codes.NotFound:
		return fmt.Errorf("%v: %w", err, ErrNotFound)
	}
	return err
}

func (c *grpcClient) Account(ctx context.Context, addr string) (uint64, uint64, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	resp, err := c.authQ.Account(ctx, &authv1beta1.QueryAccountRequest{Address: addr})
	if err != nil {
		return 0, 0, c.mapErr(err)
	}
	return accountFromAny(resp.GetAccount())
}

func (c *grpcClient) Balance(ctx context.Context, addr, denom string) (sdkmath.Int, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	resp, err := c.bankQ.Balance(ctx, &bankv1beta1.QueryBalanceRequest{Address: addr, Denom: denom})
	if err != nil {
		return sdkmath.ZeroInt(), c.mapErr(err)
	}
	if resp.GetBalance() == nil {
		return sdkmath.ZeroInt(), nil
	}
	return intFromCoinAmount(resp.GetBalance().GetAmount())
}

func (c *grpcClient) AllBalances(ctx context.Context, addr string) (sdk.Coins, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	resp, err := c.bankQ.AllBalances(ctx, &bankv1beta1.QueryAllBalancesRequest{Address: addr})
	if err != nil {
		return nil, c.mapErr(err)
	}
	return coinsFromPulsar(resp.GetBalances())
}

func (c *grpcClient) DenomMetadata(ctx context.Context, denom string) (*bankv1beta1.Metadata, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	resp, err := c.bankQ.DenomMetadata(ctx, &bankv1beta1.QueryDenomMetadataRequest{Denom: denom})
	if err != nil {
		return nil, c.mapErr(err)
	}
	return resp.GetMetadata(), nil
}

func (c *grpcClient) Tx(ctx context.Context, hash string) (*TxInfo, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	resp, err := c.txSvc.GetTx(ctx, &txsvc.GetTxRequest{Hash: hash})
	if err != nil {
		return nil, c.mapErr(err)
	}
	r := resp.TxResponse
	if r == nil {
		return nil, ErrNotFound
	}
	info := &TxInfo{
		Hash:      r.TxHash,
		Height:    r.Height,
		Code:      r.Code,
		Codespace: r.Codespace,
		RawLog:    r.RawLog,
		GasWanted: r.GasWanted,
		GasUsed:   r.GasUsed,
		Events:    eventsFromABCI(r.Events),
	}
	// Re-encoded tx (not the original raw broadcast bytes); the scanner's
	// byte-exact path uses the CometBFT transport.
	if r.Tx != nil {
		info.TxBytes = r.Tx.Value
	}
	return info, nil
}

func blockFromCmtService(blockID *tmtypespb.BlockID, sdkBlock *tmv1beta1.Block, protoBlock *tmtypespb.Block) *Block {
	b := &Block{Hash: blockID.GetHash()}
	switch {
	case sdkBlock != nil:
		h := sdkBlock.GetHeader()
		b.ChainID = h.GetChainId()
		b.Height = h.GetHeight()
		if t := h.GetTime(); t != nil {
			b.Time = t.AsTime()
		}
		b.LastBlockHash = h.GetLastBlockId().GetHash()
		b.AppHash = h.GetAppHash()
		b.Txs = sdkBlock.GetData().GetTxs()
	case protoBlock != nil:
		h := protoBlock.GetHeader()
		b.ChainID = h.GetChainId()
		b.Height = h.GetHeight()
		if t := h.GetTime(); t != nil {
			b.Time = t.AsTime()
		}
		b.LastBlockHash = h.GetLastBlockId().GetHash()
		b.AppHash = h.GetAppHash()
		b.Txs = protoBlock.GetData().GetTxs()
	}
	return b
}

func (c *grpcClient) BlockByHeight(ctx context.Context, height int64) (*Block, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	resp, err := c.cmtSvc.GetBlockByHeight(ctx, &tmv1beta1.GetBlockByHeightRequest{Height: height})
	if err != nil {
		return nil, c.mapErr(err)
	}
	return blockFromCmtService(resp.GetBlockId(), resp.GetSdkBlock(), resp.GetBlock()), nil
}

func (c *grpcClient) LatestBlock(ctx context.Context) (*Block, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	resp, err := c.cmtSvc.GetLatestBlock(ctx, &tmv1beta1.GetLatestBlockRequest{})
	if err != nil {
		return nil, c.mapErr(err)
	}
	return blockFromCmtService(resp.GetBlockId(), resp.GetSdkBlock(), resp.GetBlock()), nil
}

// BlockResults has no gRPC service in the SDK; use the CometBFT transport.
func (c *grpcClient) BlockResults(ctx context.Context, height int64) (*BlockResults, error) {
	return nil, fmt.Errorf("block results over grpc (use the CometBFT-RPC transport): %w", ErrUnsupported)
}

func (c *grpcClient) NodeStatus(ctx context.Context) (*NodeStatus, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	blk, err := c.cmtSvc.GetLatestBlock(ctx, &tmv1beta1.GetLatestBlockRequest{})
	if err != nil {
		return nil, c.mapErr(err)
	}
	sync, err := c.cmtSvc.GetSyncing(ctx, &tmv1beta1.GetSyncingRequest{})
	if err != nil {
		return nil, c.mapErr(err)
	}
	b := blockFromCmtService(blk.GetBlockId(), blk.GetSdkBlock(), blk.GetBlock())
	return &NodeStatus{
		ChainID:         b.ChainID,
		LatestHeight:    b.Height,
		LatestBlockTime: b.Time,
		LatestBlockHash: b.Hash,
		AppHash:         b.AppHash,
		CatchingUp:      sync.GetSyncing(),
	}, nil
}

func (c *grpcClient) Simulate(ctx context.Context, txBytes []byte) (*SimulateResult, error) {
	if c.probe.simulateBlocked() {
		return nil, ErrSimulateUnavailable
	}
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	resp, err := c.txSvc.Simulate(ctx, &txsvc.SimulateRequest{TxBytes: txBytes})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			c.probe.set(false)
			c.opts.countGRPCErr(c.target)
			return nil, fmt.Errorf("%v: %w", err, ErrSimulateUnavailable)
		}
		return nil, c.mapErr(err)
	}
	res := &SimulateResult{}
	if resp.GasInfo != nil {
		res.GasWanted = resp.GasInfo.GasWanted
		res.GasUsed = resp.GasInfo.GasUsed
	}
	return res, nil
}

func (c *grpcClient) Broadcast(ctx context.Context, txBytes []byte, mode BroadcastMode) (*BroadcastResult, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	grpcMode := txsvc.BroadcastMode_BROADCAST_MODE_SYNC
	switch mode {
	case BroadcastSync, "":
	case BroadcastAsync:
		grpcMode = txsvc.BroadcastMode_BROADCAST_MODE_ASYNC
	default:
		return nil, fmt.Errorf("unknown broadcast mode %q", mode)
	}
	resp, err := c.txSvc.BroadcastTx(ctx, &txsvc.BroadcastTxRequest{TxBytes: txBytes, Mode: grpcMode})
	if err != nil {
		return nil, c.mapErr(err)
	}
	r := resp.TxResponse
	if r == nil {
		return nil, fmt.Errorf("broadcast: empty tx_response")
	}
	return &BroadcastResult{
		TxHash:    r.TxHash,
		Code:      r.Code,
		Codespace: r.Codespace,
		RawLog:    r.RawLog,
		Accepted:  r.Code == 0,
	}, nil
}

func (c *grpcClient) GlobalFeeParams(ctx context.Context) (*globalfeev1.Params, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	resp, err := c.feeQ.Params(ctx, &globalfeev1.QueryParamsRequest{})
	if err != nil {
		return nil, c.mapErr(err)
	}
	return &resp.Params, nil
}

func (c *grpcClient) TxsByAddress(ctx context.Context, addr string, page *query.PageRequest, opts ...TxsByAddressOptions) (*txqueryv1.GetTxsByAddressResponse, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	resp, err := c.txqQ.GetTxsByAddress(ctx, txsByAddressRequest(addr, page, opts...))
	if err != nil {
		return nil, c.mapErr(err)
	}
	return resp, nil
}

func (c *grpcClient) Probe(ctx context.Context) (ProbeResult, error) {
	ctx, cancel := c.opts.callCtx(ctx)
	defer cancel()
	res := ProbeResult{}
	if _, err := c.cmtSvc.GetLatestBlock(ctx, &tmv1beta1.GetLatestBlockRequest{}); err != nil {
		c.opts.countGRPCErr(c.target)
		c.probe.set(false)
		return res, fmt.Errorf("probe: node unreachable at %s: %w", c.target, err)
	}
	res.NodeReachable = true
	// An empty SimulateRequest proves routability: a registered route answers
	// (even with an argument error); an unregistered one is Unimplemented.
	_, err := c.txSvc.Simulate(ctx, &txsvc.SimulateRequest{})
	res.TxServiceRoutable = status.Code(err) != codes.Unimplemented
	c.probe.set(res.TxServiceRoutable)
	c.opts.logger.Info("probe complete",
		logging.FieldNodeEndpoint, c.target,
		"node_reachable", res.NodeReachable,
		"tx_service_routable", res.TxServiceRoutable)
	return res, nil
}

func (c *grpcClient) Close() error {
	return c.conn.Close()
}
