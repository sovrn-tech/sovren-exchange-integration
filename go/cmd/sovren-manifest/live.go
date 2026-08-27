package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
)

const (
	liveCallTimeout = 15 * time.Second
	dialTimeout     = 8 * time.Second
	// Public endpoints sit behind a shared gateway that intermittently
	// returns 5xx; bounded retries keep transient flakes from failing a
	// release gate while real outages still fail.
	httpAttempts  = 3
	httpRetryWait = 2 * time.Second
)

var httpClient = &http.Client{Timeout: liveCallTimeout}

type transientHTTPError struct{ err error }

func (e *transientHTTPError) Error() string { return e.err.Error() }
func (e *transientHTTPError) Unwrap() error { return e.err }

// withRetries retries fn on network errors and HTTP 5xx (transientHTTPError).
func withRetries(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 1; attempt <= httpAttempts; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}
		var transient *transientHTTPError
		if !errors.As(err, &transient) {
			return err
		}
		if attempt < httpAttempts {
			select {
			case <-time.After(httpRetryWait):
			case <-ctx.Done():
				return err
			}
		}
	}
	return err
}

// rpcStatus is the subset of CometBFT /status the verifier consumes.
type rpcStatus struct {
	ChainID         string
	CometBFTVersion string
}

func fetchRPCStatus(ctx context.Context, rpcURL string) (*rpcStatus, error) {
	var payload struct {
		Result struct {
			NodeInfo struct {
				Network string `json:"network"`
				Version string `json:"version"`
			} `json:"node_info"`
		} `json:"result"`
	}
	if err := getJSON(ctx, strings.TrimRight(rpcURL, "/")+"/status", &payload); err != nil {
		return nil, err
	}
	if payload.Result.NodeInfo.Network == "" {
		return nil, fmt.Errorf("%s/status: empty node_info.network", rpcURL)
	}
	return &rpcStatus{
		ChainID:         payload.Result.NodeInfo.Network,
		CometBFTVersion: payload.Result.NodeInfo.Version,
	}, nil
}

// restNodeInfo is the subset of REST node_info the verifier consumes. GoVersion
// and BuildDeps come from the node's application_version — the versions the
// RUNNING binary was actually built with (rule 2 cross-checks the manifest pins
// against them, so a stale/mismatched pin cannot pass unnoticed).
type restNodeInfo struct {
	ChainID    string
	AppVersion string
	SDKVersion string
	GoVersion  string            // raw application_version.go_version, e.g. "go version go1.25.12 linux/amd64"
	BuildDeps  map[string]string // go-module path -> version (from application_version.build_deps)
}

func fetchRESTNodeInfo(ctx context.Context, restURL string) (*restNodeInfo, error) {
	var payload struct {
		DefaultNodeInfo struct {
			Network string `json:"network"`
		} `json:"default_node_info"`
		ApplicationVersion struct {
			Version          string `json:"version"`
			CosmosSDKVersion string `json:"cosmos_sdk_version"`
			GoVersion        string `json:"go_version"`
			BuildDeps        []struct {
				Path    string `json:"path"`
				Version string `json:"version"`
			} `json:"build_deps"`
		} `json:"application_version"`
	}
	url := strings.TrimRight(restURL, "/") + "/cosmos/base/tendermint/v1beta1/node_info"
	if err := getJSON(ctx, url, &payload); err != nil {
		return nil, err
	}
	if payload.ApplicationVersion.Version == "" {
		return nil, fmt.Errorf("%s: empty application_version.version", url)
	}
	deps := make(map[string]string, len(payload.ApplicationVersion.BuildDeps))
	for _, d := range payload.ApplicationVersion.BuildDeps {
		if d.Path != "" {
			deps[d.Path] = d.Version
		}
	}
	return &restNodeInfo{
		ChainID:    payload.DefaultNodeInfo.Network,
		AppVersion: payload.ApplicationVersion.Version,
		SDKVersion: payload.ApplicationVersion.CosmosSDKVersion,
		GoVersion:  payload.ApplicationVersion.GoVersion,
		BuildDeps:  deps,
	}, nil
}

// buildDep returns the version of the first build_deps entry whose module path
// starts with pathPrefix (a prefix so a major-version bump like ibc-go/v10 ->
// /v11 still resolves).
func (ni *restNodeInfo) buildDep(pathPrefix string) (string, bool) {
	for p, v := range ni.BuildDeps {
		if strings.HasPrefix(p, pathPrefix) {
			return v, true
		}
	}
	return "", false
}

var goVersionRE = regexp.MustCompile(`go(\d+\.\d+(?:\.\d+)?)`)
var bareVersionRE = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?`)

// extractGoVersion pulls "1.25.12" out of the node's go_version, which may be
// "go version go1.25.12 linux/amd64", "go1.25.12", or a bare "1.25.12".
func extractGoVersion(s string) string {
	if m := goVersionRE.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if m := bareVersionRE.FindString(s); m != "" {
		return m
	}
	return strings.TrimSpace(s)
}

// checkLiveBuildDeps cross-checks the manifest's ibc_go / cosmwasm_wasmd / go
// pins against the RUNNING binary's application_version (rule 2 live half):
//   - ibc_go, cosmwasm_wasmd must equal the live build_deps entry EXACTLY (a
//     stale pin or a node on a different build is a hard mismatch);
//   - go must be on the live binary's minor line (the go.mod directive is a
//     minimum floor, so a newer toolchain patch is a recorded note, not a fail;
//     a different minor line IS a fail).
// Missing build info (stripped binary / absent dep) degrades to a note — the
// caller has already checked the go.mod pin. Returns hard problems + notes.
func checkLiveBuildDeps(ibcGoPin, wasmdPin, goPin string, ni *restNodeInfo) (problems, notes []string) {
	checkDep := func(label, pathPrefix, want string) {
		if len(ni.BuildDeps) == 0 {
			notes = append(notes, label+": live build_deps unavailable (stripped binary?) — verified against the go.mod pin only")
			return
		}
		got, ok := ni.buildDep(pathPrefix)
		if !ok {
			notes = append(notes, label+": absent from live build_deps — verified against the go.mod pin only")
			return
		}
		if !versionsEqual(want, got) {
			problems = append(problems, fmt.Sprintf("versions.%s %q != live build_deps %q", label, want, got))
		}
	}
	checkDep("ibc_go", "github.com/cosmos/ibc-go/", ibcGoPin)
	checkDep("cosmwasm_wasmd", "github.com/CosmWasm/wasmd", wasmdPin)

	if lg := extractGoVersion(ni.GoVersion); lg != "" {
		ml, mlErr := minorLine(goPin)
		ll, llErr := minorLine(lg)
		switch {
		case mlErr != nil:
			problems = append(problems, mlErr.Error())
		case llErr != nil:
			problems = append(problems, "live go version unparseable: "+lg)
		case ml != ll:
			problems = append(problems, fmt.Sprintf("versions.go %q not on the live binary's go minor line (%q)", goPin, lg))
		case !versionsEqual(goPin, lg):
			notes = append(notes, fmt.Sprintf("go: manifest pins %s (go.mod directive); live binary built with go%s — same %s line, newer toolchain patch", goPin, lg, ml))
		}
	} else {
		// go_version absent (stripped binary / older node): record the
		// degradation so the PASS detail never implies go was verified live.
		notes = append(notes, "go: live go_version unavailable (stripped binary?) — verified against the go.mod pin only")
	}
	return problems, notes
}

// fetchGlobalFeeFloor reads the live x/globalfee floor through the kit client
// (abci_query tunnel over CometBFT RPC) and formats it as a dec-coin string.
func fetchGlobalFeeFloor(ctx context.Context, rpcURL string) (string, error) {
	c, err := client.NewCometRPC(rpcURL, client.WithTimeout(liveCallTimeout))
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	params, err := c.GlobalFeeParams(ctx)
	if err != nil {
		return "", fmt.Errorf("globalfee params via %s: %w", rpcURL, err)
	}
	if len(params.MinimumGasPrices) != 1 {
		return "", fmt.Errorf("globalfee floor has %d coins, expected exactly 1", len(params.MinimumGasPrices))
	}
	dc := params.MinimumGasPrices[0]
	return trimDec(dc.Amount.String()) + dc.Denom, nil
}

// fetchLiveChainID reads chain_id through the kit client's NodeStatus.
func fetchLiveChainID(ctx context.Context, rpcURL string) (string, error) {
	c, err := client.NewCometRPC(rpcURL, client.WithTimeout(liveCallTimeout))
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	st, err := c.NodeStatus(ctx)
	if err != nil {
		return "", fmt.Errorf("node status via %s: %w", rpcURL, err)
	}
	return st.ChainID, nil
}

// fetchLiveOperatorPrefix returns the bech32 human-readable prefix of a live
// validator operator address (e.g. "sovrvaloper") read from the staking REST
// endpoint. The HRP is decoded from the address itself — never string-matched —
// so a chain that quietly changed its account prefix cannot slip past. Injected
// as a package var so rule 11 can be unit-tested without a node.
var fetchLiveOperatorPrefix = func(ctx context.Context, restURL string) (string, error) {
	var payload struct {
		Validators []struct {
			OperatorAddress string `json:"operator_address"`
		} `json:"validators"`
	}
	// One bonded validator is enough; a live chain always has at least one.
	url := strings.TrimRight(restURL, "/") +
		"/cosmos/staking/v1beta1/validators?status=BOND_STATUS_BONDED&pagination.limit=1"
	if err := getJSON(ctx, url, &payload); err != nil {
		return "", err
	}
	if len(payload.Validators) == 0 || payload.Validators[0].OperatorAddress == "" {
		return "", fmt.Errorf("%s: no bonded validators returned", url)
	}
	oper := payload.Validators[0].OperatorAddress
	hrp, _, err := bech32.DecodeAndConvert(oper)
	if err != nil {
		return "", fmt.Errorf("decode operator_address %q: %w", oper, err)
	}
	return hrp, nil
}

func getJSON(ctx context.Context, url string, out any) error {
	return withRetries(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			return &transientHTTPError{err: err}
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 500 {
			return &transientHTTPError{err: fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)}
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return &transientHTTPError{err: err}
		}
		return json.Unmarshal(body, out)
	})
}

// checkLink passes when the URL answers with any status < 400 (rule 7).
func checkLink(ctx context.Context, url string) error {
	return withRetries(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return &transientHTTPError{err: err}
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 500 {
			return &transientHTTPError{err: fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)}
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	})
}

// peerHostPort splits "nodeid@host:port" and rejects raw-IP hosts (rule 6).
func peerHostPort(peer string) (string, error) {
	_, addr, ok := strings.Cut(peer, "@")
	if !ok {
		return "", fmt.Errorf("%q is not nodeid@host:port", peer)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("%q: %w", peer, err)
	}
	if net.ParseIP(host) != nil {
		return "", fmt.Errorf("%q uses a raw IP; DNS names are required on both networks", peer)
	}
	if !strings.Contains(host, ".") {
		return "", fmt.Errorf("%q host is not a DNS name", peer)
	}
	return addr, nil
}

// dialPeers TCP-dials every address concurrently; returns one error per
// undialable peer.
func dialPeers(addrs []string) []error {
	type result struct {
		addr string
		err  error
	}
	ch := make(chan result, len(addrs))
	for _, addr := range addrs {
		go func(addr string) {
			conn, err := net.DialTimeout("tcp", addr, dialTimeout)
			if err == nil {
				_ = conn.Close()
			}
			ch <- result{addr: addr, err: err}
		}(addr)
	}
	var errs []error
	for range addrs {
		r := <-ch
		if r.err != nil {
			errs = append(errs, fmt.Errorf("dial %s: %w", r.addr, r.err))
		}
	}
	return errs
}

// trimDec removes trailing fractional zeros ("0.001000000000000000" → "0.001").
func trimDec(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// versionsEqual compares release versions ignoring a leading "v".
func versionsEqual(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// minorLine returns "MAJOR.MINOR" of a version like "v0.38.21" or "0.38.19".
func minorLine(v string) (string, error) {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) < 2 {
		return "", errors.New("not a MAJOR.MINOR.PATCH version: " + v)
	}
	return parts[0] + "." + parts[1], nil
}
