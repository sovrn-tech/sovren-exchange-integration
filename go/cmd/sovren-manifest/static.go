package main

// Checked-in verified network values (research R1). Every value here was read
// from the live network or the release go.mod — never guessed (FR-007). The
// generate command merges these with live-chain reads (chain_id, app/sdk
// versions, globalfee floor) and the verify command uses them as the offline
// half of contract rules 1, 2 and 4.

// staticNetwork is the per-network verified-value source.
type staticNetwork struct {
	NetworkName   string
	NetworkType   string
	ChainID       string
	BootstrapRPC  string
	BootstrapREST string

	// Pinned from the release go.mod (rule 2 authority for these four).
	CometBFTVersion string
	IBCGoVersion    string
	WasmdVersion    string
	GoVersion       string
	// Cross-checked strictly against live REST node_info (rule 2).
	SDKVersion string

	RecommendedGasPrice      string
	RecommendedGasAdjustment string

	// Published cluster checksum (rule 4 offline half).
	GenesisSHA256 string

	Seeds           []string
	PersistentPeers []string

	Explorer string

	// Faucet is the network's public faucet base URL (empty = no faucet, e.g.
	// mainnet). When set, sovren-manifest verify rule 7 live-checks that it
	// resolves, and the kit's `sovren-cert fund` reads it from links.faucet.
	Faucet string

	// Intentional, documented divergences from mainnet behavior (FR-057;
	// testnet only — the loader rejects divergences on mainnet manifests).
	Divergences []string

	// Non-empty means the network manifest cannot be generated yet; the
	// value is the blocker description (exit with it, generate nothing).
	Blocked string
}

const (
	daemonName               = "sovrd"
	nodeHome                 = "$HOME/.sovr"
	accountPrefix            = "sovr"
	validatorOperatorPrefix  = "sovrvaloper"
	validatorConsensusPrefix = "sovrvalcons"
	baseDenom                = "usovr"
	displayDenom             = "SOVR"
	displayExponent          = 6
	symbol                   = "SOVR"
	accountKeyAlgorithm      = "secp256k1"
	consensusKeyAlgorithm    = "ed25519"
	slip44CoinType           = 118
)

var staticMainnet = staticNetwork{
	NetworkName:   "Sovren Mainnet",
	NetworkType:   "mainnet",
	ChainID:       "sovr-1",
	BootstrapRPC:  "https://rpc.sovrchain.net",
	BootstrapREST: "https://api.sovrchain.net",

	CometBFTVersion: "v0.38.23",
	IBCGoVersion:    "v10.5.0",
	WasmdVersion:    "v0.60.7",
	GoVersion:       "1.25.7",
	SDKVersion:      "v0.53.8",

	// RecommendedGasPrice is 25x the live x/globalfee floor (0.001usovr) —
	// mempool-competition headroom, a defined multiple of the floor, not a
	// guessed value (FR-007).
	RecommendedGasPrice:      "0.025usovr",
	RecommendedGasAdjustment: "1.5",

	GenesisSHA256: "04529695e7ccfe32fcf3bc8031c343056d27cbe4aa3b3046027e27065bb9a855",

	Seeds: []string{
		"381af5e4d0d5e24af3bb4d506b06081399622d01@seed1.mainnet.sovrchain.net:32000",
		"68d9058a5dd7062d72d917ae8f2a2e30101b5a74@seed2.mainnet.sovrchain.net:32001",
	},
	PersistentPeers: []string{
		"24ebffde61a15df70687df28d9d9259dcb4beb64@sentry1a.mainnet.sovrchain.net:32200",
		"6ee852fb24a636109f49a6b7184c8533d60d71ae@sentry1b.mainnet.sovrchain.net:32201",
		"f9f287196905f574334d87370ee28369814a01eb@sentry2a.mainnet.sovrchain.net:32220",
		"d41608b237019c09281c9542bbe90532b03b65ea@sentry2b.mainnet.sovrchain.net:32221",
		"b8c805f050e090edf52fcd5169e239826ffb21a5@sentry3a.mainnet.sovrchain.net:32240",
		"a49544b198f062c893b849a37389a227a51b40d4@sentry3b.mainnet.sovrchain.net:32241",
	},

	Explorer: "https://sovrscan.com",
}

var staticTestnet = staticNetwork{
	NetworkName:   "Sovren Testnet",
	NetworkType:   "testnet",
	ChainID:       "test-sovr-1",
	BootstrapRPC:  "https://rpc.testnet.sovrchain.net",
	BootstrapREST: "https://api.testnet.sovrchain.net",

	CometBFTVersion: "v0.38.23",
	IBCGoVersion:    "v10.5.0",
	WasmdVersion:    "v0.60.7",
	GoVersion:       "1.25.7",
	SDKVersion:      "v0.53.8",

	// RecommendedGasPrice is 25x the live x/globalfee floor (0.001usovr) —
	// mempool-competition headroom, a defined multiple of the floor, not a
	// guessed value (FR-007).
	RecommendedGasPrice:      "0.025usovr",
	RecommendedGasAdjustment: "1.5",

	GenesisSHA256: "5eb3be46b3020ffc9105be09fdaed874cad0f6e9110aaa58053d72711c8aef40",

	// P2P DNS published 2026-07-22 (plan D3 closed; pct-tf PR #872).
	Seeds: []string{
		"d5e5770e1198b6ab66dcf7bfadb90610e82bf6cd@seed1.testnet.sovrchain.net:32000",
		"c24ded42a0026b4d912d8576b3d84d3c690b2d02@seed2.testnet.sovrchain.net:32001",
	},
	PersistentPeers: []string{
		"2dae62e74b0b175e7d905eeaca530464de8d8183@sentry1a.testnet.sovrchain.net:32200",
		"5a5dca65a8fa75d66a65eca470d2c88d3c1c2703@sentry1b.testnet.sovrchain.net:32201",
		"70ac5c238c5f83d6ef7480f64ec5c22d29364797@sentry2a.testnet.sovrchain.net:32220",
		"c5385795047ac6a11d3328362033721ed3213657@sentry2b.testnet.sovrchain.net:32221",
		"8cfacb1a936e3b848923a6fe631690336dc7717e@sentry3a.testnet.sovrchain.net:32240",
		"dbf89f924f158c77222343d322e57eb1042b0a4c@sentry3b.testnet.sovrchain.net:32241",
	},

	// Faucet published 2026-07-24 (plan D2 closed); sovren-cert fund reads it
	// from links.faucet, and verify rule 7 live-checks that it resolves.
	Faucet: "https://faucet.testnet.sovrchain.net",

	Divergences: []string{
		"All P2P bootstrap names (seeds and sentries) currently resolve to a single host; mainnet fronts its P2P layer with load-balanced infrastructure. Reduced bootstrap redundancy is expected on testnet.",
	},
}

func staticFor(network string) (staticNetwork, bool) {
	switch network {
	case "mainnet":
		return staticMainnet, true
	case "testnet":
		return staticTestnet, true
	}
	return staticNetwork{}, false
}
