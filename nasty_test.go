package codec

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type nNode struct {
	Name string
	Next *nNode
}

type nTree struct {
	Val  int
	Kids []nTree
	Ptrs []*nTree
	Map  map[string]nTree
}

func chain(depth int) *nNode {
	var head *nNode
	for i := 0; i < depth; i++ {
		head = &nNode{Name: "n", Next: head}
	}
	return head
}

func nest(depth int) any {
	var v any = "leaf"
	for i := 0; i < depth; i++ {
		if i%2 == 0 {
			v = []any{v}
		} else {
			v = map[string]any{"k": v}
		}
	}
	return v
}

// Every codec path must give an error, not a stack overflow or a hang, for data
// that has no finite encoding.
func TestCyclicDataIsRefused(t *testing.T) {
	n := &nNode{Name: "loop"}
	n.Next = n
	if _, err := Marshal(n); err == nil {
		t.Error("binary: cyclic pointers accepted")
	}
	if _, err := MarshalEvolvable(n); err == nil {
		t.Error("evolvable: cyclic pointers accepted")
	}
	if _, err := MarshalText(n); err == nil {
		t.Error("text: cyclic pointers accepted")
	}
	m := map[string]any{}
	m["self"] = m
	var x any = m
	if _, err := Marshal(&x); err == nil {
		t.Error("binary: cyclic map accepted")
	}
	if _, err := MarshalText(&x); err == nil {
		t.Error("text: cyclic map accepted")
	}
	l := []any{nil}
	l[0] = l
	x = l
	if _, err := Marshal(&x); err == nil || !errors.Is(err, ErrTooDeep) {
		t.Errorf("binary: cyclic slice: %v", err)
	}
}

// A cycle that fans out (two ways back to itself) would take 2^depth steps if
// the encoder kept going after the first failure.
func TestFanOutCycleFailsFast(t *testing.T) {
	type Fan struct {
		A, B *Fan
		Kids []*Fan
	}
	f := &Fan{}
	f.A, f.B, f.Kids = f, f, []*Fan{f, f}
	for name, fn := range map[string]func() error{
		"binary": func() error { _, err := Marshal(f); return err },
		"text":   func() error { _, err := MarshalText(f); return err },
	} {
		done := make(chan error, 1)
		go func() { done <- fn() }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: cyclic data accepted", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: still running after 5s (exponential blow-up)", name)
		}
	}
}

func TestDeepNesting(t *testing.T) {
	// deep but within limits: exact round trip in every format
	for _, depth := range []int{10, 100, 400} {
		in := chain(depth)
		for name, rt := range map[string]func() (*nNode, error){
			"binary": func() (*nNode, error) {
				b, err := Marshal(in)
				if err != nil {
					return nil, err
				}
				var out nNode
				return &out, Unmarshal(b, &out)
			},
			"text": func() (*nNode, error) {
				b, err := MarshalText(in)
				if err != nil {
					return nil, err
				}
				var out nNode
				return &out, UnmarshalText(b, &out)
			},
		} {
			out, err := rt()
			if err != nil {
				t.Errorf("%s depth %d: %v", name, depth, err)
				continue
			}
			d := 0
			for p := out; p != nil; p = p.Next {
				d++
			}
			if d != depth {
				t.Errorf("%s depth %d: got depth %d", name, depth, d)
			}
		}
		var x any = nest(depth)
		b, err := Marshal(&x)
		var back any
		if err != nil || Unmarshal(b, &back) != nil || !reflect.DeepEqual(x, back) {
			t.Errorf("dynamic depth %d: %v", depth, err)
		}
	}
	// absurdly deep: refused (or handled), never a crash
	for _, depth := range []int{1000, 5000} {
		if b, err := Marshal(chain(depth)); err == nil {
			var out nNode
			_ = Unmarshal(b, &out)
		}
		if b, err := MarshalText(chain(depth)); err == nil {
			var out nNode
			_ = UnmarshalText(b, &out)
		}
		x := nest(depth)
		if b, err := Marshal(&x); err == nil {
			var back any
			_ = Unmarshal(b, &back)
		}
		var any2 any
		_ = UnmarshalText([]byte(strings.Repeat("a.", depth)+"a 1"), &any2)
	}
}

func TestRecursiveTypes(t *testing.T) {
	leaf := func(v int) nTree { return nTree{Val: v} }
	in := nTree{Val: 1,
		Kids: []nTree{{Val: 2, Kids: []nTree{leaf(3), leaf(4), leaf(5), leaf(6)}}, leaf(7)},
		Ptrs: []*nTree{{Val: 8, Ptrs: []*nTree{{Val: 9}}}, nil, {Val: 10}},
		Map:  map[string]nTree{"a": {Val: 11, Map: map[string]nTree{"b": leaf(12)}}, "c": leaf(13)},
	}
	b, err := Marshal(&in)
	if err != nil {
		t.Fatal(err)
	}
	var out nTree
	if err := Unmarshal(b, &out); err != nil || !same(reflect.ValueOf(in), reflect.ValueOf(out)) {
		t.Fatalf("binary: %v", err)
	}
	ev, _ := MarshalEvolvable(&in)
	var out2 nTree
	if err := Unmarshal(ev, &out2); err != nil || !same(reflect.ValueOf(in), reflect.ValueOf(out2)) {
		t.Fatalf("evolvable: %v", err)
	}
	tx, err := MarshalText(&in)
	if err != nil {
		t.Fatal(err)
	}
	var out3 nTree
	if err := UnmarshalText(tx, &out3); err != nil || !same(reflect.ValueOf(in), reflect.ValueOf(out3)) {
		t.Fatalf("text: %v\n%s", err, tx)
	}
}

func TestVeryWideData(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	type Row struct {
		ID   int64
		Name string
		Tags []string
		Vals []float64
	}
	rows := make([]Row, 50000)
	for i := range rows {
		rows[i] = Row{int64(i), "row", []string{"a", "b"}, []float64{float64(i), 0.5}}
	}
	ints := make([]int64, 1_000_000)
	for i := range ints {
		ints[i] = int64(i) * 7
	}
	big := map[int32]string{}
	for i := 0; i < 100000; i++ {
		big[int32(i)] = "v"
	}
	str := strings.Repeat("0123456789", 1_000_000) // 10 MB
	nested := map[string][]map[string][]Row{}
	for i := 0; i < 20; i++ {
		inner := []map[string][]Row{}
		for j := 0; j < 5; j++ {
			inner = append(inner, map[string][]Row{"k": rows[i : i+3]})
		}
		nested[string(rune('a'+i))] = inner
	}
	type Doc struct {
		Rows   []Row
		Ints   []int64
		Big    map[int32]string
		Str    string
		Nested map[string][]map[string][]Row
	}
	in := Doc{rows, ints, big, str, nested}

	b, err := Marshal(&in)
	if err != nil {
		t.Fatal(err)
	}
	var out Doc
	if err := Unmarshal(b, &out); err != nil || !reflect.DeepEqual(in, out) {
		t.Fatalf("binary: %v", err)
	}
	ev, _ := MarshalEvolvable(&in)
	var out2 Doc
	if err := Unmarshal(ev, &out2); err != nil || !reflect.DeepEqual(in, out2) {
		t.Fatalf("evolvable: %v", err)
	}
	tx, err := MarshalText(&in)
	if err != nil {
		t.Fatal(err)
	}
	var out3 Doc
	if err := UnmarshalText(tx, &out3); err != nil || !same(reflect.ValueOf(in), reflect.ValueOf(out3)) {
		t.Fatalf("text: %v", err)
	}
	t.Logf("binary %d B, text %d B for %d rows, 1M ints, 100k map entries, a 10 MB string", len(b), len(tx), len(rows))
}
