# 更新日志

本项目版本号见 `internal/app/app.go` 中的 `Version`。

## 0.4.1

### 修复

- **可用积分取错接口**：`/credits/task/getUsableAmount` 返回的是"任务可用额度"（通常 0），
  改为 `/user/account/getUserInfo` 的 `data.amount`（页面"可用积分"）。
  例：RuenChen 实际 1011 分，旧代码显示 0，导致误判 `quota_exhausted`。
- **回测创建重试失控**：`BacktestRun` 重试 240 次（5s 间隔≈20 分钟）→ **6 次**；
  单次调用 **90s** 超时；整个任务 **15 分钟**上限。卡住的任务不再无限空转。
- **日提交次数上限未识别**：新增 `isDailyCountLimit`（"太多次数/提交次数/次数回测"）；
  命中后立即失败并把账号冷却到**次日 00:00**，**不再 failover/迁移**，
  避免把其他账号一起拖入风控（实测：一个账号撞线会连带烧穿其余账号）。

### 变更

- `maxFailovers` 默认 **2 → 1**。

### 其他

- 扩展 `page/jq-page.js` VERSION **0.4.0 → 0.4.1**。
- 单测覆盖 `isDailyCountLimit`。

## 0.4.0

### 账号池

- **一条命令拉起多个账号窗口**：`jq-mcp accounts add a1 a2` + `accounts open --all`。
  自动下载并固定 Chrome for Testing（官方构建仍支持 `--load-extension`），
  每个账号一个独立浏览器 profile，人工登录后自动识别。
- **账号身份以聚宽 `uniqueKey` 为准**：扩展新增 `auth.identity`，daemon 在连接后主动探测，
  不再信任 hello 自报信息；同一 uid 的重复连接只保留最新。`userId` 是会话级字段，
  扩展重载后会变化，不能做账号主键。
- **调度器按账号发槽位**：每账号独立 cap、独立收紧、独立冷却；新增 dispatcher，
  先匹配账号再派给 worker，避免被某个忙碌账号阻塞。
- **本地代码是权威**：job 可带 `strategyKey` / `code` / `codePath`，调度器按目标账号
  自动创建或更新 `[jq-mcp] <name>-<hash>` 副本，`strategyKey + code hash` 复用，不污染用户策略。
- **容灾迁移**：提交前的未登录、额度耗尽、网络错误可迁移到其他账号（最多
  `pool.max_failovers` 次）；提交后的掉线只重试读取，不重复提交。
- **积分感知**：通过 `GET /credits/task/getUsableAmount` 读取可用积分，
  `useCredit` 任务优先派给有积分的账号。
- **免费额度优先 + 积分确认**：所有回测先以 `useCredit=false` 提交；免费额度用完时
  job 进入 `needs_confirmation` 并暂停，`jq_batch_wait` 立即返回待确认任务；
  `jq_batch_confirm_credit {approve:true, confirm:true}` 后才用积分重试，
  `approve:false` 则失败。显式 `useCredit:true` 视为预授权（仍先试免费）。
- **UA 伪装**：每个浏览器 profile 首次启动随机选择 UA（Windows/Linux/Android/
  macOS/iOS × Chrome/Chromium/Firefox/Safari）并持久化，`accounts ua` 可重随机/关闭；
  Chrome 端同时关闭 UA Client Hints，避免与伪装 UA 矛盾。Safari 只使用 macOS/iOS
  等合理组合，不会生成 Android+Safari 这种不存在的指纹。
- **jq-loop**：多账号批次的结果摘要带 `acct=<alias>`；新增 `job.needs_credit` 唤醒，
  免费额度用完时立即提醒会话确认积分（不受 `wakeOn` 限制）。
- **新增 `jq_accounts`**：查看 uid、昵称、状态、cap、在跑数、排队数、积分、冷却与错误。
- **CLI**：`jq-mcp accounts add|open|close|status|rm|default|cap|doctor`。
- `jq_batch_submit` 支持 `account` / `placement`，job 支持 `strategyKey` / `name` /
  `code` / `codePath` / `account`；批次结果与事件新增 `account` / `accountUid` /
  `attempts` / `countsByAccount`，webhook 新增 `X-JQ-Account`。
- 单账号行为保持兼容：现有 `strategyId` 任务仍走默认账号，工具参数全部可选。

### 验证

- Chrome for Testing 156.0.8078.47：headless 与 headed 均可同时加载 3 个 profile。
- 两账号端到端：4 个任务 2/2 分配，单账号 cap=1 不被突破；模拟 a1 掉登录后任务
  全部迁移到 a2 完成。
- `auth.identity` 复用 `/user/index/isLogin` 的 `uniqueKey`（`userId` 会随扩展重载变化）；旧会员页已 404，
  并发上限改为人工配置 + 平台报错自动收紧。
- UA：Chrome `--user-agent` 实测能改变 `navigator.userAgent`；配合
  `--disable-features=UserAgentClientHint` 后 `navigator.userAgentData` 为空。
- 免费额度：调度器单测覆盖“暂停 → 确认 → 积分重试”和“拒绝 → failed”两条路径。

## 0.3.0

### 架构 v2：daemon / proxy 拆分

- **`jq-mcp serve` 常驻 daemon**：它是唯一的浏览器桥、调度器、任务库与事件日志持有者。
  同一 profile 只允许一个实例：`flock` + descriptor（`~/.config/jq-mcp/instances/<profile>.json`）
  防止第二个进程启动，并让客户端自动发现地址，不再依赖硬编码端口。
- **`jq-mcp proxy`**：无状态 stdio MCP 适配器，把所有 JSON-RPC 转发到 daemon 的
  `POST /api/rpc`，通知通过 `GET /api/notifications`（SSE）回传。OpenCode 每个 workspace
  起一个 proxy 也不会再抢 8790。
- **`/mcp`（Streamable HTTP）**：`POST /mcp` 处理请求，`GET /mcp` 推送通知；支持
  `Mcp-Session-Id`、`Origin` 校验、Bearer token。客户端可直接 `type:"remote"`，
  完全不 spawn 进程。
- **`/api/rpc` / `/api/notifications`**：给 proxy 与脚本用的原始 JSON-RPC 通道。
- **`jq-mcp status` / `jq-mcp stop`**：读取 descriptor 并探活 / 优雅停止。
- **`make install-service`**：安装 `systemd --user` 单元，`Restart=always`。
- **事件增加 `daemonId`**：`(daemonId, eventId)` 才是全局唯一游标；daemon 重启后
  客户端据此重置游标。
- **`X-JQ-Session-Id` 透传**：HTTP 通道也能把 OpenCode 的 sessionId 带到 `tools/call`。

### jq-loop：per-server 单例

- 插件状态改为进程级（`globalThis`）：OpenCode 每个 workspace 都会 `setup()`，但同一
  server 进程内只有**一条长轮询、一个游标、一次唤醒**。
- 游标按 `daemonId` 对齐；daemon 重启时自动重置。
- poller 所属 workspace 关闭时自动迁移到存活实例。
- 事件处理失败不再推进游标（此前会静默丢事件）。
- `/jq-loop on|off|status` 语义为 **server 级**。

### 新增

- **本地 HTTP API**：`--http-addr 127.0.0.1:8792`（或配置 `http_addr`）。提供
  `POST /api/batches`、`GET /api/batches[/{id}]`、`GET /api/batches/{id}/events`、
  **跨批次** `GET /api/events?since=&wait=`、`POST /api/tools/{name}`，所有 `/api/*`
  需 token。让常驻消费者与 MCP stdio 共用同一个 jq-mcp 进程，不再抢 8790。
- **OpenCode V2 插件 `plugins/jq-loop`**：订阅 HTTP 事件流，批次结束后用
  `ctx.session.synthetic(delivery="queue", resume=true)` 把失败/指标摘要送回原会话，
  实现“跑完 → 唤醒模型 → 修错/调参 → 再提交”的闭环。默认关闭，`/jq-loop on` 开启；
  支持 `wakeOn=batch|failed|job`、迭代上限、冷却、`dryRun`。
- **mjs client attach 模式**：`startClient({url:"http://127.0.0.1:8792"})` 通过 HTTP
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
