// Package app: instance discovery and mutual exclusion.
//
// jq-mcp's browser bridge is a singleton per browser profile: the extension
// connects to exactly one WebSocket endpoint, so only one process may own it.
// Clients (MCP stdio proxies, drivers, the jq-loop plugin) need to find that
// process without knowing its port in advance.
//
// This file implements the two small pieces that solve it:
//
//   - a descriptor file (~/.config/jq-mcp/instances/<profile>.json) that records
//     the live HTTP address, pid and protocol version;
//   - an advisory file lock so a second `jq-mcp serve` cannot start and
//     silently diverge from the first.
//
// Deliberately NOT implemented: election, leases, heartbeats. The daemon is
// expected to run under a supervisor (systemd --user); see docs/architecture-v2.md.
package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jqhelper/jq-mcp/internal/config"
)

// Protocol is bumped whenever the HTTP/MCP surface changes incompatibly.
const Protocol = 2

// Descriptor is written by the running daemon and read by clients.
type Descriptor struct {
	ID        string    `json:"id"`
	Profile   string    `json:"profile"`
	HTTPURL   string    `json:"httpUrl"`
	WSAddr    string    `json:"wsAddr"`
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	Protocol  int       `json:"protocol"`
	StartedAt time.Time `json:"startedAt"`
}

// ErrAlreadyRunning means a healthy daemon already owns this profile.
type ErrAlreadyRunning struct{ Desc Descriptor }

func (e *ErrAlreadyRunning) Error() string {
	if e.Desc.PID == 0 {
		return "jq-mcp 已在运行（另一个进程持有实例锁）"
	}
	return fmt.Sprintf("jq-mcp 已在运行 (pid=%d, %s, version=%s)", e.Desc.PID, e.Desc.HTTPURL, e.Desc.Version)
}

// InstancesDir returns the descriptor directory.
func InstancesDir() string { return filepath.Join(config.Dir(), "instances") }

func descriptorPath(profile string) string {
	if profile == "" {
		profile = "default"
	}
	return filepath.Join(InstancesDir(), profile+".json")
}

func lockPath(profile string) string {
	if profile == "" {
		profile = "default"
	}
	return filepath.Join(InstancesDir(), profile+".lock")
}

func normalizeProfile(p string) string {
	if p == "" {
		return "default"
	}
	return p
}

// Lease is held for the lifetime of the daemon. Closing it releases the lock
// and removes the descriptor.
type Lease struct {
	lock *os.File
	path string
	held Descriptor
}

// AcquireInstance takes the per-profile lock and verifies that no healthy
// daemon is already serving it.
//
// The lock protects against a second process starting; the health probe
// protects against a stale descriptor or a daemon that does not participate in
// locking (older builds, or a manually started process).
func AcquireInstance(profile string) (*Lease, error) {
	profile = normalizeProfile(profile)
	if err := os.MkdirAll(InstancesDir(), 0o700); err != nil {
		return nil, fmt.Errorf("创建实例目录失败: %w", err)
	}
	f, err := os.OpenFile(lockPath(profile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开实例锁失败: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		// Someone else holds the lock. If it is healthy, report it; if the
		// descriptor is stale the holder will clean up when it exits.
		if d, ok := ReadDescriptor(profile); ok {
			if h, err := Probe(d.HTTPURL, ""); err == nil && h.OK {
				return nil, &ErrAlreadyRunning{Desc: d}
			}
		}
		return nil, &ErrAlreadyRunning{Desc: Descriptor{Profile: profile}}
	}
	// We hold the lock. A daemon that predates locking may still be listening.
	if d, ok := ReadDescriptor(profile); ok {
		if h, err := Probe(d.HTTPURL, ""); err == nil && h.OK {
			_ = unlockFile(f)
			_ = f.Close()
			return nil, &ErrAlreadyRunning{Desc: d}
		}
	}
	return &Lease{lock: f, path: descriptorPath(profile)}, nil
}

// Publish writes the descriptor with the addresses that are actually bound.
func (l *Lease) Publish(d Descriptor) error {
	d.Protocol = Protocol
	if d.StartedAt.IsZero() {
		d.StartedAt = time.Now()
	}
	l.held = d
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Close releases the lock and removes the descriptor.
func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	_ = os.Remove(l.path)
	if l.lock != nil {
		_ = unlockFile(l.lock)
		_ = l.lock.Close()
	}
	return nil
}

// ReadDescriptor loads the descriptor for a profile.
func ReadDescriptor(profile string) (Descriptor, bool) {
	var d Descriptor
	raw, err := os.ReadFile(descriptorPath(profile))
	if err != nil {
		return d, false
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, false
	}
	return d, true
}

// Health is the subset of /healthz that clients care about.
type Health struct {
	OK          bool   `json:"ok"`
	Version     string `json:"version"`
	Protocol    int    `json:"protocol"`
	DaemonID    string `json:"daemonId"`
	Connected   bool   `json:"connected"`
	LastEventID int64  `json:"lastEventId"`
	StartedAt   string `json:"startedAt"`
}

// Probe calls /healthz. token may be empty for the unauthenticated endpoint.
func Probe(baseURL, token string) (Health, error) {
	var h Health
	baseURL = normalizeBaseURL(baseURL)
	if baseURL == "" {
		return h, errors.New("空的 daemon 地址")
	}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		return h, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return h, fmt.Errorf("healthz 返回 %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return h, err
	}
	return h, nil
}

// ResolveHTTPURL finds the daemon address for a profile, falling back to an
// explicit override (flag/config) and then to the descriptor.
func ResolveHTTPURL(profile, override string) string {
	if override != "" {
		return normalizeBaseURL(override)
	}
	if d, ok := ReadDescriptor(profile); ok && d.HTTPURL != "" {
		return normalizeBaseURL(d.HTTPURL)
	}
	return ""
}

// normalizeBaseURL accepts "127.0.0.1:8792" as well as a full URL, because the
// config file historically stores a bare listen address.
func normalizeBaseURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	return strings.TrimRight(u, "/")
}
