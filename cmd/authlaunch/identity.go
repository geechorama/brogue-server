package main

import "strings"

// maxNickLen bounds the resolved Brogue nickname. The nick becomes the player's
// per-user save-directory name (/brogue/userdata/<nick>), so it MUST be a safe,
// single path segment: we restrict it to [A-Za-z0-9]+ (no slashes, dots, or
// whitespace — no path traversal) and cap it at 15 to keep directory names tidy
// and the name sane on Brogue's sidebar and high-score table.
const maxNickLen = 15

// sanitizeNick reduces an arbitrary string to a filesystem-safe nickname:
// alphanumerics only, truncated to maxNickLen. Everything else is dropped. The
// result may be empty, which callers treat as "no usable nick".
func sanitizeNick(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			continue
		}
		if b.Len() >= maxNickLen {
			break
		}
	}
	return b.String()
}

// pickNick resolves the dungeon name for an authenticated user. The dedicated
// game_nickname attribute (set by the player in their profile at the OIDC provider) wins;
// if it is blank or sanitizes away to nothing, we fall back to the sanitized
// preferred_username so a player who never set the attribute still gets a stable
// name. Returns ("", false) when neither yields anything usable.
func pickNick(nickAttr, preferredUsername string) (string, bool) {
	if n := sanitizeNick(nickAttr); n != "" {
		return n, true
	}
	if n := sanitizeNick(preferredUsername); n != "" {
		return n, true
	}
	return "", false
}

// normalizeGroup strips a leading slash so that an OIDC provider's full group paths
// ("/players") compare equal to a bare configured group name
// ("players").
func normalizeGroup(g string) string {
	return strings.TrimPrefix(strings.TrimSpace(g), "/")
}

// groupAllowed reports whether want appears in the token's group claim. Matching
// is exact after normalizing the leading slash on both sides.
func groupAllowed(groups []string, want string) bool {
	want = normalizeGroup(want)
	if want == "" {
		return false
	}
	for _, g := range groups {
		if normalizeGroup(g) == want {
			return true
		}
	}
	return false
}

// toStringSlice coerces a decoded JSON claim into a []string. OIDC providers emit the
// groups/roles claim as a JSON array (decoded to []interface{} of strings), but
// we also accept a bare string for robustness against odd provider configs.
func toStringSlice(v interface{}) []string {
	switch t := v.(type) {
	case []string:
		return t
	case string:
		return []string{t}
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
