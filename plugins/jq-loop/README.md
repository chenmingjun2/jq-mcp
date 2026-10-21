# jq-loop (OpenCode V2 plugin)

把 jq-mcp 的回测/编译批次完成事件，变成 OpenCode 会话里的下一轮模型调用。

```
jq_batch_submit ──► jq-mcp 调度器 ──► /api/events (HTTP long-poll)
                                          │
                                          ▼
                              jq-loop 汇总结果 + 校验护栏
                                          │
                                          ▼
                     ctx.session.synthetic(delivery=queue, resume=true)
                                          │
                                          ▼
                          模型继续：修错 / 比较指标 / 提交下一批
```

它只做“事件 → 唤醒”，不接管浏览器桥、不执行回测、不注册替代工具。

## 前置条件

- jq-mcp ≥ 0.3.0，且启动参数带 `--http-addr 127.0.0.1:8792`
- OpenCode V2（插件 API `@opencode/plugin`）
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
        "wakeOn": "batch",
        "maxIterations": 5,
      },
    },
  ],
}
```

修改配置后重启 OpenCode 服务（`opencode service restart`）。插件默认 **关闭**，在会话里
执行 `/jq-loop on` 才会自动唤醒；`/jq-loop off` 关闭，`/jq-loop status` 查看状态。

## 选项

| 选项 | 默认 | 说明 |
|---|---|---|
| `enabled` | `false` | 初始开关；也可用 `/jq-loop on` 打开 |
| `url` | `http://127.0.0.1:8792` | jq-mcp HTTP API（或环境变量 `JQ_MCP_HTTP_URL`） |
| `token` | `jqhelper-dev-token` | HTTP token（或 `JQ_MCP_TOKEN`） |
| `wakeOn` | `batch` | `batch`：整批结束唤醒；`failed`：首个失败立即唤醒；`job`：每个任务都唤醒（受冷却限制） |
| `maxIterations` | `5` | 每个会话最多自动唤醒次数；用户发新 prompt 时重置；`0` = 不限制 |
| `cooldownMs` | `5000` | 同一会话两次唤醒的最小间隔 |
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
