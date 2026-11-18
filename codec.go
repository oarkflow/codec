// Package codec implements Strata, a compact binary codec whose Marshal and
// Unmarshal work like encoding/json: they accept any Go value (structs,
// slices, arrays, maps, pointers, scalars, interface{}, time.Time and any type
// with MarshalBinary/UnmarshalBinary or MarshalText/UnmarshalText) and need
// no schema file and no code generation.
//
//	data, err := codec.Marshal(v)
//	err = codec.Unmarshal(data, &v)
//	codec.NewEncoder(w).Encode(v); codec.NewDecoder(r).Decode(&v)
//
// Differences from encoding/json, all consequences of being a binary format
// with a positional schema:
//   - Fields are matched by position, not name, so Unmarshal needs the same
//     Go type that was marshaled (a 32-bit schema fingerprint is checked and a
//     mismatch returns ErrSchemaMismatch). Unmarshaling into interface{}
//     therefore needs data marshaled from an interface{} (marshal &anyValue).
//   - interface{} values are self-describing and decode to generic types:
//     nil, bool, int64, uint64, float64, string, []byte, []any, map[string]any
//     and map[any]any. A struct inside an interface{} becomes a map.
//   - Zero values are omitted (one bit each), so there is no omitempty.
//     Unexported fields and fields tagged `codec:"-"` or `json:"-"` are skipped.
//   - Marshal sorts map output by key, so it is deterministic. A nil map, nil
//     slice and nil pointer-free zero value decode as empty/zero.
//   - Marshal(nil pointer) encodes the zero value of the pointee.
//   - Unmarshal copies strings and bytes from the input. Existing pointers,
//     slice storage and non-nil maps are reused; copy them before
//     retaining an earlier decoded snapshot. Use View for zero-copy field access.
//
// Under the hood, on first use of a Go type the codec builds an immutable
// plan (field offsets, kinds, a schema fingerprint) from reflection and
// caches it. After that, Marshal/Unmarshal run on raw field offsets with no
// per-call reflection for structs, slices and arrays (maps, interface{} and
// marshaler types go through reflection).
//
// Wire format (all integers little-endian):
//
//	message = header frame
//	header  = byte(0xC0 | widthCode) fingerprint32        (5 bytes)
//	          widthCode: 0 => 1-byte offsets, 1 => 2-byte, 2 => 4-byte
//	frame   = bitmap scalars offsets heap
//	bitmap  = max(1, ceil(nFields/8)) bytes; bit i set => field i non-zero
//	scalars = for each present scalar field, in field order:
//	            bool    : nothing (the bit is the value)
//	            int*    : zigzag uvarint
//	            uint*   : uvarint
//	            float32 : 4 raw bytes, float64: 8 raw bytes
//	offsets = one W-byte offset (relative to frame start) per present
//	          variable field (string, slice, struct, *struct)
//	heap    = the variable fields' payloads; a payload ends where the next
//	          begins, the last ends at the frame end (no length prefixes)
//
// Slice payloads:
//
//	[]uint8/[]bool/[]float32/[]float64 : raw elements
//	other integer slices               : uvarint count, zigzag-delta varints
//	string/struct/slice/pointer elems  : uvarint(count<<1 | columnar), then
//	  rows (columnar=0): W-byte offsets (relative to payload start), elements
//	  columns (columnar=1, struct elems, count >= 4): one W-byte offset per
//	    struct field, then one lane per field. An empty lane means all zero;
//	    otherwise a tag byte selects: plain, frame-of-reference bit-packing,
//	    delta varints, or dictionary (see columnar_enc.go).
//
// W is chosen per message as the narrowest width that can address the body.
package codec

import (
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

var (
	ErrNotStruct      = errors.New("codec: unsupported value")
	ErrNeedPointer    = errors.New("codec: Unmarshal needs a non-nil pointer")
	ErrBadHeader      = errors.New("codec: bad header")
	ErrSchemaMismatch = errors.New("codec: schema fingerprint mismatch")
	ErrCorrupt        = errors.New("codec: corrupt or truncated input")
	ErrTooDeep        = errors.New("codec: nesting too deep")
	ErrTooLarge       = errors.New("codec: message too large")
)

const (
	headerLen = 5
	maxDepth  = 512
)

// readUvarint handles the common one-byte case without entering the general
// varint decoder. Longer and malformed encodings retain encoding/binary's
// exact behavior.
func readUvarint(b []byte) (uint64, int) {
	if len(b) != 0 && b[0] < 0x80 {
		return uint64(b[0]), 1
	}
	return binary.Uvarint(b)
}

type kind uint8

const (
	kBool kind = iota + 1
	kInt
	kUint
	kF32
	kF64
	kString
	kSlice
	kStruct
	kPtr
	kArray
	kMap
	kIface // interface{}: self-describing dynamic value
	kBin   // type with MarshalBinary/UnmarshalBinary (or MarshalText/UnmarshalText)
)

type sliceMode uint8

const (
	smRaw    sliceMode = iota + 1 // fixed-size raw elements
	smVarint                      // zigzag-delta varints
	smVar                         // offset table of variable elements
	smStruct                      // struct elements: row table or columnar lanes
)

type node struct {
	kind      kind
	size      uintptr
	rt        reflect.Type
	elem      *node // slice/array element, pointer target
	smode     sliceMode
	fields    []field
	varFields []int
	nbm       int // bitmap bytes

	batchT reflect.Type // kPtr to struct: []Target type, to allocate pointees in one block
	alen   int          // kArray: length
	kslice *node        // kMap: node for []Key
	vslice *node        // kMap: node for []Value
	text   bool         // kBin: uses MarshalText instead of MarshalBinary
}

type field struct {
	off  uintptr
	n    *node
	name string
}

// emptyInterface matches the two-word representation used by an interface{}
// value. It is used only for non-direct-interface values while the interface
// remains live in Append; pointer-shaped values need a temporary header.
type emptyInterface struct {
	typ  unsafe.Pointer
	data unsafe.Pointer
}

func (n *node) isVar() bool {
	switch n.kind {
	case kBool, kInt, kUint, kF32, kF64:
		return false
	}
	return true
}

type plan struct {
	root *node
	fp   uint32
	last atomic.Int64 // body size of the last message, for width selection
}

var plans sync.Map // reflect.Type -> *plan

var (
	binMarshaler   = reflect.TypeFor[encoding.BinaryMarshaler]()
	binUnmarshaler = reflect.TypeFor[encoding.BinaryUnmarshaler]()
	txtMarshaler   = reflect.TypeFor[encoding.TextMarshaler]()
	txtUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// planFor returns the cached plan for values of type t. Structs are encoded
// as frames directly; any other type is wrapped as a one-field struct.
func planFor(t reflect.Type) (*plan, error) {
	if v, ok := plans.Load(t); ok {
		return v.(*plan), nil
	}
	var sb strings.Builder
	n, err := build(t, map[reflect.Type]*node{}, &sb)
	if err != nil {
		return nil, err
	}
	root := n
	if n.kind != kStruct {
		sb.WriteString("W")
		root = &node{kind: kStruct, rt: t, size: t.Size(), nbm: 1, fields: []field{{n: n, name: "V"}}}
		if n.isVar() {
			root.varFields = []int{0}
		}
	}
	h := fnv.New64a()
	h.Write([]byte(sb.String()))
	s := h.Sum64()
	pl := &plan{root: root, fp: uint32(s) ^ uint32(s>>32)}
	v, _ := plans.LoadOrStore(t, pl)
	return v.(*plan), nil
}

func classify(el *node) sliceMode {
	switch {
	case el.kind == kBool || el.kind == kF32 || el.kind == kF64 || (el.kind == kUint && el.size == 1):
		return smRaw
	case el.kind == kInt || el.kind == kUint:
		return smVarint
	case el.kind == kStruct:
		return smStruct
	}
	return smVar
}

func sliceNode(t reflect.Type, el *node) *node {
	return &node{kind: kSlice, rt: reflect.SliceOf(t), size: 24, elem: el, smode: classify(el)}
}

func fieldName(sf reflect.StructField) (skip bool) {
	if sf.PkgPath != "" {
		return true
	}
	return sf.Tag.Get("codec") == "-" || sf.Tag.Get("json") == "-"
}

func build(t reflect.Type, memo map[reflect.Type]*node, sb *strings.Builder) (*node, error) {
	n := &node{rt: t, size: t.Size()}
	if k := t.Kind(); k != reflect.Interface && k != reflect.Pointer {
		pt := reflect.PointerTo(t)
		switch {
		case pt.Implements(binMarshaler) && pt.Implements(binUnmarshaler):
			n.kind = kBin
			sb.WriteString("bin:" + t.String())
			return n, nil
		case pt.Implements(txtMarshaler) && pt.Implements(txtUnmarshaler):
			n.kind, n.text = kBin, true
			sb.WriteString("txt:" + t.String())
			return n, nil
		}
	}
	switch t.Kind() {
	case reflect.Bool:
		n.kind = kBool
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n.kind = kInt
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n.kind = kUint
	case reflect.Float32:
		n.kind = kF32
	case reflect.Float64:
		n.kind = kF64
	case reflect.String:
		n.kind = kString
	case reflect.Interface:
		if t.NumMethod() != 0 {
			return nil, fmt.Errorf("codec: unsupported interface type %s", t)
		}
		n.kind = kIface
	case reflect.Slice, reflect.Array:
		n.kind = kSlice
		sb.WriteString("[")
		if t.Kind() == reflect.Array {
			n.kind, n.alen = kArray, t.Len()
			fmt.Fprintf(sb, "%d", n.alen)
		}
		sb.WriteString("]")
		el, err := build(t.Elem(), memo, sb)
		if err != nil {
			return nil, err
		}
		n.elem, n.smode = el, classify(el)
		return n, nil
	case reflect.Map:
		n.kind = kMap
		sb.WriteString("map[")
		k, err := build(t.Key(), memo, sb)
		if err != nil {
			return nil, err
		}
		sb.WriteString("]")
		v, err := build(t.Elem(), memo, sb)
		if err != nil {
			return nil, err
		}
		n.kslice, n.vslice = sliceNode(t.Key(), k), sliceNode(t.Elem(), v)
		return n, nil
	case reflect.Pointer:
		n.kind = kPtr
		sb.WriteByte('*')
		el, err := build(t.Elem(), memo, sb)
		if err != nil {
			return nil, err
		}
		n.elem = el
		if el.kind == kStruct {
			n.batchT = reflect.SliceOf(t.Elem())
		}
		return n, nil
	case reflect.Struct:
		if m, ok := memo[t]; ok {
			sb.WriteString("@")
			sb.WriteString(t.String())
			return m, nil
		}
		n.kind = kStruct
		memo[t] = n
		sb.WriteByte('{')
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if fieldName(sf) {
				continue
			}
			sb.WriteString(sf.Name)
			sb.WriteByte(':')
			fn, err := build(sf.Type, memo, sb)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", t, sf.Name, err)
			}
			sb.WriteByte(';')
			n.fields = append(n.fields, field{off: sf.Offset, n: fn, name: sf.Name})
			if fn.isVar() {
				n.varFields = append(n.varFields, len(n.fields)-1)
			}
		}
		sb.WriteByte('}')
		n.nbm = max(1, (len(n.fields)+7)/8)
		return n, nil
	default:
		return nil, fmt.Errorf("codec: unsupported type %s", t)
	}
	fmt.Fprintf(sb, "%d.%d", n.kind, n.size)
	return n, nil
}

// target resolves v to a plan and a pointer to its (dereferenced) value, like
// json.Marshal: pointers are followed, nil becomes the zero value.
func target(v any) (*plan, unsafe.Pointer, error) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() { // Marshal(nil)
		var x any
		pl, err := planFor(reflect.TypeFor[any]())
		return pl, unsafe.Pointer(&x), err
	}
	var last reflect.Value
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			rv = reflect.New(rv.Type().Elem()).Elem() // nil pointer encodes as the zero value
			last = rv.Addr()
			break
		}
		last = rv
		rv = rv.Elem()
	}
	pl, err := planFor(rv.Type())
	if err != nil {
		return nil, nil, err
	}
	if last.IsValid() {
		return pl, last.UnsafePointer(), nil
	}
	// Most non-pointer values are boxed indirectly in the interface already.
	// Reuse that storage instead of allocating and copying another T. One-word
	// aggregates can be direct-interface values, so retain the reflective
	// fallback for them and for pointer-shaped headers (maps, funcs, channels).
	if rv.Kind() != reflect.Map && rv.Kind() != reflect.Chan && rv.Kind() != reflect.Func &&
		rv.Kind() != reflect.UnsafePointer &&
		!((rv.Kind() == reflect.Struct || rv.Kind() == reflect.Array) && rv.Type().Size() == unsafe.Sizeof(uintptr(0))) {
		return pl, (*emptyInterface)(unsafe.Pointer(&v)).data, nil
	}
	pv := reflect.New(rv.Type())
	pv.Elem().Set(rv)
	return pl, pv.UnsafePointer(), nil
}

type sliceHeader struct {
	Data unsafe.Pointer
	Len  int
	Cap  int
}

var isLE = func() bool {
	x := uint16(1)
	return *(*byte)(unsafe.Pointer(&x)) == 1
}()

func zigzag(v int64) uint64   { return uint64(v<<1) ^ uint64(v>>63) }
func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

func loadInt(size uintptr, p unsafe.Pointer) int64 {
	switch size {
	case 1:
		return int64(*(*int8)(p))
	case 2:
		return int64(*(*int16)(p))
	case 4:
		return int64(*(*int32)(p))
	}
	return *(*int64)(p)
}

func loadUint(size uintptr, p unsafe.Pointer) uint64 {
	switch size {
	case 1:
		return uint64(*(*uint8)(p))
	case 2:
		return uint64(*(*uint16)(p))
	case 4:
		return uint64(*(*uint32)(p))
	}
	return *(*uint64)(p)
}

func storeInt(size uintptr, p unsafe.Pointer, v int64) {
	switch size {
	case 1:
		*(*int8)(p) = int8(v)
	case 2:
		*(*int16)(p) = int16(v)
	case 4:
		*(*int32)(p) = int32(v)
	default:
		*(*int64)(p) = v
	}
}

func storeUint(size uintptr, p unsafe.Pointer, v uint64) {
	switch size {
	case 1:
		*(*uint8)(p) = uint8(v)
	case 2:
		*(*uint16)(p) = uint16(v)
	case 4:
		*(*uint32)(p) = uint32(v)
	default:
		*(*uint64)(p) = v
	}
}

var le = binary.LittleEndian
