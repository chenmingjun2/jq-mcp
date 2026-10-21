# jq-mcp workflows (queue-first)

The standard path is **compile batch → gate → backtest queue → stream results →
refill → cleanup**. Individual `jq_backtest_run` calls are for ad-hoc checks only.

## 0. Preflight

```
jq_health
```
- `connected:false` → stop; ask the user to open the browser, enable jqhelper and
  set the token. Do not loop.
- `loggedIn:false` → `jq_auth_open_login`, ask the user to log in, `jq_auth_refresh`.
- Concurrency cap: `jq_account_limits` (or the `details.maxConcurrent` from a
  rejection). Remember it as `cap`.

For next-turn iteration: `/jq-loop on` (OpenCode), and jq-mcp started with
`--http-addr` (default `127.0.0.1:8792`).

## 1. Read a strategy

```
jq_strategy_find {name}
jq_strategy_get  {strategyId, includeCode:true}
```

Edit ids rotate — resolve by name immediately before use.

## 2. Compile batch (gate)

One batch for all variants:

```
jq_batch_submit {
  jobs: [
    {tag:"c-v1", strategyId:"...", start:"2024-01-02", end:"2024-01-10", compile:true},
    {tag:"c-v2", strategyId:"...", start:"2024-01-02", end:"2024-01-10", compile:true}
  ]
}
jq_batch_wait {batchId}
```

- `job.done` = compile passed (no metrics); `job.failed` carries
  `error`/`errorLogs`.
- Fix failures, then recompile. Never backtest a failed variant.
- Same-turn wait is fine; for many variants use `/jq-loop on` and let stream
  wakes deliver each compile result.

## 3. Backtest queue (over-subscribe)

Flatten `variants × windows` into jobs. Submit **`cap + N`** jobs, not exactly
`cap`: queued jobs keep slots warm while you think.

```
jq_batch_submit {
  jobs: [
    {tag:"v1-long",   strategyId:"...", start:"2015-01-01", end:"...", capital:100000, frequency:"day"},
    {tag:"v1-recent", strategyId:"...", start:"2024-01-01", end:"...", capital:100000, frequency:"day"},
    ...
  ],
  maxConcurrent: 10
}
```

**Same turn** (need results before answering):

```
jq_batch_wait {batchId}                       # one blocking call
jq_batch_wait {batchId, sinceEventId: next}   # incremental follow
```

**Next turn** (turn can end):

```
/jq-loop on        # stream (default): wake on each completed job, 500ms coalesce
```

Then, on every wake:

1. append/record the new `job` results (`tag`, `status`, `metrics`, `errorLogs`);
2. if the queue is draining, submit replacements or the next variants;
3. do **not** wait for `batch.done` — it is not the iteration barrier.

If an event is missed:

```
jq_batch_events {batchId, sinceEventId: nextEventId, waitSeconds: 30}
```

## 4. Result shape

- Events: `job.status`, `job.metrics` (raw `backtest.stats`; actual metrics under
  `job.metrics.metrics`), `job.error`, `job.errorLogs`, `job.id`, `job.listId`,
  `job.taskId`.
- `jq_batch_wait` returns `{counts, done, jobs, events?, nextEventId}`.
- Use `jq_backtest_stats` for the raw payload; `logs(error:true)` for errors;
  `trades/positions/monthly/result` for detail.
- Stats use decimals: `algorithm_return=0.24` = +24%.

## 5. Drivers (Node)

Attach to the shared jq-mcp (no second bridge):

```js
import { startClient } from "<jq-mcp>/clients/mjs/jq-mcp-client.mjs";

const jq = await startClient({ url: "http://127.0.0.1:8792" });
const compile = await jq.compileAll(variants, { onJob: (r) => log(r) });
const ok = compile.filter((r) => r.status === "done");
const results = await jq.runAll(jobs, {
  submit: { maxConcurrent: cap },
  onJob: (r) => log(r.tag, r.status, r.metrics?.sharpe),
});
await jq.close();
```

- `compileAll`/`runAll` stream via HTTP events; no per-job polling.
- Only self-spawn if you need isolation; stop the OpenCode jq-mcp first, and
  release `:8790`/`:8792` afterwards.
- Scripts submitting over HTTP without `sessionId` do not wake an OpenCode
  session; use them for batch jobs, not interactive iteration.

## 6. Ablation quality checks

- Keep a control/baseline variant; vary one thing per variant.
- Compare short vs long windows; ranking flips = not generalizable.
- Separate beta from alpha (vs benchmark / equal-weight legs).
- Prefer stable improvements (drawdown/cost) over a single-point return peak.
- Use the longest available window per instrument; check listing dates first
  (a tiny strategy that logs `get_security_info(code).start_date`, then
  `jq_backtest_logs`).

## 7. Export / cleanup

```
jq_strategy_export {strategyId, path:"./s.py", format:"py"}
jq_backtest_export {backtestId, path:"./bt.json", sections:"stats,result,trades"}
jq_strategy_delete {strategyId, confirm:true}
jq_backtest_delete {backtestId:<listId>, confirm:true}
```

Summarize locally and update `trials.csv` **before** deleting cloud artifacts.
Only delete this repo’s prefixed temporary strategies; never user-owned ones.

## 8. Rate limits / failures

```
jq_account_limits
```

- On `api_error` with `details.maxConcurrent`, the scheduler already tightens and
  retries; do not spray more submissions.
- Compile/backtest runtime errors fail immediately with `errorLogs`; read them
  before retrying.
- `useCredit: true` only when authorized; record which runs used credits.

## 9. Anti-patterns

- Per-job `jq_backtest_run` + `jq_backtest_stats` loops.
- `sleep`-based polling.
- Waiting for `batch.done` before submitting the next wave.
- Submitting exactly `cap` jobs and leaving slots idle.
- Backtesting code that did not compile.
- Deleting cloud strategies before saving results.
