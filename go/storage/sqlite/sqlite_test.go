package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/internal/storetest"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
)

// TestSQLiteSuite runs the full backend conformance suite (unique
// constraints, state machines, WithTx atomicity, concurrent reservation)
// against a file-based temp database.
func TestSQLiteSuite(t *testing.T) {
	storetest.RunSuite(t, func(t *testing.T) storage.Store {
		s, err := sqlite.Open(filepath.Join(t.TempDir(), "kit.db"))
		require.NoError(t, err)
		return s
	})
}

// TestReopenPersists proves the store is durable across Open/Close cycles
// and that re-applying migrations on reopen is a no-op.
func TestReopenPersists(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kit.db")

	s, err := sqlite.Open(path)
	require.NoError(t, err)
	_, err = s.Deposits().Insert(ctx, storage.DepositRecord{
		ChainID:          "sovr-1",
		TxHash:           "PERSIST",
		RecipientAddress: "sovr1cust",
		Denom:            storage.BaseDenom,
		AmountBaseUnits:  sdkmath.NewInt(1),
		Status:           storage.DepositDiscovered,
	})
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s, err = sqlite.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	got, err := s.Deposits().Get(ctx, "sovr-1", "PERSIST", 0, 0, "sovr1cust")
	require.NoError(t, err)
	require.Equal(t, storage.DepositDiscovered, got.Status)
}

// TestFileURIDSN proves Open accepts an explicit file: DSN with its own
// query parameters.
func TestFileURIDSN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kit.db")
	s, err := sqlite.Open("file:" + path + "?cache=private")
	require.NoError(t, err)
	require.NoError(t, s.Close())
}
