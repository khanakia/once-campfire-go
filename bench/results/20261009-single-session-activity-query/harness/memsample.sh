#!/bin/sh
# Runs inside the bench host, pinned to CPUs 0-1. While a cf-native-bench container exists, records
# the server process's peak RSS (VmHWM) and its cgroup's memory.peak every 0.5 s; one line per
# sample, so the last line per container id is its peak for that round.
label=$1 out=$2
while :; do
  cid=$(docker ps -q --no-trunc -f name=cf-native-bench 2>/dev/null | head -1)
  if [ -n "$cid" ]; then
    pid=$(docker inspect -f '{{.State.Pid}}' "$cid" 2>/dev/null)
    if [ -n "$pid" ] && [ "$pid" != 0 ]; then
      hwm=$(awk '/VmHWM/{print $2}' /proc/$pid/status 2>/dev/null)
      rss=$(awk '/VmRSS/{print $2}' /proc/$pid/status 2>/dev/null)
      cg=$(sed -n 's/^0:://p' /proc/$pid/cgroup 2>/dev/null)
      peak=$(cat /sys/fs/cgroup$cg/memory.peak 2>/dev/null)
      echo "$(date +%s) $label ${cid%${cid#????????????}} hwm_kb=$hwm rss_kb=$rss cgroup_peak=$peak" >> "$out"
    fi
  fi
  sleep 0.5
done
