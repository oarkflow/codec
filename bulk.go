package codec

import (
	"math"
	"slices"
	"unsafe"
)

// Bulk reads. Value.Ints/Uints/Floats/Bools/Strings decode a whole slice of
// scalars in one pass, and Value.Column does the same for one field of a
// slice of structs (decoding that column's lane once instead of walking
// element by element). All of them append to dst, so dst[:0] can be reused;
// strings are copied (use StringsRef for zero-copy views).

// Column is one field of a slice of structs, readable in bulk.
type Column struct {
	fn   *node
	lane []byte // columnar lane; empty means all zero
	cnt  int
	w    int
	rows *Value // row-layout slice: fall back to per-element reads
	name string
}

// Column returns the named field of every element of a slice of structs.
func (v Value) Column(name string) (Column, error) {
	if v.n == nil || v.n.kind != kSlice || v.n.smode != smStruct {
		return Column{}, ErrKind
	}
	el := v.n.elem
	idx := -1
	for i := range el.fields {
		if el.fields[i].name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Column{}, ErrNoField
	}
	fn := el.fields[idx].n
	if !v.ok {
		return Column{fn: fn, w: v.w}, nil
	}
	hv, k := readUvarint(v.b)
	if k <= 0 {
		return Column{}, ErrCorrupt
	}
	if hv&1 == 0 {
		vv := v
		return Column{fn: fn, cnt: v.Len(), w: v.w, rows: &vv, name: name}, nil
	}
	d := dec{w: v.w}
	b, nf := v.b, len(el.fields)
	cnt := hv >> 1
	if cnt > uint64(MaxElements) {
		return Column{}, ErrCorrupt
	}
	tblEnd := k + nf*d.w
	if tblEnd > len(b) {
		return Column{}, ErrCorrupt
	}
	s := d.off(b, k+idx*d.w)
	e := len(b)
	if idx+1 < nf {
		e = d.off(b, k+(idx+1)*d.w)
	}
	if s < tblEnd || s > e || e > len(b) {
		return Column{}, ErrCorrupt
	}
	return Column{fn: fn, lane: b[s:e], cnt: int(cnt), w: d.w}, nil
}

// Len is the number of elements.
func (c Column) Len() int { return c.cnt }

func grow[T any](dst []T, n int) ([]T, []T) {
	l := len(dst)
	dst = slices.Grow(dst, n)[:l+n]
	return dst, dst[l:]
}

func isNum(k kind) bool { return k == kInt || k == kUint || k == kF32 || k == kF64 }

// Ints appends the column's values as int64 (uint fields are converted).
func (c Column) Ints(dst []int64) ([]int64, error) {
	if c.fn.kind != kInt && c.fn.kind != kUint {
		return dst, ErrKind
	}
	dst, out := grow(dst, c.cnt)
	return dst, c.nums(unsafe.Slice((*uint64)(unsafe.Pointer(unsafe.SliceData(out))), len(out)))
}

// Uints appends the column's values as uint64.
func (c Column) Uints(dst []uint64) ([]uint64, error) {
	if c.fn.kind != kInt && c.fn.kind != kUint {
		return dst, ErrKind
	}
	dst, out := grow(dst, c.cnt)
	return dst, c.nums(out)
}

// Floats appends the column's values as float64 (float32 fields are widened).
func (c Column) Floats(dst []float64) ([]float64, error) {
	if c.fn.kind != kF32 && c.fn.kind != kF64 {
		return dst, ErrKind
	}
	dst, out := grow(dst, c.cnt)
	bits := unsafe.Slice((*uint64)(unsafe.Pointer(unsafe.SliceData(out))), len(out))
	if err := c.nums(bits); err != nil {
		return dst, err
	}
	if c.fn.kind == kF32 {
		for j, b := range bits {
			out[j] = float64(math.Float32frombits(uint32(b)))
		}
	}
	return dst, nil
}

// Bools appends the column's values.
func (c Column) Bools(dst []bool) ([]bool, error) {
	if c.fn.kind != kBool {
		return dst, ErrKind
	}
	dst, out := grow(dst, c.cnt)
	if c.rows != nil {
		return dst, c.each(func(j int, f Value) { out[j] = f.Bool() })
	}
	if len(c.lane) == 0 {
		clear(out)
		return dst, nil
	}
	b := c.lane
	if b[0] != tagPlain || len(b)-1 != (c.cnt+7)/8 {
		return dst, ErrCorrupt
	}
	b = b[1:]
	for j := range out {
		out[j] = b[j>>3]>>(uint(j)&7)&1 == 1
	}
	return dst, nil
}

// Strings appends the column's values as copies. A dictionary-encoded column
// is copied once per distinct value.
func (c Column) Strings(dst []string) ([]string, error) { return c.strings(dst, true) }

// StringsRef is like Strings but the strings point into the message: they are
// valid only while the message is alive and unmodified.
func (c Column) StringsRef(dst []string) ([]string, error) { return c.strings(dst, false) }

func (c Column) strings(dst []string, cp bool) ([]string, error) {
	if c.fn.kind != kString {
		return dst, ErrKind
	}
	dst, out := grow(dst, c.cnt)
	if c.rows != nil {
		return dst, c.each(func(j int, f Value) {
			if cp {
				out[j] = f.String()
			} else {
				out[j] = f.StringRef()
			}
		})
	}
	if len(c.lane) == 0 {
		clear(out)
		return dst, nil
	}
	return dst, laneStrings(c.lane, c.cnt, out, cp)
}

// each reads the field of every element of a row-layout slice.
func (c Column) each(fn func(j int, f Value)) error {
	for j := 0; j < c.cnt; j++ {
		e, err := c.rows.Index(j)
		if err != nil {
			return err
		}
		f, err := e.Field(c.name)
		if err != nil {
			return err
		}
		fn(j, f)
	}
	return nil
}

// nums decodes the column as raw 64-bit patterns: sign-extended ints,
// zero-extended uints, float bits (float32 bits in the low half).
func (c Column) nums(out []uint64) error {
	if c.rows != nil {
		return c.each(func(j int, f Value) { out[j] = f.num })
	}
	if len(c.lane) == 0 {
		clear(out)
		return nil
	}
	return laneNums(c.fn, c.lane, c.cnt, out)
}

func laneNums(fn *node, lane []byte, cnt int, out []uint64) error {
	tag, b := lane[0], lane[1:]
	isInt := fn.kind == kInt || fn.kind == kUint
	fixed := 8
	if fn.kind == kF32 {
		fixed = 4
	}
	switch {
	case tag == tagFOR && isInt:
		mn, k := readUvarint(b)
		if k <= 0 || k >= len(b) {
			return ErrCorrupt
		}
		if fn.kind == kInt {
			mn = uint64(unzigzag(mn))
		}
		w := int(b[k])
		packed := b[k+1:]
		if w > 64 || len(packed) != (cnt*w+7)/8 {
			return ErrCorrupt
		}
		for j := range out {
			out[j] = mn + unpack(packed, j*w, w)
		}
	case tag == tagDelta && isInt:
		pos := 0
		var prev uint64
		for j := range out {
			u, k := readUvarint(b[pos:])
			if k <= 0 {
				return ErrCorrupt
			}
			pos += k
			prev += uint64(unzigzag(u))
			out[j] = prev
		}
	case tag == tagDFOR && isInt:
		r, err := parseDFOR(b, cnt)
		if err != nil {
			return err
		}
		cur := r.first
		out[0] = cur
		for j := 1; j < cnt; j++ {
			cur += r.dmin + unpack(r.packed, (j-1)*r.w, r.w)
			out[j] = cur
		}
	case tag == tagPlain && !isInt:
		if len(b) != cnt*fixed {
			return ErrCorrupt
		}
		for j := range out {
			if fixed == 4 {
				out[j] = uint64(le.Uint32(b[j*4:]))
			} else {
				out[j] = le.Uint64(b[j*8:])
			}
		}
	case tag == tagDict:
		nd, k := readUvarint(b)
		if k <= 0 || nd == 0 || nd > uint64(len(b)) {
			return ErrCorrupt
		}
		dict := make([]uint64, 0, nd)
		pos := k
		for i := uint64(0); i < nd; i++ {
			var v uint64
			switch {
			case isInt:
				u, k := readUvarint(b[pos:])
				if k <= 0 {
					return ErrCorrupt
				}
				pos += k
				v = u
				if fn.kind == kInt {
					v = uint64(unzigzag(u))
				}
			case pos+fixed > len(b):
				return ErrCorrupt
			case fixed == 4:
				v = uint64(le.Uint32(b[pos:]))
				pos += 4
			default:
				v = le.Uint64(b[pos:])
				pos += 8
			}
			dict = append(dict, v)
		}
		if pos >= len(b) {
			return ErrCorrupt
		}
		w := int(b[pos])
		packed := b[pos+1:]
		if w > 32 || len(packed) != (cnt*w+7)/8 {
			return ErrCorrupt
		}
		for j := range out {
			ix := unpack(packed, j*w, w)
			if ix >= nd {
				return ErrCorrupt
			}
			out[j] = dict[ix]
		}
	default:
		return ErrCorrupt
	}
	return nil
}

func laneStrings(lane []byte, cnt int, out []string, cp bool) error {
	tag, b := lane[0], lane[1:]
	view := func(s []byte) string {
		if cp {
			return string(s)
		}
		return unsafe.String(unsafe.SliceData(s), len(s))
	}
	switch tag {
	case tagPlain:
		pos := 0
		for j := range out {
			l, k := readUvarint(b[pos:])
			if k <= 0 || l > uint64(len(b)-pos-k) {
				return ErrCorrupt
			}
			pos += k
			out[j] = view(b[pos : pos+int(l)])
			pos += int(l)
		}
	case tagDict:
		nd, k := readUvarint(b)
		if k <= 0 || nd == 0 || nd > uint64(len(b)) {
			return ErrCorrupt
		}
		dict := make([]string, 0, nd)
		pos := k
		for i := uint64(0); i < nd; i++ {
			l, k := readUvarint(b[pos:])
			if k <= 0 || l > uint64(len(b)-pos-k) {
				return ErrCorrupt
			}
			pos += k
			dict = append(dict, view(b[pos:pos+int(l)]))
			pos += int(l)
		}
		if pos >= len(b) {
			return ErrCorrupt
		}
		w := int(b[pos])
		packed := b[pos+1:]
		if w > 32 || len(packed) != (cnt*w+7)/8 {
			return ErrCorrupt
		}
		for j := range out {
			ix := unpack(packed, j*w, w)
			if ix >= nd {
				return ErrCorrupt
			}
			out[j] = dict[ix]
		}
	default:
		return ErrCorrupt
	}
	return nil
}

// --- whole-slice reads of scalar slices ([]int64, []float64, []string, ...) ---

func (v Value) sliceOf(kinds ...kind) (*node, error) {
	if v.n == nil || (v.n.kind != kSlice && v.n.kind != kArray) {
		return nil, ErrKind
	}
	for _, k := range kinds {
		if v.n.elem.kind == k {
			return v.n.elem, nil
		}
	}
	return nil, ErrKind
}

// Ints appends all elements of a slice of integers as int64.
func (v Value) Ints(dst []int64) ([]int64, error) {
	if _, err := v.sliceOf(kInt, kUint); err != nil {
		return dst, err
	}
	n := v.Len()
	dst, out := grow(dst, n)
	if n == 0 {
		return dst, nil
	}
	switch v.n.smode {
	case smRaw: // []uint8
		for j := range out {
			out[j] = int64(v.b[j])
		}
	default:
		cnt, k := readUvarint(v.b)
		if k <= 0 || cnt > uint64(len(v.b)-k) {
			return dst, ErrCorrupt
		}
		pos := k
		var prev int64
		for j := range out {
			u, k := readUvarint(v.b[pos:])
			if k <= 0 {
				return dst, ErrCorrupt
			}
			pos += k
			prev += unzigzag(u)
			out[j] = prev
		}
	}
	return dst, nil
}

// Uints appends all elements of a slice of integers as uint64.
func (v Value) Uints(dst []uint64) ([]uint64, error) {
	ints, err := v.Ints(nil)
	if err != nil {
		return dst, err
	}
	dst = slices.Grow(dst, len(ints))
	for _, x := range ints {
		dst = append(dst, uint64(x))
	}
	return dst, nil
}

// Floats appends all elements of a []float32 or []float64 as float64.
func (v Value) Floats(dst []float64) ([]float64, error) {
	el, err := v.sliceOf(kF32, kF64)
	if err != nil {
		return dst, err
	}
	n := v.Len()
	dst, out := grow(dst, n)
	if len(v.b) != n*int(el.size) {
		return dst, ErrCorrupt
	}
	for j := range out {
		if el.kind == kF32 {
			out[j] = float64(math.Float32frombits(le.Uint32(v.b[j*4:])))
		} else {
			out[j] = math.Float64frombits(le.Uint64(v.b[j*8:]))
		}
	}
	return dst, nil
}

// Bools appends all elements of a []bool.
func (v Value) Bools(dst []bool) ([]bool, error) {
	if _, err := v.sliceOf(kBool); err != nil {
		return dst, err
	}
	n := v.Len()
	dst, out := grow(dst, n)
	for j := range out {
		out[j] = v.b[j] == 1
	}
	return dst, nil
}

// Strings appends all elements of a []string as copies.
func (v Value) Strings(dst []string) ([]string, error) { return v.strings(dst, true) }

// StringsRef is like Strings but zero-copy (see Value.StringRef).
func (v Value) StringsRef(dst []string) ([]string, error) { return v.strings(dst, false) }

func (v Value) strings(dst []string, cp bool) ([]string, error) {
	if _, err := v.sliceOf(kString); err != nil {
		return dst, err
	}
	n := v.Len()
	dst, out := grow(dst, n)
	for j := range out {
		e, err := v.Index(j)
		if err != nil {
			return dst, err
		}
		if cp {
			out[j] = e.String()
		} else {
			out[j] = e.StringRef()
		}
	}
	return dst, nil
}
