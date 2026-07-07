/**
 * Reference commitment computation (spec sections 6.1/6.2), built on ethers
 * v6's AbiCoder — an independent implementation of the EVM ABI standard from
 * the Go side's hand-rolled tuple encoder.
 */
import { AbiCoder, keccak256, toUtf8Bytes } from "ethers";
import { encodeCanonical, type Field } from "./canonical.js";

export const SCHEMA_VERSION = 1;
export const DOMAIN_TAG_PREIMAGE = "MST_FABRIC_TX_ANCHOR_v1";
export const DOMAIN_TAG = keccak256(toUtf8Bytes(DOMAIN_TAG_PREIMAGE));

export interface Commitment {
  schemaVersion: number;
  domainTag: string; // 0x-prefixed bytes32
  fabricTxId: string; // 0x-prefixed bytes32
  channelId: string;
  chaincodeId: string;
  blockNumber: bigint;
  timestamp: bigint;
  payloadHash: string; // 0x-prefixed bytes32
}

const TUPLE_TYPES = [
  "uint16",
  "bytes32",
  "bytes32",
  "string",
  "string",
  "uint64",
  "uint64",
  "bytes32",
];

export function abiEncodeCommitment(c: Commitment): string {
  return AbiCoder.defaultAbiCoder().encode(TUPLE_TYPES, [
    c.schemaVersion,
    c.domainTag,
    c.fabricTxId,
    c.channelId,
    c.chaincodeId,
    c.blockNumber,
    c.timestamp,
    c.payloadHash,
  ]);
}

export function commitmentHash(c: Commitment): string {
  return keccak256(abiEncodeCommitment(c));
}

export function payloadHash(fields: Field[]): string {
  return keccak256(encodeCanonical(fields));
}
