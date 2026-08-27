// Command sovren-manifest generates and verifies the kit's network manifests
// (contracts/network-manifest.md).
//
//	sovren-manifest generate --network mainnet|testnet --out <dir> [--rpc URL] [--rest URL]
//	sovren-manifest verify   --manifest <network.yaml> [--offline] [--genesis <path>]
//
// generate merges checked-in verified values (static.go) with live-chain
// reads (chain_id, app/sdk versions, x/globalfee floor) and writes
// network.yaml plus sidecars derived from it. verify machine-checks contract
// rules 1-10 and prints a JSON report to stdout; exit is non-zero on any rule
// failure. --offline skips the network-dependent checks (CI without egress).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	switch args[0] {
	case "generate":
		return cmdGenerate(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  sovren-manifest generate --network mainnet|testnet --out <dir> [--rpc URL] [--rest URL]
  sovren-manifest verify   --manifest <network.yaml> [--offline] [--genesis <path>]
`)
}

func cmdGenerate(args []string) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	network := fs.String("network", "", "mainnet|testnet")
	out := fs.String("out", "", "output directory for network.yaml + sidecars")
	rpc := fs.String("rpc", "", "override the bootstrap RPC endpoint")
	rest := fs.String("rest", "", "override the bootstrap REST endpoint")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *network == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "generate: --network and --out are required")
		return 2
	}
	res, err := runGenerate(*network, *out, *rpc, *rest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate: %v\n", err)
		return 1
	}
	emitJSON(res)
	return 0
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "path to network.yaml")
	offline := fs.Bool("offline", false, "skip network-dependent rules (records them as SKIP)")
	genesis := fs.String("genesis", "", "genesis file to hash for rule 4 (default: <manifest dir>/<genesis.file> when present)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *manifest == "" {
		fmt.Fprintln(os.Stderr, "verify: --manifest is required")
		return 2
	}
	report, err := runVerify(*manifest, *genesis, *offline)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: %v\n", err)
		return 2
	}
	emitJSON(report)
	if !report.Pass {
		return 1
	}
	return 0
}

func emitJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
