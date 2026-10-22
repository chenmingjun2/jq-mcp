import test from "node:test";
import assert from "node:assert/strict";
import { buildDigest, isCreditEvent, jobResult, normalizeMetrics, shouldWake } from "./core.mjs";

test("normalizeMetrics unwraps the raw stats payload", () => {
  assert.deepEqual(normalizeMetrics({ metrics: { sharpe: 1.2 } }), { sharpe: 1.2 });
  assert.deepEqual(normalizeMetrics({ sharpe: 1.2 }), { sharpe: 1.2 });
  assert.equal(normalizeMetrics(null), null);
});

test("jobResult normalizes compile and backtest jobs", () => {
  const bt = jobResult({
    tag: "v1",
    status: "done",
    metrics: { metrics: { sharpe: 1.1, annual_algo_return: 0.2 } },
    id: "bt-1",
    startedAt: "2026-01-01T00:00:00Z",
    endedAt: "2026-01-01T00:00:03Z",
  });
  assert.equal(bt.mode, "backtest");
  assert.equal(bt.metrics.sharpe, 1.1);
  assert.equal(bt.elapsedMs, 3000);

  const cl = jobResult({ tag: "c1", compile: true, status: "failed", errorLogs: ["SyntaxError"] });
  assert.equal(cl.mode, "compile");
  assert.equal(cl.metrics, null);
  assert.deepEqual(cl.errorLogs, ["SyntaxError"]);
});

test("jobResult keeps the account and credit metadata", () => {
  const js = jobResult({
    tag: "v1",
    status: "done",
    account: "a2",
    metrics: { metrics: { sharpe: 1.2 } },
    id: "bt-1",
  });
  assert.equal(js.account, "a2");
  assert.equal(js.metrics.sharpe, 1.2);

  const paused = jobResult({
    tag: "v2",
    status: "needs_confirmation",
    creditRequired: true,
    creditReason: "免费额度已用完",
    creditAccount: "a1",
  });
  assert.equal(paused.creditRequired, true);
  assert.equal(paused.account, "a1");
  assert.equal(paused.error, "免费额度已用完");
  assert.equal(isCreditEvent({ type: "job.needs_credit" }), true);
  assert.equal(isCreditEvent({ type: "job.done" }), false);
});

test("buildDigest renders a credit-approval request", () => {
  const text = buildDigest({
    batchId: "batch-credit",
    iteration: 1,
    phase: "credit",
    jobs: [
      {
        tag: "v1",
        status: "needs_confirmation",
        creditRequired: true,
        creditAccount: "a1",
        creditReason: "免费回测额度已用完，继续将消耗 50000 积分",
      },
    ],
  });
  assert.match(text, /需要确认积分/);
  assert.match(text, /acct=a1/);
  assert.match(text, /jq_batch_confirm_credit/);
  assert.match(text, /免费回测额度已用完/);
});

test("buildDigest prioritizes failures and truncates", () => {
  const text = buildDigest({
    batchId: "batch-1",
    iteration: 2,
    maxIterations: 5,
    instructions: "fix it",
    jobs: [
      { tag: "a", status: "done", metrics: { metrics: { sharpe: 1.23456, annual_algo_return: 0.2 } } },
      { tag: "b", status: "done", compile: true },
      {
        tag: "c",
        status: "failed",
        error: "NameError: x",
        errorLogs: ["line1", "line2", "x".repeat(500)],
      },
    ],
  });
  assert.match(text, /batch-1/);
  assert.match(text, /第 2\/5 轮/);
  assert.match(text, /done=2 failed=1/);
  assert.match(text, /NameError: x/);
  assert.match(text, /line2/);
  assert.match(text, /x{10}…/);
  assert.match(text, /sharpe=1\.2346/);
  assert.match(text, /下一步：\nfix it/);
});

test("buildDigest handles unlimited iterations", () => {
  const text = buildDigest({ batchId: "b", jobs: [], iteration: 3, maxIterations: 0 });
  assert.match(text, /第 3 轮/);
  assert.doesNotMatch(text, /3\/0/);
});

test("shouldWake enforces the guard rails", () => {
  const base = { enabled: true, sessionId: "ses_1", iteration: 0, maxIterations: 5 };
  assert.equal(shouldWake(base).ok, true);
  assert.equal(shouldWake({ ...base, enabled: false }).reason, "disabled");
  assert.equal(shouldWake({ ...base, sessionId: "" }).reason, "no_session");
  assert.equal(shouldWake({ ...base, iteration: 5 }).reason, "max_iterations");
  assert.equal(
    shouldWake({ ...base, lastWakeAt: 1000, cooldownMs: 5000, now: 3000 }).reason,
    "cooldown",
  );
  assert.equal(
    shouldWake({ ...base, lastWakeAt: 1000, cooldownMs: 5000, now: 7000 }).ok,
    true,
  );
});
