package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ccdsyy/clink/internal/mailbox"
	"github.com/ccdsyy/clink/internal/mclog"
	"github.com/ccdsyy/clink/internal/netprobe"
	"github.com/ccdsyy/clink/internal/session"
	"github.com/ccdsyy/clink/internal/sysguard"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App Wails 前后端桥接层：只做参数校验、状态转发与事件推送。
type App struct {
	ctx   context.Context
	mu    sync.Mutex
	host  *session.Host
	guest *session.Guest
	cfg   *Config

	logCtx    context.Context
	logCancel context.CancelFunc
}

// Config 持久化配置（%APPDATA%/CLink/config.json）。
type Config struct {
	TURN        string `json:"turn"`
	MCDir       string `json:"mcDir"`
	MailboxBase string `json:"mailboxBase"`
	FwDone      bool   `json:"fwDone"`
}

// NewApp 构造（加载配置）。
func NewApp() *App {
	a := &App{cfg: &Config{}}
	a.loadConfig()
	return a
}

func (a *App) configPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "CLink", "config.json")
	}
	return "clink-config.json"
}

func (a *App) loadConfig() {
	data, err := os.ReadFile(a.configPath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, a.cfg)
}

func (a *App) saveConfig() {
	path := a.configPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.MarshalIndent(a.cfg, "", "  ")
	_ = os.WriteFile(path, data, 0o644)
}

// startup Wails 生命周期。
func (a *App) startup(ctx context.Context) { a.ctx = ctx }

func (a *App) emit(e session.Event) {
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "clink:event", e)
		// 客机连接成功 → 自动复制 127.0.0.1:xxxx（保姆级交互）
		if e.Stage == "connected" && e.Addr != "" {
			_ = wruntime.ClipboardSetText(a.ctx, e.Addr)
		}
	}
}

// ---------- 供 JS 调用的绑定方法 ----------

// DetectMCPort 返回自动捕获的 MC 端口（0 = 未检测到）。
func (a *App) DetectMCPort() int {
	paths := a.logCandidates()
	if p := mclog.FindLatest(paths); p != "" {
		return mclog.ScanPort(p)
	}
	return 0
}

// StartLogWatch 持续监听 MC 日志，端口变化推事件 clink:mcport。
func (a *App) StartLogWatch() {
	if a.logCancel != nil {
		a.logCancel()
	}
	paths := a.logCandidates()
	p := mclog.FindLatest(paths)
	if p == "" {
		return
	}
	a.logCtx, a.logCancel = context.WithCancel(context.Background())
	go mclog.Watch(a.logCtx, p, func(port int) {
		if a.ctx != nil {
			wruntime.EventsEmit(a.ctx, "clink:mcport", port)
		}
	})
}

// CreateRoom 房主：创建房间，返回 6 位房间码（已自动复制到剪贴板）。
func (a *App) CreateRoom(mcPort int) (string, error) {
	a.mu.Lock()
	if a.host != nil || a.guest != nil {
		a.mu.Unlock()
		return "", fmt.Errorf("已在联机中，请先点击「退出联机」")
	}
	a.mu.Unlock()

	if mcPort <= 0 || mcPort > 65535 {
		return "", fmt.Errorf("游戏端口无效（%d）", mcPort)
	}

	// 防火墙放行（一次性，弹 UAC 授权；失败不阻塞开房）
	go func() {
		if !a.cfg.FwDone {
			if err := sysguard.EnsureFirewall(); err == nil {
				a.cfg.FwDone = true
				a.saveConfig()
			} else {
				a.emit(session.Event{Stage: "info", Msg: "防火墙放行未完成：" + err.Error() + "（联机可能受影响，可稍后重试）"})
			}
		}
	}()

	host, err := session.StartHost(a.ctx, session.HostConfig{
		MCPort:  mcPort,
		TURN:    parseServers(a.cfg.TURN),
		Mailbox: mailbox.NewClient(a.cfg.MailboxBase),
	}, a.emit)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.host = host
	a.mu.Unlock()
	// 房间码自动复制
	if a.ctx != nil {
		_ = wruntime.ClipboardSetText(a.ctx, host.Code())
	}
	return host.Code(), nil
}

// JoinRoom 客机：输入 6 位房间码加入。
func (a *App) JoinRoom(code string) error {
	a.mu.Lock()
	if a.host != nil || a.guest != nil {
		a.mu.Unlock()
		return fmt.Errorf("已在联机中，请先点击「退出联机」")
	}
	a.mu.Unlock()

	guest, err := session.StartGuest(a.ctx, session.GuestConfig{
		Code:      strings.TrimSpace(strings.ToLower(code)),
		TURN:      parseServers(a.cfg.TURN),
		Mailbox:   mailbox.NewClient(a.cfg.MailboxBase),
		LocalPort: 25565,
	}, a.emit)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.guest = guest
	a.mu.Unlock()
	return nil
}

// Leave 退出当前房间/连接。
func (a *App) Leave() {
	a.mu.Lock()
	h, g := a.host, a.guest
	a.host, a.guest = nil, nil
	a.mu.Unlock()
	if h != nil {
		h.Stop()
	}
	if g != nil {
		g.Stop()
	}
}

// Status 当前状态快照（UI 轮询兜底，主要靠事件流）。
func (a *App) Status() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := map[string]any{"mode": "idle"}
	if a.host != nil {
		st["mode"] = "host"
		st["code"] = a.host.Code()
		st["guests"] = a.host.Guests()
		st["v6"] = a.host.HasV6()
	}
	if a.guest != nil {
		st["mode"] = "guest"
		st["addr"] = a.guest.Addr()
		st["tier"] = a.guest.Tier()
	}
	return st
}

// SetAdvanced 保存高级设置。
func (a *App) SetAdvanced(turn, mcDir, mailboxBase string) error {
	a.cfg.TURN = strings.TrimSpace(turn)
	a.cfg.MCDir = strings.TrimSpace(mcDir)
	a.cfg.MailboxBase = strings.TrimSpace(mailboxBase)
	a.saveConfig()
	return nil
}

// GetAdvanced 读取高级设置。
func (a *App) GetAdvanced() map[string]string {
	return map[string]string{
		"turn":    a.cfg.TURN,
		"mcDir":   a.cfg.MCDir,
		"mailbox": a.cfg.MailboxBase,
	}
}

// NetProbe 网络体检（IPv6 / NAT 类型）。
func (a *App) NetProbe() netprobe.Report {
	return netprobe.Probe()
}

func (a *App) logCandidates() []string {
	paths := mclog.DefaultCandidates()
	if a.cfg.MCDir != "" {
		paths = append([]string{filepath.Join(a.cfg.MCDir, "logs", "latest.log")}, paths...)
	}
	return paths
}

// parseServers 解析 "turn:host:port 或 user:pass@turn:host:port" 列表（逗号/空白分隔）。
func parseServers(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
