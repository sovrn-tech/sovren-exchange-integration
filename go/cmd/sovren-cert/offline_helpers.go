package main

// Shared helpers for the offline (no-chain) scenarios: throwaway SQLite
// stores and synthetic-but-valid transactions for the stub chain.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
	"github.com/sovrn-tech/sovren-exchange-integration/go/tx"
)

// counterValue reads the current value of a single counter/gauge child.
func counterValue(c prometheus.Metric) float64 {
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		return -1
	}
	if m.Counter != nil {
		return m.Counter.GetValue()
	}
	if m.Gauge != nil {
		return m.Gauge.GetValue()
	}
	return -1
}

// certChainID is the synthetic chain id used by offline drills.
const certChainID = "cert-local-1"

// certTestMnemonic is the kit's published UNSAFE_TEST_ONLY vector mnemonic —
// used exclusively to mint deterministic throwaway keys for synthetic
// blocks. Never fund it anywhere.
const certTestMnemonic = "special sign fit simple patrol salute grocery chicken wheat radar tonight ceiling"

// tempStore opens a throwaway SQLite store; cleanup removes it.
func tempStore(prefix string) (storage.Store, func(), error) {
	dir, err := os.MkdirTemp("", "sovren-cert-"+prefix+"-*")
	if err != nil {
		return nil, nil, err
	}
	st, err := sqlite.Open(filepath.Join(dir, "cert.db"))
	if err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	cleanup := func() {
		st.Close()
		os.RemoveAll(dir)
	}
	return st, cleanup, nil
}

// certKey derives a deterministic throwaway key at index i.
func certKey(i uint32) (address.Address, error) {
	return address.DeriveAddress(certTestMnemonic, fmt.Sprintf("m/44'/118'/0'/0/%d", i))
}

// buildSignedSend assembles a fully valid single-MsgSend transaction from a
// derived key (real signature — tx.Assemble verifies it), for injection into
// stub-chain blocks.
func buildSignedSend(from address.Address, to string, amount, memo, chainID string, sequence uint64) (txBytes []byte, txHash string, err error) {
	u, err := tx.BuildMsgSend(from.Bech32, to, amount, memo)
	if err != nil {
		return nil, "", err
	}
	signDoc, _, err := u.SignDoc(chainID, 1, sequence, tx.Fee{AmountBaseUnits: "5000", GasLimit: 200000}, from.PublicKeyCompressed)
	if err != nil {
		return nil, "", err
	}
	priv := &secp256k1.PrivKey{Key: from.PrivateKey}
	sig, err := priv.Sign(signDoc)
	if err != nil {
		return nil, "", err
	}
	return tx.Assemble(u, tx.SignatureResponse{Signature: sig, PubKeyCompressed: from.PublicKeyCompressed})
}

// watchAddr registers one active watched address.
func watchAddr(ctx context.Context, st storage.Store, chainID, addr string, kind storage.WatchedAddressKind) error {
	return st.Watch().Upsert(ctx, storage.WatchedAddress{
		ChainID: chainID,
		Address: addr,
		Kind:    kind,
		Active:  true,
	})
}
