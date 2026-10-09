# Disposable Linux host for once-campfire-verification on Docker Desktop: its own dockerd,
# so the unmodified harness gets taskset, /proc/loadavg, host networking and tmpfs databases.
FROM rust:1-alpine AS loadgen
RUN apk add --no-cache musl-dev
COPY verification/loadgen /loadgen
RUN cargo build --release --locked --manifest-path /loadgen/Cargo.toml

FROM docker:29-dind
RUN apk add --no-cache ruby sqlite util-linux-misc git ffmpeg bash coreutils procps \
 && git config --system safe.directory '*'
COPY verification /bench/verification
COPY --from=loadgen /loadgen/target/release/loadgen /bench/verification/loadgen/target/release/loadgen
