// @ts-nocheck
// jq-loop: turn jq-mcp batch completions into the next model turn.
//
// The Go scheduler owns execution; this plugin only consumes its HTTP event
// feed (`GET /api/events`), and wakes the originating OpenCode session with a
// compact digest via `ctx.session.synthetic` (delivery=queue, resume=true).
//
// Scope: ONE state per OpenCode server process. OpenCode calls `setup()` once
// per workspace, but all workspaces live in the same process and share the same
// sessions/storage. Keeping per-workspace state would give us N pollers, N
// cursors on the same key, and duplicate wake-ups. `globalThis` is the only
// scope that is shared across plugin instances but isolated across servers.
//
// Enable it from opencode.json (see README.md). Disabled by default;
// `/jq-loop on|off|status` toggles it at runtime (server-wide).
import { buildDigest, isCreditEvent, isTerminalEvent, isFailedEvent, shouldWake } from "./core.mjs";

const DEFAULTS = {
  enabled: false,
  url: "",
  token: "",
  // stream (default): wake as soon as results are available, coalescing a short
  // burst; keeps free scheduler slots refilled instead of waiting for the
  // slowest job in a batch. batch: wait for batch.done. failed: first failure.
  // job: stream with no coalescing window.
  wakeOn: "stream",
  coalesceMs: 500,
  maxIterations: 0, // 0 = unlimited; reset when the user sends a new prompt
  cooldownMs: 0,
  maxJobs: 30,
  pollWaitSeconds: 20,
  instructions:
    "先处理失败项（读 errorLogs、修代码、jq_strategy_compile 通过后再回测）；" +
    "再看成功项的指标，决定下一轮要调什么。继续时用 jq_batch_submit 提交下一批。",
  dryRun: false,
};

// Process-wide singleton. Reusing the same object across module reloads keeps
// exactly one poller and one cursor per OpenCode server.
const SHARED_KEY = "__jqLoopShared";
const SHARED = (globalThis[SHARED_KEY] ??= {
  enabled: false,
  opt: null,
  storage: null,
  poller: null, // { ctx, ctl }
  instances: new Set(),
  cursor: null, // { daemonId, seq }
  resetToLatest: false,
  iterations: new Map(),
  lastWake: new Map(),
  handled: new Map(), // `${daemonId}:${batchId}` -> { woke }
  pending: new Map(), // sessionID -> { batches: Map<batchId, Map<key, job>>, timer }
  startedAt: Date.now(),
});

function opt() {
  return SHARED.opt || DEFAULTS;
}

function apiBase() {
  const o = opt();
  return String(o.url || process.env.JQ_MCP_HTTP_URL || "http://127.0.0.1:8792").replace(/\/+$/, "");
}

function authHeaders(extra = {}) {
  const headers = { "Content-Type": "application/json", ...extra };
  const token = String(opt().token || process.env.JQ_MCP_TOKEN || "jqhelper-dev-token");
  if (token) headers.Authorization = `Bearer ${token}`;
  return headers;
}

async function api(path, init = {}, timeoutMs = 10_000) {
  const res = await fetch(apiBase() + path, {
    ...init,
    headers: authHeaders(init.headers || {}),
    signal: init.signal || AbortSignal.timeout(timeoutMs),
  });
  const body = await res.json().catch(() => null);
  if (!res.ok) {
    const e = body?.error || {};
    throw new Error(`jq-mcp HTTP ${res.status}: ${e.message || path}`);
  }
  return body;
}

// ---------------------------------------------------------------- persistence

async function loadState() {
  const storage = SHARED.storage;
  if (!storage) return;
  const saved = await storage.get("cursor").catch(() => null);
  if (typeof saved === "number") {
    SHARED.cursor = { daemonId: null, seq: saved }; // legacy format
  } else if (saved && typeof saved === "object") {
    SHARED.cursor = { daemonId: saved.daemonId ?? null, seq: Number(saved.seq) || 0 };
  }
  const savedIters = await storage.get("iterations").catch(() => null);
  if (savedIters && typeof savedIters === "object") {
    for (const [k, v] of Object.entries(savedIters)) {
      const n = Number(v);
      if (Number.isFinite(n)) SHARED.iterations.set(k, n);
    }
  }
}

let lastSaved = 0;
async function saveCursor(force = false) {
  const storage = SHARED.storage;
  if (!storage) return;
  const now = Date.now();
  if (!force && now - lastSaved < 1500) return;
  lastSaved = now;
  await storage.set("cursor", SHARED.cursor ?? { daemonId: null, seq: 0 }).catch(() => {});
}

async function saveIterations() {
  const storage = SHARED.storage;
  if (!storage) return;
  await storage
    .set("iterations", Object.fromEntries(SHARED.iterations))
    .catch(() => {});
}

// ---------------------------------------------------------------------- wake

async function wake(sessionID, render, reason) {
  const o = opt();
  const iteration = SHARED.iterations.get(sessionID) ?? 0;
  const guard = shouldWake({
    enabled: SHARED.enabled,
    sessionId: sessionID,
    iteration,
    maxIterations: Number(o.maxIterations) || 0,
    lastWakeAt: SHARED.lastWake.get(sessionID) || 0,
    cooldownMs: Number(o.cooldownMs) || 0,
  });
  if (!guard.ok) {
    if (guard.reason !== "disabled") {
      console.warn(`[jq-loop] skip ${guard.reason} session=${sessionID}`);
    }
    return false;
  }
  const ctx = SHARED.poller?.ctx || [...SHARED.instances][0];
  if (!ctx) {
    console.warn("[jq-loop] no live plugin context to wake with");
    return false;
  }
  const text = render(iteration + 1);
  if (o.dryRun) {
    console.log(`[jq-loop] dry-run (${reason}) session=${sessionID}\n${text}`);
  } else {
    await ctx.session.synthetic({
      sessionID,
      text,
      description: `jq-loop ${reason}`,
      delivery: "queue",
      resume: true,
    });
  }
  SHARED.iterations.set(sessionID, iteration + 1);
  if (SHARED.iterations.size > 200) {
    const first = SHARED.iterations.keys().next().value;
    SHARED.iterations.delete(first);
  }
  await saveIterations();
  SHARED.lastWake.set(sessionID, Date.now());
  return true;
}

// ------------------------------------------------------------------- events

function batchState(event, batchId) {
  const key = `${event.daemonId || ""}:${batchId}`;
  let b = SHARED.handled.get(key);
  if (!b) {
    b = { woke: false, creditWoke: false };
    SHARED.handled.set(key, b);
    if (SHARED.handled.size > 500) {
      SHARED.handled.delete(SHARED.handled.keys().next().value);
    }
  }
  return b;
}

function enqueue(ev) {
  if (!ev.job) return;
  const sid = ev.sessionId;
  let entry = SHARED.pending.get(sid);
  if (!entry) {
    entry = { batches: new Map(), timer: null };
    SHARED.pending.set(sid, entry);
  }
  let jobs = entry.batches.get(ev.batchId);
  if (!jobs) {
    jobs = new Map();
    entry.batches.set(ev.batchId, jobs);
  }
  jobs.set(ev.job.taskId || ev.job.id || String(ev.eventId), ev.job);
  scheduleFlush(sid, entry);
}

function scheduleFlush(sid, entry) {
  if (entry.timer) clearTimeout(entry.timer);
  const windowMs = opt().wakeOn === "job" ? 0 : Math.max(0, Number(opt().coalesceMs) || 0);
  entry.timer = setTimeout(() => {
    void flush(sid).catch((err) => console.warn(`[jq-loop] flush 失败: ${String(err?.message || err)}`));
  }, windowMs);
}

// flush merges every batch that finished inside the coalescing window for one
// session into a single wake-up. Multiple batches -> one interruption.
async function flush(sid, reason = "stream") {
  const entry = SHARED.pending.get(sid);
  if (!entry) return;
  SHARED.pending.delete(sid);
  if (entry.timer) clearTimeout(entry.timer);
  const o = opt();
  const batches = [...entry.batches].filter(([, jobs]) => jobs.size > 0);
  if (!batches.length) return;
  await wake(
    sid,
    (iteration) =>
      batches
        .map(([batchId, jobs]) =>
          buildDigest({
            batchId,
            jobs: [...jobs.values()],
            iteration,
            maxIterations: Number(o.maxIterations) || 0,
            maxJobs: Number(o.maxJobs) || 30,
            instructions: o.instructions,
            phase: reason,
          }),
        )
        .join("\n\n"),
    reason,
  );
}

async function handleEvent(ev) {
  const sessionID = ev.sessionId;
  if (!sessionID) return; // batch submitted outside OpenCode (scripts/CLI)

  // Paid-credit approval is a blocking decision: wake even when wakeOn=batch,
  // otherwise the batch would never reach batch.done.
  if (isCreditEvent(ev) && ev.job) {
    const b = batchState(ev, ev.batchId);
    if (!b.creditWoke) {
      b.creditWoke =
        (await wake(
          sessionID,
          (iteration) =>
            buildDigest({
              batchId: ev.batchId,
              jobs: [ev.job],
              iteration,
              maxIterations: Number(opt().maxIterations) || 0,
              instructions: opt().instructions,
              phase: "credit",
            }),
          "credit",
        )) || b.creditWoke;
    }
    return;
  }

  if (opt().wakeOn === "failed" && isFailedEvent(ev) && ev.job) {
    const b = batchState(ev, ev.batchId);
    if (!b.woke) {
      const text = (iteration) =>
        buildDigest({
          batchId: ev.batchId,
          jobs: [ev.job],
          iteration,
          maxIterations: Number(opt().maxIterations) || 0,
          instructions: opt().instructions,
          phase: "failed",
        });
      b.woke = (await wake(sessionID, text, "failed")) || b.woke;
    }
    return;
  }

  if (opt().wakeOn === "stream" || opt().wakeOn === "job") {
    if (isTerminalEvent(ev) && ev.job) {
      enqueue(ev);
      return;
    }
    if (ev.type === "batch.done" && SHARED.pending.has(sessionID)) {
      await flush(sessionID, "stream");
    }
    return;
  }

  if (ev.type === "batch.done") {
    const b = batchState(ev, ev.batchId);
    if (b.woke) return;
    const jobs = ev.jobs || [];
    b.woke = await wake(
      sessionID,
      (iteration) =>
        buildDigest({
          batchId: ev.batchId,
          jobs,
          iteration,
          maxIterations: Number(opt().maxIterations) || 0,
          instructions: opt().instructions,
          phase: "batch",
        }),
      "batch",
    );
  }
}

// -------------------------------------------------------------------- poller

function ensureLoop(ctx) {
  if (SHARED.poller) return;
  if (!ctx) return;
  const ctl = new AbortController();
  SHARED.poller = { ctx, ctl };
  void runLoop(ctx, ctl.signal);
}

function stopLoop() {
  if (!SHARED.poller) return;
  SHARED.poller.ctl.abort();
  SHARED.poller = null;
}

async function runLoop(ctx, signal) {
  const o = opt();
  const pollWait = Math.max(1, Math.min(50, Number(o.pollWaitSeconds) || 20));
  let backoff = 1000;
  while (!signal.aborted) {
    try {
      const h = await api("/healthz", {}, 3000);
      if (!SHARED.cursor || SHARED.resetToLatest) {
        SHARED.cursor = { daemonId: h?.daemonId ?? null, seq: Number(h?.lastEventId) || 0 };
        SHARED.resetToLatest = false;
      } else if (h && SHARED.cursor.daemonId && h.daemonId && h.daemonId !== SHARED.cursor.daemonId) {
        // Daemon restarted: its event sequence restarted too.
        console.warn(`[jq-loop] daemon 重启 (${SHARED.cursor.daemonId} -> ${h.daemonId})，游标重置`);
        SHARED.cursor = { daemonId: h.daemonId, seq: Number(h.lastEventId) || 0 };
      } else if (h && Number(h.lastEventId) < SHARED.cursor.seq) {
        // Safety net for daemons that predate daemonId (or a same-id restart).
        console.warn(`[jq-loop] 事件序号回退 (${SHARED.cursor.seq} -> ${h.lastEventId})，游标重置`);
        SHARED.cursor = { daemonId: h.daemonId ?? SHARED.cursor.daemonId, seq: Number(h.lastEventId) || 0 };
      }
      const since = SHARED.cursor.seq;
      const res = await api(
        `/api/events?since=${since}&wait=${pollWait}&limit=200`,
        { signal },
        (pollWait + 15) * 1000,
      );
      const events = Array.isArray(res.events) ? res.events : [];
      let handledAll = true;
      for (const ev of events) {
        try {
          await handleEvent(ev);
        } catch (err) {
          // Do not advance past an event we failed to handle: the cursor stays
          // put so the next poll retries it.
          console.warn(`[jq-loop] 事件处理失败，稍后重试: ${String(err?.message || err)}`);
          handledAll = false;
          break;
        }
      }
      if (handledAll) {
        const next = Number(res.nextEventId);
        if (Number.isFinite(next) && next >= SHARED.cursor.seq) {
          SHARED.cursor.seq = next;
        }
        await saveCursor(events.length > 0);
      } else {
        await new Promise((r) => setTimeout(r, backoff));
        backoff = Math.min(backoff * 2, 15_000);
        continue;
      }
      backoff = 1000;
    } catch (err) {
      if (signal.aborted) break;
      console.warn(`[jq-loop] event poll failed: ${String(err?.message || err)}`);
      await new Promise((r) => setTimeout(r, backoff));
      backoff = Math.min(backoff * 2, 15_000);
    }
  }
}

// --------------------------------------------------------------------- setup

export default {
  id: "jq-loop",
  async setup(ctx) {
    SHARED.opt = { ...DEFAULTS, ...(ctx.options || {}) };
    SHARED.storage = ctx.storage;
    SHARED.instances.add(ctx);
    await loadState();
    if (SHARED.opt.enabled === true) SHARED.enabled = true;

    // Reset the iteration budget when a human (or the agent) sends a prompt.
    await ctx.session.hook("prompt", async (event) => {
      if (event?.sessionID) {
        SHARED.iterations.delete(event.sessionID);
        await saveIterations();
      }
    });

    // Runtime toggle: /jq-loop on|off|status (server-wide).
    await ctx.command.transform((editor) => {
      editor.add({
        name: "jq-loop",
        description: "Toggle jq-loop auto-iteration (on|off|status)",
        execute: async ({ sessionID, prompt }) => {
          const arg = String(prompt?.text ?? "").trim().toLowerCase();
          if (arg === "on") {
            SHARED.enabled = true;
            if (!SHARED.poller) {
              // start from the newest event, not from accumulated history
              SHARED.cursor = null;
              SHARED.resetToLatest = true;
            }
            ensureLoop(ctx);
          } else if (arg === "off") {
            SHARED.enabled = false;
            stopLoop();
          }
          const iter = SHARED.iterations.get(sessionID) ?? 0;
          const where = SHARED.poller
            ? `poller=pid:${process.pid} cursor=${SHARED.cursor?.seq ?? 0}`
            : "poller=未运行";
          const max = SHARED.opt.maxIterations || "∞";
          await ctx.session.synthetic({
            sessionID,
            text: `[jq-loop] ${SHARED.enabled ? "已开启" : "已关闭"}（server 级，迭代 ${iter}/${max}，wakeOn=${SHARED.opt.wakeOn}，${where}，workspaces=${SHARED.instances.size}）`,
            delivery: "queue",
            resume: false,
          });
        },
      });
    });

    if (SHARED.opt.enabled === true) ensureLoop(ctx);

    return () => {
      SHARED.instances.delete(ctx);
      if (SHARED.poller && SHARED.poller.ctx === ctx) {
        stopLoop();
        // The workspace that owned the poller went away; hand it to a survivor.
        const next = [...SHARED.instances][0];
        if (next && SHARED.enabled) ensureLoop(next);
      }
      for (const entry of SHARED.pending.values()) {
        if (entry.timer) clearTimeout(entry.timer);
      }
      SHARED.pending.clear();
    };
  },
};
