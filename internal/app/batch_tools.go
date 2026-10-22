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
		Description: "把回测任务提交到服务端调度器（不阻塞）。带 code/codePath 的任务可自动落到任意账号（云端会创建 [jq-mcp] 前缀副本）；带 strategyId 的任务钉在指定或默认账号。返回 batchId，用 jq_batch_wait/jq_batch_events 取结果。可选 callbackUrl：批次/任务终结时服务端会 POST 事件。",
		InputSchema: mcp.Obj(map[string]any{
			"jobs": mcp.Arr("任务列表", mcp.Obj(map[string]any{
				"tag":         mcp.Str("自定义标识（回显在结果里）"),
				"strategyId":  mcp.Str("策略 ID（账号私有；提供后视为钉住账号）"),
				"strategyKey": mcp.Str("本地策略键，用于复用云端副本（默认取 name 或代码 hash）"),
				"name":        mcp.Str("云端策略名，默认与 strategyKey 相同"),
				"code":        mcp.Str("策略源码；与 codePath 二选一"),
				"codePath":    mcp.Str("本地策略文件路径"),
				"account":     mcp.Str("指定账号别名（钉住，不迁移）"),
				"placement":   mcp.StrEnum("放置方式", "auto", "pinned", "sticky"),
				"start":       mcp.Str("开始日期 YYYY-MM-DD"),
				"end":         mcp.Str("结束日期 YYYY-MM-DD"),
				"capital":     mcp.Num("初始资金"),
				"frequency":   mcp.StrEnum("频率", "day", "minute"),
				"compile":     mcp.Bool("是否编译运行"),
				"useCredit":   mcp.Bool("是否可用积分续跑"),
				"priority":    mcp.Int("优先级（小=先跑）"),
			})),
			"account":        mcp.Str("可选：给所有未指定账号的 job 统一钉住该账号"),
			"placement":      mcp.StrEnum("默认放置方式", "auto", "pinned", "sticky"),
			"maxConcurrent":  mcp.Int("覆盖并发上限（指定 account 时只改该账号，否则改默认值）"),
			"workers":        mcp.Int("工作线程数（0=默认）"),
			"callbackUrl":    mcp.Str("可选：事件回调地址（http/https），异步 POST，失败自动重试"),
			"callbackSecret": mcp.Str("可选：HMAC-SHA256 密钥；设置后请求带 X-JQ-Signature"),
			"callbackOn":     mcp.StrEnum("回调范围，默认 batch（只回调批次完成）", "batch", "job", "both"),
		}, "jobs"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Jobs           []sched.Job `json:"jobs"`
				Account        string      `json:"account"`
				Placement      string      `json:"placement"`
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
				j := &a.Jobs[i]
				if j.Start == "" {
					return nil, apierr.Usage("每个 job 需要 start")
				}
				if j.StrategyID == "" && j.Code == "" && j.CodePath == "" {
					return nil, apierr.Usage("每个 job 需要 strategyId，或 code/codePath 之一")
				}
				if j.Account == "" {
					j.Account = a.Account
				}
				if j.Placement == "" {
					j.Placement = a.Placement
				}
				switch j.Placement {
				case "", "auto", "pinned", "sticky":
				default:
					return nil, apierr.Usage("placement 必须是 auto、pinned 或 sticky")
				}
				if j.Placement == "pinned" && j.Account == "" {
					return nil, apierr.Usage("placement=pinned 时必须指定 account")
				}
				if j.Account != "" && d.accounts != nil {
					if _, ok := d.accounts.Get(j.Account); !ok {
						return nil, apierr.Usage("账号 %s 不存在，先执行 jq-mcp accounts add", j.Account)
					}
				}
			}
			if err := validateCallback(a.CallbackURL, a.CallbackOn); err != nil {
				return nil, err
			}
			if a.MaxConcurrent > 0 {
				if a.Account != "" {
					d.sched.SetCapFor(a.Account, a.MaxConcurrent)
				} else {
					d.sched.SetCap(a.MaxConcurrent)
				}
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
				"batchId":     snap.ID,
				"count":       len(snap.Jobs),
				"status":      "submitted",
				"cap":         d.sched.Cap(),
				"accountCaps": accountCaps(d.sched),
				"jobs":        snap.Jobs,
				"createdAt":   snap.CreatedAt,
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
				return map[string]any{"batches": d.sched.List(a.Limit), "cap": d.sched.Cap(), "accountCaps": accountCaps(d.sched)}, nil
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
				res.NeedsConfirmation = pendingCreditJobs(res.Batch)
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
				NeedsConfirmation: pendingCreditJobs(b),
				Done:              allTerminalBatch(b), HasMore: more,
			}, true), nil
		},
	})
	s.Register(mcp.Tool{
		Name:        "jq_batch_confirm_credit",
		Description: "免费回测额度用完时，批次会暂停并返回 needsConfirmation。确认后传 approve=true、confirm=true 消耗积分重试；传 approve=false 则把待确认任务置为失败。",
		InputSchema: mcp.Obj(map[string]any{
			"batchId": mcp.Str("批次 ID"),
			"approve": mcp.Bool("true=允许使用积分重试；false=拒绝（任务失败）"),
			"tags":    mcp.Arr("可选：只处理这些 tag", mcp.Str("job tag")),
			"confirm": mcp.Bool("必须为 true"),
		}, "batchId", "confirm"),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				BatchID string     `json:"batchId"`
				Approve bool       `json:"approve"`
				Tags    stringList `json:"tags"`
				Confirm bool       `json:"confirm"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.BatchID == "" {
				return nil, apierr.Usage("batchId 不能为空")
			}
			if err := requireConfirm(a.Confirm, "jq_batch_confirm_credit"); err != nil {
				return nil, err
			}
			snap, err := d.sched.ConfirmCredit(a.BatchID, a.Approve, a.Tags)
			if err != nil {
				return nil, err
			}
			out := withCounts(snap)
			out["approved"] = a.Approve
			if a.Approve {
				out["hint"] = "积分重试已入队，继续调用 jq_batch_wait"
			}
			return out, nil
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

func accountCaps(s *sched.Scheduler) map[string]int {
	out := map[string]int{}
	for _, a := range s.Accounts() {
		out[a.Alias] = a.Cap
	}
	return out
}

func withCounts(b *sched.Batch) map[string]any {
	counts := map[string]int{}
	byAccount := map[string]int{}
	for _, js := range b.Jobs {
		counts[js.Status]++
		acct := js.Account
		if acct == "" {
			acct = js.CreditAccount
		}
		if acct != "" {
			byAccount[acct]++
		}
	}
	out := map[string]any{
		"batchId":         b.ID,
		"total":           len(b.Jobs),
		"counts":          counts,
		"countsByAccount": byAccount,
		"done":            allTerminalBatch(b),
		"jobs":            b.Jobs,
	}
	if b.Callback != nil {
		out["callback"] = map[string]any{
			"url":    b.Callback.URL,
			"on":     b.Callback.On,
			"signed": b.Callback.Secret != "",
		}
	}
	if pending := pendingCreditJobs(b); len(pending) > 0 {
		out["needsConfirmation"] = pending
		out["creditRequired"] = true
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
	if len(r.NeedsConfirmation) > 0 {
		out["needsConfirmation"] = r.NeedsConfirmation
		out["creditRequired"] = true
	}
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

func pendingCreditJobs(b *sched.Batch) []*sched.JobState {
	var out []*sched.JobState
	for _, js := range b.Jobs {
		if js.Status == sched.StNeedsCredit {
			out = append(out, js)
		}
	}
	return out
}
