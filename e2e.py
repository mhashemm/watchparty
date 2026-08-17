#!/usr/bin/env python3
"""End-to-end watchparty suite. Real mpv processes, real transports, no Go tests.

Builds the binaries, generates a test file, starts five nodes on this machine and
drives them by writing to one node's mpv IPC socket, then asserts the result on
the other nodes' mpv. OSD notices are read back out of each node's log, since
every show-text command appears there verbatim.

Two phases, both run by default:

  p2p    the direct HTTP mesh. Nobody is given the full peer list: each node
         bootstraps off one earlier node (A <- B,C; B <- D,E), so most of the
         mesh has to be discovered through /hi peer lists. All five still have
         to end up knowing all four others.
  relay  the same nodes against one ./relay websocket server, which is what
         people behind carrier NAT use when UPNP cannot work. Covers fan-out,
         room isolation, the secret, every abuse cap, and reconnect after the
         relay dies.

    ./e2e.py                    # both phases, ~12 minutes
    ./e2e.py --mode relay       # one phase
    ./e2e.py --file movie.mkv   # use a real file instead of a generated one
    ./e2e.py --nodes 3          # fewer nodes, faster

Needs go, mpv and (unless --file is given) ffmpeg. mpv runs headless.
Exits non-zero if any assertion fails; the run directory with all node logs is
printed at the end.
"""
import argparse, glob, http.server, json, os, shutil, signal, socket, subprocess
import sys, tempfile, threading, time, urllib.error, urllib.request

MPVFLAGS = "--vo=null --ao=null --no-resume-playback --msg-level=all=error"
COOLDOWN = 3
BOOT = COOLDOWN + 4  # cooldown, plus mpv creating its socket, plus the handshake
STAGGER = 1.6        # nodes name their socket /tmp/mpv<unixtime>, so two started
                     # within the same second would collide on one socket path
SETTLE = 2.5
STUB_HOST = "slowpeer"


# ---------------------------------------------------------------- stub peer --
# Re-invokes this script as `--stub` so the whole suite stays one file. It stands
# in for a peer that can go away and come back at the same address without
# re-registering, which is the only way to isolate the strike-reset path.

def run_stub(addr, peer, register):
    port = int(addr.rsplit(":", 1)[1])

    class H(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            self.rfile.read(int(self.headers.get("content-length") or 0))
            if self.path == "/hi":
                body = b"{}"
                self.send_response(200)
                self.send_header("counter", "0")
                self.send_header("hostname", STUB_HOST)
                self.send_header("content-length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            self.send_response(204)
            self.end_headers()

        def log_message(self, *a):
            pass

    srv = http.server.ThreadingHTTPServer(("0.0.0.0", port), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    if register:
        req = urllib.request.Request("http://" + peer + "/hi", data=b"", method="POST",
                                     headers={"hit-me-up": addr, "counter": "0", "hostname": STUB_HOST})
        urllib.request.urlopen(req, timeout=10).read()
    print("ready", flush=True)
    try:
        while True:
            time.sleep(3600)
    except KeyboardInterrupt:
        pass


if len(sys.argv) > 1 and sys.argv[1] == "--stub":
    run_stub(sys.argv[2], sys.argv[3], "noregister" not in sys.argv)
    sys.exit(0)


# ------------------------------------------------------------------- runner --
ap = argparse.ArgumentParser()
ap.add_argument("--file", help="media file to play (default: generate one with ffmpeg)")
ap.add_argument("--port-base", type=int, default=6969)
ap.add_argument("--nodes", type=int, default=5, choices=range(3, 9))
ap.add_argument("--mode", choices=("p2p", "relay", "both"), default="both")
args = ap.parse_args()

NODES = ["node" + chr(ord("A") + i) for i in range(args.nodes)]
# Nobody gets the full list: A is the root, then each node bootstraps off an
# earlier one, so most of the mesh has to be discovered through /hi peer lists.
BOOTSTRAP = {n: (None if i == 0 else NODES[(i - 1) // 2]) for i, n in enumerate(NODES)}
PORTS = {n: args.port_base + i for i, n in enumerate(NODES)}
PORTS["stub"] = args.port_base + len(NODES)
PORTS["relay"] = args.port_base + len(NODES) + 1
RELAY_URL = f"ws://127.0.0.1:{PORTS['relay']}"
ROOM, SECRET = "e2eroom", "e2esecret"
A, LAST = NODES[0], NODES[-1]

REPO = os.path.dirname(os.path.abspath(__file__))
RUN = tempfile.mkdtemp(prefix="watchparty-e2e-")

fails = []
nodes = {}
relays = []


def ok(name, cond, detail=""):
    if not cond:
        fails.append(name)
    print(("  PASS  " if cond else "  FAIL  ") + name + (f"  [{detail}]" if detail else ""))


def need(tool):
    if not shutil.which(tool):
        sys.exit(f"{tool} is required but not on PATH")


def ipc(sock, *cmd):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(5)
    try:
        s.connect(sock)
        s.sendall((json.dumps({"command": list(cmd), "request_id": 99}) + "\n").encode())
        buf = b""
        while True:
            chunk = s.recv(65536)
            if not chunk:
                return None
            buf += chunk
            for line in buf.split(b"\n"):
                if not line.strip():
                    continue
                try:
                    m = json.loads(line)
                except ValueError:
                    continue
                if m.get("request_id") == 99:
                    return m
    except (OSError, socket.timeout):
        return None
    finally:
        s.close()


def get(n, prop):
    r = ipc(nodes[n]["sock"], "get_property", prop)
    return r.get("data") if r else None


def setp(n, prop, val):
    ipc(nodes[n]["sock"], "set_property", prop, val)


def toggle(n=None, settle=0.9):
    """Flip pause to the opposite of its current value; mpv emits no event for a no-op set."""
    n = n or A
    setp(n, "pause", not get(n, "pause"))
    time.sleep(settle)


def log(n):
    return open(nodes[n]["log"]).read()


def osd(n):
    """The OSD notices this node actually displayed."""
    out = []
    for line in log(n).splitlines():
        if 'do: sent request: {"command":["show-text"' in line:
            out.append(json.loads(line.split("do: sent request: ")[1])["command"][1])
    return out


def pauses(n):
    """How many 'paused by X' notices this node displayed. osd() returns a list, so
    count() is an exact match, never a substring search."""
    return sum(1 for s in osd(n) if s.startswith("paused by "))


def known(n):
    """Peers this node has learned, inbound (/hi) or outbound (AddAddress)."""
    peers = set()
    for m in osd(n):
        if m.endswith(" has joined"):
            peers.add(m[:-len(" has joined")])
        elif m.startswith("connected to "):
            peers.add(m[len("connected to "):])
    return peers


def others(n):
    return [x for x in NODES if x != n]


def live():
    return [n for n in NODES if n in nodes and nodes[n]["proc"].poll() is None]


def strikes(watcher, addr, host):
    return log(watcher).count(f"{addr} ({host}): Post")


def dropped(watcher, addr):
    return any(s.startswith("dropped " + addr) for s in osd(watcher))


def child_mpv_socket(pid):
    """The socket of the mpv this node spawned. Beats globbing /tmp for it."""
    for d in glob.glob("/proc/[0-9]*"):
        try:
            with open(d + "/status") as f:
                if f"PPid:\t{pid}\n" not in f.read():
                    continue
            with open(d + "/cmdline", "rb") as f:
                for part in f.read().split(b"\0"):
                    if part.startswith(b"--input-ipc-server="):
                        return part.split(b"=", 1)[1].decode()
        except OSError:
            pass
    return None


def kill_mpv_for(sock):
    """Kill the mpv that owns this socket. SIGKILLing a node orphans its child."""
    for d in glob.glob("/proc/[0-9]*"):
        try:
            with open(d + "/cmdline", "rb") as f:
                if sock.encode() in f.read():
                    os.kill(int(os.path.basename(d)), signal.SIGKILL)
        except OSError:
            pass


def launch(name, addrs=None, room=None, secret=None, tag=None):
    """Start a node. room!=None selects relay mode; addrs is the p2p bootstrap."""
    key = tag or name
    path = f"{RUN}/{key}.log"
    cmd = [BIN, "-file", MEDIA, "-cooldown", str(COOLDOWN), "-hostname", name, "-mpvFlags", MPVFLAGS]
    if room is not None:
        cmd += ["-relay", RELAY_URL, "-room", room, "-secret", secret or SECRET]
    else:
        cmd += ["-local", "-port", str(PORTS[name])]
        if addrs:
            cmd += ["-addrs", addrs]
    p = subprocess.Popen(cmd, stdout=open(path, "wb"), stderr=subprocess.STDOUT)
    nodes[key] = {"proc": p, "log": path, "sock": None, "addr": None}
    return key


def launch_relay(extra=None):
    path = f"{RUN}/relay.log"
    cmd = [RELAY_BIN, "-addr", f"127.0.0.1:{PORTS['relay']}"] + (extra or [])
    # append: a restart in [R12] must not lose the first run's lines
    p = subprocess.Popen(cmd, stdout=open(path, "ab"), stderr=subprocess.STDOUT)
    relays.append(p)
    return p


def relay_log():
    return open(f"{RUN}/relay.log").read()


def resolve(name):
    n = nodes[name]
    n["sock"] = child_mpv_socket(n["proc"].pid)
    if not n["sock"]:
        sys.exit(f"{name} never spawned mpv; see {n['log']}")
    n["addr"] = open(n["log"]).readline().split("share is ")[1].strip()
    print(f"  {name} on {n['addr']}")


def stop(name, sig=signal.SIGTERM, timeout=20):
    n = nodes[name]
    if n["proc"].poll() is not None:
        return True
    n["proc"].send_signal(sig)
    try:
        n["proc"].wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        n["proc"].kill()
        n["proc"].wait()
        if n["sock"]:
            kill_mpv_for(n["sock"])
        return False
    if sig == signal.SIGKILL and n["sock"]:
        kill_mpv_for(n["sock"])  # SIGKILL skips the deferred cmd.Cancel
    return True


def raw(addr, endpoint, headers, body=b""):
    req = urllib.request.Request("http://" + addr + endpoint, data=body, method="POST", headers=headers)
    try:
        return urllib.request.urlopen(req, timeout=10).status
    except urllib.error.HTTPError as e:
        return e.code


def stub(peer, register=True):
    cmd = [sys.executable, __file__, "--stub", STUB_ADDR, peer]
    if not register:
        cmd.append("noregister")
    p = subprocess.Popen(cmd, stdout=subprocess.PIPE, text=True)
    p.stdout.readline()  # blocks until the stub is listening and registered
    return p


def stub_stop(p):
    p.send_signal(signal.SIGINT)
    p.wait(timeout=10)
    p.stdout.close()


need("go")
need("mpv")
print(f"run directory: {RUN}")
print("building watchparty and relay")
subprocess.run(["go", "build", "-o", RUN + "/watchparty", "."], cwd=REPO, check=True)
subprocess.run(["go", "build", "-o", RUN + "/relay", "./relay"], cwd=REPO, check=True)
BIN = RUN + "/watchparty"
RELAY_BIN = RUN + "/relay"

if args.file:
    MEDIA = os.path.abspath(args.file)
else:
    need("ffmpeg")
    print("generating a 120s test file")
    MEDIA = RUN + "/test.mkv"
    subprocess.run(["ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10",
                    "-f", "lavfi", "-i", "sine=frequency=440", "-t", "120", "-c:v", "libx264",
                    "-pix_fmt", "yuv420p", "-c:a", "aac", MEDIA], check=True)

def p2p_suite():
    global STUB_ADDR
    chain = ", ".join(f"{n}->{BOOTSTRAP[n]}" for n in NODES[1:])
    print(f"\n=== setup: {len(NODES)} nodes, bootstrap chain {chain} ===")
    launch(A, None)
    time.sleep(BOOT)
    resolve(A)
    IP = nodes[A]["addr"].rsplit(":", 1)[0]
    STUB_ADDR = f"{IP}:{PORTS['stub']}"
    addr_of = {n: f"{IP}:{PORTS[n]}" for n in NODES}

    for n in NODES[1:]:
        launch(n, addr_of[BOOTSTRAP[n]])  # its bootstrap target is already listening
        time.sleep(STAGGER)
    time.sleep(BOOT)
    for n in NODES[1:]:
        resolve(n)
    for n in NODES:
        ok(f"{n} advertised the expected address", nodes[n]["addr"] == addr_of[n], nodes[n]["addr"])
    time.sleep(SETTLE)

    print(f"\n[1] mesh closes transitively: every node learns all {len(NODES) - 1} others")
    for n in NODES:
        missing = set(others(n)) - known(n)
        ok(f"{n} knows the whole mesh", not missing, f"missing {sorted(missing)}" if missing else "")
    ok("nobody connected to itself", not any(n in known(n) for n in NODES))

    print(f"\n[2] all {len(NODES)} start paused")
    for n in NODES:
        ok(f"{n} paused", get(n, "pause") is True)

    print(f"\n[3] resume on {A} propagates to all {len(NODES) - 1} others")
    setp(A, "pause", False)
    time.sleep(SETTLE)
    for n in NODES:
        ok(f"{n} playing", get(n, "pause") is False)
    for n in others(A):
        ok(f"{n} credited {A}", f"resumed by {A}" in osd(n))

    print(f"\n[4] pause on {LAST} propagates back to everyone (no central host)")
    time.sleep(1.5)
    setp(LAST, "pause", True)
    time.sleep(SETTLE)
    for n in NODES:
        ok(f"{n} paused", get(n, "pause") is True)
    ok(f"{A} credited {LAST}", f"paused by {LAST}" in osd(A))

    print(f"\n[5] seek while paused syncs everyone (from {LAST}, which drove the pause)")
    setp(LAST, "time-pos", 77.0)
    time.sleep(SETTLE)
    for n in others(LAST):
        v = get(n, "time-pos")
        ok(f"{n} followed to 77s", v is not None and abs(v - 77.0) < 1.5, f"time-pos={v}")
    ok(f"{A} credited {LAST} for sync", f"synced by {LAST}" in osd(A))

    print("\n[5b] any paused peer can seek for everyone, not just whoever drove the pause")
    b = NODES[1]  # applied LAST's pause in [4]; under the old slave hack this was muted
    setp(b, "time-pos", 12.0)
    time.sleep(SETTLE)
    for n in others(b):
        v = get(n, "time-pos")
        ok(f"{n} followed {b}'s seek", v is not None and abs(v - 12.0) < 1.5, f"time-pos={v}")

    print("\n[5c] an applied event is not echoed back (the a->b->a rebound)")
    # start from a known state: everyone playing, so the pause below is a real change
    setp(A, "pause", False)
    time.sleep(SETTLE)
    for n in NODES:
        ok(f"{n} playing before the echo check", get(n, "pause") is False)
    before = {n: pauses(n) for n in NODES}

    setp(A, "pause", True)
    time.sleep(SETTLE)
    for n in NODES:
        ok(f"{n} paused", get(n, "pause") is True)
    # if any peer rebroadcast the echo, the originator would be told it was paused
    # by someone else and the rest would see the notice twice
    ok(f"{A} was not told its own pause came back",
       pauses(A) == before[A],
       f"{pauses(A) - before[A]} echoes returned to the originator")
    for n in others(A):
        got = pauses(n) - before[n]
        ok(f"{n} applied {A}'s pause exactly once", got == 1, f"{got} pause notices")

    print("\n[6] time-pos is NOT forwarded while playing (sync semantics stay narrow)")
    setp(A, "pause", False)
    time.sleep(SETTLE)
    counts = {n: osd(n).count(f"synced by {A}") for n in others(A)}
    time.sleep(4)  # A is playing, mpv emits time-pos ~10x/sec
    for n in others(A):
        after = osd(n).count(f"synced by {A}")
        ok(f"no time-pos storm reached {n}", after == counts[n], f"{after - counts[n]} sync notices in 4s")

    print("\n[7] a peer that only ever followed can still drive the party")
    b = NODES[1]
    setp(b, "pause", True)
    time.sleep(SETTLE)
    ok(f"{b}'s pause reached {A}", get(A, "pause") is True)
    setp(b, "pause", False)
    time.sleep(SETTLE)
    for n in others(b):
        ok(f"{b} (ex-slave) resume reached {n}", get(n, "pause") is False)

    print("\n[8] repeated round trips are not swallowed by the counter check")
    rounds = 0
    for i in range(5):
        setp(A, "pause", True)
        time.sleep(1.0)
        mid = {n: get(n, "pause") for n in others(A)}
        setp(A, "pause", False)
        time.sleep(1.0)
        end = {n: get(n, "pause") for n in others(A)}
        if all(mid.values()) and not any(end.values()):
            rounds += 1
    ok(f"5/5 round trips landed on all {len(NODES) - 1} peers", rounds == 5, f"{rounds}/5")
    for n in others(A):
        ok(f"{n} skipped no events", "skipped event from" not in log(n))

    print("\n[9] /event from an unknown address is rejected")
    code = raw(nodes[A]["addr"], "/event", {"hit-me-up": f"{IP}:9999", "counter": "5", "hostname": "stranger"},
               b'{"event":"property-change","name":"pause","data":"yes"}')
    ok(f"{A} rejects unknown peer's event", code == 400, f"HTTP {code}")

    print("\n[10] /bye from an unknown address is rejected")
    code = raw(nodes[A]["addr"], "/bye", {"hit-me-up": f"{IP}:9999", "counter": "5", "hostname": "stranger"})
    ok(f"{A} rejects unknown peer's bye", code == 400, f"HTTP {code}")

    print("\n[11] /bye with a stale counter cannot evict a live peer")
    code = raw(nodes[A]["addr"], "/bye", {"hit-me-up": nodes[b]["addr"], "counter": "1", "hostname": b})
    time.sleep(1)
    ok(f"{A} skipped the stale bye", code == 204, f"HTTP {code}")
    ok(f"{A} logged it as skipped", "skipped bye from" in log(A))
    setp(LAST, "pause", True)
    time.sleep(SETTLE)
    ok(f"{b} is still in the mesh", get(b, "pause") is True, "still receives events after the forged bye")
    setp(LAST, "pause", False)
    time.sleep(SETTLE)

    print(f"\n[12] SIGKILL {LAST}: every survivor evicts it after 3 strikes and stays synced")
    stop(LAST, signal.SIGKILL)
    time.sleep(1)
    dead = addr_of[LAST]
    survivors = others(LAST)
    t0 = time.time()
    for n in survivors:
        # Only the node that generates an event broadcasts, so each survivor has
        # to drive a few of its own before it can notice LAST is gone.
        tries = 0
        while strikes(n, dead, LAST) < 3 and tries < 12:
            toggle(n)
            tries += 1
    elapsed = time.time() - t0
    time.sleep(SETTLE)
    for n in survivors:
        ok(f"{n} evicted {LAST}", dropped(n, dead), f"{strikes(n, dead, LAST)} strikes")
    ok("no survivor stalled on the dead peer", elapsed < 90, f"{elapsed:.1f}s for {len(survivors)} nodes")
    setp(A, "pause", True)
    time.sleep(SETTLE)
    for n in survivors[1:]:
        ok(f"{A} still drives {n} after the eviction", get(n, "pause") is True)
    setp(A, "pause", False)
    time.sleep(SETTLE)

    print("\n[13] one success resets the strike count (a flaky peer must not be evicted)")
    s1 = stub(nodes[A]["addr"])
    time.sleep(1)
    ok("stub peer joined the mesh", f"{STUB_HOST} has joined" in osd(A))

    stub_stop(s1)
    base = strikes(A, STUB_ADDR, STUB_HOST)
    while strikes(A, STUB_ADDR, STUB_HOST) < base + 2:
        toggle()
    ok("2 strikes: still in the mesh", not dropped(A, STUB_ADDR),
       f"{strikes(A, STUB_ADDR, STUB_HOST) - base} failures logged")

    # Comes back at the same address WITHOUT re-registering, so A keeps the same
    # peer entry (fails=2) rather than replacing it via /hi. Only a success can
    # clear the strikes now.
    s2 = stub(nodes[A]["addr"], register=False)
    toggle()
    ok("no new strike while the peer is healthy", strikes(A, STUB_ADDR, STUB_HOST) == base + 2,
       "the broadcast succeeded")

    stub_stop(s2)
    base2 = strikes(A, STUB_ADDR, STUB_HOST)
    while strikes(A, STUB_ADDR, STUB_HOST) < base2 + 2:
        toggle()
    ok("2 more strikes after the success: STILL in the mesh", not dropped(A, STUB_ADDR),
       f"{strikes(A, STUB_ADDR, STUB_HOST)} total failures vs maxFails=3, only a reset explains this")

    while strikes(A, STUB_ADDR, STUB_HOST) < base2 + 3:
        toggle()
    time.sleep(SETTLE)
    ok("3rd consecutive strike evicts", dropped(A, STUB_ADDR))

    print(f"\n[14] clean shutdown: SIGTERM on {A} broadcasts /bye to every survivor")
    clean = stop(A)
    ok(f"{A} exited cleanly", clean and nodes[A]["proc"].returncode == 0, f"rc={nodes[A]['proc'].returncode}")
    time.sleep(SETTLE)
    for n in survivors[1:]:
        ok(f"{n} was told {A} left", f"{A} has left" in osd(n),
           "Shutdown must not derive its timeout from the cancelled signal context")
        ok(f"{n} did not error on {A}'s departure", "does not exists" not in log(n))

    print("\n[15] the remaining mesh keeps working without the node that bootstrapped it")
    rest = survivors[1:]
    setp(rest[0], "pause", True)
    time.sleep(SETTLE)
    for n in rest:
        ok(f"{n} still in sync after {A} left", get(n, "pause") is True)
    for n in rest:
        ok(f"{n} exited cleanly", stop(n) and nodes[n]["proc"].returncode == 0)

def relay_suite():
    print(f"\n=== relay setup: {len(NODES)} nodes through one websocket server ===")
    relay = launch_relay()
    time.sleep(1.5)

    for i, n in enumerate(NODES):
        launch(n, room=ROOM)
        time.sleep(STAGGER if i < len(NODES) - 1 else 0)
    time.sleep(BOOT)
    for n in NODES:
        resolve(n)
    time.sleep(SETTLE)

    print(f"\n[R1] every node learns all {len(NODES) - 1} others through the relay")
    for n in NODES:
        missing = set(others(n)) - known(n)
        ok(f"{n} knows the room", not missing, f"missing {sorted(missing)}" if missing else "")
    ok("nobody was told it joined itself", not any(n in known(n) for n in NODES))

    print(f"\n[R2] all {len(NODES)} start paused")
    for n in NODES:
        ok(f"{n} paused", get(n, "pause") is True)

    print(f"\n[R3] resume on {A} fans out to everyone")
    setp(A, "pause", False)
    time.sleep(SETTLE)
    for n in NODES:
        ok(f"{n} playing", get(n, "pause") is False)
    for n in others(A):
        ok(f"{n} credited {A}", f"resumed by {A}" in osd(n))

    print(f"\n[R4] pause on {LAST} propagates back (the relay is not a host)")
    time.sleep(1.5)
    setp(LAST, "pause", True)
    time.sleep(SETTLE)
    for n in NODES:
        ok(f"{n} paused", get(n, "pause") is True)
    ok(f"{A} credited {LAST}", f"paused by {LAST}" in osd(A))

    print(f"\n[R5] seek while paused syncs everyone")
    setp(LAST, "time-pos", 77.0)
    time.sleep(SETTLE)
    for n in others(LAST):
        v = get(n, "time-pos")
        ok(f"{n} followed to 77s", v is not None and abs(v - 77.0) < 1.5, f"time-pos={v}")

    print("\n[R6] time-pos is NOT forwarded while playing")
    setp(A, "pause", False)
    time.sleep(SETTLE)
    counts = {n: osd(n).count(f"synced by {A}") for n in others(A)}
    time.sleep(4)
    for n in others(A):
        after = osd(n).count(f"synced by {A}")
        ok(f"no time-pos storm reached {n}", after == counts[n], f"{after - counts[n]} sync notices in 4s")

    print("\n[R7] no a->b->a rebound (relay has no counters; this is the only guard)")
    # start from a known state: everyone playing, so the pause below is a real change
    setp(A, "pause", False)
    time.sleep(SETTLE)
    for n in NODES:
        ok(f"{n} playing before the echo check", get(n, "pause") is False)
    before = {n: pauses(n) for n in NODES}

    setp(A, "pause", True)
    time.sleep(SETTLE)
    for n in NODES:
        ok(f"{n} paused", get(n, "pause") is True)
    # if any peer rebroadcast the echo, the originator would be told it was paused
    # by someone else and the rest would see the notice twice
    ok(f"{A} was not told its own pause came back",
       pauses(A) == before[A],
       f"{pauses(A) - before[A]} echoes returned to the originator")
    for n in others(A):
        got = pauses(n) - before[n]
        ok(f"{n} applied {A}'s pause exactly once", got == 1, f"{got} pause notices")

    print("\n[R8] any paused peer can seek for everyone")
    b = NODES[1]
    setp(b, "time-pos", 33.0)
    time.sleep(SETTLE)
    for n in others(b):
        v = get(n, "time-pos")
        ok(f"{n} followed {b}'s seek", v is not None and abs(v - 33.0) < 1.5, f"time-pos={v}")

    print("\n[R9] a late joiner does not rewind the party to 0")
    launch("nodeLate", room=ROOM, tag="late")
    time.sleep(BOOT + SETTLE)
    for n in NODES:
        v = get(n, "time-pos")
        ok(f"{n} still at 33s after a late join", v is not None and abs(v - 33.0) < 1.5, f"time-pos={v}")
    stop("late")

    print("\n[R10] a different room is isolated")
    launch("nodeOther", room="otherroom", tag="other")
    time.sleep(BOOT)
    resolve("other")
    setp("other", "time-pos", 5.0)
    time.sleep(SETTLE)
    for n in NODES:
        v = get(n, "time-pos")
        ok(f"{n} ignored the other room's seek", v is not None and abs(v - 33.0) < 1.5, f"time-pos={v}")
    ok("nobody in the main room saw the other room's node", not any("nodeOther" in known(n) for n in NODES))
    stop("other")

    print("\n[R11] a wrong secret is refused and the node gives up instead of retrying")
    launch("nodeBad", room=ROOM, secret="wrongsecret", tag="bad")
    nodes["bad"]["proc"].wait(timeout=30)
    ok("the bad-secret node exited", nodes["bad"]["proc"].poll() is not None)
    ok("it was told 403", "403" in log("bad"), "a wrong secret must not upgrade")
    ok("it did not retry forever", log("bad").count("relay: dial") <= 1, "403 is permanent")
    ok("nobody in the room saw it join", not any("nodeBad" in known(n) for n in NODES))

    print("\n[R12] the party survives the relay dying and reconnects on its own")
    relay.kill()
    relay.wait(timeout=10)
    time.sleep(3)
    ok("no node died with the relay", len(live()) == len(NODES), f"{len(live())}/{len(NODES)} alive")
    relay = launch_relay()
    time.sleep(15)  # backoff is 1,2,4,8s
    for n in NODES:
        ok(f"{n} redialled", "relay: connected" in log(n) and log(n).count("relay: connected") > 1,
           "reconnect never happened")
    setp(A, "pause", False)
    time.sleep(SETTLE + 2)
    for n in others(A):
        ok(f"{A} drives {n} again after the relay came back", get(n, "pause") is False)

    print("\n[R13] clean leave: SIGTERM tells the room")
    survivors = others(A)
    ok(f"{A} exited cleanly", stop(A) and nodes[A]["proc"].returncode == 0,
       f"rc={nodes[A]['proc'].returncode}")
    time.sleep(SETTLE)
    for n in survivors:
        ok(f"{n} was told {A} left", f"{A} has left" in osd(n))

    print("\n[R14] the room keeps working without the node that opened it")
    setp(survivors[0], "pause", True)
    time.sleep(SETTLE)
    for n in survivors:
        ok(f"{n} still in sync after {A} left", get(n, "pause") is True)
    for n in survivors:
        ok(f"{n} exited cleanly", stop(n) and nodes[n]["proc"].returncode == 0)

    relay.kill()
    relay.wait(timeout=10)


def caps_suite():
    """The abuse caps, driven with real nodes rather than the Go handler tests."""
    print("\n=== relay caps ===")

    print("\n[R15] only room CREATION is rate limited, joining is free")
    relay = launch_relay(["-roomsPerHour", "1"])
    time.sleep(1.5)
    launch(A, room="capA", tag="c1")
    time.sleep(BOOT)
    launch("nodeB", room="capB", tag="c2")
    time.sleep(BOOT)
    ok("the second NEW room was refused", "429" in log("c2"), "roomsPerHour=1 must bite")
    launch("nodeC", room="capA", tag="c3")
    time.sleep(BOOT)
    ok("joining the EXISTING room still works", "relay: connected" in log("c3"),
       "a party of eight must not exhaust one person's quota")
    for t in ("c1", "c2", "c3"):
        stop(t)
    relay.kill(); relay.wait(timeout=10)

    print("\n[R16] -maxRooms refuses a second room with 503")
    relay = launch_relay(["-maxRooms", "1"])
    time.sleep(1.5)
    launch(A, room="roomOne", tag="m1")
    time.sleep(BOOT)
    launch("nodeB", room="roomTwo", tag="m2")
    time.sleep(BOOT)
    ok("the first room connected", "relay: connected" in log("m1"))
    ok("the second room got 503", "503" in log("m2"), "maxRooms=1 must bite")
    for t in ("m1", "m2"):
        stop(t)
    relay.kill(); relay.wait(timeout=10)

    print("\n[R17] -maxConns refuses the third connection with 503")
    relay = launch_relay(["-maxConns", "2"])
    time.sleep(1.5)
    for i, t in enumerate(("n1", "n2", "n3")):
        launch(NODES[i], room="connroom", tag=t)
        time.sleep(STAGGER)
    time.sleep(BOOT)
    ok("the first two connected", "relay: connected" in log("n1") and "relay: connected" in log("n2"))
    ok("the third got 503", "503" in log("n3"), "maxConns=2 must bite")
    for t in ("n1", "n2", "n3"):
        stop(t)
    relay.kill(); relay.wait(timeout=10)


try:
    if args.mode in ("p2p", "both"):
        p2p_suite()
        for name in list(nodes):
            stop(name, signal.SIGKILL, timeout=5)
        nodes.clear()
    if args.mode in ("relay", "both"):
        relay_suite()
        for name in list(nodes):
            stop(name, signal.SIGKILL, timeout=5)
        nodes.clear()
        caps_suite()
finally:
    for name in list(nodes):
        stop(name, signal.SIGKILL, timeout=5)
    for p in relays:
        if p.poll() is None:
            p.kill()
            p.wait(timeout=5)

print(f"\nlogs: {RUN}")
print("ALL PASS" if not fails else f"{len(fails)} FAILURES: " + ", ".join(fails))
sys.exit(1 if fails else 0)
