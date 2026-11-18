package codec

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// ---- version 1 of a schema ----

type v1Addr struct {
	Street string
	Zip    uint32
}

type v1Item struct {
	SKU string
	Qty int32
}

type v1Doc struct {
	ID    int64
	Name  string
	Tags  []string
	Addr  v1Addr
	Items []v1Item          // columnar when 4 or more
	PItem *v1Item           // pointer to struct
	ByKey map[string]v1Item // struct map values
	Pair  [2]v1Item         // array of structs
	Extra any
}

// ---- version 2: fields appended at every level, plus new nested types ----

type v2Geo struct {
	Lat, Lon float64
}

type v2Addr struct {
	Street  string
	Zip     uint32
	Country string // new
	Geo     v2Geo  // new, new struct type
}

type v2Item struct {
	SKU   string
	Qty   int32
	Price float64  // new
	Notes []string // new
}

type v2Doc struct {
	ID      int64
	Name    string
	Tags    []string
	Addr    v2Addr
	Items   []v2Item
	PItem   *v2Item
	ByKey   map[string]v2Item
	Pair    [2]v2Item
	Extra   any
	Email   string            // new
	Labels  map[string]string // new
	History []v2Item          // new
	Owner   *v2Addr           // new
}

func v1Sample(items int) v1Doc {
	d := v1Doc{
		ID: 42, Name: "ada", Tags: []string{"x", "y"}, Addr: v1Addr{"12 Main", 12345},
		PItem: &v1Item{"p", 9}, ByKey: map[string]v1Item{"k": {"kk", 1}, "j": {"jj", 2}},
		Pair: [2]v1Item{{"a", 1}, {"b", 2}}, Extra: map[string]any{"n": int64(3)},
	}
	for i := 0; i < items; i++ {
		d.Items = append(d.Items, v1Item{fmt.Sprint("sku-", i), int32(i + 1)})
	}
	return d
}

func checkV2FromV1(t *testing.T, got v2Doc, want v1Doc) {
	t.Helper()
	if got.ID != want.ID || got.Name != want.Name || !reflect.DeepEqual(got.Tags, want.Tags) ||
		got.Addr.Street != want.Addr.Street || got.Addr.Zip != want.Addr.Zip || !reflect.DeepEqual(got.Extra, want.Extra) {
		t.Fatalf("old fields lost: %+v", got)
	}
	if len(got.Items) != len(want.Items) {
		t.Fatalf("items %d vs %d", len(got.Items), len(want.Items))
	}
	for i, it := range want.Items {
		g := got.Items[i]
		if g.SKU != it.SKU || g.Qty != it.Qty || g.Price != 0 || g.Notes != nil {
			t.Fatalf("item %d: %+v", i, g)
		}
	}
	if (want.PItem == nil) != (got.PItem == nil) || want.PItem != nil && (got.PItem.SKU != "p" || got.PItem.Qty != 9 || got.PItem.Price != 0) {
		t.Fatalf("pointer: %+v", got.PItem)
	}
	if len(got.ByKey) != len(want.ByKey) || got.ByKey["k"].SKU != "kk" || got.ByKey["j"].Qty != 2 || got.ByKey["k"].Price != 0 {
		t.Fatalf("map: %+v", got.ByKey)
	}
	if got.Pair[0].SKU != "a" || got.Pair[1].Qty != 2 || got.Pair[1].Notes != nil {
		t.Fatalf("array: %+v", got.Pair)
	}
	// everything new reads as zero
	if got.Email != "" || got.Labels != nil || got.History != nil || got.Owner != nil || got.Addr.Country != "" || got.Addr.Geo != (v2Geo{}) {
		t.Fatalf("new fields not zero: %+v", got)
	}
}

func TestEvolveOldDataNewType(t *testing.T) {
	for _, n := range []int{0, 1, 3, 4, 50} { // 4+ elements use the columnar layout
		old := v1Sample(n)
		data, err := MarshalEvolvable(&old)
		if err != nil {
			t.Fatal(err)
		}
		var got v2Doc
		if err := Unmarshal(data, &got); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		checkV2FromV1(t, got, old)
	}
}

// Decoding into a variable that already holds data must clear the new fields,
// including inside reused slice storage.
func TestEvolveZeroesStaleDestination(t *testing.T) {
	old := v1Sample(10)
	data, _ := MarshalEvolvable(&old)
	dirtyItems := make([]v2Item, 10)
	for i := range dirtyItems {
		dirtyItems[i] = v2Item{"zz", 99, 1.5, []string{"stale"}}
	}
	got := v2Doc{
		Email: "stale@x", Labels: map[string]string{"a": "b"}, History: dirtyItems, Owner: &v2Addr{Country: "stale"},
		Items: dirtyItems, PItem: &v2Item{Price: 7, Notes: []string{"s"}}, Addr: v2Addr{Country: "stale", Geo: v2Geo{1, 2}},
		ByKey: map[string]v2Item{"k": {Price: 5}}, Pair: [2]v2Item{{Price: 3}, {Notes: []string{"q"}}},
	}
	if err := Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	checkV2FromV1(t, got, old)
	// and decoding twice into the same variable stays correct
	if err := Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	checkV2FromV1(t, got, old)
}

func TestEvolveSameVersionAndRoundTrip(t *testing.T) {
	d := v2Doc{ID: 1, Name: "n", Email: "e", Labels: map[string]string{"a": "b"}, Owner: &v2Addr{"s", 1, "NP", v2Geo{1.5, 2.5}},
		Items: []v2Item{{"a", 1, 2.5, []string{"n"}}}, History: []v2Item{{"h", 2, 0, nil}}}
	ev, err := MarshalEvolvable(&d)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := Marshal(&d)
	if len(ev) <= len(plain) {
		t.Fatalf("evolvable should carry a shape: %d vs %d", len(ev), len(plain))
	}
	var a, b v2Doc
	if err := Unmarshal(ev, &a); err != nil || Unmarshal(plain, &b) != nil || !reflect.DeepEqual(a, b) || !reflect.DeepEqual(a.Owner, d.Owner) {
		t.Fatalf("round trip differs: %v\n%+v\n%+v", err, a, b)
	}
	// the body is byte-identical: the shape only adds to the header
	_, evBody, _ := openMessage(ev, reflect.TypeOf(d), mustPlan(t, d))
	_, plBody, _ := openMessage(plain, reflect.TypeOf(d), mustPlan(t, d))
	if !bytes.Equal(evBody, plBody) {
		t.Fatal("evolvable body differs from plain body")
	}
}

func mustPlan(t *testing.T, v any) *plan {
	pl, err := planFor(reflect.TypeOf(v))
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

// Appending fields that cross a bitmap byte boundary changes the frame layout
// (the bitmap grows), which the reader has to follow.
func TestEvolveBitmapBoundary(t *testing.T) {
	type S7 struct {
		A, B, C, D, E, F, G int32
	}
	type S9 struct {
		A, B, C, D, E, F, G int32
		H, I                int32
	}
	type S8 struct {
		A, B, C, D, E, F, G, H int32
	}
	type S17 struct {
		A, B, C, D, E, F, G, H int32
		I, J, K, L, M, N, O, P int32
		Q                      string
	}
	old := S7{1, 2, 3, 0, 5, 6, 7}
	data, _ := MarshalEvolvable(&old)
	for _, tc := range []any{new(S8), new(S9), new(S17)} {
		if err := Unmarshal(data, tc); err != nil {
			t.Fatalf("%T: %v", tc, err)
		}
	}
	var s9 S9
	Unmarshal(data, &s9)
	if s9 != (S9{1, 2, 3, 0, 5, 6, 7, 0, 0}) {
		t.Fatalf("%+v", s9)
	}
	eight := S8{1, 2, 3, 4, 5, 6, 7, 8}
	d8, _ := MarshalEvolvable(&eight)
	var s17 S17
	if err := Unmarshal(d8, &s17); err != nil || s17.H != 8 || s17.I != 0 || s17.Q != "" {
		t.Fatalf("%v %+v", err, s17)
	}
}

// A new nested struct type appended to an early struct shifts where later
// struct types fall in the schema; the shape must still line up.
func TestEvolveShiftedStructOrder(t *testing.T) {
	type Tail1 struct{ X int32 }
	type Head1 struct{ A string }
	type Doc1 struct {
		H Head1
		T Tail1
	}
	type Added struct{ P, Q string }
	type Head2 struct {
		A string
		N Added // brand new struct type, discovered before Tail2
	}
	type Tail2 struct {
		X int32
		Y string
	}
	type Doc2 struct {
		H Head2
		T Tail2
	}
	data, _ := MarshalEvolvable(&Doc1{Head1{"a"}, Tail1{7}})
	var got Doc2
	if err := Unmarshal(data, &got); err != nil || got.H.A != "a" || got.T.X != 7 || got.H.N != (Added{}) || got.T.Y != "" {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestEvolveRootKinds(t *testing.T) {
	old := []v1Item{{"a", 1}, {"b", 2}, {"c", 3}, {"d", 4}, {"e", 5}}
	data, err := MarshalEvolvable(&old)
	if err != nil {
		t.Fatal(err)
	}
	var got []v2Item
	if err := Unmarshal(data, &got); err != nil || len(got) != 5 || got[4].SKU != "e" || got[4].Price != 0 {
		t.Fatalf("%v %+v", err, got)
	}
	// non-struct roots with no structs inside
	ints := []int{1, -2, 3}
	d2, _ := MarshalEvolvable(&ints)
	var back []int
	if err := Unmarshal(d2, &back); err != nil || !reflect.DeepEqual(back, ints) {
		t.Fatalf("%v %v", err, back)
	}
	m := map[string]v1Addr{"a": {"s", 1}}
	d3, _ := MarshalEvolvable(m)
	var mm map[string]v2Addr
	if err := Unmarshal(d3, &mm); err != nil || mm["a"].Street != "s" || mm["a"].Country != "" {
		t.Fatalf("%v %+v", err, mm)
	}
}

func TestEvolveRejectsWhatIsNotAppendOnly(t *testing.T) {
	type Base struct {
		A string
		B int32
		C []string
	}
	data, _ := MarshalEvolvable(&Base{"a", 2, []string{"c"}})
	cases := []struct {
		name string
		dst  any
	}{
		{"removed field", &struct {
			A string
			B int32
		}{}},
		{"reordered", &struct {
			B int32
			A string
			C []string
		}{}},
		{"renamed", &struct {
			A string
			Z int32
			C []string
		}{}},
		{"retyped", &struct {
			A string
			B int64
			C []string
		}{}},
		{"inserted in the middle", &struct {
			A string
			N int32
			B int32
			C []string
		}{}},
	}
	for _, c := range cases {
		err := Unmarshal(data, c.dst)
		if !errors.Is(err, ErrSchemaMismatch) {
			t.Errorf("%s: got %v, want ErrSchemaMismatch", c.name, err)
		}
	}
	// a newer message read by an older type says so
	type Newer struct {
		A string
		B int32
		C []string
		D bool
	}
	nd, _ := MarshalEvolvable(&Newer{"a", 1, nil, true})
	var old Base
	err := Unmarshal(nd, &old)
	if !errors.Is(err, ErrSchemaMismatch) || !strings.Contains(err.Error(), "newer schema") {
		t.Fatalf("newer message: %v", err)
	}
	// plain messages stay strict
	pd, _ := Marshal(&Base{"a", 1, nil})
	var nw Newer
	if err := Unmarshal(pd, &nw); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("plain message accepted by a different type: %v", err)
	}
}

func TestEvolveView(t *testing.T) {
	old := v1Sample(6)
	data, _ := MarshalEvolvable(&old)
	v, err := View(data, &v2Doc{})
	if err != nil {
		t.Fatal(err)
	}
	if f, err := v.Field("Name"); err != nil || f.String() != "ada" {
		t.Fatalf("Name: %v %q", err, f.String())
	}
	if f, err := v.Field("Email"); err != nil || f.String() != "" || f.Present() {
		t.Fatalf("Email (new field): %v %q", err, f.String())
	}
	if f, err := v.Field("History"); err != nil || f.Len() != 0 {
		t.Fatalf("History (new field): %v", err)
	}
	items, _ := v.Field("Items")
	it, err := items.Index(2)
	if err != nil {
		t.Fatal(err)
	}
	if sku, _ := it.Field("SKU"); sku.String() != "sku-2" {
		t.Fatalf("columnar item: %q", sku.String())
	}
	if p, err := it.Field("Price"); err != nil || p.Float() != 0 {
		t.Fatalf("item Price (new field): %v", err)
	}
	if _, err := v.Field("NoSuchField"); !errors.Is(err, ErrNoField) {
		t.Fatal(err)
	}
}

func TestEvolveStream(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	enc.Evolvable = true
	for i := 0; i < 3; i++ {
		d := v1Sample(i + 1)
		if err := enc.Encode(&d); err != nil {
			t.Fatal(err)
		}
	}
	dec := NewDecoder(&buf)
	for i := 0; i < 3; i++ {
		var got v2Doc
		if err := dec.Decode(&got); err != nil {
			t.Fatal(err)
		}
		checkV2FromV1(t, got, v1Sample(i+1))
	}
}

func TestEvolveConcurrent(t *testing.T) {
	old := v1Sample(20)
	data, _ := MarshalEvolvable(&old)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got v2Doc
			for i := 0; i < 200; i++ {
				if err := Unmarshal(data, &got); err != nil || len(got.Items) != 20 {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestEvolveHostileHeaders(t *testing.T) {
	good, _ := MarshalEvolvable(&v1Doc{ID: 1})
	var dst v2Doc
	// truncated and corrupted shapes never panic
	for n := 0; n <= 12 && n < len(good); n++ {
		_ = Unmarshal(good[:n], &dst)
	}
	hdr := func(shape ...byte) []byte {
		b := []byte{0xC4, 0, 0, 0, 0}
		return append(b, shape...)
	}
	for name, b := range map[string][]byte{
		"empty shape":    hdr(),
		"count too big":  hdr(0xFF, 0xFF, 0xFF, 0x7F),
		"too many":       hdr(200, 1, 1),
		"huge field cnt": hdr(1, 0xFF, 0xFF, 0xFF, 0x7F),
		"more than type": hdr(1, 200),
		"fewer structs":  hdr(0),
	} {
		if err := Unmarshal(b, &dst); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// random bytes after a valid evolvable header
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		b := append([]byte(nil), good...)
		for k := 0; k < 3; k++ {
			b[r.Intn(len(b))] = byte(r.Intn(256))
		}
		_ = Unmarshal(b, &dst)
		_, _ = View(b, &dst)
	}
}

func FuzzEvolve(f *testing.F) {
	for _, n := range []int{0, 2, 9} {
		d := v1Sample(n)
		b, _ := MarshalEvolvable(&d)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var a v2Doc
		var b v1Doc
		_ = Unmarshal(data, &a)
		_ = Unmarshal(data, &b)
		_, _ = View(data, &a)
	})
}

func BenchmarkEvolveDecode(b *testing.B) {
	cur := v2Doc{ID: 1, Name: "ada", Tags: []string{"x", "y"}, Addr: v2Addr{"12 Main", 1, "NP", v2Geo{1, 2}}, Email: "e@x",
		Items: []v2Item{{"a", 1, 2.5, nil}, {"b", 2, 3.5, nil}, {"c", 3, 4.5, nil}, {"d", 4, 5.5, nil}}}
	old := v1Sample(4)
	plain, _ := Marshal(&cur)
	same, _ := MarshalEvolvable(&cur)
	evolved, _ := MarshalEvolvable(&old)
	for _, c := range []struct {
		name string
		data []byte
	}{{"plain", plain}, {"evolvable-same-version", same}, {"evolvable-from-older-version", evolved}} {
		b.Run(c.name, func(b *testing.B) {
			var out v2Doc
			b.SetBytes(int64(len(c.data)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := Unmarshal(c.data, &out); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	// the same old document read by its own type, for a like-for-like baseline
	oldPlain, _ := Marshal(&old)
	b.Run("baseline-older-version-plain-into-its-own-type", func(b *testing.B) {
		var out v1Doc
		b.SetBytes(int64(len(oldPlain)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := Unmarshal(oldPlain, &out); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestEvolveSizes(t *testing.T) {
	cur := v2Doc{ID: 1, Name: "ada", Tags: []string{"x", "y"}, Addr: v2Addr{"12 Main", 1, "NP", v2Geo{1, 2}}, Email: "e@x"}
	plain, _ := Marshal(&cur)
	ev, _ := MarshalEvolvable(&cur)
	t.Logf("plain %d B, evolvable %d B (+%d: one flag-free header plus one byte per struct type)", len(plain), len(ev), len(ev)-len(plain))
}

func BenchmarkEvolveEncode(b *testing.B) {
	d := v2Doc{ID: 1, Name: "ada", Tags: []string{"x", "y"}, Addr: v2Addr{"12 Main", 1, "NP", v2Geo{1, 2}}, Email: "e@x"}
	buf := make([]byte, 0, 1024)
	b.Run("plain", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf, _ = Append(buf[:0], &d)
		}
	})
	b.Run("evolvable", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf, _ = AppendEvolvable(buf[:0], &d)
		}
	})
}
