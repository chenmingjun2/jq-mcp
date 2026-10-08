# jq-mcp 架构设计

## 1. 目标与非目标

### 目标

1. 让 AI 客户端通过 MCP 安全地操作聚宽账号：新建策略、编写/保存策略代码、发起回测、
   管理回测任务、读取回测数据。
2. 认证完全复用用户在浏览器里的真实登录态，jq-mcp 不接触账号密码。
3. 服务器**不直接请求聚宽私有接口**，所有平台操作都发生在已登录的浏览器里。
4. 单一 Go 二进制 + 零构建的浏览器插件，跨平台（Linux / macOS / Windows）。

### 非目标

- 不做全自动风险投资决策；回测只是执行与数据读取。
- 不在服务端保存或聚合用户 Cookie 到磁盘（仅在内存中按需透传）。
- 不追求脱离浏览器运行；离线无法工作是有意为之。

---

## 2. 组件与数据流

```
┌────────────────┐   MCP / stdio    ┌──────────────────────────────┐
│  AI 客户端      │ ◀──────────────▶ │           jq-mcp (Go)         │
│ (Claude/Codex) │   JSON-RPC 2.0   │                              │
└────────────────┘                  │  ┌────────────┐  ┌─────────┐ │
                                    │  │ MCP Server │  │  Task   │ │
                                    │  └─────┬──────┘  │ Manager │ │
                                    │        │         └────┬────┘ │
                                    │  ┌─────▼──────────────────▼─┐ │
                                    │  │      jq domain layer     │ │
                                    │  └─────────────┬────────────┘ │
                                    │  ┌─────────────▼────────────┐ │
                                    │  │  Bridge (WebSocket Hub)  │ │
                                    │  └─────────────┬────────────┘ │
                                    │  ┌─────────────▼────────────┐ │
                                    │  │   Browser launcher       │ │
                                    │  └──────────────────────────┘ │
                                    └──────────────┬───────────────┘
                                    ws://127.0.0.1:8790/ws (+token)
                                                   │
                                    ┌──────────────▼───────────────┐
                                    │        jqhelper (MV3)         │
                                    │  ┌────────────────────────┐   │
                                    │  │ background service      │   │
                                    │  │ worker: WS client+router│   │
                                    │  └───────┬────────────────┘   │
                                    │          │ executeScript       │
                                    │  ┌───────▼────────────────┐   │
                                    │  │ page bridge (MAIN world)│   │
                                    │  │  DOM 自动化 + 页面 fetch │   │
                                    │  └───────┬────────────────┘   │
                                    └──────────┼────────────────────┘
                                               ▼
                                    https://www.joinquant.com
```

### 请求生命周期（以 `jq_backtest_run` 为例）

1. AI 客户端调用 MCP 工具 `jq_backtest_run`，参数为策略 ID、起止日期、资金、频率等。
2. `internal/mcp` 解析参数 → 调用 `internal/jq.BacktestRun`。
3. `internal/jq` 通过 `bridge.Hub.Call(ctx, "backtest.run", params)` 发送 WebSocket 请求。
4. jqhelper 的 service worker 收到请求，确保存在一个 joinquant.com 标签页，
   注入 `page/jq-page.js`（MAIN world），调用其 `run("backtest.run", params)`。
5. 页面脚本在聚宽页面上下文内收集编辑页表单、填入参数、POST 提交，返回结构化结果。
6. 结果沿原路回传；`internal/task` 记录一条回测任务，便于后续 `jq_backtest_wait` 轮询。

---

## 3. 传输层：WebSocket 桥

- 服务端：`internal/bridge` 在 `127.0.0.1:<port>` 上提供 `/ws` 升级端点。
- 客户端：插件 service worker 主动连接；断线后指数退避重连。
- 协议：JSON-RPC 2.0 风格的请求/响应 + 通知，详见 `docs/protocol.md`。
- 鉴权：连接时 `hello` 必须携带与服务端一致的 `token`，否则立即关闭。
- 多客户端：Hub 保留所有在线连接；`Call` 默认选择最近活跃且健康的连接。

为什么用 WebSocket 而不是原生消息 / CDP：

- MV3 service worker 原生支持 WebSocket，无需额外宿主配置。
- 双向、低延迟，天然支持服务端主动下发与插件主动上报（登录态变化）。
- 与浏览器启动方式解耦：attach 模式与 launch 模式可共用同一协议。

---

## 4. 浏览器管理：双模式

配置项 `browser.mode`：

| 模式 | 行为 | 适用 |
|------|------|------|
| `attach`（默认） | jq-mcp 只提供桥服务；用户自己打开浏览器、加载插件并登录 | 日常使用、已有登录态 |
| `launch` | jq-mcp 启动 Chrome/Chromium，加载 `extension/`，使用独立 user-data-dir | 无头/隔离环境、自动化 |

`launch` 模式参数：

- `headless`：`true` 使用 `--headless=new`；`false` 显示窗口供用户登录。
- `chrome_path`：浏览器可执行文件路径（默认自动探测 `google-chrome` 等）。
- `user_data_dir`：独立配置目录，持久化登录态（默认 `~/.local/share/jq-mcp/chrome`）。
- `start_url`：启动时打开的页面（默认聚宽首页）。

由于插件通过 WebSocket 主动外连，服务器无需 CDP 即可控制；`launch` 只负责把浏览器拉起来。

> 无头模式下无法人工登录。首次使用建议 `--headless=false` 完成登录，之后可切回无头复用同一 user-data-dir。

> **禁止使用 `--disable-extensions-except`。** 它会禁用该 profile 中除 jqhelper 外的所有扩展，
> 在一次重启后就可能把用户手动安装的插件“挤掉”。`launch` 只用 `--load-extension` 加载，不影响其它扩展。
> 日常使用优先 `attach` 模式，完全不由 jq-mcp 启动浏览器。

---

## 5. 插件侧：混合自动化

插件采用「DOM 自动化 + 页面上下文 fetch」的混合策略：

- **页面上下文 fetch**：批量数据（列表、详情、指标、日志、提交表单）在 `joinquant.com`
  页面**主世界（MAIN world）**里用 `fetch` 完成。浏览器自动携带 Cookie，且同源无 CORS。
  可显式设置 `referrer`（同源）以满足服务端对 `Referer` 的校验。
- **DOM 自动化**：需要真实点击/导航/触发前端逻辑时，操作页面元素；登录、切换页面属于此类。
- **HTML 解析**：在注入页面上下文中用 `DOMParser` 解析，返回结构化 JSON；service worker
  不承担 HTML 解析（MV3 SW 无 DOM API）。

注入方式：

1. `chrome.scripting.executeScript({ files: ["page/jq-page.js"], world: "MAIN" })` 幂等安装
   `window.__jqhelper`。
2. `chrome.scripting.executeScript({ func, world: "MAIN" })` 调用 `window.__jqhelper.run(op, params)`。

所有平台接口路径集中在 `extension/page/jq-page.js` 顶部常量表中，便于页面改版时调整。

### 登录与凭据

- `auth.status`：读取 `/user/index/isLogin`。
- `auth.getCookie`：通过 `chrome.cookies.getAll({domain: ".joinquant.com"})` 读取（包含 HttpOnly）。
- `auth.openLogin`：在后台标签页打开登录页，用户在浏览器里手动完成登录。
- `auth.refresh`：重载当前聚宽标签页以续期会话。

jq-mcp 不落盘 Cookie；`jq_auth_get_cookie` 的返回值在工具描述里标注为敏感。

---

## 6. 任务管理

`internal/task` 维护 jq-mcp 侧的回测任务登记：

```
Task {
  id, strategy_id, mode(backtest|compile), start, end, capital, frequency,
  remote_id, list_id, status(running|done|failed|cancelled|timeout),
  created_at, updated_at, last_error
}
```

- `jq_backtest_run` 成功后在登记表中新建任务。
- `jq_backtest_wait` 轮询 `backtest.stats`，直到出现核心指标或状态失败/超时。
- `jq_task_list` / `jq_task_get` 让客户端查看 jq-mcp 记录的任务。
- 任务默认保存在内存，可配置 `--task-file` 落盘（JSON）以便重启恢复。

---

## 7. 错误模型

统一错误码（MCP 工具错误与桥响应错误共用）：

| code | 含义 | 建议处理 |
|------|------|----------|
| `no_extension` | 没有插件连接 | 检查插件与 token，提示用户打开插件/登录 |
| `not_authenticated` | 聚宽未登录 | 调用 `jq_auth_open_login` |
| `not_found` | 资源不存在 | 校正 ID/名称 |
| `usage_error` | 参数错误 | 修正参数 |
| `api_error` | 聚宽返回业务错误 | 查看 `details.raw` |
| `network_error` | 页面/网络请求失败 | 重试 |
| `timeout` | 等待超时 | 稍后用任务查询 |
| `internal_error` | 服务器内部错误 | 查看 stderr 日志 |

桥层在插件断开时会把在途请求立即以 `no_extension` 失败，避免悬挂。

---

## 8. 安全模型

1. **仅本机**：WebSocket 绑定 `127.0.0.1`，不监听外网。
2. **共享 token**：首次运行随机生成，写入 `~/.config/jq-mcp/config.json`（`0600`）。
3. **最小权限插件**：host 权限仅 `*://*.joinquant.com/*`，另需 `tabs/scripting/cookies/storage`。
4. **敏感输出**：Cookie、源码等只在被显式请求时返回；日志中脱敏。
5. **破坏性操作**：删除类工具要求 `confirm: true`；jq-mcp 在缺少确认时拒绝执行。

---

## 9. 可扩展点

- 增加模拟盘、社区、研究平台工具：在 `internal/jq` + `extension/handlers` 各加一层即可。
- 增加 HTTP/SSE 传输：`internal/mcp` 已隔离传输，可平行增加 `internal/mcp/http.go`。
- 页面改版兜底：`jq-page.js` 中路径与解析器分离，可在不改变协议的前提下替换 DOM 策略。

---

## 10. 聚宽接口对照表（2026-10 实测）

以下接口均在已登录的 `joinquant.com` 页面上下文中调用（同源、自动携带 Cookie），
是站点自身前端使用的路由，不是本项目自造。所有路径集中在 `extension/page/jq-page.js` 的 `PATHS`。

| 用途 | 方法与路径 | 关键参数 |
|------|-----------|----------|
| 登录态 | `GET /user/index/isLogin` | 结果在 `data.isLogin` |
| 策略列表 | `GET /algorithm/index/list` | `fId`、`page`（HTML，`tr.algorithm_list`） |
| 策略编辑页 | `GET /algorithm/index/edit` | `algorithmId`（HTML，表单 + `textarea[name="algorithm[code]"]`） |
| 新建策略 | `POST /algorithm/index/new` | 明文 `code` |
| 保存策略 | `POST /algorithm/index/save` | 表单 + `algorithm[code]`(base64) + `encrType=base64` |
| 删除策略 | `POST /algorithm/index/del` | `algorithmId`=行的 `_algorithmid` |
| 取消回测 | `POST /algorithm/index/cancel` | `backtestId`=内部 id |
| 新建文件夹 | `POST /algorithm/index/AddFile` | `pId`（query）+ `name` |
| 重命名文件夹 | `POST /algorithm/index/UpdateFile` | `fId`（query）+ `name` |
| 移动策略 | `POST /algorithm/index/AlgorithmToFile` | `ids`、`fId`（query） |
| 发起回测 | `POST /algorithm/index/build` | 表单 + `backtest[type]=0/1`；返回 `data.backtestId_`/`data.backtestId` |
| 回测列表 | `GET /algorithm/backtest/list` | `algorithmId`（用策略编辑 id 即可） |
| 编译列表 | `GET /algorithm/backtest/buildList` | 同上 |
| 回测详情 | `GET /algorithm/backtest/detail` | `backtestId`（接受列表 id 或详情 id，页面再解析出 `#backtestId` 内部 id） |
| 指标 | `GET /algorithm/backtest/stats` | `backtestId`=内部 id |
| 收益曲线 | `POST /algorithm/backtest/result` | `backtestId`=内部 id、`offset`、`userRecordOffset` |
| 运行日志 | `GET /algorithm/backtest/log` | `backtestId`=内部 id、`offset` |
| 错误日志 | `GET /algorithm/backtest/error` | `backtestId`=内部 id |
| 源码 | `GET /algorithm/backtest/source` | `backtestId`=内部 id |
| 成交明细 | `GET /algorithm/backtest/transactionInfo` | `backtestId`=内部 id |
| 持仓明细 | `GET /algorithm/backtest/positionInfo` | `backtestId`=内部 id |
| 分周期收益 | `GET /algorithm/backtest/risk` | `backtestId`=内部 id |
| 删除回测 | `POST /algorithm/backtest/del` | `type=0|1` + `backtestId` |

### 10.1 已知坑

1. **ID 有多份**：策略行同时存在编辑 id（`/edit?algorithmId=`）与内部 id（`td[_algorithmid]`）。
   编辑/保存用编辑 id；删除用内部 id。
2. **回测 ID 需二级解析**：`stats/result/log/error/source/transactionInfo/positionInfo/risk/cancel`
   都要求详情页里的内部 `#backtestId`，因此这些操作会先取一次详情页。
3. **登录误判**：已登录页面的导航含 `/user/login/logout`，不能用 `/user/login` 子串判断登录页；
   应匹配登录表单（`CyLoginForm[username]`）或登录页标题。
4. **保存编码**：源码提交前需 base64 编码并带 `encrType=base64`；`/algorithm/index/new` 的 `code` 则是明文。
5. **成交与持仓**分别由 `transactionInfo` / `positionInfo` 提供；`result` 只返回收益曲线与 `gains`/`orders` 计数。
