# CLAUDE.md

Guidance for Claude Code when working in this repository.

## Conventions

- Do **not** add the `Co-Authored-By: Claude ...` trailer to git commits, nor the
  "🤖 Generated with Claude Code" line to PR bodies.

## What this is

SSH front-end for Brogue Community Edition: a wish (Go) SSH server + an OIDC
SSO gate (`authlaunch`) that execs a forked Brogue. There is **no dgamelaunch**
(Brogue's title screen is the menu), and the **forked Brogue** adds a SIGHUP
hangup-save and a startup-hang fix. See `README.md` for the full picture.

## Build & Run

```bash
go build ./...            # wish-server + authlaunch
go test ./...             # authlaunch unit tests
podman build -t brogue-server .   # full image (prefer podman over docker)
```

The image build clones the Brogue fork `geechorama/BrogueCE` at tag
`v1.15.1-g10s3` and builds it `make TERMINAL=YES GRAPHICS=NO` (pure ncurses).

## Architecture (summary)

- `main.go` — wish SSH server, dual listener (`:2222` LAN, `:2223` Tailscale),
  forwards the pubkey, SIGHUPs the child group on disconnect (hangup-save).
- `cmd/authlaunch/` — OIDC device-code SSO gate; resolves pubkey → identity,
  enforces nick ownership, execs the target with `$AUTH_NICK`. Game-agnostic and
  env-driven (no dgamelaunch/`DGLAUTH` handoff; the nick is passed via `AUTH_NICK`).
- `brogue-launch.sh` — `WISH_COMMAND`; lays out `/brogue`, execs `authlaunch -- brogue-run.sh`.
- `brogue-run.sh` — per-user cwd + `setpriv` drop to `games` (uid 5) + exec brogue.

## The Brogue fork

The patches live in `geechorama/BrogueCE` (branch `g10s`, tag
`v1.15.1-g10s3`), not here. Two patches to `src/platform/curses-platform.c`: a
SIGHUP handler that saves the game and exits (mirroring Brogue's SDL
window-close path), and a fix for an upstream short-overflow in `_delayUpTo`
that hung the terminal title screen for up to ~32s on startup (filed upstream as
tmewett/BrogueCE#854). **To take a new Brogue release:** in that repo, rebase
`g10s` onto the new upstream tag, re-tag `vX.Y.Z-g10sN`, push, then bump the tag
in this repo's `Dockerfile` in an explicit commit.

## Notes

- `authlaunch` runs as **root** (it reads/writes the root-owned `0600`
  `keys.tsv`). Only the game is dropped to `games` (uid 5) via `setpriv` in
  `brogue-run.sh`. Never run the game as root.
- The OIDC client is **public** (no client secret); the three `OIDC_*` vars are
  mandatory or every session is refused. This server uses a group for
  access control + a profile attribute for the nickname, and adds a
  `brogue-server` client.
- CI publishes `:latest` + `<branch>-<short-sha>` to GHCR; the cluster uses
  `:latest` + `imagePullPolicy: Always`, so no commit-back is needed.
