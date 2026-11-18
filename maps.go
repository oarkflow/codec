package codec

import (
	"encoding"
	"encoding/binary"
	"math"
	"reflect"
	"sort"
	"unsafe"
)

const maxReusableMapEntries = 4096

// A map is stored as two slice payloads, keys then values, in sorted key
// order (so output is deterministic): uvarint(len(keys payload)) keys values.
// Both halves reuse the slice machinery, so integer and struct maps benefit
// from the same packing and columnar lanes.

func (e *enc) mapv(n *node, p unsafe.Pointer) {
	if n.rt == reflect.TypeFor[map[string]any]() {
		e.mapStringAny(*(*map[string]any)(p))
		return
	}
	if n.rt == reflect.TypeFor[map[string]int64]() {
		e.mapStringInt64(*(*map[string]int64)(p))
		return
	}
	if n.rt == reflect.TypeFor[map[string]string]() {
		e.mapStringString(*(*map[string]string)(p))
		return
	}
	mv := reflect.NewAt(n.rt, p).Elem()
	cnt := mv.Len()
	if cnt == 0 {
		return
	}
	if e.sc.maps == nil {
		e.sc.maps = make(map[*node]mapBuffers)
	}
	buf := e.sc.maps[n]
	cached := !buf.busy && cnt <= maxReusableMapEntries
	if !cached {
		// Recursive maps of the same type cannot share their parent's live
		// buffers. Keep this uncommon path local to preserve the parent values.
		buf = mapBuffers{}
	}
	if !buf.keys.IsValid() {
		buf.keys = reflect.New(n.kslice.rt).Elem()
	}
	if buf.keys.Cap() < cnt {
		buf.keys.Set(reflect.MakeSlice(n.kslice.rt, cnt, cnt))
	} else {
		buf.keys.SetLen(cnt)
	}
	if !buf.values.IsValid() {
		buf.values = reflect.New(n.vslice.rt).Elem()
	}
	if buf.values.Cap() < cnt {
		buf.values.Set(reflect.MakeSlice(n.vslice.rt, cnt, cnt))
	} else {
		buf.values.SetLen(cnt)
	}
	if cached {
		buf.busy = true
		e.sc.maps[n] = buf
	}
	ks, vs := buf.keys, buf.values
	defer func() {
		for i := 0; i < cnt; i++ {
			ks.Index(i).SetZero()
			vs.Index(i).SetZero()
		}
		if cached {
			buf.busy = false
			e.sc.maps[n] = buf
		}
	}()
	it := mv.MapRange()
	for i := 0; it.Next(); i++ {
		ks.Index(i).SetIterKey(it)
		vs.Index(i).SetIterValue(it)
	}
	sortPairs(n.kslice, ks, vs)
	start := len(e.buf)
	e.slice(n.kslice, ks.Addr().UnsafePointer())
	if e.fail {
		return
	}
	klen := len(e.buf) - start
	var hdr [binary.MaxVarintLen64]byte
	hn := binary.PutUvarint(hdr[:], uint64(klen))
	e.buf = append(e.buf, hdr[:hn]...)
	copy(e.buf[start+hn:], e.buf[start:start+klen])
	copy(e.buf[start:], hdr[:hn])
	e.slice(n.vslice, vs.Addr().UnsafePointer())
}

// stringMapBuffer returns reusable key storage. Oversized maps use a
// temporary buffer so one large message cannot pin a large key array in the
// encoder pool.
func (e *enc) stringMapBuffer(cnt int) ([]string, bool) {
	keys := e.sc.mapKeys[:0]
	if cnt > maxReusableMapEntries {
		keys = make([]string, 0, cnt)
	}
	return keys, cnt <= maxReusableMapEntries
}

func (e *enc) releaseStringMapBuffer(keys []string, cached bool) {
	clear(keys)
	if cached {
		e.sc.mapKeys = keys[:0]
	}
}

func (e *enc) mapStringInt64(m map[string]int64) {
	if len(m) == 0 {
		return
	}
	keys, fromCache, release := e.stringInt64Keys(m)
	if release {
		defer e.releaseStringMapBuffer(keys, len(m) <= maxReusableMapEntries)
	}
	start, keyStart := len(e.buf), len(e.buf)
	e.buf = binary.AppendUvarint(e.buf, uint64(len(keys))<<1)
	tbl := e.grow(len(keys) * e.w)
	for i, k := range keys {
		e.putOff(tbl+i*e.w, len(e.buf)-keyStart)
		e.buf = append(e.buf, k...)
	}
	klen := len(e.buf) - keyStart
	var hdr [binary.MaxVarintLen64]byte
	hn := binary.PutUvarint(hdr[:], uint64(klen))
	e.buf = append(e.buf, hdr[:hn]...)
	copy(e.buf[start+hn:], e.buf[keyStart:keyStart+klen])
	copy(e.buf[start:], hdr[:hn])
	e.buf = binary.AppendUvarint(e.buf, uint64(len(keys)))
	var prev int64
	for _, k := range keys {
		v, ok := m[k]
		if !ok && fromCache {
			// A same-sized key set changed since the cached sort. Roll back the
			// partial map and rebuild once using the current keys.
			clear(e.sc.i64Keys)
			e.sc.i64Keys = nil
			e.buf = e.buf[:start]
			e.mapStringInt64(m)
			return
		}
		e.buf = binary.AppendUvarint(e.buf, zigzag(v-prev))
		prev = v
	}
}

// stringInt64Keys reuses a small map's sorted key set when a subsequent
// marshal sees the same keys. Value lookups detect key-set mutations and
// trigger a rebuild, avoiding a separate validation pass on the hot path.
func (e *enc) stringInt64Keys(m map[string]int64) (keys []string, fromCache, release bool) {
	keys = e.sc.i64Keys
	if len(keys) == len(m) && len(keys) != 0 {
		return keys, true, false
	}
	clear(keys)
	e.sc.i64Keys = nil
	keys, release = e.stringMapBuffer(len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) <= 64 {
		total := 0
		for _, k := range keys {
			total += len(k)
		}
		if total <= 1024 {
			e.sc.i64Keys = keys
			e.sc.mapKeys = nil
			return keys, false, false
		}
	}
	return keys, false, release
}

func (e *enc) mapStringString(m map[string]string) {
	if len(m) == 0 {
		return
	}
	keys, cached := e.stringMapBuffer(len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	defer e.releaseStringMapBuffer(keys, cached)
	start := len(e.buf)
	keyStart := len(e.buf)
	e.buf = binary.AppendUvarint(e.buf, uint64(len(keys))<<1)
	tbl := e.grow(len(keys) * e.w)
	for i, k := range keys {
		e.putOff(tbl+i*e.w, len(e.buf)-keyStart)
		e.buf = append(e.buf, k...)
	}
	klen := len(e.buf) - keyStart
	var hdr [binary.MaxVarintLen64]byte
	hn := binary.PutUvarint(hdr[:], uint64(klen))
	e.buf = append(e.buf, hdr[:hn]...)
	copy(e.buf[start+hn:], e.buf[keyStart:keyStart+klen])
	copy(e.buf[start:], hdr[:hn])
	valueStart := len(e.buf)
	e.buf = binary.AppendUvarint(e.buf, uint64(len(keys))<<1)
	tbl = e.grow(len(keys) * e.w)
	for i, k := range keys {
		e.putOff(tbl+i*e.w, len(e.buf)-valueStart)
		e.buf = append(e.buf, m[k]...)
	}
}

func (e *enc) mapStringAny(m map[string]any) {
	if len(m) == 0 {
		return
	}
	keys, cached := e.stringMapBuffer(len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	defer e.releaseStringMapBuffer(keys, cached)
	start := len(e.buf)
	keyStart := len(e.buf)
	e.buf = binary.AppendUvarint(e.buf, uint64(len(keys))<<1)
	tbl := e.grow(len(keys) * e.w)
	for i, k := range keys {
		e.putOff(tbl+i*e.w, len(e.buf)-keyStart)
		e.buf = append(e.buf, k...)
	}
	klen := len(e.buf) - keyStart
	var hdr [binary.MaxVarintLen64]byte
	hn := binary.PutUvarint(hdr[:], uint64(klen))
	e.buf = append(e.buf, hdr[:hn]...)
	copy(e.buf[start+hn:], e.buf[keyStart:keyStart+klen])
	copy(e.buf[start:], hdr[:hn])
	valueStart := len(e.buf)
	e.buf = binary.AppendUvarint(e.buf, uint64(len(keys))<<1)
	tbl = e.grow(len(keys) * e.w)
	for i, k := range keys {
		e.putOff(tbl+i*e.w, len(e.buf)-valueStart)
		e.dynAny(m[k], 0)
		if e.err != nil {
			return
		}
	}
}

func (d *dec) mapv(n *node, p unsafe.Pointer, b []byte) error {
	if n.rt == reflect.TypeFor[map[string]int64]() {
		return d.mapStringInt64(p, b)
	}
	if n.rt == reflect.TypeFor[map[string]string]() {
		return d.mapStringString(p, b)
	}
	if n.rt == reflect.TypeFor[map[string]any]() {
		return d.mapStringAny(p, b)
	}
	if len(b) == 0 {
		return nil
	}
	kl, k := readUvarint(b)
	if k <= 0 || kl == 0 || kl >= uint64(len(b)-k) {
		return ErrCorrupt
	}
	kb, vb := b[k:k+int(kl)], b[k+int(kl):]
	if d.maps == nil {
		d.maps = make(map[*node]mapBuffers)
	}
	buf := d.maps[n]
	cached := !buf.busy && (buf.keys.IsValid() || len(d.maps) < maxReusableMapTypes)
	if !cached {
		buf = mapBuffers{}
	}
	if !buf.keys.IsValid() {
		buf.keys = reflect.New(n.kslice.rt).Elem()
	}
	if !buf.values.IsValid() {
		buf.values = reflect.New(n.vslice.rt).Elem()
	}
	buf.keys.SetLen(0)
	buf.values.SetLen(0)
	if cached {
		buf.busy = true
		d.maps[n] = buf
	}
	ks, vs := buf.keys, buf.values
	defer func() {
		for i := 0; i < ks.Len(); i++ {
			ks.Index(i).SetZero()
		}
		for i := 0; i < vs.Len(); i++ {
			vs.Index(i).SetZero()
		}
		ks.SetLen(0)
		vs.SetLen(0)
		if cached {
			if reusableMapBuffer(ks, n.kslice.elem) && reusableMapBuffer(vs, n.vslice.elem) {
				buf.keys, buf.values, buf.busy = ks, vs, false
				d.maps[n] = buf
			} else {
				delete(d.maps, n)
			}
		}
	}()
	if err := d.value(n.kslice, ks.Addr().UnsafePointer(), kb); err != nil {
		return err
	}
	if err := d.value(n.vslice, vs.Addr().UnsafePointer(), vb); err != nil {
		return err
	}
	kk, vv := ks, vs
	if kk.Len() != vv.Len() {
		return ErrCorrupt
	}
	if err := d.charge(kk.Len(), 48); err != nil {
		return err
	}
	m := reflect.NewAt(n.rt, p).Elem()
	if m.IsNil() {
		m = reflect.MakeMapWithSize(n.rt, kk.Len())
	} else {
		m.Clear()
	}
	for i := 0; i < kk.Len(); i++ {
		m.SetMapIndex(kk.Index(i), vv.Index(i))
	}
	reflect.NewAt(n.rt, p).Elem().Set(m)
	return nil
}

func reusableMapBuffer(v reflect.Value, elem *node) bool {
	if v.Cap() > maxReusableMapEntries {
		return false
	}
	return elem.size == 0 || uintptr(v.Cap()) <= uintptr(maxReusableMapBytes)/elem.size
}

func (d *dec) stringMapParts(b []byte) (keys, values []byte, count, tableEnd int, err error) {
	kl, k := readUvarint(b)
	if k <= 0 || kl == 0 || kl >= uint64(len(b)-k) {
		err = ErrCorrupt
		return
	}
	keys, values = b[k:k+int(kl)], b[k+int(kl):]
	c, table, e := d.stringRows(keys)
	if e != nil {
		err = e
		return
	}
	count, tableEnd = c, table
	return
}

// stringRows validates the count and offset table shared by string slices and
// string-key map sections. The returned table end bounds every row span.
func (d *dec) stringRows(b []byte) (count, tableEnd int, err error) {
	h, k := readUvarint(b)
	if k <= 0 || h&1 != 0 || h>>1 > uint64(MaxElements) {
		return 0, 0, ErrCorrupt
	}
	count = int(h >> 1)
	if count > (len(b)-k)/d.w {
		return 0, 0, ErrCorrupt
	}
	return count, k + count*d.w, nil
}

func (d *dec) stringRow(b []byte, tableEnd, count, i int) ([]byte, error) {
	tableStart := tableEnd - count*d.w
	s := d.off(b, tableStart+i*d.w)
	e := len(b)
	if i+1 < count {
		e = d.off(b, tableStart+(i+1)*d.w)
	}
	if s < tableEnd || s > e || e > len(b) {
		return nil, ErrCorrupt
	}
	return b[s:e], nil
}

func (d *dec) mapStringInt64(p unsafe.Pointer, b []byte) error {
	if len(b) == 0 {
		return nil
	}
	keys, values, count, keyTable, err := d.stringMapParts(b)
	if err != nil {
		return err
	}
	vc, pos := readUvarint(values)
	if pos <= 0 || vc != uint64(count) {
		return ErrCorrupt
	}
	if err := d.charge(count, 48); err != nil {
		return err
	}
	out := *(*map[string]int64)(p)
	if out == nil {
		out = make(map[string]int64, count)
	} else {
		clear(out)
	}
	var prev int64
	for i := 0; i < count; i++ {
		key, err := d.stringRow(keys, keyTable, count, i)
		if err != nil {
			return err
		}
		u, k := readUvarint(values[pos:])
		if k <= 0 {
			return ErrCorrupt
		}
		pos += k
		prev += unzigzag(u)
		out[d.str(key)] = prev
	}
	if pos != len(values) {
		return ErrCorrupt
	}
	*(*map[string]int64)(p) = out
	return nil
}

func (d *dec) mapStringString(p unsafe.Pointer, b []byte) error {
	if len(b) == 0 {
		return nil
	}
	keys, values, count, keyTable, err := d.stringMapParts(b)
	if err != nil {
		return err
	}
	vc, valueTable, err := d.stringRows(values)
	if err != nil || vc != count {
		return ErrCorrupt
	}
	if err := d.charge(count, 48); err != nil {
		return err
	}
	out := *(*map[string]string)(p)
	if out == nil {
		out = make(map[string]string, count)
	} else {
		clear(out)
	}
	for i := 0; i < count; i++ {
		key, err := d.stringRow(keys, keyTable, count, i)
		if err != nil {
			return err
		}
		value, err := d.stringRow(values, valueTable, count, i)
		if err != nil {
			return err
		}
		out[d.str(key)] = d.str(value)
	}
	*(*map[string]string)(p) = out
	return nil
}

func (d *dec) mapStringAny(p unsafe.Pointer, b []byte) error {
	if len(b) == 0 {
		return nil
	}
	keys, values, count, keyTable, err := d.stringMapParts(b)
	if err != nil {
		return err
	}
	vc, valueTable, err := d.stringRows(values)
	if err != nil || vc != count {
		return ErrCorrupt
	}
	if err := d.charge(count, 48); err != nil {
		return err
	}
	out := *(*map[string]any)(p)
	if out == nil {
		out = make(map[string]any, count)
	} else {
		clear(out)
	}
	for i := 0; i < count; i++ {
		key, err := d.stringRow(keys, keyTable, count, i)
		if err != nil {
			return err
		}
		value, err := d.stringRow(values, valueTable, count, i)
		if err != nil {
			return err
		}
		v, n, err := d.dyn(value, 0)
		if err != nil {
			return err
		}
		if n != len(value) {
			return ErrCorrupt
		}
		out[d.str(key)] = v
	}
	*(*map[string]any)(p) = out
	return nil
}

func (d *dec) bin(n *node, p unsafe.Pointer, b []byte) error {
	if len(b) == 0 {
		return nil
	}
	pv := reflect.NewAt(n.rt, p).Interface()
	if n.text {
		return pv.(encoding.TextUnmarshaler).UnmarshalText(b)
	}
	return pv.(encoding.BinaryUnmarshaler).UnmarshalBinary(b)
}

// sortPairs sorts the key slice (and the value slice alongside it) so the
// encoding is deterministic. Only scalar and string keys are ordered.
type pairSorter struct {
	less         func(i, j int) bool
	swapK, swapV func(i, j int)
	n            int
}

func (s *pairSorter) Len() int           { return s.n }
func (s *pairSorter) Less(i, j int) bool { return s.less(i, j) }
func (s *pairSorter) Swap(i, j int)      { s.swapK(i, j); s.swapV(i, j) }

func sortPairs(kn *node, ks, vs reflect.Value) {
	n := ks.Len()
	if n < 2 {
		return
	}
	el := kn.elem
	data := (*sliceHeader)(ks.Addr().UnsafePointer()).Data
	at := func(i int) unsafe.Pointer { return unsafe.Add(data, uintptr(i)*el.size) }
	var less func(i, j int) bool
	switch el.kind {
	case kString:
		less = func(i, j int) bool { return *(*string)(at(i)) < *(*string)(at(j)) }
	case kInt:
		less = func(i, j int) bool { return loadInt(el.size, at(i)) < loadInt(el.size, at(j)) }
	case kUint:
		less = func(i, j int) bool { return loadUint(el.size, at(i)) < loadUint(el.size, at(j)) }
	case kF32:
		less = func(i, j int) bool {
			return math.Float32frombits(*(*uint32)(at(i))) < math.Float32frombits(*(*uint32)(at(j)))
		}
	case kF64:
		less = func(i, j int) bool {
			return math.Float64frombits(*(*uint64)(at(i))) < math.Float64frombits(*(*uint64)(at(j)))
		}
	case kBool:
		less = func(i, j int) bool { return !*(*bool)(at(i)) && *(*bool)(at(j)) }
	default:
		return
	}
	sort.Sort(&pairSorter{less: less, swapK: reflect.Swapper(ks.Interface()), swapV: reflect.Swapper(vs.Interface()), n: n})
}
