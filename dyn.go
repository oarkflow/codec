package codec

import (
	"encoding"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"sort"
	"unsafe"
)

// Dynamic (interface{}) values are self-describing: one tag byte per value.
// Like encoding/json, they decode to generic Go types: nil, bool, int64,
// uint64, float64, string, []byte, []any, map[string]any and map[any]any.
// Structs inside an interface are encoded as maps keyed by field name.
const (
	dNil    = 0
	dFalse  = 1
	dTrue   = 2
	dInt    = 3  // zigzag uvarint
	dUint   = 4  // uvarint
	dFloat  = 5  // 8 bytes
	dStr    = 6  // uvarint len, bytes
	dBytes  = 7  // uvarint len, bytes
	dList   = 8  // uvarint count, items
	dMap    = 9  // uvarint count, (uvarint len, key bytes, item)...  string keys
	dAnyMap = 10 // uvarint count, (item key, item value)...
)

var errUnsupportedDyn = func(t reflect.Type) error { return fmt.Errorf("codec: unsupported type %s in interface value", t) }

func (e *enc) dyn(v reflect.Value, depth int) {
	if depth > maxDepth {
		e.setErr(ErrTooDeep)
		return
	}
	if !v.IsValid() {
		e.buf = append(e.buf, dNil)
		return
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			e.buf = append(e.buf, dNil)
			return
		}
		// pointer receivers may implement the marshalers
		if v.Kind() == reflect.Pointer && e.dynMarshaler(v) {
			return
		}
		e.dyn(v.Elem(), depth+1)
		return
	}
	if v.CanAddr() {
		if e.dynMarshaler(v.Addr()) {
			return
		}
	} else if v.Type().Implements(txtMarshaler) || v.Type().Implements(binMarshaler) {
		if e.dynMarshaler(v) {
			return
		}
	}
	switch v.Kind() {
	case reflect.Bool:
		if v.Bool() {
			e.buf = append(e.buf, dTrue)
		} else {
			e.buf = append(e.buf, dFalse)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		e.buf = append(e.buf, dInt)
		e.buf = binary.AppendUvarint(e.buf, zigzag(v.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		e.buf = append(e.buf, dUint)
		e.buf = binary.AppendUvarint(e.buf, v.Uint())
	case reflect.Float32, reflect.Float64:
		e.buf = append(e.buf, dFloat)
		e.buf = le.AppendUint64(e.buf, math.Float64bits(v.Float()))
	case reflect.String:
		e.buf = append(e.buf, dStr)
		e.buf = binary.AppendUvarint(e.buf, uint64(v.Len()))
		e.buf = append(e.buf, v.String()...)
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			e.buf = append(e.buf, dBytes)
			e.buf = binary.AppendUvarint(e.buf, uint64(v.Len()))
			if v.Kind() == reflect.Slice {
				e.buf = append(e.buf, v.Bytes()...)
			} else {
				for i := 0; i < v.Len(); i++ {
					e.buf = append(e.buf, byte(v.Index(i).Uint()))
				}
			}
			return
		}
		e.buf = append(e.buf, dList)
		e.buf = binary.AppendUvarint(e.buf, uint64(v.Len()))
		for i := 0; i < v.Len(); i++ {
			e.dyn(v.Index(i), depth+1)
		}
	case reflect.Map:
		if v.Type().Key() == reflect.TypeFor[string]() {
			e.dynStringMap(v, depth)
			return
		}

		keys := v.MapKeys()
		sortKeys(keys)
		if v.Type().Key().Kind() == reflect.String {
			e.buf = append(e.buf, dMap)
			e.buf = binary.AppendUvarint(e.buf, uint64(len(keys)))
			for _, k := range keys {
				s := k.String()
				e.buf = binary.AppendUvarint(e.buf, uint64(len(s)))
				e.buf = append(e.buf, s...)
				e.dyn(v.MapIndex(k), depth+1)
			}
			return
		}
		e.buf = append(e.buf, dAnyMap)
		e.buf = binary.AppendUvarint(e.buf, uint64(len(keys)))
		for _, k := range keys {
			e.dyn(k, depth+1)
			e.dyn(v.MapIndex(k), depth+1)
		}
	case reflect.Struct:
		t := v.Type()
		var idx []int
		var names []string
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if fieldName(sf) {
				continue
			}
			idx, names = append(idx, i), append(names, fieldLabel(sf))
		}
		e.buf = append(e.buf, dMap)
		e.buf = binary.AppendUvarint(e.buf, uint64(len(idx)))
		for j, i := range idx {
			e.buf = binary.AppendUvarint(e.buf, uint64(len(names[j])))
			e.buf = append(e.buf, names[j]...)
			e.dyn(v.Field(i), depth+1)
		}
	default:
		e.setErr(errUnsupportedDyn(v.Type()))
	}
}

func (e *enc) dynStringMap(v reflect.Value, depth int) {
	if v.Type() == reflect.TypeFor[map[string]any]() {
		e.dynStringAnyMap(v.Interface().(map[string]any), depth)
		return
	}

	for len(e.sc.dynKeys) <= depth {
		e.sc.dynKeys = append(e.sc.dynKeys, nil)
	}
	keys := e.sc.dynKeys[depth][:0]
	cached := v.Len() <= maxReusableMapEntries
	if !cached {
		keys = make([]string, 0, v.Len())
	}
	it := v.MapRange()
	for it.Next() {
		keys = append(keys, it.Key().String())
	}
	sort.Strings(keys)
	defer func() {
		clear(keys)
		if cached {
			e.sc.dynKeys[depth] = keys[:0]
		}
	}()
	e.buf = append(e.buf, dMap)
	e.buf = binary.AppendUvarint(e.buf, uint64(len(keys)))
	for _, k := range keys {
		e.buf = binary.AppendUvarint(e.buf, uint64(len(k)))
		e.buf = append(e.buf, k...)
		e.dyn(v.MapIndex(reflect.ValueOf(k)), depth+1)
	}
}

func (e *enc) dynStringAnyMap(m map[string]any, depth int) {
	for len(e.sc.dynKeys) <= depth {
		e.sc.dynKeys = append(e.sc.dynKeys, nil)
	}
	keys := e.sc.dynKeys[depth][:0]
	cached := len(m) <= maxReusableMapEntries
	if !cached {
		keys = make([]string, 0, len(m))
	}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	defer func() {
		clear(keys)
		if cached {
			e.sc.dynKeys[depth] = keys[:0]
		}
	}()
	e.buf = append(e.buf, dMap)
	e.buf = binary.AppendUvarint(e.buf, uint64(len(keys)))
	for _, k := range keys {
		e.buf = binary.AppendUvarint(e.buf, uint64(len(k)))
		e.buf = append(e.buf, k...)
		e.dynAny(m[k], depth+1)
		if e.err != nil {
			return
		}
	}
}

// dynAny handles common self-describing Go values without reflect.Value
// construction. Named types and custom marshalers fall back to dyn.
func (e *enc) dynAny(v any, depth int) {
	if depth > maxDepth {
		e.setErr(ErrTooDeep)
		return
	}
	switch x := v.(type) {
	case nil:
		e.buf = append(e.buf, dNil)
	case bool:
		if x {
			e.buf = append(e.buf, dTrue)
		} else {
			e.buf = append(e.buf, dFalse)
		}
	case int:
		e.buf = binary.AppendUvarint(append(e.buf, dInt), zigzag(int64(x)))
	case int8:
		e.buf = binary.AppendUvarint(append(e.buf, dInt), zigzag(int64(x)))
	case int16:
		e.buf = binary.AppendUvarint(append(e.buf, dInt), zigzag(int64(x)))
	case int32:
		e.buf = binary.AppendUvarint(append(e.buf, dInt), zigzag(int64(x)))
	case int64:
		e.buf = binary.AppendUvarint(append(e.buf, dInt), zigzag(x))
	case uint:
		e.buf = binary.AppendUvarint(append(e.buf, dUint), uint64(x))
	case uint8:
		e.buf = binary.AppendUvarint(append(e.buf, dUint), uint64(x))
	case uint16:
		e.buf = binary.AppendUvarint(append(e.buf, dUint), uint64(x))
	case uint32:
		e.buf = binary.AppendUvarint(append(e.buf, dUint), uint64(x))
	case uint64:
		e.buf = binary.AppendUvarint(append(e.buf, dUint), x)
	case float32:
		e.buf = append(e.buf, dFloat)
		e.buf = le.AppendUint64(e.buf, math.Float64bits(float64(x)))
	case float64:
		e.buf = append(e.buf, dFloat)
		e.buf = le.AppendUint64(e.buf, math.Float64bits(x))
	case string:
		e.buf = append(e.buf, dStr)
		e.buf = binary.AppendUvarint(e.buf, uint64(len(x)))
		e.buf = append(e.buf, x...)
	case []byte:
		e.buf = append(e.buf, dBytes)
		e.buf = binary.AppendUvarint(e.buf, uint64(len(x)))
		e.buf = append(e.buf, x...)
	case []any:
		e.buf = append(e.buf, dList)
		e.buf = binary.AppendUvarint(e.buf, uint64(len(x)))
		for _, item := range x {
			e.dynAny(item, depth+1)
			if e.err != nil {
				return
			}
		}
	case map[string]any:
		e.dynStringAnyMap(x, depth)
	default:
		e.dyn(reflect.ValueOf(v), depth)
	}
}

// dynMarshaler encodes v via TextMarshaler (as a string) or BinaryMarshaler
// (as bytes) when it implements one, reporting whether it did.
func (e *enc) dynMarshaler(v reflect.Value) bool {
	if !v.CanInterface() {
		return false
	}
	if m, ok := v.Interface().(encoding.TextMarshaler); ok {
		b, err := m.MarshalText()
		if err != nil {
			e.setErr(err)
			return true
		}
		e.buf = append(e.buf, dStr)
		e.buf = binary.AppendUvarint(e.buf, uint64(len(b)))
		e.buf = append(e.buf, b...)
		return true
	}
	if m, ok := v.Interface().(encoding.BinaryMarshaler); ok {
		b, err := m.MarshalBinary()
		if err != nil {
			e.setErr(err)
			return true
		}
		e.buf = append(e.buf, dBytes)
		e.buf = binary.AppendUvarint(e.buf, uint64(len(b)))
		e.buf = append(e.buf, b...)
		return true
	}
	return false
}

func sortKeys(keys []reflect.Value) {
	if len(keys) < 2 {
		return
	}
	switch keys[0].Kind() {
	case reflect.String:
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		sort.Slice(keys, func(i, j int) bool { return keys[i].Int() < keys[j].Int() })
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		sort.Slice(keys, func(i, j int) bool { return keys[i].Uint() < keys[j].Uint() })
	case reflect.Float32, reflect.Float64:
		sort.Slice(keys, func(i, j int) bool { return keys[i].Float() < keys[j].Float() })
	case reflect.Bool:
		sort.Slice(keys, func(i, j int) bool { return !keys[i].Bool() && keys[j].Bool() })
	}
}

// dyn decodes one dynamic value from b, returning it and the bytes consumed.
func (d *dec) dyn(b []byte, depth int) (any, int, error) {
	if depth > maxDepth {
		return nil, 0, ErrTooDeep
	}
	if len(b) == 0 {
		return nil, 0, ErrCorrupt
	}
	pos := 1
	uv := func() (uint64, bool) {
		u, k := readUvarint(b[pos:])
		if k <= 0 {
			return 0, false
		}
		pos += k
		return u, true
	}
	switch b[0] {
	case dNil:
		return nil, 1, nil
	case dFalse:
		return false, 1, nil
	case dTrue:
		return true, 1, nil
	case dInt:
		u, ok := uv()
		if !ok {
			return nil, 0, ErrCorrupt
		}
		return d.boxInt64(unzigzag(u)), pos, nil
	case dUint:
		u, ok := uv()
		if !ok {
			return nil, 0, ErrCorrupt
		}
		return u, pos, nil
	case dFloat:
		if len(b) < 9 {
			return nil, 0, ErrCorrupt
		}
		return d.boxFloat64(math.Float64frombits(le.Uint64(b[1:]))), 9, nil
	case dStr, dBytes:
		l, ok := uv()
		if !ok || l > uint64(len(b)-pos) {
			return nil, 0, ErrCorrupt
		}
		raw := b[pos : pos+int(l)]
		pos += int(l)
		if b[0] == dStr {
			return d.str(raw), pos, nil
		}
		if len(raw) == 0 {
			return []byte{}, pos, nil
		}
		if err := d.charge(len(raw), 1); err != nil {
			return nil, 0, err
		}
		p := d.carve(uintptr(len(raw)), 1)
		out := unsafe.Slice((*byte)(p), len(raw))
		copy(out, raw)
		return out, pos, nil
	case dList:
		c, ok := uv()
		if !ok || c > uint64(len(b)-pos) {
			return nil, 0, ErrCorrupt
		}
		if err := d.charge(int(c), unsafe.Sizeof(any(nil))); err != nil {
			return nil, 0, err
		}
		out := make([]any, c)
		for i := range out {
			v, n, err := d.dyn(b[pos:], depth+1)
			if err != nil {
				return nil, 0, err
			}
			out[i] = v
			pos += n
		}
		return out, pos, nil
	case dMap:
		c, ok := uv()
		if !ok || c > uint64(len(b)-pos) {
			return nil, 0, ErrCorrupt
		}
		if err := d.charge(int(c), 2*unsafe.Sizeof(any(nil))); err != nil {
			return nil, 0, err
		}
		out := make(map[string]any, c)
		for i := uint64(0); i < c; i++ {
			l, ok := uv()
			if !ok || l > uint64(len(b)-pos) {
				return nil, 0, ErrCorrupt
			}
			key := d.str(b[pos : pos+int(l)])
			pos += int(l)
			v, n, err := d.dyn(b[pos:], depth+1)
			if err != nil {
				return nil, 0, err
			}
			pos += n
			out[key] = v
		}
		return out, pos, nil
	case dAnyMap:
		c, ok := uv()
		if !ok || c > uint64(len(b)-pos) {
			return nil, 0, ErrCorrupt
		}
		if err := d.charge(int(c), 4*unsafe.Sizeof(any(nil))); err != nil {
			return nil, 0, err
		}
		out := make(map[any]any, c)
		for i := uint64(0); i < c; i++ {
			k, n, err := d.dyn(b[pos:], depth+1)
			if err != nil {
				return nil, 0, err
			}
			pos += n
			if k != nil && !reflect.TypeOf(k).Comparable() {
				return nil, 0, ErrCorrupt
			}
			v, n, err := d.dyn(b[pos:], depth+1)
			if err != nil {
				return nil, 0, err
			}
			pos += n
			out[k] = v
		}
		return out, pos, nil
	}
	return nil, 0, ErrCorrupt
}

// Boxing a number into an interface normally costs one heap allocation each.
// Instead the 8-byte payload is carved from the decode arena and the interface
// is assembled around it (the arena is pointer-free memory, so this is GC-safe
// and the interface keeps its block alive).
type eface struct{ typ, data unsafe.Pointer }

var (
	typInt64   = (*eface)(unsafe.Pointer(&[]any{int64(0)}[0])).typ
	typUint64  = (*eface)(unsafe.Pointer(&[]any{uint64(0)}[0])).typ
	typFloat64 = (*eface)(unsafe.Pointer(&[]any{float64(0)}[0])).typ
)

func (d *dec) box(typ unsafe.Pointer, bits uint64) any {
	p := d.carve(8, 8)
	*(*uint64)(p) = bits
	var v any
	*(*eface)(unsafe.Pointer(&v)) = eface{typ, p}
	return v
}

func (d *dec) boxInt64(x int64) any     { return d.box(typInt64, uint64(x)) }
func (d *dec) boxUint64(x uint64) any   { return d.box(typUint64, x) }
func (d *dec) boxFloat64(x float64) any { return d.box(typFloat64, math.Float64bits(x)) }
