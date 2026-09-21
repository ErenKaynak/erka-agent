"use strict";

// erka remote client. All server data is rendered through text nodes; this
// file never assigns HTML strings.

const TOKEN_KEY = "erka.token";
const TERMINAL = new Set(["completed", "failed", "cancelled"]);
const view = document.getElementById("view");
let timers = [];

function storedToken() {
  try { return localStorage.getItem(TOKEN_KEY) || ""; } catch { return ""; }
}
function saveToken(value) {
  try { value ? localStorage.setItem(TOKEN_KEY, value) : localStorage.removeItem(TOKEN_KEY); } catch { /* private mode */ }
}

function h(tag, attributes, ...children) {
  const element = document.createElement(tag);
  for (const [key, value] of Object.entries(attributes || {})) {
    if (value === undefined || value === null || value === false) continue;
    if (key.startsWith("on")) element.addEventListener(key.slice(2), value);
    else if (key === "class") element.className = value;
    else element.setAttribute(key, value === true ? "" : String(value));
  }
  for (const child of children.flat()) {
    if (child === undefined || child === null || child === false) continue;
    element.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return element;
}

function toast(message) {
  const element = document.getElementById("toast");
  element.textContent = message;
  element.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { element.hidden = true; }, 3500);
}

async function api(method, path, body) {
  const token = storedToken();
  if (!token) throw Object.assign(new Error("Add an API token in Settings."), { status: 401 });
  const response = await fetch(path, {
    method,
    headers: Object.assign({ Authorization: "Bearer " + token }, body === undefined ? {} : { "Content-Type": "application/json" }),
    body: body === undefined ? undefined : JSON.stringify(body),
    cache: "no-store",
  });
  const text = await response.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch { data = {}; }
  if (!response.ok) {
    const message = (data.error && data.error.message) || response.statusText || "Request failed";
    throw Object.assign(new Error(message), { status: response.status });
  }
  return data;
}

function every(milliseconds, run) {
  run();
  timers.push(setInterval(() => { if (!document.hidden) run(); }, milliseconds));
}

function relative(value) {
  const seconds = Math.round((Date.now() - new Date(value).getTime()) / 1000);
  if (!Number.isFinite(seconds)) return "";
  if (seconds < 60) return "just now";
  if (seconds < 3600) return Math.floor(seconds / 60) + " min ago";
  if (seconds < 86400) return Math.floor(seconds / 3600) + " h ago";
  return new Date(value).toLocaleDateString();
}

function badge(status) {
  return h("span", { class: "badge " + status }, status);
}

function errorCard(error) {
  const card = h("div", { class: "card attention" }, h("p", {}, error.message));
  if (error.status === 401) card.append(h("a", { class: "button", href: "#/settings" }, "Open settings"));
  return card;
}

// ---------- approvals ----------

async function decide(approval, decision, extra) {
  await api("POST", "/v1/approvals/" + encodeURIComponent(approval.id) + "/decision", Object.assign({ decision }, extra || {}));
  toast(decision === "approved" ? "Sent. The task resumes shortly." : "Rejected.");
}

function approvalCard(approval, refresh, showTask) {
  const busy = (button, run) => async () => {
    button.disabled = true;
    try { await run(); refresh(); } catch (error) { toast(error.message); button.disabled = false; }
  };
  const actions = h("div", { class: "row" });
  const card = h("div", { class: "card attention" },
    h("div", { class: "row spread" }, h("strong", {}, labelFor(approval.kind)), h("span", { class: "muted" }, relative(approval.requestedAt))),
    h("p", { class: "message" }, approval.message),
    actions);
  if (showTask) card.insertBefore(h("a", { class: "muted", href: "#/tasks/" + encodeURIComponent(approval.taskId) }, "Open task"), actions);

  if (approval.kind === "question") {
    for (const option of approval.options || []) {
      const button = h("button", {}, option);
      button.addEventListener("click", busy(button, () => decide(approval, "approved", { response: option })));
      actions.append(button);
    }
    const input = h("textarea", { placeholder: "Your answer", rows: 3 });
    const send = h("button", { class: "primary" }, "Answer");
    send.addEventListener("click", busy(send, async () => {
      if (!input.value.trim()) throw new Error("Type an answer first.");
      await decide(approval, "approved", { response: input.value.trim() });
    }));
    const skip = h("button", {}, "Let it decide");
    skip.addEventListener("click", busy(skip, () => decide(approval, "rejected")));
    card.append(h("div", { class: "stack" }, input, h("div", { class: "row" }, send, skip)));
  } else if (approval.kind === "permission") {
    const allow = h("button", { class: "primary" }, "Allow");
    allow.addEventListener("click", busy(allow, () => decide(approval, "approved")));
    const always = h("button", {}, "Allow for this task");
    always.addEventListener("click", busy(always, () => decide(approval, "approved", { scope: "task" })));
    const deny = h("button", { class: "danger" }, "Deny");
    deny.addEventListener("click", busy(deny, () => decide(approval, "rejected")));
    actions.append(allow, always, deny);
  } else {
    const accept = h("button", { class: "primary" }, "Accept changes");
    accept.addEventListener("click", busy(accept, () => decide(approval, "approved")));
    const reject = h("button", { class: "danger" }, "Reject");
    reject.addEventListener("click", busy(reject, () => decide(approval, "rejected")));
    actions.append(accept, reject);
    if (!showTask) actions.append(h("span", { class: "muted" }, "Review the Diff tab first."));
  }
  return card;
}

function labelFor(kind) {
  return { question: "Question", permission: "Permission request", accept_changes: "Review changes" }[kind] || kind;
}

// ---------- screens ----------

function inboxScreen() {
  const approvals = h("div");
  const tasks = h("div");
  view.replaceChildren(h("h2", {}, "Needs you"), approvals, h("h2", {}, "Tasks"), tasks);
  const refresh = async () => {
    try {
      const [pending, list] = await Promise.all([api("GET", "/v1/approvals?status=pending"), api("GET", "/v1/tasks")]);
      approvals.replaceChildren(...(pending.approvals.length ? pending.approvals.map((a) => approvalCard(a, refresh, true)) : [h("p", { class: "empty" }, "Nothing is waiting for you.")]));
      const sorted = list.tasks.slice().sort((a, b) => new Date(b.updatedAt) - new Date(a.updatedAt));
      tasks.replaceChildren(...(sorted.length ? sorted.map(taskRow) : [h("p", { class: "empty" }, "No tasks yet.")]));
    } catch (error) {
      approvals.replaceChildren(errorCard(error));
    }
  };
  every(5000, refresh);
}

function taskRow(task) {
  return h("a", { class: "card link", href: "#/tasks/" + encodeURIComponent(task.id) },
    h("div", { class: "row spread" }, badge(task.status), h("span", { class: "muted" }, relative(task.updatedAt))),
    h("p", { class: "objective" }, task.objective || task.id));
}

function newTaskScreen() {
  const select = h("select", { id: "project" });
  const objective = h("textarea", { id: "objective", placeholder: "What should erka do?", rows: 6 });
  const submit = h("button", { class: "primary" }, "Start task");
  view.replaceChildren(h("h1", {}, "New task"), h("label", { for: "project" }, "Project"), select, h("label", { for: "objective" }, "Request"), objective, h("div", { class: "row gap-top" }, submit));
  api("GET", "/v1/projects").then((data) => {
    select.replaceChildren(...data.projects.map((project) => h("option", { value: project.id }, project.name)));
    if (!data.projects.length) view.prepend(h("div", { class: "card attention" }, "Register a project with erka project commands first."));
  }).catch((error) => view.prepend(errorCard(error)));
  submit.addEventListener("click", async () => {
    if (!select.value || !objective.value.trim()) return toast("Choose a project and describe the task.");
    submit.disabled = true;
    try {
      const task = await api("POST", "/v1/tasks", { projectId: select.value, objective: objective.value.trim() });
      location.hash = "#/tasks/" + encodeURIComponent(task.id);
    } catch (error) { toast(error.message); submit.disabled = false; }
  });
}

function taskScreen(taskID) {
  const header = h("div");
  const pending = h("div");
  const body = h("div");
  const tabs = h("div", { class: "tabs", role: "tablist" });
  let tab = "activity";
  let items = [];
  let lastSequence = 0;
  let task = null;
  view.replaceChildren(header, pending, tabs, body);

  const tabButton = (name, label) => {
    const button = h("button", { role: "tab" }, label);
    button.addEventListener("click", () => { tab = name; renderTabs(); renderBody(); });
    return button;
  };
  const buttons = { activity: tabButton("activity", "Activity"), diff: tabButton("diff", "Diff"), previews: tabButton("previews", "Previews") };
  tabs.append(...Object.values(buttons));
  const renderTabs = () => { for (const [name, button] of Object.entries(buttons)) button.classList.toggle("active", name === tab); };
  renderTabs();

  const renderHeader = () => {
    if (!task) return;
    const controls = h("div", { class: "row" });
    if (!TERMINAL.has(task.status)) {
      const cancel = h("button", { class: "danger" }, "Cancel");
      cancel.addEventListener("click", async () => { try { await api("POST", "/v1/tasks/" + encodeURIComponent(taskID) + "/cancel"); toast("Cancelling."); } catch (error) { toast(error.message); } });
      controls.append(cancel);
    }
    if (task.status === "failed") {
      const retry = h("button", {}, "Retry");
      retry.addEventListener("click", async () => { try { await api("POST", "/v1/tasks/" + encodeURIComponent(taskID) + "/retry"); toast("Queued again."); } catch (error) { toast(error.message); } });
      controls.append(retry);
    }
    header.replaceChildren(h("div", { class: "row spread" }, badge(task.outcome || task.status), h("span", { class: "muted" }, task.runtimeStatus || "")), h("h1", { class: "objective" }, task.objective || taskID), controls);
    if (task.outcome) {
      header.append(outcomeCard(task));
      return;
    }
    if (task.status === "completed" || task.status === "failed") {
      const prompt = h("textarea", { placeholder: "Follow-up request", rows: 3 });
      const send = h("button", { class: "primary" }, "Send follow-up");
      send.addEventListener("click", async () => {
        if (!prompt.value.trim()) return;
        send.disabled = true;
        try { await api("POST", "/v1/tasks/" + encodeURIComponent(taskID) + "/messages", { prompt: prompt.value.trim() }); prompt.value = ""; toast("Queued."); }
        catch (error) { toast(error.message); }
        send.disabled = false;
      });
      header.append(h("div", { class: "stack gap-top" }, prompt, send));
    }
  };

  const renderBody = async () => {
    if (tab === "activity") return body.replaceChildren(activity(items));
    if (tab === "diff") {
      body.replaceChildren(h("p", { class: "muted" }, "Loading diff…"));
      try { const data = await api("GET", "/v1/tasks/" + encodeURIComponent(taskID) + "/diff"); if (tab === "diff") body.replaceChildren(diffView(data.diff)); }
      catch (error) { body.replaceChildren(errorCard(error)); }
      return;
    }
    body.replaceChildren(await previewsView(taskID));
  };

  const refresh = async () => {
    try {
      const [loaded, approvals, fresh] = await Promise.all([
        api("GET", "/v1/tasks/" + encodeURIComponent(taskID)),
        api("GET", "/v1/approvals?status=pending&taskId=" + encodeURIComponent(taskID)),
        api("GET", "/v1/tasks/" + encodeURIComponent(taskID) + "/items?after=" + lastSequence),
      ]);
      const changed = !task || loaded.status !== task.status || loaded.runtimeStatus !== task.runtimeStatus || loaded.outcome !== task.outcome;
      task = loaded;
      if (changed) renderHeader();
      pending.replaceChildren(...approvals.approvals.map((approval) => approvalCard(approval, refresh, false)));
      if (fresh.items.length) {
        items = items.concat(fresh.items);
        lastSequence = fresh.items[fresh.items.length - 1].sequence;
        if (tab === "activity") renderBody();
      }
    } catch (error) {
      header.replaceChildren(errorCard(error));
    }
  };
  renderBody();
  every(2500, refresh);
}

function outcomeCard(task) {
  if (task.resultBranch) {
    return h("div", { class: "card" },
      h("strong", {}, "Accepted"),
      h("p", { class: "message" }, "The changes are committed on branch " + task.resultBranch + ". On the server's repository:"),
      h("pre", {}, "git switch " + task.resultBranch));
  }
  if (task.outcome === "rejected" && task.resultRef) {
    return h("div", { class: "card" },
      h("strong", {}, "Rejected"),
      h("p", { class: "message" }, "The worktree was removed. Its changes are still recoverable:"),
      h("pre", {}, "git branch recovered " + task.resultRef));
  }
  return h("div", { class: "card" }, h("strong", {}, task.outcome === "accepted" ? "Accepted" : "Rejected"), h("p", { class: "muted" }, "The task made no changes."));
}

function activity(items) {
  const visible = items.filter((item) => item.kind !== "usage");
  if (!visible.length) return h("p", { class: "empty" }, "No activity yet.");
  const results = new Map(items.filter((item) => item.kind === "tool_result").map((item) => [item.payload.callId, item.payload]));
  const list = h("div", { class: "card" });
  for (const item of visible) {
    const payload = item.payload || {};
    if (item.kind === "user_message") list.append(h("div", { class: "item" }, h("div", { class: "who" }, "You"), h("div", { class: "message" }, payload.content)));
    else if (item.kind === "assistant_message") list.append(h("div", { class: "item" }, h("div", { class: "who" }, "erka"), h("div", { class: "message" }, payload.content)));
    else if (item.kind === "tool_call") {
      const result = results.get(payload.callId);
      const line = h("div", { class: "item" }, h("div", { class: "tool" }, (result && result.isError ? "✕ " : "› ") + payload.tool + " " + compactArguments(payload.arguments)));
      if (result && result.output) line.append(h("details", {}, h("summary", {}, result.isError ? "error output" : "output"), h("pre", {}, result.output)));
      list.append(line);
    } else if (item.kind === "checkpoint") {
      const notes = (payload.decisions || []).concat(payload.validation_results || []);
      const summary = notes.length ? notes[notes.length - 1] : payload.approved_plan;
      if (summary) list.append(h("div", { class: "item" }, h("div", { class: "who" }, "Checkpoint"), h("div", { class: "message" }, summary)));
    } else if (item.kind === "error") list.append(h("div", { class: "item" }, h("div", { class: "who" }, "Error"), h("div", { class: "message" }, payload.message)));
  }
  return list;
}

function compactArguments(argumentsValue) {
  let text = typeof argumentsValue === "string" ? argumentsValue : JSON.stringify(argumentsValue || {});
  try {
    const parsed = typeof argumentsValue === "string" ? JSON.parse(argumentsValue) : argumentsValue;
    if (parsed && Array.isArray(parsed.command)) text = parsed.command.join(" ");
    else if (parsed && parsed.path) text = parsed.path;
    else if (parsed && parsed.question) text = parsed.question;
  } catch { /* keep raw */ }
  return text.length > 140 ? text.slice(0, 140) + "…" : text;
}

function diffView(diff) {
  if (!diff || !diff.trim()) return h("p", { class: "empty" }, "No changes in the worktree.");
  const pre = h("pre", { class: "diff" });
  for (const line of diff.split("\n")) {
    const kind = line.startsWith("+") && !line.startsWith("+++") ? "add" : line.startsWith("-") && !line.startsWith("---") ? "del" : line.startsWith("@@") ? "hunk" : "";
    pre.append(h("span", { class: kind }, line + "\n"));
  }
  return pre;
}

async function previewsView(taskID) {
  const container = h("div");
  try {
    const [presets, previews] = await Promise.all([api("GET", "/v1/previews/presets"), api("GET", "/v1/tasks/" + encodeURIComponent(taskID) + "/previews")]);
    if (!presets.presets.length) return h("p", { class: "empty" }, "No preview presets are configured on the server (server.previews.commands).");
    for (const preview of previews.previews) {
      const stop = h("button", { class: "danger" }, "Stop");
      stop.addEventListener("click", async () => { try { await api("DELETE", "/v1/previews/" + encodeURIComponent(preview.id)); container.replaceWith(await previewsView(taskID)); } catch (error) { toast(error.message); } });
      const logButton = h("button", {}, "Log");
      const logArea = h("div");
      logButton.addEventListener("click", async () => { try { const data = await api("GET", "/v1/previews/" + encodeURIComponent(preview.id) + "/log"); logArea.replaceChildren(h("pre", {}, data.log || "(empty)")); } catch (error) { toast(error.message); } });
      container.append(h("div", { class: "card" },
        h("div", { class: "row spread" }, h("strong", {}, preview.preset), badge(preview.status === "ready" ? "completed" : preview.status === "exited" ? "failed" : "running")),
        h("p", { class: "muted" }, preview.status + (preview.exitNote ? " — " + preview.exitNote : "")),
        h("div", { class: "row" }, h("a", { class: "button primary", href: preview.url, target: "_blank", rel: "noopener" }, "Open " + preview.url.replace(/^https?:\/\//, "")), logButton, stop),
        logArea));
    }
    const row = h("div", { class: "row" });
    for (const preset of presets.presets) {
      const start = h("button", {}, "Start " + preset);
      start.addEventListener("click", async () => {
        start.disabled = true;
        try { await api("POST", "/v1/tasks/" + encodeURIComponent(taskID) + "/previews", { preset }); toast("Starting " + preset + "…"); setTimeout(async () => container.replaceWith(await previewsView(taskID)), 1500); }
        catch (error) { toast(error.message); start.disabled = false; }
      });
      row.append(start);
    }
    container.append(row);
  } catch (error) {
    container.append(errorCard(error));
  }
  return container;
}

function memoryScreen() {
  const list = h("div");
  const project = h("select", { id: "memory-project" }, h("option", { value: "" }, "Everything (global)"));
  const text = h("textarea", { id: "memory-text", rows: 3, placeholder: "Something every model should remember" });
  const kind = h("select", { id: "memory-kind" }, ...["preference", "fact", "decision"].map((value) => h("option", { value }, value)));
  const add = h("button", { class: "primary" }, "Save memory");
  view.replaceChildren(h("h1", {}, "Memory"), h("p", { class: "muted" }, "Shared by every connected model. Notes marked unverified were saved by a model."),
    h("div", { class: "card" }, h("label", { for: "memory-project" }, "Applies to"), project, h("label", { for: "memory-kind" }, "Kind"), kind, h("label", { for: "memory-text" }, "Note"), text, h("div", { class: "row gap-top" }, add)),
    list);
  const refresh = async () => {
    try {
      const data = await api("GET", "/v1/memories" + (project.value ? "?projectId=" + encodeURIComponent(project.value) : ""));
      list.replaceChildren(...(data.memories.length ? data.memories.map((note) => {
        const remove = h("button", { class: "danger" }, "Delete");
        remove.addEventListener("click", async () => { try { await api("DELETE", "/v1/memories/" + encodeURIComponent(note.id)); refresh(); } catch (error) { toast(error.message); } });
        return h("div", { class: "card" }, h("div", { class: "row spread" }, h("span", { class: "badge" }, note.scope + " · " + note.kind + (note.origin === "model" ? " · unverified" : "")), remove), h("p", { class: "message" }, note.text));
      }) : [h("p", { class: "empty" }, "No memories yet.")]));
    } catch (error) { list.replaceChildren(errorCard(error)); }
  };
  api("GET", "/v1/projects").then((data) => { project.append(...data.projects.map((p) => h("option", { value: p.id }, p.name))); }).catch(() => {});
  project.addEventListener("change", () => { kind.value = project.value ? "fact" : "preference"; refresh(); });
  add.addEventListener("click", async () => {
    if (!text.value.trim()) return;
    add.disabled = true;
    try { await api("POST", "/v1/memories", { text: text.value.trim(), kind: kind.value, projectId: project.value || undefined }); text.value = ""; refresh(); }
    catch (error) { toast(error.message); }
    add.disabled = false;
  });
  refresh();
}

function settingsScreen() {
  const token = h("input", { id: "token", type: "password", autocomplete: "off", placeholder: "erka server token", value: storedToken() });
  const save = h("button", { class: "primary" }, "Save token");
  const clear = h("button", {}, "Forget token");
  const status = h("p", { class: "muted" });
  view.replaceChildren(h("h1", {}, "Settings"),
    h("div", { class: "card" },
      h("p", {}, "Create a token on the server with ", h("code", {}, "erka server token create phone --scope read,write,approve"), " and paste it here. It is stored only in this browser."),
      h("label", { for: "token" }, "API token"), token, h("div", { class: "row gap-top" }, save, clear), status));
  const check = async () => {
    try { const me = await api("GET", "/v1/me"); status.textContent = "Connected as " + (me.name || me.id) + " (" + me.scopes.join(", ") + ")"; }
    catch (error) { status.textContent = error.message; }
  };
  save.addEventListener("click", () => { saveToken(token.value.trim()); check(); });
  clear.addEventListener("click", () => { saveToken(""); token.value = ""; status.textContent = "Token removed."; });
  if (storedToken()) check();
}

function route() {
  timers.forEach(clearInterval);
  timers = [];
  const hash = location.hash || "#/";
  const task = hash.match(/^#\/tasks\/([^/]+)$/);
  const section = task ? "inbox" : ({ "#/new": "new", "#/memory": "memory", "#/settings": "settings" }[hash] || "inbox");
  document.querySelectorAll("[data-nav]").forEach((link) => link.classList.toggle("active", link.dataset.nav === section));
  if (!storedToken() && hash !== "#/settings") { location.hash = "#/settings"; return; }
  if (task) return taskScreen(decodeURIComponent(task[1]));
  ({ new: newTaskScreen, memory: memoryScreen, settings: settingsScreen }[section] || inboxScreen)();
}

window.addEventListener("hashchange", route);
if ("serviceWorker" in navigator) navigator.serviceWorker.register("sw.js").catch(() => {});
route();
