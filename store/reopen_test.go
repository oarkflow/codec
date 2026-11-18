package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReopenSpeed1M(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	dir := filepath.Join(t.TempDir(), "db")
	db, _ := Open[User](dir, &Options{CompactRatio: -1})
	b := db.NewBatch()
	for i := 0; i < 1_000_000; i++ {
		b.Put(keyN(i), &User{Age: uint32(i)})
		if b.Len() == 5000 {
			b.Commit()
		}
	}
	b.Commit()
	db.Close()
	t0 := time.Now()
	db, _ = Open[User](dir, nil)
	withIdx := time.Since(t0)
	n1 := db.Len()
	db.Close()
	os.Remove(filepath.Join(dir, "INDEX"))
	t0 = time.Now()
	db, _ = Open[User](dir, nil)
	replay := time.Since(t0)
	db.Close()
	t.Logf("1M keys: reopen with checkpoint %v, full log replay %v (len %d)", withIdx, replay, n1)
}

func keyN(i int) string { return "user:" + itoa(i/10) + ":field" + itoa(i%10) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for ; n > 0; n /= 10 {
		i--
		b[i] = byte('0' + n%10)
	}
	return string(b[i:])
}

func TestReopenSpeedBigValues(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	dir := filepath.Join(t.TempDir(), "db")
	db, _ := Open[User](dir, &Options{CompactRatio: -1})
	payload := make([]byte, 2000)
	for i := range payload {
		payload[i] = byte('a' + i*7%26)
	}
	u := User{Name: string(payload)}
	for i := 0; i < 200_000; i++ {
		db.Put(keyN(i), &u)
	}
	st := db.Stats()
	db.Close()
	t0 := time.Now()
	db, _ = Open[User](dir, nil)
	withIdx := time.Since(t0)
	db.Close()
	os.Remove(filepath.Join(dir, "INDEX"))
	t0 = time.Now()
	db, _ = Open[User](dir, nil)
	replay := time.Since(t0)
	db.Close()
	t.Logf("%d keys, %d MB on disk in %d segments: reopen with checkpoint %v, full replay %v", st.Keys, st.FileBytes>>20, st.Segments, withIdx, replay)
}
