// Package logging provides the kit's structured JSON logging base (FR-050).
//
// Every log line carries a correlation ID plus the standard chain-context
// fields; secret material (private keys, mnemonics, sign-doc bytes) must never
// be logged — TestNoSecretFields enforces the field-name blocklist.
package logging

import (
	"context"
	"log/slog"
	"os"
)

// Standard field keys (FR-050). Use these constants — ad-hoc keys defeat log
// aggregation and the secret-field lint.
const (
	FieldChainID       = "chain_id"
	FieldHeight        = "height"
	FieldTxHash        = "tx_hash"
	FieldAddress       = "address"
	FieldWithdrawalID  = "withdrawal_id"
	FieldDepositID     = "deposit_id"
	FieldSweepID       = "sweep_id"
	FieldSequence      = "sequence"
	FieldErrorCode     = "error_code"
	FieldNodeEndpoint  = "node_endpoint"
	FieldCorrelationID = "correlation_id"
	FieldService       = "service"
)

// ForbiddenFieldSubstrings are field-name fragments that must never appear in a
// log call: they indicate secret material. internal/logging tests and the
// export-pipeline sanitization scan both consume this list.
var ForbiddenFieldSubstrings = []string{
	"private_key", "privkey", "mnemonic", "seed_phrase", "sign_doc_bytes", "signature_secret",
}

type ctxKey struct{}

// New returns the kit's root JSON logger for a named service.
func New(service string) *slog.Logger {
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(h).With(FieldService, service)
}

// WithCorrelation returns a context carrying a correlation ID and a logger
// bound to it. All work items (blocks, withdrawals, sweeps) get one at entry.
func WithCorrelation(ctx context.Context, logger *slog.Logger, correlationID string) (context.Context, *slog.Logger) {
	l := logger.With(FieldCorrelationID, correlationID)
	return context.WithValue(ctx, ctxKey{}, l), l
}

// From extracts the correlation-bound logger from ctx, falling back to a
// service-less root logger.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return New("unknown")
}
