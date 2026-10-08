const DEFAULT_URL = "ws://127.0.0.1:8790/ws";

const $ = (id) => document.getElementById(id);

function setStatus(text, kind) {
  const el = $("status");
  el.textContent = text;
  el.className = kind || "";
}

async function load() {
  const cfg = await chrome.storage.local.get({ wsUrl: DEFAULT_URL, token: "" });
  $("wsUrl").value = cfg.wsUrl;
  $("token").value = cfg.token;
}

async function save() {
  const wsUrl = $("wsUrl").value.trim() || DEFAULT_URL;
  const token = $("token").value.trim();
  await chrome.storage.local.set({ wsUrl, token });
  setStatus("已保存，正在重新连接…");
  try {
    await chrome.runtime.sendMessage({ type: "reconnect" });
    setStatus("已保存并触发重连。", "ok");
  } catch (e) {
    setStatus("保存成功，但重连消息发送失败：" + e, "err");
  }
}

async function test() {
  setStatus("检测中…");
  try {
    const res = await chrome.runtime.sendMessage({ type: "status" });
    if (res && res.connected) {
      setStatus("已连接到 jq-mcp。", "ok");
    } else {
      setStatus("尚未连接。请确认 jq-mcp 已启动、地址与 token 正确。", "err");
    }
  } catch (e) {
    setStatus("检测失败：" + e, "err");
  }
}

$("save").addEventListener("click", save);
$("test").addEventListener("click", test);
load();
