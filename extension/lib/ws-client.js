// BridgeClient maintains a resilient JSON-RPC style WebSocket connection from
// the extension service worker to the local jq-mcp server.
import { VERSION, toErrorPayload } from "./protocol.js";

export class BridgeClient {
  constructor({ url, token, onRequest, onStatus, onEvent }) {
    this.url = url;
    this.token = token;
    this.onRequest = onRequest;
    this.onStatus = onStatus || (() => {});
    this.onEvent = onEvent || (() => {});
    this.ws = null;
    this.connected = false;
    this.closedByUser = false;
    this.retry = 0;
    this.timer = null;
  }

  start() {
    this.closedByUser = false;
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) {
      return;
    }
    this._connect();
  }

  stop() {
    this.closedByUser = true;
    clearTimeout(this.timer);
    if (this.ws) {
      try {
        this.ws.close();
      } catch (_) {
        /* ignore */
      }
    }
  }

  isConnected() {
    return this.connected;
  }

  _connect() {
    if (this.closedByUser) return;
    let ws;
    try {
      ws = new WebSocket(this.url);
    } catch (_) {
      this._retry();
      return;
    }
    this.ws = ws;

    ws.onopen = () => {
      this.connected = true;
      this.retry = 0;
      this.onStatus({ connected: true });
      this._sendHello();
    };
    ws.onmessage = (ev) => this._onMessage(ev.data);
    ws.onclose = () => {
      if (this.ws === ws) {
        this.connected = false;
        this.onStatus({ connected: false });
      }
      this._retry();
    };
    ws.onerror = () => {
      /* onclose follows */
    };
  }

  _sendHello() {
    this._send({
      jsonrpc: "2.0",
      method: "hello",
      params: {
        client: "jqhelper",
        version: VERSION,
        token: this.token,
        browser: navigator.userAgent.includes("Edg/") ? "edge" : "chrome",
      },
    });
  }

  _retry() {
    if (this.closedByUser) return;
    clearTimeout(this.timer);
    const delay = Math.min(30000, 1000 * Math.pow(2, this.retry));
    this.retry += 1;
    this.timer = setTimeout(() => this._connect(), delay);
  }

  _send(obj) {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(obj));
      return true;
    }
    return false;
  }

  notify(method, params) {
    this._send({ jsonrpc: "2.0", method, params });
  }

  async _onMessage(raw) {
    let msg;
    try {
      msg = JSON.parse(raw);
    } catch (_) {
      return;
    }
    if (msg.method === "ping") {
      this._send({ jsonrpc: "2.0", method: "pong" });
      return;
    }
    if (msg.method === "event") {
      this.onEvent(msg.params || {});
      return;
    }
    if (!msg.id) return;
    try {
      const result = await this.onRequest(msg.method, msg.params || {});
      this._send({ jsonrpc: "2.0", id: msg.id, result: result === undefined ? {} : result });
    } catch (err) {
      this._send({ jsonrpc: "2.0", id: msg.id, error: toErrorPayload(err) });
    }
  }
}
