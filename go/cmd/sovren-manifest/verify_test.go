package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
)

// chain.json / assetlist.json that agree with the mainnet static manifest
// (sovr-1 / usovr / display "sovr" / exponent 6 / symbol SOVR).
const (
	goodChainJSON = `{"chain_id":"sovr-1","bech32_prefix":"sovr","slip44":118,"daemon_name":"sovrd"}`
	goodAssetJSON = `{"assets":[{"base":"usovr","display":"sovr","symbol":"SOVR",` +
		`"denom_units":[{"denom":"usovr","exponent":0},{"denom":"sovr","exponent":6}]}]}`
)

// rule9Verifier writes a registry (chain.json + assetlist.json) next to a
// mainnet-static manifest and returns a verifier ready to run rule9.
func rule9Verifier(t *testing.T, offline bool, chainJSON, assetJSON string) *verifier {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "network.yaml"), []byte("placeholder"), 0o644))
	regDir := filepath.Join(dir, "registry")
	require.NoError(t, os.MkdirAll(regDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(regDir, "chain.json"), []byte(chainJSON), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(regDir, "assetlist.json"), []byte(assetJSON), 0o644))
	return &verifier{
		manifestPath: filepath.Join(dir, "network.yaml"),
		offline:      offline,
		m:            manifestFromStatic(staticMainnet, "v0.16.2", "0.001usovr"),
	}
}

func rule9Result(t *testing.T, v *verifier) ruleResult {
	t.Helper()
	for _, r := range v.report.Rules {
		if r.Rule == 9 {
			return r
		}
	}
	t.Fatal("rule 9 missing from report")
	return ruleResult{}
}

// fakeDenomClient stands in for a live node's denom-metadata read.
type fakeDenomClient struct {
	md  *bankv1beta1.Metadata
	err error
}

func (f *fakeDenomClient) DenomMetadata(context.Context, string) (*bankv1beta1.Metadata, error) {
	return f.md, f.err
}

func (f *fakeDenomClient) Close() error { return nil }

// withDenomClient swaps the rule-9 on-chain client factory for the test.
func withDenomClient(t *testing.T, factory func(string) (denomMetadataClient, error)) {
	t.Helper()
	prev := newDenomMetadataClient
	newDenomMetadataClient = factory
	t.Cleanup(func() { newDenomMetadataClient = prev })
}

func fakeDenomFactory(md *bankv1beta1.Metadata, err error) func(string) (denomMetadataClient, error) {
	return func(string) (denomMetadataClient, error) {
		return &fakeDenomClient{md: md, err: err}, nil
	}
}

func matchingOnChainMetadata() *bankv1beta1.Metadata {
	return &bankv1beta1.Metadata{
		Base:    "usovr",
		Display: "sovr",
		Symbol:  "SOVR",
		DenomUnits: []*bankv1beta1.DenomUnit{
			{Denom: "usovr", Exponent: 0},
			{Denom: "sovr", Exponent: 6},
		},
	}
}

// Assetlist denom drift fails the rule even offline (the on-chain half never
// runs), because the registry file itself disagrees with the manifest.
func TestRule9FailsOnAssetlistDenomMismatch(t *testing.T) {
	cases := map[string]struct {
		asset  string
		detail string
	}{
		"exponent": {
			`{"assets":[{"base":"usovr","display":"sovr","symbol":"SOVR",` +
				`"denom_units":[{"denom":"usovr","exponent":0},{"denom":"sovr","exponent":8}]}]}`,
			"exponent",
		},
		"symbol": {
			`{"assets":[{"base":"usovr","display":"sovr","symbol":"WRONG",` +
				`"denom_units":[{"denom":"usovr","exponent":0},{"denom":"sovr","exponent":6}]}]}`,
			"symbol",
		},
		"base": {
			`{"assets":[{"base":"uwrong","display":"sovr","symbol":"SOVR",` +
				`"denom_units":[{"denom":"uwrong","exponent":0},{"denom":"sovr","exponent":6}]}]}`,
			"base",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// A failing on-chain factory would still make the point, but assetlist
			// drift must fail before the network is ever consulted.
			withDenomClient(t, func(string) (denomMetadataClient, error) {
				t.Fatal("on-chain read must not run once the assetlist already mismatches")
				return nil, nil
			})
			v := rule9Verifier(t, false, goodChainJSON, tc.asset)
			v.rule9(context.Background())
			r := rule9Result(t, v)
			require.Equal(t, ruleFail, r.Status, r.Detail)
			require.Contains(t, r.Detail, tc.detail)
		})
	}
}

func TestRule9PassesWhenOnChainMetadataMatches(t *testing.T) {
	withDenomClient(t, fakeDenomFactory(matchingOnChainMetadata(), nil))
	v := rule9Verifier(t, false, goodChainJSON, goodAssetJSON)
	v.rule9(context.Background())
	r := rule9Result(t, v)
	require.Equal(t, rulePass, r.Status, r.Detail)
	require.Contains(t, r.Detail, "present and matches")
}

func TestRule9FailsWhenOnChainMetadataMismatches(t *testing.T) {
	md := matchingOnChainMetadata()
	md.Symbol = "WRONG"
	md.DenomUnits[1].Exponent = 8
	withDenomClient(t, fakeDenomFactory(md, nil))
	v := rule9Verifier(t, false, goodChainJSON, goodAssetJSON)
	v.rule9(context.Background())
	r := rule9Result(t, v)
	require.Equal(t, ruleFail, r.Status, r.Detail)
	require.Contains(t, r.Detail, "mismatch")
}

// The plan-D1 denom-metadata upgrade has landed on mainnet and testnet, so the
// hard gate is active: absent on-chain metadata (not-found error, empty record,
// or nil-no-error) now FAILS the rule — it means the node is behind the D1
// height or the manifest denom is wrong.
func TestRule9FailsWhenOnChainMetadataAbsent(t *testing.T) {
	cases := map[string]struct {
		md  *bankv1beta1.Metadata
		err error
	}{
		"not-found":    {nil, fmt.Errorf("denom metadata: %w", client.ErrNotFound)},
		"empty-record": {&bankv1beta1.Metadata{}, nil},
		"nil-no-error": {nil, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			withDenomClient(t, fakeDenomFactory(tc.md, tc.err))
			v := rule9Verifier(t, false, goodChainJSON, goodAssetJSON)
			v.rule9(context.Background())
			r := rule9Result(t, v)
			require.Equal(t, ruleFail, r.Status, r.Detail)
			require.Contains(t, r.Detail, "must be present")
		})
	}
}

// A transport/query failure is unverifiable and must fail the rule — never a
// pass, never described as an absent gap.
func TestRule9FailsOnOnChainTransportError(t *testing.T) {
	t.Run("query-error", func(t *testing.T) {
		withDenomClient(t, fakeDenomFactory(nil, errors.New("connection refused")))
		v := rule9Verifier(t, false, goodChainJSON, goodAssetJSON)
		v.rule9(context.Background())
		r := rule9Result(t, v)
		require.Equal(t, ruleFail, r.Status, r.Detail)
		require.Contains(t, r.Detail, "unverifiable")
		require.NotContains(t, r.Detail, "documented gap")
	})
	t.Run("connect-error", func(t *testing.T) {
		withDenomClient(t, func(string) (denomMetadataClient, error) {
			return nil, errors.New("dial tcp 203.0.113.1:26657: connect: refused")
		})
		v := rule9Verifier(t, false, goodChainJSON, goodAssetJSON)
		v.rule9(context.Background())
		r := rule9Result(t, v)
		require.Equal(t, ruleFail, r.Status, r.Detail)
		require.Contains(t, r.Detail, "unverifiable")
	})
}

// --- Rule 11: account_prefix matches the live chain ---

func rule11Result(t *testing.T, v *verifier) ruleResult {
	t.Helper()
	for _, r := range v.report.Rules {
		if r.Rule == 11 {
			return r
		}
	}
	t.Fatal("rule 11 missing from report")
	return ruleResult{}
}

// withOperatorPrefixFetcher swaps the rule-11 live prefix source for the test.
func withOperatorPrefixFetcher(t *testing.T, fn func(context.Context, string) (string, error)) {
	t.Helper()
	prev := fetchLiveOperatorPrefix
	fetchLiveOperatorPrefix = fn
	t.Cleanup(func() { fetchLiveOperatorPrefix = prev })
}

// rule11Verifier is a mainnet-static verifier (account_prefix "sovr"); rule 11
// reads no files, so no on-disk fixture is needed.
func rule11Verifier(offline bool) *verifier {
	return &verifier{offline: offline, m: manifestFromStatic(staticMainnet, "v0.16.2", "0.001usovr")}
}

func TestRule11PassesWhenLivePrefixMatches(t *testing.T) {
	withOperatorPrefixFetcher(t, func(context.Context, string) (string, error) {
		return "sovrvaloper", nil
	})
	v := rule11Verifier(false)
	v.rule11(context.Background())
	r := rule11Result(t, v)
	require.Equal(t, rulePass, r.Status, r.Detail)
	require.Contains(t, r.Detail, "matches the live chain")
}

// A chain whose real account prefix differs from the manifest fails — this is
// the whole point of the rule 9 registry-file check not being enough.
func TestRule11FailsWhenLivePrefixDiffers(t *testing.T) {
	withOperatorPrefixFetcher(t, func(context.Context, string) (string, error) {
		return "cosmosvaloper", nil
	})
	v := rule11Verifier(false)
	v.rule11(context.Background())
	r := rule11Result(t, v)
	require.Equal(t, ruleFail, r.Status, r.Detail)
	require.Contains(t, r.Detail, `"cosmos"`)
	require.Contains(t, r.Detail, "!= manifest account_prefix")
}

// A live HRP that is not a <prefix>valoper address is rejected, not silently
// treated as a prefix match.
func TestRule11FailsOnNonValoperHRP(t *testing.T) {
	withOperatorPrefixFetcher(t, func(context.Context, string) (string, error) {
		return "sovr", nil
	})
	v := rule11Verifier(false)
	v.rule11(context.Background())
	r := rule11Result(t, v)
	require.Equal(t, ruleFail, r.Status, r.Detail)
	require.Contains(t, r.Detail, "not a <account_prefix>valoper")
}

// An unverifiable live read fails the rule — never a pass.
func TestRule11FailsWhenLiveReadUnverifiable(t *testing.T) {
	withOperatorPrefixFetcher(t, func(context.Context, string) (string, error) {
		return "", errors.New("connection refused")
	})
	v := rule11Verifier(false)
	v.rule11(context.Background())
	r := rule11Result(t, v)
	require.Equal(t, ruleFail, r.Status, r.Detail)
	require.Contains(t, r.Detail, "unverifiable")
}

func TestRule11OfflineSkips(t *testing.T) {
	withOperatorPrefixFetcher(t, func(context.Context, string) (string, error) {
		t.Fatal("offline rule 11 must not touch the network")
		return "", nil
	})
	v := rule11Verifier(true)
	v.rule11(context.Background())
	r := rule11Result(t, v)
	require.Equal(t, ruleSkip, r.Status)
}

// The --offline path skips the network rule: it must pass on a good registry
// without ever building the on-chain client.
func TestRule9OfflineSkipsOnChainRead(t *testing.T) {
	withDenomClient(t, func(string) (denomMetadataClient, error) {
		t.Fatal("offline rule 9 must not touch the network")
		return nil, nil
	})
	v := rule9Verifier(t, true, goodChainJSON, goodAssetJSON)
	v.rule9(context.Background())
	r := rule9Result(t, v)
	require.Equal(t, rulePass, r.Status, r.Detail)
}
