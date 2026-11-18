"""Probe the shipped script (../watchparty.lua) as the node sees it: its messages carry only
pause and time-pos, and the node hears the entry and the cache from mpv's own reports, so a
snapshot here is the script's message with the entry and cache mpv last reported, and mpv's
reports of those two count as snapshots too. Runs probe3's experiments, plus a move to a slow
entry and to one mpv can't open.
Usage: python3 -I probe4.py <go|moves|buffering|slow|broken> <rounds>. Under load:
./run.sh 0 probe4.py go:420 moves:280 buffering:40 slow:6 broken:20"""
import http.server, os, socketserver, sys, threading, time

HERE = os.path.dirname(os.path.abspath(__file__))
exec(open(os.path.join(HERE, "probe3.py")).read().rpartition('if __name__ == "__main__":')[0])

NAME = "watchparty"
WP = ["--script=../watchparty.lua"]
_VIEW = {}


class M(M):
    def __init__(self, video, extra=()):
        super().__init__(video, extra)
        # what the node observes over IPC besides the script
        for i, name in enumerate(["playlist-pos", "paused-for-cache"], 50):
            self.wait(reply(self.send("observe_property_string", i, name)))


def enrich(m):
    st = m.__dict__.setdefault("_st", {"done": 0, "entry": None, "cache": None, "pause": None, "pos": None})
    with m.cv:
        start, lines = st["done"], m.lines[st["done"]:]
    for l in lines:
        if l.get("event") == "property-change" and l.get("name") in ("playlist-pos", "paused-for-cache") and l.get("data") is not None:
            key = "entry" if l["name"] == "playlist-pos" else "cache"
            st[key] = l["data"]
            l["_wp"] = dict(kind=key, id="", pause=st["pause"], pos=st["pos"], entry=st["entry"], cache=st["cache"])
        elif l.get("event") == "client-message" and l.get("args", [None])[0] == "watchparty" and len(l["args"]) >= 5:
            a = l["args"]
            st["pause"], st["pos"] = a[3], a[4]
            d = dict(kind=a[1], id=a[2], pause=a[3], pos=a[4], entry=st["entry"], cache=st["cache"])
            l["_wp"], _VIEW[id(a)] = d, d
    st["done"] = start + len(lines)


def W(a):
    return _VIEW[id(a)]


def is_snap(l):
    return "_wp" in l


def snaps(m, i=0):
    enrich(m)
    return [(n, l["_wp"]) for n, l in enumerate(m.since(i), i) if is_snap(l)]


def wsnap(m, pred, i, timeout=3):
    end = time.time() + timeout
    while True:
        enrich(m)
        for n, l in enumerate(m.since(i), i):
            if is_snap(l) and pred(l["_wp"]):
                return n
        if time.time() > end:
            return None
        time.sleep(0.02)


def to(m, cmd, *args):
    return m.send("script-message-to", NAME, cmd.replace("wp-", "watchparty-"), *[str(a) for a in args])


class Late(http.server.SimpleHTTPRequestHandler):
    """Serves a file only after a wait, as a stream that is slow to start."""
    def send_head(self):
        time.sleep(8)
        return super().send_head()

    def log_message(self, *a):
        pass


# A move to an entry that takes 8s to start: the ack comes once its file loads, on it.
def slow(rounds):
    srv = socketserver.ThreadingTCPServer(("127.0.0.1", 0), lambda *a, **k: Late(*a, directory=HERE, **k))
    srv.daemon_threads = True
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    url = f"http://127.0.0.1:{srv.server_address[1]}/p1.mkv"
    errors = {"ack-late": [], "early": [], "away": [], "dropped": []}
    shapes = {}
    for r in range(rounds):
        m = M(url, [*WP, os.path.join(HERE, "p0.mkv")])  # M puts the file last
        ready(m, [])
        want, second = ["yes", "no"][r % 2], [None, 3.5][(r // 2) % 2]
        tag = f"s{r}"
        i, t0 = m.mark(), time.time()
        to(m, "wp-go", want, second if second else "-", 1, tag)
        k = wsnap(m, lambda s: s["kind"] == "ack" and s["id"] == tag, i, 20)
        took = time.time() - t0
        if k is None:
            errors["ack-late"].append((want, second))
            m.stop()
            continue
        loaded = m.wait(lambda l: l.get("event") == "file-loaded", i, 0)
        if loaded is None or loaded > k:
            errors["early"].append((want, second, round(took, 1)))
        time.sleep(2)
        fp, fe, fpos = get(m, "pause"), get(m, "playlist-pos"), get(m, "time-pos")
        after = [(s["kind"], s["id"] == tag, s["pause"], s["pos"]) for _, s in snaps(m, k + 1) if s["kind"] != "cache"]
        # the second only stopped: playing, mpv has moved on from it
        if fp != want or fe != "1" or second and want == "yes" and not fnear(fpos, second, 0.15):
            errors["away"].append((want, second, fp, fe, fpos, W(m.lines[k]["args"])["pos"], after))
        # the go's seek must run, with its id: acked and never run, mpv stays at the file's start
        if second and not any(a[0] == "seek" and a[1] for a in after):
            errors["dropped"].append((want, fpos, after))
        tally(shapes, (want, "seek" if second else "-", f"acked after {int(took)}s", str(after)[:80]))
        m.stop()
    srv.shutdown()
    report("slow", rounds, errors, shapes)


# A move to an entry mpv can't open: no file-loaded comes for it, and the go is acked at its
# end-file, or at the file mpv opens instead.
def broken(rounds):
    # its own file: runs on two versions share this folder
    fd, bad = tempfile.mkstemp(suffix=".mkv", prefix="tmp_broken", dir=HERE)
    with os.fdopen(fd, "wb") as f:
        f.write(b"not a video" * 100)
    errors = {"ack-late": []}
    shapes = {}
    try:
        for r in range(rounds):
            m = M(os.path.join(HERE, "p1.mkv"), [*WP, os.path.join(HERE, "p0.mkv"), bad])
            ready(m, [])
            tag = f"x{r}"
            i = m.mark()
            to(m, "wp-go", ["yes", "no"][r % 2], "-", 1, tag)
            k = wsnap(m, lambda s: s["kind"] == "ack" and s["id"] == tag, i, 10)
            if k is None:
                errors["ack-late"].append(r)
                m.stop()
                continue
            time.sleep(0.5)
            tally(shapes, (["yes", "no"][r % 2], "ends on entry " + str(get(m, "playlist-pos")), "pause " + str(get(m, "pause"))))
            m.stop()
    finally:
        os.remove(bad)
    report("broken", rounds, errors, shapes)


if __name__ == "__main__":
    os.chdir(HERE)
    exp, n = sys.argv[1], int(sys.argv[2])
    globals()[exp](n)
