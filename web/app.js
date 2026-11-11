/* Qoder 网关控制台
   零依赖：DOM 由函数构造，状态集中在 S，路由用 pushState + 手写 go()。
   区块：原语 / 状态 / 请求 / 提示 / 路由 / 登录 / 账号页 / 模型页 / 密钥页 / 用量页 / 设置页
*/

/* ---------------- 原语 ---------------- */

function $(sel) { return document.querySelector(sel); }

function el(tag, text, cls) {
  const node = document.createElement(tag);
  if (text !== undefined && text !== null) node.textContent = String(text);
  if (cls) node.className = cls;
  return node;
}

function button(text, fn, cls) {
  const b = el("button", text, cls);
  b.type = "button";
  b.addEventListener("click", fn);
  return b;
}

function div(cls, children) {
  const node = el("div", null, cls);
  (children || []).forEach(function (c) { if (c) node.appendChild(c); });
  return node;
}

function pill(text, kind) { return el("span", text, "pill" + (kind ? " " + kind : "")); }

/* ---------------- 状态 ---------------- */

const S = {
  authed: false,
  page: "/",
  data: null,
  loginId: null,
  pollTimer: null,
  busy: false
};

const NAV = [
  { path: "/", label: "账号", icon: "T" },
  { path: "/models", label: "模型", icon: "↗" },
  { path: "/keys", label: "密钥", icon: "＋" },
  { path: "/usage", label: "用量", icon: "▾" },
  { path: "/settings", label: "设置", icon: "▴" }
];

const EDITION_LABEL = { domestic: "国内版", international: "国际版" };

/* ---------------- 请求 ---------------- */

async function api(path, body, method) {
  const opts = { method: method || (body === undefined ? "GET" : "POST"), headers: {}, credentials: "same-origin" };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  let payload = null;
  try { payload = await res.json(); } catch (e) { payload = null; }
  if (!res.ok) {
    const code = payload && payload.error ? (payload.error.code || payload.error.message) : "HTTP " + res.status;
    const err = new Error(code);
    err.status = res.status;
    throw err;
  }
  return payload;
}

function notice(message, bad) {
  const box = $("#notice");
  box.textContent = message;
  box.className = "notice" + (bad ? " bad" : "");
  box.hidden = false;
  clearTimeout(notice.timer);
  notice.timer = setTimeout(function () { box.hidden = true; }, 5000);
}

async function guard(fn) {
  if (S.busy) return;
  S.busy = true;
  try {
    await fn();
  } catch (e) {
    if (e.status === 401) {
      S.authed = false;
      renderLogin();
      return;
    }
    notice(e.message || "操作失败", true);
  } finally {
    S.busy = false;
  }
}

/* ---------------- 格式化 ---------------- */

function copyText(text) {
  if (!text) return;
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(function () { notice("已复制到剪贴板"); }, function () { notice("复制失败，请手动选中", true); });
    return;
  }
  notice("当前浏览器不支持自动复制，请手动选中", true);
}

function fmtTime(ms) {
  if (!ms) return "—";
  const d = new Date(ms);
  const pad = function (n) { return String(n).padStart(2, "0"); };
  return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes());
}

function fmtDate(ms) {
  if (!ms) return "—";
  const d = new Date(ms);
  return d.getFullYear() + "-" + String(d.getMonth() + 1).padStart(2, "0") + "-" + String(d.getDate()).padStart(2, "0");
}

function fmtNum(n) {
  if (n === null || n === undefined || n === "") return "—";
  return Number(n).toLocaleString("zh-CN");
}

function fmtQuota(q) {
  if (!q) return "—";
  if (q.remaining !== undefined && q.remaining !== null) {
    const used = q.used !== null && q.used !== undefined ? " / " + fmtNum(q.used) : "";
    const total = q.total !== null && q.total !== undefined ? " / " + fmtNum(q.total) : "";
    return fmtNum(q.remaining) + used + total;
  }
  if (q.total !== null && q.total !== undefined) return fmtNum(q.total);
  return "—";
}

function statusPill(acc) {
  if (acc.status === "disabled") return pill("已停用");
  if (acc.status === "needs_relogin") return pill("需重新登录", "bad");
  if (acc.cooling) return pill("冷却中", "warn");
  if (!acc.has_token && !acc.has_pat) return pill("无凭证", "bad");
  return pill("可用", "ok");
}

function checkinText(acc) {
  const map = { 1: "已领取", 2: "今日已签", 3: "无可领活动", 4: "失败" };
  if (!acc.checkin_day) return "—";
  return (map[acc.checkin_result] || "未处理") + " · " + acc.checkin_day;
}

/* ---------------- 提示条 / 侧栏 ---------------- */

function renderChrome() {
  const nav = $("#nav");
  nav.textContent = "";
  NAV.forEach(function (item) {
    const a = el("a", null, item.path === S.page ? "on" : "");
    a.href = item.path;
    a.appendChild(el("span", item.icon, "icon"));
    a.appendChild(el("span", item.label));
    a.addEventListener("click", function (ev) {
      ev.preventDefault();
      go(item.path);
    });
    nav.appendChild(a);
  });

  const right = $("#topRight");
  right.textContent = "";
  if (!S.authed) return;
  const counts = (S.data && S.data.counts) || {};
  right.appendChild(pill("国内 " + (counts.domestic || 0)));
  right.appendChild(pill("国际 " + (counts.international || 0)));
  if (S.data && S.data.in_flight) right.appendChild(pill("进行中 " + S.data.in_flight, "warn"));
  right.appendChild(button("退出", function () {
    guard(async function () {
      await api("/admin/logout", {});
      S.authed = false;
      renderLogin();
    });
  }, "ghost tiny"));

  const foot = $("#sideFoot");
  foot.textContent = "";
  if (S.data) {
    foot.appendChild(el("div", "版本 " + (S.data.version || "—")));
    foot.appendChild(el("div", "签到 " + (S.data.checkin_enabled ? S.data.checkin_at : "已关闭")));
    foot.appendChild(el("div", "反代 " + (S.data.proxy_running ? "运行中" : "未启用")));
  }
}

/* ---------------- 路由 ---------------- */

const PAGES = {
  "/": accountsPage,
  "/models": modelsPage,
  "/keys": keysPage,
  "/usage": usagePage,
  "/settings": settingsPage
};

function go(path) {
  if (path === S.page) return;
  history.pushState({ path: path }, "", path);
  S.page = path;
  render();
}

window.addEventListener("popstate", function (ev) {
  S.page = (ev.state && ev.state.path) || location.pathname || "/";
  render();
});

/* ---------------- 渲染 ---------------- */

function renderFooter(main) {
  const foot = el("footer");
  foot.appendChild(document.createTextNode("Qoder 网关 · 单容器部署 · 移植自 Qoder-Switch2api · 服务地址 "));
  const addr = el("code");
  addr.textContent = location.origin;
  foot.appendChild(addr);
  main.appendChild(foot);
}

function render() {
  renderChrome();
  const main = $("#main");
  main.textContent = "";
  if (!S.authed) return;
  const page = PAGES[S.page] || PAGES["/"];
  const node = page(S.page);
  node.classList.add("rise");
  main.appendChild(node);
  renderFooter(main);
  window.scrollTo({ top: 0 });
}

async function load() {
  S.data = await api("/admin/state");
}

async function refresh() {
  await guard(async function () {
    await load();
    render();
  });
}

/* ---------------- 登录 ---------------- */

function renderLogin() {
  document.body.textContent = "";
  const wrap = el("div", null, "login-wrap");
  const card = el("div", null, "login-card");
  card.appendChild(el("h1", "Qoder 网关"));
  card.appendChild(el("p", "请输入管理密码。密码来自环境变量 RELAY_ADMIN_PASSWORD。", "sub"));

  const form = el("form");
  const input = el("input");
  input.type = "password";
  input.placeholder = "管理密码";
  input.autocomplete = "current-password";
  const err = el("div", null, "err");
  const submit = el("button", "登录", null, "primary");
  submit.type = "submit";

  form.appendChild(input);
  form.appendChild(err);
  form.appendChild(submit);
  form.addEventListener("submit", async function (ev) {
    ev.preventDefault();
    err.textContent = "";
    submit.disabled = true;
    try {
      await api("/admin/login", { password: input.value });
      S.authed = true;
      await boot();
    } catch (e) {
      err.textContent = e.message === "invalid_admin_password" ? "密码不正确" : (e.message || "登录失败");
      submit.disabled = false;
    }
  });

  card.appendChild(form);
  wrap.appendChild(card);
  document.body.appendChild(wrap);
  input.focus();
}

async function boot() {
  document.body.textContent = "";
  document.body.appendChild(buildShell());
  await load();
  S.page = location.pathname && PAGES[location.pathname] ? location.pathname : "/";
  render();
}

function buildShell() {
  const frag = document.createDocumentFragment();

  const header = el("header", null, "top");
  const brand = el("div", null, "brand");
  brand.appendChild(el("span", "Q", "mark"));
  const brandText = el("span", null, "brand-text");
  brandText.appendChild(el("span", "Qoder 网关"));
  brandText.appendChild(el("em", "relay"));
  brand.appendChild(brandText);
  header.appendChild(brand);
  header.appendChild(el("div", null, "top-right")).id = "topRight";
  frag.appendChild(header);

  const shell = el("div", null, "shell");
  const side = el("aside", null, "side");
  const nav = el("nav");
  nav.id = "nav";
  side.appendChild(nav);
  const foot = el("div", null, "side-foot");
  foot.id = "sideFoot";
  side.appendChild(foot);
  shell.appendChild(side);
  const main = el("main");
  main.id = "main";
  shell.appendChild(main);
  frag.appendChild(shell);

  const box = el("div", null, "notice");
  box.id = "notice";
  box.hidden = true;
  frag.appendChild(box);
  return frag;
}

/* ---------------- 账号页 ---------------- */

function pageHead(title, sub, actions) {
  const head = el("div", null, "page-head");
  const left = el("div");
  left.appendChild(el("h1", title));
  if (sub) left.appendChild(el("p", sub, "sub"));
  head.appendChild(left);
  if (actions) {
    const right = el("div", null, "row");
    actions.forEach(function (n) { right.appendChild(n); });
    head.appendChild(right);
  }
  return head;
}

function factsBlock(items, note) {
  const frag = document.createDocumentFragment();
  const dl = el("dl", null, "facts");
  items.forEach(function (item) {
    const cell = el("div");
    cell.appendChild(el("dt", item.label));
    const dd = el("dd", item.value, item.kind || "");
    cell.appendChild(dd);
    dl.appendChild(cell);
  });
  frag.appendChild(dl);
  if (note) frag.appendChild(el("p", note, "factNote"));
  return frag;
}

function accountsPage() {
  const wrap = el("div");
  const data = S.data || {};
  const accounts = data.accounts || [];
  const counts = data.counts || {};

  wrap.appendChild(pageHead("账号池", "两个档位各自独立选号；反代只使用「反代API」分组的账号。", [
    button("登录新账号", function () { openOAuthDialog(); }, "primary"),
    button("刷新", function () { refresh(); }, "ghost")
  ]));

  wrap.appendChild(factsBlock([
    { label: "国内版账号", value: fmtNum(counts.domestic || 0) },
    { label: "国际版账号", value: fmtNum(counts.international || 0) },
    { label: "国内可反代", value: fmtNum(counts.proxy_domestic || 0), kind: counts.proxy_domestic ? "green" : "" },
    { label: "国际可反代", value: fmtNum(counts.proxy_international || 0), kind: counts.proxy_international ? "green" : "" }
  ], "「可反代」表示账号有登录态且未被标记为桌面端专用或已停用。"));

  if (!accounts.length) {
    wrap.appendChild(emptyBlock(
      "还没有账号",
      "网关需要至少一个已登录的 Qoder 账号才能转发请求。",
      ["点击右上角「登录新账号」，在浏览器里完成 Qoder 登录", "回到本页等待账号出现，然后点「刷新额度」确认可用", "到「密钥」页创建一个 API Key，客户端用它调用 /v1"]
    ));
    return wrap;
  }

  const table = el("table");
  const thead = el("thead");
  const hr = el("tr");
  ["账号", "档位", "状态", "额度", "签到", "最近使用", "操作"].forEach(function (t) { hr.appendChild(el("th", t)); });
  thead.appendChild(hr);
  table.appendChild(thead);

  const tbody = el("tbody");
  accounts.forEach(function (acc) {
    const tr = el("tr");

    const nameCell = el("td");
    nameCell.appendChild(el("div", acc.label || acc.name || acc.email || acc.uid || "未命名"));
    const sub = el("div", null, "hint");
    const bits = [];
    if (acc.email) bits.push(acc.email);
    if (acc.phone) bits.push(acc.phone);
    if (acc.token_mask) bits.push(acc.token_mask);
    sub.textContent = bits.join(" · ") || acc.uid || "—";
    nameCell.appendChild(sub);
    tr.appendChild(nameCell);

    const edCell = el("td");
    edCell.appendChild(pill(EDITION_LABEL[acc.edition] || acc.edition));
    if (acc.group) edCell.appendChild(pill(acc.group));
    tr.appendChild(edCell);

    const statusCell = el("td");
    statusCell.appendChild(statusPill(acc));
    if (acc.last_error) {
      const e = el("div", null, "hint");
      e.textContent = acc.last_error;
      statusCell.appendChild(e);
    }
    tr.appendChild(statusCell);

    const quotaCell = el("td", fmtQuota(acc.quota));
    if (acc.plan && acc.plan.plan_tier_name) {
      const p = el("div", null, "hint");
      p.textContent = acc.plan.plan_tier_name + (acc.plan.end_date ? " · " + fmtDate(acc.plan.end_date) : "");
      quotaCell.appendChild(p);
    }
    tr.appendChild(quotaCell);

    tr.appendChild(el("td", checkinText(acc)));
    tr.appendChild(el("td", fmtTime(acc.last_used_at)));

    const actions = el("td", null, "actions");
    const group = el("div", null, "row end");

    group.appendChild(button("刷新额度", function () {
      guard(async function () {
        await api("/admin/account/credit", { id: acc.id });
        await load();
        render();
        notice("已刷新 " + (acc.label || acc.name || acc.uid));
      });
    }, "tiny"));

    group.appendChild(button("续期", function () {
      guard(async function () {
        await api("/admin/account/refresh", { id: acc.id });
        await load();
        render();
        notice("登录态已更新");
      });
    }, "tiny ghost"));

    group.appendChild(button("签到", function () {
      guard(async function () {
        const res = await api("/admin/account/checkin", { id: acc.id });
        await load();
        render();
        notice(res.message || "签到完成", !res.ok);
      });
    }, "tiny ghost"));

    group.appendChild(button(acc.status === "disabled" ? "启用" : "停用", function () {
      guard(async function () {
        await api("/admin/account", { id: acc.id, action: acc.status === "disabled" ? "enable" : "disable" });
        await load();
        render();
      });
    }, "tiny ghost"));

    group.appendChild(button("删除", function () {
      if (!confirm("删除账号 " + (acc.label || acc.name || acc.uid) + "？此操作不可撤销。")) return;
      guard(async function () {
        await api("/admin/account", { id: acc.id, action: "delete" });
        await load();
        render();
        notice("账号已删除");
      });
    }, "tiny"));

    actions.appendChild(group);
    tr.appendChild(actions);
    tbody.appendChild(tr);
  });
  table.appendChild(tbody);

  const scroll = el("div", null, "scroll");
  scroll.appendChild(table);
  wrap.appendChild(scroll);
  return wrap;
}

function emptyBlock(title, reason, steps) {
  const box = el("div", null, "empty");
  box.appendChild(el("strong", title));
  box.appendChild(el("p", reason));
  const ol = el("ol");
  steps.forEach(function (s) { ol.appendChild(el("li", s)); });
  box.appendChild(ol);
  return box;
}

/* ---------------- 登录对话框（设备流） ---------------- */

function openOAuthDialog() {
  const edition = prompt("选择档位：输入 domestic（国内版）或 international（国际版）", "domestic");
  if (edition !== "domestic" && edition !== "international") {
    notice("档位只能是 domestic 或 international", true);
    return;
  }
  guard(async function () {
    const start = await api("/admin/oauth/start", { edition: edition });
    const link = start.verification_uri;
    window.open(link, "_blank", "noopener");
    notice("已打开登录页，完成登录后本页会自动刷新");

    S.loginId = start.login_id;
    clearInterval(S.pollTimer);
    S.pollTimer = setInterval(function () {
      guard(async function () {
        const res = await api("/admin/oauth/poll", { login_id: S.loginId });
        if (res.status === "pending") return;
        clearInterval(S.pollTimer);
        S.pollTimer = null;
        S.loginId = null;
        if (res.status === "done") {
          await load();
          render();
          notice("账号已添加：" + ((res.account && (res.account.label || res.account.name || res.account.uid)) || ""));
        } else {
          notice(res.error || "登录失败", true);
        }
      });
    }, 2000);
  });
}

/* ---------------- 模型页 ---------------- */

function modelsPage() {
  const wrap = el("div");
  const models = (S.data && S.data.models) || [];

  wrap.appendChild(pageHead("模型目录", "默认提供 auto 与 lite；上游目录会缓存 10 分钟，可用别名覆盖展示名。", [
    button("重新拉取", function () {
      guard(async function () {
        await api("/admin/models/refresh", {});
        await load();
        render();
        notice("模型目录已更新");
      });
    }, "primary"),
    button("刷新", function () { refresh(); }, "ghost")
  ]));

  if (!models.length) {
    wrap.appendChild(emptyBlock("暂无模型", "还没有可展示的模型。", [
      "先在「账号」页登录至少一个账号",
      "点击「重新拉取」从上游获取模型目录",
      "调用 /v1/models 可确认客户端看到的列表"
    ]));
    return wrap;
  }

  const table = el("table");
  const thead = el("thead");
  const hr = el("tr");
  ["模型 Key", "展示名", "来源", "推理", "操作"].forEach(function (t) { hr.appendChild(el("th", t)); });
  thead.appendChild(hr);
  table.appendChild(thead);

  const tbody = el("tbody");
  models.forEach(function (m) {
    const tr = el("tr");
    const keyCell = el("td");
    keyCell.appendChild(el("code", m.id));
    tr.appendChild(keyCell);

    const nameCell = el("td");
    nameCell.appendChild(el("div", m.display_name || m.id));
    if (m.note) nameCell.appendChild(el("div", m.note, "hint"));
    tr.appendChild(nameCell);

    tr.appendChild(el("td", m.owned_by || m.source || "—"));

    const reasoning = el("td");
    reasoning.appendChild(pill(m.is_reasoning ? "是" : "否", m.is_reasoning ? "ok" : ""));
    tr.appendChild(reasoning);

    const actions = el("td", null, "actions");
    const group = el("div", null, "row end");

    group.appendChild(button("设别名", function () {
      const alias = prompt("展示名（留空表示清除）", m.display_name || "");
      if (alias === null) return;
      guard(async function () {
        await api("/admin/model", { id: m.id, setting: { alias: alias.trim(), disabled: !!m.disabled, note: m.note || "" } });
        await load();
        render();
        notice("已更新 " + m.id);
      });
    }, "tiny ghost"));

    group.appendChild(button(m.disabled ? "启用" : "隐藏", function () {
      guard(async function () {
        await api("/admin/model", { id: m.id, setting: { alias: m.display_name || "", disabled: !m.disabled, note: m.note || "" } });
        await load();
        render();
      });
    }, "tiny ghost"));

    actions.appendChild(group);
    tr.appendChild(actions);
    tbody.appendChild(tr);
  });
  table.appendChild(tbody);

  const scroll = el("div", null, "scroll");
  scroll.appendChild(table);
  wrap.appendChild(scroll);
  return wrap;
}

/* ---------------- 密钥页 ---------------- */

function keysPage() {
  const wrap = el("div");
  const keys = (S.data && S.data.keys) || [];

  wrap.appendChild(pageHead("API 密钥", "客户端用 Bearer 或 X-API-Key 携带；完整密钥见下表，请勿外泄。", [
    button("新建密钥", function () {
      const name = prompt("密钥名称", "默认密钥");
      if (name === null) return;
      guard(async function () {
        const res = await api("/admin/key", { action: "create", name: name });
        await load();
        render();
        notice("新密钥已创建：" + name);
        copyText(res.key.secret);
      });
    }, "primary"),
    button("刷新", function () { refresh(); }, "ghost")
  ]));

  if (!keys.length) {
    wrap.appendChild(emptyBlock("还没有密钥", "当前网关未启用鉴权，任何能访问端口的客户端都可调用 /v1。", [
      "点击「新建密钥」生成第一个 API Key",
      "把它配置到客户端（OpenAI 兼容：Authorization: Bearer sk-...）",
      "再次调用 /v1 时未携带有效密钥将被拒绝"
    ]));
    return wrap;
  }

  const table = el("table");
  const thead = el("thead");
  const hr = el("tr");
  ["名称", "密钥", "调用次数", "最近使用", "状态", "操作"].forEach(function (t) { hr.appendChild(el("th", t)); });
  thead.appendChild(hr);
  table.appendChild(thead);

  const tbody = el("tbody");
  keys.forEach(function (k) {
    const tr = el("tr");
    tr.appendChild(el("td", k.name));
    const fp = el("td", null, "keycell");
    fp.appendChild(el("code", k.secret || ""));
    fp.appendChild(button("复制", function () {
      copyText(k.secret || "");
    }, "tiny ghost"));
    tr.appendChild(fp);
    const calls = el("td", fmtNum(k.calls || 0), "num");
    tr.appendChild(calls);
    tr.appendChild(el("td", fmtTime(k.last_used_at)));

    const status = el("td");
    status.appendChild(pill(k.enabled ? "启用" : "停用", k.enabled ? "ok" : ""));
    tr.appendChild(status);

    const actions = el("td", null, "actions");
    const group = el("div", null, "row end");
    group.appendChild(button(k.enabled ? "停用" : "启用", function () {
      guard(async function () {
        await api("/admin/key", { action: "update", id: k.id, name: k.name, enabled: !k.enabled });
        await load();
        render();
      });
    }, "tiny ghost"));
    group.appendChild(button("删除", function () {
      if (!confirm("删除密钥 " + k.name + "？")) return;
      guard(async function () {
        await api("/admin/key", { action: "delete", id: k.id });
        await load();
        render();
        notice("密钥已删除");
      });
    }, "tiny"));
    actions.appendChild(group);
    tr.appendChild(actions);
    tbody.appendChild(tr);
  });
  table.appendChild(tbody);

  const scroll = el("div", null, "scroll");
  scroll.appendChild(table);
  wrap.appendChild(scroll);
  return wrap;
}

/* ---------------- 用量页 ---------------- */

function usagePage() {
  const wrap = el("div");
  const usage = (S.data && S.data.usage) || [];
  const log = (S.data && S.data.checkin_log) || [];

  let input = 0;
  let output = 0;
  let fails = 0;
  usage.forEach(function (u) {
    input += u.input_tokens || 0;
    output += u.output_tokens || 0;
    if (!u.success) fails++;
  });

  wrap.appendChild(pageHead("用量与日志", "最近 200 条调用与最近 50 条签到记录。", [
    button("清空用量", function () {
      if (!confirm("清空全部用量记录？")) return;
      guard(async function () {
        await api("/admin/usage/clear", {});
        await load();
        render();
        notice("用量已清空");
      });
    }, ""),
    button("刷新", function () { refresh(); }, "ghost")
  ]));

  wrap.appendChild(factsBlock([
    { label: "调用次数", value: fmtNum(usage.length) },
    { label: "输入 Token", value: fmtNum(input) },
    { label: "输出 Token", value: fmtNum(output) },
    { label: "失败次数", value: fmtNum(fails), kind: fails ? "warn" : "green" }
  ], "Token 由上游 usage 提供；上游未返回时按字符数估算。"));

  if (!usage.length) {
    wrap.appendChild(emptyBlock("还没有调用记录", "网关收到第一个请求后，这里会显示每次调用的模型、账号与耗时。", [
      "在「密钥」页创建 API Key",
      "用客户端向 /v1/chat/completions 发起一次请求",
      "返回本页点「刷新」查看记录"
    ]));
  } else {
    const table = el("table");
    const thead = el("thead");
    const hr = el("tr");
    ["时间", "模型", "账号", "档位", "输入", "输出", "耗时", "结果"].forEach(function (t) { hr.appendChild(el("th", t)); });
    thead.appendChild(hr);
    table.appendChild(thead);
    const tbody = el("tbody");
    usage.forEach(function (u) {
      const tr = el("tr");
      tr.appendChild(el("td", fmtTime(u.time)));
      const model = el("td");
      model.appendChild(el("code", u.model || "—"));
      tr.appendChild(model);
      tr.appendChild(el("td", u.account || "—"));
      tr.appendChild(el("td", EDITION_LABEL[u.edition] || u.edition || "—"));
      tr.appendChild(el("td", fmtNum(u.input_tokens), "num"));
      tr.appendChild(el("td", fmtNum(u.output_tokens), "num"));
      tr.appendChild(el("td", (u.duration_ms || 0) + " ms", "num"));
      const res = el("td");
      res.appendChild(pill(u.success ? "成功" : "失败", u.success ? "ok" : "bad"));
      if (u.error) {
        const e = el("div", null, "hint");
        e.textContent = u.error;
        res.appendChild(e);
      }
      tr.appendChild(res);
      tbody.appendChild(tr);
    });
    table.appendChild(tbody);
    const scroll = el("div", null, "scroll");
    scroll.appendChild(table);
    wrap.appendChild(scroll);
  }

  wrap.appendChild(el("h2", "签到日志"));
  if (!log.length) {
    wrap.appendChild(el("p", "暂无签到记录。自动签到按设置页的时间点执行，也可在账号页手动触发。", "hint"));
    return wrap;
  }
  const logTable = el("table");
  const lhead = el("thead");
  const lhr = el("tr");
  ["时间", "账号", "档位", "结果", "积分", "说明"].forEach(function (t) { lhr.appendChild(el("th", t)); });
  lhead.appendChild(lhr);
  logTable.appendChild(lhead);
  const lbody = el("tbody");
  log.forEach(function (e) {
    const tr = el("tr");
    tr.appendChild(el("td", fmtTime(e.time)));
    tr.appendChild(el("td", e.account || "—"));
    tr.appendChild(el("td", EDITION_LABEL[e.edition] || e.edition || "—"));
    const r = el("td");
    r.appendChild(pill(e.success ? "成功" : "失败", e.success ? "ok" : "bad"));
    tr.appendChild(r);
    tr.appendChild(el("td", e.credits ? "+" + fmtNum(e.credits) : "—", "num"));
    tr.appendChild(el("td", e.message || "—"));
    lbody.appendChild(tr);
  });
  logTable.appendChild(lbody);
  const lscroll = el("div", null, "scroll");
  lscroll.appendChild(logTable);
  wrap.appendChild(lscroll);
  return wrap;
}

/* ---------------- 设置页 ---------------- */

function settingsPage() {
  const wrap = el("div");
  const data = S.data || {};
  const proxy = data.proxy || {};

  wrap.appendChild(pageHead("设置", "反代监听、导入导出与运行信息。", [
    button("刷新", function () { refresh(); }, "ghost")
  ]));

  // 反代
  const proxyCard = el("div", null, "card");
  const head = el("div", null, "card-head");
  head.appendChild(el("div", "内置反代监听", "card-title"));
  head.appendChild(pill(data.proxy_running ? "运行中" : "未启用", data.proxy_running ? "ok" : ""));
  proxyCard.appendChild(head);
  proxyCard.appendChild(el("p", "开启后为两个档位各开一个端口，客户端直接指向 http://主机:端口/v1 即可，无需额外请求头。", "hint"));

  const form = el("div", null, "grid-2 mt");

  const hostLabel = el("label", null, "field");
  hostLabel.appendChild(el("span", "监听地址"));
  const hostInput = el("input");
  hostInput.value = proxy.host || "0.0.0.0";
  hostLabel.appendChild(hostInput);
  form.appendChild(hostLabel);

  const dLabel = el("label", null, "field");
  dLabel.appendChild(el("span", "国内版端口"));
  const dInput = el("input");
  dInput.type = "number";
  dInput.value = proxy.domestic_port || 7891;
  dLabel.appendChild(dInput);
  form.appendChild(dLabel);

  const iLabel = el("label", null, "field");
  iLabel.appendChild(el("span", "国际版端口"));
  const iInput = el("input");
  iInput.type = "number";
  iInput.value = proxy.international_port || 7892;
  iLabel.appendChild(iInput);
  form.appendChild(iLabel);

  proxyCard.appendChild(form);

  const row = el("div", null, "row mt-lg");
  row.appendChild(button(proxy.enabled ? "关闭反代" : "开启反代", function () {
    guard(async function () {
      await api("/admin/proxy", {
        enabled: !proxy.enabled,
        host: hostInput.value.trim(),
        domestic_port: Number(dInput.value),
        international_port: Number(iInput.value)
      });
      await load();
      render();
      notice(proxy.enabled ? "反代已关闭" : "反代已开启");
    });
  }, proxy.enabled ? "" : "primary"));
  row.appendChild(button("保存端口", function () {
    guard(async function () {
      await api("/admin/proxy", {
        enabled: !!proxy.enabled,
        host: hostInput.value.trim(),
        domestic_port: Number(dInput.value),
        international_port: Number(iInput.value)
      });
      await load();
      render();
      notice("监听配置已保存");
    });
  }, "ghost"));
  proxyCard.appendChild(row);
  wrap.appendChild(proxyCard);

  // 导入导出
  const ioCard = el("div", null, "card");
  ioCard.appendChild(el("div", "账号导入导出", "card-title"));
  ioCard.appendChild(el("p", "导出包含完整凭证，请仅保存在本地。导入支持粘贴 JSON 备份文件内容。", "hint"));

  const ioRow = el("div", null, "row mt");
  ioRow.appendChild(button("导出备份", function () {
    guard(async function () {
      const res = await fetch("/admin/accounts/export", { credentials: "same-origin" });
      if (!res.ok) throw new Error("导出失败");
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const a = el("a");
      a.href = url;
      a.download = "qoder-accounts.json";
      a.click();
      URL.revokeObjectURL(url);
      notice("备份已下载");
    });
  }, "primary"));
  ioRow.appendChild(button("导入备份", function () {
    const text = prompt("粘贴备份 JSON 内容");
    if (!text) return;
    guard(async function () {
      let parsed;
      try { parsed = JSON.parse(text); } catch (e) { throw new Error("JSON 解析失败"); }
      const res = await api("/admin/accounts/import", {
        edition: parsed.edition || (S.data && S.data.default_edition) || "domestic",
        accounts: parsed.accounts || []
      });
      await load();
      render();
      notice("已导入 " + res.imported + " 个账号");
    });
  }, "ghost"));
  ioRow.appendChild(button("导入客户端登录态", function () {
    const text = prompt("粘贴 Qoder 客户端登录态 JSON（含 security_oauth_token）");
    if (!text) return;
    guard(async function () {
      let parsed;
      try { parsed = JSON.parse(text); } catch (e) { throw new Error("JSON 解析失败"); }
      const one = {
        edition: parsed.edition || (S.data && S.data.default_edition) || "domestic",
        accounts: [{
          name: parsed.name,
          email: parsed.email,
          phone: parsed.phone,
          uid: parsed.uid || parsed.id,
          security_token: parsed.security_oauth_token || parsed.securityOauthToken,
          refresh_token: parsed.refresh_token || parsed.refreshToken,
          pat: parsed.pat || parsed.personal_token,
          user_type: parsed.user_type || parsed.userType
        }]
      };
      const res = await api("/admin/accounts/import", one);
      await load();
      render();
      notice("已导入 " + res.imported + " 个账号");
    });
  }, "ghost"));
  ioCard.appendChild(ioRow);
  wrap.appendChild(ioCard);

  // 运行信息
  const infoCard = el("div", null, "card");
  infoCard.appendChild(el("div", "运行信息", "card-title"));
  const info = el("div", null, "facts");
  [
    { label: "版本", value: data.version || "—" },
    { label: "默认档位", value: EDITION_LABEL[data.default_edition] || data.default_edition || "—" },
    { label: "自动签到", value: data.checkin_enabled ? data.checkin_at : "已关闭" },
    { label: "进行中请求", value: fmtNum(data.in_flight || 0) }
  ].forEach(function (item) {
    const cell = el("div");
    cell.appendChild(el("dt", item.label));
    cell.appendChild(el("dd", item.value));
    info.appendChild(cell);
  });
  infoCard.appendChild(info);
  infoCard.appendChild(el("p", "环境变量：RELAY_ADMIN_PASSWORD（管理密码）、RELAY_MASTER_KEY（状态加密密钥）、RELAY_DEFAULT_EDITION、RELAY_CHECKIN_ENABLED。", "factNote"));
  wrap.appendChild(infoCard);

  return wrap;
}

/* ---------------- 启动 ---------------- */

(async function start() {
  try {
    const res = await api("/admin/session");
    if (res.authenticated) {
      S.authed = true;
      await boot();
      return;
    }
  } catch (e) {
    // 未登录属于正常路径
  }
  renderLogin();
})();
