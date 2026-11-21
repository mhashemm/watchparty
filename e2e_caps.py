"""The caps phase: one fresh relay per case, for every abuse cap."""
from e2e_core import *


def caps_suite():
    """The abuse caps, driven with real nodes rather than the Go handler tests. Each case
    runs a relay of its own, with the cap it checks, on a port of its own."""
    print("\n=== relay caps: a relay of its own for every case ===")

    @contextlib.contextmanager
    def own_relay(extra=None, port=None):
        port = port or free_port()
        p = launch_relay(extra, port)
        until(lambda: listening(port))
        try:
            yield port, f"ws://127.0.0.1:{port}"
        finally:
            p.kill()
            p.wait(timeout=10)

    @case("[R16] only room CREATION is rate limited, joining is free")
    def _():
        with own_relay({"ROOMS_PER_HOUR": "1"}) as (_, url):
            launch(A, room="capA", tag="c1", relay=url)
            until(lambda: connected("c1"), BOOT)
            launch("nodeB", room="capB", tag="c2", relay=url)
            until(lambda: "429" in log("c2"), BOOT)
            ok("the second NEW room was refused", "429" in log("c2"), "ROOMS_PER_HOUR=1 must bite")
            ok("and its log says why, and that it keeps trying", "relay: dial: 429 Too Many Requests: the relay has had too many attempts from your network" in log("c2")
               and nodes["c2"]["proc"].poll() is None, log("c2")[-300:])
            launch("nodeC", room="capA", tag="c3", relay=url)
            until(lambda: connected("c3"), BOOT)
            ok("joining the EXISTING room still works", connected("c3"),
               "a party of eight must not exhaust one person's quota")
            for t in ("c1", "c2", "c3"):
                stop(t)

    @case("[R17] MAX_ROOMS refuses a second room with 503")
    def _():
        with own_relay({"MAX_ROOMS": "1"}) as (_, url):
            launch(A, room="roomOne", tag="m1", relay=url)
            until(lambda: connected("m1"), BOOT)
            launch("nodeB", room="roomTwo", tag="m2", relay=url)
            until(lambda: "503" in log("m2"), BOOT)
            ok("the first room connected", connected("m1"))
            ok("the second room got 503", "503" in log("m2"), "MAX_ROOMS=1 must bite")
            ok("and its log says the relay is full", "relay: dial: 503 Service Unavailable: the relay is full" in log("m2"), log("m2")[-300:])
            for t in ("m1", "m2"):
                stop(t)

    @case("[R18] MAX_CONNS refuses the third connection with 503")
    def _():
        with own_relay({"MAX_CONNS": "2"}) as (_, url):
            for i, t in enumerate(("n1", "n2")):
                launch(NODES[i], room="connroom", tag=t, relay=url)
                until(lambda: connected(t), BOOT)
            launch(NODES[2], room="connroom", tag="n3", relay=url)
            until(lambda: "503" in log("n3"), BOOT)
            ok("the first two connected", connected("n1") and connected("n2"))
            ok("the third got 503", "503" in log("n3"), "MAX_CONNS=2 must bite")
            for t in ("n1", "n2", "n3"):
                stop(t)

    @case("[R19a] a relay without CREATE_KEY refuses to start")
    def _():
        port = free_port()
        relay = launch_relay({"CREATE_KEY": ""}, port)
        ok("it exits at once and says why", relay.wait(timeout=10) != 0 and "CREATE_KEY is required" in relay_log(port), relay_log(port)[-300:])

    @case("[R19] -createKey: only the key holder opens a room, the rest just join")
    def _():
        with own_relay() as (_, url):
            launch("nodeB", room="", tag="k0", create_key="", relay=url)
            nodes["k0"]["proc"].wait(timeout=30)
            ok("opening a new room without the key is refused, and the node exits", "needs -createKey" in log("k0") and "not open yet" not in log("k0") and nodes["k0"]["proc"].returncode != 0, log("k0")[-300:])
            launch("nodeB", room="keyroom", tag="k1", create_key="", relay=url)
            until(lambda: "not open yet" in log("k1"), BOOT)
            ok("a keyless node waits for the room instead of giving up",
               "not open yet" in log("k1") and nodes["k1"]["proc"].poll() is None)
            launch("nodeC", room="keyroom", tag="k2", create_key="wrong", relay=url)
            nodes["k2"]["proc"].wait(timeout=30)
            ok("a wrong key is refused and the node exits", "403" in log("k2"))
            launch(A, room="keyroom", tag="k3", create_key="k", relay=url)
            until(lambda: connected("k3"), BOOT)
            ok("the key holder opened the room", connected("k3"))
            until(lambda: connected("k1"), 35)  # the keyless node's backoff may be up to 16s by now
            ok("the waiting node got in once the room was open", connected("k1"))
            for t in ("k1", "k2", "k3"):
                stop(t)

    @case("[R19b] a room or hostname can't forge a line in the relay's log")
    def _():
        with own_relay() as (port, _):
            def ws_get(query, headers):
                conn = http.client.HTTPConnection("127.0.0.1", port, timeout=SETTLE)
                conn.request("GET", "/ws?" + query, headers=headers)
                code = conn.getresponse().status
                conn.close()
                return code
            code = ws_get("room=inj%0aFORGED1&hostname=h&id=i&session=s&file=f", {"create-key": "wrong"})
            ok("a bad create key is refused", code == 403, f"HTTP {code}")
            code = ws_get("room=inj%0aFORGED2&hostname=evil%0aFORGED3&id=i&session=s&file=f",
                          {"create-key": "k", "room-secret": "s", "Connection": "Upgrade", "Upgrade": "websocket",
                           "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="})
            ok("the key holder gets in", code == 101, f"HTTP {code}")
            until(lambda: relay_event("left", "injFORGED2", port))
            refused, joined, left = relay_event("refused", "injFORGED1", port), relay_event("joined", "injFORGED2", port), relay_event("left", "injFORGED2", port)
            ok("both requests were logged", refused and joined and left and joined["hostname"] == "evilFORGED3", relay_log(port))
            ok("and every line is JSON, so nothing they sent starts one", not [e for e in relay_events(port) if "unparsed" in e], relay_log(port))
            ok("a connection's lines share one id", joined and left and joined["conn"] and joined["conn"] == left["conn"], relay_log(port))
            ok("another connection has another id", refused and joined and refused["conn"] != joined["conn"], relay_log(port))
            ok("how long it stayed is a number of seconds", left and type(left["seconds"]) is int, relay_log(port))

    @case("[R19c] a keyless member of a room a key holder opened before is held, across a relay restart, until a key holder opens it")
    def _():
        port, room = free_port(), "heldroom"
        with own_relay(port=port) as (_, url):
            launch(A, room=room, tag="k10", create_key="k", relay=url)
            must("a key holder opens the room", until(lambda: connected("k10"), BOOT), log("k10")[-300:])
            stop("k10")
            must("which closes when it leaves", until(lambda: relay_event("left", room, port)), relay_log(port)[-300:])
        with own_relay(port=port) as (_, url):
            since = len(relay_events(port))
            seen = lambda msg: any(e.get("msg") == msg and e.get("room") == room for e in relay_events(port)[since:])
            launch("nodeC", room=room, secret="wrong", tag="k5", create_key="", relay=url)
            nodes["k5"]["proc"].wait(timeout=30)
            ok("with the wrong secret it is refused, not kept waiting", "403" in log("k5") and "not open yet" not in log("k5"), log("k5")[-300:])
            launch("nodeB", room=room, tag="k4", create_key="", relay=url)
            until(lambda: seen("waiting for a key holder") and "not open yet" in log("k4"), BOOT)
            ok("with the right one the relay holds it", seen("waiting for a key holder") and "not open yet" in log("k4") and not connected("k4"), log("k4")[-300:])
            ok("and opens no room for it", not seen("joined"), relay_log(port)[-500:])
            held_at = time.time()
            launch(A, room=room, tag="k6", create_key="k", relay=url)
            until(lambda: connected("k6") and connected("k4"), BOOT)
            ok("once the key holder opens it, the held member joins at once, not after a backoff", connected("k4") and time.time() - held_at < 5, f"{time.time() - held_at:.1f}s")
            for t in ("k4", "k6"):
                stop(t)

    @case("[R19d] MAX_WAITING: a room holds only so many keyless members, the rest retry")
    def _():
        with own_relay({"MAX_WAITING": "1"}) as (port, url):
            # only a room a key holder opened before holds anyone: a new one says "not open yet"
            launch(A, room="waitroom", tag="k11", create_key="k", relay=url)
            must("a key holder opens the room", until(lambda: connected("k11"), BOOT), log("k11")[-300:])
            stop("k11")
            must("which closes when it leaves", until(lambda: relay_event("left", "waitroom", port)), relay_log(port)[-300:])
            launch("nodeB", room="waitroom", tag="k7", create_key="", relay=url)
            until(lambda: "not open yet" in log("k7"), BOOT)
            launch("nodeC", room="waitroom", tag="k8", create_key="", relay=url)
            until(lambda: "503" in log("k8"), BOOT)
            ok("the second is refused with 503 and keeps trying", "503" in log("k8") and nodes["k8"]["proc"].poll() is None, log("k8")[-300:])
            launch(A, room="waitroom", tag="k9", create_key="k", relay=url)
            ok("both get in once the key holder opens it", until(lambda: all(connected(n) for n in ("k7", "k8", "k9")), 40), str([connected(n) for n in ("k7", "k8", "k9")]))
            for t in ("k7", "k8", "k9"):
                stop(t)

    @case("[R20] a member that stops answering pings is dropped and the room hears it left")
    def _():
        with own_relay() as (port, url):
            launch(A, room="deafroom", tag="d1", relay=url)
            until(lambda: connected("d1"), BOOT)
            deaf = socket.create_connection(("127.0.0.1", port))
            deaf.sendall((f"GET /ws?room=deafroom&hostname=deaf&id=deaf&session=deaf&file={fingerprint('d1')} HTTP/1.1\r\nHost: 127.0.0.1\r\n"
                          f"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n"
                          f"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nroom-secret: {SECRET}\r\n\r\n").encode())
            ok("the deaf client got in", deaf.recv(4096).startswith(b"HTTP/1.1 101"))
            until(lambda: "deaf has joined" in osd("d1"))
            ok(f"{A} was told deaf joined", "deaf has joined" in osd("d1"))
            until(lambda: "deaf has left" in osd("d1"), 60)  # one 30s ping interval, the 10s pong wait and the 10s grace
            ok(f"{A} was told deaf left", "deaf has left" in osd("d1"), "the relay kept a member that never answers pings")
            deaf.close()
            stop("d1")

    run_cases()
