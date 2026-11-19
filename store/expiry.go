package store

import (
	"container/heap"
	"time"
)

type expiryItem struct {
	key string
	at  int64
	idx int
}

type expiryQueue []*expiryItem

func (q expiryQueue) Len() int { return len(q) }
func (q expiryQueue) Less(i, j int) bool {
	if q[i].at == q[j].at {
		return q[i].key < q[j].key
	}
	return q[i].at < q[j].at
}
func (q expiryQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i]; q[i].idx = i; q[j].idx = j }
func (q *expiryQueue) Push(x any)   { item := x.(*expiryItem); item.idx = len(*q); *q = append(*q, item) }
func (q *expiryQueue) Pop() any {
	old := *q
	n := len(old)
	x := old[n-1]
	old[n-1] = nil
	x.idx = -1
	*q = old[:n-1]
	return x
}

// rebuildExpiryQueue reconstructs the scheduling queue from the current
// index after loading either a checkpoint or replaying the log.
func (db *DB[T]) rebuildExpiryQueue() {
	db.expires = nil
	db.expiryByKey = make(map[string]*expiryItem)
	db.t.scan(nil, nil, false, func(k []byte, l loc) bool {
		if l.expiresAt != 0 {
			item := &expiryItem{key: string(k), at: l.expiresAt, idx: len(db.expires)}
			db.expires = append(db.expires, item)
			db.expiryByKey[item.key] = item
		}
		return true
	})
	heap.Init(&db.expires)
}

// scheduleExpiry maintains one heap item per currently expiring key.
// Caller holds db.mu.
func (db *DB[T]) scheduleExpiry(key string, l loc, live bool) {
	if old := db.expiryByKey[key]; old != nil {
		if l.expiresAt != 0 && live {
			old.at = l.expiresAt
			heap.Fix(&db.expires, old.idx)
			return
		}
		heap.Remove(&db.expires, old.idx)
		delete(db.expiryByKey, key)
	}
	if !live || l.expiresAt == 0 {
		return
	}
	if db.expiryByKey == nil {
		db.expiryByKey = make(map[string]*expiryItem)
	}
	item := &expiryItem{key: key, at: l.expiresAt}
	db.expiryByKey[key] = item
	heap.Push(&db.expires, item)
}

func (db *DB[T]) expiryLoop() {
	defer db.wg.Done()
	t := time.NewTicker(db.opts.ExpiryInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			db.expireDue()
		case <-db.done:
			return
		}
	}
}

// expireDue appends tombstones in bounded atomic batches, and only for queue
// entries that still describe the current version. Replacement writes
// therefore make older queue entries inert.
func (db *DB[T]) expireDue() {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return
	}
	now := time.Now().UnixNano()
	if db.expires.Len() == 0 || db.expires[0].at > now {
		return
	}
	const batchLimit = 256
	maxBatchBytes := min(int64(1<<20), db.opts.SegmentSize)
	items := make([]*expiryItem, 0, batchLimit)
	entries := make([]entry, 0, batchLimit)
	for db.expires.Len() > 0 && db.expires[0].at <= now {
		items = items[:0]
		entries = entries[:0]
		var batchBytes int64
		for len(items) < batchLimit && db.expires.Len() > 0 && db.expires[0].at <= now {
			if len(items) > 0 && batchBytes+recSize(len(db.expires[0].key), 0) > maxBatchBytes {
				break
			}
			item := heap.Pop(&db.expires).(*expiryItem)
			delete(db.expiryByKey, item.key)
			l, ok := db.t.get([]byte(item.key))
			if !ok || l.expiresAt != item.at {
				continue // deleted or replaced
			}
			items = append(items, item)
			entries = append(entries, entry{key: item.key, del: true})
			batchBytes += recSize(len(item.key), 0)
		}
		if len(items) == 0 {
			continue
		}
		buf := db.wbuf[:0]
		for i, item := range items {
			buf = appendRec(buf, item.key, nil, true, i < len(items)-1)
		}
		if cap(buf) <= 4<<20 {
			db.wbuf = buf[:0]
		}
		if err := db.write(buf, entries); err != nil {
			for _, item := range items {
				db.expiryByKey[item.key] = item
				heap.Push(&db.expires, item)
			}
			return
		}
		// A large expiry backlog should not keep foreground operations behind
		// one long write-lock hold. Each committed batch is complete before we
		// let other operations run; the next loop rechecks the live queue.
		db.mu.Unlock()
		db.mu.Lock()
		if db.closed {
			return
		}
		select {
		case <-db.done:
			return
		default:
		}
	}
}
