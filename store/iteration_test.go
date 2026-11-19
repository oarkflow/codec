package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestKeysSnapshotSortedAndCacheInvalidation(t *testing.T) {
	db, _ := open(t, nil)
	defer db.Close()
	for _, key := range []string{"b", "a"} {
		v := User{Name: key}
		if err := db.Put(key, &v); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"a", "b"}
	if got := db.KeysSnapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("first KeysSnapshot = %q, want %q", got, want)
	}
	if got := db.KeysSnapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("cached KeysSnapshot = %q, want %q", got, want)
	}
	v := User{Name: "c"}
	if err := db.Put("c", &v); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete("a"); err != nil {
		t.Fatal(err)
	}
	want = []string{"b", "c"}
	if got := db.KeysSnapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("KeysSnapshot after mutation = %q, want %q", got, want)
	}
}

func TestPageRangeAndStreamOwnValues(t *testing.T) {
	db, _ := open(t, nil)
	defer db.Close()
	for _, key := range []string{"", "a", "b", "c"} {
		v := User{Name: key, Tags: []string{key, "kept"}}
		if err := db.Put(key, &v); err != nil {
			t.Fatal(err)
		}
	}

	var pageKeys []string
	var pageItems []Item[User]
	after := ""
	for {
		page, next, err := db.Page("*", after, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page {
			pageKeys = append(pageKeys, item.Key)
			pageItems = append(pageItems, item)
		}
		if next == "" {
			break
		}
		after = next
	}
	if want := []string{"", "a", "b", "c"}; !reflect.DeepEqual(pageKeys, want) {
		t.Fatalf("Page keys = %#v", pageKeys)
	}
	for _, item := range pageItems {
		if item.Value.Name != item.Key || len(item.Value.Tags) != 2 || item.Value.Tags[0] != item.Key || item.Value.Tags[1] != "kept" {
			t.Fatalf("Page value for %q was reused: %#v", item.Key, item.Value)
		}
	}
	if _, _, err := db.Page("*", "not-a-cursor", 2); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("invalid cursor error = %v", err)
	}
	first, emptyCursor, err := db.Page("*", "", 1)
	if err != nil || len(first) != 1 || first[0].Key != "" || emptyCursor == "" {
		t.Fatalf("empty-key cursor page = %#v, %q, %v", first, emptyCursor, err)
	}
	second, _, err := db.Page("*", emptyCursor, 2)
	if err != nil || len(second) != 2 || second[0].Key != "a" || second[1].Key != "b" {
		t.Fatalf("page after empty-key cursor = %#v, %v", second, err)
	}

	var ranged []string
	if err := db.TakeRange("a", "c", 1, func(k string, _ *User) bool { ranged = append(ranged, k); return true }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ranged, []string{"a"}) {
		t.Fatalf("TakeRange = %#v", ranged)
	}

	var streamed []Item[User]
	for result := range db.StreamResults(context.Background(), "*") {
		if result.Err != nil {
			t.Fatal(result.Err)
		}
		streamed = append(streamed, result.Item)
	}
	if len(streamed) != 4 || streamed[0].Value.Tags[0] != "" || streamed[0].Value.Tags[1] != "kept" || streamed[1].Value.Name != "a" || streamed[1].Value.Tags[0] != "a" {
		t.Fatalf("stream values were lost or reused: %#v", streamed)
	}
	var streamKeys []string
	for item := range db.Stream(context.Background(), "*") {
		streamKeys = append(streamKeys, item.Key)
	}
	if !reflect.DeepEqual(streamKeys, []string{"", "a", "b", "c"}) {
		t.Fatalf("Stream keys = %#v", streamKeys)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for range db.Stream(canceled, "*") {
		t.Fatal("canceled Stream produced an item")
	}
	for result := range db.StreamResults(canceled, "*") {
		if result.Err != nil {
			t.Fatalf("canceled StreamResults returned an error: %v", result.Err)
		}
		t.Fatal("canceled StreamResults produced an item")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for result := range db.StreamResults(context.Background(), "*") {
		if !errors.Is(result.Err, ErrClosed) {
			t.Fatalf("closed stream result = %#v", result)
		}
	}
	results := db.StreamResults(context.Background(), "[")
	result, ok := <-results
	if !ok || !errors.Is(result.Err, ErrBadPattern) {
		t.Fatalf("invalid-pattern stream result = %#v, ok=%v", result, ok)
	}
	if _, ok := <-results; ok {
		t.Fatal("invalid-pattern stream returned more than one result")
	}
}

func TestTakeRangeSnapshotBorrowed(t *testing.T) {
	db, _ := open(t, nil)
	defer db.Close()
	for _, key := range []string{"a", "b", "c", "d"} {
		if err := db.Put(key, &User{Name: key}); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	err := db.TakeRangeSnapshotBorrowed("b", "d", 2, func(key []byte, value *User) bool {
		if value.Name != string(key) {
			t.Errorf("value for %q = %q", key, value.Name)
		}
		got = append(got, string(key))
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("borrowed range = %#v", got)
	}
}

func TestSnapshotAfterMappedSegmentAppendsAndRotates(t *testing.T) {
	db, _ := open(t, &Options{SegmentSize: 256, CompactRatio: -1, ExpiryInterval: -1})
	defer db.Close()
	if err := db.Put("a", &User{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RangeSnapshotBorrowed(func([]byte, *User) bool { return true }); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"b", "c"} {
		if err := db.Put(key, &User{Name: key}); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	if err := db.RangeSnapshotBorrowed(func(key []byte, value *User) bool {
		if value.Name != string(key) {
			t.Errorf("value for %q = %q", key, value.Name)
		}
		got = append(got, string(key))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("snapshot after append/rotation = %#v", got)
	}
}

func TestScanUsesCurrentValueAfterCallbackWrites(t *testing.T) {
	db, _ := open(t, nil)
	defer db.Close()
	for i, key := range []string{"a", "b", "c"} {
		if err := db.Put(key, &User{Age: uint32(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[string]uint32)
	err := db.Scan("*", func(key string, value *User) bool {
		seen[key] = value.Age
		if key == "a" {
			if err := db.Put("b", &User{Age: 20}); err != nil {
				t.Error(err)
				return false
			}
			if err := db.Delete("c"); err != nil {
				t.Error(err)
				return false
			}
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen["a"] != 1 || seen["b"] != 20 {
		t.Fatalf("scan observed stale values: %#v", seen)
	}
	if _, ok := seen["c"]; ok {
		t.Fatalf("scan returned deleted key c: %#v", seen)
	}
}

func TestScanSnapshot(t *testing.T) {
	db, _ := open(t, nil)
	defer db.Close()
	for _, key := range []string{"c", "a", "b"} {
		if err := db.Put(key, &User{Name: key}); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	err := db.RangeSnapshot(func(key string, value *User) bool {
		if value.Name != key {
			t.Errorf("value for %q = %q", key, value.Name)
		}
		got = append(got, key)
		return len(got) < 2
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("snapshot order/stop = %#v", got)
	}
	if err := db.ScanSnapshot("[", func(string, *User) bool { return true }); !errors.Is(err, ErrBadPattern) {
		t.Fatalf("malformed snapshot pattern error = %v", err)
	}
}

func TestScanSnapshotNoCopy(t *testing.T) {
	db, _ := open(t, nil)
	defer db.Close()
	want := User{Name: "borrowed", Age: 31, Tags: []string{"a", "b"}}
	if err := db.Put("k", &want); err != nil {
		t.Fatal(err)
	}
	seen := false
	err := db.ScanSnapshotNoCopy("*", func(key string, got *User) bool {
		seen = key == "k" && got.Name == want.Name && got.Age == want.Age && reflect.DeepEqual(got.Tags, want.Tags)
		return true
	})
	if err != nil || !seen {
		t.Fatalf("no-copy snapshot: seen=%v err=%v", seen, err)
	}
}

func TestScanSnapshotBorrowed(t *testing.T) {
	db, _ := open(t, nil)
	defer db.Close()
	want := User{Name: "borrowed", Age: 31, Tags: []string{"a", "b"}}
	if err := db.Put("k", &want); err != nil {
		t.Fatal(err)
	}
	seen := false
	err := db.ScanSnapshotBorrowed("*", func(key []byte, got *User) bool {
		seen = string(key) == "k" && got.Name == want.Name && got.Age == want.Age && reflect.DeepEqual(got.Tags, want.Tags)
		return true
	})
	if err != nil || !seen {
		t.Fatalf("borrowed snapshot: seen=%v err=%v", seen, err)
	}
}

func TestOpenTempRemovesDirectoryOnClose(t *testing.T) {
	db, err := OpenTemp[User](nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := db.dir
	if err := db.Put("scratch", &User{Name: "value"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temporary directory still exists: %v", err)
	}
}
