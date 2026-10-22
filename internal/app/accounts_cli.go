package app

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/jqhelper/jq-mcp/internal/account"
	"github.com/jqhelper/jq-mcp/internal/browser"
	"github.com/jqhelper/jq-mcp/internal/config"
)

// AccountsCLI implements `jq-mcp accounts ...`. It only touches registry files,
// browser profiles and processes; it never needs the daemon to be running.
func AccountsCLI(ctx context.Context, args []string) error {
	if len(args) == 0 {
		accountsUsage()
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "add":
		return accountsAdd(rest)
	case "open", "login":
		return accountsOpen(ctx, rest)
	case "close":
		return accountsClose(rest)
	case "status", "ls", "list":
		return accountsStatus(rest)
	case "rm", "remove":
		return accountsRemove(rest)
	case "default":
		return accountsDefault(rest)
	case "cap":
		return accountsCap(rest)
	case "ua":
		return accountsUA(rest)
	case "doctor":
		return accountsDoctor(ctx, rest)
	case "help", "-h", "--help":
		accountsUsage()
		return nil
	default:
		accountsUsage()
		return fmt.Errorf("未知子命令: %s", cmd)
	}
}

func accountsUsage() {
	fmt.Fprint(os.Stderr, `jq-mcp accounts — 账号池管理

  add <alias...> [--cap N]       创建 profile（不启动）
  open [alias...|--all]          启动窗口；首次打开登录页
  close [alias...|--all]         关闭窗口
  status                         查看账号、profile 与进程状态
  rm <alias> [--purge-profile]   删除账号（默认保留 profile）
  default <alias>                设置默认账号
  cap <alias> <N>                设置该账号并发上限（0=默认）
  ua <alias> [random|off|UA]     查看/重随机/关闭该账号的 UA 伪装
  doctor                         检查浏览器/扩展/目录

通用参数：--config <path>  --chrome-path <path>  --extension-dir <dir>  --headless
`)
}

type accountsFlags struct {
	fs           *flag.FlagSet
	configPath   string
	chromePath   string
	extensionDir string
	headless     bool
	cap          int
	all          bool
	purge        bool
}

func newAccountsFlags(name string, args []string) *accountsFlags {
	f := &accountsFlags{fs: flag.NewFlagSet("accounts "+name, flag.ContinueOnError)}
	f.fs.StringVar(&f.configPath, "config", "", "配置文件路径")
	f.fs.StringVar(&f.chromePath, "chrome-path", "", "Chrome/Chromium 可执行文件路径")
	f.fs.StringVar(&f.extensionDir, "extension-dir", "", "jqhelper 插件目录")
	f.fs.BoolVar(&f.headless, "headless", false, "无头模式（首次登录请勿使用）")
	f.fs.IntVar(&f.cap, "cap", -1, "该账号并发上限（0=默认）")
	f.fs.BoolVar(&f.all, "all", false, "对所有账号执行")
	f.fs.BoolVar(&f.purge, "purge-profile", false, "同时删除 profile 目录（含登录态）")
	return f
}

func (f *accountsFlags) parse(args []string) ([]string, error) {
	if err := f.fs.Parse(reorderAccountArgs(args)); err != nil {
		return nil, err
	}
	return f.fs.Args(), nil
}

// reorderAccountArgs lets flags appear after positional arguments, which is what
// users naturally type ("accounts add a1 a2 --cap 3").
func reorderAccountArgs(args []string) []string {
	valueFlags := map[string]bool{"config": true, "chrome-path": true, "extension-dir": true, "cap": true}
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if valueFlags[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, pos...)
}

func loadPoolConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func loadRegistry() (*account.Registry, error) {
	return account.Load(config.AccountsPath())
}

func poolWSURL(cfg *config.Config) string {
	host, port, err := net.SplitHostPort(cfg.WSAddr)
	if err != nil {
		return "ws://127.0.0.1:8790/ws"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "ws://" + net.JoinHostPort(host, port) + cfg.WSPath
}

func accountsAdd(args []string) error {
	f := newAccountsFlags("add", args)
	aliases, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(aliases) == 0 {
		return fmt.Errorf("至少提供一个别名，例如: jq-mcp accounts add a1 a2")
	}
	cfg, err := loadPoolConfig(f.configPath)
	if err != nil {
		return err
	}
	_ = cfg
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	for _, alias := range aliases {
		acc, err := reg.Add(alias, maxInt(f.cap, 0))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(config.ProfileDir(acc.Alias), 0o700); err != nil {
			return err
		}
		fmt.Printf("已创建账号 %s（profile: %s）\n", acc.Alias, config.ProfileDir(acc.Alias))
	}
	return nil
}

func accountsOpen(ctx context.Context, args []string) error {
	f := newAccountsFlags("open", args)
	aliases, err := f.parse(args)
	if err != nil {
		return err
	}
	cfg, err := loadPoolConfig(f.configPath)
	if err != nil {
		return err
	}
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	if f.all || len(aliases) == 0 {
		aliases = aliases[:0]
		for _, a := range reg.List() {
			aliases = append(aliases, a.Alias)
		}
	}
	if len(aliases) == 0 {
		return fmt.Errorf("账号池为空。先执行: jq-mcp accounts add a1 a2")
	}
	extSrc, err := browser.ExtensionDir(firstNonEmpty(f.extensionDir, cfg.Browser.ExtensionDir))
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	bin, err := browser.EnsureChromeForTesting(ctx, f.chromePath, cfg.Pool.Channel, log)
	if err != nil {
		return err
	}
	headless := f.headless || cfg.Pool.Headless
	wsURL := poolWSURL(cfg)
	for _, alias := range aliases {
		if _, ok := reg.Get(alias); !ok {
			return fmt.Errorf("账号 %s 不存在，先执行 accounts add", alias)
		}
		p, err := browser.PrepareProfile(alias, extSrc, wsURL, cfg.Token)
		if err != nil {
			return err
		}
		ua := p.EnsureUA(cfg.Pool.UAMode, cfg.Pool.UAPlatforms, cfg.Pool.UABrowsers)
		if pid, running := p.Running(); running {
			fmt.Printf("%s 已在运行 (pid=%d)\n", alias, pid)
			continue
		}
		pid, err := p.Launch(bin, headless, log)
		if err != nil {
			return fmt.Errorf("%s: %w", alias, err)
		}
		if ua != "" {
			fmt.Printf("%s 已启动 (pid=%d, UA=%s/%s)，%s\n", alias, pid, p.UAPlatform, p.UABrowser, loginHint())
		} else {
			fmt.Printf("%s 已启动 (pid=%d)，%s\n", alias, pid, loginHint())
		}
	}
	if !headless {
		fmt.Println("登录完成后可用 jq_accounts 查看绑定状态。")
	}
	return nil
}

func loginHint() string {
	return "首次启动会打开登录页，请手动登录聚宽"
}

func accountsClose(args []string) error {
	f := newAccountsFlags("close", args)
	aliases, err := f.parse(args)
	if err != nil {
		return err
	}
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	if f.all || len(aliases) == 0 {
		aliases = aliases[:0]
		for _, a := range reg.List() {
			aliases = append(aliases, a.Alias)
		}
	}
	for _, alias := range aliases {
		p, err := browser.LoadProfile(alias)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			continue
		}
		if pid, ok := p.Running(); ok {
			_ = p.Stop()
			fmt.Printf("%s 已关闭 (pid=%d)\n", alias, pid)
		} else {
			fmt.Printf("%s 未在运行\n", alias)
		}
	}
	return nil
}

func accountsStatus(args []string) error {
	f := newAccountsFlags("status", args)
	if _, err := f.parse(args); err != nil {
		return err
	}
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ALIAS\tUID\tNICKNAME\tENABLED\tCAP\tUA\tPROFILE\tPID")
	for _, a := range reg.List() {
		pid := "-"
		ua := "off"
		if p, err := browser.LoadProfile(a.Alias); err == nil {
			if p.UAPlatform != "" {
				ua = p.UAPlatform + "/" + p.UABrowser
			}
			if n, ok := p.Running(); ok {
				pid = fmt.Sprint(n)
			}
		}
		capText := "default"
		if a.Cap > 0 {
			capText = fmt.Sprint(a.Cap)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%s\t%s\t%s\t%s\n",
			a.Alias, shortUIDText(a.UID), a.Nickname, a.Enabled, capText, ua, a.ProfileDir, pid)
	}
	_ = w.Flush()
	if def := reg.DefaultAlias(); def != "" {
		fmt.Printf("default: %s\naccounts: %s\n", def, config.AccountsPath())
	}
	return nil
}

func accountsRemove(args []string) error {
	f := newAccountsFlags("rm", args)
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: jq-mcp accounts rm <alias> [--purge-profile]")
	}
	alias := rest[0]
	if p, err := browser.LoadProfile(alias); err == nil {
		_ = p.Stop()
	}
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	if err := reg.Remove(alias); err != nil {
		return err
	}
	if f.purge {
		if err := os.RemoveAll(config.ProfileDir(alias)); err != nil {
			return err
		}
		fmt.Printf("已删除账号 %s 及其 profile\n", alias)
	} else {
		fmt.Printf("已删除账号 %s（profile 保留在 %s）\n", alias, config.ProfileDir(alias))
	}
	return nil
}

func accountsDefault(args []string) error {
	f := newAccountsFlags("default", args)
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: jq-mcp accounts default <alias>")
	}
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	if err := reg.SetDefault(rest[0]); err != nil {
		return err
	}
	fmt.Printf("默认账号已设为 %s\n", rest[0])
	return nil
}

func accountsCap(args []string) error {
	f := newAccountsFlags("cap", args)
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return fmt.Errorf("用法: jq-mcp accounts cap <alias> <N>")
	}
	n, err := strconv.Atoi(rest[1])
	if err != nil || n < 0 {
		return fmt.Errorf("cap 必须是非负整数")
	}
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	if err := reg.SetCap(rest[0], n); err != nil {
		return err
	}
	fmt.Printf("账号 %s 的 cap 已设为 %d（0=默认）\n", rest[0], n)
	return nil
}

func accountsUA(args []string) error {
	f := newAccountsFlags("ua", args)
	rest, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 || len(rest) > 2 {
		return fmt.Errorf("用法: jq-mcp accounts ua <alias> [random|off|<UA 字符串>]")
	}
	alias := rest[0]
	cfg, err := loadPoolConfig(f.configPath)
	if err != nil {
		return err
	}
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	if _, ok := reg.Get(alias); !ok {
		return fmt.Errorf("账号 %s 不存在", alias)
	}
	p, err := browser.LoadProfile(alias)
	if err != nil {
		return err
	}
	mode := "random"
	if len(rest) == 2 {
		mode = rest[1]
	}
	switch strings.ToLower(mode) {
	case "random", "reroll":
		p.SetUAOff(false)
		p.SetUA("", "", "")
		ua := p.EnsureUA("random", cfg.Pool.UAPlatforms, cfg.Pool.UABrowsers)
		fmt.Printf("%s 的 UA 已重新随机：%s/%s\n%s\n", alias, p.UAPlatform, p.UABrowser, ua)
	case "off", "none", "disabled":
		p.SetUAOff(true)
		fmt.Printf("%s 的 UA 伪装已关闭（重启浏览器后生效）\n", alias)
	default:
		p.SetUAOff(false)
		p.SetUA(mode, "custom", "custom")
		fmt.Printf("%s 的 UA 已设为自定义值（重启浏览器后生效）\n", alias)
	}
	if pid, running := p.Running(); running {
		fmt.Printf("注意：浏览器正在运行 (pid=%d)，需要 accounts close %s 后重新 open 才会生效。\n", pid, alias)
	}
	return nil
}

func accountsDoctor(ctx context.Context, args []string) error {
	f := newAccountsFlags("doctor", args)
	if _, err := f.parse(args); err != nil {
		return err
	}
	cfg, err := loadPoolConfig(f.configPath)
	if err != nil {
		return err
	}
	fmt.Printf("config:   %s\n", firstNonEmpty(f.configPath, config.DefaultPath()))
	fmt.Printf("accounts: %s\n", config.AccountsPath())
	fmt.Printf("profiles: %s\n", config.ProfilesDir())
	fmt.Printf("ws:       %s\n", poolWSURL(cfg))
	ext, err := browser.ExtensionDir(firstNonEmpty(f.extensionDir, cfg.Browser.ExtensionDir))
	if err != nil {
		fmt.Printf("extension: ERROR %v\n", err)
	} else {
		fmt.Printf("extension: %s\n", ext)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	bin, err := browser.EnsureChromeForTesting(ctx, f.chromePath, cfg.Pool.Channel, log)
	if err != nil {
		fmt.Printf("browser:   ERROR %v\n", err)
	} else {
		fmt.Printf("browser:   %s\n", bin)
	}
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	for _, a := range reg.List() {
		p, err := browser.LoadProfile(a.Alias)
		if err != nil {
			fmt.Printf("  %-10s profile missing\n", a.Alias)
			continue
		}
		pid, running := p.Running()
		ua := "off"
		if p.UAPlatform != "" {
			ua = p.UAPlatform + "/" + p.UABrowser
		}
		if running {
			fmt.Printf("  %-10s running pid=%d ua=%s\n", a.Alias, pid, ua)
		} else {
			fmt.Printf("  %-10s stopped ua=%s\n", a.Alias, ua)
		}
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func shortUIDText(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}
