package store

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oarkflow/codec"
)

func TestExpirySurvivesReopenAndCompaction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	db, err := Open[User](dir, &Options{SegmentSize: 256})
	if err != nil {
		t.Fatal(err)
	}
	dead := User{Name: "expired"}
	live := User{Name: "live"}
	if err := db.PutUntil("dead", &dead, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTTL("live", &live, time.Hour); err != nil {
		t.Fatal(err)
	}
	if db.Has("dead") || db.Len() != 1 {
		t.Fatalf("expiry visibility: has=%v len=%d", db.Has("dead"), db.Len())
	}
	var got User
	if err := db.Get("dead", &got); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired Get = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open[User](dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Get("live", &got); err != nil || got.Name != "live" {
		t.Fatalf("live after reopen: %+v, %v", got, err)
	}
	if db.Has("dead") || db.Len() != 1 {
		t.Fatalf("reopened expiry visibility: has=%v len=%d", db.Has("dead"), db.Len())
	}
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.t.get([]byte("dead")); ok {
		t.Fatal("compaction retained the expired key in the index")
	}
	if err := db.Get("live", &got); err != nil || got.Name != "live" {
		t.Fatalf("live after compaction: %+v, %v", got, err)
	}
}

func TestBatchExpiryIsAtomicAndSurvivesReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "batch-expiry")
	db, err := Open[User](dir, &Options{CompactRatio: -1, ExpiryInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	b := db.NewBatch()
	if err := b.PutTTL("live", &User{Name: "live"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := b.PutUntil("expired", &User{Name: "expired"}, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if db.Has("expired") || !db.Has("live") {
		t.Fatal("batch expiry metadata was not applied")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open[User](dir, &Options{CompactRatio: -1, ExpiryInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got User
	if err := db.Get("live", &got); err != nil || got.Name != "live" {
		t.Fatalf("live batch entry after reopen = %+v, %v", got, err)
	}
	if err := db.Get("expired", &got); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired batch entry after reopen = %v", err)
	}
}

func TestBatchTTLStartsAtCommit(t *testing.T) {
	db, err := Open[User](filepath.Join(t.TempDir(), "batch-ttl"), &Options{
		CompactRatio: -1, ExpiryInterval: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := db.NewBatch()
	if err := b.PutTTL("delayed", &User{Name: "value"}, 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	ttl, err := db.RemainingTTL("delayed")
	if err != nil || ttl <= 250*time.Millisecond {
		t.Fatalf("TTL after delayed commit = %v, %v", ttl, err)
	}
}

func TestDeleteTreatsExpiredKeyAsMissing(t *testing.T) {
	db, err := Open[User](filepath.Join(t.TempDir(), "delete-expired"), &Options{
		CompactRatio: -1, ExpiryInterval: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PutUntil("expired", &User{Name: "gone"}, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete("expired"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete expired key = %v", err)
	}
}

func TestUpdateAndInspectExpiry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "expiry-updates")
	opts := &Options{CompactRatio: -1, ExpiryInterval: -1, ReadCacheBytes: 1 << 20}
	db, err := Open[User](dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put("key", &User{Name: "value"}); err != nil {
		t.Fatal(err)
	}
	var got User
	if err := db.Get("key", &got); err != nil || got.Name != "value" {
		t.Fatalf("value before expiry update = %+v, %v", got, err)
	}
	if ttl, err := db.RemainingTTL("key"); err != nil || ttl != 0 {
		t.Fatalf("persistent TTL = %v, %v", ttl, err)
	}
	if err := db.Expire("key", time.Hour); err != nil {
		t.Fatal(err)
	}
	if ttl, err := db.RemainingTTL("key"); err != nil || ttl <= 0 || ttl > time.Hour {
		t.Fatalf("set TTL = %v, %v", ttl, err)
	}
	if err := db.Get("key", &got); err != nil || got.Name != "value" {
		t.Fatalf("value after expiry update = %+v, %v", got, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open[User](dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if ttl, err := db.RemainingTTL("key"); err != nil || ttl <= 0 {
		t.Fatalf("TTL after reopen = %v, %v", ttl, err)
	}
	if err := db.Persist("key"); err != nil {
		t.Fatal(err)
	}
	if ttl, err := db.RemainingTTL("key"); err != nil || ttl != 0 {
		t.Fatalf("persisted TTL = %v, %v", ttl, err)
	}
	if err := db.Expire("missing", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Expire missing key = %v", err)
	}
	if err := db.Expire("key", 0); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("Expire with zero TTL = %v", err)
	}
}

func TestOpenLegacySegmentThenWriteTTL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "legacy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	v, err := codec.MarshalEvolvable(&User{Name: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("old")
	rec := make([]byte, legacyRecHeader)
	rec = append(rec, key...)
	rec = append(rec, v...)
	binary.LittleEndian.PutUint32(rec[4:], uint32(len(key)))
	binary.LittleEndian.PutUint32(rec[8:], uint32(len(v)))
	binary.LittleEndian.PutUint32(rec, crc32.Checksum(rec[4:], crcTable))
	f, err := os.Create(segPath(dir, 1))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write(append([]byte(segMagicV1), rec...))
	if err == nil {
		err = f.Close()
	} else {
		_ = f.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(dir, []uint32{1}, 2); err != nil {
		t.Fatal(err)
	}
	db, err := Open[User](dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got User
	if err := db.Get("old", &got); err != nil || got.Name != "legacy" {
		t.Fatalf("legacy read = %+v, %v", got, err)
	}
	if err := db.PutTTL("new", &User{Name: "ttl"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open[User](dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Get("new", &got); err != nil || got.Name != "ttl" {
		t.Fatalf("v2 TTL after reopen = %+v, %v", got, err)
	}
}

func TestExpiryWorkerRemovesKeysAndIgnoresStaleQueueEntries(t *testing.T) {
	db, err := Open[User](filepath.Join(t.TempDir(), "db"), &Options{
		CompactRatio:   -1,
		ExpiryInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PutTTL("replace", &User{Name: "old"}, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTTL("replace", &User{Name: "new"}, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTTL("expire", &User{Name: "gone"}, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		db.mu.RLock()
		_, stillIndexed := db.t.get([]byte("expire"))
		db.mu.RUnlock()
		if !stillIndexed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	db.mu.RLock()
	_, stillIndexed := db.t.get([]byte("expire"))
	db.mu.RUnlock()
	if stillIndexed {
		t.Fatal("expiry worker did not remove the key from the index")
	}
	var got User
	if err := db.Get("replace", &got); err != nil || got.Name != "new" {
		t.Fatalf("stale expiry deleted replacement: %+v, %v", got, err)
	}
}
