package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
)

type ruleStatus string

const (
	rulePass ruleStatus = "PASS"
	ruleFail ruleStatus = "FAIL"
	ruleSkip ruleStatus = "SKIP"
)

type ruleResult struct {
	Rule   int        `json:"rule"`
	Name   string     `json:"name"`
	Status ruleStatus `json:"status"`
	Detail string     `json:"detail,omitempty"`
}

// verifyReport is the machine-readable output contract of `verify` (stdout).
type verifyReport struct {
	Manifest string `json:"manifest"`
	Network  string `json:"network_type"`
	ChainID  string `json:"chain_id"`
	// "live" or "offline"; offline skips every network-dependent check.
	Mode  string       `json:"mode"`
	Rules []ruleResult `json:"rules"`
	// Rule 2 CometBFT patch-level skew, recorded whenever present — never
	// silently tolerated (surfaced into export reports and KitRelease).
	CometBFTLineSkew string `json:"cometbft_line_skew,omitempty"`
	Pass             bool   `json:"pass"`
}

type verifier struct {
	manifestPath string
	genesisPath  string
	offline      bool

	m   *client.NetworkManifest
	raw []byte
	st  staticNetwork

	report verifyReport
}

func (v *verifier) add(rule int, name string, status ruleStatus, detailf string, args ...any) {
	v.report.Rules = append(v.report.Rules, ruleResult{
		Rule: rule, Name: name, Status: status, Detail: fmt.Sprintf(detailf, args...),
	})
}

func (v *verifier) skipLive(rule int, name string) {
	v.add(rule, name, ruleSkip, "offline mode: live check skipped")
}

func runVerify(manifestPath, genesisPath string, offline bool) (*verifyReport, error) {
	v := &verifier{manifestPath: manifestPath, genesisPath: genesisPath, offline: offline}
	v.report.Manifest = manifestPath
	if offline {
		v.report.Mode = "offline"
	} else {
		v.report.Mode = "live"
	}

	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	v.raw = raw

	// Rule 8 first: the loader is the placeholder/missing-value gate; nothing
	// else can run on an unparseable manifest.
	m, err := client.ParseManifest(raw)
	if err != nil {
		v.add(8, "no-placeholder-values", ruleFail, "manifest failed schema/placeholder validation: %v", err)
		v.report.Pass = false
		return &v.report, nil
	}
	v.m = m
	v.add(8, "no-placeholder-values", rulePass, "schema valid; no TBD/REPLACE_*/missing values")
	v.report.Network = m.NetworkType
	v.report.ChainID = m.ChainID

	st, ok := staticFor(m.NetworkType)
	if !ok {
		return nil, fmt.Errorf("unknown network_type %q", m.NetworkType)
	}
	v.st = st

	ctx := context.Background()
	v.rule1(ctx)
	v.rule2(ctx)
	v.rule3(ctx)
	v.rule4()
	v.rule5(ctx)
	v.rule6()
	v.rule7(ctx)
	v.rule9(ctx)
	v.rule10()
	v.rule11(ctx)

	v.report.Pass = true
	for _, r := range v.report.Rules {
		if r.Status == ruleFail {
			v.report.Pass = false
		}
	}
	return &v.report, nil
}

// Rule 1: chain_id == live RPC /status .node_info.network.
func (v *verifier) rule1(ctx context.Context) {
	const name = "chain-id-matches-live"
	if v.m.ChainID != v.st.ChainID {
		v.add(1, name, ruleFail, "chain_id %q != verified static value %q", v.m.ChainID, v.st.ChainID)
		return
	}
	if v.offline {
		v.skipLive(1, name)
		return
	}
	liveID, err := fetchLiveChainID(ctx, v.rpcEndpoint())
	if err != nil {
		v.add(1, name, ruleFail, "live RPC status unavailable: %v", err)
		return
	}
	if liveID != v.m.ChainID {
		v.add(1, name, ruleFail, "manifest chain_id %q != live %q", v.m.ChainID, liveID)
		return
	}
	v.add(1, name, rulePass, "chain_id %q matches live /status", v.m.ChainID)
}

// Rule 2: the version block matches BOTH the release go.mod pins AND the
// RUNNING binary. app/sdk are strict-equal vs live REST node_info; ibc_go and
// cosmwasm_wasmd are strict-equal vs the live application_version.build_deps
// (the exact linked module versions — a stale pin cannot pass); go and cometbft
// are compared on the live minor line with patch skew RECORDED (the go.mod `go`
// directive is a floor, and CometBFT self-reports a lagging patch). Offline: the
// go.mod pins are still checked and the live half is skipped.
func (v *verifier) rule2(ctx context.Context) {
	const name = "versions-match-release-and-live"
	var problems []string
	var buildDepNotes []string // non-fatal live-vs-pin observations (patch skew, absent build info)
	pin := func(label, got, want string) {
		if !versionsEqual(got, want) {
			problems = append(problems, fmt.Sprintf("versions.%s %q != go.mod pin %q", label, got, want))
		}
	}
	pin("cometbft", v.m.Versions.CometBFT, v.st.CometBFTVersion)
	pin("ibc_go", v.m.Versions.IBCGo, v.st.IBCGoVersion)
	pin("cosmwasm_wasmd", v.m.Versions.CosmwasmWasmd, v.st.WasmdVersion)
	pin("go", v.m.Versions.Go, v.st.GoVersion)
	pin("sdk", v.m.Versions.SDK, v.st.SDKVersion)

	if v.offline {
		if len(problems) > 0 {
			v.add(2, name, ruleFail, "%s", strings.Join(problems, "; "))
			return
		}
		v.add(2, name, ruleSkip, "pinned versions match go.mod source; live app/sdk/cometbft check skipped (offline)")
		return
	}

	ni, err := fetchRESTNodeInfo(ctx, v.restEndpoint())
	if err != nil {
		problems = append(problems, fmt.Sprintf("live REST node_info unavailable: %v", err))
	} else {
		if !versionsEqual(v.m.Versions.App, ni.AppVersion) {
			problems = append(problems, fmt.Sprintf("versions.app %q != live %q", v.m.Versions.App, ni.AppVersion))
		}
		if !versionsEqual(v.m.Versions.SDK, ni.SDKVersion) {
			problems = append(problems, fmt.Sprintf("versions.sdk %q != live %q", v.m.Versions.SDK, ni.SDKVersion))
		}
		// ibc_go/cosmwasm vs the live build_deps (exact linked module versions) and
		// go vs the live go_version (minor line, patch skew recorded) — the RUNNING
		// binary, not just the go.mod pin. See checkLiveBuildDeps.
		p, n := checkLiveBuildDeps(v.m.Versions.IBCGo, v.m.Versions.CosmwasmWasmd, v.m.Versions.Go, ni)
		problems = append(problems, p...)
		buildDepNotes = append(buildDepNotes, n...)
	}

	status, err := fetchRPCStatus(ctx, v.rpcEndpoint())
	if err != nil {
		problems = append(problems, fmt.Sprintf("live RPC status unavailable: %v", err))
	} else {
		manifestLine, mlErr := minorLine(v.m.Versions.CometBFT)
		liveLine, llErr := minorLine(status.CometBFTVersion)
		switch {
		case mlErr != nil:
			problems = append(problems, mlErr.Error())
		case llErr != nil:
			problems = append(problems, llErr.Error())
		case manifestLine != liveLine:
			problems = append(problems, fmt.Sprintf(
				"versions.cometbft %q not on the live node's minor line (%q)",
				v.m.Versions.CometBFT, status.CometBFTVersion))
		case !versionsEqual(v.m.Versions.CometBFT, status.CometBFTVersion):
			v.report.CometBFTLineSkew = fmt.Sprintf(
				"manifest pins %s (go.mod authority); live node reports %s — same %s line, patch-level skew recorded",
				v.m.Versions.CometBFT, status.CometBFTVersion, manifestLine)
		}
	}

	if len(problems) > 0 {
		v.add(2, name, ruleFail, "%s", strings.Join(problems, "; "))
		return
	}
	// Base summary asserts only what is always positively verified on a PASS;
	// go/cometbft minor-line status and any build-dep degradation are conveyed by
	// the skew note + buildDepNotes appended below (so an unavailable go_version
	// never reads as "verified").
	detail := "app/sdk match live node_info; ibc_go/cosmwasm match live build_deps; cometbft on the live minor line"
	if v.report.CometBFTLineSkew != "" {
		detail += "; " + v.report.CometBFTLineSkew
	}
	for _, n := range buildDepNotes {
		detail += "; " + n
	}
	v.add(2, name, rulePass, "%s", detail)
}

// Rule 3: fees.minimum_gas_price == live x/globalfee floor.
func (v *verifier) rule3(ctx context.Context) {
	const name = "min-gas-price-matches-globalfee"
	if v.offline {
		v.skipLive(3, name)
		return
	}
	live, err := fetchGlobalFeeFloor(ctx, v.rpcEndpoint())
	if err != nil {
		v.add(3, name, ruleFail, "live globalfee query failed: %v", err)
		return
	}
	if v.m.Fees.MinimumGasPrice != live {
		v.add(3, name, ruleFail, "fees.minimum_gas_price %q != live floor %q", v.m.Fees.MinimumGasPrice, live)
		return
	}
	v.add(3, name, rulePass, "minimum_gas_price %q matches live x/globalfee floor", live)
}

// Rule 4: genesis.sha256 == published checksum == sha256 of the injected
// genesis file (file half skipped until export injects the file).
func (v *verifier) rule4() {
	const name = "genesis-checksum"
	if v.m.Genesis.SHA256 != v.st.GenesisSHA256 {
		v.add(4, name, ruleFail, "genesis.sha256 %q != published checksum %q", v.m.Genesis.SHA256, v.st.GenesisSHA256)
		return
	}
	genesisPath := v.genesisPath
	if genesisPath == "" {
		genesisPath = filepath.Join(filepath.Dir(v.manifestPath), v.m.Genesis.File)
	}
	data, err := os.ReadFile(genesisPath)
	if err != nil {
		if os.IsNotExist(err) && v.genesisPath == "" {
			v.add(4, name, rulePass, "matches published checksum; genesis file not present (injected at export) — file hash deferred")
			return
		}
		v.add(4, name, ruleFail, "genesis file unreadable: %v", err)
		return
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != v.m.Genesis.SHA256 {
		v.add(4, name, ruleFail, "sha256(%s) = %s != manifest %s", genesisPath, got, v.m.Genesis.SHA256)
		return
	}
	v.add(4, name, rulePass, "manifest, published checksum, and genesis file all agree")
}

// Rule 5: >= 2 endpoints, each responding with the manifest chain_id; when
// endpoints do not span 2 distinct providers the shared-gateway note must be
// documented in the manifest (D12 adds the independent provider).
func (v *verifier) rule5(ctx context.Context) {
	const name = "bootstrap-endpoints"
	if len(v.m.Endpoints) < 2 {
		v.add(5, name, ruleFail, "%d endpoints; contract requires >= 2", len(v.m.Endpoints))
		return
	}
	sharedInfra := distinctProviders(v.m.Endpoints) < 2
	if sharedInfra && !bytes.Contains(v.raw, []byte(sharedGatewayMarker)) {
		v.add(5, name, ruleFail,
			"endpoints share one provider but the manifest lacks the %q documented note", sharedGatewayMarker)
		return
	}
	if v.offline {
		v.skipLive(5, name)
		return
	}
	var problems []string
	for _, ep := range v.m.Endpoints {
		switch ep.Kind {
		case "rpc":
			st, err := fetchRPCStatus(ctx, ep.URL)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", ep.URL, err))
			} else if st.ChainID != v.m.ChainID {
				problems = append(problems, fmt.Sprintf("%s reports chain_id %q", ep.URL, st.ChainID))
			}
		case "rest":
			ni, err := fetchRESTNodeInfo(ctx, ep.URL)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", ep.URL, err))
			} else if ni.ChainID != v.m.ChainID {
				problems = append(problems, fmt.Sprintf("%s reports chain_id %q", ep.URL, ni.ChainID))
			}
		default:
			// No public gRPC endpoint exists today (plan D6); liveness of a
			// grpc entry is not probed by this tool.
		}
	}
	if len(problems) > 0 {
		v.add(5, name, ruleFail, "%s", strings.Join(problems, "; "))
		return
	}
	detail := fmt.Sprintf("%d endpoints respond with chain_id %q", len(v.m.Endpoints), v.m.ChainID)
	if sharedInfra {
		detail += "; shared-gateway note documented (single provider until plan D12)"
	}
	v.add(5, name, rulePass, "%s", detail)
}

// Rule 6: every peer DNS-named (no raw IPs, both networks) and TCP-dialable.
func (v *verifier) rule6() {
	const name = "peers-dns-named-and-dialable"
	peers := append(append([]string{}, v.m.Peers.Seeds...), v.m.Peers.PersistentPeers...)
	var addrs []string
	var problems []string
	for _, p := range peers {
		addr, err := peerHostPort(p)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		addrs = append(addrs, addr)
	}
	if len(problems) > 0 {
		v.add(6, name, ruleFail, "%s", strings.Join(problems, "; "))
		return
	}
	if v.offline {
		v.add(6, name, ruleSkip, "all %d peers DNS-named; dialability skipped (offline)", len(addrs))
		return
	}
	if errs := dialPeers(addrs); len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		v.add(6, name, ruleFail, "%s", strings.Join(msgs, "; "))
		return
	}
	v.add(6, name, rulePass, "all %d peers DNS-named and TCP-dialable", len(addrs))
}

// Rule 7: all non-null links resolve with HTTP < 400.
func (v *verifier) rule7(ctx context.Context) {
	const name = "links-resolve"
	links := map[string]string{}
	if v.m.Links.Explorer != "" {
		links["links.explorer"] = v.m.Links.Explorer
	}
	if v.m.Links.Faucet != nil {
		links["links.faucet"] = *v.m.Links.Faucet
	}
	if v.m.Links.StatusPage != nil {
		links["links.status_page"] = *v.m.Links.StatusPage
	}
	if v.offline {
		v.skipLive(7, name)
		return
	}
	var problems []string
	for field, url := range links {
		if err := checkLink(ctx, url); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", field, err))
		}
	}
	if len(problems) > 0 {
		v.add(7, name, ruleFail, "%s", strings.Join(problems, "; "))
		return
	}
	v.add(7, name, rulePass, "%d non-null links resolve", len(links))
}

// registry assetlist.json denom shape (Cosmos chain-registry format); only the
// denom fields rule 9 compares are decoded.
type registryDenomUnit struct {
	Denom    string `json:"denom"`
	Exponent int    `json:"exponent"`
}

type registryAsset struct {
	Base       string              `json:"base"`
	Display    string              `json:"display"`
	Symbol     string              `json:"symbol"`
	DenomUnits []registryDenomUnit `json:"denom_units"`
}

// denomMetadataClient is the narrow slice of client.Client rule 9's on-chain
// read needs; a package var builds it so tests can inject fakes and transport
// errors without a live node.
type denomMetadataClient interface {
	DenomMetadata(ctx context.Context, denom string) (*bankv1beta1.Metadata, error)
	Close() error
}

var newDenomMetadataClient = func(rpcURL string) (denomMetadataClient, error) {
	return client.NewCometRPC(rpcURL, client.WithTimeout(liveCallTimeout))
}

// Rule 9: registry chain.json scalars and assetlist.json denom units must equal
// the manifest, and the on-chain denom metadata must be present and match (the
// plan-D1 denom-metadata upgrade has landed, so absent metadata now fails). A
// network read that cannot be completed fails the rule — an unverifiable read
// is never reported as a pass or an absent gap.
func (v *verifier) rule9(ctx context.Context) {
	const name = "registry-consistency"
	dir := v.findRegistryDir()
	if dir == "" {
		v.add(9, name, ruleSkip, "registry dir %q not found next to the manifest tree (registry metadata is a separate deliverable)", v.m.Links.RegistryDir)
		return
	}
	if v.m.NetworkType == "testnet" {
		dir = filepath.Join(dir, "testnets", "sovrtestnet")
	}
	chainJSON := filepath.Join(dir, "chain.json")
	data, err := os.ReadFile(chainJSON)
	if err != nil {
		v.add(9, name, ruleSkip, "%s not present yet", chainJSON)
		return
	}
	var reg struct {
		ChainID      string `json:"chain_id"`
		Bech32Prefix string `json:"bech32_prefix"`
		Slip44       int    `json:"slip44"`
		DaemonName   string `json:"daemon_name"`
	}
	if err := json.Unmarshal(data, &reg); err != nil {
		v.add(9, name, ruleFail, "%s unparseable: %v", chainJSON, err)
		return
	}
	var problems []string
	if reg.ChainID != v.m.ChainID {
		problems = append(problems, fmt.Sprintf("registry chain_id %q != manifest %q", reg.ChainID, v.m.ChainID))
	}
	if reg.Bech32Prefix != v.m.AccountPrefix {
		problems = append(problems, fmt.Sprintf("registry bech32_prefix %q != manifest %q", reg.Bech32Prefix, v.m.AccountPrefix))
	}
	if reg.Slip44 != v.m.Slip44CoinType {
		problems = append(problems, fmt.Sprintf("registry slip44 %d != manifest %d", reg.Slip44, v.m.Slip44CoinType))
	}
	if reg.DaemonName != v.m.DaemonName {
		problems = append(problems, fmt.Sprintf("registry daemon_name %q != manifest %q", reg.DaemonName, v.m.DaemonName))
	}
	problems = append(problems, v.assetlistProblems(dir)...)
	if len(problems) > 0 {
		v.add(9, name, ruleFail, "%s", strings.Join(problems, "; "))
		return
	}
	detail := "registry chain.json and assetlist.json agree with the manifest"
	if v.offline {
		v.add(9, name, rulePass, "%s", detail)
		return
	}
	note, ok := v.checkOnChainDenomMetadata(ctx)
	detail += note
	if !ok {
		v.add(9, name, ruleFail, "%s", detail)
		return
	}
	v.add(9, name, rulePass, "%s", detail)
}

// assetlistProblems compares registry assetlist.json denom units against the
// manifest denom fields. A missing or unparseable list, a missing base asset,
// or any field mismatch is a problem. display is matched case-insensitively:
// the committed assetlist uses the lowercase display denom (e.g. "sovr") while
// the manifest carries the "SOVR" display symbol.
func (v *verifier) assetlistProblems(dir string) []string {
	assetJSON := filepath.Join(dir, "assetlist.json")
	data, err := os.ReadFile(assetJSON)
	if err != nil {
		return []string{fmt.Sprintf("registry %s unreadable: %v", assetJSON, err)}
	}
	var al struct {
		Assets []registryAsset `json:"assets"`
	}
	if err := json.Unmarshal(data, &al); err != nil {
		return []string{fmt.Sprintf("registry %s unparseable: %v", assetJSON, err)}
	}
	idx := -1
	for i := range al.Assets {
		if al.Assets[i].Base == v.m.BaseDenom {
			idx = i
			break
		}
	}
	if idx < 0 {
		return []string{fmt.Sprintf("registry assetlist has no asset with base %q", v.m.BaseDenom)}
	}
	asset := al.Assets[idx]
	var problems []string
	if !strings.EqualFold(asset.Display, v.m.DisplayDenom) {
		problems = append(problems, fmt.Sprintf("assetlist display %q != manifest display_denom %q", asset.Display, v.m.DisplayDenom))
	}
	if asset.Symbol != v.m.Symbol {
		problems = append(problems, fmt.Sprintf("assetlist symbol %q != manifest symbol %q", asset.Symbol, v.m.Symbol))
	}
	exp, ok := registryDisplayExponent(asset.DenomUnits, asset.Display)
	switch {
	case !ok:
		problems = append(problems, fmt.Sprintf("assetlist has no denom_unit for display %q", asset.Display))
	case exp != v.m.DisplayExponent:
		problems = append(problems, fmt.Sprintf("assetlist display exponent %d != manifest display_exponent %d", exp, v.m.DisplayExponent))
	}
	return problems
}

func registryDisplayExponent(units []registryDenomUnit, display string) (int, bool) {
	for _, u := range units {
		if strings.EqualFold(u.Denom, display) {
			return u.Exponent, true
		}
	}
	return 0, false
}

// checkOnChainDenomMetadata reads the live bank denom metadata for the base
// denom and classifies the result. The plan-D1 x/bank denom-metadata upgrade
// has landed on mainnet (v0.21.0, composed into v0.23.0-combined) and testnet,
// so the metadata MUST now be present: a genuine not-found / empty answer is a
// FAILURE (the node is behind the D1 upgrade height, or the manifest denom is
// wrong) — this is the hard denom/decimals gate; a present record that
// disagrees with the manifest is a failure; a transport/query error is a
// failure — an unverifiable read must never be green-lit.
func (v *verifier) checkOnChainDenomMetadata(ctx context.Context) (note string, ok bool) {
	c, err := newDenomMetadataClient(v.rpcEndpoint())
	if err != nil {
		return fmt.Sprintf("; on-chain denom metadata unverifiable (RPC connect failed: %v)", err), false
	}
	defer func() { _ = c.Close() }()
	md, err := c.DenomMetadata(ctx, v.m.BaseDenom)
	switch {
	case errors.Is(err, client.ErrNotFound):
		return "; on-chain denom metadata absent — must be present since the plan-D1 upgrade landed (node behind the D1 height, or wrong denom)", false
	case err != nil:
		return fmt.Sprintf("; on-chain denom metadata unverifiable (query failed: %v)", err), false
	case md == nil || md.GetBase() == "":
		return "; on-chain denom metadata absent — must be present since the plan-D1 upgrade landed (node behind the D1 height, or wrong denom)", false
	}
	if mism := onChainMetadataMismatches(md, v.m); len(mism) > 0 {
		return "; on-chain denom metadata mismatch: " + strings.Join(mism, ", "), false
	}
	return "; on-chain denom metadata present and matches manifest/assetlist", true
}

func onChainMetadataMismatches(md *bankv1beta1.Metadata, m *client.NetworkManifest) []string {
	var mism []string
	if md.GetBase() != m.BaseDenom {
		mism = append(mism, fmt.Sprintf("base %q != %q", md.GetBase(), m.BaseDenom))
	}
	if !strings.EqualFold(md.GetDisplay(), m.DisplayDenom) {
		mism = append(mism, fmt.Sprintf("display %q != %q", md.GetDisplay(), m.DisplayDenom))
	}
	if md.GetSymbol() != m.Symbol {
		mism = append(mism, fmt.Sprintf("symbol %q != %q", md.GetSymbol(), m.Symbol))
	}
	exp, ok := onChainDisplayExponent(md.GetDenomUnits(), md.GetDisplay())
	switch {
	case !ok:
		mism = append(mism, fmt.Sprintf("no denom_unit for display %q", md.GetDisplay()))
	case exp != m.DisplayExponent:
		mism = append(mism, fmt.Sprintf("display exponent %d != %d", exp, m.DisplayExponent))
	}
	return mism
}

func onChainDisplayExponent(units []*bankv1beta1.DenomUnit, display string) (int, bool) {
	for _, u := range units {
		if u != nil && strings.EqualFold(u.GetDenom(), display) {
			return int(u.GetExponent()), true
		}
	}
	return 0, false
}

// Rule 10: sidecars are generated from network.yaml and byte-equal.
func (v *verifier) rule10() {
	const name = "sidecars-derived-from-manifest"
	dir := filepath.Dir(v.manifestPath)
	endpoints, err := renderEndpointsJSON(v.m)
	if err != nil {
		v.add(10, name, ruleFail, "endpoints.json render: %v", err)
		return
	}
	expected := map[string][]byte{
		"genesis.sha256": renderGenesisSHA256(v.m),
		"peers.txt":      renderPeersTxt(v.m),
		"endpoints.json": endpoints,
	}
	var problems []string
	for file, want := range expected {
		got, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", file, err))
			continue
		}
		if !bytes.Equal(got, want) {
			problems = append(problems, fmt.Sprintf("%s differs from content derived from network.yaml", file))
		}
	}
	if len(problems) > 0 {
		v.add(10, name, ruleFail, "%s", strings.Join(problems, "; "))
		return
	}
	v.add(10, name, rulePass, "genesis.sha256, peers.txt, endpoints.json byte-match the manifest")
}

// Rule 11: manifest account_prefix == the live chain's actual bech32 prefix.
// Rule 9 compares the prefix against the registry *file* and skips entirely
// when the registry dir is absent; this rule pins account_prefix against chain
// truth directly, so a wrong prefix cannot ship just because registry metadata
// is a separate deliverable. Source: a live validator operator_address, whose
// decoded HRP is "<account_prefix>valoper". An unverifiable read fails the rule
// — it is never reported as a pass.
func (v *verifier) rule11(ctx context.Context) {
	const name = "account-prefix-matches-live"
	if v.offline {
		v.skipLive(11, name)
		return
	}
	operHRP, err := fetchLiveOperatorPrefix(ctx, v.restEndpoint())
	if err != nil {
		v.add(11, name, ruleFail, "live account prefix unverifiable: %v", err)
		return
	}
	const valoperSuffix = "valoper"
	if !strings.HasSuffix(operHRP, valoperSuffix) {
		v.add(11, name, ruleFail,
			"live validator prefix %q is not a <account_prefix>valoper HRP", operHRP)
		return
	}
	liveAccountPrefix := strings.TrimSuffix(operHRP, valoperSuffix)
	if liveAccountPrefix != v.m.AccountPrefix {
		v.add(11, name, ruleFail,
			"live account prefix %q != manifest account_prefix %q (operator HRP %q)",
			liveAccountPrefix, v.m.AccountPrefix, operHRP)
		return
	}
	v.add(11, name, rulePass,
		"manifest account_prefix %q matches the live chain (operator HRP %q)", v.m.AccountPrefix, operHRP)
}

func (v *verifier) rpcEndpoint() string {
	for _, ep := range v.m.Endpoints {
		if ep.Kind == "rpc" {
			return ep.URL
		}
	}
	return v.st.BootstrapRPC
}

func (v *verifier) restEndpoint() string {
	for _, ep := range v.m.Endpoints {
		if ep.Kind == "rest" {
			return ep.URL
		}
	}
	return v.st.BootstrapREST
}

func (v *verifier) findRegistryDir() string {
	rel := strings.TrimRight(v.m.Links.RegistryDir, "/")
	base := filepath.Dir(v.manifestPath)
	for _, root := range []string{base, filepath.Join(base, ".."), filepath.Join(base, "..", "..")} {
		dir := filepath.Join(root, rel)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
	}
	return ""
}
