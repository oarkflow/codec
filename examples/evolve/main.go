// evolve: change a struct after data has been written, without migrating it.
//
// Data written with MarshalEvolvable can be read by a later version of the
// type that has fields added at the end of its structs (at any depth). The
// new fields read as zero. Run: go run ./examples/evolve
package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/oarkflow/codec"
	"github.com/oarkflow/codec/store"
)

// --- version 1, as shipped last year ---
type AddrV1 struct {
	Street string
	Zip    uint32
}
type UserV1 struct {
	Name  string
	Age   uint32
	Tags  []string
	Addr  AddrV1
	Notes []AddrV1
}

// --- version 2: fields appended, even inside nested structs and lists ---
type AddrV2 struct {
	Street  string
	Zip     uint32
	Country string // new
}
type UserV2 struct {
	Name  string
	Age   uint32
	Tags  []string
	Addr  AddrV2
	Notes []AddrV2
	Email string   // new
	Roles []string // new
}

func main() {
	// 1. bytes written by v1 ...
	v1 := UserV1{
		Name: "Ada", Age: 36, Tags: []string{"admin"},
		Addr:  AddrV1{Street: "12 Main", Zip: 12345},
		Notes: []AddrV1{{Street: "a", Zip: 1}, {Street: "b", Zip: 2}},
	}
	data, err := codec.MarshalEvolvable(&v1)
	if err != nil {
		log.Fatal(err)
	}

	// ... are read by v2: old fields intact, new ones zero
	var v2 UserV2
	if err := codec.Unmarshal(data, &v2); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("v2 reads v1 data: %+v\n", v2)

	// 2. the other direction is refused, never misread
	newer, err := codec.MarshalEvolvable(&UserV2{Name: "Bob", Email: "bob@x"})
	if err != nil {
		log.Fatal(err)
	}
	var old UserV1
	fmt.Println("v1 reads v2 data:", codec.Unmarshal(newer, &old))

	// 3. what is not append-only is refused too
	type Reordered struct {
		Age  uint32
		Name string
	}
	var r Reordered
	err = codec.Unmarshal(data, &r)
	fmt.Println("reordered fields:", errors.Is(err, codec.ErrSchemaMismatch))

	// 4. the persistent store writes evolvable records by default, so an
	// upgraded program opens yesterday's database as it is
	dir, err := os.MkdirTemp("", "evolve")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "users")
	db1, err := store.Open[UserV1](path, nil)
	if err != nil {
		log.Fatal(err)
	}
	if err := db1.Put("u:1", &v1); err != nil {
		log.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		log.Fatal(err)
	}

	db2, err := store.Open[UserV2](path, nil) // same files, new type
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := db2.Close(); err != nil {
			log.Print(err)
		}
	}()
	var u UserV2
	if err := db2.Get("u:1", &u); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("store: %s, %d notes, email=%q\n", u.Name, len(u.Notes), u.Email)
	if err := db2.Update("u:1", func(u *UserV2, _ bool) error {
		u.Email = "ada@example.com"
		return nil
	}); err != nil {
		log.Fatal(err)
	}
	if err := db2.Get("u:1", &u); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("after update: email=%q (record rewritten in the v2 layout)\n", u.Email)
}
