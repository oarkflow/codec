// datatypes: a tour of every data type and structure the codec supports.
//
// Each case below is round-tripped through binary, evolvable binary, the text
// notation and base64, and compared with the original. The program exits
// non-zero if any case does not hold, so it doubles as a conformance check.
// See README.md in this directory for the full table of supported types.
//
//	go run ./examples/datatypes
package main

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/oarkflow/codec"
)

var failures int

// check round-trips v through every representation and prints one summary.
func check[T any](label string, v T, eq ...func(a, b T) bool) {
	same := func(a, b T) bool { return reflect.DeepEqual(a, b) }
	if len(eq) > 0 {
		same = eq[0]
	}
	var marks []string
	ok := true
	step := func(name string, err error, got T) {
		if err != nil || !same(v, got) {
			ok = false
			marks = append(marks, name+"✗")
			if err != nil {
				marks = append(marks, "("+err.Error()+")")
			} else {
				marks = append(marks, fmt.Sprintf("(got %v)", got))
			}
			return
		}
		marks = append(marks, name+"✓")
	}

	bin, err := codec.Marshal(&v)
	var gb T
	if err == nil {
		err = codec.Unmarshal(bin, &gb)
	}
	step("binary", err, gb)

	ev, err := codec.MarshalEvolvable(&v)
	var ge T
	if err == nil {
		err = codec.Unmarshal(ev, &ge)
	}
	step("evolvable", err, ge)

	txt, err := codec.MarshalText(&v)
	var gt T
	if err == nil {
		err = codec.UnmarshalText(txt, &gt)
	}
	step("text", err, gt)

	b64, err := codec.MarshalBase64(&v)
	var g64 T
	if err == nil {
		err = codec.UnmarshalBase64(b64, &g64)
	}
	step("base64", err, g64)

	if !ok {
		failures++
	}
	status := "OK  "
	if !ok {
		status = "FAIL"
	}
	fmt.Printf("%s %-34s binary %4d B   %s\n", status, label, len(bin), strings.Join(marks, " "))
	lines := strings.Split(strings.TrimRight(string(txt), "\n"), "\n")
	if len(lines) > 9 {
		lines = append(lines[:8], fmt.Sprintf("... (%d more lines)", len(lines)-8))
	}
	for _, l := range lines {
		fmt.Printf("       | %s\n", clip(l, 100))
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func section(title string) { fmt.Printf("\n=== %s\n", title) }

// refuse shows a value the codec rejects, and the error it gives.
func refuse(label string, v any) {
	_, err := codec.Marshal(v)
	if err == nil {
		failures++
		fmt.Printf("FAIL %-34s was accepted\n", label)
		return
	}
	fmt.Printf("OK   %-34s refused: %v\n", label, clip(err.Error(), 90))
}

// ---- types used below ----

type Point struct {
	X, Y int32
}

type Address struct {
	Street string
	City   string
	Zip    uint32
}

type Person struct {
	Name    string
	Age     uint8
	Active  bool
	Score   float64
	Home    Address            // nested struct
	Work    *Address           // pointer to struct
	Tags    []string           // slice of strings
	Scores  map[string]float64 // map
	Friends []Person           // recursive: a slice of its own type
}

type Celsius float64 // named scalar types follow their kind
type Status uint8    // ... so do enums
type Names []string  // ... named slices
type Registry map[string]Names

type Money struct {
	Cents    int64
	Currency string
}

// Money implements the binary marshaler pair, so the codec uses it as is.
func (m Money) MarshalBinary() ([]byte, error) {
	return append([]byte(m.Currency), byte(m.Cents>>24), byte(m.Cents>>16), byte(m.Cents>>8), byte(m.Cents)), nil
}
func (m *Money) UnmarshalBinary(b []byte) error {
	if len(b) < 4 {
		return errors.New("short")
	}
	m.Currency = string(b[:len(b)-4])
	n := b[len(b)-4:]
	m.Cents = int64(n[0])<<24 | int64(n[1])<<16 | int64(n[2])<<8 | int64(n[3])
	return nil
}

type Color struct{ R, G, B uint8 }

// Color implements the text marshaler pair: it travels as "#rrggbb".
func (c Color) MarshalText() ([]byte, error) {
	return fmt.Appendf(nil, "#%02x%02x%02x", c.R, c.G, c.B), nil
}
func (c *Color) UnmarshalText(b []byte) error {
	_, err := fmt.Sscanf(string(b), "#%02x%02x%02x", &c.R, &c.G, &c.B)
	return err
}

type Base struct {
	ID      int64
	Created time.Time
}

type Article struct {
	Base     // embedded: a field named Base
	Title    string
	secret   string // unexported: never encoded
	Draft    string `codec:"-"`        // excluded
	Headline string `codec:"headline"` // renamed in text and interface{} values
}

type Node struct {
	Value int
	Next  *Node // a linked list
}

type Tree struct {
	Label string
	Kids  []Tree // a tree
}

func main() {
	section("1. Integers (every width; zigzag varints, so small numbers are small)")
	check("int8 min", int8(math.MinInt8))
	check("int16", int16(-12345))
	check("int32", int32(math.MaxInt32))
	check("int64 min", int64(math.MinInt64))
	check("int", -42)
	check("uint8 max", uint8(math.MaxUint8))
	check("uint16", uint16(65535))
	check("uint32", uint32(math.MaxUint32))
	check("uint64 max", uint64(math.MaxUint64))
	check("zero int (omitted: costs one bit)", 0)

	section("2. Floats (raw IEEE 754; text keeps them exact)")
	check("float32", float32(3.14159))
	check("float64 pi", math.Pi)
	check("float64 tiny", math.SmallestNonzeroFloat64)
	check("float64 huge", math.MaxFloat64)
	check("+Inf", math.Inf(1))
	check("-Inf", math.Inf(-1))
	check("negative zero", math.Copysign(0, -1), func(a, b float64) bool { return math.Signbit(a) == math.Signbit(b) && a == b })
	check("NaN", math.NaN(), func(a, b float64) bool { return math.IsNaN(a) && math.IsNaN(b) })

	section("3. Booleans, strings and bytes")
	check("bool", true)
	check("string (unicode)", "héllo, 世界 🌍")
	check("string (newline, quotes, tab)", "line1\nline2 \"quoted\"\ttab")
	check("string (looks like other types)", "true")
	check("string (leading/trailing space)", "  padded  ")
	check("string (empty)", "")
	check("[]byte", []byte{0, 1, 2, 253, 254, 255})
	check("string (invalid UTF-8)", "ok\xff\xfe\x00end")

	section("4. Structs (any nesting; fields in declaration order)")
	p := Person{
		Name: "Ada Lovelace", Age: 36, Active: true, Score: 99.5,
		Home:   Address{"12 Main St", "London", 12345},
		Work:   &Address{"1 Analytical Way", "Cambridge", 54321},
		Tags:   []string{"admin", "dev", "two words"},
		Scores: map[string]float64{"math": 100, "poetry": 87.5},
		Friends: []Person{
			{Name: "Charles", Age: 40, Tags: []string{"engine"}},
			{Name: "Mary", Age: 38, Friends: []Person{{Name: "Augusta", Age: 5}}},
		},
	}
	check("struct with everything inside", p)
	check("struct{} (empty)", struct{}{})
	check("anonymous struct", struct {
		A int
		B string
	}{7, "x"})

	section("5. Arrays (fixed size)")
	check("[3]int", [3]int{1, -2, 3})
	check("[4]byte", [4]byte{0xde, 0xad, 0xbe, 0xef})
	check("[16]byte (uuid-sized)", [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	check("[2][2]string", [2][2]string{{"a", "b"}, {"c", "d"}})
	check("[2]Point", [2]Point{{1, 2}, {3, 4}})

	section("6. Slices (the layout adapts to the element type)")
	check("[]int (delta varints)", []int{1000, 1001, 1003, 1006})
	check("[]uint8 (raw)", []uint8{1, 2, 3})
	check("[]bool (raw)", []bool{true, false, true, true})
	check("[]float64 (raw)", []float64{1.5, -2.25, 0})
	check("[]string", []string{"alpha", "beta", "gamma"})
	check("[][]int (list of lists)", [][]int{{1, 2, 3}, {4, 5}, {6}})
	check("[][][]string (3 deep)", [][][]string{{{"a", "b"}, {"c"}}, {{"d e"}}})
	check("[]Point (3: row layout)", []Point{{1, 2}, {3, 4}, {5, 6}})
	pts := make([]Point, 1000)
	for i := range pts {
		pts[i] = Point{int32(i), int32(i * 2)}
	}
	check("[]Point (6: columnar)", pts[:6]) // 4 or more elements are stored column by column
	packed, _ := codec.Marshal(&pts)
	fmt.Printf("OK   %-34s %d B for 1000 points (raw memory: %d B): perfectly regular data\n", "[]Point (1000): columnar packing", len(packed), 1000*8)
	irregular := make([]Point, 1000)
	for i := range irregular {
		irregular[i] = Point{int32((i * 7919) % 10007), int32((i * 104729) % 65521)}
	}
	packed, _ = codec.Marshal(&irregular)
	fmt.Printf("OK   %-34s %d B for 1000 points (raw memory: %d B): irregular data, still bit-packed\n", "[]Point (1000): columnar packing", len(packed), 1000*8)
	check("[]*Point (with a nil)", []*Point{{1, 2}, nil, {5, 6}})
	check("[]any (mixed)", []any{int64(1), "two", 3.5, true, nil})

	section("7. Maps (output is sorted by key, so it is deterministic)")
	check("map[string]int", map[string]int{"one": 1, "two": 2, "three": 3})
	check("map[int]string", map[int]string{3: "c", 1: "a", 2: "b"})
	check("map[bool]float64", map[bool]float64{true: 1.5, false: -1.5})
	check("map[string][]string", map[string][]string{"fruit": {"apple", "pear"}, "veg": {"kale"}})
	check("map[string]Point", map[string]Point{"origin": {0, 0}, "p": {3, 4}})
	check("map[string]map[string]int", map[string]map[string]int{"a": {"x": 1, "y": 2}, "b": {"z": 3}})
	check("map with awkward keys", map[string]int{"has space": 1, "dot.ted": 2, "": 3, "12": 4, `q"uote`: 5})

	section("7b. Binary-only: map keys that are not simple values")
	// Struct and array keys encode in binary, but the text form needs a key it can
	// write as a name, so it refuses them (the error says so).
	for label, v := range map[string]any{"map[Point]string": map[Point]string{{1, 2}: "a"}, "map[[2]int]string": map[[2]int]string{{1, 2}: "a"}} {
		bin, err := codec.Marshal(v)
		_, terr := codec.MarshalText(v)
		fmt.Printf("OK   %-34s binary %d B (err=%v); text refused: %v\n", label, len(bin), err, terr)
		if err != nil || terr == nil {
			failures++
		}
	}

	section("8. Pointers (nil costs one bit)")
	one := 1
	zero := 0
	s := "text"
	check("*int (points to 1)", &one, func(a, b *int) bool { return *a == *b })
	check("*int (points to zero)", &zero, func(a, b *int) bool { return b != nil && *a == *b })
	check("*string", &s, func(a, b *string) bool { return *a == *b })
	check("*Address", &Address{"a", "b", 1}, func(a, b *Address) bool { return *a == *b })
	check("struct with nil and set pointers", struct {
		A *int
		B *string
		C *Address
	}{nil, &s, &Address{Street: "x"}})
	check("*[]int", &[]int{1, 2, 3}, func(a, b *[]int) bool { return reflect.DeepEqual(*a, *b) })

	section("9. interface{} (self-describing; decodes to generic types)")
	var dyn any = map[string]any{
		"null":   nil,
		"bool":   true,
		"int":    int64(-7),
		"uint":   uint64(math.MaxUint64),
		"float":  2.5,
		"string": "text",
		"bytes":  []byte{1, 2, 3},
		"list":   []any{int64(1), "two", []any{3.0}},
		"map":    map[string]any{"nested": map[string]any{"deeper": int64(1)}},
		"empty":  map[string]any{},
	}
	check("map[string]any (all dynamic kinds)", dyn)
	check("[]any nested", any([]any{[]any{int64(1), []any{int64(2)}}, map[string]any{"k": []any{}}}))
	// Inside an interface{}, values keep their kind but not their width: narrow
	// integers and float32 come back as int64 / uint64 / float64, and a time.Time
	// becomes its RFC 3339 string.
	var (
		narrow any = int32(5)
		when   any = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		f32    any = float32(1.5)
	)
	for _, v := range []*any{&narrow, &when, &f32} {
		data, _ := codec.Marshal(v)
		var out any
		codec.Unmarshal(data, &out)
		fmt.Printf("OK   %-34s -> %T %v\n", fmt.Sprintf("interface{} holding %T", *v), out, out)
	}
	// A struct inside an interface{} becomes a map keyed by field name.
	var anyStruct any = Address{"s", "c", 9}
	b, _ := codec.Marshal(&anyStruct)
	var back any
	codec.Unmarshal(b, &back)
	fmt.Printf("OK   %-34s decodes as %T: %v\n", "struct inside interface{}", back, back)

	section("10. Standard library and custom types (through their marshalers)")
	check("time.Time", time.Date(2026, 11, 18, 10, 30, 0, 123456789, time.UTC), func(a, b time.Time) bool { return a.Equal(b) })
	check("time.Duration (an int64)", 90*time.Minute)
	check("netip.Addr (IPv6)", netip.MustParseAddr("2001:db8::1"))
	check("netip.Prefix", netip.MustParsePrefix("10.0.0.0/8"))
	check("*big.Int", new(big.Int).Lsh(big.NewInt(1), 100), func(a, b *big.Int) bool { return a.Cmp(b) == 0 })
	check("Money (MarshalBinary)", Money{Cents: 123456, Currency: "USD"})
	check("Color (MarshalText -> #rrggbb)", Color{255, 136, 0})
	check("[]Color", []Color{{1, 2, 3}, {255, 255, 255}})
	check("Celsius (named float64)", Celsius(36.6))
	check("Status (named uint8)", Status(3))
	check("Names (named []string)", Names{"a", "b"})
	check("Registry (named map)", Registry{"x": {"1", "2"}, "y": {"3"}})

	section("11. Struct features: embedding, tags, unexported fields")
	art := Article{Base: Base{7, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}, Title: "Hello", secret: "never encoded", Draft: "excluded", Headline: "renamed"}
	b, _ = codec.Marshal(&art)
	var gotArt Article
	codec.Unmarshal(b, &gotArt)
	t, _ := codec.MarshalText(&art)
	fmt.Printf("OK   %-34s Draft and secret are not carried: draft=%q secret=%q\n", "tags and unexported fields", gotArt.Draft, gotArt.secret)
	fmt.Printf("       | %s\n", strings.ReplaceAll(strings.TrimRight(string(t), "\n"), "\n", "\n       | "))
	if gotArt.Draft != "" || gotArt.secret != "" || gotArt.Title != "Hello" || gotArt.Headline != "renamed" || !gotArt.Created.Equal(art.Created) || gotArt.ID != 7 {
		failures++
		fmt.Println("FAIL embedded/tags")
	}

	section("12. Recursive types")
	list := &Node{1, &Node{2, &Node{3, nil}}}
	check("linked list", list, func(a, b *Node) bool { return reflect.DeepEqual(a, b) })
	check("tree", Tree{"root", []Tree{{"a", []Tree{{"a1", nil}, {"a2", nil}}}, {"b", nil}}})

	section("13. Zero, null and empty")
	type Z struct {
		N int
		S string
		P *int
		L []int
		M map[string]int
		E struct{ A int }
	}
	z := Z{}
	bz, _ := codec.Marshal(&z)
	tz, _ := codec.MarshalText(&z)
	fmt.Printf("OK   %-34s binary %d B, text %q (all zero values are omitted)\n", "all-zero struct", len(bz), string(tz))
	var nilSlice, emptySlice []int = nil, []int{}
	b1, _ := codec.Marshal(&nilSlice)
	b2, _ := codec.Marshal(&emptySlice)
	fmt.Printf("OK   %-34s encode identically: %v\n", "nil slice vs empty slice", string(b1) == string(b2))

	section("14. What is refused (with the error you get)")
	refuse("chan int", make(chan int))
	refuse("func()", func() {})
	refuse("complex128", complex(1, 2))
	refuse("uintptr", uintptr(5))
	refuse("fmt.Stringer field", struct{ S fmt.Stringer }{})
	cyc := &Node{Value: 1}
	cyc.Next = cyc
	refuse("cyclic pointers", cyc)

	section("15. Everything at once, sizes in each format")
	all := struct {
		People  []Person
		Matrix  [][]float64
		Lookup  map[string][]Point
		Any     any
		When    time.Time
		IP      netip.Addr
		Tags    map[string]string
		Blob    []byte
		Maybe   *Address
		Samples []Point
	}{
		People:  []Person{p, {Name: "Solo"}},
		Matrix:  [][]float64{{1, 2}, {3.5}},
		Lookup:  map[string][]Point{"a": {{1, 2}}, "b": {{3, 4}, {5, 6}}},
		Any:     dyn,
		When:    time.Date(2026, 11, 18, 10, 30, 0, 0, time.UTC),
		IP:      netip.MustParseAddr("192.0.2.1"),
		Tags:    map[string]string{"env": "prod"},
		Blob:    []byte("raw bytes"),
		Maybe:   &Address{Street: "x"},
		Samples: pts[:100],
	}
	bin, _ := codec.Marshal(&all)
	ev, _ := codec.MarshalEvolvable(&all)
	txt, _ := codec.MarshalText(&all)
	b64, _ := codec.MarshalBase64(&all)
	fmt.Printf("     binary %d B, evolvable %d B, text %d B, base64 %d B\n", len(bin), len(ev), len(txt), len(b64))
	for name, run := range map[string]func() error{
		"binary":    func() error { var o = all; o.People = nil; return codec.Unmarshal(bin, &o) },
		"evolvable": func() error { var o = all; o.People = nil; return codec.Unmarshal(ev, &o) },
		"text":      func() error { var o = all; o.People = nil; return codec.UnmarshalText(txt, &o) },
		"base64":    func() error { var o = all; o.People = nil; return codec.UnmarshalBase64(b64, &o) },
	} {
		if err := run(); err != nil {
			failures++
			fmt.Printf("FAIL %s: %v\n", name, err)
		}
	}

	fmt.Println()
	if failures > 0 {
		fmt.Printf("%d case(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Println("all cases round-tripped through binary, evolvable, text and base64")
}
