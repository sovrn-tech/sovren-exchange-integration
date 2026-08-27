package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cosmos/cosmos-sdk/types/bech32"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/amounts"
)

const (
	schemaVersion = 1
	kitVersion    = "0.1.0"
)

// VectorFiles are the committed suites: the US3 four plus the US5
// transaction suites (vectors_tx.go). The tx extension appends transaction
// categories to invalid-cases.json without structural change.
var VectorFiles = append([]string{
	"addresses.json",
	"derivation.json",
	"amounts.json",
	"invalid-cases.json",
}, TxVectorFiles...)

// Standard BIP39 English test mnemonics — publicly known, UNSAFE_TEST_ONLY.
const (
	mnemonicA = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	mnemonicB = "legal winner thank year wave sausage worth useful legal winner thank yellow"
	mnemonicC = "zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo wrong"
)

type envelope struct {
	SchemaVersion  int    `json:"schema_version"`
	KitVersion     string `json:"kit_version"`
	UnsafeTestOnly bool   `json:"UNSAFE_TEST_ONLY"`
	Vectors        []any  `json:"vectors"`
}

type validation struct {
	Valid      bool   `json:"valid"`
	Normalized string `json:"normalized,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
}

type keyVector struct {
	ID                     string     `json:"id"`
	Mnemonic               string     `json:"mnemonic"`
	DerivationPath         string     `json:"derivation_path"`
	PrivateKeyHex          string     `json:"private_key_hex"`
	PublicKeyCompressedHex string     `json:"public_key_compressed_hex"`
	AccountAddressBytesHex string     `json:"account_address_bytes_hex"`
	Bech32Address          string     `json:"bech32_address"`
	Validation             validation `json:"validation"`
}

type validationVector struct {
	ID         string     `json:"id"`
	Input      string     `json:"input"`
	Prohibited []string   `json:"prohibited,omitempty"`
	Validation validation `json:"validation"`
}

type amountVector struct {
	ID        string `json:"id"`
	Display   string `json:"display"`
	BaseUnits string `json:"base_units,omitempty"`
	Valid     bool   `json:"valid"`
	ErrorCode string `json:"error_code,omitempty"`
}

type invalidCase struct {
	ID       string            `json:"id"`
	Category string            `json:"category"`
	Vector   map[string]string `json:"vector"`
	Expected map[string]string `json:"expected"`
}

// GenerateAll writes the four US3 vector files to dir. Every entry is checked
// against the Go implementation during generation, so a committed file always
// pins actual library behavior. Output is byte-deterministic: fixed inputs,
// struct field order, sorted map keys, two-space indent, trailing newline.
func GenerateAll(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	addrs, err := addressVectors()
	if err != nil {
		return fmt.Errorf("addresses.json: %w", err)
	}
	derivs, err := derivationVectors()
	if err != nil {
		return fmt.Errorf("derivation.json: %w", err)
	}
	amts, err := amountVectors()
	if err != nil {
		return fmt.Errorf("amounts.json: %w", err)
	}
	invalids, err := invalidCaseVectors()
	if err != nil {
		return fmt.Errorf("invalid-cases.json: %w", err)
	}
	txInvalids, err := txInvalidCases()
	if err != nil {
		return fmt.Errorf("invalid-cases.json (tx): %w", err)
	}
	for _, c := range txInvalids {
		invalids = append(invalids, c)
	}
	txFiles, err := txSuiteVectors()
	if err != nil {
		return fmt.Errorf("tx suites: %w", err)
	}
	files := map[string][]any{
		"addresses.json":     addrs,
		"derivation.json":    derivs,
		"amounts.json":       amts,
		"invalid-cases.json": invalids,
	}
	for name, vs := range txFiles {
		files[name] = vs
	}
	for _, name := range VectorFiles {
		env := envelope{SchemaVersion: schemaVersion, KitVersion: kitVersion, UnsafeTestOnly: true, Vectors: files[name]}
		enc, err := json.MarshalIndent(env, "", "  ")
		if err != nil {
			return err
		}
		enc = append(enc, '\n')
		if err := os.WriteFile(filepath.Join(dir, name), enc, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func makeKeyVector(id, mnemonic, path string) (keyVector, error) {
	a, err := address.DeriveAddress(mnemonic, path)
	if err != nil {
		return keyVector{}, fmt.Errorf("%s: %w", id, err)
	}
	v := address.ValidateAccountAddress(a.Bech32)
	if !v.Valid || v.NormalizedAddress != a.Bech32 {
		return keyVector{}, fmt.Errorf("%s: derived address failed validation: %+v", id, v)
	}
	return keyVector{
		ID:                     id,
		Mnemonic:               mnemonic,
		DerivationPath:         path,
		PrivateKeyHex:          hex.EncodeToString(a.PrivateKey),
		PublicKeyCompressedHex: hex.EncodeToString(a.PublicKeyCompressed),
		AccountAddressBytesHex: hex.EncodeToString(a.AddressBytes),
		Bech32Address:          a.Bech32,
		Validation:             validation{Valid: true, Normalized: v.NormalizedAddress},
	}, nil
}

func makeValidationVector(id, input, wantCode string, prohibited []string) (validationVector, error) {
	var res address.ValidationResult
	if prohibited != nil {
		res = address.ValidateAccountAddressStrict(input, address.NewPSet(prohibited...))
	} else {
		res = address.ValidateAccountAddress(input)
	}
	if wantCode == "" {
		if !res.Valid {
			return validationVector{}, fmt.Errorf("%s: expected valid, got %s (%s)", id, res.ErrorCode, res.ErrorMessage)
		}
	} else if res.Valid || res.ErrorCode != wantCode {
		return validationVector{}, fmt.Errorf("%s: expected %s, got valid=%v code=%s", id, wantCode, res.Valid, res.ErrorCode)
	}
	return validationVector{
		ID:         id,
		Input:      input,
		Prohibited: prohibited,
		Validation: validation{Valid: res.Valid, Normalized: res.NormalizedAddress, ErrorCode: res.ErrorCode},
	}, nil
}

// corruptChecksum breaks the final bech32 checksum character; single-character
// substitution is guaranteed detectable by the BCH code.
func corruptChecksum(addr string) string {
	last := "q"
	if addr[len(addr)-1] == 'q' {
		last = "p"
	}
	return addr[:len(addr)-1] + last
}

func addressVectors() ([]any, error) {
	var out []any
	keyEntries := []struct {
		id       string
		mnemonic string
		path     string
	}{
		{"addr-001", mnemonicA, "m/44'/118'/0'/0/0"},
		{"addr-002", mnemonicA, "m/44'/118'/0'/0/1"},
		{"addr-003", mnemonicB, "m/44'/118'/0'/0/0"},
	}
	for _, e := range keyEntries {
		kv, err := makeKeyVector(e.id, e.mnemonic, e.path)
		if err != nil {
			return nil, err
		}
		out = append(out, kv)
	}

	base, err := address.DeriveAddress(mnemonicA, address.DefaultHDPath)
	if err != nil {
		return nil, err
	}
	valid := base.Bech32
	valoper, err := bech32.ConvertAndEncode(address.Bech32PrefixValOper, base.AddressBytes)
	if err != nil {
		return nil, err
	}
	valcons, err := bech32.ConvertAndEncode(address.Bech32PrefixValCons, base.AddressBytes)
	if err != nil {
		return nil, err
	}
	cosmosAddr, err := bech32.ConvertAndEncode("cosmos", base.AddressBytes)
	if err != nil {
		return nil, err
	}
	payload32 := sha256.Sum256([]byte("sovren-exchange-kit-length-vector"))
	long, err := bech32.ConvertAndEncode(address.Bech32PrefixAccount, payload32[:])
	if err != nil {
		return nil, err
	}
	short, err := bech32.ConvertAndEncode(address.Bech32PrefixAccount, base.AddressBytes[:19])
	if err != nil {
		return nil, err
	}
	feeCollector, err := address.ModuleAccountAddress("fee_collector")
	if err != nil {
		return nil, err
	}

	valEntries := []struct {
		id         string
		input      string
		wantCode   string
		prohibited []string
	}{
		{"addr-val-001", valid, "", nil},
		{"addr-val-002", strings.ToUpper(valid), "", nil},
		{"addr-val-003", feeCollector, "", nil},
		{"addr-val-004", valid, "", []string{feeCollector}},
		{"addr-inv-001", "", address.CodeEmpty, nil},
		{"addr-inv-002", " " + valid, address.CodeWhitespace, nil},
		{"addr-inv-003", valid + "\n", address.CodeWhitespace, nil},
		{"addr-inv-004", valid[:10] + " " + valid[10:], address.CodeWhitespace, nil},
		{"addr-inv-005", corruptChecksum(valid), address.CodeInvalidBech32, nil},
		{"addr-inv-006", "S" + valid[1:], address.CodeInvalidBech32, nil},
		{"addr-inv-007", "not-an-address", address.CodeInvalidBech32, nil},
		{"addr-inv-008", cosmosAddr, address.CodeWrongPrefix, nil},
		{"addr-inv-009", "0x" + hex.EncodeToString(base.AddressBytes), address.CodeWrongPrefix, nil},
		{"addr-inv-010", hex.EncodeToString(base.AddressBytes), address.CodeWrongPrefix, nil},
		{"addr-inv-011", valoper, address.CodeNotAccountType, nil},
		{"addr-inv-012", valcons, address.CodeNotAccountType, nil},
		{"addr-inv-013", long, address.CodeWrongLength, nil},
		{"addr-inv-014", short, address.CodeWrongLength, nil},
		{"addr-inv-015", feeCollector, address.CodeProhibited, []string{feeCollector}},
	}
	for _, e := range valEntries {
		vv, err := makeValidationVector(e.id, e.input, e.wantCode, e.prohibited)
		if err != nil {
			return nil, err
		}
		out = append(out, vv)
	}
	return out, nil
}

func derivationVectors() ([]any, error) {
	entries := []struct {
		id       string
		mnemonic string
		path     string
	}{
		{"der-001", mnemonicA, "m/44'/118'/0'/0/0"},
		{"der-002", mnemonicA, "m/44'/118'/0'/0/1"},
		{"der-003", mnemonicA, "m/44'/118'/0'/0/2"},
		{"der-004", mnemonicA, "m/44'/118'/1'/0/0"},
		{"der-005", mnemonicA, "m/44'/118'/0'/1/0"},
		{"der-006", mnemonicA, "m/44'/118'/2'/0/7"},
		{"der-007", mnemonicB, "m/44'/118'/0'/0/0"},
		{"der-008", mnemonicB, "m/44'/118'/0'/0/1"},
		{"der-009", mnemonicC, "m/44'/118'/0'/0/0"},
	}
	var out []any
	seen := map[string]string{}
	for _, e := range entries {
		kv, err := makeKeyVector(e.id, e.mnemonic, e.path)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[kv.Bech32Address]; dup {
			return nil, fmt.Errorf("%s: address collides with %s", e.id, prev)
		}
		seen[kv.Bech32Address] = e.id
		out = append(out, kv)
	}
	return out, nil
}

func amountVectors() ([]any, error) {
	valid := []struct {
		id      string
		display string
		base    string
	}{
		{"amt-001", "1", "1000000"},
		{"amt-002", "1.0", "1000000"},
		{"amt-003", "10.25", "10250000"},
		{"amt-004", "0.000001", "1"},
		{"amt-005", "0", "0"},
		{"amt-006", "0.5", "500000"},
		{"amt-007", "123456.654321", "123456654321"},
		{"amt-008", "999999999.999999", "999999999999999"},
		{"amt-009", "1000000000", "1000000000000000"},
		{"amt-010", "0.000010", "10"},
	}
	invalid := []struct {
		id      string
		display string
		code    string
	}{
		{"amt-inv-001", "0.0000001", amounts.CodeTooManyDecimals},
		{"amt-inv-002", "1.0000000", amounts.CodeTooManyDecimals},
		{"amt-inv-003", "-1", amounts.CodeNegative},
		{"amt-inv-004", "-0.5", amounts.CodeNegative},
		{"amt-inv-005", "1e6", amounts.CodeScientificNotation},
		{"amt-inv-006", "1.5E-3", amounts.CodeScientificNotation},
		{"amt-inv-007", "1,000", amounts.CodeCommas},
		{"amt-inv-008", "", amounts.CodeEmpty},
		{"amt-inv-009", "abc", amounts.CodeNotNumeric},
		{"amt-inv-010", "1.2.3", amounts.CodeNotNumeric},
		{"amt-inv-011", ".5", amounts.CodeNotNumeric},
		{"amt-inv-012", "1.", amounts.CodeNotNumeric},
		{"amt-inv-013", " 1", amounts.CodeNotNumeric},
		{"amt-inv-014", "+1", amounts.CodeNotNumeric},
		{"amt-inv-015", "01", amounts.CodeNotNumeric},
		{"amt-inv-016", "1000000000.000001", amounts.CodeExceedsMax},
		{"amt-inv-017", "1000000001", amounts.CodeExceedsMax},
	}
	var out []any
	for _, e := range valid {
		got, err := amounts.DisplayToBaseUnits(e.display)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.id, err)
		}
		if got != e.base {
			return nil, fmt.Errorf("%s: display %q => %q, expected %q", e.id, e.display, got, e.base)
		}
		display, err := amounts.BaseToDisplayUnits(e.base)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.id, err)
		}
		roundTrip, err := amounts.DisplayToBaseUnits(display)
		if err != nil || roundTrip != e.base {
			return nil, fmt.Errorf("%s: round trip via %q broke: %q (%v)", e.id, display, roundTrip, err)
		}
		out = append(out, amountVector{ID: e.id, Display: e.display, BaseUnits: e.base, Valid: true})
	}
	for _, e := range invalid {
		_, err := amounts.DisplayToBaseUnits(e.display)
		if amounts.CodeOf(err) != e.code {
			return nil, fmt.Errorf("%s: display %q expected %s, got %v", e.id, e.display, e.code, err)
		}
		out = append(out, amountVector{ID: e.id, Display: e.display, ErrorCode: e.code})
	}
	return out, nil
}

// invalidCaseVectors covers the FR-060 address and amount categories. The
// transaction categories (WRONG_CHAIN_ID, INCORRECT_SEQUENCE, …) are appended
// by the transaction-vector task using the same entry shape.
func invalidCaseVectors() ([]any, error) {
	base, err := address.DeriveAddress(mnemonicA, address.DefaultHDPath)
	if err != nil {
		return nil, err
	}
	valoper, err := bech32.ConvertAndEncode(address.Bech32PrefixValOper, base.AddressBytes)
	if err != nil {
		return nil, err
	}
	cosmosAddr, err := bech32.ConvertAndEncode("cosmos", base.AddressBytes)
	if err != nil {
		return nil, err
	}
	cases := []invalidCase{
		{
			ID:       "neg-001",
			Category: "WRONG_BECH32_PREFIX",
			Vector:   map[string]string{"address": cosmosAddr},
			Expected: map[string]string{"stage": "library", "error_code": address.CodeWrongPrefix, "outcome": "address validation rejects"},
		},
		{
			ID:       "neg-002",
			Category: "INVALID_CHECKSUM",
			Vector:   map[string]string{"address": corruptChecksum(base.Bech32)},
			Expected: map[string]string{"stage": "library", "error_code": address.CodeInvalidBech32, "outcome": "address validation rejects"},
		},
		{
			ID:       "neg-003",
			Category: "VALIDATOR_OPERATOR_ADDRESS",
			Vector:   map[string]string{"address": valoper},
			Expected: map[string]string{"stage": "library", "error_code": address.CodeNotAccountType, "outcome": "address validation rejects"},
		},
		{
			ID:       "neg-004",
			Category: "ZERO_AMOUNT",
			Vector:   map[string]string{"amount_display": "0"},
			Expected: map[string]string{"stage": "library", "outcome": "conversion yields base 0; single-MsgSend build rejects zero amount", "base_units": "0"},
		},
		{
			ID:       "neg-005",
			Category: "NEGATIVE_AMOUNT",
			Vector:   map[string]string{"amount_display": "-25.5"},
			Expected: map[string]string{"stage": "library", "error_code": amounts.CodeNegative, "outcome": "amount conversion rejects"},
		},
		{
			ID:       "neg-006",
			Category: "EXCESS_DECIMALS",
			Vector:   map[string]string{"amount_display": "1.1234567"},
			Expected: map[string]string{"stage": "library", "error_code": amounts.CodeTooManyDecimals, "outcome": "amount conversion rejects"},
		},
	}
	for _, c := range cases {
		if err := checkInvalidCase(c); err != nil {
			return nil, err
		}
	}
	out := make([]any, len(cases))
	for i, c := range cases {
		out[i] = c
	}
	return out, nil
}

func checkInvalidCase(c invalidCase) error {
	switch c.Category {
	case "WRONG_BECH32_PREFIX", "INVALID_CHECKSUM", "VALIDATOR_OPERATOR_ADDRESS":
		res := address.ValidateAccountAddress(c.Vector["address"])
		if res.Valid || res.ErrorCode != c.Expected["error_code"] {
			return fmt.Errorf("%s: expected %s, got valid=%v code=%s", c.ID, c.Expected["error_code"], res.Valid, res.ErrorCode)
		}
	case "ZERO_AMOUNT":
		got, err := amounts.DisplayToBaseUnits(c.Vector["amount_display"])
		if err != nil || got != c.Expected["base_units"] {
			return fmt.Errorf("%s: expected base %q, got %q (%v)", c.ID, c.Expected["base_units"], got, err)
		}
	case "NEGATIVE_AMOUNT", "EXCESS_DECIMALS":
		_, err := amounts.DisplayToBaseUnits(c.Vector["amount_display"])
		if amounts.CodeOf(err) != c.Expected["error_code"] {
			return fmt.Errorf("%s: expected %s, got %v", c.ID, c.Expected["error_code"], err)
		}
	default:
		return fmt.Errorf("%s: unknown category %s", c.ID, c.Category)
	}
	return nil
}

// CompareDirs byte-compares the four vector files between two directories,
// returning a description per differing or missing file.
func CompareDirs(genDir, committedDir string) ([]string, error) {
	var mismatches []string
	for _, name := range VectorFiles {
		gen, err := os.ReadFile(filepath.Join(genDir, name))
		if err != nil {
			return nil, err
		}
		committed, err := os.ReadFile(filepath.Join(committedDir, name))
		if err != nil {
			mismatches = append(mismatches, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if !bytes.Equal(gen, committed) {
			mismatches = append(mismatches, fmt.Sprintf("%s: committed file differs from deterministic regeneration", name))
		}
	}
	return mismatches, nil
}
