package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jqhelper/jq-mcp/internal/apierr"
	"github.com/jqhelper/jq-mcp/internal/jq"
	"github.com/jqhelper/jq-mcp/internal/mcp"
	"github.com/jqhelper/jq-mcp/internal/sched"
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
			"account":    mcp.Str("可选：账号池别名"),
		}, "strategyId"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				StrategyID string `json:"strategyId"`
				Start      string `json:"start"`
				End        string `json:"end"`
				UseCredit  bool   `json:"useCredit"`
				Account    string `json:"account"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.StrategyID == "" {
				return nil, apierr.Usage("strategyId 不能为空")
			}
			dd := d
			dd.jq = d.jq.WithAccount(a.Account)
			in := jq.BacktestRunInput{
				StrategyID: a.StrategyID,
				Start:      a.Start,
				End:        a.End,
				Compile:    true,
				UseCredit:  false,
			}
			res, err := dd.jq.BacktestRun(ctx, in)
			if err != nil && sched.IsQuotaError(err) {
				acct := a.Account
				if acct == "" && d.sched != nil {
					acct = d.sched.DefaultAccount()
				}
				if d.sched != nil && acct != "" {
					d.sched.MarkFreeExhausted(acct, apierr.From(err).Message)
				}
				if !a.UseCredit {
					return nil, creditRequiredError(err, a.Account)
				}
				in.UseCredit = true
				res, err = dd.jq.BacktestRun(ctx, in)
			}
			if err != nil {
				return nil, err
			}
			errLogs, normLogs := collectCompileLogs(ctx, dd, a.StrategyID, res.ID, res.ListID)
			return map[string]any{
				"id":            res.ID,
				"listId":        res.ListID,
				"mode":          "compile",
				"account":       a.Account,
				"ok":            len(errLogs) == 0,
				"compileErrors": errLogs,
				"logs":          tail(normLogs, 20),
			}, nil
		},
	})

	s.Register(mcp.Tool{
		Name: "jq_account_limits",
		Description: "读取账号身份、可用积分与会员信息；并发上限请用 jq-mcp accounts cap 配置，" +
			"平台拒绝时会自动收紧。",
		InputSchema: mcp.Obj(map[string]any{"account": mcp.Str("可选：账号池别名")}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Account string `json:"account"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			return d.jq.WithAccount(a.Account).AccountLimits(ctx)
		},
	})
}

// collectCompileLogs waits for a compile run to finish and returns its error
// and normal logs. It exits as soon as an error shows up or the compile reaches
// a terminal state, instead of always paying a fixed multi-second wait.
func collectCompileLogs(ctx context.Context, d deps, strategyID, id, listID string) ([]string, []string) {
	var errLogs, normLogs []string
	for i := 0; i < 16; i++ {
		errLogs = fetchErrorLogs(ctx, d, id)
		if len(errLogs) > 0 {
			break
		}
		// Give the run a moment to register before trusting a terminal status,
		// then stop as soon as it is no longer running.
		if i >= 2 {
			terminal, running := compileState(ctx, d, strategyID, id, listID)
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

// compileState summarizes a strategy's compile (build) list. When the list
// exposes our run's id, that row decides the answer; otherwise it falls back to
// the coarser "is anything still running / has anything finished" heuristic
// (the ids on the build page are not always stable across requests).
func compileState(ctx context.Context, d deps, strategyID string, ids ...string) (terminal, running bool) {
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
			ID     string `json:"id"`
			ListID string `json:"listId"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return false, false
	}
	matchedStatus := ""
	for _, it := range p.Items {
		switch it.Status {
		case "running":
			running = true
		case "done", "failed", "cancelled":
			terminal = true
		}
		for _, id := range ids {
			if id != "" && (it.ID == id || it.ListID == id) {
				matchedStatus = it.Status
			}
		}
	}
	if matchedStatus != "" {
		return matchedStatus != "running", matchedStatus == "running"
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

// summarizeErrorLogs renders the tail of an error log as a single-line error,
// keeping the full list in the caller's "errors" field.
func summarizeErrorLogs(logs []string) string {
	return "回测错误日志: " + strings.Join(tail(logs, 5), "\n")
}

func tail(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return items[len(items)-n:]
}
