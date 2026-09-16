# Exchange-surface security review — completion certification (plan D9)

**Date:** 2026-09-11 · **Scope:** FR-062 / SC-010 (plan D9) · **Chain:** sovr-1
(Cosmos SDK v0.53.x) · **Kit:** Sovren Exchange Integration Kit

> **What this document is.** This is Sovren Technologies' **self-certification**
> that the exchange-surface security review required by plan D9 (FR-062 / SC-010)
> is **complete**. It is deliberately explicit about how that conclusion was
> reached, because D9 was originally defined as an *externally countersigned*
> report and this certification is issued **without** that external
> countersignature — see **§6, Deviation from the original D9 gate**. An exchange
> performing due diligence should read that section so it knows precisely what
> assurance this artifact does and does not carry.

## 1. Governing statement (SC-010)

> An ordinary native SOVR transfer — a plain `cosmos.bank.v1beta1.MsgSend` or
> `MsgMultiSend` to a normal account — does **not** mint, burn, or otherwise
> alter total supply; does **not** trigger the bridge or the 50/25/25 settlement
> split; and does **not** bypass or interact with lockups/vesting. Ordinary
> transfers preserve the chain's fixed-supply closed-loop invariant
> (`latent + locked + vesting + claimed == 1,000,000,000 SOVR`, enforced by
> `x/supply`; burns return to latent rather than destroying tokens).

**Certification: the governing statement holds** on the evidence reviewed,
across all seven FR-062 dimensions in §3.

## 2. Basis of this certification

The exchange-surface review was conducted in two complementary passes, and every
issue either pass raised has been remediated in the shipped code:

1. **Independent external security review.** An independent external security
   reviewer examined the native SOVR integration surface. The findings that
   review produced were triaged, accepted, and **fully remediated in the
   codebase** (§4). The external reviewer **did not provide a report
   countersigned into this repository**; obtaining that countersignature is the
   one deliverable D9 originally called for that could not be secured (§6).
2. **Internal adversarial pre-audit review.** Sovren engineering ran a
   structured adversarial review of the same surface — seven FR-062 dimensions,
   each reviewed against the code and then independently re-checked by a second
   reviewer instructed to refute the verdict and hunt a concrete counterexample.
   Every load-bearing conclusion was cited to source and spot-checked by hand.
   The full internal review is on file with Sovren engineering.

This certification records Sovren's own review of that combined evidence and its
conclusion that the D9 scope is satisfied in substance. It is signed by an
authorized Sovren officer (§7).

## 3. FR-062 results by dimension

| # | Dimension | Result | Basis (public-safe summary) |
|---|-----------|--------|------------------------------|
| 1 | Native-transfer path | **Upheld** | No `SendRestriction` or transfer hook is installed anywhere; the chain uses the stock Cosmos `bankkeeper.Keeper`. The only bank customization is the canonical **recipient-side block on module accounts** — it never affects an ordinary user→user transfer. The SDK PostHandler is the empty default. |
| 2 | Ante / fee handling | **Upheld** | The only coin-moving ante decorator is the stock fee-deduction step routing to the standard `fee_collector` — **not** the settlement split. The `x/globalfee` ante is validate-only (a minimum-gas-price floor); it moves no coins and cannot alter supply. |
| 3 | Supply invariant | **Upheld** | The bank send path has no route into `x/supply`. Every mint and burn site on the chain is reachable only via genesis, a Begin/EndBlock hook, or a distinct module message — never a plain `MsgSend`. A full mint/burn inventory was enumerated and confirmed unreachable from the transfer path (§4, info items). Holding a mint/burn permission is not a call site. |
| 4 | Module-account blocklist | **Upheld (after remediation)** | The chain's blocked-recipient set is auto-derived from module permissions (complete by construction), including fund-holding sub-accounts. The kit ships a client-side prohibited-account list in **both** languages (`go/address/address.go`, `typescript/src/address/`) that mirrors the chain set, reconciled for two intentional differences (a legacy renamed-module address blocked chain-side, and `gov` added as a client-only extra). Two gaps found in review were fixed (§4). |
| 5 | Upgrade handlers | **Upheld** | Every shipped upgrade handler is additive schema/param/metadata/index work; **no** handler credits or debits a user account. One historical handler mints solely to repair a bank↔supply deficit into a module account (restoring the 1B cap); no user balance is touched. |
| 6 | Genesis / chain-id / denom / address | **Upheld** | Address prefix `sovr`, coin type 118, bond denom `usovr`, on-chain bank metadata (base `usovr` / display `sovr` / exponent 6) are all consistent with the kit's static config, registry, and committed genesis + checksums. |
| 7 | Bridge / settlement / lockup / IBC isolation | **Upheld** | Each subsystem is reachable only via its own distinct message type or chain-automated Begin/EndBlock — never from the bank send path. There is no `MsgSend` override anywhere in the chain application. |

## 4. Findings and remediation

All findings raised by either review pass are resolved in the shipped kit:

| Severity | Finding | Status |
|----------|---------|--------|
| **HIGH** | The TypeScript withdrawal path shipped **no** module-account blocklist — a module account (e.g. the fee collector) could be signed and broadcast as a withdrawal destination, contradicting FR-014/FR-032. | **Fixed** — canonical prohibited-account list + defaults added to the TS address module, wired into the example, covered by a TS test that rejects every module account, plus a cross-language parity check. |
| **MEDIUM** | The chain-side drift guard verified only the chain test's copy of the blocked-account list — it did not check the kit's actual Go/TS lists or pin the legacy renamed-module address, so the kit could silently drift from the chain. | **Fixed** — the drift guard is now a multi-leg check tying the chain's authoritative set to **both** kit languages' real lists (parsed from source) and pinning the legacy address constant; a chain change now fails CI until every mirror is updated. |
| info | The original internal mint/burn inventory was incomplete. | **Corrected** — the complete inventory (all mint sites and the single burn site) is enumerated and each confirmed unreachable from a plain transfer. |
| info | Client-side blocklist coverage was not reflected in the TypeScript client-API contract / address-generation docs, so an integrator following the docs could re-introduce bare validation. | **Fixed** — both documents now document the API and an exported-API test asserts it is importable. |
| info | A small number of documented, no-change items (a `gov` account is unblocked chain-side but a plain transfer to it neither burns nor creates a deposit; case-insensitive display-denom reconciliation; the historical repair-mint above). | No change — documented; consistent with the shipped guidance. |

The module-account blocklist review specifically confirms the kit's
`DefaultProhibitedModuleAccounts()` (36 entries — 35 module-account names plus
one pinned legacy renamed-module address) is reconciled against the chain's 35
authoritative blocked names by the (now green) chain-side drift guard, across
both the Go and TypeScript kits. (The kit set differs from the chain set by two
intentional, guard-enforced reconciliations: the legacy renamed-module account
is pinned by address rather than by its retired name, and `gov` is a client-only
extra.)

## 5. Scope and limitations

- **In scope:** the on-chain exchange surface (native-transfer path, ante/fee,
  supply invariant, upgrade handlers, genesis/denom/address/chain-id config,
  bridge/settlement/lockup/IBC isolation) and the kit's client-side
  module-account blocklist in both languages.
- **Out of scope:** the adapter's deposit-crediting and withdrawal state
  machines (covered separately by the certification suite groups D/W/S/R/C) and
  the bridge relayer. The bridge ships dormant/fail-closed and is not part of the
  native listing surface.
- This is a code-and-configuration review of the integration surface. It asserts
  the structural isolation of ordinary transfers from supply mutation, the
  settlement split, the bridge, and lockups; it is not a runtime penetration test
  of an exchange's own deployment, which remains the exchange's responsibility
  (see `docs/security.md`, operational hardening).

## 6. Deviation from the original D9 gate — recorded risk acceptance

Plan D9 as originally written required an **independent, externally
countersigned** report committed to `exchange-kit/audit/`. The technical review
was performed (§2) and every finding remediated (§4), but the external reviewer
**declined to provide a report countersigned into this repository**. Rather than
leave D9 as a silent open gap, Sovren records the following explicit decision:

- **What is not present:** an external party's signature attesting to the review
  in this repository. This certification therefore does **not** carry independent
  third-party accountability for its conclusions.
- **What is present and relied upon:** a completed external review whose findings
  were all remediated; a documented internal adversarial review reaching the same
  SC-010 verdict; and the structural, testable guarantees that back that verdict
  — chiefly that ordinary transfers use the stock bank keeper with no hooks, and
  that a CI drift guard mechanically keeps the kit's client-side blocklist in
  lock-step with the chain across both languages.
- **Basis for acceptance:** the SC-010 conclusion is a **structural** property of
  a deliberately minimal surface (plain `MsgSend`, no custom transfer module, no
  wrapped denom, no bridge/IBC in the native path), independently reproducible by
  any reviewer from the shipped kit and the public chain. The residual risk of
  self-certifying is the loss of independent sign-off, not an unreviewed surface.
- **Authorization:** this risk acceptance was reviewed with, and approved by,
  Sovren's executive team before issuance.
- **Review / revocation:** this certification is superseded immediately if an
  externally countersigned report is later obtained, and is re-examined at the
  next security attestation cycle or on any material change to the native-transfer
  surface (a new transfer hook, a `MsgSend`/`SendCoins` override, a bridge/IBC
  change to the native path, or a supply-affecting upgrade handler).

Exchanges that require an independent third-party audit as a listing precondition
should treat this section as the disclosure of that gap and may commission their
own review; the shipped kit and the public chain make the SC-010 property fully
reproducible.

## 7. Certification

I certify, on behalf of Sovren Technologies, that the exchange-surface security
review required by plan D9 (FR-062 / SC-010) is complete on the evidence
described above; that the governing SC-010 statement in §1 holds; that all
findings in §4 are remediated in the shipped kit; and that the deviation and
risk acceptance in §6 are accurate and authorized.

*Signed: Daniel Lockhart, Director — Software Engineering, Sovren Technologies —
2026-09-11.*
