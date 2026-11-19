package store

import "container/list"

type cachedValue struct {
	key string
	loc loc
	raw []byte
}

func (db *DB[T]) cacheGet(key string, at loc) ([]byte, bool) {
	if db.opts.ReadCacheBytes <= 0 {
		return nil, false
	}
	db.cacheMu.RLock()
	e := db.cache[key]
	if e == nil {
		db.cacheMu.RUnlock()
		return nil, false
	}
	c := e.Value.(*cachedValue)
	if c.loc != at {
		db.cacheMu.RUnlock()
		db.cacheDeleteEntry(key, e)
		return nil, false
	}
	raw := c.raw
	db.cacheMu.RUnlock()
	return raw, true
}

func (db *DB[T]) cachePut(key string, at loc, raw []byte) {
	if db.opts.ReadCacheBytes <= 0 {
		return
	}
	size := int64(len(key) + len(raw))
	if size > db.opts.ReadCacheBytes {
		return
	}
	db.cacheMu.Lock()
	defer db.cacheMu.Unlock()
	if db.cache == nil {
		db.cache = make(map[string]*list.Element)
	}
	if e := db.cache[key]; e != nil {
		db.cacheRemove(e)
	}
	c := &cachedValue{key: key, loc: at, raw: append([]byte(nil), raw...)}
	e := db.cacheFIFO.PushBack(c)
	db.cache[key] = e
	db.cacheBytes += size
	for db.cacheBytes > db.opts.ReadCacheBytes {
		db.cacheRemove(db.cacheFIFO.Front())
	}
}

func (db *DB[T]) cacheDelete(key string) {
	if db.opts.ReadCacheBytes <= 0 {
		return
	}
	db.cacheMu.Lock()
	if e := db.cache[key]; e != nil {
		db.cacheRemove(e)
	}
	db.cacheMu.Unlock()
}

// cacheDeleteEntry removes a stale entry only if it is still the entry that
// the caller observed. Another reader may have installed a fresh value after
// the caller released its read lock.
func (db *DB[T]) cacheDeleteEntry(key string, stale *list.Element) {
	if db.opts.ReadCacheBytes <= 0 {
		return
	}
	db.cacheMu.Lock()
	if db.cache[key] == stale {
		db.cacheRemove(stale)
	}
	db.cacheMu.Unlock()
}

func (db *DB[T]) cacheRemove(e *list.Element) {
	c := e.Value.(*cachedValue)
	delete(db.cache, c.key)
	db.cacheBytes -= int64(len(c.key) + len(c.raw))
	db.cacheFIFO.Remove(e)
}

func (db *DB[T]) clearReadCache() {
	db.cacheMu.Lock()
	db.cache = nil
	db.cacheFIFO.Init()
	db.cacheBytes = 0
	db.cacheMu.Unlock()
}
