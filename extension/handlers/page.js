// Tab + page-injection helpers shared by all handlers.
import { CODES, jqError } from "../lib/protocol.js";

const JQ_MATCHES = ["https://www.joinquant.com/*", "http://www.joinquant.com/*"];
const PAGE_SCRIPT = "page/jq-page.js";

// chrome.scripting.executeScript serializes on a single tab, so a small pool of
// joinquant tabs lets read-only requests proceed in parallel. Mutating ops are
// already serialized (serializeMutation), so the pool stays small to bound memory.
const TAB_POOL = 2;        // 目标页签数
const TAB_MAX = 2;         // 硬上限：超过即回收（防并发/异常导致页签泄漏）
let tabPool = [];
let tabCursor = 0;
let ensurePromise = null;  // 并发去重：避免多个请求同时创建页签

function liveJqTabs(tabs) {
  return tabs.filter((t) => t.id != null && !t.discarded).map((t) => t.id);
}

// gcTabs 保留前 keep 个聚宽页签，关闭其余（回收内存）。供后台定时调用。
export async function gcTabs(keep = TAB_MAX) {
  try {
    const tabs = await chrome.tabs.query({ url: JQ_MATCHES });
    const live = liveJqTabs(tabs);
    const keepIds = live.slice(0, keep);
    const drop = live.slice(keep);
    for (const id of drop) {
      try {
        await chrome.tabs.remove(id);
      } catch (_) {
        /* ignore */
      }
    }
    tabPool = tabPool.filter((id) => keepIds.includes(id));
    return { kept: keepIds.length, closed: drop.length };
  } catch (_) {
    return { kept: 0, closed: 0 };
  }
}

// Tabs whose MAIN world already has the page bridge injected. Re-injecting the
// ~750-line file on every op costs an extra executeScript round trip; we only
// inject once per tab and invalidate on navigation/close.
const injectedTabs = new Set();
chrome.tabs.onUpdated.addListener((id, info) => {
  if (info.status === "loading") injectedTabs.delete(id);
});
chrome.tabs.onRemoved.addListener((id) => injectedTabs.delete(id));

// ensureTab returns an existing joinquant tab or opens a background one.
export async function ensureTab() {
  const tabs = await chrome.tabs.query({ url: JQ_MATCHES });
  const existing = tabs.find((t) => t.id != null && !t.discarded);
  if (existing) return existing;
  const created = await chrome.tabs.create({ url: "https://www.joinquant.com/", active: false });
  await waitForLoad(created.id);
  return created;
}

// ensurePool keeps up to TAB_POOL live joinquant tabs, recycles any extras, and
// de-duplicates concurrent creation (the old version could leak tabs when several
// bridge requests called it at once).
async function ensurePool() {
  if (ensurePromise) return ensurePromise;
  ensurePromise = (async () => {
    const tabs = await chrome.tabs.query({ url: JQ_MATCHES });
    const live = liveJqTabs(tabs);
    tabPool = tabPool.filter((id) => live.includes(id));
    for (const id of live) {
      if (tabPool.length >= TAB_POOL) break;
      if (!tabPool.includes(id)) tabPool.push(id);
    }
    // 回收多余页签（并发/崩溃遗留）
    for (const id of live) {
      if (tabPool.includes(id)) continue;
      try {
        await chrome.tabs.remove(id);
      } catch (_) {
        /* ignore */
      }
    }
    while (tabPool.length < TAB_POOL) {
      const created = await chrome.tabs.create({ url: "https://www.joinquant.com/", active: false });
      await waitForLoad(created.id);
      if (created.id != null) tabPool.push(created.id);
      else break;
    }
  })();
  try {
    return await ensurePromise;
  } finally {
    ensurePromise = null;
  }
}

// pickTab round-robins across the pool so concurrent requests use distinct tabs.
async function pickTab() {
  await ensurePool();
  if (!tabPool.length) {
    const t = await ensureTab();
    return t.id;
  }
  const id = tabPool[tabCursor++ % tabPool.length];
  return id;
}

// waitForLoad resolves when the tab finishes loading or after a timeout.
export function waitForLoad(tabId) {
  return new Promise((resolve) => {
    const finish = () => {
      clearTimeout(timer);
      chrome.tabs.onUpdated.removeListener(listener);
      resolve();
    };
    const timer = setTimeout(finish, 20000);
    function listener(id, info) {
      if (id === tabId && info.status === "complete") finish();
    }
    chrome.tabs.onUpdated.addListener(listener);
  });
}

// Mutating account-level operations (create/save/delete/move, folder.create)
// share server-side draft state: strategy.create's id fallback picks the newest
// strategy from the list, so two concurrent creates can cross wires and save the
// wrong code. Serialize them through a single FIFO chain so the bridge stays
// correct even when drivers issue concurrent requests. Read-only ops are
// unaffected and still run in parallel across the tab pool.
let mutationChain = Promise.resolve();
export function serializeMutation(fn) {
  const run = mutationChain.catch(() => {}).then(fn);
  mutationChain = run.catch(() => {});
  return run;
}

// pageRun injects the page bridge into a joinquant tab and invokes one op.
export async function pageRun(op, params) {
  const clean = JSON.parse(JSON.stringify(params ?? {}));
  const tabId = await pickTab();

  const inject = () =>
    chrome.scripting.executeScript({
      target: { tabId },
      files: [PAGE_SCRIPT],
      world: "MAIN",
    });
  const runOp = () =>
    chrome.scripting.executeScript({
      target: { tabId },
      world: "MAIN",
      func: (operation, payload) => window.__jqhelper && window.__jqhelper.run(operation, payload),
      args: [op, clean],
    });

  // Ensure the bridge is present (once per tab) and run the op.
  const attempt = async () => {
    if (!injectedTabs.has(tabId)) {
      await inject();
      injectedTabs.add(tabId);
    }
    return runOp();
  };

  let results;
  try {
    results = await attempt();
  } catch (_) {
    // The tab may have navigated or been discarded; reload once and retry.
    injectedTabs.delete(tabId);
    await chrome.tabs.reload(tabId).catch(() => {});
    await waitForLoad(tabId);
    results = await attempt();
  }

  let entry = results && results[0];
  if (!entry || entry.result === undefined || entry.result === null) {
    // Bridge missing (e.g. the tab navigated without us seeing a loading event):
    // force a re-inject and try once more.
    injectedTabs.delete(tabId);
    await inject();
    injectedTabs.add(tabId);
    results = await runOp();
    entry = results && results[0];
  }
  if (!entry || entry.result === undefined || entry.result === null) {
    throw jqError(CODES.internal, "页面脚本无返回，页面可能尚未加载完成");
  }
  const value = entry.result;
  if (!value.ok) {
    throw jqError(value.error.code, value.error.message, value.error.details);
  }
  return value.data;
}

// joinquantCookies returns every cookie the extension can read on joinquant.com,
// including HttpOnly cookies (chrome.cookies is not subject to page scripts).
export async function joinquantCookies() {
  const all = await chrome.cookies.getAll({});
  return all.filter((c) => /(^|\.)joinquant\.com$/.test(c.domain || ""));
}

// cookieHeader builds a Cookie request header value from cookie objects.
export function cookieHeader(cookies) {
  return cookies.map((c) => `${c.name}=${c.value}`).join("; ");
}
