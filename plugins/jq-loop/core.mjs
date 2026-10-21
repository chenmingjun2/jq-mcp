// Pure helpers for the jq-loop plugin. Kept dependency-free so they can be
// tested with `node --test plugins/jq-loop/core.test.mjs`.

export const TERMINAL_EVENT_TYPES = new Set(["job.done", "job.failed", "job.timeout"]);

export function isTerminalEvent(ev) {
  return !!ev && TERMINAL_EVENT_TYPES.has(ev.type);
}

export function isFailedEvent(ev) {
  return !!ev && (ev.type === "job.failed" || ev.type === "job.timeout");
}

/** The scheduler stores the raw backtest.stats payload; unwrap metrics. */
export function normalizeMetrics(raw) {
  if (!raw || typeof raw !== "object") return null;
  return raw.metrics && typeof raw.metrics === "object" ? raw.metrics : raw;
}

export function jobResult(job) {
  const started = job?.startedAt ? Date.parse(job.startedAt) : NaN;
  const ended = job?.endedAt ? Date.parse(job.endedAt) : NaN;
  return {
    tag: job?.tag || job?.taskId || job?.id || "job",
    status: job?.status || "unknown",
    mode: job?.compile ? "compile" : "backtest",
    metrics: normalizeMetrics(job?.metrics),
    error: job?.error || null,
    errorLogs: Array.isArray(job?.errorLogs) ? job.errorLogs : null,
    id: job?.id || null,
    elapsedMs: Number.isFinite(started) && Number.isFinite(ended) ? ended - started : null,
  };
}

function fmt(v) {
  if (typeof v === "number" && Number.isFinite(v)) return v.toFixed(4);
  if (v === undefined || v === null || v === "") return "-";
  return String(v);
}

function truncate(s, n) {
  const str = String(s ?? "");
  return str.length > n ? str.slice(0, n) + "…" : str;
}

/**
 * Build the message injected back into the session when a batch finishes.
 * Kept small: failures first, then a compact metrics table.
 */
export function buildDigest({
  batchId,
  jobs,
  iteration = 1,
  maxIterations = 5,
  maxJobs = 30,
  maxErrorLines = 4,
  instructions = "",
}) {
  const results = (jobs || []).map(jobResult);
  const counts = { done: 0, failed: 0, timeout: 0 };
  for (const r of results) counts[r.status] = (counts[r.status] || 0) + 1;

  const failed = results.filter((r) => r.status !== "done");
  const done = results.filter((r) => r.status === "done");
  const lines = [];

  lines.push(
    maxIterations > 0
      ? `[jq-loop] 批次 ${batchId} 已结束（第 ${iteration}/${maxIterations} 轮）`
      : `[jq-loop] 批次 ${batchId} 已结束（第 ${iteration} 轮）`,
  );
  lines.push(
    `任务 ${results.length} 个：done=${counts.done || 0} failed=${counts.failed || 0} timeout=${counts.timeout || 0}`,
  );

  if (failed.length) {
    lines.push("", `失败（${failed.length}）：`);
    for (const r of failed.slice(0, 10)) {
      const why = r.error ? ` — ${truncate(r.error, 240)}` : "";
      lines.push(`- ${r.tag} [${r.mode}] ${r.status}${why}`);
      for (const log of (r.errorLogs || []).slice(-maxErrorLines)) {
        lines.push(`  ${truncate(log, 240)}`);
      }
    }
    if (failed.length > 10) lines.push(`… 其余 ${failed.length - 10} 个失败省略`);
  }

  const compiles = done.filter((r) => r.mode === "compile");
  const backtests = done.filter((r) => r.mode === "backtest");
  if (compiles.length) lines.push("", `编译通过：${compiles.length} 个`);
  if (backtests.length) {
    lines.push("", `回测结果（${backtests.length}）：`);
    for (const r of backtests.slice(0, maxJobs)) {
      const m = r.metrics || {};
      lines.push(
        `- ${r.tag}: sharpe=${fmt(m.sharpe)} ann=${fmt(m.annual_algo_return)} ` +
          `ret=${fmt(m.algorithm_return)} dd=${fmt(m.max_drawdown)}` +
          (r.id ? ` (id=${r.id})` : ""),
      );
    }
    if (backtests.length > maxJobs) {
      lines.push(`… 其余 ${backtests.length - maxJobs} 个回测省略`);
    }
  }

  if (instructions) lines.push("", "下一步：", instructions);

  const text = lines.join("\n");
  return text.length > 12_000 ? text.slice(0, 12_000) + "\n…（已截断）" : text;
}

/**
 * Decide whether an automatic wake-up is allowed.
 * @returns {{ok: boolean, reason?: string}}
 */
export function shouldWake({
  enabled,
  sessionId,
  iteration,
  maxIterations,
  lastWakeAt = 0,
  cooldownMs = 0,
  now = Date.now(),
}) {
  if (!enabled) return { ok: false, reason: "disabled" };
  if (!sessionId) return { ok: false, reason: "no_session" };
  if (maxIterations > 0 && iteration >= maxIterations) return { ok: false, reason: "max_iterations" };
  if (lastWakeAt && cooldownMs > 0 && now - lastWakeAt < cooldownMs) {
    return { ok: false, reason: "cooldown" };
  }
  return { ok: true };
}

/** Reset the iteration budget when the user speaks in the session. */
export function onUserPrompt(state, sessionId) {
  if (sessionId) state.iterations.delete(sessionId);
}
