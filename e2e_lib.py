"""What e2e.py and e2e_windows.py share. Stdlib only."""
import hashlib, http.server, json, os, socket, threading, time


def until(cond, timeout):
    """Poll cond until it holds or timeout passes, and return whether it held."""
    deadline = time.time() + timeout
    while not cond():
        if time.time() > deadline:
            return False
        time.sleep(0.05)
    return True


class Window:
    """A GUI's window through its e2e control socket (gui/e2e.go): pressed and filled as a
    person would, and read back as it shows. addr is the file the GUI writes its address to."""

    def __init__(self, addr):
        self.addr = addr

    def call(self, op, **params):
        with open(self.addr) as f:
            host, port = f.read().rsplit(":", 1)
        with socket.create_connection((host, int(port)), timeout=30) as s:
            s.sendall((json.dumps({"op": op, **params}) + "\n").encode())
            line = s.makefile("rb").readline()
        if not line:
            raise RuntimeError(f"the window closed its control socket during {op}")
        r = json.loads(line)
        if r.get("error"):
            raise RuntimeError(f"{op} {params}: {r['error']}")
        return r.get("ok")

    def ui(self):
        return self.call("ui")

    def log(self):
        return self.call("log")

    def click(self, name):
        self.call("click", name=name)

    def fill(self, **fields):
        self.call("fill", fields={k: str(v).lower() if isinstance(v, bool) else v for k, v in fields.items()})

    def status(self):
        return self.ui()["status"]

    def feed_groups(self):
        """The feed's videos, latest first: [name, open, its entries, newest first]."""
        return [[g["name"], g["open"], g["items"]] for g in self.ui()["feed"]]

    def feed(self):
        """The party feed, newest first."""
        return [i for g in self.ui()["feed"] for i in g["items"]]

    def tab(self, view):
        self.click("tab:" + view)

    def go(self, timeout):
        """Press the big button and wait for the new run, or a check reads the last party's status."""
        run = self.ui()["run"]
        self.click("go")
        until(lambda: self.ui()["run"] > run, timeout)


def store(config):
    """A GUI's saved settings and history, read from its files under config (XDG_CONFIG_HOME
    or %APPDATA%)."""
    out = {"settings": {}, "history": []}
    for key, name in (("settings", "gui.json"), ("history", "history.json")):
        try:
            with open(os.path.join(config, "watchparty", name), encoding="utf-8") as f:
                v = json.load(f)
        except (OSError, ValueError):
            continue
        out[key] = (v.get("settings") or {}) if key == "settings" else (v or [])
    return out


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

            def log_message(self, format, *args):
                pass
        threading.Thread(target=http.server.ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever, daemon=True).start()


class Gateway:
    """A UPnP router on the LAN: answers SSDP, and SOAP on port. ip is the external address
    it reports, and actions lists each SOAP action it got. Binds SSDP's fixed port, so one
    per machine, and a real router on the LAN would answer too: containers only."""

    def __init__(self, port, ip):
        self.ip, self.actions = ip, []
        probe = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        probe.connect(("6.9.6.9", 6969))
        local = probe.getsockname()[0]
        probe.close()
        gw = self
        soap = '<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>%s</s:Body></s:Envelope>'
        service = "urn:schemas-upnp-org:service:WANIPConnection:1"

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.reply('<?xml version="1.0"?><scpd><actionList>'
                           "<action><name>AddPortMapping</name></action><action><name>DeletePortMapping</name></action>"
                           "<action><name>GetExternalIPAddress</name></action></actionList></scpd>" if self.path == "/scpd" else
                           '<?xml version="1.0"?><root xmlns="urn:schemas-upnp-org:device-1-0"><device>'
                           "<deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType><modelName>e2e</modelName>"
                           f"<serviceList><service><serviceType>{service}</serviceType><SCPDURL>/scpd</SCPDURL><controlURL>/ctl</controlURL></service></serviceList>"
                           "</device></root>")

            def do_POST(self):
                self.rfile.read(int(self.headers["content-length"]))
                action = self.headers["soapaction"].strip('"').split("#")[1]
                gw.actions.append(action)
                body = f"<NewExternalIPAddress>{gw.ip}</NewExternalIPAddress>" if action == "GetExternalIPAddress" else ""
                self.reply(soap % f'<u:{action}Response xmlns:u="{service}">{body}</u:{action}Response>')

            def reply(self, text):
                self.send_response(200)
                self.send_header("content-length", str(len(text.encode())))
                self.end_headers()
                self.wfile.write(text.encode())

            def log_message(self, format, *args):
                pass
        threading.Thread(target=http.server.ThreadingHTTPServer((local, port), H).serve_forever, daemon=True).start()

        ssdp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        ssdp.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        ssdp.bind(("", 1900))
        ssdp.setsockopt(socket.IPPROTO_IP, socket.IP_ADD_MEMBERSHIP, socket.inet_aton("239.255.255.250") + socket.inet_aton(local))

        def answer():
            while True:
                data, sender = ssdp.recvfrom(4096)
                if data.startswith(b"M-SEARCH"):
                    ssdp.sendto(f"HTTP/1.1 200 OK\r\nST: upnp:rootdevice\r\nLOCATION: http://{local}:{port}/desc.xml\r\n\r\n".encode(), sender)
        threading.Thread(target=answer, daemon=True).start()


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()
