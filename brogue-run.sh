#!/bin/bash
set -e

# Run by authlaunch (as root) after it resolves the player's identity. authlaunch
# exports $AUTH_NICK; we use it as the per-user save-directory name. Brogue writes
# its save (.broguesave), recordings (.broguerec) and scores (BrogueHighScores.txt,
# BrogueRunHistory.txt) to the current working directory, so we cd into the user's
# dir before launching. Read-only game resources live in /opt/brogue (--data-dir).

NICK="${AUTH_NICK:?authlaunch did not provide AUTH_NICK}"
USERDIR="/brogue/userdata/${NICK}"

mkdir -p "$USERDIR"
chown games:games "$USERDIR"
cd "$USERDIR"

# Drop from root to the unprivileged games user (uid 5, gid 60) before running
# the game. authlaunch needed root to read/write the root-owned keys.tsv; Brogue
# must not run as root. setpriv exec-replaces, so Brogue keeps this process's PID
# (and the wish server's process group), so the SIGHUP-on-disconnect hangup-save
# reaches it directly.
#
# --single-save (g10s fork): each player gets one save slot, consumed on load,
# so there's no going back to a previous save (NetHack-style). The title screen's
# "Load Game" browser becomes "Continue".
exec setpriv --reuid=5 --regid=60 --clear-groups -- \
    /opt/brogue/brogue -t --single-save --data-dir /opt/brogue
