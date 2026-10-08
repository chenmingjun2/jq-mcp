# jq-mcp tool reference

40 tools. `confirm` marks write/destructive operations that need `confirm=true`.

## Diagnostics

- `jq_bridge_status` — connected clients, extension version, last hello.
- `jq_health` — `{connected, loggedIn}` in one call.

## Auth / account

- `jq_auth_status` — `{loggedIn}`.
- `jq_auth_get_cookie` — cookie header (sensitive).
- `jq_auth_open_login` — `{url?}` open login page for manual login.
- `jq_auth_refresh` — reload the JoinQuant tab to renew the session.
- `jq_account_limits` — scrape account API + membership page for the concurrent
  compile/backtest cap and VIP info.

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
- `jq_backtest_run` — `{strategyId, start, end?, capital?, frequency?, compile?}`
  → `{taskId, id, listId, strategyId, mode, status}`.
- `jq_backtest_list` — `{strategyId, status?, limit?, compile?}` → `{items}`.
- `jq_backtest_wait` — `{backtestId, timeoutSeconds?, pollIntervalSeconds?}`
  → `{status: done|timeout, stats}`.
- `jq_backtest_cancel` — `{backtestId, confirm}`.
- `jq_backtest_delete` — `{backtestId, compile?, confirm}` (prefer `listId`).
- `jq_backtest_batch` — `{strategyId, runs:[{start,end,capital?,frequency?,compile?}], compile?}`.
- `jq_backtest_compare` — `{backtestIds[]}` → per-id `stats`.

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
