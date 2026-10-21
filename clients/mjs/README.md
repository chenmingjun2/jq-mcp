# jq-mcp mjs client

A single-file, dependency-free ESM client for **compile/backtest sweeps**. It
keeps one `jq-mcp` process alive, submits whole swaths of jobs through the
server-side scheduler, and streams results back with a lossless event cursor.

```js
import { startClient } from "./jq-mcp-client.mjs";

const jq = await startClient();               // spawns bin/jq-mcp, waits for the extension
const compiled = await jq.compileAll([
  { tag: "v1", strategyId: "SID1" },
  { tag: "v2", strategyId: "SID2" },
], { onJob: (r) => console.log(r.tag, r.status), submit: { maxConcurrent: 10 } });

const runs = await jq.runAll([
  { tag: "v1-long", strategyId: "SID1", start: "2015-01-01", end: "2026-09-30" },
], { onJob: (r) => console.log(r.tag, r.metrics?.sharpe) });

await jq.close();
```

## Why

The existing research scripts each embed a copy of an MCP stdio client, spawn
their own `jq-mcp`, and poll `jq_backtest_stats` every N seconds per job. This
client removes that per-script plumbing and moves waiting/queuing to the Go
scheduler:

| | ad-hoc script | this client |
|---|---|---|
| process | one per script | one, reused |
| scheduling | hand-rolled worker pool | `jq_batch_submit` (server owns cap/back-pressure) |
| results | `jq_backtest_stats` polls | `jq_batch_events` cursor, one call per wakeup |
| metrics | one stats call per job per poll | carried in the `job.done` event |
| compile gating | custom loops | `compileAll` batch first, then `runAll` |

## Requirements

- Node.js >= 18 (uses global `fetch`; developed on Node 24)
- `jq-mcp` >= 0.3.0 at `bin/jq-mcp` (or pass `bin`/`JQ_MCP_BIN`)
- the jqhelper extension attached and logged in

## Two modes

**spawn (default)** — the client starts its own `jq-mcp` and owns the bridge
port. The extension only connects to one bridge URL
(`ws://127.0.0.1:8790/ws`), so any other `jq-mcp` on that port must be stopped
first (the client fails fast with a clear error).

**attach** — point the client at a running `jq-mcp`'s HTTP API. No child process,
no port conflict, and it coexists with OpenCode's MCP server:

```js
const jq = await startClient({
  url: "http://127.0.0.1:8791",   // jq-mcp --http-addr
  token: "jqhelper-dev-token",
});
```

In attach mode every scheduler operation goes over HTTP:
`POST /api/batches` for submission, `GET /api/batches/{id}/events` for the
event cursor, and `POST /api/tools/{name}` for `tool()` calls. `tool()` therefore
still works for strategy CRUD / health checks, not just batches.

`JQ_MCP_HOME` defaults to a temp dir, so task snapshots/config do not touch your
normal `~/.config/jq-mcp`.

## API

### `startClient(opts) -> Promise<JqMcpClient>`

| option | default | meaning |
|---|---|---|
| `bin` | `bin/jq-mcp` | server binary (spawn mode) |
| `port` | `8790` | bridge port (spawn mode) |
| `url` | `JQ_MCP_HTTP_URL` | attach to a running jq-mcp HTTP API |
| `token` | `jqhelper-dev-token` | bridge/HTTP token |
| `sessionId` | `JQ_MCP_SESSION_ID` | optional session id stamped on batches (attach mode) |
| `home` | temp dir | `JQ_MCP_HOME` |
| `workers` | server default (16) | scheduler worker threads (`--workers`, spawn mode) |
| `waitExtensionMs` | `120000` | wait for the extension (`0` = don't) |
| `requireLogin` | `false` | also wait for `loggedIn` via `jq_health` |
| `requestTimeoutMs` | `600000` | default request timeout |
| `verbose` | `false` | pipe jq-mcp stderr (spawn mode) |
| `keepHome` | `false` | keep the temp home on `close()` |

### Methods

- `tool(name, args)` — any MCP tool, JSON payload decoded; throws `ToolError`
  on `isError` (with `code`/`details`).
- `submit(jobs, {maxConcurrent, callbackUrl, callbackSecret, callbackOn})` —
  non-blocking enqueue; returns `{batchId, jobs, ...}`.
- `stream(batchId, {sinceEventId, waitSeconds, limit, signal})` — async
  iterator over raw events; long-polls, lossless, ends when the batch is
  terminal.
- `runBatch(jobs, {onJob, onEvent, compile, submit, signal})` — submit + stream,
  returns results **in submission order**. Jobs without `tag` get `job-<n>`.
- `compileAll(jobs, opts)` / `runAll(jobs, opts)` — `runBatch` with
  `compile: true` / `false`.
- `waitForExtension({timeoutMs, requireLogin})`.
- `stats` — `{toolCalls, byTool}` client-side counters.
- `onNotification(fn)` — raw `notifications/message` (server push) subscription.
- `close()` — end stdin, wait up to 3s, then SIGKILL.

### Result shape

```js
{
  tag, strategyId,
  mode: "compile" | "backtest",
  status: "done" | "failed" | "timeout",
  metrics,          // unwrapped metrics object (sharpe, annual_algo_return, ...)
  stats,            // raw backtest.stats payload (id/resolvedId/metrics)
  error, errorLogs, // failure details (errorLogs populated by jq-mcp >= 0.2.1)
  id, listId, taskId,
  start, end, capital, frequency,
  startedAt, endedAt, elapsedMs, batchId
}
```

## Real-time patterns

**Compile gate, then batched backtests** (the usual sweep):

```js
const ok = (await jq.compileAll(variants.map(v => ({ tag: v.tag, strategyId: v.sid })),
  { onJob: r => log("compile", r.tag, r.status) })).filter(r => r.status === "done");

await jq.runAll(ok.flatMap(r => windows.map(w => ({
  tag: `${r.tag}-${w}`,
  strategyId: r.strategyId,
  start: WINDOWS[w].start,
  end: WINDOWS[w].end,
}))), {
  submit: { maxConcurrent: 10 },
  onJob: r => log("backtest", r.tag, r.status, r.metrics?.sharpe),
});
```

**Submit more work while a batch is running** — `submit()` only enqueues, so
new batches are fine at any time; the scheduler queues them behind the cap:

```js
const a = await jq.submit(batchA, { maxConcurrent: 10 });
const b = await jq.submit(batchB); // returns immediately, queued
for await (const ev of jq.stream(a.batchId)) { /* ... */ }
```

**Raw events** (job.started/done/failed/timeout, batch.done) for progress UIs:

```js
for await (const ev of jq.stream(batchId, { waitSeconds: 30 })) {
  console.log(ev.type, ev.job?.tag, ev.job?.status, ev.job?.error);
}
```

Failed jobs carry `error` and `errorLogs` (jq-mcp 0.2.1+): runtime errors fail
the job as soon as the error log appears, without waiting for the job timeout.

## Benchmark (mock extension)

Measured on this machine with a mock extension that finishes a run on the second
scheduler poll (3s interval), `workers=cap=20`; no JoinQuant traffic:

| scenario | jobs | wall | throughput | first result | p50 / p95 |
|---|---|---|---|---|---|
| compile batch | 20 | 3.0s | 6.6 jobs/s | 3.0s | 3.0s / 3.0s |
| backtest batch | 60 | 9.0s | 6.7 jobs/s | 3.0s | 6.0s / 9.0s |
| runtime error job | 3 | 3.0s | — | — | job `failed` with `errorLogs` |
| legacy `jq_backtest_run` + 8s polls, 3 workers | 12 | 64.0s | 0.19 jobs/s | — | — |

- `submit()` returns in ~3ms; results are pushed as each job ends.
- 60 backtests cost **8–10 tool calls total** (1 submit + 7–9 event long-polls); the
  legacy path needs 1 + 2 stats calls per job plus sleeps.
- Metrics ride along in `job.done`, so no per-job stats calls are made by the
  client.

The 3s floor is the server's scheduler poll interval; the client adds no polling
of its own. Real JoinQuant runs are dominated by platform time, so the win is
mostly in concurrency utilization and zero idle client polls.

## Relation to MCP

In spawn mode this is a normal MCP stdio client (1:1 with its child process).
In attach mode it speaks jq-mcp's small HTTP API instead, so several scripts,
dashboards and the OpenCode `jq-loop` plugin can share the one process that owns
the browser bridge. See `docs/protocol.md` §3 for the endpoints.
