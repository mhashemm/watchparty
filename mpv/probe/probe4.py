"""Probe the shipped script (../watchparty.lua) as the node sees it: its messages carry only
pause and time-pos, and the node hears the entry and the cache from mpv's own reports, so a
snapshot here is the script's message with the entry and cache mpv last reported, and mpv's
reports of those two count as snapshots too. Runs probe3's experiments, plus a move to a slow
entry and to one mpv can't open.
Usage: python3 -I probe4.py <go|moves|buffering|slow|broken|brokenlast|hang> <rounds>. Under load:
python3 run.py 0 probe4.py go:420 moves:280 buffering:40 slow:12 broken:20 brokenlast:20"""
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
            d = dict(kind=a[1], id=a[2], pause=a[3], pos=a[4], entry=st["entry"], cache=st["cache"],
                     n=a[5] if len(a) > 5 else "", over=len(a) > 6 and a[6] == "over", at=a[7] if len(a) > 7 else "")
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
    """A go also says the number of the last script message read before it, as the node does;
    m.sent is how many lines had come then."""
    if cmd == "wp-go":
        with m.cv:
            m.sent = len(m.lines)
            seen = next((l["args"][5] for l in reversed(m.lines) if l.get("event") == "client-message"
                         and l.get("args", [None])[0] == "watchparty" and len(l["args"]) > 5), "")
        args = (*args[:4], seen, *args[4:])
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
# end-file, or at the file mpv opens instead. last: it is the last entry, and --idle keeps mpv
# open with nothing loaded, so only its end-file can ack the go.
def brokenlast(rounds):
    broken(rounds, last=True)


def broken(rounds, last=False):
    # its own file: runs on two versions share this folder
    fd, bad = tempfile.mkstemp(suffix=".mkv", prefix="tmp_broken", dir=HERE)
    with os.fdopen(fd, "wb") as f:
        f.write(b"not a video" * 100)
    errors = {"ack-late": []}
    shapes = {}
    try:
        for r in range(rounds):
            if last:
                m = M(bad, [*WP, "--idle=yes", os.path.join(HERE, "p0.mkv")])
            else:
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
    report("brokenlast" if last else "broken", rounds, errors, shapes)


# Does the ack's "over" flag every key a go undoes before the node heard it, and only those? As
# probe3's go: the node adopts a state while the person presses space or a seek key -20..+20ms
# around it, or nothing. A key is lost when the go undid it and the node hadn't read it when it
# sent the go (what comes until the ack is nobody's), and the ack doesn't make it the person's:
#   space: mpv ends as sent, and the key's pause (the toggle of where mpv was) isn't the go's
#   a seek key, the go seeking: mpv ends on the go's second
#   a seek key, the go not seeking, mpv playing: its seek is said in flight
#   (stopped, the ack's second off where mpv was is the person's seek: caught, not lost)
# Errors: missed, a lost key with no flag; false, a flag with no key pressed; spare, a flag on a
# key the node heard or caught anyway (harmless if the node then shows nothing).
def over(rounds):
    conf = os.path.join(HERE, "input.conf")
    with open(conf, "w") as f:
        f.write("F1 seek 7 absolute\n")
    m = M(os.path.join(HERE, "v120.mkv"), [*WP, f"--input-conf={os.path.relpath(conf)}"])
    ready(m, [])
    p = person_conn(m)
    states = [("yes", "-"), ("no", "-"), ("yes", "S"), ("no", "S")]
    kinds = ["none", "space", "rel", "abskey"]
    errors = {"missed": [], "false": [], "ack-late": []}
    shapes = {}
    for r in range(rounds):
        want, seek = states[r % len(states)]
        kind = kinds[(r // len(states)) % len(kinds)]
        off = OFFSETS_MS[(r // (len(states) * len(kinds))) % len(OFFSETS_MS)]
        stopped(m)
        base = 20 + (r % 30) * 2
        setpos(m, base)
        pre = "yes"
        if r % 2:
            m.wait(reply(m.send("set_property", "pause", False)))
            time.sleep(0.05)
            pre = "no"
        second = base + 40 if seek == "S" else None
        tag = f"o{r}"
        i = m.mark()
        race(m, p, kind if kind != "none" else None, lambda: to(m, "wp-go", want, second if second else "-", "-", tag), off)
        k = wsnap(m, lambda s: s["kind"] == "ack" and s["id"] == tag, i)
        if k is None:
            errors["ack-late"].append((want, seek, kind, off))
            continue
        time.sleep(0.6)
        sent = m.sent
        ack = W(m.lines[k]["args"])
        fp, fpos = get(m, "pause"), get(m, "time-pos")
        msgs = snaps(m, i)
        if kind == "space":
            heard = any(s["kind"] == "pause" and s["pause"] != pre for n, s in msgs if n < sent)
            lost = fp == want and pre == want and not heard
        elif kind in ("rel", "abskey"):
            heard = any(s["kind"] == "seek" and s["id"] == "" for n, s in msgs if n < sent)
            # folded into ours: our seek off our second is the person's too (wentTo)
            heard = heard or second and any(s["kind"] == "seek" and s["id"] == tag and not fnear(s["pos"], second, 0.1) for _, s in msgs)
            if second:
                lost = fnear(fpos, second, 0.15) and want == "yes" and not heard
                if want == "no":
                    # playing on: mpv moved on from the second; lost if no person's seek came after the ack
                    lost = not heard and not any(s["kind"] == "seek" and s["id"] == "" for n, s in msgs if n > k)
            elif pre == "no":
                lost = not heard and any(s["kind"] == "seek" and s["id"] == "" for n, s in msgs if sent <= n < k)
            else:
                lost = False
        else:
            heard = lost = False
        flag = ack["over"]
        if lost and not flag:
            errors["missed"].append((want, seek, kind, off, pre, [(s["kind"], s["id"], s["pause"], s["pos"]) for n, s in msgs if n <= k]))
        if kind == "none" and flag:
            errors["false"].append((want, seek, off, pre))
        if kind != "none" and flag and not lost:
            errors.setdefault("spare", []).append((want, seek, kind, off, pre, "heard" if heard else "-", [(s["kind"], s["id"], s["pause"], s["pos"]) for n, s in msgs if n <= k][-4:]))
        tally(shapes, (kind, "lost" if lost else "heard" if heard else "-", "flagged" if flag else "not flagged"))
    m.stop()
    report("over", rounds, errors, shapes)


# Whose file each start is: the script says "start" with a go's id (ours), "auto" (mpv's own,
# after a file's end or error) or nothing (the person's), and the entry it opened. Raced: next
# and prev keys -20..+20ms around our move, a seek after it, playing into the entry's end, a
# move there and straight back (a second go while the first loads), and a key alone. Errors:
#   wrong: a start labelled with a cause that didn't open that entry
#   last: mpv's final entry isn't the one the last start labelled for its cause
#   ack-late: a go never acked
def starts(rounds):
    files = [os.path.join(HERE, f"p{k}.mkv") for k in range(3)]
    m = M(files[2], [*WP, files[0], files[1]])  # M puts the file last
    ready(m, [])
    p = person_conn(m)
    errors = {"wrong": [], "last": [], "ack-late": [], "missed": [], "false": []}
    shapes = {}

    def on(entry):
        stopped(m)
        if get(m, "playlist-pos") != str(entry):
            i = m.mark()
            m.send("set_property", "playlist-pos", entry)
            m.wait(lambda l: l.get("event") == "playback-restart", i, 5)
            time.sleep(0.15)

    scens = ["none", ">", "<", "seek", "eof", "back", "key"]
    for r in range(rounds):
        scen = scens[r % len(scens)]
        off = OFFSETS_MS[(r // len(scens)) % len(OFFSETS_MS)]
        e0 = r % 3
        on(e0)
        want = ["yes", "no"][(r // 2) % 2]
        target = (e0 + 2) % 3 if scen in (">", "eof") else (e0 + 1) % 3
        dest = {">": e0 + 1 if e0 < 2 else None, "<": e0 - 1 if e0 > 0 else None, "key": e0 + 1 if e0 < 2 else None}.get(scen)
        tag, back = f"s{r}", f"b{r}"
        if scen == "eof":
            setpos(m, float(get(m, "duration")) - 0.3)
            m.wait(reply(m.send("set_property", "pause", False)))
            time.sleep(0.3 + off / 1000)
        i = m.mark()
        send = lambda: to(m, "wp-go", want, "-", target, tag)
        if scen in (">", "<"):
            if off < 0:
                m.send("keypress", scen, s=p); time.sleep(-off / 1000)
            send()
            if off >= 0:
                time.sleep(off / 1000); m.send("keypress", scen, s=p)
        elif scen == "seek":
            send()
            time.sleep(0.05 + max(off, 0) / 1000)
            m.send("keypress", "RIGHT", s=p)
        elif scen == "back":
            send()
            time.sleep(abs(off) / 1000)
            to(m, "wp-go", want, "-", e0, back)
        elif scen == "key":
            m.send("keypress", ">", s=p)
        else:
            send()
        last_id = back if scen == "back" else tag
        if scen == "key":
            time.sleep(1.5)
        elif wsnap(m, lambda s: s["kind"] == "ack" and s["id"] == last_id, i, 8) is None:
            errors["ack-late"].append((scen, off))
            continue
        time.sleep(1.0)
        st = [(s["id"], s["at"]) for _, s in snaps(m, i) if s["kind"] == "start"]
        over = any(s["kind"] == "ack" and s["over"] for _, s in snaps(m, i))
        fe = get(m, "playlist-pos")
        for label, entry in st:
            ok = {tag: str(target), back: str(e0)}.get(label)
            if label in (tag, back):
                good = entry == ok
            elif label == "auto":
                good = scen == "eof" and entry == str(e0 + 1)
            else:
                # the key ran from where the round began, or from where our move took mpv
                step = 1 if scen in (">", "key") else -1
                good = scen in (">", "<", "key") and entry in {str(e + step) for e in (e0, target) if 0 <= e + step <= 2}
            if not good:
                errors["wrong"].append((scen, off, e0, target, dest, st))
        if st:
            label, entry = st[-1]
            if entry != fe:
                errors["last"].append((scen, off, e0, target, dest, st, fe))
        elif scen != "key" or dest is not None:
            if fe != str(e0):
                errors["last"].append((scen, off, e0, target, dest, st, fe))
        # a move's ack comes once its own file has started, when it does; but a go that comes
        # while one waits acks that one at once (back's first)
        idx = {s["id"]: n for n, s in snaps(m, i) if s["kind"] == "start" and s["id"] in (tag, back)}
        for t in ((back,) if scen == "back" else (tag,)):
            k = next((n for n, s in snaps(m, i) if s["kind"] == "ack" and s["id"] == t), None)
            if t in idx and k is not None and k < idx[t]:
                errors.setdefault("early", []).append((scen, off, t == back, st))
        kinds = " ".join("ours" if l in (tag, back) else "auto" if l == "auto" else "person" for l, _ in st)
        # the person's key undone by our move, unread when we sent it: their start then ours, or
        # ours alone though their key had somewhere to go (it ran first and lost to our set)
        sent = m.sent
        heard = any(s["kind"] == "start" and s["id"] == "" for n, s in snaps(m, i) if n < sent)
        # ours alone is the key's loss only if, run after our move, it would have gone somewhere
        after = scen in (">", "<") and 0 <= target + (1 if scen == ">" else -1) <= 2
        undone = scen in (">", "<") and dest is not None and fe == str(target) and (kinds == "person ours" or kinds == "ours" and after)
        if undone and not heard and not over:
            errors["missed"].append((scen, off, e0, target, dest, st))
        if scen in ("none", "seek", "eof", "back") and over:
            errors["false"].append((scen, off, e0, target, st))
        tally(shapes, (scen, kinds, "over" if over else "", "heard" if heard else ""))
    m.stop()
    report("starts", rounds, errors, shapes)


# mpv's own moves: playing into an entry's end with no go, and a go to an entry mpv can't
# open, after which mpv opens the next by itself. Each such start must say "auto".
def autos(rounds):
    fd, bad = tempfile.mkstemp(suffix=".mkv", prefix="tmp_broken", dir=HERE)
    with os.fdopen(fd, "wb") as f:
        f.write(b"not a video" * 100)
    errors = {"wrong": [], "none": []}
    shapes = {}
    try:
        files = [os.path.join(HERE, "p0.mkv"), bad, os.path.join(HERE, "p1.mkv")]
        m = M(files[2], [*WP, files[0], files[1]])  # M puts the file last
        ready(m, [])
        for r in range(rounds):
            scen = ["end", "broken"][r % 2]
            stopped(m)
            if get(m, "playlist-pos") != "0":
                i = m.mark()
                m.send("set_property", "playlist-pos", 0)
                m.wait(lambda l: l.get("event") == "playback-restart", i, 5)
                time.sleep(0.15)
            i, tag = m.mark(), f"a{r}"
            if scen == "end":
                setpos(m, float(get(m, "duration")) - 0.3)
                m.wait(reply(m.send("set_property", "pause", False)))
                want = [("auto", "1"), ("auto", "2")]  # mpv's own past p0, then past the broken one
            else:
                to(m, "wp-go", "yes", "-", 1, tag)
                want = [(tag, "1"), ("auto", "2")]
            time.sleep(2.5)
            st = [(s["id"], s["at"]) for _, s in snaps(m, i) if s["kind"] == "start"]
            if not st:
                errors["none"].append((scen, r))
            elif st != want:
                errors["wrong"].append((scen, st, want))
            tally(shapes, (scen, str(st).replace(tag, "ours")))
        m.stop()
    finally:
        os.remove(bad)
    report("autos", rounds, errors, shapes)


# A go for the file mpv is on (a stop, a seek) racing the person's next key, as the node sends
# it, naming that file (on): if mpv ended on the key's file, the go only stopped it there, never
# seeking, and its ack names that file, which the node sends out as the person's move.
def away(rounds):
    files = [os.path.join(HERE, f"p{k}.mkv") for k in range(3)]
    m = M(files[2], [*WP, files[0], files[1]])  # M puts the file last
    ready(m, [])
    p = person_conn(m)
    errors = {"seeked": [], "ack": [], "ack-late": [], "pos-wrong": []}
    shapes = {}
    for r in range(rounds):
        off = OFFSETS_MS[r % len(OFFSETS_MS)]
        e0 = r % 2
        stopped(m)
        if get(m, "playlist-pos") != str(e0):
            i = m.mark()
            m.send("set_property", "playlist-pos", e0)
            m.wait(lambda l: l.get("event") == "playback-restart", i, 5)
            time.sleep(0.15)
        if r % 4 >= 2:
            m.wait(reply(m.send("set_property", "pause", False)))
            time.sleep(0.05)
        tag = f"w{r}"
        i = m.mark()
        go = lambda: to(m, "wp-go", "yes", 30, "-", tag, str(e0))
        if off < 0:
            m.send("keypress", ">", s=p); time.sleep(-off / 1000)
        go()
        if off >= 0:
            time.sleep(off / 1000); m.send("keypress", ">", s=p)
        k = wsnap(m, lambda s: s["kind"] == "ack" and s["id"] == tag, i, 5)
        if k is None:
            errors["ack-late"].append((off, e0, [(s["kind"], s["id"], s["pause"], s.get("at")) for _, s in snaps(m, i)],
                                       [l.get("event") for l in m.since(i) if l.get("event") not in (None, "client-message", "property-change")]))
            continue
        time.sleep(0.8)
        ack = W(m.lines[k]["args"])
        fe = get(m, "playlist-pos")
        # a seek with no position is the old file's, run as mpv leaves it: the new one opens at
        # 0, and the node reads no second from it
        ours = [s for _, s in snaps(m, i) if s["kind"] == "seek" and s["id"] == tag and s["pos"] != ""]
        moved = ack["at"] == str(e0 + 1)
        fpos = get(m, "time-pos")
        # where the ack says mpv stopped is where it is: the node sends that with the move
        if moved and ack["pos"] != "" and fe == ack["at"] and (fpos is None or abs(float(fpos) - float(ack["pos"])) > 0.1):
            errors["pos-wrong"].append((off, e0, ack["pos"], fpos))
        if moved and ours:
            errors["seeked"].append((off, e0, ack["pos"], fpos, ours[0]["pos"]))
        if fe == str(e0 + 1) and not moved and not any(s["kind"] == "start" and s["id"] == "" for n, s in snaps(m, k + 1)):
            errors["ack"].append((off, e0, ack, fe))
        tally(shapes, ("playing" if r % 4 >= 2 else "stopped", "key's file at the ack" if moved else "ours at the ack", "ends on " + fe,
                       "at 30" if fpos is not None and abs(float(fpos) - 30) < 0.1 else "at 0" if fpos is not None and float(fpos) < 0.1 else "elsewhere"))
    m.stop()
    report("away", rounds, errors, shapes)


class Hang(http.server.BaseHTTPRequestHandler):
    """A stream that hangs: never answers, answers headers only, or sends the file's start."""
    how = "never"

    def do_GET(self):
        if self.how == "never":
            time.sleep(600)
            return
        body = open(os.path.join(HERE, "p1.mkv"), "rb").read()
        self.send_response(200)
        self.send_header("Content-Type", "video/x-matroska")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if self.how == "start":
            self.wfile.write(body[:len(body) // 20])
            self.wfile.flush()
        time.sleep(600)

    def log_message(self, *a):
        pass


# A move to a stream that hangs in each way: how long until the go is acked, and by what.
def hang(rounds):
    errors = {"never-acked": []}
    shapes = {}
    for how in ("never", "headers", "start"):
        handler = type("H", (Hang,), {"how": how})
        srv = socketserver.ThreadingTCPServer(("127.0.0.1", 0), handler)
        srv.daemon_threads = True
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        url = f"http://127.0.0.1:{srv.server_address[1]}/p1.mkv"
        for r in range(rounds):
            # last, under --idle, so mpv stays open once it gives up on the stream
            m = M(url, [*WP, "--idle=yes", os.path.join(HERE, "p0.mkv")])  # M puts the file last
            ready(m, [])
            tag = f"h{how}{r}"
            i, t0 = m.mark(), time.time()
            to(m, "wp-go", "yes", "-", 1, tag)
            k = wsnap(m, lambda s: s["kind"] == "ack" and s["id"] == tag, i, 200)
            took = time.time() - t0
            seen = [l.get("event") + (":" + l["reason"] if l.get("reason") else "") for l in m.since(i)
                    if l.get("event") in ("start-file", "file-loaded", "playback-restart", "end-file")]
            if k is None:
                errors["never-acked"].append((how, seen))
            tally(shapes, (how, "never acked" if k is None else f"acked after {int(took)}s", " ".join(seen)[:70], "entry " + str(get(m, "playlist-pos"))))
            m.stop()
        srv.shutdown()
    report("hang", rounds, errors, shapes)


if __name__ == "__main__":
    os.chdir(HERE)
    exp, n = sys.argv[1], int(sys.argv[2])
    globals()[exp](n)
    # for CI: an error but a harmless one fails the run
    sys.exit(1 if FAILED else 0)
