package store

import "bytes"

// loc locates a value: segment id, byte offset of the value inside that
// segment, and its length. 12 bytes per key.
type loc struct {
	sid  uint32
	off  uint32
	vlen uint32
}

// tree is an ordered, compressed radix tree from key bytes to loc. Keys that
// share a prefix share storage, iteration is in byte order, and every key
// under a prefix is found by descending once and walking the subtree, which is
// what makes prefix and wildcard scans cheap. Not safe for concurrent use.
type tree struct {
	root node
	n    int
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

// set stores l under key and returns the previous location, if any.
func (t *tree) set(key []byte, l loc) (old loc, had bool) {
	n := &t.root
	for {
		if len(key) == 0 {
			old, had = n.l, n.has
			n.l, n.has = l, true
			if !had {
				t.n++
			}
			return
		}
		i, ok := n.child(key[0])
		if !ok {
			n.insertKid(i, &node{label: append([]byte(nil), key...), l: l, has: true})
			t.n++
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
	buf := make([]byte, 0, 64)
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
		if n.has && bytes.HasPrefix(buf, prefix) && (!hasAfter || bytes.Compare(buf, after) > 0) {
			if !visit(buf, n.l) {
				return false
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
}
