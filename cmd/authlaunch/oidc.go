package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/term"
)

// oidcConfig is the OIDC wiring, all sourced from non-secret env vars. The
// client is a public OIDC client with the Device Authorization Grant enabled;
// there is no client secret.
type oidcConfig struct {
	IssuerURL    string // e.g. https://auth.example.com
	ClientID     string
	AllowedGroup string // group that grants access, e.g. players
	NickClaim    string // token claim holding the Brogue nickname attribute
	GroupsClaim  string // token claim holding group membership

	// FrontendTS, set together with a Tailscale-origin session
	// (Ingress == "tailscale"), is the scheme+host the human-facing
	// device-verification URL is rewritten to so a shared-in player reaches a
	// resolvable approval page (auth.example.com is internal-only). Empty disables
	// rewriting — LAN-only deployments behave exactly as before. Only the
	// front-channel URL is touched; discovery, device-auth, and token polling
	// keep hitting IssuerURL, so the token's iss is unchanged.
	FrontendTS string // $OIDC_FRONTEND_URL_TS, e.g. https://auth.your-tailnet.ts.net
	Ingress    string // $BROGUE_INGRESS: "lan" (default) or "tailscale"
}

// Identity is the validated result of an OIDC exchange.
type Identity struct {
	Sub               string
	PreferredUsername string
	Groups            []string
	nickAttr          string // raw game_nickname claim, before sanitization
}

// oidcClient talks to the OIDC provider. It performs OIDC discovery once at construction
// and reuses the resulting endpoints + verifier for the life of the process
// (one process per SSH session).
type oidcClient struct {
	cfg      oidcConfig
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func newOIDCClient(ctx context.Context, cfg oidcConfig) (*oidcClient, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}

	// The device_authorization_endpoint is not part of oauth2.Endpoint as
	// returned by go-oidc, so pull it from the discovery document directly.
	var disco struct {
		DeviceAuthURL string `json:"device_authorization_endpoint"`
	}
	if err := provider.Claims(&disco); err != nil {
		return nil, fmt.Errorf("oidc discovery claims: %w", err)
	}
	if disco.DeviceAuthURL == "" {
		return nil, fmt.Errorf("issuer %q advertises no device_authorization_endpoint", cfg.IssuerURL)
	}

	return &oidcClient{
		cfg: cfg,
		oauth: &oauth2.Config{
			ClientID: cfg.ClientID,
			Endpoint: oauth2.Endpoint{
				AuthURL:       provider.Endpoint().AuthURL,
				TokenURL:      provider.Endpoint().TokenURL,
				DeviceAuthURL: disco.DeviceAuthURL,
			},
			// offline_access is what makes the OIDC provider issue an *offline* refresh
			// token, decoupled from the short SSO session idle/max timeouts, so
			// it survives between a player's sessions and we can re-validate
			// against it on every reconnect.
			Scopes: []string{oidc.ScopeOpenID, "profile", oidc.ScopeOfflineAccess},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

// DeviceFlow runs the device-authorization grant: it prints the verification URL
// and user code to out, blocks until the user approves in their browser (or the
// code expires), then validates the resulting ID token. The returned string is
// the offline refresh token to persist.
func (c *oidcClient) DeviceFlow(ctx context.Context, out io.Writer) (Identity, string, error) {
	da, err := c.oauth.DeviceAuth(ctx)
	if err != nil {
		return Identity{}, "", fmt.Errorf("start device flow: %w", err)
	}

	// On a Tailscale-origin session the verification URL the OIDC provider returns points
	// at the internal-only auth.example.com, which a shared-in player can't
	// resolve. Rewrite its scheme+host to the reachable .ts.net frontend,
	// preserving the path and user_code. The back-channel poll above and below
	// is untouched, so the token's iss stays auth.example.com.
	if c.cfg.Ingress == ingressTailscale && c.cfg.FrontendTS != "" {
		da.VerificationURI = rewriteHost(da.VerificationURI, c.cfg.FrontendTS)
		da.VerificationURIComplete = rewriteHost(da.VerificationURIComplete, c.cfg.FrontendTS)
	}

	// Prefer the complete URI (user code embedded) for the QR so a phone scan
	// lands straight on the approval screen with nothing to type.
	qrURL := da.VerificationURIComplete
	if qrURL == "" {
		qrURL = da.VerificationURI
	}
	fmt.Fprint(out, "\r\nTo sign in, scan this QR code with your phone's camera:\r\n\r\n")
	renderQRCode(&crlfWriter{w: out}, qrURL)

	if da.VerificationURIComplete != "" {
		fmt.Fprintf(out, "\r\nOr open this URL in a browser:\r\n\r\n    %s\r\n\r\n", da.VerificationURIComplete)
		fmt.Fprintf(out, "(or visit %s and enter code %s)\r\n\r\n", da.VerificationURI, da.UserCode)
	} else {
		fmt.Fprintf(out, "\r\nOr visit:\r\n\r\n    %s\r\n\r\nand enter code: %s\r\n\r\n", da.VerificationURI, da.UserCode)
	}
	fmt.Fprint(out, "Waiting for you to approve in your browser... (press any key to cancel)\r\n")

	// Let the player bail out of the wait with a keypress instead of having to
	// kill their SSH client. We read the PTY in the background and cancel the
	// poll's context on the first byte; DeviceAccessToken then returns promptly.
	// Raw mode makes a single keystroke arrive immediately (cooked mode would
	// hold it until Enter) and delivers Ctrl-C as a byte rather than a signal
	// with no foreground process group to receive it. The reader goroutine is
	// left to die with the process: on success we exec the game launcher, which
	// replaces the image before any stray keystroke could be consumed.
	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if oldState, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
		go func() {
			var b [1]byte
			if _, err := os.Stdin.Read(b[:]); err == nil {
				cancel()
			}
		}()
	}

	tok, err := c.oauth.DeviceAccessToken(pollCtx, da)
	if err != nil {
		// Our cancel is the only thing that cancels pollCtx (the parent ctx is
		// never cancelled here), so a cancelled context means the user aborted.
		if pollCtx.Err() != nil {
			return Identity{}, "", errAuthCancelled
		}
		return Identity{}, "", fmt.Errorf("device authorization not completed: %w", err)
	}
	return c.validate(ctx, tok)
}

// errAuthCancelled signals that the user pressed a key to abort the device flow
// rather than completing (or letting it expire). main turns it into a friendly
// goodbye and a clean exit.
var errAuthCancelled = errors.New("sign-in cancelled by user")

// Refresh re-validates a known user by exchanging their stored offline refresh
// token for a fresh ID token. A successful refresh proves the account is still
// alive; the freshly minted token's claims (re-evaluated by the OIDC provider) are what
// catch a group removal. The returned string is the rotated refresh token.
func (c *oidcClient) Refresh(ctx context.Context, refreshToken string) (Identity, string, error) {
	ts := c.oauth.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken})
	tok, err := ts.Token()
	if err != nil {
		return Identity{}, "", fmt.Errorf("refresh: %w", err)
	}
	return c.validate(ctx, tok)
}

// validate verifies the ID token embedded in tok and extracts the identity. The
// rotated refresh token from tok is returned so callers can persist it.
func (c *oidcClient) validate(ctx context.Context, tok *oauth2.Token) (Identity, string, error) {
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return Identity{}, "", fmt.Errorf("token response carried no id_token")
	}
	idToken, err := c.verifier.Verify(ctx, rawID)
	if err != nil {
		return Identity{}, "", fmt.Errorf("verify id_token: %w", err)
	}

	var claims map[string]interface{}
	if err := idToken.Claims(&claims); err != nil {
		return Identity{}, "", fmt.Errorf("decode claims: %w", err)
	}

	id := Identity{Sub: idToken.Subject}
	if v, ok := claims["preferred_username"].(string); ok {
		id.PreferredUsername = v
	}
	id.Groups = toStringSlice(claims[c.cfg.GroupsClaim])
	id.nickAttr, _ = claims[c.cfg.NickClaim].(string)

	// The rotated refresh token may be empty if the OIDC provider has refresh-token
	// rotation disabled; in that case keep reusing the one we have.
	rt := tok.RefreshToken
	return id, rt, nil
}

// nickAttr is the raw game_nickname claim value; resolved into a sanitized
// nick by pickNick. Kept unexported on Identity so callers go through Resolve.
func (id Identity) nick() (string, bool) { return pickNick(id.nickAttr, id.PreferredUsername) }

// InAllowedGroup reports whether this identity is a member of the configured
// access group.
func (id Identity) InAllowedGroup(group string) bool { return groupAllowed(id.Groups, group) }
