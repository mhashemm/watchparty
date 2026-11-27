"""The gui phase: real GUIs, each a native window on Xvfb, driven through its e2e control
socket (gui/e2e.go), and a few by real X input."""
from e2e_core import *
import e2e_core


def gui_suite():
    KEY = "e2ekey"
    print("\n=== gui: real GUIs, each a window on Xvfb, and GUIs of its own for every case ===")
    e2e_core.BUS = private_bus()
    launch_relay({"CREATE_KEY": KEY})
    until(lambda: listening(PORTS["relay"]))
    # the playlist the check-mark cases open, each in a room of its own
    folder = videos_in(f"{RUN}/guifolder", ("e1.mkv", "e2.mkv", "e3.mkv"))

    def mpv_of(n):
        return child_mpv_socket(nodes[n]["proc"].pid)

    def joined(n):
        until(lambda: status(n).startswith("You're in"), BOOT)
        nodes[n]["sock"] = mpv_of(n)
        return status(n).startswith("You're in")

    def gui(name, key=None, relay=RELAY_URL, **kw):
        """A GUI of its own for one case, with its relay, mpv flags and maybe the create key
        saved, killed when the case ends. Its name is its folder, so a case's are its own."""
        n = launch_gui(name, **kw)
        on_end(lambda: kill_gui(n))
        fields = {"set-relay": relay, "mpvFlags": MPVFLAGS, **({"createKey": key} if key else {})}
        must(f"{n} saved its settings", save_settings(n, **fields) == "Saved.", ui(n)["settings"]["saved"])
        return n

    def chats(n):
        """The chat under the box, newest first, as "who: text", "ours " before this node's own."""
        emoji = lambda code: "".join(chr(int(c, 16)) for c in code.split("-"))
        return [("ours " if m["ours"] else "") + m["who"] + ": " + (emoji(m["react"]) if m["react"] else m["text"]) for m in ui(n)["said"]]

    def press_continue(n):
        tab(n, "history")
        must(f"{n}'s newest party offers Continue", ui(n)["history"][0]["button"] == "Continue", str(ui(n)["history"][:1]))
        run = ui(n)["run"]
        click(n, "history:0")
        until(lambda: ui(n)["run"] > run, BOOT)

    @case("[G1] the window opens at the phone-sized 560x940, and a dev build offers no update")
    def _():
        H = gui("g1", display=xvfb())
        geometry = xdo(H, "getwindowgeometry", x_window(H))
        ok("it is 560x940", "Geometry: 560x940" in geometry, geometry)
        about = ui(H)["about"]
        ok("a dev build says so and offers no update", about["version"] == "Development build" and about["button"] == "", str(about))

        part("[G1b] it reopens at the size it was left")
        xdo(H, "windowsize", x_window(H), 640, 820)
        until(lambda: "Geometry: 640x820" in xdo(H, "getwindowgeometry", x_window(H)), 5)
        hold(QUIET, "the window hears of its new size")
        x_close(H)
        must(f"{H} closed", exited(H))
        H2 = launch_gui("g1", tag="g1again", display=xvfb())
        on_end(lambda: kill_gui(H2))
        geometry = xdo(H2, "getwindowgeometry", x_window(H2))
        ok("at 640x820", "Geometry: 640x820" in geometry, geometry)
        ok("saved beside the settings", state(H2)["settings"].get("mpvFlags") == MPVFLAGS, str(state(H2)["settings"]))

    @case("[G2] the tabs switch views")
    def _():
        H = gui("g2")
        for view in ("history", "settings", "party"):
            tab(H, view)
            ok(f"{view} tab shows its view", ui(H)["tab"] == view, ui(H)["tab"])

    @case("[G3] Settings saves, and fills the party's relay")
    def _():
        H = launch_gui("g3")
        on_end(lambda: kill_gui(H))
        ok("it saved", save_settings(H, **{"set-relay": RELAY_URL, "createKey": KEY, "mpvFlags": MPVFLAGS}) == "Saved.")
        s = state(H)["settings"]
        ok("the settings reached the store", (s["relay"], s["createKey"], s["mpvFlags"]) == (RELAY_URL, KEY, MPVFLAGS), str(s))
        ok("the party's relay was filled in", ui(H)["form"]["relay"] == RELAY_URL)
        store = ui(H)["about"]["storePath"]
        ok("the store is where About says", store == f"{nodes[H]['home']}/config/watchparty", store)
        ok("the settings are 0600 in a 0700 folder", (os.stat(f"{store}/gui.json").st_mode & 0o777, os.stat(store).st_mode & 0o777) == (0o600, 0o700))
        st = ui(H)["settings"]
        ok("the mpv path shows the one it will use", st["mpvDefault"] == f"{nodes[H]['home']}/bin/mpv", st["mpvDefault"])
        ok("the cache folder shows the one it will use", st["cacheDirDefault"] == f"{nodes[H]['home']}/.cache/watchparty", st["cacheDirDefault"])

    @case("[G3b] the ? button opens the help, one topic at a time; Escape and a real click elsewhere close it")
    def _():
        H = gui("g3b", display=xvfb())
        tips = lambda: ui(H)["tips"]
        ok("it starts closed", not tips()["open"])
        click(H, "tip")
        ok("a click opens it", tips()["open"])
        ok("each topic has its own section", tips()["titles"] == ["Start a party", "Join a party", "Watch together", "Automatic pauses", "Who's in the party", "Chat", "Reactions", "Slow networks", "Links", "Playlists", "Torrents", "Connecting"], str(tips()["titles"]))
        ok("it isn't tied to one kind of video", not COUPLED.search(tips()["text"]), str(COUPLED.findall(tips()["text"])))
        x0, y0, x1, y1 = tips()["area"]
        x_click(H, (x0 + x1) // 2, y0 + 20)
        ok("a real click inside keeps it open", not until(lambda: not tips()["open"], 1) and tips()["open"])
        click(H, "tip")
        ok("the button closes it", not tips()["open"])
        click(H, "tip")
        # right after it opens, with no frame between: the first Escape must not be lost
        x_key(H, "Escape")
        ok("a real Escape closes it", until(lambda: not tips()["open"], 5))
        click(H, "tip")
        x_click(H, 30, 30)  # as soon, the same
        ok("a real click elsewhere closes it", until(lambda: not tips()["open"], 5))

    @case("[G4] Browse uses the desktop portal, and says so when there is none")
    def _():
        H, G = gui("g4"), gui("g4NoPortal", portal=False)
        click(H, "browse")
        ok(f"{H} picked the video through the portal", until(lambda: ui(H)["form"]["file"] == MEDIA), ui(H)["form"]["file"])
        tab(H, "settings")
        click(H, "browse-cacheDir")
        ok(f"{H} picked a folder through it too", until(lambda: ui(H)["settings"]["cacheDir"] == os.path.dirname(MEDIA)), ui(H)["settings"]["cacheDir"])
        fill(H, cacheDir="")
        until(lambda: not ui(H)["settings"]["picking"])
        click(H, "browse-mpv")
        ok(f"{H} can pick mpv, which no video filter would show", until(lambda: ui(H)["settings"]["mpv"] == MEDIA + ".unfiltered"), ui(H)["settings"]["mpv"])
        click(G, "browse")
        until(lambda: ui(G)["error"])
        ok(f"{G}, with no portal, is told to type the path", ui(G)["error"].startswith("No file picker found"), ui(G)["error"])

    @case("[G5] the form refuses what can't start")
    def _():
        H = gui("g5")
        fill(H, file="   ")
        click(H, "go")
        ok("a blank path asks for a video", ui(H)["error"] == "Pick a video first.", ui(H)["error"])
        fill(H, file=MEDIA, relay="")
        click(H, "go")
        ok("relay mode with no relay is refused", ui(H)["error"] == "Diagnosis: enter the relay address, or paste an invite.", ui(H)["error"])
        ok("and nothing started", not ui(H)["ran"])

    @case("[G6] the host starts a relay party from the form and gets an invite")
    def _():
        H, G = gui("g6Host", key=KEY), gui("g6Guest")
        save_settings(G, mpv=f"{nodes[G]['home']}/slow-mpv")
        start(H, hostname=H, relay=RELAY_URL, file=MEDIA)
        must(f"{H} is in", joined(H), status(H))
        until(lambda: ui(H)["invite"] != "")
        invite = ui(H)["invite"]
        room, secret = invite.split("-room ", 1)[1].split()[0], invite.split("-secret ", 1)[1].split()[0]
        ok(f"{H} shows an invite", invite == f"-relay {RELAY_URL} -room {room} -secret {secret}", invite)
        ok(f"{H} hides the form", not ui(H)["form"]["shown"] and ui(H)["go"] == "End the party")

        part("[G7] Copy invite puts it on the clipboard")
        click(H, "copy")
        ok("the button says Copied", ui(H)["copy"] == "Copied", ui(H)["copy"])
        ok("the clipboard has the invite", until(lambda: clipboard(H) == invite, 5), clipboard(H))
        ok("the button comes back by itself", until(lambda: ui(H)["copy"] == "Copy invite", 5))

        part("[G8] Save chart saves the whole log where the OS's dialog says")
        fill(H, chart=True)
        click(H, "save-chart")
        until(lambda: ui(H)["chart"]["saved"])
        saved = glob.glob(f"{os.path.dirname(MEDIA)}/watchparty-*.log")
        ok("one watchparty-<date>.log, beside where the fake portal saves", len(saved) == 1, str(saved))
        chart_file = open(saved[0]).read() if saved else ""
        ok("it holds the party's log", "init done" in chart_file and invite in chart_file)
        ok("the window says where it went", saved and ui(H)["chart"]["saved"] == f"Saved as {saved[0]}.", ui(H)["chart"]["saved"])

        part("[G9] the guest pastes the whole invite line and joins, after mpv takes its time")
        fill(G, invite="2026/11/01 12:00:00 your address to share is " + invite)
        f = ui(G)["form"]
        ok(f"{G} filled relay, room and secret from it", (f["relay"], f["room"], f["secret"], f["inviteFilled"]) == (RELAY_URL, room, secret, True), str(f))
        start(G, hostname=G, file=MEDIA)
        ok(f"{G} said it is opening mpv", until(lambda: status(G).startswith("Opening mpv"), BOOT), status(G))
        ok(f"{G}'s turtles hurry while it waits", ui(G)["walking"])
        must(f"{G} is in", joined(G), status(G))
        until(lambda: f"{G} has joined" in feed(H) and f"connected to {H}" in feed(G))
        ok(f"{H}'s feed says {G} joined", f"{G} has joined" in feed(H), str(feed(H)))
        ok(f"{G}'s feed says it connected", f"connected to {H}" in feed(G), str(feed(G)))
        members = lambda n: [f"{m} {since}" for m, since in ui(n)["members"]]
        ok(f"{H}'s members name {G} since it joined", [m.split(" since ")[0] for m in members(H)] == ["You", G], str(members(H)))
        ok(f"{G}'s name {H} as already here", members(G)[1:] == [f"{H} already here"], str(members(G)))

        part(f"[G10] a resume in {G}'s mpv: the feeds say who did it")
        must("the join is over", quiet((H, G)))
        setp(G, "pause", False)
        until(lambda: get(H, "pause") is False and f"resumed by {G}" in feed(H) and feed(G)[:1] == ["resumed by you"])
        ok(f"{H}'s mpv resumed", get(H, "pause") is False)
        ok(f"{H}'s feed credits {G}", f"resumed by {G}" in feed(H), str(feed(H)))
        ok(f"{G}'s feed says you", feed(G)[:1] == ["resumed by you"], str(feed(G)))

        part("[G11] a seek while playing pauses everyone, and the feeds say why")
        reasons = log(G).count('"name":"pause-reason"')
        ipc(nodes[G]["sock"], "seek", 30)
        # mpv's notice can land on either side of the pause, and the pause-reason that retracts
        # "paused by you" comes after it; a line in the log is one the window reads next frame
        until(lambda: get(H, "pause") is True and f"paused for {G}: seek" in feed(H) and "paused for everyone: seek" in feed(G)
              and log(G).count('"name":"pause-reason"') > reasons)
        ok(f"{H}'s feed says why", f"paused for {G}: seek" in feed(H), str(feed(H)))
        ok(f"{G}'s feed says why", "paused for everyone: seek" in feed(G), str(feed(G)))
        ok(f"{G}'s feed doesn't also say paused by you", "paused by you" not in feed(G), str(feed(G)))

        part(f"[G12] resuming after the seek shows up in {G}'s feed")
        setp(G, "pause", False)
        until(lambda: get(H, "pause") is False and feed(G)[:1] == ["resumed by you"])
        ok(f"{G}'s feed moved on from the seek", feed(G)[:1] == ["resumed by you"], str(feed(G)))

        part(f"[G13] a pause in {H}'s mpv")
        setp(H, "pause", True)
        until(lambda: get(G, "pause") is True and feed(H)[:1] == ["paused by you"] and f"paused by {H}" in feed(G))
        ok(f"{H}'s feed says you", feed(H)[:1] == ["paused by you"], str(feed(H)))
        ok(f"{G}'s feed credits {H}", f"paused by {H}" in feed(G), str(feed(G)))

        part(f"[G13b] {G} says something: both windows show it, and nothing keeps it")
        said = "g13b-chat popcorn?"
        # every file under a GUI's folder, and its log, which a desktop can keep
        kept = lambda: [f for n in (H, G) for f in [nodes[n]["log"], *glob.glob(f"{nodes[n]['home']}/**", recursive=True)]
                        if os.path.isfile(f) and not os.path.islink(f) and said.encode() in open(f, "rb").read()]
        fill(G, chat="😀" * 205)
        ok("the box takes 200 characters, an emoji as one", len(ui(G)["chat"]["text"]) == 200, str(len(ui(G)["chat"]["text"])))
        fill(G, chat=said)
        click(G, "send")
        ok(f"{H} has it under {G}'s name", until(lambda: chats(H)[:1] == [f"{G}: {said}"]), str(chats(H)))
        ok(f"{G} has it as its own, and the box emptied", until(lambda: chats(G)[:1] == [f"ours you: {said}"] and ui(G)["chat"]["text"] == ""), str(chats(G)))
        ok("it isn't in the feed", not any(said in line for n in (H, G) for line in feed(n)))
        ok("no Patient chart has it", not any(said in chart(n) for n in (H, G)))
        ok("no file has it", kept() == [], str(kept()))
        for text in ("h1", "h2", "h3"):
            fill(H, chat=text)
            click(H, "send")
            must(f"{H}'s {text} reached {G}", until(lambda: chats(G)[:1] == [f"{H}: {text}"]))
        ok(f"{G} keeps all four, the first last", len(chats(G)) == 4 and "popcorn" in chats(G)[3], str(chats(G)))
        click(H, "react-open")
        ok("the smiley opens the reactions", ui(H)["chat"]["strip"])
        click(H, "react-open")
        ok("and closes them", not ui(H)["chat"]["strip"])
        click(H, "react-open")
        laugh = ui(H)["chat"]["reactions"].index("1f602")
        click(H, f"react:{laugh}")
        ok(f"{G} has {H}'s reaction", until(lambda: chats(G)[:1] == [f"{H}: 😂"]), str(chats(G)))
        c = ui(H)["chat"]
        ok(f"{H} has it as its own, the row closed and the button rests",
           until(lambda: chats(H)[:1] == ["ours you: 😂"]) and not c["strip"] and c["resting"], str((chats(H), c)))
        ok("no log has it", not any(b"watchparty-react" in open(nodes[n]["log"], "rb").read() for n in (H, G)))

        part(f"[G14] {H}'s history has the real room and secret, and who came")
        store = ui(H)["about"]["storePath"]
        last = state(H)["history"][-1]
        ok("room and secret from the invite", (last.get("room"), last.get("secret")) == (room, secret), str(last))
        ok(f"{G} is in its people", last.get("people") == [G], str(last.get("people")))
        ok("the history and its secret are 0600 in their own file", os.stat(f"{store}/history.json").st_mode & 0o777 == 0o600 and secret in open(f"{store}/history.json").read())
        ok("the settings file has no party in it", secret not in open(f"{store}/gui.json").read() and "history" not in open(f"{store}/gui.json").read())

        part(f"[G15] {G} ends the party from its window, which pauses {H}")
        setp(H, "pause", False)
        must("the party is playing", until(lambda: get(G, "pause") is False and feed(H)[:1] == ["resumed by you"]))
        asked = end(G)
        ok("it asked first", asked == "End the party? mpv closes for you, and everyone else keeps watching.", str(asked))
        ok(f"{G} shows the form again", ui(G)["form"]["shown"] and ui(G)["go"] == "Start the party")
        ok(f"{G}'s mpv closed", until(lambda: not mpv_of(G)))
        ok(f"{H}'s feed says {G} left", until(lambda: f"{G} has left" in feed(H)), str(feed(H)))
        ok(f"{H} paused and says why", until(lambda: get(H, "pause") is True and f"paused for {G}: left" in feed(H)), str(feed(H)))
        ok(f"{H}'s feed doesn't blame {H}", feed(H)[0] != "paused by you", str(feed(H)))
        ok(f"{G}'s chat went with its party, and the box with it", chats(G) == [] and not ui(G)["chat"]["shown"], str(chats(G)))
        ok(f"{H}'s, still in it, kept it", f"{G}: {said}" in chats(H), str(chats(H)))
        end(H)
        ok(f"{H}'s went at its end", chats(H) == [], str(chats(H)))
        ok("still no file has it", kept() == [], str(kept()))

    @case("[G16] a missing mpv is diagnosed as one")
    def _():
        G = gui("g16")
        save_settings(G, mpv="/nonexistent/mpv")
        start(G, hostname=G, file=MEDIA, room="g16", secret="g16secret")
        until(lambda: status(G).startswith("Diagnosis"), BOOT)
        ok("it says mpv isn't installed", status(G).startswith("Diagnosis: mpv isn't installed"), status(G))
        ok("in red, with the chart open", ui(G)["bad"] and ui(G)["chart"]["open"])

    @case("[G17] a wrong secret, and a different file, are diagnosed")
    def _():
        with party("g17", 1, create_key=KEY):
            G = gui("g17Guest")
            start(G, hostname=G, file=MEDIA, room="g17", secret="wrong" + SECRET)
            until(lambda: status(G).startswith("Diagnosis"), BOOT)
            ok("the relay rejected the secret", status(G) == "Diagnosis: the relay rejected this room or secret. Check the invite and start again.", status(G))
            _, diff = variants()
            start(G, secret=SECRET, file=diff)
            until(lambda: status(G) == NOT_THE_SAME_VIDEO, BOOT)
            ok("the relay rejected the file", status(G) == NOT_THE_SAME_VIDEO, status(G))
            rows = json.dumps((ui(G)["refusal"] or {}).get("rows"))
            ok("and it shows what each plays", os.path.basename(MEDIA) in rows and os.path.basename(diff) in rows, rows)
            ok(f"{G}'s party ended", until(lambda: ui(G)["form"]["shown"]))

    @case("[G17b] opening a new party without the create key is diagnosed, not left waiting")
    def _():
        G = gui("g17b")
        start(G, hostname=G, file=MEDIA, room="", secret="")
        until(lambda: status(G).startswith("Diagnosis"), BOOT)
        ok("it asks for the create key", status(G) == "Diagnosis: this relay needs a create key to open a new party. Add it in Settings and start again.", status(G))
        ok(f"{G}'s party ended", until(lambda: ui(G)["form"]["shown"]))

    @case("[G17c] a relay that is rate limiting, or full, says so instead of \"Lost the relay\"")
    def _():
        G = gui("g17c")
        busy = {"status": 429}

        class Busy(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(busy["status"])
                self.send_header("Content-Length", "0")
                self.end_headers()

            def log_message(self, format, *args):
                pass
        refusing = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Busy)
        threading.Thread(target=refusing.serve_forever, daemon=True).start()
        on_end(refusing.shutdown)
        start(G, hostname=G, file=MEDIA, relay=f"ws://127.0.0.1:{refusing.server_address[1]}", room="busy", secret="busy")
        ok("a rate limit is named", until(lambda: status(G) == "This relay has had too many attempts from your network. Trying again; it clears within the hour.", BOOT), status(G))
        busy["status"] = 503
        ok("so is a full relay", until(lambda: status(G) == "The relay is full right now. Trying again…", BOOT), status(G))
        end(G)
        ok(f"{G}'s party ended", until(lambda: status(G) == "The party has ended."), status(G))

        part("[G17f] a relay that takes the connection and never answers: mpv is ready, and it is not in yet")
        silent = socket.create_server(("127.0.0.1", 0))  # never accepts: the handshake waits
        on_end(silent.close)
        start(G, hostname=G, file=MEDIA, relay=f"ws://127.0.0.1:{silent.getsockname()[1]}", room="silent", secret="silent")
        must("its mpv is ready", until(lambda: booted(G), BOOT), log(G)[-300:])
        ok("it says it is connecting", until(lambda: status(G) == "Connecting to the relay…"), status(G))
        end(G)

    @case("[G17d] a magnet link nobody seeds says so, and Stop still works")
    def _():
        G = gui("g17d")
        start(G, hostname=G, file="magnet:?xt=urn:btih:" + "ab" * 20 + "&dn=The.Video&tr=udp://tracker.invalid/announce", room="g17d", secret="g17dsecret")
        until(lambda: status(G).startswith("Looking"), BOOT)
        ok("it says it is looking", status(G) == "Looking for this torrent's peers. It can take a while…", status(G))
        # the trail shows once the step has taken half a second: the window asks for that frame itself
        until(lambda: ui(G)["trail"], BOOT)
        trail = [("*" if st == "now" else "") + label for label, st in ui(G)["trail"]]
        ok("its trail is on the torrent, the steps after it to come", trail == ["*Torrent", "Relay", "mpv", "Video"], str(trail))
        ok("and started no mpv", not mpv_of(G))
        end(G)
        ok("Stop ends it with no diagnosis", until(lambda: status(G) == "The party has ended."), status(G))
        chosen = f"{RUN}/guichosen"
        ok("the cache folder saves", save_settings(G, cacheDir=chosen) == "Saved.")
        ok("and reaches the store", state(G)["settings"]["cacheDir"] == chosen, str(state(G)["settings"]))
        go(G)
        ok("a party uses it", until(lambda: glob.glob(f"{chosen}/torrent-*")), str(glob.glob(f"{chosen}/*")))
        end(G)
        ok("and leaving empties it", glob.glob(f"{chosen}/torrent-*") == [], str(glob.glob(f"{chosen}/*")))
        ok("emptying the setting saves", save_settings(G, cacheDir="") == "Saved." and state(G)["settings"]["cacheDir"] == "")

    @case("[G17e] the check mark loads the file's folder, and starts on that file")
    def _():
        G = gui("g17e")
        fill(G, playlist=True)
        start(G, hostname=G, file=f"{folder}/e2.mkv", room="g17e", secret="g17esecret")
        until(lambda: mpv_of(G), BOOT)
        nodes[G]["sock"] = mpv_of(G)
        ok("it opens the picked file", until(lambda: get(G, "filename") == "e2.mkv"), get(G, "filename"))
        ok("with its neighbours", get(G, "playlist-count") == 3, get(G, "playlist-count"))
        ok("history remembers it", state(G)["history"][-1].get("playlist") is True, str(state(G)["history"][-1]))
        groups = lambda: feed_groups(G)
        def showing(*names):
            return lambda: [g[0] for g in groups()] == list(names) and [g[1] for g in groups()] == [True] + [False] * (len(names) - 1)
        videos = lambda: state(G)["history"][-1].get("videos", [])
        ok("the feed opens on that file", until(showing("e2.mkv")), str(groups()))
        ok("history has it from when it opened", [v["name"] for v in videos()] == ["e2.mkv"], str(videos()))
        ipc(nodes[G]["sock"], "playlist-next")
        ok("the next file gets its own group, and only it is open", until(showing("e3.mkv", "e2.mkv")), str(groups()))
        ipc(nodes[G]["sock"], "playlist-prev")
        ok("going back reopens that file's group", until(showing("e2.mkv", "e3.mkv")), str(groups()))
        ok("every switch is its own entry", until(lambda: [v["name"] for v in videos()] == ["e2.mkv", "e3.mkv", "e2.mkv"]), str(videos()))
        ok("each was left when the next opened", all(a.get("ended") == b["started"] for a, b in zip(videos(), videos()[1:])), str(videos()))
        ok("the open one hasn't been left", "ended" not in videos()[-1], str(videos()))
        end(G)
        ok("ending the party finishes it", videos()[-1].get("ended") == state(G)["history"][-1]["ended"], str(videos()))
        tab(G, "history")
        row = ui(G)["history"][0]
        ok("the History page names the folder by its full path, counts files rather than plays, and shows each with its times",
           row["title"] == f"{folder} (2 videos)" and any(v.startswith("e3.mkv, ") for v in row["videos"]) and any(" to " in v for v in row["videos"]), str(row))

    @case("[G18] a guest who comes first waits for the host, then gets in by itself")
    def _():
        H, G = gui("g18Host", key=KEY), gui("g18Guest")
        start(G, hostname=G, file=MEDIA, room="g18", secret="g18secret")
        until(lambda: status(G).startswith("Waiting"), BOOT)
        ok(f"{G} is waiting for the host", status(G) == "Waiting for the host to open this room. It starts by itself once they're in.", status(G))
        start(H, hostname=H, file=MEDIA, room="g18", secret="g18secret")
        must(f"{H} opened it", joined(H), status(H))
        must(f"{G} got in", joined(G), status(G))

    @case("[G19] losing the relay shows on both, both play on, and both come back")
    def _():
        # a relay of its own, since this one dies; delayed like the phase's under --jitter
        port = free_port()
        own = [launch_relay({"CREATE_KEY": KEY}, port)]
        on_end(lambda: own[0].kill())
        until(lambda: listening(port))
        url = f"ws://127.0.0.1:{port}"
        if JITTERED:
            hop = free_port()
            on_end(lag(hop, port, jitter).close)
            url = f"ws://127.0.0.1:{hop}"
        H, G = gui("g19Host", key=KEY, relay=url), gui("g19Guest", relay=url)
        start(H, hostname=H, file=MEDIA, room="g19", secret="g19secret")
        must(f"{H} is in", joined(H), status(H))
        start(G, hostname=G, file=MEDIA, room="g19", secret="g19secret")
        must(f"{G} is in", joined(G), status(G))
        must(f"{G}'s join is over", until(lambda: f"paused for {G}: joining" in feed(H)) and quiet([H, G]))
        setp(H, "pause", False)
        must("the party is playing", until(lambda: get(H, "pause") is False and get(G, "pause") is False))
        own[0].kill(); own[0].wait(timeout=10)
        until(lambda: all(status(n).startswith("Lost the relay") for n in (H, G)))
        for n in (H, G):
            ok(f"{n} says it is reconnecting", status(n) == "Lost the relay. Reconnecting…", status(n))
            ok(f"{n} plays on, and its feed says so", until(lambda: "lost the relay, reconnecting" in feed(n))
               and get(n, "pause") is False, str(feed(n)[:3]))
        ok(f"{G} can't chat meanwhile", ui(G)["chat"]["away"] and "send is disabled" in refused(G, "send"))
        own[0] = launch_relay({"CREATE_KEY": KEY}, port)
        until(lambda: all(status(n).startswith("You're in") for n in (H, G)), 40)
        for n in (H, G):
            must(f"{n} is back in", status(n).startswith("You're in"), status(n))
        ok(f"{G} can chat again", not ui(G)["chat"]["away"])
        must("the returns are over", quiet([H, G]))
        ok("nobody stopped", all(get(n, "pause") is False for n in (H, G)), str([feed(n)[:2] for n in (H, G)]))

    @case("[G19b] an invite ticks the box, so a playlist party can't be missed by accident")
    def _():
        H, G = gui("g19bHost", key=KEY), gui("g19bGuest")
        fill(H, playlist=True)
        start(H, hostname=H, file=f"{folder}/e2.mkv", room="", secret="")
        must(f"{H} is in", joined(H), status(H))
        until(lambda: ui(H)["invite"])
        invite = ui(H)["invite"]
        ok("the invite asks for the playlist", invite.endswith(" -playlist"), invite)
        fill(G, invite=invite)
        ok(f"pasting it ticks {G}'s box", ui(G)["form"]["playlist"])
        start(G, hostname=G, file=f"{folder}/e1.mkv")
        must(f"{G} is in", joined(G), status(G))
        ok(f"{G} goes to {H}'s entry", until(lambda: get(G, "filename") == "e2.mkv"), get(G, "filename"))
        setp(H, "pause", False)
        must("they play", until(lambda: get(G, "pause") is False))
        ipc(nodes[H]["sock"], "playlist-next")
        top = lambda n: (feed_groups(n) or [[None, None, []]])[0]
        ok(f"{G}'s real notices about the move are with the file it moved to", until(lambda: top(G)[0] == "e3.mkv" and {f"from e2.mkv to e3.mkv, by {H}", f"paused for {H}: video changed"} <= set(top(G)[2])), str(feed_groups(G)))
        ok(f"so are {H}'s", until(lambda: top(H)[0] == "e3.mkv" and "paused for everyone: video changed" in top(H)[2]), str(feed_groups(H)))
        end(G)
        ok(f"{G}'s history has every file it was on", [v["name"] for v in state(G)["history"][-1].get("videos", [])] == ["e1.mkv", "e2.mkv", "e3.mkv"], str(state(G)["history"][-1]))
        fill(G, playlist=False, file=f"{folder}/e2.mkv")
        go(G)
        tick = "Diagnosis: the party plays a playlist of 3 videos, and you picked one file. Tick the box under File and start again."
        until(lambda: status(G) == tick, BOOT)
        ok("with the box unticked, it says to tick it", status(G) == tick, status(G))
        short = f"{RUN}/guishort"
        with making:
            shutil.copytree(folder, short)
            os.remove(f"{short}/e3.mkv")
        fill(G, playlist=True, file=f"{short}/e2.mkv")
        go(G)
        until(lambda: status(G) == NOT_THE_SAME_FOLDER, BOOT)
        ok("with another folder, it says the folders differ", status(G) == NOT_THE_SAME_FOLDER, status(G))
        fill(G, invite="-relay ws://example.invalid -room r -secret s")
        ok("an invite with no playlist unticks it", not ui(G)["form"]["playlist"])

    @case("[G19c] Continue: the same room again, each from where it stopped (videos are 12s), then the key holder's spot")
    def _():
        H, G = gui("g19cHost", key=KEY), gui("g19cGuest")
        for n in (H, G):
            fill(n, playlist=True)
        start(H, hostname=H, file=f"{folder}/e1.mkv", room="g19c", secret="g19csecret")
        must(f"{H} is in", joined(H), status(H))
        start(G, hostname=G, file=f"{folder}/e1.mkv", room="g19c", secret="g19csecret")
        must(f"{G} is in", joined(G), status(G))
        must("the join is over", quiet((H, G)))
        setp(H, "playlist-pos", 1)
        must(f"{G} followed to e2.mkv", until(lambda: get(G, "filename") == "e2.mkv"), get(G, "filename"))
        must("the move is over", quiet((H, G)))
        setp(H, "time-pos", 3)
        must(f"{G} followed to 3s", until(lambda: near(get(G, "time-pos"), 3, 0.2)), get(G, "time-pos"))
        end(G)
        setp(H, "time-pos", 6)
        # mpv is there before the node reads the script's seek; stopped in between, the node
        # records the 3s it knew (jitter seed 45678502851-1)
        must(f"{H} took its seek", until(lambda: any("broadcasted event" in l and '"at":6.000000' in l for l in log(H).splitlines())), log(H)[-300:])
        end(H)
        parties = len(state(H)["history"])
        ok("each remembers where it stopped", [state(n)["history"][-1].get("stopped") for n in (H, G)] == [{"entry": "e2.mkv", "at": 6}, {"entry": "e2.mkv", "at": 3}], str([state(n)["history"][-1].get("stopped") for n in (H, G)]))
        press_continue(G)
        ok(f"{G}, with no key, waits for the key holder to open the room", until(lambda: status(G).startswith("Waiting"), BOOT), status(G))
        tab(G, "history")
        why = refused(G, "history:0")
        ok("Continue can't be pressed during a party", "is disabled" in why, f"{why!r} running={ui(G)['running']} status={status(G)!r}")
        press_continue(H)
        must(f"{H} is back in", joined(H), status(H))
        ok(f"{H} opens on e2.mkv at 6s", until(lambda: get(H, "filename") == "e2.mkv" and near(get(H, "time-pos"), 6, 0.2)), f'{get(H, "filename")} {get(H, "time-pos")}')
        ok("the same party moved to the top, not a new one", len(state(H)["history"]) == parties and state(H)["history"][-1]["room"] == "g19c" and "ended" not in state(H)["history"][-1], str(state(H)["history"][-1]))
        must(f"{G} got in by itself", joined(G), status(G))
        ok(f"{G} takes the key holder's spot, not its own 3s", until(lambda: get(G, "filename") == "e2.mkv" and near(get(G, "time-pos"), 6, 0.2)), f'{get(G, "filename")} {get(G, "time-pos")}')
        must("the join is over", quiet((H, G)))
        ok(f"{H} stayed there", near(get(H, "time-pos"), 6, 0.2), get(H, "time-pos"))
        setp(H, "playlist-pos", 2)
        ok("the next file starts at its beginning", until(lambda: all(get(n, "filename") == "e3.mkv" and near(get(n, "time-pos"), 0, 1) for n in (H, G))), str([get(n, "time-pos") for n in (H, G)]))
        must("the move is over", quiet((H, G)))
        setp(H, "time-pos", 9)
        must(f"{G} followed to 9s", until(lambda: near(get(G, "time-pos"), 9, 0.2)), get(G, "time-pos"))
        setp(H, "pause", False)
        for n in (H, G):
            ok(f"{n}'s party ends with its last file", until(lambda: status(n) == "The party has ended.", BOOT), status(n))
        ok("both know it was played to the end", all(state(n)["history"][-1].get("finished") for n in (H, G)), str([state(n)["history"][-1] for n in (H, G)]))
        tab(H, "history")
        ok("so it has no Continue", ui(H)["history"][0]["button"] != "Continue", str(ui(H)["history"][0]))

    @case("[G19d] Continue a torrent: the same file of it, by the name mpv reported, at the same second")
    def _():
        H = gui("g19dHost", key=KEY)
        pack = f"{RUN}/guipack"
        os.makedirs(pack)
        for name in ("e1.mkv", "Part 2 #b.mkv"):
            shutil.copy(MEDIA, f"{pack}/{name}")
        start(H, hostname=H, file=seeder(pack), room="g19d", secret="g19dsecret")
        must(f"{H} is in", until(lambda: status(H).startswith("You're in"), 60) and joined(H), status(H))
        setp(H, "playlist-pos", 1)
        must("it moved to the second file", until(lambda: get(H, "filename") == "Part 2 #b.mkv"), get(H, "filename"))
        setp(H, "time-pos", 50)
        until(lambda: near(get(H, "time-pos"), 50, 0.2))
        end(H)
        press_continue(H)
        must(f"{H} is back in", until(lambda: status(H).startswith("You're in"), 60) and joined(H), status(H))
        ok("it opens that file at 50s", until(lambda: get(H, "filename") == "Part 2 #b.mkv" and near(get(H, "time-pos"), 50, 0.2)), f'{get(H, "filename")} {get(H, "time-pos")}')

    @case("[G19e] with keep-open=always mpv stays on a file it finished, and Continue starts the next one")
    def _():
        H = gui("g19eHost", key=KEY)
        save_settings(H, mpvFlags=f"{MPVFLAGS} --keep-open=always")
        fill(H, playlist=True)
        start(H, hostname=H, file=f"{folder}/e1.mkv", room="g19e", secret="g19esecret")
        must(f"{H} is in", joined(H), status(H))
        setp(H, "time-pos", 9)
        until(lambda: near(get(H, "time-pos"), 9, 0.2))
        setp(H, "pause", False)
        must("it plays to the end of e1.mkv and stays there", until(lambda: get(H, "pause") is True and (get(H, "time-pos") or 0) > 11 and get(H, "filename") == "e1.mkv"), f'{get(H, "filename")} {get(H, "time-pos")}')
        end(H)
        last = state(H)["history"][-1]
        ok("it continues from the next file, which nobody opened yet", last.get("stopped") == {"entry": "e2.mkv"} and not last.get("finished") and [v["name"] for v in last["videos"]] == ["e1.mkv"], str(last))
        press_continue(H)
        must(f"{H} is back in", joined(H), status(H))
        ok("and opens it at its start", until(lambda: get(H, "filename") == "e2.mkv" and near(get(H, "time-pos"), 0, 1)), f'{get(H, "filename")} {get(H, "time-pos")}')

    @case("[G20] a peer-to-peer party after one over the relay: the invite switches the guest to Peer-to-Peer")
    def _():
        # the guest closes its window for real at [G24]: a display of its own, so it is the only window
        H, G = gui("g20Host", key=KEY), gui("g20Guest", portal=False, display=xvfb())
        hport, gport = free_port(), free_port()
        # the party over the internet that [G23]'s history starts with
        start(H, hostname=H, file=MEDIA, room="g20", secret="g20secret")
        must(f"{H} is in over the relay", joined(H), status(H))
        start(G, hostname=G, file=MEDIA, room="g20", secret="g20secret")
        must(f"{G} is in over the relay", joined(G), status(G))
        # "You're in" is mpv being ready, which can come before the relay's handshake
        must(f"{G} met {H}", until(lambda: f"connected to {H}" in feed(G)), str(feed(G)))
        end(G)
        end(H)
        fill(H, mode="direct", addrs="", psecret="")
        start(H, port=str(hport))
        must(f"{H} is in", joined(H), status(H))
        until(lambda: "-addrs" in ui(H)["invite"])
        wifi = ui(H)["invite"]
        addr, wsecret = wifi.split()[1], wifi.split()[3]
        ok(f"{H}'s invite has its Wi-Fi address", wifi.startswith("-addrs ") and addr.endswith(f":{hport}"), wifi)

        part(f"[G20b] a stranger with no secret can't rewrite {H}'s invite or status through its log")
        before = status(H), state(H)["history"][-1]
        for forged in ("relay: disconnected", "relay: rejected by relay", "your address to share is -addrs 6.6.6.6:1 -secret FORGED"):
            code = raw(f"127.0.0.1:{hport}", "/hi", {"hit-me-up": forged, "secret": "wrong"})
            ok(f"{forged!r} is refused", code in (401, 403), f"HTTP {code}")
        ok("the window has read the forged line", until(lambda: "-secret FORGED" in chart(H)))
        ok("the invite is unchanged", ui(H)["invite"] == wifi, ui(H)["invite"])
        ok("the status is unchanged", status(H) == before[0], status(H))
        ok("the saved session is unchanged", state(H)["history"][-1] == before[1], str(state(H)["history"][-1]))

        fill(G, invite=wifi)
        f = ui(G)["form"]
        ok(f"{G} switched to Peer-to-Peer and filled it in", (f["mode"], f["addrs"], f["psecret"]) == ("direct", addr, wsecret), str(f))
        _, diff = variants()
        start(G, file=diff, port=str(gport))
        until(lambda: status(G) == NOT_THE_SAME_VIDEO, BOOT)
        ok("over Wi-Fi, another file gets the same diagnosis", status(G) == NOT_THE_SAME_VIDEO, status(G))
        start(G, file=MEDIA, port=str(gport))
        must(f"{G} is in", joined(G), status(G))
        until(lambda: f"{G} has joined" in feed(H) or f"connected to {G}" in feed(H))
        setp(H, "pause", False)
        until(lambda: get(G, "pause") is False and f"resumed by {H}" in feed(G))
        ok(f"{G} follows {H} over Wi-Fi", get(G, "pause") is False and f"resumed by {H}" in feed(G), str(feed(G)))

        part(f"[G21] quitting mpv ends {G}'s party without a diagnosis")
        ipc(nodes[G]["sock"], "quit")
        until(lambda: not ui(G)["running"])
        ok(f"{G} says it ended", status(G) == "The party has ended.", status(G))
        ok(f"{H}'s feed says {G} left", until(lambda: f"{G} has left" in feed(H)), str(feed(H)))

        part(f"[G22] SIGTERM ends {H} mid-party, mpv, window and all")
        sock = nodes[H]["sock"]
        nodes[H]["proc"].send_signal(signal.SIGTERM)
        ok(f"{H} exited", until(lambda: nodes[H]["proc"].poll() is not None, 20))
        ok("with exit code 0", nodes[H]["proc"].returncode == 0, str(nodes[H]["proc"].returncode))
        ok(f"{H}'s mpv closed", not listening_unix(sock))
        ok(f"{H} logged its window closing", "gui: window closed" in log(H), log(H)[-300:])

        part(f"[G23] {G}'s history: every party, Watch again peer-to-peer, Continue over the internet")
        tab(G, "history")
        rows = ui(G)["history"]
        ok("as many as the store has", len(rows) == len(state(G)["history"]), str(rows))
        ok("newest first, a peer-to-peer party with the host", "peer-to-peer" in rows[0]["line"] and f"with {H}" in rows[0]["line"], str(rows[0]))
        ok("the first is over the internet with the host", "over the internet" in rows[-1]["line"] and f"with {H}" in rows[-1]["line"], str(rows[-1]))
        ok("a peer-to-peer party has Watch again", rows[0]["button"] == "Watch again", str(rows[0]))
        ok("a party over the internet has Continue", rows[-1]["button"] == "Continue", str(rows[-1]))
        fill(G, addrs="", psecret="", mode="relay")
        run = ui(G)["run"]
        click(G, "history:0")
        f = ui(G)["form"]
        ok("Watch again fills Peer-to-Peer and shows the form, without starting", (f["mode"], f["addrs"], f["psecret"], ui(G)["tab"], ui(G)["run"]) == ("direct", addr, wsecret, "party", run), str(f))

        part(f"[G24] closing {G}'s window, as a window manager does, ends its GUI")
        x_close(G)
        ok(f"{G} exited", exited(G))
        ok("with exit code 0", nodes[G]["proc"].returncode == 0, str(nodes[G]["proc"].returncode))
        ok(f"{G} logged it", "gui: window closed" in log(G))

        part(f"[G25] {G} again, on the same folder: everything was kept")
        count = len(rows)
        G2 = launch_gui(G, tag=f"{G}2", portal=False)
        on_end(lambda: kill_gui(G2))
        f = ui(G2)["form"]
        ok("the name, relay and Wi-Fi port are filled in", (f["hostname"], f["relay"], f["port"], f["local"]) == (G, RELAY_URL, str(gport), True), str(f))
        s = ui(G2)["settings"]
        ok("the settings are kept", (s["mpvFlags"], s["relay"]) == (MPVFLAGS, RELAY_URL), str(s))
        ok("the history is kept", len(ui(G2)["history"]) == count, str(len(ui(G2)["history"])))

        part("[G26] Clear history asks, then empties it")
        tab(G2, "history")
        click(G2, "clear-history")
        d = ui(G2)["dialog"]
        ok("it asks first", d and d["title"] == "Clear the whole party history?", str(d))
        click(G2, "dialog:cancel")
        ok("Cancel keeps it", ui(G2)["dialog"] is None and len(ui(G2)["history"]) == count)
        click(G2, "clear-history")
        click(G2, "dialog:ok")
        ok("Clear history empties the list", ui(G2)["history"] == [] and ui(G2)["dialog"] is None)
        ok("and the store", not state(G2)["history"])

        part("[G27] the cat")
        click(G2, "cat")
        ok("petting it makes a heart", ui(G2)["hearts"] == 1)

    @case("[G28] self-update: offered, refused on a bad digest, installed on a good one")
    def _():
        # the stamped binaries ask this port for their release
        rel = Release(PORTS["release"], RUN + "/gui-v0.0.2", "watchparty-gui_" + subprocess.run(["go", "env", "GOARCH"], capture_output=True, text=True).stdout.strip())
        os.makedirs(f"{RUN}/upd")
        exe = f"{RUN}/upd/watchparty-gui"
        shutil.copy(RUN + "/gui-v0.0.1", exe)
        old = sha(exe)
        U = launch_gui("guiUpdate", exe=exe)
        on_end(lambda: kill_gui(U))
        about = lambda: ui(U)["about"]
        until(lambda: about()["button"])
        ok("it offers v0.0.2", (about()["version"], about()["button"], about()["dot"]) == ("Version v0.0.1", "Update to v0.0.2", True), str(about()))
        tab(U, "settings")
        click(U, "update")
        until(lambda: about()["version"].startswith("Update failed"))
        ok("a bad digest is refused", about()["version"] == "Update failed: the download does not match its published sha256, not installing it.", about()["version"])
        ok("and the binary is untouched", sha(exe) == old)
        ok("and no download is left behind", os.listdir(f"{RUN}/upd") == ["watchparty-gui"], str(os.listdir(f"{RUN}/upd")))
        rel.digest = "sha256:" + rel.sha
        click(U, "update")
        until(lambda: ui(U)["dialog"])
        ok("a good one is installed", sha(exe) == rel.sha)
        d = ui(U)["dialog"]
        ok("and it asks to restart", (d["title"], d["text"], d["cancel"], d["ok"]) == ("Update installed", "Version v0.0.2 is installed. A restart is required to start using it.", "Restart later", "Restart now"), str(d))
        click(U, "dialog:cancel")
        ok("Restart later closes the dialog and leaves the button", ui(U)["dialog"] is None and about()["button"] == "Restart now", str(about()))

        part("[G29] Restart now hands over to the new version")
        click(U, "restart")
        ok("the old one exited", until(lambda: nodes[U]["proc"].poll() is not None))
        attach(U)
        ok("the new window runs v0.0.2 and offers nothing", until(lambda: (about()["version"], about()["button"]) == ("Version v0.0.2", "")), str(about()))

    @case("[G29b] the cli updates itself the same way, with its own asset")
    def _():
        rel = Release(PORTS["cliRelease"], RUN + "/cli-v0.0.2", "watchparty_" + subprocess.run(["go", "env", "GOARCH"], capture_output=True, text=True).stdout.strip())
        os.makedirs(f"{RUN}/updcli")
        exe = f"{RUN}/updcli/watchparty"
        shutil.copy(RUN + "/cli-v0.0.1", exe)
        old = sha(exe)
        res = subprocess.run([exe, "-update"], capture_output=True, text=True, timeout=BOOT)
        ok("a bad digest is refused", res.returncode != 0 and "does not match its published sha256" in res.stderr, res.stderr)
        ok("and the binary is untouched", sha(exe) == old)
        ok("and no download is left behind", os.listdir(f"{RUN}/updcli") == ["watchparty"], str(os.listdir(f"{RUN}/updcli")))
        rel.digest = "sha256:" + rel.sha
        res = subprocess.run([exe, "-update"], capture_output=True, text=True, timeout=BOOT)
        ok("a good one is installed", res.returncode == 0 and sha(exe) == rel.sha, res.stderr)
        res = subprocess.run([exe, "-version"], capture_output=True, text=True, timeout=BOOT)
        ok("and it runs as v0.0.2", res.stdout.strip() == "v0.0.2", res.stdout)
        res = subprocess.run([exe, "-update"], capture_output=True, text=True, timeout=BOOT)
        ok("updating again says it's up to date, and isn't a failure", res.returncode == 0 and "already on the latest version" in res.stdout, res.stdout + res.stderr)

    @case("[G37] the window follows the desktop's reduced motion and dark appearance as they change", serial=True)
    def _():
        H = gui("g37")
        ok("it animates with nothing set", not ui(H)["still"])
        portal_set("org.gnome.desktop.interface", "enable-animations", False)
        ok("animations turned off rest it", until(lambda: ui(H)["still"], 5))
        portal_set("org.gnome.desktop.interface", "enable-animations", True)
        ok("and on again move it", until(lambda: not ui(H)["still"], 5))
        portal_set("org.kde.kdeglobals.KDE", "AnimationDurationFactor", 0.0)
        ok("so does KDE's animation speed at 0", until(lambda: ui(H)["still"], 5))
        portal_set("org.kde.kdeglobals.KDE", "AnimationDurationFactor", 1.0)
        ok("and back", until(lambda: not ui(H)["still"], 5))
        G = gui("g37Later")
        portal_set("org.kde.kdeglobals.KDE", "AnimationDurationFactor", 0.0)
        G2 = gui("g37Started")
        ok("a window opened while it is set starts still", until(lambda: ui(G2)["still"] and ui(G)["still"], 5))
        portal_set("org.kde.kdeglobals.KDE", "AnimationDurationFactor", 1.0)
        ok("and moves once it's unset", until(lambda: not ui(G2)["still"], 5))
        ok("it is light with no color scheme set", not ui(H)["dark"])
        portal_set("org.freedesktop.appearance", "color-scheme", 1)
        ok("prefer dark turns it dark", until(lambda: ui(H)["dark"], 5))
        ok("and leaves its motion alone", not ui(H)["still"])
        G3 = gui("g37Dark")
        ok("a window opened while it is set starts dark", until(lambda: ui(G3)["dark"], 5))
        portal_set("org.freedesktop.appearance", "color-scheme", 2)
        ok("prefer light turns it light again", until(lambda: not ui(H)["dark"] and not ui(G3)["dark"], 5))

    @case("[G33] install.sh: the right asset, the app menu entry, the macOS bundle, and no mess on a failure")
    def _():
        install_suite()

    run_cases()


def install_suite():
    stubs = f"{RUN}/installer"
    os.makedirs(stubs)
    script(stubs + "/uname", 'case $1 in -m) echo "$FAKE_M" ;; *) echo "$FAKE_S" ;; esac')
    script(stubs + "/curl", 'echo "$5" >> "$HOME/urls"\n[ -z "$CURL_FAIL" ] || { echo partial > "$4"; exit 22; }\necho the-binary > "$4"')

    def install(home, s, m, src=REPO + "/install.sh", **env):
        os.makedirs(home, exist_ok=True)
        return subprocess.run(["sh", src], capture_output=True, text=True, timeout=SETTLE,
                              env={"HOME": home, "PATH": f"{stubs}:/usr/bin:/bin", "FAKE_S": s, "FAKE_M": m, **env})

    def asked(home):
        return open(home + "/urls").read().split() if os.path.exists(home + "/urls") else []

    home = f"{RUN}/install linux"  # a space, as a real home may have
    res = install(home, "Linux", "x86_64")
    ok("linux installs", res.returncode == 0, res.stderr)
    ok("it asks for the latest gui", asked(home) == ["https://github.com/mhashemm/watchparty/releases/latest/download/watchparty-gui_amd64"], str(asked(home)))
    exe = home + "/.local/bin/watchparty-gui"
    ok("the binary is executable, with nothing left beside it", os.access(exe, os.X_OK) and os.listdir(home + "/.local/bin") == ["watchparty-gui"], str(os.listdir(home + "/.local/bin")))
    entry = open(home + "/.local/share/applications/watchparty.desktop").read()
    ok("the app menu entry runs it, quoted", f'Exec="{exe}"' in entry and "Icon=watchparty" in entry, entry)
    ok("and has its icon", os.path.exists(home + "/.local/share/icons/hicolor/scalable/apps/watchparty.svg"))

    res = install(home, "Linux", "x86_64", CURL_FAIL="1")
    ok("a failed download fails the install", res.returncode != 0)
    ok("keeps the installed binary and leaves no partial file", open(exe).read() == "the-binary\n" and os.listdir(home + "/.local/bin") == ["watchparty-gui"], str(os.listdir(home + "/.local/bin")))

    home = f"{RUN}/install-arm"
    res = install(home, "Linux", "aarch64", XDG_BIN_HOME=home + "/b", XDG_DATA_HOME=home + "/d")
    ok("arm64 gets its asset, in the XDG folders", res.returncode == 0 and asked(home)[0].endswith("/watchparty-gui_arm64") and os.path.exists(home + "/b/watchparty-gui") and os.path.exists(home + "/d/applications/watchparty.desktop"), res.stderr)

    home = f"{RUN}/install-mac"
    # Homebrew's prefix moved into the run, with brew and the three tools already in it
    brew = home + "/homebrew"
    os.makedirs(brew + "/bin")
    for tool in ("brew", "mpv", "ffmpeg", "yt-dlp"):
        script(f"{brew}/bin/{tool}", "exit 0")
    with open(REPO + "/install.sh") as f, open(f"{RUN}/install-mac.sh", "w") as out:
        out.write(f.read().replace("/opt/homebrew", brew))
    res = install(home, "Darwin", "arm64", src=f"{RUN}/install-mac.sh")
    ok("macOS installs", res.returncode == 0, res.stderr)
    ok("it asks for the mac asset", asked(home)[0].endswith("/watchparty-gui_mac_arm64"), str(asked(home)))
    contents = home + "/Applications/Watchparty.app/Contents"
    with open(contents + "/Info.plist", "rb") as f:
        plist = plistlib.load(f)
    ok("the bundle's plist names the binary inside it", os.access(f"{contents}/MacOS/{plist['CFBundleExecutable']}", os.X_OK), str(plist))
    ok("and can find Homebrew's mpv from Finder", brew + "/bin" in plist["LSEnvironment"]["PATH"], str(plist))

    for s, m, why in (("Linux", "armv7l", "an architecture with no build"), ("FreeBSD", "amd64", "an OS with no build")):
        home = f"{RUN}/install-{m}"
        res = install(home, s, m)
        ok(f"{why} stops before downloading", res.returncode != 0 and not asked(home) and res.stderr, res.stderr)


def listening_unix(path):
    s = socket.socket(socket.AF_UNIX)
    try:
        s.connect(path)
        return True
    except OSError:
        return False
    finally:
        s.close()
