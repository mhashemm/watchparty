const TOKEN = "{{TOKEN}}";
const $ = id => document.getElementById(id);
const api = (path, body) => fetch(path, {
  method: body === undefined ? "GET" : "POST",
  headers: { "X-Token": TOKEN, "Content-Type": "application/json" },
  body: body === undefined ? undefined : JSON.stringify(body),
});

const FIELDS = ["hostname", "relay", "room", "secret", "createKey", "cacheDir", "port", "pport", "addrs", "mpv", "mpvFlags"];

function mode() { return document.querySelector("input[name=mode]:checked").value; }
function setMode(m) {
  document.querySelector(`input[name=mode][value=${m}]`).checked = true;
  $("relay-fields").hidden = m !== "relay";
  $("direct-fields").hidden = m !== "direct";
}
document.querySelectorAll("input[name=mode]").forEach(r => r.addEventListener("change", () => setMode(mode())));

function collect() {
  const f = { mode: mode(), file: $("file").value, local: $("local").checked, playlist: $("playlist").checked };
  for (const k of FIELDS) f[k] = $(k).value;
  if (f.mode === "direct") f.secret = $("psecret").value;
  return f;
}
let saved = { settings: {}, history: [] };
async function loadState() {
  try { saved = await (await api("/state")).json(); } catch { return; }
  const s = saved.settings || {};
  if (!$("hostname").value) $("hostname").value = s.hostname || "";
  if (!$("relay").value) $("relay").value = s.relay || "";
  if (!$("port").value) $("port").value = s.port || "";
  $("local").checked = s.local !== false;
  for (const k of ["mpv", "mpvFlags", "createKey", "cacheDir", "pport"]) $(k).value = s[k] || "";
  document.querySelector(`input[name=mpvPicker][value=${s.mpvPicker || "move"}]`).checked = true;
  $("mpv").placeholder = saved.defaultMpv;
  $("cacheDir").placeholder = saved.defaultCacheDir;
  $("set-relay").value = s.relay || "";
  $("store-path").textContent = saved.path;
  renderHistory();
}

function show(view) {
  for (const v of ["party", "history", "settings"]) $("view-" + v).hidden = v !== view;
  document.querySelectorAll(".tabs button").forEach(b => b.setAttribute("aria-current", b.dataset.view === view ? "page" : "false"));
  history.replaceState(null, "", view === "party" ? location.pathname : "#" + view);
  if (view === "settings" && !toolsChecked) checkTools();
}
let toolsChecked = false;
document.querySelectorAll(".tabs button").forEach(b => b.addEventListener("click", () => {
  show(b.dataset.view);
  if (b.dataset.view === "history") loadState();
}));

const fileName = path => /^magnet:/i.test(path || "")
  ? new URLSearchParams(path.split("?")[1]).get("dn") || "Magnet link"
  : (path || "").split(/[\\/]/).pop() || path;
const names = a => a.length < 3 ? a.join(" and ") : a.slice(0, -1).join(", ") + " and " + a.at(-1);
const clock = t => new Date(t).toLocaleTimeString([], { timeStyle: "short" });
// how many chat messages show before "All messages" (renderSaid; reset() runs at load), and
// the most kept, as gui/main.go keeps (saidKept)
const SAID_SHOWN = 3, SAID_KEPT = 500;
// the reactions (loadEmoji); each person bursts once every REACT_EVERY at most (mpv/react.go),
// so the button rests that long after reactedAt
const REACT_EVERY = 2000;
let emoji = null, reactedAt = 0;
function videoList(videos, hide) {
  const ul = document.createElement("ul");
  ul.className = "videos";
  ul.append(...videos.map(v => {
    const li = document.createElement("li");
    if (v.name !== hide) {
      const name = document.createElement("span");
      name.textContent = v.name;
      li.append(name);
    }
    const when = document.createElement("span");
    when.className = "when";
    when.textContent = v.ended ? `${clock(v.started)} to ${clock(v.ended)}` : `started ${clock(v.started)}`;
    li.append(when);
    return li;
  }));
  return ul;
}
// the full path of the file, or of its folder for a playlist; a magnet link has no path
function partyTitle(p, files) {
  const place = /^magnet:/i.test(p.file) ? fileName(p.file)
    : p.playlist && !/\.torrent$/i.test(p.file) ? p.file.replace(/[\\/][^\\/]*$/, "").replace(/^[^\\/]*$/, "")
    : p.file;
  const count = files > 1 ? `${files} videos` : "";
  return place && count ? `${place} (${count})` : place || count;
}
// the relay forgets a closed room 30 days after its last use (forgetAfter in relay/rooms.go)
const continuable = p => p.mode !== "direct" && !!p.room && !!p.secret && !p.finished
  && Date.now() - new Date(p.ended || p.started) < 30 * 864e5;
function renderHistory() {
  const parties = (saved.history || []).slice().reverse();
  $("history-empty").hidden = parties.length > 0;
  $("clear-history").hidden = parties.length === 0;
  $("history-list").replaceChildren(...parties.map(p => {
    const li = document.createElement("li");
    const title = document.createElement("b");
    const videos = p.videos || [];
    title.textContent = partyTitle(p, new Set(videos.map(v => v.name)).size);
    title.title = p.file;
    const meta = document.createElement("span");
    meta.className = "meta";
    const when = new Date(p.started).toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
    const who = p.people && p.people.length ? `with ${names(p.people)}` : "just you";
    meta.textContent = `${when}, ${who}, ${p.mode === "direct" ? "peer-to-peer" : "over the internet"}`;
    li.append(title, meta);
    if (continuable(p)) {
      const resume = document.createElement("button");
      resume.type = "button";
      resume.className = "small continue";
      resume.textContent = "Continue";
      resume.disabled = running;
      resume.addEventListener("click", () => continueParty(p));
      li.append(resume);
    }
    if (p.mode === "direct") {
      const again = document.createElement("button");
      again.type = "button";
      again.className = "small again";
      again.textContent = "Watch again";
      again.disabled = running;
      again.addEventListener("click", () => watchAgain(p));
      li.append(again);
    }
    if (videos.length) li.append(videoList(videos, p.playlist ? null : fileName(p.file)));
    return li;
  }));
}
function watchAgain(p) {
  $("file").value = p.file;
  $("playlist").checked = !!p.playlist;
  setMode("direct");
  $("addrs").value = p.addrs || "";
  $("psecret").value = p.secret || "";
  show("party");
}
async function continueParty(p) {
  $("file").value = p.file;
  $("playlist").checked = !!p.playlist;
  setMode("relay");
  $("relay").value = p.relay || $("relay").value;
  $("room").value = p.room;
  $("secret").value = p.secret;
  show("party");
  await startParty({ continue: p.started });
}
$("clear-history").addEventListener("click", async () => {
  if (!confirm("Clear the whole party history?")) return;
  await api("/history/clear", {});
  await loadState();
});

const showTip = open => {
  $("tip").hidden = !open;
  $("tip-button").setAttribute("aria-expanded", open);
};
$("tip-button").addEventListener("click", () => showTip($("tip").hidden));
document.addEventListener("keydown", e => { if (e.key === "Escape") showTip(false); });
document.addEventListener("contextmenu", e => { if (!e.target.closest("input, textarea")) e.preventDefault(); });
document.addEventListener("keydown", e => {
  const devtools = /^[ijc]$/i.test(e.key) && (e.ctrlKey && e.shiftKey || e.metaKey && e.altKey);
  const source = /^u$/i.test(e.key) && (e.ctrlKey || e.metaKey);
  const reload = e.key === "F5" || /^r$/i.test(e.key) && (e.ctrlKey || e.metaKey);
  if (e.key === "F12" || devtools || source || reload) e.preventDefault();
});
document.addEventListener("click", e => {
  if (!e.target.closest("#tip, #tip-button")) showTip(false);
});

$("settings-form").addEventListener("submit", async e => {
  e.preventDefault();
  const body = { relay: $("set-relay").value };
  for (const k of ["mpv", "mpvFlags", "createKey", "cacheDir", "pport"]) body[k] = $(k).value;
  const mpvPicker = document.querySelector("input[name=mpvPicker]:checked").value;
  const res = await api("/settings", { ...body, mpvPicker });
  $("settings-saved").textContent = res.ok ? "Saved." : `Couldn't save: ${(await res.text()).trim()}`;
  $("settings-saved").hidden = false;
  setTimeout(() => $("settings-saved").hidden = true, 3000);
  if (res.ok && !$("relay").value) $("relay").value = body.relay;
  if (!res.ok) return;
  const mpvChanged = body.mpv.trim() !== (saved.settings.mpv || "");
  for (const [k, v] of Object.entries(body)) saved.settings[k] = v.trim();
  saved.settings.mpvPicker = mpvPicker;
  if (mpvChanged) checkTools();
});

$("invite").addEventListener("input", () => {
  const text = $("invite").value.replace(/^[\s\S]*your address to share is/, "").trim();
  const t = text.split(/\s+/);
  let filled = false;
  if (t.includes("-relay")) {
    const map = { "-relay": "relay", "-room": "room", "-secret": "secret" };
    for (let i = 0; i < t.length - 1; i++) if (map[t[i]]) { $(map[t[i]]).value = t[i + 1]; filled = true; }
    setMode("relay");
  } else if (t.includes("-addrs")) {
    const map = { "-addrs": "addrs", "-secret": "psecret" };
    for (let i = 0; i < t.length - 1; i++) if (map[t[i]]) { $(map[t[i]]).value = t[i + 1]; filled = true; }
    setMode("direct");
  }
  if (filled) $("playlist").checked = t.includes("-playlist");
  $("invite-hint").hidden = !filled;
});

document.querySelectorAll("[data-pick]").forEach(b => b.addEventListener("click", async () => {
  const target = $(b.dataset.pick);
  b.disabled = true;
  try {
    const res = await api(b.dataset.route || "/pick", {});
    const path = (await res.text()).trim();
    if (!res.ok) throw new Error(path);
    if (path) target.value = path;
  } catch {
    showError("No file picker found on this computer. Type or paste the full path instead.");
  }
  b.disabled = false;
  target.focus();
}));

function showError(text) {
  $("error").textContent = text;
  $("error").hidden = !text;
}

let running = false;
$("form").addEventListener("submit", async e => {
  e.preventDefault();
  if (running) {
    if (confirm("End the party? mpv closes for you, and everyone else keeps watching.")) {
      await api("/stop", {});
    }
    return;
  }
  await startParty({});
});
async function startParty(extra) {
  showError("");
  if (!$("file").value.trim()) { showError("Pick a video first."); $("file").focus(); return; }
  plan = planFor(collect());
  const res = await api("/start", { ...collect(), ...extra });
  if (!res.ok) {
    const text = (await res.text()).trim();
    showError("Diagnosis: " + text + ".");
  }
  if (res.ok) loadState();
}

let run = 0, from = 0, saidFrom = 0, state = {}, plan = null;
function reset() {
  state = { ready: false, relay: false, connected: false, reconnecting: false, waiting: false, busy: "", unreachable: false, leftPause: false, stopping: null,
    shared: false, torrent: false, found: false, check: null, router: false, mpvUp: false, swarm: null, buffering: null, inAt: null, members: [],
    myAhead: null, aheadOf: {}, trailStep: null, trailAt: 0, trailShown: false, said: [] };
  renderSaid();
  chatHint();
  $("log").textContent = "";
  $("feed").innerHTML = '<p class="empty">Nothing yet. Joins, pauses and seeks show up here.</p>';
  $("share").hidden = true;
}
reset();

function feedGroup(name) {
  const feed = $("feed");
  feed.querySelector(".empty")?.remove();
  let group = [...feed.children].find(g => g.dataset.name === name);
  if (!group) {
    group = document.createElement("details");
    group.dataset.name = name;
    const summary = document.createElement("summary");
    summary.textContent = name;
    group.append(summary, document.createElement("ul"));
  }
  for (const g of feed.children) g.open = false;
  group.open = true;
  feed.prepend(group);
  return group;
}

function nowPlaying(name, at) {
  const previous = $("feed").querySelector("details");
  const group = feedGroup(name);
  if (previous === group || !previous) return;
  const moving = [];
  for (const li of previous.querySelectorAll("li")) {
    if (at - li.dataset.at > 1000 || !/^from .+, by |: video changed$|^synced by /s.test(li.lastChild.textContent)) break;
    moving.push(li);
  }
  if (moving.some(li => !li.lastChild.textContent.startsWith("synced by "))) group.lastChild.prepend(...moving);
}

function addFeed(text, at) {
  const list = ($("feed").querySelector("details") || feedGroup(fileName($("file").value))).lastChild;
  const li = document.createElement("li");
  const time = document.createElement("time");
  time.textContent = clock(at);
  li.dataset.at = +at;
  li.append(time, text);
  list.prepend(li);
  while (list.children.length > 20) list.lastChild.remove();
  return li;
}

function handle(line) {
  const log = $("log");
  log.textContent += line + "\n";
  if (log.textContent.length > 200000) log.textContent = log.textContent.slice(-150000);

  const [y, mo, d, h, mi, s] = line.slice(0, 19).split(/[/ :]/).map(Number);
  const msg = line.slice(20), at = new Date(y, mo - 1, d, h, mi, s);
  let m;
  if ((m = msg.match(/^your address to share is (.*)$/))) {
    $("share-text").textContent = m[1].trim();
    state.relay = /^-relay /.test(m[1].trim());
    state.shared = true;
  }
  if (/^relay: connected/.test(msg)) {
    // back after a drop, the relay names everyone still here again, and missed leaves are gone
    if (state.connected) state.members = [];
    Object.assign(state, { connected: true, reconnecting: false, waiting: false, busy: "" });
  }
  if (/^relay: (dial|disconnected)/.test(msg)) {
    state.reconnecting = true;
    state.waiting = /is not open yet/.test(msg);
    state.busy = /^relay: dial: 429 /.test(msg) ? "limited" : /^relay: dial: 503 /.test(msg) ? "full" : "";
  }
  if (/^init done/.test(msg)) Object.assign(state, { ready: true, inAt: at, swarm: null });
  if (/^torrent: finding /.test(msg)) state.torrent = true;
  if (/^torrent: found /.test(msg)) state.found = true;
  if ((m = msg.match(/^torrent: (\d+) peers?, (.+)$/))) state.swarm = { peers: +m[1], speed: m[2] };
  if ((m = msg.match(/^checking (\d+) of (\d+): (".*")$/))) {
    try { state.check = { i: +m[1], n: +m[2], name: JSON.parse(m[3]) }; } catch {}
  }
  if (/^asking the router for a port$/.test(msg)) state.router = true;
  if (/^mpv is up, /.test(msg)) state.mpvUp = true;
  if ((m = msg.match(/^buffering: (\d+)%$/))) state.buffering = +m[1];
  if (/^buffering: done$/.test(msg)) Object.assign(state, { buffering: null, swarm: null });
  // how many seconds a member that ran out has ready, while the party waits for it
  if ((m = msg.match(/^buffered ahead: (\d+)$/))) state.myAhead = +m[1];
  if ((m = msg.match(/^buffered ahead: (\d+) for (\{.*\})$/))) {
    try { const p = JSON.parse(m[2]); state.aheadOf[p.id] = { name: p.name, secs: +m[1] }; } catch {}
  }
  if ((m = msg.match(/^member (joined|here|left): (\{.*\})$/))) {
    try { member(m[1], JSON.parse(m[2]), at); } catch {}
  }
  if (/^unreachable address:/.test(msg)) state.unreachable = true;
  if ((m = msg.match(/^do: sent request: (\{"command":\["show-text".*)$/))) {
    try {
      const text = JSON.parse(m[1]).command[1];
      // a slow member's seconds ready, on mpv's screen every few seconds: the status has them
      if (/^(your|.*'s) network is slow: \d+s ready$/s.test(text)) return;
      const li = addFeed(text, at);
      if (/^resumed by /.test(text)) resumed();
      if (/^paused for .*: left$/.test(text)) state.leftPause = true;
      // if this node was playing it stops next, and that is this line, not a pause of its own
      const why = text.match(/^paused for .*: (joining|catching up)$|^paused: (out of sync) with .*$/);
      state.stopping = why ? { li, why: why[1] || why[2] } : null;
    } catch {}
  }
  if ((m = msg.match(/^now playing: (".*")$/))) {
    try { nowPlaying(JSON.parse(m[1]), at); } catch {}
  }
  if ((m = msg.match(/^Watch: broadcasted event: (\{.*\})$/))) {
    try {
      const e = JSON.parse(m[1]);
      if (e.name === "pause" && e.data === "yes" && state.leftPause) state.leftPause = false;
      else if (e.name === "pause" && e.data === "yes" && state.stopping) state.stopping.li.lastChild.textContent = "paused by you: " + state.stopping.why;
      else if (e.name === "pause") addFeed(e.data === "yes" ? "paused by you" : "resumed by you", at);
      if (e.name === "pause" && e.data === "no") resumed();
      if (e.name === "pause") state.stopping = null;
      if (e.name === "pause-reason") [...$("feed").querySelectorAll("li")].find(li => li.lastChild.textContent === "paused by you")?.remove();
    } catch {}
  }
}

// who is in the party, by the id its transport gave each member, so two with one name stay
// two; whoever was there before this node is "already here", as only their names come
function member(what, p, at) {
  const i = state.members.findIndex(q => q.id === p.id);
  if (what === "left") {
    if (i >= 0) state.members.splice(i, 1);
    delete state.aheadOf[p.id];
  } else if (i < 0) {
    state.members.push({ id: p.id, name: p.name, since: what === "joined" ? at : null });
  }
}
// once the party plays again, nobody is waited for
function resumed() {
  Object.assign(state, { myAhead: null, aheadOf: {} });
}
const seconds = n => `${n} ${n === 1 ? "second" : "seconds"}`;

const MUSHROOM = `<svg viewBox="0 0 32 32" aria-hidden="true"><path d="M12 18h8l1 11h-10z" fill="#f1ead6" stroke="#6e4b30" stroke-width="1.5"/><path d="M3 18C5 5 27 5 29 18z" fill="currentColor" stroke="#4a3220" stroke-width="1.5"/><circle cx="11" cy="12" r="2" fill="#f6efdc"/><circle cx="19" cy="9.5" r="1.6" fill="#f6efdc"/></svg>`;
const SHELLS = ["#7d9a4a", "#a8642a", "#c8a245", "#9a3a35"].map(c => `<svg class="shell" viewBox="0 0 24 24" aria-hidden="true"><path d="M2 17 C2 4 22 4 22 17 Z" fill="${c}" stroke="#4a3220" stroke-width="1.4"/><path d="M7 16.5 L9 10 H15 L17 16.5 M12 10 V16.5" stroke="#4a3220" stroke-width="1.1" fill="none"/><rect x="1" y="16" width="22" height="3" rx="1.5" fill="#6e4b30"/></svg>`);

// the steps a party takes to start, from the form; a page reloaded mid-party has no form for
// it, so it reads them from the lines instead (steps())
function planFor(f) {
  const file = f.file.trim(), torrent = /^magnet:/i.test(file) || /\.torrent$/i.test(file);
  const steps = torrent ? ["torrent"] : /:\/\//.test(file) ? [] : [f.playlist ? "files" : "file"];
  if (f.mode === "relay") steps.push("relay");
  else if (!f.local) steps.push("router");
  return [...steps, "mpv", "video"];
}
function steps() {
  if (plan) return plan;
  const s = state.torrent ? ["torrent"] : state.check ? ["files"] : [];
  if (state.relay) s.push("relay");
  else if (state.router) s.push("router");
  return [...s, "mpv", "video"];
}
function stepDone(k) {
  // the invite comes once the file is known, before mpv starts
  const checked = state.shared || state.router || state.mpvUp || state.ready;
  return { torrent: state.found || state.shared, file: checked, files: checked, relay: state.connected, router: state.shared, mpv: state.mpvUp || state.ready, video: state.ready }[k];
}
const swarmText = sw => [[`${sw.peers} ${sw.peers === 1 ? "peer" : "peers"}`, `, ${sw.speed}`]];
function stepText(k) {
  const c = state.check, sw = state.swarm, many = c ? c.n > 1 : k === "files";
  switch (k) {
    case "torrent": return { text: "Looking for this torrent's peers. It can take a while…", detail: sw && [[sw.peers ? `${sw.peers} ${sw.peers === 1 ? "peer" : "peers"}` : "No peers", sw.peers ? " so far" : " yet"]] };
    case "file": case "files": return { text: `Checking the ${many ? "videos" : "video"}…`, detail: c && (many ? [[`${c.i} of ${c.n}:`, " " + c.name]] : [["", c.name]]), pct: c && many ? c.i / c.n * 100 : null };
    case "router": return { text: "Asking your router to open a port. This takes a few seconds…" };
    case "relay": return { text: "Connecting to the relay…" };
    case "mpv": return { text: "Opening mpv. This takes a few seconds…" };
    case "video": return { text: "Loading the video…", detail: sw && swarmText(sw) };
  }
}
const LABELS = { torrent: "Torrent", file: "File", files: "Files", relay: "Relay", router: "Router", mpv: "mpv", video: "Video" };
// the trail shows once a step has taken half a second, then stays until the node is in: a
// quick start would only flash it
const TRAIL_AFTER = 500;
let trailTimer = 0;
function renderTrail(list, now) {
  const trail = $("trail");
  if (now !== state.trailStep) Object.assign(state, { trailStep: now, trailAt: performance.now() });
  const wait = TRAIL_AFTER - (performance.now() - state.trailAt);
  clearTimeout(trailTimer);
  if (now && !state.trailShown && wait > 0) trailTimer = setTimeout(poll, wait);
  else if (now) state.trailShown = true;
  trail.hidden = !now || !state.trailShown;
  if (trail.hidden) return;
  trail.replaceChildren(...list.map(k => {
    const st = document.createElement("div");
    st.className = "st " + (k === now ? "now-step" : stepDone(k) ? "done" : "");
    st.innerHTML = MUSHROOM;
    st.append(k === "files" && state.check?.n === 1 ? "File" : LABELS[k]);
    return st;
  }));
}
// detail is a list of [bold, plain] pairs
function renderDetail(detail, pct) {
  $("detail").hidden = !detail;
  if (detail) $("detail").replaceChildren(...detail.flatMap(([b, plain]) => {
    const strong = document.createElement("b");
    strong.textContent = b;
    return b ? [strong, plain] : [plain];
  }));
  $("bar").hidden = pct == null;
  if (pct != null) $("bar").firstChild.style.width = Math.max(0, Math.min(100, pct)) + "%";
}
function renderMembers(show) {
  $("members").hidden = !show;
  if (!show) return;
  $("member-count").textContent = 1 + state.members.length;
  const row = (i, name, since, ahead) => {
    const li = document.createElement("li");
    li.innerHTML = SHELLS[i % SHELLS.length];
    const n = document.createElement("span"), t = document.createElement("span");
    n.className = "name"; n.textContent = name;
    t.className = "since"; t.textContent = since ? "since " + clock(since) : "already here";
    li.append(n, t);
    if (ahead != null) {
      // full at a minute ready, the longest a member waits between saying so
      const r = document.createElement("span"), bar = document.createElement("span"), fill = document.createElement("i");
      r.className = "ready"; bar.className = "bar";
      fill.style.width = Math.min(100, ahead / 60 * 100) + "%";
      bar.append(fill);
      r.append(`${ahead}s ready`, bar);
      li.append(r);
    }
    return li;
  };
  $("member-list").replaceChildren(row(0, "You", state.inAt, state.myAhead), ...state.members.map((p, i) => row(i + 1, p.name, p.since, state.aheadOf[p.id]?.secs)));
}

// a refusal that says what differs (party.NotTheSameFile): the diagnosis from it, and its
// rows as party gives them, what differs on your side in berry
function refusedFor(r) {
  if (r.box === "tick") return `Diagnosis: the party plays a playlist of ${r.videos} videos, and you picked one file. Tick the box under File and start again.`;
  if (r.box === "untick") return "Diagnosis: the party plays one file, and you picked a whole folder. Untick the box under File and start again.";
  return r.playlist
    ? "Diagnosis: this folder doesn't match the party's. Everyone needs the same videos in the same order."
    : "Diagnosis: your video is not the same file as the party's. Pick the same video and start again.";
}

function renderCompare(r) {
  $("compare").hidden = !r;
  if (!r) return;
  const cell = (text, mono, cls) => {
    const s = document.createElement("span");
    if (cls) s.className = cls;
    if (mono && text) { const c = document.createElement("code"); c.textContent = text; s.append(c); } else s.textContent = text;
    return s;
  };
  const li = (...cells) => { const l = document.createElement("li"); l.append(...cells); return l; };
  $("compare").replaceChildren(
    li(cell(""), cell("The party's", false, "h"), cell("Yours", false, "h")),
    ...r.rows.map(x => li(cell(x.label, false, "k"), cell(x.party, x.mono), cell(x.yours, x.mono, x.differs ? "x" : ""))));
}

const notSameFile = () => $("playlist").checked
  ? "Diagnosis: this folder doesn't match the party's. Everyone needs the same videos in the same order, with the box ticked."
  : "Diagnosis: your video is not the same file as the party's. Pick the same video and start again. If the party is watching a whole folder, tick the box under File.";

function render(exitErr, refusal) {
  let text, bad = false, mood = "awake", walking = false, detail = null, pct = null;
  const list = steps(), now = running ? list.find(k => !stepDone(k)) : undefined;
  // the relay's rejection, as Run returned it
  const rejected = !/^rejected by relay/.test(exitErr || "") ? "" : /not the same file/.test(exitErr) ? "file" : /did not open/.test(exitErr) ? "unopened" : /-createKey/.test(exitErr) ? "key" : "secret";
  if (refusal) {
    text = refusedFor(refusal); bad = true;
  } else if (rejected === "file") {
    text = notSameFile(); bad = true;
  } else if (rejected === "unopened") {
    text = "Diagnosis: nobody opened this room for 30 minutes. Start again once someone with the create key is in."; bad = true;
  } else if (rejected === "key") {
    text = "Diagnosis: this relay needs a create key to open a new party. Add it in Settings and start again."; bad = true;
  } else if (rejected) {
    text = "Diagnosis: the relay rejected this room or secret. Check the invite and start again."; bad = true;
  } else if (running && state.waiting) {
    text = "Waiting for the host to open this room. It starts by itself once they're in."; walking = true;
  } else if (running && state.busy === "limited") {
    text = "This relay has had too many attempts from your network. Trying again; it clears within the hour."; walking = true;
  } else if (running && state.busy === "full") {
    text = "The relay is full right now. Trying again…"; walking = true;
  } else if (running && state.reconnecting) {
    text = "Lost the relay. Reconnecting…"; walking = true;
  } else if (now) {
    // the first step not done yet: the relay's handshake can end after mpv is ready, and this
    // node is in only once the room has it
    ({ text, detail, pct } = stepText(now)); walking = true;
  } else if (running && state.buffering != null) {
    text = "Your player is buffering…";
    detail = [[`${state.buffering}%`, " ready"], ...(state.swarm ? [["", ", "], ...swarmText(state.swarm)] : [])];
    pct = state.buffering;
  } else if (running && state.myAhead != null) {
    text = "Your network is slow. Everyone sees how much you have ready.";
    detail = [[seconds(state.myAhead), " of video ready"]];
  } else if (running && Object.keys(state.aheadOf).length) {
    const slow = Object.values(state.aheadOf);
    text = slow.length === 1 ? `${slow[0].name}'s network is slow. Stay paused while it buffers.`
      : `${names(slow.map(p => p.name))} have slow networks. Stay paused while they buffer.`;
    detail = slow.flatMap((p, i) => [[seconds(p.secs), ` ready on ${p.name}'s side`], ...(i < slow.length - 1 ? [["", ", "]] : [])]);
  } else if (running && state.unreachable) {
    text = "You're in, but friends outside this network can't reach this invite: your connection has no public address. Use a relay for them.";
  } else if (running) {
    text = "You're in. Pause or seek in mpv and everyone follows.";
  } else if (exitErr) {
    text = /not the same file/.test(exitErr) ? notSameFile()
      : /mpv/.test(exitErr) && /not found|cannot find|no such file/i.test(exitErr)
      ? "Diagnosis: mpv isn't installed, or this app can't find it. Install mpv, or choose mpv on the Settings page."
      : `Diagnosis: ${exitErr}. The patient chart below has the details.`;
    bad = true;
    $("chart").open = true;
  } else {
    text = "The party has ended."; mood = "sleep";
  }
  // a live region: rewriting the same words makes a screen reader say them again
  if ($("status").textContent !== text) $("status").textContent = text;
  $("status").classList.toggle("bad", bad);
  renderTrail(list, now);
  renderDetail(refusal?.detail || detail, pct);
  renderCompare(refusal);
  renderMembers(running && state.ready && !now);
  $("chat").hidden = !(running && state.ready && !now);
  // the relay would drop a message: the node says so on mpv's screen, and the box waits
  const away = running && state.reconnecting;
  $("chat-text").disabled = $("chat-send").disabled = away;
  $("react-open").disabled = away || reactedAt > Date.now() - REACT_EVERY;
  if (away) openStrip(false);
  $("chat-text").placeholder = away ? "Reconnecting to the relay…" : "Shows on everyone's mpv for a few seconds";
  document.body.dataset.mood = mood;
  document.body.classList.toggle("walking", walking);
  $("fields").hidden = running;
  $("share").hidden = !running || !$("share-text").textContent;
  $("go-label").textContent = running ? "End the party" : "Start the party";
  document.body.classList.toggle("alive", running && state.ready);
  $("go").classList.toggle("stop", running);
  $("update").disabled = running;
  $("restart").disabled = running;
  $("restart-now").disabled = running;
  document.querySelectorAll("#history-list button").forEach(b => b.disabled = running);
}

// one poll at a time: every second, and at once when /alive says there are new lines (soon,
// 100ms later, so a burst of lines is read in one go); news during a poll polls again after it
let polling = false, again = false, next = 0;
function soon() {
  if (polling) { again = true; return; }
  clearTimeout(next);
  next = setTimeout(poll, 100);
}
async function poll() {
  if (polling) { again = true; return; }
  polling = true;
  clearTimeout(next);
  try {
    const res = await api(`/log?from=${from}&said=${saidFrom}&run=${run}`);
    const j = await res.json();
    if (j.run !== run) { run = j.run; reset(); }
    const lines = j.lines || [], said = j.said || [];
    lines.forEach(handle);
    from = j.from + lines.length;
    // chat, kept apart from the log and only while the party runs: gone from the page at its end
    saidFrom = j.saidFrom + said.length;
    for (const m of said) if (m.ours && m.react) rest(m.at);
    if (said.length || (!j.running && state.said.length)) {
      state.said = j.running ? state.said.concat(said).slice(-SAID_KEPT) : [];
      renderSaid();
    }
    if (running && !j.running) loadState();
    running = j.running;
    if (run > 0) {
      $("vitals").hidden = false;
      render(j.exitErr, j.refusal);
    }
  } catch {
    polling = false;
    if (updated) return;
    showError("The app closed in the background. Close this window and open it again.");
    document.body.dataset.mood = "sleep";
  }
  polling = false;
  next = setTimeout(poll, again ? 0 : 1000);
  again = false;
}

$("copy").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText($("share-text").textContent);
    $("copy").textContent = "Copied";
  } catch {
    getSelection().selectAllChildren($("share-text"));
    $("copy").textContent = "Press Ctrl+C to copy";
  }
  setTimeout(() => $("copy").textContent = "Copy invite", 2000);
});

// the chat under the box, newest first: the latest SAID_SHOWN, the rest under "All messages".
// Whose it is shows by its colour (ours), since a member can be named "you"
function renderSaid() {
  const row = m => {
    const li = document.createElement("li"), time = document.createElement("time"), who = document.createElement("b"), text = document.createElement("span");
    time.textContent = clock(m.at);
    who.textContent = m.who;
    if (m.react) {
      // drawn as mpv draws it: a system font may not have the emoji yet
      text.className = "react";
      text.innerHTML = emoji?.svg[m.react] || "";
      text.setAttribute("aria-label", String.fromCodePoint(parseInt(m.react, 16)));
    } else {
      text.textContent = m.text;
    }
    li.classList.toggle("ours", !!m.ours);
    li.append(time, who, text);
    return li;
  };
  const newest = state.said.slice().reverse();
  $("said-latest").replaceChildren(...newest.slice(0, SAID_SHOWN).map(row));
  $("said-rest").replaceChildren(...newest.slice(SAID_SHOWN).map(row));
  $("said-count").textContent = newest.length;
  $("said-more").hidden = newest.length <= SAID_SHOWN;
}

// why a message stayed in the box, until it is changed or one goes
const NOT_SENT = { 409: "Not sent: the party has ended.", 503: "Not sent: too many messages at once. Send it again in a moment." };
const REACT_NOT_SENT = { 409: "Your reaction wasn't sent: the party has ended.", 503: "Your reaction wasn't sent: too many at once. Pick it again in a moment." };
function chatHint(text) {
  $("chat-hint").textContent = text || "";
  $("chat-hint").hidden = !text;
}

// a message is ChatLimit characters at most, counted as the node does: maxlength counts an
// emoji as two
const CHAT_LIMIT = 200;
$("chat-text").addEventListener("input", () => {
  const chars = [...$("chat-text").value];
  if (chars.length > CHAT_LIMIT) $("chat-text").value = chars.slice(0, CHAT_LIMIT).join("");
  chatHint();
});

// a message goes on everyone's mpv, and under the box once this node's mpv shows it (said).
// Kept in the box if the party didn't take it
$("chat-form").addEventListener("submit", async e => {
  e.preventDefault();
  const text = $("chat-text").value;
  if (!text.trim()) return;
  let res = null;
  try { res = await api("/chat", { text }); } catch {}
  if (res?.ok) $("chat-text").value = "";
  chatHint(res?.ok ? "" : NOT_SENT[res?.status] || "Not sent: the app didn't answer. Send it again.");
});

// reactions: the picker's emoji (GET /emoji, drawn as mpv draws them) in one row that scrolls
// sideways, opened by the button beside Send. A pick bursts on everyone's mpv.
async function loadEmoji() {
  try { emoji = await (await api("/emoji")).json(); } catch { return; }
  $("react-strip").replaceChildren(...emoji.order.map(code => {
    const b = document.createElement("button");
    b.type = "button";
    b.dataset.code = code;
    b.setAttribute("aria-label", String.fromCodePoint(parseInt(code, 16)));
    b.innerHTML = emoji.svg[code];
    return b;
  }));
  renderSaid();
}
loadEmoji();
// rest: this node's person reacted at at (ms), here or in mpv's picker, so the button rests
// REACT_EVERY: a pick meanwhile would be dropped
function rest(at) {
  reactedAt = Math.max(reactedAt, at);
  openStrip(false);
  $("react-open").disabled = true;
  setTimeout(() => {
    if (!state.reconnecting && reactedAt <= Date.now() - REACT_EVERY) $("react-open").disabled = false;
  }, reactedAt + REACT_EVERY - Date.now());
}
function openStrip(open) {
  $("react-row").hidden = $("react-strip").hidden = !open;
  $("react-open").setAttribute("aria-expanded", open);
  if (open) $("react-strip").scrollLeft = 0;
  showMore();
}
// people didn't see the row scrolls: each end with more gets an arrow and fades out
function showMore() {
  const s = $("react-strip");
  const back = !s.hidden && s.scrollLeft > 1, on = !s.hidden && s.scrollLeft + s.clientWidth < s.scrollWidth - 1;
  $("react-back").hidden = !back;
  $("react-more").hidden = !on;
  s.classList.toggle("more-back", back);
  s.classList.toggle("more-on", on);
}
$("react-strip").addEventListener("scroll", showMore);
window.addEventListener("resize", showMore);
// an arrow scrolls a rowful, less one emoji for where it was
const page = by => $("react-strip").scrollBy({ left: by * ($("react-strip").clientWidth - 72), behavior: "smooth" });
$("react-back").addEventListener("click", () => page(-1));
$("react-more").addEventListener("click", () => page(1));
$("react-open").addEventListener("click", () => openStrip($("react-strip").hidden));
$("react-row").addEventListener("keydown", e => {
  if (e.key === "Escape") { openStrip(false); $("react-open").focus(); }
});
// a mouse's wheel turns up and down: here it scrolls the row sideways
$("react-strip").addEventListener("wheel", e => {
  if (!e.deltaY || Math.abs(e.deltaX) > Math.abs(e.deltaY)) return;
  e.preventDefault();
  $("react-strip").scrollLeft += e.deltaY;
}, { passive: false });
$("react-strip").addEventListener("click", async e => {
  const b = e.target.closest("button"), code = b?.dataset.code;
  if (!code) return;
  // one an end cuts is that end's arrow, as on mpv: it scrolls, rather than picks what isn't
  // all shown (a key's focus scrolls one whole into view first)
  const row = $("react-strip").getBoundingClientRect(), at = b.getBoundingClientRect();
  if (at.left < row.left - 0.5 || at.right > row.right + 0.5) return page(at.left < row.left ? -1 : 1);
  rest(Date.now());
  let res = null;
  try { res = await api("/react", { code }); } catch {}
  chatHint(res?.ok ? "" : REACT_NOT_SENT[res?.status] || "Your reaction wasn't sent: the app didn't answer. Pick it again.");
});

$("save-chart").addEventListener("click", async () => {
  const j = await (await api(`/log?from=0&run=${run}`)).json();
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([(j.lines || []).map(l => l + "\n").join("")], { type: "text/plain" }));
  a.download = `watchparty-${new Date().toLocaleString("sv").slice(0, 16).replace(/[ :]/g, "-")}.log`;
  a.click();
  $("chart-saved").textContent = `Saved as ${a.download} in your downloads folder.`;
  $("chart-saved").hidden = false;
});

document.querySelector(".cat").addEventListener("click", () => {
  const heart = document.createElement("span");
  heart.className = "heart";
  heart.textContent = "♥";
  heart.style.left = 40 + Math.random() * 20 + "%";
  heart.addEventListener("animationend", () => heart.remove());
  document.querySelector("main").append(heart);
});

let updated = false, latest = "", installed = "";
async function checkUpdate() {
  try {
    const j = await (await api("/update")).json();
    $("version-text").textContent = j.current === "dev" ? "Development build" : `Version ${j.current}`;
    if (j.installed) {
      pendingRestart(j.installed);
    } else if (j.available) {
      latest = j.latest;
      $("update").textContent = `Update to ${j.latest}`;
      $("update").hidden = false;
      $("update-dot").hidden = false;
    }
  } catch {}
}
function pendingRestart(tag) {
  installed = tag;
  $("version-text").textContent = `${tag} is installed. Restart the app to start using it.`;
  $("update").hidden = true;
  $("restart").hidden = false;
  $("update-dot").hidden = false;
  $("restart-text").textContent = `Version ${tag} is installed. A restart is required to start using it.`;
}
$("update").addEventListener("click", async () => {
  const b = $("update");
  b.disabled = true;
  b.textContent = "Updating…";
  const res = await api("/update", {});
  const text = (await res.text()).trim();
  if (!res.ok) {
    $("version-text").textContent = `Update failed: ${text}.`;
    b.textContent = `Update to ${latest}`;
    b.disabled = running;
    return;
  }
  pendingRestart(text);
  $("restart-dialog").showModal();
});
async function restartNow() {
  $("restart-dialog").close();
  $("restart").disabled = true;
  const res = await api("/restart", {});
  if (!res.ok) {
    $("version-text").textContent = `Restart failed: ${(await res.text()).trim()}.`;
    $("restart").disabled = running;
    return;
  }
  updated = true;
  $("restart").hidden = true;
  $("version-text").textContent = `Restarting into ${installed}. If this window stays open, you can close it.`;
  window.close();
}
$("restart").addEventListener("click", restartNow);
let toolsAsked = 0;
async function checkTools() {
  const asked = ++toolsAsked;
  try {
    const list = await (await api("/tools")).json();
    if (asked !== toolsAsked) return;
    toolsChecked = true;
    $("tools").replaceChildren(...list.map(t => {
      const li = document.createElement("li"), name = document.createElement("span"), ver = document.createElement("span");
      const path = document.createElement("code"), note = document.createElement("p");
      const b = document.createElement("b"), info = document.createElement("button"), steps = document.createElement("ol");
      b.textContent = t.name;
      info.type = "button";
      info.className = "info";
      info.textContent = "i";
      info.setAttribute("aria-label", `How to install ${t.name} by hand`);
      info.setAttribute("aria-expanded", "false");
      info.addEventListener("click", () => {
        steps.hidden = !steps.hidden;
        info.setAttribute("aria-expanded", !steps.hidden);
      });
      name.className = "name";
      name.append(b);
      if (t.steps) name.append(info);
      steps.className = "steps";
      steps.hidden = true;
      steps.append(...(t.steps || []).map(text => Object.assign(document.createElement("li"), { textContent: text })));
      ver.className = "ver";
      ver.textContent = t.version || "";
      path.textContent = t.path || "";
      path.hidden = !t.path;
      note.className = "hint";
      note.textContent = t.update;
      note.hidden = !t.update;
      li.append(name, ver, path, note, steps);
      return li;
    }));
    $("tools").hidden = !list.length;
  } catch {}
}
$("restart-now").addEventListener("click", restartNow);
$("restart-later").addEventListener("click", () => $("restart-dialog").close());

if (["history", "settings"].includes(location.hash.slice(1))) show(location.hash.slice(1));
async function alive() {
  const res = await api("/alive").catch(() => null);
  if (!res?.ok) return;
  try {
    const r = res.body.getReader(), text = new TextDecoder();
    for (let chunk; !(chunk = await r.read()).done;) {
      if (text.decode(chunk.value, { stream: true }).includes("data:")) soon();
    }
  } catch {}
  alive();
}
alive();
loadState();
poll();
checkUpdate();
