"""The p2p phase: the direct HTTP mesh."""
from e2e_core import *


def p2p_suite():
    print("\n=== p2p: the direct mesh, and a mesh of its own for every case ===")

    def ip_of(n):
        return nodes[n]["addr"].rsplit(":", 1)[0]

    @case(f"[1] a mesh closes transitively: every node learns all {len(NODES) - 1} others, and starts paused")
    def _():
        with mesh("p1", len(NODES)) as P:
            chain = ", ".join(f"{P[i]}->{P[(i - 1) // 2]}" for i in range(1, len(P)))
            print(f"  bootstrap chain {chain}")
            for n in P:
                missing = set(P) - {n} - known(n)
                ok(f"{n} knows the whole mesh", not missing, f"missing {sorted(missing)}" if missing else "")
                ok(f"{n} advertised its own address", nodes[n]["addr"].endswith(f":{nodes[n]['port']}"), nodes[n]["addr"])
            ok("nobody connected to itself", not any(n in known(n) for n in P))
            ok("everyone is paused", stopped(P), where(P))

    @case("[2a] -h explains the app, one topic at a time, then lists every flag")
    def _():
        h = subprocess.run([BIN, "-h"], capture_output=True, text=True)
        text = h.stdout + h.stderr
        ok("it exits 0", h.returncode == 0, h.returncode)
        for section in ("Start a party", "Join a party", "Watch together", "Automatic pauses", "Links", "Playlists", "Torrents", "Relays", "Flags"):
            ok(f"it has {section}", re.search(rf"^{section}$", text, re.M), text[:200])
        ok("it lists the flags", "-playlist" in text and "-createKey" in text)
        ok("it isn't tied to one kind of video", not COUPLED.search(text), str(COUPLED.findall(text)))

    @case("[2b] the sync-critical mpv options win over the user's own flags")
    def _():
        with mesh("p2b", 1) as P:
            n = P[0]
            ok(f"{n} plays at normal speed", get(n, "speed") == 1, f"speed={get(n, 'speed')}")
            ok(f"{n} seeks precisely", get(n, "hr-seek") in (True, "yes"), f"hr-seek={get(n, 'hr-seek')}")
            ok(f"{n} ignores saved positions", get(n, "resume-playback") is False)
            ok(f"{n} waits for its cache", get(n, "cache-pause") is True)

    @case("[3] a resume reaches everyone over the mesh, and says who")
    def _():
        with mesh("p3") as P:
            a = P[0]
            setp(a, "pause", False)
            ok("everyone plays", until(lambda: playing(P)), where(P))
            for n in P[1:]:
                ok(f"{n} credited {a}", until(lambda: f"resumed by {a}" in osd(n)), str(osd(n)))

    @case("[5] a seek while paused takes everyone there, and says who")
    def _():
        with mesh("p5") as P:
            a, c = P[0], P[-1]
            setp(c, "time-pos", 77.0)
            ok("everyone is at 77s", until(lambda: in_step(P, 77.0)), where(P))
            ok(f"{a} credited {c} for the sync", until(lambda: f"synced by {c}" in osd(a)), str(osd(a)))

    @case("[5d] an arrow-key (relative) seek lands exactly, and goes out once")
    def _():
        with mesh("p5d") as P:
            b = P[1]
            setp(b, "time-pos", 12.0)
            must("everyone is at 12s", until(lambda: in_step(P, 12.0)) and quiet(P), where(P))
            sent = lambda: sum("broadcasted event" in l and '"time-pos"' in l for l in log(b).splitlines())
            before = sent()
            ipc(nodes[b]["sock"], "seek", 7, "relative")
            ok("everyone is at exactly 19s, not a keyframe", until(lambda: in_step(P) and near(get(b, "time-pos"), 19.0, 0.15)), where(P))
            hold(QUIET, "a second broadcast would come right behind the first")
            ok(f"{b} broadcast one position for one seek", sent() - before == 1, f"{sent() - before} sent")

    @case("[5c] an applied event is not echoed back (the a->b->a rebound)")
    def _():
        with mesh("p5c") as P:
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

    @case("[6] positions are not sent while playing")
    def _():
        with mesh("p6") as P:
            a = P[0]
            setp(a, "pause", False)
            must("everyone plays", until(lambda: playing(P)), where(P))
            before = {n: osd(n).count(f"synced by {a}") for n in P[1:]}
            hold(4, "mpv reports time-pos ~10 times a second while playing")
            for n in P[1:]:
                after = osd(n).count(f"synced by {a}")
                ok(f"no time-pos storm reached {n}", after == before[n], f"{after - before[n]} sync notices in 4s")

    @case("[8] repeated round trips are not swallowed by the counter check")
    def _():
        with mesh("p8") as P:
            a = P[0]
            rounds = 0
            for i in range(5):
                setp(a, "pause", False)
                mid = until(lambda: playing(P[1:]), 5)
                setp(a, "pause", True)
                end = until(lambda: stopped(P[1:]), 5)
                rounds += mid and end
            ok(f"5/5 round trips landed on all {len(P) - 1} peers", rounds == 5, f"{rounds}/5")
            for n in P[1:]:
                ok(f"{n} skipped no events", "skipped event from" not in log(n))

    @case("[9] /event and /bye from an unknown address are rejected")
    def _():
        with mesh("p9", 1) as P:
            a = P[0]
            code = raw(nodes[a]["addr"], "/event", {"hit-me-up": f"{ip_of(a)}:9999", "counter": "5", "hostname": "stranger", "secret": SECRET},
                       b'{"event":"property-change","name":"pause","data":"yes"}')
            ok(f"{a} rejects an unknown peer's event", code == 400, f"HTTP {code}")
            code = raw(nodes[a]["addr"], "/bye", {"hit-me-up": f"{ip_of(a)}:9999", "counter": "5", "hostname": "stranger", "secret": SECRET})
            ok(f"{a} rejects an unknown peer's bye", code == 400, f"HTTP {code}")

    @case("[9b] a client that never finishes its headers is dropped, and huge headers are refused")
    def _():
        with mesh("p9b", 1) as P:
            host, port = nodes[P[0]]["addr"].rsplit(":", 1)
            with socket.create_connection((host, int(port)), timeout=SETTLE) as s:
                s.sendall(b"POST /hi HTTP/1.1\r\nHost: x\r\nsecret:")
                try:
                    s.recv(1024)
                    ok("the server hung up on it", True)
                except TimeoutError:
                    ok("the server hung up on it", False, f"still open after {SETTLE}s")
            with socket.create_connection((host, int(port)), timeout=SETTLE) as s:
                s.sendall(b"POST /hi HTTP/1.1\r\nHost: x\r\nsecret: " + b"a" * 20000 + b"\r\n\r\n")
                got = s.recv(64)
                ok("20KB of headers gets a 431", got.startswith(b"HTTP/1.1 431"), str(got))

    @case("[10b] without the secret, a peer's own address forges nothing")
    def _():
        with mesh("p10b") as P:
            a, b, c = P
            pause = b'{"event":"property-change","name":"pause","data":"yes"}'
            for secret in (None, "wrongsecret"):
                h = {"hit-me-up": nodes[b]["addr"], "counter": "999999", "hostname": b, "file": fingerprint(a)}
                if secret:
                    h["secret"] = secret
                for endpoint in ("/hi", "/event", "/bye"):
                    code = raw(nodes[a]["addr"], endpoint, h, pause if endpoint == "/event" else b"")
                    ok(f"{a} refuses {endpoint} with {'no' if secret is None else 'a wrong'} secret", code == 403, f"HTTP {code}")
            code = raw(nodes[a]["addr"], "/event", {"hit-me-up": nodes[b]["addr"], "counter": "999999", "hostname": b,
                                                   "secret": SECRET}, b"x" * ((64 << 10) + 1))
            ok(f"{a} refuses an oversized event", code == 413, f"HTTP {code}")
            setp(c, "pause", False)
            ok(f"{b} is still in the mesh: the forged /bye was refused", until(lambda: get(b, "pause") is False), where(P))

    @case("[11] /bye with a stale counter cannot evict a live peer")
    def _():
        with mesh("p11") as P:
            a, b, c = P
            code = raw(nodes[a]["addr"], "/bye", {"hit-me-up": nodes[b]["addr"], "counter": "1", "hostname": b, "secret": SECRET})
            ok(f"{a} skipped the stale bye", code == 204, f"HTTP {code}")
            ok(f"{a} logged it as skipped", until(lambda: "skipped bye from" in log(a), 5))
            setp(c, "pause", False)
            ok(f"{b} is still in the mesh: it still hears events", until(lambda: get(b, "pause") is False), where(P))

    @case("[11b] a joiner playing a different file is refused; the same file renamed is not; a wrong secret, nobody to join or a taken port ends it")
    def _():
        with mesh("p11b", 2) as P:
            a = P[0]
            same, diff = variants()
            twin, other, bad, alone = "p11b-same", "p11b-diff", "p11b-bad", "p11b-alone"
            launch(twin, nodes[a]["addr"], media=same, port=free_port())
            ok("the renamed, moved, re-dated copy is let in", until(lambda: booted(twin) and all(f"{twin} has joined" in osd(n) for n in P), BOOT), str([osd(n) for n in P]))
            resolve(twin)
            ok("it got the same fingerprint", fingerprint(twin) == fingerprint(a))
            launch(other, nodes[a]["addr"], media=diff, port=free_port())
            until(lambda: nodes[other]["proc"].poll() is not None, BOOT)
            nodes[other]["sock"] = child_mpv_socket(nodes[other]["proc"].pid)  # only if it wrongly got in
            ok("the different file's node exited with an error", nodes[other]["proc"].poll() not in (None, 0),
               f"rc={nodes[other]['proc'].returncode}")
            ok("it was told why, and what each plays", "not the same file as the party's" in log(other) and "the party's: " in log(other) and "yours: " in log(other), log(other)[-400:])
            ok(f"{a} logged the refusal", f"refused {other}" in log(a))
            ok("nobody saw it join", not any(other in known(n) for n in P))
            launch(bad, nodes[a]["addr"], secret="wrongsecret", port=free_port())
            until(lambda: nodes[bad]["proc"].poll() is not None, BOOT)
            ok("a joiner with the wrong secret exited with an error", nodes[bad]["proc"].poll() not in (None, 0),
               f"rc={nodes[bad]['proc'].returncode}")
            ok("it was told why", "the party refused your secret" in log(bad))
            ok("nobody saw it join", not any(bad in known(n) for n in P))
            launch(alone, f"{ip_of(a)}:{free_port()}", port=free_port())
            until(lambda: nodes[alone]["proc"].poll() is not None, BOOT)
            nodes[alone]["sock"] = child_mpv_socket(nodes[alone]["proc"].pid)  # only if it wrongly started
            ok("a joiner nobody answered exited with an error instead of playing alone", nodes[alone]["proc"].poll() not in (None, 0) and "init done" not in log(alone),
               f"rc={nodes[alone]['proc'].returncode}")
            ok("it was told why", "nobody answered at the address you were given" in log(alone), log(alone)[-300:])
            taken = "p11b-taken"
            launch(taken, nodes[a]["addr"], port=nodes[a]["port"])
            until(lambda: nodes[taken]["proc"].poll() is not None, BOOT)
            ok("a joiner on a port already in use exited with an error", nodes[taken]["proc"].poll() not in (None, 0),
               f"rc={nodes[taken]['proc'].returncode}")
            ok("it was told the port, not that nobody answered", f"can't take port {nodes[a]['port']}" in log(taken) and "nobody answered" not in log(taken),
               log(taken)[-300:])

    @case("[11c] a joiner while the party plays pauses it once loaded, and starts where it stopped")
    def _():
        with mesh("p11c") as P:
            a = P[0]
            setp(a, "time-pos", 30.0)  # so "where it stopped" can't be the start
            must("everyone is at 30s", until(lambda: in_step(P, 30.0)), where(P))
            setp(a, "pause", False)
            must("everyone plays", until(lambda: playing(P)), where(P))
            mid = "p11c-mid"
            launch(mid, nodes[a]["addr"], port=free_port())
            must("the joiner is up", until(lambda: booted(mid), BOOT), log(mid)[-300:])
            resolve(mid)
            for n in P:
                ok(f"{n} told why", until(lambda: f"paused for {mid}: joining" in osd(n)), str(osd(n)))
            ok("everyone stopped in step with it, past 30s", until(lambda: in_step(P + [mid], tol=0.3)) and (get(a, "time-pos") or 0) > 30, where(P + [mid]))

    @case("[12] SIGKILL a member: every survivor evicts it after 3 strikes and stays in step")
    def _():
        with mesh("p12", 4) as P:
            a, last = P[0], P[-1]
            dead = nodes[last]["addr"]
            stop(last, signal.SIGKILL)
            survivors = P[:-1]
            t0 = time.time()
            for n in survivors:
                # only the node that generates an event broadcasts, so each survivor has
                # to drive a few of its own before it can notice the dead one is gone
                tries = 0
                while strikes(n, dead, last) < 3 and tries < 12:
                    toggle(n)
                    tries += 1
            elapsed = time.time() - t0
            for n in survivors:
                ok(f"{n} evicted {last}", until(lambda: dropped(n, dead)), f"{strikes(n, dead, last)} strikes")
            ok("no survivor stalled on the dead peer", elapsed < 90, f"{elapsed:.1f}s for {len(survivors)} nodes")
            must("the toggles are over", quiet(survivors))
            setp(a, "pause", not get(a, "pause"))
            want = get(a, "pause")
            ok(f"{a} still drives everyone after the eviction", until(lambda: all(get(n, "pause") is want for n in survivors)), where(survivors))

    @case("[13] one success resets the strike count (a flaky peer must not be evicted)")
    def _():
        with mesh("p13", 1) as P:
            a = P[0]
            at = f"{ip_of(a)}:{free_port()}"
            strikes_now = lambda: strikes(a, at, STUB_HOST)

            def toggle_until(cond, most=8):
                """a broadcast is the only way a strike happens; bounded, or a peer evicted too
                early would stop the strikes and hang the case"""
                for _ in range(most):
                    if cond():
                        return True
                    toggle(a)
                return cond()
            s1 = stub(nodes[a]["addr"], a, at)
            ok("the stub peer joined the mesh", until(lambda: f"{STUB_HOST} has joined" in osd(a), 5))
            stub_stop(s1)
            base = strikes_now()
            ok("2 strikes: still in the mesh", toggle_until(lambda: strikes_now() >= base + 2) and not dropped(a, at), f"{strikes_now() - base} failures logged")
            # back at the same address WITHOUT re-registering, so a keeps the same peer entry
            # (fails=2) rather than replacing it through /hi: only a success can clear the strikes
            s2 = stub(nodes[a]["addr"], a, at, register=False)
            toggle(a)
            ok("no new strike while the peer is healthy", strikes_now() == base + 2, "the broadcast succeeded")
            stub_stop(s2)
            base2 = strikes_now()
            ok("2 more strikes after the success: STILL in the mesh", toggle_until(lambda: strikes_now() >= base2 + 2) and not dropped(a, at),
               f"{strikes_now()} total failures vs maxFails=3, only a reset explains this")
            ok("a 3rd strike in a row evicts", toggle_until(lambda: strikes_now() >= base2 + 3) and until(lambda: dropped(a, at), 5))

    @case("[13c] a peer that missed a pause and a resume is sent the state again once it is back; a newer resume reaching a node that plays stops the party")
    def _():
        with mesh("p13c", 1) as P:
            a = P[0]
            at = f"{ip_of(a)}:{free_port()}"
            heard = f"{RUN}/p13c-stub-first.log"
            s4 = stub(nodes[a]["addr"], a, at, events=heard)
            must("the stub peer joined", until(lambda: f"{STUB_HOST} has joined" in osd(a), 5))
            setp(a, "pause", False)
            # gone before the resume reached it, the stub would cost a strike of its own
            must(f"{a} plays, and the stub heard it", until(lambda: get(a, "pause") is False and os.path.exists(heard)
                 and any(json.loads(l).get("paused") is False for l in open(heard).read().splitlines())))
            stub_stop(s4)
            base = strikes(a, at, STUB_HOST)
            toggle(a)
            toggle(a)
            ok("both were missed", strikes(a, at, STUB_HOST) == base + 2 and get(a, "pause") is False,
               f"{strikes(a, at, STUB_HOST) - base} failures logged")
            # back before the chase's next try, 2s after the last send
            events = f"{RUN}/p13c-stub-events.log"
            s5 = stub(nodes[a]["addr"], a, at, register=False, events=events)
            got = lambda: [json.loads(l) for l in open(events).read().splitlines()] if os.path.exists(events) else []
            ok("it was sent the state again, playing", until(lambda: any(e.get("again") and e.get("paused") is False for e in got()), 10),
               f"{got()}")
            ok(f"{a} stopped nobody for it: the state lands where it is behind", get(a, "pause") is False and
               not any("out of sync" in t for t in osd(a)), osd(a)[-3:])
            # the stub plays out of step now, and resumes: newer than anything a holds
            resume = {"stamp": 1 << 20, "paused": False, "at": max(num(a, "time-pos") - 10, 0), "event": "property-change",
                      "name": "pause", "data": "no"}
            code = raw(nodes[a]["addr"], "/event", {"hit-me-up": at, "counter": "999999", "hostname": STUB_HOST, "secret": SECRET},
                       json.dumps(resume).encode())
            ok(f"a newer resume reaching {a} while it plays stops the party",
               code == 204 and until(lambda: f"paused: out of sync with {STUB_HOST}" in osd(a) and get(a, "pause") is True, 5),
               f"HTTP {code}, {osd(a)[-3:]}")
            stub_stop(s5)
            drops = lambda: sum(s.startswith("dropped " + at) for s in osd(a))
            before, tries = drops(), 0
            while drops() == before and tries < 8:
                toggle(a)
                tries += 1
            ok("and is dropped once gone", drops() > before)

    @case("[13b] with the default -evictAfter, a burst of strikes does not evict a peer")
    def _():
        with mesh("p13b", 1, evict=None) as P:
            a = P[0]
            at = f"{ip_of(a)}:{free_port()}"
            s3 = stub(nodes[a]["addr"], a, at)
            must("the stub peer joined", until(lambda: f"{STUB_HOST} has joined" in osd(a), 5))
            stub_stop(s3)
            tries = 0  # an evicted peer is never sent to again, so its strikes stop at 3
            while strikes(a, at, STUB_HOST) < 4 and tries < 8:
                toggle(a)
                tries += 1
            ok("4 strikes within seconds: still in the mesh",
               strikes(a, at, STUB_HOST) >= 4 and not dropped(a, at),
               f"{strikes(a, at, STUB_HOST)} failures logged, evictAfter is 5m")

    @case("[13d] a member whose process stalls past the send timeout misses a resume; once it runs, the party ends in step")
    def _():
        with mesh("p13d") as P:
            X, Y = P[-1], P[1]
            base = strikes(Y, nodes[X]["addr"], X)
            os.kill(nodes[X]["proc"].pid, signal.SIGSTOP)
            try:
                setp(Y, "pause", False)
                stalled = until(lambda: strikes(Y, nodes[X]["addr"], X) > base, 15)
            finally:
                os.kill(nodes[X]["proc"].pid, signal.SIGCONT)
            ok(f"{Y}'s resume timed out on {X}", stalled, log(Y)[-300:])
            quiet(P, 30)
            end = {n: (get(n, "pause"), num(n, "time-pos")) for n in P}
            spread = max(t for _, t in end.values()) - min(t for _, t in end.values())
            states = {p for p, _ in end.values()}
            ok("the party ends in step", len(states) == 1 and spread < (0.5 if True in states else 1.5),
               f"{end}; {X} saw {osd(X)[-3:]}, {Y} saw {osd(Y)[-3:]}")

    @case("[14] clean shutdown: SIGTERM tells every survivor with /bye, which pauses the party")
    def _():
        with mesh("p14") as P:
            a, survivors = P[0], P[1:]
            setp(a, "pause", False)
            must("the party is playing", until(lambda: playing(P)), where(P))
            ok(f"{a} exited cleanly", stop(a) and nodes[a]["proc"].returncode == 0, f"rc={nodes[a]['proc'].returncode}")
            for n in survivors:
                ok(f"{n} was told {a} left", until(lambda: f"{a} has left" in osd(n)),
                   "Shutdown must not derive its timeout from the cancelled signal context")
                ok(f"{n} did not error on {a}'s departure", "does not exists" not in log(n))
            ok("everyone left stopped", until(lambda: stopped(survivors)), where(survivors))
            ok("someone says why", any(f"paused for {a}: left" in osd(n) for n in survivors), "another's pause can reach a node before its notice, so not everyone does")

            part("[15] the rest of the mesh keeps working without the node that bootstrapped it")
            must("the leave is over", quiet(survivors))
            setp(survivors[0], "pause", False)
            ok("a resume still reaches everyone", until(lambda: playing(survivors)), where(survivors))
            for n in survivors:
                ok(f"{n} exited cleanly", stop(n) and nodes[n]["proc"].returncode == 0)

    @case("[16] a file's folder plays as a playlist over the mesh too")
    def _():
        folder = videos_in(f"{RUN}/p2pfolder", ("e1.mkv", "e2.mkv", "e3.mkv"))
        with mesh("p16", 2, media=f"{folder}/e1.mkv", playlist=True) as P:
            host, guest = P
            ok("whoever started the party answered the joiner", "paused for p16-b: joining" in osd(host), osd(host))
            ipc(nodes[host]["sock"], "playlist-next")
            ok("the other node follows", until(lambda: get(guest, "filename") == "e2.mkv"), get(guest, "filename"))
            ok("the invite asks for the playlist", re.search(r"share is -addrs \S+ -secret \S+ -playlist\n", log(host)), log(host)[:300])

    run_cases()
