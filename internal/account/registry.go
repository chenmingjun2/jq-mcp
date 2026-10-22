// Package account stores the pool's account registry: which browser profiles
// exist, which JoinQuant uid each one is bound to, and per-account policy
// (enabled, weight, concurrency cap).
//
// It deliberately contains no runtime state. Connections, in-flight counts and
// cooldowns live in the scheduler; this file only holds things that survive a
// daemon restart. JoinQuant cookies are never stored here — they stay in the
// browser profile.
package account

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Unregistered modes control what happens when an unknown uid connects.
const (
	UnregisteredReject = "reject" // only accounts already in accounts.json may join
	UnregisteredAccept = "accept" // auto-create an entry for the connection
)

// Account is one pool member. UID is the JoinQuant userId (stable hash), not the
// phone number or nickname.
type Account struct {
	Alias      string    `json:"alias"`
	UID        string    `json:"uid,omitempty"`
	Nickname   string    `json:"nickname,omitempty"`
	Enabled    bool      `json:"enabled"`
	Weight     int       `json:"weight,omitempty"`
	Cap        int       `json:"cap,omitempty"` // 0 = pool default
	ProfileDir string    `json:"profileDir,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	CreatedAt  time.Time `json:"createdAt,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt,omitempty"`
}

// File is the on-disk schema.
type File struct {
	Version      int       `json:"version"`
	Default      string    `json:"default,omitempty"`
	Unregistered string    `json:"unregistered,omitempty"`
	Accounts     []Account `json:"accounts"`
}

// Registry is a concurrency-safe view of accounts.json.
type Registry struct {
	mu   sync.Mutex
	path string
	data File
}

var aliasRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// New returns an empty registry. Call Load to read an existing file.
func New(path string) *Registry {
	return &Registry{
		path: path,
		data: File{Version: 1, Unregistered: UnregisteredReject},
	}
}

// Load reads path. A missing file yields an empty registry, not an error.
func Load(path string) (*Registry, error) {
	r := New(path)
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &r.data); err != nil {
			return nil, fmt.Errorf("解析账号注册表失败: %w", err)
		}
	case os.IsNotExist(err):
		return r, nil
	default:
		return nil, fmt.Errorf("读取账号注册表失败: %w", err)
	}
	if r.data.Version == 0 {
		r.data.Version = 1
	}
	if r.data.Unregistered == "" {
		r.data.Unregistered = UnregisteredReject
	}
	for i := range r.data.Accounts {
		r.normalizeLocked(&r.data.Accounts[i])
	}
	return r, nil
}

// Path returns the backing file path.
func (r *Registry) Path() string { return r.path }

// Snapshot returns a deep copy of the registry contents.
func (r *Registry) Snapshot() File {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.data
	out.Accounts = append([]Account(nil), r.data.Accounts...)
	for i := range out.Accounts {
		out.Accounts[i].Tags = append([]string(nil), out.Accounts[i].Tags...)
	}
	return out
}

// List returns accounts sorted by alias for stable output.
func (r *Registry) List() []Account {
	snap := r.Snapshot()
	sort.Slice(snap.Accounts, func(i, j int) bool { return snap.Accounts[i].Alias < snap.Accounts[j].Alias })
	return snap.Accounts
}

// Get returns one account by alias.
func (r *Registry) Get(alias string) (Account, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.data.Accounts {
		if a.Alias == alias {
			return a, true
		}
	}
	return Account{}, false
}

// ByUID returns the account bound to a JoinQuant uid.
func (r *Registry) ByUID(uid string) (Account, bool) {
	if uid == "" {
		return Account{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.data.Accounts {
		if a.UID == uid {
			return a, true
		}
	}
	return Account{}, false
}

// DefaultAlias returns the configured default, falling back to the first
// enabled account.
func (r *Registry) DefaultAlias() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.defaultLocked()
}

func (r *Registry) defaultLocked() string {
	if r.data.Default != "" {
		for _, a := range r.data.Accounts {
			if a.Alias == r.data.Default && a.Enabled {
				return a.Alias
			}
		}
	}
	for _, a := range r.data.Accounts {
		if a.Enabled {
			return a.Alias
		}
	}
	return ""
}

// Add creates an empty profile entry. It fails if the alias exists.
func (r *Registry) Add(alias string, cap int) (Account, error) {
	alias, err := normalizeAlias(alias)
	if err != nil {
		return Account{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.data.Accounts {
		if a.Alias == alias {
			return Account{}, fmt.Errorf("账号 %s 已存在", alias)
		}
	}
	now := time.Now()
	acc := Account{
		Alias:      alias,
		Enabled:    true,
		Weight:     1,
		Cap:        cap,
		ProfileDir: filepath.Join("profiles", alias),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	r.data.Accounts = append(r.data.Accounts, acc)
	if r.data.Default == "" {
		r.data.Default = alias
	}
	if err := r.saveLocked(); err != nil {
		return Account{}, err
	}
	return acc, nil
}

// Remove deletes an account entry. It does not touch the browser profile.
func (r *Registry) Remove(alias string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, a := range r.data.Accounts {
		if a.Alias != alias {
			continue
		}
		r.data.Accounts = append(r.data.Accounts[:i], r.data.Accounts[i+1:]...)
		if r.data.Default == alias {
			r.data.Default = ""
		}
		return r.saveLocked()
	}
	return fmt.Errorf("账号 %s 不存在", alias)
}

// SetEnabled toggles scheduling for an account.
func (r *Registry) SetEnabled(alias string, enabled bool) error {
	return r.mutate(alias, func(a *Account) { a.Enabled = enabled })
}

// SetCap sets a per-account cap override (0 means use the pool default).
func (r *Registry) SetCap(alias string, cap int) error {
	return r.mutate(alias, func(a *Account) {
		if cap < 0 {
			cap = 0
		}
		a.Cap = cap
	})
}

// SetDefault marks alias as the default account.
func (r *Registry) SetDefault(alias string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	found := false
	for _, a := range r.data.Accounts {
		if a.Alias == alias {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("账号 %s 不存在", alias)
	}
	r.data.Default = alias
	return r.saveLocked()
}

// Bind connects an alias to a uid. An alias already bound to another uid is a
// hard conflict: silently rebinding would route jobs to the wrong account.
func (r *Registry) Bind(alias, uid, nickname string) (Account, error) {
	if uid == "" {
		return Account{}, fmt.Errorf("uid 不能为空")
	}
	alias, err := normalizeAlias(alias)
	if err != nil {
		return Account{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.data.Accounts {
		if a.UID == uid && a.Alias != alias {
			return a, fmt.Errorf("uid %s 已绑定到账号 %s，不能重复绑定到 %s", shortUID(uid), a.Alias, alias)
		}
	}
	for i := range r.data.Accounts {
		a := &r.data.Accounts[i]
		if a.Alias != alias {
			continue
		}
		if a.UID != "" && a.UID != uid {
			return *a, fmt.Errorf("账号 %s 已绑定其他 uid (%s)，需要先解绑", alias, shortUID(a.UID))
		}
		a.UID = uid
		if nickname != "" {
			a.Nickname = nickname
		}
		a.UpdatedAt = time.Now()
		if err := r.saveLocked(); err != nil {
			return *a, err
		}
		return *a, nil
	}
	return Account{}, fmt.Errorf("账号 %s 不存在", alias)
}

// BindConnection maps a freshly identified connection to an account. It accepts
// a hint from the extension's bootstrap config ("alias"), but the uid is the
// authority:
//
//   - the uid already exists  -> reuse that account (ignore a different hint);
//   - the hinted alias is free -> bind it;
//   - the hinted alias is bound to another uid -> conflict;
//   - no hint -> accept an auto alias when Unregistered=accept, otherwise fail.
//
// A completely empty registry always bootstraps the first account so existing
// single-account installs upgrade without configuration.
func (r *Registry) BindConnection(hint, uid, nickname string) (Account, error) {
	if uid == "" {
		return Account{}, fmt.Errorf("uid 不能为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, a := range r.data.Accounts {
		if a.UID == uid {
			return r.touchLocked(a.Alias, nickname)
		}
	}

	bootstrap := len(r.data.Accounts) == 0
	hint = strings.TrimSpace(hint)
	if valid, err := normalizeAlias(hint); err == nil {
		hint = valid
	} else {
		hint = ""
	}
	if hint != "" {
		if found, ok := r.findLocked(hint); ok {
			if found.UID != "" {
				if nickname != "" && found.Nickname == nickname {
					found.UID = uid
					found.UpdatedAt = time.Now()
					if err := r.saveLocked(); err != nil {
						return *found, err
					}
					return *found, nil
				}
				return *found, fmt.Errorf("账号 %s 已绑定其他 uid (%s)", hint, shortUID(found.UID))
			}
			found.UID = uid
			if nickname != "" {
				found.Nickname = nickname
			}
			found.UpdatedAt = time.Now()
			if r.data.Default == "" {
				r.data.Default = found.Alias
			}
			if err := r.saveLocked(); err != nil {
				return *found, err
			}
			return *found, nil
		}
		if bootstrap || r.data.Unregistered == UnregisteredAccept {
			now := time.Now()
			acc := Account{Alias: hint, UID: uid, Nickname: nickname, Enabled: true, Weight: 1,
				ProfileDir: filepath.Join("profiles", hint), CreatedAt: now, UpdatedAt: now}
			r.data.Accounts = append(r.data.Accounts, acc)
			if r.data.Default == "" {
				r.data.Default = hint
			}
			if err := r.saveLocked(); err != nil {
				return acc, err
			}
			return acc, nil
		}
		return Account{}, fmt.Errorf("连接自报别名 %q 未注册，且未知 uid %s 不在注册表中", hint, shortUID(uid))
	}

	// Upgrade path: 0.4.0 switched the binding key from JoinQuant's
	// session-scoped userId to the stable uniqueKey. A single-account install
	// can safely migrate when the nickname matches.
	if len(r.data.Accounts) == 1 && nickname != "" {
		a := &r.data.Accounts[0]
		if a.UID != "" && a.Nickname == nickname {
			a.UID = uid
			a.UpdatedAt = time.Now()
			if err := r.saveLocked(); err != nil {
				return *a, err
			}
			return *a, nil
		}
	}

	if !bootstrap && r.data.Unregistered != UnregisteredAccept {
		return Account{}, fmt.Errorf("未知账号 uid %s 未注册（unregistered=reject）", shortUID(uid))
	}
	alias := "acct-" + shortUID(uid)
	if bootstrap {
		alias = "default"
	}
	for i := 0; ; i++ {
		candidate := alias
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", alias, i)
		}
		if _, ok := r.findLocked(candidate); !ok {
			alias = candidate
			break
		}
	}
	now := time.Now()
	acc := Account{Alias: alias, UID: uid, Nickname: nickname, Enabled: true, Weight: 1,
		ProfileDir: filepath.Join("profiles", alias), CreatedAt: now, UpdatedAt: now}
	r.data.Accounts = append(r.data.Accounts, acc)
	if r.data.Default == "" {
		r.data.Default = alias
	}
	if err := r.saveLocked(); err != nil {
		return acc, err
	}
	return acc, nil
}

func (r *Registry) touchLocked(alias, nickname string) (Account, error) {
	a, ok := r.findLocked(alias)
	if !ok {
		return Account{}, fmt.Errorf("账号 %s 不存在", alias)
	}
	if nickname != "" && a.Nickname != nickname {
		a.Nickname = nickname
		a.UpdatedAt = time.Now()
		if err := r.saveLocked(); err != nil {
			return *a, err
		}
	}
	return *a, nil
}

func (r *Registry) findLocked(alias string) (*Account, bool) {
	for i := range r.data.Accounts {
		if r.data.Accounts[i].Alias == alias {
			return &r.data.Accounts[i], true
		}
	}
	return nil, false
}

func (r *Registry) mutate(alias string, fn func(*Account)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.findLocked(alias)
	if !ok {
		return fmt.Errorf("账号 %s 不存在", alias)
	}
	fn(a)
	a.UpdatedAt = time.Now()
	return r.saveLocked()
}

func (r *Registry) normalizeLocked(a *Account) {
	if a.Weight <= 0 {
		a.Weight = 1
	}
	if a.ProfileDir == "" {
		a.ProfileDir = filepath.Join("profiles", a.Alias)
	}
	if a.Cap < 0 {
		a.Cap = 0
	}
}

// Save persists the registry atomically with 0600 permissions.
func (r *Registry) Save() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveLocked()
}

func (r *Registry) saveLocked() error {
	if r.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("创建账号目录失败: %w", err)
	}
	raw, err := json.MarshalIndent(r.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("写入账号注册表失败: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("保存账号注册表失败: %w", err)
	}
	return nil
}

func normalizeAlias(alias string) (string, error) {
	alias = strings.ToLower(strings.TrimSpace(alias))
	if !aliasRe.MatchString(alias) {
		return "", fmt.Errorf("别名 %q 非法：只允许小写字母/数字/_/-，长度 1-32", alias)
	}
	return alias, nil
}

func shortUID(uid string) string {
	if len(uid) <= 6 {
		return uid
	}
	return uid[:6]
}
