package codec

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"strconv"
	"testing"
)

func benchVal() Rich { return sample() }

func TestSizes(t *testing.T) {
	v := benchVal()
	s, _ := Marshal(&v)
	j, _ := json.Marshal(&v)
	var g bytes.Buffer
	gob.NewEncoder(&g).Encode(&v)
	t.Logf("strata=%dB json=%dB gob(first msg)=%dB", len(s), len(j), g.Len())
}

func BenchmarkStrataMarshal(b *testing.B) {
	v := benchVal()
	buf := make([]byte, 0, 1024)
	b.ReportAllocs()
	for b.Loop() {
		buf, _ = Append(buf[:0], &v)
	}
}

func BenchmarkStrataMarshalValue(b *testing.B) {
	v := benchVal()
	buf := make([]byte, 0, 1024)
	b.ReportAllocs()
	for b.Loop() {
		buf, _ = Append(buf[:0], v)
	}
}

func BenchmarkStrataUnmarshal(b *testing.B) {
	v := benchVal()
	data, _ := Marshal(&v)
	var out Rich
	b.ReportAllocs()
	for b.Loop() {
		_ = Unmarshal(data, &out)
	}
}

func BenchmarkStrataUnmarshalNoCopy(b *testing.B) {
	v := benchVal()
	data, _ := MarshalEvolvable(&v)
	var out Rich
	b.ReportAllocs()
	for b.Loop() {
		_ = UnmarshalNoCopy(data, &out)
	}
}

func BenchmarkStrataValueDecoderNoCopy(b *testing.B) {
	v := benchVal()
	data, _ := MarshalEvolvable(&v)
	d, err := NewValueDecoder[Rich]()
	if err != nil {
		b.Fatal(err)
	}
	var out Rich
	b.ReportAllocs()
	for b.Loop() {
		_ = d.UnmarshalNoCopy(data, &out)
	}
}

func BenchmarkJSONMarshal(b *testing.B) {
	v := benchVal()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = json.Marshal(&v)
	}
}

func BenchmarkJSONMarshalValue(b *testing.B) {
	v := benchVal()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = json.Marshal(v)
	}
}

func BenchmarkJSONUnmarshal(b *testing.B) {
	v := benchVal()
	data, _ := json.Marshal(&v)
	var out Rich
	b.ReportAllocs()
	for b.Loop() {
		_ = json.Unmarshal(data, &out)
	}
}

func BenchmarkGobRoundTrip(b *testing.B) { // streaming gob with warmed type info
	v := benchVal()
	var buf bytes.Buffer
	e, d := gob.NewEncoder(&buf), gob.NewDecoder(&buf)
	var out Rich
	b.ReportAllocs()
	for b.Loop() {
		_ = e.Encode(&v)
		_ = d.Decode(&out)
	}
}

func benchBatch() Table { return Table{"t", mkRows(1000, nil)} }

func BenchmarkStrataBatchMarshal(b *testing.B) {
	v := benchBatch()
	buf := make([]byte, 0, 1<<16)
	b.ReportAllocs()
	for b.Loop() {
		buf, _ = Append(buf[:0], &v)
	}
}

func BenchmarkStrataBatchUnmarshal(b *testing.B) {
	v := benchBatch()
	data, _ := Marshal(&v)
	var out Table
	b.ReportAllocs()
	for b.Loop() {
		_ = Unmarshal(data, &out)
	}
}

type genericMapBench struct {
	Tags      map[string][]int
	Names     map[int]string
	Addresses map[string]Addr
}

func genericMapValue() genericMapBench {
	v := genericMapBench{
		Tags:      make(map[string][]int, 32),
		Names:     make(map[int]string, 32),
		Addresses: make(map[string]Addr, 32),
	}
	for i := 0; i < 32; i++ {
		key := strconv.Itoa(i)
		v.Tags[key] = []int{i, -i}
		v.Names[i] = key
		v.Addresses[key] = Addr{Street: key, Zip: uint32(i)}
	}
	return v
}

func BenchmarkStrataGenericMapUnmarshal(b *testing.B) {
	in := genericMapValue()
	data, _ := Marshal(&in)
	var out genericMapBench
	b.ReportAllocs()
	for b.Loop() {
		if err := Unmarshal(data, &out); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJSONGenericMapUnmarshal(b *testing.B) {
	in := genericMapValue()
	data, _ := json.Marshal(&in)
	var out genericMapBench
	b.ReportAllocs()
	for b.Loop() {
		if err := json.Unmarshal(data, &out); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStrataDynamicBytesUnmarshal(b *testing.B) {
	type payload struct{ Values map[string]any }
	in := payload{Values: make(map[string]any, 32)}
	for i := 0; i < 32; i++ {
		in.Values[strconv.Itoa(i)] = []byte("0123456789abcdef0123456789abcdef")
	}
	data, err := Marshal(&in)
	if err != nil {
		b.Fatal(err)
	}
	var out payload
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := Unmarshal(data, &out); err != nil {
			b.Fatal(err)
		}
	}
}
