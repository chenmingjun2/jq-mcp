// jq-mcp-client.mjs — high-throughput, event-driven client for jq-mcp.
//
// Design goals (compile/backtest sweeps):
//   * one long-lived `jq-mcp` child process, reused for the whole sweep;
//   * requests are pipelined over MCP stdio (the server dispatches tools
//     concurrently), responses are matched by id and may arrive out of order;
//   * jobs go through the server-side scheduler (`jq_batch_submit`), so
//     concurrency, back-pressure and retries stay on the Go side;
//   * results stream in through `jq_batch_events` long-polling with a cursor,
//     so every terminal job is delivered exactly once, as it happens — no
//     client-side sleep/poll loop and no per-job stats requests;
//   * `job.done` events carry the metrics snapshot, so a 100-job sweep needs
//     ~2 tool calls in total (submit + event stream).
//
// Requirements: Node.js >= 18 (fetch, AbortSignal.timeout), jq-mcp >= 0.3.0.
//
// Two modes:
//   * spawn (default) — starts its own jq-mcp; it owns the bridge port.
//   * attach          — talks to a running jq-mcp's HTTP API (`url` option or
//                       JQ_MCP_HTTP_URL), so it coexists with OpenCode's MCP
//                       server and other clients.
//
// Note: the jqhelper extension only connects to one bridge URL (default
// ws://127.0.0.1:8790/ws), so in spawn mode this client owns that port while it
// runs. Stop any other jq-mcp instance bound to the same port first, or use
// attach mode against that instance's --http-addr.

import { spawn } from "node:child_process";
import { createInterface } from "node:readline";
import { setTimeout as delay } from "node:timers/promises";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

export const DEFAULT_BRIDGE_PORT = 8790;
export const DEFAULT_TOKEN = "jqhelper-dev-token";

/** Base class for client failures. */
export class JqMcpError extends Error {
  constructor(message, { code, details, cause } = {}) {
    super(message);
    this.name = new.target.name;
    this.code = code;
    this.details = details;
    if (cause) this.cause = cause;
  }
}

/** Transport/process/bridge failures (no tool payload involved). */
export class TransportError extends JqMcpError {}

/** A tool returned `isError: true` (usage_error/api_error/network_error...). */
export class ToolError extends JqMcpError {
  constructor(message, { code, details, tool } = {}) {
    super(message, { code, details });
    this.tool = tool;
  }
}

/**
 * Start a client and (optionally) wait until the browser extension is attached.
 *
 * @param {object} [opts]
 * @param {string} [opts.bin]                 path to the jq-mcp binary
 * @param {number} [opts.port=8790]           bridge port (must match the extension)
 * @param {string} [opts.url]                 attach to a running jq-mcp HTTP API (e.g. http://127.0.0.1:8792)
 * @param {string} [opts.token]               bridge/HTTP token
 * @param {string} [opts.sessionId]           optional session id recorded with batches (attach mode)
 * @param {string} [opts.home]                JQ_MCP_HOME (default: temp dir)
 * @param {number} [opts.waitExtensionMs]     wait for the extension (0 = don't)
 * @param {boolean} [opts.requireLogin]       also wait for loggedIn (auth.status)
 * @param {number} [opts.requestTimeoutMs]    default per-request timeout
 * @param {boolean} [opts.verbose]            pipe jq-mcp stderr
 * @param {object} [opts.env]                 extra env for the child
 * @param {boolean} [opts.keepHome]           keep the temp JQ_MCP_HOME on close
 * @returns {Promise<JqMcpClient>}
 */
export async function startClient(opts = {}) {
  const client = new JqMcpClient(opts);
  await client.start();
  return client;
}

export class JqMcpClient {
  constructor(opts = {}) {
    this.bin = opts.bin || process.env.JQ_MCP_BIN || defaultBinary();
    this.port = Number(opts.port || process.env.JQ_MCP_PORT || DEFAULT_BRIDGE_PORT);
    this.token = opts.token || process.env.JQ_MCP_TOKEN || DEFAULT_TOKEN;
    this.url = opts.url ? String(opts.url).replace(/\/+$/, "") : process.env.JQ_MCP_HTTP_URL?.replace(/\/+$/, "") || "";
    this.attach = !!this.url;
    this.sessionId = opts.sessionId || process.env.JQ_MCP_SESSION_ID || "";
    this.home = opts.home || opts.env?.JQ_MCP_HOME || tempHome();
    this.waitExtensionMs = opts.waitExtensionMs ?? 120_000;
    this.requireLogin = !!opts.requireLogin;
    this.requestTimeoutMs = opts.requestTimeoutMs ?? 600_000;
    this.workers = Number(opts.workers || process.env.JQ_MCP_WORKERS || 0);
    this.verbose = !!opts.verbose;
    this.childEnv = opts.env || {};
    this.keepHome = !!opts.keepHome;

    this.child = null;
    this.stderrTail = "";
    this.notificationHandlers = new Set();
    this.pending = new Map();
    this.nextId = 1;
    this.closed = false;
    this.startedAt = null;
    /** Client-side call counters (useful for benchmarks/observability). */
    this.stats = { toolCalls: 0, byTool: {} };
  }

  // ------------------------------------------------------------------ setup

  async start() {
    if (this.child) return this;
    if (this.attach) {
      await this.#waitHTTP(30_000);
      this.startedAt = Date.now();
      if (this.waitExtensionMs > 0) {
        await this.waitForExtension({ timeoutMs: this.waitExtensionMs, requireLogin: this.requireLogin });
      }
      return this;
    }
    if (await isBridgeUp(this.port)) {
      throw new TransportError(
        `端口 ${this.port} 上已经有 jq-mcp 在运行；扩展只能连一个 bridge，` +
          `请先停掉它（或换一个端口并同步修改扩展配置）`,
      );
    }
    fs.mkdirSync(this.home, { recursive: true });
    const args = [
      "--ws-addr", `127.0.0.1:${this.port}`,
      "--token", this.token,
      "--browser-mode", "attach",
      "--config", path.join(this.home, "config.json"),
    ];
    if (this.workers > 0) args.push("--workers", String(this.workers));
    const child = spawn(this.bin, args, {
      cwd: process.cwd(),
      env: { ...process.env, ...this.childEnv, JQ_MCP_HOME: this.home },
      stdio: ["pipe", "pipe", "pipe"],
    });
    this.child = child;
    child.stdin.on("error", () => {});
    child.stderr.on("data", (d) => {
      const s = d.toString();
      this.stderrTail = (this.stderrTail + s).slice(-16_384);
      if (this.verbose) process.stderr.write(s);
    });
    child.on("exit", (code, signal) => {
      this.closed = true;
      const err = new TransportError(
        `jq-mcp 退出 (code=${code}, signal=${signal})${this.stderrTail ? `: ${this.stderrTail.trim().split("\n").slice(-3).join(" | ")}` : ""}`,
      );
      for (const p of this.pending.values()) {
        clearTimeout(p.timer);
        p.reject(err);
      }
      this.pending.clear();
    });
    createInterface({ input: child.stdout, crlfDelay: Infinity }).on("line", (line) => this.#onLine(line));

    await this.#waitBridge(30_000);
    await this.call("initialize", {
      protocolVersion: "2024-11-05",
      capabilities: {},
      clientInfo: { name: "jq-mcp-mjs-client", version: "0.1.0" },
    });
    this.startedAt = Date.now();
    if (this.waitExtensionMs > 0) {
      await this.waitForExtension({ timeoutMs: this.waitExtensionMs, requireLogin: this.requireLogin });
    }
    return this;
  }

  /** Wait until the extension is attached (and optionally logged in). */
  async waitForExtension({ timeoutMs = 120_000, requireLogin = false } = {}) {
    const deadline = Date.now() + timeoutMs;
    let lastErr = null;
    while (Date.now() < deadline) {
      try {
        const health = this.attach
          ? await this.#req("GET", "/healthz", undefined, 3000)
          : await (async () => {
              const res = await fetch(`http://127.0.0.1:${this.port}/healthz`, {
                signal: AbortSignal.timeout(2000),
              });
              return res.ok ? res.json() : null;
            })();
        if (health?.connected) {
          if (!requireLogin) return health;
          const h = await this.tool("jq_health", {});
          if (h.loggedIn) return h;
        }
      } catch (e) {
        lastErr = e;
      }
      await delay(300);
    }
    throw new TransportError(
      `扩展在 ${timeoutMs}ms 内未连接${lastErr ? `: ${lastErr.message}` : ""}`,
    );
  }

  async #waitBridge(timeoutMs) {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      if (this.closed) throw new TransportError(`jq-mcp 启动失败: ${this.stderrTail.trim()}`);
      if (await isBridgeUp(this.port)) return;
      await delay(50);
    }
    throw new TransportError(`jq-mcp 未在 ${timeoutMs}ms 内监听 127.0.0.1:${this.port}`);
  }

  // ------------------------------------------------------------ HTTP attach

  async #waitHTTP(timeoutMs) {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      try {
        await this.#req("GET", "/healthz", undefined, 2000);
        return;
      } catch {
        await delay(100);
      }
    }
    throw new TransportError(`无法连接 jq-mcp HTTP API: ${this.url}`);
  }

  async #req(method, path, body, timeoutMs = this.requestTimeoutMs) {
    const headers = { "Content-Type": "application/json" };
    if (this.token) headers.Authorization = `Bearer ${this.token}`;
    const init = {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    };
    if (timeoutMs > 0) init.signal = AbortSignal.timeout(timeoutMs);
    const res = await fetch(this.url + path, init);
    let payload = null;
    try {
      payload = await res.json();
    } catch {
      /* empty body */
    }
    if (!res.ok) {
      const e = payload?.error || {};
      throw new ToolError(e.message || `HTTP ${res.status} ${path}`, {
        code: e.code,
        details: e.details,
        tool: path,
      });
    }
    return payload;
  }

  // ------------------------------------------------------------- raw MCP

  /**
   * Call any MCP method and return its raw result.
   * Timeout of 0 disables the timer.
   */
  call(method, params = {}, { timeoutMs = this.requestTimeoutMs } = {}) {
    if (this.attach) return Promise.reject(new TransportError("attach 模式没有 MCP stdio 通道"));
    if (this.closed) return Promise.reject(new TransportError("client 已关闭"));
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      const timer =
        timeoutMs > 0
          ? setTimeout(() => {
              this.pending.delete(id);
              reject(new TransportError(`等待 ${method} 响应超时 (${timeoutMs}ms)`));
            }, timeoutMs)
          : null;
      this.pending.set(id, { resolve, reject, timer, method });
      const line = JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n";
      this.child.stdin.write(line);
    });
  }

  /** Call a tool, decode its JSON text payload and surface tool errors. */
  async tool(name, args = {}, opts = {}) {
    this.stats.toolCalls++;
    this.stats.byTool[name] = (this.stats.byTool[name] || 0) + 1;
    if (this.attach) {
      const res = await this.#req(
        "POST",
        `/api/tools/${encodeURIComponent(name)}`,
        { arguments: args },
        opts.timeoutMs ?? this.requestTimeoutMs,
      );
      return res.result;
    }
    const res = await this.call("tools/call", { name, arguments: args }, opts);
    const text = res?.content?.[0]?.text ?? "";
    let payload;
    try {
      payload = JSON.parse(text);
    } catch {
      payload = text;
    }
    if (res?.isError) {
      const e = payload?.error || {};
      throw new ToolError(e.message || String(text || "tool error"), {
        code: e.code,
        details: e.details,
        tool: name,
      });
    }
    return payload;
  }

  /** Subscribe to server notifications (notifications/message & friends). */
  onNotification(fn) {
    this.notificationHandlers.add(fn);
    return () => this.notificationHandlers.delete(fn);
  }

  #onLine(line) {
    if (!line || !line.trim()) return;
    let msg;
    try {
      msg = JSON.parse(line);
    } catch {
      return;
    }
    if (msg.id !== undefined && msg.id !== null) {
      const p = this.pending.get(msg.id);
      if (!p) return;
      this.pending.delete(msg.id);
      if (p.timer) clearTimeout(p.timer);
      if (msg.error) {
        p.reject(new TransportError(msg.error.message || "rpc error", { code: msg.error.code, details: msg.error.data }));
      } else {
        p.resolve(msg.result);
      }
      return;
    }
    for (const fn of this.notificationHandlers) {
      try {
        fn(msg);
      } catch {
        /* listener errors must not break the reader */
      }
    }
  }

  // ------------------------------------------------------------- scheduler

  /**
   * Enqueue jobs on the server-side scheduler and return immediately.
   * `jq_batch_submit` is non-blocking; use `stream()`/`runBatch()` for results.
   */
  async submit(jobs, { maxConcurrent, callbackUrl, callbackSecret, callbackOn } = {}) {
    if (!Array.isArray(jobs) || jobs.length === 0) throw new JqMcpError("jobs 不能为空");
    for (const j of jobs) {
      if (!j?.strategyId || !j?.start) throw new JqMcpError("每个 job 需要 strategyId 与 start");
    }
    const args = { jobs };
    if (maxConcurrent) args.maxConcurrent = maxConcurrent;
    if (callbackUrl) args.callbackUrl = callbackUrl;
    if (callbackSecret) args.callbackSecret = callbackSecret;
    if (callbackOn) args.callbackOn = callbackOn;
    if (this.attach) {
      if (this.sessionId) args.sessionId = this.sessionId;
      this.stats.toolCalls++;
      this.stats.byTool.jq_batch_submit = (this.stats.byTool.jq_batch_submit || 0) + 1;
      return this.#req("POST", "/api/batches", args, 120_000);
    }
    return this.tool("jq_batch_submit", args, { timeoutMs: 120_000 });
  }

  /**
   * Stream every event of one batch, oldest first, until it is terminal.
   *
   * Uses `jq_batch_events` long-polling with a cursor, so it is lossless and
   * wakes up as soon as a job changes state.
   *
   * @param {string} batchId
   * @param {object} [opts]
   * @param {number} [opts.sinceEventId=0]
   * @param {number} [opts.waitSeconds=20]
   * @param {number} [opts.limit=500]
   * @param {AbortSignal} [opts.signal]
   * @param {boolean} [opts.includeSnapshot] yield a final `{type:"snapshot"}` pseudo event
   */
  async *stream(batchId, { sinceEventId = 0, waitSeconds = 20, limit = 500, signal, includeSnapshot = false } = {}) {
    let cursor = sinceEventId;
    let snapshot = null;
    for (;;) {
      if (signal?.aborted) throw signal.reason || new TransportError("aborted");
      let res;
      if (this.attach) {
        this.stats.toolCalls++;
        this.stats.byTool.jq_batch_events = (this.stats.byTool.jq_batch_events || 0) + 1;
        res = await this.#req(
          "GET",
          `/api/batches/${encodeURIComponent(batchId)}/events?since=${cursor}&wait=${waitSeconds}&limit=${limit}`,
          undefined,
          (waitSeconds + 60) * 1000,
        );
      } else {
        res = await this.tool(
          "jq_batch_events",
          { batchId, sinceEventId: cursor, waitSeconds, limit },
          { timeoutMs: (waitSeconds + 60) * 1000 },
        );
      }
      cursor = res.nextEventId ?? cursor;
      for (const ev of res.events || []) yield ev;
      // Keep draining when the batch is terminal but more events than `limit`
      // are still queued behind the cursor.
      if (res.done && !res.hasMore) {
        snapshot = res;
        break;
      }
    }
    if (includeSnapshot) yield { type: "snapshot", snapshot };
  }

  /**
   * Submit a batch and stream terminal jobs through `onJob` as they finish.
   * Returns results in submission order. Jobs without a `tag` get `job-<n>`.
   *
   * @param {object[]} jobs
   * @param {object} [opts]
   * @param {(result:object)=>void} [opts.onJob]
   * @param {(event:object)=>void} [opts.onEvent]
   * @param {boolean} [opts.compile] force `compile: true` on every job
   * @param {number} [opts.sinceEventId]
   * @param {AbortSignal} [opts.signal]
   * @param {object} [opts.submit] options forwarded to `submit()`
   */
  async runBatch(jobs, opts = {}) {
    const tagged = normalizeJobs(jobs, opts.compile);
    const batch = await this.submit(tagged, opts.submit);
    const results = new Map(); // tag -> result
    let snapshotJobs = null;

    const absorb = (job, extra = {}) => {
      if (!job) return;
      const result = toResult(job, batch.batchId, extra);
      results.set(result.tag, result);
      if (opts.onJob) opts.onJob(result);
    };

    for await (const ev of this.stream(batch.batchId, { sinceEventId: opts.sinceEventId, signal: opts.signal })) {
      if (opts.onEvent) opts.onEvent(ev);
      if (ev.type === "snapshot") {
        snapshotJobs = ev.snapshot.jobs || null;
        continue;
      }
      if (!ev.job) continue;
      if (ev.type === "job.done" || ev.type === "job.failed" || ev.type === "job.timeout") {
        absorb(ev.job, { eventId: ev.id, at: ev.at });
      }
    }

    // Anything we did not see (e.g. aborted early) still comes from the batch
    // snapshot, which is ordered like the submission.
    if (!snapshotJobs) {
      try {
        const st = await this.tool("jq_batch_status", { batchId: batch.batchId });
        snapshotJobs = st.jobs || null;
      } catch {
        /* best effort */
      }
    }
    if (snapshotJobs) {
      for (const job of snapshotJobs) {
        const tag = job.tag;
        if (!tag || results.has(tag)) continue;
        if (job.status === "done" || job.status === "failed" || job.status === "timeout") {
          absorb(job, { final: true });
        }
      }
    }
    return tagged.map((j) => results.get(j.tag)).filter(Boolean);
  }

  /** `runBatch` with `compile: true` on every job. */
  compileAll(jobs, opts = {}) {
    return this.runBatch(jobs, { ...opts, compile: true });
  }

  /** `runBatch` for regular backtests. */
  runAll(jobs, opts = {}) {
    return this.runBatch(jobs, { ...opts, compile: false });
  }

  // ---------------------------------------------------------------- teardown

  async close() {
    if (!this.child) return;
    this.closed = true;
    for (const p of this.pending.values()) {
      if (p.timer) clearTimeout(p.timer);
      p.reject(new TransportError("client 已关闭"));
    }
    this.pending.clear();
    const child = this.child;
    this.child = null;
    try {
      child.stdin.end();
    } catch {
      /* ignore */
    }
    const exited = new Promise((resolve) => child.once("exit", resolve));
    const timeout = delay(3000).then(() => "timeout");
    if ((await Promise.race([exited, timeout])) === "timeout") {
      child.kill("SIGKILL");
      await exited;
    }
    if (!this.keepHome) fs.rmSync(this.home, { recursive: true, force: true });
  }
}

// ------------------------------------------------------------------ helpers

function normalizeJobs(jobs, forceCompile) {
  if (!Array.isArray(jobs) || jobs.length === 0) throw new JqMcpError("jobs 不能为空");
  const seen = new Set();
  return jobs.map((raw, i) => {
    const job = { ...raw };
    if (!job.strategyId || !job.start) throw new JqMcpError(`jobs[${i}] 需要 strategyId 与 start`);
    if (!job.tag) job.tag = `job-${i + 1}`;
    if (seen.has(job.tag)) throw new JqMcpError(`重复的 tag: ${job.tag}`);
    seen.add(job.tag);
    if (forceCompile) job.compile = true;
    return job;
  });
}

function toResult(job, batchId, extra = {}) {
  const started = job.startedAt ? Date.parse(job.startedAt) : null;
  const ended = job.endedAt ? Date.parse(job.endedAt) : null;
  // job.metrics is the raw backtest.stats payload ({id,resolvedId,metrics});
  // expose the unwrapped metrics as well as the raw stats.
  const stats = job.metrics ?? null;
  const metrics =
    stats && typeof stats === "object" && stats.metrics && typeof stats.metrics === "object"
      ? stats.metrics
      : stats;
  return {
    tag: job.tag,
    strategyId: job.strategyId,
    mode: job.compile ? "compile" : "backtest",
    status: job.status,
    metrics,
    stats,
    error: job.error || null,
    errorLogs: job.errorLogs || null,
    id: job.id || null,
    listId: job.listId || null,
    taskId: job.taskId || null,
    start: job.start,
    end: job.end || null,
    capital: job.capital ?? null,
    frequency: job.frequency || null,
    batchId,
    startedAt: job.startedAt || null,
    endedAt: job.endedAt || null,
    elapsedMs: started && ended ? ended - started : null,
    ...extra,
  };
}

export function defaultBinary() {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const candidates = [
    path.resolve("bin", "jq-mcp"),
    path.resolve("..", "bin", "jq-mcp"),
    path.resolve(here, "..", "..", "bin", "jq-mcp"),
  ];
  for (const c of candidates) {
    try {
      fs.accessSync(c, fs.constants.X_OK);
      return c;
    } catch {
      /* keep looking */
    }
  }
  return "jq-mcp";
}

function tempHome() {
  return fs.mkdtempSync(path.join(os.tmpdir(), "jq-mcp-client-"));
}

async function isBridgeUp(port) {
  try {
    const res = await fetch(`http://127.0.0.1:${port}/healthz`, { signal: AbortSignal.timeout(500) });
    return res.ok;
  } catch {
    return false;
  }
}
