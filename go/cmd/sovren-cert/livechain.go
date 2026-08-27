package main

// Live-chain session for the chain-gated scenario groups (D, R, W, S): a
// CometBFT RPC client, the funded test key from SOVREN_CERT_MNEMONIC, fresh
// per-scenario throwaway keys, and raw transaction construction for shapes
// the kit builder intentionally refuses (multi-message, MsgMultiSend).
//
// The session refuses mainnet (chain_id sovr-1) unconditionally.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	signingtypes "github.com/cosmos/cosmos-sdk/types/tx/signing"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"

	sdkmath "cosmossdk.io/math"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/deposits"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

type liveEnv struct {
	client   client.Client
	chainID  string
	rpcURL   string
	funder   address.Address
	mnemonic string
	gasPrice string
}

// minFunderBalanceUsovr is the session sanity floor (10 SOVR).
const minFunderBalanceUsovr = 10_000_000

// liveChain lazily opens (and caches) the live-chain session.
func (rc *RunContext) liveChain(ctx context.Context) (*liveEnv, error) {
	rc.liveMu.Lock()
	defer rc.liveMu.Unlock()
	if rc.live != nil || rc.liveErr != nil {
		return rc.live, rc.liveErr
	}
	env, err := openLiveEnv(ctx, rc)
	rc.live, rc.liveErr = env, err
	return env, err
}

func openLiveEnv(ctx context.Context, rc *RunContext) (*liveEnv, error) {
	c, err := client.NewCometRPC(rc.RPCURL, client.WithTimeout(15*time.Second))
	if err != nil {
		return nil, fmt.Errorf("rpc target %s: %w", rc.RPCURL, err)
	}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	st, err := c.NodeStatus(sctx)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("chain unreachable at %s: %w", rc.RPCURL, err)
	}
	if st.ChainID == "sovr-1" {
		c.Close()
		return nil, fmt.Errorf("refusing to run certification drills against mainnet (chain_id sovr-1); use an isolated throwaway chain")
	}
	funder, err := address.DeriveAddress(rc.Mnemonic, address.DefaultHDPath)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("%s: %w", envMnemonic, err)
	}
	bal, err := c.Balance(sctx, funder.Bech32, storage.BaseDenom)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("funded account %s not queryable: %w", funder.Bech32, err)
	}
	if bal.LT(sdkmath.NewInt(minFunderBalanceUsovr)) {
		c.Close()
		return nil, fmt.Errorf("funded account %s holds %s usovr (< %d): fund it before running chain-dependent scenarios",
			funder.Bech32, bal, int64(minFunderBalanceUsovr))
	}
	return &liveEnv{
		client:   c,
		chainID:  st.ChainID,
		rpcURL:   rc.RPCURL,
		funder:   funder,
		mnemonic: rc.Mnemonic,
		gasPrice: rc.GasPrice,
	}, nil
}

// freshKey derives a scenario-scoped throwaway key from the cert mnemonic at
// a distinct index (time-salted to avoid cross-run address reuse on a
// long-lived dev chain).
func (e *liveEnv) freshKey(slot uint32) (address.Address, error) {
	idx := 1000 + (uint32(time.Now().UnixNano()/1e6)%400000)*10 + slot
	return address.DeriveAddress(e.mnemonic, fmt.Sprintf("m/44'/118'/0'/0/%d", idx))
}

// --- raw transaction construction -----------------------------------------

type rawSigner struct {
	key      address.Address
	sequence uint64
	accNum   uint64
}

// buildRawTx marshals TxBody+AuthInfo for msgs, signing SIGN_MODE_DIRECT
// with every signer in order. When sign is false, deterministic dummy
// signatures are attached (parser drills — never broadcastable).
func buildRawTx(chainID string, msgs []proto.Message, signers []rawSigner, memo string, gasLimit uint64, feeUsovr int64, sign bool) ([]byte, string, error) {
	anys := make([]*codectypes.Any, 0, len(msgs))
	for _, m := range msgs {
		a, err := codectypes.NewAnyWithValue(m)
		if err != nil {
			return nil, "", fmt.Errorf("pack msg: %w", err)
		}
		anys = append(anys, a)
	}
	body := &txtypes.TxBody{Messages: anys, Memo: memo}
	bodyBytes, err := proto.Marshal(body)
	if err != nil {
		return nil, "", err
	}

	infos := make([]*txtypes.SignerInfo, 0, len(signers))
	for _, s := range signers {
		pk := &secp256k1.PubKey{Key: s.key.PublicKeyCompressed}
		pkAny, err := codectypes.NewAnyWithValue(pk)
		if err != nil {
			return nil, "", err
		}
		infos = append(infos, &txtypes.SignerInfo{
			PublicKey: pkAny,
			ModeInfo: &txtypes.ModeInfo{Sum: &txtypes.ModeInfo_Single_{
				Single: &txtypes.ModeInfo_Single{Mode: signingtypes.SignMode_SIGN_MODE_DIRECT},
			}},
			Sequence: s.sequence,
		})
	}
	authInfo := &txtypes.AuthInfo{
		SignerInfos: infos,
		Fee: &txtypes.Fee{
			Amount:   sdk.NewCoins(sdk.NewInt64Coin(storage.BaseDenom, feeUsovr)),
			GasLimit: gasLimit,
		},
	}
	authBytes, err := proto.Marshal(authInfo)
	if err != nil {
		return nil, "", err
	}

	sigs := make([][]byte, 0, len(signers))
	for _, s := range signers {
		if !sign {
			sigs = append(sigs, make([]byte, 64))
			continue
		}
		doc := &txtypes.SignDoc{
			BodyBytes:     bodyBytes,
			AuthInfoBytes: authBytes,
			ChainId:       chainID,
			AccountNumber: s.accNum,
		}
		docBytes, err := proto.Marshal(doc)
		if err != nil {
			return nil, "", err
		}
		priv := &secp256k1.PrivKey{Key: s.key.PrivateKey}
		sig, err := priv.Sign(docBytes)
		if err != nil {
			return nil, "", err
		}
		sigs = append(sigs, sig)
	}

	raw := &txtypes.TxRaw{BodyBytes: bodyBytes, AuthInfoBytes: authBytes, Signatures: sigs}
	txBytes, err := proto.Marshal(raw)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(txBytes)
	return txBytes, strings.ToUpper(hex.EncodeToString(digest[:])), nil
}

// broadcastAndWait pushes txBytes and waits for inclusion, returning the
// execution result (including failed executions — the caller asserts codes).
func (e *liveEnv) broadcastAndWait(ctx context.Context, txBytes []byte, txHash string) (*client.TxInfo, error) {
	bctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	res, err := e.client.Broadcast(bctx, txBytes, client.BroadcastSync)
	if err != nil {
		return nil, fmt.Errorf("broadcast: %w", err)
	}
	if !res.Accepted {
		return nil, fmt.Errorf("CheckTx rejected (code %d/%s): %s", res.Code, res.Codespace, res.RawLog)
	}
	for {
		info, err := e.client.Tx(bctx, txHash)
		if err == nil {
			return info, nil
		}
		if !errors.Is(err, client.ErrNotFound) {
			return nil, fmt.Errorf("tx lookup: %w", err)
		}
		select {
		case <-bctx.Done():
			return nil, fmt.Errorf("tx %s not included before timeout", txHash)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// sendFromKey signs and broadcasts msgs from one key, waiting for inclusion.
func (e *liveEnv) sendFromKey(ctx context.Context, key address.Address, msgs []proto.Message, memo string) (*client.TxInfo, string, error) {
	accNum, seq, err := e.client.Account(ctx, key.Bech32)
	if err != nil {
		return nil, "", fmt.Errorf("account %s: %w", key.Bech32, err)
	}
	txBytes, txHash, err := buildRawTx(e.chainID, msgs, []rawSigner{{key: key, sequence: seq, accNum: accNum}},
		memo, 250000, 6250, true)
	if err != nil {
		return nil, "", err
	}
	info, err := e.broadcastAndWait(ctx, txBytes, txHash)
	return info, txHash, err
}

// fund sends amount usovr from the funder to addr and waits for success.
func (e *liveEnv) fund(ctx context.Context, addr string, amount int64) (*client.TxInfo, string, error) {
	msg := &banktypes.MsgSend{
		FromAddress: e.funder.Bech32,
		ToAddress:   addr,
		Amount:      sdk.NewCoins(sdk.NewInt64Coin(storage.BaseDenom, amount)),
	}
	info, hash, err := e.sendFromKey(ctx, e.funder, []proto.Message{msg}, "")
	if err != nil {
		return nil, hash, err
	}
	if info.Code != 0 {
		return info, hash, fmt.Errorf("funding tx failed on chain (code %d): %s", info.Code, info.RawLog)
	}
	return info, hash, nil
}

// --- scanner-side helpers ---------------------------------------------------

// certScanner builds a scanner over a throwaway store for the live chain.
func (e *liveEnv) certScanner(st storage.Store, startHeight uint64, minDeposit int64) (*deposits.Scanner, error) {
	cfg := deposits.ScannerConfig{
		ChainID:       e.chainID,
		Confirmations: 1,
		StartHeight:   startHeight,
		PollInterval:  300 * time.Millisecond,
	}
	if minDeposit > 0 {
		cfg.MinimumDepositUsovr = sdkmath.NewInt(minDeposit)
	}
	return deposits.NewScanner(e.client, st, cfg)
}

// currentHeight returns the chain tip.
func (e *liveEnv) currentHeight(ctx context.Context) (uint64, error) {
	st, err := e.client.NodeStatus(ctx)
	if err != nil {
		return 0, err
	}
	return uint64(st.LatestHeight), nil
}

// waitDeposit cycles the scanner until the deposit identified by the FR-024
// unique key reaches one of the wanted statuses.
func waitDeposit(ctx context.Context, sc *deposits.Scanner, st storage.Store, chainID, txHash string,
	mi, ci uint32, recipient string, want map[storage.DepositStatus]bool, timeout time.Duration) (storage.DepositRecord, error) {

	deadline := time.Now().Add(timeout)
	var last storage.DepositRecord
	var lastErr error
	for time.Now().Before(deadline) {
		if err := sc.Cycle(ctx); err != nil {
			return last, fmt.Errorf("scanner cycle: %w", err)
		}
		d, err := st.Deposits().Get(ctx, chainID, txHash, mi, ci, recipient)
		lastErr = err
		if err == nil {
			last = d
			if want[d.Status] {
				return d, nil
			}
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return last, fmt.Errorf("deposit %s/%d/%d for %s never appeared: %w", txHash, mi, ci, recipient, lastErr)
	}
	return last, fmt.Errorf("deposit %s/%d/%d stuck in status %s", txHash, mi, ci, last.Status)
}

// scanPast cycles the scanner until the checkpoint reaches at least height.
func scanPast(ctx context.Context, sc *deposits.Scanner, st storage.Store, chainID string, height uint64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := sc.Cycle(ctx); err != nil {
			return fmt.Errorf("scanner cycle: %w", err)
		}
		cp, err := st.Checkpoints().Get(ctx, chainID)
		if err == nil && cp.LastFullyProcessedHeight >= height {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	return fmt.Errorf("scanner did not reach height %d before timeout", height)
}
