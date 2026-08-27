package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testManifestTemplate = `schema_version: 1
network_name: Sovren Local Dev
network_type: %NETWORK_TYPE%
chain_id: sovr-local-dev
daemon_name: sovrd
node_home: $HOME/.sovr
account_prefix: sovr
validator_operator_prefix: sovrvaloper
validator_consensus_prefix: sovrvalcons
base_denom: usovr
display_denom: SOVR
display_exponent: 6
symbol: SOVR
account_key_algorithm: secp256k1
consensus_key_algorithm: ed25519
slip44_coin_type: 118
versions:
  sdk: v0.53.6
  cometbft: v0.38.21
  ibc_go: v10.4.0
  cosmwasm_wasmd: v0.60.1
  go: "1.25.7"
  app: v0.16.2
fees:
  minimum_gas_price: "0.001usovr"
  recommended_gas_price: "0.025usovr"
  recommended_gas_adjustment: "1.3"
ports: { p2p: 26656, rpc: 26657, grpc: 9090, rest: 1317, metrics: 26660 }
genesis:
  sha256: 0000000000000000000000000000000000000000000000000000000000000000
  file: genesis.json
endpoints:
  - { kind: rpc, url: "http://localhost:26657", provider: local }
peers:
  seeds:
    - "0000000000000000000000000000000000000000@localhost:26656"
links:
  explorer: "http://localhost:26657/status"
  registry_dir: sovrlocaldev
%DIVERGENCES%`

const testAdapterConfig = `schema_version: 1
network_manifest: %MANIFEST%
nodes:
  primary: { rpc: "http://localhost:26657" }
storage:
  backend: sqlite
  dsn: "%DSN%"
scanner:
  confirmations: 2
  poll_interval: 250ms
  minimum_deposit_usovr: "1000"
signer:
  kind: %SIGNER_KIND%
admin:
  listen: "127.0.0.1:9465"
metrics:
  listen: ":9464"
`

func writeTestManifest(t *testing.T, dir, networkType string) string {
	t.Helper()
	m := strings.ReplaceAll(testManifestTemplate, "%NETWORK_TYPE%", networkType)
	div := "divergences:\n  - \"local development chain\"\n"
	if networkType == "mainnet" {
		div = ""
	}
	m = strings.ReplaceAll(m, "%DIVERGENCES%", div)
	path := filepath.Join(dir, "network."+networkType+".yaml")
	require.NoError(t, os.WriteFile(path, []byte(m), 0o644))
	return path
}

func writeTestConfig(t *testing.T, dir, manifestPath, dsn, signerKind string) string {
	t.Helper()
	c := strings.ReplaceAll(testAdapterConfig, "%MANIFEST%", manifestPath)
	c = strings.ReplaceAll(c, "%DSN%", dsn)
	c = strings.ReplaceAll(c, "%SIGNER_KIND%", signerKind)
	path := filepath.Join(dir, "adapter.yaml")
	require.NoError(t, os.WriteFile(path, []byte(c), 0o644))
	return path
}

func TestLoadConfigValid(t *testing.T) {
	dir := t.TempDir()
	manifest := writeTestManifest(t, dir, "testnet")
	path := writeTestConfig(t, dir, manifest, "file:test.db", "unsafe-local")

	cfg, m, err := LoadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "sovr-local-dev", m.ChainID)
	require.Equal(t, "sqlite", cfg.Storage.Backend)

	confirmations, start, poll, err := cfg.ScannerRuntime()
	require.NoError(t, err)
	require.Equal(t, uint64(2), confirmations)
	require.Equal(t, uint64(0), start)
	require.Equal(t, 250*time.Millisecond, poll)
}

func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	manifest := writeTestManifest(t, dir, "testnet")
	base := strings.ReplaceAll(testAdapterConfig, "%MANIFEST%", manifest)
	base = strings.ReplaceAll(base, "%DSN%", "file:test.db")
	base = strings.ReplaceAll(base, "%SIGNER_KIND%", "unsafe-local")
	bad := base + "surprise_key: true\n"
	path := filepath.Join(dir, "adapter.yaml")
	require.NoError(t, os.WriteFile(path, []byte(bad), 0o644))

	_, _, err := LoadConfig(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "surprise_key")
}

func TestLoadConfigDSNEnvExpansion(t *testing.T) {
	dir := t.TempDir()
	manifest := writeTestManifest(t, dir, "testnet")
	t.Setenv("SOVREN_TEST_DB_SECRET", "s3cret")
	path := writeTestConfig(t, dir, manifest, "postgres://adapter:${SOVREN_TEST_DB_SECRET}@localhost/adapter", "unsafe-local")

	cfg, _, err := LoadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "postgres://adapter:s3cret@localhost/adapter", cfg.Storage.DSN)

	// Unset reference fails fast — never a silently empty secret.
	path2 := writeTestConfig(t, dir, manifest, "postgres://adapter:${SOVREN_TEST_DB_MISSING}@localhost/adapter", "unsafe-local")
	_, _, err = LoadConfig(path2)
	require.Error(t, err)
	require.Contains(t, err.Error(), "SOVREN_TEST_DB_MISSING")
}

func TestLoadConfigRefusesUnsafeLocalSignerOnMainnet(t *testing.T) {
	dir := t.TempDir()
	manifest := writeTestManifest(t, dir, "mainnet")
	path := writeTestConfig(t, dir, manifest, "file:test.db", "unsafe-local")

	_, _, err := LoadConfig(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsafe-local")
	require.Contains(t, err.Error(), "mainnet")

	// The same manifest with a production signer kind is fine.
	path2 := writeTestConfig(t, dir, manifest, "file:test.db", "grpc-remote")
	_, m, err := LoadConfig(path2)
	require.NoError(t, err)
	require.Equal(t, "mainnet", m.NetworkType)
}

func TestLoadConfigValidationErrors(t *testing.T) {
	dir := t.TempDir()
	manifest := writeTestManifest(t, dir, "testnet")
	for _, tc := range []struct {
		name, mutate, want string
	}{
		{name: "bad backend", mutate: "backend: sqlite", want: "storage.backend"},
		{name: "bad integer", mutate: `minimum_deposit_usovr: "1000"`, want: "minimum_deposit_usovr"},
		{name: "bad duration", mutate: "poll_interval: 250ms", want: "poll_interval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := strings.ReplaceAll(testAdapterConfig, "%MANIFEST%", manifest)
			c = strings.ReplaceAll(c, "%DSN%", "file:test.db")
			c = strings.ReplaceAll(c, "%SIGNER_KIND%", "unsafe-local")
			switch tc.name {
			case "bad backend":
				c = strings.ReplaceAll(c, tc.mutate, "backend: mongodb")
			case "bad integer":
				c = strings.ReplaceAll(c, tc.mutate, `minimum_deposit_usovr: "10.5"`)
			case "bad duration":
				c = strings.ReplaceAll(c, tc.mutate, "poll_interval: sometimes")
			}
			path := filepath.Join(dir, "adapter-bad.yaml")
			require.NoError(t, os.WriteFile(path, []byte(c), 0o644))
			_, _, err := LoadConfig(path)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestExampleAdapterLocalConfigLoads(t *testing.T) {
	cfg, m, err := LoadConfig("../../../examples/adapter.local.yaml")
	require.NoError(t, err)
	require.Equal(t, "sovr-local-dev", m.ChainID)
	require.Equal(t, "testnet", m.NetworkType)
	require.Equal(t, "sqlite", cfg.Storage.Backend)
	confirmations, _, poll, err := cfg.ScannerRuntime()
	require.NoError(t, err)
	require.Equal(t, uint64(1), confirmations)
	require.Equal(t, time.Second, poll)
}

func TestServiceRegistryHasScanner(t *testing.T) {
	require.Contains(t, registeredServices(), "scanner")
}
