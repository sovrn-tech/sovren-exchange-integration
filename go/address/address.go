// Package address validates SOVR account addresses (FR-014) and derives
// test-only addresses from BIP39 mnemonics (FR-015). Behavior is pinned by
// test-vectors/addresses.json and test-vectors/derivation.json; the
// TypeScript library mirrors every error code and check order exactly.
package address

import (
	"fmt"
	"regexp"
	"strings"

	sdkaddr "github.com/cosmos/cosmos-sdk/types/address"
	"github.com/cosmos/cosmos-sdk/types/bech32"
)

const (
	Bech32PrefixAccount  = "sovr"
	Bech32PrefixValOper  = "sovrvaloper"
	Bech32PrefixValCons  = "sovrvalcons"
	AccountAddressLength = 20
)

// FR-014 error codes. Stable API: exchanges branch on these strings.
const (
	CodeEmpty          = "ADDRESS_EMPTY"
	CodeInvalidBech32  = "ADDRESS_INVALID_BECH32"
	CodeWrongPrefix    = "ADDRESS_WRONG_PREFIX"
	CodeWrongLength    = "ADDRESS_WRONG_LENGTH"
	CodeNotAccountType = "ADDRESS_NOT_ACCOUNT_TYPE"
	CodeProhibited     = "ADDRESS_PROHIBITED"
	CodeWhitespace     = "ADDRESS_WHITESPACE"
)

// ValidationResult is the outcome of address validation
// (contracts/go-client-api.md §address).
type ValidationResult struct {
	Valid             bool
	NormalizedAddress string // set iff Valid; always lowercase canonical bech32
	ErrorCode         string
	ErrorMessage      string
}

// PSet is a prohibited-address set for ValidateAccountAddressStrict. Entries
// must be canonical lowercase sovr1… strings; membership is exact-match
// against the normalized address.
type PSet map[string]struct{}

func NewPSet(addrs ...string) PSet {
	p := make(PSet, len(addrs))
	for _, a := range addrs {
		p[a] = struct{}{}
	}
	return p
}

func (p PSet) Contains(addr string) bool {
	_, ok := p[addr]
	return ok
}

const asciiWhitespace = " \t\n\r\v\f"

var bareHex20Re = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func invalid(code, msg string) ValidationResult {
	return ValidationResult{ErrorCode: code, ErrorMessage: msg}
}

// ValidateAccountAddress validates addr as a SOVR account address (FR-014).
// Whitespace is never trimmed; uppercase-only bech32 is valid and normalized
// to lowercase; mixed case is invalid. Check order is contract-pinned:
// empty → whitespace → hex form → mixed case → bech32 decode → prefix →
// payload length.
func ValidateAccountAddress(addr string) ValidationResult {
	if addr == "" {
		return invalid(CodeEmpty, "address is empty")
	}
	if strings.ContainsAny(addr, asciiWhitespace) {
		return invalid(CodeWhitespace, "address contains whitespace (never auto-trimmed)")
	}
	if strings.HasPrefix(addr, "0x") || strings.HasPrefix(addr, "0X") || bareHex20Re.MatchString(addr) {
		return invalid(CodeWrongPrefix, "hex (EVM-style) form is not a SOVR account address")
	}
	if strings.ToLower(addr) != addr && strings.ToUpper(addr) != addr {
		return invalid(CodeInvalidBech32, "mixed-case bech32 is invalid")
	}
	hrp, payload, err := bech32.DecodeAndConvert(addr)
	if err != nil {
		return invalid(CodeInvalidBech32, fmt.Sprintf("bech32 decode failed: %v", err))
	}
	switch hrp {
	case Bech32PrefixAccount:
	case Bech32PrefixValOper, Bech32PrefixValCons:
		return invalid(CodeNotAccountType, fmt.Sprintf("prefix %q is a validator address, not an account address", hrp))
	default:
		return invalid(CodeWrongPrefix, fmt.Sprintf("expected prefix %q, got %q", Bech32PrefixAccount, hrp))
	}
	if len(payload) != AccountAddressLength {
		return invalid(CodeWrongLength, fmt.Sprintf("expected %d-byte payload, got %d", AccountAddressLength, len(payload)))
	}
	normalized, err := bech32.ConvertAndEncode(Bech32PrefixAccount, payload)
	if err != nil {
		return invalid(CodeInvalidBech32, fmt.Sprintf("bech32 re-encode failed: %v", err))
	}
	return ValidationResult{Valid: true, NormalizedAddress: normalized}
}

// ValidateAccountAddressStrict runs ValidateAccountAddress and additionally
// rejects normalized addresses present in prohibited (module accounts,
// exchange blocklists) with ADDRESS_PROHIBITED.
func ValidateAccountAddressStrict(addr string, prohibited PSet) ValidationResult {
	res := ValidateAccountAddress(addr)
	if !res.Valid {
		return res
	}
	if prohibited.Contains(res.NormalizedAddress) {
		return invalid(CodeProhibited, "address is a prohibited account")
	}
	return res
}

// ModuleAccountAddress returns the sovr1… address of a top-level module
// account (sha256(name) truncated to 20 bytes, per Cosmos SDK convention).
func ModuleAccountAddress(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("module name must not be empty")
	}
	return bech32.ConvertAndEncode(Bech32PrefixAccount, sdkaddr.Module(name))
}

// legacyRenamedModuleAccountAddress is the derived sovr1… address of one
// orphaned module account whose internal name predates the v0.8.0 module
// rename (its module became x/exchange_allocation). The auth account was
// created while the old module existed and is NOT removed by a store
// rename, so it lingers on upgraded chains with no keeper able to spend
// from it — the chain keeps it blocked (see the sbn drift test named
// below), and a customer send to it would strand funds permanently, so
// the kit rejects it too. It is pinned as its immutable derived address
// (never as its legacy internal name) so the customer-facing kit does not
// carry retired internal terminology; the address is frozen because the
// account can never change. Cross-checked by name against the chain in
// app/exchange_kit_module_accounts_test.go in the sbn repo.
const legacyRenamedModuleAccountAddress = "sovr1evp0da9u6kzn5c3j745qyj379grxhhzwrp5fh8"

// DefaultProhibitedModuleAccounts is the kit's client-side withdrawal
// blocklist: every account a customer withdrawal must never reach, rejected
// client-side rather than left to fail — or strand funds — on-chain.
// Exchanges extend it via ValidateAccountAddressStrict.
//
// It is the chain's blocked-module-account set PLUS gov, and is deliberately
// NOT identical to the chain's bank blocklist:
//   - The 31 names below plus legacyRenamedModuleAccountAddress reproduce the
//     chain's 32 blocked accounts. The one renamed-module account is pinned by
//     address, never by its retired internal name (see
//     legacyRenamedModuleAccountAddress).
//   - gov is a CLIENT-ONLY addition. The chain omits gov from its bank
//     blocklist only so MsgDeposit can move proposal deposits into the gov
//     module account; a plain withdrawal MsgSend to that same address creates
//     no governance deposit record, so the funds land in an account no keeper
//     will ever release — permanently stranded. A withdrawal to gov is
//     therefore always an operator error, and the kit rejects it up front.
//   - mint has no counterpart: SOVR has no x/mint module, so no mint account
//     exists to block.
//
// The kit hardcodes the names (and the one legacy address) to keep itself free
// of any chain import. The 32 chain-mirrored entries are kept in sync by a
// drift test in the sbn repo (app/exchange_kit_module_accounts_test.go) that
// fails if the chain adds or removes a blocked module account; that test
// guards only the chain-mirrored names, and gov is pinned as the intentional
// client-only extra by address_test.go here. There are 33 entries in total:
// 32 names below plus the legacy address.
func DefaultProhibitedModuleAccounts() PSet {
	names := []string{
		"auction",
		"auction_bond_escrow",
		"bandwidth",
		"bonded_tokens_pool",
		"bootstrap",
		"bridge",
		"compute",
		"disputebonds",
		"distribution",
		"distro",
		"exchange_allocation",
		"fee_collector",
		"gateway",
		// gov: client-only. See the doc comment above — a withdrawal
		// MsgSend to gov strands funds even though the chain permits
		// MsgDeposit transfers into the same account.
		"gov",
		"identity",
		"inference",
		"interchainaccounts",
		"lockup",
		"nft",
		"nodelicense",
		"not_bonded_tokens_pool",
		"oracle",
		"payments",
		"policy",
		"settlement",
		"settlement_rewards",
		"storage",
		"supply",
		"track_a",
		"transfer",
		"vectordb",
		"wasm",
	}
	p := make(PSet, len(names)+1)
	for _, n := range names {
		addr, err := ModuleAccountAddress(n)
		if err != nil {
			continue
		}
		p[addr] = struct{}{}
	}
	p[legacyRenamedModuleAccountAddress] = struct{}{}
	return p
}
