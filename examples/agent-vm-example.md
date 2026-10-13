# Example: a full agent ↔ broker microVM session

This is a real recorded session (2026-10-13), not a mock-up. A coordinator
agent handed a task to a worker agent; the worker got its own microVM from
`firecracker-broker`, did work inside it over SSH, copied the results out,
and told the broker to delete the VM. Every command below went through
[`scripts/agent-vm.sh`](../scripts/agent-vm.sh) per the [AGENTS.md](../AGENTS.md)
contract — no direct host API calls, no manual SSH.

- **Worker owner name:** `zcode-demo-sub1` (one unique owner per agent)
- **VM:** id `609be1ea4021ab41`, name `zcode-demo-sub1-example` (Alpine, 1 vCPU, 233 MiB)
- **Raw verbatim log:** [`demo-transcript.txt`](demo-transcript.txt)
- **Artifact carried out of the VM:** [`vm-report.txt`](vm-report.txt)

> **PATH note:** in this checkout the broker binary is not on `PATH`, so every
> `agent-vm.sh` call below uses the script's documented override:
> `BROKER=./firecracker-broker scripts/agent-vm.sh ...`. Export `BROKER` once
> if you prefer.

## 1. Check capacity before spawning (host cap: 20 VMs)

```sh
$ ./firecracker-broker capacity
{
  "max_vms": 20,
  "existing_vms": 0,
  "running_vms": 0,
  "max_vcpus_per_vm": 4,
  "max_mem_mib_per_vm": 2048
}
```

## 2. The worker boots *its own* VM

```sh
$ BROKER=./firecracker-broker scripts/agent-vm.sh up zcode-demo-sub1 example 30m
VM 609be1ea4021ab41 (zcode-demo-sub1-example) is up for owner 'zcode-demo-sub1', ttl 30m
id:     609be1ea4021ab41
ssh:    root@192.168.0.118:2200
key:    /home/Adeel/.firecracker-broker/keys/609be1ea4021ab41.key
record: /home/Adeel/.firecracker-broker/instances/609be1ea4021ab41.env
```

## 3. First `run` waits for sshd, then looks around inside

```sh
$ BROKER=./firecracker-broker scripts/agent-vm.sh run 609be1ea4021ab41 \
    'uname -a; nproc; free -m | head -2; head -2 /etc/os-release'
waiting for sshd on zcode-demo-sub1-example (192.168.0.118:2200)...
Linux zcode-demo-sub1-example 6.1.102 #1 SMP PREEMPT_DYNAMIC ... x86_64 Linux
1
              total        used        free ...
Mem:            233          15         204 ...
NAME="Alpine Linux"
```

## 4. Real work inside the VM

One multi-step `run` built `/tmp/vm-report.txt` inside the guest: system
facts, a prime-counting awk job, and a C-compiler probe (full command in the
transcript). Nothing outside the VM was touched:

```console
vm-report (built inside the VM)
hostname: zcode-demo-sub1-example
kernel:  6.1.102
cpus:    1

task a) count primes below 5000
669 primes below 5000

task b) C compiler check
command -v cc gcc -> <none>
no C compiler available
```

## 5. Results come out before teardown (nothing survives `down`)

```sh
$ BROKER=./firecracker-broker scripts/agent-vm.sh get 609be1ea4021ab41 \
    /tmp/vm-report.txt ./examples/vm-report.txt
```

## 6. Broker-side attribution, then the worker retires its VM

```sh
$ ./firecracker-broker list --owner zcode-demo-sub1
ID                 NAME                      STATE     OWNER             AGE   IP            SSH_PORT
609be1ea4021ab41   zcode-demo-sub1-example   running   zcode-demo-sub1   4m   172.16.100.10 2200

$ BROKER=./firecracker-broker scripts/agent-vm.sh down 609be1ea4021ab41
request timed out after 5s (DELETE /v1/vms/609be1ea4021ab41); the host may still have completed it - verify with 'list' or just re-run

$ BROKER=./firecracker-broker scripts/agent-vm.sh down 609be1ea4021ab41   # idempotent re-run
note: VM 609be1ea4021ab41 is not on the host anymore; cleaning up local state
deleted 609be1ea4021ab41

$ ./firecracker-broker list
ID   NAME   STATE   OWNER   AGE   IP   SSH_PORT
```

Host confirmed empty afterwards (`capacity`: 0/20).

## Gotchas hit in this session

1. **Broker not on `PATH`** — `agent-vm.sh` defaults to `firecracker-broker`;
   from the repo root use `BROKER=./firecracker-broker` (or install/symlink
   the binary).
2. **`down` timed out after 5s** — the documented slow-teardown case: the
   first delete had already completed host-side. Re-running `down` is the
   intended recovery (idempotent), and `list` confirms the outcome.

## Reusing the pattern

Give every agent/subagent its own owner name and the same five beats:

```sh
BROKER=./firecracker-broker scripts/agent-vm.sh up  <unique-owner> <purpose> <ttl>
BROKER=./firecracker-broker scripts/agent-vm.sh run <id> '<workload>'
BROKER=./firecracker-broker scripts/agent-vm.sh get <id> /tmp/result.out ./result.out
BROKER=./firecracker-broker scripts/agent-vm.sh down <id>
```

The TTL (default 30m) is the safety net: even a dead agent's VM is reaped by
the host cron sweep — but agents should still `down` when done, as here.
