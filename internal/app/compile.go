package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jqhelper/jq-mcp/internal/apierr"
	"github.com/jqhelper/jq-mcp/internal/jq"
	"github.com/jqhelper/jq-mcp/internal/mcp"
)

// registerCompileTool adds jq_strategy_compile: it triggers the edit page's
// "编译运行" button (backtest[type]=1) and reads back compile errors/logs, so
// code can be validated before spending time on a full backtest.
func registerCompileTool(s *mcp.Server, d deps) {
	s.Register(mcp.Tool{
		Name: "jq_strategy_compile",
		Description: "触发聚宽编辑页的“编译运行”，在正式回测前校验策略代码；返回编译错误与编译日志。" +
			"ok=true 表示无编译错误。",
		InputSchema: mcp.Obj(map[string]any{
			"strategyId": mcp.Str("策略 ID"),
			"start":      mcp.Str("可选，编译用开始日期 YYYY-MM-DD（默认沿用编辑页表单值）"),
			"end":        mcp.Str("可选，编译用结束日期 YYYY-MM-DD"),
			"useCredit":  mcp.Bool("额度耗尽时消耗积分继续（聚宽会提示 50000）"),
		}, "strategyId"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				StrategyID string `json:"strategyId"`
				Start      string `json:"start"`
				End        string `json:"end"`
				UseCredit  bool   `json:"useCredit"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.StrategyID == "" {
				return nil, apierr.Usage("strategyId 不能为空")
			}
			res, err := d.jq.BacktestRun(ctx, jq.BacktestRunInput{
				StrategyID: a.StrategyID,
				Start:      a.Start,
				End:        a.End,
				Compile:    true,
				UseCredit:  a.UseCredit,
			})
			if err != nil {
				return nil, err
			}
			errLogs, normLogs := collectCompileLogs(ctx, d, a.StrategyID, res.ID)
			return map[string]any{
				"id":            res.ID,
				"listId":        res.ListID,
				"mode":          "compile",
				"ok":            len(errLogs) == 0,
				"compileErrors": errLogs,
				"logs":          tail(normLogs, 20),
			}, nil
		},
	})

	s.Register(mcp.Tool{
		Name: "jq_account_limits",
		Description: "读取账号的并行编译/回测上限与会员信息（抓账号接口与会员权益页，尽力解析；" +
			"无法确定时返回原始候选字段供排查）。",
		InputSchema: mcp.Obj(map[string]any{}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return d.jq.AccountLimits(ctx)
		},
	})
}

// collectCompileLogs waits for a compile run to finish and returns its error
// and normal logs. It exits as soon as an error shows up or the compile reaches
// a terminal state, instead of always paying a fixed multi-second wait.
func collectCompileLogs(ctx context.Context, d deps, strategyID, id string) ([]string, []string) {
	var errLogs, normLogs []string
	for i := 0; i < 16; i++ {
		errLogs = fetchErrorLogs(ctx, d, id)
		if len(errLogs) > 0 {
			break
		}
		// Give the run a moment to register before trusting a terminal status,
		// then stop as soon as it is no longer running.
		if i >= 2 {
			terminal, running := compileState(ctx, d, strategyID)
			if terminal && !running {
				if again := fetchErrorLogs(ctx, d, id); len(again) > 0 {
					errLogs = again
				}
				break
			}
		}
		select {
		case <-ctx.Done():
			return errLogs, normLogs
		case <-time.After(1 * time.Second):
		}
	}
	if raw, err := d.jq.BacktestLogs(ctx, jq.BacktestLogsInput{BacktestID: id}); err == nil {
		normLogs = parseLogArr(raw)
	}
	return errLogs, normLogs
}

func fetchErrorLogs(ctx context.Context, d deps, id string) []string {
	raw, err := d.jq.BacktestLogs(ctx, jq.BacktestLogsInput{BacktestID: id, Error: true})
	if err != nil {
		return nil
	}
	return parseLogArr(raw)
}

// compileState summarizes a strategy's compile (build) list. The list's ids
// rotate on every request, so matching by id is impossible: we only look at
// whether any run is still running and whether any has finished. This stays
// correct regardless of list ordering.
func compileState(ctx context.Context, d deps, strategyID string) (terminal, running bool) {
	raw, err := d.jq.BacktestList(ctx, jq.BacktestListInput{
		StrategyID: strategyID,
		Limit:      20,
		Compile:    true,
	})
	if err != nil {
		return false, false
	}
	var p struct {
		Items []struct {
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return false, false
	}
	for _, it := range p.Items {
		switch it.Status {
		case "running":
			running = true
		case "done", "failed", "cancelled":
			terminal = true
		}
	}
	return terminal, running
}

func parseLogArr(raw json.RawMessage) []string {
	var p struct {
		Logs []string `json:"logs"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil
	}
	return p.Logs
}

func tail(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return items[len(items)-n:]
}
