package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testEnv points HOME at a fresh temp dir and sets a token, so tests never
// touch the real ~/.firecracker-broker.
func testEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv(tokenEnvVar, "test-token")
	return dir
}

func TestUnknownCommand(t *testing.T) {
	var out, errBuf strings.Builder
	code := run([]string{"bogus"}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), `unknown command "bogus"`) {
		t.Errorf("stderr = %q, want unknown-command notice", errBuf.String())
	}
}

func TestHelpShowsUsage(t *testing.T) {
	var out, errBuf strings.Builder
	code := run([]string{"help"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"create", "list", "stop", "start", "delete", "sweep", "capacity"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage missing command %q", want)
		}
	}
	if errBuf.Len() != 0 {
		t.Errorf("help wrote to stderr: %q", errBuf.String())
	}
}

func TestCreateRequiresOwner(t *testing.T) {
	testEnv(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()

	var out, errBuf strings.Builder
	code := run([]string{"create", "--url", srv.URL}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "--owner is required") {
		t.Errorf("stderr = %q, want --owner error", errBuf.String())
	}
	if calls.Load() != 0 {
		t.Errorf("server called %d times, want 0", calls.Load())
	}
}

func TestCreateInvalidTTL(t *testing.T) {
	testEnv(t)
	var out, errBuf strings.Builder
	code := run([]string{"create", "--owner", "alice", "--ttl", "nope"}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), `invalid --ttl "nope"`) {
		t.Errorf("stderr = %q, want invalid --ttl error", errBuf.String())
	}
}

func TestCreateMissingTokenExactMessage(t *testing.T) {
	testEnv(t)
	t.Setenv(tokenEnvVar, "")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()

	var out, errBuf strings.Builder
	code := run([]string{"create", "--owner", "alice", "--url", srv.URL}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if got, want := errBuf.String(), authNotice+"\n"; got != want {
		t.Errorf("stderr = %q, want exactly %q", got, want)
	}
	if calls.Load() != 0 {
		t.Errorf("server called %d times, want 0", calls.Load())
	}
}

func TestCreateGeneratedKeyWritesFileAndHidesMaterial(t *testing.T) {
	home := testEnv(t)
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vms" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		vm := VM{
			ID: "vm-1", Name: "alice-x", State: "running", IP: "192.168.0.50",
			SSHPort: 2222, SSHUser: "root", SSHCommand: "ssh -p 2222 root@192.168.0.50",
			SSHPrivateKey: "TOPSECRET-KEY-MATERIAL",
			VCPUs:         1, MemMiB: 256, CreatedAt: "2026-10-13T10:00:00Z",
		}
		json.NewEncoder(w).Encode(vm)
	}))
	defer srv.Close()

	var out, errBuf strings.Builder
	code := run([]string{"create", "--owner", "alice", "--url", srv.URL}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errBuf.String())
	}
	if calls := gotBody; calls == nil {
		t.Fatal("server never received a body")
	}
	if name, _ := gotBody["name"].(string); !strings.HasPrefix(name, "alice-") {
		t.Errorf("name = %q, want prefix \"alice-\"", name)
	}
	if _, ok := gotBody["ssh_key"]; ok {
		t.Error("request sent ssh_key although none was provided")
	}

	stdoutStr := out.String()
	if !strings.Contains(stdoutStr, `"ssh_command"`) {
		t.Error("stdout missing ssh_command")
	}
	if !strings.Contains(stdoutStr, `"ssh_key_path"`) {
		t.Error("stdout missing ssh_key_path")
	}
	if strings.Contains(stdoutStr, "TOPSECRET") {
		t.Error("stdout leaks generated key material")
	}
	if strings.Contains(errBuf.String(), "TOPSECRET") {
		t.Error("stderr leaks generated key material")
	}

	keyPath := filepath.Join(home, ".firecracker-broker", "keys", "vm-1.key")
	b, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	if string(b) != "TOPSECRET-KEY-MATERIAL" {
		t.Errorf("key file content = %q", string(b))
	}
	if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("key file perms = %v, want 0600 (stat err: %v)", info.Mode().Perm(), err)
	}

	st, err := LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(st.VMs) != 1 {
		t.Fatalf("state has %d records, want 1", len(st.VMs))
	}
	rec := st.VMs[0]
	if rec.ID != "vm-1" || rec.Owner != "alice" || rec.KeyPath != keyPath {
		t.Errorf("record = %+v", rec)
	}
}

func TestCreateWithPurposeAndKeyFile(t *testing.T) {
	home := testEnv(t)
	pubFile := filepath.Join(t.TempDir(), "id.pub")
	pub := "ssh-ed25519 AAAATEST user@host"
	if err := os.WriteFile(pubFile, []byte(pub+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(VM{ID: "vm-2", Name: "bob-web", State: "running", CreatedAt: "2026-10-13T10:00:00Z"})
	}))
	defer srv.Close()

	var out, errBuf strings.Builder
	code := run([]string{"create", "--owner", "bob", "--purpose", "web", "--ssh-key", pubFile, "--ttl", "90m", "--url", srv.URL}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errBuf.String())
	}
	if got, want := gotBody["ssh_key"], pub; got != want {
		t.Errorf("ssh_key = %v, want file contents %q", got, want)
	}
	if got, want := gotBody["name"], "bob-web"; got != want {
		t.Errorf("name = %v, want %q", got, want)
	}
	if strings.Contains(out.String(), "ssh_key_path") {
		t.Error("stdout mentions ssh_key_path although the caller supplied the key")
	}
	if _, err := os.Stat(filepath.Join(home, ".firecracker-broker", "keys")); !os.IsNotExist(err) {
		t.Errorf("keys dir should not exist, stat err = %v", err)
	}
	st, _ := LoadState()
	if len(st.VMs) != 1 || st.VMs[0].TTL != "90m" {
		t.Errorf("records = %+v, want one with ttl 90m", st.VMs)
	}
}

func TestCreateSurfaces409(t *testing.T) {
	testEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":"vm limit reached"}`)
	}))
	defer srv.Close()

	var out, errBuf strings.Builder
	code := run([]string{"create", "--owner", "alice", "--url", srv.URL}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if got, want := errBuf.String(), "host API error 409: vm limit reached\n"; !strings.Contains(got, want) {
		t.Errorf("stderr = %q, want it to contain %q", got, want)
	}
	st, _ := LoadState()
	if len(st.VMs) != 0 {
		t.Errorf("failed create left %d state records, want 0", len(st.VMs))
	}
}

func seedRecords(t *testing.T, recs ...Record) {
	t.Helper()
	if err := UpdateState(func(s *State) error {
		s.VMs = append(s.VMs, recs...)
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

func TestListTableAndOwnerFilter(t *testing.T) {
	testEnv(t)
	now := time.Now().UTC()
	seedRecords(t,
		Record{ID: "vm-a", Name: "alice-a", Owner: "alice", CreatedAt: now.Add(-time.Hour).Format(time.RFC3339), TTL: "2h"},
		Record{ID: "vm-b", Name: "bob-b", Owner: "bob", CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)},
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"vms":[`+
			`{"id":"vm-a","name":"alice-a","state":"running","ip":"10.0.0.2","ssh_port":2222,"created_at":"2026-10-13T09:00:00Z"},`+
			`{"id":"vm-b","name":"bob-b","state":"stopped","ip":"10.0.0.3","ssh_port":2223,"created_at":"2026-10-13T08:00:00Z"}]}`)
	}))
	defer srv.Close()

	var out, errBuf strings.Builder
	code := run([]string{"list", "--url", srv.URL}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errBuf.String())
	}
	for _, want := range []string{"ID", "NAME", "STATE", "OWNER", "AGE", "IP", "SSH_PORT", "vm-a", "alice", "vm-b", "bob", "running", "stopped", "10.0.0.2", "2222"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("list output missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	code = run([]string{"list", "--owner", "alice", "--url", srv.URL}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "vm-a") {
		t.Error("filtered list missing vm-a")
	}
	if strings.Contains(out.String(), "vm-b") {
		t.Error("filtered list leaked vm-b")
	}
}

func TestList401ExactMessage(t *testing.T) {
	testEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	var out, errBuf strings.Builder
	code := run([]string{"list", "--url", srv.URL}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if got, want := errBuf.String(), authNotice+"\n"; got != want {
		t.Errorf("stderr = %q, want exactly %q", got, want)
	}
}

func TestStopStartRequireID(t *testing.T) {
	testEnv(t)
	var out, errBuf strings.Builder
	code := run([]string{"stop"}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("stop exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "--id is required") {
		t.Errorf("stop stderr = %q", errBuf.String())
	}
	errBuf.Reset()
	code = run([]string{"start"}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("start exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "--id is required") {
		t.Errorf("start stderr = %q", errBuf.String())
	}
}

func TestPowerProxiesAndDeleteCleansUp(t *testing.T) {
	home := testEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing bearer token, got %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/stop":
			json.NewEncoder(w).Encode(VM{ID: "vm-1", State: "stopped"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/start":
			json.NewEncoder(w).Encode(VM{ID: "vm-1", State: "running"})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/vms/vm-1":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	var out, errBuf strings.Builder
	if code := run([]string{"stop", "--id", "vm-1", "--url", srv.URL}, &out, &errBuf); code != 0 {
		t.Fatalf("stop exit = %d, stderr = %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `"stopped"`) {
		t.Errorf("stop output = %q", out.String())
	}

	out.Reset()
	if code := run([]string{"start", "--id", "vm-1", "--url", srv.URL}, &out, &errBuf); code != 0 {
		t.Fatalf("start exit = %d, stderr = %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `"running"`) {
		t.Errorf("start output = %q", out.String())
	}

	keyPath, err := writePrivateKey("vm-1", "SECRET")
	if err != nil {
		t.Fatal(err)
	}
	if keyPath != filepath.Join(home, ".firecracker-broker", "keys", "vm-1.key") {
		t.Fatalf("key path = %q", keyPath)
	}
	seedRecords(t, Record{ID: "vm-1", Name: "alice-x", Owner: "alice", CreatedAt: time.Now().UTC().Format(time.RFC3339), KeyPath: keyPath})

	out.Reset()
	if code := run([]string{"delete", "--id", "vm-1", "--url", srv.URL}, &out, &errBuf); code != 0 {
		t.Fatalf("delete exit = %d, stderr = %s", code, errBuf.String())
	}
	if got, want := out.String(), "deleted vm-1\n"; got != want {
		t.Errorf("delete output = %q, want %q", got, want)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Errorf("key file still exists after delete, stat err = %v", err)
	}
	st, _ := LoadState()
	if len(st.VMs) != 0 {
		t.Errorf("state records after delete = %+v, want empty", st.VMs)
	}
}

func TestDelete404StillCleansLocalState(t *testing.T) {
	testEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"no such vm"}`)
	}))
	defer srv.Close()

	seedRecords(t, Record{ID: "vm-gone", Name: "n", Owner: "alice", CreatedAt: time.Now().UTC().Format(time.RFC3339)})

	var out, errBuf strings.Builder
	code := run([]string{"delete", "--id", "vm-gone", "--url", srv.URL}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errBuf.String())
	}
	st, _ := LoadState()
	if len(st.VMs) != 0 {
		t.Errorf("state records = %+v, want empty", st.VMs)
	}
}

func TestSweepDeletesExpired(t *testing.T) {
	testEnv(t)
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms":
			fmt.Fprint(w, `{"vms":[{"id":"vm-old","name":"alice-old","state":"running"}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/vms/vm-old":
			if r.Header.Get("Authorization") != "Bearer test-token" {
				t.Errorf("delete sent no bearer token")
			}
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	keyPath, err := writePrivateKey("vm-old", "SECRET")
	if err != nil {
		t.Fatal(err)
	}
	seedRecords(t, Record{
		ID: "vm-old", Name: "alice-old", Owner: "alice",
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
		TTL:       "30m", KeyPath: keyPath,
	})

	var out, errBuf strings.Builder
	code := run([]string{"sweep", "--url", srv.URL}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errBuf.String())
	}
	if deletes.Load() != 1 {
		t.Fatalf("DELETE called %d times, want 1", deletes.Load())
	}
	if !strings.Contains(out.String(), "deleted vm-old") {
		t.Errorf("output = %q", out.String())
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Errorf("key file survived sweep, stat err = %v", err)
	}
	st, _ := LoadState()
	if len(st.VMs) != 0 {
		t.Errorf("state after sweep = %+v, want empty", st.VMs)
	}
}

func TestSweepForgetsMissingWithoutCallingDelete(t *testing.T) {
	testEnv(t)
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms":
			fmt.Fprint(w, `{"vms":[]}`)
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	seedRecords(t, Record{ID: "vm-gone", Name: "n", Owner: "alice", CreatedAt: time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)})

	var out, errBuf strings.Builder
	code := run([]string{"sweep", "--url", srv.URL}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errBuf.String())
	}
	if deletes.Load() != 0 {
		t.Errorf("DELETE called %d times for a VM that no longer exists, want 0", deletes.Load())
	}
	if !strings.Contains(out.String(), "forgot vm-gone") {
		t.Errorf("output = %q", out.String())
	}
	st, _ := LoadState()
	if len(st.VMs) != 0 {
		t.Errorf("state after sweep = %+v, want empty", st.VMs)
	}
}

func TestSweepKeepsYoungVMs(t *testing.T) {
	testEnv(t)
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms":
			fmt.Fprint(w, `{"vms":[{"id":"vm-young","name":"alice-y","state":"running"}]}`)
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	seedRecords(t, Record{ID: "vm-young", Name: "alice-y", Owner: "alice", CreatedAt: time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339), TTL: "2h"})

	var out, errBuf strings.Builder
	code := run([]string{"sweep", "--url", srv.URL}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errBuf.String())
	}
	if deletes.Load() != 0 {
		t.Errorf("DELETE called %d times for a young VM, want 0", deletes.Load())
	}
	st, _ := LoadState()
	if len(st.VMs) != 1 {
		t.Errorf("young record was dropped: %+v", st.VMs)
	}
}

func TestSweepInvalidGrace(t *testing.T) {
	testEnv(t)
	var out, errBuf strings.Builder
	code := run([]string{"sweep", "--grace", "tomorrow"}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), `invalid --grace "tomorrow"`) {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

func TestBaseURLFromEnv(t *testing.T) {
	t.Setenv(urlEnvVar, "")
	if got := baseURLFromEnv(); got != defaultBaseURL {
		t.Errorf("baseURLFromEnv() = %q, want default %q", got, defaultBaseURL)
	}
	t.Setenv(urlEnvVar, "http://example:1234")
	if got := baseURLFromEnv(); got != "http://example:1234" {
		t.Errorf("baseURLFromEnv() = %q, want env override", got)
	}
}

func TestHumanAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{5 * time.Minute, "5m"},
		{45 * time.Minute, "45m"},
		{3 * time.Hour, "3h"},
		{3*time.Hour + 27*time.Minute, "3h27m"},
		{25 * time.Hour, "1d1h"},
		{48 * time.Hour, "2d"},
		{-time.Minute, "0s"},
	}
	for _, c := range cases {
		if got := humanAge(c.d); got != c.want {
			t.Errorf("humanAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
