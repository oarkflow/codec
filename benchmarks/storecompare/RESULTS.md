# Store benchmark summary

Local results for the 50K-key fixture unless noted. Values are median time per
operation; `1K` means one operation over 1,000 records or keys, and `50K` means
one full traversal. Lower is faster. Embedded stores used non-sync writes
except in the durable section; Redis ran over loopback. These are workload
comparisons, not universal rankings.

## Writes and expiry

| Workload | Codec | Badger | bbolt | Redis | Comparison |
|---|---:|---:|---:|---:|---|
| Repeated atomic batch, 1K writes | 358 µs | 1,184 µs | 547 µs | 1,609 µs | Codec led all three |
| Single-key TTL overwrite | 5.31 µs | 7.32 µs | — | 91.2 µs | Codec led Badger and Redis |
| Repeated TTL batch, 1K keys | 496 µs | 1,086 µs | — | 1,861 µs | Codec led Badger and Redis |
| Unique TTL growth, 1K keys/batch | 809 µs | 1,068 µs | — | 2,470 µs | Codec led Badger and Redis; 500K live keys |
| Unique writes, 1K keys/batch | 530 µs | 1,021 µs | 1,184 µs | 1,740 µs | Codec led all three; 100K growth |
| Unique writes, 1K keys/batch | 536 µs | 1,044 µs | 933 µs | 1,858 µs | Codec led Badger and Redis; bbolt was 1.74× faster; 1M growth |
| Expired-key read, 1K keys | 151 µs | 571 µs | — | 530 µs | Codec led Badger and Redis |

### Durable atomic commits

Per-commit sync enabled for Codec, Badger, and bbolt; Redis AOF with
`appendfsync=always`. Time in milliseconds per atomic commit.

| Records/commit | Codec | Badger | bbolt | Redis |
|---:|---:|---:|---:|---:|
| 1 | 0.032 | 0.069 | 5.711 | 3.027 |
| 100 | 0.307 | 0.353 | 5.242 | 2.613 |
| 1,000 | 0.653 | 1.247 | 5.159 | 4.577 |
| 10,000 | 3.639 | 14.357 | 10.675 | 20.727 |

Codec had the lowest median at each size in these samples. Storage and
filesystem behavior is machine-specific.

## Reads and traversal

| Workload | Codec | Badger | bbolt | Redis | Comparison |
|---|---:|---:|---:|---:|---|
| Point `Get`, fixed hot key | 1.19 µs | 1.65 µs | 1.22 µs | 91.8 µs | Codec near bbolt; cache-enabled Codec measured 0.80 µs |
| `GetMany`, 1K owned results | 803 µs | 772 µs | 992 µs | 1,551 µs | Codec slightly behind Badger; ahead of bbolt and Redis |
| `GetManyBorrowed`, 1K callbacks | 155 µs | 772 µs | 992 µs | 1,551 µs | Codec led; 5 allocs vs 8K–14K |
| Varied point reads, 50K fixture | 1.71 µs | 2.12 µs | 1.48 µs | 89.2 µs | Codec led Badger and Redis; bbolt led Codec by 6% |
| Parallel varied point reads | 0.93 µs | 1.76 µs | 2.60 µs | 18.7 µs | Codec led all three |
| Prefix scan + decode 10 values | 4.37 µs | 13.3 µs | 9.00 µs | — | Codec led both embedded peers |
| Full owned `Range`, 50K values | 8.13 ms | 23.9 ms* | 7.79 ms | — | Codec led Badger; bbolt led Codec by 4% |
| Full `RangeSnapshot`, 50K values | 6.57 ms | 23.9 ms* | 7.79 ms | — | Codec led both in this run |
| Bounded `TakeRange`, 50K values | 8.21 ms | — | 8.03 ms | — | Near bbolt in this sample |
| Borrowed `RangeSnapshot`, 50K values | 5.70 ms | 23.5 ms* | 5.93 ms | — | Codec led both in the measured run |
| Borrowed bounded `TakeRange`, 50K values | 7.10 ms | — | 5.93 ms* | — | Chunked path trades throughput for writer opportunities |
| `KeysSnapshot`, cold / repeated 100 calls | 1.51 ms / 0.42 ms | — | 1.75 ms / 1.08 ms | — | Codec led both cases |
| `ScanKeys`, 50K keys | 1.48 ms | — | 0.89 ms | — | bbolt faster; Codec yields its read lock every 256 keys |
| Owned `Page` loop, 50K values | 33.25 ms | 47.78 ms | 51.18 ms | — | Codec led both; page materialization is allocation-heavy |
| Owned channel stream, 50K values | 36.0 ms | — | — | — | Similar to Codec `Page` loop |
| Borrowed stream, 50K values | 7.83 ms | — | — | — | 7 KB/op and 55 allocs vs ~263 MB/op for owned stream |

`*` Comparator timing came from a separate run; treat as directional. Borrowed
APIs require callback data to be consumed before the callback returns. Full
snapshots hold a stable read view; `TakeRange` yields between bounded chunks.

## Startup and recovery

| Workload | Codec | Badger | bbolt | Summary |
|---|---:|---:|---:|---|
| Clean open, 50K keys | 3.75 ms | 4.25 ms | 0.045 ms | Codec beat Badger; bbolt opens lazily and does not build an equivalent in-memory index |
| Full Codec log replay, 50K keys | 9.56 ms | — | — | 2.55× Codec clean-open time; no crash/power-loss comparison |

## Scope

Most comparisons use one Apple M2 Pro, one fixture, and short benchmark runs.
Redis is local loopback, so its numbers include client/server transport; they
do not represent remote Redis. Durable commits use local storage and should
not be generalized across filesystems. The suite does not yet compare actual
process-failure or power-loss recovery, or compaction. Use the commands in
[README.md](README.md) and longer repeated runs for new comparisons.
