package store

import (
	"errors"
	"unicode/utf8"
)

// ErrBadPattern is returned for a malformed wildcard pattern.
var ErrBadPattern = errors.New("store: malformed pattern")

// Pattern syntax (matched against the whole key, byte-order, UTF-8 aware):
//
//   - any run of characters, including none (and including '/')
//     ?        exactly one character
//     [abc]    one character from the set       [a-z]  a range
//     [^abc]   one character not in the set     ([!abc] works too)
//     \x       the character x literally (to match a literal * ? [ or \)
//
// Everything before the first wildcard is a literal prefix, and scans only
// visit keys under that prefix, so "user:42:*" touches only user 42's keys,
// while "*:email" has no prefix and must examine every key.
type glob struct {
	pat    string
	prefix []byte
	exact  bool // no wildcards at all
}

func compile(pattern string) (*glob, error) {
	g := &glob{pat: pattern}
	i := 0
	for i < len(pattern) { // literal prefix
		c := pattern[i]
		if c == '*' || c == '?' || c == '[' {
			break
		}
		if c == '\\' {
			i++
			if i >= len(pattern) {
				return nil, ErrBadPattern
			}
			c = pattern[i]
		}
		g.prefix = append(g.prefix, c)
		i++
	}
	g.exact = i == len(pattern)
	for j := i; j < len(pattern); { // validate the rest
		switch pattern[j] {
		case '\\':
			if j+1 >= len(pattern) {
				return nil, ErrBadPattern
			}
			j += 2
		case '[':
			_, next, err := parseClass(pattern, j)
			if err != nil {
				return nil, err
			}
			j = next
		default:
			j++
		}
	}
	return g, nil
}

type classRange struct{ lo, hi rune }

type class struct {
	neg bool
	rs  []classRange
}

func (c *class) matches(r rune) bool {
	in := false
	for _, x := range c.rs {
		if r >= x.lo && r <= x.hi {
			in = true
			break
		}
	}
	return in != c.neg
}

// parseClass parses the class starting at pat[i] == '[' and returns it with the
// index just past the closing ']'.
func parseClass(pat string, i int) (*class, int, error) {
	c := &class{}
	i++
	if i < len(pat) && (pat[i] == '^' || pat[i] == '!') {
		c.neg = true
		i++
	}
	read := func() (rune, bool) {
		if i >= len(pat) {
			return 0, false
		}
		if pat[i] == '\\' {
			i++
			if i >= len(pat) {
				return 0, false
			}
		}
		r, w := utf8.DecodeRuneInString(pat[i:])
		i += w
		return r, true
	}
	n := 0
	for {
		if i >= len(pat) {
			return nil, 0, ErrBadPattern
		}
		if pat[i] == ']' && n > 0 {
			return c, i + 1, nil
		}
		lo, ok := read()
		if !ok {
			return nil, 0, ErrBadPattern
		}
		hi := lo
		if i+1 < len(pat) && pat[i] == '-' && pat[i+1] != ']' {
			i++
			if hi, ok = read(); !ok || hi < lo {
				return nil, 0, ErrBadPattern
			}
		}
		c.rs = append(c.rs, classRange{lo, hi})
		n++
	}
}

// match reports whether key matches the pattern. It is linear in
// len(key)*len(pattern) in the worst case and never exponential.
func (g *glob) match(key []byte) bool {
	pat := g.pat
	p, i := 0, 0
	starP, starI := -1, 0
	for i < len(key) {
		if p < len(pat) {
			switch pat[p] {
			case '*':
				starP, starI = p, i
				p++
				continue
			case '?':
				_, w := utf8.DecodeRune(key[i:])
				i += w
				p++
				continue
			case '[':
				c, next, _ := parseClass(pat, p)
				r, w := utf8.DecodeRune(key[i:])
				if c.matches(r) {
					i += w
					p = next
					continue
				}
			case '\\':
				if p+1 < len(pat) && pat[p+1] == key[i] {
					i++
					p += 2
					continue
				}
			default:
				if pat[p] == key[i] {
					i++
					p++
					continue
				}
			}
		}
		if starP < 0 {
			return false
		}
		_, w := utf8.DecodeRune(key[starI:])
		starI += w
		i, p = starI, starP+1
	}
	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat)
}
