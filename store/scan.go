package store

import (
	"bytes"
	"strings"
)

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

// keysChunk returns up to limit keys matching g that sort after `after`.
// The read lock is held only for the duration of this call, so long scans
// never block writers for long, and the callback of Scan may itself write.
func (db *DB[T]) keysChunk(g *glob, after string, hasAfter bool, limit int) (keys []string, more bool, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, false, ErrClosed
	}
	if g.exact {
		if _, ok := db.t.get(g.prefix); ok && (!hasAfter || string(g.prefix) > after) {
			keys = append(keys, string(g.prefix))
		}
		return keys, false, nil
	}
	db.t.scan(g.prefix, []byte(after), hasAfter, func(k []byte, _ loc) bool {
		if !bytes.HasPrefix(k, g.prefix) || !g.match(k) {
			return true
		}
		if len(keys) >= limit {
			more = true
			return false
		}
		keys = append(keys, string(k))
		return true
	})
	return keys, more, nil
}

// ScanKeys calls fn for every key matching pattern, in sorted order, without
// reading any values. Return false to stop. fn may call DB methods.
func (db *DB[T]) ScanKeys(pattern string, fn func(key string) bool) error {
	g, err := compile(pattern)
	if err != nil {
		return err
	}
	after, has := "", false
	for {
		keys, more, err := db.keysChunk(g, after, has, scanChunk)
		if err != nil {
			return err
		}
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
	var v T
	var ferr error
	err := db.ScanKeys(pattern, func(k string) bool {
		if err := db.Get(k, &v); err != nil {
			if err != ErrNotFound {
				ferr = err
				return false
			}
			return true // deleted since it was listed
		}
		return fn(k, &v)
	})
	if err != nil {
		return err
	}
	return ferr
}

// ScanPrefix is Scan for all keys that start with prefix (taken literally).
func (db *DB[T]) ScanPrefix(prefix string, fn func(key string, v *T) bool) error {
	return db.Scan(Escape(prefix)+"*", fn)
}

// Range calls fn for every key in sorted order. It is Scan("*", fn).
func (db *DB[T]) Range(fn func(key string, v *T) bool) error { return db.Scan("*", fn) }

// Match returns the keys matching pattern, sorted.
func (db *DB[T]) Match(pattern string) ([]string, error) {
	var out []string
	err := db.ScanKeys(pattern, func(k string) bool { out = append(out, k); return true })
	return out, err
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
		b.Delete(k)
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
