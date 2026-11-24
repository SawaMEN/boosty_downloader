#!/usr/bin/env bash
set -euo pipefail
umask 077
root="$(cd -- "$(dirname -- "$0")/.." && pwd -P)"
out="$(mktemp -d "${TMPDIR:-/tmp}/boosty-profile.XXXXXXXX")"
out="$(cd -- "$out" && pwd -P)"
printf 'Private offline profile directory: %s\n' "$out"
cd "$root"
{
  date -u '+%Y-%m-%dT%H:%M:%SZ'
  go version
  go env GOOS GOARCH
  uname -srm
  git rev-parse HEAD
  git status --porcelain
  printf 'GOMAXPROCS=%s\n' "${GOMAXPROCS:-default}"
} > "$out/environment.txt"
binary="$out/server.test"
go test -c -o "$binary" ./internal/server
# The SQLite fixture reads ../../deploy/sqlite/schema_v1.sql.
cd "$root/internal/server"

run_case() {
  local name="$1" compare="$2" optimized="$3" iterations="$4"
  printf '\n=== %s: unprofiled comparison ===\n' "$name"
  "$binary" -test.run='^$' "-test.bench=$compare" -test.benchmem \
    "-test.benchtime=$iterations" -test.count=3 -test.timeout=10m \
    | tee "$out/$name.bench.txt"
  grep -q '^Benchmark' "$out/$name.bench.txt"
  printf '\n=== %s: optimized CPU/heap profile ===\n' "$name"
  "$binary" -test.run='^$' "-test.bench=$optimized" -test.benchmem \
    -test.benchtime=2s -test.count=1 -test.timeout=10m \
    "-test.cpuprofile=$out/$name.cpu.pprof" \
    "-test.memprofile=$out/$name.mem.pprof" \
    | tee "$out/$name.profile-run.txt"
  grep -q '^Benchmark' "$out/$name.profile-run.txt"
  go tool pprof -top -nodecount=30 "$binary" "$out/$name.cpu.pprof" > "$out/$name.cpu.txt"
  go tool pprof -top -nodecount=30 -alloc_space "$binary" "$out/$name.mem.pprof" > "$out/$name.alloc.txt"
  go tool pprof -top -nodecount=30 -inuse_space "$binary" "$out/$name.mem.pprof" > "$out/$name.heap.txt"
}

run_case copy '^BenchmarkGuardedCopyBuffer$' '^BenchmarkGuardedCopyBuffer$/^pooled$' 1000x
run_case scheduler '^BenchmarkImportSchedulerSelection$' '^BenchmarkImportSchedulerSelection$/^bounded$' 5x
run_case shadow '^BenchmarkSQLiteShadowUnchangedPlan$/^archives-10000$' '^BenchmarkSQLiteShadowUnchangedPlan$/^archives-10000$/^streaming$' 3x
run_case guard '^BenchmarkSQLPrimaryLibraryGuard$/^archives-10000$' '^BenchmarkSQLPrimaryLibraryGuard$/^archives-10000$/^streaming$' 3x

printf '\nReports: %s\n' "$out"
