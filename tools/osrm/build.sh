#!/usr/bin/env bash
# Builds OSRM road data for Ninzo from OpenStreetMap (W1, #183).
#
#   tools/osrm/build.sh bengaluru   download Geofabrik's southern zone, clip to Bengaluru, build
#   tools/osrm/build.sh test        build the committed test-zone extract (seconds; no download)
#   tools/osrm/build.sh testdata    re-cut the test-zone extract and its signal list from the
#                                   Bengaluru clip (run bengaluru first)
#
# Output goes to tools/osrm/data/<name>/ (git-ignored): the .osrm.* files and build.json, which
# records what the data was made from. Needs Docker only. Stop a running OSRM first (task
# geo:osrm:build does): it holds the files open.
#
# Licence: OpenStreetMap data is ODbL. Credit "© OpenStreetMap contributors" wherever routes or
# maps made from it are shown. Only OpenStreetMap data is used for routing (never Google or Mappls).
set -euo pipefail

# Pinned by digest, so a rebuild uses the same OSRM. v6.0.0, amd64 and arm64.
OSRM_IMAGE="ghcr.io/project-osrm/osrm-backend:v6.0.0@sha256:729461bcc9ae9e6aafa92c0f93db9b060a32e85d5e72092c01ae4a4a9f1eb564"
OSMIUM_VERSION="1.15.0-1" # must match osmium.Dockerfile
SOURCE_URL="https://download.geofabrik.de/asia/india/southern-zone-latest.osm.pbf"
# Bengaluru with a margin: lng_min,lat_min,lng_max,lat_max.
BENGALURU_BOX="77.35,12.75,77.90,13.25"
# The two test zones (cmd/testdata) with about 1 km of margin.
TEST_BOX="77.600,12.915,77.660,12.995"
PROFILE="scooter"

here="$(cd "$(dirname "$0")" && pwd)"
# The osmium image is tagged by its Dockerfile's hash, so any change to it builds a new image.
OSMIUM_IMAGE="ninzo-osmium:$OSMIUM_VERSION-$(sha256sum "$here/osmium.Dockerfile" | cut -c1-12)"
# Docker on Windows (Git Bash) needs a Windows path for volumes, and /data left unconverted.
host() { if command -v cygpath >/dev/null; then cygpath -m "$1"; else echo "$1"; fi; }
docker() { MSYS_NO_PATHCONV=1 command docker "$@"; }
# On Linux, run containers as the current user, so the files they write can be deleted next time.
as_me=()
if [ "$(uname -s)" = "Linux" ]; then
  as_me=(--user "$(id -u):$(id -g)")
fi
data="$here/data"
mkdir -p "$data"

osmium() {
  docker image inspect "$OSMIUM_IMAGE" >/dev/null 2>&1 ||
    docker build -q -t "$OSMIUM_IMAGE" -f "$(host "$here/osmium.Dockerfile")" "$(host "$here")" >/dev/null
  docker run --rm "${as_me[@]}" -v "$(host "$data"):/data" -v "$(host "$here/testdata"):/testdata" "$OSMIUM_IMAGE" "$@"
}

# stamp <path inside the osmium container>: the extract's OpenStreetMap replication timestamp.
# Docker on Windows may end the line with a carriage return; drop it.
stamp() {
  local ts
  ts=$(osmium fileinfo -g header.option.osmosis_replication_timestamp "$1" 2>/dev/null | tr -d '\r' || true)
  if [ -z "$ts" ]; then
    echo "warning: $1 has no replication timestamp; build.json will not say how old the data is" >&2
  fi
  echo "$ts"
}

sha() { sha256sum "$1" | cut -d' ' -f1; }

# build <name> <pbf inside data/> <source description> <source sha256> <box>: extract, partition,
# customize (MLD), and write build.json.
build() {
  local name="$1" pbf="$2" source="$3" source_sha="$4" box="$5" out="$data/$1"
  rm -rf "$out" && mkdir -p "$out"
  cp "$data/$pbf" "$out/$name.osm.pbf"
  local started; started=$(date +%s)
  docker run --rm "${as_me[@]}" -v "$(host "$out"):/data" -v "$(host "$here/profiles"):/profiles:ro" "$OSRM_IMAGE" \
    osrm-extract -p "/profiles/$PROFILE.lua" "/data/$name.osm.pbf"
  docker run --rm "${as_me[@]}" -v "$(host "$out"):/data" "$OSRM_IMAGE" osrm-partition "/data/$name.osrm"
  docker run --rm "${as_me[@]}" -v "$(host "$out"):/data" "$OSRM_IMAGE" osrm-customize "/data/$name.osrm"
  local took=$(( $(date +%s) - started ))
  local ts; ts=$(stamp "/data/$pbf")
  local profile_version; profile_version=$(grep -o "PROFILE_VERSION = '[^']*'" "$here/profiles/$PROFILE.lua" | cut -d"'" -f2)
  cat >"$out/build.json" <<JSON
{
  "name": "$name",
  "source": "$source",
  "source_sha256": "$source_sha",
  "clip_box": "$box",
  "osm_extract": "$pbf",
  "osm_extract_sha256": "$(sha "$data/$pbf")",
  "osm_timestamp": "$ts",
  "profile": "$PROFILE",
  "profile_version": "$profile_version",
  "profile_sha256": "$(sha "$here/profiles/$PROFILE.lua")",
  "osrm_image": "$OSRM_IMAGE",
  "osmium": "$OSMIUM_VERSION",
  "algorithm": "mld",
  "built_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "build_seconds": $took
}
JSON
  rm -f "$out/$name.osm.pbf"
  echo "built $name in ${took}s:"
  cat "$out/build.json"
}

# download fetches the southern zone, resuming a stopped download only when Geofabrik still
# publishes the same file, and checks it against Geofabrik's MD5.
download() {
  local pbf="$data/southern-zone.osm.pbf" part="$data/southern-zone.osm.pbf.part"
  # A file already on disk is reused (it was checked when downloaded), so rebuilds work offline.
  if [ -f "$pbf" ] && [ "${REFRESH:-}" != "yes" ]; then
    return
  fi
  local want; want=$(curl -fsSL --retry 3 "$SOURCE_URL.md5" | cut -d' ' -f1)
  [ -n "$want" ] || { echo "could not read $SOURCE_URL.md5" >&2; exit 1; }
  # A part file from an older "latest" would be joined to the new one; start again then.
  if [ -f "$part" ] && [ "$(cat "$part.md5" 2>/dev/null)" != "$want" ]; then
    rm -f "$part"
  fi
  echo "$want" >"$part.md5"
  curl -fL --retry 3 -C - -o "$part" "$SOURCE_URL"
  local got; got=$(md5sum "$part" | cut -d' ' -f1)
  if [ "$got" != "$want" ]; then
    rm -f "$part" "$part.md5"
    echo "download damaged: md5 $got, Geofabrik says $want; run again" >&2
    exit 1
  fi
  mv "$part" "$pbf" && rm -f "$part.md5"
}

case "${1:-}" in
  bengaluru)
    download
    # Clipping drops the replication timestamp, so copy it into the clip's header.
    osmium extract --overwrite -s smart -b "$BENGALURU_BOX" \
      --output-header="osmosis_replication_timestamp=$(stamp /data/southern-zone.osm.pbf)" \
      -o /data/bengaluru-clip.osm.pbf /data/southern-zone.osm.pbf
    build bengaluru bengaluru-clip.osm.pbf "$SOURCE_URL" "$(sha "$data/southern-zone.osm.pbf")" "$BENGALURU_BOX"
    ;;
  testdata)
    [ -f "$data/bengaluru-clip.osm.pbf" ] || { echo "run '$0 bengaluru' first" >&2; exit 1; }
    osmium extract --overwrite -s complete_ways -b "$TEST_BOX" \
      --output-header="osmosis_replication_timestamp=$(stamp /data/bengaluru-clip.osm.pbf)" \
      -o /testdata/test-zones.osm.pbf /data/bengaluru-clip.osm.pbf
    # The signal node ids, for the golden routes (internal/routing/golden_test.go).
    osmium tags-filter --overwrite /testdata/test-zones.osm.pbf n/highway=traffic_signals -o /data/signals.osm.pbf
    osmium cat /data/signals.osm.pbf -f opl | cut -d' ' -f1 | tr -d 'n\r' | sort -n >"$here/testdata/signals.txt"
    ls -l "$here/testdata/test-zones.osm.pbf" "$here/testdata/signals.txt"
    ;;
  test)
    cp "$here/testdata/test-zones.osm.pbf" "$data/test-zones-source.osm.pbf"
    build test-zones test-zones-source.osm.pbf "tools/osrm/testdata/test-zones.osm.pbf" \
      "$(sha "$here/testdata/test-zones.osm.pbf")" "$TEST_BOX"
    ;;
  *)
    echo "usage: $0 bengaluru|test|testdata" >&2
    exit 2
    ;;
esac
