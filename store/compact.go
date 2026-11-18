package store

import (
	"os"
)

// Compaction merges a short run of adjacent immutable segments into new
// segments that hold only the records still live.
//
// It does not stop the world: the inputs are immutable, so they are copied
// without holding the DB lock (the lock is taken briefly per record to ask
// "is this still the current version?"). Only the manifest swap and the
// index re-pointing take the write lock, in short bursts. Records
// overwritten or deleted while the copy runs are simply left behind in the
// output and counted as dead, to be reclaimed by a later compaction.
//
// Tombstones (delete markers) are kept unless the run includes the oldest
// segment, because an older segment outside the run might still hold a value
// the tombstone has to keep dead.

const maxRun = 4 // segments merged at once; bounds memory and time per run

var noLoc = loc{sid: ^uint32(0)} // marks a kept tombstone in the copy list

// pickRun finds the first run of adjacent immutable segments whose dead
// fraction is at least ratio (0 means any dead byte). Caller holds mu.
func (db *DB[T]) pickRun(ratio float64) []uint32 {
	var run []uint32
	for _, sid := range db.order {
		if sid == db.active.sid {
			break
		}
		s := db.segs[sid]
		if s.dead > 0 && float64(s.dead) >= ratio*float64(s.size) {
			if run = append(run, sid); len(run) == maxRun {
				break
			}
		} else if len(run) > 0 {
			break
		}
	}
	return run
}

func (db *DB[T]) compactLoop() {
	defer db.wg.Done()
	for {
		select {
		case <-db.kick:
			for {
				db.mu.RLock()
				var run []uint32
				if !db.closed {
					run = db.pickRun(db.opts.CompactRatio)
				}
				db.mu.RUnlock()
				if len(run) == 0 {
					break
				}
				if db.compactRun(run) != nil {
					break
				}
				select {
				case <-db.done:
					return
				default:
				}
			}
		case <-db.done:
			return
		}
	}
}

// Compact reclaims the space of overwritten and deleted records now. It
// merges segments a few at a time while readers and writers keep working.
func (db *DB[T]) Compact() error {
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return ErrClosed
	}
	var err error
	if db.active.size > int64(len(segMagic)) { // make the active segment compactable too
		err = db.rotate()
	}
	db.mu.Unlock()
	if err != nil {
		return err
	}
	for {
		db.mu.RLock()
		var run []uint32
		if !db.closed {
			run = db.pickRun(0)
		}
		db.mu.RUnlock()
		if len(run) == 0 {
			return nil
		}
		if err := db.compactRun(run); err != nil {
			return err
		}
	}
}

func (db *DB[T]) compactRun(run []uint32) error {
	db.cmu.Lock()
	defer db.cmu.Unlock()

	db.mu.RLock()
	if db.closed {
		db.mu.RUnlock()
		return ErrClosed
	}
	ins := make([]*segment, len(run))
	for i, sid := range run {
		if ins[i] = db.segs[sid]; ins[i] == nil || sid == db.active.sid {
			db.mu.RUnlock()
			return nil // the layout changed under us; the caller will re-pick
		}
	}
	dropTombs := db.order[0] == run[0]
	db.mu.RUnlock()

	var (
		outs []*segment
		olds []loc // for each output record, where it came from
		cur  *segment
		buf  []byte
		val  []byte
		ferr error
	)
	fail := func(err error) error {
		for _, o := range outs {
			o.f.Close()
			os.Remove(segPath(db.dir, o.sid))
		}
		return err
	}
	flush := func() error {
		if cur == nil || len(buf) == 0 {
			return nil
		}
		_, err := cur.f.WriteAt(buf, cur.size)
		cur.size += int64(len(buf))
		buf = buf[:0]
		return err
	}
	newOut := func() error {
		if err := flush(); err != nil {
			return err
		}
		db.mu.Lock()
		sid := db.nextSid
		db.nextSid++
		db.mu.Unlock()
		s, err := db.newSegment(sid)
		if err != nil {
			return err
		}
		cur = s
		outs = append(outs, s)
		return nil
	}

	for _, in := range ins {
		end := scanSegment(in.f, int64(len(segMagic)), in.size, func(r *rec) bool {
			var old loc
			if r.del {
				if dropTombs {
					return true
				}
				old = noLoc
			} else {
				old = loc{in.sid, uint32(r.voff()), r.vlen}
				db.mu.RLock()
				l, ok := db.t.get(r.key)
				db.mu.RUnlock()
				if !ok || l != old {
					return true // superseded or deleted
				}
			}
			if cur == nil || (cur.size+int64(len(buf))+r.size() > db.opts.SegmentSize && cur.size+int64(len(buf)) > int64(len(segMagic))) {
				if ferr = newOut(); ferr != nil {
					return false
				}
			}
			if r.del {
				buf = appendRec(buf, r.key, nil, true, false)
			} else {
				if cap(val) < int(r.vlen) {
					val = make([]byte, r.vlen)
				}
				val = val[:r.vlen]
				if _, ferr = in.f.ReadAt(val, r.voff()); ferr != nil {
					return false
				}
				buf = appendRec(buf, r.key, val, false, false)
			}
			olds = append(olds, old)
			if len(buf) >= 1<<20 {
				if ferr = flush(); ferr != nil {
					return false
				}
			}
			return true
		})
		if ferr == nil && end != in.size {
			ferr = ErrCorrupt
		}
		if ferr != nil {
			return fail(ferr)
		}
	}
	if err := flush(); err != nil {
		return fail(err)
	}
	for _, o := range outs {
		if err := o.f.Sync(); err != nil {
			return fail(err)
		}
	}

	// swap the manifest: outputs replace the run, atomically
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return fail(ErrClosed)
	}
	at := -1
	for i, sid := range db.order {
		if sid == run[0] {
			at = i
			break
		}
	}
	if at < 0 || at+len(run) > len(db.order) || db.order[at+len(run)-1] != run[len(run)-1] {
		db.mu.Unlock()
		return fail(nil)
	}
	order := append([]uint32(nil), db.order[:at]...)
	for _, o := range outs {
		order = append(order, o.sid)
	}
	order = append(order, db.order[at+len(run):]...)
	if err := writeManifest(db.dir, order, db.nextSid); err != nil {
		db.mu.Unlock()
		return fail(err)
	}
	db.order = order
	for _, o := range outs {
		db.segs[o.sid] = o
	}
	db.mu.Unlock()

	// re-point the index at the copies (inputs stay readable meanwhile)
	type move struct {
		key      string
		old, new loc
		size     int64
	}
	k := 0
	for _, o := range outs {
		var batch []move
		apply := func() {
			db.mu.Lock()
			for _, m := range batch {
				if l, ok := db.t.get([]byte(m.key)); ok && l == m.old {
					db.t.set([]byte(m.key), m.new)
				} else {
					o.dead += m.size // changed while copying: the copy is garbage
				}
			}
			db.mu.Unlock()
			batch = batch[:0]
		}
		scanSegment(o.f, int64(len(segMagic)), o.size, func(r *rec) bool {
			old := olds[k]
			k++
			if old != noLoc {
				batch = append(batch, move{string(r.key), old, loc{o.sid, uint32(r.voff()), r.vlen}, r.size()})
				if len(batch) >= 1024 {
					apply()
				}
			}
			return true
		})
		apply()
	}

	// drop the inputs
	db.mu.Lock()
	for _, in := range ins {
		delete(db.segs, in.sid)
		in.f.Close()
	}
	db.mu.Unlock()
	for _, in := range ins {
		os.Remove(segPath(db.dir, in.sid))
	}
	_ = db.Checkpoint() // keep Open fast; failure here is harmless
	return nil
}
