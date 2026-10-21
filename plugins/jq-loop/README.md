# jq-loop (OpenCode V2 plugin)

把 jq-mcp 的任务完成事件流，变成 OpenCode 会话里的下一轮模型调用。

```
jq_batch_submit ──► jq-mcp 调度器 ──► /api/events (HTTP long-poll)
                                          │
                                          ▼
                         jq-loop 按短窗口汇总已完成任务
                                          │
                                          ▼
                     ctx.session.synthetic(delivery=queue, resume=true)
                                          │
                                          ▼
                模型继续：修错 / 比较指标 / 立刻补下一个任务
```

它只做“事件 → 唤醒”，不接管浏览器桥、不执行回测、不注册替代工具。

## 为什么默认是流式（stream）

回测任务长短不一。如果等 `batch.done` 再唤醒：

- 短任务先结束，槽位空着，必须等最慢的任务；
- agent 无法根据早期结果提前调整/补任务；
- 一批 20 个任务里只要有一个长任务，整批迭代节奏就被它拖住。

`stream` 模式在**每个任务完成时**就反馈（默认 500ms 合并窗口），agent 可以立刻：

1. 看失败项的 `errorLogs` 先修错；
2. 根据已完成指标决定下一个变体；
3. 用 `jq_batch_submit` 补进腾出的槽位。

要真正顶满并发，建议一次提交 **并发上限 + N** 个任务（over-subscribe），
用 stream 模式持续补位，而不是“提交一批、等最慢的、再提交下一批”。

## 前置条件

- jq-mcp ≥ 0.3.0，且启动参数带 `--http-addr 127.0.0.1:8792`
- OpenCode V2（插件 API）
- 扩展已连接、聚宽已登录

`opencode.json(c)` 示例：

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "servers": {
      "jq-mcp": {
        "type": "local",
        "command": ["/path/to/bin/jq-mcp", "--ws-addr", "127.0.0.1:8790", "--http-addr", "127.0.0.1:8792", "--token", "..."],
      },
    },
  },
  "plugins": [
    {
      "package": "./plugins/jq-loop",
      "options": {
        "enabled": false,
        "url": "http://127.0.0.1:8792",
        "token": "...",
        "wakeOn": "stream",
        "coalesceMs": 500,
        "maxIterations": 0,
      },
    },
  ],
}
```

修改配置后重启 OpenCode 服务（`opencode service restart`）。插件默认 **关闭**，在会话里
执行 `/jq-loop on` 才会自动唤醒；`/jq-loop off` 关闭，`/jq-loop status` 查看状态。

> 全局安装：把同样的条目放到 `~/.config/opencode/opencode.jsonc` 的 `plugins` 里，
> `package` 用绝对路径（例如 `/home/me/project/jq-mcp/plugins/jq-loop`）。本机已经这样安装，
> 因此 `opencode2`（同一个 V2 二进制）在所有项目里都能用 `/jq-loop`。

## 选项

| 选项 | 默认 | 说明 |
|---|---|---|
| `enabled` | `false` | 初始开关；也可用 `/jq-loop on` 打开 |
| `url` | `http://127.0.0.1:8792` | jq-mcp HTTP API（或环境变量 `JQ_MCP_HTTP_URL`） |
| `token` | `jqhelper-dev-token` | HTTP token（或 `JQ_MCP_TOKEN`） |
| `wakeOn` | `stream` | `stream`：每个任务完成即汇总唤醒（推荐）；`job`：同 stream 但窗口 0；`batch`：等整批；`failed`：首个失败立即唤醒 |
| `coalesceMs` | `500` | stream 模式的合并窗口；窗口内完成的任务合成一次唤醒，`0` = 每个任务一次 |
| `maxIterations` | `0` | 每个会话最多自动唤醒次数；`0` = 不限制；用户发新 prompt 时重置 |
| `cooldownMs` | `0` | 同一会话两次唤醒的最小间隔（stream 默认不额外限流） |
| `maxJobs` | `30` | 摘要里最多列出的成功任务数（失败最多 10 个） |
| `pollWaitSeconds` | `20` | `/api/events` 长轮询时长 |
| `instructions` | 内置 | 追加到摘要末尾的“下一步”提示 |
| `dryRun` | `false` | 只打印摘要，不真正唤醒模型 |

## 行为细节

- **事件源**：`GET /api/events?since=<cursor>&wait=20`，游标保存在 `ctx.storage`。
  OpenCode 自身的事件流是 live-only（无 replay），所以这里用 jq-mcp 的游标事件流。
  插件首次加载（没有游标）时从**当前最新事件**开始，不会唤醒历史批次。
- **会话归属**：OpenCode 在 MCP `tools/call._meta["ai.opencode/sessionID"]` 里带 session id；
  jq-mcp 会把它写进批次和事件（`sessionId` 字段）。插件只唤醒事件对应的那个会话。
- **stream 汇总**：同一批次的完成事件先进入内存队列，`coalesceMs` 后一次性唤醒；
  `batch.done` 到达时会立即 flush 尚未发出的结果，不会等窗口。
- **唤醒方式**：`ctx.session.synthetic({delivery:"queue", resume:true})`，不会打断当前 step；
  synthetic 也不触发 prompt hooks，避免把自动消息当成用户输入。
- **摘要**：失败项优先（含 `errorLogs` 末尾几行），然后是成功项的核心指标；总长度截断到 12KB。
- **重启**：插件重启后从 storage 里的游标继续；jq-mcp 重启会重置事件序号，插件检测到
  `lastEventId < cursor` 时自动回退到最新位置。
- **不会唤醒**：通过脚本/HTTP 提交且没有 `sessionId` 的批次（例如 mjs CLI 的扫参）。

## 测试

纯逻辑有 Node 测试：

```sh
node --test plugins/jq-loop/core.test.mjs
```

HTTP 事件面的端到端验证见 `go test ./internal/httpapi/` 和本地 mock（`.local/bench/pluginflow.mjs`）。
