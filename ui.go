package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed ui_static
var uiStatic embed.FS

// uiOwnerPrefix namespaces everything the console (and its agent) creates.
// Mutating operations are refused for VMs outside this prefix unless
// --allow-all-owners is set: other owners' VMs are visible read-only.
const uiOwnerPrefix = "ui-"

const agentSystemPrompt = `You are the operator agent inside a firecracker-broker web console. You manage Firecracker microVMs on behalf of the user through function calls.

Rules:
- Every VM needs a ttl (default "30m"; use "1h" or "2h" for longer work).
- Owners are namespaced: anything you create is auto-prefixed "ui-". Use one owner per task, e.g. "ui-build" or "ui-test".
- You may only modify VMs whose owner starts with "ui-"; other owners' VMs are visible read-only. Never try to work around this.
- Check capacity before creating (host cap: 20 VMs, 1-4 vCPUs, 256-2048 MiB).
- When the user is done with a VM, delete it. Deletes are idempotent.
- stop keeps the disk and the VM can be restarted with start; delete wipes it forever.
- run_in_vm executes a shell command inside a running VM (as root, over SSH). Use it to do real work: inspect, install packages, build, test. Commands must be non-interactive; nothing survives delete.

Answer concisely. When you act, say what you did and include the VM id.`

// uiServer carries the shared dependencies of every console endpoint.
type uiServer struct {
	client    *Client
	allowAll  bool
	openaiKey string
	model     string
	sshBin    string // ssh executable used to reach VM shells; overridable in tests
}

const defaultOpenAIBase = "https://api.openai.com/v1"

// openAIBase is the chat-completions base URL: any OpenAI-compatible server
// works (Ollama, LM Studio, vLLM, OpenRouter, ...).
func openAIBase() string {
	return strings.TrimRight(envOr("OPENAI_BASE_URL", defaultOpenAIBase), "/")
}

// agentReady needs a key only for the hosted OpenAI API; a custom base URL
// (typically a local OpenAI-compatible server) works without one.
func (s *uiServer) agentReady() bool {
	return s.openaiKey != "" || openAIBase() != defaultOpenAIBase
}

// loadDotEnv applies KEY=VALUE lines from a .env file in the working
// directory. Variables already set in the real environment win; a missing
// file is not an error. Comments (#) and blank lines are skipped, optional
// single or double quotes around the value are stripped.
func loadDotEnv(path string, stderr io.Writer) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "warning: could not read %s: %v\n", path, err)
		}
		return
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			fmt.Fprintf(stderr, "warning: %s:%d: not KEY=VALUE, skipped: %q\n", path, i+1, line)
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key == "" || value == "" || os.Getenv(key) != "" {
			continue
		}
		os.Setenv(key, value)
	}
}

func cmdUI(args []string, stdout, stderr io.Writer) int {
	loadDotEnv(".env", stderr)
	flags := newFlagSet("ui", stderr)
	addr := flags.String("addr", "127.0.0.1:8090", "address to serve the console on")
	allowAll := flags.Bool("allow-all-owners", false, "let the console modify VMs owned by any owner, not just ui-*")
	urlFlag := flags.String("url", baseURLFromEnv(), "host API base URL")
	if proceed, code := parseFlags(flags, args); !proceed {
		return code
	}
	tok, ok := requireToken()
	if !ok {
		fmt.Fprintln(stderr, authNotice)
		return 1
	}

	s := &uiServer{
		client:    NewClient(*urlFlag, tok),
		allowAll:  *allowAll,
		openaiKey: strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		model:     envOr("OPENAI_MODEL", "gpt-4o-mini"),
		sshBin:    "ssh",
	}

	mux := http.NewServeMux()
	static, err := fs.Sub(uiStatic, "ui_static")
	if err != nil {
		return report(err, stderr)
	}
	mux.Handle("/", noCache(http.FileServer(http.FS(static))))
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/vms", s.handleVMs)
	mux.HandleFunc("/api/vms/", s.handleVMItem)
	mux.HandleFunc("/api/sweep", s.handleSweep)
	mux.HandleFunc("/api/chat", s.handleChat)

	fmt.Fprintf(stdout, "microVM console on http://%s (broker %s, model %s)\n", *addr, s.client.BaseURL, s.displayName())
	log.SetFlags(0)
	if err := http.ListenAndServe(*addr, logRequests(mux)); err != nil {
		return report(err, stderr)
	}
	return 0
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func (s *uiServer) displayName() string {
	if !s.agentReady() {
		return "disabled (set OPENAI_API_KEY or OPENAI_BASE_URL)"
	}
	return s.model
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
	})
}

// noCache keeps the console assets from being served stale: they are
// embedded in the binary, so after a rebuild the browser must refetch them.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return false
	}
	return true
}

// --- operations shared by the REST handlers and the agent's tools ---

// vmView is one row of the console: local bookkeeping (owner, ttl, age)
// merged with the live state from the host API.
type vmView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Owner   string `json:"owner"`
	State   string `json:"state"`
	IP      string `json:"ip"`
	SSHPort int    `json:"ssh_port"`
	VCPUs   int    `json:"vcpus"`
	MemMiB  int    `json:"mem_mib"`
	Age     string `json:"age"`
	TTL     string `json:"ttl,omitempty"`
	Tracked bool   `json:"tracked"`

	// SSH access details for the copy-paste command; key is the display
	// path (~-abbreviated) of the generated private key, empty when the
	// key lives on another machine.
	SSHUser string `json:"ssh_user,omitempty"`
	SSHHost string `json:"ssh_host,omitempty"`
	SSHKey  string `json:"ssh_key,omitempty"`
}

// opListMerged merges tracked records with live VMs. Tracked-but-missing VMs
// show up as state "missing"; live-but-untracked ones (created by other
// machines or agents) appear with an empty owner.
func (s *uiServer) opListMerged() ([]vmView, error) {
	live, err := s.client.ListVMs()
	if err != nil {
		return nil, err
	}
	st, err := LoadState()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]VM, len(live))
	for _, vm := range live {
		byID[vm.ID] = vm
	}
	recByID := make(map[string]Record, len(st.VMs))
	for _, rec := range st.VMs {
		recByID[rec.ID] = rec
	}

	now := time.Now()
	fallbackHost := hostnameOf(s.client.BaseURL)
	var views []vmView
	for _, vm := range live {
		v := vmView{
			ID: vm.ID, Name: vm.Name, State: stateOr(vm.State, "unknown"),
			IP: vm.IP, SSHPort: vm.SSHPort, VCPUs: vm.VCPUs, MemMiB: vm.MemMiB,
		}
		v.SSHUser = vm.SSHUser
		if v.SSHUser == "" {
			v.SSHUser = "root"
		}
		v.SSHHost = hostFromSSHCommand(vm.SSHCommand, fallbackHost)
		if t, ok := ParseTime(vm.CreatedAt); ok {
			v.Age = humanAge(now.Sub(t))
		}
		if rec, found := recByID[vm.ID]; found {
			v.Owner, v.TTL, v.Tracked, v.SSHKey = rec.Owner, rec.TTL, true, displayPath(rec.KeyPath)
			delete(recByID, rec.ID)
		}
		views = append(views, v)
	}
	for _, rec := range recByID {
		age := ""
		if t, ok := ParseTime(rec.CreatedAt); ok {
			age = humanAge(now.Sub(t))
		}
		views = append(views, vmView{
			ID: rec.ID, Name: rec.Name, Owner: rec.Owner, State: "missing",
			Age: age, TTL: rec.TTL, Tracked: true,
		})
	}
	return views, nil
}

func stateOr(state, def string) string {
	if strings.TrimSpace(state) == "" {
		return def
	}
	return state
}

// hostnameOf extracts the host from a base URL, for the ssh fallback when
// the host API does not advertise an ssh_command.
func hostnameOf(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return ""
}

// displayPath abbreviates a path under the user's home directory with ~,
// for ssh commands shown in the console.
func displayPath(p string) string {
	if p == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

type createParams struct {
	Owner   string
	Purpose string
	TTL     string
	CPUs    int
	MemMiB  int
}

// opCreateVM mirrors the CLI create: name = owner-purpose, a TTL is always
// recorded, server-generated keys land in ~/.firecracker-broker/keys.
func (s *uiServer) opCreateVM(p createParams) (map[string]any, error) {
	p.Owner = sanitizePart(p.Owner)
	if p.Owner == "" {
		p.Owner = "ui-agent"
	}
	if !strings.HasPrefix(p.Owner, uiOwnerPrefix) {
		p.Owner = uiOwnerPrefix + p.Owner
	}
	if p.TTL == "" {
		p.TTL = "30m"
	}
	if d, err := time.ParseDuration(p.TTL); err != nil || d <= 0 {
		return nil, fmt.Errorf("invalid ttl %q (examples: 30m, 2h)", p.TTL)
	}
	p.Purpose = sanitizePart(p.Purpose)

	name := p.Owner + "-"
	if p.Purpose != "" {
		name += p.Purpose
	} else {
		suffix, err := randomSuffix()
		if err != nil {
			return nil, err
		}
		name += suffix
	}

	vm, err := s.client.CreateVM(CreateVMRequest{Name: name, CPUs: p.CPUs, MemMiB: p.MemMiB})
	if err != nil {
		return nil, err
	}
	if vm.ID == "" {
		return nil, errors.New("host API returned a VM without an id")
	}

	keyPath := ""
	if vm.SSHPrivateKey != "" {
		if keyPath, err = writePrivateKey(vm.ID, vm.SSHPrivateKey); err != nil {
			_ = s.client.DeleteVM(vm.ID) // no key => unusable; do not leak the VM
			return nil, fmt.Errorf("could not save the generated private key: %w", err)
		}
	}
	if err := UpdateState(func(st *State) error {
		st.VMs = append(st.VMs, Record{
			ID:        vm.ID,
			Name:      vm.Name,
			Owner:     p.Owner,
			CreatedAt: normalizeTime(vm.CreatedAt),
			TTL:       p.TTL,
			KeyPath:   keyPath,
		})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("VM created but the local state could not be updated: %w", err)
	}

	return map[string]any{
		"id": vm.ID, "name": vm.Name, "owner": p.Owner, "ttl": p.TTL,
		"state": stateOr(vm.State, "unknown"), "ip": vm.IP, "ssh_port": vm.SSHPort,
		"ssh_user": vm.SSHUser, "ssh_command": vm.SSHCommand, "ssh_key_path": keyPath,
	}, nil
}

// mayModify enforces the ui-* namespace. A VM is modifiable when it is
// tracked for a ui-* owner, or (untracked) its name carries the prefix.
func (s *uiServer) mayModify(id string) error {
	st, err := LoadState()
	if err != nil {
		return err
	}
	for _, rec := range st.VMs {
		if rec.ID == id {
			if s.allowAll || strings.HasPrefix(rec.Owner, uiOwnerPrefix) {
				return nil
			}
			return fmt.Errorf("VM %s is owned by %q; the console only modifies %s* VMs (start with --allow-all-owners to lift this)", id, rec.Owner, uiOwnerPrefix)
		}
	}
	// Not tracked locally: fall back to the live name.
	live, err := s.client.ListVMs()
	if err != nil {
		return fmt.Errorf("cannot verify ownership of %s: %w", id, err)
	}
	for _, vm := range live {
		if vm.ID == id {
			if s.allowAll || strings.HasPrefix(vm.Name, uiOwnerPrefix) {
				return nil
			}
			return fmt.Errorf("VM %s (%s) is not a console-managed VM", id, vm.Name)
		}
	}
	return fmt.Errorf("VM %s not found", id)
}

// opDeleteVM is the guarded CLI delete: 404-tolerant, drops the record and
// the key file.
func (s *uiServer) opDeleteVM(id string) error {
	if err := s.mayModify(id); err != nil {
		return err
	}
	var keyPaths []string
	if err := s.client.DeleteVM(id); err != nil {
		var apiErr *APIError
		if !(errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound) {
			return err
		}
	}
	if err := UpdateState(func(st *State) error {
		kept := st.VMs[:0]
		for _, rec := range st.VMs {
			if rec.ID == id {
				keyPaths = append(keyPaths, rec.KeyPath)
				continue
			}
			kept = append(kept, rec)
		}
		st.VMs = kept
		return nil
	}); err != nil {
		return err
	}
	if dir, err := brokerDir(); err == nil {
		keyPaths = append(keyPaths, filepath.Join(dir, "keys", id+".key"))
	}
	for _, p := range keyPaths {
		if err := removeKeyFile(p); err != nil {
			return fmt.Errorf("VM deleted but key cleanup failed for %s: %w", p, err)
		}
	}
	return nil
}

func (s *uiServer) opPower(id, action string) (VM, error) {
	if err := s.mayModify(id); err != nil {
		return VM{}, err
	}
	if action == "stop" {
		return s.client.StopVM(id)
	}
	return s.client.StartVM(id)
}

// --- running commands inside VMs ---

const (
	vmExecMinTimeout = time.Second
	vmExecMaxTimeout = 10 * time.Minute
	vmExecMaxOutput  = 96 << 10 // combined stdout+stderr kept for the caller
)

type execResult struct {
	VM        string `json:"vm"`
	ExitCode  int    `json:"exit_code"`
	Output    string `json:"output"`
	TimedOut  bool   `json:"timed_out,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// hostFromSSHCommand pulls the destination out of a ssh_command like
// "ssh -p 2200 root@192.168.100.7" (same convention agent-vm.sh relies on).
func hostFromSSHCommand(cmd, fallback string) string {
	i := strings.LastIndex(cmd, "@")
	if i < 0 {
		return fallback
	}
	host := cmd[i+1:]
	if j := strings.IndexAny(host, " \t"); j >= 0 {
		host = host[:j]
	}
	host = strings.TrimSuffix(strings.TrimSpace(host), ":")
	if host == "" {
		return fallback
	}
	return host
}

// sshTarget resolves where a VM's shell is reachable: the broker host
// forwards one port per VM, and the generated private key lives under
// ~/.firecracker-broker/keys.
func (s *uiServer) sshTarget(id string) (dest string, port int, keyPath string, err error) {
	live, err := s.client.ListVMs()
	if err != nil {
		return "", 0, "", err
	}
	var vm *VM
	for i := range live {
		if live[i].ID == id {
			vm = &live[i]
			break
		}
	}
	if vm == nil {
		return "", 0, "", fmt.Errorf("VM %s is not on the host anymore", id)
	}

	fallback, _ := url.Parse(s.client.BaseURL)
	fallbackHost := ""
	if fallback != nil {
		fallbackHost = fallback.Hostname()
	}
	user := vm.SSHUser
	if user == "" {
		user = "root"
	}
	dest = fmt.Sprintf("%s@%s", user, hostFromSSHCommand(vm.SSHCommand, fallbackHost))
	port = vm.SSHPort
	if port == 0 {
		port = 22
	}

	keyPath = ""
	if st, err := LoadState(); err == nil {
		for _, rec := range st.VMs {
			if rec.ID == id {
				keyPath = rec.KeyPath
				break
			}
		}
	}
	if keyPath == "" {
		if dir, err := brokerDir(); err == nil {
			keyPath = filepath.Join(dir, "keys", id+".key")
		}
	}
	if keyPath == "" {
		return "", 0, "", errors.New("no ssh key known for " + id)
	}
	if _, err := os.Stat(keyPath); err != nil {
		return "", 0, "", fmt.Errorf("ssh key %s is missing (was the VM created on another machine?)", keyPath)
	}
	return dest, port, keyPath, nil
}

// opRunInVM executes one non-interactive shell command inside a VM, over
// SSH, as the VM's ssh user. The command never touches the host machine:
// the local process is only the ssh client. Output is capped, the command
// is killed at the timeout.
func (s *uiServer) opRunInVM(id, command string, timeout time.Duration) (execResult, error) {
	var res execResult
	if strings.TrimSpace(command) == "" {
		return res, errors.New("empty command")
	}
	if err := s.mayModify(id); err != nil {
		return res, err
	}
	dest, port, keyPath, err := s.sshTarget(id)
	if err != nil {
		return res, err
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	timeout = min(timeout, vmExecMaxTimeout)
	timeout = max(timeout, vmExecMinTimeout)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "LogLevel=ERROR",
		"-p", strconv.Itoa(port),
		"-i", keyPath,
		dest,
		"--", command,
	}
	cmd := exec.CommandContext(ctx, s.sshBin, args...)
	setupExecKill(cmd)
	cmd.WaitDelay = 2 * time.Second // bound the wait if pipes linger after the kill
	out, err := cmd.CombinedOutput()
	if cmd.ProcessState == nil {
		return res, fmt.Errorf("could not run %s: %v", s.sshBin, err)
	}
	res.VM = id
	res.ExitCode = cmd.ProcessState.ExitCode()
	res.TimedOut = ctx.Err() == context.DeadlineExceeded
	res.Output, res.Truncated = truncateMiddle(string(out), vmExecMaxOutput)
	return res, nil
}

// truncateMiddle keeps head and tail of oversized output with a marker in
// between, so both command banners and final errors stay visible.
func truncateMiddle(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	const marker = "\n…[output truncated]…\n"
	head := max * 2 / 3
	tail := max - head - len(marker)
	if tail < 0 {
		tail = 0
	}
	return s[:head] + marker + s[len(s)-tail:], true
}

type sweepResult struct {
	Deleted []string `json:"deleted"`
	Forgot  []string `json:"forgot"`
	Failed  []string `json:"failed"`
	Skipped int      `json:"kept"`
}

// opSweep reuses PlanSweep: TTL-expired VMs are deleted, records whose VM is
// already gone are forgotten.
func (s *uiServer) opSweep(grace time.Duration) (sweepResult, error) {
	var res sweepResult
	st, err := LoadState()
	if err != nil {
		return res, err
	}
	if len(st.VMs) == 0 {
		return res, nil
	}
	live, err := s.client.ListVMs()
	if err != nil {
		return res, err
	}
	liveIDs := make(map[string]bool, len(live))
	for _, vm := range live {
		liveIDs[vm.ID] = true
	}
	toDelete, toForget := PlanSweep(st.VMs, liveIDs, time.Now(), grace)

	swept := make(map[string]bool, len(toDelete)+len(toForget))
	for _, rec := range toForget {
		swept[rec.ID] = true
		res.Forgot = append(res.Forgot, rec.ID)
	}
	for _, rec := range toDelete {
		if err := s.client.DeleteVM(rec.ID); err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				swept[rec.ID] = true
				res.Forgot = append(res.Forgot, rec.ID)
				continue
			}
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", rec.ID, err))
			continue
		}
		swept[rec.ID] = true
		res.Deleted = append(res.Deleted, rec.ID)
		if err := removeKeyFile(rec.KeyPath); err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: key cleanup: %v", rec.ID, err))
		}
	}
	if err := UpdateState(func(st *State) error {
		kept := st.VMs[:0]
		for _, rec := range st.VMs {
			if !swept[rec.ID] {
				kept = append(kept, rec)
			}
		}
		st.VMs = kept
		res.Skipped = len(kept)
		return nil
	}); err != nil {
		return res, err
	}
	return res, nil
}

// --- REST handlers ---

func (s *uiServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("GET only"))
		return
	}
	out := map[string]any{
		"settings": map[string]any{
			"broker_url":       s.client.BaseURL,
			"agent_ready":      s.agentReady(),
			"model":            s.model,
			"allow_all_owners": s.allowAll,
		},
	}
	vms, err := s.opListMerged()
	if err != nil {
		out["broker_error"] = err.Error()
		vms, _ = s.opLocalViews()
	}
	out["vms"] = vms
	if cap, err := s.client.Capacity(); err == nil {
		out["capacity"] = cap
	}
	writeJSON(w, http.StatusOK, out)
}

// opLocalViews renders tracked records without contacting the host, for
// degraded status display when the broker is unreachable.
func (s *uiServer) opLocalViews() ([]vmView, error) {
	st, err := LoadState()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	views := make([]vmView, 0, len(st.VMs))
	for _, rec := range st.VMs {
		age := ""
		if t, ok := ParseTime(rec.CreatedAt); ok {
			age = humanAge(now.Sub(t))
		}
		views = append(views, vmView{
			ID: rec.ID, Name: rec.Name, Owner: rec.Owner, State: "unknown",
			Age: age, TTL: rec.TTL, Tracked: true,
		})
	}
	return views, nil
}

func (s *uiServer) handleVMs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("POST to create a VM"))
		return
	}
	var p createParams
	if !decodeBody(w, r, &p) {
		return
	}
	vm, err := s.opCreateVM(p)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusCreated, vm)
}

// handleVMItem routes /api/vms/<id> and /api/vms/<id>/(stop|start).
// Patterns with wildcards need Go 1.22; this module targets 1.21, so the
// path is split by hand.
func (s *uiServer) handleVMItem(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "api" || parts[1] != "vms" {
		writeErr(w, http.StatusNotFound, errors.New("not found"))
		return
	}
	id := parts[2]
	action := ""
	if len(parts) == 4 {
		action = parts[3]
	}

	switch {
	case r.Method == http.MethodDelete && action == "":
		if err := s.opDeleteVM(id); err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
	case r.Method == http.MethodPost && (action == "stop" || action == "start"):
		vm, err := s.opPower(id, action)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": vm.ID, "name": vm.Name, "state": stateOr(vm.State, "unknown"),
			"ip": vm.IP, "ssh_port": vm.SSHPort,
		})
	case r.Method == http.MethodPost && action == "exec":
		var body struct {
			Command        string `json:"command"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		res, err := s.opRunInVM(id, body.Command, time.Duration(body.TimeoutSeconds)*time.Second)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("unsupported method/action %s %s", r.Method, r.URL.Path))
	}
}

func (s *uiServer) handleSweep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("POST only"))
		return
	}
	var body struct {
		Grace string `json:"grace"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	grace := 2 * time.Hour
	if body.Grace != "" {
		d, err := time.ParseDuration(body.Grace)
		if err != nil || d <= 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid grace %q", body.Grace))
			return
		}
		grace = d
	}
	res, err := s.opSweep(grace)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func statusFor(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return http.StatusBadGateway
	}
	if errors.Is(err, ErrAuth) {
		return http.StatusUnauthorized
	}
	var unreach *UnreachableError
	var timeout *TimeoutError
	if errors.As(err, &unreach) || errors.As(err, &timeout) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadRequest
}

// --- OpenAI agent ---

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type agentAction struct {
	Name    string         `json:"name"`
	Args    map[string]any `json:"args,omitempty"`
	OK      bool           `json:"ok"`
	Summary string         `json:"summary"`
}

func (s *uiServer) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("POST only"))
		return
	}
	if !s.agentReady() {
		writeErr(w, http.StatusServiceUnavailable, errors.New("agent disabled: set OPENAI_API_KEY, or point OPENAI_BASE_URL at an OpenAI-compatible server (Ollama, LM Studio, vLLM — no key needed); the buttons above work regardless"))
		return
	}
	var req struct {
		Messages []chatMessage `json:"messages"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	msgs := []oaMessage{{Role: "system", Content: agentSystemPrompt}}
	for _, m := range req.Messages {
		if m.Role == "user" || m.Role == "assistant" {
			msgs = append(msgs, oaMessage{Role: m.Role, Content: m.Content})
		}
	}

	var actions []agentAction
	reply := ""
	for round := 0; round < 8; round++ {
		resp, err := s.openaiComplete(msgs)
		if err != nil {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("OpenAI request failed: %w", err))
			return
		}
		if len(resp.ToolCalls) == 0 {
			reply = resp.Content
			break
		}
		msgs = append(msgs, resp)
		for _, tc := range resp.ToolCalls {
			result, summary, err := s.toolExec(tc.Function.Name, tc.Function.Arguments)
			if err != nil {
				summary = "error: " + err.Error()
			}
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			actions = append(actions, agentAction{Name: tc.Function.Name, Args: args, OK: err == nil, Summary: summary})
			payload, _ := json.Marshal(result)
			msgs = append(msgs, oaMessage{Role: "tool", ToolCallID: tc.ID, Content: string(payload)})
		}
	}
	if reply == "" {
		reply = "The agent ran out of rounds; check the VM list for the current state."
	}
	writeJSON(w, http.StatusOK, map[string]any{"reply": reply, "actions": actions, "model": s.model})
}

// toolExec runs one agent tool call. Errors are returned to the caller (and
// to the model) as JSON on the tool message, so the agent can react.
func (s *uiServer) toolExec(name, argsJSON string) (result any, summary string, err error) {
	args := map[string]any{}
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return map[string]any{"error": "bad arguments"}, "", fmt.Errorf("bad arguments for %s: %v", name, err)
		}
	}
	switch name {
	case "list_vms":
		vms, err := s.opListMerged()
		if err != nil {
			return map[string]any{"error": err.Error()}, "", err
		}
		running := 0
		for _, v := range vms {
			if v.State == "running" {
				running++
			}
		}
		return vms, fmt.Sprintf("%d VMs, %d running", len(vms), running), nil
	case "capacity":
		cap, err := s.client.Capacity()
		if err != nil {
			return map[string]any{"error": err.Error()}, "", err
		}
		return cap, fmt.Sprintf("VMs %d/%d, %d free CPUs, %d MiB free", cap.ExistingVMs, cap.MaxVMs, cap.FreeHostCPUs, cap.FreeHostMemMiB), nil
	case "create_vm":
		vm, err := s.opCreateVM(createParams{
			Owner: argStr(args, "owner"), Purpose: argStr(args, "purpose"),
			TTL: argStr(args, "ttl"), CPUs: argInt(args, "cpus"), MemMiB: argInt(args, "mem_mib"),
		})
		if err != nil {
			return map[string]any{"error": err.Error()}, "", err
		}
		return vm, fmt.Sprintf("created %v (%v), owner %v, ttl %v", vm["id"], vm["name"], vm["owner"], vm["ttl"]), nil
	case "delete_vm":
		id := argStr(args, "id")
		if err := s.opDeleteVM(id); err != nil {
			return map[string]any{"error": err.Error()}, "", err
		}
		return map[string]any{"deleted": id}, "deleted " + id, nil
	case "stop_vm", "start_vm":
		id := argStr(args, "id")
		vm, err := s.opPower(id, strings.TrimSuffix(name, "_vm"))
		if err != nil {
			return map[string]any{"error": err.Error()}, "", err
		}
		return vm, fmt.Sprintf("%s %s, state now %s", name, id, stateOr(vm.State, "unknown")), nil
	case "run_in_vm":
		id := argStr(args, "id")
		timeout := time.Duration(argInt(args, "timeout_seconds")) * time.Second
		res, err := s.opRunInVM(id, argStr(args, "command"), timeout)
		if err != nil {
			return map[string]any{"error": err.Error()}, "", err
		}
		summary := fmt.Sprintf("exit %d on %s", res.ExitCode, id)
		if res.TimedOut {
			summary = fmt.Sprintf("timed out on %s after %s", id, timeout)
		}
		return res, summary, nil
	case "sweep":
		res, err := s.opSweep(2 * time.Hour)
		if err != nil {
			return map[string]any{"error": err.Error()}, "", err
		}
		return res, fmt.Sprintf("deleted %d, forgot %d, failed %d", len(res.Deleted), len(res.Forgot), len(res.Failed)), nil
	}
	return map[string]any{"error": "unknown tool " + name}, "", fmt.Errorf("unknown tool %q", name)
}

// argStr and argInt read tool arguments leniently: models often send numbers
// as strings.
func argStr(args map[string]any, key string) string {
	switch v := args[key].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%d", int64(v))
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func argInt(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case string:
		var n int
		_, err := fmt.Sscanf(v, "%d", &n)
		if err == nil {
			return n
		}
	}
	return 0
}

// --- OpenAI wire types (chat completions with tool calling) ---

type oaMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function oaFunctionCall `json:"function"`
}

// oaFunctionCall keeps Arguments as the JSON string the spec prescribes.
type oaFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// UnmarshalJSON tolerates the two shapes seen in the wild: arguments as a
// JSON string (the spec) or as a bare object (several OpenAI-compatible
// servers).
func (f *oaFunctionCall) UnmarshalJSON(b []byte) error {
	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	f.Name = raw.Name
	args := strings.TrimSpace(string(raw.Arguments))
	if args == "" || args == "null" {
		f.Arguments = "{}"
		return nil
	}
	if args[0] == '"' {
		return json.Unmarshal(raw.Arguments, &f.Arguments)
	}
	f.Arguments = args
	return nil
}

type oaToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

func obj(props map[string]any, required ...string) map[string]any {
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func prop(kind, desc string) map[string]any {
	return map[string]any{"type": kind, "description": desc}
}

func agentTools() []oaToolDef {
	defs := []struct {
		name, desc string
		params     map[string]any
	}{
		{"list_vms", "List all VMs: tracked owner, ttl, live state, age, ip.", obj(nil)},
		{"capacity", "Host capacity: max/existing/running VMs, free CPUs and memory.", obj(nil)},
		{"create_vm", "Create (boot) a new microVM. Owner is auto-prefixed ui-; a ttl is required.", obj(map[string]any{
			"owner":   prop("string", "owner name, e.g. build or test; prefixed with ui- when missing"),
			"purpose": prop("string", "short purpose, becomes part of the VM name"),
			"ttl":     prop("string", "time to live for sweep, e.g. 30m, 1h, 2h"),
			"cpus":    prop("integer", "vCPUs 1-4"),
			"mem_mib": prop("integer", "memory in MiB, 256-2048"),
		})},
		{"delete_vm", "Delete a VM permanently (disk is wiped). Idempotent.", obj(map[string]any{
			"id": prop("string", "VM id"),
		}, "id")},
		{"stop_vm", "Stop a VM; it stays restartable via start_vm.", obj(map[string]any{
			"id": prop("string", "VM id"),
		}, "id")},
		{"start_vm", "Start a previously stopped VM.", obj(map[string]any{
			"id": prop("string", "VM id"),
		}, "id")},
		{"run_in_vm", "Run a non-interactive shell command inside a running VM (root, over SSH; stdout+stderr combined). The VM must be running.", obj(map[string]any{
			"id":              prop("string", "VM id"),
			"command":         prop("string", "shell command line to execute in the VM"),
			"timeout_seconds": prop("integer", "kill after this many seconds, 1-600, default 60"),
		}, "id", "command")},
		{"sweep", "Delete TTL-expired VMs and forget records of gone VMs.", obj(nil)},
	}
	out := make([]oaToolDef, 0, len(defs))
	for _, d := range defs {
		var t oaToolDef
		t.Type = "function"
		t.Function.Name, t.Function.Description, t.Function.Parameters = d.name, d.desc, d.params
		out = append(out, t)
	}
	return out
}

var errOpenAIBase = errors.New("OPENAI_BASE_URL is invalid")

func (s *uiServer) openaiComplete(msgs []oaMessage) (oaMessage, error) {
	body, err := json.Marshal(map[string]any{
		"model":    s.model,
		"messages": msgs,
		"tools":    agentTools(),
	})
	if err != nil {
		return oaMessage{}, err
	}
	req, err := http.NewRequest(http.MethodPost, openAIBase()+"/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return oaMessage{}, errOpenAIBase
	}
	req.Header.Set("Content-Type", "application/json")
	if s.openaiKey != "" { // local OpenAI-compatible servers need no key
		req.Header.Set("Authorization", "Bearer "+s.openaiKey)
	}
	httpc := &http.Client{Timeout: 90 * time.Second}
	resp, err := httpc.Do(req)
	if err != nil {
		return oaMessage{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return oaMessage{}, err
	}
	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(data))
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return oaMessage{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Choices []struct {
			Message oaMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return oaMessage{}, fmt.Errorf("decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return oaMessage{}, errors.New("empty response from OpenAI")
	}
	return out.Choices[0].Message, nil
}
