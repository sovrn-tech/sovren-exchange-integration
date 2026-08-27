package main

// Deposit scanner service (T045 — owned by the deposits track). Registered
// via init(); other services register from their own files, never this one.

import (
	"context"
	"fmt"

	sdkmath "cosmossdk.io/math"

	"github.com/sovrn-tech/sovren-exchange-integration/go/deposits"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
)

func init() {
	register("scanner", runScanner)
}

func runScanner(ctx context.Context, deps *Deps) error {
	confirmations, startHeight, poll, err := deps.Config.ScannerRuntime()
	if err != nil {
		return err
	}
	var minDeposit sdkmath.Int
	if v := deps.Config.Scanner.MinimumDepositUsovr; v != "" {
		n, ok := sdkmath.NewIntFromString(v)
		if !ok {
			return fmt.Errorf("scanner.minimum_deposit_usovr: invalid integer %q", v)
		}
		minDeposit = n
	}
	if deps.Config.Scanner.WebsocketAccelerator != nil && *deps.Config.Scanner.WebsocketAccelerator {
		// The kit client is unary-only; poll_interval is the
		// accelerator-equivalent (documented deviation — see
		// docs/deposit-processing.md). Scanning correctness is unaffected:
		// polling is the mandated primary mechanism (FR-027).
		deps.Logger.Info("websocket_accelerator requested; using poll-interval acceleration instead",
			logging.FieldService, "scanner")
	}

	scanner, err := deposits.NewScanner(deps.Client, deps.Store, deposits.ScannerConfig{
		ChainID:             deps.Manifest.ChainID,
		Confirmations:       confirmations,
		StartHeight:         startHeight,
		PollInterval:        poll,
		MinimumDepositUsovr: minDeposit,
		Logger:              deps.Logger,
		Metrics:             deps.Metrics,
	})
	if err != nil {
		return err
	}
	return scanner.Run(ctx)
}
