# SECURITY.md

Keep security in mind on every change. The relay is the only internet-facing surface, so
its package doc (`relay/CLAUDE.md`) holds the concrete, enforced rules; this file holds the
mindset, trust model and method that shape them. Read it before any change to the relay, the
transport, or anything that parses network input.

## Mindset

- **Every byte off the network is hostile until proven otherwise** — query params, headers,
  websocket frames, file fingerprints. Sanitize or validate at ingress, not at each use.
- **Logs are attacker-reachable output.** Anything attacker-controlled that reaches a log
  line is cleaned first (control characters stripped), or a `%0a` forges log entries. Same
  for text echoed to peers (notices land in mpv's OSD and the GUI feed).
- **Defense lives at the layer that owns it.** The relay bounds only what reaches its
  handler; TLS, volumetric floods, bot/WAF filtering, slow-request timeouts and
  real-client-IP attribution belong to the TLS proxy / CDN in front. Don't move a check into
  the relay that the proxy must own — e.g. trusting a `CF-Connecting-IP` header is safe only
  once the origin rejects non-CDN traffic.

## Trust boundary

A party's **secret is the only lock.** Model every actor against it:

| Actor | Can | Cannot |
|---|---|---|
| Outsider (no secret) | reach `/ws`; create a room only if they also hold the create key | join/read a party, drive playback |
| Peer (holds the secret) | drive playback, claim any hostname, push frames to peers | — full trust, by design, as in the P2P mesh |
| The relay | fan out opaque frames, synthesize notices | read playback, persist anything |

Design and test outsider-reachable paths to stand on their own; insider abuse is accepted to
the same degree as the P2P transport.

## Method (how we work a finding)

1. **Map the surface** from code *and* the live deployment; separate outsider paths from
   insider ones before judging severity.
2. **Prove it non-destructively** — one request that demonstrates the defect, never a flood
   against a live (often shared) host. Keep the probe re-runnable.
3. **Fix at the owning layer** — relay code for input handling; the proxy / CDN for
   transport, origin-locking, real client IP, and rate / slow-request limits.
4. **Verify live and keep the check** — re-run until the finding flips; a behaviour change
   lands with its test (`relay/CLAUDE.md` conventions).

## Where the rest lives

- Enforced relay rules (rate limiter, `TRUST_PROXY` / XFF, timeouts, accepted risks):
  `relay/CLAUDE.md`.
- A dated live-deployment assessment (origin IP, proxy config, probe harness) is kept
  **private, outside this repo** — infra detail doesn't belong in a public tree.
