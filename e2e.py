#!/usr/bin/env python3
"""End-to-end watchparty suite. Real mpv processes, real transports, no Go tests.

Builds the binaries, generates a test file, starts five nodes on this machine and
drives them by writing to one node's mpv IPC socket, then asserts the result on
the other nodes' mpv. OSD notices are read back out of each node's log, since
every show-text command appears there verbatim.

Six phases, run by default at the same time as copies of this script, each on its
own ports and in its own run directory:

  p2p    the direct HTTP mesh. Nobody is given the full peer list: each node
         bootstraps off one earlier node (A <- B,C; B <- D,E), so most of the
         mesh has to be discovered through /hi peer lists. All five still have
         to end up knowing all four others.
  relay  the same nodes against one ./relay websocket server, which is what
         people behind carrier NAT use when UPNP cannot work. Covers fan-out,
         room isolation, the secret, and reconnect after the relay dies.
         Both of these also check that a joiner playing a different file is
         refused, and that the same file renamed, moved and re-dated is not.
  media  through a relay too: torrents, odd file names, and a folder as a playlist.
  caps   one fresh relay per case: every abuse cap, CREATE_KEY, and dropping a
         client that never answers pings.
  gui    real GUIs, each opening its own page in headless Chromium, driven
         through the page over CDP: every status, both modes, the invite, the
         feed, Browse through a fake portal, Settings, history, persistence,
         self-update against a local release, and the window's lifetime,
         including a real window on Xvfb closing itself.
  upnp   a fake router on the LAN: CLI and GUI behind carrier NAT, and an mpv that
         hangs up. Slow by nature: each search waits out its 6.5s.

    ./e2e.py                    # all six phases in parallel
    ./e2e.py --mode relay       # one phase
    ./e2e.py --file video.mkv   # use a real file instead of a generated one
    ./e2e.py --nodes 3          # fewer nodes, faster
    ./e2e.py --jitter 7         # the phases it can delay, with seed 7's random delays

Needs go, mpv, Chrome or Chromium, dbus-daemon and (unless --file is given) ffmpeg.
All run headless.
Exits non-zero if any assertion fails; the run directory with all node logs is
printed at the end.
"""
import importlib, sys
sys.dont_write_bytecode = True  # no __pycache__ in the repo
from e2e_core import args, finish

finish(getattr(importlib.import_module("e2e_" + args.mode), args.mode + "_suite"))
