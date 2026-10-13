// jqhelper background service worker: ensures the persistent WebSocket to
// jq-mcp is owned by an offscreen document (immune to service-worker sleep) and
// executes the bridge requests that the offscreen document relays to it.
import { BridgeClient } from "./lib/ws-client.js";
import { CODES, jqError, toErrorPayload } from "./lib/protocol.js";
import { handle } from "./handlers/index.js";

const DEFAULT_URL = "ws://127.0.0.1:8790/ws";
const KEEPALIVE_ALARM = "jqhelper-keepalive";
const TAB_GC_ALARM = "jqhelper-tab-gc";
const JQ_TAB_MAX = 2;
const OFFSCREEN_PATH = "offscreen.html";

// Fallback in-worker client, used only when the offscreen API is unavailable.
let client = null;
let config = { wsUrl: DEFAULT_URL, token: "" };
let lastStatus = { connected: false };
let starting = false;
let offscreenBroken = false;

async function loadConfig() {
  const stored = await chrome.storage.local.get({ wsUrl: "", token: "" });
  config = { wsUrl: stored.wsUrl || DEFAULT_URL, token: stored.token || "" };
  // Managed-launch bootstrap: jq-mcp writes default-config.json next to the
  // extension so a freshly launched browser can connect without manual setup.
  if (!config.token) {
    try {
      const res = await fetch(chrome.runtime.getURL("default-config.json"));
      if (res.ok) {
        const def = await res.json();
        config.wsUrl = stored.wsUrl || def.wsUrl || DEFAULT_URL;
        config.token = stored.token || def.token || "";
      }
    } catch (_) {
      /* no bootstrap file; wait for manual configuration */
    }
  }
}

function rememberStatus(status) {
  lastStatus = { ...lastStatus, ...status };
  if (chrome.storage.session) {
    chrome.storage.session.set({ jqhelperStatus: lastStatus }).catch(() => {});
  }
}

// offscreenSupported reports whether we can host the socket off-worker.
function offscreenSupported() {
  return !offscreenBroken && typeof chrome.offscreen !== "undefined";
}

// ensureOffscreen creates the offscreen document if it is not already open.
async function ensureOffscreen() {
  if (typeof chrome.offscreen === "undefined") return false;
  const offscreenUrl = chrome.runtime.getURL(OFFSCREEN_PATH);
  let exists = false;
  try {
    if (chrome.runtime.getContexts) {
      const contexts = await chrome.runtime.getContexts({
        contextTypes: ["OFFSCREEN_DOCUMENT"],
      });
      exists = contexts.some((c) => c.documentUrl === offscreenUrl);
    }
  } catch (_) {
    /* getContexts requires Chrome 116; fall through to createDocument */
  }
  if (exists) return true;
  try {
    await chrome.offscreen.createDocument({
      url: OFFSCREEN_PATH,
      reasons: [chrome.offscreen.Reason.WEB_RTC],
      justification: "维持与本地 jq-mcp 的持久 WebSocket 连接",
    });
  } catch (err) {
    // A racing createDocument reports "single offscreen document"; treat as ok.
    if (!/single offscreen/i.test(String(err))) throw err;
  }
  return true;
}

async function startClient() {
  // Guard against concurrent wakeups (SW start + storage change + alarm).
  if (starting) return;
  starting = true;
  try {
    // Load config first so get-config (from the offscreen document) has the token.
    await loadConfig();
    if (offscreenSupported()) {
      try {
        await ensureOffscreen();
        return; // the offscreen document owns the socket now
      } catch (err) {
        // e.g. the "offscreen" permission is missing in an older install:
        // disable and fall back to the in-worker client.
        offscreenBroken = true;
      }
    }
    if (client) client.stop();
    client = new BridgeClient({
      url: config.wsUrl,
      token: config.token,
      onRequest: async (method, params) => {
        if (!config.token) throw jqError(CODES.noExtension, "插件尚未配置 token，请在选项页填写");
        return handle(method, params);
      },
      onStatus: (s) => rememberStatus(s),
      onEvent: () => {},
    });
    client.start();
  } finally {
    starting = false;
  }
}

// getStatus reports the live connection state regardless of who owns the socket.
async function getStatus() {
  if (offscreenSupported()) {
    try {
      const res = await chrome.runtime.sendMessage({ type: "ws-status" });
      if (res) return { connected: !!res.connected, config };
    } catch (_) {
      /* offscreen not ready yet */
    }
    return { connected: false, config };
  }
  return { connected: !!client && client.isConnected(), config };
}

// Reconnect whenever the SW wakes up or settings change.
startClient();

chrome.runtime.onInstalled.addListener(() => startClient());
chrome.runtime.onStartup.addListener(() => startClient());

chrome.storage.onChanged.addListener((changes, area) => {
  if (area === "local" && (changes.wsUrl || changes.token)) {
    startClient();
    // The offscreen document cannot read chrome.storage; tell it to reload.
    chrome.runtime.sendMessage({ type: "config-changed" }).catch(() => {});
  }
});

// The alarm revives the worker and guarantees the offscreen document exists
// (recreating it if Chrome closed it). The socket itself reconnects with
// exponential backoff inside the offscreen document.
chrome.alarms.create(KEEPALIVE_ALARM, { periodInMinutes: 0.5 });
// 定时回收聚宽页签，避免并发/异常导致的页签泄漏把机器内存吃满。
chrome.alarms.create(TAB_GC_ALARM, { periodInMinutes: 1 });
chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === TAB_GC_ALARM) {
    gcJoinquantTabs();
    return;
  }
  if (alarm.name !== KEEPALIVE_ALARM) return;
  if (offscreenSupported()) {
    startClient();
  } else if (!client) {
    startClient();
  } else if (!client.isConnected()) {
    client.start();
  }
});

// gcJoinquantTabs 仅保留至多 JQ_TAB_MAX 个聚宽页签，其余关闭。
async function gcJoinquantTabs() {
  try {
    const tabs = await chrome.tabs.query({
      url: ["https://www.joinquant.com/*", "http://www.joinquant.com/*"],
    });
    const live = tabs.filter((t) => t.id != null && !t.discarded).map((t) => t.id);
    for (const id of live.slice(JQ_TAB_MAX)) {
      try {
        await chrome.tabs.remove(id);
      } catch (_) {
        /* ignore */
      }
    }
  } catch (_) {
    /* ignore */
  }
}

// Messaging channel: options page queries + offscreen-document relay.
chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (!msg) return false;
  if (msg.type === "status") {
    getStatus().then(sendResponse);
    return true;
  }
  if (msg.type === "reconnect") {
    startClient()
      .then(() => sendResponse({ ok: true }))
      .catch((e) => sendResponse({ ok: false, error: String(e) }));
    return true;
  }
  if (msg.type === "get-config") {
    (async () => {
      if (!config.token) {
        try {
          await loadConfig();
        } catch (_) {
          /* ignore */
        }
      }
      sendResponse({ config });
    })();
    return true;
  }
  if (msg.type === "jq-request") {
    (async () => {
      try {
        const value = await handle(msg.method, msg.params || {});
        sendResponse({ value: value === undefined ? {} : value });
      } catch (err) {
        sendResponse({ error: toErrorPayload(err) });
      }
    })();
    return true;
  }
  if (msg.type === "ws-status-change") {
    rememberStatus(msg.status || {});
    return false;
  }
  return false;
});
