package client

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// validManifestYAML mirrors the contract example (network-manifest.md) with
// concrete values everywhere the contract shows ellipses.
const validManifestYAML = `schema_version: 1
network_name: Sovren Mainnet
network_type: mainnet
chain_id: sovr-1
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
  app: v0.19.0
fees:
  minimum_gas_price: "0.001usovr"
  recommended_gas_price: "0.025usovr"
  recommended_gas_adjustment: "1.3"
ports: { p2p: 26656, rpc: 26657, grpc: 9090, rest: 1317, metrics: 26660 }
genesis:
  sha256: 04529695e7ccfe32fcf3bc8031c343056d27cbe4aa3b3046027e27065bb9a855
  file: genesis.json
endpoints:
  - { kind: rpc,  url: "https://rpc.sovrchain.net", provider: sovren }
  - { kind: rest, url: "https://api.sovrchain.net", provider: sovren }
peers:
  seeds:
    - "381af5e4d0d5e24af3bb4d506b06081399622d01@seed1.mainnet.sovrchain.net:32000"
    - "68d9058a5dd7062d72d917ae8f2a2e30101b5a74@seed2.mainnet.sovrchain.net:32001"
  persistent_peers:
    - "68d9058a5dd7062d72d917ae8f2a2e30101b5a75@sentry1.mainnet.sovrchain.net:26656"
links:
  explorer: "https://sovrscan.com"
  faucet: null
  status_page: null
  registry_dir: registry/
divergences: []
`

func writeManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "network.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestLoadManifestValid(t *testing.T) {
	m, err := LoadManifest(writeManifest(t, validManifestYAML))
	require.NoError(t, err)

	require.Equal(t, 1, m.SchemaVersion)
	require.Equal(t, "sovr-1", m.ChainID)
	require.Equal(t, "mainnet", m.NetworkType)
	require.Equal(t, "usovr", m.BaseDenom)
	require.Equal(t, 6, m.DisplayExponent)
	require.Equal(t, 118, m.Slip44CoinType)
	require.Equal(t, "v0.53.6", m.Versions.SDK)
	require.Equal(t, "v0.19.0", m.Versions.App)
	require.Equal(t, "0.001usovr", m.Fees.MinimumGasPrice)
	require.Equal(t, 9090, m.Ports.GRPC)
	require.Equal(t, 26657, m.Ports.RPC)
	require.Equal(t, "04529695e7ccfe32fcf3bc8031c343056d27cbe4aa3b3046027e27065bb9a855", m.Genesis.SHA256)
	require.Len(t, m.Endpoints, 2)
	require.Equal(t, "rpc", m.Endpoints[0].Kind)
	require.Equal(t, "https://rpc.sovrchain.net", m.Endpoints[0].URL)
	require.Len(t, m.Peers.Seeds, 2)
	require.Nil(t, m.Links.Faucet)
	require.Equal(t, "https://sovrscan.com", m.Links.Explorer)
	require.Empty(t, m.Divergences)
}

func TestLoadManifestRejections(t *testing.T) {
	mutate := func(old, new string) string {
		require.Contains(t, validManifestYAML, old)
		return strings.Replace(validManifestYAML, old, new, 1)
	}
	tests := []struct {
		name    string
		content string
		wantErr error
	}{
		{
			name:    "unknown schema major",
			content: mutate("schema_version: 1", "schema_version: 2"),
			wantErr: ErrManifestSchemaVersion,
		},
		{
			name:    "schema version zero",
			content: mutate("schema_version: 1", "schema_version: 0"),
			wantErr: ErrManifestSchemaVersion,
		},
		{
			name:    "missing schema version",
			content: mutate("schema_version: 1\n", ""),
			wantErr: ErrManifestMissingField,
		},
		{
			name:    "placeholder TBD chain_id",
			content: mutate("chain_id: sovr-1", "chain_id: TBD"),
			wantErr: ErrManifestPlaceholder,
		},
		{
			name:    "placeholder REPLACE_ genesis sha",
			content: mutate("sha256: 04529695e7ccfe32fcf3bc8031c343056d27cbe4aa3b3046027e27065bb9a855", "sha256: REPLACE_GENESIS_SHA256"),
			wantErr: ErrManifestPlaceholder,
		},
		{
			name:    "placeholder lowercase tbd endpoint url",
			content: mutate(`url: "https://rpc.sovrchain.net"`, `url: "tbd"`),
			wantErr: ErrManifestPlaceholder,
		},
		{
			name:    "empty required chain_id",
			content: mutate("chain_id: sovr-1", `chain_id: ""`),
			wantErr: ErrManifestMissingField,
		},
		{
			name: "missing seeds",
			content: mutate(`  seeds:
    - "381af5e4d0d5e24af3bb4d506b06081399622d01@seed1.mainnet.sovrchain.net:32000"
    - "68d9058a5dd7062d72d917ae8f2a2e30101b5a74@seed2.mainnet.sovrchain.net:32001"
`, "  seeds: []\n"),
			wantErr: ErrManifestMissingField,
		},
		{
			name:    "unknown top-level key",
			content: validManifestYAML + "surprise_field: 1\n",
			wantErr: ErrManifestInvalid,
		},
		{
			name:    "bad network_type",
			content: mutate("network_type: mainnet", "network_type: devnet"),
			wantErr: ErrManifestInvalid,
		},
		{
			name:    "bad genesis sha length",
			content: mutate("sha256: 04529695e7ccfe32fcf3bc8031c343056d27cbe4aa3b3046027e27065bb9a855", "sha256: 0452"),
			wantErr: ErrManifestInvalid,
		},
		{
			name:    "bad seed format",
			content: mutate("381af5e4d0d5e24af3bb4d506b06081399622d01@seed1.mainnet.sovrchain.net:32000", "seed1.mainnet.sovrchain.net:32000"),
			wantErr: ErrManifestInvalid,
		},
		{
			name:    "divergences on mainnet",
			content: mutate("divergences: []", `divergences: ["voting_period accelerated"]`),
			wantErr: ErrManifestInvalid,
		},
		{
			name:    "bad endpoint kind",
			content: mutate("kind: rest,", "kind: graphql,"),
			wantErr: ErrManifestInvalid,
		},
		{
			name:    "bad minimum gas price",
			content: mutate(`minimum_gas_price: "0.001usovr"`, `minimum_gas_price: "cheap"`),
			wantErr: ErrManifestInvalid,
		},
		{
			name:    "zero port",
			content: mutate("grpc: 9090,", "grpc: 0,"),
			wantErr: ErrManifestMissingField,
		},
		{
			name:    "not yaml",
			content: "{{{{",
			wantErr: ErrManifestInvalid,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseManifest([]byte(tc.content))
			require.Nil(t, m)
			require.Error(t, err)
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestLoadManifestTypedFieldErrors(t *testing.T) {
	content := strings.Replace(validManifestYAML, "chain_id: sovr-1", "chain_id: TBD", 1)
	_, err := ParseManifest([]byte(content))
	require.Error(t, err)
	var me *ManifestError
	require.True(t, errors.As(err, &me))
	require.Equal(t, "chain_id", me.Path)
	require.ErrorIs(t, me, ErrManifestPlaceholder)
}

func TestLoadManifestTestnetDivergencesAllowed(t *testing.T) {
	content := strings.Replace(validManifestYAML, "network_type: mainnet", "network_type: testnet", 1)
	content = strings.Replace(content, "divergences: []", `divergences: ["voting_period accelerated for testing"]`, 1)
	m, err := ParseManifest([]byte(content))
	require.NoError(t, err)
	require.Equal(t, []string{"voting_period accelerated for testing"}, m.Divergences)
}

func TestLoadManifestFileMissing(t *testing.T) {
	_, err := LoadManifest(filepath.Join(t.TempDir(), "absent.yaml"))
	require.Error(t, err)
}
