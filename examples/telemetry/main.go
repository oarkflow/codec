// telemetry: slices of structs are stored column by column, with a
// per-column choice of frame-of-reference bit-packing, delta varints or a
// dictionary. Repetitive batches shrink dramatically.
package main

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/oarkflow/codec"
)

type Point struct {
	TS     int64
	Host   string
	Metric string
	Val    float64
	Status uint32
	OK     bool
}

type Batch struct {
	Source string
	Points []Point
}

func main() {
	hosts := []string{"web-1", "web-2", "db-1", "cache-1"}
	metrics := []string{"cpu", "mem", "disk"}
	b := Batch{Source: "collector-7", Points: make([]Point, 5000)}
	for i := range b.Points {
		b.Points[i] = Point{
			TS: 1700000000000 + int64(i)*1000, Host: hosts[i%4], Metric: metrics[i%3],
			Val: float64(i%50) * 0.5, Status: uint32(200 + 300*(i%10/9)), OK: i%97 != 0,
		}
	}

	data, err := codec.Marshal(&b)
	if err != nil {
		log.Fatal(err)
	}
	js, _ := json.Marshal(&b)
	fmt.Printf("%d points: strata %d B (%.2f B/point), json %d B (%.1f B/point), %.0fx smaller\n",
		len(b.Points), len(data), float64(len(data))/float64(len(b.Points)),
		len(js), float64(len(js))/float64(len(b.Points)), float64(len(js))/float64(len(data)))

	var out Batch
	if err := codec.Unmarshal(data, &out); err != nil {
		log.Fatal(err)
	}
	fmt.Println("round trip ok:", out.Points[4999] == b.Points[4999] && len(out.Points) == len(b.Points))

	// Slices shorter than 4 elements use the row layout instead.
	few, _ := codec.Marshal(&Batch{Source: "x", Points: b.Points[:3]})
	fmt.Printf("3 points (row layout): %d B (%.1f B/point)\n", len(few), float64(len(few))/3)
}
