package benchmarks

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/oarkflow/codec"
	"github.com/oarkflow/codec/benchmarks/pb"
	shamaton "github.com/shamaton/msgpack/v2"
	vmsgpack "github.com/vmihailenco/msgpack/v5"
	"google.golang.org/protobuf/proto"
)

type tcase struct {
	name string
	enc  func(buf []byte) ([]byte, error)
	dec  func(data []byte) error
}

// cases builds one encode/decode pair per codec for value v. Decode targets
// are allocated once and reused, as is typical in hot paths. The protobuf
// message is prebuilt so we measure the codec, not struct conversion.
func cases[T any](v *T, pbMsg proto.Message, pbNew func() proto.Message) []tcase {
	out := new(T)
	var pbOut proto.Message
	if pbNew != nil {
		pbOut = pbNew()
	}
	cs := []tcase{
		{"codec", func(b []byte) ([]byte, error) { return codec.Append(b[:0], v) },
			func(d []byte) error { return codec.Unmarshal(d, out) }},
		{"codec-nocopy", func(b []byte) ([]byte, error) { return codec.Append(b[:0], v) },
			func(d []byte) error { return codec.UnmarshalNoCopy(d, out) }},
		{"msgpack-vmihailenco", func([]byte) ([]byte, error) { return vmsgpack.Marshal(v) },
			func(d []byte) error { return vmsgpack.Unmarshal(d, out) }},
		{"msgpack-shamaton", func([]byte) ([]byte, error) { return shamaton.Marshal(v) },
			func(d []byte) error { return shamaton.Unmarshal(d, out) }},
		{"cbor", func([]byte) ([]byte, error) { return cbor.Marshal(v) },
			func(d []byte) error { return cbor.Unmarshal(d, out) }},
		{"json", func([]byte) ([]byte, error) { return json.Marshal(v) },
			func(d []byte) error { return json.Unmarshal(d, out) }},
	}
	if pbMsg != nil {
		cs = append(cs[:1], append([]tcase{{"protobuf", func(b []byte) ([]byte, error) { return proto.MarshalOptions{}.MarshalAppend(b[:0], pbMsg) },
			func(d []byte) error { return proto.Unmarshal(d, pbOut) }}}, cs[1:]...)...)
	}
	return cs
}

func allWorkloads() map[string][]tcase {
	d, b, dy := NewDoc(), NewBatch(), NewDynamic()
	return map[string][]tcase{
		"doc":       cases(&d, d.ToPB(), func() proto.Message { return &pb.Doc{} }),
		"batch1000": cases(&b, b.ToPB(), func() proto.Message { return &pb.Batch{} }),
		"dynamic":   cases(&dy, nil, nil), // maps and interface{}: protobuf has no equivalent
	}
}

var order = []string{"doc", "batch1000", "dynamic"}

func TestRoundTripAndSizes(t *testing.T) {
	d, b := NewDoc(), NewBatch()
	var gd Doc
	var gb Batch
	// Codec must round-trip both workloads exactly.
	for _, c := range []struct {
		in  any
		out any
	}{{&d, &gd}, {&b, &gb}} {
		data, err := codec.Marshal(c.in)
		if err != nil {
			t.Fatal(err)
		}
		if err := codec.Unmarshal(data, c.out); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.in, c.out) {
			t.Fatalf("Codec round trip mismatch for %T", c.in)
		}
	}
	ws := allWorkloads()
	for _, w := range order {
		fmt.Printf("\nsize of %s:\n", w)
		for _, c := range ws[w] {
			data, err := c.enc(nil)
			if err != nil {
				t.Fatalf("%s %s: %v", w, c.name, err)
			}
			if err := c.dec(data); err != nil {
				t.Fatalf("%s %s decode: %v", w, c.name, err)
			}
			fmt.Printf("  %-22s %7d B\n", c.name, len(data))
		}
	}
}

func BenchmarkMarshal(b *testing.B) {
	ws := allWorkloads()
	for _, w := range order {
		for _, c := range ws[w] {
			b.Run(w+"/"+c.name, func(b *testing.B) {
				buf := make([]byte, 0, 1<<16)
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, err := c.enc(buf); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkUnmarshal(b *testing.B) {
	ws := allWorkloads()
	for _, w := range order {
		for _, c := range ws[w] {
			data, _ := c.enc(nil)
			b.Run(w+"/"+c.name, func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if err := c.dec(data); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
