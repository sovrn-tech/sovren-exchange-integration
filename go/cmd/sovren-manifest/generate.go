package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
)

// generateResult is the machine-readable output contract of `generate`
// (stdout).
type generateResult struct {
	Network          string   `json:"network_type"`
	ChainID          string   `json:"chain_id"`
	OutDir           string   `json:"out_dir"`
	Files            []string `json:"files"`
	AppVersion       string   `json:"app_version"`
	MinimumGasPrice  string   `json:"minimum_gas_price"`
	CometBFTLineSkew string   `json:"cometbft_line_skew,omitempty"`
}

// runGenerate merges the checked-in verified values with live-chain reads and
// writes network.yaml plus the three sidecars (all derived from the yaml).
func runGenerate(network, outDir, rpcOverride, restOverride string) (*generateResult, error) {
	st, ok := staticFor(network)
	if !ok {
		return nil, fmt.Errorf("unknown network %q (mainnet|testnet)", network)
	}
	if st.Blocked != "" {
		return nil, fmt.Errorf("%s manifest generation is blocked — %s", network, st.Blocked)
	}
	rpcURL := st.BootstrapRPC
	if rpcOverride != "" {
		rpcURL = rpcOverride
	}
	restURL := st.BootstrapREST
	if restOverride != "" {
		restURL = restOverride
	}

	ctx := context.Background()

	liveID, err := fetchLiveChainID(ctx, rpcURL)
	if err != nil {
		return nil, err
	}
	if liveID != st.ChainID {
		return nil, fmt.Errorf("live chain_id %q != verified static value %q — refusing to generate", liveID, st.ChainID)
	}

	ni, err := fetchRESTNodeInfo(ctx, restURL)
	if err != nil {
		return nil, err
	}
	if !versionsEqual(ni.SDKVersion, st.SDKVersion) {
		return nil, fmt.Errorf("live sdk version %q != go.mod pin %q — reconcile before generating", ni.SDKVersion, st.SDKVersion)
	}

	floor, err := fetchGlobalFeeFloor(ctx, rpcURL)
	if err != nil {
		return nil, err
	}

	res := &generateResult{Network: network, ChainID: st.ChainID, OutDir: outDir,
		AppVersion: ni.AppVersion, MinimumGasPrice: floor}

	status, err := fetchRPCStatus(ctx, rpcURL)
	if err != nil {
		return nil, err
	}
	if !versionsEqual(status.CometBFTVersion, st.CometBFTVersion) {
		manifestLine, mErr := minorLine(st.CometBFTVersion)
		liveLine, lErr := minorLine(status.CometBFTVersion)
		if mErr != nil || lErr != nil || manifestLine != liveLine {
			return nil, fmt.Errorf("live cometbft %q not on the go.mod pin's minor line (%q)", status.CometBFTVersion, st.CometBFTVersion)
		}
		res.CometBFTLineSkew = fmt.Sprintf(
			"manifest pins %s (go.mod authority); live node reports %s — same %s line, patch-level skew recorded",
			st.CometBFTVersion, status.CometBFTVersion, manifestLine)
	}

	m := manifestFromStatic(st, ni.AppVersion, floor)
	rendered := renderManifestYAML(m)

	// Round-trip through the loader: what we wrote must be exactly what
	// consumers will parse; sidecars derive from the parsed form.
	parsed, err := client.ParseManifest(rendered)
	if err != nil {
		return nil, fmt.Errorf("rendered manifest failed validation: %w", err)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	endpoints, err := renderEndpointsJSON(parsed)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{
		"network.yaml":   rendered,
		"genesis.sha256": renderGenesisSHA256(parsed),
		"peers.txt":      renderPeersTxt(parsed),
		"endpoints.json": endpoints,
	}
	for _, name := range []string{"network.yaml", "genesis.sha256", "peers.txt", "endpoints.json"} {
		path := filepath.Join(outDir, name)
		if err := os.WriteFile(path, files[name], 0o644); err != nil {
			return nil, err
		}
		res.Files = append(res.Files, path)
	}
	return res, nil
}

func manifestFromStatic(st staticNetwork, appVersion, minGasPrice string) *client.NetworkManifest {
	return &client.NetworkManifest{
		SchemaVersion:            client.SupportedManifestSchemaMajor,
		NetworkName:              st.NetworkName,
		NetworkType:              st.NetworkType,
		ChainID:                  st.ChainID,
		DaemonName:               daemonName,
		NodeHome:                 nodeHome,
		AccountPrefix:            accountPrefix,
		ValidatorOperatorPrefix:  validatorOperatorPrefix,
		ValidatorConsensusPrefix: validatorConsensusPrefix,
		BaseDenom:                baseDenom,
		DisplayDenom:             displayDenom,
		DisplayExponent:          displayExponent,
		Symbol:                   symbol,
		AccountKeyAlgorithm:      accountKeyAlgorithm,
		ConsensusKeyAlgorithm:    consensusKeyAlgorithm,
		Slip44CoinType:           slip44CoinType,
		Versions: client.ManifestVersions{
			SDK:           st.SDKVersion,
			CometBFT:      st.CometBFTVersion,
			IBCGo:         st.IBCGoVersion,
			CosmwasmWasmd: st.WasmdVersion,
			Go:            st.GoVersion,
			App:           appVersion,
		},
		Fees: client.ManifestFees{
			MinimumGasPrice:          minGasPrice,
			RecommendedGasPrice:      st.RecommendedGasPrice,
			RecommendedGasAdjustment: st.RecommendedGasAdjustment,
		},
		Ports:   client.ManifestPorts{P2P: 26656, RPC: 26657, GRPC: 9090, REST: 1317, Metrics: 26660},
		Genesis: client.ManifestGenesis{SHA256: st.GenesisSHA256, File: "genesis.json"},
		Endpoints: []client.ManifestEndpoint{
			{Kind: "rpc", URL: st.BootstrapRPC, Provider: "sovren"},
			{Kind: "rest", URL: st.BootstrapREST, Provider: "sovren"},
		},
		Peers: client.ManifestPeers{Seeds: st.Seeds, PersistentPeers: st.PersistentPeers},
		Links:       client.ManifestLinks{Explorer: st.Explorer, Faucet: optionalURL(st.Faucet), RegistryDir: "registry/"},
		Divergences: st.Divergences,
	}
}

// optionalURL maps an empty string to nil (an absent link) and any non-empty
// value to a pointer to it, for the manifest's optional *string link fields.
func optionalURL(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
