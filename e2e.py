#!/usr/bin/env python3
"""End-to-end watchparty suite. Real mpv processes, real transports, no Go tests.

Builds the binaries, generates a test file, starts five nodes on this machine and
drives them by writing to one node's mpv IPC socket, then asserts the result on
the other nodes' mpv. OSD notices are read back out of each node's log, since
every show-text command appears there verbatim.

Four phases, run by default at the same time as four copies of this script,
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
  gui    real GUIs, each opening its own page in headless Chromium, driven
         through the page over CDP: every status, both modes, the invite, the
         feed, Browse through a fake portal, Settings, history, persistence,
         self-update against a local release, and the window's lifetime,
         including a real window on Xvfb closing itself.

    ./e2e.py                    # all four phases in parallel, ~45 seconds
    ./e2e.py --mode relay       # one phase
    ./e2e.py --file movie.mkv   # use a real file instead of a generated one
    ./e2e.py --nodes 3          # fewer nodes, faster

Needs go, mpv, Chrome or Chromium, dbus-daemon and (unless --file is given) ffmpeg.
All run headless.
Exits non-zero if any assertion fails; the run directory with all node logs is
printed at the end.
"""
import argparse, functools, glob, http.server, json, os, re, shutil, signal, socket, subprocess
import queue, sys, tempfile, threading, time, urllib.error, urllib.parse, urllib.request
sys.dont_write_bytecode = True  # no __pycache__ in the repo
import e2e_lib
from e2e_lib import Page, Release, find_page, sha

# the last three try to undo what party.Run pins; [2b] and [5d] check the pins win
MPVFLAGS = "--vo=null --ao=null --msg-level=all=error --speed=2 --hr-seek=no --cache-pause=no --resume-playback=yes"
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
                                              "file": os.environ["E2E_FILE"], "secret": os.environ["E2E_SECRET"]})
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
ap.add_argument("--mode", choices=("p2p", "relay", "caps", "gui", "all"), default="all")
ap.add_argument("--audio", action="store_true", help="real audio output, muted, instead of --ao=null")
args = ap.parse_args()
if args.audio:
    MPVFLAGS = MPVFLAGS.replace("--ao=null", "--mute=yes")
# every child inherits this; an mpv that sees a tty sets it raw and no-echo, and a
# SIGKILLed one never restores it, leaving the user's terminal unusable
os.dup2(os.open(os.devnull, os.O_RDONLY), 0)

# relay-mode nodes open no ports, so the phases only need disjoint port bases
if args.mode == "all":
    flags = ["--nodes", str(args.nodes)] + (["--file", args.file] if args.file else []) + (["--audio"] if args.audio else [])
    procs = [(m, subprocess.Popen([sys.executable, __file__, "--mode", m, "--port-base", str(args.port_base + 20 * i)] + flags,
                                  stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True))
             for i, m in enumerate(("p2p", "relay", "caps", "gui"))]
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
PORTS["lagA"] = args.port_base + len(NODES) + 6
PORTS["lagB"] = args.port_base + len(NODES) + 7
PORTS["release"] = args.port_base + len(NODES) + 8
PORTS["wifiHost"] = args.port_base + len(NODES) + 9
PORTS["wifiGuest"] = args.port_base + len(NODES) + 10
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


def launch(name, addrs=None, room=None, secret=None, tag=None, relay=None, create_key=None, media=None, evict="1ms", mpvflags="", port=None):
    """Start a node. room!=None selects relay mode; addrs is the p2p bootstrap. evict is
    -evictAfter, short by default so the strike cases need no 5-minute wait."""
    key = tag or name
    path = f"{RUN}/{key}.log"
    cmd = [BIN, "-file", media or MEDIA, "-hostname", name, "-mpvFlags", f"{MPVFLAGS} {mpvflags}"]
    if room is not None:
        cmd += ["-relay", relay or RELAY_URL, "-room", room, "-secret", secret or SECRET]
        if create_key:
            cmd += ["-createKey", create_key]
    else:
        cmd += ["-local", "-port", str(port or PORTS[name]), "-secret", secret or SECRET]
        if evict:
            cmd += ["-evictAfter", evict]
        if addrs:
            cmd += ["-addrs", addrs]
    p = subprocess.Popen(cmd, stdout=open(path, "wb"), stderr=subprocess.STDOUT)
    nodes[key] = {"proc": p, "log": path, "sock": None, "addr": None}
    return key


def lag(port, target, delay):
    """A TCP proxy on port that delivers every chunk to target delay seconds late, in
    order, both ways: a network with that much one-way latency."""
    srv = socket.socket()
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", port))
    srv.listen()

    def pipe(src, dst):
        q = queue.Queue()

        def read():
            while True:
                try:
                    b = src.recv(65536)
                except OSError:
                    b = b""
                q.put((time.time() + delay, b))
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
        while True:
            try:
                c, _ = srv.accept()
                u = socket.create_connection(("127.0.0.1", target))
            except OSError:
                return
            threading.Thread(target=pipe, args=(c, u), daemon=True).start()
            threading.Thread(target=pipe, args=(u, c), daemon=True).start()
    threading.Thread(target=accept, daemon=True).start()
    return srv


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


def stub(peer, register=True):
    cmd = [sys.executable, __file__, "--stub", STUB_ADDR, peer]
    if not register:
        cmd.append("noregister")
    p = subprocess.Popen(cmd, stdout=subprocess.PIPE, text=True, env={**os.environ, "E2E_FILE": fingerprint(A), "E2E_SECRET": SECRET})
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
            flags = "--headless=new --remote-debugging-port=0"
            if browser == "xvfb":
                flags = flags.replace("--headless=new", "--ozone-platform=x11")
            run = "exit 0" if browser == "handoff" else f'PATH=/usr/bin:/bin exec {"xvfb-run -a " if browser == "xvfb" else ""}{BROWSER} {flags} "$@"'
            script(f"{home}/browser.sh", f'echo "$@" >> {home}/browser.args\n{run}')
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
    nodes[n]["page"].ready(BOOT, downloads=f"{nodes[n]['home']}/downloads")


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


need("go")
need("mpv")
print(f"run directory: {RUN}")
print("building watchparty and relay")
subprocess.run(["go", "build", "-o", RUN + "/watchparty", "."], cwd=REPO, check=True)
subprocess.run(["go", "build", "-o", RUN + "/relay", "./relay"], cwd=REPO, check=True)
BIN = RUN + "/watchparty"
RELAY_BIN = RUN + "/relay"
GUI_BIN = RUN + "/watchparty-gui"
PORTAL_BIN = RUN + "/fakeportal"
if args.mode == "gui":
    BROWSER = next(filter(None, map(shutil.which, ("google-chrome", "chromium", "google-chrome-stable"))), None)
    if not BROWSER:
        sys.exit("the gui phase needs Chrome or Chromium on PATH")
    need("dbus-daemon")
    need("xvfb-run")
    print("building the gui, two stamped versions of it, and the fake portal")
    release = f"-X main.latestReleaseURL=http://127.0.0.1:{PORTS['release']}/latest"
    for out, flags in ((GUI_BIN, ""), (RUN + "/gui-v0.0.1", f"-X main.version=v0.0.1 {release}"), (RUN + "/gui-v0.0.2", f"-X main.version=v0.0.2 {release}")):
        subprocess.run(["go", "build", "-ldflags", flags, "-o", out, "./gui"], cwd=REPO, check=True)
    subprocess.run(["go", "build", "-o", PORTAL_BIN, "./gui/fakeportal"], cwd=REPO, check=True)

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
        ok(f"{n} waits for its cache", get(n, "cache-pause") is True)

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
    code = raw(nodes[A]["addr"], "/event", {"hit-me-up": f"{IP}:9999", "counter": "5", "hostname": "stranger", "secret": SECRET},
               b'{"event":"property-change","name":"pause","data":"yes"}')
    ok(f"{A} rejects unknown peer's event", code == 400, f"HTTP {code}")

    print("\n[10] /bye from an unknown address is rejected")
    code = raw(nodes[A]["addr"], "/bye", {"hit-me-up": f"{IP}:9999", "counter": "5", "hostname": "stranger", "secret": SECRET})
    ok(f"{A} rejects unknown peer's bye", code == 400, f"HTTP {code}")

    print("\n[10b] without the secret, a peer's own address forges nothing")
    pause = b'{"event":"property-change","name":"pause","data":"yes"}'
    for secret in (None, "wrongsecret"):
        h = {"hit-me-up": nodes[b]["addr"], "counter": "999999", "hostname": b, "file": fingerprint(A)}
        if secret:
            h["secret"] = secret
        for endpoint in ("/hi", "/event", "/bye"):
            code = raw(nodes[A]["addr"], endpoint, h, pause if endpoint == "/event" else b"")
            ok(f"{A} refuses {endpoint} with {'no' if secret is None else 'a wrong'} secret", code == 403, f"HTTP {code}")
    code = raw(nodes[A]["addr"], "/event", {"hit-me-up": nodes[b]["addr"], "counter": "999999", "hostname": b,
                                           "secret": SECRET}, b"x" * ((64 << 10) + 1))
    ok(f"{A} refuses an oversized event", code == 413, f"HTTP {code}")
    setp(LAST, "pause", True)
    until(lambda: get(b, "pause") is True)
    ok(f"{b} is still in the mesh", get(b, "pause") is True, "the forged /bye was refused")
    setp(LAST, "pause", False)
    until(lambda: all(get(n, "pause") is False for n in NODES))

    print("\n[11] /bye with a stale counter cannot evict a live peer")
    code = raw(nodes[A]["addr"], "/bye", {"hit-me-up": nodes[b]["addr"], "counter": "1", "hostname": b, "secret": SECRET})
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
    launch("nodeBad", addr_of[A], secret="wrongsecret", tag="bad", port=PORTS["nodeDiff"])
    until(lambda: nodes["bad"]["proc"].poll() is not None, BOOT)
    ok("a joiner with the wrong secret exited with an error", nodes["bad"]["proc"].poll() not in (None, 0),
       f"rc={nodes['bad']['proc'].returncode}")
    ok("it was told why", "the party refused your secret" in log("bad"))
    ok("nobody saw it join", not any("nodeBad" in known(n) for n in NODES))
    stop("bad")
    stop("nodeSame")
    until(lambda: all("nodeSame has left" in osd(n) for n in NODES))

    print("\n[11c] a mid-party joiner pauses the party once loaded, and starts where it stopped")
    setp(A, "pause", False)
    until(lambda: all(get(n, "pause") is False for n in NODES))
    launch("nodeMid", addr_of[A], tag="mid", port=PORTS["nodeSame"])
    until(lambda: booted("mid"), BOOT)
    resolve("mid")
    until(lambda: all(get(n, "pause") is True for n in NODES + ["mid"])
          and all("paused for nodeMid: joining" in osd(n) for n in NODES)
          and near(get("mid", "time-pos"), get(A, "time-pos"), 0.3))
    at = get(A, "time-pos")
    for n in NODES:
        ok(f"{n} paused for the joiner", get(n, "pause") is True)
        ok(f"{n} told why", "paused for nodeMid: joining" in osd(n))
    ok(f"the joiner starts at {at:.2f}s, not 0", near(get("mid", "time-pos"), at, 0.3) and at > 1,
       f"time-pos={get('mid', 'time-pos')}")
    stop("mid")

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

    print("\n[R6b] a seek while playing pauses the party, and everyone lands where it stopped")
    ipc(nodes[A]["sock"], "seek", 20, "relative")
    until(lambda: all(get(n, "pause") is True for n in NODES)
          and all(near(get(n, "time-pos"), get(A, "time-pos"), 0.15) for n in others(A))
          and all(f"paused for {A}: seek" in osd(n) for n in others(A)))
    at = get(A, "time-pos")
    for n in NODES:
        ok(f"{n} paused", get(n, "pause") is True)
    for n in others(A):
        v = get(n, "time-pos")
        ok(f"{n} at {A}'s {at:.2f}s", near(v, at, 0.15), f"time-pos={v}")
        ok(f"{n} told why", f"paused for {A}: seek" in osd(n))
    ok(f"{A} told it paused everyone", "paused for everyone: seek" in osd(A))

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
    resolve("late")
    until(lambda: near(get("late", "time-pos"), 33.0, 0.15)
          and all("paused for nodeLate: joining" in osd(n) for n in NODES))
    for n in NODES:
        ok(f"{n} told why, though the party was already paused", "paused for nodeLate: joining" in osd(n))
    time.sleep(QUIET)  # a rewind would go out as soon as its file loaded
    ok("a trailing slash on the relay address still connects", connected("late"))
    for n in NODES:
        v = get(n, "time-pos")
        ok(f"{n} still at 33s after a late join", near(v, 33.0), f"time-pos={v}")
    ok("the late joiner starts at 33s too", near(get("late", "time-pos"), 33.0, 0.15), f"time-pos={get('late', 'time-pos')}")
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

    print("\n[R10b] a mid-party joiner pauses the party once loaded, and starts where it stopped")
    setp(A, "pause", False)
    until(lambda: all(get(n, "pause") is False for n in NODES))
    launch("nodeMid", room=ROOM, tag="mid")
    until(lambda: booted("mid"), BOOT)
    resolve("mid")
    until(lambda: all(get(n, "pause") is True for n in NODES + ["mid"])
          and all("paused for nodeMid: joining" in osd(n) for n in NODES)
          and near(get("mid", "time-pos"), get(A, "time-pos"), 0.3))
    at = get(A, "time-pos")
    for n in NODES:
        ok(f"{n} paused for the joiner", get(n, "pause") is True)
        ok(f"{n} told why", "paused for nodeMid: joining" in osd(n))
    ok(f"the joiner starts at {at:.2f}s, not 0", near(get("mid", "time-pos"), at, 0.3) and at > 1,
       f"time-pos={get('mid', 'time-pos')}")
    stop("mid")

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
        late = 0  # seconds, held only for a client whose User-Agent is "late"
        slow = 0  # bytes/s for a client whose User-Agent is "slow"

        def log_message(self, *a):
            pass

        # Range support: without it mpv calls the stream unseekable and refuses
        # any seek past what it has buffered, so [R11d] failed on a busy runner
        def do_GET(self):
            time.sleep(Quiet.late if self.headers.get("User-Agent") == "late" else Quiet.delay)
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
            slow = Quiet.slow and self.headers.get("User-Agent") == "slow"
            step = Quiet.slow // 10 if slow else len(body)
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
    until(lambda: near(get("nodeUrl2", "time-pos"), 50))
    time.sleep(QUIET)  # a late echo would go out right behind the last seek
    ok("quick seeks don't bounce back", all(near(get(n, "time-pos"), 50) for n in ("nodeUrl1", "nodeUrl2")),
       f"at {get('nodeUrl1', 'time-pos')} and {get('nodeUrl2', 'time-pos')}")
    until(lambda: booted("nodeUrl4"), BOOT)
    Quiet.delay = 0
    resolve("nodeUrl4")
    until(lambda: near(get("nodeUrl4", "time-pos"), 50) and get("nodeUrl4", "seeking") is False)
    ok("nodeUrl4 is where nodeUrl1 stopped", near(get("nodeUrl4", "time-pos"), 50), f"at {get('nodeUrl4', 'time-pos')}")
    ok("one seek, not five", osd("nodeUrl4").count("synced by nodeUrl1") == 1,
       f"{osd('nodeUrl4').count('synced by nodeUrl1')} syncs")

    print("\n[R11d2] a joiner slow to load while the party plays pauses it once loaded, not before")
    setp("nodeUrl1", "pause", False)
    until(lambda: all(get(n, "pause") is False for n in ("nodeUrl1", "nodeUrl2", "nodeUrl4")))
    Quiet.late = 3
    launch("nodeUrl5", room="urlroom", media=url, mpvflags="--user-agent=late")
    until(lambda: "nodeUrl5 has joined" in osd("nodeUrl1"), BOOT)
    ok("the party plays on while it loads", not booted("nodeUrl5") and get("nodeUrl1", "pause") is False)
    until(lambda: booted("nodeUrl5"), BOOT)
    Quiet.late = 0
    resolve("nodeUrl5")
    until(lambda: get("nodeUrl1", "pause") is True and "paused for nodeUrl5: joining" in osd("nodeUrl1") and near(get("nodeUrl5", "time-pos"), get("nodeUrl1", "time-pos"), 0.3))
    at = get("nodeUrl1", "time-pos")
    ok("then it paused the party", get("nodeUrl1", "pause") is True and "paused for nodeUrl5: joining" in osd("nodeUrl1"))
    ok(f"and starts at {at:.2f}s", near(get("nodeUrl5", "time-pos"), at, 0.3) and at > 51,
       f"time-pos={get('nodeUrl5', 'time-pos')}")
    for n in ("nodeUrl1", "nodeUrl2", "nodeUrl3", "nodeUrl4", "nodeUrl5"):
        stop(n)

    print("\n[R11f] a node that runs out of cache pauses the party, and so does a resume while it waits")
    Quiet.slow = 6000  # about half the test file's bitrate
    launch("nodeFast", room="bufroom", media=url)
    until(lambda: booted("nodeFast") and connected("nodeFast"), BOOT)
    resolve("nodeFast")
    launch("nodeSlow", room="bufroom", media=url, mpvflags="--user-agent=slow")
    until(lambda: booted("nodeSlow"), BOOT)
    resolve("nodeSlow")
    reasons = lambda: [s for s in osd("nodeFast") if s.startswith("paused for nodeSlow: ")]
    both_paused = lambda: all(get(n, "pause") is True for n in ("nodeFast", "nodeSlow"))
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
    Quiet.slow = 0
    for n in ("nodeFast", "nodeSlow"):
        stop(n)

    print("\n[R11g] over a slow network, pausing and resuming from different nodes keeps them in step")
    # nodeLagA is 20ms from the relay and nodeLagB 250ms, and the stream answers each
    # request 200ms late. A resume reaches B late and B stops late, which cancels, but
    # when B pauses what A resumed, A stops a whole round trip after B did.
    proxies = [lag(PORTS["lagA"], PORTS["relay"], 0.02), lag(PORTS["lagB"], PORTS["relay"], 0.25)]
    Quiet.delay = 0.2
    launch("nodeLagA", room="lagroom", media=url, relay=f"ws://127.0.0.1:{PORTS['lagA']}")
    until(lambda: booted("nodeLagA") and connected("nodeLagA"), BOOT)
    resolve("nodeLagA")
    launch("nodeLagB", room="lagroom", media=url, relay=f"ws://127.0.0.1:{PORTS['lagB']}")
    until(lambda: booted("nodeLagB") and "nodeLagB has joined" in osd("nodeLagA"), BOOT)
    resolve("nodeLagB")
    lagged = ("nodeLagA", "nodeLagB")
    gap = lambda: abs((get("nodeLagA", "time-pos") or 0) - (get("nodeLagB", "time-pos") or 0))
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
    Quiet.delay = 0
    for n in lagged:
        stop(n)
    for p in proxies:
        p.close()
    web.shutdown()

    print("\n[R11e] a file mpv can't play: the node fails, and its log says why")
    notvideo = f"{RUN}/notavideo.txt"
    open(notvideo, "w").write("not a video\n")
    launch("nodeNotVideo", room="notvideo", media=notvideo)
    until(lambda: nodes["nodeNotVideo"]["proc"].poll() is not None, BOOT)
    rc = nodes["nodeNotVideo"]["proc"].returncode
    ok("it exited with an error", rc not in (None, 0), f"rc={rc}")
    ok("mpv's own reason is in its log", "mpv: Failed to recognize file format." in log("nodeNotVideo"))

    print("\n[R12] a node that reconnects while the relay still holds its dead conn")
    proxy = Proxy(PORTS["relay"])
    launch("nodeFlaky", room=ROOM, tag="flaky", relay=proxy.url)
    until(lambda: all("nodeFlaky has joined" in osd(n) for n in NODES), BOOT)
    proxy.cut()
    until(lambda: connected("flaky", 2))  # 1s backoff, then the redial
    time.sleep(QUIET)  # a second join or a false leave would follow the swap at once
    connects = log("flaky").count("relay: connected")
    ok("nodeFlaky redialled", connects == 2, f"connected {connects} times, want 2")
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


def gui_suite():
    global BUS
    H, G, KEY = "guiHost", "guiGuest", "e2ekey"
    print("\n=== gui setup: two real GUIs, each in headless Chromium, through one relay ===")
    BUS = private_bus()
    relay = launch_relay({"CREATE_KEY": KEY})
    until(lambda: listening(PORTS["relay"]))
    launch_gui(H)
    launch_gui(G, portal=False)
    # launched now so it is long past the hand-off guard's 5s when [G32] closes it
    X = launch_gui("guiXvfb", browser="xvfb")

    def mpv_of(n):
        return child_mpv_socket(nodes[n]["proc"].pid)

    def go(n):
        nodes[n]["page"].go(BOOT)

    def start(n, **fields):
        fill(n, **fields)
        go(n)

    def end(n):
        js(n, "window.asked = null")
        click(n, "#go")
        until(lambda: status(n) == "The party has ended.")
        return js(n, "window.asked")

    def joined(n):
        until(lambda: status(n).startswith("You're in"), BOOT)
        nodes[n]["sock"] = mpv_of(n)
        return status(n).startswith("You're in")

    print("\n[G1] the GUI opens its own app window at the phone-sized window")
    for n in (H, G):
        argv = launches(n)[0]
        ok(f"{n} opened {nodes[n]['url']} as an app", f"--app={nodes[n]['url']} " in argv, argv)
        ok(f"{n} asked for 560x940", "--window-size=560,940" in argv)
    until(lambda: js(H, '$("version-text").textContent') != "Checking the version…")
    ok("a dev build says so and offers no update", js(H, '$("version-text").textContent == "Development build" && $("update").hidden'))

    print("\n[G2] the tabs switch views and the address remembers which")
    for view in ("history", "settings", "party"):
        tab(H, view)
        shown = js(H, '["party", "history", "settings"].filter(v => !$("view-" + v).hidden)')
        ok(f"{view} tab shows only its view", shown == [view], str(shown))
        ok(f"{view} tab sets the address", js(H, "location.hash") == ("" if view == "party" else "#" + view))

    print("\n[G3] Settings saves, and fills the party's relay")
    ok(f"{H} saved", save_settings(H, **{"set-relay": RELAY_URL, "createKey": KEY, "mpvFlags": MPVFLAGS}) == "Saved.")
    ok(f"{G} saved", save_settings(G, **{"set-relay": RELAY_URL, "mpvFlags": MPVFLAGS, "mpv": f"{nodes[G]['home']}/slow-mpv"}) == "Saved.")
    s = state(H)["settings"]
    ok("the settings reached the store", (s["relay"], s["createKey"], s["mpvFlags"]) == (RELAY_URL, KEY, MPVFLAGS), str(s))
    ok("the party's relay was filled in", js(H, '$("relay").value') == RELAY_URL)
    store = js(H, '$("store-path").textContent')
    ok("the store is where the page says", store == f"{nodes[H]['home']}/config/watchparty/gui.json", store)
    ok("the store is 0600 in a 0700 folder", (os.stat(store).st_mode & 0o777, os.stat(os.path.dirname(store)).st_mode & 0o777) == (0o600, 0o700))

    print("\n[G4] Browse uses the desktop portal, and says so when there is none")
    click(H, "[data-pick=file]")
    ok(f"{H} picked the video through the portal", until(lambda: js(H, '$("file").value') == MEDIA), js(H, '$("file").value'))
    click(G, "[data-pick=file]")
    until(lambda: js(G, '!$("error").hidden'))
    ok(f"{G}, with no portal, is told to type the path", js(G, '$("error").textContent').startswith("No file picker found"), js(G, '$("error").textContent'))

    print("\n[G5] the form refuses what can't start")
    fill(H, file="   ")
    click(H, "#go")
    ok("a blank path asks for a video", js(H, '$("error").textContent') == "Pick a video first.")
    fill(H, file=MEDIA, relay="")
    click(H, "#go")
    until(lambda: js(H, '$("error").textContent').startswith("Diagnosis"))
    ok("relay mode with no relay is refused", js(H, '$("error").textContent') == "Diagnosis: enter the relay address, or paste an invite.", js(H, '$("error").textContent'))
    ok("and nothing started", js(H, '$("vitals").hidden'))

    print("\n[G6] the host starts a relay party from the form and gets an invite")
    start(H, hostname=H, relay=RELAY_URL)
    must(f"{H} is in", joined(H), status(H))
    until(lambda: js(H, '$("share-text").textContent') != "")
    invite = js(H, '$("share-text").textContent')
    room, secret = invite.split("-room ", 1)[1].split()[0], invite.split("-secret ", 1)[1].split()[0]
    ok(f"{H} shows an invite", invite == f"-relay {RELAY_URL} -room {room} -secret {secret}", invite)
    ok(f"{H} hides the form and shows the invite box", js(H, '$("fields").hidden && !$("share").hidden'))
    ok("Update and Restart are off during a party", js(H, '$("update").disabled && $("restart").disabled && $("restart-now").disabled'))

    print("\n[G7] Copy invite puts it on the clipboard")
    click(H, "#copy")
    until(lambda: js(H, '$("copy").textContent') != "Copy invite")
    ok("the button says Copied", js(H, '$("copy").textContent') == "Copied", js(H, '$("copy").textContent'))
    ok("the clipboard has the invite", js(H, "navigator.clipboard.readText()") == invite)
    ok("the button comes back", until(lambda: js(H, '$("copy").textContent') == "Copy invite", 5))

    print("\n[G8] Save chart downloads the whole log")
    click(H, "#save-chart")
    dl = f"{nodes[H]['home']}/downloads"
    until(lambda: any(f.endswith(".log") for f in os.listdir(dl)))
    saved = [f for f in os.listdir(dl) if f.endswith(".log")]
    ok("one watchparty-<date>.log", len(saved) == 1 and saved[0].startswith("watchparty-"), str(saved))
    chart = open(f"{dl}/{saved[0]}").read() if saved else ""
    ok("it holds the party's log", "init done" in chart and invite in chart)

    print("\n[G9] the guest pastes the whole invite line and joins, after mpv takes its time")
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

    print(f"\n[G10] a resume in {G}'s mpv: the feeds say who did it")
    setp(G, "pause", False)
    until(lambda: get(H, "pause") is False and f"resumed by {G}" in feed(H) and feed(G)[:1] == ["resumed by you"])
    ok(f"{H}'s mpv resumed", get(H, "pause") is False)
    ok(f"{H}'s feed credits {G}", f"resumed by {G}" in feed(H), str(feed(H)))
    ok(f"{G}'s feed says you", feed(G)[:1] == ["resumed by you"], str(feed(G)))

    print("\n[G11] a seek while playing pauses everyone, and the feeds say why")
    ipc(nodes[G]["sock"], "seek", 30)
    until(lambda: get(H, "pause") is True and f"paused for {G}: seek" in feed(H) and "paused for everyone: seek" in feed(G))
    ok(f"{H}'s feed says why", f"paused for {G}: seek" in feed(H), str(feed(H)))
    ok(f"{G}'s feed says why", "paused for everyone: seek" in feed(G), str(feed(G)))
    ok(f"{G}'s feed doesn't also say paused by you", "paused by you" not in feed(G), str(feed(G)))

    print(f"\n[G12] resuming after the seek shows up in {G}'s feed")
    setp(G, "pause", False)
    until(lambda: get(H, "pause") is False and feed(G)[:1] == ["resumed by you"])
    ok(f"{G}'s feed moved on from the seek", feed(G)[:1] == ["resumed by you"], str(feed(G)))

    print(f"\n[G13] a pause in {H}'s mpv")
    setp(H, "pause", True)
    until(lambda: get(G, "pause") is True and feed(H)[:1] == ["paused by you"] and f"paused by {H}" in feed(G))
    ok(f"{H}'s feed says you", feed(H)[:1] == ["paused by you"], str(feed(H)))
    ok(f"{G}'s feed credits {H}", f"paused by {H}" in feed(G), str(feed(G)))

    print(f"\n[G14] {H}'s history has the real room and secret, and who came")
    last = state(H)["history"][-1]
    ok("room and secret from the invite", (last.get("room"), last.get("secret")) == (room, secret), str(last))
    ok(f"{G} is in its people", last.get("people") == [G], str(last.get("people")))

    print(f"\n[G15] {G} ends the party from the page")
    asked = end(G)
    ok("it asked first", asked == "End the party? mpv closes for you, and everyone else keeps watching.", str(asked))
    ok(f"{G} shows the form again", js(G, '!$("fields").hidden && $("go-label").textContent == "Start the party"'))
    ok(f"{G}'s mpv closed", until(lambda: not mpv_of(G)))
    ok(f"{H}'s feed says {G} left", until(lambda: f"{G} has left" in feed(H)), str(feed(H)))
    ok("Update and Restart are back on", js(G, '!$("update").disabled && !$("restart").disabled'))

    print("\n[G16] a missing mpv is diagnosed as one")
    save_settings(G, mpv="/nonexistent/mpv")
    go(G)
    until(lambda: status(G).startswith("Diagnosis"), BOOT)
    ok("it says mpv isn't installed", status(G).startswith("Diagnosis: mpv isn't installed"), status(G))
    ok("in red, with the chart open", js(G, '$("status").classList.contains("bad") && $("chart").open'))
    save_settings(G, mpv="")

    print("\n[G17] a wrong secret, and a different file, are diagnosed")
    fill(G, secret="wrong" + secret)
    go(G)
    until(lambda: status(G).startswith("Diagnosis"), BOOT)
    ok("the relay rejected the secret", status(G) == "Diagnosis: the relay rejected this room or secret. Check the invite and start again.", status(G))
    _, diff = variants()
    fill(G, secret=secret, file=diff)
    go(G)
    until(lambda: status(G).startswith("Diagnosis: your video"), BOOT)
    ok("the relay rejected the file", status(G) == "Diagnosis: your video is not the same file as the party's. Pick the same video and start again.", status(G))
    ok(f"{G}'s party ended", until(lambda: js(G, '!$("fields").hidden')))

    print("\n[G18] a guest who comes first waits for the host, then gets in by itself")
    end(H)
    start(G, file=MEDIA, room="e2ewait", secret="e2ewaitsecret")
    until(lambda: status(G).startswith("Waiting"), BOOT)
    ok(f"{G} is waiting for the host", status(G) == "Waiting for the host to open this room. It starts by itself once they're in.", status(G))
    start(H, room="e2ewait", secret="e2ewaitsecret")
    must(f"{H} opened it", joined(H), status(H))
    must(f"{G} got in", joined(G), status(G))

    print("\n[G19] losing the relay shows on both, and both come back")
    relay.kill(); relay.wait(timeout=10)
    until(lambda: all(status(n).startswith("Lost the relay") for n in (H, G)))
    for n in (H, G):
        ok(f"{n} says it is reconnecting", status(n) == "Lost the relay. Reconnecting…", status(n))
    relay = launch_relay({"CREATE_KEY": KEY})
    until(lambda: all(status(n).startswith("You're in") for n in (H, G)), 40)
    for n in (H, G):
        must(f"{n} is back in", status(n).startswith("You're in"), status(n))
    for n in (G, H):
        end(n)

    print("\n[G20] a Wi-Fi party: the invite switches the guest to Same Wi-Fi")
    js(H, 'setMode("direct"); $("addrs").value = ""; $("psecret").value = ""')
    start(H, port=str(PORTS["wifiHost"]))
    must(f"{H} is in", joined(H), status(H))
    until(lambda: "-addrs" in js(H, '$("share-text").textContent'))
    wifi = js(H, '$("share-text").textContent')
    addr, wsecret = wifi.split()[1], wifi.split()[3]
    ok(f"{H}'s invite has its Wi-Fi address", wifi.startswith("-addrs ") and addr.endswith(f":{PORTS['wifiHost']}"), wifi)
    js(G, f'$("invite").value = {json.dumps(wifi)}; $("invite").dispatchEvent(new Event("input"))')
    ok(f"{G} switched to Same Wi-Fi and filled it in", js(G, f'mode() == "direct" && $("addrs").value == {json.dumps(addr)} && $("psecret").value == {json.dumps(wsecret)}'))
    start(G, port=str(PORTS["wifiGuest"]))
    must(f"{G} is in", joined(G), status(G))
    until(lambda: f"{G} has joined" in feed(H) or f"connected to {G}" in feed(H))
    setp(H, "pause", False)
    until(lambda: get(G, "pause") is False and f"resumed by {H}" in feed(G))
    ok(f"{G} follows {H} over Wi-Fi", get(G, "pause") is False and f"resumed by {H}" in feed(G), str(feed(G)))

    print(f"\n[G21] quitting mpv ends {G}'s party without a diagnosis")
    ipc(nodes[G]["sock"], "quit")
    until(lambda: not js(G, "running"))
    ok(f"{G} says it ended", status(G) == "The party has ended.", status(G))
    ok(f"{H}'s feed says {G} left", until(lambda: f"{G} has left" in feed(H)), str(feed(H)))

    print(f"\n[G22] SIGTERM ends {H} mid-party, mpv and all")
    sock = nodes[H]["sock"]
    nodes[H]["proc"].send_signal(signal.SIGTERM)
    ok(f"{H} exited", until(lambda: nodes[H]["proc"].poll() is not None, 20))
    ok(f"{H}'s mpv closed", not listening_unix(sock))

    print(f"\n[G23] {G}'s history: every party, and Watch again for both modes")
    tab(G, "history")
    until(lambda: js(G, '$("history-list").children.length') == len(state(G)["history"]))
    items = js(G, '[...$("history-list").children].map(li => li.textContent)')
    ok("newest first, a Wi-Fi party with the host", "on Wi-Fi" in items[0] and f"with {H}" in items[0], items[0])
    ok("the first is over the internet with the host", "over the internet" in items[-1] and f"with {H}" in items[-1], items[-1])
    click(G, "#history-list li .again")
    ok("Watch again filled Same Wi-Fi", js(G, f'mode() == "direct" && $("addrs").value == {json.dumps(addr)} && $("psecret").value == {json.dumps(wsecret)} && !$("view-party").hidden'))
    tab(G, "history")
    click(G, "#history-list li:last-child .again")
    ok("and the relay party", js(G, f'mode() == "relay" && $("room").value == {json.dumps(room)} && $("file").value == {json.dumps(MEDIA)}'))

    print(f"\n[G24] closing {G}'s window ends its GUI")
    browser = next(c for c in children(nodes[G]["proc"].pid) if b"--remote-debugging-port" in open(f"/proc/{c}/cmdline", "rb").read())
    os.kill(browser, signal.SIGTERM)
    ok(f"{G} exited", until(lambda: nodes[G]["proc"].poll() is not None))
    ok(f"{G} logged it", "gui: window closed" in log(G))

    print(f"\n[G25] {G} again, on the same folder: everything was kept")
    count = len(items)
    G2 = launch_gui(G, tag="guiGuest2", portal=False)
    ok("a new port", nodes[G2]["url"] != nodes[G]["url"])
    ok("the name, relay and Wi-Fi port are filled in", js(G2, f'$("hostname").value == {json.dumps(G)} && $("relay").value == {json.dumps(RELAY_URL)} '
                                                         f'&& $("port").value == {json.dumps(str(PORTS["wifiGuest"]))} && $("local").checked'))
    tab(G2, "settings")
    ok("the settings are kept", js(G2, f'$("mpvFlags").value == {json.dumps(MPVFLAGS)} && $("set-relay").value == {json.dumps(RELAY_URL)}'))
    tab(G2, "history")
    ok("the history is kept", until(lambda: js(G2, '$("history-list").children.length') == count))

    print("\n[G26] Clear history asks, then empties it")
    click(G2, "#clear-history")
    until(lambda: js(G2, '$("history-list").children.length') == 0)
    ok("it asked first", js(G2, "window.asked") == "Clear the whole party history?")
    ok("the list is empty and says so", js(G2, '!$("history-empty").hidden && $("clear-history").hidden'))
    ok("the store is empty", not state(G2)["history"])

    print("\n[G27] the cat")
    js(G2, 'document.querySelector(".cat").dispatchEvent(new MouseEvent("click", {bubbles: true}))')
    ok("petting it makes a heart", js(G2, 'document.querySelectorAll(".heart").length') == 1)
    stop(G2)

    print("\n[G28] self-update: offered, refused on a bad digest, installed on a good one")
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

    print("\n[G29] Restart now hands over to the new version")
    click(U, "#restart")
    ok("the old one exited", until(lambda: nodes[U]["proc"].poll() is not None))
    attach(U, 2)
    until(lambda: js(U, '$("version-text").textContent') != "Checking the version…")
    ok("the new window runs v0.0.2", js(U, '$("version-text").textContent') == "Version v0.0.2", js(U, '$("version-text").textContent'))
    ok("and offers nothing", js(U, '$("update").hidden && $("restart").hidden'))

    print("\n[G30] a browser that hands the page off doesn't count as closing the window")
    O = launch_gui("guiHandoff", browser="handoff")
    until(lambda: launches(O))
    time.sleep(QUIET)
    ok("the GUI is still up", nodes[O]["proc"].poll() is None and listening(int(nodes[O]["url"].rsplit(":", 1)[1].strip("/"))))
    ok("and didn't log a closed window", "gui: window closed" not in log(O))
    stop(O)

    print("\n[G31] with no browser at all, it says where the page is")
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

    print("\n[G32] window.close() from the page closes a real window, and the GUI with it")
    # headless Chromium ignores window.close() on an --app window; Restart now relies on it
    try:
        js(X, "window.close()")
    except (OSError, RuntimeError, ValueError):
        pass  # the page can go before it answers
    ok("the GUI exited", until(lambda: nodes[X]["proc"].poll() is not None))
    ok("on a closed window", "gui: window closed" in log(X), log(X).strip())


def listening_unix(path):
    s = socket.socket(socket.AF_UNIX)
    try:
        s.connect(path)
        return True
    except OSError:
        return False
    finally:
        s.close()


try:
    {"p2p": p2p_suite, "relay": relay_suite, "caps": caps_suite, "gui": gui_suite}[args.mode]()
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

print(f"\nlogs: {RUN}")
print("ALL PASS" if not fails else f"{len(fails)} FAILURES: " + ", ".join(fails))
sys.exit(1 if fails else 0)
