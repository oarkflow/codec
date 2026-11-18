# datatypes: every data type and structure the codec supports

This example is a guided tour. It builds one or more values of every supported kind, pushes each through **binary, evolvable binary, text and base64**, and checks the result against the original. It exits non-zero if any case fails, so it also works as a conformance check.

```
go run ./examples/datatypes
```

Each case prints one summary line and the value's text form:

```
OK   map[string]Point                   binary   25 B   binary✓ evolvable✓ text✓ base64✓
       | origin ~
       | p.X 3
       | p.Y 4
```

`OK` means all four round trips matched. `binary 25 B` is the size of the binary message. The lines after `|` are the value in the [text notation](../../README.md#6-text-notation).

The tour has 15 numbered sections (plus 7b); the table below is the reference for what each type becomes.

## Supported types at a glance

| Go type | Binary | Text | Notes |
|---|---|---|---|
| `bool` | the bit in the struct's bitmap *is* the value | `true`, `false` | `false` is zero, so it is omitted |
| `int8` `int16` `int32` `int64` `int` | zigzag varint (small numbers are small) | decimal (`0x2A` also accepted) | `int` is 64-bit |
| `uint8` `uint16` `uint32` `uint64` `uint` | varint | decimal | `uintptr` is **not** supported |
| `float32` | 4 raw bytes | shortest exact decimal | `nan`, `inf`, `-inf` and `-0` round-trip |
| `float64` | 8 raw bytes | shortest exact decimal | |
| `string` | the bytes, no length prefix of its own | bare when unambiguous, otherwise `"quoted"` | Any bytes are fine, including invalid UTF-8 (text shows `\xHH`) |
| `[]byte`, `[N]byte` | raw bytes | hex, `0x00ff10` | `[16]byte` makes a compact UUID |
| `struct` | a *frame*: bitmap, scalars, offsets, payloads | one `Field value` line per non-zero field; nested as `Parent.Field` | Exported fields, in declaration order |
| `[N]T` array | like a slice, length fixed by the type | item numbers: `0 a`, `1 b` | Zero elements are omitted in text |
| `[]int` (any integer) | count, then delta varints | `+ 1000` per item at top level, or `Field 1 2 3` on one line | |
| `[]uint8` `[]bool` `[]float32` `[]float64` | raw elements, back to back | `0x..` for bytes; words for the others | |
| `[]string` | count, offset table, strings | `Tags a b "two words"` | |
| `[]struct` | rows, or **columns** when 4+ elements | `Field.+` starts each item (`Items.+`, `Items.SKU x`) | Columns pick bit-packing, delta or a dictionary per field |
| `[][]T` list of lists | rows of slice payloads | `Matrix.+ 1 2 3`, one `+` line per row | Any depth |
| `map[K]V` | key slice then value slice, **sorted by key** | `Field.key value`, the key is part of the path | Keys: strings, integers, bools, text-marshalable. Struct and array keys work in binary only |
| `*T` | nil is one bit; a non-nil pointer to a struct is just the struct's frame; to anything else a marker byte plus the value | omitted when nil, `~` for an explicit null | A pointer to a zero value is preserved |
| `interface{}` | one tag byte per value (see below) | self-describing words | Decodes to generic types |
| `time.Time` | through `MarshalBinary` | RFC 3339 string | Any type with `MarshalBinary`/`MarshalText` works |
| `net/netip` types, `*big.Int` | through their marshalers | their text form | |
| named types (`type Celsius float64`, `type Status uint8`, `type Names []string`) | as their underlying kind | as their underlying kind | Enums, units, typed aliases all work |

### Not supported

| Go type | What happens |
|---|---|
| `chan`, `func` | `codec: unsupported type chan int` |
| `complex64`, `complex128` | `codec: unsupported type complex128` |
| `uintptr`, `unsafe.Pointer` | `codec: unsupported type uintptr` |
| interfaces other than `interface{}` | `unsupported interface type fmt.Stringer` |
| cyclic data (a pointer chain that loops) | `codec: nesting too deep` (never a crash) |
| struct or array map keys, in **text** only | `unsupported value: map key main.Point` (binary accepts them) |

## Reading the example, section by section

### 1-3. Scalars, strings, bytes

Integers use zigzag varints, so `-42` costs one byte of payload while `math.MinInt64` costs ten. A zero value of any kind costs **one bit** and is omitted entirely, which is why `0`, `""` and `false` print as nothing inside a struct.

Floats are stored raw; the text form prints the shortest decimal that parses back to the identical bits, so `math.Pi`, `5e-324`, `1.7976931348623157e+308`, `inf`, `nan` and `-0` all survive.

Strings keep their exact bytes. In text a string is written bare when that is unambiguous and quoted otherwise: `"line1\nline2"`, `"  padded  "`, `""`. A string that merely *looks* like another type (`"true"`) is safe as a field or list value because the field's type decides how it is read.

### 4. Structs

```go
type Person struct {
	Name    string
	Home    Address            // nested struct
	Work    *Address           // pointer to struct
	Tags    []string
	Scores  map[string]float64
	Friends []Person           // a struct containing slices of itself
}
```

becomes (this is the example's real output, trimmed to the top-level shape):

```
Name Ada Lovelace
Age 36
Active true
Score 99.5
Home.Street 12 Main St
Home.City London
Home.Zip 12345
Work.Street 1 Analytical Way
Work.City Cambridge
Work.Zip 54321
Tags admin dev "two words"
Scores.math 100
Scores.poetry 87.5
Friends.+
Friends.Name Charles
Friends.Age 40
Friends.Tags engine
Friends.+
Friends.Name Mary
Friends.Age 38
Friends.Friends.+
Friends.Friends.Name Augusta
Friends.Friends.Age 5
```

Read `Friends.+` as "the next friend", and `Friends.Friends.+` as "the next friend of the friend just started". There is nothing to indent or count.

Nesting has no depth limit short of 4,096 levels.

### 5-6. Arrays and slices

The binary layout adapts to the element type:

| Element | Layout |
|---|---|
| `uint8`, `bool`, `float32`, `float64` | raw, back to back |
| other integers | delta varints: `[1000, 1001, 1003, 1006]` stores `1000, 1, 2, 3` |
| strings, slices, pointers | an offset table, then each element |
| structs, fewer than 4 | rows: an offset table of frames |
| structs, 4 or more | **columns**: one lane per field |

Columnar storage is why batches are small. The example prints both ends:

```
[]Point (1000): columnar packing   19 B for 1000 points (raw memory: 8000 B): perfectly regular data
[]Point (1000): columnar packing   3770 B for 1000 points (raw memory: 8000 B): irregular data, still bit-packed
```

`X=i, Y=2i` is perfectly regular, so delta coding stores it in a few bytes. The irregular case still beats the raw 8 bytes per point because each lane is bit-packed to the width its values need. Lanes also pick a dictionary for repetitive strings and values.

### 7. Maps

Map output is sorted by key, so the same map always produces the same bytes. In text each entry's key is part of the path (`one 1`, `fruit apple pear`, `a.x 1`), and a key that could be confused with something else is quoted: numbers (`"12"`, so they are not read as list positions), strings with spaces or dots (`"dot.ted"`, `"has space"`), the empty string (`""`).

Section **7b** shows the binary-only case: struct and array keys.

### 8. Pointers

A nil pointer is one bit and prints nothing. A pointer to a zero value is **kept distinct** from nil: `*int` pointing to `0` round-trips as non-nil, in every format.

### 9. interface{}

An `interface{}` value carries a one-byte tag, so it needs no schema:

| Tag | Go value | Decodes as |
|---|---|---|
| nil | `nil` | `nil` |
| false / true | `bool` | `bool` |
| int | any signed integer | `int64` |
| uint | any unsigned integer | `uint64` |
| float | `float32`, `float64` | `float64` |
| string | `string` | `string` |
| bytes | `[]byte`, `[N]byte` | `[]byte` |
| list | slices, arrays | `[]any` |
| map | maps with string keys, and structs | `map[string]any` |
| any-map | maps with other keys | `map[any]any` |

The example prints the consequences: `int32(5)` comes back as `int64`, `float32(1.5)` as `float64`, a `time.Time` as its RFC 3339 string, and a struct as `map[City:c Street:s Zip:9]`. To decode into an `interface{}`, marshal an `interface{}` variable (`var v any = ...; codec.Marshal(&v)`).

In text, an `interface{}` is read from its words: `36` is `int64`, `3.5` is `float64`, `true` is a bool, `~` is nil, `0x01ff` is bytes, a quoted or any other word is a string, and a path (`list.+ 1`, `map.key value`) builds lists and maps. Quote a string that would read as one of those (`"36"`).

### 10. Library types and your own types

Any type with `MarshalBinary`/`UnmarshalBinary`, or `MarshalText`/`UnmarshalText`, is encoded through that pair; if it has both, binary uses the binary pair and text uses the text pair.

```go
type Color struct{ R, G, B uint8 }

func (c Color) MarshalText() ([]byte, error) { return []byte(fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)), nil }
func (c *Color) UnmarshalText(b []byte) error { _, err := fmt.Sscanf(string(b), "#%02x%02x%02x", &c.R, &c.G, &c.B); return err }
```

`Color` now prints as `"#ff8800"`, and `[]Color` as a list of such strings. `Money` in the example uses `MarshalBinary` instead, so its text form is hex bytes (`0x5553440001e240`). Pointer receivers are fine, as in the example.

Named types follow their underlying kind: `type Celsius float64`, `type Status uint8`, `type Names []string` and `type Registry map[string]Names` need nothing at all.

### 11. Struct features

```go
type Article struct {
	Base                          // embedded: a field named Base (text: Base.ID, Base.Created)
	Title    string
	secret   string               // unexported: never encoded
	Draft    string `codec:"-"`   // excluded from every format
	Headline string `codec:"headline"` // renamed in text and in interface{} values
}
```

- Unexported fields and fields tagged `codec:"-"` are skipped.
- `codec:"name"` changes the field's name in the **text** form and in `interface{}` values. The binary layout is positional, so it is not affected.
- Embedded structs are encoded as a field named after the type; there is no field promotion.

### 12. Recursive types

Linked lists (`Next *Node`) and trees (`Kids []Tree`) work to a depth of 4,096:

```
Value 1
Next.Value 2
Next.Next.Value 3
```

### 13. Zero, null and empty

- Every zero value is omitted: an all-zero struct is a few bytes in binary and **empty text**.
- A nil slice and an empty slice encode identically (and decode as the same thing); likewise for maps.
- A pointer to a zero value is *not* the same as nil (section 8), and `~` in text is an explicit null.

### 14. What is refused

Unsupported types give a clear error at encode time, not a crash. Cyclic data is the case worth knowing: the encoder tracks nesting and returns `ErrTooDeep` instead of overflowing the stack, even for a structure that fans out.

### 15. Everything at once

One value containing structs, matrices, maps of slices, an `interface{}` tree, a `time.Time`, an IP address, bytes, a pointer and 100 points, in all four formats:

```
binary 504 B, evolvable 509 B, text 4655 B, base64 672 B
```

- **binary** is the compact form for programs and storage.
- **evolvable** adds 5 bytes so later versions of the types can read it (see [schema evolution](../../README.md#5-schema-evolution) and `examples/evolve`).
- **text** is the readable form, bigger because it spells out every path.
- **base64** is the binary message as one opaque token.

## Choosing a format

| You want | Use |
|---|---|
| Smallest and fastest | binary (`Marshal`) |
| Data that outlives a program version | evolvable binary (`MarshalEvolvable`) |
| People to read or write it, `curl`, config, debugging | text (`MarshalText`) |
| One token in a URL, header or environment variable | base64 (`MarshalBase64`) |

All four carry exactly the same data and convert in both directions (`ToText`, `FromText`).

## Related examples

- `examples/basic`, `examples/jsonlike`: the simplest use.
- `examples/view`: reading one field without decoding.
- `examples/telemetry`: columnar storage on realistic data.
- `examples/evolve`: changing a type after data is written.
- `examples/store`, `examples/http`: persistence and transport.
