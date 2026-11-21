"""The upnp phase: a fake router on the LAN."""
from e2e_core import *
import e2e_core


def upnp_suite():
    print("\n=== upnp: a fake router on the LAN ===")
    e2e_core.BUS = private_bus()
    if not os.environ.get("E2E_NO_SANDBOX"):
        print("\n[G34] skipped outside the container: a real router on this LAN would answer and map ports")
    else:
        # the LAN has one router, so the cases that ask it take turns (group "router"), and
        # each sets the address it reports itself
        gw = Gateway(PORTS["gateway"], "10.172.178.175")

        @case("[G34] UPnP behind carrier NAT: the party starts, warns, and removes its mapping", group="router")
        def _():
            gw.ip = "10.172.178.175"
            n = launch("upnpCarrier", port=PORTS["upnpNode"], local=False)
            must("it starts", until(lambda: booted(n), BOOT), log(n))
            ok("the mapping was added", "AddPortMapping" in gw.actions, str(gw.actions))
            ok("it warns", "unreachable address: 10.172.178.175 " in log(n), log(n))
            ok("and still prints the invite, for friends on the LAN", "your address to share is -addrs 10.172.178.175:" in log(n), log(n))
            stop(n)
            ok("quitting removes the mapping", "DeletePortMapping" in gw.actions and "upnp: port mapping deleted" in log(n), str(gw.actions))

        @case("[G34b] a public address gets no warning", group="router")
        def _():
            gw.ip = "203.0.113.7"
            n = launch("upnpPublic", port=PORTS["upnpNode"], local=False)
            must("it starts", until(lambda: booted(n), BOOT), log(n))
            ok("the invite has it", "your address to share is -addrs 203.0.113.7:" in log(n), log(n))
            ok("and no warning", "unreachable address" not in log(n), log(n))
            stop(n)

        @case("[G34c] a gateway can't start a log line of its own through the address it reports", group="router")
        def _():
            gw.ip = "10.0.0.1&#10;2026/01/01 00:00:00 FORGED"
            n = launch("upnpForged", port=PORTS["upnpNode"], local=False)
            must("it starts", until(lambda: booted(n), BOOT), log(n))
            ok("an address that isn't one warns too", "unreachable address: 10.0.0.1 2026/01/01" in log(n), log(n))
            ok("and nothing it sent starts a line", not [l for l in log(n).splitlines() if l.startswith("2026/01/01")], log(n))
            stop(n)

        @case("[G34d] stopping during discovery doesn't wait out the search, and isn't an error", group="router")
        def _():
            calls = len(gw.actions)
            n = launch("upnpStopped", port=PORTS["upnpNode"], local=False)
            must("it reached discovery", until(lambda: "file fingerprint" in log(n), BOOT), log(n))
            began = time.time()
            stop(n)
            took = time.time() - began
            ok("it stops well inside the 6.5s search", took < 4, f"{took:.1f}s")
            ok("with exit code 0", nodes[n]["proc"].returncode == 0, str(nodes[n]["proc"].returncode))
            ok("and no error, and nothing mapped", "canceled" not in log(n) and "AddPortMapping" not in gw.actions[calls:], log(n))

        @case("[G35] the GUI says so too, and only for the party it's true of; its Stop during discovery ends the party, without a diagnosis", group="router")
        def _():
            gw.ip = "10.172.178.175"
            P = launch_gui("guiUpnp")
            try:
                ok(f"{P} saved", save_settings(P, mpvFlags=MPVFLAGS) == "Saved.", js(P, '$("settings-saved").textContent'))
                js(P, 'setMode("direct"); $("local").checked = false')
                start(P, hostname=P, file=MEDIA, port=str(PORTS["upnpNode"]))
                must("it starts", until(lambda: status(P).startswith("You're in"), BOOT), status(P))
                ok("the status warns", status(P) == "You're in, but friends outside this network can't reach this invite: your connection has no public address. Use a relay for them.", status(P))
                until(lambda: js(P, '$("share-text").textContent'))
                ok("and the invite still shows", js(P, '$("share-text").textContent').startswith("-addrs 10.172.178.175:"), js(P, '$("share-text").textContent'))
                end(P)
                gw.ip = "203.0.113.7"
                go(P)
                must("the next party starts", until(lambda: status(P).startswith("You're in"), BOOT), status(P))
                ok("with a public address it doesn't warn", status(P) == "You're in. Pause or seek in mpv and everyone follows.", status(P))
                end(P)

                part("[G35b] the GUI's Stop during discovery ends the party, without a diagnosis")
                go(P)
                began = time.time()
                end(P)
                took = time.time() - began
                ok("it says it ended", status(P) == "The party has ended.", status(P))
                ok("well inside the 6.5s search", took < 4, f"{took:.1f}s")
            finally:
                stop(P)

    pidfile = f"{RUN}/hungup.pid"
    with open(f"{RUN}/hungup.py", "w") as f:
        f.write(f"""import json, os, socket, sys, time
open({pidfile!r}, "w").write(str(os.getpid()))
path = next(a.split("=", 1)[1] for a in sys.argv if a.startswith("--input-ipc-server="))
s = socket.socket(socket.AF_UNIX)
s.bind(path)
s.listen()
c = s.accept()[0]
r = c.makefile("rb")
for _ in range(4):
    c.sendall(json.dumps({{"request_id": json.loads(r.readline())["request_id"], "error": "success"}}).encode() + b"\\n")
r.close()
c.close()
time.sleep(600)
""")
    script(f"{RUN}/hungup", f'exec python3 {RUN}/hungup.py "$@"')

    @case("[G36] mpv's socket dropping while mpv stays alive ends the party with a reason, not a hang")
    def _():
        n = launch("hungUp", port=free_port(), mpv=f"{RUN}/hungup")
        ok("it exits with an error", until(lambda: nodes[n]["proc"].poll() is not None, BOOT) and nodes[n]["proc"].returncode == 1, str(nodes[n]["proc"].poll()))
        ok("that says what happened", "mpv stopped answering" in log(n), log(n))
        ok("and takes mpv with it", until(lambda: gone(int(open(pidfile).read()))))

    run_cases()
