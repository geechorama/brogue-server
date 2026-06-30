package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRecordRoundTrip(t *testing.T) {
	in := keyRecord{
		Fingerprint:  "SHA256:abc123",
		Sub:          "uuid-1",
		Nick:         "Conan",
		RegisteredAt: time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC),
		LastSeen:     time.Date(2026, 6, 2, 11, 0, 0, 0, time.UTC),
		RefreshToken: "eyJhbGciOi.refresh.token",
	}
	out, ok := parseRecord(formatRecord(in))
	if !ok {
		t.Fatal("parseRecord rejected a record it just formatted")
	}
	if out != in {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestParseRecordRejectsShortLines(t *testing.T) {
	if _, ok := parseRecord("only\ttwo"); ok {
		t.Error("expected short line to be rejected")
	}
	if _, ok := parseRecord("\t\t\t\t\t"); ok {
		t.Error("expected empty-fingerprint line to be rejected")
	}
}

func TestKeyStoreUpsertLookupDelete(t *testing.T) {
	ks := newKeyStore(filepath.Join(t.TempDir(), "keys.tsv"))

	if _, found, err := ks.Lookup("SHA256:nope"); err != nil || found {
		t.Fatalf("lookup on empty store: found=%v err=%v", found, err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	rec := keyRecord{Fingerprint: "SHA256:k1", Sub: "s1", Nick: "alice", RegisteredAt: now, LastSeen: now, RefreshToken: "rt1"}
	if err := ks.Upsert(rec); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, found, err := ks.Lookup("SHA256:k1")
	if err != nil || !found {
		t.Fatalf("lookup after upsert: found=%v err=%v", found, err)
	}
	if got != rec {
		t.Errorf("lookup mismatch:\n got %+v\nwant %+v", got, rec)
	}

	// Upsert with same fingerprint must replace (rotate token), not duplicate.
	rec.RefreshToken = "rt2"
	if err := ks.Upsert(rec); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	recs, err := readRecords(ks.path)
	if err != nil || len(recs) != 1 {
		t.Fatalf("expected exactly 1 record after re-upsert, got %d (err=%v)", len(recs), err)
	}
	if recs[0].RefreshToken != "rt2" {
		t.Errorf("token not rotated: got %q", recs[0].RefreshToken)
	}

	if err := ks.Delete("SHA256:k1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := ks.Lookup("SHA256:k1"); found {
		t.Error("record still present after delete")
	}
}
