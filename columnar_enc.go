package codec

import (
	"encoding/binary"
	"hash/maphash"
	"math/bits"
	"reflect"
	"sync"
	"unsafe"
)

// columnarMin is the smallest slice-of-struct length that is stored as
// columns; shorter slices use the row layout.
const columnarMin = 4

// checkEvery is the spacing of the random-access checkpoints in delta lanes.
const checkEvery = 64

// Lane tags. Every non-empty lane starts with one tag byte; an empty lane
// (zero bytes) means every element's field is the zero value.
const (
	tagPlain = 0 // bools: bit-packed; floats: raw; strings: len+bytes; rows: offset table
	tagFOR   = 1 // ints: uvarint(min) width packed(v-min)
	tagDelta = 2 // ints: zigzag-delta varints
	tagDict  = 3 // uvarint(n) entries width packed(index)
	tagDFOR  = 4 // ints: uvarint(zigzag(first)) uvarint(zigzag(minDelta)) width packed(delta-minDelta)
	//              [ckWidth packed(prefix sums every 64 deltas)]  -- the checkpoints give O(1) random access
)

type scratch struct {
	vals    []uint64
	idx     []uint64
	dl      []uint64
	ck      []uint64
	dict    []uint64
	sdict   []string
	us      uset
	ss      sset
	maps    map[*node]mapBuffers
	mapKeys []string
	i64Keys []string
	dynKeys [][]string
}

// mapBuffers retain the typed temporary slices used to make deterministic
// map output. Their elements are cleared before returning scratch to the pool.
type mapBuffers struct {
	keys, values reflect.Value
	busy         bool
}

var scratchPool = sync.Pool{New: func() any { return new(scratch) }}

var strSeed = maphash.MakeSeed()

// uset maps uint64 -> dense id with open addressing and a generation stamp,
// so reset is O(1) and nothing allocates once warm.
type uset struct {
	keys []uint64
	ids  []int32
	gen  []uint32
	cur  uint32
	mask uint64
}

func (m *uset) reset(n int) {
	size := 16
	for size < n {
		size <<= 1
	}
	if size > len(m.keys) {
		m.keys, m.ids, m.gen, m.cur = make([]uint64, size), make([]int32, size), make([]uint32, size), 0
	}
	m.mask = uint64(size - 1)
	if m.cur++; m.cur == 0 {
		clear(m.gen)
		m.cur = 1
	}
}

// slot returns the slot for v and whether it is occupied.
func (m *uset) slot(v uint64) (uint64, bool) {
	i := (v * 0x9E3779B97F4A7C15) >> 32 & m.mask
	for m.gen[i] == m.cur {
		if m.keys[i] == v {
			return i, true
		}
		i = (i + 1) & m.mask
	}
	return i, false
}

type sset struct {
	ids  []int32
	gen  []uint32
	cur  uint32
	mask uint64
}

func (m *sset) reset(n int) {
	size := 16
	for size < n {
		size <<= 1
	}
	if size > len(m.ids) {
		m.ids, m.gen, m.cur = make([]int32, size), make([]uint32, size), 0
	}
	m.mask = uint64(size - 1)
	if m.cur++; m.cur == 0 {
		clear(m.gen)
		m.cur = 1
	}
}

// find returns the dense id of s among dict, or reports the free slot.
func (m *sset) find(s string, dict []string) (slot uint64, id int32, ok bool) {
	i := maphash.String(strSeed, s) & m.mask
	for m.gen[i] == m.cur {
		if dict[m.ids[i]] == s {
			return i, m.ids[i], true
		}
		i = (i + 1) & m.mask
	}
	return i, 0, false
}

func uvSize(v uint64) int { return (bits.Len64(v|1) + 6) / 7 }

func (e *enc) pack(vals []uint64, sub uint64, w int) {
	if w == 0 || len(vals) == 0 {
		return
	}
	start := e.grow((len(vals)*w + 7) / 8)
	buf := e.buf[start:]
	pos := 0
	for _, v := range vals {
		v -= sub
		idx, sh := pos>>3, uint(pos&7)
		x := v << sh
		if idx+8 <= len(buf) {
			le.PutUint64(buf[idx:], le.Uint64(buf[idx:])|x)
		} else {
			for k := 0; idx+k < len(buf) && k < 8; k++ {
				buf[idx+k] |= byte(x >> (8 * uint(k)))
			}
		}
		if int(sh)+w > 64 {
			buf[idx+8] |= byte(v >> (64 - sh))
		}
		pos += w
	}
}

// columns writes the lane directory and lanes for cnt struct elements. It
// reports false (leaving garbage the caller truncates) when every lane is empty.
func (e *enc) columns(el *node, data unsafe.Pointer, cnt, start int) bool {
	nf := len(el.fields)
	tbl := e.grow(nf * e.w)
	lanesAt := len(e.buf)
	for i := range el.fields {
		e.putOff(tbl+i*e.w, len(e.buf)-start)
		f := &el.fields[i]
		base := unsafe.Add(data, f.off)
		switch f.n.kind {
		case kBool:
			e.boolLane(base, el.size, cnt)
		case kInt, kUint, kF32, kF64:
			e.numLane(f.n, base, el.size, cnt)
		case kString:
			e.stringLane(base, el.size, cnt)
		default:
			e.rowLane(f.n, base, el.size, cnt, start)
		}
		if e.fail {
			return true
		}
	}
	return len(e.buf) > lanesAt
}

func (e *enc) boolLane(base unsafe.Pointer, stride uintptr, cnt int) {
	vals := e.sc.vals[:0]
	any := false
	for j := 0; j < cnt; j++ {
		var v uint64
		if *(*bool)(unsafe.Add(base, uintptr(j)*stride)) {
			v, any = 1, true
		}
		vals = append(vals, v)
	}
	e.sc.vals = vals
	if !any {
		return
	}
	e.buf = append(e.buf, tagPlain)
	e.pack(vals, 0, 1)
}

func entryEnc(k kind, v uint64) uint64 {
	if k == kInt {
		return zigzag(int64(v))
	}
	return v
}

func (e *enc) numLane(fn *node, base unsafe.Pointer, stride uintptr, cnt int) {
	sc := e.sc
	vals := sc.vals[:0]
	any := false
	for j := 0; j < cnt; j++ {
		q := unsafe.Add(base, uintptr(j)*stride)
		var v uint64
		switch fn.kind {
		case kInt:
			v = uint64(loadInt(fn.size, q))
		case kUint:
			v = loadUint(fn.size, q)
		case kF32:
			v = uint64(*(*uint32)(q))
		default:
			v = *(*uint64)(q)
		}
		vals = append(vals, v)
		any = any || v != 0
	}
	sc.vals = vals
	if !any {
		return
	}
	isInt := fn.kind == kInt || fn.kind == kUint
	fixed := 8
	if fn.kind == kF32 {
		fixed = 4
	}

	best, bestSize := -1, 1<<62
	var mn, dmin uint64
	var width, dwid, cw int
	ck := sc.ck
	if isInt {
		mn = vals[0]
		mx := vals[0]
		for _, v := range vals {
			if fn.kind == kInt {
				if int64(v) < int64(mn) {
					mn = v
				}
				if int64(v) > int64(mx) {
					mx = v
				}
			} else {
				mn, mx = min(mn, v), max(mx, v)
			}
		}
		width = bits.Len64(mx - mn)
		if s := uvSize(entryEnc(fn.kind, mn)) + 1 + (cnt*width+7)/8; s < bestSize {
			best, bestSize = tagFOR, s
		}
		var prev uint64
		ds := 0
		for _, v := range vals {
			ds += uvSize(zigzag(int64(v - prev)))
			prev = v
		}
		if ds < bestSize && cnt <= checkEvery { // delta varints have no index: only for short lanes
			best, bestSize = tagDelta, ds
		}
		if cnt >= 2 {
			dl := sc.dl[:0]
			for j := 1; j < cnt; j++ {
				dl = append(dl, vals[j]-vals[j-1])
			}
			sc.dl = dl
			dmn, dmx := dl[0], dl[0]
			for _, d := range dl {
				if int64(d) < int64(dmn) {
					dmn = d
				}
				if int64(d) > int64(dmx) {
					dmx = d
				}
			}
			dmin, dwid = dmn, bits.Len64(dmx-dmn)
			dsize := uvSize(zigzag(int64(vals[0]))) + uvSize(zigzag(int64(dmn))) + 1 + ((cnt-1)*dwid+7)/8
			ck = ck[:0]
			if nck := (cnt - 2) / checkEvery; dwid > 0 && nck > 0 {
				var sum, mxc uint64
				for t, d := range dl {
					if t > 0 && t%checkEvery == 0 {
						ck = append(ck, sum)
						mxc = max(mxc, sum)
					}
					sum += d - dmn
				}
				cw = bits.Len64(mxc)
				dsize += 1 + (nck*cw+7)/8
			}
			sc.ck = ck
			if s := dsize; s < bestSize {
				best, bestSize = tagDFOR, s
			}
		}
	} else {
		best, bestSize = tagPlain, cnt*fixed
	}

	// dictionary candidate
	var dictSize, dw int
	haveDict := false
	if cnt >= 8 {
		sc.us.reset(cnt)
		sc.dict, sc.idx = sc.dict[:0], sc.idx[:0]
		haveDict = true
		for _, v := range vals {
			slot, ok := sc.us.slot(v)
			var id int32
			if ok {
				id = sc.us.ids[slot]
			} else {
				if len(sc.dict) >= cnt/2 {
					haveDict = false
					break
				}
				unique := len(sc.dict) + 1
				minSize := uvSize(uint64(unique)) + 1 + (cnt*bits.Len64(uint64(unique-1))+7)/8
				if isInt {
					minSize += unique
				} else {
					minSize += unique * fixed
				}
				if minSize >= bestSize {
					haveDict = false
					break
				}
				id = int32(len(sc.dict))
				sc.us.keys[slot], sc.us.ids[slot], sc.us.gen[slot] = v, id, sc.us.cur
				sc.dict = append(sc.dict, v)
			}
			sc.idx = append(sc.idx, uint64(id))
		}
		if haveDict {
			dw = bits.Len64(uint64(len(sc.dict) - 1))
			dictSize = uvSize(uint64(len(sc.dict))) + 1 + (cnt*dw+7)/8
			for _, v := range sc.dict {
				if isInt {
					dictSize += uvSize(entryEnc(fn.kind, v))
				} else {
					dictSize += fixed
				}
			}
			if dictSize < bestSize {
				best = tagDict
			}
		}
	}

	e.buf = append(e.buf, byte(best))
	switch best {
	case tagFOR:
		e.buf = binary.AppendUvarint(e.buf, entryEnc(fn.kind, mn))
		e.buf = append(e.buf, byte(width))
		e.pack(vals, mn, width)
	case tagDelta:
		var prev uint64
		for _, v := range vals {
			e.buf = binary.AppendUvarint(e.buf, zigzag(int64(v-prev)))
			prev = v
		}
	case tagDFOR:
		e.buf = binary.AppendUvarint(e.buf, zigzag(int64(vals[0])))
		e.buf = binary.AppendUvarint(e.buf, zigzag(int64(dmin)))
		e.buf = append(e.buf, byte(dwid))
		e.pack(sc.dl, dmin, dwid)
		if len(sc.ck) > 0 && dwid > 0 {
			e.buf = append(e.buf, byte(cw))
			e.pack(sc.ck, 0, cw)
		}
	case tagPlain:
		for _, v := range vals {
			if fixed == 4 {
				e.buf = le.AppendUint32(e.buf, uint32(v))
			} else {
				e.buf = le.AppendUint64(e.buf, v)
			}
		}
	case tagDict:
		e.buf = binary.AppendUvarint(e.buf, uint64(len(sc.dict)))
		for _, v := range sc.dict {
			switch {
			case isInt:
				e.buf = binary.AppendUvarint(e.buf, entryEnc(fn.kind, v))
			case fixed == 4:
				e.buf = le.AppendUint32(e.buf, uint32(v))
			default:
				e.buf = le.AppendUint64(e.buf, v)
			}
		}
		e.buf = append(e.buf, byte(dw))
		e.pack(sc.idx, 0, dw)
	}
}

func (e *enc) stringLane(base unsafe.Pointer, stride uintptr, cnt int) {
	sc := e.sc
	any := false
	plain := 0
	for j := 0; j < cnt; j++ {
		s := *(*string)(unsafe.Add(base, uintptr(j)*stride))
		any = any || len(s) > 0
		plain += uvSize(uint64(len(s))) + len(s)
	}
	if !any {
		return
	}
	useDict := false
	var dw int
	if cnt >= 8 {
		sc.ss.reset(cnt)
		sc.sdict, sc.idx = sc.sdict[:0], sc.idx[:0]
		useDict = true
		dsz := 0
		for j := 0; j < cnt; j++ {
			s := *(*string)(unsafe.Add(base, uintptr(j)*stride))
			slot, id, ok := sc.ss.find(s, sc.sdict)
			if !ok {
				if len(sc.sdict) >= cnt/2 {
					useDict = false
					break
				}
				id = int32(len(sc.sdict))
				sc.ss.ids[slot], sc.ss.gen[slot] = id, sc.ss.cur
				sc.sdict = append(sc.sdict, s)
				dsz += uvSize(uint64(len(s))) + len(s)
			}
			sc.idx = append(sc.idx, uint64(id))
		}
		if useDict {
			dw = bits.Len64(uint64(len(sc.sdict) - 1))
			dsz += uvSize(uint64(len(sc.sdict))) + 1 + (cnt*dw+7)/8
			useDict = dsz < plain
		}
	}
	if useDict {
		e.buf = append(e.buf, tagDict)
		e.buf = binary.AppendUvarint(e.buf, uint64(len(sc.sdict)))
		for _, s := range sc.sdict {
			e.buf = binary.AppendUvarint(e.buf, uint64(len(s)))
			e.buf = append(e.buf, s...)
		}
		e.buf = append(e.buf, byte(dw))
		e.pack(sc.idx, 0, dw)
		return
	}
	e.buf = append(e.buf, tagPlain)
	for j := 0; j < cnt; j++ {
		s := *(*string)(unsafe.Add(base, uintptr(j)*stride))
		e.buf = binary.AppendUvarint(e.buf, uint64(len(s)))
		e.buf = append(e.buf, s...)
	}
}

// rowLane stores a non-scalar field (nested struct, slice, pointer) as an
// offset table of per-element payloads, relative to the lane start.
func (e *enc) rowLane(fn *node, base unsafe.Pointer, stride uintptr, cnt, _ int) {
	live := fn.kind == kStruct
	for j := 0; j < cnt && !live; j++ {
		q := unsafe.Add(base, uintptr(j)*stride)
		switch fn.kind {
		case kPtr:
			live = *(*unsafe.Pointer)(q) != nil
		case kSlice:
			live = (*sliceHeader)(q).Len > 0
		default:
			live = true
		}
	}
	if !live {
		return
	}
	start := len(e.buf)
	e.buf = append(e.buf, tagPlain)
	tbl := e.grow(cnt * e.w)
	for j := 0; j < cnt; j++ {
		e.putOff(tbl+j*e.w, len(e.buf)-start)
		e.value(fn, unsafe.Add(base, uintptr(j)*stride))
		if e.fail {
			return
		}
	}
}
