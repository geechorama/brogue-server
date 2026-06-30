package main

import "testing"

func TestSanitizeNick(t *testing.T) {
	cases := map[string]string{
		"Conan":                "Conan",
		"conan the barbarian":  "conanthebarbari", // spaces dropped, capped at 15
		"a-b_c.d":              "abcd",
		"😀rogue":               "rogue",
		"":                     "",
		"!!!":                  "",
		"0123456789abcdefghij": "0123456789abcde", // 15-char cap
	}
	for in, want := range cases {
		if got := sanitizeNick(in); got != want {
			t.Errorf("sanitizeNick(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickNick(t *testing.T) {
	tests := []struct {
		name, attr, preferred, want string
		ok                          bool
	}{
		{"attr wins", "Gandalf", "gandalf_oidc", "Gandalf", true},
		{"fallback to username", "", "alice", "alice", true},
		{"attr sanitizes to empty -> fallback", "***", "bob", "bob", true},
		{"both empty", "", "", "", false},
		{"username sanitizes from email-ish", "", "alice@example.com", "aliceexamplecom", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pickNick(tc.attr, tc.preferred)
			if got != tc.want || ok != tc.ok {
				t.Errorf("pickNick(%q,%q) = (%q,%v), want (%q,%v)", tc.attr, tc.preferred, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestGroupAllowed(t *testing.T) {
	groups := []string{"/players", "/everyone"}
	if !groupAllowed(groups, "players") {
		t.Error("bare want should match slash-prefixed group")
	}
	if !groupAllowed(groups, "/players") {
		t.Error("slash-prefixed want should match")
	}
	if groupAllowed(groups, "admins") {
		t.Error("non-member group should not match")
	}
	if groupAllowed(groups, "") {
		t.Error("empty want must never match")
	}
	if groupAllowed(nil, "players") {
		t.Error("nil groups must not match")
	}
}

func TestToStringSlice(t *testing.T) {
	if got := toStringSlice([]interface{}{"a", "b", 3, "c"}); len(got) != 3 {
		t.Errorf("expected 3 strings (ints dropped), got %v", got)
	}
	if got := toStringSlice("solo"); len(got) != 1 || got[0] != "solo" {
		t.Errorf("bare string should become 1-element slice, got %v", got)
	}
	if got := toStringSlice(nil); got != nil {
		t.Errorf("nil claim should yield nil, got %v", got)
	}
}
