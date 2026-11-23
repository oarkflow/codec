# Codec production readiness

## Scope and result

This review covers the Go codec, persistent store, and TypeScript package at the
current worktree. It uses synthetic library workloads because no application
traffic profile or deployment environment was supplied. On the available
machine (Go 1.27.4, macOS arm64, Node.js 22.23.5), the implemented checks pass.
This is a release-preparation result, not a certification for an unspecified
deployment.

## Changes made

- The TypeScript decoder now enforces a 256 MiB default per-message size and an
  approximate 256 MiB decoded-allocation budget. Stream decoding shares one
  budget across all frames; callers may lower limits per call.
- Store `Close` syncs the active segment before writing its checkpoint. Atomic
  metadata writes now propagate Unix directory-sync errors. Non-Unix platforms
  retain atomic rename behavior, but directory metadata power-loss durability
  depends on the operating system and filesystem.
- Added focused Go and TypeScript regression coverage, bounded readiness
  benchmarks, repeatable validation scripts, an npm lockfile and package
  metadata, and matching MIT license files.
- Public documentation now uses Codec as the product name. Existing `strata`
  media types and on-disk identifiers remain unchanged for compatibility.

## Validation performed

- `go test ./...`, `go test -race ./...`, and `go vet ./...` passed.
- `go build ./...` passed for Linux, macOS, and Windows on amd64 and arm64.
  Runtime tests were run only on macOS arm64.
- `npm ci --offline`, `npm test`, and `npm pack --dry-run` passed. All eight
  TypeScript tests passed, including Go-to-TypeScript and
  TypeScript-to-Go interoperability. The npm install reported zero known
  vulnerabilities from its available audit data.
- `git diff --check`, shell syntax checks, and license-file equality passed.
- Benchmarks ran on an Apple M2 Pro, macOS arm64, Go 1.27.4, with 500 ms per
  Go benchmark. The Go comparisons below use three baseline runs from the
  repository commit and one final run; timings vary with machine load.

| Workload | Baseline median | Final | Change |
|---|---:|---:|---:|
| Codec encode | 489 ns/op | 506 ns/op | +3.4% |
| Codec decode | 624 ns/op | 620 ns/op | -0.7% |
| 1,000-row encode | 120,071 ns/op | 120,101 ns/op | +0.0% |
| 1,000-row decode | 72,565 ns/op | 71,960 ns/op | -0.8% |
| Column scan, per element | 502,552 ns/op | 503,147 ns/op | +0.1% |
| Column scan, column lane | 25,859 ns/op | 25,671 ns/op | -0.7% |
| Store Put | 2,185 ns/op | 2,278 ns/op | +4.3% |
| Store Get | 554 ns/op | 566 ns/op | +2.2% |
| Store Update | 2,629 ns/op | 2,683 ns/op | +2.1% |

All comparable Go workloads stayed within the 10% investigation threshold.
Five TypeScript smoke runs reported 284–315K scalar encodes/s, 371–384K scalar
decodes/s, 396–422 1K-row encodes/s, 1,190–1,433 1K-row decodes/s, and
311–384K malformed-input rejections/s. Heap deltas are noisy and are emitted
only as diagnostic estimates.

`benchmarks/COMPARISON.md` records direct comparisons against Protobuf, CBOR,
two MessagePack libraries, JSON, Badger, and bbolt. On the captured synthetic
workloads, Codec led all six wire-format cases, single-key writes, atomic 1K
batches, prefix scans, and a 50K borrowed range. Badger led uncached point reads
and owned 1K `GetMany`. These are local medians, not performance guarantees; the
comparison script reruns the same bounded workloads and prints a per-workload
median ranking for the wire-format results. The captured store table reports
both wins and losses against Badger and bbolt.

## Remaining validation limits

- Fuzz targets are included in `scripts/validate.sh` but are opt-in and were
  not run in this review: the available execution environment does not provide
  the isolated, resource-limited process sandbox required for target-controlled
  fuzzing. Run `RUN_FUZZ=1 ./scripts/validate.sh` inside an appropriately
  isolated environment before release.
- The exact minimum versions Go 1.27.3 and Node.js 22.0.0 were not available;
  validation used Go 1.27.4 and Node.js 22.23.5.
- Linux and Windows were cross-built, not runtime-tested. Power-loss behavior
  still needs validation on the actual filesystem and storage device.
- Multi-process access to one store directory remains unsupported. The store
  keeps all keys in RAM; choose `SyncAlways`, `SyncInterval`, `Sync`, and
  `Close` based on the application's durability needs.
- No live publishing or deployment was performed. The TypeScript package is
  configured for a public npm release, but publishing and a Go version tag
  remain separate release operations.
- The final security-review fix charges schema-generated default arrays and
  nested default objects against the TypeScript allocation budget; a regression
  test is included. It was added after the last TypeScript test run, so this
  final patch needs a rerun in an environment that can safely execute the
  package checks. The current workspace runner does not expose the OS-enforced
  source isolation and resource caps required by the security review workflow.
