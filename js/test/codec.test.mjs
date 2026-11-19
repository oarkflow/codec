import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import test from "node:test";

import { CodecError, GoInt64, decode, decodeBase64, decodeStream, encode, encodeBase64, encodeStream, types as t } from "../dist/index.js";

const bytes = (hex) => Uint8Array.from(hex.match(/../g), (x) => Number.parseInt(x, 16));
const hex = (data) => Buffer.from(data).toString("hex");
const runGo = (args, input = "") => spawnSync("go", ["run", "./test/verify_go.go", ...args], {
  cwd: fileURLToPath(new URL("..", import.meta.url)),
  env: { ...process.env, GOCACHE: "/tmp/codec-js-go-cache" },
  input, encoding: "utf8",
});

test("matches Go's plain and evolvable worked examples byte for byte", () => {
  const schema = t.struct({ A: t.uint32, B: t.string });
  const plain = "c0582db81d03ac02046869";
  const evolved = "c4582db81d010203ac02046869";
  assert.equal(hex(encode(schema, { A: 300, B: "hi" })), plain);
  assert.equal(hex(encode(schema, { A: 300, B: "hi" }, { evolvable: true })), evolved);
  assert.deepEqual(decode(schema, bytes(plain)), { A: 300, B: "hi" });
  assert.deepEqual(decode(schema, bytes(evolved)), { A: 300, B: "hi" });
});

test("encodes and decodes map and non-struct root schemas like Go", () => {
  const mapSchema = t.map(t.string, t.int64);
  const mapMessage = "c02104d5e30102050403046162020202";
  assert.equal(hex(encode(mapSchema, { a: 1n, b: 2n })), mapMessage);
  assert.deepEqual(decode(mapSchema, bytes(mapMessage)), Object.assign(Object.create(null), { a: 1n, b: 2n }));

  const scalar = t.int32;
  const scalarMessage = encode(scalar, -2);
  assert.deepEqual(decode(scalar, scalarMessage), -2);
});

test("reads Go columnar slices and older evolvable structs", () => {
  const point = t.struct({ TS: t.int64, Host: t.string });
  const points = t.slice(point);
  const goColumns = "c0bc42534a010209030801d00f02e40003776562037765620377656203776562";
  assert.deepEqual(decode(points, bytes(goColumns)), [
    { TS: 1000n, Host: "web" }, { TS: 1001n, Host: "web" },
    { TS: 1002n, Host: "web" }, { TS: 1003n, Host: "web" },
  ]);

  const old = t.struct({ Name: t.string });
  const current = t.struct({ Name: t.string, Email: t.string });
  const oldFixture = bytes("c45ac3f68c01010102416461");
  assert.equal(hex(encode(old, { Name: "Ada" }, { evolvable: true })), hex(oldFixture));
  const goOld = oldFixture;
  assert.deepEqual(decode(current, goOld), { Name: "Ada", Email: "" });
});

test("decodes Go nested, pointer, dynamic, map, and evolved values", () => {
  const point = t.struct({ TS: t.int64, Host: t.string });
  const record = t.struct({
    ID: t.uint64, Name: t.string, Active: t.bool, Scores: t.slice(t.int32),
    Labels: t.map(t.string, t.string), Flags: t.array(3, t.bool), Maybe: t.pointer(t.string),
    Groups: t.slice(t.slice(t.string)), Peers: t.slice(t.pointer(point)), Meta: t.dynamic,
  });
  const result = runGo(["--emit"]);
  assert.equal(result.status, 0, result.stderr || result.stdout);
  const fixture = JSON.parse(result.stdout);
  const got = decode(record, bytes(fixture.record));
  assert.equal(got.ID, 9007199254740993n);
  assert.equal(got.Maybe, "linked");
  assert.deepEqual(got.Groups, [["x", "", "y"], []]);
  assert.deepEqual(got.Peers, [{ TS: 9n, Host: "db" }, null]);
  assert.ok(got.Meta.count instanceof GoInt64);
  assert.equal(got.Meta.count.value, 7n);
  assert.deepEqual(got.Meta.flags, [true, false]);
  assert.deepEqual(got.Meta.raw, new Uint8Array([1, 2]));
  assert.deepEqual(decode(t.slice(point), bytes(fixture.points)), [
    { TS: 1000n, Host: "web" }, { TS: 1001n, Host: "web" },
    { TS: 1002n, Host: "db" }, { TS: 1003n, Host: "web" },
  ]);
  const metric = t.struct({ Counter: t.int64, Category: t.uint64, Ratio: t.float64, Name: t.string, Flag: t.bool });
  const metrics = decode(t.slice(metric), bytes(fixture.metrics));
  const metricMessage = bytes(fixture.metrics);
  const offsetWidth = [1, 2, 4][metricMessage[0] & 3];
  const rootOffset = Number(readLittle(metricMessage, 6, offsetWidth));
  const sliceStart = 5 + rootOffset;
  let countBytes = 1;
  while ((metricMessage[sliceStart + countBytes - 1] & 0x80) !== 0) countBytes++;
  assert.equal(metricMessage[sliceStart] & 1, 1, "Go fixture should use columnar rows");
  const laneTag = (i) => sliceStart + Number(readLittle(metricMessage, sliceStart + countBytes + i * offsetWidth, offsetWidth));
  assert.equal(metricMessage[laneTag(0)], 4, "fixture should exercise delta frame-of-reference decoding");
  assert.equal(metricMessage[laneTag(1)], 3, "fixture should exercise integer dictionary decoding");
  assert.equal(metricMessage[laneTag(2)], 3, "fixture should exercise float dictionary decoding");
  assert.equal(metricMessage[laneTag(3)], 3, "fixture should exercise string dictionary decoding");
  assert.equal(metricMessage[laneTag(4)], 0, "fixture should exercise packed bool decoding");
  assert.equal(metrics.length, 100);
  for (let i = 0; i < metrics.length; i++) {
    assert.deepEqual(metrics[i], {
      Counter: 1000000n + BigInt(i) * 100n + BigInt(i % 2) * 2n,
      Category: [1n, 1n << 40n, (1n << 40n) + 10n][i % 3],
      Ratio: i % 2 + 1.5, Name: i % 2 ? "core" : "edge", Flag: i % 2 === 0,
    });
  }
  const current = t.struct({ Name: t.string, Email: t.string });
  assert.deepEqual(decode(current, bytes(fixture.older)), { Name: "Ada", Email: "" });
});

function readLittle(data, pos, width) {
  let out = 0n;
  for (let i = width - 1; i >= 0; i--) out = (out << 8n) | BigInt(data[pos + i]);
  return out;
}

test("base64 armor round-trips and schema mismatches fail clearly", () => {
  const schema = t.struct({ Name: t.string });
  const token = encodeBase64(schema, { Name: "Ada" });
  assert.deepEqual(decodeBase64(schema, token), { Name: "Ada" });
  assert.throws(() => decode(t.struct({ Age: t.uint32 }), encode(schema, { Name: "Ada" })), CodecError);
});

test("reads Go Encoder streams and writes streams Go Decoder can read", () => {
  const schema = t.struct({ ID: t.uint32, Name: t.string });
  const first = { ID: 1, Name: "Ada" }, second = { ID: 2, Name: "Grace" };
  const go = runGo(["--emit"]);
  assert.equal(go.status, 0, go.stderr || go.stdout);
  const fixture = JSON.parse(go.stdout);
  const streamSchema = t.struct({ ID: t.uint64, Name: t.string, Active: t.bool, Scores: t.slice(t.int32), Labels: t.map(t.string, t.string), Flags: t.array(3, t.bool), Maybe: t.pointer(t.string), Groups: t.slice(t.slice(t.string)), Peers: t.slice(t.pointer(t.struct({ TS: t.int64, Host: t.string }))), Meta: t.dynamic });
  const goStream = decodeStream(streamSchema, bytes(fixture.stream));
  assert.equal(goStream.length, 2);
  assert.equal(goStream[0].Name, "Ada");
  assert.deepEqual(decodeStream(schema, encodeStream(schema, [first, second], { evolvable: true })), [first, second]);
  assert.throws(() => decodeStream(schema, Uint8Array.from([3, 1])), CodecError);

  const outgoing = encodeStream(streamSchema, [goStream[0], goStream[1]], { evolvable: true });
  const checked = runGo([], JSON.stringify({ record: fixture.record, stream: hex(outgoing), points: fixture.points, evolved: fixture.evolved }));
  assert.equal(checked.status, 0, checked.stderr || checked.stdout);
});

test("Go can decode messages emitted by the TypeScript encoder", () => {
  const record = t.struct({
    ID: t.uint64,
    Name: t.string,
    Active: t.bool,
    Scores: t.slice(t.int32),
    Labels: t.map(t.string, t.string),
    Flags: t.array(3, t.bool),
    Maybe: t.pointer(t.string),
    Groups: t.slice(t.slice(t.string)),
    Peers: t.slice(t.pointer(t.struct({ TS: t.int64, Host: t.string }))),
    Meta: t.dynamic,
  });
  const point = t.struct({ TS: t.int64, Host: t.string });
  const old = t.struct({ Name: t.string, Age: t.uint32 });
  const recordValue = {
    ID: 9007199254740993n, Name: "Ada", Active: true, Scores: [-2, 0, 3], Labels: { z: "last", a: "first" },
    Flags: [true, false, true], Maybe: "linked", Groups: [["x", "", "y"], []],
    Peers: [{ TS: 9n, Host: "db" }, null], Meta: { role: "admin", count: 7, flags: [true, false], raw: new Uint8Array([1, 2]) },
  };
  const fixtures = {
    record: hex(encode(record, recordValue)),
    stream: hex(encodeStream(record, [recordValue, recordValue], { evolvable: true })),
    points: hex(encode(t.slice(point), [
      { TS: 1000n, Host: "web" }, { TS: 1001n, Host: "web" },
      { TS: 1002n, Host: "db" }, { TS: 1003n, Host: "web" },
    ])),
    evolved: hex(encode(old, { Name: "Ada", Age: 36 }, { evolvable: true })),
  };
  const go = runGo([], JSON.stringify(fixtures));
  assert.equal(go.status, 0, go.stderr || go.stdout);
  assert.match(go.stdout, /Go decoded all TypeScript fixtures/);
});
