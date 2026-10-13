#!/usr/bin/env bash
# agent-vm.sh — personal microVM lifecycle for agents and subagents.
#
# Thin wrapper around firecracker-broker: spawn a VM, run tasks inside over
# SSH, copy results out, retire it. One VM per agent, identified by --owner.
# Never talks to the host API directly and never touches other owners' VMs.
#
# Requires: firecracker-broker (override with BROKER=...), ssh/scp, python3
# (for JSON parsing). Auth: MICROVMS_TOKEN env or ~/.firecracker-broker/token.
set -euo pipefail
umask 077

BROKER="${BROKER:-firecracker-broker}"
INST_DIR="$HOME/.firecracker-broker/instances"
DEFAULT_TTL="${DEFAULT_TTL:-30m}"
BOOT_WAIT_SECONDS="${BOOT_WAIT_SECONDS:-180}"

usage() {
	cat <<'EOF'
usage:
  agent-vm.sh up <owner> [purpose] [ttl]    boot a VM for this agent (default purpose "task", ttl 30m)
  agent-vm.sh run <id> [command ...]        wait for sshd, then run command inside (interactive shell if none)
  agent-vm.sh get <id> <remote> <local>     copy a file out of the VM
  agent-vm.sh down <id>                     delete the VM and forget it (safe to re-run)
  agent-vm.sh info <id>                     print the saved connection details

Auth: MICROVMS_TOKEN env var, or ~/.firecracker-broker/token (0600).
Binary: firecracker-broker on PATH, or BROKER=/path/to/firecracker-broker.
EOF
	exit 2
}

need_token() {
	if [ -z "${MICROVMS_TOKEN:-}" ] && [ -f "$HOME/.firecracker-broker/token" ]; then
		MICROVMS_TOKEN="$(cat "$HOME/.firecracker-broker/token")"
		export MICROVMS_TOKEN
	fi
	[ -n "${MICROVMS_TOKEN:-}" ] || {
		echo "error: MICROVMS_TOKEN not set and ~/.firecracker-broker/token missing" >&2
		exit 1
	}
}

inst_file() { printf '%s/%s.env' "$INST_DIR" "$1"; }

load_inst() {
	local id="${1:?}" f
	f="$(inst_file "$id")"
	[ -f "$f" ] || {
		echo "error: no record for $id (expected $f); run 'up' first" >&2
		exit 1
	}
	# shellcheck disable=SC1090
	. "$f"
}

cmd="${1:-}"
case "$cmd" in
	up | run | get | down | info) ;;
	*) usage ;;
esac
shift
mkdir -p "$INST_DIR"

case "$cmd" in
	up)
		owner="${1:-}"
		[ -n "$owner" ] || { echo "error: owner required" >&2; exit 2; }
		purpose="${2:-task}"
		ttl="${3:-$DEFAULT_TTL}"
		need_token
		out="$("$BROKER" create --owner "$owner" --purpose "$purpose" --ttl "$ttl")"
		eval "$(python3 -c '
import json, re, shlex, sys
vm = json.load(sys.stdin)
m = re.search(r"@(\S+)\s*$", vm.get("ssh_command") or "")
print("VM_ID=" + shlex.quote(vm.get("id") or ""))
print("VM_NAME=" + shlex.quote(vm.get("name") or ""))
print("SSH_HOST=" + shlex.quote(m.group(1) if m else (vm.get("ip") or "")))
print("SSH_PORT=" + shlex.quote(str(vm.get("ssh_port") or "")))
print("SSH_USER=" + shlex.quote(vm.get("ssh_user") or ""))
print("SSH_KEY=" + shlex.quote(vm.get("ssh_key_path") or ""))
' <<<"$out")"
		[ -n "$VM_ID" ] || { echo "error: create returned no id" >&2; exit 1; }
		f="$(inst_file "$VM_ID")"
		{
			printf 'VM_ID=%s\n' "$(printf '%q' "$VM_ID")"
			printf 'VM_NAME=%s\n' "$(printf '%q' "$VM_NAME")"
			printf 'SSH_HOST=%s\n' "$(printf '%q' "$SSH_HOST")"
			printf 'SSH_PORT=%s\n' "$(printf '%q' "$SSH_PORT")"
			printf 'SSH_USER=%s\n' "$(printf '%q' "$SSH_USER")"
			printf 'SSH_KEY=%s\n' "$(printf '%q' "$SSH_KEY")"
			printf 'TTL=%s\n' "$(printf '%q' "$ttl")"
		} >"$f"
		echo "VM $VM_ID ($VM_NAME) is up for owner '$owner', ttl $ttl"
		echo "id:     $VM_ID"
		echo "ssh:    ${SSH_USER:-root}@$SSH_HOST:$SSH_PORT"
		if [ -n "$SSH_KEY" ]; then
			echo "key:    $SSH_KEY"
		else
			echo "key:    (none generated; pass your own via ssh -i)"
		fi
		echo "record: $f"
		;;
	run)
		id="${1:-}"
		[ -n "$id" ] || usage
		shift || true
		load_inst "$id"
		need_token
		ssho=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o LogLevel=ERROR -o ConnectTimeout=5 -p "$SSH_PORT")
		if [ -n "$SSH_KEY" ]; then ssho+=(-i "$SSH_KEY"); fi
		echo "waiting for sshd on $VM_NAME ($SSH_HOST:$SSH_PORT)..." >&2
		up=0
		for _ in $(seq 1 $((BOOT_WAIT_SECONDS / 5))); do
			if ssh "${ssho[@]}" "$SSH_USER@$SSH_HOST" true 2>/dev/null; then up=1; break; fi
			sleep 5
		done
		[ "$up" = 1 ] || {
			echo "error: sshd did not come up within ${BOOT_WAIT_SECONDS}s" >&2
			exit 1
		}
		exec ssh "${ssho[@]}" "$SSH_USER@$SSH_HOST" "$@"
		;;
	get)
		[ $# -eq 3 ] || usage
		load_inst "$1"
		need_token
		scpo=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o LogLevel=ERROR -P "$SSH_PORT")
		if [ -n "$SSH_KEY" ]; then scpo+=(-i "$SSH_KEY"); fi
		exec scp "${scpo[@]}" "$SSH_USER@$SSH_HOST:$2" "$3"
		;;
	down)
		id="${1:-}"
		[ -n "$id" ] || usage
		need_token
		rc=0
		"$BROKER" delete --id "$id" || rc=$?
		rm -f "$(inst_file "$id")"
		exit "$rc"
		;;
	info)
		id="${1:-}"
		[ -n "$id" ] || usage
		load_inst "$1"
		echo "VM_ID=$VM_ID"
		echo "VM_NAME=$VM_NAME"
		echo "SSH_HOST=$SSH_HOST"
		echo "SSH_PORT=$SSH_PORT"
		echo "SSH_USER=$SSH_USER"
		echo "SSH_KEY=$SSH_KEY"
		;;
esac
