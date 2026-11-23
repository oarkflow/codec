# Codec

A fast, compact binary codec for Go with a readable text form, zero-copy reads, schema evolution, and a persistent typed store with wildcard queries.

```go
data, err := codec.Marshal(&user)          // struct, slice, map, pointer, interface{} ... anything
err = codec.Unmarshal(data, &user)         // no schema file, no code generation
```

- **Fast and allocation-free.** Encoding allocates nothing. Decoding a whole 1,000-record batch is one allocation (zero with `UnmarshalNoCopy`).
- **Small.** Zero values cost one bit; slices of structs are stored column by column, so batches shrink 17× against Protobuf and 37× against JSON.
- **Works with any Go data.** Structs, slices, arrays, maps, pointers, `interface{}`, `time.Time`, anything with `MarshalBinary`/`MarshalText`, nested to any depth. Verified on 76,000 randomly generated types.
- **Readable too.** A line-based text notation with no braces, no indentation and no indexes to count, for `curl`, config files and debugging.
- **Zero-copy reads.** Read one field out of a message without decoding it.
- **Schema evolution.** Add fields to your structs without migrating data already written.
- **Persistent store.** A typed, crash-safe key-value store with expiry, temporary stores, atomic batches, bounded range reads/deletes, pagination, streaming and wildcard queries.
- **No dependencies, no HTTP, no JSON.** The library imports only the standard library, and a test enforces that it never imports `encoding/json` or `net/http`.

Module: `github.com/oarkflow/codec` (Go 1.27.3+). Linux, macOS and Windows are supported; Unix systems use read-only memory mappings for store segments, while other systems use file reads. Tests, examples and benchmarks are in this repository.

**New here?** Run `go run ./examples/datatypes`: a guided tour of every supported data type and structure, each one round-tripped through all four formats, with its [own README](examples/datatypes/README.md). The formats themselves are defined in [section 10](#10-specification-formats-protocols-and-standards).

## Contents

1. [Quick start](#1-quick-start)
2. [Binary encoding](#2-binary-encoding)
3. [Streams](#3-streams)
4. [Zero-copy reads: View and Column](#4-zero-copy-reads-view-and-column)
5. [Schema evolution](#5-schema-evolution)
6. [Text notation](#6-text-notation)
7. [Base64 armor](#7-base64-armor)
8. [Formats, media types and HTTP](#8-formats-media-types-and-http)
   - [JavaScript and TypeScript](#84-javascript-and-typescript)
9. [The store](#9-the-store)
10. [Specification: formats, protocols and standards](#10-specification-formats-protocols-and-standards)
11. [Performance](#11-performance)
12. [Safety and limits](#12-safety-and-limits)
13. [Examples](#13-examples)
14. [Testing](#14-testing)
15. [FAQ](#15-faq)

---

## 1. Quick start

```go
package main

import (
	"fmt"

	"github.com/oarkflow/codec"
)

type Address struct {
	Street string
	Zip    uint32
}

type User struct {
	Name   string
	Age    uint32
	Tags   []string
	Home   Address
	Orders []Address // slices of structs are stored column by column
}

func main() {
	in := User{Name: "Ada", Age: 36, Tags: []string{"admin", "dev"}, Home: Address{"12 Main St", 12345}}

	// Binary: smallest and fastest.
	data, _ := codec.Marshal(&in)
	var out User
	_ = codec.Unmarshal(data, &out)

	// Text: readable, and what you type into curl.
	text, _ := codec.MarshalText(&in)
	fmt.Print(string(text))
	// Name Ada
	// Age 36
	// Tags admin dev
	// Home.Street 12 Main St
	// Home.Zip 12345

	// Persistent store with wildcard queries.
	// db, _ := store.Open[User]("./users", nil); db.Put("user:1", &in); db.Match("user:*")
}
```

The binary form is positional (fields are matched by order, not by name), so reading needs the same Go type that wrote it, or, with [schema evolution](#5-schema-evolution), a later version of it.

---

## 2. Binary encoding

### 2.1 Functions

| Function | What it does |
|---|---|
| `Marshal(v any) ([]byte, error)` | Encodes `v`. A pointer is followed; `nil` encodes as the zero value. |
| `Append(dst []byte, v any) ([]byte, error)` | Like `Marshal`, appending to `dst`. Reuse a buffer (`buf[:0]`) and encoding allocates nothing. |
| `Unmarshal(data []byte, v any) error` | Decodes into `v`, which must be a non-nil pointer. Strings and bytes are copied out of `data`. |
| `UnmarshalNoCopy(data []byte, v any) error` | Like `Unmarshal`, but decoded strings (and `[]byte` inside `interface{}`) point into `data`. Zero allocations; you must not modify `data` while they are in use. |
| `NewValueDecoder[T]()` | Creates a sequential decoder that reuses the schema plan and scratch state across `Unmarshal` or `UnmarshalNoCopy` calls. Not safe for concurrent use. |
| `MarshalEvolvable` / `AppendEvolvable` | Like `Marshal`/`Append`, but readable by later versions of the type (see [section 5](#5-schema-evolution)). |

```go
buf := make([]byte, 0, 1024)
for _, rec := range records {
	buf, _ = codec.Append(buf[:0], &rec) // no allocation per record
	send(buf)
}
```

Like `json.Unmarshal`, `Unmarshal` follows (and allocates) pointers, so `**T` and `*T` behave alike.

### 2.2 What can be encoded

| Go type | Notes |
|---|---|
| `bool`, `int`/`int8`...`int64`, `uint`/`uint8`...`uint64` | Integers are varints (zigzag for signed). |
| `float32`, `float64` | Raw 4 or 8 bytes; zero costs nothing. |
| `string`, `[]byte` | |
| structs | Exported fields, in declaration order. Unexported fields and fields tagged `codec:"-"` are skipped. |
| slices, arrays | Raw for bytes/bools/floats, delta varints for integers, columnar for slices of structs. |
| maps | Any key type that can be encoded. Output is **sorted by key**, so it is deterministic. |
| pointers | `nil` pointers cost one bit. |
| `interface{}` | Self-describing; decodes to generic types (see below). |
| `time.Time`, `net/netip` types, any type with `MarshalBinary`/`UnmarshalBinary` (or `MarshalText`/`UnmarshalText`) | Encoded through the marshaler. |

Not supported: channels, functions, complex numbers, and interfaces other than `interface{}`. Cyclic data (a pointer chain that loops back) has no finite encoding and returns `ErrTooDeep`.

### 2.3 Behavior to know

- **Zero is absent.** A zero value costs one bit and decodes as zero. There is no `omitempty`; it is always on. Consequently a nil slice and an empty slice are indistinguishable after decoding, as are nil and empty maps.
- **Struct tags.** `codec:"-"` skips a field. `codec:"name"` renames a field in the [text form](#6-text-notation) and in `interface{}` values; it does not affect the binary layout.
- **Decoding resets missing fields to zero.** Existing pointers, slice storage and non-nil maps are **reused** when possible; copy them first if you must keep an earlier decoded snapshot.
- **`interface{}`.** Values inside an `interface{}` are self-describing and decode to `nil`, `bool`, `int64`, `uint64`, `float64`, `string`, `[]byte`, `[]any`, `map[string]any` or `map[any]any`. A struct inside an `interface{}` becomes a `map[string]any`. To decode into an `interface{}`, marshal an `interface{}` variable (`var v any = ...; codec.Marshal(&v)`).
- **Concurrency.** Everything in the package is safe for concurrent use after the first call for a type. The per-type plan is built once and cached.

### 2.4 Errors

| Error | Meaning |
|---|---|
| `ErrNeedPointer` | `Unmarshal` got a non-pointer or nil pointer. |
| `ErrBadHeader` | The data is not a Codec message. |
| `ErrSchemaMismatch` | The message was written by a different type (see [section 5](#5-schema-evolution) for what is allowed). The text says whether the writer was newer. |
| `ErrCorrupt` | Truncated or malformed data. Malformed input returns an error and never panics. |
| `ErrTooDeep` | Nesting beyond 4,096 levels, or cyclic data. |
| `ErrTooLarge` | A message or allocation exceeds a limit (see [section 12](#12-safety-and-limits)). |
| `ErrNotStruct` | An unsupported type. |

---

## 3. Streams

`Encoder` and `Decoder` read and write a sequence of values on an `io.Writer`/`io.Reader`, each prefixed with its length, like `json.Encoder`.

```go
enc := codec.NewEncoder(w)
enc.Evolvable = true // optional: later versions of the type can read these
for _, rec := range records {
	if err := enc.Encode(&rec); err != nil { ... }
}

dec := codec.NewDecoder(r)
for dec.More() {
	var rec Record
	if err := dec.Decode(&rec); err != nil { ... } // io.EOF at a clean end
}
```

`Decode` reuses one buffer between calls. `codec.MaxMessageSize` (default 256 MiB) bounds the frame a `Decoder` will read. An `Encoder` or `Decoder` is not safe for concurrent use.

---

## 4. Zero-copy reads: View and Column

`View` opens a message for lazy access. Nothing is decoded until you ask, so reading one field of a large message costs a few reads, not a full decode.

```go
type Point struct { Street string; Zip uint32 }
type Reading struct {
	Host   string
	Times  []int64
	Points []Point
}

v, err := codec.View(data, &Reading{})   // the second argument supplies the type only

host, _ := v.Field("Host")
fmt.Println(host.String())               // copies the string
fmt.Println(host.StringRef())            // zero-copy: valid while data is alive and unmodified

points, _ := v.Field("Points")
fmt.Println(points.Len())                // 4
first, _ := points.Index(0)              // one element, without decoding the others
street, _ := first.Field("Street")
fmt.Println(street.String())
```

| `Value` method | Returns |
|---|---|
| `Field(name)` | A struct's field (`ErrNoField` if it does not exist; fields added after the message was written read as zero). |
| `Index(i)` | A slice or array element (`ErrIndex` if out of range). |
| `Int()`, `Uint()`, `Float()`, `Bool()` | Numbers, read in place with no allocation. |
| `String()`, `Bytes()` | Copies, safe to keep. |
| `StringRef()`, `BytesRef()` | Zero-copy views into the message; treat as read-only. |
| `Len()` | Length of a string, bytes or slice. |
| `Present()` | Whether the value is non-zero in the message. |

Costs: `Field` is O(number of fields); `Index` is O(1) for fixed-width slices, row layouts and bit-packed integer columns, O(i) for delta-encoded and plain string columns.

### Bulk reads

Decode a whole slice of scalars in one pass, appending to a slice you can reuse:

```go
times, _ := v.Field("Times")
ts, _ := times.Ints(nil)                  // [10 20 30]

col, _ := points.Column("Zip")            // one field of every struct in a slice of structs
zips, _ := col.Uints(nil)                 // [1 2 3 4], decoding that column once
col, _ = points.Column("Street")
streets, _ := col.StringsRef(nil)         // zero-copy strings
```

`Value` offers `Ints`, `Uints`, `Floats`, `Bools`, `Strings` and `StringsRef` for slice values, and `Column(name)` for a slice of structs. `Column` offers the same readers for one field, decoding that column's lane once instead of element by element (the columnar layout makes this the fastest way to scan one field of a large batch).

---

## 5. Schema evolution

Plain messages carry a fingerprint of the whole schema, so they can only be read by exactly the same type. Data that outlives one program version (files, databases, queues) should be written with `MarshalEvolvable`. Those messages also record how many fields each struct had, so a later version of the type, with fields **added at the end** of its structs, can still read them. Added fields come back as zero.

```go
type UserV1 struct{ Name string; Age uint32 }
type UserV2 struct{ Name string; Age uint32; Email string; Tags []string } // appended

data, _ := codec.MarshalEvolvable(&UserV1{"Ada", 36})
var u UserV2
codec.Unmarshal(data, &u) // u.Email == "", u.Tags == nil
```

| Change | Result |
|---|---|
| Append fields to a struct, at any depth (inside slices, maps, pointers, arrays, nested structs) | Allowed |
| Introduce new nested struct types in appended fields | Allowed |
| Rename a struct *type* | Allowed |
| Read an older message with a newer type | Allowed (new fields are zero) |
| Read a **newer** message with an older type | `ErrSchemaMismatch`, saying "written by a newer schema" |
| Remove, reorder, rename or retype a field, or insert one before existing fields | `ErrSchemaMismatch` |

To retire a field, keep it in the struct and stop using it.

Details:

- **Cost:** 1 byte plus 1 byte per struct type in the message. Encoding and same-version decoding are the same speed as plain messages; decoding data from an older version is about 1.3× the like-for-like baseline.
- **Plain messages stay strict.** `Marshal` data needs an exact match. If you want evolution, write with `MarshalEvolvable` from the start.
- `Unmarshal`, `UnmarshalNoCopy`, `View`, `Column` and `Decoder` all read both kinds. `Encoder.Evolvable = true` writes evolvable messages to a stream.
- The [store](#9-the-store) writes evolvable records by default.
- Derived read plans are cached per shape (at most 256), so a hostile header cannot grow memory.

See `examples/evolve`.

---

## 6. Text notation

A readable, hand-writable form of exactly the values the binary form carries, with its own printer and parser. It has **no braces, no brackets, no indentation, no `begin`/`end`, and no indexes to count**.

```go
text, _ := codec.MarshalText(&order)   // value -> text
err := codec.UnmarshalText(text, &order) // text -> value
```

Every line is `path value`:

```
ID           7
Customer     Ada Lovelace
Lines.+                       # "+" starts the next list item
Lines.SKU    sku-1            # a plain name fills in that item
Lines.Qty    2
Lines.+
Lines.SKU    sku-2
Tags         rush "gift wrap" # several simple values on one line
Meta.source  web              # map keys are part of the path
Addr.Zip     12345            # nested object
Matrix.+     1 2 3            # list of lists: one "+" line per row
Matrix.+     4 5 6
Stock."red pear"  5           # quote a name that has spaces or dots
Owner        ~                # null
```

### 6.1 The complete rules

- **Lines.** A line is `path value`: the path runs to the first space; the value is the rest of the line. Lines starting with `#` are comments; blank lines are ignored. Lines may come in any order; later lines override earlier ones.
- **One-liners.** A `;` also ends a line, so a whole document fits in one line, which suits `curl -d`:
  `Name Ada; Age 36; Addr.Zip 12345; Items.+; Items.SKU a; Items.+; Items.SKU b`
- **Paths.** A path is names joined by dots. Each name is a struct field or a map key. A name containing spaces, dots, quotes or other special characters is written in double quotes. Field names are matched ignoring case.
- **Lists.**
  - `Items.+` appends a new, empty item; `Items.+ value` appends an item with that value (`Tags.+ admin`, `Matrix.+ 1 2 3`).
  - A name after a list means "in the item just started" (`Items.SKU x`). If there is no item yet, the first such line starts the first one, so a one-item list needs no `+`.
  - For a list inside a list, `^` means "the item just started": `Cube.+; Cube.^.+ a b; Cube.^.+ c`.
  - A number (`Items.2.Qty 5`) picks an existing item, for editing in place. The next free number appends. Fixed-size arrays use numbers, since they cannot grow.
- **Strings.** Written bare when that is unambiguous, otherwise in double quotes with the escapes `\n \r \t \\ \" \xHH \uXXXX \UXXXXXXXX`. A bare string is the whole rest of the line, so `Name Ada Lovelace` needs no quotes. In a line of several values the words are split on spaces, so a word containing spaces is quoted. Invalid UTF-8 survives as `\xHH`.
- **Numbers.** `42`, `-7`, `0x2A`, `3.5`, `1e9`, `nan`, `inf`, `-inf`. Out-of-range values are an error.
- **Booleans.** `true`, `false`.
- **Bytes.** Hex with a `0x` prefix: `Raw 0x00ff10` (`0x` alone is empty).
- **Null and zero.** `~` is null. An absent field, an empty value, `0`, `""` and `false` are all the zero value. Printing omits zero fields (as the binary form does), so printed text is as short as it can be. An item that is itself zero is written `Items.+` so it keeps its place.
- **Empty but present.** `~list` and `~map` mean "an empty list/map, not nil". They are used for `interface{}` values and for non-nil pointers to collections.
- **Errors.** Unknown field names are an error that lists the valid ones, with line and column.
- **`interface{}` values.** Self-describing: `36` is `int64` (`uint64` beyond `MaxInt64`), `3.5` is `float64`, `true`/`false` are bool, `~` is nil, `"..."` or any other word is a string, `0x..` is `[]byte`. A path through an `interface{}` builds `map[string]any` for names and `[]any` for `+`. Several words on one line give a `[]any` of them. Quote a string that would otherwise read as one of these (`"36"`, `"true"`), and a name that is a number to keep it a key. Structs inside an `interface{}` print as maps.
- **Marshalers.** Types implementing `encoding.TextMarshaler` (`time.Time`, `netip.Addr`, ...) are written as a string; types with only `encoding.BinaryMarshaler` as hex bytes.
- **Single values.** A document that is a single simple value is just that value on one line; a top-level list is `+ first`, `+ second`.

### 6.2 Functions

| Function | What it does |
|---|---|
| `MarshalText(v) ([]byte, error)` | The text of `v`. |
| `AppendText(dst, v) ([]byte, error)` | Appending to `dst`. Allocates almost nothing. |
| `UnmarshalText(text, v) error` | Parses into `v` (a non-nil pointer). Reuses slice and map storage. Errors are `*TextError{Line, Col, Msg}`. |
| `ToText(data, schema) ([]byte, error)` | Converts a binary message to text. `schema` is a pointer to a value of the marshaled type (used only for its type). |
| `FromText(text, schema) ([]byte, error)` | Converts text to a binary message. |
| `AppendTextEntry(dst, key, v)` | Appends the lines of one top-level map entry (`key.path value`), for building a document of many records without holding them all in memory. |
| `TextKey(line) (string, error)` | The first (unquoted) name in a line's path: the key of a top-level map entry. For splitting such a document. |

### 6.3 Guarantees and limits

- Binary → text → binary preserves the data exactly. Text is the same data, not a lossy rendering.
- `interface{}` numbers come back as `int64`, `uint64` (above `MaxInt64` only) and `float64`, not as narrower types.
- A pointer to an all-zero value is written so that it reads back non-nil (an explicit zero field, or `~list`/`~map`). A pointer to a pointer to nothing (`**T`) can collapse to nil.
- Map keys in text must be simple values (numbers, strings, bools, text-marshalable types). The binary form has no such restriction.
- In a one-line `;` document, a bare string containing `;` must be quoted (the printer does this automatically).
- Speed: encode is about as fast as `encoding/json`, decode about the same, with 5 and 1 allocations instead of hundreds. Binary is about 7× faster than text; use text where people read it.

---

## 7. Base64 armor

The binary message as URL-safe base64 text (no padding), for places that need a single opaque token: a URL parameter, a header, an environment variable. About 4/3 the binary size.

| Function | What it does |
|---|---|
| `MarshalBase64(v)` / `AppendBase64(dst, v)` | Encode. |
| `UnmarshalBase64(text, v)` | Decode. Whitespace around the token is ignored, and standard base64 (`+`, `/`, `=` padding) is accepted as well as the URL-safe form. |

---

## 8. Formats, media types and HTTP

The library has no transport dependency. It provides plain functions over strings and bytes, so any transport that labels its payloads (HTTP, queues, files) can negotiate between the three representations.

### 8.1 Formats

| `Format` | Media type constant | Value |
|---|---|---|
| `FormatBinary` | `MediaBinary` | `application/x-strata` |
| `FormatText` | `MediaText` | `text/x-strata` |
| `FormatBase64` | `MediaBase64` | `application/x-strata+base64` |

The `strata` media-type names are established wire-protocol identifiers and
remain stable for compatibility; the library and packages are named Codec.

### 8.2 Functions

| Function | What it does |
|---|---|
| `EncodeAs(dst, f, v)` | Appends `v` in format `f`. |
| `DecodeAs(contentType, body, v)` | Decodes a labeled payload and reports the format that was used. |
| `Negotiate(accept, fallback)` | Picks a `Format` from an `Accept` header value, honoring q-values. `text/plain` selects text; `*/*` or empty selects `fallback`. |
| `Detect(contentType, body)` | Decides the format of a payload. Strata media types are exact. Generic labels that tools attach by default (empty, `text/plain`, `application/octet-stream`, curl's `application/x-www-form-urlencoded`) are sniffed: base64-only payloads are base64 (falling back to text if they do not decode), anything else is text; `application/octet-stream` defaults to binary. Any other label is an error. |
| `FormatForMedia(mediaType)` | Maps a media type (parameters ignored) to a `Format`. |
| `ParseFormat(name)` | Understands `binary`, `text`, `base64` (and aliases). |
| `Format.MediaType()` / `Format.String()` | The media type (with `charset` for text) and the name. |

### 8.3 HTTP

`examples/http` wires format detection, decoding, encoding, and negotiation to `net/http` (the library itself does not import it) and puts a store behind it. Its default demo uses an in-process HTTP transport; `-serve` starts a real listener. Every request and response can be any of the three formats:

```
curl -X PUT localhost:8080/users/ada -d 'Name Ada; Age 36; Tags admin dev'
curl localhost:8080/users/ada                          # answers in readable text
curl -H 'Accept: application/x-strata' localhost:8080/users/ada | xxd
curl 'localhost:8080/users/ada?format=base64'          # one opaque token
curl -g 'localhost:8080/users?match=user:[ab]*'        # wildcard search (-g: no curl globbing)
curl -X DELETE localhost:8080/users/ada
```

Run `go run ./examples/http` for a self-contained demo, or `go run ./examples/http -serve :8080` to keep the server up. Errors come back in the negotiated format, for example `error codec text: line 1, column 1: main.User has no field "Nmae" (fields: Name, Age, Tags, Email)`.

### 8.4 JavaScript and TypeScript

The [`js`](js/README.md) package implements the binary wire format with
explicit JS/TS schemas. It reads Go messages, writes messages the Go decoder
accepts, supports evolvable schemas, Base64 armor and Go's length-prefixed
streams, and includes cross-language fixtures. Run `cd js && npm test` to build
it and verify both directions. The package is currently source-only in this
repository and prepared for npm packaging; publishing is a separate release
action. Its decoder bounds frames and estimated decoded memory. Custom Go
marshalers and invalid UTF-8 Go strings require application-specific handling.

---

## 9. The store

`package store` is a persistent, typed key-value store built on the codec:

```go
import "github.com/oarkflow/codec/store"

db, err := store.Open[User]("./users", nil)   // a directory; created if missing
defer db.Close()

db.Put("user:1", &user)
var u User
err = db.Get("user:1", &u)
db.Update("user:1", func(u *User, exists bool) error { u.Age++; return nil })
db.Delete("user:1")
```

### 9.1 Design

A segmented log-structured store with an ordered in-memory index:

- **Writes append** to the active segment file; nothing is rewritten in place. When a segment reaches `SegmentSize` a new one starts, so no file grows without bound.
- **The index** is a compressed radix tree from key to the location of the latest value: 24 bytes of location metadata per key (including expiry) plus shared key prefixes. A `Get` is one tree lookup plus a value read and decode. Segments use read-only memory mappings on supported Unix systems. Snapshot scans refresh the active segment mapping as it grows; appended bytes past the current mapping use file reads until the next refresh. Mapped pages are faulted in by the OS as needed, and the optional read cache has an explicit byte limit. The index itself scales with key count; stored values remain in the segment files.
- **Ordered keys** make prefix and wildcard scans visit only the keys they need, in sorted order.
- **Every record has a CRC.** A crash leaves at most a torn tail, which is cut off on reopen.
- **Compaction** reclaims overwritten and deleted space in the background, a few segments at a time, without blocking readers or writers except for short moments.
- **A checkpoint** of the index (written on `Close` and by `Checkpoint`) lets `Open` skip replaying the log: reopening 388 MB took 23 ms from the checkpoint against 143 ms replaying.

Limits: keys up to 64 KiB, values up to 1 GiB, all keys must fit in RAM. A `DB` is safe for concurrent use in one process; it does **not** lock the directory against other processes.

### 9.2 Reading and writing

| Method | What it does |
|---|---|
| `Open[T](dir, *Options)` | Opens or creates a store. Replays the log (or loads the checkpoint). |
| `Put(key, *T)` | Stores a value, replacing any previous one. |
| `PutTTL(key, *T, duration)` | Stores a value that becomes unavailable after the duration, starting after encoding; too-large durations return `store.ErrTTLTooLarge`. |
| `PutUntil(key, *T, time)` | Stores a value until an absolute time; zero time means no expiry. Times outside the supported Unix-nanosecond range return `store.ErrExpiryRange`. |
| `PutIfAbsent(key, *T)` | Atomically stores a value only when the key is missing or expired. |
| `PutIfAbsentTTL(key, *T, duration)` | `PutIfAbsent` with expiry starting after encoding; too-large durations return `store.ErrTTLTooLarge`. |
| `Expire(key, duration)` | Sets expiry on a live key without decoding or re-encoding its value; too-large durations return `store.ErrTTLTooLarge`. |
| `Persist(key)` | Removes expiry from a live key. |
| `RemainingTTL(key)` | Returns time until expiry; zero means the key has no expiry. |
| `Get(key, *T)` | Decodes the value; `ErrNotFound` if missing. |
| `GetBorrowed(key, fn)` | Decodes without copying strings (and byte slices inside interface values), then calls `fn` while holding a read lock. The value must not be retained, and `fn` must not call DB methods. |
| `GetMany(keys)` | Reads keys in input order as bounded 256-key chunks, returning aligned values and found flags; missing or expired keys are not errors. Concurrent writes may be reflected between chunks. |
| `GetManyBorrowed(keys, fn)` | Visits keys in order without allocating a result slice. The decoded `*T` is reused and its no-copy fields are valid only during the callback. The callback runs under a chunk read lock, so it must not call this DB or retain the value or its fields; return `false` to stop early. |
| `Delete(key)` | Removes a key; `ErrNotFound` if missing. |
| `Has(key)`, `Len()` | Existence and key count. |
| `Update(key, fn)` | Atomic read-modify-write. `fn(v *T, exists bool) error` gets the current value (zero if absent). If `fn` returns nil the result is stored; an error writes nothing. Existing expiry is preserved. No other write interleaves, so keep `fn` quick and do not call the `DB` from it. |
| `UpdateTTL(key, duration, fn)` | Atomic read-modify-write that sets expiry after the updated value is encoded; a missing or expired key is created with that expiry. Too-large durations return `store.ErrTTLTooLarge`; a callback error writes nothing. |
| `View(key, fn)` | Gives `fn` the raw encoded bytes (valid only during the call), for `codec.View` or `codec.UnmarshalNoCopy`. |
| `NewBatch()` | Collects writes for atomic commit (below). |
| `Sync()` | Flushes to stable storage. |
| `Stats()` | `Keys`, `Segments`, `FileBytes`, `DeadBytes` (reclaimable), and read-cache bytes/entries. |
| `Checkpoint()` | Saves the index now (also done on `Close`). Writers wait while it runs. |
| `Compact()` | Reclaims dead space now. |
| `Close()` | Stops background work, syncs the active segment, writes a checkpoint, closes files. |

### 9.3 Atomic batches

```go
b := db.NewBatch()
b.Put("acc:3", &Account{...})
_ = b.Delete("acc:2")
err := b.Commit()   // one write: after a crash, all of it is visible or none of it
```

`Batch` has `Put`, `PutTTL`, `PutUntil`, `Delete` (deleting a missing key is not an error), `Len`, `Reset` and `Commit`. Relative TTLs begin at `Commit`; expiry metadata is part of the same atomic commit as each value. A batch is limited to 1 GiB of encoded records and lives in one segment. Oversized keys or batches return `store.ErrTooLarge`; a failed `Commit` retains queued operations for retry, while `Reset` discards them.

With `SyncAlways`, each successful `Put` or `Batch.Commit` waits for its write to reach durable storage. Otherwise, recent writes are synced by `Sync`, `SyncInterval`, or a successful `Close`. A batch amortizes sync cost across its records; durable-write latency depends strongly on batch size and the storage device.

### 9.4 Wildcard and pattern queries

```go
keys, _ := db.Match("user:*:email")        // sorted matching keys
n, _     := db.Count("session:u[12]:*")    // no values are read
db.Scan("order:2026-*", func(key string, o *Order) bool { ...; return true })
db.ScanPrefix("user:", fn)                 // literal prefix
deleted, _ := db.DeleteMatch("session:*")  // atomic batches of 1000
```

| Pattern | Matches |
|---|---|
| `*` | any run of characters, including none (and including `/`) |
| `?` | exactly one character |
| `[abc]`, `[a-z]` | one character from the set / range |
| `[^abc]`, `[!abc]` | one character not in the set |
| `\x` | the character `x` literally |

Everything before the first wildcard is a literal prefix, and only keys under that prefix are examined: `user:42:*` touches only user 42's keys (about 1 µs over a million keys), while `*:email` has no prefix and must examine every key (about 65 ms per million keys). It still reads values only for keys that match. Matching is never exponential.

| Method | What it does |
|---|---|
| `Match(pattern)` | Sorted matching keys. |
| `Count(pattern)` | How many keys match, reading no values. |
| `Keys()` | All keys, sorted, in chunks that allow writers to proceed between pages. |
| `KeysSnapshot()` | All live keys, sorted from one stable view; writers wait until the returned slice is collected. Repeated full owned snapshots can reuse the lazy key cache. |
| `Scan(pattern, fn)` | Matching keys with decoded values, sorted. Return `false` to stop. The `*T` is reused between calls, so copy anything you keep. `fn` may call `DB` methods, including writes; the scan sees each value as of when it is reached and skips keys deleted meanwhile. |
| `ScanKeys(pattern, fn)` | Matching keys without reading values. |
| `ScanPrefix(prefix, fn)` | `Scan` for a literal prefix. |
| `Range(fn)` | `Scan("*", fn)`. |
| `ScanSnapshot(pattern, fn)` | Scans one stable index view while holding a read lock. The callback must not call DB methods; writers and compaction wait until it returns. |
| `ScanSnapshotNoCopy(pattern, fn)` | Snapshot scan using `codec.UnmarshalNoCopy`; decoded fields are borrowed and valid only during the callback. Copy fields you retain. |
| `ScanSnapshotBorrowed(pattern, fn)` | Snapshot scan with borrowed key bytes and no-copy decoded fields. Both are valid only during the callback; copy anything retained. |
| `RangeSnapshot(fn)` | `ScanSnapshot("*", fn)`. |
| `RangeSnapshotNoCopy(fn)` | `ScanSnapshotNoCopy("*", fn)`. |
| `RangeSnapshotBorrowed(fn)` | `ScanSnapshotBorrowed("*", fn)`. |
| `TakeRange(start, end, limit, fn)` | Visits up to `limit` live sorted keys in `[start, end)` in chunks of at most 256 keys or 8 MiB of encoded values (one larger value is allowed); empty `end` is unbounded. The callback runs outside the lock and may call DB methods. Its `*T` is reused; copy data you retain. |
| `TakeRangeBorrowed(start, end, limit, fn)` | Bounded range scan with borrowed keys and no-copy values; the callback holds a read lock for up to 1024 keys and must not call DB methods or retain either argument. |
| `DeleteRange(start, end, limit)` | Deletes up to `limit` live keys in `[start, end)` in atomic chunks without decoding values; empty `end` is unbounded. |
| `TakeRangeSnapshotBorrowed(start, end, limit, fn)` | Bounded stable range with borrowed key bytes and no-copy values; callback must not call DB methods or retain either argument. |
| `Page(pattern, after, limit)` | Returns a page of decoded entries and an exclusive cursor bound to the same pattern for the next page; changing patterns returns `store.ErrBadCursor`. |
| `Stream(ctx, pattern)` | Streams sorted `Item[T]` values until exhausted or canceled. The channel buffers up to 256 items; each page is capped at 8 MiB of encoded values, except for one larger value. |
| `StreamResults(ctx, pattern)` | Like `Stream`, with a terminal result carrying any scan error. |
| `StreamBorrowed(ctx, pattern, fn)` | Visits sorted matches synchronously with borrowed keys and decoded fields in bounded chunks; callback data is valid only during the call, and the callback must not call the DB or retain borrowed data. Cancellation returns `ctx.Err()`. |
| `DeleteMatch(pattern)` | Deletes every match in atomic batches of 1000; returns the count. |
| `store.Escape(s)` | Quotes pattern metacharacters so `s` matches literally. |
| `store.MatchKey(pattern, key)` | Tests one key against a pattern. |

A malformed pattern returns `store.ErrBadPattern`. `Scan`, `Range`, `TakeRange`, `TakeRangeBorrowed`, `DeleteRange`, and streaming work in chunks, so long scans yield regularly to writers. Concurrent writes can be reflected between chunks. `ScanSnapshot` holds a read lock for the full scan; use it when a stable view and maximum traversal throughput matter more than write latency.

The first full `ScanSnapshot("*")`, `RangeSnapshot`, or `KeysSnapshot` uses the regular traversal. If another full owned snapshot is requested, that call lazily builds an ordered cache of stable key strings for reuse by later full snapshots. Inserts and deletes invalidate the cache, and the next full snapshot rebuilds it. The cache retains approximately one string header plus the key bytes per entry; borrowed snapshot scans avoid that retained cache.

`OpenTemp[T](&Options{})` creates an isolated store whose directory is removed
when `Close` returns. It is useful for caches and scratch data that should not
survive the process. The temporary directory is still backed by the filesystem
while the store is open.

Expired entries behave as missing for `Get`, `Has`, scans, and `Len`. Expiry is
stored in the checksummed record, survives reopen, and is preserved by
compaction. A background worker appends tombstones after expiry so expired keys
leave the index; compaction later reclaims their record bytes. Set
`Options.ExpiryInterval` to adjust the cleanup cadence or a negative value to
disable the worker.

### 9.5 Options

```go
db, _ := store.Open[User]("./users", &store.Options{
	SegmentSize:  64 << 20,           // default 64 MiB, at most 2 GiB
	SyncAlways:   false,              // sync every write (durable, much slower)
	SyncInterval: time.Second,        // background sync when SyncAlways is false
	ExpiryInterval: time.Second,      // background removal of expired keys
	ReadCacheBytes: 8 << 20,          // optional cache of recent Get values
	StreamPageBytes: 8 << 20,         // encoded value bytes retained per stream page
	CompactRatio: 0.5,                // compact segments that are >50% dead; negative disables
	Strict:       false,              // true writes plain messages (see below)
})
```

| Option | Default | Meaning |
|---|---|---|
| `SegmentSize` | 64 MiB | Size at which the active segment is closed. Smaller segments mean finer-grained compaction. |
| `SyncAlways` | false | Forces each successful write to durable storage before return. Without it, a process crash preserves writes held by the OS, while power loss can lose recent writes until `Sync`, `SyncInterval`, or successful `Close`. |
| `SyncInterval` | 0 | If positive (and `SyncAlways` is false), syncs writes in the background at this interval. |
| `ExpiryInterval` | 1 s | How often the background worker removes expired keys; negative disables it. |
| `ReadCacheBytes` | 0 | Maximum key and encoded-value bytes held in the optional FIFO `Get` read-through cache; zero disables it. This limit does not include map/list overhead. |
| `StreamPageBytes` | 8 MiB | Maximum encoded value bytes retained per stream page; a single larger value is streamed alone. This limit does not include decoded object overhead. |
| `CompactRatio` | 0.5 | Background compaction of segments whose dead bytes exceed this fraction. Negative disables automatic compaction (`Compact` still works). |
| `Strict` | false | Write plain messages (one byte per struct type smaller) that only the exact same type can read. By default records are evolvable. |

### 9.6 Schema evolution in the store

The store writes evolvable records by default, so an upgraded program opens yesterday's database as it is. Records written by the old type read as the new type with the added fields zero; updating a record rewrites it in the new layout; compaction copies old and new records alike. An old program reading a record written by a newer type gets a clear "written by a newer schema" error instead of misreading it.

### 9.7 Export and import

`ExportText(w)` writes every entry in the [text notation](#6-text-notation), one `key.path value` line per field, in key order, with the lines of one entry adjacent:

```
user:1.Name Ada
user:1.Tags admin dev
user:2.Name Bob
```

`ImportText(r)` reads it back (blank lines and `#` comments skipped) in atomic batches of 1000. Keys containing dots, spaces or quotes are written quoted. The file can be edited by hand.

### 9.8 On-disk layout

```
MANIFEST        ordered list of live segment ids (atomically replaced)
NNNNNNNNNN.seg  segments: magic, then records, append-only
INDEX           optional checkpoint of the key index
```

A v1 record is `crc32c | key length | value length + flags | key | value`.
A v2 record adds an absolute expiry timestamp to the header. Old segments and
checkpoints remain readable; new writes use v2, and compaction writes v2
segments. A checkpoint is ignored (and the log fully replayed) unless every
segment it lists is still in the manifest and at least as long as recorded, so
a stale or damaged checkpoint can never produce wrong data. Compaction writes
new segments completely, swaps the manifest atomically, then deletes the old
ones, so a crash at any point leaves a consistent store.

See `examples/store`.

---

## 10. Specification: formats, protocols and standards

This section defines everything another implementation would need: the binary message format, the stream protocol, the text notation, the base64 armor, the media types and negotiation rules, the store's file formats, and the pattern language. The Go implementation in this repository is the reference; the byte examples below were produced by it.

Keywords **MUST**, **MUST NOT**, **SHOULD** and **MAY** are used as in RFC 2119. "Decoder" and "encoder" refer to any implementation.

### 10.1 Standards and conventions used

| Concern | Standard / convention |
|---|---|
| Byte order | Little-endian for all fixed-width integers and floats |
| Variable-length integers | Unsigned LEB128, as in Protocol Buffers and Go's `encoding/binary` (`uvarint`): 7 bits per byte, low group first, high bit = continuation, at most 10 bytes |
| Signed integers | ZigZag mapping `(v << 1) ^ (v >> 63)` to unsigned, then `uvarint` |
| Floating point | IEEE 754-2008 binary32 / binary64, written as their raw bit patterns (little-endian). NaN payloads and `-0` are preserved |
| Strings (binary) | Arbitrary bytes. Not validated as UTF-8 |
| Strings (text notation) | UTF-8 (RFC 3629); bytes that are not valid UTF-8 are written as `\xHH` escapes |
| Checksums (store) | CRC-32C (Castagnoli) |
| Schema fingerprint | FNV-1a 64-bit, folded to 32 bits |
| Base64 armor | RFC 4648 section 5 (URL-safe alphabet), no padding on output; standard alphabet and padding accepted on input |
| Timestamps in text | RFC 3339 (via `time.Time.MarshalText`) |
| Content negotiation | HTTP `Accept` / `Content-Type` semantics (RFC 9110), including `q` weights |
| Media types | `application/x-strata`, `text/x-strata`, `application/x-strata+base64`. The `x-` prefix marks them as **not registered** with IANA |

### 10.2 Binary message

```
message = header frame
```

A message is self-delimiting only inside a transport that supplies its length (a file, a stream frame, an HTTP body, a store record).

#### Header

```
header       = marker fingerprint [ shape ]
marker       = 1 byte:   bits 7..3 = 11000   (fixed)
                         bit  2    = evolvable flag
                         bits 1..0 = offset width code: 0 => 1 byte, 1 => 2 bytes, 2 => 4 bytes
fingerprint  = 4 bytes (uint32, little-endian)
shape        = present iff the evolvable flag is set: uvarint(n) then n uvarint field counts
```

The valid marker bytes are `0xC0 0xC1 0xC2` (plain) and `0xC4 0xC5 0xC6` (evolvable). A decoder **MUST** reject every other first byte, and width code 3, as a bad header. `W` below is the offset width the marker selects. An encoder **MUST** choose a `W` in which every offset in the message fits; the reference encoder chooses the narrowest (1 byte for bodies of up to 256 bytes, 2 up to 65,536, otherwise 4), so a message is limited to 4 GiB.

#### Schema fingerprint

The fingerprint is `uint32(h) XOR uint32(h >> 32)` where `h` is the 64-bit FNV-1a hash (offset basis `14695981039346656037`, prime `1099511628211`) of the schema description string:

```
schema   = type [ "W" ]                      "W" is appended when the root is not a struct
type     = scalar | array | slice | map | pointer | struct | back-ref | marshaler
scalar   = kind "." size                     size = size in bytes of the Go type; strings and interfaces are 16
array    = "[" length "]" type
slice    = "[" "]" type
map      = "map[" type "]" type
pointer  = "*" type
struct   = "{" { fieldname ":" type ";" } "}"      exported, non-excluded fields, in order, by Go field name
back-ref = "@" index                         a struct type seen before, by its position in first-use order
marshaler= ( "bin:" | "txt:" ) typename      types encoded through MarshalBinary / MarshalText
kind     = 1 bool | 2 int | 3 uint | 4 float32 | 5 float64 | 6 string | 12 interface{}
```

Examples: `struct{A uint32; B string}` is `{A:3.4;B:6.16;}` (fingerprint bytes `58 2d b8 1d`); `[]struct{TS int64; Host string}` is `[]{TS:2.8;Host:6.16;}W`; `map[string]int64` is `map[6.16]2.8W`; `interface{}` is `12.16W`.

A plain message's decoder **MUST** check the fingerprint against its own schema and reject a mismatch. The fingerprint is a hash of a Go schema (Go field names, Go type sizes, Go type names for marshaler types), so an implementation in another language either reproduces that description or skips the check and relies on a known schema.

#### Shape (evolvable messages only)

`shape` lists the number of fields (after excluding unexported and `codec:"-"` fields) of **every struct type** in the schema, in order of first use: depth-first, fields in declaration order, root first. A struct type that appears more than once is listed once. The fingerprint of an evolvable message is the fingerprint of *the writer's* schema. A reader whose type has the same shape compares the fingerprint directly. A reader whose structs have **more** fields at the end rebuilds the writer's schema by keeping only the first `count` fields of each struct, and compares the fingerprint of that. A reader whose struct has **fewer** fields than the writer's count, or whose truncated schema hashes differently, **MUST** reject the message (a newer message read by an older type, or a non-append change).

#### Frame

A frame encodes one struct.

```
frame   = bitmap scalars offsets heap
bitmap  = max(1, ceil(nFields / 8)) bytes; bit i (byte i/8, bit i%8, least significant first) set => field i is present
scalars = for each present field in order, if it is a scalar kind: its value
offsets = for each present non-scalar field in order: one W-byte little-endian offset
heap    = the payloads of the present non-scalar fields, in field order
```

- An **offset** is measured from the first byte of the frame (the bitmap). The payload of the *k*-th present non-scalar field spans from its offset to the next one's offset; the last spans to the end of the frame. There are no length prefixes. A decoder **MUST** reject offsets that are smaller than the end of the offset table, decrease, or exceed the frame.
- `nFields` is the number of fields of that struct as the *writer* had it.
- A struct with no present fields is a frame of `ceil(nFields/8)` zero bytes (at least 1).
- A message whose root is **not** a struct is encoded as a frame of a virtual struct with exactly one field (`V`), and the schema ends in `W`.

#### Fields: presence and payload

| Kind | Present when | Payload |
|---|---|---|
| `bool` | `true` (the bit *is* the value) | none |
| signed integer | non-zero | in `scalars`: `uvarint(zigzag(v))` |
| unsigned integer | non-zero | in `scalars`: `uvarint(v)` |
| `float32` | bit pattern non-zero | in `scalars`: 4 bytes |
| `float64` | bit pattern non-zero | in `scalars`: 8 bytes |
| `string` | length > 0 | the bytes |
| slice | length > 0 | slice payload (10.3) |
| array | always (length > 0) | slice payload with the count fixed by the type |
| map | length > 0 | map payload (10.4) |
| struct | always | a nested frame |
| pointer to struct | non-nil | the struct's frame |
| pointer to anything else | non-nil | one byte `0x01`, then the pointee's payload (so a pointer to an empty value is non-empty) |
| `interface{}` | non-nil | dynamic value (10.5) |
| marshaler type | value is not the zero value | the bytes returned by `MarshalBinary` (or `MarshalText` if only that exists) |

Zero values are **absent**: nothing is written but one bit. A decoder **MUST** set absent fields to their zero value.

### 10.3 Slice payloads

The payload of a slice or array depends on the element kind. The element count of an array is fixed by the schema; a decoder **MUST** reject a payload whose count differs.

| Element kind | Payload |
|---|---|
| `uint8`, `bool`, `float32`, `float64` | the elements back to back: 1, 1, 4, 8 bytes each. `bool` is `0` or `1` (anything else is corrupt). The payload length must be a multiple of the element size |
| other integers | `uvarint(count)`, then for each element `uvarint(zigzag(v - previous))`, with `previous = 0` for the first |
| string, slice, pointer, map, array, `interface{}`, marshaler, and structs of fewer than 4 elements | **rows**: `uvarint(count << 1)`, then `count` W-byte offsets, then the elements. Offsets are from the start of the slice payload (its first byte); element *i* spans from its offset to the next one's, the last to the payload end. A nil or empty element is a zero-length span |
| struct, 4 or more elements | **columns**: `uvarint(count << 1 \| 1)`, then one W-byte offset per struct field (the *lane directory*, from the start of the slice payload), then the lanes |

The low bit of the `uvarint` selects rows (0) or columns (1) for element kinds that allow both. A decoder **MUST** bound `count` (the reference limits columnar counts to `MaxElements`, 16,777,216, and row counts by the input length, so a few bytes cannot demand a huge allocation).

#### Columns

Lane *i* holds field *i* of every element. It spans from its directory offset to the next lane's offset (the last to the payload end). A lane of **zero bytes** means every element's field is the zero value. At least one lane **MUST** be non-empty. Every other lane starts with a one-byte **tag**:

| Tag | Name | Used for | Body |
|---|---|---|---|
| 0 | plain | bool | `count` bits, packed (below) |
| 0 | plain | float32 / float64 | `count` raw values, 4 or 8 bytes each |
| 0 | plain | string | per element `uvarint(len)` then the bytes |
| 0 | plain | struct, slice, pointer, other | an offset table of `count` W-byte offsets measured from the lane's first byte (the tag), then the payload of each element, using the field's normal payload rules |
| 1 | frame of reference | integers | `uvarint(E(min))`, one byte `width`, then `count` values `v - min` packed at `width` bits |
| 2 | delta | integers | per element `uvarint(zigzag(v - previous))`, `previous = 0` first (the reference encoder uses it only for lanes of up to 64 elements) |
| 3 | dictionary | integers, floats, strings | `uvarint(n)`, then `n` entries (integers: `uvarint(E(v))`; float32: 4 bytes; float64: 8 bytes; strings: `uvarint(len)` and bytes), one byte `width`, then `count` indexes packed at `width` bits |
| 4 | delta frame of reference | integers | `uvarint(zigzag(first))`, `uvarint(zigzag(minDelta))`, one byte `dwidth`, then the `count - 1` values `delta - minDelta` packed at `dwidth` bits; then, if `dwidth > 0` and `k = (count - 2) / 64 > 0`, one byte `cwidth` and `k` packed checkpoints, where checkpoint *t* is the sum of `delta - minDelta` over the first `64t` deltas (they allow O(1) random access) |

`E(v)` is `zigzag(v)` for signed fields and `v` for unsigned. Subtraction and addition are modulo 2^64, and "min" of signed fields is compared as a signed value. Float fields are treated as raw bit patterns and use only plain or dictionary lanes.

**Bit packing.** Value *i* occupies bits `[i*width, (i+1)*width)` of a bit string, filled from the least significant bit of byte 0 upward; the string is `ceil(n*width/8)` bytes. With `width = 0` there are no bytes and every value is the base (`min` or `minDelta`). A decoder **MUST** reject lengths that do not match.

### 10.4 Maps

A map payload is `uvarint(byte length of keys slice payload)`, then the **keys slice payload** (10.3, with the key type as element), then the **values slice payload**. Both slices have the same element count. For string, integer, float and bool keys the encoder writes entries in ascending key order (integers and floats numerically, strings bytewise, false before true), which makes the output deterministic; a decoder does not rely on the order. Struct and array keys are encoded but have no defined order.

### 10.5 Dynamic values (`interface{}`)

A dynamic value is a tag byte followed by its body. It is self-describing and recursive.

| Tag | Meaning | Body |
|---|---|---|
| 0 | nil | none |
| 1 | false | none |
| 2 | true | none |
| 3 | signed integer | `uvarint(zigzag(v))` |
| 4 | unsigned integer | `uvarint(v)` |
| 5 | float | 8 bytes (float32 is widened to float64) |
| 6 | string | `uvarint(len)`, bytes |
| 7 | bytes | `uvarint(len)`, bytes |
| 8 | list | `uvarint(count)`, then `count` dynamic values |
| 9 | map with string keys | `uvarint(count)`, then per entry `uvarint(len)`, key bytes, a dynamic value |
| 10 | map with other keys | `uvarint(count)`, then per entry a dynamic key, a dynamic value |

A decoder produces `nil`, `bool`, `int64`, `uint64`, `float64`, `string`, `[]byte`, `[]any`, `map[string]any`, `map[any]any`. Structs are written as tag 9 maps keyed by field name; a `time.Time` or other text marshaler is written as a string. Map entries are written in ascending key order (string, integer, float and bool keys). Nesting beyond 4,096 levels **MUST** be rejected, and a dynamic map key that is not comparable is corrupt.

### 10.6 Worked examples

All use the reference encoder; fingerprints depend on the Go types shown.

**A struct** `struct{A uint32; B string}{300, "hi"}`:

```
c0 | 58 2d b8 1d | 03 | ac 02 | 04 | 68 69
^marker  ^fingerprint  ^bitmap (A and B present)
                          ^scalars: A = uvarint(300)
                                 ^offsets: B starts at byte 4 of the frame (1 bitmap + 2 scalar + 1 table)
                                       ^heap: "hi"
```

The same value, evolvable (`MarshalEvolvable`): `c4 | 58 2d b8 1d | 01 02 | 03 ac 02 04 68 69`. The shape `01 02` says: 1 struct type, with 2 fields.

**Several kinds** `struct{X int32; Y []byte; Z []uint16}{-2, {1,2}, {1,2,4}}`:

```
c0 | 4c 11 b8 b6 | 07 | 03 | 04 06 | 01 02 | 03 02 02 04
                     ^all three present
                          ^X = uvarint(zigzag(-2)) = 3
                               ^Y at 4, Z at 6
                                       ^Y = raw bytes
                                              ^Z: count 3, then zigzag deltas 1,1,2 -> 02 02 04
```

**A non-struct root** `interface{}` holding `"hi"`: `c0 | 72 e6 30 f8 | 01 | 02 | 06 02 68 69` (the virtual one-field struct: bitmap `01`, offset `02`, then dynamic string tag `06`, length 2, bytes).

**A map** `map[string]int64{"a":1,"b":2}`: `c0 | 21 04 d5 e3 | 01 | 02 | 05 | 04 03 04 61 62 | 02 02 02`. After the frame's bitmap and offset comes `05` (the keys payload is 5 bytes), the keys as rows (`04` = count 2 shifted, offsets `03 04`, bytes `a` `b`), then the values as delta varints (`02` = count, deltas 1 and 1 as zigzag `02 02`).

**A columnar slice** of four `struct{TS int64; Host string}` with `TS` = 1000..1003 and `Host` = `"web"`:

```
c0 | bc 42 53 4a | 01 | 02 | 09 | 03 08 | 01 d0 0f 02 e4 | 00 03 77 65 62 03 77 65 62 03 77 65 62 03 77 65 62
                               ^uvarint(4<<1|1): columns
                                  ^lane directory: lane 0 at 3, lane 1 at 8
                                              ^TS lane: tag 1 (frame of reference), min = zigzag(1000) = d0 0f,
                                               width 2, values 0,1,2,3 packed = e4
                                                         ^Host lane: tag 0 (plain), each "web" as len + bytes
```

### 10.7 Stream protocol

A stream is a sequence of messages, each prefixed with its length:

```
stream = { uvarint(length) message }
```

`length` is the byte length of the message (header and frame). A reader reaches a clean end of stream only between messages; a truncated length or message is an error (`io.ErrUnexpectedEOF`). A reader **MUST** reject lengths above its configured maximum (`MaxMessageSize`, 256 MiB by default). Messages in one stream may be plain or evolvable independently.

### 10.8 Text notation

#### Grammar

```
document   = { line }
line       = blank | comment | entry
terminator = LF | ";"                    ";" outside double quotes also ends a line
comment    = a physical line whose first non-blank character is "#"
blank      = { SP | TAB | CR }
entry      = path [ 1*blank value ]
path       = name { "." name }
name       = quoted | bare
bare       = 1*( any byte except blank, ".", LF, ";" )      a '"' inside a bare name is an ordinary character
quoted     = '"' { any byte except '"' and '\' | escape } '"'    a name starting with '"' is quoted
escape     = "\n" | "\r" | "\t" | "\0" | "\\" | "\"" | "\'" | "\/"
           | "\x" 2*hex | "\u" 4*hex | "\U" 8*hex
value      = rest of the entry's line, trimmed of blanks
```

Quoting is recognized only at the **start of a name** (the beginning of the path or right after a `.`) and at the **start of a word** in a value. A `;` inside a quoted string does not end the line. Indentation and tabs carry no meaning. Each entry is independent; later entries override earlier ones.

#### Values by target type

The value text is interpreted according to the type it is assigned to:

| Target | Value text |
|---|---|
| any type | empty or `~` = the zero value (for pointers, interfaces: null) |
| `string`, text-marshaled types | the whole rest of the line: either one quoted string, or a bare run (which may contain spaces) |
| integers | decimal (a leading zero is still decimal: `010` is ten), or `0x` / `0b` / `0o` prefixed, with an optional sign; out-of-range is an error |
| floats | Go float syntax, `nan`, `inf`, `+inf`, `-inf` |
| `bool` | `true` or `false` |
| `[]byte`, `[N]byte`, binary-marshaled types | `0x` and hex digits |
| list of simple values | words separated by blanks; a word is a quoted string or a run of non-blank characters; `~` as a whole word is a nil element |
| `interface{}` | one word: `~` nil, `true`/`false`, a number (`int64`, or `uint64` above `MaxInt64`, or `float64` if it has `.` or an exponent), `0x..` bytes, `~list`, `~map`, a quoted string, or any other word as a string; several words make a `[]any` |
| `~list`, `~map` | an empty, non-nil list or map (a zeroed array); used for `interface{}` values and non-nil pointers to collections |

#### Path semantics

A path is resolved from the root value. Each name is applied to the value reached so far:

| Current value | Name | Effect |
|---|---|---|
| struct | field name (ignoring case), or its `codec:"name"` tag | selects the field; an unknown name is an error that lists the valid ones |
| map | any name (quoted if special) | the key, parsed as the key type; the entry is created if absent |
| list | `+` | appends a new zero element and selects it |
| list | `^` | selects the last element (an error if empty) |
| list | a decimal number | selects that element; the next free number appends; numbers must not exceed the length |
| list | any other name | selects the **last** element, creating the first one if the list is empty (so a one-item list needs no `+`) |
| array | a decimal number | selects that position; `+` is an error |
| pointer | any | allocated if nil, then followed |
| `interface{}` | any | builds `map[string]any` for names and `[]any` for `+`, `^` and numbers; a quoted number is a key |

The names `+` and `^` and all-digit names are reserved for list use and **MUST** be quoted when used as map keys or field names.

#### Canonical printing

A printer **SHOULD** produce this form; the reference printer does:

- struct fields in declaration order, **omitting zero values** (nil pointers, empty collections, `0`, `""`, `false`, and structs/arrays that are entirely zero); map entries in ascending key order (integers numerically, others bytewise);
- one `path value` line per leaf; a list of simple values on one line when it has elements and no nil elements that would make it ambiguous; otherwise `path.+` per element, followed by the element's fields as `path.field value`; a list of lists uses `path.^.+` for the inner level; arrays use positions and omit zero elements;
- a string is written **bare** unless it is empty, starts with a blank, `"`, `#` or `~`, ends with a blank, or contains a control character, `;`, or invalid UTF-8; in a list of words it must also contain no blank; in an `interface{}` context it must also not start with `-` and must not look like a number, bool or hex literal; in a **name** it must also contain no `.` or `"`, not be `+` or `^`, and not be all digits. Otherwise it is written quoted;
- an item that is itself zero is `path.+` alone, or `path.+ ~` for a nil pointer; a present value that would print nothing (a non-nil pointer to a zero struct, an empty collection behind a pointer, an all-zero array) is written with an explicit marker (one zero field, `~list` or `~map`) so it keeps its identity.

#### Errors

A parser **MUST** report the line and column of the first error and **MUST NOT** crash on any input. Resource use is bounded by input size, and nesting by 4,096 levels.

### 10.9 Base64 armor

The binary message encoded with the URL-safe base64 alphabet of RFC 4648 section 5, without padding. A decoder **SHOULD** also accept the standard alphabet (`+`, `/`) and `=` padding, and **MUST** ignore surrounding whitespace.

### 10.10 Media types and content negotiation

| Format | Media type | Notes |
|---|---|---|
| binary | `application/x-strata` | alias `application/strata` accepted |
| text | `text/x-strata; charset=utf-8` | alias `text/strata` accepted; UTF-8 only |
| base64 | `application/x-strata+base64` | |

**Choosing a response format** from an `Accept` value (`Negotiate`): split on commas; for each entry read its media type and `q` (default 1); skip `q <= 0`; map the Strata types to their formats, `text/plain` to text, and `*/*`, `text/*`, `application/*` to the caller's fallback; take the entry with the highest `q` (the first listed wins ties). An empty header, or one with no usable entry, selects the fallback.

**Identifying a request body** (`Detect`): an exact Strata media type decides. These generic labels are sniffed: empty, `text/plain`, `application/octet-stream` and `application/x-www-form-urlencoded` (what `curl -d` sends). A body that consists only of base64 characters (no blanks, no newlines inside) is base64, and if it then fails to decode it is read as text; anything else is text. `application/octet-stream` means binary. Any other label is an error.

**Suggested HTTP profile** (what `examples/http` implements; not required): the response format is, in order, an explicit `?format=binary|text|base64`, the negotiated `Accept`, the request's own format, then text for requests that named no Strata type; the response carries `Content-Type` for the chosen format; errors are returned as a value of the same format with a status code (400 for a body that cannot be read, 404 for a missing key, 204 for a delete).

### 10.11 Store file formats and protocols

A store is a directory:

```
MANIFEST         ordered list of live segments (atomically replaced)
NNNNNNNNNN.seg   segment files, named by 10-digit zero-padded id, append-only
INDEX            optional checkpoint of the key index
```

#### Segment and record

A v1 segment starts with `STRATASG\x01`; a v2 segment starts with
`STRATASG\x02`. Both have a 9-byte magic. V1 records are:

```
record = crc32c(4) klen(4) vflags(4) key value        all integers little-endian
```

- `vflags` bit 31 = **delete** (a tombstone; the value is empty), bit 30 = **more** (the record belongs to a batch that continues in the next record); the low 30 bits are the value length (at most 2^30 - 1 bytes). `klen` is at most 65,536.
- `crc32c` is CRC-32C (Castagnoli) over `klen`, `vflags`, `key` and `value`.
- **Commit rule.** A record without `more` commits itself and every directly preceding record with `more`. A trailing run of `more` records with no closing record is **not committed** and is discarded: this is what makes a batch atomic. A batch is written with one write call and lives in one segment.
- The value is a codec message (plain or evolvable) of the store's value type.

V2 records add an `expires-at(8)` little-endian signed Unix-nanosecond timestamp
between `vflags` and `key`. Zero means no expiry. The CRC covers `klen`,
`vflags`, `expires-at`, `key` and `value`. New databases use v2; opening a v1
database keeps its old segments readable and rotates to a v2 active segment.
V2 checkpoints use `STRATAIX\x02` and append `uvarint(expires-at)` to each
entry; v1 checkpoints remain supported.

#### Manifest

```
manifest = "STRATAMF\x01" count(4) { segment-id(4) } next-id(4) crc32c(4)
```

All integers little-endian; the CRC covers everything before it. It lists the live segments oldest first (the last is the active one) and the next unused id. It is replaced atomically: write `MANIFEST.tmp`, fsync, rename over `MANIFEST`, then sync the directory on Unix. Non-Unix targets use the atomic rename but do not have a portable directory-flush operation in Go, so sudden-power-loss durability of directory metadata depends on the platform and filesystem.

#### Checkpoint

```
index-v1 = "STRATAIX\x01" uvarint(nSegments) { uvarint(id) uvarint(size) uvarint(dead) }
           uvarint(nEntries) { uvarint(klen) key uvarint(segment-id) uvarint(value-offset) uvarint(value-length) }
index-v2 = "STRATAIX\x02" uvarint(nSegments) { uvarint(id) uvarint(size) uvarint(dead) }
           uvarint(nEntries) { uvarint(klen) key uvarint(segment-id) uvarint(value-offset) uvarint(value-length) uvarint(expires-at) }
        crc32c(4)
```

Entries are in key order. The checkpoint is valid only if its CRC matches, every listed segment is still in the manifest, and every file is at least as long as recorded; otherwise it **MUST** be ignored and the log replayed in full. After loading a valid checkpoint, each segment is replayed from its recorded size.

#### Recovery

1. Read the manifest; delete any `*.seg` file it does not list and any `*.tmp` file.
2. Load the checkpoint if valid; otherwise start with an empty index.
3. Replay committed records of each segment (in manifest order) from the start, or from the checkpoint's size, applying puts and deletes in order. Verify every CRC.
4. A torn or corrupt tail in the **last** segment is truncated to the last commit point. Damage anywhere else is an error (`ErrCorrupt`).

#### Rotation and compaction

- When appending would pass `SegmentSize`, fsync the active segment, create the next one, rewrite the manifest, and continue there.
- **Compaction** merges a run of at most 4 adjacent immutable segments: copy the records that are still the current version of their key (checking the index) into new segments, fsync them, atomically replace the run in the manifest with the new segments, re-point the index (skipping keys that changed meanwhile), then delete the old files. A crash at any step leaves a consistent store (unlisted files are removed on the next open).
- **Tombstones** are copied unless the run includes the oldest segment, because an older segment outside the run may still hold a value the tombstone has to keep dead.

#### Durability

Without `SyncAlways`, a process crash loses nothing that was written (the OS holds it), but a power failure can lose the most recent writes unless `Sync`, `SyncInterval`, or a successful `Close` synced them. With `SyncAlways`, every write is synced to durable storage before it returns. Unix directory renames are synced; other targets rely on platform filesystem behavior for rename metadata durability.

### 10.12 Pattern language

Used by `Scan`, `Match`, `Count` and `DeleteMatch`, and matched against the whole key as UTF-8:

```
pattern = { item }
item    = "*"                 any run of characters, including none
        | "?"                 exactly one character
        | "[" [ "^" | "!" ] class-item { class-item } "]"
        | "\" any             that character literally
        | any other character  itself
class-item = char | char "-" char           a range; "\" escapes inside a class
```

The **literal prefix** (everything before the first `*`, `?` or `[`, with escapes resolved) is used to restrict the scan to keys under it. Matching runs in at most O(len(key) x len(pattern)) time. A malformed pattern (unclosed `[`, trailing `\`, a backwards range, an empty class) is an error.

### 10.13 Versioning and compatibility rules

- The marker byte (10.2) and store magics carry the format version. Segment and checkpoint v1 and v2 are supported; unknown values are rejected (`ErrBadHeader`, `ErrCorrupt`).
- **Plain** messages are readable only by a schema with an identical fingerprint.
- **Evolvable** messages are readable by any schema whose structs are the writer's with fields **appended** (10.2). Anything else is `ErrSchemaMismatch`.
- A repeated struct type is referred to by position in the fingerprint, so renaming a Go type does not change it.
- The text notation is name-based, so it is independent of field order and tolerant of added fields on either side (an unknown field in the text is an error on purpose).

### 10.14 Conformance and security considerations

A conforming **decoder** MUST:

- reject malformed input with an error and never read out of bounds or panic: invalid header, unknown tags, offsets outside the frame or out of order, counts that exceed the input, bools other than 0/1, packed lengths that do not match, all-empty column directories, nesting beyond the limit;
- bound allocation by the input size and by configured limits, so that a few bytes cannot request gigabytes (the reference: `MaxElements`, `MaxAlloc`, `MaxMessageSize`, a cap on cached evolved plans);
- treat decoded strings and bytes as data, never as code or paths.

A conforming **encoder** MUST:

- omit zero values and set presence bits exactly as in 10.2;
- write map entries with string, integer, float or bool keys in ascending key order (so output is deterministic for equal data);
- reject (not recurse forever on) cyclic data;
- choose an offset width in which every offset fits.

Because the offset width is chosen per message, **two encodings of equal data may differ in bytes** (and in the header width code): compare decoded values, not bytes. Lower `MaxAlloc` and `MaxMessageSize` when decoding untrusted input.

---

## 11. Performance

Measured on an Apple M2 Pro, Go 1.27, 500 ms per benchmark. "doc" is one small record with nested objects and lists; "batch1000" is 1,000 telemetry points; "dynamic" is untyped `map[string]any` data.

### Speed (ns/op, allocations in brackets; lower is better)

| | Codec | Protobuf | CBOR | MsgPack (shamaton) | MsgPack (vmihailenco) | JSON |
|---|---|---|---|---|---|---|
| Encode doc | **363** (0) | 477 (0) | 570 (1) | 1,100 (17) | 1,305 (6) | 1,673 (1) |
| Encode batch1000 | **46,220** (0) | 57,610 (0) | 57,380 (1) | 101,821 (7) | 128,375 (12) | 169,190 (1) |
| Encode dynamic | **1,020** (0) | n/a | 1,572 (1) | 1,998 (22) | 3,810 (88) | 5,572 (61) |
| Decode doc | **432** (1) | 761 (25) | 1,595 (10) | 1,030 (22) | 2,171 (15) | 3,020 (3) |
| Decode batch1000 | **13,402** (1) | 85,939 (2,012) | 186,892 (1,001) | 97,765 (1,004) | 214,709 (1,003) | 331,384 (500) |
| Decode dynamic | **1,221** (8) | n/a | 4,213 (72) | 2,393 (81) | 5,790 (147) | 8,109 (120) |

With `UnmarshalNoCopy`, decoding the doc takes 396 ns with 0 allocations, and the batch takes 13,081 ns with 0 allocations.

### Encoded size

| | Codec | Protobuf | CBOR | MsgPack | JSON |
|---|---|---|---|---|---|
| doc | **199 B** | 238 B | 357 B | 359-415 B | 481 B |
| batch1000 | **1,604 B** | 28,083 B | 46,422 B | 46-49 KB | 58,885 B |
| dynamic | **639 B** | n/a | 651 B | 649-914 B | 838 B |

### Other layers

| | |
|---|---|
| Text format (100 orders, 24.8 KB) | encode 77 µs, decode 148 µs (JSON: 91 µs and 150 µs); 5 and 1 allocations (JSON: about 400 and 700) |
| Evolvable messages | same speed as plain; reading older data about 1.3× the baseline |
| Store | Put 1.6 µs (0 allocations), Get 0.5 µs, Update 2.6 µs |
| Store scans, 1,000,000 keys | literal prefix about 1 µs; `user:00?2:field3` about 30 µs; leading wildcard (`*:field7`) about 65 ms |
| Store reopen | 388 MB in 23 ms from the checkpoint (143 ms replaying the log) |

Run the comparison yourself:

```
cd benchmarks && go test -run xxx -bench . -benchmem      # speed, vs Protobuf, CBOR, MsgPack, JSON
cd benchmarks && go test -run TestSizes -v                # encoded sizes
go test -run xxx -bench . -benchmem ./store               # store benchmarks
```

Numbers vary by machine and data shape (about ±10% run to run); the workloads are in `benchmarks/types.go`.

---

## 12. Safety and limits

Malformed or hostile input returns an error and never panics. The codec is fuzzed (see [Testing](#14-testing)).

| Limit | Value | Notes |
|---|---|---|
| Nesting depth | 4,096 levels | `ErrTooDeep`. Also stops cyclic data on encode, failing fast even for a cycle that fans out. |
| `codec.MaxElements` | 16,777,216 | Bounds the element count of a columnar slice (a tiny hostile message can describe very many elements). |
| `codec.MaxAlloc` | 256 MiB | Bounds what one decode may allocate for slices and pointees; exceeding it gives `ErrTooLarge`. **Lower it when decoding untrusted input.** |
| `codec.MaxMessageSize` | 256 MiB | Largest frame a `Decoder` will read. |
| Evolved plans cached | 256 | Per process, so hostile headers cannot grow memory. |
| Store key / value | 64 KiB / 1 GiB | `store.ErrTooLarge`. |
| Store index | all keys in RAM | 24 bytes of location metadata per key, plus key text and radix-tree nodes. |

Other things to know:

- The Go decoder limits are package-wide settings. Lower them before starting concurrent decodes; do not mutate them while codec calls are active. TypeScript limits can be set per decode call.
- Decoded strings share memory blocks (a retained string keeps its block alive). With `UnmarshalNoCopy`, they point into your input buffer.
- `Unmarshal` into an existing variable reuses its slices and maps; copy first if you hold earlier results.
- Cyclic data, channels, functions, complex numbers and non-empty interfaces are not encodable.
- The store does not lock its directory against other processes.

---

## 13. Examples

Each is runnable with `go run ./examples/<name>`.

| Example | Shows |
|---|---|
| `datatypes` | **A tour of every supported data type and structure**, each round-tripped through all four formats, with its own README. |
| `basic` | Marshal and Unmarshal a struct; size against JSON. |
| `jsonlike` | Using the codec the way you would use `encoding/json`: any Go value, no schema, streams. |
| `allocs` | Where allocations go: `Append` reuses your buffer, `Unmarshal` returns independent values, `View` reads without allocating. |
| `view` | Reading single fields from an encoded message without decoding it. |
| `stream` | `Encoder` and `Decoder` over a pipe. |
| `file` | Saving data to disk: one value per file, and an append-only log of records. |
| `telemetry` | Columnar encoding: slices of structs shrink dramatically. |
| `evolve` | Changing a struct after data has been written, in memory and in the store. |
| `store` | The persistent store: update, atomic batch, wildcard queries, text forms. |
| `http` | The codec over HTTP in three formats, with a store behind it; works with `curl`. |

(`basic`, `jsonlike` and `telemetry` import `encoding/json` only to compare sizes with JSON; the library does not.)

---

## 14. Testing

The provider-neutral release checks are `./scripts/validate.sh` and
`./scripts/benchmarks.sh`. The validation script runs the Go suite, race
detector, vet, Go builds for Linux/macOS/Windows on amd64 and arm64, then the
TypeScript tests and an npm package dry run. Bounded fuzzing is opt-in with
`RUN_FUZZ=1` and should only run inside an isolated, resource-limited sandbox. It requires Go
1.27.3+, Node.js 22+, npm, and a network connection or populated npm cache for
`npm ci`. Set `FUZZTIME` to change each fuzz target's time budget. The
benchmark script prints allocations and operation timings; compare repeated
runs on the same machine and investigate changes over 10% before accepting a
performance change. It prints a median-based Codec-versus-format ranking for
each codec workload and runs the Badger/bbolt store comparisons; the captured
comparison and workload caveats are in
[`benchmarks/COMPARISON.md`](benchmarks/COMPARISON.md). No application-specific
SLA is implied by these synthetic baselines.

```
go test ./...                       # everything
go test -race ./...                 # with the race detector
go test -short ./...                # skips the largest tests
```

- **Property test over random shapes.** `TestZooAnyShape` generates random types (structs, slices, arrays, maps, pointers, `interface{}`, nested to depth 5-6) with random values, and checks that binary, evolvable binary and text all round-trip, that printed text becomes stable, and that binary → text → binary preserves the data. Control it with `CODEC_ZOO_SEED`, `CODEC_ZOO_N` and `CODEC_ZOO_DEPTH`.
- **Hostile structures.** `nasty_test.go` covers cyclic data (including fan-out cycles), self-referential types, linked lists hundreds of nodes long, and very wide data (a million ints, 100,000 map entries, a 10 MB string, 50,000 rows).
- **Crash recovery.** The store tests chop and corrupt segment files, cut atomic batches in half, run compaction against concurrent writers, and check that a deleted key never reappears.
- **Fuzzing.** `FuzzUnmarshal`, `FuzzUnmarshalNoCopy`, `FuzzColumnar`, `FuzzMixed`, `FuzzText`, `FuzzEvolve`:

  ```
  go test -run xxx -fuzz '^FuzzText$' -fuzztime 30s .
  ```

- **Specification examples.** `TestSpecWorkedExamples` regenerates every byte example in section 10 and recomputes its fingerprint independently, so the documented format cannot drift from the encoder.
- **Dependency rule.** `TestNoForbiddenImports` fails if the core package or the store ever imports `encoding/json`, `net/http` or `encoding/xml`.

---

## 15. FAQ

**Do I need a schema file or code generation?** No. The codec reads your Go types with reflection once, builds a plan with field offsets, and runs from that plan afterwards.

**Why is the binary form positional?** It is what makes it small and fast: no field names in the data. The cost is that readers need the same type, or a later version of it with fields appended (use `MarshalEvolvable` for data that outlives a version).

**When should I use text and when binary?** Binary for programs talking to programs, storage and anything performance- or size-sensitive. Text for configuration, debugging, `curl`, logs and anything a person reads or writes. They carry exactly the same data and convert both ways.

**Is it compatible with other languages?** No. It is a Go codec. If you need cross-language interchange, Protobuf is the better choice.

**How do I handle untrusted input?** Lower `codec.MaxAlloc`, keep `codec.MaxMessageSize` and the store's value limits in mind, and use the error returns: malformed input never panics.

**Can I rename or remove a field?** Not in evolvable messages: only appending is supported. Keep the old field and stop using it. Renaming a struct *type* is fine.

**Why are two encodings of equal data sometimes different bytes?** Offsets are sized adaptively to the message. Compare decoded values.

**Can two processes use one store directory?** No. A `DB` is safe for concurrent use inside one process, but the directory is not locked against other processes.
