#!/usr/bin/env python3
"""End-to-end suite for what only Windows has. Real mpv, real transports, a real GUI.

e2e.py covers the behaviour; this covers the Windows half of it: mpv's named pipe,
mpv in its own console and process group, Ctrl-Break, the mpv.com wrapper, the
GUI built with -H=windowsgui (as released) starting mpv and Chrome by absolute path,
closing its window, the hand-off guard, and the self-update's .old swap.

    python e2e_windows.py

Needs go, mpv, ffmpeg and Chrome. The GUI finds Chrome at %ProgramFiles%; each GUI
gets a ProgramFiles of its own holding gui/fakebrowser as chrome.exe, which runs the
real Chrome headless with a debugging port. Exits non-zero if any assertion fails;
the run directory with every log is printed at the end.
"""
import argparse, json, os, re, shutil, signal, subprocess
import sys, tempfile, threading, time

sys.dont_write_bytecode = True
import e2e_lib
from e2e_lib import Page, Release, find_page, sha

if os.name != "nt":
    sys.exit("e2e_windows.py runs on Windows; on Linux, use make e2e")

ap = argparse.ArgumentParser()
ap.add_argument("--port-base", type=int, default=7969)
args = ap.parse_args()

MPVFLAGS = "--vo=null --ao=null --msg-level=all=error"
BOOT, SETTLE, QUIET = 30, 15, 1.0
PORTS = {"relay": args.port_base, "release": args.port_base + 1, "p1": args.port_base + 2, "p2": args.port_base + 3}
RELAY_URL = f"ws://127.0.0.1:{PORTS['relay']}"
REPO = os.path.dirname(os.path.abspath(__file__))
RUN = tempfile.mkdtemp(prefix="watchparty-e2e-")
CHROME = os.path.join(os.environ.get("ProgramFiles", ""), r"Google\Chrome\Application\chrome.exe")

fails, nodes, others_procs = [], {}, []


def ok(name, cond, detail=""):
    if not cond:
        fails.append(name)
    print(("  PASS  " if cond else "  FAIL  ") + name + (f"  [{detail}]" if detail else ""), flush=True)


class Stop(Exception):
    """A setup step failed: every case after it would fail for the same reason."""


def must(name, cond, detail=""):
    """ok(), but a failure ends the phase here instead of cascading into later cases."""
    ok(name, cond, detail)
    if not cond:
        raise Stop(name)


def until(cond, timeout=SETTLE):
    return e2e_lib.until(cond, timeout)


def need(tool):
    if not shutil.which(tool):
        sys.exit(f"{tool} is required but not on PATH")


def build(out, pkg, ldflags=""):
    subprocess.run(["go", "build", "-ldflags", ldflags, "-o", os.path.join(RUN, out), pkg], cwd=REPO, check=True)
    return os.path.join(RUN, out)


# ------------------------------------------------------------- processes --

def processes():
    ps = "Get-CimInstance Win32_Process | Select-Object ProcessId,ParentProcessId,Name,CommandLine | ConvertTo-Json -Compress"
    out = subprocess.run(["powershell", "-NoProfile", "-Command", ps], capture_output=True, text=True).stdout
    return json.loads(out or "[]")


def children(pid, name=None):
    return [p for p in processes() if p["ParentProcessId"] == pid and (name is None or (p["Name"] or "").lower() == name)]


def mpv_pipe(pid):
    """The pipe of the mpv.exe this process started, or None."""
    for p in children(pid, "mpv.exe"):
        m = re.search(r"--input-ipc-server=(\S+)", p["CommandLine"] or "")
        if m:
            return m.group(1).strip('"')
    return None


def kill_tree(pid):
    subprocess.run(["taskkill", "/F", "/T", "/PID", str(pid)], capture_output=True)


def exists(pipe):
    try:
        open(pipe, "r+b", buffering=0).close()
        return True
    except OSError:
        return False


# ------------------------------------------------------------------- mpv --

def ipc(pipe, *cmd):
    """One request over mpv's named pipe. In a thread, so a stuck pipe can't hang the suite."""
    out = {}

    def run():
        with open(pipe, "r+b", buffering=0) as f:
            f.write((json.dumps({"command": list(cmd), "request_id": 99}) + "\n").encode())
            buf = b""
            while True:
                chunk = f.read(65536)
                if not chunk:
                    return
                buf += chunk
                *lines, buf = buf.split(b"\n")
                for line in lines:
                    try:
                        m = json.loads(line)
                    except ValueError:
                        continue
                    if m.get("request_id") == 99:
                        out["m"] = m
                        return
    t = threading.Thread(target=lambda: _quiet(run), daemon=True)
    t.start()
    t.join(5)
    return out.get("m")


def _quiet(f):
    try:
        f()
    except OSError:
        pass


def get(n, prop):
    r = ipc(nodes[n]["pipe"], "get_property", prop)
    return r.get("data") if r else None


def setp(n, prop, val):
    ipc(nodes[n]["pipe"], "set_property", prop, val)


def near(v, x, tol=1.5):
    return v is not None and abs(v - x) < tol


# ----------------------------------------------------------------- nodes --

def log(n):
    with open(nodes[n]["log"], encoding="utf-8", errors="replace") as f:
        return f.read()


def osd(n):
    out = []
    for line in log(n).splitlines():
        if 'do: sent request: {"command":["show-text"' in line:
            out.append(json.loads(line.split("do: sent request: ")[1])["command"][1])
    return out


def known(n):
    peers = set()
    for m in osd(n):
        if m.endswith(" has joined"):
            peers.add(m[:-len(" has joined")])
        elif m.startswith("connected to "):
            peers.add(m[len("connected to "):])
    return peers


def launch(name, *flags, mpv=None):
    path = os.path.join(RUN, name + ".log")
    cmd = [BIN, "-file", MEDIA, "-hostname", name, "-mpvFlags", MPVFLAGS] + list(flags) + (["-mpv", mpv] if mpv else [])
    # its own process group, so Ctrl-Break reaches it and not this script
    p = subprocess.Popen(cmd, stdout=open(path, "wb"), stderr=subprocess.STDOUT, creationflags=subprocess.CREATE_NEW_PROCESS_GROUP)
    nodes[name] = {"proc": p, "log": path, "pipe": None}
    return name


def booted(n):
    return "init done" in log(n)


def resolve(n):
    until(lambda: mpv_pipe(nodes[n]["proc"].pid), BOOT)
    nodes[n]["pipe"] = mpv_pipe(nodes[n]["proc"].pid)
    if not nodes[n]["pipe"]:
        sys.exit(f"{n} never started mpv; see {nodes[n]['log']}")


def ctrl_break(n):
    nodes[n]["proc"].send_signal(signal.CTRL_BREAK_EVENT)


# ------------------------------------------------------------------- gui --

def environ(**over):
    """os.environ with over applied. Windows names are case-insensitive and Python keeps
    them upper-cased, so a mixed-case key would sit next to the real one, not replace it."""
    return {**os.environ, **{k.upper(): v for k, v in over.items()}}


def launch_gui(name, exe=None, browser=True):
    home = os.path.join(RUN, name)
    pf = os.path.join(home, "pf", r"Google\Chrome\Application")
    if not os.path.exists(home):
        for d in ("appdata", "local", "tmp", "pf86", "pf"):
            os.makedirs(os.path.join(home, d))
        os.makedirs(pf)
        shutil.copy(FAKEBROWSER, os.path.join(pf, "chrome.exe"))
    env = environ(APPDATA=os.path.join(home, "appdata"), LOCALAPPDATA=os.path.join(home, "local"),
                  TMP=os.path.join(home, "tmp"), TEMP=os.path.join(home, "tmp"),
                  # a 64-bit process gets ProgramFiles rebuilt from ProgramW6432, so set both
                  ProgramFiles=os.path.join(home, "pf"), ProgramW6432=os.path.join(home, "pf"),
                  **{"ProgramFiles(x86)": os.path.join(home, "pf86")},
                  E2E_BROWSER=CHROME if browser else "", E2E_BROWSER_LOG=os.path.join(home, "browser.args"))
    path = os.path.join(RUN, name + ".log")
    nodes[name] = {"home": home}
    opened = len(launches(name))
    p = subprocess.Popen([exe or GUI_BIN], env=env, stdout=open(path, "wb"), stderr=subprocess.STDOUT)
    nodes[name].update(proc=p, log=path, pipe=None, gui=True)
    until(lambda: "gui: http" in log(name), BOOT)
    nodes[name]["url"] = log(name).split("gui: ", 1)[1].split("\n", 1)[0].strip()
    if browser:
        attach(name, opened + 1)
    return name


def launches(n):
    try:
        with open(os.path.join(nodes[n]["home"], "browser.args")) as f:
            return f.read().splitlines()
    except OSError:
        return []


def attach(n, count):
    def ws():
        lines = launches(n)
        return find_page(lines[count - 1]) if len(lines) >= count else None
    until(ws, BOOT)
    url = ws()
    if not url:
        tree = [(p["Name"], p["CommandLine"]) for p in processes() if p["ParentProcessId"] == nodes[n]["proc"].pid]
        files = [os.path.join(d, f) for d, _, fs in os.walk(nodes[n]["home"]) for f in fs][:40]
        print(f"--- {n}'s log:\n{log(n)}--- its browser launches: {launches(n)}\n--- its children: {tree}\n--- its files: {files}")
        sys.exit(f"{n} never opened its page")
    nodes[n]["page"] = Page(url)
    nodes[n]["page"].ready(BOOT)


def js(n, expr):
    return nodes[n]["page"].js(expr)


def status(n):
    return nodes[n]["page"].status()


def feed(n):
    return nodes[n]["page"].feed()


def click(n, selector):
    nodes[n]["page"].click(selector)


def fill(n, **fields):
    nodes[n]["page"].fill(**fields)


def go(n):
    nodes[n]["page"].go(BOOT)


def browser_of(n):
    return next((p["ProcessId"] for p in children(nodes[n]["proc"].pid, "chrome.exe")), None)


# ----------------------------------------------------------------- cases --

def cli_suite():
    A, B, C = "winA", "winB", "winC"
    print("\n=== cli: three nodes through one relay, over mpv's named pipes ===")
    relay = subprocess.Popen([RELAY_BIN], env=environ(ADDR=f"127.0.0.1:{PORTS['relay']}"),
                             stdout=open(os.path.join(RUN, "relay.log"), "wb"), stderr=subprocess.STDOUT)
    others_procs.append(relay)
    for n in (A, B, C):
        launch(n, "-relay", RELAY_URL, "-room", "winroom", "-secret", "winsecret")
    until(lambda: all(booted(n) for n in (A, B, C)), BOOT)
    for n in (A, B, C):
        resolve(n)

    print("\n[W1] mpv runs as mpv.exe on a named pipe, and everyone meets")
    for n in (A, B, C):
        ok(f"{n}'s mpv listens on {nodes[n]['pipe']}", nodes[n]["pipe"].startswith("\\\\.\\pipe\\mpv") and get(n, "pause") is True)
    until(lambda: all({A, B, C} - {n} <= known(n) for n in (A, B, C)))
    for n in (A, B, C):
        ok(f"{n} knows the others", {A, B, C} - {n} <= known(n), str(known(n)))

    print(f"\n[W2] a resume on {A} reaches everyone")
    setp(A, "pause", False)
    until(lambda: all(get(n, "pause") is False for n in (B, C)) and all(f"resumed by {A}" in osd(n) for n in (B, C)))
    for n in (B, C):
        ok(f"{n} plays, credited to {A}", get(n, "pause") is False and f"resumed by {A}" in osd(n))

    print(f"\n[W3] a seek while playing on {B} pauses everyone")
    ipc(nodes[B]["pipe"], "seek", 30)
    until(lambda: all(get(n, "pause") is True for n in (A, C)) and f"paused for {B}: seek" in osd(A))
    for n in (A, C):
        ok(f"{n} paused, told why", get(n, "pause") is True and f"paused for {B}: seek" in osd(n), str(osd(n)[-3:]))

    print(f"\n[W4] a seek while paused on {A} moves everyone")
    setp(A, "time-pos", 60)
    until(lambda: all(near(get(n, "time-pos"), 60) for n in (B, C)))
    for n in (B, C):
        ok(f"{n} is at 60s", near(get(n, "time-pos"), 60), str(get(n, "time-pos")))

    print(f"\n[W5] Ctrl-Break ends {C} cleanly, mpv and all")
    pipe = nodes[C]["pipe"]
    ctrl_break(C)
    ok(f"{C} exited 0", until(lambda: nodes[C]["proc"].poll() is not None, 20) and nodes[C]["proc"].returncode == 0, str(nodes[C]["proc"].poll()))
    ok(f"{C}'s mpv is gone", until(lambda: not exists(pipe)))
    ok(f"{A} was told {C} left", until(lambda: f"{C} has left" in osd(A)))

    print(f"\n[W6] quitting mpv ends {B} with exit 0")
    ipc(nodes[B]["pipe"], "quit")
    ok(f"{B} exited 0", until(lambda: nodes[B]["proc"].poll() is not None, 20) and nodes[B]["proc"].returncode == 0, str(nodes[B]["proc"].poll()))
    ok(f"{A} was told {B} left", until(lambda: f"{B} has left" in osd(A)))
    ctrl_break(A)
    nodes[A]["proc"].wait(20)

    print("\n[W7] two nodes on -local find each other and sync")
    P1, P2 = "winP1", "winP2"
    launch(P1, "-local", "-port", str(PORTS["p1"]), "-secret", "winsecret")
    until(lambda: booted(P1), BOOT)
    addr = log(P1).split("-addrs ", 1)[1].split()[0]
    launch(P2, "-local", "-port", str(PORTS["p2"]), "-secret", "winsecret", "-addrs", addr)
    until(lambda: booted(P2), BOOT)
    for n in (P1, P2):
        resolve(n)
    until(lambda: P2 in known(P1) and P1 in known(P2))
    ok("they met", P2 in known(P1) and P1 in known(P2))
    setp(P2, "pause", False)
    ok(f"{P1} follows {P2}", until(lambda: get(P1, "pause") is False))
    for n in (P1, P2):
        ctrl_break(n)
    for n in (P1, P2):
        ok(f"{n} exited 0", nodes[n]["proc"].wait(20) == 0)

    print("\n[W8] given mpv.com, the party runs mpv.exe and can still end it")
    com = os.path.join(os.path.dirname(shutil.which("mpv")), "mpv.com")
    if not os.path.exists(com):
        print(f"  SKIP  no mpv.com next to {shutil.which('mpv')}")
    else:
        M = launch("winCom", "-relay", RELAY_URL, "-room", "comroom", "-secret", "winsecret", mpv=com)
        until(lambda: booted(M), BOOT)
        resolve(M)
        ok("its child is mpv.exe", bool(nodes[M]["pipe"]))
        pipe = nodes[M]["pipe"]
        ctrl_break(M)
        ended = until(lambda: nodes[M]["proc"].poll() is not None, 20)
        ok("the party ended", ended, "" if ended else "Run never returned: mpv.exe kept the pipe open")
        ok("and took mpv.exe with it", until(lambda: not exists(pipe)))
    relay.kill()


def gui_suite():
    H, G = "winHost", "winGuest"
    print("\n=== gui: two GUIs built -H=windowsgui, each in headless Chrome ===")
    relay = subprocess.Popen([RELAY_BIN], env=environ(ADDR=f"127.0.0.1:{PORTS['relay']}"),
                             stdout=open(os.path.join(RUN, "relay-gui.log"), "wb"), stderr=subprocess.STDOUT)
    others_procs.append(relay)
    for n in (H, G):
        launch_gui(n)

    print("\n[W9] the GUI opens Chrome from Program Files as an app window")
    for n in (H, G):
        argv = launches(n)[0]
        ok(f"{n} opened {nodes[n]['url']} at 560x940", f"--app={nodes[n]['url']}" in argv and "--window-size=560,940" in argv, argv)

    print("\n[W10] a relay party from the page: mpv starts under the windowsgui parent")
    fill(H, hostname=H, file=MEDIA, relay=RELAY_URL, mpvFlags=MPVFLAGS)
    go(H)
    must(f"{H} is in", until(lambda: status(H).startswith("You're in"), BOOT), status(H))
    nodes[H]["pipe"] = mpv_pipe(nodes[H]["proc"].pid)
    ok(f"{H}'s mpv.exe answers", bool(nodes[H]["pipe"]) and get(H, "pause") is True)
    until(lambda: js(H, '$("share-text").textContent') != "")
    invite = js(H, '$("share-text").textContent')
    js(G, f'$("invite").value = {json.dumps(invite)}; $("invite").dispatchEvent(new Event("input"))')
    fill(G, hostname=G, file=MEDIA, mpvFlags=MPVFLAGS)
    go(G)
    must(f"{G} is in", until(lambda: status(G).startswith("You're in"), BOOT), status(G))
    nodes[G]["pipe"] = mpv_pipe(nodes[G]["proc"].pid)

    print(f"\n[W11] {G}'s resume reaches {H}, and both feeds say so")
    setp(G, "pause", False)
    until(lambda: get(H, "pause") is False and f"resumed by {G}" in feed(H) and feed(G)[:1] == ["resumed by you"])
    ok(f"{H} plays", get(H, "pause") is False)
    ok("the feeds", f"resumed by {G}" in feed(H) and feed(G)[:1] == ["resumed by you"], f"{feed(H)} / {feed(G)}")

    print(f"\n[W12] {G} ends the party from the page")
    pipe = nodes[G]["pipe"]
    click(G, "#go")
    until(lambda: status(G) == "The party has ended.")
    ok(f"{G} says it ended", status(G) == "The party has ended.", status(G))
    ok(f"{G}'s mpv.exe is gone", until(lambda: not exists(pipe)))
    ok(f"{H} was told", until(lambda: f"{G} has left" in feed(H)), str(feed(H)))

    print(f"\n[W13] closing {H}'s window ends its GUI and its mpv")
    pipe = nodes[H]["pipe"]
    kill_tree(browser_of(H))
    ok(f"{H} exited", until(lambda: nodes[H]["proc"].poll() is not None))
    ok("on a closed window", "gui: window closed" in log(H))
    ok(f"{H}'s mpv.exe is gone", until(lambda: not exists(pipe)))
    kill_tree(nodes[G]["proc"].pid)

    print("\n[W14] a browser that hands the page off doesn't count as closing the window")
    O = launch_gui("winHandoff", browser=False)
    until(lambda: launches(O))
    time.sleep(QUIET)
    ok("the GUI is still up", nodes[O]["proc"].poll() is None and "gui: window closed" not in log(O))
    kill_tree(nodes[O]["proc"].pid)

    print("\n[W15] self-update: the running .exe becomes .old, Restart runs v0.0.2, the next start removes .old")
    rel = Release(PORTS["release"], GUI_NEW, "watchparty-gui.exe", good=True)
    os.makedirs(os.path.join(RUN, "upd"))
    exe = os.path.join(RUN, "upd", "watchparty-gui.exe")
    shutil.copy(GUI_OLD, exe)
    U = launch_gui("winUpdate", exe=exe)
    nodes[U]["page"].tab("settings")
    until(lambda: js(U, '!$("update").hidden'))
    ok("it offers v0.0.2", js(U, '$("update").textContent') == "Update to v0.0.2", js(U, '$("version-text").textContent'))
    click(U, "#update")
    until(lambda: js(U, '$("restart-dialog").open') or js(U, '$("version-text").textContent').startswith("Update failed"), 60)
    ok("installed", js(U, '$("restart-dialog").open') and sha(exe) == rel.sha, js(U, '$("version-text").textContent'))
    ok("the running binary was moved to .old", os.path.exists(exe + ".old") and sha(exe + ".old") == sha(GUI_OLD))
    old_pid = nodes[U]["proc"].pid
    click(U, "#restart-now")
    ok("the old one exited", until(lambda: nodes[U]["proc"].poll() is not None))
    attach(U, 2)
    until(lambda: js(U, '$("version-text").textContent') != "Checking the version…")
    ok("the new window runs v0.0.2", js(U, '$("version-text").textContent') == "Version v0.0.2", js(U, '$("version-text").textContent'))
    new = [p for p in processes() if (p["CommandLine"] or "").strip('" ').lower().startswith(exe.lower()) and p["ProcessId"] != old_pid]
    for p in new:
        kill_tree(p["ProcessId"])
    nodes[U]["proc"] = subprocess.Popen([exe], env=environ(APPDATA=os.path.join(RUN, "winUpdate", "appdata"),
                                                           ProgramFiles=os.path.join(RUN, "winUpdate", "pf"),
                                                           ProgramW6432=os.path.join(RUN, "winUpdate", "pf"), E2E_BROWSER="",
                                                           E2E_BROWSER_LOG=os.path.join(RUN, "winUpdate", "browser.args")))
    ok("the next start removes .old", until(lambda: not os.path.exists(exe + ".old")))
    kill_tree(nodes[U]["proc"].pid)
    relay.kill()


# ------------------------------------------------------------------ main --

need("go")
need("mpv")
need("ffmpeg")
if not os.path.exists(CHROME):
    sys.exit(f"Chrome is required at {CHROME}")
print(f"run directory: {RUN}")
BIN = build("watchparty.exe", ".")
RELAY_BIN = build("relay.exe", "./relay")
release = f"-X main.latestReleaseURL=http://127.0.0.1:{PORTS['release']}/latest"
GUI_BIN = build("watchparty-gui.exe", "./gui", "-H=windowsgui")
GUI_OLD = build("gui-v0.0.1.exe", "./gui", f"-H=windowsgui -X main.version=v0.0.1 {release}")
GUI_NEW = build("gui-v0.0.2.exe", "./gui", f"-H=windowsgui -X main.version=v0.0.2 {release}")
FAKEBROWSER = build("fakebrowser.exe", "./gui/fakebrowser")
MEDIA = os.path.join(RUN, "test.mkv")
subprocess.run(["ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10", "-t", "120",
                "-c:v", "libx264", "-pix_fmt", "yuv420p", MEDIA], check=True)

try:
    cli_suite()
    gui_suite()
except Stop as e:
    print(f"  STOP  the rest of the suite depends on: {e}")
finally:
    for n in nodes.values():
        if "proc" in n and n["proc"].poll() is None:
            kill_tree(n["proc"].pid)
    for p in others_procs:
        if p.poll() is None:
            p.kill()
    # anything left that was started from the run directory: a restarted GUI, its Chrome, an mpv
    for p in processes():
        if RUN.lower() in (p["CommandLine"] or "").lower() and p["ProcessId"] != os.getpid():
            kill_tree(p["ProcessId"])

print(f"\nlogs: {RUN}")
print("ALL PASS" if not fails else f"{len(fails)} FAILURES: " + ", ".join(fails))
sys.exit(1 if fails else 0)
