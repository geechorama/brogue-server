#!/bin/bash
set -e

# WISH_COMMAND: run by the wish SSH server once per session, as root. It lays out
# the data volume, then hands off to authlaunch (the OIDC SSO gate), which on success
# execs brogue-run.sh with $AUTH_NICK set.

# Normalize the terminal — Brogue's ncurses front-end misbehaves on unknown TERM
# values and cannot run on TERM=dumb.
case "$TERM" in
  dumb)
    echo "Brogue requires a terminal with PTY + colors (got TERM=$TERM)." >&2
    exit 1 ;;
  xterm|"")
    TERM=xterm-256color ;;
esac
export TERM
export LANG=C.UTF-8
export LC_ALL=C.UTF-8

# SSH forwards TERM but not COLORTERM, so Brogue's curses build can't tell the
# client supports 24-bit color and falls back to its banded 256-color cube.
# Assume a truecolor terminal (iTerm2 etc.) so Brogue emits exact RGB colors.
export COLORTERM=truecolor

# /brogue is the mounted data volume. Lay it out:
#   /brogue/auth      — SSO pubkey store (keys.tsv) + nick registry (nicks.tsv)
#   /brogue/userdata  — per-user save dirs (Brogue saves/recordings/scores)
mkdir -p /brogue/auth /brogue/userdata

# Per-user dirs are written by Brogue, which runs as the unprivileged games user
# (see brogue-run.sh), so the parent must be writable by that user.
chown -R games:games /brogue/userdata

# The pubkey store holds offline refresh tokens (bearer credentials). Keep it
# root-owned and unreadable by the games user that Brogue runs as — authlaunch
# runs as root and is the only thing that touches it.
chown -R root:root /brogue/auth
chmod 700 /brogue/auth

# authlaunch resolves the connecting client's key (forwarded in $SSH_USER_PUBKEY
# by the wish server) to an OIDC identity, then execs brogue-run.sh with
# $AUTH_NICK set. SSO is mandatory — with no key or with the OIDC env vars unset
# it refuses the session, so Brogue never runs for an unidentified user.
exec /usr/local/bin/authlaunch -- /usr/local/bin/brogue-run.sh
