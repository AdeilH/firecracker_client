// Command firecracker-broker manages Firecracker microVMs on a shared host
// through the host's REST API, on behalf of multiple agents.
package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	defaultBaseURL = "http://192.168.0.118:8420"
	tokenEnvVar    = "MICROVMS_TOKEN"
	urlEnvVar      = "MICROVMS_URL"
	authNotice     = "microvms access not configured or revoked"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usageText = `firecracker-broker manages Firecracker microVMs on a shared host.

Usage:
  firecracker-broker <command> [flags]

Commands:
  create     create a VM and track it for the owning agent
  list       list VMs tracked by this broker
  stop       stop a VM (disk is kept, VM stays restartable)
  start      start a previously stopped VM
  delete     delete a VM and drop its local record
  sweep      delete tracked VMs older than their TTL
  capacity   show host capacity

Every command accepts --url to override the host API base URL
(default: $MICROVMS_URL or ` + defaultBaseURL + `).

Authentication: the bearer token in $MICROVMS_TOKEN is sent with every
request; if it is missing or rejected, the command fails closed.

Examples:
  firecracker-broker create --owner alice --purpose demo --cpus 2 --mem-mib 512 --ttl 2h
  firecracker-broker create --owner bob --ssh-key ~/.ssh/id_ed25519.pub
  firecracker-broker list --owner alice
  firecracker-broker stop --id vm-42
  firecracker-broker delete --id vm-42
  firecracker-broker sweep --grace 2h
  firecracker-broker capacity
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "create":
		return cmdCreate(rest, stdout, stderr)
	case "list":
		return cmdList(rest, stdout, stderr)
	case "stop":
		return cmdPower(rest, stdout, stderr, "stop")
	case "start":
		return cmdPower(rest, stdout, stderr, "start")
	case "delete":
		return cmdDelete(rest, stdout, stderr)
	case "sweep":
		return cmdSweep(rest, stdout, stderr)
	case "capacity":
		return cmdCapacity(rest, stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "firecracker-broker 0.1.0")
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) { fmt.Fprint(w, usageText) }

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: firecracker-broker %s [flags]\n\nFlags:\n", name)
		fs.PrintDefaults()
	}
	return fs
}

// parseFlags parses args and reports whether the command should proceed; a
// false proceed comes with the exit code to return (0 for -h/--help, 2 for
// usage errors).
func parseFlags(fs *flag.FlagSet, args []string) (proceed bool, code int) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, 0
		}
		return false, 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "error: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return false, 2
	}
	return true, 0
}

func baseURLFromEnv() string {
	if v := strings.TrimSpace(os.Getenv(urlEnvVar)); v != "" {
		return v
	}
	return defaultBaseURL
}

// requireToken returns the API token, or false when it is not configured.
func requireToken() (string, bool) {
	tok := strings.TrimSpace(os.Getenv(tokenEnvVar))
	return tok, tok != ""
}

func report(err error, stderr io.Writer) int {
	var unreachable *UnreachableError
	if errors.As(err, &unreachable) {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	var timeoutErr *TimeoutError
	if errors.As(err, &timeoutErr) {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	if errors.Is(err, ErrAuth) {
		fmt.Fprintln(stderr, authNotice)
		return 1
	}
	fmt.Fprintf(stderr, "error: %v\n", err)
	return 1
}

// vmOutput is what the CLI prints for a VM. It deliberately has no field for
// ssh_private_key, so generated key material can never reach stdout.
type vmOutput struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	State      string `json:"state"`
	IP         string `json:"ip"`
	SSHPort    int    `json:"ssh_port"`
	SSHUser    string `json:"ssh_user"`
	SSHCommand string `json:"ssh_command"`
	VCPUs      int    `json:"vcpus"`
	MemMiB     int    `json:"mem_mib"`
	CreatedAt  string `json:"created_at"`
	KeyPath    string `json:"ssh_key_path,omitempty"`
}

func printVM(vm VM, keyPath string, stdout io.Writer) {
	out := vmOutput{
		ID:         vm.ID,
		Name:       vm.Name,
		State:      vm.State,
		IP:         vm.IP,
		SSHPort:    vm.SSHPort,
		SSHUser:    vm.SSHUser,
		SSHCommand: vm.SSHCommand,
		VCPUs:      vm.VCPUs,
		MemMiB:     vm.MemMiB,
		CreatedAt:  vm.CreatedAt,
		KeyPath:    keyPath,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintf(stdout, "{\"id\":%q,\"print_error\":%q}\n", vm.ID, err.Error())
		return
	}
	fmt.Fprintln(stdout, string(b))
}

func cmdCreate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("create", stderr)
	owner := fs.String("owner", "", "owner name; becomes part of the VM name (required)")
	purpose := fs.String("purpose", "", "optional purpose; becomes part of the VM name")
	cpus := fs.Int("cpus", 0, "vCPUs, 1-4 (host default when omitted)")
	mem := fs.Int("mem-mib", 0, "memory in MiB, 256-2048 (host default when omitted)")
	sshKey := fs.String("ssh-key", "", "public key: path to a file, or the key string itself")
	ttlFlag := fs.String("ttl", "", "time to live used by sweep, e.g. 30m, 2h")
	urlFlag := fs.String("url", baseURLFromEnv(), "host API base URL")
	if proceed, code := parseFlags(fs, args); !proceed {
		return code
	}
	if strings.TrimSpace(*owner) == "" {
		fmt.Fprintln(fs.Output(), "error: --owner is required")
		return 2
	}
	ttl := strings.TrimSpace(*ttlFlag)
	if ttl != "" {
		if d, err := time.ParseDuration(ttl); err != nil || d <= 0 {
			fmt.Fprintf(fs.Output(), "error: invalid --ttl %q (examples: 30m, 2h)\n", ttl)
			return 2
		}
	}
	tok, ok := requireToken()
	if !ok {
		fmt.Fprintln(stderr, authNotice)
		return 1
	}

	name := sanitizePart(*owner) + "-"
	if p := sanitizePart(*purpose); p != "" {
		name += p
	} else {
		suffix, err := randomSuffix()
		if err != nil {
			return report(err, stderr)
		}
		name += suffix
	}

	keyValue := ""
	if *sshKey != "" {
		v, err := resolveSSHKey(*sshKey)
		if err != nil {
			return report(err, stderr)
		}
		keyValue = v
	}

	vm, err := NewClient(*urlFlag, tok).CreateVM(CreateVMRequest{
		Name:   name,
		CPUs:   *cpus,
		MemMiB: *mem,
		SSHKey: keyValue,
	})
	if err != nil {
		return report(err, stderr)
	}
	if vm.ID == "" {
		fmt.Fprintln(stderr, "error: host API returned a VM without an id")
		return 1
	}

	exit := 0
	keyPath := ""
	if vm.SSHPrivateKey != "" {
		keyPath, err = writePrivateKey(vm.ID, vm.SSHPrivateKey)
		if err != nil {
			fmt.Fprintf(stderr, "warning: could not save the generated private key (%v); the key is lost, consider deleting this VM\n", err)
			exit = 1
		}
	}
	if err := UpdateState(func(s *State) error {
		s.VMs = append(s.VMs, Record{
			ID:        vm.ID,
			Name:      vm.Name,
			Owner:     *owner,
			CreatedAt: normalizeTime(vm.CreatedAt),
			TTL:       ttl,
			KeyPath:   keyPath,
		})
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "warning: VM created but the local state could not be updated: %v\n", err)
		exit = 1
	}

	printVM(vm, keyPath, stdout)
	return exit
}

// sanitizePart makes a name fragment safe to embed in a VM name.
func sanitizePart(s string) string {
	return strings.Join(strings.Fields(s), "-")
}

// randomSuffix returns five random lowercase alphanumeric characters.
func randomSuffix() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 5)
	for i := range out {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", fmt.Errorf("generate name suffix: %w", err)
		}
		out[i] = alphabet[k.Int64()]
	}
	return string(out), nil
}

// resolveSSHKey accepts either a path to a public key file or the key
// material itself and returns the key material. A slash-bearing value that
// does not exist is treated as a mistyped path and rejected.
func resolveSSHKey(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", errors.New("empty --ssh-key")
	}
	if _, err := os.Stat(v); err == nil {
		b, readErr := os.ReadFile(v)
		if readErr != nil {
			return "", readErr
		}
		s := strings.TrimSpace(string(b))
		if s == "" {
			return "", fmt.Errorf("ssh key file %s is empty", v)
		}
		return s, nil
	} else if strings.ContainsRune(v, os.PathSeparator) && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("ssh key file %s: %w", v, err)
	} else if errors.Is(err, os.ErrNotExist) && strings.ContainsRune(v, os.PathSeparator) {
		return "", fmt.Errorf("ssh key file %s not found", v)
	}
	return v, nil
}

// writePrivateKey stores a server-generated private key under
// ~/.firecracker-broker/keys/<id>.key with 0600 permissions and returns the
// path. The material must never be printed anywhere.
func writePrivateKey(id, material string) (string, error) {
	dir, err := brokerDir()
	if err != nil {
		return "", err
	}
	keysDir := filepath.Join(dir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(keysDir, id+".key")
	if err := os.WriteFile(path, []byte(material), 0o600); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func removeKeyFile(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func cmdList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("list", stderr)
	owner := fs.String("owner", "", "only show VMs recorded for this owner (exact match)")
	urlFlag := fs.String("url", baseURLFromEnv(), "host API base URL")
	if proceed, code := parseFlags(fs, args); !proceed {
		return code
	}
	tok, ok := requireToken()
	if !ok {
		fmt.Fprintln(stderr, authNotice)
		return 1
	}
	st, err := LoadState()
	if err != nil {
		return report(err, stderr)
	}
	live, err := NewClient(*urlFlag, tok).ListVMs()
	if err != nil {
		return report(err, stderr)
	}
	byID := make(map[string]VM, len(live))
	for _, vm := range live {
		byID[vm.ID] = vm
	}

	now := time.Now()
	tw := tabwriter.NewWriter(stdout, 0, 2, 3, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tOWNER\tAGE\tIP\tSSH_PORT")
	for _, rec := range st.VMs {
		if *owner != "" && rec.Owner != *owner {
			continue
		}
		state, ip, port := "missing", "-", "-"
		age := "-"
		if t, ok2 := ParseTime(rec.CreatedAt); ok2 {
			age = humanAge(now.Sub(t))
		}
		if vm, found := byID[rec.ID]; found {
			state = vm.State
			if state == "" {
				state = "unknown"
			}
			if vm.IP == "" {
				ip = "-"
			} else {
				ip = vm.IP
			}
			port = strconv.Itoa(vm.SSHPort)
			if t, ok3 := ParseTime(vm.CreatedAt); ok3 {
				age = humanAge(now.Sub(t))
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", rec.ID, rec.Name, state, rec.Owner, age, ip, port)
	}
	if err := tw.Flush(); err != nil {
		return report(err, stderr)
	}
	return 0
}

// humanAge renders a duration compactly: 45s, 12m, 3h27m, 2d4h.
func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	if days > 0 {
		if h > 0 {
			return fmt.Sprintf("%dd%dh", days, h)
		}
		return fmt.Sprintf("%dd", days)
	}
	if m > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dh", h)
}

// cmdPower proxies POST /v1/vms/{id}/stop and /start.
func cmdPower(args []string, stdout, stderr io.Writer, action string) int {
	fs := newFlagSet(action, stderr)
	id := fs.String("id", "", "VM id (required)")
	urlFlag := fs.String("url", baseURLFromEnv(), "host API base URL")
	if proceed, code := parseFlags(fs, args); !proceed {
		return code
	}
	if *id == "" {
		fmt.Fprintln(fs.Output(), "error: --id is required")
		return 2
	}
	tok, ok := requireToken()
	if !ok {
		fmt.Fprintln(stderr, authNotice)
		return 1
	}
	client := NewClient(*urlFlag, tok)
	var vm VM
	var err error
	if action == "stop" {
		vm, err = client.StopVM(*id)
	} else {
		vm, err = client.StartVM(*id)
	}
	if err != nil {
		return report(err, stderr)
	}
	printVM(vm, "", stdout)
	return 0
}

func cmdDelete(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("delete", stderr)
	id := fs.String("id", "", "VM id (required)")
	urlFlag := fs.String("url", baseURLFromEnv(), "host API base URL")
	if proceed, code := parseFlags(fs, args); !proceed {
		return code
	}
	if *id == "" {
		fmt.Fprintln(fs.Output(), "error: --id is required")
		return 2
	}
	tok, ok := requireToken()
	if !ok {
		fmt.Fprintln(stderr, authNotice)
		return 1
	}
	if err := NewClient(*urlFlag, tok).DeleteVM(*id); err != nil {
		var apiErr *APIError
		if !(errors.As(err, &apiErr) && apiErr.StatusCode == 404) {
			return report(err, stderr)
		}
		fmt.Fprintf(stderr, "note: VM %s is not on the host anymore; cleaning up local state\n", *id)
	}

	var keyPaths []string
	stateErr := UpdateState(func(s *State) error {
		kept := s.VMs[:0]
		for _, rec := range s.VMs {
			if rec.ID == *id {
				if rec.KeyPath != "" {
					keyPaths = append(keyPaths, rec.KeyPath)
				}
				continue
			}
			kept = append(kept, rec)
		}
		s.VMs = kept
		return nil
	})
	// Also try the conventional location in case the record was already gone.
	if dir, err := brokerDir(); err == nil {
		keyPaths = append(keyPaths, filepath.Join(dir, "keys", *id+".key"))
	}
	for _, p := range keyPaths {
		if err := removeKeyFile(p); err != nil {
			fmt.Fprintf(stderr, "warning: could not remove key file %s: %v\n", p, err)
		}
	}
	if stateErr != nil {
		fmt.Fprintf(stderr, "warning: VM deleted but the local state could not be updated: %v\n", stateErr)
		return 1
	}
	fmt.Fprintf(stdout, "deleted %s\n", *id)
	return 0
}

func cmdSweep(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("sweep", stderr)
	graceFlag := fs.String("grace", "2h", "TTL applied to records without their own --ttl, e.g. 30m, 2h")
	urlFlag := fs.String("url", baseURLFromEnv(), "host API base URL")
	if proceed, code := parseFlags(fs, args); !proceed {
		return code
	}
	grace, err := time.ParseDuration(*graceFlag)
	if err != nil || grace <= 0 {
		fmt.Fprintf(fs.Output(), "error: invalid --grace %q (examples: 30m, 2h)\n", *graceFlag)
		return 2
	}
	st, err := LoadState()
	if err != nil {
		return report(err, stderr)
	}
	if len(st.VMs) == 0 {
		fmt.Fprintln(stdout, "sweep: nothing tracked")
		return 0
	}
	tok, ok := requireToken()
	if !ok {
		fmt.Fprintln(stderr, authNotice)
		return 1
	}
	client := NewClient(*urlFlag, tok)
	live, err := client.ListVMs()
	if err != nil {
		return report(err, stderr)
	}
	liveIDs := make(map[string]bool, len(live))
	for _, vm := range live {
		liveIDs[vm.ID] = true
	}

	toDelete, toForget := PlanSweep(st.VMs, liveIDs, time.Now(), grace)

	swept := make(map[string]bool)
	deleted, forgotten, failed := 0, 0, 0
	for _, rec := range toForget {
		swept[rec.ID] = true
		forgotten++
		fmt.Fprintf(stdout, "forgot %s (%s): no longer on the host\n", rec.ID, rec.Name)
	}
	for _, rec := range toDelete {
		err := client.DeleteVM(rec.ID)
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
				swept[rec.ID] = true
				forgotten++
				fmt.Fprintf(stdout, "forgot %s (%s): already gone from the host\n", rec.ID, rec.Name)
				if rmErr := removeKeyFile(rec.KeyPath); rmErr != nil {
					fmt.Fprintf(stderr, "sweep: warning: could not remove key file %s: %v\n", rec.KeyPath, rmErr)
				}
				continue
			}
			failed++
			fmt.Fprintf(stderr, "sweep: could not delete %s (%s): %v\n", rec.ID, rec.Name, err)
			continue
		}
		swept[rec.ID] = true
		deleted++
		fmt.Fprintf(stdout, "deleted %s (%s): TTL expired\n", rec.ID, rec.Name)
		if rmErr := removeKeyFile(rec.KeyPath); rmErr != nil {
			fmt.Fprintf(stderr, "sweep: warning: could not remove key file %s: %v\n", rec.KeyPath, rmErr)
		}
	}

	if err := UpdateState(func(s *State) error {
		kept := s.VMs[:0]
		for _, rec := range s.VMs {
			if !swept[rec.ID] {
				kept = append(kept, rec)
			}
		}
		s.VMs = kept
		return nil
	}); err != nil {
		return report(err, stderr)
	}

	fmt.Fprintf(stdout, "sweep: deleted %d, forgot %d, failed %d\n", deleted, forgotten, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

func cmdCapacity(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("capacity", stderr)
	urlFlag := fs.String("url", baseURLFromEnv(), "host API base URL")
	if proceed, code := parseFlags(fs, args); !proceed {
		return code
	}
	tok, ok := requireToken()
	if !ok {
		fmt.Fprintln(stderr, authNotice)
		return 1
	}
	cap, err := NewClient(*urlFlag, tok).Capacity()
	if err != nil {
		return report(err, stderr)
	}
	b, err := json.MarshalIndent(cap, "", "  ")
	if err != nil {
		return report(err, stderr)
	}
	fmt.Fprintln(stdout, string(b))
	return 0
}
