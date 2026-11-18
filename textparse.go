package codec

import (
	"bytes"
	"encoding"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
	"unsafe"
)

// UnmarshalText parses the text notation (see textfmt.go) into v, which must
// be a non-nil pointer. Fields missing from the text keep the zero value;
// existing slices and maps are replaced, not merged. Malformed input returns
// a *TextError with the line and column, and never panics.
func UnmarshalText(text []byte, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return ErrNeedPointer
	}
	tt, err := textPlan(rv.Type().Elem())
	if err != nil {
		return err
	}
	d := tdecPool.Get().(*tdec)
	d.s, d.lines, d.chunk, d.cValid, d.gen = text, d.lines[:0], nil, false, 0
	err = d.scan()
	if err == nil {
		err = d.top(rv.Elem(), tt)
	}
	d.s, d.chunk, d.cV, d.cTT, d.curKey, d.cPrefix = nil, nil, reflect.Value{}, nil, nil, nil
	if cap(d.lines) <= 1<<16 {
		tdecPool.Put(d)
	}
	return err
}

// TextKey returns the first name in the path of a text line, unquoted: the key
// of a top-level map entry. It is for splitting a document of many records.
func TextKey(line []byte) (string, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return "", fmt.Errorf("codec text: empty line")
	}
	key, _, _, _, bad := splitEntry(line)
	if bad >= 0 {
		return "", fmt.Errorf("codec text: bad key")
	}
	seg, quoted, _, ok := nextSeg(key)
	if !ok {
		return "", fmt.Errorf("codec text: bad key")
	}
	if quoted {
		return unquote(seg)
	}
	return string(seg), nil
}

var tdecPool = sync.Pool{New: func() any { return &tdec{lines: make([]tline, 0, 64)} }}

// tline is one logical line: the trimmed content span, the physical line
// number, and the column where it starts.
type tline struct {
	start, end int32
	line, col  int32
}

type tdec struct {
	s     []byte
	lines []tline
	chunk []byte // pointer-free block that decoded strings are carved from

	// Consecutive lines usually write into the same struct (Lines.SKU,
	// Lines.Qty, ...), so the last struct a line resolved to is remembered with
	// its path prefix. It is trusted only while gen is unchanged (no list has
	// been appended to or moved since) and only for a line whose prefix matches;
	// every other line drops it first.
	curKey   []byte
	volatile bool // this line went through a map or interface{}: not cacheable
	gen      uint32
	cValid   bool
	cPrefix  []byte
	cV       reflect.Value
	cTT      *ttype
	cGen     uint32
}

// str copies b into the decode's string block and returns it as a string: one
// allocation serves many strings (a retained string keeps its block alive).
func (d *tdec) str(b []byte) string {
	n := len(b)
	if n == 0 {
		return ""
	}
	if cap(d.chunk)-len(d.chunk) < n {
		size := min(max(len(d.s)/4, 512), 1<<16)
		d.chunk = make([]byte, 0, max(size, n))
	}
	at := len(d.chunk)
	d.chunk = append(d.chunk, b...)
	return unsafe.String(&d.chunk[at], n)
}

func (d *tdec) errAt(ln tline, format string, args ...any) error {
	return &TextError{Line: int(ln.line), Col: int(ln.col), Msg: fmt.Sprintf(format, args...)}
}

func (d *tdec) content(ln tline) []byte { return d.s[ln.start:ln.end] }

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' }

// plainName reports whether a path name starting with c can be a struct field
// (it is not the list markers + and ^, and not an item number).
func plainName(c byte) bool { return c != '+' && c != '^' && (c < '0' || c > '9') }

// special marks the bytes scan has to look at: newline, ';', '"', '\\'.
var special = func() (t [256]bool) {
	t['\n'], t[';'], t['"'], t['\\'] = true, true, true, true
	return
}()

// scan splits the input into logical lines in a single pass. Physical lines
// end at \n; a ';' outside double quotes also ends a line; blank lines and
// lines whose first character is '#' are dropped.
func (d *tdec) scan() error {
	s := d.s
	n := len(s)
	if int64(n) > math.MaxInt32 {
		return &TextError{Line: 1, Col: 1, Msg: "text too large"}
	}
	lineNo := int32(1)
	ls := 0 // start of the physical line
	for ls <= n {
		p := ls
		for p < n && (s[p] == ' ' || s[p] == '\t' || s[p] == '\r') {
			p++
		}
		if p < n && s[p] == '#' { // comment line
			for p < n && s[p] != '\n' {
				p++
			}
		} else {
			seg := p
			inQuote := false
			for ; p < n; p++ {
				c := s[p]
				if !special[c] {
					continue
				}
				if c == '\n' {
					break
				}
				if inQuote {
					if c == '\\' && p+1 < n && s[p+1] != '\n' {
						p++
					} else if c == '"' {
						inQuote = false
					}
					continue
				}
				if c == ';' {
					d.emit(seg, p, ls, lineNo)
					seg = p + 1
				} else if c == '"' && (p == seg || isSpace(s[p-1]) || s[p-1] == '.') {
					inQuote = true
				}
			}
			d.emit(seg, p, ls, lineNo)
		}
		ls = p + 1
		lineNo++
	}
	return nil
}

func (d *tdec) emit(a, b, lineStart int, lineNo int32) {
	for a < b && isSpace(d.s[a]) {
		a++
	}
	for b > a && isSpace(d.s[b-1]) {
		b--
	}
	if a < b {
		d.lines = append(d.lines, tline{int32(a), int32(b), lineNo, int32(a-lineStart) + 1})
	}
}

// splitEntry splits `path value`. Quotes inside the path may contain spaces.
// lastDot is the index of the last '.' in key outside quotes, or -1; quoted
// reports whether the path contains any quote.
func splitEntry(c []byte) (key, rest []byte, lastDot int, quoted bool, bad int) {
	i := 0
	lastDot = -1
	for i < len(c) && !isSpace(c[i]) {
		switch c[i] {
		case '.':
			lastDot = i
		case '"':
			if i > 0 && c[i-1] != '.' { // a quote only opens a name at the start of one
				break
			}
			quoted = true
			j := i + 1
			for j < len(c) && c[j] != '"' {
				if c[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(c) {
				return nil, nil, -1, true, len(c)
			}
			i = j
		}
		i++
	}
	key, rest = c[:i], c[i:]
	for len(rest) > 0 && isSpace(rest[0]) {
		rest = rest[1:]
	}
	return key, rest, lastDot, quoted, -1
}

// nextSeg takes the first name off a path. rem is what follows its dot (empty
// when the name was the last one).
func nextSeg(key []byte) (seg []byte, quoted bool, rem []byte, ok bool) {
	if len(key) > 0 && key[0] == '"' {
		j := 1
		for j < len(key) && key[j] != '"' {
			if key[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(key) {
			return nil, true, nil, false
		}
		seg, rem, quoted = key[:j+1], key[j+1:], true
		if len(rem) == 0 {
			return seg, true, nil, true
		}
		if rem[0] != '.' || len(rem) == 1 {
			return nil, true, nil, false
		}
		return seg, true, rem[1:], true
	}
	i := 0
	for i < len(key) && key[i] != '.' {
		i++
	}
	if i == len(key) {
		return key, false, nil, len(key) > 0
	}
	if i == 0 || i == len(key)-1 {
		return nil, false, nil, false
	}
	return key[:i], false, key[i+1:], true
}

func segText(seg []byte, quoted bool) ([]byte, error) {
	if !quoted {
		return seg, nil
	}
	s, err := unquote(seg)
	return []byte(s), err
}

func isIndex(seg []byte, quoted bool) bool {
	if quoted || len(seg) == 0 || len(seg) > 9 {
		return false
	}
	for _, c := range seg {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// top parses a whole document into v.
func (d *tdec) top(v reflect.Value, tt *ttype) error {
	if len(d.lines) == 0 {
		resetValue(v, tt)
		return nil
	}
	for tt.kind == tkPtr {
		if v.IsNil() {
			v.Set(reflect.New(tt.t.Elem()))
		}
		v, tt = v.Elem(), tt.elem
	}
	switch tt.kind {
	case tkStruct, tkMap, tkList:
		resetValue(v, tt)
		for _, ln := range d.lines {
			key, rest, dot, quote, bad := splitEntry(d.content(ln))
			if bad >= 0 {
				return d.errAt(ln, "unterminated quote in the path")
			}
			d.curKey, d.volatile = key, false
			if d.cValid {
				// a quote anywhere in the path means: no shortcut
				if !quote && d.cGen == d.gen && len(key)-dot-1 > 0 && plainName(key[dot+1]) && bytes.Equal(key[:max(dot, 0)], d.cPrefix) {
					if err := d.assign(d.cV, d.cTT, key[dot+1:], rest, ln); err != nil {
						return err
					}
					continue
				}
				d.cValid = false
			}
			if err := d.assign(v, tt, key, rest, ln); err != nil {
				return err
			}
		}
		return nil
	case tkAny:
		first := d.content(d.lines[0])
		if len(d.lines) == 1 {
			if _, rest, _, _, bad := splitEntry(first); bad < 0 && len(rest) == 0 {
				x, err := d.dynScalar(first, d.lines[0])
				if err == nil {
					setAny(v, x)
				}
				return err
			}
		}
		var root any
		for _, ln := range d.lines {
			key, rest, _, _, bad := splitEntry(d.content(ln))
			if bad >= 0 {
				return d.errAt(ln, "unterminated quote in the path")
			}
			segs, err := d.segments(key, ln)
			if err != nil {
				return err
			}
			val, err := d.dynInline(rest, ln)
			if err != nil {
				return err
			}
			if root, err = d.dynAssign(root, segs, val, ln); err != nil {
				return err
			}
		}
		setAny(v, root)
		return nil
	}
	// a single simple value
	if len(d.lines) != 1 {
		return d.errAt(d.lines[1], "expected a single value")
	}
	ln := d.lines[0]
	return d.inline(v, tt, d.content(ln), ln)
}

func setAny(v reflect.Value, x any) {
	if x == nil {
		v.SetZero()
	} else {
		v.Set(reflect.ValueOf(x))
	}
}

// assign sets the value at the path key (relative to v) to rest.
func (d *tdec) assign(v reflect.Value, tt *ttype, key, rest []byte, ln tline) error {
	if len(key) == 0 {
		return d.value(v, tt, rest, ln)
	}
	for tt.kind == tkPtr {
		if v.IsNil() {
			v.Set(reflect.New(tt.t.Elem()))
		}
		v, tt = v.Elem(), tt.elem
	}
	seg, quoted, rem, ok := nextSeg(key)
	if !ok {
		return d.errAt(ln, "bad path %q", key)
	}
	switch tt.kind {
	case tkStruct:
		name, err := segText(seg, quoted)
		if err != nil {
			return d.errAt(ln, "%v", err)
		}
		idx, found := tt.lookup(name)
		if !found {
			names := make([]string, len(tt.fields))
			for i := range tt.fields {
				names[i] = tt.fields[i].name
			}
			return d.errAt(ln, "%s has no field %q (fields: %s)", tt.t, name, strings.Join(names, ", "))
		}
		f := &tt.fields[idx]
		if len(rem) == 0 && !quoted && !d.volatile {
			plen := len(d.curKey) - len(seg)
			if plen > 0 {
				plen-- // the dot before the last name
			}
			d.cPrefix, d.cV, d.cTT, d.cGen, d.cValid = d.curKey[:plen], v, tt, d.gen, true
		}
		return d.assign(v.Field(f.idx), f.typ, rem, rest, ln)
	case tkMap:
		d.volatile = true
		name, err := segText(seg, quoted)
		if err != nil {
			return d.errAt(ln, "%v", err)
		}
		if v.IsNil() {
			v.Set(reflect.MakeMap(tt.t))
		}
		h := tt.getHold()
		err = d.assignMap(v, tt, h, name, rem, rest, ln)
		tt.putHold(h)
		return err
	case tkList:
		n := v.Len()
		switch {
		case !quoted && len(seg) == 1 && seg[0] == '+': // a new item
			if tt.array {
				return d.errAt(ln, "%s is a fixed-size list: use item numbers (0, 1, ...) instead of +", tt.t)
			}
			appendZero(v, tt)
			d.gen++
			return d.assign(v.Index(n), tt.elem, rem, rest, ln)
		case !quoted && len(seg) == 1 && seg[0] == '^': // the item just started
			if n == 0 {
				return d.errAt(ln, "there is no item yet: start one with +")
			}
			return d.assign(v.Index(n-1), tt.elem, rem, rest, ln)
		case isIndex(seg, quoted): // an item by number
			idx, _ := strconv.Atoi(string(seg))
			if idx == n && !tt.array {
				appendZero(v, tt)
				d.gen++
			}
			if idx >= v.Len() {
				return d.errAt(ln, "item %d does not exist: the list has %d items", idx, v.Len())
			}
			return d.assign(v.Index(idx), tt.elem, rem, rest, ln)
		}
		// a name: it belongs to the item just started (the first one if none)
		if n == 0 {
			if tt.array {
				return d.errAt(ln, "%s is a fixed-size list: use item numbers", tt.t)
			}
			appendZero(v, tt)
			d.gen++
			n = 1
		}
		return d.assign(v.Index(n-1), tt.elem, key, rest, ln)
	case tkAny:
		d.volatile = true
		segs, err := d.segments(key, ln)
		if err != nil {
			return err
		}
		val, err := d.dynInline(rest, ln)
		if err != nil {
			return err
		}
		var cur any
		if !v.IsNil() {
			cur = v.Interface()
		}
		x, err := d.dynAssign(cur, segs, val, ln)
		if err == nil {
			setAny(v, x)
		}
		return err
	}
	return d.errAt(ln, "%s has nothing inside it to name with .%s", tt.t, seg)
}

// appendZero adds a zeroed element to the slice v, growing it generously.
func appendZero(v reflect.Value, tt *ttype) {
	n := v.Len()
	if n == v.Cap() {
		ns := reflect.MakeSlice(v.Type(), n, max(4, 2*n))
		reflect.Copy(ns, v)
		v.Set(ns)
	}
	v.SetLen(n + 1)
	resetValue(v.Index(n), tt.elem)
}

// resetValue returns v to its zero value but keeps what can be reused: slice
// capacity and map storage (as Unmarshal does), so decoding into the same
// variable again allocates little.
func resetValue(v reflect.Value, tt *ttype) {
	switch tt.kind {
	case tkStruct:
		for i := range tt.fields {
			f := &tt.fields[i]
			resetValue(v.Field(f.idx), f.typ)
		}
	case tkList:
		if tt.array {
			v.SetZero()
		} else {
			v.SetLen(0)
		}
	case tkMap:
		if !v.IsNil() {
			v.Clear()
		}
	default:
		v.SetZero()
	}
}

func (d *tdec) assignMap(v reflect.Value, tt *ttype, h *mapHold, name, rem, rest []byte, ln tline) error {
	if err := d.keyText(h.k, tt.key, name, ln); err != nil {
		return err
	}
	if len(rem) > 0 { // descending: start from the existing value
		if cur := v.MapIndex(h.k); cur.IsValid() {
			h.v.Set(cur)
		}
	}
	if err := d.assign(h.v, tt.elem, rem, rest, ln); err != nil {
		return err
	}
	v.SetMapIndex(h.k, h.v)
	return nil
}

type tseg struct {
	b      []byte
	quoted bool
}

func (d *tdec) segments(key []byte, ln tline) ([]tseg, error) {
	var segs []tseg
	for len(key) > 0 {
		seg, quoted, rem, ok := nextSeg(key)
		if !ok {
			return nil, d.errAt(ln, "bad path %q", key)
		}
		segs, key = append(segs, tseg{seg, quoted}), rem
	}
	return segs, nil
}

// dynAssign sets the value at segs inside the untyped value cur and returns
// the updated value. `+` appends to a list, numbers pick a list item, `^` is
// the item just started, and other names key a map (or, after a list, belong
// to its last item).
func (d *tdec) dynAssign(cur any, segs []tseg, val any, ln tline) (any, error) {
	if len(segs) == 0 {
		return val, nil
	}
	s := segs[0]
	plus := !s.quoted && len(s.b) == 1 && s.b[0] == '+'
	last := !s.quoted && len(s.b) == 1 && s.b[0] == '^'
	index := isIndex(s.b, s.quoted)
	list, isList := cur.([]any)
	_, isMap := cur.(map[string]any)
	switch {
	case plus || last || (index && !isMap):
		if cur != nil && !isList {
			return nil, d.errAt(ln, "cannot use %s inside a %T", s.b, cur)
		}
		var idx int
		switch {
		case plus:
			idx = len(list)
			list = append(list, nil)
		case last:
			if len(list) == 0 {
				return nil, d.errAt(ln, "there is no item yet: start one with +")
			}
			idx = len(list) - 1
		default:
			idx, _ = strconv.Atoi(string(s.b))
			if idx > len(list) {
				return nil, d.errAt(ln, "item %d does not exist: the list has %d items", idx, len(list))
			}
			if idx == len(list) {
				list = append(list, nil)
			}
		}
		x, err := d.dynAssign(list[idx], segs[1:], val, ln)
		if err != nil {
			return nil, err
		}
		list[idx] = x
		return list, nil
	case isList: // a name after a list: the last item (the first if none)
		if len(list) == 0 {
			list = append(list, nil)
		}
		x, err := d.dynAssign(list[len(list)-1], segs, val, ln)
		if err != nil {
			return nil, err
		}
		list[len(list)-1] = x
		return list, nil
	}
	name, err := segText(s.b, s.quoted)
	if err != nil {
		return nil, d.errAt(ln, "%v", err)
	}
	m, _ := cur.(map[string]any)
	if cur != nil && !isMap {
		return nil, d.errAt(ln, "cannot name %q inside %T", name, cur)
	}
	if m == nil {
		m = map[string]any{}
	}
	x, err := d.dynAssign(m[string(name)], segs[1:], val, ln)
	if err != nil {
		return nil, err
	}
	m[string(name)] = x
	return m, nil
}

// value sets v from what follows its path: rest (possibly empty).
func (d *tdec) value(v reflect.Value, tt *ttype, rest []byte, ln tline) error {
	if tt.kind == tkPtr {
		if len(rest) == 0 || (len(rest) == 1 && rest[0] == '~') {
			v.SetZero() // nothing, or ~, is null (a pointer to an all-zero value reads back as nil too)
			return nil
		}
		if v.IsNil() {
			v.Set(reflect.New(tt.t.Elem()))
		}
		return d.value(v.Elem(), tt.elem, rest, ln)
	}
	if len(rest) == 0 || (len(rest) == 1 && rest[0] == '~' && tt.kind != tkAny) {
		resetValue(v, tt) // zero, but keep reusable storage
		return nil
	}
	return d.inline(v, tt, rest, ln)
}

// word returns the next token of rest starting at pos: a quoted string or a
// run of non-space characters. end is the index after it; bad is true for an
// unterminated quote.
func word(rest []byte, pos int) (tok []byte, end int, bad bool) {
	for pos < len(rest) && isSpace(rest[pos]) {
		pos++
	}
	if pos >= len(rest) {
		return nil, pos, false
	}
	start := pos
	if rest[pos] == '"' {
		pos++
		for pos < len(rest) && rest[pos] != '"' {
			if rest[pos] == '\\' {
				pos++
			}
			pos++
		}
		if pos >= len(rest) {
			return nil, len(rest), true
		}
		return rest[start : pos+1], pos + 1, false
	}
	for pos < len(rest) && !isSpace(rest[pos]) {
		pos++
	}
	return rest[start:pos], pos, false
}

// inline parses a value written on one line.
func (d *tdec) inline(v reflect.Value, tt *ttype, rest []byte, ln tline) error {
	switch tt.kind {
	case tkPtr:
		return d.value(v, tt, rest, ln)
	case tkAny:
		x, err := d.dynInline(rest, ln)
		if err == nil {
			setAny(v, x)
		}
		return err
	case tkList:
		if !tt.inline {
			return d.errAt(ln, "a list of %s is written one item per line: Name.0.Field value, Name.1.Field value, ...", tt.elem.t)
		}
		return d.words(v, tt, rest, ln)
	case tkStruct, tkMap:
		return d.errAt(ln, "a %s is written one field per line: Name.Field value (or ~ for none)", tt.t)
	case tkString, tkText:
		return d.scalarText(v, tt, rest, ln)
	}
	// numbers, bools and hex bytes are one word
	tok, end, bad := word(rest, 0)
	if bad {
		return d.errAt(ln, "unterminated quote")
	}
	if t2, _, _ := word(rest, end); len(t2) > 0 {
		return d.errAt(ln, "unexpected text %q after %q", t2, tok)
	}
	return d.scalarText(v, tt, tok, ln)
}

// words parses a list of simple values written on one line.
func (d *tdec) words(v reflect.Value, tt *ttype, rest []byte, ln tline) error {
	n := 0
	for pos := 0; ; n++ {
		tok, end, bad := word(rest, pos)
		if bad {
			return d.errAt(ln, "unterminated quote")
		}
		if len(tok) == 0 {
			break
		}
		pos = end
	}
	if tt.array {
		if n > v.Len() {
			return d.errAt(ln, "%d items do not fit in %s", n, tt.t)
		}
		v.SetZero()
	} else if v.Cap() >= n {
		v.SetLen(n)
	} else {
		v.Set(reflect.MakeSlice(tt.t, n, n))
	}
	pos := 0
	for i := 0; i < n; i++ {
		tok, end, _ := word(rest, pos)
		pos = end
		if err := d.scalarText(v.Index(i), tt.elem, tok, ln); err != nil {
			return err
		}
	}
	return nil
}

func unsafeStr(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

func parseIntTok(tok []byte) (int64, bool) {
	i := 0
	neg := false
	if len(tok) > 0 && (tok[0] == '-' || tok[0] == '+') {
		neg, i = tok[0] == '-', 1
	}
	if i < len(tok) && len(tok)-i <= 18 {
		var n int64
		j := i
		for ; j < len(tok); j++ {
			c := tok[j] - '0'
			if c > 9 {
				break
			}
			n = n*10 + int64(c)
		}
		if j == len(tok) {
			if neg {
				n = -n
			}
			return n, true
		}
	}
	n, err := strconv.ParseInt(unsafeStr(tok), 0, 64)
	return n, err == nil
}

func parseUintTok(tok []byte) (uint64, bool) {
	if len(tok) > 0 && len(tok) <= 19 {
		var n uint64
		j := 0
		for ; j < len(tok); j++ {
			c := tok[j] - '0'
			if c > 9 {
				break
			}
			n = n*10 + uint64(c)
		}
		if j == len(tok) {
			return n, true
		}
	}
	n, err := strconv.ParseUint(unsafeStr(tok), 0, 64)
	return n, err == nil
}

func parseFloatWord(w string) (float64, bool) {
	if f, err := strconv.ParseFloat(w, 64); err == nil {
		return f, true // also nan, inf, -inf, infinity in any case
	}
	if n, err := strconv.ParseInt(w, 0, 64); err == nil { // 0x10 and friends
		return float64(n), true
	}
	return 0, false
}

// keyText sets a map key from the (already unquoted) text of a path name. Unlike
// a value, a key has no null: "~" is just a name.
func (d *tdec) keyText(v reflect.Value, tt *ttype, name []byte, ln tline) error {
	if tt.kind == tkString {
		v.SetString(d.str(name))
		return nil
	}
	return d.scalarText(v, tt, name, ln)
}

// scalarText sets v from the text of one value. For strings and text types
// tok is the whole value (quoted or bare); for the rest it is a single word.
func (d *tdec) scalarText(v reflect.Value, tt *ttype, tok []byte, ln tline) error {
	if tt.kind == tkPtr {
		if len(tok) == 1 && tok[0] == '~' {
			v.SetZero()
			return nil
		}
		if v.IsNil() {
			v.Set(reflect.New(tt.t.Elem()))
		}
		return d.scalarText(v.Elem(), tt.elem, tok, ln)
	}
	if len(tok) == 1 && tok[0] == '~' && tt.kind != tkAny {
		v.SetZero()
		return nil
	}
	switch tt.kind {
	case tkBool:
		switch string(tok) {
		case "true":
			v.SetBool(true)
		case "false":
			v.SetBool(false)
		default:
			return d.errAt(ln, "%q is not true or false", tok)
		}
	case tkInt:
		n, ok := parseIntTok(tok)
		if !ok || v.OverflowInt(n) {
			return d.errAt(ln, "%q is not a valid %s", tok, tt.t)
		}
		v.SetInt(n)
	case tkUint:
		n, ok := parseUintTok(tok)
		if !ok || v.OverflowUint(n) {
			return d.errAt(ln, "%q is not a valid %s", tok, tt.t)
		}
		v.SetUint(n)
	case tkFloat:
		f, ok := parseFloatWord(unsafeStr(tok))
		if !ok || (tt.bits == 32 && !math.IsInf(f, 0) && !math.IsNaN(f) && math.Abs(f) > math.MaxFloat32) {
			return d.errAt(ln, "%q is not a valid %s", tok, tt.t)
		}
		v.SetFloat(f)
	case tkString:
		if len(tok) > 0 && tok[0] == '"' {
			s, err := unquote(tok)
			if err != nil {
				return d.errAt(ln, "%v", err)
			}
			v.SetString(s)
		} else {
			v.SetString(d.str(tok))
		}
	case tkBytes:
		raw, err := parseHex(tok)
		if err != nil {
			return d.errAt(ln, "%v", err)
		}
		if v.Kind() == reflect.Slice {
			v.SetBytes(raw)
		} else {
			if len(raw) > v.Len() {
				return d.errAt(ln, "%d bytes do not fit in %s", len(raw), tt.t)
			}
			v.SetZero()
			reflect.Copy(v, reflect.ValueOf(raw))
		}
	case tkText:
		txt := tok
		if len(tok) > 0 && tok[0] == '"' {
			s, err := unquote(tok)
			if err != nil {
				return d.errAt(ln, "%v", err)
			}
			txt = []byte(s)
		}
		if err := v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText(txt); err != nil {
			return d.errAt(ln, "%v", err)
		}
	case tkBin:
		raw, err := parseHex(tok)
		if err != nil {
			return d.errAt(ln, "%v", err)
		}
		if err := v.Addr().Interface().(encoding.BinaryUnmarshaler).UnmarshalBinary(raw); err != nil {
			return d.errAt(ln, "%v", err)
		}
	default:
		return d.errAt(ln, "cannot read a %s from a single value", tt.t)
	}
	return nil
}

func parseHex(tok []byte) ([]byte, error) {
	if len(tok) < 2 || tok[0] != '0' || (tok[1] != 'x' && tok[1] != 'X') {
		return nil, fmt.Errorf("expected hex bytes like 0x00ff, got %q", tok)
	}
	raw := make([]byte, hex.DecodedLen(len(tok)-2))
	if _, err := hex.Decode(raw, tok[2:]); err != nil {
		return nil, fmt.Errorf("bad hex bytes %q: %v", tok, err)
	}
	return raw, nil
}

// unquote decodes a double-quoted string with escapes.
func unquote(tok []byte) (string, error) {
	if len(tok) < 2 || tok[0] != '"' || tok[len(tok)-1] != '"' {
		return "", fmt.Errorf("bad quoted string %s", tok)
	}
	in := tok[1 : len(tok)-1]
	if bytes.IndexByte(in, '\\') < 0 {
		return string(in), nil
	}
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c != '\\' {
			out = append(out, c)
			continue
		}
		i++
		if i >= len(in) {
			return "", fmt.Errorf("unterminated escape in %s", tok)
		}
		switch x := in[i]; x {
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case '0':
			out = append(out, 0)
		case '\\', '"', '\'', '/':
			out = append(out, x)
		case 'x', 'u', 'U':
			n := 2
			switch x {
			case 'u':
				n = 4
			case 'U':
				n = 8
			}
			if i+n >= len(in) {
				return "", fmt.Errorf("short \\%c escape in %s", x, tok)
			}
			u, err := strconv.ParseUint(string(in[i+1:i+1+n]), 16, 32)
			if err != nil {
				return "", fmt.Errorf("bad \\%c escape in %s", x, tok)
			}
			i += n
			if x == 'x' {
				out = append(out, byte(u))
			} else {
				out = utf8.AppendRune(out, rune(u))
			}
		default:
			return "", fmt.Errorf("unknown escape \\%c in %s", x, tok)
		}
	}
	return string(out), nil
}

// ---------------------------------------------------------------- dynamic

// dynInline parses an untyped value on one line: nothing is nil, one word is
// a scalar, several words are a list.
func (d *tdec) dynInline(rest []byte, ln tline) (any, error) {
	if len(rest) == 0 {
		return nil, nil
	}
	tok, end, bad := word(rest, 0)
	if bad {
		return nil, d.errAt(ln, "unterminated quote")
	}
	if t2, _, _ := word(rest, end); len(t2) == 0 {
		return d.dynScalar(tok, ln)
	}
	out := []any{}
	for pos := 0; ; {
		tok, end, bad := word(rest, pos)
		if bad {
			return nil, d.errAt(ln, "unterminated quote")
		}
		if len(tok) == 0 {
			return out, nil
		}
		x, err := d.dynScalar(tok, ln)
		if err != nil {
			return nil, err
		}
		out, pos = append(out, x), end
	}
}

func (d *tdec) dynScalar(tok []byte, ln tline) (any, error) {
	switch c := tok[0]; {
	case c == '"':
		s, err := unquote(tok)
		if err != nil {
			return nil, d.errAt(ln, "%v", err)
		}
		return s, nil
	case c == '~':
		switch string(tok) {
		case "~":
			return nil, nil
		case "~list":
			return []any{}, nil
		case "~map":
			return map[string]any{}, nil
		}
	case len(tok) == 4 && string(tok) == "true":
		return true, nil
	case len(tok) == 5 && string(tok) == "false":
		return false, nil
	case c == '0' && len(tok) >= 2 && (tok[1] == 'x' || tok[1] == 'X'):
		raw, err := parseHex(tok)
		if err != nil {
			return nil, d.errAt(ln, "%v", err)
		}
		return raw, nil
	case c >= '0' && c <= '9' || c == '-' || c == '+' || c == '.' || c == 'n' || c == 'N' || c == 'i' || c == 'I':
		if n, ok := parseIntTok(tok); ok {
			return n, nil
		}
		if n, ok := parseUintTok(tok); ok {
			return n, nil
		}
		if f, ok := parseFloatWord(unsafeStr(tok)); ok {
			return f, nil
		}
	}
	return d.str(tok), nil
}
