// Sovren Exchange Integration Kit — TypeScript client library.
// Public surface per specs/008-exchange-integration-kit/contracts/typescript-client-api.md.
// Modules are populated by their owning tasks (T016, T037, T046, T054).

export * from "./address/index.js";
export * from "./amounts/index.js";
export * from "./client/index.js";
export * from "./tx/index.js";
export * from "./signer/index.js";
export * from "./deposits/index.js";
export * from "./sequences/index.js";
export {
  GetTxsByAddressRequest,
  GetTxsByAddressResponse,
  OrderBy as TxQueryOrderBy,
  TXQUERY_GET_TXS_BY_ADDRESS,
  TXQUERY_QUERY_SERVICE,
  TXQUERY_REST_PATH_PREFIX,
  type GetTxsByAddressRequest as GetTxsByAddressRequestMsg,
  type GetTxsByAddressResponse as GetTxsByAddressResponseMsg,
} from "./gen/sovr/txquery/v1/query.js";

export const KIT_NAME = "@sovren/exchange-integration";

// Chain constants (verified against the live network manifest at release — FR-007).
export const CHAIN_CONSTANTS = {
  bech32Prefix: "sovr",
  validatorOperatorPrefix: "sovrvaloper",
  validatorConsensusPrefix: "sovrvalcons",
  baseDenom: "usovr",
  displayDenom: "SOVR",
  displayExponent: 6,
  slip44CoinType: 118,
  defaultHdPath: "m/44'/118'/0'/0/0",
} as const;
