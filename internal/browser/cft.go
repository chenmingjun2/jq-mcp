package browser

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jqhelper/jq-mcp/internal/config"
)

// Chrome for Testing is the only supported pool browser: branded Chrome removed
// --load-extension in 137, while CfT keeps it. The binary is downloaded on
// demand and pinned per version so behaviour does not drift mid-experiment.
const cftManifestURL = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"

type cftManifest struct {
	Channels map[string]struct {
		Version   string `json:"version"`
		Downloads struct {
			Chrome []struct {
				Platform string `json:"platform"`
				URL      string `json:"url"`
			} `json:"chrome"`
		} `json:"downloads"`
	} `json:"channels"`
}

// EnsureChromeForTesting returns a usable CfT binary, downloading it on first
// use. explicitPath (flag/env) always wins. On network failure it falls back to
// the newest already-installed version.
func EnsureChromeForTesting(ctx context.Context, explicitPath, channel string, log *slog.Logger) (string, error) {
	if log == nil {
		log = slog.Default()
	}
	if explicitPath == "" {
		explicitPath = os.Getenv("JQ_MCP_CHROME")
	}
	if explicitPath != "" {
		if _, err := os.Stat(explicitPath); err != nil {
			return "", fmt.Errorf("指定的浏览器路径不存在: %s", explicitPath)
		}
		return explicitPath, nil
	}
	if channel == "" {
		channel = "Stable"
	}
	root := filepath.Join(config.BrowsersDir(), "chrome-for-testing")
	if p := currentBinary(root); p != "" {
		return p, nil
	}
	if p := newestInstalled(root); p != "" {
		log.Info("复用已安装的 Chrome for Testing", "path", p)
		writeCurrent(root, p)
		return p, nil
	}

	platform, err := cftPlatform()
	if err != nil {
		return "", err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	manifestClient := &http.Client{Timeout: 30 * time.Second, Transport: transport}
	downloadClient := &http.Client{Timeout: 30 * time.Minute, Transport: transport}

	version, url, err := fetchCFT(ctx, manifestClient, channel, platform)
	if err != nil {
		return "", fmt.Errorf("获取 Chrome for Testing 清单失败: %w（可用 --chrome-path 指定本地浏览器）", err)
	}
	log.Info("开始下载 Chrome for Testing", "version", version, "platform", platform)
	bin, err := installCFT(ctx, downloadClient, root, version, url, log)
	if err != nil {
		return "", err
	}
	writeCurrent(root, bin)
	return bin, nil
}

func fetchCFT(ctx context.Context, client *http.Client, channel, platform string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cftManifestURL, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("清单返回 HTTP %d", resp.StatusCode)
	}
	var m cftManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return "", "", err
	}
	ch, ok := m.Channels[channel]
	if !ok {
		if ch, ok = m.Channels["Stable"]; !ok {
			return "", "", fmt.Errorf("清单中没有 %s 渠道", channel)
		}
	}
	for _, d := range ch.Downloads.Chrome {
		if d.Platform == platform {
			return ch.Version, d.URL, nil
		}
	}
	return "", "", fmt.Errorf("清单中没有平台 %s 的包", platform)
}

func installCFT(ctx context.Context, client *http.Client, root, version, url string, log *slog.Logger) (string, error) {
	final := filepath.Join(root, version)
	tmp := filepath.Join(root, "tmp-"+version+"-"+fmt.Sprint(time.Now().UnixNano()))
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	zipPath := filepath.Join(tmp, "chrome.zip")
	if err := download(ctx, client, url, zipPath, log); err != nil {
		return "", err
	}
	extractDir := filepath.Join(tmp, "extract")
	if err := unzip(zipPath, extractDir); err != nil {
		return "", fmt.Errorf("解压 Chrome for Testing 失败: %w", err)
	}
	bin, err := findCFTBinary(extractDir)
	if err != nil {
		return "", err
	}
	if err := verifyBinary(bin); err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if _, err := os.Stat(final); err == nil {
		_ = os.RemoveAll(final)
	}
	if err := os.Rename(extractDir, final); err != nil {
		return "", fmt.Errorf("安装 Chrome for Testing 失败: %w", err)
	}
	_ = os.WriteFile(filepath.Join(final, ".ready"), []byte(version), 0o600)
	bin, err = findCFTBinary(final)
	if err != nil {
		return "", err
	}
	log.Info("Chrome for Testing 安装完成", "version", version, "path", bin)
	return bin, nil
}

func download(ctx context.Context, client *http.Client, url, dst string, log *slog.Logger) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载返回 HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	var total int64
	nextLog := int64(50 << 20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			total += int64(n)
			if total >= nextLog {
				log.Info("下载进度", "mb", total>>20)
				nextLog += 50 << 20
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return f.Sync()
}

func unzip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		name := filepath.Clean(f.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("压缩包包含非法路径: %s", f.Name)
		}
		target := filepath.Join(dst, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode()|0o600)
		if err != nil {
			return err
		}
		in, err := f.Open()
		if err != nil {
			out.Close()
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func cftPlatform() (string, error) {
	switch runtime.GOOS {
	case "linux":
		if runtime.GOARCH == "arm64" {
			return "linux-arm64", nil
		}
		return "linux64", nil
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "mac-arm64", nil
		}
		return "mac-x64", nil
	case "windows":
		if runtime.GOARCH == "386" {
			return "win32", nil
		}
		return "win64", nil
	}
	return "", fmt.Errorf("不支持的平台: %s/%s", runtime.GOOS, runtime.GOARCH)
}

func findCFTBinary(root string) (string, error) {
	candidates := []string{
		filepath.Join(root, "chrome-linux64", "chrome"),
		filepath.Join(root, "chrome-linux-arm64", "chrome"),
		filepath.Join(root, "chrome-mac-x64", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"),
		filepath.Join(root, "chrome-mac-arm64", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"),
		filepath.Join(root, "chrome-win64", "chrome.exe"),
		filepath.Join(root, "chrome-win32", "chrome.exe"),
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	// Fall back to a shallow walk (packages sometimes gain a nested top dir).
	var found string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return nil
		}
		base := d.Name()
		if base == "chrome" || base == "chrome.exe" || base == "Google Chrome for Testing" {
			found = p
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("解压后未找到 Chrome for Testing 可执行文件")
	}
	return found, nil
}

func verifyBinary(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("浏览器无法运行: %w", err)
	}
	if !strings.Contains(strings.ToLower(string(out)), "chrome") {
		return fmt.Errorf("浏览器版本输出异常: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func currentBinary(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, "current"))
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(raw))
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}

func writeCurrent(root, bin string) {
	_ = os.MkdirAll(root, 0o700)
	_ = os.WriteFile(filepath.Join(root, "current"), []byte(bin+"\n"), 0o600)
}

func newestInstalled(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), "tmp-") {
			versions = append(versions, e.Name())
		}
	}
	// Sort descending lexically; CfT versions are dotted and monotonic.
	for i := 0; i < len(versions); i++ {
		for j := i + 1; j < len(versions); j++ {
			if versions[j] > versions[i] {
				versions[i], versions[j] = versions[j], versions[i]
			}
		}
	}
	for _, v := range versions {
		if bin, err := findCFTBinary(filepath.Join(root, v)); err == nil {
			return bin
		}
	}
	return ""
}
