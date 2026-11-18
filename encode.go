package codec

import (
	"encoding"
	"encoding/binary"
	"reflect"
	"runtime"
	"unsafe"
)

type enc struct {
	buf  []byte
	w    int
	max  uint64
	fail bool
	err  error
	sc   *scratch
}

func (e *enc) setErr(err error) {
	if e.err == nil {
		e.err = err
	}
	e.fail = true
}

func widthFor(body int64) int {
	switch {
	case body <= 1<<8:
		return 1
	case body <= 1<<16:
		return 2
	}
	return 4
}

func widthCode(w int) byte {
	switch w {
	case 1:
		return 0
	case 2:
		return 1
	}
	return 2
}

// Marshal encodes v (a struct or pointer to struct).
func Marshal(v any) ([]byte, error) { return Append(nil, v) }

// Append encodes v and appends the result to dst.
func Append(dst []byte, v any) ([]byte, error) { return appendValue(dst, v, false) }

// MarshalEvolvable is Marshal for data that will outlive the program version
// that wrote it (files, databases, queues). The message also records how many
// fields each struct had, so a later version of the type with fields added at
// the end of its structs can still read it: the new fields come back as zero.
// It costs one byte per struct type in the message (the header grows from 5
// bytes to 6 + one byte per struct type), and the layout of the data itself
// is unchanged.
//
// Evolution is append-only. Allowed: adding fields at the end of a struct, at
// any nesting depth, including new nested struct types. Not allowed (reading
// returns ErrSchemaMismatch): removing, reordering, renaming or retyping
// fields, and reading a newer message with an older type. Unmarshal and View
// read both plain and evolvable messages; plain ones need an exact schema match.
func MarshalEvolvable(v any) ([]byte, error) { return AppendEvolvable(nil, v) }

// AppendEvolvable is MarshalEvolvable appending to dst.
func AppendEvolvable(dst []byte, v any) ([]byte, error) { return appendValue(dst, v, true) }

func appendValue(dst []byte, v any, evolvable bool) ([]byte, error) {
	pl, p, err := target(v)
	if err != nil {
		return nil, err
	}
	out, err := appendPlan(dst, pl, p, evolvable)
	runtime.KeepAlive(v)
	return out, err
}

func appendPlan(dst []byte, pl *plan, p unsafe.Pointer, evolvable bool) ([]byte, error) {
	sc := scratchPool.Get().(*scratch)
	defer scratchPool.Put(sc)
	base := len(dst)
	w := widthFor(pl.last.Load())
	shrunk := false
	var e enc
	for {
		e = enc{buf: dst[:base], w: w, max: 1<<(8*uint(w)) - 1, sc: sc}
		flag := byte(0)
		if evolvable {
			flag = hdrEvolvable
		}
		e.buf = append(e.buf, 0xC0|flag|widthCode(w), 0, 0, 0, 0)
		binary.LittleEndian.PutUint32(e.buf[base+1:], pl.fp)
		hl := headerLen
		if evolvable {
			e.buf = append(e.buf, pl.shape...)
			hl += len(pl.shape)
		}
		e.frame(pl.root, p)
		if e.err != nil {
			return nil, e.err
		}
		dst = e.buf[:cap(e.buf)][:base] // keep capacity for retries
		if e.fail {
			if w == 4 {
				return nil, ErrTooLarge
			}
			if w == 1 {
				w = 2
			} else {
				w = 4
			}
			continue
		}
		body := int64(len(e.buf) - base - hl)
		if need := widthFor(body); need < w && !shrunk {
			w, shrunk = need, true
			continue
		}
		pl.last.Store(body)
		return e.buf, nil
	}
}

func (e *enc) grow(n int) int {
	l := len(e.buf)
	if cap(e.buf)-l < n {
		nb := make([]byte, l, max(2*cap(e.buf), l+n))
		copy(nb, e.buf)
		e.buf = nb
	}
	e.buf = e.buf[:l+n]
	clear(e.buf[l:])
	return l
}

func (e *enc) putOff(pos, v int) {
	if uint64(v) > e.max {
		e.fail = true
		return
	}
	switch e.w {
	case 1:
		e.buf[pos] = byte(v)
	case 2:
		le.PutUint16(e.buf[pos:], uint16(v))
	default:
		le.PutUint32(e.buf[pos:], uint32(v))
	}
}

func (e *enc) frame(n *node, p unsafe.Pointer) {
	start := len(e.buf)
	e.grow(n.nbm)
	nv := 0
	for i := range n.fields {
		f := &n.fields[i]
		fp := unsafe.Add(p, f.off)
		fn := f.n
		present := false
		switch fn.kind {
		case kBool:
			present = *(*bool)(fp)
		case kInt:
			if v := loadInt(fn.size, fp); v != 0 {
				present = true
				e.buf = binary.AppendUvarint(e.buf, zigzag(v))
			}
		case kUint:
			if v := loadUint(fn.size, fp); v != 0 {
				present = true
				e.buf = binary.AppendUvarint(e.buf, v)
			}
		case kF32:
			if v := *(*uint32)(fp); v != 0 {
				present = true
				e.buf = le.AppendUint32(e.buf, v)
			}
		case kF64:
			if v := *(*uint64)(fp); v != 0 {
				present = true
				e.buf = le.AppendUint64(e.buf, v)
			}
		case kString:
			present = len(*(*string)(fp)) > 0
		case kSlice:
			present = (*sliceHeader)(fp).Len > 0
		case kStruct:
			present = true
		case kPtr:
			present = *(*unsafe.Pointer)(fp) != nil
		case kArray:
			present = fn.alen > 0
		case kMap:
			present = reflect.NewAt(fn.rt, fp).Elem().Len() > 0
		case kIface:
			present = (*[2]unsafe.Pointer)(fp)[0] != nil
		case kBin:
			present = !reflect.NewAt(fn.rt, fp).Elem().IsZero()
		}
		if present {
			e.buf[start+i>>3] |= 1 << (i & 7)
			if fn.isVar() {
				nv++
			}
		}
	}
	if nv == 0 {
		return
	}
	tbl := e.grow(nv * e.w)
	j := 0
	for _, i := range n.varFields {
		if e.buf[start+i>>3]&(1<<(i&7)) == 0 {
			continue
		}
		f := &n.fields[i]
		e.putOff(tbl+j*e.w, len(e.buf)-start)
		j++
		e.value(f.n, unsafe.Add(p, f.off))
		if e.fail {
			return
		}
	}
}

// value encodes the payload of a value at p as a standalone span.
func (e *enc) value(n *node, p unsafe.Pointer) {
	switch n.kind {
	case kBool:
		if *(*bool)(p) {
			e.buf = append(e.buf, 1)
		} else {
			e.buf = append(e.buf, 0)
		}
	case kInt:
		e.buf = binary.AppendUvarint(e.buf, zigzag(loadInt(n.size, p)))
	case kUint:
		e.buf = binary.AppendUvarint(e.buf, loadUint(n.size, p))
	case kF32:
		e.buf = le.AppendUint32(e.buf, *(*uint32)(p))
	case kF64:
		e.buf = le.AppendUint64(e.buf, *(*uint64)(p))
	case kString:
		e.buf = append(e.buf, *(*string)(p)...)
	case kStruct:
		e.frame(n, p)
	case kPtr:
		q := *(*unsafe.Pointer)(p)
		if q == nil {
			return
		}
		if n.elem.kind == kStruct {
			e.frame(n.elem, q)
			return
		}
		e.buf = append(e.buf, 1) // marker: keeps a non-nil pointer to an empty value non-empty
		e.value(n.elem, q)
	case kSlice:
		e.slice(n, p)
	case kArray:
		e.elems(n, p, n.alen)
	case kMap:
		e.mapv(n, p)
	case kIface:
		e.dyn(reflect.NewAt(n.rt, p).Elem(), 0)
	case kBin:
		pv := reflect.NewAt(n.rt, p)
		var b []byte
		var err error
		if n.text {
			b, err = pv.Interface().(encoding.TextMarshaler).MarshalText()
		} else {
			b, err = pv.Interface().(encoding.BinaryMarshaler).MarshalBinary()
		}
		if err != nil {
			e.setErr(err)
			return
		}
		e.buf = append(e.buf, b...)
	}
}

func (e *enc) slice(n *node, p unsafe.Pointer) {
	h := (*sliceHeader)(p)
	e.elems(n, h.Data, h.Len)
}

// elems encodes cnt elements of n.elem stored contiguously at data (the
// backing store of a slice or an array).
func (e *enc) elems(n *node, data unsafe.Pointer, cnt int) {
	el := n.elem
	if cnt == 0 {
		return
	}
	h := struct{ Data unsafe.Pointer }{data}
	switch n.smode {
	case smRaw:
		sz := int(el.size)
		if isLE || sz == 1 {
			e.buf = append(e.buf, unsafe.Slice((*byte)(h.Data), cnt*sz)...)
			return
		}
		for i := 0; i < cnt; i++ {
			q := unsafe.Add(h.Data, uintptr(i)*el.size)
			if sz == 4 {
				e.buf = le.AppendUint32(e.buf, *(*uint32)(q))
			} else {
				e.buf = le.AppendUint64(e.buf, *(*uint64)(q))
			}
		}
	case smVarint:
		e.buf = binary.AppendUvarint(e.buf, uint64(cnt))
		var prev int64
		for i := 0; i < cnt; i++ {
			q := unsafe.Add(h.Data, uintptr(i)*el.size)
			var v int64
			if el.kind == kInt {
				v = loadInt(el.size, q)
			} else {
				v = int64(loadUint(el.size, q))
			}
			e.buf = binary.AppendUvarint(e.buf, zigzag(v-prev))
			prev = v
		}
	default: // smVar, smStruct
		start := len(e.buf)
		if n.smode == smStruct && cnt >= columnarMin && len(el.fields) > 0 {
			e.buf = binary.AppendUvarint(e.buf, uint64(cnt)<<1|1)
			if e.columns(el, h.Data, cnt, start) {
				return
			}
			e.buf = e.buf[:start] // every lane was all-zero: use rows
		}
		e.buf = binary.AppendUvarint(e.buf, uint64(cnt)<<1)
		tbl := e.grow(cnt * e.w)
		for i := 0; i < cnt; i++ {
			e.putOff(tbl+i*e.w, len(e.buf)-start)
			e.value(el, unsafe.Add(h.Data, uintptr(i)*el.size))
			if e.fail {
				return
			}
		}
	}
}
