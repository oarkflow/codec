// allocs: where allocations go. Append reuses your output buffer, so encoding
// allocates nothing; Unmarshal always returns fresh, independent values
// (safe to keep) in a handful of allocations; View reads fields straight from
// the bytes without allocating.
package main

import (
	"fmt"
	"log"
	"runtime"

	"github.com/oarkflow/codec"
)

type Reading struct {
	ID    uint32
	Name  string
	Vals  []float64
	Stamp []int64
}

func main() {
	in := Reading{ID: 7, Name: "sensor-a", Vals: make([]float64, 256), Stamp: make([]int64, 256)}
	for i := range in.Vals {
		in.Vals[i] = float64(i) / 3
		in.Stamp[i] = 1700000000 + int64(i)
	}

	var buf []byte
	var out Reading
	var err error

	measure := func(name string, f func()) {
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		const n = 10000
		for i := 0; i < n; i++ {
			f()
		}
		runtime.ReadMemStats(&b)
		fmt.Printf("%-18s %6.2f allocs/op %8.0f B/op\n", name,
			float64(b.Mallocs-a.Mallocs)/n, float64(b.TotalAlloc-a.TotalAlloc)/n)
	}

	measure("Marshal", func() { _, _ = codec.Marshal(&in) })
	measure("Append(reused)", func() { buf, err = codec.Append(buf[:0], &in) })
	if err != nil {
		log.Fatal(err)
	}
	measure("Unmarshal", func() { var r Reading; _ = codec.Unmarshal(buf, &r) })
	root, err := codec.View(buf, &Reading{})
	if err != nil {
		log.Fatal(err)
	}
	var sum float64
	measure("View (read 1 field)", func() {
		f, _ := root.Field("Vals")
		e, _ := f.Index(100)
		sum += e.Float()
	})
	_ = codec.Unmarshal(buf, &out)
	fmt.Println("decoded:", out.ID, out.Name, len(out.Vals), out.Stamp[255], sum > 0)
}
