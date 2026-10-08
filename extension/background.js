// jqhelper background service worker: owns the WebSocket connection to jq-mcp
// and routes incoming bridge requests to the appropriate handler.
import { BridgeClient } from "./lib/ws-client.js";
import { CODES, jqError } from "./lib/protocol.js";
import { handle } from "./handlers/index.js";

const DEFAULT_URL = "ws://127.0.0.1:8790/ws";
const KEEPALIVE_ALARM = "jqhelper-keepalive";

let client = null;
let config = { wsUrl: DEFAULT_URL, token: "" };
let lastStatus = { connected: false };

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

async function startClient() {
  if (client) client.stop();
  await loadConfig();
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
}

// Reconnect whenever the SW wakes up or settings change.
startClient();

chrome.runtime.onInstalled.addListener(() => startClient());
chrome.runtime.onStartup.addListener(() => startClient());

chrome.storage.onChanged.addListener((changes, area) => {
  if (area === "local" && (changes.wsUrl || changes.token)) {
    startClient();
  }
});

chrome.alarms.create(KEEPALIVE_ALARM, { periodInMinutes: 0.5 });
chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name !== KEEPALIVE_ALARM) return;
  if (!client) {
    startClient();
  } else if (!client.isConnected()) {
    client.start();
  }
});

// Messaging channel for the options page.
chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (msg && msg.type === "status") {
    sendResponse({ connected: !!client && client.isConnected(), config });
    return true;
  }
  if (msg && msg.type === "reconnect") {
    startClient()
      .then(() => sendResponse({ ok: true }))
      .catch((e) => sendResponse({ ok: false, error: String(e) }));
    return true;
  }
  return false;
});
