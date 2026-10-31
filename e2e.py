#!/usr/bin/env python3
"""End-to-end watchparty suite. Real mpv processes, real transports, no Go tests.

Builds the binaries, generates a test file, starts five nodes on this machine and
drives them by writing to one node's mpv IPC socket, then asserts the result on
the other nodes' mpv. OSD notices are read back out of each node's log, since
every show-text command appears there verbatim.

Three phases, run by default at the same time as three copies of this script,
each on its own ports and in its own run directory:

  p2p    the direct HTTP mesh. Nobody is given the full peer list: each node
         bootstraps off one earlier node (A <- B,C; B <- D,E), so most of the
         mesh has to be discovered through /hi peer lists. All five still have
         to end up knowing all four others.
  relay  the same nodes against one ./relay websocket server, which is what
         people behind carrier NAT use when UPNP cannot work. Covers fan-out,
         room isolation, the secret, and reconnect after the relay dies.
         Both of these also check that a joiner playing a different file is
         refused, and that the same file renamed, moved and re-dated is not.
  caps   one fresh relay per case: every abuse cap, CREATE_KEY, and dropping a
         client that never answers pings.

    ./e2e.py                    # all three phases in parallel, ~45 seconds
    ./e2e.py --mode relay       # one phase
    ./e2e.py --file movie.mkv   # use a real file instead of a generated one
    ./e2e.py --nodes 3          # fewer nodes, faster

Needs go, mpv and (unless --file is given) ffmpeg. mpv runs headless.
Exits non-zero if any assertion fails; the run directory with all node logs is
printed at the end.
"""
import argparse, functools, glob, http.server, json, os, shutil, signal, socket, subprocess
import sys, tempfile, threading, time, urllib.error, urllib.parse, urllib.request

# the last three try to undo what party.Run pins; [2b] and [5d] check the pins win
MPVFLAGS = "--vo=null --ao=null --msg-level=all=error --speed=2 --hr-seek=no --resume-playback=yes"
# Waits poll until their condition holds, so these are ceilings, not delays. Only a
# check that something did NOT happen sleeps, for QUIET, after the positive part is in.
BOOT = 20            # mpv loading the file, plus the handshake
SETTLE = 10          # an event reaching every node
QUIET = 1.0
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
                                     headers={"hit-me-up": addr, "counter": "0", "hostname": STUB_HOST,
                                              "file": os.environ["E2E_FILE"]})
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
ap.add_argument("--mode", choices=("p2p", "relay", "caps", "all"), default="all")
args = ap.parse_args()
# every child inherits this; an mpv that sees a tty sets it raw and no-echo, and a
# SIGKILLed one never restores it, leaving the user's terminal unusable
os.dup2(os.open(os.devnull, os.O_RDONLY), 0)

# relay-mode nodes open no ports, so the phases only need disjoint port bases
if args.mode == "all":
    flags = ["--nodes", str(args.nodes)] + (["--file", args.file] if args.file else [])
    procs = [(m, subprocess.Popen([sys.executable, __file__, "--mode", m, "--port-base", str(args.port_base + 20 * i)] + flags,
                                  stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True))
             for i, m in enumerate(("p2p", "relay", "caps"))]
    for m, p in procs:
        print(f"\n######## {m} ########\n" + p.communicate()[0], end="")
    bad = [m for m, p in procs if p.returncode]
    print("\n" + ("ALL PASS" if not bad else "FAILED: " + ", ".join(bad)))
    sys.exit(1 if bad else 0)

NODES = ["node" + chr(ord("A") + i) for i in range(args.nodes)]
# Nobody gets the full list: A is the root, then each node bootstraps off an
# earlier one, so most of the mesh has to be discovered through /hi peer lists.
BOOTSTRAP = {n: (None if i == 0 else NODES[(i - 1) // 2]) for i, n in enumerate(NODES)}
PORTS = {n: args.port_base + i for i, n in enumerate(NODES)}
PORTS["stub"] = args.port_base + len(NODES)
PORTS["relay"] = args.port_base + len(NODES) + 1
PORTS["nodeSame"] = args.port_base + len(NODES) + 2
PORTS["nodeDiff"] = args.port_base + len(NODES) + 3
PORTS["http"] = args.port_base + len(NODES) + 4
PORTS["nodeKeep"] = args.port_base + len(NODES) + 5
RELAY_URL = f"ws://127.0.0.1:{PORTS['relay']}"
ROOM, SECRET = "e2eroom", "e2esecret"
A, LAST = NODES[0], NODES[-1]

REPO = os.path.dirname(os.path.abspath(__file__))
RUN = tempfile.mkdtemp(prefix="watchparty-e2e-")

fails = []
nodes = {}
relays = []
stubs = []


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


def until(cond, timeout=SETTLE):
    """Poll cond until it holds or timeout passes, and return whether it held."""
    deadline = time.time() + timeout
    while not cond():
        if time.time() > deadline:
            return False
        time.sleep(0.05)
    return True


def listening(port):
    try:
        socket.create_connection(("127.0.0.1", port), timeout=0.2).close()
        return True
    except OSError:
        return False


def near(v, x, tol=1.5):
    return v is not None and abs(v - x) < tol


def booted(n):
    return "init done" in log(n)


def connected(n, times=1):
    return log(n).count("relay: connected") >= times


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


def launch(name, addrs=None, room=None, secret=None, tag=None, relay=None, create_key=None, media=None, evict="1ms"):
    """Start a node. room!=None selects relay mode; addrs is the p2p bootstrap. evict is
    -evictAfter, short by default so the strike cases need no 5-minute wait."""
    key = tag or name
    path = f"{RUN}/{key}.log"
    cmd = [BIN, "-file", media or MEDIA, "-hostname", name, "-mpvFlags", MPVFLAGS]
    if room is not None:
        cmd += ["-relay", relay or RELAY_URL, "-room", room, "-secret", secret or SECRET]
        if create_key:
            cmd += ["-createKey", create_key]
    else:
        cmd += ["-local", "-port", str(PORTS[name])]
        if evict:
            cmd += ["-evictAfter", evict]
        if addrs:
            cmd += ["-addrs", addrs]
    p = subprocess.Popen(cmd, stdout=open(path, "wb"), stderr=subprocess.STDOUT)
    nodes[key] = {"proc": p, "log": path, "sock": None, "addr": None}
    return key


def launch_relay(extra=None):
    path = f"{RUN}/relay.log"
    env = {**os.environ, "ADDR": f"127.0.0.1:{PORTS['relay']}", **(extra or {})}
    # append: a restart in [R13] must not lose the first run's lines
    p = subprocess.Popen([RELAY_BIN], env=env, stdout=open(path, "ab"), stderr=subprocess.STDOUT)
    relays.append(p)
    return p


class Proxy:
    """A TCP hop in front of the relay. cut() drops the node's side and leaves the
    relay's side open and silent, which is how a NAT rebinding looks to the relay."""

    def __init__(self, port):
        self.port, self.pairs = port, []
        self.ls = socket.create_server(("127.0.0.1", 0))
        self.url = f"ws://127.0.0.1:{self.ls.getsockname()[1]}"
        threading.Thread(target=self.accept, daemon=True).start()

    def accept(self):
        while True:
            c, _ = self.ls.accept()
            pair = {"c": c, "u": socket.create_connection(("127.0.0.1", self.port)), "live": True}
            self.pairs.append(pair)
            threading.Thread(target=self.pump, args=(pair, "c", "u"), daemon=True).start()
            threading.Thread(target=self.pump, args=(pair, "u", "c"), daemon=True).start()

    def pump(self, pair, src, dst):
        try:
            while (b := pair[src].recv(65536)) and pair["live"]:
                pair[dst].sendall(b)
            if pair["live"]:
                pair[dst].shutdown(socket.SHUT_WR)
        except OSError:
            pass

    def cut(self):
        for pair in self.pairs:
            pair["live"] = False
            pair["c"].shutdown(socket.SHUT_RDWR)


def relay_log():
    return open(f"{RUN}/relay.log").read()


def resolve(name):
    n = nodes[name]
    n["sock"] = child_mpv_socket(n["proc"].pid)
    if not n["sock"]:
        sys.exit(f"{name} never spawned mpv; see {n['log']}")
    # not the first line: in relay mode Connect starts first and can log before it
    n["addr"] = log(name).split("share is ", 1)[1].split("\n", 1)[0].strip()
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


def fingerprint(n):
    return log(n).split("file fingerprint: ", 1)[1].split("\n", 1)[0].strip()


def variants():
    """MEDIA copied under another name, folder and timestamp, and MEDIA one byte longer."""
    os.makedirs(f"{RUN}/elsewhere", exist_ok=True)
    same, diff = f"{RUN}/elsewhere/renamed copy.mkv", f"{RUN}/different.mkv"
    shutil.copyfile(MEDIA, same)
    os.utime(same, (0, 0))
    shutil.copyfile(MEDIA, diff)
    with open(diff, "ab") as f:
        f.write(b"\0")
    return same, diff


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
    p = subprocess.Popen(cmd, stdout=subprocess.PIPE, text=True, env={**os.environ, "E2E_FILE": fingerprint(A)})
    stubs.append(p)
    # blocks until the stub is listening and registered; a stale stub left on the
    # port would answer for it and the strike loops in [13] would never end
    if p.stdout.readline() != "ready\n":
        sys.exit(f"the stub peer could not start on {STUB_ADDR}; is the port taken?")
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
    until(lambda: booted(A), BOOT)
    resolve(A)
    IP = nodes[A]["addr"].rsplit(":", 1)[0]
    STUB_ADDR = f"{IP}:{PORTS['stub']}"
    addr_of = {n: f"{IP}:{PORTS[n]}" for n in NODES}

    for n in NODES[1:]:
        launch(n, addr_of[BOOTSTRAP[n]])
        until(lambda: listening(PORTS[n]), BOOT)  # a later node may bootstrap off this one
    until(lambda: all(booted(n) for n in NODES), BOOT)
    for n in NODES[1:]:
        resolve(n)
    for n in NODES:
        ok(f"{n} advertised the expected address", nodes[n]["addr"] == addr_of[n], nodes[n]["addr"])

    print(f"\n[1] mesh closes transitively: every node learns all {len(NODES) - 1} others")
    until(lambda: all(set(others(n)) <= known(n) for n in NODES))
    for n in NODES:
        missing = set(others(n)) - known(n)
        ok(f"{n} knows the whole mesh", not missing, f"missing {sorted(missing)}" if missing else "")
    ok("nobody connected to itself", not any(n in known(n) for n in NODES))

    print(f"\n[2] all {len(NODES)} start paused")
    for n in NODES:
        ok(f"{n} paused", get(n, "pause") is True)

    print("\n[2b] the sync-critical mpv options win over the user's own flags")
    for n in NODES:
        ok(f"{n} plays at normal speed", get(n, "speed") == 1, f"speed={get(n, 'speed')}")
        ok(f"{n} seeks precisely", get(n, "hr-seek") in (True, "yes"), f"hr-seek={get(n, 'hr-seek')}")
        ok(f"{n} ignores saved positions", get(n, "resume-playback") is False)

    print(f"\n[3] resume on {A} propagates to all {len(NODES) - 1} others")
    setp(A, "pause", False)
    until(lambda: all(get(n, "pause") is False for n in NODES)
          and all(f"resumed by {A}" in osd(n) for n in others(A)))
    for n in NODES:
        ok(f"{n} playing", get(n, "pause") is False)
    for n in others(A):
        ok(f"{n} credited {A}", f"resumed by {A}" in osd(n))

    print(f"\n[4] pause on {LAST} propagates back to everyone (no central host)")
    time.sleep(1.5)
    setp(LAST, "pause", True)
    until(lambda: all(get(n, "pause") is True for n in NODES) and f"paused by {LAST}" in osd(A))
    for n in NODES:
        ok(f"{n} paused", get(n, "pause") is True)
    ok(f"{A} credited {LAST}", f"paused by {LAST}" in osd(A))

    print(f"\n[5] seek while paused syncs everyone (from {LAST}, which drove the pause)")
    setp(LAST, "time-pos", 77.0)
    until(lambda: all(near(get(n, "time-pos"), 77.0) for n in others(LAST)) and f"synced by {LAST}" in osd(A))
    for n in others(LAST):
        v = get(n, "time-pos")
        ok(f"{n} followed to 77s", near(v, 77.0), f"time-pos={v}")
    ok(f"{A} credited {LAST} for sync", f"synced by {LAST}" in osd(A))

    print("\n[5b] any paused peer can seek for everyone, not just whoever drove the pause")
    b = NODES[1]  # applied LAST's pause in [4]; under the old slave hack this was muted
    setp(b, "time-pos", 12.0)
    until(lambda: all(near(get(n, "time-pos"), 12.0) for n in others(b)))
    for n in others(b):
        v = get(n, "time-pos")
        ok(f"{n} followed {b}'s seek", near(v, 12.0), f"time-pos={v}")

    print("\n[5d] an arrow-key (relative) seek lands exactly, and goes out once")
    sent = lambda: sum("broadcasted event" in l and '"time-pos"' in l for l in log(b).splitlines())
    before = sent()
    ipc(nodes[b]["sock"], "seek", 7, "relative")
    until(lambda: all(near(get(n, "time-pos"), 19.0, 0.15) for n in NODES))
    time.sleep(QUIET)  # a second broadcast would come right behind the first
    for n in NODES:
        v = get(n, "time-pos")
        ok(f"{n} at exactly 19s, not a keyframe", near(v, 19.0, 0.15), f"time-pos={v}")
    ok(f"{b} broadcast one position for one seek", sent() - before == 1, f"{sent() - before} sent")

    print("\n[5c] an applied event is not echoed back (the a->b->a rebound)")
    # start from a known state: everyone playing, so the pause below is a real change
    setp(A, "pause", False)
    until(lambda: all(get(n, "pause") is False for n in NODES))
    for n in NODES:
        ok(f"{n} playing before the echo check", get(n, "pause") is False)
    before = {n: pauses(n) for n in NODES}

    setp(A, "pause", True)
    until(lambda: all(get(n, "pause") is True for n in NODES)
          and all(pauses(n) > before[n] for n in others(A)))
    time.sleep(QUIET)  # an echo would be right behind the pause
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
    until(lambda: all(get(n, "pause") is False for n in NODES))
    counts = {n: osd(n).count(f"synced by {A}") for n in others(A)}
    time.sleep(4)  # A is playing, mpv emits time-pos ~10x/sec
    for n in others(A):
        after = osd(n).count(f"synced by {A}")
        ok(f"no time-pos storm reached {n}", after == counts[n], f"{after - counts[n]} sync notices in 4s")

    print("\n[7] a peer that only ever followed can still drive the party")
    b = NODES[1]
    setp(b, "pause", True)
    until(lambda: get(A, "pause") is True)
    ok(f"{b}'s pause reached {A}", get(A, "pause") is True)
    setp(b, "pause", False)
    until(lambda: all(get(n, "pause") is False for n in others(b)))
    for n in others(b):
        ok(f"{b} (ex-slave) resume reached {n}", get(n, "pause") is False)

    print("\n[8] repeated round trips are not swallowed by the counter check")
    rounds = 0
    for i in range(5):
        setp(A, "pause", True)
        mid = until(lambda: all(get(n, "pause") is True for n in others(A)), 5)
        setp(A, "pause", False)
        end = until(lambda: all(get(n, "pause") is False for n in others(A)), 5)
        rounds += mid and end
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
    until(lambda: "skipped bye from" in log(A), 5)
    ok(f"{A} skipped the stale bye", code == 204, f"HTTP {code}")
    ok(f"{A} logged it as skipped", "skipped bye from" in log(A))
    setp(LAST, "pause", True)
    until(lambda: get(b, "pause") is True)
    ok(f"{b} is still in the mesh", get(b, "pause") is True, "still receives events after the forged bye")
    setp(LAST, "pause", False)
    until(lambda: all(get(n, "pause") is False for n in NODES))

    print("\n[11b] a joiner playing a different file is refused; the same file renamed is not")
    same, diff = variants()
    launch("nodeSame", addr_of[A], media=same)
    until(lambda: booted("nodeSame") and all("nodeSame has joined" in osd(n) for n in NODES), BOOT)
    resolve("nodeSame")
    ok("the renamed, moved, re-dated copy got the same fingerprint", fingerprint("nodeSame") == fingerprint(A))
    for n in NODES:
        ok(f"{n} let the renamed copy in", "nodeSame has joined" in osd(n))
    launch("nodeDiff", addr_of[A], media=diff)
    until(lambda: nodes["nodeDiff"]["proc"].poll() is not None, BOOT)
    nodes["nodeDiff"]["sock"] = child_mpv_socket(nodes["nodeDiff"]["proc"].pid)  # only if it wrongly got in
    ok("the different file's node exited with an error", nodes["nodeDiff"]["proc"].poll() not in (None, 0),
       f"rc={nodes['nodeDiff']['proc'].returncode}")
    ok("it was told why", "not the same file as the party's" in log("nodeDiff"))
    ok(f"{A} logged the refusal", "refused nodeDiff" in log(A))
    ok("nobody saw it join", not any("nodeDiff" in known(n) for n in NODES))
    stop("nodeDiff")
    stop("nodeSame")
    until(lambda: all("nodeSame has left" in osd(n) for n in NODES))

    print(f"\n[12] SIGKILL {LAST}: every survivor evicts it after 3 strikes and stays synced")
    stop(LAST, signal.SIGKILL)
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
    until(lambda: all(dropped(n, dead) for n in survivors))
    for n in survivors:
        ok(f"{n} evicted {LAST}", dropped(n, dead), f"{strikes(n, dead, LAST)} strikes")
    ok("no survivor stalled on the dead peer", elapsed < 90, f"{elapsed:.1f}s for {len(survivors)} nodes")
    setp(A, "pause", not get(A, "pause"))
    want = get(A, "pause")
    until(lambda: all(get(n, "pause") is want for n in survivors[1:]))
    for n in survivors[1:]:
        ok(f"{A} still drives {n} after the eviction", get(n, "pause") is want)

    print("\n[13] one success resets the strike count (a flaky peer must not be evicted)")
    s1 = stub(nodes[A]["addr"])
    until(lambda: f"{STUB_HOST} has joined" in osd(A), 5)
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
    until(lambda: dropped(A, STUB_ADDR), 5)
    ok("3rd consecutive strike evicts", dropped(A, STUB_ADDR))

    print("\n[13b] with the default -evictAfter, a burst of strikes does not evict a peer")
    launch("nodeKeep", evict=None)
    until(lambda: booted("nodeKeep"), BOOT)
    resolve("nodeKeep")
    s3 = stub(nodes["nodeKeep"]["addr"])
    until(lambda: f"{STUB_HOST} has joined" in osd("nodeKeep"), 5)
    stub_stop(s3)
    tries = 0  # an evicted peer is never sent to again, so its strikes stop at 3
    while strikes("nodeKeep", STUB_ADDR, STUB_HOST) < 4 and tries < 8:
        toggle("nodeKeep")
        tries += 1
    ok("4 strikes within seconds: still in the mesh",
       strikes("nodeKeep", STUB_ADDR, STUB_HOST) >= 4 and not dropped("nodeKeep", STUB_ADDR),
       f"{strikes('nodeKeep', STUB_ADDR, STUB_HOST)} failures logged, evictAfter is 5m")
    stop("nodeKeep")

    print(f"\n[14] clean shutdown: SIGTERM on {A} broadcasts /bye to every survivor")
    clean = stop(A)
    ok(f"{A} exited cleanly", clean and nodes[A]["proc"].returncode == 0, f"rc={nodes[A]['proc'].returncode}")
    until(lambda: all(f"{A} has left" in osd(n) for n in survivors[1:]))
    for n in survivors[1:]:
        ok(f"{n} was told {A} left", f"{A} has left" in osd(n),
           "Shutdown must not derive its timeout from the cancelled signal context")
        ok(f"{n} did not error on {A}'s departure", "does not exists" not in log(n))

    print("\n[15] the remaining mesh keeps working without the node that bootstrapped it")
    rest = survivors[1:]
    setp(rest[0], "pause", not get(rest[0], "pause"))
    want = get(rest[0], "pause")
    until(lambda: all(get(n, "pause") is want for n in rest))
    for n in rest:
        ok(f"{n} still in sync after {A} left", get(n, "pause") is want)
    for n in rest:
        ok(f"{n} exited cleanly", stop(n) and nodes[n]["proc"].returncode == 0)

def relay_suite():
    print(f"\n=== relay setup: {len(NODES)} nodes through one websocket server ===")
    relay = launch_relay()
    until(lambda: listening(PORTS["relay"]))

    for n in NODES:
        launch(n, room=ROOM)  # no listener to wait for, so no stagger
    until(lambda: all(booted(n) and connected(n) for n in NODES), BOOT)
    for n in NODES:
        resolve(n)

    print(f"\n[R1] every node learns all {len(NODES) - 1} others through the relay")
    until(lambda: all(set(others(n)) <= known(n) for n in NODES))
    for n in NODES:
        missing = set(others(n)) - known(n)
        ok(f"{n} knows the room", not missing, f"missing {sorted(missing)}" if missing else "")
    ok("nobody was told it joined itself", not any(n in known(n) for n in NODES))

    print(f"\n[R2] all {len(NODES)} start paused")
    for n in NODES:
        ok(f"{n} paused", get(n, "pause") is True)

    print(f"\n[R3] resume on {A} fans out to everyone")
    setp(A, "pause", False)
    until(lambda: all(get(n, "pause") is False for n in NODES)
          and all(f"resumed by {A}" in osd(n) for n in others(A)))
    for n in NODES:
        ok(f"{n} playing", get(n, "pause") is False)
    for n in others(A):
        ok(f"{n} credited {A}", f"resumed by {A}" in osd(n))

    print(f"\n[R4] pause on {LAST} propagates back (the relay is not a host)")
    time.sleep(1.5)
    setp(LAST, "pause", True)
    until(lambda: all(get(n, "pause") is True for n in NODES) and f"paused by {LAST}" in osd(A))
    for n in NODES:
        ok(f"{n} paused", get(n, "pause") is True)
    ok(f"{A} credited {LAST}", f"paused by {LAST}" in osd(A))

    print(f"\n[R5] seek while paused syncs everyone")
    setp(LAST, "time-pos", 77.0)
    until(lambda: all(near(get(n, "time-pos"), 77.0) for n in others(LAST)))
    for n in others(LAST):
        v = get(n, "time-pos")
        ok(f"{n} followed to 77s", near(v, 77.0), f"time-pos={v}")

    print("\n[R6] time-pos is NOT forwarded while playing")
    setp(A, "pause", False)
    until(lambda: all(get(n, "pause") is False for n in NODES))
    counts = {n: osd(n).count(f"synced by {A}") for n in others(A)}
    time.sleep(4)
    for n in others(A):
        after = osd(n).count(f"synced by {A}")
        ok(f"no time-pos storm reached {n}", after == counts[n], f"{after - counts[n]} sync notices in 4s")

    print("\n[R7] no a->b->a rebound (relay has no counters; this is the only guard)")
    # start from a known state: everyone playing, so the pause below is a real change
    setp(A, "pause", False)
    until(lambda: all(get(n, "pause") is False for n in NODES))
    for n in NODES:
        ok(f"{n} playing before the echo check", get(n, "pause") is False)
    before = {n: pauses(n) for n in NODES}

    setp(A, "pause", True)
    until(lambda: all(get(n, "pause") is True for n in NODES)
          and all(pauses(n) > before[n] for n in others(A)))
    time.sleep(QUIET)  # an echo would be right behind the pause
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
    until(lambda: all(near(get(n, "time-pos"), 33.0) for n in others(b)))
    for n in others(b):
        v = get(n, "time-pos")
        ok(f"{n} followed {b}'s seek", near(v, 33.0), f"time-pos={v}")

    print("\n[R9] a late joiner does not rewind the party to 0")
    launch("nodeLate", room=ROOM, tag="late", relay=RELAY_URL + "/")
    until(lambda: booted("late") and connected("late"), BOOT)
    time.sleep(QUIET)  # a rewind would go out as soon as its file loaded
    ok("a trailing slash on the relay address still connects", connected("late"))
    for n in NODES:
        v = get(n, "time-pos")
        ok(f"{n} still at 33s after a late join", near(v, 33.0), f"time-pos={v}")
    stop("late")

    print("\n[R10] a different room is isolated")
    launch("nodeOther", room="otherroom", tag="other")
    until(lambda: booted("other") and connected("other"), BOOT)
    resolve("other")
    setp("other", "time-pos", 5.0)
    time.sleep(QUIET)  # what we wait for is something NOT arriving
    for n in NODES:
        v = get(n, "time-pos")
        ok(f"{n} ignored the other room's seek", near(v, 33.0), f"time-pos={v}")
    ok("nobody in the main room saw the other room's node", not any("nodeOther" in known(n) for n in NODES))
    stop("other")

    print("\n[R11] a wrong secret is refused and the node gives up instead of retrying")
    launch("nodeBad", room=ROOM, secret="wrongsecret", tag="bad")
    nodes["bad"]["proc"].wait(timeout=30)
    ok("the bad-secret node exited", nodes["bad"]["proc"].poll() is not None)
    ok("it was told 403", "403" in log("bad"), "a wrong secret must not upgrade")
    ok("it did not retry forever", log("bad").count("relay: dial") <= 1, "403 is permanent")
    ok("nobody in the room saw it join", not any("nodeBad" in known(n) for n in NODES))

    print("\n[R11b] a joiner playing a different file is refused; the same file renamed is not")
    same, diff = variants()
    launch("nodeSame", room=ROOM, media=same)
    until(lambda: connected("nodeSame") and all("nodeSame has joined" in osd(n) for n in NODES), BOOT)
    resolve("nodeSame")
    ok("the renamed, moved, re-dated copy got the same fingerprint", fingerprint("nodeSame") == fingerprint(A))
    for n in NODES:
        ok(f"{n} let the renamed copy in", "nodeSame has joined" in osd(n))
    stop("nodeSame")
    launch("nodeDiff", room=ROOM, media=diff)
    until(lambda: nodes["nodeDiff"]["proc"].poll() is not None, BOOT)
    nodes["nodeDiff"]["sock"] = child_mpv_socket(nodes["nodeDiff"]["proc"].pid)  # only if it wrongly got in
    ok("the different file's node exited with an error", nodes["nodeDiff"]["proc"].poll() not in (None, 0),
       f"rc={nodes['nodeDiff']['proc'].returncode}")
    ok("it was told 409 and why", "409" in log("nodeDiff") and "not the same file as the party's" in log("nodeDiff"))
    ok("it did not retry", log("nodeDiff").count("relay: dial") == 0, "409 is permanent")
    ok("nobody in the room saw it join", not any("nodeDiff" in known(n) for n in NODES))
    stop("nodeDiff")

    print("\n[R11c] a URL plays too: the same URL gets in, another URL is refused")

    class Quiet(http.server.SimpleHTTPRequestHandler):
        delay = 0

        def log_message(self, *a):
            pass

        def do_GET(self):
            time.sleep(Quiet.delay)
            super().do_GET()

    web = http.server.ThreadingHTTPServer(("127.0.0.1", PORTS["http"]),
                                          functools.partial(Quiet, directory=os.path.dirname(MEDIA)))
    threading.Thread(target=web.serve_forever, daemon=True).start()
    url = f"http://127.0.0.1:{PORTS['http']}/{urllib.parse.quote(os.path.basename(MEDIA))}"
    launch("nodeUrl1", room="urlroom", media=url)
    until(lambda: booted("nodeUrl1") and connected("nodeUrl1"), BOOT)
    resolve("nodeUrl1")
    launch("nodeUrl2", room="urlroom", media=url)
    until(lambda: booted("nodeUrl2") and "nodeUrl2 has joined" in osd("nodeUrl1"), BOOT)
    resolve("nodeUrl2")
    ok("the same URL got in", "nodeUrl2 has joined" in osd("nodeUrl1"))
    launch("nodeUrl3", room="urlroom", media=url + "?other")
    until(lambda: nodes["nodeUrl3"]["proc"].poll() is not None, BOOT)
    nodes["nodeUrl3"]["sock"] = child_mpv_socket(nodes["nodeUrl3"]["proc"].pid)  # only if it wrongly got in
    ok("another URL was refused", "409" in log("nodeUrl3") and "not the same file" in log("nodeUrl3"))
    ok("nobody saw it join", "nodeUrl3" not in known("nodeUrl1"))

    print("\n[R11d] a joiner still loading a slow stream applies only the last of what it missed")
    Quiet.delay = 3
    launch("nodeUrl4", room="urlroom", media=url)
    until(lambda: "nodeUrl4 has joined" in osd("nodeUrl1"), BOOT)
    for pos in (10, 20, 30, 40, 50):
        setp("nodeUrl1", "time-pos", pos)
        until(lambda: near(get("nodeUrl1", "time-pos"), pos))
    ok("the seeks landed while nodeUrl4 was loading", not booted("nodeUrl4"))
    until(lambda: booted("nodeUrl4"), BOOT)
    Quiet.delay = 0
    resolve("nodeUrl4")
    until(lambda: near(get("nodeUrl4", "time-pos"), 50))
    ok("nodeUrl4 is where nodeUrl1 stopped", near(get("nodeUrl4", "time-pos"), 50), f"at {get('nodeUrl4', 'time-pos')}")
    ok("one seek, not five", osd("nodeUrl4").count("synced by nodeUrl1") == 1,
       f"{osd('nodeUrl4').count('synced by nodeUrl1')} syncs")
    for n in ("nodeUrl1", "nodeUrl2", "nodeUrl3", "nodeUrl4"):
        stop(n)
    web.shutdown()

    print("\n[R12] a node that reconnects while the relay still holds its dead conn")
    proxy = Proxy(PORTS["relay"])
    launch("nodeFlaky", room=ROOM, tag="flaky", relay=proxy.url)
    until(lambda: all("nodeFlaky has joined" in osd(n) for n in NODES), BOOT)
    proxy.cut()
    until(lambda: connected("flaky", 2))  # 1s backoff, then the redial
    time.sleep(QUIET)  # a second join or a false leave would follow the swap at once
    ok("nodeFlaky redialled", log("flaky").count("relay: connected") == 2, "reconnect never happened")
    for n in NODES:
        ok(f"{n} was told nodeFlaky joined once", osd(n).count("nodeFlaky has joined") == 1,
           f"{osd(n).count('nodeFlaky has joined')} joins")
        ok(f"{n} was not told the stale nodeFlaky left", "nodeFlaky has left" not in osd(n))
    ok("nodeFlaky was not told it is in the room", "connected to nodeFlaky" not in osd("flaky"))
    stop("flaky")
    until(lambda: all("nodeFlaky has left" in osd(n) for n in NODES))
    for n in NODES:
        ok(f"{n} was told nodeFlaky left", "nodeFlaky has left" in osd(n))

    print("\n[R13] the party survives the relay dying and reconnects on its own")
    relay.kill()
    relay.wait(timeout=10)
    time.sleep(3)
    ok("no node died with the relay", len(live()) == len(NODES), f"{len(live())}/{len(NODES)} alive")
    relay = launch_relay()
    until(lambda: all(connected(n, 2) for n in NODES), 30)  # backoff is 1,2,4,8s
    for n in NODES:
        ok(f"{n} redialled", connected(n, 2), "reconnect never happened")
    setp(A, "pause", False)
    until(lambda: all(get(n, "pause") is False for n in others(A)))
    for n in others(A):
        ok(f"{A} drives {n} again after the relay came back", get(n, "pause") is False)

    print("\n[R14] clean leave: SIGTERM tells the room")
    survivors = others(A)
    ok(f"{A} exited cleanly", stop(A) and nodes[A]["proc"].returncode == 0,
       f"rc={nodes[A]['proc'].returncode}")
    until(lambda: all(f"{A} has left" in osd(n) for n in survivors))
    for n in survivors:
        ok(f"{n} was told {A} left", f"{A} has left" in osd(n))

    print("\n[R15] the room keeps working without the node that opened it")
    setp(survivors[0], "pause", True)
    until(lambda: all(get(n, "pause") is True for n in survivors))
    for n in survivors:
        ok(f"{n} still in sync after {A} left", get(n, "pause") is True)

    print("\n[R15b] closing mpv is a normal end, not an unexpected stop")
    quitter, rest = survivors[0], survivors[1:]
    ipc(nodes[quitter]["sock"], "quit")
    try:
        nodes[quitter]["proc"].wait(timeout=20)
    except subprocess.TimeoutExpired:
        pass
    ok(f"{quitter} exits 0 when its mpv is closed", nodes[quitter]["proc"].returncode == 0,
       f"rc={nodes[quitter]['proc'].returncode}")
    until(lambda: all(f"{quitter} has left" in osd(n) for n in rest))
    for n in rest:
        ok(f"{n} was told {quitter} left", f"{quitter} has left" in osd(n))
    for n in rest:
        ok(f"{n} exited cleanly", stop(n) and nodes[n]["proc"].returncode == 0)

    relay.kill()
    relay.wait(timeout=10)


def caps_suite():
    """The abuse caps, driven with real nodes rather than the Go handler tests."""
    print("\n=== relay caps ===")

    print("\n[R16] only room CREATION is rate limited, joining is free")
    relay = launch_relay({"ROOMS_PER_HOUR": "1"})
    until(lambda: listening(PORTS["relay"]))
    launch(A, room="capA", tag="c1")
    until(lambda: connected("c1"), BOOT)
    launch("nodeB", room="capB", tag="c2")
    until(lambda: "429" in log("c2"), BOOT)
    ok("the second NEW room was refused", "429" in log("c2"), "ROOMS_PER_HOUR=1 must bite")
    launch("nodeC", room="capA", tag="c3")
    until(lambda: connected("c3"), BOOT)
    ok("joining the EXISTING room still works", connected("c3"),
       "a party of eight must not exhaust one person's quota")
    for t in ("c1", "c2", "c3"):
        stop(t)
    relay.kill(); relay.wait(timeout=10)

    print("\n[R17] MAX_ROOMS refuses a second room with 503")
    relay = launch_relay({"MAX_ROOMS": "1"})
    until(lambda: listening(PORTS["relay"]))
    launch(A, room="roomOne", tag="m1")
    until(lambda: connected("m1"), BOOT)
    launch("nodeB", room="roomTwo", tag="m2")
    until(lambda: "503" in log("m2"), BOOT)
    ok("the first room connected", connected("m1"))
    ok("the second room got 503", "503" in log("m2"), "MAX_ROOMS=1 must bite")
    for t in ("m1", "m2"):
        stop(t)
    relay.kill(); relay.wait(timeout=10)

    print("\n[R18] MAX_CONNS refuses the third connection with 503")
    relay = launch_relay({"MAX_CONNS": "2"})
    until(lambda: listening(PORTS["relay"]))
    for i, t in enumerate(("n1", "n2")):
        launch(NODES[i], room="connroom", tag=t)
        until(lambda: connected(t), BOOT)
    launch(NODES[2], room="connroom", tag="n3")
    until(lambda: "503" in log("n3"), BOOT)
    ok("the first two connected", connected("n1") and connected("n2"))
    ok("the third got 503", "503" in log("n3"), "MAX_CONNS=2 must bite")
    for t in ("n1", "n2", "n3"):
        stop(t)
    relay.kill(); relay.wait(timeout=10)

    print("\n[R19] -createKey: only the key holder opens a room, the rest just join")
    relay = launch_relay({"CREATE_KEY": "k"})
    until(lambda: listening(PORTS["relay"]))
    launch("nodeB", room="keyroom", tag="k1")
    until(lambda: "not open yet" in log("k1"), BOOT)
    ok("a keyless node waits for the room instead of giving up",
       "not open yet" in log("k1") and nodes["k1"]["proc"].poll() is None)
    launch("nodeC", room="keyroom", tag="k2", create_key="wrong")
    nodes["k2"]["proc"].wait(timeout=30)
    ok("a wrong key is refused and the node exits", "403" in log("k2"))
    launch(A, room="keyroom", tag="k3", create_key="k")
    until(lambda: connected("k3"), BOOT)
    ok("the key holder opened the room", connected("k3"))
    until(lambda: connected("k1"), 35)  # the keyless node's backoff may be up to 16s by now
    ok("the waiting node got in once the room was open", connected("k1"))
    for t in ("k1", "k2", "k3"):
        stop(t)
    relay.kill(); relay.wait(timeout=10)

    print("\n[R20] a member that stops answering pings is dropped and the room hears it left")
    relay = launch_relay()
    until(lambda: listening(PORTS["relay"]))
    launch(A, room="deafroom", tag="d1")
    until(lambda: connected("d1"), BOOT)
    deaf = socket.create_connection(("127.0.0.1", PORTS["relay"]))
    deaf.sendall((f"GET /ws?room=deafroom&hostname=deaf&file={fingerprint('d1')} HTTP/1.1\r\nHost: 127.0.0.1\r\n"
                  f"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n"
                  f"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nroom-secret: {SECRET}\r\n\r\n").encode())
    ok("the deaf client got in", deaf.recv(4096).startswith(b"HTTP/1.1 101"))
    until(lambda: "deaf has joined" in osd("d1"))
    ok(f"{A} was told deaf joined", "deaf has joined" in osd("d1"))
    until(lambda: "deaf has left" in osd("d1"), 50)  # one 30s ping interval plus the 10s pong wait
    ok(f"{A} was told deaf left", "deaf has left" in osd("d1"), "the relay kept a member that never answers pings")
    deaf.close()
    stop("d1")
    relay.kill(); relay.wait(timeout=10)


try:
    {"p2p": p2p_suite, "relay": relay_suite, "caps": caps_suite}[args.mode]()
finally:
    for name in list(nodes):
        stop(name, signal.SIGKILL, timeout=5)
    for p in stubs + relays:
        if p.poll() is None:
            p.kill()
            p.wait(timeout=5)

print(f"\nlogs: {RUN}")
print("ALL PASS" if not fails else f"{len(fails)} FAILURES: " + ", ".join(fails))
sys.exit(1 if fails else 0)
