// Package store is a persistent, typed key-value store built on the Strata
// codec, designed to stay fast and light as it grows.
//
// Design (a segmented log-structured store with an ordered in-memory index):
//
//   - Writes append records to the active segment file; nothing is ever
//     rewritten in place. When a segment reaches Options.SegmentSize a new one
//     starts, so no single file grows without bound.
//   - The index is a compressed radix tree from key to the location of the
//     latest value (12 bytes per key plus shared key prefixes). A Get is one
//     tree lookup plus one pread. Memory use depends on the number of keys,
//     not on how much value data is stored; values stay on disk.
//   - Because the tree is ordered, prefix and wildcard scans visit only the
//     keys they need, in sorted order (see Scan, Match, DeleteMatch).
//   - Records carry a CRC, a crash leaves at most a torn tail that is cut off on
//     reopen, and a Batch is atomic: all of it or none of it.
//   - Space from overwritten and deleted records is reclaimed by compaction,
//     which merges a few segments at a time in the background without blocking
//     readers or writers for more than short moments.
//   - A checkpoint of the index (written on Close and by Checkpoint) lets Open
//     skip replaying the log, so reopening a large store is fast.
//
// Values are any type the codec can marshal (up to 1 GiB each; keys up to 64
// KiB). A DB is safe for concurrent use by multiple goroutines. It does not
// lock the directory against other processes.
package store

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
	"time"

	"github.com/oarkflow/codec"
)

var (
	ErrNotFound = errors.New("store: key not found")
	ErrClosed   = errors.New("store: closed")
	ErrTooLarge = errors.New("store: key, value or batch too large")
	ErrCorrupt  = errors.New("store: corrupt data")
)

// Options configures Open. The zero value is a good default.
type Options struct {
	// SegmentSize is the size at which the active segment is closed and a new
	// one started (default 64 MiB, at most 2 GiB). Compaction works one run of
	// segments at a time, so smaller segments mean finer-grained compaction.
	SegmentSize int64
	// SyncAlways fsyncs after every write. Durable against power loss but much
	// slower. Without it, data survives a process crash, but the last few
	// writes can be lost on power failure unless Sync is called.
	SyncAlways bool
	// SyncInterval, if > 0 and SyncAlways is false, fsyncs in the background.
	SyncInterval time.Duration
	// Strict writes plain messages, which are one byte per struct type smaller
	// but can only be read back by exactly the same type. By default values are
	// written so that later versions of T, with fields added at the end of its
	// structs, can still read them (see codec.MarshalEvolvable).
	Strict bool
	// CompactRatio starts a background compaction of segments whose dead bytes
	// exceed this fraction of their size (default 0.5; negative disables
	// automatic compaction, Compact still works).
	CompactRatio float64
}

type segment struct {
	sid        uint32
	f          *os.File
	size, dead int64
}

// DB is a store of values of type T keyed by string.
type DB[T any] struct {
	mu      sync.RWMutex
	dir     string
	t       *tree
	segs    map[uint32]*segment
	order   []uint32 // segment ids, oldest first; the last is active
	active  *segment
	nextSid uint32
	opts    Options
	wbuf    []byte
	rpool   sync.Pool
	closed  bool
	dirty   bool

	cmu    sync.Mutex // serializes compactions
	snapMu sync.Mutex // serializes checkpoints
	kick   chan struct{}
	done   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

type entry struct {
	key  string
	vlen int
	del  bool
}

// Open opens or creates the store in directory dir.
func Open[T any](dir string, opts *Options) (*DB[T], error) {
	db := &DB[T]{dir: dir, t: &tree{}, segs: map[uint32]*segment{}, kick: make(chan struct{}, 1), done: make(chan struct{})}
	if opts != nil {
		db.opts = *opts
	}
	if db.opts.SegmentSize <= 0 {
		db.opts.SegmentSize = 64 << 20
	}
	db.opts.SegmentSize = min(db.opts.SegmentSize, 2<<30)
	if db.opts.CompactRatio == 0 {
		db.opts.CompactRatio = 0.5
	}
	db.rpool.New = func() any { b := make([]byte, 0, 512); return &b }
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := db.load(); err != nil {
		for _, s := range db.segs {
			s.f.Close()
		}
		return nil, err
	}
	if db.opts.CompactRatio >= 0 {
		db.wg.Add(1)
		go db.compactLoop()
	}
	if db.opts.SyncInterval > 0 && !db.opts.SyncAlways {
		db.wg.Add(1)
		go db.syncLoop()
	}
	return db, nil
}

func (db *DB[T]) newSegment(sid uint32) (*segment, error) {
	f, err := os.OpenFile(segPath(db.dir, sid), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteAt([]byte(segMagic), 0); err != nil {
		f.Close()
		os.Remove(segPath(db.dir, sid))
		return nil, err
	}
	return &segment{sid: sid, f: f, size: int64(len(segMagic))}, nil
}

func (db *DB[T]) load() error {
	order, next, ok, err := readManifest(db.dir)
	if err != nil {
		return err
	}
	if !ok {
		s, err := db.newSegment(1)
		if err != nil {
			return err
		}
		db.segs[1], db.order, db.active, db.nextSid = s, []uint32{1}, s, 2
		return writeManifest(db.dir, db.order, db.nextSid)
	}
	live := map[uint32]bool{}
	for _, sid := range order {
		live[sid] = true
	}
	db.nextSid = max(next, removeOrphans(db.dir, live)+1)
	db.order = order
	for _, sid := range order {
		f, err := os.OpenFile(segPath(db.dir, sid), os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("%w: segment %d: %v", ErrCorrupt, sid, err)
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		db.segs[sid] = &segment{sid: sid, f: f, size: st.Size()}
		magic := make([]byte, len(segMagic))
		if _, err := f.ReadAt(magic, 0); err != nil || string(magic) != segMagic {
			return fmt.Errorf("%w: segment %d: bad magic", ErrCorrupt, sid)
		}
	}
	if len(order) == 0 {
		return fmt.Errorf("%w: empty manifest", ErrCorrupt)
	}
	db.active = db.segs[order[len(order)-1]]

	// use the checkpoint only if it is consistent with what is on disk
	covered := map[uint32]segState{}
	if states, t, ok := readSnapshot(db.dir); ok {
		for _, s := range states {
			if sg := db.segs[s.sid]; sg == nil || sg.size < s.size {
				ok = false
				break
			}
		}
		if ok {
			db.t = t
			for _, s := range states {
				covered[s.sid] = s
				db.segs[s.sid].dead = s.dead
			}
		}
	}
	for _, sid := range order {
		start := int64(len(segMagic))
		if c, ok := covered[sid]; ok {
			start = c.size
		}
		if err := db.replay(db.segs[sid], start, sid == db.active.sid); err != nil {
			return err
		}
	}
	return nil
}

// replay applies the records of seg from offset start to the index. A torn
// tail is truncated in the active segment and is corruption anywhere else.
func (db *DB[T]) replay(seg *segment, start int64, last bool) error {
	type pend struct {
		key  string
		l    loc
		del  bool
		vlen int
	}
	var batch []pend
	good := start
	scanSegment(seg.f, start, seg.size, func(r *rec) bool {
		batch = append(batch, pend{string(r.key), loc{seg.sid, uint32(r.voff()), r.vlen}, r.del, int(r.vlen)})
		if !r.more { // commit point: the record, or the end of a batch
			for _, p := range batch {
				db.apply(p.key, p.l, p.del, p.vlen)
			}
			batch = batch[:0]
			good = r.off + r.size()
		}
		return true
	})
	if good != seg.size {
		if !last {
			return fmt.Errorf("%w: segment %d damaged at offset %d", ErrCorrupt, seg.sid, good)
		}
		if err := seg.f.Truncate(good); err != nil {
			return err
		}
		seg.size = good
	}
	return nil
}

// apply updates the index and dead-byte accounting for one committed record.
func (db *DB[T]) apply(key string, l loc, del bool, vlen int) {
	var old loc
	var had bool
	if del {
		old, had = db.t.del([]byte(key))
	} else {
		old, had = db.t.set([]byte(key), l)
	}
	if had {
		if s := db.segs[old.sid]; s != nil {
			s.dead += recSize(len(key), int(old.vlen))
			db.maybeKick(s)
		}
	}
	if del { // a tombstone is itself dead weight in its segment
		db.segs[l.sid].dead += recSize(len(key), 0)
	}
}

func (db *DB[T]) maybeKick(s *segment) {
	if db.opts.CompactRatio < 0 || db.active == nil || s == db.active {
		return
	}
	if float64(s.dead) >= db.opts.CompactRatio*float64(s.size) {
		select {
		case db.kick <- struct{}{}:
		default:
		}
	}
}

// rotate closes the active segment (making it immutable) and starts a new one.
func (db *DB[T]) rotate() error {
	old := db.active
	if err := old.f.Sync(); err != nil {
		return err
	}
	sid := db.nextSid
	s, err := db.newSegment(sid)
	if err != nil {
		return err
	}
	order := append(append([]uint32(nil), db.order...), sid)
	if err := writeManifest(db.dir, order, sid+1); err != nil {
		s.f.Close()
		os.Remove(segPath(db.dir, sid))
		return err
	}
	db.nextSid = sid + 1
	db.order = order
	db.segs[sid] = s
	db.active = s
	db.dirty = false
	db.maybeKick(old)
	return nil
}

// write appends buf (the encoded records described by entries) to the active
// segment, rotating first if it is full, and registers them in the index.
func (db *DB[T]) write(buf []byte, entries []entry) error {
	if db.closed {
		return ErrClosed
	}
	a := db.active
	if a.size > int64(len(segMagic)) && a.size+int64(len(buf)) > db.opts.SegmentSize {
		if err := db.rotate(); err != nil {
			return err
		}
		a = db.active
	}
	if _, err := a.f.WriteAt(buf, a.size); err != nil {
		return err // size unchanged: a partial tail is overwritten or cut off on reopen
	}
	if db.opts.SyncAlways {
		if err := a.f.Sync(); err != nil {
			return err
		}
	} else {
		db.dirty = true
	}
	off := a.size
	a.size += int64(len(buf))
	for _, e := range entries {
		db.apply(e.key, loc{a.sid, uint32(off + recHeader + int64(len(e.key))), uint32(e.vlen)}, e.del, e.vlen)
		off += recSize(len(e.key), e.vlen)
	}
	return nil
}

// Put stores v under key, replacing any previous value.
func (db *DB[T]) Put(key string, v *T) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.put(key, v)
}

func (db *DB[T]) put(key string, v *T) error {
	if db.closed {
		return ErrClosed
	}
	if len(key) > maxKey {
		return ErrTooLarge
	}
	buf := append(db.wbuf[:0], make([]byte, recHeader)...)
	buf = append(buf, key...)
	vstart := len(buf)
	appendFn := codec.AppendEvolvable
	if db.opts.Strict {
		appendFn = codec.Append
	}
	buf, err := appendFn(buf, v)
	if err != nil {
		return err
	}
	vlen := len(buf) - vstart
	if vlen > lenMask {
		return ErrTooLarge
	}
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(key)))
	binary.LittleEndian.PutUint32(buf[8:], uint32(vlen))
	binary.LittleEndian.PutUint32(buf, crc32.Checksum(buf[4:], crcTable))
	if cap(buf) <= 4<<20 { // keep the scratch buffer, unless one huge value inflated it
		db.wbuf = buf[:0]
	}
	return db.write(buf, []entry{{key: key, vlen: vlen}})
}

// Delete removes key. It returns ErrNotFound if the key does not exist.
func (db *DB[T]) Delete(key string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if _, ok := db.t.get([]byte(key)); !ok {
		return ErrNotFound
	}
	buf := appendRec(db.wbuf[:0], key, nil, true, false)
	db.wbuf = buf[:0]
	return db.write(buf, []entry{{key: key, del: true}})
}

// Get decodes the value stored under key into v, overwriting it. It returns
// ErrNotFound if the key does not exist.
func (db *DB[T]) Get(key string, v *T) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.get(key, v)
}

func (db *DB[T]) get(key string, v *T) error {
	return db.read(key, func(b []byte) error { return codec.Unmarshal(b, v) })
}

// read gives fn the raw value bytes of key; they are valid only during fn.
// Caller holds db.mu (read or write).
func (db *DB[T]) read(key string, fn func(raw []byte) error) error {
	if db.closed {
		return ErrClosed
	}
	l, ok := db.t.get([]byte(key))
	if !ok {
		return ErrNotFound
	}
	bp := db.rpool.Get().(*[]byte)
	b := *bp
	if cap(b) < int(l.vlen) {
		b = make([]byte, l.vlen)
	}
	b = b[:l.vlen]
	_, err := db.segs[l.sid].f.ReadAt(b, int64(l.off))
	if err == nil {
		err = fn(b)
	}
	if cap(b) <= 1<<20 { // do not pin huge buffers in the pool
		*bp = b
		db.rpool.Put(bp)
	}
	return err
}

// View calls fn with the raw encoded value of key. The bytes are only valid
// during the call; use codec.View on them for zero-copy field access, or
// codec.UnmarshalNoCopy for allocation-free decoding. fn must not call
// methods that write to the DB.
func (db *DB[T]) View(key string, fn func(raw []byte) error) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.read(key, fn)
}

// Has reports whether key exists.
func (db *DB[T]) Has(key string) bool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	_, ok := db.t.get([]byte(key))
	return ok
}

// Len returns the number of live keys.
func (db *DB[T]) Len() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.t.n
}

// Update is an atomic read-modify-write. fn receives the current value (the
// zero value if the key does not exist, with exists == false) and may change
// it. If fn returns nil the result is stored; if it returns an error nothing
// is written and the error is returned. No other write can interleave, so fn
// should be quick and must not call DB methods.
func (db *DB[T]) Update(key string, fn func(v *T, exists bool) error) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	var v T
	exists := true
	if err := db.get(key, &v); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		exists = false
	}
	if err := fn(&v, exists); err != nil {
		return err
	}
	return db.put(key, &v)
}

// Batch collects writes that Commit applies atomically.
type Batch[T any] struct {
	db   *DB[T]
	recs []batchRec
}

type batchRec struct {
	key string
	val []byte
	del bool
}

// NewBatch starts a batch. It is not tied to the DB until Commit.
func (db *DB[T]) NewBatch() *Batch[T] { return &Batch[T]{db: db} }

// Put queues a write.
func (b *Batch[T]) Put(key string, v *T) error {
	marshal := codec.MarshalEvolvable
	if b.db.opts.Strict {
		marshal = codec.Marshal
	}
	val, err := marshal(v)
	if err != nil {
		return err
	}
	b.recs = append(b.recs, batchRec{key: key, val: val})
	return nil
}

// Delete queues a delete. Deleting a missing key in a batch is not an error.
func (b *Batch[T]) Delete(key string) { b.recs = append(b.recs, batchRec{key: key, del: true}) }

// Len is the number of queued operations.
func (b *Batch[T]) Len() int { return len(b.recs) }

// Commit writes all queued operations with a single write call. After a crash
// either every operation is visible or none is. The batch is empty afterwards.
func (b *Batch[T]) Commit() error {
	db := b.db
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if len(b.recs) == 0 {
		return nil
	}
	buf := db.wbuf[:0]
	entries := make([]entry, len(b.recs))
	for i, r := range b.recs {
		if len(r.key) > maxKey || len(r.val) > lenMask || len(buf) > 1<<30 {
			return ErrTooLarge
		}
		buf = appendRec(buf, r.key, r.val, r.del, i < len(b.recs)-1)
		entries[i] = entry{key: r.key, vlen: len(r.val), del: r.del}
	}
	if cap(buf) <= 4<<20 {
		db.wbuf = buf[:0]
	}
	b.recs = b.recs[:0]
	return db.write(buf, entries)
}

// Sync flushes buffered writes to stable storage.
func (db *DB[T]) Sync() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.sync()
}

func (db *DB[T]) sync() error {
	if db.closed {
		return ErrClosed
	}
	db.dirty = false
	return db.active.f.Sync()
}

func (db *DB[T]) syncLoop() {
	defer db.wg.Done()
	t := time.NewTicker(db.opts.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			db.mu.Lock()
			if !db.closed && db.dirty {
				_ = db.sync()
			}
			db.mu.Unlock()
		case <-db.done:
			return
		}
	}
}

// Stats describes the store.
type Stats struct {
	Keys      int
	Segments  int
	FileBytes int64
	DeadBytes int64 // reclaimable by compaction
}

func (db *DB[T]) Stats() Stats {
	db.mu.RLock()
	defer db.mu.RUnlock()
	s := Stats{Keys: db.t.n, Segments: len(db.order)}
	for _, sg := range db.segs {
		s.FileBytes += sg.size
		s.DeadBytes += sg.dead
	}
	return s
}

// Checkpoint saves the key index so the next Open does not have to replay the
// log. It also runs on Close. Writers wait while it runs; it takes time
// proportional to the number of keys.
func (db *DB[T]) Checkpoint() error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return ErrClosed
	}
	return db.checkpoint()
}

func (db *DB[T]) checkpoint() error { // caller holds mu
	db.snapMu.Lock()
	defer db.snapMu.Unlock()
	if err := db.active.f.Sync(); err != nil { // the checkpoint may only describe durable bytes
		return err
	}
	states := make([]segState, 0, len(db.order))
	for _, sid := range db.order {
		s := db.segs[sid]
		states = append(states, segState{sid, s.size, s.dead})
	}
	return writeSnapshot(db.dir, states, db.t)
}

// ExportText writes every entry in the Strata text notation, in key order:
// one `key.path value` line per field, with the key as the first name of the
// path, e.g.
//
//	user:1.Name Ada
//	user:1.Tags admin dev
//	user:2.Name Bob
//
// The lines of one entry are adjacent, so the file can be read in a single
// pass, edited by hand, and fed back to ImportText.
func (db *DB[T]) ExportText(w io.Writer) error {
	bw := bufio.NewWriterSize(w, 1<<16)
	var line []byte
	var werr error
	err := db.Scan("*", func(k string, v *T) bool {
		line, werr = codec.AppendTextEntry(line[:0], k, v)
		if werr == nil {
			_, werr = bw.Write(line)
		}
		return werr == nil
	})
	if err == nil {
		err = werr
	}
	if err == nil {
		err = bw.Flush()
	}
	return err
}

// ImportText reads text written by ExportText (blank lines and # comments are
// skipped) and stores the entries in atomic batches of 1000. All lines of one
// key must be adjacent, one entry per line.
func (db *DB[T]) ImportText(r io.Reader) error {
	br := bufio.NewReaderSize(r, 1<<16)
	b := db.NewBatch()
	var group []byte
	var cur string
	groupLine := 0
	flush := func() error {
		if len(group) == 0 {
			return nil
		}
		var m map[string]T
		if err := codec.UnmarshalText(group, &m); err != nil {
			return fmt.Errorf("entry %q (from line %d): %w", cur, groupLine, err)
		}
		for k, v := range m {
			v := v
			if err := b.Put(k, &v); err != nil {
				return err
			}
		}
		group = group[:0]
		if b.Len() >= 1000 {
			return b.Commit()
		}
		return nil
	}
	for lineNo := 1; ; lineNo++ {
		line, rerr := br.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 && t[0] != '#' {
			key, err := codec.TextKey(t)
			if err != nil {
				return fmt.Errorf("line %d: %w", lineNo, err)
			}
			if key != cur || len(group) == 0 {
				if err := flush(); err != nil {
					return err
				}
				cur, groupLine = key, lineNo
			}
			group = append(group, line...)
			if line[len(line)-1] != '\n' {
				group = append(group, '\n')
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return b.Commit()
}

// Close stops background work, writes a checkpoint and closes all files.
func (db *DB[T]) Close() error {
	db.once.Do(func() { close(db.done) })
	db.wg.Wait()
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	err := db.checkpoint()
	db.closed = true
	for _, s := range db.segs {
		if cerr := s.f.Close(); err == nil {
			err = cerr
		}
	}
	return err
}
