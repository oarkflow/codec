# Store comparison benchmarks

This module compares single-key reads/overwrites, 1K-key batched reads and
writes, and 50K-key lookups/prefix scans using the same typed payload and
Codec encoding across Codec's typed store (including borrowed reads and
its optional read cache), Badger, bbolt, and an
optional Redis service. Fixture creation,
initial writes, and close are explicitly outside the timed section.
Codec segments are memory-mapped on supported Unix systems. Snapshot scans
refresh the active segment mapping as it grows; an unmapped tail uses file
reads until the next scan. The optional Codec read cache is separately bounded
by `ReadCacheBytes`.

The backends use non-fsync write modes for this first pass: Codec's default,
Badger `SyncWrites(false)`, bbolt `NoSync`, and Redis with persistence disabled.
This is a throughput comparison, not a power-loss durability comparison. Redis
adds client/server and transport overhead; run it on loopback and report it as
a service comparison, not as an embedded-library result.

Run embedded engines:

```sh
cd benchmarks/storecompare
go test -run '^$' -bench 'Benchmark(Put|Get)$' -benchmem -count 5
go test -run '^$' -bench 'Benchmark(Get50K|Get50KWarm|ScanPrefix50K|ParallelGet50K)$' -benchmem -benchtime=5000x -count 3
go test -run '^$' -bench '^BenchmarkBatchPut1K$' -benchmem -benchtime=100x -count 3
go test -run '^$' -bench '^BenchmarkUniqueBatchPut1K$' -benchmem -benchtime=100x -count 5
go test -run '^$' -bench '^BenchmarkRandomUniqueBatchPut1K$' -benchmem -benchtime=100x -count 5
go test -run '^$' -bench '^BenchmarkDurableBatchPut$' -benchmem -benchtime=20x -count 5
go test -run '^$' -bench '^BenchmarkCleanReopen50K$' -benchmem -benchtime=10x -count 5
go test -run '^$' -bench '^BenchmarkLogReplay50K$' -benchmem -benchtime=10x -count 5
go test -run '^$' -bench '^BenchmarkGetMany1K$' -benchmem -benchtime=100x -count 5
go test -run '^$' -bench '^BenchmarkKeys50K$' -benchmem -benchtime=100x -count 5
go test -run '^$' -bench '^BenchmarkScanKeys50K$' -benchmem -benchtime=500x -count 7
go test -run '^$' -bench '^BenchmarkPutTTL$' -benchmem -benchtime=1000x -count 5
go test -run '^$' -bench '^BenchmarkPutTTL1K$' -benchmem -benchtime=100x -count 5
go test -run '^$' -bench '^BenchmarkUniquePutTTL1K$' -benchmem -benchtime=100x -count 5
go test -run '^$' -bench '^BenchmarkExpiredGetMany1K$' -benchmem -benchtime=100x -count 5
go test -run '^$' -bench '^BenchmarkRange50K$' -benchmem -benchtime=5x -count 10
go test -run '^$' -bench '^BenchmarkRangeNoCopy50K$' -benchmem -benchtime=5x -count 10
go test -run '^$' -bench '^BenchmarkRangeBorrowed50K$' -benchmem -benchtime=20x -count 5
go test -run '^$' -bench '^BenchmarkTakeRange50K$' -benchmem -benchtime=20x -count 5
go test -run '^$' -bench '^BenchmarkPage50K$' -benchmem -benchtime=20x -count 5
go test -run '^$' -bench '^BenchmarkStream50K$' -benchmem -benchtime=20x -count 5
```

Include Redis when a local server is available:

```sh
REDIS_ADDR=127.0.0.1:6379 go test -run '^$' -bench 'Benchmark(Put|Get|Get50K|Get50KWarm|ScanPrefix50K|ParallelGet50K)$' -benchmem -benchtime=5000x -count 3
REDIS_ADDR=127.0.0.1:6379 go test -run '^$' -bench '^BenchmarkBatchPut1K$' -benchmem -benchtime=100x -count 3
REDIS_ADDR=127.0.0.1:6379 go test -run '^$' -bench '^BenchmarkUniqueBatchPut1K$' -benchmem -benchtime=100x -count 5
REDIS_ADDR=127.0.0.1:6379 go test -run '^$' -bench '^BenchmarkDurableBatchPut$' -benchmem -benchtime=20x -count 5
REDIS_ADDR=127.0.0.1:6379 go test -run '^$' -bench '^BenchmarkGetMany1K$' -benchmem -benchtime=100x -count 5
REDIS_ADDR=127.0.0.1:6379 go test -run '^$' -bench '^BenchmarkPutTTL$' -benchmem -benchtime=1000x -count 5
REDIS_ADDR=127.0.0.1:6379 go test -run '^$' -bench '^BenchmarkPutTTL1K$' -benchmem -benchtime=100x -count 5
REDIS_ADDR=127.0.0.1:6379 go test -run '^$' -bench '^BenchmarkUniquePutTTL1K$' -benchmem -benchtime=100x -count 5
REDIS_ADDR=127.0.0.1:6379 go test -run '^$' -bench '^BenchmarkExpiredGetMany1K$' -benchmem -benchtime=100x -count 5
REDIS_ADDR=127.0.0.1:6379 go test -run '^$' -bench '^BenchmarkRange50K$' -benchmem -benchtime=5x -count 10
REDIS_ADDR=127.0.0.1:6379 go test -run '^$' -bench '^BenchmarkRangeNoCopy50K$' -benchmem -benchtime=5x -count 10
```

`BatchPut1K` overwrites a fixed set of 1,000 keys per operation. All backends
encode the same payloads with the Codec and commit each operation atomically;
it reports both time per batch and records per operation. The embedded-only
command uses 100 batches per sample because each operation writes 1,000 keys.

`UniqueBatchPut1K` inserts 1,000 new keys per operation in one atomic write.
The database grows during each sample to expose index and append-log scaling;
fixture creation remains outside the timed section. The companion
`RandomUniqueBatchPut1K` benchmark permutes key suffixes deterministically
within each batch to show sensitivity to insertion order.

For a larger growth sample, run `go test -run '^$' -bench '^BenchmarkUniqueBatchPut1K$' -benchmem -benchtime=500x -count=3` (set `REDIS_ADDR` to include Redis).
For a one-million-record growth sample, use `-benchtime=1000x -count=3` with the same command.

`DurableBatchPut` overwrites a fixed set of 1, 100, 1,000, or 10,000 keys in one
atomic write with per-commit syncing enabled: Codec `SyncAlways`, Badger
`SyncWrites(true)`, and bbolt's default sync behavior. Redis is included only
when AOF is enabled with `appendfsync=always`; the benchmark checks these
settings and skips Redis when they cannot be verified. Start a dedicated local Redis server with
`redis-server --appendonly yes --appendfsync always` before running this case.

`CleanReopen50K` measures only `Open` on a cleanly closed 50,000-key fixture;
fixture creation and each `Close` are outside the timer. It measures checkpoint
loading, not crash recovery. bbolt opens lazily without enumerating every key,
so its startup time does not mean the same amount of data has been touched as
Codec or Badger at return from `Open`.

`LogReplay50K` removes Codec's checkpoint before each timed open, forcing the
store to verify and replay all committed records. The fixture is closed
cleanly before the checkpoint is removed, so this measures full log replay but
does not simulate process failure or power loss.

`PutTTL` overwrites one key with a one-hour expiry and measures native expiry
metadata writes in Codec, Badger, and optional Redis. bbolt is excluded because
it has no native TTL operation. Setup and close are outside the timed region;
all backends use non-fsync write modes.

`PutTTL1K` atomically overwrites the same 1,000 keys with one-hour TTLs per
operation. This keeps the expiry scheduler at a steady 1,000 live entries
while measuring batch encoding, transaction cost, and TTL metadata handling.

`UniquePutTTL1K` inserts 1,000 new keys with one-hour TTLs per operation.
This grows the live expiry index through each sample; use `-benchtime=500x`
to reach 500,000 expiring keys. Codec's background expiration worker is
disabled, keeping the measurement focused on writes and expiry-index updates.

`ExpiredGetMany1K` reads the same 1,000 keys after a one-second TTL elapses.
Fixture creation and the expiration wait are outside the timed region; Codec's
background tombstone worker is disabled to isolate logical expiry checks.

`Range50K` decodes all 50,000 values per operation using each backend's ordered
iterator. It compares Codec's chunked `Range`, copying `RangeSnapshot`, and
borrowed `RangeSnapshotNoCopy`; each backend produces a string key per record
to match Codec's callback shape. The no-copy callback does not retain decoded
fields. Each operation is a full traversal, so `5x` provides a handful of
samples while keeping setup outside the timed section.

`RangeBorrowed50K` measures the snapshot iterator with borrowed key/value
lifetimes. It compares Codec, Badger, and bbolt without forcing key strings;
callbacks consume each key and decoded value before advancing.

`TakeRange50K` visits a bounded key range with owned string keys and decoded
values. Codec uses the chunked `TakeRange` callback; Badger and bbolt use a
single read transaction and seek to the same inclusive lower and exclusive
upper bounds.

`Page50K` materializes owned key/value pages of 256 entries. Codec calls
`Page` repeatedly with its returned cursor; Badger and bbolt iterate in one
read transaction and build/reset equivalent local pages. This compares the
cost of returning owned pages, with each backend using its native cursor model.

`Stream50K` measures Codec's `Page`, `Stream`, `StreamResults`, and
`StreamBorrowed` over the same 50K keys. The channel APIs return owned items;
`StreamBorrowed` invokes a callback with borrowed keys and values in bounded
chunks, so the callback must consume them immediately and must not call the DB.

The 50K fixture contains 5,000 sorted user prefixes with 10 fields each;
`Get50K` visits varying keys, `Get50KWarm` reads the whole set before timing,
and `ScanPrefix50K` decodes the 10 values under one prefix. `ParallelGet50K`
uses the Go benchmark parallel worker count. Redis uses `SCAN MATCH` plus
`MGET`, which necessarily scans the keyspace. Use the same Go version, machine, filesystem, background load,
benchmark count, and server configuration for comparisons. Root store
benchmarks use different payloads and setups, so their results are not directly
comparable to this suite.
