# 架构 v2（已实现）：常驻 daemon + 薄适配器

> 状态：**P0 / P1 / P2 已实现**（`jq-mcp 0.3.0`，`protocol=2`）。
> 实现记录见 §13。关联：[architecture.md](architecture.md)（v1 背景）、[protocol.md](protocol.md)。
>
> 设计要点：**取消单例选举**，改为「daemon 常驻 + flock 互斥 + descriptor 发现」；
> jq-loop 为 **per-server 单例**。

---

## 0. 结论先行

1. **daemon 常驻**（systemd user service），客户端只探活，不负责拉起。
2. **不做选举**。只保留两件小事：`flock` 防重复启动、descriptor 供客户端发现端口/版本。
3. **状态全部下沉 daemon**：浏览器桥、调度器、任务库、事件日志只有一个持有者。
4. **MCP 变成适配器**：P1 用 `jq-mcp proxy`（stdio）；P2 增加 `/mcp`（Streamable HTTP），
   客户端可直接 `type:"remote"`，彻底不再 spawn 进程。
5. **jq-loop 改成 per-server 单例**：一个 OpenCode server 内只有一份状态/一条轮询/一个游标。

---

## 1. 问题陈述（证据）

### 1.1 端口争抢

`internal/app/app.go:65` 在 8790 上 bind 失败即退出：

```
mcp connect failed server=jq-mcp
  error="... 监听 127.0.0.1:8790 失败: bind: address already in use"
  directory=/home/chenmingjun/project/jq-mcp-research
```

根因：OpenCode 对每个 workspace 各 spawn 一个 stdio MCP 子进程，而三个项目的
`opencode.json` 都指向同一对端口。**先 bind 者赢，其余会话整场没有 `jq_*` 工具。**

### 1.2 jq-loop 是 per-workspace

同一个 `opencode serve`、同一次启动：

```
07:10:19.309  workspace booted: cc-item
07:10:19.355  workspace booted: jq-mcp-research
07:10:20.261  workspace booted: pos-agent
07:10:19.365  loading plugin .../jq-loop/index.ts
07:10:19.516  loading plugin .../jq-loop/index.ts
07:10:20.270  loading plugin .../jq-loop/index.ts
```

3 个 workspace → 3 次 `setup()` → 3 份 `state`。插件存储又是全局键：

```sql
select key from kv;
plugin:006a0071002d006c006f006f70:iterations   -- "jq-loop"，无 workspace 维度
```

后果：两个实例同时 on 时，同一事件被唤醒两次；两个实例写同一个 `cursor` 键，后者覆盖前者。

---

## 2. 目标与非目标

### 目标

| # | 目标 | 验收方式 |
|---|---|---|
| G1 | 一个浏览器 profile 只有一个浏览器桥/调度器/事件日志 | `ss -ltnp \| grep 8790` 恒为 1 |
| G2 | 任意多 OpenCode 实例/workspace 同时可用 | 所有会话 `jq_*` 工具齐全 |
| G3 | jq-loop per-server 单例 | `ss -tnp \| grep 8792` 恒为 1 条长轮询 |
| G4 | daemon 重启不中断客户端会话 | kill -9 后 proxy 自动重连 |
| G5 | 向后兼容 | `jq-mcp` 无参数行为不变 |

### 非目标（本期）

- 多账号 / 多浏览器 profile 联邦（descriptor 已按 profile 分键，留扩展点）。
- 跨机器部署（仅 loopback）。
- 事件持久化到数据库（P3 可选 JSONL）。
- 多租户隔离（同 OS 用户信任边界内）。

---

## 3. 运行模型

### 3.1 进程角色

| 进程 | 是否常驻 | 状态 | 职责 |
|---|---|---|---|
| `jq-mcp serve` | ✅ | 全部 | bridge + jq domain + scheduler + tasks + event log + HTTP(+`/mcp`) |
| `jq-mcp proxy` | 每客户端一个 | 无 | stdio MCP ↔ daemon HTTP |
| `jq-mcp`（无参数） | 兼容模式 | 全部 | daemon + stdio 同进程（= 当前行为） |
| jq-loop | 随 OpenCode | 少量本地 | 消费事件 → `session.synthetic` |
| drivers / curl | — | 无 | 直连 HTTP |

**硬约束**：调度器、任务库、事件日志只允许 daemon 持有；proxy 不缓存业务状态。

### 3.2 为什么不选举

先分清三件事：

| 能力 | 解决什么 | 本方案 |
|---|---|---|
| 生命周期 | 谁保证进程活着 | systemd `--user`（`Restart=always`） |
| 发现 | 客户端怎么知道端口/版本 | descriptor 文件 + `/healthz` |
| 互斥 | 防止第二个实例启动 | `flock` + 启动前探活 |

选举只在「没有 supervisor、又要零配置、由第一个客户端拉起」时才需要。本项目默认走
systemd，故**不做选举**；仅保留 `--auto-start` 作为可选便携模式（非默认路径）。

### 3.3 启动与探活

```
jq-mcp serve:
  拿 flock(<profile>.lock)
    ├─ descriptor 存在且 /healthz 认证通过 → 打印 "already running pid=…" 并以 0 退出
    └─ 否则 → 写 descriptor → bind 8790/8792 → 释放锁

jq-mcp proxy:
  读 descriptor → 探 /healthz
    ├─ 健康 → 进入转发模式
    └─ 不健康 → 报错：
       "jq-mcp daemon 未运行（profile=work）。执行：systemctl --user start jq-mcp"
       （--auto-start 时才尝试拉起，失败同样报错）
```

> 旧进程未退、端口未释放的情况：新进程 bind 失败 → 退出并提示检查 `pid`，绝不 kill。
> systemd 的 `Restart=always` + `RestartSec=2` 负责兜底。

### 3.4 systemd 单元（示例）

```ini
# ~/.config/systemd/user/jq-mcp.service
[Unit]
Description=jq-mcp daemon (JoinQuant bridge)
After=network.target

[Service]
ExecStart=%h/project/jq-mcp/bin/jq-mcp serve
Restart=always
RestartSec=2
Environment=JQ_MCP_PROFILE=work

[Install]
WantedBy=default.target
```

- 需要「退出登录也常驻」时：`loginctl enable-linger $USER`（本机当前 `Linger=no`）。
- 日志走 journal：`journalctl --user -u jq-mcp -f`。

---

## 4. 发现与配置

```
~/.config/jq-mcp/
├─ config.json                 # 现有配置：ws_addr / http_addr / token / browser
└─ instances/
   ├─ work.json                # descriptor（0600）
   └─ work.lock                # flock
```

```json
{
  "id": "work",
  "profile": "work",
  "httpUrl": "http://127.0.0.1:8792",
  "wsAddr": "127.0.0.1:8790",
  "pid": 720874,
  "version": "0.4.0",
  "protocol": 2,
  "startedAt": "2026-10-21T15:29:18+08:00"
}
```

- **descriptor 不含 token**；token 仍只在 `config.json`，客户端按 profile 读取。
- HTTP 端口可配 `:0`，真实端口写入 descriptor（WS 端口必须固定，扩展里写死）。
- `/healthz` 增加：`daemonId`、`startedAt`、`protocol`、`instances`（通过 HTTP 提交的活跃批次数）。

---

## 5. 接口变更

### 5.1 事件（只加字段，不改语义）

```json
{
  "eventId": 129,
  "daemonId": "work-7f3a",
  "type": "job.done",
  "batchId": "batch-20261021-1",
  "sessionId": "ses_…",
  "consumerId": null,
  "at": "2026-10-21T16:02:11+08:00",
  "job": { "tag": "v1", "status": "done", "metrics": {} }
}
```

- `eventId` 是 **daemon 内**单调序号；`(daemonId, eventId)` 才是全局唯一游标。
- 保留「daemon 重启后 `lastEventId` 回退 → 客户端从最新开始」的现有语义。
- `consumerId` 为 P3 预留，本期恒为 `null`。

### 5.2 `/api/tools/{name}` 透传会话

新增请求头 `X-JQ-Session-Id: ses_…`，daemon 注入
`tools/call._meta["ai.opencode/sessionID"]`。否则 proxy 转发后 `sessionId` 丢失，
jq-loop 无法唤醒。`POST /api/batches` 已支持 body/头两种方式，保持不变。

### 5.3 MCP 两种 surface

| 阶段 | 客户端配置 | 说明 |
|---|---|---|
| P1 | `{"type":"local","command":["jq-mcp","proxy"]}` | 兼容所有 MCP 客户端 |
| P2 | `{"type":"remote","url":"http://127.0.0.1:8792/mcp"}` | **推荐终点**，不 spawn 进程 |

`/mcp`（Streamable HTTP，MCP 2025-03-26+）：

- `POST /mcp`：JSON-RPC → `application/json` 直返；需要流式时用 SSE。
- `GET /mcp`：SSE，承载 `notifications/message`。
- 必须处理 `MCP-Session-Id`、`MCP-Protocol-Version`、`Origin` 校验、`Last-Event-ID` 重放。
- 现有 `internal/mcp.Server.Run(ctx, io.Reader, io.Writer)` 增加「响应写入器」适配即可复用
  dispatch，无需重写工具层。

---

## 6. jq-loop：per-server 单例

### 6.1 语义

> **一个 OpenCode server 进程内，jq-loop 只有一份状态、一条长轮询、一个游标、一次唤醒。**

同一 server 内所有 session 都在同一进程，`ctx.session.synthetic` 可唤醒任意会话，
因此**不需要 session 归属过滤**；按 workspace 拆份只会带来重复唤醒与游标争用。

### 6.2 实现（P0，约 0.5 天）

```ts
const SHARED = (globalThis.__jqLoop ??= {
  enabled: false,
  cursor: null as number | null,
  poller: null as { ctx: any; ctl: AbortController } | null,
  instances: new Set<any>(),
  pending: new Map<string, any>(),
  iterations: new Map<string, number>(),
  handled: new Map<string, any>(),
});
```

`setup(ctx)`：

1. `SHARED.instances.add(ctx)`；
2. 注册 `/jq-loop` 命令与 `prompt` hook（读写 `SHARED`）；
3. 若 `SHARED.poller == null && SHARED.enabled` → 用当前 ctx 启动轮询；
4. cleanup：从 `instances` 移除；若自己是 poller 持有者 → `abort()`，并从剩余存活实例中
   挑一个重启轮询（**窗口关闭不能把轮询带走**）。

### 6.3 必须同时修的三处

| 问题 | 修正 |
|---|---|
| `handleEvent` 抛错后仍推进游标 | 失败**不推进**，指数退避重试 |
| `handled` 以 `batchId` 为键 | 改为 `${daemonId}:${batchId}` |
| `/jq-loop status` 只报 enabled | 追加 poller pid、cursor、存活实例数、活跃会话数 |

### 6.4 多 server 的兜底（P3）

同机跑两个 `opencode serve` 时 `globalThis` 不共享 → 回到多消费者。

```
POST /api/consumers {id, kind, sessions[], ttl}
GET  /api/events?consumerId=…&since=…
```

daemon 在 `tools/call` 用 `sessionID` 查表 → 事件带 `consumerId`。本期只留字段，不实现。

---

## 7. 状态与生命周期

| 状态 | 归属 | daemon 重启后 |
|---|---|---|
| 浏览器桥 | daemon | 扩展指数退避重连（20–60s） |
| 调度队列 / 运行中任务 | daemon | **丢失**（P3 可选落盘） |
| 任务快照 | daemon（`task_file`） | 恢复 |
| 事件日志 | daemon（环形 10k） | 丢失；客户端游标回退 |
| jq-loop 游标 | OpenCode kv | 保留 |
| `enabled` | OpenCode kv | 取决于 §11 决策 |

已知缺口：daemon 重启会丢掉「在跑批次」的最终事件。P3 建议把事件追加到
`~/.local/share/jq-mcp/events.jsonl`，重启后 replay 最后 N 条。

---

## 8. 安全

- 全部绑定 `127.0.0.1`；HTTP 必须 `Authorization: Bearer <token>`。
- `/mcp` 额外校验 `Origin`（无 Origin 的本地客户端放行，浏览器来源 403）。
- descriptor 0600，不含 token；token 不写日志、不进报告。
- daemon 可代表已登录用户操作，等同本地凭据；不做多用户隔离。

---

## 9. 实施阶段

| 阶段 | 内容 | 主要文件 | 兼容性 | 预估 |
|---|---|---|---|---|
| **P0** | jq-loop 进程级单例 + 游标/去重修正 | `plugins/jq-loop/index.ts` | `/jq-loop on` 变 server 级 | 0.5d |
| **P1** | `serve`/`proxy` 子命令 + flock + descriptor | `cmd/jq-mcp/main.go`、`internal/app` | 无参数行为不变 | 1d |
| **P2** | `/mcp` Streamable HTTP + `X-JQ-Session-Id` | `internal/mcp`、`internal/httpapi` | proxy 仍可用 | 1d |
| **P3** | `consumerId`、事件落盘、多 profile | — | 可选 | 1d |

P0 不依赖 daemon 改造，可先合入；P1 修端口；P2 是终点。

### 代码改动清单（预估）

```
cmd/jq-mcp/main.go          + 子命令 serve / proxy / status / stop
internal/app/app.go         Run() 拆成 RunDaemon() 与 RunProxy()
internal/app/instance.go    + descriptor 读写、flock、探活
internal/httpapi/server.go  + X-JQ-Session-Id；事件加 daemonId
internal/mcp/http.go        + Streamable HTTP transport（P2）
plugins/jq-loop/index.ts    + globalThis 单例、游标/去重修正
docs/architecture-v2.md     本文
```

---

## 10. 验收

1. **端口**：3 workspace + 1 driver → `ss -ltnp | grep 8790` 恒为 1。
2. **工具**：所有 workspace 的 `jq_*` 工具齐全（44 个），无 `mcp connect failed`。
3. **jq-loop**：3 个窗口都 on → `ss -tnp | grep 8792` 恒为 **1** 条长轮询。
4. **唤醒**：同一批次只唤醒一次；A 的任务不出现在 B 的会话里。
5. **故障**：`kill -9` daemon → proxy 保持 stdio 并重连；扩展重连后 `connected:true`。
6. **回归**：`.local/plugincheck/run.mjs`、`node --test plugins/jq-loop/core.test.mjs` 通过。
7. **重启**：`systemctl --user restart jq-mcp` 后客户端会话不中断（P2 下验证）。

---

## 11. 待审核决策

| # | 决策 | 推荐 | 备选 | 影响 |
|---|---|---|---|---|
| D1 | daemon 生命周期 | systemd `--user` 常驻 | 手动 tmux / `--auto-start` | 常驻则无需选举 |
| D2 | `/jq-loop on` 作用域 | **server 级** | 仅当前会话 | server 级实现简单，但会同时开启其它窗口 |
| D3 | `enabled` 是否持久化 | 不持久化（重启后默认关） | 持久化 | 持久化更省事，但可能"莫名自动跑" |
| D4 | P2 是否直接切 `type:"remote"` | 是 | 先跑一段 proxy | remote 更干净，依赖 OpenCode 兼容性 |
| D5 | 事件落盘 | 纳入 P3 | 不做 | 不落盘则重启丢在跑批次的终态 |
| D6 | HTTP 端口 | 默认 8792 固定 | `:0` 动态 + descriptor | 动态更抗冲突，配置略复杂 |

---

## 12. 风险

| 风险 | 缓解 |
|---|---|
| daemon 单点 | systemd `Restart=always`；proxy 重连；`jq-mcp status` 探活 |
| `globalThis` 假设失效（插件被隔离到 worker） | P0 后加运行时探针：`instances.size>1` 时告警；失效则启用 consumerId |
| OpenCode 远程 MCP 兼容性 | P1 的 proxy 作为退路；P2 灰度切换 |
| 固定 WS 端口限制多 profile | descriptor 按 profile 分键，预留多端口 |
| 协议演进导致客户端缓存旧工具表 | 变更时递增 `protocol`，客户端探活不匹配则提示重连 |

---

## 13. 实现记录

### 新增 / 修改

```
cmd/jq-mcp/main.go          serve / proxy / status / stop 子命令
internal/app/app.go         拆成 runDaemon(serveStdio) 与 RunProxy
internal/app/instance.go    descriptor、flock、Probe、ResolveHTTPURL
internal/app/lock_unix.go   flock（Windows 走 health 探活兜底）
internal/app/cli.go         status / stop
internal/httpapi/rpc.go     /api/rpc、/api/notifications(SSE)、/mcp(POST/GET)
internal/httpapi/server.go  health 增加 daemonId/protocol/instances；X-JQ-Session-Id
internal/mcp/server.go      HandleMessage（同步）、通知 sinks、WithSessionID
internal/sched/*            Event.DaemonID + Scheduler.SetDaemonID
plugins/jq-loop/index.ts    globalThis 单例、游标迁移、失败不推进游标
deploy/jq-mcp.service.in    systemd --user 模板
```

### 验证命令

```bash
make build && go test ./...
node --test plugins/jq-loop/core.test.mjs
node .local/plugincheck/run.mjs      # 单 workspace
node .local/plugincheck/multi.mjs    # 两 workspace：每批一次唤醒 + poller 迁移
node .local/plugincheck/proxy.mjs    # descriptor 发现 + 44 工具 + 二次 serve 被拒
node .local/plugincheck/mcphttp.mjs  # /mcp initialize/tools/list/Origin/401
```

### 实测结果

| 用例 | 结果 |
|---|---|
| 两个 workspace 同时提交 | 每个批次 **1 次**唤醒，session 路由正确 |
| 关闭 poller 所属 workspace | 下一批次由另一 workspace 的 ctx 唤醒 |
| 第二个 `serve` | 退出码 0，输出“已在运行 (pid=…)” |
| `proxy` 读 descriptor | 44 个工具，initialize/tools/list 正常 |
| `/mcp` | initialize 返回 `Mcp-Session-Id`；外部 Origin 403；无 token 401 |

### 版本说明

`Version` 仍为 `0.3.0`（按目标版本要求）。注意 git 中 `0.3.0` 已有一个发布提交
（`6e26245`），若该版本已对外发布，应把 `internal/app/app.go` 的 `Version` 改为
`0.4.0`，避免两个不同功能集共用同一版本号；`protocol` 已是 `2`，客户端可按协议号区分。
