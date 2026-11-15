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
import argparse, contextlib, functools, glob, http.client, http.server, itertools, json, os, re, shutil, signal, socket, subprocess
import plistlib, queue, random, sys, tempfile, threading, time, traceback, urllib.error, urllib.parse, urllib.request
sys.dont_write_bytecode = True  # no __pycache__ in the repo
import e2e_lib
from e2e_lib import Gateway, Page, Release, find_page, sha

# the last three try to undo what party.Run pins; [2b] and [5d] check the pins win
MPVFLAGS = "--vo=null --ao=null --msg-level=all=error --speed=2 --hr-seek=no --cache-pause=no --resume-playback=yes"
# Waits poll until their condition holds, so these are ceilings, not delays. Only a
# check that something did NOT happen sleeps, for QUIET, after the positive part is in.
BOOT = 20            # mpv loading the file, plus the handshake
SETTLE = 10          # an event reaching every node
QUIET = 1.0
CASES_AT_ONCE = 4    # cases with parties of their own run side by side, this many at a time
JITTER = 0.4         # a message crosses the proxy twice, node to relay to node: this stays under QUIET
STUB_HOST = "slowpeer"


# ---------------------------------------------------------------- stub peer --
# Re-invokes this script as `--stub` so the whole suite stays one file. It stands
# in for a peer that can go away and come back at the same address without
# re-registering, which is the only way to isolate the strike-reset path.

def run_stub(addr, peer, register):
    port = int(addr.rsplit(":", 1)[1])
    # SIGINT goes to any thread that doesn't block it: a handler thread's only sets a flag,
    # and the main thread slept on, so blocked in every thread, the main one waits for it
    signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGINT})

    class H(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            body = self.rfile.read(int(self.headers.get("content-length") or 0))
            if self.path == "/event" and os.environ.get("E2E_STUB_EVENTS"):
                with open(os.environ["E2E_STUB_EVENTS"], "ab") as f:
                    f.write(body + b"\n")
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
                                              "file": os.environ["E2E_FILE"], "secret": os.environ["E2E_SECRET"]})
        urllib.request.urlopen(req, timeout=10).read()
    print("ready", flush=True)
    signal.sigwait({signal.SIGINT})


if len(sys.argv) > 1 and sys.argv[1] == "--stub":
    run_stub(sys.argv[2], sys.argv[3], "noregister" not in sys.argv)
    sys.exit(0)


# ------------------------------------------------------------------- runner --
PHASES = ("p2p", "relay", "media", "caps", "gui", "upnp")
JITTER_PHASES = ("relay", "media", "gui")
ap = argparse.ArgumentParser()
ap.add_argument("--file", help="media file to play (default: generate one with ffmpeg)")
ap.add_argument("--port-base", type=int, default=6969)
ap.add_argument("--nodes", type=int, default=5, choices=range(3, 9))
ap.add_argument("--mode", choices=PHASES + ("all",), default="all")
ap.add_argument("--audio", action="store_true", help="real audio output, muted, instead of --ao=null")
ap.add_argument("--jitter", metavar="SEED", help="random delays on the relay and gui phases' connections, replayable by seed")
ap.add_argument("--bin", help="a folder of binaries built by build(), so the phases don't build them each")
args = ap.parse_args()
if args.audio:
    MPVFLAGS = MPVFLAGS.replace("--ao=null", "--mute=yes")
# every child inherits this; an mpv that sees a tty sets it raw and no-echo, and a
# SIGKILLed one never restores it, leaving the user's terminal unusable
os.dup2(os.open(os.devnull, os.O_RDONLY), 0)

REPO = os.path.dirname(os.path.abspath(__file__))


def build(dest, gui):
    """What every phase runs, and with gui what the gui and upnp phases add, in one go
    build so its packages compile in parallel and once."""
    pkgs = [".", "./relay", "./party/fakeseed"] + (["./gui", "./gui/fakeportal"] if gui else [])
    subprocess.run(["go", "build", "-o", dest + "/"] + pkgs, cwd=REPO, check=True)
    if gui:
        os.rename(dest + "/gui", dest + "/watchparty-gui")


# relay-mode nodes open no ports, so the phases only need disjoint port bases
if args.mode == "all":
    flags = ["--nodes", str(args.nodes)] + (["--file", args.file] if args.file else []) + (["--audio"] if args.audio else []) + (["--jitter", args.jitter] if args.jitter else [])
    if not args.bin:
        bins = tempfile.mkdtemp(prefix="watchparty-e2e-bin-")
        print("building every phase's binaries once")
        build(bins, True)
        flags += ["--bin", bins]
    procs = [(m, subprocess.Popen([sys.executable, __file__, "--mode", m, "--port-base", str(args.port_base + 20 * i)] + flags,
                                  stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True))
             for i, m in enumerate(PHASES)]
    took = []
    for m, p in procs:
        out = p.communicate()[0]
        print(f"\n######## {m} ########\n" + out, end="")
        took += [f"{m:6} {line}" for line in out.splitlines() if line.startswith(("took:", "slowest:"))]
    print("\n" + "\n".join(took))
    bad = [m for m, p in procs if p.returncode]
    if not args.bin:
        shutil.rmtree(bins)
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
PORTS["lagA"] = args.port_base + len(NODES) + 6
PORTS["lagB"] = args.port_base + len(NODES) + 7
PORTS["release"] = args.port_base + len(NODES) + 8
PORTS["wifiHost"] = args.port_base + len(NODES) + 9
PORTS["wifiGuest"] = args.port_base + len(NODES) + 10
PORTS["cliRelease"] = args.port_base + len(NODES) + 11
PORTS["gateway"] = args.port_base + len(NODES) + 12
PORTS["upnpNode"] = args.port_base + len(NODES) + 13
PORTS["jitter"] = args.port_base + len(NODES) + 14
JITTERED = args.jitter if args.mode in JITTER_PHASES else None
RELAY_URL = f"ws://127.0.0.1:{PORTS['jitter' if JITTERED else 'relay']}"
CREATE_KEY = "k"
# the first node launched into a room is its host and has the key, as someone who starts a
# party and shares the invite; the rest join without it. create_key overrides either way.
hosted = set()
ROOM, SECRET = "e2eroom", "e2esecret"
A, LAST = NODES[0], NODES[-1]

RUN = tempfile.mkdtemp(prefix="watchparty-e2e-")

fails = []
nodes = {}
relays = []
stubs = []


def ok(name, cond, detail=""):
    if not cond:
        now = getattr(running, "id", None) or (cases[-1][0] if cases else None)
        fails.append(f"{now} {name}" if now else name)
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


class Stop(Exception):
    """A setup step failed: every case after it would fail for the same reason."""


def must(name, cond, detail=""):
    """ok(), but a failure ends the phase here instead of cascading into later cases."""
    ok(name, cond, detail)
    if not cond:
        raise Stop(name)


def until(cond, timeout=SETTLE):
    return e2e_lib.until(cond, timeout)


def listening(port):
    try:
        socket.create_connection(("127.0.0.1", port), timeout=0.2).close()
        return True
    except OSError:
        return False


def near(v, x, tol=1.5):
    """Either can be None: mpv has no position while it switches files."""
    return v is not None and x is not None and abs(v - x) < tol


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


def played(n):
    """The file names this node's mpv reported, oldest first."""
    return [json.loads(l[20:].removeprefix("now playing: ")) for l in log(n).splitlines() if l[20:].startswith("now playing: ")]


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


def quiet(names, timeout=SETTLE):
    """Wait until these nodes' logs have stopped growing for QUIET, longer than anything
    can take to arrive. For a step that a message still on its way would undo."""
    last, since = None, time.time()

    def still():
        nonlocal last, since
        now = [os.path.getsize(nodes[n]["log"]) for n in names]
        if now != last:
            last, since = now, time.time()
        return time.time() - since >= QUIET
    return until(still, timeout)


def where(names):
    """What each node's mpv says now, for a failure's detail."""
    return str({n: (get(n, "pause"), get(n, "time-pos"), get(n, "filename")) for n in names})


def playing(names):
    return all(get(n, "pause") is False for n in names)


def stopped(names):
    return all(get(n, "pause") is True for n in names)


def in_step(names, at=None, tol=0.15):
    """The end state the protocol guarantees: everyone stopped, on one file, on one second
    (near at, if given). Positions are only ever compared stopped: each node starts playing
    when the resume reaches it, so playing nodes are apart by however long that took."""
    now = [(get(n, "pause"), get(n, "time-pos"), get(n, "filename")) for n in names]
    return (all(p is True and f == now[0][2] and near(t, now[0][1], tol) for p, t, f in now)
            and (at is None or near(now[0][1], at)))


def offsets(names):
    """How far each node plays from the first. What a check that nothing moved compares
    while they play, before and after; never the positions themselves."""
    ref = get(names[0], "time-pos")
    return {n: None if ref is None or get(n, "time-pos") is None else get(n, "time-pos") - ref for n in names}


def unmoved(before, after, tol=0.15):
    return all(before[n] is not None and after[n] is not None and abs(after[n] - before[n]) <= tol for n in before)


def ready(name, cond, detail):
    """must() for a fixture's setup, which says nothing when it worked."""
    if not cond:
        must(name, cond, detail())


@contextlib.contextmanager
def mesh(prefix, n=3, **kw):
    """party() for the direct mesh: n nodes on free ports, each bootstrapping off an earlier
    one (a <- b, c; b <- d, e), so most of it is found through /hi peer lists. Ready once
    every node knows every other, since a node sends only to the peers it knows."""
    names = [f"{prefix}-{chr(ord('a') + i)}" for i in range(n)]
    ports = {x: free_port() for x in names}
    try:
        launch(names[0], None, port=ports[names[0]], **kw)
        ready(f"{names[0]} is up", until(lambda: booted(names[0]), BOOT), lambda: log(names[0])[-300:])
        resolve(names[0])
        ip = nodes[names[0]]["addr"].rsplit(":", 1)[0]
        for i, name in enumerate(names[1:], 1):
            launch(name, f"{ip}:{ports[names[(i - 1) // 2]]}", port=ports[name], **kw)
            until(lambda: listening(ports[name]), BOOT)  # a later node may bootstrap off this one
        ready("everyone is up", until(lambda: all(booted(x) for x in names), BOOT), lambda: str([log(x)[-200:] for x in names]))
        for name in names[1:]:
            resolve(name)
        ready("everyone knows everyone", until(lambda: all(set(names) - {x} <= known(x) for x in names)), lambda: str({x: sorted(known(x)) for x in names}))
        ready("and is paused, with every join over", quiet(names) and stopped(names), lambda: where(names))
        yield names
    finally:
        for name in [k for k in nodes if k.startswith(prefix + "-")]:
            stop(name)


@contextlib.contextmanager
def party(room, n=3, **kw):
    """A party of its own for one case: n nodes in a fresh room, the first its host, every
    one booted, connected, paused and quiet. A case starts from this, never from what the
    last case left. Name what it adds room + "-..." and that stops with it too."""
    names = [f"{room}-{chr(ord('a') + i)}" for i in range(n)]
    try:
        launch(names[0], room=room, **kw)
        ready(f"{names[0]} opened {room}", until(lambda: booted(names[0]) and connected(names[0]), BOOT), lambda: log(names[0])[-300:])
        for name in names[1:]:
            launch(name, room=room, **kw)
        ready("everyone is in", until(lambda: all(booted(x) and connected(x) for x in names), BOOT), lambda: str([log(x)[-200:] for x in names]))
        for name in names:
            resolve(name)
        ready("and paused, with every join over", quiet(names) and stopped(names), lambda: where(names))
        yield names
    finally:
        for name in [k for k in nodes if k.startswith(room + "-")]:
            stop(name)


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


def launch(name, addrs=None, room=None, secret=None, tag=None, relay=None, create_key=None, media=None, evict="1ms", local=True, mpv=None, mpvflags="", port=None, playlist=False, cache=None):
    """Start a node. room!=None selects relay mode; addrs is the p2p bootstrap. evict is
    -evictAfter, short by default so the strike cases need no 5-minute wait."""
    key = tag or name
    path = f"{RUN}/{key}.log"
    cmd = [BIN, "-file", media or MEDIA, "-hostname", name, "-mpvFlags", f"{MPVFLAGS} {mpvflags}"] + (["-mpv", mpv] if mpv else []) + (["-playlist"] if playlist else []) + (["-cacheDir", cache] if cache else [])
    if room is not None:
        cmd += ["-relay", relay or RELAY_URL, "-room", room, "-secret", secret or SECRET]
        if create_key is None:
            create_key = "" if room in hosted else CREATE_KEY
        hosted.add(room)
        if create_key:
            cmd += ["-createKey", create_key]
    else:
        cmd += ["-local"] if local else []
        cmd += ["-port", str(port or PORTS[name]), "-secret", secret or SECRET]
        if evict:
            cmd += ["-evictAfter", evict]
        if addrs:
            cmd += ["-addrs", addrs]
    p = subprocess.Popen(cmd, stdout=open(path, "wb"), stderr=subprocess.STDOUT)
    nodes[key] = {"proc": p, "log": path, "sock": None, "addr": None, "port": None if room is not None else port or PORTS[name]}
    return key


def lag(port, target, delay):
    """A TCP proxy on port that delivers every chunk to target delay seconds late, in
    order, both ways: a network with that much one-way latency. delay can instead be a
    function from a direction's name to a function giving each chunk's delay."""
    delays = delay if callable(delay) else lambda _: lambda: delay
    srv = socket.socket()
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", port))
    srv.listen()

    def pipe(src, dst, wait):
        q = queue.Queue()

        def read():
            while True:
                try:
                    b = src.recv(65536)
                except OSError:
                    b = b""
                q.put((time.time() + wait(), b))
                if not b:
                    return
        threading.Thread(target=read, daemon=True).start()
        while True:
            at, b = q.get()
            time.sleep(max(0, at - time.time()))
            try:
                if not b:
                    dst.shutdown(socket.SHUT_WR)
                    return
                dst.sendall(b)
            except OSError:
                return

    def accept():
        for n in itertools.count():
            try:
                c, _ = srv.accept()
            except OSError:
                return
            try:
                u = socket.create_connection(("127.0.0.1", target))
            except OSError:  # the target is down for now, as in [R13]
                c.close()
                continue
            threading.Thread(target=pipe, args=(c, u, delays(f"{n} up")), daemon=True).start()
            threading.Thread(target=pipe, args=(u, c, delays(f"{n} down")), daemon=True).start()
    threading.Thread(target=accept, daemon=True).start()
    return srv


def jitter(name):
    """One direction of one connection: some are slower than others, so messages between
    nodes arrive in a different order from one seed to the next."""
    rng = random.Random(f"{JITTERED}/{name}")
    worst = rng.uniform(0, JITTER)
    return lambda: rng.uniform(0, worst)


def relay_files(port):
    """A relay on another port than the phase's keeps its own log and database."""
    return f"{RUN}/relay" if port in (None, PORTS["relay"]) else f"{RUN}/relay-{port}"


def free_port():
    s = socket.create_server(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def launch_relay(extra=None, port=None):
    path = relay_files(port) + ".log"
    # every case opens a room of its own; the caps phase tests the limit on relays of its own
    env = {**os.environ, "ADDR": f"127.0.0.1:{port or PORTS['relay']}", "DB": relay_files(port) + ".db", "CREATE_KEY": CREATE_KEY, "ROOMS_PER_HOUR": "1000", **(extra or {})}
    # append: a restart in [R13] must not lose the first run's lines
    p = subprocess.Popen([RELAY_BIN], env=env, stdout=open(path, "ab"), stderr=subprocess.STDOUT)
    relays.append(p)
    return p


class Proxy:
    """A TCP hop in front of the relay. cut() drops the node's side and leaves the
    relay's side open and silent, which is how a NAT rebinding looks to the relay. While
    shut, it hangs up on every new connection, so a node stays out of the room."""

    def __init__(self, port):
        self.port, self.pairs, self.shut = port, [], False
        self.ls = socket.create_server(("127.0.0.1", 0))
        self.url = f"ws://127.0.0.1:{self.ls.getsockname()[1]}"
        threading.Thread(target=self.accept, daemon=True).start()

    def accept(self):
        while True:
            c, _ = self.ls.accept()
            if self.shut:
                c.close()
                continue
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

    def cut(self, relay=False):
        """With relay, the relay's side ends too, so the relay sees the drop first."""
        for pair in self.pairs:
            if pair["live"]:
                pair["live"] = False
                pair["c"].shutdown(socket.SHUT_RDWR)
                if relay:
                    pair["u"].shutdown(socket.SHUT_RDWR)


def relay_log(port=None):
    return open(relay_files(port) + ".log").read()


def relay_events(port=None):
    """One dict per finished line; a line that isn't JSON comes back as {"unparsed": line}."""
    events = []
    for line in relay_log(port).rpartition("\n")[0].split("\n"):
        try:
            events.append(json.loads(line))
        except ValueError:
            events.append({"unparsed": line})
    return events


def relay_event(msg, room, port=None):
    return next((e for e in relay_events(port) if e.get("msg") == msg and e.get("room") == room), None)


def resolve(name):
    n = nodes[name]
    n["sock"] = child_mpv_socket(n["proc"].pid)
    if not n["sock"]:
        sys.exit(f"{name} never spawned mpv; see {n['log']}")
    # not the first line: in relay mode Connect starts first and can log before it
    n["addr"] = log(name).split("share is ", 1)[1].split("\n", 1)[0].strip()
    if n["addr"].startswith("-addrs "):
        n["addr"] = n["addr"].split()[1]
    print(f"  {name} on {n['addr']}")


def stop(name, sig=signal.SIGTERM, timeout=20):
    n = nodes[name]
    if n["proc"].poll() is not None:
        return True
    n["sock"] = n["sock"] or child_mpv_socket(n["proc"].pid)  # a crash can come before resolve()
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


def stub(peer, of, addr, register=True, events=""):
    """A peer at addr that joins node of's party through peer, its address."""
    cmd = [sys.executable, __file__, "--stub", addr, peer]
    if not register:
        cmd.append("noregister")
    p = subprocess.Popen(cmd, stdout=subprocess.PIPE, text=True,
                         env={**os.environ, "E2E_FILE": fingerprint(of), "E2E_SECRET": SECRET, "E2E_STUB_EVENTS": events})
    stubs.append(p)
    # blocks until the stub is listening and registered; a stale stub left on the
    # port would answer for it and the strike loops in [13] would never end
    if p.stdout.readline() != "ready\n":
        sys.exit(f"the stub peer could not start on {addr}; is the port taken?")
    return p


def stub_stop(p):
    p.send_signal(signal.SIGINT)
    p.wait(timeout=10)
    p.stdout.close()


def gone(pid):
    try:
        os.kill(pid, 0)
        return open(f"/proc/{pid}/stat").read().split(")")[-1].split()[0] == "Z"
    except OSError:
        return True


def children(pid):
    out = []
    for d in glob.glob("/proc/[0-9]*"):
        try:
            with open(d + "/status") as f:
                if f"PPid:\t{pid}\n" in f.read():
                    out.append(int(os.path.basename(d)))
        except OSError:
            pass
    return out


def launch_gui(name, tag=None, exe=None, browser="headless", portal=True):
    """A real GUI in its own HOME, XDG folders and TMPDIR, with a PATH of only mpv and
    an xdg-open that fails. browser="headless" gives it one browser, a desktop entry
    for a wrapper that runs headless Chromium, so the GUI's own launch path opens the
    page and nothing reaches the screen; "xvfb" runs it headed on a virtual display;
    "handoff" is a browser that exits at once, None no browser. Every GUI talks to the private bus, never the desktop's: godbus
    finds /run/user/<uid>/bus on its own and would open a real file chooser."""
    home, key = f"{RUN}/{name}", tag or name
    if not os.path.exists(home):
        for d in ("bin", "config", "data/applications", "tmp", "downloads"):
            os.makedirs(f"{home}/{d}")
        os.symlink(shutil.which("mpv"), f"{home}/bin/mpv")
        script(f"{home}/bin/xdg-open", f'echo "$@" >> {home}/xdg-open.args\nexit 1')
        script(f"{home}/slow-mpv", f'{shutil.which("sleep")} 3\nexec {shutil.which("mpv")} "$@"')
        if browser:
            # e2e.Dockerfile sets it: Docker's seccomp profile blocks the user namespaces the sandbox needs
            flags = "--headless=new --remote-debugging-port=0" + (" --no-sandbox" if os.environ.get("E2E_NO_SANDBOX") else "")
            if browser == "xvfb":
                flags = flags.replace("--headless=new", "--ozone-platform=x11")
            run = "exit 0" if browser == "handoff" else f'PATH=/usr/bin:/bin exec {"xvfb-run -a " if browser == "xvfb" else ""}{BROWSER} {flags} "$@"'
            script(f"{home}/browser.sh", f'echo "$@" >> {home}/browser.args\necho "$GOOGLE_API_KEY" > {home}/browser.key\n{run}')
            with open(f"{home}/data/applications/e2e-chromium.desktop", "w") as f:
                f.write(f"[Desktop Entry]\nType=Application\nName=e2e\nExec={home}/browser.sh %U\nMimeType=x-scheme-handler/https;\n")
            with open(f"{home}/config/mimeapps.list", "w") as f:
                f.write("[Default Applications]\nx-scheme-handler/https=e2e-chromium.desktop\n")
    env = {"HOME": home, "PATH": f"{home}/bin", "TMPDIR": f"{home}/tmp", "XDG_CONFIG_HOME": f"{home}/config",
           "XDG_DATA_HOME": f"{home}/data", "XDG_DATA_DIRS": f"{home}/data",
           "DBUS_SESSION_BUS_ADDRESS": BUS if portal else f"unix:path={home}/no-bus"}
    path = f"{RUN}/{key}.log"
    nodes[key] = {"home": home}
    opened = len(launches(key))
    # its own process group, so cleanup takes its browser, its mpv and any GUI it restarts into
    p = subprocess.Popen([exe or GUI_BIN], env=env, stdout=open(path, "wb"), stderr=subprocess.STDOUT, start_new_session=True)
    nodes[key] = {"proc": p, "log": path, "sock": None, "addr": None, "home": home, "gui": True}
    until(lambda: "gui: http" in log(key), BOOT)
    nodes[key]["url"] = log(key).split("gui: ", 1)[1].split("\n", 1)[0].strip()
    if browser in ("headless", "xvfb"):
        attach(key, opened + 1)
    return key


def script(path, body):
    with open(path, "w") as f:
        f.write("#!/bin/sh\n" + body + "\n")
    os.chmod(path, 0o755)


def profile(n):
    """The temporary browser profile of the last window this GUI's folder opened."""
    return next(a.split("=", 1)[1] for a in launches(n)[-1].split() if a.startswith("--user-data-dir="))


def launches(n):
    try:
        return open(f"{nodes[n]['home']}/browser.args").read().splitlines()
    except OSError:
        return []


def attach(n, count):
    """Connect to the page of the count-th window this GUI's folder has opened."""
    def ws():
        lines = launches(n)
        return find_page(lines[count - 1]) if len(lines) >= count else None
    until(ws, BOOT)
    url = ws()
    if not url:
        sys.exit(f"{n} never opened its page; see {nodes[n]['log']}")
    nodes[n]["page"] = Page(url)
    nodes[n]["page"].ready(BOOT, downloads=f"{nodes[n]['home']}/downloads", jitter=(random.Random(f"{JITTERED}/{n}/{count}").getrandbits(32), JITTER) if JITTERED else None)


def js(n, expr):
    return nodes[n]["page"].js(expr)


def feed(n):
    return nodes[n]["page"].feed()


def status(n):
    return nodes[n]["page"].status()


def click(n, selector):
    nodes[n]["page"].click(selector)


def fill(n, **fields):
    nodes[n]["page"].fill(**fields)


def tab(n, view):
    nodes[n]["page"].tab(view)


def state(n):
    return nodes[n]["page"].state()


def go(n):
    nodes[n]["page"].go(BOOT)


def start(n, **fields):
    fill(n, **fields)
    go(n)


def end(n):
    js(n, "window.asked = null")
    click(n, "#go")
    until(lambda: status(n) == "The party has ended.")
    # seeing the party stop, the page reloads the saved settings without waiting; landing
    # after the next case's fill, it put the saved mpv options back ([G19e] under --jitter)
    until(lambda: js(n, "window.loading") == 0)
    return js(n, "window.asked")


def save_settings(n, **fields):
    tab(n, "settings")
    fill(n, **fields)
    js(n, '$("settings-saved").hidden = true')
    click(n, "#settings-form button[type=submit]")
    until(lambda: js(n, '!$("settings-saved").hidden'))
    tab(n, "party")
    return js(n, '$("settings-saved").textContent')


def private_bus():
    """A dbus-daemon of our own, with no service directories so nothing real can be
    activated on it, and the fake portal on it."""
    path = f"{RUN}/bus"
    with open(f"{RUN}/bus.conf", "w") as f:
        f.write(f'<busconfig><type>session</type><listen>unix:path={path}</listen><auth>EXTERNAL</auth>'
                '<policy context="default"><allow send_destination="*" eavesdrop="true"/><allow eavesdrop="true"/>'
                '<allow own="*"/></policy></busconfig>')
    relays.append(subprocess.Popen(["dbus-daemon", f"--config-file={RUN}/bus.conf", "--nofork"]))
    until(lambda: os.path.exists(path))
    p = subprocess.Popen([PORTAL_BIN, MEDIA], env={"DBUS_SESSION_BUS_ADDRESS": f"unix:path={path}"}, stdout=subprocess.PIPE, text=True)
    relays.append(p)
    if p.stdout.readline() != "ready\n":
        sys.exit("the fake portal could not start")
    return f"unix:path={path}"


marks = [("", time.time())]


def mark(name):
    marks.append((name, time.time()))


cases = []    # (id, start) of each story header, timed until the next one starts
took = []     # (seconds, id) of each case that ran on its own
pending = []  # (title, function, serial) of each @case, run by run_cases()
running = threading.local()  # the case this thread runs, and the buffer its prints go to


class CaseOutput:
    """Each case's prints go to a buffer of its own while cases run side by side, and come
    out whole, so two cases' lines never interleave."""

    def __init__(self, real):
        self.real, self.lock = real, threading.Lock()

    def write(self, text):
        buf = getattr(running, "buf", None)
        if buf is None:
            with self.lock:
                self.real.write(text)
        else:
            buf.append(text)

    def flush(self):
        self.real.flush()


sys.stdout = CaseOutput(sys.stdout)


def header(title):
    """A case in a phase still written as one story: says what it checks, and times it
    until the next one starts."""
    cases.append((title.split()[0], time.time()))
    print("\n" + title)


def part(title):
    """A labelled step inside a case: its failures carry its own id."""
    running.id = title.split()[0]
    print("\n" + title)


def case(title, serial=False):
    """Adds the function below as a case, which run_cases() runs. It sets up what it checks
    from a party() of its own, so cases run side by side; serial is for one that touches
    what all share, like the relay itself. A must() that fails or a crash ends it alone."""
    def add(f):
        pending.append((title, f, serial))
        return f
    return add


def run_case(title, f):
    running.id, running.buf = title.split()[0], []
    start = time.time()
    print("\n" + title)
    try:
        f()
    except Stop as e:
        print(f"  STOP  the rest of this case depends on: {e}")
    except Exception:
        traceback.print_exc(file=sys.stdout)
        fails.append(f"{title.split()[0]} crashed")
    took.append((time.time() - start, title.split()[0]))
    out, running.buf, running.id = "".join(running.buf), None, None
    print(out, end="")


def run_cases(at_once=CASES_AT_ONCE):
    """Runs every @case so far, at_once at a time, then the serial ones alone."""
    side, alone = [c for c in pending if not c[2]], [c for c in pending if c[2]]
    pending.clear()
    threads, free = [], threading.Semaphore(at_once)

    def one(title, f):
        try:
            run_case(title, f)
        finally:
            free.release()
    for title, f, _ in side:
        free.acquire()
        threads.append(threading.Thread(target=one, args=(title, f)))
        threads[-1].start()
    for t in threads:
        t.join()
    for title, f, _ in alone:
        run_case(title, f)


def slowest(end, n=5):
    told = sorted([(t2 - t1, c) for (c, t1), (_, t2) in zip(cases, cases[1:] + [("", end)])] + took, reverse=True)
    return ", ".join(f"{c} {t:.0f}s" for t, c in told[:n])


need("go")
need("mpv")
print(f"run directory: {RUN}")
GUI = args.mode in ("gui", "upnp")
BINS = args.bin or RUN
if not args.bin:
    print("building")
    build(RUN, GUI)
mark("build")
BIN = BINS + "/watchparty"
RELAY_BIN = BINS + "/relay"
FAKESEED_BIN = BINS + "/fakeseed"
GUI_BIN = BINS + "/watchparty-gui"
PORTAL_BIN = BINS + "/fakeportal"
if GUI:
    BROWSER = next(filter(None, map(shutil.which, ("google-chrome", "chromium", "google-chrome-stable"))), None)
    if not BROWSER:
        sys.exit("the gui phase needs Chrome or Chromium on PATH")
    need("dbus-daemon")
    need("xvfb-run")
if args.mode == "gui":
    # the update cases' releases are on this phase's ports, so only it can build these
    print("building two stamped versions of the gui and the cli")
    release = f"-X github.com/mhashemm/watchparty/update.ReleaseURL=http://127.0.0.1:{{}}/latest"
    builds = [(RUN + f"/gui-{v}", f"-X main.version={v} {release.format(PORTS['release'])}", "./gui") for v in ("v0.0.1", "v0.0.2")]
    builds += [(RUN + f"/cli-{v}", f"-X main.version={v} {release.format(PORTS['cliRelease'])}", ".") for v in ("v0.0.1", "v0.0.2")]
    for p in [subprocess.Popen(["go", "build", "-ldflags", flags, "-o", out, pkg], cwd=REPO) for out, flags, pkg in builds]:
        if p.wait():
            sys.exit("a stamped build failed")
    mark("gui build")

if args.file:
    MEDIA = os.path.abspath(args.file)
else:
    need("ffmpeg")
    print("generating a 120s test file")
    MEDIA = RUN + "/test.mkv"
    subprocess.run(["ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10",
                    "-f", "lavfi", "-i", "sine=frequency=440", "-t", "120", "-c:v", "libx264",
                    "-pix_fmt", "yuv420p", "-c:a", "aac", MEDIA], check=True)
    mark("video")

def seeder(path):
    """Seeds path until the phase ends, and returns its magnet link."""
    p = subprocess.Popen([FAKESEED_BIN, path], stdout=subprocess.PIPE, text=True)
    nodes[f"seed{len(nodes)}"] = {"proc": p, "log": os.devnull, "sock": None, "addr": None}
    return p.stdout.readline().strip()


NOT_THE_SAME_VIDEO = ("Diagnosis: your video is not the same file as the party's. Pick the same video and start again. "
                      "If the party is watching a whole folder, tick the box under File.")
NOT_THE_SAME_FOLDER = "Diagnosis: this folder doesn't match the party's. Everyone needs the same videos in the same order, with the box ticked."


def videos_in(path, names):
    """Three short, different entries in a folder: names[i] holds the i-th picture."""
    os.makedirs(path)
    for name, picture in zip(names, ("testsrc", "testsrc2", "smptebars")):
        target = f"{RUN}/{picture}.mkv"
        if not os.path.exists(target):
            subprocess.run(["ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", f"{picture}=size=160x120:rate=10", "-t", "12",
                            "-c:v", "libx264", "-pix_fmt", "yuv420p", target], check=True)
        shutil.copy(target, f"{path}/{name}")
    return path


COUPLED = re.compile(r"season|episode|movie|\btv\b", re.I)


def p2p_suite():
    print("\n=== p2p: the direct mesh, and a mesh of its own for every case ===")

    def ip_of(n):
        return nodes[n]["addr"].rsplit(":", 1)[0]

    @case(f"[1] a mesh closes transitively: every node learns all {len(NODES) - 1} others, and starts paused")
    def _():
        with mesh("p1", len(NODES)) as P:
            chain = ", ".join(f"{P[i]}->{P[(i - 1) // 2]}" for i in range(1, len(P)))
            print(f"  bootstrap chain {chain}")
            for n in P:
                missing = set(P) - {n} - known(n)
                ok(f"{n} knows the whole mesh", not missing, f"missing {sorted(missing)}" if missing else "")
                ok(f"{n} advertised its own address", nodes[n]["addr"].endswith(f":{nodes[n]['port']}"), nodes[n]["addr"])
            ok("nobody connected to itself", not any(n in known(n) for n in P))
            ok("everyone is paused", stopped(P), where(P))

    @case("[2a] -h explains the app, one topic at a time, then lists every flag")
    def _():
        h = subprocess.run([BIN, "-h"], capture_output=True, text=True)
        text = h.stdout + h.stderr
        ok("it exits 0", h.returncode == 0, h.returncode)
        for section in ("Start a party", "Join a party", "Watch together", "Automatic pauses", "Links", "Playlists", "Torrents", "Relays", "Flags"):
            ok(f"it has {section}", re.search(rf"^{section}$", text, re.M), text[:200])
        ok("it lists the flags", "-playlist" in text and "-createKey" in text)
        ok("it isn't tied to one kind of video", not COUPLED.search(text), str(COUPLED.findall(text)))

    @case("[2b] the sync-critical mpv options win over the user's own flags")
    def _():
        with mesh("p2b", 1) as P:
            n = P[0]
            ok(f"{n} plays at normal speed", get(n, "speed") == 1, f"speed={get(n, 'speed')}")
            ok(f"{n} seeks precisely", get(n, "hr-seek") in (True, "yes"), f"hr-seek={get(n, 'hr-seek')}")
            ok(f"{n} ignores saved positions", get(n, "resume-playback") is False)
            ok(f"{n} waits for its cache", get(n, "cache-pause") is True)

    @case("[3] a resume reaches everyone over the mesh, and says who")
    def _():
        with mesh("p3") as P:
            a = P[0]
            setp(a, "pause", False)
            ok("everyone plays", until(lambda: playing(P)), where(P))
            for n in P[1:]:
                ok(f"{n} credited {a}", until(lambda: f"resumed by {a}" in osd(n)), str(osd(n)))

    @case("[4] a pause on the last to join reaches the first (no central host)")
    def _():
        with mesh("p4") as P:
            a, c = P[0], P[-1]
            setp(a, "pause", False)
            must("everyone plays", until(lambda: playing(P)), where(P))
            must("the resume is over", quiet(P))
            setp(c, "pause", True)
            ok("everyone stops", until(lambda: stopped(P)), where(P))
            ok(f"{a} credited {c}", until(lambda: f"paused by {c}" in osd(a)), str(osd(a)))

    @case("[5] a seek while paused takes everyone there, and says who")
    def _():
        with mesh("p5") as P:
            a, c = P[0], P[-1]
            setp(c, "time-pos", 77.0)
            ok("everyone is at 77s", until(lambda: in_step(P, 77.0)), where(P))
            ok(f"{a} credited {c} for the sync", until(lambda: f"synced by {c}" in osd(a)), str(osd(a)))

    @case("[5b] any paused member can seek for everyone, not just whoever drove the pause")
    def _():
        with mesh("p5b") as P:
            a, b, c = P
            setp(a, "pause", False)
            must("everyone plays", until(lambda: playing(P)), where(P))
            setp(c, "pause", True)
            must(f"{c}'s pause stopped everyone", until(lambda: stopped(P)) and quiet(P), where(P))
            setp(b, "time-pos", 12.0)
            ok(f"everyone followed {b}'s seek", until(lambda: in_step(P, 12.0)), where(P))

    @case("[5d] an arrow-key (relative) seek lands exactly, and goes out once")
    def _():
        with mesh("p5d") as P:
            b = P[1]
            setp(b, "time-pos", 12.0)
            must("everyone is at 12s", until(lambda: in_step(P, 12.0)) and quiet(P), where(P))
            sent = lambda: sum("broadcasted event" in l and '"time-pos"' in l for l in log(b).splitlines())
            before = sent()
            ipc(nodes[b]["sock"], "seek", 7, "relative")
            ok("everyone is at exactly 19s, not a keyframe", until(lambda: in_step(P) and near(get(b, "time-pos"), 19.0, 0.15)), where(P))
            time.sleep(QUIET)  # a second broadcast would come right behind the first
            ok(f"{b} broadcast one position for one seek", sent() - before == 1, f"{sent() - before} sent")

    @case("[5c] an applied event is not echoed back (the a->b->a rebound)")
    def _():
        with mesh("p5c") as P:
            a = P[0]
            setp(a, "pause", False)
            must("everyone plays", until(lambda: playing(P)), where(P))
            before = {n: pauses(n) for n in P}
            setp(a, "pause", True)
            until(lambda: stopped(P) and all(pauses(n) > before[n] for n in P[1:]))
            time.sleep(QUIET)  # an echo would be right behind the pause
            ok("everyone stopped", stopped(P), where(P))
            # if any peer rebroadcast the echo, the originator would be told it was paused
            # by someone else and the rest would see the notice twice
            ok(f"{a} was not told its own pause came back", pauses(a) == before[a],
               f"{pauses(a) - before[a]} echoes returned to the originator")
            for n in P[1:]:
                got = pauses(n) - before[n]
                ok(f"{n} applied {a}'s pause exactly once", got == 1, f"{got} pause notices")

    @case("[6] positions are not sent while playing")
    def _():
        with mesh("p6") as P:
            a = P[0]
            setp(a, "pause", False)
            must("everyone plays", until(lambda: playing(P)), where(P))
            before = {n: osd(n).count(f"synced by {a}") for n in P[1:]}
            time.sleep(4)  # mpv reports time-pos ~10 times a second while playing
            for n in P[1:]:
                after = osd(n).count(f"synced by {a}")
                ok(f"no time-pos storm reached {n}", after == before[n], f"{after - before[n]} sync notices in 4s")

    @case("[7] a member that only ever followed can drive the party")
    def _():
        with mesh("p7") as P:
            a, b = P[0], P[1]
            setp(a, "pause", False)
            must(f"{a} started it", until(lambda: playing(P)) and quiet(P), where(P))
            setp(b, "pause", True)
            ok(f"{b}'s pause reached everyone", until(lambda: stopped(P)), where(P))
            must("the pause is over", quiet(P))
            setp(b, "pause", False)
            ok(f"{b}'s resume reached everyone", until(lambda: playing(P)), where(P))

    @case("[8] repeated round trips are not swallowed by the counter check")
    def _():
        with mesh("p8") as P:
            a = P[0]
            rounds = 0
            for i in range(5):
                setp(a, "pause", False)
                mid = until(lambda: playing(P[1:]), 5)
                setp(a, "pause", True)
                end = until(lambda: stopped(P[1:]), 5)
                rounds += mid and end
            ok(f"5/5 round trips landed on all {len(P) - 1} peers", rounds == 5, f"{rounds}/5")
            for n in P[1:]:
                ok(f"{n} skipped no events", "skipped event from" not in log(n))

    @case("[9] /event and /bye from an unknown address are rejected")
    def _():
        with mesh("p9", 1) as P:
            a = P[0]
            code = raw(nodes[a]["addr"], "/event", {"hit-me-up": f"{ip_of(a)}:9999", "counter": "5", "hostname": "stranger", "secret": SECRET},
                       b'{"event":"property-change","name":"pause","data":"yes"}')
            ok(f"{a} rejects an unknown peer's event", code == 400, f"HTTP {code}")
            code = raw(nodes[a]["addr"], "/bye", {"hit-me-up": f"{ip_of(a)}:9999", "counter": "5", "hostname": "stranger", "secret": SECRET})
            ok(f"{a} rejects an unknown peer's bye", code == 400, f"HTTP {code}")

    @case("[9b] a client that never finishes its headers is dropped, and huge headers are refused")
    def _():
        with mesh("p9b", 1) as P:
            host, port = nodes[P[0]]["addr"].rsplit(":", 1)
            with socket.create_connection((host, int(port)), timeout=SETTLE) as s:
                s.sendall(b"POST /hi HTTP/1.1\r\nHost: x\r\nsecret:")
                try:
                    s.recv(1024)
                    ok("the server hung up on it", True)
                except TimeoutError:
                    ok("the server hung up on it", False, f"still open after {SETTLE}s")
            with socket.create_connection((host, int(port)), timeout=SETTLE) as s:
                s.sendall(b"POST /hi HTTP/1.1\r\nHost: x\r\nsecret: " + b"a" * 20000 + b"\r\n\r\n")
                got = s.recv(64)
                ok("20KB of headers gets a 431", got.startswith(b"HTTP/1.1 431"), str(got))

    @case("[10b] without the secret, a peer's own address forges nothing")
    def _():
        with mesh("p10b") as P:
            a, b, c = P
            pause = b'{"event":"property-change","name":"pause","data":"yes"}'
            for secret in (None, "wrongsecret"):
                h = {"hit-me-up": nodes[b]["addr"], "counter": "999999", "hostname": b, "file": fingerprint(a)}
                if secret:
                    h["secret"] = secret
                for endpoint in ("/hi", "/event", "/bye"):
                    code = raw(nodes[a]["addr"], endpoint, h, pause if endpoint == "/event" else b"")
                    ok(f"{a} refuses {endpoint} with {'no' if secret is None else 'a wrong'} secret", code == 403, f"HTTP {code}")
            code = raw(nodes[a]["addr"], "/event", {"hit-me-up": nodes[b]["addr"], "counter": "999999", "hostname": b,
                                                   "secret": SECRET}, b"x" * ((64 << 10) + 1))
            ok(f"{a} refuses an oversized event", code == 413, f"HTTP {code}")
            setp(c, "pause", False)
            ok(f"{b} is still in the mesh: the forged /bye was refused", until(lambda: get(b, "pause") is False), where(P))

    @case("[11] /bye with a stale counter cannot evict a live peer")
    def _():
        with mesh("p11") as P:
            a, b, c = P
            code = raw(nodes[a]["addr"], "/bye", {"hit-me-up": nodes[b]["addr"], "counter": "1", "hostname": b, "secret": SECRET})
            ok(f"{a} skipped the stale bye", code == 204, f"HTTP {code}")
            ok(f"{a} logged it as skipped", until(lambda: "skipped bye from" in log(a), 5))
            setp(c, "pause", False)
            ok(f"{b} is still in the mesh: it still hears events", until(lambda: get(b, "pause") is False), where(P))

    @case("[11b] a joiner playing a different file is refused; the same file renamed is not; a wrong secret or nobody to join ends it")
    def _():
        with mesh("p11b", 2) as P:
            a = P[0]
            same, diff = variants()
            twin, other, bad, alone = "p11b-same", "p11b-diff", "p11b-bad", "p11b-alone"
            launch(twin, nodes[a]["addr"], media=same, port=free_port())
            ok("the renamed, moved, re-dated copy is let in", until(lambda: booted(twin) and all(f"{twin} has joined" in osd(n) for n in P), BOOT), str([osd(n) for n in P]))
            resolve(twin)
            ok("it got the same fingerprint", fingerprint(twin) == fingerprint(a))
            launch(other, nodes[a]["addr"], media=diff, port=free_port())
            until(lambda: nodes[other]["proc"].poll() is not None, BOOT)
            nodes[other]["sock"] = child_mpv_socket(nodes[other]["proc"].pid)  # only if it wrongly got in
            ok("the different file's node exited with an error", nodes[other]["proc"].poll() not in (None, 0),
               f"rc={nodes[other]['proc'].returncode}")
            ok("it was told why", "not the same file as the party's" in log(other))
            ok(f"{a} logged the refusal", f"refused {other}" in log(a))
            ok("nobody saw it join", not any(other in known(n) for n in P))
            launch(bad, nodes[a]["addr"], secret="wrongsecret", port=free_port())
            until(lambda: nodes[bad]["proc"].poll() is not None, BOOT)
            ok("a joiner with the wrong secret exited with an error", nodes[bad]["proc"].poll() not in (None, 0),
               f"rc={nodes[bad]['proc'].returncode}")
            ok("it was told why", "the party refused your secret" in log(bad))
            ok("nobody saw it join", not any(bad in known(n) for n in P))
            launch(alone, f"{ip_of(a)}:{free_port()}", port=free_port())
            until(lambda: nodes[alone]["proc"].poll() is not None, BOOT)
            nodes[alone]["sock"] = child_mpv_socket(nodes[alone]["proc"].pid)  # only if it wrongly started
            ok("a joiner nobody answered exited with an error instead of playing alone", nodes[alone]["proc"].poll() not in (None, 0) and "init done" not in log(alone),
               f"rc={nodes[alone]['proc'].returncode}")
            ok("it was told why", "nobody answered at the address you were given" in log(alone), log(alone)[-300:])

    @case("[11c] a joiner while the party plays pauses it once loaded, and starts where it stopped")
    def _():
        with mesh("p11c") as P:
            a = P[0]
            setp(a, "time-pos", 30.0)  # so "where it stopped" can't be the start
            must("everyone is at 30s", until(lambda: in_step(P, 30.0)), where(P))
            setp(a, "pause", False)
            must("everyone plays", until(lambda: playing(P)), where(P))
            mid = "p11c-mid"
            launch(mid, nodes[a]["addr"], port=free_port())
            must("the joiner is up", until(lambda: booted(mid), BOOT), log(mid)[-300:])
            resolve(mid)
            for n in P:
                ok(f"{n} told why", until(lambda: f"paused for {mid}: joining" in osd(n)), str(osd(n)))
            ok("everyone stopped in step with it, past 30s", until(lambda: in_step(P + [mid], tol=0.3)) and get(a, "time-pos") > 30, where(P + [mid]))

    @case("[12] SIGKILL a member: every survivor evicts it after 3 strikes and stays in step")
    def _():
        with mesh("p12", 4) as P:
            a, last = P[0], P[-1]
            dead = nodes[last]["addr"]
            stop(last, signal.SIGKILL)
            survivors = P[:-1]
            t0 = time.time()
            for n in survivors:
                # only the node that generates an event broadcasts, so each survivor has
                # to drive a few of its own before it can notice the dead one is gone
                tries = 0
                while strikes(n, dead, last) < 3 and tries < 12:
                    toggle(n)
                    tries += 1
            elapsed = time.time() - t0
            for n in survivors:
                ok(f"{n} evicted {last}", until(lambda: dropped(n, dead)), f"{strikes(n, dead, last)} strikes")
            ok("no survivor stalled on the dead peer", elapsed < 90, f"{elapsed:.1f}s for {len(survivors)} nodes")
            must("the toggles are over", quiet(survivors))
            setp(a, "pause", not get(a, "pause"))
            want = get(a, "pause")
            ok(f"{a} still drives everyone after the eviction", until(lambda: all(get(n, "pause") is want for n in survivors)), where(survivors))

    @case("[13] one success resets the strike count (a flaky peer must not be evicted)")
    def _():
        with mesh("p13", 1) as P:
            a = P[0]
            at = f"{ip_of(a)}:{free_port()}"
            strikes_now = lambda: strikes(a, at, STUB_HOST)

            def toggle_until(cond, most=8):
                """a broadcast is the only way a strike happens; bounded, or a peer evicted too
                early would stop the strikes and hang the case"""
                for _ in range(most):
                    if cond():
                        return True
                    toggle(a)
                return cond()
            s1 = stub(nodes[a]["addr"], a, at)
            ok("the stub peer joined the mesh", until(lambda: f"{STUB_HOST} has joined" in osd(a), 5))
            stub_stop(s1)
            base = strikes_now()
            ok("2 strikes: still in the mesh", toggle_until(lambda: strikes_now() >= base + 2) and not dropped(a, at), f"{strikes_now() - base} failures logged")
            # back at the same address WITHOUT re-registering, so a keeps the same peer entry
            # (fails=2) rather than replacing it through /hi: only a success can clear the strikes
            s2 = stub(nodes[a]["addr"], a, at, register=False)
            toggle(a)
            ok("no new strike while the peer is healthy", strikes_now() == base + 2, "the broadcast succeeded")
            stub_stop(s2)
            base2 = strikes_now()
            ok("2 more strikes after the success: STILL in the mesh", toggle_until(lambda: strikes_now() >= base2 + 2) and not dropped(a, at),
               f"{strikes_now()} total failures vs maxFails=3, only a reset explains this")
            ok("a 3rd strike in a row evicts", toggle_until(lambda: strikes_now() >= base2 + 3) and until(lambda: dropped(a, at), 5))

    @case("[13c] a peer that missed a pause and a resume is sent the state again once it is back; a newer resume reaching a node that plays stops the party")
    def _():
        with mesh("p13c", 1) as P:
            a = P[0]
            at = f"{ip_of(a)}:{free_port()}"
            heard = f"{RUN}/p13c-stub-first.log"
            s4 = stub(nodes[a]["addr"], a, at, events=heard)
            must("the stub peer joined", until(lambda: f"{STUB_HOST} has joined" in osd(a), 5))
            setp(a, "pause", False)
            # gone before the resume reached it, the stub would cost a strike of its own
            must(f"{a} plays, and the stub heard it", until(lambda: get(a, "pause") is False and os.path.exists(heard)
                 and any(json.loads(l).get("paused") is False for l in open(heard).read().splitlines())))
            stub_stop(s4)
            base = strikes(a, at, STUB_HOST)
            toggle(a)
            toggle(a)
            ok("both were missed", strikes(a, at, STUB_HOST) == base + 2 and get(a, "pause") is False,
               f"{strikes(a, at, STUB_HOST) - base} failures logged")
            # back before the chase's next try, 2s after the last send
            events = f"{RUN}/p13c-stub-events.log"
            s5 = stub(nodes[a]["addr"], a, at, register=False, events=events)
            got = lambda: [json.loads(l) for l in open(events).read().splitlines()] if os.path.exists(events) else []
            ok("it was sent the state again, playing", until(lambda: any(e.get("again") and e.get("paused") is False for e in got()), 10),
               f"{got()}")
            ok(f"{a} stopped nobody for it: the state lands where it is behind", get(a, "pause") is False and
               not any("out of sync" in t for t in osd(a)), osd(a)[-3:])
            # the stub plays out of step now, and resumes: newer than anything a holds
            resume = {"stamp": 1 << 20, "paused": False, "at": max(get(a, "time-pos") - 10, 0), "event": "property-change",
                      "name": "pause", "data": "no"}
            code = raw(nodes[a]["addr"], "/event", {"hit-me-up": at, "counter": "999999", "hostname": STUB_HOST, "secret": SECRET},
                       json.dumps(resume).encode())
            ok(f"a newer resume reaching {a} while it plays stops the party",
               code == 204 and until(lambda: f"paused: out of sync with {STUB_HOST}" in osd(a) and get(a, "pause") is True, 5),
               f"HTTP {code}, {osd(a)[-3:]}")
            stub_stop(s5)
            drops = lambda: sum(s.startswith("dropped " + at) for s in osd(a))
            before, tries = drops(), 0
            while drops() == before and tries < 8:
                toggle(a)
                tries += 1
            ok("and is dropped once gone", drops() > before)

    @case("[13b] with the default -evictAfter, a burst of strikes does not evict a peer")
    def _():
        with mesh("p13b", 1, evict=None) as P:
            a = P[0]
            at = f"{ip_of(a)}:{free_port()}"
            s3 = stub(nodes[a]["addr"], a, at)
            must("the stub peer joined", until(lambda: f"{STUB_HOST} has joined" in osd(a), 5))
            stub_stop(s3)
            tries = 0  # an evicted peer is never sent to again, so its strikes stop at 3
            while strikes(a, at, STUB_HOST) < 4 and tries < 8:
                toggle(a)
                tries += 1
            ok("4 strikes within seconds: still in the mesh",
               strikes(a, at, STUB_HOST) >= 4 and not dropped(a, at),
               f"{strikes(a, at, STUB_HOST)} failures logged, evictAfter is 5m")

    @case("[13d] a member whose process stalls past the send timeout misses a resume; once it runs, the party ends in step")
    def _():
        with mesh("p13d") as P:
            X, Y = P[-1], P[1]
            base = strikes(Y, nodes[X]["addr"], X)
            os.kill(nodes[X]["proc"].pid, signal.SIGSTOP)
            try:
                setp(Y, "pause", False)
                stalled = until(lambda: strikes(Y, nodes[X]["addr"], X) > base, 15)
            finally:
                os.kill(nodes[X]["proc"].pid, signal.SIGCONT)
            ok(f"{Y}'s resume timed out on {X}", stalled, log(Y)[-300:])
            quiet(P, 30)
            end = {n: (get(n, "pause"), get(n, "time-pos")) for n in P}
            spread = max(t for _, t in end.values()) - min(t for _, t in end.values())
            states = {p for p, _ in end.values()}
            ok("the party ends in step", len(states) == 1 and spread < (0.5 if True in states else 1.5),
               f"{end}; {X} saw {osd(X)[-3:]}, {Y} saw {osd(Y)[-3:]}")

    @case("[14] clean shutdown: SIGTERM tells every survivor with /bye, which pauses the party")
    def _():
        with mesh("p14") as P:
            a, survivors = P[0], P[1:]
            setp(a, "pause", False)
            must("the party is playing", until(lambda: playing(P)), where(P))
            ok(f"{a} exited cleanly", stop(a) and nodes[a]["proc"].returncode == 0, f"rc={nodes[a]['proc'].returncode}")
            for n in survivors:
                ok(f"{n} was told {a} left", until(lambda: f"{a} has left" in osd(n)),
                   "Shutdown must not derive its timeout from the cancelled signal context")
                ok(f"{n} did not error on {a}'s departure", "does not exists" not in log(n))
            ok("everyone left stopped", until(lambda: stopped(survivors)), where(survivors))
            ok("someone says why", any(f"paused for {a}: left" in osd(n) for n in survivors), "another's pause can reach a node before its notice, so not everyone does")

            part("[15] the rest of the mesh keeps working without the node that bootstrapped it")
            must("the leave is over", quiet(survivors))
            setp(survivors[0], "pause", False)
            ok("a resume still reaches everyone", until(lambda: playing(survivors)), where(survivors))
            for n in survivors:
                ok(f"{n} exited cleanly", stop(n) and nodes[n]["proc"].returncode == 0)

    @case("[16] a file's folder plays as a playlist over the mesh too")
    def _():
        folder = videos_in(f"{RUN}/p2pfolder", ("e1.mkv", "e2.mkv", "e3.mkv"))
        with mesh("p16", 2, media=f"{folder}/e1.mkv", playlist=True) as P:
            host, guest = P
            ok("whoever started the party answered the joiner", "paused for p16-b: joining" in osd(host), osd(host))
            ipc(nodes[host]["sock"], "playlist-next")
            ok("the other node follows", until(lambda: get(guest, "filename") == "e2.mkv"), get(guest, "filename"))
            ok("the invite asks for the playlist", re.search(r"share is -addrs \S+ -secret \S+ -playlist\n", log(host)), log(host)[:300])

    run_cases()


def relay_suite():
    print("\n=== relay: one websocket server, and a party in a room of its own for every case ===")
    relays_now = [launch_relay()]
    until(lambda: listening(PORTS["relay"]))

    @case(f"[R1] {len(NODES)} nodes learn each other through the relay, and start paused")
    def _():
        with party("r1", len(NODES)) as P:
            for n in P:
                missing = set(P) - {n} - known(n)
                ok(f"{n} knows the room", not missing, f"missing {sorted(missing)}" if missing else "")
            ok("nobody was told it joined itself", not any(n in known(n) for n in P))
            ok("everyone is paused", stopped(P), where(P))

    @case("[R3] a resume on one node reaches everyone, and says who")
    def _():
        with party("r3") as P:
            a = P[0]
            setp(a, "pause", False)
            ok("everyone plays", until(lambda: playing(P)), where(P))
            for n in P[1:]:
                ok(f"{n} credited {a}", until(lambda: f"resumed by {a}" in osd(n)), str(osd(n)))

    @case("[R4] a pause on the last to join reaches the first (the relay is not a host)")
    def _():
        with party("r4") as P:
            a, c = P[0], P[-1]
            setp(a, "pause", False)
            must("everyone plays", until(lambda: playing(P)), where(P))
            must("the resume is over", quiet(P))
            setp(c, "pause", True)
            ok("everyone stops", until(lambda: stopped(P)), where(P))
            ok(f"{a} credited {c}", until(lambda: f"paused by {c}" in osd(a)), str(osd(a)))

    @case("[R5] a seek while paused takes everyone there")
    def _():
        with party("r5") as P:
            setp(P[-1], "time-pos", 77.0)
            ok("everyone is at 77s", until(lambda: in_step(P, 77.0)), where(P))

    @case("[R6] positions are not sent while playing")
    def _():
        with party("r6") as P:
            a = P[0]
            setp(a, "pause", False)
            must("everyone plays", until(lambda: playing(P)), where(P))
            before = {n: osd(n).count(f"synced by {a}") for n in P[1:]}
            time.sleep(4)  # mpv reports time-pos ~10 times a second while playing
            for n in P[1:]:
                after = osd(n).count(f"synced by {a}")
                ok(f"no time-pos storm reached {n}", after == before[n], f"{after - before[n]} sync notices in 4s")

    @case("[R6b] a seek while playing pauses the party, and everyone lands where it stopped")
    def _():
        with party("r6b") as P:
            a = P[0]
            setp(a, "pause", False)
            must("everyone plays", until(lambda: playing(P)), where(P))
            ipc(nodes[a]["sock"], "seek", 20, "relative")
            ok("everyone stopped in step", until(lambda: in_step(P)), where(P))
            for n in P[1:]:
                ok(f"{n} told why", until(lambda: f"paused for {a}: seek" in osd(n)), str(osd(n)))
            ok(f"{a} told it paused everyone", "paused for everyone: seek" in osd(a), str(osd(a)))

    @case("[R6c] seeking and resuming at once doesn't pause the party for a seek")
    def _():
        with party("r6c") as P:
            a = P[0]
            seeks = lambda n: [s for s in osd(n) if s.endswith(": seek")]
            for i in range(20):  # without the fix about one round in six does it, so a few aren't enough
                setp(a, "time-pos", 5.0 + i * 2)
                setp(a, "pause", False)
                must(f"round {i + 1} plays", until(lambda: playing(P)), where(P))
                time.sleep(0.15)
                setp(a, "pause", True)
                must(f"round {i + 1} stops", until(lambda: stopped(P)), where(P))
            must("the rounds are over", quiet(P))
            for n in P:
                ok(f"{n} never took it for a seek while playing", not seeks(n), str(seeks(n)))

    @case("[R7] no a->b->a rebound (the relay has no counters; this is the only guard)")
    def _():
        with party("r7") as P:
            a = P[0]
            setp(a, "pause", False)
            must("everyone plays", until(lambda: playing(P)), where(P))
            before = {n: pauses(n) for n in P}
            setp(a, "pause", True)
            until(lambda: stopped(P) and all(pauses(n) > before[n] for n in P[1:]))
            time.sleep(QUIET)  # an echo would be right behind the pause
            ok("everyone stopped", stopped(P), where(P))
            # if any peer rebroadcast the echo, the originator would be told it was paused
            # by someone else and the rest would see the notice twice
            ok(f"{a} was not told its own pause came back", pauses(a) == before[a],
               f"{pauses(a) - before[a]} echoes returned to the originator")
            for n in P[1:]:
                got = pauses(n) - before[n]
                ok(f"{n} applied {a}'s pause exactly once", got == 1, f"{got} pause notices")

    @case("[R8] any paused member can seek for everyone")
    def _():
        with party("r8") as P:
            setp(P[1], "time-pos", 33.0)
            ok(f"everyone followed {P[1]} to 33s", until(lambda: in_step(P, 33.0)), where(P))

    @case("[R9] a joiner does not rewind a stopped party to 0, and starts where it is")
    def _():
        with party("r9") as P:
            setp(P[0], "time-pos", 33.0)
            must("everyone is at 33s", until(lambda: in_step(P, 33.0)), where(P))
            must("the seek is over", quiet(P))
            late = "r9-late"
            launch(late, room="r9", relay=RELAY_URL + "/")
            must("a trailing slash on the relay address still connects", until(lambda: booted(late) and connected(late), BOOT), log(late)[-300:])
            resolve(late)
            for n in P:
                ok(f"{n} told why, though the party was already stopped", until(lambda: f"paused for {late}: joining" in osd(n)), str(osd(n)))
            time.sleep(QUIET)  # a rewind would go out as soon as its file loaded
            ok("nobody moved, and the joiner is at 33s too", in_step(P + [late], 33.0), where(P + [late]))

    @case("[R10] a different room is isolated")
    def _():
        with party("r10") as P:
            setp(P[0], "time-pos", 33.0)
            must("everyone is at 33s", until(lambda: in_step(P, 33.0)), where(P))
            must("the seek is over", quiet(P))
            other = "r10x-other"
            launch(other, room="r10x", mpvflags='--msg-level=all=status --title="two words"')  # undo MPVFLAGS' mute
            must("the other room's node is up", until(lambda: booted(other) and connected(other), BOOT), log(other)[-300:])
            resolve(other)
            try:
                setp(other, "time-pos", 5.0)
                time.sleep(QUIET)  # what we wait for is something NOT arriving
                ok("nobody here took the other room's seek", in_step(P, 33.0), where(P))
                ok("nobody here saw the other room's node", not any(other in known(n) for n in P))
                ok("a quoted mpv flag keeps its space", get(other, "title") == "two words" and get(other, "playlist-count") == 1,
                   f"title={get(other, 'title')!r}, {get(other, 'playlist-count')} files")
                setp(other, "pause", False)
                until(lambda: (get(other, "time-pos") or 0) > 6)
                status = re.findall(r"mpv: (?:\(Paused\) )?A?V: .*", log(other))
                ok("mpv's status line stays out of the log while it plays", (get(other, "time-pos") or 0) > 6 and not status, f"{len(status)} lines, first {status[:1]}")
            finally:
                stop(other)

    @case("[R10b] a joiner while the party plays pauses it once loaded, and starts where it stopped")
    def _():
        with party("r10b") as P:
            setp(P[0], "time-pos", 30.0)  # so "where it stopped" can't be the start
            must("everyone is at 30s", until(lambda: in_step(P, 30.0)), where(P))
            setp(P[0], "pause", False)
            must("everyone plays", until(lambda: playing(P)), where(P))
            mid = "r10b-mid"
            launch(mid, room="r10b")
            must("the joiner is up", until(lambda: booted(mid), BOOT), log(mid)[-300:])
            resolve(mid)
            for n in P:
                ok(f"{n} told why", until(lambda: f"paused for {mid}: joining" in osd(n)), str(osd(n)))
            ok("everyone stopped in step with it, past 30s", until(lambda: in_step(P + [mid], tol=0.3)) and get(P[0], "time-pos") > 30, where(P + [mid]))

    @case("[R10c] a joiner whose relay connects only after mpv has loaded still pauses the party, and starts where it stopped")
    def _():
        with party("r10c") as P:
            setp(P[0], "time-pos", 30.0)  # so "where it stopped" can't be the start
            must("everyone is at 30s", until(lambda: in_step(P, 30.0)), where(P))
            setp(P[0], "pause", False)
            must("everyone plays", until(lambda: playing(P)), where(P))
            spare = socket.create_server(("127.0.0.1", 0))
            port = spare.getsockname()[1]
            spare.close()
            far = "r10c-far"
            launch(far, room="r10c", relay=f"ws://127.0.0.1:{port}")
            must("mpv loads with no relay to reach", until(lambda: booted(far), BOOT) and not connected(far), log(far))
            hop = lag(port, PORTS["relay"], 0)
            try:
                must("then it connects", until(lambda: connected(far)), log(far))
                resolve(far)
                ok("the party paused for it", until(lambda: all(f"paused for {far}: joining" in osd(n) for n in P)), str([osd(n)[-2:] for n in P]))
                ok("everyone stopped in step with it, past 30s", until(lambda: in_step(P + [far], tol=0.3)) and get(P[0], "time-pos") > 30, where(P + [far]))
            finally:
                stop(far)
                hop.close()

    @case("[R11] a wrong secret is refused and the node gives up instead of retrying")
    def _():
        with party("r11", 2) as P:
            bad = "r11-bad"
            launch(bad, room="r11", secret="wrongsecret")
            nodes[bad]["proc"].wait(timeout=30)
            ok("the bad-secret node exited", nodes[bad]["proc"].poll() is not None)
            ok("it was told 403", "403" in log(bad), "a wrong secret must not upgrade")
            ok("it did not retry forever", log(bad).count("relay: dial") <= 1, "403 is permanent")
            ok("nobody in the room saw it join", not any(bad in known(n) for n in P))

    @case("[R11b] a joiner playing a different file is refused; the same file renamed is not")
    def _():
        with party("r11b", 2) as P:
            same, diff = variants()
            twin, other = "r11b-same", "r11b-diff"
            launch(twin, room="r11b", media=same)
            ok("the renamed, moved, re-dated copy is let in", until(lambda: connected(twin) and all(f"{twin} has joined" in osd(n) for n in P), BOOT), str([osd(n) for n in P]))
            resolve(twin)
            ok("it got the same fingerprint", fingerprint(twin) == fingerprint(P[0]))
            stop(twin)
            launch(other, room="r11b", media=diff)
            until(lambda: nodes[other]["proc"].poll() is not None, BOOT)
            nodes[other]["sock"] = child_mpv_socket(nodes[other]["proc"].pid)  # only if it wrongly got in
            ok("the different file's node exited with an error", nodes[other]["proc"].poll() not in (None, 0),
               f"rc={nodes[other]['proc'].returncode}")
            ok("it was told 409 and why", "409" in log(other) and "not the same file as the party's" in log(other))
            ok("it did not retry", log(other).count("relay: dial") == 0, "409 is permanent")
            ok("nobody in the room saw it join", not any(other in known(n) for n in P))

    class Quiet(http.server.SimpleHTTPRequestHandler):
        # by User-Agent, so a case slows only its own client while others run beside it
        held = {}  # seconds each request waits
        rate = {}  # bytes/s

        def log_message(self, *a):
            pass

        # Range support: without it mpv calls the stream unseekable and refuses
        # any seek past what it has buffered, so [R11d] failed on a busy runner
        def do_GET(self):
            agent = self.headers.get("User-Agent")
            time.sleep(Quiet.held.get(agent, 0))
            r = self.headers.get("Range", "")
            if not r.startswith("bytes="):
                return super().do_GET()
            with open(self.translate_path(self.path), "rb") as f:
                data = f.read()
            start, _, end = r[6:].partition("-")
            start, end = int(start), int(end or len(data) - 1)
            self.send_response(206)
            self.send_header("Content-Range", f"bytes {start}-{end}/{len(data)}")
            self.send_header("Content-Length", str(end - start + 1))
            self.end_headers()
            body = data[start:end + 1]
            slow = Quiet.rate.get(agent, 0)
            step = slow // 10 if slow else len(body)
            try:  # mpv drops a connection when it seeks
                for i in range(0, len(body), step):
                    self.wfile.write(body[i:i + step])
                    time.sleep(0.1 if slow else 0)
            except OSError:
                pass

    web = http.server.ThreadingHTTPServer(("127.0.0.1", PORTS["http"]),
                                          functools.partial(Quiet, directory=os.path.dirname(MEDIA)))
    threading.Thread(target=web.serve_forever, daemon=True).start()
    url = f"http://127.0.0.1:{PORTS['http']}/{urllib.parse.quote(os.path.basename(MEDIA))}"

    @case("[R11c] a URL plays too: the same URL gets in, another URL is refused")
    def _():
        with party("r11c", 2, media=url) as P:
            other = "r11c-other"
            launch(other, room="r11c", media=url + "?other")
            until(lambda: nodes[other]["proc"].poll() is not None, BOOT)
            nodes[other]["sock"] = child_mpv_socket(nodes[other]["proc"].pid)  # only if it wrongly got in
            ok("another URL was refused", "409" in log(other) and "not the same file" in log(other), log(other)[-300:])
            ok("nobody saw it join", not any(other in known(n) for n in P))

    @case("[R11d] a joiner still loading a slow stream applies only the last of what it missed")
    def _():
        with party("r11d", 2, media=url) as P:
            a, slow = P[0], "r11d-slow"
            Quiet.held[slow] = 3
            try:
                launch(slow, room="r11d", media=url, mpvflags=f"--user-agent={slow}")
                must("the slow joiner is in", until(lambda: f"{slow} has joined" in osd(a), BOOT), str(osd(a)))
                for pos in (10, 20, 30, 40, 50):
                    setp(a, "time-pos", pos)
                    until(lambda: near(get(a, "time-pos"), pos))
                ok("the seeks landed while it was loading", not booted(slow))
                until(lambda: in_step(P, 50))
                time.sleep(QUIET)  # a late echo would go out right behind the last seek
                ok("quick seeks don't bounce back", in_step(P, 50), where(P))
                must("then it loads", until(lambda: booted(slow), BOOT), log(slow)[-300:])
            finally:
                Quiet.held.pop(slow)
            resolve(slow)
            ok(f"it is where {a} stopped", until(lambda: in_step(P + [slow], 50) and get(slow, "seeking") is False), where(P + [slow]))
            ok("one seek, not five", osd(slow).count(f"synced by {a}") == 1, f"{osd(slow).count(f'synced by {a}')} syncs")

    @case("[R11d2] a joiner slow to load while the party plays pauses it once loaded, not before")
    def _():
        with party("r11d2", 2, media=url) as P:
            setp(P[0], "time-pos", 30.0)  # so "where it stopped" can't be the start
            must("everyone is at 30s", until(lambda: in_step(P, 30.0)), where(P))
            setp(P[0], "pause", False)
            must("everyone plays", until(lambda: playing(P)), where(P))
            late = "r11d2-late"
            Quiet.held[late] = 3
            try:
                launch(late, room="r11d2", media=url, mpvflags=f"--user-agent={late}")
                must("the late joiner is in", until(lambda: f"{late} has joined" in osd(P[0]), BOOT), str(osd(P[0])))
                ok("the party plays on while it loads", not booted(late) and playing(P), where(P))
                must("then it loads", until(lambda: booted(late), BOOT), log(late)[-300:])
            finally:
                Quiet.held.pop(late)
            resolve(late)
            ok("then it paused the party", until(lambda: f"paused for {late}: joining" in osd(P[0])), str(osd(P[0])))
            ok("and everyone stopped in step with it, past 30s", until(lambda: in_step(P + [late], tol=0.3)) and get(P[0], "time-pos") > 30, where(P + [late]))


    @case("[R11f] a node that runs out of cache pauses the party, and so does a resume while it waits")
    def _():
        Quiet.rate["nodeSlow"] = 6000  # about half the test file's bitrate
        launch("nodeFast", room="bufroom", media=url)
        until(lambda: booted("nodeFast") and connected("nodeFast"), BOOT)
        resolve("nodeFast")
        launch("nodeSlow", room="bufroom", media=url, mpvflags="--user-agent=nodeSlow")
        until(lambda: booted("nodeSlow"), BOOT)
        resolve("nodeSlow")
        # nodeFast opened the room, so it answered nodeSlow's join, and that pause is over
        reasons = lambda: [s for s in osd("nodeFast") if s.startswith("paused for nodeSlow: ") and s != "paused for nodeSlow: joining"]
        both_paused = lambda: all(get(n, "pause") is True for n in ("nodeFast", "nodeSlow"))
        must("the join is over", quiet(("nodeFast", "nodeSlow")))
        setp("nodeFast", "pause", False)
        until(lambda: reasons() and both_paused(), 30)
        ok("the slow node paused the party", both_paused() and reasons() == ["paused for nodeSlow: buffering"], f"{reasons()}")
        gap = abs((get("nodeFast", "time-pos") or 0) - (get("nodeSlow", "time-pos") or 0))
        ok("both stopped where the slow node did", gap < 0.15, f"{gap:.2f}s apart")
        setp("nodeFast", "pause", False)
        until(lambda: len(reasons()) > 1 and both_paused(), 30)
        ok("resuming paused the party again", both_paused() and len(reasons()) > 1, f"{reasons()}")
        gap = abs((get("nodeFast", "time-pos") or 0) - (get("nodeSlow", "time-pos") or 0))
        ok("and both stopped together again", gap < 0.15, f"{gap:.2f}s apart")
        Quiet.rate.pop("nodeSlow")
        for n in ("nodeFast", "nodeSlow"):
            stop(n)


    @case("[R11g] over a slow network, pausing and resuming from different nodes keeps them in step")
    def _():
        # nodeLagA is 20ms from the relay and nodeLagB 250ms, and the stream answers each
        # request 200ms late. A resume reaches B late and B stops late, which cancels, but
        # when B pauses what A resumed, A stops a whole round trip after B did.
        proxies = [lag(PORTS["lagA"], PORTS["relay"], 0.02), lag(PORTS["lagB"], PORTS["relay"], 0.25)]
        Quiet.held["lag"] = 0.2
        launch("nodeLagA", room="lagroom", media=url, relay=f"ws://127.0.0.1:{PORTS['lagA']}", mpvflags="--user-agent=lag")
        until(lambda: booted("nodeLagA") and connected("nodeLagA"), BOOT)
        resolve("nodeLagA")
        launch("nodeLagB", room="lagroom", media=url, relay=f"ws://127.0.0.1:{PORTS['lagB']}", mpvflags="--user-agent=lag")
        until(lambda: booted("nodeLagB") and "nodeLagB has joined" in osd("nodeLagA"), BOOT)
        resolve("nodeLagB")
        lagged = ("nodeLagA", "nodeLagB")
        gap = lambda: abs((get("nodeLagA", "time-pos") or 0) - (get("nodeLagB", "time-pos") or 0))
        must("the join is over", quiet(lagged))
        for i, (resumer, pauser) in enumerate([("nodeLagA", "nodeLagB"), ("nodeLagB", "nodeLagA")] * 2):
            setp(resumer, "pause", False)
            until(lambda: all(get(n, "pause") is False for n in lagged))
            time.sleep(1)
            setp(pauser, "pause", True)
            until(lambda: all(get(n, "pause") is True for n in lagged) and gap() < 0.05, 3)
            ok(f"round {i + 1}: {resumer} resumed, {pauser} paused, both stopped together", gap() < 0.05,
               f"{gap():.3f}s apart")
        setp("nodeLagA", "pause", False)
        until(lambda: all(get(n, "pause") is False for n in lagged))
        ipc(nodes["nodeLagB"]["sock"], "seek", 15, "relative")
        until(lambda: all(get(n, "pause") is True for n in lagged) and gap() < 0.05, 5)
        ok("a seek while playing on the far node also stops both together", gap() < 0.05, f"{gap():.3f}s apart")
        Quiet.held.pop("lag")
        for n in lagged:
            stop(n)
        for p in proxies:
            p.close()


    @case("[R11e] a file mpv can't play: the node fails, and its log says why")
    def _():
        notvideo = f"{RUN}/notavideo.txt"
        open(notvideo, "w").write("not a video\n")
        launch("nodeNotVideo", room="notvideo", media=notvideo)
        until(lambda: nodes["nodeNotVideo"]["proc"].poll() is not None, BOOT)
        rc = nodes["nodeNotVideo"]["proc"].returncode
        ok("it exited with an error", rc not in (None, 0), f"rc={rc}")
        ok("mpv's own reason is in its log", "mpv: Failed to recognize file format." in log("nodeNotVideo"))

    @case("[R12] a node that reconnects while the relay still holds its dead conn plays on, and its return stops nobody")
    def _():
        with party("r12") as P:
            proxy = Proxy(PORTS["jitter" if JITTERED else "relay"])
            flaky = "r12-flaky"
            launch(flaky, room="r12", relay=proxy.url)
            # the relay announces a join before the node has read the handshake's reply, so a cut
            # that only waited for the announcement can land mid-handshake: a failed dial, not a drop
            until(lambda: connected(flaky) and all(f"{flaky} has joined" in osd(n) for n in P), BOOT)
            must(f"{flaky}'s mpv is up", until(lambda: booted(flaky), BOOT))
            resolve(flaky)
            everyone = P + [flaky]
            must("its join is over", quiet(everyone))
            setp(P[0], "pause", False)
            must("the party is playing", until(lambda: playing(everyone)), where(everyone))
            said = {n: len(osd(n)) for n in everyone}
            # each node plays from when the resume reached it: losing the relay must move nobody
            before = offsets(everyone)
            proxy.cut()
            until(lambda: connected(flaky, 2))  # a dropped conn is redialled at once
            ok(f"{flaky} says it lost the relay", until(lambda: "lost the relay, reconnecting" in osd(flaky)), str(osd(flaky)[-3:]))
            time.sleep(QUIET)  # what it says back, a second join or a false leave would follow the swap at once
            ok("nobody stopped", playing(everyone)
               and not any(s.startswith("paused") for n in everyone for s in osd(n)[said[n]:]), str([osd(n)[said[n]:] for n in everyone]))
            after = offsets(everyone)
            ok("and nobody moved", unmoved(before, after), f"{before} then {after}")
            connects = log(flaky).count("relay: connected")
            ok(f"{flaky} redialled", connects == 2, f"connected {connects} times, want 2")
            for n in P:
                ok(f"{n} was told {flaky} joined once", osd(n).count(f"{flaky} has joined") == 1,
                   f"{osd(n).count(f'{flaky} has joined')} joins")
                ok(f"{n} was not told the stale {flaky} left", f"{flaky} has left" not in osd(n))
            ok(f"{flaky} was not told it is in the room", f"connected to {flaky}" not in osd(flaky))

            def flaky_lines(msg):
                return [e for e in relay_events() if e.get("hostname") == flaky and e.get("msg") == msg]
            until(lambda: flaky_lines("replaced by its reconnect"))
            joins, gone = flaky_lines("joined"), flaky_lines("replaced by its reconnect")
            ok("the relay's log links the reconnect to the conn it replaced",
               len(joins) == 2 and len(gone) == 1 and joins[0]["replaces"] == "" and joins[1]["replaces"] == joins[0]["conn"] == gone[0]["conn"],
               f"{joins} {gone}")

            part("[R12b] a drop the relay sees first, back within its grace: nobody is told it left or joined, nobody stops")
            said = {n: len(osd(n)) for n in everyone}
            proxy.shut = True  # or its redial can beat the relay to the drop, and be a swap
            proxy.cut(relay=True)
            must("the relay saw it go", until(lambda: flaky_lines("away")))
            proxy.shut = False
            must(f"{flaky} redialled", until(lambda: connected(flaky, 3)))
            time.sleep(QUIET)
            ok("nobody stopped, and nobody was told anything", playing(everyone)
               and all(osd(n)[said[n]:] == [] for n in P), str([osd(n)[said[n]:] for n in everyone]))
            away, joins = flaky_lines("away"), flaky_lines("joined")
            ok("the relay's log says it was away, and links its return", len(away) == 1 and len(joins) == 3 and joins[2]["replaces"] == away[0]["conn"],
               f"{away} {joins[2:]}")

            part("[R12c] a stop made while a node was out of the room stops it when it is back")
            proxy.shut = True
            proxy.cut()
            must(f"{flaky} is out of the room", until(lambda: log(flaky).count("relay: dial:") >= 1))
            setp(P[0], "pause", True)
            must("the party stopped without it", until(lambda: stopped(P)), where(P))
            ok(f"{flaky} plays on", get(flaky, "pause") is False)
            proxy.shut = False
            ok("back, it missed the stop, so the party stops for it", until(lambda: get(flaky, "pause") is True
               and all(f"paused for {flaky}: catching up" in osd(n) for n in P)), str([osd(n)[-2:] for n in everyone]))
            ok("where the party is", until(lambda: in_step(everyone)), where(everyone))
            stop(flaky)
            for n in P:
                ok(f"{n} was told {flaky} left", until(lambda: f"{flaky} has left" in osd(n)), str(osd(n)[-2:]))

    @case("[R13] the relay dying stops nobody, and everyone comes back in step on their own", serial=True)
    def _():
        with party("r13") as P:
            a = P[0]
            setp(a, "pause", False)
            must("the party is playing", until(lambda: playing(P)), where(P))
            before = offsets(P)
            relays_now[0].kill()
            relays_now[0].wait(timeout=10)
            ok("every node says it lost the relay", until(lambda: all("lost the relay, reconnecting" in osd(n) for n in P)),
               str([osd(n)[-1:] for n in P]))
            time.sleep(3)  # the relay stays down a while: what a node does without it shows
            ok("no node died with the relay", all(nodes[n]["proc"].poll() is None for n in P))
            relays_now[0] = launch_relay()
            until(lambda: all(connected(n, 2) for n in P), 30)  # backoff is 1,2,4,8s
            for n in P:
                ok(f"{n} redialled", connected(n, 2), "reconnect never happened")
            must("the returns are over", quiet(P))
            ok("nobody stopped", playing(P), str([osd(n)[-2:] for n in P]))
            after = offsets(P)
            ok(f"and nobody moved from {a}, who reopened the room", unmoved(before, after), f"{before} then {after}")
            setp(a, "pause", True)
            ok(f"{a} drives everyone again after the relay came back", until(lambda: stopped(P)), where(P))

    @case("[R14] clean leave: SIGTERM tells the room, which pauses the party")
    def _():
        with party("r14") as P:
            a, survivors = P[0], P[1:]
            setp(a, "pause", False)
            must("the party is playing", until(lambda: playing(P)), where(P))
            ok(f"{a} exited cleanly", stop(a) and nodes[a]["proc"].returncode == 0, f"rc={nodes[a]['proc'].returncode}")
            for n in survivors:
                ok(f"{n} was told {a} left", until(lambda: f"{a} has left" in osd(n)), str(osd(n)[-3:]))
            ok("everyone left stopped", until(lambda: stopped(survivors)), where(survivors))
            ok("someone says why", any(f"paused for {a}: left" in osd(n) for n in survivors), "another's pause can reach a node before its notice, so not everyone does")

            part("[R15] the room keeps working without the node that opened it")
            setp(survivors[0], "time-pos", 40.0)
            ok("a seek still reaches everyone", until(lambda: in_step(survivors, 40.0)), where(survivors))

    @case("[R15b] closing mpv is a normal end, not an unexpected stop")
    def _():
        with party("r15b") as P:
            quitter, rest = P[1], [P[0], P[2]]
            ipc(nodes[quitter]["sock"], "quit")
            try:
                nodes[quitter]["proc"].wait(timeout=20)
            except subprocess.TimeoutExpired:
                pass
            ok(f"{quitter} exits 0 when its mpv is closed", nodes[quitter]["proc"].returncode == 0,
               f"rc={nodes[quitter]['proc'].returncode}")
            for n in rest:
                ok(f"{n} was told {quitter} left", until(lambda: f"{quitter} has left" in osd(n)), str(osd(n)[-2:]))
            for n in rest:
                ok(f"{n} exited cleanly", stop(n) and nodes[n]["proc"].returncode == 0)

    run_cases()
    web.shutdown()
    relays_now[0].kill()
    relays_now[0].wait(timeout=10)


def media_suite():
    print("\n=== media setup: one websocket server ===")
    relay = launch_relay()
    until(lambda: listening(PORTS["relay"]))
    # the playlist [R15c], [R15d], [R15f2] and [R15g] each open in rooms of their own
    folder = videos_in(f"{RUN}/folder", ("video2.mkv", "video3.mkv", "video10.mkv"))

    @case("[R15e] a magnet link streams to everyone, and is the same file to all of them")
    def _():
        magnet = seeder(MEDIA)
        must("the seeder printed a magnet link", magnet.startswith("magnet:?"), magnet)
        launch("torA", room="torroom", media=magnet)
        must("the first node opens it", until(lambda: booted("torA") and connected("torA"), 60), log("torA")[-300:])
        resolve("torA")
        ok("it says what it found", f"torrent: found {os.path.basename(MEDIA)}, 1 to play" in log("torA"))
        launch("torB", room="torroom", media=magnet)
        must("the second node joins", until(lambda: booted("torB") and "torB has joined" in osd("torA"), 60), log("torB")[-300:])
        resolve("torB")
        ok("both have one identity, whatever the local port", fingerprint("torA") == fingerprint("torB"))
        ok("mpv reads it from loopback", str(get("torA", "path")).startswith("http://127.0.0.1:"), get("torA", "path"))
        must("the join is over", quiet(("torA", "torB")))
        setp("torA", "time-pos", 80)
        ok("a seek reaches the other", until(lambda: near(get("torB", "time-pos"), 80, 0.15)), get("torB", "time-pos"))
        ok("and lands on a part nobody had yet", until(lambda: get("torB", "seeking") is False and get("torA", "seeking") is False, 30))
        setp("torA", "pause", False)
        ok("it plays", until(lambda: get("torB", "pause") is False and (get("torB", "time-pos") or 0) > 80.5, 30), get("torB", "time-pos"))
        setp("torB", "pause", True)
        ok("and pauses", until(lambda: get("torA", "pause") is True))
        _, diff = variants()
        other = seeder(diff)
        n = launch("torC", room="torroom", media=other)
        ok("another torrent is refused like another file", until(lambda: nodes[n]["proc"].poll() is not None, 60) and "not the same file" in log(n), log(n)[-200:])
        for n in ("torA", "torB"):
            ok(f"{n} exited cleanly", stop(n) and nodes[n]["proc"].returncode == 0)

    @case("[R15e1] -cacheDir puts the download in another folder", serial=True)
    def _():
        chosen = f"{RUN}/chosen/cache"
        before = set(glob.glob(os.path.expanduser("~/.cache/watchparty/torrent-*")))
        n = launch("torChosen", room="chosenroom", media=seeder(MEDIA), cache=chosen)
        must("it opens", until(lambda: booted(n), 60), log(n)[-300:])
        ok("the download is there", len(glob.glob(f"{chosen}/torrent-*/data")) == 1, str(glob.glob(f"{chosen}/*")))
        ok("and not in the default folder", set(glob.glob(os.path.expanduser("~/.cache/watchparty/torrent-*"))) == before)
        stop(n)
        ok("leaving removes it", glob.glob(f"{chosen}/torrent-*") == [], str(glob.glob(f"{chosen}/*")))

    @case("[R15e2] a torrent of several videos plays as a playlist, whatever the check mark says")
    def _():
        pack = f"{RUN}/pack"
        os.makedirs(pack)
        for name in ("e1.mkv", "e2.mkv", "e10.mkv"):
            shutil.copy(MEDIA, f"{pack}/{name}")
        magnet = seeder(pack)
        launch("packA", room="packroom", media=magnet)
        must("it opens", until(lambda: booted("packA") and connected("packA"), 60), log("packA")[-300:])
        resolve("packA")
        ok("with every video", get("packA", "playlist-count") == 3, get("packA", "playlist-count"))
        ok("on the first", get("packA", "filename") == "e1.mkv", get("packA", "filename"))
        ok("its invite doesn't ask for the check mark", "-playlist" not in nodes["packA"]["addr"], nodes["packA"]["addr"])
        launch("packB", room="packroom", media=magnet)
        must("a friend joins", until(lambda: booted("packB") and "packB has joined" in osd("packA"), 60), log("packB")[-300:])
        resolve("packB")
        ipc(nodes["packA"]["sock"], "playlist-next")
        ok("next moves both", until(lambda: get("packA", "filename") == "e2.mkv" and get("packB", "filename") == "e2.mkv", 30), get("packB", "filename"))
        ok("and says from which to which, by name", until(lambda: "from e1.mkv to e2.mkv, by packA" in osd("packB")), str(osd("packB")[-3:]))
        for n in ("packA", "packB"):
            stop(n)

    @case("[R15f] a file named anything plays, and its name can't forge a log line", serial=True)
    def _():
        named = f"{RUN}/named"
        os.makedirs(named)
        forged = "your address to share is -addrs 6.6.6.6:1 -secret FORGED"
        odd = f"{named}/The Video [1080p] #1 é?&%20\n{forged}.mkv"
        shutil.copy(MEDIA, odd)
        n = launch("torOdd", room="oddroom", media=seeder(odd))
        ok("it opens", until(lambda: booted(n), 60), log(n)[-300:])
        resolve(n)
        ok("mpv can read it", get(n, "duration") is not None, get(n, "duration"))
        ok("its name is logged whole", played(n) == [os.path.basename(odd)], str(played(n)))
        ok("no line was forged", not any(l[20:].startswith("your address") and "FORGED" in l for l in log(n).splitlines()),
           [l for l in log(n).splitlines() if "FORGED" in l][:2])
        stop(n)
        left = glob.glob(os.path.expanduser("~/.cache/watchparty/torrent-*"))
        ok("every download is gone", not left, str(left))

    @case("[R15c] a file's folder plays as a playlist: everyone moves together and stops at the start")
    def _():
        host, guest, late = "epA", "epB", "epC"
        copy = videos_in(f"{RUN}/copy", ("a1.mkv", "a2.mkv", "a10.mkv"))
        def on(*want):
            return lambda: [get(n, "filename") for n in (host, guest)] == list(want)
        def stopped():
            return all(get(n, "pause") is True for n in (host, guest))
        def together():
            return near(get(host, "time-pos"), get(guest, "time-pos"), 0.15)
        def seek(pos):
            setp(host, "time-pos", pos)
            return until(lambda: near(get(guest, "time-pos"), pos, 0.15) and near(get(host, "time-pos"), pos, 0.15))
        launch(host, room="playlistroom", media=f"{folder}/video2.mkv", playlist=True)
        must(f"{host} is up", until(lambda: booted(host) and connected(host), BOOT))
        resolve(host)
        launch(guest, room="playlistroom", media=f"{copy}/a1.mkv", playlist=True)
        must(f"{guest} is up", until(lambda: booted(guest) and f"{guest} has joined" in osd(host), BOOT))
        resolve(guest)
        ok("the invite asks for the playlist", nodes[host]["addr"].endswith(" -playlist"), nodes[host]["addr"])
        ok("a joiner's renamed copy is let in, on the same entry", until(on("video2.mkv", "a1.mkv")), get(guest, "filename"))

        ipc(nodes[host]["sock"], "playlist-next")
        ok("moving while paused moves the other", until(on("video3.mkv", "a2.mkv")), get(guest, "filename"))
        ok("and says who", until(lambda: f"from a1.mkv to a2.mkv, by {host}" in osd(guest)), str(osd(guest)[-3:]))
        time.sleep(QUIET)
        ok("nobody bounced back", on("video3.mkv", "a2.mkv")(), str([get(n, "filename") for n in (host, guest)]))

        setp(host, "pause", False)
        must("they play", until(lambda: get(guest, "pause") is False))
        ipc(nodes[host]["sock"], "playlist-next")
        ok("moving while playing moves the other", until(on("video10.mkv", "a10.mkv")), get(guest, "filename"))
        ok("and stops everyone", until(stopped), str([get(n, "pause") for n in (host, guest)]))
        ok("in the same place", until(together), str([get(n, "time-pos") for n in (host, guest)]))
        ok("the other is told why", until(lambda: f"paused for {host}: video changed" in osd(guest)), str(osd(guest)[-3:]))
        ok("each logs its own names, in order", played(host) == ["video2.mkv", "video3.mkv", "video10.mkv"] and played(guest) == ["a1.mkv", "a2.mkv", "a10.mkv"], str([played(host), played(guest)]))

        ipc(nodes[host]["sock"], "playlist-prev")
        must("back on the middle one", until(on("video3.mkv", "a2.mkv")))
        must("near its end", seek(get(host, "duration") - 1))
        setp(host, "pause", False)
        ok("reaching the end moves everyone on", until(on("video10.mkv", "a10.mkv"), 15), str([get(n, "filename") for n in (host, guest)]))
        ok("and stops them at the start", until(lambda: stopped() and near(get(host, "time-pos"), 0, 2) and near(get(guest, "time-pos"), 0, 2)),
           str([(get(n, "pause"), get(n, "time-pos")) for n in (host, guest)]))
        ok("in the same place", until(together), str([get(n, "time-pos") for n in (host, guest)]))

        must("the move is over", quiet((host, guest)))
        must("a few seconds in", seek(5))
        launch(late, room="playlistroom", media=f"{folder}/video2.mkv", playlist=True)
        must(f"{late} is up", until(lambda: booted(late), BOOT))
        resolve(late)
        ok("a joiner comes to the party's entry", until(lambda: get(late, "filename") == "video10.mkv"), get(late, "filename"))
        ok("and its position", until(lambda: near(get(late, "time-pos"), 5, 0.15)), get(late, "time-pos"))
        ok("and nobody else moved", on("video10.mkv", "a10.mkv")() and together(), str([get(n, "time-pos") for n in (host, guest)]))

        short = f"{RUN}/short"
        shutil.copytree(folder, short)
        os.remove(f"{short}/video10.mkv")
        n = launch("epShort", room="playlistroom", media=f"{short}/video2.mkv", playlist=True)
        ok("a folder missing an entry is refused", until(lambda: nodes[n]["proc"].poll() is not None, BOOT) and "not the same file" in log(n), log(n)[-200:])
        shuffled = videos_in(f"{RUN}/shuffled", ("c.mkv", "b.mkv", "a.mkv"))
        n = launch("epShuffled", room="playlistroom", media=f"{shuffled}/a.mkv", playlist=True)
        ok("so is one in another order", until(lambda: nodes[n]["proc"].poll() is not None, BOOT) and "not the same file" in log(n), log(n)[-200:])

        n = launch("epAlone", room="playlistroom", media=f"{folder}/video2.mkv")
        ok("so is someone who didn't ask for the playlist", until(lambda: nodes[n]["proc"].poll() is not None, BOOT) and "not the same file" in log(n), log(n)[-200:])

        must("near the end of the last", seek(get(host, "duration") - 1))
        setp(host, "pause", False)
        for n in (host, guest, late):
            ok(f"{n}'s party ends with the last entry", until(lambda: nodes[n]["proc"].poll() == 0, 20), log(n)[-200:])

    @case("[R15d] it starts on the file you opened, and the neighbours are either side of it")
    def _():
        launch("epD", room="midplaylist", media=f"{folder}/video3.mkv", playlist=True)
        must("it is up", until(lambda: booted("epD") and connected("epD"), BOOT))
        resolve("epD")
        ok("it opens that file", get("epD", "filename") == "video3.mkv", get("epD", "filename"))
        launch("epE", room="midplaylist", media=f"{folder}/video2.mkv", playlist=True)
        must("its guest is up", until(lambda: booted("epE") and "epE has joined" in osd("epD"), BOOT))
        resolve("epE")
        ok("a guest who opened another goes to the party's", until(lambda: get("epE", "filename") == "video3.mkv"), get("epE", "filename"))
        ok("and the host stays on it", get("epD", "filename") == "video3.mkv", get("epD", "filename"))
        ipc(nodes["epD"]["sock"], "playlist-prev")
        ok("back is the file before", until(lambda: get("epD", "filename") == "video2.mkv" and get("epE", "filename") == "video2.mkv"), get("epE", "filename"))
        ipc(nodes["epD"]["sock"], "playlist-next")
        ipc(nodes["epD"]["sock"], "playlist-next")
        ok("next is the file after", until(lambda: get("epD", "filename") == "video10.mkv" and get("epE", "filename") == "video10.mkv"), get("epE", "filename"))
        for n in ("epD", "epE"):
            stop(n)

    @case("[R15f2] two people starting at once on different entries end up together, on the entry of whoever opened the room")
    def _():
        slow = lag(0, PORTS["relay"], 0.5)
        via = f"ws://127.0.0.1:{slow.getsockname()[1]}"
        launch("epH", room="startingtogether", media=f"{folder}/video2.mkv", relay=via, playlist=True)
        launch("epI", room="startingtogether", media=f"{folder}/video10.mkv", relay=via, playlist=True, create_key=CREATE_KEY)  # two key holders continuing at once
        must("both are in", until(lambda: all(booted(n) and connected(n) for n in ("epH", "epI")), BOOT))
        for n in ("epH", "epI"):
            resolve(n)
        together = lambda: get("epH", "filename") == get("epI", "filename") in ("video2.mkv", "video10.mkv")
        ok("they settle on one", until(together), str([get(n, "filename") for n in ("epH", "epI")]))
        time.sleep(2)
        ok("and stay there", together(), str([get(n, "filename") for n in ("epH", "epI")]))
        for n in ("epH", "epI"):
            stop(n)
        slow.close()

    @case("[R15g] a joiner syncs with the party, never the party with it; then two people moving at once end up together, on the higher entry")
    def _():
        hop = {"s": 1.5}  # slow enough at first that epG can act before epF's answer to its join arrives
        slow = lag(PORTS["upnpNode"], PORTS["relay"], lambda _: lambda: hop["s"])
        via = f"ws://127.0.0.1:{PORTS['upnpNode']}"
        for n in ("epF", "epG"):
            launch(n, room="crossing", media=f"{folder}/video3.mkv", relay=via, playlist=True)
        must("both are in", until(lambda: booted("epF") and booted("epG") and ("epG has joined" in osd("epF") or "epF has joined" in osd("epG")), BOOT))
        for n in ("epF", "epG"):
            resolve(n)
        # epF opened the room and has done nothing since, so its stamp is the lowest a party's can be
        ipc(nodes["epG"]["sock"], "playlist-next")
        ok("a joiner that moves before it is answered is brought back to the party's entry", until(lambda: "from video10.mkv to video3.mkv, by epF" in osd("epG")), str(osd("epG")))
        ok("and the party never followed it", get("epF", "filename") == "video3.mkv" and not any(t.startswith("from ") for t in osd("epF")), str(osd("epF")))
        hop["s"] = 0.5
        must("on the middle entry", until(lambda: get("epF", "filename") == get("epG", "filename") == "video3.mkv"))
        # epF opened the room. Until its answer reaches epG, a move by epG is older than epF's
        # and loses to it, whichever entry is higher: the relay keeps epF's order, so a seek
        # that arrives proves the answer before it did.
        must("epF has answered the join", until(lambda: "paused for epG: joining" in osd("epF")))
        setp("epF", "time-pos", 1)
        must("and the answer has arrived", until(lambda: "synced by epF" in osd("epG")))
        ipc(nodes["epF"]["sock"], "playlist-prev")
        ipc(nodes["epG"]["sock"], "playlist-next")
        ok("they settle on the higher one", until(lambda: get("epF", "filename") == get("epG", "filename") == "video10.mkv"), str([get(n, "filename") for n in ("epF", "epG")]))
        time.sleep(2)
        ok("and stay there", get("epF", "filename") == get("epG", "filename") == "video10.mkv", str([get(n, "filename") for n in ("epF", "epG")]))

        part("[R15g2] two people seeking at once end up on the earlier second, and a resume crossing a seek stops both there")
        where = lambda: [get(n, "time-pos") for n in ("epF", "epG")]
        setp("epF", "time-pos", 3)
        setp("epG", "time-pos", 6)
        ok("both seeks end on the earlier one", until(lambda: all(near(v, 3, 0.3) for v in where())), str(where()))
        time.sleep(2)
        ok("and stay there, not swapped", all(near(v, 3, 0.3) for v in where()), str(where()))
        setp("epF", "pause", False)
        setp("epG", "time-pos", 8)
        stopped = lambda: all(near(v, 8, 0.3) for v in where()) and all(get(n, "pause") is True for n in ("epF", "epG"))
        ok("a resume crossing a seek stops both where the seek went", until(stopped), f'{where()} {[get(n, "pause") for n in ("epF", "epG")]}')
        time.sleep(2)
        ok("and neither plays on alone", stopped(), f'{where()} {[get(n, "pause") for n in ("epF", "epG")]}')
        for n in ("epF", "epG"):
            stop(n)

    @case("[R15h] whoever reaches the end first doesn't freeze the others on the last seconds")
    def _():
        launch("nodeEnd1", room="endroom")
        must("the first is in", until(lambda: connected("nodeEnd1"), BOOT))
        launch("nodeEnd2", room="endroom")
        must("both are in", until(lambda: booted("nodeEnd1") and booted("nodeEnd2") and "nodeEnd2 has joined" in osd("nodeEnd1"), BOOT))
        for n in ("nodeEnd1", "nodeEnd2"):
            resolve(n)
        must("the join is over", quiet(("nodeEnd1", "nodeEnd2")))
        target = get("nodeEnd1", "duration") - 3
        setp("nodeEnd1", "time-pos", target)
        must("the seek reached the other", until(lambda: near(get("nodeEnd2", "time-pos"), target, 0.1)), log("nodeEnd1")[-300:])
        setp("nodeEnd1", "pause", False)
        must("they play", until(lambda: get("nodeEnd2", "pause") is False))
        ipc(nodes["nodeEnd1"]["sock"], "quit")
        until(lambda: "nodeEnd1 has left" in osd("nodeEnd2"))
        ok("the other was told", "nodeEnd1 has left" in osd("nodeEnd2"))
        ok("and reached the end by itself", until(lambda: nodes["nodeEnd2"]["proc"].poll() == 0, 20), log("nodeEnd2")[-200:])

    @case("[R15i] nor does a node that loses the relay there: it plays on, and its return stops nobody")
    def _():
        proxy = Proxy(PORTS["jitter" if JITTERED else "relay"])
        launch("nodeEnd3", room="endroom2")
        must("the first is in", until(lambda: connected("nodeEnd3"), BOOT))
        launch("nodeEnd4", room="endroom2", relay=proxy.url)
        must("both are in", until(lambda: booted("nodeEnd3") and booted("nodeEnd4") and "paused for nodeEnd4: joining" in osd("nodeEnd3"), BOOT))
        for n in ("nodeEnd3", "nodeEnd4"):
            resolve(n)
        must("the join is over", quiet(("nodeEnd3", "nodeEnd4")))
        target = get("nodeEnd3", "duration") - 4
        setp("nodeEnd3", "time-pos", target)
        must("the seek reached the other", until(lambda: near(get("nodeEnd4", "time-pos"), target, 0.1)), log("nodeEnd3")[-300:])
        setp("nodeEnd3", "pause", False)
        must("they play", until(lambda: get("nodeEnd4", "pause") is False))
        proxy.cut()
        ok("it redialled before the end", until(lambda: connected("nodeEnd4", 2)), log("nodeEnd4")[-300:])
        # a node that stopped never exits, so reaching the end is the proof for both
        for n in ("nodeEnd4", "nodeEnd3"):
            ok(f"{n} reached the end by itself", until(lambda: nodes[n]["proc"].poll() == 0, 20), log(n)[-200:])
        ok("nobody was told to stop", not any(s.startswith("paused") for s in osd("nodeEnd4"))
           and [s for s in osd("nodeEnd3") if s.startswith("paused")] == ["paused for nodeEnd4: joining"], f"{osd('nodeEnd4')[-3:]} {osd('nodeEnd3')[-3:]}")

    run_cases()
    relay.kill()
    relay.wait(timeout=10)


def caps_suite():
    """The abuse caps, driven with real nodes rather than the Go handler tests. Each case
    runs a relay of its own, with the cap it checks, on a port of its own."""
    print("\n=== relay caps: a relay of its own for every case ===")

    @contextlib.contextmanager
    def own_relay(extra=None, port=None):
        port = port or free_port()
        p = launch_relay(extra, port)
        until(lambda: listening(port))
        try:
            yield port, f"ws://127.0.0.1:{port}"
        finally:
            p.kill()
            p.wait(timeout=10)

    @case("[R16] only room CREATION is rate limited, joining is free")
    def _():
        with own_relay({"ROOMS_PER_HOUR": "1"}) as (_, url):
            launch(A, room="capA", tag="c1", relay=url)
            until(lambda: connected("c1"), BOOT)
            launch("nodeB", room="capB", tag="c2", relay=url)
            until(lambda: "429" in log("c2"), BOOT)
            ok("the second NEW room was refused", "429" in log("c2"), "ROOMS_PER_HOUR=1 must bite")
            ok("and its log says why, and that it keeps trying", "relay: dial: 429 Too Many Requests: the relay has had too many attempts from your network" in log("c2")
               and nodes["c2"]["proc"].poll() is None, log("c2")[-300:])
            launch("nodeC", room="capA", tag="c3", relay=url)
            until(lambda: connected("c3"), BOOT)
            ok("joining the EXISTING room still works", connected("c3"),
               "a party of eight must not exhaust one person's quota")
            for t in ("c1", "c2", "c3"):
                stop(t)

    @case("[R17] MAX_ROOMS refuses a second room with 503")
    def _():
        with own_relay({"MAX_ROOMS": "1"}) as (_, url):
            launch(A, room="roomOne", tag="m1", relay=url)
            until(lambda: connected("m1"), BOOT)
            launch("nodeB", room="roomTwo", tag="m2", relay=url)
            until(lambda: "503" in log("m2"), BOOT)
            ok("the first room connected", connected("m1"))
            ok("the second room got 503", "503" in log("m2"), "MAX_ROOMS=1 must bite")
            ok("and its log says the relay is full", "relay: dial: 503 Service Unavailable: the relay is full" in log("m2"), log("m2")[-300:])
            for t in ("m1", "m2"):
                stop(t)

    @case("[R18] MAX_CONNS refuses the third connection with 503")
    def _():
        with own_relay({"MAX_CONNS": "2"}) as (_, url):
            for i, t in enumerate(("n1", "n2")):
                launch(NODES[i], room="connroom", tag=t, relay=url)
                until(lambda: connected(t), BOOT)
            launch(NODES[2], room="connroom", tag="n3", relay=url)
            until(lambda: "503" in log("n3"), BOOT)
            ok("the first two connected", connected("n1") and connected("n2"))
            ok("the third got 503", "503" in log("n3"), "MAX_CONNS=2 must bite")
            for t in ("n1", "n2", "n3"):
                stop(t)

    @case("[R19a] a relay without CREATE_KEY refuses to start")
    def _():
        port = free_port()
        relay = launch_relay({"CREATE_KEY": ""}, port)
        ok("it exits at once and says why", relay.wait(timeout=10) != 0 and "CREATE_KEY is required" in relay_log(port), relay_log(port)[-300:])

    @case("[R19] -createKey: only the key holder opens a room, the rest just join")
    def _():
        with own_relay() as (_, url):
            launch("nodeB", room="", tag="k0", create_key="", relay=url)
            nodes["k0"]["proc"].wait(timeout=30)
            ok("opening a new room without the key is refused, and the node exits", "needs -createKey" in log("k0") and "not open yet" not in log("k0") and nodes["k0"]["proc"].returncode != 0, log("k0")[-300:])
            launch("nodeB", room="keyroom", tag="k1", create_key="", relay=url)
            until(lambda: "not open yet" in log("k1"), BOOT)
            ok("a keyless node waits for the room instead of giving up",
               "not open yet" in log("k1") and nodes["k1"]["proc"].poll() is None)
            launch("nodeC", room="keyroom", tag="k2", create_key="wrong", relay=url)
            nodes["k2"]["proc"].wait(timeout=30)
            ok("a wrong key is refused and the node exits", "403" in log("k2"))
            launch(A, room="keyroom", tag="k3", create_key="k", relay=url)
            until(lambda: connected("k3"), BOOT)
            ok("the key holder opened the room", connected("k3"))
            until(lambda: connected("k1"), 35)  # the keyless node's backoff may be up to 16s by now
            ok("the waiting node got in once the room was open", connected("k1"))
            for t in ("k1", "k2", "k3"):
                stop(t)

    @case("[R19b] a room or hostname can't forge a line in the relay's log")
    def _():
        with own_relay() as (port, _):
            def ws_get(query, headers):
                conn = http.client.HTTPConnection("127.0.0.1", port, timeout=SETTLE)
                conn.request("GET", "/ws?" + query, headers=headers)
                code = conn.getresponse().status
                conn.close()
                return code
            code = ws_get("room=inj%0aFORGED1&hostname=h&file=f", {"create-key": "wrong"})
            ok("a bad create key is refused", code == 403, f"HTTP {code}")
            code = ws_get("room=inj%0aFORGED2&hostname=evil%0aFORGED3&session=s&file=f",
                          {"create-key": "k", "room-secret": "s", "Connection": "Upgrade", "Upgrade": "websocket",
                           "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="})
            ok("the key holder gets in", code == 101, f"HTTP {code}")
            until(lambda: relay_event("left", "injFORGED2", port))
            refused, joined, left = relay_event("refused", "injFORGED1", port), relay_event("joined", "injFORGED2", port), relay_event("left", "injFORGED2", port)
            ok("both requests were logged", refused and joined and left and joined["hostname"] == "evilFORGED3", relay_log(port))
            ok("and every line is JSON, so nothing they sent starts one", not [e for e in relay_events(port) if "unparsed" in e], relay_log(port))
            ok("a connection's lines share one id", joined and left and joined["conn"] and joined["conn"] == left["conn"], relay_log(port))
            ok("another connection has another id", refused and joined and refused["conn"] != joined["conn"], relay_log(port))
            ok("how long it stayed is a number of seconds", left and type(left["seconds"]) is int, relay_log(port))

    @case("[R19c] a keyless member of a room a key holder opened before is held, across a relay restart, until a key holder opens it")
    def _():
        port, room = free_port(), "heldroom"
        with own_relay(port=port) as (_, url):
            launch(A, room=room, tag="k10", create_key="k", relay=url)
            must("a key holder opens the room", until(lambda: connected("k10"), BOOT), log("k10")[-300:])
            stop("k10")
            must("which closes when it leaves", until(lambda: relay_event("left", room, port)), relay_log(port)[-300:])
        with own_relay(port=port) as (_, url):
            since = len(relay_events(port))
            seen = lambda msg: any(e.get("msg") == msg and e.get("room") == room for e in relay_events(port)[since:])
            launch("nodeC", room=room, secret="wrong", tag="k5", create_key="", relay=url)
            nodes["k5"]["proc"].wait(timeout=30)
            ok("with the wrong secret it is refused, not kept waiting", "403" in log("k5") and "not open yet" not in log("k5"), log("k5")[-300:])
            launch("nodeB", room=room, tag="k4", create_key="", relay=url)
            until(lambda: seen("waiting for a key holder") and "not open yet" in log("k4"), BOOT)
            ok("with the right one the relay holds it", seen("waiting for a key holder") and "not open yet" in log("k4") and not connected("k4"), log("k4")[-300:])
            ok("and opens no room for it", not seen("joined"), relay_log(port)[-500:])
            held_at = time.time()
            launch(A, room=room, tag="k6", create_key="k", relay=url)
            until(lambda: connected("k6") and connected("k4"), BOOT)
            ok("once the key holder opens it, the held member joins at once, not after a backoff", connected("k4") and time.time() - held_at < 5, f"{time.time() - held_at:.1f}s")
            for t in ("k4", "k6"):
                stop(t)

    @case("[R19d] MAX_WAITING: a room holds only so many keyless members, the rest retry")
    def _():
        with own_relay({"MAX_WAITING": "1"}) as (port, url):
            # only a room a key holder opened before holds anyone: a new one says "not open yet"
            launch(A, room="waitroom", tag="k11", create_key="k", relay=url)
            must("a key holder opens the room", until(lambda: connected("k11"), BOOT), log("k11")[-300:])
            stop("k11")
            must("which closes when it leaves", until(lambda: relay_event("left", "waitroom", port)), relay_log(port)[-300:])
            launch("nodeB", room="waitroom", tag="k7", create_key="", relay=url)
            until(lambda: "not open yet" in log("k7"), BOOT)
            launch("nodeC", room="waitroom", tag="k8", create_key="", relay=url)
            until(lambda: "503" in log("k8"), BOOT)
            ok("the second is refused with 503 and keeps trying", "503" in log("k8") and nodes["k8"]["proc"].poll() is None, log("k8")[-300:])
            launch(A, room="waitroom", tag="k9", create_key="k", relay=url)
            ok("both get in once the key holder opens it", until(lambda: all(connected(n) for n in ("k7", "k8", "k9")), 40), str([connected(n) for n in ("k7", "k8", "k9")]))
            for t in ("k7", "k8", "k9"):
                stop(t)

    @case("[R20] a member that stops answering pings is dropped and the room hears it left")
    def _():
        with own_relay() as (port, url):
            launch(A, room="deafroom", tag="d1", relay=url)
            until(lambda: connected("d1"), BOOT)
            deaf = socket.create_connection(("127.0.0.1", port))
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

    run_cases()


def gui_suite():
    global BUS
    H, G, KEY = "guiHost", "guiGuest", "e2ekey"
    print("\n=== gui setup: two real GUIs, each in headless Chromium, through one relay ===")
    BUS = private_bus()
    relay = launch_relay({"CREATE_KEY": KEY})
    until(lambda: listening(PORTS["relay"]))
    launch_gui(H)
    launch_gui(G, portal=False)
    X = launch_gui("guiXvfb", browser="xvfb")

    def mpv_of(n):
        return child_mpv_socket(nodes[n]["proc"].pid)

    def read_all(n):
        """The page has handled every log line its GUI has, so the feed isn't half a burst."""
        return js(n, "api(`/log?from=${from}&run=${run}`).then(r => r.json()).then(j => j.run == run && !j.lines.length)")

    def joined(n):
        until(lambda: status(n).startswith("You're in"), BOOT)
        nodes[n]["sock"] = mpv_of(n)
        return status(n).startswith("You're in")

    header("[G1] the GUI opens its own app window at the phone-sized window")
    for n in (H, G):
        argv = launches(n)[0]
        ok(f"{n} opened {nodes[n]['url']} as an app", f"--app={nodes[n]['url']} " in argv, argv)
        ok(f"{n} asked for 560x940", "--window-size=560,940" in argv)
    until(lambda: js(H, '$("version-text").textContent') != "Checking the version…")
    ok("a dev build says so and offers no update", js(H, '$("version-text").textContent == "Development build" && $("update").hidden'))

    header("[G1b] the window asks for no Google keys, and doesn't invite selecting or inspecting")
    ok("the browser is told there are no keys", open(f"{nodes[H]['home']}/browser.key").read().strip() == "no")
    ok("text can't be selected", js(H, 'getComputedStyle(document.body).userSelect') == "none")
    ok("but the invite can", js(H, 'getComputedStyle($("share-text")).userSelect') == "text")
    prevented = lambda target, event: js(H, f'!{target}.dispatchEvent(new {event})')
    ok("no right-click menu on the page", prevented("document.body", 'MouseEvent("contextmenu", {bubbles: true, cancelable: true})'))
    ok("but there is in a field, to paste", not prevented('$("invite")', 'MouseEvent("contextmenu", {bubbles: true, cancelable: true})'))
    for name, keys in (("F12", 'key: "F12"'), ("Ctrl+Shift+I", 'key: "I", ctrlKey: true, shiftKey: true'), ("Cmd+Alt+J", 'key: "j", metaKey: true, altKey: true'), ("Ctrl+U", 'key: "u", ctrlKey: true')):
        ok(f"{name} is blocked", prevented("document.body", f'KeyboardEvent("keydown", {{{keys}, bubbles: true, cancelable: true}})'))
    ok("Ctrl+V isn't", not prevented('$("invite")', 'KeyboardEvent("keydown", {key: "v", ctrlKey: true, bubbles: true, cancelable: true})'))
    js(H, "window.notReloaded = true")
    for key, code, vk, mods in (("F5", "F5", 116, 0), ("r", "KeyR", 82, 2), ("r", "KeyR", 82, 4), ("R", "KeyR", 82, 10)):
        for kind in ("rawKeyDown", "keyUp"):
            nodes[H]["page"].call("Input.dispatchKeyEvent", type=kind, key=key, code=code, windowsVirtualKeyCode=vk, nativeVirtualKeyCode=vk, modifiers=mods)
    time.sleep(QUIET)
    ok("F5, Ctrl+R, Cmd+R and Ctrl+Shift+R, pressed for real, don't reload it", js(H, "window.notReloaded") is True)

    header("[G2] the tabs switch views and the address remembers which")
    for view in ("history", "settings", "party"):
        tab(H, view)
        shown = js(H, '["party", "history", "settings"].filter(v => !$("view-" + v).hidden)')
        ok(f"{view} tab shows only its view", shown == [view], str(shown))
        ok(f"{view} tab sets the address", js(H, "location.hash") == ("" if view == "party" else "#" + view))

    header("[G3] Settings saves, and fills the party's relay")
    ok(f"{H} saved", save_settings(H, **{"set-relay": RELAY_URL, "createKey": KEY, "mpvFlags": MPVFLAGS}) == "Saved.")
    ok(f"{G} saved", save_settings(G, **{"set-relay": RELAY_URL, "mpvFlags": MPVFLAGS, "mpv": f"{nodes[G]['home']}/slow-mpv"}) == "Saved.")
    s = state(H)["settings"]
    ok("the settings reached the store", (s["relay"], s["createKey"], s["mpvFlags"]) == (RELAY_URL, KEY, MPVFLAGS), str(s))
    ok("the party's relay was filled in", js(H, '$("relay").value') == RELAY_URL)
    store = js(H, '$("store-path").textContent')
    ok("the store is where the page says", store == f"{nodes[H]['home']}/config/watchparty", store)
    ok("the settings are 0600 in a 0700 folder", (os.stat(f"{store}/gui.json").st_mode & 0o777, os.stat(store).st_mode & 0o777) == (0o600, 0o700))

    ok("the mpv path shows the one it will use", js(H, '$("mpv").placeholder') == f"{nodes[H]['home']}/bin/mpv" and js(H, 'document.querySelector("label[for=mpv]").textContent') == "mpv path", js(H, '$("mpv").placeholder'))
    ok("the cache folder shows the one it will use", js(H, '$("cacheDir").placeholder') == f"{nodes[H]['home']}/.cache/watchparty", js(H, '$("cacheDir").placeholder'))

    header("[G3b] the ? button opens the help, one topic at a time")
    ok("it starts closed", js(H, '$("tip").hidden'))
    click(H, "#tip-button")
    ok("a click opens it", js(H, '!$("tip").hidden && $("tip-button").getAttribute("aria-expanded") == "true"'))
    sections = js(H, '[...$("tip").querySelectorAll("h2")].map(h => h.textContent)')
    ok("each topic has its own section", sections == ["Start a party", "Join a party", "Watch together", "Automatic pauses", "Links", "Playlists", "Torrents", "Connecting"], str(sections))
    tip = js(H, '$("tip").textContent')
    ok("it isn't tied to one kind of video", not COUPLED.search(tip), str(COUPLED.findall(tip)))
    click(H, "#tip h2")
    ok("a click inside keeps it open", js(H, '!$("tip").hidden'))
    click(H, "#tip-button")
    ok("the button closes it", js(H, '$("tip").hidden'))
    click(H, "#tip-button")
    js(H, 'document.dispatchEvent(new KeyboardEvent("keydown", {key: "Escape"}))')
    ok("Escape closes it", js(H, '$("tip").hidden'))
    click(H, "#tip-button")
    click(H, "h1")
    ok("a click elsewhere closes it", js(H, '$("tip").hidden'))

    header("[G4] Browse uses the desktop portal, and says so when there is none")
    click(H, "[data-pick=file]")
    ok(f"{H} picked the video through the portal", until(lambda: js(H, '$("file").value') == MEDIA), js(H, '$("file").value'))
    click(H, "[data-pick=cacheDir]")
    ok(f"{H} picked a folder through it too", until(lambda: js(H, '$("cacheDir").value') == os.path.dirname(MEDIA)), js(H, '$("cacheDir").value'))
    js(H, '$("cacheDir").value = ""')
    click(H, "[data-pick=mpv]")
    ok(f"{H} can pick mpv, which no video filter would show", until(lambda: js(H, '$("mpv").value') == MEDIA + ".unfiltered"), js(H, '$("mpv").value'))
    js(H, '$("mpv").value = ""')
    click(G, "[data-pick=file]")
    until(lambda: js(G, '!$("error").hidden'))
    ok(f"{G}, with no portal, is told to type the path", js(G, '$("error").textContent').startswith("No file picker found"), js(G, '$("error").textContent'))

    header("[G5] the form refuses what can't start")
    fill(H, file="   ")
    click(H, "#go")
    ok("a blank path asks for a video", js(H, '$("error").textContent') == "Pick a video first.")
    fill(H, file=MEDIA, relay="")
    click(H, "#go")
    until(lambda: js(H, '$("error").textContent').startswith("Diagnosis"))
    ok("relay mode with no relay is refused", js(H, '$("error").textContent') == "Diagnosis: enter the relay address, or paste an invite.", js(H, '$("error").textContent'))
    ok("and nothing started", js(H, '$("vitals").hidden'))

    header("[G6] the host starts a relay party from the form and gets an invite")
    start(H, hostname=H, relay=RELAY_URL)
    must(f"{H} is in", joined(H), status(H))
    until(lambda: js(H, '$("share-text").textContent') != "")
    invite = js(H, '$("share-text").textContent')
    room, secret = invite.split("-room ", 1)[1].split()[0], invite.split("-secret ", 1)[1].split()[0]
    ok(f"{H} shows an invite", invite == f"-relay {RELAY_URL} -room {room} -secret {secret}", invite)
    ok(f"{H} hides the form and shows the invite box", js(H, '$("fields").hidden && !$("share").hidden'))
    ok("Update and Restart are off during a party", js(H, '$("update").disabled && $("restart").disabled && $("restart-now").disabled'))

    header("[G7] Copy invite puts it on the clipboard")
    click(H, "#copy")
    until(lambda: js(H, '$("copy").textContent') != "Copy invite")
    ok("the button says Copied", js(H, '$("copy").textContent') == "Copied", js(H, '$("copy").textContent'))
    ok("the clipboard has the invite", js(H, "navigator.clipboard.readText()") == invite)
    ok("the button comes back", until(lambda: js(H, '$("copy").textContent') == "Copy invite", 5))

    header("[G8] Save chart downloads the whole log")
    click(H, "#save-chart")
    dl = f"{nodes[H]['home']}/downloads"
    until(lambda: any(f.endswith(".log") for f in os.listdir(dl)))
    saved = [f for f in os.listdir(dl) if f.endswith(".log")]
    ok("one watchparty-<date>.log", len(saved) == 1 and saved[0].startswith("watchparty-"), str(saved))
    chart = open(f"{dl}/{saved[0]}").read() if saved else ""
    ok("it holds the party's log", "init done" in chart and invite in chart)

    header("[G9] the guest pastes the whole invite line and joins, after mpv takes its time")
    js(G, f'$("invite").value = {json.dumps("2026/11/01 12:00:00 your address to share is " + invite)}; '
          '$("invite").dispatchEvent(new Event("input"))')
    ok(f"{G} filled relay, room and secret from it", js(G, f'$("relay").value == {json.dumps(RELAY_URL)} && $("room").value == {json.dumps(room)} '
                                                      f'&& $("secret").value == {json.dumps(secret)} && !$("invite-hint").hidden'))
    start(G, hostname=G, file=MEDIA)
    ok(f"{G} said it is opening mpv", until(lambda: status(G).startswith("Opening mpv"), BOOT), status(G))
    ok(f"{G} walks while it waits", js(G, 'document.body.classList.contains("walking")'))
    must(f"{G} is in", joined(G), status(G))
    until(lambda: f"{G} has joined" in feed(H) and f"connected to {H}" in feed(G))
    ok(f"{H}'s feed says {G} joined", f"{G} has joined" in feed(H), str(feed(H)))
    ok(f"{G}'s feed says it connected", f"connected to {H}" in feed(G), str(feed(G)))

    header(f"[G10] a resume in {G}'s mpv: the feeds say who did it")
    must("the join is over", quiet((H, G)))
    setp(G, "pause", False)
    until(lambda: get(H, "pause") is False and f"resumed by {G}" in feed(H) and feed(G)[:1] == ["resumed by you"])
    ok(f"{H}'s mpv resumed", get(H, "pause") is False)
    ok(f"{H}'s feed credits {G}", f"resumed by {G}" in feed(H), str(feed(H)))
    ok(f"{G}'s feed says you", feed(G)[:1] == ["resumed by you"], str(feed(G)))

    header("[G11] a seek while playing pauses everyone, and the feeds say why")
    reasons = log(G).count('"name":"pause-reason"')
    ipc(nodes[G]["sock"], "seek", 30)
    # mpv's notice can land on either side of the pause, and the pause-reason that retracts
    # "paused by you" comes after it, so the feed is checked once the page has read them all
    until(lambda: get(H, "pause") is True and f"paused for {G}: seek" in feed(H) and "paused for everyone: seek" in feed(G)
          and log(G).count('"name":"pause-reason"') > reasons and read_all(G))
    ok(f"{H}'s feed says why", f"paused for {G}: seek" in feed(H), str(feed(H)))
    ok(f"{G}'s feed says why", "paused for everyone: seek" in feed(G), str(feed(G)))
    ok(f"{G}'s feed doesn't also say paused by you", "paused by you" not in feed(G), str(feed(G)))

    header(f"[G12] resuming after the seek shows up in {G}'s feed")
    setp(G, "pause", False)
    until(lambda: get(H, "pause") is False and feed(G)[:1] == ["resumed by you"])
    ok(f"{G}'s feed moved on from the seek", feed(G)[:1] == ["resumed by you"], str(feed(G)))

    header(f"[G13] a pause in {H}'s mpv")
    setp(H, "pause", True)
    until(lambda: get(G, "pause") is True and feed(H)[:1] == ["paused by you"] and f"paused by {H}" in feed(G))
    ok(f"{H}'s feed says you", feed(H)[:1] == ["paused by you"], str(feed(H)))
    ok(f"{G}'s feed credits {H}", f"paused by {H}" in feed(G), str(feed(G)))

    header(f"[G14] {H}'s history has the real room and secret, and who came")
    last = state(H)["history"][-1]
    ok("room and secret from the invite", (last.get("room"), last.get("secret")) == (room, secret), str(last))
    ok(f"{G} is in its people", last.get("people") == [G], str(last.get("people")))
    ok("the history and its secret are 0600 in their own file", os.stat(f"{store}/history.json").st_mode & 0o777 == 0o600 and secret in open(f"{store}/history.json").read())
    ok("the settings file has no party in it", secret not in open(f"{store}/gui.json").read() and "history" not in open(f"{store}/gui.json").read())

    header(f"[G15] {G} ends the party from the page, which pauses {H}")
    setp(H, "pause", False)
    must("the party is playing", until(lambda: get(G, "pause") is False and feed(H)[:1] == ["resumed by you"]))
    asked = end(G)
    ok("it asked first", asked == "End the party? mpv closes for you, and everyone else keeps watching.", str(asked))
    ok(f"{G} shows the form again", js(G, '!$("fields").hidden && $("go-label").textContent == "Start the party"'))
    ok(f"{G}'s mpv closed", until(lambda: not mpv_of(G)))
    ok(f"{H}'s feed says {G} left", until(lambda: f"{G} has left" in feed(H)), str(feed(H)))
    ok(f"{H} paused and says why", until(lambda: get(H, "pause") is True and f"paused for {G}: left" in feed(H)), str(feed(H)))
    ok(f"{H}'s feed doesn't blame {H}", feed(H)[0] != "paused by you", str(feed(H)))
    ok("Update and Restart are back on", js(G, '!$("update").disabled && !$("restart").disabled'))

    header("[G16] a missing mpv is diagnosed as one")
    save_settings(G, mpv="/nonexistent/mpv")
    go(G)
    until(lambda: status(G).startswith("Diagnosis"), BOOT)
    ok("it says mpv isn't installed", status(G).startswith("Diagnosis: mpv isn't installed"), status(G))
    ok("in red, with the chart open", js(G, '$("status").classList.contains("bad") && $("chart").open'))
    save_settings(G, mpv="")

    header("[G17] a wrong secret, and a different file, are diagnosed")
    fill(G, secret="wrong" + secret)
    go(G)
    until(lambda: status(G).startswith("Diagnosis"), BOOT)
    ok("the relay rejected the secret", status(G) == "Diagnosis: the relay rejected this room or secret. Check the invite and start again.", status(G))
    _, diff = variants()
    fill(G, secret=secret, file=diff)
    go(G)
    until(lambda: status(G).startswith("Diagnosis: your video"), BOOT)
    ok("the relay rejected the file", status(G) == NOT_THE_SAME_VIDEO, status(G))
    ok(f"{G}'s party ended", until(lambda: js(G, '!$("fields").hidden')))

    header("[G17b] opening a new party without the create key is diagnosed, not left waiting")
    fill(G, file=MEDIA, room="", secret="")
    go(G)
    until(lambda: status(G).startswith("Diagnosis"), BOOT)
    ok("it asks for the create key", status(G) == "Diagnosis: this relay needs a create key to open a new party. Add it in Settings and start again.", status(G))
    ok(f"{G}'s party ended", until(lambda: js(G, '!$("fields").hidden')))

    header("[G17c] a relay that is rate limiting, or full, says so instead of \"Lost the relay\"")
    busy = {"status": 429}

    class Busy(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(busy["status"])
            self.send_header("Content-Length", "0")
            self.end_headers()

        def log_message(self, *a):
            pass
    refusing = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Busy)
    threading.Thread(target=refusing.serve_forever, daemon=True).start()
    fill(G, file=MEDIA, relay=f"ws://127.0.0.1:{refusing.server_address[1]}", room="busy", secret="busy")
    go(G)
    ok("a rate limit is named", until(lambda: status(G) == "This relay has had too many attempts from your network. Trying again; it clears within the hour.", BOOT), status(G))
    busy["status"] = 503
    ok("so is a full relay", until(lambda: status(G) == "The relay is full right now. Trying again…", BOOT), status(G))
    end(G)
    ok(f"{G}'s party ended", until(lambda: status(G) == "The party has ended."), status(G))
    refusing.shutdown()
    fill(G, relay=RELAY_URL)

    header("[G17d] a magnet link nobody seeds says so, and Stop still works")
    fill(G, file="magnet:?xt=urn:btih:" + "ab" * 20 + "&dn=The.Video&tr=udp://tracker.invalid/announce", room="e2etorrent", secret="e2etorrent")
    go(G)
    until(lambda: status(G).startswith("Looking"), BOOT)
    ok("it says it is looking", status(G) == "Looking for this torrent's peers. It can take a while…", status(G))
    ok("and started no mpv", not mpv_of(G))
    end(G)
    ok("Stop ends it with no diagnosis", until(lambda: status(G) == "The party has ended."), status(G))
    chosen = f"{RUN}/guichosen"
    ok("the cache folder saves", save_settings(G, cacheDir=chosen) == "Saved.")
    ok("and reaches the store", state(G)["settings"]["cacheDir"] == chosen, str(state(G)["settings"]))
    go(G)
    ok("a party uses it", until(lambda: glob.glob(f"{chosen}/torrent-*")), str(glob.glob(f"{chosen}/*")))
    end(G)
    ok("and leaving empties it", glob.glob(f"{chosen}/torrent-*") == [], str(glob.glob(f"{chosen}/*")))
    ok("emptying the setting saves", save_settings(G, cacheDir="") == "Saved." and state(G)["settings"]["cacheDir"] == "")
    ok("history shows its name", js(G, 'fileName($("file").value)') == "The.Video", js(G, 'fileName($("file").value)'))

    header("[G17c] the check mark loads the file's folder, and starts on that file")
    folder = videos_in(f"{RUN}/guifolder", ("e1.mkv", "e2.mkv", "e3.mkv"))
    js(G, '$("playlist").checked = true')
    start(G, file=f"{folder}/e2.mkv", room="e2eplaylist", secret="e2eplaylist")
    until(lambda: mpv_of(G), BOOT)
    nodes[G]["sock"] = mpv_of(G)
    ok("it opens the picked file", until(lambda: get(G, "filename") == "e2.mkv"), get(G, "filename"))
    ok("with its neighbours", get(G, "playlist-count") == 3, get(G, "playlist-count"))
    ok("history remembers it", state(G)["history"][-1].get("playlist") is True, str(state(G)["history"][-1]))
    groups = lambda: nodes[G]["page"].feed_groups()
    def showing(*names):
        return lambda: [g[0] for g in groups()] == list(names) and [g[1] for g in groups()] == [True] + [False] * (len(names) - 1)
    videos = lambda: state(G)["history"][-1].get("videos", [])
    ok("the feed opens on that file", until(showing("e2.mkv")), str(groups()))
    ok("history has it from when it opened", [v["name"] for v in videos()] == ["e2.mkv"], str(videos()))
    ipc(nodes[G]["sock"], "playlist-next")
    ok("the next file gets its own group, and only it is open", until(showing("e3.mkv", "e2.mkv")), str(groups()))
    ipc(nodes[G]["sock"], "playlist-prev")
    ok("going back reopens that file's group", until(showing("e2.mkv", "e3.mkv")), str(groups()))
    ok("every switch is its own entry", until(lambda: [v["name"] for v in videos()] == ["e2.mkv", "e3.mkv", "e2.mkv"]), str(videos()))
    ok("each was left when the next opened", all(a.get("ended") == b["started"] for a, b in zip(videos(), videos()[1:])), str(videos()))
    ok("the open one hasn't been left", "ended" not in videos()[-1], str(videos()))
    end(G)
    ok("ending the party finishes it", videos()[-1].get("ended") == state(G)["history"][-1]["ended"], str(videos()))
    tab(G, "history")
    until(lambda: js(G, '$("history-list").querySelector(".videos") !== null'))
    shown = js(G, '$("history-list").firstElementChild.textContent')
    ok("the History page names the folder by its full path, counts files rather than plays, and shows each with its times", f"{folder} (2 videos)" in shown and "e3.mkv" in shown and " to " in shown, shown)
    title = lambda file, playlist=True: js(G, f'partyTitle({{file: {json.dumps(file)}, playlist: {json.dumps(playlist)}}}, 2)')
    ok("a torrent has no folder of its own, so it's named for itself", (title("magnet:?xt=urn:btih:x&dn=The.Pack"), title("/d/pack.torrent")) == ("The.Pack (2 videos)", "/d/pack.torrent (2 videos)"), str((title("magnet:?xt=urn:btih:x&dn=The.Pack"), title("/d/pack.torrent"))))
    ok("a Windows path works, and a path with no folder just counts", (title("C:\\v\\trip\\a.mkv"), title("a.mkv")) == ("C:\\v\\trip (2 videos)", "2 videos"), str((title("C:\\v\\trip\\a.mkv"), title("a.mkv"))))
    ok("one file is its own full path", js(G, 'partyTitle({file: "/v/a.mkv"}, 1)') == "/v/a.mkv", js(G, 'partyTitle({file: "/v/a.mkv"}, 1)'))
    tab(G, "party")
    line = lambda m, at="18:00:00": f"2026/11/05 {at} {m}"
    notice = lambda text, at="18:00:00": line("do: sent request: " + json.dumps({"command": ["show-text", text, 5000]}, separators=(",", ":")), at)
    js(G, f'handle({json.dumps(notice("paused by Sam"))}); handle({json.dumps(notice("from e2\nsecond line.mkv to e1.mkv, by Sam"))}); handle({json.dumps(notice("synced by Sam"))}); handle({json.dumps(notice("paused for Sam: video changed"))}); handle({json.dumps(line("now playing: " + json.dumps("e1.mkv")))})')
    ok("a move's notices land with the file it moved to, in order, whichever came first", groups()[0][0] == "e1.mkv" and groups()[0][2] == ["paused for Sam: video changed", "synced by Sam", "from e2\nsecond line.mkv to e1.mkv, by Sam"], str(groups()))
    ok("and the rest stays behind", "paused by Sam" in groups()[1][2], str(groups()))
    js(G, f'handle({json.dumps(line("now playing: " + json.dumps("e3.mkv"), "18:00:10"))})')
    ok("a later move leaves the notices of the one before", groups()[1][0] == "e1.mkv" and len(groups()[1][2]) == 3, str(groups()))
    js(G, f'handle({json.dumps(notice("synced by Noor", "18:00:20"))}); handle({json.dumps(line("now playing: " + json.dumps("e2.mkv"), "18:00:20"))})')
    ok("a seek on its own stays with the file it was on", groups()[1][0] == "e3.mkv" and "synced by Noor" in groups()[1][2], str(groups()))
    row = '[...$("feed").querySelectorAll("li")].find(li => li.lastChild.textContent == "synced by Noor")'
    got = js(G, f'[+{row}.dataset.at, {row}.querySelector("time").textContent]')
    want = js(G, '[+new Date(2026, 10, 5, 18, 0, 20), clock(new Date(2026, 10, 5, 18, 0, 20))]')
    ok("a row's moment and time are the log line's own", got == want, str((got, want)))
    stops = json.dumps(line('Watch: broadcasted event: {"name":"pause","data":"yes"}', "18:01:00"))
    js(G, f'handle({json.dumps(notice("paused for Sam: catching up", "18:01:00"))}); handle({stops})')
    ok("stopping for someone who fell behind is one line, not also paused by you", feed(G)[0] == "paused by you: catching up" and not {"paused by you", "paused for Sam: catching up"} & set(feed(G)), str(feed(G)[:3]))
    js(G, f'handle({json.dumps(notice("paused for Sam: catching up", "18:01:10"))}); handle({json.dumps(notice("resumed by Sam", "18:01:20"))}); handle({stops})')
    ok("a party already stopped keeps the notice, and a later pause is your own", feed(G)[:3] == ["paused by you", "resumed by Sam", "paused for Sam: catching up"], str(feed(G)[:4]))
    for why, text in (("joining", "paused for Sam: joining"), ("out of sync", "paused: out of sync with Sam")):
        js(G, f'handle({json.dumps(notice(text, "18:01:30"))}); handle({stops})')
        ok(f"stopping for someone {why} is one line too", feed(G)[0] == f"paused by you: {why}" and text not in feed(G), str(feed(G)[:3]))
    js(G, '$("playlist").checked = false')

    header("[G18] a guest who comes first waits for the host, then gets in by itself")
    end(H)
    start(G, file=MEDIA, room="e2ewait", secret="e2ewaitsecret")
    until(lambda: status(G).startswith("Waiting"), BOOT)
    ok(f"{G} is waiting for the host", status(G) == "Waiting for the host to open this room. It starts by itself once they're in.", status(G))
    start(H, room="e2ewait", secret="e2ewaitsecret")
    must(f"{H} opened it", joined(H), status(H))
    must(f"{G} got in", joined(G), status(G))

    header("[G19] losing the relay shows on both, both play on, and both come back")
    must(f"{G}'s join is over", until(lambda: f"paused for {G}: joining" in feed(H)) and quiet([H, G]))
    setp(H, "pause", False)
    must("the party is playing", until(lambda: get(H, "pause") is False and get(G, "pause") is False))
    relay.kill(); relay.wait(timeout=10)
    until(lambda: all(status(n).startswith("Lost the relay") for n in (H, G)))
    for n in (H, G):
        ok(f"{n} says it is reconnecting", status(n) == "Lost the relay. Reconnecting…", status(n))
        ok(f"{n} plays on, and its feed says so", until(lambda: "lost the relay, reconnecting" in feed(n) and read_all(n))
           and get(n, "pause") is False, str(feed(n)[:3]))
    relay = launch_relay({"CREATE_KEY": KEY})
    until(lambda: all(status(n).startswith("You're in") for n in (H, G)), 40)
    for n in (H, G):
        must(f"{n} is back in", status(n).startswith("You're in"), status(n))
    must("the returns are over", quiet([H, G]))
    ok("nobody stopped", all(get(n, "pause") is False for n in (H, G)), str([feed(n)[:2] for n in (H, G)]))
    for n in (G, H):
        end(n)

    header("[G19b] an invite ticks the box, so a playlist party can't be missed by accident")
    fill(H, room="", secret="")
    js(H, '$("playlist").checked = true')
    start(H, file=f"{folder}/e2.mkv")
    must(f"{H} is in", joined(H), status(H))
    until(lambda: js(H, '$("share-text").textContent'))
    invite = js(H, '$("share-text").textContent')
    ok("the invite asks for the playlist", invite.endswith(" -playlist"), invite)
    relay_addr = js(G, '$("relay").value')
    def paste(text):
        js(G, f'$("invite").value = {json.dumps(text)}; $("invite").dispatchEvent(new Event("input"))')
    paste(invite)
    ok(f"pasting it ticks {G}'s box", js(G, '$("playlist").checked'))
    start(G, file=f"{folder}/e1.mkv")
    must(f"{G} is in", joined(G), status(G))
    ok(f"{G} goes to {H}'s entry", until(lambda: get(G, "filename") == "e2.mkv"), get(G, "filename"))
    setp(H, "pause", False)
    must("they play", until(lambda: get(G, "pause") is False))
    ipc(nodes[H]["sock"], "playlist-next")
    top = lambda n: (nodes[n]["page"].feed_groups() or [[None, None, []]])[0]
    ok(f"{G}'s real notices about the move are with the file it moved to", until(lambda: top(G)[0] == "e3.mkv" and {f"from e2.mkv to e3.mkv, by {H}", f"paused for {H}: video changed"} <= set(top(G)[2])), str(nodes[G]["page"].feed_groups()))
    ok(f"so are {H}'s", until(lambda: top(H)[0] == "e3.mkv" and "paused for everyone: video changed" in top(H)[2]), str(nodes[H]["page"].feed_groups()))
    end(G)
    ok(f"{G}'s history has every file it was on", [v["name"] for v in state(G)["history"][-1].get("videos", [])] == ["e1.mkv", "e2.mkv", "e3.mkv"], str(state(G)["history"][-1]))
    js(G, '$("playlist").checked = false')
    fill(G, file=f"{folder}/e2.mkv")
    go(G)
    until(lambda: status(G).startswith("Diagnosis"), BOOT)
    ok("with the box unticked, it says to tick it", status(G) == NOT_THE_SAME_VIDEO, status(G))
    short = f"{RUN}/guishort"
    shutil.copytree(folder, short)
    os.remove(f"{short}/e3.mkv")
    js(G, '$("playlist").checked = true')
    fill(G, file=f"{short}/e2.mkv")
    go(G)
    until(lambda: status(G).startswith("Diagnosis: this folder"), BOOT)
    ok("with another folder, it says the folders differ", status(G) == NOT_THE_SAME_FOLDER, status(G))
    paste("-relay ws://example.invalid -room r -secret s")
    ok("an invite with no playlist unticks it", not js(G, '$("playlist").checked'))
    fill(G, relay=relay_addr, room="", secret="", file=MEDIA)
    js(G, '$("invite").value = ""')
    end(H)
    js(H, '$("playlist").checked = false')
    fill(H, file=MEDIA)

    header("[G19c] Continue: the same room again, each from where it stopped (videos are 12s), then the key holder's spot")
    for n in (H, G):
        js(n, '$("playlist").checked = true')
    start(H, file=f"{folder}/e1.mkv", room="e2econt", secret="e2econtsecret")
    must(f"{H} is in", joined(H), status(H))
    start(G, file=f"{folder}/e1.mkv", room="e2econt", secret="e2econtsecret")
    must(f"{G} is in", joined(G), status(G))
    must("the join is over", quiet((H, G)))
    setp(H, "playlist-pos", 1)
    must(f"{G} followed to e2.mkv", until(lambda: get(G, "filename") == "e2.mkv"), get(G, "filename"))
    must("the move is over", quiet((H, G)))
    setp(H, "time-pos", 3)
    must(f"{G} followed to 3s", until(lambda: near(get(G, "time-pos"), 3, 0.2)), get(G, "time-pos"))
    end(G)
    setp(H, "time-pos", 6)
    until(lambda: near(get(H, "time-pos"), 6, 0.2))
    end(H)
    parties = len(state(H)["history"])
    ok("each remembers where it stopped", [state(n)["history"][-1].get("stopped") for n in (H, G)] == [{"entry": "e2.mkv", "at": 6}, {"entry": "e2.mkv", "at": 3}], str([state(n)["history"][-1].get("stopped") for n in (H, G)]))
    def press_continue(n):
        tab(n, "history")
        run = js(n, "run")
        click(n, "#history-list li .continue")
        until(lambda: js(n, "run") > run, BOOT)
    press_continue(G)
    ok(f"{G}, with no key, waits for the key holder to open the room", until(lambda: status(G).startswith("Waiting"), BOOT), status(G))
    ok("Continue can't be pressed during a party", js(G, '[...$("history-list").querySelectorAll(".continue")].every(b => b.disabled)'))
    press_continue(H)
    must(f"{H} is back in", joined(H), status(H))
    ok(f"{H} opens on e2.mkv at 6s", until(lambda: get(H, "filename") == "e2.mkv" and near(get(H, "time-pos"), 6, 0.2)), f'{get(H, "filename")} {get(H, "time-pos")}')
    ok("the same party moved to the top, not a new one", len(state(H)["history"]) == parties and state(H)["history"][-1]["room"] == "e2econt" and "ended" not in state(H)["history"][-1], str(state(H)["history"][-1]))
    must(f"{G} got in by itself", joined(G), status(G))
    ok(f"{G} takes the key holder's spot, not its own 3s", until(lambda: get(G, "filename") == "e2.mkv" and near(get(G, "time-pos"), 6, 0.2)), f'{get(G, "filename")} {get(G, "time-pos")}')
    must("the join is over", quiet((H, G)))
    ok(f"{H} stayed there", near(get(H, "time-pos"), 6, 0.2), get(H, "time-pos"))
    setp(H, "playlist-pos", 2)
    ok("the next file starts at its beginning", until(lambda: all(get(n, "filename") == "e3.mkv" and near(get(n, "time-pos"), 0, 1) for n in (H, G))), str([get(n, "time-pos") for n in (H, G)]))
    must("the move is over", quiet((H, G)))
    setp(H, "time-pos", 9)
    must(f"{G} followed to 9s", until(lambda: near(get(G, "time-pos"), 9, 0.2)), get(G, "time-pos"))
    setp(H, "pause", False)
    for n in (H, G):
        ok(f"{n}'s party ends with its last file", until(lambda: status(n) == "The party has ended.", BOOT), status(n))
    ok("both know it was played to the end", all(state(n)["history"][-1].get("finished") for n in (H, G)), str([state(n)["history"][-1] for n in (H, G)]))
    tab(H, "history")
    ok("so it has no Continue", js(H, '$("history-list").firstElementChild.querySelector(".continue") === null'))
    ago = lambda days: js(H, f'continuable({{mode: "relay", room: "r", secret: "s", ended: new Date(Date.now() - {days} * 864e5).toISOString()}})')
    ok("Continue is gone once the relay has forgotten the room, 30 days after", (ago(29), ago(31)) == (True, False), str((ago(29), ago(31))))
    tab(H, "party")
    for n in (G, H):
        js(n, '$("playlist").checked = false')
    fill(H, file=MEDIA, room="", secret="")
    fill(G, file=MEDIA, room="", secret="")

    header("[G19d] Continue a torrent: the same file of it, by the name mpv reported, at the same second")
    pack = f"{RUN}/guipack"
    os.makedirs(pack)
    for name in ("e1.mkv", "Part 2 #b.mkv"):
        shutil.copy(MEDIA, f"{pack}/{name}")
    start(H, file=seeder(pack), room="e2etorrent", secret="e2etorrentsecret")
    must(f"{H} is in", until(lambda: status(H).startswith("You're in"), 60) and joined(H), status(H))
    setp(H, "playlist-pos", 1)
    must("it moved to the second file", until(lambda: get(H, "filename") == "Part 2 #b.mkv"), get(H, "filename"))
    setp(H, "time-pos", 50)
    until(lambda: near(get(H, "time-pos"), 50, 0.2))
    end(H)
    press_continue(H)
    must(f"{H} is back in", until(lambda: status(H).startswith("You're in"), 60) and joined(H), status(H))
    ok("it opens that file at 50s", until(lambda: get(H, "filename") == "Part 2 #b.mkv" and near(get(H, "time-pos"), 50, 0.2)), f'{get(H, "filename")} {get(H, "time-pos")}')
    end(H)
    fill(H, file=MEDIA, room="", secret="")

    header("[G19e] with keep-open=always mpv stays on a file it finished, and Continue starts the next one")
    fill(H, mpvFlags=f"{MPVFLAGS} --keep-open=always")
    js(H, '$("playlist").checked = true')
    start(H, file=f"{folder}/e1.mkv", room="e2ekeep", secret="e2ekeepsecret")
    must(f"{H} is in", joined(H), status(H))
    setp(H, "time-pos", 9)
    until(lambda: near(get(H, "time-pos"), 9, 0.2))
    setp(H, "pause", False)
    must("it plays to the end of e1.mkv and stays there", until(lambda: get(H, "pause") is True and (get(H, "time-pos") or 0) > 11 and get(H, "filename") == "e1.mkv"), f'{get(H, "filename")} {get(H, "time-pos")}')
    end(H)
    last = state(H)["history"][-1]
    ok("it continues from the next file, which nobody opened yet", last.get("stopped") == {"entry": "e2.mkv"} and not last.get("finished") and [v["name"] for v in last["videos"]] == ["e1.mkv"], str(last))
    press_continue(H)
    must(f"{H} is back in", joined(H), status(H))
    ok("and opens it at its start", until(lambda: get(H, "filename") == "e2.mkv" and near(get(H, "time-pos"), 0, 1)), f'{get(H, "filename")} {get(H, "time-pos")}')
    end(H)
    js(H, '$("playlist").checked = false')
    fill(H, file=MEDIA, room="", secret="", mpvFlags=MPVFLAGS)

    header("[G20] a peer-to-peer party: the invite switches the guest to Peer-to-Peer")
    js(H, 'setMode("direct"); $("addrs").value = ""; $("psecret").value = ""')
    start(H, port=str(PORTS["wifiHost"]))
    must(f"{H} is in", joined(H), status(H))
    until(lambda: "-addrs" in js(H, '$("share-text").textContent'))
    wifi = js(H, '$("share-text").textContent')
    addr, wsecret = wifi.split()[1], wifi.split()[3]
    ok(f"{H}'s invite has its Wi-Fi address", wifi.startswith("-addrs ") and addr.endswith(f":{PORTS['wifiHost']}"), wifi)
    header(f"[G20b] a stranger with no secret can't rewrite {H}'s invite or status through its log")
    before = status(H), state(H)["history"][-1]
    for forged in ("relay: disconnected", "relay: rejected by relay", "your address to share is -addrs 6.6.6.6:1 -secret FORGED"):
        code = raw(f"127.0.0.1:{PORTS['wifiHost']}", "/hi", {"hit-me-up": forged, "secret": "wrong"})
        ok(f"{forged!r} is refused", code in (401, 403), f"HTTP {code}")
    until(lambda: "-secret FORGED" in js(H, '$("log").textContent'))
    ok("the page has read the forged line", "-secret FORGED" in js(H, '$("log").textContent'))
    ok("the invite is unchanged", js(H, '$("share-text").textContent') == wifi, js(H, '$("share-text").textContent'))
    ok("the status is unchanged", status(H) == before[0], status(H))
    ok("the saved session is unchanged", state(H)["history"][-1] == before[1], str(state(H)["history"][-1]))

    js(G, f'$("invite").value = {json.dumps(wifi)}; $("invite").dispatchEvent(new Event("input"))')
    ok(f"{G} switched to Peer-to-Peer and filled it in", js(G, f'mode() == "direct" && $("addrs").value == {json.dumps(addr)} && $("psecret").value == {json.dumps(wsecret)}'))
    _, diff = variants()
    start(G, file=diff, port=str(PORTS["wifiGuest"]))
    until(lambda: status(G).startswith("Diagnosis"), BOOT)
    ok("over Wi-Fi, another file gets the same diagnosis", status(G) == NOT_THE_SAME_VIDEO, status(G))
    fill(G, file=MEDIA)
    start(G, port=str(PORTS["wifiGuest"]))
    must(f"{G} is in", joined(G), status(G))
    until(lambda: f"{G} has joined" in feed(H) or f"connected to {G}" in feed(H))
    setp(H, "pause", False)
    until(lambda: get(G, "pause") is False and f"resumed by {H}" in feed(G))
    ok(f"{G} follows {H} over Wi-Fi", get(G, "pause") is False and f"resumed by {H}" in feed(G), str(feed(G)))

    header(f"[G21] quitting mpv ends {G}'s party without a diagnosis")
    ipc(nodes[G]["sock"], "quit")
    until(lambda: not js(G, "running"))
    ok(f"{G} says it ended", status(G) == "The party has ended.", status(G))
    ok(f"{H}'s feed says {G} left", until(lambda: f"{G} has left" in feed(H)), str(feed(H)))

    header(f"[G22] SIGTERM ends {H} mid-party, mpv, window and all")
    sock = nodes[H]["sock"]
    window = next(c for c in children(nodes[H]["proc"].pid) if b"--remote-debugging-port" in open(f"/proc/{c}/cmdline", "rb").read())
    nodes[H]["proc"].send_signal(signal.SIGTERM)
    ok(f"{H} exited", until(lambda: nodes[H]["proc"].poll() is not None, 20))
    ok(f"{H}'s mpv closed", not listening_unix(sock))
    ok(f"{H}'s window closed", until(lambda: gone(window)))
    ok("and its browser profile is deleted", not os.path.exists(profile(H)), profile(H))

    header(f"[G23] {G}'s history: every party, Watch again peer-to-peer, Continue over the internet")
    tab(G, "history")
    until(lambda: js(G, '$("history-list").children.length') == len(state(G)["history"]))
    items = js(G, '[...$("history-list").children].map(li => li.textContent)')
    ok("newest first, a peer-to-peer party with the host", "peer-to-peer" in items[0] and f"with {H}" in items[0], items[0])
    ok("the first is over the internet with the host", "over the internet" in items[-1] and f"with {H}" in items[-1], items[-1])
    buttons = lambda li: js(G, f'[...$("history-list").{li}.querySelectorAll("button")].map(b => b.textContent)')
    ok("a peer-to-peer party has Watch again alone", buttons("firstElementChild") == ["Watch again"], str(buttons("firstElementChild")))
    ok("a party over the internet has Continue alone", buttons("lastElementChild") == ["Continue"], str(buttons("lastElementChild")))
    click(G, "#history-list li .again")
    ok("Watch again fills Peer-to-Peer and shows the form", js(G, f'mode() == "direct" && $("addrs").value == {json.dumps(addr)} && $("psecret").value == {json.dumps(wsecret)} && !$("view-party").hidden'))

    header(f"[G24] closing {G}'s window ends its GUI")
    browser = next(c for c in children(nodes[G]["proc"].pid) if b"--remote-debugging-port" in open(f"/proc/{c}/cmdline", "rb").read())
    os.kill(browser, signal.SIGTERM)
    ok(f"{G} exited", until(lambda: nodes[G]["proc"].poll() is not None))
    ok(f"{G} logged it", "gui: window closed" in log(G))

    header(f"[G25] {G} again, on the same folder: everything was kept")
    count = len(items)
    G2 = launch_gui(G, tag="guiGuest2", portal=False)
    ok("a new port", nodes[G2]["url"] != nodes[G]["url"])
    ok("the name, relay and Wi-Fi port are filled in", js(G2, f'$("hostname").value == {json.dumps(G)} && $("relay").value == {json.dumps(RELAY_URL)} '
                                                         f'&& $("port").value == {json.dumps(str(PORTS["wifiGuest"]))} && $("local").checked'))
    tab(G2, "settings")
    ok("the settings are kept", js(G2, f'$("mpvFlags").value == {json.dumps(MPVFLAGS)} && $("set-relay").value == {json.dumps(RELAY_URL)}'))
    tab(G2, "history")
    ok("the history is kept", until(lambda: js(G2, '$("history-list").children.length') == count))

    header("[G26] Clear history asks, then empties it")
    click(G2, "#clear-history")
    until(lambda: js(G2, '$("history-list").children.length') == 0)
    ok("it asked first", js(G2, "window.asked") == "Clear the whole party history?")
    ok("the list is empty and says so", js(G2, '!$("history-empty").hidden && $("clear-history").hidden'))
    ok("the store is empty", not state(G2)["history"])

    header("[G27] the cat")
    js(G2, 'document.querySelector(".cat").dispatchEvent(new MouseEvent("click", {bubbles: true}))')
    ok("petting it makes a heart", js(G2, 'document.querySelectorAll(".heart").length') == 1)

    header(f"[G27a] a reload keeps {G2} up; closing its window ends it, even with the browser still running, as on macOS")
    nodes[G2]["page"].call("Page.reload")
    time.sleep(3 + QUIET)  # the GUI's closeGrace
    ok("the reload didn't end it", nodes[G2]["proc"].poll() is None and "gui: window closed" not in log(G2), log(G2).strip())
    try:
        nodes[G2]["page"].call("Page.close")
    except (OSError, RuntimeError, ValueError):
        pass  # the page can go before it answers
    ok(f"{G2} exited", until(lambda: nodes[G2]["proc"].poll() is not None, 10))
    ok(f"{G2} logged it", "gui: window closed" in log(G2), log(G2).strip())
    ok("and its browser profile is deleted", not os.path.exists(profile(G2)), profile(G2))

    header("[G27b] a history saved by an older version moves to its own file, and is kept")
    folder = f"{nodes[G]['home']}/config/watchparty"
    os.remove(f"{folder}/history.json")
    saved = json.load(open(f"{folder}/gui.json"))
    saved["history"] = [{"started": "2026-01-02T03:04:05Z", "ended": "2026-01-02T04:04:05Z", "file": "/m/from-before.mkv", "mode": "direct", "people": ["Sam"]}]
    json.dump(saved, open(f"{folder}/gui.json", "w"))
    G3 = launch_gui(G, tag="guiGuest3", portal=False)
    tab(G3, "history")
    ok("the page shows it", until(lambda: "from-before.mkv" in js(G3, '$("history-list").textContent')))
    ok("it moved", "from-before.mkv" in open(f"{folder}/history.json").read() and "history" not in open(f"{folder}/gui.json").read())
    ok("the settings stayed", json.load(open(f"{folder}/gui.json"))["settings"] == saved["settings"])
    stop(G3)

    header("[G28] self-update: offered, refused on a bad digest, installed on a good one")
    rel = Release(PORTS["release"], RUN + "/gui-v0.0.2", "watchparty-gui_" + subprocess.run(["go", "env", "GOARCH"], capture_output=True, text=True).stdout.strip())
    os.makedirs(f"{RUN}/upd")
    exe = f"{RUN}/upd/watchparty-gui"
    shutil.copy(RUN + "/gui-v0.0.1", exe)
    old = sha(exe)
    U = launch_gui("guiUpdate", exe=exe)
    tab(U, "settings")
    until(lambda: js(U, '!$("update").hidden'))
    ok("it offers v0.0.2", js(U, '$("version-text").textContent == "Version v0.0.1" && $("update").textContent == "Update to v0.0.2" && !$("update-dot").hidden'))
    click(U, "#update")
    until(lambda: js(U, '$("version-text").textContent').startswith("Update failed"))
    ok("a bad digest is refused", js(U, '$("version-text").textContent') == "Update failed: the download does not match its published sha256, not installing it.", js(U, '$("version-text").textContent'))
    ok("and the binary is untouched", sha(exe) == old)
    ok("and no download is left behind", os.listdir(f"{RUN}/upd") == ["watchparty-gui"], str(os.listdir(f"{RUN}/upd")))
    rel.digest = "sha256:" + rel.sha
    click(U, "#update")
    until(lambda: js(U, '$("restart-dialog").open'))
    ok("a good one is installed", sha(exe) == rel.sha)
    ok("and it asks to restart", js(U, '$("restart-text").textContent') == "Version v0.0.2 is installed. A restart is required to start using it.")
    click(U, "#restart-later")
    ok("Restart later closes the dialog and leaves the button", js(U, '!$("restart-dialog").open && !$("restart").hidden && $("update").hidden'))

    header("[G29] Restart now hands over to the new version")
    click(U, "#restart")
    ok("the old one exited", until(lambda: nodes[U]["proc"].poll() is not None))
    attach(U, 2)
    until(lambda: js(U, '$("version-text").textContent') != "Checking the version…")
    ok("the new window runs v0.0.2", js(U, '$("version-text").textContent') == "Version v0.0.2", js(U, '$("version-text").textContent'))
    ok("and offers nothing", js(U, '$("update").hidden && $("restart").hidden'))

    header("[G29b] the cli updates itself the same way, with its own asset")
    rel = Release(PORTS["cliRelease"], RUN + "/cli-v0.0.2", "watchparty_" + subprocess.run(["go", "env", "GOARCH"], capture_output=True, text=True).stdout.strip())
    os.makedirs(f"{RUN}/updcli")
    exe = f"{RUN}/updcli/watchparty"
    shutil.copy(RUN + "/cli-v0.0.1", exe)
    old = sha(exe)
    res = subprocess.run([exe, "-update"], capture_output=True, text=True, timeout=BOOT)
    ok("a bad digest is refused", res.returncode != 0 and "does not match its published sha256" in res.stderr, res.stderr)
    ok("and the binary is untouched", sha(exe) == old)
    ok("and no download is left behind", os.listdir(f"{RUN}/updcli") == ["watchparty"], str(os.listdir(f"{RUN}/updcli")))
    rel.digest = "sha256:" + rel.sha
    res = subprocess.run([exe, "-update"], capture_output=True, text=True, timeout=BOOT)
    ok("a good one is installed", res.returncode == 0 and sha(exe) == rel.sha, res.stderr)
    res = subprocess.run([exe, "-version"], capture_output=True, text=True, timeout=BOOT)
    ok("and it runs as v0.0.2", res.stdout.strip() == "v0.0.2", res.stdout)
    res = subprocess.run([exe, "-update"], capture_output=True, text=True, timeout=BOOT)
    ok("updating again says it's up to date, and isn't a failure", res.returncode == 0 and "already on the latest version" in res.stdout, res.stdout + res.stderr)

    header("[G30] a browser that hands the page off doesn't count as closing the window")
    O = launch_gui("guiHandoff", browser="handoff")
    until(lambda: launches(O))
    time.sleep(QUIET)
    ok("the GUI is still up", nodes[O]["proc"].poll() is None and listening(int(nodes[O]["url"].rsplit(":", 1)[1].strip("/"))))
    ok("and didn't log a closed window", "gui: window closed" not in log(O))
    stop(O)

    header("[G31] with no browser at all, it says where the page is")
    system = [d for d in ("/var/lib/flatpak/exports/share/applications", "/var/lib/snapd/desktop/applications")
              if os.path.isdir(d) and any(re.search("chrom|edge|brave|vivaldi", f, re.I) for f in os.listdir(d))]
    if system:
        print(f"  SKIP  a system-wide browser in {system} would open on screen")
    else:
        N = launch_gui("guiNoBrowser", browser=None)
        until(lambda: "no browser could be opened" in log(N))
        ok("it tried xdg-open", open(f"{nodes[N]['home']}/xdg-open.args").read().strip() == nodes[N]["url"])
        ok("and logged the address", f"gui: no browser could be opened (exit status 1), open {nodes[N]['url']} yourself" in log(N), log(N).strip())
        stop(N)

    header("[G32] window.close() from the page closes a real window, and the GUI with it")
    # headless Chromium ignores window.close() on an --app window; Restart now relies on it
    try:
        js(X, "window.close()")
    except (OSError, RuntimeError, ValueError):
        pass  # the page can go before it answers
    ok("the GUI exited", until(lambda: nodes[X]["proc"].poll() is not None))
    ok("on a closed window", "gui: window closed" in log(X), log(X).strip())
    install_suite()


def upnp_suite():
    global BUS
    print("\n=== upnp: a fake router on the LAN ===")
    BUS = private_bus()
    if not os.environ.get("E2E_NO_SANDBOX"):
        header("[G34] skipped outside the container: a real router on this LAN would answer and map ports")
    else:
        header("[G34] UPnP behind carrier NAT: the party starts, warns, and removes its mapping")
        gw = Gateway(PORTS["gateway"], "10.172.178.175")
        n = launch("upnpCarrier", port=PORTS["upnpNode"], local=False)
        must("it starts", until(lambda: booted(n), BOOT), log(n))
        ok("the mapping was added", "AddPortMapping" in gw.actions, str(gw.actions))
        ok("it warns", "unreachable address: 10.172.178.175 " in log(n), log(n))
        ok("and still prints the invite, for friends on the LAN", "your address to share is -addrs 10.172.178.175:" in log(n), log(n))
        stop(n)
        ok("quitting removes the mapping", "DeletePortMapping" in gw.actions and "upnp: port mapping deleted" in log(n), str(gw.actions))

        header("[G34b] a public address gets no warning")
        gw.ip = "203.0.113.7"
        n = launch("upnpPublic", port=PORTS["upnpNode"], local=False)
        must("it starts", until(lambda: booted(n), BOOT), log(n))
        ok("the invite has it", "your address to share is -addrs 203.0.113.7:" in log(n), log(n))
        ok("and no warning", "unreachable address" not in log(n), log(n))
        stop(n)

        header("[G34c] a gateway can't start a log line of its own through the address it reports")
        gw.ip = "10.0.0.1&#10;2026/01/01 00:00:00 FORGED"
        n = launch("upnpForged", port=PORTS["upnpNode"], local=False)
        must("it starts", until(lambda: booted(n), BOOT), log(n))
        ok("an address that isn't one warns too", "unreachable address: 10.0.0.1 2026/01/01" in log(n), log(n))
        ok("and nothing it sent starts a line", not [l for l in log(n).splitlines() if l.startswith("2026/01/01")], log(n))
        stop(n)

        header("[G34d] stopping during discovery doesn't wait out the search, and isn't an error")
        calls = len(gw.actions)
        n = launch("upnpStopped", port=PORTS["upnpNode"], local=False)
        must("it reached discovery", until(lambda: "file fingerprint" in log(n), BOOT), log(n))
        began = time.time()
        stop(n)
        took = time.time() - began
        ok("it stops well inside the 6.5s search", took < 4, f"{took:.1f}s")
        ok("with exit code 0", nodes[n]["proc"].returncode == 0, str(nodes[n]["proc"].returncode))
        ok("and no error, and nothing mapped", "canceled" not in log(n) and "AddPortMapping" not in gw.actions[calls:], log(n))

        header("[G35] the GUI says so too, and only for the party it's true of")
        gw.ip = "10.172.178.175"
        P = launch_gui("guiUpnp")
        ok(f"{P} saved", save_settings(P, mpvFlags=MPVFLAGS) == "Saved.", js(P, '$("settings-saved").textContent'))
        js(P, 'setMode("direct"); $("local").checked = false')
        start(P, hostname=P, file=MEDIA, port=str(PORTS["upnpNode"]))
        must("it starts", until(lambda: status(P).startswith("You're in"), BOOT), status(P))
        ok("the status warns", status(P) == "You're in, but friends outside this network can't reach this invite: your connection has no public address. Use a relay for them.", status(P))
        until(lambda: js(P, '$("share-text").textContent'))
        ok("and the invite still shows", js(P, '$("share-text").textContent').startswith("-addrs 10.172.178.175:"), js(P, '$("share-text").textContent'))
        end(P)
        gw.ip = "203.0.113.7"
        go(P)
        must("the next party starts", until(lambda: status(P).startswith("You're in"), BOOT), status(P))
        ok("with a public address it doesn't warn", status(P) == "You're in. Pause or seek in mpv and everyone follows.", status(P))
        end(P)

        header("[G35b] the GUI's Stop during discovery ends the party, without a diagnosis")
        go(P)
        began = time.time()
        end(P)
        took = time.time() - began
        ok("it says it ended", status(P) == "The party has ended.", status(P))
        ok("well inside the 6.5s search", took < 4, f"{took:.1f}s")
        stop(P)

    header("[G36] mpv's socket dropping while mpv stays alive ends the party with a reason, not a hang")
    pidfile = f"{RUN}/hungup.pid"
    with open(f"{RUN}/hungup.py", "w") as f:
        f.write(f"""import json, os, socket, sys, time
open({pidfile!r}, "w").write(str(os.getpid()))
path = next(a.split("=", 1)[1] for a in sys.argv if a.startswith("--input-ipc-server="))
s = socket.socket(socket.AF_UNIX)
s.bind(path)
s.listen()
c = s.accept()[0]
r = c.makefile("rb")
for _ in range(4):
    c.sendall(json.dumps({{"request_id": json.loads(r.readline())["request_id"], "error": "success"}}).encode() + b"\\n")
r.close()
c.close()
time.sleep(600)
""")
    script(f"{RUN}/hungup", f'exec python3 {RUN}/hungup.py "$@"')
    n = launch("hungUp", port=PORTS["upnpNode"], mpv=f"{RUN}/hungup")
    ok("it exits with an error", until(lambda: nodes[n]["proc"].poll() is not None, BOOT) and nodes[n]["proc"].returncode == 1, str(nodes[n]["proc"].poll()))
    ok("that says what happened", "mpv stopped answering" in log(n), log(n))
    ok("and takes mpv with it", until(lambda: gone(int(open(pidfile).read()))))


def install_suite():
    header("[G33] install.sh: the right asset, the app menu entry, the macOS bundle, and no mess on a failure")
    stubs = f"{RUN}/installer"
    os.makedirs(stubs)
    script(stubs + "/uname", 'case $1 in -m) echo "$FAKE_M" ;; *) echo "$FAKE_S" ;; esac')
    script(stubs + "/curl", 'echo "$5" >> "$HOME/urls"\n[ -z "$CURL_FAIL" ] || { echo partial > "$4"; exit 22; }\necho the-binary > "$4"')

    def install(home, s, m, src=REPO + "/install.sh", **env):
        os.makedirs(home, exist_ok=True)
        return subprocess.run(["sh", src], capture_output=True, text=True, timeout=SETTLE,
                              env={"HOME": home, "PATH": f"{stubs}:/usr/bin:/bin", "FAKE_S": s, "FAKE_M": m, **env})

    def asked(home):
        return open(home + "/urls").read().split() if os.path.exists(home + "/urls") else []

    home = f"{RUN}/install linux"  # a space, as a real home may have
    res = install(home, "Linux", "x86_64")
    ok("linux installs", res.returncode == 0, res.stderr)
    ok("it asks for the latest gui", asked(home) == ["https://github.com/mhashemm/watchparty/releases/latest/download/watchparty-gui_amd64"], str(asked(home)))
    exe = home + "/.local/bin/watchparty-gui"
    ok("the binary is executable, with nothing left beside it", os.access(exe, os.X_OK) and os.listdir(home + "/.local/bin") == ["watchparty-gui"], str(os.listdir(home + "/.local/bin")))
    entry = open(home + "/.local/share/applications/watchparty.desktop").read()
    ok("the app menu entry runs it, quoted", f'Exec="{exe}"' in entry and "Icon=watchparty" in entry, entry)
    ok("and has its icon", os.path.exists(home + "/.local/share/icons/hicolor/scalable/apps/watchparty.svg"))

    res = install(home, "Linux", "x86_64", CURL_FAIL="1")
    ok("a failed download fails the install", res.returncode != 0)
    ok("keeps the installed binary and leaves no partial file", open(exe).read() == "the-binary\n" and os.listdir(home + "/.local/bin") == ["watchparty-gui"], str(os.listdir(home + "/.local/bin")))

    home = f"{RUN}/install-arm"
    res = install(home, "Linux", "aarch64", XDG_BIN_HOME=home + "/b", XDG_DATA_HOME=home + "/d")
    ok("arm64 gets its asset, in the XDG folders", res.returncode == 0 and asked(home)[0].endswith("/watchparty-gui_arm64") and os.path.exists(home + "/b/watchparty-gui") and os.path.exists(home + "/d/applications/watchparty.desktop"), res.stderr)

    home = f"{RUN}/install-mac"
    # Homebrew's prefix moved into the run, with brew and the three tools already in it
    brew = home + "/homebrew"
    os.makedirs(brew + "/bin")
    for tool in ("brew", "mpv", "ffmpeg", "yt-dlp"):
        script(f"{brew}/bin/{tool}", "exit 0")
    with open(REPO + "/install.sh") as f, open(f"{RUN}/install-mac.sh", "w") as out:
        out.write(f.read().replace("/opt/homebrew", brew))
    res = install(home, "Darwin", "arm64", src=f"{RUN}/install-mac.sh")
    ok("macOS installs", res.returncode == 0, res.stderr)
    ok("it asks for the mac asset", asked(home)[0].endswith("/watchparty-gui_mac_arm64"), str(asked(home)))
    contents = home + "/Applications/Watchparty.app/Contents"
    with open(contents + "/Info.plist", "rb") as f:
        plist = plistlib.load(f)
    ok("the bundle's plist names the binary inside it", os.access(f"{contents}/MacOS/{plist['CFBundleExecutable']}", os.X_OK), str(plist))
    ok("and can find Homebrew's mpv from Finder", brew + "/bin" in plist["LSEnvironment"]["PATH"], str(plist))

    for s, m, why in (("Linux", "armv7l", "an architecture with no build"), ("FreeBSD", "amd64", "an OS with no build")):
        home = f"{RUN}/install-{m}"
        res = install(home, s, m)
        ok(f"{why} stops before downloading", res.returncode != 0 and not asked(home) and res.stderr, res.stderr)


def listening_unix(path):
    s = socket.socket(socket.AF_UNIX)
    try:
        s.connect(path)
        return True
    except OSError:
        return False
    finally:
        s.close()


def keep_logs():
    """Copy this run's logs to $E2E_LOGS/<mode>, which make e2e mounts from the host, and
    say where they are. Only logs: the binaries, the video and browser profiles stay."""
    dest = os.environ.get("E2E_LOGS")
    if not dest:
        return RUN
    dest = os.path.join(dest, args.mode)
    for d, dirs, files in os.walk(RUN):
        dirs[:] = [x for x in dirs if x != "tmp"]  # a GUI's TMPDIR: browser profiles, mpv sockets
        for f in files:
            if f.endswith((".log", ".args")):
                rel = os.path.relpath(os.path.join(d, f), RUN)
                os.makedirs(os.path.join(dest, os.path.dirname(rel)), exist_ok=True)
                shutil.copy(os.path.join(d, f), os.path.join(dest, rel))
    return dest


if JITTERED:
    print(f"jitter seed {JITTERED}: replay with --mode {args.mode} --jitter {JITTERED}")
    lag(PORTS["jitter"], PORTS["relay"], jitter)
try:
    {"p2p": p2p_suite, "relay": relay_suite, "media": media_suite, "caps": caps_suite, "gui": gui_suite, "upnp": upnp_suite}[args.mode]()
except Stop as e:
    print(f"  STOP  the rest of this phase depends on: {e}")
finally:
    for name in list(nodes):
        if nodes[name].get("gui"):
            try:
                os.killpg(nodes[name]["proc"].pid, signal.SIGKILL)
            except OSError:
                pass
            nodes[name]["proc"].wait(timeout=5)
            continue
        stop(name, signal.SIGKILL, timeout=5)
    for p in stubs + relays:
        if p.poll() is None:
            p.kill()
            p.wait(timeout=5)
    mark("cases")
    print("took: " + ", ".join(f"{n} {t - marks[i][1]:.0f}s" for i, (n, t) in enumerate(marks[1:])) + f", total {time.time() - marks[0][1]:.0f}s")
    print("slowest: " + slowest(marks[-1][1]))
    # here, so a crash or an early exit still leaves its logs
    print(f"\nlogs: {keep_logs()}")

print("ALL PASS" if not fails else f"{len(fails)} FAILURES: " + ", ".join(fails))
sys.exit(1 if fails else 0)
