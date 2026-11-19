// store: persistent typed storage with updates and deletes, plus the text
// forms (base64 armor and the JSON bridge).
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/oarkflow/codec"
	"github.com/oarkflow/codec/store"
)

type Account struct {
	Owner   string
	Balance int64
	Tags    []string
}

func main() {
	dir, _ := os.MkdirTemp("", "strata")
	defer os.RemoveAll(dir)

	db, err := store.Open[Account](filepath.Join(dir, "accounts"), nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	db.Put("acc:1", &Account{"ada", 100, []string{"vip"}})
	db.Put("acc:2", &Account{"bob", 50, nil})

	// atomic read-modify-write
	db.Update("acc:1", func(a *Account, exists bool) error {
		a.Balance += 25
		return nil
	})
	// atomic multi-key change: all or nothing, even across a crash
	b := db.NewBatch()
	b.Put("acc:3", &Account{"cy", 5, nil})
	if err := b.Delete("acc:2"); err != nil {
		log.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		log.Fatal(err)
	}

	// wildcard queries: sorted, and only keys under the literal prefix are visited
	for i := 1; i <= 3; i++ {
		db.Put(fmt.Sprintf("session:u%d:web", i), &Account{Owner: fmt.Sprint("u", i)})
		db.Put(fmt.Sprintf("session:u%d:app", i), &Account{Owner: fmt.Sprint("u", i)})
	}
	web, _ := db.Match("session:*:web") // * any run, ? one char, [a-c] sets, [^x] negation
	u2, _ := db.Match("session:u2:*")   // literal prefix: cheap even with millions of keys
	n, _ := db.Count("session:u[12]:*") // no values are read
	fmt.Println("web:", web, "u2:", u2, "u1+u2:", n)
	db.Scan("session:u3:*", func(k string, a *Account) bool { fmt.Println("  scan", k, a.Owner); return true })
	gone, _ := db.DeleteMatch("session:*") // atomic batches
	fmt.Println("deleted", gone, "sessions")

	var a Account
	db.Get("acc:1", &a)
	fmt.Println("acc:1 =", a, "keys =", db.Keys())

	// text forms: readable notation, and base64 armor for opaque tokens
	txt, _ := codec.MarshalText(&a)
	b64, _ := codec.MarshalBase64(&a)
	fmt.Printf("text (%d B):\n%sbase64 (%d B): %s\n", len(txt), txt, len(b64), b64)
	var back Account
	codec.UnmarshalText([]byte("owner Ada; balance 125; tags vip"), &back) // type it by hand (names are case-insensitive)
	fmt.Println("parsed by hand:", back)
}

func mustMarshal(v any) []byte { b, _ := codec.Marshal(v); return b }
