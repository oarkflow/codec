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

const bulkBatchSize = 1000

type batchWriter struct {
	write func(*testing.B)
	close func() error
}

type batchReader struct {
	read  func(*testing.B)
	close func() error
}

func openBatchWriter(b *testing.B, name string) batchWriter {
	b.Helper()
	dir := filepath.Join(b.TempDir(), name)
	ns := "bulk:" + filepath.Base(filepath.Dir(dir)) + ":"
	keys := make([]string, bulkBatchSize)
	values := make([]value, bulkBatchSize)
	for i := range keys {
		keys[i] = fmt.Sprintf("%srecord:%04d", ns, i)
		values[i] = sample
		values[i].ID = uint64(i)
	}
	switch name {
	case "Codec":
		db, err := store.Open[value](dir, &store.Options{CompactRatio: -1, ExpiryInterval: -1})
		if err != nil {
			b.Fatal(err)
		}
		batch := db.NewBatch()
		return batchWriter{
			write: func(b *testing.B) {
				for i := range keys {
					if err := batch.Put(keys[i], &values[i]); err != nil {
						b.Fatal(err)
					}
				}
				if err := batch.Commit(); err != nil {
					b.Fatal(err)
				}
			},
			close: db.Close,
		}
	case "Badger":
		db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil).WithSyncWrites(false))
		if err != nil {
			b.Fatal(err)
		}
		return batchWriter{
			write: func(b *testing.B) {
				if err := db.Update(func(txn *badger.Txn) error {
					for i := range keys {
						encoded, err := codec.MarshalEvolvable(&values[i])
						if err != nil {
							return err
						}
						if err := txn.Set([]byte(keys[i]), encoded); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
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
		return batchWriter{
			write: func(b *testing.B) {
				if err := db.Update(func(tx *bbolt.Tx) error {
					bucket := tx.Bucket([]byte("data"))
					for i := range keys {
						encoded, err := codec.MarshalEvolvable(&values[i])
						if err != nil {
							return err
						}
						if err := bucket.Put([]byte(keys[i]), encoded); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
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
			b.Fatalf("Redis unavailable at REDIS_ADDR: %v", err)
		}
		return batchWriter{
			write: func(b *testing.B) {
				_, err := client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
					for i := range keys {
						encoded, err := codec.MarshalEvolvable(&values[i])
						if err != nil {
							return err
						}
						pipe.Set(ctx, keys[i], encoded, 0)
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
			},
			close: func() error {
				_ = client.Del(ctx, keys...).Err()
				return client.Close()
			},
		}
	default:
		b.Fatalf("unknown backend %q", name)
		return batchWriter{}
	}
}

func openBatchReader(b *testing.B, name string) batchReader {
	b.Helper()
	dir := filepath.Join(b.TempDir(), name)
	ns := "many:" + name + ":"
	keys := make([]string, bulkBatchSize)
	values := make([]value, bulkBatchSize)
	for i := range keys {
		keys[i] = fmt.Sprintf("%srecord:%04d", ns, i)
		values[i] = sample
		values[i].ID = uint64(i)
	}
	consume := func(b *testing.B, got value) { pageValueSink = got.ID }
	switch name {
	case "CodecGetMany", "CodecGetManyBorrowed", "CodecGetLoop":
		db, err := store.Open[value](dir, &store.Options{CompactRatio: -1, ExpiryInterval: -1})
		if err != nil {
			b.Fatal(err)
		}
		batch := db.NewBatch()
		for i := range keys {
			if err := batch.Put(keys[i], &values[i]); err != nil {
				b.Fatal(err)
			}
		}
		if err := batch.Commit(); err != nil {
			b.Fatal(err)
		}
		return batchReader{
			read: func(b *testing.B) {
				if name == "CodecGetMany" {
					got, found, err := db.GetMany(keys)
					if err != nil {
						b.Fatal(err)
					}
					for i := range got {
						if !found[i] {
							b.Fatalf("GetMany missed %q", keys[i])
						}
						consume(b, got[i])
					}
					return
				}
				if name == "CodecGetManyBorrowed" {
					missed := false
					err := db.GetManyBorrowed(keys, func(_ string, got *value, found bool) bool {
						if !found {
							missed = true
							return false
						}
						consume(b, *got)
						return true
					})
					if err != nil {
						b.Fatal(err)
					}
					if missed {
						b.Fatal("GetManyBorrowed missed a key")
					}
					return
				}
				for _, key := range keys {
					var got value
					if err := db.Get(key, &got); err != nil {
						b.Fatal(err)
					}
					consume(b, got)
				}
			},
			close: db.Close,
		}
	case "Badger":
		db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil).WithSyncWrites(false))
		if err != nil {
			b.Fatal(err)
		}
		if err := db.Update(func(txn *badger.Txn) error {
			for i := range keys {
				encoded, err := codec.MarshalEvolvable(&values[i])
				if err != nil {
					return err
				}
				if err := txn.Set([]byte(keys[i]), encoded); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		return batchReader{
			read: func(b *testing.B) {
				err := db.View(func(txn *badger.Txn) error {
					for _, key := range keys {
						item, err := txn.Get([]byte(key))
						if err != nil {
							return err
						}
						var got value
						if err := item.Value(func(raw []byte) error { return codec.Unmarshal(raw, &got) }); err != nil {
							return err
						}
						consume(b, got)
					}
					return nil
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
		if err := db.Update(func(tx *bbolt.Tx) error {
			bucket, err := tx.CreateBucketIfNotExists([]byte("data"))
			if err != nil {
				return err
			}
			for i := range keys {
				encoded, err := codec.MarshalEvolvable(&values[i])
				if err != nil {
					return err
				}
				if err := bucket.Put([]byte(keys[i]), encoded); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		return batchReader{
			read: func(b *testing.B) {
				err := db.View(func(tx *bbolt.Tx) error {
					bucket := tx.Bucket([]byte("data"))
					for _, key := range keys {
						var got value
						if err := codec.Unmarshal(bucket.Get([]byte(key)), &got); err != nil {
							return err
						}
						consume(b, got)
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
			},
			close: db.Close,
		}
	case "RedisMGet":
		addr := os.Getenv("REDIS_ADDR")
		if addr == "" {
			b.Skip("set REDIS_ADDR to include the Redis service benchmark")
		}
		client := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
		ctx := context.Background()
		if err := client.Ping(ctx).Err(); err != nil {
			b.Fatalf("Redis unavailable at REDIS_ADDR: %v", err)
		}
		if _, err := client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			for i := range keys {
				encoded, err := codec.MarshalEvolvable(&values[i])
				if err != nil {
					return err
				}
				pipe.Set(ctx, keys[i], encoded, 0)
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		return batchReader{
			read: func(b *testing.B) {
				got, err := client.MGet(ctx, keys...).Result()
				if err != nil {
					b.Fatal(err)
				}
				for i, raw := range got {
					text, ok := raw.(string)
					if !ok {
						b.Fatalf("MGET returned %T for %q", raw, keys[i])
					}
					var v value
					if err := codec.Unmarshal([]byte(text), &v); err != nil {
						b.Fatal(err)
					}
					consume(b, v)
				}
			},
			close: func() error {
				_ = client.Del(ctx, keys...).Err()
				return client.Close()
			},
		}
	default:
		b.Fatalf("unknown backend %q", name)
		return batchReader{}
	}
}

// BenchmarkGetMany1K compares 1,000-key reads using each backend's batched
// native operation. CodecGetLoop is the per-key Get baseline.
func BenchmarkGetMany1K(b *testing.B) {
	for _, name := range []string{"CodecGetMany", "CodecGetManyBorrowed", "CodecGetLoop", "Badger", "bbolt", "RedisMGet"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			reader := openBatchReader(b, name)
			defer func() { b.StopTimer(); _ = reader.close() }()
			b.ReportAllocs()
			b.ResetTimer()
			b.ReportMetric(bulkBatchSize, "records/op")
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				reader.read(b)
			}
		})
	}
}

// BenchmarkBatchPut1K writes the same 1,000-key batch per operation so store
// growth stays bounded. Encoding, transaction creation and commit are timed.
// Durability settings match the other non-fsync benchmarks in this module.
func BenchmarkBatchPut1K(b *testing.B) {
	for _, name := range []string{"Codec", "Badger", "bbolt", "Redis"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			writer := openBatchWriter(b, name)
			defer func() { b.StopTimer(); _ = writer.close() }()
			b.ReportAllocs()
			b.ResetTimer()
			b.ReportMetric(bulkBatchSize, "records/op")
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				writer.write(b)
			}
		})
	}
}
