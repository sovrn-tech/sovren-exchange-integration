# Address Generation & Validation

How SOVR account addresses are formatted, validated, and derived, and how an
exchange should structure customer deposit addresses. Library behavior
described here is pinned by the shared vector suites
(`test-vectors/addresses.json`, `test-vectors/derivation.json`) and enforced
identically in Go (`go/address`) and TypeScript
(`@sovren/exchange-integration` `address` module).

## Address format

| Property | Value |
|----------|-------|
| Encoding | bech32 (BIP-173) |
| Account prefix | `sovr` |
| Validator operator prefix | `sovrvaloper` (never a customer address) |
| Validator consensus prefix | `sovrvalcons` (never a customer address) |
| Payload | exactly 20 bytes (`ripemd160(sha256(compressed secp256k1 pubkey))`) |
| Canonical form | all-lowercase |

A canonical account address is 43 characters: `sovr1` followed by 38 data and
checksum characters, e.g. `sovr19rl4cm2hmr8afy4kldpxz3fka4jguq0aes824x`.

## Validation rules (FR-014)

`address.ValidateAccountAddress` (Go) / `validateAccountAddress` (TS) return
either a normalized address or exactly one documented error code:

| Error code | Trigger |
|------------|---------|
| `ADDRESS_EMPTY` | empty string |
| `ADDRESS_WHITESPACE` | any whitespace, anywhere — input is **never trimmed**; reject and make the customer re-paste |
| `ADDRESS_WRONG_PREFIX` | foreign bech32 prefix (`cosmos1…`), `0x…`, or bare 40-char hex (EVM form) |
| `ADDRESS_INVALID_BECH32` | bad checksum, invalid characters, or **mixed-case** input |
| `ADDRESS_NOT_ACCOUNT_TYPE` | `sovrvaloper…` / `sovrvalcons…` — a validator address is not a transfer destination |
| `ADDRESS_WRONG_LENGTH` | valid bech32 under `sovr` but payload ≠ 20 bytes |
| `ADDRESS_PROHIBITED` | strict variant only: normalized address is in the caller's prohibited set |

Normalization: all-uppercase bech32 is valid per BIP-173 and is normalized to
lowercase; always store and compare the returned `NormalizedAddress`, never
the raw input. Mixed case is invalid.

The strict variant additionally rejects module accounts and any exchange-side
blocklist — in Go `ValidateAccountAddressStrict`, in TypeScript the `prohibited`
option: `validateAccountAddress(dest, { prohibited: defaultProhibitedModuleAccounts() })`.
The canonical set is `DefaultProhibitedModuleAccounts()` in Go and
`defaultProhibitedModuleAccounts()` in TypeScript (both derived from the same
names — `PROHIBITED_MODULE_NAMES`/`LEGACY_PROHIBITED_ADDRESS` in TS — and kept in
lockstep with the chain by the cross-language drift test). It seeds the set with
the chain's blocked module accounts (`fee_collector`, `distribution`,
`bonded_tokens_pool`, `not_bonded_tokens_pool`, …) plus `gov`. These two groups
fail differently, and the blocklist guards both: a `MsgSend` to a **blocked**
module account is **rejected on-chain** — the withdrawal fails atomically and no
funds move; rejecting it client-side just avoids a doomed broadcast. `gov` is a
**client-only** entry: the chain permits `MsgDeposit` transfers into the gov
account, so it is *absent* from the chain's bank blocklist — a plain withdrawal
`MsgSend` to `gov` therefore **succeeds and strands the funds** (no keeper can
release them), which makes client-side rejection the only safeguard. `ModuleAccountAddress(name)` / `moduleAccountAddress(name)`
computes further module addresses. Use the strict variant for **withdrawal
destinations** at minimum (FR-032) — the shipped
`examples/external-signer-withdrawal.ts` does exactly this.

## Key derivation (FR-015)

The documented, Cosmos-compatible hierarchical derivation path is:

```
m/44'/118'/{account}'/{change}/{index}     (SLIP-44 coin type 118)
```

Default: `m/44'/118'/0'/0/0`. The kit's `DeriveAddress` implements
BIP39 mnemonic → BIP32 secp256k1 → bech32 for `m/44'/118'/…` paths and
refuses other purposes/coin types, so every address it produces can be
re-derived by standard Cosmos tooling. `test-vectors/derivation.json` pins
the full pipeline (private key, compressed public key, address bytes,
bech32) across account/change/index variations in both languages.

> **UNSAFE_TEST_ONLY.** `DeriveAddress` and every mnemonic in the vector
> files exist for testing and vector generation only. The vector mnemonics
> (`abandon abandon … about`, etc.) are publicly known BIP39 test phrases —
> any funds sent to their addresses on any network are **immediately
> stealable**. Production key generation, storage, and signing structure are
> entirely the exchange's responsibility and stay behind the
> `TransactionSigner` boundary (`contracts/signer-interface.md`); the kit
> never needs, and must never be given, a production mnemonic or private
> key. `sovren-vectors derive --new-test-address` mints throwaway
> faucet/test addresses — same warning applies.

## Deposit-address models (FR-016)

Both models are exchange-controlled policy; the chain supports both.

### 1. Unique address per customer (preferred)

One derived address per customer; the deposit scanner watch-set attributes
any transfer to that address to the customer, no memo needed.

- Attribution is structural — nothing the customer can forget or mistype.
- Memos are ignored for crediting (still recorded for audit).
- Sweeps consolidate to hot/cold wallets on the exchange's schedule.

### 2. Omnibus address + memo

A single deposit address for all customers; the customer supplies an
exchange-assigned memo (numeric customer tag) in the transaction memo field.

- Standard transaction memo support is preserved by the kit's tx builder
  (`BuildMsgSend(..., memo)`), so this model works out of the box.
- Deposits arriving **without** a valid memo must not be silently credited or
  discarded: route them to manual review with a documented reclaim process.
- Publish the memo requirement prominently; memo mistakes are the dominant
  support-ticket source for this model.

**Memo policy:** memo-based deposits are never mandatory at the protocol
level. An exchange choosing model 1 must not reject deposits that happen to
carry a memo, and must never require customers to set one.

## Practical rules for exchange integration

1. Validate on paste (UI) **and** at withdrawal-processing time (server) —
   always on the server's own validation, never trusting the client.
2. Store only normalized addresses; compare addresses only after
   normalization.
3. Never auto-correct: no trimming, no case-fixing beyond the documented
   uppercase→lowercase normalization of fully valid input.
4. Run the strict (prohibited-set) variant for withdrawal destinations;
   include your own hot/cold/omnibus addresses in the set to catch
   self-sends misfiled as customer withdrawals.
5. Regression-test your integration against `test-vectors/addresses.json`
   and `test-vectors/derivation.json` — CI-diffed, deterministic, and
   identical across both reference implementations
   (`test/conformance/run.sh`).
