package main

import "net/url"

// ingressTailscale is the $BROGUE_INGRESS value the wish server sets on a
// Tailscale-origin session. (The wish server defines the same value in its own
// package; authlaunch is a separate binary, so it carries its own copy.)
const ingressTailscale = "tailscale"

// rewriteHost returns raw with its scheme and host replaced by those of base,
// preserving the path, query, and fragment. It points the device-flow
// verification URL at a hostname the client can actually resolve (the Tailscale
// .ts.net frontend) without disturbing the path or the user_code query.
//
// It is best-effort: an empty raw or base, or either failing to parse (or a
// base carrying no host), yields raw unchanged — rewriting must never blank out
// a URL the player needs to complete sign-in.
func rewriteHost(raw, base string) string {
	if raw == "" || base == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	b, err := url.Parse(base)
	if err != nil || b.Host == "" {
		return raw
	}
	u.Scheme = b.Scheme
	u.Host = b.Host
	return u.String()
}
