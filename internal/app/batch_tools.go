package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jqhelper/jq-mcp/internal/apierr"
	"github.com/jqhelper/jq-mcp/internal/mcp"
	"github.com/jqhelper/jq-mcp/internal/sched"
)

// registerBatchTools wires the server-side scheduler into MCP. Clients declare
// jobs and read results; concurrency, work-stealing and back-pressure are owned
// by the scheduler (not the client).
func registerBatchTools(s *mcp.Server, d deps) {
	if d.sched == nil {
		return
	}

	s.Register(mcp.Tool{
		Name:        "jq_batch_submit",
		Description: "把回测任务提交到服务端调度器（不阻塞）。调度器用固定工作线程消费共享队列（工作窃取），把并发顶满，并在被平台拒绝时自动收紧/背压重试。返回 batchId，用 jq_batch_status/jq_batch_wait 取结果。",
		InputSchema: mcp.Obj(map[string]any{
			"jobs": mcp.Arr("任务列表", mcp.Obj(map[string]any{
				"tag":        mcp.Str("自定义标识（回显在结果里）"),
				"strategyId": mcp.Str("策略 ID"),
				"start":      mcp.Str("开始日期 YYYY-MM-DD"),
				"end":        mcp.Str("结束日期 YYYY-MM-DD"),
				"capital":    mcp.Num("初始资金"),
				"frequency":  mcp.StrEnum("频率", "day", "minute"),
				"compile":    mcp.Bool("是否编译运行"),
				"useCredit":  mcp.Bool("是否可用积分续跑"),
				"priority":   mcp.Int("优先级（小=先跑）"),
			})),
			"maxConcurrent": mcp.Int("覆盖并发上限（0=沿用当前）"),
			"workers":       mcp.Int("工作线程数（0=默认）"),
		}, "jobs"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Jobs          []sched.Job `json:"jobs"`
				MaxConcurrent int         `json:"maxConcurrent"`
				Workers       int         `json:"workers"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if len(a.Jobs) == 0 {
				return nil, apierr.Usage("jobs 不能为空")
			}
			for i := range a.Jobs {
				if a.Jobs[i].StrategyID == "" || a.Jobs[i].Start == "" {
					return nil, apierr.Usage("每个 job 需含 strategyId 与 start")
				}
			}
			if a.MaxConcurrent > 0 {
				d.sched.SetCap(a.MaxConcurrent)
			}
			b := d.sched.Submit(a.Jobs)
			return map[string]any{
				"batchId":   b.ID,
				"count":     len(b.Jobs),
				"status":    "submitted",
				"cap":       d.sched.Cap(),
				"jobs":      b.Jobs,
				"createdAt": b.CreatedAt,
			}, nil
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_batch_status",
		Description: "查看批次进度与每个任务的状态/指标（非阻塞）。",
		InputSchema: mcp.Obj(map[string]any{
			"batchId": mcp.Str("批次 ID；留空则返回最近批次列表"),
			"limit":   mcp.Int("列表条数（batchId 为空时）"),
		}),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				BatchID string `json:"batchId"`
				Limit   int    `json:"limit"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.BatchID == "" {
				return map[string]any{"batches": d.sched.List(a.Limit), "cap": d.sched.Cap()}, nil
			}
			b, err := d.sched.Status(a.BatchID)
			if err != nil {
				return nil, err
			}
			return withCounts(b), nil
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_batch_wait",
		Description: "阻塞直到批次全部任务终结（或超时），返回结果。一次调用即可，无需客户端轮询。",
		InputSchema: mcp.Obj(map[string]any{
			"batchId":        mcp.Str("批次 ID"),
			"timeoutSeconds": mcp.Int("最长等待秒数（默认 3600）"),
		}, "batchId"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				BatchID        string `json:"batchId"`
				TimeoutSeconds int    `json:"timeoutSeconds"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.BatchID == "" {
				return nil, apierr.Usage("batchId 不能为空")
			}
			if a.TimeoutSeconds <= 0 {
				a.TimeoutSeconds = 3600
			}
			b, err := d.sched.Wait(ctx, a.BatchID, time.Duration(a.TimeoutSeconds)*time.Second)
			if err != nil {
				return nil, err
			}
			return withCounts(b), nil
		},
	})
}

func withCounts(b *sched.Batch) map[string]any {
	counts := map[string]int{}
	for _, js := range b.Jobs {
		counts[js.Status]++
	}
	return map[string]any{
		"batchId": b.ID,
		"total":   len(b.Jobs),
		"counts":  counts,
		"done":    allTerminalBatch(b),
		"jobs":    b.Jobs,
	}
}

func allTerminalBatch(b *sched.Batch) bool {
	for _, js := range b.Jobs {
		switch js.Status {
		case sched.StDone, sched.StFailed, sched.StTimeout:
		default:
			return false
		}
	}
	return true
}
