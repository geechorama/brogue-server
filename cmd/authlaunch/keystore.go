package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// keyRecord is one row of the pubkey store: an SSH public key bound to a
// OIDC identity, plus the offline refresh token we use to re-validate that
// identity (and its group membership) on every reconnect.
type keyRecord struct {
	Fingerprint  string    // SHA256:... fingerprint of the SSH public key
	Sub          string    // OIDC subject (stable user id)
	Nick         string    // resolved Brogue nickname (per-user save-dir name)
	RegisteredAt time.Time // first time this key was registered
	LastSeen     time.Time // last successful (re)validation
	RefreshToken string    // offline refresh token, rotated on each use
}

// keyStore is the flat-file pubkey store on the data volume. It is intentionally tiny:
// the whole file is read into memory under an advisory lock for every
// operation. The file holds bearer credentials (offline refresh tokens), so it
// is created 0600 and never world-readable.
//
// Concurrency: multiple SSH sessions in the same pod run their own authlaunch
// process, so every mutation takes an exclusive flock on a sidecar lock file
// before its read-modify-write. The lock file is separate from the data file so
// the atomic temp-file rename never invalidates a held lock.
type keyStore struct {
	path     string
	lockPath string
}

func newKeyStore(path string) *keyStore {
	return &keyStore{path: path, lockPath: path + ".lock"}
}

// Lookup returns the record for a fingerprint, if present.
func (ks *keyStore) Lookup(fingerprint string) (keyRecord, bool, error) {
	recs, err := readRecords(ks.path)
	if err != nil {
		return keyRecord{}, false, err
	}
	for _, r := range recs {
		if r.Fingerprint == fingerprint {
			return r, true, nil
		}
	}
	return keyRecord{}, false, nil
}

// Upsert inserts or replaces the record keyed by fingerprint, under lock.
func (ks *keyStore) Upsert(rec keyRecord) error {
	return ks.withLock(func(recs []keyRecord) ([]keyRecord, error) {
		out := make([]keyRecord, 0, len(recs)+1)
		replaced := false
		for _, r := range recs {
			if r.Fingerprint == rec.Fingerprint {
				out = append(out, rec)
				replaced = true
			} else {
				out = append(out, r)
			}
		}
		if !replaced {
			out = append(out, rec)
		}
		return out, nil
	})
}

// Delete removes the record for a fingerprint (used on revocation). Absent
// fingerprints are a no-op.
func (ks *keyStore) Delete(fingerprint string) error {
	return ks.withLock(func(recs []keyRecord) ([]keyRecord, error) {
		out := recs[:0]
		for _, r := range recs {
			if r.Fingerprint != fingerprint {
				out = append(out, r)
			}
		}
		return out, nil
	})
}

// withLock takes the exclusive flock, reads the records, applies fn, and writes
// the result back atomically.
func (ks *keyStore) withLock(fn func([]keyRecord) ([]keyRecord, error)) error {
	if err := os.MkdirAll(filepath.Dir(ks.path), 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(ks.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", ks.lockPath, err)
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)

	recs, err := readRecords(ks.path)
	if err != nil {
		return err
	}
	out, err := fn(recs)
	if err != nil {
		return err
	}
	return writeRecords(ks.path, out)
}

// readRecords parses the TSV file. A missing file is an empty store, not an
// error. Malformed lines are skipped rather than aborting the whole read.
func readRecords(path string) ([]keyRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var recs []keyRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // refresh tokens are long
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if r, ok := parseRecord(line); ok {
			recs = append(recs, r)
		}
	}
	return recs, sc.Err()
}

// writeRecords serializes records to a temp file and renames it over the target,
// so a crash mid-write never leaves a truncated store. Must be called under lock.
func writeRecords(path string, recs []keyRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".keys-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	w := bufio.NewWriter(tmp)
	fmt.Fprintln(w, "# fingerprint\tsub\tnick\tregistered_at\tlast_seen\trefresh_token")
	for _, r := range recs {
		fmt.Fprintln(w, formatRecord(r))
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

const recordFields = 6

func formatRecord(r keyRecord) string {
	return strings.Join([]string{
		r.Fingerprint,
		r.Sub,
		r.Nick,
		r.RegisteredAt.UTC().Format(time.RFC3339),
		r.LastSeen.UTC().Format(time.RFC3339),
		r.RefreshToken,
	}, "\t")
}

// parseRecord parses one TSV line. ok=false for lines that lack the required
// columns. Timestamps that fail to parse become the zero time rather than
// dropping the row, so a corrupt date never locks a user out.
func parseRecord(line string) (keyRecord, bool) {
	parts := strings.Split(line, "\t")
	if len(parts) < recordFields {
		return keyRecord{}, false
	}
	if parts[0] == "" {
		return keyRecord{}, false
	}
	reg, _ := time.Parse(time.RFC3339, parts[3])
	seen, _ := time.Parse(time.RFC3339, parts[4])
	return keyRecord{
		Fingerprint:  parts[0],
		Sub:          parts[1],
		Nick:         parts[2],
		RegisteredAt: reg,
		LastSeen:     seen,
		RefreshToken: parts[5],
	}, true
}
