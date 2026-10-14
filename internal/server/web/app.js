"use strict";

// Field definitions per connection type.  The editor is generated from these.
const TYPES = {
  sftp: {
    label: "SFTP",
    fields: [
      { k: "host", label: "Server", ph: "example.com", req: true },
      { k: "port", label: "Port", ph: "22" },
      { k: "user", label: "Username", req: true },
      { k: "password", label: "Password", type: "password" },
      { k: "private_key", label: "Private key file (optional)", ph: "~/.ssh/id_ed25519" },
      { k: "passphrase", label: "Key passphrase", type: "password" },
      { k: "path", label: "Remote path", ph: "/" },
    ],
  },
  ftp: {
    label: "FTP / FTPS",
    fields: [
      { k: "host", label: "Server", req: true },
      { k: "port", label: "Port", ph: "21" },
      { k: "user", label: "Username" },
      { k: "password", label: "Password", type: "password" },
      { k: "tls", label: "Use explicit FTPS (TLS)", type: "bool" },
      { k: "path", label: "Remote path", ph: "/" },
    ],
  },
  webdav: {
    label: "WebDAV",
    fields: [
      { k: "url", label: "URL", ph: "https://example.com/dav/", req: true },
      { k: "user", label: "Username" },
      { k: "password", label: "Password", type: "password" },
      { k: "path", label: "Remote path", ph: "/" },
    ],
  },
  s3: {
    label: "Amazon S3 / compatible",
    fields: [
      { k: "endpoint", label: "Endpoint (blank for AWS)", ph: "https://s3.amazonaws.com" },
      { k: "region", label: "Region", ph: "us-east-1" },
      { k: "bucket", label: "Bucket", req: true },
      { k: "access_key", label: "Access key ID", req: true },
      { k: "secret_key", label: "Secret access key", type: "password" },
      { k: "session_token", label: "Session token (optional)", type: "password" },
      { k: "path_style", label: "Force path-style addressing", type: "bool" },
      { k: "path", label: "Prefix (optional)" },
    ],
  },
  gdrive: {
    label: "Google Drive",
    oauth: "gdrive",
    note: "Click “Sign in with Google Drive” and approve access in your browser — no client ID, secret or token needed. Google rate-limits the shared default app heavily (project 202264815644), so for reliable access paste your own OAuth client below first: console.cloud.google.com → APIs & Services → Credentials → Create credentials → OAuth client ID → Desktop app. Leave it blank to use the built-in shared client.",
    fields: [
      { k: "client_id", label: "OAuth client ID (optional)" },
      { k: "client_secret", label: "OAuth client secret (optional)", type: "password" },
      { k: "token", label: "Token JSON (optional, for scripts)", type: "password" },
      { k: "root_id", label: "Root folder ID (optional)" },
    ],
  },
  dropbox: {
    label: "Dropbox",
    oauth: "dropbox",
    note: "Click “Sign in with Dropbox” and approve access in your browser. Requires a Dropbox app key/secret on the server (QKMOUNT_DROPBOX_KEY).",
    fields: [
      { k: "client_id", label: "App key (optional)" },
      { k: "client_secret", label: "App secret (optional)", type: "password" },
      { k: "token", label: "Access / refresh token", type: "password" },
      { k: "path", label: "Folder (optional)", ph: "/" },
    ],
  },
  onedrive: {
    label: "Microsoft OneDrive",
    oauth: "onedrive",
    note: "Click “Sign in with Microsoft OneDrive” and approve access in your browser. Requires an Azure app client ID on the server (QKMOUNT_ONEDRIVE_CLIENT_ID).",
    fields: [
      { k: "client_id", label: "Application (client) ID (optional)" },
      { k: "client_secret", label: "Client secret (optional)", type: "password" },
      { k: "token", label: "Token JSON", type: "password" },
      { k: "drive_id", label: "Drive ID (optional)" },
      { k: "path", label: "Folder (optional)", ph: "/" },
    ],
  },
  b2: {
    label: "Backblaze B2",
    fields: [
      { k: "account_id", label: "Key ID", req: true },
      { k: "key", label: "Application key", type: "password", req: true },
      { k: "bucket", label: "Bucket", req: true },
      { k: "path", label: "Prefix (optional)" },
    ],
  },
  azure: {
    label: "Azure Blob Storage",
    fields: [
      { k: "account", label: "Storage account", req: true },
      { k: "key", label: "Account key", type: "password", req: true },
      { k: "container", label: "Container", req: true },
      { k: "endpoint", label: "Endpoint suffix", ph: "core.windows.net" },
      { k: "path", label: "Prefix (optional)" },
    ],
  },
  smb: {
    label: "SMB / Windows share",
    fields: [
      { k: "host", label: "Server", req: true },
      { k: "share", label: "Share", req: true },
      { k: "domain", label: "Domain / workgroup" },
      { k: "user", label: "Username" },
      { k: "password", label: "Password", type: "password" },
      { k: "path", label: "Subfolder (optional)", ph: "/" },
    ],
  },
  local: {
    label: "Local folder",
    fields: [{ k: "path", label: "Folder", ph: "/home/user/Documents", req: true }],
  },
};

let state = { conns: [], status: new Map(), selected: null, editing: null, kinds: Object.keys(TYPES) };
let oauthPoll = null;

const $ = (id) => document.getElementById(id);

function api(method, path, body) {
  return fetch(path, {
    method,
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
  }).then(async (r) => {
    const text = await r.text();
    const data = text ? JSON.parse(text) : null;
    if (!r.ok) throw new Error((data && data.error) || r.statusText);
    return data;
  });
}

function toast(msg, ok) {
  const t = $("toast");
  t.textContent = msg;
  t.style.background = ok === false ? "#b91c1c" : "#1f2733";
  t.classList.remove("hidden");
  clearTimeout(t._h);
  t._h = setTimeout(() => t.classList.add("hidden"), 3200);
}

// ---------------------------------------------------------------- rendering
async function refresh() {
  const [conns, status] = await Promise.all([
    api("GET", "/api/connections"),
    api("GET", "/api/status"),
  ]);
  state.conns = conns;
  state.status = new Map(status.map((s) => [s.id, s]));
  renderList();
  if (state.selected) renderDetail(state.selected);
}

function statusOf(id) {
  return state.status.get(id) || { status: "stopped" };
}

function renderList() {
  const ul = $("conn-list");
  ul.innerHTML = "";
  for (const c of state.conns) {
    const st = statusOf(c.id);
    const li = document.createElement("li");
    if (c.id === state.selected) li.classList.add("active");
    li.innerHTML =
      `<span class="dot ${st.status}"></span>` +
      `<span class="c-name">${escapeHtml(c.name)}<br><span class="c-type">${escapeHtml(typeLabel(c.type))}</span></span>`;
    li.onclick = () => select(c.id);
    ul.appendChild(li);
  }
  $("empty").classList.toggle("hidden", state.conns.length > 0);
  $("detail").classList.toggle("hidden", state.conns.length === 0 || !state.selected);
}

function select(id) {
  state.selected = id;
  renderList();
  renderDetail(id);
}

function renderDetail(id) {
  const c = state.conns.find((x) => x.id === id);
  if (!c) return;
  const st = statusOf(c.id);
  $("d-name").textContent = c.name;
  $("d-sub").textContent = typeLabel(c.type);
  $("d-type").textContent = typeLabel(c.type);
  $("d-mount").textContent = c.mountpoint || "—";
  $("d-auto").textContent = c.auto_mount ? "Yes" : "No";
  $("d-restarts").textContent = st.restarts || 0;

  const badge = $("d-status");
  badge.textContent = st.status || "stopped";
  badge.className = "badge " + (st.status || "stopped");

  const msg = $("d-message");
  if (st.message) {
    msg.textContent = st.message;
    msg.classList.remove("hidden");
    msg.classList.toggle("ok", st.status === "mounted");
  } else {
    msg.classList.add("hidden");
  }

  const tbl = $("d-opts");
  tbl.innerHTML = "";
  const schema = TYPES[c.type];
  const keys = Object.keys(c.options || {}).filter((k) => !k.endsWith("__set"));
  for (const k of keys) {
    const label = fieldLabel(schema, k);
    let v = c.options[k];
    if (v === "" && c.options[k + "__set"]) v = "••••••••";
    if (v === "" ) v = "—";
    const tr = document.createElement("tr");
    tr.innerHTML = `<td>${escapeHtml(label)}</td><td class="mono">${escapeHtml(v)}</td>`;
    tbl.appendChild(tr);
  }
  if (keys.length === 0) tbl.innerHTML = `<tr><td>—</td><td>—</td></tr>`;
}

function typeLabel(t) { return (TYPES[t] && TYPES[t].label) || t; }
function fieldLabel(schema, k) {
  if (schema) {
    const f = schema.fields.find((f) => f.k === k);
    if (f) return f.label;
  }
  return k.replace(/_/g, " ");
}

function escapeHtml(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, (m) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[m]));
}

// ---------------------------------------------------------------- editor
function openEditor(id) {
  state.editing = id || null;
  const c = id ? state.conns.find((x) => x.id === id) : null;
  $("m-title").textContent = c ? "Edit Connection" : "New Connection";
  $("m-name").value = c ? c.name : "";
  $("m-mountpoint").value = c ? c.mountpoint : "";
  $("m-automount").checked = c ? !!c.auto_mount : false;
  $("m-readonly").checked = c ? !!c.read_only : false;
  $("m-error").classList.add("hidden");

  const sel = $("m-type");
  sel.innerHTML = "";
  for (const k of state.kinds) {
    const o = document.createElement("option");
    o.value = k;
    o.textContent = typeLabel(k);
    sel.appendChild(o);
  }
  sel.value = c ? c.type : "sftp";
  renderFields(c);
  sel.onchange = () => renderFields(null);
  $("modal").classList.remove("hidden");
}

function renderFields(existing) {
  if (oauthPoll) { clearInterval(oauthPoll); oauthPoll = null; }
  const type = $("m-type").value;
  const schema = TYPES[type];
  const box = $("m-fields");
  box.innerHTML = "";
  if (schema && schema.note) {
    const p = document.createElement("p");
    p.className = "subtle";
    p.style.marginTop = "0";
    p.textContent = schema.note;
    box.appendChild(p);
  }
  if (schema && schema.oauth) {
    const ob = document.createElement("div");
    ob.className = "oauth-box";
    ob.innerHTML = `<button type="button" class="primary" id="oauth-start">Sign in with ${escapeHtml(schema.label)}</button>
      <div id="oauth-status" class="subtle hidden"></div>`;
    box.appendChild(ob);
    $("oauth-start").onclick = () => startOAuth(schema.oauth);
  }
  const fields = (schema && schema.fields) || [];
  for (const f of fields) {
    const wrap = document.createElement("div");
    wrap.className = "field" + (f.type === "bool" ? " checkbox" : "");
    const val = existing && existing.options ? existing.options[f.k] : "";
    if (f.type === "bool") {
      wrap.innerHTML = `<input type="checkbox" id="f-${f.k}" ${val === "1" || val === "true" ? "checked" : ""}>
        <label for="f-${f.k}">${escapeHtml(f.label)}</label>`;
    } else {
      const inputType = f.type === "password" ? "password" : "text";
      wrap.innerHTML = `<label>${escapeHtml(f.label)}${f.req ? " *" : ""}</label>
        <input type="${inputType}" id="f-${f.k}" placeholder="${escapeHtml(f.ph || "")}" value="${escapeHtml(val || "")}">`;
    }
    box.appendChild(wrap);
  }
}

function collect() {
  const type = $("m-type").value;
  const schema = TYPES[type];
  const options = {};
  for (const f of (schema && schema.fields) || []) {
    const el = $("f-" + f.k);
    if (!el) continue;
    if (f.type === "bool") options[f.k] = el.checked ? "1" : "0";
    else options[f.k] = el.value.trim();
  }
  return {
    id: state.editing || undefined,
    name: $("m-name").value.trim(),
    type,
    options,
    mountpoint: $("m-mountpoint").value.trim(),
    auto_mount: $("m-automount").checked,
    read_only: $("m-readonly").checked,
  };
}

async function save() {
  const body = collect();
  if (!body.name) { showErr("Please enter a name."); return; }
  try {
    if (state.editing) {
      await api("PUT", "/api/connections/" + state.editing, body);
      toast("Connection updated");
    } else {
      const created = await api("POST", "/api/connections", body);
      state.selected = created.id;
      toast("Connection added");
    }
    $("modal").classList.add("hidden");
    await refresh();
  } catch (e) {
    showErr(e.message);
  }
}

function showErr(msg) {
  const e = $("m-error");
  e.textContent = msg;
  e.classList.remove("hidden");
}

// Start an interactive browser sign-in for a provider and poll until it lands.
async function startOAuth(kind) {
  const btn = $("oauth-start");
  const st = $("oauth-status");
  st.classList.remove("hidden");
  st.textContent = "Opening your browser…";
  // If the user filled in their own OAuth client, use it for this sign-in so
  // the resulting tokens are not subject to the shared client's rate limits.
  const cid = ($("f-client_id") || {}).value || "";
  const csec = ($("f-client_secret") || {}).value || "";
  const name = ($("m-name") || {}).value ? $("m-name").value.trim() : "";
  try {
    await api("POST", `/api/oauth/${kind}/start`, { client_id: cid, client_secret: csec, name });
  } catch (e) {
    st.textContent = "Could not start sign-in: " + e.message;
    return;
  }
  btn.disabled = true;
  btn.textContent = "Waiting for sign-in…";
  if (oauthPoll) clearInterval(oauthPoll);
  oauthPoll = setInterval(async () => {
    let s;
    try { s = await api("GET", `/api/oauth/${kind}/status`); } catch { return; }
    if (s.auth_url && !st.dataset.url) {
      st.dataset.url = "1";
      st.innerHTML = `Waiting for you to finish in your browser. <a href="${s.auth_url}" target="_blank" rel="noopener">Open the sign-in page</a>`;
    }
    if (s.status === "done") {
      clearInterval(oauthPoll); oauthPoll = null;
      $("modal").classList.add("hidden");
      toast("Signed in — connection added");
      state.selected = s.connection_id || null;
      await refresh();
    } else if (s.status === "error") {
      clearInterval(oauthPoll); oauthPoll = null;
      st.textContent = s.message || "Sign-in failed.";
      btn.disabled = false;
      btn.textContent = `Sign in with ${TYPES[kind].label}`;
    }
  }, 1000);
}

// ---------------------------------------------------------------- actions
async function doMount() {
  try { await api("POST", "/api/connections/" + state.selected + "/mount"); toast("Mounting…"); refresh(); }
  catch (e) { toast(e.message, false); }
}
async function doUnmount() {
  try { await api("POST", "/api/connections/" + state.selected + "/unmount"); toast("Unmounted"); refresh(); }
  catch (e) { toast(e.message, false); }
}
async function doDelete() {
  const c = state.conns.find((x) => x.id === state.selected);
  if (!confirm(`Delete “${c.name}”? This does not delete any remote data.`)) return;
  try {
    await api("DELETE", "/api/connections/" + c.id);
    state.selected = null;
    toast("Deleted");
    await refresh();
  } catch (e) { toast(e.message, false); }
}
async function doOpen() {
  try { await api("POST", "/api/connections/" + state.selected + "/open"); }
  catch (e) { toast(e.message, false); }
}

// ---------------------------------------------------------------- wiring
$("btn-add").onclick = () => openEditor(null);
$("btn-add-2").onclick = () => openEditor(null);
$("m-cancel").onclick = () => $("modal").classList.add("hidden");
$("m-save").onclick = save;
$("btn-mount").onclick = doMount;
$("btn-unmount").onclick = doUnmount;
$("btn-del").onclick = doDelete;
$("btn-edit").onclick = () => openEditor(state.selected);
$("btn-open").onclick = doOpen;
$("btn-settings").onclick = async () => {
  const s = await api("GET", "/api/settings");
  const root = prompt("Mount root folder:", s.mount_root);
  if (root == null) return;
  await api("PUT", "/api/settings", { ...s, mount_root: root });
  toast("Settings saved");
};

refresh().catch((e) => toast(e.message, false));
setInterval(() => refresh().catch(() => {}), 4000);
