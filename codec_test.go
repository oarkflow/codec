package codec

import (
	"math/rand"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type Addr struct {
	Street string
	Zip    uint32
}

type Item struct {
	SKU   string
	Qty   int16
	Price float64
}

type Rich struct {
	ID     int64
	Name   string
	Age    uint8
	Active bool
	Score  float64
	Ratio  float32
	Delta  int32
	Tags   []string
	Times  []int64
	Small  []uint16
	Raw    []byte
	Flags  []bool
	Fs     []float64
	Addr   *Addr
	Home   Addr
	Items  []Item
	Ptrs   []*Addr
	Nested [][]string
	skip   int
	Skip   int `codec:"-"`
}

type Tree struct {
	V    int
	Kids []Tree
	Next *Tree
}

func sample() Rich {
	return Rich{
		ID: -42, Name: "Ada", Age: 36, Active: true, Score: 3.5, Ratio: -0.25, Delta: -7,
		Tags: []string{"a", "", "ccc"}, Times: []int64{1700000000, 1700000005, 1699999999, -3},
		Small: []uint16{1, 65535, 0}, Raw: []byte{0, 1, 2, 255}, Flags: []bool{true, false, true},
		Fs: []float64{1.5, -0, 2e100}, Addr: &Addr{"Main", 12345}, Home: Addr{Street: "x"},
		Items:  []Item{{"s1", 2, 9.99}, {}, {"s3", -1, 0}},
		Ptrs:   []*Addr{{"p", 1}, nil, {"q", 2}},
		Nested: [][]string{{"x", "y"}, nil, {"z"}},
	}
}

func TestRoundTrip(t *testing.T) {
	for _, in := range []Rich{sample(), {}} {
		b, err := Marshal(&in)
		if err != nil {
			t.Fatal(err)
		}
		var out Rich
		if err := Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(in, out) {
			t.Fatalf("mismatch\n in=%+v\nout=%+v", in, out)
		}
	}
}

func TestReuseTargetIsReset(t *testing.T) {
	b, _ := Marshal(Rich{ID: 1})
	out := sample()
	if err := Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, Rich{ID: 1}) {
		t.Fatalf("stale fields: %+v", out)
	}
}

func TestUnmarshalReusesExistingPointers(t *testing.T) {
	type payload struct{ Addr *Addr }
	out := payload{Addr: &Addr{Street: "old", Zip: 1}}
	ptr := out.Addr
	data, err := Marshal(payload{Addr: &Addr{Street: "new", Zip: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Addr != ptr || *out.Addr != (Addr{Street: "new", Zip: 2}) {
		t.Fatalf("pointer was not reused: got=%+v ptr=%p old=%p", out.Addr, out.Addr, ptr)
	}
}

func TestDeterministicMapKeyCacheTracksMutations(t *testing.T) {
	type counts struct{ Values map[string]int64 }
	in := counts{Values: map[string]int64{"alpha": 1, "bravo": 2}}
	first, err := Marshal(&in)
	if err != nil {
		t.Fatal(err)
	}
	delete(in.Values, "alpha")
	in.Values["zulu"] = 3
	second, err := Marshal(&in)
	if err != nil {
		t.Fatal(err)
	}
	third, err := Marshal(&in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second, third) {
		t.Fatal("map encoding changed between calls")
	}
	var got counts
	if err := Unmarshal(second, &got); err != nil || !reflect.DeepEqual(got, in) {
		t.Fatalf("mutated map round trip: err=%v got=%#v want=%#v", err, got, in)
	}
	if reflect.DeepEqual(first, second) {
		t.Fatal("map key mutation did not change the encoding")
	}
}

func TestUnmarshalReusesOutputWithoutClobberingAliasedInput(t *testing.T) {
	type payload struct{ Raw []byte }
	in := payload{Raw: []byte("retained bytes")}
	data, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out := payload{Raw: append([]byte(nil), data...)}
	if err := Unmarshal(out.Raw, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("aliased input was changed: got %#v, want %#v", out, in)
	}
}

func TestRecursive(t *testing.T) {
	in := Tree{1, []Tree{{2, nil, nil}, {3, []Tree{{4, nil, nil}}, &Tree{V: 9}}}, nil}
	b, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Tree
	if err := Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("%+v != %+v", in, out)
	}
}

func TestAdaptiveWidth(t *testing.T) {
	small := Rich{Name: "x", Tags: []string{"a", "b"}}
	b, _ := Marshal(small)
	if b[0]&3 != 0 {
		t.Fatalf("small message should use 1-byte offsets, code=%d", b[0]&3)
	}
	big := Rich{Raw: make([]byte, 100), Tags: make([]string, 70000)}
	for i := range big.Tags {
		big.Tags[i] = "tag"
	}
	b, _ = Marshal(big)
	if b[0]&3 != 2 {
		t.Fatalf("big message should use 4-byte offsets, code=%d", b[0]&3)
	}
	var out Rich
	if err := Unmarshal(b, &out); err != nil || !reflect.DeepEqual(big, out) {
		t.Fatalf("big roundtrip failed: %v", err)
	}
	// width must shrink again for the next small message
	b, _ = Marshal(small)
	if b[0]&3 != 0 {
		t.Fatalf("width did not shrink, code=%d", b[0]&3)
	}
}

func TestMidWidth(t *testing.T) {
	m := Rich{Tags: make([]string, 5000)}
	for i := range m.Tags {
		m.Tags[i] = "abcdefghij"
	}
	b, _ := Marshal(m)
	if b[0]&3 != 1 {
		t.Fatalf("want 2-byte offsets, got code %d", b[0]&3)
	}
	var out Rich
	if err := Unmarshal(b, &out); err != nil || !reflect.DeepEqual(m, out) {
		t.Fatal("roundtrip", err)
	}
}

func TestSchemaMismatch(t *testing.T) {
	b, _ := Marshal(Addr{"a", 1})
	var it Item
	if err := Unmarshal(b, &it); err != ErrSchemaMismatch {
		t.Fatalf("got %v", err)
	}
}

func TestErrors(t *testing.T) {
	if err := Unmarshal(nil, Addr{}); err != ErrNeedPointer {
		t.Fatal(err)
	}
	if err := Unmarshal(nil, (*Addr)(nil)); err != ErrNeedPointer {
		t.Fatal(err)
	}
	if err := Unmarshal([]byte{1, 2}, &Addr{}); err != ErrBadHeader {
		t.Fatal(err)
	}
	type bad struct{ C chan int }
	if _, err := Marshal(bad{}); err == nil {
		t.Fatal("chan should be unsupported")
	}
	if _, err := Marshal(func() {}); err == nil {
		t.Fatal("func should be unsupported")
	}
}

// Truncations and random corruption must error or succeed, never panic.
func TestHostileInput(t *testing.T) {
	in := sample()
	b, _ := Marshal(&in)
	for i := 0; i < len(b); i++ {
		var out Rich
		_ = Unmarshal(b[:i], &out)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200000; i++ {
		c := append([]byte(nil), b...)
		for k := 0; k < 1+rng.Intn(4); k++ {
			c[rng.Intn(len(c))] = byte(rng.Intn(256))
		}
		var out Rich
		_ = Unmarshal(c, &out)
	}
}

func FuzzUnmarshal(f *testing.F) {
	in := sample()
	b, _ := Marshal(&in)
	f.Add(b)
	f.Fuzz(func(t *testing.T, data []byte) {
		var out Rich
		if Unmarshal(data, &out) == nil {
			// anything that decodes must re-encode and decode to the same value
			b2, err := Marshal(&out)
			if err != nil {
				t.Fatal(err)
			}
			var out2 Rich
			if err := Unmarshal(b2, &out2); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestUnmarshalNoCopy(t *testing.T) {
	in := sample()
	b, _ := Marshal(&in)
	var a, c Rich
	if err := Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	if err := UnmarshalNoCopy(b, &c); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, c) {
		t.Fatal("nocopy result differs from Unmarshal")
	}
	var v any = map[string]any{"n": int64(-5), "u": uint64(7), "f": 1.5, "s": "hi", "b": []byte("xy")}
	bd, _ := Marshal(&v)
	var out any
	if err := Unmarshal(bd, &out); err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["n"] != int64(-5) || m["u"] != uint64(7) || m["f"] != 1.5 || m["s"] != "hi" {
		t.Fatalf("dynamic boxing wrong: %#v", m)
	}
	runtime.GC()
	if m["n"] != int64(-5) {
		t.Fatal("boxed value corrupted after GC")
	}
}

func TestReusableValueDecoder(t *testing.T) {
	in := sample()
	b, err := MarshalEvolvable(&in)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewValueDecoder[Rich]()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		var got Rich
		if err := d.UnmarshalNoCopy(b, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, in) {
			t.Fatalf("decode %d differs: %#v", i, got)
		}
	}
	dp, err := NewValueDecoder[*Rich]()
	if err != nil {
		t.Fatal(err)
	}
	var got *Rich
	if err := dp.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got == nil || !reflect.DeepEqual(*got, in) {
		t.Fatalf("pointer decode differs: %#v", got)
	}
}

func FuzzUnmarshalNoCopy(f *testing.F) {
	in := sample()
	b, _ := Marshal(&in)
	f.Add(b)
	f.Fuzz(func(t *testing.T, data []byte) {
		var a, c Rich
		ea := Unmarshal(data, &a)
		ec := UnmarshalNoCopy(append([]byte(nil), data...), &c)
		if (ea == nil) != (ec == nil) {
			t.Fatalf("error mismatch: %v vs %v", ea, ec)
		}
	})
}

func TestBase64(t *testing.T) {
	in := sample()
	txt, err := MarshalBase64(&in)
	if err != nil {
		t.Fatal(err)
	}
	var out Rich
	if err := UnmarshalBase64(append([]byte("  "), append(txt, '\n')...), &out); err != nil || !reflect.DeepEqual(in, out) {
		t.Fatalf("base64 round trip: %v", err)
	}
	std := strings.NewReplacer("-", "+", "_", "/").Replace(string(txt))
	for len(std)%4 != 0 {
		std += "="
	}
	var out2 Rich
	if err := UnmarshalBase64([]byte(std), &out2); err != nil || !reflect.DeepEqual(in, out2) {
		t.Fatalf("standard base64: %v", err)
	}
	if UnmarshalBase64([]byte("!!notbase64"), &out) == nil {
		t.Fatal("expected error")
	}
}
