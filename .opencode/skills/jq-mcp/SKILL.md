---
name: jq-mcp
description: Operate a JoinQuant (聚宽) account through jq-mcp — queue compile/backtest batches, stream results, iterate. Use when the user asks to inspect or modify JoinQuant strategies/backtests, run ablations/sweeps, or set up a jq-mcp driver.
---

# jq-mcp skill

jq-mcp exposes a **logged-in JoinQuant browser session** as MCP tools and as a
local HTTP event API. The extension (jqhelper) performs every browser action;
jq-mcp owns the queue, concurrency, retries and result events.

Read this before touching a strategy. The flow below is **queue-first**: compile
and backtest are batches, results are events.

## 0. Mental model

```
agent ──jq_batch_submit──► shared queue ──► jq-mcp workers ──► JoinQuant
   ▲                                                        │
   └──────── job.done / job.failed events (metrics+logs) ◄──┘
```

- You submit **jobs**, not individual runs.
- The scheduler owns the concurrency cap, back-pressure and terminal waiting.
- Results arrive as events. Wait for them either in the same turn
  (`jq_batch_wait`) or in the next turn (`jq-loop`, default `wakeOn=stream`).
- Compile runs are jobs too (`compile: true`). Never send code to a backtest
  before its compile job passed.

## 1. Preflight

```
jq_health                         # {connected, loggedIn, cap, accounts}; stop if false
jq_accounts                       # pool: ready/pending_login, cap, in-flight, credits
jq_account_limits                 # credits/VIP; concurrency cap is manual + learned
```

- `connected:false` → tell the user to open the browser/enable jqhelper/fix the
  token. Do not retry in a loop.
- `loggedIn:false` → `jq_auth_open_login`, ask the user to log in, then
  `jq_auth_refresh`.
- Any account in `pending_login` → ask the user to finish login in that window;
  the daemon binds the uid automatically. Do not submit jobs that are pinned to it.
- Multi-account: prefer portable jobs (`code`/`codePath` + `strategyKey`) and let
  the scheduler place them; `account` pins a job and disables failover.
- For next-turn iteration: jq-loop must be enabled (`/jq-loop on`) and jq-mcp must
  be running with `--http-addr` (default `127.0.0.1:8792`).
- Confirm before any mutation (`save/clone/delete/move/import`, `cancel/delete`,
  `file_write`); never fabricate `confirm=true`.

## 2. The flow

### Phase A — variants

Create or edit strategies and keep cloud name ↔ local file one-to-one. Record
the intent in `trials.csv` **before** the first formal backtest. For sweeps:
one variant per code change, unique `tag`.

### Phase B — compile batch (gate)

```
jq_batch_submit {
  jobs: [
    {tag:"c-v1", strategyId, start:"2024-01-02", end:"2024-01-10", compile:true},
    ...
  ]
}
```

- Same turn: `jq_batch_wait {batchId}`.
- Next turn: `/jq-loop on`; a `job.failed` arrives with `job.errorLogs`.
- Compile jobs have no metrics; success = `status:"done"`, failure carries
  `error`/`errorLogs`.
- Fix failed variants and recompile. **Do not** submit a backtest for a variant
  whose compile did not pass.

### Phase C — backtest queue (over-subscribe)

Submit **more jobs than the concurrency cap**: `cap + N`, not exactly `cap`.
Queued jobs keep every slot busy while the model is thinking.

```
jq_batch_submit {
  jobs: [ {tag:"v1-long", strategyId:"...", start:"2015-01-01", end:"...",
           capital:100000, frequency:"day"}, ... ]
}
```

- Same turn: `jq_batch_wait {batchId}` (one call; no sleep loop).
- Next turn: `/jq-loop on` — default **stream** mode wakes on completed jobs
  (500ms coalescing), **not** on `batch.done`. Record each result, then submit
  replacements/next variants immediately; do not wait for the slowest job.
- `batch.done` only means “this submission is drained”; it is not the iteration
  barrier.
- If a wake is missed or a client disconnects, resume with
  `jq_batch_events {batchId, sinceEventId: nextEventId}` (or HTTP
  `GET /api/batches/<id>/events?since=`).

### Phase D — results

Terminal events carry what you usually need:

- `job.status`: `done` | `failed` | `timeout`
- `job.metrics`: raw `backtest.stats` payload; the actual metrics are under
  `job.metrics.metrics` (jq-loop digests already unwrap this)
- `job.error`, `job.errorLogs`, `job.id`, `job.listId`, `job.taskId`

Use `jq_backtest_stats` only when you need the raw payload again;
`jq_backtest_logs {error:true}`, `trades`, `positions`, `monthly`, `result` for
details. Export before cleanup when the result matters.

### Phase E — cleanup

Summarize locally, update `trials.csv`, then delete only this repo’s temporary
strategies/backtests (ask first; prefer `listId` for deletion).

## 3. Same-turn vs next-turn

| Situation | Use |
|---|---|
| You need the result before writing the answer | `jq_batch_wait` (single blocking call) |
| The turn can end; continue when results arrive | `/jq-loop on` (stream) |
| External process / cron / audit | webhook (`callbackUrl`) or HTTP attach |
| Client truncates long calls | `jq_batch_events` / HTTP events with `since` |

`jq-loop` wakes only batches submitted **through MCP** (OpenCode puts the session
id in `tools/call._meta["ai.opencode/sessionID"]`). Scripts submitting over HTTP
without `sessionId` write results but do not wake a session.

## 4. Drivers / scripts

- Default: **attach** to the shared jq-mcp; do not spawn a second bridge:

```js
import { startClient } from "<jq-mcp>/clients/mjs/jq-mcp-client.mjs";
const jq = await startClient({ url: "http://127.0.0.1:8792" });
const runs = await jq.compileAll(variants);
const results = await jq.runAll(jobs, { onJob: (r) => console.log(r.tag, r.status, r.metrics?.sharpe) });
await jq.close();
```

- The extension connects to exactly one bridge (`ws://127.0.0.1:8790/ws`). If you
  must self-spawn (different account/token), stop the OpenCode jq-mcp first and
  release both `:8790` and `:8792` when done.
- Use `jq_batch_submit`/`jq_batch_wait` (or the client above), never a hand-rolled
  sleep/poll loop.

## 5. Hard rules / anti-patterns

- Never loop `jq_backtest_run` + `jq_backtest_stats` over a sweep.
- Never `sleep` then poll; use `jq_batch_wait`, events, or `/jq-loop`.
- Never submit exactly `cap` jobs and wait for all: over-subscribe and refill.
- Never backtest uncompiled code.
- Never retry a platform rejection blindly; read `details.maxConcurrent` and let
  the scheduler back off.
- `end` ≤ previous trading day. IDs rotate; resolve strategy ids by name.
- Keep tokens/cookies/source out of reports and logs.
- `useCredit: true` only when the user/team allows it; record it in `trials.csv`.
- Free quota is tried first. If `jq_batch_wait` returns `creditRequired:true` /
  `needsConfirmation`, stop and ask the user; only then call
  `jq_batch_confirm_credit {approve:true, confirm:true}`. Never auto-approve
  credits.

See `references/workflows.md` for command-level recipes and
`references/tools.md` for tool parameters.
