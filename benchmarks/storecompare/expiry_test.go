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
)

const ttlBenchmarkDuration = time.Hour

type ttlWriter struct {
	put   func(*testing.B)
	close func() error
}

type ttlBatchWriter struct {
	put   func(*testing.B)
	close func() error
}

type ttlGrowthWriter struct {
	put   func(*testing.B, int)
	close func() error
}

type expiredReader struct {
	read  func(*testing.B)
	close func() error
}

func openTTLWriter(b *testing.B, name string) ttlWriter {
	b.Helper()
	dir := filepath.Join(b.TempDir(), name)
	key := "ttl:benchmark:record"
	switch name {
	case "Codec":
		db, err := store.Open[value](dir, &store.Options{CompactRatio: -1, ExpiryInterval: -1})
		if err != nil {
			b.Fatal(err)
		}
		return ttlWriter{
			put: func(b *testing.B) {
				if err := db.PutTTL(key, &sample, ttlBenchmarkDuration); err != nil {
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
		return ttlWriter{
			put: func(b *testing.B) {
				encoded, err := codec.MarshalEvolvable(&sample)
				if err != nil {
					b.Fatal(err)
				}
				entry := badger.NewEntry([]byte(key), encoded).WithTTL(ttlBenchmarkDuration)
				if err := db.Update(func(txn *badger.Txn) error { return txn.SetEntry(entry) }); err != nil {
					b.Fatal(err)
				}
			},
			close: db.Close,
		}
	case "Redis":
		addr := os.Getenv("REDIS_ADDR")
		if addr == "" {
			b.Skip("set REDIS_ADDR to include the Redis TTL benchmark")
		}
		client := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
		ctx := context.Background()
		if err := client.Ping(ctx).Err(); err != nil {
			b.Fatalf("Redis unavailable at REDIS_ADDR: %v", err)
		}
		return ttlWriter{
			put: func(b *testing.B) {
				encoded, err := codec.MarshalEvolvable(&sample)
				if err != nil {
					b.Fatal(err)
				}
				if err := client.Set(ctx, key, encoded, ttlBenchmarkDuration).Err(); err != nil {
					b.Fatal(err)
				}
			},
			close: func() error {
				_ = client.Del(ctx, key).Err()
				return client.Close()
			},
		}
	default:
		b.Fatalf("unknown TTL backend %q", name)
		return ttlWriter{}
	}
}

// BenchmarkPutTTL compares repeated overwrites of one value with a one-hour
// expiry. Setup and close are outside the timed region; all backends use
// non-fsync write settings. bbolt is excluded because it has no native TTL.
func BenchmarkPutTTL(b *testing.B) {
	for _, name := range []string{"Codec", "Badger", "Redis"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			writer := openTTLWriter(b, name)
			defer func() { b.StopTimer(); _ = writer.close() }()
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				writer.put(b)
			}
		})
	}
}

func openTTLBatchWriter(b *testing.B, name string) ttlBatchWriter {
	b.Helper()
	dir := filepath.Join(b.TempDir(), name)
	keys := make([]string, bulkBatchSize)
	values := make([]value, bulkBatchSize)
	for i := range keys {
		keys[i] = "ttl:batch:" + fmt.Sprintf("record:%04d", i)
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
		return ttlBatchWriter{
			put: func(b *testing.B) {
				for i := range keys {
					if err := batch.PutTTL(keys[i], &values[i], ttlBenchmarkDuration); err != nil {
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
		return ttlBatchWriter{
			put: func(b *testing.B) {
				if err := db.Update(func(txn *badger.Txn) error {
					for i := range keys {
						encoded, err := codec.MarshalEvolvable(&values[i])
						if err != nil {
							return err
						}
						entry := badger.NewEntry([]byte(keys[i]), encoded).WithTTL(ttlBenchmarkDuration)
						if err := txn.SetEntry(entry); err != nil {
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
			b.Skip("set REDIS_ADDR to include the Redis TTL batch benchmark")
		}
		client := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
		ctx := context.Background()
		if err := client.Ping(ctx).Err(); err != nil {
			b.Fatalf("Redis unavailable at REDIS_ADDR: %v", err)
		}
		return ttlBatchWriter{
			put: func(b *testing.B) {
				_, err := client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
					for i := range keys {
						encoded, err := codec.MarshalEvolvable(&values[i])
						if err != nil {
							return err
						}
						pipe.Set(ctx, keys[i], encoded, ttlBenchmarkDuration)
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
		b.Fatalf("unknown TTL backend %q", name)
		return ttlBatchWriter{}
	}
}

// BenchmarkPutTTL1K compares atomic writes of 1,000 keys with one-hour TTLs.
// The same key set is overwritten each operation so expiry scheduling remains
// at a steady 1,000 live entries. bbolt has no native TTL and is excluded.
func BenchmarkPutTTL1K(b *testing.B) {
	for _, name := range []string{"Codec", "Badger", "Redis"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			writer := openTTLBatchWriter(b, name)
			defer func() { b.StopTimer(); _ = writer.close() }()
			b.ReportAllocs()
			b.ReportMetric(bulkBatchSize, "records/op")
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				writer.put(b)
			}
		})
	}
}

// BenchmarkUniquePutTTL1K inserts 1,000 fresh keys with one-hour TTLs per
// operation, exposing the cost of a growing expiry index without waiting for
// expiration. The dataset grows for the duration of each sample.
func BenchmarkUniquePutTTL1K(b *testing.B) {
	for _, name := range []string{"Codec", "Badger", "Redis"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			writer := openUniqueTTLBatchWriter(b, name)
			defer func() { b.StopTimer(); _ = writer.close() }()
			b.ReportAllocs()
			b.ReportMetric(bulkBatchSize, "records/op")
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				writer.put(b, i)
			}
		})
	}
}

func openUniqueTTLBatchWriter(b *testing.B, name string) ttlGrowthWriter {
	b.Helper()
	dir := filepath.Join(b.TempDir(), name)
	ns := "ttl-growth:" + filepath.Base(dir) + ":"
	writeBatch := func(op int, put func(string, *value) error) error {
		for i := 0; i < bulkBatchSize; i++ {
			v := sample
			v.ID = uint64(op*bulkBatchSize + i)
			key := fmt.Sprintf("%s%08d:%04d", ns, op, i)
			if err := put(key, &v); err != nil {
				return err
			}
		}
		return nil
	}
	switch name {
	case "Codec":
		db, err := store.Open[value](dir, &store.Options{CompactRatio: -1, ExpiryInterval: -1})
		if err != nil {
			b.Fatal(err)
		}
		batch := db.NewBatch()
		return ttlGrowthWriter{put: func(b *testing.B, op int) {
			if err := writeBatch(op, func(key string, v *value) error { return batch.PutTTL(key, v, ttlBenchmarkDuration) }); err != nil {
				b.Fatal(err)
			}
			if err := batch.Commit(); err != nil {
				b.Fatal(err)
			}
		}, close: db.Close}
	case "Badger":
		db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil).WithSyncWrites(false))
		if err != nil {
			b.Fatal(err)
		}
		return ttlGrowthWriter{put: func(b *testing.B, op int) {
			if err := db.Update(func(txn *badger.Txn) error {
				return writeBatch(op, func(key string, v *value) error {
					encoded, err := codec.MarshalEvolvable(v)
					if err != nil {
						return err
					}
					return txn.SetEntry(badger.NewEntry([]byte(key), encoded).WithTTL(ttlBenchmarkDuration))
				})
			}); err != nil {
				b.Fatal(err)
			}
		}, close: db.Close}
	case "Redis":
		addr := os.Getenv("REDIS_ADDR")
		if addr == "" {
			b.Skip("set REDIS_ADDR to include the Redis TTL growth benchmark")
		}
		client := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
		ctx := context.Background()
		if err := client.Ping(ctx).Err(); err != nil {
			b.Fatalf("Redis unavailable at REDIS_ADDR: %v", err)
		}
		return ttlGrowthWriter{put: func(b *testing.B, op int) {
			_, err := client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				return writeBatch(op, func(key string, v *value) error {
					encoded, err := codec.MarshalEvolvable(v)
					if err != nil {
						return err
					}
					pipe.Set(ctx, key, encoded, ttlBenchmarkDuration)
					return nil
				})
			})
			if err != nil {
				b.Fatal(err)
			}
		}, close: func() error {
			var cursor uint64
			for {
				keys, next, err := client.Scan(ctx, cursor, ns+"*", 1000).Result()
				if err != nil {
					_ = client.Close()
					return err
				}
				if len(keys) > 0 {
					if err := client.Del(ctx, keys...).Err(); err != nil {
						_ = client.Close()
						return err
					}
				}
				cursor = next
				if cursor == 0 {
					break
				}
			}
			return client.Close()
		}}
	default:
		b.Fatalf("unknown TTL growth backend %q", name)
		return ttlGrowthWriter{}
	}
}

func openExpiredReader(b *testing.B, name string) expiredReader {
	b.Helper()
	dir := filepath.Join(b.TempDir(), name)
	ns := "expired:" + filepath.Base(dir) + ":"
	ttl := time.Second
	keys := make([]string, bulkBatchSize)
	values := make([]value, bulkBatchSize)
	for i := range keys {
		keys[i] = fmt.Sprintf("%srecord:%04d", ns, i)
		values[i] = sample
		values[i].ID = uint64(i)
	}
	switch name {
	case "CodecGetMany", "CodecGetManyBorrowed":
		db, err := store.Open[value](dir, &store.Options{CompactRatio: -1, ExpiryInterval: -1})
		if err != nil {
			b.Fatal(err)
		}
		batch := db.NewBatch()
		for i := range keys {
			if err := batch.PutTTL(keys[i], &values[i], ttl); err != nil {
				b.Fatal(err)
			}
		}
		if err := batch.Commit(); err != nil {
			b.Fatal(err)
		}
		return expiredReader{
			read: func(b *testing.B) {
				count := 0
				if name == "CodecGetManyBorrowed" {
					err := db.GetManyBorrowed(keys, func(_ string, _ *value, found bool) bool {
						if found {
							count++
						}
						return true
					})
					if err != nil {
						b.Fatal(err)
					}
				} else {
					_, found, err := db.GetMany(keys)
					if err != nil {
						b.Fatal(err)
					}
					for _, ok := range found {
						if ok {
							count++
						}
					}
				}
				if count != 0 {
					b.Fatalf("Codec still found %d expired keys", count)
				}
				expiredReadSink = count
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
				if err := txn.SetEntry(badger.NewEntry([]byte(keys[i]), encoded).WithTTL(ttl)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		return expiredReader{
			read: func(b *testing.B) {
				found := 0
				if err := db.View(func(txn *badger.Txn) error {
					for _, key := range keys {
						if _, err := txn.Get([]byte(key)); err == nil {
							found++
						} else if err != badger.ErrKeyNotFound {
							return err
						}
					}
					return nil
				}); err != nil {
					b.Fatal(err)
				}
				if found != 0 {
					b.Fatalf("Badger still found %d expired keys", found)
				}
				expiredReadSink = found
			},
			close: db.Close,
		}
	case "Redis":
		addr := os.Getenv("REDIS_ADDR")
		if addr == "" {
			b.Skip("set REDIS_ADDR to include the Redis expired-read benchmark")
		}
		client := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
		ctx := context.Background()
		if err := client.Ping(ctx).Err(); err != nil {
			b.Fatalf("Redis unavailable at REDIS_ADDR: %v", err)
		}
		_, err := client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			for i := range keys {
				encoded, err := codec.MarshalEvolvable(&values[i])
				if err != nil {
					return err
				}
				pipe.Set(ctx, keys[i], encoded, ttl)
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
		return expiredReader{
			read: func(b *testing.B) {
				got, err := client.MGet(ctx, keys...).Result()
				if err != nil {
					b.Fatal(err)
				}
				found := 0
				for _, value := range got {
					if value != nil {
						found++
					}
				}
				if found != 0 {
					b.Fatalf("Redis still found %d expired keys", found)
				}
				expiredReadSink = found
			},
			close: func() error {
				_ = client.Del(ctx, keys...).Err()
				return client.Close()
			},
		}
	default:
		b.Fatalf("unknown expired-read backend %q", name)
		return expiredReader{}
	}
}

var expiredReadSink int

// BenchmarkExpiredGetMany1K checks 1,000 keys after a one-second TTL has
// elapsed. Setup and expiration wait are outside timing.
func BenchmarkExpiredGetMany1K(b *testing.B) {
	for _, name := range []string{"CodecGetMany", "CodecGetManyBorrowed", "Badger", "Redis"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			reader := openExpiredReader(b, name)
			defer func() { b.StopTimer(); _ = reader.close() }()
			time.Sleep(time.Second + 250*time.Millisecond)
			b.ReportAllocs()
			b.ReportMetric(bulkBatchSize, "records/op")
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				reader.read(b)
			}
		})
	}
}
