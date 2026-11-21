"""The media phase: torrents, odd file names, and a folder as a playlist, through a relay."""
from e2e_core import *


def media_suite():
    print("\n=== media setup: one websocket server ===")
    relay = launch_relay()
    until(lambda: listening(PORTS["relay"]))
    # the playlist [R15c], [R15d] and [R15g] each open in rooms of their own
    folder = videos_in(f"{RUN}/folder", ("video2.mkv", "video3.mkv", "video10.mkv"))

    @case("[R15e] a magnet link streams to everyone, and is the same file to all of them")
    def _():
        magnet = seeder(MEDIA)
        must("the seeder printed a magnet link", magnet.startswith("magnet:?"), magnet)
        launch("torA", room="torroom", media=magnet)
        must("the first node opens it", until(lambda: booted("torA") and connected("torA"), 60), log("torA")[-300:])
        resolve("torA")
        ok("it says what it found", f"torrent: found {os.path.basename(MEDIA)}, 1 to play" in log("torA"))
        launch("torB", room="torroom", media=magnet)
        must("the second node joins", until(lambda: booted("torB") and "torB has joined" in osd("torA"), 60), log("torB")[-300:])
        resolve("torB")
        ok("both have one identity, whatever the local port", fingerprint("torA") == fingerprint("torB"))
        ok("mpv reads it from loopback", str(get("torA", "path")).startswith("http://127.0.0.1:"), get("torA", "path"))
        must("the join is over", quiet(("torA", "torB")))
        setp("torA", "time-pos", 80)
        ok("a seek reaches the other", until(lambda: near(get("torB", "time-pos"), 80, 0.15)), get("torB", "time-pos"))
        ok("and lands on a part nobody had yet", until(lambda: get("torB", "seeking") is False and get("torA", "seeking") is False, 30))
        setp("torA", "pause", False)
        ok("it plays", until(lambda: get("torB", "pause") is False and (get("torB", "time-pos") or 0) > 80.5, 30), get("torB", "time-pos"))
        setp("torB", "pause", True)
        ok("and pauses", until(lambda: get("torA", "pause") is True))
        _, diff = variants()
        other = seeder(diff)
        n = launch("torC", room="torroom", media=other)
        ok("another torrent is refused like another file", until(lambda: nodes[n]["proc"].poll() is not None, 60) and "not the same file" in log(n), log(n)[-200:])
        for n in ("torA", "torB"):
            ok(f"{n} exited cleanly", stop(n) and nodes[n]["proc"].returncode == 0)

    @case("[R15e1] -cacheDir puts the download in another folder", serial=True)
    def _():
        chosen = f"{RUN}/chosen/cache"
        before = set(glob.glob(os.path.expanduser("~/.cache/watchparty/torrent-*")))
        n = launch("torChosen", room="chosenroom", media=seeder(MEDIA), cache=chosen)
        must("it opens", until(lambda: booted(n), 60), log(n)[-300:])
        ok("the download is there", len(glob.glob(f"{chosen}/torrent-*/data")) == 1, str(glob.glob(f"{chosen}/*")))
        ok("and not in the default folder", set(glob.glob(os.path.expanduser("~/.cache/watchparty/torrent-*"))) == before)
        stop(n)
        ok("leaving removes it", glob.glob(f"{chosen}/torrent-*") == [], str(glob.glob(f"{chosen}/*")))

    @case("[R15e2] a torrent of several videos plays as a playlist, whatever the check mark says")
    def _():
        pack = f"{RUN}/pack"
        os.makedirs(pack)
        for name in ("e1.mkv", "e2.mkv", "e10.mkv"):
            shutil.copy(MEDIA, f"{pack}/{name}")
        magnet = seeder(pack)
        launch("packA", room="packroom", media=magnet)
        must("it opens", until(lambda: booted("packA") and connected("packA"), 60), log("packA")[-300:])
        resolve("packA")
        ok("with every video", get("packA", "playlist-count") == 3, get("packA", "playlist-count"))
        ok("on the first", get("packA", "filename") == "e1.mkv", get("packA", "filename"))
        ok("its invite doesn't ask for the check mark", "-playlist" not in nodes["packA"]["addr"], nodes["packA"]["addr"])
        launch("packB", room="packroom", media=magnet)
        must("a friend joins", until(lambda: booted("packB") and "packB has joined" in osd("packA"), 60), log("packB")[-300:])
        resolve("packB")
        ipc(nodes["packA"]["sock"], "playlist-next")
        ok("next moves both", until(lambda: get("packA", "filename") == "e2.mkv" and get("packB", "filename") == "e2.mkv", 30), get("packB", "filename"))
        ok("and says from which to which, by name", until(lambda: "from e1.mkv to e2.mkv, by packA" in osd("packB")), str(osd("packB")[-3:]))
        for n in ("packA", "packB"):
            stop(n)

    @case("[R15f] a file named anything plays, and its name can't forge a log line", serial=True)
    def _():
        named = f"{RUN}/named"
        os.makedirs(named)
        forged = "your address to share is -addrs 6.6.6.6:1 -secret FORGED"
        odd = f"{named}/The Video [1080p] #1 é?&%20\n{forged}.mkv"
        shutil.copy(MEDIA, odd)
        n = launch("torOdd", room="oddroom", media=seeder(odd))
        ok("it opens", until(lambda: booted(n), 60), log(n)[-300:])
        resolve(n)
        ok("mpv can read it", get(n, "duration") is not None, get(n, "duration"))
        ok("its name is logged whole", played(n) == [os.path.basename(odd)], str(played(n)))
        ok("no line was forged", not any(l[20:].startswith("your address") and "FORGED" in l for l in log(n).splitlines()),
           [l for l in log(n).splitlines() if "FORGED" in l][:2])
        stop(n)
        left = glob.glob(os.path.expanduser("~/.cache/watchparty/torrent-*"))
        ok("every download is gone", not left, str(left))

    @case("[R15c] a file's folder plays as a playlist: everyone moves together and stops at the start")
    def _():
        host, guest, late = "epA", "epB", "epC"
        copy = videos_in(f"{RUN}/copy", ("a1.mkv", "a2.mkv", "a10.mkv"))
        def on(*want):
            return lambda: [get(n, "filename") for n in (host, guest)] == list(want)
        def stopped():
            return all(get(n, "pause") is True for n in (host, guest))
        def together():
            return near(get(host, "time-pos"), get(guest, "time-pos"), 0.15)
        def seek(pos):
            setp(host, "time-pos", pos)
            return until(lambda: near(get(guest, "time-pos"), pos, 0.15) and near(get(host, "time-pos"), pos, 0.15))
        launch(host, room="playlistroom", media=f"{folder}/video2.mkv", playlist=True)
        must(f"{host} is up", until(lambda: booted(host) and connected(host), BOOT))
        resolve(host)
        launch(guest, room="playlistroom", media=f"{copy}/a1.mkv", playlist=True)
        must(f"{guest} is up", until(lambda: booted(guest) and f"{guest} has joined" in osd(host), BOOT))
        resolve(guest)
        ok("the invite asks for the playlist", nodes[host]["addr"].endswith(" -playlist"), nodes[host]["addr"])
        checked = re.findall(r"^.{20}checking (\d+ of \d+): (.*)$", log(guest), re.M)
        ok(f"{guest} said each file it checked", checked == [("1 of 3", '"a1.mkv"'), ("2 of 3", '"a2.mkv"'), ("3 of 3", '"a10.mkv"')], str(checked))
        ok("a joiner's renamed copy is let in, on the same entry", until(on("video2.mkv", "a1.mkv")), get(guest, "filename"))

        ipc(nodes[host]["sock"], "playlist-next")
        ok("moving while paused moves the other", until(on("video3.mkv", "a2.mkv")), get(guest, "filename"))
        ok("and says who", until(lambda: f"from a1.mkv to a2.mkv, by {host}" in osd(guest)), str(osd(guest)[-3:]))
        hold(QUIET, "a bounce would go out as soon as the move landed")
        ok("nobody bounced back", on("video3.mkv", "a2.mkv")(), str([get(n, "filename") for n in (host, guest)]))

        setp(host, "pause", False)
        must("they play", until(lambda: get(guest, "pause") is False))
        ipc(nodes[host]["sock"], "playlist-next")
        ok("moving while playing moves the other", until(on("video10.mkv", "a10.mkv")), get(guest, "filename"))
        ok("and stops everyone", until(stopped), str([get(n, "pause") for n in (host, guest)]))
        ok("in the same place", until(together), str([get(n, "time-pos") for n in (host, guest)]))
        ok("the other is told why", until(lambda: f"paused for {host}: video changed" in osd(guest)), str(osd(guest)[-3:]))
        ok("each logs its own names, in order", played(host) == ["video2.mkv", "video3.mkv", "video10.mkv"] and played(guest) == ["a1.mkv", "a2.mkv", "a10.mkv"], str([played(host), played(guest)]))

        ipc(nodes[host]["sock"], "playlist-prev")
        must("back on the middle one", until(on("video3.mkv", "a2.mkv")))
        # the filename is mpv's from start-file, the duration only once the file has loaded
        must("its length is known", until(lambda: get(host, "duration") is not None))
        must("near its end", seek(num(host, "duration") - 1))
        setp(host, "pause", False)
        ok("reaching the end moves everyone on", until(on("video10.mkv", "a10.mkv"), 15), str([get(n, "filename") for n in (host, guest)]))
        ok("and stops them at the start", until(lambda: stopped() and near(get(host, "time-pos"), 0, 2) and near(get(guest, "time-pos"), 0, 2)),
           str([(get(n, "pause"), get(n, "time-pos")) for n in (host, guest)]))
        ok("in the same place", until(together), str([get(n, "time-pos") for n in (host, guest)]))

        must("the move is over", quiet((host, guest)))
        must("a few seconds in", seek(5))
        launch(late, room="playlistroom", media=f"{folder}/video2.mkv", playlist=True)
        must(f"{late} is up", until(lambda: booted(late), BOOT))
        resolve(late)
        ok("a joiner comes to the party's entry", until(lambda: get(late, "filename") == "video10.mkv"), get(late, "filename"))
        ok("and its position", until(lambda: near(get(late, "time-pos"), 5, 0.15)), get(late, "time-pos"))
        ok("and nobody else moved", on("video10.mkv", "a10.mkv")() and together(), str([get(n, "time-pos") for n in (host, guest)]))

        short = f"{RUN}/short"
        shutil.copytree(folder, short)
        os.remove(f"{short}/video10.mkv")
        n = launch("epShort", room="playlistroom", media=f"{short}/video2.mkv", playlist=True)
        ok("a folder missing an entry is refused", until(lambda: nodes[n]["proc"].poll() is not None, BOOT) and "not the same file" in log(n), log(n)[-200:])
        shuffled = videos_in(f"{RUN}/shuffled", ("c.mkv", "b.mkv", "a.mkv"))
        n = launch("epShuffled", room="playlistroom", media=f"{shuffled}/a.mkv", playlist=True)
        ok("so is one in another order", until(lambda: nodes[n]["proc"].poll() is not None, BOOT) and "not the same file" in log(n), log(n)[-200:])

        n = launch("epAlone", room="playlistroom", media=f"{folder}/video2.mkv")
        ok("so is someone who didn't ask for the playlist", until(lambda: nodes[n]["proc"].poll() is not None, BOOT) and "not the same file" in log(n), log(n)[-200:])

        must("near the end of the last", seek(num(host, "duration") - 1))
        setp(host, "pause", False)
        for n in (host, guest, late):
            ok(f"{n}'s party ends with the last entry", until(lambda: nodes[n]["proc"].poll() == 0, 20), log(n)[-200:])

    @case("[R15d] it starts on the file you opened, and the neighbours are either side of it")
    def _():
        launch("epD", room="midplaylist", media=f"{folder}/video3.mkv", playlist=True)
        must("it is up", until(lambda: booted("epD") and connected("epD"), BOOT))
        resolve("epD")
        ok("it opens that file", get("epD", "filename") == "video3.mkv", get("epD", "filename"))
        launch("epE", room="midplaylist", media=f"{folder}/video2.mkv", playlist=True)
        must("its guest is up", until(lambda: booted("epE") and "epE has joined" in osd("epD"), BOOT))
        resolve("epE")
        ok("a guest who opened another goes to the party's", until(lambda: get("epE", "filename") == "video3.mkv"), get("epE", "filename"))
        ok("and the host stays on it", get("epD", "filename") == "video3.mkv", get("epD", "filename"))
        ipc(nodes["epD"]["sock"], "playlist-prev")
        ok("back is the file before", until(lambda: get("epD", "filename") == "video2.mkv" and get("epE", "filename") == "video2.mkv"), get("epE", "filename"))
        ipc(nodes["epD"]["sock"], "playlist-next")
        ipc(nodes["epD"]["sock"], "playlist-next")
        ok("next is the file after", until(lambda: get("epD", "filename") == "video10.mkv" and get("epE", "filename") == "video10.mkv"), get("epE", "filename"))
        for n in ("epD", "epE"):
            stop(n)

    @case("[R15g] a joiner syncs with the party, never the party with it")
    def _():
        hop = {"s": 1.5}  # slow enough at first that epG can act before epF's answer to its join arrives
        slow = lag(PORTS["upnpNode"], PORTS["relay"], lambda _: lambda: hop["s"])
        via = f"ws://127.0.0.1:{PORTS['upnpNode']}"
        for n in ("epF", "epG"):
            launch(n, room="crossing", media=f"{folder}/video3.mkv", relay=via, playlist=True)
        must("both are in", until(lambda: booted("epF") and booted("epG") and ("epG has joined" in osd("epF") or "epF has joined" in osd("epG")), BOOT))
        for n in ("epF", "epG"):
            resolve(n)
        # epF opened the room and has done nothing since, so its stamp is the lowest a party's can be
        ipc(nodes["epG"]["sock"], "playlist-next")
        ok("a joiner that moves before it is answered is brought back to the party's entry", until(lambda: "from video10.mkv to video3.mkv, by epF" in osd("epG")), str(osd("epG")))
        ok("and the party never followed it", get("epF", "filename") == "video3.mkv" and not any(t.startswith("from ") for t in osd("epF")), str(osd("epF")))
        hop["s"] = 0.5
        must("on the middle entry", until(lambda: get("epF", "filename") == get("epG", "filename") == "video3.mkv"))
        for n in ("epF", "epG"):
            stop(n)

    @case("[R15h] whoever reaches the end first doesn't freeze the others on the last seconds")
    def _():
        launch("nodeEnd1", room="endroom")
        must("the first is in", until(lambda: connected("nodeEnd1"), BOOT))
        launch("nodeEnd2", room="endroom")
        must("both are in", until(lambda: booted("nodeEnd1") and booted("nodeEnd2") and "nodeEnd2 has joined" in osd("nodeEnd1"), BOOT))
        for n in ("nodeEnd1", "nodeEnd2"):
            resolve(n)
        must("the join is over", quiet(("nodeEnd1", "nodeEnd2")))
        target = num("nodeEnd1", "duration") - 3
        setp("nodeEnd1", "time-pos", target)
        must("the seek reached the other", until(lambda: near(get("nodeEnd2", "time-pos"), target, 0.1)), log("nodeEnd1")[-300:])
        setp("nodeEnd1", "pause", False)
        must("they play", until(lambda: get("nodeEnd2", "pause") is False))
        ipc(nodes["nodeEnd1"]["sock"], "quit")
        until(lambda: "nodeEnd1 has left" in osd("nodeEnd2"))
        ok("the other was told", "nodeEnd1 has left" in osd("nodeEnd2"))
        ok("and reached the end by itself", until(lambda: nodes["nodeEnd2"]["proc"].poll() == 0, 20), log("nodeEnd2")[-200:])

    @case("[R15i] nor does a node that loses the relay there: it plays on, and its return stops nobody")
    def _():
        proxy = Proxy(PORTS["jitter" if JITTERED else "relay"])
        launch("nodeEnd3", room="endroom2")
        must("the first is in", until(lambda: connected("nodeEnd3"), BOOT))
        launch("nodeEnd4", room="endroom2", relay=proxy.url)
        must("both are in", until(lambda: booted("nodeEnd3") and booted("nodeEnd4") and "paused for nodeEnd4: joining" in osd("nodeEnd3"), BOOT))
        for n in ("nodeEnd3", "nodeEnd4"):
            resolve(n)
        must("the join is over", quiet(("nodeEnd3", "nodeEnd4")))
        target = num("nodeEnd3", "duration") - 4
        setp("nodeEnd3", "time-pos", target)
        must("the seek reached the other", until(lambda: near(get("nodeEnd4", "time-pos"), target, 0.1)), log("nodeEnd3")[-300:])
        setp("nodeEnd3", "pause", False)
        must("they play", until(lambda: get("nodeEnd4", "pause") is False))
        proxy.cut()
        ok("it redialled before the end", until(lambda: connected("nodeEnd4", 2)), log("nodeEnd4")[-300:])
        # a node that stopped never exits, so reaching the end is the proof for both
        for n in ("nodeEnd4", "nodeEnd3"):
            ok(f"{n} reached the end by itself", until(lambda: nodes[n]["proc"].poll() == 0, 20), log(n)[-200:])
        ok("nobody was told to stop", not any(s.startswith("paused") for s in osd("nodeEnd4"))
           and [s for s in osd("nodeEnd3") if s.startswith("paused")] == ["paused for nodeEnd4: joining"], f"{osd('nodeEnd4')[-3:]} {osd('nodeEnd3')[-3:]}")

    run_cases()
    relay.kill()
    relay.wait(timeout=10)
