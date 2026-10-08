---
name: jq-mcp
description: Operate a JoinQuant (聚宽) account through jq-mcp MCP tools — read strategy code, create/edit strategies, compile-check, run/manage backtests, read backtest data, run ablations/sweeps, and clean up. Use when the user asks to inspect or modify JoinQuant strategies/backtests via jq-mcp.
---

# jq-mcp skill

jq-mcp exposes a **logged-in JoinQuant browser session** as MCP tools. The server
never calls JoinQuant directly; a browser extension (jqhelper) performs every
action. Treat this skill as the operating manual for those tools.

## When to use

Use when the user wants to, via jq-mcp:
- check auth / bridge status,
- read or edit strategy source,
- compile-check and run backtests,
- read backtest metrics / trades / positions / logs / monthly,
- run **ablation / parameter sweeps** across many variants,
- export strategy/backtest data locally, and clean up cloud artifacts.

## Prerequisites (check first)

1. `jq_health` → must be `{"connected":true,"loggedIn":true}`.
   - `connected:false` → the plugin isn't attached (user must open the browser /
     enable jqhelper / fix token). Do NOT retry in a loop.
   - `loggedIn:false` → call `jq_auth_open_login` and ask the user to log in.

## Golden rules

1. **Confirm before mutating.** `jq_strategy_save/clone/delete`, `jq_strategy_move`,
   `jq_strategy_import`, `jq_backtest_cancel/delete`, `jq_file_write` require
   `confirm=true`. Only pass it when the user approved.
2. **Compile before backtesting.** Run `jq_strategy_compile` first; if it returns
   `ok:false`, fix the code — do not spend a backtest on code that won't compile.
3. **IDs rotate / need resolution.** JoinQuant edit ids change per request; resolve
   by name via `jq_strategy_find`. Prefer the ids returned by the latest call.
   The backtest `id` (detail) and `listId` (list) are different — use `listId` to delete.
4. **Dates.** `YYYY-MM-DD`. The **end date must not be later than the previous
   trading day** (today won't work). Use the last completed trading day.
5. **Respect the concurrency cap.** JoinQuant allows few concurrent compile/backtests.
   On rejection, `details.maxConcurrent` carries the number. Back off and retry
   rather than firing everything at once. See `jq_account_limits`.
6. **Don't leak secrets.** `jq_auth_get_cookie` / source code are sensitive; do not
   echo them broadly. Don't log tokens.
7. **Clean up** temporary strategies/backtests you created (ask first), unless the
   user wants to keep them.

## Standard loops

- Read-only inspect: `jq_health` → `jq_strategy_list` → `jq_backtest_list` →
  `jq_backtest_stats`.
- Edit + backtest: `jq_strategy_find` → `jq_strategy_get(includeCode)` → edit →
  `jq_strategy_save(confirm)` → `jq_strategy_compile` → `jq_backtest_run` →
  `jq_backtest_wait` / `jq_backtest_stats`.
- Ablation: for each variant `jq_strategy_create` → `jq_strategy_compile` →
  `jq_backtest_run` → collect `jq_backtest_stats`; finally delete variants.

See `references/workflows.md` for step-by-step recipes and
`references/tools.md` for every tool's parameters.
