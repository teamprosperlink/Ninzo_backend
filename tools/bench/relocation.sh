#!/usr/bin/env bash
# Relocation benchmark (issue 255): every relocation rule, from both starts, under both bounds on a
# move's cost, on the committed OSRM test-zone data. Writes the environment and the raw output of
# every run to OUT_DIR, from a clean checkout only, so the commit recorded is the code that ran.
#
#   tools/bench/relocation.sh <out_dir> [osrm_url] [only]   (OSRM: task geo:osrm:up, port 5001)
#
# only, if given, is a regular expression over the setup ids (K-even-20, K-hot-20, I-even-20,
# I-hot-20, K-hot-10): to finish a run that was stopped, without redoing the setups already written.
# A partial file of a stopped run is overwritten.
set -euo pipefail
out="${1:?usage: tools/bench/relocation.sh <out_dir> [osrm_url]}"
osrm="${2:-http://127.0.0.1:5001}"
only="${3:-.}"
root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"
if [ -n "$(git status --porcelain -- cmd internal go.mod go.sum)" ]; then
  echo "relocation.sh: commit the code first: the record must name the commit that ran" >&2
  exit 1
fi
mkdir -p "$out"
bin="$(mktemp -d)/sim"
go build -o "$bin" ./cmd/sim

{
  echo "---"
  echo "benchmark: relocation rules (issue 255)"
  echo "only: $only"
  echo "started_utc: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit: $(git rev-parse HEAD)"
  echo "go: $(go version)"
  echo "os: $(uname -srm)"
  echo "cpu: $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //') ($(nproc) threads)"
  echo "memory_kb: $(grep -m1 MemTotal /proc/meminfo 2>/dev/null | awk '{print $2}')"
  echo "osrm_url: $osrm"
  echo "osrm_data_build: $(tr -d '\n' < tools/osrm/data/test-zones/build.json)"
  echo "fleet: 6 riders; 3 h; 20 seeds (1-20); plan demand seed 5000; check 30 s; window 1800 s; min orders 2; rest 900 s"
} >> "$out/environment.txt" # appended: a resumed run adds its own block

kor_hot="12.941,77.636,0.5,250;12.928,77.615,0.2,200"
ind_hot="12.982,77.646,0.6,250"
# id | zone | hotspots | orders per hour
configs=(
  "K-even-20|koramangala||20"
  "K-hot-20|koramangala|$kor_hot|20"
  "I-even-20|indiranagar||20"
  "I-hot-20|indiranagar|$ind_hot|20"
  "K-hot-10|koramangala|$kor_hot|10"
)
for c in "${configs[@]}"; do
  IFS='|' read -r id zone hot rate <<< "$c"
  [[ "$id" =~ $only ]] || continue
  for start in fixed plan; do
    for movers in pessimistic optimistic; do
      args=(-osrm "$osrm" -zone "$zone" -fleet 6 -relocate -start "$start" -orders-per-hour "$rate" -hours 3 -seeds 20)
      [ -n "$hot" ] && args+=(-hotspots "$hot")
      [ "$movers" = optimistic ] && args+=(-move-takes-orders)
      f="$out/$id-start-$start-$movers.txt"
      echo "sim ${args[*]}" > "$f"
      "$bin" "${args[@]}" >> "$f"
      echo "wrote $f"
    done
  done
done
echo "finished_utc: $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$out/environment.txt"
