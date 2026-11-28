package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ccdsyy/clink/internal/blacklist"
	"github.com/ccdsyy/clink/internal/ipc"
	"github.com/ccdsyy/clink/internal/lanscan"
	"github.com/ccdsyy/clink/internal/mailbox"
	"github.com/ccdsyy/clink/internal/mclog"
	"github.com/ccdsyy/clink/internal/modhub"
	"github.com/ccdsyy/clink/internal/netprobe"
	"github.com/ccdsyy/clink/internal/notify"
	"github.com/ccdsyy/clink/internal/relay"
	"github.com/ccdsyy/clink/internal/session"
	"github.com/ccdsyy/clink/internal/smsbox"
	"github.com/ccdsyy/clink/internal/sysguard"
	"github.com/ccdsyy/clink/internal/toolbox"
	"github.com/ccdsyy/clink/internal/updater"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App Wails 前后端桥接层：只做参数校验、状态转发与事件推送。
type App struct {
	ctx   context.Context
	mu    sync.Mutex
	host  *session.Host
	guest *session.Guest
	cfg   *Config

	bl *blacklist.Store
	tb *toolbox.Client
	dl *modhub.Downloader

	sms    *smsbox.Client
	smsLim *smsbox.Limiter

	scanner *lanscan.Scanner // LAN 组播发现（ROADMAP #66）

	ballMode  bool // 悬浮球模式
	minimised bool // 主窗口最小化中（通知判断用）
	restoreW  int  // 悬浮球恢复尺寸
	restoreH  int

	updCancel context.CancelFunc // 更新检查循环

	logCtx    context.Context
	logCancel context.CancelFunc
}

// 版本信息：每次发版改这里 + website/update.json（build 必须同时 +1）。
const (
	AppVersion = "1.4.0"
	AppBuild   = 5 // 发布编号：1=v1.0.0 … 4=v1.3.0, 5=v1.4.0, …
)

// Config 持久化配置（%APPDATA%/CLink/config.json）。
type Config struct {
	TURN              string   `json:"turn"`        // 自定义 TURN 中继列表
	MCDir             string   `json:"mcDir"`       // MC 目录（模组下载用）
	MailboxBase       string   `json:"mailboxBase"` // 自定义信令节点地址
	MailboxNode       string   `json:"mailboxNode"` // 信令节点选择：auto|clip1|uapi|custom（空=auto）
	StunMode          string   `json:"stunMode"`    // STUN 模式：auto|custom（空=auto）
	StunCustom        string   `json:"stunCustom"`  // 自定义 STUN 列表
	TurnNodes         []string `json:"turnNodes"`   // 启用的预设 TURN 节点名
	LocalPort         int      `json:"localPort"`   // 客机本地监听端口（默认 25565）
	RecentRooms       []string `json:"recentRooms"` // 最近房间码（快速重连）
	RelayNodes        string   `json:"relayNodes"`  // 自建中继节点列表（ROADMAP #31，udp://ip:port 逗号分隔）
	RelayPSK          string   `json:"relayPSK"`    // 中继共享口令（与 clink-relay 服务端一致）
	FwDone            bool     `json:"fwDone"`
	Name              string   `json:"name"`              // 玩家昵称（黑名单/通知/成员列表标识）
	NotifyOn          bool     `json:"notifyOn"`          // 加入通知开关
	LastNotifiedBuild int      `json:"lastNotifiedBuild"` // 已通知过的发布编号（去重）
	SmsURL            string   `json:"smsUrl"`            // 短信盒子地址（http://ip:端口）
	SmsPIN            string   `json:"smsPin"`            // 盒子 PIN（未设置则空）
	SmsPhone          string   `json:"smsPhone"`          // 房主接收通知的手机号
	SmsJoinOn         bool     `json:"smsJoinOn"`         // 有人加入 → 短信提醒
	SmsRejectOn       bool     `json:"smsRejectOn"`       // 拦截黑名单 → 短信提醒
}

// NetCfg 网络与信令设置（设置页整卡读写）。
type NetCfg struct {
	Node       string   `json:"node"`       // 信令节点：auto|clip1|uapi|custom
	Mailbox    string   `json:"mailbox"`    // 自定义信令节点地址
	StunMode   string   `json:"stunMode"`   // auto|custom
	Stun       string   `json:"stun"`       // 自定义 STUN 列表
	TurnNodes  []string `json:"turnNodes"`  // 启用的预设 TURN 节点名
	Turn       string   `json:"turn"`       // 自定义 TURN 列表
	LocalPort  int      `json:"localPort"`  // 客机本地端口（1-65535）
	MCDir      string   `json:"mcDir"`      // MC 目录
	RelayNodes string   `json:"relayNodes"` // 自建中继节点
	RelayPSK   string   `json:"relayPSK"`   // 中继共享口令
}

// NewApp 构造（加载配置）。
func NewApp() *App {
	a := &App{cfg: &Config{NotifyOn: true, SmsJoinOn: true, SmsRejectOn: true, LocalPort: 25565}}
	a.loadConfig()
	if a.cfg.LocalPort <= 0 || a.cfg.LocalPort > 65535 {
		a.cfg.LocalPort = 25565
	}
	switch a.cfg.MailboxNode {
	case "auto", "clip1", "uapi", "custom":
	default:
		a.cfg.MailboxNode = "auto"
	}
	if a.cfg.RelayPSK == "" {
		a.cfg.RelayPSK = "clink-relay-psk-v1" // 内置默认；自建节点时改成自己的
	}
	switch a.cfg.StunMode {
	case "auto", "google", "miwifi", "cloudflare", "nextcloud", "custom":
	default:
		a.cfg.StunMode = "auto"
	}
	a.bl, _ = blacklist.Load("") // 加载失败则用空表，不影响主流程
	a.tb = toolbox.New()
	a.dl = modhub.NewDownloader(a.onModProgress)
	a.sms = smsbox.New(a.cfg.SmsURL, a.cfg.SmsPIN)
	a.smsLim = smsbox.NewLimiter()
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

// mailboxClient 按用户选择的信令节点构造客户端（多节点容灾）。
func (a *App) mailboxClient() *mailbox.Client {
	switch a.cfg.MailboxNode {
	case "clip1":
		return mailbox.NewClient(mailbox.EndpointClip1)
	case "uapi":
		return mailbox.NewClient(mailbox.EndpointUAPI)
	case "custom":
		if strings.TrimSpace(a.cfg.MailboxBase) != "" {
			return mailbox.NewClient(a.cfg.MailboxBase)
		}
		return mailbox.NewClientMulti(mailbox.DefaultBases...)
	default: // auto / 旧配置
		return mailbox.NewClientMulti(mailbox.DefaultBases...)
	}
}

// stunList STUN 节点：具名节点 / 自定义列表 / 自动选优（nil → session 层 PickSTUN）。
func (a *App) stunList() []string {
	switch a.cfg.StunMode {
	case "google", "miwifi", "cloudflare", "nextcloud":
		for _, n := range netprobe.PublicSTUN {
			if strings.Contains(n, a.cfg.StunMode) {
				return []string{n}
			}
		}
	case "custom":
		if l := netprobe.NormalizeSTUN(a.cfg.StunCustom); len(l) > 0 {
			return l
		}
	}
	return nil // auto → session 层 netprobe.PickSTUN() 多节点探测选优
}

// turnList TURN 中继：启用的预设节点 + 自定义列表（user:pass@turn:host:port 格式）。
func (a *App) turnList() []string {
	enabled := map[string]bool{}
	for _, n := range a.cfg.TurnNodes {
		enabled[n] = true
	}
	var out []string
	for _, n := range netprobe.PresetTURN {
		if enabled[n.Name] { // 用户显式勾选才启用（含标注已失效的节点）
			out = append(out, n.Cred+"@"+n.URL)
		}
	}
	if l := parseServers(a.cfg.TURN); len(l) > 0 {
		out = append(out, l...)
	}
	return out
}

// startup Wails 生命周期。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	// 窗口最小化/还原跟踪（通知条件）
	wruntime.EventsOn(ctx, "window:minimise", func(...any) { a.mu.Lock(); a.minimised = true; a.mu.Unlock() })
	wruntime.EventsOn(ctx, "window:restore", func(...any) { a.mu.Lock(); a.minimised = false; a.mu.Unlock() })

	// 在线更新检查：启动 10 秒后第一次，之后每 6 小时
	updCtx, cancel := context.WithCancel(ctx)
	a.updCancel = cancel
	go a.updateLoop(updCtx)

	// LAN 组播发现（ROADMAP #66）：房主声明 / 客机扫描
	a.scanner = lanscan.StartScan()

	// 本地 IPC（ROADMAP #69）：启动器 / 第三方 Mod 可调用穿透栈
	ipc.Serve(ipc.Handler{
		Status: a.Status,
		Create: a.CreateRoom,
		Join:   a.JoinRoom,
		Leave:  a.Leave,
		Recent: a.RecentRooms,
	})

	// 启动预热（ROADMAP #9/#60 输码即预热）：NAT 分类 + STUN 选优缓存
	// + 对选中节点发 5 次 binding 焐热 NAT 映射
	go func() {
		_ = netprobe.MappingClass()
		_ = netprobe.PickSTUN()
		for i := 0; i < 5; i++ {
			if _, err := netprobe.PublicV4(); err != nil {
				return
			}
			time.Sleep(400 * time.Millisecond)
		}
	}()
}

// Prewarm 供前端在输入房间码时主动触发（与启动预热共用缓存）。
func (a *App) Prewarm() {
	_ = netprobe.MappingClass()
	_ = netprobe.PickSTUN()
}

// updateLoop 周期检查新版本（发布编号去重通知）。
func (a *App) updateLoop(ctx context.Context) {
	select {
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
		return
	}
	a.checkUpdate(true)
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.checkUpdate(true)
		case <-ctx.Done():
			return
		}
	}
}

// checkUpdate 拉取 update.json 并按发布编号判断是否通知。
// doNotify=true（自动检查）：遵守去重（同一 build 只通知一次）；
// 手动立即检查由 CheckUpdate 绑定处理，不受去重限制（直接返回结果给 UI）。
func (a *App) checkUpdate(doNotify bool) (*updater.UpdateInfo, error) {
	up, err := updater.Check(nil)
	if err != nil {
		return nil, err
	}
	info := &updater.UpdateInfo{
		CurrentVersion: AppVersion, LatestVersion: up.Version,
		CurrentBuild: AppBuild, LatestBuild: up.Build,
		Notes: up.Notes, Download: up.Download, Page: up.Page,
		HasUpdate: up.Build > AppBuild,
	}
	if !info.HasUpdate || !doNotify {
		return info, nil
	}

	// 发布编号去重：已通知过 ≥ 此 build 则不再打扰
	a.mu.Lock()
	last := a.cfg.LastNotifiedBuild
	already := up.Build <= last
	if !already {
		a.cfg.LastNotifiedBuild = up.Build
	}
	a.mu.Unlock()
	if already {
		return info, nil
	}
	a.saveConfig()

	// UI 事件 + 系统通知
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "clink:update", info)
	}
	if a.cfg.NotifyOn {
		notify.Send("update-"+up.Version, "🚀 CLink 有新版本 v"+up.Version,
			"发现新版本 v"+up.Version+"（发布编号 "+fmt.Sprint(up.Build)+"）："+up.Notes)
	}
	return info, nil
}

// CheckUpdate 手动「立即检查」（设置页）：返回结果由 UI 呈现，不重复系统通知。
func (a *App) CheckUpdate() (*updater.UpdateInfo, error) {
	return a.checkUpdate(false)
}

// CurrentVersion 当前客户端版本信息（关于/诊断用）。
func (a *App) CurrentVersion() map[string]any {
	return map[string]any{"version": AppVersion, "build": AppBuild, "page": "https://clink.541887.xyz"}
}

// shutdown Wails 退出前收尾。
func (a *App) shutdown(ctx context.Context) {
	if a.updCancel != nil {
		a.updCancel()
	}
	a.dl.CancelAll()
}

// OnBeforeClose 关窗拦截：不退出，缩小为悬浮球继续守护房间（真退出走悬浮球菜单）。
func (a *App) OnBeforeClose(ctx context.Context) bool {
	if a.host == nil && a.guest == nil {
		return false // 空闲时正常退出
	}
	a.EnterBallMode()
	return true
}

// emit 事件统一出口：转发 UI + 成员列表 + 悬浮球/最小化时的系统通知。
func (a *App) emit(e session.Event) {
	if a.ctx == nil {
		return
	}
	wruntime.EventsEmit(a.ctx, "clink:event", e)

	// 客机连接成功 → 自动复制 127.0.0.1:xxxx（保姆级交互）
	if e.Stage == "connected" && e.Addr != "" {
		_ = wruntime.ClipboardSetText(a.ctx, e.Addr)
	}

	// 房主侧：成员状态变化 → 推成员列表 + 系统通知
	switch e.Stage {
	case "guest-joined", "guest-left", "guest-kicked", "guest-rejected":
		a.mu.Lock()
		var members []string
		if a.host != nil {
			members = a.host.Members()
		}
		ball, mini, on := a.ballMode, a.minimised, a.cfg.NotifyOn
		a.mu.Unlock()
		wruntime.EventsEmit(a.ctx, "clink:members", members)

		if on && (ball || mini) && e.Stage == "guest-joined" {
			notify.Send("join-"+e.Name, "🎮 有小伙伴加入了！",
				e.Name+" 进入了房间，当前 "+fmt.Sprint(e.Guests)+" 人在线")
		}
		if on && (ball || mini) && e.Stage == "guest-left" {
			notify.Send("left-"+e.Name, "👋 有小伙伴掉线了",
				e.Name+" 的连接已断开，当前 "+fmt.Sprint(e.Guests)+" 人在线")
		}
		if on && (ball || mini) && e.Stage == "guest-rejected" {
			notify.Send("reject-"+e.Name, "🛡️ 已拦截加入请求", "黑名单玩家 "+e.Name+" 被拒绝加入")
		}

		// 短信盒子联动（人不在电脑前，手机也能收到）
		a.mu.Lock()
		joinOn, rejectOn := a.cfg.SmsJoinOn, a.cfg.SmsRejectOn
		a.mu.Unlock()
		if e.Stage == "guest-joined" && joinOn {
			a.smsNotify("join-"+e.Name, fmt.Sprintf(
				"【CLink】🎮 %s 加入了你的房间（当前 %d 人在线）。", e.Name, e.Guests))
		}
		if e.Stage == "guest-rejected" && rejectOn {
			a.smsNotify("reject-"+e.Name, fmt.Sprintf(
				"【CLink】🛡️ 已拦截黑名单玩家 %s 的加入请求。", e.Name))
		}
	}
}

// smsNotify 异步发提醒短信（限流器防轰炸：同 key 60s 去抖 + 每小时 20 条上限）。
func (a *App) smsNotify(key, text string) {
	a.mu.Lock()
	url, phone := a.cfg.SmsURL, a.cfg.SmsPhone
	a.mu.Unlock()
	if strings.TrimSpace(url) == "" || strings.TrimSpace(phone) == "" {
		return // 未配置短信盒子则静默跳过
	}
	if !a.smsLim.Allow(key) {
		return
	}
	go func() {
		c := smsbox.New(url, a.cfg.SmsPIN)
		_ = c.Send([]string{phone}, text)
	}()
}

func (a *App) onModProgress(p modhub.Progress) {
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "clink:modprogress", p)
	}
}

// ---------- 悬浮球模式 ----------

// EnterBallMode 缩为悬浮球：置顶 + 88x88 + 前端切球视图。
func (a *App) EnterBallMode() {
	if a.ctx == nil || a.ballMode {
		return
	}
	a.mu.Lock()
	a.ballMode = true
	a.mu.Unlock()
	w, h := wruntime.WindowGetSize(a.ctx)
	a.restoreW, a.restoreH = w, h
	wruntime.WindowSetMinSize(a.ctx, 0, 0) // 解除最小尺寸限制（否则无法缩到 88px）
	wruntime.WindowSetAlwaysOnTop(a.ctx, true)
	wruntime.WindowSetSize(a.ctx, 88, 88)
	wruntime.WindowUnminimise(a.ctx)
	wruntime.WindowShow(a.ctx)
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "clink:ball", true)
	}
}

// ExitBallMode 恢复主窗口。
func (a *App) ExitBallMode() {
	if a.ctx == nil || !a.ballMode {
		return
	}
	a.mu.Lock()
	a.ballMode = false
	a.mu.Unlock()
	wruntime.WindowSetAlwaysOnTop(a.ctx, false)
	wruntime.WindowSetMinSize(a.ctx, 720, 560)
	w, h := a.restoreW, a.restoreH
	if w < 720 || h < 560 {
		w, h = 960, 700
	}
	wruntime.WindowSetSize(a.ctx, w, h)
	wruntime.WindowShow(a.ctx)
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "clink:ball", false)
	}
}

// QuitApp 真正退出（悬浮球右键/按钮调用）。
func (a *App) QuitApp() {
	if a.ctx != nil {
		wruntime.Quit(a.ctx)
	}
}

// ---------- 供 JS 调用的绑定方法 ----------

// NetProbe 网络诊断（IPv6 / NAT 类型），供设置页展示。
func (a *App) NetProbe() netprobe.Report {
	return netprobe.Probe()
}

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
		if name := sysguard.DetectThirdPartyFirewall(); name != "" {
			a.emit(session.Event{Stage: "info", Msg: "⚠️ 检测到 " + name + "：请在其弹窗中允许 CLink 联网，否则打洞可能被拦截"})
		}
	}()

	host, err := session.StartHost(a.ctx, session.HostConfig{
		MCPort:     mcPort,
		STUN:       a.stunList(),
		TURN:       a.turnList(),
		Mailbox:    a.mailboxClient(),
		Blacklist:  a.bl,
		RelayAddrs: relay.ParseAddrs(a.cfg.RelayNodes),
		RelayPSK:   a.cfg.RelayPSK,
	}, a.emit)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.host = host
	a.mu.Unlock()
	// 房间码自动复制 + 记入最近房间
	if a.ctx != nil {
		_ = wruntime.ClipboardSetText(a.ctx, host.Code())
	}
	a.addRecentRoom(host.Code())
	return host.Code(), nil
}

// JoinRoom 客机：输入 6 位房间码加入（name 为本机昵称）。
func (a *App) JoinRoom(code, name string) error {
	a.mu.Lock()
	if a.host != nil || a.guest != nil {
		a.mu.Unlock()
		return fmt.Errorf("已在联机中，请先点击「退出联机」")
	}
	a.mu.Unlock()

	if n := strings.TrimSpace(name); n != "" {
		a.cfg.Name = n
		a.saveConfig()
	}

	guest, err := session.StartGuest(a.ctx, session.GuestConfig{
		Code:       strings.TrimSpace(strings.ToLower(code)),
		Name:       strings.TrimSpace(name),
		STUN:       a.stunList(),
		TURN:       a.turnList(),
		Mailbox:    a.mailboxClient(),
		LocalPort:  a.cfg.LocalPort,
		RelayAddrs: relay.ParseAddrs(a.cfg.RelayNodes),
		RelayPSK:   a.cfg.RelayPSK,
		Scanner:    a.scanner,
	}, a.emit)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.guest = guest
	a.mu.Unlock()
	a.addRecentRoom(strings.TrimSpace(strings.ToLower(code)))
	return nil
}

// RecentRooms 最近房间码（快速重连）。
func (a *App) RecentRooms() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.cfg.RecentRooms))
	out = append(out, a.cfg.RecentRooms...)
	return out
}

// addRecentRoom 记录最近使用的房间码（去重，最多 8 条）。
func (a *App) addRecentRoom(code string) {
	if !mailbox.ValidateRoomCode(code) {
		return
	}
	a.mu.Lock()
	var next []string
	next = append(next, code)
	for _, c := range a.cfg.RecentRooms {
		if c != code {
			next = append(next, c)
		}
	}
	if len(next) > 8 {
		next = next[:8]
	}
	a.cfg.RecentRooms = next
	a.mu.Unlock()
	a.saveConfig()
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
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "clink:members", []string{})
	}
}

// KickMember 房主：踢出成员（ban=true 同时拉黑）。
func (a *App) KickMember(name string, ban bool) error {
	a.mu.Lock()
	h := a.host
	a.mu.Unlock()
	if h == nil {
		return fmt.Errorf("当前没有开房")
	}
	h.Kick(name, ban)
	return nil
}

// ---------- 黑名单 ----------

// BlList 黑名单列表。
func (a *App) BlList() []blacklist.Entry {
	if a.bl == nil {
		return []blacklist.Entry{}
	}
	return a.bl.List()
}

// BlAdd 手动拉黑（设置页）。
func (a *App) BlAdd(name string) error {
	if a.bl == nil {
		return fmt.Errorf("黑名单不可用")
	}
	return a.bl.Add(name, "手动添加")
}

// BlRemove 解除拉黑。
func (a *App) BlRemove(name string) error {
	if a.bl == nil {
		return fmt.Errorf("黑名单不可用")
	}
	return a.bl.Remove(name)
}

// ---------- 工具箱（UAPI） ----------

// TbQueryIP 查询 IP 归属地（空 = 查本机出口 IP）。
func (a *App) TbQueryIP(ip string) (*toolbox.IPInfo, error) {
	return a.tb.QueryIP(strings.TrimSpace(ip))
}

// TbQRCode 文本 → 二维码（base64 dataURL，前端 img 直接用）。
func (a *App) TbQRCode(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("内容为空")
	}
	return a.tb.QRCodeDataURL(strings.TrimSpace(text), 300)
}

// TbMCStatus 查询任意 MC Java 服务器状态。
func (a *App) TbMCStatus(addr string) (*toolbox.MCServerStatus, error) {
	return a.tb.QueryMCServer(strings.TrimSpace(addr))
}

// ---------- 模组仓库 ----------

// ModSearch 搜索 Modrinth 模组。
func (a *App) ModSearch(query string) ([]modhub.ModResult, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("搜索词为空")
	}
	return modhub.Search(q, 24)
}

// ModVersions 拉取某模组可用版本。
func (a *App) ModVersions(projectID, gameVersion, loader string) ([]modhub.ModVersion, error) {
	return modhub.Versions(projectID, gameVersion, loader)
}

// ModDownload 多线程下载模组（断点续传）。dest 为完整目标路径。
func (a *App) ModDownload(url, dest string, threads int) error {
	if url == "" || dest == "" {
		return fmt.Errorf("参数缺失")
	}
	if threads <= 0 || threads > 32 {
		threads = 8
	}
	if dir := filepath.Dir(dest); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return a.dl.Download(dest, url, dest, threads)
}

// ModJobs 当前下载任务列表（进度恢复用）。
func (a *App) ModJobs() []modhub.Progress { return a.dl.Jobs() }

// ModCancel 取消任务。
func (a *App) ModCancel(dest string) { a.dl.Cancel(dest) }

// ModResumeAll 恢复目录下所有未完成下载（应用启动时调用）。返回恢复数量。
func (a *App) ModResumeAll(dir string) (int, error) {
	return a.dl.ResumeAll(strings.TrimSpace(dir), 8)
}

// OpenFolder 用资源管理器打开目录（mods 文件夹等）。
func (a *App) OpenFolder(dir string) error {
	if dir == "" {
		return fmt.Errorf("路径为空")
	}
	_ = os.MkdirAll(dir, 0o755)
	// Windows: explorer；mac/linux fallback
	return exec.Command("explorer", dir).Start()
}

// ---------- 短信盒子（sms-box APK 联动） ----------

// SmsSave 保存短信盒子配置。
func (a *App) SmsSave(url, pin, phone string, joinOn, rejectOn bool) error {
	url = strings.TrimSpace(url)
	phone = strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(phone), "，", " ")), "")
	if url != "" && !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + url
	}
	if url != "" && phone == "" && (joinOn || rejectOn) {
		return fmt.Errorf("开启了短信提醒但未填接收手机号")
	}
	a.mu.Lock()
	a.cfg.SmsURL, a.cfg.SmsPIN, a.cfg.SmsPhone = url, strings.TrimSpace(pin), phone
	a.cfg.SmsJoinOn, a.cfg.SmsRejectOn = joinOn, rejectOn
	a.mu.Unlock()
	a.saveConfig()
	a.sms = smsbox.New(a.cfg.SmsURL, a.cfg.SmsPIN)
	return nil
}

// SmsGet 读取短信盒子配置（设置页回显）。
func (a *App) SmsGet() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]any{
		"url": a.cfg.SmsURL, "pin": a.cfg.SmsPIN, "phone": a.cfg.SmsPhone,
		"joinOn": a.cfg.SmsJoinOn, "rejectOn": a.cfg.SmsRejectOn,
	}
}

// SmsPing 测试盒子连通性（免认证端点 + 认证探测）。
func (a *App) SmsPing() error {
	a.mu.Lock()
	url := strings.TrimSpace(a.cfg.SmsURL)
	a.mu.Unlock()
	if url == "" {
		return fmt.Errorf("请先填写盒子地址（短信盒子 APP 首页会显示，如 http://192.168.1.5:8080）")
	}
	return smsbox.New(url, a.cfg.SmsPIN).Ping()
}

// SmsTest 发送一条测试短信到指定手机号。
func (a *App) SmsTest(phone string) error {
	a.mu.Lock()
	url := strings.TrimSpace(a.cfg.SmsURL)
	pin := a.cfg.SmsPIN
	a.mu.Unlock()
	if url == "" {
		return fmt.Errorf("请先填写并保存盒子地址")
	}
	phone = strings.ReplaceAll(strings.TrimSpace(phone), " ", "")
	if phone == "" {
		return fmt.Errorf("请填写手机号")
	}
	return smsbox.New(url, pin).Send([]string{phone},
		"【CLink】✅ 短信盒子连接成功！有人加入房间时这里会第一时间提醒你。")
}

// SmsInvite 房主把房间码短信发给朋友（一键邀请）。
func (a *App) SmsInvite(phone string) error {
	a.mu.Lock()
	url, pin := strings.TrimSpace(a.cfg.SmsURL), a.cfg.SmsPIN
	code := ""
	if a.host != nil {
		code = a.host.Code()
	}
	a.mu.Unlock()
	if code == "" {
		return fmt.Errorf("请先创建房间再发邀请")
	}
	if url == "" {
		return fmt.Errorf("请先在设置中配置短信盒子")
	}
	phone = strings.ReplaceAll(strings.TrimSpace(phone), " ", "")
	if phone == "" {
		return fmt.Errorf("请填写朋友的手机号")
	}
	text := fmt.Sprintf("【CLink】你的好朋友邀请你一起玩 Minecraft！房间码：%s（6位）。"+
		"下载 CLink：https://clink.541887.xyz 或加互助群 1108157805", code)
	if err := smsbox.New(url, pin).Send([]string{phone}, text); err != nil {
		return err
	}
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "clink:event", session.Event{Stage: "info",
			Msg: "邀请短信已通过短信盒子发出 ✓"})
	}
	return nil
}

// ---------- 通用 ----------

// Status 当前状态快照（UI 轮询兜底，主要靠事件流）。
func (a *App) Status() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := map[string]any{"mode": "idle", "name": a.cfg.Name, "ball": a.ballMode,
		"nat": netprobe.MappingClass()}
	if a.host != nil {
		st["mode"] = "host"
		st["code"] = a.host.Code()
		st["guests"] = a.host.Guests()
		st["v6"] = a.host.HasV6()
		st["members"] = a.host.Members()
	}
	if a.guest != nil {
		st["mode"] = "guest"
		st["addr"] = a.guest.Addr()
		st["tier"] = a.guest.Tier()
	}
	return st
}

// SetNet 保存网络与信令设置（信令节点 / STUN / TURN / 本地端口 / MC 目录）。
func (a *App) SetNet(v NetCfg) error {
	lp := v.LocalPort
	if lp <= 0 || lp > 65535 {
		return fmt.Errorf("本地端口要在 1-65535 之间")
	}
	switch v.Node {
	case "auto", "clip1", "uapi", "custom":
	default:
		return fmt.Errorf("未知的信令节点类型（%s）", v.Node)
	}
	switch v.StunMode {
	case "auto", "google", "miwifi", "cloudflare", "nextcloud", "custom":
	default:
		return fmt.Errorf("未知的 STUN 模式（%s）", v.StunMode)
	}
	if v.Node == "custom" && strings.TrimSpace(v.Mailbox) == "" {
		return fmt.Errorf("自定义信令节点需要填写地址")
	}
	a.mu.Lock()
	a.cfg.MailboxNode = v.Node
	a.cfg.MailboxBase = strings.TrimSpace(v.Mailbox)
	a.cfg.StunMode = v.StunMode
	a.cfg.StunCustom = strings.TrimSpace(v.Stun)
	a.cfg.TurnNodes = v.TurnNodes
	if a.cfg.TurnNodes == nil {
		a.cfg.TurnNodes = []string{}
	}
	a.cfg.TURN = strings.TrimSpace(v.Turn)
	a.cfg.LocalPort = lp
	a.cfg.MCDir = strings.TrimSpace(v.MCDir)
	a.cfg.RelayNodes = strings.TrimSpace(v.RelayNodes)
	a.cfg.RelayPSK = strings.TrimSpace(v.RelayPSK)
	a.mu.Unlock()
	a.saveConfig()
	return nil
}

// GetNet 读取网络与信令设置（设置页回显）。
func (a *App) GetNet() NetCfg {
	a.mu.Lock()
	defer a.mu.Unlock()
	return NetCfg{
		Node:       a.cfg.MailboxNode,
		Mailbox:    a.cfg.MailboxBase,
		StunMode:   a.cfg.StunMode,
		Stun:       a.cfg.StunCustom,
		TurnNodes:  a.cfg.TurnNodes,
		Turn:       a.cfg.TURN,
		LocalPort:  a.cfg.LocalPort,
		MCDir:      a.cfg.MCDir,
		RelayNodes: a.cfg.RelayNodes,
		RelayPSK:   a.cfg.RelayPSK,
	}
}

// PresetNodes 预设节点清单（设置页动态渲染：信令节点 / TURN 节点）。
func (a *App) PresetNodes() map[string]any {
	return map[string]any{
		"mailbox": mailbox.PresetEndpoints,
		"stun":    netprobe.PublicSTUN,
		"turn":    netprobe.PresetTURN,
	}
}

// SetProfile 保存昵称与通知开关。
func (a *App) SetProfile(name string, notifyOn bool) {
	a.cfg.Name = strings.TrimSpace(name)
	a.cfg.NotifyOn = notifyOn
	a.saveConfig()
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
