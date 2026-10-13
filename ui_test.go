package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newFakeBroker serves a minimal host API: a fixed VM list plus a create
// endpoint that records the request and echoes a plausible VM back.
func newFakeBroker(t *testing.T) (*httptest.Server, *[]CreateVMRequest) {
	t.Helper()
	var creates []CreateVMRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms":
			var req CreateVMRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			creates = append(creates, req)
			writeTestJSON(w, map[string]any{
				"id": "vm-fake1", "name": req.Name, "state": "running",
				"ip": "172.16.100.9", "ssh_port": 2200, "ssh_user": "root",
				"created_at": "2026-10-13T12:00:00Z",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms":
			writeTestJSON(w, map[string]any{"vms": []map[string]any{
				{"id": "vm-fake1", "name": "ui-build-test", "state": "running", "ip": "172.16.100.9", "ssh_port": 2200,
					"ssh_user": "root", "ssh_command": "ssh -p 2200 root@192.0.2.10"},
				{"id": "vm-other", "name": "codex-sub1-task", "state": "running", "ip": "172.16.100.10", "ssh_port": 2201,
					"ssh_user": "root", "ssh_command": "ssh -p 2201 root@192.0.2.10"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/capacity":
			writeTestJSON(w, map[string]any{"max_vms": 20, "existing_vms": 2, "running_vms": 2})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/vms/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &creates
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newTestUIServer(t *testing.T, brokerURL string) *uiServer {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(tokenEnvVar, "test-token")
	return &uiServer{client: NewClient(brokerURL, "test-token"), model: "test-model"}
}

func TestUIMayModifyGuard(t *testing.T) {
	broker, _ := newFakeBroker(t)
	s := newTestUIServer(t, broker.URL)
	if err := UpdateState(func(st *State) error {
		st.VMs = []Record{
			{ID: "vm-mine", Name: "ui-build-test", Owner: "ui-build"},
			{ID: "vm-theirs", Name: "codex-sub1-task", Owner: "codex-sub1"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.mayModify("vm-mine"); err != nil {
		t.Errorf("ui- owned VM must be modifiable, got %v", err)
	}
	if err := s.mayModify("vm-theirs"); err == nil {
		t.Error("foreign-owned VM must be refused")
	}
	if err := s.mayModify("vm-fake1"); err != nil {
		t.Errorf("untracked VM with ui- name must be modifiable, got %v", err)
	}
	if err := s.mayModify("vm-other"); err == nil {
		t.Error("untracked foreign VM must be refused")
	}
	if err := s.mayModify("vm-nope"); err == nil {
		t.Error("unknown VM must be refused")
	}

	s.allowAll = true
	if err := s.mayModify("vm-theirs"); err != nil {
		t.Errorf("--allow-all-owners must lift the guard, got %v", err)
	}
}

func TestUICreateVMNamespacesAndDefaults(t *testing.T) {
	broker, creates := newFakeBroker(t)
	s := newTestUIServer(t, broker.URL)

	vm, err := s.opCreateVM(createParams{Owner: "build", Purpose: "hello world", TTL: ""})
	if err != nil {
		t.Fatal(err)
	}
	if vm["owner"] != "ui-build" {
		t.Errorf("owner must be prefixed, got %v", vm["owner"])
	}
	if got := (*creates)[0].Name; got != "ui-build-hello-world" {
		t.Errorf("name must be owner-purpose with sanitized parts, got %q", got)
	}
	st, _ := LoadState()
	if len(st.VMs) != 1 || st.VMs[0].Owner != "ui-build" || st.VMs[0].TTL != "30m" {
		t.Errorf("record must track ui- owner and default ttl, got %+v", st.VMs)
	}

	if _, err := s.opCreateVM(createParams{Owner: "ui-x", TTL: "nope"}); err == nil {
		t.Error("invalid ttl must be rejected")
	}
}

func TestUIDeleteVMRefusesForeignOwner(t *testing.T) {
	broker, _ := newFakeBroker(t)
	s := newTestUIServer(t, broker.URL)
	if err := UpdateState(func(st *State) error {
		st.VMs = []Record{{ID: "vm-theirs", Name: "codex-sub1-task", Owner: "codex-sub1"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.opDeleteVM("vm-theirs"); err == nil {
		t.Fatal("delete of a foreign-owned VM must be refused")
	}
	st, _ := LoadState()
	if len(st.VMs) != 1 {
		t.Error("record must survive a refused delete")
	}
}

func TestUIStatusDegradesWhenBrokerDown(t *testing.T) {
	dead := httptest.NewServer(http.NewServeMux())
	url := dead.URL
	dead.Close() // port now refuses connections

	s := newTestUIServer(t, url)
	if err := UpdateState(func(st *State) error {
		st.VMs = []Record{{ID: "vm-mine", Name: "ui-build-test", Owner: "ui-build"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status must stay 200 in degraded mode, got %d", rec.Code)
	}
	var out struct {
		BrokerError string   `json:"broker_error"`
		VMs         []vmView `json:"vms"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.BrokerError == "" {
		t.Error("degraded status must report broker_error")
	}
	if len(out.VMs) != 1 || out.VMs[0].Owner != "ui-build" || out.VMs[0].State != "unknown" {
		t.Errorf("degraded status must fall back to local records, got %+v", out.VMs)
	}
}

// newFakeOpenAI answers the first completion with a list_vms tool call and
// the second with a final reply. Note the object-form arguments: several
// OpenAI-compatible servers send them that way instead of as a JSON string.
func newFakeOpenAI(t *testing.T) *httptest.Server {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_vms","arguments":{}}}]}}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"2 VMs, both running."}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUIChatAgentLoop(t *testing.T) {
	broker, _ := newFakeBroker(t)
	s := newTestUIServer(t, broker.URL)
	s.openaiKey = "test-key"
	openai := newFakeOpenAI(t)
	t.Setenv("OPENAI_BASE_URL", openai.URL)

	body := `{"messages":[{"role":"user","content":"what is running?"}]}`
	rec := httptest.NewRecorder()
	s.handleChat(rec, httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("chat failed: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Reply   string        `json:"reply"`
		Actions []agentAction `json:"actions"`
		Model   string        `json:"model"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Reply != "2 VMs, both running." {
		t.Errorf("unexpected reply: %q", out.Reply)
	}
	if len(out.Actions) != 1 || !out.Actions[0].OK || out.Actions[0].Name != "list_vms" {
		t.Errorf("expected one successful list_vms action, got %+v", out.Actions)
	}
	if out.Model != "test-model" {
		t.Errorf("model echoed wrong: %q", out.Model)
	}
}

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	t.Setenv("PRESET", "real-env")
	os.WriteFile(path, []byte("# comment\n\nPRESET=from-file\nNEWVAR=from-file\nQUOTED=\"quoted value\"\nSPACED =  spaced  \nEMPTY=\nMALFORMED\n"), 0o600)
	t.Cleanup(func() { os.Unsetenv("NEWVAR"); os.Unsetenv("QUOTED"); os.Unsetenv("SPACED") })

	var buf strings.Builder
	loadDotEnv(path, &buf)

	if got := os.Getenv("PRESET"); got != "real-env" {
		t.Errorf("real environment must win over .env, got %q", got)
	}
	if got := os.Getenv("NEWVAR"); got != "from-file" {
		t.Errorf("new variable must be applied, got %q", got)
	}
	if got := os.Getenv("QUOTED"); got != "quoted value" {
		t.Errorf("quotes must be stripped, got %q", got)
	}
	if got := os.Getenv("SPACED"); got != "spaced" {
		t.Errorf("whitespace must be trimmed, got %q", got)
	}
	if got := os.Getenv("EMPTY"); got != "" {
		t.Errorf("empty value must not be set, got %q", got)
	}
	if !strings.Contains(buf.String(), "MALFORMED") {
		t.Errorf("malformed line must be reported, got %q", buf.String())
	}

	buf.Reset()
	loadDotEnv(filepath.Join(dir, "missing.env"), &buf)
	if buf.String() != "" {
		t.Errorf("missing file must be silent, got %q", buf.String())
	}
}

// writeFakeSSH installs a stand-in ssh binary and returns its path.
func writeFakeSSH(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ssh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHostFromSSHCommand(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{"ssh -p 2200 root@192.168.100.7", "192.168.100.7"},
		{"ssh -i /k -o x root@host.example:2200 ", "host.example:2200"},
		{"", "fallback"},
		{"no-at-sign-here", "fallback"},
	}
	for _, c := range cases {
		if got := hostFromSSHCommand(c.cmd, "fallback"); got != c.want {
			t.Errorf("hostFromSSHCommand(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestUIListMergedSSHInfo(t *testing.T) {
	broker, _ := newFakeBroker(t)
	s := newTestUIServer(t, broker.URL)
	if err := UpdateState(func(st *State) error {
		st.VMs = []Record{{ID: "vm-fake1", Name: "ui-build-test", Owner: "ui-build",
			KeyPath: filepath.Join(t.TempDir(), "vm-fake1.key")}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	vms, err := s.opListMerged()
	if err != nil {
		t.Fatal(err)
	}
	var v *vmView
	for i := range vms {
		if vms[i].ID == "vm-fake1" {
			v = &vms[i]
		}
	}
	if v == nil {
		t.Fatal("vm-fake1 missing from merged view")
	}
	if v.SSHHost != "192.0.2.10" || v.SSHUser != "root" || v.SSHPort != 2200 {
		t.Errorf("ssh endpoint wrong: %+v", v)
	}
	if v.SSHKey == "" || !strings.HasSuffix(v.SSHKey, "vm-fake1.key") {
		t.Errorf("key path missing from view: %+v", v)
	}

	if got := displayPath(filepath.Join("srv", "keys", "x.key")); got != "srv/keys/x.key" {
		t.Errorf("paths outside home must stay absolute, got %q", got)
	}
	if got := hostnameOf("http://192.168.100.7:8420"); got != "192.168.100.7" {
		t.Errorf("hostnameOf wrong: %q", got)
	}
}

func TestUIRunInVM(t *testing.T) {
	broker, _ := newFakeBroker(t)
	s := newTestUIServer(t, broker.URL)

	// A tracked ui- VM with a key on disk.
	key := filepath.Join(t.TempDir(), "vm-fake1.key")
	os.WriteFile(key, []byte("dummy key material"), 0o600)
	if err := UpdateState(func(st *State) error {
		st.VMs = []Record{{ID: "vm-fake1", Name: "ui-build-test", Owner: "ui-build", KeyPath: key}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	recPath := filepath.Join(t.TempDir(), "argv")
	s.sshBin = writeFakeSSH(t, fmt.Sprintf("printf '%%s\\n' \"$@\" > %q\necho 'hello from vm'\n", recPath))
	res, err := s.opRunInVM("vm-fake1", "uname -a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "hello from vm") {
		t.Errorf("unexpected result: %+v", res)
	}
	argv, _ := os.ReadFile(recPath)
	tokens := strings.Fields(string(argv))
	for _, want := range []string{"-p", "2200", "-i", key, "root@192.0.2.10", "--", "uname", "-a"} {
		if !containsToken(tokens, want) {
			t.Errorf("ssh argv missing %q: %q", want, string(argv))
		}
	}

	// Foreign owner: refused before ssh even runs.
	s.sshBin = writeFakeSSH(t, "echo SHOULD-NOT-RUN\n")
	if _, err := s.opRunInVM("vm-other", "uname -a", 0); err == nil {
		t.Error("exec inside a foreign-owned VM must be refused")
	}
}

func containsToken(tokens []string, want string) bool {
	for _, tk := range tokens {
		if tk == want {
			return true
		}
	}
	return false
}

func TestUIRunInVMTimeout(t *testing.T) {
	broker, _ := newFakeBroker(t)
	s := newTestUIServer(t, broker.URL)
	key := filepath.Join(t.TempDir(), "vm-fake1.key")
	os.WriteFile(key, []byte("dummy key material"), 0o600)
	if err := UpdateState(func(st *State) error {
		st.VMs = []Record{{ID: "vm-fake1", Name: "ui-build-test", Owner: "ui-build", KeyPath: key}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	s.sshBin = writeFakeSSH(t, "sleep 5\necho done\n")
	res, err := s.opRunInVM("vm-fake1", "sleep 5", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Errorf("expected a timeout, got %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("command should be killed at the timeout, took %s", elapsed)
	}
}

func TestUIChatWithoutKey(t *testing.T) {
	broker, _ := newFakeBroker(t)
	s := newTestUIServer(t, broker.URL)

	rec := httptest.NewRecorder()
	s.handleChat(rec, httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"messages":[]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("chat without OPENAI_API_KEY must be 503, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "OPENAI_API_KEY") {
		t.Errorf("error must name the missing env var: %s", rec.Body.String())
	}
}

// TestUIChatLocalCompatNoKey covers a local OpenAI-compatible server: a
// custom OPENAI_BASE_URL enables the agent without any API key, and no
// Authorization header is sent.
func TestUIChatLocalCompatNoKey(t *testing.T) {
	broker, _ := newFakeBroker(t)
	s := newTestUIServer(t, broker.URL)
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"all quiet"}}]}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_BASE_URL", srv.URL)

	rec := httptest.NewRecorder()
	s.handleChat(rec, httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"status?"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("local compat server must work without a key: %d %s", rec.Code, rec.Body.String())
	}
	if gotAuth != "" {
		t.Errorf("no Authorization header expected without a key, got %q", gotAuth)
	}
	var out struct {
		Reply string `json:"reply"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Reply != "all quiet" {
		t.Errorf("unexpected reply: %q", out.Reply)
	}
}
