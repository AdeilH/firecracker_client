"use strict";

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s).replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
  return data;
}

function banner(text) {
  const el = $("banner");
  el.textContent = text;
  el.classList.toggle("hidden", !text);
}

// --- status / vm table ---

let lastVms = [];

async function refresh() {
  let st;
  try {
    st = await api("/api/status");
    banner(st.broker_error ? "broker: " + st.broker_error : "");
  } catch (e) {
    banner("console unreachable: " + e.message);
    return;
  }

  const chips = [];
  if (st.capacity) {
    chips.push(`<span class="chip">VMs <b>${st.capacity.existing_vms}</b>/${st.capacity.max_vms}</span>`);
    chips.push(`<span class="chip">running <b>${st.capacity.running_vms}</b></span>`);
    chips.push(`<span class="chip">free cpus <b>${st.capacity.free_host_cpus}</b></span>`);
    chips.push(`<span class="chip">free mem <b>${st.capacity.free_host_mem_mib}</b> MiB</span>`);
  } else {
    chips.push(`<span class="chip">capacity unavailable</span>`);
  }
  $("stats").innerHTML = chips.join("");

  const badge = $("model-badge");
  badge.textContent = st.settings.agent_ready
    ? `agent: ${st.settings.model}`
    : "agent: set OPENAI_API_KEY or OPENAI_BASE_URL";
  badge.classList.toggle("off", !st.settings.agent_ready);
  badge.title = st.settings.broker_url;

  lastVms = st.vms || [];
  renderVms(lastVms);
}

function stateBadge(state) {
  const known = ["running", "stopped", "missing", "unknown", "suspended"];
  const cls = known.includes(state) ? state : "default";
  return `<span class="state ${cls}">${esc(state)}</span>`;
}

function renderVms(vms) {
  const tbody = $("vmrows");
  if (!vms.length) {
    tbody.innerHTML = `<tr><td colspan="8" class="empty">no VMs — boot one above or ask the agent</td></tr>`;
    return;
  }
  tbody.innerHTML = vms.map((v) => {
    const mine = (v.owner || "").startsWith("ui-") || (!v.tracked && (v.name || "").startsWith("ui-"));
    const power = v.state === "running"
      ? `<button data-act="stop" data-id="${esc(v.id)}">stop</button>`
      : (v.state === "stopped" ? `<button data-act="start" data-id="${esc(v.id)}">start</button>` : "");
    const ssh = v.ssh_host && v.state !== "missing"
      ? `<button data-act="ssh" data-id="${esc(v.id)}">ssh</button>`
      : "";
    const del = mine || v.state === "missing"
      ? `<button class="danger" data-act="delete" data-id="${esc(v.id)}">delete</button>`
      : "";
    const size = v.vcpus || v.mem_mib ? `${v.vcpus || "?"}c/${v.mem_mib || "?"}M` : "—";
    return `<tr>
      <td title="${esc(v.id)}">${esc(v.name || v.id)}${v.tracked ? "" : ' <span class="mono-dim" title="not in this machine\'s state.json">(untracked)</span>'}</td>
      <td>${esc(v.owner || "—")}</td>
      <td>${stateBadge(v.state)}</td>
      <td class="mono-dim">${esc(v.age || "—")}</td>
      <td class="mono-dim">${esc(v.ttl || "—")}</td>
      <td class="mono-dim">${esc(v.ip || "—")}${v.ssh_port ? ":" + v.ssh_port : ""}</td>
      <td class="mono-dim">${size}</td>
      <td><div class="cell-actions">${ssh}${power}${del}</div></td>
    </tr>`;
  }).join("");
}

// --- ssh command box ---

function sshCommand(v) {
  const parts = ["ssh"];
  if (v.ssh_key) parts.push(`-i ${v.ssh_key}`);
  parts.push("-o StrictHostKeyChecking=no", "-o UserKnownHostsFile=/dev/null");
  if (v.ssh_port) parts.push(`-p ${v.ssh_port}`);
  parts.push(`${v.ssh_user || "root"}@${v.ssh_host}`);
  return parts.join(" ");
}

function showSshModal(vm) {
  $("ssh-title").textContent = `ssh into ${vm.name || vm.id}`;
  $("ssh-cmd").textContent = sshCommand(vm);
  $("ssh-note").textContent = vm.ssh_key
    ? "Private key: " + vm.ssh_key + " (created with the VM, removed on delete)"
    : "No key on this machine for this VM (created elsewhere) — you will need your own key.";
  $("sshmodal").classList.remove("hidden");
}

$("vmrows").addEventListener("click", async (ev) => {
  const btn = ev.target.closest("button[data-act]");
  if (!btn) return;
  const { act, id } = btn.dataset;
  const vm = lastVms.find((v) => v.id === id);
  if (act === "ssh") {
    if (vm) showSshModal(vm);
    return;
  }
  if (act === "delete" && !confirm(`Delete ${vm ? vm.name : id} (${id})? The disk is wiped.`)) return;
  btn.disabled = true;
  try {
    if (act === "delete") await api(`/api/vms/${id}`, { method: "DELETE" });
    else await api(`/api/vms/${id}/${act}`, { method: "POST" });
  } catch (e) {
    banner(`${act} ${id}: ${e.message}`);
  }
  refresh();
});

$("newvm").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  const body = {
    owner: f.owner.value, purpose: f.purpose.value, ttl: f.ttl.value,
    cpus: Number(f.cpus.value) || 0, mem_mib: Number(f.mem_mib.value) || 0,
  };
  try {
    const vm = await api("/api/vms", { method: "POST", body });
    f.purpose.value = "";
    banner("");
    addChatMessage("assistant", `Booted ${vm.name} (id ${vm.id}, owner ${vm.owner}, ttl ${vm.ttl}). SSH: ${vm.ssh_command || vm.ip + ":" + vm.ssh_port}`);
  } catch (e) {
    banner("create: " + e.message);
  }
  refresh();
});

$("ssh-close").addEventListener("click", () => $("sshmodal").classList.add("hidden"));
$("sshmodal").addEventListener("click", (ev) => {
  if (ev.target === $("sshmodal")) $("sshmodal").classList.add("hidden");
});

// copyText never hangs: the async clipboard API is raced against a timeout,
// with an execCommand fallback and, as a last resort, text selection so the
// user can press Ctrl+C manually.
function copyText(text) {
  const clipboard = (navigator.clipboard && navigator.clipboard.writeText)
    ? navigator.clipboard.writeText(text)
    : Promise.reject(new Error("no clipboard API"));
  return Promise.race([
    clipboard,
    new Promise((_, reject) => setTimeout(() => reject(new Error("clipboard timeout")), 800)),
  ]).catch(() => new Promise((resolve, reject) => {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand("copy");
    ta.remove();
    if (ok) {
      resolve();
    } else {
      const range = document.createRange();
      range.selectNodeContents($("ssh-cmd"));
      const sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      reject(new Error("selected for manual copy"));
    }
  }));
}

$("ssh-copy").addEventListener("click", async () => {
  const btn = $("ssh-copy");
  try {
    await copyText($("ssh-cmd").textContent);
    btn.textContent = "Copied ✓";
  } catch {
    btn.textContent = "Press Ctrl+C";
  }
  setTimeout(() => { btn.textContent = "Copy command"; }, 1600);
});

$("refresh").addEventListener("click", refresh);
$("sweep").addEventListener("click", async () => {
  try {
    const r = await api("/api/sweep", { method: "POST", body: {} });
    banner(`sweep: deleted ${r.deleted?.length || 0}, forgot ${r.forgot?.length || 0}, failed ${r.failed?.length || 0}` +
      (r.failed?.length ? " — " + r.failed.join("; ") : ""));
  } catch (e) {
    banner("sweep: " + e.message);
  }
  refresh();
});

// --- agent chat ---

function addChatMessage(role, text) {
  const div = document.createElement("div");
  div.className = `msg ${role}`;
  div.textContent = text;
  $("chat").appendChild(div);
  scrollChat();
  return div;
}

function addActions(actions) {
  if (!actions?.length) return;
  const wrap = document.createElement("div");
  wrap.className = "actions";
  wrap.innerHTML = actions.map((a) =>
    `<div class="action ${a.ok ? "ok" : "fail"}" title="${esc(JSON.stringify(a.args || {}))}"><b>${esc(a.name)}</b> ${esc(a.summary)}</div>`
  ).join("");
  $("chat").appendChild(wrap);
  scrollChat();
}

function scrollChat() { $("chat").scrollTop = $("chat").scrollHeight; }

const history = [];

$("chatform").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const input = $("chatinput");
  const text = input.value.trim();
  if (!text) return;
  input.value = "";
  $("sendbtn").disabled = true;
  addChatMessage("user", text);
  const thinking = addChatMessage("assistant", "…");
  try {
    const res = await api("/api/chat", { method: "POST", body: { messages: [...history, { role: "user", content: text }] } });
    thinking.remove();
    addActions(res.actions);
    addChatMessage("assistant", res.reply);
    history.push({ role: "user", content: text }, { role: "assistant", content: res.reply });
    refresh();
  } catch (e) {
    thinking.textContent = "error: " + e.message;
  }
  $("sendbtn").disabled = false;
  input.focus();
});

refresh();
setInterval(refresh, 10000);
