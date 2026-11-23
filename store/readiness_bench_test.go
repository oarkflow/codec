package store

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

var readinessUser = User{Name: "Ada Lovelace", Age: 36, Tags: []string{"math", "computing"}, Score: 9.5}

// BenchmarkReadiness covers the store's write, batch, TTL, sync, scan,
// compaction and reopen paths with bounded local fixtures.
func BenchmarkReadiness(b *testing.B) {
	b.Run("BatchPut100", func(b *testing.B) {
		db, _ := open(b, &Options{CompactRatio: -1, ExpiryInterval: -1})
		defer db.Close()
		for b.Loop() {
			batch := db.NewBatch()
			for i := 0; i < 100; i++ {
				if err := batch.Put(fmt.Sprintf("batch:%d", i), &readinessUser); err != nil {
					b.Fatal(err)
				}
			}
			if err := batch.Commit(); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("PutTTL", func(b *testing.B) {
		db, _ := open(b, &Options{CompactRatio: -1, ExpiryInterval: -1})
		defer db.Close()
		b.ReportAllocs()
		for b.Loop() {
			if err := db.PutTTL("ttl", &readinessUser, time.Hour); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("SyncAlwaysPut", func(b *testing.B) {
		db, _ := open(b, &Options{CompactRatio: -1, ExpiryInterval: -1, SyncAlways: true})
		defer db.Close()
		b.ReportAllocs()
		for b.Loop() {
			if err := db.Put("sync", &readinessUser); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("PrefixScan1K", func(b *testing.B) {
		db, _ := open(b, &Options{CompactRatio: -1, ExpiryInterval: -1})
		defer db.Close()
		for i := 0; i < 1000; i++ {
			if err := db.Put(fmt.Sprintf("user:%04d", i), &readinessUser); err != nil {
				b.Fatal(err)
			}
		}
		runtime.GC()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		b.ReportMetric(float64(mem.HeapAlloc)/1e6, "heapMB")
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			if err := db.ScanPrefix("user:", func(_ string, _ *User) bool { return true }); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Compact1K", func(b *testing.B) {
		db, _ := open(b, &Options{SegmentSize: 16 << 10, CompactRatio: -1, ExpiryInterval: -1})
		defer db.Close()
		for i := 0; i < 1000; i++ {
			if err := db.Put(fmt.Sprintf("compact:%04d", i), &readinessUser); err != nil {
				b.Fatal(err)
			}
		}
		b.ResetTimer()
		for b.Loop() {
			for i := 0; i < 100; i++ {
				if err := db.Put(fmt.Sprintf("compact:%04d", i), &readinessUser); err != nil {
					b.Fatal(err)
				}
			}
			if err := db.Compact(); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Reopen1K", func(b *testing.B) {
		db, dir := open(b, &Options{CompactRatio: -1, ExpiryInterval: -1})
		var err error
		for i := 0; i < 1000; i++ {
			if err := db.Put(fmt.Sprintf("reopen:%04d", i), &readinessUser); err != nil {
				b.Fatal(err)
			}
		}
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
		b.ResetTimer()
		for b.Loop() {
			db, err = Open[User](dir, &Options{CompactRatio: -1, ExpiryInterval: -1})
			if err != nil {
				b.Fatal(err)
			}
			if err := db.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
