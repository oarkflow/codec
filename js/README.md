# JavaScript and TypeScript interoperability

This directory contains a runtime-dependency-free TypeScript implementation of the
Codec binary message format. It uses explicit schemas so JS/TS can read and
write the same bytes as Go. It is prepared as an in-repository npm package; publishing
is a separate release action. To install it from a local checkout:

```sh
cd js
npm ci
npm run build
# in the consuming application's directory:
npm install /path/to/codec/js
```

## Build and verify

Requires Node.js 22 or newer and TypeScript 5.2 or newer. The tests include
Go-produced byte fixtures and pass TypeScript-produced values back through the
Go decoder.

```sh
cd js
npm test
```

## Example

```ts
import { decode, encode, types } from "@oarkflow/codec";

interface User {
  ID: bigint;
  Name: string;
  Active: boolean;
}

const UserSchema = types.struct({
  ID: types.uint64,
  Name: types.string,
  Active: types.bool,
});

const bytes = encode(UserSchema, { ID: 42n, Name: "Ada", Active: true }, { evolvable: true });
const user = decode<User>(UserSchema, bytes);
console.log(user);
```

The equivalent Go schema must use the same exported fields in the same order
and matching Go types:

```go
type User struct {
    ID     uint64
    Name   string
    Active bool
}

data, err := codec.MarshalEvolvable(&user)
```

Use evolvable messages for persisted or long-lived data. New fields may be
appended to structs; field renames, removals, reordering, and type changes are
schema changes and are rejected. Plain messages are also supported and require
an exact schema match.

## Supported values

- Go booleans, signed and unsigned integers, `float32`, `float64`, and strings.
- Structs, pointers, slices, fixed arrays, maps, and `interface{}` values.
- Go columnar struct slices, including plain, dictionary, frame-of-reference,
  delta, and delta-frame-of-reference lanes when decoding Go messages.
- Binary messages, URL-safe Base64 armor, and Go's length-prefixed message streams.

Go's `int` and `uint` are treated as 64-bit, matching the supported Go target.
Use JavaScript `bigint` for 64-bit integers; `number` is accepted only when it
is a safe integer. JS/TS strings encode as UTF-8. A Go string containing
invalid UTF-8 cannot be represented as a JavaScript string and is rejected on
decode. Go custom `MarshalBinary`/`MarshalText` implementations need a
language-specific schema/adapter and are not inferred by this package.

Dynamic `interface{}` integers decode to `GoInt64` or `GoUint64` wrappers so
their signedness survives a decode/encode cycle. Construct these wrappers when
writing 64-bit integer values inside a `dynamic` value.

Maps with string keys decode as plain objects; maps with other key types decode
as `Map`. Supply the same map key and value schemas as the Go type.

## API

`types` provides scalar values (`bool`, `int8`…`int64`, `uint8`…`uint64`,
`float32`, `float64`, `string`, `dynamic`) and constructors for `struct`,
`slice`, `array`, `map`, and `pointer`. Struct field insertion order is wire
order. Reuse one schema object when a Go named struct type appears more than
once, including recursive references.

- `encode(schema, value, { evolvable? })` returns a `Uint8Array`.
- `decode(schema, bytes)` validates the fingerprint and returns the decoded
  value. Evolvable messages written by a schema with fewer trailing fields are
  accepted and newly added fields are zero-valued.
- Decoding is bounded by `MAX_MESSAGE_SIZE` (256 MiB), `MAX_ELEMENTS`
  (16,777,216 columnar elements), and `MAX_ALLOC` (256 MiB estimated decoded
  memory). Pass `{ maxAlloc, maxMessageSize }` as the third argument to
  `decode`, or the fourth argument to `decodeStream`, to lower per-call limits.
  A stream shares one allocation budget across all its messages.
- `encodeStream(schema, values, options)` and `decodeStream(schema, bytes)`
  interoperate with Go's uvarint-length-prefixed `Encoder` and `Decoder`.
- `encodeBase64` and `decodeBase64` handle URL-safe unpadded armor.

The package follows the binary protocol in [the root specification](../README.md#10-specification-formats-protocols-and-standards).
