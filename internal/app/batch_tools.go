package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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
		Description: "把回测任务提交到服务端调度器（不阻塞）。调度器用固定工作线程消费共享队列（工作窃取），把并发顶满，并在被平台拒绝时自动收紧/背压重试。返回 batchId，用 jq_batch_wait/jq_batch_events 取结果。可选 callbackUrl：批次/任务终结时服务端会 POST 事件，并附 HMAC 签名。",
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
			"maxConcurrent":  mcp.Int("覆盖并发上限（0=沿用当前）"),
			"workers":        mcp.Int("工作线程数（0=默认）"),
			"callbackUrl":    mcp.Str("可选：事件回调地址（http/https），异步 POST，失败自动重试"),
			"callbackSecret": mcp.Str("可选：HMAC-SHA256 密钥；设置后请求带 X-JQ-Signature"),
			"callbackOn":     mcp.StrEnum("回调范围，默认 batch（只回调批次完成）", "batch", "job", "both"),
		}, "jobs"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Jobs           []sched.Job `json:"jobs"`
				MaxConcurrent  int         `json:"maxConcurrent"`
				Workers        int         `json:"workers"`
				CallbackURL    string      `json:"callbackUrl"`
				CallbackSecret string      `json:"callbackSecret"`
				CallbackOn     string      `json:"callbackOn"`
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
			if err := validateCallback(a.CallbackURL, a.CallbackOn); err != nil {
				return nil, err
			}
			if a.MaxConcurrent > 0 {
				d.sched.SetCap(a.MaxConcurrent)
			}
			b := d.sched.SubmitWithOptions(a.Jobs, sched.SubmitOptions{
				CallbackURL:    a.CallbackURL,
				CallbackSecret: a.CallbackSecret,
				CallbackOn:     a.CallbackOn,
				SessionID:      mcp.SessionIDFromContext(ctx),
			})
			// Read a snapshot: workers may start mutating job state immediately.
			snap, err := d.sched.Status(b.ID)
			if err != nil {
				return nil, err
			}
			out := map[string]any{
				"batchId":   snap.ID,
				"count":     len(snap.Jobs),
				"status":    "submitted",
				"cap":       d.sched.Cap(),
				"jobs":      snap.Jobs,
				"createdAt": snap.CreatedAt,
			}
			if snap.Callback != nil {
				out["callback"] = map[string]any{
					"url":    snap.Callback.URL,
					"on":     snap.Callback.On,
					"signed": snap.Callback.Secret != "",
				}
			}
			return out, nil
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
		Description: "阻塞直到批次全部任务终结（或超时），返回结果。一次调用即可，无需客户端 sleep 轮询；客户端提供 progressToken 时会收到进度通知。sinceEventId>0 时每次有新事件即返回并附事件明细；includeEvents=true 可强制附带事件。",
		InputSchema: mcp.Obj(map[string]any{
			"batchId":        mcp.Str("批次 ID"),
			"timeoutSeconds": mcp.Int("最长等待秒数（默认 3600）"),
			"sinceEventId":   mcp.Int("事件游标；>0 时有事即返回（续等/增量模式）"),
			"includeEvents":  mcp.Bool("是否在结果中附带事件明细（sinceEventId>0 时默认附带）"),
		}, "batchId"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				BatchID        string `json:"batchId"`
				TimeoutSeconds int    `json:"timeoutSeconds"`
				SinceEventID   int64  `json:"sinceEventId"`
				IncludeEvents  bool   `json:"includeEvents"`
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
			progress := mcp.ProgressFromContext(ctx)
			res, err := d.sched.WaitEvents(ctx, a.BatchID, a.SinceEventID,
				time.Duration(a.TimeoutSeconds)*time.Second,
				func(done, total int) {
					progress(float64(done), float64(total), fmt.Sprintf("%d/%d 任务已终结", done, total))
				})
			if err != nil {
				return nil, err
			}
			return withEvents(res, a.IncludeEvents || a.SinceEventID > 0), nil
		},
	})

	s.Register(mcp.Tool{
		Name:        "jq_batch_events",
		Description: "增量读取批次事件（job.started/done/failed/timeout、batch.done）。用 nextEventId 作为下次的 sinceEventId。waitSeconds>0 且暂无新事件时做长轮询，避免客户端 sleep。",
		InputSchema: mcp.Obj(map[string]any{
			"batchId":      mcp.Str("批次 ID"),
			"sinceEventId": mcp.Int("只需返回 eventId 大于该值的事件（默认 0）"),
			"waitSeconds":  mcp.Int("无新事件时最长阻塞秒数（默认 0=立即返回）"),
			"limit":        mcp.Int("本次最多返回条数（默认 200）"),
		}, "batchId"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				BatchID      string `json:"batchId"`
				SinceEventID int64  `json:"sinceEventId"`
				WaitSeconds  int    `json:"waitSeconds"`
				Limit        int    `json:"limit"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.BatchID == "" {
				return nil, apierr.Usage("batchId 不能为空")
			}
			if a.SinceEventID < 0 {
				a.SinceEventID = 0
			}
			if a.Limit <= 0 {
				a.Limit = 200
			}
			if a.Limit > 500 {
				a.Limit = 500
			}
			if a.WaitSeconds > 0 {
				progress := mcp.ProgressFromContext(ctx)
				res, err := d.sched.NextEvents(ctx, a.BatchID, a.SinceEventID,
					time.Duration(a.WaitSeconds)*time.Second, a.Limit)
				if err != nil {
					return nil, err
				}
				progress(float64(len(res.Events)), float64(a.Limit), "事件轮询返回")
				return withEvents(res, true), nil
			}
			evs, next, more, err := d.sched.Events(a.BatchID, a.SinceEventID, a.Limit)
			if err != nil {
				return nil, err
			}
			b, err := d.sched.Status(a.BatchID)
			if err != nil {
				return nil, err
			}
			return withEvents(&sched.WaitResult{
				Batch: b, Events: evs, NextEventID: next,
				Done: allTerminalBatch(b), HasMore: more,
			}, true), nil
		},
	})
}

func validateCallback(raw, on string) error {
	switch on {
	case "", "batch", "job", "both":
	default:
		return apierr.Usage("callbackOn 必须是 batch、job 或 both")
	}
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return apierr.Usage("callbackUrl 必须是合法的 http/https URL")
	}
	return nil
}

func withCounts(b *sched.Batch) map[string]any {
	counts := map[string]int{}
	for _, js := range b.Jobs {
		counts[js.Status]++
	}
	out := map[string]any{
		"batchId": b.ID,
		"total":   len(b.Jobs),
		"counts":  counts,
		"done":    allTerminalBatch(b),
		"jobs":    b.Jobs,
	}
	if b.Callback != nil {
		out["callback"] = map[string]any{
			"url":    b.Callback.URL,
			"on":     b.Callback.On,
			"signed": b.Callback.Secret != "",
		}
	}
	return out
}

func withEvents(r *sched.WaitResult, includeEvents bool) map[string]any {
	out := withCounts(r.Batch)
	if includeEvents {
		out["events"] = r.Events
	}
	out["newEventCount"] = len(r.Events)
	out["nextEventId"] = r.NextEventID
	out["done"] = r.Done
	if r.HasMore {
		out["hasMore"] = true
	}
	return out
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
