package codec

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

func mustField(t testing.TB, v Value, name string) Value {
	t.Helper()
	f, err := v.Field(name)
	if err != nil {
		t.Fatalf("Field(%s): %v", name, err)
	}
	return f
}

func TestViewRich(t *testing.T) {
	in := sample()
	b, _ := Marshal(&in)
	v, err := View(b, &Rich{})
	if err != nil {
		t.Fatal(err)
	}
	if mustField(t, v, "ID").Int() != in.ID || mustField(t, v, "Name").String() != in.Name ||
		mustField(t, v, "Age").Uint() != uint64(in.Age) || !mustField(t, v, "Active").Bool() ||
		mustField(t, v, "Score").Float() != in.Score || mustField(t, v, "Ratio").Float() != float64(in.Ratio) ||
		mustField(t, v, "Delta").Int() != int64(in.Delta) {
		t.Fatal("scalar mismatch")
	}
	tags := mustField(t, v, "Tags")
	if tags.Len() != len(in.Tags) {
		t.Fatal("tags len")
	}
	for i, want := range in.Tags {
		e, err := tags.Index(i)
		if err != nil || e.String() != want {
			t.Fatalf("tag %d: %q %v", i, e.String(), err)
		}
	}
	times := mustField(t, v, "Times")
	for i, want := range in.Times {
		e, err := times.Index(i)
		if err != nil || e.Int() != want {
			t.Fatalf("time %d", i)
		}
	}
	fs := mustField(t, v, "Fs")
	for i, want := range in.Fs {
		e, _ := fs.Index(i)
		if e.Float() != want {
			t.Fatalf("fs %d", i)
		}
	}
	if string(mustField(t, v, "Raw").Bytes()) != string(in.Raw) {
		t.Fatal("raw")
	}
	addr := mustField(t, v, "Addr")
	if s := mustField(t, addr, "Street").String(); s != "Main" || mustField(t, addr, "Zip").Uint() != 12345 {
		t.Fatal("addr", s)
	}
	items := mustField(t, v, "Items")
	e, _ := items.Index(2)
	if mustField(t, e, "SKU").String() != "s3" || mustField(t, e, "Qty").Int() != -1 {
		t.Fatal("item")
	}
	ptrs := mustField(t, v, "Ptrs")
	if p, _ := ptrs.Index(1); p.Present() {
		t.Fatal("nil ptr element reported present")
	}
	if _, err := v.Field("Nope"); err != ErrNoField {
		t.Fatal(err)
	}
	if _, err := tags.Index(99); err != ErrIndex {
		t.Fatal(err)
	}
	// absent fields read as zero
	z, _ := Marshal(&Rich{ID: 5})
	zv, _ := View(z, &Rich{})
	if f := mustField(t, zv, "Name"); f.Present() || f.String() != "" || f.Len() != 0 {
		t.Fatal("absent string")
	}
}

func TestViewColumnar(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	for _, rows := range [][]Row{mkRows(300, nil), mkRows(300, rng), mkRows(5, nil)} {
		in := Table{"tbl", rows}
		b, _ := Marshal(&in)
		v, err := View(b, &Table{})
		if err != nil {
			t.Fatal(err)
		}
		rs := mustField(t, v, "Rows")
		if rs.Len() != len(rows) {
			t.Fatalf("len %d != %d", rs.Len(), len(rows))
		}
		for i, r := range rows {
			e, err := rs.Index(i)
			if err != nil {
				t.Fatal(err)
			}
			ok := mustField(t, e, "TS").Int() == r.TS &&
				mustField(t, e, "Host").String() == r.Host &&
				mustField(t, e, "Val").Float() == r.Val &&
				mustField(t, e, "F32").Float() == float64(r.F32) &&
				mustField(t, e, "Status").Uint() == uint64(r.Status) &&
				mustField(t, e, "U64").Uint() == r.U64 &&
				mustField(t, e, "I8").Int() == int64(r.I8) &&
				mustField(t, e, "OK").Bool() == r.OK &&
				mustField(t, e, "Note").String() == r.Note
			if !ok {
				t.Fatalf("row %d mismatch: %+v", i, r)
			}
			sub := mustField(t, e, "Sub")
			if mustField(t, sub, "Zip").Uint() != uint64(r.Sub.Zip) || mustField(t, sub, "Street").String() != r.Sub.Street {
				t.Fatalf("row %d sub", i)
			}
			ref := mustField(t, e, "Ref")
			if ref.Present() != (r.Ref != nil) {
				t.Fatalf("row %d ref presence", i)
			}
			if r.Ref != nil && mustField(t, ref, "Zip").Uint() != uint64(r.Ref.Zip) {
				t.Fatalf("row %d ref", i)
			}
			if mustField(t, e, "Tags").Len() != len(r.Tags) {
				t.Fatalf("row %d tags", i)
			}
		}
	}
}

func TestViewHostile(t *testing.T) {
	b, _ := Marshal(&Table{"t", mkRows(40, nil)})
	rng := rand.New(rand.NewSource(9))
	touch := func(data []byte) {
		v, err := View(data, &Table{})
		if err != nil {
			return
		}
		rs, err := v.Field("Rows")
		if err != nil {
			return
		}
		for i := 0; i < min(rs.Len(), 50); i++ {
			e, err := rs.Index(i)
			if err != nil {
				continue
			}
			for _, f := range []string{"TS", "Host", "Val", "Status", "OK", "Note", "Sub", "Ref", "Tags"} {
				x, err := e.Field(f)
				if err == nil {
					_ = x.String()
					_ = x.Int()
					_ = x.Len()
				}
			}
		}
	}
	for i := 0; i < len(b); i++ {
		touch(b[:i])
	}
	for i := 0; i < 100000; i++ {
		c := append([]byte(nil), b...)
		for k := 0; k < 1+rng.Intn(4); k++ {
			c[rng.Intn(len(c))] = byte(rng.Intn(256))
		}
		touch(c)
	}
}

func TestViewDoesNotAllocate(t *testing.T) {
	in := Table{"t", mkRows(200, nil)}
	b, _ := Marshal(&in)
	v, _ := View(b, &Table{})
	a := testing.AllocsPerRun(50, func() {
		rs, _ := v.Field("Rows")
		e, _ := rs.Index(150)
		f, _ := e.Field("Host")
		_ = f.StringRef()
		g, _ := e.Field("TS")
		_ = g.Int()
	})
	if a != 0 {
		t.Fatalf("view allocated %v times", a)
	}
}

func TestAllocLimit(t *testing.T) {
	old := MaxAlloc
	defer func() { MaxAlloc = old }()
	big, _ := Marshal(&Table{"t", mkRows(1000, nil)})
	MaxAlloc = 1000
	var tb Table
	if err := Unmarshal(big, &tb); err != ErrTooLarge {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestViewStringsAreCopiesUnlessRef(t *testing.T) {
	in := Rich{Name: "Ada"}
	b, _ := Marshal(&in)
	v, _ := View(b, &Rich{})
	f := mustField(t, v, "Name")
	cp, ref := f.String(), f.StringRef()
	for i := range b {
		if string(b[i:min(i+3, len(b))]) == "Ada" {
			b[i] = 'X'
		}
	}
	if cp != "Ada" {
		t.Fatalf("String() must be a copy, got %q", cp)
	}
	if ref != "Xda" {
		t.Fatalf("StringRef() should alias, got %q", ref)
	}
}

type TS struct {
	T int64
	U uint32
}
type TSTable struct{ R []TS }

func TestDeltaLaneRandomAccess(t *testing.T) {
	rng := rand.New(rand.NewSource(77))
	for _, n := range []int{5, 64, 65, 66, 129, 130, 1000, 4097} {
		for _, mode := range []string{"steady", "jitter", "wild"} {
			in := TSTable{make([]TS, n)}
			cur := int64(1700000000000)
			for i := range in.R {
				switch mode {
				case "steady":
					cur += 1000
				case "jitter":
					cur += 1000 + int64(rng.Intn(50))
				default:
					cur += int64(rng.Intn(1 << 20))
				}
				in.R[i] = TS{cur, uint32(rng.Intn(1000))}
			}
			b, _ := Marshal(&in)
			var back TSTable
			if err := Unmarshal(b, &back); err != nil || !reflect.DeepEqual(in, back) {
				t.Fatalf("n=%d %s roundtrip: %v", n, mode, err)
			}
			v, _ := View(b, &TSTable{})
			rs := mustField(t, v, "R")
			for i := 0; i < n; i++ {
				e, err := rs.Index(i)
				if err != nil {
					t.Fatal(err)
				}
				if got := mustField(t, e, "T").Int(); got != in.R[i].T {
					t.Fatalf("n=%d %s i=%d: %d != %d", n, mode, i, got, in.R[i].T)
				}
			}
			c, _ := rs.Column("T")
			all, _ := c.Ints(nil)
			for i := range all {
				if all[i] != in.R[i].T {
					t.Fatalf("bulk n=%d %s i=%d", n, mode, i)
				}
			}
		}
	}
}

func BenchmarkRandomAccess(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	in := TSTable{make([]TS, 100000)}
	cur := int64(1700000000000)
	for i := range in.R {
		cur += 1000 + int64(rng.Intn(50))
		in.R[i] = TS{cur, uint32(i)}
	}
	data, _ := Marshal(&in)
	v, _ := View(data, &TSTable{})
	rs := mustField(b, v, "R")
	for _, idx := range []int{10, 50000, 99999} {
		b.Run(fmt.Sprintf("index=%d", idx), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				e, _ := rs.Index(idx)
				f, _ := e.Field("T")
				_ = f.Int()
			}
		})
	}
}
