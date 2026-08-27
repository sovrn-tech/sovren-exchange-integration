package sequences

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/postgres"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
)

const testSource = "sovr1hotwallettestaddr"

// fakeChain is an in-memory sequences.Chain.
type fakeChain struct {
	mu            sync.Mutex
	accountNumber uint64
	sequence      uint64
	included      map[string]*client.TxInfo
	broadcasts    [][]byte
	broadcastRes  client.BroadcastResult
}

func newFakeChain(seq uint64) *fakeChain {
	return &fakeChain{
		accountNumber: 42,
		sequence:      seq,
		included:      map[string]*client.TxInfo{},
		broadcastRes:  client.BroadcastResult{Accepted: true},
	}
}

func (f *fakeChain) Account(ctx context.Context, addr string) (uint64, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accountNumber, f.sequence, nil
}

func (f *fakeChain) Tx(ctx context.Context, hash string) (*client.TxInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if info, ok := f.included[strings.ToUpper(hash)]; ok {
		return info, nil
	}
	return nil, client.ErrNotFound
}

func (f *fakeChain) Broadcast(ctx context.Context, txBytes []byte, mode client.BroadcastMode) (*client.BroadcastResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(txBytes))
	copy(cp, txBytes)
	f.broadcasts = append(f.broadcasts, cp)
	res := f.broadcastRes
	res.TxHash = hashOf(txBytes)
	return &res, nil
}

func (f *fakeChain) include(hash string, height int64, code uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.included[strings.ToUpper(hash)] = &client.TxInfo{Hash: hash, Height: height, Code: code}
}

func hashOf(b []byte) string {
	d := sha256.Sum256(b)
	return strings.ToUpper(hex.EncodeToString(d[:]))
}

func openSQLite(t *testing.T) storage.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "kit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// walkWithdrawal creates a withdrawal and advances it to the wanted status,
// persisting signed bytes/hash at SIGNED (legal-transition walk only).
func walkWithdrawal(t *testing.T, s storage.Store, chainID, id string, target storage.WithdrawalStatus, signedBytes []byte) storage.WithdrawalRecord {
	t.Helper()
	ctx := context.Background()
	rec, err := s.Withdrawals().Create(ctx, storage.WithdrawalRecord{
		WithdrawalID:       id,
		IdempotencyKey:     "idem-" + id,
		ChainID:            chainID,
		SourceAddress:      testSource,
		DestinationAddress: "sovr1destinationtestaddr",
		Denom:              storage.BaseDenom,
		AmountBaseUnits:    sdkmath.NewInt(1000000),
		SignMode:           storage.SignModeDirect,
		Status:             storage.WithdrawalRequested,
	})
	require.NoError(t, err)

	path := []storage.WithdrawalStatus{
		storage.WithdrawalAddressValidated, storage.WithdrawalComplianceApproved,
		storage.WithdrawalFundsReserved, storage.WithdrawalSequenceReserved,
		storage.WithdrawalTransactionBuilt, storage.WithdrawalTransactionSimulated,
		storage.WithdrawalSigned, storage.WithdrawalBroadcast,
	}
	from := storage.WithdrawalRequested
	for _, next := range path {
		if from == target {
			break
		}
		set := storage.WithdrawalUpdate{}
		if next == storage.WithdrawalSigned {
			set.SignedTxBytes = signedBytes
			h := hashOf(signedBytes)
			set.TxHash = &h
		}
		require.NoError(t, s.Withdrawals().UpdateStatus(ctx, id, from, next, set))
		from = next
	}
	got, err := s.Withdrawals().Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, target, got.Status)
	return rec
}

func TestReserveSequentialAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openSQLite(t)
	chain := newFakeChain(5)
	m := NewManager(s, chain)
	const chainID = "test-sovr-1"

	for i, want := range []uint64{5, 6, 7} {
		id := fmt.Sprintf("W%d", i)
		walkWithdrawal(t, s, chainID, id, storage.WithdrawalRequested, nil)
		res, err := m.Reserve(ctx, chainID, testSource, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: id})
		require.NoError(t, err)
		require.Equal(t, want, res.Sequence)
		require.Equal(t, uint64(42), res.AccountNumber)
	}

	// Same work_ref again: the existing binding wins; no new slot.
	again, err := m.Reserve(ctx, chainID, testSource, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: "W0"})
	require.NoError(t, err)
	require.Equal(t, uint64(5), again.Sequence)
	open, err := s.Sequences().ListUnconsumed(ctx, chainID, testSource)
	require.NoError(t, err)
	require.Len(t, open, 3)
}

// TestConcurrentReserve races goroutines allocating sequences for ONE
// account: every worker must get a distinct sequence with no duplicate-slot
// errors (SQLite: single-writer serialization).
func TestConcurrentReserve(t *testing.T) {
	testConcurrentReserveOn(t, openSQLite(t))
}

// TestConcurrentReservePostgres runs the same race on PostgreSQL
// (SELECT … FOR UPDATE serialization) when SOVREN_TEST_POSTGRES_DSN is set.
func TestConcurrentReservePostgres(t *testing.T) {
	dsn := os.Getenv("SOVREN_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SOVREN_TEST_POSTGRES_DSN not set; skipping PostgreSQL race")
	}
	s, err := postgres.Open(dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	testConcurrentReserveOn(t, s)
}

func testConcurrentReserveOn(t *testing.T, s storage.Store) {
	t.Helper()
	ctx := context.Background()
	chain := newFakeChain(0)
	m := NewManager(s, chain)
	// Unique chain ID per run keeps reruns against a persistent database clean.
	chainID := "race-" + hashOf([]byte(t.Name() + t.TempDir()))[:12]

	const workers = 20
	for i := range workers {
		walkWithdrawal(t, s, chainID, fmt.Sprintf("CW-%s-%02d", chainID, i), storage.WithdrawalRequested, nil)
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seqs = map[uint64]bool{}
		errs []error
	)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := m.Reserve(ctx, chainID, testSource, storage.WorkRef{
				Kind: storage.WorkWithdrawal, ID: fmt.Sprintf("CW-%s-%02d", chainID, i),
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			require.False(t, seqs[res.Sequence], "sequence %d handed out twice", res.Sequence)
			seqs[res.Sequence] = true
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Len(t, seqs, workers)
	for i := range uint64(workers) {
		require.True(t, seqs[i], "sequence %d missing", i)
	}
}

// TestReconcileMatrix pins the §6 crash-recovery matrix:
// RESERVED/SIGNED/BROADCAST × tx found/not-found. Only unsigned RESERVED
// reservations release; every signed-or-ambiguous case quarantines.
func TestReconcileMatrix(t *testing.T) {
	signedBytes := []byte("signed-tx-bytes-exact")
	cases := []struct {
		name        string
		resStatus   storage.SequenceReservationStatus
		wdStatus    storage.WithdrawalStatus
		signedBytes []byte
		txFound     bool
		want        storage.SequenceReservationStatus
	}{
		{"reserved_not_found", storage.SequenceReserved, storage.WithdrawalFundsReserved, nil, false, storage.SequenceReleased},
		{"reserved_but_signed_bytes_exist", storage.SequenceReserved, storage.WithdrawalSigned, signedBytes, false, storage.SequenceReconciliationRequired},
		{"signed_found", storage.SequenceSigned, storage.WithdrawalSigned, signedBytes, true, storage.SequenceConsumed},
		{"signed_not_found", storage.SequenceSigned, storage.WithdrawalSigned, signedBytes, false, storage.SequenceReconciliationRequired},
		{"broadcast_found", storage.SequenceBroadcast, storage.WithdrawalBroadcast, signedBytes, true, storage.SequenceConsumed},
		{"broadcast_not_found", storage.SequenceBroadcast, storage.WithdrawalBroadcast, signedBytes, false, storage.SequenceReconciliationRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := openSQLite(t)
			chain := newFakeChain(10)
			m := NewManager(s, chain)
			const chainID = "test-sovr-1"

			id := "W-" + tc.name
			walkWithdrawal(t, s, chainID, id, tc.wdStatus, tc.signedBytes)
			res, err := s.Sequences().Reserve(ctx, storage.SequenceReservation{
				ChainID: chainID, SourceAddress: testSource, AccountNumber: 42, Sequence: 10,
				WorkRef: storage.WorkRef{Kind: storage.WorkWithdrawal, ID: id},
			})
			require.NoError(t, err)
			from := storage.SequenceReserved
			for _, next := range []storage.SequenceReservationStatus{storage.SequenceSigned, storage.SequenceBroadcast} {
				if from == tc.resStatus {
					break
				}
				require.NoError(t, s.Sequences().UpdateStatus(ctx, res.ID, from, next))
				from = next
			}
			if tc.txFound {
				chain.include(hashOf(tc.signedBytes), 100, 0)
			}

			report, err := m.ReconcileAccount(ctx, chainID, testSource)
			require.NoError(t, err)
			got, err := s.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: id})
			require.NoError(t, err)
			require.Equal(t, tc.want, got.Status)

			switch tc.want {
			case storage.SequenceReleased:
				require.Equal(t, 1, report.Released)
			case storage.SequenceConsumed:
				require.Equal(t, 1, report.Consumed)
			case storage.SequenceReconciliationRequired:
				require.Equal(t, 1, report.Quarantined, "not-found must quarantine, never release")
			}
			// A quarantined or consumed reservation never RELEASED implicitly.
			if tc.want != storage.SequenceReleased {
				require.Equal(t, 0, report.Released)
			}
		})
	}
}

// TestRebroadcastPersistedIdenticalBytes pins the recovery contract: search
// first, then rebroadcast the EXACT persisted bytes; an included tx is never
// rebroadcast; nothing here can re-sign.
func TestRebroadcastPersistedIdenticalBytes(t *testing.T) {
	ctx := context.Background()
	s := openSQLite(t)
	chain := newFakeChain(0)
	m := NewManager(s, chain)
	bytes := []byte("persisted-signed-bytes")

	// Not found: broadcast happens with byte-identical payload.
	res, err := m.RebroadcastPersisted(ctx, bytes)
	require.NoError(t, err)
	require.False(t, res.AlreadyIncluded)
	require.True(t, res.Accepted)
	require.Len(t, chain.broadcasts, 1)
	require.Equal(t, bytes, chain.broadcasts[0])
	require.Equal(t, hashOf(bytes), res.TxHash)

	// Found: no second broadcast.
	chain.include(hashOf(bytes), 77, 0)
	res, err = m.RebroadcastPersisted(ctx, bytes)
	require.NoError(t, err)
	require.True(t, res.AlreadyIncluded)
	require.Equal(t, int64(77), res.Height)
	require.Len(t, chain.broadcasts, 1)
}

// TestReleasedSlotReclaim pins the released-reservation path: the same work
// item reclaims its original slot while it is still ahead of chain truth and
// is refused (quarantine signal) once the slot is burned.
func TestReleasedSlotReclaim(t *testing.T) {
	ctx := context.Background()
	s := openSQLite(t)
	chain := newFakeChain(5)
	m := NewManager(s, chain)
	const chainID = "test-sovr-1"

	walkWithdrawal(t, s, chainID, "W-rc", storage.WithdrawalFundsReserved, nil)
	ref := storage.WorkRef{Kind: storage.WorkWithdrawal, ID: "W-rc"}
	res, err := m.Reserve(ctx, chainID, testSource, ref)
	require.NoError(t, err)
	require.NoError(t, s.Sequences().UpdateStatus(ctx, res.ID, storage.SequenceReserved, storage.SequenceReleased))

	// Slot still ahead of chain truth: reclaimed, same sequence.
	re, err := m.Reserve(ctx, chainID, testSource, ref)
	require.NoError(t, err)
	require.Equal(t, res.Sequence, re.Sequence)
	require.Equal(t, storage.SequenceReserved, re.Status)

	// Burn the slot (chain moved past it): refuse with the quarantine signal.
	require.NoError(t, s.Sequences().UpdateStatus(ctx, re.ID, storage.SequenceReserved, storage.SequenceReleased))
	chain.mu.Lock()
	chain.sequence = re.Sequence + 1
	chain.mu.Unlock()
	_, err = m.Reserve(ctx, chainID, testSource, ref)
	require.ErrorIs(t, err, ErrReleasedSlotUnusable)
}
