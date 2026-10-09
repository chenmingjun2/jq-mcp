package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jqhelper/jq-mcp/internal/apierr"
	"github.com/jqhelper/jq-mcp/internal/bridge"
	"github.com/jqhelper/jq-mcp/internal/jq"
	"github.com/jqhelper/jq-mcp/internal/mcp"
	"github.com/jqhelper/jq-mcp/internal/task"
)

type deps struct {
	jq    *jq.Client
	tasks *task.Manager
	hub   *bridge.Hub
}

func decodeArgs(args json.RawMessage, v any) error {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if err := json.Unmarshal(args, v); err != nil {
		return apierr.Usage("参数解析失败: %v", err)
	}
	return nil
}

func requireConfirm(confirm bool, action string) error {
	if !confirm {
		return apierr.Usage("%s 需要显式确认：请传入 confirm=true", action)
	}
	return nil
}

func rawJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// stringList accepts either a JSON array of strings or a single comma/space
// separated string, so clients can pass either form.
type stringList []string

func (s *stringList) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*s = splitList(str)
		return nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*s = arr
	return nil
}

// registerTools wires every MCP tool to the jq domain client and task manager.
func registerTools(s *mcp.Server, d deps) {
	registerDiagnosticTools(s, d)
	registerAuthTools(s, d)
	registerStrategyTools(s, d)
	registerFolderTools(s, d)
	registerBacktestTools(s, d)
	registerBacktestDataTools(s, d)
	registerLocalTools(s, d)
	registerCompileTool(s, d)
}

// ---------------------------------------------------------------- diagnostics

func registerDiagnosticTools(s *mcp.Server, d deps) {
	s.Register(mcp.Tool{
		Name:        "jq_bridge_status",
		Description: "查看 jq-mcp 与 jqhelper 浏览器插件的连接状态、插件版本与登录态标记。其它工具返回 no_extension 时先用它排查。",
		InputSchema: mcp.Obj(map[string]any{}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return d.hub.Status(), nil
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_health",
		Description: "一次探测桥接连接与聚宽登录态，返回 connected 与 loggedIn。",
		InputSchema: mcp.Obj(map[string]any{}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			out := map[string]any{"connected": d.hub.Connected()}
			if d.hub.Connected() {
				if raw, err := d.jq.AuthStatus(ctx); err == nil {
					var probe struct {
						LoggedIn bool `json:"loggedIn"`
					}
					_ = json.Unmarshal(raw, &probe)
					out["loggedIn"] = probe.LoggedIn
				} else {
					out["authError"] = apierr.From(err)
				}
			}
			return out, nil
		},
	})
}

// ----------------------------------------------------------------------- auth

func registerAuthTools(s *mcp.Server, d deps) {
	s.Register(mcp.Tool{
		Name:        "jq_auth_status",
		Description: "检测浏览器中聚宽账号的登录状态，返回 loggedIn。",
		InputSchema: mcp.Obj(map[string]any{}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return d.jq.AuthStatus(ctx)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_auth_get_cookie",
		Description: "读取浏览器中聚宽域名的 Cookie（含 HttpOnly）。返回值属于敏感凭据，请勿记录或外传。",
		InputSchema: mcp.Obj(map[string]any{}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return d.jq.Cookie(ctx)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_auth_open_login",
		Description: "在浏览器打开聚宽登录页，提示用户手动完成登录或续期。",
		InputSchema: mcp.Obj(map[string]any{
			"url": mcp.Str("可选，自定义登录页地址"),
		}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				URL string `json:"url"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			return d.jq.OpenLogin(ctx, a.URL)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_auth_refresh",
		Description: "重新加载聚宽标签页以续期登录态，然后返回最新登录状态。",
		InputSchema: mcp.Obj(map[string]any{}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return d.jq.Refresh(ctx)
		},
	})
}

// ------------------------------------------------------------------- strategy

func registerStrategyTools(s *mcp.Server, d deps) {
	s.Register(mcp.Tool{
		Name:        "jq_strategy_list",
		Description: "列出策略与文件夹。返回 items（id/internalId/name/type/updatedAt/runCount/backtestCount）与 folders（fid/name）。",
		InputSchema: mcp.Obj(map[string]any{
			"fid":   mcp.Str("可选，只列出该文件夹"),
			"limit": mcp.Int("最多返回条数，默认 50"),
			"all":   mcp.Bool("递归文件夹拉取全部策略"),
			"sort":  mcp.StrEnum("排序方式", "updated", "name", "created"),
		}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a jq.StrategyListInput
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			return d.jq.StrategyList(ctx, a)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_strategy_find",
		Description: "按名称查找策略，返回精确匹配、模糊匹配与首个命中项。",
		InputSchema: mcp.Obj(map[string]any{
			"name": mcp.Str("策略名称或片段"),
		}, "name"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Name string `json:"name"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.Name == "" {
				return nil, apierr.Usage("name 不能为空")
			}
			return d.jq.StrategyFind(ctx, a.Name)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_strategy_get",
		Description: "读取单个策略详情；includeCode=true 时同时返回完整源码。",
		InputSchema: mcp.Obj(map[string]any{
			"strategyId":  mcp.Str("策略 ID（来自 jq_strategy_list）"),
			"includeCode": mcp.Bool("是否返回源码，默认 false"),
		}, "strategyId"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				StrategyID  string `json:"strategyId"`
				IncludeCode bool   `json:"includeCode"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.StrategyID == "" {
				return nil, apierr.Usage("strategyId 不能为空")
			}
			return d.jq.StrategyGet(ctx, a.StrategyID, a.IncludeCode)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_strategy_create",
		Description: "新建策略，可同时写入初始源码；folderId 可将策略建在指定文件夹下。返回新策略 id。",
		InputSchema: mcp.Obj(map[string]any{
			"name":     mcp.Str("策略名称"),
			"code":     mcp.Str("可选的初始 Python 源码"),
			"type":     mcp.StrEnum("策略类型，默认 stock", "stock", "futures"),
			"folderId": mcp.Str("可选，目标文件夹 fid"),
		}, "name"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a jq.StrategyCreateInput
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.Name == "" {
				return nil, apierr.Usage("name 不能为空")
			}
			return d.jq.StrategyCreate(ctx, a)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_strategy_save",
		Description: "保存策略名称和/或源码到聚宽，会覆盖远端当前内容，需要 confirm=true。",
		InputSchema: mcp.Obj(map[string]any{
			"strategyId": mcp.Str("策略 ID"),
			"name":       mcp.Str("新的策略名称（可选）"),
			"code":       mcp.Str("新的完整源码（可选）"),
			"confirm":    mcp.Bool("必须为 true，确认覆盖远端策略内容"),
		}, "strategyId", "confirm"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				jq.StrategySaveInput
				Confirm bool `json:"confirm"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.StrategyID == "" {
				return nil, apierr.Usage("strategyId 不能为空")
			}
			if err := requireConfirm(a.Confirm, "jq_strategy_save"); err != nil {
				return nil, err
			}
			if a.Name == "" && a.Code == "" {
				return nil, apierr.Usage("name 与 code 至少提供一个")
			}
			return d.jq.StrategySave(ctx, a.StrategySaveInput)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_strategy_clone",
		Description: "复制一个已有策略（读取源码后新建副本）。需要 confirm=true。",
		InputSchema: mcp.Obj(map[string]any{
			"strategyId": mcp.Str("源策略 ID"),
			"name":       mcp.Str("副本名称，默认“原名-copy”"),
			"confirm":    mcp.Bool("必须为 true，确认创建副本"),
		}, "strategyId", "confirm"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				StrategyID string `json:"strategyId"`
				Name       string `json:"name"`
				Confirm    bool   `json:"confirm"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.StrategyID == "" {
				return nil, apierr.Usage("strategyId 不能为空")
			}
			if err := requireConfirm(a.Confirm, "jq_strategy_clone"); err != nil {
				return nil, err
			}
			src, err := d.jq.StrategyGet(ctx, a.StrategyID, true)
			if err != nil {
				return nil, err
			}
			var detail struct {
				Name string `json:"name"`
				Code string `json:"code"`
			}
			_ = json.Unmarshal(src, &detail)
			name := a.Name
			if name == "" {
				name = detail.Name + "-copy"
			}
			return d.jq.StrategyCreate(ctx, jq.StrategyCreateInput{Name: name, Code: detail.Code, Type: "stock"})
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_strategy_delete",
		Description: "删除策略，不可恢复，需要 confirm=true。",
		InputSchema: mcp.Obj(map[string]any{
			"strategyId": mcp.Str("策略 ID"),
			"confirm":    mcp.Bool("必须为 true，确认删除"),
		}, "strategyId", "confirm"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				StrategyID string `json:"strategyId"`
				Confirm    bool   `json:"confirm"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.StrategyID == "" {
				return nil, apierr.Usage("strategyId 不能为空")
			}
			if err := requireConfirm(a.Confirm, "jq_strategy_delete"); err != nil {
				return nil, err
			}
			return d.jq.StrategyDelete(ctx, a.StrategyID)
		},
	})
}

// -------------------------------------------------------------------- folders

func registerFolderTools(s *mcp.Server, d deps) {
	s.Register(mcp.Tool{
		Name:        "jq_folder_list",
		Description: "列出策略文件夹。",
		InputSchema: mcp.Obj(map[string]any{
			"fid":       mcp.Str("可选，父文件夹 fid；默认根目录"),
			"recursive": mcp.Bool("是否递归子文件夹"),
		}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Fid       string `json:"fid"`
				Recursive bool   `json:"recursive"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			return d.jq.FolderList(ctx, a.Fid, a.Recursive)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_folder_create",
		Description: "新建策略文件夹。",
		InputSchema: mcp.Obj(map[string]any{
			"name":     mcp.Str("文件夹名称"),
			"parentId": mcp.Str("可选，父文件夹 fid；默认根目录"),
		}, "name"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Name     string `json:"name"`
				ParentID string `json:"parentId"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.Name == "" {
				return nil, apierr.Usage("name 不能为空")
			}
			return d.jq.FolderCreate(ctx, a.Name, a.ParentID)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_strategy_move",
		Description: "把策略移动到文件夹。需要 confirm=true。",
		InputSchema: mcp.Obj(map[string]any{
			"strategyIds": mcp.Arr("策略 ID 列表", mcp.Str("策略 ID")),
			"folderId":    mcp.Str("目标文件夹 fid"),
			"confirm":     mcp.Bool("必须为 true，确认移动"),
		}, "strategyIds", "folderId", "confirm"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				StrategyIDs stringList `json:"strategyIds"`
				FolderID    string     `json:"folderId"`
				Confirm     bool       `json:"confirm"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if len(a.StrategyIDs) == 0 || a.FolderID == "" {
				return nil, apierr.Usage("strategyIds 与 folderId 为必填")
			}
			if err := requireConfirm(a.Confirm, "jq_strategy_move"); err != nil {
				return nil, err
			}
			return d.jq.StrategyMove(ctx, a.StrategyIDs, a.FolderID)
		},
	})
}

// ------------------------------------------------------------------- backtest

func registerBacktestTools(s *mcp.Server, d deps) {
	s.Register(mcp.Tool{
		Name:        "jq_backtest_run",
		Description: "发起回测。默认正式回测；compile=true 仅编译运行。返回 taskId、id、listId，可用 jq_backtest_wait 等待。",
		InputSchema: mcp.Obj(map[string]any{
			"strategyId": mcp.Str("策略 ID"),
			"start":      mcp.Str("开始日期 YYYY-MM-DD"),
			"end":        mcp.Str("结束日期 YYYY-MM-DD，默认今日"),
			"capital":    mcp.Num("初始资金"),
			"frequency":  mcp.StrEnum("频率，默认 day", "day", "minute"),
			"compile":    mcp.Bool("true 表示只做编译运行"),
			"useCredit":  mcp.Bool("额度耗尽时消耗积分继续（聚宽会提示 50000）"),
		}, "strategyId", "start"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a jq.BacktestRunInput
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.StrategyID == "" || a.Start == "" {
				return nil, apierr.Usage("strategyId 与 start 为必填")
			}
			res, err := d.jq.BacktestRun(ctx, a)
			if err != nil {
				return nil, err
			}
			t := d.tasks.Add(runTask(a, res))
			return map[string]any{
				"taskId": t.ID, "id": res.ID, "listId": res.ListID,
				"strategyId": a.StrategyID, "mode": t.Mode, "status": "running",
			}, nil
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_backtest_list",
		Description: "列出某策略的回测记录，返回详情 id 与列表 id。默认正式回测，compile=true 读编译运行列表。",
		InputSchema: mcp.Obj(map[string]any{
			"strategyId": mcp.Str("策略 ID"),
			"status":     mcp.StrEnum("过滤状态，默认 all", "all", "running", "done", "failed", "cancelled"),
			"limit":      mcp.Int("最多返回条数，默认 50"),
			"compile":    mcp.Bool("读取编译运行列表"),
		}, "strategyId"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a jq.BacktestListInput
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.StrategyID == "" {
				return nil, apierr.Usage("strategyId 不能为空")
			}
			return d.jq.BacktestList(ctx, a)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_backtest_cancel",
		Description: "取消运行中的回测/编译任务。需要 confirm=true。",
		InputSchema: mcp.Obj(map[string]any{
			"backtestId": mcp.Str("回测 id（详情 id 或列表 id）"),
			"confirm":    mcp.Bool("必须为 true，确认取消"),
		}, "backtestId", "confirm"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				BacktestID string `json:"backtestId"`
				Confirm    bool   `json:"confirm"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.BacktestID == "" {
				return nil, apierr.Usage("backtestId 不能为空")
			}
			if err := requireConfirm(a.Confirm, "jq_backtest_cancel"); err != nil {
				return nil, err
			}
			return d.jq.BacktestCancel(ctx, a.BacktestID)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_backtest_delete",
		Description: "删除回测记录，不可恢复，需要 confirm=true。建议传 listId。",
		InputSchema: mcp.Obj(map[string]any{
			"backtestId": mcp.Str("回测列表 id（listId）"),
			"compile":    mcp.Bool("true 表示删除编译运行记录"),
			"confirm":    mcp.Bool("必须为 true，确认删除"),
		}, "backtestId", "confirm"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				BacktestID string `json:"backtestId"`
				Compile    bool   `json:"compile"`
				Confirm    bool   `json:"confirm"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.BacktestID == "" {
				return nil, apierr.Usage("backtestId 不能为空")
			}
			if err := requireConfirm(a.Confirm, "jq_backtest_delete"); err != nil {
				return nil, err
			}
			return d.jq.BacktestDelete(ctx, a.BacktestID, a.Compile)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_backtest_wait",
		Description: "轮询回测直到出现核心指标或超时，返回 status=done 时附带 metrics。",
		InputSchema: mcp.Obj(map[string]any{
			"backtestId":          mcp.Str("回测 id"),
			"timeoutSeconds":      mcp.Int("最长等待秒数，默认 900"),
			"pollIntervalSeconds": mcp.Int("轮询间隔秒数，默认 10"),
		}, "backtestId"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				BacktestID          string `json:"backtestId"`
				TimeoutSeconds      int    `json:"timeoutSeconds"`
				PollIntervalSeconds int    `json:"pollIntervalSeconds"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.BacktestID == "" {
				return nil, apierr.Usage("backtestId 不能为空")
			}
			if a.TimeoutSeconds <= 0 {
				a.TimeoutSeconds = 900
			}
			if a.PollIntervalSeconds <= 0 {
				a.PollIntervalSeconds = 10
			}
			return waitForBacktest(ctx, d, a.BacktestID, time.Duration(a.TimeoutSeconds)*time.Second, time.Duration(a.PollIntervalSeconds)*time.Second)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_backtest_batch",
		Description: "批量提交回测（多区间/多资金），逐个提交并登记任务，返回每条任务的 id 与 listId。",
		InputSchema: mcp.Obj(map[string]any{
			"strategyId": mcp.Str("策略 ID"),
			"runs": mcp.Arr("回测参数列表", mcp.Obj(map[string]any{
				"start":     mcp.Str("开始日期 YYYY-MM-DD"),
				"end":       mcp.Str("结束日期 YYYY-MM-DD"),
				"capital":   mcp.Num("初始资金"),
				"frequency": mcp.StrEnum("频率", "day", "minute"),
				"compile":   mcp.Bool("是否编译运行"),
			})),
			"compile": mcp.Bool("默认 compile 值（可被单条覆盖）"),
		}, "strategyId", "runs"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				StrategyID string           `json:"strategyId"`
				Runs       []map[string]any `json:"runs"`
				Compile    bool             `json:"compile"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.StrategyID == "" || len(a.Runs) == 0 {
				return nil, apierr.Usage("strategyId 与 runs 为必填")
			}
			results := make([]map[string]any, 0, len(a.Runs))
			for _, run := range a.Runs {
				in := jq.BacktestRunInput{StrategyID: a.StrategyID, Compile: a.Compile}
				in.Start, _ = run["start"].(string)
				in.End, _ = run["end"].(string)
				in.Frequency, _ = run["frequency"].(string)
				if c, ok := run["capital"].(float64); ok {
					in.Capital = c
				}
				if c, ok := run["compile"].(bool); ok {
					in.Compile = c
				}
				if in.Start == "" {
					results = append(results, map[string]any{"start": in.Start, "error": "start 不能为空"})
					continue
				}
				res, err := d.jq.BacktestRun(ctx, in)
				if err != nil {
					results = append(results, map[string]any{"start": in.Start, "end": in.End, "error": apierr.From(err)})
					continue
				}
				t := d.tasks.Add(runTask(in, res))
				results = append(results, map[string]any{
					"taskId": t.ID, "id": res.ID, "listId": res.ListID,
					"start": in.Start, "end": in.End, "mode": t.Mode,
				})
			}
			return map[string]any{"items": results}, nil
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_backtest_compare",
		Description: "对比多次回测的核心指标。传入回测 id 列表，返回各自 metrics 与关键字段。",
		InputSchema: mcp.Obj(map[string]any{
			"backtestIds": mcp.Arr("回测 id 列表", mcp.Str("回测 id")),
		}, "backtestIds"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				BacktestIDs stringList `json:"backtestIds"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if len(a.BacktestIDs) == 0 {
				return nil, apierr.Usage("backtestIds 不能为空")
			}
			items := make([]map[string]any, 0, len(a.BacktestIDs))
			for _, id := range a.BacktestIDs {
				entry := map[string]any{"backtestId": id}
				raw, err := d.jq.BacktestStats(ctx, id)
				if err != nil {
					entry["error"] = apierr.From(err)
				} else {
					entry["stats"] = raw
				}
				items = append(items, entry)
			}
			return map[string]any{"items": items}, nil
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_task_list",
		Description: "列出 jq-mcp 记录的回测任务。",
		InputSchema: mcp.Obj(map[string]any{"limit": mcp.Int("最多返回条数，默认 20")}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Limit int `json:"limit"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.Limit <= 0 {
				a.Limit = 20
			}
			return map[string]any{"items": d.tasks.List(a.Limit)}, nil
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_task_get",
		Description: "读取单个 jq-mcp 回测任务。",
		InputSchema: mcp.Obj(map[string]any{"taskId": mcp.Str("任务 ID，形如 task-20260101-1")}, "taskId"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				TaskID string `json:"taskId"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.TaskID == "" {
				return nil, apierr.Usage("taskId 不能为空")
			}
			return d.tasks.Get(a.TaskID)
		},
	})
}

// -------------------------------------------------------------- backtest data

func registerBacktestDataTools(s *mcp.Server, d deps) {
	registerIDTool(s, d, "jq_backtest_get", "读取回测详情，聚合状态、指标与源码。", func(ctx context.Context, id string) (any, error) {
		return d.jq.BacktestGet(ctx, id)
	})
	registerIDTool(s, d, "jq_backtest_stats", "读取回测收益与风险指标。", func(ctx context.Context, id string) (any, error) {
		return d.jq.BacktestStats(ctx, id)
	})
	registerIDTool(s, d, "jq_backtest_trades", "读取回测成交明细（transactionInfo）。", func(ctx context.Context, id string) (any, error) {
		return d.jq.BacktestTrades(ctx, id)
	})
	registerIDTool(s, d, "jq_backtest_positions", "读取回测每日持仓明细（positionInfo）。", func(ctx context.Context, id string) (any, error) {
		return d.jq.BacktestPositions(ctx, id)
	})
	registerIDTool(s, d, "jq_backtest_monthly", "读取分周期/月度收益（risk.algorithmPeriodReturn 等）。", func(ctx context.Context, id string) (any, error) {
		return d.jq.BacktestMonthly(ctx, id)
	})

	s.Register(mcp.Tool{
		Name:        "jq_backtest_result",
		Description: "读取回测收益曲线数据（策略收益、基准、用户记录）。",
		InputSchema: mcp.Obj(map[string]any{
			"backtestId":       mcp.Str("回测 id"),
			"offset":           mcp.Int("起始偏移，默认 0"),
			"userRecordOffset": mcp.Int("用户记录偏移，默认 0"),
		}, "backtestId"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				BacktestID       string `json:"backtestId"`
				Offset           int    `json:"offset"`
				UserRecordOffset int    `json:"userRecordOffset"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.BacktestID == "" {
				return nil, apierr.Usage("backtestId 不能为空")
			}
			return d.jq.BacktestResult(ctx, a.BacktestID, a.Offset, a.UserRecordOffset)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_backtest_logs",
		Description: "读取回测运行日志或错误日志。",
		InputSchema: mcp.Obj(map[string]any{
			"backtestId": mcp.Str("回测 id"),
			"offset":     mcp.Int("日志起始偏移，默认 0"),
			"error":      mcp.Bool("true 读取错误日志"),
			"all":        mcp.Bool("拉取全部普通日志页"),
		}, "backtestId"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a jq.BacktestLogsInput
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.BacktestID == "" {
				return nil, apierr.Usage("backtestId 不能为空")
			}
			return d.jq.BacktestLogs(ctx, a)
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_backtest_export",
		Description: "把回测数据（stats/result/trades/positions/monthly/logs/source）导出为本地 JSON 文件，返回文件路径。",
		InputSchema: mcp.Obj(map[string]any{
			"backtestId": mcp.Str("回测 id"),
			"path":       mcp.Str("本地输出路径，例如 ./backtests/bt-xxx.json"),
			"sections":   mcp.Str("要导出的段落，逗号分隔；默认 stats,result"),
		}, "backtestId", "path"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				BacktestID string `json:"backtestId"`
				Path       string `json:"path"`
				Sections   string `json:"sections"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.BacktestID == "" || a.Path == "" {
				return nil, apierr.Usage("backtestId 与 path 为必填")
			}
			sections := splitList(a.Sections)
			if len(sections) == 0 {
				sections = []string{"stats", "result"}
			}
			out := map[string]any{"backtestId": a.BacktestID, "exportedAt": time.Now().Format(time.RFC3339)}
			for _, sec := range sections {
				raw, err := fetchSection(ctx, d, a.BacktestID, sec, "")
				if err != nil {
					out[sec] = map[string]any{"error": apierr.From(err)}
					continue
				}
				out[sec] = raw
			}
			data, _ := json.MarshalIndent(out, "", "  ")
			if err := writeLocalFile(a.Path, data); err != nil {
				return nil, err
			}
			abs, _ := filepath.Abs(a.Path)
			return map[string]any{"path": abs, "bytes": len(data), "sections": sections}, nil
		},
	})
}

func registerIDTool(s *mcp.Server, d deps, name, desc string, fn func(context.Context, string) (any, error)) {
	s.Register(mcp.Tool{
		Name:        name,
		Description: desc,
		InputSchema: mcp.Obj(map[string]any{"backtestId": mcp.Str("回测 id（详情 id 或列表 id）")}, "backtestId"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				BacktestID string `json:"backtestId"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.BacktestID == "" {
				return nil, apierr.Usage("backtestId 不能为空")
			}
			return fn(ctx, a.BacktestID)
		},
	})
}

// ------------------------------------------------------------- local code/fs

func registerLocalTools(s *mcp.Server, d deps) {
	s.Register(mcp.Tool{
		Name:        "jq_code_standardize",
		Description: "本地把标准滑点/手续费注入策略源码的 initialize，返回标准化后的代码；可选写入 outputPath。",
		InputSchema: mcp.Obj(map[string]any{
			"code":       mcp.Str("源码内容（与 path 二选一）"),
			"path":       mcp.Str("本地源码文件路径（与 code 二选一）"),
			"outputPath": mcp.Str("可选，写入本地文件"),
		}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Code       string `json:"code"`
				Path       string `json:"path"`
				OutputPath string `json:"outputPath"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			code := a.Code
			if code == "" && a.Path != "" {
				b, err := os.ReadFile(a.Path)
				if err != nil {
					return nil, apierr.Wrap(apierr.CodeInternal, fmt.Errorf("读取文件失败: %w", err))
				}
				code = string(b)
			}
			if code == "" {
				return nil, apierr.Usage("code 与 path 至少提供一个")
			}
			out := StandardizeCode(code)
			result := map[string]any{"code": out}
			if a.OutputPath != "" {
				if err := writeLocalFile(a.OutputPath, []byte(out)); err != nil {
					return nil, err
				}
				abs, _ := filepath.Abs(a.OutputPath)
				result["path"] = abs
			}
			return result, nil
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_strategy_export",
		Description: "读取远端策略源码并保存到本地文件。format=py 写源码，format=json 写完整详情。",
		InputSchema: mcp.Obj(map[string]any{
			"strategyId": mcp.Str("策略 ID"),
			"path":       mcp.Str("本地输出路径"),
			"format":     mcp.StrEnum("输出格式，默认 py", "py", "json"),
		}, "strategyId", "path"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				StrategyID string `json:"strategyId"`
				Path       string `json:"path"`
				Format     string `json:"format"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.StrategyID == "" || a.Path == "" {
				return nil, apierr.Usage("strategyId 与 path 为必填")
			}
			raw, err := d.jq.StrategyGet(ctx, a.StrategyID, true)
			if err != nil {
				return nil, err
			}
			var detail struct {
				Name string `json:"name"`
				Code string `json:"code"`
			}
			_ = json.Unmarshal(raw, &detail)
			var data []byte
			if a.Format == "json" {
				data, _ = json.MarshalIndent(json.RawMessage(raw), "", "  ")
			} else {
				data = []byte(detail.Code)
			}
			if err := writeLocalFile(a.Path, data); err != nil {
				return nil, err
			}
			abs, _ := filepath.Abs(a.Path)
			return map[string]any{"path": abs, "bytes": len(data), "name": detail.Name}, nil
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_strategy_import",
		Description: "从本地文件读取源码并保存到远端策略。需要 confirm=true。",
		InputSchema: mcp.Obj(map[string]any{
			"strategyId": mcp.Str("策略 ID"),
			"path":       mcp.Str("本地源码文件路径"),
			"name":       mcp.Str("可选，同时改策略名"),
			"confirm":    mcp.Bool("必须为 true，确认覆盖远端策略内容"),
		}, "strategyId", "path", "confirm"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				StrategyID string `json:"strategyId"`
				Path       string `json:"path"`
				Name       string `json:"name"`
				Confirm    bool   `json:"confirm"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.StrategyID == "" || a.Path == "" {
				return nil, apierr.Usage("strategyId 与 path 为必填")
			}
			if err := requireConfirm(a.Confirm, "jq_strategy_import"); err != nil {
				return nil, err
			}
			b, err := os.ReadFile(a.Path)
			if err != nil {
				return nil, apierr.Wrap(apierr.CodeInternal, fmt.Errorf("读取文件失败: %w", err))
			}
			return d.jq.StrategySave(ctx, jq.StrategySaveInput{StrategyID: a.StrategyID, Name: a.Name, Code: string(b)})
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_file_read",
		Description: "读取本地文本文件内容。",
		InputSchema: mcp.Obj(map[string]any{
			"path": mcp.Str("本地文件路径"),
		}, "path"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.Path == "" {
				return nil, apierr.Usage("path 不能为空")
			}
			b, err := os.ReadFile(a.Path)
			if err != nil {
				return nil, apierr.Wrap(apierr.CodeInternal, fmt.Errorf("读取文件失败: %w", err))
			}
			abs, _ := filepath.Abs(a.Path)
			return map[string]any{"path": abs, "content": string(b), "bytes": len(b)}, nil
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_file_write",
		Description: "写入本地文本文件，需 confirm=true。",
		InputSchema: mcp.Obj(map[string]any{
			"path":    mcp.Str("本地文件路径"),
			"content": mcp.Str("文件内容"),
			"confirm": mcp.Bool("必须为 true，确认写入"),
		}, "path", "content", "confirm"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
				Confirm bool   `json:"confirm"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.Path == "" {
				return nil, apierr.Usage("path 不能为空")
			}
			if err := requireConfirm(a.Confirm, "jq_file_write"); err != nil {
				return nil, err
			}
			if err := writeLocalFile(a.Path, []byte(a.Content)); err != nil {
				return nil, err
			}
			abs, _ := filepath.Abs(a.Path)
			return map[string]any{"path": abs, "bytes": len(a.Content)}, nil
		},
	})
}

// --------------------------------------------------------------- shared helpers

func runTask(in jq.BacktestRunInput, res jq.BacktestRunResult) task.Task {
	mode := "backtest"
	if in.Compile {
		mode = "compile"
	}
	return task.Task{
		StrategyID: in.StrategyID,
		Mode:       mode,
		Start:      in.Start,
		End:        in.End,
		Capital:    in.Capital,
		Frequency:  in.Frequency,
		RemoteID:   res.ID,
		ListID:     res.ListID,
		Status:     task.StatusRunning,
	}
}

func waitForBacktest(ctx context.Context, d deps, backtestID string, timeout, interval time.Duration) (any, error) {
	deadline := time.Now().Add(timeout)
	var last json.RawMessage
	for {
		raw, err := d.jq.BacktestStats(ctx, backtestID)
		if err == nil {
			last = raw
			if statsLookComplete(raw) {
				return map[string]any{"status": "done", "stats": raw}, nil
			}
		} else if e := apierr.From(err); e.Code != apierr.CodeAPI && e.Code != apierr.CodeNotFound {
			return nil, err
		}
		if time.Now().After(deadline) {
			res := map[string]any{"status": "timeout"}
			if last != nil {
				res["stats"] = last
			}
			return res, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func statsLookComplete(raw json.RawMessage) bool {
	var p struct {
		Metrics map[string]any `json:"metrics"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Metrics == nil {
		return false
	}
	for _, key := range []string{"sharpe", "annual_algo_return", "algorithm_return", "max_drawdown"} {
		v, ok := p.Metrics[key]
		if !ok || v == nil {
			continue
		}
		if s, isStr := v.(string); isStr && (s == "" || s == "--") {
			continue
		}
		return true
	}
	return false
}

func fetchSection(ctx context.Context, d deps, id, section, _ string) (json.RawMessage, error) {
	switch section {
	case "stats":
		return d.jq.BacktestStats(ctx, id)
	case "result":
		return d.jq.BacktestResult(ctx, id, 0, 0)
	case "trades":
		return d.jq.BacktestTrades(ctx, id)
	case "positions":
		return d.jq.BacktestPositions(ctx, id)
	case "monthly":
		return d.jq.BacktestMonthly(ctx, id)
	case "logs":
		return d.jq.BacktestLogs(ctx, jq.BacktestLogsInput{BacktestID: id, All: true})
	case "errors":
		return d.jq.BacktestLogs(ctx, jq.BacktestLogsInput{BacktestID: id, Error: true})
	case "source":
		return d.jq.BacktestGet(ctx, id)
	default:
		return nil, apierr.Usage("未知导出段落: %s", section)
	}
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range splitAny(s, ",; \t\n") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func splitAny(s, seps string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(seps, r) })
}

func writeLocalFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return apierr.Wrap(apierr.CodeInternal, fmt.Errorf("创建目录失败: %w", err))
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return apierr.Wrap(apierr.CodeInternal, fmt.Errorf("写入文件失败: %w", err))
	}
	return nil
}
