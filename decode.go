package codec

import (
	"reflect"
	"sync"
	"unsafe"
)

// MaxElements bounds the element count of a columnar slice. Columnar lanes
// can describe very many elements in few bytes (a constant lane is a few
// bytes), so without a cap a tiny hostile message could request a huge
// allocation. Row-layout slices are bounded by the input length instead.
var MaxElements = 1 << 24

// MaxAlloc bounds the memory one decode may allocate for slices and
// pointees (strings are bounded by the input size). A message exceeding it
// fails with ErrTooLarge. Lower it when decoding untrusted input.
var MaxAlloc int64 = 1 << 28

const maxReusableDecodeScratch = 1 << 14
const maxReusableMapTypes = 16
const maxReusableMapBytes = 16 << 10

type decScratch struct {
	u64  []uint64
	strs []string
	maps map[*node]mapBuffers
}

type sliceArena struct {
	data unsafe.Pointer
	cap  int
	used int
	keep reflect.Value
}

var decScratchPool = sync.Pool{New: func() any { return new(decScratch) }}

type dec struct {
	w        int
	depth    int
	budget   int64
	keep     []uint64       // current pointer-free block (strings and numeric slices are carved from it)
	base     unsafe.Pointer // start of keep
	used     uintptr        // bytes carved so far
	size     uintptr        // bytes in keep
	chunk    int            // minimum size of each block
	u64      []uint64       // dictionary scratch
	strs     []string       // dictionary scratch
	src      []byte         // input, used to avoid reusing aliased output slices
	strArena sliceArena
	arenas   map[*node]sliceArena
	maps     map[*node]mapBuffers
}

// Unmarshal decodes data into v, which must be a non-nil pointer to a value of
// the type that was marshaled (like json.Unmarshal, v can point at any type,
// including a struct, slice, map, scalar or interface{}). Missing fields are
// reset to zero. Existing pointers, slice storage and non-nil maps
// are reused when possible; copy them first if an earlier decoded snapshot
// must remain unchanged. Decoded strings share arena blocks, so a retained
// string keeps its block alive. Malformed input returns an error and never
// panics.

func Unmarshal(data []byte, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return ErrNeedPointer
	}
	// like json.Unmarshal, follow (and allocate) pointers: **T and *T decode alike
	for rv.Elem().Kind() == reflect.Pointer {
		if rv.Elem().IsNil() {
			rv.Elem().Set(reflect.New(rv.Elem().Type().Elem()))
		}
		rv = rv.Elem()
	}
	pl, err := planFor(rv.Type().Elem())
	if err != nil {
		return err
	}
	if len(data) < headerLen || data[0]&0xFC != 0xC0 || data[0]&3 > 2 {
		return ErrBadHeader
	}
	if le.Uint32(data[1:]) != pl.fp {
		return ErrSchemaMismatch
	}
	sc := decScratchPool.Get().(*decScratch)
	// Decoded string bytes cannot exceed the input size; cap the initial arena
	// so a single large message does not force one oversized allocation.
	chunk := min(max(len(data)/2, 512), 16<<10)
	if len(data) < 256 {
		chunk = max(len(data)/2, 64)
	}
	d := dec{w: 1 << (data[0] & 3), budget: MaxAlloc, chunk: chunk, u64: sc.u64, strs: sc.strs, src: data, maps: sc.maps}
	err = d.frame(pl.root, rv.UnsafePointer(), data[headerLen:])
	clear(d.strs) // decoded strings live in d.keep, never in pooled scratch
	if cap(d.u64) <= maxReusableDecodeScratch {
		sc.u64 = d.u64[:0]
	} else {
		sc.u64 = nil
	}
	if cap(d.strs) <= maxReusableDecodeScratch {
		sc.strs = d.strs[:0]
	} else {
		sc.strs = nil
	}
	sc.maps = d.maps
	decScratchPool.Put(sc)
	return err
}

// carve returns n zeroed bytes aligned to align from the decode's current
// pointer-free block, starting a new block when it is full. Everything carved
// shares a block, so a decode costs a handful of allocations instead of one
// per string or slice; slices are capacity-limited so append never writes
// into a neighbour.
func (d *dec) carve(n, align uintptr) unsafe.Pointer {
	off := (d.used + align - 1) &^ (align - 1)
	if off+n > d.size {
		words := (max(n, uintptr(d.chunk)) + 7) / 8
		d.keep = make([]uint64, words)
		d.base = unsafe.Pointer(unsafe.SliceData(d.keep))
		d.size = words * 8
		off = 0
	}
	d.used = off + n
	return unsafe.Add(d.base, off)
}

// str copies b into the block and returns it as a string.
func (d *dec) str(b []byte) string {
	n := len(b)
	if n == 0 {
		return ""
	}
	p := d.carve(uintptr(n), 1)
	copy(unsafe.Slice((*byte)(p), n), b)
	return unsafe.String((*byte)(p), n)
}

func (d *dec) off(b []byte, pos int) int {
	switch d.w {
	case 1:
		return int(b[pos])
	case 2:
		return int(le.Uint16(b[pos:]))
	}
	return int(le.Uint32(b[pos:]))
}

func (d *dec) frame(n *node, p unsafe.Pointer, b []byte) error {
	if d.depth++; d.depth > maxDepth {
		return ErrTooDeep
	}
	err := d.frame0(n, p, b)
	d.depth--
	return err
}

func (d *dec) frame0(n *node, p unsafe.Pointer, b []byte) error {
	if len(b) < n.nbm {
		return ErrCorrupt
	}
	pos := n.nbm
	nv := 0
	for i := range n.fields {
		f := &n.fields[i]
		fn := f.n
		fp := unsafe.Add(p, f.off)
		present := b[i>>3]&(1<<(i&7)) != 0
		if !present {
			zeroAt(fn, fp)
			continue
		}
		switch fn.kind {
		case kBool:
			*(*bool)(fp) = true
		case kInt:
			u, k := readUvarint(b[pos:])
			if k <= 0 {
				return ErrCorrupt
			}
			pos += k
			storeInt(fn.size, fp, unzigzag(u))
		case kUint:
			u, k := readUvarint(b[pos:])
			if k <= 0 {
				return ErrCorrupt
			}
			pos += k
			storeUint(fn.size, fp, u)
		case kF32:
			if pos+4 > len(b) {
				return ErrCorrupt
			}
			*(*uint32)(fp) = le.Uint32(b[pos:])
			pos += 4
		case kF64:
			if pos+8 > len(b) {
				return ErrCorrupt
			}
			*(*uint64)(fp) = le.Uint64(b[pos:])
			pos += 8
		default:
			if fn.kind == kArray || fn.kind == kBin {
				zeroAt(fn, fp)
			}
			nv++
		}
	}
	if nv == 0 {
		return nil
	}
	tblEnd := pos + nv*d.w
	if tblEnd > len(b) {
		return ErrCorrupt
	}
	j := 0
	for _, i := range n.varFields {
		if b[i>>3]&(1<<(i&7)) == 0 {
			continue
		}
		f := &n.fields[i]
		s := d.off(b, pos+j*d.w)
		e := len(b)
		if j+1 < nv {
			e = d.off(b, pos+(j+1)*d.w)
		}
		if s < tblEnd || s > e || e > len(b) {
			return ErrCorrupt
		}
		if err := d.value(f.n, unsafe.Add(p, f.off), b[s:e]); err != nil {
			return err
		}
		j++
	}
	return nil
}

func zeroAt(n *node, p unsafe.Pointer) {
	switch n.kind {
	case kBool:
		*(*bool)(p) = false
	case kInt:
		storeInt(n.size, p, 0)
	case kUint:
		storeUint(n.size, p, 0)
	case kF32:
		*(*uint32)(p) = 0
	case kF64:
		*(*uint64)(p) = 0
	case kString:
		*(*string)(p) = ""
	case kPtr, kMap:
		q := (*unsafe.Pointer)(p)
		if *q != nil {
			*q = nil
		}
	case kSlice:
		h := (*sliceHeader)(p)
		if h.Data != nil {
			*h = sliceHeader{}
		}
	case kIface:
		*(*any)(p) = nil
	default:
		reflect.NewAt(n.rt, p).Elem().SetZero()
	}
}

func (d *dec) value(n *node, p unsafe.Pointer, b []byte) error {
	switch n.kind {
	case kBool:
		if len(b) != 1 || b[0] > 1 {
			return ErrCorrupt
		}
		*(*bool)(p) = b[0] == 1
	case kInt, kUint:
		u, k := readUvarint(b)
		if k <= 0 || k != len(b) {
			return ErrCorrupt
		}
		if n.kind == kInt {
			storeInt(n.size, p, unzigzag(u))
		} else {
			storeUint(n.size, p, u)
		}
	case kF32:
		if len(b) != 4 {
			return ErrCorrupt
		}
		*(*uint32)(p) = le.Uint32(b)
	case kF64:
		if len(b) != 8 {
			return ErrCorrupt
		}
		*(*uint64)(p) = le.Uint64(b)
	case kString:
		*(*string)(p) = d.str(b)
	case kStruct:
		return d.frame(n, p, b)
	case kPtr:
		if len(b) == 0 {
			zeroAt(n, p)
			return nil
		}
		if d.depth++; d.depth > maxDepth {
			return ErrTooDeep
		}
		q := *(*unsafe.Pointer)(p)
		if q == nil {
			q = reflect.New(n.elem.rt).UnsafePointer()
		}
		var err error
		if n.elem.kind == kStruct {
			err = d.frame(n.elem, q, b)
		} else if b[0] != 1 {
			err = ErrCorrupt
		} else {
			err = d.value(n.elem, q, b[1:])
		}
		d.depth--
		if err != nil {
			return err
		}
		*(*unsafe.Pointer)(p) = q
	case kSlice, kArray:
		if len(b) == 0 {
			zeroAt(n, p)
			return nil
		}
		if d.depth++; d.depth > maxDepth {
			return ErrTooDeep
		}
		err := d.slice(n, p, b)
		d.depth--
		return err
	case kMap:
		if len(b) == 0 {
			zeroAt(n, p)
			return nil
		}
		if d.depth++; d.depth > maxDepth {
			return ErrTooDeep
		}
		err := d.mapv(n, p, b)
		d.depth--
		return err
	case kIface:
		if len(b) == 0 {
			zeroAt(n, p)
			return nil
		}
		v, used, err := d.dyn(b, 0)
		if err != nil {
			return err
		}
		if used != len(b) {
			return ErrCorrupt
		}
		*(*any)(p) = v
	case kBin:
		return d.bin(n, p, b)
	}
	return nil
}

// storage returns where cnt elements of n go: a fresh slice for slices, the
// array itself for arrays (whose length must match).
func (d *dec) storage(n *node, p unsafe.Pointer, cnt int) (unsafe.Pointer, error) {
	if n.kind == kArray {
		if cnt != n.alen {
			return nil, ErrCorrupt
		}
		return p, nil
	}
	return d.makeSlice(n, p, cnt)
}

func mk[T any](p unsafe.Pointer, n int) unsafe.Pointer {
	s := make([]T, n)
	*(*[]T)(p) = s
	return unsafe.Pointer(unsafe.SliceData(s))
}

// makeSlice allocates a slice of cnt elements
// at *p and returns its data pointer.
func (d *dec) makeSlice(n *node, p unsafe.Pointer, cnt int) (unsafe.Pointer, error) {
	if data, ok := d.reuseSlice(n, p, cnt); ok {
		return data, nil
	}
	switch n.elem.kind {
	case kBool, kInt, kUint, kF32, kF64:
		if err := d.charge(cnt, n.elem.size); err != nil {
			return nil, err
		}
		data := d.carve(uintptr(cnt)*n.elem.size, n.elem.size)
		*(*sliceHeader)(p) = sliceHeader{Data: data, Len: cnt, Cap: cnt}
		return data, nil
	}
	if cnt > 0 && n.elem.size > 0 {
		return d.arenaSlice(n, p, cnt)
	}
	if err := d.charge(cnt, n.elem.size); err != nil {
		return nil, err
	}
	return makeSliceRaw(n, p, cnt), nil
}

// arenaSlice packs pointerful slice backing arrays into typed per-decode
// chunks. Each returned slice gets a disjoint, capped range; the slice itself
// keeps its chunk alive after Unmarshal returns.
func (d *dec) arenaSlice(n *node, p unsafe.Pointer, cnt int) (unsafe.Pointer, error) {
	if n.elem.kind == kString {
		a := d.strArena
		if a.cap-a.used < cnt {
			chunk := max(cnt, min(256, max(1, 4096/int(n.elem.size))))
			if err := d.charge(chunk, n.elem.size); err != nil {
				return nil, err
			}
			keep := reflect.MakeSlice(n.rt, chunk, chunk)
			a = sliceArena{data: unsafe.Pointer(keep.Pointer()), cap: chunk, keep: keep}
		}
		start := a.used
		a.used += cnt
		data := unsafe.Add(a.data, uintptr(start)*n.elem.size)
		*(*sliceHeader)(p) = sliceHeader{Data: data, Len: cnt, Cap: cnt}
		d.strArena = a
		return data, nil
	}
	if d.arenas == nil {
		d.arenas = make(map[*node]sliceArena)
	}
	a := d.arenas[n]
	if a.cap-a.used < cnt {
		chunk := max(cnt, min(256, max(1, 4096/int(n.elem.size))))
		if err := d.charge(chunk, n.elem.size); err != nil {
			return nil, err
		}
		keep := reflect.MakeSlice(n.rt, chunk, chunk)
		a = sliceArena{data: unsafe.Pointer(keep.Pointer()), cap: chunk, keep: keep}
	}
	start := a.used
	a.used += cnt
	data := unsafe.Add(a.data, uintptr(start)*n.elem.size)
	*(*sliceHeader)(p) = sliceHeader{Data: data, Len: cnt, Cap: cnt}
	d.arenas[n] = a
	return data, nil
}

func (d *dec) reuseSlice(n *node, p unsafe.Pointer, cnt int) (unsafe.Pointer, bool) {
	v := reflect.NewAt(n.rt, p).Elem()
	if v.Cap() < cnt || d.aliasesInput(n, p) {
		return nil, false
	}
	if cnt < v.Len() {
		v.Slice(cnt, v.Len()).Clear()
	}
	v.SetLen(cnt)
	// Every successful slice decoder writes every element. Struct frames and
	// empty pointer spans explicitly reset omitted or nil values, so retaining
	// the old backing array here lets nested slices reuse their own storage.
	return (*sliceHeader)(p).Data, true
}

func (d *dec) aliasesInput(n *node, p unsafe.Pointer) bool {
	if len(d.src) == 0 {
		return false
	}
	h := (*sliceHeader)(p)
	if h.Data == nil || h.Cap == 0 {
		return false
	}
	outStart := uintptr(h.Data)
	bytes := uintptr(h.Cap) * n.elem.size
	outEnd := outStart + bytes
	inStart := uintptr(unsafe.Pointer(unsafe.SliceData(d.src)))
	inEnd := inStart + uintptr(len(d.src))
	if outEnd < outStart || inEnd < inStart {
		return true
	}
	return outStart < inEnd && inStart < outEnd
}

func (d *dec) charge(cnt int, size uintptr) error {
	c := int64(cnt) * int64(size)
	if c < 0 || c > d.budget {
		return ErrTooLarge
	}
	d.budget -= c
	return nil
}

func makeSliceRaw(n *node, p unsafe.Pointer, cnt int) unsafe.Pointer {
	el := n.elem
	switch el.kind {
	case kBool:
		return mk[bool](p, cnt)
	case kInt:
		switch el.size {
		case 1:
			return mk[int8](p, cnt)
		case 2:
			return mk[int16](p, cnt)
		case 4:
			return mk[int32](p, cnt)
		}
		return mk[int64](p, cnt)
	case kUint:
		switch el.size {
		case 1:
			return mk[uint8](p, cnt)
		case 2:
			return mk[uint16](p, cnt)
		case 4:
			return mk[uint32](p, cnt)
		}
		return mk[uint64](p, cnt)
	case kF32:
		return mk[float32](p, cnt)
	case kF64:
		return mk[float64](p, cnt)
	case kString:
		return mk[string](p, cnt)
	}
	v := reflect.MakeSlice(n.rt, cnt, cnt)
	reflect.NewAt(n.rt, p).Elem().Set(v)
	return v.UnsafePointer()
}

func (d *dec) slice(n *node, p unsafe.Pointer, b []byte) error {
	el := n.elem
	switch n.smode {
	case smRaw:
		sz := int(el.size)
		if len(b)%sz != 0 {
			return ErrCorrupt
		}
		cnt := len(b) / sz
		if el.kind == kBool {
			for _, c := range b {
				if c > 1 {
					return ErrCorrupt
				}
			}
		}
		data, err := d.storage(n, p, cnt)
		if err != nil {
			return err
		}
		if isLE || sz == 1 {
			copy(unsafe.Slice((*byte)(data), len(b)), b)
			return nil
		}
		for i := 0; i < cnt; i++ {
			q := unsafe.Add(data, uintptr(i)*el.size)
			if sz == 4 {
				*(*uint32)(q) = le.Uint32(b[i*4:])
			} else {
				*(*uint64)(q) = le.Uint64(b[i*8:])
			}
		}
	case smVarint:
		cnt, k := readUvarint(b)
		if k <= 0 || cnt > uint64(len(b)-k) {
			return ErrCorrupt
		}
		pos := k
		data, err := d.storage(n, p, int(cnt))
		if err != nil {
			return err
		}
		var prev int64
		for i := 0; i < int(cnt); i++ {
			u, k := readUvarint(b[pos:])
			if k <= 0 {
				return ErrCorrupt
			}
			pos += k
			prev += unzigzag(u)
			q := unsafe.Add(data, uintptr(i)*el.size)
			if el.kind == kInt {
				storeInt(el.size, q, prev)
			} else {
				storeUint(el.size, q, uint64(prev))
			}
		}
	default:
		hv, k := readUvarint(b)
		if k <= 0 {
			return ErrCorrupt
		}
		cnt := hv >> 1
		if hv&1 == 1 {
			if n.smode != smStruct || cnt > uint64(MaxElements) {
				return ErrCorrupt
			}
			return d.columns(n, p, b, k, int(cnt))
		}
		if cnt > uint64(len(b)-k)/uint64(d.w) {
			return ErrCorrupt
		}
		return d.rows(n, p, b, k, int(cnt))
	}
	return nil
}

func (d *dec) rows(n *node, p unsafe.Pointer, b []byte, k, c int) error {
	el := n.elem
	tblEnd := k + c*d.w
	span := func(i int) (int, int, bool) {
		s := d.off(b, k+i*d.w)
		e := len(b)
		if i+1 < c {
			e = d.off(b, k+(i+1)*d.w)
		}
		return s, e, s >= tblEnd && s <= e && e <= len(b)
	}
	// Pointer elements: validate spans, count the live ones and allocate
	// all pointees as one block.
	var block unsafe.Pointer
	var bi uintptr
	if el.kind == kPtr && el.elem.kind == kStruct {
		live := 0
		for i := 0; i < c; i++ {
			s, e, ok := span(i)
			if !ok {
				return ErrCorrupt
			}
			if e > s {
				live++
			}
		}
		if live > 0 {
			if err := d.charge(live, el.elem.size); err != nil {
				return err
			}
			block = reflect.MakeSlice(el.batchT, live, live).UnsafePointer()
		}
	}
	data, err := d.storage(n, p, c)
	if err != nil {
		return err
	}
	for i := 0; i < c; i++ {
		s, e, ok := span(i)
		if !ok {
			return ErrCorrupt
		}
		ep := unsafe.Add(data, uintptr(i)*el.size)
		if el.kind == kPtr && el.elem.kind == kStruct {
			if e == s {
				*(*unsafe.Pointer)(ep) = nil
				continue
			}
			q := unsafe.Add(block, bi*el.elem.size)
			bi++
			if err := d.frame(el.elem, q, b[s:e]); err != nil {
				return err
			}
			*(*unsafe.Pointer)(ep) = q
			continue
		}
		if err := d.value(el, ep, b[s:e]); err != nil {
			return err
		}
	}
	return nil
}
