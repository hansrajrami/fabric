import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { hexlify, getBytes } from "ethers";
import { encodeCanonical, type Field, type FieldValue } from "../src/canonical.js";
import {
  DOMAIN_TAG,
  DOMAIN_TAG_PREIMAGE,
  SCHEMA_VERSION,
  abiEncodeCommitment,
  commitmentHash,
  payloadHash,
} from "../src/commitment.js";

interface FieldSpec {
  name?: string;
  nameHex?: string;
  type: string;
  value?: unknown;
  valueHex?: string;
}

interface VectorFile {
  schemaVersion: number;
  domainTagPreimage: string;
  domainTag: string;
  agreement: { id: string; payload: FieldSpec[]; encoding: string; payloadHash: string }[];
  commitments: {
    id: string;
    payload: FieldSpec[];
    fabricTxId: string;
    channelId: string;
    chaincodeId: string;
    blockNumber: number;
    timestamp: number;
    payloadHash: string;
    abiEncoding: string;
    commitment: string;
  }[];
  ordering: { id: string; payloads: FieldSpec[][] }[];
  distinct: { id: string; payloads: FieldSpec[][] }[];
  rejection: { id: string; description: string; payload: FieldSpec[] }[];
}

const vectorsUrl = new URL("../../vectors.json", import.meta.url);
const vectors: VectorFile = JSON.parse(readFileSync(fileURLToPath(vectorsUrl), "utf8"));

/**
 * Builds Field objects from vector specs. Anything outside the four-type
 * system (floats, objects, arrays, malformed numbers) throws — that IS the
 * rejection behavior the D-vectors assert.
 */
function buildPayload(specs: FieldSpec[]): Field[] {
  return specs.map((s) => {
    const name: string | Uint8Array =
      s.nameHex !== undefined ? getBytes(s.nameHex) : (s.name as string);
    let spec: FieldValue;
    switch (s.type) {
      case "int":
        spec = { type: "int", value: BigInt(s.value as string) };
        break;
      case "bool":
        if (typeof s.value !== "boolean") throw new Error("bool vector value must be boolean");
        spec = { type: "bool", value: s.value };
        break;
      case "string":
        spec =
          s.valueHex !== undefined
            ? { type: "string", value: getBytes(s.valueHex) }
            : { type: "string", value: s.value as string };
        break;
      case "bytes":
        spec = { type: "bytes", value: getBytes(s.valueHex as string) };
        break;
      default:
        // float / object / array / anything else: no representation exists.
        throw new Error(`unsupported field type: ${s.type}`);
    }
    return { name, spec };
  });
}

describe("constants", () => {
  it("schema version matches the vectors", () => {
    expect(SCHEMA_VERSION).toBe(vectors.schemaVersion);
  });

  it("domain tag preimage and hash match the vectors", () => {
    expect(DOMAIN_TAG_PREIMAGE).toBe(vectors.domainTagPreimage);
    expect(DOMAIN_TAG).toBe(vectors.domainTag);
  });
});

describe("agreement vectors", () => {
  for (const v of vectors.agreement) {
    it(v.id, () => {
      const payload = buildPayload(v.payload);
      expect(hexlify(encodeCanonical(payload))).toBe(v.encoding);
      expect(payloadHash(payload)).toBe(v.payloadHash);
    });
  }
});

describe("commitment vectors", () => {
  for (const v of vectors.commitments) {
    it(v.id, () => {
      const c = {
        schemaVersion: vectors.schemaVersion,
        domainTag: DOMAIN_TAG,
        fabricTxId: v.fabricTxId,
        channelId: v.channelId,
        chaincodeId: v.chaincodeId,
        blockNumber: BigInt(v.blockNumber),
        timestamp: BigInt(v.timestamp),
        payloadHash: payloadHash(buildPayload(v.payload)),
      };
      expect(c.payloadHash).toBe(v.payloadHash);
      expect(abiEncodeCommitment(c)).toBe(v.abiEncoding);
      expect(commitmentHash(c)).toBe(v.commitment);
    });
  }
});

describe("ordering vectors (same hash)", () => {
  for (const v of vectors.ordering) {
    it(v.id, () => {
      const hashes = v.payloads.map((p) => payloadHash(buildPayload(p)));
      for (const h of hashes) expect(h).toBe(hashes[0]);
    });
  }
});

describe("distinctness vectors (all different)", () => {
  for (const v of vectors.distinct) {
    it(v.id, () => {
      const hashes = v.payloads.map((p) => payloadHash(buildPayload(p)));
      expect(new Set(hashes).size).toBe(hashes.length);
    });
  }
});

describe("rejection vectors (must throw, no bytes)", () => {
  for (const v of vectors.rejection) {
    it(`${v.id}: ${v.description}`, () => {
      expect(() => encodeCanonical(buildPayload(v.payload))).toThrow();
    });
  }
});

describe("independent properties", () => {
  it("int canonicalization: BigInts built differently agree; 1 != 256", () => {
    const h1 = payloadHash([{ name: "v", spec: { type: "int", value: 1n } }]);
    const h2 = payloadHash([{ name: "v", spec: { type: "int", value: BigInt("0x01") } }]);
    expect(h1).toBe(h2);
    const h256 = payloadHash([{ name: "v", spec: { type: "int", value: 256n } }]);
    expect(h256).not.toBe(h1);
  });

  it("order independence under many shuffles", () => {
    const fields: Field[] = [
      { name: "x", spec: { type: "int", value: 1n } },
      { name: "y", spec: { type: "int", value: 2n } },
      { name: "flag", spec: { type: "bool", value: true } },
      { name: "s", spec: { type: "string", value: "hello" } },
    ];
    const ref = payloadHash(fields);
    for (let i = 0; i < 200; i++) {
      const shuffled = [...fields].sort(() => Math.random() - 0.5);
      expect(payloadHash(shuffled)).toBe(ref);
    }
  });
});
