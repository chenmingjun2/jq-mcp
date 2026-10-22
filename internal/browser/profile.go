package browser

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jqhelper/jq-mcp/internal/config"
)

// Profile is one managed browser profile (one JoinQuant account).
type Profile struct {
	Alias    string    `json:"alias"`
	Dir      string    `json:"dir"`
	Chrome   string    `json:"chromeDir"`
	Ext      string    `json:"extensionDir"`
	LoggedIn bool      `json:"loggedIn,omitempty"`
	LastOpen time.Time `json:"lastOpen,omitempty"`

	// UA is generated once per profile (not per launch) and persisted so the
	// account keeps a stable fingerprint across restarts.
	UserAgent  string `json:"userAgent,omitempty"`
	UAPlatform string `json:"uaPlatform,omitempty"`
	UABrowser  string `json:"uaBrowser,omitempty"`
}

// PrepareProfile creates/refreshes the profile directory and writes the
// per-profile extension bootstrap (wsUrl/token/alias).
func PrepareProfile(alias, extSrc, wsURL, token string) (*Profile, error) {
	dir := config.ProfileDir(alias)
	p := &Profile{
		Alias:  alias,
		Dir:    dir,
		Chrome: filepath.Join(dir, "chrome"),
		Ext:    filepath.Join(dir, "extension"),
	}
	// Preserve the persisted per-profile identity (UA) and timestamps.
	if raw, err := os.ReadFile(filepath.Join(dir, "profile.json")); err == nil {
		_ = json.Unmarshal(raw, p)
		p.Alias, p.Dir = alias, dir
		p.Chrome, p.Ext = filepath.Join(dir, "chrome"), filepath.Join(dir, "extension")
	}
	if err := os.MkdirAll(p.Chrome, 0o700); err != nil {
		return nil, fmt.Errorf("创建浏览器配置目录失败: %w", err)
	}
	if err := syncExtension(extSrc, p.Ext); err != nil {
		return nil, err
	}
	bootstrap := map[string]string{
		"wsUrl": wsURL, "token": token, "alias": alias, "daemonProfile": "default",
	}
	raw, _ := json.MarshalIndent(bootstrap, "", "  ")
	if err := os.WriteFile(filepath.Join(p.Ext, "default-config.json"), raw, 0o600); err != nil {
		return nil, fmt.Errorf("写入插件引导配置失败: %w", err)
	}
	return p, nil
}

// EnsureUA assigns a random identity once when UA spoofing is enabled. Existing
// assignments are kept, which is what makes the fingerprint stable.
func (p *Profile) EnsureUA(mode string, platforms, browsers []string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "off", "none", "disabled":
		if p.UserAgent != "" {
			p.UserAgent, p.UAPlatform, p.UABrowser = "", "", ""
			p.save()
		}
		return ""
	}
	if p.UserAgent != "" {
		return p.UserAgent
	}
	spec := RandomUA(platforms, browsers)
	p.UserAgent, p.UAPlatform, p.UABrowser = spec.UserAgent, spec.Platform, spec.Browser
	p.save()
	return p.UserAgent
}

// SetUA overrides the profile identity. An empty value clears it.
func (p *Profile) SetUA(ua, platform, browser string) {
	p.UserAgent, p.UAPlatform, p.UABrowser = strings.TrimSpace(ua), platform, browser
	p.save()
}

// Launch starts the browser detached and records its pid.
func (p *Profile) Launch(bin string, headless bool, log *slog.Logger) (int, error) {
	if pid, ok := p.Running(); ok {
		return pid, nil
	}
	args := []string{
		"--user-data-dir=" + p.Chrome,
		"--load-extension=" + p.Ext,
		"--no-first-run",
		"--no-default-browser-check",
	}
	features := "Translate"
	if p.UserAgent != "" {
		args = append(args, "--user-agent="+p.UserAgent)
		// Keep client hints from contradicting the spoofed UA.
		features = "Translate,UserAgentClientHint"
	}
	args = append(args, "--disable-features="+features)
	if headless {
		args = append(args, "--headless=new", "--disable-gpu")
	}
	url := "https://www.joinquant.com/"
	if !p.firstOpenDone() {
		url = "https://www.joinquant.com/user/login/index"
	}
	args = append(args, url)

	logPath := filepath.Join(p.Dir, "chrome.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("打开浏览器日志失败: %w", err)
	}
	defer lf.Close()

	cmd := exec.Command(bin, args...)
	cmd.Stdout = lf
	cmd.Stderr = lf
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("启动浏览器失败: %w", err)
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(p.pidPath(), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		log.Warn("写入浏览器 pid 失败", "err", err)
	}
	p.LastOpen = time.Now()
	p.save()
	_ = p.markFirstOpen()
	return pid, nil
}

// Running reports whether the recorded browser process is still alive.
func (p *Profile) Running() (int, bool) {
	raw, err := os.ReadFile(p.pidPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if !processAlive(pid) {
		_ = os.Remove(p.pidPath())
		return 0, false
	}
	return pid, true
}

// Stop terminates the browser process.
func (p *Profile) Stop() error {
	pid, ok := p.Running()
	if !ok {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	_ = proc.Signal(syscall.SIGTERM)
	for i := 0; i < 30; i++ {
		if !processAlive(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(pid) {
		_ = proc.Kill()
	}
	_ = os.Remove(p.pidPath())
	return nil
}

// LoadProfile reads an existing profile descriptor.
func LoadProfile(alias string) (*Profile, error) {
	dir := config.ProfileDir(alias)
	p := &Profile{Alias: alias, Dir: dir, Chrome: filepath.Join(dir, "chrome"), Ext: filepath.Join(dir, "extension")}
	raw, err := os.ReadFile(filepath.Join(dir, "profile.json"))
	if err == nil {
		_ = json.Unmarshal(raw, p)
		p.Alias, p.Dir = alias, dir
		p.Chrome, p.Ext = filepath.Join(dir, "chrome"), filepath.Join(dir, "extension")
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("账号 %s 的 profile 不存在，先执行 jq-mcp accounts add %s", alias, alias)
	}
	return p, nil
}

func (p *Profile) pidPath() string       { return filepath.Join(p.Dir, "chrome.pid") }
func (p *Profile) firstOpenPath() string { return filepath.Join(p.Dir, ".first-open-done") }

func (p *Profile) firstOpenDone() bool {
	_, err := os.Stat(p.firstOpenPath())
	return err == nil
}

func (p *Profile) markFirstOpen() error {
	return os.WriteFile(p.firstOpenPath(), []byte(time.Now().Format(time.RFC3339)), 0o600)
}

func (p *Profile) save() {
	_ = os.MkdirAll(p.Dir, 0o700)
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(p.Dir, "profile.json"), raw, 0o600)
}

func processAlive(pid int) bool {
	if runtime.GOOS == "linux" {
		_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
		return err == nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// syncExtension copies the bundled extension into the profile and refreshes it
// when the source version changes. The browser profile itself is never touched.
func syncExtension(src, dst string) error {
	version, err := extensionVersion(src)
	if err != nil {
		return err
	}
	stamp := filepath.Join(dst, ".jq-ext-version")
	if raw, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(raw)) == version {
		return nil
	}
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("清理旧扩展副本失败: %w", err)
	}
	if err := copyTree(src, dst); err != nil {
		return fmt.Errorf("复制扩展失败: %w", err)
	}
	return os.WriteFile(stamp, []byte(version), 0o600)
}

func extensionVersion(src string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(src, "manifest.json"))
	if err != nil {
		return "", fmt.Errorf("扩展目录缺少 manifest.json: %w", err)
	}
	var m struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return "", fmt.Errorf("解析扩展 manifest 失败")
	}
	st, _ := os.Stat(filepath.Join(src, "lib", "protocol.js"))
	if st != nil {
		return m.Version + "-" + strconv.FormatInt(st.ModTime().Unix(), 10), nil
	}
	return m.Version, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode()|0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
