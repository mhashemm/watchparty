#!/usr/bin/env python3
"""End-to-end suite for what only Windows has. Real mpv, real transports, a real GUI.

e2e.py covers the behaviour; this covers the Windows half of it: mpv's named pipe,
mpv in its own console and process group, Ctrl-Break, the mpv.com wrapper, the
GUI built with -H=windowsgui (as released) starting mpv by absolute path, closing
its window, the self-update's .old swap, and the installer.

    python e2e_windows.py

Needs go, mpv, ffmpeg and makensis. The GUI is built with -tags e2e and driven through
its control socket (gui/e2e.go). Exits non-zero if any assertion fails; the run
directory with every log is printed at the end.
"""
import argparse, ctypes, json, os, re, shutil, signal, subprocess
import sys, tempfile, threading, time

assert sys.platform == "win32", "e2e_windows.py runs on Windows; elsewhere, make e2e"
import winreg

sys.dont_write_bytecode = True
import e2e_lib
from e2e_lib import Release, Window, sha

if os.name != "nt":
    sys.exit("e2e_windows.py runs on Windows; on Linux, use make e2e")

ap = argparse.ArgumentParser()
ap.add_argument("--port-base", type=int, default=7969)
args = ap.parse_args()

MPVFLAGS = "--vo=null --ao=null --msg-level=all=error"
BOOT, SETTLE = 30, 15
PORTS = {"relay": args.port_base, "release": args.port_base + 1, "p1": args.port_base + 2, "p2": args.port_base + 3, "cliRelease": args.port_base + 4}
RELAY_URL = f"ws://127.0.0.1:{PORTS['relay']}"
REPO = os.path.dirname(os.path.abspath(__file__))
RUN = tempfile.mkdtemp(prefix="watchparty-e2e-")

fails, nodes, others_procs = [], {}, []


def ok(name, cond, detail: object = ""):
    if not cond:
        fails.append(name)
    print(("  PASS  " if cond else "  FAIL  ") + name + (f"  [{detail}]" if detail else ""), flush=True)


class Stop(Exception):
    """A setup step failed: every case after it would fail for the same reason."""


def must(name, cond, detail: object = ""):
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
    """A binary into the run directory; the GUI with its e2e control socket."""
    tags = ["-tags", "e2e"] if pkg == "./gui" else []
    subprocess.run(["go", "build", *tags, "-ldflags", ldflags, "-o", os.path.join(RUN, out), pkg], cwd=REPO, check=True)
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


def gui_env(home):
    return environ(APPDATA=os.path.join(home, "appdata"), LOCALAPPDATA=os.path.join(home, "local"),
                   TMP=os.path.join(home, "tmp"), TEMP=os.path.join(home, "tmp"), WATCHPARTY_E2E=os.path.join(home, "control"))


def launch_gui(name, exe=None):
    """A GUI with its own APPDATA and TMP, its window driven through its e2e control socket."""
    home = os.path.join(RUN, name)
    for d in ("appdata", "local", "tmp"):
        os.makedirs(os.path.join(home, d), exist_ok=True)
    path = os.path.join(RUN, name + ".log")
    p = subprocess.Popen([exe or GUI_BIN], env=gui_env(home), stdout=open(path, "wb"), stderr=subprocess.STDOUT)
    nodes[name] = {"home": home, "proc": p, "log": path, "pipe": None, "gui": True}
    attach(name)
    return name


def attach(n):
    """Wait for the window's control socket: the GUI is up, its window open and loaded."""
    w = Window(os.path.join(nodes[n]["home"], "control"))
    def up():
        try:
            return w.ui() is not None
        except (OSError, ValueError, RuntimeError):
            return False
    if not until(up, BOOT):
        tree = [(p["Name"], p["CommandLine"]) for p in processes() if p["ParentProcessId"] == nodes[n]["proc"].pid]
        print(f"--- {n}'s log:\n{log(n)}--- its children: {tree}")
        sys.exit(f"{n}'s window never answered")
    nodes[n]["window"] = w


def ui(n):
    return nodes[n]["window"].ui()


def status(n):
    return nodes[n]["window"].status()


def feed(n):
    return nodes[n]["window"].feed()


def click(n, name):
    nodes[n]["window"].click(name)


def fill(n, **fields):
    nodes[n]["window"].fill(**fields)


def tab(n, view):
    nodes[n]["window"].tab(view)


def go(n):
    nodes[n]["window"].go(BOOT)


def save_settings(n, **fields):
    tab(n, "settings")
    fill(n, **fields)
    click(n, "settings-save")
    saved = ui(n)["settings"]["saved"]
    tab(n, "party")
    return saved


def end(n):
    """End the party by its button, through the window's confirm."""
    click(n, "go")
    click(n, "dialog:ok")
    until(lambda: status(n) == "The party has ended.")


def windows_of(n):
    """n's process's visible windows."""
    user32 = ctypes.windll.user32
    user32.GetWindowThreadProcessId.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_ulong)]
    user32.IsWindowVisible.argtypes = [ctypes.c_void_p]
    pid, found = nodes[n]["proc"].pid, []

    @ctypes.WINFUNCTYPE(ctypes.c_bool, ctypes.c_void_p, ctypes.c_void_p)
    def each(hwnd, _):
        owner = ctypes.c_ulong()
        user32.GetWindowThreadProcessId(hwnd, ctypes.byref(owner))
        if owner.value == pid and user32.IsWindowVisible(hwnd):
            found.append(hwnd)
        return True
    user32.EnumWindows(each, None)
    return found


def close_window(n):
    """What the title bar's close button sends: WM_CLOSE, to each window of n's process."""
    user32 = ctypes.windll.user32
    user32.PostMessageW.argtypes = [ctypes.c_void_p, ctypes.c_uint, ctypes.c_void_p, ctypes.c_void_p]
    found = windows_of(n)
    for hwnd in found:
        user32.PostMessageW(hwnd, 0x0010, None, None)  # WM_CLOSE
    return len(found)


def frame_dark(n):
    """Whether n's window asked for a dark frame (DWMWA_USE_IMMERSIVE_DARK_MODE), or the
    HRESULT of a refused read."""
    dwm = ctypes.windll.dwmapi
    dwm.DwmGetWindowAttribute.argtypes = [ctypes.c_void_p, ctypes.c_uint, ctypes.c_void_p, ctypes.c_uint]
    on = ctypes.c_int(0)
    found = windows_of(n)
    if not found:
        return "no window"
    hr = dwm.DwmGetWindowAttribute(found[0], 20, ctypes.byref(on), 4)
    return bool(on.value) if hr == 0 else f"hr {hr & 0xffffffff:#x}"


def app_mode(dark):
    """Set Settings' "Choose your app mode"; returns the value it had (None: unset)."""
    k = winreg.CreateKeyEx(winreg.HKEY_CURRENT_USER, r"Software\Microsoft\Windows\CurrentVersion\Themes\Personalize", 0,
                           winreg.KEY_QUERY_VALUE | winreg.KEY_SET_VALUE)
    try:
        try:
            was = winreg.QueryValueEx(k, "AppsUseLightTheme")[0]
        except FileNotFoundError:
            was = None
        if dark is None:
            try:
                winreg.DeleteValue(k, "AppsUseLightTheme")
            except FileNotFoundError:
                pass
        else:
            winreg.SetValueEx(k, "AppsUseLightTheme", 0, winreg.REG_DWORD, 0 if dark else 1)
        return was
    finally:
        winreg.CloseKey(k)


# ----------------------------------------------------------------- cases --

def cli_suite():
    A, B, C = "winA", "winB", "winC"
    print("\n=== cli: three nodes through one relay, over mpv's named pipes ===")
    relay = subprocess.Popen([RELAY_BIN], env=environ(ADDR=f"127.0.0.1:{PORTS['relay']}", CREATE_KEY="k", DB=os.path.join(RUN, "relay.db")),
                             stdout=open(os.path.join(RUN, "relay.log"), "wb"), stderr=subprocess.STDOUT)
    others_procs.append(relay)
    for n in (A, B, C):
        launch(n, "-relay", RELAY_URL, "-room", "winroom", "-secret", "winsecret", *(["-createKey", "k"] if n == A else []))
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

    # B's seek stopped the party, so B leads it: only its seeks move everyone until a resume
    print(f"\n[W4] a seek while paused on {B}, who leads the stop, moves everyone")
    setp(B, "time-pos", 60)
    until(lambda: all(near(get(n, "time-pos"), 60) for n in (A, C)))
    for n in (A, C):
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
    com = os.path.join(os.path.dirname(shutil.which("mpv") or sys.exit("e2e needs mpv on PATH")), "mpv.com")
    if not os.path.exists(com):
        print(f"  SKIP  no mpv.com next to {shutil.which('mpv')}")
    else:
        M = launch("winCom", "-relay", RELAY_URL, "-room", "comroom", "-secret", "winsecret", "-createKey", "k", mpv=com)
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
    print("\n=== gui: two GUIs built -H=windowsgui, each a real window ===")
    relay = subprocess.Popen([RELAY_BIN], env=environ(ADDR=f"127.0.0.1:{PORTS['relay']}", CREATE_KEY="k", DB=os.path.join(RUN, "relay-gui.db")),
                             stdout=open(os.path.join(RUN, "relay-gui.log"), "wb"), stderr=subprocess.STDOUT)
    others_procs.append(relay)
    for n in (H, G):
        launch_gui(n)

    print("\n[W9b] About lists mpv, ffmpeg and yt-dlp, each run for its version under the windowsgui parent")
    ok("a row each", until(lambda: len(ui(H)["about"]["tools"] or []) == 3), str(ui(H)["about"]["tools"]))
    tools = {t["name"]: t for t in ui(H)["about"]["tools"]}
    ok("mpv and ffmpeg from PATH, with a version", all(tools[n].get("version") and os.path.isabs(tools[n].get("path", "")) for n in ("mpv", "ffmpeg")), tools)
    ok("they show only that: no note, no steps", not any(tools[n].get(k) for n in ("mpv", "ffmpeg") for k in ("update", "steps")), tools)
    y = tools["yt-dlp"]
    ok("yt-dlp from elsewhere shows no update line; missing, how to install it", not y.get("update") if y.get("path") else
       y.get("update") == "Not found." and y.get("steps", [""])[-1].startswith("Restart Watchparty"), y)

    print("\n[W10] a relay party from the window: mpv starts under the windowsgui parent")
    for n in (H, G):
        must(f"{n} saved its settings", save_settings(n, mpvFlags=MPVFLAGS, **({"createKey": "k"} if n == H else {})) == "Saved.", ui(n)["settings"]["saved"])
    fill(H, hostname=H, file=MEDIA, relay=RELAY_URL)
    go(H)
    must(f"{H} is in", until(lambda: status(H).startswith("You're in"), BOOT), status(H))
    nodes[H]["pipe"] = mpv_pipe(nodes[H]["proc"].pid)
    ok(f"{H}'s mpv.exe answers", bool(nodes[H]["pipe"]) and get(H, "pause") is True)
    until(lambda: ui(H)["invite"] != "")
    fill(G, invite=ui(H)["invite"])
    fill(G, hostname=G, file=MEDIA)
    go(G)
    must(f"{G} is in", until(lambda: status(G).startswith("You're in"), BOOT), status(G))
    nodes[G]["pipe"] = mpv_pipe(nodes[G]["proc"].pid)

    print(f"\n[W11] {G}'s resume reaches {H}, and both feeds say so")
    setp(G, "pause", False)
    until(lambda: get(H, "pause") is False and f"resumed by {G}" in feed(H) and feed(G)[:1] == ["resumed by you"])
    ok(f"{H} plays", get(H, "pause") is False)
    ok("the feeds", f"resumed by {G}" in feed(H) and feed(G)[:1] == ["resumed by you"], f"{feed(H)} / {feed(G)}")

    print(f"\n[W16] {H} follows the app mode to dark and back, its own title bar and frame too")
    was = app_mode(True)
    try:
        ok(f"{H} turned dark", until(lambda: ui(H)["dark"], 10))
        ok("its native frame too", until(lambda: frame_dark(H) is True, 5), str(frame_dark(H)))
        app_mode(False)
        ok(f"{H} turned light again", until(lambda: not ui(H)["dark"], 10))
        ok("its native frame too", until(lambda: frame_dark(H) is False, 5), str(frame_dark(H)))
    finally:
        app_mode(None if was is None else not was)

    print(f"\n[W12] {G} ends the party from its window")
    pipe = nodes[G]["pipe"]
    end(G)
    ok(f"{G} says it ended", status(G) == "The party has ended.", status(G))
    ok(f"{G}'s mpv.exe is gone", until(lambda: not exists(pipe)))
    ok(f"{H} was told", until(lambda: f"{G} has left" in feed(H)), str(feed(H)))

    print(f"\n[W13] closing {H}'s window ends its GUI and its mpv")
    pipe = nodes[H]["pipe"]
    must(f"{H} has a window to close", close_window(H) > 0)
    ok(f"{H} exited", until(lambda: nodes[H]["proc"].poll() is not None))
    ok("on a closed window", "gui: window closed" in log(H))
    ok(f"{H}'s mpv.exe is gone", until(lambda: not exists(pipe)))
    kill_tree(nodes[G]["proc"].pid)

    print("\n[W15] self-update: the running .exe becomes .old, Restart runs v0.0.2, the next start removes .old")
    rel = Release(PORTS["release"], GUI_NEW, "watchparty-gui.exe", good=True)
    os.makedirs(os.path.join(RUN, "upd"))
    exe = os.path.join(RUN, "upd", "watchparty-gui.exe")
    shutil.copy(GUI_OLD, exe)
    U = launch_gui("winUpdate", exe=exe)
    about = lambda: ui(U)["about"]
    until(lambda: about()["button"])
    ok("it offers v0.0.2", about()["button"] == "Update to v0.0.2", str(about()))
    tab(U, "settings")
    click(U, "update")
    until(lambda: ui(U)["dialog"] or about()["version"].startswith("Update failed"), 60)
    ok("installed", ui(U)["dialog"] and sha(exe) == rel.sha, about()["version"])
    ok("the running binary was moved to .old", os.path.exists(exe + ".old") and sha(exe + ".old") == sha(GUI_OLD))
    old_pid = nodes[U]["proc"].pid
    click(U, "dialog:ok")
    ok("the old one exited", until(lambda: nodes[U]["proc"].poll() is not None))
    attach(U)
    ok("the new window runs v0.0.2", until(lambda: about()["version"] == "Version v0.0.2"), about()["version"])
    new = [p for p in processes() if (p["CommandLine"] or "").strip('" ').lower().startswith(exe.lower()) and p["ProcessId"] != old_pid]
    for p in new:
        kill_tree(p["ProcessId"])
    nodes[U]["proc"] = subprocess.Popen([exe], env=gui_env(nodes[U]["home"]))
    ok("the next start removes .old", until(lambda: not os.path.exists(exe + ".old")))
    kill_tree(nodes[U]["proc"].pid)
    relay.kill()

    print("\n[W15b] the cli's -update swaps its own running .exe too")
    rel = Release(PORTS["cliRelease"], CLI_NEW, "watchparty.exe", good=True)
    os.makedirs(os.path.join(RUN, "updcli"))
    exe = os.path.join(RUN, "updcli", "watchparty.exe")
    shutil.copy(CLI_OLD, exe)
    res = subprocess.run([exe, "-update"], capture_output=True, text=True, timeout=BOOT)
    ok("installed", res.returncode == 0 and sha(exe) == rel.sha, res.stderr)
    ok("the running binary was moved to .old", os.path.exists(exe + ".old") and sha(exe + ".old") == sha(CLI_OLD))
    res = subprocess.run([exe, "-version"], capture_output=True, text=True, timeout=BOOT)
    ok("and it runs as v0.0.2", res.stdout.strip() == "v0.0.2", res.stdout)

    print("\n[W16] the installer: per user, silent, mpv, ffmpeg and yt-dlp, and its uninstaller leaves nothing")
    def setup(version, env=None):
        out = os.path.join(RUN, f"watchparty-gui-setup-{version}.exe")
        subprocess.run([MAKENSIS, "/V2", f"/DEXE={GUI_BIN}", f"/DOUT={out}", f"/DVERSION={version}", os.path.join(REPO, "gui", "setup.nsi")], cwd=REPO, check=True)
        code = subprocess.run([out, "/S", f"/D={target}"], env=env, timeout=600).returncode
        crash = "(Get-WinEvent -FilterHashtable @{LogName='Application'; Id=1000} -MaxEvents 1).Message"
        must(f"the {version} installer exits 0", code == 0, "" if code == 0 else f"{code:#x} " + subprocess.run(["powershell", "-NoProfile", "-Command", crash], capture_output=True, text=True, timeout=BOOT).stdout)

    def installed(name):
        with winreg.OpenKey(winreg.HKEY_CURRENT_USER, key) as k:
            return winreg.QueryValueEx(k, name)[0]

    def app_id(lnk):
        ps = f"(New-Object -ComObject Shell.Application).NameSpace('{os.path.dirname(lnk)}').ParseName('{os.path.basename(lnk)}').ExtendedProperty('System.AppUserModel.ID')"
        return subprocess.run(["powershell", "-NoProfile", "-Command", ps], capture_output=True, text=True, timeout=BOOT).stdout.strip()

    def user_path():
        try:
            with winreg.OpenKey(winreg.HKEY_CURRENT_USER, "Environment") as k:
                return [d for d in winreg.QueryValueEx(k, "Path")[0].split(";") if d]
        except FileNotFoundError:
            return []

    target = os.path.join(RUN, "installed")
    key = r"Software\Microsoft\Windows\CurrentVersion\Uninstall\watchparty"
    tools = {"mpv": r"mpv\mpv.exe", "ffmpeg": r"ffmpeg\bin\ffmpeg.exe", "yt-dlp": r"yt-dlp\yt-dlp.exe"}
    tool = lambda t: os.path.join(target, tools[t])
    on_path = {t for t in tools if shutil.which(t)}
    path_before = user_path()
    bare = os.pathsep.join(os.path.join(os.environ["SystemRoot"], d) for d in ("System32", "", r"System32\WindowsPowerShell\v1.0"))
    offline = "http://127.0.0.1:9"
    out = os.path.join(RUN, "watchparty-gui-setup-offline.exe")
    subprocess.run([MAKENSIS, "/V2", f"/DEXE={GUI_BIN}", f"/DOUT={out}", "/DVERSION=v0.0.9", os.path.join(REPO, "gui", "setup.nsi")], cwd=REPO, check=True)
    code = subprocess.run([out, "/S", f"/D={target}"], env=environ(PATH=bare, HTTPS_PROXY=offline), timeout=BOOT).returncode
    ok("mpv failing to download, silent, cancels the install", code != 0, f"exit {code}")
    ok("and leaves nothing behind", not os.path.exists(target) and user_path() == path_before, str(os.path.exists(target) and os.listdir(target)))
    setup("v0.0.9", environ(HTTPS_PROXY=offline))
    ok("with mpv on PATH, ffmpeg or yt-dlp failing to download, silent, goes on without them", not any(os.path.exists(tool(t)) for t in tools), os.listdir(target))
    exe = os.path.join(target, "watchparty-gui.exe")
    ok("the exe is the one it was built from", sha(exe) == sha(GUI_BIN))
    menu = os.path.join(os.environ["APPDATA"], r"Microsoft\Windows\Start Menu\Programs\Watchparty.lnk")
    desktop = os.path.join(os.path.expanduser("~"), "Desktop", "Watchparty.lnk")
    ok("Start menu and desktop shortcuts", os.path.exists(menu) and os.path.exists(desktop))
    ok("both carry the id the GUI gives its window, so a pinned one groups it", app_id(menu) == app_id(desktop) == "mhashemm.watchparty", (app_id(menu), app_id(desktop)))
    ok("Add/Remove Programs points at its uninstaller", os.path.join(target, "uninstall.exe") in installed("UninstallString"))
    ok("and knows its version and folder", installed("DisplayVersion") == "v0.0.9" and installed("InstallLocation") == target, (installed("DisplayVersion"), installed("InstallLocation")))
    stray = os.path.join(RUN, "mpv.exe")
    open(stray, "w").close()
    setup("v0.0.1", environ(PATH=bare))
    os.remove(stray)
    ok("an older installer, silent, answers yes to replacing a newer one", installed("DisplayVersion") == "v0.0.1", installed("DisplayVersion"))
    ok("with nothing on PATH, and an mpv.exe next to the installer that isn't on it, it downloads mpv, ffmpeg and yt-dlp", all(os.path.exists(tool(t)) for t in tools), os.listdir(target))
    ok("their folders are on the user's PATH", all(os.path.dirname(tool(t)) in user_path() for t in tools), user_path())
    res = subprocess.run([tool("yt-dlp"), "-v"], env=environ(PATH=os.pathsep.join(user_path() + [bare])), capture_output=True, text=True, timeout=BOOT)
    ok("yt-dlp finds that ffmpeg on the user's PATH", "exe versions: ffmpeg" in res.stdout + res.stderr, (res.stdout + res.stderr)[-2000:])
    res = subprocess.run([tool("mpv"), "--version"], capture_output=True, text=True, timeout=BOOT)
    ok("and mpv runs", res.returncode == 0 and res.stdout.startswith("mpv"), res.stdout + res.stderr)
    for left in (exe + ".old", os.path.join(target, ".watchparty-update-1"), os.path.join(target, ".watchparty-gui-update-1")):
        open(left, "w").close()
    subprocess.run([os.path.join(target, "uninstall.exe"), "/S"], check=True, timeout=BOOT)
    ok("uninstalling removes the folder, mpv, ffmpeg and yt-dlp, and a self-update's leftovers", until(lambda: not os.path.exists(target)), str(os.path.exists(target) and os.listdir(target)))
    # the uninstaller runs from a copy of itself, so /S returns at once, and it removes the
    # folder before the shortcuts and its Add/Remove Programs entry (setup.nsi): wait for each
    ok("and both shortcuts", until(lambda: not os.path.exists(menu) and not os.path.exists(desktop)))
    ok("and gives the user's PATH back as it was", user_path() == path_before, (path_before, user_path()))
    def unlisted():
        try:
            winreg.OpenKey(winreg.HKEY_CURRENT_USER, key).Close()
            return False
        except FileNotFoundError:
            return True
    ok("and its Add/Remove Programs entry", until(unlisted))


# ------------------------------------------------------------------ main --

need("go")
need("mpv")
need("ffmpeg")
MAKENSIS = shutil.which("makensis") or os.path.join(os.environ.get("ProgramFiles(x86)", ""), r"NSIS\makensis.exe")
if not os.path.exists(MAKENSIS):
    sys.exit(f"makensis is required, looked at {MAKENSIS}")
print(f"run directory: {RUN}")
BIN = build("watchparty.exe", ".")
RELAY_BIN = build("relay.exe", "./relay")
release = f"-X github.com/mhashemm/watchparty/update.ReleaseURL=http://127.0.0.1:{PORTS['release']}/latest"
GUI_BIN = build("watchparty-gui.exe", "./gui", "-H=windowsgui")
GUI_OLD = build("gui-v0.0.1.exe", "./gui", f"-H=windowsgui -X main.version=v0.0.1 {release}")
GUI_NEW = build("gui-v0.0.2.exe", "./gui", f"-H=windowsgui -X main.version=v0.0.2 {release}")
release = f"-X github.com/mhashemm/watchparty/update.ReleaseURL=http://127.0.0.1:{PORTS['cliRelease']}/latest"
CLI_OLD = build("cli-v0.0.1.exe", ".", f"-X main.version=v0.0.1 {release}")
CLI_NEW = build("cli-v0.0.2.exe", ".", f"-X main.version=v0.0.2 {release}")
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
    # anything left that was started from the run directory: a restarted GUI, an mpv
    for p in processes():
        if RUN.lower() in (p["CommandLine"] or "").lower() and p["ProcessId"] != os.getpid():
            kill_tree(p["ProcessId"])

print(f"\nlogs: {RUN}")
print("ALL PASS" if not fails else f"{len(fails)} FAILURES: " + ", ".join(fails))
sys.exit(1 if fails else 0)
