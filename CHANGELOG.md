# 更新日志

本项目版本号见 `internal/app/app.go` 中的 `Version`。

## 0.3.0

### 新增

- **本地 HTTP API**：`--http-addr 127.0.0.1:8791`（或配置 `http_addr`）。提供
  `POST /api/batches`、`GET /api/batches[/{id}]`、`GET /api/batches/{id}/events`、
  **跨批次** `GET /api/events?since=&wait=`、`POST /api/tools/{name}`，所有 `/api/*`
  需 token。让常驻消费者与 MCP stdio 共用同一个 jq-mcp 进程，不再抢 8790。
- **OpenCode V2 插件 `plugins/jq-loop`**：订阅 HTTP 事件流，批次结束后用
  `ctx.session.synthetic(delivery="queue", resume=true)` 把失败/指标摘要送回原会话，
  实现“跑完 → 唤醒模型 → 修错/调参 → 再提交”的闭环。默认关闭，`/jq-loop on` 开启；
  支持 `wakeOn=batch|failed|job`、迭代上限、冷却、`dryRun`。
- **mjs client attach 模式**：`startClient({url:"http://127.0.0.1:8791"})` 通过 HTTP
  提交批次/读取事件/代理工具，可与 OpenCode 的 MCP server 共存。
- **会话透传**：MCP `tools/call._meta["ai.opencode/sessionID"]` 会写入批次的
  `sessionId`，并随事件下发（webhook / HTTP / 通知），便于插件路由。
- 调度器新增跨批次事件读取（`AllEvents` / `NextAllEvents` / `LastEventID`）。

### 修复

- `internal/sched` 事件日志现在通过 `AllEvents` 跨批次合并读取，游标在进程内单调；
  HTTP `events` 空结果统一返回 `[]` 而不是 `null`，便于插件/脚本直接迭代。

## 0.2.1

### 新增

- **回测错误日志快速失败**：`jq_backtest_wait` 与调度器在轮询回测指标的同时
  轮询错误日志；一旦出现错误立即判定为 `failed` 并返回/下发 `errorLogs`，
  不再等到 `timeoutSeconds` / `jobTimeout`。
- 调度器任务失败时会立刻产生 `job.failed`（批次内全部终结时再产生
  `batch.done`），webhook / MCP 通知即时送达；任务登记新增 `errorLogs` 字段。

### 修复

- **调度器编译任务不再空等到超时**：编译运行不产出指标，改为轮询聚宽编译列表
  判定终态（优先按 `id`/`listId` 精确匹配，必要时宽限期兜底），远端
  `failed` / `cancelled` 映射为任务失败。
- `jq_strategy_compile` 的编译状态检查同样先按本次运行的 `id`/`listId` 匹配，
  减少把历史编译记录误判为本次结果的情况。
- `jq_backtest_wait` 通过任务登记识别编译任务（`mode=compile`），改为轮询编译
  列表判定完成，不再对 `jq_backtest_run{compile=true}` 等到超时；等待结束时会把
  `done` / `failed`（含 `errorLogs`）回写到 `jq_task_get`。

## 0.2.0

### 新增

- **批次完成回调（webhook）**：`jq_batch_submit` 支持 `callbackUrl` / `callbackSecret` / `callbackOn`，
  批次或任务终结时异步 POST 事件，支持 HMAC-SHA256 签名、指数退避重试，回调失败不影响调度。
- **批次事件流**：新增 `jq_batch_events`，按 `sinceEventId` 增量读取事件，支持 `waitSeconds` 长轮询。
- **事件驱动的等待**：`jq_batch_wait` 新增 `sinceEventId` / `includeEvents`，无新事件时阻塞并有心跳。
- **MCP 主动推送**：服务端支持 `notifications/message`（批次终结）与 `notifications/progress`（等待进度）。

### 改进

- 调度器新增生命周期事件：`job.started` / `job.done` / `job.failed` / `job.timeout` / `batch.done`。
- `jq_batch_submit` 改用状态快照返回，避免与工作线程并发读写冲突。

### 修复

- 修复调度器 `Metrics` / `RemoteID` / `TaskID` 在锁外写入的竞争。
- 修复 MCP 响应与通知并发写入可能交错的问题。

## 更早（0.2.0 之前，未单独编号）

- **服务端批量调度**：`jq_batch_submit` / `jq_batch_status` / `jq_batch_wait`，固定工作线程消费共享队列、并发顶满、被平台拒绝时自动收紧并退避重试。
- **稳定性**：聚宽页签回收复用，修复页签泄漏导致的机器内存压力。
- **性能**：编译轮询加速、回测 ID 缓存；支持积分续跑（`useCredit`）。
- **初始版本**：登录态检测、策略读写与编译、回测提交/查询/导出、策略批量回测与多区间对比。
