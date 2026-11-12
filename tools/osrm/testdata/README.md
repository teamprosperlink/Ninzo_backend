# OSRM test data

- `test-zones.osm.pbf`: OpenStreetMap data for the two test zones in `cmd/testdata` (Koramangala and Indiranagar), the box 77.600,12.915 to 77.660,12.995. It was cut from Geofabrik's southern-zone extract, replication timestamp 2026-11-11T21:22:47Z, by `tools/osrm/build.sh testdata`.
- `signals.txt`: the ids of the `highway=traffic_signals` nodes in that extract, for the golden routes in `internal/routing/golden_test.go`.

© OpenStreetMap contributors. The data is available under the Open Database Licence (ODbL), https://www.openstreetmap.org/copyright. Credit it wherever routes or maps made from it are shown.

To refresh, run `tools/osrm/build.sh bengaluru` and then `tools/osrm/build.sh testdata`. Then re-check the golden pairs; never loosen a rule to make them pass.

The repository's own licence does not cover these two files: they stay under the ODbL, which allows sharing them with the credit above.
