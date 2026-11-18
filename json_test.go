package codec

import (
	"bytes"
	"io"
	"math/rand"
	"net/netip"
	"reflect"
	"testing"
	"time"
)

// rt marshals in, unmarshals into a new value of the same type, compares.
func rt[T any](t *testing.T, in T) T {
	t.Helper()
	b, err := Marshal(in)
	if err != nil {
		t.Fatalf("%T marshal: %v", in, err)
	}
	var out T
	if err := Unmarshal(b, &out); err != nil {
		t.Fatalf("%T unmarshal: %v (%x)", in, err, b)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("%T mismatch\n in=%#v\nout=%#v", in, in, out)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func TestJSONLikeTopLevel(t *testing.T) {
	rt(t, 42)
	rt(t, -7)
	rt(t, uint8(200))
	rt(t, 3.25)
	rt(t, float32(-1.5))
	rt(t, "héllo")
	rt(t, "")
	rt(t, true)
	rt(t, false)
	rt(t, []byte("raw bytes"))
	rt(t, []int{1, 2, 3, -4})
	rt(t, []string{"a", "", "c"})
	rt(t, []float64{1.5, 2.5})
	rt(t, [][]int{{1, 2}, nil, {3}})
	rt(t, [3]int{7, 8, 9})
	rt(t, [4]string{"a", "b", "", "d"})
	rt(t, [2]Addr{{"x", 1}, {"y", 2}})
	rt(t, [16]byte{1, 2, 3})
	rt(t, []Addr{{"a", 1}, {"b", 2}, {"c", 3}, {"d", 4}, {"e", 5}}) // columnar
	rt(t, []*Addr{{"p", 1}, nil, {"q", 2}})
	rt(t, Addr{"s", 9})
	rt(t, &Addr{"s", 9})
}

func TestJSONLikeMaps(t *testing.T) {
	rt(t, struct {
		Counts map[string]int64
		Labels map[string]string
		Extra  map[string]any
	}{
		Counts: map[string]int64{"": -1, "zero": 0, "large": 1 << 50},
		Labels: map[string]string{"": "", "env": "prod", "region": "eu-west-1"},
		Extra:  map[string]any{"nil": nil, "items": []any{int64(3), "x"}, "nested": map[string]any{"ok": true}},
	})
	rt(t, map[string]int{"a": 1, "b": -2, "zz": 3})
	rt(t, map[string]int64{"": -1, "a": 0, "zz": 1 << 50})
	rt(t, map[string]string{"": "", "a": "one", "zz": "two"})
	rt(t, map[int]string{3: "c", 1: "a", 2: ""})
	rt(t, map[string][]string{"x": {"1", "2"}, "y": nil, "z": {"3"}}) // nil value round trips as nil
	rt(t, map[string]Addr{"home": {"h", 1}, "work": {"w", 2}})
	rt(t, map[[2]int]string{{1, 2}: "a", {3, 4}: "b"})
	rt(t, map[string]map[string]int{"o": {"i": 1, "j": 2}, "p": {"k": 3}})
	rt(t, map[string]*Addr{"a": {"x", 1}, "b": nil})
	rt(t, map[uint16]float64{1: 1.5, 2: 2.5})
	big := map[string]int{}
	for i := 0; i < 5000; i++ {
		big[string(rune('a'+i%26))+string(rune('A'+i/26%26))+string(rune('0'+i/676))] = i
	}
	rt(t, big)
	// deterministic output regardless of Go's map iteration order
	m := map[string]int{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6}
	first, _ := Marshal(m)
	for i := 0; i < 30; i++ {
		if b, _ := Marshal(m); !bytes.Equal(b, first) {
			t.Fatal("map encoding not deterministic")
		}
	}
	// nil and empty maps
	var nm map[string]int
	b, _ := Marshal(nm)
	var out map[string]int
	if err := Unmarshal(b, &out); err != nil || len(out) != 0 {
		t.Fatal(err, out)
	}
}

func TestMapDecodeReusesAndClearsMaps(t *testing.T) {
	type mapSet struct {
		Counts map[string]int64
		Labels map[string]string
		Extra  map[string]any
		Tags   map[string][]int
	}
	want := mapSet{
		Counts: map[string]int64{"new": 7},
		Labels: map[string]string{"region": "west"},
		Extra:  map[string]any{"enabled": true},
		Tags:   map[string][]int{"new": {2, 3}},
	}
	data, err := Marshal(&want)
	if err != nil {
		t.Fatal(err)
	}
	out := mapSet{
		Counts: map[string]int64{"stale": 1, "new": 0},
		Labels: map[string]string{"stale": "old"},
		Extra:  map[string]any{"stale": "old", "enabled": false},
		Tags:   map[string][]int{"stale": {1}, "new": {0}},
	}
	counts, labels, extra, tags := out.Counts, out.Labels, out.Extra, out.Tags
	if err := Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, want) || !reflect.DeepEqual(counts, want.Counts) ||
		!reflect.DeepEqual(labels, want.Labels) || !reflect.DeepEqual(extra, want.Extra) ||
		!reflect.DeepEqual(tags, want.Tags) {
		t.Fatalf("map reuse or clearing failed: got=%#v want=%#v", out, want)
	}
}

func TestJSONLikePointers(t *testing.T) {
	rt(t, ptr(5))
	rt(t, ptr(ptr(6)))
	rt(t, ptr(""))
	rt(t, ptr(0))
	rt(t, ptr([]int{1, 2}))
	rt(t, []*string{ptr("a"), nil, ptr("")}) // nil vs pointer-to-empty must differ
	rt(t, []*int{ptr(0), nil, ptr(5)})
	type P struct {
		A *int
		B *string
		C **float64
		D *[]int
		E *map[string]int
		F *Addr
		G *bool
	}
	rt(t, P{})
	rt(t, P{A: ptr(0), B: ptr(""), C: ptr(ptr(1.5)), D: &[]int{1}, E: &map[string]int{"k": 1}, F: &Addr{"z", 1}, G: ptr(false)})
	// Marshal(x) and Marshal(&x) agree, and nil pointer encodes as the zero value
	x := Addr{"a", 1}
	b1, _ := Marshal(x)
	b2, _ := Marshal(&x)
	if !bytes.Equal(b1, b2) {
		t.Fatal("value and pointer encodings differ")
	}
	var np *Addr
	b3, err := Marshal(np)
	if err != nil {
		t.Fatal(err)
	}
	var z Addr
	if err := Unmarshal(b3, &z); err != nil || z != (Addr{}) {
		t.Fatal(err, z)
	}
}

type Mixed struct {
	Name  string
	Any   any
	List  []any
	Obj   map[string]any
	When  time.Time
	Dur   time.Duration
	IP    netip.Addr
	Tags  map[string][]int
	Inner *Mixed
}

func TestJSONLikeInterface(t *testing.T) {
	// interface values decode to generic types, like encoding/json
	in := map[string]any{
		"s": "str", "i": int64(-5), "u": uint64(7), "f": 2.5, "b": true, "n": nil,
		"l": []any{int64(1), "two", 3.5, nil, map[string]any{"k": "v"}}, "by": []byte{1, 2},
	}
	b, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := Unmarshal(b, &out); err != nil || !reflect.DeepEqual(in, out) {
		t.Fatalf("err=%v\n in=%#v\nout=%#v", err, in, out)
	}
	// concrete values inside interface{} come back as generic types. To decode
	// into an interface{}, marshal from one (a *any, or a field of type any).
	var got any
	src := any([]any{1, "a", 2.5, true, nil, []string{"x"}, map[string]int{"k": 1}, Addr{"st", 3}, uint8(4), float32(1.5)})
	b, _ = Marshal(&src)
	if err := Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := []any{int64(1), "a", 2.5, true, nil, []any{"x"}, map[string]any{"k": int64(1)}, map[string]any{"Street": "st", "Zip": uint64(3)}, uint64(4), 1.5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
	// top-level nil
	b, _ = Marshal(nil)
	got = 1
	if err := Unmarshal(b, &got); err != nil || got != nil {
		t.Fatal(err, got)
	}
	// non-string map keys inside interface values
	src = map[int]string{1: "a", 2: "b"}
	b, _ = Marshal(&src)
	if err := Unmarshal(b, &got); err != nil || !reflect.DeepEqual(got, map[any]any{int64(1): "a", int64(2): "b"}) {
		t.Fatal(err, got)
	}
}

func TestDynamicBytesAreOwnedAndBudgeted(t *testing.T) {
	src := any([]byte{1, 2, 3, 4})
	data, err := Marshal(&src)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err := Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	decoded, ok := got.([]byte)
	if !ok || !reflect.DeepEqual(decoded, []byte{1, 2, 3, 4}) {
		t.Fatalf("decoded %#v", got)
	}
	decoded[0] = 9
	var again any
	if err := Unmarshal(data, &again); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, []byte{1, 2, 3, 4}) {
		t.Fatalf("decoded bytes alias input: %#v", again)
	}

	oldBudget := MaxAlloc
	MaxAlloc = 3
	defer func() { MaxAlloc = oldBudget }()
	got = nil
	if err := Unmarshal(data, &got); err != ErrTooLarge {
		t.Fatalf("expected allocation budget error, got %v", err)
	}
}

func TestJSONLikeTimeAndMarshalers(t *testing.T) {
	now := time.Date(2026, 11, 18, 8, 30, 15, 123456789, time.UTC)
	rt(t, now)
	rt(t, []time.Time{now, now.Add(time.Hour)})
	rt(t, map[string]time.Time{"a": now})
	z := time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("x", 5*3600+45*60))
	zb, _ := Marshal(z)
	var zz time.Time
	if err := Unmarshal(zb, &zz); err != nil || !zz.Equal(z) {
		t.Fatal("time zone offset lost", err)
	}
	if _, off := zz.Zone(); off != 5*3600+45*60 {
		t.Fatalf("offset %d", off)
	}
	ip := netip.MustParseAddr("2001:db8::1")
	rt(t, ip)
	m := Mixed{Name: "m", When: now, Dur: 90 * time.Second, IP: ip, Tags: map[string][]int{"a": {1, 2}}, Any: "x"}
	m.Inner = &Mixed{Name: "inner", Any: int64(5), List: []any{"a", int64(1)}, Obj: map[string]any{"k": []any{true}}}
	b, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Mixed
	if err := Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, back) {
		t.Fatalf("\n in=%+v\nout=%+v", m, back)
	}
	// a time.Time inside interface{} becomes an RFC 3339 string, as in encoding/json
	b, _ = Marshal(map[string]any{"t": now})
	var g map[string]any
	if err := Unmarshal(b, &g); err != nil || g["t"] != "2026-11-18T08:30:15.123456789Z" {
		t.Fatal(err, g)
	}
}

func TestJSONLikeStream(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	vals := []any{42, "hello", []int{1, 2, 3}, map[string]float64{"pi": 3.14}, Addr{"s", 1}, &Mixed{Name: "x"}}
	for _, v := range vals {
		if err := enc.Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	dec := NewDecoder(&buf)
	var i int
	var s string
	var l []int
	var m map[string]float64
	var a Addr
	var mx Mixed
	for _, p := range []any{&i, &s, &l, &m, &a, &mx} {
		if !dec.More() {
			t.Fatal("stream ended early")
		}
		if err := dec.Decode(p); err != nil {
			t.Fatal(err)
		}
	}
	if i != 42 || s != "hello" || !reflect.DeepEqual(l, []int{1, 2, 3}) || m["pi"] != 3.14 || a.Street != "s" || mx.Name != "x" {
		t.Fatal("stream values wrong")
	}
	if dec.More() {
		t.Fatal("More after end")
	}
	if err := dec.Decode(&i); err != io.EOF {
		t.Fatalf("want io.EOF, got %v", err)
	}
	// truncated stream
	var b2 bytes.Buffer
	NewEncoder(&b2).Encode("some value")
	if err := NewDecoder(bytes.NewReader(b2.Bytes()[:b2.Len()-2])).Decode(&s); err != io.ErrUnexpectedEOF {
		t.Fatalf("want ErrUnexpectedEOF, got %v", err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestEncoderShortWrite(t *testing.T) {
	enc := NewEncoder(writerFunc(func(p []byte) (int, error) { return len(p) - 1, nil }))
	if err := enc.Encode(1); err != io.ErrShortWrite {
		t.Fatalf("want io.ErrShortWrite, got %v", err)
	}
}

func TestJSONLikeWrongType(t *testing.T) {
	b, _ := Marshal([]int{1, 2})
	var s []string
	if err := Unmarshal(b, &s); err != ErrSchemaMismatch {
		t.Fatalf("got %v", err)
	}
	var m map[string]int
	if err := Unmarshal(b, &m); err != ErrSchemaMismatch {
		t.Fatalf("got %v", err)
	}
}

func TestJSONLikeHostile(t *testing.T) {
	m := Mixed{Name: "m", Any: map[string]any{"a": []any{int64(1), "x"}}, Tags: map[string][]int{"a": {1, 2, 3}},
		When: time.Now().UTC(), Inner: &Mixed{Name: "i"}, Obj: map[string]any{"k": "v"}}
	b, _ := Marshal(m)
	rng := rand.New(rand.NewSource(5))
	for i := 0; i < len(b); i++ {
		var out Mixed
		_ = Unmarshal(b[:i], &out)
	}
	for i := 0; i < 200000; i++ {
		c := append([]byte(nil), b...)
		for k := 0; k < 1+rng.Intn(4); k++ {
			c[rng.Intn(len(c))] = byte(rng.Intn(256))
		}
		var out Mixed
		_ = Unmarshal(c, &out)
		var any1 any
		_ = Unmarshal(c, &any1)
	}
}

func FuzzMixed(f *testing.F) {
	b, _ := Marshal(Mixed{Name: "m", Any: []any{int64(1)}, Tags: map[string][]int{"a": {1}}, Inner: &Mixed{}})
	f.Add(b)
	f.Fuzz(func(t *testing.T, data []byte) {
		var out Mixed
		if Unmarshal(data, &out) == nil {
			for _, marshal := range []func(any) ([]byte, error){Marshal} {
				if _, err := marshal(&out); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
}
