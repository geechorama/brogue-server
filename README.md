# brogue-server

SSH front-end for [Brogue Community Edition](https://github.com/tmewett/BrogueCE)
served via [charmbracelet/wish](https://github.com/charmbracelet/wish). Players
connect over SSH; identity is established by OIDC single sign-on (see
[Authentication](#authentication-oidc-sso)), and they land directly in Brogue
under the nickname from their profile at the OIDC provider. Saves, recordings,
and scores persist on a data volume mounted into the pod by the Kubernetes
deployment manifests.

```
player → ssh brogue.example.com → wish-server → authlaunch ⇄ OIDC provider
                                                    │  (export AUTH_NICK, drop privs)
                                                    ▼
                                      brogue -t --data-dir /opt/brogue
                                                    │  (cwd = /brogue/userdata/<nick>)
                                              /brogue (data volume)
```

There is **no dgamelaunch** game-selection layer — Brogue's own title screen is
the menu — and Brogue is built from a small **fork** that adds a hangup-save
(see [The fork](#the-fork)).

Authentication is mandatory on both layers: connecting requires an SSH key pair
(keyless clients get a friendly explanation and are disconnected), and the
resolved OIDC identity is the only way in — `authlaunch` refuses the session
(never spawning the game) when it can't establish who the user is, including when
the OIDC environment variables are unset.

## Running

### Container (podman/docker)

```bash
podman build -t brogue-server .

# The OIDC vars are mandatory — without them every session is refused.
podman run --rm -it -p 2222:2222 \
  -v /tmp/brogue:/brogue \
  -v /path/to/host_key:/etc/wish/host_key \
  -e WISH_HOST_KEY_PATH=/etc/wish/host_key \
  -e OIDC_ISSUER_URL=https://auth.example.com \
  -e OIDC_CLIENT_ID=brogue-server \
  -e OIDC_ALLOWED_GROUP=players \
  brogue-server

ssh -p 2222 localhost   # an SSH key pair is required
```

### Kubernetes

Deploy with any Kubernetes tooling. The image is pulled from
`ghcr.io/geechorama/brogue-server:latest` with `imagePullPolicy: Always`, so
each pod restart picks up the most recent CI build.

## Configuration

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `WISH_COMMAND` | Yes | `/usr/local/bin/brogue-launch.sh` (set in Dockerfile) | Command run per SSH session |
| `WISH_HOST_KEY_PATH` | No | `.ssh/term_info_ed25519` | Host key location |
| `TZ` | No | UTC | Timezone (libc). Set on the k8s deployment. |
| `OIDC_ISSUER_URL` | Yes | — | OIDC issuer URL, e.g. `https://auth.example.com` |
| `OIDC_CLIENT_ID` | Yes | — | Public client ID with the Device Authorization Grant enabled |
| `OIDC_ALLOWED_GROUP` | Yes | — | Group that grants access (matched against the token's group claim) |
| `OIDC_NICK_CLAIM` | No | `game_nickname` | Token claim holding the player's nickname |
| `OIDC_GROUPS_CLAIM` | No | `groups` | Token claim holding group membership |
| `OIDC_FRONTEND_URL_TS` | No | — | `scheme://host` to rewrite the device-verification URL to for Tailscale-origin sessions (e.g. `https://auth.your-tailnet.ts.net`). Empty disables rewriting. |
| `BROGUE_INGRESS` | No (set per-listener) | `lan` | Which ingress path the session arrived on (`lan` or `tailscale`). The server sets this itself per listener; don't set it on the deployment. |
| `AUTH_KEYS_PATH` | No | `/brogue/auth/keys.tsv` | Pubkey → identity store (holds offline refresh tokens) |
| `AUTH_NICKS_PATH` | No | `/brogue/auth/nicks.tsv` | Nick → owning subject registry (prevents nick hijack) |

`OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, and `OIDC_ALLOWED_GROUP` must all be set; if
any is missing, `authlaunch` refuses every session. None are secrets — the OIDC
client is public (no client secret).

## Authentication (OIDC SSO)

**Trust-on-first-use SSH pubkey registration.** An SSH key pair is required. The
first connection from a device runs the OIDC device-code flow — `authlaunch`
renders the verification URL as a QR code in the terminal and as text — and every
subsequent connection from that device is silent, re-validated against the stored
`offline_access` refresh token (the live revocation check). `authlaunch` then
execs the Brogue launcher with the resolved nickname in `$AUTH_NICK`.

- **Pubkey store** `/brogue/auth/keys.tsv` — fingerprint → identity + offline
  refresh token. Root-owned, `0600` (bearer creds, kept off the `games` user).
- **Nick registry** `/brogue/auth/nicks.tsv` — first-come-first-served nick →
  the OIDC `sub`, so a user-editable nickname can't hijack another account's
  saves. The nick is the per-user save-directory name.

The OIDC provider must emit a groups claim and a nickname claim, have the
Device Authorization Grant enabled on the client, and support `offline_access`.
This server uses a group for access control and a profile attribute for the
nickname, and registers its own public client `brogue-server`.

## Persistence layout

`/opt/brogue/` in the image is **immutable** — the compiled Brogue binary plus
its resources. The game is launched with `--data-dir /opt/brogue`. Everything
player-visible lives under `/brogue/` (the data volume):

| Path | Purpose |
|---|---|
| `/brogue/auth/keys.tsv` | SSO pubkey → identity + offline refresh tokens (root-owned, `0600`) |
| `/brogue/auth/nicks.tsv` | Nick → owning OIDC-subject registry |
| `/brogue/userdata/<nick>/` | Per-user **cwd** — Brogue saves (`.broguesave`), recordings (`.broguerec`), `BrogueHighScores.txt`, `BrogueRunHistory.txt` |

Brogue writes saves **and** scores to its current working directory (no
`HACKDIR`-style env, no chdir of its own), so `brogue-run.sh` `cd`s into the
per-user dir before launching. Per-user cwd ⇒ per-user saves and scores. (A
global shared scoreboard is deferred.) Because the server runs with
`--single-save` (see [The fork](#the-fork)), each per-user dir holds at most one
save (`Game.broguesave`) at a time; recordings still accumulate.

## The fork

We run a small fork —
[`geechorama/BrogueCE`](https://github.com/geechorama/BrogueCE), branch
`g10s`, tag **`v1.15.1-g10s4`** (pinned in the Dockerfile) — carrying these
changes:

- **SIGHUP hangup-save.** Stock Brogue CE has no SIGHUP handling, so an SSH
  disconnect would kill the game unsaved. The patch (~40 lines) mirrors Brogue's
  own SDL window-close path (`quitImmediately(); exit()`), saving at the
  `nextBrogueEvent` input boundary so the recording is always left reloadable.
  The save lands on the data volume, so it survives pod restarts (unlike an
  in-memory session-persistence approach); on reconnect the player picks
  "Continue saved game".
- **Startup-hang fix (`_delayUpTo`).** A genuine upstream integer-overflow bug:
  on the first frame `lastDelayTime` is 0, so the elapsed-time correction
  subtracts the full epoch-milliseconds value into a `short`, wrapping it to a
  delay of up to ~32s and freezing the title screen on startup — intermittently,
  since about half the time it wraps negative and there is no hang. The fix
  computes the remainder in `long` and skips the correction on the first call.
  It affects only the ncurses (TERMINAL) build, and is filed upstream as
  [tmewett/BrogueCE#854](https://github.com/tmewett/BrogueCE/pull/854).
- **`--single-save` mode** (`MainMenu.c`, `Recordings.c`, `main.c`). A new
  command-line flag that limits the player to one save slot, NetHack-style: the
  save uses a single fixed filename and is overwritten rather than accumulating
  numbered files; the title screen's "Load Game" file browser becomes a
  "Continue" that loads that one save directly; and starting a new game while a
  save exists asks for confirmation, then abandons the old run. Loading still
  consumes the save (Brogue's existing delete-on-load), so there is no going back
  to a previous state. `brogue-run.sh` passes the flag; without it the binary
  behaves exactly like stock Brogue.

The fork tracks upstream via its `upstream` remote; to take a new Brogue release,
rebase the `g10s` branch onto the new tag, re-tag `vX.Y.Z-g10sN`, and bump the
Dockerfile.

## Architecture

- **`main.go`** — the wish SSH server. Accepts any public key (verifying the
  client holds the private key) and forwards it to the launcher in
  `$SSH_USER_PUBKEY`; keyless clients get a key-setup message and are
  disconnected. Two listeners share one host key: `:2222` (LAN) and
  `:2223` (Tailscale), tagging sessions `BROGUE_INGRESS=lan|tailscale`. On
  disconnect it SIGHUPs the child's process group (the forked Brogue's
  hangup-save), with a `WaitDelay` hard-kill backstop.
- **`cmd/authlaunch/`** — the OIDC SSO gate exec'd between the wish server
  and the game. Resolves the pubkey (or runs the device flow) to an OIDC
  identity, enforces nick ownership, and execs the target with `$AUTH_NICK` set.
  Pure logic is unit-tested; OIDC network calls are not.
- **`brogue-launch.sh`** — `WISH_COMMAND`. Normalizes `TERM`/`LANG`, lays out the
  `/brogue` data directories, then execs `authlaunch -- brogue-run.sh`.
- **`brogue-run.sh`** — run by `authlaunch` after auth. Makes the per-user dir,
  `cd`s into it, drops root → `games` (uid 5) via `setpriv`, and execs
  `brogue -t --single-save --data-dir /opt/brogue` (see [The fork](#the-fork) for
  `--single-save`).
- **`Dockerfile`** — three stages: Go builder (`wish-server` + `authlaunch`); C
  builder (clones the Brogue fork at `v1.15.1-g10s4`, `make TERMINAL=YES
  GRAPHICS=NO`); slim runtime (ncurses, `util-linux` for `setpriv`, tzdata).
- **`.github/workflows/ci.yml`** — builds `linux/amd64`, pushes `:latest` and
  `<branch>-<short-sha>` to `ghcr.io/geechorama/brogue-server`.

## Deferred features

- **Global shared high-score table** (currently per-user).
- **Prometheus metrics exporter** (Brogue has no xlogfile; it writes
  `BrogueRunHistory.txt`).
- **Upstreaming the fork patches** to BrogueCE — the `_delayUpTo`
  startup-hang fix ([#854](https://github.com/tmewett/BrogueCE/pull/854)), and
  the SIGHUP-save (which overlaps upstream issue #210).

## License

Copyright 2026 Andrew McGeachie.

Licensed under the Apache License, Version 2.0 — see [`LICENSE`](LICENSE).

The container image this repository builds bundles
[Brogue CE](https://github.com/tmewett/BrogueCE), which is licensed under the GNU
AGPL-3.0. `brogue-server` is a separate program that launches Brogue as a child
process; the modified Brogue source the image is built from is published at
[`geechorama/BrogueCE`](https://github.com/geechorama/BrogueCE).
