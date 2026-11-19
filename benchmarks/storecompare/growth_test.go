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

type uniqueBatchWriter struct {
	write func(*testing.B, int)
	close func() error
}

func openUniqueBatchWriter(b *testing.B, name string) uniqueBatchWriter {
	return openUniqueBatchWriterOrder(b, name, false)
}

func openShuffledUniqueBatchWriter(b *testing.B, name string) uniqueBatchWriter {
	return openUniqueBatchWriterOrder(b, name, true)
}

func openUniqueBatchWriterOrder(b *testing.B, name string, shuffled bool) uniqueBatchWriter {
	b.Helper()
	dir := filepath.Join(b.TempDir(), name)
	ns := "unique:" + filepath.Base(dir) + ":"
	writeBatch := func(b *testing.B, op int, put func(string, *value) error) {
		for i := 0; i < bulkBatchSize; i++ {
			v := sample
			v.ID = uint64(op*bulkBatchSize + i)
			keyIndex := i
			if shuffled {
				keyIndex = (i*537 + op*97) % bulkBatchSize
			}
			if err := put(fmt.Sprintf("%s%08d:%04d", ns, op, keyIndex), &v); err != nil {
				b.Fatal(err)
			}
		}
	}
	switch name {
	case "Codec":
		db, err := store.Open[value](dir, &store.Options{CompactRatio: -1, ExpiryInterval: -1})
		if err != nil {
			b.Fatal(err)
		}
		batch := db.NewBatch()
		return uniqueBatchWriter{
			write: func(b *testing.B, op int) {
				writeBatch(b, op, batch.Put)
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
		return uniqueBatchWriter{
			write: func(b *testing.B, op int) {
				if err := db.Update(func(txn *badger.Txn) error {
					return writeBatchErrOrder(op, ns, shuffled, func(key string, v *value) error {
						encoded, err := codec.MarshalEvolvable(v)
						if err != nil {
							return err
						}
						return txn.Set([]byte(key), encoded)
					})
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
		return uniqueBatchWriter{
			write: func(b *testing.B, op int) {
				if err := db.Update(func(tx *bbolt.Tx) error {
					bucket := tx.Bucket([]byte("data"))
					return writeBatchErrOrder(op, ns, shuffled, func(key string, v *value) error {
						encoded, err := codec.MarshalEvolvable(v)
						if err != nil {
							return err
						}
						return bucket.Put([]byte(key), encoded)
					})
				}); err != nil {
					b.Fatal(err)
				}
			},
			close: db.Close,
		}
	case "Redis":
		addr := os.Getenv("REDIS_ADDR")
		if addr == "" {
			b.Skip("set REDIS_ADDR to include the Redis unique-write benchmark")
		}
		client := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
		ctx := context.Background()
		if err := client.Ping(ctx).Err(); err != nil {
			b.Fatalf("Redis unavailable at REDIS_ADDR: %v", err)
		}
		return uniqueBatchWriter{
			write: func(b *testing.B, op int) {
				_, err := client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
					return writeBatchErrOrder(op, ns, shuffled, func(key string, v *value) error {
						encoded, err := codec.MarshalEvolvable(v)
						if err != nil {
							return err
						}
						pipe.Set(ctx, key, encoded, 0)
						return nil
					})
				})
				if err != nil {
					b.Fatal(err)
				}
			},
			close: func() error {
				var cursor uint64
				var allKeys []string
				for {
					keys, next, err := client.Scan(ctx, cursor, ns+"*", 1000).Result()
					if err != nil {
						_ = client.Close()
						return err
					}
					allKeys = append(allKeys, keys...)
					cursor = next
					if cursor == 0 {
						break
					}
				}
				for len(allKeys) > 0 {
					batch := allKeys[:min(len(allKeys), 1000)]
					if err := client.Del(ctx, batch...).Err(); err != nil {
						_ = client.Close()
						return err
					}
					allKeys = allKeys[len(batch):]
				}
				return client.Close()
			},
		}
	default:
		b.Fatalf("unknown unique-write backend %q", name)
		return uniqueBatchWriter{}
	}
}

func writeBatchErr(op int, ns string, put func(string, *value) error) error {
	return writeBatchErrOrder(op, ns, false, put)
}

func writeBatchErrOrder(op int, ns string, shuffled bool, put func(string, *value) error) error {
	for i := 0; i < bulkBatchSize; i++ {
		v := sample
		v.ID = uint64(op*bulkBatchSize + i)
		keyIndex := i
		if shuffled {
			keyIndex = (i*537 + op*97) % bulkBatchSize
		}
		if err := put(fmt.Sprintf("%s%08d:%04d", ns, op, keyIndex), &v); err != nil {
			return err
		}
	}
	return nil
}

// BenchmarkUniqueBatchPut1K inserts 1,000 new keys per atomic operation. The
// database grows throughout each sample, exposing index and log growth costs.
func BenchmarkUniqueBatchPut1K(b *testing.B) {
	benchmarkUniqueBatchPut1K(b, false)
}

// BenchmarkRandomUniqueBatchPut1K inserts 1,000 new keys per atomic batch in
// a deterministic permuted order, reducing bias toward sorted index inserts.
func BenchmarkRandomUniqueBatchPut1K(b *testing.B) {
	benchmarkUniqueBatchPut1K(b, true)
}

func benchmarkUniqueBatchPut1K(b *testing.B, shuffled bool) {
	for _, name := range []string{"Codec", "Badger", "bbolt", "Redis"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			var writer uniqueBatchWriter
			if shuffled {
				writer = openShuffledUniqueBatchWriter(b, name)
			} else {
				writer = openUniqueBatchWriter(b, name)
			}
			defer func() { b.StopTimer(); _ = writer.close() }()
			b.ReportAllocs()
			b.ReportMetric(bulkBatchSize, "records/op")
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				writer.write(b, i)
			}
		})
	}
}
