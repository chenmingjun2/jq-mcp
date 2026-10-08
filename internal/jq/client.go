// Package jq is the JoinQuant domain layer. Every method simply forwards a
// high-level command to the connected browser extension over the bridge; this
// package never issues an HTTP request to JoinQuant itself. That separation is
// deliberate: the browser holds the authenticated session.
package jq

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jqhelper/jq-mcp/internal/apierr"
	"github.com/jqhelper/jq-mcp/internal/bridge"
)

// Client forwards JoinQuant operations to the extension.
type Client struct {
	hub     *bridge.Hub
	timeout time.Duration
}

// New builds a client. timeout applies to each bridge round-trip.
func New(hub *bridge.Hub, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Client{hub: hub, timeout: timeout}
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.hub.Call(cctx, method, params)
}

// BridgeStatus returns the raw bridge connectivity snapshot.
func (c *Client) BridgeStatus() bridge.Status { return c.hub.Status() }

// ---------------------------------------------------------------------------
// auth
// ---------------------------------------------------------------------------

// AuthStatus checks the JoinQuant login state inside the browser.
func (c *Client) AuthStatus(ctx context.Context) (json.RawMessage, error) {
	return c.call(ctx, "auth.status", map[string]any{})
}

// Cookie returns the JoinQuant cookie header assembled by the extension.
func (c *Client) Cookie(ctx context.Context) (json.RawMessage, error) {
	return c.call(ctx, "auth.getCookie", map[string]any{})
}

// OpenLogin asks the extension to open the login page in a background tab.
func (c *Client) OpenLogin(ctx context.Context, url string) (json.RawMessage, error) {
	return c.call(ctx, "auth.openLogin", map[string]any{"url": url})
}

// Refresh reloads the JoinQuant tab to renew the session.
func (c *Client) Refresh(ctx context.Context) (json.RawMessage, error) {
	return c.call(ctx, "auth.refresh", map[string]any{})
}

// AccountLimits reads account/VIP limits such as the concurrent backtest cap.
func (c *Client) AccountLimits(ctx context.Context) (json.RawMessage, error) {
	return c.call(ctx, "account.limits", map[string]any{})
}

// ---------------------------------------------------------------------------
// strategy
// ---------------------------------------------------------------------------

// StrategyListInput filters the strategy listing.
type StrategyListInput struct {
	Fid   string `json:"fid,omitempty"`
	Limit int    `json:"limit,omitempty"`
	All   bool   `json:"all,omitempty"`
	Sort  string `json:"sort,omitempty"`
}

// StrategyCreateInput creates a strategy, optionally with initial source code.
type StrategyCreateInput struct {
	Name     string `json:"name"`
	Code     string `json:"code,omitempty"`
	Type     string `json:"type,omitempty"`
	FolderID string `json:"folderId,omitempty"`
}

// StrategySaveInput updates a strategy name and/or source code.
type StrategySaveInput struct {
	StrategyID string `json:"strategyId"`
	Name       string `json:"name,omitempty"`
	Code       string `json:"code,omitempty"`
}

// StrategyList lists strategies.
func (c *Client) StrategyList(ctx context.Context, in StrategyListInput) (json.RawMessage, error) {
	return c.call(ctx, "strategy.list", in)
}

// StrategyGet reads a strategy's metadata and, optionally, its source code.
func (c *Client) StrategyGet(ctx context.Context, strategyID string, includeCode bool) (json.RawMessage, error) {
	return c.call(ctx, "strategy.get", map[string]any{
		"strategyId":  strategyID,
		"includeCode": includeCode,
	})
}

// StrategyCreate creates a new strategy.
func (c *Client) StrategyCreate(ctx context.Context, in StrategyCreateInput) (json.RawMessage, error) {
	return c.call(ctx, "strategy.create", in)
}

// StrategySave saves name and/or code to a strategy.
func (c *Client) StrategySave(ctx context.Context, in StrategySaveInput) (json.RawMessage, error) {
	return c.call(ctx, "strategy.save", in)
}

// StrategyDelete deletes a strategy.
func (c *Client) StrategyDelete(ctx context.Context, strategyID string) (json.RawMessage, error) {
	return c.call(ctx, "strategy.delete", map[string]any{"strategyId": strategyID})
}

// StrategyFind locates strategies by name.
func (c *Client) StrategyFind(ctx context.Context, name string) (json.RawMessage, error) {
	return c.call(ctx, "strategy.find", map[string]any{"name": name})
}

// StrategyMove moves strategies into a folder.
func (c *Client) StrategyMove(ctx context.Context, strategyIDs []string, folderID string) (json.RawMessage, error) {
	return c.call(ctx, "strategy.move", map[string]any{
		"strategyIds": strategyIDs,
		"folderId":    folderID,
	})
}

// FolderList lists strategy folders.
func (c *Client) FolderList(ctx context.Context, fid string, recursive bool) (json.RawMessage, error) {
	return c.call(ctx, "folder.list", map[string]any{"fid": fid, "recursive": recursive})
}

// FolderCreate creates a strategy folder.
func (c *Client) FolderCreate(ctx context.Context, name, parentID string) (json.RawMessage, error) {
	return c.call(ctx, "folder.create", map[string]any{"name": name, "parentId": parentID})
}

// ---------------------------------------------------------------------------
// backtest
// ---------------------------------------------------------------------------

// BacktestRunInput parameterizes a backtest or compile-only run.
type BacktestRunInput struct {
	StrategyID string  `json:"strategyId"`
	Start      string  `json:"start"`
	End        string  `json:"end,omitempty"`
	Capital    float64 `json:"capital,omitempty"`
	Frequency  string  `json:"frequency,omitempty"`
	Compile    bool    `json:"compile,omitempty"`
}

// BacktestRunResult is the identifiers returned by a submitted run.
type BacktestRunResult struct {
	ID     string `json:"id"`
	ListID string `json:"listId"`
	Status string `json:"status"`
}

// BacktestListInput filters backtest records.
type BacktestListInput struct {
	StrategyID string `json:"strategyId"`
	Status     string `json:"status,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Compile    bool   `json:"compile,omitempty"`
}

// BacktestLogsInput parameterizes log retrieval.
type BacktestLogsInput struct {
	BacktestID string `json:"backtestId"`
	Offset     int    `json:"offset,omitempty"`
	Error      bool   `json:"error,omitempty"`
	All        bool   `json:"all,omitempty"`
}

// BacktestRun submits a backtest and returns its identifiers.
func (c *Client) BacktestRun(ctx context.Context, in BacktestRunInput) (BacktestRunResult, error) {
	var out BacktestRunResult
	raw, err := c.call(ctx, "backtest.run", in)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, apierr.Wrap(apierr.CodeInternal, err)
	}
	return out, nil
}

// BacktestList lists backtest records for a strategy.
func (c *Client) BacktestList(ctx context.Context, in BacktestListInput) (json.RawMessage, error) {
	return c.call(ctx, "backtest.list", in)
}

// BacktestGet reads detail, metrics and source for a backtest.
func (c *Client) BacktestGet(ctx context.Context, backtestID string) (json.RawMessage, error) {
	return c.call(ctx, "backtest.get", map[string]any{"backtestId": backtestID})
}

// BacktestStats reads return/risk metrics.
func (c *Client) BacktestStats(ctx context.Context, backtestID string) (json.RawMessage, error) {
	return c.call(ctx, "backtest.stats", map[string]any{"backtestId": backtestID})
}

// BacktestResult reads the return curve data.
func (c *Client) BacktestResult(ctx context.Context, backtestID string, offset, userRecordOffset int) (json.RawMessage, error) {
	return c.call(ctx, "backtest.result", map[string]any{
		"backtestId":       backtestID,
		"offset":           offset,
		"userRecordOffset": userRecordOffset,
	})
}

// BacktestLogs reads run logs or error logs.
func (c *Client) BacktestLogs(ctx context.Context, in BacktestLogsInput) (json.RawMessage, error) {
	return c.call(ctx, "backtest.logs", in)
}

// BacktestDelete deletes a backtest record.
func (c *Client) BacktestDelete(ctx context.Context, backtestID string, compile bool) (json.RawMessage, error) {
	return c.call(ctx, "backtest.delete", map[string]any{
		"backtestId": backtestID,
		"compile":    compile,
	})
}

// BacktestCancel cancels a running backtest/compile.
func (c *Client) BacktestCancel(ctx context.Context, backtestID string) (json.RawMessage, error) {
	return c.call(ctx, "backtest.cancel", map[string]any{"backtestId": backtestID})
}

// BacktestTrades reads the trade (transaction) detail of a backtest.
func (c *Client) BacktestTrades(ctx context.Context, backtestID string) (json.RawMessage, error) {
	return c.call(ctx, "backtest.trades", map[string]any{"backtestId": backtestID})
}

// BacktestPositions reads the daily position detail of a backtest.
func (c *Client) BacktestPositions(ctx context.Context, backtestID string) (json.RawMessage, error) {
	return c.call(ctx, "backtest.positions", map[string]any{"backtestId": backtestID})
}

// BacktestMonthly reads period/monthly return risk data.
func (c *Client) BacktestMonthly(ctx context.Context, backtestID string) (json.RawMessage, error) {
	return c.call(ctx, "backtest.monthly", map[string]any{"backtestId": backtestID})
}
