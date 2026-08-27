#!/usr/bin/env bash
# run.sh — cross-language conformance harness (contracts/test-vectors.md).
#
# Runs the Go and TypeScript kits over every committed vector suite in
# exchange-kit/test-vectors/ and diffs the outputs field-by-field via
# `sovren-vectors compare`. Exits non-zero on any divergence, any uncovered
# vector id, or any expectation mismatch against the vector files themselves.
#
# Prerequisites: Go toolchain; `npm ci` (or install) in exchange-kit/typescript
# (provides tsx). Runtime target: < 1 minute.
#
# Convention: explicit error handling, no `set -euo pipefail`.

KIT_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
VECTORS="$KIT_ROOT/test-vectors"
TSX="$KIT_ROOT/typescript/node_modules/.bin/tsx"

fail() { echo "conformance: FAIL: $1" >&2; exit 1; }

[ -d "$VECTORS" ] || fail "vector directory not found: $VECTORS"
[ -x "$TSX" ] || fail "tsx not found at $TSX (run 'npm ci' in exchange-kit/typescript)"

WORK="$(mktemp -d)" || fail "mktemp failed"
trap 'rm -rf "$WORK"' EXIT

echo "==> [1/4] byte-identity: committed vectors match deterministic regeneration"
if ! (cd "$KIT_ROOT/go" && GOWORK=off CGO_ENABLED=0 go run ./cmd/sovren-vectors verify --dir "$VECTORS"); then
  fail "committed vectors are not byte-identical to regeneration"
fi

echo "==> [2/4] Go runner"
if ! (cd "$KIT_ROOT/go" && GOWORK=off CGO_ENABLED=0 go run ./cmd/sovren-vectors conformance --dir "$VECTORS" --out "$WORK/go-results.json"); then
  fail "Go conformance runner failed"
fi

echo "==> [3/4] TypeScript runner"
if ! "$TSX" "$KIT_ROOT/test/conformance/ts-runner.ts" --dir "$VECTORS" --out "$WORK/ts-results.json"; then
  fail "TypeScript conformance runner failed"
fi

echo "==> [4/4] field-by-field diff + coverage"
if ! (cd "$KIT_ROOT/go" && GOWORK=off CGO_ENABLED=0 go run ./cmd/sovren-vectors compare --dir "$VECTORS" --a "$WORK/go-results.json" --b "$WORK/ts-results.json"); then
  fail "Go and TypeScript outputs diverge"
fi

echo "conformance: PASS"
