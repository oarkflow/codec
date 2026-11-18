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
//     Unexported fields and fields tagged `codec:"-"` are skipped; `codec:"name"`
//     renames a field in the text form and in interface{} values.
//   - Marshal sorts map output by key, so it is deterministic. A nil map, nil
//     slice and nil pointer-free zero value decode as empty/zero.
//   - Marshal(nil pointer) encodes the zero value of the pointee.
//   - Unmarshal copies strings and bytes from the input. Existing pointers,
//     slice storage and non-nil maps are reused; copy them before
//     retaining an earlier decoded snapshot. Use View for zero-copy field access.
//
// # Schema evolution
//
// Plain messages (Marshal) carry a fingerprint of the whole schema and can only
// be read by exactly the same type. Data that outlives one program version
// (files, databases, queues) should be written with MarshalEvolvable instead.
// Those messages also record how many fields each struct had, which lets a
// later version of the type, with fields added at the end of its structs,
// still read them: the added fields come back as zero.
//
//	type UserV1 struct { Name string; Age uint32 }
//	type UserV2 struct { Name string; Age uint32; Email string; Tags []string } // appended
//
//	data, _ := codec.MarshalEvolvable(&UserV1{"Ada", 36})
//	var u UserV2
//	codec.Unmarshal(data, &u) // u.Email == "", u.Tags == nil
//
// Allowed at any nesting depth (structs inside slices, maps, pointers, arrays):
// appending fields to a struct and introducing new struct types in them. Not
// allowed, and reported as ErrSchemaMismatch: removing, reordering, renaming
// or retyping fields, inserting a field before existing ones, and reading a
// newer message with an older type (the error says so). Renaming a struct
// type is harmless. The cost is 1 byte plus one byte per struct type in the
// message; encoding and decoding speed are unchanged. To drop a field, keep it
// in the struct and stop using it (or tag it `codec:"-"` only for a type that
// never has to read old data).
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
//	          or, for MarshalEvolvable, byte(0xC4 | widthCode) fingerprint32
//	          shape, where shape = uvarint(n) then n uvarint field counts, one
//	          per struct type in the schema, in first-use order (see Schema evolution)
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
	headerLen    = 5
	maxDepth     = 512
	hdrEvolvable = 0x04 // header flag: the message carries its struct field counts
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
	nbm       int          // bitmap bytes
	extra     []extraField // evolved plans: fields this type has that the writer's version lacked
	sid       int          // kStruct: creation order, used to refer back to this struct in the fingerprint

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

// extraField is a field of the reader's struct that the message's writer did
// not have (it was added later). It is zeroed on decode.
type extraField struct {
	off  uintptr
	rt   reflect.Type
	name string
	n    *node // for zeroing by kind; nil falls back to reflection
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
	root   *node
	fp     uint32
	counts []int        // field count of every struct in the schema, in creation order
	shape  []byte       // counts, encoded for the evolvable header
	last   atomic.Int64 // body size of the last message, for width selection
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
	b := &builder{memo: map[reflect.Type]*node{}}
	n, err := b.build(t)
	if err != nil {
		return nil, err
	}
	pl := b.finish(t, n)
	v, _ := plans.LoadOrStore(t, pl)
	return v.(*plan), nil
}

// finish wraps a non-struct root, fingerprints the schema, and records the
// struct field counts.
func (b *builder) finish(t reflect.Type, n *node) *plan {
	root := n
	if n.kind != kStruct {
		b.sb.WriteString("W")
		root = &node{kind: kStruct, rt: t, size: t.Size(), nbm: 1, fields: []field{{n: n, name: "V"}}}
		if n.isVar() {
			root.varFields = []int{0}
		}
	}
	h := fnv.New64a()
	h.Write([]byte(b.sb.String()))
	s := h.Sum64()
	counts := make([]int, len(b.order))
	for i, sn := range b.order {
		counts[i] = len(sn.fields)
	}
	return &plan{root: root, fp: uint32(s) ^ uint32(s>>32), counts: counts, shape: appendShape(nil, counts)}
}

// appendShape encodes struct field counts: uvarint(len) then each count.
func appendShape(dst []byte, counts []int) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(counts)))
	for _, c := range counts {
		dst = binary.AppendUvarint(dst, uint64(c))
	}
	return dst
}

// parseShape decodes appendShape output from the front of b and returns the
// counts and the bytes consumed.
func parseShape(b []byte) (counts []int, n int, ok bool) {
	cnt, k := readUvarint(b)
	if k <= 0 || cnt > uint64(len(b)) || cnt > maxShape {
		return nil, 0, false
	}
	n = k
	counts = make([]int, cnt)
	for i := range counts {
		c, k := readUvarint(b[n:])
		if k <= 0 || c > 1<<16 {
			return nil, 0, false
		}
		counts[i], n = int(c), n+k
	}
	return counts, n, true
}

const maxShape = 1 << 12 // most structs a schema may contain

type evolvedKey struct {
	t     reflect.Type
	shape string
}

var (
	evolvedPlans sync.Map // evolvedKey -> *plan
	evolvedCount atomic.Int64
)

const maxEvolvedPlans = 256 // bounds the cache against hostile headers

// evolvedPlan returns the plan for reading messages that an older version of
// t wrote, where each struct had the given number of fields (a prefix of the
// fields t has now). Fields added since are zeroed on decode. It fails with
// ErrSchemaMismatch if a struct had more fields than it has now.
func evolvedPlan(t reflect.Type, counts []int) (*plan, error) {
	key := evolvedKey{t, string(appendShape(nil, counts))}
	if v, ok := evolvedPlans.Load(key); ok {
		return v.(*plan), nil
	}
	b := &builder{memo: map[reflect.Type]*node{}, counts: counts}
	n, err := b.build(t)
	if err != nil {
		return nil, err
	}
	if len(b.order) != len(counts) {
		return nil, fmt.Errorf("%w: the message describes %d structs, this type has %d", ErrSchemaMismatch, len(counts), len(b.order))
	}
	pl := b.finish(t, n)
	if evolvedCount.Load() < maxEvolvedPlans {
		if v, loaded := evolvedPlans.LoadOrStore(key, pl); loaded {
			return v.(*plan), nil
		}
		evolvedCount.Add(1)
	}
	return pl, nil
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
	return sf.Tag.Get("codec") == "-"
}

// fieldLabel is the name a struct field goes by in text and in dynamic
// (interface{}) values: the codec tag if it has one, else the Go field name.
func fieldLabel(sf reflect.StructField) string {
	if tag, _, _ := strings.Cut(sf.Tag.Get("codec"), ","); tag != "" && tag != "-" {
		return tag
	}
	return sf.Name
}

// builder constructs the node tree and schema fingerprint for a type.
type builder struct {
	memo   map[reflect.Type]*node
	sb     strings.Builder
	order  []*node // struct nodes in creation order
	counts []int   // if set, struct i keeps only its first counts[i] fields
}

func (b *builder) build(t reflect.Type) (*node, error) {
	sb, memo := &b.sb, b.memo
	n := &node{rt: t, size: t.Size()}
	if k := t.Kind(); k != reflect.Interface && k != reflect.Pointer {
		pt := reflect.PointerTo(t)
		switch {
		case pt.Implements(binMarshaler) && pt.Implements(binUnmarshaler):
			n.kind = kBin
			sb.WriteString("bin:")
			sb.WriteString(t.String())
			return n, nil
		case pt.Implements(txtMarshaler) && pt.Implements(txtUnmarshaler):
			n.kind, n.text = kBin, true
			sb.WriteString("txt:")
			sb.WriteString(t.String())
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
		el, err := b.build(t.Elem())
		if err != nil {
			return nil, err
		}
		n.elem, n.smode = el, classify(el)
		return n, nil
	case reflect.Map:
		n.kind = kMap
		sb.WriteString("map[")
		k, err := b.build(t.Key())
		if err != nil {
			return nil, err
		}
		sb.WriteString("]")
		v, err := b.build(t.Elem())
		if err != nil {
			return nil, err
		}
		n.kslice, n.vslice = sliceNode(t.Key(), k), sliceNode(t.Elem(), v)
		return n, nil
	case reflect.Pointer:
		n.kind = kPtr
		sb.WriteByte('*')
		el, err := b.build(t.Elem())
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
			fmt.Fprintf(sb, "@%d", m.sid) // by position, so renaming a type does not change the schema
			return m, nil
		}
		n.kind = kStruct
		memo[t] = n
		idx := len(b.order)
		n.sid = idx
		b.order = append(b.order, n)
		limit := -1 // evolved plans keep only the first limit fields
		if b.counts != nil {
			if idx >= len(b.counts) {
				return nil, fmt.Errorf("%w: the message describes fewer structs than this type has", ErrSchemaMismatch)
			}
			limit = b.counts[idx]
			visible := 0
			for i := 0; i < t.NumField(); i++ {
				if !fieldName(t.Field(i)) {
					visible++
				}
			}
			if limit > visible {
				return nil, fmt.Errorf("%w: written by a newer schema: the message has %d fields for %s, this version has %d", ErrSchemaMismatch, limit, t, visible)
			}
		}
		sb.WriteByte('{')
		kept := 0
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if fieldName(sf) {
				continue
			}
			if limit >= 0 && kept >= limit {
				x := extraField{off: sf.Offset, rt: sf.Type, name: sf.Name}
				if xn, err := (&builder{memo: map[reflect.Type]*node{}}).build(sf.Type); err == nil {
					x.n = xn // built on its own, so it does not disturb the schema's struct order
				}
				n.extra = append(n.extra, x)
				continue
			}
			kept++
			sb.WriteString(sf.Name)
			sb.WriteByte(':')
			fn, err := b.build(sf.Type)
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
