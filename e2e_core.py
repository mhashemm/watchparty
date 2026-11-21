"""What every e2e phase shares: the run's settings and ports, the binaries, the helpers that
drive nodes, relays and GUIs, and the case runner. e2e.py imports it once, then the phase's
module, which takes all of it with `from e2e_core import *`."""
import argparse, ast, contextlib, functools, glob, http.client, http.server, itertools, json, os, re, shutil, signal, socket, subprocess
import plistlib, queue, random, sys, tempfile, threading, time, traceback, urllib.error, urllib.parse, urllib.request
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

        def log_message(self, format, *args):
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
# the script a phase or a stub peer runs as: copies start where e2e.py starts
ENTRY = os.path.join(REPO, "e2e.py")


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
    bins = None if args.bin else tempfile.mkdtemp(prefix="watchparty-e2e-bin-")
    if bins:
        print("building every phase's binaries once")
        build(bins, True)
        flags += ["--bin", bins]
    procs = [(m, subprocess.Popen([sys.executable, ENTRY, "--mode", m, "--port-base", str(args.port_base + 20 * i)] + flags,
                                  stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True))
             for i, m in enumerate(PHASES)]
    took = []
    for m, p in procs:
        out = p.communicate()[0]
        print(f"\n######## {m} ########\n" + out, end="")
        took += [f"{m:6} {line}" for line in out.splitlines() if line.startswith(("took:", "slowest:"))]
    print("\n" + "\n".join(took))
    bad = [m for m, p in procs if p.returncode]
    if bins:
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
# the private session bus the gui and upnp phases start (private_bus), read by launch_gui
BUS = ""


def ok(name, cond, detail: object = ""):
    if not cond:
        now = getattr(running, "id", None)
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


def num(n, prop):
    """A number mpv must hold: none ends the case, as must() does."""
    v = get(n, prop)
    if v is None:
        ok(f"{n} holds {prop}", False)
        raise Stop(f"{n} holds {prop}")
    return float(v)


def setp(n, prop, val):
    ipc(nodes[n]["sock"], "set_property", prop, val)


class Stop(Exception):
    """A setup step failed: every case after it would fail for the same reason."""


def must(name, cond, detail: object = ""):
    """ok(), but a failure ends the case here instead of cascading into its later checks."""
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


def hold(secs, why):
    """The one sleep a case takes: a window in which something must NOT happen (an echo,
    a bounce, a second notice), or time let pass on purpose (the video plays, the relay
    stays down). why says which. Anything that does happen is waited for with until();
    the run refuses a bare time.sleep outside the harness's own helpers."""
    time.sleep(secs)


def toggle(n=None, settle=0.9):
    """Flip pause to the opposite of its current value; mpv emits no event for a no-op set."""
    n = n or A
    setp(n, "pause", not get(n, "pause"))
    hold(settle, "mpv's change and its broadcast land")


def log(n):
    """Its log's finished lines: a line longer than what is left of a page is written a page at
    a time, so a read can end halfway through it."""
    text = open(nodes[n]["log"]).read()
    return text[:text.rfind("\n") + 1]


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


# below 32768, where the kernel never picks a port for bind(0) or an outgoing connection, and a
# range of this phase's own: picked by bind(0), two phases side by side got the same port
FIRST_PORT = 16000 + 2000 * (PHASES + ("all",)).index(args.mode)
PORT_POOL = iter(range(FIRST_PORT, FIRST_PORT + 2000))


def free_port():
    for port in PORT_POOL:
        try:
            socket.create_server(("", port)).close()
            return port
        except OSError:
            pass
    raise RuntimeError("this phase used up its ports")


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
        # a copy: a redial accepted while this runs is a new connection, not one to cut
        for pair in list(self.pairs):
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


making = threading.Lock()  # cases side by side mustn't read a file another is still writing


def variants():
    """MEDIA copied under another name, folder and timestamp, and MEDIA one byte longer."""
    same, diff = f"{RUN}/elsewhere/renamed copy.mkv", f"{RUN}/different.mkv"
    with making:
        if not os.path.exists(diff):
            os.makedirs(f"{RUN}/elsewhere", exist_ok=True)
            shutil.copyfile(MEDIA, same)
            os.utime(same, (0, 0))
            shutil.copyfile(MEDIA, diff + ".part")
            with open(diff + ".part", "ab") as f:
                f.write(b"\0")
            os.rename(diff + ".part", diff)
    return same, diff


def raw(addr, endpoint, headers, body=b""):
    req = urllib.request.Request("http://" + addr + endpoint, data=body, method="POST", headers=headers)
    try:
        return urllib.request.urlopen(req, timeout=10).status
    except urllib.error.HTTPError as e:
        return e.code


def stub(peer, of, addr, register=True, events=""):
    """A peer at addr that joins node of's party through peer, its address."""
    cmd = [sys.executable, ENTRY, "--stub", addr, peer]
    if not register:
        cmd.append("noregister")
    p = subprocess.Popen(cmd, stdout=subprocess.PIPE, text=True,
                         env={**os.environ, "E2E_FILE": fingerprint(of), "E2E_SECRET": SECRET, "E2E_STUB_EVENTS": events})
    stubs.append(p)
    # blocks until the stub is listening and registered; a stale stub left on the
    # port would answer for it and the strike loops in [13] would never end
    assert p.stdout  # stdout=PIPE
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


def launch_gui(name, tag=None, exe=None, browser: str | None = "headless", portal=True):
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
        os.symlink(shutil.which("mpv") or sys.exit("e2e needs mpv on PATH"), f"{home}/bin/mpv")
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


def pgid(p):
    try:
        return os.getpgid(p)
    except OSError:
        return None


def kill_gui(n):
    """A GUI and everything under it: its mpv and a GUI it restarted into share its process
    group, but each browser has a group of its own (ownGroup), which only the GUI kills, on
    a clean exit. Left running, the browsers of finished cases starved later ones' pages."""
    pid, groups = nodes[n]["proc"].pid, set()
    todo = [pid] + [int(d.rsplit("/", 1)[1]) for d in glob.glob("/proc/[0-9]*") if pgid(int(d.rsplit("/", 1)[1])) == pid]  # a restarted GUI too
    while todo:
        p = todo.pop()
        groups.add(pgid(p))
        todo += children(p)
    for g in (groups - {None}) | {pid}:
        try:
            os.killpg(g, signal.SIGKILL)
        except OSError:
            pass
    nodes[n]["proc"].wait(timeout=5)


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
    assert p.stdout  # stdout=PIPE
    if p.stdout.readline() != "ready\n":
        sys.exit("the fake portal could not start")
    return f"unix:path={path}"


marks = [("", time.time())]


def mark(name):
    marks.append((name, time.time()))


took = []     # (seconds, id) of each case that ran on its own
pending = []  # (title, function, serial, group) of each @case, run by run_cases()
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


def part(title):
    """A labelled step inside a case: its failures carry its own id."""
    running.id = title.split()[0]
    print("\n" + title)


def case(title, serial=False, group=None):
    """Adds the function below as a case, which run_cases() runs. It sets up what it checks
    from a party() of its own, so cases run side by side. serial is for one that touches
    what all share, like the relay itself: it runs alone, after the rest. Cases of one group
    share a thing only one can use at a time, like the LAN's one router: they take turns,
    in order, beside the other cases. A must() that fails or a crash ends one case alone."""
    def add(f):
        pending.append((title, f, serial, group))
        return f
    return add


def on_end(fn):
    """Runs fn when the running case ends, however it ends: for what no with-block holds."""
    running.cleanup.append(fn)


def run_case(title, f):
    running.id, running.buf, running.cleanup = title.split()[0], [], []
    start = time.time()
    print("\n" + title)
    try:
        f()
    except Stop as e:
        print(f"  STOP  the rest of this case depends on: {e}")
    except Exception:
        traceback.print_exc(file=sys.stdout)
        fails.append(f"{title.split()[0]} crashed")
    for fn in reversed(running.cleanup):
        try:
            fn()
        except Exception:
            traceback.print_exc(file=sys.stdout)
    took.append((time.time() - start, title.split()[0]))
    out, running.buf, running.id = "".join(running.buf), None, None
    print(out, end="")


def run_cases(at_once=CASES_AT_ONCE):
    """Runs every @case so far, at_once at a time (a group counts as one, its cases in
    turn), then the serial ones alone."""
    units, groups = [], {}
    for title, f, serial, group in pending:
        if serial:
            continue
        if group is None:
            units.append([(title, f)])
        elif group in groups:
            groups[group].append((title, f))
        else:
            groups[group] = [(title, f)]
            units.append(groups[group])
    alone = [(title, f) for title, f, serial, _ in pending if serial]
    pending.clear()
    threads, free = [], threading.Semaphore(at_once)

    def one(unit):
        try:
            for title, f in unit:
                run_case(title, f)
        finally:
            free.release()
    for unit in units:
        free.acquire()
        threads.append(threading.Thread(target=one, args=(unit,)))
        threads[-1].start()
    for t in threads:
        t.join()
    for title, f in alone:
        run_case(title, f)


def slowest(n=5):
    return ", ".join(f"{c} {t:.0f}s" for t, c in sorted(took, reverse=True)[:n])


def bare_sleeps(allowed=("hold", "pipe", "do_GET")):
    """file:line of each time.sleep in the suite's files outside the helpers allowed to sleep."""
    found = []

    def walk(node, owner):
        for child in ast.iter_child_nodes(node):
            name = child.name if isinstance(child, (ast.FunctionDef, ast.ClassDef)) else owner
            if isinstance(child, ast.Call) and ast.unparse(child.func) == "time.sleep" and name not in allowed:
                found.append(f"{os.path.basename(path)}:{child.lineno}")
            walk(child, name)
    for path in sorted(glob.glob(os.path.join(REPO, "e2e*.py"))):
        if path.endswith(("e2e_lib.py", "e2e_windows.py")):
            continue
        walk(ast.parse(open(path).read()), None)
    return found


if bare_sleeps():
    sys.exit(f"{', '.join(bare_sleeps())}: a bare time.sleep. Wait for what happens with until(), "
             "and for what must not with hold(secs, why)")
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
    assert p.stdout  # stdout=PIPE
    return p.stdout.readline().strip()


# once the party has said what it plays (party.NotTheSameFile); before, the relay's 409 alone
NOT_THE_SAME_VIDEO = "Diagnosis: your video is not the same file as the party's. Pick the same video and start again."
NOT_THE_SAME_FOLDER = "Diagnosis: this folder doesn't match the party's. Everyone needs the same videos in the same order."


def videos_in(path, names):
    """Three short, different entries in a folder: names[i] holds the i-th picture."""
    os.makedirs(path)
    for name, picture in zip(names, ("testsrc", "testsrc2", "smptebars")):
        target = f"{RUN}/{picture}.mkv"
        with making:
            if not os.path.exists(target):
                subprocess.run(["ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", f"{picture}=size=160x120:rate=10", "-t", "12",
                                "-c:v", "libx264", "-pix_fmt", "yuv420p", target], check=True)
        shutil.copy(target, f"{path}/{name}")
    return path


COUPLED = re.compile(r"season|episode|movie|\btv\b", re.I)


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


def finish(suite):
    """Runs the phase's suite, stops everything it started, and exits with the result."""
    if JITTERED:
        print(f"jitter seed {JITTERED}: replay with --mode {args.mode} --jitter {JITTERED}")
        lag(PORTS["jitter"], PORTS["relay"], jitter)
    try:
        suite()
    except Stop as e:
        print(f"  STOP  the rest of this phase depends on: {e}")
    finally:
        for name in list(nodes):
            if nodes[name].get("gui"):
                kill_gui(name)
                continue
            stop(name, signal.SIGKILL, timeout=5)
        for p in stubs + relays:
            if p.poll() is None:
                p.kill()
                p.wait(timeout=5)
        mark("cases")
        print("took: " + ", ".join(f"{n} {t - marks[i][1]:.0f}s" for i, (n, t) in enumerate(marks[1:])) + f", total {time.time() - marks[0][1]:.0f}s")
        print("slowest: " + slowest())
        # here, so a crash or an early exit still leaves its logs
        print(f"\nlogs: {keep_logs()}")

    print("ALL PASS" if not fails else f"{len(fails)} FAILURES: " + ", ".join(fails))
    sys.exit(1 if fails else 0)
