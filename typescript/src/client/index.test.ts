import { toHex } from "@cosmjs/encoding";
import type { CometClient } from "@cosmjs/tendermint-rpc";
import { BinaryWriter } from "cosmjs-types/binary.js";
import { BaseAccount } from "cosmjs-types/cosmos/auth/v1beta1/auth.js";
import { QueryAccountResponse } from "cosmjs-types/cosmos/auth/v1beta1/query.js";
import { DecCoin } from "cosmjs-types/cosmos/base/v1beta1/coin.js";
import { SimulateResponse } from "cosmjs-types/cosmos/tx/v1beta1/service.js";
import { describe, expect, it } from "vitest";

import {
  ClientError,
  decFromScaledInt,
  decodeGlobalFeeParamsResponse,
  normalizeDecString,
  parseGlobalFeeRestResponse,
  SovrenClient,
} from "./index.js";

const ADDR_A = "sovr1jhg0e7s6gn44tfc5k37kr04sznyhedtcqngn68";
const TX_HASH = "AB".repeat(32);

function mockComet(overrides: Record<string, unknown>): CometClient {
  return { disconnect: () => undefined, ...overrides } as unknown as CometClient;
}

function abciOk(value: Uint8Array) {
  return { code: 0, value, height: 10, key: new Uint8Array(), codespace: "", info: "", log: "" };
}

// sovr.globalfee.v1.QueryParamsResponse{params: {minimum_gas_prices, bypass list, max gas}}
function encodeGlobalFeeResponse(coins: { denom: string; amount: string }[]): Uint8Array {
  const writer = new BinaryWriter();
  const params = writer.uint32(10).fork();
  for (const coin of coins) {
    DecCoin.encode(DecCoin.fromPartial(coin), params.uint32(10).fork()).ldelim();
  }
  params.uint32(18).string("/ibc.core.client.v1.MsgUpdateClient");
  params.uint32(24).uint64(2_000_000);
  params.ldelim();
  return writer.finish();
}

describe("decFromScaledInt", () => {
  const cases: { input: string; expected: string }[] = [
    { input: "7500000000000000", expected: "0.007500000000000000" },
    { input: "1000000000000000000", expected: "1.000000000000000000" },
    { input: "1250000000000000000", expected: "1.250000000000000000" },
    { input: "0", expected: "0.000000000000000000" },
    { input: "1", expected: "0.000000000000000001" },
  ];
  for (const c of cases) {
    it(`${c.input} -> ${c.expected}`, () => {
      expect(decFromScaledInt(c.input)).toBe(c.expected);
    });
  }

  for (const bad of ["", "-1", "1.5", "abc", "1e18"]) {
    it(`rejects ${JSON.stringify(bad)}`, () => {
      expect(() => decFromScaledInt(bad)).toThrowError(ClientError);
    });
  }
});

describe("normalizeDecString", () => {
  const cases: { input: string; expected: string }[] = [
    { input: "0.0075", expected: "0.007500000000000000" },
    { input: "0.007500000000000000", expected: "0.007500000000000000" },
    { input: "1", expected: "1.000000000000000000" },
    { input: "00.5", expected: "0.500000000000000000" },
    { input: "12.000000000000000001", expected: "12.000000000000000001" },
  ];
  for (const c of cases) {
    it(`${c.input} -> ${c.expected}`, () => {
      expect(normalizeDecString(c.input)).toBe(c.expected);
    });
  }

  for (const bad of ["", "-0.5", "1.2.3", "abc", ".5", "1.", "0.0000000000000000001"]) {
    it(`rejects ${JSON.stringify(bad)}`, () => {
      expect(() => normalizeDecString(bad)).toThrowError(ClientError);
    });
  }
});

describe("decodeGlobalFeeParamsResponse", () => {
  it("decodes minimum gas prices and skips unknown fields", () => {
    const bytes = encodeGlobalFeeResponse([
      { denom: "usovr", amount: "7500000000000000" },
      { denom: "uother", amount: "1000000000000000000" },
    ]);
    expect(decodeGlobalFeeParamsResponse(bytes)).toEqual([
      { denom: "usovr", amount: "0.007500000000000000" },
      { denom: "uother", amount: "1.000000000000000000" },
    ]);
  });

  it("returns empty for an empty floor", () => {
    expect(decodeGlobalFeeParamsResponse(encodeGlobalFeeResponse([]))).toEqual([]);
    expect(decodeGlobalFeeParamsResponse(new Uint8Array())).toEqual([]);
  });
});

describe("parseGlobalFeeRestResponse", () => {
  it("parses and normalizes amounts", () => {
    expect(
      parseGlobalFeeRestResponse({
        params: {
          minimum_gas_prices: [{ denom: "usovr", amount: "0.007500000000000000" }],
          bypass_min_fee_msg_types: [],
          max_total_bypass_min_fee_msg_gas_usage: "2000000",
        },
      }),
    ).toEqual([{ denom: "usovr", amount: "0.007500000000000000" }]);
  });

  const badPayloads: { name: string; payload: unknown }[] = [
    { name: "null", payload: null },
    { name: "no params", payload: {} },
    { name: "non-array prices", payload: { params: { minimum_gas_prices: "x" } } },
    { name: "missing denom", payload: { params: { minimum_gas_prices: [{ amount: "1" }] } } },
    { name: "numeric amount", payload: { params: { minimum_gas_prices: [{ denom: "usovr", amount: 1 }] } } },
  ];
  for (const c of badPayloads) {
    it(`rejects ${c.name}`, () => {
      expect(() => parseGlobalFeeRestResponse(c.payload)).toThrowError(ClientError);
    });
  }
});

describe("SovrenClient.broadcast", () => {
  const hash = Uint8Array.from(Buffer.from(TX_HASH.toLowerCase(), "hex"));

  it("maps CheckTx acceptance", async () => {
    const client = SovrenClient.withCometClient(
      mockComet({ broadcastTxSync: async () => ({ code: 0, hash, codespace: "", log: "" }) }),
    );
    expect(await client.broadcast(Uint8Array.from([1]))).toEqual({
      txHash: TX_HASH,
      accepted: true,
      code: 0,
      codespace: "",
      rawLog: "",
    });
  });

  it("maps CheckTx rejection without throwing", async () => {
    const client = SovrenClient.withCometClient(
      mockComet({
        broadcastTxSync: async () => ({ code: 13, hash, codespace: "sdk", log: "insufficient fee" }),
      }),
    );
    expect(await client.broadcast(Uint8Array.from([1]))).toEqual({
      txHash: TX_HASH,
      accepted: false,
      code: 13,
      codespace: "sdk",
      rawLog: "insufficient fee",
    });
  });

  it("rejects empty tx bytes", async () => {
    const client = SovrenClient.withCometClient(mockComet({}));
    await expect(client.broadcast(new Uint8Array())).rejects.toMatchObject({ code: "INVALID_ARGUMENT" });
  });
});

describe("SovrenClient.tx", () => {
  it("rejects malformed hashes", async () => {
    const client = SovrenClient.withCometClient(mockComet({}));
    await expect(client.tx("nope")).rejects.toMatchObject({ code: "INVALID_ARGUMENT" });
  });

  it("maps a not-found RPC error to found: false", async () => {
    const client = SovrenClient.withCometClient(
      mockComet({
        tx: async () => {
          throw new Error(`tx (${TX_HASH}) not found`);
        },
      }),
    );
    expect(await client.tx(`0x${TX_HASH.toLowerCase()}`)).toEqual({ found: false, hash: TX_HASH });
  });

  it("maps a found tx", async () => {
    const txBytes = Uint8Array.from([9, 9, 9]);
    const client = SovrenClient.withCometClient(
      mockComet({
        tx: async (params: { hash: Uint8Array }) => ({
          hash: params.hash,
          height: 1234,
          index: 0,
          tx: txBytes,
          result: { code: 0, log: "ok", gasUsed: 51234n, gasWanted: 200000n, events: [] },
        }),
      }),
    );
    expect(await client.tx(TX_HASH)).toEqual({
      found: true,
      hash: TX_HASH,
      height: 1234n,
      code: 0,
      rawLog: "ok",
      gasUsed: 51234n,
      gasWanted: 200000n,
      txBytes,
    });
  });

  it("rethrows non-not-found errors", async () => {
    const client = SovrenClient.withCometClient(
      mockComet({
        tx: async () => {
          throw new Error("connection refused");
        },
      }),
    );
    await expect(client.tx(TX_HASH)).rejects.toThrowError("connection refused");
  });
});

describe("SovrenClient.simulate", () => {
  it("returns gas usage as bigint", async () => {
    const value = SimulateResponse.encode(
      SimulateResponse.fromPartial({ gasInfo: { gasUsed: 51234n, gasWanted: 200000n } }),
    ).finish();
    const client = SovrenClient.withCometClient(
      mockComet({
        abciQuery: async (params: { path: string }) => {
          expect(params.path).toBe("/cosmos.tx.v1beta1.Service/Simulate");
          return abciOk(value);
        },
      }),
    );
    expect(await client.simulate(Uint8Array.from([1, 2]))).toEqual({ gasUsed: 51234n, gasWanted: 200000n });
  });

  it("rejects empty tx bytes", async () => {
    const client = SovrenClient.withCometClient(mockComet({}));
    await expect(client.simulate(new Uint8Array())).rejects.toMatchObject({ code: "INVALID_ARGUMENT" });
  });
});

describe("SovrenClient.account", () => {
  it("decodes a BaseAccount with bigint fields", async () => {
    const value = QueryAccountResponse.encode(
      QueryAccountResponse.fromPartial({
        account: {
          typeUrl: "/cosmos.auth.v1beta1.BaseAccount",
          value: BaseAccount.encode(
            BaseAccount.fromPartial({ address: ADDR_A, accountNumber: 42n, sequence: 7n }),
          ).finish(),
        },
      }),
    ).finish();
    const client = SovrenClient.withCometClient(
      mockComet({
        abciQuery: async (params: { path: string }) => {
          expect(params.path).toBe("/cosmos.auth.v1beta1.Query/Account");
          return abciOk(value);
        },
      }),
    );
    expect(await client.account(ADDR_A)).toEqual({ address: ADDR_A, accountNumber: 42n, sequence: 7n });
  });
});

describe("SovrenClient.globalFeeParams", () => {
  const encoded = encodeGlobalFeeResponse([{ denom: "usovr", amount: "7500000000000000" }]);
  const expected = [{ denom: "usovr", amount: "0.007500000000000000" }];

  it("uses the RPC abci_query route when present", async () => {
    const client = SovrenClient.withCometClient(
      mockComet({
        abciQuery: async (params: { path: string }) => {
          expect(params.path).toBe("/sovr.globalfee.v1.Query/Params");
          return abciOk(encoded);
        },
      }),
    );
    expect(await client.globalFeeParams()).toEqual(expected);
  });

  it("falls back to REST when the RPC route is absent", async () => {
    const fetched: string[] = [];
    const client = SovrenClient.withCometClient(
      mockComet({
        abciQuery: async () => {
          throw new Error("Query failed with (6): unknown query path");
        },
      }),
      {
        restUrl: "http://rest.invalid/",
        fetchFn: (async (url: string | URL | Request) => {
          fetched.push(String(url));
          return new Response(
            JSON.stringify({ params: { minimum_gas_prices: [{ denom: "usovr", amount: "0.0075" }] } }),
            { status: 200, headers: { "content-type": "application/json" } },
          );
        }) as typeof fetch,
      },
    );
    expect(await client.globalFeeParams()).toEqual(expected);
    expect(fetched).toEqual(["http://rest.invalid/sovr/globalfee/v1/params"]);
  });

  it("fails typed when the RPC route is absent and no restUrl is set", async () => {
    const client = SovrenClient.withCometClient(
      mockComet({
        abciQuery: async () => {
          throw new Error("unknown query path");
        },
      }),
    );
    await expect(client.globalFeeParams()).rejects.toMatchObject({ code: "GLOBALFEE_UNAVAILABLE" });
  });

  it("fails typed on a REST error status", async () => {
    const client = SovrenClient.withCometClient(
      mockComet({
        abciQuery: async () => {
          throw new Error("unknown query path");
        },
      }),
      {
        restUrl: "http://rest.invalid",
        fetchFn: (async () => new Response("{}", { status: 500 })) as typeof fetch,
      },
    );
    await expect(client.globalFeeParams()).rejects.toMatchObject({ code: "GLOBALFEE_UNAVAILABLE" });
  });
});

describe("SovrenClient.balance mapping", () => {
  it("hex-normalizes nothing and defaults denom to usovr", async () => {
    // bank.balance goes through abci_query; assert the request path and echo a coin.
    const { QueryBalanceRequest, QueryBalanceResponse } = await import(
      "cosmjs-types/cosmos/bank/v1beta1/query.js"
    );
    const client = SovrenClient.withCometClient(
      mockComet({
        abciQuery: async (params: { path: string; data: Uint8Array }) => {
          expect(params.path).toBe("/cosmos.bank.v1beta1.Query/Balance");
          const request = QueryBalanceRequest.decode(params.data);
          expect(request.denom).toBe("usovr");
          expect(request.address).toBe(ADDR_A);
          return abciOk(
            QueryBalanceResponse.encode(
              QueryBalanceResponse.fromPartial({ balance: { denom: "usovr", amount: "123456" } }),
            ).finish(),
          );
        },
      }),
    );
    expect(await client.balance(ADDR_A)).toEqual({ denom: "usovr", amount: "123456" });
  });
});

describe("hex fixture sanity", () => {
  it("round-trips the tx hash fixture", () => {
    expect(toHex(Uint8Array.from(Buffer.from(TX_HASH, "hex"))).toUpperCase()).toBe(TX_HASH);
  });
});

describe("SovrenClient.txsByAddress", () => {
  it("encodes startDate/endDate on the abci_query request", async () => {
    const { GetTxsByAddressRequest } = await import("../gen/sovr/txquery/v1/query.js");
    let capturedData: Uint8Array | undefined;
    const client = SovrenClient.withCometClient(
      mockComet({
        abciQuery: async (params: { path: string; data: Uint8Array }) => {
          expect(params.path).toBe("/sovr.txquery.v1.Query/GetTxsByAddress");
          capturedData = params.data;
          // empty response (no fields) is valid
          return abciOk(new Uint8Array());
        },
      }),
    );
    const resp = await client.txsByAddress(ADDR_A, {
      startDate: "2026-07-01",
      endDate: "2026-08-03",
    });
    expect(resp.total).toBe(0n);
    expect(capturedData).toBeDefined();
    const req = GetTxsByAddressRequest.decode(capturedData!);
    expect(req.address).toBe(ADDR_A);
    expect(req.startDate).toBe("2026-07-01");
    expect(req.endDate).toBe("2026-08-03");
  });
});
