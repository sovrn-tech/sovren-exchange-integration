package main

// `sovren-cert fund` — faucet helper (contracts/certification.md). Reads
// links.faucet from the network manifest and requests funds for an address —
// by default the SOVREN_CERT_MNEMONIC key at m/44'/118'/0'/0/0, so the funded
// key is exactly the one the chain-dependent scenarios use.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
)

func cmdFund(args []string) int {
	fs := flag.NewFlagSet("sovren-cert fund", flag.ContinueOnError)
	addr := fs.String("address", "", "sovr1… address to fund (default: the SOVREN_CERT_MNEMONIC key at m/44'/118'/0'/0/0)")
	network := fs.String("network", "testnet", "target network: testnet | local")
	manifestPath := fs.String("manifest", "", "network manifest path (default: <kit-root>/network/<network>/network.yaml)")
	kitRoot := fs.String("kit-root", "", "exchange-kit root (default: auto-detect)")
	if err := fs.Parse(args); err != nil {
		return exitPreflight
	}

	// Default to the SOVREN_CERT_MNEMONIC key so `sovren-cert fund` funds the
	// exact address the chain-dependent scenarios sign with (the provisioning
	// flow: export the mnemonic, fund, run).
	targetAddr := *addr
	if targetAddr == "" {
		mnemonic := os.Getenv(envMnemonic)
		if mnemonic == "" {
			fmt.Fprintln(os.Stderr, "sovren-cert fund: pass --address, or export "+envMnemonic+" to fund its m/44'/118'/0'/0/0 address")
			return exitPreflight
		}
		a, err := address.DeriveAddress(mnemonic, address.DefaultHDPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sovren-cert fund: deriving address from %s: %v\n", envMnemonic, err)
			return exitPreflight
		}
		targetAddr = a.Bech32
		fmt.Fprintf(os.Stderr, "sovren-cert fund: funding the %s key %s\n", envMnemonic, targetAddr)
	}
	if res := address.ValidateAccountAddress(targetAddr); !res.Valid {
		fmt.Fprintf(os.Stderr, "sovren-cert fund: invalid address: %s (%s)\n", res.ErrorCode, res.ErrorMessage)
		return exitPreflight
	}

	path := *manifestPath
	if path == "" {
		root, err := findKitRoot(*kitRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sovren-cert fund: %v\n", err)
			return exitPreflight
		}
		path = filepath.Join(root, "network", *network, "network.yaml")
	}

	m, err := client.LoadManifest(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sovren-cert fund: could not load the %s manifest at %s: %v\n%s\n", *network, path, err, fundFallbackHelp)
		return exitPreflight
	}

	faucet := ""
	if m.Links.Faucet != nil {
		faucet = *m.Links.Faucet
	}
	if faucet == "" {
		fmt.Fprintf(os.Stderr, "sovren-cert fund: the %s manifest declares no faucet (links.faucet is empty).\n%s\n",
			m.ChainID, fundFallbackHelp)
		return exitPreflight
	}

	if err := requestFaucet(faucet, targetAddr, m.BaseDenom); err != nil {
		fmt.Fprintf(os.Stderr, "sovren-cert fund: faucet request failed: %v\n", err)
		return exitInternal
	}
	fmt.Printf("sovren-cert fund: requested %s funds for %s from %s\n", m.BaseDenom, targetAddr, faucet)
	return exitOK
}

const fundFallbackHelp = "Provide a manifest whose links.faucet points at a live faucet, or fund the\n" +
	"address manually (e.g. from a locally controlled funded key) and run the\n" +
	"chain-dependent scenarios via SOVREN_CERT_CHAIN_RPC + SOVREN_CERT_MNEMONIC."

// requestFaucet posts a cosmjs-faucet-compatible credit request.
func requestFaucet(baseURL, addr, denom string) error {
	body, err := json.Marshal(map[string]string{"address": addr, "denom": denom})
	if err != nil {
		return err
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Post(faucetCreditURL(baseURL), "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("faucet returned %s: %s", resp.Status, string(payload))
	}
	return nil
}

func faucetCreditURL(baseURL string) string {
	for len(baseURL) > 0 && baseURL[len(baseURL)-1] == '/' {
		baseURL = baseURL[:len(baseURL)-1]
	}
	return baseURL + "/credit"
}
