package codec

import (
	"errors"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type tAddr struct {
	Street string
	Zip    uint32
}

type tLine struct {
	SKU   string
	Qty   int32
	Price float64
}

type tOrder struct {
	ID       int64
	Customer string
	Lines    []tLine
	Tags     []string
	Meta     map[string]string
}

type tAll struct {
	ID       int64
	Name     string
	Age      uint8
	Active   bool
	Score    float64
	Ratio    float32
	Tags     []string
	Nums     []int32
	Raw      []byte
	Arr      [3]uint16
	Addr     *tAddr
	Home     tAddr
	Items    []tAddr
	Orders   []tOrder
	Matrix   [][]int
	Grid     [2][2]int8
	Slots    [3]tAddr
	Rows     [2][]tLine
	Cube     [][][]string
	PtrItems []*tAddr
	Opt      *int
	M        map[string]int64
	IM       map[int32]string
	ByCity   map[string][]tAddr
	Deep     map[string]map[string][]int
	Any      any
	Anys     []any
	When     time.Time
	IP       netip.Addr
	Renamed  string `codec:"alias"`
	Skipped  string `codec:"-"`
	private  int
}

func tSample() tAll {
	opt := 0
	return tAll{
		ID: -1234567, Name: "Ada \"the\" Lovelace\n\ttab é 日本", Age: 36, Active: true, Score: 3.5, Ratio: -0.25,
		Tags: []string{"admin", "two words", "ops;x", "~", "-"}, Nums: []int32{1, -2, 300}, Raw: []byte{0, 1, 0xfe, 0xff},
		Arr: [3]uint16{1, 0, 65535}, Addr: &tAddr{"12 Main St", 12345}, Home: tAddr{"9 Elm", 1},
		Items: []tAddr{{"a", 1}, {}, {"c", 3}},
		Orders: []tOrder{
			{ID: 1, Customer: "Ann", Lines: []tLine{{"x", 1, 2.5}, {"y", 2, 0}}, Tags: []string{"rush"}, Meta: map[string]string{"k": "v", "a b": "c d"}},
			{ID: 2},
			{ID: 3, Lines: []tLine{{SKU: "z"}}},
		},
		Matrix: [][]int{{1, 2, 3}, nil, {4}, {5, 6}}, Grid: [2][2]int8{{1, 2}, {0, -4}}, Slots: [3]tAddr{{}, {"mid", 5}, {}}, Rows: [2][]tLine{nil, {{"r", 1, 2}, {SKU: "s"}}},
		Cube:     [][][]string{{{"a", "b"}, {"c"}}, {{"d e"}}},
		PtrItems: []*tAddr{{"p", 1}, nil, {"q", 2}}, Opt: &opt,
		M: map[string]int64{"z": 1, "a": -2, "m k": 3, "": 4, "1": 5}, IM: map[int32]string{2: "two", -1: "neg"},
		ByCity: map[string][]tAddr{"Paris": {{"r1", 1}, {"r2", 2}}, "New York": {{"b", 9}}},
		Deep:   map[string]map[string][]int{"a": {"x": {1, 2}, "y": {3}}, "b": {"z": {4}}},
		Any: map[string]any{"n": int64(5), "f": 2.0, "s": "x y", "l": []any{true, nil, "y", []any{int64(1), int64(2)}},
			"b": []byte{9}, "m": map[string]any{"k": "v", "7": "seven", "empty": map[string]any{}}, "e": []any{}, "num-str": "36"},
		Anys: []any{int64(1), "two", 3.5, uint64(math.MaxUint64), map[string]any{"a": int64(1)}, []any{}, "true", "-x"},
		When: time.Date(2026, 11, 18, 10, 30, 0, 123, time.UTC), IP: netip.MustParseAddr("2001:db8::1"),
		Renamed: "r", Skipped: "never",
	}
}

func eqAll(t *testing.T, got, want tAll, what string) {
	t.Helper()
	if !got.When.Equal(want.When) {
		t.Fatalf("%s: time %v vs %v", what, got.When, want.When)
	}
	got.When, want.When = time.Time{}, time.Time{}
	want.Skipped = ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s mismatch\n got %#v\nwant %#v", what, got, want)
	}
}

func TestTextRoundTrip(t *testing.T) {
	in := tSample()
	txt, err := MarshalText(&in)
	if err != nil {
		t.Fatal(err)
	}
	var out tAll
	if err := UnmarshalText(txt, &out); err != nil {
		t.Fatalf("%v\n%s", err, txt)
	}
	eqAll(t, out, in, "text round trip\n"+string(txt))
	if strings.Contains(string(txt), "never") || strings.Contains(string(txt), "Skipped") || !strings.Contains(string(txt), "alias ") {
		t.Fatalf("tag handling:\n%s", txt)
	}
	// printing what was parsed gives the same text (stable)
	txt2, _ := MarshalText(&out)
	if string(txt) != string(txt2) {
		t.Fatalf("not stable:\n%s\n----\n%s", txt, txt2)
	}
}

func TestTextShape(t *testing.T) {
	in := tOrder{ID: 7, Customer: "Ada Lovelace", Lines: []tLine{{"a", 2, 9.5}, {"b", 1, 0}}, Tags: []string{"x", "y z"}, Meta: map[string]string{"k": "v"}}
	got, err := MarshalText(&in)
	if err != nil {
		t.Fatal(err)
	}
	want := `ID 7
Customer Ada Lovelace
Lines.+
Lines.SKU a
Lines.Qty 2
Lines.Price 9.5
Lines.+
Lines.SKU b
Lines.Qty 1
Tags x "y z"
Meta.k v
`
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// no structure characters at all
	if strings.ContainsAny(string(got), "{}[]\t") || strings.Contains(string(got), "begin") {
		t.Fatalf("unexpected syntax:\n%s", got)
	}
}

func TestTextMatchesBinary(t *testing.T) {
	// binary -> text -> binary must be byte-identical
	in := tSample()
	in.Any = map[string]any{"k": int64(1)}
	in.Anys = nil
	bin, err := Marshal(&in)
	if err != nil {
		t.Fatal(err)
	}
	txt, err := ToText(bin, &tAll{})
	if err != nil {
		t.Fatal(err)
	}
	bin2, err := FromText(txt, &tAll{})
	if err != nil {
		t.Fatalf("%v\n%s", err, txt)
	}
	if string(bin) != string(bin2) {
		t.Fatalf("binary differs after text round trip\n%s", txt)
	}
}

func TestTextByHand(t *testing.T) {
	var a tAll
	src := `
	# a person, written by hand: any order, any spacing, comments anywhere between lines
	name Ada Lovelace            # (this is part of the name: comments are whole lines only)
	AGE 0x24
	Active true
	tags   a "b c"  d
	nums 1 2 3
	raw 0xdeadbeef
	arr 7 8
	addr.street Main
	addr.zip 9
	items.+
	items.street one
	items.zip 1
	items.+
	items.+ ~
	items.street three
	orders.id 5
	orders.lines.sku s1
	orders.lines.qty 3
	orders.lines.+
	orders.lines.sku s2
	orders.meta."a.b" dotted
	orders.meta.plain x
	matrix.+ 1 2
	matrix.+ 3
	grid.1 5 6
	m.x 1; m."y z" 2; m.q 3
	byCity.Paris.street r1
	byCity."New York".+.zip 7
	deep.a.b 1 2 3
	score 3
	any.k.+ 1
	any.k.+ 2.5
	any.k.+ s
	any.k.+ ~
	any.name "36"
	opt 0
	`
	if err := UnmarshalText([]byte(src), &a); err != nil {
		t.Fatal(err)
	}
	if a.Name != "Ada Lovelace            # (this is part of the name: comments are whole lines only)" {
		t.Fatalf("name %q", a.Name)
	}
	a.Name = "Ada"
	if a.Age != 36 || !a.Active || !reflect.DeepEqual(a.Tags, []string{"a", "b c", "d"}) || string(a.Raw) != "\xde\xad\xbe\xef" ||
		a.Arr != [3]uint16{7, 8, 0} || a.Addr != nil && false || a.Score != 3 {
		t.Fatalf("%+v", a)
	}
	if len(a.Items) != 3 || a.Items[0] != (tAddr{"one", 1}) || a.Items[1] != (tAddr{}) || a.Items[2].Street != "three" {
		t.Fatalf("items %+v", a.Items)
	}
	o := a.Orders[0]
	if o.ID != 5 || len(o.Lines) != 2 || o.Lines[1].SKU != "s2" || o.Meta["a.b"] != "dotted" || o.Meta["plain"] != "x" {
		t.Fatalf("orders %+v", a.Orders)
	}
	if !reflect.DeepEqual(a.Matrix, [][]int{{1, 2}, {3}}) || a.Grid[1] != [2]int8{5, 6} {
		t.Fatalf("matrix %v grid %v", a.Matrix, a.Grid)
	}
	if a.M["y z"] != 2 || a.M["q"] != 3 || a.ByCity["Paris"][0].Street != "r1" || a.ByCity["New York"][0].Zip != 7 ||
		!reflect.DeepEqual(a.Deep["a"]["b"], []int{1, 2, 3}) || a.Opt == nil || *a.Opt != 0 {
		t.Fatalf("maps %+v", a)
	}
	m := a.Any.(map[string]any)
	if got := m["k"].([]any); got[0] != int64(1) || got[1] != 2.5 || got[2] != "s" || got[3] != nil || m["name"] != "36" {
		t.Fatalf("%#v", m)
	}

	// a whole document on one line, as for curl -d
	var o2 tOrder
	if err := UnmarshalText([]byte(`ID 7; Customer Ada Lovelace; Lines.SKU a; Lines.Qty 2; Lines.+; Lines.SKU b; Tags x y`), &o2); err != nil ||
		o2.ID != 7 || o2.Customer != "Ada Lovelace" || len(o2.Lines) != 2 || o2.Lines[0].Qty != 2 || len(o2.Tags) != 2 {
		t.Fatalf("one-liner: %v %+v", err, o2)
	}

	// top-level scalars, lists and dynamic values
	var n int
	var s string
	var x any
	var l []string
	var ll [][]int
	if UnmarshalText([]byte("-42"), &n) != nil || n != -42 || UnmarshalText([]byte("hello  there"), &s) != nil || s != "hello  there" {
		t.Fatal(n, s)
	}
	if UnmarshalText([]byte("+ a\n+ \"b c\"\n"), &l) != nil || !reflect.DeepEqual(l, []string{"a", "b c"}) {
		t.Fatal(l)
	}
	if UnmarshalText([]byte("+ 1 2\n+ 3\n"), &ll) != nil || !reflect.DeepEqual(ll, [][]int{{1, 2}, {3}}) {
		t.Fatal(ll)
	}
	// numbers still pick an existing item, for editing one in place
	if UnmarshalText([]byte("+ 1 2\n+ 3\n0 9\n1 8 7\n"), &ll) != nil || !reflect.DeepEqual(ll, [][]int{{9}, {8, 7}}) {
		t.Fatal(ll)
	}
	if err := UnmarshalText([]byte("a 1\na.b 2\n"), &x); err == nil || !strings.Contains(err.Error(), "cannot name") {
		t.Fatalf("naming inside a scalar should fail: %v", err)
	}
	if err := UnmarshalText([]byte("a.b 1\na.0 2\n"), &x); err != nil || x.(map[string]any)["a"].(map[string]any)["0"] != int64(2) {
		t.Fatalf("a number under an object is a key: %v %#v", err, x)
	}
	if err := UnmarshalText([]byte("a.+ 1\na.+\na.b x\nc ~list"), &x); err != nil ||
		!reflect.DeepEqual(x, map[string]any{"a": []any{int64(1), map[string]any{"b": "x"}}, "c": []any{}}) {
		t.Fatalf("%v %#v", err, x)
	}
	if UnmarshalText([]byte("42"), &x) != nil || x != int64(42) {
		t.Fatal(x)
	}
}

func TestTextZeroNullEmpty(t *testing.T) {
	type S struct {
		P  *tAddr
		I  *int
		L  []string
		M  map[string]int
		A  tAddr
		B  bool
		F  float64
		St string
	}
	// zero values are omitted
	txt, _ := MarshalText(&S{})
	if len(txt) != 0 {
		t.Fatalf("zero struct printed %q", txt)
	}
	var s S
	if err := UnmarshalText(txt, &s); err != nil || !reflect.DeepEqual(s, S{}) {
		t.Fatal(err, s)
	}
	// explicit zero, null and empty are all accepted and mean zero
	src := "P ~\nI ~\nL ~\nM ~\nA ~\nB false\nF 0\nSt \"\"\n"
	s = S{P: &tAddr{}, L: []string{"x"}, M: map[string]int{"k": 1}}
	if err := UnmarshalText([]byte(src), &s); err != nil || s.P != nil || s.I != nil || len(s.L) != 0 || len(s.M) != 0 ||
		s.A != (tAddr{}) || s.B || s.F != 0 || s.St != "" {
		t.Fatalf("%v %+v", err, s)
	}
	// an explicit zero inside a list keeps its place
	var l []tAddr
	if err := UnmarshalText([]byte("+\nStreet a\n+\n+\nStreet c\n"), &l); err != nil || len(l) != 3 || l[1] != (tAddr{}) {
		t.Fatal(err, l)
	}
	out, _ := MarshalText(l)
	if string(out) != "+\nStreet a\n+\n+\nStreet c\n" {
		t.Fatalf("%q", out)
	}
	var ps []*int
	one := 1
	out, _ = MarshalText([]*int{&one, nil, &one})
	if string(out) != "+ 1\n+ ~\n+ 1\n" {
		t.Fatalf("%q", out)
	}
	if err := UnmarshalText(out, &ps); err != nil || ps[1] != nil || *ps[0] != 1 {
		t.Fatal(err, ps)
	}
}

func TestTextErrors(t *testing.T) {
	cases := []struct{ src, want string }{
		{`Name x; Nmae 1`, `no field "Nmae"`},
		{`Age 300`, "not a valid uint8"},
		{`Age -1`, "not a valid uint8"},
		{`Age 1 2`, `unexpected text "2"`},
		{`Tags "a`, "unterminated quote"},
		{`Arr 1 2 3 4`, "do not fit"},
		{`Items.5.Street x`, "item 5 does not exist"},
		{`Items.x.Street x`, `no field "x"`},
		{`Items.^.Street x`, "no item yet"},
		{`Arr.+ 1`, "fixed-size"},
		{`Items 1`, "written one item per line"},
		{`Addr 1`, "one field per line"},
		{`Name.x 1`, "nothing inside"},
		{`Name "bad \q escape"`, "unknown escape"},
		{`Raw 0xabc`, "bad hex"},
		{`Raw 12`, "expected hex"},
		{`Active yes`, "not true or false"},
		{`IP not-an-ip`, "ParseAddr"},
		{`Addr..Zip 1`, "bad path"},
		{`Addr. 1`, "bad path"},
		{`"Addr 1`, "unterminated quote"},
	}
	for _, c := range cases {
		var a tAll
		err := UnmarshalText([]byte(c.src), &a)
		var te *TextError
		if !errors.As(err, &te) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want %q", c.src, err, c.want)
		}
	}
	// positions are line/column
	var a tAll
	err := UnmarshalText([]byte("Name x\n  Bogus 1\n"), &a)
	var te *TextError
	if !errors.As(err, &te) || te.Line != 2 || te.Col != 3 {
		t.Fatalf("position: %v", err)
	}
	if err := UnmarshalText([]byte("1"), a); !errors.Is(err, ErrNeedPointer) {
		t.Fatal(err)
	}
	// the message for an unknown field lists the right names
	if err := UnmarshalText([]byte("Zipp 1"), &tAddr{}); err == nil || !strings.Contains(err.Error(), "fields: Street, Zip") {
		t.Fatal(err)
	}
	// deep paths are handled without recursion trouble
	var x any
	deep := strings.Repeat("a.", 5000) + "a 1"
	if err := UnmarshalText([]byte(deep), &x); err != nil {
		t.Fatal(err)
	}
}

func TestTextLeadingZerosAreDecimal(t *testing.T) {
	for in, want := range map[string]int64{"010": 10, "0010": 10, "0x10": 16, "0b101": 5, "0o17": 15, "-007": -7, "0": 0, "0123456789012345678": 123456789012345678, "00000000000000000000010": 10} {
		var n int64
		if err := UnmarshalText([]byte(in), &n); err != nil || n != want {
			t.Errorf("%q -> %d, %v; want %d", in, n, err, want)
		}
	}
	var u uint64
	if err := UnmarshalText([]byte("0000000000000000000010"), &u); err != nil || u != 10 {
		t.Errorf("uint: %d %v", u, err)
	}
}

func TestTextFloats(t *testing.T) {
	for _, f := range []float64{0.5, 3, -7, 1e21, 1e-9, math.MaxFloat64, math.SmallestNonzeroFloat64, math.Inf(1), math.Inf(-1)} {
		txt, _ := MarshalText(f)
		var g float64
		if err := UnmarshalText(txt, &g); err != nil || g != f {
			t.Fatalf("%v -> %s -> %v %v", f, txt, g, err)
		}
		var x any = f
		txt, _ = MarshalText(&x)
		var y any
		UnmarshalText(txt, &y)
		if y != f {
			t.Fatalf("dynamic float %v printed %q read back as %#v", f, txt, y)
		}
	}
	txt, _ := MarshalText(math.NaN())
	var g float64
	if UnmarshalText(txt, &g) != nil || !math.IsNaN(g) {
		t.Fatal(string(txt))
	}
}

func TestTextStringsRoundTrip(t *testing.T) {
	for _, in := range []string{"", " ", " lead", "trail ", "a;b", "#x", "~", "~list", "\"q\"", "a\nb", "a\tb", "tab\t", "-", "- x", "0", "1.5", "true", "0xff",
		"ok\xff\xfe\x00end", "日本語 テスト", "has \\ backslash", "end", "begin", "a.b", "x=y", "mid\"quote", "+", "^", "12", "a\"", "\"a"} {
		for _, wrap := range []string{"scalar", "field", "list", "map", "dyn", "dynlist", "key"} {
			var txt []byte
			var err error
			check := func(got string) {
				t.Helper()
				if got != in {
					t.Fatalf("%s %q -> %q -> %q", wrap, in, txt, got)
				}
			}
			switch wrap {
			case "scalar":
				txt, _ = MarshalText(in)
				var o string
				err = UnmarshalText(txt, &o)
				check(o)
			case "field":
				v := struct{ S string }{in}
				txt, _ = MarshalText(&v)
				var o struct{ S string }
				err = UnmarshalText(txt, &o)
				if in == "" {
					continue
				}
				check(o.S)
			case "list":
				txt, _ = MarshalText([]string{"x", in, "y"})
				var o []string
				err = UnmarshalText(txt, &o)
				if err == nil && (len(o) != 3 || o[0] != "x" || o[2] != "y") {
					t.Fatalf("list %q -> %q -> %q", in, txt, o)
				}
				if len(o) == 3 {
					check(o[1])
				}
			case "map":
				txt, _ = MarshalText(map[string]string{in: in})
				var o map[string]string
				err = UnmarshalText(txt, &o)
				if err == nil {
					check(o[in])
				}
			case "dyn":
				x := any(map[string]any{"k": in})
				txt, _ = MarshalText(&x)
				var o any
				err = UnmarshalText(txt, &o)
				if err == nil {
					s, _ := o.(map[string]any)["k"].(string)
					check(s)
				}
			case "dynlist":
				x := any([]any{in, in})
				txt, _ = MarshalText(&x)
				var o any
				err = UnmarshalText(txt, &o)
				if err == nil {
					l, _ := o.([]any)
					if len(l) != 2 {
						t.Fatalf("dynlist %q -> %q -> %#v", in, txt, o)
					}
					s, _ := l[0].(string)
					check(s)
				}
			case "key":
				x := any(map[string]any{in: "v"})
				txt, _ = MarshalText(&x)
				var o any
				err = UnmarshalText(txt, &o)
				if err == nil {
					if _, ok := o.(map[string]any)[in]; !ok {
						t.Fatalf("key %q -> %q -> %#v", in, txt, o)
					}
				}
			}
			if err != nil {
				t.Fatalf("%s %q -> %q: %v", wrap, in, txt, err)
			}
		}
	}
}

func TestTextPointersAndRecursion(t *testing.T) {
	type Node struct {
		Name string
		Kids []*Node
		Next *Node
	}
	in := &Node{Name: "root", Kids: []*Node{{Name: "a", Kids: []*Node{{Name: "a1"}}}, {Name: "b"}}, Next: &Node{Name: "n"}}
	txt, err := MarshalText(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Node
	if err := UnmarshalText(txt, &out); err != nil || !reflect.DeepEqual(&out, in) {
		t.Fatalf("%v\n%s\n%+v", err, txt, out)
	}
	if !strings.Contains(string(txt), "Kids.Kids.Name a1") {
		t.Fatal(string(txt))
	}
}

func TestMedia(t *testing.T) {
	if f, ok := FormatForMedia("Text/X-Strata; charset=utf-8"); !ok || f != FormatText {
		t.Fatal(f, ok)
	}
	for _, c := range []struct {
		accept string
		want   Format
	}{
		{"", FormatBinary}, {"*/*", FormatBinary}, {MediaText, FormatText}, {"text/plain", FormatText},
		{"application/x-strata;q=0.4, text/x-strata;q=0.9", FormatText},
		{"text/x-strata;q=0, application/x-strata+base64", FormatBase64},
		{"image/png", FormatBinary},
	} {
		if got := Negotiate(c.accept, FormatBinary); got != c.want {
			t.Errorf("Negotiate(%q) = %v want %v", c.accept, got, c.want)
		}
	}
	if got := Negotiate("*/*", FormatText); got != FormatText {
		t.Fatal(got)
	}

	in := tOrder{ID: 4, Customer: "Ann Lee", Lines: []tLine{{"a", 1, 1.5}}}
	bin, _ := Marshal(&in)
	txt, _ := MarshalText(&in)
	b64, _ := MarshalBase64(&in)
	form := "application/x-www-form-urlencoded"
	for _, c := range []struct {
		ct   string
		body []byte
		want Format
	}{
		{MediaBinary, bin, FormatBinary}, {MediaText, txt, FormatText}, {MediaBase64, b64, FormatBase64},
		{form, txt, FormatText}, {form, b64, FormatBase64}, {"", txt, FormatText}, {"text/plain", b64, FormatBase64},
		{"application/octet-stream", bin, FormatBinary}, {form, []byte("ID 4; Customer Ann Lee; Lines.SKU a; Lines.Qty 1; Lines.Price 1.5"), FormatText},
	} {
		var out tOrder
		f, err := DecodeAs(c.ct, c.body, &out)
		if err != nil || f != c.want || !reflect.DeepEqual(out, in) {
			t.Errorf("DecodeAs(%q, %.20q): %v %v %+v", c.ct, c.body, f, err, out)
		}
	}
	var n int64
	if f, err := DecodeAs("text/plain", []byte("42"), &n); err != nil || f != FormatText || n != 42 {
		t.Fatalf("bare number: %v %v %d", f, err, n)
	}
	if _, err := DecodeAs("application/xml", []byte("<a/>"), &in); err == nil {
		t.Fatal("xml accepted")
	}
	for _, f := range []Format{FormatBinary, FormatText, FormatBase64} {
		b, err := EncodeAs(nil, f, &in)
		var out tOrder
		if _, err2 := DecodeAs(f.MediaType(), b, &out); err != nil || err2 != nil || !reflect.DeepEqual(out, in) {
			t.Fatalf("%v: %v %v", f, err, err2)
		}
	}
}

func TestTextKey(t *testing.T) {
	for line, want := range map[string]string{`user:1.Name Ada`: "user:1", `"a.b".x 1`: "a.b", `k v`: "k", `  "x y" 1`: "x y"} {
		if got, err := TextKey([]byte(line)); err != nil || got != want {
			t.Errorf("TextKey(%q) = %q, %v", line, got, err)
		}
	}
}

// The core protocol must not depend on any other text format or a transport.
func TestNoForbiddenImports(t *testing.T) {
	for _, dir := range []string{".", "store"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, _ := os.ReadFile(f)
			for _, bad := range []string{`"encoding/json"`, `"net/http"`, `"encoding/xml"`} {
				if strings.Contains(string(b), bad) {
					t.Errorf("%s imports %s", f, bad)
				}
			}
		}
	}
}

func FuzzText(f *testing.F) {
	in := tSample()
	txt, _ := MarshalText(&in)
	f.Add(txt)
	f.Add([]byte("Name x; Tags a b; Items.Street s; Items.+ ~; Items.+; Items.Zip 4"))
	f.Add([]byte("Cube.+; Cube.^.+ a b; Cube.^.+ c; Cube.+; Cube.^.+ d"))
	f.Add([]byte("+ a\n+ b\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		var a tAll
		if UnmarshalText(data, &a) == nil {
			out, err := MarshalText(&a)
			if err != nil {
				t.Fatal(err)
			}
			var b tAll
			if err := UnmarshalText(out, &b); err != nil {
				t.Fatalf("reparse: %v\n%s", err, out)
			}
			out2, _ := MarshalText(&b)
			if string(out) != string(out2) {
				t.Fatalf("unstable:\n%s\n---\n%s", out, out2)
			}
		}
		var x any
		_ = UnmarshalText(data, &x)
		var l []tOrder
		_ = UnmarshalText(data, &l)
	})
}

func benchOrders() []tOrder {
	o := make([]tOrder, 100)
	for i := range o {
		o[i] = tOrder{ID: int64(1000 + i), Customer: "Customer Name", Lines: []tLine{{"sku-001", 2, 9.99}, {"sku-002", 1, 24.5}, {"sku-003", 10, 0.5}}, Tags: []string{"rush", "gift", "web"}, Meta: map[string]string{"source": "web", "region": "eu"}}
	}
	return o
}

func BenchmarkTextEncode(b *testing.B) {
	v := benchOrders()
	buf, _ := AppendText(nil, &v)
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		buf, _ = AppendText(buf[:0], &v)
	}
}

func BenchmarkTextDecode(b *testing.B) {
	v := benchOrders()
	txt, _ := MarshalText(&v)
	var out []tOrder
	b.SetBytes(int64(len(txt)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := UnmarshalText(txt, &out); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBinaryEncodeSameData(b *testing.B) {
	v := benchOrders()
	buf, _ := Append(nil, &v)
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		buf, _ = Append(buf[:0], &v)
	}
}

func BenchmarkBinaryDecodeSameData(b *testing.B) {
	v := benchOrders()
	bin, _ := Marshal(&v)
	var out []tOrder
	b.SetBytes(int64(len(bin)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := Unmarshal(bin, &out); err != nil {
			b.Fatal(err)
		}
	}
}
