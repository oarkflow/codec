// Package store is a persistent, typed key-value store built on the Strata
// codec, designed to stay fast and light as it grows.
//
// Design (a segmented log-structured store with an ordered in-memory index):
//
//   - Writes append records to the active segment file; nothing is ever
//     rewritten in place. When a segment reaches Options.SegmentSize a new one
//     starts, so no single file grows without bound.
//   - The index is a compressed radix tree from key to the location of the
//     latest value (24 bytes of location metadata per key plus shared key prefixes). A Get is one
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
	"container/list"
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
	ErrNotFound    = errors.New("store: key not found")
	ErrClosed      = errors.New("store: closed")
	ErrTooLarge    = errors.New("store: key, value or batch too large")
	ErrCorrupt     = errors.New("store: corrupt data")
	ErrInvalidTTL  = errors.New("store: TTL must be positive")
	ErrTTLTooLarge = errors.New("store: TTL exceeds the supported expiry range")
	ErrExpiryRange = errors.New("store: expiry time is outside the supported range")
)

const maxBatchSize = int64(1 << 30)

// Options configures Open. The zero value is a good default.
type Options struct {
	// SegmentSize is the size at which the active segment is closed and a new
	// one started (default 64 MiB, at most 2 GiB). Compaction works one run of
	// segments at a time, so smaller segments mean finer-grained compaction.
	SegmentSize int64
	// SyncAlways waits for each write to reach durable storage. This protects
	// against power loss but is much slower. Without it, data survives a process
	// crash, but the last few writes can be lost on power failure unless Sync is
	// called.
	SyncAlways bool
	// SyncInterval, if > 0 and SyncAlways is false, fsyncs in the background.
	SyncInterval time.Duration
	// ExpiryInterval controls automatic removal of expired keys (default 1s;
	// negative disables the expiry worker).
	ExpiryInterval time.Duration
	// ReadCacheBytes bounds the optional read-through cache of encoded values.
	// Zero disables caching. The limit counts key and value bytes, not map overhead.
	ReadCacheBytes int64
	// StreamPageBytes bounds encoded value bytes retained per streaming page
	// (default 8 MiB). A single value larger than the limit is streamed alone.
	StreamPageBytes int64
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
	v2         bool
	dsync      bool
	mapped     []byte // read-only mapping; active maps may lag the append-only tail
	mapFailed  bool
}

// DB is a store of values of type T keyed by string.
type DB[T any] struct {
	mu          sync.RWMutex
	dir         string
	t           *tree
	segs        map[uint32]*segment
	order       []uint32 // segment ids, oldest first; the last is active
	active      *segment
	nextSid     uint32
	opts        Options
	wbuf        []byte
	rpool       sync.Pool
	cacheMu     sync.RWMutex
	cache       map[string]*list.Element
	cacheFIFO   list.List
	cacheBytes  int64
	treeBufPool sync.Pool // reusable radix-tree scan path buffers
	indexGen    uint64    // advances when a write or compaction changes indexed locations
	closed      bool
	closeErr    error
	temp        bool // remove the directory when closed (OpenTemp only)
	dirty       bool
	expires     expiryQueue
	expiryByKey map[string]*expiryItem

	cmu    sync.Mutex // serializes compactions
	snapMu sync.Mutex // serializes checkpoints
	kick   chan struct{}
	done   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

// OpenTemp creates an isolated temporary store. Its directory is removed by
// Close. Use Open when the data must survive process exit.
func OpenTemp[T any](opts *Options) (*DB[T], error) {
	dir, err := os.MkdirTemp("", "codec-store-*")
	if err != nil {
		return nil, err
	}
	db, err := Open[T](dir, opts)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	db.temp = true
	return db, nil
}

type entry struct {
	key       string
	vlen      int
	del       bool
	expiresAt int64
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
	if db.opts.StreamPageBytes <= 0 {
		db.opts.StreamPageBytes = 8 << 20
	}
	db.rpool.New = func() any { b := make([]byte, 0, 512); return &b }
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := db.load(); err != nil {
		db.discardOpenSegments()
		return nil, err
	}
	// Upgrade only the append target. Existing v1 segments remain readable and
	// are rewritten by compaction when space reclamation runs.
	if !db.active.v2 {
		if err := db.rotate(); err != nil {
			db.discardOpenSegments()
			return nil, err
		}
	}
	db.rebuildExpiryQueue()
	if db.opts.CompactRatio >= 0 {
		db.wg.Add(1)
		go db.compactLoop()
	}
	if db.opts.SyncInterval > 0 && !db.opts.SyncAlways {
		db.wg.Add(1)
		go db.syncLoop()
	}
	if db.opts.ExpiryInterval == 0 {
		db.opts.ExpiryInterval = time.Second
	}
	if db.opts.ExpiryInterval > 0 {
		db.wg.Add(1)
		go db.expiryLoop()
	}
	return db, nil
}

// discardOpenSegments releases resources when Open fails before returning a DB.
func (db *DB[T]) discardOpenSegments() {
	for _, s := range db.segs {
		s.unmap()
		_ = s.f.Close()
	}
}

func (db *DB[T]) newSegment(sid uint32) (*segment, error) {
	f, dsync, err := openSegmentFile(segPath(db.dir, sid), true, db.opts.SyncAlways)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteAt([]byte(segMagic), 0); err != nil {
		f.Close()
		os.Remove(segPath(db.dir, sid))
		return nil, err
	}
	return &segment{sid: sid, f: f, size: int64(len(segMagic)), v2: true, dsync: dsync}, nil
}

// ensureActiveMapped keeps a read-only mapping of the active segment through
// its current logical size. Appends beyond the mapping remain readable through
// ReadAt until the next scan refreshes it. The write lock excludes readers
// while a stale mapping is replaced.
func (db *DB[T]) ensureActiveMapped() {
	db.mu.RLock()
	active := db.active
	stale := !db.closed && active != nil && !active.mapFailed && int64(len(active.mapped)) < active.size
	db.mu.RUnlock()
	if !stale {
		return
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed || db.active == nil || db.active.mapFailed || int64(len(db.active.mapped)) >= db.active.size {
		return
	}
	db.active.unmap()
	db.active.mapReadOnly()
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
		if err := writeManifest(db.dir, db.order, db.nextSid); err != nil {
			_ = s.f.Close()
			_ = os.Remove(segPath(db.dir, s.sid))
			delete(db.segs, s.sid)
			db.order, db.active = nil, nil
			return err
		}
		return nil
	}
	live := map[uint32]bool{}
	for _, sid := range order {
		live[sid] = true
	}
	db.nextSid = max(next, removeOrphans(db.dir, live)+1)
	db.order = order
	for _, sid := range order {
		f, dsync, err := openSegmentFile(segPath(db.dir, sid), false, db.opts.SyncAlways && sid == order[len(order)-1])
		if err != nil {
			return fmt.Errorf("%w: segment %d: %v", ErrCorrupt, sid, err)
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		db.segs[sid] = &segment{sid: sid, f: f, size: st.Size(), dsync: dsync}
		magic := make([]byte, len(segMagic))
		if _, err := f.ReadAt(magic, 0); err != nil || (string(magic) != segMagic && string(magic) != segMagicV1) {
			return fmt.Errorf("%w: segment %d: bad magic", ErrCorrupt, sid)
		}
		db.segs[sid].v2 = string(magic) == segMagic
	}
	if len(order) == 0 {
		return fmt.Errorf("%w: empty manifest", ErrCorrupt)
	}
	db.active = db.segs[order[len(order)-1]]

	// use the checkpoint only if it is consistent with what is on disk
	covered := map[uint32]segState{}
	if states, t, ok := readSnapshot(db.dir, db.segs); ok {
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
	for _, sid := range order {
		if db.segs[sid] != db.active {
			db.segs[sid].mapReadOnly()
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
		batch = append(batch, pend{string(r.key), loc{seg: seg, off: uint32(r.voff()), vlen: r.vlen, expiresAt: r.expiresAt}, r.del, int(r.vlen)})
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
	db.cacheDelete(key)
	var old loc
	var had bool
	if del {
		old, had = db.t.del([]byte(key))
	} else {
		old, had = db.t.set([]byte(key), l)
	}
	if had {
		if s := old.seg; s != nil {
			if s.v2 {
				s.dead += recSize(len(key), int(old.vlen))
			} else {
				s.dead += recSizeV1(len(key), int(old.vlen))
			}
			db.maybeKick(s)
		}
	}
	if del { // a tombstone is itself dead weight in its segment
		if s := l.seg; s != nil {
			if s.v2 {
				s.dead += recSize(len(key), 0)
			} else {
				s.dead += recSizeV1(len(key), 0)
			}
		}
	}
	db.scheduleExpiry(key, l, !del)
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
	old.unmap()
	old.mapReadOnly()
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
	if db.opts.SyncAlways && !a.dsync {
		if err := a.f.Sync(); err != nil {
			return err
		}
	} else {
		db.dirty = true
	}
	off := a.size
	a.size += int64(len(buf))
	for _, e := range entries {
		db.apply(e.key, loc{seg: a, off: uint32(off + recHeader + int64(len(e.key))), vlen: uint32(e.vlen), expiresAt: e.expiresAt}, e.del, e.vlen)
		off += recSize(len(e.key), e.vlen)
	}
	db.indexGen++
	return nil
}

// Put stores v under key, replacing any previous value.
func (db *DB[T]) Put(key string, v *T) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.put(key, v)
}

// PutTTL stores v until ttl elapses, starting after the value is encoded. A
// nonpositive duration is rejected.
func (db *DB[T]) PutTTL(key string, v *T, ttl time.Duration) error {
	if ttl <= 0 {
		return ErrInvalidTTL
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.putWithTTL(key, v, ttl)
}

// PutUntil stores v with an absolute expiry. A zero time means no expiry.
func (db *DB[T]) PutUntil(key string, v *T, expiresAt time.Time) error {
	ns, err := expiryTimestamp(expiresAt)
	if err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.putUntil(key, v, ns)
}

// PutIfAbsent stores v only when key is missing or expired. It returns true
// when the value was written. The check and write are atomic.
func (db *DB[T]) PutIfAbsent(key string, v *T) (bool, error) {
	return db.putIfAbsent(key, v, 0)
}

// PutIfAbsentTTL is PutIfAbsent with an expiry starting after the value is
// encoded. A nonpositive ttl is rejected.
func (db *DB[T]) PutIfAbsentTTL(key string, v *T, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, ErrInvalidTTL
	}
	return db.putIfAbsent(key, v, ttl)
}

func (db *DB[T]) putIfAbsent(key string, v *T, ttl time.Duration) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return false, ErrClosed
	}
	if l, ok := db.t.getString(key); ok && (l.expiresAt == 0 || time.Now().UnixNano() < l.expiresAt) {
		return false, nil
	}
	if ttl > 0 {
		if err := db.putWithTTL(key, v, ttl); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := db.putUntil(key, v, 0); err != nil {
		return false, err
	}
	return true, nil
}

// expiryAfter returns an absolute Unix-nanosecond deadline without relying on
// time.Time.UnixNano outside its representable range (which wraps for dates
// after 2262).
func expiryAfter(now time.Time, ttl time.Duration) (int64, error) {
	if ttl <= 0 {
		return 0, ErrInvalidTTL
	}
	nowNS := now.UnixNano()
	if int64(ttl) > int64(^uint64(0)>>1)-nowNS {
		return 0, ErrTTLTooLarge
	}
	return nowNS + int64(ttl), nil
}

func expiryTimestamp(at time.Time) (int64, error) {
	if at.IsZero() {
		return 0, nil
	}
	if at.Before(time.Unix(0, -1<<63)) || at.After(time.Unix(0, 1<<63-1)) {
		return 0, ErrExpiryRange
	}
	return at.UnixNano(), nil
}

// Expire updates key to expire after ttl without decoding or re-encoding its
// value. A nonpositive duration is rejected; a missing or already expired key
// returns ErrNotFound.
func (db *DB[T]) Expire(key string, ttl time.Duration) error {
	if ttl <= 0 {
		return ErrInvalidTTL
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.updateExpiry(key, 0, ttl)
}

// Persist removes key's expiry. It is a no-op for a live key without expiry.
func (db *DB[T]) Persist(key string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.updateExpiry(key, 0, 0)
}

// RemainingTTL returns the time until key expires. A zero duration means the
// key has no expiry. Missing or expired keys return ErrNotFound.
func (db *DB[T]) RemainingTTL(key string) (time.Duration, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return 0, ErrClosed
	}
	l, ok := db.t.getString(key)
	if !ok {
		return 0, ErrNotFound
	}
	if l.expiresAt == 0 {
		return 0, nil
	}
	remaining := l.expiresAt - time.Now().UnixNano()
	if remaining <= 0 {
		return 0, ErrNotFound
	}
	return time.Duration(remaining), nil
}

func (db *DB[T]) updateExpiry(key string, expiresAt int64, ttl time.Duration) error {
	if db.closed {
		return ErrClosed
	}
	l, ok := db.t.getString(key)
	if !ok || (l.expiresAt != 0 && time.Now().UnixNano() >= l.expiresAt) {
		return ErrNotFound
	}
	if ttl == 0 && l.expiresAt == expiresAt {
		return nil
	}
	var buf []byte
	err := db.readLoc(key, l, func(raw []byte) error {
		if ttl > 0 {
			buf = append(db.wbuf[:0], make([]byte, recHeader)...)
			buf = append(buf, key...)
			buf = append(buf, raw...)
		} else {
			buf = appendRecTTL(db.wbuf[:0], key, raw, false, false, expiresAt)
		}
		return nil
	}, false)
	if err != nil {
		return err
	}
	if ttl > 0 {
		expiresAt, err = expiryAfter(time.Now(), ttl)
		if err != nil {
			return err
		}
		vlen := len(buf) - recHeader - len(key)
		binary.LittleEndian.PutUint32(buf[4:], uint32(len(key)))
		binary.LittleEndian.PutUint32(buf[8:], uint32(vlen))
		binary.LittleEndian.PutUint64(buf[12:], uint64(expiresAt))
		binary.LittleEndian.PutUint32(buf, crc32.Checksum(buf[4:], crcTable))
	}
	if cap(buf) <= 4<<20 {
		db.wbuf = buf[:0]
	}
	return db.write(buf, []entry{{key: key, vlen: len(buf) - recHeader - len(key), expiresAt: expiresAt}})
}

func (db *DB[T]) put(key string, v *T) error {
	return db.putUntil(key, v, 0)
}

func (db *DB[T]) putUntil(key string, v *T, expiresAt int64) error {
	return db.putEncoded(key, v, expiresAt, 0)
}

func (db *DB[T]) putWithTTL(key string, v *T, ttl time.Duration) error {
	return db.putEncoded(key, v, 0, ttl)
}

func (db *DB[T]) putEncoded(key string, v *T, expiresAt int64, ttl time.Duration) error {
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
	if ttl > 0 {
		expiresAt, err = expiryAfter(time.Now(), ttl)
		if err != nil {
			return err
		}
	}
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(key)))
	binary.LittleEndian.PutUint32(buf[8:], uint32(vlen))
	binary.LittleEndian.PutUint64(buf[12:], uint64(expiresAt))
	binary.LittleEndian.PutUint32(buf, crc32.Checksum(buf[4:], crcTable))
	if cap(buf) <= 4<<20 { // keep the scratch buffer, unless one huge value inflated it
		db.wbuf = buf[:0]
	}
	return db.write(buf, []entry{{key: key, vlen: vlen, expiresAt: expiresAt}})
}

// Delete removes key. It returns ErrNotFound if the key does not exist.
func (db *DB[T]) Delete(key string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	l, ok := db.t.getString(key)
	if !ok || (l.expiresAt != 0 && time.Now().UnixNano() >= l.expiresAt) {
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

// GetBorrowed decodes key into a temporary value and calls fn while holding a
// read lock. Strings (and byte slices inside interface values) may refer to
// the store's encoded bytes; fn must consume the value before returning and
// must not call DB methods. Use Get when the value must outlive the callback.
func (db *DB[T]) GetBorrowed(key string, fn func(*T) error) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return ErrClosed
	}
	l, ok := db.t.getString(key)
	if !ok || (l.expiresAt != 0 && time.Now().UnixNano() >= l.expiresAt) {
		return ErrNotFound
	}
	var value T
	return db.readLoc(key, l, func(raw []byte) error {
		if err := codec.UnmarshalNoCopy(raw, &value); err != nil {
			return err
		}
		return fn(&value)
	}, false)
}

// GetMany reads keys in input order. values and found have the same length as
// keys; missing or expired keys have found[i] == false and a zero value in
// values[i]. Any storage or decode error fails the whole call. Reads are
// grouped into bounded chunks, so writes can proceed between chunks and may
// be reflected in later results.
func (db *DB[T]) GetMany(keys []string) (values []T, found []bool, err error) {
	values = make([]T, len(keys))
	found = make([]bool, len(keys))
	if len(keys) == 0 {
		db.mu.RLock()
		defer db.mu.RUnlock()
		if db.closed {
			return nil, nil, ErrClosed
		}
		return values, found, nil
	}
	db.ensureActiveMapped()
	var decoder *codec.ValueDecoder[T]
	for base := 0; base < len(keys); base += scanChunk {
		end := min(base+scanChunk, len(keys))
		db.mu.RLock()
		if db.closed {
			db.mu.RUnlock()
			return nil, nil, ErrClosed
		}
		now := time.Now().UnixNano()
		for i := base; i < end; i++ {
			l, ok := db.t.getString(keys[i])
			if !ok || (l.expiresAt != 0 && now >= l.expiresAt) {
				continue
			}
			if decoder == nil {
				decoder, err = codec.NewValueDecoder[T]()
				if err != nil {
					db.mu.RUnlock()
					return nil, nil, err
				}
			}
			err = db.readLoc(keys[i], l, func(raw []byte) error {
				return decoder.Unmarshal(raw, &values[i])
			}, true)
			if err != nil {
				db.mu.RUnlock()
				return nil, nil, err
			}
			found[i] = true
		}
		db.mu.RUnlock()
	}
	return values, found, nil
}

// GetManyBorrowed reads keys in input order and calls fn for each result.
// Missing or expired keys are reported with found == false. The *T is reused;
// decoded slices and strings are borrowed only for the callback. The callback
// runs under a read lock for one 256-key chunk and must not call DB methods or
// retain the value or its fields. Returning false stops the read. Writes may
// proceed between chunks.
func (db *DB[T]) GetManyBorrowed(keys []string, fn func(key string, value *T, found bool) bool) error {
	if len(keys) == 0 {
		db.mu.RLock()
		defer db.mu.RUnlock()
		if db.closed {
			return ErrClosed
		}
		return nil
	}
	db.ensureActiveMapped()
	var decoder *codec.ValueDecoder[T]
	var value T
	for base := 0; base < len(keys); base += scanChunk {
		end := min(base+scanChunk, len(keys))
		keepGoing, err := db.getManyBorrowedChunk(keys, base, end, &value, &decoder, fn)
		if err != nil {
			return err
		}
		if !keepGoing {
			return nil
		}
	}
	return nil
}

func (db *DB[T]) getManyBorrowedChunk(keys []string, start, end int, value *T, decoder **codec.ValueDecoder[T], fn func(string, *T, bool) bool) (keepGoing bool, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return false, ErrClosed
	}
	now := time.Now().UnixNano()
	var zero T
	for i := start; i < end; i++ {
		l, ok := db.t.getString(keys[i])
		if !ok || (l.expiresAt != 0 && now >= l.expiresAt) {
			*value = zero
			if !fn(keys[i], value, false) {
				return false, nil
			}
			continue
		}
		keepGoing = true
		readErr := db.readLoc(keys[i], l, func(raw []byte) error {
			if *decoder == nil {
				*decoder, err = codec.NewValueDecoder[T]()
				if err != nil {
					return err
				}
			}
			if err := (*decoder).UnmarshalNoCopy(raw, value); err != nil {
				return err
			}
			keepGoing = fn(keys[i], value, true)
			return nil
		}, false)
		if readErr != nil {
			return false, readErr
		}
		if !keepGoing {
			return false, nil
		}
	}
	return true, nil
}

func (db *DB[T]) get(key string, v *T) error {
	return db.readWithCache(key, func(b []byte) error { return codec.Unmarshal(b, v) }, true)
}

// read gives fn the raw value bytes of key; they are valid only during fn.
// Caller holds db.mu (read or write).
func (db *DB[T]) read(key string, fn func(raw []byte) error) error {
	return db.readWithCache(key, fn, false)
}

func (db *DB[T]) readWithCache(key string, fn func(raw []byte) error, useCache bool) error {
	if db.closed {
		return ErrClosed
	}
	l, ok := db.t.getString(key)
	if !ok || (l.expiresAt != 0 && time.Now().UnixNano() >= l.expiresAt) {
		return ErrNotFound
	}
	return db.readLoc(key, l, fn, useCache)
}

// readLoc reads a location while the caller holds db.mu.
func (db *DB[T]) readLoc(key string, l loc, fn func(raw []byte) error, useCache bool) error {
	return db.readLocWithMap(key, l, fn, useCache)
}

func (db *DB[T]) readLocWithMap(key string, l loc, fn func(raw []byte) error, useCache bool) error {
	if useCache {
		if raw, hit := db.cacheGet(key, l); hit {
			return fn(raw)
		}
	}
	start, end := int(l.off), int(l.off)+int(l.vlen)
	sg := l.seg
	if len(sg.mapped) > 0 && start >= 0 && end >= start && end <= len(sg.mapped) {
		raw := sg.mapped[start:end]
		if useCache {
			db.cachePut(key, l, raw)
		}
		return fn(raw)
	}
	bp := db.rpool.Get().(*[]byte)
	b := *bp
	if cap(b) < int(l.vlen) {
		b = make([]byte, l.vlen)
	}
	b = b[:l.vlen]
	_, err := sg.f.ReadAt(b, int64(l.off))
	if err == nil {
		if useCache {
			db.cachePut(key, l, b)
		}
		err = fn(b)
	}
	if cap(b) <= 1<<20 { // do not pin huge buffers in the pool
		*bp = b
		db.rpool.Put(bp)
	}
	return err
}

// View calls fn with the raw encoded value of key. The bytes are only valid
// during the call and must be treated as read-only. Use codec.View on them for
// zero-copy field access, or codec.UnmarshalNoCopy for allocation-free
// decoding. fn must not call methods on the DB.
func (db *DB[T]) View(key string, fn func(raw []byte) error) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.read(key, fn)
}

// Has reports whether key exists.
func (db *DB[T]) Has(key string) bool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	l, ok := db.t.getString(key)
	return ok && (l.expiresAt == 0 || time.Now().UnixNano() < l.expiresAt)
}

// Len returns the number of live keys.
func (db *DB[T]) Len() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	now := time.Now().UnixNano()
	if db.expires.Len() == 0 || db.expires[0].at > now {
		return db.t.n
	}
	n := 0
	db.t.scan(nil, nil, false, func(_ []byte, l loc) bool {
		if l.expiresAt == 0 || now < l.expiresAt {
			n++
		}
		return true
	})
	return n
}

// Update is an atomic read-modify-write. fn receives the current value (the
// zero value if the key does not exist, with exists == false) and may change
// it. If fn returns nil the result is stored; if it returns an error nothing
// is written and the error is returned. No other write can interleave, so fn
// should be quick and must not call DB methods. Updating an existing key
// preserves its expiry; creating a missing or expired key stores it without
// an expiry.
func (db *DB[T]) Update(key string, fn func(v *T, exists bool) error) error {
	return db.update(key, fn, 0, false)
}

// UpdateTTL atomically updates key and sets its expiry ttl after the updated
// value is encoded. If key is missing or expired, fn receives the zero value
// with exists == false. A callback error leaves the key unchanged. A
// nonpositive ttl is rejected. As with Update, fn must be quick and must not
// call DB methods.
func (db *DB[T]) UpdateTTL(key string, ttl time.Duration, fn func(v *T, exists bool) error) error {
	if ttl <= 0 {
		return ErrInvalidTTL
	}
	return db.update(key, fn, ttl, true)
}

func (db *DB[T]) update(key string, fn func(v *T, exists bool) error, ttl time.Duration, resetExpiry bool) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	var v T
	exists := true
	var expiresAt int64
	if err := db.get(key, &v); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		exists = false
	} else if l, ok := db.t.getString(key); ok {
		// Preserve a live key's expiry when updating its value. An expired key
		// is treated as absent by db.get and starts without an expiry if recreated.
		expiresAt = l.expiresAt
	}
	if err := fn(&v, exists); err != nil {
		return err
	}
	if resetExpiry {
		return db.putWithTTL(key, &v, ttl)
	}
	return db.putUntil(key, &v, expiresAt)
}

// Batch collects writes that Commit applies atomically.
type Batch[T any] struct {
	db    *DB[T]
	recs  []batchRec
	bytes int64
}

type batchRec struct {
	key       string
	val       []byte
	del       bool
	expiresAt int64
	ttl       time.Duration
}

// NewBatch starts a batch. It is not tied to the DB until Commit.
func (db *DB[T]) NewBatch() *Batch[T] { return &Batch[T]{db: db} }

// Put queues a write.
func (b *Batch[T]) Put(key string, v *T) error {
	return b.putUntil(key, v, 0)
}

// PutTTL queues a write that expires ttl after Commit. A nonpositive duration
// is rejected.
func (b *Batch[T]) PutTTL(key string, v *T, ttl time.Duration) error {
	if ttl <= 0 {
		return ErrInvalidTTL
	}
	return b.putExpiry(key, v, 0, ttl)
}

// PutUntil queues a write with an absolute expiry. A zero time means no expiry.
func (b *Batch[T]) PutUntil(key string, v *T, expiresAt time.Time) error {
	ns, err := expiryTimestamp(expiresAt)
	if err != nil {
		return err
	}
	return b.putUntil(key, v, ns)
}

func (b *Batch[T]) putUntil(key string, v *T, expiresAt int64) error {
	return b.putExpiry(key, v, expiresAt, 0)
}

func (b *Batch[T]) putExpiry(key string, v *T, expiresAt int64, ttl time.Duration) error {
	if len(key) > maxKey {
		return ErrTooLarge
	}
	marshal := codec.MarshalEvolvable
	if b.db.opts.Strict {
		marshal = codec.Marshal
	}
	val, err := marshal(v)
	if err != nil {
		return err
	}
	recordBytes := recSize(len(key), len(val))
	if b.bytes > maxBatchSize-recordBytes {
		return ErrTooLarge
	}
	b.recs = append(b.recs, batchRec{key: key, val: val, expiresAt: expiresAt, ttl: ttl})
	b.bytes += recordBytes
	return nil
}

// Delete queues a delete. Deleting a missing key in a batch is not an error.
func (b *Batch[T]) Delete(key string) error {
	if len(key) > maxKey {
		return ErrTooLarge
	}
	recordBytes := recSize(len(key), 0)
	if b.bytes > maxBatchSize-recordBytes {
		return ErrTooLarge
	}
	b.recs = append(b.recs, batchRec{key: key, del: true})
	b.bytes += recordBytes
	return nil
}

// Len is the number of queued operations.
func (b *Batch[T]) Len() int { return len(b.recs) }

// Reset discards all queued operations without writing them.
func (b *Batch[T]) Reset() {
	clear(b.recs)
	b.recs = b.recs[:0]
	b.bytes = 0
}

// Commit writes all queued operations with a single write call. After a crash
// either every operation is visible or none is. The batch is cleared on
// success; if Commit returns an error, the queued operations remain for retry.
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
	commitAt := time.Now()
	for i, r := range b.recs {
		if len(r.key) > maxKey || len(r.val) > lenMask || int64(len(buf))+recSize(len(r.key), len(r.val)) > maxBatchSize {
			return ErrTooLarge
		}
		expiresAt := r.expiresAt
		if r.ttl > 0 {
			deadline, err := expiryAfter(commitAt, r.ttl)
			if err != nil {
				return err
			}
			expiresAt = deadline
		}
		buf = appendRecTTL(buf, r.key, r.val, r.del, i < len(b.recs)-1, expiresAt)
		entries[i] = entry{key: r.key, vlen: len(r.val), del: r.del, expiresAt: expiresAt}
	}
	if cap(buf) <= 4<<20 {
		db.wbuf = buf[:0]
	}
	if err := db.write(buf, entries); err != nil {
		return err
	}
	b.Reset()
	return nil
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
	if err := db.active.f.Sync(); err != nil {
		return err
	}
	db.dirty = false
	return nil
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
	Keys          int
	Segments      int
	FileBytes     int64
	DeadBytes     int64 // reclaimable by compaction
	CachedBytes   int64
	CachedEntries int
}

func (db *DB[T]) Stats() Stats {
	db.mu.RLock()
	defer db.mu.RUnlock()
	s := Stats{Segments: len(db.order)}
	now := time.Now().UnixNano()
	if db.expires.Len() == 0 || db.expires[0].at > now {
		s.Keys = db.t.n
	} else {
		db.t.scan(nil, nil, false, func(_ []byte, l loc) bool {
			if l.expiresAt == 0 || now < l.expiresAt {
				s.Keys++
			}
			return true
		})
	}
	for _, sg := range db.segs {
		s.FileBytes += sg.size
		s.DeadBytes += sg.dead
	}
	db.cacheMu.RLock()
	s.CachedBytes, s.CachedEntries = db.cacheBytes, len(db.cache)
	db.cacheMu.RUnlock()
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

// Close stops background work, syncs appended records, writes a checkpoint and
// closes all files.
func (db *DB[T]) Close() error {
	db.once.Do(func() { close(db.done) })
	db.wg.Wait()
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return db.closeErr
	}
	// Flush appended records before saving an index checkpoint that points to
	// them. A failed sync must not leave a newer checkpoint claiming durability.
	err := db.sync()
	if err == nil {
		err = db.checkpoint()
	}
	db.closed = true
	for _, s := range db.segs {
		s.unmap()
		if cerr := s.f.Close(); err == nil {
			err = cerr
		}
	}
	db.clearReadCache()
	if db.temp {
		if rerr := os.RemoveAll(db.dir); err == nil {
			err = rerr
		}
	}
	db.closeErr = err
	return db.closeErr
}
