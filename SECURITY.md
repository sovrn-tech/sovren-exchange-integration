# Security Policy

## Reporting a vulnerability

Report suspected vulnerabilities in this kit or the Sovren network privately to **support@sovrentech.io** (also listed in `docs/contacts.md`). Do **not** open public issues for security reports. You should receive an acknowledgement within 2 business days.

## Scope

- Kit code (Go and TypeScript libraries, reference adapter, certification suite)
- The deposit/withdrawal/sweep safety properties the kit documents (exactly-once crediting, sequence safety, signer isolation)
- Published release artifacts (checksums, container digests)

## Key-handling guarantees

No component of this kit ever requires, stores, logs, or transmits private keys. Transaction construction and signing are separated by the `TransactionSigner` boundary; the only in-process signer ships gated behind an explicit `UNSAFE_TEST_ONLY` flag and refuses to run against mainnet. If you find any code path that violates this, treat it as a critical vulnerability and report it.

## Verifying releases

Every release ships `checksums.txt` (optionally GPG-signed) and `verify-kit.sh`, which re-verifies all artifact checksums and re-runs the standalone build proof. Verify before use.
