// Command authlaunch is the SSO gate that sits between the wish SSH server and
// the game (Brogue). The wish server forwards the connecting client's
// authenticated public key in $SSH_USER_PUBKEY; authlaunch resolves that key to
// an OIDC identity, then execs the target command (the Brogue launcher) with
// the resolved nickname exported as $AUTH_NICK. Both the key and the OIDC
// configuration are mandatory: a session with no key, or a deployment with OIDC
// unset, is refused before the game ever runs.
//
// Flow per connection:
//
//   - Known key   -> exchange the stored offline refresh token for a fresh ID
//     token (a live re-check against the OIDC provider: account still valid AND still in
//     the allowed group), then log in.
//   - Unknown key -> run the OIDC device-authorization flow in the PTY; on
//     success bind the key to the identity for next time.
//   - No key      -> refuse (the wish server already disconnects these).
//
// Identity reaches the game purely through the environment: authlaunch execs the
// target with $AUTH_NICK set to the resolved nickname, and the launcher uses it
// to pick the per-user save directory. (There is no dgamelaunch layer and no
// DGLAUTH handoff.)
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	gossh "golang.org/x/crypto/ssh"
)

// nickEnvVar names the environment variable through which authlaunch hands the
// resolved nickname to the target launcher (brogue-run.sh), which uses
// it as the per-user save-directory name.
const nickEnvVar = "AUTH_NICK"

// config is the full runtime configuration, from env vars (see README).
type config struct {
	oidc       oidcConfig
	keysPath   string
	nicksPath  string
	pubkeyLine string // $SSH_USER_PUBKEY: authorized_keys line, may be empty
	target     []string
}

func loadConfig(argv []string) (config, error) {
	c := config{
		oidc: oidcConfig{
			IssuerURL:    os.Getenv("OIDC_ISSUER_URL"),
			ClientID:     os.Getenv("OIDC_CLIENT_ID"),
			AllowedGroup: os.Getenv("OIDC_ALLOWED_GROUP"),
			NickClaim:    envOr("OIDC_NICK_CLAIM", "game_nickname"),
			GroupsClaim:  envOr("OIDC_GROUPS_CLAIM", "groups"),
			FrontendTS:   os.Getenv("OIDC_FRONTEND_URL_TS"),
			Ingress:      envOr("BROGUE_INGRESS", "lan"),
		},
		keysPath:   envOr("AUTH_KEYS_PATH", "/brogue/auth/keys.tsv"),
		nicksPath:  envOr("AUTH_NICKS_PATH", "/brogue/auth/nicks.tsv"),
		pubkeyLine: strings.TrimSpace(os.Getenv(pubkeyEnvVar)),
		target:     argv,
	}
	if len(c.target) == 0 {
		return c, errors.New("no target command given (expected: authlaunch -- <cmd> [args...])")
	}
	return c, nil
}

const pubkeyEnvVar = "SSH_USER_PUBKEY"

// oidcConfigured reports whether the three required OIDC variables are set.
// SSO is mandatory: without it authlaunch refuses the session rather than
// passing through to the game — an unidentified user never reaches Brogue.
func (c config) oidcConfigured() bool {
	return c.oidc.IssuerURL != "" && c.oidc.ClientID != "" && c.oidc.AllowedGroup != ""
}

func main() {
	log.SetPrefix("authlaunch")
	routeServerLog()

	argv := targetArgs(os.Args[1:])
	cfg, err := loadConfig(argv)
	if err != nil {
		log.Fatal("configuration error", "error", err)
	}

	if !cfg.oidcConfigured() {
		// Never hand an unauthenticated session to the game. This is a
		// deployment bug, not a user error — say so on both channels.
		fmt.Fprintf(os.Stdout, "\r\nThis server is misconfigured (sign-in is not set up). Please tell the admin.\r\n")
		log.Fatal("OIDC not configured (need OIDC_ISSUER_URL, OIDC_CLIENT_ID, OIDC_ALLOWED_GROUP); refusing session")
	}

	nick, err := authenticate(cfg, os.Stdout)
	if err != nil {
		if errors.Is(err, errAuthCancelled) {
			fmt.Fprintf(os.Stdout, "\r\nSign-in cancelled. Goodbye.\r\n")
			log.Info("sign-in cancelled by user")
			os.Exit(0)
		}
		// The message has already been shown to the user on stdout; keep the
		// server-side log terse.
		fmt.Fprintf(os.Stdout, "\r\nSign-in failed: %s\r\n", err)
		log.Error("authentication failed", "error", err)
		os.Exit(1)
	}

	execTarget(cfg.target, nick)
}

// routeServerLog points the logger at the fd the wish server passed via
// $AUTHLAUNCH_LOG_FD. Our stderr is the session PTY, so without this every log
// line would print on the player's screen (cleared by the game's first redraw)
// and never reach the container logs. When the variable is unset or bogus
// (running outside the wish server, e.g. local dev), stderr is kept.
func routeServerLog() {
	v := os.Getenv("AUTHLAUNCH_LOG_FD")
	if v == "" {
		return
	}
	fd, err := strconv.Atoi(v)
	if err != nil || fd <= 2 {
		return
	}
	f := os.NewFile(uintptr(fd), "server-log")
	if f == nil {
		return
	}
	if _, err := f.Stat(); err != nil {
		return // fd not actually open; keep stderr
	}
	log.SetOutput(f)
}

// targetArgs returns the command after a "--" separator, or all args if there is
// none.
func targetArgs(args []string) []string {
	for i, a := range args {
		if a == "--" {
			return args[i+1:]
		}
	}
	return args
}

// authenticate resolves the connecting client to a Brogue nickname, running
// either the refresh-token re-check (known key) or the device flow (unknown
// key). It prints user-facing progress to out. A session without a usable
// public key is refused outright — the wish server already disconnects keyless
// clients, so this is defense-in-depth for running outside it.
func authenticate(cfg config, out *os.File) (string, error) {
	ctx := context.Background()

	fp := fingerprint(cfg.pubkeyLine)
	if fp == "" {
		return "", errors.New("no SSH public key was presented; reconnect with a key pair (ssh-keygen -t ed25519)")
	}

	discCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	oc, err := newOIDCClient(discCtx, cfg.oidc)
	if err != nil {
		return "", fmt.Errorf("connecting to identity provider: %w", err)
	}

	ks := newKeyStore(cfg.keysPath)
	nr := newNickRegistry(cfg.nicksPath)

	// Known-key fast path: re-validate via the stored offline refresh token.
	if rec, found, err := ks.Lookup(fp); err != nil {
		log.Warn("keystore lookup failed; falling back to device flow", "error", err)
	} else if found {
		if nick, ok := refreshKnownKey(ctx, cfg, oc, ks, nr, rec); ok {
			return nick, nil
		}
		// Refresh failed or access was revoked: fall through to a fresh
		// device flow so the user can re-establish (or be told no).
	}

	// Device-authorization flow for an unknown key, or after a failed refresh.
	id, refresh, err := oc.DeviceFlow(ctx, out)
	if err != nil {
		return "", err
	}
	if !id.InAllowedGroup(cfg.oidc.AllowedGroup) {
		_ = ks.Delete(fp)
		return "", fmt.Errorf("your account is not a member of %q", cfg.oidc.AllowedGroup)
	}
	nick, ok := id.nick()
	if !ok {
		return "", errors.New("no usable Brogue nickname; set one in your account profile")
	}
	// Bind the nick to this identity before we register the key. A first-time
	// collision is fatal: the user must pick a free nick (we don't want to
	// register their key to a nick they can't use).
	if err := nr.Claim(nick, id.Sub); err != nil {
		return "", err
	}

	now := time.Now()
	if err := ks.Upsert(keyRecord{
		Fingerprint:  fp,
		Sub:          id.Sub,
		Nick:         nick,
		RegisteredAt: now,
		LastSeen:     now,
		RefreshToken: refresh,
	}); err != nil {
		log.Warn("could not persist key registration", "error", err)
	}
	fmt.Fprintf(out, "\r\nThis key is now registered to %q.\r\n", nick)
	return nick, nil
}

// refreshKnownKey re-validates a recognized key against the OIDC provider using its
// stored offline refresh token. Returns (nick, true) on success; on any failure
// or revocation it returns ("", false) so the caller falls back to a device
// flow. A confirmed group removal also deletes the key.
func refreshKnownKey(ctx context.Context, cfg config, oc *oidcClient, ks *keyStore, nr *nickRegistry, rec keyRecord) (string, bool) {
	rCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	id, refresh, err := oc.Refresh(rCtx, rec.RefreshToken)
	if err != nil {
		log.Info("refresh failed; will re-run device flow", "sub", rec.Sub, "error", err)
		return "", false
	}
	if !id.InAllowedGroup(cfg.oidc.AllowedGroup) {
		log.Info("access revoked (no longer in allowed group); deleting key", "sub", rec.Sub)
		_ = ks.Delete(rec.Fingerprint)
		return "", false
	}
	nick, ok := id.nick()
	if !ok {
		nick = rec.Nick // keep the previously-known nick if the attribute vanished
	}
	// Enforce nick ownership. If the user changed their game_nickname to one
	// already owned by someone else, don't lock this returning player out — keep
	// them on the nick they already own (which they can always reclaim).
	if err := nr.Claim(nick, rec.Sub); err != nil {
		log.Warn("requested nick unavailable; keeping previously-owned nick",
			"sub", rec.Sub, "requested", nick, "error", err)
		nick = rec.Nick
		if err := nr.Claim(nick, rec.Sub); err != nil {
			log.Warn("could not reclaim own nick; re-running device flow", "sub", rec.Sub, "nick", nick, "error", err)
			return "", false
		}
	}

	if refresh == "" {
		refresh = rec.RefreshToken // rotation disabled: keep the existing token
	}
	rec.Nick = nick
	rec.LastSeen = time.Now()
	rec.RefreshToken = refresh
	if err := ks.Upsert(rec); err != nil {
		log.Warn("could not update keystore after refresh", "error", err)
	}
	return nick, true
}

// fingerprint computes the SHA256 fingerprint of an authorized_keys line. It
// returns "" for an empty or unparseable line (treated as "no key").
func fingerprint(authorizedKey string) string {
	if authorizedKey == "" {
		return ""
	}
	key, _, _, _, err := gossh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		log.Warn("could not parse forwarded public key", "error", err)
		return ""
	}
	return gossh.FingerprintSHA256(key)
}

// execTarget replaces this process with the target command, exporting the
// resolved nickname as $AUTH_NICK for the launcher. Using execve preserves the
// pid (and thus the process group the wish server set up for hangup-save on
// disconnect).
func execTarget(target []string, nick string) {
	env := os.Environ()
	if nick != "" {
		env = append(env, nickEnvVar+"="+nick)
	}
	if err := syscall.Exec(target[0], target, env); err != nil {
		log.Fatal("exec target failed", "cmd", target[0], "error", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
