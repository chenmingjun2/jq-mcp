// Tab + page-injection helpers shared by all handlers.
import { CODES, jqError } from "../lib/protocol.js";

const JQ_MATCHES = ["https://www.joinquant.com/*", "http://www.joinquant.com/*"];
const PAGE_SCRIPT = "page/jq-page.js";

// chrome.scripting.executeScript serializes on a single tab, so running several
// bridge requests through one joinquant tab makes them effectively sequential.
// A small pool of joinquant tabs lets requests proceed in parallel.
const TAB_POOL = 3;
let tabPool = [];
let tabCursor = 0;

// ensureTab returns an existing joinquant tab or opens a background one.
export async function ensureTab() {
  const tabs = await chrome.tabs.query({ url: JQ_MATCHES });
  const existing = tabs.find((t) => t.id != null && !t.discarded);
  if (existing) return existing;
  const created = await chrome.tabs.create({ url: "https://www.joinquant.com/", active: false });
  await waitForLoad(created.id);
  return created;
}

// ensurePool keeps up to TAB_POOL live joinquant tabs and returns their ids.
async function ensurePool() {
  const tabs = await chrome.tabs.query({ url: JQ_MATCHES });
  const live = tabs.filter((t) => t.id != null && !t.discarded).map((t) => t.id);
  tabPool = tabPool.filter((id) => live.includes(id));
  for (const id of live) {
    if (tabPool.length >= TAB_POOL) break;
    if (!tabPool.includes(id)) tabPool.push(id);
  }
  while (tabPool.length < TAB_POOL) {
    const created = await chrome.tabs.create({ url: "https://www.joinquant.com/", active: false });
    await waitForLoad(created.id);
    if (created.id != null) tabPool.push(created.id);
    else break;
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

  try {
    await inject();
  } catch (_) {
    // The tab may have navigated or been discarded; reload once and retry.
    await chrome.tabs.reload(tabId).catch(() => {});
    await waitForLoad(tabId);
    await inject();
  }

  const results = await chrome.scripting.executeScript({
    target: { tabId },
    world: "MAIN",
    func: (operation, payload) => window.__jqhelper && window.__jqhelper.run(operation, payload),
    args: [op, clean],
  });

  const entry = results && results[0];
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
