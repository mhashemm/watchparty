"""The gui phase: real GUIs, each in headless Chromium, driven through the page."""
from e2e_core import *
import e2e_core


def gui_suite():
    KEY = "e2ekey"
    print("\n=== gui: real GUIs, each in headless Chromium, and GUIs of its own for every case ===")
    e2e_core.BUS = private_bus()
    launch_relay({"CREATE_KEY": KEY})
    until(lambda: listening(PORTS["relay"]))
    # the playlist the check-mark cases open, each in a room of its own
    folder = videos_in(f"{RUN}/guifolder", ("e1.mkv", "e2.mkv", "e3.mkv"))

    def mpv_of(n):
        return child_mpv_socket(nodes[n]["proc"].pid)

    def read_all(n):
        """The page has handled every log line its GUI has, so the feed isn't half a burst."""
        return js(n, "api(`/log?from=${from}&run=${run}`).then(r => r.json()).then(j => j.run == run && !j.lines.length)")

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
        must(f"{n} saved its settings", save_settings(n, **fields) == "Saved.", js(n, '$("settings-saved").textContent'))
        return n

    def chats(n):
        """The chat under the box, newest first, as "who: text", "ours " before this node's own."""
        return js(n, '[...document.querySelectorAll("#said-latest li, #said-rest li")]'
                     '.map(li => (li.classList.contains("ours") ? "ours " : "") + li.children[1].textContent + ": " + (li.children[2].getAttribute("aria-label") || li.children[2].textContent))')

    def press_continue(n):
        tab(n, "history")
        run = js(n, "run")
        click(n, "#history-list li .continue")
        until(lambda: js(n, "run") > run, BOOT)

    @case("[G1] the GUI opens its own app window at the phone-sized window")
    def _():
        H = gui("g1")
        argv = launches(H)[0]
        ok(f"it opened {nodes[H]['url']} as an app", f"--app={nodes[H]['url']} " in argv, argv)
        ok("it asked for 560x940", "--window-size=560,940" in argv)
        until(lambda: js(H, '$("version-text").textContent') != "Checking the version…")
        ok("a dev build says so and offers no update", js(H, '$("version-text").textContent == "Development build" && $("update").hidden'))

        part("[G1b] the window asks for no Google keys, and doesn't invite selecting or inspecting")
        ok("the browser is told there are no keys", open(f"{nodes[H]['home']}/browser.key").read().strip() == "no")
        ok("text can't be selected", js(H, 'getComputedStyle(document.body).userSelect') == "none")
        ok("but the invite can", js(H, 'getComputedStyle($("share-text")).userSelect') == "text")
        prevented = lambda target, event: js(H, f'!{target}.dispatchEvent(new {event})')
        ok("no right-click menu on the page", prevented("document.body", 'MouseEvent("contextmenu", {bubbles: true, cancelable: true})'))
        ok("but there is in a field, to paste", not prevented('$("invite")', 'MouseEvent("contextmenu", {bubbles: true, cancelable: true})'))
        for name, keys in (("F12", 'key: "F12"'), ("Ctrl+Shift+I", 'key: "I", ctrlKey: true, shiftKey: true'), ("Cmd+Alt+J", 'key: "j", metaKey: true, altKey: true'), ("Ctrl+U", 'key: "u", ctrlKey: true')):
            ok(f"{name} is blocked", prevented("document.body", f'KeyboardEvent("keydown", {{{keys}, bubbles: true, cancelable: true}})'))
        ok("Ctrl+V isn't", not prevented('$("invite")', 'KeyboardEvent("keydown", {key: "v", ctrlKey: true, bubbles: true, cancelable: true})'))
        js(H, "window.notReloaded = true")
        for key, code, vk, mods in (("F5", "F5", 116, 0), ("r", "KeyR", 82, 2), ("r", "KeyR", 82, 4), ("R", "KeyR", 82, 10)):
            for kind in ("rawKeyDown", "keyUp"):
                nodes[H]["page"].call("Input.dispatchKeyEvent", type=kind, key=key, code=code, windowsVirtualKeyCode=vk, nativeVirtualKeyCode=vk, modifiers=mods)
        hold(QUIET, "a reload would land right behind the keys")
        ok("F5, Ctrl+R, Cmd+R and Ctrl+Shift+R, pressed for real, don't reload it", js(H, "window.notReloaded") is True)

    @case("[G2] the tabs switch views and the address remembers which")
    def _():
        H = gui("g2")
        for view in ("history", "settings", "party"):
            tab(H, view)
            shown = js(H, '["party", "history", "settings"].filter(v => !$("view-" + v).hidden)')
            ok(f"{view} tab shows only its view", shown == [view], str(shown))
            ok(f"{view} tab sets the address", js(H, "location.hash") == ("" if view == "party" else "#" + view))

    @case("[G3] Settings saves, and fills the party's relay")
    def _():
        H = launch_gui("g3")
        on_end(lambda: kill_gui(H))
        ok("it saved", save_settings(H, **{"set-relay": RELAY_URL, "createKey": KEY, "mpvFlags": MPVFLAGS}) == "Saved.")
        s = state(H)["settings"]
        ok("the settings reached the store", (s["relay"], s["createKey"], s["mpvFlags"]) == (RELAY_URL, KEY, MPVFLAGS), str(s))
        ok("the party's relay was filled in", js(H, '$("relay").value') == RELAY_URL)
        store = js(H, '$("store-path").textContent')
        ok("the store is where the page says", store == f"{nodes[H]['home']}/config/watchparty", store)
        ok("the settings are 0600 in a 0700 folder", (os.stat(f"{store}/gui.json").st_mode & 0o777, os.stat(store).st_mode & 0o777) == (0o600, 0o700))
        ok("the mpv path shows the one it will use", js(H, '$("mpv").placeholder') == f"{nodes[H]['home']}/bin/mpv" and js(H, 'document.querySelector("label[for=mpv]").textContent') == "mpv path", js(H, '$("mpv").placeholder'))
        ok("the cache folder shows the one it will use", js(H, '$("cacheDir").placeholder') == f"{nodes[H]['home']}/.cache/watchparty", js(H, '$("cacheDir").placeholder'))

    @case("[G3b] the ? button opens the help, one topic at a time")
    def _():
        H = gui("g3b")
        ok("it starts closed", js(H, '$("tip").hidden'))
        click(H, "#tip-button")
        ok("a click opens it", js(H, '!$("tip").hidden && $("tip-button").getAttribute("aria-expanded") == "true"'))
        sections = js(H, '[...$("tip").querySelectorAll("h2")].map(h => h.textContent)')
        ok("each topic has its own section", sections == ["Start a party", "Join a party", "Watch together", "Automatic pauses", "Who's in the party", "Chat", "Reactions", "Slow networks", "Links", "Playlists", "Torrents", "Connecting"], str(sections))
        tip = js(H, '$("tip").textContent')
        ok("it isn't tied to one kind of video", not COUPLED.search(tip), str(COUPLED.findall(tip)))
        click(H, "#tip h2")
        ok("a click inside keeps it open", js(H, '!$("tip").hidden'))
        click(H, "#tip-button")
        ok("the button closes it", js(H, '$("tip").hidden'))
        click(H, "#tip-button")
        js(H, 'document.dispatchEvent(new KeyboardEvent("keydown", {key: "Escape"}))')
        ok("Escape closes it", js(H, '$("tip").hidden'))
        click(H, "#tip-button")
        click(H, "h1")
        ok("a click elsewhere closes it", js(H, '$("tip").hidden'))

    @case("[G4] Browse uses the desktop portal, and says so when there is none")
    def _():
        H, G = gui("g4"), gui("g4NoPortal", portal=False)
        click(H, "[data-pick=file]")
        ok(f"{H} picked the video through the portal", until(lambda: js(H, '$("file").value') == MEDIA), js(H, '$("file").value'))
        click(H, "[data-pick=cacheDir]")
        ok(f"{H} picked a folder through it too", until(lambda: js(H, '$("cacheDir").value') == os.path.dirname(MEDIA)), js(H, '$("cacheDir").value'))
        js(H, '$("cacheDir").value = ""')
        click(H, "[data-pick=mpv]")
        ok(f"{H} can pick mpv, which no video filter would show", until(lambda: js(H, '$("mpv").value') == MEDIA + ".unfiltered"), js(H, '$("mpv").value'))
        click(G, "[data-pick=file]")
        until(lambda: js(G, '!$("error").hidden'))
        ok(f"{G}, with no portal, is told to type the path", js(G, '$("error").textContent').startswith("No file picker found"), js(G, '$("error").textContent'))

    @case("[G5] the form refuses what can't start")
    def _():
        H = gui("g5")
        fill(H, file="   ")
        click(H, "#go")
        ok("a blank path asks for a video", js(H, '$("error").textContent') == "Pick a video first.")
        fill(H, file=MEDIA, relay="")
        click(H, "#go")
        until(lambda: js(H, '$("error").textContent').startswith("Diagnosis"))
        ok("relay mode with no relay is refused", js(H, '$("error").textContent') == "Diagnosis: enter the relay address, or paste an invite.", js(H, '$("error").textContent'))
        ok("and nothing started", js(H, '$("vitals").hidden'))

    @case("[G6] the host starts a relay party from the form and gets an invite")
    def _():
        H, G = gui("g6Host", key=KEY), gui("g6Guest")
        save_settings(G, mpv=f"{nodes[G]['home']}/slow-mpv")
        start(H, hostname=H, relay=RELAY_URL, file=MEDIA)
        must(f"{H} is in", joined(H), status(H))
        until(lambda: js(H, '$("share-text").textContent') != "")
        invite = js(H, '$("share-text").textContent')
        room, secret = invite.split("-room ", 1)[1].split()[0], invite.split("-secret ", 1)[1].split()[0]
        ok(f"{H} shows an invite", invite == f"-relay {RELAY_URL} -room {room} -secret {secret}", invite)
        ok(f"{H} hides the form and shows the invite box", js(H, '$("fields").hidden && !$("share").hidden'))
        ok("Update and Restart are off during a party", js(H, '$("update").disabled && $("restart").disabled && $("restart-now").disabled'))

        part("[G7] Copy invite puts it on the clipboard")
        click(H, "#copy")
        until(lambda: js(H, '$("copy").textContent') != "Copy invite")
        ok("the button says Copied", js(H, '$("copy").textContent') == "Copied", js(H, '$("copy").textContent'))
        ok("the clipboard has the invite", js(H, "navigator.clipboard.readText()") == invite)
        ok("the button comes back", until(lambda: js(H, '$("copy").textContent') == "Copy invite", 5))

        part("[G8] Save chart downloads the whole log")
        click(H, "#save-chart")
        dl = f"{nodes[H]['home']}/downloads"
        until(lambda: any(f.endswith(".log") for f in os.listdir(dl)))
        saved = [f for f in os.listdir(dl) if f.endswith(".log")]
        ok("one watchparty-<date>.log", len(saved) == 1 and saved[0].startswith("watchparty-"), str(saved))
        chart = open(f"{dl}/{saved[0]}").read() if saved else ""
        ok("it holds the party's log", "init done" in chart and invite in chart)
        ok("the page says where it went", saved and js(H, '$("chart-saved").textContent') == f"Saved as {saved[0]} in your downloads folder.", js(H, '$("chart-saved").textContent'))

        part("[G9] the guest pastes the whole invite line and joins, after mpv takes its time")
        js(G, f'$("invite").value = {json.dumps("2026/11/01 12:00:00 your address to share is " + invite)}; '
              '$("invite").dispatchEvent(new Event("input"))')
        ok(f"{G} filled relay, room and secret from it", js(G, f'$("relay").value == {json.dumps(RELAY_URL)} && $("room").value == {json.dumps(room)} '
                                                          f'&& $("secret").value == {json.dumps(secret)} && !$("invite-hint").hidden'))
        start(G, hostname=G, file=MEDIA)
        ok(f"{G} said it is opening mpv", until(lambda: status(G).startswith("Opening mpv"), BOOT), status(G))
        ok(f"{G} walks while it waits", js(G, 'document.body.classList.contains("walking")'))
        must(f"{G} is in", joined(G), status(G))
        until(lambda: f"{G} has joined" in feed(H) and f"connected to {H}" in feed(G))
        ok(f"{H}'s feed says {G} joined", f"{G} has joined" in feed(H), str(feed(H)))
        ok(f"{G}'s feed says it connected", f"connected to {H}" in feed(G), str(feed(G)))
        members = lambda n: js(n, '[...$("member-list").children].map(li => li.querySelector(".name").textContent + " " + li.querySelector(".since").textContent)')
        ok(f"{H}'s members name {G} since it joined", [m.split(" since ")[0] for m in members(H)] == ["You", G] and js(H, '$("member-count").textContent') == "2", str(members(H)))
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
        # "paused by you" comes after it, so the feed is checked once the page has read them all
        until(lambda: get(H, "pause") is True and f"paused for {G}: seek" in feed(H) and "paused for everyone: seek" in feed(G)
              and log(G).count('"name":"pause-reason"') > reasons and read_all(G))
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

        part(f"[G13b] {G} says something: both feeds show it, and nothing keeps it")
        said = "g13b-chat popcorn?"
        # every file under a GUI's folder, its browser profile too, and its log, which a desktop can keep
        kept = lambda: [f for n in (H, G) for f in [nodes[n]["log"], *glob.glob(f"{nodes[n]['home']}/**", recursive=True)]
                        if os.path.isfile(f) and not os.path.islink(f) and said.encode() in open(f, "rb").read()]
        js(G, '$("chat-text").value = "😀".repeat(205); $("chat-text").dispatchEvent(new Event("input"))')
        ok("the box takes 200 characters, an emoji as one", js(G, '[...$("chat-text").value].length') == 200)
        fill(G, **{"chat-text": said})
        click(G, "#chat-send")
        ok(f"{H} has it under {G}'s name", until(lambda: chats(H)[:1] == [f"{G}: {said}"]), str(chats(H)))
        ok(f"{G} has it as its own, and the box emptied", until(lambda: chats(G)[:1] == [f"ours you: {said}"] and js(G, '$("chat-text").value') == ""), str(chats(G)))
        ok("it isn't in the feed", not any(said in line for n in (H, G) for line in feed(n)))
        ok("no Patient chart has it", not any(js(n, f'$("log").textContent.includes({json.dumps(said)})') for n in (H, G)))
        ok("no file has it", kept() == [], str(kept()))
        for text in ("h1", "h2", "h3"):
            fill(H, **{"chat-text": text})
            click(H, "#chat-send")
            must(f"{H}'s {text} reached {G}", until(lambda: chats(G)[:1] == [f"{H}: {text}"]))
        ok(f"{G} shows the latest three, the first under All messages, 4",
           js(G, '[$("said-latest").children.length, $("said-more").hidden, $("said-count").textContent, $("said-rest").textContent.includes("popcorn")]') == [3, False, "4", True])
        click(H, "#react-open")
        ok("the row says it has more: an arrow and a fade at its end, none at its start",
           js(H, '!$("react-more").hidden && $("react-back").hidden && $("react-strip").className == "react-strip more-on"'),
           js(H, '[$("react-back").hidden, $("react-strip").className, $("react-strip").scrollLeft]'))
        click(H, "#react-more")
        ok("the arrow scrolls it, and then its start has them too",
           until(lambda: js(H, '$("react-strip").scrollLeft > 100 && !$("react-back").hidden && $("react-strip").classList.contains("more-back")'), 5))
        click(H, "#react-back")
        must("the other scrolls it back", until(lambda: js(H, '$("react-strip").scrollLeft == 0'), 5))
        # the emoji the end cuts
        js(H, '[...$("react-strip").children].find(b => b.getBoundingClientRect().right > $("react-strip").getBoundingClientRect().right).click()')
        ok("the emoji the end cuts scrolls it as its arrow does, and isn't picked",
           until(lambda: js(H, '$("react-strip").scrollLeft > 100'), 5))
        click(H, "#react-back")
        must("it scrolls back", until(lambda: js(H, '$("react-strip").scrollLeft == 0'), 5))
        click(H, "#react-more")
        must("scrolled again", until(lambda: js(H, '$("react-strip").scrollLeft > 100'), 5))
        click(H, "#react-open")
        must("the smiley closes the row", js(H, '$("react-strip").hidden'))
        click(H, "#react-open")
        ok("opened again, the row is where it was left",
           until(lambda: js(H, '$("react-strip").scrollLeft > 100 && !$("react-back").hidden'), 5), js(H, '$("react-strip").scrollLeft'))
        # a real Tab from Send, the last before the row, as a keyboard reaches it
        at = js(H, '$("react-strip").scrollLeft')
        js(H, '$("chat-send").focus()')
        for t in ("rawKeyDown", "keyUp"):
            nodes[H]["page"].call("Input.dispatchKeyEvent", type=t, key="Tab", code="Tab", windowsVirtualKeyCode=9)
        ok("Tab lands on its first emoji in view, and the row stays where it was",
           until(lambda: js(H, f'(() => {{ const s = $("react-strip"), b = document.activeElement, r = s.getBoundingClientRect(), a = b.getBoundingClientRect(); '
                               f'return s.contains(b) && a.left >= r.left - 0.5 && a.left < r.left + 80 && Math.abs(s.scrollLeft - {at}) < 1 }})()'), 5),
           js(H, '[document.activeElement.dataset.code, $("react-strip").scrollLeft]'))
        # and Shift+Tab from All messages, the first after the row
        js(H, '$("said-more").querySelector("summary").focus()')
        for t in ("rawKeyDown", "keyUp"):
            nodes[H]["page"].call("Input.dispatchKeyEvent", type=t, key="Tab", code="Tab", windowsVirtualKeyCode=9, modifiers=8)
        ok("Shift+Tab lands on its last emoji in view, and the row stays where it was",
           until(lambda: js(H, f'(() => {{ const s = $("react-strip"), b = document.activeElement, r = s.getBoundingClientRect(), a = b.getBoundingClientRect(); '
                               f'return s.contains(b) && a.right <= r.right + 0.5 && a.right > r.right - 80 && Math.abs(s.scrollLeft - {at}) < 1 }})()'), 5),
           js(H, '[document.activeElement.dataset.code, $("react-strip").scrollLeft]'))
        click(H, "#react-back")
        must("and scrolls back", until(lambda: js(H, '$("react-strip").scrollLeft == 0'), 5))

        def settled():
            """Where the row's scroll comes to rest: the same for 8 reads in a row, ~0.4s."""
            seen = []
            until(lambda: seen.append(js(H, '$("react-strip").scrollLeft')) or len(seen) >= 8 and len(set(seen[-8:])) == 1, 10)
            return seen[-1]
        click(H, "#react-more")
        lands = settled()
        click(H, "#react-back")
        must("and back once more", settled() == 0)
        # a mouse press on an emoji as an arrow scrolls the row, released away so nothing is picked:
        # the focus handler puts the row back where it is, which must not stop the scroll
        r = js(H, '(() => { const b = $("react-strip").children[1].getBoundingClientRect(); return [b.x + b.width / 2, b.y + b.height / 2] })()')
        js(H, '$("react-more").click()')
        must("the arrow is scrolling it", until(lambda: 20 < js(H, '$("react-strip").scrollLeft') < lands - 60, 2))
        nodes[H]["page"].call("Input.dispatchMouseEvent", type="mousePressed", x=r[0], y=r[1], button="left", clickCount=1)
        nodes[H]["page"].call("Input.dispatchMouseEvent", type="mouseReleased", x=5, y=5, button="left", clickCount=1)
        must("the press focused an emoji", js(H, '$("react-strip").contains(document.activeElement)'), js(H, 'document.activeElement.outerHTML.slice(0, 80)'))
        ok("a click's focus doesn't stop the arrow's scroll", settled() == lands, str([js(H, '$("react-strip").scrollLeft'), lands]))
        click(H, "#react-back")
        must("and back", settled() == 0)
        js(H, '$("react-more").click(); $("react-open").click()')
        must("the row closes as its arrow starts scrolling it", js(H, '$("react-strip").hidden'))
        click(H, "#react-open")
        ok("it opens where the arrow was taking it", settled() == lands, str([js(H, '$("react-strip").scrollLeft'), lands]))
        click(H, "#react-more")
        must("the arrow scrolls on", settled() > lands)
        js(H, '$("react-strip").scrollLeft = 0')  # as a finger drags it back
        must("dragged back to its start", settled() == 0)
        click(H, "#react-open")
        click(H, "#react-open")
        ok("an arrow's scroll that ended is forgotten: it opens where it was dragged", settled() == 0, str(js(H, '$("react-strip").scrollLeft')))
        click(H, '#react-strip button[data-code="1f602"]')
        ok(f"{G} has {H}'s reaction", until(lambda: chats(G)[:1] == [f"{H}: 😂"]), str(chats(G)))
        ok(f"{H} has it as its own, the row closed and the button rests",
           until(lambda: chats(H)[:1] == ["ours you: 😂"]) and js(H, '$("react-strip").hidden && $("react-open").disabled'), str(chats(H)))
        ok("no log has it", not any(b"watchparty-react" in open(nodes[n]["log"], "rb").read() for n in (H, G)))

        part(f"[G14] {H}'s history has the real room and secret, and who came")
        store = js(H, '$("store-path").textContent')
        last = state(H)["history"][-1]
        ok("room and secret from the invite", (last.get("room"), last.get("secret")) == (room, secret), str(last))
        ok(f"{G} is in its people", last.get("people") == [G], str(last.get("people")))
        ok("the history and its secret are 0600 in their own file", os.stat(f"{store}/history.json").st_mode & 0o777 == 0o600 and secret in open(f"{store}/history.json").read())
        ok("the settings file has no party in it", secret not in open(f"{store}/gui.json").read() and "history" not in open(f"{store}/gui.json").read())

        part(f"[G15] {G} ends the party from the page, which pauses {H}")
        setp(H, "pause", False)
        must("the party is playing", until(lambda: get(G, "pause") is False and feed(H)[:1] == ["resumed by you"]))
        asked = end(G)
        ok("it asked first", asked == "End the party? mpv closes for you, and everyone else keeps watching.", str(asked))
        ok(f"{G} shows the form again", js(G, '!$("fields").hidden && $("go-label").textContent == "Start the party"'))
        ok(f"{G}'s mpv closed", until(lambda: not mpv_of(G)))
        ok(f"{H}'s feed says {G} left", until(lambda: f"{G} has left" in feed(H)), str(feed(H)))
        ok(f"{H} paused and says why", until(lambda: get(H, "pause") is True and f"paused for {G}: left" in feed(H)), str(feed(H)))
        ok(f"{H}'s feed doesn't blame {H}", feed(H)[0] != "paused by you", str(feed(H)))
        ok("Update and Restart are back on", js(G, '!$("update").disabled && !$("restart").disabled'))
        ok(f"{G}'s chat went with its party", chats(G) == [] and js(G, '$("said-more").hidden'), str(chats(G)))
        js(G, '$("chat-text").value = "too late"; $("chat-form").requestSubmit()')
        ok("a message sent after the end stays in the box, and says why",
           until(lambda: js(G, '[$("chat-text").value, $("chat-hint").textContent]') == ["too late", "Not sent: the party has ended."]), js(G, '$("chat-hint").textContent'))
        js(G, '$("chat-text").value = ""; $("chat-text").dispatchEvent(new Event("input"))')
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
        ok("in red, with the chart open", js(G, '$("status").classList.contains("bad") && $("chart").open'))

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
            rows = js(G, '$("compare").hidden ? "" : $("compare").textContent')
            ok("and it shows what each plays", os.path.basename(MEDIA) in rows and os.path.basename(diff) in rows, rows)
            ok(f"{G}'s party ended", until(lambda: js(G, '!$("fields").hidden')))

    @case("[G17b] opening a new party without the create key is diagnosed, not left waiting")
    def _():
        G = gui("g17b")
        start(G, hostname=G, file=MEDIA, room="", secret="")
        until(lambda: status(G).startswith("Diagnosis"), BOOT)
        ok("it asks for the create key", status(G) == "Diagnosis: this relay needs a create key to open a new party. Add it in Settings and start again.", status(G))
        ok(f"{G}'s party ended", until(lambda: js(G, '!$("fields").hidden')))

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
        # the trail shows once the step has taken half a second
        until(lambda: js(G, '!$("trail").hidden'), BOOT)
        trail = js(G, '[...$("trail").children].map(s => (s.classList.contains("now-step") ? "*" : "") + s.textContent)')
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
        ok("history shows its name", js(G, 'fileName($("file").value)') == "The.Video", js(G, 'fileName($("file").value)'))

    @case("[G17e] the check mark loads the file's folder, and starts on that file")
    def _():
        G = gui("g17e")
        js(G, '$("playlist").checked = true')
        start(G, hostname=G, file=f"{folder}/e2.mkv", room="g17e", secret="g17esecret")
        until(lambda: mpv_of(G), BOOT)
        nodes[G]["sock"] = mpv_of(G)
        ok("it opens the picked file", until(lambda: get(G, "filename") == "e2.mkv"), get(G, "filename"))
        ok("with its neighbours", get(G, "playlist-count") == 3, get(G, "playlist-count"))
        ok("history remembers it", state(G)["history"][-1].get("playlist") is True, str(state(G)["history"][-1]))
        groups = lambda: nodes[G]["page"].feed_groups()
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
        until(lambda: js(G, '$("history-list").querySelector(".videos") !== null'))
        shown = js(G, '$("history-list").firstElementChild.textContent')
        ok("the History page names the folder by its full path, counts files rather than plays, and shows each with its times", f"{folder} (2 videos)" in shown and "e3.mkv" in shown and " to " in shown, shown)
        title = lambda file, playlist=True: js(G, f'partyTitle({{file: {json.dumps(file)}, playlist: {json.dumps(playlist)}}}, 2)')
        ok("a torrent has no folder of its own, so it's named for itself", (title("magnet:?xt=urn:btih:x&dn=The.Pack"), title("/d/pack.torrent")) == ("The.Pack (2 videos)", "/d/pack.torrent (2 videos)"), str((title("magnet:?xt=urn:btih:x&dn=The.Pack"), title("/d/pack.torrent"))))
        ok("a Windows path works, and a path with no folder just counts", (title("C:\\v\\trip\\a.mkv"), title("a.mkv")) == ("C:\\v\\trip (2 videos)", "2 videos"), str((title("C:\\v\\trip\\a.mkv"), title("a.mkv"))))
        ok("one file is its own full path", js(G, 'partyTitle({file: "/v/a.mkv"}, 1)') == "/v/a.mkv", js(G, 'partyTitle({file: "/v/a.mkv"}, 1)'))
        tab(G, "party")
        line = lambda m, at="18:00:00": f"2026/11/05 {at} {m}"
        notice = lambda text, at="18:00:00": line("do: sent request: " + json.dumps({"command": ["show-text", text, 5000]}, separators=(",", ":")), at)
        js(G, f'handle({json.dumps(notice("paused by Sam"))}); handle({json.dumps(notice("from e2\nsecond line.mkv to e1.mkv, by Sam"))}); handle({json.dumps(notice("synced by Sam"))}); handle({json.dumps(notice("paused for Sam: video changed"))}); handle({json.dumps(line("now playing: " + json.dumps("e1.mkv")))})')
        ok("a move's notices land with the file it moved to, in order, whichever came first", groups()[0][0] == "e1.mkv" and groups()[0][2] == ["paused for Sam: video changed", "synced by Sam", "from e2\nsecond line.mkv to e1.mkv, by Sam"], str(groups()))
        ok("and the rest stays behind", "paused by Sam" in groups()[1][2], str(groups()))
        js(G, f'handle({json.dumps(line("now playing: " + json.dumps("e3.mkv"), "18:00:10"))})')
        ok("a later move leaves the notices of the one before", groups()[1][0] == "e1.mkv" and len(groups()[1][2]) == 3, str(groups()))
        js(G, f'handle({json.dumps(notice("synced by Noor", "18:00:20"))}); handle({json.dumps(line("now playing: " + json.dumps("e2.mkv"), "18:00:20"))})')
        ok("a seek on its own stays with the file it was on", groups()[1][0] == "e3.mkv" and "synced by Noor" in groups()[1][2], str(groups()))
        row = '[...$("feed").querySelectorAll("li")].find(li => li.lastChild.textContent == "synced by Noor")'
        got = js(G, f'[+{row}.dataset.at, {row}.querySelector("time").textContent]')
        want = js(G, '[+new Date(2026, 10, 5, 18, 0, 20), clock(new Date(2026, 10, 5, 18, 0, 20))]')
        ok("a row's moment and time are the log line's own", got == want, str((got, want)))
        stops = json.dumps(line('Watch: broadcasted event: {"name":"pause","data":"yes"}', "18:01:00"))
        js(G, f'handle({json.dumps(notice("paused for Sam: catching up", "18:01:00"))}); handle({stops})')
        ok("stopping for someone who fell behind is one line, not also paused by you", feed(G)[0] == "paused by you: catching up" and not {"paused by you", "paused for Sam: catching up"} & set(feed(G)), str(feed(G)[:3]))
        js(G, f'handle({json.dumps(notice("paused for Sam: catching up", "18:01:10"))}); handle({json.dumps(notice("resumed by Sam", "18:01:20"))}); handle({stops})')
        ok("a party already stopped keeps the notice, and a later pause is your own", feed(G)[:3] == ["paused by you", "resumed by Sam", "paused for Sam: catching up"], str(feed(G)[:4]))
        for why, text in (("joining", "paused for Sam: joining"), ("out of sync", "paused: out of sync with Sam")):
            js(G, f'handle({json.dumps(notice(text, "18:01:30"))}); handle({stops})')
            ok(f"stopping for someone {why} is one line too", feed(G)[0] == f"paused by you: {why}" and text not in feed(G), str(feed(G)[:3]))

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
            ok(f"{n} plays on, and its feed says so", until(lambda: "lost the relay, reconnecting" in feed(n) and read_all(n))
               and get(n, "pause") is False, str(feed(n)[:3]))
        ok(f"{G} can't chat meanwhile", js(G, '$("chat-text").disabled && $("chat-send").disabled'))
        own[0] = launch_relay({"CREATE_KEY": KEY}, port)
        until(lambda: all(status(n).startswith("You're in") for n in (H, G)), 40)
        for n in (H, G):
            must(f"{n} is back in", status(n).startswith("You're in"), status(n))
        ok(f"{G} can chat again", js(G, '!$("chat-text").disabled && !$("chat-send").disabled'))
        must("the returns are over", quiet([H, G]))
        ok("nobody stopped", all(get(n, "pause") is False for n in (H, G)), str([feed(n)[:2] for n in (H, G)]))

    @case("[G19b] an invite ticks the box, so a playlist party can't be missed by accident")
    def _():
        H, G = gui("g19bHost", key=KEY), gui("g19bGuest")
        js(H, '$("playlist").checked = true')
        start(H, hostname=H, file=f"{folder}/e2.mkv", room="", secret="")
        must(f"{H} is in", joined(H), status(H))
        until(lambda: js(H, '$("share-text").textContent'))
        invite = js(H, '$("share-text").textContent')
        ok("the invite asks for the playlist", invite.endswith(" -playlist"), invite)
        relay_addr = js(G, '$("relay").value')
        def paste(text):
            js(G, f'$("invite").value = {json.dumps(text)}; $("invite").dispatchEvent(new Event("input"))')
        paste(invite)
        ok(f"pasting it ticks {G}'s box", js(G, '$("playlist").checked'))
        start(G, hostname=G, file=f"{folder}/e1.mkv")
        must(f"{G} is in", joined(G), status(G))
        ok(f"{G} goes to {H}'s entry", until(lambda: get(G, "filename") == "e2.mkv"), get(G, "filename"))
        setp(H, "pause", False)
        must("they play", until(lambda: get(G, "pause") is False))
        ipc(nodes[H]["sock"], "playlist-next")
        top = lambda n: (nodes[n]["page"].feed_groups() or [[None, None, []]])[0]
        ok(f"{G}'s real notices about the move are with the file it moved to", until(lambda: top(G)[0] == "e3.mkv" and {f"from e2.mkv to e3.mkv, by {H}", f"paused for {H}: video changed"} <= set(top(G)[2])), str(nodes[G]["page"].feed_groups()))
        ok(f"so are {H}'s", until(lambda: top(H)[0] == "e3.mkv" and "paused for everyone: video changed" in top(H)[2]), str(nodes[H]["page"].feed_groups()))
        end(G)
        ok(f"{G}'s history has every file it was on", [v["name"] for v in state(G)["history"][-1].get("videos", [])] == ["e1.mkv", "e2.mkv", "e3.mkv"], str(state(G)["history"][-1]))
        js(G, '$("playlist").checked = false')
        fill(G, file=f"{folder}/e2.mkv")
        go(G)
        tick = "Diagnosis: the party plays a playlist of 3 videos, and you picked one file. Tick the box under File and start again."
        until(lambda: status(G) == tick, BOOT)
        ok("with the box unticked, it says to tick it", status(G) == tick, status(G))
        short = f"{RUN}/guishort"
        with making:
            shutil.copytree(folder, short)
            os.remove(f"{short}/e3.mkv")
        js(G, '$("playlist").checked = true')
        fill(G, file=f"{short}/e2.mkv")
        go(G)
        until(lambda: status(G) == NOT_THE_SAME_FOLDER, BOOT)
        ok("with another folder, it says the folders differ", status(G) == NOT_THE_SAME_FOLDER, status(G))
        paste("-relay ws://example.invalid -room r -secret s")
        ok("an invite with no playlist unticks it", not js(G, '$("playlist").checked'))

    @case("[G19c] Continue: the same room again, each from where it stopped (videos are 12s), then the key holder's spot")
    def _():
        H, G = gui("g19cHost", key=KEY), gui("g19cGuest")
        for n in (H, G):
            js(n, '$("playlist").checked = true')
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
        ok("Continue can't be pressed during a party", js(G, '[...$("history-list").querySelectorAll(".continue")].every(b => b.disabled)'))
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
        ok("so it has no Continue", js(H, '$("history-list").firstElementChild.querySelector(".continue") === null'))
        ago = lambda days: js(H, f'continuable({{mode: "relay", room: "r", secret: "s", ended: new Date(Date.now() - {days} * 864e5).toISOString()}})')
        ok("Continue is gone once the relay has forgotten the room, 30 days after", (ago(29), ago(31)) == (True, False), str((ago(29), ago(31))))

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
        fill(H, mpvFlags=f"{MPVFLAGS} --keep-open=always")
        js(H, '$("playlist").checked = true')
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
        H, G = gui("g20Host", key=KEY), gui("g20Guest", portal=False)
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
        js(H, 'setMode("direct"); $("addrs").value = ""; $("psecret").value = ""')
        start(H, port=str(hport))
        must(f"{H} is in", joined(H), status(H))
        until(lambda: "-addrs" in js(H, '$("share-text").textContent'))
        wifi = js(H, '$("share-text").textContent')
        addr, wsecret = wifi.split()[1], wifi.split()[3]
        ok(f"{H}'s invite has its Wi-Fi address", wifi.startswith("-addrs ") and addr.endswith(f":{hport}"), wifi)

        part(f"[G20b] a stranger with no secret can't rewrite {H}'s invite or status through its log")
        before = status(H), state(H)["history"][-1]
        for forged in ("relay: disconnected", "relay: rejected by relay", "your address to share is -addrs 6.6.6.6:1 -secret FORGED"):
            code = raw(f"127.0.0.1:{hport}", "/hi", {"hit-me-up": forged, "secret": "wrong"})
            ok(f"{forged!r} is refused", code in (401, 403), f"HTTP {code}")
        until(lambda: "-secret FORGED" in js(H, '$("log").textContent'))
        ok("the page has read the forged line", "-secret FORGED" in js(H, '$("log").textContent'))
        ok("the invite is unchanged", js(H, '$("share-text").textContent') == wifi, js(H, '$("share-text").textContent'))
        ok("the status is unchanged", status(H) == before[0], status(H))
        ok("the saved session is unchanged", state(H)["history"][-1] == before[1], str(state(H)["history"][-1]))

        js(G, f'$("invite").value = {json.dumps(wifi)}; $("invite").dispatchEvent(new Event("input"))')
        ok(f"{G} switched to Peer-to-Peer and filled it in", js(G, f'mode() == "direct" && $("addrs").value == {json.dumps(addr)} && $("psecret").value == {json.dumps(wsecret)}'))
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
        until(lambda: not js(G, "running"))
        ok(f"{G} says it ended", status(G) == "The party has ended.", status(G))
        ok(f"{H}'s feed says {G} left", until(lambda: f"{G} has left" in feed(H)), str(feed(H)))

        part(f"[G22] SIGTERM ends {H} mid-party, mpv, window and all")
        sock = nodes[H]["sock"]
        window = next(c for c in children(nodes[H]["proc"].pid) if b"--remote-debugging-port" in open(f"/proc/{c}/cmdline", "rb").read())
        nodes[H]["proc"].send_signal(signal.SIGTERM)
        ok(f"{H} exited", until(lambda: nodes[H]["proc"].poll() is not None, 20))
        ok(f"{H}'s mpv closed", not listening_unix(sock))
        ok(f"{H}'s window closed", until(lambda: gone(window)))
        ok("and its browser profile is deleted", not os.path.exists(profile(H)), profile(H))

        part(f"[G23] {G}'s history: every party, Watch again peer-to-peer, Continue over the internet")
        tab(G, "history")
        until(lambda: js(G, '$("history-list").children.length') == len(state(G)["history"]))
        items = js(G, '[...$("history-list").children].map(li => li.textContent)')
        ok("newest first, a peer-to-peer party with the host", "peer-to-peer" in items[0] and f"with {H}" in items[0], items[0])
        ok("the first is over the internet with the host", "over the internet" in items[-1] and f"with {H}" in items[-1], items[-1])
        buttons = lambda li: js(G, f'[...$("history-list").{li}.querySelectorAll("button")].map(b => b.textContent)')
        ok("a peer-to-peer party has Watch again alone", buttons("firstElementChild") == ["Watch again"], str(buttons("firstElementChild")))
        ok("a party over the internet has Continue alone", buttons("lastElementChild") == ["Continue"], str(buttons("lastElementChild")))
        click(G, "#history-list li .again")
        ok("Watch again fills Peer-to-Peer and shows the form", js(G, f'mode() == "direct" && $("addrs").value == {json.dumps(addr)} && $("psecret").value == {json.dumps(wsecret)} && !$("view-party").hidden'))

        part(f"[G24] closing {G}'s window ends its GUI")
        browser = next(c for c in children(nodes[G]["proc"].pid) if b"--remote-debugging-port" in open(f"/proc/{c}/cmdline", "rb").read())
        os.kill(browser, signal.SIGTERM)
        ok(f"{G} exited", until(lambda: nodes[G]["proc"].poll() is not None))
        ok(f"{G} logged it", "gui: window closed" in log(G))

        part(f"[G25] {G} again, on the same folder: everything was kept")
        count = len(items)
        G2 = launch_gui(G, tag=f"{G}2", portal=False)
        on_end(lambda: kill_gui(G2))
        ok("a new port", nodes[G2]["url"] != nodes[G]["url"])
        ok("the name, relay and Wi-Fi port are filled in", js(G2, f'$("hostname").value == {json.dumps(G)} && $("relay").value == {json.dumps(RELAY_URL)} '
                                                             f'&& $("port").value == {json.dumps(str(gport))} && $("local").checked'))
        tab(G2, "settings")
        ok("the settings are kept", js(G2, f'$("mpvFlags").value == {json.dumps(MPVFLAGS)} && $("set-relay").value == {json.dumps(RELAY_URL)}'))
        tab(G2, "history")
        ok("the history is kept", until(lambda: js(G2, '$("history-list").children.length') == count))

        part("[G26] Clear history asks, then empties it")
        click(G2, "#clear-history")
        until(lambda: js(G2, '$("history-list").children.length') == 0)
        ok("it asked first", js(G2, "window.asked") == "Clear the whole party history?")
        ok("the list is empty and says so", js(G2, '!$("history-empty").hidden && $("clear-history").hidden'))
        ok("the store is empty", not state(G2)["history"])

        part("[G27] the cat")
        js(G2, 'document.querySelector(".cat").dispatchEvent(new MouseEvent("click", {bubbles: true}))')
        ok("petting it makes a heart", js(G2, 'document.querySelectorAll(".heart").length') == 1)

        part(f"[G27a] a reload keeps {G2} up; closing its window ends it, even with the browser still running, as on macOS")
        nodes[G2]["page"].call("Page.reload")
        hold(3 + QUIET, "the GUI's closeGrace: a reload that ended it would show")
        ok("the reload didn't end it", nodes[G2]["proc"].poll() is None and "gui: window closed" not in log(G2), log(G2).strip())
        try:
            nodes[G2]["page"].call("Page.close")
        except (OSError, RuntimeError, ValueError):
            pass  # the page can go before it answers
        ok(f"{G2} exited", until(lambda: nodes[G2]["proc"].poll() is not None, 10))
        ok(f"{G2} logged it", "gui: window closed" in log(G2), log(G2).strip())
        ok("and its browser profile is deleted", not os.path.exists(profile(G2)), profile(G2))

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
        tab(U, "settings")
        until(lambda: js(U, '!$("update").hidden'))
        ok("it offers v0.0.2", js(U, '$("version-text").textContent == "Version v0.0.1" && $("update").textContent == "Update to v0.0.2" && !$("update-dot").hidden'))
        click(U, "#update")
        until(lambda: js(U, '$("version-text").textContent').startswith("Update failed"))
        ok("a bad digest is refused", js(U, '$("version-text").textContent') == "Update failed: the download does not match its published sha256, not installing it.", js(U, '$("version-text").textContent'))
        ok("and the binary is untouched", sha(exe) == old)
        ok("and no download is left behind", os.listdir(f"{RUN}/upd") == ["watchparty-gui"], str(os.listdir(f"{RUN}/upd")))
        rel.digest = "sha256:" + rel.sha
        click(U, "#update")
        until(lambda: js(U, '$("restart-dialog").open'))
        ok("a good one is installed", sha(exe) == rel.sha)
        ok("and it asks to restart", js(U, '$("restart-text").textContent') == "Version v0.0.2 is installed. A restart is required to start using it.")
        click(U, "#restart-later")
        ok("Restart later closes the dialog and leaves the button", js(U, '!$("restart-dialog").open && !$("restart").hidden && $("update").hidden'))

        part("[G29] Restart now hands over to the new version")
        click(U, "#restart")
        ok("the old one exited", until(lambda: nodes[U]["proc"].poll() is not None))
        attach(U, 2)
        until(lambda: js(U, '$("version-text").textContent') != "Checking the version…")
        ok("the new window runs v0.0.2", js(U, '$("version-text").textContent') == "Version v0.0.2", js(U, '$("version-text").textContent'))
        ok("and offers nothing", js(U, '$("update").hidden && $("restart").hidden'))

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

    @case("[G30] a browser that hands the page off doesn't count as closing the window")
    def _():
        O = launch_gui("guiHandoff", browser="handoff")
        on_end(lambda: kill_gui(O))
        until(lambda: launches(O))
        hold(QUIET, "the GUI would end closeGrace after its window went")
        ok("the GUI is still up", nodes[O]["proc"].poll() is None and listening(int(nodes[O]["url"].rsplit(":", 1)[1].strip("/"))))
        ok("and didn't log a closed window", "gui: window closed" not in log(O))

    @case("[G31] with no browser at all, it says where the page is")
    def _():
        system = [d for d in ("/var/lib/flatpak/exports/share/applications", "/var/lib/snapd/desktop/applications")
                  if os.path.isdir(d) and any(re.search("chrom|edge|brave|vivaldi", f, re.I) for f in os.listdir(d))]
        if system:
            print(f"  SKIP  a system-wide browser in {system} would open on screen")
            return
        N = launch_gui("guiNoBrowser", browser=None)
        on_end(lambda: kill_gui(N))
        until(lambda: "no browser could be opened" in log(N))
        ok("it tried xdg-open", open(f"{nodes[N]['home']}/xdg-open.args").read().strip() == nodes[N]["url"])
        ok("and logged the address", f"gui: no browser could be opened (exit status 1), open {nodes[N]['url']} yourself" in log(N), log(N).strip())

    @case("[G32] window.close() from the page closes a real window, and the GUI with it")
    def _():
        # headless Chromium ignores window.close() on an --app window; Restart now relies on it
        X = launch_gui("guiXvfb", browser="xvfb")
        on_end(lambda: kill_gui(X))
        try:
            js(X, "window.close()")
        except (OSError, RuntimeError, ValueError):
            pass  # the page can go before it answers
        ok("the GUI exited", until(lambda: nodes[X]["proc"].poll() is not None))
        ok("on a closed window", "gui: window closed" in log(X), log(X).strip())

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
