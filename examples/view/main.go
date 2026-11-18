// view: read single fields from an encoded message without decoding it.
// Numbers are read in place with no allocation; strings are copied unless you
// ask for the zero-copy *Ref variants.
package main

import (
	"fmt"
	"log"

	"github.com/oarkflow/codec"
)

type Point struct {
	TS   int64
	Host string
	Val  float64
}

type Batch struct {
	Source string
	Points []Point
}

func main() {
	b := Batch{Source: "collector-7", Points: make([]Point, 10000)}
	for i := range b.Points {
		b.Points[i] = Point{TS: 1700000000 + int64(i), Host: fmt.Sprintf("web-%d", i%4), Val: float64(i%100) / 4}
	}
	data, err := codec.Marshal(&b)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("message: %d bytes, %d points\n", len(data), len(b.Points))

	root, err := codec.View(data, &Batch{})
	if err != nil {
		log.Fatal(err)
	}
	src, _ := root.Field("Source")
	pts, _ := root.Field("Points")
	fmt.Println("source:", src.String(), "points:", pts.Len())

	// Random access to one element of a columnar slice: no full decode.
	row, err := pts.Index(7321)
	if err != nil {
		log.Fatal(err)
	}
	ts, _ := row.Field("TS")
	host, _ := row.Field("Host")
	val, _ := row.Field("Val")
	fmt.Printf("point 7321: ts=%d host=%s val=%.2f\n", ts.Int(), host.String(), val.Float())

	// Scan one column across all rows: the column is decoded once, in bulk,
	// into a reusable buffer (about 50x faster than reading element by element).
	col, err := pts.Column("Val")
	if err != nil {
		log.Fatal(err)
	}
	vals, err := col.Floats(nil) // pass vals[:0] next time to reuse the buffer
	if err != nil {
		log.Fatal(err)
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	fmt.Printf("sum of Val over %d points: %.2f\n", col.Len(), sum)

	hosts, _ := pts.Column("Host")
	names, _ := hosts.Strings(nil) // copies; use StringsRef for zero-copy views
	fmt.Println("first hosts:", names[:4])
}
