package app

import (
	"fmt"
	"os"
	"time"
)

// Status prints the descriptor and live health for a profile.
func Status(profile string) error {
	profile = normalizeProfile(profile)
	d, ok := ReadDescriptor(profile)
	if !ok {
		fmt.Printf("profile %s: 未运行（没有 descriptor）\n", profile)
		return nil
	}
	fmt.Printf("profile:  %s\n", profile)
	fmt.Printf("http:     %s\n", d.HTTPURL)
	fmt.Printf("ws:       %s\n", d.WSAddr)
	fmt.Printf("pid:      %d\n", d.PID)
	fmt.Printf("version:  %s (protocol %d)\n", d.Version, d.Protocol)
	fmt.Printf("daemonId: %s\n", d.ID)
	fmt.Printf("started:  %s\n", d.StartedAt.Format(time.RFC3339))
	h, err := Probe(d.HTTPURL, "")
	if err != nil {
		fmt.Printf("health:   不可达（%v）\n", err)
		return nil
	}
	fmt.Printf("health:   ok=%v connected=%v lastEventId=%d\n", h.OK, h.Connected, h.LastEventID)
	return nil
}

// Stop asks the daemon for a profile to shut down gracefully after verifying
// that it really is the process described by the descriptor.
func Stop(profile string) error {
	profile = normalizeProfile(profile)
	d, ok := ReadDescriptor(profile)
	if !ok {
		return fmt.Errorf("profile %s 未运行", profile)
	}
	if _, err := Probe(d.HTTPURL, ""); err != nil {
		return fmt.Errorf("profile %s 的 daemon 不可达（%s）：%w", profile, d.HTTPURL, err)
	}
	p, err := os.FindProcess(d.PID)
	if err != nil {
		return fmt.Errorf("找不到进程 %d: %w", d.PID, err)
	}
	// os.Interrupt maps to SIGINT on unix and CTRL_C on Windows; the daemon
	// listens for both SIGINT and SIGTERM.
	if err := p.Signal(os.Interrupt); err != nil {
		return fmt.Errorf("发送停止信号失败: %w", err)
	}
	fmt.Printf("已请求停止 profile %s (pid=%d)\n", profile, d.PID)
	return nil
}
