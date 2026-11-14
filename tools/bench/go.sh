#!/usr/bin/env bash
# Go performance benchmarks (issue 259): every Benchmark in the packages below, 10 runs each with
# memory stats, summarised by benchstat (median and 95% confidence interval). From a clean
# checkout only, so the commit recorded is the code that ran. Run on an otherwise idle machine:
# another heavy process on the same CPU makes the numbers wrong.
#
#   tools/bench/go.sh <out_dir> [count]
set -euo pipefail
out="${1:?usage: tools/bench/go.sh <out_dir> [count]}"
count="${2:-10}"
benchstat_version="v0.0.0-20261009192801-be2c69fb417e" # golang.org/x/perf, pinned
pkgs=(./internal/placement/ ./internal/demand/ ./internal/hexgrid/ ./internal/orders/ ./internal/fcm/)
root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"
if [ -n "$(git status --porcelain -- cmd internal go.mod go.sum)" ]; then
  echo "go.sh: commit the code first: the record must name the commit that ran" >&2
  exit 1
fi
mkdir -p "$out"
{
  echo "benchmark: Go performance (issue 259)"
  echo "started_utc: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit: $(git rev-parse HEAD)"
  echo "go: $(go version)"
  echo "os: $(uname -srm)"
  echo "cpu: $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //') ($(nproc) threads)"
  echo "memory_kb: $(grep -m1 MemTotal /proc/meminfo 2>/dev/null | awk '{print $2}')"
  echo "command: go test -run '^\$' -bench . -benchmem -count $count ${pkgs[*]}"
  echo "summary: benchstat (golang.org/x/perf $benchstat_version)"
} > "$out/environment.txt"
go test -run '^$' -bench . -benchmem -count "$count" "${pkgs[@]}" | tee "$out/raw.txt"
go run "golang.org/x/perf/cmd/benchstat@$benchstat_version" "$out/raw.txt" > "$out/benchstat.txt"
echo "finished_utc: $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$out/environment.txt"
cat "$out/benchstat.txt"
