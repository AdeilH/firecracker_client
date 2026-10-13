# firecracker-broker

Go CLI that manages Firecracker microVMs on a shared host through its REST
API. Single static binary, Go stdlib only.

## Build

```sh
CGO_ENABLED=0 go build -o firecracker-broker .
```

## Setup

```sh
export MICROVMS_TOKEN=...   # bearer token, required for every command
export MICROVMS_URL=...     # optional; default http://192.168.0.118:8420 (--url overrides)
```

- Missing or revoked token: prints `microvms access not configured or revoked`, exits 1.
- Host down: prints `host unreachable: is 192.168.0.118 online?`, exits 1.
- All HTTP calls: 5s timeout, no retries.

## Commands

```sh
firecracker-broker create --owner alice --purpose demo --cpus 2 --mem-mib 512 --ttl 2h
firecracker-broker create --owner bob --ssh-key ~/.ssh/id_ed25519.pub
firecracker-broker list [--owner alice]
firecracker-broker stop --id vm-42
firecracker-broker start --id vm-42
firecracker-broker delete --id vm-42
firecracker-broker sweep [--grace 2h]
firecracker-broker capacity
```

- VM name is `<owner>-<purpose|random5>`. Host limits: 1–4 vCPUs, 256–2048 MiB, 20 VMs.
- Without `--ssh-key` the server generates a keypair: the private key goes to
  `~/.firecracker-broker/keys/<id>.key` (0600) and the create output shows
  `ssh_command` and `ssh_key_path` — never the key material.
- `delete` is idempotent: a 404 still cleans local state and exits 0. A slow
  host-side teardown can outlast the 5s timeout; on `request timed out after 5s`
  the delete may still have completed — check `list` or re-run.
- `sweep` deletes tracked VMs older than their ttl (default grace 2h) and
  forgets records whose VM is gone. Suggested cron:
  `*/10 * * * * firecracker-broker sweep`

## Files

| Path | Purpose |
| --- | --- |
| `~/.firecracker-broker/state.json` | tracked VMs (id, owner, ttl, key path); atomic writes |
| `~/.firecracker-broker/keys/<id>.key` | generated private keys, 0600 |
| `~/.firecracker-broker/token` | optional; scripts read it when `MICROVMS_TOKEN` is unset |

## Agents & subagents

[`AGENTS.md`](AGENTS.md) is the contract file to hand to agents;
[`scripts/agent-vm.sh`](scripts/agent-vm.sh) gives each one a one-command VM
lifecycle: `up <owner>` / `run <id> <cmd>` / `get <id> <remote> <local>` /
`down <id>`. One unique `--owner` per agent keeps `list --owner` attribution
and lets cron `sweep` reap abandoned VMs. A full recorded session — worker
agent boots its VM, works inside, carries results out, retires it — lives in
[`examples/agent-vm-example.md`](examples/agent-vm-example.md).

## Web console & agent

```sh
export MICROVMS_TOKEN=...          # or ~/.firecracker-broker/token
export MICROVMS_URL=http://host:8420
export OPENAI_API_KEY=sk-...       # optional: enables the agent chat
firecracker-broker ui              # http://127.0.0.1:8090
```

The `ui` command also auto-loads a gitignored `.env` from the working
directory (copy [`.env.example`](.env.example)); real environment variables
take precedence over the file.

`ui` serves a single-page console: capacity chips, a VM table (boot / stop /
start / delete / sweep-expired buttons) and a chat panel backed by any
OpenAI-compatible chat-completions API with tool calling. The agent gets
`list_vms`, `capacity`, `create_vm`, `delete_vm`, `stop_vm`, `start_vm`,
`run_in_vm` and `sweep`. `run_in_vm` executes one non-interactive shell
command inside a running VM over SSH (system `ssh` binary, per-VM generated
key): the model works inside the VM's shell but never gets a shell on the
host machine or on other owners' VMs — same ui-* guard as every other
mutation, plus per-call timeouts (1s–10m) and capped output. The agent
follows the same contract as AGENTS.md: a TTL is always set
(default 30m), one owner per task, VMs are retired when done. Everything the
console and its agent create is namespaced under the `ui-` owner prefix;
VMs owned by others are visible read-only, and mutations on them are
refused (lift with `firecracker-broker ui --allow-all-owners`). The buttons
work without any LLM config. For the agent, `OPENAI_API_KEY` enables the
hosted OpenAI API; alternatively point `OPENAI_BASE_URL` at an
OpenAI-compatible server — local ones (Ollama `http://localhost:11434/v1`,
LM Studio, vLLM) need no key. `OPENAI_MODEL` (default `gpt-4o-mini`) names
the model on that endpoint. Console state is the shared
`~/.firecracker-broker/state.json`, so CLI, script and UI stay in sync.

## Development

```sh
go test ./...
gofmt -l .
go vet ./...
```
