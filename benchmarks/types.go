package benchmarks

import "fmt"

// Plain Go types shared by Strata, JSON, gob, msgpack and CBOR.
// They avoid constructs protobuf cannot express (nil elements in lists).

type Addr struct {
	Street string
	Zip    uint32
}

type Item struct {
	SKU   string
	Qty   int32
	Price float64
}

type Doc struct {
	ID     int64
	Name   string
	Age    uint32
	Active bool
	Score  float64
	Ratio  float32
	Delta  int32
	Tags   []string
	Times  []int64
	Small  []uint32
	Raw    []byte
	Flags  []bool
	Fs     []float64
	Addr   *Addr
	Home   Addr
	Items  []Item
}

type Point struct {
	TS     int64
	Host   string
	Val    float64
	Status uint32
}

type Batch struct {
	Source string
	Points []Point
}

func NewDoc() Doc {
	return Doc{
		ID: 1234567, Name: "Ada Lovelace", Age: 36, Active: true, Score: 3.5, Ratio: -0.25, Delta: -7,
		Tags: []string{"admin", "dev", "ops"}, Times: []int64{1700000000, 1700000005, 1700000011, 1700000020},
		Small: []uint32{1, 20, 300, 4000}, Raw: []byte{0, 1, 2, 3, 4, 5, 6, 7},
		Flags: []bool{true, false, true}, Fs: []float64{1.5, 2.5, 3.5},
		Addr: &Addr{"12 Main St", 12345}, Home: Addr{"9 Elm Ave", 54321},
		Items: []Item{{"sku-001", 2, 9.99}, {"sku-002", 1, 24.5}, {"sku-003", 10, 0.5}},
	}
}

// NewBatch: 1000 telemetry points, steady timestamps, few distinct hosts.
func NewBatch() Batch {
	b := Batch{Source: "collector-7", Points: make([]Point, 1000)}
	hosts := []string{"web-1", "web-2", "db-1", "cache-1"}
	for i := range b.Points {
		b.Points[i] = Point{TS: 1700000000000 + int64(i)*1000, Host: hosts[i%4], Val: float64(i%50) * 0.5, Status: uint32(200 + (i%7)/6*300)}
	}
	return b
}

// Dynamic exercises maps and interface{} values (what JSON users often have).
type Dynamic struct {
	Name   string
	Counts map[string]int64
	Labels map[string]string
	Extra  map[string]any
}

func NewDynamic() Dynamic {
	d := Dynamic{Name: "svc", Counts: map[string]int64{}, Labels: map[string]string{
		"env": "prod", "region": "eu-west-1", "team": "core", "tier": "backend"},
		Extra: map[string]any{"enabled": true, "ratio": 0.75, "tags": []any{"a", "b", "c"}, "limits": map[string]any{"cpu": int64(4), "mem": int64(8192)}}}
	for i := 0; i < 40; i++ {
		d.Counts[fmt.Sprintf("metric_%02d", i)] = int64(i * 37)
	}
	return d
}
