/* OCeanic RMM dashboard — dependency-free single page app. */
"use strict";

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));
const view = $("#view");
const state = { me: null, timer: null, orgs: [], scripts: [], selected: new Set(), deviceFilter: { q: "", org: "", status: "" } };

// ---------- utilities ----------
function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
function fmtBytes(n) {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB", "PB"]; let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (n >= 100 || i === 0 ? n.toFixed(0) : n.toFixed(1)) + " " + u[i];
}
function fmtAgo(ts) {
  if (!ts) return "never";
  const s = Math.max(0, Math.floor(Date.now() / 1000 - ts));
  if (s < 60) return s + "s ago";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago";
  return Math.floor(s / 86400) + "d ago";
}
function fmtDur(sec) {
  sec = Math.floor(sec || 0);
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
}
function fmtTime(ts) { return ts ? new Date(ts * 1000).toLocaleString() : "—"; }
function osIcon(os) { return os === "windows" ? "⊞" : os === "linux" ? "🐧" : os === "darwin" ? "" : "•"; }
function devName(d) { return d.display_name || d.hostname; }
function maxDisk(m) { return (m && m.disks || []).reduce((a, d) => Math.max(a, d.pct), 0); }
function bar(pct) {
  if (pct == null || isNaN(pct)) return '<span class="muted">—</span>';
  const cls = pct >= 90 ? "crit" : pct >= 75 ? "warn" : "";
  return `<div class="bar"><div class="track"><div class="fill ${cls}" style="width:${Math.min(100, pct)}%"></div></div><span>${Math.round(pct)}%</span></div>`;
}
function statusPill(online) { return `<span class="status ${online ? "online" : "offline"}">${online ? "Online" : "Offline"}</span>`; }
function jobBadge(s) { return `<span class="badge ${esc(s)}">${esc(s)}</span>`; }

async function api(method, path, body) {
  const opts = { method, headers: { "X-OCeanic-Request": "1" }, credentials: "same-origin" };
  if (body !== undefined) { opts.headers["Content-Type"] = "application/json"; opts.body = JSON.stringify(body); }
  const r = await fetch(path, opts);
  if (r.status === 401 && path !== "/api/login") { showLogin(); throw new Error("not logged in"); }
  const data = r.headers.get("content-type")?.includes("json") ? await r.json() : await r.text();
  if (!r.ok) throw new Error(data?.error || r.statusText);
  return data;
}

let toastT;
function toast(msg, err = false) {
  const t = $("#toast");
  t.textContent = msg; t.className = "toast" + (err ? " err" : "");
  clearTimeout(toastT); toastT = setTimeout(() => t.classList.add("hidden"), 3500);
}
function openModal(title, html) {
  $("#modal-title").textContent = title;
  $("#modal-body").innerHTML = html;
  $("#modal").classList.remove("hidden");
}
function closeModal() {
  $("#modal").classList.add("hidden");
  $("#modal-body").innerHTML = "";
  if (state.jobPoll) { clearInterval(state.jobPoll); state.jobPoll = null; }
}
async function copy(text) {
  try { await navigator.clipboard.writeText(text); toast("Copied to clipboard"); }
  catch { toast("Copy failed — select and copy manually", true); }
}
function setRefresh(fn, ms = 15000) {
  clearInterval(state.timer);
  state.timer = fn ? setInterval(() => { if (document.visibilityState === "visible" && $("#modal").classList.contains("hidden")) fn(); }, ms) : null;
}

// ---------- auth ----------
function showLogin() {
  $("#app").classList.add("hidden");
  $("#login").classList.remove("hidden");
  setRefresh(null);
}
$("#login-form").addEventListener("submit", async e => {
  e.preventDefault();
  const f = new FormData(e.target);
  $("#login-error").textContent = "";
  try {
    await api("POST", "/api/login", { username: f.get("username"), password: f.get("password") });
    e.target.reset();
    boot();
  } catch (err) { $("#login-error").textContent = err.message; }
});

async function boot() {
  try { state.me = await api("GET", "/api/me"); } catch { return; }
  $("#login").classList.add("hidden");
  $("#app").classList.remove("hidden");
  $("#whoami").textContent = state.me.username;
  route();
}

// ---------- router ----------
window.addEventListener("hashchange", route);
async function route() {
  if (!state.me) return;
  const parts = (location.hash.replace(/^#\/?/, "") || "dashboard").split("/");
  const page = parts[0];
  $$("nav a").forEach(a => a.classList.toggle("active", a.dataset.nav === page));
  setRefresh(null);
  if (state.cleanup) { state.cleanup(); state.cleanup = null; }
  state.selected.clear();
  try {
    switch (page) {
      case "devices": parts[1] ? await pageDevice(parts[1], parts[2] || "overview") : await pageDevices(); break;
      case "scripts": await pageScripts(); break;
      case "activity": await pageActivity(); break;
      case "deploy": await pageDeploy(); break;
      case "settings": await pageSettings(); break;
      default: await pageDashboard();
    }
  } catch (err) {
    if (err.message !== "not logged in") view.innerHTML = `<div class="card empty">Error: ${esc(err.message)}</div>`;
  }
}

// ---------- dashboard ----------
async function pageDashboard() {
  const d = await api("GET", "/api/dashboard");
  const os = Object.entries(d.by_os).map(([k, v]) => `${osIcon(k)} ${esc(k || "unknown")}: <b>${v}</b>`).join(" &nbsp; ") || "—";
  view.innerHTML = `
    <div class="page-head"><div><h1>Dashboard</h1><div class="sub">${os}</div></div>
      <a class="btn primary" href="#/deploy">+ Add devices</a></div>
    <div class="grid tiles">
      <div class="card tile"><div class="label">Devices</div><div class="value">${d.total}</div></div>
      <div class="card tile"><div class="label">Online</div><div class="value">${d.online} <small>/ ${d.total}</small></div></div>
      <div class="card tile"><div class="label">Offline</div><div class="value">${d.offline}</div></div>
      <div class="card tile"><div class="label">Jobs pending</div><div class="value">${d.jobs_pending}</div></div>
      <div class="card tile"><div class="label">Failed jobs (24h)</div><div class="value">${d.jobs_failed}</div></div>
    </div>
    <div class="grid cols-2" style="margin-top:16px">
      <div class="card flush"><h2>Alerts</h2>
        ${d.alerts.length ? `<div class="table-wrap"><table><tbody>${d.alerts.map(a => `
          <tr class="clickable" data-href="#/devices/${a.device_id}"><td style="width:90px"><span class="sev ${a.severity}">${a.severity === "critical" ? "●" : "▲"} ${a.severity}</span></td>
          <td><b>${esc(a.device)}</b><div class="muted small">${esc(a.message)}</div></td></tr>`).join("")}</tbody></table></div>`
        : `<div class="empty">✓ All clear — no active alerts</div>`}
      </div>
      <div class="card flush"><h2>Recent activity</h2>${jobsTable(d.recent_jobs, true)}</div>
    </div>`;
  setRefresh(pageDashboard);
}

function jobsTable(jobs, compact = false) {
  if (!jobs.length) return `<div class="empty">No jobs yet</div>`;
  return `<div class="table-wrap"><table><thead><tr><th>Job</th>${compact ? "" : "<th>Device</th>"}<th>Status</th><th>Exit</th><th>${compact ? "When" : "Created"}</th>${compact ? "" : "<th>By</th>"}</tr></thead><tbody>
    ${jobs.map(j => `<tr class="clickable" data-job="${j.id}">
      <td><b>${esc(j.name)}</b>${compact ? `<div class="muted small">${esc(j.hostname)}</div>` : ""}</td>
      ${compact ? "" : `<td>${esc(j.hostname)}</td>`}
      <td>${jobBadge(j.status)}</td><td class="num">${j.exit_code ?? "—"}</td>
      <td class="muted">${fmtAgo(j.created_at)}</td>${compact ? "" : `<td class="muted">${esc(j.created_by)}</td>`}</tr>`).join("")}
  </tbody></table></div>`;
}

// ---------- devices list ----------
async function pageDevices() {
  const [devs, orgs] = await Promise.all([api("GET", "/api/devices"), api("GET", "/api/orgs")]);
  state.orgs = orgs;
  const f = state.deviceFilter;
  view.innerHTML = `
    <div class="page-head"><div><h1>Devices</h1><div class="sub">${devs.length} managed device(s)</div></div>
      <div class="row">
        <input class="search" id="dev-q" placeholder="Search name, IP, OS…" value="${esc(f.q)}">
        <select id="dev-org"><option value="">All organizations</option>${orgs.map(o => `<option value="${o.id}" ${f.org === o.id ? "selected" : ""}>${esc(o.name)}</option>`).join("")}</select>
        <select id="dev-status"><option value="">Any status</option><option value="online" ${f.status === "online" ? "selected" : ""}>Online</option><option value="offline" ${f.status === "offline" ? "selected" : ""}>Offline</option></select>
      </div></div>
    <div class="row" style="margin-bottom:12px" id="bulk">
      <span class="muted" id="sel-count">0 selected</span>
      <button class="btn sm" data-action="bulk-script" disabled>Run script</button>
      <button class="btn sm" data-action="bulk-reboot" disabled>Reboot</button>
      <button class="btn sm" data-action="bulk-inventory" disabled>Refresh inventory</button>
      <button class="btn sm" data-action="bulk-update" disabled>Update agent</button>
    </div>
    <div class="card flush"><div class="table-wrap"><table id="dev-table"><thead><tr>
      <th class="chk"><input type="checkbox" id="sel-all" aria-label="Select all"></th><th>Device</th><th>Status</th><th>Organization</th><th>OS</th><th>CPU</th><th>Memory</th><th>Disk</th><th>IP</th><th>Last seen</th>
    </tr></thead><tbody></tbody></table></div></div>`;
  state.devs = devs;
  renderDeviceRows();
  $("#dev-q").addEventListener("input", e => { f.q = e.target.value; renderDeviceRows(); });
  $("#dev-org").addEventListener("change", e => { f.org = e.target.value; renderDeviceRows(); });
  $("#dev-status").addEventListener("change", e => { f.status = e.target.value; renderDeviceRows(); });
  $("#sel-all").addEventListener("change", e => {
    $$("#dev-table tbody input[type=checkbox]").forEach(c => { c.checked = e.target.checked; c.checked ? state.selected.add(c.value) : state.selected.delete(c.value); });
    updateBulk();
  });
  setRefresh(async () => { state.devs = await api("GET", "/api/devices"); renderDeviceRows(); });
}

function renderDeviceRows() {
  const f = state.deviceFilter, q = f.q.toLowerCase();
  const rows = state.devs.filter(d =>
    (!f.org || d.org_id === f.org) &&
    (!f.status || (f.status === "online") === d.online) &&
    (!q || [d.hostname, d.display_name, d.platform, d.remote_ip, d.metrics.local_ip, d.org_name].join(" ").toLowerCase().includes(q)));
  const tb = $("#dev-table tbody");
  if (!tb) return;
  tb.innerHTML = rows.length ? rows.map(d => `
    <tr class="clickable" data-href="#/devices/${d.id}">
      <td class="chk"><input type="checkbox" value="${d.id}" ${state.selected.has(d.id) ? "checked" : ""} aria-label="Select ${esc(devName(d))}"></td>
      <td><b>${esc(devName(d))}</b>${d.display_name ? `<div class="muted small">${esc(d.hostname)}</div>` : ""}</td>
      <td>${statusPill(d.online)}</td><td>${esc(d.org_name)}</td>
      <td>${osIcon(d.os)} ${esc(d.platform || d.os)}</td>
      <td>${d.online ? bar(d.metrics.cpu_pct) : "—"}</td><td>${d.online ? bar(d.metrics.mem_pct) : "—"}</td><td>${bar(maxDisk(d.metrics))}</td>
      <td class="mono small">${esc(d.metrics.local_ip || "")}</td><td class="muted">${fmtAgo(d.last_seen)}</td>
    </tr>`).join("") : `<tr><td colspan="10" class="empty">${state.devs.length ? "No devices match your filters" : 'No devices yet — <a href="#/deploy">deploy an agent</a>'}</td></tr>`;
  $$("input[type=checkbox]", tb).forEach(c => c.addEventListener("change", () => { c.checked ? state.selected.add(c.value) : state.selected.delete(c.value); updateBulk(); }));
  updateBulk();
}
function updateBulk() {
  const n = state.selected.size;
  const el = $("#sel-count"); if (el) el.textContent = `${n} selected`;
  $$("#bulk button").forEach(b => b.disabled = n === 0);
}

// ---------- device detail ----------
async function pageDevice(id, tab) {
  const d = await api("GET", `/api/devices/${id}`);
  const si = d.sysinfo || {};
  const m = d.metrics || {};
  const tabs = ["overview", "desktop", "terminal", "performance", "software", "services", "activity", "notes"];
  view.innerHTML = `
    <div class="page-head">
      <div><div class="muted small"><a href="#/devices">Devices</a> / ${esc(d.org_name)}</div>
        <h1>${osIcon(d.os)} ${esc(devName(d))} ${statusPill(d.online)}</h1>
        <div class="sub">${esc(d.platform)} · agent ${esc(d.agent_version)} · last seen ${fmtAgo(d.last_seen)}</div></div>
      <div class="row">
        <a class="btn primary" href="#/devices/${d.id}/desktop">Remote desktop</a>
        <a class="btn" href="#/devices/${d.id}/terminal">${d.os === "windows" ? "PowerShell" : "Shell"}</a>
        <button class="btn" data-action="dev-script" data-id="${d.id}" data-os="${esc(d.os)}">Run script</button>
        <button class="btn" data-action="dev-job" data-type="reboot" data-id="${d.id}">Reboot</button>
        <button class="btn" data-action="dev-job" data-type="shutdown" data-id="${d.id}">Shut down</button>
        <button class="btn" data-action="dev-job" data-type="inventory" data-id="${d.id}">Refresh inventory</button>
        <button class="btn" data-action="dev-job" data-type="update" data-id="${d.id}">Update agent</button>
        <button class="btn" data-action="dev-edit" data-id="${d.id}">Edit</button>
        <button class="btn danger" data-action="dev-job" data-type="uninstall" data-id="${d.id}">Uninstall agent</button>
        <button class="btn danger" data-action="dev-delete" data-id="${d.id}">Delete</button>
      </div></div>
    <div class="tabs">${tabs.map(t => `<button data-tab="${t}" class="${t === tab ? "active" : ""}">${t === "desktop" ? "Remote desktop" : t[0].toUpperCase() + t.slice(1)}${t === "software" && si.software ? ` (${si.software.length})` : ""}</button>`).join("")}</div>
    <div id="tab-body"></div>`;
  $$(".tabs button").forEach(b => b.addEventListener("click", () => { location.hash = `#/devices/${id}/${b.dataset.tab}`; }));
  const body = $("#tab-body");

  if (tab === "overview") {
    body.innerHTML = `
      <div class="grid cols-2">
        <div class="card"><h2>System</h2><dl class="kv">
          <dt>Hostname</dt><dd>${esc(si.hostname || d.hostname)}</dd>
          <dt>OS</dt><dd>${esc(si.platform || d.platform)} ${esc(si.platform_version || "")}</dd>
          <dt>Kernel</dt><dd>${esc(si.kernel_version || "—")}</dd>
          <dt>Architecture</dt><dd>${esc(si.arch || "—")}</dd>
          <dt>Uptime</dt><dd>${d.online ? fmtDur(m.uptime) : "—"}</dd>
          <dt>Logged-in users</dt><dd>${esc((m.users || si.logged_in_users || []).join(", ") || "none")}</dd>
          <dt>Local IP</dt><dd class="mono">${esc(m.local_ip || "—")}</dd>
          <dt>Public IP</dt><dd class="mono">${esc(d.remote_ip || "—")}</dd>
          <dt>Enrolled</dt><dd>${fmtTime(d.enrolled_at)}</dd>
          <dt>Inventory</dt><dd>${si.collected_at ? new Date(si.collected_at).toLocaleString() : "—"}</dd>
        </dl></div>
        <div class="card"><h2>Hardware</h2><dl class="kv">
          <dt>Manufacturer</dt><dd>${esc(si.manufacturer || "—")}</dd>
          <dt>Model</dt><dd>${esc(si.model || "—")}</dd>
          <dt>Serial</dt><dd class="mono">${esc(si.serial_number || "—")}</dd>
          <dt>CPU</dt><dd>${esc(si.cpu_model || "—")} (${si.cpu_cores || "?"} threads)</dd>
          <dt>Memory</dt><dd>${fmtBytes(si.mem_total)}</dd>
          <dt>CPU now</dt><dd>${d.online ? bar(m.cpu_pct) : "—"}</dd>
          <dt>Memory now</dt><dd>${d.online ? bar(m.mem_pct) : "—"}</dd>
        </dl></div>
        <div class="card flush"><h2>Disks</h2><div class="table-wrap"><table><thead><tr><th>Volume</th><th>FS</th><th>Used</th><th>Size</th><th style="width:35%">Usage</th></tr></thead><tbody>
          ${(m.disks || si.disks || []).map(x => `<tr><td class="mono">${esc(x.mount)}</td><td>${esc(x.fstype)}</td><td class="num">${fmtBytes(x.used)}</td><td class="num">${fmtBytes(x.total)}</td><td>${bar(x.pct)}</td></tr>`).join("") || `<tr><td colspan="5" class="empty">No data</td></tr>`}
        </tbody></table></div></div>
        <div class="card flush"><h2>Network</h2><div class="table-wrap"><table><thead><tr><th>Interface</th><th>MAC</th><th>Addresses</th></tr></thead><tbody>
          ${(si.nics || []).map(n => `<tr><td>${esc(n.name)}</td><td class="mono small">${esc(n.mac)}</td><td class="mono small">${(n.addrs || []).map(esc).join("<br>")}</td></tr>`).join("") || `<tr><td colspan="3" class="empty">No data</td></tr>`}
        </tbody></table></div></div>
      </div>
      <div class="card" style="margin-top:16px"><h2>Quick command</h2>
        <form id="quick" class="form"><div class="row">
          <select name="shell">${shellOptions(d.os)}</select>
          <input name="script" class="mono" style="flex:1;min-width:200px" placeholder="${d.os === "windows" ? "Get-Service | Where Status -eq Running" : "uptime && df -h"}" required>
          <button class="btn primary">Run</button></div></form></div>`;
    $("#quick").addEventListener("submit", async e => {
      e.preventDefault();
      const f = new FormData(e.target);
      const r = await api("POST", "/api/jobs", { device_ids: [id], type: "script", shell: f.get("shell"), script: f.get("script"), name: "Quick: " + f.get("script").slice(0, 60), timeout_sec: 120 });
      showJob(r.job_ids[0]);
    });
    setRefresh(() => pageDevice(id, "overview"), 20000);
  } else if (tab === "performance") {
    body.innerHTML = `<div class="card"><div class="page-head" style="margin-bottom:8px"><h2 style="margin:0">CPU, memory &amp; fullest disk (%)</h2>
      <select id="range"><option value="1">Last hour</option><option value="6">6 hours</option><option value="24" selected>24 hours</option><option value="168">7 days</option></select></div>
      <div id="chart" class="chart"></div></div>`;
    const load = async () => renderChart($("#chart"), await api("GET", `/api/devices/${id}/metrics?hours=${$("#range").value}`));
    $("#range").addEventListener("change", load);
    await load();
    setRefresh(load, 30000);
  } else if (tab === "software") {
    const sw = si.software || [];
    body.innerHTML = `<div class="row" style="margin-bottom:12px"><input class="search" id="sw-q" placeholder="Filter ${sw.length} packages…"></div>
      <div class="card flush"><div class="table-wrap"><table><thead><tr><th>Name</th><th>Version</th><th>Publisher</th><th>Installed</th></tr></thead><tbody id="sw-body"></tbody></table></div></div>`;
    const render = q => {
      q = (q || "").toLowerCase();
      const rows = sw.filter(s => !q || (s.name + " " + s.publisher).toLowerCase().includes(q)).slice(0, 1000);
      $("#sw-body").innerHTML = rows.map(s => `<tr><td>${esc(s.name)}</td><td class="mono small">${esc(s.version)}</td><td class="muted">${esc(s.publisher)}</td><td class="muted">${esc(s.install_date || "")}</td></tr>`).join("") || `<tr><td colspan="4" class="empty">No software inventory yet</td></tr>`;
    };
    $("#sw-q").addEventListener("input", e => render(e.target.value));
    render("");
  } else if (tab === "services") {
    const svcs = si.services || [];
    body.innerHTML = `<div class="row" style="margin-bottom:12px"><input class="search" id="svc-q" placeholder="Filter services…"></div>
      <div class="card flush"><div class="table-wrap"><table><thead><tr><th>Service</th><th>Name</th><th>Status</th><th>Startup</th><th></th></tr></thead><tbody id="svc-body"></tbody></table></div></div>`;
    const render = q => {
      q = (q || "").toLowerCase();
      $("#svc-body").innerHTML = svcs.filter(s => !q || (s.name + " " + s.display_name).toLowerCase().includes(q)).map(s => `
        <tr><td>${esc(s.display_name || s.name)}</td><td class="mono small">${esc(s.name)}</td>
        <td><span class="badge ${s.status === "running" || s.status === "active" ? "done" : s.status === "failed" ? "failed" : ""}">${esc(s.status)}</span></td><td class="muted">${esc(s.start_type)}</td>
        <td class="row"><button class="btn sm" data-action="svc" data-op="restart" data-svc="${esc(s.name)}" data-id="${id}" data-os="${esc(d.os)}">Restart</button>
        <button class="btn sm" data-action="svc" data-op="${s.status === "running" || s.status === "active" ? "stop" : "start"}" data-svc="${esc(s.name)}" data-id="${id}" data-os="${esc(d.os)}">${s.status === "running" || s.status === "active" ? "Stop" : "Start"}</button></td></tr>`).join("") || `<tr><td colspan="5" class="empty">No service inventory yet</td></tr>`;
    };
    $("#svc-q").addEventListener("input", e => render(e.target.value));
    render("");
  } else if (tab === "activity") {
    const load = async () => { body.innerHTML = `<div class="card flush">${jobsTable(await api("GET", `/api/devices/${id}/jobs`))}</div>`; };
    await load();
    setRefresh(load, 10000);
  } else if (tab === "desktop") {
    await openDesktop(body, d);
  } else if (tab === "terminal") {
    await openTerminal(body, d);
  } else if (tab === "notes") {
    body.innerHTML = `<div class="card"><form id="notes" class="form"><textarea name="notes" rows="12" placeholder="Passwords don't belong here — but warranty info, location, quirks do.">${esc(d.notes)}</textarea><div><button class="btn primary">Save notes</button></div></form></div>`;
    $("#notes").addEventListener("submit", async e => {
      e.preventDefault();
      await api("PATCH", `/api/devices/${id}`, { notes: new FormData(e.target).get("notes") });
      toast("Notes saved");
    });
  }
}

// ---------- remote desktop (JPEG tiles over WebSocket onto a canvas) ----------
async function openDesktop(body, d) {
  let q = "auto"; try { q = localStorage.getItem("oceanic-rd-quality") || "auto"; } catch {}
  body.innerHTML = `
    <div class="term-bar row">
      <button class="btn primary" id="rd-connect">Connect</button>
      <select id="rd-quality" aria-label="Quality">
        ${[["auto", "Auto quality"], ["high", "High (100%)"], ["balanced", "Balanced (75%)"], ["low", "Low bandwidth (50%)"]].map(([v, n]) => `<option value="${v}" ${v === q ? "selected" : ""}>${n}</option>`).join("")}
      </select>
      <button class="btn" id="rd-cad" title="Send Ctrl+Alt+Del" ${d.os === "windows" ? "" : "disabled"}>Ctrl+Alt+Del</button>
      <button class="btn" id="rd-paste" title="Type text on the remote machine">Paste text</button>
      <label class="row" style="flex-direction:row;gap:6px;font-weight:500"><input type="checkbox" id="rd-view"> View only</label>
      <button class="btn" id="rd-full">⤢ Full screen</button>
      <span id="rd-status" class="term-status muted">${d.online ? "Ready" : "Device is offline."}</span>
      <span id="rd-stats" class="muted small" style="margin-left:auto"></span>
    </div>
    <div id="rd-wrap" class="rd-wrap"><canvas id="rd-canvas" tabindex="0" width="16" height="9"></canvas>
      <div id="rd-hint" class="rd-hint">${d.os === "windows" || d.os === "linux" ? "Press Connect to view this device's screen." : "Remote desktop isn't available for this OS."}</div></div>`;
  const canvas = $("#rd-canvas"), ctx2d = canvas.getContext("2d"), btn = $("#rd-connect");
  const status = (t, cls = "") => { const el = $("#rd-status"); if (el) { el.textContent = t; el.className = "term-status " + cls; } };
  const hint = t => { const h = $("#rd-hint"); if (h) { h.textContent = t || ""; h.classList.toggle("hidden", !t); } };
  const qmap = { auto: { q: 60, scale: 0 }, high: { q: 75, scale: 1 }, balanced: { q: 60, scale: 0.75 }, low: { q: 45, scale: 0.5 } };
  let ws = null, ended = false, chain = Promise.resolve(), frames = 0, bytes = 0, lastStats = performance.now();
  const held = new Set();
  const send = obj => { if (ws && ws.readyState === 1 && !(obj.t !== "cfg" && obj.t !== "refresh" && obj.t !== "cad" && $("#rd-view").checked)) ws.send(JSON.stringify(obj)); };

  const drawFrame = async buf => {
    const v = new DataView(buf);
    if (v.getUint8(0) !== 1) return;
    const n = v.getUint16(1);
    let o = 3;
    const parts = [];
    for (let i = 0; i < n; i++) {
      const x = v.getUint16(o), y = v.getUint16(o + 2), len = v.getUint32(o + 8);
      parts.push({ x, y, blob: new Blob([new Uint8Array(buf, o + 12, len)], { type: "image/jpeg" }) });
      o += 12 + len;
    }
    const bmps = await Promise.all(parts.map(p => createImageBitmap(p.blob)));
    bmps.forEach((b, i) => { ctx2d.drawImage(b, parts[i].x, parts[i].y); b.close(); });
  };

  const releaseAll = () => { for (const code of held) send({ t: "ku", code }); held.clear(); };
  const disconnect = () => { releaseAll(); if (ws) { ws.onclose = null; ws.close(); ws = null; } btn.textContent = "Connect"; $("#rd-stats").textContent = ""; };
  const connect = () => {
    ended = false; frames = 0; bytes = 0;
    status("Connecting… starting the remote helper"); hint("Connecting…");
    btn.textContent = "Disconnect";
    const proto = location.protocol === "https:" ? "wss" : "ws";
    ws = new WebSocket(`${proto}://${location.host}/api/devices/${d.id}/desktop`);
    ws.binaryType = "arraybuffer";
    ws.onmessage = ev => {
      if (typeof ev.data !== "string") {
        const buf = ev.data; bytes += buf.byteLength;
        chain = chain.then(() => drawFrame(buf)).catch(() => {}).then(() => {
          frames++;
          if (ws && ws.readyState === 1) ws.send(new Uint8Array([1])); // ack -> next frame
        });
        return;
      }
      let m; try { m = JSON.parse(ev.data); } catch { return; }
      if (m.type === "ready") { status("Waiting for the screen…"); send({ t: "cfg", ...qmap[$("#rd-quality").value] }); }
      else if (m.type === "size") {
        canvas.width = m.w; canvas.height = m.h; hint("");
        status("● Connected", "ok"); canvas.focus();
      } else if (m.type === "error") { ended = true; status(m.message, "err"); hint(m.message); }
    };
    ws.onclose = () => { ws = null; btn.textContent = "Connect"; releaseAll(); if (!ended) { status("Disconnected"); } $("#rd-stats").textContent = ""; };
  };
  btn.addEventListener("click", () => ws ? (disconnect(), status("Disconnected")) : connect());
  $("#rd-quality").addEventListener("change", e => { try { localStorage.setItem("oceanic-rd-quality", e.target.value); } catch {} send({ t: "cfg", ...qmap[e.target.value] }); });
  $("#rd-cad").addEventListener("click", () => { send({ t: "cad" }); canvas.focus(); });
  $("#rd-paste").addEventListener("click", async () => {
    let text = "";
    try { text = await navigator.clipboard.readText(); } catch {}
    text = prompt("Text to type on the remote machine:", text);
    if (text) send({ t: "text", s: text });
    canvas.focus();
  });
  const full = async () => {
    const wrap = $("#rd-wrap");
    if (document.fullscreenElement) { await document.exitFullscreen(); return; }
    try { await wrap.requestFullscreen(); await navigator.keyboard?.lock?.(); } catch {}
    canvas.focus();
  };
  $("#rd-full").addEventListener("click", full);

  // --- input ---
  const pos = e => {
    const r = canvas.getBoundingClientRect();
    // The canvas keeps its aspect ratio (object-fit: contain), so map through the drawn area.
    const s = Math.min(r.width / canvas.width, r.height / canvas.height);
    const dw = canvas.width * s, dh = canvas.height * s, ox = (r.width - dw) / 2, oy = (r.height - dh) / 2;
    return { x: Math.min(1, Math.max(0, (e.clientX - r.left - ox) / dw)), y: Math.min(1, Math.max(0, (e.clientY - r.top - oy) / dh)) };
  };
  let pendingMove = null;
  canvas.addEventListener("pointermove", e => {
    if (!pendingMove) requestAnimationFrame(() => { if (pendingMove) send({ t: "mm", ...pendingMove }); pendingMove = null; });
    pendingMove = pos(e);
  });
  const btnMap = { 0: 0, 1: 1, 2: 2 };
  canvas.addEventListener("pointerdown", e => { canvas.focus(); canvas.setPointerCapture(e.pointerId); send({ t: "md", b: btnMap[e.button] ?? 0, ...pos(e) }); e.preventDefault(); });
  canvas.addEventListener("pointerup", e => { send({ t: "mu", b: btnMap[e.button] ?? 0, ...pos(e) }); e.preventDefault(); });
  canvas.addEventListener("contextmenu", e => e.preventDefault());
  let wheelAcc = 0;
  canvas.addEventListener("wheel", e => {
    e.preventDefault();
    wheelAcc += e.deltaMode === 1 ? e.deltaY * 33 : e.deltaY;
    const steps = Math.trunc(wheelAcc / 100);
    if (steps) { send({ t: "wh", dy: steps }); wheelAcc -= steps * 100; }
    if (e.deltaX && Math.abs(e.deltaX) > 50) send({ t: "wh", dx: Math.sign(e.deltaX) });
  }, { passive: false });
  canvas.addEventListener("keydown", e => {
    if (!ws) return;
    e.preventDefault(); e.stopPropagation();
    held.add(e.code); send({ t: "kd", code: e.code });
  });
  canvas.addEventListener("keyup", e => {
    if (!ws) return;
    e.preventDefault(); e.stopPropagation();
    held.delete(e.code); send({ t: "ku", code: e.code });
  });
  canvas.addEventListener("blur", releaseAll);

  const statsTimer = setInterval(() => {
    const now = performance.now(), dt = (now - lastStats) / 1000;
    if (ws && frames) $("#rd-stats").textContent = `${canvas.width}×${canvas.height} · ${(frames / dt).toFixed(1)} fps · ${(bytes * 8 / dt / 1e6).toFixed(2)} Mbit/s`;
    frames = 0; bytes = 0; lastStats = now;
  }, 2000);

  state.cleanup = () => { disconnect(); clearInterval(statsTimer); if (document.fullscreenElement) document.exitFullscreen().catch(() => {}); };
  if (d.online && (d.os === "windows" || d.os === "linux")) connect();
}

// ---------- interactive terminal (xterm.js over WebSocket) ----------
let xtermLoaded;
function loadXterm() {
  if (!xtermLoaded) {
    const css = document.createElement("link");
    css.rel = "stylesheet"; css.href = "vendor/xterm.css";
    document.head.appendChild(css);
    const js = src => new Promise((ok, fail) => { const s = document.createElement("script"); s.src = src; s.onload = ok; s.onerror = fail; document.head.appendChild(s); });
    xtermLoaded = js("vendor/xterm.js").then(() => js("vendor/addon-fit.js"));
  }
  return xtermLoaded;
}

async function openTerminal(body, d) {
  const shells = d.os === "windows" ? [["powershell", "Windows PowerShell"], ["cmd", "Command Prompt"], ["pwsh", "PowerShell 7"]] : [["bash", "bash"], ["sh", "sh"], ["pwsh", "PowerShell 7"]];
  let pref = ""; try { pref = localStorage.getItem("oceanic-shell-" + d.os) || ""; } catch {}
  body.innerHTML = `
    <div class="term-bar row">
      <select id="term-shell" aria-label="Shell">${shells.map(([v, n]) => `<option value="${v}" ${v === pref ? "selected" : ""}>${n}</option>`).join("")}</select>
      <button class="btn primary" id="term-connect">Connect</button>
      <button class="btn" id="term-full" title="Toggle full screen (Shift+Esc to exit)">⤢ Full screen</button>
      <span id="term-status" class="term-status muted">${d.online ? "Ready" : "Device is offline. It will connect if it comes back within 30s."}</span>
      <span class="muted small" style="margin-left:auto">Runs as ${d.os === "windows" ? "SYSTEM" : "root"} · closes after 30 min idle</span>
    </div>
    <div id="term-wrap" class="term-wrap"><div id="term"></div></div>`;
  await loadXterm();
  const term = new window.Terminal({
    cursorBlink: true, fontSize: 13.5, scrollback: 10000, allowProposedApi: false,
    fontFamily: 'ui-monospace, "Cascadia Mono", "Cascadia Code", Menlo, Consolas, "DejaVu Sans Mono", monospace',
    theme: { background: "#111214", foreground: "#e6e6e3", cursor: "#e6e6e3", selectionBackground: "#3987e566" },
  });
  const fit = new window.FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open($("#term"));
  fit.fit();
  term.writeln("\x1b[90mOCeanic remote terminal: " + devName(d) + ". Choose a shell and press Connect.\x1b[0m");

  const status = (t, cls = "") => { const el = $("#term-status"); if (el) { el.textContent = t; el.className = "term-status " + cls; } };
  const btn = $("#term-connect");
  const enc = new TextEncoder();
  let ws = null, ended = false;

  const sendResize = () => {
    if (ws && ws.readyState === 1) ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
  };
  const ro = new ResizeObserver(() => { try { fit.fit(); } catch {} });
  ro.observe($("#term-wrap"));
  term.onResize(sendResize);
  term.onData(data => { if (ws && ws.readyState === 1) ws.send(enc.encode(data)); });
  term.onBinary(data => { if (ws && ws.readyState === 1) ws.send(Uint8Array.from(data, c => c.charCodeAt(0))); });

  const disconnect = () => { if (ws) { ws.onclose = null; ws.close(); ws = null; } btn.textContent = "Connect"; };
  const connect = () => {
    const shell = $("#term-shell").value;
    try { localStorage.setItem("oceanic-shell-" + d.os, shell); } catch {}
    fit.fit();
    term.reset();
    ended = false;
    status("Connecting… waiting for the agent", "");
    btn.textContent = "Disconnect";
    const proto = location.protocol === "https:" ? "wss" : "ws";
    ws = new WebSocket(`${proto}://${location.host}/api/devices/${d.id}/terminal?shell=${shell}&cols=${term.cols}&rows=${term.rows}`);
    ws.binaryType = "arraybuffer";
    ws.onmessage = ev => {
      if (typeof ev.data !== "string") { term.write(new Uint8Array(ev.data)); return; }
      let m; try { m = JSON.parse(ev.data); } catch { return; }
      if (m.type === "ready") { status("● Connected", "ok"); sendResize(); term.focus(); }
      else if (m.type === "exit") { ended = true; status(`Shell exited (code ${m.code ?? 0}). Press Connect for a new session.`); term.writeln(`\r\n\x1b[90m[shell exited with code ${m.code ?? 0}]\x1b[0m`); }
      else if (m.type === "error") { ended = true; status(m.message, "err"); term.writeln(`\r\n\x1b[31m${m.message}\x1b[0m`); }
    };
    ws.onclose = () => {
      ws = null; btn.textContent = "Connect";
      if (!ended) { status("Disconnected"); term.writeln("\r\n\x1b[90m[disconnected]\x1b[0m"); }
    };
  };
  btn.addEventListener("click", () => ws ? (disconnect(), status("Disconnected")) : connect());
  $("#term-shell").addEventListener("change", () => { if (ws) { disconnect(); connect(); } });
  const full = () => { $("#term-wrap").classList.toggle("full"); setTimeout(() => { fit.fit(); term.focus(); }, 50); };
  $("#term-full").addEventListener("click", full);
  // Shift+Esc leaves full screen; swallow it so no ESC byte reaches the shell.
  term.attachCustomKeyEventHandler(e => {
    if (e.key === "Escape" && e.shiftKey) {
      if (e.type === "keydown" && $("#term-wrap")?.classList.contains("full")) full();
      return false;
    }
    return true;
  });

  state.cleanup = () => { disconnect(); ro.disconnect(); term.dispose(); };
  if (d.online) connect();
}

function shellOptions(os, selected) {
  const list = os === "windows" ? ["powershell", "cmd", "pwsh", "python"] : os ? ["bash", "sh", "python", "pwsh"] : ["powershell", "cmd", "pwsh", "bash", "sh", "python"];
  return list.map(s => `<option ${s === selected ? "selected" : ""}>${s}</option>`).join("");
}

// ---------- chart (SVG, one 0–100% axis, three series) ----------
function renderChart(el, pts) {
  if (!pts.length) { el.innerHTML = `<div class="empty">No data for this range yet</div>`; return; }
  const W = Math.max(320, el.clientWidth || 900), H = 240, L = 40, R = 96, T = 10, B = 26;
  const t0 = pts[0].t, t1 = pts[pts.length - 1].t || t0 + 1;
  const x = t => L + (t1 === t0 ? (W - L - R) / 2 : (t - t0) / (t1 - t0) * (W - L - R));
  const y = v => T + (1 - Math.min(100, Math.max(0, v)) / 100) * (H - T - B);
  const series = [["cpu", "CPU", 1], ["mem", "Memory", 2], ["disk", "Disk", 3]];
  const path = k => pts.map((p, i) => `${i ? "L" : "M"}${x(p.t).toFixed(1)},${y(p[k]).toFixed(1)}`).join("");
  const grid = [0, 25, 50, 75, 100].map(v => `<line class="gridline" x1="${L}" x2="${W - R}" y1="${y(v)}" y2="${y(v)}"/><text class="axis-label" x="${L - 6}" y="${y(v) + 4}" text-anchor="end">${v}%</text>`).join("");
  const span = t1 - t0;
  const fmtT = t => { const d = new Date(t * 1000); return span > 2 * 86400 ? d.toLocaleDateString(undefined, { weekday: "short", day: "numeric" }) : d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" }); };
  const ticks = [0, .25, .5, .75, 1].map(f => t0 + f * span).map(t => `<text class="axis-label" x="${x(t)}" y="${H - 6}" text-anchor="middle">${fmtT(t)}</text>`).join("");
  const last = pts[pts.length - 1];
  // Direct end labels, nudged apart so they never collide.
  const ends = series.map(([k, n]) => ({ n, v: last[k], y: y(last[k]) })).sort((a, b) => a.y - b.y);
  for (let i = 1; i < ends.length; i++) if (ends[i].y - ends[i - 1].y < 13) ends[i].y = ends[i - 1].y + 13;
  el.innerHTML = `
    <div class="legend">${series.map(([, n, i]) => `<span><i style="background:var(--series-${i})"></i>${n}</span>`).join("")}</div>
    <svg viewBox="0 0 ${W} ${H}" role="img" aria-label="CPU, memory and disk usage over time">
      ${grid}${ticks}
      ${series.map(([k, , i]) => `<path class="line s${i}" d="${path(k)}"/>`).join("")}
      ${pts.length < 3 ? pts.map(p => series.map(([k, , i]) => `<circle class="dot dot${i}" r="4.5" cx="${x(p.t)}" cy="${y(p[k])}"/>`).join("")).join("") : ""}
      ${ends.map(e => `<text class="end-label" x="${W - R + 6}" y="${e.y + 4}">${e.n} ${Math.round(e.v)}%</text>`).join("")}
      <g id="hover" style="display:none"><line class="cross" y1="${T}" y2="${H - B}"/>${series.map(([, , i]) => `<circle class="dot dot${i}" r="4.5"/>`).join("")}</g>
      <rect x="${L}" y="${T}" width="${W - L - R}" height="${H - T - B}" fill="transparent" id="hit"/>
    </svg><div class="tooltip hidden"></div>`;
  const svg = $("svg", el), tip = $(".tooltip", el), hov = $("#hover", el);
  $("#hit", el).addEventListener("mousemove", ev => {
    const rect = svg.getBoundingClientRect();
    const sx = (ev.clientX - rect.left) / rect.width * W;
    let best = pts[0];
    for (const p of pts) if (Math.abs(x(p.t) - sx) < Math.abs(x(best.t) - sx)) best = p;
    const cx = x(best.t);
    hov.style.display = "";
    $("line", hov).setAttribute("x1", cx); $("line", hov).setAttribute("x2", cx);
    $$("circle", hov).forEach((c, i) => { c.setAttribute("cx", cx); c.setAttribute("cy", y(best[series[i][0]])); });
    tip.innerHTML = `<div class="t">${new Date(best.t * 1000).toLocaleString()}</div>` + series.map(([k, n, i]) => `<div class="r"><span><i style="background:var(--series-${i})"></i>${n}</span><b class="num">${best[k].toFixed(1)}%</b></div>`).join("");
    tip.classList.remove("hidden");
    const px = cx / W * rect.width;
    tip.style.left = (px > rect.width - 200 ? px - 172 : px + 12) + "px";
    tip.style.top = "28px";
  });
  $("#hit", el).addEventListener("mouseleave", () => { hov.style.display = "none"; tip.classList.add("hidden"); });
}

// ---------- jobs ----------
async function showJob(id) {
  openModal("Job", `<div class="empty">Loading…</div>`);
  const render = async () => {
    const j = await api("GET", `/api/jobs/${id}`);
    $("#modal-title").textContent = j.name;
    $("#modal-body").innerHTML = `
      <div class="row" style="margin-bottom:12px">${jobBadge(j.status)} <span class="muted">${esc(j.hostname)} · ${esc(j.type)}${j.shell ? " · " + esc(j.shell) : ""} · exit ${j.exit_code ?? "—"} · created ${fmtTime(j.created_at)} by ${esc(j.created_by)}</span>
        ${j.status === "queued" ? `<button class="btn sm danger" data-action="cancel-job" data-id="${j.id}">Cancel</button>` : ""}</div>
      ${j.status === "queued" ? `<p class="muted">Waiting for the device to pick this up (offline devices run it when they reconnect)…</p>` : ""}
      ${j.status === "sent" ? `<p class="muted">Running…</p>` : ""}
      ${j.error ? `<p class="error">${esc(j.error)}</p>` : ""}
      ${j.output ? `<pre class="output">${esc(j.output)}</pre>` : j.status === "done" || j.status === "failed" ? `<p class="muted">(no output)</p>` : ""}
      ${j.script ? `<details style="margin-top:12px"><summary class="muted">Script</summary><pre class="output">${esc(j.script)}</pre></details>` : ""}`;
    if (!["queued", "sent"].includes(j.status) && state.jobPoll) { clearInterval(state.jobPoll); state.jobPoll = null; }
  };
  await render();
  clearInterval(state.jobPoll);
  state.jobPoll = setInterval(render, 1500);
}

async function runScriptDialog(deviceIds, os) {
  if (!state.scripts.length) state.scripts = await api("GET", "/api/scripts");
  const scripts = state.scripts.filter(s => !os || s.os === "any" || s.os === os);
  openModal(`Run script on ${deviceIds.length} device(s)`, `
    <form id="run" class="form">
      <label>From library<select name="script_id"><option value="">— Ad-hoc script —</option>${scripts.map(s => `<option value="${s.id}">${esc(s.name)}${s.os !== "any" ? ` [${s.os}]` : ""}</option>`).join("")}</select></label>
      <div id="adhoc" class="form">
        <div class="row"><label>Shell<select name="shell">${shellOptions(os)}</select></label><label>Timeout (seconds)<input name="timeout_sec" type="number" value="300" min="5"></label></div>
        <label>Script<textarea class="code" name="script" spellcheck="false" placeholder="Write-Output 'Hello from OCeanic'"></textarea></label>
      </div>
      <div class="row"><button class="btn primary">Run now</button><span class="muted small">Runs as SYSTEM / root.</span></div>
    </form>`);
  const form = $("#run");
  form.script_id.addEventListener("change", () => $("#adhoc").classList.toggle("hidden", !!form.script_id.value));
  form.addEventListener("submit", async e => {
    e.preventDefault();
    const f = new FormData(form);
    const body = { device_ids: deviceIds, type: "script", timeout_sec: +f.get("timeout_sec") };
    if (f.get("script_id")) body.script_id = f.get("script_id"); else { body.shell = f.get("shell"); body.script = f.get("script"); }
    try {
      const r = await api("POST", "/api/jobs", body);
      if (r.job_ids.length === 1) showJob(r.job_ids[0]);
      else { closeModal(); toast(`Queued on ${r.job_ids.length} devices`); location.hash = "#/activity"; }
    } catch (err) { toast(err.message, true); }
  });
}

const confirmText = {
  reboot: "Reboot this device now?", shutdown: "Shut down this device? You'll need physical/WoL access to power it back on.",
  uninstall: "Remove the agent from this device? It will stop reporting and you'll need to reinstall to manage it again.",
  update: "Download the server's current agent build to this device and restart the agent?", inventory: null,
};
async function queueJob(ids, type) {
  if (confirmText[type] && !confirm(ids.length > 1 ? `${type} ${ids.length} devices?` : confirmText[type])) return;
  const r = await api("POST", "/api/jobs", { device_ids: ids, type });
  toast(`${type} queued for ${r.job_ids.length} device(s)`);
  return r;
}

async function pageActivity() {
  const load = async () => {
    const jobs = await api("GET", "/api/jobs?limit=300");
    view.innerHTML = `<div class="page-head"><div><h1>Activity</h1><div class="sub">Every script and action sent to your devices</div></div></div>
      <div class="card flush">${jobsTable(jobs)}</div>`;
  };
  await load();
  setRefresh(load, 10000);
}

// ---------- scripts ----------
async function pageScripts() {
  state.scripts = await api("GET", "/api/scripts");
  view.innerHTML = `
    <div class="page-head"><div><h1>Automation library</h1><div class="sub">Reusable scripts you can run on one device or many</div></div>
      <button class="btn primary" data-action="script-new">+ New script</button></div>
    <div class="card flush"><div class="table-wrap"><table><thead><tr><th>Name</th><th>Shell</th><th>OS</th><th>Timeout</th><th></th></tr></thead><tbody>
    ${state.scripts.map(s => `<tr><td><b>${esc(s.name)}</b>${s.builtin ? ' <span class="badge">built-in</span>' : ""}<div class="muted small">${esc(s.description)}</div></td>
      <td class="mono small">${esc(s.shell)}</td><td>${esc(s.os)}</td><td class="num">${s.timeout_sec}s</td>
      <td><div class="row" style="justify-content:flex-end"><button class="btn sm" data-action="script-edit" data-id="${s.id}">Edit</button><button class="btn sm danger" data-action="script-delete" data-id="${s.id}">Delete</button></div></td></tr>`).join("") || `<tr><td colspan="5" class="empty">No scripts</td></tr>`}
    </tbody></table></div></div>`;
}

function scriptEditor(s = { name: "", description: "", shell: "powershell", os: "windows", timeout_sec: 300, body: "" }) {
  openModal(s.id ? "Edit script" : "New script", `
    <form id="script-form" class="form">
      <div class="row"><label>Name<input name="name" required value="${esc(s.name)}"></label>
        <label>Shell<select name="shell">${shellOptions("", s.shell)}</select></label>
        <label>Target OS<select name="os">${["any", "windows", "linux"].map(o => `<option ${o === s.os ? "selected" : ""}>${o}</option>`).join("")}</select></label>
        <label>Timeout (s)<input name="timeout_sec" type="number" min="5" value="${s.timeout_sec}"></label></div>
      <label>Description<input name="description" value="${esc(s.description)}"></label>
      <label>Script<textarea class="code" name="body" spellcheck="false" required>${esc(s.body)}</textarea></label>
      <div><button class="btn primary">Save</button></div>
    </form>`);
  $("#script-form").addEventListener("submit", async e => {
    e.preventDefault();
    const f = Object.fromEntries(new FormData(e.target));
    f.timeout_sec = +f.timeout_sec;
    try {
      await api(s.id ? "PUT" : "POST", s.id ? `/api/scripts/${s.id}` : "/api/scripts", f);
      closeModal(); toast("Script saved"); state.scripts = []; pageScripts();
    } catch (err) { toast(err.message, true); }
  });
}

// ---------- deploy ----------
async function pageDeploy() {
  const [orgs, info] = await Promise.all([api("GET", "/api/orgs"), api("GET", "/api/installers")]);
  const base = info.public_url;
  const label = { "windows-amd64": "Windows x64 (.exe)", "windows-arm64": "Windows ARM64 (.exe)", "linux-amd64": "Linux x64", "linux-arm64": "Linux ARM64", "linux-arm": "Linux ARMv7 (Pi)" };
  view.innerHTML = `
    <div class="page-head"><div><h1>Deploy agents</h1><div class="sub">Each organization gets its own installer and enrollment token</div></div>
      <form id="new-org" class="row"><input name="name" placeholder="New organization name" required><button class="btn">+ Add organization</button></form></div>
    ${info.public_url_set ? "" : `<div class="callout"><b>OCEANIC_PUBLIC_URL isn't set.</b> Installers will point agents at <code>${esc(base)}</code> (taken from this browser request). Set it to your Cloudflare hostname, e.g. <code>https://rmm.example.com</code>, so agents always use the tunnel.</div>`}
    ${info.platforms?.length ? "" : `<div class="callout"><b>No agent binaries found.</b> They're built into the Docker image; when running from source, run <code>make agents</code> first.</div>`}
    <div class="grid">${orgs.map(o => {
      const ps = `irm ${base}/install/${o.enroll_token}/windows.ps1 | iex`;
      const sh = `curl -fsSL ${base}/install/${o.enroll_token}/linux.sh | sudo sh`;
      return `<div class="card">
        <div class="page-head" style="margin-bottom:12px"><div><h2 style="margin:0">${esc(o.name)}</h2><div class="muted small">${o.devices} device(s) · ${o.online} online</div></div>
          <div class="row"><button class="btn sm" data-action="org-rename" data-id="${o.id}" data-name="${esc(o.name)}">Rename</button>
          <button class="btn sm" data-action="org-rotate" data-id="${o.id}">Rotate token</button>
          <button class="btn sm danger" data-action="org-delete" data-id="${o.id}">Delete</button></div></div>
        <div class="form">
          <div><div class="muted small" style="margin-bottom:6px">Download installer (double-click on Windows, or <code>Setup.exe /S</code> for silent / GPO / Intune)</div>
            <div class="row">${(info.platforms || []).map(p => `<a class="btn ${p === "windows-amd64" ? "primary" : ""}" href="/api/orgs/${o.id}/installer/${p}" download>⬇ ${label[p] || p}</a>`).join("")}</div></div>
          <label>Windows one-liner (elevated PowerShell)<div class="copy-box"><code>${esc(ps)}</code><button type="button" class="btn sm" data-action="copy" data-text="${esc(ps)}">Copy</button></div></label>
          <label>Linux one-liner<div class="copy-box"><code>${esc(sh)}</code><button type="button" class="btn sm" data-action="copy" data-text="${esc(sh)}">Copy</button></div></label>
        </div></div>`;
    }).join("")}</div>`;
  $("#new-org").addEventListener("submit", async e => {
    e.preventDefault();
    try { await api("POST", "/api/orgs", { name: new FormData(e.target).get("name") }); pageDeploy(); } catch (err) { toast(err.message, true); }
  });
}

// ---------- settings ----------
async function pageSettings() {
  const audit = await api("GET", "/api/audit");
  view.innerHTML = `
    <div class="page-head"><div><h1>Settings</h1></div></div>
    <div class="grid cols-2">
      <div class="card"><h2>Change password</h2><form id="pw" class="form">
        <label>Current password<input type="password" name="current" required autocomplete="current-password"></label>
        <label>New password (10+ characters)<input type="password" name="new" required minlength="10" autocomplete="new-password"></label>
        <div><button class="btn primary">Update password</button></div>
        <p class="muted small">If OCEANIC_ADMIN_PASSWORD is set in your environment, it resets the password on every restart.</p></form></div>
      <div class="card"><h2>Appearance</h2><div class="row">
        <button class="btn" data-action="theme" data-theme="">System</button><button class="btn" data-action="theme" data-theme="light">Light</button><button class="btn" data-action="theme" data-theme="dark">Dark</button></div></div>
    </div>
    <div class="card flush" style="margin-top:16px"><h2>Audit log</h2><div class="table-wrap"><table><thead><tr><th>When</th><th>Actor</th><th>Action</th><th>Target</th><th>Detail</th></tr></thead><tbody>
      ${audit.map(a => `<tr><td class="muted small">${fmtTime(a.ts)}</td><td>${esc(a.actor)}</td><td class="mono small">${esc(a.action)}</td><td class="mono small">${esc(a.target)}</td><td class="muted small">${esc(a.detail)}</td></tr>`).join("")}
    </tbody></table></div></div>`;
  $("#pw").addEventListener("submit", async e => {
    e.preventDefault();
    try { await api("POST", "/api/password", Object.fromEntries(new FormData(e.target))); e.target.reset(); toast("Password updated"); }
    catch (err) { toast(err.message, true); }
  });
}

function applyTheme() {
  let t = ""; try { t = localStorage.getItem("oceanic-theme") || ""; } catch {}
  if (t) document.documentElement.dataset.theme = t; else delete document.documentElement.dataset.theme;
}
applyTheme();

// ---------- global click delegation (CSP forbids inline handlers) ----------
const svcCmd = (os, op, name) => os === "windows"
  ? { shell: "powershell", script: `${{ restart: "Restart-Service", stop: "Stop-Service", start: "Start-Service" }[op]} -Name '${name.replace(/'/g, "''")}' -Force -PassThru | Format-List Name,Status` }
  : { shell: "sh", script: `systemctl ${op} '${name.replace(/'/g, "")}' && systemctl status --no-pager '${name.replace(/'/g, "")}' | head -n 5` };

document.addEventListener("click", async e => {
  const a = e.target.closest("[data-action]");
  if (!a) {
    if (e.target.closest("input, button, a, select, label")) return;
    const job = e.target.closest("[data-job]"); if (job) return showJob(job.dataset.job);
    const row = e.target.closest("[data-href]"); if (row) location.hash = row.dataset.href;
    if (e.target.id === "modal") closeModal();
    return;
  }
  const d = a.dataset, sel = [...state.selected];
  try {
    switch (d.action) {
      case "logout": await api("POST", "/api/logout"); state.me = null; showLogin(); break;
      case "close-modal": closeModal(); break;
      case "copy": copy(d.text); break;
      case "theme": try { d.theme ? localStorage.setItem("oceanic-theme", d.theme) : localStorage.removeItem("oceanic-theme"); } catch {} applyTheme(); break;
      case "bulk-script": runScriptDialog(sel, commonOS(sel)); break;
      case "bulk-reboot": await queueJob(sel, "reboot"); break;
      case "bulk-inventory": await queueJob(sel, "inventory"); break;
      case "bulk-update": await queueJob(sel, "update"); break;
      case "dev-script": runScriptDialog([d.id], d.os); break;
      case "dev-job": { const r = await queueJob([d.id], d.type); if (r && d.type !== "inventory") showJob(r.job_ids[0]); break; }
      case "dev-edit": {
        const dev = await api("GET", `/api/devices/${d.id}`);
        const orgs = await api("GET", "/api/orgs");
        openModal("Edit device", `<form id="dev-form" class="form">
          <label>Display name<input name="display_name" value="${esc(dev.display_name)}" placeholder="${esc(dev.hostname)}"></label>
          <label>Organization<select name="org_id">${orgs.map(o => `<option value="${o.id}" ${o.id === dev.org_id ? "selected" : ""}>${esc(o.name)}</option>`).join("")}</select></label>
          <div><button class="btn primary">Save</button></div></form>`);
        $("#dev-form").addEventListener("submit", async ev => {
          ev.preventDefault();
          await api("PATCH", `/api/devices/${d.id}`, Object.fromEntries(new FormData(ev.target)));
          closeModal(); route();
        });
        break;
      }
      case "dev-delete":
        if (confirm("Delete this device from OCeanic? (This does not uninstall the agent — use 'Uninstall agent' first if it's still online.)")) {
          await api("DELETE", `/api/devices/${d.id}`); location.hash = "#/devices";
        }
        break;
      case "svc": {
        const c = svcCmd(d.os, d.op, d.svc);
        const r = await api("POST", "/api/jobs", { device_ids: [d.id], type: "script", name: `${d.op} service ${d.svc}`, timeout_sec: 120, ...c });
        showJob(r.job_ids[0]); break;
      }
      case "cancel-job": await api("POST", `/api/jobs/${d.id}/cancel`); toast("Job cancelled"); break;
      case "script-new": scriptEditor(); break;
      case "script-edit": scriptEditor(state.scripts.find(s => s.id === d.id)); break;
      case "script-delete": if (confirm("Delete this script?")) { await api("DELETE", `/api/scripts/${d.id}`); pageScripts(); } break;
      case "org-rotate":
        if (confirm("Rotate the enrollment token? Previously downloaded installers and one-liners stop working. Already-installed agents are unaffected.")) {
          await api("POST", `/api/orgs/${d.id}/rotate-token`); pageDeploy();
        }
        break;
      case "org-rename": { const n = prompt("New name", d.name); if (n) { await api("PATCH", `/api/orgs/${d.id}`, { name: n }); pageDeploy(); } break; }
      case "org-delete": if (confirm("Delete this organization?")) { await api("DELETE", `/api/orgs/${d.id}`); pageDeploy(); } break;
    }
  } catch (err) { toast(err.message, true); }
});
document.addEventListener("keydown", e => { if (e.key === "Escape" && !e.target.closest?.(".xterm")) closeModal(); });

function commonOS(ids) {
  const oses = new Set((state.devs || []).filter(d => ids.includes(d.id)).map(d => d.os));
  return oses.size === 1 ? [...oses][0] : "";
}

boot().catch(() => showLogin());
