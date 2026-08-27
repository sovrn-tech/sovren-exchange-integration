// Command sovren-cert is the Sovren Technical Integration Certification suite
// — it certifies the exchange's technical integration only, never the
// exchange's compliance, custody, or legal posture (contract:
// specs/008-exchange-integration-kit/contracts/certification.md; data model
// §13). It drives the kit's runnable adapter components and both client
// libraries against a target network and emits a machine-readable JSON
// report plus a rendered Markdown certification report.
//
// Usage:
//
//	sovren-cert run  --network testnet|local --manifest <network.yaml> \
//	                 --adapter-config <adapter.yaml> --report-dir ./report \
//	                 [--scenario <id> ...] [--scenario-dir <dir>]
//	sovren-cert fund --address sovr1…  [--network testnet|local] [--manifest <network.yaml>]
//	sovren-cert render --report-dir ./report
//
// A certification report is only valid from --network testnet; --network
// local targets a local dev chain and the report is marked
// environment mode "local", non-certifying.
//
// Chain-dependent scenario groups (D, R, W, S and parts of N) need an
// isolated throwaway chain plus a funded test key:
//
//	export SOVREN_CERT_CHAIN_RPC=http://127.0.0.1:26657
//	export SOVREN_CERT_MNEMONIC="<funded test mnemonic at m/44'/118'/0'/0/0>"  # UNSAFE_TEST_ONLY
//
// Without them those scenarios report SKIPPED (local mode) or
// BLOCKED(D2)/BLOCKED(D3) (testnet mode — plan.md dependencies: D2 testnet
// faucet, D3 testnet manifest). The suite never runs against mainnet
// (chain_id sovr-1 is refused).
//
// Exit codes: 0 all pass; 1 any FAIL; 2 environment not ready (preflight);
// 3 internal error.
package main

import (
	"fmt"
	"os"
)

// Version / Commit are stamped via -ldflags at release build time.
var (
	Version = "dev"
	Commit  = "none"
)

const (
	exitOK        = 0
	exitFail      = 1
	exitPreflight = 2
	exitInternal  = 3
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitPreflight
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "fund":
		return cmdFund(args[1:])
	case "render":
		return cmdRender(args[1:])
	case "-h", "--help", "help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "sovren-cert: unknown command %q\n", args[0])
		usage()
		return exitPreflight
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `sovren-cert — Sovren Technical Integration Certification suite

Commands:
  run     Run certification scenarios and write report/certification.json + .md
  fund    Request testnet funds for an address from the manifest's faucet
  render  Re-render report/certification.md from report/certification.json

Run:
  sovren-cert run --network testnet|local [--manifest network.yaml]
                  [--adapter-config adapter.yaml] [--report-dir ./report]
                  [--scenario <id> ...] [--scenario-dir <dir>]
                  [--kit-root <dir>] [--operator <name>] [--exchange <name>]

Environment (chain-dependent scenarios — groups D, R, W, S, and N2):
  SOVREN_CERT_CHAIN_RPC   CometBFT RPC of an isolated throwaway chain
  SOVREN_CERT_MNEMONIC    funded test mnemonic (m/44'/118'/0'/0/0; UNSAFE_TEST_ONLY)
  SOVREN_CERT_GAS_PRICE   usovr per gas (default 0.025)

Exit codes: 0 all pass; 1 any FAIL; 2 environment not ready; 3 internal error.
`)
}
