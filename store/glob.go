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
	prefix     []byte
	tokens     []globToken
	exact      bool // no wildcards at all
	prefixOnly bool // the pattern is exactly the literal prefix followed by '*'
}

const (
	globLiteral byte = iota
	globStar
	globAny
	globClass
)

type globToken struct {
	kind  byte
	lit   byte
	class *class
}

func compile(pattern string) (*glob, error) {
	g := &glob{}
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
	g.prefixOnly = !g.exact && pattern[i:] == "*"
	for j := 0; j < len(pattern); {
		switch pattern[j] {
		case '\\':
			if j+1 >= len(pattern) {
				return nil, ErrBadPattern
			}
			g.tokens = append(g.tokens, globToken{kind: globLiteral, lit: pattern[j+1]})
			j += 2
		case '*':
			g.tokens = append(g.tokens, globToken{kind: globStar})
			j++
		case '?':
			g.tokens = append(g.tokens, globToken{kind: globAny})
			j++
		case '[':
			c, next, err := parseClass(pattern, j)
			if err != nil {
				return nil, err
			}
			g.tokens = append(g.tokens, globToken{kind: globClass, class: c})
			j = next
		default:
			g.tokens = append(g.tokens, globToken{kind: globLiteral, lit: pattern[j]})
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
	tokens := g.tokens
	p, i := 0, 0
	starP, starI := -1, 0
	for i < len(key) {
		if p < len(tokens) {
			tok := tokens[p]
			switch tok.kind {
			case globStar:
				starP, starI = p, i
				p++
				continue
			case globAny:
				_, w := utf8.DecodeRune(key[i:])
				i += w
				p++
				continue
			case globClass:
				r, w := utf8.DecodeRune(key[i:])
				if tok.class.matches(r) {
					i += w
					p++
					continue
				}
			default:
				if tok.lit == key[i] {
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
	for p < len(tokens) && tokens[p].kind == globStar {
		p++
	}
	return p == len(tokens)
}
