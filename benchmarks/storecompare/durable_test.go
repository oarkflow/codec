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

type durableBatchWriter struct {
	write func(*testing.B)
	close func() error
}

func openDurableBatchWriter(b *testing.B, name string, batchSize int) durableBatchWriter {
	b.Helper()
	dir := filepath.Join(b.TempDir(), name)
	keys := make([]string, batchSize)
	values := make([]value, batchSize)
	for i := range keys {
		keys[i] = fmt.Sprintf("durable:%s:record:%04d", filepath.Base(dir), i)
		values[i] = sample
		values[i].ID = uint64(i)
	}
	switch name {
	case "Codec":
		db, err := store.Open[value](dir, &store.Options{SyncAlways: true, CompactRatio: -1, ExpiryInterval: -1})
		if err != nil {
			b.Fatal(err)
		}
		batch := db.NewBatch()
		return durableBatchWriter{
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
		db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil).WithSyncWrites(true))
		if err != nil {
			b.Fatal(err)
		}
		return durableBatchWriter{
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
		db, err := bbolt.Open(dir+".db", 0o600, nil)
		if err != nil {
			b.Fatal(err)
		}
		if err := db.Update(func(tx *bbolt.Tx) error { _, err := tx.CreateBucketIfNotExists([]byte("data")); return err }); err != nil {
			b.Fatal(err)
		}
		return durableBatchWriter{
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
			b.Skip("set REDIS_ADDR to include the durable Redis benchmark")
		}
		client := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
		ctx := context.Background()
		if err := client.Ping(ctx).Err(); err != nil {
			b.Fatalf("Redis unavailable at REDIS_ADDR: %v", err)
		}
		aof, err := client.ConfigGet(ctx, "appendonly").Result()
		if err != nil {
			_ = client.Close()
			b.Skipf("Redis AOF mode cannot be verified: %v", err)
		}
		fsync, err := client.ConfigGet(ctx, "appendfsync").Result()
		if err != nil {
			_ = client.Close()
			b.Skipf("Redis AOF fsync mode cannot be verified: %v", err)
		}
		if aof["appendonly"] != "yes" || fsync["appendfsync"] != "always" {
			_ = client.Close()
			b.Skipf("Redis durable benchmark requires appendonly=yes and appendfsync=always (got %q, %q)", aof["appendonly"], fsync["appendfsync"])
		}
		return durableBatchWriter{
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
		b.Fatalf("unknown durable backend %q", name)
		return durableBatchWriter{}
	}
}

// BenchmarkDurableBatchPut compares atomic commits of several sizes with each
// backend's per-commit durability option enabled.
func BenchmarkDurableBatchPut(b *testing.B) {
	for _, batchSize := range []int{1, 100, 1000, 10000} {
		b.Run(fmt.Sprint(batchSize), func(b *testing.B) {
			for _, name := range []string{"Codec", "Badger", "bbolt", "Redis"} {
				b.Run(name, func(b *testing.B) {
					b.StopTimer()
					writer := openDurableBatchWriter(b, name, batchSize)
					defer func() { b.StopTimer(); _ = writer.close() }()
					b.ReportAllocs()
					b.ReportMetric(float64(batchSize), "records/op")
					b.ResetTimer()
					b.StartTimer()
					for i := 0; i < b.N; i++ {
						writer.write(b)
					}
				})
			}
		})
	}
}
