#!/bin/bash
# Production images for main, A and B, linux/arm64, all on base 9b0ef06; main first so A/B reuse its media stages.
set -euo pipefail
B=/Volumes/D/www/projects/khanakia/sessions/campfire_20261006_182352
OUT=$(dirname "$0")
REV=$(git -C $B/go-pr rev-parse HEAD)
build() { # tag tree
  echo "### build $1 from $2 $(date +%T)"
  docker build --platform linux/arm64 --build-arg APP_VERSION=bench-$1 --build-arg GIT_REVISION=$REV \
    --build-arg OCI_SOURCE=https://github.com/basecamp/once-campfire-go -t campfire-go-bench:$1 $B/$2 > $OUT/build-$1.log 2>&1
  echo "### done $1 $(date +%T) $(docker image inspect -f '{{.Id}}' campfire-go-bench:$1)"
}
build main go-pr
build a go-pr-a &
build b go-pr-b &
wait
for t in main a b; do echo "$t $(docker image inspect -f '{{.Id}} {{.Architecture}}' campfire-go-bench:$t)"; done
echo "ALL BUILT $(date +%T)"
