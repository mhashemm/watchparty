#!/bin/sh
# usage: run.sh <core> <probe.py|probe2.py> <experiment:rounds>...
# Three busy loops on <core>, and mpv pinned to it (LOAD), so mpv's threads lose races as on a
# loaded machine. Run it on 0.40 too: docker run --rm --init --cpuset-cpus=2,3 -v "$PWD":/p -w /p
# watchparty-e2e ./run.sh 2 ... (the e2e image).
core=$1; file=$2; shift 2
pids=""
for i in 1 2 3; do taskset -c "$core" python3 -c 'while 1: pass' & pids="$pids $!"; done
export LOAD=$core
mpv --version | head -1
for e in "$@"; do
  timeout 1800 python3 -I "$file" "${e%%:*}" "${e##*:}" 2>&1
done
kill $pids
