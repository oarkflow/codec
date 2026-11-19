package store

import (
	"errors"
	"testing"
	"time"

	"github.com/oarkflow/codec"
)

func TestReadCacheInvalidationAndExpiry(t *testing.T) {
	db, _ := open(t, &Options{ReadCacheBytes: 1 << 10, CompactRatio: -1, ExpiryInterval: -1})
	defer db.Close()
	first := User{Name: "first", Tags: []string{"original"}}
	if err := db.Put("key", &first); err != nil {
		t.Fatal(err)
	}
	var got User
	if err := db.Get("key", &got); err != nil || got.Name != "first" {
		t.Fatalf("initial Get = %+v, %v", got, err)
	}
	if s := db.Stats(); s.CachedEntries != 1 || s.CachedBytes == 0 {
		t.Fatalf("cache stats = %+v", s)
	}
	if err := db.View("key", func(raw []byte) error { raw[0] ^= 0xff; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := db.Get("key", &got); err != nil || got.Name != "first" {
		t.Fatalf("View changed cached value: %+v, %v", got, err)
	}

	second := User{Name: "second"}
	if err := db.Put("key", &second); err != nil {
		t.Fatal(err)
	}
	if err := db.Get("key", &got); err != nil || got.Name != "second" {
		t.Fatalf("cache was not invalidated on Put: %+v, %v", got, err)
	}
	if err := db.Delete("key"); err != nil {
		t.Fatal(err)
	}
	if err := db.Get("key", &got); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v", err)
	}

	ttl := User{Name: "soon"}
	if err := db.PutTTL("ttl", &ttl, 15*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := db.Get("ttl", &got); err != nil {
		t.Fatal(err)
	} // warm the cache
	time.Sleep(25 * time.Millisecond)
	if err := db.Get("ttl", &got); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired cached value remained visible: %v", err)
	}
}

func TestReadCacheRespectsByteLimit(t *testing.T) {
	v := User{Name: "payload sized for one cache entry"}
	raw, err := codec.MarshalEvolvable(&v)
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(len("a") + len(raw) + 1)
	db, _ := open(t, &Options{ReadCacheBytes: limit, CompactRatio: -1, ExpiryInterval: -1})
	defer db.Close()
	if err := db.Put("a", &v); err != nil {
		t.Fatal(err)
	}
	if err := db.Put("b", &v); err != nil {
		t.Fatal(err)
	}
	var got User
	if err := db.Get("a", &got); err != nil {
		t.Fatal(err)
	}
	if err := db.Get("b", &got); err != nil {
		t.Fatal(err)
	}
	s := db.Stats()
	if s.CachedBytes > limit || s.CachedEntries != 1 {
		t.Fatalf("cache exceeded its byte limit: %+v, limit %d", s, limit)
	}
}
