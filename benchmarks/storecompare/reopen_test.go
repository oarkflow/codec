package storecompare

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/oarkflow/codec"
	"github.com/oarkflow/codec/store"
	"go.etcd.io/bbolt"
)

type reopenFunc func() (func() error, error)

// BenchmarkCleanReopen50K measures open latency for a cleanly closed 50K-key
// fixture. Close is outside the timer; this does not model crash recovery.
func BenchmarkCleanReopen50K(b *testing.B) {
	for _, name := range []string{"Codec", "Badger", "bbolt"} {
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			_, opener := createReopenFixture(b, name)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StartTimer()
				closeDB, err := opener()
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				if err := closeDB(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkLogReplay50K measures Codec open time after removing the index
// checkpoint, forcing recovery by replaying all committed segment records.
func BenchmarkLogReplay50K(b *testing.B) {
	b.StopTimer()
	dir, opener := createReopenFixture(b, "Codec")
	index := filepath.Join(dir, "INDEX")
	if err := os.Remove(index); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StartTimer()
		closeDB, err := opener()
		b.StopTimer()
		if err != nil {
			b.Fatal(err)
		}
		if err := closeDB(); err != nil {
			b.Fatal(err)
		}
		if err := os.Remove(index); err != nil {
			b.Fatal(err)
		}
	}
}

func createReopenFixture(b *testing.B, name string) (string, reopenFunc) {
	b.Helper()
	dir := filepath.Join(b.TempDir(), name)
	ns := "reopen:" + filepath.Base(dir) + ":"
	encode := func(i int) (string, []byte, error) {
		v := sample
		v.ID = uint64(i)
		raw, err := codec.MarshalEvolvable(&v)
		return fmt.Sprintf("%s%08d", ns, i), raw, err
	}
	switch name {
	case "Codec":
		opts := &store.Options{CompactRatio: -1, ExpiryInterval: -1}
		db, err := store.Open[value](dir, opts)
		if err != nil {
			b.Fatal(err)
		}
		batch := db.NewBatch()
		for i := 0; i < datasetSize; i++ {
			key := fmt.Sprintf("%s%08d", ns, i)
			v := sample
			v.ID = uint64(i)
			if err := batch.Put(key, &v); err != nil {
				b.Fatal(err)
			}
			if batch.Len() == bulkBatchSize {
				if err := batch.Commit(); err != nil {
					b.Fatal(err)
				}
			}
		}
		if err := batch.Commit(); err != nil {
			b.Fatal(err)
		}
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
		return dir, func() (func() error, error) {
			db, err := store.Open[value](dir, opts)
			if err != nil {
				return nil, err
			}
			return db.Close, nil
		}
	case "Badger":
		db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil).WithSyncWrites(false))
		if err != nil {
			b.Fatal(err)
		}
		if err := db.Update(func(txn *badger.Txn) error {
			for i := 0; i < datasetSize; i++ {
				key, raw, err := encode(i)
				if err != nil {
					return err
				}
				if err := txn.Set([]byte(key), raw); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
		return dir, func() (func() error, error) {
			db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil).WithSyncWrites(false))
			if err != nil {
				return nil, err
			}
			return db.Close, nil
		}
	case "bbolt":
		path := dir + ".db"
		db, err := bbolt.Open(path, 0o600, &bbolt.Options{NoSync: true})
		if err != nil {
			b.Fatal(err)
		}
		if err := db.Update(func(tx *bbolt.Tx) error {
			bucket, err := tx.CreateBucketIfNotExists([]byte("data"))
			if err != nil {
				return err
			}
			for i := 0; i < datasetSize; i++ {
				key, raw, err := encode(i)
				if err != nil {
					return err
				}
				if err := bucket.Put([]byte(key), raw); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
		return path, func() (func() error, error) {
			db, err := bbolt.Open(path, 0o600, &bbolt.Options{NoSync: true})
			if err != nil {
				return nil, err
			}
			return db.Close, nil
		}
	default:
		b.Fatalf("unknown reopen backend %q", name)
		return "", nil
	}
}
