# update/CLAUDE.md

Self-update for the cli and the gui: check GitHub's latest release, download this
platform's asset, verify it, and swap it over the running binary. Both `main` packages
call `Install`; neither carries its own copy of the download or the platform swap.

Security: the download is **refused unless its SHA-256 matches the release's published
`digest`** — that check is the whole defense against a tampered or wrong binary, so it
never gets an escape hatch. HTTPS to the GitHub API, and it fails closed. Cross-cutting
mindset and trust model: `SECURITY.md` (repo root).

- **`ReleaseURL` is a `var`, not a `const`,** so e2e can stamp a local stand-in with
  `-X github.com/mhashemm/watchparty/update.ReleaseURL=…`. `-X` silently ignores a
  `const`, and the update cases would then hit the real GitHub.
- **`version` is the caller's**, stamped `-X main.version=<tag>` into each binary and
  passed in. A `"dev"` build is older than every release; both callers gate it out
  before calling `Install`, so a local build can't overwrite itself.
- **`base` is the asset name without the platform suffix** — `watchparty` (cli),
  `watchparty-gui` (gui) — matching the release assets in `publisher.yml`. `assetName`
  adds the suffix per `GOOS`/`GOARCH`.
- **`replaceExe` is split by OS.** Unix renames over the running file (the kernel keeps
  the open inode). Windows can't, so it renames the running `.exe` to `.old` (removed on
  the next swap) with a rollback. `go build` compiles only one half, so check the other
  with `GOOS=windows go build ./update`.
