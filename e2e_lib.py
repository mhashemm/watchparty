"""What e2e.py and e2e_windows.py share. Stdlib only."""
import base64, json, os, socket, time, urllib.parse


def until(cond, timeout):
    """Poll cond until it holds or timeout passes, and return whether it held."""
    deadline = time.time() + timeout
    while not cond():
        if time.time() > deadline:
            return False
        time.sleep(0.05)
    return True


class CDP:
    """A page over the Chrome DevTools Protocol: a websocket client just big enough
    for Runtime.evaluate, so the suite needs nothing outside the stdlib."""

    def __init__(self, url):
        u = urllib.parse.urlparse(url)
        self.s = socket.create_connection((u.hostname, u.port), timeout=10)
        key = base64.b64encode(os.urandom(16)).decode()
        self.s.sendall((f"GET {u.path} HTTP/1.1\r\nHost: {u.netloc}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                        f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n").encode())
        self.f = self.s.makefile("rb")
        while self.f.readline() not in (b"\r\n", b""):
            pass
        self.id = 0

    def call(self, method, **params):
        self.id += 1
        data = json.dumps({"id": self.id, "method": method, "params": params}).encode()
        n, mask = len(data), os.urandom(4)
        size = bytes([0x80 | n]) if n < 126 else bytes([0x80 | 126]) + n.to_bytes(2, "big") if n < 65536 else bytes([0x80 | 127]) + n.to_bytes(8, "big")
        self.s.sendall(b"\x81" + size + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(data)))
        while True:
            m = json.loads(self.recv())
            if m.get("id") == self.id:
                if "error" in m:
                    raise RuntimeError(m["error"])
                return m["result"]

    def recv(self):
        out = b""
        while True:
            b0, b1 = self.f.read(2)
            n = b1 & 0x7F
            if n >= 126:
                n = int.from_bytes(self.f.read(2 if n == 126 else 8), "big")
            out += self.f.read(n)
            if b0 & 0x80:
                return out

    def js(self, expr):
        r = self.call("Runtime.evaluate", expression=expr, returnByValue=True, awaitPromise=True)
        if "exceptionDetails" in r:
            raise RuntimeError(r["exceptionDetails"])
        return r["result"].get("value")
