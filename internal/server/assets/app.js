const $ = (sel, el = document) => el.querySelector(sel);
const $$ = (sel, el = document) => [...el.querySelectorAll(sel)];

const state = { file: "nodes", slot: null, node: null, label: "" };

// ---- tiny helpers ---------------------------------------------------------
async function api(path, opts = {}) {
  const res = await fetch(path, { headers: { "Content-Type": "application/json" }, ...opts });
  const text = await res.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = text; }
  if (!res.ok) throw new Error(body && body.error ? body.error : `${res.status} ${res.statusText}`);
  return body;
}

function toast(msg, isErr = false) {
  const t = $("#toast");
  t.textContent = msg;
  t.className = "toast show" + (isErr ? " err" : "");
  clearTimeout(toast._t);
  toast._t = setTimeout(() => (t.className = "toast"), 3200);
}

const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const fmt = (n) => (n == null ? "-" : n.toLocaleString());

function propsTable(props) {
  if (!props || !Object.keys(props).length) return '<span class="muted">无属性</span>';
  return `<div class="kv">${Object.entries(props)
    .map(([k, v]) => `<span class="k">${esc(k)}</span><span class="mono">${esc(typeof v === "object" ? JSON.stringify(v) : v)}</span>`)
    .join("")}</div>`;
}

// ---- tabs -----------------------------------------------------------------
$$(".tabs button").forEach((b) =>
  b.addEventListener("click", () => {
    $$(".tabs button").forEach((x) => x.classList.toggle("active", x === b));
    $$(".tab").forEach((t) => t.classList.toggle("active", t.id === "tab-" + b.dataset.tab));
    if (b.dataset.tab === "records") loadPages();
  })
);

// ---- global header --------------------------------------------------------
async function loadStats() {
  try {
    const st = await api("/api/stats");
    const io = st.io || {};
    $("#stats").innerHTML = [
      `<span>节点 <b>${fmt(st.nodes)}</b></span>`,
      `<span>关系 <b>${fmt(st.rels)}</b></span>`,
      `<span>链描述符 <b>${fmt(st.descriptors)}</b></span>`,
      `<span>空闲槽 <b>${fmt(st.freeNodes + st.freeRels)}</b></span>`,
      `<span>nodes.dat <b>${fmt(st.nodeFileBytes)}</b> B</span>`,
      `<span>rels.dat <b>${fmt(st.relFileBytes)}</b> B</span>`,
      `<span>读盘页 <b>${fmt(io.pageReads)}</b></span>`,
      `<span>密度阈值 <b>${st.denseThreshold}</b> · 分组 <b>${st.grouping ? "开" : "关"}</b></span>`,
    ].join("");
  } catch (e) { toast(e.message, true); }
}

// ---- graph tab ------------------------------------------------------------
async function loadLabels() {
  const labels = await api("/api/labels");
  $("#label-filter").innerHTML = `<option value="">全部标签</option>` +
    Object.entries(labels).map(([l, c]) => `<option value="${esc(l)}">${esc(l)} (${c})</option>`).join("");
}

async function loadNodes() {
  const q = state.label ? `?label=${encodeURIComponent(state.label)}&limit=500` : "?limit=500";
  const data = await api("/api/nodes" + q);
  const search = $("#node-search").value.trim().toLowerCase();
  const items = data.nodes.filter((n) => !search || String(n.id).includes(search));
  $("#node-list").innerHTML = items
    .map(
      (n) => `<li data-id="${n.id}" class="${state.node === n.id ? "sel" : ""}">
        <span><span class="id">#${n.id}</span> ${esc(n.label)}</span>
        <span class="deg">${n.outCount}→ ${n.inCount}← ${n.groupedOut || n.groupedIn ? '<span class="badge dense">分组</span>' : ""}</span>
      </li>`
    )
    .join("");
  $$("#node-list li").forEach((li) => li.addEventListener("click", () => selectNode(Number(li.dataset.id))));
}

async function selectNode(id) {
  state.node = id;
  await loadNodes();
  const panel = $("#node-detail");
  panel.innerHTML = `<p class="muted">加载中…</p>`;
  let n, out, inn, groups;
  try {
    [n, out, inn, groups] = await Promise.all([
      api(`/api/nodes/${id}`),
      api(`/api/nodes/${id}/traverse?dir=out`),
      api(`/api/nodes/${id}/traverse?dir=in`),
      api(`/api/nodes/${id}/groups`),
    ]);
  } catch (e) { panel.innerHTML = `<p class="muted">${esc(e.message)}</p>`; return; }

  const relRows = (rels, dirLabel) => {
    if (!rels.length) return `<tr><td colspan="5" class="muted">无</td></tr>`;
    return rels.map((r) => {
      const other = dirLabel === "out" ? r.end : r.start;
      const p = Object.entries(r.props || {}).map(([k, v]) => k + "=" + v).join(", ");
      return `<tr>
        <td class="mono">${r.id}</td><td>${esc(r.type)}</td>
        <td class="mono">${r.start} → ${r.end}</td>
        <td><a href="#" data-goto="${other}" class="node-link">#${other}</a></td>
        <td class="mono">${esc(p)}</td></tr>`;
    }).join("");
  };

  panel.innerHTML = `
    <div class="section-title"><h3>节点 #${n.id}</h3>
      <div class="row" style="max-width:340px">
        <button id="add-rel">新增关系</button>
        <button id="del-node" class="ghost">删除节点</button>
      </div>
    </div>
    <div class="kv">
      <span class="k">标签</span><span>${esc(n.label)}</span>
      <span class="k">出度 / 入度</span><span class="mono">${n.outCount} / ${n.inCount}</span>
      <span class="k">分组 (出/入)</span><span>${n.groupedOut ? '<span class="badge dense">出：按类型分组</span>' : "出：单链"} ${n.groupedIn ? '<span class="badge dense">入：按类型分组</span>' : "入：单链"}</span>
      <span class="k">按类型计数</span><span class="mono">${esc(JSON.stringify({ out: n.outTypes || {}, in: n.inTypes || {} }))}${n.exactTypes ? "" : " (溢出)"}</span>
      <span class="k">属性</span><span>${propsTable(n.props)}</span>
    </div>
    <div class="section-title"><h3>出向关系 <span class="muted">(${out.rels.length})</span></h3></div>
    <table><thead><tr><th>关系</th><th>类型</th><th>起点→终点</th><th>对端</th><th>属性</th></tr></thead>
    <tbody>${relRows(out.rels, "out")}</tbody></table>
    <div class="section-title"><h3>入向关系 <span class="muted">(${inn.rels.length})</span></h3></div>
    <table><thead><tr><th>关系</th><th>类型</th><th>起点→终点</th><th>对端</th><th>属性</th></tr></thead>
    <tbody>${relRows(inn.rels, "in")}</tbody></table>
    <div class="section-title"><h3>存储链</h3></div>
    ${groups.length ? groups.map(chainCard).join("") : '<p class="muted">此节点没有关系链。</p>'}
  `;

  $$("#node-detail a[data-goto]").forEach((a) =>
    a.addEventListener("click", (ev) => { ev.preventDefault(); selectNode(Number(a.dataset.goto)); })
  );
  $$("#node-detail .chain .node-link").forEach((a) =>
    a.addEventListener("click", () => selectNode(Number(a.dataset.goto)))
  );
  $("#del-node").addEventListener("click", async () => {
    if (!confirm(`删除节点 #${n.id} 及其全部关系？`)) return;
    try {
      await api(`/api/nodes/${n.id}`, { method: "DELETE" });
      toast("节点已删除");
      state.node = null;
      await Promise.all([loadStats(), loadNodes()]);
      panel.innerHTML = '<p class="muted">节点已删除。</p>';
    } catch (e) { toast(e.message, true); }
  });
  $("#add-rel").addEventListener("click", () => addRelDialog(n.id));
}

function chainCard(g) {
  const dir = g.dir === "out" || g.dir === 0 ? "出向" : "入向";
  return `<div class="chain">
    <h4>${dir} · ${esc(g.type || "整方向单链")}</h4>
    <div class="meta">链描述符槽 <span class="mono">${g.descSlot}</span> · 长度 <b>${g.size}</b> · 首关系 <span class="mono">${g.firstRel || "-"}</span> · 尾关系 <span class="mono">${g.lastRel || "-"}</span></div>
  </div>`;
}

function addRelDialog(from) {
  const to = prompt("终点节点 id：");
  if (!to) return;
  const type = prompt("关系类型：", "collected");
  if (!type) return;
  api("/api/rels", { method: "POST", body: JSON.stringify({ start: from, end: Number(to), type }) })
    .then(() => { toast("关系已创建"); loadStats(); selectNode(from); })
    .catch((e) => toast(e.message, true));
}

$("#label-filter").addEventListener("change", (e) => { state.label = e.target.value; loadNodes(); });
$("#node-search").addEventListener("input", () => loadNodes());
$("#btn-demo").addEventListener("click", async () => {
  try {
    const res = await api("/api/demo", { method: "POST" });
    toast(res.loaded ? "示例数据已载入" : "已有数据，未载入：" + (res.reason || ""));
    await Promise.all([loadStats(), loadLabels(), loadNodes()]);
  } catch (e) { toast(e.message, true); }
});
$("#btn-compact").addEventListener("click", async () => {
  try {
    const res = await api("/api/compact", { method: "POST" });
    toast(`整理完成：移动 ${res.relsMoved} 条关系，回收 ${res.freedSlots} 个槽，形成 ${res.runs} 段连续`);
    await loadStats();
    if (state.node != null) selectNode(state.node);
    loadPages();
  } catch (e) { toast(e.message, true); }
});

// ---- records tab ----------------------------------------------------------
let pagesData = null;

async function loadPages() {
  const panel = $("#pages");
  panel.innerHTML = '<p class="muted">加载中…</p>';
  try { pagesData = await api("/api/storage/pages"); }
  catch (e) { panel.innerHTML = `<p class="muted">${esc(e.message)}</p>`; return; }
  const hdrs = pagesData.pages[state.file] || [];
  panel.innerHTML = hdrs.map((p) => {
    const pref = [...p.prefix].map((c) => {
      const cls = c === "N" ? "N" : c === "R" ? "R" : c === "C" ? "C" : "f";
      return `<span class="${cls}">${c === "." ? "·" : c}</span>`;
    }).join("");
    return `<div class="page" data-page="${p.page}">
      <span class="no">页 ${p.page}</span>
      <span class="pref">${pref}</span>
      <span class="deg">${p.liveRecords} 活 / ${p.freeRecords} 空</span>
    </div>`;
  }).join("");
  $$("#pages .page").forEach((el) => el.addEventListener("click", () => {
    $$("#pages .page").forEach((x) => x.classList.remove("sel"));
    el.classList.add("sel");
    showPage(Number(el.dataset.page));
  }));
  $("#slot-detail").innerHTML = `<p class="muted">文件 <b>${state.file}</b>：${hdrs.length} 页，每页 ${pagesData.pageSize} 字节 = ${pagesData.recordsPerPage} 条 ${pagesData.recordSize} 字节定长记录。选择一页查看解码后的记录。</p>`;
}

async function showPage(page) {
  const first = page * pagesData.recordsPerPage;
  const detail = $("#slot-detail");
  const isNodes = state.file === "nodes";
  let html = `<div class="section-title"><h3>页 ${page} · 记录 ${first}–${first + pagesData.recordsPerPage - 1}</h3></div><div class="pages">`;
  for (let i = 0; i < pagesData.recordsPerPage; i++) {
    const slot = first + i;
    try {
      const d = isNodes ? await api(`/api/storage/nodes/${slot}`) : await api(`/api/storage/rels/${slot}`);
      html += recordCard(d);
    } catch (e) {
      html += `<div class="chain"><h4>槽 ${slot}</h4><span class="muted">${esc(e.message)}</span></div>`;
    }
  }
  html += `</div>`;
  detail.innerHTML = html;
  $$("#slot-detail .node-link").forEach((a) =>
    a.addEventListener("click", (ev) => { ev.preventDefault(); selectNode(Number(a.dataset.goto)); })
  );
}

function recordCard(d) {
  const fields = d.fields.map((f) => {
    let v = esc(f.value);
    const isNode = ["startNode", "endNode", "outGroupHead", "inGroupHead"].includes(f.name);
    const isChainRef = ["outChain", "inChain", "first", "last", "dirPrev", "dirNext", "startPrev (OUT chain)", "startNext (OUT chain)", "endPrev (IN chain)", "endNext (IN chain)"].includes(f.name);
    if (f.value !== "-" && (isNode || isChainRef)) {
      v = `<a href="#" class="node-link" data-goto="${esc(f.value)}">${esc(f.value)}</a>`;
    }
    return `<div><span class="k">${esc(f.name)}</span> <span class="mono">${v}</span></div>`;
  }).join("");
  return `<div class="chain" data-slot="${d.slot}">
    <h4><span class="badge ${d.kind === "free" ? "" : "dense"}">${esc(d.kind)}</span> 槽 ${d.slot} · 页 ${d.page} · 页内偏移 ${d.offset}</h4>
    <div class="kv" style="font-size:12px">${fields}</div>
  </div>`;
}

$$(".seg button").forEach((b) => b.addEventListener("click", () => {
  $$(".seg button").forEach((x) => x.classList.toggle("active", x === b));
  state.file = b.dataset.file;
  loadPages();
}));

// ---- groups tab -----------------------------------------------------------
$("#group-load").addEventListener("click", loadGroups);

async function loadGroups() {
  const id = Number($("#group-node").value);
  const detail = $("#group-detail");
  detail.innerHTML = '<p class="muted">加载中…</p>';
  try {
    const [n, groups, out] = await Promise.all([
      api(`/api/nodes/${id}`),
      api(`/api/nodes/${id}/groups`),
      api(`/api/nodes/${id}/traverse?dir=out&limit=60`),
    ]);
    let html = `<div class="section-title"><h3>节点 #${id} · ${esc(n.label)}</h3>
      ${n.groupedOut || n.groupedIn ? '<span class="badge dense">已按类型分组</span>' : '<span class="badge">单链存储</span>'}</div>
      <div class="kv"><span class="k">出度 / 入度</span><span class="mono">${n.outCount} / ${n.inCount}</span>
      <span class="k">布局</span><span>${n.groupedOut || n.groupedIn ? "超过密度阈值，关系按类型 + 方向拆分为多条链" : "未超过密度阈值，每个方向一条链"}</span></div>`;
    if (!groups.length) html += `<p class="muted" style="margin-top:14px">此节点没有关系链。</p>`;
    else {
      html += `<div class="section-title"><h3>链描述符</h3></div>`;
      for (const g of groups) {
        const dirLabel = g.dir === "out" || g.dir === 0 ? "出向" : "入向";
        html += `<div class="chain">
          <h4>${dirLabel} · ${esc(g.type || "整方向单链")} <span class="muted">(${g.size} 条)</span></h4>
          <div class="meta">描述符槽 <span class="mono">${g.descSlot}</span> · 首 <span class="mono">${g.firstRel || "-"}</span> · 尾 <span class="mono">${g.lastRel || "-"}</span></div>
          <div class="flow">${chainFlow(id, g, out.rels)}</div>
        </div>`;
      }
    }
    html += `<div class="section-title"><h3>按类型过滤的遍历代价</h3></div>
      <p class="hint">选择一种关系类型，观察遍历只进入对应链时读取的页数。分组后过滤某类型不会扫其他类型的关系。</p>
      <div class="row" style="max-width:420px">
        <select id="type-pick"><option value="">全部类型</option>${[...new Set(groups.map((g) => g.type).filter(Boolean))].map((t) => `<option>${esc(t)}</option>`).join("")}</select>
        <button id="type-run">遍历</button></div>
      <div id="type-out"></div>`;
    detail.innerHTML = html;
    $("#type-run").addEventListener("click", async () => {
      const t = $("#type-pick").value;
      const q = `dir=both&trace=1${t ? "&type=" + encodeURIComponent(t) : ""}`;
      const res = await api(`/api/nodes/${id}/traverse?${q}`);
      $("#type-out").innerHTML = `<div class="io">返回 <b>${res.rels.length}</b> 条关系 · 触及页 <b>${res.io.pageTouches}</b> · 读盘页 <b>${res.io.pageReads}</b> · 不同页 <b>${res.io.distinctPages}</b> · 读记录 <b>${res.io.recordsRead}</b></div>
        <div class="trace">${(res.steps || []).map((s) => `<div>${s.dirName} · 链类型 ${esc(s.type || "全部")} · 描述符槽 ${s.descSlot} · 长度 ${s.chainLen}${s.visited ? " · 遍历 " + s.visited : ""}</div>`).join("")}</div>`;
    });
  } catch (e) {
    detail.innerHTML = `<p class="muted">${esc(e.message)}</p>`;
  }
}

function chainFlow(nodeID, g, rels) {
  const matching = rels.filter((r) => (g.type === "" ? true : r.type === g.type));
  const seq = matching.slice(0, 14);
  if (!seq.length) return '<span class="muted">（关系不在前 60 条加载范围内）</span>';
  let html = `<span class="mono">#${nodeID}</span>`;
  for (const r of seq) {
    const other = g.dir === "out" || g.dir === 0 ? r.end : r.start;
    html += `<span class="arrow">→</span><span class="node-link" data-goto="${other}">#${other}</span>`;
  }
  if (matching.length > seq.length) html += `<span class="arrow">…</span>`;
  return html;
}

// ---- query tab ------------------------------------------------------------
$("#q-run").addEventListener("click", runQuery);

async function runQuery() {
  const from = Number($("#q-from").value);
  const to = Number($("#q-to").value);
  const dir = $("#q-dir").value;
  const depth = Number($("#q-depth").value);
  const types = $("#q-types").value.trim();
  const withRels = $("#q-rels").checked;
  const detail = $("#query-detail");
  detail.innerHTML = '<p class="muted">展开中…</p>';
  const params = new URLSearchParams({ from, to, dir, depth, withRels: withRels ? "1" : "0" });
  if (types) params.set("types", types);
  try {
    const res = await api(`/api/paths?${params}`);
    let html = `<div class="section-title"><h3>#${from} → #${to}</h3><span class="badge">${res.paths.length} 条路径</span></div>`;
    if (res.truncated) html += `<p class="hint">路径数超过上限，结果已截断。</p>`;
    if (!res.paths.length) html += `<p class="muted">在深度 ${depth} 内没有找到满足条件的路径。</p>`;
    for (const p of res.paths.slice(0, 80)) {
      html += `<div class="path">
        <div class="nodes">${p.nodes.map((n, i) => `<a href="#" class="node-link" data-goto="${n}">#${n}</a>${i < p.nodes.length - 1 ? ' <span class="arrow">→</span> ' : ""}`).join("")}</div>
        ${p.rels ? `<div class="rels">经过关系：${p.rels.map((r) => `<span class="mono">${r}</span>`).join(", ")}</div>` : ""}
      </div>`;
    }
    if (res.paths.length > 80) html += `<p class="muted">还有 ${res.paths.length - 80} 条路径未显示。</p>`;
    const steps = res.steps || [];
    if (steps.length) {
      const byDesc = {};
      for (const s of steps) {
        const key = `${s.nodeId}|${s.dir}|${s.type || "全部"}|${s.descSlot}|${s.chainLen}`;
        byDesc[key] = Math.max(byDesc[key] || 0, s.visited || 0);
      }
      html += `<div class="section-title"><h3>展开过程</h3></div>
        <p class="hint">路径枚举从起点逐层展开。下表列出本次展开进入过的链：分组链只包含对应类型，单链则是该方向的全部关系。</p>
        <table><thead><tr><th>展开节点</th><th>方向</th><th>链类型</th><th>描述符槽</th><th>链长</th><th>访问</th></tr></thead><tbody>
        ${Object.entries(byDesc).map(([k, visited]) => {
          const parts = k.split("|");
          const [node, d, t, slot, chainLen] = parts;
          return `<tr><td class="mono">#${esc(node)}</td><td>${d === "0" ? "出" : "入"}</td><td>${esc(t)}</td><td class="mono">${slot}</td><td class="mono">${chainLen}</td><td class="mono">${visited}</td></tr>`;
        }).join("")}</tbody></table>`;
    }
    html += `<div class="io">读盘页 <b>${res.io.pageReads}</b> · 触及页 <b>${res.io.pageTouches}</b> · 不同页 <b>${res.io.distinctPages}</b> · 读记录 <b>${res.io.recordsRead}</b></div>`;
    detail.innerHTML = html;
    $$("#query-detail .node-link").forEach((a) =>
      a.addEventListener("click", (ev) => { ev.preventDefault(); selectNode(Number(a.dataset.goto)); })
    );
  } catch (e) {
    detail.innerHTML = `<p class="muted">${esc(e.message)}</p>`;
  }
}

// ---- boot -----------------------------------------------------------------
(async function boot() {
  await Promise.all([loadStats(), loadLabels(), loadNodes()]);
})();
