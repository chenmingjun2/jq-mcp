# jq-mcp

**让 AI Agent 安全地操作你的聚宽（JoinQuant）账号。** 由两个组件构成：

- **jqhelper**（`extension/`）：Chrome/Edge Manifest V3 浏览器插件，持有你的登录态，在已登录的聚宽页面里执行操作。
- **jq-mcp**（`cmd/`、`internal/`）：Go 实现的 MCP 服务器，把「策略管理、编译校验、回测、回测数据读取」等能力以 MCP 工具暴露给 AI Agent。

> **核心原则：jq-mcp 从不直接请求聚宽的私有接口。** 它只通过本机 WebSocket 把指令下发给已登录的浏览器插件，由插件在页面上下文里完成真实操作。

```text
AI Agent ──MCP(stdio)──▶ jq-mcp (Go) ──WebSocket(本机+token)──▶ jqhelper(插件) ──DOM/同源fetch──▶ joinquant.com
```

**定位**：面向 AI Agent 的 MCP 执行层；任何 MCP 客户端（AI、脚本、CLI）都可调用。它**不是**行情数据源、**不是**托管平台、**不**做自动交易决策。

---

## 目录

- [快速开始](#快速开始)
- [运行模式](#运行模式)
- [工具一览](#工具一览)
- [典型工作流](#典型工作流)
- [账号并发限制](#账号并发限制)
- [安全模型](#安全模型)
- [已知限制与风险](#已知限制与风险)
- [目录结构](#目录结构)
- [开发与测试](#开发与测试)
- [License](#license)

---

## 快速开始

### 1. 构建 jq-mcp

```bash
make build          # 产出 bin/jq-mcp
bin/jq-mcp --version
```

要求：Go 1.24+。

### 2. 安装浏览器插件 jqhelper

1. 打开 `chrome://extensions`，开启右上角 **开发者模式**。
2. **加载已解压的扩展程序** → 选择本仓库的 `extension/` 目录。
3. 在浏览器里登录 `https://www.joinquant.com`（jq-mcp **不接触**你的账号密码）。

### 3. 配置地址与 token

首次运行 `bin/jq-mcp` 会在 `~/.config/jq-mcp/config.json` 生成随机 token，并打印到 stderr。

在插件 **选项页**（`chrome://extensions` → jqhelper → 详情 → 扩展程序选项）填入：

- WebSocket 地址：`ws://127.0.0.1:8790/ws`
- 访问 token：与 `config.json` 一致
- 点 **保存并重连**

> 也可用 `--token <token>` 与 `--ws-addr <host:port>` 覆盖。
>
> 需要插件/脚本共享同一个 jq-mcp 时，再加 `--http-addr 127.0.0.1:8792`：
> 提供批次提交、跨批次事件长轮询与工具代理（见 `docs/protocol.md` §3）。

### 4. 接入 MCP 客户端

先起 daemon（见下节「运行模式」），再让客户端连上来。以 OpenCode 为例，项目级 `opencode.json`：

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "servers": {
      "jq-mcp-proxy": {
        "type": "local",
        "command": ["/absolute/path/to/bin/jq-mcp", "proxy"]
      }
    }
  }
}
```

或直连 Streamable HTTP（不 spawn 进程）：

```json
"jq-mcp-proxy": {
  "type": "remote",
  "url": "http://127.0.0.1:8792/mcp",
  "headers": { "Authorization": "Bearer YOUR_TOKEN" }
}
```

（`opencode.json` 含 token，勿提交；本仓库已将其加入 `.gitignore`。）

### 5. 验证

在客户端调用 `jq_health`，应返回 `{"connected": true, "loggedIn": true}`。

---

## 运行模式

进程分成「一个常驻 daemon + 若干薄客户端」：

| 命令 | 行为 | 适用 |
|------|------|------|
| `jq-mcp serve` | **常驻 daemon**：浏览器桥、调度器、任务库、事件日志、HTTP API、`/mcp` | 日常（systemd 托管） |
| `jq-mcp proxy` | 无状态 stdio MCP 适配器，转发到 daemon | 不支持远程 MCP 的客户端 |
| `jq-mcp`（无参数） | 兼容模式：daemon + stdio 同进程 | 旧配置、单次调试 |
| `jq-mcp status` / `jq-mcp stop` | 查看 / 优雅停止某个 profile | 运维 |

同一台机器、同一个浏览器 profile **只允许一个 `serve`**：第二个进程会打印
“已在运行 (pid=…)” 并以 0 退出，不会再去抢 8790。

### 常驻部署（推荐）

```bash
make build && make install-service
systemctl --user enable --now jq-mcp-server
# 需要“退出登录也常驻”时：
# loginctl enable-linger $USER

jq-mcp status          # descriptor + /healthz
jq-mcp stop            # 优雅停止
```

### 客户端配置

远程 MCP（推荐：客户端不再 spawn 任何进程）：

```json
{
  "mcp": {
    "servers": {
      "jq-mcp-proxy": {
        "type": "remote",
        "url": "http://127.0.0.1:8792/mcp",
        "headers": { "Authorization": "Bearer <token>" }
      }
    }
  }
}
```

或 stdio 代理（兼容所有 MCP 客户端）：

```json
{
  "mcp": {
    "servers": {
      "jq-mcp-proxy": {
        "type": "local",
        "command": ["/path/to/jq-mcp/bin/jq-mcp", "proxy"]
      }
    }
  }
}
```

> `proxy` 从 `~/.config/jq-mcp/instances/<profile>.json` 读取 daemon 地址；
> daemon 未运行时给出明确的启动提示，而不是 `bind: address already in use`。

### 浏览器模式

| 模式 | 行为 | 适用 |
|------|------|------|
| `attach`（默认） | jq-mcp 只提供桥服务，用户自己打开浏览器、加载插件并登录 | 日常使用、已有登录态 |
| `launch` | jq-mcp 启动 Chrome/Chromium 并加载 `extension/` | 无头/隔离环境 |

```bash
bin/jq-mcp                                    # 兼容模式 + attach
bin/jq-mcp serve --browser-mode launch --headless=false   # 首次登录用有头
bin/jq-mcp serve --browser-mode launch --headless         # 之后可用无头
```

`launch` 相关参数：`--chrome-path`、`--user-data-dir`、`--extension-dir`。

> ⚠️ **绝不要使用 `--disable-extensions-except`。** 它会禁用该 profile 中除 jqhelper 外的所有扩展，
> 重启后可能把用户手动安装的插件“挤掉”。`launch` 只用 `--load-extension` 加载，不影响其它扩展。

### 账号池（多账号并发）

一个 daemon 可以同时接多个浏览器 profile，每个 profile 登录一个聚宽账号。
浏览器统一用 Chrome for Testing（官方 Chrome 137+ 已移除 `--load-extension`），
首次执行 `accounts open` 会自动下载并固定版本。

```bash
bin/jq-mcp accounts add a1 a2          # 建 profile（--cap 可指定单账号并发）
bin/jq-mcp accounts open --all         # 弹窗口；首次打开登录页，人工登录
bin/jq-mcp accounts status             # 查看 profile / pid
bin/jq-mcp accounts cap a1 3           # 修改单账号并发上限（0=默认）
bin/jq-mcp accounts ua a1 random       # 重随机 UA；off 关闭；也可传自定义 UA
bin/jq-mcp accounts close --all
```

登录后扩展会自动上报 `auth.identity`，daemon 以聚宽稳定的 `uniqueKey` 绑定账号；
`jq_accounts` 可查看每个账号的状态、cap、在跑数、积分与冷却。

**调度语义**：

- `strategyId` 是账号私有的。只传 `strategyId` 的任务钉在默认账号（旧行为）。
- 传 `strategyKey` + `code` / `codePath` 的任务是可迁移的：调度器自动选择账号，
  在该账号创建/更新 `[jq-mcp] <name>-<hash>` 副本（按代码 hash 复用），失败了可换号重跑。
- 显式 `account` 表示钉住该账号；`placement` 支持 `auto` / `pinned` / `sticky`。
- 每账号独立 cap，平台返回“最多 N 个”只收紧该账号；额度耗尽会标记 24 小时并迁移任务。
- **UA 伪装**：每个 profile 第一次启动时从 Windows/Linux/Android/macOS/iOS ×
  Chrome/Chromium/Firefox/Safari 中随机选一个 UA，持久化后每次启动复用；
  `accounts ua <alias> random|off|<UA>` 可重随机或关闭。范围可在
  `pool.ua_mode / pool.ua_platforms / pool.ua_browsers` 配置。
- **免费额度优先**：调度器始终先用 `useCredit=false` 提交。免费额度用完时，
  任务进入 `needs_confirmation` 并暂停；确认后调用
  `jq_batch_confirm_credit {approve:true, confirm:true}` 才会消耗积分重试。
  提前设置 `useCredit:true` 视为已授权，会先试免费、失败后自动用积分。
- 提交后账号掉线**不会**自动重跑（远端可能还在跑），只重试读取并带 `remoteId` 失败。

`accounts.json` 只保存别名/uid/上限，不保存 Cookie；Cookie 始终留在浏览器 profile 里。

完整设计、状态机与验证记录见 [docs/account-pool.md](docs/account-pool.md)。

---

## 工具一览

共 **41** 个工具。✅ 只读，✍️ 写操作（带 `confirm` 门禁）。

### 诊断
| 工具 | 说明 |
|------|------|
| ✅ `jq_bridge_status` | 插件连接数、版本、账号绑定 |
| ✅ `jq_health` | 一次探测 `connected` + `loggedIn` + 账号池摘要 |
| ✅ `jq_accounts` | 账号池：uid/昵称/状态/cap/在跑/排队/积分/冷却 |

### 认证 / 账号
| 工具 | 说明 |
|------|------|
| ✅ `jq_auth_status` | 聚宽登录状态（可指定 `account`） |
| ✅ `jq_auth_get_cookie` | 读取 Cookie（敏感） |
| ✅ `jq_auth_open_login` | 打开登录页供手动登录 |
| ✅ `jq_auth_refresh` | 刷新登录态 |
| ✅ `jq_account_limits` | 账号身份、积分与会员信息（并发上限需人工配置） |

### 策略
| 工具 | 说明 |
|------|------|
| ✅ `jq_strategy_list` | 策略 + 文件夹（`fid/all/limit/sort`） |
| ✅ `jq_strategy_find` | 按名称查找 |
| ✅ `jq_strategy_get` | 详情（`includeCode` 带源码） |
| ✍️ `jq_strategy_create` | 新建（`code/type/folderId` 可选） |
| ✍️ `jq_strategy_save` | 保存名称/源码（`confirm`） |
| ✍️ `jq_strategy_clone` | 复制策略（`confirm`） |
| ✍️ `jq_strategy_delete` | 删除策略（`confirm`） |

### 文件夹
| 工具 | 说明 |
|------|------|
| ✅ `jq_folder_list` | 文件夹列表（`recursive`） |
| ✍️ `jq_folder_create` | 新建文件夹 |
| ✍️ `jq_strategy_move` | 移动策略到文件夹（`confirm`） |

### 回测执行 / 编译
| 工具 | 说明 |
|------|------|
| ✅ `jq_strategy_compile` | 触发“编译运行”，返回编译错误/日志（正式回测前校验） |
| ✍️ `jq_backtest_run` | 发起回测（`compile=true` 为编译运行） |
| ✅ `jq_backtest_list` | 回测记录 |
| ✅ `jq_backtest_wait` | 轮询到终态；有错误日志立即 `failed`；编译任务按编译列表判定 |
| ✍️ `jq_backtest_cancel` | 取消运行中的回测（`confirm`） |
| ✍️ `jq_backtest_delete` | 删除记录（`confirm`，`compile` 区分） |
| ✍️ `jq_backtest_batch` | 批量提交（同策略多区间/多资金） |
| ✅ `jq_backtest_compare` | 多次回测指标对比 |

### 任务队列 / 批处理（服务端调度）
| 工具 | 说明 |
|------|------|
| ✍️ `jq_batch_submit` | 提交任务队列；支持 `account`/`placement`/`code`/`codePath`/回调 |
| ✅ `jq_batch_status` | 批次进度与结果（非阻塞） |
| ✅ `jq_batch_wait` | 一次阻塞到批次终结，无需客户端 sleep 轮询 |
| ✅ `jq_batch_events` | 按 `sinceEventId` 增量读事件，可长轮询 |
| ✍️ `jq_batch_confirm_credit` | 免费额度用完时确认使用积分（或拒绝） |

### 回测数据
| 工具 | 说明 |
|------|------|
| ✅ `jq_backtest_get` | 详情 + 指标 + 源码 |
| ✅ `jq_backtest_stats` | 收益/风险指标 |
| ✅ `jq_backtest_result` | 收益曲线（`offset` 分页） |
| ✅ `jq_backtest_logs` | 运行/错误日志 |
| ✅ `jq_backtest_trades` | 成交明细 |
| ✅ `jq_backtest_positions` | 每日持仓明细 |
| ✅ `jq_backtest_monthly` | 分周期/月度收益 |
| ✍️ `jq_backtest_export` | 导出到本地 JSON |

### 本地代码 / 文件
| 工具 | 说明 |
|------|------|
| ✅ `jq_code_standardize` | 向 `initialize` 注入滑点/手续费 |
| ✅ `jq_strategy_export` | 远端策略源码导出到本地 |
| ✍️ `jq_strategy_import` | 本地文件导入到远端（`confirm`） |
| ✅ `jq_file_read` | 读本地文本文件 |
| ✍️ `jq_file_write` | 写本地文件（`confirm`） |

### 任务登记
| 工具 | 说明 |
|------|------|
| ✅ `jq_task_list` | jq-mcp 记录的回测任务 |
| ✅ `jq_task_get` | 单个任务 |

---

## 典型工作流

### 只读巡检
```
jq_health → jq_strategy_list → jq_backtest_list → jq_backtest_stats
```

### 新建 / 修改策略并回测
```
jq_strategy_find(name) → jq_strategy_get(includeCode) → 修改源码
→ jq_strategy_compile(strategyId)             # 先编译校验，失败即返回错误
→ jq_backtest_run(strategyId, start, end)     # 返回 taskId/id/listId
→ jq_backtest_wait(backtestId)                # 或稍后 jq_backtest_stats
→ jq_backtest_trades / positions / monthly / logs
```

### 消融 / 参数扫描（Agent 编排）
对每个变体：`jq_strategy_create`（带变体代码）→ `jq_strategy_compile` → `jq_backtest_run` → 收集 `stats`。
最后 `jq_strategy_delete(confirm)` 清理。受**账号并发上限**约束（见下）。

脚本化大批量扫描可以用 Node 客户端 `clients/mjs/jq-mcp-client.mjs`：单进程常驻、
`jq_batch_submit` 投递、`jq_batch_events` 游标实时收结果，指标随 `job.done` 一起返回，
不需要每任务轮询。用法与基准见 `clients/mjs/README.md`。

### 小步快跑（自动迭代）
`plugins/jq-loop` 是 OpenCode V2 插件：订阅 jq-mcp 的 HTTP 事件流，在任务完成时（默认 stream）用
`ctx.session.synthetic` 把结果摘要送回原会话，模型可立刻进入下一轮修复/调参。
默认关闭，用 `/jq-loop on` 开启；需要 jq-mcp 以 `--http-addr` 启动。
说明见 `plugins/jq-loop/README.md`。

### 云端策略导出到本地
```
jq_strategy_export(strategyId, path="./x.py")      # format=py|json
jq_backtest_export(backtestId, path="./bt.json")   # sections=stats,result,trades,...
```

---

## 账号并发限制

聚宽限制「并行编译/回测数量最多 N 个」（N 由账号会员等级决定）。本项目的处理：

- 提交被拒时，错误 `details.maxConcurrent` 会带上 N（从服务端文案解析）。
- 每账号并发上限用 `jq-mcp accounts cap <alias> <N>` 配置；`jq_account_limits`
  返回账号身份、积分与会员信息（平台已不再提供可靠的并行数）。
- 服务端调度器（`jq_batch_*`）按账号发槽位，把各账号并发顶满，
  并在被平台拒绝时只收紧该账号、退避重试；客户端只负责声明任务。

---

## 任务完成回调

`jq_batch_submit` 可选 `callbackUrl`（http/https）、`callbackSecret`、`callbackOn`
（`batch` 默认 / `job` / `both`）。批次或任务终结时，服务端异步 POST 事件：

```json
{
  "eventId": 12,
  "type": "batch.done",
  "batchId": "batch-20261018-1",
  "at": "2026-10-18T12:00:00+08:00",
  "jobs": [{"tag": "v1", "status": "done", "...": "..."}],
  "counts": {"done": 1}
}
```

- 事件类型：`job.started`、`job.done`、`job.failed`、`job.timeout`、`job.needs_credit`、`batch.done`。
- **错误日志快速失败**：调度器轮询正式回测时会同时读取错误日志；一旦出现错误，
  立即把任务置为 `failed`（附 `error` 与 `errorLogs`），并立刻发出
  `job.failed` / `batch.done` 事件触发 webhook，而不是等到 `jobTimeout`。
  编译任务（`compile=true`）同样适用。
- **编译任务按编译列表判定完成**：编译运行不产出回测指标，调度器改为轮询
  聚宽编译列表（buildList），优先用提交时返回的 `id` / `listId` 精确匹配该行；
  若平台返回的 id 对不上，则在 10s 宽限期后用“没有运行中 + 已有终态”兜底，
  不会再空等到 `jobTimeout`。远端状态 `failed` / `cancelled` 会映射为任务失败。
- 请求头：`X-JQ-Event`、`X-JQ-Event-Id`、`X-JQ-Batch-Id`、`X-JQ-Delivery`、
  `X-JQ-Timestamp`；设置 `callbackSecret` 后另有
  `X-JQ-Signature: sha256=HMAC_SHA256(secret, timestamp + "." + body)`。
- 失败按 1s/2s/4s/8s 退避重试共 5 次，单次 10s 超时；回调不会阻塞调度线程。
- 同时服务端会向 MCP 客户端推送 `notifications/message`（level/logger/data），
  并支持 `notifications/progress`。

对 agent 而言，**不要 sleep 后轮询**：提交后直接调用 `jq_batch_wait`
一次阻塞到终结；若客户端会截断长调用，用 `sinceEventId` + `jq_batch_events`
（`waitSeconds` 长轮询）续读，事件游标保证不丢结果。MCP 没有服务端主动唤醒
模型的机制，webhook 面向外部脚本/服务，`jq_batch_wait` 才是 agent 侧的正确用法。

---

## 安全模型

1. **仅本机**：WebSocket 绑定 `127.0.0.1`。
2. **共享 token**：随机生成，写入 `~/.config/jq-mcp/config.json`（`0600`）。
3. **最小权限插件**：host 权限仅 `*://*.joinquant.com/*`；另需 `tabs/scripting/cookies/storage/alarms`。
4. **密码不落库**：登录完全由用户在浏览器完成；jq-mcp 不接触账号密码。
5. **破坏性操作**：删除/覆盖类工具需 `confirm=true`。
6. **敏感输出**：Cookie、源码等仅在显式请求时返回，日志脱敏。

---

## 已知限制与风险

- **依赖站点私有接口**：路径与解析在 `extension/page/jq-page.js` 集中维护，站点改版需跟进。
- **依赖浏览器在线**：不是后台服务；插件断开时工具返回 `no_extension`。
- **账号权益约束**：并行数、积分、频率。
- **合规**：仅限用户本人账号、研究用途；不绕过登录与风控。
- **回测口径**：日期输入 `YYYY-MM-DD`；结束日不得晚于上一交易日。

---

## 目录结构

```text
jq-mcp/
├── cmd/jq-mcp/                 # 入口
├── internal/
│   ├── apierr/  bridge/  browser/  config/  httpapi/  jq/  mcp/  task/  app/
├── extension/                  # jqhelper（MV3，无需构建）
│   ├── manifest.json  background.js
│   ├── lib/  handlers/  page/  options.html
├── clients/mjs/                # Node ESM 编译/回测客户端（spawn/attach）
├── plugins/jq-loop/            # OpenCode V2 自动迭代插件
├── .opencode/skills/jq-mcp/    # OpenCode skill（辅助 AI 使用）
├── docs/{architecture,protocol}.md
└── Makefile
```

---

## 开发与测试

```bash
make build     # 编译
make test      # go test ./...
make vet       # go vet ./...
make package   # 打包插件 zip 到 dist/
```

研究产物（脚本、回测结果）建议放在 `.local/`（已在 `.gitignore`）。

---

## License

MIT. See [LICENSE](LICENSE).
