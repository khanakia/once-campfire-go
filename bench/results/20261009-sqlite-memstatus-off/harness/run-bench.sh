#!/bin/bash
# Start the disposable bench host, load the three images, build the seed, then run the
# unmodified harness in alternating blocks: main,A,A,main and main,B,B,main, for the normal
# profile and the mixed-write profile. Each block is one bin/benchmark call with two rounds.
set -uo pipefail
S=$(cd "$(dirname "$0")" && pwd)
B=/Volumes/D/www/projects/khanakia/sessions/campfire_20261006_182352
H=campfire-bench-host
V=/bench/verification
LOG=$S/blocks.log
x() { docker exec "$H" "$@"; }

if ! docker ps -q -f name=^$H$ | grep -q .; then
  docker run -d --privileged --name $H -e DOCKER_TLS_CERTDIR= \
    -v $B:$B:ro -v $S:/scripts --tmpfs $V/tmp:exec,size=4g campfire-bench-host:local >/dev/null || exit 1
  until x docker info >/dev/null 2>&1; do sleep 1; done
  echo "host up $(date +%T)" | tee -a $LOG
  docker save campfire-go-bench:main campfire-go-bench:a campfire-go-bench:b | docker exec -i $H docker load | tee -a $LOG
fi
if ! x test -f $V/fixtures/default/labels.json; then
  echo "seed $(date +%T)" | tee -a $LOG
  x sh -c "cd $V && bin/seed" > $S/seed.log 2>&1 || { echo "SEED FAILED"; tail -20 $S/seed.log; exit 1; }
  x cat $V/fixtures/default/source.json | tee -a $LOG
fi

block() { # profile label tag tree
  local profile=$1 label=$2 tag=$3 tree=$4 extra= routes=room_show,messages_page,sidebar,search,post_message
  if [ "$profile" = mixed ]; then extra="--mixed-write-rate 10"; routes=room_show,messages_page,sidebar,search; fi
  x sh -c "rm -f /bench/once-campfire-go && ln -s $B/$tree /bench/once-campfire-go"
  local out=$V/tmp/bench/results/$label
  echo "### $label image=$tag tree=$tree mac_load=$(sysctl -n vm.loadavg) vm_load=$(x cat /proc/loadavg) $(date +%T)" | tee -a $LOG
  x taskset -c 0-1 sh /scripts/memsample.sh "$label" /scripts/mem.log &
  local sampler=$!
  docker exec --user 1000:1000 -e HOME=/tmp/u -e TMPDIR=/tmp/u "$H" env GO_IMAGE=campfire-go-bench:$tag sh -c "cd $V && bin/benchmark --apps go --rounds 2 --duration 8 --concurrencies 16 \
    --cpus 2-5 --client-cpus 6-9 --routes $routes $extra --output $out" > $S/$label.log 2>&1
  local status=$?
  x sh -c "pkill -f memsample.sh; true" >/dev/null 2>&1
  kill $sampler 2>/dev/null
  echo "    exit=$status $(grep -c 'req/s' $S/$label.log) samples $(date +%T)" | tee -a $LOG
  [ $status = 0 ] || { tail -15 $S/$label.log; exit 1; }
}

# The app image runs as uid 1000; run the harness as that user too, as on an ordinary Linux host,
# so the databases it copies are writable by the app.
x sh -c "chmod 666 /var/run/docker.sock && mkdir -p /tmp/u $V/tmp/bench && chown -R 1000:1000 /tmp/u $V/tmp $V/fixtures/default && rm -rf $V/tmp/bench/results/01-normal-main"
n=0
for profile in normal mixed; do
  for pair in "a go-pr-a" "b go-pr-b"; do
    set -- $pair
    for step in "main go-pr" "$1 $2" "$1 $2" "main go-pr"; do
      set -- $step; n=$((n+1))
      block $profile "$(printf %02d $n)-$profile-$1" "$1" "$2"
    done
    set -- $pair
  done
done
docker cp $H:$V/tmp/bench/results $S/results-raw
echo "ALL BLOCKS DONE $(date +%T)" | tee -a $LOG
