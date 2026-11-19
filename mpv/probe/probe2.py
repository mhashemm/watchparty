"""Probe the whole script design (wp2.lua) against everything the node does with mpv.
Usage: python3 -I probe2.py <experiment> <rounds> (mpv/ORDER.md holds the results).
Experiments: startup, pause, seekp, playing, moves, buffering, ending, mixed, began. Run under
load as: python3 run.py 0 probe2.py startup:60 pause:280 seekp:420 playing:280 moves:280 buffering:40
ending:60, and LANDING=began python3 run.py 0 probe2.py began:80 seekp:420 playing:280"""
import http.server, os, socketserver, sys, threading, time

HERE = os.path.dirname(os.path.abspath(__file__))
exec(open(os.path.join(HERE, "probe.py")).read().split('if __name__ == "__main__":')[0])

NAME = "wp2"
# what counts as where a seek went: "landed" at playback-restart, or "began" at the seek event
LANDING = os.environ.get("LANDING", "landed")
WP = ["--script=wp2.lua"]


class M(Mpv):
    def read(self, s):
        self.eof = False
        try:
            Mpv.read(self, s)
        finally:
            with self.cv:
                self.eof = True
                self.cv.notify_all()


def W(a):
    return dict(kind=a[1], id=a[2], pause=a[3], pos=a[4], entry=a[5], cache=a[6])


def is_snap(l):
    return l.get("event") == "client-message" and l.get("args", [None])[0] == "wp"


def snaps(m, i=0):
    return [(n, W(l["args"])) for n, l in enumerate(m.since(i), i) if is_snap(l)]


def wsnap(m, pred, i, timeout=3):
    return m.wait(lambda l: is_snap(l) and pred(W(l["args"])), i, timeout)


def get(m, name):
    i = m.mark()
    j = m.wait(reply(m.send("get_property_string", name)), i)
    return None if j is None else m.lines[j].get("data")


def to(m, *args):
    return m.send("script-message-to", NAME, *[str(a) for a in args])


def person_conn(m):
    p = m.conn()
    drain(p)
    return p


def fnear(a, b, tol=0.1):
    try:
        return abs(float(a) - float(b)) < tol
    except (TypeError, ValueError):
        return False


def stopped(m):
    """Pause plainly and wait until quiet, outside any measured window."""
    m.wait(reply(m.send("set_property", "pause", True)))
    time.sleep(0.1)


def setpos(m, pos):
    i = m.mark()
    m.wait(reply(m.send("set_property", "time-pos", str(pos))))
    m.wait(lambda l: l.get("event") == "playback-restart", i)
    time.sleep(0.1)


def tally(d, k):
    d[k] = d.get(k, 0) + 1


# errors counted but harmless: printed, never a failure
HARMLESS = {"spare"}
FAILED = []


def report(title, rounds, errors, shapes, legend=""):
    print(f"{title}: rounds={rounds} " + " ".join(f"{k}={len(v)}" for k, v in errors.items()))
    FAILED.extend(k for k, v in errors.items() if v and k not in HARMLESS)
    for k, v in errors.items():
        for e in v[:4]:
            print(f"  {k}: {e}")
    if legend:
        print(f"  {legend}")
    for k, v in sorted(shapes.items(), key=lambda kv: str(kv[0])):
        print(f"  {v:4d}  {k}")


# 1. Startup: is the script there for the first message, and does a name clash hide it?
def startup(rounds):
    os.makedirs("other", exist_ok=True)
    with open("other/wp2.lua", "w") as f:
        f.write("-- a user's script that happens to share our name\n")
    errors = {"no-hello": [], "plays-before-hello": []}
    shapes = {}
    for r in range(rounds):
        variant = ["plain", "clash", "load-scripts=no"][r % 3]
        extra = {"plain": WP, "clash": ["--script=other/wp2.lua", *WP], "load-scripts=no": ["--load-scripts=no", *WP]}[variant]
        m = M(os.path.join(HERE, "v24.mkv"), extra)
        i = m.mark()
        rid = m.send("script-message-to", NAME, "wp-hello")
        rid2 = m.send("script-message", "wp-hello")
        rto = m.lines[m.wait(reply(rid), i)].get("error")
        h = wsnap(m, lambda s: s["kind"] == "hello", i, 5)
        names = sorted({s["id"] for _, s in snaps(m, i) if s["kind"] == "hello"}) if h is not None else []
        restart = m.wait(lambda l: l.get("event") == "playback-restart", i, 5)
        first = snaps(m, i)[:1]
        if h is None:
            errors["no-hello"].append(variant)
        tally(shapes, (variant, f"script-message-to {NAME}: {rto}", f"hello from {names}",
                       f"first snapshot {first[0][1]['kind'] if first else '-'}"))
        m.stop()
    report("startup", rounds, errors, shapes)


# 2. Pause and resume, ours racing the person's space: the last snapshot is mpv's state, and a
#    stopped snapshot's position is where mpv stopped (what every pause carries, `at`).
def pause(rounds):
    m = M(os.path.join(HERE, "v24.mkv"), WP)
    ready(m, [])
    p = person_conn(m)
    errors = {"no-ack": [], "pause-wrong": [], "pos-wrong": []}
    shapes, worst = {}, 0.0
    for r in range(rounds):
        playing_first = r % 2 == 0
        m.wait(reply(m.send("set_property", "pause", not playing_first)))
        time.sleep(0.08)
        want = "yes" if playing_first else "no"
        off = OFFSETS_MS[(r // 2) % len(OFFSETS_MS)]
        tag = f"p{r}"
        i = m.mark()
        race(m, p, "space", lambda: to(m, "wp-pause", want, tag), off)
        a = wsnap(m, lambda s: s["kind"] == "ack" and s["id"] == tag, i)
        if a is None:
            errors["no-ack"].append(off)
            continue
        time.sleep(0.3)
        sp, spos = get(m, "pause"), get(m, "time-pos")
        last = snaps(m, i)[-1][1]
        if last["pause"] != sp:
            errors["pause-wrong"].append((want, off, [(s["kind"], s["pause"]) for _, s in snaps(m, i)], sp))
        if sp == "yes" and last["pause"] == "yes":
            d = abs(float(last["pos"]) - float(spos))
            worst = max(worst, d)
            if d > 0.001:
                errors["pos-wrong"].append((want, off, last, spos))
        tally(shapes, (f"ours={want}", f"{off:+d}ms", " ".join(f"{s['kind']}:{s['pause']}" for _, s in snaps(m, i)), f"settled={sp}"))
    m.stop()
    report(f"pause (worst stopped-position gap {worst:.6f}s)", rounds, errors, shapes)


# 3. Seeks while paused, through the full script (regression of lua_seek).
def seekp(rounds):
    conf = os.path.join(HERE, "input.conf")
    with open(conf, "w") as f:
        f.write("F1 seek 7 absolute\n")
    m = M(os.path.join(HERE, "v24.mkv"), [*WP, f"--input-conf={os.path.relpath(conf)}"])
    ready(m, [])
    p = person_conn(m)
    kinds = ["none", "here", "rel", "abskey", "absipc", "space"]
    errors = {"leak": [], "lost": [], "wrong": [], "no-landing": [], "pause-wrong": []}
    shapes = {}
    for r in range(rounds):
        kind = kinds[r % len(kinds)]
        off = OFFSETS_MS[(r // len(kinds)) % len(OFFSETS_MS)]
        stopped(m)
        base = 20 + (r % 40) * 2
        setpos(m, base)
        target = base if kind == "here" else base + 50
        tag = f"s{r}"
        i = m.mark()
        race(m, p, kind, lambda: to(m, "wp-seek", target, tag), off)
        time.sleep(0.6)
        sp, spos = get(m, "pause"), get(m, "time-pos")
        ss = snaps(m, i)
        land = [s for _, s in ss if s["kind"] == LANDING]
        if not any(s["id"] == tag for s in land):
            errors["no-landing"].append((kind, off, ss))
            continue
        ours = lambda s: s["id"] == tag and fnear(s["pos"], target)
        k = next(n for n, s in enumerate(land) if s["id"] == tag)
        if fnear(spos, target) and sp == "yes" and any(not ours(s) for s in land[k:]):
            errors["leak"].append((kind, off, land, spos))
        if not fnear(spos, target) and ours(land[-1]) and sp == "yes":
            errors["lost"].append((kind, off, land, spos))
        if sp == "yes" and not fnear(ss[-1][1]["pos"], spos, 0.001):
            errors["wrong"].append((kind, off, ss[-1][1], spos))
        if ss[-1][1]["pause"] != sp:
            errors["pause-wrong"].append((kind, off, ss, sp))
        tally(shapes, (kind, " ".join("O" if ours(s) else "o" if s["id"] == tag else "p" for s in land), "at target" if fnear(spos, target) else "elsewhere"))
    m.stop()
    report("seekp", rounds, errors, shapes, "landings: O ours at target, o ours carried elsewhere, p the person's")


# 4. While playing. A: the person seeks, the node answers by pausing the party (pauseParty) while
#    the person acts again. B: a peer's resume from its second: our seek, then our resume, racing
#    the person's seek or space.
def playing(rounds):
    conf = os.path.join(HERE, "input.conf")
    with open(conf, "w") as f:
        f.write("F1 seek 7 absolute\n")
    m = M(os.path.join(HERE, "v24.mkv"), [*WP, f"--input-conf={os.path.relpath(conf)}"])
    ready(m, [])
    p = person_conn(m)
    errors = {"A-missed-seek": [], "A-wrong": [], "B-no-landing": [], "B-misread": [], "B-pause-wrong": []}
    shapes = {}
    for r in range(rounds):
        off = OFFSETS_MS[(r // 4) % len(OFFSETS_MS)]
        second = ["rel", "abskey", "space", None][(r // 2) % 4]
        stopped(m)
        base = 20 + (r % 40) * 2
        setpos(m, base)
        tag = f"q{r}"
        if r % 2 == 0:
            m.wait(reply(m.send("set_property", "pause", False)))
            time.sleep(0.15)
            i = m.mark()
            m.send("keypress", "RIGHT", s=p)
            n = wsnap(m, lambda s: s["kind"] == LANDING and s["id"] == "", i)
            if n is None:
                errors["A-missed-seek"].append(off)
                continue
            race(m, p, second, lambda: to(m, "wp-pause", "yes", tag), off)
            time.sleep(0.4)
            sp, spos = get(m, "pause"), get(m, "time-pos")
            last = snaps(m, i)[-1][1]
            ok = last["pause"] == sp and (sp == "no" or fnear(last["pos"], spos, 0.001))
            if not ok:
                errors["A-wrong"].append((second, off, [(s["kind"], s["pause"], s["pos"]) for _, s in snaps(m, i)], sp, spos))
            tally(shapes, ("A", second, " ".join(f"{s['kind']}:{s['pause']}" for _, s in snaps(m, n + 1)), f"settled={sp}"))
        else:
            target = base + 50
            i = m.mark()
            race(m, p, second, lambda: (to(m, "wp-seek", target, tag), to(m, "wp-pause", "no", tag + "r")), off)
            t0 = time.time()
            time.sleep(0.5)
            sp, spos = get(m, "pause"), float(get(m, "time-pos"))
            ss = snaps(m, i)
            land = [s for _, s in ss if s["kind"] == LANDING]
            if not any(s["id"] == tag for s in land):
                errors["B-no-landing"].append((second, off, ss))
                continue
            ours = lambda s: s["id"] == tag and fnear(s["pos"], target)
            # truth from where mpv is: from our target it played at most ~0.6s
            truth = "ours" if target - 0.05 <= spos <= target + 1.2 else "person's"
            if sp == "yes":
                truth = "ours" if fnear(spos, target, 0.7) and spos >= target - 0.05 else "person's"
            k = next(n for n, s in enumerate(land) if s["id"] == tag)
            seen = "ours" if ours(land[-1]) else "person's"
            if seen != truth:
                errors["B-misread"].append((second, off, [(s["kind"], s["id"], s["pause"], s["pos"]) for _, s in ss], sp, spos))
            if ss[-1][1]["pause"] != sp:
                errors["B-pause-wrong"].append((second, off, ss, sp))
            tally(shapes, ("B", second, " ".join("O" if ours(s) else "o" if s["id"] == tag else "p" for s in land), truth, f"settled={sp}"))
    m.stop()
    report("playing", rounds, errors, shapes, "B landings: O ours at target, o ours carried elsewhere, p the person's")


# 5. Playlists: our move racing the person's next/prev; a seek right after our move; there and
#    back; mpv's own move at a file's end.
def moves(rounds):
    files = [os.path.join(HERE, f"p{k}.mkv") for k in range(3)]
    m = M(files[2], [*WP, files[0], files[1]])  # M puts the file last
    ready(m, [])
    p = person_conn(m)
    errors = {"no-load": [], "entry-wrong": [], "misread": [], "seek-lost": [], "stuck": [], "eof-misread": []}
    shapes = {}

    def go(entry):
        stopped(m)
        if get(m, "playlist-pos") != str(entry):
            i = m.mark()
            m.send("set_property", "playlist-pos", entry)
            m.wait(lambda l: l.get("event") == "playback-restart", i, 5)
            time.sleep(0.15)

    for r in range(rounds):
        scen = ["race", "seek", "back", "eof"][r % 4]
        off = OFFSETS_MS[(r // 4) % len(OFFSETS_MS)]
        e0 = r % 3
        go(e0)
        tag = f"m{r}"
        i = m.mark()
        if scen == "race":
            key = [">", "<"][(r // 4) % 2]
            target = (e0 + 2) % 3 if key == ">" else (e0 + 1) % 3  # never where the key goes
            if off < 0:
                m.send("keypress", key, s=p); time.sleep(-off / 1000)
            to(m, "wp-move", target, tag)
            if off >= 0:
                time.sleep(off / 1000); m.send("keypress", key, s=p)
            time.sleep(0.6)
            se = get(m, "playlist-pos")
            loads = [s for _, s in snaps(m, i) if s["kind"] == "loaded"]
            if not loads:
                errors["no-load"].append((scen, key, off, snaps(m, i)))
                continue
            if loads[-1]["entry"] != se:
                errors["entry-wrong"].append((key, off, loads, se))
            ours = lambda s: s["id"] == tag and s["entry"] == str(target)
            if (se == str(target)) != ours(loads[-1]) and not (se == str(target) and loads[-1]["id"] == ""):
                errors["misread"].append((key, off, loads, se))
            tally(shapes, ("race", key, " ".join("O" if ours(s) else "o" if s["id"] == tag else "p" for s in loads), "target" if se == str(target) else "elsewhere"))
        elif scen == "seek":
            target = (e0 + 1) % 3
            to(m, "wp-move", target, tag)
            to(m, "wp-seek", 12.5, tag + "s")
            if off >= 0:
                time.sleep(off / 1000 + 0.05); m.send("keypress", "RIGHT", s=p)
            time.sleep(0.8)
            se, spos = get(m, "playlist-pos"), get(m, "time-pos")
            ss = snaps(m, i)
            land = [s for _, s in ss if s["kind"] == LANDING]
            ours = [s for s in land if s["id"] == tag + "s"]
            if se != str(target) or not ours or ours[0]["entry"] != str(target):
                errors["seek-lost"].append((off, [(s["kind"], s["id"], s["entry"], s["pos"]) for _, s in ss], se, spos))
            elif fnear(spos, 12.5) != (land[-1]["id"] == tag + "s" and fnear(land[-1]["pos"], 12.5)):
                errors["misread"].append(("seek", off, land, spos))
            tally(shapes, ("seek", "person RIGHT" if off >= 0 else "-", " ".join(s["kind"] + (":" + s["id"] if s["id"] else "") for _, s in ss)))
        elif scen == "back":
            a, b = (e0 + 1) % 3, e0
            to(m, "wp-move", a, tag + "a")
            if off > 0:
                time.sleep(off / 1000)
            to(m, "wp-move", b, tag + "b")
            time.sleep(0.8)
            se = get(m, "playlist-pos")
            ss = snaps(m, i)
            loads = [s for _, s in ss if s["kind"] == "loaded"]
            if loads and loads[-1]["entry"] != se:
                errors["entry-wrong"].append(("back", off, loads, se))
            i2 = m.mark()
            to(m, "wp-seek", 3, tag + "x")
            if wsnap(m, lambda s: s["kind"] == LANDING and s["id"] == tag + "x", i2, 2) is None:
                errors["stuck"].append(("back", off, [(s["kind"], s["id"], s["entry"]) for _, s in ss + snaps(m, i2)]))
            tally(shapes, ("back", f"{max(off, 0)}ms", " ".join(s["kind"] + (":" + s["id"] if s["id"] else "") for _, s in ss if s["kind"] in ("loaded", "end", "ack")), f"settled={se}"))
        else:
            setpos(m, float(get(m, "duration")) - 0.4)
            i = m.mark()
            m.send("set_property", "pause", False)
            if off >= 0:
                time.sleep(0.4 + off / 1000); m.send("keypress", ">", s=p)
            time.sleep(1.0)
            se = get(m, "playlist-pos")
            ss = snaps(m, i)
            loads = [s for _, s in ss if s["kind"] == "loaded"]
            if (loads and loads[-1]["entry"] != se) or (not loads and (se != str(e0) or ss[-1][1]["pause"] != "yes")):
                errors["eof-misread"].append((off, [(s["kind"], s["id"], s["entry"]) for _, s in ss], se))
            tally(shapes, ("eof", "person >" if off >= 0 else "-", " ".join(s["kind"] + (":" + s["id"] if s["id"] else "") for _, s in ss if s["kind"] in ("loaded", "end", "pause")), f"settled={se}"))
    m.stop()
    report("moves", rounds, errors, shapes)


# 6. Buffering: a throttled HTTP source runs the cache dry while playing.
class Slow(http.server.SimpleHTTPRequestHandler):
    rate = 70_000  # bytes/s, below the file's ~126 KB/s

    def log_message(self, *a):
        pass

    def copyfile(self, src, dst):
        while True:
            b = src.read(self.rate // 20)
            if not b:
                return
            try:
                dst.write(b)
            except OSError:
                return
            time.sleep(0.05)

    def send_head(self):
        path = self.translate_path(self.path)
        size = os.path.getsize(path)
        start, end = 0, size - 1
        rng = self.headers.get("Range")
        f = open(path, "rb")
        if rng and rng.startswith("bytes="):
            a, _, b = rng[6:].partition("-")
            start = int(a) if a else 0
            end = int(b) if b else size - 1
            self.send_response(206)
            self.send_header("Content-Range", f"bytes {start}-{end}/{size}")
        else:
            self.send_response(200)
        f.seek(start)
        self.send_header("Content-Type", "video/x-matroska")
        self.send_header("Accept-Ranges", "bytes")
        self.send_header("Content-Length", str(end - start + 1))
        self.end_headers()
        return f


def buffering(rounds):
    srv = socketserver.ThreadingTCPServer(("127.0.0.1", 0), lambda *a, **k: Slow(*a, directory=HERE, **k))
    srv.daemon_threads = True
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    url = f"http://127.0.0.1:{srv.server_address[1]}/net.mkv"
    errors = {"no-stall": [], "pause-wrong": [], "cache-wrong": []}
    shapes = {}
    for r in range(rounds):
        m = M(url, [*WP, "--cache=yes", "--demuxer-readahead-secs=1", "--cache-secs=1"])
        p = person_conn(m)
        m.wait(lambda l: l.get("event") == "playback-restart", 0, 10)
        i = m.mark()
        m.send("set_property", "pause", False)
        st = wsnap(m, lambda s: s["kind"] == "cache" and s["cache"] == "yes", i, 15)
        if st is None:
            errors["no-stall"].append(r)
            m.stop()
            continue
        act = ["pause", "resume-while-stalled", "space", "none"][r % 4]
        off = OFFSETS_MS[(r // 4) % len(OFFSETS_MS)]
        tag = f"b{r}"
        if act == "pause":
            race(m, p, "space" if r % 8 < 4 else None, lambda: to(m, "wp-pause", "yes", tag), off)
        elif act == "resume-while-stalled":
            m.send("set_property", "pause", True)
            time.sleep(0.2)
            to(m, "wp-pause", "no", tag)
        elif act == "space":
            m.send("keypress", "SPACE", s=p)
        time.sleep(2.5)
        sp, sc = get(m, "pause"), get(m, "paused-for-cache")
        ss = snaps(m, i)
        last = ss[-1][1]
        if last["pause"] != sp:
            errors["pause-wrong"].append((act, off, [(s["kind"], s["pause"], s["cache"]) for _, s in ss], sp))
        lastc = [s for _, s in ss if s["kind"] == "cache"][-1]
        if lastc["cache"] != sc:
            errors["cache-wrong"].append((act, off, [(s["kind"], s["pause"], s["cache"]) for _, s in ss], sc))
        tally(shapes, (act, " ".join(f"{s['kind']}:{s['pause']}/{s['cache']}" for _, s in ss)[:150], f"settled pause={sp} cache={sc}"))
        m.stop()
    srv.shutdown()
    report("buffering", rounds, errors, shapes)


# 7. Ending: playing into the end with and without keep-open, a quit, a seek past the end.
def ending(rounds):
    errors = {"lost-before-close": [], "no-end": []}
    shapes = {}
    for r in range(rounds):
        scen = ["eof keep-open", "eof", "quit ipc", "quit key", "past end paused", "past end keep-open"][r % 6]
        extra = [*WP] + ([] if "keep-open" in scen else ["--keep-open=no"])
        m = M(os.path.join(HERE, "e.mkv"), extra)
        ready(m, [])
        p = person_conn(m)
        i = m.mark()
        if scen.startswith("eof"):
            setpos(m, 3.4)
            i = m.mark()
            m.send("set_property", "pause", False)
        elif scen == "quit ipc":
            m.send("quit", s=p)
        elif scen == "quit key":
            m.send("keypress", "q", s=p)
        else:
            to(m, "wp-seek", 10, f"e{r}")
        with m.cv:
            m.cv.wait_for(lambda: m.eof, 2.5)
            closed = m.eof
        ss = snaps(m, i)
        if closed and not any(s["kind"] == "end" for _, s in ss):
            errors["lost-before-close"].append((scen, ss))
        tally(shapes, (scen, " ".join(s["kind"] + (":" + s["id"] if s["id"] else "") + f"[{s['pause']} {s['pos'][:6]}]" for _, s in ss), "mpv closed" if closed else "mpv open"))
        m.stop()
    report("ending", rounds, errors, shapes)


# 8. Two streams: pause from the script, time-pos from an IPC observe. Does a playing time-pos
#    come after the script's stopped snapshot (it would read as a seek while paused)?
def mixed(rounds):
    m = M(os.path.join(HERE, "v120.mkv"), WP)
    ready(m, ["time-pos"])
    late, total = [], 0
    for r in range(rounds):
        m.wait(reply(m.send("set_property", "pause", False)))
        time.sleep(0.03 + (r % 7) * 0.01)
        i = m.mark()
        tag = f"x{r}"
        if r % 2:
            to(m, "wp-pause", "yes", tag)
            k = wsnap(m, lambda s: s["kind"] == "ack" and s["id"] == tag, i)
        else:
            m.send("set_property", "pause", True)
            k = wsnap(m, lambda s: s["kind"] == "pause" and s["pause"] == "yes", i)
        time.sleep(0.15)
        if k is None:
            continue
        stopped = W(m.lines[k]["args"])["pos"]
        total += 1
        after = [l["data"] for l in m.since(k + 1) if l.get("event") == "property-change" and l.get("name") == "time-pos"]
        if any(not fnear(a, stopped, 0.0005) for a in after):
            late.append(("ours via script" if r % 2 else "plain set", stopped, after))
    m.stop()
    print(f"mixed: rounds={total} playing time-pos after the script's stopped snapshot: {len(late)}")
    for e in late[:6]:
        print("  ", e)


# 8. A seek and a resume at once ([R6c]): the person seeks while paused and presses space at
#    once, or the node's seek races the space. Is the snapshot at the seek event (began) where the
#    seek lands, and does it come before the resume's change mark?
def began(rounds):
    conf = os.path.join(HERE, "input.conf")
    with open(conf, "w") as f:
        f.write("F1 seek 7 absolute\n")
    m = M(os.path.join(HERE, "v24.mkv"), [*WP, f"--input-conf={os.path.relpath(conf)}"])
    ready(m, [])
    p = person_conn(m)
    kinds = ["rel", "abskey", "absipc", "ours"]
    gaps = [0, 1, 5, 20]
    errors = {"began-off": [], "no-began": [], "began-not-paused": [], "resume-first": []}
    shapes = {}
    for r in range(rounds):
        kind, gap = kinds[r % len(kinds)], gaps[(r // len(kinds)) % len(gaps)]
        stopped(m)
        base = 20 + (r % 40) * 2
        setpos(m, base)
        target = {"rel": base + 5, "abskey": 7, "absipc": 9, "ours": base + 50}[kind]
        tag = f"b{r}"
        i = m.mark()
        if kind == "ours":
            to(m, "wp-seek", target, tag)
        else:
            race(m, p, kind, lambda: None, 0)
        time.sleep(gap / 1000)
        m.send("keypress", "SPACE", s=p)
        time.sleep(0.6)
        ss = [s for _, s in snaps(m, i)]
        order = [s["kind"] + ("-" + s["pause"] if s["kind"] == "pause" else "") for s in ss if s["kind"] in ("began", "landed", "pause")]
        b = [s for s in ss if s["kind"] == "began"]
        land = [s for s in ss if s["kind"] == "landed"]
        if land and not b:
            errors["no-began"].append((kind, gap, order))
        elif b and land and not fnear(b[0]["pos"], land[-1]["pos"]):
            errors["began-off"].append((kind, gap, b[0], land[-1]))
        if b and order.index("began") < (order.index("pause-no") if "pause-no" in order else 99) and b[0]["pause"] != "yes":
            errors["began-not-paused"].append((kind, gap, order, b[0]))
        if "pause-no" in order and b and order.index("pause-no") < order.index("began"):
            errors["resume-first"].append((kind, gap, order))
        tally(shapes, (kind, " ".join(order)))
    m.stop()
    report("began", rounds, errors, shapes)


if __name__ == "__main__":
    os.chdir(HERE)
    exp, n = sys.argv[1], int(sys.argv[2])
    globals()[exp](n)
