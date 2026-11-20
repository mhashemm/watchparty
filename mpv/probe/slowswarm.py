"""Moves over a slow swarm: two CLI nodes on a local relay play a torrent of three videos
from a seeder capped at RATE bytes/s (party/fakeseed), so each entry they move to is still
downloading. Each round does something by hand on one node's mpv (a second IPC client, as a
key is) and then checks the protocol's end state: both paused on one entry and one second.
Linux only (it finds mpv's socket in /proc). Builds the CLI, the relay and fakeseed from this
checkout; needs go, mpv and ffmpeg. Exits 1 if any round ends apart, and keeps its logs then.
Usage: python3 -I slowswarm.py <rounds> [rate], 30 rounds take about 15 minutes."""
import glob, json, os, random, shutil, signal, socket, subprocess, sys, tempfile, time

ROUNDS = int(sys.argv[1])
RATE = int(sys.argv[2]) if len(sys.argv) > 2 else 300_000
HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
T = tempfile.mkdtemp(prefix="tmpswarm-", dir=HERE)
BIN, RELAY, SEED = f"{T}/build/watchparty", f"{T}/build/relay", f"{T}/build/fakeseed"
for out, pkg in ((BIN, "."), (RELAY, "./relay"), (SEED, "./party/fakeseed")):
    subprocess.run(["go", "build", "-o", out, pkg], cwd=REPO, check=True)
procs = []

# isolated: no desktop, bus or browser (CLAUDE.md, "Verifying without touching the user's machine")
os.makedirs(f"{T}/bin")
os.symlink(shutil.which("mpv") or sys.exit("slowswarm.py needs mpv on PATH"), f"{T}/bin/mpv")
with open(f"{T}/bin/xdg-open", "w") as f:
    f.write("#!/bin/sh\nexit 0\n")
os.chmod(f"{T}/bin/xdg-open", 0o755)
ENV = {"HOME": T, "PATH": f"{T}/bin", "XDG_CONFIG_HOME": f"{T}/.config", "XDG_CACHE_HOME": f"{T}/.cache",
       "XDG_DATA_HOME": f"{T}/.local/share", "XDG_RUNTIME_DIR": T, "TMPDIR": T,
       "DBUS_SESSION_BUS_ADDRESS": f"unix:path={T}/no-bus"}


def start(cmd, name, env=None):
    p = subprocess.Popen(cmd, env={**ENV, **(env or {})}, stdout=open(f"{T}/{name}.log", "wb"), stderr=subprocess.STDOUT)
    procs.append(p)
    return p


def log(name):
    with open(f"{T}/{name}.log", errors="replace") as f:
        return f.read()


def until(cond, limit):
    end = time.time() + limit
    while time.time() < end:
        v = cond()
        if v:
            return v
        time.sleep(0.1)
    return None


def child_sock(pid):
    for d in glob.glob("/proc/[0-9]*"):
        try:
            if f"PPid:\t{pid}\n" not in open(d + "/status").read():
                continue
            for part in open(d + "/cmdline", "rb").read().split(b"\0"):
                if part.startswith(b"--input-ipc-server="):
                    return part.split(b"=", 1)[1].decode()
        except OSError:
            pass


def ipc(sock, *cmd):
    s = socket.socket(socket.AF_UNIX)
    s.settimeout(5)
    try:
        s.connect(sock)
        s.sendall((json.dumps({"command": list(cmd), "request_id": 7}) + "\n").encode())
        buf = b""
        while True:
            buf += s.recv(65536)
            for line in buf.split(b"\n"):
                if line.strip() and json.loads(line).get("request_id") == 7:
                    return json.loads(line).get("data")
    except (OSError, ValueError):
        return None
    finally:
        s.close()


def get(n, prop):
    return ipc(nodes[n], "get_property", prop)


def state(n):
    return get(n, "pause"), get(n, "playlist-pos"), get(n, "time-pos")


def in_step():
    (pa, ea, ta), (pb, eb, tb) = state("a"), state("b")
    return pa is True and pb is True and ea == eb and ta is not None and tb is not None and abs(ta - tb) <= 0.25


# three videos ~2.3MB each (~150KB/s for 15s): an entry the party moves to is still coming
pack = f"{T}/pack"
os.makedirs(pack)
for k, src in enumerate(("testsrc", "testsrc2", "smptebars")):
    subprocess.run(["ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", f"{src}=size=320x240:rate=24,noise=alls=60:allf=t",
                    "-t", "15", "-c:v", "libx264", "-b:v", "1.2M", "-g", "48", "-pix_fmt", "yuv420p", f"{pack}/v{k + 1}.mkv"], check=True)
seeder = subprocess.Popen([SEED, pack, str(RATE)], stdout=subprocess.PIPE, text=True, env=ENV)
procs.append(seeder)
assert seeder.stdout  # stdout=PIPE
magnet = seeder.stdout.readline().strip()
port = random.randint(30000, 40000)
start([RELAY], "relay", {"ADDR": f"127.0.0.1:{port}", "DB": f"{T}/relay.db", "CREATE_KEY": "k", "ROOMS_PER_HOUR": "1000"})
until(lambda: socket.socket().connect_ex(("127.0.0.1", port)) == 0, 10)
common = ["-file", magnet, "-relay", f"ws://127.0.0.1:{port}", "-room", "swarm", "-secret", "s",
          "-mpvFlags", "--vo=null --ao=null --msg-level=all=error"]
nodes = {}
pa = start([BIN, *common, "-hostname", "a", "-createKey", "k", "-cacheDir", f"{T}/ca"], "a")
until(lambda: "relay: connected" in log("a") and child_sock(pa.pid), 120)
nodes["a"] = child_sock(pa.pid)
pb = start([BIN, *common, "-hostname", "b", "-cacheDir", f"{T}/cb"], "b")
until(lambda: "b has joined" in log("a") or "connected to a" in log("b"), 180)
until(lambda: child_sock(pb.pid), 30)
nodes["b"] = child_sock(pb.pid)
print("both in:", bool(until(in_step, 120)), state("a"), state("b"))

def take_lead(who, other):
    """who's resume starts everyone from its screen, and its stop leads, so what it does next
    goes out (a follower's move or seek stays on its own screen). A key while a go is still on
    its way is lost (mpv/CLAUDE.md), as while an entry the party moved to hasn't started on
    the slow swarm: so the resume counts once the other plays, else it is pressed again."""
    for _ in range(6):
        ipc(nodes[who], "set_property", "pause", False)
        if until(lambda: get(other, "pause") is False, 10):
            time.sleep(0.7)
            ipc(nodes[who], "set_property", "pause", True)
            return until(in_step, 90)
        ipc(nodes[who], "set_property", "pause", True)
        time.sleep(2)
    return None


bad = []
for r in range(ROUNDS):
    who, other = random.choice([("a", "b"), ("b", "a")])
    kind = ["next", "prev", "next+seek", "play+next", "seek-while-loading"][r % 5]
    lead = take_lead(who, other)
    if kind == "next":
        ipc(nodes[who], "playlist-next")
    elif kind == "prev":
        ipc(nodes[who], "playlist-prev")
    elif kind == "next+seek":
        ipc(nodes[who], "playlist-next")
        until(lambda: get(who, "time-pos") is not None, 60)
        ipc(nodes[who], "seek", 7, "absolute")
    elif kind == "play+next":
        ipc(nodes[who], "set_property", "pause", False)
        time.sleep(1.5)
        # mpv quits at the last entry's end
        ipc(nodes[who], "playlist-prev" if get(who, "playlist-pos") == 2 else "playlist-next")
    elif kind == "seek-while-loading":
        # the mover leads the stop, so its seek goes out while the other still loads; a
        # follower's would stay on its own screen
        ipc(nodes[who], "playlist-next")
        time.sleep(random.uniform(0.2, 1.5))
        ipc(nodes[who], "seek", 5, "absolute")
    time.sleep(1)
    ok = until(in_step, 90)
    # settled: still so a moment later
    time.sleep(2)
    ok = ok and in_step()
    print(f"round {r:2} {kind:18} by {who}: {'in step' if ok else 'APART'}{'' if lead else ' (no lead taken before it)'}  a={state('a')} b={state('b')}", flush=True)
    if not ok:
        bad.append((r, kind, who))
    for n in ("a", "b"):
        if "did not answer in time" in log(n):
            print(f"  {n}: a go timed out")

print(f"rounds={ROUNDS} apart={len(bad)} {bad}")
print("a overruled notices:", log("a").count("press it again"), "b:", log("b").count("press it again"))
for p in procs:
    p.send_signal(signal.SIGTERM)
for p in procs:
    try:
        p.wait(20)
    except subprocess.TimeoutExpired:
        p.kill()
if bad:
    print("logs in", T)
    sys.exit(1)
shutil.rmtree(T)
