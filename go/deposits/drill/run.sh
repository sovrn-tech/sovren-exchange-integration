#!/usr/bin/env bash
# Deposit-pipeline integration drill (T049) against a local compose chain.
#
# Runs the env-gated tests in exchange-kit/go/deposits/integration_test.go:
#   - end-to-end fund → scan → credit
#   - scanner kill/restart mid-range (no loss, no duplication — SC-004)
#   - range replay idempotency (FR-024/FR-026)
#   - database outage + recovery
#
# Usage:
#   SOVREN_LOCAL_CHAIN_MNEMONIC="word1 ... word24" ./run.sh [rpc-url]
#
#   rpc-url                       defaults to http://localhost:26657
#   SOVREN_LOCAL_CHAIN_MNEMONIC   mnemonic of a funded account at
#                                 m/44'/118'/0'/0/0 (the drill sends several
#                                 small transfers plus fees from it)
#
# Typical local setup (from the repository root):
#   docker compose up -d                # start the local dev chain
#   # fund the drill account, then:
#   cd exchange-kit/go/deposits/drill && SOVREN_LOCAL_CHAIN_MNEMONIC=... ./run.sh
#
# The drill needs only the CometBFT RPC endpoint; REST/gRPC are not used by
# the scanner. Never point this at mainnet: it broadcasts transactions.

set -euo pipefail

RPC_URL="${1:-${SOVREN_LOCAL_CHAIN_RPC:-http://localhost:26657}}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GO_MODULE_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"

if [[ -z "${SOVREN_LOCAL_CHAIN_MNEMONIC:-}" ]]; then
  echo "error: SOVREN_LOCAL_CHAIN_MNEMONIC must be set to a funded local-chain mnemonic" >&2
  exit 1
fi

echo "==> probing chain at ${RPC_URL}"
if ! curl -sf --max-time 5 "${RPC_URL}/status" >/dev/null; then
  echo "error: no CometBFT RPC at ${RPC_URL} — is the local chain up? (docker compose up -d)" >&2
  exit 1
fi

CHAIN_ID="$(curl -sf --max-time 5 "${RPC_URL}/status" | sed -n 's/.*"network"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
echo "==> chain_id: ${CHAIN_ID:-unknown}"
case "${CHAIN_ID}" in
  sovr-1)
    echo "error: refusing to run the drill against mainnet (sovr-1)" >&2
    exit 1
    ;;
esac

echo "==> running deposit integration drills"
cd "${GO_MODULE_DIR}"
SOVREN_LOCAL_CHAIN_RPC="${RPC_URL}" \
SOVREN_LOCAL_CHAIN_MNEMONIC="${SOVREN_LOCAL_CHAIN_MNEMONIC}" \
GOWORK=off CGO_ENABLED=0 \
  go test ./deposits/ -run 'TestIntegration' -count=1 -v -timeout 10m

echo "==> deposit drill complete"
