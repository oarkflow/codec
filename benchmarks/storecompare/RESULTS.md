# Store comparison results

Median results from the local benchmark suite. Lower time is faster. `—` means
the backend or operation was not measured. See [README.md](README.md) for setup,
commands, and workload definitions.

| Area | Workload | Codec | Badger | bbolt | Redis | Summary |
|---|---|---:|---:|---:|---:|---|
| Writes | Atomic batch, 1K records | 358 µs | 1,184 µs | 547 µs | 1,609 µs | Codec fastest |
| Writes | Unique batch, 1K records; 100K growth | 530 µs | 1,021 µs | 1,184 µs | 1,740 µs | Codec fastest |
| Writes | Unique batch, 1K records; 1M growth | 536 µs | 1,044 µs | 933 µs | 1,858 µs | Codec fastest except bbolt |
| Expiry | TTL overwrite, one key | 5.31 µs | 7.32 µs | — | 91.2 µs | Codec fastest among measured TTL backends |
| Expiry | Expired reads, 1K keys | 151 µs | 571 µs | — | 530 µs | Codec fastest among measured TTL backends |
| Durable writes | Synced atomic commit, 1 record | 0.032 ms | 0.069 ms | 5.711 ms | 3.027 ms | Codec fastest |
| Durable writes | Synced atomic commit, 10K records | 3.639 ms | 14.357 ms | 10.675 ms | 20.727 ms | Codec fastest |
| Reads | Varied point reads, 50K fixture | 1.71 µs | 2.12 µs | 1.48 µs | 89.2 µs | bbolt fastest; Codec ahead of Badger and Redis |
| Reads | Parallel varied reads, 50K fixture | 0.93 µs | 1.76 µs | 2.60 µs | 18.7 µs | Codec fastest |
| Reads | `GetMany`, 1K owned values | 803 µs | 772 µs | 992 µs | 1,551 µs | Badger fastest; Codec close |
| Reads | `GetManyBorrowed`, 1K callbacks | 155 µs | 772 µs | 992 µs | 1,551 µs | Codec fastest; 5 vs 8K–14K allocations |
| Scans | Prefix scan, decode 10 values | 4.37 µs | 13.3 µs | 9.00 µs | — | Codec fastest |
| Scans | Full owned range, 50K values | 8.13 ms | 23.9 ms* | 7.79 ms | — | bbolt fastest; Codec ahead of Badger |
| Scans | Full borrowed snapshot, 50K values | 5.70 ms | 23.5 ms* | 5.93 ms | — | Codec fastest in this run |
| Scans | `Page` loop, 50K values | 33.25 ms | 47.78 ms | 51.18 ms | — | Codec fastest |
| Scans | Borrowed stream, 50K values | 7.83 ms | — | — | — | 7 KB/op vs ~263 MB/op for owned stream |
| Startup | Clean open, 50K keys | 3.75 ms | 4.25 ms | 0.045 ms | — | Codec ahead of Badger; bbolt opens lazily |

`*` Comparator scan values came from a separate run and are directional. Results
are from one Apple M2 Pro, one fixture, and short local runs; Redis used loopback.
Durability and performance vary with storage and filesystem. These measurements
do not cover crash recovery or compaction, and are not universal rankings.
