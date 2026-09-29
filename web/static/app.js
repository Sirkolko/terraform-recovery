"use strict";

// Terraform State Recovery Assistant — user interface.
// Plain JavaScript without dependencies. All content is inserted with
// textContent / DOM APIs, never as HTML, so values coming from the
// infrastructure or the configuration cannot inject markup.
(() => {
  const csrf = document.querySelector('meta[name="csrf-token"]').content;
  const $ = (sel) => document.querySelector(sel);

  // ------------------------------------------------------------------ helpers

  function h(tag, props, ...children) {
    const el = document.createElement(tag);
    if (props) {
      for (const [k, v] of Object.entries(props)) {
        if (v === undefined || v === null || v === false) continue;
        if (k === "class") el.className = v;
        else if (k === "text") el.textContent = v;
        else if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2), v);
        else if (k === "dataset") Object.assign(el.dataset, v);
        else if (k === "value") el.value = v;
        else if (k === "checked") el.checked = !!v;
        else el.setAttribute(k, v === true ? "" : String(v));
      }
    }
    for (const c of children.flat(Infinity)) {
      if (c === null || c === undefined || c === false) continue;
      el.append(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    return el;
  }

  async function api(method, path, body) {
    const opts = { method, headers: {}, credentials: "same-origin" };
    if (method !== "GET") {
      opts.headers["Content-Type"] = "application/json";
      opts.headers["X-CSRF-Token"] = csrf;
      opts.body = JSON.stringify(body ?? {});
    }
    const res = await fetch(path, opts);
    const text = await res.text();
    let data = null;
    try {
      data = text ? JSON.parse(text) : null;
    } catch {
      data = null;
    }
    if (!res.ok) throw new Error((data && data.error) || `${res.status} ${res.statusText}`);
    return data;
  }

  let toastTimer;
  function toast(msg, kind = "info") {
    const t = $("#toast");
    t.textContent = msg;
    t.className = `toast toast-${kind}`;
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => (t.hidden = true), kind === "error" ? 9000 : 3500);
  }

  async function act(fn, okMsg) {
    try {
      const r = await fn();
      if (okMsg) toast(typeof okMsg === "function" ? okMsg(r) : okMsg, "ok");
      await refresh();
      return r;
    } catch (e) {
      toast(e.message, "error");
      return null;
    }
  }

  const q = encodeURIComponent;
  const plural = (n, one, many) => `${n} ${n === 1 ? one : many || one + "s"}`;

  function when(iso) {
    if (!iso) return "";
    const d = new Date(iso);
    if (isNaN(d)) return "";
    return d.toLocaleString();
  }

  const STATUS = {
    matched: { icon: "✓", label: "Matched" },
    review: { icon: "?", label: "Needs review" },
    unmatched: { icon: "✗", label: "Unmatched" },
    ignored: { icon: "–", label: "Ignored" },
  };
  const AWS_STATUS = { ...STATUS, unmatched: { icon: "○", label: "Unmanaged" } };

  const SOURCE = {
    suggested: "Suggested by the matching engine — not confirmed",
    accepted: "Suggestion accepted",
    manual: "Linked manually",
    "manual-id": "Import ID entered manually",
    derived: "Import ID derived from related mappings",
    "import-block": "Import block already present in the configuration",
    state: "Already managed — present in the Terraform state",
  };
  const OUTCOME_ICON = { match: "✓", partial: "≈", mismatch: "✗", info: "·" };

  function statusIcon(status, aws) {
    const s = (aws ? AWS_STATUS : STATUS)[status] || { icon: "?", label: status };
    return h("span", { class: `st st-${status}`, title: s.label, "aria-label": s.label }, s.icon);
  }

  function confClass(n) {
    if (n >= 90) return "conf-high";
    if (n >= 50) return "conf-mid";
    return "conf-low";
  }

  function confBadge(n, ambiguous) {
    return h("span", { class: `conf ${confClass(n)}${ambiguous ? " conf-amb" : ""}`, title: ambiguous ? "Ambiguous match" : "Confidence" },
      `${n}%`, ambiguous ? " ⚠" : "");
  }

  function kv(rows) {
    return h("dl", { class: "kv" }, rows.filter((r) => r && r[1] !== undefined && r[1] !== null && r[1] !== "").map(([k, v]) => [h("dt", null, k), h("dd", null, v)]));
  }

  function card(title, ...children) {
    return h("section", { class: "card" }, title ? h("h3", null, title) : null, ...children);
  }

  // -------------------------------------------------------------------- state

  const state = {
    session: null,
    version: "",
    view: null,
    tfSel: null,
    awsSel: null,
    detail: null,
    cloudDetail: null,
    pair: null,
    pairError: null,
    tfFilter: "all",
    awsFilter: "all",
    tfQuery: "",
    awsQuery: "",
    compatibleOnly: true,
    job: null,
    jobLog: "",
    jobOffset: 0,
    jobTimer: null,
    preview: null,
    profiles: null,
    profile: null,
    regionsInput: null,
    identity: null,
    planText: null,
    applyAck: false,
  };

  const tfList = () => (state.session && state.session.terraform) || [];
  const awsList = () => (state.session && state.session.cloud) || [];
  const findTF = (addr) => tfList().find((r) => r.address === addr);
  const findAWS = (key) => awsList().find((r) => r.key === key);
  const jobRunning = () => state.job && state.job.state === "running";

  // --------------------------------------------------------------------- data

  async function refresh() {
    let data;
    try {
      data = await api("GET", "/api/session");
    } catch (e) {
      toast(e.message, "error");
      return;
    }
    state.session = data.session;
    state.version = data.version;
    if (!state.view) state.view = initialView();
    if (state.tfSel && !findTF(state.tfSel)) {
      state.tfSel = null;
      state.detail = null;
    }
    if (state.awsSel && !findAWS(state.awsSel)) {
      state.awsSel = null;
      state.cloudDetail = null;
    }
    const j = state.session.job;
    if (j && (!state.job || state.job.id !== j.id)) {
      if (j.state === "running") startJobPoll(j.id);
      else await loadJobLog(j);
    }
    await Promise.all([
      state.tfSel ? loadDetail(state.tfSel) : null,
      state.awsSel ? loadCloud(state.awsSel) : null,
      state.view === "plan" ? loadPreview() : null,
    ]);
    if (state.tfSel && state.awsSel) await loadPair();
    renderAll();
  }

  function initialView() {
    const fromHash = location.hash.replace("#", "");
    if (["scan", "review", "plan"].includes(fromHash)) return fromHash;
    return awsList().length === 0 ? "scan" : "review";
  }

  async function loadDetail(addr) {
    try {
      state.detail = await api("GET", `/api/resource?address=${q(addr)}`);
    } catch (e) {
      state.detail = null;
      toast(e.message, "error");
    }
  }

  async function loadCloud(key) {
    try {
      state.cloudDetail = await api("GET", `/api/cloud?key=${q(key)}`);
    } catch (e) {
      state.cloudDetail = null;
    }
  }

  async function loadPair() {
    state.pair = null;
    state.pairError = null;
    const tf = findTF(state.tfSel);
    const a = state.detail && state.detail.assignment;
    if (!tf || (a && a.cloud_key === state.awsSel)) return;
    try {
      state.pair = await api("GET", `/api/pair?address=${q(state.tfSel)}&cloud_key=${q(state.awsSel)}`);
    } catch (e) {
      state.pairError = e.message;
    }
  }

  async function loadPreview() {
    try {
      state.preview = await api("GET", "/api/imports");
    } catch (e) {
      state.preview = null;
      toast(e.message, "error");
    }
  }

  // --------------------------------------------------------------------- jobs

  const JOB_LABEL = { scan: "AWS scan", plan: "Terraform plan", apply: "Import apply", modules: "Module installation" };

  // loadJobLog shows the output of a job that finished before the page loaded.
  async function loadJobLog(j) {
    state.job = j;
    state.jobLog = "";
    state.jobOffset = 0;
    try {
      const v = await api("GET", `/api/jobs/${q(j.id)}?offset=0`);
      state.job = v;
      state.jobLog = v.log;
      state.jobOffset = v.next_offset;
    } catch {
      // The log is informational only.
    }
  }

  function startJobPoll(id) {
    clearTimeout(state.jobTimer);
    state.job = { id, state: "running", kind: "" };
    state.jobLog = "";
    state.jobOffset = 0;
    pollJob();
  }

  async function pollJob() {
    if (!state.job) return;
    try {
      const v = await api("GET", `/api/jobs/${q(state.job.id)}?offset=${state.jobOffset}`);
      state.job = v;
      state.jobLog += v.log;
      state.jobOffset = v.next_offset;
      updateJobLogs();
      if (v.state === "running") {
        state.jobTimer = setTimeout(pollJob, 700);
      } else {
        await onJobDone(v);
      }
    } catch (e) {
      state.jobTimer = setTimeout(pollJob, 2000);
    }
  }

  async function onJobDone(v) {
    const label = JOB_LABEL[v.kind] || v.kind;
    if (v.state === "succeeded") toast(`${label} finished`, "ok");
    else if (v.state === "cancelled") toast(`${label} cancelled`, "warn");
    else toast(`${label} failed: ${v.error}`, "error");
    state.preview = null;
    state.planText = null;
    state.applyAck = false;
    if (v.kind === "scan" && v.state === "succeeded") setView("review");
    await refresh();
  }

  async function startJob(path, body) {
    try {
      const r = await api("POST", path, body);
      startJobPoll(r.job);
      renderAll();
    } catch (e) {
      toast(e.message, "error");
    }
  }

  function jobPanel(kinds) {
    if (!state.job || !kinds.includes(state.job.kind || kinds[0])) return null;
    const j = state.job;
    const running = j.state === "running";
    return card(
      `${JOB_LABEL[j.kind] || "Operation"} — ${running ? "running…" : j.state}`,
      j.error && !running ? h("p", { class: "alert alert-error" }, j.error) : null,
      h("pre", { class: "joblog", tabindex: "0" }, state.jobLog || "Starting…"),
      running && j.cancelable
        ? h("button", { class: "btn", type: "button", onclick: () => act(() => api("POST", `/api/jobs/${q(j.id)}/cancel`)) }, "Cancel")
        : null,
    );
  }

  function updateJobLogs() {
    for (const pre of document.querySelectorAll(".joblog")) {
      const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 20;
      pre.textContent = state.jobLog;
      if (atBottom) pre.scrollTop = pre.scrollHeight;
    }
  }

  // ------------------------------------------------------------------- render

  function setView(v) {
    state.view = v;
    if (location.hash !== `#${v}`) history.replaceState(null, "", `#${v}`);
  }

  function renderAll() {
    if (!state.session) return;
    renderSteps();
    renderSummary();
    renderDiagnostics();
    for (const v of ["scan", "review", "plan"]) $(`#view-${v}`).hidden = state.view !== v;
    if (state.view === "scan") renderScan();
    if (state.view === "review") renderReview();
    if (state.view === "plan") renderPlan();
    updateJobLogs();
  }

  function renderSteps() {
    const s = state.session;
    for (const btn of document.querySelectorAll("#steps button")) {
      btn.classList.toggle("active", btn.dataset.view === state.view);
    }
    $("#step-scan").textContent = s.aws.account_id ? `account ${s.aws.account_id}` : "not scanned";
    const sum = s.summary;
    $("#step-review").textContent = sum.terraform ? `${sum.matched}/${sum.terraform} matched` : "";
    const p = s.plan;
    $("#step-plan").textContent = p ? (p.applied ? "applied" : p.error ? "plan failed" : p.can_apply ? "ready to apply" : "review plan") : "";
  }

  function renderSummary() {
    const s = state.session.summary;
    const stat = (label, n, cls, view, filter, side) =>
      h("button", {
        type: "button", class: `stat ${cls || ""}`,
        onclick: () => {
          setView(view || "review");
          if (side === "aws") state.awsFilter = filter || "all";
          else state.tfFilter = filter || "all";
          renderAll();
        },
      }, h("strong", null, String(n)), h("span", null, label));
    const high = s.high_confidence;
    $("#summary").replaceChildren(
      h("div", { class: "stats" },
        h("div", { class: "stat-group" },
          stat("Terraform resources", s.terraform, "", "review", "all"),
          stat("Matched", s.matched, "c-matched", "review", "matched"),
          stat("Needs review", s.review, "c-review", "review", "review"),
          stat("Unmatched Terraform", s.unmatched, "c-unmatched", "review", "unmatched"),
          stat("Ignored", s.ignored, "c-ignored", "review", "ignored"),
        ),
        h("div", { class: "stat-group" },
          stat("AWS resources discovered", s.cloud, "", "review", "all", "aws"),
          stat("Unmanaged AWS resources", s.unmanaged, "c-unmatched", "review", "unmatched", "aws"),
          stat("Ignored AWS", s.cloud_ignored, "c-ignored", "review", "ignored", "aws"),
        ),
      ),
      h("div", { class: "summary-actions" },
        h("button", {
          type: "button", class: "btn btn-primary", disabled: high === 0,
          title: `Confirm every unambiguous suggestion with at least ${state.session.thresholds.high}% confidence`,
          onclick: () => act(() => api("POST", "/api/accept-above", { threshold: state.session.thresholds.high }),
            (r) => `Accepted ${plural(r.accepted, "mapping")}`),
        }, `Accept all high-confidence matches (${high})`),
        h("button", { type: "button", class: "btn", onclick: () => { setView("plan"); refresh(); } }, "Go to plan →"),
      ),
    );
  }

  function renderDiagnostics() {
    const diags = state.session.diagnostics || [];
    const box = $("#diagnostics");
    if (!diags.length) {
      box.replaceChildren();
      return;
    }
    const order = { error: 0, warning: 1, info: 2 };
    const sorted = [...diags].sort((a, b) => (order[a.severity] ?? 3) - (order[b.severity] ?? 3));
    const errors = sorted.filter((d) => d.severity === "error").length;
    const item = (d) => h("li", { class: `diag diag-${d.severity}` },
      h("strong", null, d.summary),
      d.detail ? h("span", null, ` — ${d.detail}`) : null,
      d.file ? h("code", null, ` ${d.file}${d.line ? ":" + d.line : ""}`) : null);
    box.replaceChildren(h("details", { class: errors ? "diag-box has-errors" : "diag-box", open: errors > 0 },
      h("summary", null, `${plural(errors, "error")}, ${plural(sorted.length - errors, "warning")} — the user should never have to guess what was skipped`),
      h("ul", null, sorted.map(item))));
  }

  // -------------------------------------------------------------- review view

  function renderReview() {
    renderTFPanel();
    renderAWSPanel();
    renderDetail();
  }

  function filterChips(counts, current, onPick, labels) {
    return h("div", { class: "chips", role: "group" }, Object.entries(labels).map(([key, label]) =>
      h("button", { type: "button", class: `chip ${current === key ? "active" : ""}`, onclick: () => onPick(key) },
        `${label} ${counts[key] ?? 0}`)));
  }

  function countBy(list) {
    const c = { all: list.length };
    for (const r of list) c[r.status] = (c[r.status] || 0) + 1;
    return c;
  }

  function renderTFPanel() {
    const panel = $("#tf-panel");
    const list = tfList();
    const search = h("input", {
      type: "search", placeholder: "Filter by address…", value: state.tfQuery, "aria-label": "Filter Terraform resources",
      oninput: (e) => { state.tfQuery = e.target.value; renderTFList(); },
    });
    panel.replaceChildren(
      h("div", { class: "panel-head" }, h("h2", null, "Terraform configuration"), h("span", { class: "muted" }, plural(list.length, "instance"))),
      h("div", { class: "toolbar" }, search,
        filterChips(countBy(list), state.tfFilter, (k) => { state.tfFilter = k; renderTFPanel(); },
          { all: "All", review: "Review", unmatched: "Unmatched", matched: "Matched", ignored: "Ignored" })),
      h("div", { class: "list", id: "tf-list", role: "list" }),
    );
    renderTFList();
  }

  function renderTFList() {
    const box = $("#tf-list");
    if (!box) return;
    const query = state.tfQuery.toLowerCase();
    const mappedToAWS = state.awsSel ? findAWS(state.awsSel)?.mapped_to : null;
    const rows = tfList().filter((r) =>
      (state.tfFilter === "all" || r.status === state.tfFilter) && (!query || r.address.toLowerCase().includes(query)));
    const groups = new Map();
    for (const r of rows) {
      const g = r.module || "root module";
      if (!groups.has(g)) groups.set(g, []);
      groups.get(g).push(r);
    }
    const out = [];
    for (const [g, items] of groups) {
      out.push(h("div", { class: "group" }, g, h("span", { class: "muted" }, ` ${items.length}`)));
      for (const r of items) {
        const short = r.module ? r.address.slice(r.module.length + 1) : r.address;
        const m = r.mapping;
        out.push(h("button", {
          type: "button", role: "listitem",
          class: `row ${state.tfSel === r.address ? "selected" : ""} ${mappedToAWS === r.address ? "linked" : ""}`,
          title: [r.address, ...(r.notes || [])].join("\n"),
          onclick: () => selectTF(r.address),
        },
        statusIcon(r.status),
        h("span", { class: "addr" }, short),
        h("span", { class: "row-meta" },
          r.support === "manual" && r.status !== "matched" ? h("span", { class: "tag" }, "manual ID") : null,
          r.support === "derived" ? h("span", { class: "tag" }, "derived") : null,
          r.unexpanded ? h("span", { class: "tag tag-warn" }, "keys?") : null,
          m && m.source !== "state" && m.source !== "manual-id" && m.source !== "derived" && m.source !== "import-block" ? confBadge(m.confidence, m.ambiguous) : null,
          m && m.source === "state" ? h("span", { class: "tag tag-ok" }, "in state") : null,
        )));
      }
    }
    if (!out.length) out.push(h("p", { class: "empty" }, tfList().length ? "No resources match the filter." : "No resources found in the configuration."));
    box.replaceChildren(...out);
  }

  function renderAWSPanel() {
    const panel = $("#aws-panel");
    const list = awsList();
    const cloudType = state.detail && state.detail.cloud_type;
    const search = h("input", {
      type: "search", placeholder: "Filter by ID, name or tag…", value: state.awsQuery, "aria-label": "Filter AWS resources",
      oninput: (e) => { state.awsQuery = e.target.value; renderAWSList(); },
    });
    panel.replaceChildren(
      h("div", { class: "panel-head" }, h("h2", null, "AWS infrastructure"),
        h("span", { class: "muted" }, state.session.aws.account_id ? `account ${state.session.aws.account_id}` : "not scanned")),
      h("div", { class: "toolbar" }, search,
        filterChips(countBy(list), state.awsFilter, (k) => { state.awsFilter = k; renderAWSPanel(); },
          { all: "All", unmatched: "Unmanaged", review: "Review", matched: "Matched", ignored: "Ignored" }),
        state.tfSel && cloudType
          ? h("label", { class: "check" },
            h("input", { type: "checkbox", checked: state.compatibleOnly, onchange: (e) => { state.compatibleOnly = e.target.checked; renderAWSList(); } }),
            ` Only ${cloudType} (compatible with the selection)`)
          : null),
      h("div", { class: "list", id: "aws-list", role: "list" }),
    );
    renderAWSList();
  }

  function renderAWSList() {
    const box = $("#aws-list");
    if (!box) return;
    const query = state.awsQuery.toLowerCase();
    const cloudType = state.detail && state.detail.cloud_type;
    const mappedKey = state.detail && state.detail.assignment && state.detail.assignment.cloud_key;
    const candidates = new Set(((state.detail && state.detail.candidates) || []).filter((c) => !c.disqualified).map((c) => c.cloud_key));
    const rows = awsList().filter((r) => {
      if (state.awsFilter !== "all" && r.status !== state.awsFilter) return false;
      if (state.tfSel && cloudType && state.compatibleOnly && r.type !== cloudType) return false;
      if (!query) return true;
      const hay = [r.id, r.name, r.type, r.region, r.mapped_to, ...Object.entries(r.tags || {}).flat()].join(" ").toLowerCase();
      return hay.includes(query);
    });
    const groups = new Map();
    for (const r of rows) {
      if (!groups.has(r.type)) groups.set(r.type, []);
      groups.get(r.type).push(r);
    }
    const multiRegion = new Set(awsList().map((r) => r.region).filter((r) => r !== "global")).size > 1;
    const out = [];
    for (const [type, items] of [...groups.entries()].sort()) {
      out.push(h("div", { class: "group" }, type, h("span", { class: "muted" }, ` ${items.length}`)));
      for (const r of items) {
        out.push(h("button", {
          type: "button", role: "listitem",
          class: `row ${state.awsSel === r.key ? "selected" : ""} ${mappedKey === r.key ? "linked" : ""} ${candidates.has(r.key) && mappedKey !== r.key ? "candidate" : ""}`,
          title: [r.id, r.name, r.ignore_reason, r.mapped_to ? `→ ${r.mapped_to}` : ""].filter(Boolean).join("\n"),
          onclick: () => selectAWS(r.key),
        },
        statusIcon(r.status, true),
        h("span", { class: "addr" }, h("span", { class: "rid" }, r.id.length > 48 ? "…" + r.id.slice(-46) : r.id),
          r.name && r.name !== r.id ? h("span", { class: "rname" }, r.name) : null),
        h("span", { class: "row-meta" },
          multiRegion ? h("span", { class: "tag" }, r.region) : null,
          r.mapped_to ? h("span", { class: "tag tag-link" }, `→ ${r.mapped_to}`) : null,
          r.auto_ignored ? h("span", { class: "tag", title: r.ignore_reason }, "auto-ignored") : null)));
      }
    }
    if (!out.length) {
      out.push(h("p", { class: "empty" }, awsList().length ? "No resources match the filter." : "No inventory yet — scan AWS first."));
    }
    box.replaceChildren(...out);
  }

  async function selectTF(addr) {
    if (state.tfSel === addr) {
      state.tfSel = null;
      state.detail = null;
      state.pair = null;
    } else {
      state.tfSel = addr;
      await loadDetail(addr);
      if (state.awsSel) {
        const aws = findAWS(state.awsSel);
        if (state.detail && aws && state.detail.cloud_type && aws.type !== state.detail.cloud_type) {
          state.awsSel = null;
          state.cloudDetail = null;
        }
      }
      if (state.awsSel) await loadPair();
    }
    renderReview();
  }

  async function selectAWS(key) {
    if (state.awsSel === key) {
      state.awsSel = null;
      state.cloudDetail = null;
      state.pair = null;
    } else {
      state.awsSel = key;
      // A resource that cannot be linked to the selected Terraform resource
      // is shown on its own instead of as a (impossible) link preview.
      const aws = findAWS(key);
      if (state.tfSel && state.detail && aws && state.detail.cloud_type !== aws.type) {
        state.tfSel = null;
        state.detail = null;
        state.pair = null;
      }
      await loadCloud(key);
      if (state.tfSel) await loadPair();
    }
    renderReview();
  }

  function signalsTable(signals) {
    if (!signals || !signals.length) return h("p", { class: "muted" }, "No evidence recorded.");
    return h("table", { class: "signals" }, h("tbody", null, signals.map((s) =>
      h("tr", { class: `sig-${s.outcome}` },
        h("td", { class: "sig-icon" }, OUTCOME_ICON[s.outcome] || ""),
        h("td", { class: "sig-label" }, s.label),
        h("td", { class: "sig-points" }, s.outcome === "info" ? "" : `${s.points > 0 ? "+" : ""}${s.points}${s.max ? " / " + s.max : ""}`),
        h("td", { class: "sig-detail" }, s.detail || "")))));
  }

  function ignoreForm(kind, key, help) {
    const input = h("input", { type: "text", placeholder: "Reason (recommended)", maxlength: "500", "aria-label": "Reason" });
    return h("details", { class: "inline-form" },
      h("summary", null, kind === "terraform" ? "Ignore — do not import this resource…" : "Ignore — leave this resource unmanaged…"),
      h("p", { class: "muted" }, help),
      h("div", { class: "form-row" }, input,
        h("button", { type: "button", class: "btn btn-warn", onclick: () => act(() => api("POST", "/api/ignore", { kind, key, reason: input.value }), "Marked as ignored") }, "Ignore")));
  }

  function renderDetail() {
    const box = $("#detail");
    const tf = state.tfSel && findTF(state.tfSel);
    const aws = state.awsSel && findAWS(state.awsSel);
    const parts = [];
    if (!awsList().length && !tf) {
      parts.push(h("div", { class: "hint-box" },
        h("h2", null, "No AWS inventory yet"),
        h("p", null, "Scan your AWS account first. Discovery only uses read-only Describe/List/Get calls."),
        h("button", { type: "button", class: "btn btn-primary", onclick: () => { setView("scan"); renderAll(); } }, "Go to scan")));
    }
    if (tf && state.detail) parts.push(...tfDetail(tf, aws));
    else if (aws) parts.push(...awsDetail(aws));
    else if (awsList().length) {
      parts.push(h("div", { class: "hint-box" },
        h("h2", null, "Review the mappings"),
        h("p", null, "Select a Terraform resource on the left to see its suggested AWS resource and the evidence behind it."),
        h("p", null, "To link resources manually, select a Terraform resource, then an AWS resource on the right, and press Link."),
        h("p", { class: "muted" }, "Suggestions are never imported until you confirm them.")));
    }
    box.replaceChildren(...parts);
  }

  function tfDetail(tf, aws) {
    const d = state.detail;
    const r = d.resource;
    const a = d.assignment;
    const out = [];
    out.push(h("div", { class: "detail-head" },
      statusIcon(tf.status),
      h("div", null,
        h("h2", { class: "mono" }, tf.address),
        h("p", { class: "muted" }, [tf.type, tf.file ? `${tf.file}:${tf.line}` : null, `provider ${tf.provider}${tf.region ? " · " + tf.region : ""}`].filter(Boolean).join(" · "))),
      h("button", { type: "button", class: "btn btn-ghost", title: "Clear selection", onclick: () => selectTF(tf.address) }, "✕")));

    // Pair preview when an AWS resource is selected as well.
    if (aws && (!a || a.cloud_key !== aws.key)) {
      out.push(h("section", { class: "card pair" },
        h("h3", null, "Link preview"),
        h("div", { class: "link-visual" },
          h("span", { class: "mono" }, tf.address),
          h("span", { class: "arrow" }, "↕", state.pair ? confBadge(state.pair.confidence) : null),
          h("span", { class: "mono" }, aws.id, aws.name && aws.name !== aws.id ? ` (${aws.name})` : "")),
        state.pairError ? h("p", { class: "alert alert-error" }, state.pairError) : null,
        state.pair && state.pair.disqualified ? h("p", { class: "alert alert-warn" }, `Strong evidence against this link: ${state.pair.disqualified}.`) : null,
        state.pair ? signalsTable(state.pair.signals) : null,
        h("div", { class: "actions" },
          h("button", {
            type: "button", class: "btn btn-primary", disabled: !!state.pairError,
            onclick: () => act(() => api("POST", "/api/link", { address: tf.address, cloud_key: aws.key }), "Linked"),
          }, "Link these resources"),
          h("button", { type: "button", class: "btn", onclick: () => selectAWS(aws.key) }, "Cancel"))));
    }

    // Current mapping.
    const mapping = [];
    if (tf.status === "ignored") {
      mapping.push(h("p", null, `Ignored: ${tf.ignore_reason}`),
        h("p", { class: "muted" }, "This resource is not imported. Terraform will propose to create it later unless you map it."),
        h("div", { class: "actions" }, h("button", { type: "button", class: "btn", onclick: () => act(() => api("POST", "/api/unignore", { kind: "terraform", key: tf.address }), "Restored") }, "Stop ignoring")));
    } else if (a) {
      const cloud = a.cloud_key && findAWS(a.cloud_key);
      mapping.push(h("div", { class: "link-visual" },
        h("span", { class: "mono" }, tf.address),
        h("span", { class: "arrow" }, "↕", a.source !== "state" && a.source !== "manual-id" && a.source !== "import-block" ? confBadge(a.confidence, a.ambiguous) : null),
        h("span", { class: "mono" }, cloud ? cloud.id : a.import_id || "—", cloud && cloud.name && cloud.name !== cloud.id ? ` (${cloud.name})` : "")));
      mapping.push(h("p", { class: `source ${a.confirmed ? "ok" : "pending"}` }, SOURCE[a.source] || a.source));
      if (a.import_id) mapping.push(kv([["Import ID", h("code", null, a.import_id)]]));
      for (const n of a.notes || []) mapping.push(h("p", { class: "alert alert-warn" }, n));
      const actions = [];
      if (!a.confirmed && a.source === "suggested") {
        actions.push(h("button", { type: "button", class: "btn btn-primary", onclick: () => act(() => api("POST", "/api/accept", { addresses: [tf.address] }), "Mapping confirmed") }, "Confirm mapping"));
      }
      if (["accepted", "manual", "manual-id"].includes(a.source)) {
        actions.push(h("button", { type: "button", class: "btn", onclick: () => act(() => api("POST", "/api/unlink", { address: tf.address }), "Mapping removed") }, "Unlink"));
      }
      if (cloud) actions.push(h("button", { type: "button", class: "btn btn-ghost", onclick: () => selectAWS(cloud.key) }, "Show AWS resource"));
      if (actions.length) mapping.push(h("div", { class: "actions" }, actions));
      if (a.signals && a.signals.length) mapping.push(h("h4", null, "Evidence"), signalsTable(a.signals));
    } else {
      mapping.push(h("p", null, "No mapping."));
      for (const n of tf.notes || []) mapping.push(h("p", { class: "alert alert-warn" }, n));
    }
    out.push(card("Mapping", ...mapping));

    // Candidates.
    const cands = d.candidates || [];
    if (d.cloud_type) {
      out.push(card(`Candidates (${d.cloud_type})`,
        cands.length ? h("table", { class: "cands" }, h("tbody", null, cands.map((c) => {
          const current = a && a.cloud_key === c.cloud_key;
          return h("tr", { class: c.disqualified ? "rejected" : "" },
            h("td", null, c.disqualified ? h("span", { class: "conf conf-low" }, "✗") : confBadge(c.confidence)),
            h("td", null, h("button", { type: "button", class: "linkish mono", onclick: () => selectAWS(c.cloud_key) }, c.id),
              c.name && c.name !== c.id ? h("div", { class: "muted" }, c.name) : null,
              c.disqualified ? h("div", { class: "muted" }, `Rejected: ${c.disqualified}`) : null),
            h("td", null, c.mapped_to && !current ? h("span", { class: "tag tag-link" }, `→ ${c.mapped_to}`) : null),
            h("td", null, current ? h("span", { class: "tag tag-ok" }, "current")
              : c.disqualified ? null
                : h("button", { type: "button", class: "btn btn-small", onclick: () => act(() => api("POST", "/api/link", { address: tf.address, cloud_key: c.cloud_key }), "Linked") }, "Link")));
        }))) : h("p", { class: "muted" }, "No discovered resource of this type is a candidate. Select one on the right to link it anyway, or enter the import ID manually.")));
    }

    // Manual import ID.
    if (tf.status !== "ignored" && !(a && ["state", "import-block"].includes(a.source))) {
      const input = h("input", { type: "text", placeholder: "Import ID, e.g. vpc-0a1b2c3d", value: a && a.source === "manual-id" ? a.import_id : "", "aria-label": "Import ID" });
      out.push(h("details", { class: "card inline-form", open: tf.support === "manual" && tf.status === "unmatched" },
        h("summary", null, "Enter the import ID manually"),
        h("p", { class: "muted" }, d.derived ? `The ID is normally derived (${d.derived}); set it here only to override.` : "Use this for resource types without automatic discovery. Check the provider documentation for the import ID format."),
        h("div", { class: "form-row" }, input,
          h("button", { type: "button", class: "btn", onclick: () => act(() => api("POST", "/api/manual-id", { address: tf.address, import_id: input.value }), "Import ID saved") }, "Save"))));
    }

    // Instance keys for unexpanded resources.
    if (tf.unexpanded) {
      const input = h("input", { type: "text", placeholder: "0, 1, 2  or  a, b", "aria-label": "Instance keys" });
      out.push(card("Instance keys",
        h("p", { class: "muted" }, "count/for_each depends on values only known during apply. List the instance keys that exist (count indexes or for_each keys), separated by commas."),
        h("div", { class: "form-row" }, input,
          h("button", { type: "button", class: "btn", onclick: () => act(() => api("POST", "/api/instance-keys", { resource: tf.address, keys: input.value.split(",").map((s) => s.trim()).filter(Boolean) }), "Instance keys saved") }, "Save keys"))));
    }

    if (tf.status !== "ignored") {
      out.push(ignoreForm("terraform", tf.address,
        "Use this only when the resource really does not exist in AWS or is recovered some other way. It will not be imported, and Terraform will propose to create it on a normal plan."));
    }

    out.push(configDetails(r));
    return out;
  }

  function configDetails(r) {
    const attrs = Object.entries(r.attributes || {});
    const tags = Object.entries(r.tags || {});
    const refs = r.references || [];
    return h("details", { class: "card" },
      h("summary", null, "Configuration values used for matching"),
      attrs.length ? h("table", { class: "props" }, h("tbody", null, attrs.map(([k, v]) => h("tr", null, h("th", null, k), h("td", null, v.join(", ")))))) : h("p", { class: "muted" }, "No attribute values could be evaluated."),
      r.unknown && r.unknown.length ? h("p", { class: "muted" }, `Not statically known: ${r.unknown.join(", ")}`) : null,
      h("h4", null, `Tags${r.tags_known ? "" : " (partially known)"}`),
      tags.length ? h("table", { class: "props" }, h("tbody", null, tags.map(([k, v]) => h("tr", null, h("th", null, k), h("td", null, v))))) : h("p", { class: "muted" }, "None."),
      refs.length ? [h("h4", null, "References"), h("ul", { class: "refs" }, refs.map((x) => h("li", null, h("code", null, x.attribute), " → ",
        h("button", { type: "button", class: "linkish mono", onclick: () => findTF(x.target) && selectTF(x.target) }, `${x.target}.${x.target_attr}`))))] : null,
      h("p", { class: "muted" }, "Only attributes relevant for matching are read; secrets such as passwords are never evaluated."));
  }

  function awsDetail(aws) {
    const d = state.cloudDetail;
    const r = d && d.resource;
    const out = [];
    out.push(h("div", { class: "detail-head" },
      statusIcon(aws.status, true),
      h("div", null, h("h2", { class: "mono" }, aws.id), h("p", { class: "muted" }, [aws.type, aws.name && aws.name !== aws.id ? aws.name : null, aws.region].filter(Boolean).join(" · "))),
      h("button", { type: "button", class: "btn btn-ghost", title: "Clear selection", onclick: () => selectAWS(aws.key) }, "✕")));
    const body = [];
    if (aws.mapped_to) {
      body.push(h("p", null, aws.status === "matched" ? "Mapped to " : "Suggested for ",
        h("button", { type: "button", class: "linkish mono", onclick: () => selectTF(aws.mapped_to) }, aws.mapped_to)));
    } else if (aws.status === "ignored") {
      body.push(h("p", null, `Ignored: ${aws.ignore_reason}`));
      if (!aws.auto_ignored) {
        body.push(h("div", { class: "actions" }, h("button", { type: "button", class: "btn", onclick: () => act(() => api("POST", "/api/unignore", { kind: "aws", key: aws.key }), "Restored") }, "Stop ignoring")));
      } else {
        body.push(h("p", { class: "muted" }, "Ignored automatically because it is normally not managed with Terraform. It can still be linked if your configuration manages it."));
      }
    } else {
      body.push(h("p", null, "Not managed by this configuration."),
        h("p", { class: "muted" }, "Select a Terraform resource on the left to link it, or ignore it deliberately to leave it unmanaged."));
    }
    for (const hint of aws.hints || []) body.push(h("p", { class: "alert alert-warn" }, hint));
    out.push(card("Status", ...body));
    if (!aws.mapped_to && aws.status !== "ignored") {
      out.push(ignoreForm("aws", aws.key, "It will stay outside of Terraform. It is listed as ignored so nothing is ever silently skipped."));
    }
    if (r) {
      const props = [
        ...Object.entries(r.attributes || {}).map(([k, v]) => [k, v.join(", ")]),
        ...Object.entries(r.relations || {}).map(([k, v]) => [`${k} →`, v.join(", ")]),
      ];
      out.push(h("details", { class: "card", open: true },
        h("summary", null, "Discovered properties"),
        kv([["Import ID", h("code", null, r.import_id)], ["ARN", r.arn ? h("code", null, r.arn) : ""], ["Account", r.account_id]]),
        props.length ? h("table", { class: "props" }, h("tbody", null, props.map(([k, v]) => h("tr", null, h("th", null, k), h("td", null, v))))) : null,
        Object.keys(r.tags || {}).length ? [h("h4", null, "Tags"), h("table", { class: "props" }, h("tbody", null, Object.entries(r.tags).map(([k, v]) => h("tr", null, h("th", null, k), h("td", null, v)))))] : null));
    }
    return out;
  }

  // ---------------------------------------------------------------- scan view

  async function loadProfiles() {
    if (state.profiles) return;
    try {
      state.profiles = await api("GET", "/api/profiles");
      if (state.profile === null) state.profile = state.session.aws.profile || state.profiles.default || "";
      renderAll();
    } catch (e) {
      state.profiles = { profiles: [], default: "" };
    }
  }

  function renderScan() {
    loadProfiles();
    const s = state.session;
    const aws = s.aws;
    const profiles = (state.profiles && state.profiles.profiles) || [];
    if (state.profile === null) state.profile = aws.profile || "";
    if (state.regionsInput === null) state.regionsInput = (aws.suggested_regions || []).join(", ");
    const select = h("select", { "aria-label": "AWS profile", onchange: (e) => { state.profile = e.target.value; state.identity = null; } },
      h("option", { value: "" }, "Default credential chain (environment, SSO, instance role…)"),
      profiles.map((p) => h("option", { value: p }, p)),
      // A profile recorded in an inventory from another machine may not exist here.
      state.profile && !profiles.includes(state.profile)
        ? h("option", { value: state.profile }, `${state.profile} (not found in the local AWS config)`) : null);
    select.value = state.profile || "";
    const regions = h("input", { type: "text", value: state.regionsInput, "aria-label": "Regions", placeholder: "eu-west-1, us-east-1",
      oninput: (e) => (state.regionsInput = e.target.value) });

    const parts = [];
    parts.push(card("1. Choose AWS credentials",
      h("p", { class: "muted" }, "Credentials are never entered here. The AWS SDK default credential chain is used: environment variables, profiles in ~/.aws, IAM Identity Center (SSO), or the role of this machine."),
      h("div", { class: "form-row" }, select,
        h("button", { type: "button", class: "btn", onclick: async () => {
          try {
            state.identity = await api("POST", "/api/identity", { profile: state.profile });
          } catch (e) {
            state.identity = { error: e.message };
          }
          renderAll();
        } }, "Check identity")),
      state.identity ? (state.identity.error ? h("p", { class: "alert alert-error" }, state.identity.error)
        : h("p", { class: "alert alert-ok" }, `Authenticated as ${state.identity.arn} (account ${state.identity.account_id})`)) : null));

    parts.push(card("2. Choose regions",
      h("p", { class: "muted" }, "Global services (IAM, S3 bucket list) are always included. Regions from the provider configuration are suggested."),
      h("div", { class: "form-row" }, regions),
      h("div", { class: "actions" },
        h("button", {
          type: "button", class: "btn btn-primary", disabled: jobRunning(),
          onclick: () => startJob("/api/scan", { profile: state.profile, regions: state.regionsInput.split(/[\s,]+/).filter(Boolean) }),
        }, "Scan AWS (read-only)"),
        h("span", { class: "muted" }, "Only Describe*, List* and Get* calls are made."))));

    const job = jobPanel(["scan", "modules"]);
    if (job) parts.push(job);

    if (aws.account_id) {
      const failed = (aws.coverage || []).filter((c) => c.status !== "ok");
      parts.push(card("Last inventory",
        kv([
          ["Account", aws.account_id],
          ["Caller", aws.caller_arn],
          ["Regions", (aws.scanned_regions || []).join(", ")],
          ["Scanned", when(aws.scanned_at)],
          ["Source", { scan: "scanned in this session", saved: "saved inventory (.recovery/inventory.json)", file: "inventory file (--inventory)" }[aws.source] || aws.source],
          ["Resources", String(awsList().length)],
        ]),
        failed.length ? h("p", { class: "alert alert-warn" }, `${plural(failed.length, "discovery call")} failed — resources of these kinds may be missing.`) : null,
        h("details", { open: failed.length > 0 },
          h("summary", null, `Discovery coverage (${(aws.coverage || []).length} calls)`),
          h("table", { class: "coverage" },
            h("thead", null, h("tr", null, h("th", null, "Region"), h("th", null, "API call"), h("th", null, "Found"), h("th", null, "Status"))),
            h("tbody", null, [...(aws.coverage || [])].sort((a, b) => (a.status === "ok") - (b.status === "ok")).map((c) =>
              h("tr", { class: c.status !== "ok" ? "bad" : "" },
                h("td", null, c.region), h("td", null, h("code", null, c.service)), h("td", null, String(c.count)),
                h("td", null, c.status, c.error ? h("div", { class: "muted" }, c.error) : null))))))));
    }

    const p = s.project;
    const unloaded = (s.modules || []).filter((m) => !m.loaded);
    parts.push(card("Terraform project",
      kv([
        ["Directory", h("code", null, p.dir)],
        ["Backend", p.backend || "local (terraform.tfstate)"],
        ["Workspace", p.workspace],
        ["Terraform", p.terraform || h("span", { class: "bad-text" }, p.terraform_error || "not found")],
        ["Recovery data", h("code", null, p.state_dir)],
      ]),
      p.config_error ? h("p", { class: "alert alert-error" }, p.config_error) : null,
      (s.providers || []).length ? h("table", { class: "coverage" },
        h("thead", null, h("tr", null, h("th", null, "Provider"), h("th", null, "Region"), h("th", null, "Details"))),
        h("tbody", null, s.providers.map((pr) => h("tr", null, h("td", null, h("code", null, pr.address)),
          h("td", null, pr.region || (pr.region_known ? "" : "from environment")),
          h("td", null, [pr.implicit ? "no provider block" : "", pr.profile ? `profile ${pr.profile}` : "", pr.assume_role_arn ? `assumes ${pr.assume_role_arn}` : ""].filter(Boolean).join(" · ")))))) : null,
      unloaded.length ? h("p", { class: "alert alert-warn" }, `${plural(unloaded.length, "module")} not installed: ${unloaded.map((m) => m.address).join(", ")}. Their resources are not listed.`) : null,
      h("div", { class: "actions" },
        h("button", { type: "button", class: "btn", onclick: () => act(() => api("POST", "/api/reload"), "Configuration reloaded") }, "Reload configuration"),
        unloaded.length && !s.dry_run ? h("button", { type: "button", class: "btn", disabled: jobRunning(), onclick: () => startJob("/api/modules") }, "Install modules (terraform get)") : null)));

    parts.push(card("Privacy and safety",
      h("ul", { class: "bullets" },
        h("li", null, "Runs only on this machine; the web UI listens on localhost and requires a one-time token."),
        h("li", null, "No telemetry, no analytics, no external services. Infrastructure data is never uploaded."),
        h("li", null, "Discovery is read-only. See docs/iam-discovery-policy.json for a least-privilege policy."),
        h("li", null, "Nothing is imported until you confirm mappings, review a Terraform plan and explicitly approve it."),
        h("li", null, "Terraform itself performs the import; only plans that import without any other change can be applied."))));

    $("#view-scan").replaceChildren(h("div", { class: "cards" }, parts));
  }

  // ---------------------------------------------------------------- plan view

  const ACTION_LABEL = {
    import: "import", "import-update": "import + update", "import-replace": "import + REPLACE",
    create: "CREATE", update: "update", replace: "REPLACE", delete: "DESTROY", forget: "forget",
  };

  function renderPlan() {
    const s = state.session;
    const pv = state.preview;
    const parts = [];
    const sum = s.summary;

    parts.push(card("Readiness",
      h("div", { class: "readiness" },
        h("div", { class: "ready-item c-matched" }, h("strong", null, String(sum.importable)), h("span", null, "confirmed, will be imported")),
        h("div", { class: "ready-item c-matched" }, h("strong", null, String(sum.in_state)), h("span", null, "already in state")),
        h("div", { class: "ready-item c-review" }, h("strong", null, String(sum.review)), h("span", null, "need review")),
        h("div", { class: "ready-item c-unmatched" }, h("strong", null, String(sum.unmatched)), h("span", null, "unmatched")),
        h("div", { class: "ready-item c-ignored" }, h("strong", null, String(sum.ignored)), h("span", null, "ignored"))),
      sum.review + sum.unmatched > 0
        ? h("p", { class: "alert alert-warn" }, `${plural(sum.review + sum.unmatched, "resource")} are not resolved yet. You can import the confirmed ones now and continue later; unresolved resources are excluded from the plan.`)
        : h("p", { class: "alert alert-ok" }, "Every Terraform resource is mapped or deliberately ignored.")));

    if (!pv) {
      parts.push(card("Generated imports", h("p", { class: "muted" }, "Loading…")));
    } else {
      if (pv.excluded && pv.excluded.length) {
        parts.push(h("details", { class: "card" },
          h("summary", null, `${plural(pv.excluded.length, "resource")} excluded from the import`),
          h("p", { class: "muted" }, "The plan is limited with -target to the imported resources, so Terraform does not propose to create the excluded ones."),
          h("table", { class: "coverage" }, h("tbody", null, pv.excluded.map((e) => h("tr", null,
            h("td", null, statusIcon(e.status)), h("td", null, h("button", { type: "button", class: "linkish mono", onclick: () => { setView("review"); selectTF(e.address); } }, e.address)),
            h("td", { class: "muted" }, e.reason)))))));
      }
      parts.push(card(`Generated ${"recovery.import.tf"} (${plural((pv.specs || []).length, "import block")})`,
        pv.hcl ? h("pre", { class: "code" }, pv.hcl) : h("p", { class: "muted" }, "No confirmed mappings yet. Confirm mappings in the review step."),
        h("div", { class: "actions" },
          pv.hcl ? h("a", { class: "btn", href: "/api/download/imports.tf", download: "recovery.import.tf" }, "Download recovery.import.tf") : null,
          h("a", { class: "btn", href: "/api/download/mapping.json", download: "mapping.json" }, "Download mapping.json"))));

      const fileState = s.project.recovery_file_state;
      parts.push(card("Run terraform plan",
        h("p", null, "This writes ", h("code", null, "recovery.import.tf"), " into the project directory (a new, clearly marked file — your configuration files are never modified), backs up local state and lock files into a recovery snapshot, and runs ",
          h("code", null, "terraform init"), ", ", h("code", null, "validate"), " and ", h("code", null, "plan"), ". Planning changes nothing."),
        h("p", { class: "muted" }, s.project.terraform_profile
          ? `Terraform runs with AWS_PROFILE=${s.project.terraform_profile}, the profile used for discovery.`
          : "Terraform runs with the AWS credentials of the current environment (the default credential chain used for discovery)."),
        fileState === "foreign" ? h("p", { class: "alert alert-error" }, "A recovery.import.tf that was not generated by this tool exists in the project. It will not be overwritten; rename or remove it first.") : null,
        (pv.blockers || []).map((b) => h("p", { class: "alert alert-warn" }, b)),
        h("div", { class: "actions" },
          h("button", { type: "button", class: "btn btn-primary", disabled: !pv.can_plan || jobRunning(), onclick: () => startJob("/api/plan") },
            "Write recovery.import.tf and run terraform plan"))));
    }

    const job = jobPanel(["plan", "apply"]);
    if (job) parts.push(job);

    if (s.plan) parts.push(...planResult(s.plan));

    $("#view-plan").replaceChildren(h("div", { class: "cards" }, parts));
  }

  function planResult(p) {
    const out = [];
    const a = p.analysis;
    const body = [kv([["Plan", h("code", null, p.id)], ["Created", when(p.created_at)], ["Snapshot", h("code", null, p.dir)],
      ["Scope", p.targeted ? "limited with -target to the imported resources" : "whole configuration"]])];
    if (p.error) body.push(h("p", { class: "alert alert-error" }, `The plan did not complete: ${p.error}`));
    for (const v of p.validation || []) body.push(h("p", { class: "alert alert-error" }, `${v.summary}${v.detail ? " — " + v.detail : ""}${v.file ? ` (${v.file}:${v.line})` : ""}`));
    if (a) {
      body.push(h("div", { class: "plan-counts" },
        h("div", { class: "pc pc-import" }, h("strong", null, String(a.import)), h("span", null, "to import")),
        h("div", { class: `pc ${a.add ? "pc-bad" : ""}` }, h("strong", null, String(a.add)), h("span", null, "to add")),
        h("div", { class: `pc ${a.change ? "pc-bad" : ""}` }, h("strong", null, String(a.change)), h("span", null, "to change")),
        h("div", { class: `pc ${a.destroy ? "pc-bad" : ""}` }, h("strong", null, String(a.destroy)), h("span", null, "to destroy"))));
      if (p.can_apply) {
        body.push(h("p", { class: "alert alert-ok" }, `Import-only plan: ${plural(a.import, "existing resource")} will be recorded in the Terraform state. Nothing will be created, changed or destroyed.`));
      } else if (!p.applied) {
        body.push(h("div", { class: "alert alert-error big" }, h("strong", null, p.stale ? "⚠ Plan is out of date" : "⚠ Unexpected changes detected"), h("p", null, p.block_reason)));
      }
      for (const m of a.missing_imports || []) body.push(h("p", { class: "alert alert-warn" }, `Expected import not in the plan: ${m} (already in state?)`));
      for (const m of a.mismatched_imports || []) body.push(h("p", { class: "alert alert-error" }, `Import ID differs from the generated one: ${m}`));
      const changes = (a.resources || []).filter((r) => r.action !== "import");
      if (changes.length) {
        body.push(h("h4", null, "Planned changes other than imports"),
          h("table", { class: "coverage" }, h("thead", null, h("tr", null, h("th", null, "Resource"), h("th", null, "Action"), h("th", null, "Changed attributes"))),
            h("tbody", null, changes.map((r) => h("tr", { class: "bad" },
              h("td", null, h("code", null, r.address)),
              h("td", null, h("span", { class: `act act-${r.action}` }, ACTION_LABEL[r.action] || r.action)),
              h("td", null, (r.changed || []).join(", ") || (r.reason || ""))))),
          h("p", { class: "muted" }, "Adjust the configuration so it matches the real infrastructure (or correct the mapping), then plan again. Only import-only plans can be applied here.")));
      }
    }
    const textBox = h("pre", { class: "code" }, state.planText || "");
    body.push(h("details", {
      ontoggle: async (e) => {
        if (e.target.open && !state.planText) {
          try {
            state.planText = (await api("GET", `/api/plan/text?id=${q(p.id)}`)).text;
          } catch (err) {
            state.planText = err.message;
          }
          textBox.textContent = state.planText;
        }
      },
    }, h("summary", null, "Full terraform plan output"), textBox));
    out.push(card("Last plan", ...body));

    if (p.applied && p.apply) {
      const fileState = state.session.project.recovery_file_state;
      out.push(card("State recovered",
        h("p", { class: "alert alert-ok big" }, `✓ ${plural(p.apply.imported.length, "resource")} imported into the Terraform state.`),
        (p.apply.missing || []).length ? h("p", { class: "alert alert-error" }, `Not found in the state after apply: ${p.apply.missing.join(", ")}`) : null,
        h("p", null, "Next: run ", h("code", null, "terraform plan"), " in the project to confirm there are no remaining changes. Resources that were excluded still need attention."),
        fileState === "generated" ? h("div", { class: "actions" },
          h("button", { type: "button", class: "btn", onclick: () => act(() => api("POST", "/api/remove-recovery-file"), "recovery.import.tf removed") }, "Remove recovery.import.tf"),
          h("span", { class: "muted" }, "The import blocks are no longer needed. A copy stays in the recovery snapshot.")) : null));
    } else if (a && !p.error) {
      const ack = h("input", { type: "checkbox", id: "apply-ack", checked: state.applyAck, disabled: !p.can_apply, onchange: (e) => { state.applyAck = e.target.checked; renderAll(); } });
      out.push(card("Apply the import",
        h("label", { class: "check big-check", for: "apply-ack" }, ack,
          ` I reviewed this plan. Terraform will record ${plural(a.import, "existing resource")} in the state of the ${state.session.project.backend || "local"} backend. No infrastructure will be created, changed or destroyed.`),
        h("div", { class: "actions" },
          h("button", {
            type: "button", class: "btn btn-danger", disabled: !p.can_apply || !state.applyAck || jobRunning(),
            onclick: () => startJob("/api/apply", { plan_id: p.id, confirm: true }),
          }, "Apply import"),
          !p.can_apply ? h("span", { class: "muted" }, "Import has NOT been applied.") : h("span", { class: "muted" }, "Runs terraform apply with exactly this saved plan; the current state is backed up first."))));
    }
    return out;
  }

  // --------------------------------------------------------------------- init

  for (const btn of document.querySelectorAll("#steps button")) {
    btn.addEventListener("click", () => {
      setView(btn.dataset.view);
      if (state.view === "plan") {
        state.preview = null;
        refresh();
      } else {
        renderAll();
      }
    });
  }
  window.addEventListener("hashchange", () => {
    const v = location.hash.replace("#", "");
    if (["scan", "review", "plan"].includes(v) && v !== state.view) {
      state.view = v;
      refresh();
    }
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && state.view === "review" && !(e.target instanceof HTMLInputElement)) {
      state.tfSel = null;
      state.awsSel = null;
      state.detail = null;
      state.cloudDetail = null;
      state.pair = null;
      renderReview();
    }
  });
  refresh();
})();
