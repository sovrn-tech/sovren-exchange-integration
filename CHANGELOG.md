# Changelog

All notable changes to the Sovren Exchange Integration Kit are documented here. Versioning follows semver; each release lists the sovrd release it targets and its artifact checksums.

## [Unreleased]

- Align `sovr.txquery.v1.GetTxsByAddressRequest` with chain: optional
  `start_date` / `end_date` (YYYY-MM-DD UTC). Regenerated Go codegen;
  `client.TxsByAddress` accepts optional `TxsByAddressOptions` to pass them
  (and `order_by`) on gRPC and CometRPC transports.
- TypeScript: ship `typescript/src/gen/sovr/txquery/v1/query.ts` wire types
  (encode/decode including fields 4–5) and `SovrenClient.txsByAddress` with
  `startDate` / `endDate` options.
- Initial kit development: scaffolding, Go/TypeScript foundations, proto re-homing.
