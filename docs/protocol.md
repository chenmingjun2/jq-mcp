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

MCP 侧另有 `credit_required`：同步回测/编译遇到免费额度耗尽，确认后可带 `useCredit=true` 重试。

---

## 2. MCP 工具

传输：stdio，newline-delimited JSON-RPC 2.0。支持 `initialize` / `tools/list` / `tools/call` / `ping`；
服务端可下发 `notifications/message`（任务终结）与 `notifications/progress`
（客户端在 `tools/call` 的 `_meta.progressToken` 中提供 token 时）。

### 2.1 诊断

| 工具 | 参数 | 说明 |
|------|------|------|
| `jq_bridge_status` | — | 插件连接数、账号绑定、最近 hello |
| `jq_health` | — | 一次探测 connected + loggedIn + 账号池摘要 |
| `jq_accounts` | `refresh?` | 账号池快照（uid/状态/cap/在跑/积分/冷却） |

### 2.2 认证

| 工具 | 参数 |
|------|------|
| `jq_auth_status` | `account?` |
| `jq_auth_get_cookie` | `account?`（敏感） |
| `jq_auth_open_login` | `url?`, `account?` |
| `jq_auth_refresh` | `account?` |
| `jq_account_limits` | `account?` |

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
| `jq_backtest_run` | `strategyId`, `start`, `end?`, `capital?`, `frequency?`, `compile?`, `account?` | 登记任务 |
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

### 2.7 任务队列 / 事件回调

| 工具 | 参数 | 说明 |
|------|------|------|
| `jq_batch_submit` | `jobs[]`, `account?`, `placement?`, `maxConcurrent?`, `workers?`, `callbackUrl?`, `callbackSecret?`, `callbackOn?` | 非阻塞入队；job 支持 `strategyKey/name/code/codePath/account`；先用免费额度 |
| `jq_batch_status` | `batchId?`, `limit?` | 快照（含 `countsByAccount`） |
| `jq_batch_wait` | `batchId`, `timeoutSeconds?`, `sinceEventId?`, `includeEvents?` | 阻塞到终结或需要积分确认 |
| `jq_batch_events` | `batchId`, `sinceEventId?`, `waitSeconds?`, `limit?` | 事件游标 / 长轮询 |
| `jq_batch_confirm_credit` | `batchId`, `approve`, `tags?`, `confirm` | 免费额度用完后确认/拒绝使用积分 |

事件类型：`job.started`、`job.done`、`job.failed`、`job.timeout`、`job.needs_credit`、`batch.done`。
`JobState` 额外字段：`account`、`accountUid`、`attempts`、`creditRequired`、
`creditReason`、`creditAccount`；状态新增 `needs_confirmation`。
`jq_batch_wait` 遇到待确认任务会立即返回 `creditRequired:true` 与
`needsConfirmation[]`，不会阻塞到超时。

### 2.8 账号池

- `jq_accounts`：`{default, cap, connected, accounts:[{alias,uid,nickname,state,cap,inFlight,queued,credits,creditsKnown,cooldownUntil,lastError}], clients:[...]}`。
- 账号状态：`pending_login`、`ready`、`busy`、`cooling`、`free_exhausted`（免费额度用完，仅接受已确认的积分任务）、`quota_exhausted`、`logged_out`、`disabled`。
- 只有带 `code`/`codePath` 的 job 才能自动换号；`strategyId` 视为钉住默认账号。
- 提交后掉线不会重跑，避免重复消耗额度：调度器只重试读取，最终失败时带 `remoteId`。
`jq_batch_events` 返回 `{events, nextEventId, done, hasMore}`，把 `nextEventId`
作为下次的 `sinceEventId` 即可增量消费；`waitSeconds>0` 且无新事件时服务端最多阻塞该秒数。

Webhook（`callbackUrl`，`callbackOn=batch|job|both`）：

```http
POST /your-endpoint HTTP/1.1
Content-Type: application/json
X-JQ-Event: batch.done
X-JQ-Event-Id: 12
X-JQ-Batch-Id: batch-20261018-1
X-JQ-Delivery: 9f2c...
X-JQ-Timestamp: 1760000000
X-JQ-Signature: sha256=<HMAC-SHA256(secret, timestamp + "." + body)>
```
Body 为事件对象（`batch.done` 时附 `jobs` 与 `counts`）。失败按指数退避重试 5 次，
单次 10s 超时；投递异步执行，不阻塞调度。

MCP 通知：

```json
{"jsonrpc":"2.0","method":"notifications/message",
 "params":{"level":"info","logger":"jq-mcp","data":{"type":"batch.done","batchId":"..."}}}
{"jsonrpc":"2.0","method":"notifications/progress",
 "params":{"progressToken":"p1","progress":3,"total":10,"message":"3/10 任务已终结"}}
```

### 2.8 工具返回

成功：`{"content":[{"type":"text","text":"<JSON>"}],"isError":false}`
失败：`{"content":[{"type":"text","text":"<错误 JSON>"}],"isError":true}`

### 2.9 日期与数值约定

- 日期：`YYYY-MM-DD`；时间戳：Unix 毫秒。
- 百分比：小数值（`0.1832` = `18.32%`），除非字段名含 `percent`。

---

## 3. 本地 HTTP API

启用：`jq-mcp --http-addr 127.0.0.1:8792`（或配置 `http_addr`）。仅建议监听回环地址；
所有 `/api/*` 需要 token，支持 `Authorization: Bearer <token>`、`X-JQ-Token: <token>`
或 `?token=<token>`。`/healthz` 不需要鉴权。

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/healthz` | `{ok,version,protocol,daemonId,startedAt,connected,cap,lastEventId,instances}` |
| POST | `/api/tools/{name}` | body `{"arguments":{...}}`，同步调用任意 MCP 工具 |
| POST | `/api/rpc` | 原始 JSON-RPC（MCP 一行），供 `jq-mcp proxy` 使用 |
| GET | `/api/notifications` | SSE，推送 `notifications/message` / `notifications/progress` |
| POST | `/mcp` | MCP Streamable HTTP：一个 JSON-RPC 请求 → `application/json` 响应 |
| GET | `/mcp` | MCP Streamable HTTP 的通知流（SSE） |
| POST | `/api/batches` | body `{jobs,maxConcurrent?,callbackUrl?,callbackSecret?,callbackOn?,sessionId?}`，提交批次 |
| GET | `/api/batches?limit=20` | 最近批次 |
| GET | `/api/batches/{id}` | 批次快照（`counts`/`done`/`jobs`） |
| GET | `/api/batches/{id}/events?since=&wait=20&limit=200` | 单批次事件长轮询 |
| GET | `/api/events?since=&wait=20&limit=200` | **跨批次**事件长轮询（jq-loop 插件使用） |

事件响应：

```json
{"events":[{"eventId":12,"daemonId":"default-720874","type":"batch.done",
            "batchId":"...","sessionId":"ses_...","at":"...","jobs":[...]}],
 "nextEventId":12,"hasMore":false}
```

- `eventId` 只在**一个 daemon 进程内**单调；全局唯一游标是 `(daemonId, eventId)`。
  daemon 重启后 `eventId` 归零，客户端应以 `daemonId` 变化作为重置信号。
- `since` 是游标；服务端保留本进程内的事件日志，支持补发。
- `wait` 秒内无新事件时阻塞，`0` 表示立即返回；上限 60s。
- `sessionId` 来自 OpenCode MCP `tools/call._meta["ai.opencode/sessionID"]`，或由
  `X-JQ-Session-Id` 请求头提供（`proxy`/HTTP 通道）。
- 事件类型：`job.started` / `job.done` / `job.failed` / `job.timeout` / `batch.done`。
- `/mcp` 额外要求：`Origin` 为空或 loopback，否则 403；无 token 401。

---

## 4. 兼容与演进

- 桥协议新增字段必须向后兼容；未知方法返回 `usage_error`。
- MCP 工具名稳定；新增能力以新工具形式加入。
- `protocolVersion` 在 `initialize` 中协商；服务端回显客户端版本，缺省 `2024-11-05`。
