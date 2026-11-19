package storecompare

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/oarkflow/codec"
	"github.com/oarkflow/codec/store"
	"github.com/redis/go-redis/v9"
	"go.etcd.io/bbolt"
)

type value struct {
	ID      uint64
	Name    string
	Payload []byte
	Tags    []string
}

var sample = value{
	ID: 42, Name: "ada-lovelace",
	Payload: []byte("strata-badger-bbolt-redis-comparison-payload-0123456789"),
	Tags:    []string{"primary", "active", "benchmark"},
}

var borrowedGetSink uint64

type backend struct {
	put   func(*testing.B)
	get   func(*testing.B)
	close func() error
}

func openBackend(b *testing.B, name string) backend {
	b.Helper()
	dir := filepath.Join(b.TempDir(), name)
	key := "record:" + filepath.Base(filepath.Dir(dir)) + ":" + name
	switch name {
	case "Codec", "CodecCached", "CodecBorrowed":
		opts := &store.Options{CompactRatio: -1, ExpiryInterval: -1}
		if name == "CodecCached" {
			opts.ReadCacheBytes = 1 << 20
		}
		db, err := store.Open[value](dir, opts)
		if err != nil {
			b.Fatal(err)
		}
		return backend{
			put: func(b *testing.B) {
				if err := db.Put(key, &sample); err != nil {
					b.Fatal(err)
				}
			},
			get: func(b *testing.B) {
				if name == "CodecBorrowed" {
					if err := db.GetBorrowed(key, func(got *value) error {
						borrowedGetSink = got.ID + uint64(len(got.Name)+len(got.Payload)+len(got.Tags))
						return nil
					}); err != nil {
						b.Fatal(err)
					}
				} else {
					var got value
					if err := db.Get(key, &got); err != nil {
						b.Fatal(err)
					}
				}
			},
			close: db.Close,
		}
	case "Badger":
		db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil).WithSyncWrites(false))
		if err != nil {
			b.Fatal(err)
		}
		return backend{
			put: func(b *testing.B) {
				encoded, err := codec.MarshalEvolvable(&sample)
				if err != nil {
					b.Fatal(err)
				}
				if err := db.Update(func(txn *badger.Txn) error { return txn.Set([]byte(key), encoded) }); err != nil {
					b.Fatal(err)
				}
			},
			get: func(b *testing.B) {
				var got value
				err := db.View(func(txn *badger.Txn) error {
					item, err := txn.Get([]byte(key))
					if err != nil {
						return err
					}
					return item.Value(func(raw []byte) error { return codec.Unmarshal(raw, &got) })
				})
				if err != nil {
					b.Fatal(err)
				}
			},
			close: db.Close,
		}
	case "bbolt":
		db, err := bbolt.Open(dir+".db", 0o600, &bbolt.Options{NoSync: true})
		if err != nil {
			b.Fatal(err)
		}
		if err := db.Update(func(tx *bbolt.Tx) error { _, err := tx.CreateBucketIfNotExists([]byte("data")); return err }); err != nil {
			b.Fatal(err)
		}
		return backend{
			put: func(b *testing.B) {
				encoded, err := codec.MarshalEvolvable(&sample)
				if err != nil {
					b.Fatal(err)
				}
				if err := db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("data")).Put([]byte(key), encoded) }); err != nil {
					b.Fatal(err)
				}
			},
			get: func(b *testing.B) {
				var got value
				err := db.View(func(tx *bbolt.Tx) error {
					raw := tx.Bucket([]byte("data")).Get([]byte(key))
					if raw == nil {
						return fmt.Errorf("missing key")
					}
					return codec.Unmarshal(raw, &got)
				})
				if err != nil {
					b.Fatal(err)
				}
			},
			close: db.Close,
		}
	case "Redis":
		addr := os.Getenv("REDIS_ADDR")
		if addr == "" {
			b.Skip("set REDIS_ADDR to include the Redis service benchmark")
		}
		client := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
		ctx := context.Background()
		if err := client.Ping(ctx).Err(); err != nil {
			b.Fatalf("Redis is unavailable at REDIS_ADDR: %v", err)
		}
		return backend{
			put: func(b *testing.B) {
				encoded, err := codec.MarshalEvolvable(&sample)
				if err != nil {
					b.Fatal(err)
				}
				if err := client.Set(ctx, key, encoded, 0).Err(); err != nil {
					b.Fatal(err)
				}
			},
			get: func(b *testing.B) {
				raw, err := client.Get(ctx, key).Bytes()
				if err != nil {
					b.Fatal(err)
				}
				var got value
				if err := codec.Unmarshal(raw, &got); err != nil {
					b.Fatal(err)
				}
			},
			close: func() error {
				_ = client.Del(ctx, key).Err()
				return client.Close()
			},
		}
	default:
		b.Fatalf("unknown backend %q", name)
		return backend{}
	}
}

func BenchmarkPut(b *testing.B) {
	for _, name := range []string{"Codec", "CodecCached", "Badger", "bbolt", "Redis"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			engine := openBackend(b, name)
			defer func() { b.StopTimer(); _ = engine.close() }()
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				engine.put(b)
			}
		})
	}
}

func BenchmarkGet(b *testing.B) {
	for _, name := range []string{"Codec", "CodecBorrowed", "CodecCached", "Badger", "bbolt", "Redis"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			engine := openBackend(b, name)
			engine.put(b)
			defer func() { b.StopTimer(); _ = engine.close() }()
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				engine.get(b)
			}
		})
	}
}
