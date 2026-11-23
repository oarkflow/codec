#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

go_version=$(go env GOVERSION)
if ! printf '%s\n' "$go_version" | awk -F '[. ]' '
  { major = substr($1, 3) + 0; minor = $2 + 0; patch = $3 + 0 }
  END { exit !(major > 1 || (major == 1 && (minor > 27 || (minor == 27 && patch >= 3)))) }
'; then
  echo "Codec requires Go 1.27.3 or newer (found $go_version)" >&2
  exit 1
fi

node_major=$(node -p 'Number(process.versions.node.split(".")[0])')
if [ "$node_major" -lt 22 ]; then
  echo "Codec's TypeScript package requires Node.js 22 or newer" >&2
  exit 1
fi

go test ./...
go test -race ./...
go vet ./...

if [ "${RUN_FUZZ:-0}" = 1 ]; then
	fuzz_time=${FUZZTIME:-10s}
	for target in FuzzUnmarshal FuzzUnmarshalNoCopy FuzzColumnar FuzzMixed FuzzText FuzzEvolve; do
		go test -run '^$' -fuzz "^${target}$" -fuzztime "$fuzz_time" .
	done
else
	echo "Skipping fuzzing; set RUN_FUZZ=1 only inside an isolated, resource-limited sandbox."
fi

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${target%/*}
  arch=${target#*/}
  echo "Building $target"
  GOOS=$os GOARCH=$arch go build ./...
done

(
  cd "$ROOT/js"
  npm ci
  npm test
  npm_config_cache="${TMPDIR:-/tmp}/codec-npm-cache" npm pack --dry-run
)
