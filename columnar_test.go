package codec

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
)

type Row struct {
	TS     int64
	Host   string
	Val    float64
	F32    float32
	Status uint32
	U64    uint64
	I8     int8
	OK     bool
	Note   string
	Tags   []string
	Sub    Addr
	Ref    *Addr
}

type Table struct {
	Name string
	Rows []Row
}

func mkRows(n int, rng *rand.Rand) []Row {
	hosts := []string{"web-1", "web-2", "db-1", "cache-1"}
	rows := make([]Row, n)
	for i := range rows {
		r := Row{
			TS: 1700000000000 + int64(i)*1000, Host: hosts[i%4], Val: float64(i%50) * 0.5,
			F32: float32(i%3) * 1.5, Status: uint32(200 + i%3), U64: math.MaxUint64 - uint64(i%2),
			I8: int8(i%7 - 3), OK: i%5 == 0, Sub: Addr{Street: hosts[i%2], Zip: uint32(i)},
		}
		if rng != nil {
			r.Val = rng.NormFloat64()
			r.TS = int64(rng.Uint64())
			r.Note = string(rune('a' + rng.Intn(26)))
		}
		if i%3 == 0 {
			r.Ref = &Addr{"r", uint32(i)}
		}
		if i%4 == 1 {
			r.Tags = []string{"x", hosts[i%4]}
		}
		rows[i] = r
	}
	return rows
}

func roundTrip(t *testing.T, in Table) []byte {
	t.Helper()
	b, err := Marshal(&in)
	if err != nil {
		t.Fatal(err)
	}
	var out Table
	if err := Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("mismatch (n=%d)\n in=%+v\nout=%+v", len(in.Rows), in.Rows[:min(3, len(in.Rows))], out.Rows[:min(3, len(out.Rows))])
	}
	return b
}

func TestColumnarShapes(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for _, n := range []int{1, 3, 4, 5, 8, 9, 100, 1000} {
		roundTrip(t, Table{"t", mkRows(n, nil)})
		roundTrip(t, Table{"t", mkRows(n, rng)})
	}
	// all-zero elements fall back to rows and still round trip
	roundTrip(t, Table{"z", make([]Row, 50)})
	// constant lanes, extreme ints
	rows := make([]Row, 20)
	for i := range rows {
		rows[i] = Row{TS: math.MinInt64 + int64(i%2)*math.MaxInt64, U64: math.MaxUint64, I8: -128 + int8(i%2)*127}
	}
	roundTrip(t, Table{"x", rows})
}

func TestReuseColumnarClearsNestedSlicesAndPointers(t *testing.T) {
	rows := mkRows(64, nil)
	data, err := Marshal(&Table{Name: "populated", Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	var out Table
	if err := Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}

	rows = make([]Row, len(rows))
	for i := range rows {
		rows[i].TS = int64(i + 1) // Keep the columnar layout while other lanes are zero.
	}
	want := Table{Name: "cleared", Rows: rows}
	data, err = Marshal(&want)
	if err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatal("reused columnar target retained nested values")
	}
}

func TestColumnarSmallerThanRows(t *testing.T) {
	rows := mkRows(1000, nil)
	col, _ := Marshal(&Table{"t", rows})
	// 3 rows => row layout; scale cost per element to compare fairly
	small, _ := Marshal(&Table{"t", rows[:3]})
	t.Logf("columnar 1000 rows=%dB (%.1f B/row), row layout 3 rows=%dB (%.1f B/row)",
		len(col), float64(len(col))/1000, len(small), float64(len(small))/3)
	if float64(len(col))/1000 > float64(len(small))/3 {
		t.Fatal("columnar should be smaller per row")
	}
}

func TestColumnarHostile(t *testing.T) {
	b, _ := Marshal(&Table{"t", mkRows(64, nil)})
	for i := 0; i < len(b); i++ {
		var out Table
		_ = Unmarshal(b[:i], &out)
	}
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 200000; i++ {
		c := append([]byte(nil), b...)
		for k := 0; k < 1+rng.Intn(4); k++ {
			c[rng.Intn(len(c))] = byte(rng.Intn(256))
		}
		var out Table
		_ = Unmarshal(c, &out)
	}
}

func FuzzColumnar(f *testing.F) {
	b, _ := Marshal(&Table{"t", mkRows(40, nil)})
	f.Add(b)
	f.Fuzz(func(t *testing.T, data []byte) {
		var out Table
		if Unmarshal(data, &out) == nil {
			b2, err := Marshal(&out)
			if err != nil {
				t.Fatal(err)
			}
			var out2 Table
			if err := Unmarshal(b2, &out2); err != nil {
				t.Fatal(err)
			}
		}
	})
}
