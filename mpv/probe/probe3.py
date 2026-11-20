"""Probe the one-message design (wp3.lua): the node sends the whole state it adopts at once.
Usage: python3 -I probe3.py <go|moves|buffering> <rounds>. Under load:
python3 run.py 0 probe3.py go:420 moves:280 buffering:40"""
import os, socketserver, sys, threading, time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.append(HERE)  # -I leaves this folder off the path; last, so the standard library wins
from probe import OFFSETS_MS, race, ready, reply
from probe2 import Slow, Wp2, fnear, get, person_conn, report, setpos, stopped, tally


class Wp3(Wp2):
    """probe3's experiments, against wp3.lua, with probe2's others; swaps as Wp2 says."""

    NAME = "wp3"
    WP = ["--script=wp3.lua"]

    # The node adopts a state (stopped or playing, with or without a seek) while the person presses
    # space or seeks around it, or does nothing. After the ack, the node holds the state it sent;
    # any later snapshot that differs is the person's change. Errors:
    #   silent: mpv ends away from what the node sent, and no snapshot after the ack says so
    #   false: the person did nothing, yet a snapshot after the ack differs from what was sent
    #   ack-late: no ack within 3s
    def go(self, rounds):
        conf = os.path.join(HERE, "input.conf")
        with open(conf, "w") as f:
            f.write("F1 seek 7 absolute\n")
        m = self.M(os.path.join(HERE, "v120.mkv"), [*self.WP, f"--input-conf={os.path.relpath(conf)}"])
        ready(m, [])
        p = person_conn(m)
        states = [("yes", "-"), ("no", "-"), ("yes", "S"), ("no", "S")]
        kinds = ["none", "space", "rel", "abskey"]
        errors = {"silent": [], "false": [], "ack-late": []}
        shapes, overwritten = {}, {}
        for r in range(rounds):
            want_pause, seek = states[r % len(states)]
            kind = kinds[(r // len(states)) % len(kinds)]
            off = OFFSETS_MS[(r // (len(states) * len(kinds))) % len(OFFSETS_MS)]
            stopped(m)
            base = 20 + (r % 30) * 2
            setpos(m, base)
            if r % 2:
                m.wait(reply(m.send("set_property", "pause", False)))
                time.sleep(0.05)
            second = base + 40 if seek == "S" else None
            tag = f"g{r}"
            i = m.mark()
            race(m, p, kind if kind != "none" else None, lambda: self.to(m, "wp-go", want_pause, second if second else "-", "-", tag), off)
            k = self.wsnap(m, lambda s: s["kind"] == "ack" and s["id"] == tag, i)
            if k is None:
                errors["ack-late"].append((want_pause, seek, kind, off))
                continue
            time.sleep(0.6)
            after = [s for _, s in self.snaps(m, k + 1)]
            fp, fpos = get(m, "pause"), get(m, "time-pos")
            # where the node holds itself: the second it sent, or where mpv was at the ack
            ack = self.W(m.lines[k]["args"])
            held_pos = second if second else float(ack["pos"])
            def differs(s):
                if s["pause"] != want_pause:
                    return True
                return want_pause == "yes" and not fnear(s["pos"], held_pos, 0.15)
            final = {"pause": fp, "pos": fpos}
            away = differs(final) if want_pause == "yes" else fp != want_pause
            seen = any(differs(s) for s in after) or differs(ack)
            if away and not seen:
                errors["silent"].append((want_pause, seek, kind, off, ack, after, final))
            if kind == "none" and any(differs(s) for s in after):
                errors["false"].append((want_pause, seek, off, ack, after))
            tally(shapes, (want_pause, seek, kind, "away" if away else "as sent", " ".join(s["kind"] for s in after)))
            if kind != "none" and not away:
                # the key acted before the handler, which overwrote it: did the node hear it first?
                pre = "no" if r % 2 else "yes"
                before = [s for n, s in self.snaps(m, i) if n < k]
                heard = any(s["kind"] == "pause" and s["pause"] != pre for s in before) or any(s["kind"] == "seek" and s["id"] == "" for s in before)
                tally(overwritten, (kind, "heard before the ack" if heard else "never heard"))
        m.stop()
        report("go", rounds, errors, shapes)
        print("  overwritten keys:")
        for key, v in sorted(overwritten.items()):
            print(f"  {v:4d}  {key}")

    # Moves: the node adopts a state on another entry (stopped or playing, with or without a seek)
    # while the person presses next or prev, seeks right after, or mpv plays into the entry's end
    # and moves on by itself. Errors as for go, with the entry part of the state.
    def moves(self, rounds):
        files = [os.path.join(HERE, f"p{k}.mkv") for k in range(3)]
        m = self.M(files[2], [*self.WP, files[0], files[1]])  # M puts the file last
        ready(m, [])
        p = person_conn(m)
        errors = {"silent": [], "false": [], "ack-late": []}
        shapes = {}

        def on(entry):
            stopped(m)
            if get(m, "playlist-pos") != str(entry):
                i = m.mark()
                m.send("set_property", "playlist-pos", entry)
                m.wait(lambda l: l.get("event") == "playback-restart", i, 5)
                time.sleep(0.15)

        scens = ["none", ">", "<", "seek", "eof"]
        for r in range(rounds):
            scen = scens[r % len(scens)]
            off = OFFSETS_MS[(r // len(scens)) % len(OFFSETS_MS)]
            e0 = r % 3
            on(e0)
            want_pause = ["yes", "no"][(r // 2) % 2]
            second = [None, 3.5][(r // 4) % 2]
            # never where the key or the file's end goes
            target = (e0 + 2) % 3 if scen in (">", "eof") else (e0 + 1) % 3
            tag = f"m{r}"
            if scen == "eof":
                setpos(m, float(get(m, "duration")) - 0.3)
                m.wait(reply(m.send("set_property", "pause", False)))
                time.sleep(0.3 + off / 1000)
            i = m.mark()
            send = lambda: self.to(m, "wp-go", want_pause, second if second else "-", target, tag)
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
            else:
                send()
            k = self.wsnap(m, lambda s: s["kind"] in ("ack", "refused") and s["id"] == tag, i, 5)
            if k is None:
                errors["ack-late"].append((scen, off, [(s["kind"], s["id"], s["entry"]) for _, s in self.snaps(m, i)]))
                continue
            if self.W(m.lines[k]["args"])["kind"] == "refused":
                # a key moved mpv as the go's seek began: no file to seek in; the node weighs the
                # refusal's entry as an ack's
                tally(shapes, (scen, "refused", "on " + self.W(m.lines[k]["args"]).get("at", "")))
                continue
            time.sleep(0.8)
            ack = self.W(m.lines[k]["args"])
            after = [s for _, s in self.snaps(m, k + 1)]
            fp, fpos, fe = get(m, "pause"), get(m, "time-pos"), get(m, "playlist-pos")
            held_pos = second if second else float(ack["pos"] or 0)

            def differs(s):
                if s["entry"] != str(target) or s["pause"] != want_pause:
                    return True
                return want_pause == "yes" and not fnear(s["pos"], held_pos, 0.15)
            final = {"pause": fp, "pos": fpos, "entry": fe}
            away = differs(final) if want_pause == "yes" else (fp != want_pause or fe != str(target))
            seen = differs(ack) or any(differs(s) for s in after)
            if away and not seen:
                errors["silent"].append((scen, off, want_pause, second, ack, after, final))
            if scen == "none" and any(differs(s) for s in after):
                errors["false"].append((off, want_pause, second, ack, after))
            tally(shapes, (scen, want_pause, "seek" if second else "-", "away" if away else "as sent", " ".join(s["kind"] for s in after if s["kind"] != "cache")[:60]))
        m.stop()
        report("moves", rounds, errors, shapes)

    # Buffering: a throttled stream runs the cache dry while playing; the node adopts a stop or a
    # resume while stalled, against the person's space or nothing.
    def buffering(self, rounds):
        srv = socketserver.ThreadingTCPServer(("127.0.0.1", 0), lambda *a, **k: Slow(*a, directory=HERE, **k))
        srv.daemon_threads = True
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        url = f"http://127.0.0.1:{srv.server_address[1]}/net.mkv"
        errors = {"no-stall": [], "silent": [], "false": [], "ack-late": []}
        shapes = {}
        for r in range(rounds):
            m = self.M(url, [*self.WP, "--cache=yes", "--demuxer-readahead-secs=1", "--cache-secs=1"])
            p = person_conn(m)
            m.wait(lambda l: l.get("event") == "playback-restart", 0, 10)
            i = m.mark()
            m.send("set_property", "pause", False)
            if self.wsnap(m, lambda s: s["kind"] == "cache" and s["cache"] == "yes", i, 15) is None:
                errors["no-stall"].append(r)
                m.stop()
                continue
            want = ["yes", "no"][r % 2]
            key = (r // 2) % 2 == 1
            off = OFFSETS_MS[(r // 4) % len(OFFSETS_MS)]
            if want == "no":
                m.send("set_property", "pause", True)
                time.sleep(0.2)
            tag = f"b{r}"
            i = m.mark()
            race(m, p, "space" if key else None, lambda: self.to(m, "wp-go", want, "-", "-", tag), off)
            k = self.wsnap(m, lambda s: s["kind"] == "ack" and s["id"] == tag, i, 5)
            if k is None:
                errors["ack-late"].append((want, key, off))
                m.stop()
                continue
            time.sleep(2.0)
            ack = self.W(m.lines[k]["args"])
            after = [s for _, s in self.snaps(m, k + 1)]
            fp = get(m, "pause")
            differs = lambda s: s["pause"] != want
            if fp != want and not (differs(ack) or any(differs(s) for s in after)):
                errors["silent"].append((want, key, off, ack, after, fp))
            if not key and any(differs(s) for s in after):
                errors["false"].append((want, off, ack, [(s["kind"], s["pause"], s["cache"]) for s in after]))
            tally(shapes, (want, "space" if key else "-", "away" if fp != want else "as sent", " ".join(f"{s['kind']}:{s['pause']}/{s['cache']}" for s in after)[:90]))
            m.stop()
        srv.shutdown()
        report("buffering", rounds, errors, shapes)


if __name__ == "__main__":
    os.chdir(HERE)
    exp, n = sys.argv[1], int(sys.argv[2])
    getattr(Wp3(), exp)(n)
