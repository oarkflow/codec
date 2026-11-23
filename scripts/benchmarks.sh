#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
BENCHTIME=${BENCHTIME:-500ms}
COMPARE_BENCHTIME=${COMPARE_BENCHTIME:-300ms}
COMPARE_COUNT=${COMPARE_COUNT:-3}
RESULTS_DIR=$(mktemp -d "${TMPDIR:-/tmp}/codec-bench.XXXXXX")
trap 'rm -rf "$RESULTS_DIR"' EXIT HUP INT TERM

cd "$ROOT"
go test -run '^$' -bench 'Benchmark(CodecMarshal|CodecUnmarshal|CodecBatchMarshal|CodecBatchUnmarshal|ColumnScan|MalformedDecode)$' -benchmem -benchtime "$BENCHTIME" .
go test -run '^$' -bench 'Benchmark(Readiness|Put|Get|Update)$' -benchmem -benchtime "$BENCHTIME" ./store

# Compare codec throughput/allocation costs against established wire formats.
(
	cd "$ROOT/benchmarks"
	if go test -run '^$' -bench 'Benchmark(Marshal|Unmarshal)$' -benchmem -benchtime "$COMPARE_BENCHTIME" -count "$COMPARE_COUNT" ./... > "$RESULTS_DIR/codecs.txt" 2>&1; then
		cat "$RESULTS_DIR/codecs.txt"
	else
		cat "$RESULTS_DIR/codecs.txt"
		exit 1
	fi
)
awk -f "$ROOT/scripts/summarize_codec_benchmarks.awk" "$RESULTS_DIR/codecs.txt"

# Compare representative embedded-store operations against Badger and bbolt.
# Redis is optional and is only included by the storecompare suite when configured.
(
	cd "$ROOT/benchmarks/storecompare"
	if go test -run '^$' -bench '^(Benchmark(Put|Get|BatchPut1K|GetMany1K|ScanPrefix50K|RangeBorrowed50K))$' -benchmem -benchtime "$COMPARE_BENCHTIME" -count "$COMPARE_COUNT" > "$RESULTS_DIR/stores.txt" 2>&1; then
		cat "$RESULTS_DIR/stores.txt"
	else
		cat "$RESULTS_DIR/stores.txt"
		exit 1
	fi
)

printf '%s\n' "Comparison summary: $ROOT/benchmarks/COMPARISON.md"
(
	cd "$ROOT/js"
	if [ ! -x node_modules/.bin/tsc ]; then npm ci; fi
	npm run build
	npm run bench
)
