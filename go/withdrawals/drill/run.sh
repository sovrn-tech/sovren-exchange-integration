#!/usr/bin/env bash
# run.sh — withdrawal drills against a LOCAL dev chain (T058).
#
# Runs the env-gated live-chain tests in exchange-kit/go/withdrawals:
#   1. lifecycle:  build -> sign (unsafe-local) -> broadcast -> confirm
#   2. duplicate submit: one idempotency key => one on-chain transaction
#   3. concurrent-20: twenty withdrawals, one hot wallet, distinct sequences
#
# Required environment:
#   SOVREN_LOCAL_CHAIN_RPC   e.g. http://127.0.0.1:26657
#   SOVREN_DRILL_MNEMONIC    funded test mnemonic (UNSAFE_TEST_ONLY — never a
#                            production secret; fund it from the local dev
#                            chain's faucet/genesis account)
# Optional:
#   SOVREN_LOCAL_CHAIN_REST / SOVREN_LOCAL_CHAIN_GRPC (other drills)
#   SOVREN_DRILL_GAS_PRICE   default 0.025
#
# Convention: explicit error handling, no `set -euo pipefail`.

KIT_GO="$(cd "$(dirname "$0")/../.." && pwd)"

fail() { echo "withdrawal-drill: FAIL: $1" >&2; exit 1; }

[ -n "$SOVREN_LOCAL_CHAIN_RPC" ] || fail "SOVREN_LOCAL_CHAIN_RPC is not set (local chain required)"
[ -n "$SOVREN_DRILL_MNEMONIC" ] || fail "SOVREN_DRILL_MNEMONIC is not set (funded test key required)"

echo "==> withdrawal drills against $SOVREN_LOCAL_CHAIN_RPC"
if ! (cd "$KIT_GO" && GOWORK=off CGO_ENABLED=0 go test ./withdrawals/ -run 'TestDrill' -v -count=1 -timeout 20m); then
  fail "drill test run failed"
fi
echo "withdrawal-drill: PASS"
