package codec

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
)

func TestColumnBulk(t *testing.T) {
	rng := rand.New(rand.NewSource(21))
	for _, rows := range [][]Row{mkRows(500, nil), mkRows(500, rng), mkRows(3, nil), mkRows(9, nil), nil, make([]Row, 40)} {
		b, _ := Marshal(&Table{"t", rows})
		v, err := View(b, &Table{})
		if err != nil {
			t.Fatal(err)
		}
		rs := mustField(t, v, "Rows")
		col := func(name string) Column {
			c, err := rs.Column(name)
			if err != nil {
				t.Fatalf("Column(%s): %v", name, err)
			}
			if c.Len() != len(rows) {
				t.Fatalf("%s len %d != %d", name, c.Len(), len(rows))
			}
			return c
		}
		var wTS, gTS []int64
		var wSt, gSt []uint64
		var wVal, gVal, wF32, gF32 []float64
		var wOK, gOK []bool
		var wHost, gHost []string
		var wI8, gI8 []int64
		var wU64, gU64 []uint64
		for _, r := range rows {
			wTS, wSt = append(wTS, r.TS), append(wSt, uint64(r.Status))
			wVal, wF32 = append(wVal, r.Val), append(wF32, float64(r.F32))
			wOK, wHost = append(wOK, r.OK), append(wHost, r.Host)
			wI8, wU64 = append(wI8, int64(r.I8)), append(wU64, r.U64)
		}
		var e1, e2, e3, e4, e5, e6, e7 error
		gTS, e1 = col("TS").Ints(nil)
		gSt, e2 = col("Status").Uints(nil)
		gVal, e3 = col("Val").Floats(nil)
		gF32, e4 = col("F32").Floats(nil)
		gOK, e5 = col("OK").Bools(nil)
		gHost, e6 = col("Host").Strings(nil)
		gI8, e7 = col("I8").Ints(nil)
		var e8 error
		gU64, e8 = col("U64").Uints(nil)
		for _, e := range []error{e1, e2, e3, e4, e5, e6, e7, e8} {
			if e != nil {
				t.Fatal(e)
			}
		}
		eq := func(name string, a, b any) {
			if len(rows) == 0 {
				return
			}
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("n=%d %s mismatch", len(rows), name)
			}
		}
		eq("TS", wTS, gTS)
		eq("Status", wSt, gSt)
		eq("Val", wVal, gVal)
		eq("F32", wF32, gF32)
		eq("OK", wOK, gOK)
		eq("Host", wHost, gHost)
		eq("I8", wI8, gI8)
		eq("U64", wU64, gU64)
		// wrong-kind and unknown-name errors
		if _, err := col("Host").Ints(nil); err != ErrKind {
			t.Fatal(err)
		}
		if _, err := rs.Column("Nope"); err != ErrNoField {
			t.Fatal(err)
		}
	}
}

func TestColumnAppendsAndReuses(t *testing.T) {
	b, _ := Marshal(&Table{"t", mkRows(100, nil)})
	v, _ := View(b, &Table{})
	rs := mustField(t, v, "Rows")
	c, _ := rs.Column("TS")
	buf := make([]int64, 0, 128)
	buf, _ = c.Ints(buf[:0])
	if len(buf) != 100 || buf[99] != 1700000000000+99*1000 {
		t.Fatal("ints")
	}
	buf, _ = c.Ints(buf) // appends
	if len(buf) != 200 {
		t.Fatal("append semantics")
	}
	a := testing.AllocsPerRun(20, func() { buf, _ = c.Ints(buf[:0]) })
	if a != 0 {
		t.Fatalf("Ints allocated %v", a)
	}
}

func TestScalarSliceBulk(t *testing.T) {
	in := sample()
	b, _ := Marshal(&in)
	v, _ := View(b, &Rich{})
	ints, err := mustField(t, v, "Times").Ints(nil)
	if err != nil || !reflect.DeepEqual(ints, in.Times) {
		t.Fatalf("times %v %v", ints, err)
	}
	us, _ := mustField(t, v, "Small").Uints(nil)
	if !reflect.DeepEqual(us, []uint64{1, 65535, 0}) {
		t.Fatalf("small %v", us)
	}
	fs, _ := mustField(t, v, "Fs").Floats(nil)
	if len(fs) != 3 || fs[0] != 1.5 || fs[2] != 2e100 || math.Signbit(fs[1]) != math.Signbit(in.Fs[1]) {
		t.Fatalf("fs %v", fs)
	}
	bs, _ := mustField(t, v, "Flags").Bools(nil)
	if !reflect.DeepEqual(bs, in.Flags) {
		t.Fatal("flags")
	}
	ss, _ := mustField(t, v, "Tags").Strings(nil)
	if !reflect.DeepEqual(ss, in.Tags) {
		t.Fatalf("tags %q", ss)
	}
	if _, err := mustField(t, v, "Tags").Ints(nil); err != ErrKind {
		t.Fatal(err)
	}
}

func TestColumnHostile(t *testing.T) {
	b, _ := Marshal(&Table{"t", mkRows(64, nil)})
	rng := rand.New(rand.NewSource(33))
	touch := func(data []byte) {
		v, err := View(data, &Table{})
		if err != nil {
			return
		}
		rs, err := v.Field("Rows")
		if err != nil {
			return
		}
		for _, n := range []string{"TS", "Host", "Val", "F32", "Status", "OK", "Sub", "I8"} {
			c, err := rs.Column(n)
			if err != nil {
				continue
			}
			_, _ = c.Ints(nil)
			_, _ = c.Floats(nil)
			_, _ = c.Strings(nil)
			_, _ = c.Bools(nil)
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

func BenchmarkColumnScan(b *testing.B) {
	data, _ := Marshal(&Table{"t", mkRows(10000, nil)})
	v, _ := View(data, &Table{})
	rs, _ := v.Field("Rows")
	b.Run("per-element", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var sum float64
			for j := 0; j < rs.Len(); j++ {
				e, _ := rs.Index(j)
				f, _ := e.Field("Val")
				sum += f.Float()
			}
			_ = sum
		}
	})
	b.Run("column", func(b *testing.B) {
		c, _ := rs.Column("Val")
		buf := make([]float64, 0, 10000)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf, _ = c.Floats(buf[:0])
			var sum float64
			for _, x := range buf {
				sum += x
			}
			_ = sum
		}
	})
}
