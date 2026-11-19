package storecompare

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/oarkflow/codec"
	"github.com/oarkflow/codec/store"
	"github.com/redis/go-redis/v9"
	"go.etcd.io/bbolt"
)

const datasetSize = 50_000

var rangeKeySink string
var rangeKeyByteSink byte
var pageValueSink uint64
var datasetValueSink atomic.Uint64

type datasetBackend struct {
	get               func(*testing.B, int)
	parallelGet       func(int) error
	scanPrefix        func(*testing.B) int
	scanKeysAll       func(*testing.B) int
	keysAll           func(*testing.B) int
	scanAll           func(*testing.B) int
	scanAllNoCopy     func(*testing.B) int
	scanAllBorrowed   func(*testing.B) int
	takeRangeAll      func(*testing.B) int
	pageAll           func(*testing.B) int
	streamAll         func(*testing.B) int
	streamResultAll   func(*testing.B) int
	streamBorrowedAll func(*testing.B) int
	stats             func() store.Stats
	close             func() error
}

func datasetKey(ns string, i int) string {
	return fmt.Sprintf("%suser:%04d:field:%d", ns, i/10, i%10)
}

func datasetPrefix(ns string) string { return ns + "user:0042:" }

func openDataset(b *testing.B, name string) datasetBackend {
	b.Helper()
	dir := filepath.Join(b.TempDir(), name)
	ns := "bench:" + filepath.Base(filepath.Dir(dir)) + ":"
	prefix := datasetPrefix(ns)
	valueFor := func(i int) value {
		v := sample
		v.ID = uint64(i)
		return v
	}
	switch name {
	case "Codec", "CodecBorrowed", "CodecCached", "CodecSnapshot", "CodecSnapshotNoCopy", "CodecSnapshotBorrowed", "CodecTakeBorrowed", "CodecStreamBorrowed":
		opts := &store.Options{CompactRatio: -1, ExpiryInterval: -1, SegmentSize: 1 << 20}
		if name == "CodecCached" {
			opts.ReadCacheBytes = 8 << 20
		}
		db, err := store.Open[value](dir, opts)
		if err != nil {
			b.Fatal(err)
		}
		batch := db.NewBatch()
		for i := 0; i < datasetSize; i++ {
			v := valueFor(i)
			if err := batch.Put(datasetKey(ns, i), &v); err != nil {
				b.Fatal(err)
			}
			if batch.Len() == 1000 {
				if err := batch.Commit(); err != nil {
					b.Fatal(err)
				}
			}
		}
		if err := batch.Commit(); err != nil {
			b.Fatal(err)
		}
		return datasetBackend{
			takeRangeAll: func(b *testing.B) int {
				n := 0
				err := db.TakeRange(ns+"user:", ns+"user;", datasetSize, func(key string, got *value) bool {
					rangeKeySink = key
					pageValueSink = got.ID
					n++
					return true
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			get: func(b *testing.B, i int) {
				key := datasetKey(ns, i)
				if name == "CodecBorrowed" {
					if err := db.GetBorrowed(key, func(got *value) error {
						pageValueSink = got.ID
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
			parallelGet: func(i int) error {
				key := datasetKey(ns, i)
				if name == "CodecBorrowed" {
					return db.GetBorrowed(key, func(got *value) error {
						datasetValueSink.Store(got.ID)
						return nil
					})
				}
				var got value
				return db.Get(key, &got)
			},
			pageAll: func(b *testing.B) int {
				n := 0
				after := ""
				for {
					items, next, err := db.Page("*", after, 256)
					if err != nil {
						b.Fatal(err)
					}
					for _, item := range items {
						rangeKeySink = item.Key
						pageValueSink = item.Value.ID
						n++
					}
					if next == "" {
						return n
					}
					after = next
				}
			},
			streamAll: func(b *testing.B) int {
				n := 0
				for item := range db.Stream(context.Background(), "*") {
					rangeKeySink = item.Key
					pageValueSink = item.Value.ID
					n++
				}
				return n
			},
			streamResultAll: func(b *testing.B) int {
				n := 0
				for result := range db.StreamResults(context.Background(), "*") {
					if result.Err != nil {
						b.Fatal(result.Err)
					}
					rangeKeySink = result.Item.Key
					pageValueSink = result.Item.Value.ID
					n++
				}
				return n
			},
			streamBorrowedAll: func(b *testing.B) int {
				n := 0
				err := db.StreamBorrowed(context.Background(), "*", func(key []byte, got *value) bool {
					if len(key) > 0 {
						rangeKeyByteSink = key[0]
					}
					pageValueSink = got.ID
					n++
					return true
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			scanAllBorrowed: func(b *testing.B) int {
				n := 0
				visit := func(key []byte, got *value) bool {
					if len(key) > 0 {
						rangeKeyByteSink = key[0]
					}
					pageValueSink = got.ID
					n++
					return true
				}
				var err error
				if name == "CodecTakeBorrowed" {
					err = db.TakeRangeBorrowed(ns+"user:", ns+"user;", datasetSize, visit)
				} else {
					err = db.RangeSnapshotBorrowed(visit)
				}
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			scanAllNoCopy: func(b *testing.B) int {
				n := 0
				if err := db.RangeSnapshotNoCopy(func(key string, _ *value) bool { rangeKeySink = key; n++; return true }); err != nil {
					b.Fatal(err)
				}
				return n
			},
			scanAll: func(b *testing.B) int {
				n := 0
				visit := func(key string, _ *value) bool { rangeKeySink = key; n++; return true }
				var err error
				if name == "CodecSnapshotNoCopy" {
					err = db.RangeSnapshotNoCopy(visit)
				} else if name == "CodecSnapshot" {
					err = db.RangeSnapshot(visit)
				} else {
					err = db.Range(visit)
				}
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			scanKeysAll: func(b *testing.B) int {
				n := 0
				if err := db.ScanKeys("*", func(key string) bool {
					rangeKeySink = key
					n++
					return true
				}); err != nil {
					b.Fatal(err)
				}
				return n
			},
			keysAll: func(b *testing.B) int {
				keys := db.KeysSnapshot()
				if len(keys) > 0 {
					rangeKeySink = keys[len(keys)-1]
				}
				return len(keys)
			},
			scanPrefix: func(b *testing.B) int {
				n := 0
				if err := db.ScanPrefix(prefix, func(_ string, _ *value) bool { n++; return true }); err != nil {
					b.Fatal(err)
				}
				return n
			},
			stats: db.Stats,
			close: db.Close,
		}
	case "Badger":
		db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil).WithSyncWrites(false))
		if err != nil {
			b.Fatal(err)
		}
		if err := db.Update(func(txn *badger.Txn) error {
			for i := 0; i < datasetSize; i++ {
				v := valueFor(i)
				encoded, err := codec.MarshalEvolvable(&v)
				if err != nil {
					return err
				}
				if err := txn.Set([]byte(datasetKey(ns, i)), encoded); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		return datasetBackend{
			takeRangeAll: func(b *testing.B) int {
				n := 0
				start, end := []byte(ns+"user:"), []byte(ns+"user;")
				var got value
				err := db.View(func(txn *badger.Txn) error {
					it := txn.NewIterator(badger.DefaultIteratorOptions)
					defer it.Close()
					for it.Seek(start); it.Valid(); it.Next() {
						item := it.Item()
						if bytes.Compare(item.Key(), end) >= 0 {
							break
						}
						rangeKeySink = string(item.Key())
						if err := item.Value(func(raw []byte) error { return codec.Unmarshal(raw, &got) }); err != nil {
							return err
						}
						pageValueSink = got.ID
						n++
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			get: func(b *testing.B, i int) {
				var got value
				err := db.View(func(txn *badger.Txn) error {
					item, err := txn.Get([]byte(datasetKey(ns, i)))
					if err != nil {
						return err
					}
					return item.Value(func(raw []byte) error { return codec.Unmarshal(raw, &got) })
				})
				if err != nil {
					b.Fatal(err)
				}
			},
			parallelGet: func(i int) error {
				var got value
				return db.View(func(txn *badger.Txn) error {
					item, err := txn.Get([]byte(datasetKey(ns, i)))
					if err != nil {
						return err
					}
					return item.Value(func(raw []byte) error { return codec.Unmarshal(raw, &got) })
				})
			},
			pageAll: func(b *testing.B) int {
				n := 0
				page := make([]store.Item[value], 0, 256)
				flush := func() {
					for _, item := range page {
						pageValueSink = item.Value.ID
						rangeKeySink = item.Key
						n++
					}
					page = page[:0]
				}
				err := db.View(func(txn *badger.Txn) error {
					it := txn.NewIterator(badger.DefaultIteratorOptions)
					defer it.Close()
					for it.Rewind(); it.Valid(); it.Next() {
						item := it.Item()
						page = append(page, store.Item[value]{Key: string(item.Key())})
						dst := &page[len(page)-1].Value
						if err := item.Value(func(raw []byte) error { return codec.Unmarshal(raw, dst) }); err != nil {
							return err
						}
						if len(page) == cap(page) {
							flush()
						}
					}
					flush()
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			scanAll: func(b *testing.B) int {
				n := 0
				var got value
				err := db.View(func(txn *badger.Txn) error {
					opts := badger.DefaultIteratorOptions
					it := txn.NewIterator(opts)
					defer it.Close()
					for it.Rewind(); it.Valid(); it.Next() {
						rangeKeySink = string(it.Item().Key())
						if err := it.Item().Value(func(raw []byte) error { return codec.Unmarshal(raw, &got) }); err != nil {
							return err
						}
						n++
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			scanKeysAll: func(b *testing.B) int {
				n := 0
				err := db.View(func(txn *badger.Txn) error {
					it := txn.NewIterator(badger.DefaultIteratorOptions)
					defer it.Close()
					for it.Rewind(); it.Valid(); it.Next() {
						rangeKeySink = string(it.Item().Key())
						n++
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			scanAllNoCopy: func(b *testing.B) int {
				n := 0
				var got value
				err := db.View(func(txn *badger.Txn) error {
					it := txn.NewIterator(badger.DefaultIteratorOptions)
					defer it.Close()
					for it.Rewind(); it.Valid(); it.Next() {
						rangeKeySink = string(it.Item().Key())
						if err := it.Item().Value(func(raw []byte) error { return codec.UnmarshalNoCopy(raw, &got) }); err != nil {
							return err
						}
						n++
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			scanAllBorrowed: func(b *testing.B) int {
				n := 0
				var got value
				keyBuf := make([]byte, 0, 64)
				err := db.View(func(txn *badger.Txn) error {
					it := txn.NewIterator(badger.DefaultIteratorOptions)
					defer it.Close()
					for it.Rewind(); it.Valid(); it.Next() {
						key := it.Item().KeyCopy(keyBuf[:0])
						if len(key) > 0 {
							rangeKeyByteSink = key[0]
						}
						if err := it.Item().Value(func(raw []byte) error { return codec.UnmarshalNoCopy(raw, &got) }); err != nil {
							return err
						}
						n++
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			scanPrefix: func(b *testing.B) int {
				n := 0
				err := db.View(func(txn *badger.Txn) error {
					opts := badger.DefaultIteratorOptions
					opts.Prefix = []byte(prefix)
					it := txn.NewIterator(opts)
					defer it.Close()
					for it.Rewind(); it.Valid(); it.Next() {
						var got value
						if err := it.Item().Value(func(raw []byte) error { return codec.Unmarshal(raw, &got) }); err != nil {
							return err
						}
						n++
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
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
		if err := db.Update(func(tx *bbolt.Tx) error {
			bucket := tx.Bucket([]byte("data"))
			for i := 0; i < datasetSize; i++ {
				v := valueFor(i)
				encoded, err := codec.MarshalEvolvable(&v)
				if err != nil {
					return err
				}
				if err := bucket.Put([]byte(datasetKey(ns, i)), encoded); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		return datasetBackend{
			takeRangeAll: func(b *testing.B) int {
				n := 0
				start, end := []byte(ns+"user:"), []byte(ns+"user;")
				var got value
				err := db.View(func(tx *bbolt.Tx) error {
					c := tx.Bucket([]byte("data")).Cursor()
					for k, raw := c.Seek(start); k != nil; k, raw = c.Next() {
						if bytes.Compare(k, end) >= 0 {
							break
						}
						rangeKeySink = string(k)
						if err := codec.Unmarshal(raw, &got); err != nil {
							return err
						}
						pageValueSink = got.ID
						n++
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			get: func(b *testing.B, i int) {
				var got value
				err := db.View(func(tx *bbolt.Tx) error {
					raw := tx.Bucket([]byte("data")).Get([]byte(datasetKey(ns, i)))
					if raw == nil {
						return fmt.Errorf("missing key")
					}
					return codec.Unmarshal(raw, &got)
				})
				if err != nil {
					b.Fatal(err)
				}
			},
			parallelGet: func(i int) error {
				var got value
				return db.View(func(tx *bbolt.Tx) error {
					raw := tx.Bucket([]byte("data")).Get([]byte(datasetKey(ns, i)))
					if raw == nil {
						return fmt.Errorf("missing key")
					}
					return codec.Unmarshal(raw, &got)
				})
			},
			pageAll: func(b *testing.B) int {
				n := 0
				page := make([]store.Item[value], 0, 256)
				flush := func() {
					for _, item := range page {
						pageValueSink = item.Value.ID
						rangeKeySink = item.Key
						n++
					}
					page = page[:0]
				}
				err := db.View(func(tx *bbolt.Tx) error {
					c := tx.Bucket([]byte("data")).Cursor()
					for k, raw := c.First(); k != nil; k, raw = c.Next() {
						page = append(page, store.Item[value]{Key: string(k)})
						dst := &page[len(page)-1].Value
						if err := codec.Unmarshal(raw, dst); err != nil {
							return err
						}
						if len(page) == cap(page) {
							flush()
						}
					}
					flush()
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			scanAll: func(b *testing.B) int {
				n := 0
				var got value
				err := db.View(func(tx *bbolt.Tx) error {
					c := tx.Bucket([]byte("data")).Cursor()
					for k, raw := c.First(); k != nil; k, raw = c.Next() {
						rangeKeySink = string(k)
						if err := codec.Unmarshal(raw, &got); err != nil {
							return err
						}
						n++
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			scanKeysAll: func(b *testing.B) int {
				n := 0
				err := db.View(func(tx *bbolt.Tx) error {
					c := tx.Bucket([]byte("data")).Cursor()
					for k, _ := c.First(); k != nil; k, _ = c.Next() {
						rangeKeySink = string(k)
						n++
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			keysAll: func(b *testing.B) int {
				keys := make([]string, 0, datasetSize)
				err := db.View(func(tx *bbolt.Tx) error {
					c := tx.Bucket([]byte("data")).Cursor()
					for k, _ := c.First(); k != nil; k, _ = c.Next() {
						keys = append(keys, string(k))
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				if len(keys) > 0 {
					rangeKeySink = keys[len(keys)-1]
				}
				return len(keys)
			},
			scanAllNoCopy: func(b *testing.B) int {
				n := 0
				var got value
				err := db.View(func(tx *bbolt.Tx) error {
					c := tx.Bucket([]byte("data")).Cursor()
					for k, raw := c.First(); k != nil; k, raw = c.Next() {
						rangeKeySink = string(k)
						if err := codec.UnmarshalNoCopy(raw, &got); err != nil {
							return err
						}
						n++
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			scanAllBorrowed: func(b *testing.B) int {
				n := 0
				var got value
				err := db.View(func(tx *bbolt.Tx) error {
					c := tx.Bucket([]byte("data")).Cursor()
					for k, raw := c.First(); k != nil; k, raw = c.Next() {
						if len(k) > 0 {
							rangeKeyByteSink = k[0]
						}
						if err := codec.UnmarshalNoCopy(raw, &got); err != nil {
							return err
						}
						n++
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			scanPrefix: func(b *testing.B) int {
				n := 0
				err := db.View(func(tx *bbolt.Tx) error {
					c := tx.Bucket([]byte("data")).Cursor()
					p := []byte(prefix)
					for k, v := c.Seek(p); bytes.HasPrefix(k, p); k, v = c.Next() {
						var got value
						if err := codec.Unmarshal(v, &got); err != nil {
							return err
						}
						n++
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				return n
			},
			close: db.Close,
		}
	case "Redis":
		addr := os.Getenv("REDIS_ADDR")
		if addr == "" {
			b.Skip("set REDIS_ADDR to include Redis dataset benchmark")
		}
		client := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
		ctx := context.Background()
		if err := client.Ping(ctx).Err(); err != nil {
			b.Fatalf("Redis unavailable at REDIS_ADDR: %v", err)
		}
		_, err := client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for i := 0; i < datasetSize; i++ {
				v := valueFor(i)
				encoded, err := codec.MarshalEvolvable(&v)
				if err != nil {
					return err
				}
				pipe.Set(ctx, datasetKey(ns, i), encoded, 0)
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
		return datasetBackend{
			get: func(b *testing.B, i int) {
				raw, err := client.Get(ctx, datasetKey(ns, i)).Bytes()
				if err != nil {
					b.Fatal(err)
				}
				var got value
				if err := codec.Unmarshal(raw, &got); err != nil {
					b.Fatal(err)
				}
			},
			parallelGet: func(i int) error {
				raw, err := client.Get(ctx, datasetKey(ns, i)).Bytes()
				if err != nil {
					return err
				}
				var got value
				return codec.Unmarshal(raw, &got)
			},
			scanPrefix: func(b *testing.B) int {
				n := 0
				var cursor uint64
				match := prefix + "*"
				for {
					keys, next, err := client.Scan(ctx, cursor, match, 1000).Result()
					if err != nil {
						b.Fatal(err)
					}
					if len(keys) > 0 {
						values, err := client.MGet(ctx, keys...).Result()
						if err != nil {
							b.Fatal(err)
						}
						for i, item := range values {
							raw, ok := item.(string)
							if !ok {
								b.Fatalf("unexpected Redis MGET value type %T", item)
							}
							rangeKeySink = keys[i]
							var got value
							if err := codec.Unmarshal([]byte(raw), &got); err != nil {
								b.Fatal(err)
							}
							n++
						}
					}
					cursor = next
					if cursor == 0 {
						break
					}
				}
				return n
			},
			scanAll: func(b *testing.B) int {
				n := 0
				var got value
				var cursor uint64
				match := ns + "*"
				for {
					keys, next, err := client.Scan(ctx, cursor, match, 1000).Result()
					if err != nil {
						b.Fatal(err)
					}
					if len(keys) > 0 {
						values, err := client.MGet(ctx, keys...).Result()
						if err != nil {
							b.Fatal(err)
						}
						for _, item := range values {
							raw, ok := item.(string)
							if !ok {
								b.Fatalf("unexpected Redis MGET value type %T", item)
							}
							if err := codec.Unmarshal([]byte(raw), &got); err != nil {
								b.Fatal(err)
							}
							n++
						}
					}
					cursor = next
					if cursor == 0 {
						break
					}
				}
				return n
			},
			scanAllNoCopy: func(b *testing.B) int {
				n := 0
				var got value
				var cursor uint64
				match := ns + "*"
				for {
					keys, next, err := client.Scan(ctx, cursor, match, 1000).Result()
					if err != nil {
						b.Fatal(err)
					}
					if len(keys) > 0 {
						values, err := client.MGet(ctx, keys...).Result()
						if err != nil {
							b.Fatal(err)
						}
						for i, item := range values {
							raw, ok := item.(string)
							if !ok {
								b.Fatalf("unexpected Redis MGET value type %T", item)
							}
							rangeKeySink = keys[i]
							if err := codec.UnmarshalNoCopy([]byte(raw), &got); err != nil {
								b.Fatal(err)
							}
							n++
						}
					}
					cursor = next
					if cursor == 0 {
						break
					}
				}
				return n
			},
			close: func() error {
				var cursor uint64
				for {
					keys, next, err := client.Scan(ctx, cursor, ns+"*", 1000).Result()
					if err != nil {
						_ = client.Close()
						return err
					}
					if len(keys) > 0 {
						_ = client.Del(ctx, keys...).Err()
					}
					cursor = next
					if cursor == 0 {
						break
					}
				}
				return client.Close()
			},
		}
	default:
		b.Fatalf("unknown backend %q", name)
		return datasetBackend{}
	}
}

func BenchmarkParallelGet50K(b *testing.B) {
	for _, name := range []string{"Codec", "CodecBorrowed", "CodecCached", "Badger", "bbolt", "Redis"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			db := openDataset(b, name)
			defer func() { b.StopTimer(); _ = db.close() }()
			for i := 0; i < datasetSize; i++ {
				db.get(b, i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			var seq atomic.Uint64
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					i := int((seq.Add(1) * 9973) % datasetSize)
					if err := db.parallelGet(i); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

func BenchmarkGet50K(b *testing.B) {
	for _, name := range []string{"Codec", "CodecBorrowed", "Badger", "bbolt", "Redis"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			db := openDataset(b, name)
			defer func() { b.StopTimer(); _ = db.close() }()
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				db.get(b, (i*9973)%datasetSize)
			}
		})
	}
}

func BenchmarkScanPrefix50K(b *testing.B) {
	for _, name := range []string{"Codec", "Badger", "bbolt", "Redis"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			db := openDataset(b, name)
			defer func() { b.StopTimer(); _ = db.close() }()
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				if n := db.scanPrefix(b); n != 10 {
					b.Fatalf("prefix scan returned %d values, want 10", n)
				}
			}
		})
	}
}

// BenchmarkRange50K decodes the entire 50K-key fixture through each backend's
// ordered iteration API. Redis uses SCAN plus MGET in batches of up to 1,000.
func BenchmarkRange50K(b *testing.B) {
	for _, name := range []string{"Codec", "CodecSnapshot", "Badger", "bbolt", "Redis"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			db := openDataset(b, name)
			defer func() { b.StopTimer(); _ = db.close() }()
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				if n := db.scanAll(b); n != datasetSize {
					b.Fatalf("full range returned %d values, want %d", n, datasetSize)
				}
			}
		})
	}
}

// BenchmarkScanKeys50K isolates ordered key traversal from value reads and
// decoding, helping separate index costs from the full range benchmark.
func BenchmarkScanKeys50K(b *testing.B) {
	for _, name := range []string{"Codec", "Badger", "bbolt"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			db := openDataset(b, name)
			defer func() { b.StopTimer(); _ = db.close() }()
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				if n := db.scanKeysAll(b); n != datasetSize {
					b.Fatalf("key scan returned %d keys, want %d", n, datasetSize)
				}
			}
		})
	}
}

// BenchmarkKeys50K compares Codec's owned KeysSnapshot result with a bbolt cursor
// snapshot, without decoding values.
func BenchmarkKeys50K(b *testing.B) {
	for _, name := range []string{"Codec", "bbolt"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			db := openDataset(b, name)
			defer func() { b.StopTimer(); _ = db.close() }()
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				if n := db.keysAll(b); n != datasetSize {
					b.Fatalf("Keys returned %d keys, want %d", n, datasetSize)
				}
			}
		})
	}
}

// BenchmarkRangeNoCopy50K applies codec.UnmarshalNoCopy to every backend's
// borrowed value bytes and consumes all values before advancing its iterator.
func BenchmarkRangeNoCopy50K(b *testing.B) {
	for _, name := range []string{"CodecSnapshotNoCopy", "Badger", "bbolt", "Redis"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			db := openDataset(b, name)
			defer func() { b.StopTimer(); _ = db.close() }()
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				if n := db.scanAllNoCopy(b); n != datasetSize {
					b.Fatalf("full no-copy range returned %d values, want %d", n, datasetSize)
				}
			}
		})
	}
}

// BenchmarkRangeBorrowed50K scans borrowed keys and values, avoiding string
// key copies while matching cursor key/value lifetimes on embedded engines.
func BenchmarkRangeBorrowed50K(b *testing.B) {
	for _, name := range []string{"CodecSnapshotBorrowed", "CodecTakeBorrowed", "Badger", "bbolt"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			db := openDataset(b, name)
			defer func() { b.StopTimer(); _ = db.close() }()
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				if n := db.scanAllBorrowed(b); n != datasetSize {
					b.Fatalf("full borrowed range returned %d values, want %d", n, datasetSize)
				}
			}
		})
	}
}

// BenchmarkTakeRange50K compares bounded owned range callbacks with native
// ordered range iteration in the embedded engines.
func BenchmarkTakeRange50K(b *testing.B) {
	for _, name := range []string{"Codec", "Badger", "bbolt"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			db := openDataset(b, name)
			defer func() { b.StopTimer(); _ = db.close() }()
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				if n := db.takeRangeAll(b); n != datasetSize {
					b.Fatalf("take range returned %d values, want %d", n, datasetSize)
				}
			}
		})
	}
}

// BenchmarkPage50K materializes owned key/value pages of 256 entries across
// the same 50K fixture for each embedded backend.
func BenchmarkPage50K(b *testing.B) {
	for _, name := range []string{"Codec", "Badger", "bbolt"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			db := openDataset(b, name)
			defer func() { b.StopTimer(); _ = db.close() }()
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				if n := db.pageAll(b); n != datasetSize {
					b.Fatalf("page traversal returned %d values, want %d", n, datasetSize)
				}
			}
		})
	}
}

// BenchmarkStream50K compares the Codec channel APIs with repeated Page calls
// over the same owned-value workload.
func BenchmarkStream50K(b *testing.B) {
	for _, mode := range []string{"Page", "Stream", "StreamResults", "StreamBorrowed"} {
		b.Run(mode, func(b *testing.B) {
			b.StopTimer()
			backendName := "Codec"
			if mode == "StreamBorrowed" {
				backendName = "CodecStreamBorrowed"
			}
			db := openDataset(b, backendName)
			defer func() { b.StopTimer(); _ = db.close() }()
			run := map[string]func(*testing.B) int{
				"Page":           db.pageAll,
				"Stream":         db.streamAll,
				"StreamResults":  db.streamResultAll,
				"StreamBorrowed": db.streamBorrowedAll,
			}[mode]
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				if n := run(b); n != datasetSize {
					b.Fatalf("%s returned %d values, want %d", mode, n, datasetSize)
				}
			}
		})
	}
}

// BenchmarkGet50KWarm measures a varied lookup pass after all backends have
// read the full working set once. CodecCached retains encoded values in its
// configured bounded cache; the other backends use their normal page caches.
func BenchmarkGet50KWarm(b *testing.B) {
	for _, name := range []string{"Codec", "CodecBorrowed", "CodecCached", "Badger", "bbolt", "Redis"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			db := openDataset(b, name)
			defer func() { b.StopTimer(); _ = db.close() }()
			for i := 0; i < datasetSize; i++ {
				db.get(b, i)
			}
			var cacheMiB float64
			if name == "CodecCached" && db.stats().CachedEntries != datasetSize {
				b.Fatalf("warm cache stats %+v, want %d entries", db.stats(), datasetSize)
			}
			if name == "CodecCached" {
				cacheMiB = float64(db.stats().CachedBytes) / (1 << 20)
			}
			b.ReportAllocs()
			b.ResetTimer()
			if cacheMiB > 0 {
				b.ReportMetric(cacheMiB, "cacheMiB")
			}
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				db.get(b, (i*9973)%datasetSize)
			}
		})
	}
}
