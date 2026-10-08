# jq-mcp troubleshooting

## `no_extension`
The browser plugin isn't attached to the bridge.
- Ensure the browser is open and jqhelper is enabled (`chrome://extensions`).
- Ensure the plugin options page has the correct `ws://127.0.0.1:<port>/ws` and `token`.
- Ensure `bin/jq-mcp` is running and listening; check `jq_bridge_status`.

## `not_authenticated`
- Log in to JoinQuant in the browser, then `jq_auth_refresh` / `jq_health`.
- The extension shares the browser session; there is no separate password.

## Connection flaps (connect then immediately drop)
Token mismatch. The server logs `providedLen` vs `expectedLen`. Make the plugin's
token equal to the server's token (`~/.config/jq-mcp/config.json`).

## `api_error: 当前并行编译或回测数量最多 N 个`
The account concurrency cap is hit. `details.maxConcurrent` = N. Wait for a slot
and retry; do not submit everything at once.

## `api_error: 结束时间不能大于前一个交易日`
Pick an `end` on or before the last completed trading day (today usually fails).

## Stale strategy id
JoinQuant edit ids rotate. Re-resolve with `jq_strategy_find` and use the id from
the most recent call. `internalId` is what delete needs; the extension resolves it.

## Backtest finished but stats empty
`jq_backtest_stats` returns core metrics only when done. Poll
`jq_backtest_stats` / `jq_backtest_list` (status) or use `jq_backtest_wait`.
If `jq_backtest_stats` errors repeatedly, the id may be stale — re-list.

## Launch mode dropped my extensions
Never pass `--disable-extensions-except`. `launch` only uses `--load-extension`.
For daily use prefer `attach` mode and never let jq-mcp restart your browser.

## Headless extension doesn't connect
MV3 service workers can be unreliable headless. Prefer `attach` (your own window)
for reliability; if you must use headless, verify `jq_health` connects.

## Sensitive data
`jq_auth_get_cookie` and strategy source are sensitive. Don't echo tokens/cookies
in summaries; redact when reporting.
