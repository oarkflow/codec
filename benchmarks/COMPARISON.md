# Codec comparison summary

Captured 2026-11-23 on an Apple M2 Pro, macOS arm64, Go 1.27.4. These are
diagnostic synthetic workloads, not a universal ranking. Each Go benchmark was
run for 300 ms, three times, with `-benchmem`; times below are the median of the
three reported `ns/op` samples. The codec suite reuses prebuilt values and decode
targets. Its `doc` and `batch1000` cases compare the same Go values; Protobuf
uses a prebuilt equivalent message. Dynamic maps have no Protobuf equivalent.

## Wire codec workloads

Lower time and allocations are better. Codec is fastest in each measured
workload below. Its append encoder reuses a caller-owned buffer; therefore the
zero encode allocations are expected and should be compared with that API
choice in mind.

| Workload | Codec time / allocs | Next fastest | Codec faster |
|---|---:|---:|---:|
| Typed document encode | 374 ns / 0 | Protobuf, 494 ns | 24% |
| Typed document decode | 445 ns / 1 | Protobuf, 802 ns | 45% |
| 1,000-row batch encode | 45.97 µs / 0 | Protobuf, 58.49 µs | 21% |
| 1,000-row batch decode | 14.26 µs / 1 | Protobuf, 88.81 µs | 84% |
| Dynamic map encode | 1.01 µs / 0 | CBOR, 1.64 µs | 38% |
| Dynamic map decode | 1.24 µs / 8 | MessagePack (shamaton), 2.42 µs | 49% |

The comparison also included CBOR, two Go MessagePack implementations, and
`encoding/json`. They were slower than Codec for all six workloads. Allocation
counts vary substantially by format and workload; see the full raw output from
`scripts/benchmarks.sh` when making a workload-specific decision. The benchmark
module can be run independently with:

```sh
cd benchmarks
go test -run '^$' -bench 'Benchmark(Marshal|Unmarshal)$' -benchmem -benchtime=300ms -count=3
```

## Persistent store workloads

Captured in the same environment, using the comparison suite's common typed
payload, with 100 ms per benchmark and three samples. Codec's default write
mode, Badger `SyncWrites(false)`, and bbolt `NoSync` are used; these are not
power-loss durability comparisons. Redis was not configured and is excluded.

| Operation | Codec | Badger | bbolt | Result |
|---|---:|---:|---:|---|
| Single-key Put | 2.23 µs | 5.93 µs | 11.97 µs | Codec fastest |
| Single-key Get, no Codec cache | 1.30 µs | 1.04 µs | 1.25 µs | Badger fastest; Codec close |
| Single-key Get, Codec read cache | 0.94 µs | 1.04 µs | 1.25 µs | Codec fastest |
| Atomic batch of 1,000 writes | 339 µs | 1,108 µs | 510 µs | Codec fastest |
| GetMany, 1,000 owned values | 781 µs | 713 µs | 947 µs | Badger fastest; Codec ahead of bbolt |
| Prefix scan, 10 decoded values | 3.46 µs | 13.19 µs | 9.22 µs | Codec fastest |
| Borrowed range, 50,000 values | 5.79 ms | 27.92 ms | 6.33 ms | Codec fastest in this run |

The borrowed range APIs have different callback and allocation semantics than
owned-return APIs. The 50K traversal sample is especially sensitive to machine
load and filesystem/cache state. Reproduce it with:

```sh
cd benchmarks/storecompare
go test -run '^$' -bench '^(Benchmark(Put|Get|BatchPut1K|GetMany1K|ScanPrefix50K|RangeBorrowed50K))$' -benchmem -benchtime=300ms -count=3
```

`../../scripts/benchmarks.sh` runs both comparison suites along with the root Go
and TypeScript diagnostic benchmarks. Results from different machines, runtime
versions, storage devices, or durability modes should not be compared as if
they were controlled head-to-head trials. Codec wins these tested encoding,
batch-write, prefix-scan, and borrowed-range cases; Badger leads the tested
uncached point read and owned `GetMany` cases. Choose based on the intended
workload and durability requirements.
