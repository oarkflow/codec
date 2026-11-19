/**
 * A dependency-free JavaScript/TypeScript implementation of the Codec binary
 * wire format. Callers provide a schema because plain messages are positional
 * and carry a fingerprint of the Go schema.
 *
 * The first release focuses on interoperable scalar, struct, slice, array,
 * map, pointer and interface{} values. Go's custom MarshalBinary/MarshalText
 * types are intentionally represented by an explicit schema in JS.
 */

export type ScalarName =
  | "bool" | "int" | "int8" | "int16" | "int32" | "int64"
  | "uint" | "uint8" | "uint16" | "uint32" | "uint64"
  | "float32" | "float64" | "string" | "dynamic";

export interface ScalarType { readonly kind: "scalar"; readonly name: ScalarName; }
export interface StructType { readonly kind: "struct"; readonly fields: Record<string, CodecType>; }
export interface SliceType { readonly kind: "slice"; readonly element: CodecType; }
export interface ArrayType { readonly kind: "array"; readonly length: number; readonly element: CodecType; }
export interface MapType { readonly kind: "map"; readonly key: CodecType; readonly value: CodecType; }
export interface PointerType { readonly kind: "pointer"; readonly element: CodecType; }
export type CodecType = ScalarType | StructType | SliceType | ArrayType | MapType | PointerType;

const scalar = (name: ScalarName): ScalarType => Object.freeze({ kind: "scalar", name });

/** Schema constructors. Field insertion order is the Go struct declaration order. */
export const types = {
  bool: scalar("bool"), int: scalar("int"), int8: scalar("int8"), int16: scalar("int16"),
  int32: scalar("int32"), int64: scalar("int64"), uint: scalar("uint"), uint8: scalar("uint8"),
  uint16: scalar("uint16"), uint32: scalar("uint32"), uint64: scalar("uint64"),
  float32: scalar("float32"), float64: scalar("float64"), string: scalar("string"),
  dynamic: scalar("dynamic"),
  struct(fields: Record<string, CodecType>): StructType { return { kind: "struct", fields }; },
  slice(element: CodecType): SliceType { return { kind: "slice", element }; },
  array(length: number, element: CodecType): ArrayType {
    if (!Number.isSafeInteger(length) || length < 0) throw new RangeError("array length must be a non-negative integer");
    return { kind: "array", length, element };
  },
  map(key: CodecType, value: CodecType): MapType { return { kind: "map", key, value }; },
  pointer(element: CodecType): PointerType { return { kind: "pointer", element }; },
};

export class CodecError extends Error {
  constructor(message: string) { super(`codec: ${message}`); this.name = "CodecError"; }
}

/** Preserve Go's signed-versus-unsigned dynamic integer tag across decode/encode. */
export class GoInt64 {
  constructor(readonly value: bigint) {}
}
export class GoUint64 {
  constructor(readonly value: bigint) {}
}

const U64 = (1n << 64n) - 1n;
const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true });
export const MAX_ELEMENTS = 1 << 24;
export const MAX_MESSAGE_SIZE = 1 << 28;

function scalarInfo(t: ScalarType): { desc: string; bytes: number; cls: "bool" | "int" | "uint" | "float" | "string" | "dynamic"; bits: number } {
  switch (t.name) {
    case "bool": return { desc: "1.1", bytes: 1, cls: "bool", bits: 1 };
    case "int": case "int64": return { desc: "2.8", bytes: 8, cls: "int", bits: 64 };
    case "int8": return { desc: "2.1", bytes: 1, cls: "int", bits: 8 };
    case "int16": return { desc: "2.2", bytes: 2, cls: "int", bits: 16 };
    case "int32": return { desc: "2.4", bytes: 4, cls: "int", bits: 32 };
    case "uint": case "uint64": return { desc: "3.8", bytes: 8, cls: "uint", bits: 64 };
    case "uint8": return { desc: "3.1", bytes: 1, cls: "uint", bits: 8 };
    case "uint16": return { desc: "3.2", bytes: 2, cls: "uint", bits: 16 };
    case "uint32": return { desc: "3.4", bytes: 4, cls: "uint", bits: 32 };
    case "float32": return { desc: "4.4", bytes: 4, cls: "float", bits: 32 };
    case "float64": return { desc: "5.8", bytes: 8, cls: "float", bits: 64 };
    case "string": return { desc: "6.16", bytes: 16, cls: "string", bits: 0 };
    case "dynamic": return { desc: "12.16", bytes: 16, cls: "dynamic", bits: 0 };
  }
}

interface SchemaInfo {
  description: string;
  fingerprint: number;
  structIDs: Map<StructType, number>;
  counts: number[];
}

function schemaInfo(root: CodecType, limits?: number[]): SchemaInfo {
  const structIDs = new Map<StructType, number>();
  const counts: number[] = [];
  const describe = (t: CodecType): string => {
    switch (t.kind) {
      case "scalar": return scalarInfo(t).desc;
      case "slice": return `[]${describe(t.element)}`;
      case "array": return `[${t.length}]${describe(t.element)}`;
      case "map": return `map[${describe(t.key)}]${describe(t.value)}`;
      case "pointer": return `*${describe(t.element)}`;
      case "struct": {
        const known = structIDs.get(t);
        if (known !== undefined) return `@${known}`;
        const id = structIDs.size;
        structIDs.set(t, id);
        const all = Object.entries(t.fields);
        const n = limits ? limits[id] : all.length;
        if (n === undefined || n > all.length) throw new CodecError("schema field count is newer than the local type");
        counts[id] = n;
        return `{${all.slice(0, n).map(([name, field]) => `${name}:${describe(field)};`).join("")}}`;
      }
    }
  };
  let description = describe(root);
  if (root.kind !== "struct") description += "W";
  if (limits && counts.length !== limits.length) throw new CodecError("evolvable schema has a different struct shape");
  let h = 14695981039346656037n;
  for (const b of encoder.encode(description)) h = ((h ^ BigInt(b)) * 1099511628211n) & U64;
  const fingerprint = Number((h ^ (h >> 32n)) & 0xffffffffn) >>> 0;
  return { description, fingerprint, structIDs, counts };
}

function uvarint(value: bigint): number[] {
  if (value < 0n || value > U64) throw new CodecError("integer is outside the uint64 wire range");
  const out: number[] = [];
  while (value >= 0x80n) { out.push(Number(value & 0x7fn) | 0x80); value >>= 7n; }
  out.push(Number(value));
  return out;
}

function readUvarint(data: Uint8Array, pos: number): [bigint, number] {
  let value = 0n;
  for (let i = 0; i < 10; i++) {
    if (pos + i >= data.length) throw new CodecError("corrupt or truncated varint");
    const b = data[pos + i];
    if (i === 9 && b > 1) throw new CodecError("varint overflows uint64");
    value |= BigInt(b & 0x7f) << BigInt(i * 7);
    if (b < 0x80) return [value, pos + i + 1];
  }
  throw new CodecError("varint is too long");
}

function zigzag(v: bigint): bigint { return ((v << 1n) ^ (v >> 63n)) & U64; }
function unzigzag(v: bigint): bigint { return (v >> 1n) ^ -(v & 1n); }
function wrap64(v: bigint): bigint { return BigInt.asIntN(64, v); }
function concat(...parts: number[][]): number[] { return parts.flat(); }
function appendBytes(dst: number[], src: ArrayLike<number>): void {
  for (let i = 0; i < src.length; i++) dst.push(src[i]);
}
function widthCode(w: number): number { return w === 1 ? 0 : w === 2 ? 1 : 2; }
function widthFor(n: number): number { return n <= 0x100 ? 1 : n <= 0x10000 ? 2 : 4; }

function writeOffset(value: number, width: number): number[] {
  const out = new Array<number>(width);
  let v = BigInt(value);
  if (v < 0n || v >= (1n << BigInt(width * 8))) throw new CodecError("message offset exceeds selected width");
  for (let i = 0; i < width; i++) { out[i] = Number(v & 0xffn); v >>= 8n; }
  return out;
}

function readOffset(data: Uint8Array, pos: number, width: number): number {
  if (pos < 0 || pos + width > data.length) throw new CodecError("corrupt offset table");
  let v = 0n;
  for (let i = width - 1; i >= 0; i--) v = (v << 8n) | BigInt(data[pos + i]);
  if (v > BigInt(Number.MAX_SAFE_INTEGER)) throw new CodecError("offset is too large for JavaScript");
  return Number(v);
}

function intValue(t: ScalarType, value: unknown): bigint {
  const info = scalarInfo(t);
  let n: bigint;
  if (typeof value === "bigint") n = value;
  else if (typeof value === "number" && Number.isSafeInteger(value)) n = BigInt(value);
  else throw new CodecError(`${t.name} requires an integer number or bigint`);
  const bits = BigInt(info.bits);
  if (info.cls === "int") {
    const min = -(1n << (bits - 1n)), max = (1n << (bits - 1n)) - 1n;
    if (n < min || n > max) throw new CodecError(`${t.name} is out of range`);
  } else if (n < 0n || n > (1n << bits) - 1n) throw new CodecError(`${t.name} is out of range`);
  return n;
}

function floatBytes(value: unknown, bits: number): number[] {
  if (typeof value !== "number") throw new CodecError("float requires a number");
  const b = new ArrayBuffer(bits / 8), d = new DataView(b);
  if (bits === 32) d.setFloat32(0, value, true); else d.setFloat64(0, value, true);
  return Array.from(new Uint8Array(b));
}

function encodeScalarPayload(t: ScalarType, value: unknown): number[] {
  const info = scalarInfo(t);
  switch (info.cls) {
    case "bool": return [value ? 1 : 0];
    case "int": return uvarint(zigzag(intValue(t, value)));
    case "uint": return uvarint(intValue(t, value));
    case "float": return floatBytes(value, info.bits);
    case "string":
      if (typeof value !== "string") throw new CodecError("string schema requires a string");
      return Array.from(encoder.encode(value));
    case "dynamic": return encodeDynamic(value, 0);
  }
}

function encodeFrame(fields: [string, CodecType][], value: unknown, width: number, depth = 0): number[] {
  if (depth > 4096) throw new CodecError("nesting too deep");
  const object = value as Record<string, unknown>;
  const bitmap = new Array<number>(Math.max(1, Math.ceil(fields.length / 8))).fill(0);
  const scalars: number[] = [];
  const vars: { index: number; bytes: number[] }[] = [];
  for (let i = 0; i < fields.length; i++) {
    const [name, t] = fields[i];
    const raw = object?.[name];
    const v = raw === undefined ? defaultValue(t) : raw;
    const present = isPresent(t, v);
    if (!present) continue;
    bitmap[i >> 3] |= 1 << (i & 7);
    if (t.kind === "scalar") {
      const info = scalarInfo(t);
      if (info.cls === "bool") continue;
      if (info.cls === "int" || info.cls === "uint" || info.cls === "float") scalars.push(...encodeScalarPayload(t, v));
      else vars.push({ index: i, bytes: encodeScalarPayload(t, v) });
    } else vars.push({ index: i, bytes: encodeValuePayload(t, v, width, depth + 1) });
  }
  const body = [...bitmap, ...scalars];
  const tableStart = body.length;
  const payloadStart = tableStart + vars.length * width;
  let offset = payloadStart;
  for (const item of vars) {
    body.push(...writeOffset(offset, width));
    offset += item.bytes.length;
  }
  for (const item of vars) appendBytes(body, item.bytes);
  return body;
}

function isPresent(t: CodecType, v: unknown): boolean {
  if (t.kind === "pointer") return v !== null && v !== undefined;
  if (t.kind === "struct") return true;
  if (t.kind === "array") return t.length > 0;
  if (t.kind === "slice") return Array.isArray(v) && v.length > 0;
  if (t.kind === "map") return v === null || v === undefined ? false : mapEntries(v).length > 0;
  const info = scalarInfo(t);
  switch (info.cls) {
    case "bool": return v === true;
    case "int": case "uint": return v !== null && v !== undefined && intValue(t, v) !== 0n;
    case "float": return v !== null && v !== undefined && floatBytes(v, info.bits).some((b) => b !== 0);
    case "string": return typeof v === "string" && v.length > 0;
    case "dynamic": return v !== null && v !== undefined;
  }
}

function encodeValuePayload(t: CodecType, value: unknown, width: number, depth = 0): number[] {
  if (depth > 4096) throw new CodecError("nesting too deep");
  switch (t.kind) {
    case "scalar": return encodeScalarPayload(t, value);
    case "struct": return encodeFrame(Object.entries(t.fields), value, width, depth + 1);
    case "pointer": {
      if (value === null || value === undefined) return [];
      if (t.element.kind === "struct") return encodeValuePayload(t.element, value, width, depth + 1);
      return concat([1], encodeValuePayload(t.element, value, width, depth + 1));
    }
    case "slice": return (value as unknown[]).length === 0 ? [] : encodeElements(t.element, value as unknown[], width, depth + 1);
    case "array": {
      const vals = value as unknown[];
      if (!Array.isArray(vals) || vals.length !== t.length) throw new CodecError(`array requires exactly ${t.length} elements`);
      return encodeElements(t.element, vals, width, depth + 1);
    }
    case "map": {
      const entries = mapEntries(value).sort((a, b) => compareKeys(t.key, a[0], b[0]));
      if (entries.length === 0) return [];
      const keys = encodeElements(t.key, entries.map((e) => e[0]), width, depth + 1);
      return concat(uvarint(BigInt(keys.length)), keys, encodeElements(t.value, entries.map((e) => e[1]), width, depth + 1));
    }
  }
}

function encodeElements(t: CodecType, values: unknown[], width: number, depth = 0): number[] {
  if (depth > 4096) throw new CodecError("nesting too deep");
  const count = values.length;
  if (count === 0) return [];
  if (t.kind === "scalar") {
    const info = scalarInfo(t);
    if (info.cls === "bool") return values.map((v) => v ? 1 : 0);
    if (t.name === "uint8") return values.map((v) => Number(intValue(t, v)));
    if (info.cls === "float") return values.flatMap((v) => floatBytes(v, info.bits));
    if (info.cls === "int" || info.cls === "uint") {
      const out = uvarint(BigInt(count));
      let previous = 0n;
      for (const item of values) {
        const current = wrap64(intValue(t, item));
        out.push(...uvarint(zigzag(wrap64(current - previous))));
        previous = current;
      }
      return out;
    }
  }
  const out = uvarint(BigInt(count) << 1n);
  const tableBytes = count * width;
  const payloads = values.map((v) => encodeValuePayload(t, v === undefined ? defaultValue(t) : v, width, depth + 1));
  let offset = out.length + tableBytes;
  for (const p of payloads) { out.push(...writeOffset(offset, width)); offset += p.length; }
  for (const p of payloads) appendBytes(out, p);
  return out;
}

function mapEntries(v: unknown): [unknown, unknown][] {
  if (v instanceof Map) return Array.from(v.entries());
  if (v && typeof v === "object" && !Array.isArray(v)) return Object.entries(v as Record<string, unknown>);
  throw new CodecError("map schema requires a Map or object");
}

function compareKeys(t: CodecType, a: unknown, b: unknown): number {
  if (t.kind === "scalar" && scalarInfo(t).cls === "string") {
    const aa = encoder.encode(String(a)), bb = encoder.encode(String(b));
    for (let i = 0; i < Math.min(aa.length, bb.length); i++) if (aa[i] !== bb[i]) return aa[i] - bb[i];
    return aa.length - bb.length;
  }
  if (t.kind === "scalar" && scalarInfo(t).cls === "bool") return Number(Boolean(a)) - Number(Boolean(b));
  if (t.kind === "scalar" && scalarInfo(t).cls === "float") {
    const aa = Number(a), bb = Number(b);
    return aa < bb ? -1 : aa > bb ? 1 : 0;
  }
  if (t.kind !== "scalar" || (scalarInfo(t).cls !== "int" && scalarInfo(t).cls !== "uint")) return 0;
  const aa = typeof a === "bigint" ? a : BigInt(a as number);
  const bb = typeof b === "bigint" ? b : BigInt(b as number);
  return aa < bb ? -1 : aa > bb ? 1 : 0;
}

function encodeDynamic(v: unknown, depth: number): number[] {
  if (depth > 4096) throw new CodecError("nesting too deep");
  if (v === null || v === undefined) return [0];
  if (v === false) return [1];
  if (v === true) return [2];
  if (v instanceof GoInt64) return concat([3], uvarint(zigzag(v.value)));
  if (v instanceof GoUint64) return concat([4], uvarint(v.value));
  if (typeof v === "bigint") return v < 0n ? concat([3], uvarint(zigzag(v))) : concat([4], uvarint(v));
  if (typeof v === "number") {
    if (Number.isSafeInteger(v)) return concat([3], uvarint(zigzag(BigInt(v))));
    return concat([5], floatBytes(v, 64));
  }
  if (typeof v === "string") { const b = Array.from(encoder.encode(v)); return concat([6], uvarint(BigInt(b.length)), b); }
  if (v instanceof Uint8Array) return concat([7], uvarint(BigInt(v.length)), Array.from(v));
  if (Array.isArray(v)) return concat([8], uvarint(BigInt(v.length)), ...v.map((x) => encodeDynamic(x, depth + 1)));
  if (v instanceof Map) {
    const entries = Array.from(v.entries());
    const stringKeys = entries.every(([k]) => typeof k === "string");
    if (stringKeys) entries.sort((a, b) => compareUtf8(a[0] as string, b[0] as string));
    const body: number[] = [];
    for (const [k, x] of entries) {
      if (stringKeys) { const kb = Array.from(encoder.encode(k as string)); body.push(...uvarint(BigInt(kb.length)), ...kb, ...encodeDynamic(x, depth + 1)); }
      else body.push(...encodeDynamic(k, depth + 1), ...encodeDynamic(x, depth + 1));
    }
    return concat([stringKeys ? 9 : 10], uvarint(BigInt(entries.length)), body);
  }
  if (typeof v === "object") {
    const entries = Object.entries(v as Record<string, unknown>).sort((a, b) => compareUtf8(a[0], b[0]));
    const body: number[] = [];
    for (const [k, x] of entries) { const kb = Array.from(encoder.encode(k)); body.push(...uvarint(BigInt(kb.length)), ...kb, ...encodeDynamic(x, depth + 1)); }
    return concat([9], uvarint(BigInt(entries.length)), body);
  }
  throw new CodecError(`unsupported dynamic value ${typeof v}`);
}

function compareUtf8(a: string, b: string): number {
  return compareBytes(encoder.encode(a), encoder.encode(b));
}
function compareBytes(a: Uint8Array, b: Uint8Array): number {
  for (let i = 0; i < Math.min(a.length, b.length); i++) if (a[i] !== b[i]) return a[i] - b[i];
  return a.length - b.length;
}

function encodeMessage(schema: CodecType, value: unknown, evolvable: boolean): Uint8Array {
  const info = schemaInfo(schema);
  const fields: [string, CodecType][] = schema.kind === "struct" ? Object.entries(schema.fields) : [["V", schema]];
  let width = 1;
  let body: number[] = [];
  for (;;) {
    try {
      body = encodeFrame(fields, schema.kind === "struct" ? value : { V: value }, width);
    } catch (err) {
      if (!(err instanceof CodecError) || err.message !== "codec: message offset exceeds selected width" || width === 4) throw err;
      width = width === 1 ? 2 : 4;
      continue;
    }
    const needed = widthFor(body.length);
    if (needed <= width) break;
    width = needed;
  }
  const marker = 0xc0 | (evolvable ? 0x04 : 0) | widthCode(width);
  const fp = info.fingerprint;
  const header = [marker, fp & 0xff, (fp >>> 8) & 0xff, (fp >>> 16) & 0xff, (fp >>> 24) & 0xff];
  if (evolvable) header.push(...uvarint(BigInt(info.counts.length)), ...info.counts.flatMap((c) => uvarint(BigInt(c))));
  return Uint8Array.from([...header, ...body]);
}

/** Encode one value, optionally using the append-only evolvable header. */
export function encode(schema: CodecType, value: unknown, options: { evolvable?: boolean } = {}): Uint8Array {
  return encodeMessage(schema, value, options.evolvable ?? false);
}

/** Encode several values as Codec's uvarint-length-prefixed stream protocol. */
export function encodeStream(schema: CodecType, values: Iterable<unknown>, options: { evolvable?: boolean } = {}): Uint8Array {
  const out: number[] = [];
  for (const value of values) {
    const message = encode(schema, value, options);
    appendBytes(out, uvarint(BigInt(message.length)));
    appendBytes(out, message);
  }
  return Uint8Array.from(out);
}

interface DecodeContext {
  width: number;
  info: SchemaInfo;
  limits?: number[];
  depth: number;
}

function defaultValue(t: CodecType): unknown {
  switch (t.kind) {
    case "pointer": return null;
    case "struct": return Object.fromEntries(Object.entries(t.fields).map(([k, v]) => [k, defaultValue(v)]));
    case "slice": return [];
    case "array": return Array.from({ length: t.length }, () => defaultValue(t.element));
    case "map": return t.key.kind === "scalar" && scalarInfo(t.key).cls === "string" ? {} : new Map();
    case "scalar": {
      const cls = scalarInfo(t).cls;
      if (cls === "dynamic") return null;
      if (cls === "bool") return false;
      if (cls === "int" || cls === "uint") return scalarInfo(t).bits === 64 ? 0n : 0;
      if (cls === "float") return 0;
      return "";
    }
  }
}

function readFloat(data: Uint8Array, pos: number, bits: number): number {
  const bytes = bits / 8;
  if (pos < 0 || pos + bytes > data.length) throw new CodecError("corrupt floating-point value");
  const view = new DataView(data.buffer, data.byteOffset + pos, bytes);
  return bits === 32 ? view.getFloat32(0, true) : view.getFloat64(0, true);
}

function integerValue(t: ScalarType, value: bigint): number | bigint {
  const info = scalarInfo(t);
  const n = info.cls === "int" ? BigInt.asIntN(info.bits, value) : BigInt.asUintN(info.bits, value);
  return info.bits === 64 ? n : Number(n);
}

function decodeFrame(t: StructType, data: Uint8Array, ctx: DecodeContext): Record<string, unknown> {
  if (++ctx.depth > 4096) throw new CodecError("nesting too deep");
  try {
    const id = ctx.info.structIDs.get(t);
    if (id === undefined) throw new CodecError("invalid local schema");
    const nFields = ctx.limits ? ctx.limits[id] : Object.keys(t.fields).length;
    if (nFields === undefined) throw new CodecError("invalid evolvable shape");
    const fields = Object.entries(t.fields);
    const bitmapBytes = Math.max(1, Math.ceil(nFields / 8));
    if (data.length < bitmapBytes) throw new CodecError("truncated struct frame");
    const out = Object.fromEntries(fields.map(([name, field]) => [name, defaultValue(field)]));
    let pos = bitmapBytes;
    const vars: { index: number; type: CodecType }[] = [];
    for (let i = 0; i < nFields; i++) {
      const [name, field] = fields[i];
      const present = (data[i >> 3] & (1 << (i & 7))) !== 0;
      if (!present) continue;
      if (field.kind !== "scalar") { vars.push({ index: i, type: field }); continue; }
      const si = scalarInfo(field);
      switch (si.cls) {
        case "bool": out[name] = true; break;
        case "int": case "uint": {
          const [v, next] = readUvarint(data, pos); pos = next;
          out[name] = integerValue(field, si.cls === "int" ? unzigzag(v) : v); break;
        }
        case "float": out[name] = readFloat(data, pos, si.bits); pos += si.bytes; break;
        case "string": case "dynamic": vars.push({ index: i, type: field }); break;
      }
    }
    const tableEnd = pos + vars.length * ctx.width;
    if (tableEnd > data.length) throw new CodecError("truncated struct offset table");
    for (let j = 0; j < vars.length; j++) {
      const start = readOffset(data, pos + j * ctx.width, ctx.width);
      const end = j + 1 < vars.length ? readOffset(data, pos + (j + 1) * ctx.width, ctx.width) : data.length;
      if (start < tableEnd || start > end || end > data.length) throw new CodecError("invalid struct offset");
      const [name] = fields[vars[j].index];
      out[name] = decodeValuePayload(vars[j].type, data.subarray(start, end), ctx);
    }
    return out;
  } finally { ctx.depth--; }
}

function decodeValuePayload(t: CodecType, data: Uint8Array, ctx: DecodeContext): unknown {
  if (++ctx.depth > 4096) throw new CodecError("nesting too deep");
  try {
    switch (t.kind) {
      case "scalar": {
        const info = scalarInfo(t);
        switch (info.cls) {
          case "bool": if (data.length !== 1 || data[0] > 1) throw new CodecError("invalid bool payload"); return data[0] === 1;
          case "int": case "uint": {
            const [v, end] = readUvarint(data, 0);
            if (end !== data.length) throw new CodecError("trailing integer bytes");
            return integerValue(t, info.cls === "int" ? unzigzag(v) : v);
          }
          case "float": if (data.length !== info.bytes) throw new CodecError("invalid float payload"); return readFloat(data, 0, info.bits);
          case "string": return decodeString(data);
          case "dynamic": { const [v, end] = decodeDynamic(data, 0, 0); if (end !== data.length) throw new CodecError("trailing dynamic bytes"); return v; }
        }
      }
      case "struct": return decodeFrame(t, data, ctx);
      case "pointer": {
        if (data.length === 0) return null;
        if (t.element.kind === "struct") return decodeValuePayload(t.element, data, ctx);
        if (data[0] !== 1) throw new CodecError("invalid non-nil pointer marker");
        return decodeValuePayload(t.element, data.subarray(1), ctx);
      }
      case "slice": return data.length === 0 ? [] : decodeElements(t.element, data, ctx);
      case "array": {
        const values = decodeElements(t.element, data, ctx);
        if (values.length !== t.length) throw new CodecError("array length does not match schema");
        return values;
      }
      case "map": return decodeMap(t, data, ctx);
    }
  } finally { ctx.depth--; }
}

function decodeString(data: Uint8Array): string {
  try { return decoder.decode(data); }
  catch { throw new CodecError("string contains invalid UTF-8; JavaScript strings must be valid UTF-8"); }
}

function decodeElements(t: CodecType, data: Uint8Array, ctx: DecodeContext): unknown[] {
  if (t.kind === "scalar") {
    const info = scalarInfo(t);
    if (info.cls === "bool") {
      if (data.some((v) => v > 1)) throw new CodecError("invalid bool slice");
      return Array.from(data, (v) => v === 1);
    }
    if (t.name === "uint8") return Array.from(data);
    if (info.cls === "float") {
      if (data.length % info.bytes !== 0) throw new CodecError("invalid float slice length");
      const values: number[] = [];
      for (let pos = 0; pos < data.length; pos += info.bytes) values.push(readFloat(data, pos, info.bits));
      return values;
    }
    if (info.cls === "int" || info.cls === "uint") {
      const [countBig, start] = readUvarint(data, 0);
      if (countBig > BigInt(data.length - start)) throw new CodecError("integer slice count exceeds payload");
      const count = Number(countBig), values: unknown[] = [];
      let pos = start, prev = 0n;
      for (let i = 0; i < count; i++) {
        const [delta, next] = readUvarint(data, pos); pos = next;
        prev = wrap64(prev + unzigzag(delta));
        values.push(integerValue(t, prev));
      }
      if (pos !== data.length) throw new CodecError("trailing integer slice bytes");
      return values;
    }
  }
  const [header, pos] = readUvarint(data, 0);
  const countBig = header >> 1n;
  if (countBig > BigInt(Number.MAX_SAFE_INTEGER)) throw new CodecError("slice is too large");
  const count = Number(countBig);
  if (header & 1n) {
    if (t.kind !== "struct" || count > MAX_ELEMENTS) throw new CodecError("invalid or oversized columnar slice");
    return decodeColumns(t, count, data, pos, ctx);
  }
  if (count > Math.floor((data.length - pos) / ctx.width)) throw new CodecError("slice count exceeds offset table");
  const tableEnd = pos + count * ctx.width;
  const values: unknown[] = [];
  for (let i = 0; i < count; i++) {
    const start = readOffset(data, pos + i * ctx.width, ctx.width);
    const end = i + 1 < count ? readOffset(data, pos + (i + 1) * ctx.width, ctx.width) : data.length;
    if (start < tableEnd || start > end || end > data.length) throw new CodecError("invalid slice offset");
    values.push(decodeValuePayload(t, data.subarray(start, end), ctx));
  }
  return values;
}

function decodeMap(t: MapType, data: Uint8Array, ctx: DecodeContext): unknown {
  if (data.length === 0) return defaultValue(t);
  const [keyLen, pos] = readUvarint(data, 0);
  if (keyLen > BigInt(data.length - pos)) throw new CodecError("map key payload exceeds message");
  const keyEnd = pos + Number(keyLen);
  const keyData = data.subarray(pos, keyEnd);
  const keys = decodeElements(t.key, keyData, ctx);
  const values = decodeElements(t.value, data.subarray(keyEnd), ctx);
  if (keys.length !== values.length) throw new CodecError("map key/value count mismatch");
  const count = keys.length;
  if (t.key.kind === "scalar" && scalarInfo(t.key).cls === "string") {
    const out: Record<string, unknown> = Object.create(null);
    for (let i = 0; i < count; i++) out[String(keys[i])] = values[i];
    return out;
  }
  return new Map(keys.map((key, i) => [key, values[i]]));
}

function unpack(data: Uint8Array, bitPos: number, width: number): bigint {
  if (width === 0) return 0n;
  let out = 0n;
  for (let i = 0; i < width; i++) {
    const p = bitPos + i;
    if ((data[p >> 3] & (1 << (p & 7))) !== 0) out |= 1n << BigInt(i);
  }
  return out;
}

function decodeColumns(t: StructType, count: number, data: Uint8Array, tableStart: number, ctx: DecodeContext): unknown[] {
  const fields = Object.entries(t.fields);
  const tableEnd = tableStart + fields.length * ctx.width;
  if (fields.length === 0 || tableEnd > data.length) throw new CodecError("invalid column directory");
  const spans: [number, number][] = [];
  for (let f = 0; f < fields.length; f++) {
    const start = readOffset(data, tableStart + f * ctx.width, ctx.width);
    const end = f + 1 < fields.length ? readOffset(data, tableStart + (f + 1) * ctx.width, ctx.width) : data.length;
    if (start < tableEnd || start > end || end > data.length) throw new CodecError("invalid column offset");
    spans.push([start, end]);
  }
  const rows = Array.from({ length: count }, () => defaultValue(t) as Record<string, unknown>);
  let hasData = false;
  for (let f = 0; f < fields.length; f++) {
    const [start, end] = spans[f];
    if (start === end) continue;
    hasData = true;
    const values = decodeLane(fields[f][1], count, data.subarray(start, end), ctx);
    for (let i = 0; i < count; i++) rows[i][fields[f][0]] = values[i];
  }
  if (!hasData) throw new CodecError("empty columnar struct slice");
  return rows;
}

function decodeLane(t: CodecType, count: number, lane: Uint8Array, ctx: DecodeContext): unknown[] {
  if (lane.length === 0) return Array.from({ length: count }, () => defaultValue(t));
  const tag = lane[0], b = lane.subarray(1);
  if (t.kind === "scalar") {
    const info = scalarInfo(t);
    if (info.cls === "bool") {
      if (tag !== 0 || b.length !== Math.ceil(count / 8)) throw new CodecError("invalid bool lane");
      return Array.from({ length: count }, (_, i) => (b[i >> 3] & (1 << (i & 7))) !== 0);
    }
    if (info.cls === "string") {
      if (tag === 0) {
        let pos = 0; const out: string[] = [];
        for (let i = 0; i < count; i++) {
          const [n, next] = readUvarint(b, pos); pos = next;
          if (n > BigInt(b.length - pos)) throw new CodecError("truncated string lane");
          out.push(decodeString(b.subarray(pos, pos + Number(n)))); pos += Number(n);
        }
        if (pos !== b.length) throw new CodecError("trailing string lane bytes");
        return out;
      }
      if (tag === 3) {
        const [n, p0] = readUvarint(b, 0); let pos = p0; const dict: string[] = [];
        if (n === 0n || n > BigInt(b.length)) throw new CodecError("invalid string dictionary");
        for (let i = 0; i < Number(n); i++) {
          const [len, next] = readUvarint(b, pos); pos = next;
          if (len > BigInt(b.length - pos)) throw new CodecError("truncated string dictionary");
          dict.push(decodeString(b.subarray(pos, pos + Number(len)))); pos += Number(len);
        }
        if (pos >= b.length) throw new CodecError("missing string dictionary width");
        const width = b[pos++], packed = b.subarray(pos);
        if (width > 32 || packed.length !== Math.ceil(count * width / 8)) throw new CodecError("invalid string dictionary indexes");
        return Array.from({ length: count }, (_, i) => {
          const ix = Number(unpack(packed, i * width, width));
          if (ix >= dict.length) throw new CodecError("string dictionary index out of range");
          return dict[ix];
        });
      }
      throw new CodecError("unsupported string lane tag");
    }
    if (info.cls === "int" || info.cls === "uint") return decodeIntegerLane(t, count, tag, b);
    if (info.cls === "float") {
      if (tag === 0) {
        if (b.length !== count * info.bytes) throw new CodecError("invalid float lane length");
        return Array.from({ length: count }, (_, i) => readFloat(b, i * info.bytes, info.bits));
      }
      if (tag === 3) return decodeDictionaryLane(t, count, b);
    }
  }
  if (tag !== 0 || b.length < 1 + count * ctx.width) throw new CodecError("invalid row lane");
  const tableEnd = 1 + count * ctx.width, values: unknown[] = [];
  for (let i = 0; i < count; i++) {
    const start = readOffset(lane, 1 + i * ctx.width, ctx.width);
    const end = i + 1 < count ? readOffset(lane, 1 + (i + 1) * ctx.width, ctx.width) : lane.length;
    if (start < tableEnd || start > end || end > lane.length) throw new CodecError("invalid row lane offset");
    values.push(decodeValuePayload(t, lane.subarray(start, end), ctx));
  }
  return values;
}

function decodeIntegerLane(t: ScalarType, count: number, tag: number, b: Uint8Array): unknown[] {
  const info = scalarInfo(t), signed = info.cls === "int";
  if (tag === 1) {
    const [rawMin, p0] = readUvarint(b, 0);
    let p = p0;
    if (p >= b.length) throw new CodecError("truncated frame-of-reference lane");
    const min = signed ? unzigzag(rawMin) : rawMin, width = b[p++], packed = b.subarray(p);
    if (width > 64 || packed.length !== Math.ceil(count * width / 8)) throw new CodecError("invalid packed lane");
    return Array.from({ length: count }, (_, i) => integerValue(t, wrap64(min + unpack(packed, i * width, width))));
  }
  if (tag === 2) {
    const out: unknown[] = []; let pos = 0, prev = 0n;
    for (let i = 0; i < count; i++) { const [v, next] = readUvarint(b, pos); pos = next; prev = wrap64(prev + unzigzag(v)); out.push(integerValue(t, prev)); }
    if (pos !== b.length) throw new CodecError("trailing delta lane bytes");
    return out;
  }
  if (tag === 4) {
    const [firstRaw, p1] = readUvarint(b, 0), [minRaw, p0] = readUvarint(b, p1);
    let p2 = p0;
    if (p2 >= b.length) throw new CodecError("truncated delta frame-of-reference lane");
    let current = wrap64(unzigzag(firstRaw));
    const dmin = unzigzag(minRaw), width = b[p2++], nDeltas = Math.max(0, count - 1);
    const packedLen = Math.ceil(nDeltas * width / 8);
    if (width > 64 || p2 + packedLen > b.length) throw new CodecError("invalid delta packed lane");
    const packed = b.subarray(p2, p2 + packedLen); p2 += packedLen;
    const checkpoints = Math.floor(Math.max(0, nDeltas - 1) / 64);
    if (width > 0 && checkpoints > 0) {
      if (p2 >= b.length) throw new CodecError("missing delta checkpoints");
      const cw = b[p2++], clen = Math.ceil(checkpoints * cw / 8);
      if (cw > 64 || p2 + clen !== b.length) throw new CodecError("invalid delta checkpoints");
    } else if (p2 !== b.length) throw new CodecError("trailing delta lane bytes");
    const out: unknown[] = [integerValue(t, current)];
    for (let i = 0; i < nDeltas; i++) { current = wrap64(current + dmin + unpack(packed, i * width, width)); out.push(integerValue(t, current)); }
    return out;
  }
  if (tag === 3) return decodeDictionaryLane(t, count, b);
  throw new CodecError("unsupported integer lane tag");
}

function decodeDictionaryLane(t: ScalarType, count: number, b: Uint8Array): unknown[] {
  const info = scalarInfo(t), [n, p0] = readUvarint(b, 0);
  if (n === 0n || n > BigInt(b.length)) throw new CodecError("invalid dictionary size");
  let pos = p0; const dict: unknown[] = [];
  for (let i = 0; i < Number(n); i++) {
    if (info.cls === "int" || info.cls === "uint") {
      const [raw, next] = readUvarint(b, pos); pos = next;
      dict.push(integerValue(t, info.cls === "int" ? unzigzag(raw) : raw));
    } else if (info.cls === "float") {
      if (pos + info.bytes > b.length) throw new CodecError("truncated float dictionary");
      dict.push(readFloat(b, pos, info.bits)); pos += info.bytes;
    } else throw new CodecError("invalid dictionary value type");
  }
  if (pos >= b.length) throw new CodecError("missing dictionary width");
  const width = b[pos++], packed = b.subarray(pos);
  if (width > 32 || packed.length !== Math.ceil(count * width / 8)) throw new CodecError("invalid dictionary indexes");
  return Array.from({ length: count }, (_, i) => {
    const ix = Number(unpack(packed, i * width, width));
    if (ix >= dict.length) throw new CodecError("dictionary index out of range");
    return dict[ix];
  });
}

function decodeDynamic(data: Uint8Array, pos: number, depth: number): [unknown, number] {
  if (depth > 4096 || pos >= data.length) throw new CodecError("truncated or deeply nested dynamic value");
  const tag = data[pos++];
  if (tag === 0) return [null, pos];
  if (tag === 1) return [false, pos];
  if (tag === 2) return [true, pos];
  if (tag === 3 || tag === 4) {
    const [v, next] = readUvarint(data, pos);
    return [tag === 3 ? new GoInt64(unzigzag(v)) : new GoUint64(v), next];
  }
  if (tag === 5) return [readFloat(data, pos, 64), pos + 8];
  if (tag === 6 || tag === 7) {
    const [len, next] = readUvarint(data, pos); pos = next;
    if (len > BigInt(data.length - pos)) throw new CodecError("truncated dynamic bytes");
    const b = data.subarray(pos, pos + Number(len));
    return [tag === 6 ? decodeString(b) : new Uint8Array(b), pos + Number(len)];
  }
  if (tag === 8) {
    const [n, next] = readUvarint(data, pos); pos = next;
    if (n > BigInt(data.length - pos)) throw new CodecError("dynamic list count exceeds payload");
    const values: unknown[] = [];
    for (let i = 0; i < Number(n); i++) { const [v, end] = decodeDynamic(data, pos, depth + 1); values.push(v); pos = end; }
    return [values, pos];
  }
  if (tag === 9 || tag === 10) {
    const [n, next] = readUvarint(data, pos); pos = next;
    if (n > BigInt(data.length - pos)) throw new CodecError("dynamic map count exceeds payload");
    const obj: Record<string, unknown> = Object.create(null), map = new Map<unknown, unknown>();
    for (let i = 0; i < Number(n); i++) {
      let key: unknown;
      if (tag === 9) {
        const [len, p] = readUvarint(data, pos); pos = p;
        if (len > BigInt(data.length - pos)) throw new CodecError("truncated dynamic map key");
        key = decodeString(data.subarray(pos, pos + Number(len))); pos += Number(len);
      } else { const [k, p] = decodeDynamic(data, pos, depth + 1); key = k; pos = p; }
      const [v, p] = decodeDynamic(data, pos, depth + 1); pos = p;
      if (tag === 9) obj[key as string] = v; else map.set(key, v);
    }
    return [tag === 9 ? obj : map, pos];
  }
  throw new CodecError(`unknown dynamic tag ${tag}`);
}

function parseShape(data: Uint8Array, pos: number): [number[], number] {
  const [count, next] = readUvarint(data, pos);
  if (count > 4096n || count > BigInt(data.length - next)) throw new CodecError("invalid evolvable shape");
  pos = next; const fields: number[] = [];
  for (let i = 0; i < Number(count); i++) {
    const [n, end] = readUvarint(data, pos); pos = end;
    if (n > 65536n) throw new CodecError("invalid evolvable field count");
    fields.push(Number(n));
  }
  return [fields, pos];
}

/** Decode a Go Codec message using an explicit schema. Evolvable messages may
 * have fewer trailing fields than the supplied schema; new fields are zeroed. */
export function decode<T = unknown>(schema: CodecType, message: Uint8Array): T {
  if (message.length < 5 || (message[0] & 0xf8) !== 0xc0 || (message[0] & 3) > 2) throw new CodecError("bad message header");
  const widthCodeValue = message[0] & 3, width = widthCodeValue === 0 ? 1 : widthCodeValue === 1 ? 2 : 4;
  const fp = (message[1] | (message[2] << 8) | (message[3] << 16) | (message[4] << 24)) >>> 0;
  let info = schemaInfo(schema), pos = 5, limits: number[] | undefined;
  if ((message[0] & 4) !== 0) {
    [limits, pos] = parseShape(message, pos);
    info = schemaInfo(schema, limits);
  }
  if (fp !== info.fingerprint) throw new CodecError("schema fingerprint mismatch");
  const ctx: DecodeContext = { width, info, limits, depth: 0 };
  const body = message.subarray(pos);
  if (schema.kind === "struct") return decodeFrame(schema, body, ctx) as T;
  const root = types.struct({ V: schema });
  const rootInfo = schemaInfo(root);
  // The non-struct schema fingerprint includes W, while its virtual frame has
  // one struct field. Decode the value span directly from that frame.
  const [value] = decodeVirtualRoot(schema, body, ctx);
  void rootInfo;
  return value as T;
}

/** Decode a complete buffer of length-prefixed messages written by Go's
 * Encoder. A partial final prefix or message is reported as corruption. */
export function decodeStream<T = unknown>(schema: CodecType, stream: Uint8Array, maxMessageSize = MAX_MESSAGE_SIZE): T[] {
  const values: T[] = [];
  let pos = 0;
  while (pos < stream.length) {
    const [len, start] = readUvarint(stream, pos);
    if (len > BigInt(maxMessageSize)) throw new CodecError(`stream message of ${len} bytes exceeds MaxMessageSize`);
    if (len > BigInt(stream.length - start)) throw new CodecError("truncated stream message");
    const end = start + Number(len);
    values.push(decode<T>(schema, stream.subarray(start, end)));
    pos = end;
  }
  return values;
}

function decodeVirtualRoot(schema: CodecType, body: Uint8Array, ctx: DecodeContext): [unknown, number] {
  const bitmapBytes = 1;
  if (body.length < bitmapBytes) throw new CodecError("truncated root frame");
  const present = (body[0] & 1) !== 0;
  if (!present) return [defaultValue(schema), body.length];
  if (schema.kind === "scalar") {
    const info = scalarInfo(schema);
    if (info.cls === "bool") return [true, 1];
    if (info.cls === "int" || info.cls === "uint") {
      const [raw, pos] = readUvarint(body, 1);
      return [integerValue(schema, info.cls === "int" ? unzigzag(raw) : raw), pos];
    }
    if (info.cls === "float") return [readFloat(body, 1, info.bits), 1 + info.bytes];
  }
  const tableEnd = 1 + ctx.width;
  if (tableEnd > body.length) throw new CodecError("truncated root offset");
  const start = readOffset(body, 1, ctx.width);
  if (start < tableEnd || start > body.length) throw new CodecError("invalid root offset");
  return [decodeValuePayload(schema, body.subarray(start), ctx), body.length];
}

/** URL-safe, unpadded Base64 representation of a binary message. */
export function encodeBase64(schema: CodecType, value: unknown, options: { evolvable?: boolean } = {}): string {
  const bytes = encode(schema, value, options);
  let binary = "";
  for (const b of bytes) binary += String.fromCharCode(b);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

/** Decode URL-safe or standard padded Base64 into a schema-checked value. */
export function decodeBase64<T = unknown>(schema: CodecType, text: string): T {
  const clean = text.trim().replaceAll("-", "+").replaceAll("_", "/");
  const binary = atob(clean + "=".repeat((4 - clean.length % 4) % 4));
  return decode<T>(schema, Uint8Array.from(binary, (c) => c.charCodeAt(0)));
}
