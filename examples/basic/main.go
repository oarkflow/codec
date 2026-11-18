// basic: marshal and unmarshal a struct with no schema file and no codegen.
package main

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/oarkflow/codec"
)

type Address struct {
	Street string
	Zip    uint32
}

type User struct {
	ID      int64
	Name    string
	Email   string
	Active  bool
	Score   float64
	Tags    []string
	Address *Address
	secret  string // unexported fields are skipped
	Cache   int    `codec:"-"` // so are tagged ones
}

func main() {
	in := User{
		ID: 42, Name: "Ada Lovelace", Email: "ada@example.com", Active: true, Score: 99.5,
		Tags: []string{"admin", "dev"}, Address: &Address{"12 Main St", 12345},
	}

	data, err := codec.Marshal(&in)
	if err != nil {
		log.Fatal(err)
	}
	js, _ := json.Marshal(in)
	fmt.Printf("strata: %d bytes, json: %d bytes\n", len(data), len(js))
	fmt.Printf("hex: %x\n", data)

	var out User
	if err := codec.Unmarshal(data, &out); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("decoded: %+v address=%+v\n", out, *out.Address)

	// Zero values cost one bit, so a sparse struct is tiny.
	sparse, _ := codec.Marshal(User{ID: 1})
	fmt.Printf("sparse user: %d bytes (%x)\n", len(sparse), sparse)

	// A different struct type is rejected by the schema fingerprint.
	var wrong Address
	fmt.Println("decode into wrong type:", codec.Unmarshal(data, &wrong))
}
