#!/bin/sh
# The probes' videos, made in this folder (git ignores them).
set -e
cd "$(dirname "$0")"
mk() { src=$1 t=$2 g=$3 out=$4; shift 4; ffmpeg -v error -y -f lavfi -i "$src" -t "$t" -c:v libx264 -g "$g" "$@" -pix_fmt yuv420p "$out"; }
mk testsrc=size=160x120:rate=24 200 240 v24.mkv    # a keyframe every 10s
mk testsrc=size=160x120:rate=120 120 240 v120.mkv  # a frame every 8ms
mk testsrc=size=160x120:rate=24 30 48 p0.mkv       # a playlist of three lengths
mk testsrc=size=160x120:rate=24 40 48 p1.mkv
mk testsrc=size=160x120:rate=24 50 48 p2.mkv
mk testsrc=size=160x120:rate=24 4 24 e.mkv         # short, to play into its end
mk "testsrc=size=320x240:rate=24,noise=alls=60:allf=t" 60 48 net.mkv -b:v 1M  # ~126 KB/s, served slower
