// omnigate 管理面板前端（原生 JS，无构建步骤，直接 fetch 管理 API）
"use strict";

const $ = (id) => document.getElementById(id);

// ---------- API 封装 ----------
// 管理 API 会话：cookie 为 HttpOnly 不读（fetch 需 same-origin 自动携带），
// 前端只持有「口令指纹」用于判断登录态（session=state.auth_session）。
let authSession = "";
async function api(path, body) {
  const opts = { method: "GET", headers: { "Content-Type": "application/json" }, credentials: "same-origin" };
  if (body !== undefined) {
    opts.method = "POST";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opts);
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    if (resp.status === 401 && data.need_login) showLogin(); // 会话失效：即时弹登录层
    throw new Error(data.error || ("请求失败 " + resp.status));
  }
  return data;
}

// ---------- 管理面板登录 ----------
// 注意：id 一律带 admin 前缀，与「添加账号」弹层（loginOverlay/loginMsg）语义不同，切勿混用。
// 已渲染过面板数据时不再只盖一层遮罩，而是整页重载：彻底清掉 DOM/内存里的账号与密钥
// （登出或会话失效后残留展示不安全）。首次加载（state 为 null）不重载，避免死循环。
function showLogin(msg) {
  const hadData = state !== null;
  state = null;
  if (hadData) { location.reload(); return; }
  $("adminLoginOverlay").classList.remove("hidden");
  $("adminLoginErr").textContent = msg || "";
  $("adminLoginPass").focus();
}

async function submitLogin() {
  const pass = $("adminLoginPass").value;
  if (!pass) { $("adminLoginErr").textContent = "请输入管理员密码"; return; }
  $("btnAdminLogin").disabled = true;
  try {
    const r = await api("/api/auth/login", { password: pass });
    authSession = r.session || "";
    $("adminLoginPass").value = "";
    $("adminLoginOverlay").classList.add("hidden");
    await loadState();
    await loadFees();
  } catch (e) {
    $("adminLoginErr").textContent = e.message;
  } finally {
    $("btnAdminLogin").disabled = false;
  }
}

async function logout(msg) {
  try { await api("/api/auth/logout", {}); } catch (e) { /* 会话已失效也继续收敛到登录层 */ }
  authSession = "";
  showLogin(msg || "已退出登录");
}

// ensureSession 重新探测登录态：未登录弹登录层，否则加载面板数据。
// 用于密码变更/清空后重新同步（改密码会作废旧会话，清空密码则鉴权消失）。
async function ensureSession(msg) {
  try {
    const st = await api("/api/auth/state");
    if (st.auth_enabled && !st.auth_session) { showLogin(msg); return; }
    authSession = st.auth_session || "";
  } catch (e) { showLogin(msg); return; }
  await loadState();
}

// ---------- 工具 ----------
function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

let toastTimer = null;
function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add("hidden"), 3000);
}

function shortUid(uid) {
  if (!uid) return "";
  return uid.length <= 12 ? uid : uid.slice(0, 6) + "…" + uid.slice(-4);
}

// ---------- 全局状态 ----------
let state = null;

// ---------- 数据加载 ----------
async function loadState() {
  state = await api("/api/state");
  // 账号数据已变（刷新/签到/增删），明细缓存随之失效，避免 tooltip 展示旧余额。
  detailCache = {};
  render();
}

// 轮询「调用中」状态。
//
// 为什么单独拉 /api/state 而不是复用 loadState：loadState 会清空 detailCache
// 并全量 render()，高频调用会让用户的积分 tooltip 反复消失、正在编辑的输入框
// 被重绘打断。而这个功能只需要 in_use 这一个字段。
//
// 为什么仍走 /api/state 而不加专用端点：/api/state 是纯读、无副作用，且当前
// 账号规模下开销可忽略（几十 KB JSON）。为它新增端点反而多一处要维护的契约。
async function pollInUse() {
  // 用户正在输入时不要动 DOM：重渲染会把光标位置和已输入内容弄乱。
  const t = document.activeElement;
  if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA")) return;
  try {
    const st = await api("/api/state");
    const accs = st.accounts || [];
    // 只改徽标与高亮，不整体重建 DOM —— 保住 hover 态与滚动位置。
    for (const el of document.querySelectorAll(".acct-card")) {
      const uid = el.dataset.uid;
      const a = accs.find((x) => x.uid === uid);
      if (!a) continue;
      el.classList.toggle("in-use", !!a.in_use);
      const box = el.querySelector(".acct-live-slot");
      if (!box) continue;
      box.innerHTML = liveTagHtml(a);
    }
  } catch (e) { /* 轮询失败静默：不该因瞬时网络抖动弹错误 */ }
}

// liveTagHtml 与 renderAccounts 里保持一致（供 render + pollInUse 共用，避免两处失同步）。
function liveTagHtml(a) {
  return a.in_use
    ? `<span class="live-badge" title="该账号正在处理请求"><span class="live-dot"></span>调用中</span>`
    : (a.last_used_at
      ? `<span class="used-badge" title="最近调用：${esc(a.last_used_at)}">最近用过</span>`
      : "");
}

async function loadFees() {
  try {
    const fees = await api("/api/fees");
    renderFees(fees);
  } catch (e) { /* 费率接口失败不阻塞 */ }
}

async function refreshFees() {
  $("btnRefreshFees").disabled = true;
  try {
    await api("/api/fees/refresh", {});
    const fees = await api("/api/fees");
    renderFees(fees);
    toast("模型列表和费率已刷新");
  } catch (e) { toast(e.message); } finally {
    $("btnRefreshFees").disabled = false;
  }
}

// ---- 一键添加自定义模型（写入 WorkBuddy 的 models.json）----
//
// 写入是**整体覆盖**用户的自定义清单，故必须先预览（拿真实的「会覆盖多少条」），
// 再由用户确认；后端也要求显式 confirm=true 才落盘。
let mjPreview = null;

// exportModelsJson 预览 → 弹覆盖警告 → 用户确认后写入。
async function exportModelsJson() {
  let pv;
  try {
    pv = await api("/api/fees/models_json");
  } catch (e) { toast(e.message); return; }

  if (!pv.api_key_set) {
    toast("未设置 API Key，无法写入自定义模型");
    return;
  }
  if (!pv.will_write) {
    toast("没有可写入的模型：请先点「刷新」获取倍率数据");
    return;
  }
  mjPreview = pv;
  $("mjMsg").innerHTML = modelsJsonConfirmHTML(pv);
  $("modelsJsonOverlay").classList.remove("hidden");
}

// modelsJsonConfirmHTML 组装确认文案。覆盖与新建两种情形的措辞完全不同 ——
// 「会覆盖你 N 条自定义模型」必须显式说出来，不能含糊成「将更新」。
function modelsJsonConfirmHTML(pv) {
  const rows = [];
  rows.push(`<div class="glm-warn">
    <div class="glm-warn-title">⚠️ ${pv.exists ? "这会覆盖已有的自定义模型清单" : "即将新建自定义模型清单"}</div>
    <ol>
      ${pv.exists
        ? `<li>目标文件已存在，其中 <b>${pv.existing_count}</b> 条自定义模型将被<b>整体覆盖</b>。</li>`
        : "<li>目标文件不存在，将新建。</li>"}
      <li>写入前会自动备份为 <code>models.json.bak-时间戳</code>（同目录），但仍建议你自己先备份一份。</li>
      <li>写入后需在 WorkBuddy 里<b>重启或刷新</b>模型列表才会看到。</li>
    </ol>
  </div>`);
  rows.push(`<div class="hint" style="margin-top:0">
    将写入 <b>${pv.will_write}</b> 个模型${pv.skipped ? `（另有 ${pv.skipped} 个无倍率模型不写入）` : ""}<br>
    文件：<code>${esc(pv.path)}</code><br>
    端点：<code>${esc(pv.endpoint)}</code>
  </div>`);
  return rows.join("");
}

// confirmModelsJson 确认写入（后端同样要求 confirm=true，双保险）。
async function confirmModelsJson() {
  if (!mjPreview) { closeModelsJson(); return; }
  $("btnMjConfirm").disabled = true;
  try {
    const r = await api("/api/fees/models_json", { confirm: true });
    closeModelsJson();
    toast(r.note || `已写入 ${r.count} 条自定义模型`);
  } catch (e) {
    toast(e.message);
  } finally {
    $("btnMjConfirm").disabled = false;
  }
}

function closeModelsJson() {
  mjPreview = null;
  $("modelsJsonOverlay").classList.add("hidden");
}

// ---------- 积分明细 tooltip ----------
// 明细条数可能很多（TraeWork 每个签到奖励都是独立条目，常见 30+），
// 故分页展示：每页 DETAIL_PAGE_SIZE 条，页码状态存在 detailState 里，
// 翻页不重新请求（detailCache 已缓存该账号的完整响应）。
const DETAIL_PAGE_SIZE = 8;
let detailTimer = null;
let detailCache = {};
// detailState 当前 tooltip 的展示状态：{uid, page}。
// detailState 当前 tooltip 的展示状态：{uid, page}。
let detailState = null;

async function showCreditDetail(e, uid) {
  const el = e.currentTarget;
  if (detailTimer) { clearTimeout(detailTimer); detailTimer = null; }

  // 切换到另一个账号时重置页码；同一账号重复 hover 保留原页码。
  const page = (detailState && detailState.uid === uid) ? detailState.page : 0;

  let d = detailCache[uid];
  if (!d) {
    try {
      d = await api("/api/account/resource_detail", { uid });
      detailCache[uid] = d;
    } catch (err) { return; }
  }
  if (!d || !d.items || !d.items.length) return;

  let tip = $("creditTip");
  if (!tip) {
    tip = document.createElement("div");
    tip.id = "creditTip";
    tip.className = "credit-tip";
    document.body.appendChild(tip);
  }

  const rect = el.getBoundingClientRect();
  detailState = { uid, page };
  renderCreditDetail();
  // 先渲染再量尺寸，才能决定向上还是向下弹出。
  const h = tip.offsetHeight;
  let top = rect.bottom + 4;
  if (top + h > window.innerHeight) top = Math.max(4, rect.top - h - 4);
  let left = rect.left;
  if (left + tip.offsetWidth > window.innerWidth) left = Math.max(4, window.innerWidth - tip.offsetWidth - 10);
  tip.style.left = left + "px";
  tip.style.top = top + "px";
}

// renderCreditDetail 按 detailState 重绘 tooltip（含分页控件与可用/不可用小计）。
function renderCreditDetail() {
  const tip = $("creditTip");
  if (!tip || !detailState) return;
  const d = detailCache[detailState.uid];
  if (!d || !d.items || !d.items.length) return;

  const items = d.items;
  const pages = Math.max(1, Math.ceil(items.length / DETAIL_PAGE_SIZE));
  const page = Math.min(Math.max(0, detailState.page), pages - 1);
  detailState.page = page;
  const slice = items.slice(page * DETAIL_PAGE_SIZE, (page + 1) * DETAIL_PAGE_SIZE);

  // 有效期列仅当上游确实下发了到期时间时才出现——渠道未返回则不显示该列，
  // 避免一列全空或把「无到期信息」误读成「永不过期」。
  const hasExpiry = items.some((it) => it.expire_at);

  // 头部：翻页器在顶部居中，标题与条数分列两端（用户方案：翻页不跨出浮窗）。
  let html = `<div class="detail-head">`;
  html += `<span class="detail-count">共 ${items.length} 条</span>`;
  if (pages > 1) {
    html += `<span class="detail-pager">`;
    html += `<span class="detail-pg${page === 0 ? " off" : ""}" data-pg="${page - 1}">‹</span>`;
    html += `<span class="detail-pg-info">${page + 1} / ${pages}</span>`;
    html += `<span class="detail-pg${page >= pages - 1 ? " off" : ""}" data-pg="${page + 1}">›</span>`;
    html += `</span>`;
  }
  html += `<span class="detail-title">积分明细</span></div>`;
  html += `<table class="detail-table"><thead><tr><th>套餐</th><th>总额</th><th>已用</th><th>剩余</th>`;
  if (hasExpiry) html += `<th>有效期</th>`;
  html += `</tr></thead><tbody>`;
  for (const it of slice) {
    // 不可用额度整行淡显 + 角标，与可用额度区分开（如 TraeWork 的官方客户端专用池）。
    const cls = it.usable ? "" : ' class="detail-unusable"';
    let tag = "";
    if (!it.usable) {
      tag = '<span class="detail-tag" title="该额度仅供官方客户端使用，本工具无法消耗">不可用</span>';
    } else if (it.info_only) {
      // 单位与积分不同（如 MonkeyCode 的每日 Token 额度）：展示但不计入合计。
      tag = '<span class="detail-tag" title="单位与积分不同，仅作展示，不计入合计">仅展示</span>';
    }
    html += `<tr${cls}><td>${esc(it.name)}${tag}</td><td>${it.total}</td><td>${it.used}</td><td>${it.remain}</td>`;
    if (hasExpiry) html += `<td>${it.expire_at ? esc(it.expire_at) : "-"}</td>`;
    html += `</tr>`;
  }
  html += `</tbody></table>`;

  // 小计行：只在确实存在不可用额度时才拆开展示，否则保持单数字（不制造无意义的 0）。
  const usable = d.usable_remain || 0;
  const unusable = d.unusable_remain || 0;
  html += `<div class="detail-sum">`;
  html += `<span>可用 <b>${usable}</b></span>`;
  if (unusable > 0) html += `<span class="detail-sum-unusable">不可用 <b>${unusable}</b></span>`;
  html += `</div>`;
  tip.innerHTML = html;
  tip.style.display = "block";

  // 翻页：只改状态重绘，不重新请求接口。事件重挂由 hideCreditDetail 统一负责。
  tip.querySelectorAll(".detail-pg").forEach((btn) => {
    if (btn.classList.contains("off")) return;
    btn.onclick = (ev) => {
      ev.stopPropagation();
      if (detailTimer) { clearTimeout(detailTimer); detailTimer = null; } // 翻页即取消关闭
      const target = Number(btn.dataset.pg);
      if (Number.isFinite(target)) { detailState.page = target; renderCreditDetail(); }
    };
  });
}

function hideCreditDetail() {
  detailTimer = setTimeout(() => {
    const tip = $("creditTip");
    if (tip) tip.style.display = "none";
  }, 300);
  const tip = $("creditTip");
  if (tip) {
    tip.onmouseenter = () => { if (detailTimer) { clearTimeout(detailTimer); detailTimer = null; } };
    tip.onmouseleave = () => { tip.style.display = "none"; };
  }
}

// ---------- 渲染 ----------
function render() {
  renderTopbar();
  renderSummary();
  renderAccounts();
  applyAcctFilter();
  refreshTabBalances();
  renderTimes();
}

// renderTopbar 顶栏 API 地址渲染：
// - 监听 127.0.0.1/localhost → 原样显示；
// - 监听 0.0.0.0/::/空（所有网卡）→ 显示局域网 IP（lan_ip），客户端可跨机接入；
//   取不到局域网 IP 时兑底 127.0.0.1；
// - 其他自定义地址 → 原样显示。
// 此前 0.0.0.0 被硬换成 127.0.0.1，局域网客户端复制到的是本机环回地址，无法跨机接入。
function renderTopbar() {
  $("ver").textContent = "v" + state.version;
  $("serverLine").textContent = state.running ? "服务运行中" : "服务未启动";
  $("aboutVer").textContent = state.version;

  const wildcard = state.listen_host === "0.0.0.0" || state.listen_host === "" || state.listen_host === "::";
  const host = wildcard ? (state.lan_ip || "127.0.0.1")
    : (state.listen_host === "localhost" ? "127.0.0.1" : state.listen_host);
  const apiURL = `http://${host}:${state.listen_port}/v1`;
  $("apiAddr").querySelector(".val").textContent = apiURL;
  // 监听所有网卡时提示真实含义（避免误导为仅本机可用）
  $("apiAddr").title = wildcard
    ? `监听 0.0.0.0（所有网卡），局域网可用；点击复制`
    : "点击复制地址";

  const key = state.api_key;
  $("apiKeyDisplay").querySelector(".val").textContent = key === "" ? "（无鉴权）" : key;
}

// 渠道显示名与 CSS 短类名（后端 group / 费率 channel 均为 provider.Kind）。
const CH_LABEL = { workbuddy: "WorkBuddyCN", workbuddyai: "WorkBuddyAI", traework: "TraeWork", traecode: "TraeCode", qoder: "Qoder", qodercn: "QoderCN", qodercom: "QoderCOM", qwenwork: "千问办公", glm: "智谱清言", monkeycode: "MonkeyCode", raccoon: "小浣熊", loomy: "Loomy", minimax: "MiniMax", cline: "Cline" };
const CH_CLASS = { workbuddy: "wb", workbuddyai: "wbai", traework: "trae", traecode: "traecode", qoder: "qoder", qodercn: "qodercn", qodercom: "qodercom", qwenwork: "qwenwork", glm: "glm", monkeycode: "monkeycode", raccoon: "raccoon", loomy: "loomy", minimax: "minimax", cline: "cline" };
const chLabel = (k) => CH_LABEL[k] || "WorkBuddy";
const chClass = (k) => CH_CLASS[k] || "wb";
// 不支持显式签到（手动按钮）的渠道：
// WorkBuddy 国际版不提供手动签到，而是自动对话保活领日活奖励；
// 千问办公无签到活动；MonkeyCode / Loomy 无签到端点。
// 小浣熊**不在此列**（R61）：上游没有领取端点，每日 300 积分是当天首次触达积分服务时
// 懒发放的，「签到」= 摸一次余额 + 核对流水，幂等且不消耗额度，故给出手动按钮。
const NO_EXPLICIT_CHECKIN = new Set(["workbuddyai", "qwenwork", "monkeycode", "loomy"]);
const noExplicitCheckin = (g) => NO_EXPLICIT_CHECKIN.has(g);
// 导入型渠道：凭据由本机已登录的官方客户端提供，没有浏览器登录流程（见 internal/app/import_local.go）。
const IMPORT_LOCAL_CHANNELS = new Set(["monkeycode", "raccoon", "loomy"]);
const isImportLocal = (ch) => IMPORT_LOCAL_CHANNELS.has(ch);
// 支持「浏览器授权登录」的渠道：登录期间临时把该渠道的自定义协议回调指向本工具，
// 以便接住授权码并完成 token 兑换。
// 小浣熊两个集合都命中 —— 弹窗里同时给「浏览器授权登录」与「从客户端导入」两个动作。
const PROTOCOL_LOGIN_CHANNELS = new Set(["raccoon"]);
const hasProtocolLogin = (ch) => PROTOCOL_LOGIN_CHANNELS.has(ch);
// 无手动签到渠道的状态文案：国际版是「自动领日活奖励」，千问办公为「无签到」。
const NO_CHECKIN_TAG = { workbuddyai: "自动领日活奖励" };
const noCheckinText = (g) => NO_CHECKIN_TAG[g] || "无签到";
// 无积分概念的渠道：积分区域显示「不适用」，并隐藏刷新积分/明细入口。
// 当前所有渠道都有积分概念，故为空集；机制保留以备将来接入匿名通道。
const NO_CREDITS = new Set();

// creditsText 账号卡片的积分文案。
// 拆成「可用 / 不可用 / 临期」三个数字：渠道（如 TraeWork）会下发官方客户端专用的
// 额度池，对本工具是看得见用不了的，混进一个数字会让人误判可用余额；
// 临期是可消耗余额中 24h 内（到期日≤明天）到期的部分，提示优先消耗。
// 不可用仅在 >0 时显示；临期同理，凭空多个灰/红 0 很吵。
// 旧版本 state 文件（v2.2.0 及之前，无 unusable 字段）读入后 credits_stale=true，
// 此时不把旧值当真值，改显示「待刷新」；自动刷新首刷成功后即变回真实拆分。
function creditsText(a) {
  if (a.credits_na) {
    return `<span class="credit-na" title="匿名通道无积分概念">不适用</span>`;
  }
  if (a.credits_stale) {
    return `<span class="credit-stale" title="余额口径已过期（旧版本状态文件），正在自动刷新…">待刷新</span>`;
  }
  let html = `<span class="credit-num">${a.credits}</span><span>可用积分</span>`;
  if ((a.expiring_credits || 0) > 0) {
    html += `<span class="credit-expiring"> (临期${a.expiring_credits})</span>`;
  }
  if ((a.unusable_credits || 0) > 0) {
    html += `<span class="credit-unusable"> (不可用${a.unusable_credits})</span>`;
  }
  return html;
}

// 渠道汇总固定顺序（与添加按钮布局一致；traecode 是别名池不单独出现，qoder 下线仅存量兜底）
const CH_SUMMARY_ORDER = ["workbuddy", "workbuddyai", "traework", "qodercn", "qodercom", "qwenwork", "glm", "qoder"];

// renderSummary 渠道积分汇总条：每渠道一行「可用总积分 / 临期」，末尾合计。
// 数据全部来自 state.accounts（pool 实时值），与账号卡片同源；
// 口径与 R17 一致：不可用额度只作角标、不进合计；credits_na（匿名）不参与合计。
function renderSummary() {
  const el = $("creditSummary");
  if (!el) return;
  if (!state.accounts.length) { el.classList.add("hidden"); return; }
  el.classList.remove("hidden");

  const byCh = {};
  for (const a of state.accounts) {
    const g = a.group || "workbuddy";
    let c = byCh[g];
    if (!c) { c = byCh[g] = { n: 0, credits: 0, expiring: 0, unusable: 0, stale: 0, na: false }; }
    c.n++;
    if (a.credits_na) { c.na = true; continue; }
    c.credits += a.credits || 0;
    c.expiring += a.expiring_credits || 0;
    c.unusable += a.unusable_credits || 0;
    if (a.credits_stale) c.stale++;
  }

  const fmt = (n) => n.toLocaleString("zh-CN");
  // 已知渠道按固定顺序，未知渠道（新增时）按字母序垫后，保证不丢行
  const chs = CH_SUMMARY_ORDER.filter((k) => byCh[k])
    .concat(Object.keys(byCh).filter((k) => !CH_SUMMARY_ORDER.includes(k)).sort());

  let total = 0, totalExp = 0, staleChs = 0;
  let html = `<span class="cs-title">积分汇总</span>`;
  for (const k of chs) {
    const c = byCh[k];
    const allStale = c.stale === c.n;
    // R17 口径：口径过期的数字不当真值——全渠道待刷新时不进合计
    // （否则"待刷新"的旧值会悄悄抬高合计，用户看到的数字无从解释）。
    if (!c.na && !allStale) { total += c.credits; totalExp += c.expiring; }
    if (!c.na && allStale) staleChs++;
    let body;
    if (c.na) {
      body = `<span class="credit-na">不适用</span>`;
    } else if (allStale) {
      body = `<span class="credit-stale">待刷新</span>`;
    } else {
      body = `<span class="cs-num">${fmt(c.credits)}</span>`
        + (c.expiring > 0 ? `<span class="cs-exp" title="24 小时内到期">临期${fmt(c.expiring)}</span>` : "")
        + (c.unusable > 0 ? `<span class="cs-unu" title="账号名下、本工具不可消耗的额度（不计入合计）">不可用${fmt(c.unusable)}</span>` : "")
        + (c.stale > 0 ? `<span class="cs-stale" title="部分账号余额待刷新">${c.stale} 待刷新</span>` : "");
    }
    const on = acctFilter === k ? " on" : "";
    html += `<span class="cs-chip clickable${on}" data-wg="${esc(k)}" role="button" tabindex="0"`
      + ` title="点击筛选该渠道账号，再次点击取消"><span class="badge ${chClass(k)}">${esc(chLabel(k))}</span>`
      + `<span class="cs-n">${c.n}号</span>${body}</span>`;
  }
  // 「全部」重置项：仅在有筛选时出现，点击清除筛选。整条不参与合计，故放在合计 chip 之前。
  if (acctFilter) {
    html += `<span class="cs-chip clickable cs-all" data-wg="" role="button" tabindex="0" title="显示全部账号">`
      + `<span class="cs-n">全部</span></span>`;
  }
  html += `<span class="cs-chip cs-total" title="各渠道可用积分合计（不含「不可用」与匿名通道）">`
    + `<span class="cs-title">合计</span><span class="cs-num">${fmt(total)}</span>`
    + (totalExp > 0 ? `<span class="cs-exp">临期${fmt(totalExp)}</span>` : "")
    + (staleChs > 0 ? `<span class="cs-stale" title="有渠道整体待刷新，未计入合计">${staleChs} 渠道待刷新</span>` : "")
    + `</span>`;
  el.innerHTML = html;
}

// acctFilter 账号筛选状态：null = 全部；否则为渠道 key（provider.Kind）。
// 汇总条 chip 与账号卡片同源于 state.accounts，点某个渠道 chip 即筛选其账号卡片。
let acctFilter = null;

// applyAcctFilter 按 acctFilter 显示/隐藏账号卡片（与汇总条 chip 联动）。
// 逐张卡片读徽章的渠道类名反查（卡片本身不带 data-group，避免改动 renderAccounts 的既有结构）。
function applyAcctFilter() {
  const grid = $("acctList");
  if (!grid) return;
  grid.querySelectorAll(".acct-card").forEach((card) => {
    const badge = card.querySelector(".acct-top .badge");
    const key = badge ? (CLS_KIND[badge.classList[1]] || "") : "";
    const show = !acctFilter || key === acctFilter;
    card.classList.toggle("hidden", !show);
  });
}

// CLS_KIND 徽章 CSS 短类名 → 渠道 key 的反查表（与 CH_CLASS 互逆）。
const CLS_KIND = {};
for (const k of Object.keys(CH_CLASS)) CLS_KIND[CH_CLASS[k]] = k;

// setAcctFilter 设置筛选并同步汇总条高亮与账号卡片可见性。
function setAcctFilter(kind) {
  acctFilter = (acctFilter === kind) ? null : kind; // 再次点击同一渠道 = 取消
  renderSummary();
  applyAcctFilter();
}

// bindSummaryFilter 汇总条 chip 点击/键盘筛选（事件委托，绑定一次）。
function bindSummaryFilter() {
  const el = $("creditSummary");
  if (!el) return;
  const pick = (target) => {
    const chip = target.closest(".cs-chip.clickable");
    if (!chip) return;
    setAcctFilter(chip.dataset.wg || null);
  };
  el.addEventListener("click", (e) => pick(e.target));
  el.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") { e.preventDefault(); pick(e.target); }
  });
}
function renderAccounts() {
  const grid = $("acctList");
  const empty = $("acctEmpty");
  if (!state.accounts.length) {
    grid.innerHTML = "";
    empty.classList.remove("hidden");
    return;
  }
  empty.classList.add("hidden");

  // 本轮任务带来的余额增量（uid → 增加分数），在卡片上打 +N 角标。
  // 只算一次：renderAccounts 逐卡片渲染，多次调用会重复遍历全部账号。
  const gained = creditDelta();

  grid.innerHTML = state.accounts.map((a) => {
    const group = chClass(a.group);
    const groupName = chLabel(a.group);
    const noCheckin = noExplicitCheckin(a.group); // 无显式签到，签到按钮灰掉
    const noCheckinTitle = a.group === "workbuddyai"
      ? "无需手动签到：定时自动对话保活并领取日活奖励"
      : `${groupName} 不支持手动签到`;

    const checkinTag = a.last_checkin_at
      ? `<span class="tag ${a.last_checkin_ok ? "ok" : "bad"}">${a.last_checkin_ok ? "签到成功" : "签到失败"}</span>`
      : (noCheckin ? `<span class="tag neutral" title="${esc(noCheckinTitle)}">${noCheckinText(a.group)}</span>` : '<span class="tag neutral">未签到</span>');

    const disabledClass = a.disabled ? " disabled" : "";
    const disableIcon = a.disabled ? "▶" : "⏸";
    const disableTitle = a.disabled ? "启用" : "停用";

    // 冷却标签（后端 pool.Status 已透出 cooling/until/reason，此前前端未渲染）。
    // 为什么需要：账号被冷却时挑号会跳过它，表现为「积分明明很多却报 503/上游限流」，
    // 而卡片上毫无提示——用户只能靠猜。这里把「冷却到几点 + 原因」直接摆出来。
    // until 由后端 fmtTime 预格式化为 "01-02 15:04"，为空则退化为「冷却中」。
    const coolingTag = a.cooling
      ? `<span class="tag warn" title="${esc(a.reason || "冷却中")}">冷却至 ${esc(a.until || "-")}</span>`
      : "";
    const coolingClass = a.cooling ? " cooling" : "";
    const checkinBtn = noCheckin
      ? `<span class="icon-op off" title="${esc(noCheckinTitle)}" onclick="return false">✓</span>`
      : `<span class="icon-op" title="签到" onclick="checkin('${a.uid}')">✓</span>`;

    // 匿名渠道（无积分/无签到）：只保留「不可操作」的静态指示，
    // 不给刷新积分/停用/删除入口——后端也会硬拒，避免用户白点一次。
    const noCredits = NO_CREDITS.has(a.group);
    // 成长任务入口：仅成长中心支持的渠道显示（目前只有 WorkBuddy 国内版）。
    // 用后端下发的 a.growth 判定，不在前端硬编码渠道名。
    const taskBtn = a.growth
      ? `<span class="icon-op" title="执行成长任务（会真实改变账号状态）" onclick="runTasks('${a.uid}')">★</span>`
      : "";
    const ops = noCredits
      ? `<span class="icon-op off" title="固定账号，不可停用/删除" onclick="return false">🔒</span>`
      : `${checkinBtn}
          ${taskBtn}
          <span class="icon-op" title="刷新积分" onclick="refreshOne('${a.uid}')">↻</span>
          <span class="icon-op warn" title="${disableTitle}" onclick="toggleDisable('${a.uid}',${a.disabled})">${disableIcon}</span>
          <span class="icon-op danger" title="删除账号" onclick="removeAcct('${a.uid}')">✕</span>`;
    // 显示名：点击直接弹出改名框（无积分渠道降级为普通文本）。
    const nameHtml = noCredits
      ? `<span class="acct-name">${esc(a.nickname || shortUid(a.uid))}</span>`
      : `<span class="acct-name editable" title="点击修改显示名" onclick="openRename('${a.uid}','${esc(a.nickname || shortUid(a.uid)).replace(/'/g, "&#39;")}')">${esc(a.nickname || shortUid(a.uid))}</span>`;

    // 「调用中 / 最近用过」徽标。HTML 由 liveTagHtml 统一生成，
    // renderAccounts 与 pollInUse 共用，避免两处写法漂移。
    //
    // in_use 由后端按 pool.InUseWindow 推断（流式调用无结束信号，用时间窗近似）。
    // 前端不做时间推算 —— 面板可能因后台标签页节流而刷新变慢，本地算时间会与后端口径漂移。
    const liveTag = `<span class="acct-live-slot">${liveTagHtml(a)}</span>`;

    return `
    <div class="acct-card${disabledClass}${coolingClass}${a.in_use ? " in-use" : ""}" data-uid="${esc(a.uid)}">
      <div class="acct-top">
        <div>
          <span class="badge ${group}">${groupName}</span>
          ${nameHtml}
          ${liveTag}
        </div>
        <div class="acct-ops">
          ${ops}
        </div>
      </div>
      <div class="acct-uid">UID: ${esc(shortUid(a.uid))}</div>
      <div class="acct-mid">
        <div class="acct-credits"${noCredits ? "" : ` onmouseenter="showCreditDetail(event,'${a.uid}')" onmouseleave="hideCreditDetail()"`}>${creditsText(a)}</div>
        <div class="acct-checkin">${gained[a.uid] ? `<span class="tag ok" title="本轮任务带来的积分增量">+${gained[a.uid]}</span>` : ""}${coolingTag}${checkinTag}</div>
      </div>
    </div>`;
  }).join("");
}

function renderTimes() {
  const box = $("timesBox");
  box.innerHTML = (state.checkin_times || []).map((t) =>
    `<span class="time-chip" title="点击删除" onclick="delTime('${t}')">${t} ✕</span>`).join("");
  $("nextCheckin").textContent = state.next_checkin || "-";
}

// channelCredits 汇总某渠道全部账号的可用积分（与积分汇总条同源 state.accounts）。
// 无该渠道账号时返回 null（前端不渲染角标，避免显示误导性的 0）。
function channelCredits(kind) {
  if (!state || !state.accounts) return null;
  let sum = 0, has = false;
  for (const a of state.accounts) {
    if ((a.group || "workbuddy") !== kind) continue;
    if (a.credits_na) return null; // 匿名渠道无积分概念
    has = true;
    sum += a.credits || 0;
  }
  return has ? sum : null;
}

// fmtNum 千分位整数（与汇总条的 fmt 同口径，独立命名避免作用域冲突）。
function fmtNum(n) {
  return n.toLocaleString("zh-CN");
}

// refreshTabBalances 就地更新费率渠道 tab 的积分余额角标。
// 账号余额刷新远频于费率表（state 每次 loadState 都更新，fees 只在加载/手动刷新时变），
// 若只在 renderFees 里写余额，账号刷新后角标会停在旧值。这里在每次 render() 时
// 只改角标文本（不重建 DOM），既保持最新又不打断用户浏览/滚动。
function refreshTabBalances() {
  const box = $("feesBox");
  if (!box) return;
  box.querySelectorAll(".fees-tab[data-feech]").forEach((tab) => {
    const bal = channelCredits(tab.dataset.feech);
    const el = tab.querySelector(".fees-tab-bal");
    if (bal === null) {
      if (el) el.remove();
      return;
    }
    const txt = `余额 ${fmtNum(bal)} 分`;
    if (el) { if (el.textContent !== txt) el.textContent = txt; }
    else { const s = document.createElement("span"); s.className = "fees-tab-bal"; s.textContent = txt; s.title = "该渠道全部账号当前可用积分合计"; tab.appendChild(s); }
  });
}

function renderFees(fees) {
  // 缓存最近一次结果：搜索重渲染要用（applyFeeSearch 依赖它，不必再请求接口）。
  renderFees.lastData = fees;
  const box = $("feesBox");
  const channels = fees.channels || [];
  // 「显示隐藏模型」开关状态要反映到按钮上：每次重渲染都同步（切 tab、搜索都会走到这里），
  // 否则开关在重新拉数据后会视觉回退成未按下，与实际筛选状态不一致。
  syncShowAllBtn();

  if (channels.length === 0) {
    box.innerHTML = `<div class="note">${esc(fees.note || "")}</div>
      <div class="note">${esc(fees.disclaimer || "")}</div>`;
    return;
  }
  let html = `<div class="note">${esc(fees.note || "")}</div>`;
  if (fees.cached_at) html += `<div class="note">费率上次更新：${esc(fees.cached_at)}</div>`;
  if (fees.error) html += `<div class="note" style="color:var(--danger)">${esc(fees.error)}</div>`;

  const UNKNOWN_TIP = "上游未返回，请在客户端自行确认";

  // 能力图标：模型 ID 后的小标记，title 属性提供文字描述。
  // 只展示上游明确声明的能力；未声明的（字段缺失或上游返回 false）不显示图标。
  // tool_calls 不展示：几乎所有模型都支持，图标信息量低。
  const capIcons = (m) => {
    if (!m) return "";
    const caps = [];
    if (m.supports_images) {
      caps.push(`<span class="cap-icon cap-img" title="支持图像输入（多模态视觉）：可直接发送图片给该模型">👁</span>`);
    }
    if (m.supports_reasoning) {
      caps.push(`<span class="cap-icon cap-reason" title="支持思考/推理模式：回复前会进行推理（可能含 reasoning_content）">🧠</span>`);
    }
    return caps.length > 0 ? ` <span class="cap-icons">${caps.join("")}</span>` : "";
  };

  // 上下文标记：模型 ID 后的 (1M)/(180K) 小字标。
  // 只在上游接口真实返回时展示（has_context），不拿估算值充数。
  const ctxTag = (m) => {
    if (!m || !m.has_context || !m.context_window) return "";
    return ` <span class="ctx-tag" title="上下文窗口：${fmtTokens(m.context_window)} tokens">(${fmtTokens(m.context_window)})</span>`;
  };

  // 能力文字摘要，拼进模型 tooltip。
  // 措辞说明：上游模型列表接口未声明某能力时，本工具不自行断言其「不支持」，
  // 只说「未声明」——避免把缺失信息当成否定结论。
  const capText = (m) => {
    if (!m) return null;
    const yes = [], unknown = [];
    (m.supports_images ? yes : unknown).push("图像输入");
    (m.supports_reasoning ? yes : unknown).push("思考模式");
    const parts = [];
    if (yes.length) parts.push(`支持：${yes.join("、")}`);
    if (unknown.length) parts.push(`上游未声明：${unknown.join("、")}`);
    return parts.join("；");
  };

  // 模型 id 的 tooltip：能拿到上下文则展示，否则明确说未知
  const modelTip = (m) => {
    const parts = [`模型：${m.model}`];
    if (m.has_context && m.context_window) {
      parts.push(`上下文窗口：${fmtTokens(m.context_window)}`);
      if (m.max_tokens) parts.push(`最大输出：${fmtTokens(m.max_tokens)}`);
    } else {
      parts.push("上下文窗口：未知");
      parts.push("最大输出：未知");
      parts.push("（上游未提供该信息）");
    }
    const ct = capText(m);
    if (ct) parts.push(ct);
    return parts.join("\n");
  };

  // 单行倍率单元格
  const rateCell = (m) => {
    if (!m) return "";
    if (!m.priced) {
      return `<span class="rate-unknown" title="${esc(UNKNOWN_TIP)}">unknown</span>`;
    }
    if (m.free) {
      return `<span class="rate-free" title="上游标注为免费（x0.00）">✦ Free</span>`;
    }
    return `<span class="rate-paid">x${m.rate.toFixed(2)}</span>`;
  };

  // 促销标签：使用上游给的颜色（原本被拼在文案里没解析）
  const noteCell = (m) => {
    if (!m || !m.note) return "";
    const style = m.color ? ` style="color:${esc(m.color)}"` : "";
    return ` <span class="rate-note"${style}>${esc(m.note)}</span>`;
  };

  // 模型标识单元格：模型名 + 上下文/能力/促销标记。
  // 模型名可点击复制。复制内容 = **带倍率后缀**的完整标识（如
  // `workbuddy/glm-5.3-flash (0.06x)`）—— 后端路由会剥掉后缀，所以粘到客户端
  // 仍能正常请求，而用户在客户端里能看到「这个烧多少积分」。
  // 无倍率（上游未返回定价）的模型复制裸名，不用 0 冒充。
  // data-mid 存复制内容供事件委托读取；data-plain 存裸名（显示/tooltip 用）。
  const modelIdCell = (m, kind) => {
    if (!m) return "";
    const plain = `${kind}/${m.model}`;
    // 有明确倍率才加后缀；free(0.00) 也要显示 —— 用户需要知道「这个免费」。
    const copy = m.priced ? plain + ` (${m.rate.toFixed(2)}x)` : plain;
    const copyTip = `点击复制：${copy}`;
    const title = `${modelTip(m)}\n${copyTip}`;
    return `<code class="fee-model-copy" data-mid="${esc(copy)}" data-plain="${esc(plain)}" role="button" tabindex="0" title="${esc(title)}">${esc(m.model)}</code>`
      + `${ctxTag(m)}${capIcons(m)}${noteCell(m)}`;
  };

  // 渠道多标签：每个渠道一个 tab，panel 内双列模型布局不变（issue：费率表太长）。
  // 记住上次选中的渠道，重渲染后自动恢复（后台刷新不打断用户浏览）。
  if (!renderFees.lastCh) renderFees.lastCh = channels[0].channel;
  // 选中的渠道若已不在列表里（渠道被移除），回退到第一个
  if (!channels.some((c) => c.channel === renderFees.lastCh)) {
    renderFees.lastCh = channels[0].channel;
  }
  const active = renderFees.lastCh;

  html += `<div class="fees-tabs">`;
  for (const ch of channels) {
    // tab 上的数字必须与表里**实际行数**同口径，否则用户按数字核对会以为漏了模型。
    // 三种口径（与下方表格筛选逻辑一一对应，改一处必须同步另一处）：
    //   搜索态  → 本渠道命中数（0 表示该渠道无命中，正是搜索时判断该看哪个 tab 的信号）
    //   展开态  → 全部模型数
    //   默认态  → 仅上游给了倍率的模型数
    // 早期版本搜索态仍显示默认态计数（搜出 2 行却标 20），看着像「模型清单不全」。
    const all = ch.models || [];
    // R65：上游压根不提供倍率的渠道（MiniMax/Cline/GLM 等），整组模型都是 unknown。
    // 这类渠道若仍按「只显示有倍率的行」筛选，默认态会一行不剩 → 面板看着像没模型。
    // 因此整渠道无倍率时默认显示全部（倍率列标 unknown，见下方说明）。
    const allUnpriced = all.length > 0 && !all.some((m) => m && m.priced);
    const searching = !!feeQuery();
    const n = searching
      ? all.filter((m) => m && feeMatch(ch.channel, m)).length
      : all.filter((m) => m && (m.priced || allUnpriced || showAllModels())).length;
    const on = ch.channel === active ? " active" : "";
    // 渠道积分余额角标：取自 state.accounts（与积分汇总条同源），
    // 让用户在选择渠道时即可看到「这个渠道还有多少积分可用」。
    const bal = channelCredits(ch.channel);
    const balHtml = bal === null ? ""
      : `<span class="fees-tab-bal" title="该渠道全部账号当前可用积分合计">余额 ${fmtNum(bal)} 分</span>`;
    html += `<button class="fees-tab${on}" data-feech="${esc(ch.channel)}"
      title="${esc(chLabel(ch.channel))}">${esc(chLabel(ch.channel))}<span class="fees-tab-n">${n}</span>${balHtml}</button>`;
  }
  html += `</div>`;

  for (const ch of channels) {
    const chCls = chClass(ch.channel);
    const all = ch.models || [];
    // 本表的用途是「比价」：没有倍率的模型（unknown）在这里没有信息量，
    // 且多为上游的内部/子代理模型（browser_use_subagent、computer_use_subagent 等，
    // 它们本就不按 token 计费，所以费率接口不返回）。
    // 因此默认只渲染有倍率的行，隐藏数量在表下注明 —— 避免用户以为模型清单缺了东西：
    // 接口与客户端看到的始终是完整清单（/v1/models 不受影响）。
    // 「显示隐藏模型」开关打开后，这些模型一并显示（用户要的是**完整清单**，比如
    // 确认某个子代理模型到底存不存在），此时不再需要隐藏说明。
    //
    // 搜索态无条件显示命中项：用户搜的就是具体模型，没倍率也该看到
    // （并明确标注 unknown），否则搜了等于没搜。此时按钮不参与筛选 ——
    // 搜索结果已是全部命中项，再叠加开关只会让用户以为按钮失灵。
    const searching = !!feeQuery();
    const allUnpriced = all.length > 0 && !all.some((m) => m && m.priced);
    const models = searching ? all.filter((m) => m && feeMatch(ch.channel, m))
                             : all.filter((m) => m && (m.priced || allUnpriced || showAllModels()));
    const unpriced = all.length - all.filter((m) => m && m.priced).length;
    const hidden = ch.channel !== active ? ' class="hidden"' : '';
    html += `<div class="fees-panel${hidden ? ' hidden' : ''}" data-feepanel="${esc(ch.channel)}">`;
    if (models.length === 0) {
      // 搜索无结果时给出明确说明，避免用户以为「这个渠道没有模型」。
      html += `<div class="note">${searching ? "该渠道没有匹配「" + esc(feeQuery()) + "」的模型" : "暂无可显示模型"}</div></div>`;
      continue;
    }
    html += `<table><thead><tr><th>模型</th><th>倍率</th><th>模型</th><th>倍率</th></tr></thead><tbody>`;
    for (let i = 0; i < models.length; i += 2) {
      const m1 = models[i];
      const m2 = models[i + 1];
      html += `<tr><td>${modelIdCell(m1, ch.channel)}</td><td>${rateCell(m1)}</td><td>${modelIdCell(m2, ch.channel)}</td><td>${rateCell(m2)}</td></tr>`;
    }
    html += `</tbody></table>`;
    if (!searching && allUnpriced) {
      // R65：整渠道无倍率 —— 说清楚「不是没模型，是这家的上游不发倍率」，
      // 并指出真实花费在哪看（流水/运行统计），别让用户以为面板坏了。
      html += `<div class="note" style="margin-top:6px">该渠道上游不提供倍率接口，故全部模型显示 unknown；模型本身可正常调用，实际积分/费用消耗见「流水」与「运行统计」。</div>`;
    } else if (!searching && unpriced > 0) {
      html += showAllModels()
        ? `<div class="note" style="margin-top:6px">上方标「上游未返回」的 ${unpriced} 个模型倍率缺失（多为内部/子代理模型），仍可正常调用，只是无法比价。</div>`
        : `<div class="note" style="margin-top:6px">本渠道另有 ${unpriced} 个模型上游未提供倍率（多为内部/子代理模型），已从本表隐藏；接口与客户端不受影响，仍可正常调用。点上方「显示隐藏模型」可展开。</div>`;
    }
    html += `</div>`;
  }

  html += `<div class="note" style="margin-top:8px">${esc(fees.disclaimer || "")}</div>`;
  box.innerHTML = html;
}

// ---- 「显示隐藏模型」开关 ----
//
// 默认态只列上游给了倍率的模型（比价表要干净）。但「干净」的代价是用户
// 无法确认某些模型**是否存在** —— 比如 browser_use_subagent 这类内部/子代理
// 模型，只在隐藏列表里出现过一次，用户会怀疑自己记错了。
// 这个开关把完整清单交还给用户，同时保持默认态不变（多数人不需要它）。
//
// 持久化到 localStorage：这是**用户的阅读偏好**（临时看一眼 vs 长期对照清单），
// 每次开面板重新点一遍没有意义。
const SHOW_ALL_KEY = "fees-show-all";

// showAllModels 当前是否展开隐藏模型。
function showAllModels() {
  try { return localStorage.getItem(SHOW_ALL_KEY) === "1"; } catch (e) { return false; }
}

function setShowAllModels(on) {
  try { localStorage.setItem(SHOW_ALL_KEY, on ? "1" : "0"); } catch (e) { /* 忽略 */ }
  syncShowAllBtn();
}

// syncShowAllBtn 把开关状态反映到按钮外观与文案。
// 文案带数量：用户点之前就知道会多出多少行，避免点开才发现是几十个内部模型。
function syncShowAllBtn() {
  const btn = $("btnShowAllModels");
  if (!btn) return;
  const on = showAllModels();
  btn.classList.toggle("on", on);
  let hidden = 0;
  const data = renderFees.lastData;
  if (data) {
    for (const ch of data.channels || []) {
      for (const m of ch.models || []) if (m && !m.priced) hidden++;
    }
  }
  btn.textContent = on ? "隐藏无倍率模型" : "显示隐藏模型";
  btn.title = on
    ? `当前已显示全部 ${hidden} 个无倍率模型。点击收起，仅保留有倍率的可比价项`
    : `上游有 ${hidden} 个模型未提供倍率（多为内部/子代理模型），默认隐藏。点击可查看完整清单`;
}

// toggleShowAllModels 点击切换并重渲染。
function toggleShowAllModels() {
  const next = !showAllModels();
  setShowAllModels(next);
  // 清掉搜索词：搜索态下开关不参与筛选（结果已是全部命中项），
  // 保留搜索词会让切换看起来「完全没反应」，是典型的交互误导。
  const el = $("feeSearch");
  if (el && el.value) el.value = "";
  const data = renderFees.lastData;
  if (data) renderFees(data);
}

// ---- 费率表搜索 ----
//
// 135 个模型分散在十几个渠道 tab 里，靠翻找很痛苦。加一个即时过滤框：
// 按模型名 / 渠道名 / 完整 ID（含倍率）匹配。
//
// 搜索态的两处特殊处理（与默认态不同，勿混）：
//   1. **保留倍率 unknown 的条目** —— 用户搜的是具体模型，没倍率也该看到
//      （并明确标 unknown）；默认态隐藏它们是为了让「比价」表干净。
//   2. **自动切到有匹配的渠道 tab** —— 否则用户输入关键词却仍停在当前 tab，
//      看到的还是不匹配的列表（最容易被当成「搜索坏了」）。

// feeQuery 当前搜索词（小写；空串 = 未搜索）。
function feeQuery() {
  const el = $("feeSearch");
  return el ? el.value.trim().toLowerCase() : "";
}

// feeMatch 某渠道下某模型是否命中搜索词。
function feeMatch(channel, m) {
  const q = feeQuery();
  if (!q) return false;
  if ((m.model || "").toLowerCase().includes(q)) return true;
  if ((channel || "").toLowerCase().includes(q)) return true;
  // 完整 ID（含倍率后缀）也算可搜：用户可能直接粘 `workbuddy/glm-5.3-flash (0.06x)`。
  const full = `${channel}/${m.model}` + (m.priced ? ` (${m.rate.toFixed(2)}x)` : "");
  return full.toLowerCase().includes(q);
}

// applyFeeSearch 搜索变化时重渲染。
// 只在「当前 tab 无匹配、但别的 tab 有」时切 tab —— 不无条件抢走用户的选择。
function applyFeeSearch() {
  const data = renderFees.lastData;
  if (!data) return;
  if (feeQuery()) {
    const chans = data.channels || [];
    const has = (ch) => (ch.models || []).some((m) => m && feeMatch(ch.channel, m));
    if (!has(renderFees.lastCh) && chans.some(has)) {
      renderFees.lastCh = chans.find(has).channel;
    }
  }
  renderFees(data);
}

// bindFeesTabs 费率渠道标签点击切换（事件委托，绑定一次）。
// 渲染只改 active 类与面板可见性，不重建 DOM，避免滚动位置跳动。
function bindFeesTabs() {
  const box = $("feesBox");
  box.addEventListener("click", (e) => {
    // 模型名点击复制优先判定：code.fee-model-copy 不是 .fees-tab，互不冲突。
    const code = e.target.closest("code.fee-model-copy");
    if (code && box.contains(code)) {
      copyText(code.dataset.mid, "模型名");
      return;
    }
    const btn = e.target.closest(".fees-tab");
    if (!btn) return;
    const ch = btn.dataset.feech;
    renderFees.lastCh = ch;
    box.querySelectorAll(".fees-tab").forEach((x) => x.classList.toggle("active", x === btn));
          box.querySelectorAll(".fees-panel").forEach(function (p) {
        if (p.dataset.feepanel === ch) { p.classList.remove("hidden"); }
        else { p.classList.add("hidden"); }
      });
  });
  // 键盘可达：模型名支持 Enter/Space 复制（与 role="button" 对应）。
  box.addEventListener("keydown", (e) => {
    if (e.key !== "Enter" && e.key !== " ") return;
    const code = e.target.closest("code.fee-model-copy");
    if (code && box.contains(code)) { e.preventDefault(); copyText(code.dataset.mid, "模型名"); }
  });
}

// fmtTokens 把 token 数格式化为 1M / 192k 形式。
function fmtTokens(n) {
  if (!n) return "-";
  if (n >= 1000000 && n % 1000000 === 0) return `${n / 1000000}M`;
  if (n >= 1000) return `${Math.round(n / 1000)}k`;
  return String(n);
}

// ---------- 账号操作 ----------
async function checkin(uid) {
  try {
    const r = await api("/api/account/checkin", { uid });
    // 未达成且可重试（活动尚未创建/上游瞬时故障）：说明会在签到窗口内自动重试，
    // 避免用户看到「无可用签到活动」误以为失败。
    if (!r.ok && r.retryable) toast(`暂未领到：${r.msg}（10:00–12:00 窗口内会自动重试）`);
    else toast(r.ok ? `签到成功：${r.msg}（剩余 ${r.remain}）` : `签到：${r.msg}`);
    loadState();
  } catch (e) { toast(e.message); }
}

async function refreshOne(uid) {
  try {
    const r = await api("/api/account/refresh", { uid });
    toast(`刷新成功：剩余积分 ${r.remain}`);
    loadState();
  } catch (e) { toast(e.message); }
}

async function toggleDisable(uid, currentlyDisabled) {
  const action = currentlyDisabled ? "启用" : "停用";
  confirmDialog(`确定${action}该账号？${currentlyDisabled ? "" : "停用后路由不会分配给该账号。"}`, async () => {
    try {
      await api("/api/account/disable", { uid, disabled: !currentlyDisabled });
      toast(`账号已${action}`);
      loadState();
    } catch (e) { toast(e.message); }
  });
}

function removeAcct(uid) {
  confirmDialog("确定删除该账号？删除后需重新登录。", async () => {
    try {
      await api("/api/account/remove", { uid });
      toast("账号已删除");
      loadState();
    } catch (e) { toast(e.message); }
  });
}

// ---------- 批量操作 ----------
async function checkinAll() {
  $("btnCheckinAll").disabled = true;
  try {
    const r = await api("/api/account/checkin_all", {});
    const ok = (r.results || []).filter((x) => x.ok).length;
    const pending = (r.results || []).filter((x) => !x.ok && x.retryable).length;
    const skip = (state.accounts || []).filter((a) => noExplicitCheckin(a.group)).length;
    const total = (r.results || []).length + skip;
    const parts = [skip ? `成功 ${ok} / ${total}（${skip} 个无签到活动跳过）` : `成功 ${ok} / 共 ${total}`];
    if (pending) parts.push(`${pending} 个未领到将在窗口内自动重试`);
    toast(`批量签到完成：${parts.join("；")}`);
    loadState();
  } catch (e) { toast(e.message); } finally {
    $("btnCheckinAll").disabled = false;
  }
}

async function refreshAll() {
  $("btnRefreshAll").disabled = true;
  try {
    const r = await api("/api/account/refresh_all", {});
    toast(r.busy ? "已有刷新任务进行中" : `积分刷新完成：成功 ${r.ok} / 失败 ${r.failed}`);
    loadState();
  } catch (e) { toast(e.message); } finally {
    $("btnRefreshAll").disabled = false;
  }
}

// ---------- 成长任务「一键完成」 ----------
//
// 为什么做成「异步 + 轮询」而不是等一个长请求：
// 单账号要发几十个请求（含真实对话、专家召唤链），多账号可到数分钟，
// 同步 fetch 会被超时掐断，用户看到的是「点了没反应」。
let taskPollTimer = null;
// creditsBefore 本轮任务执行前各账号余额（uid → credits）；用于展示积分增量。
let creditsBefore = null;

// 任务状态图标与配色类（与 style.css 的 .tk-* 对应）。
const TASK_STATUS_META = {
  claimed:     { ico: "✓", cls: "tk-claimed", text: "已领奖" },
  done:        { ico: "•", cls: "tk-done",    text: "已完成" },
  error:       { ico: "✗", cls: "tk-error",   text: "失败" },
  unsupported: { ico: "–", cls: "tk-manual",  text: "渠道不支持" },
  manual:      { ico: "·", cls: "tk-manual",  text: "需手动" },
  skipped:     { ico: "–", cls: "tk-skipped", text: "跳过" },
};

// runTasks 触发「一键完成」。uid 为空 = 全部 WorkBuddy 账号。
//
// 必须二次确认：这些动作会**真实改变账号状态**（点亮进度并领奖），
// 且事件形状来自第三方逆向成果，本项目未独立实测。
async function runTasks(uid) {
  const who = uid ? "该账号" : "全部支持成长中心的账号";
  const msg =
    `即将对${who}执行成长任务「一键完成」。\n\n` +
    "⚠️ 这会真实改变账号状态：点亮任务进度、领取奖励（不可撤销）。\n" +
    "动作靠模拟客户端行为实现，首次建议只对一个账号执行，核对哪些任务真的点亮后再全量跑。\n\n" +
    "耗时可能数十秒到数分钟（有真实对话与请求间隔），期间可在账号列表上方查看进度。\n\n" +
    "确定继续？";
  if (!confirm(msg)) return;
  // 记录执行前的余额，用于结束后高亮「涨了多少」
  snapshotCredits();
  try {
    await api("/api/account/tasks", { uid: uid || "", include_mp: false });
    toast("任务已开始执行，进度显示在账号列表上方");
    startTaskPolling();
  } catch (e) { toast(e.message); }
}

// snapshotCredits 记录当前各账号余额（uid → credits）。
// 用于任务结束后展示积分增量；任务不涉及奖励时不会产生噪音。
function snapshotCredits() {
  creditsBefore = {};
  (state.accounts || []).forEach((a) => { creditsBefore[a.uid] = a.credits || 0; });
}

// creditDelta 返回本轮各账号的积分增量（只含 >0 的）。
function creditDelta() {
  const out = {};
  if (!creditsBefore || !state) return out;
  (state.accounts || []).forEach((a) => {
    const d = (a.credits || 0) - (creditsBefore[a.uid] ?? (a.credits || 0));
    if (d > 0) out[a.uid] = d;
  });
  return out;
}

// startTaskPolling 轮询任务进度；进行中每 2.5s 刷一次。
function startTaskPolling() {
  if (taskPollTimer) return;
  const tick = async () => {
    try {
      const v = await api("/api/account/tasks_status");
      renderTaskPanel(v);
      if (v.running) {
        taskPollTimer = setTimeout(tick, 2500);
      } else {
        taskPollTimer = null;
        finishTaskRun(v);
      }
    } catch (e) {
      taskPollTimer = null;
      toast("任务状态查询失败：" + e.message);
    }
  };
  tick();
}

// finishTaskRun 任务结束后的收尾：刷新积分并提示到账。
//
// 为什么必须调刷新接口：`/api/state` 里的 credits 读自**后端缓存**
// （pool.SetCreditDetail 写入），任务执行本身不会改它 —— 只 loadState()
// 会看到领奖前的旧值，表现为「任务做完了但积分没变」。
//
// 后端在 runTasks 末尾也会刷一遍（覆盖无头/定时场景），这里是给页面兜底：
// 后端刷新失败、或用户在任务跑到一半时打开页面时，仍能拿到正确余额。
async function finishTaskRun(v) {
  const got = (v && v.credit) || 0;
  const energy = (v && v.energy) || 0;
  let refreshed = null;
  try {
    const r = await api("/api/account/refresh_all", {});
    if (!r.busy) refreshed = r;
  } catch (e) {
    toast("任务已完成，但积分刷新失败：" + e.message);
  }
  await loadState();
  // 真实增量（刷新后的余额 − 执行前余额）。用它比用任务自报的 credit 更可信：
  // 后者只统计「本次领奖」，若期间还有签到/其他变动会与余额对不上。
  const delta = creditDelta();
  let gained = 0;
  Object.values(delta).forEach((n) => { gained += n; });
  renderTaskPanel(v, delta);
  if (refreshed) {
    const tail = `积分已刷新（成功 ${refreshed.ok} / 失败 ${refreshed.failed || 0}）`;
    if (gained > 0) toast(`任务完成：余额 +${gained} 分` + (energy ? ` +${energy} 能` : "") + `；${tail}`);
    else if (got || energy) toast(`任务完成：领奖 +${got} 分 +${energy} 能；${tail}`);
    else toast(`任务完成（本次余额无变化）；${tail}`);
  }
}

// renderTaskPanel 渲染任务执行面板（进度或结果）。
// delta 为可选的「本轮余额增量」（uid → 增加的分数），用于按账号标注实际收益。
function renderTaskPanel(v, delta) {
  const box = $("taskPanel");
  if (!v || (!v.running && !(v.reports || []).length && !v.err)) {
    box.classList.add("hidden");
    box.innerHTML = "";
    return;
  }
  box.classList.remove("hidden");
  delta = delta || {};

  let head = "";
  if (v.running) {
    const cur = v.current ? ` · ${esc(v.current)}` : "";
    head = `<span class="tk-title">成长任务执行中</span>
      <span class="tk-run">${v.accounts_done}/${v.accounts_total} 个账号${cur}</span>
      <span class="tk-spacer"></span>
      <span class="tk-sum">已完成项会实时显示</span>`;
  } else {
    const bad = (v.reports || []).some((r) => r.err);
    head = `<span class="tk-title">成长任务结果</span>
      <span class="${bad ? "tk-err" : "tk-ok"}">到账 +${v.credit} 分 +${v.energy} 能</span>
      <span class="tk-spacer"></span>
      <span class="tk-sum">${esc(v.finished_at || "")}</span>
      <button class="btn small" id="btnTaskClose">关闭</button>`;
  }

  const accounts = (v.reports || []).map((r) => {
    const name = r.nickname ? `${esc(r.nickname)}` : "";
    const uid8 = (r.uid || "").slice(0, 8);
    // 余额增量（刷新后 − 执行前）。有则高亮显示，是用户最关心的「实际到手多少」。
    const d = delta[r.uid] || 0;
    const gain = d > 0 ? `<span class="tk-gain">余额 +${d}</span>` : "";
    const sum = r.err
      ? `<span class="tk-err">${esc(r.err)}</span>`
      : `<span class="tk-sum">${esc(r.msg || "")}</span>${gain}`;
    const items = (r.tasks || []).map(taskRow).join("");
    const manual = (r.manual || []).length
      ? `<div class="task-manual">需在客户端手动完成：${(r.manual || []).map(esc).join("、")}</div>`
      : "";
    return `<div class="task-acct">
      <div class="task-acct-head">${name}<span class="tk-code">${esc(uid8)}</span>${sum}</div>
      <div class="task-list">${items}</div>
      ${manual}
    </div>`;
  }).join("");

  box.innerHTML = `<div class="task-panel-head">${head}</div>${accounts}`;
  const closeBtn = $("btnTaskClose");
  if (closeBtn) {
    closeBtn.onclick = () => {
      $("taskPanel").classList.add("hidden");
      clearCreditDelta(); // 关闭结果面板即视为「看过了」，撤掉卡片上的 +N 角标
    };
  }
}

// clearCreditDelta 清掉增量快照，使卡片角标不再显示。
function clearCreditDelta() {
  creditsBefore = null;
  renderAccounts();
}

// taskRow 渲染单条任务结果。
function taskRow(t) {
  const meta = TASK_STATUS_META[t.status] || { ico: "·", cls: "", text: t.status };
  const prog = t.after || t.before
    ? `<span class="tk-code">${esc(t.before || "")}${t.after ? " → " + esc(t.after) : ""}</span>`
    : "";
  const reward = (t.credit || t.energy)
    ? `<span class="tk-reward">+${t.credit || 0}分 +${t.energy || 0}能</span>` : "";
  const title = t.title ? `（${esc(t.title)}）` : "";
  return `<div class="task-item ${meta.cls}">
    <span class="tk-ico" title="${esc(meta.text)}">${meta.ico}</span>
    <span class="tk-code" title="${esc(t.task_code)}">${esc(t.task_code)}</span>
    <span class="tk-msg">${esc(t.msg || "")}${title} ${prog}</span>
    <span class="tk-reward">${reward}</span>
  </div>`;
}

// ---------- 登录 ----------
let pendingChannel = null;
// 待执行动作："login" = 打开浏览器登录，"import" = 从本机客户端导入。
// 小浣熊两种都支持，由弹窗里的两个按钮分别设定。
let pendingAction = "login";
// 各渠道登录提示的补充说明（自动领取/凭据续期等与签到无关的差异）。
const NO_CHECKIN_LOGIN_HINT = {
  workbuddyai: "（无需手动签到，定时自动对话保活并领取日活奖励）",
  qwenwork: "（每日积分服务端 00:00 自动发放；若浏览器已登录千问办公则全自动完成，否则需扫码一次）",
  monkeycode: "（凭据来自本机已登录的 MonkeyCode 客户端；上游无续期接口，客户端重新登录后需再次导入）",
  raccoon: "（凭据来自本机已登录的小浣熊客户端；access_token 约 2 小时，本工具会自动续期）",
  loomy: "（凭据来自本机已登录的 Loomy 客户端；上游无续期接口，约 14 天后需重新登录并再次导入）",
  glm: "（登录后请按下方指引复制 refresh_token 粘贴回来）",
  minimax: "（设备码授权登录；每日 00:00 重置的免费积分，需要签到领取）",
  cline: "（粘贴官方 sk_ API Key 即可；免费模型池按账号计额度）",
};
function promptLogin(channel) {
  pendingChannel = channel;
  const name = chLabel(channel);
  pendingAction = isImportLocal(channel) ? "import" : "login";
  // 次按钮默认隐藏，只在「两条路都通」的渠道（小浣熊）里显示。
  const altBtn = $("btnLoginAlt");
  altBtn.classList.add("hidden");
  altBtn.onclick = null;

  // 浏览器授权登录渠道：主按钮走浏览器授权 + 协议回调接管，次按钮回退到本机客户端导入。
  if (hasProtocolLogin(channel)) {
    pendingAction = "login";
    $("lcTitle").textContent = "添加 " + name + " 账号";
    $("lcMsg").textContent = `点击「登录${name}」将打开浏览器授权页，登录完成后本工具会自动接管回调并保存账号。`
      + `登录期间会把 ${name} 的协议注册临时指向本工具（结束即恢复），请勿在此期间启动${name}客户端，否则协议注册会被它覆盖。`
      + `也可以改用「从客户端导入」：直接读取本机已登录客户端的凭据。`;
    $("btnLoginConfirm").textContent = "登录" + name;
    altBtn.textContent = "从客户端导入";
    altBtn.classList.remove("hidden");
    altBtn.onclick = () => { pendingAction = "import"; confirmLogin(); };
    $("loginConfirmOverlay").classList.remove("hidden");
    return;
  }

  // 纯导入型渠道：文案与动作都不同（读本机客户端凭据，而不是打开浏览器登录）。
  if (isImportLocal(channel)) {
    $("lcTitle").textContent = "导入 " + name + " 账号";
    $("lcMsg").textContent = `将读取本机已登录的${name}客户端凭据并保存到 omnigate（不会修改客户端本身）。`
      + `若提示未找到凭据，请先打开并登录${name}客户端后重试。${NO_CHECKIN_LOGIN_HINT[channel] || ""}`;
    $("btnLoginConfirm").textContent = "导入";
    $("loginConfirmOverlay").classList.remove("hidden");
    return;
  }
  $("lcTitle").textContent = "添加 " + name + " 账号";
  $("lcMsg").textContent = noExplicitCheckin(channel)
    ? `点击「登录${name}」将打开浏览器窗口，请按照指示正常登录${name}账号，登录成功后关闭浏览器窗口即可。${NO_CHECKIN_LOGIN_HINT[channel] || ""}`
    : `点击「登录${name}」将打开浏览器窗口，请按照指示正常登录${name}账号，登录成功后关闭浏览器窗口即可。`;
  $("btnLoginConfirm").textContent = "登录" + name;
  $("loginConfirmOverlay").classList.remove("hidden");
}
function confirmLogin() {
  $("loginConfirmOverlay").classList.add("hidden");
  if (!pendingChannel) return;
  // 按 pendingAction 分发：小浣熊两种动作都支持，不能只看渠道是否属于导入型。
  if (pendingAction === "import") importLocal(pendingChannel);
  else startLogin(pendingChannel);
}

// importLocal 从本机已登录的官方客户端导入凭据（MonkeyCode / 小浣熊 / Loomy）。
// note 只在有降级时非空（如某个凭据文件没读到）—— 必须显示出来，
// 否则用户只看到"已导入成功"，等积分不显示时才发现。
async function importLocal(channel) {
  try {
    const r = await api("/api/account/import_local", { channel });
    const head = `已导入 ${chLabel(channel)} 账号 ${r.uid || ""}`;
    if (r.note) toast(`${head}。${r.note}`, 8000);
    else toast(head);
    await loadState();
  } catch (e) {
    toast(e.message);
  }
}

async function startLogin(channel) {
  try {
    const r = await api("/api/login/start", { channel });
    const url = r.auth_url;
    if (!url) { toast("无法获取登录链接"); return; }
    $("loginTitle").textContent = `添加 ${chLabel(channel)} 账号`;
    $("loginMsg").textContent = hasProtocolLogin(channel)
      ? `请在浏览器新窗口中完成${chLabel(channel)}登录；完成后本工具会自动接管回调并保存账号（期间请勿启动${chLabel(channel)}客户端）。`
      : "请在浏览器新窗口中完成登录…";
    $("loginOverlay").classList.remove("hidden");
    $("btnCopyUrl").dataset.url = url;
    window.open(url, "_blank", "noopener,noreferrer");
    startLoginPoll();
  } catch (e) {
    toast(e.message);
  }
}

async function cancelLogin() {
  try {
    await api("/api/login/cancel", {});
    stopLoginPoll();
    $("loginOverlay").classList.add("hidden");
    toast("登录已取消");
  } catch (e) { toast(e.message); }
}

function copyUrl() {
  const url = $("btnCopyUrl").dataset.url;
  if (!url) { toast("暂无链接"); return; }
  navigator.clipboard.writeText(url).then(() => toast("链接已复制")).catch(() => toast("复制失败，请手动复制"));
}

// 登录轮询
let loginPoll = null;
function startLoginPoll() {
  stopLoginPoll();
  loginPoll = setInterval(async () => {
    try {
      const st = await api("/api/state");
      if (!st.login_busy) {
        stopLoginPoll();
        $("loginOverlay").classList.add("hidden");
        // 后端在 login_error 里回传终态失败（超时/回调不可用/兑换失败）；
        // 拿不到就是真的成功了。
        if (st.login_error) {
          toast(st.login_error, 8000);
          return;
        }
        toast("登录完成，正在同步账号…");
        await loadState();
        refreshFees();
      }
    } catch (e) { /* 忽略 */ }
  }, 3000);
}
function stopLoginPoll() {
  if (loginPoll) { clearInterval(loginPoll); loginPoll = null; }
}

// ---------- 智谱清言登录（自动捕获 Cookie，手工粘贴为兜底） ----------
// 自动路径：omnigate 拉起独立 profile 的浏览器 → 用户正常登录 → CDP 捕获 Cookie。
// 独立 profile 天然隔离，多账号逐个添加互不干扰（无需手动开无痕）。
let glmPoll = null;

function startGLMLogin() {
  $("glmErr").textContent = "";
  $("glmStatus").textContent = "正在启动浏览器…";
  $("glmManual").classList.add("hidden");
  $("glmAuto").classList.remove("hidden");
  $("glmOverlay").classList.remove("hidden");
  api("/api/login/glm_auto", {})
    .then(() => {
      $("glmStatus").textContent = "浏览器已打开，请在其中登录智谱清言…";
      startGLMPoll();
    })
    .catch((e) => {
      $("glmStatus").textContent = "";
      $("glmErr").textContent = (e.message || "启动失败") + "（可展开下方手工方式）";
      $("glmManual").classList.remove("hidden");
    });
}

function startGLMPoll() {
  stopGLMPoll();
  glmPoll = setInterval(async () => {
    try {
      const st = await api("/api/login/glm_auto_status");
      if (st.status === "success") {
        stopGLMPoll();
        $("glmStatus").textContent = `✅ 已添加账号：${st.nickname || st.uid}`;
        toast("智谱清言账号已添加");
        await loadState();
        refreshFees();
        setTimeout(() => $("glmOverlay").classList.add("hidden"), 1200);
      } else if (st.status === "failed") {
        stopGLMPoll();
        $("glmStatus").textContent = "";
        $("glmErr").textContent = (st.error || "登录失败") + "（可展开下方手工方式）";
        $("glmManual").classList.remove("hidden");
      } else if (st.status === "cancelled" || st.status === "idle") {
        stopGLMPoll();
      }
    } catch (e) { /* 忽略瞬时错误 */ }
  }, 2000);
}

function stopGLMPoll() {
  if (glmPoll) { clearInterval(glmPoll); glmPoll = null; }
}

function cancelGLMLogin() {
  stopGLMPoll();
  api("/api/login/glm_auto_cancel", {}).catch(() => {});
  $("glmOverlay").classList.add("hidden");
}

// 手工兜底：粘贴 refresh_token
async function submitGLMToken() {
  const token = ($("glmTokenInput").value || "").trim();
  if (!token) { $("glmErr").textContent = "请粘贴 refresh_token"; return; }
  const btn = $("btnGLMSubmit");
  btn.disabled = true;
  $("glmErr").textContent = "验证中…";
  try {
    await api("/api/login/glm_token", { refresh_token: token });
    stopGLMPoll();
    $("glmOverlay").classList.add("hidden");
    toast("智谱清言账号已添加");
    await loadState();
    refreshFees();
  } catch (e) {
    $("glmErr").textContent = e.message || "验证失败";
  } finally {
    btn.disabled = false;
  }
}

// ---------- Cline 添加账号（粘贴官方 API Key） ----------
function openClineAdd() {
  $("clineErr").textContent = "";
  $("clineKeyInput").value = "";
  $("clineOverlay").classList.remove("hidden");
  $("clineKeyInput").focus();
}

async function submitClineKey() {
  const key = ($("clineKeyInput").value || "").trim();
  if (!key) { $("clineErr").textContent = "请粘贴 API Key"; return; }
  const btn = $("btnClineSubmit");
  btn.disabled = true;
  $("clineErr").textContent = "校验中…";
  try {
    const res = await api("/api/login/cline_key", { key });
    $("clineOverlay").classList.add("hidden");
    toast(res && res.warn ? `Cline 账号已添加；${res.warn}` : "Cline 账号已添加");
    await loadState();
    refreshFees();
  } catch (e) {
    $("clineErr").textContent = e.message || "验证失败";
  } finally {
    btn.disabled = false;
  }
}

// ---------- 签到时间（设置弹层内编辑；单次变更立即保存） ----------
function delTime(t) {
  const times = (state.checkin_times || []).filter((x) => x !== t);
  saveTimes(times);
}

function addTime() {
  const times = (state.checkin_times || []).slice();
  const now = new Date();
  const next = `${String(now.getHours()).padStart(2, "0")}:${String(now.getMinutes()).padStart(2, "0")}`;
  if (!times.includes(next)) times.push(next);
  saveTimes(times.sort());
}

async function saveTimes(times) {
  try {
    await api("/api/config/checkin_times", { times });
    toast("签到时间已更新");
    loadState();
  } catch (e) { toast(e.message); }
}

// ---------- 开机自启（设置弹层内，切换立即保存） ----------
async function toggleAutostart() {
  try {
    await api("/api/config/autostart", { on: $("chkAutostart").checked });
    toast("设置已保存");
  } catch (e) { toast(e.message); loadState(); }
}

// ---------- 设置弹层（统一配置：监听/API-Key/签到/自启/模型路由/渠道代理） ----------
// PROXY_CHANNELS 渠道上游代理列表（顺序与面板渠道序一致；旧 qoder 已下线不提供代理配置）。
const PROXY_CHANNELS = ["workbuddy", "workbuddyai", "qodercn", "qodercom", "traework", "qwenwork", "glm", "monkeycode", "raccoon", "loomy", "minimax", "cline"];
const PROXY_HINT = { workbuddy: "WorkBuddyCN", workbuddyai: "WorkBuddyAI", traework: "TraeWork", qodercn: "QoderCN", qodercom: "QoderCOM", qwenwork: "千问办公", glm: "智谱清言" };

// renderProxyList 按当前 state.proxies 渲染每渠道一个输入行。
function renderProxyList() {
  const proxies = state.proxies || {};
  $("proxyList").innerHTML = PROXY_CHANNELS.map((ch) => {
    const val = proxies[ch] || "";
    return `<div class="row proxy-row">
      <label class="lbl wide" title="${esc(PROXY_HINT[ch] || ch)}">${esc(PROXY_HINT[ch] || ch)}</label>
      <input class="input grow proxy-input" data-ch="${ch}" value="${esc(val)}" spellcheck="false" placeholder="如 socks5://127.0.0.1:1080（留空直连）">
    </div>`;
  }).join("");
  $("proxyErr").textContent = "";
}

// selectedHost 当前下拉框（+自定义输入）选定的监听主机名。
// 与后端 config.Listen 归一化口径保持一致：空/`::` 等通配写法按「全部网卡」看待（即对外暴露）。
function selectedHost() {
  const v = $("selHost").value;
  if (v !== "__custom__") return v;
  return $("inHost").value.trim();
}

// isLoopbackHost 判定是否「仅本机可访问」：环回 IP / localhost。
// 对应后端 config.Listen.IsLoopback：**显式写环回才算安全**，空主机名视为对外暴露。
function isLoopbackHost(h) {
  const s = (h || "").trim().toLowerCase().replace(/^\[|\]$/g, "");
  if (s === "localhost") return true;
  return s === "127.0.0.1" || s === "::1" || /^127\./.test(s);
}

// syncListenRisk 按当前选定的监听地址，动态显示/隐藏黄色警告条与管理密码的必填星号。
// 默认 127.0.0.1（本机）时两者均不显示——本机监听无需密码，提示只会干扰。
function syncListenRisk() {
  const needPass = !isLoopbackHost(selectedHost());
  $("listenRiskTip").classList.toggle("hidden", !needPass);
  $("adminPassReq").classList.toggle("hidden", !needPass);
}

function openSettings() {
  // 监听
  $("inPort").value = state.listen_port;
  $("selHost").value = state.listen_host === "127.0.0.1" ? "127.0.0.1"
    : (state.listen_host === "0.0.0.0" || state.listen_host === "" || state.listen_host === "::") ? "0.0.0.0"
    : "__custom__";
  if ($("selHost").value === "__custom__") {
    $("inHost").value = state.listen_host;
    $("customHostRow").classList.remove("hidden");
  } else {
    $("customHostRow").classList.add("hidden");
  }
  // API-Key
  $("keyInput").value = state.api_key;
  // 管理密码：后端不回显，只提示是否已设置（留空 = 不改动）
  $("adminPassInput").value = "";
  $("adminPassInput").placeholder = state.admin_pass_set
    ? "已设置（留空 = 不改动；填入新值 = 修改）"
    : (state.auth_required ? "必填：监听非 127.0.0.1 时面板必须鉴权" : "留空 = 面板不鉴权（仅本机监听时允许）");
  // 模型路由
  const cc = state.compat || {};
  const channels = cc.channels || [];
  $("selCh").innerHTML = channels.map(c => `<option value="${c}">${c}</option>`).join("");
  $("selCh").value = cc.default_channel || (channels[0] || "");
  $("inMaxTok").value = cc.max_tokens_cap || 0;
  const map = cc.model_map || {};
  const entries = Object.entries(map);
  $("mapSummary").textContent = entries.length === 0 ? "（空）" : entries.map(([k,v]) => `${k} → ${v}`).join("\u00A0 \u00A0");
  // 映射编辑器：随对话框打开而重置为当前值，收起
  $("mapText").value = entries.map(([k,v]) => `${k} = ${v}`).join("\n");
  $("mapEditor").classList.add("hidden");
  $("mapErr").textContent = "";
  renderMapPresets(channels);
  // 渠道代理
  renderProxyList();
  // 自动签到 + 开机自启 + 临期阈值
  $("chkAutostart").checked = !!state.autostart;
  $("selExpiring").value = String(state.expiring_days || 1);
  // 挑号策略（下拉由后端下发候选，避免前后端两份枚举各写一遍）
  const opts = state.strategy_options || [];
  $("selStrategy").innerHTML = opts.map(o =>
    `<option value="${o.value}" title="${esc(o.desc)}">${esc(o.label)}</option>`).join("");
  $("selStrategy").value = state.strategy || "expire";
  syncStrategyDesc();
  // 成长中心自动化开关（默认全关）
  const g = state.growth || {};
  $("chkGrowthRedeem").checked = !!g.redeem_streak;
  $("chkGrowthTravel").checked = !!g.travel;
  $("selTravelLoc").value = String(g.travel_location || 1);
  $("chkGrowthTasks").checked = !!g.tasks;
  $("chkGrowthTasksMP").checked = !!g.tasks_include_mp;
  syncListenRisk(); // 按当前监听地址初始化警告条与必填星号
  $("settingsOverlay").classList.remove("hidden");
}

function closeSettings() {
  $("settingsOverlay").classList.add("hidden");
}

// syncStrategyDesc 按当前选中的策略刷新说明文案。
function syncStrategyDesc() {
  const opts = state?.strategy_options || [];
  const cur = opts.find(o => o.value === $("selStrategy").value);
  $("strategyDesc").textContent = cur ? cur.desc : (state?.strategy_desc || "");
}

// validateProxies 收集代理输入并做本地形态校验，返回 {proxies, err}。
function validateProxies() {
  const proxies = {};
  for (const inp of document.querySelectorAll(".proxy-input")) {
    const ch = inp.dataset.ch;
    const v = inp.value.trim();
    if (!v) continue;
    let u;
    try { u = new URL(v); } catch { return [null, `渠道 ${ch} 代理地址无效：${v}`]; }
    if (!["http:", "https:", "socks5:"].includes(u.protocol)) {
      return [null, `渠道 ${ch} 代理协议不支持（仅 http/https/socks5）：${v}`];
    }
    if (!u.hostname) return [null, `渠道 ${ch} 代理缺少主机名：${v}`];
    proxies[ch] = v;
  }
  return [proxies, ""];
}

async function saveSettings() {
  // 1) 监听地址 + 管理密码（同请求提交：后端先存密码再切监听）
  let host = $("selHost").value;
  if (host === "__custom__") host = $("inHost").value.trim() || "127.0.0.1";
  const port = parseInt($("inPort").value, 10);
  const adminPass = $("adminPassInput").value.trim();
  const listenBody = { host, port };
  if (adminPass) listenBody.admin_password = adminPass; // 空 = 不改动，避免覆盖已设密码
  try {
    await api("/api/config/listen", listenBody);
  } catch (e) { toast(e.message); return; }
  // 刚设置/修改密码：旧会话已被作废，立即用新密码重新登录，保证本次保存的后续步骤可用。
  // （从「本机监听 + 无密码」切到 0.0.0.0 时鉴权刚启用，此前根本没有 cookie。）
  if (adminPass) {
    try {
      const r = await api("/api/auth/login", { password: adminPass });
      authSession = r.session || "";
    } catch (e) { toast(e.message); closeSettings(); showLogin("管理密码已变更，请重新登录"); return; }
  }

  // 2) 代理（先本地校验，失败阻断保存）
  const [proxies, perr] = validateProxies();
  if (perr) { $("proxyErr").textContent = perr; toast(perr); return; }
  const body = { proxies };
  try {
    await api("/api/config/proxies", body);
  } catch (e) { toast(e.message); return; }

  // 3) 模型映射：优先读编辑器；编辑器从未展开过则用原值（保证「只改监听/渠道不碰映射」）
  let modelMap = state.compat?.model_map || {};
  if (!$("mapEditor").classList.contains("hidden")) {
    const [parsed, err] = parseMapText($("mapText").value);
    if (err) { $("mapErr").textContent = err; toast(err); return; }
    modelMap = parsed;
  }
  const defaultChannel = $("selCh").value;
  const maxTokensCap = parseInt($("inMaxTok").value, 10) || 0;
  try {
    await api("/api/config/compat", { default_channel: defaultChannel, max_tokens_cap: maxTokensCap, model_map: modelMap });
  } catch (e) { toast(e.message); return; }

  // 4) 临期阈值（1/2/3 天，独立端点即时生效）
  const expDays = parseInt($("selExpiring").value, 10) || 1;
  if (expDays !== (state.expiring_days || 1)) {
    try {
      await api("/api/config/expiring_days", { days: expDays });
    } catch (e) { toast(e.message); return; }
  }

  // 4.5) 挑号策略（热更新到各渠道账号池，无需重启）
  const strategy = $("selStrategy").value;
  if (strategy && strategy !== (state.strategy || "expire")) {
    try {
      await api("/api/config/strategy", { strategy });
    } catch (e) { toast(e.message); return; }
  }

  // 5) 成长中心自动化开关（四项，默认全关；保存后热生效，无需重启）
  const growthBody = {
    redeem_streak: $("chkGrowthRedeem").checked,
    travel: $("chkGrowthTravel").checked,
    travel_location: parseInt($("selTravelLoc").value, 10) || 1,
    tasks: $("chkGrowthTasks").checked,
    tasks_include_mp: $("chkGrowthTasksMP").checked,
  };
  try {
    await api("/api/config/growth", growthBody);
  } catch (e) { toast(e.message); return; }

  // 6) API-Key（最后保存：改 Key 可能影响当前会话的后续请求）
  try {
    await api("/api/config/api_key", { key: $("keyInput").value.trim() });
  } catch (e) { toast(e.message); return; }

  // 7) 密码已在第 1 步生效（会话已换新），无需再强制重登
  toast(adminPass ? "设置已保存，管理密码已生效" : "设置已保存");
  closeSettings();
  loadState();
}

// clearAdminPassword 「清除」按钮：关闭面板鉴权（仅环回监听允许，后端会校验）。
async function clearAdminPassword() {
  if (!confirm("确定清除管理密码？清除后面板将不再鉴权（仅允许监听 127.0.0.1）。")) return;
  try {
    await api("/api/config/admin_password", { password: "" });
  } catch (e) { toast(e.message); return; }
  authSession = "";
  toast("管理密码已清除，面板鉴权已关闭");
  closeSettings();
  await ensureSession();
}

// ---------- 显示名修改弹层 ----------
let renameUid = null;

function openRename(uid, currentName) {
  renameUid = uid;
  $("rnUid").textContent = shortUid(uid);
  $("rnInput").value = currentName === shortUid(uid) ? "" : (currentName || "");
  $("renameOverlay").classList.remove("hidden");
  $("rnInput").focus();
}

function closeRename() {
  $("renameOverlay").classList.add("hidden");
  renameUid = null;
}

async function saveRename() {
  if (!renameUid) return;
  const nickname = $("rnInput").value.trim();
  if (!nickname) { toast("显示名不能为空"); return; }
  try {
    await api("/api/account/nickname", { uid: renameUid, nickname });
    toast("显示名已更新");
    closeRename();
    loadState();
  } catch (e) { toast(e.message); }
}

// ---------- 模型映射编辑器 ----------

// CHANNEL_PRESETS 各渠道的缺省建议映射（模型为该渠道常用/免费模型）。
// 用途：未绑定某渠道账号、或不知道该渠道有哪些模型时，给出可点选的起点。
const CHANNEL_PRESETS = {
  workbuddy:   { label: "Claude Code → workbuddy",  items: ["claude-* = workbuddy/glm-5.2", "claude-sonnet-* = workbuddy/kimi-k2.7"] },
  traework:    { label: "Codex → traework",          items: ["gpt-5* = traework/glm-5.2", "codex-* = traework/DeepSeek-V4-Pro"] },
  // TraeCode 与 TraeWork 共用账号，预设沿用 Codex 语义（面向代码场景的新版模型）。
  traecode:    { label: "Codex → traecode",          items: ["gpt-5* = traecode/deepseek-v4.1-flash", "codex-* = traecode/glm-5.3-flash"] },
  workbuddyai: { label: "Claude Code → workbuddyai", items: ["claude-* = workbuddyai/deepseek-v4.1-flash"] },
  qodercn:     { label: "→ qodercn",                 items: ["gpt-* = qodercn/glm-5.3"] },
  qodercom:    { label: "→ qodercom",                items: ["gpt-* = qodercom/glm-5.3"] },
  qwenwork:    { label: "→ qwenwork",                items: ["gpt-* = qwenwork/flash", "claude-* = qwenwork/pro"] },
  glm:         { label: "→ glm (智谱清言)",           items: ["gpt-* = glm/chatglm", "claude-* = glm/chatglm-think"] },
};

// renderMapPresets 按当前已接入渠道渲染缺省建议按钮。
// 仅列出「已接入（有账号）」的渠道——未绑定的渠道点了也会因无账号而失败，不给误导性入口。
function renderMapPresets(channels) {
  const bound = new Set((state.accounts || []).map(a => a.group));
  // TraeCode 与 TraeWork 共用账号，账号列表里只会出现 traework；
  // 但 TraeCode 是可独立路由的渠道，其预设也应可见。
  if (bound.has("traework")) bound.add("traecode");
  const box = $("mapPresets");
  box.innerHTML = channels.filter(c => CHANNEL_PRESETS[c] && bound.has(c)).map(c => {
    const p = CHANNEL_PRESETS[c];
    return `<span class="btn tiny preset" data-ch="${c}" title="${esc(p.items.join("\n"))}">${esc(p.label)}</span>`;
  }).join(" ") || `<span class="hint">（尚未接入任何渠道，先在账号管理添加账号）</span>`;
  box.querySelectorAll(".preset").forEach(b => {
    b.onclick = () => {
      const p = CHANNEL_PRESETS[b.dataset.ch];
      // 追加尚未存在的行，避免重复插入
      const cur = $("mapText").value.split(/\r?\n/).map(s => s.trim()).filter(Boolean);
      const have = new Set(cur.map(l => l.split("=")[0].trim()));
      const add = p.items.filter(it => !have.has(it.split("=")[0].trim()));
      if (!add.length) { toast(`${p.label} 的建议映射已存在`); return; }
      $("mapText").value = cur.concat(add).join("\n");
    };
  });
}

// toggleMapEditor 展开/收起映射编辑器。
function toggleMapEditor() {
  $("mapEditor").classList.toggle("hidden");
}

// parseMapText 校验编辑器内容，返回 (modelMap, 错误信息)。
// 语法错误就地提示并阻断保存，不再弹 prompt 循环。
function parseMapText(raw) {
  const map = {};
  const channels = new Set(state.compat?.channels || []);
  for (const line0 of raw.split(/\r?\n/)) {
    const line = line0.trim();
    if (!line || line.startsWith("#")) continue;
    const idx = line.indexOf("=");
    if (idx < 0) return [null, `格式错误（缺少 =）：${line}`];
    const k = line.substring(0, idx).trim(), v = line.substring(idx + 1).trim();
    if (!k || !v) return [null, `格式错误（键或值为空）：${line}`];
    const vi = v.indexOf("/");
    if (vi <= 0 || !v.substring(vi + 1).trim()) return [null, `映射目标必须是「渠道/模型」形式：${v}`];
    const ch = v.substring(0, vi);
    if (channels.size && !channels.has(ch)) {
      return [null, `未知渠道「${ch}」；已知渠道：${[...channels].join(", ")}`];
    }
    if (map[k] !== undefined) return [null, `重复的键：${k}`];
    map[k] = v;
  }
  return [map, ""];
}

// ---------- 复制到剪贴板 ----------
async function copyText(text, label) {
  try {
    await navigator.clipboard.writeText(text);
    toast(`${label}已复制到剪贴板`);
  } catch (e) {
    // 降级方案：execCommand
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    try {
      document.execCommand("copy");
      toast(`${label}已复制到剪贴板`);
    } catch (err) {
      toast("复制失败，请手动复制");
    }
    document.body.removeChild(ta);
  }
}
function openHelp() { $("helpOverlay").classList.remove("hidden"); }
function closeHelp() { $("helpOverlay").classList.add("hidden"); }
function openAbout() { $("aboutOverlay").classList.remove("hidden"); }
function closeAbout() { $("aboutOverlay").classList.add("hidden"); }

// 「关于」里的仓库地址：点击复制。
//
// 为什么不做成 <a href>：面板常跑在局域网地址上，点链接会让浏览器新开
// GitHub —— 在不少内网/离线环境里这一步就是卡住，用户拿不到地址。
// 给出可一键复制的纯文本更实用，也与项目其它「点一下就复制」的交互一致。
const REPO_URL = "https://github.com/ruo40018293/OmniGate";
function initAboutRepo() {
  const el = $("aboutRepo");
  if (!el) return;
  // 以 DOM 文本为准（REPO_URL 是同值的常量），避免两处失同步时用户复制到错的
  el.textContent = REPO_URL;
  el.onclick = () => copyText(REPO_URL, "仓库地址");
}

// ---------- 通用确认框 ----------
function confirmDialog(msg, onOk) {
  $("confirmMsg").textContent = msg;
  $("confirmOverlay").classList.remove("hidden");
  $("btnConfirmOk").onclick = () => { $("confirmOverlay").classList.add("hidden"); onOk(); };
  $("btnConfirmCancel").onclick = () => $("confirmOverlay").classList.add("hidden");
}

// ---------- 事件绑定 ----------
function bind() {
  $("btnAddWB").onclick = () => promptLogin("workbuddy");
  $("btnAddWBAI").onclick = () => promptLogin("workbuddyai");
  $("btnAddTrae").onclick = () => promptLogin("traework");
  $("btnAddQoderCN").onclick = () => promptLogin("qodercn");
  $("btnAddQoderCOM").onclick = () => promptLogin("qodercom");
  $("btnAddQwen").onclick = () => promptLogin("qwenwork");
  $("btnAddMonkeyCode").onclick = () => promptLogin("monkeycode");
  $("btnAddRaccoon").onclick = () => promptLogin("raccoon");
  $("btnAddLoomy").onclick = () => promptLogin("loomy");
  $("btnAddGLM").onclick = () => startGLMLogin();
  $("btnAddMinimax").onclick = () => promptLogin("minimax");
  $("btnAddCline").onclick = openClineAdd;
  $("btnClineSubmit").onclick = submitClineKey;
  $("btnClineCancel").onclick = () => $("clineOverlay").classList.add("hidden");
  $("btnGLMSubmit").onclick = submitGLMToken;
  $("btnGLMCancel").onclick = cancelGLMLogin;
  $("btnGLMToggleManual").onclick = () => $("glmManual").classList.toggle("hidden");
  $("btnCheckinAll").onclick = checkinAll;
  $("btnRefreshAll").onclick = refreshAll;
  $("btnTasksAll").onclick = () => runTasks("");
  $("btnAddTime").onclick = addTime;
  $("btnCopyUrl").onclick = copyUrl;
  $("btnCancelLogin").onclick = cancelLogin;
  $("btnRefreshFees").onclick = refreshFees;
  $("btnExportModels").onclick = exportModelsJson;
  $("btnShowAllModels").onclick = toggleShowAllModels;
  $("feeSearch").addEventListener("input", applyFeeSearch);
  $("btnMjConfirm").onclick = confirmModelsJson;
  $("btnMjCancel").onclick = closeModelsJson;
  $("modelsJsonOverlay").onclick = (e) => { if (e.target === $("modelsJsonOverlay")) closeModelsJson(); };
  $("chkAutostart").onchange = toggleAutostart;
  $("selStrategy").onchange = syncStrategyDesc;
  $("btnAdminLogout").onclick = () => logout();
  $("btnClearAdminPass").onclick = clearAdminPassword;
  $("btnAdminLogin").onclick = submitLogin;
  $("adminLoginPass").onkeydown = (e) => { if (e.key === "Enter") submitLogin(); };

  $("apiAddr").onclick = () => {
    const v = $("apiAddr").querySelector(".val").textContent;
    copyText(v, "OpenAI 接口地址");
  };
  $("apiKeyDisplay").onclick = () => {
    const v = $("apiKeyDisplay").querySelector(".val").textContent;
    if (v === "（无鉴权）") { toast("当前未设置 API-Key"); return; }
    copyText(v, "API-Key");
  };
  // 顶栏齿轮进入统一设置（点击地址/Key 文本仍为复制）
  $("btnSettings").onclick = openSettings;

  // 注意：这里**不能**读 state —— bind() 在 loadState() 之前运行，
  // 此刻 state 还是 null，访问 state.xxx 会抛 TypeError 并中断整个 bind()
  // （表现为「账号列表空、所有按钮都没绑上」）。
  // 依赖 state 的初始化一律放在 init() 的 await loadState() 之后。

  // 统一设置弹层
  $("btnSettingsSave").onclick = saveSettings;
  $("btnSettingsCancel").onclick = closeSettings;
  $("btnCompatMap").onclick = toggleMapEditor;
  $("selHost").onchange = () => {
    $("customHostRow").classList.toggle("hidden", $("selHost").value !== "__custom__");
    syncListenRisk(); // 监听地址变化 → 实时间同步警告条与必填星号
  };
  // 自定义主机名边输边判（可能一开始就填着非本机地址）
  $("inHost").addEventListener("input", syncListenRisk);
  $("keyInput").addEventListener("keydown", (e) => { if (e.key === "Enter") saveSettings(); });

  // 显示名修改弹层
  $("btnRenameSave").onclick = saveRename;
  $("btnRenameCancel").onclick = closeRename;
  $("rnInput").addEventListener("keydown", (e) => { if (e.key === "Enter") saveRename(); });

  // 点击弹层空白处关闭
  $("settingsOverlay").onclick = (e) => { if (e.target === $("settingsOverlay")) closeSettings(); };
  $("renameOverlay").onclick = (e) => { if (e.target === $("renameOverlay")) closeRename(); };
  $("helpOverlay").onclick = (e) => { if (e.target === $("helpOverlay")) closeHelp(); };

  $("btnHelp").onclick = openHelp;
  $("btnAbout").onclick = openAbout;
  initAboutRepo();
  $("btnHelpClose").onclick = closeHelp;
  $("btnAboutClose").onclick = closeAbout;

  // 登录确认弹层
  $("btnLoginConfirm").onclick = confirmLogin;
  $("btnLoginConfirmCancel").onclick = () => $("loginConfirmOverlay").classList.add("hidden");
  $("loginConfirmOverlay").onclick = (e) => { if (e.target === $("loginConfirmOverlay")) $("loginConfirmOverlay").classList.add("hidden"); };

  $("aboutOverlay").onclick = (e) => { if (e.target === $("aboutOverlay")) closeAbout(); };
}

// ---------- 初始化 ----------
// afterStateReady 在 state 就绪后执行「依赖 state 的一次性初始化」。
//
// 为什么单独抽出来：bind() 跑在 loadState() **之前**，此刻 state 仍是 null。
// 在 bind() 里访问 state.xxx 会抛 TypeError 并中断整个 bind()，
// 连带 init() 走不到 await loadState() —— 表现为「账号列表空、所有按钮都没绑上」，
// 而且控制台只报一个不起眼的 null 读取错误，极难联想到是初始化顺序问题。
function afterStateReady() {
  // 成长任务入口：无支持渠道时禁用（成长中心目前仅 WorkBuddy 国内版提供）。
  if (state && !state.growth_supported) {
    $("btnTasksAll").disabled = true;
    $("btnTasksAll").title = "当前没有支持成长中心的渠道（仅 WorkBuddy 国内版提供）";
  }
  // 探测是否有上一轮成长任务仍在执行（例如刷新页面后），有则续上轮询。
  startTaskPolling();
}

(async function init() {
  bind();
  bindMainTabs();
  bindUsage();
  // 会话探针（不返回 401）：未登录状态下只拉这接口，避免控制台报错
  try {
    const st = await api("/api/auth/state");
    if (st.auth_enabled && !st.auth_session) { showLogin(); return; }
    authSession = st.auth_session || "";
  } catch (e) { /* 探针失败走下面的常规加载（如代理拦截） */ }  loadUsage(); // 页面加载即拉取（首次渲染自动刷新，不依赖手动点击）
  await loadState(); // 状态瞬间返回
  // 依赖 state 的初始化必须放在这里之后（bind() 阶段 state 还是 null）。
  afterStateReady();
  await loadFees();  // 费率表用缓存/静态兜底，秒开
  // 运行统计：统计/请求日志 30s、运行日志 15s 常驻轮询
  // （不依赖当前 tab——里程碑 toast 要在任何 tab 下及时弹出；tab 切入时另有即时拉取）
  loadStats();
  setInterval(loadStats, STATS_POLL_MS);
  // 实时请求（R56）：默认 tab 是账号管理，先不轮询；切到运行统计时 startLivePoll。
  // 但若用户刷新页面时正停在统计 tab，bindMainTabs 已按 active 状态启动，这里不重复。
  loadAppLog();
  setInterval(loadAppLog, APPLOG_POLL_MS);
  // 「调用中」徽标：2s 一跳。
  //
  // 2s 而非更短：后端 InUseWindow 是 5s，2s 采样能稳定捕获状态变化，
  // 又不至于把请求打得太密（面板常驻开着）。pollInUse 只改徽标不重建 DOM，
  // 代价很小。
  pollInUse();
  setInterval(pollInUse, INUSE_POLL_MS);
})();

// ---------- 用量与流水面板 ----------
let usageDays = 7;
let usageChart = null;    // token 折线图（echarts 实例）
let creditChart = null;   // 积分消耗折线图（echarts 实例）
let recentAll = [];       // 全量最近流水（分页源，时间升序）
let recentPage = 0;
let lastNames = {};       // 最近一次渲染的 uid→昵称表（翻页时复用）
const RECENT_PAGE_SIZE = 20;
const MODEL_TOP_N = 20;   // 模型榜只保留前 N 名

function fmtCredits(n) {
  if (n == null) return "-";
  return Number(n).toLocaleString("zh-CN");
}

function fmtTokensFull(n) {
  if (!n) return "0";
  if (n >= 1e8) return (n / 1e8).toFixed(2) + "亿";
  if (n >= 1e4) return (n / 1e4).toFixed(1) + "万";
  return Number(n).toLocaleString("zh-CN");
}

const CH_NAMES = {
  workbuddy: "WorkBuddyCN", workbuddyai: "WorkBuddyAI", traework: "TraeWork",
  qoder: "Qoder", qodercn: "QoderCN", qodercom: "QoderCOM", qwenwork: "千问办公", glm: "智谱清言",
};

async function loadUsage() {
  try {
    const st = await api(`/api/usage?days=${usageDays}`);
    if (st.disabled) {
      return;
    }
    renderUsage(st);
  } catch (e) { /* 统计加载失败不阻塞 */ }
}

function renderUsage(st) {
  $("ucTokens").textContent = fmtTokensFull(st.token.total);
  $("ucReqs").textContent = fmtCredits(st.token.requests);
  $("ucSpend").textContent = fmtCredits(st.credit.spend);
  $("ucEarn").textContent = fmtCredits(st.credit.earn);

  renderModelTable(st.token.by_model || [], usageDays);
  renderCreditTab(st.credit || {});
  renderUsageChart(st);
  renderCreditChart(st.credit || {});
}

function renderModelTable(rows, days) {
  const tb = $("tblModels").querySelector("tbody");
  if (!rows.length) { tb.innerHTML = `<tr><td colspan="5" class="empty-cell">暂无数据（流水自本功能上线后开始记录）</td></tr>`; return; }
  const top = rows.slice(0, MODEL_TOP_N); // 前 20 名，已按 token 降序
  tb.innerHTML = top.map((r) => {
    const avg = days > 1 ? Math.round((r.pt + r.ct) / days) : (r.pt + r.ct);
    return `<tr>
      <td>${esc(r.model)}</td><td><span class="badge ${chClass(r.channel)}">${esc(chLabel(r.channel))}</span></td>
      <td class="num">${fmtCredits(r.requests)}</td><td class="num">${fmtTokensFull(r.pt + r.ct)}</td>
      <td class="num">${fmtTokensFull(avg)}</td></tr>`;
  }).join("");
}

function renderCreditTab(credit) {
  const entries = credit.entries || [];
  const names = credit.name_map || {};
  // 分页渲染流水（折线图由 renderCreditChart 基于同一份条目自算）
  recentAll = entries;
  recentPage = 0;
  lastNames = names;
  renderRecentPage(names);
  renderCreditChart(entries, names);
}

function renderRecentPage(names) {
  const kinds = { earn: "↑签到/发放", spend: "↓消耗", expire: "✖过期" };
  const pages = Math.max(1, Math.ceil(recentAll.length / RECENT_PAGE_SIZE));
  if (recentPage >= pages) recentPage = pages - 1;
  // 倒序展示（最新在前）：从尾部往前取当前页
  const end = recentAll.length - recentPage * RECENT_PAGE_SIZE;
  const start = Math.max(0, end - RECENT_PAGE_SIZE);
  const slice = recentAll.slice(start, end).reverse();
  $("recentList").innerHTML = slice.length
    ? slice.map((r) => `<div class="rline ${r.kind}">
        <span class="rtime">${new Date(r.ts * 1000).toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" })}</span>
        <span class="rkind">${kinds[r.kind] || r.kind}</span>
        <span class="ramount">${fmtCredits(r.amount)}</span>
        <span class="racct"><span class="badge ${chClass(r.channel)}">${esc(chLabel(r.channel))}</span> ${esc((names || {})[r.uid] || shortUid(r.uid))}</span>
        <span class="rname">${esc(r.note || "")}</span>
        <span class="rbal">余额 ${fmtCredits(r.balance)}</span></div>`).join("")
    : `<div class="muted" style="padding:8px">暂无流水</div>`;
  $("pgInfo").textContent = `${recentPage + 1} / ${pages}`;
  $("pgPrev").disabled = recentPage === 0;
  $("pgNext").disabled = recentPage >= pages - 1;
}

// makeChart 懒建 echarts 实例；CDN 不可达时在容器里显示提示并返回 null。
function makeChart(box, existing) {
  if (typeof echarts === "undefined") {
    box.textContent = "图表库加载失败（CDN 不可达），表格不受影响";
    return null;
  }
  return existing || echarts.init(box);
}

function renderUsageChart(st) {
  const box = $("usageChart");
  usageChart = makeChart(box, usageChart);
  if (!usageChart) return;
  const byDay = (st.token.by_day || []);
  const dates = byDay.map((d) => d.date);
  // 渠道系列：取所有出现过的渠道并集
  const chans = [...new Set(byDay.flatMap((d) => Object.keys(d.by_channel || {})))];
  const series = chans.map((ch) => ({
    name: CH_NAMES[ch] || ch, type: "line", smooth: true,
    data: byDay.map((d) => d.by_channel[ch] || 0),
  }));
  usageChart.setOption({
    tooltip: { trigger: "axis" },
    legend: { data: series.map((s) => s.name) },
    grid: { left: 50, right: 20, top: 36, bottom: 28 },
    xAxis: { type: "category", data: dates },
    yAxis: { type: "value", axisLabel: { formatter: (v) => fmtTokensFull(v) } },
    series,
  }, true);
  usageChart.resize();
}

// renderCreditChart 积分消耗折线图：按日聚合各账号 spend（原始条目自算），
// 只画消耗总量前 10 的账号；不含入项/过期。
function renderCreditChart(entries, names) {
  const box = $("creditChart");
  const spends = entries.filter((e) => e.kind === "spend");
  if (!spends.length) { box.style.display = "none"; if (creditChart) { creditChart.dispose(); creditChart = null; } return; }
  box.style.display = "";
  creditChart = makeChart(box, creditChart);
  if (!creditChart) return;
  // 按日 × 账号聚合
  const dates = [...new Set(spends.map((e) => fmtDay(e.ts)))].sort();
  const byAcct = {}; // uid -> {date: spend}
  const totals = {}; // uid -> 总消耗
  for (const e of spends) {
    const d = fmtDay(e.ts);
    (byAcct[e.uid] ||= {})[d] = (byAcct[e.uid][d] || 0) + -e.amount;
    totals[e.uid] = (totals[e.uid] || 0) + -e.amount;
  }
  const top10 = Object.keys(totals).sort((a, b) => totals[b] - totals[a]).slice(0, 10);
  // 系列名带渠道前缀（跨渠道同名昵称可区分）：取该账号消耗条目中最多的渠道
  const mainChOf = (uid) => {
    const cnt = {};
    for (const e of spends) if (e.uid === uid) cnt[e.channel] = (cnt[e.channel] || 0) + 1;
    return Object.keys(cnt).sort((a, b) => cnt[b] - cnt[a])[0] || "";
  };
  const series = top10.map((uid) => ({
    name: `${chLabel(mainChOf(uid))} ${names[uid] || shortUid(uid)}`, type: "line", smooth: true,
    data: dates.map((d) => byAcct[uid][d] || 0),
  }));
  creditChart.setOption({
    tooltip: { trigger: "axis", valueFormatter: (v) => fmtCredits(v) },
    legend: { data: series.map((s) => s.name) },
    grid: { left: 50, right: 20, top: 36, bottom: 28 },
    xAxis: { type: "category", data: dates },
    yAxis: { type: "value", axisLabel: { formatter: (v) => fmtCredits(v) } },
    series,
  }, true);
  creditChart.resize();
}

// fmtDay 时间戳 → "09-21"（与 token 图 X 轴口径一致）
function fmtDay(ts) {
  const d = new Date(ts * 1000);
  return `${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

// 主面板 tab 切换（账号管理 / 用量与流水 / 运行统计）
function bindMainTabs() {
  document.querySelectorAll(".main-tabs .mtab").forEach((b) => {
    b.onclick = () => {
      document.querySelectorAll(".main-tabs .mtab").forEach((x) => x.classList.toggle("active", x === b));
      $("mtabAccounts").classList.toggle("hidden", b.dataset.mtab !== "accounts");
      $("mtabUsage").classList.toggle("hidden", b.dataset.mtab !== "usage");
      $("mtabStats").classList.toggle("hidden", b.dataset.mtab !== "stats");
      if (b.dataset.mtab === "usage") {
        loadUsage(); // 切到用量 tab 时拉最新（首次渲染自动刷新）
        if (usageChart) usageChart.resize();
        if (creditChart) creditChart.resize();
      }
      if (b.dataset.mtab === "stats") {
        loadStats(); // 切到运行统计 tab 时拉最新（后台 30s 轮询兜底）
        startLivePoll(); // 实时请求 2s 轮询，仅在本 tab 可见时运行（省 CPU/网络）
      } else {
        stopLivePoll(); // 离开 tab 立即停：排队状态只在用户看的时候才有意义
      }
    };
  });

  bindFeesTabs(); // 费率面板渠道标签切换（事件委托一次绑定）
  bindSummaryFilter(); // 积分汇总条 chip 点击筛选账号（事件委托一次绑定）
}

// 面板 tab / 范围切换事件（bind 末尾调用）
function bindUsage() {
  $("btnRefreshUsage").onclick = loadUsage;
  $("usageRange").querySelectorAll(".seg-btn").forEach((b) => {
    b.onclick = () => {
      usageDays = parseInt(b.dataset.days, 10);
      $("usageRange").querySelectorAll(".seg-btn").forEach((x) => x.classList.toggle("active", x === b));
      loadUsage();
    };
  });
  document.querySelectorAll(".tabs .tab").forEach((b) => {
    b.onclick = () => {
      document.querySelectorAll(".tabs .tab").forEach((x) => x.classList.toggle("active", x === b));
      $("tabToken").classList.toggle("hidden", b.dataset.tab !== "token");
      $("tabCredit").classList.toggle("hidden", b.dataset.tab !== "credit");
      if (b.dataset.tab === "token" && usageChart) usageChart.resize();
      if (b.dataset.tab === "credit" && creditChart) creditChart.resize();
    };
  });
  $("pgPrev").onclick = () => { if (recentPage > 0) { recentPage--; renderRecentPage(lastNames); } };
  $("pgNext").onclick = () => { recentPage++; renderRecentPage(lastNames); };
  window.addEventListener("resize", () => { if (usageChart) usageChart.resize(); if (creditChart) creditChart.resize(); });
}

// ---------- 运行统计面板（第三个主 tab；数据来自 internal/stats 引擎的 /api/stats） ----------
const STATS_POLL_MS = 30000;   // 统计/请求日志轮询周期
const APPLOG_POLL_MS = 15000;  // 运行日志（app.log）轮询周期
const INUSE_POLL_MS = 2000;    // 「调用中」徽标轮询周期（后端 InUseWindow=5s，取其一半以稳定采样）
const APPLOG_MAX_LINES = 200;  // 渲染上限（上游 /api/logs 最多回 300 行，前端再多不渲染）
const STATS_LOGS_URL = "/api/stats/logs?limit=15";
const TOAST_SEEN_KEY = "stats-toast-seen"; // 里程碑 toast 已读去重（localStorage，防每轮轮询重复弹）

// 小数积分（模型行）：整数走千分位，非整数保留两位
function fmtCreditS(x) {
  x = Number(x) || 0;
  return Number.isInteger(x) ? fmtCredits(x) : x.toFixed(2);
}
// tok/s 一位小数（≥100 取整）；0 值由调用方给「—」不解为 0 速度
function fmtPct(x) {
  if (!x) return "";
  return x >= 100 ? String(Math.round(x)) : x.toFixed(1);
}
// Trae 系渠道判定（TraeWork/TraeCode 共用账号积分池，上游 usage 帧不含单次积分 → 积分为分摊估算）
function isTraeChannel(model) {
  const s = String(model || "");
  const i = s.indexOf("/");
  return i > 0 && (s.slice(0, i) === "traework" || s.slice(0, i) === "traecode");
}

let statsRefreshing = false; // 防重入：上轮未完成时跳过本轮（防慢响应晚到覆盖新数据）
let statsWarned = false;     // 拉取失败只 warn 一次，避免控制台刷屏
let statsLastOk = "";        // 最近一次成功拉取时刻（失败角标要注明数据停止于何时）

// 更新角标：成功 → 记录时刻；失败 → 标红注明数据停止时间（陈旧数据必须可感知）
function stMarkUpdated() {
  const el = $("stUpdated");
  el.classList.remove("st-stale");
  el.textContent = "上次更新 " + (statsLastOk = new Date().toTimeString().slice(0, 8));
}
function stMarkStale() {
  const el = $("stUpdated");
  el.classList.add("st-stale");
  el.textContent = statsLastOk ? `拉取失败 · 数据停止于 ${statsLastOk}` : "拉取失败";
}

async function loadStats() {
  if (statsRefreshing) return;
  statsRefreshing = true;
  try {
    const [s, lg] = await Promise.all([
      api("/api/stats"),
      api(STATS_LOGS_URL).catch(() => null), // 失败保旧：跳过本轮渲染，不清空已渲染的请求日志
    ]);
    renderStats(s);
    renderAbnormal(s.abnormal || []);
    if (lg) renderLogs(lg.rows || []);
    stMarkUpdated();
  } catch (e) {
    stMarkStale();
    if (!statsWarned) { statsWarned = true; console.warn("运行统计加载失败：", e); }
  } finally {
    statsRefreshing = false;
  }
}

async function loadAppLog() {
  try {
    const d = await api("/api/logs");
    renderAppLog(d.lines || []);
  } catch (e) { /* 上游瞬时不可达：保留旧内容，下轮重试 */ }
}

// 汇总横条条目（标签 + 等宽数值 + 可选小字注解）
function stSumItem(label, val, cls, sub) {
  return `<span class="st-sum-item"><span class="st-sum-label">${label}</span>` +
    `<span class="st-sum-val ${cls || ""}">${val}</span>` +
    (sub ? `<span class="st-sum-sub">${sub}</span>` : "") + `</span>`;
}
// R66：「今日输出」的副标题 —— 输出占今日 Token 的比例。合计为 0 时留空，
// 不写 "0.0%"（无样本时的百分比是噪音）。比例只做一位小数，避免给出假精度。
function outPctSub(tu) {
  const total = Number(tu.today) || 0;
  const out = Number(tu.out_today) || 0;
  if (total <= 0 || out <= 0) return "";
  return `占 ${((out / total) * 100).toFixed(1)}% · 其余为输入`;
}

// Token 多天行条目（tip 非空时挂 title 悬停说明，R66）
function stTkItem(label, val, tip) {  const t = tip ? ` title="${esc(tip)}"` : "";
  return `<span class="st-tk-item"${t}>${label} <span class="st-tk-val">${val}</span></span>`;
}

function renderStats(s) {
  const tu = s.token_usage || {};
  // 汇总横条：作废/今日到期仅 >0 时出现（避免每天多条 0 值噪音）
  $("stSum").innerHTML =
    stSumItem("总积分", fmtCredits(s.total_credits) + "分", "pur", fmtCredits(s.total_accounts) + " 个账号") +
    stSumItem("收入积分", fmtCredits(s.credit_in), "ok", "签到事件口径") +
    stSumItem("今日消耗", fmtCredits(s.credit_used), "warn", "精确 " + fmtCreditS(s.credit_out_exact)) +
    (Number(s.credit_expired) > 0
      ? stSumItem("今日作废", fmtCredits(s.credit_expired), "risk", "积分包到期 · 非消耗")
      : "") +
    stSumItem("消耗 Token", fmtTokensFull(s.tokens), "warn") +
    // R66：今日输出（模型真正产出的量）+ 占合计的比例。官方口径里「token 用量」
    // 通常指输出（生成长度），而输入是每轮重发的上下文——两者必须并排看才有意义。
    stSumItem("今日输出", fmtTokensFull(tu.out_today), "warn", outPctSub(tu)) +
    (Number(s.expire_today) > 0
      ? stSumItem("今日到期", fmtCredits(s.expire_today) + "分", "risk", "今晚作废尽快消耗")
      : "");
  $("stMilestone").textContent = s.milestone_count > 0 ? `里程碑 ${s.milestone_count}×100分` : "";
  renderPlatformTable(s);
  // Token 多天行（今日情况标题右侧）
  // R66：多一个「其中输出」——输出=模型真正产出的字，输入=每轮重发的上下文。
  // 长会话里输入远大于输出，只看合计会误判成「模型很费 token」。
  $("stToken").innerHTML =
    stTkItem("Token 今日", fmtTokensFull(tu.today)) +
    stTkItem("其中输出", fmtTokensFull(tu.out_today),
      `输出（模型产出）今日 ${fmtTokensFull(tu.out_today)} · 7日 ${fmtTokensFull(tu.out7)} · 30日 ${fmtTokensFull(tu.out30)}；` +
      `输入（上下文重发）今日 ${fmtTokensFull(tu.in_today)} · 7日 ${fmtTokensFull(tu.in7)}`) +
    stTkItem("7日", fmtTokensFull(tu.days7)) +
    stTkItem("30日", fmtTokensFull(tu.days30)) +
    stTkItem("请求 今日", fmtCredits(tu.req_today) + " / " + fmtCredits(tu.req_all)) +
    stTkItem("已记录", fmtCredits(tu.days) + " 天");
  renderRotation(s.rotation || []);
  renderExpiry(s.expiry || []);
  renderModels(s.models || []);
  handleToasts(s.toasts || []);
}

// 平台表（今日情况主视图）：列 = #/平台/账号/当前积分/今日收入/今日消耗(净值)/今日作废/
// 今日Token/今日到期/明日到期/7日内到期，末行合计。
// 「今日消耗」= 差值花费 − 积分包到期作废（净值才是真消耗），「今日作废」即被扣掉的那部分。
// 合计行取全池口径，与顶部汇总横条严格同源（合计 == 横条可互相印证）。
function renderPlatformTable(s) {
  const rows = s.platforms || [];
  if (!rows.length) { $("stPlat").innerHTML = ""; return; }
  let html = `<div class="st-plat-row st-plat-head">` +
    `<span class="st-plat-no">#</span>` +
    `<span class="st-plat-slot">平台</span>` +
    `<span class="st-plat-accts">账号</span>` +
    `<span class="st-plat-credits">当前积分</span>` +
    `<span class="st-plat-in">今日收入</span>` +
    `<span class="st-plat-out" title="花费差值扣除积分包到期作废后的净消耗">今日消耗</span>` +
    `<span class="st-plat-expd" title="今日因积分包到期/回收而作废的额度（已从今日消耗中扣除）">今日作废</span>` +
    `<span class="st-plat-tokens" title="上行=今日 Token 合计；下行「出」= 其中输出（模型真正产出的量）">今日Token</span>` +
    `<span class="st-plat-exp">今日到期</span>` +
    `<span class="st-plat-exp">明日到期</span>` +
    `<span class="st-plat-exp">7日内到期</span>` +
    `</div>`;
  rows.forEach((r, i) => {
    html += `<div class="st-plat-row">` +
      `<span class="st-plat-no">${i + 1}</span>` +
      `<span class="st-plat-slot"><span class="badge ${chClass(r.group)}">${esc(chLabel(r.group))}</span></span>` +
      `<span class="st-plat-accts">${fmtCredits(r.accounts)}</span>` +
      stPlatNum("st-plat-credits", r.credits, "分") +
      stPlatNum("st-plat-in", r.credit_in) +
      stPlatNum("st-plat-out", r.credit_used) +
      stPlatNum("st-plat-expd", r.credit_expired, "分") +
      stPlatTok(r.tokens, r.tokens_out) +
      stPlatNum("st-plat-exp", r.expire_today, "分") +
      stPlatNum("st-plat-exp", r.expire_tomorrow, "分") +
      stPlatNum("st-plat-exp", r.expire_7d, "分") +
      `</div>`;
  });
  // 合计行：与顶部汇总横条同源
  html += `<div class="st-plat-row st-plat-sum-row">` +
    `<span class="st-plat-no"></span>` +
    `<span class="st-plat-slot">合计</span>` +
    `<span class="st-plat-accts">${fmtCredits(s.total_accounts)}</span>` +
    stPlatNum("st-plat-credits", s.total_credits, "分") +
    stPlatNum("st-plat-in", s.credit_in) +
    stPlatNum("st-plat-out", s.credit_used) +
    stPlatNum("st-plat-expd", s.credit_expired, "分") +
    stPlatTok(s.tokens, (s.token_usage || {}).out_today) +
    stPlatNum("st-plat-exp", s.expire_today, "分") +
    stPlatNum("st-plat-exp", s.expire_tomorrow, "分") +
    stPlatNum("st-plat-exp", s.expiring_7d, "分") +
    `</div>`;
  $("stPlat").innerHTML = html;
}

// 平台表数值单元格：0 值置灰「—」（避免满屏 0 淹没真正有数的列），颜色由列类自带
function stPlatNum(cls, amount, suffix) {
  amount = Number(amount) || 0;
  if (amount <= 0) return `<span class="${cls} st-plat-zero">—</span>`;
  return `<span class="${cls}">${fmtCredits(amount)}${suffix || ""}</span>`;
}
// Token 单元格：上行=今日合计（万/亿），下行=其中输出（R66）。
// 0 值置灰——但合计为 0 而输出有值时仍展示输出，避免拆分层被合计的置灰逻辑吞掉。
function stPlatTok(amount, out) {
  amount = Number(amount) || 0;
  out = Number(out) || 0;
  if (amount <= 0 && out <= 0) return `<span class="st-plat-tokens st-plat-zero">—</span>`;
  const main = amount > 0 ? fmtTokensFull(amount) : `<span class="st-plat-zero">—</span>`;
  const sub = out > 0 ? `<i class="st-plat-toksub" title="输出（completion）= 模型真正产出的量">出 ${fmtTokensFull(out)}</i>` : "";
  return `<span class="st-plat-tokens">${main}${sub}</span>`;
}

const ST_ROT_MARKS = ["①", "②", "③"];

// 使用中 / 接棒顺序：当前=粘性账号；接棒=临期优先+余额排序推算的前 3
function renderRotation(rot) {
  if (!rot.length) { $("stRotation").innerHTML = ""; $("stRotation").className = "st-rotation"; return; }
  let html = `<div class="st-block-title">使用中 / 接棒顺序<span class="st-hint">当前=粘性账号 · 接棒=临期优先+余额排序推算</span></div>`;
  for (const rv of rot) {
    const cur = rv.current
      ? `使用中 <b>${esc(rv.current.nickname)}</b>（粘性 ${esc(rv.current.sticky || "0")}，余额 ${fmtCredits(rv.current.credits)}分）`
      : `<span class="st-rot-none">暂无使用记录</span>`;
    let nextHtml = "";
    (rv.next || []).forEach((n, j) => {
      nextHtml += `<span class="st-rot-next">${ST_ROT_MARKS[j]} ${esc(n.nickname)}` +
        `（${fmtCredits(n.credits)}分${n.expiring > 0 ? "·临期" + fmtCredits(n.expiring) : ""}）</span>`;
    });
    html += `<div class="st-rot-row"><span class="badge ${chClass(rv.group)}">${esc(chLabel(rv.group))}</span>` +
      `<span class="st-rot-cur">${cur}</span>` +
      (nextHtml ? `<span class="st-rot-nexts">${nextHtml}</span>` : "") + `</div>`;
  }
  $("stRotation").innerHTML = html;
  $("stRotation").className = "st-rotation st-block";
}

// 最近临期：到期日升序；risk=按今日速度花不完 → 给预计作废量（额度−外推消耗）
function renderExpiry(ex) {
  if (!ex.length) { $("stExpiry").innerHTML = ""; $("stExpiry").className = "st-expiry"; return; }
  let html = `<div class="st-block-title">最近临期<span class="st-hint">按到期日升序 · ${ex.length} 个账号</span></div>` +
    `<div class="st-scroll st-ex-scroll">` +
    `<div class="st-ex-row st-ex-head">` +
    `<span class="st-ex-tag">#</span>` +
    `<span class="st-ex-plat-slot">平台</span>` +
    `<span class="st-ex-name">账号</span>` +
    `<span class="st-ex-date">到期日</span>` +
    `<span class="st-ex-days">剩余</span>` +
    `<span class="st-ex-amt">额度 · 今日已耗</span>` +
    `</div>`;
  ex.forEach((r, i) => {
    const dayTxt = r.days_left <= 0 ? "今日到期" : (r.days_left === 1 ? "明日到期" : `${r.days_left} 天后到期`);
    const burn = r.risk ? ` · 预计作废 ${fmtCredits(r.amount - r.burn_forecast)}分` : "";
    html += `<div class="st-ex-row${r.risk ? " st-risk" : ""}">` +
      `<span class="st-ex-tag">${i + 1}</span>` +
      `<span class="st-ex-plat-slot"><span class="badge ${chClass(r.group)}">${esc(chLabel(r.group))}</span></span>` +
      `<span class="st-ex-name" title="${esc(r.uid)}">${esc(r.nickname || r.uid)}</span>` +
      `<span class="st-ex-date">${esc(r.expire_at)}</span>` +
      `<span class="st-ex-days">${esc(dayTxt)}</span>` +
      `<span class="st-ex-amt">${fmtCredits(r.amount)}分 · 已耗 ${fmtCredits(r.today_out)}${burn}</span>` +
      `</div>`;
  });
  html += `</div>`;
  $("stExpiry").innerHTML = html;
  $("stExpiry").className = "st-expiry st-block";
}

// 模型消耗（今日，按积分降序）：Trae 系积分加「≈」（分摊估算），平均速度=成功请求实测均值
function renderModels(models) {
  // R66：Token 拆成「输入/输出/合计」三列——只看合计无法判断是「模型吐得多」还是
  // 「上下文重发得多」（后者是长会话的常态，与模型本身无关）。
  // 旧快照没有拆分时 in/out 均为 0，此列显示 "—"，不拿合计凑数。
  let mh = `<tr><th>#</th><th>模型</th><th>次数</th>` +
    `<th title="输入 = 每次请求重发的上下文（prompt tokens）">输入</th>` +
    `<th title="输出 = 模型真正产出的内容（completion tokens）">输出</th>` +
    `<th>合计</th><th>积分</th>` +
    `<th title="成功请求实测均值：tok/s = 生成 token ÷ 生成耗时（按时长加权）；TTFB = 首字节耗时算术平均">平均速度</th></tr>`;
  models.forEach((m, j) => {
    const isTrae = isTraeChannel(m.model);
    let creditCell;
    if (m.credit > 0) {
      creditCell = isTrae
        ? `<span title="TraeWork 上游不下发单次积分：按该渠道今日真实消耗（余额差值）与 token 占比分摊，为估算值">≈${fmtCreditS(m.credit)}</span>`
        : fmtCreditS(m.credit);
    } else {
      creditCell = isTrae
        ? `<span title="TraeWork 无单次精确积分，按渠道真实消耗分摊；暂无余额差值消耗">—</span>`
        : fmtCreditS(m.credit);
    }
    const tps = Number(m.avg_tok_per_sec) || 0;
    const ttfb = Number(m.avg_ttfb_ms) || 0;
    const parts = [];
    if (tps > 0) parts.push(fmtPct(tps) + " tok/s");
    if (ttfb > 0) parts.push(fmtCredits(ttfb) + "ms");
    const perfCell = parts.length
      ? `<span title="样本 ${fmtCredits(m.perf_samples)} 次成功请求">${parts.join(" · ")}</span>`
      : "—";
    const inTok = Number(m.in_tokens) || 0;
    const outTok = Number(m.out_tokens) || 0;
    const splitReady = inTok > 0 || outTok > 0;
    const inCell = splitReady ? fmtTokensFull(inTok) : "—";
    const outCell = splitReady ? fmtTokensFull(outTok) : "—";
    mh += `<tr><td class="st-idx">${j + 1}</td>` +
      `<td class="st-mono" title="${esc(m.model)}">${esc(m.model)}</td>` +
      `<td>${fmtCredits(m.reqs)}</td>` +
      `<td>${inCell}</td><td>${outCell}</td>` +
      `<td>${fmtTokensFull(m.tokens)}</td><td>${creditCell}</td>` +
      `<td class="st-mono">${perfCell}</td></tr>`;
  });
  $("stModels").innerHTML = models.length ? mh : `<tr><td class="st-empty" colspan="8">今日暂无消耗</td></tr>`;
}

// 请求日志：降序渲染（最新在最上）；tok/s 仅流式有值，0/空给「—」
function renderLogs(rows) {
  let h = `<tr><th>#</th><th>时间</th><th>模型</th><th>模式</th><th>状态</th><th>TTFB</th><th>tok</th><th>tok/s</th><th>total</th><th>积分</th><th>失败原因</th></tr>`;
  for (let i = rows.length - 1; i >= 0; i--) {
    const r = rows[i];
    const status = r.status < 400 ? String(r.status) : `<span class="st-bad">${r.status}</span>`;
    const tps = r.tok_per_sec > 0 ? fmtPct(r.tok_per_sec) : "—";
    // 失败原因（R56）：上游 HTTP 已 200 但内容是错误码时，状态列看不出问题，
    // 这一列就是唯一的区分点，必须常显不能折叠。
    const err = r.err ? `<span class="st-bad" title="${esc(r.err)}">${esc(r.err)}</span>` : "";
    h += `<tr${r.err ? ' class="st-row-bad"' : ""}><td>${fmtCredits(r.seq)}</td><td class="st-mono">${esc(r.time)}</td><td class="st-mono">${esc(r.model)}</td>` +
      `<td>${esc(r.mode)}</td><td>${status}</td><td>${fmtCredits(r.ttfb_ms)}ms</td><td>${fmtCredits(r.tok)}</td><td>${tps}</td>` +
      `<td>${r.total_sec ? r.total_sec.toFixed(1) + "s" : "—"}</td><td>${r.credit > 0 ? fmtCreditS(r.credit) : "—"}</td>` +
      `<td class="st-live-err">${err}</td></tr>`;
  }
  $("stLogs").innerHTML = rows.length ? h : `<tr><td class="st-empty">暂无请求</td></tr>`;
}

// ---------------------------------------------------------------------------
// 进行中的请求（R56）：/api/live 快照。
//
// 为什么需要：排队保活期间网关不向客户端发字节，客户端界面只有计时器，
// 用户体感是「卡住了」。这里把「谁在等、等什么、等了多久、上游说前方多少人」
// 变成一屏可见，不必去翻 app.log（用户原话：我看运行日志啥都没看出问题）。
// ---------------------------------------------------------------------------
const LIVE_POLL_MS = 2000;
let liveTimer = null;
let liveWarned = false;

const LIVE_PHASE_META = {
  queued:    { tag: "排队中", cls: "st-live-queued" },
  retrying:  { tag: "重投中", cls: "st-live-retry" },
  streaming: { tag: "出流中", cls: "st-live-stream" },
  done:      { tag: "已完成", cls: "" },
};

function fmtElapsed(started) {
  const t = Date.parse(started);
  if (!t) return "—";
  const s = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (s < 60) return s + "s";
  return Math.floor(s / 60) + "m" + String(s % 60).padStart(2, "0") + "s";
}

async function refreshLive() {
  try {
    const d = await api("/api/live");
    renderLive(d.entries || []);
    liveWarned = false;
  } catch (e) {
    // 失败保旧：不清空表格（用户正在看的就是等待状态）
    if (!liveWarned) { liveWarned = true; console.warn("实时请求加载失败：", e); }
  }
}

function renderLive(rows) {
  const box = $("stLive");
  if (!box) return;
  let h = `<tr><th>开始</th><th>已等</th><th>渠道</th><th>模型</th><th>阶段</th><th>前方</th><th>预计</th><th>第几跳</th><th>最近进展</th></tr>`;
  for (let i = rows.length - 1; i >= 0; i--) {
    const r = rows[i];
    const meta = LIVE_PHASE_META[r.phase] || { tag: r.phase || "—", cls: "" };
    const notes = r.notes || [];
    const last = notes.length ? notes[notes.length - 1].text : "";
    const pos = r.queue_pos > 0 ? fmtCredits(r.queue_pos) : "—";
    const wait = r.queue_wait > 0 ? r.queue_wait + "s" : "—";
    h += `<tr class="${meta.cls}"><td class="st-mono">${esc((r.started || "").slice(11, 19))}</td>` +
      `<td>${fmtElapsed(r.started)}</td><td>${esc(r.channel)}</td><td class="st-mono">${esc(r.model)}</td>` +
      `<td><span class="st-live-tag">${esc(meta.tag)}</span></td><td>${pos}</td><td>${wait}</td>` +
      `<td>${r.attempts || 1}</td><td class="st-live-note" title="${esc(last)}">${esc(last)}</td></tr>`;
  }
  box.innerHTML = rows.length ? h : `<tr><td class="st-empty" colspan="9">当前没有进行中的请求</td></tr>`;
}

function startLivePoll() {
  if (liveTimer) return;
  refreshLive();
  liveTimer = setInterval(refreshLive, LIVE_POLL_MS);
}
function stopLivePoll() {
  if (liveTimer) { clearInterval(liveTimer); liveTimer = null; }
}

// 异常账号（三级：disabled 已停用 / cool 整号冷却 / err 错误累计；无异常整块隐藏）
const ABNORMAL_META = {
  disabled: { cls: "st-ab-disabled", tag: "已停用", tip: "账号被上游停用（如登录态失效），需人工处理" },
  cool: { cls: "st-ab-cool", tag: "整号冷却", tip: "账号整体进入冷却（限流/余额不足/账号故障），解冻前不可用" },
  err: { cls: "st-ab-err", tag: "错误累计", tip: "连续错误累计中，达阈值将触发冷却" },
};

function renderAbnormal(rows) {
  if (!rows.length) { $("stAbnormal").innerHTML = ""; $("stAbnormal").className = "st-abnormal"; return; }
  const counts = { disabled: 0, cool: 0, err: 0 };
  rows.forEach((r) => { if (counts[r.level] != null) counts[r.level]++; });
  // 摘要徽章只显示存在的级别（顺序固定：停用→整号冷却→错误累计）
  let sumHtml = "";
  ["disabled", "cool", "err"].forEach((lv) => {
    if (counts[lv] > 0) {
      sumHtml += `<span class="st-ab-sum ${ABNORMAL_META[lv].cls}" title="${esc(ABNORMAL_META[lv].tip)}">` +
        `${ABNORMAL_META[lv].tag} ${counts[lv]}</span>`;
    }
  });
  let html = `<div class="st-block-title">异常账号<span class="st-hint">限流 / 冻结 / 亚健康 · 共 ${rows.length} 个</span>` +
    `<span class="st-ab-sums">${sumHtml}</span></div>` +
    `<div class="st-scroll st-ab-scroll">` +
    `<div class="st-ab-row st-ab-head">` +
    `<span class="st-ab-idx">#</span>` +
    `<span class="st-ab-tag-slot">状态</span>` +
    `<span class="st-ab-plat-slot">平台</span>` +
    `<span class="st-ab-name">账号</span>` +
    `<span class="st-ab-reason">原因</span>` +
    `<span class="st-ab-until">解冻</span>` +
    `<span class="st-ab-credit">余额</span>` +
    `</div>`;
  rows.forEach((r, i) => {
    const meta = ABNORMAL_META[r.level] || ABNORMAL_META.err;
    let reason = r.reason || meta.tag;
    if (r.level === "err" && r.err_count) reason = `连续错误 ${fmtCredits(r.err_count)} 次`;
    html += `<div class="st-ab-row ${meta.cls}">` +
      `<span class="st-ab-idx">${i + 1}</span>` +
      `<span class="st-ab-tag-slot"><span class="st-ab-tag" title="${esc(meta.tip)}">${meta.tag}</span></span>` +
      `<span class="st-ab-plat-slot"><span class="badge ${chClass(r.group)}">${esc(chLabel(r.group))}</span></span>` +
      `<span class="st-ab-name" title="${esc(r.uid)}">${esc(r.nickname || r.uid)}</span>` +
      `<span class="st-ab-reason" title="${esc(reason)}">${esc(reason)}</span>` +
      `<span class="st-ab-until">${r.until ? esc(r.until) : "—"}</span>` +
      `<span class="st-ab-credit">${r.credits > 0 ? fmtCredits(r.credits) + "分" : "—"}</span>` +
      `</div>`;
  });
  html += `</div>`;
  $("stAbnormal").innerHTML = html;
  $("stAbnormal").className = "st-abnormal st-block";
}

// 运行日志（app.log 尾部）：级别判定顺序有讲究——
// ① code=9074/ok=false 先标红（「签到完成」话术里装的业务失败，先判会被误放行为 info）；
// ② 已签到 是良性重复签到，不标红；③ 汇总成功行（含 failed=N）仍按成功放行。
function appLogLevel(text) {
  if (/code=9074|ok=false/.test(text)) return "err";
  if (/完成|成功|ok=true|已签到/.test(text)) return "info";
  if (/PANIC|FATAL|失败|错误|无效|Error|failed/.test(text)) return "err";
  if (/warn|WARN|警告/.test(text)) return "warn";
  return "info";
}

let applogFirst = true;
function renderAppLog(lines) {
  // 用户上翻阅读时不打扰；贴底（或首帧）才自动吸底
  const box = $("stApplog");
  const pinned = applogFirst || box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  applogFirst = false;
  let html = "";
  const start = Math.max(0, lines.length - APPLOG_MAX_LINES);
  for (let i = start; i < lines.length; i++) {
    const line = lines[i];
    if (!line) continue;
    // Go log 前缀 "2026/09/22 15:04:05.123456 " → 展示 MM-DD HH:MM:SS
    const m = line.match(/^(\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?)\s(.*)$/);
    const cls = `st-logline st-log-${appLogLevel(line)}`;
    if (m) {
      html += `<div class="${cls}"><span class="st-log-time">${esc(m[1].slice(5, 19))}</span>${esc(m[2])}</div>`;
    } else {
      html += `<div class="${cls}">${esc(line)}</div>`;
    }
  }
  box.innerHTML = html || `<div class="st-empty">暂无日志</div>`;
  if (pinned) box.scrollTop = box.scrollHeight;
}

// 里程碑 toast：浏览器通知优先，降级面板 toast（复用上游 #toast）；localStorage 按 id 去重
function statsToastSeen() {
  try { return JSON.parse(localStorage.getItem(TOAST_SEEN_KEY) || "[]"); } catch (e) { return []; }
}
function statsToastMark(id) {
  const ids = statsToastSeen();
  ids.push(id);
  try { localStorage.setItem(TOAST_SEEN_KEY, JSON.stringify(ids.slice(-200))); } catch (e) { /* 忽略 */ }
}
function handleToasts(list) {
  let seen = {};
  try { statsToastSeen().forEach((x) => { seen[x] = 1; }); } catch (e) { seen = {}; }
  for (const t of list) {
    if (seen[t.id]) continue;
    showStatsNotification(t.title, t.body);
    statsToastMark(t.id);
  }
}
function showStatsNotification(title, body) {
  try {
    if ("Notification" in window) {
      if (Notification.permission === "granted") {
        new Notification(title, { body });
        return;
      }
      if (Notification.permission === "default") Notification.requestPermission();
    }
  } catch (e) { /* 降级面板提示 */ }
  toast(`${title}：${body}`);
}

