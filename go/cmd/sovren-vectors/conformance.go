package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/amounts"
)

// Suites in fixed processing order; ids are unique within a suite.
var conformanceSuites = append(
	[]string{"addresses", "derivation", "amounts", "invalid-cases"}, txSuites...)

var suiteFiles = map[string]string{
	"addresses":             "addresses.json",
	"derivation":            "derivation.json",
	"amounts":               "amounts.json",
	"invalid-cases":         "invalid-cases.json",
	"unsigned-transactions": "unsigned-transactions.json",
	"sign-documents":        "sign-documents.json",
	"signed-transactions":   "signed-transactions.json",
	"transaction-hashes":    "transaction-hashes.json",
}

// rawVector is the union read-shape over every US3 suite entry.
type rawVector struct {
	ID             string            `json:"id"`
	Mnemonic       string            `json:"mnemonic"`
	DerivationPath string            `json:"derivation_path"`
	Input          *string           `json:"input"`
	Prohibited     []string          `json:"prohibited"`
	Display        *string           `json:"display"`
	BaseUnits      string            `json:"base_units"`
	Valid          *bool             `json:"valid"`
	ErrorCode      string            `json:"error_code"`
	Category       string            `json:"category"`
	Vector         map[string]string `json:"vector"`
	Expected       map[string]string `json:"expected"`
	Validation     *validation       `json:"validation"`

	PrivateKeyHex          string `json:"private_key_hex"`
	PublicKeyCompressedHex string `json:"public_key_compressed_hex"`
	AccountAddressBytesHex string `json:"account_address_bytes_hex"`
	Bech32Address          string `json:"bech32_address"`

	// Transaction-suite fields (vectors_tx.go).
	Kind             string       `json:"kind"`
	ChainID          string       `json:"chain_id"`
	AccountNumber    string       `json:"account_number"`
	Sequence         string       `json:"sequence"`
	From             string       `json:"from"`
	To               string       `json:"to"`
	AmountBaseUnits  string       `json:"amount_base_units"`
	FeeBaseUnits     string       `json:"fee_base_units"`
	GasLimit         string       `json:"gas_limit"`
	Memo             string       `json:"memo"`
	SignerMnemonic   string       `json:"signer_mnemonic"`
	SignerHDPath     string       `json:"signer_hd_path"`
	BodyBytesHex     string       `json:"body_bytes_hex"`
	AuthInfoBytesHex string       `json:"auth_info_bytes_hex"`
	SignDocBytesHex  string       `json:"sign_doc_bytes_hex"`
	Summary          *summaryJSON `json:"summary"`
	SignatureHex     string       `json:"signature_hex"`
	TxRawBytesHex    string       `json:"tx_raw_bytes_hex"`
	TxHash           string       `json:"tx_hash"`
}

type conformanceResult struct {
	ID     string            `json:"id"`
	Suite  string            `json:"suite"`
	Fields map[string]string `json:"fields"`
}

type resultsFile struct {
	Kit     string              `json:"kit"`
	Results []conformanceResult `json:"results"`
}

func loadSuite(dir, suite string) ([]rawVector, error) {
	data, err := os.ReadFile(filepath.Join(dir, suiteFiles[suite]))
	if err != nil {
		return nil, err
	}
	var env struct {
		SchemaVersion  int         `json:"schema_version"`
		UnsafeTestOnly bool        `json:"UNSAFE_TEST_ONLY"`
		Vectors        []rawVector `json:"vectors"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%s: %w", suiteFiles[suite], err)
	}
	if env.SchemaVersion != schemaVersion {
		return nil, fmt.Errorf("%s: schema_version %d, expected %d", suiteFiles[suite], env.SchemaVersion, schemaVersion)
	}
	if !env.UnsafeTestOnly {
		return nil, fmt.Errorf("%s: missing UNSAFE_TEST_ONLY marker", suiteFiles[suite])
	}
	return env.Vectors, nil
}

func cmdConformance(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("conformance", flag.ContinueOnError)
	dir := fs.String("dir", "", "vector directory")
	out := fs.String("out", "", "results output file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *out == "" {
		return fmt.Errorf("--dir and --out are required")
	}
	results, err := runGoConformance(*dir)
	if err != nil {
		return err
	}
	enc, err := json.MarshalIndent(resultsFile{Kit: "go", Results: results}, "", "  ")
	if err != nil {
		return err
	}
	enc = append(enc, '\n')
	if err := os.WriteFile(*out, enc, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "go conformance: %d results\n", len(results))
	return nil
}

func runGoConformance(dir string) ([]conformanceResult, error) {
	var results []conformanceResult
	for _, suite := range conformanceSuites {
		vectors, err := loadSuite(dir, suite)
		if err != nil {
			return nil, err
		}
		for _, v := range vectors {
			fields, err := runGoVector(suite, v)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", suite, v.ID, err)
			}
			results = append(results, conformanceResult{ID: v.ID, Suite: suite, Fields: fields})
		}
	}
	return results, nil
}

func runGoVector(suite string, v rawVector) (map[string]string, error) {
	if isTxSuite(suite) {
		return runGoTxVector(suite, v)
	}
	switch suite {
	case "addresses", "derivation":
		if v.Mnemonic != "" {
			return goDeriveFields(v)
		}
		if v.Input == nil {
			return nil, fmt.Errorf("entry has neither mnemonic nor input")
		}
		return goValidateFields(*v.Input, v.Prohibited), nil
	case "amounts":
		return goAmountFields(v)
	case "invalid-cases":
		return goInvalidCaseFields(v)
	}
	return nil, fmt.Errorf("unknown suite %s", suite)
}

func goDeriveFields(v rawVector) (map[string]string, error) {
	a, err := address.DeriveAddress(v.Mnemonic, v.DerivationPath)
	if err != nil {
		return map[string]string{"error": err.Error()}, nil
	}
	fields := map[string]string{
		"private_key_hex":           hex.EncodeToString(a.PrivateKey),
		"public_key_compressed_hex": hex.EncodeToString(a.PublicKeyCompressed),
		"account_address_bytes_hex": hex.EncodeToString(a.AddressBytes),
		"bech32_address":            a.Bech32,
	}
	res := address.ValidateAccountAddress(a.Bech32)
	fields["valid"] = strconv.FormatBool(res.Valid)
	fields["normalized"] = res.NormalizedAddress
	fields["error_code"] = res.ErrorCode
	return fields, nil
}

func goValidateFields(input string, prohibited []string) map[string]string {
	var res address.ValidationResult
	if prohibited != nil {
		res = address.ValidateAccountAddressStrict(input, address.NewPSet(prohibited...))
	} else {
		res = address.ValidateAccountAddress(input)
	}
	return map[string]string{
		"valid":      strconv.FormatBool(res.Valid),
		"normalized": res.NormalizedAddress,
		"error_code": res.ErrorCode,
	}
}

func goAmountFields(v rawVector) (map[string]string, error) {
	if v.Display == nil {
		return nil, fmt.Errorf("amount entry missing display")
	}
	base, err := amounts.DisplayToBaseUnits(*v.Display)
	if err != nil {
		return map[string]string{"error_code": amounts.CodeOf(err)}, nil
	}
	canonical, err := amounts.BaseToDisplayUnits(base)
	if err != nil {
		return nil, err
	}
	roundTrip, err := amounts.DisplayToBaseUnits(canonical)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"base_units":        base,
		"error_code":        "",
		"display_canonical": canonical,
		"roundtrip_base":    roundTrip,
	}, nil
}

func goInvalidCaseFields(v rawVector) (map[string]string, error) {
	switch v.Category {
	case "WRONG_BECH32_PREFIX", "INVALID_CHECKSUM", "VALIDATOR_OPERATOR_ADDRESS":
		res := address.ValidateAccountAddress(v.Vector["address"])
		return map[string]string{"valid": strconv.FormatBool(res.Valid), "error_code": res.ErrorCode}, nil
	case "ZERO_AMOUNT":
		base, err := amounts.DisplayToBaseUnits(v.Vector["amount_display"])
		if err != nil {
			return map[string]string{"error_code": amounts.CodeOf(err)}, nil
		}
		return map[string]string{"base_units": base}, nil
	case "NEGATIVE_AMOUNT", "EXCESS_DECIMALS":
		_, err := amounts.DisplayToBaseUnits(v.Vector["amount_display"])
		return map[string]string{"error_code": amounts.CodeOf(err)}, nil
	}
	return goTxInvalidCaseFields(v)
}

func cmdCompare(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	dir := fs.String("dir", "", "vector directory")
	a := fs.String("a", "", "first results file (reference kit)")
	b := fs.String("b", "", "second results file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *a == "" || *b == "" {
		return fmt.Errorf("--dir, --a and --b are required")
	}
	resA, err := loadResults(*a)
	if err != nil {
		return err
	}
	resB, err := loadResults(*b)
	if err != nil {
		return err
	}
	divergences, compared, err := compareResults(*dir, resA, resB)
	if err != nil {
		return err
	}
	for suite, n := range compared {
		fmt.Fprintf(stdout, "compare %-13s %d vectors\n", suite+":", n)
	}
	if len(divergences) > 0 {
		sort.Strings(divergences)
		for _, d := range divergences {
			fmt.Fprintf(stdout, "DIVERGENCE %s\n", d)
		}
		return fmt.Errorf("%d divergence(s)", len(divergences))
	}
	fmt.Fprintln(stdout, "compare: PASS (no divergences, full coverage)")
	return nil
}

func loadResults(path string) (map[string]conformanceResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rf resultsFile
	if err := json.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make(map[string]conformanceResult, len(rf.Results))
	for _, r := range rf.Results {
		key := r.Suite + "/" + r.ID
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("%s: duplicate result %s", path, key)
		}
		out[key] = r
	}
	return out, nil
}

// compareResults enforces three properties: (1) every committed vector id is
// covered by both kits, with no extras; (2) both kits produced field-identical
// output; (3) where the vector file itself pins expectations (validation
// results, base_units, error codes), both kits match them.
func compareResults(dir string, resA, resB map[string]conformanceResult) ([]string, map[string]int, error) {
	var divergences []string
	compared := map[string]int{}
	seen := map[string]bool{}
	for _, suite := range conformanceSuites {
		vectors, err := loadSuite(dir, suite)
		if err != nil {
			return nil, nil, err
		}
		for _, v := range vectors {
			key := suite + "/" + v.ID
			seen[key] = true
			a, okA := resA[key]
			b, okB := resB[key]
			if !okA || !okB {
				divergences = append(divergences, fmt.Sprintf("%s: uncovered (a=%v b=%v)", key, okA, okB))
				continue
			}
			compared[suite]++
			divergences = append(divergences, diffFields(key, a.Fields, b.Fields)...)
			divergences = append(divergences, checkExpectations(key, suite, v, a.Fields, b.Fields)...)
		}
	}
	for key := range resA {
		if !seen[key] {
			divergences = append(divergences, fmt.Sprintf("%s: result in %s has no committed vector", key, "a"))
		}
	}
	for key := range resB {
		if !seen[key] {
			divergences = append(divergences, fmt.Sprintf("%s: result in %s has no committed vector", key, "b"))
		}
	}
	return divergences, compared, nil
}

func diffFields(key string, a, b map[string]string) []string {
	var out []string
	names := map[string]bool{}
	for k := range a {
		names[k] = true
	}
	for k := range b {
		names[k] = true
	}
	sorted := make([]string, 0, len(names))
	for k := range names {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		va, okA := a[k]
		vb, okB := b[k]
		if !okA || !okB || va != vb {
			out = append(out, fmt.Sprintf("%s field %s: a=%q(present=%v) b=%q(present=%v)", key, k, va, okA, vb, okB))
		}
	}
	return out
}

func checkExpectations(key, suite string, v rawVector, fieldsA, fieldsB map[string]string) []string {
	expected := map[string]string{}
	switch {
	case isTxSuite(suite):
		expected = txExpectations(suite, v)
	case suite == "invalid-cases":
		if code := v.Expected["error_code"]; code != "" {
			expected["error_code"] = code
		}
		if base := v.Expected["base_units"]; base != "" {
			expected["base_units"] = base
		}
	case suite == "amounts":
		if v.Valid != nil && *v.Valid {
			expected["base_units"] = v.BaseUnits
			expected["roundtrip_base"] = v.BaseUnits
			expected["error_code"] = ""
		} else {
			expected["error_code"] = v.ErrorCode
		}
	case v.Mnemonic != "":
		expected["private_key_hex"] = v.PrivateKeyHex
		expected["public_key_compressed_hex"] = v.PublicKeyCompressedHex
		expected["account_address_bytes_hex"] = v.AccountAddressBytesHex
		expected["bech32_address"] = v.Bech32Address
		expected["valid"] = "true"
		if v.Validation != nil {
			expected["normalized"] = v.Validation.Normalized
		}
	default:
		if v.Validation == nil {
			return []string{fmt.Sprintf("%s: validation entry missing validation object", key)}
		}
		expected["valid"] = strconv.FormatBool(v.Validation.Valid)
		expected["normalized"] = v.Validation.Normalized
		expected["error_code"] = v.Validation.ErrorCode
	}
	var out []string
	keys := make([]string, 0, len(expected))
	for k := range expected {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		want := expected[k]
		if got := fieldsA[k]; got != want {
			out = append(out, fmt.Sprintf("%s expectation %s: vector=%q a=%q", key, k, want, got))
		}
		if got := fieldsB[k]; got != want {
			out = append(out, fmt.Sprintf("%s expectation %s: vector=%q b=%q", key, k, want, got))
		}
	}
	return out
}
