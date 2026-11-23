import { performance } from "node:perf_hooks";
import { CodecError, decode, encode, types as t } from "../dist/index.js";

function measure(name, iterations, fn) {
  for (let i = 0; i < Math.min(20, iterations); i++) fn();
  const before = process.memoryUsage().heapUsed;
  const start = performance.now();
  for (let i = 0; i < iterations; i++) fn();
  const elapsed = performance.now() - start;
  const after = process.memoryUsage().heapUsed;
  console.log(`${name}: ${((iterations * 1000) / elapsed).toFixed(1)} ops/s, ${elapsed.toFixed(1)} ms, heap delta ${((after - before) / 1e6).toFixed(2)} MB`);
}

const scalar = t.struct({ ID: t.uint64, Name: t.string, Active: t.bool });
const scalarValue = { ID: 42n, Name: "Ada", Active: true };
const scalarBytes = encode(scalar, scalarValue);
measure("scalar encode", 20_000, () => encode(scalar, scalarValue));
measure("scalar decode", 20_000, () => decode(scalar, scalarBytes));

const row = t.struct({ Index: t.uint64, Counter: t.int64, Host: t.string, Enabled: t.bool });
const batch = t.slice(row);
const rows = Array.from({ length: 1_000 }, (_, i) => ({
  Index: BigInt(i), Counter: BigInt(i * 10), Host: i % 2 ? "api" : "worker", Enabled: i % 3 === 0,
}));
const batchBytes = encode(batch, rows);
measure("columnar 1K encode", 50, () => encode(batch, rows));
measure("columnar 1K decode", 50, () => decode(batch, batchBytes));

const malformed = scalarBytes.slice();
malformed[0] = 0;
measure("malformed decode", 20_000, () => {
  try { decode(scalar, malformed); } catch (err) { if (!(err instanceof CodecError)) throw err; }
});
