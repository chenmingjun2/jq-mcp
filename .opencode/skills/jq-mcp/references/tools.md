# jq-mcp tool reference

41 tools. `confirm` marks write/destructive operations that need `confirm=true`.

## Diagnostics

- `jq_bridge_status` — connected clients, extension version, account binding, last hello.
- `jq_health` — `{connected, loggedIn, cap, accounts[]}` in one call.
- `jq_accounts` — `{refresh?}` → `{default, cap, connected, accounts[], clients[]}`.
  Account fields: alias, uid, nickname, state (`pending_login|ready|busy|cooling|
  quota_exhausted|logged_out|disabled`), cap, inFlight, queued, credits,
  creditsKnown, cooldownUntil, lastError.

## Auth / account

- `jq_auth_status` — `{account?}` → `{loggedIn}`.
- `jq_auth_get_cookie` — cookie header (sensitive).
- `jq_auth_open_login` — `{url?}` open login page for manual login.
- `jq_auth_refresh` — reload the JoinQuant tab to renew the session.
- `jq_account_limits` — `{account?}` → account identity, credits, VIP info.
  The concurrency cap is configured manually (`jq-mcp accounts cap`); the old
  VIP pages are 404 and platform errors tighten the cap automatically.

## Account pool

- One daemon serves many browser profiles. Each profile is one JoinQuant account;
  identity is the stable `userId` returned by `auth.identity`, never the hello
  payload or the nickname.
- Launch/manage profiles from a shell:
  `jq-mcp accounts add a1 a2` → `accounts open --all` → login manually →
  `accounts status` / `accounts cap a1 3` / `accounts close --all`.
- Use `jq_accounts` to see readiness, cap, in-flight, queue, credits, cooldown.
- `pending_login` means the browser is connected but not logged in; ask the user
  to finish login, then the daemon binds the uid automatically.
- Prefer portable jobs (`code`/`codePath` + `strategyKey`) when you want the
  scheduler to spread work across accounts. Pin with `account` only when the
  strategy must stay on one account.
- UA spoofing is per profile and persisted. Check/reroll with
  `jq-mcp accounts ua <alias> [random|off|<UA>]`; it only takes effect after the
  browser is restarted.
- Free quota is always tried first. If a batch pauses with
  `creditRequired:true`, ask the user before calling
  `jq_batch_confirm_credit {approve:true, confirm:true}`.

## Strategy

- `jq_strategy_list` — `{fid?, limit?, all?, sort?}` → `{items, folders}`.
  Items carry `id` (edit id), `internalId`, `name`, `type`, `updatedAt`,
  `runCount`, `backtestCount`, `folderId?`.
- `jq_strategy_find` — `{name}` → `{matched, exact[], partial[]}`.
- `jq_strategy_get` — `{strategyId, includeCode?}` → `{id, saveId, backtestId, name, code?}`.
- `jq_strategy_create` — `{name, code?, type?, folderId?}` → `{id, name, type}`.
- `jq_strategy_save` — `{strategyId, name?, code?, confirm}`.
- `jq_strategy_clone` — `{strategyId, name?, confirm}`.
- `jq_strategy_delete` — `{strategyId, confirm}`.

## Folders

- `jq_folder_list` — `{fid?, recursive?}` → `{folders:[{fid,name}]}`.
- `jq_folder_create` — `{name, parentId?}`.
- `jq_strategy_move` — `{strategyIds[], folderId, confirm}`.

## Backtest execution / compile

- `jq_strategy_compile` — `{strategyId, start?, end?}` → `{id, listId, ok,
  compileErrors[], logs[]}`. Use before real backtests.
- `jq_backtest_run` — `{strategyId, start, end?, capital?, frequency?, compile?, account?}`
  → `{taskId, id, listId, strategyId, mode, status, account}`.
- `jq_backtest_list` — `{strategyId, status?, limit?, compile?}` → `{items}`.
- `jq_backtest_wait` — `{backtestId, timeoutSeconds?, pollIntervalSeconds?}`
  → `{status: done|failed|timeout, stats?, error?, errors?}`. Fails fast when
  the run writes an error log. Runs registered as compile tasks poll the build
  list instead of stats.
- `jq_backtest_cancel` — `{backtestId, confirm}`.
- `jq_backtest_delete` — `{backtestId, compile?, confirm}` (prefer `listId`).
- `jq_backtest_batch` — `{strategyId, runs:[{start,end,capital?,frequency?,compile?}], compile?}`.
- `jq_backtest_compare` — `{backtestIds[]}` → per-id `stats`.

## Task queue / batch (server-side scheduler)

- `jq_batch_submit` — `{jobs:[{tag?,strategyId?,strategyKey?,name?,code?,codePath?,
  account?,placement?,start,end?,capital?,frequency?,compile?,useCredit?,priority?}],
  account?, placement?, maxConcurrent?, workers?, callbackUrl?, callbackSecret?,
  callbackOn?}` → `{batchId, cap, accountCaps, jobs,...}`.
  Non-blocking. Jobs with `code`/`codePath` are portable: the scheduler picks an
  account and upserts a `[jq-mcp] <name>` copy there (reused by code hash).
  Jobs with only `strategyId` stay on the default/pinned account.
- `jq_batch_status` — `{batchId?, limit?}` → batch snapshot (with
  `countsByAccount`) or recent batches.
- `jq_batch_wait` — `{batchId, timeoutSeconds?, sinceEventId?, includeEvents?}`
  → `{counts, countsByAccount, done, jobs, events?, nextEventId, newEventCount}`.
  Blocks until terminal; with `sinceEventId>0` returns on any new event.
- `jq_batch_events` — `{batchId, sinceEventId?, waitSeconds?, limit?}` → `{events,
  nextEventId, done, hasMore}`. Use for resume/long-poll.
- `jq_batch_confirm_credit` — `{batchId, approve, tags?, confirm}`. Free quota is
  always tried first; on exhaustion jobs become `needs_confirmation` and
  `jq_batch_wait` returns immediately with `creditRequired:true`. Approve only
  after the user explicitly agrees to spend credits; `approve:false` fails them.
- Each job carries `account`, `accountUid`, `attempts`; webhook headers add
  `X-JQ-Account`. After submission, account loss never re-runs the job (the
  remote run may still be alive); it retries reads and fails with `remoteId`.
- Callback: when `callbackUrl` is set, terminal events are POSTed asynchronously
  with `X-JQ-Signature` (HMAC-SHA256 over `timestamp.body`) and retried with
  backoff. MCP also emits `notifications/message` on terminal events.
  Error logs are polled alongside metrics: a run that writes one becomes
  `failed` immediately (`job.errorLogs` in the payload), so the webhook does not
  wait for the job timeout.

### Loop modes (jq-loop)

- `/jq-loop on` makes the OpenCode plugin wake the originating session from the
  HTTP event feed. Default `wakeOn=stream`: each terminal job wakes the session
  (500ms coalescing), **not** `batch.done`. `job` = window 0; `batch` = old
  barrier behavior; `failed` = first failure only.
- Only jobs submitted through MCP carry the OpenCode `sessionId`; HTTP/CLI
  submissions write events but do not wake a session.
- For slot utilization, submit `cap + N` jobs and refill on each wake; do not
  wait for the slowest job in a batch.
- `jq_batch_wait` remains the same-turn primitive; `jq-loop` is for turns that
  already ended.

## Backtest data

- `jq_backtest_get` — `{backtestId}` → detail + metrics + source.
- `jq_backtest_stats` — `{backtestId}` → `{metrics}` (algorithm_return,
  annual_algo_return, benchmark_return, max_drawdown, sharpe, sortino,
  volatility, win_ratio, turnover_rate, trading_days, ...).
- `jq_backtest_result` — `{backtestId, offset?, userRecordOffset?}` → curve.
- `jq_backtest_logs` — `{backtestId, offset?, error?, all?}`.
- `jq_backtest_trades` — `{backtestId}` → `{count, items:[Trade]}`.
- `jq_backtest_positions` — `{backtestId}` → `{count, items:[Position]}`.
- `jq_backtest_monthly` — `{backtestId}` → `{risk:{algorithmPeriodReturn:[...]}}`.
- `jq_backtest_export` — `{backtestId, path, sections?}`; sections:
  `stats,result,trades,positions,monthly,logs,errors,source`.

## Local code / files

- `jq_code_standardize` — `{code?, path?, outputPath?}` injects slippage/cost into `initialize`.
- `jq_strategy_export` — `{strategyId, path, format?}` (`py|json`).
- `jq_strategy_import` — `{strategyId, path, name?, confirm}`.
- `jq_file_read` — `{path}`.
- `jq_file_write` — `{path, content, confirm}`.

## Task registry

- `jq_task_list` — `{limit?}` tasks submitted by this jq-mcp process.
- `jq_task_get` — `{taskId}`.

## Error shape

Tool errors are JSON in the text part: `{"error":{"code","message","details"}}`.
Codes: `no_extension`, `not_authenticated`, `not_found`, `usage_error`,
`api_error`, `network_error`, `timeout`, `internal_error`.
`api_error.details.maxConcurrent` is set when the concurrency cap is hit.

## Local HTTP API (non-MCP)

Enabled with `--http-addr 127.0.0.1:8792`. `/api/*` requires the bridge token
(`Authorization: Bearer`, `X-JQ-Token`, or `?token=`); `/healthz` is open.

- `POST /api/batches` — same scheduler submit shape as `jq_batch_submit`
  (`jobs`, `maxConcurrent?`, `callback*`, `sessionId?`).
- `GET /api/batches[/{id}]` — batch snapshots with `counts`/`done`.
- `GET /api/batches/{id}/events?since=&wait=&limit=` — per-batch cursor events.
- `GET /api/events?since=&wait=&limit=` — cross-batch cursor events (used by the
  OpenCode `jq-loop` plugin).
- `POST /api/tools/{name}` — synchronous tool proxy (`{"arguments":{...}}`).

See `docs/protocol.md` §3 for response shapes.
