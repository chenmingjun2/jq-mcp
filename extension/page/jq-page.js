/*
 * jqhelper page bridge.
 *
 * Injected into a www.joinquant.com tab in the MAIN world. It runs with the
 * page's origin and cookies, so it can call the site's own frontend endpoints
 * (no CORS, cookies attached automatically) and parse HTML with DOMParser.
 *
 * Entry point: window.__jqhelper.run(op, params).
 *
 * Endpoint map below was verified against the live site (2026-10). Every path
 * is kept in PATHS so a redesign can be adapted here without touching the
 * bridge protocol or the Go server.
 */
(() => {
  "use strict";

  const VERSION = "0.2.2";
  const BASE = "https://www.joinquant.com";

  const PATHS = {
    loginState: "/user/index/isLogin",
    // strategy
    strategyList: "/algorithm/index/list",
    strategyEdit: "/algorithm/index/edit",
    strategyNew: "/algorithm/index/new",
    strategySave: "/algorithm/index/save",
    strategyDel: "/algorithm/index/del",
    cancel: "/algorithm/index/cancel",
    // folders
    addFile: "/algorithm/index/AddFile",
    updateFile: "/algorithm/index/UpdateFile",
    algoToFile: "/algorithm/index/AlgorithmToFile",
    fileToFile: "/algorithm/index/FileToFile",
    // backtest
    build: "/algorithm/index/build",
    backtestList: "/algorithm/backtest/list",
    backtestBuildList: "/algorithm/backtest/buildList",
    backtestDetail: "/algorithm/backtest/detail",
    backtestSource: "/algorithm/backtest/source",
    backtestStats: "/algorithm/backtest/stats",
    backtestResult: "/algorithm/backtest/result",
    backtestLog: "/algorithm/backtest/log",
    backtestError: "/algorithm/backtest/error",
    backtestTrades: "/algorithm/backtest/transactionInfo",
    backtestPositions: "/algorithm/backtest/positionInfo",
    backtestRisk: "/algorithm/backtest/risk",
    backtestDel: "/algorithm/backtest/del",
  };

  // --------------------------------------------------------------------- utils

  function fail(code, message, details) {
    const e = new Error(message);
    e.code = code;
    if (details) e.details = details;
    throw e;
  }

  function num(value) {
    if (value === undefined || value === null) return null;
    const n = parseFloat(String(value).replace(/,/g, ""));
    return Number.isFinite(n) ? n : null;
  }

  function normalize(text) {
    return (text || "").replace(/\s+/g, " ").trim();
  }

  function base64Utf8(str) {
    const bytes = new TextEncoder().encode(str || "");
    let binary = "";
    const chunk = 0x8000;
    for (let i = 0; i < bytes.length; i += chunk) {
      binary += String.fromCharCode.apply(null, bytes.subarray(i, i + chunk));
    }
    return btoa(binary);
  }

  function parseDoc(html) {
    return new DOMParser().parseFromString(html || "", "text/html");
  }

  function looksLikeLogin(html) {
    // Match the login form / login page specifically. A plain "/user/login"
    // substring is not enough: every authenticated page links to
    // /user/login/logout and would otherwise look like a login page.
    return /CyLoginForm\[username\]|action=["']\/user\/login\/doLogin|用户登录\s*-\s*JoinQuant/.test(html || "");
  }

  // --------------------------------------------------------------- http helper

  async function req(path, opts = {}) {
    const { method = "GET", params, form, referrer, headers = {}, raw = false } = opts;
    let url = path.startsWith("http") ? path : BASE + path;
    if (params) {
      const qs = new URLSearchParams();
      for (const [k, v] of Object.entries(params)) {
        if (v !== undefined && v !== null && v !== "") qs.append(k, String(v));
      }
      const s = qs.toString();
      if (s) url += (url.includes("?") ? "&" : "?") + s;
    }

    const init = {
      method,
      credentials: "include",
      cache: "no-store",
      headers: { "X-Requested-With": "XMLHttpRequest", ...headers },
    };
    if (referrer) init.referrer = referrer;
    if (form) {
      init.headers["Content-Type"] = "application/x-www-form-urlencoded; charset=UTF-8";
      const body = new URLSearchParams();
      for (const [k, v] of Object.entries(form)) {
        if (v !== undefined && v !== null) body.append(k, String(v));
      }
      init.body = body.toString();
    }

    let resp;
    try {
      resp = await fetch(url, init);
    } catch (e) {
      fail("network_error", "请求失败: " + path + " (" + (e && e.message) + ")");
    }
    if (resp.status === 401 || resp.status === 403) fail("not_authenticated", "聚宽未登录或登录已过期");
    if (resp.status === 404) fail("not_found", "资源不存在: " + path);
    if (!resp.ok) fail("api_error", "请求失败 HTTP " + resp.status + ": " + path);

    const text = await resp.text();
    if (raw) return text;
    const ct = resp.headers.get("content-type") || "";
    const trimmed = text.trim();
    if (ct.includes("application/json") || trimmed.startsWith("{") || trimmed.startsWith("[")) {
      try {
        return JSON.parse(text);
      } catch (_) {
        /* fall through */
      }
    }
    return { __html: text };
  }

  function ok(data) {
    return !!(data && (String(data.status) === "0" || data.code === "00000" || data.code === 0));
  }

  // -------------------------------------------------------------- html parsing

  function collectForm(doc) {
    const fields = {};
    doc.querySelectorAll("input[name]").forEach((el) => {
      fields[el.name] = el.value;
    });
    doc.querySelectorAll("textarea[name]").forEach((el) => {
      fields[el.name] = el.value;
    });
    return fields;
  }

  function extractStrategyId(data) {
    if (!data || typeof data !== "object") return "";
    const inner = data.data || {};
    return String(inner.algorithmId || inner.id || data.algorithmId || data.id || "");
  }

  function parseList(html) {
    if (looksLikeLogin(html)) fail("not_authenticated", "聚宽未登录或登录已过期");
    const doc = parseDoc(html);
    const items = [];
    const folders = [];
    doc.querySelectorAll("tr.algorithm_list").forEach((tr) => {
      const selectTd = tr.querySelector("td.select-box, td.un-selectbox");
      const folderId = selectTd ? selectTd.getAttribute("_fid") || selectTd.getAttribute("_fId") || "" : "";
      const internalId =
        selectTd ? selectTd.getAttribute("_algorithmid") || selectTd.getAttribute("_algorithmId") || "" : "";
      const tds = [...tr.querySelectorAll("td")].map((td) => normalize(td.textContent));
      const name = tds[1] || "";
      const type = tds[2] || "";
      if (folderId && type === "文件夹") {
        folders.push({ fid: folderId, name });
        return;
      }
      const link = tr.querySelector('a[href*="algorithmId="]');
      let id = "";
      if (link) {
        try {
          id = new URL(link.getAttribute("href"), BASE).searchParams.get("algorithmId") || "";
        } catch (_) {
          id = "";
        }
      }
      if (!id) return;
      items.push({
        id,
        internalId,
        name,
        type: type || "Code",
        updatedAt: tds[3] || "",
        runCount: num(tds[4]),
        backtestCount: num(tds[5]),
      });
    });
    return { items, folders, maxPage: maxListPage(doc) };
  }

  function maxListPage(doc) {
    let max = 1;
    doc.querySelectorAll('a[href*="/algorithm/index/list"]').forEach((a) => {
      try {
        const page = parseInt(new URL(a.getAttribute("href"), BASE).searchParams.get("page") || "1", 10);
        if (Number.isFinite(page) && page > max) max = page;
      } catch (_) {
        /* ignore */
      }
    });
    return max;
  }

  async function fetchListPages(fid) {
    const firstHtml = await req(PATHS.strategyList, { params: fid ? { fId: fid } : {}, raw: true });
    const first = parseList(firstHtml);
    const items = [...first.items];
    const folders = [...first.folders];
    for (let page = 2; page <= first.maxPage; page++) {
      const html = await req(PATHS.strategyList, { params: fid ? { fId: fid, page } : { page }, raw: true });
      const p = parseList(html);
      items.push(...p.items);
      folders.push(...p.folders);
    }
    return { items, folders };
  }

  function parseStrategyEdit(html, requestedId) {
    if (looksLikeLogin(html)) fail("not_authenticated", "聚宽未登录或登录已过期");
    const doc = parseDoc(html);
    const fields = collectForm(doc);
    const idEl = doc.querySelector("#algorithmId, input[name='algorithmId']");
    const btEl = doc.querySelector("#backtestId, input[name='backtestId']");
    const saveId = fields["algorithm[algorithmId]"] || (idEl ? idEl.value : "") || requestedId;
    return {
      id: requestedId,
      saveId,
      backtestId: btEl ? btEl.value : "",
      name: fields["algorithm[name]"] || "",
      code: fields["algorithm[code]"] || "",
      form: fields,
    };
  }

  function parseStatus(rawStatus, cells) {
    const text = cells.join(" ");
    if (text.includes("完成")) return "done";
    if (text.includes("失败")) return "failed";
    if (text.includes("取消")) return "cancelled";
    if (text.includes("运行") || text.includes("回测中") || text.includes("进行中")) return "running";
    const map = { "0": "running", "1": "failed", "2": "done", "3": "cancelled" };
    return map[String(rawStatus)] || String(rawStatus || "");
  }

  function parseBacktestList(html, strategyId) {
    if (looksLikeLogin(html)) fail("not_authenticated", "聚宽未登录或登录已过期");
    const doc = parseDoc(html);
    const items = [];
    doc.querySelectorAll("tr.backtest-tr").forEach((tr) => {
      const attrs = {};
      for (const a of tr.attributes) attrs[a.name.toLowerCase()] = a.value;
      const tds = [...tr.querySelectorAll("td")].map((td) => normalize(td.textContent));
      const src = tr.querySelector("input.source-code");
      const offset = tds.length && /^\d+$/.test(tds[0]) ? 0 : 1;
      const range = tds[offset + 3] || "";
      const dates = range.match(/\d{4}-\d{2}-\d{2}/g) || [];
      items.push({
        id: attrs["_backtestid2"] || attrs["_backtestid"] || "",
        listId: attrs["_backtestid"] || "",
        sourceId: src ? src.getAttribute("_backtestId") || src.getAttribute("_backtestid") || "" : "",
        strategyId,
        name: tds[offset + 1] || "",
        status: parseStatus(attrs["_status"], tds),
        startDate: dates[0] || "",
        endDate: dates[1] || "",
        capital: num(tds[offset + 4]),
        frequency: tds[offset + 6] || "",
        metrics: {
          algorithmReturn: tds[offset + 7] || "",
          benchmarkReturn: tds[offset + 8] || "",
          maxDrawdown: tds[offset + 9] || "",
        },
        submittedAt: tds[offset + 2] || "",
      });
    });
    return items;
  }

  function unwrap(payload) {
    if (payload && typeof payload === "object" && payload.data && typeof payload.data === "object") {
      return payload.data;
    }
    return payload && typeof payload === "object" ? payload : {};
  }

  function hasCoreMetrics(metrics) {
    return (
      metrics && typeof metrics === "object" &&
      ["annual_algo_return", "sharpe"].every((k) => metrics[k] !== undefined && metrics[k] !== null)
    );
  }

  function hasAnyMetrics(metrics) {
    if (!metrics || typeof metrics !== "object") return false;
    return ["annual_algo_return", "algorithm_return", "max_drawdown", "sharpe", "trading_days"].some(
      (k) => metrics[k] !== undefined && metrics[k] !== null
    );
  }

  // ----------------------------------------------------------------- ops: auth

  async function authStatus() {
    const data = await req(PATHS.loginState);
    const raw = data && data.__html ? {} : data || {};
    const info = raw.data || raw;
    return { loggedIn: info.isLogin === 1 || info.isLogin === "1", raw };
  }

  // ------------------------------------------------------------- ops: strategy

  async function strategyList(params) {
    const rootFid = params.fid || "";
    const first = await fetchListPages(rootFid);
    let items = first.items;
    const folders = [...first.folders];

    if (params.all) {
      const seen = new Set();
      const queue = first.folders.map((f) => f.fid);
      while (queue.length) {
        const fid = queue.shift();
        if (!fid || seen.has(fid)) continue;
        seen.add(fid);
        const page = await fetchListPages(fid);
        page.items.forEach((it) => {
          it.folderId = fid;
          items.push(it);
        });
        page.folders.forEach((f) => {
          folders.push(f);
          queue.push(f.fid);
        });
      }
    }

    const sort = params.sort || "updated";
    if (sort === "name") items.sort((a, b) => String(a.name).localeCompare(String(b.name)));
    else if (sort === "updated") items.sort((a, b) => String(b.updatedAt).localeCompare(String(a.updatedAt)));
    if (!params.all && params.limit > 0) items = items.slice(0, params.limit);

    return { items, folders };
  }

  async function strategyFind(params) {
    const name = params.name || "";
    const list = await strategyList({ all: true, limit: 0 });
    const exact = list.items.filter((x) => x.name === name);
    const partial = name ? list.items.filter((x) => x.name.includes(name)) : [];
    return { exact, partial, matched: exact[0] || partial[0] || null };
  }

  async function strategyGet(params) {
    const id = params.strategyId;
    const html = await req(PATHS.strategyEdit, { params: { algorithmId: id }, raw: true });
    const detail = parseStrategyEdit(html, id);
    const out = {
      id: detail.id,
      saveId: detail.saveId,
      backtestId: detail.backtestId,
      name: detail.name,
      type: "Code",
    };
    if (params.includeCode) out.code = detail.code;
    return out;
  }

  async function strategySave(params) {
    const id = params.strategyId;
    const html = await req(PATHS.strategyEdit, { params: { algorithmId: id }, raw: true });
    const detail = parseStrategyEdit(html, id);
    const form = { ...detail.form };
    form["algorithm[algorithmId]"] = detail.saveId;
    if (params.name) form["algorithm[name]"] = params.name;
    if (params.code !== undefined && params.code !== null) {
      form["algorithm[code]"] = base64Utf8(params.code);
      form["encrType"] = "base64";
    }
    const data = await req(PATHS.strategySave, {
      method: "POST",
      form,
      referrer: BASE + "/algorithm/index/edit?algorithmId=" + encodeURIComponent(id),
    });
    const savedId = extractStrategyId(data) || detail.saveId;
    const finalName = form["algorithm[name]"] || "";
    let resolvedId = id;
    if (finalName) {
      const list = await strategyList({ all: true, limit: 0 });
      const found = list.items.find((x) => x.name === finalName);
      if (found) resolvedId = found.id;
    }
    return { id: resolvedId, saveId: savedId, name: finalName };
  }

  async function strategyCreate(params) {
    const name = params.name;
    const code = params.code;
    const form = {};
    if (code !== undefined && code !== null) form.code = code; // /new expects plaintext code
    const query = params.folderId ? { fId: params.folderId } : undefined;
    const data = await req(PATHS.strategyNew, {
      method: "POST",
      params: query,
      form,
      referrer: BASE + "/algorithm/index/list",
    });
    let id = extractStrategyId(data);
    if (!id) {
      const list = await strategyList({ all: true, limit: 0 });
      id = list.items.length ? list.items[0].id : "";
    }
    if (name && id) {
      await strategySave({ strategyId: id, name, code });
    }
    if (name) {
      const list = await strategyList({ all: true, limit: 0 });
      const found = list.items.find((x) => x.name === name);
      if (found) id = found.id;
    }
    return { id, name, type: params.type || "stock" };
  }

  async function strategyDelete(params) {
    const id = params.strategyId;
    const list = await strategyList({ all: true, limit: 0 });
    const found = list.items.find((x) => x.id === id || x.internalId === id);
    const internalId = found ? found.internalId || found.id : id;
    const data = await req(PATHS.strategyDel, {
      method: "POST",
      form: { algorithmId: internalId },
      referrer: BASE + "/algorithm/index/list",
    });
    return { ok: ok(data), id, internalId, raw: data };
  }

  async function strategyMove(params) {
    const ids = Array.isArray(params.strategyIds) ? params.strategyIds.join(",") : String(params.strategyIds || "");
    const list = await strategyList({ all: true, limit: 0 });
    const internalIds = ids
      .split(",")
      .map((id) => {
        const found = list.items.find((x) => x.id === id || x.internalId === id);
        return found ? found.internalId || found.id : id;
      })
      .join(",");
    const data = await req(PATHS.algoToFile, {
      method: "POST",
      params: { ids: internalIds, fId: params.folderId || "" },
      referrer: BASE + "/algorithm/index/list",
    });
    return { ok: ok(data), ids: internalIds, folderId: params.folderId || "", raw: data };
  }

  // ------------------------------------------------------------- ops: folders

  async function folderList(params) {
    const page = await fetchListPages(params.fid || "");
    const queue = [...page.folders];
    const seen = new Set();
    const all = [];
    while (queue.length) {
      const f = queue.shift();
      if (!f.fid || seen.has(f.fid)) continue;
      seen.add(f.fid);
      all.push(f);
      if (params.recursive) {
        const sub = await fetchListPages(f.fid);
        sub.folders.forEach((x) => queue.push(x));
      }
    }
    return { folders: all };
  }

  async function folderCreate(params) {
    const data = await req(PATHS.addFile, {
      method: "POST",
      params: { pId: params.parentId || "" },
      form: { name: params.name },
      referrer: BASE + "/algorithm/index/list",
    });
    return { ok: ok(data) || !!data, name: params.name, parentId: params.parentId || "", raw: data };
  }

  // ------------------------------------------------------------- ops: backtest

  async function resolveInternalBacktestId(id) {
    const html = await req(PATHS.backtestDetail, { params: { backtestId: id }, raw: true });
    if (looksLikeLogin(html)) fail("not_authenticated", "聚宽未登录或登录已过期");
    const doc = parseDoc(html);
    const el = doc.querySelector("#backtestId");
    const inner = el ? el.value : "";
    return { internalId: inner || id, doc };
  }

  async function backtestRun(params) {
    const id = params.strategyId;
    const html = await req(PATHS.strategyEdit, { params: { algorithmId: id }, raw: true });
    const detail = parseStrategyEdit(html, id);
    const form = { ...detail.form };
    form["algorithm[algorithmId]"] = detail.saveId;
    form["algorithm[code]"] = base64Utf8(detail.code || "");
    form["encrType"] = "base64";
    if (params.start) form["backtest[startTime]"] = params.start + " 00:00:00";
    if (params.end) form["backtest[endTime]"] = params.end + " 23:59:59";
    if (params.capital) form["backtest[baseCapital]"] = String(params.capital);
    form["backtest[frequency]"] = params.frequency === "minute" ? "minute" : "day";
    form["backtest[type]"] = params.compile ? "1" : "0";

    const data = await req(PATHS.build, {
      method: "POST",
      form,
      referrer: BASE + "/algorithm/index/edit?algorithmId=" + encodeURIComponent(id),
    });
    const inner = (data && data.data) || {};
    const detailId = String(inner.backtestId_ || inner.backtestId || "");
    const listId = String(inner.backtestId || "");
    if (!detailId && !listId) {
      const msg = data && data.msg ? data.msg : JSON.stringify(data || {});
      const details = { raw: data };
      // JoinQuant embeds the account's concurrency cap in the rejection text,
      // e.g. "当前并行编译或回测数量最多2个。" Expose it structurally.
      const mm = String(msg).match(/最多\s*(\d+)\s*个/);
      if (mm) details.maxConcurrent = parseInt(mm[1], 10);
      fail("api_error", "创建回测失败: " + msg, details);
    }
    return { id: detailId, listId, status: "running" };
  }

  async function backtestList(params) {
    const path = params.compile ? PATHS.backtestBuildList : PATHS.backtestList;
    const html = await req(path, { params: { algorithmId: params.strategyId }, raw: true });
    let items = parseBacktestList(html, params.strategyId);
    if (params.status && params.status !== "all") items = items.filter((x) => x.status === params.status);
    if (params.limit > 0) items = items.slice(0, params.limit);
    return { items };
  }

  async function backtestGet(params) {
    const { internalId } = await resolveInternalBacktestId(params.backtestId);
    const source = await req(PATHS.backtestSource, { params: { backtestId: internalId } });
    const stats = await req(PATHS.backtestStats, { params: { backtestId: internalId } });
    const metrics = unwrap(stats);
    return {
      id: params.backtestId,
      listId: internalId,
      status: hasCoreMetrics(metrics) ? "done" : "running",
      code: (source && source.data && source.data.source) || "",
      metrics,
    };
  }

  async function backtestStats(params) {
    const { internalId } = await resolveInternalBacktestId(params.backtestId);
    const payload = await req(PATHS.backtestStats, { params: { backtestId: internalId } });
    return { id: params.backtestId, resolvedId: internalId, metrics: unwrap(payload), raw: payload };
  }

  async function backtestResult(params) {
    const { internalId } = await resolveInternalBacktestId(params.backtestId);
    const offset = params.offset || 0;
    const userRecordOffset = params.userRecordOffset || 0;
    const payload = await req(PATHS.backtestResult, {
      params: { backtestId: internalId, offset, userRecordOffset, ajax: 1 },
    });
    const data = unwrap(payload);
    return { id: params.backtestId, resolvedId: internalId, offset, userRecordOffset, state: data.state, result: data.result, userRecord: data.userRecord ?? null };
  }

  async function backtestLogs(params) {
    const { internalId } = await resolveInternalBacktestId(params.backtestId);
    const path = params.error ? PATHS.backtestError : PATHS.backtestLog;
    const logs = [];
    let cur = params.offset || 0;
    let state;
    let max = false;
    for (let i = 0; i < 50; i++) {
      const query = { backtestId: internalId };
      if (!params.error) query.offset = cur;
      const payload = await req(path, { params: query });
      const data = unwrap(payload);
      const arr = Array.isArray(data.logArr) ? data.logArr : [];
      logs.push(...arr.map((x) => String(x)));
      if (data.state !== undefined) state = data.state;
      if (data.max) max = true;
      if (params.error || !params.all || arr.length === 0 || max) break;
      cur += arr.length;
    }
    return {
      id: params.backtestId,
      resolvedId: internalId,
      kind: params.error ? "error" : "log",
      offset: params.offset || 0,
      nextOffset: (params.offset || 0) + logs.length,
      state,
      max,
      logs,
    };
  }

  async function backtestTrades(params) {
    const { internalId } = await resolveInternalBacktestId(params.backtestId);
    const payload = await req(PATHS.backtestTrades, { params: { backtestId: internalId } });
    const data = unwrap(payload);
    const items = Array.isArray(data.transaction) ? data.transaction : [];
    return { id: params.backtestId, resolvedId: internalId, status: data.status, count: items.length, items };
  }

  async function backtestPositions(params) {
    const { internalId } = await resolveInternalBacktestId(params.backtestId);
    const payload = await req(PATHS.backtestPositions, { params: { backtestId: internalId } });
    const data = unwrap(payload);
    const items = Array.isArray(data.position) ? data.position : [];
    return { id: params.backtestId, resolvedId: internalId, status: data.status, count: items.length, items };
  }

  async function backtestMonthly(params) {
    const { internalId } = await resolveInternalBacktestId(params.backtestId);
    const payload = await req(PATHS.backtestRisk, { params: { backtestId: internalId } });
    const data = unwrap(payload);
    return { id: params.backtestId, resolvedId: internalId, risk: data.risk || data };
  }

  async function backtestCancel(params) {
    const { internalId } = await resolveInternalBacktestId(params.backtestId);
    const data = await req(PATHS.cancel, {
      method: "POST",
      form: { backtestId: internalId },
      referrer: BASE + "/algorithm/backtest/detail?backtestId=" + encodeURIComponent(params.backtestId),
    });
    return { ok: ok(data) || data === undefined || data === null, id: params.backtestId, resolvedId: internalId, raw: data };
  }

  async function backtestDelete(params) {
    const compile = !!params.compile;
    const data = await req(PATHS.backtestDel, {
      method: "POST",
      params: { type: compile ? "1" : "0" },
      form: { backtestId: params.backtestId, algorithmId: "" },
      referrer: BASE + (compile ? "/algorithm/backtest/buildList" : "/algorithm/backtest/list"),
    });
    return { ok: ok(data), id: params.backtestId, raw: data };
  }

  // ------------------------------------------------------------ ops: account

  // accountLimits scrapes the account API and membership page for the
  // concurrency cap (JoinQuant enforces it server-side by VIP tier).
  async function accountLimits() {
    const out = { tried: [] };
    try {
      const acc = await req("/user/account/index", { params: { json: 1 } });
      out.account = acc && acc.__html ? { html: String(acc.__html).slice(0, 800) } : acc;
    } catch (e) {
      out.accountError = e && e.message ? e.message : String(e);
    }
    const urls = ["/view/vip", "/view/vip/index", "/default/index/vip", "/user/vip/index", "/vip/index"];
    const kw = /(并行|同时|并发)[^。！\n<]{0,60}/g;
    const numRe = /最多\s*(\d+)\s*个/g;
    for (const u of urls) {
      out.tried.push(u);
      let html;
      try {
        html = await req(u, { raw: true });
      } catch (_) {
        continue;
      }
      const text = String(html).replace(/<[^>]+>/g, " ").replace(/&nbsp;/g, " ").replace(/\s+/g, " ");
      const limits = [];
      let m;
      while ((m = kw.exec(text)) && limits.length < 10) limits.push(m[0].trim());
      const nums = [];
      let n;
      while ((n = numRe.exec(text)) && nums.length < 10) nums.push(parseInt(n[1], 10));
      if (limits.length || nums.length) {
        out.vipPageUrl = u;
        out.limits = limits;
        out.maxConcurrentCandidates = nums;
        break;
      }
    }
    return out;
  }

  // --------------------------------------------------------------- dispatch

  const OPS = {
    ping: async () => ({ pong: true, href: location.href }),
    "auth.status": authStatus,
    "account.limits": accountLimits,
    "strategy.list": strategyList,
    "strategy.find": strategyFind,
    "strategy.get": strategyGet,
    "strategy.create": strategyCreate,
    "strategy.save": strategySave,
    "strategy.delete": strategyDelete,
    "strategy.move": strategyMove,
    "folder.list": folderList,
    "folder.create": folderCreate,
    "backtest.run": backtestRun,
    "backtest.list": backtestList,
    "backtest.get": backtestGet,
    "backtest.stats": backtestStats,
    "backtest.result": backtestResult,
    "backtest.logs": backtestLogs,
    "backtest.trades": backtestTrades,
    "backtest.positions": backtestPositions,
    "backtest.monthly": backtestMonthly,
    "backtest.cancel": backtestCancel,
    "backtest.delete": backtestDelete,
  };

  async function run(op, params) {
    try {
      const fn = OPS[op];
      if (!fn) return { ok: false, error: { code: "usage_error", message: "未知操作: " + op } };
      const data = await fn(params || {});
      return { ok: true, data };
    } catch (e) {
      if (e && e.code) {
        return { ok: false, error: { code: e.code, message: e.message, details: e.details } };
      }
      return { ok: false, error: { code: "internal_error", message: String((e && e.message) || e) } };
    }
  }

  if (!window.__jqhelper || window.__jqhelper.version !== VERSION) {
    window.__jqhelper = { version: VERSION, run };
  }
})();
