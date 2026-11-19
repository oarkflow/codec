package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"github.com/oarkflow/codec"
)

var ErrBadCursor = errors.New("store: invalid page cursor")

func encodePageCursor(pattern, key string) string {
	buf := make([]byte, 0, binary.MaxVarintLen64*2+len(pattern)+len(key))
	buf = binary.AppendUvarint(buf, uint64(len(pattern)))
	buf = append(buf, pattern...)
	buf = binary.AppendUvarint(buf, uint64(len(key)))
	buf = append(buf, key...)
	return "p1" + base64.RawURLEncoding.EncodeToString(buf)
}

func decodePageCursor(cursor, pattern string) (string, error) {
	if len(cursor) == 0 {
		return "", nil
	}
	if strings.HasPrefix(cursor, "p") {
		if !strings.HasPrefix(cursor, "p1") {
			return "", ErrBadCursor
		}
		b, err := base64.RawURLEncoding.DecodeString(cursor[2:])
		if err != nil {
			return "", ErrBadCursor
		}
		patternLen, n := binary.Uvarint(b)
		if n <= 0 || patternLen > uint64(len(b)-n) {
			return "", ErrBadCursor
		}
		b = b[n:]
		if uint64(len(pattern)) != patternLen || string(b[:patternLen]) != pattern {
			return "", ErrBadCursor
		}
		b = b[patternLen:]
		keyLen, n := binary.Uvarint(b)
		if n <= 0 || keyLen != uint64(len(b)-n) {
			return "", ErrBadCursor
		}
		return string(b[n:]), nil
	}
	// Accept key-only cursors issued before cursors were bound to a pattern.
	if cursor[0] != 'c' {
		return "", ErrBadCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor[1:])
	if err != nil {
		return "", ErrBadCursor
	}
	return string(b), nil
}

// Item is one key/value returned by Page or Stream. Values are independent
// copies and remain valid after the next item is read.
type Item[T any] struct {
	Key   string
	Value T
}

// StreamResult is either one streamed item or the terminal scan error.
type StreamResult[T any] struct {
	Item Item[T]
	Err  error
}

// Escape quotes the pattern metacharacters in s so it matches literally.
func Escape(s string) string {
	if !strings.ContainsAny(s, `*?[\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '*' || c == '?' || c == '[' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// MatchKey reports whether key matches the wildcard pattern (see the pattern
// syntax below).
//
// Patterns are used by Scan, ScanKeys, Match, Count and DeleteMatch:
//
//   - any run of characters, including none (and including '/')
//     ?        exactly one character
//     [abc]    one character from the set      [a-z]  a range
//     [^abc]   one character not in the set    ([!abc] works too)
//     \x       the character x literally
//
// Keys are visited in sorted order. Everything before the first wildcard is a
// literal prefix and only keys under that prefix are examined, so
// "user:42:*" costs time proportional to user 42's keys. A pattern that
// starts with a wildcard ("*:email") has to examine every key; it still only
// reads the values of keys that match.
func MatchKey(pattern, key string) (bool, error) {
	g, err := compile(pattern)
	if err != nil {
		return false, err
	}
	return g.match([]byte(key)), nil
}

const scanChunk = 256
const ownedRangeChunkBytes = 8 << 20

// borrowedRangeChunk amortizes lock and cursor setup while keeping the
// callback's read-lock window bounded. Borrowed callbacks must be short.
const borrowedRangeChunk = 1024

// keysChunk returns up to limit keys matching g that sort after `after`.
// The read lock is held only for the duration of this call, so long scans
// never block writers for long, and the callback of Scan may itself write.
func (db *DB[T]) keysChunk(g *glob, after string, hasAfter bool, limit int, reuse []string, treeScratch []byte) (keys []string, more bool, scratch []byte, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, false, treeScratch, ErrClosed
	}
	if g.exact {
		keys = reuse[:0]
		l, ok := db.t.get(g.prefix)
		if ok && (l.expiresAt == 0 || time.Now().UnixNano() < l.expiresAt) && (!hasAfter || string(g.prefix) > after) {
			keys = append(keys, string(g.prefix))
		}
		return keys, false, treeScratch, nil
	}
	keys = reuse[:0]
	if cap(keys) < min(limit, scanChunk) {
		keys = make([]string, 0, min(limit, scanChunk))
	}
	now := time.Now().UnixNano()
	db.t.scanFromReuse(g.prefix, []byte(after), hasAfter, false, &treeScratch, func(k []byte, l loc) bool {
		if l.expiresAt != 0 && now >= l.expiresAt {
			return true
		}
		if !g.prefixOnly && !g.match(k) {
			return true
		}
		if len(keys) >= limit {
			more = true
			return false
		}
		keys = append(keys, string(k))
		return true
	})
	return keys, more, treeScratch, nil
}

type scanValueKey struct {
	key string
	loc loc
}

func (db *DB[T]) valueKeysChunk(g *glob, after string, hasAfter bool, limit int, reuse []scanValueKey, treeScratch []byte) (keys []scanValueKey, more bool, generation uint64, scratch []byte, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, false, 0, treeScratch, ErrClosed
	}
	keys = reuse[:0]
	generation = db.indexGen
	if g.exact {
		l, ok := db.t.get(g.prefix)
		if ok && (l.expiresAt == 0 || time.Now().UnixNano() < l.expiresAt) && (!hasAfter || string(g.prefix) > after) {
			keys = append(keys, scanValueKey{key: string(g.prefix), loc: l})
		}
		return keys, false, generation, treeScratch, nil
	}
	now := time.Now().UnixNano()
	db.t.scanFromReuse(g.prefix, []byte(after), hasAfter, false, &treeScratch, func(k []byte, l loc) bool {
		if l.expiresAt != 0 && now >= l.expiresAt {
			return true
		}
		if !g.prefixOnly && !g.match(k) {
			return true
		}
		if len(keys) >= limit {
			more = true
			return false
		}
		keys = append(keys, scanValueKey{key: string(k), loc: l})
		return true
	})
	return keys, more, generation, treeScratch, nil
}

// getAtGenerationWith uses a captured location when the index has not changed.
// If a writer or compactor moved the index, it resolves the current location
// before reading, matching Scan's live-view semantics.
func (db *DB[T]) getAtGenerationWith(key string, captured loc, generation uint64, decode func(raw []byte) error) (bool, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return false, ErrClosed
	}
	l := captured
	if generation != db.indexGen {
		var ok bool
		l, ok = db.t.getString(key)
		if !ok {
			return false, nil
		}
	}
	if l.expiresAt != 0 && time.Now().UnixNano() >= l.expiresAt {
		return false, nil
	}
	return true, db.readLoc(key, l, decode, true)
}

// ScanKeys calls fn for every key matching pattern, in sorted order, without
// reading any values. Return false to stop. fn may call DB methods.
func (db *DB[T]) ScanKeys(pattern string, fn func(key string) bool) error {
	g, err := compile(pattern)
	if err != nil {
		return err
	}
	after, has := "", false
	keys := make([]string, 0, scanChunk)
	scratch := db.takeTreeScratch()
	treeScratch := *scratch
	defer func() {
		*scratch = treeScratch
		db.putTreeScratch(scratch)
	}()
	for {
		var more bool
		var err error
		var nextScratch []byte
		keys, more, nextScratch, err = db.keysChunk(g, after, has, scanChunk, keys, treeScratch)
		if err != nil {
			return err
		}
		treeScratch = nextScratch
		for _, k := range keys {
			if !fn(k) {
				return nil
			}
		}
		if !more || len(keys) == 0 {
			return nil
		}
		after, has = keys[len(keys)-1], true
	}
}

// Scan calls fn with every key matching pattern and its decoded value, in
// sorted order. Return false to stop. The *T is reused between calls, so copy
// anything you keep. fn may call DB methods, including writes; the scan sees
// each value as it is when it is reached, and skips keys deleted meanwhile.
func (db *DB[T]) Scan(pattern string, fn func(key string, v *T) bool) error {
	g, err := compile(pattern)
	if err != nil {
		return err
	}
	db.ensureActiveMapped()
	var v T
	var decoder *codec.ValueDecoder[T]
	decode := func(raw []byte) error {
		if decoder == nil {
			var err error
			decoder, err = codec.NewValueDecoder[T]()
			if err != nil {
				return err
			}
		}
		return decoder.Unmarshal(raw, &v)
	}
	after, hasAfter := "", false
	keys := make([]scanValueKey, 0, scanChunk)
	scratch := db.takeTreeScratch()
	treeScratch := *scratch
	defer func() {
		*scratch = treeScratch
		db.putTreeScratch(scratch)
	}()
	for {
		var more bool
		var generation uint64
		var err error
		keys, more, generation, treeScratch, err = db.valueKeysChunk(g, after, hasAfter, scanChunk, keys, treeScratch)
		if err != nil {
			return err
		}
		for _, item := range keys {
			found, err := db.getAtGenerationWith(item.key, item.loc, generation, decode)
			if err != nil {
				return err
			}
			if found && !fn(item.key, &v) {
				return nil
			}
		}
		if !more || len(keys) == 0 {
			return nil
		}
		after, hasAfter = keys[len(keys)-1].key, true
	}
}

// ScanPrefix is Scan for all keys that start with prefix (taken literally).
func (db *DB[T]) ScanPrefix(prefix string, fn func(key string, v *T) bool) error {
	return db.Scan(Escape(prefix)+"*", fn)
}

// Range calls fn for every key in sorted order. It is Scan("*", fn).
func (db *DB[T]) Range(fn func(key string, v *T) bool) error { return db.Scan("*", fn) }

// ScanSnapshot scans a stable index view while holding the store's read lock.
// The callback receives a reused *T and must not call methods on db. Writers,
// expiry cleanup, and compaction wait until the scan returns. Use Scan or
// StreamResults when scans must yield regularly to writers.
func (db *DB[T]) ScanSnapshot(pattern string, fn func(key string, v *T) bool) error {
	visit := func(_ []byte, key string, v *T) bool { return fn(key, v) }
	var decoder *codec.ValueDecoder[T]
	decode := func(raw []byte, v *T) error {
		if decoder == nil {
			var err error
			decoder, err = codec.NewValueDecoder[T]()
			if err != nil {
				return err
			}
		}
		return decoder.Unmarshal(raw, v)
	}
	return db.scanSnapshot(pattern, visit, decode, true)
}

// ScanSnapshotNoCopy is ScanSnapshot using codec.UnmarshalNoCopy. Decoded
// fields are borrowed and valid only during fn; copy fields that must outlive
// the callback. The scan holds a read lock for its full duration.
func (db *DB[T]) ScanSnapshotNoCopy(pattern string, fn func(key string, v *T) bool) error {
	visit := func(_ []byte, key string, v *T) bool { return fn(key, v) }
	var decoder *codec.ValueDecoder[T]
	decode := func(raw []byte, v *T) error {
		if decoder == nil {
			var err error
			decoder, err = codec.NewValueDecoder[T]()
			if err != nil {
				return err
			}
		}
		return decoder.UnmarshalNoCopy(raw, v)
	}
	return db.scanSnapshot(pattern, visit, decode, true)
}

// ScanSnapshotBorrowed supplies both the key and decoded value as borrowed
// data valid only during fn. The callback must not call DB methods or retain
// either argument; it must copy what it needs. This avoids key and value copies.
func (db *DB[T]) ScanSnapshotBorrowed(pattern string, fn func(key []byte, v *T) bool) error {
	visit := func(key []byte, _ string, v *T) bool { return fn(key, v) }
	var decoder *codec.ValueDecoder[T]
	decode := func(raw []byte, v *T) error {
		if decoder == nil {
			var err error
			decoder, err = codec.NewValueDecoder[T]()
			if err != nil {
				return err
			}
		}
		return decoder.UnmarshalNoCopy(raw, v)
	}
	return db.scanSnapshot(pattern, visit, decode, false)
}

func (db *DB[T]) scanSnapshot(pattern string, fn func([]byte, string, *T) bool, decode func([]byte, *T) error, useCache bool) error {
	g, err := compile(pattern)
	if err != nil {
		return err
	}
	db.ensureActiveMapped()
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return ErrClosed
	}
	now := time.Now().UnixNano()
	cacheEnabled := useCache && db.opts.ReadCacheBytes > 0
	var v T
	var readErr error
	visitKey := func(k []byte, key string, l loc) bool {
		if l.expiresAt != 0 && now >= l.expiresAt {
			return true
		}
		if !g.prefixOnly && !g.match(k) {
			return true
		}
		keepGoing := true
		readErr = db.readLocWithMap(key, l, func(raw []byte) error {
			if err := decode(raw, &v); err != nil {
				return err
			}
			keepGoing = fn(k, key, &v)
			return nil
		}, cacheEnabled)
		return readErr == nil && keepGoing
	}
	visit := func(k []byte, l loc) bool {
		key := ""
		if useCache {
			key = string(k)
		}
		return visitKey(k, key, l)
	}
	if g.exact {
		if l, ok := db.t.get(g.prefix); ok {
			key := ""
			if useCache {
				key = string(g.prefix)
			}
			visitKey(g.prefix, key, l)
		}
	} else if useCache && g.prefixOnly && len(g.prefix) == 0 {
		if !db.t.scanAllKeys(func(key string, l loc) bool { return visitKey(nil, key, l) }) {
			db.t.scan(g.prefix, nil, false, visit)
		}
	} else {
		db.t.scan(g.prefix, nil, false, func(k []byte, l loc) bool {
			return visit(k, l)
		})
	}
	return readErr
}

// RangeSnapshot is ScanSnapshot("*", fn).
func (db *DB[T]) RangeSnapshot(fn func(key string, v *T) bool) error {
	return db.ScanSnapshot("*", fn)
}

// RangeSnapshotNoCopy is ScanSnapshotNoCopy("*", fn).
func (db *DB[T]) RangeSnapshotNoCopy(fn func(key string, v *T) bool) error {
	return db.ScanSnapshotNoCopy("*", fn)
}

// RangeSnapshotBorrowed is ScanSnapshotBorrowed("*", fn).
func (db *DB[T]) RangeSnapshotBorrowed(fn func(key []byte, v *T) bool) error {
	return db.ScanSnapshotBorrowed("*", fn)
}

// StreamBorrowed visits matching entries in sorted order with bounded memory.
// The callback receives borrowed key and value data valid only for the call;
// it must not retain either argument or call methods on db. A read lock is
// held for at most borrowedRangeChunk matching entries at a time, so writes
// may proceed between chunks. Returning false stops the stream. Cancellation
// returns ctx.Err().
func (db *DB[T]) StreamBorrowed(ctx context.Context, pattern string, fn func(key []byte, v *T) bool) error {
	g, err := compile(pattern)
	if err != nil {
		return err
	}
	db.ensureActiveMapped()
	var value T
	var decoder *codec.ValueDecoder[T]
	after, hasAfter := "", false
	scratch := db.takeTreeScratch()
	treeScratch := *scratch
	defer func() {
		*scratch = treeScratch
		db.putTreeScratch(scratch)
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		more, last, nextScratch, err := db.streamBorrowedChunk(ctx, g, after, hasAfter, &value, &decoder, fn, treeScratch)
		if err != nil {
			return err
		}
		treeScratch = nextScratch
		if !more {
			return nil
		}
		after, hasAfter = last, true
	}
}

func (db *DB[T]) streamBorrowedChunk(ctx context.Context, g *glob, after string, hasAfter bool, value *T, decoder **codec.ValueDecoder[T], fn func([]byte, *T) bool, treeScratch []byte) (more bool, last string, scratch []byte, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return false, "", treeScratch, ErrClosed
	}
	now := time.Now().UnixNano()
	var count int
	var readErr error
	var stopped bool
	visit := func(key []byte, l loc) bool {
		if err := ctx.Err(); err != nil {
			readErr = err
			return false
		}
		if l.expiresAt != 0 && now >= l.expiresAt {
			return true
		}
		if !g.prefixOnly && !g.match(key) {
			return true
		}
		if count >= borrowedRangeChunk {
			more = true
			return false
		}
		readErr = db.readLocWithMap("", l, func(raw []byte) error {
			if *decoder == nil {
				var err error
				*decoder, err = codec.NewValueDecoder[T]()
				if err != nil {
					return err
				}
			}
			if err := (*decoder).UnmarshalNoCopy(raw, value); err != nil {
				return err
			}
			stopped = !fn(key, value)
			return nil
		}, false)
		if readErr != nil || stopped {
			return false
		}
		count++
		if count == borrowedRangeChunk {
			last = string(key)
		}
		return true
	}
	if g.exact {
		if l, ok := db.t.get(g.prefix); ok && (!hasAfter || bytes.Compare(g.prefix, []byte(after)) > 0) {
			visit(g.prefix, l)
		}
		return false, last, treeScratch, readErr
	}
	db.t.scanFromReuse(g.prefix, []byte(after), hasAfter, false, &treeScratch, visit)
	if readErr != nil {
		return false, "", treeScratch, readErr
	}
	if stopped {
		return false, "", treeScratch, nil
	}
	return more, last, treeScratch, nil
}

// TakeRange visits at most limit live keys in byte-sorted order in [start, end).
// An empty end means there is no upper bound. Returning false stops early.
// It reads chunks of at most 256 keys or 8 MiB of encoded values (except for
// one larger value) under one stable read lock, then calls fn outside the lock;
// fn may call DB methods. Concurrent writes can be reflected between chunks.
// The *T is reused between calls, so copy any value data that must be retained.
func (db *DB[T]) TakeRange(start, end string, limit int, fn func(key string, v *T) bool) error {
	if limit <= 0 {
		return nil
	}
	db.ensureActiveMapped()
	items := make([]scanValueKey, 0, min(limit, scanChunk))
	scratch := db.takeTreeScratch()
	treeScratch := *scratch
	defer func() {
		*scratch = treeScratch
		db.putTreeScratch(scratch)
	}()
	after, hasAfter := "", false
	visited := 0
	var v T
	var rawValues [][]byte
	var decoder *codec.ValueDecoder[T]
	for visited < limit {
		chunkLimit := min(scanChunk, limit-visited)
		var more bool
		var err error
		items, rawValues, more, treeScratch, err = db.rangeRawChunk(start, end, after, hasAfter, chunkLimit, ownedRangeChunkBytes, items, rawValues, treeScratch)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		for i, item := range items {
			if decoder == nil {
				decoder, err = codec.NewValueDecoder[T]()
				if err != nil {
					return err
				}
			}
			if err := decoder.Unmarshal(rawValues[i], &v); err != nil {
				return err
			}
			visited++
			if !fn(item.key, &v) {
				return nil
			}
		}
		after, hasAfter = items[len(items)-1].key, true
		if !more {
			return nil
		}
	}
	return nil
}

// TakeRangeBorrowed visits a bounded key range in byte-sorted order without
// copying keys or decoded value fields. The callback runs under a read lock for
// one chunk at a time (up to 1024 keys); it must not call DB methods or retain
// either argument.
// Copy anything that must outlive fn. Concurrent writes can be reflected
// between chunks. An empty end is unbounded.
func (db *DB[T]) TakeRangeBorrowed(start, end string, limit int, fn func(key []byte, v *T) bool) error {
	if limit <= 0 {
		return nil
	}
	scratch := db.takeTreeScratch()
	treeScratch := *scratch
	defer func() {
		*scratch = treeScratch
		db.putTreeScratch(scratch)
	}()
	after, hasAfter := "", false
	var lastKey []byte
	var value T
	var decoder *codec.ValueDecoder[T]
	visited := 0
	for visited < limit {
		chunkLimit := min(borrowedRangeChunk, limit-visited)
		count, last, more, stopped, nextKey, nextScratch, err := db.takeRangeBorrowedChunk(start, end, after, hasAfter, chunkLimit, fn, &value, &decoder, lastKey, treeScratch)
		if err != nil {
			return err
		}
		visited += count
		lastKey, treeScratch = nextKey, nextScratch
		if stopped || !more || count == 0 {
			return nil
		}
		after, hasAfter = last, true
	}
	return nil
}

func (db *DB[T]) takeRangeBorrowedChunk(start, end, after string, hasAfter bool, limit int, fn func([]byte, *T) bool, value *T, decoder **codec.ValueDecoder[T], lastKey []byte, treeScratch []byte) (visited int, last string, more, stopped bool, nextKey, scratch []byte, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return 0, "", false, false, lastKey, treeScratch, ErrClosed
	}
	now := time.Now().UnixNano()
	lower, inclusive := start, true
	if hasAfter {
		lower, inclusive = after, false
	}
	endBytes := []byte(end)
	var readErr error
	db.t.scanFromReuse(nil, []byte(lower), hasAfter || start != "", inclusive, &treeScratch, func(key []byte, l loc) bool {
		if len(endBytes) > 0 && bytes.Compare(key, endBytes) >= 0 {
			return false
		}
		if l.expiresAt != 0 && now >= l.expiresAt {
			return true
		}
		if visited >= limit {
			more = true
			return false
		}
		continueScan := true
		readErr = db.readLocWithMap("", l, func(raw []byte) error {
			if *decoder == nil {
				var derr error
				*decoder, derr = codec.NewValueDecoder[T]()
				if derr != nil {
					return derr
				}
			}
			if derr := (*decoder).UnmarshalNoCopy(raw, value); derr != nil {
				return derr
			}
			nextKey = append(lastKey[:0], key...)
			lastKey = nextKey
			visited++
			continueScan = fn(key, value)
			if !continueScan {
				stopped = true
			}
			return nil
		}, false)
		if readErr != nil || !continueScan {
			return false
		}
		return true
	})
	if readErr != nil {
		return visited, "", false, stopped, nextKey, treeScratch, readErr
	}
	return visited, string(nextKey), more, stopped, nextKey, treeScratch, nil
}

// rangeRawChunk copies encoded values under one read lock, then lets TakeRange
// decode and call user code after unlocking. This preserves a stable chunk
// view while bounding the retained snapshot bytes.
func (db *DB[T]) rangeRawChunk(start, end, after string, hasAfter bool, limit int, maxValueBytes int64, reuse []scanValueKey, rawValues [][]byte, treeScratch []byte) (items []scanValueKey, raws [][]byte, more bool, scratch []byte, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, nil, false, treeScratch, ErrClosed
	}
	items = reuse[:0]
	raws = rawValues[:0]
	now := time.Now().UnixNano()
	lower := start
	inclusive := true
	if hasAfter {
		lower = after
		inclusive = false
	}
	useLower := hasAfter || start != ""
	endBytes := []byte(end)
	var readErr error
	var valueBytes int64
	visit := func(k []byte, l loc) bool {
		if len(endBytes) > 0 && bytes.Compare(k, endBytes) >= 0 {
			return false
		}
		if l.expiresAt != 0 && now >= l.expiresAt {
			return true
		}
		if len(items) >= limit {
			more = true
			return false
		}
		if len(items) > 0 && valueBytes+int64(l.vlen) > maxValueBytes {
			more = true
			return false
		}
		items = append(items, scanValueKey{key: string(k), loc: l})
		index := len(raws)
		var rawBuf []byte
		if index < cap(raws) {
			rawBuf = raws[:cap(raws)][index]
		}
		raws = append(raws, rawBuf[:0])
		valueBytes += int64(l.vlen)
		key := items[len(items)-1].key
		readErr = db.readLoc(key, l, func(raw []byte) error {
			raws[index] = append(raws[index], raw...)
			return nil
		}, true)
		return readErr == nil
	}
	db.t.scanFromReuse(nil, []byte(lower), useLower, inclusive, &treeScratch, visit)
	if readErr != nil {
		return nil, nil, false, treeScratch, readErr
	}
	return items, raws, more, treeScratch, nil
}

// DeleteRange deletes up to limit live keys in byte-sorted order in [start,
// end). An empty end means there is no upper bound. Each chunk is deleted as
// one atomic batch, and the write lock is released between chunks so other
// operations can proceed. Concurrent changes may be reflected between chunks.
func (db *DB[T]) DeleteRange(start, end string, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	scratch := db.takeTreeScratch()
	treeScratch := *scratch
	defer func() {
		*scratch = treeScratch
		db.putTreeScratch(scratch)
	}()
	after, hasAfter := "", false
	deleted := 0
	keys := make([]string, 0, scanChunk)
	entries := make([]entry, 0, scanChunk)
	for deleted < limit {
		chunkLimit := min(scanChunk, limit-deleted)
		nextKeys, nextEntries, count, last, more, nextScratch, err := db.deleteRangeChunk(start, end, after, hasAfter, chunkLimit, keys, entries, treeScratch)
		if err != nil {
			return deleted, err
		}
		keys, entries = nextKeys, nextEntries
		treeScratch = nextScratch
		if count == 0 {
			return deleted, nil
		}
		deleted += count
		after, hasAfter = last, true
		if !more {
			return deleted, nil
		}
	}
	return deleted, nil
}

func (db *DB[T]) deleteRangeChunk(start, end, after string, hasAfter bool, limit int, keys []string, entries []entry, treeScratch []byte) (keysOut []string, entriesOut []entry, deleted int, last string, more bool, scratch []byte, err error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return keys, entries, 0, "", false, treeScratch, ErrClosed
	}
	keys = keys[:0]
	entries = entries[:0]
	now := time.Now().UnixNano()
	maxBatchBytes := min(int64(1<<20), db.opts.SegmentSize)
	var batchBytes int64
	lower, inclusive := start, true
	if hasAfter {
		lower, inclusive = after, false
	}
	endBytes := []byte(end)
	db.t.scanFromReuse(nil, []byte(lower), hasAfter || start != "", inclusive, &treeScratch, func(key []byte, l loc) bool {
		if len(endBytes) > 0 && bytes.Compare(key, endBytes) >= 0 {
			return false
		}
		if l.expiresAt != 0 && now >= l.expiresAt {
			return true
		}
		if len(keys) >= limit || (len(keys) > 0 && batchBytes+recSize(len(key), 0) > maxBatchBytes) {
			more = true
			return false
		}
		keys = append(keys, string(key))
		batchBytes += recSize(len(key), 0)
		return true
	})
	if len(keys) == 0 {
		return keys, entries, 0, "", more, treeScratch, nil
	}
	buf := db.wbuf[:0]
	for i, key := range keys {
		buf = appendRec(buf, key, nil, true, i < len(keys)-1)
		entries = append(entries, entry{key: key, del: true})
	}
	if cap(buf) <= 4<<20 {
		db.wbuf = buf[:0]
	}
	if err := db.write(buf, entries); err != nil {
		return keys, entries, 0, "", false, treeScratch, err
	}
	return keys, entries, len(keys), keys[len(keys)-1], more, treeScratch, nil
}

// TakeRangeSnapshotBorrowed visits at most limit live keys in byte-sorted
// order in [start, end), decoding values without copying. The callback runs
// under a stable read lock and must not call DB methods or retain key or value
// data; copy anything that must outlive the callback. An empty end is unbounded.
func (db *DB[T]) TakeRangeSnapshotBorrowed(start, end string, limit int, fn func(key []byte, v *T) bool) error {
	if limit <= 0 {
		return nil
	}
	db.ensureActiveMapped()
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return ErrClosed
	}
	now := time.Now().UnixNano()
	endBytes := []byte(end)
	var v T
	var decoder *codec.ValueDecoder[T]
	var readErr error
	visited := 0
	db.t.scanFrom(nil, []byte(start), start != "", true, func(key []byte, l loc) bool {
		if len(endBytes) > 0 && bytes.Compare(key, endBytes) >= 0 {
			return false
		}
		if l.expiresAt != 0 && now >= l.expiresAt {
			return true
		}
		keepGoing := true
		readErr = db.readLocWithMap("", l, func(raw []byte) error {
			if decoder == nil {
				var err error
				decoder, err = codec.NewValueDecoder[T]()
				if err != nil {
					return err
				}
			}
			if err := decoder.UnmarshalNoCopy(raw, &v); err != nil {
				return err
			}
			keepGoing = fn(key, &v)
			return nil
		}, false)
		if readErr != nil {
			return false
		}
		visited++
		return keepGoing && visited < limit
	})
	return readErr
}

// Page returns up to limit matching entries after the exclusive cursor. The
// returned cursor is empty when the page is exhausted; otherwise pass it as
// after in the next call with the same pattern. New cursors are bound to that
// pattern; key-only cursors from older versions remain accepted. Each page is
// read under one stable lock, so concurrent changes may be reflected between
// pages.
func (db *DB[T]) Page(pattern, after string, limit int) ([]Item[T], string, error) {
	g, err := compile(pattern)
	if err != nil {
		return nil, after, err
	}
	if limit <= 0 {
		return nil, after, nil
	}
	last, err := decodePageCursor(after, pattern)
	if err != nil {
		return nil, after, err
	}
	db.ensureActiveMapped()
	scratch := db.takeTreeScratch()
	treeScratch := *scratch
	defer func() {
		*scratch = treeScratch
		db.putTreeScratch(scratch)
	}()
	items, nextKey, more, treeScratch, err := db.pageAfter(g, last, after != "", limit, treeScratch)
	if err != nil {
		return nil, after, err
	}
	next := ""
	if more && len(items) > 0 {
		next = encodePageCursor(pattern, nextKey)
	}
	return items, next, nil
}

// pageAfter reads one stable page. nextKey is the last returned key when more
// results remain; hasAfter distinguishes an empty key from the first page.
func (db *DB[T]) pageAfter(g *glob, last string, hasAfter bool, limit int, treeScratch []byte) (items []Item[T], nextKey string, more bool, scratch []byte, err error) {
	return db.pageAfterBudget(g, last, hasAfter, limit, 0, treeScratch)
}

// pageAfterBudget reads one stable page, optionally bounding its encoded value
// bytes. The byte limit applies to values; a single oversized value is allowed.
func (db *DB[T]) pageAfterBudget(g *glob, last string, hasAfter bool, limit int, maxValueBytes int64, treeScratch []byte) (items []Item[T], nextKey string, more bool, scratch []byte, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, "", false, treeScratch, ErrClosed
	}
	now := time.Now().UnixNano()
	candidates := make([]scanValueKey, 0, min(limit, scanChunk))
	var candidateBytes int64
	addCandidate := func(key []byte, l loc) bool {
		if maxValueBytes > 0 && len(candidates) > 0 && candidateBytes+int64(l.vlen) > maxValueBytes {
			more = true
			return false
		}
		candidates = append(candidates, scanValueKey{key: string(key), loc: l})
		candidateBytes += int64(l.vlen)
		return true
	}
	if g.exact {
		l, ok := db.t.get(g.prefix)
		if ok && (l.expiresAt == 0 || now < l.expiresAt) && (!hasAfter || string(g.prefix) > last) {
			addCandidate(g.prefix, l)
		}
	} else {
		db.t.scanFromReuse(g.prefix, []byte(last), hasAfter, false, &treeScratch, func(k []byte, l loc) bool {
			if l.expiresAt != 0 && now >= l.expiresAt {
				return true
			}
			if !g.prefixOnly && !g.match(k) {
				return true
			}
			if len(candidates) >= limit {
				more = true
				return false
			}
			return addCandidate(k, l)
		})
	}
	items = make([]Item[T], len(candidates))
	var decoder *codec.ValueDecoder[T]
	for i, candidate := range candidates {
		items[i].Key = candidate.key
		if err := db.readLoc(candidate.key, candidate.loc, func(raw []byte) error {
			if decoder == nil {
				var err error
				decoder, err = codec.NewValueDecoder[T]()
				if err != nil {
					return err
				}
			}
			return decoder.Unmarshal(raw, &items[i].Value)
		}, true); err != nil {
			return nil, "", false, treeScratch, err
		}
	}
	if more && len(candidates) > 0 {
		nextKey = candidates[len(candidates)-1].key
	}
	return items, nextKey, more, treeScratch, nil
}

// Stream sends matching entries in sorted order until the scan finishes or
// ctx is canceled. The channel buffers up to one scan chunk (256 items), and
// each produced page contains at most 8 MiB of encoded values, except for one
// value larger than that. A queued chunk can overlap the next page being
// prepared. The channel is closed on every return path. Use StreamResults when
// the caller must receive scan errors.
func (db *DB[T]) Stream(ctx context.Context, pattern string) <-chan Item[T] {
	out := make(chan Item[T], scanChunk)
	go func() {
		defer close(out)
		g, err := compile(pattern)
		if err != nil {
			return
		}
		db.ensureActiveMapped()
		after := ""
		hasAfter := false
		scratch := db.takeTreeScratch()
		treeScratch := *scratch
		defer func() {
			*scratch = treeScratch
			db.putTreeScratch(scratch)
		}()
		for {
			if ctx.Err() != nil {
				return
			}
			items, nextKey, more, scratch, err := db.pageAfterBudget(g, after, hasAfter, scanChunk, db.opts.StreamPageBytes, treeScratch)
			if err != nil {
				return
			}
			treeScratch = scratch
			for _, item := range items {
				select {
				case out <- item:
				case <-ctx.Done():
					return
				}
			}
			if !more {
				return
			}
			after, hasAfter = nextKey, true
		}
	}()
	return out
}

// StreamResults sends matching entries in sorted order and then one result
// with Err set if the scan fails. Cancellation stops the stream; the channel
// always closes. Each Item owns its decoded value. Pages contain at most 8 MiB
// of encoded values by default, configurable with Options.StreamPageBytes;
// one oversized value is allowed in its own page.
func (db *DB[T]) StreamResults(ctx context.Context, pattern string) <-chan StreamResult[T] {
	out := make(chan StreamResult[T], scanChunk)
	go func() {
		defer close(out)
		g, err := compile(pattern)
		if err != nil {
			select {
			case out <- StreamResult[T]{Err: err}:
			case <-ctx.Done():
			}
			return
		}
		db.ensureActiveMapped()
		after := ""
		hasAfter := false
		scratch := db.takeTreeScratch()
		treeScratch := *scratch
		defer func() {
			*scratch = treeScratch
			db.putTreeScratch(scratch)
		}()
		for {
			if err := ctx.Err(); err != nil {
				return
			}
			items, nextKey, more, scratch, err := db.pageAfterBudget(g, after, hasAfter, scanChunk, db.opts.StreamPageBytes, treeScratch)
			if err != nil {
				select {
				case out <- StreamResult[T]{Err: err}:
				case <-ctx.Done():
				}
				return
			}
			treeScratch = scratch
			for _, item := range items {
				select {
				case out <- StreamResult[T]{Item: item}:
				case <-ctx.Done():
					return
				}
			}
			if !more {
				return
			}
			after, hasAfter = nextKey, true
		}
	}()
	return out
}

// Match returns the keys matching pattern, sorted.
func (db *DB[T]) Match(pattern string) ([]string, error) {
	var out []string
	err := db.ScanKeys(pattern, func(k string) bool { out = append(out, k); return true })
	return out, err
}

// KeysSnapshot returns all live keys, sorted from one stable index view.
// Writers wait while the key slice is collected. Repeated calls reuse the
// full-scan key cache until an insert or delete invalidates it.
func (db *DB[T]) KeysSnapshot() []string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil
	}
	now := time.Now().UnixNano()
	keys := make([]string, 0, db.t.n)
	visit := func(key string, l loc) bool {
		if l.expiresAt == 0 || now < l.expiresAt {
			keys = append(keys, key)
		}
		return true
	}
	if !db.t.scanAllKeys(visit) {
		db.t.scan(nil, nil, false, func(key []byte, l loc) bool {
			if l.expiresAt == 0 || now < l.expiresAt {
				keys = append(keys, string(key))
			}
			return true
		})
	}
	return keys
}

// Keys returns all keys, sorted.
func (db *DB[T]) Keys() []string {
	ks, _ := db.Match("*")
	return ks
}

// Count returns how many keys match pattern, without reading any values.
func (db *DB[T]) Count(pattern string) (int, error) {
	n := 0
	err := db.ScanKeys(pattern, func(string) bool { n++; return true })
	return n, err
}

// DeleteMatch deletes every key matching pattern and returns how many it
// deleted. Deletions are committed in atomic batches of up to 1000 keys, so a
// crash can stop it part-way, but never in the middle of a batch.
func (db *DB[T]) DeleteMatch(pattern string) (int, error) {
	b := db.NewBatch()
	n := 0
	var cerr error
	flush := func() {
		if b.Len() > 0 && cerr == nil {
			k := b.Len()
			if cerr = b.Commit(); cerr == nil {
				n += k
			}
		}
	}
	err := db.ScanKeys(pattern, func(k string) bool {
		if cerr = b.Delete(k); cerr != nil {
			return false
		}
		if b.Len() >= 1000 {
			flush()
		}
		return cerr == nil
	})
	flush()
	if err == nil {
		err = cerr
	}
	return n, err
}
