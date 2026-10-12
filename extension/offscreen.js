// Offscreen bridge: owns the persistent WebSocket connection to jq-mcp.
//
// Why here and not in the service worker: Chrome terminates idle MV3 service
// workers after ~30s. An active WebSocket resets that timer, but the worker must
// first be awake to reconnect. An offscreen document is a normal page context
// with no such lifecycle limit, so the socket stays up across worker sleeps.
//
// Note: an offscreen document only exposes a subset of the extension API
// (chrome.runtime messaging). chrome.storage is NOT available here, so the
// bridge URL/token are read from the service worker and from default-config.json.
// Bridge requests are relayed to the service worker, which wakes on demand.
import { BridgeClient } from "./lib/ws-client.js";

const DEFAULT_URL = "ws://127.0.0.1:8790/ws";

let client = null;
let config = { wsUrl: DEFAULT_URL, token: "" };
let starting = false;

async function loadConfig() {
  config = { wsUrl: DEFAULT_URL, token: "" };
  // Ask the service worker (it owns chrome.storage).
  try {
    const res = await chrome.runtime.sendMessage({ type: "get-config" });
    if (res && res.config) {
      config = { wsUrl: res.config.wsUrl || DEFAULT_URL, token: res.config.token || "" };
    }
  } catch (_) {
    /* worker not ready; fall back to the bootstrap file */
  }
  if (!config.token) {
    try {
      const r = await fetch(chrome.runtime.getURL("default-config.json"));
      if (r.ok) {
        const def = await r.json();
        config.wsUrl = config.wsUrl || def.wsUrl || DEFAULT_URL;
        config.token = def.token || "";
      }
    } catch (_) {
      /* wait for manual configuration */
    }
  }
}

// relay hands a request to the service worker and returns its result. A failed
// first send (worker still starting) is retried once.
async function relay(method, params) {
  let res;
  for (let i = 0; i < 2; i++) {
    try {
      res = await chrome.runtime.sendMessage({ type: "jq-request", method, params });
      if (res) break;
    } catch (err) {
      if (i === 1) throw err;
    }
    await new Promise((r) => setTimeout(r, 150));
  }
  if (!res) throw new Error("service worker 未响应");
  if (res.error) {
    const e = new Error(res.error.message);
    e.code = res.error.code;
    e.details = res.error.details;
    throw e;
  }
  return res.value;
}

async function start() {
  // Guard against concurrent wakeups (top-level start + reconnect message).
  if (starting) return;
  starting = true;
  try {
    if (client) client.stop();
    await loadConfig();
    client = new BridgeClient({
      url: config.wsUrl,
      token: config.token,
      onRequest: (method, params) => relay(method, params),
      onStatus: (s) => {
        try {
          chrome.runtime.sendMessage({ type: "ws-status-change", status: { ...s } }).catch(() => {});
        } catch (_) {
          /* ignore */
        }
      },
      onEvent: () => {},
    });
    client.start();
  } finally {
    starting = false;
  }
}

function safe(fn) {
  try {
    fn();
  } catch (_) {
    /* ignore listener registration failures */
  }
}

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (!msg) return false;
  if (msg.type === "ws-status") {
    sendResponse({ connected: !!client && client.isConnected() });
    return true;
  }
  if (msg.type === "reconnect" || msg.type === "config-changed") {
    start()
      .then(() => sendResponse({ ok: true }))
      .catch((e) => sendResponse({ ok: false, error: String(e) }));
    return true;
  }
  return false;
});

safe(start);
