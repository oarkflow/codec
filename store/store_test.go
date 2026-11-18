package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oarkflow/codec"
)

type User struct {
	Name  string
	Age   uint32
	Tags  []string
	Score float64
}

func open(t testing.TB, o *Options) (*DB[User], string) {
	dir := filepath.Join(t.TempDir(), "db")
	db, err := Open[User](dir, o)
	if err != nil {
		t.Fatal(err)
	}
	return db, dir
}

func reopen(t testing.TB, dir string, o *Options) *DB[User] {
	db, err := Open[User](dir, o)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestCRUD(t *testing.T) {
	db, dir := open(t, nil)
	u := User{"ada", 36, []string{"a", "b"}, 1.5}
	if err := db.Put("u1", &u); err != nil {
		t.Fatal(err)
	}
	var got User
	if err := db.Get("u1", &got); err != nil || !reflect.DeepEqual(got, u) {
		t.Fatalf("get: %v %+v", err, got)
	}
	if err := db.Update("u1", func(v *User, ok bool) error {
		if !ok {
			t.Fatal("should exist")
		}
		v.Age++
		v.Tags = append(v.Tags, "c")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update("new", func(v *User, ok bool) error { v.Name = "n"; return nil }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := db.Update("u1", func(v *User, ok bool) error { v.Age = 99; return boom }); err != boom {
		t.Fatal(err)
	}
	db.Get("u1", &got)
	if got.Age != 37 || len(got.Tags) != 3 {
		t.Fatalf("update: %+v", got)
	}
	if err := db.Delete("u1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Get("u1", &got); err != ErrNotFound {
		t.Fatal(err)
	}
	if err := db.Delete("u1"); err != ErrNotFound {
		t.Fatal(err)
	}
	if db.Len() != 1 || !db.Has("new") {
		t.Fatal("len/has")
	}
	db.Close()
	if err := db.Put("x", &u); err != ErrClosed {
		t.Fatal(err)
	}

	for _, dropIndex := range []bool{false, true} { // with and without the checkpoint
		if dropIndex {
			os.Remove(filepath.Join(dir, "INDEX"))
		}
		db = reopen(t, dir, nil)
		if db.Len() != 1 || db.Has("u1") {
			t.Fatalf("after reopen (dropIndex=%v): %v", dropIndex, db.Keys())
		}
		db.Get("new", &got)
		if got.Name != "n" {
			t.Fatal(got)
		}
		db.Close()
	}
}

func lastSeg(t testing.TB, dir string) string {
	m, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	sort.Strings(m)
	return m[len(m)-1]
}

func TestTornTail(t *testing.T) {
	db, dir := open(t, nil)
	for i := 0; i < 10; i++ {
		db.Put(fmt.Sprint("k", i), &User{Name: strings.Repeat("x", i+1)})
	}
	db.Close()
	os.Remove(filepath.Join(dir, "INDEX")) // force a full replay
	seg := lastSeg(t, dir)
	orig, _ := os.ReadFile(seg)
	for _, cut := range []int{1, 5, 13, 30} { // chop into the last record
		os.Remove(filepath.Join(dir, "INDEX")) // the test rewrites the file out-of-band
		os.WriteFile(seg, orig[:len(orig)-cut], 0o644)
		d := reopen(t, dir, nil)
		if d.Len() != 9 {
			t.Fatalf("cut %d: len %d", cut, d.Len())
		}
		d.Put("after", &User{Name: "ok"}) // must append cleanly after recovery
		d.Close()
		os.Remove(filepath.Join(dir, "INDEX"))
		d = reopen(t, dir, nil)
		var u User
		if err := d.Get("after", &u); err != nil || u.Name != "ok" || d.Len() != 10 {
			t.Fatalf("cut %d: %v %v", cut, err, d.Len())
		}
		d.Close()
	}
	// bit flip in the middle: everything from there on is dropped
	b := append([]byte(nil), orig...)
	b[len(b)/2] ^= 0xFF
	os.WriteFile(seg, b, 0o644)
	os.Remove(filepath.Join(dir, "INDEX"))
	d := reopen(t, dir, nil)
	if d.Len() >= 10 || d.Len() == 0 {
		t.Fatalf("flip: %d", d.Len())
	}
	d.Close()
}

func TestBatchAtomic(t *testing.T) {
	db, dir := open(t, nil)
	db.Put("keep", &User{Name: "k"})
	b := db.NewBatch()
	b.Put("a", &User{Name: "a"})
	b.Put("b", &User{Name: "b"})
	b.Delete("keep")
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if db.Has("keep") || !db.Has("a") || !db.Has("b") {
		t.Fatal("batch not applied")
	}
	db.Close()
	os.Remove(filepath.Join(dir, "INDEX"))
	seg := lastSeg(t, dir)
	full, _ := os.ReadFile(seg)
	os.WriteFile(seg, full[:len(full)-3], 0o644) // cut inside the batch
	d := reopen(t, dir, nil)
	defer d.Close()
	if d.Len() != 1 || !d.Has("keep") || d.Has("a") {
		t.Fatalf("partial batch visible: %v", d.Keys())
	}
}

func TestSegmentsAndCompaction(t *testing.T) {
	o := &Options{SegmentSize: 4096, CompactRatio: -1}
	db, dir := open(t, o)
	for r := 0; r < 20; r++ {
		for i := 0; i < 100; i++ {
			db.Put(fmt.Sprint("k", i), &User{Name: "n", Age: uint32(r)})
		}
	}
	for i := 0; i < 50; i++ {
		db.Delete(fmt.Sprint("k", i))
	}
	before := db.Stats()
	if before.Segments < 5 || before.DeadBytes < before.FileBytes/2 {
		t.Fatalf("expected many segments and mostly dead data: %+v", before)
	}
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	after := db.Stats()
	if after.FileBytes >= before.FileBytes/4 || after.Keys != 50 {
		t.Fatalf("compact: %+v -> %+v", before, after)
	}
	var u User
	if err := db.Get("k75", &u); err != nil || u.Age != 19 {
		t.Fatal(err, u)
	}
	if db.Has("k10") {
		t.Fatal("deleted key came back")
	}
	db.Put("post", &User{Name: "p"})
	db.Close()
	for _, dropIndex := range []bool{false, true} {
		if dropIndex {
			os.Remove(filepath.Join(dir, "INDEX"))
		}
		db = reopen(t, dir, o)
		if db.Len() != 51 || db.Has("k10") {
			t.Fatalf("reopen dropIndex=%v: len=%d", dropIndex, db.Len())
		}
		db.Get("k99", &u)
		if u.Age != 19 {
			t.Fatal(u)
		}
		db.Close()
	}
}

// A tombstone must survive compaction of a middle run, or a full replay would
// resurrect the deleted key from an older segment.
func TestTombstoneNotResurrected(t *testing.T) {
	o := &Options{SegmentSize: 1, CompactRatio: -1} // one record per segment
	db, dir := open(t, o)
	db.Put("a", &User{Name: "old"}) // seg 1
	db.Put("b", &User{Name: "x"})   // seg 2
	db.Delete("a")                  // seg 3 (tombstone)
	db.Put("c", &User{Name: "y"})   // seg 4
	db.Put("b", &User{Name: "z"})   // seg 5, makes seg 2 dead
	db.mu.Lock()
	run := []uint32{db.order[1], db.order[2]} // segs holding b(old) and the tombstone: a middle run
	db.mu.Unlock()
	if err := db.compactRun(run); err != nil {
		t.Fatal(err)
	}
	db.Close()
	os.Remove(filepath.Join(dir, "INDEX"))
	db = reopen(t, dir, o)
	defer db.Close()
	if db.Has("a") || !db.Has("b") || !db.Has("c") || db.Len() != 2 {
		t.Fatalf("tombstone lost: %v", db.Keys())
	}
}

func TestAutoCompact(t *testing.T) {
	db, _ := open(t, &Options{SegmentSize: 2048})
	defer db.Close()
	for i := 0; i < 5000; i++ {
		db.Put("same", &User{Name: "n", Age: uint32(i)})
	}
	for i := 0; i < 500 && db.Stats().FileBytes >= 20000; i++ { // let the background worker finish
		time.Sleep(10 * time.Millisecond)
	}
	if s := db.Stats(); s.FileBytes > 20000 {
		t.Fatalf("auto compaction did not run: %+v", s)
	}
	var u User
	db.Get("same", &u)
	if u.Age != 4999 {
		t.Fatal(u)
	}
}

func TestCompactWhileWriting(t *testing.T) {
	o := &Options{SegmentSize: 8192, CompactRatio: 0.3}
	db, dir := open(t, o)
	const keys = 200
	var wg sync.WaitGroup
	stop := make(chan struct{})
	final := make([]uint32, keys)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for r := uint32(1); r <= 60; r++ {
			for i := 0; i < keys; i++ {
				if i%7 == 0 && r%10 == 0 {
					db.Delete(fmt.Sprint("k", i))
					final[i] = 0
					continue
				}
				db.Put(fmt.Sprint("k", i), &User{Name: "v", Age: r})
				final[i] = r
			}
		}
		close(stop)
	}()
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var u User
			for {
				select {
				case <-stop:
					return
				default:
				}
				db.Get("k5", &u)
				db.ScanKeys("k1*", func(string) bool { return true })
			}
		}()
	}
	for i := 0; i < 5; i++ {
		db.Compact()
	}
	wg.Wait()
	db.Compact()
	check := func(d *DB[User], tag string) {
		for i := 0; i < keys; i++ {
			var u User
			err := d.Get(fmt.Sprint("k", i), &u)
			if final[i] == 0 {
				if err != ErrNotFound {
					t.Fatalf("%s k%d should be deleted: %v", tag, i, err)
				}
			} else if err != nil || u.Age != final[i] {
				t.Fatalf("%s k%d: %v %+v want age %d", tag, i, err, u, final[i])
			}
		}
	}
	check(db, "live")
	db.Close()
	os.Remove(filepath.Join(dir, "INDEX"))
	db = reopen(t, dir, o)
	check(db, "replayed")
	db.Close()
}

func TestStaleCheckpointIgnored(t *testing.T) {
	o := &Options{SegmentSize: 2048, CompactRatio: -1}
	db, dir := open(t, o)
	for i := 0; i < 300; i++ {
		db.Put(fmt.Sprint("k", i%30), &User{Age: uint32(i)})
	}
	db.Close() // writes INDEX
	idx, _ := os.ReadFile(filepath.Join(dir, "INDEX"))
	db = reopen(t, dir, o)
	db.Compact() // segments change; INDEX from before is now stale
	db.Put("later", &User{Name: "l"})
	db.Close()
	os.WriteFile(filepath.Join(dir, "INDEX"), idx, 0o644)
	db = reopen(t, dir, o)
	defer db.Close()
	var u User
	if db.Len() != 31 || db.Get("k29", &u) != nil || u.Age != 299 || !db.Has("later") {
		t.Fatalf("stale checkpoint was trusted: len=%d %+v", db.Len(), u)
	}
	// a corrupt checkpoint is ignored too
	db.Close()
	b, _ := os.ReadFile(filepath.Join(dir, "INDEX"))
	b[len(b)/2] ^= 1
	os.WriteFile(filepath.Join(dir, "INDEX"), b, 0o644)
	db = reopen(t, dir, o)
	if db.Len() != 31 {
		t.Fatal(db.Len())
	}
}

func TestPatterns(t *testing.T) {
	db, _ := open(t, nil)
	defer db.Close()
	keys := []string{"user:1:name", "user:1:email", "user:2:name", "user:2:email", "user:10:name", "order:1", "order:2", "a*b", "a?b"}
	for _, k := range keys {
		db.Put(k, &User{Name: k})
	}
	cases := []struct {
		pat  string
		want []string
	}{
		{"user:1:*", []string{"user:1:email", "user:1:name"}},
		{"user:*:name", []string{"user:10:name", "user:1:name", "user:2:name"}},
		{"*:email", []string{"user:1:email", "user:2:email"}},
		{"order:?", []string{"order:1", "order:2"}},
		{"user:[12]:name", []string{"user:1:name", "user:2:name"}},
		{"user:[^1]:*", []string{"user:2:email", "user:2:name"}},
		{`a\*b`, []string{"a*b"}},
		{"a?b", []string{"a*b", "a?b"}},
		{"order:1", []string{"order:1"}},
		{"nothing*", nil},
	}
	for _, c := range cases {
		got, err := db.Match(c.pat)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("Match(%q) = %q, %v; want %q", c.pat, got, err, c.want)
		}
		n, _ := db.Count(c.pat)
		if n != len(c.want) {
			t.Errorf("Count(%q) = %d", c.pat, n)
		}
	}
	if _, err := db.Match("[bad"); !errors.Is(err, ErrBadPattern) {
		t.Fatal(err)
	}
	var names []string
	db.Scan("user:2:*", func(k string, v *User) bool {
		if v.Name != k {
			t.Fatalf("value mismatch %q %q", k, v.Name)
		}
		names = append(names, k)
		return true
	})
	if len(names) != 2 {
		t.Fatal(names)
	}
	var pre []string
	db.ScanPrefix("a*", func(k string, _ *User) bool { pre = append(pre, k); return true }) // literal '*'
	if !reflect.DeepEqual(pre, []string{"a*b"}) && len(pre) != 0 {
		t.Fatal(pre)
	}
	stopped := 0
	db.ScanKeys("*", func(string) bool { stopped++; return stopped < 3 })
	if stopped != 3 {
		t.Fatal(stopped)
	}
	if n, err := db.DeleteMatch("user:1*"); err != nil || n != 3 {
		t.Fatalf("DeleteMatch: %d %v", n, err)
	}
	if db.Len() != len(keys)-3 || db.Has("user:10:name") || !db.Has("user:2:name") {
		t.Fatal(db.Keys())
	}
}

func TestScanLargeAndWriteInCallback(t *testing.T) {
	db, _ := open(t, &Options{SegmentSize: 1 << 14})
	defer db.Close()
	const n = 2000 // several scan chunks
	for i := 0; i < n; i++ {
		db.Put(fmt.Sprintf("item:%05d", i), &User{Age: uint32(i)})
	}
	prev, cnt := "", 0
	err := db.Scan("item:*", func(k string, v *User) bool {
		if k <= prev {
			t.Fatalf("not sorted: %q after %q", k, prev)
		}
		prev, cnt = k, cnt+1
		return db.Put("copy:"+k, v) == nil // writes inside the callback must not deadlock
	})
	if err != nil || cnt != n || db.Len() != 2*n {
		t.Fatalf("scan: %v cnt=%d len=%d", err, cnt, db.Len())
	}
	if c, _ := db.Count("item:0001?"); c != 10 {
		t.Fatal(c)
	}
	if got, _ := db.Match("copy:item:0000[05]"); len(got) != 2 {
		t.Fatal(got)
	}
}

func TestConcurrent(t *testing.T) {
	db, _ := open(t, &Options{SegmentSize: 1 << 16})
	defer db.Close()
	db.Put("ctr", &User{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				db.Update("ctr", func(v *User, _ bool) error { v.Age++; return nil })
				db.Put(fmt.Sprint("g", g, "-", i), &User{Name: "x"})
				var u User
				db.Get("ctr", &u)
				db.Count(fmt.Sprint("g", g, "-*"))
			}
		}(g)
	}
	wg.Wait()
	var u User
	db.Get("ctr", &u)
	if u.Age != 1600 || db.Len() != 1601 {
		t.Fatalf("lost updates: %d %d", u.Age, db.Len())
	}
}

func TestLargeValue(t *testing.T) {
	db, dir := open(t, &Options{SegmentSize: 1 << 20})
	big := User{Name: strings.Repeat("0123456789", 600000), Tags: []string{"t"}} // ~6 MB
	if err := db.Put("big", &big); err != nil {
		t.Fatal(err)
	}
	db.Put("small", &User{Name: "s"})
	db.Close()
	db = reopen(t, dir, &Options{SegmentSize: 1 << 20})
	defer db.Close()
	var got User
	if err := db.Get("big", &got); err != nil || got.Name != big.Name {
		t.Fatal(err)
	}
	if st := db.Stats(); st.Segments < 2 {
		t.Fatalf("%+v", st)
	}
}

func TestExportImportText(t *testing.T) {
	db, _ := open(t, nil)
	defer db.Close()
	db.Put("a", &User{Name: "a", Age: 1, Tags: []string{"x"}})
	db.Put("b", &User{Name: "b", Score: 2.5})
	var buf bytes.Buffer
	if err := db.ExportText(&buf); err != nil {
		t.Fatal(err)
	}
	exported := buf.String()
	want := "a.Name a\na.Age 1\na.Tags x\nb.Name b\nb.Score 2.5\n"
	if exported != want {
		t.Fatalf("unexpected export:\n%s\nwant:\n%s", exported, want)
	}
	db2, _ := open(t, nil)
	defer db2.Close()
	if err := db2.ImportText(&buf); err != nil {
		t.Fatal(err)
	}
	var a, b User
	db2.Get("a", &a)
	db2.Get("b", &b)
	if a.Age != 1 || a.Tags[0] != "x" || b.Score != 2.5 {
		t.Fatal(a, b)
	}
}

func TestViewRaw(t *testing.T) {
	db, _ := open(t, nil)
	defer db.Close()
	db.Put("k", &User{Name: "zero-copy", Age: 7})
	err := db.View("k", func(raw []byte) error {
		v, err := codec.View(raw, &User{})
		if err != nil {
			return err
		}
		f, _ := v.Field("Name")
		if f.String() != "zero-copy" {
			t.Fatal(f.String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func BenchmarkPut(b *testing.B) {
	db, _ := open(b, &Options{SegmentSize: 8 << 20, CompactRatio: -1})
	defer db.Close()
	u := User{"ada lovelace", 36, []string{"a", "b"}, 1.5}
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = fmt.Sprint("user:", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.Put(keys[i&1023], &u)
	}
}

func BenchmarkGet(b *testing.B) {
	db, _ := open(b, nil)
	defer db.Close()
	u := User{"ada lovelace", 36, []string{"a", "b"}, 1.5}
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = fmt.Sprint("user:", i)
		db.Put(keys[i], &u)
	}
	var out User
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.Get(keys[i&1023], &out)
	}
}

func BenchmarkUpdate(b *testing.B) {
	db, _ := open(b, &Options{SegmentSize: 8 << 20})
	defer db.Close()
	db.Put("k", &User{Name: "x"})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.Update("k", func(v *User, _ bool) error { v.Age++; return nil })
	}
}

// Wildcard scans over a million keys: the literal prefix narrows the walk.
var (
	scanDB   *DB[User]
	scanOnce sync.Once
	scanHeap float64
)

func scanFixture(b *testing.B) *DB[User] {
	scanOnce.Do(func() {
		dir, _ := os.MkdirTemp("", "strata-scan")
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		scanDB, _ = Open[User](dir, &Options{CompactRatio: -1})
		batch := scanDB.NewBatch()
		for i := 0; i < 1_000_000; i++ {
			batch.Put(fmt.Sprintf("user:%04d:field%d", i/10, i%10), &User{Age: uint32(i)})
			if batch.Len() == 5000 {
				batch.Commit()
			}
		}
		batch.Commit()
		runtime.GC()
		runtime.ReadMemStats(&m1)
		scanHeap = float64(m1.HeapAlloc-m0.HeapAlloc) / 1e6
	})
	return scanDB
}

func benchScan(b *testing.B, pattern string, wantMin int) {
	db := scanFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if n, _ := db.Count(pattern); n < wantMin {
			b.Fatal(n)
		}
	}
	b.ReportMetric(scanHeap, "heapMB/1Mkeys")
}

func BenchmarkScanPrefix1M(b *testing.B) { benchScan(b, "user:0042:*", 10) }
func BenchmarkScanMiddle1M(b *testing.B) { benchScan(b, "user:00?2:field3", 10) }
func BenchmarkScanSuffix1M(b *testing.B) { benchScan(b, "*:field7", 100000) }

type Order struct {
	ID    int
	Lines []struct {
		SKU string
		Qty int
	}
	Tags []string
	Meta map[string]string
}

func TestExportImportNested(t *testing.T) {
	db, err := Open[Order](filepath.Join(t.TempDir(), "o"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mk := func(id int, skus ...string) Order {
		o := Order{ID: id, Tags: []string{"a b", "c"}, Meta: map[string]string{"k.1": "v 1", "z": ""}}
		for i, s := range skus {
			o.Lines = append(o.Lines, struct {
				SKU string
				Qty int
			}{s, i + 1})
		}
		return o
	}
	keys := []string{"order:1", "order.2", "has space", `quo"te`, "日本"}
	for i, k := range keys {
		o := mk(i+1, "x", "y z")
		db.Put(k, &o)
	}
	empty := Order{}
	db.Put("empty", &empty)
	var buf bytes.Buffer
	if err := db.ExportText(&buf); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	if strings.ContainsAny(text, "{}[]") {
		t.Fatalf("unexpected syntax in export:\n%s", text)
	}
	db2, _ := Open[Order](filepath.Join(t.TempDir(), "o2"), nil)
	defer db2.Close()
	if err := db2.ImportText(strings.NewReader("# comment\n\n" + text)); err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	if db2.Len() != db.Len() {
		t.Fatalf("keys: %d vs %d\n%s", db2.Len(), db.Len(), text)
	}
	for _, k := range append(keys, "empty") {
		var a, b Order
		if db.Get(k, &a) != nil || db2.Get(k, &b) != nil || !reflect.DeepEqual(a.Lines, b.Lines) || a.ID != b.ID ||
			!reflect.DeepEqual(a.Tags, b.Tags) || len(a.Meta) != len(b.Meta) {
			t.Fatalf("key %q differs:\n%+v\n%+v\n--- export:\n%s", k, a, b, text)
		}
	}
}

// ---- a store written by one version of a type and read by the next ----

type ProfileV1 struct {
	Name  string
	Tags  []string
	Stats struct {
		Logins int
	}
}

type ProfileV2 struct {
	Name  string
	Tags  []string
	Stats struct {
		Logins int
		Failed int // new
	}
	Email  string   // new
	Groups []string // new
}

func TestSchemaEvolutionAcrossReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profiles")
	v1, err := Open[ProfileV1](dir, &Options{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		var p ProfileV1
		p.Name = fmt.Sprint("user", i)
		p.Tags = []string{"a", "b"}
		p.Stats.Logins = i
		if err := v1.Put(fmt.Sprint("p:", i), &p); err != nil {
			t.Fatal(err)
		}
	}
	v1.Close()

	// the application is upgraded: the same directory is opened with the new type
	v2, err := Open[ProfileV2](dir, &Options{SegmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer v2.Close()
	var got ProfileV2
	if err := v2.Get("p:57", &got); err != nil || got.Name != "user57" || got.Stats.Logins != 57 || got.Email != "" || got.Groups != nil || got.Stats.Failed != 0 {
		t.Fatalf("%v %+v", err, got)
	}
	// update an old record: it is rewritten with the new fields, readable by v2 again
	if err := v2.Update("p:57", func(p *ProfileV2, _ bool) error {
		p.Email = "a@b"
		p.Stats.Failed = 3
		p.Groups = []string{"g"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// compaction copies the raw bytes of old and new records alike
	if err := v2.Compact(); err != nil {
		t.Fatal(err)
	}
	var again ProfileV2
	if err := v2.Get("p:57", &again); err != nil || again.Email != "a@b" || again.Stats.Failed != 3 || again.Name != "user57" {
		t.Fatalf("%v %+v", err, again)
	}
	var old ProfileV2
	if err := v2.Get("p:3", &old); err != nil || old.Name != "user3" || old.Stats.Logins != 3 || old.Email != "" {
		t.Fatalf("%v %+v", err, old)
	}
	n := 0
	if err := v2.Scan("p:1*", func(k string, p *ProfileV2) bool { n++; return p.Name != "" }); err != nil || n == 0 {
		t.Fatalf("scan: %v %d", err, n)
	}
	v2.Close()

	// downgrading (an old binary on new data) is refused clearly, not misread
	v1b, err := Open[ProfileV1](dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v1b.Close()
	var p1 ProfileV1
	if err := v1b.Get("p:57", &p1); err == nil || !strings.Contains(err.Error(), "newer schema") {
		t.Fatalf("downgrade should be refused: %v", err)
	}
	if err := v1b.Get("p:3", &p1); err != nil || p1.Name != "user3" { // records still at v1 are fine
		t.Fatalf("%v %+v", err, p1)
	}
}

func TestStrictOptionWritesPlain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s")
	db, _ := Open[ProfileV1](dir, &Options{Strict: true})
	p := ProfileV1{Name: "x"}
	db.Put("k", &p)
	db.Close()
	v2, _ := Open[ProfileV2](dir, nil)
	defer v2.Close()
	var got ProfileV2
	if err := v2.Get("k", &got); err == nil {
		t.Fatalf("strict (plain) records must not read as a different type: %+v", got)
	}
}
