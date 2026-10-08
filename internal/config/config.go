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

// Config is the persisted configuration.
type Config struct {
	WSAddr   string  `json:"ws_addr"`
	WSPath   string  `json:"ws_path"`
	Token    string  `json:"token"`
	TaskFile string  `json:"task_file"`
	Browser  Browser `json:"browser"`
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
