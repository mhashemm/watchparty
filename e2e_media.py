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
        ok("so is someone who didn't ask for the playlist, told to", until(lambda: nodes[n]["proc"].poll() is not None, BOOT) and "you picked one file: tick the box" in log(n), log(n)[-200:])

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

    @case("[R15j] mpv's reaction picker: its smiley opens the row, a click picks, it rests, and never means never")
    def _():
        # a window of its own: with --vo=null, mpv says the mouse is never over it. mpv is the
        # node's child itself, not xvfb-run's, or the node's end would leave it running
        x = subprocess.Popen(["Xvfb", "-displayfd", "1", "-screen", "0", "1280x720x24"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        display = x.stdout.readline().decode().strip()
        xmpv = f"{RUN}/xmpv"
        script(xmpv, f'DISPLAY=:{display} exec mpv "$@" --vo=x11 --geometry=1280x720 --no-border --no-osc')
        launch("rxA", room="reacting", mpv=xmpv)
        must("it is up", until(lambda: booted("rxA") and connected("rxA"), BOOT))
        resolve("rxA")
        sock = nodes["rxA"]["sock"]
        dims = (ipc(sock, "get_property", "osd-dimensions") or {}).get("data") or {}
        w, h = dims.get("w") or 1280, dims.get("h") or 720

        u = h / 720
        size, gap, pad = 52 * u, 12 * u, 14 * u
        peek = size * 0.55 + gap
        shown = max(1, min(9, int((w - 2 * pad - 32 * u + gap - peek) // (size + gap))))
        row_w = 2 * pad + shown * size + (shown - 1) * gap + peek
        x0 = (w - row_w) / 2
        order = json.load(open("mpv/emoji.json"))["order"]
        button = (int(w - 16 * u - 30 * u), int(h - 90 * u - 30 * u))
        row = lambda i: (int(x0 + pad + i * (size + gap) + size / 2), int(h / 2))
        # what the picker says reaches every client of mpv's, this listener too
        picked = []
        listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        listener.connect(sock)

        def listen():
            for line in listener.makefile("rb"):
                with contextlib.suppress(ValueError):
                    m = json.loads(line)
                    if m.get("event") == "client-message" and m["args"][:1] == ["watchparty-reacted"]:
                        picked.append(m["args"][1])
        threading.Thread(target=listen, daemon=True).start()

        def holds():
            """Whether the picker holds mpv's click: it takes clicks only while over itself."""
            b = (ipc(sock, "get_property", "input-bindings") or {}).get("data") or []
            return any(c.get("cmd", "").endswith("/watchparty-picker-MBTN_LEFT") for c in b)

        def move(x, y):
            ipc(sock, "mouse", x + 1, y)
            ipc(sock, "mouse", x, y)

        def over(x, y):
            """Moves the mouse to x, y, a pixel off and back each try, until the picker holds."""
            return until(lambda: move(x, y) or holds(), 5)

        def not_over(x, y, why):
            move(x, y)
            hold(0.4, why)
            return not holds()

        def click(x, y):
            """A click where the mouse just moved, as a tap is. Then a rest, as a person's next
            move comes after: the script reads a click's place as it hears it, a moment later."""
            ipc(sock, "mouse", x, y, 0, "single")
            hold(0.2, "the script hears the click before the next move")

        ipc(sock, "mouse", w // 3, h // 3)
        must("the smiley shows and takes clicks", over(*button))
        click(*button)
        must("its row opens", over(*row(0)))
        click(*row(0))
        ok("the first emoji is picked", until(lambda: picked == ["1f602"]), str(picked))
        must("the smiley is still there", over(*button))
        click(*button)
        ok("so a pick right after isn't made: the picker rests, its smiley opens nothing",
           not_over(*row(1), "the row would open") and picked == ["1f602"], str(picked))
        hold(1.6, "the rest ends")
        ipc(sock, "script-message-to", "watchparty_reactions", "watchparty-picker", "off")
        ipc(sock, "mouse", w // 3, h // 3)
        ok("never means no smiley to click", not_over(*button, "the smiley would show"))
        ipc(sock, "script-message-to", "watchparty_reactions", "watchparty-picker", "move")
        ipc(sock, "mouse", w // 3, h // 3)
        must("back on, it shows again", over(*button))
        click(*button)
        # a tap: the mouse goes there and clicks in one go, before the script hears it moved
        click(*row(1))
        ok("the second is picked, by a tap", until(lambda: picked == ["1f602", "1f62d"]), str(picked))

        # what the picker shows leaves no trace in mpv's properties: its areas on screen, against
        # the same areas while it is off (the video is paused, so nothing else changes there)
        shot = f"{RUN}/rxA.png"

        def area(x, y, aw, ah):
            ipc(sock, "screenshot-to-file", shot, "window")
            return subprocess.run(["ffmpeg", "-v", "error", "-i", shot, "-vf", f"crop={int(aw)}:{int(ah)}:{int(x)}:{int(y)}",
                                   "-f", "rawvideo", "-pix_fmt", "gray", "-"], capture_output=True).stdout

        def differs(a, b):
            return len(a) != len(b) or sum(abs(p - q) for p, q in zip(a, b)) / max(1, len(a)) > 4
        row_area = lambda: area(x0, (h - size) / 2 - pad, row_w, size + 2 * pad)
        button_area = lambda: area(w - 76 * u, h - 150 * u, 60 * u, 60 * u)
        wiggled = itertools.count()

        def wiggle(x, y):
            ipc(sock, "mouse", x + next(wiggled) % 2, y)

        hold(2.2, "the burst flies off and the rest ends")
        ipc(sock, "script-message-to", "watchparty_reactions", "watchparty-picker", "off")
        row_off, button_off = row_area(), button_area()
        ipc(sock, "script-message-to", "watchparty_reactions", "watchparty-picker", "move")
        must("the smiley shows", over(*button))
        click(*button)
        must("its row opens", over(*row(0)) and differs(row_area(), row_off))
        ok("moving away closes the row", until(lambda: wiggle(w // 4, h // 4) or not differs(row_area(), row_off), 5))
        ok("while the smiley stays, as the mouse moves", differs(button_area(), button_off))

        ipc(sock, "script-message-to", "watchparty_reactions", "watchparty-picker", "off")
        ipc(sock, "script-message-to", "watchparty_reactions", "watchparty-picker", "corner")
        for _ in range(4):
            wiggle(w // 4, h // 4)
        hold(0.4, "a move far from the corner would show the smiley")
        ok("near its corner: a move elsewhere doesn't show the smiley", not differs(button_area(), button_off))
        ok("a move near it does", until(lambda: wiggle(w * 3 // 4, h * 3 // 4) or differs(button_area(), button_off), 5))

        ipc(sock, "script-message-to", "watchparty_reactions", "watchparty-picker", "move")
        must("the smiley takes clicks", over(*button))
        click(*button)
        must("the row has an arrow at its end, as there is more", over(int(x0 + row_w), h // 2))
        click(int(x0 + row_w), h // 2)
        # the dimmed emoji peeking in at that end, off the arrow, is the arrow too
        peek_at = (int(x0 + row_w - pad - 14 * u), int(h / 2 - 20 * u))
        must("as is the emoji cut by that end", over(*peek_at))
        click(*peek_at)
        must("which scrolled it twice", over(*row(0)))
        click(*row(0))
        ok("by a rowful less one each: its first is now the one that was last, twice over",
           until(lambda: picked[2:] == [order[2 * (shown - 1)]]), f"{picked} {order[2 * (shown - 1)]}")
        hold(2.2, "the smiley rests after a pick")
        must("the smiley takes clicks again", over(*button))
        click(*button)
        must("its row opens", over(*row(0)))
        click(*row(0))
        ok("where it was left: its first is the same one", until(lambda: picked[3:] == [order[2 * (shown - 1)]]), str(picked))
        hold(2.2, "the smiley rests after a pick")
        # a smaller window draws smaller emoji: the row still opens on that one, lined up
        ipc(sock, "set_property", "geometry", "640x360")
        dims = lambda: (ipc(sock, "get_property", "osd-dimensions") or {}).get("data") or {}
        must("the window is smaller", until(lambda: dims().get("h") == 360, 5), str(dims()))
        w2, h2 = dims()["w"], dims()["h"]
        u2 = h2 / 720
        size2, gap2, pad2 = 52 * u2, 12 * u2, 14 * u2
        peek2 = size2 * 0.55 + gap2
        shown2 = max(1, min(9, int((w2 - 2 * pad2 - 32 * u2 + gap2 - peek2) // (size2 + gap2))))
        x2 = (w2 - (2 * pad2 + shown2 * size2 + (shown2 - 1) * gap2 + peek2)) / 2
        must("its smiley takes clicks", over(int(w2 - 46 * u2), int(h2 - 120 * u2)))
        click(int(w2 - 46 * u2), int(h2 - 120 * u2))
        first2 = (int(x2 + pad2 + size2 / 2), int(h2 / 2))
        must("its row opens", over(*first2))
        click(*first2)
        ok("its first is the same one", until(lambda: picked[4:] == [order[2 * (shown - 1)]]), str(picked))

        def window(gw, gh):
            """mpv's window resized, and the picker's places in it: its smiley, the row's ith emoji, how many it shows."""
            ipc(sock, "set_property", "geometry", f"{gw}x{gh}")
            must(f"the window is {gw}x{gh}", until(lambda: dims().get("h") == gh, 5), str(dims()))
            ww, wh = dims()["w"], dims()["h"]
            wu = wh / 720
            ws, wg, wp = 52 * wu, 12 * wu, 14 * wu
            wpeek = ws * 0.55 + wg
            n = max(1, min(9, int((ww - 2 * wp - 32 * wu + wg - wpeek) // (ws + wg))))
            wx = (ww - (2 * wp + n * ws + (n - 1) * wg + wpeek)) / 2
            return (int(ww - 46 * wu), int(wh - 120 * wu)), lambda i: (int(wx + wp + i * (ws + wg) + ws / 2), int(wh / 2)), n
        hold(2.2, "the smiley rests after a pick")
        # a narrow window shows fewer emoji, so its row scrolls further: to its end, then wider
        smiley, at, n = window(360, 640)
        must("narrow, its smiley takes clicks", over(*smiley))
        click(*smiley)
        must("its row opens", over(*at(0)))
        for _ in range(len(order)):
            ipc(sock, "mouse", *at(0), 4, "single")
        hold(0.3, "the wheel's last turn lands")
        click(*smiley)
        smiley, at, n = window(1280, 720)
        must("wide again, its smiley takes clicks", over(*smiley))
        click(*smiley)
        must("its row opens", over(*at(0)))
        ipc(sock, "mouse", *at(0), 3, "single")
        must("a wheel turn back lands", over(*at(0)))
        click(*at(0))
        ok("it opens at the end the wider row allows, so one turn back moves it",
           until(lambda: picked[5:] == [order[len(order) - n - 1]]), f"{picked} {order[len(order) - n - 1]}")
        listener.close()
        ok("no log has them", "watchparty-react" not in log("rxA"))
        stop("rxA")
        x.kill()
        x.wait()

    run_cases()
    relay.kill()
    relay.wait(timeout=10)
