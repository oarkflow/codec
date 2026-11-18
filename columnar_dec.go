package codec

import (
	"reflect"
	"unsafe"
)

func unpack(b []byte, pos, w int) uint64 {
	if w == 0 {
		return 0
	}
	idx, sh := pos>>3, uint(pos&7)
	var x uint64
	if idx+8 <= len(b) {
		x = le.Uint64(b[idx:]) >> sh
	} else {
		for k := 0; idx+k < len(b) && k < 8; k++ {
			x |= uint64(b[idx+k]) << (8 * uint(k))
		}
		x >>= sh
	}
	if int(sh)+w > 64 {
		x |= uint64(b[idx+8]) << (64 - sh)
	}
	if w < 64 {
		x &= 1<<uint(w) - 1
	}
	return x
}

func storeNum(fn *node, q unsafe.Pointer, v uint64) {
	switch fn.kind {
	case kInt:
		storeInt(fn.size, q, int64(v))
	case kUint:
		storeUint(fn.size, q, v)
	case kF32:
		*(*uint32)(q) = uint32(v)
	default:
		*(*uint64)(q) = v
	}
}

func (d *dec) columns(n *node, p unsafe.Pointer, b []byte, k, cnt int) error {
	el := n.elem
	nf := len(el.fields)
	tblEnd := k + nf*d.w
	if nf == 0 || tblEnd > len(b) {
		return ErrCorrupt
	}
	data, err := d.storage(n, p, cnt)
	if err != nil {
		return err
	}
	if el.extra != nil {
		for j := 0; j < cnt; j++ {
			zeroExtra(el, unsafe.Add(data, uintptr(j)*el.size))
		}
	}
	any := false
	for i := range el.fields {
		s := d.off(b, k+i*d.w)
		e := len(b)
		if i+1 < nf {
			e = d.off(b, k+(i+1)*d.w)
		}
		if s < tblEnd || s > e || e > len(b) {
			return ErrCorrupt
		}
		f := &el.fields[i]
		base := unsafe.Add(data, f.off)
		lane := b[s:e]
		if len(lane) == 0 {
			for j := 0; j < cnt; j++ {
				zeroAt(f.n, unsafe.Add(base, uintptr(j)*el.size))
			}
			continue
		}
		any = true
		switch f.n.kind {
		case kBool:
			err = d.boolLane(base, el.size, cnt, lane)
		case kInt, kUint, kF32, kF64:
			err = d.numLane(f.n, base, el.size, cnt, lane)
		case kString:
			err = d.stringLane(base, el.size, cnt, lane)
		default:
			err = d.rowLane(f.n, base, el.size, cnt, lane)
		}
		if err != nil {
			return err
		}
	}
	if !any {
		return ErrCorrupt
	}
	return nil
}

func (d *dec) boolLane(base unsafe.Pointer, stride uintptr, cnt int, b []byte) error {
	if b[0] != tagPlain || len(b)-1 != (cnt+7)/8 {
		return ErrCorrupt
	}
	b = b[1:]
	for j := 0; j < cnt; j++ {
		*(*bool)(unsafe.Add(base, uintptr(j)*stride)) = b[j>>3]>>(uint(j)&7)&1 == 1
	}
	return nil
}

func (d *dec) numLane(fn *node, base unsafe.Pointer, stride uintptr, cnt int, b []byte) error {
	tag := b[0]
	b = b[1:]
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
		for j := 0; j < cnt; j++ {
			storeNum(fn, unsafe.Add(base, uintptr(j)*stride), mn+unpack(packed, j*w, w))
		}
	case tag == tagDelta && isInt:
		pos := 0
		var prev uint64
		for j := 0; j < cnt; j++ {
			u, k := readUvarint(b[pos:])
			if k <= 0 {
				return ErrCorrupt
			}
			pos += k
			prev += uint64(unzigzag(u))
			storeNum(fn, unsafe.Add(base, uintptr(j)*stride), prev)
		}
	case tag == tagDFOR && isInt:
		r, err := parseDFOR(b, cnt)
		if err != nil {
			return err
		}
		cur := r.first
		storeNum(fn, base, cur)
		for j := 1; j < cnt; j++ {
			cur += r.dmin + unpack(r.packed, (j-1)*r.w, r.w)
			storeNum(fn, unsafe.Add(base, uintptr(j)*stride), cur)
		}
	case tag == tagPlain && !isInt:
		if len(b) != cnt*fixed {
			return ErrCorrupt
		}
		for j := 0; j < cnt; j++ {
			var v uint64
			if fixed == 4 {
				v = uint64(le.Uint32(b[j*4:]))
			} else {
				v = le.Uint64(b[j*8:])
			}
			storeNum(fn, unsafe.Add(base, uintptr(j)*stride), v)
		}
	case tag == tagDict:
		nd, k := readUvarint(b)
		if k <= 0 || nd == 0 || nd > uint64(len(b)) {
			return ErrCorrupt
		}
		pos := k
		d.u64 = d.u64[:0]
		if cap(d.u64) < int(nd) {
			d.u64 = make([]uint64, 0, int(nd))
		}
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
			d.u64 = append(d.u64, v)
		}
		if pos >= len(b) {
			return ErrCorrupt
		}
		w := int(b[pos])
		packed := b[pos+1:]
		if w > 32 || len(packed) != (cnt*w+7)/8 {
			return ErrCorrupt
		}
		for j := 0; j < cnt; j++ {
			ix := unpack(packed, j*w, w)
			if ix >= nd {
				return ErrCorrupt
			}
			storeNum(fn, unsafe.Add(base, uintptr(j)*stride), d.u64[ix])
		}
	default:
		return ErrCorrupt
	}
	return nil
}

func (d *dec) stringLane(base unsafe.Pointer, stride uintptr, cnt int, b []byte) error {
	tag := b[0]
	b = b[1:]
	switch tag {
	case tagPlain:
		pos := 0
		for j := 0; j < cnt; j++ {
			l, k := readUvarint(b[pos:])
			if k <= 0 || l > uint64(len(b)-pos-k) {
				return ErrCorrupt
			}
			pos += k
			*(*string)(unsafe.Add(base, uintptr(j)*stride)) = d.str(b[pos : pos+int(l)])
			pos += int(l)
		}
	case tagDict:
		nd, k := readUvarint(b)
		if k <= 0 || nd == 0 || nd > uint64(len(b)) {
			return ErrCorrupt
		}
		pos := k
		d.strs = d.strs[:0]
		if cap(d.strs) < int(nd) {
			d.strs = make([]string, 0, int(nd))
		}
		for i := uint64(0); i < nd; i++ {
			l, k := readUvarint(b[pos:])
			if k <= 0 || l > uint64(len(b)-pos-k) {
				return ErrCorrupt
			}
			pos += k
			d.strs = append(d.strs, d.str(b[pos:pos+int(l)]))
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
		for j := 0; j < cnt; j++ {
			ix := unpack(packed, j*w, w)
			if ix >= nd {
				return ErrCorrupt
			}
			*(*string)(unsafe.Add(base, uintptr(j)*stride)) = d.strs[ix]
		}
	default:
		return ErrCorrupt
	}
	return nil
}

func (d *dec) rowLane(fn *node, base unsafe.Pointer, stride uintptr, cnt int, b []byte) error {
	if b[0] != tagPlain || uint64(cnt) > uint64(len(b)-1)/uint64(d.w) {
		return ErrCorrupt
	}
	tblEnd := 1 + cnt*d.w
	span := func(j int) (int, int, bool) {
		s := d.off(b, 1+j*d.w)
		e := len(b)
		if j+1 < cnt {
			e = d.off(b, 1+(j+1)*d.w)
		}
		return s, e, s >= tblEnd && s <= e && e <= len(b)
	}
	// Allocate non-nil pointers to structs as one typed block. Columnar lanes
	// otherwise create one heap object per row, even though their lifetimes match.
	var block unsafe.Pointer
	if fn.kind == kPtr && fn.elem.kind == kStruct {
		live := 0
		for j := 0; j < cnt; j++ {
			s, e, ok := span(j)
			if !ok {
				return ErrCorrupt
			}
			if e > s {
				live++
			}
		}
		if live > 0 {
			if err := d.charge(live, fn.elem.size); err != nil {
				return err
			}
			block = reflect.MakeSlice(fn.batchT, live, live).UnsafePointer()
		}
	}
	var bi uintptr
	for j := 0; j < cnt; j++ {
		s, e, ok := span(j)
		if !ok {
			return ErrCorrupt
		}
		if fn.kind == kPtr && fn.elem.kind == kStruct {
			if e == s {
				*(*unsafe.Pointer)(unsafe.Add(base, uintptr(j)*stride)) = nil
				continue
			}
			q := unsafe.Add(block, bi*fn.elem.size)
			bi++
			if err := d.frame(fn.elem, q, b[s:e]); err != nil {
				return err
			}
			*(*unsafe.Pointer)(unsafe.Add(base, uintptr(j)*stride)) = q
			continue
		}
		if err := d.value(fn, unsafe.Add(base, uintptr(j)*stride), b[s:e]); err != nil {
			return err
		}
	}
	return nil
}

// dfor is a parsed tagDFOR lane.
type dfor struct {
	first, dmin uint64
	w, cw, nck  int
	packed, ck  []byte
}

func parseDFOR(b []byte, cnt int) (dfor, error) {
	var r dfor
	if cnt < 2 {
		return r, ErrCorrupt
	}
	f, k1 := readUvarint(b)
	if k1 <= 0 {
		return r, ErrCorrupt
	}
	dm, k2 := readUvarint(b[k1:])
	if k2 <= 0 || k1+k2 >= len(b) {
		return r, ErrCorrupt
	}
	r.first, r.dmin = uint64(unzigzag(f)), uint64(unzigzag(dm))
	r.w = int(b[k1+k2])
	L := cnt - 1
	plen := (L*r.w + 7) / 8
	p := k1 + k2 + 1
	if r.w > 64 || p+plen > len(b) {
		return r, ErrCorrupt
	}
	r.packed = b[p : p+plen]
	rest := b[p+plen:]
	r.nck = (L - 1) / checkEvery
	if r.w > 0 && r.nck > 0 {
		if len(rest) < 1 {
			return r, ErrCorrupt
		}
		r.cw = int(rest[0])
		if r.cw > 64 || len(rest)-1 != (r.nck*r.cw+7)/8 {
			return r, ErrCorrupt
		}
		r.ck = rest[1:]
	} else if len(rest) != 0 {
		return r, ErrCorrupt
	}
	return r, nil
}

// at returns element i (0-based) in O(checkEvery).
func (r *dfor) at(i int) uint64 {
	if i == 0 {
		return r.first
	}
	v := r.first + uint64(i)*r.dmin
	if r.w == 0 {
		return v
	}
	m := min(i/checkEvery, r.nck)
	var sum uint64
	if m > 0 {
		sum = unpack(r.ck, (m-1)*r.cw, r.cw)
	}
	for t := m * checkEvery; t < i; t++ {
		sum += unpack(r.packed, t*r.w, r.w)
	}
	return v + sum
}
