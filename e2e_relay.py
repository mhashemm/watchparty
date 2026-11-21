"""The relay phase: nodes through one ./relay websocket server."""
from e2e_core import *


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
            hold(4, "mpv reports time-pos ~10 times a second while playing")
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
            for i in range(10):  # without the fix about one round in six does it: 10 rounds miss it one run in six
                setp(a, "time-pos", 5.0 + i * 2)
                setp(a, "pause", False)
                must(f"round {i + 1} plays", until(lambda: playing(P)), where(P))
                hold(0.15, "long enough to play a few frames")
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
            hold(QUIET, "an echo would be right behind the pause")
            ok("everyone stopped", stopped(P), where(P))
            # if any peer rebroadcast the echo, the originator would be told it was paused
            # by someone else and the rest would see the notice twice
            ok(f"{a} was not told its own pause came back", pauses(a) == before[a],
               f"{pauses(a) - before[a]} echoes returned to the originator")
            for n in P[1:]:
                got = pauses(n) - before[n]
                ok(f"{n} applied {a}'s pause exactly once", got == 1, f"{got} pause notices")

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
            hold(QUIET, "a rewind would go out as soon as its file loaded")
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
                hold(QUIET, "the other room's seek would arrive at once")
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
            ok("everyone stopped in step with it, past 30s", until(lambda: in_step(P + [mid], tol=0.3)) and (get(P[0], "time-pos") or 0) > 30, where(P + [mid]))

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
                ok("everyone stopped in step with it, past 30s", until(lambda: in_step(P + [far], tol=0.3)) and (get(P[0], "time-pos") or 0) > 30, where(P + [far]))
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
            ok("and what each plays", "the party's: " in log(other) and "yours: " in log(other), log(other)[-400:])
            ok("it did not retry", log(other).count("relay: dial") == 0, "409 is permanent")
            ok("nobody in the room saw it join", not any(other in known(n) for n in P))

    class Quiet(http.server.SimpleHTTPRequestHandler):
        # by User-Agent, so a case slows only its own client while others run beside it
        held = {}  # seconds each request waits
        rate = {}  # bytes/s

        def log_message(self, format, *args):
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
                hold(QUIET, "a late echo would go out right behind the last seek")
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
            ok("and everyone stopped in step with it, past 30s", until(lambda: in_step(P + [late], tol=0.3)) and (get(P[0], "time-pos") or 0) > 30, where(P + [late]))


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
        ok("its log says how full its cache is, then that it's done", until(lambda: re.search(r"^.{20}buffering: \d+%$(?s:.*)^.{20}buffering: done$", log("nodeSlow"), re.M), 30), log("nodeSlow")[-600:])
        ok("while the party waits, it says how many seconds it has ready", until(lambda: re.search(r"^.{20}buffered ahead: \d+$", log("nodeSlow"), re.M), 30), log("nodeSlow")[-400:])
        ok("and the others hear it", until(lambda: re.search(r'^.{20}buffered ahead: \d+ for \{"id":"\w+","name":"nodeSlow"\}$', log("nodeFast"), re.M), 10), log("nodeFast")[-400:])
        ok("and show it on their mpv", any(re.fullmatch(r"nodeSlow's network is slow: \d+s ready", t) for t in osd("nodeFast")), str(osd("nodeFast")[-4:]))
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
            hold(QUIET, "what it says back, a second join or a false leave would follow the swap at once")
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
            hold(QUIET, "a stop or a notice would follow the redial at once")
            ok("nobody stopped, and nobody was told anything", playing(everyone)
               and all(osd(n)[said[n]:] == [] for n in P), str([osd(n)[said[n]:] for n in everyone]))
            away, joins = flaky_lines("away"), flaky_lines("joined")
            ok("the relay's log says it was away, and links its return", len(away) == 1 and len(joins) == 3 and joins[2]["replaces"] == away[0]["conn"],
               f"{away} {joins[2:]}")

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
            hold(3, "the relay stays down a while: what a node does without it shows")
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
