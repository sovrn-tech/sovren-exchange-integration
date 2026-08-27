// TypeScript wire types for sovr.txquery.v1 — keep in lock-step with
// exchange-kit/proto/sovr/txquery/v1/query.proto (and the Go package under
// go/gen/sovr/txquery/v1). The kit does not yet run protoc-ts against this
// file; regenerate / edit when the proto changes, especially start_date /
// end_date (fields 4–5).

import { BinaryReader, BinaryWriter } from "cosmjs-types/binary.js";
import {
  PageRequest,
  PageResponse,
  type PageRequest as PageRequestMsg,
  type PageResponse as PageResponseMsg,
} from "cosmjs-types/cosmos/base/query/v1beta1/pagination.js";

/** Mirrors cosmos.tx.v1beta1.OrderBy; 0 = UNSPECIFIED (server treats as ASC). */
export enum OrderBy {
  ORDER_BY_UNSPECIFIED = 0,
  ORDER_BY_ASC = 1,
  ORDER_BY_DESC = 2,
  UNRECOGNIZED = -1,
}

/**
 * GetTxsByAddressRequest is the request type for Query/GetTxsByAddress.
 * Field numbers: 1 address, 2 pagination, 3 order_by, 4 start_date, 5 end_date.
 */
export interface GetTxsByAddressRequest {
  address: string;
  pagination?: PageRequestMsg;
  orderBy: OrderBy;
  /** Inclusive lower bound YYYY-MM-DD (UTC calendar day). Empty = no lower bound. */
  startDate: string;
  /** Inclusive upper bound YYYY-MM-DD (UTC calendar day). Empty = no upper bound. */
  endDate: string;
}

export interface GetTxsByAddressResponse {
  /** Opaque rows for now — consumers that need full Tx decode can use cosmjs against the ABCI bytes. */
  txs: Uint8Array[];
  txResponses: Uint8Array[];
  pagination?: PageResponseMsg;
  total: bigint;
}

function createBaseRequest(): GetTxsByAddressRequest {
  return { address: "", orderBy: OrderBy.ORDER_BY_UNSPECIFIED, startDate: "", endDate: "" };
}

export const GetTxsByAddressRequest = {
  typeUrl: "/sovr.txquery.v1.GetTxsByAddressRequest" as const,

  encode(message: GetTxsByAddressRequest, writer: BinaryWriter = new BinaryWriter()): BinaryWriter {
    if (message.address !== "") {
      writer.uint32(10).string(message.address);
    }
    if (message.pagination !== undefined) {
      PageRequest.encode(message.pagination, writer.uint32(18).fork()).ldelim();
    }
    if (message.orderBy !== OrderBy.ORDER_BY_UNSPECIFIED) {
      writer.uint32(24).int32(message.orderBy);
    }
    if (message.startDate !== "") {
      writer.uint32(34).string(message.startDate);
    }
    if (message.endDate !== "") {
      writer.uint32(42).string(message.endDate);
    }
    return writer;
  },

  decode(input: BinaryReader | Uint8Array, length?: number): GetTxsByAddressRequest {
    const reader = input instanceof BinaryReader ? input : new BinaryReader(input);
    const end = length === undefined ? reader.len : reader.pos + length;
    const message = createBaseRequest();
    while (reader.pos < end) {
      const tag = reader.uint32();
      switch (tag >>> 3) {
        case 1:
          if (tag !== 10) break;
          message.address = reader.string();
          continue;
        case 2:
          if (tag !== 18) break;
          message.pagination = PageRequest.decode(reader, reader.uint32());
          continue;
        case 3:
          if (tag !== 24) break;
          message.orderBy = reader.int32() as OrderBy;
          continue;
        case 4:
          if (tag !== 34) break;
          message.startDate = reader.string();
          continue;
        case 5:
          if (tag !== 42) break;
          message.endDate = reader.string();
          continue;
        default:
          reader.skipType(tag & 7);
          continue;
      }
      reader.skipType(tag & 7);
    }
    return message;
  },

  fromPartial(object: {
    address?: string;
    pagination?: PageRequestMsg;
    orderBy?: OrderBy | number;
    startDate?: string;
    endDate?: string;
  }): GetTxsByAddressRequest {
    return {
      address: object.address ?? "",
      ...(object.pagination !== undefined ? { pagination: object.pagination } : {}),
      orderBy: (object.orderBy as OrderBy | undefined) ?? OrderBy.ORDER_BY_UNSPECIFIED,
      startDate: object.startDate ?? "",
      endDate: object.endDate ?? "",
    };
  },
};

function createBaseResponse(): GetTxsByAddressResponse {
  return { txs: [], txResponses: [], total: 0n };
}

export const GetTxsByAddressResponse = {
  typeUrl: "/sovr.txquery.v1.GetTxsByAddressResponse" as const,

  decode(input: BinaryReader | Uint8Array, length?: number): GetTxsByAddressResponse {
    const reader = input instanceof BinaryReader ? input : new BinaryReader(input);
    const end = length === undefined ? reader.len : reader.pos + length;
    const message = createBaseResponse();
    while (reader.pos < end) {
      const tag = reader.uint32();
      switch (tag >>> 3) {
        case 1:
          if (tag !== 10) break;
          message.txs.push(reader.bytes());
          continue;
        case 2:
          if (tag !== 18) break;
          message.txResponses.push(reader.bytes());
          continue;
        case 3:
          if (tag !== 26) break;
          message.pagination = PageResponse.decode(reader, reader.uint32());
          continue;
        case 4:
          if (tag !== 32) break;
          message.total = reader.uint64();
          continue;
        default:
          reader.skipType(tag & 7);
          continue;
      }
      reader.skipType(tag & 7);
    }
    return message;
  },
};

export const TXQUERY_QUERY_SERVICE = "sovr.txquery.v1.Query";
export const TXQUERY_GET_TXS_BY_ADDRESS = "GetTxsByAddress";
/** grpc-gateway path; query params: pagination.*, order_by, start_date, end_date. */
export const TXQUERY_REST_PATH_PREFIX = "/parler-tech/sovr/txquery/v1/txs/by_address/";
