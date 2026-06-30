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

// nickRecord is one row of the nick-ownership registry: a Brogue nickname bound
// to the OIDC subject that first claimed it.
type nickRecord struct {
	Nick      string    // nickname as first claimed (display/debug; matched case-insensitively)
	Sub       string    // OIDC subject that owns this nick
	ClaimedAt time.Time // first time this nick was claimed
}

// nickRegistry enforces that a Brogue nickname maps to exactly one OIDC
// identity. The nick is the per-user save-directory name, and thus the key to a
// player's saves, scoreboard entries and in-progress games — so letting two
// different OIDC accounts resolve to the same nick would let one player take
// over another's games. game_nickname is freely user-editable and stock
// OIDC providers have no uniqueness validator for custom attributes, so we enforce
// first-come-first-served ownership here, at the trust boundary, regardless of
// how the directory is configured.
//
// Comparison is case-insensitive for stable, predictable save-dir ownership
// (findUserPassword uses EqualFold), so "Alice" and "alice" are the same account
// and cannot be owned by two different subjects.
//
// Locking and atomic writes mirror keyStore: an exclusive flock on a sidecar
// lock file guards every read-modify-write, and the data file is replaced via
// temp-file rename so a crash mid-write never truncates the registry.
type nickRegistry struct {
	path     string
	lockPath string
}

func newNickRegistry(path string) *nickRegistry {
	return &nickRegistry{path: path, lockPath: path + ".lock"}
}

// errNickTaken is returned by Claim when a nick is owned by a different subject.
type errNickTaken struct{ nick string }

func (e errNickTaken) Error() string {
	return fmt.Sprintf("nickname %q is already registered to another account; "+
		"choose a different game_nickname in your account profile", e.nick)
}

// Claim binds nick to sub on a first-come-first-served basis, under an exclusive
// lock. It succeeds (recording the claim) when the nick is unowned, and succeeds
// without change when sub already owns it. It returns errNickTaken when the nick
// is owned by a different subject.
func (nr *nickRegistry) Claim(nick, sub string) error {
	if nick == "" || sub == "" {
		return fmt.Errorf("refusing to claim empty nick or subject")
	}
	return nr.withLock(func(recs []nickRecord) ([]nickRecord, error) {
		for _, r := range recs {
			if strings.EqualFold(r.Nick, nick) {
				if r.Sub == sub {
					return recs, nil // already ours: no change
				}
				return nil, errNickTaken{nick}
			}
		}
		return append(recs, nickRecord{Nick: nick, Sub: sub, ClaimedAt: time.Now()}), nil
	})
}

// withLock takes the exclusive flock, reads the records, applies fn, and writes
// the result back atomically. If fn returns an error nothing is written.
func (nr *nickRegistry) withLock(fn func([]nickRecord) ([]nickRecord, error)) error {
	if err := os.MkdirAll(filepath.Dir(nr.path), 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(nr.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", nr.lockPath, err)
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)

	recs, err := readNickRecords(nr.path)
	if err != nil {
		return err
	}
	out, err := fn(recs)
	if err != nil {
		return err
	}
	return writeNickRecords(nr.path, out)
}

// readNickRecords parses the TSV file. A missing file is an empty registry, not
// an error. Malformed lines are skipped rather than aborting the whole read.
func readNickRecords(path string) ([]nickRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var recs []nickRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if r, ok := parseNickRecord(line); ok {
			recs = append(recs, r)
		}
	}
	return recs, sc.Err()
}

// writeNickRecords serializes records to a temp file and renames it over the
// target. Must be called under lock. The registry maps nicks to subject UUIDs —
// no bearer secrets — but it lives alongside keys.tsv in the root-owned auth dir,
// so it is created 0600 to keep the convention uniform.
func writeNickRecords(path string, recs []nickRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".nicks-*.tmp")
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
	fmt.Fprintln(w, "# nick\tsub\tclaimed_at")
	for _, r := range recs {
		fmt.Fprintln(w, formatNickRecord(r))
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

const nickRecordFields = 3

func formatNickRecord(r nickRecord) string {
	return strings.Join([]string{
		r.Nick,
		r.Sub,
		r.ClaimedAt.UTC().Format(time.RFC3339),
	}, "\t")
}

// parseNickRecord parses one TSV line. ok=false for lines that lack the required
// columns or carry an empty nick/sub. A timestamp that fails to parse becomes the
// zero time rather than dropping the row.
func parseNickRecord(line string) (nickRecord, bool) {
	parts := strings.Split(line, "\t")
	if len(parts) < nickRecordFields {
		return nickRecord{}, false
	}
	if parts[0] == "" || parts[1] == "" {
		return nickRecord{}, false
	}
	claimed, _ := time.Parse(time.RFC3339, parts[2])
	return nickRecord{Nick: parts[0], Sub: parts[1], ClaimedAt: claimed}, true
}
