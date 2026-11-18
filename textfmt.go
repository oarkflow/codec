package codec

import (
	"cmp"
	"encoding"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Strata text notation
//
// A readable, hand-writable form of exactly the values the binary protocol
// carries, with its own printer and parser (no other format involved).
//
// A document is a list of lines, and every line stands alone: `path value`.
// Nesting is written in the path with dots, never with indentation, tabs,
// brackets or begin/end words, and you never number anything: a list item is
// started with `+`.
//
//	Name           Ada Lovelace          a field and its value (the rest of the line)
//	Age            36
//	Active         true
//	Score          3.5
//	Tags           admin dev ops         several simple values: words on one line
//	Notes          "needs review" urgent a word with spaces is quoted
//	Raw            0x00ff10              bytes, in hex
//	Addr.Street    12 Main St            a nested object: Addr's Street
//	Addr.Zip       12345
//	Items.+                              a list of objects: "+" starts the next item,
//	Items.SKU      sku-1                 and the lines after it fill that item in
//	Items.Qty      2
//	Items.+
//	Items.SKU      sku-2
//	Matrix.+       1 2 3                 a list of lists: one "+" line per row
//	Matrix.+       4 5 6
//	Stock.apple    3                     a map: the key is part of the path
//	Stock."red pear"  5
//	Owner          ~                     null
//
// Rules, complete:
//
//   - A line is `path value`: the path runs to the first space; the value is
//     the rest of the line. Lines starting with # are comments; blank lines
//     are ignored. A `;` also ends a line, so a whole document fits on one
//     line, which suits `curl -d`:
//     Name Ada; Addr.Zip 12345; Items.+; Items.SKU a; Items.+; Items.SKU b
//   - A path is names joined by dots. Each name is a struct field or a map
//     key; a name with spaces, dots or other special characters is written in
//     double quotes. Field names are matched ignoring case.
//   - Lists. `Items.+` appends a new, empty item; `Items.+ value` appends an
//     item with that value (`Tags.+ admin`, `Matrix.+ 1 2 3`). A name after a
//     list means "in the item just started" (`Items.SKU x`), and the first
//     such line starts the first item if there is none yet, so a one-item list
//     needs no `+`. For a list inside a list, `^` means the item just started:
//     `Cube.+; Cube.^.+ a b; Cube.^.+ c`. A number (`Items.2.Qty 5`) picks an
//     existing item, for editing; the next free number appends.
//   - Strings are written bare when that is unambiguous, otherwise in double
//     quotes with the escapes \n \r \t \\ \" \xHH \uXXXX \UXXXXXXXX. A bare
//     string is the whole rest of the line, so `Name Ada Lovelace` needs no
//     quotes. In a line of several values the words are split on spaces, so a
//     word with spaces is quoted.
//   - `~` is null. An absent field, an empty value, `0`, `""` and `false` are
//     all the zero value, and printing omits zero fields (as the binary form
//     does), so printed text is as short as it can be.
//   - Unknown field names are an error that lists the valid ones. Later lines
//     override earlier ones.
//   - Values of type interface{} are self-describing: 36 is int64 (uint64
//     beyond MaxInt64), 3.5 float64, true/false bool, ~ nil, "..." or any
//     other word string, 0x.. []byte. A path through an interface{} builds
//     map[string]any for names and []any for `+`. Several words on one line
//     give a []any of them; ~list and ~map are the empty list and map. Quote
//     a string that would otherwise read as one of these ("36", "true"), and
//     a name that is a number, to keep it a key. Structs inside interface{}
//     print as maps, as in the binary form.
//   - Types implementing encoding.TextMarshaler (time.Time, netip.Addr, ...)
//     are written as a string, and those with only encoding.BinaryMarshaler
//     as hex bytes.
//   - A document that is a single simple value (a number, a string) is just
//     that value on one line; a top-level list is `+ first`, `+ second`.

// TextError reports a syntax or type error with its position.
type TextError struct {
	Line, Col int
	Msg       string
}

func (e *TextError) Error() string {
	return fmt.Sprintf("codec text: line %d, column %d: %s", e.Line, e.Col, e.Msg)
}

// ------------------------------------------------------------------ plans

type tkind uint8

const (
	tkBool tkind = iota
	tkInt
	tkUint
	tkFloat
	tkString
	tkBytes
	tkText // encoding.TextMarshaler / TextUnmarshaler
	tkBin  // encoding.BinaryMarshaler / BinaryUnmarshaler
	tkPtr
	tkList
	tkMap
	tkStruct
	tkAny
)

type ttype struct {
	t       reflect.Type
	kind    tkind
	scalar  bool // one token, no children
	elem    *ttype
	key     *ttype
	inline  bool // list whose elements are scalars: written on one line
	array   bool
	bits    int
	fields  []tfield
	byName  map[string]int
	byLower map[string]int
	addr    bool      // marshal methods have pointer receivers
	hold    sync.Pool // *mapHold, for map types
}

type tfield struct {
	idx  int
	name string
	key  []byte // name as a key token
	typ  *ttype
}

// lookup finds a struct field by name, exactly or ignoring case. Small structs
// are searched linearly, which beats hashing.
func (t *ttype) lookup(name []byte) (int, bool) {
	if len(t.fields) <= 12 {
		for i := range t.fields {
			if f := t.fields[i].name; len(f) == len(name) && f == string(name) {
				return i, true
			}
		}
		for i := range t.fields {
			if f := t.fields[i].name; len(f) == len(name) && strings.EqualFold(f, string(name)) {
				return i, true
			}
		}
		return 0, false
	}
	if i, ok := t.byName[string(name)]; ok {
		return i, true
	}
	i, ok := t.byLower[strings.ToLower(string(name))]
	return i, ok
}

func (t *ttype) scalarish() bool {
	return t.scalar || (t.kind == tkPtr && t.elem.scalarish())
}

var textPlans sync.Map // reflect.Type -> *ttype

func textPlan(t reflect.Type) (*ttype, error) {
	if p, ok := textPlans.Load(t); ok {
		return p.(*ttype), nil
	}
	memo := map[reflect.Type]*ttype{}
	tt, err := buildT(t, memo)
	if err != nil {
		return nil, err
	}
	for k, v := range memo {
		textPlans.Store(k, v)
	}
	return tt, nil
}

func buildT(t reflect.Type, memo map[reflect.Type]*ttype) (*ttype, error) {
	if tt, ok := memo[t]; ok {
		return tt, nil
	}
	tt := &ttype{t: t}
	memo[t] = tt
	pt := reflect.PointerTo(t)
	k := t.Kind()
	switch {
	case k != reflect.Pointer && k != reflect.Interface && pt.Implements(textUnmarshalerType) &&
		(t.Implements(textMarshalerType) || pt.Implements(textMarshalerType)):
		tt.kind, tt.scalar = tkText, true
		tt.addr = !t.Implements(textMarshalerType)
	case k != reflect.Pointer && k != reflect.Interface && pt.Implements(binUnmarshalerType) &&
		(t.Implements(binMarshalerType) || pt.Implements(binMarshalerType)):
		tt.kind, tt.scalar = tkBin, true
		tt.addr = !t.Implements(binMarshalerType)
	default:
		switch k {
		case reflect.Bool:
			tt.kind, tt.scalar = tkBool, true
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			tt.kind, tt.scalar = tkInt, true
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			tt.kind, tt.scalar = tkUint, true
		case reflect.Float32, reflect.Float64:
			tt.kind, tt.scalar, tt.bits = tkFloat, true, t.Bits()
		case reflect.String:
			tt.kind, tt.scalar = tkString, true
		case reflect.Slice, reflect.Array:
			tt.array = k == reflect.Array
			if t.Elem().Kind() == reflect.Uint8 {
				tt.kind, tt.scalar = tkBytes, true
				break
			}
			el, err := buildT(t.Elem(), memo)
			if err != nil {
				return nil, err
			}
			tt.kind, tt.elem = tkList, el
			tt.inline = el.scalarish()
		case reflect.Map:
			kt, err := buildT(t.Key(), memo)
			if err != nil {
				return nil, err
			}
			if !kt.scalarish() {
				return nil, fmt.Errorf("%w: map key %s", ErrNotStruct, t.Key())
			}
			el, err := buildT(t.Elem(), memo)
			if err != nil {
				return nil, err
			}
			tt.kind, tt.key, tt.elem = tkMap, kt, el
		case reflect.Pointer:
			el, err := buildT(t.Elem(), memo)
			if err != nil {
				return nil, err
			}
			tt.kind, tt.elem = tkPtr, el
		case reflect.Interface:
			if t.NumMethod() != 0 {
				return nil, fmt.Errorf("%w: %s", ErrNotStruct, t)
			}
			tt.kind = tkAny
		case reflect.Struct:
			tt.kind = tkStruct
			tt.byName, tt.byLower = map[string]int{}, map[string]int{}
			for i := 0; i < t.NumField(); i++ {
				sf := t.Field(i)
				if fieldName(sf) {
					continue
				}
				ft, err := buildT(sf.Type, memo)
				if err != nil {
					return nil, err
				}
				name := fieldLabel(sf)
				n := len(tt.fields)
				tt.fields = append(tt.fields, tfield{idx: i, name: name, key: appendKey(nil, name), typ: ft})
				tt.byName[name] = n
				if _, dup := tt.byLower[strings.ToLower(name)]; !dup {
					tt.byLower[strings.ToLower(name)] = n
				}
			}
		default:
			return nil, fmt.Errorf("%w: %s", ErrNotStruct, t)
		}
	}
	return tt, nil
}

var (
	textMarshalerType   = reflect.TypeFor[encoding.TextMarshaler]()
	binMarshalerType    = reflect.TypeFor[encoding.BinaryMarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
	binUnmarshalerType  = reflect.TypeFor[encoding.BinaryUnmarshaler]()
)

// ----------------------------------------------------------------- public

// MarshalText returns the text notation of v (see the format description
// above). v may be any value Marshal accepts; a pointer prints as its target.
func MarshalText(v any) ([]byte, error) { return AppendText(nil, v) }

// AppendText is MarshalText appending to dst.
func AppendText(dst []byte, v any) ([]byte, error) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return append(dst, "~\n"...), nil
	}
	for rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv = rv.Elem()
	}
	tt, err := textPlan(rv.Type())
	if err != nil {
		return dst, err
	}
	e := tenc{b: dst}
	if err := e.document(rv, tt); err != nil {
		return dst, err
	}
	return e.b, nil
}

// AppendTextEntry appends the lines of one top-level map entry, `key.path
// value`, for building a document of many records without holding them all
// in memory.
func AppendTextEntry(dst []byte, key string, v any) ([]byte, error) {
	rv := reflect.ValueOf(v)
	for rv.IsValid() && rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv = rv.Elem()
	}
	e := tenc{b: dst, pre: appendKey(nil, key)}
	if !rv.IsValid() {
		e.null("~")
		return e.b, nil
	}
	tt, err := textPlan(rv.Type())
	if err != nil {
		return dst, err
	}
	if err := e.walk(rv, tt, false, 0); err != nil {
		return dst, err
	}
	return e.b, nil
}

// ToText converts a binary message to text. schema is a pointer to a value of
// the type that was marshaled; it is used only for its type.
func ToText(data []byte, schema any) ([]byte, error) {
	v, err := newOf(schema)
	if err != nil {
		return nil, err
	}
	if err := Unmarshal(data, v); err != nil {
		return nil, err
	}
	return AppendText(nil, v)
}

// FromText parses text into a new value of the type schema points to and
// returns it as a binary message.
func FromText(text []byte, schema any) ([]byte, error) {
	v, err := newOf(schema)
	if err != nil {
		return nil, err
	}
	if err := UnmarshalText(text, v); err != nil {
		return nil, err
	}
	return Marshal(v)
}

func newOf(schema any) (any, error) {
	rv := reflect.ValueOf(schema)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return nil, ErrNeedPointer
	}
	return reflect.New(rv.Type().Elem()).Interface(), nil
}

// ---------------------------------------------------------------- printing

// tenc prints one `path value` line per leaf. pre is the path to the value
// being printed.
type tenc struct {
	b       []byte
	pre     []byte
	kb      []byte // scratch: one rendered map key
	scratch []byte // scratch: sorted map entries
}

// settle dereferences pointers and interfaces. null reports a nil on the way;
// dyn turns on once an interface is crossed; ptr reports that a pointer was
// crossed.
func settle(v reflect.Value, tt *ttype, dyn bool) (rv reflect.Value, rt *ttype, rdyn, null, ptr bool, err error) {
	for {
		switch tt.kind {
		case tkPtr:
			if v.IsNil() {
				return v, tt, dyn, true, ptr, nil
			}
			v, tt, ptr = v.Elem(), tt.elem, true
		case tkAny:
			if v.IsNil() {
				return v, tt, dyn, true, ptr, nil
			}
			v = v.Elem()
			if tt, err = textPlan(v.Type()); err != nil {
				return v, tt, dyn, false, ptr, err
			}
			dyn = true
		default:
			return v, tt, dyn, false, ptr, nil
		}
	}
}

func (e *tenc) document(v reflect.Value, tt *ttype) error {
	return e.walk(v, tt, false, 0)
}

// push appends a path segment; pop undoes it.
func (e *tenc) push(seg string) int {
	n := len(e.pre)
	if n > 0 {
		e.pre = append(e.pre, '.')
	}
	e.pre = append(e.pre, seg...)
	return n
}

func (e *tenc) pushB(seg []byte) int {
	n := len(e.pre)
	if n > 0 {
		e.pre = append(e.pre, '.')
	}
	e.pre = append(e.pre, seg...)
	return n
}

func (e *tenc) pop(n int) { e.pre = e.pre[:n] }

// start begins a line: the path and a space (nothing at the top level).
func (e *tenc) start() {
	if len(e.pre) > 0 {
		e.b = append(e.b, e.pre...)
		e.b = append(e.b, ' ')
	}
}

func (e *tenc) null(word string) {
	e.start()
	e.b = append(e.b, word...)
	e.b = append(e.b, '\n')
}

// marker writes a line that is just the path, e.g. `Items.+`.
func (e *tenc) marker() {
	e.b = append(e.b, e.pre...)
	e.b = append(e.b, '\n')
}

// fields prints the lines for a struct's fields or a map's entries under the
// current path. It reports whether it printed any. With force, a struct that
// would print nothing gets its first simple field written out as an explicit
// zero (so a non-nil pointer to a zero struct reads back non-nil).
func (e *tenc) fields(v reflect.Value, tt *ttype, dyn, force bool, depth int) (bool, error) {
	if tt.kind == tkMap {
		if v.Len() == 0 {
			return false, nil
		}
		return true, e.mapEntries(v, tt, dyn, depth)
	}
	printed := false
	for i := range tt.fields {
		f := &tt.fields[i]
		fv := v.Field(f.idx)
		if !dyn && zeroField(fv) {
			continue
		}
		printed = true
		n := e.pushB(f.key)
		err := e.walk(fv, f.typ, dyn, depth+1)
		e.pop(n)
		if err != nil {
			return printed, err
		}
	}
	if !printed && force {
		for i := range tt.fields {
			f := &tt.fields[i]
			if f.typ.scalar {
				n := e.pushB(f.key)
				err := e.walk(v.Field(tt.fields[i].idx), f.typ, true, depth+1)
				e.pop(n)
				return err == nil, err
			}
		}
	}
	return printed, nil
}

func (e *tenc) walk(v reflect.Value, tt *ttype, dyn bool, depth int) error {
	if depth > maxDepth {
		return ErrTooDeep
	}
	v, tt, dyn, null, ptr, err := settle(v, tt, dyn)
	if err != nil {
		return err
	}
	if null {
		e.null("~")
		return nil
	}
	switch tt.kind {
	case tkStruct, tkMap:
		printed, err := e.fields(v, tt, dyn, ptr, depth)
		if err != nil {
			return err
		}
		if !printed && len(e.pre) > 0 {
			switch {
			case dyn && tt.kind == tkMap:
				e.null("~map")
			case dyn:
				e.null("~map")
			default:
				e.null("~")
			}
		}
		return nil
	case tkList:
		if v.Len() == 0 {
			if dyn {
				e.null("~list")
			} else if len(e.pre) > 0 {
				e.null("~")
			}
			return nil
		}
		if tt.inline && !dyn && len(e.pre) > 0 {
			e.start()
			if err := e.tokens(v, tt); err != nil {
				return err
			}
			e.b = append(e.b, '\n')
			return nil
		}
		return e.items(v, tt, dyn, depth)
	}
	e.start()
	if err := e.scalar(v, tt, dyn, qWhole); err != nil {
		return err
	}
	e.b = append(e.b, '\n')
	return nil
}

// items prints a list as `path.+` lines, one per element (a fixed-size array
// uses the element's position instead, and leaves zero elements out).
func (e *tenc) items(v reflect.Value, tt *ttype, dyn bool, depth int) error {
	arr := tt.array && !dyn
	for i := 0; i < v.Len(); i++ {
		ev, et, edyn, null, ptr, err := settle(v.Index(i), tt.elem, dyn)
		if err != nil {
			return err
		}
		var n int
		if arr {
			if !null && ev.IsZero() {
				continue
			}
			var num [20]byte
			n = e.pushB(strconv.AppendInt(num[:0], int64(i), 10))
		} else {
			n = e.push("+")
		}
		switch {
		case null:
			e.null("~")
			e.pop(n)
		case et.kind == tkStruct || et.kind == tkMap:
			if edyn && et.kind == tkMap && ev.Len() == 0 {
				e.null("~map")
				e.pop(n)
				continue
			}
			if arr { // fields live under the element's number
				_, err := e.fields(ev, et, edyn, ptr, depth+1)
				e.pop(n)
				if err != nil {
					return err
				}
				continue
			}
			e.marker() // `Items.+`, then the item's fields as `Items.Field value`
			e.pop(n)
			if _, err := e.fields(ev, et, edyn, ptr, depth+1); err != nil {
				return err
			}
		case et.kind == tkList:
			if ev.Len() == 0 {
				if edyn {
					e.null("~list")
				} else if !arr {
					e.marker()
				}
				e.pop(n)
				continue
			}
			if et.inline && !edyn {
				e.start()
				if err := e.tokens(ev, et); err != nil {
					return err
				}
				e.b = append(e.b, '\n')
				e.pop(n)
				continue
			}
			if arr { // the inner list's lines live under the element's number
				err := e.items(ev, et, edyn, depth+1)
				e.pop(n)
				if err != nil {
					return err
				}
				continue
			}
			e.marker()
			e.pop(n)
			m := e.push("^")
			err := e.items(ev, et, edyn, depth+1)
			e.pop(m)
			if err != nil {
				return err
			}
		default:
			e.start()
			if err := e.scalar(ev, et, edyn, qWhole); err != nil {
				return err
			}
			e.b = append(e.b, '\n')
			e.pop(n)
		}
	}
	return nil
}

// mapHold is reusable scratch for printing or reading one map type.
type mapHold struct {
	k, v reflect.Value
	ents []mapEnt
}

type mapEnt struct {
	s        string
	n        int64
	u        uint64
	f        float64
	off, end int
}

func (tt *ttype) getHold() *mapHold {
	if h, ok := tt.hold.Get().(*mapHold); ok {
		return h
	}
	return &mapHold{k: reflect.New(tt.t.Key()).Elem(), v: reflect.New(tt.t.Elem()).Elem()}
}

func (tt *ttype) putHold(h *mapHold) {
	h.k.SetZero()
	h.v.SetZero()
	h.ents = h.ents[:0]
	tt.hold.Put(h)
}

// mapEntries prints a map's entries in key order. Each entry is rendered
// straight into the output as it is met, then the rendered pieces are put in
// sorted order, so no per-entry copies are made.
func (e *tenc) mapEntries(v reflect.Value, tt *ttype, dyn bool, depth int) error {
	h := tt.getHold()
	defer tt.putHold(h)
	kind := tt.key.t.Kind()
	start := len(e.b)
	for it := v.MapRange(); it.Next(); {
		h.k.SetIterKey(it)
		h.v.SetIterValue(it)
		var ent mapEnt
		switch kind {
		case reflect.String:
			ent.s = h.k.String()
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			ent.n = h.k.Int()
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			ent.u = h.k.Uint()
		case reflect.Float32, reflect.Float64:
			ent.f = h.k.Float()
		}
		kv, kt, _, _, _, err := settle(h.k, tt.key, false)
		if err != nil {
			return err
		}
		ke := tenc{b: e.kb[:0]}
		if err := ke.scalar(kv, kt, false, qKey); err != nil {
			return err
		}
		e.kb = ke.b
		if kind != reflect.String && kind != reflect.Int && kind != reflect.Int8 && kind != reflect.Int16 && kind != reflect.Int32 &&
			kind != reflect.Int64 && kind != reflect.Uint && kind != reflect.Uint8 && kind != reflect.Uint16 && kind != reflect.Uint32 &&
			kind != reflect.Uint64 && kind != reflect.Uintptr && kind != reflect.Float32 && kind != reflect.Float64 {
			ent.s = string(e.kb) // other key types sort by their text
		}
		ent.off = len(e.b)
		n := e.pushB(e.kb)
		err = e.walk(h.v, tt.elem, dyn, depth+1)
		e.pop(n)
		if err != nil {
			return err
		}
		ent.end = len(e.b)
		h.ents = append(h.ents, ent)
	}
	if len(h.ents) < 2 {
		return nil
	}
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		slices.SortFunc(h.ents, func(a, b mapEnt) int { return cmp.Compare(a.n, b.n) })
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		slices.SortFunc(h.ents, func(a, b mapEnt) int { return cmp.Compare(a.u, b.u) })
	case reflect.Float32, reflect.Float64:
		slices.SortFunc(h.ents, func(a, b mapEnt) int { return cmp.Compare(a.f, b.f) })
	default:
		slices.SortFunc(h.ents, func(a, b mapEnt) int { return strings.Compare(a.s, b.s) })
	}
	out := e.scratch[:0]
	for _, ent := range h.ents {
		out = append(out, e.b[ent.off:ent.end]...)
	}
	copy(e.b[start:], out)
	e.scratch = out[:0]
	return nil
}

func zeroField(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Slice, reflect.Map:
		return v.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	}
	return v.IsZero()
}

// tokens writes the elements of a list of scalars separated by spaces.
func (e *tenc) tokens(v reflect.Value, tt *ttype) error {
	for i := 0; i < v.Len(); i++ {
		if i > 0 {
			e.b = append(e.b, ' ')
		}
		ev, et, _, null, _, err := settle(v.Index(i), tt.elem, false)
		if err != nil {
			return err
		}
		if null {
			e.b = append(e.b, '~')
			continue
		}
		if err := e.scalar(ev, et, false, qToken); err != nil {
			return err
		}
	}
	return nil
}

// quoting contexts for strings
const (
	qWhole  = iota // the rest of a line
	qToken         // one word of an inline list
	qKey           // a path name
	qKeyDyn        // a path name inside an interface{} (digits would read as a list index)
)

func (e *tenc) scalar(v reflect.Value, tt *ttype, dyn bool, q int) error {
	switch tt.kind {
	case tkBool:
		e.b = strconv.AppendBool(e.b, v.Bool())
	case tkInt:
		e.b = strconv.AppendInt(e.b, v.Int(), 10)
	case tkUint:
		e.b = strconv.AppendUint(e.b, v.Uint(), 10)
	case tkFloat:
		e.b = appendFloat(e.b, v.Float(), tt.bits, dyn)
	case tkString:
		e.str(v.String(), dyn, q)
	case tkBytes:
		e.b = append(e.b, '0', 'x')
		if v.Kind() == reflect.Slice {
			e.b = hex.AppendEncode(e.b, v.Bytes())
		} else {
			raw := make([]byte, v.Len())
			reflect.Copy(reflect.ValueOf(raw), v)
			e.b = hex.AppendEncode(e.b, raw)
		}
	case tkText:
		m, ok := marshalerOf[encoding.TextMarshaler](v, tt)
		if !ok {
			return fmt.Errorf("%w: %s", ErrNotStruct, tt.t)
		}
		txt, err := m.MarshalText()
		if err != nil {
			return err
		}
		e.str(string(txt), false, q)
	case tkBin:
		m, ok := marshalerOf[encoding.BinaryMarshaler](v, tt)
		if !ok {
			return fmt.Errorf("%w: %s", ErrNotStruct, tt.t)
		}
		bin, err := m.MarshalBinary()
		if err != nil {
			return err
		}
		e.b = append(e.b, '0', 'x')
		e.b = hex.AppendEncode(e.b, bin)
	default:
		return fmt.Errorf("%w: %s", ErrNotStruct, tt.t)
	}
	return nil
}

func marshalerOf[M any](v reflect.Value, tt *ttype) (M, bool) {
	var zero M
	if tt.addr {
		if !v.CanAddr() {
			c := reflect.New(v.Type())
			c.Elem().Set(v)
			v = c.Elem()
		}
		m, ok := v.Addr().Interface().(M)
		return m, ok
	}
	if !v.CanInterface() {
		return zero, false
	}
	m, ok := v.Interface().(M)
	return m, ok
}

func appendFloat(b []byte, f float64, bits int, dyn bool) []byte {
	switch {
	case math.IsNaN(f):
		return append(b, "nan"...)
	case math.IsInf(f, 1):
		return append(b, "inf"...)
	case math.IsInf(f, -1):
		return append(b, "-inf"...)
	}
	start := len(b)
	b = strconv.AppendFloat(b, f, 'g', -1, bits)
	if dyn { // keep it a float when read back as interface{}
		for _, c := range b[start:] {
			if c == '.' || c == 'e' {
				return b
			}
		}
		b = append(b, ".0"...)
	}
	return b
}

// str writes s bare when that reads back unchanged, otherwise quoted.
func (e *tenc) str(s string, dyn bool, q int) {
	if strNeedsQuote(s, dyn, q) {
		e.b = appendQuoted(e.b, s)
	} else {
		e.b = append(e.b, s...)
	}
}

func strNeedsQuote(s string, dyn bool, q int) bool {
	if s == "" {
		return true
	}
	switch s[0] {
	case ' ', '\t', '"', '#', '~':
		return true
	}
	if c := s[len(s)-1]; c == ' ' || c == '\t' {
		return true
	}
	hi := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= utf8.RuneSelf:
			hi = true
		case c < 0x20 || c == 0x7f || c == ';':
			return true
		case c == ' ' && (q != qWhole || dyn):
			return true
		case q == qKey && (c == '.' || c == '"'):
			return true
		}
	}
	if hi && !utf8.ValidString(s) {
		return true
	}
	switch q {
	case qKey:
		return allDigits(s) || s == "+" || s == "^"
	}
	if dyn {
		return s[0] == '-' || dynLooksLiteral(s)
	}
	return false
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// dynLooksLiteral reports whether a bare s would read as a number, bool or
// bytes in an interface{} context.
func dynLooksLiteral(s string) bool {
	if s == "true" || s == "false" {
		return true
	}
	switch c := s[0]; {
	case c >= '0' && c <= '9', c == '+', c == '.':
		return true
	case c == 'n' || c == 'N' || c == 'i' || c == 'I':
		_, ok := parseFloatWord(s)
		return ok
	}
	return false
}

const hexDigits = "0123456789abcdef"

func appendQuoted(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\':
				b = append(b, '\\', c)
			case c == '\n':
				b = append(b, '\\', 'n')
			case c == '\r':
				b = append(b, '\\', 'r')
			case c == '\t':
				b = append(b, '\\', 't')
			case c < 0x20 || c == 0x7f:
				b = append(b, '\\', 'x', hexDigits[c>>4], hexDigits[c&15])
			default:
				b = append(b, c)
			}
			i++
			continue
		}
		r, w := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && w == 1 { // invalid UTF-8 survives as a byte escape
			b = append(b, '\\', 'x', hexDigits[c>>4], hexDigits[c&15])
		} else {
			b = append(b, s[i:i+w]...)
		}
		i += w
	}
	return append(b, '"')
}

// appendKey appends s as a key token: bare when possible.
func appendKey(b []byte, s string) []byte {
	if strNeedsQuote(s, false, qKey) {
		return appendQuoted(b, s)
	}
	return append(b, s...)
}
