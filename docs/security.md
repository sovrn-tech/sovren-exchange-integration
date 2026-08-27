# Security

Security posture of the exchange integration: where keys live (never here),
what the signing boundary verifies, how public endpoints are hardened, and
where the audit evidence is. Vulnerability reporting: `SECURITY.md` (repo
root) and the security contact in `docs/contacts.md`.

## Key isolation

The adapter **never holds withdrawal keys**. Signing is delegated across a
hard process boundary through the `signer.TransactionSigner` interface
(contracts/signer-interface.md):

| Kind | Boundary | Production stance |
|------|----------|-------------------|
| `grpc-remote` | mTLS gRPC to the exchange's signing service / HSM front-end | **the** production path; mTLS is required — `SOVREN_SIGNER_ALLOW_INSECURE_DEV` is refused on mainnet |
| `exec` | subprocess with the sign-doc on stdin, signature on stdout | air-gap-friendly; the binary is the exchange's custody boundary |
| `unsafe-local` | in-process test signer | **refused when the network manifest declares mainnet**; requires `SOVREN_SIGNER_UNSAFE=UNSAFE_TEST_ONLY` |

Consequences of the isolation:

- No key material, mnemonic, or sign-doc secret ever appears in
  `adapter.yaml` (transport credentials arrive via environment variables) or
  in logs — the logging layer enforces a forbidden-field blocklist
  (`internal/logging`, FR-050) and the export pipeline's sanitization scan
  re-checks it.
- A fully compromised adapter can *propose* transactions but cannot sign
  them; the signer system independently re-derives what it is signing (next
  section) and applies its own policy (amount limits, destination
  allowlists, velocity checks).
- Deposit scanning requires no keys at all; a signer outage never affects
  crediting.

## Signing summary verification

Every signing request carries the exact ADR-020 `SignDocBytes` **and** a
derived `SigningSummary` (chain ID, account/sequence, message type, sender,
recipient, amount, denom, fee, gas, memo). Two independent checks make the
summary trustworthy:

1. **Adapter side** — before requesting a signature, the workflow re-derives
   the summary from the sign-doc bytes it just built and quarantines the
   withdrawal on any mismatch with the approved request (amount, recipient,
   fee bound). One `MsgSend` per transaction is enforced by construction
   (FR-036): the summary derivation rejects any doc carrying more than one
   message.
2. **Signer side** — production signers MUST decode `SignDocBytes`
   themselves, re-derive the summary, and refuse to sign on mismatch. The
   summary field exists for display and cross-checking; **the bytes are the
   only authority**. A signer that signs whatever summary it is shown has no
   security boundary.

After signing, the adapter verifies the signature over the sign-doc against
the expected key and sender address before broadcast
(`withdrawals.VerifySignedResponse`); the signed bytes are persisted so
recovery paths rebroadcast the identical transaction and never re-sign
(FR-035).

## Operational hardening (adapter deployment)

- Admin API (`admin.listen`): bind to localhost or a private interface only;
  front with mTLS or bearer authentication at the deployment layer; every
  mutation is audit-logged (FR-051).
- Metrics (`:9464`): scrape-network only; it exposes operational posture
  (balances, queue depths) that is useful to an attacker.
- Storage DSN secrets arrive via environment expansion only and are never
  committed (config contract).
- Run the scanner against exchange-operated nodes; the two-node disagreement
  monitor (FR-044) exists precisely so a single lying node cannot drive
  crediting.

## FR-063 — Public-endpoint hardening attestation

FR-063 requires the published public query endpoints (`rpc.sovrchain.net`,
`api.sovrchain.net`) to be hardened: TLS, rate limiting, DoS protection,
request-size and connection limits, method restrictions, monitoring, and
geographic redundancy — and that **no validator consensus node is publicly
routable** (validators sit behind sentries; see the sentry topology
runbook).

> **Attestation — verified by Daniel Lockhart (Director - Software Engineering, Sovren Technologies); baseline signed 2026-08-06, updated 2026-08-10.**
> **Status: complete.** Every FR-063 control is either **verified in force**
> or **formally waived** by the signed waiver recorded below — no control is
> silently unmet. Verified against the live edge configuration and external
> probes of both published endpoints. Evidence below is kept public-safe (no
> origin hostnames, node identifiers, or addresses).

Per-property status for **both** `rpc.sovrchain.net` (CometBFT RPC) and
`api.sovrchain.net` (Cosmos REST):

| FR-063 property | Status | Evidence / note |
|-----------------|--------|-----------------|
| **TLS** | Enforced | Minimum TLS 1.2, TLS 1.3 enabled, all HTTP redirected to HTTPS. Verified live: TLS 1.3 negotiated, HTTP/2, valid certificate chain. Edge↔origin is encrypted (Full mode; origin certificate is not chain-validated — *Strict* is a future hardening step). HSTS is not currently set. |
| **DoS / volumetric protection** | Baseline | Both endpoints are served through a global CDN/proxy edge with always-on network-layer (L3/4) and application-layer (L7) DDoS mitigation. No custom WAF ruleset is configured for this zone. |
| **Request rate limiting** | Enforced (verified live 2026-08-10) | Per-IP rate-limit rules are configured at the edge for both endpoints: a tighter limit on the transaction-broadcast routes and a general limit on ordinary queries, both returning HTTP 429 on breach. Verified by a bounded probe against the broadcast route, which began returning 429 at request 122 — matching the configured threshold. Counters are per-IP per-datacenter. |
| **Request-size limits** | Edge defaults | Enforced at the edge by the account plan's default request-size limits; no custom override. |
| **Connection limits** | Edge-managed | Concurrent/per-client connection handling is managed at the edge; no custom origin-side cap is configured. |
| **Method restrictions** | Enforced for unsafe methods; query-only **formally waived** (2026-08-10) | Unsafe CometBFT RPC methods are not reachable — `dial_peers`, `dial_seeds`, and `unsafe_flush_mempool` return HTTP 404; the `TRACE` method returns 405; ordinary query methods return 200. **The REST endpoint is not query-only:** `POST /cosmos/tx/v1beta1/txs` reaches the standard Cosmos transaction-broadcast handler (an empty body returns `400 invalid empty tx`), so a validly signed transaction can be broadcast through the public endpoint. That is stock Cosmos behaviour and a legitimate API function — every transaction still requires a valid signature, so the endpoint cannot create or authorize a transfer. Waived under the recorded FR-063 waiver (below); the exposure is rate-limited broadcast spam, bounded by the dedicated broadcast rate limit and the network minimum gas-price floor. |
| **Monitoring** | In place | Both endpoints are probed by independent external uptime monitoring, separate from the gateway. (Formal SLO measurement is tracked separately as plan D11.) |
| **Geographic redundancy** | **Formally waived** (2026-08-10) | The serving edge is a globally-distributed anycast network, but these are dynamic RPC/REST endpoints with no cacheable surface: effectively every request reaches a **single origin gateway**, so an outage of that origin or its region takes **both** published endpoints down. One failure domain is not geographic redundancy. This is an **availability** exposure only — no integrity or custody impact. Waived under the recorded FR-063 waiver (below), because exchange-operated nodes are the mandated redundancy path under FR-006/FR-013 and the integration never depended on Sovren endpoint redundancy. |

**No validator consensus node is publicly routable.** The public
`sovrchain.net` DNS zone exposes only seed and sentry P2P endpoints (for
peer discovery and validator shielding) and the proxied query endpoints
(`rpc`, `api`, `grpc`). There is **no** validator (`val-*`) record in public
DNS. Public query traffic terminates at the CDN edge and is proxied to a
full-node gateway; validators sit behind sentries and are never directly
addressable from a public route (see `mainnet-runbook/sentry-topology.md`).

### Recorded FR-063 waiver (2026-08-10)

Two FR-063 controls are **not** met and are **formally waived** by a signed,
recorded decision rather than left as silent gaps:

1. **Geographic redundancy.** Every request to these dynamic endpoints reaches
   a single origin gateway, so that origin and its region are one failure
   domain for both published endpoints.
2. **Method restrictions, insofar as FR-063 would require the public REST
   endpoint to be query-only.** The standard Cosmos transaction-broadcast
   route is reachable (above).

**Basis for acceptance.** Neither exposure touches integrity or custody. The
first is availability-only, and the integration's redundancy model never
depended on Sovren endpoint redundancy: **FR-006** already requires the
manifest to document the shared-infrastructure fact and designates
**exchange-operated nodes as the mandated redundancy path for custody-critical
operations**, which **FR-013** reinforces. The second is stock Cosmos
behaviour on a signed, fee-paying, rate-limited route — the exposure is
broadcast spam, not unauthorized value movement, and restricting it would
break legitimate integrators.

**Compensating controls in force:** exchange-run nodes as the mandated
redundancy path (with the kit shipping everything needed to run one
independently — node distribution, genesis + checksum, peers, deployment
examples); a dedicated tighter rate limit on the broadcast routes plus a
general query limit; the network minimum gas-price floor; baseline CDN DDoS
protection; independent external uptime monitoring against a 99.9%/month
availability objective (SC-011); and a published status page and incident
channel.

**Not waived:** every other FR-063 control above remains in force and verified
— including the assertion that **no validator consensus node is publicly
routable**.

**Review.** The waiver is not open-ended. It is revised or revoked on the
earliest of: an infrastructure-independent second endpoint being delivered (at
which point the redundancy control is simply met); 12 months (by 2027-08-10) at
the next attestation cycle; or a triggering event — a sustained origin outage
materially affecting a live integration, any integrator adopting these public
endpoints as a production data plane contrary to FR-006/FR-013 guidance, or
broadcast abuse the rate limit does not adequately bound.

Separately, HSTS is not set and edge↔origin TLS is Full rather than Strict.
Those are hardening improvements, not FR-063-named controls, and are tracked
outside this waiver.

Exchanges should nonetheless run **their own nodes** for production traffic
(quickstart R2); the public endpoints are bootstrap and fallback
infrastructure, not the integration's data plane.

*Signed: Daniel Lockhart, Director - Software Engineering, Sovren Technologies
— attestation 2026-08-06, FR-063 waiver 2026-08-10.*

## Audit reports (D9)

The exchange-surface security review (FR-062 — including the module-account
blocklist review and confirmation that plain transfers preserve the
closed-loop supply invariants) ships **inside the kit** under
`exchange-kit/audit/` (plan D9); the export pipeline injects the report and
its checksum at release. Until D9 closes, `audit/` carries the review scope
and status stub — a kit without a completed D9 report is not GA (SC-010).
Verify the report's checksum against `checksums.txt` like any other
artifact.
