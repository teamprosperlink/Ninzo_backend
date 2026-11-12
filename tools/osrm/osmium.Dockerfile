# osmium-tool for clipping OpenStreetMap extracts (#183). Debian's package at a pinned version, on
# a base pinned by digest. Keep OSMIUM_VERSION in build.sh in step with the package version.
FROM debian:bookworm-20251103-slim@sha256:936abff852736f951dab72d91a1b6337cf04217b2a77a5eaadc7c0f2f1ec1758
RUN apt-get update \
 && apt-get install -y --no-install-recommends osmium-tool=1.15.0-1 ca-certificates \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /data
ENTRYPOINT ["osmium"]
