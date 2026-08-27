// sovren-vectors — deterministic test-vector tooling (contracts/test-vectors.md).
//
// Subcommands:
//
//	generate --out <dir>          write the four US3 vector files (byte-deterministic)
//	verify --dir <dir>            regenerate to a temp dir and byte-compare
//	derive --new-test-address     print a fresh random UNSAFE_TEST_ONLY address (nondeterministic)
//	conformance --dir --out       run the Go kit over every committed vector, write results JSON
//	compare --dir --a --b         diff two results files field-by-field against the vector suites
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	bip39 "github.com/cosmos/go-bip39"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	var err error
	switch args[0] {
	case "generate":
		err = cmdGenerate(args[1:], stdout)
	case "verify":
		err = cmdVerify(args[1:], stdout)
	case "derive":
		err = cmdDerive(args[1:], stdout)
	case "conformance":
		err = cmdConformance(args[1:], stdout)
	case "compare":
		err = cmdCompare(args[1:], stdout)
	default:
		usage(stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "sovren-vectors %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: sovren-vectors <generate --out DIR | verify --dir DIR | derive --new-test-address | conformance --dir DIR --out FILE | compare --dir DIR --a FILE --b FILE>")
}

func cmdGenerate(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("--out is required")
	}
	if err := GenerateAll(*out); err != nil {
		return err
	}
	for _, f := range VectorFiles {
		fmt.Fprintf(stdout, "wrote %s\n", f)
	}
	return nil
}

func cmdVerify(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory holding committed vector files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return fmt.Errorf("--dir is required")
	}
	tmp, err := os.MkdirTemp("", "sovren-vectors-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := GenerateAll(tmp); err != nil {
		return err
	}
	mismatches, err := CompareDirs(tmp, *dir)
	if err != nil {
		return err
	}
	if len(mismatches) > 0 {
		for _, m := range mismatches {
			fmt.Fprintf(stdout, "MISMATCH %s\n", m)
		}
		return fmt.Errorf("%d vector file(s) are not byte-identical to regeneration", len(mismatches))
	}
	fmt.Fprintf(stdout, "verify: %d vector files byte-identical\n", len(VectorFiles))
	return nil
}

// cmdDerive is the one intentionally nondeterministic subcommand: it mints a
// fresh random UNSAFE_TEST_ONLY address (for faucet funding in integration
// tests) and prints the mnemonic that re-derives it.
func cmdDerive(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("derive", flag.ContinueOnError)
	newAddr := fs.Bool("new-test-address", false, "generate a fresh random test address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*newAddr {
		return fmt.Errorf("--new-test-address is required")
	}
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return err
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return err
	}
	a, err := address.DeriveAddress(mnemonic, address.DefaultHDPath)
	if err != nil {
		return err
	}
	out := struct {
		UnsafeTestOnly bool   `json:"UNSAFE_TEST_ONLY"`
		Mnemonic       string `json:"mnemonic"`
		DerivationPath string `json:"derivation_path"`
		Bech32Address  string `json:"bech32_address"`
	}{true, mnemonic, a.Path, a.Bech32}
	enc, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, string(enc))
	return nil
}
