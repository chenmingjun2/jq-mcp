package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jqhelper/jq-mcp/internal/account"
	"github.com/jqhelper/jq-mcp/internal/bridge"
	"github.com/jqhelper/jq-mcp/internal/jq"
	"github.com/jqhelper/jq-mcp/internal/sched"
	"github.com/jqhelper/jq-mcp/internal/task"
)

// poolRuntime connects the bridge, the account registry and the scheduler:
// it probes identities, binds uids to aliases and keeps runtime state fresh.
type poolRuntime struct {
	reg   *account.Registry
	hub   *bridge.Hub
	jq    *jq.Client
	sch   *sched.Scheduler
	tasks *task.Manager
	log   *slog.Logger

	defaultCap    int
	maxConcurrent int

	mu      sync.Mutex
	regMu   sync.RWMutex
	probing map[string]bool
	regPath string
	regMod  time.Time
}

func newPoolRuntime(reg *account.Registry, hub *bridge.Hub, client *jq.Client, sch *sched.Scheduler, tasks *task.Manager, defaultCap, maxConcurrent int, log *slog.Logger) *poolRuntime {
	if log == nil {
		log = slog.Default()
	}
	if defaultCap <= 0 {
		defaultCap = 2
	}
	return &poolRuntime{
		reg: reg, hub: hub, jq: client, sch: sch, tasks: tasks, log: log,
		defaultCap: defaultCap, maxConcurrent: maxConcurrent,
		probing: map[string]bool{}, regPath: reg.Path(),
	}
}

// start wires the bridge lifecycle and publishes the registry snapshot.
func (p *poolRuntime) start(ctx context.Context) {
	p.sync()
	p.hub.SetClientHandler(p.onClient)
	if st, err := os.Stat(p.regPath); err == nil {
		p.regMod = st.ModTime()
	}
	go p.watchRegistry(ctx)
}

func (p *poolRuntime) registry() *account.Registry {
	p.regMu.RLock()
	defer p.regMu.RUnlock()
	return p.reg
}

// watchRegistry reloads accounts.json when it changes, so `jq-mcp accounts cap`
// / `add` take effect without restarting the daemon.
func (p *poolRuntime) watchRegistry(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	health := time.NewTicker(15 * time.Second)
	defer health.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-health.C:
			p.healthCheck()
		case <-ticker.C:
		}
		st, err := os.Stat(p.regPath)
		if err != nil {
			continue
		}
		if !st.ModTime().After(p.regMod) {
			continue
		}
		p.regMod = st.ModTime()
		p.reload()
	}
}

// healthCheck re-probes accounts that are connected but quarantined, so a user
// who logs back in (or renews an expired session) is picked up without a
// daemon restart.
func (p *poolRuntime) healthCheck() {
	views := map[string]sched.AccountView{}
	for _, v := range p.sch.Accounts() {
		views[v.Alias] = v
	}
	for _, c := range p.hub.Clients() {
		if c.Stale || c.UID == "" || c.Alias == "" {
			continue
		}
		switch views[c.Alias].State {
		case sched.AcctLoggedOut, sched.AcctPending:
			p.probeAsync(c)
		}
	}
}

// reload re-reads accounts.json and republishes the scheduler snapshot.
func (p *poolRuntime) reload() {
	reg, err := account.Load(p.regPath)
	if err != nil {
		p.log.Warn("重载账号注册表失败", "err", err)
		return
	}
	p.regMu.Lock()
	p.reg = reg
	p.regMu.Unlock()
	p.sync()
	p.log.Info("账号注册表已重载")
}

func (p *poolRuntime) onClient(info bridge.ClientInfo) {
	if !info.Connected {
		p.onDisconnect(info)
		return
	}
	p.sync()
	p.probeAsync(info)
}

func (p *poolRuntime) onDisconnect(info bridge.ClientInfo) {
	alias := info.Alias
	if alias == "" && info.UID != "" {
		if a, ok := p.registry().ByUID(info.UID); ok {
			alias = a.Alias
		}
	}
	// A duplicate connection may have replaced this one; do not mark the
	// account offline while a healthy replacement is still attached.
	for _, c := range p.hub.Clients() {
		if c.ID == info.ID || c.Stale || !c.Connected {
			continue
		}
		if (info.UID != "" && c.UID == info.UID) || (alias != "" && c.Alias == alias) {
			return
		}
	}
	if alias == "" {
		p.sync()
		return
	}
	p.sync()
	p.sch.MarkAccountDisconnected(alias)
	p.log.Warn("账号连接断开", "account", alias)
}

func (p *poolRuntime) probeAsync(info bridge.ClientInfo) {
	p.mu.Lock()
	if p.probing[info.ID] {
		p.mu.Unlock()
		return
	}
	p.probing[info.ID] = true
	p.mu.Unlock()
	go func() {
		defer func() {
			p.mu.Lock()
			delete(p.probing, info.ID)
			p.mu.Unlock()
		}()
		p.probe(info)
	}()
}

func (p *poolRuntime) probe(info bridge.ClientInfo) {
	delay := 200 * time.Millisecond
	attempt := 0
	for {
		if !p.clientAlive(info.ID) {
			return
		}
		time.Sleep(delay)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		identity, err := p.jq.IdentityOn(ctx, info.ID)
		cancel()
		if err != nil {
			if attempt%20 == 0 {
				p.log.Warn("账号身份探测失败，稍后重试", "alias", info.Alias, "id", info.ID, "err", err)
			}
			attempt++
			delay = 15 * time.Second
			continue
		}
		if !identity.LoggedIn || identity.UID == "" {
			if attempt%20 == 0 {
				p.log.Info("账号尚未登录，等待用户完成登录", "alias", info.Alias, "id", info.ID)
			}
			attempt++
			delay = 15 * time.Second
			continue
		}
		acc, err := p.registry().BindConnection(info.Alias, identity.UID, identity.Nickname)
		if err != nil {
			p.log.Error("账号绑定失败", "alias", info.Alias, "uid", shortUID(identity.UID), "err", err)
			return
		}
		if err := p.hub.BindAccount(info.ID, acc.Alias, identity.UID, identity.Nickname); err != nil {
			p.log.Error("绑定桥连接失败", "account", acc.Alias, "err", err)
			return
		}
		p.sync()
		p.sch.SetAccountIdentity(acc.Alias, identity.UID, identity.Nickname, identity.Credits, identity.CreditsKnown)
		p.log.Info("账号已就绪", "account", acc.Alias, "nickname", identity.Nickname,
			"credits", identity.Credits, "creditsKnown", identity.CreditsKnown)
		return
	}
}

func (p *poolRuntime) clientAlive(id string) bool {
	for _, c := range p.hub.Clients() {
		if c.ID == id {
			return true
		}
	}
	return false
}

// sync rebuilds the scheduler's account set from the registry + live clients.
func (p *poolRuntime) sync() {
	accounts := p.registry().List()
	clients := p.hub.Clients()
	rts := make([]sched.AccountRuntime, 0, len(accounts))
	for _, a := range accounts {
		var live *bridge.ClientInfo
		for i := range clients {
			c := clients[i]
			if c.Stale {
				continue
			}
			if (a.UID != "" && c.UID == a.UID) || (c.Alias != "" && c.Alias == a.Alias) {
				live = &clients[i]
				break
			}
		}
		cap := a.Cap
		if cap <= 0 {
			if len(accounts) == 1 {
				// Preserve the single-account behaviour (--max-concurrent).
				cap = p.maxConcurrent
			} else {
				cap = p.defaultCap
			}
		}
		rts = append(rts, sched.AccountRuntime{
			Alias: a.Alias, UID: a.UID, Nickname: a.Nickname,
			Enabled: a.Enabled, Weight: a.Weight, Cap: cap,
			Connected: live != nil, LoggedIn: live != nil && live.UID != "",
			Tags: a.Tags,
		})
	}
	p.sch.SyncAccounts(rts)
	if def := p.registry().DefaultAlias(); def != "" {
		p.sch.SetDefaultAccount(def)
		p.hub.SetDefaultAccount(def)
	}
}

// snapshot returns the jq_accounts payload.
func (p *poolRuntime) snapshot() map[string]any {
	return map[string]any{
		"default":   p.sch.DefaultAccount(),
		"cap":       p.sch.Cap(),
		"accounts":  p.sch.Accounts(),
		"connected": p.hub.Connected(),
		"clients":   p.hub.Clients(),
	}
}

// resolveAccount routes read-only calls by the backtest id they carry. The
// scheduler/task manager recorded the owning account at submit time.
func (p *poolRuntime) resolveAccount(method string, params json.RawMessage) string {
	if !strings.HasPrefix(method, "backtest.") {
		return ""
	}
	var probe struct {
		BacktestID string `json:"backtestId"`
	}
	if len(params) > 0 && json.Unmarshal(params, &probe) == nil && probe.BacktestID != "" {
		if t := p.tasks.FindByRemoteID(probe.BacktestID); t != nil && t.Account != "" {
			return t.Account
		}
		if alias := p.sch.AccountForRemote(probe.BacktestID); alias != "" {
			return alias
		}
	}
	return ""
}

func (p *poolRuntime) defaultAccount() string { return p.sch.DefaultAccount() }

func shortUID(uid string) string {
	if len(uid) <= 6 {
		return uid
	}
	return uid[:6] + "…"
}
