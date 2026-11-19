package store

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// On-disk layout of a store directory:
//
//	MANIFEST        ordered list of live segment ids (atomically replaced)
//	NNNNNNNNNN.seg  segments: magic, then records, append-only
//	INDEX           optional checkpoint of the key index (see snapshot)
//
// Record: crc32c(4) | klen(4) | vlen+flags(4) | key | value. The CRC covers
// everything after itself.
const (
	segMagic        = "STRATASG\x02"
	segMagicV1      = "STRATASG\x01"
	mfMagic         = "STRATAMF\x01"
	snapMagic       = "STRATAIX\x02"
	snapMagicV1     = "STRATAIX\x01"
	recHeader       = 20 // v2 header; v1 segments use legacyRecHeader
	legacyRecHeader = 12
	flagDel         = 1 << 31
	flagMore        = 1 << 30 // record belongs to a batch that continues after it
	lenMask         = flagMore - 1
	maxKey          = 1 << 16
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

func segPath(dir string, sid uint32) string { return filepath.Join(dir, fmt.Sprintf("%010d.seg", sid)) }

type rec struct {
	off       int64 // start of the record in the segment
	headerLen int
	klen      uint32
	vlen      uint32
	expiresAt int64 // Unix nanoseconds; zero means no expiry
	del, more bool
	key       []byte // valid only during the callback
}

func (r *rec) size() int64           { return int64(r.headerLen) + int64(r.klen) + int64(r.vlen) }
func (r *rec) voff() int64           { return r.off + int64(r.headerLen) + int64(r.klen) }
func recSize(klen, vlen int) int64   { return recHeader + int64(klen) + int64(vlen) }
func recSizeV1(klen, vlen int) int64 { return legacyRecHeader + int64(klen) + int64(vlen) }

// scanSegment reads records from start up to size, verifying each CRC. It
// calls fn for every valid record and stops at the first invalid one (torn or
// corrupt), returning the offset just past the last valid record.
func scanSegment(f *os.File, start, size int64, fn func(r *rec) bool) int64 {
	var magic [len(segMagic)]byte
	if _, err := f.ReadAt(magic[:], 0); err != nil {
		return start
	}
	headerLen := legacyRecHeader
	v2 := string(magic[:]) == segMagic
	if !v2 && string(magic[:]) != segMagicV1 {
		return start
	}
	if v2 {
		headerLen = recHeader
	}
	br := bufio.NewReaderSize(io.NewSectionReader(f, start, size-start), 1<<18)
	off := start
	var hdr [recHeader]byte
	var key []byte
	for off < size {
		if _, err := io.ReadFull(br, hdr[:headerLen]); err != nil {
			break
		}
		klen := binary.LittleEndian.Uint32(hdr[4:])
		vf := binary.LittleEndian.Uint32(hdr[8:])
		vlen := vf & lenMask
		if klen > maxKey || int64(klen)+int64(vlen) > size-off-int64(headerLen) {
			break
		}
		if cap(key) < int(klen) {
			key = make([]byte, klen)
		}
		key = key[:klen]
		if _, err := io.ReadFull(br, key); err != nil {
			break
		}
		h := crc32.Update(0, crcTable, hdr[4:headerLen])
		h = crc32.Update(h, crcTable, key)
		bad := false
		for rem := int(vlen); rem > 0; { // stream the value through the CRC, no copy, no allocation
			n := min(rem, 32<<10)
			p, err := br.Peek(n)
			if err != nil {
				bad = true
				break
			}
			h = crc32.Update(h, crcTable, p)
			br.Discard(n)
			rem -= n
		}
		if bad || h != binary.LittleEndian.Uint32(hdr[:4]) {
			break
		}
		r := rec{off: off, headerLen: headerLen, klen: klen, vlen: vlen, del: vf&flagDel != 0, more: vf&flagMore != 0, key: key}
		if v2 {
			r.expiresAt = int64(binary.LittleEndian.Uint64(hdr[12:20]))
		}
		off += r.size()
		if !fn(&r) {
			break
		}
	}
	return off
}

// appendRec appends one encoded record to dst.
func appendRec[K ~string | ~[]byte](dst []byte, key K, val []byte, del, more bool) []byte {
	return appendRecTTL(dst, key, val, del, more, 0)
}

func appendRecTTL[K ~string | ~[]byte](dst []byte, key K, val []byte, del, more bool, expiresAt int64) []byte {
	start := len(dst)
	dst = append(dst, make([]byte, recHeader)...)
	dst = append(dst, key...)
	dst = append(dst, val...)
	vf := uint32(len(val))
	if del {
		vf |= flagDel
	}
	if more {
		vf |= flagMore
	}
	binary.LittleEndian.PutUint32(dst[start+4:], uint32(len(key)))
	binary.LittleEndian.PutUint32(dst[start+8:], vf)
	binary.LittleEndian.PutUint64(dst[start+12:], uint64(expiresAt))
	binary.LittleEndian.PutUint32(dst[start:], crc32.Checksum(dst[start+4:], crcTable))
	return dst
}

// ---- manifest ----

func readManifest(dir string) (order []uint32, next uint32, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, "MANIFEST"))
	if os.IsNotExist(err) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	bad := fmt.Errorf("%w: bad manifest", ErrCorrupt)
	if len(b) < len(mfMagic)+12 || string(b[:len(mfMagic)]) != mfMagic {
		return nil, 0, false, bad
	}
	body, sum := b[:len(b)-4], binary.LittleEndian.Uint32(b[len(b)-4:])
	if crc32.Checksum(body, crcTable) != sum {
		return nil, 0, false, bad
	}
	p := body[len(mfMagic):]
	n := int(binary.LittleEndian.Uint32(p))
	p = p[4:]
	if len(p) != n*4+4 {
		return nil, 0, false, bad
	}
	for i := 0; i < n; i++ {
		order = append(order, binary.LittleEndian.Uint32(p[i*4:]))
	}
	return order, binary.LittleEndian.Uint32(p[n*4:]), true, nil
}

// writeManifest atomically replaces the manifest (tmp file, fsync, rename).
func writeManifest(dir string, order []uint32, next uint32) error {
	b := append([]byte(mfMagic), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(b[len(mfMagic):], uint32(len(order)))
	for _, s := range order {
		b = binary.LittleEndian.AppendUint32(b, s)
	}
	b = binary.LittleEndian.AppendUint32(b, next)
	b = binary.LittleEndian.AppendUint32(b, crc32.Checksum(b, crcTable))
	return atomicWrite(dir, "MANIFEST", func(w io.Writer) error { _, err := w.Write(b); return err })
}

func atomicWrite(dir, name string, fill func(io.Writer) error) error {
	tmp := filepath.Join(dir, name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<18)
	err = fill(bw)
	if err == nil {
		err = bw.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if d, derr := os.Open(dir); derr == nil { // make the rename durable
		d.Sync()
		d.Close()
	}
	return nil
}

// removeOrphans deletes segment files the manifest does not list (left by a
// crash during rotation or compaction) and returns the highest sid seen.
func removeOrphans(dir string, live map[uint32]bool) (maxSid uint32) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") {
			os.Remove(filepath.Join(dir, name))
			continue
		}
		var sid uint32
		if _, err := fmt.Sscanf(name, "%010d.seg", &sid); err == nil && strings.HasSuffix(name, ".seg") {
			maxSid = max(maxSid, sid)
			if !live[sid] {
				os.Remove(filepath.Join(dir, name))
			}
		}
	}
	return
}

// ---- index checkpoint ----
//
// INDEX stores every key's location plus the size each segment had, so Open can
// skip replaying the log and only replay what was appended after. It is
// ignored (and the log fully replayed) unless every segment it lists is still
// in the manifest and at least as long as recorded, so a stale or damaged
// checkpoint can never produce wrong data.

type segState struct {
	sid        uint32
	size, dead int64
}

type snapshotEntry struct {
	key []byte
	l   loc
}

func writeSnapshot(dir string, states []segState, t *tree) error {
	return atomicWrite(dir, "INDEX", func(w io.Writer) error {
		h := uint32(0)
		var scratch [binary.MaxVarintLen64]byte
		out := func(b []byte) error {
			h = crc32.Update(h, crcTable, b)
			_, err := w.Write(b)
			return err
		}
		uv := func(x uint64) error { return out(scratch[:binary.PutUvarint(scratch[:], x)]) }
		if err := out([]byte(snapMagic)); err != nil {
			return err
		}
		uv(uint64(len(states)))
		for _, s := range states {
			uv(uint64(s.sid))
			uv(uint64(s.size))
			uv(uint64(s.dead))
		}
		uv(uint64(t.n))
		var err error
		t.scan(nil, nil, false, func(k []byte, l loc) bool {
			if err = uv(uint64(len(k))); err != nil {
				return false
			}
			if err = out(k); err != nil {
				return false
			}
			uv(uint64(l.seg.sid))
			uv(uint64(l.off))
			if err = uv(uint64(l.vlen)); err == nil {
				err = uv(uint64(l.expiresAt))
			}
			return err == nil
		})
		if err != nil {
			return err
		}
		var sum [4]byte
		binary.LittleEndian.PutUint32(sum[:], h)
		_, err = w.Write(sum[:])
		return err
	})
}

// readSnapshot loads INDEX. ok is false when it is missing or invalid.
func readSnapshot(dir string, segments map[uint32]*segment) (states []segState, t *tree, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, "INDEX"))
	if err != nil || len(b) < len(snapMagic)+4 || (string(b[:len(snapMagic)]) != snapMagic && string(b[:len(snapMagic)]) != snapMagicV1) {
		return nil, nil, false
	}
	body := b[:len(b)-4]
	if crc32.Checksum(body, crcTable) != binary.LittleEndian.Uint32(b[len(b)-4:]) {
		return nil, nil, false
	}
	v2 := string(body[:len(snapMagic)]) == snapMagic
	p := body[len(snapMagic):]
	bad := false
	uv := func() uint64 {
		x, n := binary.Uvarint(p)
		if n <= 0 {
			bad = true
			return 0
		}
		p = p[n:]
		return x
	}
	ns := uv()
	if ns > uint64(len(p)) {
		return nil, nil, false
	}
	for i := uint64(0); i < ns && !bad; i++ {
		states = append(states, segState{uint32(uv()), int64(uv()), int64(uv())})
	}
	n := uv()
	if bad || n > uint64(len(p)) { // every entry takes at least one byte
		return nil, nil, false
	}
	entries := make([]snapshotEntry, 0, int(n))
	for i := uint64(0); i < n && !bad; i++ {
		kl := uv()
		if bad || kl > maxKey || kl > uint64(len(p)) {
			return nil, nil, false
		}
		key := p[:kl]
		p = p[kl:]
		sid := uint32(uv())
		seg := segments[sid]
		l := loc{seg: seg, off: uint32(uv()), vlen: uint32(uv())}
		if seg == nil {
			bad = true
		}
		if v2 {
			l.expiresAt = int64(uv())
		}
		if !bad {
			entries = append(entries, snapshotEntry{key: key, l: l})
		}
	}
	if bad || len(p) != 0 {
		return nil, nil, false
	}
	for i := 1; i < len(entries); i++ {
		if bytes.Compare(entries[i-1].key, entries[i].key) >= 0 {
			return nil, nil, false
		}
	}
	t = buildSortedTree(entries)
	return states, t, true
}
