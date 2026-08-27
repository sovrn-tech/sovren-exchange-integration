// withdrawals service — drives the data-model §5 withdrawal state machine
// (workflow + FR-035 broadcaster) and wires the configured signer kind
// (contracts/signer-interface.md §Remote signer transports).
//
// One-MsgSend-per-transaction (FR-036) is enforced by construction: the
// workflow builds through tx.BuildMsgSend only, and the sign-doc summary
// derivation rejects any doc that carries more than one message.
//
// Signer transport secrets never appear in adapter.yaml; they arrive via
// environment variables documented in docs/withdrawal-processing.md:
//
//	grpc-remote: SOVREN_SIGNER_TLS_CA_FILE / _TLS_CERT_FILE / _TLS_KEY_FILE
//	             / _TLS_SERVER_NAME (mTLS REQUIRED in production);
//	             SOVREN_SIGNER_ALLOW_INSECURE_DEV=true only off-mainnet
//	exec:        binary path from signer.endpoint
//	unsafe-local: SOVREN_SIGNER_UNSAFE=UNSAFE_TEST_ONLY +
//	             SOVREN_SIGNER_MNEMONIC (+ optional SOVREN_SIGNER_HD_PATH);
//	             refused when the manifest declares mainnet
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/sequences"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer/execsigner"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer/grpcremote"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer/local"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/withdrawals"
)

func init() {
	register("withdrawals", runWithdrawalsService)
}

const withdrawalsDrivePeriod = 2 * time.Second

func runWithdrawalsService(ctx context.Context, deps *Deps) error {
	log := deps.Logger.With(logging.FieldService, "withdrawals")

	sg, closeSigner, err := buildWithdrawalsSigner(deps)
	if err != nil {
		return fmt.Errorf("withdrawals: signer: %w", err)
	}
	if closeSigner != nil {
		defer closeSigner()
	}

	seqMgr := sequences.NewManager(deps.Store, deps.Client,
		sequences.WithLogger(log), sequences.WithMetrics(deps.Metrics))

	cfg, err := withdrawalsWorkflowConfig(deps)
	if err != nil {
		return fmt.Errorf("withdrawals: config: %w", err)
	}
	wf, err := withdrawals.New(deps.Store, deps.Client, seqMgr, sg, cfg,
		withdrawals.WithLogger(log), withdrawals.WithMetrics(deps.Metrics))
	if err != nil {
		return err
	}

	// Startup reconciliation (FR-034): every hot wallet and every source
	// with in-flight work is re-derived from chain truth before any new
	// sequence is handed out. Discovery must be COMPLETE (all in-flight
	// sources) and reconciliation must be TOTAL (every source succeeds) —
	// otherwise a source could receive a new sequence before its chain truth
	// was verified. Both are therefore fatal to startup, not fail-open.
	sources, err := withdrawalsSourceAddresses(ctx, deps)
	if err != nil {
		return fmt.Errorf("withdrawals: startup source discovery: %w", err)
	}
	for _, source := range sources {
		report, err := seqMgr.ReconcileAccount(ctx, deps.Manifest.ChainID, source)
		if err != nil {
			// A reconcile failure blocks new-sequence issuance for this
			// source. The withdrawals.New API cannot consult a per-account
			// blocked set from this file, so the least-invasive correct
			// option is to refuse to start the worker at all (FR-034).
			return fmt.Errorf("withdrawals: startup reconciliation for %s: %w", source, err)
		}
		if len(report.Actions) > 0 {
			log.Info("startup sequence reconciliation",
				logging.FieldAddress, source,
				"consumed", report.Consumed, "released", report.Released,
				"quarantined", report.Quarantined)
		}
	}

	ticker := time.NewTicker(withdrawalsDrivePeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			driveWithdrawals(ctx, deps, wf, log)
		}
	}
}

// driveWithdrawals advances every actionable withdrawal one step per pass.
// COMPLIANCE approval is externally supplied (admin API) — ADDRESS_VALIDATED
// records are intentionally not driven here.
func driveWithdrawals(ctx context.Context, deps *Deps, wf *withdrawals.Workflow, log interface {
	Warn(msg string, args ...any)
}) {
	chainID := deps.Manifest.ChainID
	steps := []struct {
		status storage.WithdrawalStatus
		step   func(context.Context, string) error
	}{
		{storage.WithdrawalRequested, wf.ValidateAddress},
		{storage.WithdrawalComplianceApproved, wf.ReserveFunds},
		{storage.WithdrawalFundsReserved, wf.ReserveSequence},
		{storage.WithdrawalSequenceReserved, wf.Build},
		{storage.WithdrawalTransactionBuilt, wf.Simulate},
		{storage.WithdrawalTransactionSimulated, wf.Sign},
		{storage.WithdrawalSigned, func(ctx context.Context, id string) error {
			_, err := wf.Broadcast(ctx, id)
			return err
		}},
		{storage.WithdrawalBroadcast, func(ctx context.Context, id string) error {
			_, err := wf.Confirm(ctx, id)
			return err
		}},
		{storage.WithdrawalIncluded, func(ctx context.Context, id string) error {
			_, err := wf.Confirm(ctx, id)
			return err
		}},
	}
	for _, s := range steps {
		if ctx.Err() != nil {
			return
		}
		recs, err := deps.Store.Withdrawals().ListByStatus(ctx, chainID, s.status, 50)
		if err != nil {
			log.Warn("withdrawal list failed", "status", string(s.status), "error", err.Error())
			continue
		}
		for _, rec := range recs {
			if err := s.step(ctx, rec.WithdrawalID); err != nil {
				// Expected holds (pause, queue policies, quarantine already
				// recorded) surface at debug level in the workflow itself.
				if isWithdrawalHold(err) {
					continue
				}
				log.Warn("withdrawal step failed",
					logging.FieldWithdrawalID, rec.WithdrawalID,
					"status", string(s.status), "error", err.Error())
			}
		}
	}
}

// isWithdrawalHold reports errors that mean "try again later by design".
func isWithdrawalHold(err error) bool {
	return errors.Is(err, withdrawals.ErrPaused) ||
		errors.Is(err, withdrawals.ErrAwaitingCompliance) ||
		errors.Is(err, withdrawals.ErrSimulationUnavailable) ||
		errors.Is(err, withdrawals.ErrSignerUnavailable) ||
		errors.Is(err, withdrawals.ErrQuarantined) ||
		errors.Is(err, storage.ErrStatusConflict)
}

// withdrawalsWorkflowConfig maps adapter.yaml + manifest values into the
// workflow config; every economic value stays configuration (FR-040).
func withdrawalsWorkflowConfig(deps *Deps) (withdrawals.Config, error) {
	confirmations, _, _, err := deps.Config.ScannerRuntime()
	if err != nil {
		return withdrawals.Config{}, err
	}
	broadcastTimeout := 15 * time.Second
	if v := deps.Config.Withdrawals.BroadcastTimeout; v != "" {
		if broadcastTimeout, err = time.ParseDuration(v); err != nil {
			return withdrawals.Config{}, fmt.Errorf("broadcast_timeout: %w", err)
		}
	}
	policy := withdrawals.SimulateQueue
	if deps.Config.Withdrawals.SimulateUnavailable == "static" {
		policy = withdrawals.SimulateStatic
	}
	staticGas := uint64(120000)
	if v := os.Getenv("SOVREN_WITHDRAWALS_STATIC_GAS"); v != "" {
		if staticGas, err = strconv.ParseUint(v, 10, 64); err != nil {
			return withdrawals.Config{}, fmt.Errorf("SOVREN_WITHDRAWALS_STATIC_GAS: %w", err)
		}
	}
	gasPrice, err := withdrawalsGasPrice(deps.Manifest.Fees.RecommendedGasPrice, deps.Manifest.BaseDenom)
	if err != nil {
		return withdrawals.Config{}, err
	}
	gasAdjustment, err := resolveGasAdjustment(deps.Config.Withdrawals.GasAdjustment, deps.Manifest.Fees.RecommendedGasAdjustment)
	if err != nil {
		return withdrawals.Config{}, err
	}
	prohibited, err := resolveProhibitedDestinations(deps.Config.Withdrawals.ProhibitedDestinations)
	if err != nil {
		return withdrawals.Config{}, err
	}
	return withdrawals.Config{
		ChainID:                deps.Manifest.ChainID,
		MinimumWithdrawalUsovr: deps.Config.Withdrawals.MinimumWithdrawalUsovr,
		MaxFeeUsovr:            deps.Config.Withdrawals.MaxFeeUsovr,
		GasAdjustment:          gasAdjustment,
		GasPriceUsovr:          gasPrice,
		SimulateUnavailable:    policy,
		StaticGasLimit:         staticGas,
		BroadcastTimeout:       broadcastTimeout,
		Confirmations:          confirmations,
		ProhibitedDestinations: prohibited,
	}, nil
}

// resolveProhibitedDestinations merges the default core-module-account set
// (always enforced) with the exchange-configured blocklist. Each configured
// entry is validated and normalized so the workflow's strict check — which
// compares the NORMALIZED destination — matches regardless of input casing.
// A withdrawal destination in the combined set is rejected (FR-032 item 1).
func resolveProhibitedDestinations(configured []string) ([]string, error) {
	set := address.DefaultProhibitedModuleAccounts()
	for _, d := range configured {
		res := address.ValidateAccountAddress(d)
		if !res.Valid {
			return nil, fmt.Errorf("withdrawals.prohibited_destinations: %q is not a valid sovr account address (%s)", d, res.ErrorCode)
		}
		set[res.NormalizedAddress] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

// withdrawalsGasPrice strips the denom suffix from a manifest gas-price
// value like "0.025usovr".
func withdrawalsGasPrice(v, denom string) (string, error) {
	v = strings.TrimSuffix(strings.TrimSpace(v), denom)
	if v == "" {
		return "", fmt.Errorf("network manifest has no recommended_gas_price")
	}
	return v, nil
}

// sourceReconcilePageSize is the batch size for the source-discovery scans.
// The repo ListByStatus API bounds by limit only (no offset cursor), so
// completeness comes from growing the limit until a batch returns fewer
// than requested (see paginateAll) — never from a fixed cap that would
// silently drop in-flight sources past the first page.
const sourceReconcilePageSize = 500

// withdrawalsSourceAddresses collects hot wallets plus every source with
// in-flight work for startup reconciliation. Store errors PROPAGATE (a
// transient DB error must not present as an empty, "already reconciled" set)
// and every status queue is paged to completion.
func withdrawalsSourceAddresses(ctx context.Context, deps *Deps) ([]string, error) {
	chainID := deps.Manifest.ChainID
	seen := map[string]bool{}
	var out []string
	add := func(addr string) {
		if addr != "" && !seen[addr] {
			seen[addr] = true
			out = append(out, addr)
		}
	}
	watched, err := deps.Store.Watch().ListActive(ctx, chainID)
	if err != nil {
		return nil, fmt.Errorf("list active watched addresses: %w", err)
	}
	for _, w := range watched {
		if w.Kind == storage.WatchHotWallet {
			add(w.Address)
		}
	}
	for _, status := range []storage.WithdrawalStatus{
		storage.WithdrawalSequenceReserved, storage.WithdrawalTransactionBuilt,
		storage.WithdrawalTransactionSimulated, storage.WithdrawalSigned,
		storage.WithdrawalBroadcast, storage.WithdrawalReviewRequired,
	} {
		status := status
		err := paginateAll(func(limit int) (int, error) {
			recs, err := deps.Store.Withdrawals().ListByStatus(ctx, chainID, status, limit)
			if err != nil {
				return 0, fmt.Errorf("list withdrawals in status %s: %w", status, err)
			}
			for _, r := range recs {
				add(r.SourceAddress)
			}
			return len(recs), nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// paginateAll drains a limit-only ListByStatus repo to completion. It calls
// fetch with a growing limit and stops once a batch returns fewer records
// than requested, which proves the queue is exhausted. Growing the limit
// (rather than an offset cursor) is required because the repo API exposes no
// offset; the caller deduplicates, so re-observing earlier rows is harmless.
func paginateAll(fetch func(limit int) (count int, err error)) error {
	for limit := sourceReconcilePageSize; ; limit += sourceReconcilePageSize {
		n, err := fetch(limit)
		if err != nil {
			return err
		}
		if n < limit {
			return nil
		}
	}
}

// resolveGasAdjustment picks the effective gas adjustment: the adapter
// config value when set, else the network manifest's
// recommended_gas_adjustment. An explicit value is REQUIRED — an empty
// config with an empty manifest recommendation is a configuration error,
// never a silent hardcoded fallback.
func resolveGasAdjustment(configured, manifestRecommended string) (string, error) {
	if v := strings.TrimSpace(configured); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(manifestRecommended); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("gas_adjustment: unset in adapter config and network manifest has no recommended_gas_adjustment; set withdrawals.gas_adjustment explicitly")
}

// buildWithdrawalsSigner wires the configured signer kind. The returned
// close function releases transport resources (nil when not needed).
func buildWithdrawalsSigner(deps *Deps) (signer.TransactionSigner, func(), error) {
	kind := deps.Config.Signer.Kind
	endpoint := deps.Config.Signer.Endpoint
	networkType := deps.Manifest.NetworkType

	switch kind {
	case "grpc-remote":
		if endpoint == "" {
			return nil, nil, fmt.Errorf("signer.endpoint required for grpc-remote")
		}
		cfg := grpcremote.Config{Target: endpoint}
		caFile := os.Getenv("SOVREN_SIGNER_TLS_CA_FILE")
		certFile := os.Getenv("SOVREN_SIGNER_TLS_CERT_FILE")
		keyFile := os.Getenv("SOVREN_SIGNER_TLS_KEY_FILE")
		if caFile != "" || certFile != "" || keyFile != "" {
			cfg.TLS = &grpcremote.TLSConfig{
				CAFile:     caFile,
				CertFile:   certFile,
				KeyFile:    keyFile,
				ServerName: os.Getenv("SOVREN_SIGNER_TLS_SERVER_NAME"),
			}
		} else if os.Getenv("SOVREN_SIGNER_ALLOW_INSECURE_DEV") == "true" {
			if strings.EqualFold(networkType, "mainnet") {
				return nil, nil, fmt.Errorf("SOVREN_SIGNER_ALLOW_INSECURE_DEV is refused on mainnet: configure mTLS")
			}
			cfg.AllowInsecureDev = true
		}
		c, err := grpcremote.New(cfg)
		if err != nil {
			return nil, nil, err
		}
		return c, func() { _ = c.Close() }, nil

	case "exec":
		if endpoint == "" {
			return nil, nil, fmt.Errorf("signer.endpoint (binary path) required for exec")
		}
		s, err := execsigner.New(execsigner.Config{Path: endpoint})
		if err != nil {
			return nil, nil, err
		}
		return s, nil, nil

	case "unsafe-local":
		if os.Getenv("SOVREN_SIGNER_UNSAFE") != "UNSAFE_TEST_ONLY" {
			return nil, nil, fmt.Errorf("unsafe-local requires SOVREN_SIGNER_UNSAFE=UNSAFE_TEST_ONLY")
		}
		mnemonic := os.Getenv("SOVREN_SIGNER_MNEMONIC")
		if mnemonic == "" {
			return nil, nil, fmt.Errorf("unsafe-local requires SOVREN_SIGNER_MNEMONIC")
		}
		s, err := local.New(local.Options{UnsafeTestOnly: true, NetworkType: networkType})
		if err != nil {
			return nil, nil, err
		}
		hdPath := os.Getenv("SOVREN_SIGNER_HD_PATH")
		if hdPath == "" {
			hdPath = address.DefaultHDPath
		}
		derived, err := address.DeriveAddress(mnemonic, hdPath)
		if err != nil {
			return nil, nil, err
		}
		if err := s.ImportKey(derived.Bech32, derived.PrivateKey); err != nil {
			return nil, nil, err
		}
		return s, nil, nil

	case "":
		return nil, nil, fmt.Errorf("signer.kind required for the withdrawals service (grpc-remote | exec | unsafe-local)")
	default:
		return nil, nil, fmt.Errorf("unknown signer.kind %q", kind)
	}
}
