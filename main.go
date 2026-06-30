package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/logging"
	gossh "golang.org/x/crypto/ssh"
)

// hangupGrace is how long the child process group has to react to the SIGHUP
// we send on disconnect (Brogue's hangup save) before we hard-kill it.
const hangupGrace = 15 * time.Second

// pubkeyEnvVar names the environment variable through which we hand the
// connecting client's authenticated public key down to WISH_COMMAND (the auth
// launcher). The value is a single authorized_keys line ("<type> <base64>").
const pubkeyEnvVar = "SSH_USER_PUBKEY"

// logFdEnvVar tells the child which fd carries the server's log stream. The
// child's stdio is the session PTY — anything it writes there lands on the
// player's screen (and is promptly cleared by the game's first redraw),
// never in the container logs. So we pass our own stderr as an extra fd and
// authlaunch directs its server-side logging at it.
const logFdEnvVar = "AUTHLAUNCH_LOG_FD"

// ingressEnvVar tells the child (authlaunch) which ingress path the session
// arrived on, so it can hand Tailscale-origin players a device-auth URL they
// can actually resolve. The local listener port is the discriminator (see the
// two listeners in main) — deterministic and immune to any SNAT in the
// Tailscale proxy, which is why we don't sniff the client's source IP.
const ingressEnvVar = "BROGUE_INGRESS"

// Listener addresses and their ingress tags. Both listeners present the same
// SSH host key (one identity) and differ only in which ingress maps to them:
// :2222 is the LAN path, :2223 the Tailscale path.
const (
	lanAddr          = ":2222"
	tailscaleAddr    = ":2223"
	ingressLAN       = "lan"
	ingressTailscale = "tailscale"
)

// ctxKeyPubKey is the ssh.Context key under which the public-key auth handler
// stashes the authenticated key for the session handler to read.
type ctxKeyPubKey struct{}

// publicKeyHandler accepts every offered key. We do NOT use the key to decide
// access here — that decision belongs to the downstream OIDC launcher, which
// looks the key up against the OIDC provider. Returning true matters for one reason:
// golang.org/x/crypto/ssh only completes public-key auth after it has verified
// the client actually holds the matching private key. So a key that reaches us
// here (and that we stash for the launcher) is cryptographically authenticated,
// not merely advertised — the launcher can trust the fingerprint.
func publicKeyHandler(ctx ssh.Context, key ssh.PublicKey) bool {
	ctx.SetValue(ctxKeyPubKey{}, strings.TrimSpace(string(gossh.MarshalAuthorizedKey(key))))
	return true
}

// keyboardInteractiveHandler lets clients that offer no public key (e.g. a
// fresh laptop, or `ssh -o PubkeyAuthentication=no`) still open a session —
// but only so the session handler can print noKeyMessage and close. If we
// rejected them at the auth layer instead, all they'd see is an opaque
// "Permission denied (publickey,keyboard-interactive)". A key pair is required
// because identity is bound to the key: the launcher resolves it against
// the OIDC provider and remembers it for next time.
func keyboardInteractiveHandler(ssh.Context, gossh.KeyboardInteractiveChallenge) bool {
	return true
}

// noKeyMessage is shown to clients that authenticated without a public key,
// just before we close the session. Explicit \r\n: the client's terminal is in
// raw mode for the PTY session, and these writes go straight down the channel
// (no line-discipline translation), so a bare \n would staircase.
const noKeyMessage = "" +
	"This server requires an SSH key pair — your key is how it remembers you.\r\n" +
	"\r\n" +
	"If you don't have one yet, generate one with:\r\n" +
	"\r\n" +
	"    ssh-keygen -t ed25519\r\n" +
	"\r\n" +
	"then simply reconnect; ssh offers your keys automatically.\r\n"

// runSession execs the per-connection command attached to the session's PTY.
//
// It deliberately does NOT use wish.Command. wish.Command runs the command via
// exec.CommandContext and, on disconnect (session context cancelled), reacts by
// SIGKILLing the child — which would kill Brogue outright with no chance to
// save. Our launch chain is a pure exec sequence (brogue-launch.sh -> authlaunch
// -> brogue-run.sh -> setpriv -> brogue), so the child process IS Brogue.
//
// Instead, on a dropped session we send SIGHUP, which our forked Brogue turns
// into a clean save-and-exit ("hangup save"). We put the child in its own
// process group (Setpgid) and signal the whole group (negative PID) as defense
// in depth — harmless since the group is just Brogue — and WaitDelay then
// hard-kills anything that ignores the hangup so we never leak a process.
func runSession(s ssh.Session, command, ingress string) error {
	cmd := exec.CommandContext(s.Context(), command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	ppty, _, hasPty := s.Pty()

	// Build the child environment. We set TERM explicitly from the PTY the
	// client negotiated, for two reasons: bash (brogue-launch.sh's interpreter)
	// defaults an unset TERM to "dumb", which brogue-launch.sh rejects; and
	// deriving it per-session gives each player their real terminal type
	// (xterm-256color, tmux-256color, ...) instead of relying on a container-
	// wide TERM injected via a TTY. Strip any inherited TERM so the session's
	// value wins, then forward the authenticated public key so the launcher
	// can resolve the connecting client against the OIDC provider. (Keyless sessions
	// never reach this point — the middleware disconnects them first.)
	cmd.Env = make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TERM=") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	if hasPty && ppty.Term != "" {
		cmd.Env = append(cmd.Env, "TERM="+ppty.Term)
	}
	if v, ok := s.Context().Value(ctxKeyPubKey{}).(string); ok && v != "" {
		cmd.Env = append(cmd.Env, pubkeyEnvVar+"="+v)
	}
	// Tag the session's ingress path so authlaunch can rewrite the device-auth
	// verification URL for Tailscale-origin players (see ingressEnvVar).
	cmd.Env = append(cmd.Env, ingressEnvVar+"="+ingress)
	// Hand the server log stream (our stderr) to the child as fd 3 so
	// authlaunch's log lines reach the container logs instead of the PTY.
	// ExtraFiles[0] always becomes fd 3 in the child; bash and the exec chain
	// (brogue-launch.sh -> authlaunch -> brogue-run.sh -> brogue) preserve
	// inherited fds.
	cmd.ExtraFiles = []*os.File{os.Stderr}
	cmd.Env = append(cmd.Env, logFdEnvVar+"=3")
	cmd.Cancel = func() error {
		// Send SIGHUP (not SIGKILL) so Brogue's hangup handler saves the game.
		// Negative PID signals the whole process group; with Setpgid the child
		// is its own group leader, so its PID is the group ID. The exec chain
		// keeps Brogue at that PID, so this reaches it.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
	}
	cmd.WaitDelay = hangupGrace

	if hasPty {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = ppty.Slave, ppty.Slave, ppty.Slave
	} else {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = s, s, s.Stderr()
	}

	err := cmd.Run()
	// A dropped connection cancels the session context; that's the normal
	// autosave path, not a failure worth reporting to a client that's gone.
	if s.Context().Err() != nil {
		return nil
	}
	return err
}

// newServer builds a wish SSH server bound to addr whose sessions are tagged
// with the given ingress label ("lan" or "tailscale"). Every listener shares
// the same host key, auth handlers, and middleware; only the address and the
// ingress tag differ. The tag is threaded into runSession, which exports it to
// the child as $BROGUE_INGRESS.
func newServer(addr, ingress, command, hostKeyPath string) (*ssh.Server, error) {
	return wish.NewServer(
		wish.WithAddress(addr),
		wish.WithHostKeyPath(hostKeyPath),

		// Accept any public key (identity is resolved downstream against
		// the OIDC provider) but capture it. Keyless clients are let in only far
		// enough for the session middleware to print noKeyMessage and
		// disconnect them — a key pair is required to play.
		wish.WithPublicKeyAuth(publicKeyHandler),
		wish.WithKeyboardInteractiveAuth(keyboardInteractiveHandler),

		ssh.AllocatePty(),
		wish.WithMiddleware(
			func(next ssh.Handler) ssh.Handler {
				return func(s ssh.Session) {
					// A key pair is mandatory: identity downstream is keyed by
					// the public key. Keyboard-interactive sessions carry none,
					// so explain and hang up before spawning anything.
					if v, _ := s.Context().Value(ctxKeyPubKey{}).(string); v == "" {
						_, _ = s.Write([]byte(noKeyMessage))
						_ = s.Exit(1)
						return
					}
					if err := runSession(s, command, ingress); err != nil {
						wish.Fatalln(s, err)
					}
					next(s)
				}
			},
			logging.Middleware(),
		),
	)
}

func main() {
	command := os.Getenv("WISH_COMMAND")
	if command == "" {
		log.Fatal("WISH_COMMAND environment variable must be set")
	}

	hostKeyPath := os.Getenv("WISH_HOST_KEY_PATH")
	if hostKeyPath == "" {
		hostKeyPath = ".ssh/term_info_ed25519"
	}

	// Two listeners, one identity. :2222 is the LAN path;
	// :2223 is the Tailscale path. The port a
	// connection lands on is the only thing that distinguishes the two ingress
	// routes, so it's what we use to tag the session — see ingressEnvVar.
	listeners := []struct{ addr, ingress string }{
		{lanAddr, ingressLAN},
		{tailscaleAddr, ingressTailscale},
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	servers := make([]*ssh.Server, 0, len(listeners))
	for _, l := range listeners {
		srv, err := newServer(l.addr, l.ingress, command, hostKeyPath)
		if err != nil {
			log.Fatal("Could not create server", "addr", l.addr, "error", err)
		}
		servers = append(servers, srv)
		go func(srv *ssh.Server, addr string) {
			log.Info("Starting SSH server", "addr", addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
				log.Error("Server failed", "addr", addr, "error", err)
				done <- nil
			}
		}(srv, l.addr)
	}

	<-done
	log.Info("Stopping SSH servers")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
			log.Error("Could not stop server", "error", err)
		}
	}
}
