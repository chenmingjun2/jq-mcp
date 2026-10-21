// @ts-nocheck
// jq-loop: turn jq-mcp batch completions into the next model turn.
//
// The Go scheduler owns execution; this plugin only consumes its HTTP event
// feed (`GET /api/events`), and wakes the originating OpenCode session with a
// compact digest via `ctx.session.synthetic` (delivery=queue, resume=true).
//
// Enable it from opencode.json (see README.md). It is disabled by default, and
// a per-session `/jq-loop on|off|status` command toggles it at runtime.
import { buildDigest, isTerminalEvent, isFailedEvent, shouldWake } from "./core.mjs";

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

export default {
  id: "jq-loop",
  async setup(ctx) {
    const opt = { ...DEFAULTS, ...(ctx.options || {}) };
    const base = String(opt.url || process.env.JQ_MCP_HTTP_URL || "http://127.0.0.1:8792").replace(/\/+$/, "");
    const token = String(opt.token || process.env.JQ_MCP_TOKEN || "jqhelper-dev-token");
    const headers = { "Content-Type": "application/json" };
    if (token) headers.Authorization = `Bearer ${token}`;
    const pollWait = Math.max(1, Math.min(50, Number(opt.pollWaitSeconds) || 20));

    const state = {
      enabled: opt.enabled === true,
      cursor: null,
      iterations: new Map(), // sessionID -> count
      lastWake: new Map(), // sessionID -> epoch ms
      handled: new Map(), // batchId -> {woke:boolean}
    };
    const saved = await ctx.storage.get("cursor");
    if (typeof saved === "number") state.cursor = saved;
    const savedIters = await ctx.storage.get("iterations");
    if (savedIters && typeof savedIters === "object") {
      for (const [k, v] of Object.entries(savedIters)) {
        const n = Number(v);
        if (Number.isFinite(n)) state.iterations.set(k, n);
      }
    }
    let lastSaved = 0;

    async function api(path, init = {}, timeoutMs = 10_000) {
      const res = await fetch(base + path, {
        ...init,
        headers: { ...headers, ...(init.headers || {}) },
        signal: init.signal || AbortSignal.timeout(timeoutMs),
      });
      const body = await res.json().catch(() => null);
      if (!res.ok) {
        const e = body?.error || {};
        throw new Error(`jq-mcp HTTP ${res.status}: ${e.message || path}`);
      }
      return body;
    }

    async function health() {
      return api("/healthz", {}, 3000);
    }

    async function saveCursor(force = false) {
      const now = Date.now();
      if (!force && now - lastSaved < 1500) return;
      lastSaved = now;
      await ctx.storage.set("cursor", state.cursor ?? 0).catch(() => {});
    }

    async function saveIterations() {
      await ctx.storage.set("iterations", Object.fromEntries(state.iterations)).catch(() => {});
    }

    function batchState(batchId) {
      let b = state.handled.get(batchId);
      if (!b) {
        b = { woke: false };
        state.handled.set(batchId, b);
        // Keep the map bounded; mature batches fall off.
        if (state.handled.size > 500) {
          const first = state.handled.keys().next().value;
          state.handled.delete(first);
        }
      }
      return b;
    }

    async function wake(sessionID, jobs, batchId, reason) {
      const iteration = state.iterations.get(sessionID) ?? 0;
      const guard = shouldWake({
        enabled: state.enabled,
        sessionId: sessionID,
        iteration,
        maxIterations: Number(opt.maxIterations) || 0,
        lastWakeAt: state.lastWake.get(sessionID) || 0,
        cooldownMs: Number(opt.cooldownMs) || 0,
      });
      if (!guard.ok) {
        if (guard.reason !== "disabled") {
          console.warn(`[jq-loop] skip ${guard.reason} session=${sessionID} batch=${batchId}`);
        }
        return false;
      }
      const text = buildDigest({
        batchId,
        jobs,
        iteration: iteration + 1,
        maxIterations: Number(opt.maxIterations) || 0,
        maxJobs: Number(opt.maxJobs) || 30,
        instructions: opt.instructions,
        phase: reason,
      });
      if (opt.dryRun) {
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
      state.iterations.set(sessionID, iteration + 1);
      if (state.iterations.size > 200) {
        const first = state.iterations.keys().next().value;
        state.iterations.delete(first);
      }
      await saveIterations();
      state.lastWake.set(sessionID, Date.now());
      return true;
    }

    async function handleEvent(ev) {
      const sessionID = ev.sessionId;
      if (!sessionID) return; // batch submitted outside OpenCode (scripts/CLI)
      const batchId = ev.batchId;

      if (opt.wakeOn === "failed" && isFailedEvent(ev) && ev.job) {
        const b = batchState(batchId);
        if (!b.woke) {
          b.woke = (await wake(sessionID, [ev.job], batchId, "failed")) || b.woke;
        }
        return;
      }

      // stream/job: deliver every completed job (coalesced for a short window)
      // instead of waiting for the whole batch. This keeps the agent refilling
      // freed scheduler slots while longer jobs are still running.
      if (opt.wakeOn === "stream" || opt.wakeOn === "job") {
        if (isTerminalEvent(ev) && ev.job) {
          enqueueJob(ev);
          return;
        }
        if (ev.type === "batch.done" && pendingResults.has(batchId)) {
          await flushBatch(batchId);
        }
        return;
      }

      if (ev.type === "batch.done") {
        const b = batchState(batchId);
        if (b.woke) return;
        b.woke = await wake(sessionID, ev.jobs || [], batchId, "batch");
      }
    }

    // --- streaming wake-ups -------------------------------------------------
    // A batch may contain jobs of very different durations. Waiting for
    // batch.done idles every slot that finishes early, so the default mode
    // reports finished jobs in small groups and lets the session submit the
    // next jobs right away.
    const pendingResults = new Map(); // batchId -> {sessionID, jobs:Map, timer}
    const coalesceMs = Math.max(0, Number(opt.coalesceMs) || 0);
    const windowMs = opt.wakeOn === "job" ? 0 : coalesceMs;

    function enqueueJob(ev) {
      if (!ev.job) return;
      const key = ev.job.taskId || ev.job.id || String(ev.eventId);
      let entry = pendingResults.get(ev.batchId);
      if (!entry) {
        entry = { sessionID: ev.sessionId, jobs: new Map(), timer: null };
        pendingResults.set(ev.batchId, entry);
      }
      entry.jobs.set(key, ev.job);
      if (entry.timer) clearTimeout(entry.timer);
      entry.timer = setTimeout(() => {
        void flushBatch(ev.batchId);
      }, windowMs);
    }

    async function flushBatch(batchId) {
      const entry = pendingResults.get(batchId);
      if (!entry) return;
      pendingResults.delete(batchId);
      if (entry.timer) clearTimeout(entry.timer);
      await wake(entry.sessionID, [...entry.jobs.values()], batchId, opt.wakeOn === "job" ? "job" : "stream");
    }

    // Reset the iteration budget when a human (or the agent) sends a prompt.
    await ctx.session.hook("prompt", async (event) => {
      if (event?.sessionID) {
        state.iterations.delete(event.sessionID);
        await saveIterations();
      }
    });

    // Runtime toggle: /jq-loop on|off|status
    await ctx.command.transform((editor) => {
      editor.add({
        name: "jq-loop",
        description: "Toggle jq-loop auto-iteration (on|off|status)",
        execute: async ({ sessionID, prompt }) => {
          const arg = String(prompt?.text ?? "").trim().toLowerCase();
          if (arg === "on") {
            state.enabled = true;
            state.cursor = null; // skip events accumulated while disabled
            startLoop();
          } else if (arg === "off") {
            state.enabled = false;
          }
          const iter = state.iterations.get(sessionID) ?? 0;
          await ctx.session.synthetic({
            sessionID,
            text: `[jq-loop] ${state.enabled ? "已开启" : "已关闭"}（迭代 ${iter}/${opt.maxIterations || "∞"}，wakeOn=${opt.wakeOn}）`,
            delivery: "queue",
            resume: false,
          });
        },
      });
    });

    let controller = null;
    let loopStarted = false;
    function startLoop() {
      if (loopStarted) return;
      loopStarted = true;
      controller = new AbortController();
      const signal = controller.signal;
      void (async () => {
        let backoff = 1000;
        while (!signal.aborted) {
          try {
            if (state.cursor === null) {
              const h = await health();
              state.cursor = Number(h.lastEventId) || 0;
            }
            const res = await api(
              `/api/events?since=${state.cursor}&wait=${pollWait}&limit=200`,
              { signal },
              (pollWait + 15) * 1000,
            );
            const next = Number(res.nextEventId);
            const events = Array.isArray(res.events) ? res.events : [];
            for (const ev of events) {
              try {
                await handleEvent(ev);
              } catch (err) {
                console.warn(`[jq-loop] event ${ev?.type}/${ev?.batchId} failed: ${String(err?.message || err)}`);
              }
            }
            if (Number.isFinite(next) && next >= state.cursor) state.cursor = next;
            if (events.length === 0) {
              // A jq-mcp restart resets the event sequence; re-sync instead of
              // waiting forever on a cursor from the previous process.
              try {
                const h = await health();
                const last = Number(h.lastEventId) || 0;
                if (last < state.cursor) state.cursor = last;
              } catch {
                /* keep polling */
              }
            }
            await saveCursor(events.length > 0);
            backoff = 1000;
          } catch (err) {
            if (signal.aborted) break;
            console.warn(`[jq-loop] event poll failed: ${String(err?.message || err)}`);
            await new Promise((r) => setTimeout(r, backoff));
            backoff = Math.min(backoff * 2, 15_000);
          }
        }
      })();
    }

    // Only poll while enabled; `/jq-loop on` starts the loop lazily.
    if (state.enabled) startLoop();
    return () => {
      controller?.abort();
      for (const entry of pendingResults.values()) {
        if (entry.timer) clearTimeout(entry.timer);
      }
      pendingResults.clear();
    };
  },
};
