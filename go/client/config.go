package client

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"gopkg.in/yaml.v3"
)

// SupportedManifestSchemaMajor is the network.yaml contract major this loader
// understands (contracts/network-manifest.md); unknown majors are rejected.
const SupportedManifestSchemaMajor = 1

var (
	// ErrManifestSchemaVersion — schema_version has an unknown major.
	ErrManifestSchemaVersion = errors.New("unsupported manifest schema_version")
	// ErrManifestPlaceholder — a value is TBD / REPLACE_* (SC-007).
	ErrManifestPlaceholder = errors.New("placeholder value in manifest")
	// ErrManifestMissingField — a required value is empty or absent.
	ErrManifestMissingField = errors.New("missing required manifest value")
	// ErrManifestInvalid — the file is not parseable as the contract schema.
	ErrManifestInvalid = errors.New("invalid manifest")
)

// ManifestError is a typed per-field validation failure; Unwrap yields one of
// the ErrManifest* sentinels for errors.Is matching.
type ManifestError struct {
	Path   string
	Detail string
	kind   error
}

func (e *ManifestError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("manifest %s: %v", e.Path, e.kind)
	}
	return fmt.Sprintf("manifest %s: %s: %v", e.Path, e.Detail, e.kind)
}

func (e *ManifestError) Unwrap() error { return e.kind }

type ManifestVersions struct {
	SDK           string `yaml:"sdk"`
	CometBFT      string `yaml:"cometbft"`
	IBCGo         string `yaml:"ibc_go"`
	CosmwasmWasmd string `yaml:"cosmwasm_wasmd"`
	Go            string `yaml:"go"`
	App           string `yaml:"app"`
}

type ManifestFees struct {
	MinimumGasPrice          string `yaml:"minimum_gas_price"`
	RecommendedGasPrice      string `yaml:"recommended_gas_price"`
	RecommendedGasAdjustment string `yaml:"recommended_gas_adjustment"`
}

type ManifestPorts struct {
	P2P     int `yaml:"p2p"`
	RPC     int `yaml:"rpc"`
	GRPC    int `yaml:"grpc"`
	REST    int `yaml:"rest"`
	Metrics int `yaml:"metrics"`
}

type ManifestGenesis struct {
	SHA256 string `yaml:"sha256"`
	File   string `yaml:"file"`
}

type ManifestEndpoint struct {
	Kind     string `yaml:"kind"`
	URL      string `yaml:"url"`
	Provider string `yaml:"provider"`
}

type ManifestPeers struct {
	Seeds           []string `yaml:"seeds"`
	PersistentPeers []string `yaml:"persistent_peers"`
}

type ManifestLinks struct {
	Explorer    string  `yaml:"explorer"`
	Faucet      *string `yaml:"faucet"`
	StatusPage  *string `yaml:"status_page"`
	RegistryDir string  `yaml:"registry_dir"`
}

// NetworkManifest is the parsed network/<net>/network.yaml
// (contracts/network-manifest.md, schema_version 1).
type NetworkManifest struct {
	SchemaVersion            int                `yaml:"schema_version"`
	NetworkName              string             `yaml:"network_name"`
	NetworkType              string             `yaml:"network_type"`
	ChainID                  string             `yaml:"chain_id"`
	DaemonName               string             `yaml:"daemon_name"`
	NodeHome                 string             `yaml:"node_home"`
	AccountPrefix            string             `yaml:"account_prefix"`
	ValidatorOperatorPrefix  string             `yaml:"validator_operator_prefix"`
	ValidatorConsensusPrefix string             `yaml:"validator_consensus_prefix"`
	BaseDenom                string             `yaml:"base_denom"`
	DisplayDenom             string             `yaml:"display_denom"`
	DisplayExponent          int                `yaml:"display_exponent"`
	Symbol                   string             `yaml:"symbol"`
	AccountKeyAlgorithm      string             `yaml:"account_key_algorithm"`
	ConsensusKeyAlgorithm    string             `yaml:"consensus_key_algorithm"`
	Slip44CoinType           int                `yaml:"slip44_coin_type"`
	Versions                 ManifestVersions   `yaml:"versions"`
	Fees                     ManifestFees       `yaml:"fees"`
	Ports                    ManifestPorts      `yaml:"ports"`
	Genesis                  ManifestGenesis    `yaml:"genesis"`
	Endpoints                []ManifestEndpoint `yaml:"endpoints"`
	Peers                    ManifestPeers      `yaml:"peers"`
	Links                    ManifestLinks      `yaml:"links"`
	Divergences              []string           `yaml:"divergences"`
}

// LoadManifest reads and validates a network.yaml. Any schema-major mismatch,
// placeholder value, or missing required value fails with a typed error
// (errors.Is against the ErrManifest* sentinels).
func LoadManifest(path string) (*NetworkManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseManifest(data)
}

// ParseManifest is LoadManifest on in-memory bytes.
func ParseManifest(data []byte) (*NetworkManifest, error) {
	// Schema-version gate first, on a tolerant decode: a future-major file may
	// contain keys the strict decoder would reject before the gate fires.
	var probe struct {
		SchemaVersion *int `yaml:"schema_version"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil, &ManifestError{Path: "(document)", Detail: err.Error(), kind: ErrManifestInvalid}
	}
	if probe.SchemaVersion == nil {
		return nil, &ManifestError{Path: "schema_version", kind: ErrManifestMissingField}
	}
	if *probe.SchemaVersion != SupportedManifestSchemaMajor {
		return nil, &ManifestError{
			Path:   "schema_version",
			Detail: fmt.Sprintf("got %d, supported major is %d", *probe.SchemaVersion, SupportedManifestSchemaMajor),
			kind:   ErrManifestSchemaVersion,
		}
	}

	var m NetworkManifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, &ManifestError{Path: "(document)", Detail: err.Error(), kind: ErrManifestInvalid}
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func isPlaceholder(s string) bool {
	t := strings.TrimSpace(s)
	return strings.EqualFold(t, "TBD") || strings.HasPrefix(t, "REPLACE_")
}

var (
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
	peerRe   = regexp.MustCompile(`^[0-9a-fA-F]{40}@[^@\s:]+:\d+$`)
)

func (m *NetworkManifest) validate() error {
	var errs []error
	fail := func(path, detail string, kind error) {
		errs = append(errs, &ManifestError{Path: path, Detail: detail, kind: kind})
	}
	requireString := func(path, v string) {
		switch {
		case strings.TrimSpace(v) == "":
			fail(path, "", ErrManifestMissingField)
		case isPlaceholder(v):
			fail(path, fmt.Sprintf("%q", v), ErrManifestPlaceholder)
		}
	}
	optionalString := func(path string, v *string) {
		if v != nil && isPlaceholder(*v) {
			fail(path, fmt.Sprintf("%q", *v), ErrManifestPlaceholder)
		}
	}
	requirePositive := func(path string, v int) {
		if v <= 0 {
			fail(path, fmt.Sprintf("got %d", v), ErrManifestMissingField)
		}
	}

	requireString("network_name", m.NetworkName)
	requireString("network_type", m.NetworkType)
	requireString("chain_id", m.ChainID)
	requireString("daemon_name", m.DaemonName)
	requireString("node_home", m.NodeHome)
	requireString("account_prefix", m.AccountPrefix)
	requireString("validator_operator_prefix", m.ValidatorOperatorPrefix)
	requireString("validator_consensus_prefix", m.ValidatorConsensusPrefix)
	requireString("base_denom", m.BaseDenom)
	requireString("display_denom", m.DisplayDenom)
	requireString("symbol", m.Symbol)
	requireString("account_key_algorithm", m.AccountKeyAlgorithm)
	requireString("consensus_key_algorithm", m.ConsensusKeyAlgorithm)
	requirePositive("display_exponent", m.DisplayExponent)
	requirePositive("slip44_coin_type", m.Slip44CoinType)

	if m.NetworkType != "" && m.NetworkType != "mainnet" && m.NetworkType != "testnet" {
		fail("network_type", fmt.Sprintf("%q is not mainnet|testnet", m.NetworkType), ErrManifestInvalid)
	}

	requireString("versions.sdk", m.Versions.SDK)
	requireString("versions.cometbft", m.Versions.CometBFT)
	requireString("versions.ibc_go", m.Versions.IBCGo)
	requireString("versions.cosmwasm_wasmd", m.Versions.CosmwasmWasmd)
	requireString("versions.go", m.Versions.Go)
	requireString("versions.app", m.Versions.App)

	requireString("fees.minimum_gas_price", m.Fees.MinimumGasPrice)
	requireString("fees.recommended_gas_price", m.Fees.RecommendedGasPrice)
	requireString("fees.recommended_gas_adjustment", m.Fees.RecommendedGasAdjustment)
	if m.Fees.MinimumGasPrice != "" && !isPlaceholder(m.Fees.MinimumGasPrice) {
		if _, err := sdk.ParseDecCoin(m.Fees.MinimumGasPrice); err != nil {
			fail("fees.minimum_gas_price", err.Error(), ErrManifestInvalid)
		}
	}
	if m.Fees.RecommendedGasPrice != "" && !isPlaceholder(m.Fees.RecommendedGasPrice) {
		if _, err := sdk.ParseDecCoin(m.Fees.RecommendedGasPrice); err != nil {
			fail("fees.recommended_gas_price", err.Error(), ErrManifestInvalid)
		}
	}
	if m.Fees.RecommendedGasAdjustment != "" && !isPlaceholder(m.Fees.RecommendedGasAdjustment) {
		adj, err := sdkmath.LegacyNewDecFromStr(m.Fees.RecommendedGasAdjustment)
		if err != nil {
			fail("fees.recommended_gas_adjustment", err.Error(), ErrManifestInvalid)
		} else if !adj.IsPositive() {
			fail("fees.recommended_gas_adjustment", "must be > 0", ErrManifestInvalid)
		}
	}

	requirePositive("ports.p2p", m.Ports.P2P)
	requirePositive("ports.rpc", m.Ports.RPC)
	requirePositive("ports.grpc", m.Ports.GRPC)
	requirePositive("ports.rest", m.Ports.REST)
	requirePositive("ports.metrics", m.Ports.Metrics)

	requireString("genesis.sha256", m.Genesis.SHA256)
	requireString("genesis.file", m.Genesis.File)
	if s := m.Genesis.SHA256; s != "" && !isPlaceholder(s) && !sha256Re.MatchString(s) {
		fail("genesis.sha256", "not a lowercase 64-hex sha256", ErrManifestInvalid)
	}

	if len(m.Endpoints) == 0 {
		fail("endpoints", "", ErrManifestMissingField)
	}
	for i, ep := range m.Endpoints {
		p := fmt.Sprintf("endpoints[%d]", i)
		requireString(p+".kind", ep.Kind)
		requireString(p+".url", ep.URL)
		requireString(p+".provider", ep.Provider)
		switch ep.Kind {
		case "", "rpc", "rest", "grpc":
		default:
			fail(p+".kind", fmt.Sprintf("%q is not rpc|rest|grpc", ep.Kind), ErrManifestInvalid)
		}
	}

	if len(m.Peers.Seeds) == 0 {
		fail("peers.seeds", "", ErrManifestMissingField)
	}
	for i, s := range m.Peers.Seeds {
		p := fmt.Sprintf("peers.seeds[%d]", i)
		requireString(p, s)
		if s != "" && !isPlaceholder(s) && !peerRe.MatchString(s) {
			fail(p, fmt.Sprintf("%q is not nodeid@host:port", s), ErrManifestInvalid)
		}
	}
	for i, s := range m.Peers.PersistentPeers {
		p := fmt.Sprintf("peers.persistent_peers[%d]", i)
		requireString(p, s)
		if s != "" && !isPlaceholder(s) && !peerRe.MatchString(s) {
			fail(p, fmt.Sprintf("%q is not nodeid@host:port", s), ErrManifestInvalid)
		}
	}

	// Explorer is mainnet-required; the testnet has no public branded
	// explorer (R12) so the field is nullable there like faucet/status_page.
	if m.NetworkType == "mainnet" || m.Links.Explorer != "" {
		requireString("links.explorer", m.Links.Explorer)
	}
	requireString("links.registry_dir", m.Links.RegistryDir)
	optionalString("links.faucet", m.Links.Faucet)
	optionalString("links.status_page", m.Links.StatusPage)

	for i, d := range m.Divergences {
		requireString(fmt.Sprintf("divergences[%d]", i), d)
	}
	if m.NetworkType == "mainnet" && len(m.Divergences) > 0 {
		fail("divergences", "testnet-only field set on a mainnet manifest (FR-057)", ErrManifestInvalid)
	}

	return errors.Join(errs...)
}
