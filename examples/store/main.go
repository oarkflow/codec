// store: persistent typed storage with atomic updates, expiry, keyset pages,
// bounded scans, streaming, and a temporary-store example.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/oarkflow/codec"
	"github.com/oarkflow/codec/store"
)

type Account struct {
	Owner   string
	Balance int64
	Tags    []string
}

func main() {
	dir, err := os.MkdirTemp("", "codec-store-example")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := store.Open[Account](filepath.Join(dir, "accounts"), nil)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { must(db.Close()) }()

	// Basic writes and an atomic read-modify-write.
	must(db.Put("acc:1", &Account{"ada", 100, []string{"vip"}}))
	must(db.Put("acc:2", &Account{"bob", 50, nil}))
	must(db.Update("acc:1", func(a *Account, exists bool) error {
		if !exists {
			return fmt.Errorf("account disappeared")
		}
		a.Balance += 25
		return nil
	}))

	// A batch makes related changes visible atomically.
	b := db.NewBatch()
	b.Put("acc:3", &Account{"cy", 5, nil})
	must(b.Delete("acc:2"))
	must(b.Commit())

	// TTL values behave as missing after expiry; RemainingTTL reports the
	// remaining lifetime while the key is live.
	must(db.PutTTL("session:ada", &Account{Owner: "ada"}, time.Hour))
	remaining, err := db.RemainingTTL("session:ada")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("session TTL:", remaining.Round(time.Second))

	// Wildcard queries are sorted. Only the literal prefix before the first
	// wildcard is visited.
	for i := 1; i <= 3; i++ {
		owner := fmt.Sprint("u", i)
		must(db.Put("session:"+owner+":web", &Account{Owner: owner}))
		must(db.Put("session:"+owner+":app", &Account{Owner: owner}))
	}
	web, err := db.Match("session:*:web") // * any run, ? one char, [a-c] sets
	if err != nil {
		log.Fatal(err)
	}
	u2, err := db.Match("session:u2:*") // literal prefix: visits only user 2
	if err != nil {
		log.Fatal(err)
	}
	n, err := db.Count("session:u[12]:*") // counts without decoding values
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("web:", web, "u2:", u2, "u1+u2:", n)

	// Page uses an exclusive, pattern-bound cursor. Each page has its own
	// stable read view; concurrent writes can appear between pages.
	after := ""
	for {
		items, next, err := db.Page("session:*", after, 2)
		if err != nil {
			log.Fatal(err)
		}
		for _, item := range items {
			fmt.Printf("  page %s -> %s\n", item.Key, item.Value.Owner)
		}
		if next == "" {
			break
		}
		after = next
	}

	// TakeRange visits a sorted half-open key range in bounded chunks. Its
	// callback can call the DB; the decoded value is reused between calls.
	must(db.TakeRange("session:u1:", "session:u2:", 10, func(key string, a *Account) bool {
		fmt.Println("  range", key, a.Owner)
		return true
	}))

	// StreamResults carries terminal scan errors as well as owned items.
	for result := range db.StreamResults(context.Background(), "session:u3:*") {
		if result.Err != nil {
			log.Fatal(result.Err)
		}
		fmt.Println("  stream", result.Item.Key, result.Item.Value.Owner)
	}

	deleted, err := db.DeleteMatch("session:u[12]:*")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("deleted sessions:", deleted)

	var a Account
	must(db.Get("acc:1", &a))
	fmt.Println("acc:1 =", a, "keys =", db.KeysSnapshot())

	// Temporary stores remove their files on Close; use Open for data that
	// should survive process exit.
	temp, err := store.OpenTemp[Account](nil)
	if err != nil {
		log.Fatal(err)
	}
	must(temp.Put("scratch", &Account{Owner: "temporary"}))
	must(temp.Close())

	// Text is readable and Base64 is convenient for opaque tokens.
	txt, err := codec.MarshalText(&a)
	if err != nil {
		log.Fatal(err)
	}
	b64, err := codec.MarshalBase64(&a)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("text (%d B):\n%sbase64 (%d B): %s\n", len(txt), txt, len(b64), b64)
	var back Account
	must(codec.UnmarshalText([]byte("owner Ada; balance 125; tags vip"), &back))
	fmt.Println("parsed by hand:", back)
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
