package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestLoadStateMissingFileIsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState on missing file: %v", err)
	}
	if len(st.VMs) != 0 {
		t.Errorf("got %+v, want empty state", st)
	}
}

func TestStateRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want := State{VMs: []Record{
		{ID: "vm-1", Name: "alice-a", Owner: "alice", CreatedAt: "2026-10-13T10:00:00Z", TTL: "2h", KeyPath: "/home/u/.firecracker-broker/keys/vm-1.key"},
		{ID: "vm-2", Name: "bob-b", Owner: "bob", CreatedAt: "2026-10-13T11:30:00Z"},
	}}
	if err := SaveState(want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}

	// UpdateState must load-modify-save without losing existing records.
	if err := UpdateState(func(s *State) error {
		s.VMs = append(s.VMs, Record{ID: "vm-3", Owner: "carol"})
		return nil
	}); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	got, err = LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(got.VMs) != 3 || got.VMs[2].ID != "vm-3" {
		t.Errorf("after update: %+v", got)
	}
}

func TestUpdateStateIsSafeForConcurrentInvocations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := UpdateState(func(s *State) error {
				s.VMs = append(s.VMs, Record{ID: fmt.Sprintf("vm-%d", i), Owner: "owner"})
				return nil
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent update: %v", err)
	}

	got, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(got.VMs) != n {
		t.Fatalf("got %d records, want %d (lost updates?)", len(got.VMs), n)
	}
	seen := map[string]bool{}
	for _, r := range got.VMs {
		if seen[r.ID] {
			t.Errorf("duplicate record %s", r.ID)
		}
		seen[r.ID] = true
	}
}

func TestSaveStateLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := SaveState(State{}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, ".firecracker-broker"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".firecracker-broker", "state.json")); err != nil {
		t.Errorf("state.json missing: %v", err)
	}
}

func TestLoadStateRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	broker := filepath.Join(dir, ".firecracker-broker")
	os.MkdirAll(broker, 0o755)
	os.WriteFile(filepath.Join(broker, "state.json"), []byte("{not json"), 0o600)
	if _, err := LoadState(); err == nil {
		t.Error("LoadState accepted a corrupt file")
	}
}

func TestPlanSweep(t *testing.T) {
	now := time.Now()
	old := now.Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	young := now.Add(-time.Minute).UTC().Format(time.RFC3339)
	recs := []Record{
		{ID: "young", CreatedAt: young, TTL: "2h"},
		{ID: "old-by-ttl", CreatedAt: old, TTL: "2h"},
		{ID: "old-by-grace", CreatedAt: old},
		{ID: "grace-default", CreatedAt: now.Add(-90 * time.Minute).UTC().Format(time.RFC3339)},
		{ID: "gone", CreatedAt: old, TTL: "9999h"},
		{ID: "bad-timestamp", CreatedAt: "not-a-time", TTL: "1ns"},
	}
	live := map[string]bool{"young": true, "old-by-ttl": true, "old-by-grace": true, "grace-default": true, "bad-timestamp": true}

	toDelete, toForget := PlanSweep(recs, live, now, 2*time.Hour)

	gotDelete := map[string]bool{}
	for _, r := range toDelete {
		gotDelete[r.ID] = true
	}
	if !gotDelete["old-by-ttl"] {
		t.Error("old-by-ttl should be deleted (created 3h ago, ttl 2h)")
	}
	if !gotDelete["old-by-grace"] {
		t.Error("old-by-grace should be deleted (no ttl, grace 2h)")
	}
	if gotDelete["young"] {
		t.Error("young must not be deleted")
	}
	if gotDelete["grace-default"] {
		t.Error("grace-default is 1h30m old, under the 2h grace, must not be deleted")
	}
	if gotDelete["bad-timestamp"] {
		t.Error("record with unparseable timestamp must not be deleted")
	}
	if len(toDelete) != 2 {
		t.Errorf("toDelete = %+v, want exactly 2", toDelete)
	}
	if len(toForget) != 1 || toForget[0].ID != "gone" {
		t.Errorf("toForget = %+v, want only \"gone\" (absent from live set)", toForget)
	}
}

func TestEffectiveTTL(t *testing.T) {
	r := Record{TTL: "30m"}
	if got := effectiveTTL(r, time.Hour); got != 30*time.Minute {
		t.Errorf("effectiveTTL = %v, want 30m", got)
	}
	if got := effectiveTTL(Record{}, time.Hour); got != time.Hour {
		t.Errorf("effectiveTTL without ttl = %v, want grace 1h", got)
	}
	if got := effectiveTTL(Record{TTL: "bogus"}, time.Hour); got != time.Hour {
		t.Errorf("effectiveTTL with bogus ttl = %v, want grace 1h", got)
	}
}

func TestParseTimeAndNormalize(t *testing.T) {
	if _, ok := ParseTime("2026-10-13T12:00:00+02:00"); !ok {
		t.Error("RFC3339 with offset should parse")
	}
	if _, ok := ParseTime("2026-10-13 12:00:00"); !ok {
		t.Error("naive layout should parse")
	}
	if _, ok := ParseTime("whenever"); ok {
		t.Error("garbage should not parse")
	}
	if got, want := normalizeTime("2026-10-13T12:00:00+02:00"), "2026-10-13T10:00:00Z"; got != want {
		t.Errorf("normalizeTime = %q, want %q", got, want)
	}
	got := normalizeTime("garbage")
	parsed, ok := ParseTime(got)
	if !ok || time.Since(parsed) > time.Minute {
		t.Errorf("normalizeTime fallback = %q, want a recent parseable time", got)
	}
}

func TestStateFileIsValidJSONWithExpectedFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := SaveState(State{VMs: []Record{{ID: "vm-1", Owner: "alice", TTL: "2h"}}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".firecracker-broker", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("state.json is not valid JSON: %v", err)
	}
	vms, _ := doc["vms"].([]any)
	if len(vms) != 1 {
		t.Fatalf("state.json vms = %v", doc["vms"])
	}
	rec := vms[0].(map[string]any)
	for _, key := range []string{"id", "owner", "created_at", "ttl"} {
		if _, ok := rec[key]; !ok {
			t.Errorf("record missing field %q: %v", key, rec)
		}
	}
}
