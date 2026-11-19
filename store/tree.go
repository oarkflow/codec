package store

import (
	"bytes"
	"sync"
)

func (db *DB[T]) takeTreeScratch() *[]byte {
	if v := db.treeBufPool.Get(); v != nil {
		return v.(*[]byte)
	}
	return new([]byte)
}

func (db *DB[T]) putTreeScratch(scratch *[]byte) {
	if cap(*scratch) > maxKey {
		*scratch = nil
	}
	db.treeBufPool.Put(scratch)
}

// loc locates a value and carries its optional absolute expiry time.
type loc struct {
	seg       *segment
	off       uint32
	vlen      uint32
	expiresAt int64
}

// tree is an ordered, compressed radix tree from key bytes to loc. Keys that
// share a prefix share storage, iteration is in byte order, and every key
// under a prefix is found by descending once and walking the subtree, which is
// what makes prefix and wildcard scans cheap. Not safe for concurrent use.
type tree struct {
	root        node
	n           int
	keysMu      sync.Mutex
	keysReady   bool
	keyScanSeen bool
	keyStrings  []string
}

type node struct {
	label []byte
	kids  []*node // sorted by label[0]
	l     loc
	has   bool
}

func (n *node) child(b byte) (int, bool) {
	lo, hi := 0, len(n.kids)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if n.kids[m].label[0] < b {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo, lo < len(n.kids) && n.kids[lo].label[0] == b
}

func (n *node) insertKid(i int, k *node) {
	n.kids = append(n.kids, nil)
	copy(n.kids[i+1:], n.kids[i:])
	n.kids[i] = k
}

func commonPrefix(a, b []byte) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

func (t *tree) get(key []byte) (loc, bool) {
	n := &t.root
	for len(key) > 0 {
		i, ok := n.child(key[0])
		if !ok {
			return loc{}, false
		}
		k := n.kids[i]
		if !bytes.HasPrefix(key, k.label) {
			return loc{}, false
		}
		key = key[len(k.label):]
		n = k
	}
	return n.l, n.has
}

// getString is the string-key equivalent of get without allocating a []byte
// copy of the caller's key. It is used on the DB's common Get/Has paths.
func (t *tree) getString(key string) (loc, bool) {
	n := &t.root
	for len(key) > 0 {
		i, ok := n.child(key[0])
		if !ok {
			return loc{}, false
		}
		k := n.kids[i]
		if len(key) < len(k.label) {
			return loc{}, false
		}
		for j, b := range k.label {
			if key[j] != b {
				return loc{}, false
			}
		}
		key = key[len(k.label):]
		n = k
	}
	return n.l, n.has
}

// set stores l under key and returns the previous location, if any.
func (t *tree) set(key []byte, l loc) (old loc, had bool) {
	n := &t.root
	for {
		if len(key) == 0 {
			old, had = n.l, n.has
			n.l, n.has = l, true
			if !had {
				t.n++
				t.keysReady = false
				t.keyStrings = nil
			}
			return
		}
		i, ok := n.child(key[0])
		if !ok {
			child := &node{label: append([]byte(nil), key...), l: l, has: true}
			n.insertKid(i, child)
			t.n++
			t.keysReady = false
			t.keyStrings = nil
			return loc{}, false
		}
		k := n.kids[i]
		cp := commonPrefix(key, k.label)
		if cp < len(k.label) { // split k at cp
			mid := &node{label: k.label[:cp:cp]}
			k.label = k.label[cp:]
			mid.kids = []*node{k}
			n.kids[i] = mid
			k = mid
		}
		key = key[cp:]
		n = k
	}
}

// buildSortedTree constructs a checkpoint tree from strictly sorted entries.
// Building shared-prefix groups in bulk avoids repeated radix-tree searches
// and splits when opening a store from its ordered INDEX checkpoint.
func buildSortedTree(entries []snapshotEntry) *tree {
	t := &tree{n: len(entries)}
	totalKeyBytes := 0
	for _, entry := range entries {
		totalKeyBytes += len(entry.key)
	}
	blockSize := min(512, max(4, len(entries)))
	builder := sortedTreeBuilder{
		blockSize: blockSize,
		labels:    make([]byte, 0, totalKeyBytes),
	}
	populateSortedNode(&t.root, entries, 0, &builder)
	return t
}

type sortedTreeBuilder struct {
	blocks    [][]node
	current   []node
	blockSize int
	labels    []byte
}

func (b *sortedTreeBuilder) newNode(label []byte) *node {
	if len(b.current) == cap(b.current) {
		b.current = make([]node, 0, b.blockSize)
		b.blocks = append(b.blocks, b.current)
	}
	b.current = append(b.current, node{})
	n := &b.current[len(b.current)-1]
	start := len(b.labels)
	b.labels = append(b.labels, label...)
	n.label = b.labels[start:len(b.labels):len(b.labels)]
	return n
}

func populateSortedNode(n *node, entries []snapshotEntry, depth int, builder *sortedTreeBuilder) {
	start := 0
	if len(entries) > 0 && len(entries[0].key) == depth {
		n.has, n.l = true, entries[0].l
		start = 1
	}
	groups := 0
	for i := start; i < len(entries); {
		groups++
		first := entries[i].key
		j := i + 1
		for j < len(entries) && len(entries[j].key) > depth && entries[j].key[depth] == first[depth] {
			j++
		}
		i = j
	}
	n.kids = make([]*node, 0, groups)
	for i := start; i < len(entries); {
		first := entries[i].key
		j := i + 1
		for j < len(entries) && len(entries[j].key) > depth && entries[j].key[depth] == first[depth] {
			j++
		}
		last := entries[j-1].key
		labelEnd := depth + commonPrefix(first[depth:], last[depth:])
		child := builder.newNode(first[depth:labelEnd])
		populateSortedNode(child, entries[i:j], labelEnd, builder)
		n.kids = append(n.kids, child)
		i = j
	}
}

// del removes key and returns its previous location.
func (t *tree) del(key []byte) (old loc, had bool) {
	type step struct {
		p *node
		i int
	}
	var stack [32]step
	path := stack[:0]
	n := &t.root
	for len(key) > 0 {
		i, ok := n.child(key[0])
		if !ok || !bytes.HasPrefix(key, n.kids[i].label) {
			return loc{}, false
		}
		path = append(path, step{n, i})
		key = key[len(n.kids[i].label):]
		n = n.kids[i]
	}
	if !n.has {
		return loc{}, false
	}
	old, had = n.l, true
	n.l, n.has = loc{}, false
	t.keysReady = false
	t.keyStrings = nil
	t.n--
	// prune empty leaves and re-compress single-child chains
	for len(path) > 0 {
		s := path[len(path)-1]
		path = path[:len(path)-1]
		c := s.p.kids[s.i]
		switch {
		case c.has:
			return
		case len(c.kids) == 0:
			s.p.kids = append(s.p.kids[:s.i], s.p.kids[s.i+1:]...)
			if len(s.p.kids) == 0 {
				s.p.kids = nil
			}
			continue // parent may now need compression
		case len(c.kids) == 1:
			g := c.kids[0]
			lbl := make([]byte, 0, len(c.label)+len(g.label))
			g.label = append(append(lbl, c.label...), g.label...)
			s.p.kids[s.i] = g
		}
		return
	}
	return
}

// scan visits, in key order, every key that starts with prefix and is
// strictly greater than after (when hasAfter). visit returns false to stop.
// The key slice passed to visit is only valid during the call.
func (t *tree) scan(prefix, after []byte, hasAfter bool, visit func(key []byte, l loc) bool) {
	t.scanFrom(prefix, after, hasAfter, false, visit)
}

// scanAllKeys visits terminal keys as stable strings. It avoids reconstructing
// every key into a reusable path buffer when callers need owned string keys.
func (t *tree) scanAllKeys(visit func(key string, l loc) bool) bool {
	t.keysMu.Lock()
	if t.keysReady {
		keys := t.keyStrings
		t.keysMu.Unlock()
		keyIndex := 0
		var rec func(n *node) bool
		rec = func(n *node) bool {
			if n.has {
				key := keys[keyIndex]
				keyIndex++
				if !visit(key, n.l) {
					return false
				}
			}
			for _, child := range n.kids {
				if !rec(child) {
					return false
				}
			}
			return true
		}
		rec(&t.root)
		return true
	}
	if !t.keyScanSeen {
		t.keyScanSeen = true
		t.keysMu.Unlock()
		return false
	}
	t.keysMu.Unlock()

	keys := make([]string, 0, t.n)
	var path []byte
	var rec func(n *node) bool
	rec = func(n *node) bool {
		if n.has {
			key := string(path)
			keys = append(keys, key)
			if !visit(key, n.l) {
				return false
			}
		}
		for _, child := range n.kids {
			start := len(path)
			path = append(path, child.label...)
			if !rec(child) {
				return false
			}
			path = path[:start]
		}
		return true
	}
	complete := rec(&t.root)
	if complete {
		t.keysMu.Lock()
		if !t.keysReady {
			t.keyStrings = keys
			t.keysReady = true
		}
		t.keysMu.Unlock()
	}
	return true
}

// scanFrom is scan with an optional inclusive lower bound.
func (t *tree) scanFrom(prefix, after []byte, hasAfter, inclusive bool, visit func(key []byte, l loc) bool) {
	var buf []byte
	t.scanFromReuse(prefix, after, hasAfter, inclusive, &buf, visit)
}

// scanFromReuse traverses the tree like scanFrom and retains its path buffer
// for the caller to reuse across adjacent chunks. Callback keys remain valid
// only during the callback.
func (t *tree) scanFromReuse(prefix, after []byte, hasAfter, inclusive bool, scratch *[]byte, visit func(key []byte, l loc) bool) {
	buf := *scratch
	if cap(buf) < 64 {
		buf = make([]byte, 0, 64)
	} else {
		buf = buf[:0]
	}
	if len(prefix) == 0 && !hasAfter {
		var rec func(n *node) bool
		rec = func(n *node) bool {
			if n.has && !visit(buf, n.l) {
				return false
			}
			for _, k := range n.kids {
				start := len(buf)
				buf = append(buf, k.label...)
				if !rec(k) {
					return false
				}
				buf = buf[:start]
			}
			return true
		}
		rec(&t.root)
		*scratch = buf[:0]
		return
	}
	viable := func(p []byte) bool {
		if len(p) >= len(prefix) {
			if !bytes.HasPrefix(p, prefix) {
				return false
			}
		} else if !bytes.HasPrefix(prefix, p) {
			return false
		}
		if hasAfter {
			m := min(len(p), len(after))
			if bytes.Compare(p[:m], after[:m]) < 0 {
				return false
			}
		}
		return true
	}
	var rec func(n *node) bool
	rec = func(n *node) bool {
		if n.has && bytes.HasPrefix(buf, prefix) {
			if !hasAfter {
				if !visit(buf, n.l) {
					return false
				}
			} else {
				cmp := bytes.Compare(buf, after)
				if (cmp > 0 || (inclusive && cmp == 0)) && !visit(buf, n.l) {
					return false
				}
			}
		}
		for _, k := range n.kids {
			start := len(buf)
			buf = append(buf, k.label...)
			if viable(buf) && !rec(k) {
				return false
			}
			buf = buf[:start]
		}
		return true
	}
	rec(&t.root)
	*scratch = buf[:0]
}
