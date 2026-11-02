"""What e2e.py and e2e_windows.py share. Stdlib only."""
import base64, hashlib, http.server, json, os, socket, threading, time, urllib.parse, urllib.request


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


def find_page(line):
    """The DevTools websocket of the app window a stand-in browser was launched for;
    line is the arguments it logged. None until the page is up."""
    argv = dict(a.split("=", 1) for a in line.split() if a.startswith("--") and "=" in a)
    try:
        with open(os.path.join(argv["--user-data-dir"], "DevToolsActivePort")) as f:
            port = f.read().split()[0]
        tabs = json.load(urllib.request.urlopen(f"http://127.0.0.1:{port}/json/list", timeout=1))
    except (OSError, ValueError, IndexError, KeyError):
        return None
    return next((t["webSocketDebuggerUrl"] for t in tabs if t["type"] == "page" and t["url"] == argv.get("--app")), None)


class Page(CDP):
    """The GUI's page, driven the way a person would, plus the few hooks a suite needs."""

    def ready(self, timeout, downloads=None):
        self.call("Browser.grantPermissions", permissions=["clipboardReadWrite", "clipboardSanitizedWrite"])
        if downloads:
            self.call("Browser.setDownloadBehavior", behavior="allow", downloadPath=downloads)
        # the first loadState fills the form; filling it before then would be overwritten
        until(lambda: self.js('document.getElementById("store-path")?.textContent') not in (None, ""), timeout)
        # a real confirm() waits for a click forever; this one says yes and keeps the question.
        # loadState()s in flight are counted: a late one refills the form with what the
        # store had when it asked, over anything filled in since, so tab() waits for them.
        self.js("window.confirm = q => (window.asked = q, true); window.loading = 0; window.loads = 0; const real = loadState; "
                "loadState = async () => { window.loading++; window.loads++; try { await real() } finally { window.loading-- } }")
        # these hooks lean on the page's globals; if a refactor moves them, fail here, loudly
        hooked = self.js("window.confirm('?') === true && window.asked === '?' && typeof api === 'function' && typeof run === 'number'")
        if not hooked:
            raise RuntimeError("the page no longer has the globals e2e hooks into (loadState, api, run, confirm)")

    def status(self):
        return self.js('$("status").textContent')

    def feed(self):
        """The party feed, newest first."""
        return self.js('[...$("feed").children].filter(li => !li.classList.contains("empty")).map(li => li.lastChild.textContent)')

    def click(self, selector):
        self.js(f"document.querySelector({json.dumps(selector)}).click()")

    def fill(self, **fields):
        self.js(f'for (const [k, v] of Object.entries({json.dumps(fields)})) $(k).value = v')

    def tab(self, view, timeout=10):
        loads = self.js("window.loads")
        self.click(f".tabs button[data-view={view}]")
        if view == "history" and not until(lambda: self.js("window.loads") > loads, timeout):
            raise RuntimeError("the History tab no longer calls loadState(); the e2e hook in Page.ready is stale")
        until(lambda: self.js("window.loading") == 0, timeout)

    def go(self, timeout):
        """Press the big button and wait for the new run, or a check reads the last party's status."""
        run = self.js("run")
        self.click("#go")
        until(lambda: self.js("run") > run, timeout)

    def state(self):
        return self.js('api("/state").then(r => r.json())')


class Release:
    """GitHub's releases/latest for the self-update cases, offering v0.0.2 as name.
    digest starts wrong unless good; set it to "sha256:" + .sha to make it right."""

    def __init__(self, port, asset, name, good=False):
        with open(asset, "rb") as f:
            data = f.read()
        self.sha = hashlib.sha256(data).hexdigest()
        self.digest = "sha256:" + (self.sha if good else "0" * 64)
        rel = self

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                body = data if self.path == "/asset" else json.dumps({"tag_name": "v0.0.2", "assets": [
                    {"name": name, "browser_download_url": f"http://127.0.0.1:{port}/asset", "digest": rel.digest}]}).encode()
                self.send_response(200)
                self.send_header("content-length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *a):
                pass
        threading.Thread(target=http.server.ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever, daemon=True).start()


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()
