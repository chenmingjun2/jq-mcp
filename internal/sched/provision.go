package sched

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jqhelper/jq-mcp/internal/apierr"
	"github.com/jqhelper/jq-mcp/internal/jq"
)

// provisioner keeps one reusable, prefixed strategy copy per (account, key).
// The local code is authoritative; the cloud copy is only a backtest vehicle.
type provisioner struct {
	mu    sync.Mutex
	dir   string
	locks map[string]*sync.Mutex
	cache map[string]map[string]provEntry
	log   *slog.Logger
}

type provEntry struct {
	StrategyID string    `json:"strategyId"`
	Hash       string    `json:"hash"`
	Name       string    `json:"name"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type provFile struct {
	Version int                  `json:"version"`
	Items   map[string]provEntry `json:"items"`
}

var provNameRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func newProvisioner(log *slog.Logger) *provisioner {
	if log == nil {
		log = slog.Default()
	}
	return &provisioner{log: log, locks: map[string]*sync.Mutex{}, cache: map[string]map[string]provEntry{}}
}

func (p *provisioner) setDir(dir string) {
	p.mu.Lock()
	p.dir = dir
	p.cache = map[string]map[string]provEntry{}
	p.mu.Unlock()
}

func (p *provisioner) keyLock(key string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	l, ok := p.locks[key]
	if !ok {
		l = &sync.Mutex{}
		p.locks[key] = l
	}
	return l
}

// ensure returns the account-local strategy id for code, creating or updating
// the prefixed copy as needed. Concurrent calls for the same key serialize.
func (p *provisioner) ensure(ctx context.Context, client strategyClient, alias, key, name, code string) (string, error) {
	if client == nil {
		return "", apierr.New(apierr.CodeInternal, "策略落地缺少客户端")
	}
	// Serialize provisioning per account: the cache map is mutated here and
	// read by other keys, so a per-key lock would race on the map itself.
	lock := p.keyLock(alias)
	lock.Lock()
	defer lock.Unlock()

	hash := codeHash(code)
	items := p.load(alias)
	entry, ok := items[key]
	if ok && entry.Hash == hash && entry.StrategyID != "" {
		return entry.StrategyID, nil
	}
	if ok && entry.StrategyID != "" {
		raw, err := client.StrategySave(ctx, jq.StrategySaveInput{StrategyID: entry.StrategyID, Code: code})
		if err == nil {
			if id := parseStrategyID(raw, entry.StrategyID); id != "" {
				entry.StrategyID = id
			}
			entry.Hash = hash
			entry.UpdatedAt = time.Now()
			items[key] = entry
			p.store(alias, items)
			return entry.StrategyID, nil
		}
		if !isMissingStrategy(err) {
			return "", err
		}
		p.log.Warn("缓存策略已不存在，重新创建", "account", alias, "key", key, "id", entry.StrategyID)
		delete(items, key)
	}

	cloudName := prefixedName(name, key)
	raw, err := client.StrategyCreate(ctx, jq.StrategyCreateInput{
		Name: cloudName, Code: code, Type: "stock",
	})
	if err != nil {
		return "", err
	}
	id := parseStrategyID(raw, "")
	if id == "" {
		return "", apierr.New(apierr.CodeInternal, "创建策略副本后未返回 id")
	}
	items[key] = provEntry{StrategyID: id, Hash: hash, Name: cloudName, UpdatedAt: time.Now()}
	p.store(alias, items)
	p.log.Info("策略副本已落地", "account", alias, "key", key, "name", cloudName, "id", id)
	return id, nil
}

func (p *provisioner) load(alias string) map[string]provEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if items, ok := p.cache[alias]; ok {
		return items
	}
	items := map[string]provEntry{}
	if p.dir != "" && alias != "" {
		raw, err := os.ReadFile(filepath.Join(p.dir, alias, "strategies.json"))
		if err == nil {
			var f provFile
			if json.Unmarshal(raw, &f) == nil && f.Items != nil {
				items = f.Items
			}
		}
	}
	p.cache[alias] = items
	return items
}

func (p *provisioner) store(alias string, items map[string]provEntry) {
	p.mu.Lock()
	p.cache[alias] = items
	dir := p.dir
	p.mu.Unlock()
	if dir == "" || alias == "" {
		return
	}
	raw, err := json.MarshalIndent(provFile{Version: 1, Items: items}, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(dir, alias, "strategies.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		p.log.Warn("写入策略缓存目录失败", "err", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		p.log.Warn("写入策略缓存失败", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		p.log.Warn("保存策略缓存失败", "err", err)
	}
}

func codeHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func prefixedName(name, key string) string {
	base := name
	if base == "" {
		base = key
	}
	base = provNameRe.ReplaceAllString(strings.TrimSpace(base), "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "strategy"
	}
	if len(base) > 40 {
		base = base[:40]
	}
	sum := sha256.Sum256([]byte(key))
	return "[jq-mcp] " + base + "-" + hex.EncodeToString(sum[:3])
}

func parseStrategyID(raw json.RawMessage, fallback string) string {
	var p struct {
		ID string `json:"id"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &p) == nil && p.ID != "" {
		return p.ID
	}
	return fallback
}

func isMissingStrategy(err error) bool {
	e := apierr.From(err)
	if e.Code == apierr.CodeNotFound {
		return true
	}
	msg := strings.ToLower(e.Message)
	return strings.Contains(msg, "不存在") || strings.Contains(msg, "not found") ||
		strings.Contains(msg, "404") || strings.Contains(msg, "no such")
}

func strategyKey(js *JobState, code string) string {
	if js.StrategyKey != "" {
		return js.StrategyKey
	}
	if js.Name != "" {
		return js.Name
	}
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:6])
}

func strategyName(js *JobState, key string) string {
	if js.Name != "" {
		return js.Name
	}
	return key
}

func isQuotaError(e *apierr.Error) bool {
	if e == nil {
		return false
	}
	text := strings.ToLower(e.Message)
	for _, k := range []string{"积分", "额度", "次数", "免费", "付费", "余额", "credit", "quota", "50000"} {
		if strings.Contains(text, k) {
			return true
		}
	}
	if e.Details != nil {
		if v, ok := e.Details["creditRequired"].(bool); ok && v {
			return true
		}
		for _, v := range e.Details {
			if s, ok := v.(string); ok {
				for _, k := range []string{"积分", "额度", "credit", "quota"} {
					if strings.Contains(strings.ToLower(s), k) {
						return true
					}
				}
			}
		}
	}
	return false
}

// MarkFreeExhausted lets the synchronous tools update the same account state
// used by the scheduler after they observe a free-quota rejection.
func (s *Scheduler) MarkFreeExhausted(alias, reason string) { s.markFreeExhausted(alias, reason) }

// IsQuotaError reports whether an error looks like an exhausted free/paid
// backtest allowance. The sync backtest tools use it to ask for confirmation
// instead of surfacing a generic API error.
func IsQuotaError(err error) bool { return isQuotaError(apierr.From(err)) }

// compile-time check that the production client satisfies the narrow interface.
var _ strategyClient = (*jq.Client)(nil)

// String is handy in tests and logs.
func (p provEntry) String() string {
	return fmt.Sprintf("%s(%s)", p.Name, p.StrategyID)
}
