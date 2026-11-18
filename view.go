package codec

import (
	"errors"
	"math"
	"reflect"
	"unsafe"
)

// Errors returned by the lazy View API.
var (
	ErrNoField  = errors.New("codec: no such field")
	ErrKind     = errors.New("codec: value has a different kind")
	ErrIndex    = errors.New("codec: index out of range")
	ErrNilValue = errors.New("codec: nil pointer")
)

// Value is a lazily decoded position inside an encoded message. Nothing is
// decoded or allocated until you ask: Field, Index and the numeric accessors
// only walk the bytes they need. String and Bytes return copies; the *Ref
// variants return zero-copy views that are valid only while the message is
// alive and unmodified.
//
// Costs: Field is O(number of fields); Index is O(1) for fixed-width and
// row-layout slices and for columnar integer lanes using bit-packing, and
// O(i) for delta-encoded and plain string lanes.
type Value struct {
	n   *node
	w   int
	b   []byte
	num uint64
	ok  bool
	// columnar element: element col of a slice of cnt structs; b is the
	// whole slice payload and k the offset of its lane directory.
	col, cnt, k int
}

// View opens an encoded message for lazy access. schema is a struct or
// pointer to struct of the type the message was encoded from.
func View(data []byte, schema any) (Value, error) {
	t := reflect.TypeOf(schema)
	if t == nil {
		return Value{}, ErrNotStruct
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	pl, err := planFor(t)
	if err != nil {
		return Value{}, err
	}
	pl, body, err := openMessage(data, t, pl)
	if err != nil {
		return Value{}, err
	}
	return Value{n: pl.root, w: 1 << (data[0] & 3), b: body, ok: true, col: -1}, nil
}

// absentValue is the value of a field that the message predates.
func absentValue(rt reflect.Type) (Value, error) {
	b := &builder{memo: map[reflect.Type]*node{}}
	n, err := b.build(rt)
	if err != nil {
		return Value{}, err
	}
	return Value{n: n, w: 1, col: -1}, nil
}

// Present reports whether the value is non-zero in the message. Absent
// fields read as their zero value.
func (v Value) Present() bool { return v.ok }

func (v Value) Int() int64 {
	if v.n != nil && v.n.kind == kInt {
		return int64(v.num)
	}
	return 0
}

func (v Value) Uint() uint64 {
	if v.n != nil && v.n.kind == kUint {
		return v.num
	}
	return 0
}

func (v Value) Float() float64 {
	if v.n == nil {
		return 0
	}
	switch v.n.kind {
	case kF32:
		return float64(math.Float32frombits(uint32(v.num)))
	case kF64:
		return math.Float64frombits(v.num)
	}
	return 0
}

func (v Value) Bool() bool { return v.n != nil && v.n.kind == kBool && v.ok }

func (v Value) isText() bool {
	return v.n != nil && v.ok && len(v.b) > 0 &&
		(v.n.kind == kString || (v.n.kind == kSlice && v.n.smode == smRaw && v.n.elem.size == 1 && v.n.elem.kind == kUint))
}

// String returns a string or []byte value as a new string (a copy, so it is
// safe to keep after the message changes).
func (v Value) String() string {
	if !v.isText() {
		return ""
	}
	return string(v.b)
}

// Bytes returns a copy of a []byte or string value.
func (v Value) Bytes() []byte {
	if !v.isText() {
		return nil
	}
	return append([]byte(nil), v.b...)
}

// StringRef is like String but does not copy: the result points into the
// message, so it is only valid while the message is alive and unmodified.
func (v Value) StringRef() string {
	if !v.isText() {
		return ""
	}
	return unsafe.String(unsafe.SliceData(v.b), len(v.b))
}

// BytesRef is like Bytes but does not copy. The result must be treated as
// read-only and is only valid while the message is alive and unmodified.
func (v Value) BytesRef() []byte {
	if !v.isText() {
		return nil
	}
	return v.b[:len(v.b):len(v.b)]
}

// Len is the length of a string, []byte or slice value.
func (v Value) Len() int {
	if v.n == nil || !v.ok {
		return 0
	}
	switch v.n.kind {
	case kString:
		return len(v.b)
	case kSlice, kArray:
		switch v.n.smode {
		case smRaw:
			return len(v.b) / int(v.n.elem.size)
		case smVarint:
			c, k := readUvarint(v.b)
			if k <= 0 {
				return 0
			}
			return int(min(c, uint64(len(v.b))))
		default:
			hv, k := readUvarint(v.b)
			if k <= 0 {
				return 0
			}
			return int(min(hv>>1, uint64(len(v.b)<<3)))
		}
	}
	return 0
}

func scalar(n *node, num uint64, ok bool) Value { return Value{n: n, num: num, ok: ok, col: -1} }

func (d *dec) viewVar(n *node, span []byte) Value {
	v := Value{n: n, b: span, ok: true, col: -1, w: d.w}
	if len(span) == 0 && (n.kind == kPtr || n.kind == kSlice) {
		v.ok = false
	}
	return v
}

// Field returns the named field of a struct (or pointer to struct) value.
func (v Value) Field(name string) (Value, error) {
	n := v.n
	if n == nil {
		return Value{}, ErrKind
	}
	if n.kind == kPtr {
		if !v.ok {
			return Value{}, ErrNilValue
		}
		n = n.elem
	}
	if n.kind != kStruct {
		return Value{}, ErrKind
	}
	idx := -1
	for i := range n.fields {
		if n.fields[i].name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		for i := range n.extra { // added after the message was written: reads as zero
			if n.extra[i].name == name {
				return absentValue(n.extra[i].rt)
			}
		}
		return Value{}, ErrNoField
	}
	d := dec{w: v.w}
	if v.col >= 0 {
		return d.colField(n, v, idx)
	}
	return d.frameField(n, v.b, idx)
}

func (d *dec) frameField(n *node, b []byte, idx int) (Value, error) {
	if len(b) < n.nbm {
		return Value{}, ErrCorrupt
	}
	pos, nv, jIdx := n.nbm, 0, -1
	var res Value
	for i := range n.fields {
		fn := n.fields[i].n
		present := b[i>>3]&(1<<(i&7)) != 0
		if !present {
			if i == idx {
				res = scalar(fn, 0, false)
				if fn.isVar() {
					res = Value{n: fn, ok: false, col: -1, w: d.w}
				}
			}
			continue
		}
		switch fn.kind {
		case kBool:
			if i == idx {
				res = scalar(fn, 1, true)
			}
		case kInt, kUint:
			u, k := readUvarint(b[pos:])
			if k <= 0 {
				return Value{}, ErrCorrupt
			}
			pos += k
			if i == idx {
				if fn.kind == kInt {
					u = uint64(unzigzag(u))
				}
				res = scalar(fn, u, true)
			}
		case kF32:
			if pos+4 > len(b) {
				return Value{}, ErrCorrupt
			}
			if i == idx {
				res = scalar(fn, uint64(le.Uint32(b[pos:])), true)
			}
			pos += 4
		case kF64:
			if pos+8 > len(b) {
				return Value{}, ErrCorrupt
			}
			if i == idx {
				res = scalar(fn, le.Uint64(b[pos:]), true)
			}
			pos += 8
		default:
			if i == idx {
				jIdx = nv
			}
			nv++
		}
	}
	if jIdx < 0 {
		return res, nil
	}
	tblEnd := pos + nv*d.w
	if tblEnd > len(b) {
		return Value{}, ErrCorrupt
	}
	s := d.off(b, pos+jIdx*d.w)
	e := len(b)
	if jIdx+1 < nv {
		e = d.off(b, pos+(jIdx+1)*d.w)
	}
	if s < tblEnd || s > e || e > len(b) {
		return Value{}, ErrCorrupt
	}
	return d.viewVar(n.fields[idx].n, b[s:e]), nil
}

// Index returns element i of a slice value.
func (v Value) Index(i int) (Value, error) {
	if v.n == nil || (v.n.kind != kSlice && v.n.kind != kArray) {
		return Value{}, ErrKind
	}
	if !v.ok || i < 0 || i >= v.Len() {
		return Value{}, ErrIndex
	}
	n, el, b := v.n, v.n.elem, v.b
	d := dec{w: v.w}
	switch n.smode {
	case smRaw:
		sz := int(el.size)
		q := b[i*sz:]
		switch {
		case el.kind == kBool:
			return scalar(el, uint64(q[0]), q[0] == 1), nil
		case sz == 1:
			return scalar(el, uint64(q[0]), q[0] != 0), nil
		case sz == 4:
			return scalar(el, uint64(le.Uint32(q)), true), nil
		}
		return scalar(el, le.Uint64(q), true), nil
	case smVarint:
		cnt, k := readUvarint(b)
		if k <= 0 || cnt > uint64(len(b)) {
			return Value{}, ErrCorrupt
		}
		var prev int64
		pos := k
		for j := 0; j <= i; j++ {
			u, k := readUvarint(b[pos:])
			if k <= 0 {
				return Value{}, ErrCorrupt
			}
			pos += k
			prev += unzigzag(u)
		}
		return scalar(el, uint64(prev), true), nil
	}
	hv, k := readUvarint(b)
	if k <= 0 {
		return Value{}, ErrCorrupt
	}
	cnt := int(hv >> 1)
	if hv&1 == 1 {
		return Value{n: el, w: v.w, b: b, ok: true, col: i, cnt: cnt, k: k}, nil
	}
	tblEnd := k + cnt*d.w
	if tblEnd > len(b) {
		return Value{}, ErrCorrupt
	}
	s := d.off(b, k+i*d.w)
	e := len(b)
	if i+1 < cnt {
		e = d.off(b, k+(i+1)*d.w)
	}
	if s < tblEnd || s > e || e > len(b) {
		return Value{}, ErrCorrupt
	}
	return d.viewVar(el, b[s:e]), nil
}

// colField reads field idx of columnar element v.col.
func (d *dec) colField(n *node, v Value, idx int) (Value, error) {
	b, nf := v.b, len(n.fields)
	tblEnd := v.k + nf*d.w
	if tblEnd > len(b) {
		return Value{}, ErrCorrupt
	}
	s := d.off(b, v.k+idx*d.w)
	e := len(b)
	if idx+1 < nf {
		e = d.off(b, v.k+(idx+1)*d.w)
	}
	if s < tblEnd || s > e || e > len(b) {
		return Value{}, ErrCorrupt
	}
	fn := n.fields[idx].n
	lane := b[s:e]
	if len(lane) == 0 {
		if fn.isVar() {
			return Value{n: fn, col: -1, w: d.w}, nil
		}
		return scalar(fn, 0, false), nil
	}
	return d.laneAt(fn, lane, v.cnt, v.col)
}

func (d *dec) laneAt(fn *node, lane []byte, cnt, i int) (Value, error) {
	tag, b := lane[0], lane[1:]
	switch fn.kind {
	case kBool:
		if tag != tagPlain || len(b) != (cnt+7)/8 {
			return Value{}, ErrCorrupt
		}
		bit := b[i>>3]>>(uint(i)&7)&1 == 1
		return scalar(fn, 0, bit), nil
	case kInt, kUint, kF32, kF64:
		return d.numAt(fn, tag, b, cnt, i)
	case kString:
		return d.strAt(fn, tag, b, cnt, i)
	}
	// row lane
	if tag != tagPlain || uint64(cnt) > uint64(len(lane)-1)/uint64(d.w) {
		return Value{}, ErrCorrupt
	}
	tblEnd := 1 + cnt*d.w
	s := d.off(lane, 1+i*d.w)
	e := len(lane)
	if i+1 < cnt {
		e = d.off(lane, 1+(i+1)*d.w)
	}
	if s < tblEnd || s > e || e > len(lane) {
		return Value{}, ErrCorrupt
	}
	return d.viewVar(fn, lane[s:e]), nil
}

func (d *dec) numAt(fn *node, tag byte, b []byte, cnt, i int) (Value, error) {
	isInt := fn.kind == kInt || fn.kind == kUint
	fixed := 8
	if fn.kind == kF32 {
		fixed = 4
	}
	switch {
	case tag == tagFOR && isInt:
		mn, k := readUvarint(b)
		if k <= 0 || k >= len(b) {
			return Value{}, ErrCorrupt
		}
		if fn.kind == kInt {
			mn = uint64(unzigzag(mn))
		}
		w := int(b[k])
		packed := b[k+1:]
		if w > 64 || len(packed) != (cnt*w+7)/8 {
			return Value{}, ErrCorrupt
		}
		return scalar(fn, mn+unpack(packed, i*w, w), true), nil
	case tag == tagDelta && isInt:
		pos := 0
		var prev uint64
		for j := 0; j <= i; j++ {
			u, k := readUvarint(b[pos:])
			if k <= 0 {
				return Value{}, ErrCorrupt
			}
			pos += k
			prev += uint64(unzigzag(u))
		}
		return scalar(fn, prev, true), nil
	case tag == tagDFOR && isInt:
		r, err := parseDFOR(b, cnt)
		if err != nil {
			return Value{}, err
		}
		return scalar(fn, r.at(i), true), nil
	case tag == tagPlain && !isInt:
		if len(b) != cnt*fixed {
			return Value{}, ErrCorrupt
		}
		if fixed == 4 {
			return scalar(fn, uint64(le.Uint32(b[i*4:])), true), nil
		}
		return scalar(fn, le.Uint64(b[i*8:]), true), nil
	case tag == tagDict:
		nd, k := readUvarint(b)
		if k <= 0 || nd == 0 || nd > uint64(len(b)) {
			return Value{}, ErrCorrupt
		}
		// Walk entries once to find the index width, remembering offsets of
		// nothing: entries are read again after we know which one we need.
		entryAt := func(pos int) (uint64, int, bool) {
			switch {
			case isInt:
				u, k := readUvarint(b[pos:])
				if k <= 0 {
					return 0, 0, false
				}
				if fn.kind == kInt {
					u = uint64(unzigzag(u))
				}
				return u, k, true
			case pos+fixed > len(b):
				return 0, 0, false
			case fixed == 4:
				return uint64(le.Uint32(b[pos:])), 4, true
			}
			return le.Uint64(b[pos:]), 8, true
		}
		pos := k
		if !isInt { // fixed-width entries: end of dictionary is arithmetic
			if nd > uint64(len(b)/fixed) {
				return Value{}, ErrCorrupt
			}
			pos += int(nd) * fixed
		}
		for e := uint64(0); isInt && e < nd; e++ {
			_, sz, ok := entryAt(pos)
			if !ok {
				return Value{}, ErrCorrupt
			}
			pos += sz
		}
		if pos >= len(b) {
			return Value{}, ErrCorrupt
		}
		w := int(b[pos])
		packed := b[pos+1:]
		if w > 32 || len(packed) != (cnt*w+7)/8 {
			return Value{}, ErrCorrupt
		}
		ix := unpack(packed, i*w, w)
		if ix >= nd {
			return Value{}, ErrCorrupt
		}
		p2 := k
		if isInt {
			for e := uint64(0); e < ix; e++ {
				_, sz, _ := entryAt(p2)
				p2 += sz
			}
		} else {
			p2 += int(ix) * fixed
		}
		val, _, _ := entryAt(p2)
		return scalar(fn, val, true), nil
	}
	return Value{}, ErrCorrupt
}

func (d *dec) strAt(fn *node, tag byte, b []byte, cnt, i int) (Value, error) {
	str := func(s []byte) Value { return Value{n: fn, b: s, ok: len(s) > 0, col: -1} }
	readLen := func(pos int) (int, int, bool) {
		l, k := readUvarint(b[pos:])
		if k <= 0 || l > uint64(len(b)-pos-k) {
			return 0, 0, false
		}
		return int(l), k, true
	}
	switch tag {
	case tagPlain:
		pos := 0
		for j := 0; ; j++ {
			l, k, ok := readLen(pos)
			if !ok {
				return Value{}, ErrCorrupt
			}
			pos += k
			if j == i {
				return str(b[pos : pos+l]), nil
			}
			pos += l
		}
	case tagDict:
		nd, k := readUvarint(b)
		if k <= 0 || nd == 0 || nd > uint64(len(b)) {
			return Value{}, ErrCorrupt
		}
		pos := k
		for e := uint64(0); e < nd; e++ {
			l, k, ok := readLen(pos)
			if !ok {
				return Value{}, ErrCorrupt
			}
			pos += k + l
		}
		if pos >= len(b) {
			return Value{}, ErrCorrupt
		}
		w := int(b[pos])
		packed := b[pos+1:]
		if w > 32 || len(packed) != (cnt*w+7)/8 {
			return Value{}, ErrCorrupt
		}
		ix := unpack(packed, i*w, w)
		if ix >= nd {
			return Value{}, ErrCorrupt
		}
		p2 := k
		for e := uint64(0); e < ix; e++ {
			l, k, _ := readLen(p2)
			p2 += k + l
		}
		l, k2, _ := readLen(p2)
		return str(b[p2+k2 : p2+k2+l]), nil
	}
	return Value{}, ErrCorrupt
}
