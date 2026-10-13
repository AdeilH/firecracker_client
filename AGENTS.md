# Agent instructions: your personal microVM

You get your own Firecracker microVM for your task. Manage it **only**
through `scripts/agent-vm.sh` — it wraps `firecracker-broker`. Never call
the host HTTP API directly, never SSH into the host, never touch another
agent's VM.

## Rules

1. **One owner per agent.** Every agent and subagent uses its own unique
   owner name (your agent id). The VM created with it belongs to you alone:
   ```sh
   scripts/agent-vm.sh up <your-agent-name> [purpose] [ttl]
   ```
2. **Never touch other owners' VMs.** `firecracker-broker list` shows the
   owner of every tracked VM; `list --owner <name>` shows one agent's VMs.
3. **A TTL is always set** (default 30m). When it expires, the host cron
   (`*/10 * * * * firecracker-broker sweep`) deletes your VM even if you
   died mid-task. For longer work pass a longer ttl, e.g. `2h`.
4. **Nothing inside the VM survives `down`.** Copy results out first:
   ```sh
   scripts/agent-vm.sh get <id> /tmp/result.txt ./result.txt
   ```
5. **Retire your VM when done**, even early:
   ```sh
   scripts/agent-vm.sh down <id>
   ```
   `down` is idempotent — safe to re-run.
6. **Check capacity before spawning** (host cap: 20 VMs):
   `firecracker-broker capacity`. If it's full, wait and retry — never
   delete other agents' VMs to make room.

## Typical session

```sh
scripts/agent-vm.sh up codex-sub1 build      # prints the VM id — remember it
scripts/agent-vm.sh run <id> 'uname -a && df -h'
scripts/agent-vm.sh run <id> ./build.sh      # longer tasks: run a script
scripts/agent-vm.sh get <id> /tmp/out.bin ./out.bin
scripts/agent-vm.sh down <id>
```

## Troubleshooting

- `run` reports sshd not coming up: freshly created VMs need a few seconds
  to boot; the script retries for up to 3 minutes.
- A `delete` that reports `request timed out after 5s` may still have
  completed host-side — re-run `down` (idempotent) or check
  `firecracker-broker list`.
- `microvms access not configured or revoked` means the token is missing or
  invalid: check `MICROVMS_TOKEN` or `~/.firecracker-broker/token`.
