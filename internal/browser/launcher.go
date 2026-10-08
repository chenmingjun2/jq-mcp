// Package browser launches a managed Chrome/Chromium instance with the
// jqhelper extension loaded when jq-mcp runs in "launch" mode.
package browser

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/jqhelper/jq-mcp/internal/config"
)

// Launcher owns the managed browser process.
type Launcher struct {
	cmd    *exec.Cmd
	logger *slog.Logger
	path   string
}

// Launch starts the browser and returns immediately. Callers should defer Stop.
// bridgeURL and token are written into the extension directory as
// default-config.json so the freshly launched extension connects automatically.
func Launch(cfg config.Browser, bridgeURL, token string, logger *slog.Logger) (*Launcher, error) {
	if logger == nil {
		logger = slog.Default()
	}
	bin, err := resolveChrome(cfg.ChromePath)
	if err != nil {
		return nil, err
	}
	extDir, err := resolveExtensionDir(cfg.ExtensionDir)
	if err != nil {
		return nil, err
	}
	if bridgeURL != "" && token != "" {
		if err := writeDefaultConfig(extDir, bridgeURL, token); err != nil {
			logger.Warn("写入插件默认配置失败", "err", err)
		}
	}
	userDataDir := cfg.UserDataDir
	if userDataDir == "" {
		userDataDir = config.DefaultUserDataDir()
	}
	if err := os.MkdirAll(userDataDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建浏览器配置目录失败: %w", err)
	}

	args := []string{
		"--user-data-dir=" + userDataDir,
		"--load-extension=" + extDir,
		// NOTE: never pass --disable-extensions-except here. It disables every
		// other extension in the profile, which silently drops the user's own
		// extensions (including a manually installed jqhelper) after a restart.
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=Translate",
	}
	if cfg.Headless {
		args = append(args, "--headless=new", "--disable-gpu")
	}
	if os.Geteuid() == 0 {
		// Chrome refuses to run its sandbox as root; common in containers.
		args = append(args, "--no-sandbox", "--disable-dev-shm-usage")
	}
	startURL := cfg.StartURL
	if startURL == "" {
		startURL = "https://www.joinquant.com/"
	}
	args = append(args, startURL)

	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动浏览器失败: %w", err)
	}
	logger.Info("已启动受管浏览器", "path", bin, "headless", cfg.Headless, "extension", extDir)
	return &Launcher{cmd: cmd, logger: logger, path: bin}, nil
}

// Stop terminates the managed browser.
func (l *Launcher) Stop() {
	if l == nil || l.cmd == nil || l.cmd.Process == nil {
		return
	}
	_ = l.cmd.Process.Kill()
	_, _ = l.cmd.Process.Wait()
}

// writeDefaultConfig drops a bootstrap config into the unpacked extension dir.
func writeDefaultConfig(extDir, bridgeURL, token string) error {
	payload, err := json.MarshalIndent(map[string]string{"wsUrl": bridgeURL, "token": token}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(extDir, "default-config.json"), payload, 0o600)
}

func resolveChrome(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("指定的浏览器路径不存在: %s", explicit)
		}
		return explicit, nil
	}
	candidates := chromeCandidates()
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到 Chrome/Chromium，请用 --chrome-path 指定")
}

func chromeCandidates() []string {
	base := []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"}
	switch runtime.GOOS {
	case "darwin":
		return append([]string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		}, base...)
	case "windows":
		return append([]string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		}, base...)
	default:
		return base
	}
}

func resolveExtensionDir(explicit string) (string, error) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "extension"))
	}
	candidates = append(candidates, "extension", filepath.Join("..", "extension"))
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			abs, _ := filepath.Abs(c)
			if _, err := os.Stat(filepath.Join(abs, "manifest.json")); err == nil {
				return abs, nil
			}
		}
	}
	return "", fmt.Errorf("未找到 jqhelper 插件目录，请用 --extension-dir 指定（需包含 manifest.json）")
}
