"""Probe what mpv's core guarantees over JSON IPC (mpv/ORDER.md holds the results).
Usage: python3 -I probe.py <experiment> <rounds>, after ./videos.sh; run.py runs them under load.
Experiments: echo, r6c, fence, pause, check, repause, lua, luaseek."""
import json, os, socket, subprocess, sys, tempfile, threading, time

HERE = os.path.dirname(os.path.abspath(__file__))
MPV = os.environ.get("MPV", "mpv")
WINDOWS = sys.platform == "win32"


def pin(p, core):
    """Keeps process p on one core, on Windows; Linux starts it under taskset, and macOS has no
    way to pin (run.py loads every core there instead)."""
    if sys.platform == "win32":
        import ctypes
        if not ctypes.windll.kernel32.SetProcessAffinityMask(ctypes.c_void_p(int(p._handle)), ctypes.c_size_t(1 << int(core))):
            raise OSError(ctypes.get_last_error(), "SetProcessAffinityMask")


# sys.platform, not WINDOWS: pyright reads the platform only from the check itself
if sys.platform == "win32":
    class Pipe:
        """mpv's IPC on Windows, a named pipe, as the probes use a socket (recv, sendall).
        Overlapped: on a plain handle a read blocked in one thread holds up every write from another."""

        def __init__(self, name):
            import _winapi
            self.w = _winapi
            for _ in range(100):
                try:
                    self.h = _winapi.CreateFile(name, _winapi.GENERIC_READ | _winapi.GENERIC_WRITE, 0, _winapi.NULL,
                                                _winapi.OPEN_EXISTING, _winapi.FILE_FLAG_OVERLAPPED, _winapi.NULL)
                    return
                except OSError as e:
                    # every instance taken: mpv makes the next one once it has a client on this one
                    if e.winerror != _winapi.ERROR_PIPE_BUSY:
                        raise
                    time.sleep(0.01)
            raise OSError(f"{name}: busy")

        def recv(self, n):
            try:
                ov, _ = self.w.ReadFile(self.h, n, overlapped=True)
                ov.GetOverlappedResult(True)
            except (BrokenPipeError, OSError):
                return b""
            return ov.getbuffer()

        def sendall(self, b):
            ov, _ = self.w.WriteFile(self.h, b, overlapped=True)
            ov.GetOverlappedResult(True)


class Mpv:
    def __init__(self, video, extra=()):
        self.dir = tempfile.mkdtemp(dir=HERE)
        # Windows: a named pipe, mpv's IPC there
        self.sock = rf"\\.\pipe\{os.path.basename(self.dir)}" if WINDOWS else os.path.relpath(os.path.join(self.dir, "s"))
        load = os.environ.get("LOAD")
        self.p = subprocess.Popen(
            [*(["taskset", "-c", load] if load and sys.platform == "linux" else []), MPV, "--no-config", "--vo=null", "--ao=null", "--pause", "--speed=1", "--hr-seek=yes",
             "--cache-pause=yes", "--no-resume-playback", "--quiet", "--keep-open=yes",
             f"--input-ipc-server={self.sock}", *extra, video],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if load:
            pin(self.p, load)
        for _ in range(200):
            try:
                self.s = self.conn()
                break
            except OSError:
                time.sleep(0.02)
        self.lines, self.cv = [], threading.Condition()
        threading.Thread(target=self.read, args=(self.s,), daemon=True).start()
        self.rid = 0

    def conn(self):
        if sys.platform == "win32":
            return Pipe(self.sock)
        s = socket.socket(socket.AF_UNIX)
        s.connect(self.sock)
        return s

    def read(self, s):
        buf = b""
        while True:
            d = s.recv(65536)
            if not d:
                return
            buf += d
            while b"\n" in buf:
                l, buf = buf.split(b"\n", 1)
                with self.cv:
                    self.lines.append(json.loads(l))
                    self.cv.notify_all()

    def send(self, *cmd, asyn=False, s=None):
        self.rid += 1
        m = {"command": list(cmd), "request_id": self.rid}
        if asyn:
            m["async"] = True
        (s or self.s).sendall(json.dumps(m).encode() + b"\n")
        return self.rid

    def mark(self):
        with self.cv:
            return len(self.lines)

    def wait(self, pred, start=0, timeout=3):
        end = time.time() + timeout
        with self.cv:
            while True:
                for i in range(start, len(self.lines)):
                    if pred(self.lines[i]):
                        return i
                left = end - time.time()
                if left <= 0:
                    return None
                self.cv.wait(left)

    def need(self, pred, start=0, timeout=3):
        """wait for what must come: a timeout stops the probe, saying what it waited for."""
        i = self.wait(pred, start, timeout)
        if i is None:
            raise TimeoutError(f"no line within {timeout}s after line {start}; the last: {self.since(max(start, len(self.lines) - 3))}")
        return i

    def since(self, i):
        with self.cv:
            return list(self.lines[i:])

    def stop(self):
        self.p.kill()
        self.p.wait()


def reply(rid):
    return lambda l: l.get("request_id") == rid


def prop(name, data=None):
    return lambda l: l.get("event") == "property-change" and l.get("name") == name and (data is None or l.get("data") == data)


def ready(m, observe):
    for i, name in enumerate(observe, 2):
        m.wait(reply(m.send("observe_property_string", i, name)))
    m.wait(lambda l: l.get("event") == "playback-restart", timeout=5)
    time.sleep(0.3)


def own_echo_before_reply(rounds):
    """Claim: on one connection a report caused by our set never comes before the set's reply."""
    m = Mpv(os.path.join(HERE, "v24.mkv"))
    ready(m, ["pause", "time-pos"])
    early = {"pause": 0, "seek": 0}
    missing = 0
    for r in range(rounds):
        # pause toggled while playing, then a seek while paused
        for want in ("no", "yes"):
            i = m.mark()
            rid = m.send("set_property", "pause", want == "yes")
            j = m.need(reply(rid), i)
            k = m.wait(prop("pause", want), i)
            if k is None:
                missing += 1
            elif k < j:
                early["pause"] += 1
            if want == "no":
                time.sleep(0.05)
        t = f"{10 + (r % 50) * 1.5:.6f}"
        i = m.mark()
        rid = m.send("set_property", "time-pos", t)
        j = m.need(reply(rid), i)
        k = m.wait(lambda l: l.get("event") == "seek", i)
        if k is not None and k < j:
            early["seek"] += 1
        m.wait(lambda l: l.get("event") == "playback-restart", i)
    m.stop()
    print(f"own_echo_before_reply: rounds={rounds} early={early} missing_pause_reports={missing}")


def pause_then_position(rounds, order):
    """[R6c]: after our pause of a playing node, does a time-pos come after pause=yes, and does
    the observe order decide it?"""
    m = Mpv(os.path.join(HERE, "v120.mkv"))
    ready(m, order)
    after, total, moved = 0, 0, 0
    for r in range(rounds):
        m.wait(reply(m.send("set_property", "pause", False)))
        time.sleep(0.03 + (r % 7) * 0.01)
        i = m.mark()
        rid = m.send("set_property", "pause", True)
        k = m.need(prop("pause", "yes"), i)
        time.sleep(0.1)
        tail = [l for l in m.since(k + 1) if l.get("event") == "property-change" and l.get("name") == "time-pos"]
        total += 1
        if tail:
            after += 1
            # is the late time-pos where mpv stopped? ask now that it is quiet
            now = m.need(reply(m.send("get_property_string", "time-pos")), i)
            if m.lines[now]["data"] != tail[-1]["data"]:
                moved += 1
    m.stop()
    print(f"pause_then_position order={order}: rounds={total} time-pos after pause=yes in {after}, differing from the stopped position in {moved}")


def async_fence(rounds):
    """With async:true the reply travels in the event queue: is every seek event after it a seek
    that ran after ours was queued? A person's key (keypress from a second connection) races it."""
    m = Mpv(os.path.join(HERE, "v24.mkv"))
    ready(m, ["pause", "time-pos"])
    person = m.conn()
    threading.Thread(target=lambda: [None for _ in iter(lambda: person.recv(65536), b"")], daemon=True).start()
    stats = {}
    for r in range(rounds):
        base = 20 + (r % 40) * 2
        m.wait(reply(m.send("set_property", "time-pos", str(base))))
        m.wait(lambda l: l.get("event") == "playback-restart", m.mark() - 3)
        time.sleep(0.15)
        target = base + 50
        i = m.mark()
        when = r % 3  # 0: person first, 1: ours first, 2: ours only
        if when == 0:
            m.send("keypress", "RIGHT", s=person)  # default binding: seek 5
        rid = m.send("seek", target, "absolute+exact", asyn=True)
        if when == 1:
            m.send("keypress", "RIGHT", s=person)
        j = m.need(reply(rid), i)
        time.sleep(0.4)
        got = m.need(reply(m.send("get_property", "time-pos")), i)
        landed = round(m.lines[got]["data"], 2)
        seq = [l.get("event") or ("reply" if l.get("request_id") == rid else None) for l in m.since(i)]
        seq = [s for s in seq if s in ("seek", "playback-restart", "reply")]
        key = (("person-first", "ours-first", "ours-only")[when], " ".join(seq), "ours" if abs(landed - target) < 0.1 else
               "ours+5" if abs(landed - target - 5) < 0.1 else "person-only" if abs(landed - base - 5) < 0.1 else f"other {landed - base:.2f}")
        stats[key] = stats.get(key, 0) + 1
    m.stop()
    print("async_fence:")
    for k, v in sorted(stats.items()):
        print(f"  {v:4d}  {k}")


OFFSETS_MS = [-20, -5, -1, 0, 1, 5, 20]  # the person's action, relative to ours


def drain(sock):
    threading.Thread(target=lambda: [None for _ in iter(lambda: sock.recv(65536), b"")], daemon=True).start()


def race(m, person, kind, ours, offset):
    """Send ours (a callable returning its request id) and the person's action `offset` ms apart."""
    act = {
        "rel": lambda: m.send("keypress", "RIGHT", s=person),  # built-in: seek 5
        "abskey": lambda: m.send("keypress", "F1", s=person),  # input.conf: seek 7 absolute
        "absipc": lambda: m.send("set_property", "time-pos", "9", s=person),  # a script, the OSC
        "space": lambda: m.send("keypress", "SPACE", s=person),  # built-in: cycle pause
    }.get(kind)
    if act and offset < 0:
        act()
        time.sleep(-offset / 1000)
    rid = ours()
    if act and offset >= 0:
        time.sleep(offset / 1000)
        act()
    return rid


def near(a, b):
    return abs(float(a) - float(b)) < 0.1


def pause_partition(rounds):
    """Our sync pause set racing the person's space key: does every pause report after our reply
    show a state from after our set?"""
    m = Mpv(os.path.join(HERE, "v24.mkv"))
    ready(m, ["pause", "time-pos"])
    person = m.conn()
    drain(person)
    bad, stats = [], {}
    for r in range(rounds):
        m.wait(reply(m.send("set_property", "pause", False)))
        time.sleep(0.06)
        i = m.mark()
        off = OFFSETS_MS[r % len(OFFSETS_MS)]
        rid = race(m, person, "space", lambda: m.send("set_property", "pause", True), off)
        j = m.need(reply(rid), i)
        asked = m.lines[m.need(reply(m.send("get_property_string", "pause")), j)]["data"]
        time.sleep(0.3)
        settled = m.lines[m.need(reply(m.send("get_property_string", "pause")), i)]["data"]
        after = [l["data"] for l in m.since(j + 1) if l.get("event") == "property-change" and l.get("name") == "pause"]
        # a report after the reply showing what held before our set breaks the partition
        ok = ("no" not in after) if settled == "yes" else (after in ([], ["no"], ["yes", "no"]))
        silent = settled == "no" and after == []
        key = (off, settled, " ".join(after) + (" (silent: no report of the person's resume)" if silent else ""), asked)
        stats[key] = stats.get(key, 0) + 1
        if not ok:
            bad.append((off, settled, after))
    m.stop()
    print(f"pause_partition: rounds={rounds} violations={len(bad)} {bad[:5]}")
    for k, v in sorted(stats.items()):
        print(f"  {v:4d}  offset={k[0]:+d}ms settled={k[1]} asked right after reply={k[3]} reports after reply: {k[2] or '-'}")


def seek_check(rounds):
    """The prototype check: our seek goes async; at the first playback-restart after its reply,
    mpv at our target means ours, anywhere else means the person's (folded or later)."""
    conf = os.path.join(HERE, "input.conf")
    with open(conf, "w") as f:
        f.write("F1 seek 7 absolute\n")
    m = Mpv(os.path.join(HERE, "v24.mkv"), [f"--input-conf={os.path.relpath(conf)}"])
    ready(m, ["pause", "time-pos"])
    person = m.conn()
    drain(person)
    kinds = ["none", "here", "rel", "abskey", "absipc", "same"]
    errors = {"leak": [], "lost": [], "wrong": [], "no-restart": []}
    shapes = {}
    for r in range(rounds):
        kind = kinds[r % len(kinds)]
        off = OFFSETS_MS[(r // len(kinds)) % len(OFFSETS_MS)]
        base = 20 + (r % 40) * 2
        i0 = m.mark()
        m.wait(reply(m.send("set_property", "time-pos", str(base))))
        m.wait(lambda l: l.get("event") == "playback-restart", i0)
        time.sleep(0.15)
        target = base if kind == "here" else base + 50
        i = m.mark()
        if kind == "same":
            rid = race(m, person, None, lambda: m.send("seek", target, "absolute+exact", asyn=True), 0)
            m.send("set_property", "time-pos", str(target), s=person)
        else:
            rid = race(m, person, kind, lambda: m.send("seek", target, "absolute+exact", asyn=True), off)
        j = m.need(reply(rid), i)
        sk = m.wait(lambda l: l.get("event") == "seek", j + 1)
        k = None if sk is None else m.wait(lambda l: l.get("event") == "playback-restart", sk + 1)
        if k is None:
            errors["no-restart"].append((kind, off))
            continue
        at = m.lines[m.need(reply(m.send("get_property_string", "time-pos")), k)]["data"]
        sent = [] if near(at, target) else [at]
        time.sleep(0.5)
        settled = m.lines[m.need(reply(m.send("get_property_string", "time-pos")), k)]["data"]
        evs = m.since(i)
        later = [l for l in m.since(k + 1) if l.get("event") == "seek"]
        if later:
            sent.append(settled)
        seq = " ".join("R" if l.get("request_id") == rid else {"seek": "s", "playback-restart": "r"}[l["event"]]
                       for l in evs if l.get("request_id") == rid or l.get("event") in ("seek", "playback-restart"))
        key = (kind, seq, "ours" if near(settled, target) else "elsewhere")
        shapes[key] = shapes.get(key, 0) + 1
        if near(settled, target) and sent:
            errors["leak"].append((kind, off, seq, at, settled))
        if not near(settled, target) and not sent:
            errors["lost"].append((kind, off, seq, at, settled))
        if sent and not near(sent[-1], settled):
            errors["wrong"].append((kind, off, seq, sent, settled))
    m.stop()
    print(f"seek_check: rounds={rounds} " + " ".join(f"{k}={len(v)}" for k, v in errors.items()))
    for k, v in errors.items():
        for e in v[:4]:
            print(f"  {k}: {e}")
    print("  shapes (R = our async reply, s = seek, r = playback-restart):")
    for k, v in sorted(shapes.items()):
        print(f"  {v:4d}  {k[0]:7s} {k[1]:14s} settled {k[2]}")


def pause_reobserve(rounds):
    """Our pause set racing the person's space, then unobserve and observe pause again: no report
    of the old observer after the unobserve's reply, and the fresh report plus any after it end
    on mpv's settled state."""
    m = Mpv(os.path.join(HERE, "v24.mkv"))
    ready(m, ["pause", "time-pos"])
    person = m.conn()
    drain(person)
    errors = {"old-after-unobserve": [], "ends-wrong": [], "no-fresh": []}
    stats = {}
    for r in range(rounds):
        m.wait(reply(m.send("set_property", "pause", False)))
        time.sleep(0.06)
        i = m.mark()
        off = OFFSETS_MS[r % len(OFFSETS_MS)]
        rid = race(m, person, "space", lambda: m.send("set_property", "pause", True), off)
        m.wait(reply(rid), i)
        u = m.need(reply(m.send("unobserve_property", 2)), i)
        o = m.need(reply(m.send("observe_property_string", 2, "pause")), u)
        pauses = lambda a, b=None: [(n, l["data"]) for n, l in enumerate(m.lines[a:b], a) if l.get("event") == "property-change" and l.get("name") == "pause"]
        if pauses(u + 1, o):
            errors["old-after-unobserve"].append((off, pauses(u + 1, o)))
        f = m.wait(prop("pause"), o + 1)
        if f is None:
            errors["no-fresh"].append(off)
            continue
        time.sleep(0.3)
        settled = m.lines[m.need(reply(m.send("get_property_string", "pause")), f)]["data"]
        seen = [d for _, d in pauses(f)]
        verdict = "ours" if seen[0] == "yes" else "person's"
        if seen[-1] != settled:
            errors["ends-wrong"].append((off, seen, settled))
        key = (off, verdict, " ".join(seen), settled)
        stats[key] = stats.get(key, 0) + 1
    m.stop()
    print(f"pause_reobserve: rounds={rounds} " + " ".join(f"{k}={len(v)}" for k, v in errors.items()))
    for k, v in errors.items():
        for e in v[:4]:
            print(f"  {k}: {e}")
    for k, v in sorted(stats.items()):
        print(f"  {v:4d}  offset={k[0]:+d}ms fresh report says {k[1]:8s} reports from it: {k[2]:8s} settled={k[3]}")


def msg(name):
    return lambda l: l.get("event") == "client-message" and l.get("args", [None])[0] == name


def lua_pause(rounds):
    """Our pause set through wp.lua racing the person's space: after the script's ack, at least
    one wp-pause always comes, and the last one is where mpv settled."""
    m = Mpv(os.path.join(HERE, "v24.mkv"), ["--script=wp.lua"])
    ready(m, ["time-pos"])
    m.wait(msg("wp-pause"), timeout=5)
    person = m.conn()
    drain(person)
    errors = {"ends-wrong": [], "no-ack": []}
    stats = {}
    for r in range(rounds):
        m.wait(reply(m.send("set_property", "pause", False)))
        time.sleep(0.06)
        i = m.mark()
        off = OFFSETS_MS[r % len(OFFSETS_MS)]
        tag = str(r)
        race(m, person, "space", lambda: m.send("script-message-to", "wp", "wp-set-pause", "yes", tag), off)
        a = m.wait(lambda l: msg("wp-ack")(l) and l["args"][1] == tag, i)
        if a is None:
            errors["no-ack"].append(off)
            continue
        time.sleep(0.3)
        settled = m.lines[m.need(reply(m.send("get_property_string", "pause")), a)]["data"]
        after = [m.lines[a]["args"][2]] + [l["args"][1] for l in m.since(a + 1) if msg("wp-pause")(l)]
        if after[-1] != settled:
            errors["ends-wrong"].append((off, after, settled))
        key = (off, " ".join(after), settled)
        stats[key] = stats.get(key, 0) + 1
    m.stop()
    print(f"lua_pause: rounds={rounds} " + " ".join(f"{k}={len(v)}" for k, v in errors.items()))
    for k, v in errors.items():
        for e in v[:4]:
            print(f"  {k}: {e}")
    for k, v in sorted(stats.items()):
        print(f"  {v:4d}  offset={k[0]:+d}ms ack, then: {k[1]:10s} settled={k[2]}")


def lua_seek(rounds):
    """Our seek through wp.lua racing the person's seeks. The node's rule: a landing with our id at
    our target is ours; any other landing is the person's, broadcast at its position."""
    conf = os.path.join(HERE, "input.conf")
    with open(conf, "w") as f:
        f.write("F1 seek 7 absolute\n")
    m = Mpv(os.path.join(HERE, "v24.mkv"), ["--script=wp.lua", f"--input-conf={os.path.relpath(conf)}"])
    ready(m, ["time-pos"])
    person = m.conn()
    drain(person)
    kinds = ["none", "here", "rel", "abskey", "absipc", "same"]
    errors = {"leak": [], "lost": [], "wrong": [], "no-id": []}
    shapes = {}
    for r in range(rounds):
        kind = kinds[r % len(kinds)]
        off = OFFSETS_MS[(r // len(kinds)) % len(OFFSETS_MS)]
        base = 20 + (r % 40) * 2
        i0 = m.mark()
        m.wait(reply(m.send("set_property", "time-pos", str(base))))
        m.wait(msg("wp-landed"), i0)
        time.sleep(0.15)
        target = base if kind == "here" else base + 50
        tag = f"s{r}"
        ours = lambda: m.send("script-message-to", "wp", "wp-seek", str(target), tag)
        i = m.mark()
        if kind == "same":
            race(m, person, None, ours, 0)
            m.send("set_property", "time-pos", str(target), s=person)
        else:
            race(m, person, kind, ours, off)
        time.sleep(0.6)
        settled = m.lines[m.need(reply(m.send("get_property_string", "time-pos")), i)]["data"]
        landings = [(l["args"][1], l["args"][2]) for l in m.since(i) if msg("wp-landed")(l)]
        mine = [n for n, (_, id) in enumerate(landings) if id == tag]
        if not mine:
            errors["no-id"].append((kind, off, landings, settled))
            continue
        k = mine[0]
        theirs = [(pos, n >= k) for n, (pos, id) in enumerate(landings) if not (id == tag and near(pos, target))]
        last_pos, last_id = landings[-1]
        shape = " ".join(("O" if id == tag and near(pos, target) else "o" if id == tag else "p") for pos, id in landings)
        key = (kind, shape, "ours" if near(settled, target) else "elsewhere")
        shapes[key] = shapes.get(key, 0) + 1
        if near(settled, target) and kind != "same" and any(after for _, after in theirs):
            errors["leak"].append((kind, off, landings, settled))
        if not near(settled, target) and last_id == tag and near(last_pos, target):
            errors["lost"].append((kind, off, landings, settled))
        if not near(last_pos, settled):
            errors["wrong"].append((kind, off, landings, settled))
    m.stop()
    print(f"lua_seek: rounds={rounds} " + " ".join(f"{k}={len(v)}" for k, v in errors.items()))
    for k, v in errors.items():
        for e in v[:4]:
            print(f"  {k}: {e}")
    print("  landings (O = ours at target, o = ours carried elsewhere, p = the person's):")
    for k, v in sorted(shapes.items()):
        print(f"  {v:4d}  {k[0]:7s} {k[1]:10s} settled {k[2]}")


if __name__ == "__main__":
    os.chdir(HERE)
    exp, n = sys.argv[1], int(sys.argv[2])
    if exp == "echo":
        own_echo_before_reply(n)
    elif exp == "r6c":
        pause_then_position(n, ["pause", "time-pos"])
        pause_then_position(n, ["time-pos", "pause"])
    elif exp == "fence":
        async_fence(n)
    elif exp == "pause":
        pause_partition(n)
    elif exp == "repause":
        pause_reobserve(n)
    elif exp == "lua":
        lua_pause(n)
    elif exp == "luaseek":
        lua_seek(n)
    elif exp == "check":
        seek_check(n)
