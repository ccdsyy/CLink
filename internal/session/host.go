package session

import (
        "context"
        "encoding/json"
        "fmt"
        "sort"
        "strings"
        "sync"
        "sync/atomic"
        "time"

        "github.com/ccdsyy/clink/internal/blacklist"
        "github.com/ccdsyy/clink/internal/mailbox"
        "github.com/ccdsyy/clink/internal/netprobe"
        "github.com/ccdsyy/clink/internal/tunnel/p2p"
        "github.com/ccdsyy/clink/internal/tunnel/v6direct"
)

// HostConfig 房主配置。
type HostConfig struct {
        MCPort    int
        STUN      []string
        TURN      []string
        Mailbox   *mailbox.Client
        Blacklist *blacklist.Store // 可为 nil（无黑名单）
}

// Host 房主会话状态机。
//
// 生命周期：StartHost → 探测 v6 → 起 v6 监听 + p2p 端点 → 写 Offer 信箱+槽位
// → 轮询 Answer 槽 → 客机接入 → epoch+1 写新 Offer（支持下一位玩家）→ 循环。
type Host struct {
        ctx    context.Context
        cancel context.CancelFunc
        cfg    HostConfig
        emit   func(Event)

        code   string
        epoch  int
        guests int32

        v6     *v6direct.Host
        v6Addr string
        v6Port int

        mu      sync.Mutex
        pcs     map[string]*p2p.Host // 昵称 → 该客机的 WebRTC 端点
        members map[string]bool      // 在线成员（昵称去重）
}

// StartHost 启动房主会话（后台 goroutine 运行，事件经 emit 推送）。
func StartHost(ctx context.Context, cfg HostConfig, emit func(Event)) (*Host, error) {
        if cfg.MCPort <= 0 || cfg.MCPort > 65535 {
                return nil, fmt.Errorf("游戏端口无效（%d）", cfg.MCPort)
        }
        if cfg.Mailbox == nil {
                cfg.Mailbox = mailbox.NewClient("")
        }
        if len(cfg.STUN) == 0 {
                cfg.STUN = []string{"stun:stun.l.google.com:19302"}
        }
        if ctx == nil {
                ctx = context.Background()
        }
        c, cancel := context.WithCancel(ctx)

        h := &Host{
                ctx:     c,
                cancel:  cancel,
                cfg:     cfg,
                emit:    emit,
                code:    mailbox.GenRoomCode(),
                pcs:     make(map[string]*p2p.Host),
                members: make(map[string]bool),
        }

        // Tier 1 准备：有公网 v6 就起直连监听（信令里同时保留 WebRTC 备用）
        if addrs := netprobe.PublicIPv6(); len(addrs) > 0 && netprobe.HasIPv6Route() {
                vh, port, err := v6direct.StartHost(c, cfg.MCPort)
                if err == nil {
                        h.v6 = vh
                        h.v6Addr, h.v6Port = addrs[0].String(), port
                }
        }

        go h.loop()
        return h, nil
}

// Code 房间码。
func (h *Host) Code() string { return h.code }

// Guests 已加入人数。
func (h *Host) Guests() int { return int(atomic.LoadInt32(&h.guests)) }

// Members 在线成员列表（昵称，去重排序）。
func (h *Host) Members() []string {
        h.mu.Lock()
        defer h.mu.Unlock()
        out := make([]string, 0, len(h.members))
        for n := range h.members {
                out = append(out, n)
        }
        sort.Strings(out)
        return out
}

// Kick 踢出成员并可选拉黑（webrtc 层精确断开；被踢者会自动重连并被拒）。
func (h *Host) Kick(name string, ban bool) {
        name = strings.TrimSpace(name)
        if name == "" {
                return
        }
        if ban && h.cfg.Blacklist != nil {
                _ = h.cfg.Blacklist.Add(name, "房主在房间内拉黑")
        }
        h.mu.Lock()
        pc := h.pcs[name]
        delete(h.pcs, name)
        delete(h.members, name)
        h.mu.Unlock()
        if pc != nil {
                pc.Stop() // 客机侧收到 Dead → 自动重连 → 黑名单拒绝
        }
        h.emit(Event{Stage: "guest-kicked", Name: name,
                Msg: fmt.Sprintf("%s 已被%s", name, map[bool]string{true: "踢出并拉黑", false: "踢出房间"}[ban]),
                Guests: h.Guests()})
}

// HasV6 是否具备 IPv6 直连条件。
func (h *Host) HasV6() bool { return h.v6 != nil }

// Stop 结束房间。
func (h *Host) Stop() {
        h.cancel()
        h.mu.Lock()
        pcs := h.pcs
        h.pcs = make(map[string]*p2p.Host)
        h.members = make(map[string]bool)
        h.mu.Unlock()
        for _, p := range pcs {
                p.Stop()
        }
        if h.v6 != nil {
                h.v6.Stop()
        }
}

func (h *Host) loop() {
        h.emit(Event{Stage: "created", Room: h.code, Msg: "房间已创建"})
        for {
                if h.ctx.Err() != nil {
                        return
                }
                if err := h.runEpoch(); err != nil {
                        if h.ctx.Err() != nil {
                                return
                        }
                        h.emit(Event{Stage: "error", Msg: "信箱通信受阻：" + err.Error() + "，5 秒后自动重试"})
                        select {
                        case <-h.ctx.Done():
                                return
                        case <-time.After(5 * time.Second):
                        }
                }
        }
}

// runEpoch 一个 epoch：挂出 Offer → 等一位客机接入 → 返回（loop 将进入下一 epoch）。
func (h *Host) runEpoch() error {
        // epoch 轮转上限
        if h.epoch >= mailbox.MaxEpoch {
                h.emit(Event{Stage: "info", Msg: "房间已达最大人数（31），如需更多请退出后重新开房"})
                <-h.ctx.Done()
                return nil
        }

        // 1. WebRTC 端点（即使有 v6 也准备，双通道提高客机加入成功率）
        pc, err := p2p.NewHost(p2p.Config{STUN: h.cfg.STUN, TURN: h.cfg.TURN, MCPort: h.cfg.MCPort})
        if err != nil {
                return fmt.Errorf("WebRTC 初始化失败: %w", err)
        }
        sdpType, sdp, err := pc.Offer()
        if err != nil {
                pc.Stop()
                return fmt.Errorf("SDP 生成失败: %w", err)
        }

        // 2. 信令上链
        sig := mailbox.Signal{
                V:       mailbox.ProtoVersion,
                Type:    "offer",
                Epoch:   h.epoch,
                Tier:    "dual",
                Nonce:   mailbox.Nonce(),
                Ts:      time.Now().Unix(),
                SDP:     sdp,
                SDPType: sdpType,
                MCPort:  h.cfg.MCPort,
        }
        if h.v6 != nil {
                sig.V6, sig.V6Port = h.v6Addr, h.v6Port
        } else {
                sig.Tier = "webrtc"
        }
        payload, err := json.Marshal(sig)
        if err != nil {
                pc.Stop()
                return err
        }
        key, err := mailbox.DeriveKey(h.code)
        if err != nil {
                pc.Stop()
                return err
        }
        sealed, err := mailbox.Seal(key, payload)
        if err != nil {
                pc.Stop()
                return err
        }
        id, err := h.cfg.Mailbox.Store(sealed, mailbox.DefaultTTL)
        if err != nil {
                pc.Stop()
                return fmt.Errorf("Offer 信箱写入失败: %w", err)
        }
        offerSlot := mailbox.SlotCode(h.code, h.epoch, mailbox.RoleOffer)
        if err := h.cfg.Mailbox.Shorten(mailbox.GitHubPagesRedirect+"?id="+id, offerSlot); err != nil {
                pc.Stop()
                return fmt.Errorf("房间码注册失败: %w", err)
        }

        h.emit(Event{Stage: "waiting", Room: h.code, Guests: h.Guests(),
                Msg: "房间已就绪，把房间码发给小伙伴吧"})

        // 3. 轮询 Answer 槽（2 秒一次）
        answerSlot := mailbox.SlotCode(h.code, h.epoch, mailbox.RoleAnswer)
        var ans *mailbox.Signal
        for {
                if h.ctx.Err() != nil {
                        pc.Stop()
                        return nil
                }
                redirect, err := h.cfg.Mailbox.RedirectInfo(answerSlot)
                if err == nil {
                        ansID, idErr := mailbox.IDFromRedirectURL(redirect)
                        if idErr == nil {
                                sealedAns, gErr := h.cfg.Mailbox.Get(ansID)
                                if gErr == nil {
                                        if plain, oErr := mailbox.Open(key, sealedAns); oErr == nil {
                                                var s mailbox.Signal
                                                if jErr := json.Unmarshal(plain, &s); jErr == nil && s.Validate("answer") == nil && s.Epoch == h.epoch {
                                                        ans = &s
                                                        break
                                                }
                                        }
                                }
                        }
                }
                select {
                case <-h.ctx.Done():
                        pc.Stop()
                        return nil
                case <-time.After(2 * time.Second):
                }
        }

        // 4. 客机接入
        if guestName(ans.Name) != "小伙伴" && h.cfg.Blacklist != nil && h.cfg.Blacklist.Has(ans.Name) {
                // 黑名单命中：拒绝接入，直接进入下一 epoch 继续等下一位玩家
                pc.Stop()
                h.epoch++
                h.emit(Event{Stage: "guest-rejected", Name: ans.Name,
                        Msg: fmt.Sprintf("已拒绝黑名单玩家 %s 的加入请求", ans.Name)})
                return nil
        }
        if ans.Tier == "ipv6" {
                // 客机走 IPv6 直连，无需 WebRTC
                pc.Stop()
                n := int(atomic.AddInt32(&h.guests, 1))
                name := guestName(ans.Name)
                h.mu.Lock()
                h.members[name] = true
                h.mu.Unlock()
                h.emit(Event{Stage: "guest-joined", Name: name, Guests: n, Tier: "ipv6",
                        Msg: fmt.Sprintf("%s 已通过 IPv6 直连加入", name)})
        } else {
                if err := pc.AcceptAnswer(ans.SDPType, ans.SDP); err != nil {
                        pc.Stop()
                        return fmt.Errorf("Answer 接受失败: %w", err)
                }
                name := guestName(ans.Name)
                h.mu.Lock()
                h.pcs[name] = pc // 登记，供 Kick 精确断开
                h.mu.Unlock()

                // 等 ctrl 打通（45 秒）
                select {
                case <-pc.Established():
                        n := int(atomic.AddInt32(&h.guests, 1))
                        h.mu.Lock()
                        h.members[name] = true
                        h.mu.Unlock()
                        h.emit(Event{Stage: "guest-joined", Name: name, Guests: n, Tier: "webrtc",
                                Msg: fmt.Sprintf("%s 已通过 P2P 打洞加入", name)})
                case <-time.After(45 * time.Second):
                        h.emit(Event{Stage: "error", Guests: h.Guests(),
                                Msg: "有小伙伴加入失败（打洞未完成）。对方网络可能较复杂，建议其切换手机热点后重新加入"})
                case <-h.ctx.Done():
                        return nil
                }
        }

        // 5. 本轮完成 → 下一 epoch 挂新 Offer
        h.epoch++
        return nil
}

func guestName(name string) string {
        if name == "" {
                return "小伙伴"
        }
        return name
}
