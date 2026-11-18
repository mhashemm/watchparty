"""Probe the one-message design (wp3.lua): the node sends the whole state it adopts at once.
Usage: python3 -I probe3.py go <rounds>. Under load: ./run.sh 0 probe3.py go:420"""
import os, sys, time

HERE = os.path.dirname(os.path.abspath(__file__))
exec(open(os.path.join(HERE, "probe2.py")).read().rpartition('if __name__ == "__main__":')[0])

NAME = "wp3"
WP = ["--script=wp3.lua"]


# The node adopts a state (stopped or playing, with or without a seek) while the person presses
# space or seeks around it, or does nothing. After the ack, the node holds the state it sent;
# any later snapshot that differs is the person's change. Errors:
#   silent: mpv ends away from what the node sent, and no snapshot after the ack says so
#   false: the person did nothing, yet a snapshot after the ack differs from what was sent
#   ack-late: no ack within 3s
def go(rounds):
    conf = os.path.join(HERE, "input.conf")
    with open(conf, "w") as f:
        f.write("F1 seek 7 absolute\n")
    m = M(os.path.join(HERE, "v120.mkv"), [*WP, f"--input-conf={os.path.relpath(conf)}"])
    ready(m, [])
    p = person_conn(m)
    states = [("yes", "-"), ("no", "-"), ("yes", "S"), ("no", "S")]
    kinds = ["none", "space", "rel", "abskey"]
    errors = {"silent": [], "false": [], "ack-late": []}
    shapes = {}
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
        race(m, p, kind if kind != "none" else None, lambda: to(m, "wp-go", want_pause, second if second else "-", tag), off)
        k = wsnap(m, lambda s: s["kind"] == "ack" and s["id"] == tag, i)
        if k is None:
            errors["ack-late"].append((want_pause, seek, kind, off))
            continue
        time.sleep(0.6)
        after = [s for _, s in snaps(m, k + 1)]
        fp, fpos = get(m, "pause"), get(m, "time-pos")
        # where the node holds itself: the second it sent, or where mpv was at the ack
        ack = W(m.lines[k]["args"])
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
    m.stop()
    report("go", rounds, errors, shapes)


if __name__ == "__main__":
    os.chdir(HERE)
    exp, n = sys.argv[1], int(sys.argv[2])
    globals()[exp](n)
