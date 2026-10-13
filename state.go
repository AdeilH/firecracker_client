package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const stateFileName = "state.json"

// Record is the broker's local bookkeeping for one VM it created.
type Record struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	CreatedAt string `json:"created_at"` // RFC3339, UTC
	TTL       string `json:"ttl,omitempty"`
	KeyPath   string `json:"key_path,omitempty"`
}

// State is the on-disk document in ~/.firecracker-broker/state.json.
type State struct {
	VMs []Record `json:"vms"`
}

func brokerDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".firecracker-broker"), nil
}

func statePath() (string, error) {
	dir, err := brokerDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, stateFileName), nil
}

// LoadState reads the state file. A missing file is an empty state, not an
// error. Safe against torn reads because SaveState renames atomically.
func LoadState() (State, error) {
	path, err := statePath()
	if err != nil {
		return State{}, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

// SaveState writes the state file atomically: temp file in the same
// directory, fsync, then rename over the target.
func SaveState(s State) error {
	dir, err := brokerDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, stateFileName))
}

// UpdateState runs fn under an exclusive lock with the freshly loaded state
// and persists whatever fn leaves in place. Concurrent invocations of
// read-modify-write sequences serialize on the lock file.
func UpdateState(fn func(*State) error) error {
	dir, err := brokerDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	unlock, err := lockDir(dir)
	if err != nil {
		return err
	}
	defer unlock()

	s, err := LoadState()
	if err != nil {
		return err
	}
	if err := fn(&s); err != nil {
		return err
	}
	return SaveState(s)
}

// ParseTime accepts RFC3339 plus a couple of tolerant layouts; ok is false
// when the value cannot be understood.
func ParseTime(s string) (t time.Time, ok bool) {
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// normalizeTime converts a server timestamp into RFC3339 UTC; when the
// server's value cannot be parsed it falls back to the current time so the
// record still gets a sane sweep anchor.
func normalizeTime(s string) string {
	if t, ok := ParseTime(s); ok {
		return t.UTC().Format(time.RFC3339)
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// effectiveTTL is the record's own ttl or, when unset, the sweep grace
// period.
func effectiveTTL(r Record, grace time.Duration) time.Duration {
	if r.TTL != "" {
		if d, err := time.ParseDuration(r.TTL); err == nil && d > 0 {
			return d
		}
	}
	return grace
}

// PlanSweep classifies tracked records against the live VM list:
// toDelete are expired records whose VM still exists on the host,
// toForget are records whose VM no longer exists (state reconciliation).
// Records with an unparseable created_at are never deleted.
func PlanSweep(recs []Record, liveIDs map[string]bool, now time.Time, grace time.Duration) (toDelete, toForget []Record) {
	for _, r := range recs {
		if !liveIDs[r.ID] {
			toForget = append(toForget, r)
			continue
		}
		created, ok := ParseTime(r.CreatedAt)
		if !ok {
			continue
		}
		if now.Sub(created) > effectiveTTL(r, grace) {
			toDelete = append(toDelete, r)
		}
	}
	return toDelete, toForget
}
