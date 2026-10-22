# 账号池开发方案（account pool）

> 状态：**0.4.0 已实现（M1–M3；M4 文档/清理进行中）**  
> 目标版本：`jq-mcp 0.4.0`（`protocol` 保持 2，仅新增字段/工具）  
> 关联：[architecture-v2.md](architecture-v2.md)、[protocol.md](protocol.md)  
> 本文取代架构 v2 中「多账号联邦」这条非目标：账号池仍是**单 daemon**，只是同时接多个浏览器 profile。
>
> 实现落点：`internal/account`（注册表）、`internal/app/pool.go`（身份探测/同步）、
> `internal/sched`（dispatcher、per-account cap、provisioner、failover）、
> `internal/browser/cft.go` + `profile.go`（启动器）、`extension`（alias hello、auth.identity）。

---

## 0. 结论

1. **一个 daemon，多个 Chrome profile**。每个 profile 登录一个聚宽账号、加载一份 jqhelper 扩展，全部连同一个 `ws://127.0.0.1:8790/ws`。
2. **账号身份以聚宽 uid 为准**，由 daemon 主动调用 `auth.identity` 获取并校验，扩展自报的信息只作提示。
3. **调度器按账号发槽位**：每账号独立并发上限、独立收紧、独立冷却；任务可以在账号之间迁移。
4. **本地代码是权威**：云端策略只是回测载体。提交时按目标账号 `ensure` 一份 `[jq-mcp] <key>` 副本，按 `key + code hash` 复用，代码变了就覆盖。
5. **浏览器由启动器管理**：`jq-mcp accounts open` 自动下载 Chrome for Testing、创建 profile、带扩展起窗口，用户只需登录。
6. 不做：Firefox、同一实验铺满所有账号（`spread`）、多 daemon 联邦、自动登录。

---

## 1. 目标与非目标

### 1.1 目标

| # | 目标 | 验收方式 |
|---|---|---|
| G1 | 一条命令拉起 N 个互相隔离的账号窗口 | `jq-mcp accounts add a1 a2 && jq-mcp accounts open --all` 弹出两个窗口，各开登录页 |
| G2 | 多账号并发叠加，单账号不超限 | 提交 6 个任务，两个账号并行；任一时点 `account.inFlight <= account.cap` |
| G3 | 账号故障可迁移 | 手动退出一个账号的登录，剩余任务在另一账号跑完，批次正常 `done` |
| G4 | 额度/积分感知 | 某账号额度耗尽后不再派单，任务迁到有额度的账号 |
| G5 | 单账号旧行为不变 | 只连一个扩展时，现有工具、批次、事件游标、webhook 行为与 0.3.x 一致 |
| G6 | 可观测 | `jq_accounts` 能看到每个账号的状态、cap、在跑数、冷却、错误 |

### 1.2 非目标

- Firefox / Safari 支持（已评估，工程摩擦大于收益）。
- `spread`：同一实验在所有账号各跑一份并合并结果。
- 跨机器或远程浏览器（桥仍然只绑 loopback）。
- 多租户权限隔离（同一 OS 用户内视为可信）。
- 自动登录 / 自动过验证码。

---

## 2. 现状与约束

| 事实 | 位置 | 影响 |
|---|---|---|
| 会话只存在于浏览器，Go 不发聚宽 HTTP | `internal/jq/client.go` 包注释 | 账号 = 浏览器 profile，无法用 Cookie 池替代 |
| 桥可以连多个扩展，但路由是“最新连接优先” | `internal/bridge/hub.go` `pick()` | 多连接会串号，必须改成按账号路由 |
| `hello` 没有账号身份 | `internal/bridge/hub.go` `Hello` | 需要新增 identity 协商 |
| 调度器只有全局并发上限 | `internal/sched/scheduler.go` `maxConcurrent`/`tighten` | 需要改成每账号独立 cap |
| `Job.Priority` 声明了但未使用 | `internal/sched/scheduler.go:41` | 池化后必须实现，否则大批次饿死小批次 |
| `strategyId` / `backtestId` 账号私有 | 平台语义 | 读取类调用必须记住归属账号 |
| 启动器已是单 profile 的完整实现 | `internal/browser/launcher.go` | 多 profile 是自然扩展，不是重写 |
| 扩展靠 `default-config.json` 自动连接 | `extension/background.js`、`extension/offscreen.js` | 可在每个 profile 写入不同 `alias` |
| 官方 Chrome 137+ 移除 `--load-extension` | Chrome 平台策略 | 池子必须用 **Chrome for Testing** |
| 调度器/任务库/事件日志只能 daemon 持有 | `docs/architecture-v2.md` | 池化只改调度器内部，不引入第二个状态源 |

### 2.1 关键推论

- **一个 profile 只能一个账号**：Chrome 的 Cookie 是 profile 级的，容器能力只有 Firefox 有。因此“N 个账号”物理上就是“N 个 profile”。
- **云端策略可以丢**：既然本地代码是权威，任务就不必绑定某个账号的 `strategyId`，调度器可以自由选号，这是整个池化成立的前提。
- **窗口生命周期与 daemon 解耦**：daemon 由 systemd 常驻，浏览器窗口由 `accounts open` 拉起；daemon 重启不应该杀掉用户正在登录的窗口。

---

## 3. 总体架构

```
┌─────────────┐   ┌─────────────┐   ┌─────────────┐
│ profile a1  │   │ profile a2  │   │ profile a3  │
│ Chrome+CfT  │   │ Chrome+CfT  │   │ Chrome+CfT  │
│ jqhelper    │   │ jqhelper    │   │ jqhelper    │
└──────┬──────┘   └──────┬──────┘   └──────┬──────┘
       │ ws + token       │                  │
       └──────────┬───────┴──────────────────┘
                  ▼
        ┌──────────────────────────────────────────┐
        │              jq-mcp daemon               │
        │                                          │
        │  Bridge Hub ── 按 uid 路由 ──┐           │
        │                              ▼           │
        │                     Account Registry     │
        │                     (uid/alias/health)   │
        │                              │           │
        │  MCP/HTTP ── Scheduler ──────┤           │
        │              │  每账号槽位    │           │
        │              ▼               ▼           │
        │         Provisioner      jq.Client(a1)   │
        │         (代码→云端副本)   jq.Client(a2)   │
        │              │               │           │
        │              └──── Task/Event/Webhook ───┘
        └──────────────────────────────────────────┘
```

职责边界：

- **Hub**：连接管理、账号绑定、按 uid 找连接。
- **Registry**：账号身份与配置（alias、uid、enabled、权重、cap 覆盖）。
- **Scheduler**：队列、槽位、放置、重试/迁移、每账号运行态（inFlight/cap/cooldown）。
- **Provisioner**：本地代码 → 目标账号策略副本 → `strategyId`。
- **jq.Client**：无状态转发，新增“指定账号”入口。

> 运行态（inFlight/cap/cooldown）只存在 Scheduler 内存里，由 `s.mu` 保护；Registry 只存持久身份与配置，避免两把锁互相等待。

---

## 4. 数据模型

### 4.1 配置（`config.json` 新增 `pool`）

```json
{
  "pool": {
    "default_cap": 2,
    "max_failovers": 2,
    "default_placement": "auto",
    "browser": "chrome-for-testing",
    "channel": "stable",
    "headless": false,
    "ua_mode": "random",
    "ua_platforms": ["windows", "linux", "android", "macos", "ios"],
    "ua_browsers": ["chrome", "chromium", "firefox", "safari"]
  }
}
```

说明：

- `defaultCap` 是冷启动猜测值；平台返回“最多 N 个”后以平台为准。
- `headless` 默认 `false`；首次登录必须可见。
- 池窗口**不随 daemon 退出而关闭**（与现有 `launcher.Stop()` 行为不同）。

### 4.2 账号注册表（`~/.config/jq-mcp/accounts.json`）

独立文件，不放进 `config.json`：daemon 启动时会 `cfg.Save()` 重写配置，运行期可变数据放一起容易被覆盖。

```json
{
  "version": 1,
  "default": "a1",
  "unregistered": "reject",
  "accounts": [
    {
      "alias": "a1",
      "uid": "1000123",
      "nickname": "research-main",
      "enabled": true,
      "weight": 1,
      "cap": null,
      "profileDir": "profiles/a1",
      "tags": []
    },
    {
      "alias": "a2",
      "uid": "1000456",
      "nickname": "research-alt",
      "enabled": true,
      "weight": 1,
      "cap": 1,
      "profileDir": "profiles/a2",
      "tags": ["no-credit"]
    }
  ]
}
```

- `unregistered`：`reject`（默认，未知 uid 不参与调度）或 `accept`（自动建条目，别名 `acct-<uid 后 4 位>`）。
- `cap: null` 表示用 `pool.defaultCap`；数字表示硬覆盖。
- `weight` 影响轮询频率，默认 1。

### 4.3 策略副本缓存（每账号一份）

`~/.config/jq-mcp/accounts/<alias>/strategies.json`：

```json
{
  "version": 1,
  "items": {
    "momentum-v3": {
      "strategyId": "abc123",
      "hash": "sha256:9f2c...",
      "name": "[jq-mcp] momentum-v3-a1b2c3",
      "updatedAt": "2026-10-22T10:00:00+08:00"
    }
  }
}
```

- 只记录 daemon 自己创建的副本；用户自己的策略永远不覆盖。
- 写入用临时文件 + `rename`。
- 缓存丢失（删文件/换机器）只会导致重新创建一份，不影响正确性。

### 4.4 运行态模型

```go
// internal/account
type Account struct {
    Alias, UID, Nickname string
    Enabled  bool
    Weight   int
    Cap      int    // 0 = 用默认
    Profile  string
    Tags     []string
}

// internal/sched（运行态，不持久化）
type accountState struct {
    alias       string
    uid         string
    state       string // pending_login|ready|busy|cooling|quota_exhausted|logged_out|disabled
    cap         int
    inFlight    int
    cooldownTo  time.Time
    credits     int
    creditsOK   bool
    lastError   string
    lastUsedAt  time.Time
}
```

对外视图（`jq_accounts`）：

```json
{
  "default": "a1",
  "accounts": [
    {
      "alias": "a1", "uid": "1000123", "nickname": "research-main",
      "state": "ready", "cap": 3, "inFlight": 1, "queue": 2,
      "credits": 17, "creditsKnown": true,
      "cooldownSeconds": 0, "lastError": "",
      "connectedAt": "2026-10-22T09:00:00+08:00", "version": "0.4.0"
    }
  ]
}
```

### 4.5 账号状态机

```
                 ┌──────────────┐
  连接建立 ─────▶ │ pending_login│ ◀──── 掉登录
                 └──────┬───────┘
                   identity 成功
                        ▼
   disabled ◀────── ┌────────┐ ──────▶ cooling ──┐
                    │ ready  │ ◀── 冷却结束 ─────┘
                    └───┬────┘
                 有任务 │ ▲ 任务结束
                        ▼ │
                     ┌──────┐
                     │ busy │
                     └──────┘
                        │ 额度耗尽
                        ▼
                 quota_exhausted ── 重置时间到 ──▶ ready
```

---

## 5. 启动器设计

### 5.1 Chrome for Testing 下载

- 清单：`https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json`
- 取 `channels.Stable` 的版本与对应平台包：
  - `linux64`、`mac-x64`、`mac-arm64`、`win64`、`win32`
- 安装目录：`~/.config/jq-mcp/browsers/chrome-for-testing/<version>/`
- 流程：下载到 `*.tmp-<pid>` → `archive/zip` 解压到临时目录 → 执行 `--version` 校验 → 原子 `rename` → 写 `.ready` 标记。
- 失败重试 3 次，指数退避；尊重 `HTTPS_PROXY`（`http.ProxyFromEnvironment`）。
- 已存在 `.ready` 直接复用；版本不匹配时重新下载，但不删旧版本（回滚方便）。
- 手动路径：`--chrome-path` 或环境变量 `JQ_MCP_CHROME` 优先；离线环境走这条。
- 说明：CfT 是官方自动化构建，保留 `--load-extension`；**不承诺兼容 Chrome 稳定版**。
  已验证版本：`156.0.8078.47`（linux64，headless 与 headed 均可，3 profile 并发连接正常）。

### 5.2 Profile 布局

```
~/.config/jq-mcp/profiles/a1/          # 0700
  chrome/                              # user-data-dir
  extension/                           # 扩展副本
    default-config.json                # {wsUrl, token, alias, daemonProfile}
    .jq-ext-version                    # 源扩展版本戳
  profile.json                         # alias/createdAt/lastOpenedAt
```

- 每个 profile 一份扩展副本，是为了让扩展在 `hello` 里自报 alias；扩展 ID 不同不影响功能。
- 扩展副本在以下情况刷新：源 manifest/version 变化。刷新时只替换 `extension/`，保留 `chrome/` 和 `profile.json`。
- 权限：目录 `0700`、文件 `0600`（里面有登录态，等同凭据）。

### 5.3 启动流程

```
accounts open a1
  ├─ ensureBrowser()            # CfT 已就绪？
  ├─ ensureProfile(a1)          # 目录、扩展副本、bootstrap
  ├─ checkRunning(a1)           # pidfile / SingletonLock → 已开则跳过
  ├─ spawn(chrome, --user-data-dir, --load-extension, URL)
  │     URL = 首次 ? 登录页 : 聚宽首页
  └─ waitHello(a1, 15s)         # 扩展连上则打印状态；否则提示手动检查
```

- 进程以 detached 方式启动，daemon 不接管生命周期。
- 记录 pid 到 `profiles/<alias>/profile.json`；进程已退出则清理。
- 同一 `user-data-dir` 被占用时，Chrome 会自行拒绝；启动器提前检查并给出可读提示。
- `accounts close a1` 才主动结束进程（默认 `SIGTERM`，3 秒后 `SIGKILL`）。

### 5.4 CLI

```
jq-mcp accounts add a1 a2 [--cap 2]
jq-mcp accounts open [a1 ... | --all]
jq-mcp accounts close [a1 ... | --all]
jq-mcp accounts status
jq-mcp accounts rm a1 [--purge-profile]
jq-mcp accounts doctor          # 检查二进制、profile、扩展、连接
```

- `add` 只写注册表与目录，不启动。
- `open` 幂等；已在运行的窗口跳过。
- `status` 在 daemon 不在线时仍能显示 profile/pid 层面状态（读文件），在线时合并账号运行态。

### 5.5 失败模式

| 现象 | 处理 |
|---|---|
| 下载失败/网络受限 | 报错并提示 `--chrome-path`；不阻塞 daemon |
| 无图形环境 | 检测 `DISPLAY`/`WAYLAND_DISPLAY`（Linux），提示或建议 `--headless` |
| profile 被锁 | 提示“窗口已开或上次异常退出”，附 pid 与 lock 路径 |
| 扩展没连上 | 15 秒超时报错，提示检查 `default-config.json` 与扩展是否被禁用 |
| 二进制版本过旧 | `doctor` 检查 `--version` 与清单，建议 `--refresh` |

---

## 6. 账号身份与注册

### 6.1 `auth.identity`（新增 page op）

2026-10-22 实测：`/user/login/state` 的 `data` 已经包含 `userId`、`uniqueKey`、`alias`、
`userName`、`vipType`、`level`，只是 `auth.status` 目前仅透出 `loggedIn`。所以
`auth.identity` 不需要新接口，只做归一化：

- `uid` 取 `data.userId`（稳定哈希，不是手机号）；`uniqueKey` 作为辅助校验。
- `nickname` 取 `data.alias`；`vip` 取 `vipType != ""`；`level` 原样带回。
- 积分：`GET /credits/task/getUsableAmount` → `{"data":{"amount":0},...}`；失败则
  `creditsKnown:false`，不阻塞连接。
- `GET /credits/index/getMessage` 返回积分变动消息（无消息时 `data:false`），后续可用于提示，
  不参与调度。

```json
{
  "uid": "<userId>",
  "uniqueKey": "<uniqueKey>",
  "nickname": "<alias>",
  "vip": false,
  "level": 0,
  "credits": 0,
  "creditsKnown": true,
  "checkedAt": "2026-10-22T15:00:00+08:00",
  "source": "login_state"
}
```

注意：

- 不要用 `userName`（手机号）或 `alias`（可改）做主键。
- 未登录时 `userId` 缺失，保持 `pending_login`。
- 不把完整原始响应写日志（含手机号/邮箱）；只记 uid 与昵称。

### 6.2 `hello` 扩展

```json
{
  "jsonrpc": "2.0",
  "method": "hello",
  "params": {
    "client": "jqhelper",
    "version": "0.4.0",
    "token": "...",
    "browser": "chrome",
    "alias": "a1",
    "daemonProfile": "default"
  }
}
```

- `alias` 来自该 profile 的 `default-config.json`，由 `background.js` 读取并透传给 offscreen。
- **alias 仅用于展示/对账**；账号身份仍以 `auth.identity` 的 uid 为准。

### 6.3 绑定与去重

1. 连接认证通过 → 状态 `pending_login`。
2. daemon 调用 `auth.identity`（10s 超时）：
   - 成功 → 按 uid 查注册表。
     - 已知 uid → 绑定 alias，状态 `ready`。
     - 未知 uid → 依据 `unregistered` 拒绝或自动建条目。
     - alias 已绑定其他 uid → **不覆盖**，报冲突，要求 `accounts rebind`。
   - 失败 → 保持 `pending_login`，每 15s 重试，5 分钟后退到 60s。
3. 同一 uid 出现第二条连接 → 保留最新，旧连接标 `stale` 并记录告警；调度只认最新。
4. 连接断开 → 状态 `logged_out`（除非进程仍在、只是 WS 抖动；以连接状态为准）。

### 6.4 登录检测

- 扩展在 `tabs.onUpdated` 检测到聚宽登录页离开时，主动触发一次 `event: auth.changed`。
- daemon 收到后立即重试 `auth.identity`，成功即绑定；不依赖用户手动刷新。
- 如果扩展没有上报（例如登录在别的标签完成），定时探测兜底。

---

## 7. 调度器设计

### 7.1 从“先取任务”改成“先有槽位”

现有 `process()` 是 worker 取到任务后 `acquire()` 等全局槽位；池化后如果继续这样，worker 会抱着一个“只能跑在 a1 的任务”等 a1，而 a2 空转。

改成 **dispatcher + 执行 worker**：

```
dispatcher（单 goroutine，持 s.mu）:
  for {
    waitCond()                       // 队列非空 且 至少一个账号有空槽
    job  := pickJobByPriority()
    acct := place(job)               // 找不到就继续等
    job.account = acct; acct.inFlight++
    dispatch <- {job, acct}
  }

worker（N 个）:
  for { j := <-dispatch; run(j); release(j.acct) }
```

优点：

- 不会被“暂时无法放置”的任务卡住其他账号。
- 槽位与任务的匹配是原子的，不会超发。
- 优先级、年龄、账号亲和都在 dispatcher 一处实现。

`cond.Broadcast()` 触发点：入队、任务结束、cap 变化、账号状态变化、冷却结束。

### 7.2 放置（placement）

| 模式 | 行为 |
|---|---|
| `pinned` | 只用 `job.account`；该账号不可用则等待/失败，绝不换号 |
| `auto`（默认，仅限带 code 的任务） | 在所有可用账号中按评分选 |
| `sticky` | 优先上次执行该 `strategyKey`/`backtestId` 的账号；不可用则按 `auto`（若允许） |

评分（降序）：

1. 剩余槽位 `cap - inFlight` 多者优先；
2. `weight` 大者优先（加权轮询）；
3. `lastUsedAt` 早者优先（防偏斜）。

过滤条件：`enabled && state==ready && inFlight<cap && now>cooldownTo`；`useCredit` 任务额外要求 `!creditsKnown || credits>0`。

### 7.3 每账号并发上限

- 生效 cap = `min(账号覆盖, 平台学习值, 全局安全上限)`。
- **没有可靠的权益接口**：旧 `/view/vip`、`/user/vip/index` 等路径已 404，`account.limits`
  抓不到并行上限。因此 cap 初值取 `pool.defaultCap`（默认 2），首次被拒后按错误学习。
- 平台返回 `maxConcurrent=N` → 只收紧当前账号；学习值保存在进程内（重启后回到配置值），
  24h 内不会自动抬高。
- 成功后不自动抬高 cap（避免反复撞墙）；只有 `accounts refresh` 或账号 reconnect 才重新探测。
- 全局 worker 数 = `min(config.workers, Σ 有效 cap)`，只作为资源上限，不再承担业务限流。

### 7.4 优先级与公平性

- `Job.Priority` 升序（小=先跑），同优先级 FIFO。
- 队列用切片 + 插入排序即可（批量通常 < 1000，不需要堆）。
- 防饥饿：等待超过 10 分钟的普通任务临时提升一级（实现上按 `enqueuedAt` 加权）。

### 7.5 任务生命周期

```
pending → waiting_account → provisioning → submitting → polling → done
                │                │             │           │
                │                │             │           └→ failed/timeout
                │                │             └→ failed（不可重试）
                │                └→ failed / (failover) → waiting_account
                └→ failed（超时无可用账号）
```

- `waiting_account` 不占用槽位；超过 `jobSubmitTimeout`（默认 10 分钟）仍无可用账号则失败，错误里列出各账号不可用原因。
- 提交成功后账号掉线：**不立即重跑**。远端回测可能还在跑，盲目换号会重复消耗额度。策略是：
  - 保持 `polling`，用退避重试读取（最长 30 分钟）；
  - 仍不可用则 `failed`，错误里带 `remoteId` 与所属账号，用户可稍后手动取结果；
  - 只有“提交前”的失败才允许迁移。
- 提交超时（结果未知）→ 先 `backtest.list` 对账，确认没有记录再迁移，避免重复提交。

### 7.6 批次与事件

- 一个批次可以跨多个账号；`JobState` 增加：

```go
Account   string `json:"account,omitempty"`   // alias
AccountUID string `json:"accountUid,omitempty"`
Attempts  int    `json:"attempts,omitempty"`  // 迁移/重试次数
```

- `batch.done` 事件保留 `jobs` 全量快照；额外提供 `countsByAccount`。
- webhook 头新增 `X-JQ-Account`（job 事件）；`batch.done` 看 body 里的 jobs。
- MCP 通知、事件游标、`includeEvents` 均无需改协议，只是多字段。

### 7.7 与旧参数的兼容

- `jq_batch_submit.maxConcurrent`：保留。
  - 批次指定 `account` → 覆盖该账号 cap；
  - `auto` → 覆盖所有参与账号的默认 cap；
  - 单账号场景与旧行为完全一致。
- 响应里的 `cap`：返回 **有效总并发**（Σ 各账号 cap）；新增 `accountCaps` 明细。单账号时数值不变。

---

## 8. 策略落地（provisioning）

### 8.1 输入解析

优先级：`code` > `codePath` > `strategyId`。

```
job.code       直接使用
job.codePath   读取本地文件（≤ 2 MiB，读失败即任务失败）
job.strategyId 走 pinned 模式，不 ensure、不迁移
```

`strategyKey` 缺省时依次回退到 `name`、`sha256(code)[:12]`。

### 8.2 ensure 流程

```
ensure(account, key, code):
  h := sha256(code)
  lock(key, account)                       # 进程内 keyed mutex
  if cached[key].hash == h:
      return cached[key].strategyId
  if cached[key] exists:
      try strategy.save(id, code)
      on not_found: delete cache, fall through
      else: update hash, return id
  id := strategy.create(name="[jq-mcp] "+key+"-"+hash6, code)
  cache[key] = {id, h, name}
  persist atomically
  unlock
  return id
```

- **singleflight**：并发任务落到同一账号同一 key 时只创建一次。
- **命名**：固定前缀 `[jq-mcp] `；超过 40 字符截断，并附 key 的 6 位 hash 后缀，避免不同 key 撞名。
- **不覆盖用户策略**：只操作缓存里记录的 id；同名但不在缓存的策略视为用户策略，改用带后缀的新名字。
- **失效处理**：`strategy.get` 返回 not_found 时清缓存重建一次；仍失败则任务失败。
- **清理**（P2）：`jq_accounts prune --cloud` 删除缓存中已不再需要的 `[jq-mcp]` 策略；默认不自动删。

### 8.3 与任务流程的衔接

- 放置到账号后、提交前执行 `ensure`；耗时计入 `provisioning` 状态。
- `ensure` 失败：
  - 网络/超时 → 重试 1 次，再失败可迁移；
  - 代码非法/权限 → 直接失败，不迁移。
- 迁移到新账号时重新 `ensure`（缓存通常 miss，会创建一份）。

---

## 9. 容灾与额度

### 9.1 错误分类

| 错误 | 判定 | 处理 |
|---|---|---|
| `not_authenticated` / `no_extension`（提交前） | 账号掉登录/连接断 | 账号置 `logged_out`，任务迁移（有 code）或失败（pinned） |
| `not_authenticated` / `no_extension`（提交后） | 远端可能仍在跑 | 保持 polling + 退避，30 分钟后 failed，带 `remoteId` |
| `api_error` + `details.maxConcurrent=N` | 平台限流 | 收紧该账号 cap，任务回队重试 |
| 额度/积分不足（文案匹配 + 详情字段） | 账号额度耗尽 | 置 `quota_exhausted`（估算重置时间），任务迁移 |
| `network_error` / `timeout`（提交前） | 传输失败 | 账号短冷却，原账号重试 3 次后迁移 |
| 提交超时（结果未知） | 幂等风险 | 先 `backtest.list` 对账，再决定迁移 |
| 编译错误 / 运行错误日志 | 代码问题 | 直接 failed，不迁移 |
| `usage_error` / `not_found` | 参数/策略问题 | 直接 failed |

迁移预算：`pool.maxFailovers`（默认 2）。pinned 任务永远不迁移。

### 9.2 额度与积分

- 连接时通过 `auth.identity` 读取 `credits/creditsKnown`（数据源：
  `GET /credits/task/getUsableAmount`）。
- `useCredit` 任务优先派给 `credits>0` 的账号；全部未知时按普通任务处理。
- 额度错误后：
  - 若响应里带重置时间则使用；
  - 否则默认 `now+24h`，并允许 `jq_accounts refresh` 提前解除。
- `tags` 可用于人工标记（如 `no-credit`），放置时跳过。

### 9.3 冷却与恢复

- 连续 `api_error`/网络错误：30s → 2m → 5m → 15m（上限）。
- 成功一次即清零。
- `logged_out` 不做指数冷却，靠重连/重新登录恢复。
- `accounts refresh` 强制重探 identity 与 cap（慎用，会打扰正在跑的任务？探测本身是只读，允许）。

---

## 10. 读取类调用的路由

问题：`jq_backtest_result/logs/export` 等工具只给 `backtestId`，不知道该找哪个账号。

方案：

1. `task.Manager` 的 `Task` 增加 `Account`、`AccountUID`，随快照持久化。
2. `Scheduler` 的 `JobState` 同时记录账号。
3. 新增解析顺序：
   ```
   resolveOwner(backtestId):
     explicit account? -> 校验归属，不匹配则报错
     tasks[backtestId].Account
     batches[*].Jobs[remoteId].Account
     else -> 报错并提示显式传 account
   ```
4. 读取调用（`jq_backtest_result` / `logs` / `export` / `wait`）默认走解析出的账号。
5. `jq_backtest_list` 只列 default 账号；需要合并时显式传 `account:"all"`（P2，先不做）。

---

## 11. 对外接口

### 11.1 新工具 `jq_accounts`

```json
// 输入
{ "refresh": false }
```

输出见 §4.4。字段稳定后写入 `docs/protocol.md` 与 skill 文档。

### 11.2 变更工具

| 工具 | 新增参数 | 说明 |
|---|---|---|
| `jq_accounts` | — | 新增 |
| `jq_batch_submit` | `account`, `placement`, `failover`；job: `strategyKey/name/code/codePath/account` | 池化核心入口 |
| `jq_backtest_run` | `account`, `placement`, `strategyKey/code/codePath` | 单次回测也可池化 |
| `jq_strategy_compile` | 同上 | 编译同样需要落地 |
| `jq_backtest_result` / `jq_backtest_logs` / `jq_backtest_export` | `account`（可选） | 缺省自动解析归属 |
| `jq_backtest_cancel/delete` | `account`（可选） | 用归属解析 |
| `jq_strategy_list/get/save/create/...` | `account`（可选） | 缺省 default |
| `jq_account_limits` | `account`（可选） | 逐账号刷新 cap |
| `jq_health` / `jq_bridge_status` | — | 增加 accounts 摘要 |

所有参数可选；不传时行为与 0.3.x 一致。

### 11.3 Job schema

```json
{
  "tag": "v1",
  "strategyKey": "momentum-v3",
  "name": "momentum-v3",
  "codePath": "./strategies/momentum.py",
  "start": "2023-01-01",
  "end": "2023-12-31",
  "capital": 1000000,
  "frequency": "day",
  "useCredit": false,
  "priority": 0,
  "account": null
}
```

`strategyId` 仍可用，但一旦出现即视为 pinned。

### 11.4 事件与 webhook

- `JobState` 增加 `account` / `accountUid` / `attempts`。
- `batch.done` 增加 `countsByAccount`。
- webhook 新增请求头 `X-JQ-Account`；body 结构不变（多字段）。
- MCP `notifications/message` 的 `data` 增加 `account`。
- 事件游标语义不变。

### 11.5 版本与协议

- `Version` → `0.4.0`。
- `Protocol` 保持 2：只新增工具/字段，不改变既有语义；`/healthz` 增加 `accounts` 数组。
- `maxConcurrent` 语义变化写进 README 与 protocol 文档。

---

## 12. 任务拆解

### M1 启动器（3 人日）

| 任务 | 文件 | 完成定义 |
|---|---|---|
| CfT 清单解析与下载 | `internal/browser/download.go`（新） | Linux/macOS/Windows 路径正确，校验 `--version`，断网报错清晰 |
| 多 profile 启动 | `internal/browser/profile.go`（新）、`launcher.go`（重构） | `accounts open` 起窗口、写 bootstrap、刷新扩展副本 |
| CLI 子命令 | `cmd/jq-mcp/main.go`、`internal/app/accounts.go`（新） | `add/open/close/status/rm/doctor` 可用 |
| 注册表读写 | `internal/account/registry.go`（新） | 原子写、0700、并发安全 |
| 扩展 hello 带 alias | `extension/background.js`、`offscreen.js`、`lib/ws-client.js` | 两个 profile 的 hello 能区分 |
| **DoD** | — | 两个窗口、两个 uid、`jq_accounts` 可见；daemon 重启窗口不退出 |

### M2 身份与调度（4 人日）

| 任务 | 文件 | 完成定义 |
|---|---|---|
| `auth.identity` | `extension/page/jq-page.js`、`handlers/auth.js` | 复用 `/user/login/state` + `/credits/task/getUsableAmount`，返回 uid/昵称/积分 |
| Hub 按账号路由 | `internal/bridge/hub.go`、`client.go` | `CallAccount`；同 uid 去重；默认账号 |
| 账号运行态 | `internal/sched/accounts.go`（新） | 状态机、cap、cooldown、credits |
| dispatcher + 每账号槽位 | `internal/sched/scheduler.go` | 不再有全局 `acquire`；优先级生效 |
| placement | `internal/sched/placement.go`（新） | pinned/auto/sticky；单账号回归不变 |
| 策略落地 | `internal/sched/provision.go`（新） | 并发只建一份；hash 变化覆盖 |
| 工具接入 | `internal/app/batch_tools.go`、`tools.go` | `jq_accounts`、新参数、事件带 account |
| **DoD** | — | 6 任务分两号并行，单号不超 cap；旧单号流程通过回归 |

### M3 容灾与额度（3 人日）

| 任务 | 文件 | 完成定义 |
|---|---|---|
| 错误分类与迁移 | `internal/sched/failover.go`（新） | 提交前迁移、提交后不盲跑；对账逻辑 |
| 额度/冷却 | `scheduler.go`、`accounts.go` | quota 标记、重置时间、指数冷却 |
| 读取归属路由 | `internal/task/manager.go`、`tools.go` | result/logs/export 自动找号 |
| 观测 | `internal/app/tools.go`、`internal/httpapi/server.go` | `jq_accounts` 字段完整；`/healthz` 汇总 |
| **DoD** | — | 手动掉登录后任务迁移跑完；额度耗尽不再派单 |

### M4 收尾（2 人日）

- `accounts prune`、策略副本清理（可选）。
- README / protocol / architecture / skill 文档更新。
- 全量回归、压测、发布 0.4.0。

总计约 **12 人日**（不含评审与真实账号联调的不确定性）。

---

## 13. 测试计划

### 13.1 单元测试

- 下载器：平台映射、版本解析、原子安装、失败重试（用 `httptest`）。
- 注册表：uid 冲突、alias 冲突、并发读写、原子落盘。
- placement：优先级、权重、冷却过滤、pinned 不迁移、单账号退化。
- 槽位：并发下 `inFlight <= cap`，cap 收紧后不超发。
- provisioning：并发 ensure 只 create 一次；hash 变化触发 save；not_found 重建。
- failover：错误矩阵逐项断言；提交后掉线不重跑。
- 路由：`backtestId → account`，显式冲突报错。

### 13.2 集成测试（假扩展）

用 Go 写一个假扩展 WS 客户端：响应 `hello`、`auth.identity`、`strategy.*`、`backtest.*`，可脚本化延迟/失败/掉线。

场景：

1. 两个账号连接，提交 4 任务 → 各 2 个。
2. 账号 A cap=1，B cap=2 → 分布 1/2，剩余排队。
3. A 返回 `maxConcurrent=1` → 只收紧 A。
4. A 提交前掉线 → 任务全迁 B，最终 done。
5. A 提交后掉线 → 不重复提交，超时 failed 且带 remoteId。
6. 同 uid 两条连接 → 旧连接 stale，cap 不重复计算。
7. 单账号回归：现有 `scheduler_test.go`、`events_test.go` 全绿。

### 13.3 手工验收（真实账号）

- 两个聚宽账号分别登录，确认 `jq_accounts` 的 uid/昵称正确。
- 同策略代码分别落到两个账号，确认云端只各有一份 `[jq-mcp]` 副本。
- 修改本地代码再跑，确认副本被覆盖而不是新增。
- 退出一个账号登录，确认任务迁移、结果完整。
- 重启 daemon，确认窗口保留、账号自动重连。

---

## 14. 风险与缓解

| 风险 | 影响 | 缓解 |
|---|---|---|
| CfT 未来移除 `--load-extension` | 启动器失效 | 抽象 `LoadExtension` 策略；备选 CDP `Extensions.loadUnpacked`（需 `--enable-unsafe-extension-debugging`）。156 已验证可用 |
| 聚宽风控（同 IP 多账号） | 账号被封/限流 | 控制并发节奏、避免同策略狂刷、文档明确风险；不做绕过 |
| N 个 Chrome 内存占用 | 机器压力 | 默认 cap 保守；可选 `--headless`；文档给资源估算 |
| profile 含登录态 | 凭据泄露 | 0700/0600；文档说明；不上传、不打包 |
| 积分数据源不稳定 | 额度感知失效 | 先做“报错后标记”，积分仅作提示 |
| 同 uid 重复连接 | cap 重复计算 | 连接级去重，保留最新，旧连接 stale |
| 策略副本膨胀 | 云端库变脏 | 固定前缀 + 每账号每 key 一份 + `prune` |
| 迁移导致重复回测 | 浪费额度 | 提交前才迁移；提交超时先对账；提交后只重试读取 |
| `config.Save()` 覆盖运行数据 | 账号信息丢失 | 账号数据独立 `accounts.json` |

---

## 15. 发布与迁移

- 无账号配置时：行为与 0.3.x 完全一致。
- 首次升级：`accounts.json` 不存在 → 若只有一个连接，自动建 `default` 条目并绑定 uid。
- `jq-mcp accounts add` 可在 daemon 不在线时执行（只写文件）。
- 回滚：删除 `accounts.json` 与新工具调用即可回到单账号模式；已有批次/事件不受影响。
- 文档更新：README「账号池」章节、`docs/protocol.md` 工具表、`docs/architecture.md` 数据流、skill `references/tools.md`。

---

## 16. 验证结果与待验证项（2026-10-22）

### 已验证

1. **CfT 156.0.8078.47（linux64）可用**：`--headless=new` 下 `--load-extension` 正常，
   同时起 3 个 profile，全部连到同一个 daemon（日志 `clients=3`）；headed 模式
   （`DISPLAY=:0`）也能启动并连接。系统自带的 Chrome 154 正式版不能作为底座。
2. **uid 来源确定**：`/user/login/state` 返回 `userId` / `uniqueKey` / `alias`，
   `auth.identity` 无需新增接口，只做归一化。
3. **积分来源确定**：`GET /credits/task/getUsableAmount` 稳定返回 `data.amount`
   （实测当前账号为 0）。
4. **旧会员页已失效**：`/view/vip`、`/user/vip/index`、`/default/index/vip` 等路径均
   302/404 到 404 页，`account.limits` 的 VIP 抓取拿不到任何并行上限；cap 初值只能
   用配置默认 + 错误学习。
5. **未登录 profile 的 hello 仍会连上**，只是 `loggedIn=false`；身份必须由 daemon
   连接后主动探测，不能依赖 hello。

### 仍待验证

1. 平台并发上限是否有可靠接口（当前结论：没有，靠“最多 N 个”错误学习）。
2. 提交后掉线的远端可读性：确认 `backtest.stats` 是否严格要求同一账号 Cookie（预期是）。
3. macOS Gatekeeper 对 CfT 的拦截与 `xattr` 处理。
4. Windows 下 CfT 解压后的目录权限与杀毒软件误报。

---

## 17. UA 伪装（0.4.0）

- 每个 profile 首次 `accounts open` 时随机生成 UA 并写入 `profile.json`，
  之后每次启动复用；`accounts ua <alias> random` 重随机，`off` 关闭。
- 平台/浏览器矩阵只包含合理组合（Safari 仅 macOS/iOS）；Android+Safari 会回退到
  iOS/macOS Safari，而不是生成不存在的指纹。
- Chrome 端用 `--user-agent` 注入，同时 `--disable-features=UserAgentClientHint`
  避免 `sec-ch-ua` 与伪装 UA 矛盾。
- 配置：`pool.ua_mode`（`random|off`）、`pool.ua_platforms`、`pool.ua_browsers`。
- 注意：UA 只是表层指纹；TLS/JS 引擎仍是 Chrome。Safari/移动 UA 可能让聚宽返回
  不同页面，必要时把 `ua_mode` 设为 `off` 或限定 `ua_browsers`。

## 18. 免费额度优先与积分确认（0.4.0）

- 所有提交（包括 `useCredit:true` 的预授权任务）都先以 `useCredit=false` 尝试；
  确保优先消耗免费额度。
- 免费额度用完：
  - `useCredit:false` → job 进入 `needs_confirmation`，账号标记 `free_exhausted`
    （24h 冷却），`jq_batch_wait` 立即返回 `creditRequired:true` 和
    `needsConfirmation[]`。
  - `useCredit:true` → 视为已授权，标记 free_exhausted 后自动带积分重试一次；
    若积分也不足，标记 `quota_exhausted` 并迁移/失败。
- 确认接口：`jq_batch_confirm_credit {batchId, approve, tags?, confirm}`。
  `approve:true` 重新入队（优先原账号；无积分容量时直接报错），
  `approve:false` 将待确认任务置为 failed。
- 同步工具（`jq_backtest_run` / `jq_strategy_compile`）遇到免费额度错误返回
  `credit_required`，调用方确认后带 `useCredit:true` 重试。

---

## 附录 A：Go 接口草案

```go
// internal/account
type Registry interface {
    List() []Account
    Get(alias string) (Account, bool)
    Default() (Account, bool)
    ByUID(uid string) (Account, bool)
    Bind(alias, uid, nickname string) error
}

// internal/sched
type Router interface {
    // Acquire 为 job 选一个账号并占槽；返回的 Lease 负责释放与错误上报。
    Acquire(ctx context.Context, job *JobState, needCredit bool) (Lease, error)
}

type Lease interface {
    Account() account.Account
    Release()
    Tighten(n int)        // 平台返回“最多 N 个”
    Fail(err error)       // 分类：cooldown / quota / logged_out / permanent
}
```

Import 方向：`app → sched → account → jq → bridge`，`account` 不反向依赖 `sched`，避免循环。

## 附录 B：`default-config.json`

```json
{
  "wsUrl": "ws://127.0.0.1:8790/ws",
  "token": "<daemon token>",
  "alias": "a1",
  "daemonProfile": "default"
}
```

`extension/background.js` 读取后通过 `get-config` 传给 offscreen；`hello` 携带 `alias`。
