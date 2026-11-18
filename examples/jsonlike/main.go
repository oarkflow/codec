// jsonlike: Marshal and Unmarshal work like encoding/json. Any Go value can
// be marshaled (structs, slices, arrays, maps, pointers, scalars, interface{},
// time.Time, anything with MarshalBinary/MarshalText) and you Unmarshal into a
// pointer to the same type. No schema file and no generated code.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/netip"
	"time"

	"github.com/oarkflow/codec"
)

type Profile struct {
	Name     string
	Born     time.Time
	Home     netip.Addr
	Scores   map[string]float64
	Friends  []*Profile
	Meta     map[string]any // dynamic values decode to generic types, as in encoding/json
	Nickname *string
}

func roundTrip[T any](label string, in T) T {
	data, err := codec.Marshal(in)
	if err != nil {
		log.Fatal(err)
	}
	js, _ := json.Marshal(in)
	var out T
	if err := codec.Unmarshal(data, &out); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%-22s strata %4d B   json %4d B   -> %v\n", label, len(data), len(js), out)
	return out
}

func main() {
	roundTrip("int", 42)
	roundTrip("string", "héllo")
	roundTrip("[]int", []int{1, 2, 3, 4, 5})
	roundTrip("[3]string", [3]string{"a", "b", "c"})
	roundTrip("map[string]int", map[string]int{"apples": 3, "pears": 5})
	roundTrip("map[int][]string", map[int][]string{1: {"a"}, 2: {"b", "c"}})
	roundTrip("time.Time", time.Date(2026, 11, 18, 8, 30, 0, 0, time.UTC))

	nick := "ace"
	p := Profile{
		Name: "Ada", Born: time.Date(1815, 12, 10, 0, 0, 0, 0, time.UTC), Home: netip.MustParseAddr("10.0.0.7"),
		Scores:   map[string]float64{"math": 99.5, "poetry": 87},
		Friends:  []*Profile{{Name: "Charles"}, nil},
		Meta:     map[string]any{"admin": true, "level": 7, "tags": []any{"a", "b"}},
		Nickname: &nick,
	}
	out := roundTrip("Profile (struct)", p)
	fmt.Println("  nickname:", *out.Nickname, "| level type:", fmt.Sprintf("%T", out.Meta["level"]))

	// Decode into interface{}: marshal from one (a *any or an `any` field).
	var anyIn any = map[string]any{"k": []any{1, "two", 3.5}}
	data, _ := codec.Marshal(&anyIn)
	var anyOut any
	if err := codec.Unmarshal(data, &anyOut); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("interface{}: %v\n", anyOut)

	// Streams, like json.Encoder / json.Decoder.
	var buf bytes.Buffer
	enc := codec.NewEncoder(&buf)
	for i := 1; i <= 3; i++ {
		enc.Encode(map[string]int{"n": i})
	}
	dec := codec.NewDecoder(&buf)
	for dec.More() {
		var m map[string]int
		if err := dec.Decode(&m); err != nil {
			log.Fatal(err)
		}
		fmt.Println("stream item:", m)
	}

	// Maps are encoded in sorted key order, so output is deterministic.
	a, _ := codec.Marshal(map[string]int{"x": 1, "y": 2, "z": 3})
	b, _ := codec.Marshal(map[string]int{"z": 3, "y": 2, "x": 1})
	fmt.Println("deterministic:", bytes.Equal(a, b))
}
