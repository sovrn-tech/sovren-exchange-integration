// SOVR account-address validation (FR-014) and UNSAFE_TEST_ONLY derivation
// (FR-015). Mirrors the Go `address` package exactly: same error codes, same
// check order, pinned by test-vectors/addresses.json and derivation.json.

import { Bip39, EnglishMnemonic, Secp256k1, Slip10, Slip10Curve, ripemd160, sha256, stringToPath } from "@cosmjs/crypto";
import { fromBech32, toBech32 } from "@cosmjs/encoding";

export const BECH32_PREFIX_ACCOUNT = "sovr";
export const BECH32_PREFIX_VALOPER = "sovrvaloper";
export const BECH32_PREFIX_VALCONS = "sovrvalcons";
export const ACCOUNT_ADDRESS_LENGTH = 20;
export const DEFAULT_DERIVATION_PATH = "m/44'/118'/0'/0/0";

export type AddressErrorCode =
  | "ADDRESS_EMPTY"
  | "ADDRESS_INVALID_BECH32"
  | "ADDRESS_WRONG_PREFIX"
  | "ADDRESS_WRONG_LENGTH"
  | "ADDRESS_NOT_ACCOUNT_TYPE"
  | "ADDRESS_PROHIBITED"
  | "ADDRESS_WHITESPACE";

export interface AddressValidationResult {
  valid: boolean;
  normalizedAddress?: string; // set iff valid; always lowercase canonical bech32
  errorCode?: AddressErrorCode;
  errorMessage?: string;
}

export interface ValidateAddressOpts {
  // Prohibited canonical lowercase sovr1… addresses (module accounts,
  // blocklists); matched against the normalized address.
  prohibited?: Iterable<string>;
}

const ASCII_WHITESPACE_RE = /[ \t\n\r\v\f]/;
const BARE_HEX_20_RE = /^[0-9a-fA-F]{40}$/;

function invalid(errorCode: AddressErrorCode, errorMessage: string): AddressValidationResult {
  return { valid: false, errorCode, errorMessage };
}

// Check order is contract-pinned and identical to Go: empty → whitespace →
// hex form → mixed case → bech32 decode → prefix → payload length →
// prohibited set. Whitespace is never trimmed; uppercase-only bech32 is
// valid and normalized to lowercase; mixed case is invalid.
export function validateAccountAddress(addr: string, opts?: ValidateAddressOpts): AddressValidationResult {
  if (addr === "") {
    return invalid("ADDRESS_EMPTY", "address is empty");
  }
  if (ASCII_WHITESPACE_RE.test(addr)) {
    return invalid("ADDRESS_WHITESPACE", "address contains whitespace (never auto-trimmed)");
  }
  if (addr.startsWith("0x") || addr.startsWith("0X") || BARE_HEX_20_RE.test(addr)) {
    return invalid("ADDRESS_WRONG_PREFIX", "hex (EVM-style) form is not a SOVR account address");
  }
  if (addr.toLowerCase() !== addr && addr.toUpperCase() !== addr) {
    return invalid("ADDRESS_INVALID_BECH32", "mixed-case bech32 is invalid");
  }
  let decoded;
  try {
    decoded = fromBech32(addr);
  } catch (err) {
    return invalid("ADDRESS_INVALID_BECH32", `bech32 decode failed: ${err instanceof Error ? err.message : String(err)}`);
  }
  if (decoded.prefix === BECH32_PREFIX_VALOPER || decoded.prefix === BECH32_PREFIX_VALCONS) {
    return invalid("ADDRESS_NOT_ACCOUNT_TYPE", `prefix "${decoded.prefix}" is a validator address, not an account address`);
  }
  if (decoded.prefix !== BECH32_PREFIX_ACCOUNT) {
    return invalid("ADDRESS_WRONG_PREFIX", `expected prefix "${BECH32_PREFIX_ACCOUNT}", got "${decoded.prefix}"`);
  }
  if (decoded.data.length !== ACCOUNT_ADDRESS_LENGTH) {
    return invalid("ADDRESS_WRONG_LENGTH", `expected ${ACCOUNT_ADDRESS_LENGTH}-byte payload, got ${decoded.data.length}`);
  }
  const normalized = toBech32(BECH32_PREFIX_ACCOUNT, decoded.data);
  if (opts?.prohibited !== undefined) {
    for (const p of opts.prohibited) {
      if (p === normalized) {
        return invalid("ADDRESS_PROHIBITED", "address is a prohibited account");
      }
    }
  }
  return { valid: true, normalizedAddress: normalized };
}

// moduleAccountAddress returns the sovr1… address of a top-level module
// account (sha256(name) truncated to 20 bytes, per Cosmos SDK convention).
export function moduleAccountAddress(name: string): string {
  if (name === "") {
    throw new Error("module name must not be empty");
  }
  return toBech32(BECH32_PREFIX_ACCOUNT, sha256(new TextEncoder().encode(name)).slice(0, ACCOUNT_ADDRESS_LENGTH));
}

// PROHIBITED_MODULE_NAMES is the canonical set of module-account NAMES an
// exchange must never sign a withdrawal to. It guards against two DISTINCT
// outcomes — do not conflate them:
//   • Blocked accounts (all names here except `gov`) are in the chain's bank
//     blocked-address set, so a MsgSend to them is REJECTED atomically on-chain:
//     the withdrawal fails, no recipient balance is created, funds stay with the
//     sender. Rejecting client-side just avoids a guaranteed-to-fail broadcast
//     (wasted fee/sequence, a stuck withdrawal in your queue) — no funds are lost.
//   • `gov` is deliberately NOT blocked on-chain (see its line below), so a plain
//     withdrawal MsgSend to it SUCCEEDS and the funds land in an account no keeper
//     will release — permanently STRANDED. Client-side rejection is the ONLY
//     safeguard for this and any other unblocked sink.
// This MUST mirror the Go kit's DefaultProhibitedModuleAccounts() name list; both
// are tied to the chain's authoritative set by the drift test in
// app/exchange_kit_module_accounts_test.go.
export const PROHIBITED_MODULE_NAMES: readonly string[] = [
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
  "gov", // client-only extra: the chain permits MsgDeposit into gov, but a plain withdrawal MsgSend strands funds.
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
];

// LEGACY_PROHIBITED_ADDRESS is the derived sovr1… address of one orphaned module
// account whose internal name predates the v0.8.0 module rename (its module
// became x/exchange_allocation). The account lingers on upgraded chains with no
// keeper able to spend from it, so the chain keeps it blocked and the kit rejects
// it too. It is pinned by its immutable derived address, never by its retired
// internal name, so the kit carries no retired internal terminology. Mirrors Go's
// legacyRenamedModuleAccountAddress; pinned to the chain derivation by the drift test.
export const LEGACY_PROHIBITED_ADDRESS = "sovr1evp0da9u6kzn5c3j745qyj379grxhhzwrp5fh8";

// defaultProhibitedModuleAccounts returns the canonical set of lowercase sovr1…
// addresses an exchange must reject as withdrawal destinations — every module
// account plus the legacy address. Pass it straight to validateAccountAddress:
//   validateAccountAddress(dest, { prohibited: defaultProhibitedModuleAccounts() })
// This is the TypeScript equivalent of the Go DefaultProhibitedModuleAccounts().
export function defaultProhibitedModuleAccounts(): Set<string> {
  const set = new Set<string>();
  for (const name of PROHIBITED_MODULE_NAMES) {
    set.add(moduleAccountAddress(name));
  }
  set.add(LEGACY_PROHIBITED_ADDRESS);
  return set;
}

// UNSAFE_TEST_ONLY — deriveAddress handles raw mnemonics and private keys in
// process memory with no protection whatsoever. It exists solely for test
// vectors and integration testing (FR-015). Production key generation,
// storage, and signing belong entirely to the exchange's own custody
// infrastructure; never pass a production mnemonic here.
export interface DerivedAddress {
  path: string;
  privateKey: Uint8Array; // 32-byte secp256k1 secret — UNSAFE_TEST_ONLY
  publicKeyCompressed: Uint8Array; // 33 bytes
  addressBytes: Uint8Array; // 20 bytes: ripemd160(sha256(pubkey))
  bech32Address: string; // sovr1…
}

// Derivation is pinned to the documented m/44'/118'/… purpose/coin type
// (FR-015); other paths are refused, matching Go.
const REQUIRED_HD_PATH_PREFIX = "m/44'/118'/";

export async function deriveAddress(mnemonic: string, hdPath: string): Promise<DerivedAddress> {
  if (mnemonic.trim() === "") {
    throw new Error("mnemonic must not be empty");
  }
  if (!hdPath.startsWith(REQUIRED_HD_PATH_PREFIX)) {
    throw new Error(`derivation path must start with "${REQUIRED_HD_PATH_PREFIX}" (FR-015), got "${hdPath}"`);
  }
  const seed = await Bip39.mnemonicToSeed(new EnglishMnemonic(mnemonic));
  const { privkey } = Slip10.derivePath(Slip10Curve.Secp256k1, seed, stringToPath(hdPath));
  const keypair = await Secp256k1.makeKeypair(privkey);
  const publicKeyCompressed = Secp256k1.compressPubkey(keypair.pubkey);
  const addressBytes = ripemd160(sha256(publicKeyCompressed));
  return {
    path: hdPath,
    privateKey: privkey,
    publicKeyCompressed,
    addressBytes,
    bech32Address: toBech32(BECH32_PREFIX_ACCOUNT, addressBytes),
  };
}
