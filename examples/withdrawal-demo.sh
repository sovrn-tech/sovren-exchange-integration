#!/usr/bin/env bash
# withdrawal-demo.sh — end-to-end withdrawal demonstration against a LOCAL
# dev chain: build -> sign (unsafe-local) -> broadcast -> confirm, plus the
# duplicate-submit and concurrent-20 drills (T058).
#
# This wraps the kit's env-gated live-chain drills (go/withdrawals) so the
# whole flow runs with one command once a local chain is up:
#
#   export SOVREN_LOCAL_CHAIN_RPC=http://127.0.0.1:26657
#   export SOVREN_DRILL_MNEMONIC="<funded test mnemonic>"   # UNSAFE_TEST_ONLY
#   ./examples/withdrawal-demo.sh [lifecycle|duplicate|concurrent|all]
#
# Getting a funded test key on a local dev chain:
#   go run ./go/cmd/sovren-vectors derive --new-test-address
# then fund the printed address from the dev chain's genesis/faucet account
# (at least 25 SOVR = 25000000usovr for the concurrent drill), and export the
# printed mnemonic as SOVREN_DRILL_MNEMONIC.
#
# The unsafe-local signer never runs against mainnet; this demo is for local
# and test networks only.
#
# Convention: explicit error handling, no `set -euo pipefail`.

KIT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODE="${1:-all}"

fail() { echo "withdrawal-demo: FAIL: $1" >&2; exit 1; }

[ -n "$SOVREN_LOCAL_CHAIN_RPC" ] || fail "SOVREN_LOCAL_CHAIN_RPC is not set (start a local chain first)"
[ -n "$SOVREN_DRILL_MNEMONIC" ] || fail "SOVREN_DRILL_MNEMONIC is not set (see header for how to mint + fund one)"

case "$MODE" in
  lifecycle)  RUN='TestDrillWithdrawalLifecycle' ;;
  duplicate)  RUN='TestDrillDuplicateSubmit' ;;
  concurrent) RUN='TestDrillConcurrent20' ;;
  all)        RUN='TestDrill' ;;
  *)          fail "unknown mode $MODE (lifecycle|duplicate|concurrent|all)" ;;
esac

echo "==> withdrawal demo ($MODE) against $SOVREN_LOCAL_CHAIN_RPC"
if ! (cd "$KIT_ROOT/go" && GOWORK=off CGO_ENABLED=0 go test ./withdrawals/ -run "$RUN" -v -count=1 -timeout 20m); then
  fail "demo run failed"
fi
echo "withdrawal-demo: PASS"
