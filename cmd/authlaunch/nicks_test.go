package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestNickRegistryClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nicks.tsv")
	nr := newNickRegistry(path)

	// First claim of a free nick succeeds.
	if err := nr.Claim("Alice", "sub-A"); err != nil {
		t.Fatalf("first claim: unexpected error %v", err)
	}
	// Same subject re-claiming its own nick is a no-op success (idempotent
	// across reconnects).
	if err := nr.Claim("Alice", "sub-A"); err != nil {
		t.Fatalf("re-claim by owner: unexpected error %v", err)
	}
	// A different subject claiming the same nick is rejected.
	err := nr.Claim("Alice", "sub-B")
	if err == nil {
		t.Fatal("expected collision error when another subject claims the nick")
	}
	var taken errNickTaken
	if !errors.As(err, &taken) {
		t.Errorf("expected errNickTaken, got %T: %v", err, err)
	}
}

func TestNickRegistryCaseInsensitive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nicks.tsv")
	nr := newNickRegistry(path)

	if err := nr.Claim("Alice", "sub-A"); err != nil {
		t.Fatalf("claim Alice: %v", err)
	}
	// nicks are compared case-insensitively, so "alice" is the same
	// account: a different subject must not be able to claim it.
	if err := nr.Claim("alice", "sub-B"); err == nil {
		t.Error("case-variant nick should collide for a different subject")
	}
	// ...but the owner may resolve to a case variant of their own nick.
	if err := nr.Claim("ALICE", "sub-A"); err != nil {
		t.Errorf("owner reclaiming case-variant of own nick: %v", err)
	}
}

func TestNickRegistryPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nicks.tsv")
	if err := newNickRegistry(path).Claim("Wizard", "sub-A"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// A fresh registry instance must read the prior claim back from disk.
	if err := newNickRegistry(path).Claim("Wizard", "sub-B"); err == nil {
		t.Error("claim should persist across registry instances")
	}
}

func TestNickRegistryRejectsEmpty(t *testing.T) {
	nr := newNickRegistry(filepath.Join(t.TempDir(), "nicks.tsv"))
	if err := nr.Claim("", "sub-A"); err == nil {
		t.Error("empty nick must be rejected")
	}
	if err := nr.Claim("Alice", ""); err == nil {
		t.Error("empty subject must be rejected")
	}
}

func TestParseNickRecord(t *testing.T) {
	if _, ok := parseNickRecord("Alice\tsub-A\t2026-06-02T00:00:00Z"); !ok {
		t.Error("well-formed line should parse")
	}
	if _, ok := parseNickRecord("Alice\tsub-A"); ok {
		t.Error("line missing claimed_at column should be rejected")
	}
	if _, ok := parseNickRecord("\tsub-A\t2026-06-02T00:00:00Z"); ok {
		t.Error("empty nick should be rejected")
	}
	if _, ok := parseNickRecord("Alice\t\t2026-06-02T00:00:00Z"); ok {
		t.Error("empty sub should be rejected")
	}
}
