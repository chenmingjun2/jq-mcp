# jq-mcp 协议说明

本文档定义两层协议：

1. **内层**：jq-mcp ↔ jqhelper 的 WebSocket 桥协议。
2. **外层**：jq-mcp ↔ AI 客户端的 MCP 工具协议。

---

## 1. WebSocket 桥协议

### 1.1 连接

```
GET ws://127.0.0.1:8790/ws HTTP/1.1
Upgrade: websocket
```

连接建立后，插件必须在 5 秒内发送 `hello` 通知并携带正确 `token`，否则服务端关闭连接。

### 1.2 信封

采用 JSON-RPC 2.0 风格，按帧发送单个 JSON 对象。

服务端 → 插件（请求）：

```json
{"jsonrpc":"2.0","id":"s-123","method":"strategy.list","params":{"limit":50}}
```

插件 → 服务端（成功响应）：

```json
{"jsonrpc":"2.0","id":"s-123","result":{"items":[]}}
```

插件 → 服务端（错误响应）：

```json
{"jsonrpc":"2.0","id":"s-123","error":{"code":"not_authenticated","message":"未登录","details":{}}}
```

插件 → 服务端（通知，无 `id`）：

```json
{"jsonrpc":"2.0","method":"hello","params":{"client":"jqhelper","version":"0.2.1","token":"...","browser":"chrome"}}
{"jsonrpc":"2.0","method":"event","params":{"type":"auth.changed","loggedIn":false}}
```

服务端 → 插件（保活）：`{"jsonrpc":"2.0","method":"ping"}`，插件回 `{"jsonrpc":"2.0","method":"pong"}`。

### 1.3 桥方法

| method | params | result |
|--------|--------|--------|
| `ping` | `{}` | `{"pong":true}` |
| `auth.status` | `{}` | `{"loggedIn":bool,"raw":{}}` |
| `auth.getCookie` | `{}` | `{"cookie":"k=v; ...","count":n}` |
| `auth.openLogin` | `{"url"}` | `{"tabId":123,"url":"..."}` |
| `auth.refresh` | `{}` | `{"loggedIn":bool}` |
| `strategy.list` | `{"fid","limit","all","sort"}` | `{"items":[Strategy],"folders":[Folder]}` |
| `strategy.find` | `{"name"}` | `{"matched":Strategy?,"exact":[],"partial":[]}` |
| `strategy.get` | `{"strategyId","includeCode"}` | `StrategyDetail` |
| `strategy.create` | `{"name","code","type","folderId"}` | `{"id","name","type"}` |
| `strategy.save` | `{"strategyId","name","code"}` | `{"id","saveId","name"}` |
| `strategy.delete` | `{"strategyId"}` | `{"ok","id","internalId"}` |
| `strategy.move` | `{"strategyIds":[],"folderId"}` | `{"ok","ids","folderId"}` |
| `folder.list` | `{"fid","recursive"}` | `{"folders":[{"fid","name"}]}` |
| `folder.create` | `{"name","parentId"}` | `{"ok","name","parentId"}` |
| `backtest.run` | `{"strategyId","start","end","capital","frequency","compile"}` | `{"id","listId","status"}` |
| `backtest.list` | `{"strategyId","status","limit","compile"}` | `{"items":[Backtest]}` |
| `backtest.get` | `{"backtestId"}` | `{"id","listId","status","code","metrics"}` |
| `backtest.stats` | `{"backtestId"}` | `{"id","resolvedId","metrics"}` |
| `backtest.result` | `{"backtestId","offset","userRecordOffset"}` | `{"id","resolvedId","state","result"}` |
| `backtest.logs` | `{"backtestId","offset","error","all"}` | `{"id","resolvedId","logs","state","nextOffset"}` |
| `backtest.trades` | `{"backtestId"}` | `{"id","resolvedId","count","items":[Trade]}` |
| `backtest.positions` | `{"backtestId"}` | `{"id","resolvedId","count","items":[Position]}` |
| `backtest.monthly` | `{"backtestId"}` | `{"id","resolvedId","risk":{}}` |
| `backtest.cancel` | `{"backtestId"}` | `{"ok","id","resolvedId"}` |
| `backtest.delete` | `{"backtestId","compile"}` | `{"ok","id"}` |

`Strategy`：`{id, internalId, name, type, updatedAt, runCount, backtestCount, folderId?}`。
`Folder`：`{fid, name}`。
`Backtest`：`{id, listId, sourceId, strategyId, name, status, startDate, endDate, capital, frequency, metrics, submittedAt}`。

### 1.4 桥错误码

`no_extension` · `not_authenticated` · `not_found` · `usage_error` · `api_error` · `network_error` · `timeout` · `internal_error`

---

## 2. MCP 工具

传输：stdio，newline-delimited JSON-RPC 2.0。支持 `initialize` / `tools/list` / `tools/call` / `ping`。

### 2.1 诊断

| 工具 | 参数 | 说明 |
|------|------|------|
| `jq_bridge_status` | — | 插件连接数、最近 hello |
| `jq_health` | — | 一次探测 connected + loggedIn |

### 2.2 认证

| 工具 | 参数 |
|------|------|
| `jq_auth_status` | — |
| `jq_auth_get_cookie` | —（敏感） |
| `jq_auth_open_login` | `url?` |
| `jq_auth_refresh` | — |

### 2.3 策略

| 工具 | 参数 | 备注 |
|------|------|------|
| `jq_strategy_list` | `fid?`, `limit?`, `all?`, `sort?` | 返回 items + folders |
| `jq_strategy_find` | `name` | 精确/模糊匹配 |
| `jq_strategy_get` | `strategyId`, `includeCode?` | |
| `jq_strategy_create` | `name`, `code?`, `type?`, `folderId?` | |
| `jq_strategy_save` | `strategyId`, `name?`, `code?`, `confirm` | 需 confirm |
| `jq_strategy_clone` | `strategyId`, `name?`, `confirm` | 需 confirm |
| `jq_strategy_delete` | `strategyId`, `confirm` | 需 confirm |
| `jq_folder_list` | `fid?`, `recursive?` | |
| `jq_folder_create` | `name`, `parentId?` | |
| `jq_strategy_move` | `strategyIds[]`, `folderId`, `confirm` | 需 confirm |

### 2.4 回测执行 / 任务

| 工具 | 参数 | 备注 |
|------|------|------|
| `jq_backtest_run` | `strategyId`, `start`, `end?`, `capital?`, `frequency?`, `compile?` | 登记任务 |
| `jq_backtest_list` | `strategyId`, `status?`, `limit?`, `compile?` | |
| `jq_backtest_wait` | `backtestId`, `timeoutSeconds?`, `pollIntervalSeconds?` | |
| `jq_backtest_cancel` | `backtestId`, `confirm` | 需 confirm |
| `jq_backtest_delete` | `backtestId`, `compile?`, `confirm` | 需 confirm |
| `jq_backtest_batch` | `strategyId`, `runs[]`, `compile?` | 批量提交 |
| `jq_backtest_compare` | `backtestIds[]` | 指标对比 |
| `jq_task_list` | `limit?` | |
| `jq_task_get` | `taskId` | |

### 2.5 回测数据

| 工具 | 参数 |
|------|------|
| `jq_backtest_get` | `backtestId` |
| `jq_backtest_stats` | `backtestId` |
| `jq_backtest_result` | `backtestId`, `offset?`, `userRecordOffset?` |
| `jq_backtest_logs` | `backtestId`, `offset?`, `error?`, `all?` |
| `jq_backtest_trades` | `backtestId` |
| `jq_backtest_positions` | `backtestId` |
| `jq_backtest_monthly` | `backtestId` |
| `jq_backtest_export` | `backtestId`, `path`, `sections?` |

### 2.6 本地代码 / 文件

| 工具 | 参数 | 备注 |
|------|------|------|
| `jq_code_standardize` | `code?`, `path?`, `outputPath?` | 注入滑点/手续费 |
| `jq_strategy_export` | `strategyId`, `path`, `format?` | 远端→本地 |
| `jq_strategy_import` | `strategyId`, `path`, `name?`, `confirm` | 本地→远端 |
| `jq_file_read` | `path` | |
| `jq_file_write` | `path`, `content`, `confirm` | 需 confirm |

### 2.7 工具返回

成功：`{"content":[{"type":"text","text":"<JSON>"}],"isError":false}`
失败：`{"content":[{"type":"text","text":"<错误 JSON>"}],"isError":true}`

### 2.8 日期与数值约定

- 日期：`YYYY-MM-DD`；时间戳：Unix 毫秒。
- 百分比：小数值（`0.1832` = `18.32%`），除非字段名含 `percent`。

---

## 3. 兼容与演进

- 桥协议新增字段必须向后兼容；未知方法返回 `usage_error`。
- MCP 工具名稳定；新增能力以新工具形式加入。
- `protocolVersion` 在 `initialize` 中协商；服务端回显客户端版本，缺省 `2024-11-05`。
