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

### 4. 接入 MCP 客户端

jq-mcp 使用 **stdio** 传输。以 OpenCode 为例，项目级 `opencode.json`：

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "servers": {
      "jq-mcp": {
        "type": "local",
        "command": ["/absolute/path/to/bin/jq-mcp",
                    "--ws-addr", "127.0.0.1:8790",
                    "--token", "YOUR_TOKEN"]
      }
    }
  }
}
```

（`opencode.json` 含 token，勿提交；本仓库已将其加入 `.gitignore`。）

### 5. 验证

在客户端调用 `jq_health`，应返回 `{"connected": true, "loggedIn": true}`。

---

## 运行模式

| 模式 | 行为 | 适用 |
|------|------|------|
| `attach`（默认） | jq-mcp 只提供桥服务，用户自己打开浏览器、加载插件并登录 | 日常使用、已有登录态 |
| `launch` | jq-mcp 启动 Chrome/Chromium 并加载 `extension/` | 无头/隔离环境 |

```bash
bin/jq-mcp                                   # attach（推荐）
bin/jq-mcp --browser-mode launch --headless=false   # 首次登录用有头
bin/jq-mcp --browser-mode launch --headless         # 之后可用无头
```

`launch` 相关参数：`--chrome-path`、`--user-data-dir`、`--extension-dir`。

> ⚠️ **绝不要使用 `--disable-extensions-except`。** 它会禁用该 profile 中除 jqhelper 外的所有扩展，
> 重启后可能把用户手动安装的插件“挤掉”。`launch` 只用 `--load-extension` 加载，不影响其它扩展。

---

## 工具一览

共 **40** 个工具。✅ 只读，✍️ 写操作（带 `confirm` 门禁）。

### 诊断
| 工具 | 说明 |
|------|------|
| ✅ `jq_bridge_status` | 插件连接数、版本、最近 hello |
| ✅ `jq_health` | 一次探测 `connected` + `loggedIn` |

### 认证 / 账号
| 工具 | 说明 |
|------|------|
| ✅ `jq_auth_status` | 聚宽登录状态 |
| ✅ `jq_auth_get_cookie` | 读取 Cookie（敏感） |
| ✅ `jq_auth_open_login` | 打开登录页供手动登录 |
| ✅ `jq_auth_refresh` | 刷新登录态 |
| ✅ `jq_account_limits` | 并行回测/编译上限与会员信息（抓账号接口+会员页） |

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
| ✅ `jq_backtest_wait` | 轮询到终态 |
| ✍️ `jq_backtest_cancel` | 取消运行中的回测（`confirm`） |
| ✍️ `jq_backtest_delete` | 删除记录（`confirm`，`compile` 区分） |
| ✍️ `jq_backtest_batch` | 批量提交（同策略多区间/多资金） |
| ✅ `jq_backtest_compare` | 多次回测指标对比 |

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

### 云端策略导出到本地
```
jq_strategy_export(strategyId, path="./x.py")      # format=py|json
jq_backtest_export(backtestId, path="./bt.json")   # sections=stats,result,trades,...
```

---

## 账号并发限制

聚宽限制「并行编译/回测数量最多 N 个」（N 由账号会员等级决定）。本项目的处理：

- 提交被拒时，错误 `details.maxConcurrent` 会带上 N（从服务端文案解析）。
- `jq_account_limits` 抓账号接口与会员页，尽力返回 N 与会员信息。
- 建议客户端按 N 控制并发；脚本侧可用“重试直到有空位”的背压方式。

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
│   ├── apierr/  bridge/  browser/  config/  jq/  mcp/  task/  app/
├── extension/                  # jqhelper（MV3，无需构建）
│   ├── manifest.json  background.js
│   ├── lib/  handlers/  page/  options.html
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
