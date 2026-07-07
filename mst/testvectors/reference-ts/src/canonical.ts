/**
 * Independent reference implementation of the MST canonical payload encoding
 * (spec section 6.3). Written from the specification text, NOT ported from the
 * Go implementation, so that agreement between the two is meaningful evidence
 * of correctness.
 *
 * Rules implemented:
 *  1. Sort fields by the raw UTF-8 bytes of their names (never locale-aware).
 *  2. Emit uint32 big-endian field count.
 *  3. Per field: u32 len(name) ‖ name ‖ u8 type tag ‖ u32 len(value) ‖ value.
 *  4. Per-type canonical value forms:
 *     int (0x01): exactly 32 big-endian bytes, unsigned.
 *     bool (0x02): one byte 0x00/0x01.
 *     string (0x03): NFC-normalized valid UTF-8.
 *     bytes (0x04): raw.
 *  5. Floats, nesting, duplicate names, invalid/non-NFC UTF-8: throw, never encode.
 */

export const TYPE_INT = 0x01;
export const TYPE_BOOL = 0x02;
export const TYPE_STRING = 0x03;
export const TYPE_BYTES = 0x04;

export const MAX_FIELDS = 4096;
export const MAX_NAME_LEN = 1024;
export const MAX_VALUE_LEN = 1 << 20;

const U256_MAX = (1n << 256n) - 1n;

export type FieldValue =
  | { type: "int"; value: bigint }
  | { type: "bool"; value: boolean }
  | { type: "string"; value: string | Uint8Array }
  | { type: "bytes"; value: Uint8Array };

export interface Field {
  /** Field name; a Uint8Array is treated as raw candidate UTF-8 name bytes. */
  name: string | Uint8Array;
  spec: FieldValue;
}

const utf8Encoder = new TextEncoder();
const utf8StrictDecoder = new TextDecoder("utf-8", { fatal: true });

class CanonicalError extends Error {}

function utf8BytesToValidatedString(bytes: Uint8Array, what: string): string {
  let s: string;
  try {
    s = utf8StrictDecoder.decode(bytes);
  } catch {
    throw new CanonicalError(`${what}: invalid UTF-8`);
  }
  if (s.normalize("NFC") !== s) {
    throw new CanonicalError(`${what}: not NFC-normalized`);
  }
  return s;
}

function encodeName(name: string | Uint8Array): Uint8Array {
  const bytes = typeof name === "string" ? utf8Encoder.encode(name) : name;
  const s = utf8BytesToValidatedString(bytes, "field name");
  if (s.length === 0) throw new CanonicalError("field name: empty");
  if (bytes.length > MAX_NAME_LEN) throw new CanonicalError("field name: too long");
  if (typeof name === "string" && s.normalize("NFC") !== name) {
    throw new CanonicalError("field name: not NFC-normalized");
  }
  return bytes;
}

function encodeValue(spec: FieldValue): { tag: number; bytes: Uint8Array } {
  switch (spec.type) {
    case "int": {
      const v = spec.value;
      if (typeof v !== "bigint") throw new CanonicalError("int value must be a bigint");
      if (v < 0n) throw new CanonicalError("int value must be non-negative");
      if (v > U256_MAX) throw new CanonicalError("int value exceeds 256 bits");
      const out = new Uint8Array(32);
      let rest = v;
      for (let i = 31; i >= 0 && rest > 0n; i--) {
        out[i] = Number(rest & 0xffn);
        rest >>= 8n;
      }
      return { tag: TYPE_INT, bytes: out };
    }
    case "bool":
      return { tag: TYPE_BOOL, bytes: Uint8Array.of(spec.value ? 0x01 : 0x00) };
    case "string": {
      const raw =
        typeof spec.value === "string" ? utf8Encoder.encode(spec.value) : spec.value;
      utf8BytesToValidatedString(raw, "string value");
      if (raw.length > MAX_VALUE_LEN) throw new CanonicalError("string value too long");
      return { tag: TYPE_STRING, bytes: raw };
    }
    case "bytes": {
      if (!(spec.value instanceof Uint8Array)) {
        throw new CanonicalError("bytes value must be a Uint8Array");
      }
      if (spec.value.length > MAX_VALUE_LEN) throw new CanonicalError("bytes value too long");
      return { tag: TYPE_BYTES, bytes: spec.value };
    }
    default:
      // Floats, objects, arrays, and anything else land here.
      throw new CanonicalError(
        `unsupported field type: ${(spec as { type: string }).type}`
      );
  }
}

function compareBytes(a: Uint8Array, b: Uint8Array): number {
  const n = Math.min(a.length, b.length);
  for (let i = 0; i < n; i++) {
    if (a[i] !== b[i]) return a[i] - b[i];
  }
  return a.length - b.length;
}

function u32be(v: number): Uint8Array {
  const out = new Uint8Array(4);
  new DataView(out.buffer).setUint32(0, v, false);
  return out;
}

/** Canonically encodes a flat payload; throws on any disallowed input. */
export function encodeCanonical(fields: Field[]): Uint8Array {
  if (fields.length > MAX_FIELDS) throw new CanonicalError("too many fields");

  const prepared = fields.map((f) => {
    const nameBytes = encodeName(f.name);
    const { tag, bytes } = encodeValue(f.spec);
    return { nameBytes, tag, valueBytes: bytes };
  });

  prepared.sort((x, y) => compareBytes(x.nameBytes, y.nameBytes));
  for (let i = 1; i < prepared.length; i++) {
    if (compareBytes(prepared[i - 1].nameBytes, prepared[i].nameBytes) === 0) {
      throw new CanonicalError("duplicate field name");
    }
  }

  const parts: Uint8Array[] = [u32be(prepared.length)];
  for (const f of prepared) {
    parts.push(u32be(f.nameBytes.length), f.nameBytes, Uint8Array.of(f.tag), u32be(f.valueBytes.length), f.valueBytes);
  }

  const total = parts.reduce((sum, p) => sum + p.length, 0);
  const out = new Uint8Array(total);
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}
