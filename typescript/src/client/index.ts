// Thin typed wrappers over CosmJS against the exchange's own node (Comet38 RPC),
// plus a REST fallback for the x/globalfee params route.

import {
  accountFromAny,
  createProtobufRpcClient,
  QueryClient,
  setupAuthExtension,
  setupBankExtension,
  type ProtobufRpcClient,
} from "@cosmjs/stargate";
import { connectComet, comet38, type CometClient } from "@cosmjs/tendermint-rpc";
import { fromHex, toHex } from "@cosmjs/encoding";
import { BinaryReader } from "cosmjs-types/binary.js";
import { BaseAccount } from "cosmjs-types/cosmos/auth/v1beta1/auth.js";
import { DecCoin } from "cosmjs-types/cosmos/base/v1beta1/coin.js";
import type { PageRequest as PageRequestMsg } from "cosmjs-types/cosmos/base/query/v1beta1/pagination.js";
import { ServiceClientImpl, SimulateRequest } from "cosmjs-types/cosmos/tx/v1beta1/service.js";

import {
  GetTxsByAddressRequest,
  GetTxsByAddressResponse,
  OrderBy as TxQueryOrderBy,
  TXQUERY_GET_TXS_BY_ADDRESS,
  TXQUERY_QUERY_SERVICE,
  type GetTxsByAddressResponse as GetTxsByAddressResponseMsg,
} from "../gen/sovr/txquery/v1/query.js";

export type ClientErrorCode =
  | "INVALID_ARGUMENT"
  | "ACCOUNT_NOT_FOUND"
  | "QUERY_FAILED"
  | "GLOBALFEE_UNAVAILABLE";

export class ClientError extends Error {
  readonly code: ClientErrorCode;

  constructor(code: ClientErrorCode, message: string) {
    super(message);
    this.name = "ClientError";
    this.code = code;
  }
}

export interface ClientOpts {
  // REST (grpc-gateway) base URL; required only for the globalFeeParams REST fallback.
  restUrl?: string;
  // Test seam; defaults to global fetch.
  fetchFn?: typeof fetch;
}

export interface AccountInfo {
  address: string;
  accountNumber: bigint;
  sequence: bigint;
}

export interface Coin {
  denom: string;
  amount: string;
}

export type TxLookupResult =
  | { found: false; hash: string }
  | {
      found: true;
      hash: string;
      height: bigint;
      code: number;
      rawLog: string;
      gasUsed: bigint;
      gasWanted: bigint;
      txBytes: Uint8Array;
    };

export interface BroadcastResult {
  txHash: string;
  // CheckTx passed and the tx entered the mempool. False = node-side
  // pre-inclusion rejection (code/codespace/rawLog say why). Transport
  // failures throw instead.
  accepted: boolean;
  code: number;
  codespace: string;
  rawLog: string;
}

export interface SimulateResult {
  gasUsed: bigint;
  gasWanted: bigint;
}

// amount is a decimal gas-price string normalized to 18 fractional digits.
// Opaque precision-preserving representation — never parse as a float.
export interface MinGasPrice {
  denom: string;
  amount: string;
}

export interface TxsByAddressOptions {
  pagination?: PageRequestMsg;
  orderBy?: TxQueryOrderBy | number;
  /** Inclusive lower bound YYYY-MM-DD (UTC). Empty / omitted = none. */
  startDate?: string;
  /** Inclusive upper bound YYYY-MM-DD (UTC). Empty / omitted = none. */
  endDate?: string;
}

export type BlockResponse = comet38.BlockResponse;
// Union across the CometClient backends (Comet 0.38 nodes report
// finalizeBlockEvents; Tendermint 0.37 nodes report begin/end block events).
export type BlockResultsResponse = Awaited<ReturnType<CometClient["blockResults"]>>;

const BASE_DENOM = "usovr";
const DEC_FRACTIONAL_DIGITS = 18;
const DEC_ONE = 10n ** BigInt(DEC_FRACTIONAL_DIGITS);
const TX_HASH_RE = /^(0x)?[0-9a-fA-F]{64}$/;
const BASE_ACCOUNT_TYPE_URL = "/cosmos.auth.v1beta1.BaseAccount";
const GLOBALFEE_QUERY_SERVICE = "sovr.globalfee.v1.Query";
const GLOBALFEE_REST_PATH = "/sovr/globalfee/v1/params";

// LegacyDec wire form: the 10^18-scaled value as a base-10 integer string.
export function decFromScaledInt(scaled: string): string {
  if (!/^[0-9]+$/.test(scaled)) {
    throw new ClientError("QUERY_FAILED", `invalid scaled decimal ${JSON.stringify(scaled)}`);
  }
  const value = BigInt(scaled);
  const integer = value / DEC_ONE;
  const fraction = value % DEC_ONE;
  return `${integer}.${fraction.toString().padStart(DEC_FRACTIONAL_DIGITS, "0")}`;
}

// REST wire form: a plain decimal string, e.g. "0.007500000000000000".
export function normalizeDecString(dec: string): string {
  const match = /^([0-9]+)(?:\.([0-9]+))?$/.exec(dec);
  if (match === null) {
    throw new ClientError("QUERY_FAILED", `invalid decimal ${JSON.stringify(dec)}`);
  }
  const integer = BigInt(match[1]!);
  const fraction = match[2] ?? "";
  if (fraction.length > DEC_FRACTIONAL_DIGITS) {
    throw new ClientError("QUERY_FAILED", `decimal ${JSON.stringify(dec)} exceeds ${DEC_FRACTIONAL_DIGITS} fractional digits`);
  }
  return `${integer}.${fraction.padEnd(DEC_FRACTIONAL_DIGITS, "0")}`;
}

// sovr.globalfee.v1.QueryParamsResponse { Params params = 1 } where
// Params.minimum_gas_prices is field 1 (repeated cosmos.base.v1beta1.DecCoin).
// Hand-decoded: the kit ships no TS stubs for the sovr proto namespace.
export function decodeGlobalFeeParamsResponse(bytes: Uint8Array): MinGasPrice[] {
  const prices: MinGasPrice[] = [];
  const outer = new BinaryReader(bytes);
  while (outer.pos < outer.len) {
    const tag = outer.uint32();
    if (tag >>> 3 === 1 && (tag & 7) === 2) {
      const inner = new BinaryReader(outer.bytes());
      while (inner.pos < inner.len) {
        const innerTag = inner.uint32();
        if (innerTag >>> 3 === 1 && (innerTag & 7) === 2) {
          const coin = DecCoin.decode(inner, inner.uint32());
          prices.push({ denom: coin.denom, amount: decFromScaledInt(coin.amount) });
        } else {
          inner.skip(innerTag & 7);
        }
      }
    } else {
      outer.skip(tag & 7);
    }
  }
  return prices;
}

export function parseGlobalFeeRestResponse(payload: unknown): MinGasPrice[] {
  const params = (payload as { params?: { minimum_gas_prices?: unknown } } | null)?.params;
  const raw = params?.minimum_gas_prices;
  if (!Array.isArray(raw)) {
    throw new ClientError("QUERY_FAILED", "globalfee REST response has no params.minimum_gas_prices array");
  }
  return raw.map((entry) => {
    const { denom, amount } = (entry ?? {}) as { denom?: unknown; amount?: unknown };
    if (typeof denom !== "string" || denom.length === 0 || typeof amount !== "string") {
      throw new ClientError("QUERY_FAILED", `malformed minimum_gas_prices entry ${JSON.stringify(entry)}`);
    }
    return { denom, amount: normalizeDecString(amount) };
  });
}

export class SovrenClient {
  private readonly cometClient: CometClient;
  private readonly opts: ClientOpts;
  private readonly queryClient: QueryClient &
    ReturnType<typeof setupAuthExtension> &
    ReturnType<typeof setupBankExtension>;
  private readonly rpc: ProtobufRpcClient;
  private readonly txService: ServiceClientImpl;

  private constructor(cometClient: CometClient, opts: ClientOpts) {
    this.cometClient = cometClient;
    this.opts = opts;
    this.queryClient = QueryClient.withExtensions(cometClient, setupAuthExtension, setupBankExtension);
    this.rpc = createProtobufRpcClient(this.queryClient);
    this.txService = new ServiceClientImpl(this.rpc);
  }

  static async connect(rpcUrl: string, opts?: ClientOpts): Promise<SovrenClient> {
    return new SovrenClient(await connectComet(rpcUrl), opts ?? {});
  }

  // Dependency-injection / test entry point.
  static withCometClient(cometClient: CometClient, opts?: ClientOpts): SovrenClient {
    return new SovrenClient(cometClient, opts ?? {});
  }

  disconnect(): void {
    this.cometClient.disconnect();
  }

  async account(address: string): Promise<AccountInfo> {
    const anyAccount = await this.queryClient.auth.account(address);
    if (anyAccount === null) {
      throw new ClientError("ACCOUNT_NOT_FOUND", `account ${address} does not exist on chain`);
    }
    if (anyAccount.typeUrl === BASE_ACCOUNT_TYPE_URL) {
      const base = BaseAccount.decode(anyAccount.value);
      return { address: base.address, accountNumber: base.accountNumber, sequence: base.sequence };
    }
    const account = accountFromAny(anyAccount);
    return {
      address: account.address,
      accountNumber: BigInt(account.accountNumber),
      sequence: BigInt(account.sequence),
    };
  }

  async balance(address: string, denom: string = BASE_DENOM): Promise<Coin> {
    const coin = await this.queryClient.bank.balance(address, denom);
    return { denom: coin.denom, amount: coin.amount };
  }

  async allBalances(address: string): Promise<Coin[]> {
    const coins = await this.queryClient.bank.allBalances(address);
    return coins.map((c) => ({ denom: c.denom, amount: c.amount }));
  }

  async tx(hash: string): Promise<TxLookupResult> {
    if (!TX_HASH_RE.test(hash)) {
      throw new ClientError("INVALID_ARGUMENT", `tx hash must be 64 hex characters, got ${JSON.stringify(hash)}`);
    }
    const normalized = hash.replace(/^0x/, "").toUpperCase();
    let response: comet38.TxResponse;
    try {
      response = await this.cometClient.tx({ hash: fromHex(normalized) });
    } catch (err) {
      if (err instanceof Error && /not found/i.test(err.message)) {
        return { found: false, hash: normalized };
      }
      throw err;
    }
    return {
      found: true,
      hash: toHex(response.hash).toUpperCase(),
      height: BigInt(response.height),
      code: response.result.code,
      rawLog: response.result.log ?? "",
      gasUsed: response.result.gasUsed,
      gasWanted: response.result.gasWanted,
      txBytes: response.tx,
    };
  }

  async block(height?: number): Promise<BlockResponse> {
    return this.cometClient.block(height);
  }

  async blockResults(height?: number): Promise<BlockResultsResponse> {
    return this.cometClient.blockResults(height);
  }

  async simulate(txBytes: Uint8Array): Promise<SimulateResult> {
    if (txBytes.length === 0) {
      throw new ClientError("INVALID_ARGUMENT", "txBytes must not be empty");
    }
    const response = await this.txService.Simulate(SimulateRequest.fromPartial({ txBytes }));
    if (response.gasInfo === undefined) {
      throw new ClientError("QUERY_FAILED", "simulate returned no gas info");
    }
    return { gasUsed: response.gasInfo.gasUsed, gasWanted: response.gasInfo.gasWanted };
  }

  async broadcast(txBytes: Uint8Array): Promise<BroadcastResult> {
    if (txBytes.length === 0) {
      throw new ClientError("INVALID_ARGUMENT", "txBytes must not be empty");
    }
    const response = await this.cometClient.broadcastTxSync({ tx: txBytes });
    return {
      txHash: toHex(response.hash).toUpperCase(),
      accepted: response.code === 0,
      code: response.code,
      codespace: response.codespace ?? "",
      rawLog: response.log ?? "",
    };
  }

  async globalFeeParams(): Promise<MinGasPrice[]> {
    let rpcError: unknown;
    try {
      const value = await this.rpc.request(GLOBALFEE_QUERY_SERVICE, "Params", new Uint8Array());
      return decodeGlobalFeeParamsResponse(value);
    } catch (err) {
      rpcError = err;
    }
    if (this.opts.restUrl === undefined) {
      throw new ClientError(
        "GLOBALFEE_UNAVAILABLE",
        `globalfee RPC query failed (${rpcError instanceof Error ? rpcError.message : String(rpcError)}) and no restUrl is configured for fallback`,
      );
    }
    return this.globalFeeParamsRest();
  }

  /**
   * Merged sender-OR-recipient history for an account via sovr.txquery.v1.
   * Pass startDate / endDate (YYYY-MM-DD UTC) to height-bound the query —
   * same fields as the chain proto GetTxsByAddressRequest.
   */
  async txsByAddress(address: string, opts?: TxsByAddressOptions): Promise<GetTxsByAddressResponseMsg> {
    if (address.length === 0) {
      throw new ClientError("INVALID_ARGUMENT", "address must not be empty");
    }
    const req = GetTxsByAddressRequest.fromPartial({
      address,
      ...(opts?.pagination !== undefined ? { pagination: opts.pagination } : {}),
      ...(opts?.orderBy !== undefined ? { orderBy: opts.orderBy } : {}),
      ...(opts?.startDate !== undefined ? { startDate: opts.startDate } : {}),
      ...(opts?.endDate !== undefined ? { endDate: opts.endDate } : {}),
    });
    try {
      const value = await this.rpc.request(
        TXQUERY_QUERY_SERVICE,
        TXQUERY_GET_TXS_BY_ADDRESS,
        GetTxsByAddressRequest.encode(req).finish(),
      );
      return GetTxsByAddressResponse.decode(value);
    } catch (err) {
      throw new ClientError(
        "QUERY_FAILED",
        `txquery GetTxsByAddress failed: ${err instanceof Error ? err.message : String(err)}`,
      );
    }
  }

  private async globalFeeParamsRest(): Promise<MinGasPrice[]> {
    const fetchFn = this.opts.fetchFn ?? fetch;
    const url = `${this.opts.restUrl!.replace(/\/+$/, "")}${GLOBALFEE_REST_PATH}`;
    let response: Response;
    try {
      response = await fetchFn(url);
    } catch (err) {
      throw new ClientError(
        "GLOBALFEE_UNAVAILABLE",
        `globalfee REST fetch failed: ${err instanceof Error ? err.message : String(err)}`,
      );
    }
    if (!response.ok) {
      throw new ClientError("GLOBALFEE_UNAVAILABLE", `globalfee REST returned HTTP ${response.status}`);
    }
    return parseGlobalFeeRestResponse(await response.json());
  }
}
