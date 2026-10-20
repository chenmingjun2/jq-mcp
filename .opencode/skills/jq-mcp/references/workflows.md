# jq-mcp workflows

## 0. Always start here

```
jq_health
```
- `connected:false` → stop; tell the user to open the browser, enable jqhelper, and
  set the token. Do not loop.
- `loggedIn:false` → `jq_auth_open_login`, ask the user to log in, then `jq_auth_refresh`.

## 1. Read a strategy's source

```
jq_strategy_find {name}
jq_strategy_get  {strategyId, includeCode:true}
```
Note: JoinQuant edit ids rotate — resolve by name right before you need it.

## 2. Create / edit and backtest

```
jq_strategy_create {name, code}          # or jq_strategy_save on an existing id
jq_strategy_compile {strategyId}         # ok:false → fix code, do not backtest
jq_backtest_run {strategyId, start, end, capital?, frequency?}
jq_backtest_wait {backtestId}            # or poll jq_backtest_stats later
```
- `end` must be ≤ the previous trading day.
- On `api_error` with `details.maxConcurrent`, a slot is busy — wait and retry.
- `jq_backtest_wait` also polls the error log: as soon as the run writes one it
  returns `status:"failed"` with `errors[]` instead of waiting for the timeout.
  The scheduler does the same for `jq_batch_submit` and fires `job.failed` /
  `batch.done` webhooks immediately.

## 3. Read backtest results

```
jq_backtest_stats     {backtestId}
jq_backtest_result    {backtestId, offset}      # equity curve, paged
jq_backtest_trades    {backtestId}              # transactions
jq_backtest_positions {backtestId}              # daily positions
jq_backtest_monthly   {backtestId}              # period/monthly returns
jq_backtest_logs      {backtestId, error:true}  # compile/runtime errors
```
Stats use decimals: `algorithm_return=0.24` means +24%; `sharpe`, `max_drawdown`,
`annual_algo_return`, `trading_days`, `turnover_rate`.

## 4. Ablation / parameter sweep

For each variant (one strategy per code change):
1. `jq_strategy_create {name:"ABL-<tag>", code}`
2. `jq_strategy_compile {strategyId}` — skip variants that fail
3. `jq_backtest_run {strategyId, start, end, capital, frequency:"day"}`
4. collect `jq_backtest_stats`

Guidance:
- Keep a control (baseline) variant.
- Vary **one** thing per variant (a leg, a weight, a band, the window).
- Respect the concurrency cap; prefer `jq_batch_submit` so the server fills
  parallel slots and backs off automatically.
- Use the **longest available window per instrument**; check listing dates first
  (see below) — a real ETF may be newer than your intended start.
- Record results locally (e.g. via `jq_backtest_export`) before cleanup.

Recommended loop (no client-side sleep):

```
jq_batch_submit {jobs:[{tag:"v1",strategyId,start,end}, ...]}
jq_batch_wait   {batchId}                    # one blocking call
# if the client truncates long calls:
jq_batch_wait   {batchId, sinceEventId: nextEventId}
jq_batch_events {batchId, sinceEventId: nextEventId, waitSeconds: 60}
```

If you need an external process to react, pass `callbackUrl` (and optionally
`callbackSecret`) to `jq_batch_submit`; jq-mcp POSTs `batch.done` / `job.*`
events with retries. MCP cannot wake the agent, so `jq_batch_wait` remains the
agent-side primitive.

To get an instrument's listing date, run a tiny strategy whose `initialize` logs
`get_security_info(code).start_date`, then read `jq_backtest_logs`.

### Interpreting an ablation (overfitting checks)

- If a leg/asset only exists recently (check listing dates) but the strategy
  "needs" it, that is **hindsight selection**.
- Compare **short vs long** windows: if ranking flips (which leg is best), recent
  results don't generalize.
- Compare vs a naive equal-weight of the legs (rebalancing premium) and vs the
  benchmark to separate **Beta from alpha**.
- Prefer improvements that are stable across windows (e.g. adding a bond/cash leg
  to cut drawdown) over "swap an asset to raise return".

## 5. Export / import

```
jq_strategy_export {strategyId, path:"./s.py", format:"py"}
jq_backtest_export {backtestId, path:"./bt.json", sections:"stats,result,trades"}
jq_strategy_import {strategyId, path:"./s.py", confirm:true}
jq_code_standardize {code}      # inject slippage/cost into initialize
```

## 6. Cleanup

Delete temporary strategies you created (ask first):
```
jq_strategy_delete {strategyId, confirm:true}
jq_backtest_delete {backtestId: <listId>, compile?, confirm:true}
```

## 7. Rate limits

```
jq_account_limits        # try to read the concurrency cap
```
Or rely on the reactive `details.maxConcurrent` from a rejected submit.
