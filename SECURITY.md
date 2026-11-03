# SECURITY.md

Keep security in mind on every change. Two surfaces face the network, both guarded only by
the party secret: the relay (`relay/`, always public) and the P2P server (`server/`, on the
LAN and, under UPNP, the public internet). Their package docs hold the concrete, enforced
rules; this file holds the mindset, trust model and method that shape them. Read it before
any change to either, to the relay client, the mpv event handling, the GUI server, or
anything else that parses network input.

## Mindset

- **Every byte off the network is hostile until proven otherwise** — query params, headers,
  websocket frames, file fingerprints. Sanitize or validate at ingress, not at each use.
- **Logs are attacker-reachable output.** Anything attacker-controlled that reaches a log
  line is cleaned first (control characters stripped), or a `%0a` forges log entries. Same
  for text echoed to peers (notices land in mpv's OSD and the GUI feed). The hole is input
  URL-decoded *after* parsing (a query param, as on the relay); Go's server already rejects
  control chars in a header *value* with 400, so the P2P server, reading only headers,
  needs no such cleaning.
- **Defense lives at the layer that owns it.** Behind a proxy (the relay), TLS, volumetric
  floods, bot/WAF filtering and real-client-IP attribution belong to that proxy / CDN; don't
  move a check into the app that the proxy must own — trusting a `CF-Connecting-IP` header is
  safe only once the origin rejects non-CDN traffic. With *no* proxy (the P2P server), the Go
  server owns what it can: a request-header timeout and a header-size cap, or Go's no-timeout
  default is free slowloris on a listener UPNP has put on the internet.

## Trust boundary

A party's **secret is the only lock.** Model every actor against it:

| Actor | Can | Cannot |
|---|---|---|
| Outsider (no secret) | reach `/ws`; create a room only if they also hold the create key | join/read a party, drive playback |
| Peer (holds the secret) | drive playback, claim any hostname, push frames to peers | — full trust, by design, as in the P2P mesh |
| The relay | fan out opaque frames, synthesize notices | read playback, persist anything |

Design and test outsider-reachable paths to stand on their own; insider abuse is accepted to
the same degree as the P2P transport. One insider limit is load-bearing, though: the wire
carries raw mpv JSON, but the receiver (`mpv.apply`) only ever dispatches a fixed allow-list
of event names to parameterised `set_property`/`show-text`. Never "simplify" it into writing
frames straight to mpv's IPC — that turns any peer into command execution on every machine.

## Method (how we work a finding)

1. **Map the surface** from code *and* the live deployment; separate outsider paths from
   insider ones before judging severity.
2. **Prove it non-destructively** — one request that demonstrates the defect, never a flood
   against a live (often shared) host. Keep the probe re-runnable.
3. **Fix at the owning layer** — app code for input handling and, with no proxy, for the
   header timeout; the proxy / CDN, when there is one, for transport, origin-locking, real
   client IP, and rate / slow-request limits.
4. **Verify and keep the check** — re-run until the finding flips. A behaviour change lands
   with its test; for a layer that isn't unit-tested (the P2P server, the proxy) keep a
   re-runnable probe instead.

## Where the rest lives

- Enforced relay rules (rate limiter, `TRUST_PROXY` / XFF, timeouts, accepted risks):
  `relay/CLAUDE.md`. Enforced P2P rules (secret gate, 64KB body cap, server timeouts):
  `server/CLAUDE.md` and `party/CLAUDE.md`.
- Dated assessments (the relay's live deployment, origin IP and probe harness; the
  whole-repo code audit) are kept **private, outside this repo** — infra and attack detail
  doesn't belong in a public tree.
