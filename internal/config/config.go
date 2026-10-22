// Package config loads and persists jq-mcp settings, including the shared
// bridge token and the dual-mode browser options.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Browser modes.
const (
	BrowserAttach = "attach" // reuse a browser the user already runs
	BrowserLaunch = "launch" // jq-mcp spawns a browser with the extension loaded
)

// Browser holds the browser-launch related settings.
type Browser struct {
	Mode         string `json:"mode"`
	Headless     bool   `json:"headless"`
	ChromePath   string `json:"chrome_path"`
	UserDataDir  string `json:"user_data_dir"`
	ExtensionDir string `json:"extension_dir"`
	StartURL     string `json:"start_url"`
}

// Pool holds account-pool defaults. Per-account overrides live in accounts.json.
type Pool struct {
	DefaultCap       int      `json:"default_cap"`
	MaxFailovers     int      `json:"max_failovers"`
	DefaultPlacement string   `json:"default_placement"`
	DefaultAccount   string   `json:"default_account,omitempty"`
	Browser          string   `json:"browser"`  // chrome-for-testing
	Channel          string   `json:"channel"`  // stable
	Headless         bool     `json:"headless"` // pool windows are visible by default
	UAMode           string   `json:"ua_mode"`  // random|off
	UAPlatforms      []string `json:"ua_platforms,omitempty"`
	UABrowsers       []string `json:"ua_browsers,omitempty"`
}

// Config is the persisted configuration.
type Config struct {
	WSAddr   string  `json:"ws_addr"`
	WSPath   string  `json:"ws_path"`
	HTTPAddr string  `json:"http_addr,omitempty"`
	Token    string  `json:"token"`
	TaskFile string  `json:"task_file"`
	Browser  Browser `json:"browser"`
	Pool     Pool    `json:"pool"`
}

// Default returns the built-in defaults.
func Default() *Config {
	return &Config{
		WSAddr:   "127.0.0.1:8790",
		WSPath:   "/ws",
		TaskFile: "",
		Browser: Browser{
			Mode:         BrowserAttach,
			Headless:     false,
			ChromePath:   "",
			UserDataDir:  "",
			ExtensionDir: "",
			StartURL:     "https://www.joinquant.com/",
		},
		Pool: Pool{
			DefaultCap:       2,
			MaxFailovers:     2,
			DefaultPlacement: "auto",
			Browser:          "chrome-for-testing",
			Channel:          "stable",
			Headless:         false,
			UAMode:           "random",
			UAPlatforms:      []string{"windows", "linux", "android", "macos", "ios"},
			UABrowsers:       []string{"chrome", "chromium", "firefox", "safari"},
		},
	}
}

// Dir returns the jq-mcp config directory, honoring JQ_MCP_HOME.
func Dir() string {
	if v := os.Getenv("JQ_MCP_HOME"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "jq-mcp")
		}
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "jq-mcp")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "jq-mcp")
	}
	return filepath.Join(home, ".config", "jq-mcp")
}

// DefaultPath is the default config file location.
func DefaultPath() string { return filepath.Join(Dir(), "config.json") }

// AccountsPath is the account registry path.
func AccountsPath() string { return filepath.Join(Dir(), "accounts.json") }

// ProfilesDir is the root of managed browser profiles.
func ProfilesDir() string { return filepath.Join(Dir(), "profiles") }

// BrowsersDir is the root of downloaded browser builds.
func BrowsersDir() string { return filepath.Join(Dir(), "browsers") }

// ProfileDir returns the directory for one pool account.
func ProfileDir(alias string) string { return filepath.Join(ProfilesDir(), alias) }

// DefaultUserDataDir returns the default managed-browser profile directory.
func DefaultUserDataDir() string {
	return filepath.Join(Dir(), "chrome")
}

// Load reads the config at path. A missing file yields defaults; a missing
// token is generated and persisted so the extension can be configured once.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultPath()
	}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("解析配置文件失败: %w", err)
		}
	default:
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
	}
	if cfg.WSAddr == "" {
		cfg.WSAddr = "127.0.0.1:8790"
	}
	if cfg.WSPath == "" {
		cfg.WSPath = "/ws"
	}
	if cfg.Browser.Mode == "" {
		cfg.Browser.Mode = BrowserAttach
	}
	if cfg.Browser.StartURL == "" {
		cfg.Browser.StartURL = "https://www.joinquant.com/"
	}
	if cfg.Pool.DefaultCap <= 0 {
		cfg.Pool.DefaultCap = 2
	}
	if cfg.Pool.MaxFailovers <= 0 {
		cfg.Pool.MaxFailovers = 2
	}
	if cfg.Pool.DefaultPlacement == "" {
		cfg.Pool.DefaultPlacement = "auto"
	}
	if cfg.Pool.Browser == "" {
		cfg.Pool.Browser = "chrome-for-testing"
	}
	if cfg.Pool.Channel == "" {
		cfg.Pool.Channel = "stable"
	}
	if cfg.Pool.UAMode == "" {
		cfg.Pool.UAMode = "random"
	}
	if len(cfg.Pool.UAPlatforms) == 0 {
		cfg.Pool.UAPlatforms = []string{"windows", "linux", "android", "macos", "ios"}
	}
	if len(cfg.Pool.UABrowsers) == 0 {
		cfg.Pool.UABrowsers = []string{"chrome", "chromium", "firefox", "safari"}
	}
	if cfg.Token == "" {
		token, err := generateToken()
		if err != nil {
			return nil, err
		}
		cfg.Token = token
	}
	if err := cfg.Save(path); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save writes the config atomically with 0600 permissions.
func (c *Config) Save(path string) error {
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}
	return nil
}

func generateToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成 token 失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
