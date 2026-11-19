# Local 50K-key results

## Atomic 1K-record writes

`BenchmarkBatchPut1K` overwrites the same 1,000 keys per operation. Five
samples of 100 writes each, without fsync, measured:

| Backend | Median ns per 1K-record commit (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `Batch` | 358,087 (329,224–509,396) | 346,722 | 5,028 |
| Badger v4.9.6 | 1,184,042 (1,085,735–1,214,128) | 817,779 | 19,061 |
| bbolt v1.5.0 | 546,975 (536,225–576,237) | 528,899 | 9,249 |
| Redis `MULTI` + pipeline | 1,609,296 (1,595,516–1,776,012) | 609,134 | 11,028 |

Codec was about 3.3x faster than Badger, 1.53x faster than bbolt, and 4.5x
faster than Redis by median, with fewer allocations and bytes per batch. The
repeated-key workload keeps index size bounded and measures encoding plus the
atomic commit; it does not measure durable fsync latency or sustained growth.
Redis ran on loopback with persistence disabled.

## Single-key TTL overwrites

`BenchmarkPutTTL` repeatedly overwrites one value with a one-hour TTL. Five
samples of 1,000 operations each were run with Codec, Badger v4.9.6, and Redis
on loopback; setup and close were outside the timed region. All backends used
non-fsync writes, and Redis persistence was disabled.

| Backend | Median ns/op (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `PutTTL` | 5,313 (3,081–8,473) | 2 | 0 |
| Badger TTL transaction | 7,315 (5,538–9,606) | 1,810 | 42 |
| Redis `SET` with TTL | 91,244 (87,545–94,914) | 601 | 14 |

Codec was about 1.4x faster than Badger and 17x faster than loopback Redis by
median in this repeated-key write workload. The sample ranges overlap for
Codec and Badger, and the short samples are sensitive to background activity.
This measures TTL overwrite cost, not expiry backlog cleanup or durability.
Redis's result includes client/server and loopback transport overhead.

### Atomic 1K-key TTL batches

`BenchmarkPutTTL1K` overwrites the same 1,000 keys with one-hour TTLs in one
atomic operation. Five samples of 100 operations were run on the same Apple
M2 Pro, with setup and close excluded. Codec and Badger used non-fsync writes;
Redis ran on loopback with persistence disabled and used `MULTI` plus a
pipeline. Reusing the same key set keeps the live expiry count at 1,000.

| Backend | Median ns per 1K-key batch (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `Batch.PutTTL` | 496,424 (465,674–651,395) | 348,339 | 5,038 |
| Badger TTL transaction | 1,085,549 (1,068,547–1,165,107) | 816,999 | 19,061 |
| Redis `MULTI` + TTL pipeline | 1,861,286 (1,850,010–1,876,353) | 617,134 | 12,028 |

Codec was about 2.2x faster than Badger and 3.8x faster than loopback Redis by
median, with fewer bytes and allocations per batch. This repeated-key,
non-fsync fixture measures TTL writes and scheduler updates; it does not
measure power-loss durability or expiry backlog cleanup. bbolt has no native
TTL operation and is not included.

### Unique-key 1K TTL growth

`BenchmarkUniquePutTTL1K` inserts fresh keys in each batch, with one-hour
expiries, growing each sample by 500,000 live keys. Three samples were run on
the same Apple M2 Pro with non-fsync settings. Redis ran on loopback with
persistence disabled.

| Backend | Median ns per 1K-key batch (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `Batch.PutTTL` | 809,211 (769,692–827,913) | 837,325 | 12,908 |
| Badger TTL transaction | 1,067,906 (1,060,428–1,130,477) | 1,504,306 | 23,518 |
| Redis `MULTI` + TTL pipeline | 2,469,998 (2,448,029–2,509,853) | 800,017 | 16,261 |

Codec's median was 1.32x faster than Badger and 3.05x faster than loopback
Redis in this 500K-key growth run, with fewer allocations than both. The test
uses one machine, payload, and persistence configuration; it is not a general
performance ranking.

## Durable atomic batches by size

`BenchmarkDurableBatchPut` overwrites fixed key sets of 1, 100, 1,000, or 10,000
records per atomic commit with per-commit sync enabled: Codec `SyncAlways`,
Badger `SyncWrites(true)`, and bbolt's default transaction sync. Redis ran on
loopback with AOF enabled and `appendfsync=always`; the benchmark verifies
those settings. Five samples of 20 operations per size were run because each
commit synchronizes storage.

| Records/commit | Codec median ns/op (range) | Badger median ns/op (range) | bbolt median ns/op (range) | Redis median ns/op (range) |
|---:|---:|---:|---:|---:|
| 1 | 32,206 (24,831–40,456) | 68,796 (58,092–116,662) | 5,711,200 (5,519,340–5,881,927) | 3,026,942 (2,141,510–3,305,383) |
| 100 | 306,698 (295,669–364,421) | 352,556 (261,798–397,402) | 5,242,410 (4,919,102–5,397,762) | 2,613,396 (2,387,671–2,967,052) |
| 1,000 | 653,469 (525,765–914,160) | 1,247,179 (1,176,560–1,377,719) | 5,159,444 (4,589,842–9,751,910) | 4,577,342 (4,196,369–5,230,208) |
| 10,000 | 3,639,492 (3,514,212–4,109,669) | 14,357,383 (12,821,379–15,808,621) | 10,675,481 (10,435,602–11,550,062) | 20,726,504 (20,323,581–21,476,910) |

Codec was about 2.1x faster than Badger on single-record commits, 1.15x faster
on 100-record commits, 1.9x faster on 1,000-record commits, and 3.9x faster on
10,000-record commits by median. It also led bbolt and loopback Redis at all
four sizes in this run. The 100-record Codec/Badger ranges overlap, so that
small-commit lead is directional. On Linux and macOS, `SyncAlways`
opens the active segment for data-synchronous appends; other platforms retain
the `File.Sync` fallback. Redis used a temporary local AOF with
`appendfsync=always`. These storage- and filesystem-specific results remain
directional, not a universal durability ranking.

## Clean reopen of 50K keys

`BenchmarkCleanReopen50K` measures `Open` after a clean `Close` on a prepared
50,000-key dataset. Five samples of 10 opens were run on the same Apple M2 Pro;
all close calls and fixture creation were outside the timed region. This
measures clean checkpoint/index startup, not crash recovery.

| Backend | Median Open ns/op (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec | 3,751,063 (3,551,279–4,082,546) | 10,086,730 | 5,755 |
| Badger v4.9.6 | 4,250,783 (4,154,200–5,413,712) | 89,810,213 | 4,088 |
| bbolt v1.5.0 | 45,346 (40,687–51,342) | 1,816 | 21 |

Codec opened this fixture about 1.13x faster than Badger by median and used
about 8.9x fewer allocated bytes. Building the radix tree from sorted entries
in node and label arenas reduced Codec's allocations from about 117K to 5.8K
per open; allocated bytes rose from about 9.4 MB to 10.1 MB. bbolt's open is
about 83x faster than Codec because bbolt maps its database
without enumerating all keys at open; the number is not an equivalent
ready-to-query measurement. This exposes a current trade-off: Codec rebuilds
its in-memory radix index eagerly before returning from `Open`.

### Full log replay

`BenchmarkLogReplay50K` removes the Codec `INDEX` checkpoint before each timed
open, so Codec verifies and replays all committed records. Five samples of 10
opens on the same machine measured 9,564,633 ns/op (9,450,488–9,835,037),
11,015,660 B/op, and 233,414 allocs/op. This is about 2.55x the clean-reopen
median above. The fixture was closed cleanly before the checkpoint was
removed; this measures replay behavior, not recovery from process failure or
power loss. No equivalent cross-backend crash-recovery ranking is available.

## Unique-key 1K batches

`BenchmarkUniqueBatchPut1K` inserts 1,000 new keys in each atomic batch rather
than overwriting a fixed set. Five samples of 100 batches each grew the
dataset by 100,000 keys per sample. Codec, Badger, and bbolt used non-fsync settings;
Redis ran on loopback with persistence disabled and used a transaction plus
pipeline.

| Backend | Median ns per 1K-key batch (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `Batch` | 530,593 (515,280–699,307) | 644,291 | 11,413 |
| Badger transaction | 1,021,137 (992,300–1,084,147) | 1,023,542 | 22,805 |
| bbolt transaction | 1,184,195 (1,156,329–1,215,187) | 994,060 | 13,950 |
| Redis `MULTI` + pipeline | 1,739,923 (1,722,314–1,780,725) | 783,911 | 14,774 |

Codec was about 1.9x faster than Badger, 2.2x faster than bbolt, and 3.3x
faster than loopback Redis by median. This short growth run does not establish
performance at much larger database sizes or under durable-write settings.

### Shuffled unique-key batches

To check sensitivity to the naturally sorted suffixes above, five samples of
100 batches permuted the 1,000 suffixes deterministically within each batch.
The run was on the same Apple M2 Pro, with Redis unavailable and therefore
skipped. Each sample again grew the database by 100,000 keys.

| Backend | Sorted median ns/op | Shuffled median ns/op | Shuffled B/op | Shuffled allocs/op |
|---|---:|---:|---:|---:|
| Codec `Batch` | 547,018 | 611,929 (579,108–625,834) | 644,303 | 11,413 |
| Badger transaction | 1,094,530 | 1,069,573 (1,022,005–1,138,006) | 1,023,908 | 22,805 |
| bbolt transaction | 1,061,481 | 1,359,430 (1,333,529–1,431,862) | 992,110 | 13,950 |

Codec remained faster in this shuffled 100K-growth workload: about 1.75x
Badger and 2.22x bbolt by median. Its median time increased about 12% from the
sorted run, while bbolt increased about 28%. This is one deterministic key
permutation on one machine, not a general performance guarantee.

### Extended 500K-record growth run

Three samples of 500 batches each grew each database by 500,000 new records
per sample. The same atomic transaction and non-fsync settings were used.

| Backend | Median ns per 1K-key batch (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `Batch` | 617,299 (602,732–663,159) | 648,050 | 11,900 |
| Badger transaction | 1,065,377 (1,058,636–1,124,951) | 1,504,433 | 23,520 |
| bbolt transaction | 1,049,516 (1,001,954–1,205,156) | 1,009,251 | 14,376 |
| Redis `MULTI` + pipeline | 1,941,322 (1,938,992–1,975,238) | 791,996 | 15,261 |

At this 500K-record scale Codec was about 1.7x faster than Badger and bbolt,
and 3.1x faster than loopback Redis by median. Codec also used fewer bytes
and allocations per batch. This still uses one machine and non-fsync writes;
the 1M-record result is reported separately below, and neither run covers
durable-write performance.

### Extended 1M-record growth run

Three samples of 1,000 batches each inserted one million new records per
backend per sample. Each operation remained one atomic 1K-record batch, with
non-fsync settings for embedded stores and persistence disabled for loopback
Redis.

| Backend | Median ns per 1K-key batch (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `Batch` | 536,027 (531,868–566,281) | 649,982 | 12,155 |
| Badger transaction | 1,044,358 (1,041,454–1,081,648) | 1,377,167 | 23,777 |
| bbolt transaction | 933,191 (915,120–936,905) | 1,014,680 | 14,622 |
| Redis `MULTI` + pipeline | 1,857,996 (1,848,614–1,871,988) | 794,578 | 15,517 |

At this scale Codec's median batch time was about 1.95x faster than Badger,
1.74x faster than bbolt, and 3.47x faster than Redis. Codec also allocated
fewer bytes and objects per batch than each peer. This extends the non-fsync
growth result to one million keys; durability, recovery, and larger datasets
remain separate workloads.

## Batched 1K-key reads

`BenchmarkGetMany1K` reads the same 1,000 keys per operation. Five samples of
100 reads each measured:

| Backend | Median ns per 1K-key read (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `GetMany` | 803,446 (795,728–1,093,246) | 5,154,852 | 5,003 |
| Codec individual `Get` loop | 1,237,572 (1,218,660–1,262,886) | 5,165,202 | 6,008 |
| Badger transaction | 772,052 (764,386–819,786) | 5,664,230 | 14,001 |
| bbolt read transaction | 991,583 (962,450–1,106,748) | 5,235,294 | 8,011 |
| Redis MGET over loopback | 1,550,772 (1,527,385–1,574,876) | 5,452,479 | 10,012 |

`GetMany` was about 35% faster than Codec's individual `Get` loop, about 4%
slower than Badger, 19% faster than bbolt, and 1.9x faster than Redis MGET by
median. It used fewer allocations than every comparison. Redis ran locally
with persistence disabled; these fixture results do not represent remote
service latency or larger key distributions.

### Borrowed callback read

Five samples of 100 reads each on the same 1,000-key fixture measured the
`GetManyBorrowed` callback API against Codec's other read methods, Badger,
bbolt, and Redis MGET over loopback.

| Backend | Median ns per 1K-key read (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `GetManyBorrowed` | 155,132 (154,453–156,741) | 5,096 | 5 |
| Codec `GetMany` | 803,446 (795,728–1,093,246) | 5,154,852 | 5,003 |
| Codec individual `Get` loop | 1,237,572 (1,218,660–1,262,886) | 5,165,202 | 6,008 |
| Badger transaction | 772,052 (764,386–819,786) | 5,664,230 | 14,001 |
| bbolt read transaction | 991,583 (962,450–1,106,748) | 5,235,294 | 8,011 |
| Redis MGET over loopback | 1,550,772 (1,527,385–1,574,876) | 5,452,479 | 10,012 |

The borrowed callback path was about 5x faster than Badger and 10x faster than
Redis MGET, using 5 allocations for the entire 1,000-key read. Its median
latency was about 5x lower than `GetMany`; the APIs have different ownership costs because
`GetManyBorrowed` visits one reused value instead of returning 1,000 owned
values. Five samples of 100 operations were run without Redis. This is a local
workload result, not a general performance guarantee.

## Borrowed point read

`GetBorrowed` decodes one key without copying strings (and byte slices inside
interface values), then
invokes a callback while the store holds a read lock. Five 5,000-operation
samples repeatedly read the same one-record fixture; Redis ran over loopback
with persistence disabled.

| Backend | Median ns/op (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `GetBorrowed` | 1,194 (1,113–1,234) | 5,036 | 4 |
| Codec `Get` | 1,457 (1,367–1,696) | 5,165 | 6 |
| Codec `Get` with 1 MiB cache | 796 (745–828) | 5,163 | 6 |
| Badger | 1,649 (1,506–1,804) | 5,792 | 15 |
| bbolt | 1,221 (1,116–1,448) | 5,755 | 13 |
| Redis | 91,781 (89,620–96,458) | 5,490 | 13 |

Borrowed Codec was about 18% faster than ordinary Codec `Get`, and close to
bbolt in this fixed-key workload while allocating fewer objects. The optional
read cache was fastest among the embedded rows for this one hot key. These
results do not cover varied-key cache misses or values retained after the
callback; borrowed fields must not escape the callback.

### Varied keys across the 50K fixture

Three samples of 5,000 lookups each visited 5,000 different keys from the
50,000-record fixture. Redis ran over loopback with persistence disabled.

| Backend | Varied Get median ns/op (range) | Warm varied Get median ns/op (range) | Median allocs/op, varied / warm |
|---|---:|---:|---:|
| Codec `Get` | 1,711 (1,708–1,714) | 1,091 (1,066–1,122) | 8 / 8 |
| Codec `GetBorrowed` | 1,563 (1,517–1,814) | 1,017 (1,009–1,101) | 6 / 6 |
| Codec `Get` with 8 MiB cache | — | 1,198 (1,196–1,200) | — / 8 |
| Badger | 2,118 (1,976–2,266) | 1,720 (1,639–1,746) | 17 / 17 |
| bbolt | 1,481 (1,440–1,761) | 1,599 (1,570–1,635) | 17 / 17 |
| Redis | 89,224 (87,303–89,950) | 86,022 (85,784–87,426) | 15 / 15 |

Borrowed reads reduced Codec's median by about 9% on varied keys and 7% on the
warm pass, with two fewer allocations per operation. Codec remained about 6%
slower than bbolt on the varied pass by median, with overlapping ranges; it
led the measured embedded peers on the warm pass. These are three-sample
directional results, not a general lead.

### Parallel varied reads

Three samples of 5,000 varied-key reads ran with Go's default parallel worker
count of 10 on the same fixture. Redis ran over loopback with persistence
disabled.

| Backend | Median ns/op (range) | Median allocs/op |
|---|---:|---:|
| Codec `Get` | 926 (906–958) | 8 |
| Codec `GetBorrowed` | 908 (864–933) | 6 |
| Codec `Get` with 8 MiB cache | 637 (619–790) | 8 |
| Badger | 1,762 (1,644–1,890) | 17 |
| bbolt | 2,601 (2,507–2,612) | 18 |
| Redis | 18,696 (18,297–23,517) | 16 |

Borrowed Codec retained nearly the same latency as ordinary `Get` while using
two fewer allocations. The two Codec ranges overlap; both led the uncached
embedded peers by median in this run. The cache variant was fastest for this
resident working set, at the cost of bounded memory. These are short,
machine-specific parallel samples.

## mmap read path update

After enabling read-only mappings for immutable segments, the suite was rerun
with 5,000 operations per sample and three samples (Redis was not running).
The 8 MiB read cache was warmed across the full 50K-key fixture and retained
6.374 MiB of key/value bytes. These short samples are directional; compare on
the same machine and use longer runs before drawing conclusions.

| Operation | Codec | CodecCached | Badger v4.9.6 | bbolt v1.5.0 |
|---|---:|---:|---:|---:|
| Varied Get + decode, median ns/op (range) | 1,501 (1,465–1,544) | — | 2,091 (1,994–2,255) | 1,576 (1,471–1,666) |
| Warm full-set Get + decode, median ns/op (range) | 1,188 (1,136–1,205) | 1,123 (1,114–1,170) | 1,654 (1,616–1,745) | 1,646 (1,595–1,685) |
| Parallel Get + decode, median ns/op (range) | 905 (896–906) | 770 (658–777) | 1,683 (1,571–1,701) | 2,623 (2,599–2,667) |
| Prefix scan + decode 10 records, median ns/op (range) | 4,369 (4,171–4,788) | — | 13,334 (13,328–13,747) | 9,001 (8,966–9,537) |

Codec led Badger and was close to bbolt on varied Get, led both on warm and
parallel Get, and led both on the measured prefix scan. This is one in-memory
fixture on one machine; it does not establish an across-the-board performance
lead.

### Copying-value comparison before the full-tree fast path

This run decodes all 50,000 values per operation with copying decoders. Ten
samples of five traversals each, normalized per operation and without Redis,
produced:

| Backend | Median ns per 50K traversal (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `Range` | 11,921,667 (10,375,575–12,195,758) | 4,820,728 | 100,208 |
| Codec `RangeSnapshot` | 10,630,542 (10,057,042–11,108,750) | 4,806,680 | 100,009 |
| Badger iterator | 23,570,575 (22,018,117–26,459,783) | 5,673,497 | 150,718 |
| bbolt cursor | 7,488,200 (7,405,717–7,609,975) | 4,806,423 | 100,016 |

Codec beat Badger in this full traversal. Copying `RangeSnapshot` was about
1.4x slower than bbolt, while chunked `Range` was about 1.6x slower.
Passing indexed locations through the scan removed a repeated tree lookup;
reusing the per-scan candidate buffer keeps allocation bytes near 4.8 MiB.
Snapshot scans trade writer latency for a single locked traversal and map the
active segment locally for the duration of the scan on supported Unix systems.
The no-copy callback must not retain decoded fields. The comparison includes
string-key production for every backend to match Codec's callback contract. A
range-scan gap remains despite Codec's advantage on a 10-record prefix scan.
The fixture and backend-specific iteration APIs make this a workload
comparison, not a universal ranking.
Redis was not measured in this run because the environment denied binding a
local loopback port.

### Local Redis scan comparison

On the same Apple M2 Pro, Redis ran on loopback with persistence disabled.
`BenchmarkScanPrefix50K` measures a 10-record prefix result; `BenchmarkRange50K`
measures all 50K values, with Redis using `SCAN` and `MGET` batches. Five
samples of five operations each measured:

| Operation | Codec | Badger | bbolt | Redis |
|---|---:|---:|---:|---:|
| Prefix scan, 10 values (median ns/op; range) | 12,033 (10,325–22,492) | 28,000 (23,883–35,217) | 14,833 (10,592–18,250) | 8,715,717 (8,410,850–9,169,258) |
| Full range, 50K values (median ns/op; range) | 10,177,633 (9,452,383–10,869,133) | 32,850,267 (29,435,958–34,660,242) | 8,166,800 (8,079,842–8,227,767) | 46,900,008 (46,074,917–47,906,750) |

On this fixture Codec was about 2.3x faster than Badger and 724x faster than
Redis for the 10-value prefix scan. For the full traversal it was about 3.2x
faster than Badger and 4.6x faster than Redis, while bbolt was about 25%
faster than Codec. Redis's full scan used about 20.1 MB/op and 300,929
allocations; Codec used about 4.8 MB/op and 100,010 allocations. The prefix
sample is very short and remote Redis would add network latency; these numbers
are specific to this local workload.

### Owned traversal before the stable-key cache

Five samples of 10 full traversals, with the active segment mapping refreshed
before value scans, measured on one run. The default Codec configuration has
no read cache; snapshot scans skip cache lookups in that configuration.

| Backend | Median ns per 50K traversal (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `Range` | 9,196,492 (8,738,246–10,568,942) | 4,806,525 | 100,010 |
| Codec `RangeSnapshot` | 8,476,646 (8,323,662–8,712,042) | 4,805,315 | 100,009 |
| Badger iterator | 36,865,529 (29,802,350–42,068,575) | 5,672,591 | 150,717 |
| bbolt cursor | 8,185,000 (8,140,654–8,305,525) | 4,806,980 | 100,016 |

Codec was about 4x faster than Badger, but `Range` was about 12% slower than
bbolt and `RangeSnapshot` about 4% slower in this run. This is the baseline
before the owned full-snapshot key-string cache below. This comparison
isolates callback traversal and decoding; it does not measure concurrent
writer latency.

### Owned full snapshots with lazy key-string cache

After a full owned scan has been requested once, the next full owned scan
builds a sorted cache of stable key strings for later reuse. Five samples of
100 traversals each on the same Apple M2 Pro measured:

| Backend | Median ns per 50K traversal (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `RangeSnapshot` | 7,156,076 (7,139,447–7,172,431) | 3,245,271 | 51,008 |
| bbolt cursor | 7,926,905 (7,901,552–7,933,160) | 4,807,543 | 100,017 |

The Codec median was about 10% faster than bbolt, with about half the
allocations and 33% fewer allocated bytes. The one-traversal cold run did not
build the cache: Codec measured 10,027,791 ns/op (8,448,709–11,952,667),
while bbolt measured 8,443,875 ns/op (8,260,750–8,907,916). The optimization
therefore benefits repeated full owned scans; one-shot scans keep their
existing memory behavior and do not retain cached keys. On repeated scans,
the second traversal builds the cache while it visits results. Inserts and
deletes invalidate a built cache. Once built, it retains roughly one string
header plus the key bytes per entry. A separate five-sample check measured Codec
unique 1K-key batches at 544,932 ns/op (540,278–559,927), 644,304 B/op, and
11,413 allocs/op. Clean reopen remained at 3,900,987 ns/op median,
10,086,919 B/op, and 5,755 allocs/op; it does not build the cache.

### Key-list collection

`KeysSnapshot` returns a sorted, owned slice from one stable view. The bbolt
comparison also materializes a `[]string` while its cursor transaction is
open. Five samples of one cold call and 100 repeated calls measured:

| Backend | Cold median ns/op (range) | Cold B/op / allocs | 100-call median ns/op (range) | B/op / allocs |
|---|---:|---:|---:|---:|
| Codec `KeysSnapshot` | 1,510,875 (1,319,916–1,780,667) | 2,402,880 / 50,002 | 421,669 (395,017–432,862) | 842,846 / 1,001 |
| bbolt cursor + slice | 1,753,625 (1,650,208–1,903,750) | 2,403,408 / 50,011 | 1,076,759 (1,066,386–1,132,142) | 2,403,400 / 50,010 |

Codec was about 14% faster for the cold call and 2.6x faster across repeated
calls. The repeated-call result reuses stable key strings in the index, so it
allocates the returned slice but avoids copying every key again.

`ScanKeys` remains the writer-friendly chunked API. Five longer samples of 500
scans measured Codec at 1,479,181 ns/op (1,436,314–1,879,610), 1,600,450 B/op,
and 50,002 allocs; bbolt measured 894,434 ns/op (869,100–1,301,363),
1,600,580 B/op, and 50,009 allocs. Codec yields its read lock every 256 keys,
while the bbolt cursor retains one read transaction for the whole scan, so
this timing includes a different writer-latency tradeoff. Reusing the bounded
key page buffer reduced Codec's allocation count from 51,766 to 50,002 and
allocated bytes from about 3.42 MB to 1.60 MB per scan.

### No-copy values with string keys, before the full-tree fast path

This variant decodes values with `codec.UnmarshalNoCopy` for every backend and
produces string keys for all three. Ten samples of five traversals each
measured:

| Backend | Median ns per 50K traversal (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `RangeSnapshotNoCopy` | 9,278,638 (8,385,000–9,890,150) | 1,605,740 | 50,007 |
| Badger iterator | 37,351,342 (33,891,475–38,282,575) | 2,472,801 | 100,716 |
| bbolt cursor | 6,960,505 (6,798,592–7,201,825) | 1,605,908 | 50,013 |

Codec beat Badger, but bbolt was about 1.3x faster. This no-copy API still
allocates a string key per record; the borrowed-key variant below removes that
cost.

### Borrowed key and value traversal

`RangeBorrowed50K` lets both the key and decoded value be borrowed only for the
callback. Five samples of 20 traversals each, after snapshot scans started
reusing a typed decoder, measured:

| Backend | Median ns per 50K traversal (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `RangeSnapshotBorrowed` | 6,517,885 (6,370,050–7,447,310) | 5,240 | 8 |
| Codec `TakeRangeBorrowed` (1024-key chunks) | 7,958,404 (7,869,323–8,013,750) | 6,896 | 55 |
| Badger iterator | 38,764,290 (37,707,394–43,275,215) | 872,523 | 50,718 |
| bbolt cursor | 6,579,731 (6,511,638–7,823,308) | 5,683 | 13 |

Codec was near parity with bbolt (about 1% faster by median) and about 5.9x
faster than Badger in this run, with fewer allocations and bytes per operation
than both. The Codec and bbolt sample ranges overlap, so the median difference
does not establish a reliable lead. This API holds a read lock for the full
traversal and the callback must not retain keys or decoded fields.

`TakeRangeBorrowed` releases the read lock between chunks to let writers
progress. Reusing the decoded value across 1024-key chunks reduced the result
to about 6.9 KB and 55 allocations per traversal, down from roughly 993 KB
and 983 allocations before buffer reuse. The median remains about 22% slower
than the full snapshot callback in these separate runs; the latter's lock and
snapshot semantics are cheaper when writer fairness is not needed. The
1024-key chunk size was an experiment; a later comparison found no clear time
gain and higher memory use, so the implementation now uses 256-key chunks.

### Reusable decoder microbenchmark

Ten samples of the same evolvable payload on the same machine measured
`UnmarshalNoCopy` at a 564 ns/op median and `ValueDecoder[Rich].UnmarshalNoCopy`
at 544 ns/op. Both used 72 B/op and 2 allocs/op. Reusing the plan and scratch
state improved this isolated decode by about 3.5%; the full range comparison
did not show a reliable relative win over bbolt.

### Owned 256-entry pages

`BenchmarkPage50K` traverses the same 50K fixture and materializes owned
key/value pages of 256 entries. Five samples of 20 traversals each measured:

| Backend | Median ns per 50K traversal (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `Page` | 33,246,867 (33,185,096–33,675,735) | 262,653,525 | 302,214 |
| Badger iterator pages | 47,780,490 (47,277,892–49,818,594) | 256,496,538 | 350,728 |
| bbolt cursor pages | 51,180,704 (47,883,406–52,707,715) | 255,748,626 | 300,226 |

In this run Codec was about 1.44x faster than Badger and 1.54x faster than
bbolt by median. Codec calls `Page` with a pattern-bound cursor between pages; Badger and
bbolt use one read transaction and reuse a local 256-entry output page. The
differing cursor lifetimes are part of each backend's native API, so this is a
workload comparison rather than a universal ranking.

### Channel streaming

`BenchmarkStream50K` consumes every owned item from Codec's direct `Page`,
`Stream`, and `StreamResults` APIs. Five samples of 20 traversals each
measured:

| API | Median ns per 50K traversal (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| `Page` loop | 34,211,150 (33,515,050–34,299,550) | 262,654,053 | 302,215 |
| `Stream` | 36,133,940 (35,613,038–36,581,833) | 262,572,459 | 300,600 |
| `StreamResults` | 36,007,906 (35,806,654–36,335,740) | 262,575,088 | 300,600 |
| `StreamBorrowed` | 7,827,742 (7,273,858–8,039,633) | 7,017 | 55 |

The owned channel samples overlap substantially; their timing differences do
not establish a reliable ordering. `StreamBorrowed` completed in about 7.8 ms
with 7 KB/op and 55 allocations, compared with roughly 36 ms and 263 MB/op for
owned channel streaming. Its callback must consume borrowed data immediately.
Five samples of five traversals were run for `StreamBorrowed`; the owned
channel figures above are older 20-traversal samples, so compare these as
directional results. Stream pages cap encoded value bytes at the configured
limit (8 MiB by default; one oversized item can exceed it), while preserving a
channel buffer of up to 256 items. While a slow consumer has a queued chunk,
the producer may have the next page prepared; the encoded-value cap applies to
each page separately and decoded object overhead is additional.

### Bounded owned range traversal

`BenchmarkTakeRange50K` decodes and consumes 50,000 values in one bounded key
range. Five samples of 20 traversals each after the bounded chunk-snapshot
change measured:

| Backend | Median ns per 50K traversal (range) | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Codec `TakeRange` | 8,937,812 (8,744,412–8,968,833) | 4,861,642 | 100,273 |
| Badger iterator | 25,286,196 (22,665,521–26,072,854) | 5,672,577 | 150,719 |
| bbolt cursor | 7,874,988 (7,737,919–8,297,604) | 4,807,213 | 100,016 |

Codec was about 2.8x faster than Badger and 1.14x slower than bbolt in this
workload. Refreshing the active segment's read-only mapping before scanning
improved Codec's earlier median from 9.81 ms to 9.18 ms; the current run
measured 8.94 ms after moving value reads into the bounded chunk snapshot.
`TakeRange` copies at most 8 MiB of encoded values per chunk (one larger value
is allowed), releases the read lock, then decodes and invokes callbacks. This
keeps chunk values stable while allowing callback DB calls and writer progress
between chunks. Badger and bbolt keep a read transaction for the full
traversal. The short samples are directional, and the different snapshot
lifetimes make this a workload comparison, not a universal ranking.

### Follow-up: direct segment references

The tree locations now hold a direct pointer to their segment. Profiling had
shown a segment-ID map lookup in every value read; this removes that lookup.
Checkpoint files continue to store segment IDs, resolved to pointers during
load. The following measurements were collected after this change. `Range`
and `RangeSnapshot` are three samples of 100 traversals; `TakeRange` is five
samples of 100 traversals. The latter has no Badger result in this run.

| API | Codec median ns per 50K traversal (range) | bbolt median ns (range) |
|---|---:|---:|
| `Range` | 8,131,887 (8,070,140–8,144,338) | 7,790,790 (7,767,574–7,812,775) |
| `RangeSnapshot` | 6,574,056 (6,453,272–6,585,941) | 7,790,790 (7,767,574–7,812,775) |
| `TakeRange` | 8,214,486 (8,203,975–9,204,426) | 8,025,985 (7,979,346–8,310,428) |

In a separate three-sample borrowed-iteration run, `RangeSnapshotBorrowed`
measured 5,696,930 ns (5,685,068–5,708,014), and `TakeRangeBorrowed` measured
7,102,195 ns (7,058,748–7,110,455). In the paired run, bbolt's borrowed
cursor traversal measured a 5,925,201 ns median. Borrowed snapshot used about
5.2 KB/op and 8 allocations; bounded borrowed traversal used about 6.7 KB/op
and 55 allocations. These short runs show that the direct reference change
brought owned `TakeRange` close to bbolt's time in this workload. They do not
establish a general performance ranking.

## Atomic 1K-record updates

The `BenchmarkBatchPut1K` workload repeatedly overwrites the same 1,000 keys in
one atomic operation. Each backend encodes the same values using the Codec;
Codec uses `Batch.Commit`, Badger uses one update transaction, and bbolt uses
one write transaction. Three samples of 100 batches, without Redis, measured:

| Backend | Median ns per 1K-record batch (range) | Approx. ns per record |
|---|---:|---:|
| Codec | 364,345 (362,485–396,172) | 364 |
| Badger v4.9.6 | 1,067,875 (1,030,974–1,233,374) | 1,068 |
| bbolt v1.5.0 | 535,772 (512,302–546,738) | 536 |

Codec led both embedded engines in this repeated-update batch workload. The
benchmark disables fsync consistently with the other embedded comparisons;
it does not compare power-loss durability, initial bulk loading, or Redis.

## Historical 500-op read baseline (before mmap)

This earlier run used a different benchmark revision and is kept as historical
context. The fixture had 50,000 sorted records, each timed prefix scan returned
and decoded 10 records, and each sample ran 500 operations. Fixture creation
and cleanup were outside the timer. Writes used non-fsync settings; Redis ran
locally over loopback with persistence disabled.

Environment: Go 1.27.4, macOS Darwin 25.5, Apple M2 Pro, Redis 8.10.2.

| Operation | Codec | Badger v4.9.6 | bbolt v1.5.0 | Redis 8.10.2 |
|---|---:|---:|---:|---:|
| Get + decode, median ns/op (sample range) | 2,739 (1,297–2,771) | 2,118 (1,745–2,227) | 1,628 (1,587–1,722) | 27,075 (26,296–27,765) |
| Prefix scan + decode 10 records, median ns/op (sample range) | 8,478 (7,999–8,706) | 15,689 (15,253–15,745) | 6,872 (5,567–7,045) | 4,999,367 (4,965,808–5,011,646) |

On varied-key Get, default Codec trailed Badger and bbolt; on prefix scan, it
beat Badger but trailed bbolt. Redis's prefix query scans the keyspace with
`SCAN MATCH`, so that row includes loopback and command-pagination overhead.

With an 8 MiB cache warmed across all 50K values (6.374 MiB of key/value
payload bytes, plus map/list overhead), three 5,000-operation Get samples
measured these medians:

| Warm full-set Get | Median ns/op (sample range) |
|---|---:|
| Codec, cache off | 1,494 (1,488–1,560) |
| Codec, 8 MiB cache | 1,343 (1,184–1,524) |
| Badger | 1,697 (1,603–1,844) |
| bbolt | 1,455 (1,452–1,461) |

The parallel 50K lookup benchmark used Go's default parallel worker count
(10 on this machine), with all 50K values preloaded and the Codec cache warmed.
Three 5,000-operation samples produced these medians:

| Parallel Get | Median ns/op (sample range) |
|---|---:|
| Codec, cache off | 1,412 (1,400–1,419) |
| Codec, 8 MiB cache | 675 (652–752) |
| Badger | 1,758 (1,725–1,792) |
| bbolt | 2,599 (2,568–2,634) |

The cache helps this resident working set but uses bounded RAM; default Codec
still loses some single-threaded varied-key Get and prefix scans to bbolt. The
suite also measures bulk and unique-key batches, durable commits, expiry
growth, clean reopen, full log replay, and 50K-key traversals. It still lacks
benchmarks for actual process-failure/power-loss recovery and compaction, and
this cache comparison does not sweep cache capacities or working-set sizes.

Reproduce using the commands in `README.md`; collect more repetitions and use
benchstat before making decisions from new runs.

## Hot-key read cache

The repeated single-key Get benchmark includes an optional Codec variant with
a 1 MiB read cache. Three 1,000-operation samples measured a 901 ns/op median
(804–1,163) for cached Codec, compared with 1,188 ns/op (1,043–1,235) for the
default Codec configuration, 1,161 ns/op (1,049–1,326) for Badger, and 1,559
ns/op (1,457–1,845) for bbolt. The cache is off by default and uses bounded
memory.

## Expired 1K-key reads

`BenchmarkExpiredGetMany1K` writes 1,000 values with a one-second TTL, waits
1.25 seconds outside the timer, then checks that every key is logically
expired. Codec's background expiry worker is disabled for this case so the
measurement covers lookup-time expiry checks. The latest completed five-sample
run measured standard Codec `GetMany` at a 150,642 ns/op median (66,081–156,635),
Badger's transaction `Get` loop at 570,913 ns/op (285,912–747,931), and Redis
`MGET` at 529,920 ns/op (333,082–735,198). Codec's median was about 3.8x lower
than Badger's and 3.5x lower than Redis's. Each operation checks the same 1,000
keys; Codec uses a batch API, while Badger loops point lookups within one read
transaction and Redis uses one `MGET` command over loopback. Redis persistence
was disabled. This is a narrow expired-key lookup result, not a general read
ranking. The borrowed Codec variant is implemented in the benchmark but has
also completed five-sample comparisons. Against Badger, borrowed Codec measured
a 150,642 ns/op median (141,833–165,412), while Badger measured 425,565 ns/op
(344,998–652,314). In a separate Redis run, borrowed Codec measured a 105,045
ns/op median (71,495–157,968), while Redis measured 667,550 ns/op
(643,416–690,101). Borrowed Codec used 80 B/op and one allocation; Redis used
48,936 B/op and 1,006 allocations. These are separate runs with visible Codec
timing variance, so the ratios are directional rather than a same-run ranking.
